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

// resetGrokBillingAttribution puts the process-wide append guard back into a
// clean state so each test starts from a cold agent.
func resetGrokBillingAttribution(t *testing.T) {
	t.Helper()
	// Deliberately a no-op body: the guard keeps NO in-process state, because
	// any cached "already attributed" answer is a window in which a newer
	// identity can displace ours unnoticed. The provider log is the only state,
	// and each test builds its own. Kept as the call site every attribution test
	// starts from, so re-introducing process state has one place to reset.
	_ = t
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

	ensureGrokBillingAttribution()
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

	ensureGrokBillingAttribution()
	ensureGrokBillingAttribution()
	// Our line is still the newest identity in the log: verify, do not write.
	ensureGrokBillingAttribution()
	if got := grokIdentityLineCount(t, base); got != 1 {
		t.Fatalf("identity lines = %d, want 1 — the guard appended on an unchanged log", got)
	}

	helperWriteJSON(t, filepath.Join(base, "auth.json"), map[string]any{"user_id": "acct-2"})
	ensureGrokBillingAttribution()
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

	ensureGrokBillingAttribution()
	// Grok rotates the log out from under us.
	if err := os.WriteFile(grokBillingLogPath(base), []byte(`{"msg":"log rotated"}`+"\n"), 0o600); err != nil {
		t.Fatalf("rotate log: %v", err)
	}
	if got := grokIdentityLineCount(t, base); got != 0 {
		t.Fatalf("identity lines = %d after rotation, want 0", got)
	}

	// The very next session start notices the rotation and self-heals. There is
	// no grace window: a direct run inside one would write records nothing in
	// the log identifies, and those are refused outright.
	ensureGrokBillingAttribution()
	if got := grokIdentityLineCount(t, base); got != 1 {
		t.Fatalf("identity lines = %d, want attribution restored on the next session start", got)
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

	ensureGrokBillingAttribution()
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

// A managed session for another account persists ITS identity after ours. That
// newer marker is the one grokRecordBelongsToCurrentAccount binds later records
// to, so the guard must re-append rather than settle for our older marker still
// being somewhere in the tail — otherwise every subsequent direct record for the
// signed-in account is attributed to the other one until the stale marker ages
// out of the 1 MiB tail. The displacing marker lands moments after our own, so
// this also pins that there is no "recently verified" grace window in which a
// direct session skips the check — such a window is precisely how a managed
// account-B exit made the next direct account-A run unobservable.
func TestEnsureGrokBillingAttribution_ReArmsWhenANewerIdentityDisplacesOurs(t *testing.T) {
	resetGrokBillingAttribution(t)
	base := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", base)

	ensureGrokBillingAttribution()
	if got := grokIdentityLineCount(t, base); got != 1 {
		t.Fatalf("identity lines = %d, want 1", got)
	}

	// A managed acct-2 session exits and persists its own producer identity.
	foreign, err := grokBillingIdentityLine("acct-2")
	if err != nil {
		t.Fatalf("build foreign identity line: %v", err)
	}
	helperAppendGrokLogLine(t, base, string(foreign))
	if grokBillingIdentityIsNewest(base, "acct-1") {
		t.Fatal("acct-1 must not read as the newest identity once acct-2 logged one")
	}

	ensureGrokBillingAttribution()
	if got := grokIdentityLineCount(t, base); got != 3 {
		t.Fatalf("identity lines = %d, want 3 — the displaced account was not re-attributed", got)
	}

	helperAppendGrokLogLine(t, base,
		grokUnmeteredLine("2026-08-19T13:05:00Z", grokBillingLogMessage))
	snap, ok := readGrokBillingSnapshot(base, grokIdentityCandidates(base))
	if !ok {
		t.Fatal("the direct record written after the re-append must be attributable to acct-1")
	}
	if got := snap.ObservedAt.UTC().Format(time.RFC3339); got != "2026-08-19T13:05:00Z" {
		t.Fatalf("ObservedAt = %s, want the newest record", got)
	}
}

// A newer identity-shaped line the reader cannot decode (malformed, or a
// partially written one) is DISPLACEMENT, not noise: grokRecordBelongsToCurrentAccount
// stops and refuses on it rather than falling back to older evidence, so an
// older matching marker sitting behind it does not keep later records
// attributable. The guard must re-append instead of reading the log as
// still-ours.
func TestEnsureGrokBillingAttribution_ReArmsWhenANewerIdentityIsUndecodable(t *testing.T) {
	resetGrokBillingAttribution(t)
	base := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", base)

	ensureGrokBillingAttribution()
	if got := grokIdentityLineCount(t, base); got != 1 {
		t.Fatalf("identity lines = %d, want 1", got)
	}

	// A truncated write leaves an identity-shaped line that is not valid JSON.
	helperAppendGrokLogLine(t, base, `{"msg":"session start","ctx":{"user_id":"acct-`)
	if grokBillingIdentityIsNewest(base, "acct-1") {
		t.Fatal("an undecodable newer identity line must not leave acct-1 reading as newest")
	}

	ensureGrokBillingAttribution()
	if got := grokIdentityLineCount(t, base); got != 2 {
		t.Fatalf("identity lines = %d, want 2 — the corrective append was suppressed", got)
	}

	helperAppendGrokLogLine(t, base,
		grokUnmeteredLine("2026-08-19T13:05:00Z", grokBillingLogMessage))
	snap, ok := readGrokBillingSnapshot(base, grokIdentityCandidates(base))
	if !ok {
		t.Fatal("the record written after the re-append must be attributable to acct-1")
	}
	if got := snap.ObservedAt.UTC().Format(time.RFC3339); got != "2026-08-19T13:05:00Z" {
		t.Fatalf("ObservedAt = %s, want the newest record", got)
	}
}

// A record dated beyond grokBillingMaxClockSkew — a clock correction, a bad
// container clock — is refused by the READ path, so it can never be published.
// Letting it block merges too would freeze managed usage at Unknown until
// wall-clock caught up with the bogus timestamp. The two paths have to distrust
// the same records.
func TestPersistGrokManagedBillingSnapshot_MergesPastAnUntrustedFutureRecord(t *testing.T) {
	resetGrokBillingAttribution(t)
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	isolated := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", persistent)

	if err := appendGrokBillingIdentity(persistent); err != nil {
		t.Fatalf("seed persistent identity: %v", err)
	}
	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	helperAppendGrokLogLine(t, persistent,
		grokBillingLine(future, 52, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	// A perfectly good fresh managed fetch, dated now.
	if err := seedGrokManagedBillingIdentity(isolated); err != nil {
		t.Fatalf("seed isolated identity: %v", err)
	}
	observed := time.Now().UTC().Format(time.RFC3339)
	helperAppendGrokLogLine(t, isolated,
		grokBillingLine(observed, 12, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if outcome != grokManagedBillingPersisted {
		t.Fatalf("outcome = %s, want %s — an unpublishable future record blocked a "+
			"fresh managed observation, leaving the card Unknown until that time arrives",
			outcome, grokManagedBillingPersisted)
	}

	usage, ok := grokUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, time.Now())
	if !ok {
		t.Fatal("Parse failed")
	}
	if len(usage.Metrics) != 1 || usage.Metrics[0].ObservedAt != observed {
		t.Fatalf("want the merged observation published, got %+v", usage.Metrics)
	}
}

// A managed session can fetch credits early and exit long after a direct run has
// written a NEWER observation. readGrokBillingSnapshot takes the last billing
// line by FILE ORDER, so appending the older managed receipt last would replace
// the newer direct percentage with a stale one until the next fetch.
func TestPersistGrokManagedBillingSnapshot_DoesNotSupersedeANewerObservation(t *testing.T) {
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	isolated := helperGrokHomeWithAccount(t, "acct-1")

	// A direct run's fresh percentage is already the newest thing in the log.
	if err := appendGrokBillingIdentity(persistent); err != nil {
		t.Fatalf("seed persistent identity: %v", err)
	}
	helperAppendGrokLogLine(t, persistent,
		grokBillingLine("2026-08-19T12:00:00Z", 52, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))
	before, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read persistent log: %v", err)
	}

	// The managed child fetched an HOUR EARLIER but only exits now.
	if err := seedGrokManagedBillingIdentity(isolated); err != nil {
		t.Fatalf("seed isolated identity: %v", err)
	}
	helperAppendGrokLogLine(t, isolated,
		grokBillingLine("2026-08-19T11:00:00Z", 12, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if outcome != grokManagedBillingSuperseded {
		t.Fatalf("outcome = %s, want %s", outcome, grokManagedBillingSuperseded)
	}

	after, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read persistent log: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("an older managed receipt rewrote the log:\nbefore=%s\nafter=%s", before, after)
	}
	snap, ok := readGrokBillingSnapshot(persistent, grokIdentityCandidates(persistent))
	if !ok || !snap.HasUsedPercent || snap.UsedPercent != 52 {
		t.Fatalf("the newer direct observation must survive the merge: %+v ok=%v", snap, ok)
	}
}

// The guard is a staleness check, not a blanket refusal: a managed receipt that
// is genuinely newer than everything in the persistent log still merges.
func TestPersistGrokManagedBillingSnapshot_MergesANewerObservation(t *testing.T) {
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	isolated := helperGrokHomeWithAccount(t, "acct-1")

	if err := appendGrokBillingIdentity(persistent); err != nil {
		t.Fatalf("seed persistent identity: %v", err)
	}
	helperAppendGrokLogLine(t, persistent,
		grokBillingLine("2026-08-19T11:00:00Z", 12, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	if err := seedGrokManagedBillingIdentity(isolated); err != nil {
		t.Fatalf("seed isolated identity: %v", err)
	}
	helperAppendGrokLogLine(t, isolated,
		grokBillingLine("2026-08-19T12:00:00Z", 52, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if outcome != grokManagedBillingPersisted {
		t.Fatalf("outcome = %s, want %s", outcome, grokManagedBillingPersisted)
	}
	snap, ok := readGrokBillingSnapshot(persistent, grokIdentityCandidates(persistent))
	if !ok || !snap.HasUsedPercent || snap.UsedPercent != 52 {
		t.Fatalf("the newer managed observation must win: %+v ok=%v", snap, ok)
	}
}

// Re-persisting the same session's snapshot writes nothing the second time: an
// EQUAL timestamp counts as superseded, so a retried merge is idempotent and
// cannot pile duplicate records into a provider-owned log.
func TestPersistGrokManagedBillingSnapshot_IsIdempotentForTheSameRecord(t *testing.T) {
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	isolated := helperGrokHomeWithAccount(t, "acct-1")

	if err := seedGrokManagedBillingIdentity(isolated); err != nil {
		t.Fatalf("seed isolated identity: %v", err)
	}
	helperAppendGrokLogLine(t, isolated,
		grokUnmeteredLine("2026-08-19T11:58:00Z", grokBillingLogMessage))

	if outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent); err != nil ||
		outcome != grokManagedBillingPersisted {
		t.Fatalf("first persist: outcome=%s err=%v", outcome, err)
	}
	first, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read persistent log: %v", err)
	}

	if outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent); err != nil ||
		outcome != grokManagedBillingSuperseded {
		t.Fatalf("second persist: outcome=%s err=%v, want %s", outcome, err, grokManagedBillingSuperseded)
	}
	second, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read persistent log: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("a repeated merge duplicated the record:\nfirst=%s\nsecond=%s", first, second)
	}
}
