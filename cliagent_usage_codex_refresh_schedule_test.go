package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_codex_refresh_schedule_test.go — the ladder, the single
   process timer, the age-out clamp and the gather's nudge
   (cliagent_usage_codex_refresh_schedule.go).
   ------------------------------------------------------------------------ */

// oweUnpaidDebt records a finished run whose telemetry is unobserved, with the
// counters the caller wants, and returns the state the schedule would be handed.
func (f codexFreshnessFixture) oweUnpaidDebt(t *testing.T, runStart, completedAt time.Time, scans, reads int) codexRunFreshnessState {
	t.Helper()
	if !codexRecordRunFreshness(f.fp, completedAt, func(snap *codexRateLimitSnapshot) {
		codexOweRunRefresh(snap, runStart, completedAt)
		snap.RefreshOwedAttempts, snap.RefreshLiveReads = scans, reads
	}) {
		t.Fatal("recording the debt failed")
	}
	// Evaluated at completedAt, not now: a test that deliberately seeds an
	// over-age debt still needs a valid generation id to drive the ladder with.
	state := codexRunFreshnessForAccount(f.fp, completedAt)
	if !state.owed {
		t.Fatalf("fixture debt must be owed: %+v", state)
	}
	return state
}

// bookedRung reads the rung the schedule persisted, or zero.
func (f codexFreshnessFixture) bookedRung(t *testing.T) time.Time {
	t.Helper()
	ms := f.snapshot(t).NextAttemptAtMs
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

/* ───────────────────────────── rung selection ──────────────────────────── */

// Each retry kind indexes the ladder by the counter of the PHASE the debt is in:
// a scan deferral by the scans spent, a failed live read by the reads spent.
// Without that split a debt in the live-read phase would ask for rung 4-of-4, be
// told the scan budget is spent and book nothing at all.
func TestCodexScheduleRunDebtRetry_RungPerPhaseCounter(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name        string
		scans, read int
		kind        codexRunDebtRetryKind
		// rung is an index into the ladder, resolved INSIDE the subtest: the
		// fixture pins the ladder small, and the table is built before it runs.
		rung int
	}{
		{"first scan deferral", 1, 0, codexRetryAfterScan, 0},
		{"second scan deferral", 2, 0, codexRetryAfterScan, 1},
		{"third scan deferral", 3, 0, codexRetryAfterScan, 2},
		{"first failed read", codexRefreshAfterRunMaxAttempts, 1, codexRetryAfterRead, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
			f.seedPreRunReading(t, now.Add(-time.Hour), now)
			state := f.oweUnpaidDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute), tc.scans, tc.read)
			want := codexRunDebtRetryLadder[tc.rung]

			at := time.Now()
			if !codexScheduleRunDebtRetry(f.fp, state, at, tc.kind) {
				t.Fatal("nothing booked")
			}
			got := f.bookedRung(t).Sub(at)
			if got < want-2*time.Millisecond || got > want+50*time.Millisecond {
				t.Fatalf("booked in %s, want ~%s", got, want)
			}
			stopCodexRunDebtRetry()
		})
	}
}

// A spent phase budget books nothing and clears the rung, so the notice can
// surface instead of the ladder walking forever.
func TestCodexScheduleRunDebtRetry_SpentBudgetBooksNothing(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	state := f.oweUnpaidDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute),
		codexRefreshAfterRunMaxAttempts, codexRefreshLiveReadMaxAttempts)

	if codexScheduleRunDebtRetry(f.fp, state, time.Now(), codexRetryAfterScan) {
		t.Fatal("a spent scan budget booked a rung")
	}
	if codexScheduleRunDebtRetry(f.fp, state, time.Now(), codexRetryAfterRead) {
		t.Fatal("a spent read budget booked a rung")
	}
	if got := f.bookedRung(t); !got.IsZero() {
		t.Fatalf("rung %s booked with both budgets spent", got)
	}
	if codexRunDebtRetryPending() {
		t.Fatal("no timer may be armed with both budgets spent")
	}
}

// A refusal that spent nothing (free) backs off with the debt's AGE — no budget
// bounds it, so the age is what keeps a permanently offline device from
// re-checking at the floor for six hours — and caps at the longest rung.
func TestCodexScheduleRunDebtRetry_FreeRungBacksOffWithAgeAndCaps(t *testing.T) {
	longest := codexRunDebtRetryLadder[len(codexRunDebtRetryLadder)-1]
	for _, tc := range []struct {
		name    string
		owedFor time.Duration
		want    time.Duration
	}{
		{"young debt takes the floor", 0, codexRunDebtFreeRetryDelay},
		{"older debt backs off", codexRunDebtFreeRetryDelay * 3, codexRunDebtFreeRetryDelay * 3},
		{"very old debt caps", longest * 10, longest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexFreeRetryDelay(tc.owedFor); got != tc.want {
				t.Fatalf("free rung for a debt owed %s = %s, want %s", tc.owedFor, got, tc.want)
			}
		})
	}
}

// Every rung the ladder can pick — plus the free rung and the minimum interval —
// must read as an ordinary retry, never as a backwards clock step. Judging a rung
// against codexRunFloorLocalSkew (30 s) instead would fire every rung past a
// minute immediately and collapse the ladder.
func TestCodexRunDebtRetryHorizon_AcceptsEveryConfiguredRung(t *testing.T) {
	prev := codexRunDebtRetryLadder
	codexRunDebtRetryLadder = []time.Duration{15 * time.Second, time.Minute, 4 * time.Minute, 15 * time.Minute}
	t.Cleanup(func() { codexRunDebtRetryLadder = prev })

	horizon := codexRunDebtRetryHorizon()
	for _, rung := range append(append([]time.Duration{}, codexRunDebtRetryLadder...),
		codexRunDebtFreeRetryDelay, codexForcedReconcileMinInterval) {
		if rung > horizon {
			t.Fatalf("rung %s is beyond the horizon %s and would be mistaken for a clock rollback", rung, horizon)
		}
	}
	// Only something the schedule could never have booked is a rollback.
	if horizon >= 24*time.Hour {
		t.Fatalf("horizon %s is so wide a real rollback would never be caught", horizon)
	}
}

/* ───────────────────────── age-out clamp + retirement ──────────────────── */

// No rung is ever booked past the age-out, and none lands ON it: the view keeps a
// debt owed while now-owedAt <= maxAge, so a rung landing exactly at the deadline
// would fire, find the debt still owed, spend a free attempt and re-book the same
// already-due instant — an immediate-callback loop.
func TestCodexScheduleRunDebtRetry_ClampsToTheFirstExpiredInstant(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-9*time.Hour))
	// AFTER the fixture, which pins the ladder small for every other test: here
	// the rung has to be long enough to overshoot the age-out deadline.
	prev := codexRunDebtRetryLadder
	codexRunDebtRetryLadder = []time.Duration{time.Hour, time.Hour, time.Hour, time.Hour}
	t.Cleanup(func() { codexRunDebtRetryLadder = prev })

	// Owed so long ago that a one-hour rung would land past the deadline.
	owedAt := now.Add(-codexRefreshOwedMaxAge + time.Minute)
	runStart := owedAt.Add(-time.Minute)
	// Observed BEFORE the run, so the debt is genuinely unpaid.
	f.seedPreRunReading(t, runStart.Add(-time.Hour), now)
	state := f.oweUnpaidDebt(t, runStart, owedAt, 1, 0)

	for _, kind := range []codexRunDebtRetryKind{codexRetryAfterScan, codexRetryFree} {
		if !codexScheduleRunDebtRetry(f.fp, state, now, kind) {
			t.Fatalf("kind %v booked nothing", kind)
		}
		want := codexRunDebtExpiryInstant(owedAt.UnixMilli())
		if got := f.bookedRung(t); !got.Equal(want.Truncate(time.Millisecond)) {
			t.Fatalf("kind %v booked %s, want the clamp at %s", kind, got, want)
		}
		// And the clamped instant really is expired, so the rung that fires there
		// retires the debt rather than rescheduling itself.
		if !codexRunDebtExpired(owedAt.UnixMilli(), want) {
			t.Fatalf("the clamped instant %s is not yet expired", want)
		}
	}
	stopCodexRunDebtRetry()
}

// Firing at the clamped instant retires the debt once, leaves the bounded expiry
// marker and books no replacement — the boundary loop the clamp exists to
// prevent, asserted with the clock pinned exactly there.
func TestCodexRunDebtRetry_FiringAtTheDeadlineRetiresOnce(t *testing.T) {
	real := time.Now()
	f := newCodexFreshnessFixture(t, real.Add(-9*time.Hour))
	runStart := real.Add(-codexRefreshOwedMaxAge - time.Hour)
	owedAt := runStart.Add(time.Minute)
	f.seedPreRunReading(t, runStart.Add(-time.Hour), real)
	state := f.oweUnpaidDebt(t, runStart, owedAt, 1, 0)

	// Pin the freshness clock exactly at the first expired instant.
	pinned := codexRunDebtExpiryInstant(owedAt.UnixMilli())
	prevNow := codexUsageFreshnessNow
	codexUsageFreshnessNow = func() time.Time { return pinned }
	t.Cleanup(func() { codexUsageFreshnessNow = prevNow })

	codexRunDebtRetryFired(retryTimerGeneration(), state.debtID(), f.fp)
	drainCodexRunDebtLadder(t)

	snap := f.snapshot(t)
	if snap.RefreshOwedAtMs != 0 || snap.NextAttemptAtMs != 0 {
		t.Fatalf("the deadline firing must retire the debt and book nothing: %+v", snap)
	}
	if snap.StaleRunNoticeFloorMs != runStart.UnixMilli() || snap.StaleRunNoticeAtMs != pinned.UnixMilli() {
		t.Fatalf("retirement must leave the bounded marker: %+v", snap)
	}
	if codexStaleRunNotice(codexRunFreshnessForAccount(f.fp, pinned)) == "" {
		t.Fatal("the marker must warn instead of the card going quiet")
	}
}

// retryTimerGeneration is the generation a callback must carry to be accepted.
// Arming a zero-delay rung and letting it be replaced is how production gets one;
// a test drives the callback directly, so it reads the counter.
func retryTimerGeneration() uint64 {
	t := &codexRunDebtRetryTimer
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.gen
}

// A continuously offline debt crossing the age-out books only FREE rungs, spends
// no live-read budget, and the firing rung that first sees it over-age retires it
// and leaves the marker — so the card warns rather than going quiet. Driven
// through the real timer, never by calling codexSettleRunFreshness.
func TestCodexRunDebtRetry_OfflineDebtRetiresWithAMarker(t *testing.T) {
	real := time.Now()
	f := newCodexFreshnessFixture(t, real.Add(-9*time.Hour))
	setCodexTestOffline(t, true)
	calls := stubCodexFallbackRead(t, capturingFallbackRead)
	runStart := real.Add(-2 * time.Minute)
	state := f.oweUnpaidDebt(t, runStart, real.Add(-time.Minute), codexRefreshAfterRunMaxAttempts, 0)
	f.seedPreRunReading(t, runStart.Add(-time.Hour), real)

	// Free rungs while the device is offline: no read leaves, none is charged.
	if kind := codexLiveUsageFallback(f.fp, state); kind != codexRetryFree {
		t.Fatalf("an offline fallback booked %v, want free", kind)
	}
	if !codexScheduleRunDebtRetry(f.fp, state, real, codexRetryFree) {
		t.Fatal("a free rung must be booked while the debt is inside its age-out")
	}
	if got := f.snapshot(t).RefreshLiveReads; got != 0 || *calls != 0 {
		t.Fatalf("an offline ladder charged %d reads / %d calls", got, *calls)
	}

	// Now the debt is over-age: the rung that fires retires it.
	pinned := real.Add(codexRefreshOwedMaxAge + time.Hour)
	prevNow := codexUsageFreshnessNow
	codexUsageFreshnessNow = func() time.Time { return pinned }
	t.Cleanup(func() { codexUsageFreshnessNow = prevNow })

	codexRunDebtRetryFired(retryTimerGeneration(), state.debtID(), f.fp)
	drainCodexRunDebtLadder(t)

	snap := f.snapshot(t)
	if snap.RefreshOwedAtMs != 0 {
		t.Fatalf("an over-age debt must be retired by the firing rung: %+v", snap)
	}
	if snap.StaleRunNoticeFloorMs == 0 || snap.StaleRunNoticeAtMs == 0 {
		t.Fatalf("retirement must leave the marker rather than vanishing: %+v", snap)
	}
}

// The gather is a second path that can walk away from an over-age debt, and it
// must retire it too.
func TestCodexReconcileForGather_RetiresAnOverAgeDebt(t *testing.T) {
	real := time.Now()
	f := newCodexFreshnessFixture(t, real.Add(-9*time.Hour))
	runStart := real.Add(-codexRefreshOwedMaxAge - 2*time.Hour)
	owedAt := runStart.Add(time.Minute)
	f.seedPreRunReading(t, runStart.Add(-time.Hour), real)
	f.oweUnpaidDebt(t, runStart, owedAt, 1, 0)

	ctx, cancel := context.WithTimeout(context.Background(), codexForcedReconcileBudget)
	defer cancel()
	codexReconcileForGather(ctx, f.home, f.fp, real, false)

	snap := f.snapshot(t)
	if snap.RefreshOwedAtMs != 0 || snap.StaleRunNoticeFloorMs != runStart.UnixMilli() {
		t.Fatalf("the gather must retire an over-age debt with its marker: %+v", snap)
	}
}

// A marker left in the FUTURE by a clock rollback is cleared, not pulled back: no
// present observation could ever clear it, so it would be a permanent warning.
func TestCodexRebaseFutureRunFreshness_ClearsAFutureExpiryMarker(t *testing.T) {
	now := time.Now()
	for _, field := range []string{"floor", "at"} {
		t.Run(field, func(t *testing.T) {
			f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
			f.seedPreRunReading(t, now.Add(-time.Hour), now)
			ahead := now.Add(codexRunFloorLocalSkew + time.Hour).UnixMilli()
			codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
				snap.StaleRunNoticeFloorMs, snap.StaleRunNoticeAtMs = now.UnixMilli(), now.UnixMilli()
				if field == "floor" {
					snap.StaleRunNoticeFloorMs = ahead
				} else {
					snap.StaleRunNoticeAtMs = ahead
				}
			})

			codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
				codexRebaseFutureRunFreshness(snap, now, now)
			})

			if snap := f.snapshot(t); snap.StaleRunNoticeFloorMs != 0 || snap.StaleRunNoticeAtMs != 0 {
				t.Fatalf("a future-dated marker must be dropped, not kept: %+v", snap)
			}
		})
	}
}

/* ─────────────────────────── timer discipline ─────────────────────────── */

// Concurrent settles replace ONE timer rather than multiplying attempts.
func TestCodexArmRunDebtRetry_SingleTimerAcrossConcurrentSettles(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	state := f.oweUnpaidDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute), 1, 0)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codexScheduleRunDebtRetry(f.fp, state, time.Now(), codexRetryAfterScan)
		}()
	}
	wg.Wait()
	drainCodexRunDebtLadder(t)

	// Every booking replaced the previous timer, so the debt cannot have spent
	// more than its bounded scan budget however many settles raced.
	if got := f.snapshot(t).RefreshOwedAttempts; got > codexRefreshAfterRunMaxAttempts {
		t.Fatalf("racing settles spent %d scans, want at most %d", got, codexRefreshAfterRunMaxAttempts)
	}
}

// A rung for a superseded generation, or for another account, writes nothing.
func TestCodexRunDebtRetryFired_IgnoresAStaleGenerationOrAccount(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	f.oweUnpaidDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute), 1, 0)
	before, err := os.ReadFile(f.cache)
	if err != nil {
		t.Fatal(err)
	}

	// A generation that never existed, and a live generation under a foreign
	// fingerprint.
	codexRunDebtRetryFired(retryTimerGeneration(), codexDebtID{floorMs: 1, owedAtMs: 2}, f.fp)
	codexRunDebtRetryFired(retryTimerGeneration(), codexRunFreshnessForAccount(f.fp, now).debtID(), "not-this-account")
	drainCodexRunDebtLadder(t)

	after, err := os.ReadFile(f.cache)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a rung for another generation or account must write nothing")
	}
}

// A rung armed minutes out cannot fire after the gate is reset: the reset stops
// the timer itself, because in-flight accounting only covers a callback that has
// ALREADY fired and waitIdle would otherwise return while the rung waits.
func TestResetCodexUsageRefreshGate_StopsAFutureRung(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	state := f.oweUnpaidDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute), 1, 0)
	codexArmRunDebtRetry(state.debtID(), f.fp, 30*time.Millisecond)
	if !codexRunDebtRetryPending() {
		t.Fatal("the rung was not armed")
	}
	before, err := os.ReadFile(f.cache)
	if err != nil {
		t.Fatal(err)
	}

	resetCodexUsageRefreshGate()
	if codexRunDebtRetryPending() {
		t.Fatal("the reset must cancel the pending rung")
	}
	time.Sleep(100 * time.Millisecond)

	after, err := os.ReadFile(f.cache)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a rung cancelled by the reset still wrote to the cache")
	}
}

/* ────────────────────────────── the nudge ─────────────────────────────── */

// With a debt pending the nudge arms the worker ONLY on a due rung; its own timer
// owns every other one.
func TestNudgeCodexUsageRefresh_ArmsOnlyOnADueRung(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	state := f.oweUnpaidDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute), 1, 0)
	rollout := now.Add(-time.Minute)

	// Rung booked well ahead: the nudge leaves it to the timer.
	codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		snap.NextAttemptAtMs = now.Add(time.Hour).UnixMilli()
	})
	if nudgeCodexUsageRefresh(f.home, f.fp, now, codexRolloutNudgeEvidence{newest: rollout}, state.latest) {
		t.Fatal("a rung in the future must be left to its own timer")
	}

	// Rung due: the nudge arms the worker.
	resetCodexRefreshNudge()
	codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		snap.NextAttemptAtMs = now.Add(-time.Second).UnixMilli()
	})
	if !nudgeCodexUsageRefresh(f.home, f.fp, now, codexRolloutNudgeEvidence{newest: rollout}, state.latest) {
		t.Fatal("a due rung must arm the worker")
	}
	drainCodexRunDebtLadder(t)
}

// With NO debt pending the nudge creates one floored at the rollout mtime it was
// handed — the run the user started in their own shell, which no spawn path
// classified. Arming the worker alone would be a no-op: it retires on
// owed == false.
func TestNudgeCodexUsageRefresh_CreatesADebtForAnUnmanagedRun(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	observed := now.Add(-time.Hour)
	f.seedPreRunReading(t, observed, now)
	rollout := now.Add(-2 * codexForcedReconcileMinInterval).Truncate(time.Millisecond)

	if !nudgeCodexUsageRefresh(f.home, f.fp, now, codexRolloutNudgeEvidence{newest: rollout}, observed) {
		t.Fatal("telemetry newer than the reading with no debt must create one")
	}
	drainCodexRunDebtLadder(t)

	snap := f.snapshot(t)
	if snap.RunFloorMs != rollout.UnixMilli() {
		t.Fatalf("created debt floored at %d, want the rollout mtime %d", snap.RunFloorMs, rollout.UnixMilli())
	}
}

// Every refusal the nudge owes: a zero mtime (the reconcile did not complete, or
// nothing was account-eligible), a rollout no newer than the reading, one younger
// than the minimum interval (still being written), a live local run, and its own
// cooldown.
func TestNudgeCodexUsageRefresh_Refusals(t *testing.T) {
	now := time.Now()
	observed := now.Add(-time.Hour)
	settled := now.Add(-2 * codexForcedReconcileMinInterval)

	t.Run("zero mtime", func(t *testing.T) {
		f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
		f.seedPreRunReading(t, observed, now)
		if nudgeCodexUsageRefresh(f.home, f.fp, now, codexRolloutNudgeEvidence{newest: time.Time{}}, observed) {
			t.Fatal("a pass that reported no eligible mtime must nudge nothing")
		}
	})

	t.Run("rollout older than the reading", func(t *testing.T) {
		f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
		f.seedPreRunReading(t, observed, now)
		if nudgeCodexUsageRefresh(f.home, f.fp, now, codexRolloutNudgeEvidence{newest: observed.Add(-time.Minute)}, observed) {
			t.Fatal("telemetry the reading already covers is not a missed run")
		}
	})

	t.Run("rollout still being written", func(t *testing.T) {
		f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
		f.seedPreRunReading(t, observed, now)
		if nudgeCodexUsageRefresh(f.home, f.fp, now, codexRolloutNudgeEvidence{newest: now}, observed) {
			t.Fatal("a rollout younger than the minimum interval may still be appending")
		}
	})

	t.Run("cooldown", func(t *testing.T) {
		f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
		f.seedPreRunReading(t, observed, now)
		prev := codexRefreshNudgeCooldown
		codexRefreshNudgeCooldown = time.Hour
		t.Cleanup(func() { codexRefreshNudgeCooldown = prev })
		if !nudgeCodexUsageRefresh(f.home, f.fp, now, codexRolloutNudgeEvidence{newest: settled}, observed) {
			t.Fatal("the first nudge must go through")
		}
		drainCodexRunDebtLadder(t)
		if nudgeCodexUsageRefresh(f.home, f.fp, now, codexRolloutNudgeEvidence{newest: settled}, observed) {
			t.Fatal("a second nudge inside the cooldown must be refused")
		}
	})

	t.Run("live local run", func(t *testing.T) {
		f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
		f.seedPreRunReading(t, observed, now)
		// A run this process armed is still open: it settles itself.
		armCodexUsageRunFloor(time.Now())
		drainCodexRunDebtLadder(t)
		if nudgeCodexUsageRefresh(f.home, f.fp, now, codexRolloutNudgeEvidence{newest: settled}, observed) {
			t.Fatal("a run of this process must settle itself, not be nudged")
		}
	})
}

// A rollout an OLD account's session is still appending to has the newest mtime on
// disk, but its session start predates auth.json: the scan reports it ineligible,
// the nudge is handed a zero mtime, and no debt the new account can never pay is
// invented.
func TestCodexReconcileFromRollout_OldAccountRolloutIsNotEligible(t *testing.T) {
	now := time.Now()
	loginAt := now.Add(-time.Minute)
	f := newCodexFreshnessFixture(t, loginAt)
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	// Session started BEFORE the login, still being appended to (newest mtime).
	writeCodexRunRollout(t, f.home, "old-account", loginAt.Add(-time.Hour), now.Add(-30*time.Second), now.Add(-time.Second), true,
		[]map[string]any{codexRateLimitFrame(80, 90, now)})

	_, _, _, rollouts := codexReconcileFromRollout(context.Background(), f.home, f.fp, now)

	if !rollouts.newest.IsZero() {
		t.Fatalf("an old-account rollout reported mtime %s; the nudge would floor a debt the new account cannot pay", rollouts.newest)
	}
}

// A completed pass reports the newest mtime among the candidates that cleared the
// login guard AND still owe telemetry — that is what the nudge floors a created
// debt at — while a candidate whose telemetry the pass mined is reported as
// COVERED instead. A rollout stamps its own observation before the write that
// advances its mtime (and trailing records widen the gap), so reporting a mined
// file as owing a refresh would floor a debt no re-read could ever pay.
func TestCodexReconcileFromRollout_ReportsTheNewestEligibleMtime(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	older := now.Add(-5 * time.Minute).Truncate(time.Second)
	newer := now.Add(-2 * time.Minute).Truncate(time.Second)
	// Telemetry landed in this one, stamped before its mtime.
	writeCodexRunRollout(t, f.home, "mined", now.Add(-10*time.Minute), older.Add(-time.Second), older, true,
		[]map[string]any{codexRateLimitFrame(20, 30, older.Add(-time.Second))})
	// Written but silent: the run the nudge exists to notice.
	writeCodexRunRollout(t, f.home, "silent", now.Add(-8*time.Minute), newer, newer, true, nil)

	_, _, _, rollouts := codexReconcileFromRollout(context.Background(), f.home, f.fp, now)

	if rollouts.newest.Truncate(time.Second) != newer {
		t.Fatalf("eligible mtime = %s, want the newest guard-passing candidate still owing telemetry %s", rollouts.newest, newer)
	}
	if got := codexCoveredMtimes(rollouts); len(got) != 1 || got[0].Truncate(time.Second) != older {
		t.Fatalf("covered mtimes = %v, want only the mined candidate %s", got, older)
	}
	if rollouts.newestEntry == "" || rollouts.covers(rollouts.newestEntry) {
		t.Fatalf("the uncovered rollout must be reported with its own identity, not a covered one (entry=%q)", rollouts.newestEntry)
	}
}

// The reported case: a single rollout that DOES carry telemetry, whose embedded
// observation necessarily predates the write that set its mtime. Judged on mtime
// alone the freshly mined reading looks behind its own rollout, so the nudge would
// create a debt floored at that mtime — one no re-read of the file could ever pay,
// burning the whole scan ladder plus the live reads on an ordinary unmanaged run
// and ending in a stale warning that is not true.
func TestCodexReconcileFromRollout_MinedTelemetryCoversItsOwnRollout(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	mtime := now.Add(-2 * time.Minute).Truncate(time.Second)
	// The ordinary gap: the frame is stamped, then the same turn's trailing
	// records land moments later and advance the mtime past it.
	observedAt := mtime.Add(-2 * time.Second)
	writeCodexRunRollout(t, f.home, "mined", now.Add(-10*time.Minute), observedAt, mtime, true,
		[]map[string]any{codexRateLimitFrame(20, 30, observedAt)})

	_, _, latest, rollouts := codexReconcileFromRollout(context.Background(), f.home, f.fp, now)

	if !rollouts.newest.IsZero() {
		t.Fatalf("a rollout whose telemetry was mined reported mtime %s as still owing a refresh", rollouts.newest)
	}
	if got := codexCoveredMtimes(rollouts); len(got) != 1 || got[0].Truncate(time.Second) != mtime {
		t.Fatalf("covered mtimes = %v, want %s", got, mtime)
	}
	if latest.Before(observedAt) {
		t.Fatalf("the pass did not mine the rollout's telemetry (latest = %s)", latest)
	}
	// End to end: the nudge is handed exactly this, and must not owe anything.
	if nudgeCodexUsageRefresh(f.home, f.fp, now, rollouts, latest) {
		t.Fatal("the reading the pass just mined FROM the rollout covers it; no debt is owed")
	}
	if snap := f.snapshot(t); snap.RefreshOwedAtMs != 0 || snap.PendingRolloutMtimeMs != 0 {
		t.Fatalf("a covered rollout left state behind: owed=%d pending=%d", snap.RefreshOwedAtMs, snap.PendingRolloutMtimeMs)
	}
}

// Coverage forgives the ordinary stamp/mtime gap, not an arbitrary one. A
// multi-turn rollout whose EARLIER turn stated telemetry and whose later turn
// appended only completion records holds a usable window — so the pass mines one
// — while its mtime has moved well past it. Granting coverage on "the file
// yielded a frame" would suppress the debt for the later turn, whose utilization
// really is missing, and the card would sit stale with the live-read fallback
// never reached.
func TestCodexReconcileFromRollout_StaleFrameDoesNotCoverALaterAppend(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	mtime := now.Add(-2 * time.Minute).Truncate(time.Second)
	// The earlier turn's frame, then a later turn that emitted no telemetry.
	observedAt := mtime.Add(-10 * time.Minute)
	writeCodexRunRollout(t, f.home, "multiturn", now.Add(-30*time.Minute), observedAt, mtime, true,
		[]map[string]any{codexRateLimitFrame(20, 30, observedAt)})

	_, _, latest, rollouts := codexReconcileFromRollout(context.Background(), f.home, f.fp, now)

	if latest.Before(observedAt) {
		t.Fatalf("the pass did not mine the earlier turn's telemetry (latest = %s)", latest)
	}
	if rollouts.newest.Truncate(time.Second) != mtime {
		t.Fatalf("eligible mtime = %s, want the later append %s still owing telemetry", rollouts.newest, mtime)
	}
	if len(rollouts.covered) != 0 {
		t.Fatalf("a frame %s older than the file's newest write must not cover it (covered = %v)",
			mtime.Sub(observedAt), codexCoveredMtimes(rollouts))
	}
	// End to end: the nudge owes a debt floored at the uncovered append, which the
	// live-read fallback can pay.
	if !nudgeCodexUsageRefresh(f.home, f.fp, now, rollouts, latest) {
		t.Fatal("a later append with no telemetry of its own must owe a refresh")
	}
	if snap := f.snapshot(t); snap.RefreshOwedAtMs == 0 || snap.RunFloorMs != mtime.UnixMilli() {
		t.Fatalf("debt not floored at the uncovered append: owed=%d floor=%d want floor=%d",
			snap.RefreshOwedAtMs, snap.RunFloorMs, mtime.UnixMilli())
	}
}

// Coverage takes TWO facts, and a turn that began after the telemetry overrides
// the time window entirely: the lag only bounds a build whose turn-start record
// this scan does not recognise.
func TestCodexRolloutMinedCoversMtime_Boundary(t *testing.T) {
	minedAt := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		mined     time.Time
		laterTurn bool
		mtime     time.Time
		want      bool
	}{
		{"no telemetry mined", time.Time{}, false, minedAt, false},
		{"stamp after the write", minedAt, false, minedAt.Add(-time.Second), true},
		{"inside the lag", minedAt, false, minedAt.Add(codexRolloutCoverageLag - time.Millisecond), true},
		{"on the lag", minedAt, false, minedAt.Add(codexRolloutCoverageLag), true},
		{"past the lag", minedAt, false, minedAt.Add(codexRolloutCoverageLag + time.Millisecond), false},
		// A later turn that finished FAST sits well inside the lag, so the window
		// alone would call it covered and the run's utilization would stay stale.
		{"later turn inside the lag", minedAt, true, minedAt.Add(time.Second), false},
		{"later turn on the lag", minedAt, true, minedAt.Add(codexRolloutCoverageLag), false},
		{"later turn without telemetry", time.Time{}, true, minedAt, false},
	} {
		cov := codexRolloutCoverage{minedAt: tc.mined, laterTurn: tc.laterTurn}
		if got := codexRolloutMinedCoversMtime(cov, tc.mtime); got != tc.want {
			t.Errorf("%s: covers = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Every record Codex opens a turn with is recognised, and nothing else is. A
// prompt that merely QUOTES one of those type names is not a turn boundary: it
// would mark an ordinary covered rollout as owing telemetry for ever.
func TestCodexRolloutLineStartsTurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want bool
	}{
		{"turn_context envelope", `{"timestamp":"2026-09-20T10:00:00Z","type":"turn_context","payload":{"cwd":"/x"}}`, true},
		{"user_message payload", `{"timestamp":"2026-09-20T10:00:00Z","type":"event_msg","payload":{"type":"user_message","message":"hi"}}`, true},
		{"token_count telemetry", `{"timestamp":"2026-09-20T10:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{}}}`, false},
		{"assistant message", `{"timestamp":"2026-09-20T10:00:00Z","type":"response_item","payload":{"type":"message","role":"assistant"}}`, false},
		{"prompt quoting a type name", `{"timestamp":"2026-09-20T10:00:00Z","type":"response_item","payload":{"type":"message","content":"explain turn_context and user_message"}}`, false},
		{"malformed", `{"type":"turn_context"`, false},
	} {
		if got := codexRolloutLineStartsTurn(tc.line); got != tc.want {
			t.Errorf("%s: startsTurn = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Coverage has to release evidence already HELD, not just suppress a fresh
// report: the reconcile may only reach a file's frames a pass or two after first
// reporting its mtime, and the reading it then merges is EARLIER than that mtime.
// Held evidence judged on mtime alone would stay "behind" for ever.
func TestNudgeCodexUsageRefresh_CoverageReleasesHeldEvidence(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	observed := now.Add(-time.Hour)
	f.seedPreRunReading(t, observed, now)
	rollout := now.Add(-2 * time.Minute)
	const entry = "held-rollout"
	codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		snap.PendingRolloutMtimeMs, snap.PendingRolloutEntry = rollout.UnixMilli(), entry
	})

	// The pass that finally mines that rollout reports it as covered, and reports
	// no newest (its cursor has consumed the file).
	if nudgeCodexUsageRefresh(f.home, f.fp, now, codexRolloutNudgeEvidence{covered: map[string]time.Time{entry: rollout}}, observed) {
		t.Fatal("evidence the mined telemetry covers must not create a debt")
	}
	if snap := f.snapshot(t); snap.PendingRolloutMtimeMs != 0 || snap.RefreshOwedAtMs != 0 {
		t.Fatalf("covered evidence was neither released nor ignored: pending=%d owed=%d", snap.PendingRolloutMtimeMs, snap.RefreshOwedAtMs)
	}
}

// The notice a RETIRED debt raises names the marker's floor, not the zero time:
// the debt's own RunFloorMs is gone by then, so both the stale-run notice and the
// capture-drift notice have to read the marker instead.
func TestCodexStaleRunNotice_RetiredDebtNamesTheMarkersFloor(t *testing.T) {
	floor := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	state := codexRunFreshnessState{
		latest:       floor.Add(-time.Hour),
		expiredFloor: floor,
		codexVersion: "codex-cli 0.149.0",
	}

	notice := codexStaleRunNotice(state)
	if notice == "" {
		t.Fatal("a retired debt's marker must still warn")
	}
	for _, want := range []string{"09:00 UTC", "10:00 UTC", "never found"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("notice %q missing %q", notice, want)
		}
	}
	if strings.Contains(notice, "0001-01-01") {
		t.Fatalf("notice printed the zero time: %q", notice)
	}
	// The drift notice reads the same floor.
	drift := codexRunFreshnessNotice(state, "codex-cli 0.150.0")
	if !strings.Contains(drift, "10:00 UTC") || strings.Contains(drift, "0001-01-01") {
		t.Fatalf("drift notice must name the marker's floor: %q", drift)
	}
}

// The expiry marker outlives its debt on purpose, so nothing in the debt's own
// settle path can clear it: a later COVERING observation must. Without that the
// warning would survive every refresh and only an account rescope could ever
// remove it — a permanent notice on a card that is now current.
func TestCodexClearCoveredStaleRunNotice_ALaterObservationClearsTheMarker(t *testing.T) {
	real := time.Now()
	f := newCodexFreshnessFixture(t, real.Add(-9*time.Hour))
	runStart := real.Add(-codexRefreshOwedMaxAge - time.Hour)
	owedAt := runStart.Add(time.Minute)
	f.seedPreRunReading(t, runStart.Add(-time.Hour), real)
	f.oweUnpaidDebt(t, runStart, owedAt, 1, 0)

	if !codexRetireExpiredRunDebt(f.fp, real) {
		t.Fatal("the over-age debt was not retired")
	}
	if got := f.snapshot(t).StaleRunNoticeFloorMs; got != runStart.UnixMilli() {
		t.Fatalf("marker floor = %d, want the unobserved run start %d", got, runStart.UnixMilli())
	}
	if codexStaleRunNotice(codexRunFreshnessForAccount(f.fp, real)) == "" {
		t.Fatal("the retired debt must warn")
	}

	// A fresh reading lands, covering the floor the marker names. No debt exists
	// any more, so only codexClearCoveredStaleRunNotice can retract the warning.
	f.seedPreRunReading(t, real, real)

	snap := f.snapshot(t)
	if snap.StaleRunNoticeFloorMs != 0 || snap.StaleRunNoticeAtMs != 0 {
		t.Fatalf("a covering observation must clear the marker: %+v", snap)
	}
	if notice := codexStaleRunNotice(codexRunFreshnessForAccount(f.fp, real)); notice != "" {
		t.Fatalf("a refreshed card must carry no stale notice, got %q", notice)
	}
}

// An observation that covers a NEWER debt's floor says nothing about the older
// run the marker names, so it must not retract that warning. (A withdrawal can
// legitimately roll RunFloorMs back below the marker's floor.)
func TestCodexClearCoveredStaleRunNotice_KeepsAMarkerNoObservationCovers(t *testing.T) {
	real := time.Now()
	f := newCodexFreshnessFixture(t, real.Add(-9*time.Hour))
	f.seedPreRunReading(t, real.Add(-8*time.Hour), real)
	// A marker for a run that started AFTER any reading we will land below.
	codexRecordRunFreshness(f.fp, real, func(snap *codexRateLimitSnapshot) {
		snap.StaleRunNoticeFloorMs = real.Add(time.Hour).UnixMilli()
		snap.StaleRunNoticeAtMs = real.UnixMilli()
	})

	// An observation older than the marker's floor.
	f.seedPreRunReading(t, real.Add(-time.Minute), real)

	if got := f.snapshot(t).StaleRunNoticeFloorMs; got == 0 {
		t.Fatal("an observation that does not cover the marker's floor must not clear it")
	}
}

// Retirement must PROMOTE a newer run parked behind the expired debt. That
// parked floor is the newer run's only crash-recovery marker, and dropping it
// would leave a restart unable to classify it as interrupted.
func TestCodexRetireExpiredRunDebt_PromotesAParkedNewerRun(t *testing.T) {
	real := time.Now()
	f := newCodexFreshnessFixture(t, real.Add(-9*time.Hour))
	runStart := real.Add(-codexRefreshOwedMaxAge - time.Hour)
	owedAt := runStart.Add(time.Minute)
	f.seedPreRunReading(t, runStart.Add(-time.Hour), real)
	f.oweUnpaidDebt(t, runStart, owedAt, 1, 0)
	// A second run started while the older debt stood, so it was parked.
	parked := real.Add(-time.Minute).Truncate(time.Millisecond)
	codexRecordRunFreshness(f.fp, owedAt, func(snap *codexRateLimitSnapshot) {
		codexArmRunFloor(snap, parked)
	})
	if got := f.snapshot(t).ActiveRunFloorMs; got != parked.UnixMilli() {
		t.Fatalf("fixture must park the newer run: ActiveRunFloorMs=%d want %d", got, parked.UnixMilli())
	}

	if !codexRetireExpiredRunDebt(f.fp, real) {
		t.Fatal("the over-age debt was not retired")
	}

	snap := f.snapshot(t)
	if snap.RunFloorMs != parked.UnixMilli() {
		t.Fatalf("retirement dropped the parked run: RunFloorMs=%d want the promoted %d", snap.RunFloorMs, parked.UnixMilli())
	}
	if snap.ActiveRunFloorMs != 0 {
		t.Fatalf("a promoted floor must leave the park empty: %+v", snap)
	}
	// And that promoted run is what a restart now classifies as interrupted.
	if state := codexRunFreshnessForAccount(f.fp, real); !state.interrupted {
		t.Fatalf("the promoted run must still be recoverable at startup: %+v", state)
	}
}

// Only `exhausted` releases the warning for a live debt. Every other fallback
// state is still trying, and the instants between a payment pass returning
// `deferred` and the worker booking its rung must NOT read as "given up" — that
// is the flicker (warn, then un-warn on the next gather) the gate exists to
// prevent. The expiry marker is the other, independent way in.
func TestCodexStaleRunNoticeDue_OnlyExhaustedOrTheExpiryMarker(t *testing.T) {
	floor := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	base := codexRunFreshnessState{
		floor: floor, owedAt: floor.Add(time.Minute), latest: floor.Add(-time.Hour),
		owed: true, attempts: codexRefreshAfterRunMaxAttempts,
	}
	for _, tc := range []struct {
		name     string
		fallback string
		rung     time.Time
		want     bool
	}{
		{"fallback not reached yet", codexFallbackUnset, time.Time{}, false},
		{"deferred with a rung booked is still trying", codexFallbackDeferred, floor.Add(time.Hour), false},
		{"deferred mid-hand-off to the schedule is still trying", codexFallbackDeferred, time.Time{}, false},
		{"a read in flight is still trying", codexFallbackOutstanding, time.Time{}, false},
		{"exhausted has nothing left", codexFallbackExhausted, time.Time{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := base
			state.fallback, state.nextAttemptAt = tc.fallback, tc.rung
			if got := codexStaleRunNoticeDue(state); got != tc.want {
				t.Fatalf("due = %v, want %v (%+v)", got, tc.want, state)
			}
		})
	}

	// A retired debt's marker is due regardless of the counters it no longer has.
	retired := codexRunFreshnessState{latest: floor.Add(-time.Hour), expiredFloor: floor}
	if !codexStaleRunNoticeDue(retired) {
		t.Fatalf("the expiry marker must be due on its own: %+v", retired)
	}
	// ...but not while a NEW debt is being chased.
	retired.owed, retired.floor = true, floor.Add(time.Hour)
	if codexStaleRunNoticeDue(retired) {
		t.Fatal("a live debt still being paid must not surface an older expiry")
	}
}

// A rung the bounded cache locks refuse to persist must not be DROPPED — that is
// the exact failure this schedule exists to fix. It is armed in-process anyway,
// so the ladder keeps walking even though nothing reached disk.
func TestCodexScheduleRunDebtRetry_ARefusedWriteStillArmsARetry(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	state := f.oweUnpaidDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute), 1, 0)

	prevWait, prevPoll := codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll
	codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = 20*time.Millisecond, time.Millisecond
	t.Cleanup(func() { codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = prevWait, prevPoll })

	booked := func() bool {
		codexRateLimitMu.Lock()
		defer codexRateLimitMu.Unlock()
		return codexScheduleRunDebtRetry(f.fp, state, time.Now(), codexRetryAfterScan)
	}()

	if !booked {
		t.Fatal("a refused write must still leave a retry armed, not drop the debt")
	}
	if !codexRunDebtRetryPending() {
		t.Fatal("no timer armed after the refused write")
	}
	drainCodexRunDebtLadder(t)
}

// Contributors are not permanent — an empty authoritative full snapshot drops
// them — so the marker must clear off the PAID WATERMARK too. Trusting the live
// map alone would let such a clear resurrect a warning a reading had already
// answered.
func TestCodexClearCoveredStaleRunNotice_ClearsOffThePaidWatermark(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	markerFloor := now.Add(-2 * time.Hour)

	// The watermark already covers the marker's floor, but no contributor does.
	codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		snap.Contributors = map[string]map[string]codexRateLimitBucket{}
		snap.StaleRunNoticeFloorMs = markerFloor.UnixMilli()
		snap.StaleRunNoticeAtMs = now.UnixMilli()
		snap.RunFloorPaidMs = markerFloor.Add(time.Minute).UnixMilli()
	})

	if snap := f.snapshot(t); snap.StaleRunNoticeFloorMs != 0 {
		t.Fatalf("a watermark past the marker's floor must clear it: %+v", snap)
	}

	// A watermark that has NOT reached the floor leaves the warning standing.
	codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		snap.Contributors = map[string]map[string]codexRateLimitBucket{}
		snap.StaleRunNoticeFloorMs = markerFloor.UnixMilli()
		snap.StaleRunNoticeAtMs = now.UnixMilli()
		snap.RunFloorPaidMs = markerFloor.Add(-time.Minute).UnixMilli()
	})
	if snap := f.snapshot(t); snap.StaleRunNoticeFloorMs == 0 {
		t.Fatal("a watermark short of the marker's floor must not clear it")
	}
}

// The legacy fallback migration has TWO call sites — the loader and the cache
// transaction's own read — because a mutation can branch on the fallback state
// and must never see a value no current code path understands. Cover the
// transaction site: a legacy cache mutated by any writer comes out migrated.
func TestCodexMigrateLegacyFallbackState_AppliesInsideTheTransaction(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	f.oweUnpaidDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute), 1, 0)

	// Rewrite the persisted state to the pre-split value, behind the cache API,
	// so only the transaction's own read can migrate it.
	raw, err := os.ReadFile(f.cache)
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(string(raw), `"refreshOwedAttempts": 1`,
		`"refreshOwedAttempts": 1,`+"\n"+`  "refreshFallbackState": "`+codexLegacyFallbackSpent+`"`, 1)
	if legacy == string(raw) {
		t.Fatalf("fixture shape changed; could not inject the legacy state:\n%s", raw)
	}
	if err := os.WriteFile(f.cache, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	// Any mutation at all: the migration runs on the transaction's read.
	var seen string
	codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		seen = snap.RefreshFallbackState
	})

	if seen != codexFallbackDeferred {
		t.Fatalf("the mutation saw %q, want the migrated %q", seen, codexFallbackDeferred)
	}
	snap := f.snapshot(t)
	if snap.RefreshFallbackState != codexFallbackDeferred || snap.RefreshLiveReads != 1 {
		t.Fatalf("the transaction must persist the migrated state: %+v", snap)
	}
}

// The nudge runs on every gather (~30 s). In the steady state it must not take
// the blocking cross-process cache lock at all — proven by wedging that lock and
// asserting the nudge still returns promptly rather than spending the whole
// bounded wait. The case that matters most is a debt PENDING with its next rung
// still in the future: its own timer owns that rung, and without the cheap read
// the nudge would pay for the lock on every gather for the debt's whole life.
func TestNudgeCodexUsageRefresh_SteadyStateTakesNoCacheLock(t *testing.T) {
	now := time.Now()
	observed := now.Add(-time.Hour)
	settledRollout := now.Add(-2 * codexForcedReconcileMinInterval)

	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f codexFreshnessFixture)
		// rollout handed to the nudge; zero means "the reconcile reported nothing".
		rollout time.Time
	}{
		{
			name:    "nothing owed and no missed run",
			setup:   func(*testing.T, codexFreshnessFixture) {},
			rollout: time.Time{},
		},
		{
			// The realistic steady state: the rollout was consumed by the scan
			// cursor long ago, so the reconcile reports nothing new.
			name: "debt pending with its rung still in the future",
			setup: func(t *testing.T, f codexFreshnessFixture) {
				f.oweUnpaidDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute), 1, 0)
				codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
					snap.NextAttemptAtMs = now.Add(time.Hour).UnixMilli()
				})
			},
			rollout: time.Time{},
		},
		{
			// Evidence ALREADY on disk and re-reported unchanged is not fresh, so it
			// needs no write either — a re-report must not re-take the lock.
			name: "evidence already recorded, re-reported unchanged",
			setup: func(t *testing.T, f codexFreshnessFixture) {
				f.oweUnpaidDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute), 1, 0)
				codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
					snap.NextAttemptAtMs = now.Add(time.Hour).UnixMilli()
					snap.PendingRolloutMtimeMs = settledRollout.UnixMilli()
				})
			},
			rollout: settledRollout,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
			f.seedPreRunReading(t, observed, now)
			tc.setup(t, f)

			// A wait long enough that paying it would be unmistakable.
			prevWait, prevPoll := codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll
			codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = 2*time.Second, 5*time.Millisecond
			t.Cleanup(func() { codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = prevWait, prevPoll })

			codexRateLimitMu.Lock()
			defer codexRateLimitMu.Unlock()

			start := time.Now()
			if nudgeCodexUsageRefresh(f.home, f.fp, now, codexRolloutNudgeEvidence{newest: tc.rollout}, observed) {
				t.Fatal("the steady state must arm nothing")
			}
			if took := time.Since(start); took > 300*time.Millisecond {
				t.Fatalf("the nudge waited %s on the wedged cache lock; it must decide off the cheap read", took)
			}
		})
	}
}

// codexNudgeNeedsWrite must mirror exactly what the transaction would decide, or
// the cheap guard would silently drop work.
func TestCodexNudgeNeedsWrite_MirrorsTheTransactionsDecision(t *testing.T) {
	now := time.Now()
	nowMs := now.UnixMilli()
	for _, tc := range []struct {
		name                 string
		view                 codexCacheView
		settled, live        bool
		freshEvidence, stale bool
		want                 bool
	}{
		{name: "idle", want: false},
		{name: "fresh rollout evidence to record", freshEvidence: true, want: true},
		{name: "held evidence the reading caught up with", stale: true, want: true},
		{name: "missed run to own", settled: true, want: true},
		{name: "missed run but a local run is open", settled: true, live: true, want: false},
		{
			name: "debt with a future rung belongs to its timer",
			view: codexCacheView{refreshOwedAtMs: nowMs - 1000, nextAttemptAtMs: nowMs + 60_000},
			want: false,
		},
		{
			name: "debt with a due rung arms the worker",
			view: codexCacheView{refreshOwedAtMs: nowMs - 1000, nextAttemptAtMs: nowMs - 1},
			want: true,
		},
		{
			name: "debt mid-hand-off (no rung) belongs to its worker",
			view: codexCacheView{refreshOwedAtMs: nowMs - 1000},
			want: false,
		},
		{
			name: "an over-age debt must be retired",
			view: codexCacheView{refreshOwedAtMs: now.Add(-codexRefreshOwedMaxAge - time.Hour).UnixMilli()},
			want: true,
		},
		{
			name: "a future-dated floor must be rebased",
			view: codexCacheView{runFloorMs: now.Add(codexRunFloorLocalSkew + time.Hour).UnixMilli()},
			want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexNudgeNeedsWrite(tc.view, now, tc.settled, tc.live, tc.freshEvidence, tc.stale); got != tc.want {
				t.Fatalf("needsWrite = %v, want %v", got, tc.want)
			}
		})
	}
}

// cancelCodexRefreshInFlight / restoreCodexRefreshCancel drive the gate's cancel
// channel — what a shutdown or a gate reset does to an in-flight live read. The
// field is guarded by the gate mutex (see cancelCh), so a test may close and
// replace it; the process-wide shutdownChan may NOT be driven this way, because
// production closes it exactly once and never reassigns it.
func cancelCodexRefreshInFlight(t *testing.T) {
	t.Helper()
	codexUsageRefresh.mu.Lock()
	close(codexUsageRefresh.cancel)
	codexUsageRefresh.mu.Unlock()
}

func restoreCodexRefreshCancel(t *testing.T) {
	t.Helper()
	codexUsageRefresh.mu.Lock()
	codexUsageRefresh.cancel = make(chan struct{})
	codexUsageRefresh.mu.Unlock()
}

/* ─────────────────────── the unscoped (identity-less) account ─────────────────────── */

// unscopeAccount rewrites auth.json with credentials carrying no derivable
// identity (API-key-only auth), the state in which
// currentCodexAccountFingerprint returns "" and the cache operates UNSCOPED.
func (f codexFreshnessFixture) unscopeAccount(t *testing.T) codexFreshnessFixture {
	t.Helper()
	helperWriteJSON(t, filepath.Join(f.home, "auth.json"), map[string]any{"OPENAI_API_KEY": "sk-test"})
	if fp := currentCodexAccountFingerprint(); fp != "" {
		t.Fatalf("fixture must be unscoped, got fingerprint %q", fp)
	}
	f.fp = ""
	return f
}

// An empty fingerprint is a real, ELIGIBLE account — not a missing one. Refusing
// to book a rung for it left such a device unable to retry at all: an empty first
// scan could never reach the live-read fallback, so the utilization stayed stale
// until the debt aged out, which the pre-ladder fallback did not do.
func TestCodexScheduleRunDebtRetry_BooksForAnUnscopedAccount(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour)).unscopeAccount(t)
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	state := f.oweUnpaidDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute), 1, 0)

	if !codexScheduleRunDebtRetry(f.fp, state, now, codexRetryAfterScan) {
		t.Fatal("an unscoped account booked no rung")
	}
	rung := f.bookedRung(t)
	if rung.IsZero() {
		t.Fatal("no rung persisted for the unscoped account")
	}
	if want := now.Add(codexRunDebtRetryLadder[0]); rung.UnixMilli() != want.UnixMilli() {
		t.Fatalf("rung %s, want %s", rung, want)
	}
	if !codexRunDebtRetryPending() {
		t.Fatal("no timer armed for the unscoped account")
	}
	// The live-read phase is reachable too — the phase the old guard cut off.
	spent := f.oweUnpaidDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute),
		codexRefreshAfterRunMaxAttempts, 1)
	if !codexScheduleRunDebtRetry(f.fp, spent, now, codexRetryAfterRead) {
		t.Fatal("an unscoped account booked no rung after a failed live read")
	}
}

// Retirement must reach the unscoped account too: it is the only path that leaves
// the expiry marker, so refusing it would age a debt out with no marker and no
// warning — a card that looks current for ever.
func TestCodexRetireExpiredRunDebt_RetiresForAnUnscopedAccount(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-9*time.Hour)).unscopeAccount(t)
	runStart := now.Add(-codexRefreshOwedMaxAge - time.Hour)
	f.seedPreRunReading(t, runStart.Add(-time.Hour), now)
	f.oweUnpaidDebt(t, runStart, runStart.Add(time.Minute), codexRefreshAfterRunMaxAttempts, 0)

	if !codexRetireExpiredRunDebt(f.fp, now) {
		t.Fatal("an over-age unscoped debt was not retired")
	}
	snap := f.snapshot(t)
	if snap.StaleRunNoticeFloorMs != runStart.UnixMilli() || snap.StaleRunNoticeAtMs == 0 {
		t.Fatalf("expiry marker not written: floor=%d at=%d", snap.StaleRunNoticeFloorMs, snap.StaleRunNoticeAtMs)
	}
	if snap.RefreshOwedAtMs != 0 || snap.NextAttemptAtMs != 0 {
		t.Fatalf("retired debt left behind: owed=%d rung=%d", snap.RefreshOwedAtMs, snap.NextAttemptAtMs)
	}
}

// The nudge is the only trigger an unmanaged run ever gets, so it has to accept
// the unscoped account as well; rejecting it meant a `codex` the user ran in their
// own shell could never converge on such a device.
func TestNudgeCodexUsageRefresh_AcceptsAnUnscopedAccount(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour)).unscopeAccount(t)
	observedAt := now.Add(-time.Hour)
	f.seedPreRunReading(t, observedAt, now)
	rollout := now.Add(-codexForcedReconcileMinInterval - time.Second)

	if !nudgeCodexUsageRefresh(f.home, f.fp, now, codexRolloutNudgeEvidence{newest: rollout}, observedAt) {
		t.Fatal("the nudge refused the unscoped account")
	}
	if got := f.snapshot(t).RefreshOwedAtMs; got == 0 {
		t.Fatal("the nudge created no debt for the unscoped account")
	}
	if got := f.snapshot(t).RunFloorMs; got != rollout.UnixMilli() {
		t.Fatalf("debt floored at %d, want the rollout mtime %d", got, rollout.UnixMilli())
	}
}

/* ────────────────── the retired-debt notice keeps its last reading ────────────────── */

// codexRetireRunDebtInSnapshot clears RunFloorMs but KEEPS the expiry marker, so
// the retired-debt notice is read out of a state with no floor. Returning from
// codexRunFreshnessFromView before populating `latest` made that warning claim no
// Codex reading had ever been observed even though a perfectly good pre-run one
// was still cached — the one number the notice exists to report.
func TestCodexRunFreshnessFromView_RetiredDebtKeepsTheLastObservation(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-9*time.Hour))
	runStart := now.Add(-codexRefreshOwedMaxAge - time.Hour)
	observedAt := runStart.Add(-time.Hour).Truncate(time.Millisecond)
	f.seedPreRunReading(t, observedAt, now)
	f.oweUnpaidDebt(t, runStart, runStart.Add(time.Minute), codexRefreshAfterRunMaxAttempts, 0)
	if !codexRetireExpiredRunDebt(f.fp, now) {
		t.Fatal("the over-age debt was not retired")
	}

	state := codexRunFreshnessForAccount(f.fp, now)
	if state.owed {
		t.Fatal("a retired debt must not read as owed")
	}
	if !state.floor.IsZero() {
		t.Fatalf("retirement must clear the floor, got %s", state.floor)
	}
	if state.latest.UnixMilli() != observedAt.UnixMilli() {
		t.Fatalf("latest observation = %s, want the cached pre-run reading %s", state.latest, observedAt)
	}
	notice := codexStaleRunNotice(state)
	if !strings.Contains(notice, "last observed") {
		t.Fatalf("notice must name the last observed reading, got %q", notice)
	}
	if strings.Contains(notice, "No Codex utilization reading has been observed") {
		t.Fatalf("notice denied a cached reading it still has: %q", notice)
	}
}

// A later turn that finishes FAST does not inherit the previous turn's coverage.
//
// codexRolloutCoverageLag forgives the gap between a telemetry frame and the
// same turn's trailing records, but a whole later turn can land inside that
// window too. Judged on the time gap alone the file reads as covered, so the
// nudge withholds its mtime, no debt is created for the later run, and its
// utilization stays stale with the card showing nothing wrong — the reported
// shape, one turn smaller. The turn-start record is what tells the two apart.
func TestCodexReconcileFromRollout_FastLaterTurnIsNotCovered(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)

	observedAt := now.Add(-2 * time.Minute).Truncate(time.Second)
	// Well inside codexRolloutCoverageLag: the whole point of the case.
	mtime := observedAt.Add(2 * time.Second)
	path := writeCodexRunRollout(t, f.home, "fastturn", now.Add(-30*time.Minute), observedAt, mtime, true,
		[]map[string]any{codexRateLimitFrame(20, 30, observedAt)})
	// The later turn: its own turn_context and a completion record, no telemetry.
	appendCodexRolloutLines(t, path, mtime,
		map[string]any{"timestamp": mtime.UTC().Format(time.RFC3339Nano), "type": "turn_context", "payload": map[string]any{"cwd": "/w"}},
		map[string]any{"timestamp": mtime.UTC().Format(time.RFC3339Nano), "type": "event_msg", "payload": map[string]any{"type": "task_complete"}},
	)

	_, _, latest, rollouts := codexReconcileFromRollout(context.Background(), f.home, f.fp, now)

	if latest.Before(observedAt) {
		t.Fatalf("the pass did not mine the earlier turn's telemetry (latest = %s)", latest)
	}
	if len(rollouts.covered) != 0 {
		t.Fatalf("a turn that began AFTER the mined frame must leave the file uncovered (covered = %v)", codexCoveredMtimes(rollouts))
	}
	if rollouts.newest.Truncate(time.Second) != mtime.Truncate(time.Second) {
		t.Fatalf("eligible mtime = %s, want the later turn %s still owing telemetry", rollouts.newest, mtime)
	}
	// End to end: the nudge owes a debt floored at that turn, so the ladder runs.
	if !nudgeCodexUsageRefresh(f.home, f.fp, now, rollouts, latest) {
		t.Fatal("a later turn with no telemetry of its own must owe a refresh")
	}
	if snap := f.snapshot(t); snap.RefreshOwedAtMs == 0 || snap.RunFloorMs != mtime.UnixMilli() {
		t.Fatalf("debt not floored at the later turn: owed=%d floor=%d want floor=%d",
			snap.RefreshOwedAtMs, snap.RunFloorMs, mtime.UnixMilli())
	}
}

// The other half of the same predicate: an ordinary single-turn rollout whose
// trailing records advanced the mtime STAYS covered, so the common case makes no
// request and arms no ladder.
func TestCodexReconcileFromRollout_TrailingRecordsOfTheSameTurnStayCovered(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)

	observedAt := now.Add(-2 * time.Minute).Truncate(time.Second)
	mtime := observedAt.Add(2 * time.Second)
	path := writeCodexRunRollout(t, f.home, "sameturn", now.Add(-30*time.Minute), observedAt, mtime, true,
		[]map[string]any{codexRateLimitFrame(20, 30, observedAt)})
	appendCodexRolloutLines(t, path, mtime,
		map[string]any{"timestamp": mtime.UTC().Format(time.RFC3339Nano), "type": "event_msg", "payload": map[string]any{"type": "task_complete"}},
	)

	_, _, _, rollouts := codexReconcileFromRollout(context.Background(), f.home, f.fp, now)
	if got := codexCoveredMtimes(rollouts); len(got) != 1 || got[0].Truncate(time.Second) != mtime.Truncate(time.Second) {
		t.Fatalf("covered = %v, want the file's own mtime %s", got, mtime)
	}
	if !rollouts.newest.IsZero() {
		t.Fatalf("a covered file must not be reported as owing telemetry (newest = %s)", rollouts.newest)
	}
}

// Coverage belongs to the file that produced it. With overlapping sessions a
// covered rollout B (telemetry, then trailing records) can carry a LATER mtime
// than an uncovered rollout A whose telemetry-free run B's reading predates.
// Ordering the two mtimes would call A covered on B's account, so A would never
// owe a refresh and its utilization would stay stale with no fallback reached.
func TestCodexReconcileFromRollout_AnotherSessionsCoverageDoesNotCoverIt(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)

	bFrame := now.Add(-3 * time.Minute).Truncate(time.Second)
	bMtime := bFrame.Add(29 * time.Second)
	aMtime := bFrame.Add(19 * time.Second)
	writeCodexRunRollout(t, f.home, "covered-b", now.Add(-30*time.Minute), bFrame, bMtime, true,
		[]map[string]any{codexRateLimitFrame(20, 30, bFrame)})
	writeCodexRunRollout(t, f.home, "silent-a", now.Add(-20*time.Minute), aMtime, aMtime, true, nil)

	_, _, latest, rollouts := codexReconcileFromRollout(context.Background(), f.home, f.fp, now)

	if rollouts.newest.Truncate(time.Second) != aMtime {
		t.Fatalf("newest = %s, want the uncovered rollout %s", rollouts.newest, aMtime)
	}
	if got := codexCoveredMtimes(rollouts); len(got) != 1 || got[0].Truncate(time.Second) != bMtime {
		t.Fatalf("covered = %v, want only B at %s", got, bMtime)
	}
	if !nudgeCodexUsageRefresh(f.home, f.fp, now, rollouts, latest) {
		t.Fatal("A's run owes a refresh; B's newer covered mtime must not hide it")
	}
	if snap := f.snapshot(t); snap.RefreshOwedAtMs == 0 || snap.RunFloorMs != aMtime.UnixMilli() {
		t.Fatalf("debt not floored at A: owed=%d floor=%d want floor=%d",
			snap.RefreshOwedAtMs, snap.RunFloorMs, aMtime.UnixMilli())
	}
}

// The held-evidence half of the same rule: evidence already on disk for file A
// is released only when a pass mines A itself, never because some other file
// with a newer mtime was covered.
func TestNudgeCodexUsageRefresh_AnotherFilesCoverageKeepsHeldEvidence(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	observed := now.Add(-time.Hour)
	f.seedPreRunReading(t, observed, now)
	held := now.Add(-3 * time.Minute)
	codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		snap.PendingRolloutMtimeMs, snap.PendingRolloutEntry = held.UnixMilli(), "rollout-a"
	})

	other := codexRolloutNudgeEvidence{covered: map[string]time.Time{"rollout-b": held.Add(time.Minute)}}
	if !nudgeCodexUsageRefresh(f.home, f.fp, now, other, observed) {
		t.Fatal("held evidence for A must still owe a refresh when only B was covered")
	}
	if snap := f.snapshot(t); snap.RefreshOwedAtMs == 0 || snap.RunFloorMs != held.UnixMilli() {
		t.Fatalf("debt not floored at A's held evidence: owed=%d floor=%d", snap.RefreshOwedAtMs, snap.RunFloorMs)
	}
}

// A held value the pass covered yields to a fresh report even when it is the
// newer of the two, so an OLDER uncovered file reported in the same pass is not
// shadowed by evidence that no longer owes anything.
func TestNudgeCodexUsageRefresh_CoveredHeldEvidenceYieldsToAFreshReport(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	observed := now.Add(-time.Hour)
	f.seedPreRunReading(t, observed, now)
	held := now.Add(-2 * time.Minute)
	codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		snap.PendingRolloutMtimeMs, snap.PendingRolloutEntry = held.UnixMilli(), "rollout-a"
	})
	older := now.Add(-4 * time.Minute).Truncate(time.Millisecond)

	if !nudgeCodexUsageRefresh(f.home, f.fp, now, codexRolloutNudgeEvidence{
		newest: older, newestEntry: "rollout-c",
		covered: map[string]time.Time{"rollout-a": held},
	}, observed) {
		t.Fatal("the uncovered report must owe a refresh once the covered held evidence yields")
	}
	if snap := f.snapshot(t); snap.RunFloorMs != older.UnixMilli() {
		t.Fatalf("the uncovered report was shadowed by covered held evidence: floor=%d want %d",
			snap.RunFloorMs, older.UnixMilli())
	}
}

// codexCoveredMtimes lists the covered rollouts' mtimes, for assertions.
func codexCoveredMtimes(e codexRolloutNudgeEvidence) []time.Time {
	out := make([]time.Time, 0, len(e.covered))
	for _, m := range e.covered {
		out = append(out, m)
	}
	return out
}

// appendCodexRolloutLines adds records to a rollout written by
// writeCodexRunRollout (which emits its extras BEFORE the telemetry frames) and
// restores the file mtime the test is pinning.
func appendCodexRolloutLines(t *testing.T, path string, mtime time.Time, lines ...map[string]any) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open rollout: %v", err)
	}
	for _, line := range lines {
		encoded, err := json.Marshal(line)
		if err != nil {
			f.Close()
			t.Fatalf("marshal rollout line: %v", err)
		}
		if _, err := f.Write(append(encoded, '\n')); err != nil {
			f.Close()
			t.Fatalf("append rollout line: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close rollout: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes rollout: %v", err)
	}
}

// The reconcile reports a ZERO observation from every path that returns before
// folding the cached reading in — most commonly the steady state where its cursor
// has consumed every candidate. Held evidence must not be judged "behind" on
// that: the transaction's authoritative contributor check would decline to create
// a debt while `stale` stayed false, so nothing released the evidence and every
// later gather re-took the blocking cache lock for ever.
func TestNudgeCodexUsageRefresh_CaughtUpReadingReleasesHeldEvidence(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	held := now.Add(-2 * codexForcedReconcileMinInterval)
	// The live read that already caught up with the held evidence.
	f.seedPreRunReading(t, held.Add(time.Minute), now)
	codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		snap.PendingRolloutMtimeMs, snap.PendingRolloutEntry = held.UnixMilli(), "rollout-a"
	})

	// The reconcile reported nothing at all: no newest, no coverage, and — from an
	// early return — a zero observation.
	if nudgeCodexUsageRefresh(f.home, f.fp, now, codexRolloutNudgeEvidence{}, time.Time{}) {
		t.Fatal("evidence the cached reading has caught up with must not create a debt")
	}
	if snap := f.snapshot(t); snap.PendingRolloutMtimeMs != 0 || snap.PendingRolloutEntry != "" || snap.RefreshOwedAtMs != 0 {
		t.Fatalf("caught-up evidence was not released: pending=%d entry=%q owed=%d",
			snap.PendingRolloutMtimeMs, snap.PendingRolloutEntry, snap.RefreshOwedAtMs)
	}

	// And with the evidence gone the next gather decides off the cheap read alone:
	// it must never re-take the blocking cross-process lock.
	prevWait, prevPoll := codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll
	codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = 2*time.Second, 5*time.Millisecond
	t.Cleanup(func() { codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = prevWait, prevPoll })
	codexRateLimitMu.Lock()
	defer codexRateLimitMu.Unlock()
	start := time.Now()
	if nudgeCodexUsageRefresh(f.home, f.fp, now.Add(codexRefreshNudgeCooldown), codexRolloutNudgeEvidence{}, time.Time{}) {
		t.Fatal("a released-evidence steady state must arm nothing")
	}
	if took := time.Since(start); took > 300*time.Millisecond {
		t.Fatalf("the nudge waited %s on the wedged cache lock; the release must be once, not every gather", took)
	}
}
