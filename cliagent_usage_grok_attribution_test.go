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
	// The guard itself keeps NO in-process state, because any cached "already
	// attributed" answer is a window in which a newer identity can displace ours
	// unnoticed. The provider log is the only state, and each test builds its own.
	//
	// The attribution KEEPER does keep state: live direct runs are arm-counted per
	// account, and a leaked arm (a test that fails before releasing) makes every
	// LATER test look like two accounts overlapping, so they all write the
	// contested marker instead of a real one. Clear it here rather than debugging
	// that cascade again.
	grokAttributionKeeperMu.Lock()
	grokAttributionKeeperRefs = 0
	grokAttributionKeeperAccounts = map[string]grokAttributionKeeperAccount{}
	stop := grokAttributionKeeperStop
	grokAttributionKeeperStop = nil
	grokAttributionKeeperMu.Unlock()
	if stop != nil {
		close(stop)
	}
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

	ensureGrokBillingAttribution(grokDirectRunLaunch{})
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

	ensureGrokBillingAttribution(grokDirectRunLaunch{})
	ensureGrokBillingAttribution(grokDirectRunLaunch{})
	// Our line is still the newest identity in the log: verify, do not write.
	ensureGrokBillingAttribution(grokDirectRunLaunch{})
	if got := grokIdentityLineCount(t, base); got != 1 {
		t.Fatalf("identity lines = %d, want 1 — the guard appended on an unchanged log", got)
	}

	helperWriteJSON(t, filepath.Join(base, "auth.json"), map[string]any{"user_id": "acct-2"})
	ensureGrokBillingAttribution(grokDirectRunLaunch{})
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

	ensureGrokBillingAttribution(grokDirectRunLaunch{})
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
	ensureGrokBillingAttribution(grokDirectRunLaunch{})
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

	outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent, false)
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
	outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent, false)
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

	ensureGrokBillingAttribution(grokDirectRunLaunch{})
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

	ensureGrokBillingAttribution(grokDirectRunLaunch{})
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

	ensureGrokBillingAttribution(grokDirectRunLaunch{})
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

	ensureGrokBillingAttribution(grokDirectRunLaunch{})
	if got := grokIdentityLineCount(t, base); got != 1 {
		t.Fatalf("identity lines = %d, want 1", got)
	}

	// A truncated write leaves an identity-shaped line that is not valid JSON.
	helperAppendGrokLogLine(t, base, `{"msg":"session start","ctx":{"user_id":"acct-`)
	if grokBillingIdentityIsNewest(base, "acct-1") {
		t.Fatal("an undecodable newer identity line must not leave acct-1 reading as newest")
	}

	ensureGrokBillingAttribution(grokDirectRunLaunch{})
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

	outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent, false)
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

	outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent, false)
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

	outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent, false)
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

	if outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent, false); err != nil ||
		outcome != grokManagedBillingPersisted {
		t.Fatalf("first persist: outcome=%s err=%v", outcome, err)
	}
	first, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read persistent log: %v", err)
	}

	if outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent, false); err != nil ||
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

// The supersession guard has to find the newest record belonging to THIS
// account, not the newest record in the log. When another account's record sits
// on top of ours, a global latest-only read reports "this account has nothing"
// and the guard lets an hour-old managed receipt be appended last — after which
// every gather for this account publishes the stale percentage.
func TestPersistGrokManagedBillingSnapshot_LooksPastAnInterveningAccountsRecord(t *testing.T) {
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	isolated := helperGrokHomeWithAccount(t, "acct-1")

	// Our fresh observation...
	if err := appendGrokBillingIdentity(persistent); err != nil {
		t.Fatalf("seed persistent identity: %v", err)
	}
	helperAppendGrokLogLine(t, persistent,
		grokBillingLine("2026-08-19T12:00:00Z", 52, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))
	// ...buried under a DIFFERENT account's later one.
	if err := appendGrokBillingIdentityValue(persistent, "acct-2"); err != nil {
		t.Fatalf("seed foreign identity: %v", err)
	}
	helperAppendGrokLogLine(t, persistent,
		grokBillingLine("2026-08-19T12:30:00Z", 7, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))
	before, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read persistent log: %v", err)
	}

	// A managed acct-1 session that fetched credits an hour before ours exits now.
	if err := seedGrokManagedBillingIdentity(isolated); err != nil {
		t.Fatalf("seed isolated identity: %v", err)
	}
	helperAppendGrokLogLine(t, isolated,
		grokBillingLine("2026-08-19T11:00:00Z", 12, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent, false)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if outcome != grokManagedBillingSuperseded {
		t.Fatalf("outcome = %s, want %s — an intervening account's record hid our "+
			"newer observation from the staleness guard", outcome, grokManagedBillingSuperseded)
	}
	after, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read persistent log: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("an older managed receipt rewrote the log:\nbefore=%s\nafter=%s", before, after)
	}
}

// The scoped scan must not turn the guard into a blanket refusal: another
// account's record is not evidence about ours, so a managed receipt newer than
// anything WE have still merges even when a foreign line is the log's newest.
func TestPersistGrokManagedBillingSnapshot_AForeignRecordDoesNotBlockAFreshMerge(t *testing.T) {
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	isolated := helperGrokHomeWithAccount(t, "acct-1")

	if err := appendGrokBillingIdentity(persistent); err != nil {
		t.Fatalf("seed persistent identity: %v", err)
	}
	helperAppendGrokLogLine(t, persistent,
		grokBillingLine("2026-08-19T10:00:00Z", 52, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))
	if err := appendGrokBillingIdentityValue(persistent, "acct-2"); err != nil {
		t.Fatalf("seed foreign identity: %v", err)
	}
	helperAppendGrokLogLine(t, persistent,
		grokBillingLine("2026-08-19T23:00:00Z", 7, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	if err := seedGrokManagedBillingIdentity(isolated); err != nil {
		t.Fatalf("seed isolated identity: %v", err)
	}
	helperAppendGrokLogLine(t, isolated,
		grokBillingLine("2026-08-19T12:00:00Z", 31, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent, false)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if outcome != grokManagedBillingPersisted {
		t.Fatalf("outcome = %s, want %s — a foreign account's newer record must not "+
			"block our own fresher observation", outcome, grokManagedBillingPersisted)
	}
	snap, ok := readGrokBillingSnapshot(persistent, grokIdentityCandidates(persistent))
	if !ok || !snap.HasUsedPercent || snap.UsedPercent != 31 {
		t.Fatalf("the merged observation must be published: %+v ok=%v", snap, ok)
	}
}

// helperNoGrokSystemConfigLayers pins the system TOML layers to empty so a real
// /etc/grok on the build host cannot decide these tests either way.
func helperNoGrokSystemConfigLayers(t *testing.T) {
	t.Helper()
	prev := grokSystemConfigPathsFn
	grokSystemConfigPathsFn = func() []string { return nil }
	t.Cleanup(func() { grokSystemConfigPathsFn = prev })
}

// A direct session inherits the user's shell, so an exported XAI_API_KEY reaches
// the child and may be the credential it bills — while the identity we can
// resolve comes from the cached login, a DIFFERENT account. Naming that login
// would publish the key holder's spend as the login's, so the cached login must
// be withheld and the record left unattributable.
func TestEnsureGrokBillingAttribution_WithholdsTheLoginWhenAnAPIKeyOverrideIsActive(t *testing.T) {
	resetGrokBillingAttribution(t)
	helperNoGrokSystemConfigLayers(t)
	base := helperGrokHomeWithAccount(t, "acct-login")
	t.Setenv("GROK_HOME", base)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	childEnv := []string{"PATH=/usr/bin", "XAI_API_KEY=xai-credential-sentinel"}
	if got := grokDirectRunBillingIdentity(grokDirectRunLaunch{Env: childEnv}, base); got != grokContestedBillingIdentity {
		t.Fatalf("identity = %q, want the contested sentinel", got)
	}
	ensureGrokBillingAttribution(grokDirectRunLaunch{Env: childEnv})

	// The key's account — not the cached login's — then fetches billing.
	helperAppendGrokLogLine(t, base,
		grokUnmeteredLine("2026-08-19T11:59:00Z", grokBillingLogMessage))

	if snap, ok := readGrokBillingSnapshot(base, grokIdentityCandidates(base)); ok {
		t.Fatalf("an API-key run's record must not bind to the cached login: %+v", snap)
	}
	usage, parsed := grokUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !parsed {
		t.Fatal("Parse failed")
	}
	if len(usage.Metrics) != 1 || !usage.Metrics[0].Unknown || usage.Metrics[0].ObservedAt != "" {
		t.Fatalf("want the inferred placeholder with no observation time, got %+v", usage.Metrics)
	}

	// And the marker itself never carries credential material.
	raw, err := os.ReadFile(grokBillingLogPath(base))
	if err != nil {
		t.Fatalf("read unified.jsonl: %v", err)
	}
	if strings.Contains(string(raw), "xai-credential-sentinel") {
		t.Fatal("the API key leaked into the provider log")
	}
	if !strings.Contains(string(raw), grokContestedBillingIdentity) {
		t.Fatalf("the contested marker was not written: %s", raw)
	}
}

// The override need not be in the environment: a direct child reads the user's
// REAL config.toml (the ACP path neutralises it, this one cannot), and a
// per-model `api_key` there is just as much a credential the cached login did
// not pay for. A direct run has no resolved model, so ANY pinned key counts.
func TestGrokDirectRunBillingIdentity_ContestsAPersistedConfigKey(t *testing.T) {
	resetGrokBillingAttribution(t)
	helperNoGrokSystemConfigLayers(t)
	base := helperGrokHomeWithAccount(t, "acct-login")

	if got := grokDirectRunBillingIdentity(grokDirectRunLaunch{}, base); got != "acct-login" {
		t.Fatalf("identity = %q, want the cached login when nothing overrides it", got)
	}

	if err := os.WriteFile(filepath.Join(base, "config.toml"),
		[]byte("[model.grok-4-fast]\napi_key = \"xai-persisted-sentinel\"\n"), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
	if got := grokDirectRunBillingIdentity(grokDirectRunLaunch{}, base); got != grokContestedBillingIdentity {
		t.Fatalf("identity = %q, want the contested sentinel for a pinned per-model key", got)
	}

	// An empty value is not a credential — it must not cost a honest run its
	// observability.
	if err := os.WriteFile(filepath.Join(base, "config.toml"),
		[]byte("[model]\napi_key = \"\"\n"), 0o600); err != nil {
		t.Fatalf("rewrite config.toml: %v", err)
	}
	if got := grokDirectRunBillingIdentity(grokDirectRunLaunch{}, base); got != "acct-login" {
		t.Fatalf("identity = %q, want the cached login for an empty api_key", got)
	}
}

// An override run ARMS the sentinel, not just writes it once. That is what makes
// a concurrent honest run contested too — otherwise the honest run's marker
// would stand as the newest one and the override run's next record would bind
// to it, which is the same misattribution by a longer route.
func TestEnsureGrokBillingAttribution_AnArmedOverrideRunContestsAConcurrentRun(t *testing.T) {
	resetGrokBillingAttribution(t)
	helperNoGrokSystemConfigLayers(t)
	base := helperGrokHomeWithAccount(t, "acct-login")
	t.Setenv("GROK_HOME", base)

	release := startGrokBillingAttributionKeeper(grokContestedBillingIdentity)
	defer release()

	// A second, credential-clean direct run starts for the signed-in account.
	ensureGrokBillingAttribution(grokDirectRunLaunch{})

	raw, err := os.ReadFile(grokBillingLogPath(base))
	if err != nil {
		t.Fatalf("read unified.jsonl: %v", err)
	}
	if strings.Contains(string(raw), "acct-login") {
		t.Fatalf("the honest account was named while an override run was live: %s", raw)
	}
	if !strings.Contains(string(raw), grokContestedBillingIdentity) {
		t.Fatalf("the contested marker was not written: %s", raw)
	}
}

// A merge is correct at the time it is taken and can be wrong later: ownership
// of the reader moves with a `grok login`. A managed acct-1 merge made while the
// persistent credentials still resolved to acct-1 leaves `acct-2 record, acct-1
// record, acct-2 marker` for a live direct acct-2 run — right then, because
// acct-2's record is older and acct-1 is the account reading here. After the
// user switches the shared home back to acct-2, every gather stops at acct-1's
// foreign record and publishes nothing, while the trailing acct-2 marker makes
// the keeper report attribution intact forever. The re-assertion has to restore
// the reader's own newest record, not just check the marker.
func TestReassertGrokDirectAttribution_RestoresTheReadersRecordAfterALoginSwitch(t *testing.T) {
	resetGrokBillingAttribution(t)
	base := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", base)

	// The direct acct-2 run's own (older) observation.
	if err := appendGrokBillingIdentityValue(base, "acct-2"); err != nil {
		t.Fatalf("seed direct identity: %v", err)
	}
	helperAppendGrokLogLine(t, base,
		grokBillingLine("2026-08-19T10:00:00Z", 41, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))
	// The managed acct-1 merge that landed on top of it, ending — correctly for
	// the state at that moment — with the direct account's bare marker.
	if err := appendGrokBillingIdentityValue(base, "acct-1"); err != nil {
		t.Fatalf("seed managed identity: %v", err)
	}
	helperAppendGrokLogLine(t, base,
		grokBillingLine("2026-08-19T11:00:00Z", 9, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))
	if err := appendGrokBillingIdentityValue(base, "acct-2"); err != nil {
		t.Fatalf("seed trailing direct marker: %v", err)
	}

	// The login switch: the shared home now reads under acct-2.
	helperWriteJSON(t, filepath.Join(base, "auth.json"), map[string]any{"user_id": "acct-2"})

	candidates := grokIdentityCandidates(base)
	if _, ok := readGrokBillingSnapshot(base, candidates); ok {
		t.Fatal("precondition: acct-2 should be dark before the repair — the fixture no longer reproduces the defect")
	}
	if !grokBillingIdentityIsNewest(base, "acct-2") {
		t.Fatal("precondition: the trailing marker should already name acct-2, which is what suppressed the repair")
	}

	finish := startGrokBillingAttributionKeeper("acct-2")
	defer finish()
	reassertGrokDirectAttribution()

	snap, ok := readGrokBillingSnapshot(base, candidates)
	if !ok {
		t.Fatal("acct-2 still publishes nothing: the re-assertion checked only the marker")
	}
	want, err := time.Parse(time.RFC3339, "2026-08-19T10:00:00Z")
	if err != nil {
		t.Fatalf("parse want: %v", err)
	}
	if !snap.ObservedAt.Equal(want) {
		t.Fatalf("ObservedAt = %s, want the reader's own newest record at %s", snap.ObservedAt, want)
	}

	// Idempotent: a healthy log costs a read and no append, so the keeper does
	// not grow a provider-owned file one line per tick.
	before, err := os.ReadFile(grokBillingLogPath(base))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	reassertGrokDirectAttribution()
	after, err := os.ReadFile(grokBillingLogPath(base))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("a second re-assertion rewrote a healthy log:\nbefore=%s\nafter=%s", before, after)
	}
}

// The repair must not fire for an account that is NOT the one reading here:
// moving an older record last while its owner can still be read republishes a
// stale reading, which is the failure the supersession guard exists to prevent.
func TestReassertGrokDirectAttribution_LeavesTheLogAloneWhileTheMergedAccountReads(t *testing.T) {
	resetGrokBillingAttribution(t)
	base := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", base)

	if err := appendGrokBillingIdentityValue(base, "acct-2"); err != nil {
		t.Fatalf("seed direct identity: %v", err)
	}
	helperAppendGrokLogLine(t, base,
		grokBillingLine("2026-08-19T10:00:00Z", 41, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))
	if err := appendGrokBillingIdentityValue(base, "acct-1"); err != nil {
		t.Fatalf("seed managed identity: %v", err)
	}
	helperAppendGrokLogLine(t, base,
		grokBillingLine("2026-08-19T11:00:00Z", 9, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))
	if err := appendGrokBillingIdentityValue(base, "acct-2"); err != nil {
		t.Fatalf("seed trailing direct marker: %v", err)
	}

	before, err := os.ReadFile(grokBillingLogPath(base))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}

	finish := startGrokBillingAttributionKeeper("acct-2")
	defer finish()
	reassertGrokDirectAttribution()

	after, err := os.ReadFile(grokBillingLogPath(base))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("the repair displaced a readable account's newer record:\nbefore=%s\nafter=%s", before, after)
	}
}

// grokMalformedBillingLine is a RECOGNIZED billing response the publish reader
// cannot render — here an unparseable `ts`. It is the provider's newest answer
// all the same, so readGrokBillingSnapshot fails closed on it rather than
// reviving an older percentage, and the repairs below must respect that.
func grokMalformedBillingLine() string {
	return grokBillingLine("not-a-timestamp", 77, "USAGE_PERIOD_TYPE_WEEKLY",
		"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z")
}

// The supersession scan walks PAST a response it cannot decode to an older
// record; the publish reader stops dead at it. The re-assertion sits between
// them, so a bare "the reader published nothing" was ambiguous: it is the repair
// signal for a FOREIGN newest record and a fail-closed refusal for an
// undecodable one. Re-appending the older record here would put it last and hand
// the next gather the stale percentage the reader had just refused.
func TestReassertGrokDirectAttribution_LeavesAMalformedNewestResponseStanding(t *testing.T) {
	resetGrokBillingAttribution(t)
	base := helperGrokHomeWithAccount(t, "acct-2")
	t.Setenv("GROK_HOME", base)

	if err := appendGrokBillingIdentityValue(base, "acct-2"); err != nil {
		t.Fatalf("seed direct identity: %v", err)
	}
	helperAppendGrokLogLine(t, base,
		grokBillingLine("2026-08-19T10:00:00Z", 41, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))
	if err := appendGrokBillingIdentityValue(base, "acct-2"); err != nil {
		t.Fatalf("seed identity for the malformed response: %v", err)
	}
	helperAppendGrokLogLine(t, base, grokMalformedBillingLine())

	candidates := grokIdentityCandidates(base)
	if _, ok := readGrokBillingSnapshot(base, candidates); ok {
		t.Fatal("precondition: the malformed newest response should make the reader fail closed")
	}
	if _, ok := newestTrustedGrokBillingRecordFor(
		base, candidates, time.Now().Add(grokBillingMaxClockSkew)); !ok {
		t.Fatal("precondition: the supersession scan should still see the older record — the fixture no longer reproduces the defect")
	}

	before, err := os.ReadFile(grokBillingLogPath(base))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}

	finish := startGrokBillingAttributionKeeper("acct-2")
	defer finish()
	reassertGrokDirectAttribution()

	after, err := os.ReadFile(grokBillingLogPath(base))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("the repair revived a record the newest response had superseded:\nbefore=%s\nafter=%s", before, after)
	}
	if _, ok := readGrokBillingSnapshot(base, candidates); ok {
		t.Fatal("a stale percentage became publishable again after the repair")
	}
}

// The merge-time twin of the case above: preserving the armed direct account's
// own newest record is right when its owner is the one reading here, but not
// when a recognized response it cannot decode already supersedes it. The run
// falls back to the bare marker so the NEXT direct record is still attributable.
func TestGrokDirectBillingPairToPreserve_DeclinesUnderAMalformedNewestResponse(t *testing.T) {
	resetGrokBillingAttribution(t)
	base := helperGrokHomeWithAccount(t, "acct-2")
	t.Setenv("GROK_HOME", base)

	if err := appendGrokBillingIdentityValue(base, "acct-2"); err != nil {
		t.Fatalf("seed direct identity: %v", err)
	}
	helperAppendGrokLogLine(t, base,
		grokBillingLine("2026-08-19T10:00:00Z", 41, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	merged, err := time.Parse(time.RFC3339, "2026-08-19T11:00:00Z")
	if err != nil {
		t.Fatalf("parse merged time: %v", err)
	}

	// Without the malformed response the direct account owns the reader, so its
	// older record IS preserved — the arm this test then has to see declined.
	grokBillingAttributionSerialize.Lock()
	_, ok := grokDirectBillingPairToPreserve(base, "acct-2", "acct-1", merged)
	grokBillingAttributionSerialize.Unlock()
	if !ok {
		t.Fatal("precondition: the reader's own record should be preserved while nothing supersedes it")
	}

	if err := appendGrokBillingIdentityValue(base, "acct-2"); err != nil {
		t.Fatalf("seed identity for the malformed response: %v", err)
	}
	helperAppendGrokLogLine(t, base, grokMalformedBillingLine())

	grokBillingAttributionSerialize.Lock()
	_, ok = grokDirectBillingPairToPreserve(base, "acct-2", "acct-1", merged)
	grokBillingAttributionSerialize.Unlock()
	if ok {
		t.Fatal("an older record was preserved over a newest response the reader refuses to render")
	}
}

// The producer boundary the two tests above stop at: an undecodable response is
// only authoritative over the account that FETCHED it. When account B's response
// is the newest recognized line but our armed account A has a good record of its
// own below it, refusing to restore A's record leaves A blind for no reason —
// the same file layout with a USABLE foreign record is repaired over already.
func TestReassertGrokDirectAttribution_RepairsUnderAForeignMalformedResponse(t *testing.T) {
	resetGrokBillingAttribution(t)
	base := helperGrokHomeWithAccount(t, "acct-2")
	t.Setenv("GROK_HOME", base)

	if err := appendGrokBillingIdentityValue(base, "acct-2"); err != nil {
		t.Fatalf("seed direct identity: %v", err)
	}
	helperAppendGrokLogLine(t, base,
		grokBillingLine("2026-08-19T10:00:00Z", 41, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))
	// Another account then fetched billing here and its response is unreadable.
	if err := appendGrokBillingIdentityValue(base, "acct-9"); err != nil {
		t.Fatalf("seed the foreign producer: %v", err)
	}
	helperAppendGrokLogLine(t, base, grokMalformedBillingLine())

	candidates := grokIdentityCandidates(base)
	if _, ok := readGrokBillingSnapshot(base, candidates); ok {
		t.Fatal("precondition: the reader stops at the foreign newest response")
	}

	finish := startGrokBillingAttributionKeeper("acct-2")
	defer finish()
	reassertGrokDirectAttribution()

	snap, ok := readGrokBillingSnapshot(base, candidates)
	if !ok {
		t.Fatal("the armed account's own record stayed unpublishable under ANOTHER account's unreadable response")
	}
	if got := snap.ObservedAt.UTC().Format(time.RFC3339); got != "2026-08-19T10:00:00Z" {
		t.Fatalf("ObservedAt = %s, want the armed account's own newest observation", got)
	}
}

// The merge-time twin: the same foreign unreadable response must not veto
// preserving the armed direct account's record either.
func TestGrokDirectBillingPairToPreserve_IgnoresAForeignMalformedResponse(t *testing.T) {
	resetGrokBillingAttribution(t)
	base := helperGrokHomeWithAccount(t, "acct-2")
	t.Setenv("GROK_HOME", base)

	if err := appendGrokBillingIdentityValue(base, "acct-2"); err != nil {
		t.Fatalf("seed direct identity: %v", err)
	}
	helperAppendGrokLogLine(t, base,
		grokBillingLine("2026-08-19T10:00:00Z", 41, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))
	if err := appendGrokBillingIdentityValue(base, "acct-9"); err != nil {
		t.Fatalf("seed the foreign producer: %v", err)
	}
	helperAppendGrokLogLine(t, base, grokMalformedBillingLine())

	merged, err := time.Parse(time.RFC3339, "2026-08-19T11:00:00Z")
	if err != nil {
		t.Fatalf("parse merged time: %v", err)
	}

	grokBillingAttributionSerialize.Lock()
	_, ok := grokDirectBillingPairToPreserve(base, "acct-2", "acct-1", merged)
	grokBillingAttributionSerialize.Unlock()
	if !ok {
		t.Fatal("a response ANOTHER account could not have produced for us vetoed preserving our own record")
	}
}

// An unattributable unreadable response — no producer marker above it at all —
// stays authoritative. The scoping is a proof of foreign production, never an
// absence of proof.
func TestGrokUnusableOutcome_UnattributableResponseStillSupersedes(t *testing.T) {
	resetGrokBillingAttribution(t)
	base := helperGrokHomeWithAccount(t, "acct-2")
	t.Setenv("GROK_HOME", base)

	// No identity line anywhere above it, so nothing says whose fetch it was —
	// the shape a CLI old enough never to log a producer leaves behind.
	helperAppendGrokLogLine(t, base, grokMalformedBillingLine())

	if !grokNewestBillingResponseIsUnusableFor(base, "acct-2") {
		t.Fatal("an unattributable unreadable response must keep superseding older records")
	}
}
