package main

// Bounds and races on the DURABLE Claude owed-refresh debt
// (cliagent_usage_claudecode_freshness.go). Everything runs against an
// httptest server pinned through AIEXPEDITE_CLAUDE_USAGE_PROBE_URL and a
// private cache, so no case here reaches api.anthropic.com.

import (
	"bytes"
	"context"
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

// waitForClaudeDebt polls until a debt appears on the cache. The owe is
// persisted from the goroutine triggerClaudeUsageProbeAfterRun spawns, so
// polling is the only honest way to observe it.
func waitForClaudeDebt(t *testing.T, cache string, within time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(within); time.Now().Before(deadline); {
		if snap, ok := loadClaudeRateLimitSnapshot(cache); ok && snap.RefreshOwedAtMs != 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no owed-refresh debt reached the cache")
}

// waitForClaudeProbeReading polls until the probe has persisted a reading. The
// trailing probe is asynchronous by design, so polling is the only honest way
// to observe it.
func waitForClaudeProbeReading(t *testing.T, cache string, within time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(within); time.Now().Before(deadline); {
		if snap, ok := loadClaudeRateLimitSnapshot(cache); ok && snap.LastProbeObservedAtMs != 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the smoke's trailing probe never persisted a reading")
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

	// A scoped -> other-scoped flip — a real `/login`. The helper deliberately
	// REFUSES a scoped -> "" downgrade (it cannot tell one from a failed
	// credential read); that refusal has its own case,
	// TestMutateClaudeRateLimitSnapshot_RefusesAnUnresolvedIdentityOverAScopedCache.
	const current = "current-account"
	mutateClaudeRateLimitSnapshot(cache, current,
		func(snap *claudeRateLimitSnapshot) bool {
			snap.RefreshOwedAtMs = now.UnixMilli()
			return true
		})

	snap := claudeCacheSnapshot(t, cache)
	if snap.AccountFingerprint != current {
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
	if owed, _, _ := claudePersistedProbeStateFor(current); !owed.IsZero() {
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

// The skewed-hold clear is bounded the same way: only the exact value the
// replay judged is dropped, so a real 429 recorded between the unlocked read and
// the lock keeps its backpressure — including a maximum-length one another
// process stamped from a slightly later clock, which lands just past the
// ceiling the replay computed from its own `now`.
func TestDropClaudeSkewedHold_KeepsAHoldRenewedSinceTheRead(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	now := time.Now()
	ceilingMs := now.Add(claudeUsageProbeMaxRetryAfter).UnixMilli()
	skewedMs := now.Add(48 * time.Hour).UnixMilli()

	// A legitimate hold replaced the skewed one before the lock was granted.
	legit := now.Add(30 * time.Minute)
	seedClaudeRefreshDebt(t, cache, fp, now, 0, legit)
	if mutateClaudeRateLimitSnapshot(cache, fp, dropClaudeSkewedHold(skewedMs)) {
		t.Fatal("a hold renewed since the read must not be rewritten")
	}
	if snap := claudeCacheSnapshot(t, cache); snap.HeldUntilMs != legit.UnixMilli() {
		t.Fatalf("live backpressure was discarded: %+v", snap)
	}

	// A maximum-length Retry-After taken a moment later by another process sits
	// just past the replay's stale ceiling. It is still live and must survive.
	renewed := time.UnixMilli(ceilingMs + 250)
	seedClaudeRefreshDebt(t, cache, fp, now, 0, renewed)
	if mutateClaudeRateLimitSnapshot(cache, fp, dropClaudeSkewedHold(skewedMs)) {
		t.Fatal("a renewed maximum-length hold must not be dropped as the skewed one")
	}
	if snap := claudeCacheSnapshot(t, cache); snap.HeldUntilMs != renewed.UnixMilli() {
		t.Fatalf("renewed hold was discarded: %+v", snap)
	}

	// The value actually judged is dropped.
	seedClaudeRefreshDebt(t, cache, fp, now, 0, time.UnixMilli(skewedMs))
	if !mutateClaudeRateLimitSnapshot(cache, fp, dropClaudeSkewedHold(skewedMs)) {
		t.Fatal("the judged skewed hold must be dropped")
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

// Only a PROBE write may durably settle a debt. `observed` is the constraining
// window of the WRITE, not of the card, so it is a claim about every displayed
// row only for the writer that samples them all. A status-line render answers
// five_hour/seven_day and a stream capture often one window: letting either
// settle cleared the durable debt while the weekly / Fable row still showed a
// PRE-run reading, so a self-update moments later found nothing to replay —
// a diluted form of the defect this whole path exists to fix.
func TestMergeClaudeRateLimitCache_PartialWriteCannotSettleTheDurableDebt(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	now := time.Now()
	preRun := now.Add(-time.Hour)

	for _, source := range []string{claudeRateLimitSourceStatusLine, claudeRateLimitSourceStream} {
		t.Run(source, func(t *testing.T) {
			// Two displayed rows, both observed BEFORE the run.
			mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
				claudeWindowFiveHour: {UsedPercentage: 10, ResetsAtMs: now.Add(time.Hour).UnixMilli(),
					ObservedAtMs: preRun.UnixMilli(), usageKnown: true},
				claudeWindowSevenDayOverageIncluded: {UsedPercentage: 20, ResetsAtMs: now.Add(72 * time.Hour).UnixMilli(),
					ObservedAtMs: preRun.UnixMilli(), usageKnown: true},
			}, preRun, fp, claudeRateLimitSourceStatusLine)

			runEnded := time.Now()
			seedClaudeRefreshDebt(t, cache, fp, runEnded, 0, time.Time{})

			// A post-run write that only knows five_hour.
			after := runEnded.Add(time.Second)
			mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
				claudeWindowFiveHour: {UsedPercentage: 11, ResetsAtMs: now.Add(time.Hour).UnixMilli(),
					ObservedAtMs: after.UnixMilli(), usageKnown: true},
			}, after, fp, source)

			// The card is still stale, so the debt must still be owed.
			displayed := claudeSnapshotFreshness(loadMergedClaudeRateLimitView(fp), time.Now())
			if claudeUsageObservationCovers(displayed, runEnded) {
				t.Fatalf("fixture is wrong: the displayed card (%v) already covers the run %v", displayed, runEnded)
			}
			if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != runEnded.UnixMilli() {
				t.Fatalf("a %s write settled a debt the card still owes: %+v", source, snap)
			}
		})
	}

	// The same write from the PROBE does settle it: the probe stamps every
	// window the endpoint supplies, so its observation speaks for the card.
	runEnded := time.Now()
	seedClaudeRefreshDebt(t, cache, fp, runEnded, 0, time.Time{})
	after := runEnded.Add(time.Second)
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {UsedPercentage: 12, ResetsAtMs: now.Add(time.Hour).UnixMilli(),
			ObservedAtMs: after.UnixMilli(), usageKnown: true},
		claudeWindowSevenDayOverageIncluded: {UsedPercentage: 21, ResetsAtMs: now.Add(72 * time.Hour).UnixMilli(),
			ObservedAtMs: after.UnixMilli(), usageKnown: true},
	}, after, fp, claudeRateLimitSourceProbe)
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("a covering probe write must settle the debt: %+v", snap)
	}
}

// A debt a partial write DID cover costs no request: the startup replay's
// row-aware pre-check clears it. That is what makes the probe-only settlement
// rule free rather than a source of extra OAuth calls.
func TestPayOwedClaudeUsageRefresh_ClearsADebtAPartialWriteCovered(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	now := time.Now()
	runEnded := now.Add(-time.Minute)

	// One displayed row, refreshed by a status-line render after the run — so
	// the CARD is genuinely current even though no probe wrote.
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {UsedPercentage: 13, ResetsAtMs: now.Add(time.Hour).UnixMilli(),
			ObservedAtMs: runEnded.Add(time.Second).UnixMilli(), usageKnown: true},
	}, runEnded.Add(time.Second), fp, claudeRateLimitSourceStatusLine)
	seedClaudeRefreshDebt(t, cache, fp, runEnded, 0, time.Time{})

	payOwedClaudeUsageRefreshAt(now)

	if got := atomic.LoadInt64(calls); got != 0 {
		t.Errorf("request count=%d, want 0 — the card is already current", got)
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 {
		t.Errorf("the replay must retire a debt the card already covers: %+v", snap)
	}
}

// The charge and the refund share one guard, so they cannot drift apart — a
// refund guarded differently from its charge would leak or invent attempts,
// and that counter is the only thing bounding a crash-looping agent.
func TestAdjustClaudeRefreshAttemptsAt_IsSymmetricAndScopedToTheJudgedDebt(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	judged := time.Now().Add(-time.Minute)

	seedClaudeRefreshDebt(t, cache, fp, judged, 0, time.Time{})
	if !mutateClaudeRateLimitSnapshot(cache, fp, adjustClaudeRefreshAttemptsAt(judged, +1)) {
		t.Fatal("the charge must land on the judged debt")
	}
	if got := claudeCacheSnapshot(t, cache).RefreshOwedAttempts; got != 1 {
		t.Fatalf("attempts=%d after one charge, want 1", got)
	}
	if !mutateClaudeRateLimitSnapshot(cache, fp, adjustClaudeRefreshAttemptsAt(judged, -1)) {
		t.Fatal("the refund must land on the judged debt")
	}
	if got := claudeCacheSnapshot(t, cache).RefreshOwedAttempts; got != 0 {
		t.Fatalf("attempts=%d after charge+refund, want 0", got)
	}

	// It can never drive the counter negative, so a stray refund cannot hand a
	// debt budget it was never granted.
	if mutateClaudeRateLimitSnapshot(cache, fp, adjustClaudeRefreshAttemptsAt(judged, -1)) {
		t.Fatal("a refund below zero must be refused, not wrap the budget")
	}
	if got := claudeCacheSnapshot(t, cache).RefreshOwedAttempts; got != 0 {
		t.Fatalf("attempts=%d after an over-refund, want 0", got)
	}

	// And neither half touches a debt recorded since the verdict.
	newer := time.Now()
	seedClaudeRefreshDebt(t, cache, fp, newer, 0, time.Time{})
	for _, delta := range []int{+1, -1} {
		if mutateClaudeRateLimitSnapshot(cache, fp, adjustClaudeRefreshAttemptsAt(judged, delta)) {
			t.Fatalf("delta %+d moved the budget of a debt it did not judge", delta)
		}
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != newer.UnixMilli() || snap.RefreshOwedAttempts != 0 {
		t.Fatalf("the newer debt was disturbed: %+v", snap)
	}
}

// payOwedClaudeUsageRefresh is the function StartAgent actually calls, and the
// only one of this file's entry points that every other test bypasses in
// favour of the clock-injected body. Four properties live in that six-line
// wrapper, all of them reliability-critical at agent boot:
//
//   - the settlement counter is raised on the CALLER's goroutine, so a reset
//     cannot drain past a replay that has not started yet (the same defect
//     already fixed once in triggerClaudeUsageProbeAfterRun);
//   - endSettling runs on every exit, or every later reset burns its whole
//     drain budget against a counter that never returns to zero;
//   - a panic is recovered — this runs on a goroutine, where an unrecovered
//     panic takes the whole tray app down at startup;
//   - it SPAWNS, so StartAgent is never blocked behind the cache.
func TestPayOwedClaudeUsageRefresh_SpawnsBoundedAndContainsAPanic(t *testing.T) {
	t.Run("a panic cannot escape or leak the gate", func(t *testing.T) {
		cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
		fp := currentClaudeAccountFingerprint()
		seedClaudeRefreshDebt(t, cache, fp, time.Now().Add(-time.Minute), 0, time.Time{})

		// Opted out, so the replay's first act is the debt-clearing write — and
		// that write blows up. Installed AFTER the seed, which uses the same
		// seam.
		SetClaudeUsageProbeDisabled(true)
		original := claudeRateLimitCacheWriteFile
		claudeRateLimitCacheWriteFile = func(string, []byte, os.FileMode) error {
			panic("cache write blew up")
		}
		t.Cleanup(func() { claudeRateLimitCacheWriteFile = original })

		payOwedClaudeUsageRefresh()

		// Raised synchronously: no sleep, so this cannot be a timing-dependent
		// read.
		claudeUsageProbe.mu.Lock()
		settling := claudeUsageProbe.settling
		claudeUsageProbe.mu.Unlock()
		if settling == 0 {
			t.Fatal("the replay returned with settling=0; a reset could drain past it before it starts")
		}

		// The process is still here, and the counter came back down — so the
		// panic was recovered and endSettling ran on the unwind.
		claudeFreshnessWaitIdle(t)

		// The unwind must also have released the cache gate, or every later
		// writer on this device is wedged for good.
		if !lockClaudeRateLimitCacheUntil(time.Now().Add(2 * time.Second)) {
			t.Fatal("the panicking replay leaked the cache gate")
		}
		unlockClaudeRateLimitCache()
	})

	t.Run("it spawns rather than blocking the boot goroutine", func(t *testing.T) {
		cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
		fp := currentClaudeAccountFingerprint()
		seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))
		seedClaudeRefreshDebt(t, cache, fp, time.Now().Add(-time.Minute), 0, time.Time{})

		// Pin the gate so the replay's first write would block for the whole
		// best-effort wait if it ran inline.
		lockClaudeRateLimitCache()
		started := time.Now()
		payOwedClaudeUsageRefresh()
		elapsed := time.Since(started)
		unlockClaudeRateLimitCache()
		claudeFreshnessWaitIdle(t)

		if elapsed > claudeRateLimitBestEffortGateWait/2 {
			t.Fatalf("payOwedClaudeUsageRefresh blocked its caller for %v; StartAgent must never wait on the cache", elapsed)
		}
	})
}

// The attempt charge must reach DISK before the request does. With another
// writer holding the cache gate past the best-effort wait the charge is
// dropped, and probing anyway would leave RefreshOwedAttempts untouched — one
// startup request per restart, forever, on a persistently contended cache. The
// debt is left uncharged and payable instead.
func TestPayOwedClaudeUsageRefresh_UnpersistedChargeIssuesNoRequest(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	owed := now.Add(-time.Minute)
	claudeOweRunRefresh(owed)

	original := claudeRateLimitBestEffortGateWait
	claudeRateLimitBestEffortGateWait = 20 * time.Millisecond
	t.Cleanup(func() { claudeRateLimitBestEffortGateWait = original })

	lockClaudeRateLimitCache()
	payOwedClaudeUsageRefreshAt(now)
	unlockClaudeRateLimitCache()

	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("request count=%d, want 0 — an uncharged attempt must not go out", got)
	}
	snap := claudeCacheSnapshot(t, cache)
	if snap.RefreshOwedAtMs != owed.UnixMilli() {
		t.Fatalf("the debt must stand for the next start: %+v", snap)
	}
	if snap.RefreshOwedAttempts != 0 {
		t.Fatalf("RefreshOwedAttempts=%d, want 0 — the charge never reached disk", snap.RefreshOwedAttempts)
	}
}

// The startup replay races the first gather's seed: it can persist a covering
// reading and clear the debt on disk AND on the gate while the seed's unlocked
// cache read is in flight. Adopting the value read before that would resurrect
// a PAID debt, and the gather's own probe would then be refused by the throttle
// the replay just spent — publishing the pre-replay view. The seed must detect
// the settlement and hand the covering reading back instead.
func TestClaudeUsageProbeGate_CacheSeedDoesNotResurrectASettledDebt(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	runEnded := time.Now().Add(-time.Minute)
	latest := runEnded.Add(-time.Minute) // the PRE-update reading this gather loaded
	seedClaudeProbeReading(t, cache, latest)
	claudeOweRunRefresh(runEnded)

	resetClaudeUsageProbeGate()
	SetClaudeUsageProbeDisabled(false)

	settled := runEnded.Add(time.Second)
	originalHook := claudeUsageProbeAfterSeedRead
	t.Cleanup(func() { claudeUsageProbeAfterSeedRead = originalHook })
	// Stand in for the startup replay landing inside the race window: a probe of
	// this process persisted a covering reading, settled the debt on the gate,
	// and cleared it on disk.
	claudeUsageProbeAfterSeedRead = func() {
		claudeUsageProbe.mu.Lock()
		claudeUsageProbe.refreshes++
		claudeUsageProbe.lastRefreshAt = settled
		claudeUsageProbe.lastRefreshFingerprint = fp
		claudeUsageProbe.owedBaseline = time.Time{}
		claudeUsageProbe.mu.Unlock()
		mutateClaudeRateLimitSnapshot(cache, fp, retireClaudeRefreshDebtAt(runEnded))
	}

	got := claudeUsageProbe.seedOwedFromCache(context.Background(), fp, claudeUsageProbe.refreshGeneration(), time.Now(), latest)

	if owed := claudeUsageProbe.owedObservation(); !owed.IsZero() {
		t.Fatalf("a settled debt was resurrected on the gate as %v", owed)
	}
	if got.UnixMilli() != settled.UnixMilli() {
		t.Fatalf("seedOwedFromCache()=%v, want the covering reading %v so the gather re-reads the cache", got, settled)
	}
	if n := atomic.LoadInt64(calls); n != 0 {
		t.Errorf("the seed issued %d requests, want 0 — it is a pure cache read", n)
	}
}

// A debt the gather's OWN reading already covers is not adopted: probing for an
// observation the caller is holding spends a request for nothing.
func TestClaudeUsageProbeGate_CacheSeedSkipsADebtTheCallerAlreadyCovers(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	runEnded := time.Now().Add(-time.Minute)
	seedClaudeProbeReading(t, cache, runEnded.Add(time.Second))
	claudeOweRunRefresh(runEnded)

	resetClaudeUsageProbeGate()
	SetClaudeUsageProbeDisabled(false)

	if got := claudeUsageProbe.seedOwedFromCache(context.Background(), fp, claudeUsageProbe.refreshGeneration(), time.Now(), runEnded.Add(time.Second)); !got.IsZero() {
		t.Fatalf("seedOwedFromCache()=%v, want zero — the caller's own reading needs no re-read", got)
	}
	if owed := claudeUsageProbe.owedObservation(); !owed.IsZero() {
		t.Fatalf("a debt the caller's reading already covers was adopted as %v", owed)
	}
	_ = cache
}

// A debt the startup replay would retire without a request — at the attempt cap,
// past the age limit, or stamped beyond the skew ceiling — is not adopted by a
// gather that reaches the seed first. Adopting it would let the gather spend an
// uncharged request, bypassing the cap that bounds a crash-looping agent. The
// seed is a read: the debt stays on disk for the replay to retire.
func TestClaudeUsageProbeGate_CacheSeedSkipsADebtTheReplayWouldRetire(t *testing.T) {
	cases := []struct {
		name     string
		owedAgo  time.Duration
		attempts int
	}{
		{"at the attempt cap", time.Minute, claudeUsageProbeAfterRunMaxAttempts},
		{"past the age limit", claudeRefreshOwedMaxAge + time.Minute, 0},
		{"stamped beyond the skew ceiling", -2 * claudeRefreshOwedLocalSkew, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
			fp := currentClaudeAccountFingerprint()
			now := time.Now()
			seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
			owed := now.Add(-tc.owedAgo)
			mutateClaudeRateLimitSnapshot(cache, fp, func(snap *claudeRateLimitSnapshot) bool {
				snap.RefreshOwedAtMs = owed.UnixMilli()
				snap.RefreshOwedAttempts = tc.attempts
				return true
			})

			resetClaudeUsageProbeGate()
			SetClaudeUsageProbeDisabled(false)

			claudeUsageProbe.seedOwedFromCache(context.Background(), fp, claudeUsageProbe.refreshGeneration(), now, now.Add(-time.Hour))

			if got := claudeUsageProbe.owedObservation(); !got.IsZero() {
				t.Fatalf("a retirable debt was adopted as %v", got)
			}
			if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != owed.UnixMilli() || snap.RefreshOwedAttempts != tc.attempts {
				t.Errorf("debt=(%d,%d), want the untouched (%d,%d) — only the replay retires it",
					snap.RefreshOwedAtMs, snap.RefreshOwedAttempts, owed.UnixMilli(), tc.attempts)
			}
		})
	}
}

// The replay settles the debt in the SAME locked write that carries the reading,
// so a replay landing during the seed's unlocked cache read has already cleared
// the debt from disk by the time the seed loads it. Keying the "must the gather
// re-read?" answer off a surviving debt therefore reports "nothing happened" for
// precisely the case the generation counter exists to catch, and the gather
// publishes its pre-replay view for the whole TTL — the stale card this file
// exists to clear.
func TestClaudeUsageProbeGate_CacheSeedReportsARefreshThatAlreadyClearedTheDebt(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	runEnded := time.Now().Add(-time.Minute)
	latest := runEnded.Add(-time.Minute) // the PRE-replay reading this gather loaded
	seedClaudeProbeReading(t, cache, latest)
	claudeOweRunRefresh(runEnded)

	resetClaudeUsageProbeGate()
	SetClaudeUsageProbeDisabled(false)

	settled := runEnded.Add(time.Second)
	originalHook := claudeUsageProbeAfterSeedRead
	t.Cleanup(func() { claudeUsageProbeAfterSeedRead = originalHook })
	// The replay lands mid-read and CLEARS the debt first, which is the ordering
	// the single settling write produces — unlike the sibling test above, the seed
	// finds nothing owed on disk.
	claudeUsageProbeAfterSeedRead = func() {
		mutateClaudeRateLimitSnapshot(cache, fp, retireClaudeRefreshDebtAt(runEnded))
		claudeUsageProbe.mu.Lock()
		claudeUsageProbe.refreshes++
		claudeUsageProbe.lastRefreshAt = settled
		claudeUsageProbe.lastRefreshFingerprint = fp
		claudeUsageProbe.owedBaseline = time.Time{}
		claudeUsageProbe.mu.Unlock()
	}

	// Cleared before the seed's own read, so it loads no debt at all.
	mutateClaudeRateLimitSnapshot(cache, fp, retireClaudeRefreshDebtAt(runEnded))

	got := claudeUsageProbe.seedOwedFromCache(context.Background(), fp, claudeUsageProbe.refreshGeneration(), time.Now(), latest)

	if got.UnixMilli() != settled.UnixMilli() {
		t.Fatalf("seedOwedFromCache()=%v, want the landed reading %v so the gather re-reads the cache", got, settled)
	}
	if owed := claudeUsageProbe.owedObservation(); !owed.IsZero() {
		t.Errorf("a settled debt was resurrected on the gate as %v", owed)
	}
	if n := atomic.LoadInt64(calls); n != 0 {
		t.Errorf("the seed issued %d requests, want 0 — it is a pure cache read", n)
	}
}

// The re-read answer stays a RELATIVE reading of the generation counter, never an
// absolute claim about lastRefreshAt: resetClaudeUsageProbeGate deliberately
// leaves that pair standing, so a refresh recorded before this gather started —
// or one belonging to another account, or no newer than the caller's own view —
// is no reason to re-read.
func TestClaudeUsageProbeGate_CacheSeedReportsNoReReadWithoutAFreshRefresh(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	latest := time.Now().Add(-time.Minute)
	seedClaudeProbeReading(t, cache, latest)
	originalHook := claudeUsageProbeAfterSeedRead
	t.Cleanup(func() { claudeUsageProbeAfterSeedRead = originalHook })

	for _, tc := range []struct {
		name        string
		landsDuring bool
		fingerprint string
		at          time.Time
	}{
		{"a refresh recorded before this gather started", false, fp, latest.Add(time.Minute)},
		{"another account's refresh", true, "someone-else", latest.Add(time.Minute)},
		{"a reading the caller already holds", true, fp, latest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetClaudeUsageProbeGate()
			SetClaudeUsageProbeDisabled(false)
			record := func() {
				claudeUsageProbe.mu.Lock()
				claudeUsageProbe.refreshes++
				claudeUsageProbe.lastRefreshAt = tc.at
				claudeUsageProbe.lastRefreshFingerprint = tc.fingerprint
				claudeUsageProbe.mu.Unlock()
			}
			claudeUsageProbeAfterSeedRead = func() {}
			if tc.landsDuring {
				claudeUsageProbeAfterSeedRead = record
			} else {
				record()
			}
			if got := claudeUsageProbe.seedOwedFromCache(context.Background(), fp, claudeUsageProbe.refreshGeneration(), time.Now(), latest); !got.IsZero() {
				t.Fatalf("seedOwedFromCache()=%v, want zero — no fresh reading supersedes the caller's", got)
			}
		})
	}
	_ = cache
}

// The startup replay restores a persisted hold from a SPAWNED goroutine, so the
// first gather of a fresh agent can reach begin() while that goroutine is still
// reading the credential store. The gather-side seed adopts the hold from the
// same unlocked read it takes the debt from, so no request can go out inside a
// window the endpoint already imposed.
func TestClaudeUsageProbeGate_CacheSeedRestoresAPersistedHold(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	held := now.Add(20 * time.Minute)
	mutateClaudeRateLimitSnapshot(cache, fp, func(snap *claudeRateLimitSnapshot) bool {
		snap.HeldUntilMs = held.UnixMilli()
		return true
	})

	resetClaudeUsageProbeGate()
	SetClaudeUsageProbeDisabled(false)

	claudeUsageProbe.seedOwedFromCache(context.Background(), fp, claudeUsageProbe.refreshGeneration(), now, now.Add(-time.Hour))

	claudeUsageProbe.mu.Lock()
	gateHold := claudeUsageProbe.heldUntil
	claudeUsageProbe.mu.Unlock()
	if gateHold.UnixMilli() != held.UnixMilli() {
		t.Fatalf("in-memory hold=%v, want the persisted %v", gateHold, held)
	}
	// And the hold outranks force, exactly as begin() documents.
	if claudeUsageProbe.begin(now, true) {
		t.Fatal("a gather was admitted inside the persisted hold window")
	}
	// The seed is a READ: it neither clears nor rewrites what it adopted.
	if snap := claudeCacheSnapshot(t, cache); snap.HeldUntilMs != held.UnixMilli() {
		t.Errorf("HeldUntilMs=%d, want the untouched %d", snap.HeldUntilMs, held.UnixMilli())
	}
}

// A hold stamped beyond the ceiling a Retry-After could legitimately reach is a
// clock step, not backpressure. The seed IGNORES it rather than parking
// utilization for days — and leaves it on disk, because the durable drop belongs
// to payOwedClaudeUsageRefreshAt and this path writes nothing.
func TestClaudeUsageProbeGate_CacheSeedIgnoresASkewedHold(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	skewed := now.Add(2 * claudeUsageProbeMaxRetryAfter)
	mutateClaudeRateLimitSnapshot(cache, fp, func(snap *claudeRateLimitSnapshot) bool {
		snap.HeldUntilMs = skewed.UnixMilli()
		return true
	})

	resetClaudeUsageProbeGate()
	SetClaudeUsageProbeDisabled(false)

	claudeUsageProbe.seedOwedFromCache(context.Background(), fp, claudeUsageProbe.refreshGeneration(), now, now.Add(-time.Hour))

	claudeUsageProbe.mu.Lock()
	gateHold := claudeUsageProbe.heldUntil
	claudeUsageProbe.mu.Unlock()
	if !gateHold.IsZero() {
		t.Fatalf("in-memory hold=%v, want none — a skewed hold is not backpressure", gateHold)
	}
	if snap := claudeCacheSnapshot(t, cache); snap.HeldUntilMs != skewed.UnixMilli() {
		t.Errorf("HeldUntilMs=%d, want the untouched %d — only the replay clears a skewed hold", snap.HeldUntilMs, skewed.UnixMilli())
	}
}

// The seed compares the refresh generation against the moment the CALLER read the
// cache, not the moment the seed runs. A replay landing in that gap — after
// ParseContext loaded its view, before the seed is entered — is already counted in
// a sample taken at entry, and the same write cleared the debt from disk, so
// nothing would report that the loaded view is superseded and the gather would
// publish the pre-replay reading for the whole staleness TTL.
func TestClaudeUsageProbeGate_CacheSeedReportsARefreshThatLandedBeforeItWasEntered(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	runEnded := time.Now().Add(-time.Minute)
	latest := runEnded.Add(-time.Minute) // the PRE-replay reading this gather loaded
	seedClaudeProbeReading(t, cache, latest)
	claudeOweRunRefresh(runEnded)

	resetClaudeUsageProbeGate()
	SetClaudeUsageProbeDisabled(false)

	// Sampled where the gather samples it: BEFORE the cache load whose freshness is
	// handed to the seed.
	generation := claudeUsageProbe.refreshGeneration()

	// The replay lands in the gap between that load and the seed call: it persists a
	// covering reading and clears the debt in the same write.
	settled := runEnded.Add(time.Second)
	claudeUsageProbe.mu.Lock()
	claudeUsageProbe.refreshes++
	claudeUsageProbe.lastRefreshAt = settled
	claudeUsageProbe.lastRefreshFingerprint = fp
	claudeUsageProbe.mu.Unlock()
	mutateClaudeRateLimitSnapshot(cache, fp, retireClaudeRefreshDebtAt(runEnded))

	got := claudeUsageProbe.seedOwedFromCache(context.Background(), fp, generation, time.Now(), latest)

	if got.UnixMilli() != settled.UnixMilli() {
		t.Fatalf("seedOwedFromCache()=%v, want the landed reading %v so the gather re-reads the cache", got, settled)
	}
	if owed := claudeUsageProbe.owedObservation(); !owed.IsZero() {
		t.Errorf("a settled debt was adopted on the gate as %v", owed)
	}
	if n := atomic.LoadInt64(calls); n != 0 {
		t.Errorf("the seed issued %d requests, want 0 — it is a pure cache read", n)
	}
}

// The generation counter only sees probes of THIS process, but the cache and the
// durable debt are shared: an overlapping old agent or a second agent channel can
// persist a covering reading — settling the debt in the same write — after the
// gather loaded its view. The seed must compare what is on disk now against the
// caller's view, or it finds no debt, reports nothing, and the gather publishes
// the pre-refresh reading.
func TestClaudeUsageProbeGate_CacheSeedReportsARefreshByAnotherProcess(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	runEnded := time.Now().Add(-time.Minute)
	latest := runEnded.Add(-time.Minute) // the PRE-refresh reading this gather loaded
	seedClaudeProbeReading(t, cache, latest)
	claudeOweRunRefresh(runEnded)

	resetClaudeUsageProbeGate()
	SetClaudeUsageProbeDisabled(false)

	generation := claudeUsageProbe.refreshGeneration()
	// Another process's probe writes a covering reading, which clears the debt in
	// the same write; this process's refresh counter never moves.
	settled := runEnded.Add(time.Second)
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 40, ResetsAtMs: settled.Add(time.Hour).UnixMilli(),
			ObservedAtMs: settled.UnixMilli(), usageKnown: true,
		},
	}, settled, fp, claudeRateLimitSourceProbe)
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("precondition: the covering write left the debt at %d", snap.RefreshOwedAtMs)
	}

	got := claudeUsageProbe.seedOwedFromCache(context.Background(), fp, generation, time.Now(), latest)

	if got.UnixMilli() != settled.UnixMilli() {
		t.Fatalf("seedOwedFromCache()=%v, want the other process's reading %v so the gather re-reads the cache", got, settled)
	}
	if owed := claudeUsageProbe.owedObservation(); !owed.IsZero() {
		t.Errorf("a settled debt was adopted on the gate as %v", owed)
	}
	if n := atomic.LoadInt64(calls); n != 0 {
		t.Errorf("the seed issued %d requests, want 0 — it is a pure cache read", n)
	}
}

// Two gathers entering a fresh process together: the second must not pass the
// latch while the first is still reading the cache, or it runs on with neither
// the persisted debt nor the hold adopted.
func TestClaudeUsageProbeGate_CacheSeedPeersWaitForTheAdoption(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	runEnded := now.Add(-time.Minute)
	claudeOweRunRefresh(runEnded)

	resetClaudeUsageProbeGate()
	SetClaudeUsageProbeDisabled(false)

	reading := make(chan struct{})
	release := make(chan struct{})
	originalHook := claudeUsageProbeAfterSeedRead
	t.Cleanup(func() { claudeUsageProbeAfterSeedRead = originalHook })
	claudeUsageProbeAfterSeedRead = func() {
		close(reading)
		<-release
	}
	first := make(chan struct{})
	go func() {
		defer close(first)
		claudeUsageProbe.seedOwedFromCache(context.Background(), fp, claudeUsageProbe.refreshGeneration(), now, now.Add(-time.Hour))
	}()
	<-reading

	peer := make(chan time.Time, 1)
	go func() {
		claudeUsageProbe.seedOwedFromCache(context.Background(), fp, claudeUsageProbe.refreshGeneration(), now, now.Add(-time.Hour))
		peer <- claudeUsageProbe.owedObservation()
	}()
	select {
	case owed := <-peer:
		t.Fatalf("a peer returned while the first seed was mid-read (owed=%v)", owed)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-first
	select {
	case owed := <-peer:
		if owed.UnixMilli() != runEnded.UnixMilli() {
			t.Fatalf("the peer saw owed=%v, want the adopted debt %v", owed, runEnded)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the peer never returned after the first seed finished")
	}
}

// A peer waiting on someone else's seed is still bounded by its own gather.
func TestClaudeUsageProbeGate_CacheSeedPeerWaitHonoursItsContext(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))

	resetClaudeUsageProbeGate()
	SetClaudeUsageProbeDisabled(false)

	reading := make(chan struct{})
	release := make(chan struct{})
	originalHook := claudeUsageProbeAfterSeedRead
	t.Cleanup(func() { claudeUsageProbeAfterSeedRead = originalHook })
	claudeUsageProbeAfterSeedRead = func() {
		close(reading)
		<-release
	}
	first := make(chan struct{})
	go func() {
		defer close(first)
		claudeUsageProbe.seedOwedFromCache(context.Background(), fp, claudeUsageProbe.refreshGeneration(), time.Now(), time.Time{})
	}()
	<-reading
	defer func() { close(release); <-first }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := claudeUsageProbe.seedOwedFromCache(ctx, fp, claudeUsageProbe.refreshGeneration(), time.Now(), time.Time{}); !got.IsZero() {
		t.Fatalf("seedOwedFromCache()=%v, want zero from a gather whose context ended", got)
	}
}

// The latch is scoped to the account. A first gather that could not identify it —
// a transient Keychain timeout hands it an empty fingerprint, which no scoped
// snapshot matches — must not consume the real account's only chance to adopt
// its debt.
func TestClaudeUsageProbeGate_CacheSeedRetriesAfterAnUnknownAccount(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	const fp = "scoped-account"
	runEnded := time.Now().Add(-time.Minute)
	mutateClaudeRateLimitSnapshot(cache, fp, func(snap *claudeRateLimitSnapshot) bool {
		snap.RefreshOwedAtMs = runEnded.UnixMilli()
		return true
	})

	resetClaudeUsageProbeGate()
	SetClaudeUsageProbeDisabled(false)

	claudeUsageProbe.seedOwedFromCache(context.Background(), "", claudeUsageProbe.refreshGeneration(), time.Now(), time.Time{})
	if owed := claudeUsageProbe.owedObservation(); !owed.IsZero() {
		t.Fatalf("an unidentified gather adopted a scoped debt: %v", owed)
	}
	claudeUsageProbe.seedOwedFromCache(context.Background(), fp, claudeUsageProbe.refreshGeneration(), time.Now(), time.Time{})
	if owed := claudeUsageProbe.owedObservation(); owed.UnixMilli() != runEnded.UnixMilli() {
		t.Fatalf("owedObservation()=%v, want the persisted debt %v once the account is known", owed, runEnded)
	}
}

// The attempt charge pays for a turn at the endpoint. An admitted attempt that
// returns because the credential store handed back no token — a transient
// Keychain timeout on macOS — asked nothing and learned nothing, so the charge
// bought nothing: two such starts would otherwise exhaust the cap and retire a
// debt that was never once put to the endpoint, leaving the pre-run card stale.
func TestPayOwedClaudeUsageRefresh_RefundsAChargeWhenTheCredentialStoreYieldsNoToken(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	owed := now.Add(-time.Minute)
	claudeOweRunRefresh(owed)

	// The credential is readable and parses, but carries no access token — what a
	// failed Keychain read leaves claudeUsageProbeStoredIdentity holding. The
	// account (and so the cache fingerprint) is unchanged.
	writeClaudeProbeCredential(t, os.Getenv("CLAUDE_CONFIG_DIR"), "")

	payOwedClaudeUsageRefreshAt(now)
	claudeFreshnessWaitIdle(t)

	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("request count=%d, want 0 — there was no token to ask with", got)
	}
	snap := claudeCacheSnapshot(t, cache)
	if snap.RefreshOwedAtMs != owed.UnixMilli() {
		t.Fatalf("the debt must stand for the next start: %+v", snap)
	}
	if snap.RefreshOwedAttempts != 0 {
		t.Fatalf("RefreshOwedAttempts=%d, want 0 — a charge that bought no request is refunded", snap.RefreshOwedAttempts)
	}
}

// A transiently unreadable credential resolves to exactly the "" an accountless
// claude.ai login does. Owing a debt through it must NOT be taken for an account
// boundary: that would drop the scoped buckets and stamp the debt under "",
// where the next start — resolving the recovered fingerprint — never looks for
// it. The durable write is skipped instead; the in-memory debt still stands.
func TestClaudeOweRunRefresh_UnresolvedFingerprintLeavesAScopedCacheAlone(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	if fp := currentClaudeAccountFingerprint(); fp != "" {
		t.Fatalf("the fixture credential resolved to %q, want the unscoped fixture this case needs", fp)
	}

	observedAt := time.Now().Add(-time.Hour)
	if !mutateClaudeRateLimitSnapshot(cache, "scoped-account", func(snap *claudeRateLimitSnapshot) bool {
		snap.Buckets[claudeWindowFiveHour] = claudeRateLimitBucket{
			UsedPercentage: 41, ResetsAtMs: observedAt.Add(time.Hour).UnixMilli(),
			ObservedAtMs: observedAt.UnixMilli(), usageKnown: true,
		}
		return true
	}) {
		t.Fatal("seeding a scoped snapshot wrote nothing")
	}
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
		t.Fatalf("an unresolved fingerprint rewrote a scoped cache:\nbefore %s\nafter  %s", before, after)
	}
	snap := claudeCacheSnapshot(t, cache)
	if snap.AccountFingerprint != "scoped-account" {
		t.Fatalf("AccountFingerprint=%q, want the scoped account preserved", snap.AccountFingerprint)
	}
	if len(snap.Buckets) != 1 {
		t.Fatalf("Buckets=%v, want the scoped reading preserved", snap.Buckets)
	}
	if snap.RefreshOwedAtMs != 0 {
		t.Fatalf("RefreshOwedAtMs=%d, want no debt written under an unknown scope", snap.RefreshOwedAtMs)
	}
}

// An UNSCOPED cache is the ordinary claude.ai case, not a failure: a debt still
// persists there. The guard above must refuse the scoped -> "" downgrade only.
func TestClaudeOweRunRefresh_UnscopedCacheStillTakesTheDebt(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)

	runEnded := time.Now()
	claudeOweRunRefresh(runEnded)

	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != runEnded.UnixMilli() {
		t.Fatalf("RefreshOwedAtMs=%d, want the debt at %d on an unscoped cache", snap.RefreshOwedAtMs, runEnded.UnixMilli())
	}
}

// A gather that waited on someone else's seed — or simply arrived after it —
// must inherit the re-read verdict, not just the adopted debt. The claimant's
// answer was about the claimant's `latest`; a peer that loaded the pre-replay
// view and is told "nothing to re-read" publishes exactly the stale utilization
// this whole path exists to prevent.
func TestClaudeUsageProbeGate_CacheSeedLatchedPeerInheritsTheReReadVerdict(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	runEnded := time.Now().Add(-time.Minute)
	stale := runEnded.Add(-time.Minute) // the PRE-replay view both gathers loaded
	seedClaudeProbeReading(t, cache, stale)
	claudeOweRunRefresh(runEnded)

	resetClaudeUsageProbeGate()
	SetClaudeUsageProbeDisabled(false)

	generation := claudeUsageProbe.refreshGeneration()
	// The replay (or another agent channel) persists a covering reading, clearing
	// the debt in the same locked write.
	settled := runEnded.Add(time.Second)
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 40, ResetsAtMs: settled.Add(time.Hour).UnixMilli(),
			ObservedAtMs: settled.UnixMilli(), usageKnown: true,
		},
	}, settled, fp, claudeRateLimitSourceProbe)

	now := time.Now()
	// The claimant takes the latch and is told to re-read.
	if got := claudeUsageProbe.seedOwedFromCache(context.Background(), fp, generation, now, stale); got.UnixMilli() != settled.UnixMilli() {
		t.Fatalf("precondition: the claimant got %v, want the covering reading %v", got, settled)
	}
	// The peer holds the same pre-replay view and is turned away by the latch.
	if got := claudeUsageProbe.seedOwedFromCache(context.Background(), fp, generation, now, stale); got.UnixMilli() != settled.UnixMilli() {
		t.Fatalf("a latched peer got %v, want the same re-read verdict %v", got, settled)
	}
	// A caller already holding the covering reading has nothing to re-read.
	if got := claudeUsageProbe.seedOwedFromCache(context.Background(), fp, generation, now, settled); !got.IsZero() {
		t.Errorf("a latched caller already holding the reading got %v, want zero", got)
	}
	if n := atomic.LoadInt64(calls); n != 0 {
		t.Errorf("the seed issued %d requests, want 0 — the latched path re-reads nothing", n)
	}
}

// Opting out of the probe must not erase utilization the status-line path
// supplies independently of it. A transiently empty fingerprint over a scoped
// cache is an unresolved identity, not a logout, so the opt-out clear is skipped
// rather than allowed to take the buckets down with the debt.
func TestPayOwedClaudeUsageRefresh_OptOutLeavesAScopedCacheAloneWhenTheIdentityIsUnresolved(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	if fp := currentClaudeAccountFingerprint(); fp != "" {
		t.Fatalf("the fixture credential resolved to %q, want the unscoped fixture this case needs", fp)
	}
	runEnded := time.Now().Add(-time.Minute)
	observed := runEnded.Add(-time.Minute)
	if !mutateClaudeRateLimitSnapshot(cache, "scoped-account", func(snap *claudeRateLimitSnapshot) bool {
		snap.RefreshOwedAtMs = runEnded.UnixMilli()
		snap.Buckets[claudeWindowFiveHour] = claudeRateLimitBucket{
			UsedPercentage: 55, ResetsAtMs: observed.Add(time.Hour).UnixMilli(),
			ObservedAtMs: observed.UnixMilli(), usageKnown: true,
		}
		return true
	}) {
		t.Fatal("seeding a scoped snapshot wrote nothing")
	}
	before, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}

	SetClaudeUsageProbeDisabled(true)
	t.Cleanup(func() { SetClaudeUsageProbeDisabled(false) })
	payOwedClaudeUsageRefreshAt(time.Now())

	after, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("the opt-out clear rewrote a scoped cache under an unresolved identity:\nbefore %s\nafter  %s", before, after)
	}
	if n := atomic.LoadInt64(calls); n != 0 {
		t.Errorf("an opted-out replay issued %d requests, want 0", n)
	}
}

// The opt-out clear still runs when the identity IS resolved — the guard above
// must refuse the scoped -> "" downgrade only, or a debt nothing can pay is
// stranded forever.
func TestPayOwedClaudeUsageRefresh_OptOutClearsADebtUnderAMatchingScope(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	observed := time.Now().Add(-2 * time.Minute)
	seedClaudeProbeReading(t, cache, observed)
	if !mutateClaudeRateLimitSnapshot(cache, fp, func(snap *claudeRateLimitSnapshot) bool {
		snap.RefreshOwedAtMs = time.Now().Add(-time.Minute).UnixMilli()
		return true
	}) {
		t.Fatal("seeding the debt wrote nothing")
	}

	SetClaudeUsageProbeDisabled(true)
	t.Cleanup(func() { SetClaudeUsageProbeDisabled(false) })
	payOwedClaudeUsageRefreshAt(time.Now())

	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("RefreshOwedAtMs=%d, want an opted-out debt retired", snap.RefreshOwedAtMs)
	}
	if snap := claudeCacheSnapshot(t, cache); len(snap.Buckets) != 1 {
		t.Errorf("Buckets=%v, want the existing reading preserved by the clear", snap.Buckets)
	}
	if n := atomic.LoadInt64(calls); n != 0 {
		t.Errorf("an opted-out replay issued %d requests, want 0", n)
	}
}

// The scoped -> unscoped refusal lives INSIDE the locked mutation, so it holds
// even when another writer scoped the snapshot after an unlocked check would
// have passed: an empty fingerprint is never read as an account boundary by a
// debt or hold marker, and fn never sees the snapshot.
func TestMutateClaudeRateLimitSnapshot_RefusesAnUnresolvedIdentityOverAScopedCache(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	observedAt := time.Now().Add(-time.Hour)
	if !mutateClaudeRateLimitSnapshot(cache, "scoped-account", func(snap *claudeRateLimitSnapshot) bool {
		snap.Buckets[claudeWindowFiveHour] = claudeRateLimitBucket{
			UsedPercentage: 41, ResetsAtMs: observedAt.Add(time.Hour).UnixMilli(),
			ObservedAtMs: observedAt.UnixMilli(), usageKnown: true,
		}
		return true
	}) {
		t.Fatal("seeding a scoped snapshot wrote nothing")
	}
	before, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}

	ran := false
	if mutateClaudeRateLimitSnapshot(cache, "", func(snap *claudeRateLimitSnapshot) bool {
		ran = true
		snap.RefreshOwedAtMs = time.Now().UnixMilli()
		return true
	}) {
		t.Fatal("an unresolved fingerprint was allowed to rewrite a scoped cache")
	}
	if ran {
		t.Fatal("fn ran against a scoped snapshot under an unresolved fingerprint")
	}
	after, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("the scoped cache changed:\nbefore %s\nafter  %s", before, after)
	}
}

// An admitted attempt that returns before asking anything — the credential
// store handing back no token — gives back the in-memory throttle slot as well
// as the durable attempt charge, so the next caller (the startup gather that
// adopted the debt) is not refused for a whole interval by a probe that never
// left the process.
func TestClaudeUsageProbe_UnissuedAdmissionRefundsTheThrottleSlot(t *testing.T) {
	_, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	earlier := time.Now().Add(-time.Hour)
	claudeUsageProbe.mu.Lock()
	claudeUsageProbe.lastAttempt = earlier
	claudeUsageProbe.mu.Unlock()

	noToken := func() claudeUsageProbeIdentity { return claudeUsageProbeIdentity{} }
	admitted, issued, _, _, _ := probeClaudeUsageAdmitted(context.Background(), time.Now(), noToken, time.Time{}, false)
	if !admitted || issued {
		t.Fatalf("admitted=%v issued=%v, want an admitted attempt that issued nothing", admitted, issued)
	}
	claudeUsageProbe.mu.Lock()
	got := claudeUsageProbe.lastAttempt
	claudeUsageProbe.mu.Unlock()
	if !got.Equal(earlier) {
		t.Fatalf("lastAttempt=%v, want the pre-admission %v restored", got, earlier)
	}
	if !claudeUsageProbe.begin(time.Now(), false) {
		t.Fatal("the next attempt was throttled by an admission that issued nothing")
	}
	claudeUsageProbe.finish(nil, false, time.Time{}, "")
	if n := atomic.LoadInt64(calls); n != 0 {
		t.Errorf("issued %d requests, want 0", n)
	}
}

// The startup replay can land AFTER the seed has returned — settling the debt
// the seed adopted — so the gather then finds nothing owed and a view that looks
// fresh. It must still report the landed reading so the caller re-reads the
// cache, rather than publishing the pre-replay view for the whole TTL.
func TestRefreshClaudeUsageIfStale_ReportsAReplayThatLandedAfterTheSeed(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	latest := time.Now().Add(-2 * time.Minute) // fresh by the TTL
	seedClaudeProbeReading(t, cache, latest)

	generation := claudeUsageProbe.refreshGeneration()
	// The seed has already run for this account and found nothing to re-read.
	if got := claudeUsageProbe.seedOwedFromCache(context.Background(), fp, generation, time.Now(), latest); !got.IsZero() {
		t.Fatalf("seedOwedFromCache()=%v, want zero before the replay lands", got)
	}
	// The replay lands now, after the seed.
	claudeUsageProbe.mu.Lock()
	claudeUsageProbe.refreshes++
	claudeUsageProbe.lastRefreshAt = latest.Add(time.Minute)
	claudeUsageProbe.lastRefreshFingerprint = fp
	claudeUsageProbe.mu.Unlock()

	if !refreshClaudeUsageIfStaleFrom(context.Background(), generation, time.Now(), latest, "token", fp) {
		t.Fatal("a replay that landed after the seed was not reported, so the gather would publish the pre-replay view")
	}
	// A gather that sampled its generation AFTER the replay holds nothing stale.
	if refreshClaudeUsageIfStaleFrom(context.Background(), claudeUsageProbe.refreshGeneration(), time.Now(), latest, "token", fp) {
		t.Fatal("an unchanged generation must not ask for a re-read")
	}
	if n := atomic.LoadInt64(calls); n != 0 {
		t.Errorf("issued %d requests, want 0 — the view was fresh", n)
	}
}

// A routine gather that owes a debt and finds the slot held by the probe paying
// it — the startup replay, or this process's trailing probe — joins that probe
// instead of being refused and publishing the pre-run view.
func TestRefreshClaudeUsageIfStale_OwingGatherJoinsTheInFlightPayer(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	runEnded := time.Now().Add(-30 * time.Second)
	latest := runEnded.Add(-time.Minute) // the PRE-run reading
	seedClaudeProbeReading(t, cache, latest)
	claudeUsageProbe.recordOwed(runEnded)

	// The payer holds the slot and lands a covering reading shortly.
	if !claudeUsageProbe.begin(time.Now(), false) {
		t.Fatal("the simulated payer was not admitted")
	}
	covering := runEnded.Add(time.Second)
	go func() {
		time.Sleep(50 * time.Millisecond)
		claudeUsageProbe.finish(nil, true, covering, fp)
	}()

	generation := claudeUsageProbe.refreshGeneration()
	if !refreshClaudeUsageIfStaleFrom(context.Background(), generation, time.Now(), latest, "token", fp) {
		t.Fatal("an owing gather refused by the payer's slot did not report the covering reading it landed")
	}
	if owed := claudeUsageProbe.owedObservation(); !owed.IsZero() {
		t.Fatalf("owedObservation()=%v, want the debt settled by the joined reading", owed)
	}
	if n := atomic.LoadInt64(calls); n != 0 {
		t.Errorf("issued %d requests, want 0 — the joined probe answered", n)
	}
}

// The post-seed replay can also land WITHOUT persisting anything itself: another
// process wrote a covering reading first, so the replay takes the cross-process
// dedupe path and settles the debt with it. That shared reading is on disk, so an
// overlapping gather that loaded its view earlier must still re-read — while a
// forced refresh joining that probe must not inherit it as an answer.
func TestRefreshClaudeUsageIfStale_ReportsADedupedReplayThatLandedAfterTheSeed(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	latest := time.Now().Add(-2 * time.Minute) // fresh by the TTL
	seedClaudeProbeReading(t, cache, latest)

	generation := claudeUsageProbe.refreshGeneration()
	if got := claudeUsageProbe.seedOwedFromCache(context.Background(), fp, generation, time.Now(), latest); !got.IsZero() {
		t.Fatalf("seedOwedFromCache()=%v, want zero before the replay lands", got)
	}
	// The replay is admitted, finds another writer's covering reading, and
	// finishes on the dedupe path: nothing written by us, an observation reported.
	if !claudeUsageProbe.begin(time.Now(), false) {
		t.Fatal("the simulated replay was not admitted")
	}
	before := claudeUsageProbe.refreshGeneration()
	joined := make(chan time.Time, 1)
	go func() {
		ok, at := claudeUsageProbe.joinInFlight(context.Background(), fp)
		if !ok {
			at = time.Unix(1, 0) // sentinel: the join did not happen
		}
		joined <- at
	}()
	for {
		claudeUsageProbe.mu.Lock()
		waiting := claudeUsageProbe.doneCh != nil
		claudeUsageProbe.mu.Unlock()
		if waiting {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let the joiner park on doneCh
	claudeUsageProbe.finish(nil, false, latest.Add(time.Minute), fp)

	if at := <-joined; !at.IsZero() {
		t.Fatalf("joinInFlight()=%v, want a joined-but-unusable answer — a forced refresh never inherits a dedupe", at)
	}
	if claudeUsageProbe.refreshGeneration() == before {
		t.Fatal("a deduped replay did not advance the refresh generation")
	}
	if !refreshClaudeUsageIfStaleFrom(context.Background(), generation, time.Now(), latest, "token", fp) {
		t.Fatal("a deduped replay that landed after the seed was not reported, so the gather would publish the pre-replay view")
	}
	// Another account's shared reading says nothing about this one.
	if refreshClaudeUsageIfStaleFrom(context.Background(), generation, time.Now(), latest, "token", "someone-else") {
		t.Fatal("a shared reading for another account must not ask this one to re-read")
	}
	if n := atomic.LoadInt64(calls); n != 0 {
		t.Errorf("issued %d requests, want 0 — the view was fresh", n)
	}
}
