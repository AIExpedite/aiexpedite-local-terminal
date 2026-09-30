package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_smoke_codex_usage_capture_test.go — the `__cli_smoke__` turn
   produces its own utilization reading. Integration over the
   runCodexSmokeCommand seam with the REAL freshness lifecycle (no hook
   recorder): arm → capture → settle → the settle's own write finds the floor
   covered. No rollout file exists in any row, so nothing here can be paid by
   the scan this change exists to stop depending on.
   ------------------------------------------------------------------------ */

// codexSmokeTokenCountLine is the `exec --json` token_count event a turn
// prints, carrying both windows.
func codexSmokeTokenCountLine(t *testing.T, primaryPct, secondaryPct float64, now time.Time) string {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		"type": "event_msg",
		"payload": map[string]any{
			"type":        "token_count",
			"info":        map[string]any{"total_token_usage": map[string]any{"total_tokens": 1}},
			"rate_limits": codexRateLimitFrame(primaryPct, secondaryPct, now),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(line)
}

// codexSmokeCaptureEnv is a real-lifecycle smoke fixture: an isolated
// CODEX_HOME and cache with a signed-in account and a pre-run reading, the
// freshness gate armed, and a stub binary.
func codexSmokeCaptureEnv(t *testing.T) (codexFreshnessFixture, string) {
	t.Helper()
	smokeEnv(t)
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	t.Setenv("OPENAI_API_KEY", "")
	stubCodexLogin(t, true, true)
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	return f, stubCodexBinary(t)
}

// A frame-bearing smoke advances observedAt past its own run floor and leaves
// no debt, with zero rollout files on disk; the reading is stamped with the
// binary the smoke validated.
func TestRunCodexSmoke_OwnFramesAdvanceObservedAtWithoutRollouts(t *testing.T) {
	f, path := codexSmokeCaptureEnv(t)
	started := time.Now()
	stubCodexSmokeExec(t, func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
		frames := append(codexPreUpdateFrames(codexMarkerFromLaunch(t, launch)),
			[]byte(codexSmokeTokenCountLine(t, 61, 62, time.Now())+"\n")...)
		return frames, nil, nil
	})

	if result := runCodexSmoke(context.Background(), path, codexSmokeTestVersion); result.Status != cliSmokeStatusSuccess {
		t.Fatalf("result = %+v, want success", result)
	}
	drainCodexRunDebtLadder(t)

	session := codexSessionMetric(t, codexMetricsFromCache(time.Now(), f.fp))
	if got := metricObservedAt(t, session); got.Before(started.Truncate(time.Second)) {
		t.Fatalf("observedAt %s predates the smoke's run floor %s", got, started)
	}
	if session.Consumed == nil || *session.Consumed != 61 {
		t.Fatalf("session metric = %+v, want the smoke turn's 61%%", session)
	}
	snap := f.snapshot(t)
	if snap.RefreshOwedAtMs != 0 {
		t.Fatalf("the smoke's own capture must pay its run: %+v", snap)
	}
	if snap.CodexVersion != codexSmokeTestVersion {
		t.Fatalf("stamp = %q, want the smoked binary %q", snap.CodexVersion, codexSmokeTestVersion)
	}
}

// A marker-only turn (no rate-limit frame — the post-update shape drift) still
// settles, and owes the debt the rollout attempts and the live fallback then
// chase, exactly as before.
func TestRunCodexSmoke_MarkerOnlyTurnStillOwesADebt(t *testing.T) {
	f, path := codexSmokeCaptureEnv(t)
	stubCodexFallbackRead(t, func(context.Context, string) string { return liveProbeOutcomeNoReading })
	stubCodexSmokeExec(t, func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
		return codexPreUpdateFrames(codexMarkerFromLaunch(t, launch)), nil, nil
	})

	runCodexSmoke(context.Background(), path, codexSmokeTestVersion)
	drainCodexRunDebtLadder(t)

	if snap := f.snapshot(t); snap.RefreshOwedAtMs == 0 || snap.RefreshLiveReads == 0 {
		t.Fatalf("a marker-only turn must owe a debt that reaches its fallback: %+v", snap)
	}
}

// usageCaptured is sticky across the ladder: a first rung that merged a
// numeric window and was then rejected on the droppable flag, followed by a
// rung that captured nothing and left no other evidence, still settles — the
// window is already in the cache.
func TestRunCodexSmoke_CaptureOnAnEarlierRungStillSettles(t *testing.T) {
	_, rec := codexSmokeEnv(t)
	walks := countCodexRolloutWalks(t)
	path := stubCodexBinary(t)
	exitErr := codexExitError(t)
	calls, _ := stubCodexSmokeExec(t, func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
		if launch.LastMessageFile != "" && strings.Contains(strings.Join(launch.Args, " "), "--output-last-message") {
			return []byte(codexSmokeTokenCountLine(t, 12, 13, time.Now()) + "\n"),
				[]byte("error: unexpected argument '--output-last-message' found"), exitErr
		}
		return []byte(`{"type":"thread.started"}` + "\n"), nil, nil
	})

	result := runCodexSmoke(context.Background(), path, codexSmokeTestVersion)

	if *calls != 2 {
		t.Fatalf("ladder spawned %d children, want both rungs", *calls)
	}
	if result.Diagnostic != cliSmokeDiagnosticNoEnvelope {
		t.Fatalf("verdict comes from the final rung: %+v", result)
	}
	if _, settled, disarmed := rec.lifecycle(); settled != 1 || disarmed != 0 || *walks != 0 {
		t.Fatalf("settled=%d disarmed=%d walks=%d, want the earlier rung's capture to settle without a walk", settled, disarmed, *walks)
	}
}

// codexSmokeTokenCountLineWithLimitID is codexSmokeTokenCountLine as current
// builds print it: the aggregate carries `limit_id`.
func codexSmokeTokenCountLineWithLimitID(t *testing.T, limitID string, primaryPct, secondaryPct float64, now time.Time) string {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		"type": "event_msg",
		"payload": map[string]any{
			"type":        "token_count",
			"info":        map[string]any{"total_token_usage": map[string]any{"total_tokens": 1}},
			"rate_limits": codexRateLimitFrameWithLimitID(limitID, primaryPct, secondaryPct, now),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(line)
}

// A `limit_id: "codex"` smoke frame against a cache already holding an OLDER
// `codex` contributor with higher usage: the smoke's reading is what the card
// shows — its value and its observedAt — and the capture generation advanced.
func TestRunCodexSmoke_LimitIDFrameWinsOverAnOlderCodexReading(t *testing.T) {
	f, path := codexSmokeCaptureEnv(t)
	older := time.Now().Add(-20 * time.Minute)
	if !captureCodexRateLimitLineForAccount(codexSmokeTokenCountLineWithLimitID(t, "codex", 88, 90, older), older, f.fp) {
		t.Fatal("seeding the older codex reading failed")
	}
	before := f.snapshot(t)
	started := time.Now()
	stubCodexSmokeExec(t, func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
		frames := append(codexPreUpdateFrames(codexMarkerFromLaunch(t, launch)),
			[]byte(codexSmokeTokenCountLineWithLimitID(t, "codex", 14, 27, time.Now())+"\n")...)
		return frames, nil, nil
	})

	if result := runCodexSmoke(context.Background(), path, codexSmokeTestVersion); result.Status != cliSmokeStatusSuccess {
		t.Fatalf("result = %+v, want success", result)
	}
	drainCodexRunDebtLadder(t)

	metrics := codexMetricsFromCache(time.Now(), f.fp)
	session, weekly := codexSessionMetric(t, metrics), codexWeeklyMetric(t, metrics)
	if session.Consumed == nil || *session.Consumed != 14 || weekly.Consumed == nil || *weekly.Consumed != 27 {
		t.Fatalf("session=%+v weekly=%+v, want the smoke's 14%% / 27%%", session, weekly)
	}
	for _, m := range []cliAgentUsageMetric{session, weekly} {
		if got := metricObservedAt(t, m); got.Before(started.Truncate(time.Second)) {
			t.Fatalf("observedAt %s predates the smoke %s", got, started)
		}
	}
	after := f.snapshot(t)
	if after.GenerationEpoch == before.GenerationEpoch && after.Generation <= before.Generation {
		t.Fatalf("generation did not advance: {%d,%d} → {%d,%d}",
			before.GenerationEpoch, before.Generation, after.GenerationEpoch, after.Generation)
	}
}
