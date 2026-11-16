package main

import (
	"testing"
	"time"
)

// The age trigger for a gated build (antigravityAgedReadingFloor): with no run
// to owe a refresh, every gather on `agy` 1.5.x used to replay one Code Assist
// reading until the next run, so the card and run routing aged it for hours
// (seen on a device whose reading sat at a morning's quota exhaustion all day).

// A gated reading older than the max age owes one read, paid by the ordinary
// worker under its own route.
func TestAntigravityAgedReading_OwesAndPaysOneRead(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	now := time.Now()
	observed := now.Add(-3 * time.Hour).Truncate(time.Second)
	helperWriteAntigravityCache(t, cache, observed)
	reads := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })

	floor := antigravityAgedReadingFloor(now, observed.UnixMilli(), antigravityOwedFloor{})
	if !floor.Aged || !floor.Proven || !floor.At.Equal(now) {
		t.Fatalf("floor=%+v, want an aged, proven floor at now", floor)
	}
	if !nudgeAntigravityUsageRefreshFloor(now, observed.UnixMilli(), floor) {
		t.Fatal("the nudge ignored a gated reading three hours old")
	}
	antigravityUsageRefreshWaitIdle()
	state := helperFreshnessState(t)
	if state.RefreshOwedAtMs == 0 || state.RefreshOwedFloorMs != now.UnixMilli() {
		t.Fatalf("state=%+v, want a debt floored at now", state)
	}
	if reads.Load() != 1 {
		t.Errorf("reads=%d, want one read", reads.Load())
	}
	d := &antigravityDebtRoute
	d.mu.Lock()
	route := d.route
	d.mu.Unlock()
	if route != antigravityDebtRouteAged {
		t.Errorf("route=%q, want %q", route, antigravityDebtRouteAged)
	}
}

// Nothing for a young reading, for a device that never had one, and a run
// floor the reading does not cover keeps priority over the age.
func TestAntigravityAgedReadingFloor_Boundaries(t *testing.T) {
	now := time.Now()
	young := now.Add(-antigravityGatedReadingMaxAge + time.Second).UnixMilli()
	old := now.Add(-antigravityGatedReadingMaxAge - time.Second).UnixMilli()

	if f := antigravityAgedReadingFloor(now, young, antigravityOwedFloor{}); !f.At.IsZero() {
		t.Errorf("young reading: floor=%+v, want none", f)
	}
	if f := antigravityAgedReadingFloor(now, 0, antigravityOwedFloor{}); !f.At.IsZero() {
		t.Errorf("never observed: floor=%+v, want none", f)
	}
	run := antigravityOwedFloor{At: now.Add(-2 * time.Minute)}
	if f := antigravityAgedReadingFloor(now, old, run); f != run {
		t.Errorf("run floor newer than the reading: floor=%+v, want the run's %+v", f, run)
	}
	covered := antigravityOwedFloor{At: time.UnixMilli(old).Add(-time.Minute)}
	if f := antigravityAgedReadingFloor(now, old, covered); !f.Aged {
		t.Errorf("run floor the reading covers: floor=%+v, want an aged floor", f)
	}
}

// A terminal debt (no login, an exhausted ladder, an expired login) is never
// replaced by an aged floor: that would retry it on every gather.
func TestAntigravityAgedReading_KeepsATerminalDebt(t *testing.T) {
	for _, outcome := range []string{liveProbeOutcomeCodeAssistNoLogin, liveProbeOutcomeCodeAssistTokenExpired} {
		t.Run(outcome, func(t *testing.T) {
			_, cache := helperIsolateAntigravityFreshness(t)
			now := time.Now()
			observed := now.Add(-3 * time.Hour).Truncate(time.Second)
			helperWriteAntigravityCache(t, cache, observed)
			owedAt := now.Add(-time.Hour)
			helperOwedDebt(t, antigravityUsageFreshness{
				RefreshOwedFloorMs: owedAt.UnixMilli(), RefreshOwedAtMs: owedAt.UnixMilli(),
				Attempts: antigravityRefreshDebtMaxAttempts, Outcome: outcome,
			})
			reads := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistOK })

			floor := antigravityAgedReadingFloor(now, observed.UnixMilli(), antigravityOwedFloor{})
			if nudgeAntigravityUsageRefreshFloor(now, observed.UnixMilli(), floor) {
				t.Fatal("an aged floor replaced a terminal debt")
			}
			antigravityUsageRefreshWaitIdle()
			state := helperFreshnessState(t)
			if state.Outcome != outcome || state.RefreshOwedFloorMs != owedAt.UnixMilli() {
				t.Errorf("state=%+v, want the terminal debt left alone", state)
			}
			if reads.Load() != 0 {
				t.Errorf("reads=%d, want none", reads.Load())
			}
		})
	}
}
