package main

import (
	"context"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_codex_post_update_freshness_test.go — the acceptance path of
   the reported bug: after ANY Codex run (direct app-server turn, terminal
   `codex` session, or a green `__cli_smoke__`) observedAt advances within the
   ladder, and it survives an agent self-update.

   Every row here starts from the conditions that used to park the debt until
   the age-out discarded it: the rollout is silent when the run settles, or the
   device is offline right then, or the process is replaced immediately after.
   ------------------------------------------------------------------------ */

// codexObservedAt reads what the CLI Agents card would show for the session row.
func codexObservedAt(t *testing.T, f codexFreshnessFixture) time.Time {
	t.Helper()
	return metricObservedAt(t, codexSessionMetric(t, codexMetricsFromCache(time.Now(), f.fp)))
}

// A run's rollout lands on a LATER rung than the settle pass — the common
// post-update case, where Codex has not flushed its telemetry when the run ends.
// The old worker spent two reconciles five seconds apart and retired; the ladder
// keeps coming back, so observedAt advances past the floor.
//
// Run per source: the direct app-server path and a terminal `codex` session both
// arm and settle through codexUsageRunStarted / codexUsageRunSettled, so one
// table covers both.
func TestCodexPostUpdate_RolloutLandingOnALaterRungPaysTheDebt(t *testing.T) {
	for _, source := range []string{"direct app-server turn", "terminal codex session"} {
		t.Run(source, func(t *testing.T) {
			now := time.Now()
			runStart := now.Add(-2 * time.Minute)
			f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
			f.seedPreRunReading(t, now.Add(-time.Hour), now)
			// The cursor is already past the run, so only a FORCED scan from below
			// the floor can find this rollout.
			f.advanceCursorPast(t, now.Add(-10*time.Second), now)

			codexUsageRunStarted(runStart)
			// The rollout is silent at settle time: nothing for the first pass.
			codexUsageRunSettled(runStart)
			waitCodexUsageRefreshIdle(t)
			if snap := f.snapshot(t); snap.RefreshOwedAtMs == 0 {
				t.Fatalf("the settle must record the debt: %+v", snap)
			}

			// Codex flushes its telemetry a moment later — while the ladder is
			// still walking.
			writeCodexRunRollout(t, f.home, "late-flush", runStart, runStart.Add(time.Minute), runStart.Add(70*time.Second), true,
				[]map[string]any{codexRateLimitFrame(47, 52, now)})
			drainCodexRunDebtLadder(t)

			if got := codexObservedAt(t, f); got.UnixMilli() < runStart.UnixMilli() {
				t.Fatalf("observedAt %s still predates the run floor %s", got, runStart)
			}
			if snap := f.snapshot(t); snap.RefreshOwedAtMs != 0 {
				t.Fatalf("a paid debt must clear: %+v", snap)
			}
		})
	}
}

// The rollout NEVER lands, and the agent was offline at settle time. That used to
// write the final `skipped` and end the debt's only network recovery for good;
// now the refusal costs nothing and a later rung's live read pays it once the
// device is back.
func TestCodexPostUpdate_OfflineAtSettleStillPaysByLiveReadLater(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-2 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	calls := stubCodexFallbackRead(t, capturingFallbackRead)
	setCodexTestOffline(t, true)

	codexUsageRunStarted(runStart)
	codexUsageRunSettled(runStart)
	drainCodexRunDebtLadder(t)

	if *calls != 0 {
		t.Fatalf("an offline agent spent %d live reads", *calls)
	}
	state := codexRunFreshnessForAccount(f.fp, time.Now())
	if !state.owed {
		t.Fatalf("an offline settle must keep the debt: %+v", state)
	}
	if state.fallback == codexFallbackExhausted {
		t.Fatalf("an offline refusal sent nothing and must stay retryable: %+v", state)
	}
	if state.liveReads != 0 {
		t.Fatalf("an offline refusal charged %d reads", state.liveReads)
	}

	// The device comes back and a rung fires.
	setCodexTestOffline(t, false)
	codexRunDebtWorker(f.home, f.fp)
	drainCodexRunDebtLadder(t)

	if *calls == 0 {
		t.Fatal("the live read never ran after the device came back online")
	}
	if got := codexObservedAt(t, f); got.Before(runStart.Truncate(time.Second)) {
		t.Fatalf("observedAt %s still predates the run %s after the live read", got, runStart)
	}
	if snap := f.snapshot(t); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("the live read must clear the debt: %+v", snap)
	}
}

// The reported case, end to end: a marker-matched GREEN smoke whose telemetry is
// silent, then an immediate simulated self-update — no wait for the workers at
// all — and then the rollout evidence lands. observedAt advances past the smoke's
// floor, and the restart itself spends no attempt, because the rung the smoke
// booked is still in the future.
func TestCodexPostUpdate_GreenSmokeThenUpdateThenRolloutLands(t *testing.T) {
	f, path := codexSmokeFreshnessFixture(t)
	f.seedPreRunReading(t, time.Now().Add(-time.Hour), time.Now())
	// A first rung far enough out that it is still in the FUTURE when the restart
	// replays — which is the point being asserted. With the fixture's 10 ms rung
	// the replay would legitimately find it due and reconcile.
	prevLadder := codexRunDebtRetryLadder
	codexRunDebtRetryLadder = []time.Duration{time.Minute, time.Minute, time.Minute, time.Minute}
	t.Cleanup(func() { codexRunDebtRetryLadder = prevLadder })
	stubCodexSmokeExec(t, func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
		return codexPreUpdateFrames(codexMarkerFromLaunch(t, launch)), nil, nil
	})
	// No live read is reachable in this row: the rollout is what pays it.
	stubCodexFallbackRead(t, func(context.Context, string) string { return liveProbeOutcomeNoReading })

	before := time.Now()
	if result := runCodexSmoke(context.Background(), path, codexSmokeTestVersion); result.Status != cliSmokeStatusSuccess {
		t.Fatalf("result = %+v, want the green smoke the bug report describes", result)
	}
	// Durability WITHOUT waiting for the workers: the debt and its first rung are
	// on disk the instant runCodexSmoke returns, so an update handoff right here
	// cannot take them with it. Read the cache directly — going through
	// waitIdle() would pass even if persistence moved back onto the goroutine.
	settled := f.snapshot(t)
	if settled.RefreshOwedAtMs == 0 || settled.RunFloorMs < before.UnixMilli() {
		t.Fatalf("the smoke must persist its debt synchronously: %+v", settled)
	}
	if settled.NextAttemptAtMs == 0 {
		t.Fatalf("the smoke must book its first rung synchronously: %+v", settled)
	}

	// The self-update replaces the process. Only the cache file carries over.
	// The baseline is read AFTER the reset, which drains the settle's own async
	// worker — whatever that worker spent is not the restart's doing.
	simulateCodexAgentRestart(t)
	baseline := f.snapshot(t)
	if baseline.NextAttemptAtMs <= time.Now().UnixMilli() {
		t.Fatalf("this row needs the booked rung still in the future: %+v", baseline)
	}
	// Restarted repeatedly inside the first rung — a device rebooted a few times
	// before Codex flushed its telemetry — the whole scan budget must survive.
	for i := 0; i < 5; i++ {
		payOwedCodexUsageRefresh()
		waitCodexUsageRefreshIdle(t)
		if got := f.snapshot(t).RefreshOwedAttempts; got != baseline.RefreshOwedAttempts {
			t.Fatalf("restart %d spent an attempt (%d, was %d); a future rung must simply be re-armed",
				i+1, got, baseline.RefreshOwedAttempts)
		}
		simulateCodexAgentRestart(t)
	}

	// Codex finally flushes the run's telemetry. Back to a short ladder so the
	// remaining rungs run at test speed.
	codexRunDebtRetryLadder = prevLadder
	floor := time.UnixMilli(settled.RunFloorMs)
	writeCodexRunRollout(t, f.home, "post-update", floor, floor.Add(time.Second), floor.Add(2*time.Second), true,
		[]map[string]any{codexRateLimitFrame(58, 59, time.Now())})
	codexRunDebtWorker(f.home, f.fp)
	drainCodexRunDebtLadder(t)

	if got := codexObservedAt(t, f); got.UnixMilli() < settled.RunFloorMs {
		t.Fatalf("observedAt %s still predates the smoke's floor %s", got, floor)
	}
	if snap := f.snapshot(t); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("the debt must clear once its telemetry is found: %+v", snap)
	}
}

// The same reported case with the rollout NEVER landing: the live read pays it
// instead, and the restart still costs no attempt.
func TestCodexPostUpdate_GreenSmokeThenUpdateThenLiveReadPays(t *testing.T) {
	f, path := codexSmokeFreshnessFixture(t)
	f.seedPreRunReading(t, time.Now().Add(-time.Hour), time.Now())
	stubCodexSmokeExec(t, func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
		return codexPreUpdateFrames(codexMarkerFromLaunch(t, launch)), nil, nil
	})
	calls := stubCodexFallbackRead(t, capturingFallbackRead)

	if result := runCodexSmoke(context.Background(), path, codexSmokeTestVersion); result.Status != cliSmokeStatusSuccess {
		t.Fatalf("result = %+v, want success", result)
	}
	settled := f.snapshot(t)
	if settled.RefreshOwedAtMs == 0 {
		t.Fatalf("the smoke must persist its debt synchronously: %+v", settled)
	}

	simulateCodexAgentRestart(t)
	payOwedCodexUsageRefresh()
	drainCodexRunDebtLadder(t)

	if *calls == 0 {
		t.Fatal("with no rollout evidence the live read must pay the debt")
	}
	if got := codexObservedAt(t, f); got.UnixMilli() < settled.RunFloorMs {
		t.Fatalf("observedAt %s still predates the smoke's floor %d", got, settled.RunFloorMs)
	}
	if snap := f.snapshot(t); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("the live read must clear the debt: %+v", snap)
	}
	usage, _ := codexUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{}, time.Now())
	if usage.Notice != "" {
		t.Fatalf("a refreshed card must carry no notice: %q", usage.Notice)
	}
}

// A run nothing armed: the user ran `codex` in their own shell, so no spawn path
// classified it and no debt exists. The gather's nudge creates one from the newest
// account-eligible rollout mtime the reconcile already stat-ed, and it converges.
func TestCodexPostUpdate_UnmanagedRunConvergesThroughTheNudge(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	observed := now.Add(-time.Hour)
	f.seedPreRunReading(t, observed, now)
	// Written by a `codex` this agent never spawned, long enough ago that it is
	// not still being appended to.
	rollout := now.Add(-2 * codexForcedReconcileMinInterval).Truncate(time.Millisecond)
	writeCodexRunRollout(t, f.home, "own-shell", now.Add(-5*time.Minute), rollout, rollout, true,
		[]map[string]any{codexRateLimitFrame(71, 72, rollout)})

	// Nothing is armed and no debt exists.
	if state := codexRunFreshnessForAccount(f.fp, now); state.owed || state.interrupted {
		t.Fatalf("this row must start with nothing owed: %+v", state)
	}
	// The gather: reconcile, then nudge with the mtime it saw.
	res := codexReconcileForGather(context.Background(), f.home, f.fp, now, false)
	nudgeCodexUsageRefresh(f.home, f.fp, now, res.newestRollout, res.latestObservation)
	drainCodexRunDebtLadder(t)

	if got := codexObservedAt(t, f); !got.After(observed) {
		t.Fatalf("observedAt %s did not advance past the pre-run reading %s", got, observed)
	}
}
