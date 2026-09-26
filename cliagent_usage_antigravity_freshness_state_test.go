package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The run-completion debt state machine, without a spawn, a poller or the
// network. Every seam is pinned small here, so no case pays the shipped 60 s
// interval or the 5 s retry delay.

// helperIsolateAntigravityFreshness points the debt file, the quota cache and
// the Code Assist read at test-owned locations and shrinks the timing seams.
func helperIsolateAntigravityFreshness(t *testing.T) (state, cache string) {
	t.Helper()
	helperStopAntigravityRefreshSchedule()
	state = filepath.Join(t.TempDir(), "agy_freshness.json")
	cache = filepath.Join(t.TempDir(), "agyq.json")
	t.Setenv(antigravityFreshnessEnv, state)
	t.Setenv("AIEXPEDITE_AGY_QUOTA_CACHE", cache)
	helperResetAntigravityLiveRuns()
	helperIsolateAntigravityGate(t)

	origRetry, origInterval, origNow := antigravityRefreshAfterRunRetryDelay, antigravityRefreshMinInterval, antigravityUsageFreshnessNow
	t.Cleanup(func() {
		// The worker reads all three, so it has to be out of flight first —
		// and no retry rung may fire into the next test's state file.
		helperStopAntigravityRefreshSchedule()
		antigravityRefreshAfterRunRetryDelay = origRetry
		antigravityRefreshMinInterval = origInterval
		antigravityUsageFreshnessNow = origNow
	})
	antigravityRefreshAfterRunRetryDelay = time.Millisecond
	// `agy` has to look installed, or every debt retires before it is paid.
	helperFakeAgyOnPath(t)
	return state, cache
}

// helperStopAntigravityRefreshSchedule cancels a pending retry rung and waits
// out every worker, so nothing a test booked outlives it. The ladder's shipped
// rungs are a minute and longer, so a test that does not drain the schedule
// itself leaves a timer that would otherwise fire into a later test.
func helperStopAntigravityRefreshSchedule() {
	stopAntigravityRunDebtRetry()
	antigravityUsageRefreshWaitIdle()
	// A worker that was still in flight may have booked a rung on its way out.
	stopAntigravityRunDebtRetry()
	// The nudge cooldown is per process, so one test's gather must not hold
	// back the next test's.
	antigravityRefreshNudge.mu.Lock()
	antigravityRefreshNudge.lastAt = time.Time{}
	antigravityRefreshNudge.mu.Unlock()
}

// helperFakeAgyOnPath puts a trivial `agy` first on PATH so the CLI looks
// installed and a debt is not retired before it can be paid. Deliberately not
// helperMockAgyOnPath: that copies the whole test binary (tens of MB) per call,
// which these cases pay for nothing — none of them spawns `agy`, and the debt
// worker only asks whether it EXISTS. A case that needs the version probe to
// answer must use the real mock binary instead: Windows cannot CreateProcess
// the `.cmd` written here.
func helperFakeAgyOnPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	name, body, mode := "agy", "#!/bin/sh\necho 'agy version 1.2.3'\n", os.FileMode(0o755)
	if runtime.GOOS == "windows" {
		name, body = "agy.cmd", "@echo off\r\necho agy version 1.2.3\r\n"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("write fake agy: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

// helperWriteAntigravityCache seeds a cached reading observed at `at`.
func helperWriteAntigravityCache(t *testing.T, cache string, at time.Time) {
	t.Helper()
	helperWriteAntigravityCacheAt(t, cache, at, at.UnixMilli())
}

// helperWriteAntigravityCacheAt seeds a cached reading with an explicit
// millisecond instant; 0 is a reading cached by an agent that predates the
// field, which carries only the RFC3339 second.
func helperWriteAntigravityCacheAt(t *testing.T, cache string, at time.Time, observedAtMs int64) {
	t.Helper()
	snap := antigravityQuotaSnapshot{
		ObservedAt:         at.UTC().Format(time.RFC3339),
		ObservedAtMs:       observedAtMs,
		AccountFingerprint: fingerprintAccount("antigravity", "ada@example.com"),
		Account:            "ada@example.com",
		Buckets: []antigravityQuotaBucket{
			{Group: "Gemini Models", Window: "weekly", RemainingFraction: 0.4, ResetTime: "2126-08-14T00:00:00Z"},
		},
	}
	helperWriteJSON(t, cache, snap)
}

// Only a reading taken at or after the run COMPLETED retires the debt. A
// turn's quota is debited at the end of the turn, so a snapshot taken while the
// run was still going — a Refresh click mid-run, or an overlapping run's Code
// Assist read — does not contain this run's usage and must not be mistaken for
// coverage. The boundary is the whole settle decision; a flag would have made
// any newer reading look like the run's own.
func TestAntigravityFreshness_ReadingClearsOnlyAtOrAfterTheRunCompleted(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	floor := time.Now().Truncate(time.Second)
	// A run that took a minute, so "during the run" and "after the run" are
	// distinguishable instants rather than the same millisecond.
	completed := floor.Add(time.Minute)
	antigravityUsageFreshnessNow = func() time.Time { return completed }

	for _, tc := range []struct {
		name      string
		observed  time.Time
		wantOwing bool
	}{
		{"a reading from before the run", floor.Add(-time.Minute), true},
		{"a reading at exactly the run's start", floor, true},
		{"a reading from during the run", floor.Add(30 * time.Second), true},
		{"a reading at exactly the completion", completed, false},
		{"a reading from after the run", completed.Add(30 * time.Second), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(antigravityFreshnessPath())
			helperWriteAntigravityCache(t, cache, tc.observed)
			calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })

			antigravityUsageRunSettled(floor, false, true)
			antigravityUsageRefreshWaitIdle()

			if owing := helperFreshnessState(t).RefreshOwedAtMs != 0; owing != tc.wantOwing {
				t.Errorf("owing=%v, want %v", owing, tc.wantOwing)
			}
			if tc.wantOwing && calls.Load() == 0 {
				t.Error("an unpaid run spent no Code Assist read")
			}
			if !tc.wantOwing && calls.Load() != 0 {
				t.Errorf("a covered run spent %d Code Assist reads", calls.Load())
			}
		})
	}
}

// The completion boundary holds below one second. observedAt is RFC3339
// seconds, and the run-completion comparison used to round every reading up by
// a whole second to absorb that — which let a Refresh click, or an overlapping
// run's read, taken a few hundred milliseconds BEFORE a run completed pass as
// that run's own reading. The exact instant decides now, and a reading known
// only to the second (cached by an older agent) is taken as the START of that
// second: it can cost a refresh, never lose one.
func TestAntigravityFreshness_SubSecondBoundaryIsExact(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	floor := time.Now().Truncate(time.Second)
	completed := floor.Add(time.Minute + 600*time.Millisecond)
	antigravityUsageFreshnessNow = func() time.Time { return completed }

	for _, tc := range []struct {
		name       string
		observedMs int64
		wantOwing  bool
	}{
		{"400 ms before the completion, same second", completed.Add(-400 * time.Millisecond).UnixMilli(), true},
		{"1 ms before the completion", completed.Add(-time.Millisecond).UnixMilli(), true},
		{"exactly the completion", completed.UnixMilli(), false},
		{"100 ms after the completion, same second", completed.Add(100 * time.Millisecond).UnixMilli(), false},
		{"same second, no millisecond instant", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(antigravityFreshnessPath())
			helperWriteAntigravityCacheAt(t, cache, completed, tc.observedMs)
			calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })

			antigravityUsageRunSettled(floor, false, true)
			antigravityUsageRefreshWaitIdle()

			if owing := helperFreshnessState(t).RefreshOwedAtMs != 0; owing != tc.wantOwing {
				t.Errorf("owing=%v, want %v", owing, tc.wantOwing)
			}
			if !tc.wantOwing && calls.Load() != 0 {
				t.Errorf("a covered run spent %d Code Assist reads", calls.Load())
			}
		})
	}
}

// A millisecond instant is trusted only inside the second observedAt names;
// anything else falls back to the start of that second.
func TestAntigravitySnapshotObservedMs(t *testing.T) {
	second := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	at := second.Format(time.RFC3339)
	for _, tc := range []struct {
		name string
		snap antigravityQuotaSnapshot
		want int64
	}{
		{"exact instant", antigravityQuotaSnapshot{ObservedAt: at, ObservedAtMs: second.UnixMilli() + 750}, second.UnixMilli() + 750},
		{"no instant", antigravityQuotaSnapshot{ObservedAt: at}, second.UnixMilli()},
		{"instant in a later second", antigravityQuotaSnapshot{ObservedAt: at, ObservedAtMs: second.UnixMilli() + 1000}, second.UnixMilli()},
		{"instant in an earlier second", antigravityQuotaSnapshot{ObservedAt: at, ObservedAtMs: second.UnixMilli() - 1}, second.UnixMilli()},
		{"unparseable", antigravityQuotaSnapshot{ObservedAt: "yesterday", ObservedAtMs: second.UnixMilli()}, 0},
	} {
		if got := antigravitySnapshotObservedMs(tc.snap); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

// Two readings inside the same second are ordered by their millisecond
// instants, so a post-completion reading is not refused as "not newer" than a
// mid-run one stamped with the same RFC3339 second.
func TestSaveAntigravityQuotaSnapshotIfNewer_OrdersWithinOneSecond(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	second := time.Now().Truncate(time.Second)
	helperWriteAntigravityCacheAt(t, cache, second, second.UnixMilli()+200)

	fp := fingerprintAccount("antigravity", "ada@example.com")
	reading := func(ms int64) antigravityQuotaSnapshot {
		return antigravityQuotaSnapshot{
			ObservedAt: second.UTC().Format(time.RFC3339), ObservedAtMs: ms,
			AccountFingerprint: fp, Account: "ada@example.com",
			Buckets: []antigravityQuotaBucket{
				{Group: "Gemini Models", Window: "weekly", RemainingFraction: 0.3, ResetTime: "2126-08-14T00:00:00Z"},
			},
		}
	}
	if saveAntigravityQuotaSnapshotIfNewer(reading(second.UnixMilli() + 100)) {
		t.Error("an earlier reading in the same second replaced a later one")
	}
	if !saveAntigravityQuotaSnapshotIfNewer(reading(second.UnixMilli() + 800)) {
		t.Error("a later reading in the same second was refused as not newer")
	}
	if got := cachedAntigravityObservedMs(); got != second.UnixMilli()+800 {
		t.Errorf("cached instant=%d, want %d", got, second.UnixMilli()+800)
	}
}

// The attempt caps: the settle pass spends one immediate read and one retry,
// later passes spend what is left of the debt's lifetime budget, and then the
// debt stops spending however long it stays unpaid.
func TestAntigravityFreshness_AttemptCapIsHonoured(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	floor := time.Now()
	helperWriteAntigravityCache(t, cache, floor.Add(-time.Hour))
	calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })
	antigravityRefreshMinInterval = time.Nanosecond

	antigravityUsageRunSettled(floor, false, true)
	antigravityUsageRefreshWaitIdle()
	if got := calls.Load(); got != int64(antigravityRefreshAfterRunMaxAttempts) {
		t.Fatalf("reads=%d, want the settle pass's %d", got, antigravityRefreshAfterRunMaxAttempts)
	}
	state := helperFreshnessState(t)
	if state.RefreshOwedAtMs == 0 || state.Attempts != antigravityRefreshAfterRunMaxAttempts {
		t.Fatalf("state=%+v, want the debt kept with the settle pass's attempts booked", state)
	}
	if state.NextAttemptAtMs == 0 || !antigravityRunDebtRetryPending() {
		t.Fatalf("state=%+v pending=%v, want the kept debt scheduled", state, antigravityRunDebtRetryPending())
	}

	// Later passes may spend the rest of the lifetime budget and no more.
	for i := 0; i < antigravityRefreshDebtMaxAttempts; i++ {
		antigravityStartRunDebtWorker(antigravityRefreshAfterRunMaxAttempts, true)
		antigravityUsageRefreshWaitIdle()
	}
	if got := calls.Load(); got != int64(antigravityRefreshDebtMaxAttempts) {
		t.Errorf("reads=%d after repeated workers, want the lifetime cap %d", got, antigravityRefreshDebtMaxAttempts)
	}
	state = helperFreshnessState(t)
	if state.RefreshOwedAtMs == 0 || state.Attempts != antigravityRefreshDebtMaxAttempts || state.NextAttemptAtMs != 0 {
		t.Errorf("state=%+v, want the spent debt kept (for the notice) with nothing scheduled", state)
	}
}

// The minimum interval spaces the outbound call a NEW run's debt triggers. A
// burst of short runs therefore costs at most one read per interval.
func TestAntigravityFreshness_MinimumIntervalBlocksTheNextRunsPayment(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	helperWriteAntigravityCache(t, cache, time.Now().Add(-time.Hour))
	calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistNotSigned })

	antigravityUsageRunSettled(time.Now(), false, true)
	antigravityUsageRefreshWaitIdle()
	first := calls.Load()
	if first == 0 {
		t.Fatal("the first run spent no read")
	}

	// A second run seconds later: the debt's floor advances, its budget resets,
	// and the interval — not the cap — is what holds the outbound call back.
	antigravityUsageRunSettled(time.Now(), false, true)
	antigravityUsageRefreshWaitIdle()
	if got := calls.Load(); got != first {
		t.Errorf("reads=%d, want the interval to block the second run's payment (was %d)", got, first)
	}
	state := helperFreshnessState(t)
	if state.RefreshOwedAtMs == 0 {
		t.Fatal("the blocked debt was dropped instead of kept")
	}
	// Kept is not enough: something has to come back for it. The deferral
	// books a rung no earlier than the interval allows.
	if state.NextAttemptAtMs < state.LastPaidAtMs+antigravityRefreshMinInterval.Milliseconds() || !antigravityRunDebtRetryPending() {
		t.Fatalf("state=%+v pending=%v, want a retry booked once the interval lapses", state, antigravityRunDebtRetryPending())
	}
	// The interval lapses and the rung fires: the deferred run is paid.
	antigravityRefreshMinInterval = time.Nanosecond
	antigravityArmRunDebtRetry(state.debtID(), 0)
	helperDrainAntigravityRefreshScheduleOnce(t)
	if got := calls.Load(); got != first+1 {
		t.Errorf("reads=%d, want the rung to pay the deferred run once (was %d)", got, first)
	}
}

// helperDrainAntigravityRefreshScheduleOnce waits for the rung that was just
// armed to fire and its worker to finish, then stops whatever that worker
// booked next — shipped rungs are a minute and longer.
func helperDrainAntigravityRefreshScheduleOnce(t *testing.T) {
	t.Helper()
	timer := &antigravityRunDebtRetryTimer
	timer.mu.Lock()
	armed := timer.gen
	timer.mu.Unlock()
	deadline := time.Now().Add(30 * time.Second)
	for {
		// Fired: the callback cleared the timer, or the worker it started has
		// already booked the next rung (a new generation).
		timer.mu.Lock()
		fired := timer.timer == nil || timer.gen != armed
		timer.mu.Unlock()
		if fired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the armed rung never fired")
		}
		time.Sleep(time.Millisecond)
	}
	antigravityUsageRefreshWaitIdle()
}

// A debt whose worker is already in flight must not start a second one: the
// debt they would both pay is the same unpaid run.
func TestAntigravityFreshness_SingleFlightBlocksAConcurrentPayment(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	helperWriteAntigravityCache(t, cache, time.Now().Add(-time.Hour))
	antigravityRefreshMinInterval = time.Nanosecond
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	calls := helperStubAntigravityCodeAssistOutcome(t, func() string {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return liveProbeOutcomeCodeAssistHTTPError
	})

	antigravityUsageRunSettled(time.Now(), false, true)
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the debt worker never started")
	}
	antigravityStartRunDebtWorker(antigravityRefreshAfterRunMaxAttempts, true)
	antigravityStartRunDebtWorker(antigravityRefreshAfterRunMaxAttempts, true)
	if got := calls.Load(); got != 1 {
		t.Errorf("reads=%d while one worker is in flight, want 1", got)
	}
	close(release)
	antigravityUsageRefreshWaitIdle()
}

// no_login stops attempting at once (nothing on this machine can pay it) but
// keeps the debt, so the card can say why the reading is not moving.
// token_expired keeps both the debt AND its budget: the next real run refreshes
// the keyring token for free, and the worker must never spawn `agy` to do it.
func TestAntigravityFreshness_LocalRefusalsDecideWhetherToKeepSpending(t *testing.T) {
	for _, tc := range []struct {
		outcome      string
		wantAttempts int
		wantReads    int64
	}{
		{liveProbeOutcomeCodeAssistNoLogin, antigravityRefreshDebtMaxAttempts, 1},
		{liveProbeOutcomeCodeAssistTokenExpired, 0, 1},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			_, cache := helperIsolateAntigravityFreshness(t)
			helperWriteAntigravityCache(t, cache, time.Now().Add(-time.Hour))
			calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return tc.outcome })

			antigravityUsageRunSettled(time.Now(), false, true)
			antigravityUsageRefreshWaitIdle()

			state := helperFreshnessState(t)
			if state.RefreshOwedAtMs == 0 {
				t.Fatalf("%s dropped the debt", tc.outcome)
			}
			if state.Attempts != tc.wantAttempts {
				t.Errorf("attempts=%d, want %d", state.Attempts, tc.wantAttempts)
			}
			if got := calls.Load(); got != tc.wantReads {
				t.Errorf("reads=%d, want %d", got, tc.wantReads)
			}
			if state.LastPaidAtMs != 0 {
				t.Error("a local refusal sent no request and must not space the next one")
			}
			// no_login is terminal; an expired login comes back on the free
			// rung instead of waiting for a run that may never settle here.
			wantScheduled := tc.outcome == liveProbeOutcomeCodeAssistTokenExpired
			if scheduled := state.NextAttemptAtMs != 0; scheduled != wantScheduled {
				t.Errorf("nextAttemptAtMs=%d, want scheduled=%v", state.NextAttemptAtMs, wantScheduled)
			}
			if !wantScheduled {
				return
			}
			// The rung fires: one more local check, still free of budget.
			antigravityArmRunDebtRetry(state.debtID(), 0)
			helperDrainAntigravityRefreshScheduleOnce(t)
			if got := calls.Load(); got != tc.wantReads+1 {
				t.Errorf("reads=%d after the rung, want %d", got, tc.wantReads+1)
			}
			if again := helperFreshnessState(t); again.Attempts != 0 || again.NextAttemptAtMs == 0 {
				t.Errorf("state=%+v, want the budget intact and the next free rung booked", again)
			}
		})
	}
}

// helperBlockingCodeAssistStub answers `outcome`, holding the FIRST call until
// the returned channel is closed, so a test can settle another run while a
// payment is genuinely in flight.
func helperBlockingCodeAssistStub(t *testing.T, outcome string) (calls *atomic.Int64, entered <-chan struct{}, release func()) {
	t.Helper()
	gate := make(chan struct{})
	first := make(chan struct{}, 1)
	var once sync.Once
	stub := helperStubAntigravityCodeAssistOutcome(t, func() string {
		select {
		case first <- struct{}{}:
			<-gate
		default:
		}
		return outcome
	})
	t.Cleanup(func() { once.Do(func() { close(gate) }) })
	return stub, first, func() { once.Do(func() { close(gate) }) }
}

// A run that finishes while a payment is in flight must still get worked. The
// single flight is held, so its request cannot start a worker of its own — and
// the worker holding it may be one read from returning (its read succeeded,
// the login is gone, the interval blocked it, or it is the startup replay's
// single attempt). Without a re-arm the run that just finished waits for the
// NEXT run, or the next agent start, to be refreshed at all — which is the
// staleness this whole path exists to remove.
func TestAntigravityFreshness_ARunSettlingDuringTheStartupReplayIsStillPaid(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	now := time.Now()
	helperWriteAntigravityCache(t, cache, now.Add(-time.Hour))
	antigravityRefreshMinInterval = time.Nanosecond
	// A debt the previous process left behind: the startup replay adopts it and
	// spends exactly ONE attempt on it.
	helperWriteJSON(t, antigravityFreshnessPath(), antigravityUsageFreshness{
		SchemaVersion: antigravityFreshnessSchema,
		RunFloorMs:    now.Add(-10 * time.Minute).UnixMilli(),
	})
	reads, entered, release := helperBlockingCodeAssistStub(t, liveProbeOutcomeCodeAssistHTTPError)

	payOwedAntigravityUsageRefresh()
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the startup replay never reached its read")
	}

	// A real run finishes while that single attempt is still in flight.
	runFloor := time.Now()
	antigravityUsageRunSettled(runFloor, false, true)
	release()
	antigravityUsageRefreshWaitIdle()

	state := helperFreshnessState(t)
	// The debt asks for a reading covering the run's COMPLETION, which is at or
	// after the floor it armed — never the older, adopted floor.
	if state.RefreshOwedFloorMs < runFloor.UnixMilli() {
		t.Fatalf("owed floor=%d, want the finished run's completion (>= %d)", state.RefreshOwedFloorMs, runFloor.UnixMilli())
	}
	// One read for the adopted debt, then the finished run's OWN budget — not
	// one read total with the run left stranded until the age-out.
	if got := reads.Load(); got != int64(1+antigravityRefreshAfterRunMaxAttempts) {
		t.Errorf("reads=%d, want 1 for the adopted debt plus %d for the run that settled during it",
			got, antigravityRefreshAfterRunMaxAttempts)
	}
	if state.Attempts != antigravityRefreshAfterRunMaxAttempts {
		t.Errorf("attempts=%d, want the finished run charged its own budget only", state.Attempts)
	}
}

// The other half: an attempt spent on one generation of the debt must not be
// charged to the run that replaced it mid-read, or that run reaches the
// attempt cap having been tried fewer times than the cap says.
func TestAntigravityFreshness_AttemptsAreChargedToTheDebtTheyWereSpentOn(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	now := time.Now()
	helperWriteAntigravityCache(t, cache, now.Add(-time.Hour))
	antigravityRefreshMinInterval = time.Nanosecond
	reads, entered, release := helperBlockingCodeAssistStub(t, liveProbeOutcomeCodeAssistHTTPError)
	// A clock that ticks one millisecond per read: the two settles below run
	// back to back, and on a fast machine the real clock hands both the same
	// millisecond — the same debt generation — which is not the case this test
	// is about. It must still move, or the minimum interval would read every
	// later payment as "0 s since the last read". Atomic because the worker
	// reads it while the test advances it.
	var clockMs atomic.Int64
	clockMs.Store(now.UnixMilli())
	antigravityUsageFreshnessNow = func() time.Time { return time.UnixMilli(clockMs.Add(1)) }

	antigravityUsageRunSettled(now.Add(-time.Minute), false, true)
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the first run's payment never reached its read")
	}
	before := helperFreshnessState(t).debtID()

	// The second run finishes while that read is in flight, replacing the
	// pending generation with its own newer floor.
	second := now.Add(time.Second)
	clockMs.Store(second.UnixMilli())
	antigravityUsageRunSettled(second, false, true)
	if helperFreshnessState(t).debtID() == before {
		t.Fatal("the second run did not replace the pending debt")
	}
	release()
	antigravityUsageRefreshWaitIdle()

	state := helperFreshnessState(t)
	if state.RefreshOwedFloorMs < second.UnixMilli() {
		t.Fatalf("owed floor=%d, want the newer run's completion (>= %d)", state.RefreshOwedFloorMs, second.UnixMilli())
	}
	// The in-flight read belonged to the first generation, so every read after
	// it — and only those — is charged to the second run, which is tried at
	// least a full settle pass of its own.
	got := reads.Load()
	if got < int64(1+antigravityRefreshAfterRunMaxAttempts) {
		t.Errorf("reads=%d, want the first generation's one plus at least the second's own %d",
			got, antigravityRefreshAfterRunMaxAttempts)
	}
	if int64(state.Attempts) != got-1 {
		t.Errorf("attempts=%d after %d reads, want exactly the second run's own reads charged", state.Attempts, got)
	}
}

// A debt nothing could pay must age out rather than pin a worker or a warning
// forever.
func TestAntigravityFreshness_DebtPastMaxAgeRetires(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	now := time.Now()
	helperWriteAntigravityCache(t, cache, now.Add(-24*time.Hour))
	stale := now.Add(-antigravityRefreshOwedMaxAge - time.Minute)
	helperWriteJSON(t, antigravityFreshnessPath(), antigravityUsageFreshness{
		SchemaVersion:      antigravityFreshnessSchema,
		RefreshOwedFloorMs: stale.UnixMilli(),
		RefreshOwedAtMs:    stale.UnixMilli(),
		Attempts:           antigravityRefreshAfterRunMaxAttempts,
		Outcome:            liveProbeOutcomeCodeAssistHTTPError,
	})

	notice, pending := antigravityFreshnessNotice(now.Add(-24*time.Hour).Format(time.RFC3339), now)
	if pending || notice != "" {
		t.Errorf("notice=%q pending=%v, want an aged-out debt to warn about nothing", notice, pending)
	}

	calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })
	antigravityStartRunDebtWorker(antigravityRefreshAfterRunMaxAttempts, true)
	antigravityUsageRefreshWaitIdle()
	if calls.Load() != 0 {
		t.Errorf("reads=%d, want an aged-out debt to spend nothing", calls.Load())
	}
	if state := helperFreshnessState(t); state.RefreshOwedAtMs != 0 {
		t.Errorf("state=%+v, want the aged-out debt retired", state)
	}
}

// A floor further ahead of `now` than the local skew is a backwards clock step.
// Parked in the future it would make every later run owe a debt no reading can
// cover, so it is discarded.
func TestAntigravityFreshness_FutureFloorIsDiscardedAsAClockRollback(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	now := time.Now()
	helperWriteAntigravityCache(t, cache, now)
	future := now.Add(antigravityRunFloorLocalSkew + time.Hour)
	helperWriteJSON(t, antigravityFreshnessPath(), antigravityUsageFreshness{
		SchemaVersion:      antigravityFreshnessSchema,
		RunFloorMs:         future.UnixMilli(),
		RefreshOwedFloorMs: future.UnixMilli(),
		RefreshOwedAtMs:    future.UnixMilli(),
	})

	if _, pending := antigravityFreshnessNotice(now.Format(time.RFC3339), now); pending {
		t.Error("a rolled-back clock left a debt pending against a floor nothing can cover")
	}
	// Arming rewrites the floor at the current clock rather than keeping the
	// future one.
	floor := armAntigravityUsageRunFloor(now)
	antigravityUsageRefreshWaitIdle()
	if state := helperFreshnessState(t); state.RunFloorMs != floor.UnixMilli() {
		t.Errorf("runFloorMs=%d, want the rearmed %d", state.RunFloorMs, floor.UnixMilli())
	}
}

// A corrupt or truncated state file is "no debt", never a panic: this is a
// freshness optimisation and the next run rewrites it.
func TestAntigravityFreshness_CorruptStateFileReadsAsNoDebt(t *testing.T) {
	statePath, _ := helperIsolateAntigravityFreshness(t)
	if err := os.WriteFile(statePath, []byte(`{"refreshOwedAtMs":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if notice, pending := antigravityFreshnessNotice("", time.Now()); pending || notice != "" {
		t.Errorf("notice=%q pending=%v, want a corrupt file to read as no debt", notice, pending)
	}
	floor := armAntigravityUsageRunFloor(time.Now())
	antigravityUsageRefreshWaitIdle()
	if state := helperFreshnessState(t); state.RunFloorMs != floor.UnixMilli() {
		t.Errorf("runFloorMs=%d, want the corrupt file replaced by a real floor", state.RunFloorMs)
	}
}

// An offline agent makes no outbound request at all; the debt waits for the
// next run rather than retiring, because offline is temporary.
func TestAntigravityFreshness_OfflineKeepsTheDebtAndSpendsNothing(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	helperWriteAntigravityCache(t, cache, time.Now().Add(-time.Hour))
	calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistOK })

	offlineMutex.Lock()
	wasOffline := isOffline
	isOffline = true
	offlineMutex.Unlock()
	t.Cleanup(func() {
		offlineMutex.Lock()
		isOffline = wasOffline
		offlineMutex.Unlock()
	})

	antigravityUsageRunSettled(time.Now(), false, true)
	antigravityUsageRefreshWaitIdle()
	if calls.Load() != 0 {
		t.Errorf("reads=%d while offline, want none", calls.Load())
	}
	state := helperFreshnessState(t)
	if state.RefreshOwedAtMs == 0 || state.Attempts != 0 {
		t.Errorf("state=%+v, want the debt kept with no attempt consumed", state)
	}
	// Offline spends nothing, so it comes back on the free rung rather than
	// waiting for a run that may never settle.
	if until := time.Until(time.UnixMilli(state.NextAttemptAtMs)); until <= 0 || until > antigravityRunDebtFreeRetryDelay+time.Second {
		t.Fatalf("next attempt is %s away, want the free rung %s", until, antigravityRunDebtFreeRetryDelay)
	}
	// Back online; the rung fires and pays.
	offlineMutex.Lock()
	isOffline = false
	offlineMutex.Unlock()
	antigravityArmRunDebtRetry(state.debtID(), 0)
	helperDrainAntigravityRefreshScheduleOnce(t)
	if calls.Load() != 1 {
		t.Errorf("reads=%d once back online, want the kept debt paid", calls.Load())
	}
}

// An uninstall between the run and the payment retires the debt without an
// attempt: neither a retry nor a notice belongs to a provider the card no
// longer shows.
func TestAntigravityFreshness_UninstalledAgyRetiresTheDebt(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	helperWriteAntigravityCache(t, cache, time.Now().Add(-time.Hour))
	calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistOK })
	// An empty PATH and an installer dir that holds nothing: `agy` is gone.
	t.Setenv("PATH", t.TempDir())

	antigravityUsageRunSettled(time.Now(), false, true)
	antigravityUsageRefreshWaitIdle()
	if calls.Load() != 0 {
		t.Errorf("reads=%d for an uninstalled CLI, want none", calls.Load())
	}
	if state := helperFreshnessState(t); state.RefreshOwedAtMs != 0 {
		t.Errorf("state=%+v, want the debt retired", state)
	}
	if notice, pending := antigravityFreshnessNotice("", time.Now()); pending || notice != "" {
		t.Errorf("notice=%q pending=%v, want nothing said about a CLI that is gone", notice, pending)
	}
}

// The notice accessor is the single source for both questions the parser asks.
// Nothing is worded while attempts remain; once the budget is spent every
// cause is worded, gated or not — ParseContext decides whether the gate banner
// outranks it, and a gated debt that could never speak would leave the card
// silently stale once a Code Assist reading made the gate the older fact.
func TestAntigravityFreshnessNotice_WordsEachCauseOnceSpent(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	floor := now.Add(-5 * time.Minute)
	lastObserved := now.Add(-48 * time.Hour).Format(time.RFC3339)

	for _, tc := range []struct {
		name        string
		state       antigravityUsageFreshness
		wantPending bool
		wantNotice  []string
	}{
		{
			name:        "a gated debt with attempts left says nothing yet",
			state:       antigravityUsageFreshness{Gated: true, Attempts: antigravityRefreshDebtMaxAttempts - 1, Outcome: liveProbeOutcomeCodeAssistHTTPError},
			wantPending: true,
		},
		{
			name:        "a spent gated debt is worded too",
			state:       antigravityUsageFreshness{Gated: true, Attempts: antigravityRefreshDebtMaxAttempts, Outcome: liveProbeOutcomeCodeAssistHTTPError},
			wantPending: true,
			wantNotice:  []string{"Google returned no reading"},
		},
		{
			name:        "an expired login is worded before the budget is spent",
			state:       antigravityUsageFreshness{Attempts: 1, Outcome: liveProbeOutcomeCodeAssistTokenExpired},
			wantPending: true,
			wantNotice:  []string{"has expired", "next Antigravity run renews it", "2026-09-20 11:55 UTC"},
		},
		{
			name:        "a debt with attempts left says nothing yet",
			state:       antigravityUsageFreshness{Attempts: 0},
			wantPending: true,
		},
		{
			name:        "a spent no_login debt names the missing login",
			state:       antigravityUsageFreshness{Attempts: antigravityRefreshDebtMaxAttempts, Outcome: liveProbeOutcomeCodeAssistNoLogin},
			wantPending: true,
			wantNotice:  []string{"No Antigravity login is stored", "2026-09-18 12:00 UTC", "2026-09-20 11:55 UTC"},
		},
		{
			name:        "a spent failing debt names the stored login",
			state:       antigravityUsageFreshness{Attempts: antigravityRefreshDebtMaxAttempts, Outcome: liveProbeOutcomeCodeAssistHTTPError},
			wantPending: true,
			wantNotice:  []string{"Google returned no reading", "2026-09-20 11:55 UTC"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			helperIsolateAntigravityFreshness(t)
			state := tc.state
			state.SchemaVersion = antigravityFreshnessSchema
			state.RefreshOwedFloorMs = floor.UnixMilli()
			state.RefreshOwedAtMs = floor.UnixMilli()
			helperWriteJSON(t, antigravityFreshnessPath(), state)

			notice, pending := antigravityFreshnessNotice(lastObserved, now)
			if pending != tc.wantPending {
				t.Errorf("pending=%v, want %v", pending, tc.wantPending)
			}
			if len(tc.wantNotice) == 0 {
				if notice != "" {
					t.Errorf("notice=%q, want none", notice)
				}
				return
			}
			for _, want := range tc.wantNotice {
				if !strings.Contains(notice, want) {
					t.Errorf("notice=%q, want it to contain %q", notice, want)
				}
			}
			// Timestamps and fixed text only.
			for _, forbidden := range []string{"@", "http", "token", string(os.PathSeparator) + "Users"} {
				if strings.Contains(notice, forbidden) {
					t.Errorf("notice=%q leaked %q", notice, forbidden)
				}
			}
		})
	}
}

// A debt the previous process never settled is adopted and paid ONCE at
// startup, bypassing the interval: nothing in this fresh process has read yet.
func TestPayOwedAntigravityUsageRefresh_AdoptsAnUnsettledFloor(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	now := time.Now()
	helperWriteAntigravityCache(t, cache, now.Add(-time.Hour))
	helperWriteJSON(t, antigravityFreshnessPath(), antigravityUsageFreshness{
		SchemaVersion: antigravityFreshnessSchema,
		RunFloorMs:    now.Add(-time.Minute).UnixMilli(),
		// A read seconds ago would block a NEW debt; a startup adoption
		// bypasses the interval.
		LastPaidAtMs: now.Add(-time.Second).UnixMilli(),
	})
	calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })

	payOwedAntigravityUsageRefresh()
	antigravityUsageRefreshWaitIdle()

	if got := calls.Load(); got != 1 {
		t.Fatalf("reads=%d, want exactly one bounded attempt at startup", got)
	}
	state := helperFreshnessState(t)
	if state.RefreshOwedAtMs == 0 || state.RefreshOwedFloorMs != now.Add(-time.Minute).UnixMilli() {
		t.Errorf("state=%+v, want the interrupted run's floor adopted as a debt", state)
	}
}

// Startup must not resurrect a run older than the age-out, and must spend
// nothing when a reading already covers the adopted floor.
func TestPayOwedAntigravityUsageRefresh_SkipsWhatItCannotOrNeedNotPay(t *testing.T) {
	t.Run("a floor older than the age-out", func(t *testing.T) {
		_, cache := helperIsolateAntigravityFreshness(t)
		now := time.Now()
		helperWriteAntigravityCache(t, cache, now.Add(-24*time.Hour))
		helperWriteJSON(t, antigravityFreshnessPath(), antigravityUsageFreshness{
			SchemaVersion: antigravityFreshnessSchema,
			RunFloorMs:    now.Add(-antigravityRefreshOwedMaxAge - time.Minute).UnixMilli(),
		})
		calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistOK })

		payOwedAntigravityUsageRefresh()
		antigravityUsageRefreshWaitIdle()
		if calls.Load() != 0 {
			t.Errorf("reads=%d, want none for a run too old to matter", calls.Load())
		}
	})

	t.Run("a floor a run of THIS process armed", func(t *testing.T) {
		_, cache := helperIsolateAntigravityFreshness(t)
		now := time.Now()
		helperWriteAntigravityCache(t, cache, now.Add(-time.Hour))
		calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistOK })

		// The replay is spawned, so a session can arm between StartAgent asking
		// and the goroutine reading its state. Pin that ordering rather than
		// racing it: a floor stamped after this call's own instant is a run
		// this process armed. Converting it would book a completion time for a
		// run that is still going, and the reading it triggered — taken at the
		// run's START — would then satisfy the run's own settle, leaving the
		// finished run with no refresh at all.
		helperWriteJSON(t, antigravityFreshnessPath(), antigravityUsageFreshness{
			SchemaVersion: antigravityFreshnessSchema,
			RunFloorMs:    now.Add(500 * time.Millisecond).UnixMilli(),
		})
		payOwedAntigravityUsageRefresh()
		antigravityUsageRefreshWaitIdle()

		if calls.Load() != 0 {
			t.Errorf("reads=%d, want none for a run that is still running", calls.Load())
		}
		state := helperFreshnessState(t)
		if state.RefreshOwedAtMs != 0 {
			t.Errorf("state=%+v, want a live run's floor left to its own settle", state)
		}
		if state.RunFloorMs == 0 {
			t.Error("the live run's floor was dropped")
		}
	})

	t.Run("a floor a reading already covers", func(t *testing.T) {
		_, cache := helperIsolateAntigravityFreshness(t)
		now := time.Now()
		helperWriteAntigravityCache(t, cache, now)
		helperWriteJSON(t, antigravityFreshnessPath(), antigravityUsageFreshness{
			SchemaVersion: antigravityFreshnessSchema,
			RunFloorMs:    now.Add(-time.Minute).UnixMilli(),
		})
		calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistOK })

		payOwedAntigravityUsageRefresh()
		antigravityUsageRefreshWaitIdle()
		if calls.Load() != 0 {
			t.Errorf("reads=%d, want none when the run already has its reading", calls.Load())
		}
	})
}

// Every route that lands a reading is a settler: a Code Assist read, a Refresh
// click and a concurrent run's poller all go through
// writeAntigravityQuotaSnapshotLocked, so the debt is retired exactly once by
// whichever of them covers the floor — with no call back into the freshness
// worker.
func TestSettleAntigravityRunFreshness_AnyPersistedReadingRetiresTheDebt(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	floor := time.Now()
	helperWriteAntigravityCache(t, cache, floor.Add(-time.Hour))
	helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })
	antigravityUsageRunSettled(floor, false, true)
	antigravityUsageRefreshWaitIdle()
	if helperFreshnessState(t).RefreshOwedAtMs == 0 {
		t.Fatal("no debt to retire")
	}

	snap := antigravityQuotaSnapshot{
		ObservedAt:         floor.Add(time.Second).UTC().Format(time.RFC3339),
		AccountFingerprint: fingerprintAccount("antigravity", "ada@example.com"),
		Account:            "ada@example.com",
		Buckets: []antigravityQuotaBucket{
			{Group: "Gemini Models", Window: "weekly", RemainingFraction: 0.3, ResetTime: "2126-08-14T00:00:00Z"},
		},
	}
	if !saveAntigravityQuotaSnapshotIfNewer(snap) {
		t.Fatal("the reading was not persisted")
	}
	state := helperFreshnessState(t)
	if state.RefreshOwedAtMs != 0 || state.RunFloorMs != 0 {
		t.Errorf("state=%+v, want the landed reading to retire both the debt and the floor", state)
	}
}

// The redaction contract for the state file, asserted on the SERIALIZED bytes:
// timestamps, counters and a hashed fingerprint, never a credential, an
// account, a port, a path or a command.
func TestAntigravityFreshness_StateFileCarriesNothingIdentifying(t *testing.T) {
	statePath, cache := helperIsolateAntigravityFreshness(t)
	helperWriteAntigravityCache(t, cache, time.Now().Add(-time.Hour))
	helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })

	antigravityUsageRunSettled(time.Now(), false, true)
	antigravityUsageRefreshWaitIdle()

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	body := string(raw)
	// "http" alone would match the closed-set outcome code codeassist_http_error.
	for _, forbidden := range []string{"ada@example.com", "access_token", "Bearer", "http://", "https://", "agy", "127.0.0.1", statePath, cache} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the freshness state leaked %q:\n%s", forbidden, body)
		}
	}
	allowed := map[string]bool{
		"schemaVersion": true, "runFloorMs": true, "refreshOwedFloorMs": true,
		"refreshOwedAtMs": true, "lastPaidAtMs": true, "attempts": true,
		"gated": true, "accountFingerprint": true, "outcome": true,
		"nextAttemptAtMs": true,
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("state is not a JSON object: %v", err)
	}
	for key := range decoded {
		if !allowed[key] {
			t.Errorf("unexpected persisted field %q", key)
		}
	}
	// The outcome is one of the closed Code Assist codes, never free text.
	if outcome, _ := decoded["outcome"].(string); outcome != "" && !strings.HasPrefix(outcome, "codeassist_") {
		t.Errorf("outcome=%q, want a closed-set code", outcome)
	}
}

// probeAntigravityQuotaCodeAssistFn is the ONLY outbound call the worker makes,
// and it must be reachable with no detectedCLIAgent: the version the request
// identifies itself as comes from the same cached probe detection uses.
func TestAntigravityCodeAssistBuildVersion_FallsBackToTheInstalledBinary(t *testing.T) {
	helperIsolateAntigravityFreshness(t)
	// The one case that needs a REAL executable: helperFakeAgyOnPath's shell
	// script is enough for "is agy installed", but Windows cannot CreateProcess
	// a .cmd directly (the same reason grokProbeVersion exists), so the probe
	// would answer "" there and this assertion would pass only on Unix. The
	// mock binary is a genuine .exe on every platform.
	helperMockAgyOnPath(t, "antigravity-diagnostic")
	if got := antigravityCodeAssistBuildVersion("1.2.4"); got != "1.2.4" {
		t.Errorf("version=%q, want the detected one kept", got)
	}
	resetVersionProbeCache()
	if got := antigravityCodeAssistBuildVersion(""); !strings.Contains(got, "1.2.3") {
		t.Errorf("version=%q, want it probed off the installed binary", got)
	}
	t.Setenv("PATH", t.TempDir())
	if got := antigravityCodeAssistBuildVersion(""); got != "" {
		t.Errorf("version=%q, want empty when nothing is installed", got)
	}
	// And the User-Agent still names a build, so a licensed request is never
	// sent with Go's default header.
	if ua := antigravityCodeAssistUserAgent(""); !strings.HasPrefix(ua, "antigravity/cli/") {
		t.Errorf("user agent=%q", ua)
	}
}

// The state file is the SOLE crash-recovery record, so it is replaced
// atomically: a truncate-in-place the agent is killed or self-replaced in the
// middle of would leave invalid JSON, which reads back as "no debt" and loses
// the very floor this file exists to carry across an interruption. Asserts the
// bytes on disk after every rewrite — the observable the reader depends on —
// and that no intermediate file is left behind for the next start to trip over.
func TestAntigravityFreshness_StateFileIsReplacedAtomically(t *testing.T) {
	state, _ := helperIsolateAntigravityFreshness(t)
	now := time.Now()

	for i, owed := range []int64{
		now.Add(-5 * time.Minute).UnixMilli(),
		now.Add(-4 * time.Minute).UnixMilli(),
		now.Add(-3 * time.Minute).UnixMilli(),
	} {
		updateAntigravityUsageFreshness(func(s *antigravityUsageFreshness) {
			s.RefreshOwedFloorMs, s.RefreshOwedAtMs = owed, owed
			s.Attempts = i
		})
		// Every rewrite leaves a COMPLETE document behind, never a truncated
		// one: the reader treats a partial file as no debt at all.
		body, err := os.ReadFile(state)
		if err != nil {
			t.Fatalf("read state: %v", err)
		}
		var decoded antigravityUsageFreshness
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("state file is not valid JSON after rewrite %d: %v (%q)", i, err, body)
		}
		if decoded.RefreshOwedFloorMs != owed {
			t.Fatalf("owed floor=%d, want %d", decoded.RefreshOwedFloorMs, owed)
		}
	}

	entries, err := os.ReadDir(filepath.Dir(state))
	if err != nil {
		t.Fatalf("read state dir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp.") {
			t.Errorf("intermediate file left behind: %s", entry.Name())
		}
	}
}

// The capture hint says the poller reached a server, never that the run was
// covered: a snapshot persisted WHILE the run was going predates the turn's own
// debit, so the run still owes a refresh once the post-release tail has had its
// bounded chance to land the reading that covers it.
func TestAntigravityFreshness_ACaptureDuringTheRunIsNotCoverage(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	// The tail this run is waiting for never lands a reading; keep the wait
	// short so the case costs milliseconds rather than the shipped window.
	t.Setenv(antigravityCaptureTailEnv, "10ms")
	origGrace := antigravityPostRunReadingGrace
	t.Cleanup(func() { antigravityPostRunReadingGrace = origGrace })
	antigravityPostRunReadingGrace = 10 * time.Millisecond

	floor := time.Now().Truncate(time.Second)
	completed := floor.Add(time.Minute)
	antigravityUsageFreshnessNow = func() time.Time { return completed }
	// The newest reading is from the middle of the run, which is exactly what
	// the hint reports on an ungated build.
	helperWriteAntigravityCache(t, cache, floor.Add(30*time.Second))
	calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })

	antigravityUsageRunSettled(floor, true, false)
	antigravityUsageRefreshWaitIdle()

	state := helperFreshnessState(t)
	if state.RefreshOwedAtMs == 0 {
		t.Fatalf("state=%+v, want a mid-run reading to leave the run owing a refresh", state)
	}
	if state.RefreshOwedFloorMs != completed.UnixMilli() {
		t.Errorf("owed floor=%d, want the completion %d", state.RefreshOwedFloorMs, completed.UnixMilli())
	}
	if calls.Load() == 0 {
		t.Error("the unpaid run spent no Code Assist read")
	}
}

// The same hint, with the tail actually landing its reading: the run waits the
// bounded window, sees the post-completion snapshot, and owes nothing — so an
// ungated build still spends no outbound read per run.
func TestAntigravityFreshness_ThePostRunTailReadingCoversTheRun(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	t.Setenv(antigravityCaptureTailEnv, "2s")
	floor := time.Now().Truncate(time.Second)
	completed := floor.Add(time.Minute)
	antigravityUsageFreshnessNow = func() time.Time { return completed }
	helperWriteAntigravityCache(t, cache, floor.Add(30*time.Second))
	calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })

	// The tail's write, a moment after the run released.
	go func() {
		time.Sleep(20 * time.Millisecond)
		helperWriteAntigravityCache(t, cache, completed)
	}()
	antigravityUsageRunSettled(floor, true, false)
	antigravityUsageRefreshWaitIdle()

	if state := helperFreshnessState(t); state.RefreshOwedAtMs != 0 {
		t.Errorf("state=%+v, want the tail reading to leave nothing owed", state)
	}
	if calls.Load() != 0 {
		t.Errorf("a covered run spent %d Code Assist reads", calls.Load())
	}
}

// A reading that lands while a run is still going covers the persisted floor,
// but must not drop it: the floor is the only record a crash or self-update
// before that run's settle can recover from.
func TestAntigravityFreshness_ALiveRunKeepsItsCrashMarker(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })
	started := time.Now().Truncate(time.Second)
	antigravityUsageFreshnessNow = func() time.Time { return started }

	floor := armAntigravityUsageRunFloor(started)
	antigravityUsageRefreshWaitIdle()
	if state := helperFreshnessState(t); state.RunFloorMs != floor.UnixMilli() {
		t.Fatalf("state=%+v, want the armed run's floor %d", state, floor.UnixMilli())
	}

	// A Refresh click mid-run: newer than the floor, but taken before the run
	// spent the usage it is still spending.
	settleAntigravityRunFreshness(started.Add(10 * time.Second).UnixMilli())
	if state := helperFreshnessState(t); state.RunFloorMs != floor.UnixMilli() {
		t.Fatalf("state=%+v, want a live run to keep its floor for crash recovery", state)
	}

	// Once that run settles as covered, nothing is left for a restart to adopt.
	completed := started.Add(time.Minute)
	antigravityUsageFreshnessNow = func() time.Time { return completed }
	helperWriteAntigravityCache(t, cache, completed)
	antigravityUsageRunSettled(floor, false, true)
	antigravityUsageRefreshWaitIdle()
	if state := helperFreshnessState(t); state.RunFloorMs != 0 || state.RefreshOwedAtMs != 0 {
		t.Errorf("state=%+v, want a settled and covered run to leave nothing behind", state)
	}
}

// Two overlapping runs: a reading that covers the newer one rolls the marker
// back to the older run still in flight rather than dropping it.
func TestAntigravityFreshness_ACoveringReadingRollsTheMarkerBackToTheOldestLiveRun(t *testing.T) {
	helperIsolateAntigravityFreshness(t)
	started := time.Now().Truncate(time.Second)
	antigravityUsageFreshnessNow = func() time.Time { return started }

	older := armAntigravityUsageRunFloor(started)
	newer := armAntigravityUsageRunFloor(started.Add(30 * time.Second))
	antigravityUsageRefreshWaitIdle()
	t.Cleanup(func() {
		antigravityReleaseLiveRun(older.UnixMilli())
		antigravityReleaseLiveRun(newer.UnixMilli())
	})
	if state := helperFreshnessState(t); state.RunFloorMs != newer.UnixMilli() {
		t.Fatalf("state=%+v, want the newest arm's floor %d", state, newer.UnixMilli())
	}

	settleAntigravityRunFreshness(started.Add(time.Minute).UnixMilli())
	if state := helperFreshnessState(t); state.RunFloorMs != older.UnixMilli() {
		t.Errorf("floor=%d, want it rolled back to the oldest live run %d",
			helperFreshnessState(t).RunFloorMs, older.UnixMilli())
	}
}

// The uninstall check follows the ACTIVE catalog, so a deployment that points
// the provider at another command cannot have the card detect the CLI while the
// debt worker retires the debt as "no longer installed".
func TestAntigravityExecutablePath_FollowsTheActiveCatalogCommand(t *testing.T) {
	dir := t.TempDir()
	name := "agy-custom"
	if runtime.GOOS == "windows" {
		name += ".cmd"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write custom agy: %v", err)
	}
	// Only the custom command exists: a check still resolving the literal `agy`
	// would report the CLI uninstalled.
	t.Setenv("PATH", dir)
	t.Cleanup(func() { SetCLIAgentCatalog(nil) })

	if got := antigravityExecutablePath(); got != "" {
		t.Fatalf("antigravityExecutablePath()=%q with the default catalog, want it not found", got)
	}
	SetCLIAgentCatalog([]cliAgentCatalogEntry{
		{ID: "antigravity", DisplayName: "Antigravity", Command: "agy-custom", DetectionKeys: []string{"antigravity"}},
	})
	if got := antigravityExecutablePath(); got == "" {
		t.Error("antigravityExecutablePath()=\"\", want the catalog's command resolved")
	}
}

// NextAttemptAtMs round-trips through the state file and has its own rebase
// rule: the floors' 30 s skew ceiling would discard every rung past the first,
// so a 30-minute rung must survive it, while a value past any rung the ladder
// can book is a backwards clock step and resolves to "due now".
func TestAntigravityFreshness_NextAttemptRoundTripsAndRebases(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		next time.Time
		keep bool
	}{
		{"the first rung", now.Add(time.Minute), true},
		{"the longest rung", now.Add(30 * time.Minute), true},
		{"past the longest rung", now.Add(30*time.Minute + antigravityRunFloorLocalSkew + time.Minute), false},
		{"a clock stepped back hours", now.Add(5 * time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			helperIsolateAntigravityFreshness(t)
			helperOwedDebt(t, antigravityUsageFreshness{Attempts: 4, NextAttemptAtMs: tc.next.UnixMilli()})

			raw, err := os.ReadFile(antigravityFreshnessPath())
			if err != nil || !strings.Contains(string(raw), `"nextAttemptAtMs":`) {
				t.Fatalf("state file %q (err=%v), want nextAttemptAtMs persisted", raw, err)
			}
			state, owed := antigravityPendingDebt(now)
			if !owed || state.Attempts != 4 {
				t.Fatalf("state=%+v owed=%v, want the debt itself untouched", state, owed)
			}
			want := int64(0)
			if tc.keep {
				want = tc.next.UnixMilli()
			}
			if state.NextAttemptAtMs != want {
				t.Errorf("nextAttemptAtMs=%d, want %d", state.NextAttemptAtMs, want)
			}
		})
	}
}

// The age-out moved from 30 minutes to 6 hours: a debt just inside it is still
// owed — and still worth a read — while one just past it is retired.
func TestAntigravityFreshness_DebtAtTheSixHourEdge(t *testing.T) {
	now := time.Now()
	if antigravityRefreshOwedMaxAge != 6*time.Hour {
		t.Fatalf("age-out=%s, want 6h", antigravityRefreshOwedMaxAge)
	}
	for _, tc := range []struct {
		name  string
		owed  time.Time
		keeps bool
	}{
		{"inside", now.Add(-antigravityRefreshOwedMaxAge + time.Minute), true},
		{"past", now.Add(-antigravityRefreshOwedMaxAge - time.Minute), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cache := helperIsolateAntigravityFreshness(t)
			helperWriteAntigravityCache(t, cache, now.Add(-24*time.Hour))
			calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })
			antigravityRefreshMinInterval = time.Nanosecond
			helperOwedDebt(t, antigravityUsageFreshness{
				RefreshOwedFloorMs: tc.owed.UnixMilli(), RefreshOwedAtMs: tc.owed.UnixMilli(), Attempts: 1,
			})

			antigravityStartRunDebtWorker(1, false)
			antigravityUsageRefreshWaitIdle()
			state := helperFreshnessState(t)
			if tc.keeps {
				if state.RefreshOwedAtMs == 0 || calls.Load() != 1 {
					t.Errorf("state=%+v reads=%d, want a debt inside the window still paid", state, calls.Load())
				}
				return
			}
			if state.RefreshOwedAtMs != 0 || calls.Load() != 0 {
				t.Errorf("state=%+v reads=%d, want a debt past the window retired unpaid", state, calls.Load())
			}
		})
	}
}
