package main

import (
	"context"
	"os"
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
	if nudgeCodexUsageRefresh(f.home, f.fp, now, rollout, state.latest) {
		t.Fatal("a rung in the future must be left to its own timer")
	}

	// Rung due: the nudge arms the worker.
	resetCodexRefreshNudge()
	codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		snap.NextAttemptAtMs = now.Add(-time.Second).UnixMilli()
	})
	if !nudgeCodexUsageRefresh(f.home, f.fp, now, rollout, state.latest) {
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

	if !nudgeCodexUsageRefresh(f.home, f.fp, now, rollout, observed) {
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
		if nudgeCodexUsageRefresh(f.home, f.fp, now, time.Time{}, observed) {
			t.Fatal("a pass that reported no eligible mtime must nudge nothing")
		}
	})

	t.Run("rollout older than the reading", func(t *testing.T) {
		f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
		f.seedPreRunReading(t, observed, now)
		if nudgeCodexUsageRefresh(f.home, f.fp, now, observed.Add(-time.Minute), observed) {
			t.Fatal("telemetry the reading already covers is not a missed run")
		}
	})

	t.Run("rollout still being written", func(t *testing.T) {
		f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
		f.seedPreRunReading(t, observed, now)
		if nudgeCodexUsageRefresh(f.home, f.fp, now, now, observed) {
			t.Fatal("a rollout younger than the minimum interval may still be appending")
		}
	})

	t.Run("cooldown", func(t *testing.T) {
		f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
		f.seedPreRunReading(t, observed, now)
		prev := codexRefreshNudgeCooldown
		codexRefreshNudgeCooldown = time.Hour
		t.Cleanup(func() { codexRefreshNudgeCooldown = prev })
		if !nudgeCodexUsageRefresh(f.home, f.fp, now, settled, observed) {
			t.Fatal("the first nudge must go through")
		}
		drainCodexRunDebtLadder(t)
		if nudgeCodexUsageRefresh(f.home, f.fp, now, settled, observed) {
			t.Fatal("a second nudge inside the cooldown must be refused")
		}
	})

	t.Run("live local run", func(t *testing.T) {
		f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
		f.seedPreRunReading(t, observed, now)
		// A run this process armed is still open: it settles itself.
		armCodexUsageRunFloor(time.Now())
		drainCodexRunDebtLadder(t)
		if nudgeCodexUsageRefresh(f.home, f.fp, now, settled, observed) {
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

	_, _, _, newest := codexReconcileFromRollout(context.Background(), f.home, f.fp, now)

	if !newest.IsZero() {
		t.Fatalf("an old-account rollout reported mtime %s; the nudge would floor a debt the new account cannot pay", newest)
	}
}

// A completed pass reports the newest mtime among the candidates that cleared the
// login guard, which is what the nudge floors a created debt at.
func TestCodexReconcileFromRollout_ReportsTheNewestEligibleMtime(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	older := now.Add(-5 * time.Minute).Truncate(time.Second)
	newer := now.Add(-2 * time.Minute).Truncate(time.Second)
	writeCodexRunRollout(t, f.home, "older", now.Add(-10*time.Minute), older, older, true,
		[]map[string]any{codexRateLimitFrame(20, 30, older)})
	writeCodexRunRollout(t, f.home, "newer", now.Add(-8*time.Minute), newer, newer, true,
		[]map[string]any{codexRateLimitFrame(21, 31, newer)})

	_, _, _, newest := codexReconcileFromRollout(context.Background(), f.home, f.fp, now)

	if newest.Truncate(time.Second) != newer {
		t.Fatalf("eligible mtime = %s, want the newest guard-passing candidate %s", newest, newer)
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
		expiredAt:    floor.Add(codexRefreshOwedMaxAge),
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

