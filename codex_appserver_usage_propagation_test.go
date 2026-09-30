package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   codex_appserver_usage_propagation_test.go — the direct app-server path end
   to end on the device: a `token_count` carrying `limit_id` on the app-server's
   stdout is merged, produces one signed usage hint, and the
   `__cli_usage_refresh__` receipt that hint asks for carries numeric Codex
   metrics and the hint's generation. The reading survives an agent update.
   ------------------------------------------------------------------------ */

// codexRefreshReceiptAgent is the Codex provider of a signed refresh receipt,
// as handleCLIUsageRefreshCommand would build it for a forced refresh.
func codexRefreshReceiptAgent(t *testing.T, f codexFreshnessFixture) cliAgentUsage {
	t.Helper()
	usage, ok := codexUsageParser{}.ParseContext(WithCodexUsageForceRefresh(context.Background()), f.home, detectedCLIAgent{Path: "codex"}, time.Now())
	if !ok {
		t.Fatal("codex parse failed")
	}
	receipt, normalized, _, err := prepareCLIUsageRefreshResult("secret", "refresh-1", time.Now().UnixMilli(), true, []cliAgentUsage{*usage}, nil)
	if err != nil || receipt == "" || len(normalized) != 1 {
		t.Fatalf("receipt: %q %v", receipt, err)
	}
	return normalized[0]
}

func assertNumericCodexMetrics(t *testing.T, agent cliAgentUsage) {
	t.Helper()
	for _, m := range agent.Metrics {
		if m.Consumed != nil && !m.Unknown {
			return
		}
	}
	t.Fatalf("receipt carries zero numeric Codex metrics: %+v", agent.Metrics)
}

// readCodexAppServerStdout feeds lines through the app-server manager's real
// stdout reader.
func readCodexAppServerStdout(t *testing.T, lines ...string) {
	t.Helper()
	session := &CodexAppServerSession{
		ID:         "appserver-usage",
		Stdout:     io.NopCloser(strings.NewReader(strings.Join(lines, "\n") + "\n")),
		Stderr:     io.NopCloser(strings.NewReader("")),
		done:       make(chan struct{}),
		streamDone: make(chan struct{}),
	}
	NewCodexAppServerManager(nil).readStream(session, func(resultMsg) {})
}

func codexAppServerTokenCount(t *testing.T, sessionPct, weeklyPct float64, now time.Time) string {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "codex/event/token_count",
		"params": map[string]any{"msg": map[string]any{
			"type":        "token_count",
			"rate_limits": codexRateLimitFrameWithLimitID("codex", sessionPct, weeklyPct, now),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(line)
}

func TestCodexAppServer_TokenCountPropagatesToTheReceipt(t *testing.T) {
	withCodexGenerationEpoch(t, 7001)
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-time.Hour))
	stubCodexLogin(t, true, true)
	rec, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)

	readCodexAppServerStdout(t, codexAppServerTokenCount(t, 23, 57, now))

	hint := waitHints(t, rec, 1, 0)[0].hint
	agent := codexRefreshReceiptAgent(t, f)
	assertNumericCodexMetrics(t, agent)
	if agent.UsageGeneration == nil || agent.UsageGeneration.Epoch != hint.GenerationEpoch || agent.UsageGeneration.Counter != hint.Generation {
		t.Fatalf("receipt generation %+v, hint {%d,%d}", agent.UsageGeneration, hint.GenerationEpoch, hint.Generation)
	}
	if s := codexSessionMetric(t, agent.Metrics); s.Consumed == nil || *s.Consumed != 23 {
		t.Fatalf("session = %+v, want the app-server's 23%%", s)
	}

	// A simulated agent update: a new process over the same cache, a new
	// build stamp. The same numbers come back, now under the new epoch.
	simulateCodexProcessRestart(t, 7002)
	publishCodexUsageCaptureVersionFrom("codex", "codex-cli 9.9.9")
	codexRotateGenerationEpoch(time.Now())
	after := codexRefreshReceiptAgent(t, f)
	if s := codexSessionMetric(t, after.Metrics); s.Consumed == nil || *s.Consumed != 23 {
		t.Fatalf("after the update session = %+v, want 23%% to survive", s)
	}
	if w := codexWeeklyMetric(t, after.Metrics); w.Consumed == nil || *w.Consumed != 57 {
		t.Fatalf("after the update weekly = %+v, want 57%% to survive", w)
	}
	if after.UsageGeneration == nil || after.UsageGeneration.Epoch != 7002 {
		t.Fatalf("after the update generation = %+v, want the new process's epoch", after.UsageGeneration)
	}
}
