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

	finish := startGrokBillingAttributionKeeper()
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
	finish := startGrokBillingAttributionKeeper()
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

	first := startGrokBillingAttributionKeeper()
	second := startGrokBillingAttributionKeeper()
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
