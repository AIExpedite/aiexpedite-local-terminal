package main

import (
	"context"
	"os"
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
	// Durability WITHOUT waiting for the workers: the DEBT is on disk the instant
	// runCodexSmoke returns, so an update handoff right here cannot take it with
	// it. Read the cache directly — going through waitIdle() would pass even if
	// persistence moved back onto the goroutine.
	//
	// Only the debt is asserted here, not the rung: the asynchronous settle is
	// racing the synchronous persist for the same run, and whichever lands second
	// legitimately replaces the generation. The rung is asserted below, once the
	// workers have drained.
	settled := f.snapshot(t)
	if settled.RefreshOwedAtMs == 0 || settled.RunFloorMs < before.UnixMilli() {
		t.Fatalf("the smoke must persist its debt synchronously: %+v", settled)
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
	nudgeCodexUsageRefresh(f.home, f.fp, now, res.rollouts, res.latestObservation)
	drainCodexRunDebtLadder(t)

	if got := codexObservedAt(t, f); !got.After(observed) {
		t.Fatalf("observedAt %s did not advance past the pre-run reading %s", got, observed)
	}
}

// The migration's whole point, end to end: a cache written by the PREVIOUS agent
// version, whose debt was retired offline (`skipped`, which used to be final),
// must become payable again on the very upgrade that ships this fix. Mapping it
// to `exhausted` would carry the reported bug across that upgrade — the debt
// would be permanently unpayable on every cache written before it.
func TestCodexPostUpdate_LegacyOfflineRetiredDebtIsPayableAfterTheUpgrade(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-2 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	calls := stubCodexFallbackRead(t, capturingFallbackRead)
	// The state the old agent left: every rollout scan spent and the fallback
	// resolved to the old, terminal "skipped" because the device was offline.
	if !codexRecordRunFreshness(f.fp, now.Add(-time.Minute), func(snap *codexRateLimitSnapshot) {
		codexOweRunRefresh(snap, runStart, now.Add(-time.Minute))
		snap.RefreshOwedAttempts = codexRefreshAfterRunMaxAttempts
		snap.RefreshFallbackState = codexLegacyFallbackSkipped
	}) {
		t.Fatal("seeding the legacy debt failed")
	}

	// The upgraded agent starts and replays the debt.
	simulateCodexAgentRestart(t)
	payOwedCodexUsageRefresh()
	drainCodexRunDebtLadder(t)

	if *calls == 0 {
		t.Fatal("a legacy offline-retired debt must be payable again after the upgrade")
	}
	if got := codexObservedAt(t, f); got.Before(runStart.Truncate(time.Second)) {
		t.Fatalf("observedAt %s still predates the run %s", got, runStart)
	}
	if snap := f.snapshot(t); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("the migrated debt must be paid, not stuck: %+v", snap)
	}
	usage, _ := codexUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{}, time.Now())
	if usage.Notice != "" {
		t.Fatalf("a paid card must carry no notice: %q", usage.Notice)
	}
}

// The other legacy value: "spent" proved exactly ONE outbound read, so the
// upgrade must leave the second read of the new budget available rather than
// treating the debt as finished.
func TestCodexPostUpdate_LegacySpentDebtGetsItsRemainingRead(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-2 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	calls := stubCodexFallbackRead(t, func(context.Context, string) string { return liveProbeOutcomeNoReading })
	if !codexRecordRunFreshness(f.fp, now.Add(-time.Minute), func(snap *codexRateLimitSnapshot) {
		codexOweRunRefresh(snap, runStart, now.Add(-time.Minute))
		snap.RefreshOwedAttempts = codexRefreshAfterRunMaxAttempts
		snap.RefreshFallbackState = codexLegacyFallbackSpent
	}) {
		t.Fatal("seeding the legacy debt failed")
	}

	simulateCodexAgentRestart(t)
	payOwedCodexUsageRefresh()
	drainCodexRunDebtLadder(t)

	// Exactly the one read the new budget still had, then exhausted.
	want := codexRefreshLiveReadMaxAttempts - 1
	if int(*calls) != want {
		t.Fatalf("live reads = %d, want the %d the legacy debt had left", *calls, want)
	}
	snap := f.snapshot(t)
	if snap.RefreshLiveReads != codexRefreshLiveReadMaxAttempts {
		t.Fatalf("read counter = %d, want the budget fully spent (%d)", snap.RefreshLiveReads, codexRefreshLiveReadMaxAttempts)
	}
	if snap.RefreshFallbackState != codexFallbackExhausted {
		t.Fatalf("fallback = %q, want exhausted once the migrated budget is spent", snap.RefreshFallbackState)
	}
	// And the card now explains itself rather than looking current.
	if codexStaleRunNotice(codexRunFreshnessForAccount(f.fp, time.Now())) == "" {
		t.Fatal("a debt that ran out of everything must warn")
	}
}

// A credentials change between the probe arming and its settle must DROP the
// debt, not book it. Under the live account it would show a stale-run warning for
// a run that account never made; under the armed one it would rescope the cache
// and discard the live account's readings. The synchronous persist has to apply
// the same rule codexRefreshAfterRun does, since it writes outside that path.
func TestPersistCodexSmokeRunDebt_DropsTheDebtWhenTheAccountChanged(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	// The account live NOW is not the one the probe armed under.
	armedByAnotherAccount := "codex-account-that-signed-out"

	persistCodexSmokeRunDebt(now.Add(-time.Minute), armedByAnotherAccount)

	if snap := f.snapshot(t); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("a run made under another account must not owe a debt here: %+v", snap)
	}

	// The matching account still books it, so the guard is not simply refusing
	// everything.
	persistCodexSmokeRunDebt(now.Add(-time.Minute), f.fp)
	if snap := f.snapshot(t); snap.RefreshOwedAtMs == 0 {
		t.Fatalf("the arming account's own run must still be recorded: %+v", snap)
	}
	drainCodexRunDebtLadder(t)
}

// The run-lifecycle seam exists so a test can observe arm/settle WITHOUT any
// cache write. The synchronous persist runs beside codexUsageRunSettled, so it
// has to honour that seam too or a recorder would see clean lifecycle counts
// while the cache was written behind it.
func TestPersistCodexSmokeRunDebt_StandsDownForAStubbedLifecycle(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	before, err := os.ReadFile(f.cache)
	if err != nil {
		t.Fatal(err)
	}
	// The package's own recorder, which restores whatever override was in place
	// rather than clobbering it with nil.
	rec := recordCodexRunHooks(t)

	codexUsageRunSettled(now.Add(-time.Minute))
	persistCodexSmokeRunDebt(now.Add(-time.Minute), f.fp)

	if _, settled, _ := rec.lifecycle(); settled != 1 {
		t.Fatalf("the recorder must still observe the settle: settled=%d", settled)
	}

	after, err := os.ReadFile(f.cache)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a stubbed lifecycle must not have the cache written behind it")
	}
}

/* ───────────── adversarial-review regressions (secondary pass) ───────────── */

// A shutdown during an OUTSTANDING live read must not exhaust the debt. The read
// is cancelled, so nothing was spent; persisting `exhausted` there made the next
// process refuse the debt outright, defeating update survival on the one path —
// gracefulShutdown, which an update handoff goes through — where it matters most.
func TestCodexPostUpdate_ShutdownDuringALiveReadKeepsTheDebtPayable(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-2 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)

	// The read blocks until the process starts shutting down, then reports the
	// cancellation the real probe reports.
	entered := make(chan struct{})
	reads := stubCodexFallbackRead(t, func(ctx context.Context, fp string) string {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-ctx.Done()
		return liveProbeOutcomeTimeout
	})
	state := f.oweExhaustedDebt(t, runStart, now.Add(-time.Minute))

	done := make(chan codexRunDebtRetryKind, 1)
	go func() { done <- codexLiveUsageFallback(f.fp, state) }()
	<-entered
	// Cancel the in-flight read the way a shutdown (or a gate reset) does. The
	// gate's cancel channel is guarded by its mutex, so this is safe to drive
	// from a test — unlike the process-wide shutdownChan, which production closes
	// exactly once and never reassigns.
	cancelCodexRefreshInFlight(t)
	if kind := <-done; kind == codexRetryNone {
		t.Fatal("a cancelled read spent nothing and must report a retryable kind")
	}
	restoreCodexRefreshCancel(t)

	// The worker's retirement then runs the resolve. While the process is going
	// down it must write nothing rather than persist a terminal verdict.
	shutdownInProgress.Store(true)
	t.Cleanup(func() { shutdownInProgress.Store(false) })
	codexResolveOutstandingFallback(f.fp, false)

	snap := f.snapshot(t)
	if snap.RefreshFallbackState == codexFallbackExhausted {
		t.Fatalf("a cancelled read must not exhaust the debt: %+v", snap)
	}
	if snap.RefreshLiveReads != 0 {
		t.Fatalf("a cancelled read charged %d reads", snap.RefreshLiveReads)
	}
	if *reads != 1 {
		t.Fatalf("reads = %d, want the one that was cancelled", *reads)
	}

	// The replacement process: the debt is still payable and a read advances it.
	shutdownInProgress.Store(false)
	simulateCodexAgentRestart(t)
	stubCodexFallbackRead(t, capturingFallbackRead)
	codexRunDebtWorker(f.home, f.fp)
	drainCodexRunDebtLadder(t)

	if got := codexObservedAt(t, f); got.Before(runStart.Truncate(time.Second)) {
		t.Fatalf("observedAt %s still predates the run %s after the restart", got, runStart)
	}
	if snap := f.snapshot(t); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("the restarted process must be able to pay the debt: %+v", snap)
	}
}

// An unmanaged run whose rollout carries NO readable utilization must still
// converge. The reconcile reports that rollout about once — its cursor then treats
// the file as consumed and unchanged — so a nudge that declined the report for
// being too young used to throw away the run's only trigger, leaving the card
// stale with no debt, no fallback and no warning.
//
// Two consecutive gathers over the SAME unchanged rollout: the first inside the
// age threshold (declined), the second past it (must act).
func TestCodexPostUpdate_UnmanagedRunSurvivesAnEarlyDeclinedNudge(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	observed := now.Add(-time.Hour)
	f.seedPreRunReading(t, observed, now)
	calls := stubCodexFallbackRead(t, capturingFallbackRead)
	// A rollout with a session header and NO usable rate-limit frame: exactly the
	// post-upgrade shape the scan cannot mine.
	rollout := now.Truncate(time.Millisecond)
	writeCodexRunRollout(t, f.home, "own-shell-unreadable", now.Add(-5*time.Minute), time.Time{}, rollout, false, nil)

	// Gather 1, immediately after the write: the reconcile reports the mtime, the
	// nudge declines it as possibly still being appended to.
	first := codexReconcileForGather(context.Background(), f.home, f.fp, now, false)
	if nudgeCodexUsageRefresh(f.home, f.fp, now, first.rollouts, first.latestObservation) {
		t.Fatal("a rollout younger than the minimum interval must be declined")
	}
	if snap := f.snapshot(t); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("nothing should be owed yet: %+v", snap)
	}

	// Gather 2, past the age threshold. The file is unchanged and the cursor has
	// consumed it, so the reconcile reports nothing this time — the retained
	// evidence is all that is left.
	// No resetCodexRefreshNudge here: it would clear the retained evidence that is
	// the whole point of this test. The first nudge declined before reaching its
	// transaction, so it consumed no cooldown either.
	later := now.Add(2 * codexForcedReconcileMinInterval)
	second := codexReconcileForGather(context.Background(), f.home, f.fp, later, false)
	if !second.rollouts.newest.IsZero() {
		t.Logf("note: the reconcile still reported %s; the retained path is exercised when it reports zero", second.rollouts.newest)
	}
	if !nudgeCodexUsageRefresh(f.home, f.fp, later, second.rollouts, second.latestObservation) {
		t.Fatal("the run's evidence was lost: the second gather nudged nothing")
	}
	drainCodexRunDebtLadder(t)

	if *calls == 0 {
		t.Fatal("the debt's live fallback never ran, so the card would stay stale in silence")
	}
	if got := codexObservedAt(t, f); !got.After(observed) {
		t.Fatalf("observedAt %s did not advance past the pre-run reading %s", got, observed)
	}
}

// The live-read counter must survive a refused write. codexRecordLiveReadOutcome
// used to ignore its transaction result, so a read that reached OpenAI but whose
// counter write the bounded cache locks refused vanished — and the schedule picks
// the next rung off RefreshLiveReads on DISK, so the debt looked as though it had
// budget left. The count is retained by generation and folded into the next write.
func TestCodexRecordLiveReadOutcome_RetainsARefusedCount(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	state := f.oweExhaustedDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute))
	id := state.debtID()

	prevWait, prevPoll := codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll
	codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = 20*time.Millisecond, time.Millisecond
	t.Cleanup(func() { codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = prevWait, prevPoll })

	// One outbound read whose counter write is refused.
	func() {
		codexRateLimitMu.Lock()
		defer codexRateLimitMu.Unlock()
		codexRecordLiveReadOutcome(f.fp, id, liveProbeOutcomeRPCError, codexRetryAfterRead)
	}()
	if got := f.snapshot(t).RefreshLiveReads; got != 0 {
		t.Fatalf("precondition: the refused write must leave the counter at 0, got %d", got)
	}
	if got := codexUsageRefresh.peekLiveReads(f.fp, id); got != 1 {
		t.Fatalf("retained reads = %d, want the refused one held for this generation", got)
	}

	// The next write folds it in, so the debt is charged what it actually spent.
	codexRecordLiveReadOutcome(f.fp, id, liveProbeOutcomeRPCError, codexRetryAfterRead)
	snap := f.snapshot(t)
	if snap.RefreshLiveReads != codexRefreshLiveReadMaxAttempts {
		t.Fatalf("RefreshLiveReads = %d, want the refused read folded in (%d)",
			snap.RefreshLiveReads, codexRefreshLiveReadMaxAttempts)
	}
	if snap.RefreshFallbackState != codexFallbackExhausted {
		t.Fatalf("fallback = %q, want exhausted once the folded budget is spent", snap.RefreshFallbackState)
	}
	if got := codexUsageRefresh.peekLiveReads(f.fp, id); got != 0 {
		t.Fatalf("a folded count must be consumed, %d still retained", got)
	}
}

// A count retained for a generation the debt has moved past is never charged to
// the new one — the same rule the scan counter follows.
func TestCodexRecordLiveReadOutcome_RetainedCountIsPerGeneration(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	stale := codexDebtID{floorMs: now.Add(-time.Hour).UnixMilli(), owedAtMs: now.Add(-time.Hour).UnixMilli()}
	codexUsageRefresh.rememberLiveReads(f.fp, stale, codexRefreshLiveReadMaxAttempts)

	fresh := f.oweExhaustedDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute))
	if got := codexUsageRefresh.peekLiveReads(f.fp, fresh.debtID()); got != 0 {
		t.Fatalf("a newer debt inherited %d reads from a generation it replaced", got)
	}
}

// The retained count must GATE the next read, not merely be recorded: that is what
// keeps the promised two outbound reads per debt honest when the counter writes
// are being refused. Seeded through the gate, which is exactly the state a refused
// write leaves, so this needs no wedged cache of its own.
func TestCodexLiveFallback_RetainedReadsBoundTheBudget(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	reads := stubCodexFallbackRead(t, func(context.Context, string) string { return liveProbeOutcomeRPCError })
	state := f.oweExhaustedDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute))

	// The whole budget spent, but none of it on disk — every counter write was
	// refused.
	codexUsageRefresh.rememberLiveReads(f.fp, state.debtID(), codexRefreshLiveReadMaxAttempts)
	if got := f.snapshot(t).RefreshLiveReads; got != 0 {
		t.Fatalf("precondition: the counter on disk must still read 0, got %d", got)
	}

	codexLiveUsageFallback(f.fp, state)

	if *reads != 0 {
		t.Fatalf("reads = %d, want none: the retained count says the budget is spent", *reads)
	}
	if got := codexRunFreshnessForAccount(f.fp, time.Now()).fallback; got != codexFallbackExhausted {
		t.Fatalf("fallback = %q, want exhausted", got)
	}
}

// Prior finding 2, the part memory could not fix: the agent RESTARTS between the
// declined nudge and the later gather. The scan cursor has consumed the rollout,
// so no later reconcile will report it again — the evidence has to be on DISK or
// the run loses its only trigger and the card stays stale in silence.
func TestCodexPostUpdate_DeferredRolloutEvidenceSurvivesARestart(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	observed := now.Add(-time.Hour)
	f.seedPreRunReading(t, observed, now)
	calls := stubCodexFallbackRead(t, capturingFallbackRead)
	// An unmanaged run whose rollout carries no readable utilization.
	rollout := now.Truncate(time.Millisecond)
	writeCodexRunRollout(t, f.home, "own-shell-unreadable", now.Add(-5*time.Minute), time.Time{}, rollout, false, nil)

	// Gather 1: reported, then declined for being too young to judge.
	first := codexReconcileForGather(context.Background(), f.home, f.fp, now, false)
	if nudgeCodexUsageRefresh(f.home, f.fp, now, first.rollouts, first.latestObservation) {
		t.Fatal("a rollout younger than the minimum interval must be declined")
	}
	if got := f.snapshot(t).PendingRolloutMtimeMs; got != rollout.UnixMilli() {
		t.Fatalf("the declined report must be PERSISTED, got %d want %d", got, rollout.UnixMilli())
	}

	// The agent is replaced — a self-update, which is the likeliest restart and
	// happens right after a smoke. Only the cache file carries over.
	simulateCodexAgentRestart(t)

	// Gather 2, past the age threshold. The file is unchanged and consumed, so the
	// reconcile reports nothing: the persisted evidence is all that is left.
	later := now.Add(2 * codexForcedReconcileMinInterval)
	second := codexReconcileForGather(context.Background(), f.home, f.fp, later, false)
	if !nudgeCodexUsageRefresh(f.home, f.fp, later, second.rollouts, second.latestObservation) {
		t.Fatal("the run's evidence did not survive the restart: the second gather nudged nothing")
	}
	drainCodexRunDebtLadder(t)

	if *calls == 0 {
		t.Fatal("the debt's live fallback never ran, so the card would stay stale in silence")
	}
	if got := codexObservedAt(t, f); !got.After(observed) {
		t.Fatalf("observedAt %s did not advance past the pre-run reading %s", got, observed)
	}
	if got := f.snapshot(t).PendingRolloutMtimeMs; got != 0 {
		t.Fatalf("evidence acted on must be released, %d still held", got)
	}
}

// Prior finding 2's release must be atomic with the debt it hands off to. The
// evidence is cleared INSIDE the mutation, so a transaction that does not commit
// leaves both the evidence and the absence of a debt untouched — rather than
// dropping the only trigger for a rollout the scan cursor already consumed.
func TestCodexPostUpdate_AFailedNudgeWriteKeepsTheRolloutEvidence(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	observed := now.Add(-time.Hour)
	f.seedPreRunReading(t, observed, now)
	rollout := now.Add(-2 * codexForcedReconcileMinInterval).Truncate(time.Millisecond)
	// Evidence already recorded by an earlier, declined gather.
	codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		snap.PendingRolloutMtimeMs = rollout.UnixMilli()
	})

	// A TEMPORARY commit failure, after the mutation has already run: the nudge's
	// own write fails, anything after it succeeds. That is what made the old
	// caller-side release unsafe — the callback had set its flag, the caller
	// ignored the transaction result, and its follow-up clear committed happily.
	original := codexCommitRateLimitSnapshot
	failedOnce := false
	codexCommitRateLimitSnapshot = func(p string, out []byte, at time.Time) bool {
		if !failedOnce {
			failedOnce = true
			return false
		}
		return original(p, out, at)
	}
	nudgeCodexUsageRefresh(f.home, f.fp, now, codexRolloutNudgeEvidence{newest: rollout}, observed)
	drainCodexRunDebtLadder(t)
	codexCommitRateLimitSnapshot = original
	if !failedOnce {
		t.Fatal("the nudge never attempted a commit, so nothing was exercised")
	}

	snap := f.snapshot(t)
	if snap.PendingRolloutMtimeMs != rollout.UnixMilli() {
		t.Fatalf("a nudge whose write failed must keep the evidence: %+v", snap)
	}
	if snap.RefreshOwedAtMs != 0 {
		t.Fatalf("no debt can exist when the write did not commit: %+v", snap)
	}

	// With the cache writable the same evidence still converges.
	resetCodexRefreshNudge()
	if !nudgeCodexUsageRefresh(f.home, f.fp, now, codexRolloutNudgeEvidence{}, observed) {
		t.Fatal("the retained evidence must still create the debt on a later gather")
	}
	after := f.snapshot(t)
	if after.RefreshOwedAtMs == 0 || after.RunFloorMs != rollout.UnixMilli() {
		t.Fatalf("the debt must be floored at the retained rollout: %+v", after)
	}
	if after.PendingRolloutMtimeMs != 0 {
		t.Fatalf("evidence handed to a debt must be released in that same write: %+v", after)
	}
	drainCodexRunDebtLadder(t)
}

// A rollout mtime dated in the FUTURE (a clock correction, or restored file
// metadata) must not be held raw. Held raw it is the newest value for as long as
// the clock takes to catch up, and `settled` can never become true for it — so
// every eligible run behind it is blocked from the fallback. It is CORRECTED
// instead, which both keeps the trigger and lets it act.
func TestCodexPostUpdate_AFutureDatedRolloutIsCorrectedNotHeldRaw(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	observed := now.Add(-time.Hour)
	f.seedPreRunReading(t, observed, now)

	// A report dated hours ahead.
	future := now.Add(3 * time.Hour)
	nudgeCodexUsageRefresh(f.home, f.fp, now, codexRolloutNudgeEvidence{newest: future}, observed)

	snap := f.snapshot(t)
	// Nothing future-dated may reach disk, in either field: a held mtime ahead of
	// the clock shadows every later report, and a floor ahead of it asks for
	// telemetry no observation can reach.
	if snap.PendingRolloutMtimeMs > now.UnixMilli() {
		t.Fatalf("a future-dated mtime was stored raw (%d > %d)", snap.PendingRolloutMtimeMs, now.UnixMilli())
	}
	if snap.RunFloorMs > now.UnixMilli() {
		t.Fatalf("debt floored ahead of the clock: %d > %d", snap.RunFloorMs, now.UnixMilli())
	}
	// And the trigger must not be LOST: the correction makes the report actionable,
	// so either a debt already carries it or the evidence is still held for one.
	if snap.RefreshOwedAtMs == 0 && snap.PendingRolloutMtimeMs == 0 {
		t.Fatalf("the trigger was dropped; the file was still written: %+v", snap)
	}
	drainCodexRunDebtLadder(t)
}

// Prior finding 3's remaining gap: evidence recorded BEFORE a backwards clock
// step is itself future-dated afterwards. It must be detected by the rollback
// repair, corrected rather than dropped, and must not shadow a later valid report.
//
// The whole scenario is built in the ROLLED-BACK timeframe so the cache stays
// self-consistent — a reading dated after the corrected clock would be pulled back
// by the repair and would then legitimately cover the rollout, which is a
// different behaviour from shadowing.
func TestCodexPostUpdate_EvidenceLeftAheadByAClockRollbackRecovers(t *testing.T) {
	real := time.Now()
	rolledBack := real.Add(-4 * time.Hour)
	f := newCodexFreshnessFixture(t, rolledBack.Add(-5*time.Hour))
	observed := rolledBack.Add(-2 * time.Hour)
	f.seedPreRunReading(t, observed, rolledBack)

	// Evidence recorded while the clock still read `real` — ahead of the corrected
	// clock by hours.
	codexRecordRunFreshness(f.fp, rolledBack, func(snap *codexRateLimitSnapshot) {
		snap.PendingRolloutMtimeMs = real.UnixMilli()
	})

	view := codexCacheViewForAccount(f.fp)
	if !codexRunFreshnessInFuture(view, rolledBack) {
		t.Fatal("evidence left ahead of the clock must be detected as a rollback to repair")
	}
	if !codexRebaseFutureRunFreshnessForAccount(f.fp, view, rolledBack) {
		t.Fatal("the repair must run")
	}
	repaired := f.snapshot(t).PendingRolloutMtimeMs
	if repaired == 0 {
		t.Fatal("the evidence was dropped; the rollout was still written, so the trigger is worth keeping")
	}
	if repaired > rolledBack.UnixMilli() {
		t.Fatalf("repaired evidence %d is still ahead of the corrected clock %d",
			repaired, rolledBack.UnixMilli())
	}

	// It must now be able to act, on the very next nudge, rather than shadowing
	// itself for another settle window.
	resetCodexRefreshNudge()
	if !nudgeCodexUsageRefresh(f.home, f.fp, rolledBack, codexRolloutNudgeEvidence{}, observed) {
		t.Fatal("the repaired evidence could not act; recovery is still shadowed")
	}
	snap := f.snapshot(t)
	if snap.RefreshOwedAtMs == 0 {
		t.Fatalf("the repaired evidence must own a debt: %+v", snap)
	}
	if snap.RunFloorMs > rolledBack.UnixMilli() {
		t.Fatalf("the debt floor %d is ahead of the corrected clock %d", snap.RunFloorMs, rolledBack.UnixMilli())
	}
	if snap.PendingRolloutMtimeMs != 0 {
		t.Fatalf("evidence handed to a debt must be released: %+v", snap)
	}
	drainCodexRunDebtLadder(t)
}

// And a later, NEWER report is still taken as fresh rather than being shadowed by
// evidence a rollback left ahead — the shadowing mechanism itself, asserted
// directly.
func TestCodexNudgeRolloutEvidence_FutureHeldValueCannotShadowANewerReport(t *testing.T) {
	now := time.Now()
	ahead := codexCacheView{pendingRolloutMs: now.Add(2 * time.Hour).UnixMilli()}
	// Newer than the corrected held value (now - the settle window) and not itself
	// in the future. Held RAW, the +2h value would make this — and every other
	// non-future report — non-fresh for two hours.
	reported := now.Add(-time.Second).Truncate(time.Millisecond)

	evidence, fresh := codexNudgeRolloutEvidence(ahead, reported, now)

	if !fresh {
		t.Fatalf("a report newer than the CORRECTED held value must read as fresh; evidence=%s", evidence)
	}
	if !evidence.Equal(reported) {
		t.Fatalf("evidence = %s, want the report %s", evidence, reported)
	}
	// With nothing reported, the corrected held value stands and is usable.
	held, fresh := codexNudgeRolloutEvidence(ahead, time.Time{}, now)
	if fresh {
		t.Fatal("a held value is not a fresh report")
	}
	if held.After(now) {
		t.Fatalf("held evidence %s is still ahead of now %s", held, now)
	}
}

// Prior finding 1's remaining gap: the evidence must be written by the SAME
// transaction that advances the cursor over the rollout. Here the reconcile
// commits and then EVERY later write fails — a refused nudge write, or the process
// exiting between the two transactions — and the run must still converge, because
// the reconcile itself already recorded that something owes attention.
func TestCodexPostUpdate_EvidenceIsAtomicWithTheCursorThatConsumesIt(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	observed := now.Add(-time.Hour)
	f.seedPreRunReading(t, observed, now)
	calls := stubCodexFallbackRead(t, capturingFallbackRead)
	rollout := now.Truncate(time.Millisecond)
	writeCodexRunRollout(t, f.home, "own-shell-unreadable", now.Add(-5*time.Minute), time.Time{}, rollout, false, nil)

	// The reconcile runs and commits. Its own write must carry the evidence.
	first := codexReconcileForGather(context.Background(), f.home, f.fp, now, false)
	if got := f.snapshot(t).PendingRolloutMtimeMs; got != rollout.UnixMilli() {
		t.Fatalf("the reconcile must record the evidence in its OWN write, got %d want %d",
			got, rollout.UnixMilli())
	}

	// Everything after it fails, which stands in for the process exiting between
	// the two transactions.
	original := codexCommitRateLimitSnapshot
	codexCommitRateLimitSnapshot = func(string, []byte, time.Time) bool { return false }
	nudgeCodexUsageRefresh(f.home, f.fp, now, first.rollouts, first.latestObservation)
	codexCommitRateLimitSnapshot = original
	if got := f.snapshot(t).PendingRolloutMtimeMs; got != rollout.UnixMilli() {
		t.Fatalf("the evidence must survive a nudge whose writes all failed, got %d", got)
	}

	// Restart, then a later gather: the file is unchanged and consumed, so the
	// reconcile reports nothing. Only the persisted evidence remains.
	simulateCodexAgentRestart(t)
	later := now.Add(2 * codexForcedReconcileMinInterval)
	second := codexReconcileForGather(context.Background(), f.home, f.fp, later, false)
	if !nudgeCodexUsageRefresh(f.home, f.fp, later, second.rollouts, second.latestObservation) {
		t.Fatal("the run lost its trigger: the later gather nudged nothing")
	}
	drainCodexRunDebtLadder(t)

	if *calls == 0 {
		t.Fatal("the fallback never ran, so the card would stay stale in silence")
	}
	if got := codexObservedAt(t, f); !got.After(observed) {
		t.Fatalf("observedAt %s did not advance past the pre-run reading %s", got, observed)
	}
}
