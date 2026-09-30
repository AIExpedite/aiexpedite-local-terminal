package main

import (
	"context"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_smoke_codex_update_handoff_test.go — the reported regression: the
   post-update `__cli_smoke__` merges a numeric reading, the agent shuts down
   for the update handoff BEFORE the propagator's debounce sends anything, and
   the replacement process — with no further Codex frame — still gets that
   reading to the backend: one signed hint, and a refresh receipt whose Codex
   metrics are numeric and whose generation matches the hint.
   ------------------------------------------------------------------------ */

func TestCodexSmoke_ReadingSurvivesAnUpdateHandoffBeforeTheDebounce(t *testing.T) {
	withCodexGenerationEpoch(t, 7101)
	f, path := codexSmokeCaptureEnv(t)
	rec, cfg := propagatorFixture(t)
	cliUsageHintDebounce = time.Hour // the handoff wins the race
	startCLIUsagePropagator(cfg)

	stubCodexSmokeExec(t, func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
		frames := append(codexPreUpdateFrames(codexMarkerFromLaunch(t, launch)),
			[]byte(codexSmokeTokenCountLineWithLimitID(t, "codex", 18, 36, time.Now())+"\n")...)
		return frames, nil, nil
	})
	if result := runCodexSmoke(context.Background(), path, codexSmokeTestVersion); result.Status != cliSmokeStatusSuccess {
		t.Fatalf("smoke = %+v, want success", result)
	}
	drainCodexRunDebtLadder(t)

	stopCLIUsagePropagator() // gracefulShutdown, before the debounce fired
	if n := len(rec.all()); n != 0 {
		t.Fatalf("the old process sent %d hints; the scenario needs the handoff to win", n)
	}

	// The replacement process: a new epoch, the same cache file, no new frame.
	cliUsageHintDebounce = 20 * time.Millisecond
	simulateCodexProcessRestart(t, 7102)
	startCLIUsagePropagator(cfg)

	hints := waitHints(t, rec, 1, cliUsageHintSpacing/2)
	if len(hints) != 1 {
		t.Fatalf("got %d hints before the follow-up boundary, want exactly 1", len(hints))
	}
	hint := hints[0].hint
	if hint.GenerationEpoch != 7102 {
		t.Fatalf("hint = %+v, want the replacement process's epoch", hint)
	}
	agent := codexRefreshReceiptAgent(t, f)
	assertNumericCodexMetrics(t, agent)
	if agent.UsageGeneration == nil || agent.UsageGeneration.Epoch != hint.GenerationEpoch || agent.UsageGeneration.Counter != hint.Generation {
		t.Fatalf("receipt generation %+v does not match the hint {%d,%d}", agent.UsageGeneration, hint.GenerationEpoch, hint.Generation)
	}
	if s := codexSessionMetric(t, agent.Metrics); s.Consumed == nil || *s.Consumed != 18 {
		t.Fatalf("session = %+v, want the smoke's 18%%", s)
	}
}
