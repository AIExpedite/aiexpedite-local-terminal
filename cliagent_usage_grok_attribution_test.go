// cliagent_usage_grok_attribution_test.go
// -----------------------------------------------------------------------------
// A billing record is only observable when a producer identity was logged
// BEFORE it. The Grok CLI logs none, so a DIRECT (PTY) run left records nobody
// could attribute — the reason a passing smoke still reported
// observableMetricCount 0. These tests cover both run shapes end to end:
// the direct-run identity append (including its re-arm after a log rotation)
// and the terminal-managed merge out of an isolated home.
// -----------------------------------------------------------------------------

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// resetGrokBillingAttribution clears the process-wide append guard so each test
// starts from a cold agent.
func resetGrokBillingAttribution(t *testing.T) {
	t.Helper()
	grokBillingAttribution.mu.Lock()
	grokBillingAttribution.identity = ""
	grokBillingAttribution.lastVerified = time.Time{}
	grokBillingAttribution.mu.Unlock()
	t.Cleanup(func() {
		grokBillingAttribution.mu.Lock()
		grokBillingAttribution.identity = ""
		grokBillingAttribution.lastVerified = time.Time{}
		grokBillingAttribution.mu.Unlock()
	})
}

// grokIdentityLineCount counts our producer markers in a home's unified log.
func grokIdentityLineCount(t *testing.T, base string) int {
	t.Helper()
	raw, err := os.ReadFile(grokBillingLogPath(base))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read unified.jsonl: %v", err)
	}
	return strings.Count(string(raw), grokManagedBillingIdentityMessage)
}

// helperGrokHomeWithAccount builds a Grok home whose auth resolves to account.
func helperGrokHomeWithAccount(t *testing.T, account string) string {
	t.Helper()
	base := t.TempDir()
	helperWriteJSON(t, filepath.Join(base, "auth.json"), map[string]any{"user_id": account})
	return base
}

// helperAppendGrokLogLine appends one line to a home's unified log, the way the
// Grok CLI itself would.
func helperAppendGrokLogLine(t *testing.T, base, line string) {
	t.Helper()
	path := grokBillingLogPath(base)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open unified.jsonl: %v", err)
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatalf("append unified.jsonl: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close unified.jsonl: %v", err)
	}
}

// The direct-run acceptance: after one session start, a record the CLI writes
// for itself is attributable and produces a timestamped metric.
func TestEnsureGrokBillingAttribution_MakesADirectRunRecordObservable(t *testing.T) {
	resetGrokBillingAttribution(t)
	base := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", base)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	ensureGrokBillingAttribution(now)
	if got := grokIdentityLineCount(t, base); got != 1 {
		t.Fatalf("identity lines = %d, want exactly 1", got)
	}

	// The CLI then fetches billing, as it does at session start.
	helperAppendGrokLogLine(t, base,
		grokUnmeteredLine("2026-08-19T11:59:00Z", grokBillingLogMessage))

	usage, ok := grokUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatal("Parse failed")
	}
	if len(usage.Metrics) != 1 || !usage.Metrics[0].Unknown {
		t.Fatalf("want one confirmed-unmetered row, got %+v", usage.Metrics)
	}
	if usage.Metrics[0].ObservedAt != "2026-08-19T11:59:00Z" {
		t.Fatalf("ObservedAt = %q — the direct run's record was not attributable",
			usage.Metrics[0].ObservedAt)
	}
}

// Idempotent while the line is still in the tail, and re-armed by an account
// change: a `grok login` to another account must not leave later records
// attributed to the previous one.
func TestEnsureGrokBillingAttribution_IsIdempotentAndReArmsOnAccountChange(t *testing.T) {
	resetGrokBillingAttribution(t)
	base := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", base)
	start := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	ensureGrokBillingAttribution(start)
	ensureGrokBillingAttribution(start.Add(time.Minute))
	// Past the recheck window, with our line still present: verify, do not write.
	ensureGrokBillingAttribution(start.Add(grokBillingAttributionRecheck + time.Minute))
	if got := grokIdentityLineCount(t, base); got != 1 {
		t.Fatalf("identity lines = %d, want 1 — the guard appended on an unchanged log", got)
	}

	helperWriteJSON(t, filepath.Join(base, "auth.json"), map[string]any{"user_id": "acct-2"})
	ensureGrokBillingAttribution(start.Add(2 * time.Minute))
	if got := grokIdentityLineCount(t, base); got != 2 {
		t.Fatalf("identity lines = %d, want 2 after an account change", got)
	}

	helperAppendGrokLogLine(t, base,
		grokUnmeteredLine("2026-08-19T12:03:00Z", grokBillingLogMessage))
	snap, ok := readGrokBillingSnapshot(base, grokIdentityCandidates(base))
	if !ok {
		t.Fatal("the record produced under acct-2 must be attributable to acct-2")
	}
	if got := snap.ObservedAt.UTC().Format(time.RFC3339); got != "2026-08-19T12:03:00Z" {
		t.Fatalf("ObservedAt = %s, want the newest record", got)
	}
}

// Grok rotates unified.jsonl, which discards our line. A once-per-process guard
// would leave every later record unattributable for the life of the agent.
func TestEnsureGrokBillingAttribution_ReArmsAfterLogRotation(t *testing.T) {
	resetGrokBillingAttribution(t)
	base := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", base)
	start := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	ensureGrokBillingAttribution(start)
	// Grok rotates the log out from under us.
	if err := os.WriteFile(grokBillingLogPath(base), []byte(`{"msg":"log rotated"}`+"\n"), 0o600); err != nil {
		t.Fatalf("rotate log: %v", err)
	}
	if got := grokIdentityLineCount(t, base); got != 0 {
		t.Fatalf("identity lines = %d after rotation, want 0", got)
	}

	// A recheck inside the window is deliberately cheap and does nothing; the
	// next one past it notices the rotation and self-heals.
	ensureGrokBillingAttribution(start.Add(time.Minute))
	if got := grokIdentityLineCount(t, base); got != 0 {
		t.Fatalf("identity lines = %d, want no read/write inside the recheck window", got)
	}
	ensureGrokBillingAttribution(start.Add(grokBillingAttributionRecheck + time.Minute))
	if got := grokIdentityLineCount(t, base); got != 1 {
		t.Fatalf("identity lines = %d, want attribution restored after rotation", got)
	}

	helperAppendGrokLogLine(t, base,
		grokUnmeteredLine("2026-08-19T13:05:00Z", grokBillingLogMessage))
	if _, ok := readGrokBillingSnapshot(base, grokIdentityCandidates(base)); !ok {
		t.Fatal("post-rotation records must be attributable again")
	}
}

// The terminal-managed acceptance: a child that logged an unmetered record into
// its isolated home leaves a confirmed-unmetered metric in the persistent home.
func TestPersistGrokManagedBillingSnapshot_ManagedRunLeavesAConfirmedUnmeteredMetric(t *testing.T) {
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	isolated := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", persistent)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	if err := seedGrokManagedBillingIdentity(isolated); err != nil {
		t.Fatalf("seed isolated identity: %v", err)
	}
	helperAppendGrokLogLine(t, isolated,
		grokUnmeteredLine("2026-08-19T11:58:00Z", grokBillingLogMessage))

	outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if outcome != grokManagedBillingPersisted {
		t.Fatalf("outcome = %s, want %s", outcome, grokManagedBillingPersisted)
	}

	usage, ok := grokUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatal("Parse failed")
	}
	if len(usage.Metrics) != 1 || !usage.Metrics[0].Unknown {
		t.Fatalf("want one confirmed-unmetered row, got %+v", usage.Metrics)
	}
	if usage.Metrics[0].ObservedAt != "2026-08-19T11:58:00Z" {
		t.Fatalf("ObservedAt = %q, want the managed run's observation", usage.Metrics[0].ObservedAt)
	}
	if usage.Plan != "SuperGrok" {
		t.Fatalf("Plan = %q, want the tier carried through the merge", usage.Plan)
	}
}

// A managed run that never fetched credits reports the no-record outcome and
// leaves an existing good reading alone — it must never downgrade the card.
func TestPersistGrokManagedBillingSnapshot_NoRecordLeavesThePriorReadingIntact(t *testing.T) {
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	isolated := helperGrokHomeWithAccount(t, "acct-1")

	if err := appendGrokBillingIdentity(persistent); err != nil {
		t.Fatalf("seed persistent identity: %v", err)
	}
	helperAppendGrokLogLine(t, persistent,
		grokUnmeteredLine("2026-08-19T11:00:00Z", grokBillingLogMessage))
	before, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read persistent log: %v", err)
	}

	// The isolated home has an identity but the child never fetched billing.
	if err := seedGrokManagedBillingIdentity(isolated); err != nil {
		t.Fatalf("seed isolated identity: %v", err)
	}
	outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if outcome != grokManagedBillingNoRecord {
		t.Fatalf("outcome = %s, want %s", outcome, grokManagedBillingNoRecord)
	}

	after, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read persistent log: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("a no-record run rewrote the persistent log:\nbefore=%s\nafter=%s", before, after)
	}
	snap, ok := readGrokBillingSnapshot(persistent, grokIdentityCandidates(persistent))
	if !ok || snap.ObservedAt.UTC().Format(time.RFC3339) != "2026-08-19T11:00:00Z" {
		t.Fatalf("the prior reading must survive a no-record managed run: %+v ok=%v", snap, ok)
	}
}

// Seeding attribution never widens the account boundary: a record produced by a
// different identity is still refused.
func TestEnsureGrokBillingAttribution_DoesNotAdoptAnotherAccountsRecord(t *testing.T) {
	resetGrokBillingAttribution(t)
	base := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", base)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	ensureGrokBillingAttribution(now)
	// A different account's session logs its own identity and fetches billing.
	helperAppendGrokLogLine(t, base, `{"ts":"2026-08-19T12:01:00Z","msg":"session start","ctx":{"user_id":"acct-9"}}`)
	helperAppendGrokLogLine(t, base,
		grokUnmeteredLine("2026-08-19T12:02:00Z", grokBillingLogMessage))

	if snap, ok := readGrokBillingSnapshot(base, grokIdentityCandidates(base)); ok {
		t.Fatalf("a foreign record must not inherit our identity line: %+v", snap)
	}

	// And a gather is a pure read: it must not append attribution on its own.
	linesBefore := grokIdentityLineCount(t, base)
	_, parsed := grokUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !parsed {
		t.Fatal("Parse failed")
	}
	if got := grokIdentityLineCount(t, base); got != linesBefore {
		t.Fatalf("identity lines = %d, want %d — the append must happen only at session start",
			got, linesBefore)
	}
}
