package main

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// The acceptance clause that observedAt survives an update. A self-update is a
// restart: nothing of the old process survives but its state files, so these
// cases write exactly what a "previous process" would have left and replay it
// through payOwedAntigravityUsageRefresh, as StartAgent does.

// A due schedule left behind pays ONE attempt that bypasses the interval, and
// when that attempt fails the debt goes back on the ladder instead of going
// quiet until the next run happens to settle.
func TestAntigravityRefreshRestart_DueScheduleIsPaidOnceThenLadder(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	now := time.Now()
	helperWriteAntigravityCache(t, cache, now.Add(-time.Hour))
	reads := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })
	helperOwedDebt(t, antigravityUsageFreshness{
		RunFloorMs: now.Add(-3 * time.Minute).UnixMilli(),
		Attempts:   2,
		// The previous process read seconds before it was replaced: a NEW
		// debt would be spaced by it, the replay of an old one is not.
		LastPaidAtMs:    now.Add(-5 * time.Second).UnixMilli(),
		NextAttemptAtMs: now.Add(-time.Second).UnixMilli(),
	})

	payOwedAntigravityUsageRefresh()
	antigravityUsageRefreshWaitIdle()

	if got := reads.Load(); got != 1 {
		t.Fatalf("reads=%d, want exactly the startup replay's one attempt", got)
	}
	state := helperFreshnessState(t)
	if state.RefreshOwedAtMs == 0 || state.Attempts != 3 {
		t.Fatalf("state=%+v, want the debt kept with the replay's attempt booked", state)
	}
	// Three attempts booked: the third rung.
	rung, _ := antigravityRetryDelayForAttempt(3)
	if until := time.Until(time.UnixMilli(state.NextAttemptAtMs)); until < rung-5*time.Second || until > rung+time.Second {
		t.Errorf("next attempt is %s away, want the ladder's %s", until, rung)
	}
	if !antigravityRunDebtRetryPending() {
		t.Error("the replay's failure left nothing scheduled")
	}
}

// The whole acceptance path across a restart on a gated build: the previous
// process armed a run and died, the next one pays it from the stored login,
// and the reading it lands is what the card serves — to this process and to
// the one after it.
func TestAntigravityRefreshRestart_ObservedAtSurvivesAnUpdate(t *testing.T) {
	helperStubAntigravityKeyring(t, map[string]any{
		"access_token": "access-A", "token_type": "Bearer", "refresh_token": "never-read",
		"expiry": time.Now().Add(30 * time.Minute).Format(time.RFC3339Nano),
	})
	quotaCalls, _ := helperCodeAssistServers(t,
		func(string) (int, string) { return http.StatusOK, antigravityCodeAssistFixture },
		func(string) (int, string) { return http.StatusOK, `{"sub":"123","email":"ada@example.com"}` })
	// After helperCodeAssistServers, which points the cache at its own dir.
	home, cache := helperIsolateAntigravityCapture(t, "20ms")
	helperIsolateAntigravityGate(t)
	noteAntigravityQuotaGate("", time.Now().Add(-time.Hour))
	helperSeedStaleAntigravityCache(t, cache)
	probeAntigravityQuotaCodeAssistFn = probeAntigravityQuotaCodeAssist
	stale, _ := time.Parse(time.RFC3339, helperStaleObservedAt)

	// Written by the process the update replaced: a floor, a debt it could not
	// pay before it exited, and a rung that came due while no agent was up.
	now := time.Now()
	helperOwedDebt(t, antigravityUsageFreshness{
		RunFloorMs:         now.Add(-4 * time.Minute).UnixMilli(),
		RefreshOwedFloorMs: now.Add(-3 * time.Minute).UnixMilli(),
		RefreshOwedAtMs:    now.Add(-3 * time.Minute).UnixMilli(),
		Attempts:           1,
		Gated:              true,
		Outcome:            liveProbeOutcomeCodeAssistHTTPError,
		NextAttemptAtMs:    now.Add(-10 * time.Second).UnixMilli(),
	})

	payOwedAntigravityUsageRefresh()
	snap := helperAwaitPaidRefresh(t, cache, stale)
	if atomic.LoadInt32(quotaCalls) != 1 {
		t.Errorf("Code Assist reads=%d, want one", atomic.LoadInt32(quotaCalls))
	}
	if antigravitySnapshotObservedMs(snap) < now.Add(-3*time.Minute).UnixMilli() {
		t.Fatalf("observedAt=%s does not cover the run the previous process owed", snap.ObservedAt)
	}
	if antigravityRunDebtRetryPending() {
		t.Error("a paid debt left a rung armed")
	}

	usage, observed := helperParsedObservedAt(t, home, time.Now())
	if !observed.After(stale) {
		t.Fatalf("observedAt=%s did not advance past the stale %s across the update", observed, stale)
	}
	for _, m := range usage.Metrics {
		if m.Unknown || m.Consumed == nil {
			t.Errorf("metric %+v is not numeric", m)
		}
	}

	// And the process after THAT one serves the same reading: nothing is owed,
	// so the replay spends nothing and the cache is not aged backwards.
	payOwedAntigravityUsageRefresh()
	antigravityUsageRefreshWaitIdle()
	if atomic.LoadInt32(quotaCalls) != 1 {
		t.Errorf("Code Assist reads=%d after a second restart, want none further", atomic.LoadInt32(quotaCalls))
	}
	if _, again := helperParsedObservedAt(t, home, time.Now()); !again.Equal(observed) {
		t.Errorf("observedAt=%s after a second restart, want the paid reading %s kept", again, observed)
	}
}
