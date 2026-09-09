// cliagent_usage_grok_attribution_keeper_test.go
// -----------------------------------------------------------------------------
// Attributing once at session start holds only until something else appends an
// identity. The Grok CLI keeps fetching credits for the whole life of a direct
// run, and grokRecordBelongsToCurrentAccount binds each record to the NEAREST
// preceding identity — so a managed exit for another account, or a `grok login`
// outside the agent, silently turns every later record of a live session
// unattributable. These tests pin the run-scoped keeper that repairs that, and
// the serialization that keeps the managed merge from landing inside the direct
// path's read-then-append.
// -----------------------------------------------------------------------------

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// helperGrokIdentityLine renders one producer marker for a given account.
func helperGrokIdentityLine(t *testing.T, account string) string {
	t.Helper()
	line, err := grokBillingIdentityLine(account)
	if err != nil {
		t.Fatalf("render identity line: %v", err)
	}
	return string(line)
}

// helperGrokLastLogLine returns the newest line in a home's unified log. The
// marker that matters is always the last one written: a record binds to the
// nearest identity above it.
func helperGrokLastLogLine(t *testing.T, base string) string {
	t.Helper()
	raw, err := os.ReadFile(grokBillingLogPath(base))
	if err != nil {
		t.Fatalf("read unified.jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	return lines[len(lines)-1]
}

// helperWaitForIdentityLines polls until the log holds want markers, so the test
// asserts on the keeper's observable effect rather than on goroutine timing.
func helperWaitForIdentityLines(t *testing.T, base string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if grokIdentityLineCount(t, base) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("identity lines = %d after 5s, want %d — displaced attribution was never repaired",
		grokIdentityLineCount(t, base), want)
}

// The P1 regression: a displacing identity arrives mid-session, and the records
// the live run writes afterwards must still be attributable to us.
func TestGrokBillingAttributionKeeper_RepairsDisplacementDuringALiveSession(t *testing.T) {
	resetGrokBillingAttribution(t)
	base := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", base)
	t.Setenv(grokBillingAttributionKeeperIntervalEnv, "10ms")

	ensureGrokBillingAttribution()
	if got := grokIdentityLineCount(t, base); got != 1 {
		t.Fatalf("identity lines = %d after session start, want 1", got)
	}

	finish := startGrokBillingAttributionKeeper("acct-1")
	defer finish()

	// Someone else's identity becomes the newest one — the exact displacement a
	// start-only guard can never see.
	helperAppendGrokLogLine(t, base, helperGrokIdentityLine(t, "acct-other"))

	// Three markers, not two: the displacing line carries the same producer
	// message, so waiting for two would be satisfied by the displacement itself
	// and would assert nothing about the repair.
	helperWaitForIdentityLines(t, base, 3)

	// A record the still-running CLI writes now is ours again.
	helperAppendGrokLogLine(t, base,
		grokUnmeteredLine("2026-08-19T12:10:00Z", grokBillingLogMessage))
	usage, ok := grokUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true},
		time.Date(2026, 8, 19, 12, 11, 0, 0, time.UTC))
	if !ok {
		t.Fatal("Parse failed")
	}
	if len(usage.Metrics) != 1 || usage.Metrics[0].ObservedAt != "2026-08-19T12:10:00Z" {
		t.Fatalf("want the post-displacement record attributed to us, got %+v", usage.Metrics)
	}
}

// An undisplaced session must not keep writing into the provider-owned log: the
// keeper verifies every tick but appends only when our marker is not newest.
func TestGrokBillingAttributionKeeper_WritesNothingWhileAttributionHolds(t *testing.T) {
	resetGrokBillingAttribution(t)
	base := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", base)
	t.Setenv(grokBillingAttributionKeeperIntervalEnv, "5ms")

	ensureGrokBillingAttribution()
	finish := startGrokBillingAttributionKeeper("acct-1")
	time.Sleep(120 * time.Millisecond) // many ticks
	finish()

	if got := grokIdentityLineCount(t, base); got != 1 {
		t.Fatalf("identity lines = %d, want 1 — the keeper appended on an undisplaced log", got)
	}
}

// Release is ref-counted across concurrently live direct sessions and idempotent
// per arm, so one session ending cannot stop another's keeper and a double
// release cannot drop the count below zero.
func TestGrokBillingAttributionKeeper_IsRefCountedAndReleaseIsIdempotent(t *testing.T) {
	resetGrokBillingAttribution(t)
	base := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", base)
	t.Setenv(grokBillingAttributionKeeperIntervalEnv, "5ms")

	first := startGrokBillingAttributionKeeper("acct-1")
	second := startGrokBillingAttributionKeeper("acct-1")
	first()
	first() // idempotent

	grokAttributionKeeperMu.Lock()
	refs, running := grokAttributionKeeperRefs, grokAttributionKeeperStop != nil
	grokAttributionKeeperMu.Unlock()
	if refs != 1 || !running {
		t.Fatalf("refs = %d running = %v — one session ending stopped the other's keeper",
			refs, running)
	}

	second()
	grokAttributionKeeperMu.Lock()
	refs, running = grokAttributionKeeperRefs, grokAttributionKeeperStop != nil
	grokAttributionKeeperMu.Unlock()
	if refs != 0 || running {
		t.Fatalf("refs = %d running = %v — the last release did not stop the keeper",
			refs, running)
	}
}

// The account boundary. A direct run keeps writing records under the
// credentials it was SPAWNED with, so a re-assertion that re-read the shared
// home would name a later sign-in above those records and publish one
// account's utilization as another's — strictly worse than the unattributable
// record the keeper exists to prevent.
func TestGrokBillingAttributionKeeper_ReassertsTheAccountTheRunStartedUnder(t *testing.T) {
	resetGrokBillingAttribution(t)
	base := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", base)
	t.Setenv(grokBillingAttributionKeeperIntervalEnv, "10ms")

	ensureGrokBillingAttribution()
	finish := startGrokBillingAttributionKeeper("acct-1")
	defer finish()

	// The user signs in as someone else mid-run. The acct-1 CLI is still live.
	helperWriteJSON(t, filepath.Join(base, "auth.json"), map[string]any{"user_id": "acct-2"})
	helperAppendGrokLogLine(t, base, helperGrokIdentityLine(t, "acct-2"))

	helperWaitForIdentityLines(t, base, 3)

	raw, err := os.ReadFile(grokBillingLogPath(base))
	if err != nil {
		t.Fatalf("read unified.jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(last, "acct-1") || strings.Contains(last, "acct-2") {
		t.Fatalf("keeper re-asserted %q — it must name the account the live run was "+
			"started under, never whoever is signed in now", last)
	}
}

// A live direct run under account A must not be re-labelled by ANY later
// session-start attribution for account B — the direct child keeps writing
// records under A's credentials, and a B marker above them publishes A's usage
// as B's. This is the session-start half of the contested rule: the writer that
// resolves the live credentials is the one that has to notice the conflict.
func TestEnsureGrokBillingAttribution_ContestsInsteadOfNamingANewAccountOverALiveRun(t *testing.T) {
	resetGrokBillingAttribution(t)
	base := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", base)
	// A tick would repair this on its own; the point is that the SESSION START
	// never leaves the wrong marker standing in the first place.
	t.Setenv(grokBillingAttributionKeeperIntervalEnv, "1h")

	ensureGrokBillingAttribution()
	finish := startGrokBillingAttributionKeeper("acct-1")
	defer finish()

	// The user signs in as someone else and any second session starts.
	helperWriteJSON(t, filepath.Join(base, "auth.json"), map[string]any{"user_id": "acct-2"})
	ensureGrokBillingAttribution()

	if last := helperGrokLastLogLine(t, base); !strings.Contains(last, grokContestedBillingIdentity) {
		t.Fatalf("session start named %q above a live acct-1 run — it must write the "+
			"contested marker so neither account claims the other's records", last)
	}

	// The acct-1 child's next record is unattributable rather than published as
	// acct-2's, which is the boundary this rule exists to hold.
	helperAppendGrokLogLine(t, base,
		grokUnmeteredLine(time.Now().UTC().Format(time.RFC3339), grokBillingLogMessage))
	for _, account := range []string{"acct-1", "acct-2"} {
		if _, ok := readGrokBillingSnapshot(base, []string{account}); ok {
			t.Fatalf("a record under the contested marker was attributed to %s", account)
		}
	}
}

// Two live direct runs on DIFFERENT accounts share one log, and a record binds
// to the nearest identity above it, so naming either account would attribute the
// other run's records to it. The keeper asserts the CONTESTED sentinel: every
// record written while they overlap is refused by the reader, which is
// recoverable, where a misattributed one is a billing lie. Asserting nothing is
// not neutral — whatever marker happens to be newest would keep binding both
// runs' records to it.
func TestGrokBillingAttributionKeeper_MarksTheLogContestedWhenArmedRunsDisagree(t *testing.T) {
	resetGrokBillingAttribution(t)
	base := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", base)
	t.Setenv(grokBillingAttributionKeeperIntervalEnv, "5ms")

	ensureGrokBillingAttribution()
	first := startGrokBillingAttributionKeeper("acct-1")
	defer first()
	second := startGrokBillingAttributionKeeper("acct-2")
	released := false
	defer func() {
		if !released {
			second()
		}
	}()

	// Someone else's marker becomes newest while the two runs overlap.
	helperAppendGrokLogLine(t, base, helperGrokIdentityLine(t, "acct-other"))
	helperWaitForIdentityLines(t, base, 3)

	if last := helperGrokLastLogLine(t, base); !strings.Contains(last, grokContestedBillingIdentity) {
		t.Fatalf("keeper asserted %q — with two accounts armed it must name the "+
			"contested sentinel, never one of them", last)
	}

	// A record written under the contested marker is refused for BOTH accounts.
	helperAppendGrokLogLine(t, base,
		grokUnmeteredLine(time.Now().UTC().Format(time.RFC3339), grokBillingLogMessage))
	for _, account := range []string{"acct-1", "acct-2"} {
		if _, ok := readGrokBillingSnapshot(base, []string{account}); ok {
			t.Fatalf("a record under the contested marker was attributed to %s", account)
		}
	}

	// The ambiguity is the second run's doing: once it releases, the surviving
	// run's account is unambiguous again and attribution self-heals.
	second()
	released = true
	helperWaitForIdentityLines(t, base, 4)
	if last := helperGrokLastLogLine(t, base); !strings.Contains(last, "acct-1") ||
		strings.Contains(last, grokContestedBillingIdentity) {
		t.Fatalf("re-asserted %q — the surviving run's account must be named again "+
			"once the conflicting run releases", last)
	}
}

// The managed merge must not be able to land between the direct path's "am I
// newest?" read and its append; both take grokBillingAttributionSerialize.
func TestPersistGrokManagedBillingSnapshot_SerializesWithDirectAttribution(t *testing.T) {
	resetGrokBillingAttribution(t)
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	isolated := helperGrokHomeWithAccount(t, "acct-managed")
	helperAppendGrokLogLine(t, isolated, helperGrokIdentityLine(t, "acct-managed"))
	helperAppendGrokLogLine(t, isolated,
		grokUnmeteredLine("2026-08-19T12:20:00Z", grokBillingLogMessage))

	grokBillingAttributionSerialize.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = persistGrokManagedBillingSnapshot(isolated, persistent)
	}()

	select {
	case <-done:
		grokBillingAttributionSerialize.Unlock()
		t.Fatal("the managed merge appended while the attribution lock was held — a direct session's newest-identity check can go stale between its read and its write")
	case <-time.After(50 * time.Millisecond):
	}

	grokBillingAttributionSerialize.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the managed merge never completed after the lock was released")
	}
	if got := grokIdentityLineCount(t, persistent); got != 1 {
		t.Fatalf("merged identity lines = %d, want 1", got)
	}
}

// The tick honors its test seam and otherwise falls back to the shipped default.
func TestGrokBillingAttributionKeeperInterval_HonorsItsSeam(t *testing.T) {
	t.Setenv(grokBillingAttributionKeeperIntervalEnv, "250ms")
	if got := grokBillingAttributionKeeperIntervalValue(); got != 250*time.Millisecond {
		t.Fatalf("seam → %v, want 250ms", got)
	}
	for _, raw := range []string{"", "nonsense", "-5s", "0s"} {
		t.Setenv(grokBillingAttributionKeeperIntervalEnv, raw)
		if got := grokBillingAttributionKeeperIntervalValue(); got != grokBillingAttributionKeeperInterval {
			t.Fatalf("override %q → %v, want the default %v",
				raw, got, grokBillingAttributionKeeperInterval)
		}
	}
}

// The P2 regression: a relative GROK_HOME must reach the child as the same
// absolute path attribution and the reader use, or the CLI logs into a
// different tree than the one carrying our marker.
func TestGrokDirectChildHomeOverride_PinsARelativeHomeToTheAttributedPath(t *testing.T) {
	root := t.TempDir()
	daemonCwd := filepath.Join(root, "daemon")
	childCwd := filepath.Join(root, "child")
	for _, dir := range []string{daemonCwd, childCwd} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(daemonCwd); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })

	t.Setenv("GROK_HOME", "relative-grok-home")
	resolved := grokDirectChildHomeOverride("relative-grok-home")
	if resolved != grokPersistentHome() || !filepath.IsAbs(resolved) {
		t.Fatalf("override = %q, want the absolute attributed path %q",
			resolved, grokPersistentHome())
	}
	// A child spawned with proc.Dir = childCwd and the inherited RELATIVE value
	// would have resolved GROK_HOME under childCwd — a different tree entirely.
	if inherited := filepath.Join(childCwd, "relative-grok-home"); inherited == resolved {
		t.Fatal("test setup no longer distinguishes the two cwds")
	}
}

// An absolute or unset GROK_HOME already means the same directory to parent and
// child, so the spawn path must leave it exactly as the operator set it.
func TestGrokDirectChildHomeOverride_LeavesAbsoluteAndUnsetHomesAlone(t *testing.T) {
	absolute := t.TempDir()
	t.Setenv("GROK_HOME", absolute)
	if got := grokDirectChildHomeOverride(absolute); got != "" {
		t.Fatalf("absolute GROK_HOME rewritten to %q — want it inherited untouched", got)
	}
	if got := grokDirectChildHomeOverride(""); got != "" {
		t.Fatalf("unset GROK_HOME introduced as %q — want no override", got)
	}
}

// The keeper's first tick is a full interval away, so a displacement WE cause
// must be repaired synchronously: a short direct run that is displaced, writes
// its only billing record and exits inside one interval would otherwise lose
// that record entirely.
func TestPersistGrokManagedBillingSnapshot_RepairsAnArmedDirectRunImmediately(t *testing.T) {
	resetGrokBillingAttribution(t)
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", persistent)
	// An interval far longer than the test: nothing here may depend on a tick.
	t.Setenv(grokBillingAttributionKeeperIntervalEnv, "1h")

	ensureGrokBillingAttribution()
	finish := startGrokBillingAttributionKeeper("acct-1")
	defer finish()

	isolated := helperGrokHomeWithAccount(t, "acct-managed")
	helperAppendGrokLogLine(t, isolated, helperGrokIdentityLine(t, "acct-managed"))
	helperAppendGrokLogLine(t, isolated,
		grokUnmeteredLine("2026-08-19T12:20:00Z", grokBillingLogMessage))

	if outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent); err != nil ||
		outcome != grokManagedBillingPersisted {
		t.Fatalf("persist → %v, %v; want persisted", outcome, err)
	}

	// A record the still-running direct CLI writes right after the merge is
	// ours again — with no keeper tick in between.
	helperAppendGrokLogLine(t, persistent,
		grokUnmeteredLine("2026-08-19T12:21:00Z", grokBillingLogMessage))
	usage, ok := grokUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true},
		time.Date(2026, 8, 19, 12, 22, 0, 0, time.UTC))
	if !ok {
		t.Fatal("Parse failed")
	}
	if len(usage.Metrics) != 1 || usage.Metrics[0].ObservedAt != "2026-08-19T12:21:00Z" {
		t.Fatalf("want the post-merge record attributed to the live direct run, got %+v",
			usage.Metrics)
	}
}

// With no direct run armed, a managed merge must write nothing beyond its own
// pair — the repair is for live sessions only, not a standing extra append into
// a provider-owned file.
func TestPersistGrokManagedBillingSnapshot_DoesNotRepairWhenNoDirectRunIsArmed(t *testing.T) {
	resetGrokBillingAttribution(t)
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", persistent)

	isolated := helperGrokHomeWithAccount(t, "acct-managed")
	helperAppendGrokLogLine(t, isolated, helperGrokIdentityLine(t, "acct-managed"))
	helperAppendGrokLogLine(t, isolated,
		grokUnmeteredLine("2026-08-19T12:20:00Z", grokBillingLogMessage))

	if _, err := persistGrokManagedBillingSnapshot(isolated, persistent); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if got := grokIdentityLineCount(t, persistent); got != 1 {
		t.Fatalf("identity lines = %d, want 1 — the merge repaired attribution with no direct run armed", got)
	}
}

// The repair must ride in the merge's OWN write, not a follow-up append: the
// write lock serializes this process's helpers, not the direct Grok child, so a
// record landing between two appends would bind to the managed account and be
// refused. Pinned by asserting the merged log already ends with the direct
// account's marker the moment persist returns.
func TestPersistGrokManagedBillingSnapshot_RepairsInTheSameAtomicWrite(t *testing.T) {
	resetGrokBillingAttribution(t)
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", persistent)
	t.Setenv(grokBillingAttributionKeeperIntervalEnv, "1h")

	ensureGrokBillingAttribution()
	finish := startGrokBillingAttributionKeeper("acct-1")
	defer finish()

	isolated := helperGrokHomeWithAccount(t, "acct-managed")
	helperAppendGrokLogLine(t, isolated, helperGrokIdentityLine(t, "acct-managed"))
	helperAppendGrokLogLine(t, isolated,
		grokUnmeteredLine("2026-08-19T12:20:00Z", grokBillingLogMessage))

	if outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent); err != nil ||
		outcome != grokManagedBillingPersisted {
		t.Fatalf("persist → %v, %v; want persisted", outcome, err)
	}

	raw, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read unified.jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(last, grokManagedBillingIdentityMessage) ||
		!strings.Contains(last, "acct-1") {
		t.Fatalf("merged log ends with %q — want the direct account's marker as "+
			"the last line, so no record can land unattributed after the merge", last)
	}
	// The managed record must still bind to the managed identity ABOVE it: the
	// repair marker only names the direct account for what comes NEXT.
	if len(lines) < 4 ||
		!strings.Contains(lines[len(lines)-3], grokManagedBillingIdentityMessage) ||
		!strings.Contains(lines[len(lines)-3], "acct-managed") ||
		!strings.Contains(lines[len(lines)-2], grokBillingLogMessage) {
		t.Fatalf("merged pair ordering broke: %q", lines)
	}
}

// The repair marker is for a DIFFERENT account. When the managed session ran as
// the same account the direct run uses, the merged pair already names it, and a
// second identical marker would be a standing extra write into a provider-owned
// file for no gain.
func TestPersistGrokManagedBillingSnapshot_SkipsRepairForTheSameAccount(t *testing.T) {
	resetGrokBillingAttribution(t)
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", persistent)
	t.Setenv(grokBillingAttributionKeeperIntervalEnv, "1h")

	finish := startGrokBillingAttributionKeeper("acct-1")
	defer finish()

	isolated := helperGrokHomeWithAccount(t, "acct-1")
	helperAppendGrokLogLine(t, isolated, helperGrokIdentityLine(t, "acct-1"))
	helperAppendGrokLogLine(t, isolated,
		grokUnmeteredLine("2026-08-19T12:20:00Z", grokBillingLogMessage))

	if outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent); err != nil ||
		outcome != grokManagedBillingPersisted {
		t.Fatalf("persist → %v, %v; want persisted", outcome, err)
	}
	if got := grokIdentityLineCount(t, persistent); got != 1 {
		t.Fatalf("identity lines = %d, want 1 — the merge repaired an account that was already newest", got)
	}
}

// The armed CHECK must be taken UNDER the write lock, not by the caller before
// it. A direct session arms BEFORE its ensureGrokBillingAttribution reaches the
// lock, so a check taken outside can observe "not armed", let the direct
// session's own append land first, and then append the managed identity last —
// the direct child then runs under the wrong newest identity and can lose its
// only billing record before the keeper ticks.
//
// Pinned by arming while the lock is held with the merge already in flight
// behind it: the merge cannot have read the arm state before the arm happened,
// so the marker can only be present if the check is inside the lock.
func TestPersistGrokManagedBillingSnapshot_ArmDuringTheWriteStillRepairs(t *testing.T) {
	resetGrokBillingAttribution(t)
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", persistent)
	t.Setenv(grokBillingAttributionKeeperIntervalEnv, "1h")

	ensureGrokBillingAttribution()

	isolated := helperGrokHomeWithAccount(t, "acct-managed")
	helperAppendGrokLogLine(t, isolated, helperGrokIdentityLine(t, "acct-managed"))
	helperAppendGrokLogLine(t, isolated,
		grokUnmeteredLine("2026-08-19T12:20:00Z", grokBillingLogMessage))

	grokBillingAttributionSerialize.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := persistGrokManagedBillingSnapshot(isolated, persistent)
		done <- err
	}()
	// Give the merge time to reach the lock it is now blocked on, so the arm
	// below genuinely races the write rather than preceding it.
	time.Sleep(50 * time.Millisecond)
	finish := startGrokBillingAttributionKeeper("acct-1")
	defer finish()
	grokBillingAttributionSerialize.Unlock()

	if err := <-done; err != nil {
		t.Fatalf("persist: %v", err)
	}

	raw, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read unified.jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(last, grokManagedBillingIdentityMessage) ||
		!strings.Contains(last, "acct-1") {
		t.Fatalf("merged log ends with %q — a session armed before the merge took "+
			"the lock must still get its repair marker in that same write", last)
	}
}

// The disagreement check and the append have to be ONE serialized decision.
// When account A tested for a conflict before B armed but appended after B's
// contested marker, the log ended up naming A while both accounts were live —
// so B's records were either refused or published as A's utilization. The
// invariant this pins: once a second account is armed, the newest marker is the
// contested sentinel, whatever order the two writers interleave in.
func TestGrokBillingAttribution_ContestedCheckIsTakenUnderTheAppendLock(t *testing.T) {
	resetGrokBillingAttribution(t)
	base := helperGrokHomeWithAccount(t, "acct-1")

	release := startGrokBillingAttributionKeeper("acct-1")
	defer release()

	// Hold the append lock so acct-1's assertion is parked at exactly the point
	// the ordering hinges on. Whether it has ALREADY decided what to name is the
	// whole question: a check taken before the lock is frozen against a world in
	// which acct-2 does not exist yet.
	grokBillingAttributionSerialize.Lock()

	asserted := make(chan struct{})
	go func() {
		defer close(asserted)
		ensureGrokBillingIdentityNamed(base, "acct-1")
	}()

	// Give the goroutine time to reach the lock. Both the correct and the broken
	// orderings block here, so this only sequences the test — it does not decide
	// the outcome.
	time.Sleep(100 * time.Millisecond)

	// acct-2 arms WHILE acct-1 is blocked: the second live account appears after
	// acct-1 would have tested for a conflict, but before it writes.
	conflicting := startGrokBillingAttributionKeeper("acct-2")
	defer conflicting()

	grokBillingAttributionSerialize.Unlock()
	<-asserted

	if last := helperGrokLastLogLine(t, base); !strings.Contains(last, grokContestedBillingIdentity) {
		t.Fatalf("newest marker = %s, want the contested sentinel — acct-1's marker "+
			"outlived the arming of acct-2, so every record either account writes "+
			"from here binds to acct-1", last)
	}
}

// Releasing one of two disagreeing runs RESOLVES the disagreement, so the
// contested sentinel it forced has to come down immediately. Leaving it for the
// next keeper tick is a known window in which every record the surviving child
// writes binds to a name no account matches and is permanently refused.
func TestGrokBillingAttributionKeeper_ReassertsTheSurvivorWhenAConflictingRunExits(t *testing.T) {
	resetGrokBillingAttribution(t)
	base := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", base)
	// A tick long enough that only the synchronous repair can satisfy the
	// assertion below.
	t.Setenv(grokBillingAttributionKeeperIntervalEnv, "1h")

	survivor := startGrokBillingAttributionKeeper("acct-1")
	defer survivor()
	conflicting := startGrokBillingAttributionKeeper("acct-2")

	ensureGrokBillingIdentityNamed(base, "acct-1")
	if last := helperGrokLastLogLine(t, base); !strings.Contains(last, grokContestedBillingIdentity) {
		t.Fatalf("newest marker = %s, want the contested sentinel while both runs are live", last)
	}

	conflicting()

	last := helperGrokLastLogLine(t, base)
	if strings.Contains(last, grokContestedBillingIdentity) || !strings.Contains(last, "acct-1") {
		t.Fatalf("newest marker = %s, want acct-1 — the survivor's records stay "+
			"unattributable until the next keeper tick", last)
	}
}

// The release repair must stay quiet in the ordinary case: a lone run exiting
// asserts nothing (no live run to attribute), and it must not pile a marker into
// a provider-owned log on every child reap.
func TestGrokBillingAttributionKeeper_LastReleaseWritesNothing(t *testing.T) {
	resetGrokBillingAttribution(t)
	base := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", base)
	t.Setenv(grokBillingAttributionKeeperIntervalEnv, "1h")

	finish := startGrokBillingAttributionKeeper("acct-1")
	ensureGrokBillingIdentityNamed(base, "acct-1")
	before := grokIdentityLineCount(t, base)

	finish()

	if got := grokIdentityLineCount(t, base); got != before {
		t.Fatalf("identity lines = %d after the last release, want %d", got, before)
	}
}
