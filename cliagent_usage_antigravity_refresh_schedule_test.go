package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The retry ladder, its lifetime cap, the single process-wide timer and the
// gather's nudge (cliagent_usage_antigravity_refresh_schedule.go). No spawn, no
// poller, no network: the Code Assist read is stubbed and every clock that
// matters is pinned small.

// helperPinAntigravityRefreshSchedule shrinks the ladder, the free rung and the
// nudge cooldown for one test, restoring them only once nothing is in flight.
func helperPinAntigravityRefreshSchedule(t *testing.T, rung, free time.Duration) {
	t.Helper()
	origLadder, origFree, origCooldown := antigravityRunDebtRetryLadder, antigravityRunDebtFreeRetryDelay, antigravityRefreshNudgeCooldown
	t.Cleanup(func() {
		helperStopAntigravityRefreshSchedule()
		antigravityRunDebtRetryLadder, antigravityRunDebtFreeRetryDelay, antigravityRefreshNudgeCooldown = origLadder, origFree, origCooldown
	})
	antigravityRunDebtRetryLadder = []time.Duration{rung}
	antigravityRunDebtFreeRetryDelay = free
}

// helperDrainAntigravityRefreshSchedule lets every booked rung fire and every
// worker it starts finish, until nothing is scheduled or in flight.
func helperDrainAntigravityRefreshSchedule(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		antigravityUsageRefreshWaitIdle()
		if !antigravityRunDebtRetryPending() && antigravityFreshnessInFlight.Load() == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the refresh schedule never drained")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// helperOwedDebt writes a pending debt the previous pass left behind.
func helperOwedDebt(t *testing.T, state antigravityUsageFreshness) antigravityUsageFreshness {
	t.Helper()
	state.SchemaVersion = antigravityFreshnessSchema
	if state.RefreshOwedAtMs == 0 {
		now := time.Now()
		state.RefreshOwedFloorMs, state.RefreshOwedAtMs = now.Add(-time.Minute).UnixMilli(), now.Add(-time.Minute).UnixMilli()
	}
	helperWriteJSON(t, antigravityFreshnessPath(), state)
	return state
}

// helperWriteRunLogAt writes one CLI run log under home's modern install tree
// and backdates it to mtime.
func helperWriteRunLogAt(t *testing.T, home string, mtime time.Time) {
	t.Helper()
	base := filepath.Join(home, ".gemini", "antigravity-cli")
	helperWriteAntigravityLog(t, base, "run.log", "I0811 12:00:00.000000 42 main.go:1] started\n")
	if err := os.Chtimes(filepath.Join(antigravityLogDir(base), "run.log"), mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

func TestAntigravityRetryDelayForAttempt_Ladder(t *testing.T) {
	for _, tc := range []struct {
		attempts int
		want     time.Duration
		ok       bool
	}{
		// A deferral before any read comes back on the first rung.
		{0, time.Minute, true},
		{1, time.Minute, true},
		{2, 2 * time.Minute, true},
		{3, 8 * time.Minute, true},
		{4, 30 * time.Minute, true},
		// The lifetime budget is spent: nothing more is ever booked.
		{antigravityRefreshDebtMaxAttempts, 0, false},
		{antigravityRefreshDebtMaxAttempts + 3, 0, false},
	} {
		got, ok := antigravityRetryDelayForAttempt(tc.attempts)
		if got != tc.want || ok != tc.ok {
			t.Errorf("attempts=%d: delay=%s ok=%v, want %s %v", tc.attempts, got, ok, tc.want, tc.ok)
		}
	}
	// The first rung is the minimum interval on purpose: a shorter one would
	// fire, defer on the interval and reschedule itself for the remainder.
	if first, _ := antigravityRetryDelayForAttempt(1); first != antigravityRefreshMinInterval {
		t.Errorf("first rung=%s, want the minimum interval %s", first, antigravityRefreshMinInterval)
	}

	// Clamped at the last rung when the ladder is shorter than the budget.
	orig := antigravityRunDebtRetryLadder
	t.Cleanup(func() { antigravityRunDebtRetryLadder = orig })
	antigravityRunDebtRetryLadder = []time.Duration{time.Second, 2 * time.Second}
	if got, ok := antigravityRetryDelayForAttempt(4); !ok || got != 2*time.Second {
		t.Errorf("clamped rung=%s ok=%v, want the last rung", got, ok)
	}
}

// A kept debt books its rung on disk and exactly one timer however many
// settles ask at once, and the whole schedule then spends the debt's lifetime
// budget and no more.
func TestAntigravityScheduleRunDebtRetry_ConcurrentSettlesArmOneBoundedSchedule(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	helperWriteAntigravityCache(t, cache, time.Now().Add(-time.Hour))
	reads := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })
	antigravityRefreshMinInterval = time.Nanosecond
	state := helperOwedDebt(t, antigravityUsageFreshness{})

	// Shipped rungs first, so nothing fires while the settles race.
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			antigravityScheduleRunDebtRetry(state, time.Now(), false)
		}()
	}
	wg.Wait()
	if !antigravityRunDebtRetryPending() {
		t.Fatal("no retry was armed for a kept debt")
	}
	booked := helperFreshnessState(t).NextAttemptAtMs
	if until := time.Until(time.UnixMilli(booked)); until < 50*time.Second || until > time.Minute+time.Second {
		t.Errorf("nextAttemptAtMs is %s away, want the first rung (%s)", until, antigravityRunDebtRetryLadder[0])
	}
	if reads.Load() != 0 {
		t.Fatalf("reads=%d before any rung fired", reads.Load())
	}

	// Now let the schedule run on a short ladder: a single timer means one
	// attempt per firing, and the budget caps the total.
	helperPinAntigravityRefreshSchedule(t, 10*time.Millisecond, 10*time.Millisecond)
	antigravityScheduleRunDebtRetry(state, time.Now(), false)
	helperDrainAntigravityRefreshSchedule(t)
	if got := reads.Load(); got != antigravityRefreshDebtMaxAttempts {
		t.Errorf("reads=%d, want exactly the lifetime budget %d", got, antigravityRefreshDebtMaxAttempts)
	}
	final := helperFreshnessState(t)
	if final.RefreshOwedAtMs == 0 || final.Attempts != antigravityRefreshDebtMaxAttempts || final.NextAttemptAtMs != 0 {
		t.Errorf("state=%+v, want the spent debt kept for the notice with nothing booked", final)
	}
}

// A rung that fires for a debt another route has since paid — or that a newer
// run replaced — writes nothing and spends nothing.
func TestAntigravityRunDebtRetry_FiringForAPaidDebtDoesNothing(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	helperWriteAntigravityCache(t, cache, time.Now().Add(-time.Hour))
	reads := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })
	state := helperOwedDebt(t, antigravityUsageFreshness{})

	antigravityArmRunDebtRetry(antigravityDebtID{floorMs: 1, owedAtMs: 2}, 0)
	helperDrainAntigravityRefreshSchedule(t)
	if reads.Load() != 0 {
		t.Errorf("reads=%d for a rung booked against another generation", reads.Load())
	}

	helperWriteAntigravityCache(t, cache, time.Now())
	settleAntigravityRunFreshness(time.Now().UnixMilli())
	antigravityArmRunDebtRetry(state.debtID(), 0)
	helperDrainAntigravityRefreshSchedule(t)
	if reads.Load() != 0 {
		t.Errorf("reads=%d for a debt a landed reading already retired", reads.Load())
	}
}

// Shutdown stops the pending rung, and a stopped schedule never fires.
func TestStopAntigravityRunDebtRetry_CancelsThePendingRung(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	helperWriteAntigravityCache(t, cache, time.Now().Add(-time.Hour))
	reads := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })
	state := helperOwedDebt(t, antigravityUsageFreshness{})

	antigravityArmRunDebtRetry(state.debtID(), 20*time.Millisecond)
	stopAntigravityRunDebtRetry()
	if antigravityRunDebtRetryPending() {
		t.Fatal("a rung is still armed after the stop")
	}
	time.Sleep(60 * time.Millisecond)
	antigravityUsageRefreshWaitIdle()
	if reads.Load() != 0 {
		t.Errorf("reads=%d from a rung stopped before it fired", reads.Load())
	}
	// The schedule itself is on disk for the next process to re-arm.
	if helperFreshnessState(t).RefreshOwedAtMs == 0 {
		t.Error("stopping the timer dropped the debt")
	}
}

// The restart-loop guard: a persisted NextAttemptAtMs still in the future is
// honoured as booked — the remainder is armed and nothing is paid at once.
func TestPayOwedAntigravityUsageRefresh_FutureScheduleArmsTheRemainder(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	helperWriteAntigravityCache(t, cache, time.Now().Add(-time.Hour))
	reads := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })
	next := time.Now().Add(2 * time.Minute)
	helperOwedDebt(t, antigravityUsageFreshness{Attempts: 2, NextAttemptAtMs: next.UnixMilli()})

	// A restart loop: every "process" replays the same state file.
	for i := 0; i < 5; i++ {
		payOwedAntigravityUsageRefresh()
		antigravityUsageRefreshWaitIdle()
	}
	if got := reads.Load(); got != 0 {
		t.Errorf("reads=%d across restarts before the booked rung, want none", got)
	}
	if !antigravityRunDebtRetryPending() {
		t.Error("the booked rung was not re-armed")
	}
	if state := helperFreshnessState(t); state.NextAttemptAtMs != next.UnixMilli() || state.Attempts != 2 {
		t.Errorf("state=%+v, want the booked schedule and budget untouched", state)
	}
}

// The gather's nudge creates a debt floored at the newest run log when that
// log postdates the cached reading, and pays it through the ordinary worker.
func TestNudgeAntigravityUsageRefresh_CreatesADebtForAnUnseenRun(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	home := t.TempDir()
	now := time.Now()
	observed := now.Add(-time.Hour).Truncate(time.Second)
	helperWriteAntigravityCache(t, cache, observed)
	logAt := now.Add(-5 * time.Minute)
	helperWriteRunLogAt(t, home, logAt)
	reads := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })

	newest := antigravityNewestRunLog(filepath.Join(home, ".gemini", "antigravity-cli"))
	if !nudgeAntigravityUsageRefresh(now, observed.Format(time.RFC3339), newest) {
		t.Fatal("the nudge ignored a run log newer than the cached reading")
	}
	antigravityUsageRefreshWaitIdle()
	state := helperFreshnessState(t)
	if state.RefreshOwedAtMs == 0 || state.RefreshOwedFloorMs != newest.UnixMilli() {
		t.Fatalf("state=%+v, want a debt floored at the run log (%d)", state, newest.UnixMilli())
	}
	if state.RunFloorMs != 0 {
		t.Errorf("runFloorMs=%d, want the nudge to leave the run floor alone", state.RunFloorMs)
	}
	if reads.Load() != 1 {
		t.Errorf("reads=%d, want one attempt per nudge", reads.Load())
	}
	if state.NextAttemptAtMs == 0 {
		t.Error("the unpaid nudge debt was not put on the ladder")
	}
}

// Every case where the nudge must stay quiet: nothing behind, a log still being
// written, a Refresh click that just read, the per-process cooldown, a debt
// that already exists and is not due, and a run of this process still live.
func TestNudgeAntigravityUsageRefresh_Refusals(t *testing.T) {
	now := time.Now()
	observed := now.Add(-time.Hour).Truncate(time.Second)
	for _, tc := range []struct {
		name     string
		logAt    time.Time
		observed time.Time
		state    *antigravityUsageFreshness
		setup    func(t *testing.T)
	}{
		{name: "the cached reading already covers the run", logAt: now.Add(-5 * time.Minute), observed: now.Add(-time.Minute).Truncate(time.Second)},
		{name: "a log from the reading's own second", logAt: observed.Add(900 * time.Millisecond), observed: observed},
		{name: "a log still settling", logAt: now.Add(-10 * time.Second), observed: observed},
		{name: "no run log at all", observed: observed},
		{
			name: "a Refresh click just read", logAt: now.Add(-5 * time.Minute), observed: observed,
			state: &antigravityUsageFreshness{LastPaidAtMs: now.Add(-5 * time.Second).UnixMilli()},
		},
		{
			name: "a pending debt whose rung is not due", logAt: now.Add(-5 * time.Minute), observed: observed,
			state: &antigravityUsageFreshness{
				RefreshOwedFloorMs: now.Add(-10 * time.Minute).UnixMilli(), RefreshOwedAtMs: now.Add(-10 * time.Minute).UnixMilli(),
				Attempts: 1, NextAttemptAtMs: now.Add(time.Minute).UnixMilli(),
			},
		},
		{
			name: "a pending debt with its budget spent", logAt: now.Add(-5 * time.Minute), observed: observed,
			state: &antigravityUsageFreshness{
				RefreshOwedFloorMs: now.Add(-10 * time.Minute).UnixMilli(), RefreshOwedAtMs: now.Add(-10 * time.Minute).UnixMilli(),
				Attempts: antigravityRefreshDebtMaxAttempts,
			},
		},
		{
			name: "a run of this process is still live", logAt: now.Add(-5 * time.Minute), observed: observed,
			setup: func(t *testing.T) {
				antigravityRegisterLiveRun(now.Add(-time.Minute).UnixMilli())
				t.Cleanup(helperResetAntigravityLiveRuns)
			},
		},
		{
			name: "the nudge cooldown", logAt: now.Add(-5 * time.Minute), observed: observed,
			setup: func(t *testing.T) {
				antigravityRefreshNudge.mu.Lock()
				antigravityRefreshNudge.lastAt = now.Add(-time.Second)
				antigravityRefreshNudge.mu.Unlock()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cache := helperIsolateAntigravityFreshness(t)
			helperWriteAntigravityCache(t, cache, tc.observed)
			reads := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })
			home := t.TempDir()
			if !tc.logAt.IsZero() {
				helperWriteRunLogAt(t, home, tc.logAt)
			}
			var before antigravityUsageFreshness
			if tc.state != nil {
				before = *tc.state
				before.SchemaVersion = antigravityFreshnessSchema
				helperWriteJSON(t, antigravityFreshnessPath(), before)
			}
			if tc.setup != nil {
				tc.setup(t)
			}

			newest := antigravityNewestRunLog(filepath.Join(home, ".gemini", "antigravity-cli"))
			if nudgeAntigravityUsageRefresh(now, tc.observed.Format(time.RFC3339), newest) {
				t.Error("the nudge armed the worker")
			}
			antigravityUsageRefreshWaitIdle()
			if reads.Load() != 0 {
				t.Errorf("reads=%d, want none", reads.Load())
			}
			after := helperFreshnessState(t)
			before.SchemaVersion, after.SchemaVersion = 0, 0
			if after != before {
				t.Errorf("state=%+v, want it untouched (%+v)", after, before)
			}
		})
	}
}

// A pending debt whose booked rung has passed — a timer lost to a sleep, or a
// process that never re-armed it — is picked up by the next gather.
func TestNudgeAntigravityUsageRefresh_PaysADueDebt(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	now := time.Now()
	observed := now.Add(-time.Hour).Truncate(time.Second)
	helperWriteAntigravityCache(t, cache, observed)
	reads := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })
	helperOwedDebt(t, antigravityUsageFreshness{Attempts: 1, NextAttemptAtMs: now.Add(-time.Second).UnixMilli()})

	if !nudgeAntigravityUsageRefresh(now, observed.Format(time.RFC3339), time.Time{}) {
		t.Fatal("the nudge ignored a due debt")
	}
	antigravityUsageRefreshWaitIdle()
	if reads.Load() != 1 {
		t.Errorf("reads=%d, want the due attempt paid once", reads.Load())
	}
	if state := helperFreshnessState(t); state.Attempts != 2 || state.NextAttemptAtMs <= now.UnixMilli() {
		t.Errorf("state=%+v, want the attempt booked and the next rung scheduled", state)
	}
}

// Nothing ever observed is behind any run the CLI logged.
func TestNudgeAntigravityUsageRefresh_NeverObservedIsBehindAnyRun(t *testing.T) {
	helperIsolateAntigravityFreshness(t)
	reads := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistNoLogin })
	now := time.Now()
	if !nudgeAntigravityUsageRefresh(now, "", now.Add(-5*time.Minute)) {
		t.Fatal("the nudge ignored a run on a device that never observed a reading")
	}
	antigravityUsageRefreshWaitIdle()
	if reads.Load() != 1 {
		t.Errorf("reads=%d, want one attempt", reads.Load())
	}
	// no_login is terminal: nothing is booked, and the notice explains it.
	if state := helperFreshnessState(t); state.NextAttemptAtMs != 0 || antigravityRunDebtRetryPending() {
		t.Errorf("state=%+v, want a no_login debt left unscheduled", state)
	}
}
