package main

import (
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   session_codex_rollout_propagation_test.go — the terminal-managed path end to
   end on the device. Current `codex exec --json` prints no rate-limit frames,
   so a terminal session's reading only exists in its rollout: the post-run
   debt pays from it (or, when a mid-run auth.json rewrite fences it, from the
   pinned early live read), one hint goes out, and the receipt it asks for is
   numeric. The session manager arms and settles through
   codexUsageRunStarted / codexUsageRunSettled, which is what these rows drive.
   ------------------------------------------------------------------------ */

func TestCodexTerminalSession_RolloutPaysAndPropagates(t *testing.T) {
	withCodexGenerationEpoch(t, 7201)
	now := time.Now()
	runStart := now.Add(-2 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	stubCodexLogin(t, true, true)
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	codexRotateGenerationEpoch(now)
	rec, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)
	waitHints(t, rec, 2, 0) // the startup recovery hint and its follow-up
	baseline := len(rec.all())

	codexUsageRunStarted(runStart)
	writeCodexRunRollout(t, f.home, "exec-json", runStart, runStart.Add(time.Minute), runStart.Add(70*time.Second), true,
		[]map[string]any{codexRateLimitFrameWithLimitID("codex", 44, 66, now)})
	codexUsageRunSettled(runStart)
	drainCodexRunDebtLadder(t)

	if snap := f.snapshot(t); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("the rollout must pay the debt: %+v", snap)
	}
	hints := waitHints(t, rec, baseline+1, 0)
	hint := hints[len(hints)-1].hint
	agent := codexRefreshReceiptAgent(t, f)
	assertNumericCodexMetrics(t, agent)
	if s := codexSessionMetric(t, agent.Metrics); s.Consumed == nil || *s.Consumed != 44 {
		t.Fatalf("session = %+v, want the run's 44%%", s)
	}
	if agent.UsageGeneration == nil || agent.UsageGeneration.Counter < hint.Generation {
		t.Fatalf("receipt generation %+v predates the hint {%d,%d}", agent.UsageGeneration, hint.GenerationEpoch, hint.Generation)
	}
}

// A same-account auth.json rewrite mid-run fences the run's rollout; the
// stubbed live read pays instead. A failed hint send still earns its follow-up.
func TestCodexTerminalSession_FencedRolloutPaysByLiveReadAndStillPropagates(t *testing.T) {
	withCodexGenerationEpoch(t, 7301)
	now := time.Now()
	runStart := now.Add(-2 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	stubCodexLogin(t, true, true)
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	codexRotateGenerationEpoch(now)
	calls := stubCodexFallbackRead(t, capturingFallbackRead)
	rec, cfg := propagatorFixture(t)
	rec.status = func(cliUsageObservedHint) int { return 500 }
	startCLIUsagePropagator(cfg)
	waitHints(t, rec, 2, 0) // the startup recovery hint and its follow-up
	baseline := len(rec.all())

	codexUsageRunStarted(runStart)
	writeCodexRunRollout(t, f.home, "exec-json", runStart, runStart.Add(time.Minute), runStart.Add(70*time.Second), true,
		[]map[string]any{codexRateLimitFrameWithLimitID("codex", 44, 66, now)})
	helperCodexAuthAt(t, f.home, "dev@example.com", runStart.Add(30*time.Second))
	codexUsageRunSettled(runStart)
	drainCodexRunDebtLadder(t)

	if *calls != 1 {
		t.Fatalf("live reads = %d, want the one early read", *calls)
	}
	if snap := f.snapshot(t); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("the live read must pay the debt: %+v", snap)
	}
	hints := waitHints(t, rec, baseline+2, 0)
	if last, prev := hints[len(hints)-1].hint, hints[len(hints)-2].hint; last.Generation != prev.Generation {
		t.Fatalf("the follow-up must restate the failed hint's generation: %+v then %+v", prev, last)
	}
	agent := codexRefreshReceiptAgent(t, f)
	assertNumericCodexMetrics(t, agent)
}
