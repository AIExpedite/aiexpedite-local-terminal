package main

// Acceptance: Claude Code's numeric utilization comes back after an agent
// update, on BOTH run paths — a direct chat run (claude_native.go) and a
// terminal-managed run (session.go), including its abnormal exit — and after a
// passing `__cli_smoke__` that lands inside a pre-update 429 hold.
//
// The shape of the reported defect: the run's own attempt cannot pay its debt
// (the endpoint refuses, or a hold is active), the agent is replaced, and the
// card then shows the dashed "usage unobservable" bars. Each case here refuses
// the run's own attempt, simulates the update restart, and requires numeric
// five-hour and weekly rows on the PUBLISHED usage from a loopback endpoint.

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// armPostUpdateEndpoint serves numeric five-hour and weekly windows once
// `healthy` is set, and 500s until then.
func armPostUpdateEndpoint(t *testing.T) (cache string, healthy *atomic.Bool) {
	t.Helper()
	healthy = &atomic.Bool{}
	cache, _ = armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		reset := time.Now().Add(3 * time.Hour).Unix()
		fmt.Fprintf(w, `{"limits":[{"kind":"session","percent":41,"resets_at":%d},`+
			`{"kind":"weekly_all","percent":17,"resets_at":%d}]}`, reset, reset+86400)
	})
	return cache, healthy
}

// requireNumericClaudeRows asserts the published five-hour and weekly rows
// carry the endpoint's numbers rather than the unobservable placeholder.
func requireNumericClaudeRows(t *testing.T) {
	t.Helper()
	usage, ok := claudeCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{}, time.Now())
	if !ok || len(usage.Metrics) < 2 {
		t.Fatalf("the Claude usage parser published no rows: ok=%v %+v", ok, usage)
	}
	for i, want := range []float64{41, 17} {
		row := usage.Metrics[i]
		if row.Unknown || row.Consumed == nil || *row.Consumed < want-0.1 || *row.Consumed > want+0.1 {
			t.Errorf("row %d (%s): unknown=%v consumed=%v, want numeric %v", i, row.Label, row.Unknown, row.Consumed, want)
		}
	}
}

// payAfterUpdate simulates the update hand-off and the new process's start,
// then waits for its replay (and any rung it arms) to land.
func payAfterUpdate(t *testing.T, cache string, healthy *atomic.Bool) {
	t.Helper()
	waitForClaudeDebt(t, cache, 10*time.Second)
	claudeFreshnessWaitIdle(t)
	healthy.Store(true)
	simulateClaudeAgentRestart(t)
	payOwedClaudeUsageRefreshAt(claudeAfterFirstRung())
	waitForClaudeCondition(t, 10*time.Second, "the owed refresh was never paid after the update", func() bool {
		snap, ok := loadClaudeRateLimitSnapshot(cache)
		return ok && snap.RefreshOwedAtMs == 0
	})
	claudeFreshnessWaitIdle(t)
}

func TestClaudePostUpdateFreshness_DirectRun(t *testing.T) {
	skipIfUnsupportedOS(t)
	cache, healthy := armPostUpdateEndpoint(t)
	seedStaleClaudeObservation(t, cache)
	tmpDir := installMockClaude(t, "claude-heartbeat-result")

	m := NewClaudeNativeManager(nil)
	id := fmt.Sprintf("claude-post-update-%d", time.Now().UnixNano())
	if err := m.Start(id, tmpDir, nil, "hello", "ws", "uid", func(resultMsg) {}, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.End(id) })

	payAfterUpdate(t, cache, healthy)
	requireNumericClaudeRows(t)
}

func TestClaudePostUpdateFreshness_TerminalManagedRun(t *testing.T) {
	skipIfUnsupportedOS(t)
	cache, healthy := armPostUpdateEndpoint(t)
	seedStaleClaudeObservation(t, cache)

	sm, id := startManagedClaudeSession(t, "claude-heartbeat-result")
	t.Cleanup(func() { _ = sm.EndSession(id) })

	payAfterUpdate(t, cache, healthy)
	requireNumericClaudeRows(t)
}

func TestClaudePostUpdateFreshness_TerminalManagedAbnormalExit(t *testing.T) {
	skipIfUnsupportedOS(t)
	cache, healthy := armPostUpdateEndpoint(t)
	seedStaleClaudeObservation(t, cache)

	sm, id := startManagedClaudeSession(t, "claude-heartbeat-hang")
	// Let the usage-less heartbeat land, then kill the run mid-turn.
	time.Sleep(200 * time.Millisecond)
	if err := sm.EndSession(id); err != nil {
		t.Fatalf("EndSession: %v", err)
	}

	payAfterUpdate(t, cache, healthy)
	requireNumericClaudeRows(t)
}

// A 429 hold persisted before the update, then a passing smoke: the smoke's
// debt is on disk at once, the attempt is held, and once the hold ends the rung
// pays it — no click, no Pub/Sub refresh needed.
func TestClaudePostUpdateFreshness_HeldThenPassingSmokeConverges(t *testing.T) {
	cache, healthy := armPostUpdateEndpoint(t)
	healthy.Store(true)
	pinClaudeRunDebtLadder(t, []time.Duration{50 * time.Millisecond}, 50*time.Millisecond, 20*time.Millisecond)
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))
	claudeHoldUsageProbe("", time.Now().Add(400*time.Millisecond))

	// The update: a new process adopts the persisted hold at start.
	simulateClaudeAgentRestart(t)
	payOwedClaudeUsageRefreshAt(time.Now())
	claudeFreshnessWaitIdle(t)

	// Not smokeEnv: that would point the smoke at a cache other than the one
	// the hold is persisted in.
	resetCLISmokeState()
	t.Cleanup(resetCLISmokeState)
	stubSmokePath(t, stubClaudeBinary(t))
	stubAuthProbe(t, true, true)
	stubSmokeExec(t, func(_ context.Context, _ []string, prompt string) ([]byte, []byte, error) {
		return successEnvelope(markerFromPrompt(prompt)), nil, nil
	})
	if result := runClaudeCodeSmoke(context.Background(), resolveClaudeSmokePath(), "2.1.251 (Claude Code)"); result.Status != cliSmokeStatusSuccess {
		t.Fatalf("fixture smoke did not pass: %+v", result)
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs == 0 || snap.HeldUntilMs == 0 {
		t.Fatalf("the smoke's debt must stand behind the hold: %+v", snap)
	}

	waitForClaudeCondition(t, 10*time.Second, "the smoke's debt was not paid once the hold ended", func() bool {
		snap, ok := loadClaudeRateLimitSnapshot(cache)
		return ok && snap.RefreshOwedAtMs == 0
	})
	claudeFreshnessWaitIdle(t)
	requireNumericClaudeRows(t)
}
