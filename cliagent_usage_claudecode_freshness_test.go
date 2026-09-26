package main

// Bounds and races on the DURABLE Claude owed-refresh debt
// (cliagent_usage_claudecode_freshness.go). Everything runs against an
// httptest server pinned through AIEXPEDITE_CLAUDE_USAGE_PROBE_URL and a
// private cache, so no case here reaches api.anthropic.com.

import (
	"bytes"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// claudeFreshnessWaitIdle blocks until no probe or settlement is in flight.
// Both the trailing probe and the startup replay are asynchronous by design,
// so polling the gate is the only honest way to assert what they did.
func claudeFreshnessWaitIdle(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		claudeUsageProbe.mu.Lock()
		idle := !claudeUsageProbe.inFlight && claudeUsageProbe.settling == 0
		claudeUsageProbe.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("a probe or settlement is still in flight after 30s")
}

// claudeCacheSnapshot reads the pinned cache, failing the test when it is
// missing.
func claudeCacheSnapshot(t *testing.T, cache string) claudeRateLimitSnapshot {
	t.Helper()
	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("cache %s is missing or unreadable", cache)
	}
	return snap
}

// seedClaudeProbeReading puts one observed five-hour bucket in the cache under
// the current account, which is what a status-line render or an earlier probe
// would have left.
func seedClaudeProbeReading(t *testing.T, cache string, observedAt time.Time) {
	t.Helper()
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 33, ResetsAtMs: observedAt.Add(time.Hour).UnixMilli(),
			ObservedAtMs: observedAt.UnixMilli(), usageKnown: true,
		},
	}, observedAt, currentClaudeAccountFingerprint(), claudeRateLimitSourceStatusLine)
}

// unreachableProbeHandler is a server that always refuses, so a debt under test
// is never settled out from under the assertion.
func unreachableProbeHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusInternalServerError)
}

// A repeat owe for an UNCHANGED baseline must not rewrite the cache and must
// not reset RefreshOwedAttempts. Resetting on an unchanged instant would hand
// the same debt a fresh pair of attempts on every restart and silently undo
// the cap.
func TestClaudeOweRunRefresh_UnchangedBaselineLeavesAttemptsAndBytesAlone(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()

	runEnded := time.Now()
	claudeOweRunRefresh(runEnded)
	// One replay attempt has already been spent against this debt.
	mutateClaudeRateLimitSnapshot(cache, fp, func(snap *claudeRateLimitSnapshot) bool {
		snap.RefreshOwedAttempts = 1
		return true
	})
	before, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}

	claudeOweRunRefresh(runEnded)
	claudeOweRunRefresh(runEnded.Add(-time.Minute)) // an OLDER run cannot lower it either

	after, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("an unchanged baseline rewrote the cache:\nbefore %s\nafter  %s", before, after)
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAttempts != 1 {
		t.Fatalf("RefreshOwedAttempts=%d, want the cap budget preserved at 1", snap.RefreshOwedAttempts)
	}
}

// A genuinely NEWER run gets its own budget.
func TestClaudeOweRunRefresh_NewerBaselineResetsTheAttemptBudget(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()

	first := time.Now().Add(-time.Minute)
	claudeOweRunRefresh(first)
	mutateClaudeRateLimitSnapshot(cache, fp, func(snap *claudeRateLimitSnapshot) bool {
		snap.RefreshOwedAttempts = claudeUsageProbeAfterRunMaxAttempts
		return true
	})

	second := time.Now()
	claudeOweRunRefresh(second)

	snap := claudeCacheSnapshot(t, cache)
	if snap.RefreshOwedAtMs != second.UnixMilli() {
		t.Errorf("RefreshOwedAtMs=%d, want the newer run %d", snap.RefreshOwedAtMs, second.UnixMilli())
	}
	if snap.RefreshOwedAttempts != 0 {
		t.Errorf("RefreshOwedAttempts=%d, want 0 — a newer run gets its own budget", snap.RefreshOwedAttempts)
	}
}

// recordOwed is called synchronously from the session stdout path, so it must
// perform NO file I/O while holding the gate mutex. Pin the cache gate held and
// require it to return promptly: if the debt write ever migrated under g.mu,
// frame handling would block on a cross-process flock.
func TestClaudeRecordOwed_DoesNoFileIOUnderTheGateMutex(t *testing.T) {
	armClaudeUsageProbe(t, unreachableProbeHandler)

	lockClaudeRateLimitCache()
	defer unlockClaudeRateLimitCache()

	done := make(chan struct{})
	go func() {
		defer close(done)
		claudeUsageProbe.recordOwed(time.Now())
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("recordOwed blocked on the cache gate — it must stay a pure in-memory update")
	}
}

// The best-effort contract: a gate held past claudeRateLimitBestEffortGateWait
// DROPS the debt write rather than stalling, and leaves the cache intact.
func TestClaudeOweRunRefresh_GateHeldPastTheWaitDropsTheDebt(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))
	before, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}

	original := claudeRateLimitBestEffortGateWait
	claudeRateLimitBestEffortGateWait = 20 * time.Millisecond
	t.Cleanup(func() { claudeRateLimitBestEffortGateWait = original })

	lockClaudeRateLimitCache()
	claudeOweRunRefresh(time.Now())
	unlockClaudeRateLimitCache()

	after, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a dropped debt write must leave the cache byte-identical, not corrupt it")
	}
}

// The mutator and the merge take the same gate + flock ladder in the same
// order. Run them concurrently under -race: a deadlock or a torn write would
// live here.
func TestMutateClaudeRateLimitSnapshot_RacesTheMergeSafely(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	now := time.Now()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			mutateClaudeRateLimitSnapshot(cache, fp, func(snap *claudeRateLimitSnapshot) bool {
				snap.RefreshOwedAtMs = now.Add(time.Duration(i) * time.Millisecond).UnixMilli()
				return true
			})
		}(i)
		go func(i int) {
			defer wg.Done()
			at := now.Add(time.Duration(i) * time.Millisecond)
			mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
				claudeWindowSevenDay: {
					UsedPercentage: float64(i), ResetsAtMs: now.Add(72 * time.Hour).UnixMilli(),
					ObservedAtMs: at.UnixMilli(), usageKnown: true,
				},
			}, at, fp, claudeRateLimitSourceStream)
		}(i)
	}
	wg.Wait()

	// Whatever interleaving won, the file must still be a whole, parseable
	// snapshot for this account.
	if snap := claudeCacheSnapshot(t, cache); snap.AccountFingerprint != fp {
		t.Fatalf("concurrent writers left the cache scoped to %q, want %q", snap.AccountFingerprint, fp)
	}
}

// A snapshot belonging to ANOTHER account is reset before fn runs, so a debt
// can never be recorded onto — or read back off — an obsolete login's cache.
func TestMutateClaudeRateLimitSnapshot_ResetsAForeignFingerprintFirst(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	now := time.Now()
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 90, ResetsAtMs: now.Add(time.Hour).UnixMilli(),
			ObservedAtMs: now.UnixMilli(), usageKnown: true,
		},
	}, now, "someone-else", claudeRateLimitSourceProbe)

	claudeOweRunRefresh(now)

	snap := claudeCacheSnapshot(t, cache)
	if snap.AccountFingerprint != currentClaudeAccountFingerprint() {
		t.Fatalf("debt recorded under %q, want the current account", snap.AccountFingerprint)
	}
	if len(snap.Buckets) != 0 {
		t.Errorf("the previous account's buckets survived the debt write: %+v", snap.Buckets)
	}
	// And it must not be READ back off a foreign cache either.
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 90, ResetsAtMs: now.Add(time.Hour).UnixMilli(),
			ObservedAtMs: now.UnixMilli(), usageKnown: true,
		},
	}, now, "someone-else", claudeRateLimitSourceProbe)
	mutateClaudeRateLimitSnapshot(cache, "someone-else", func(snap *claudeRateLimitSnapshot) bool {
		snap.RefreshOwedAtMs = now.UnixMilli()
		return true
	})
	if owed, _ := claudeRunRefreshOwedFor(currentClaudeAccountFingerprint()); !owed.IsZero() {
		t.Errorf("another account's debt is visible to this one: %v", owed)
	}
}

// A debt or hold stamped implausibly far ahead is a backwards clock step, not
// skew. Both are retired without a request.
func TestPayOwedClaudeUsageRefresh_RetiresFutureStampedDebtAndHold(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	mutateClaudeRateLimitSnapshot(cache, fp, func(snap *claudeRateLimitSnapshot) bool {
		snap.RefreshOwedAtMs = now.Add(2 * claudeRefreshOwedLocalSkew).UnixMilli()
		snap.HeldUntilMs = now.Add(2 * claudeUsageProbeMaxRetryAfter).UnixMilli()
		return true
	})

	payOwedClaudeUsageRefreshAt(now)

	if atomic.LoadInt64(calls) != 0 {
		t.Errorf("request count=%d, want 0 — a future-stamped debt is retired, not paid", atomic.LoadInt64(calls))
	}
	snap := claudeCacheSnapshot(t, cache)
	if snap.RefreshOwedAtMs != 0 {
		t.Errorf("future-stamped debt survived: %+v", snap)
	}
	if snap.HeldUntilMs != 0 {
		t.Errorf("future-stamped hold survived: %+v", snap)
	}
}

// A hold still live at start suppresses the replay WITHOUT charging an attempt,
// so the debt is still payable once the hold expires.
func TestPayOwedClaudeUsageRefresh_LiveHoldSuppressesWithoutCharging(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	held := now.Add(20 * time.Minute)
	mutateClaudeRateLimitSnapshot(cache, fp, func(snap *claudeRateLimitSnapshot) bool {
		snap.RefreshOwedAtMs = now.Add(-time.Minute).UnixMilli()
		snap.HeldUntilMs = held.UnixMilli()
		return true
	})

	payOwedClaudeUsageRefreshAt(now)

	if atomic.LoadInt64(calls) != 0 {
		t.Errorf("request count=%d, want 0 — the service asked us to stop", atomic.LoadInt64(calls))
	}
	snap := claudeCacheSnapshot(t, cache)
	if snap.RefreshOwedAtMs == 0 || snap.RefreshOwedAttempts != 0 {
		t.Errorf("a suppressed replay must leave debt and counter as found: %+v", snap)
	}
	if snap.HeldUntilMs != held.UnixMilli() {
		t.Errorf("HeldUntilMs=%d, want the surviving hold %d", snap.HeldUntilMs, held.UnixMilli())
	}
	// The surviving hold is carried into this process's gate too.
	claudeUsageProbe.mu.Lock()
	gateHold := claudeUsageProbe.heldUntil
	claudeUsageProbe.mu.Unlock()
	if gateHold.UnixMilli() != held.UnixMilli() {
		t.Errorf("in-memory hold=%v, want the persisted %v", gateHold, held)
	}
}

// An offline agent spends no request and charges no attempt either.
func TestPayOwedClaudeUsageRefresh_OfflineSpendsNothing(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	mutateClaudeRateLimitSnapshot(cache, fp, func(snap *claudeRateLimitSnapshot) bool {
		snap.RefreshOwedAtMs = now.Add(-time.Minute).UnixMilli()
		return true
	})

	// Pin the flag directly, as the sibling Antigravity freshness test does,
	// rather than calling SetOffline: this case is about the replay's gate
	// decision, and SetOffline additionally releases the reconnect drain,
	// signals offlineChan and logs a transition — process-global side effects
	// a freshness assertion has no business emitting into a shared suite.
	offlineMutex.Lock()
	wasOffline := isOffline
	isOffline = true
	offlineMutex.Unlock()
	t.Cleanup(func() {
		offlineMutex.Lock()
		isOffline = wasOffline
		offlineMutex.Unlock()
	})

	payOwedClaudeUsageRefreshAt(now)

	if atomic.LoadInt64(calls) != 0 {
		t.Errorf("request count=%d, want 0 while offline", atomic.LoadInt64(calls))
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs == 0 || snap.RefreshOwedAttempts != 0 {
		t.Errorf("an offline start must leave the debt payable: %+v", snap)
	}
}

// A settlement write dropped under contention leaves the marker standing over a
// reading that is already on disk. The coverage pre-check clears it without a
// request — the regression guard for one wasted OAuth call per restart.
func TestPayOwedClaudeUsageRefresh_CoveredDebtIsClearedWithoutARequest(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	now := time.Now()
	runEnded := now.Add(-time.Minute)
	// The covering reading landed; only the settlement write was lost.
	seedClaudeProbeReading(t, cache, runEnded.Add(time.Second))
	mutateClaudeRateLimitSnapshot(cache, fp, func(snap *claudeRateLimitSnapshot) bool {
		snap.RefreshOwedAtMs = runEnded.UnixMilli()
		return true
	})

	payOwedClaudeUsageRefreshAt(now)

	if atomic.LoadInt64(calls) != 0 {
		t.Errorf("request count=%d, want 0 — the covering reading is already on disk", atomic.LoadInt64(calls))
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 {
		t.Errorf("a covered debt must be cleared: %+v", snap)
	}
}

// With the probe opted out nothing can ever pay a debt, so none is recorded and
// the cache stays byte-identical.
func TestClaudeOweRunRefresh_OptedOutRecordsNothing(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))
	SetClaudeUsageProbeDisabled(true)

	before, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	claudeOweRunRefresh(time.Now())
	after, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("an opted-out process must record no debt")
	}
}

// A debt written BEFORE the user opted out must be cleared rather than left as
// a marker nothing will ever retire.
func TestPayOwedClaudeUsageRefresh_OptedOutStartClearsAnInheritedDebt(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	mutateClaudeRateLimitSnapshot(cache, fp, func(snap *claudeRateLimitSnapshot) bool {
		snap.RefreshOwedAtMs = now.Add(-time.Minute).UnixMilli()
		return true
	})

	SetClaudeUsageProbeDisabled(true)
	payOwedClaudeUsageRefreshAt(now)

	if atomic.LoadInt64(calls) != 0 {
		t.Errorf("request count=%d, want 0 while opted out", atomic.LoadInt64(calls))
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 {
		t.Errorf("an opted-out start must retire the inherited debt: %+v", snap)
	}
}

// The 429 hold is mirrored to disk beside the gate's in-memory copy, so a
// restart inside the window does not fire straight at an endpoint that just
// told us to stop.
func TestClaudeUsageProbe_RetryAfterHoldIsPersisted(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "1800")
		w.WriteHeader(http.StatusTooManyRequests)
	})

	now := time.Now()
	claudeUsageProbeAfterRun(now)
	claudeFreshnessWaitIdle(t)

	snap := claudeCacheSnapshot(t, cache)
	if snap.HeldUntilMs == 0 {
		t.Fatalf("a 429 Retry-After must persist a hold: %+v", snap)
	}
	if got := time.UnixMilli(snap.HeldUntilMs); got.Before(now.Add(25*time.Minute)) || got.After(now.Add(35*time.Minute)) {
		t.Errorf("HeldUntilMs=%v, want ~30 minutes out", got)
	}
}

// An attempt the single-flight gate never admitted issued no request, so the
// charge is REFUNDED. Without it two unlucky starts would retire a debt that
// was never once put to the endpoint — exactly the stale card this path clears.
func TestPayOwedClaudeUsageRefresh_UnadmittedAttemptIsRefunded(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	owed := now.Add(-time.Minute)
	claudeOweRunRefresh(owed)

	// Pin the single-flight slot, as a concurrent gather's probe would.
	claudeUsageProbe.mu.Lock()
	claudeUsageProbe.inFlight = true
	claudeUsageProbe.doneCh = make(chan struct{})
	claudeUsageProbe.mu.Unlock()

	payOwedClaudeUsageRefreshAt(now)

	claudeUsageProbe.mu.Lock()
	claudeUsageProbe.inFlight = false
	close(claudeUsageProbe.doneCh)
	claudeUsageProbe.doneCh = nil
	claudeUsageProbe.mu.Unlock()

	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("request count=%d, want 0 — the gate refused the attempt", got)
	}
	snap := claudeCacheSnapshot(t, cache)
	if snap.RefreshOwedAtMs != owed.UnixMilli() {
		t.Fatalf("the debt must stand: %+v", snap)
	}
	if snap.RefreshOwedAttempts != 0 {
		t.Fatalf("RefreshOwedAttempts=%d, want 0 — an unadmitted attempt spends nothing", snap.RefreshOwedAttempts)
	}
	if snap.AccountFingerprint != fp {
		t.Errorf("cache scoped to %q, want %q", snap.AccountFingerprint, fp)
	}
}

// A debt-only cache the mutator had to CREATE still carries a real updatedAt,
// so a diagnostics upload never shows an empty timestamp.
func TestMutateClaudeRateLimitSnapshot_StampsACreatedSnapshot(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	if _, err := os.Stat(cache); err == nil {
		t.Fatal("this case needs a cache that does not exist yet")
	}

	claudeOweRunRefresh(time.Now())

	snap := claudeCacheSnapshot(t, cache)
	if snap.UpdatedAt == "" {
		t.Fatalf("a created snapshot must be stamped: %+v", snap)
	}
	if _, err := time.Parse(time.RFC3339, snap.UpdatedAt); err != nil {
		t.Fatalf("updatedAt %q is not RFC3339: %v", snap.UpdatedAt, err)
	}

	// An EXISTING cache keeps its merge stamp: a debt marker is not an
	// observation and must not pass for one.
	stamped := claudeCacheSnapshot(t, cache).UpdatedAt
	claudeOweRunRefresh(time.Now().Add(time.Minute))
	if got := claudeCacheSnapshot(t, cache).UpdatedAt; got != stamped {
		t.Errorf("updatedAt moved on a debt-only write: %q -> %q", stamped, got)
	}
}

// Every retire decision in payOwedClaudeUsageRefreshAt is made from an UNLOCKED
// read, and that read can be overtaken by a run of this process finishing. The
// clear must therefore be scoped to the debt it judged: a blanket clear would
// discard a newer, perfectly payable debt and leave that run's card stale.
func TestRetireClaudeRefreshDebtAt_OnlyClearsTheJudgedDebt(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	judged := time.Now().Add(-time.Hour)
	newer := time.Now()

	// A newer run landed between the verdict and the lock.
	seedClaudeRefreshDebt(t, cache, fp, newer, 0, time.Time{})
	if mutateClaudeRateLimitSnapshot(cache, fp, retireClaudeRefreshDebtAt(judged)) {
		t.Fatal("a verdict about an older debt must not rewrite the cache")
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != newer.UnixMilli() {
		t.Fatalf("a newer debt was discarded by a stale verdict: %+v", snap)
	}

	// The debt it actually judged is cleared, counter and all.
	seedClaudeRefreshDebt(t, cache, fp, judged, 1, time.Time{})
	if !mutateClaudeRateLimitSnapshot(cache, fp, retireClaudeRefreshDebtAt(judged)) {
		t.Fatal("the judged debt must be retired")
	}
	snap := claudeCacheSnapshot(t, cache)
	if snap.RefreshOwedAtMs != 0 || snap.RefreshOwedAttempts != 0 {
		t.Fatalf("retire left the debt behind: %+v", snap)
	}
}

// The skewed-hold clear is bounded the same way: only a hold still beyond the
// ceiling is dropped, so a real 429 recorded between the unlocked read and the
// lock keeps its backpressure.
func TestDropClaudeSkewedHold_KeepsAHoldInsideTheCeiling(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	now := time.Now()
	ceilingMs := now.Add(claudeUsageProbeMaxRetryAfter).UnixMilli()

	// A legitimate hold replaced the skewed one before the lock was granted.
	legit := now.Add(30 * time.Minute)
	seedClaudeRefreshDebt(t, cache, fp, now, 0, legit)
	if mutateClaudeRateLimitSnapshot(cache, fp, dropClaudeSkewedHold(ceilingMs)) {
		t.Fatal("a hold inside the ceiling must not be rewritten")
	}
	if snap := claudeCacheSnapshot(t, cache); snap.HeldUntilMs != legit.UnixMilli() {
		t.Fatalf("live backpressure was discarded: %+v", snap)
	}

	// A hold exactly AT the ceiling is the longest one the bound allows.
	seedClaudeRefreshDebt(t, cache, fp, now, 0, time.UnixMilli(ceilingMs))
	if mutateClaudeRateLimitSnapshot(cache, fp, dropClaudeSkewedHold(ceilingMs)) {
		t.Fatal("a hold exactly at the ceiling is legal and must be kept")
	}

	// One beyond it is a clock step.
	seedClaudeRefreshDebt(t, cache, fp, now, 0, time.UnixMilli(ceilingMs+1))
	if !mutateClaudeRateLimitSnapshot(cache, fp, dropClaudeSkewedHold(ceilingMs)) {
		t.Fatal("a hold beyond the ceiling must be dropped")
	}
	if snap := claudeCacheSnapshot(t, cache); snap.HeldUntilMs != 0 {
		t.Fatalf("skewed hold survived: %+v", snap)
	}
}

// resetClaudeUsageProbeGate's drain must cover the DURABLE debt write, not just
// the probe. The settlement counter is therefore raised on the caller's
// goroutine, before the spawn: a reset that sampled it in the gap would declare
// the drain complete while a cache write was still on its way to disk — in a
// test, the next case's cache written by the previous case's run.
func TestTriggerClaudeUsageProbeAfterRun_CountsTheDebtWriteInTheDrain(t *testing.T) {
	armClaudeUsageProbe(t, unreachableProbeHandler)

	triggerClaudeUsageProbeAfterRun()
	// Sampled with no sleep: beginSettling is synchronous with the trigger, so
	// this cannot be a timing-dependent read.
	claudeUsageProbe.mu.Lock()
	settling := claudeUsageProbe.settling
	claudeUsageProbe.mu.Unlock()
	if settling == 0 {
		t.Fatal("the trigger returned with settling=0; a reset could drain past the pending debt write")
	}
	claudeFreshnessWaitIdle(t)
}
