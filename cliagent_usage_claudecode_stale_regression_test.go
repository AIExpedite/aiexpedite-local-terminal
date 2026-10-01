package main

// The reported defect, end to end: on Windows a passing Claude `__cli_smoke__`
// left the CLI Agents card on an old reading, still stale after the 90 s Refresh
// window and a 30 s retry. Each case drives the production entry points against
// a loopback stand-in for api.anthropic.com/api/oauth/usage (the
// AIEXPEDITE_CLAUDE_USAGE_PROBE_URL seam) — the live endpoint cannot be reached
// from CI — and asserts on the PUBLISHED rows, through the parser. Mirrors
// cliagent_usage_antigravity_stale_regression_test.go.

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// claudeAllRowsResponse is a stand-in reading for every row the card shows:
// the session, the unified weekly, and the scoped Fable meter.
func claudeAllRowsResponse(w http.ResponseWriter, session, weekly, fable float64, resetsAt time.Time) {
	fmt.Fprintf(w, `{"limits":[
		{"kind":"session","percent":%g,"resets_at":%d},
		{"kind":"weekly_all","percent":%g,"resets_at":%d},
		{"kind":"weekly_scoped","scope":{"model":{"display_name":"Fable"}},"percent":%g,"resets_at":%d}]}`,
		session, resetsAt.Unix(), weekly, resetsAt.Unix(), fable, resetsAt.Unix())
}

// claudePublishedRows parses the card through the production parser.
func claudePublishedRows(t *testing.T, ctx context.Context) []cliAgentUsageMetric {
	t.Helper()
	usage, ok := claudeCodeUsageParser{}.ParseContext(ctx, "", detectedCLIAgent{}, time.Now())
	if !ok {
		t.Fatal("the Claude usage parser produced nothing to publish")
	}
	return usage.Metrics
}

// assertClaudeRowsNumericAfter fails unless every published row carries a
// numeric reading observed at or after `after`.
func assertClaudeRowsNumericAfter(t *testing.T, rows []cliAgentUsageMetric, after time.Time) {
	t.Helper()
	if len(rows) != 3 {
		t.Fatalf("published %d rows, want 3: %+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.Unknown || row.Consumed == nil {
			t.Errorf("row %q published without a numeric reading: %+v", row.Label, row)
			continue
		}
		if got := time.UnixMilli(observedAtMsFromRFC3339(row.ObservedAt)); got.Before(after.Truncate(time.Millisecond)) {
			t.Errorf("row %q observedAt %v predates the run %v", row.Label, got, after)
		}
	}
}

// waitForClaudeDebtPaid polls until the durable debt is gone.
func waitForClaudeDebtPaid(t *testing.T, cache string, within time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(within); time.Now().Before(deadline); {
		if snap, ok := loadClaudeRateLimitSnapshot(cache); ok && snap.RefreshOwedAtMs == 0 && snap.LastProbeObservedAtMs != 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	snap, _ := loadClaudeRateLimitSnapshot(cache)
	t.Fatalf("the run's debt was never paid: %+v", snap)
}

// Terminal-managed smoke: the smoke passes, its first trailing request times out
// (the cold-handshake case), and a later rung of the ladder pays the debt — with
// no gather in between. Before the ladder the debt sat on disk until some
// backend-initiated gather, and the card stayed on the pre-smoke reading.
func TestClaudeStaleRegression_TerminalManagedSmokeLandsAfterATimedOutProbe(t *testing.T) {
	resets := time.Now().Add(2 * time.Hour)
	var served int64
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt64(&served, 1) == 1 {
			// Slower than the (pinned) trailing request bound.
			select {
			case <-time.After(600 * time.Millisecond):
			case <-r.Context().Done():
			}
			return
		}
		claudeAllRowsResponse(w, 41, 23, 7, resets)
	})
	origTimeout := claudeUsageProbeTrailingRequestTimeout
	claudeUsageProbeTrailingRequestTimeout = 200 * time.Millisecond
	t.Cleanup(func() { claudeUsageProbeTrailingRequestTimeout = origTimeout })
	pinClaudeRunDebtLadder(t, []time.Duration{100 * time.Millisecond}, 100*time.Millisecond)
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))

	resetCLISmokeState()
	t.Cleanup(resetCLISmokeState)
	stubSmokePath(t, stubClaudeBinary(t))
	stubAuthProbe(t, true, true)
	stubSmokeExec(t, func(_ context.Context, _ []string, prompt string) ([]byte, []byte, error) {
		return successEnvelope(markerFromPrompt(prompt)), nil, nil
	})

	result := runClaudeCodeSmoke(context.Background(), resolveClaudeSmokePath(), "2.1.251 (Claude Code)")
	smokeEnded := time.Now()
	if result.Status != cliSmokeStatusSuccess {
		t.Fatalf("fixture smoke did not pass: %+v", result)
	}

	waitForClaudeDebtPaid(t, cache, 10*time.Second)
	claudeFreshnessWaitIdle(t)
	if got := atomic.LoadInt64(calls); got != 2 {
		t.Fatalf("request count=%d, want 2 (the timed-out trailing attempt, then one rung)", got)
	}
	// The smoke turn completes a hair before runClaudeCodeSmoke returns.
	assertClaudeRowsNumericAfter(t, claudePublishedRows(t, context.Background()), smokeEnded.Add(-time.Second))
	if claudeRunDebtRetryPending() {
		t.Fatal("a paid debt left a rung armed")
	}
}

// Direct (user-shell) run: an interactive status-line render keeps five_hour /
// seven_day fresh while an old per-model weekly bucket pinned the weekly row,
// and the freshness check then refused to re-probe it. A Refresh click's forced
// probe must leave every displayed row numeric and observed after the run.
func TestClaudeStaleRegression_DirectRunRefreshPublishesFreshRows(t *testing.T) {
	resets := time.Now().Add(48 * time.Hour)
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		claudeAllRowsResponse(w, 30, 20, 5, resets)
	})
	fp := currentClaudeAccountFingerprint()
	old := time.Now().Add(-6 * time.Hour)
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowSevenDayOpus: {
			UsedPercentage: 90, ResetsAtMs: resets.UnixMilli(), ObservedAtMs: old.UnixMilli(), usageKnown: true,
		},
	}, old, fp, claudeRateLimitSourceStream)
	runEnded := time.Now()
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {UsedPercentage: 25, ResetsAtMs: time.Now().Add(time.Hour).UnixMilli(), ObservedAtMs: runEnded.UnixMilli(), usageKnown: true},
		claudeWindowSevenDay: {UsedPercentage: 18, ResetsAtMs: resets.UnixMilli(), ObservedAtMs: runEnded.UnixMilli(), usageKnown: true},
	}, runEnded, fp, claudeRateLimitSourceStatusLine)

	rows := claudePublishedRows(t, WithClaudeUsageForceProbe(context.Background()))
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("request count=%d, want the one forced probe", got)
	}
	assertClaudeRowsNumericAfter(t, rows, runEnded)
	for _, row := range rows {
		if row.Kind == limitKindWeekly && row.Model == "" && *row.Consumed != 20 {
			t.Errorf("weekly row shows %v%%, want the probe's 20%% — the superseded 90%% Opus reading still pins it", *row.Consumed)
		}
	}
	// And the routine gather that follows judges the card fresh: no probe loop
	// on a row the endpoint does not supply.
	claudePublishedRows(t, context.Background())
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("request count=%d after a routine gather, want no further probe", got)
	}
}

// Post-update survival: the first trailing attempt fails, the agent is replaced
// before the rung fires, and the new process re-arms the persisted rung rather
// than attempting early — then that rung pays the debt.
func TestClaudeStaleRegression_RungSurvivesAnAgentUpdate(t *testing.T) {
	resets := time.Now().Add(2 * time.Hour)
	var refuse atomic.Bool
	refuse.Store(true)
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		if refuse.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		claudeAllRowsResponse(w, 44, 26, 9, resets)
	})
	pinClaudeRunDebtLadder(t, []time.Duration{time.Second}, time.Second)
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))

	runEnded := time.Now()
	triggerClaudeUsageProbeAfterRun()
	waitForClaudeDebt(t, cache, 5*time.Second)
	claudeFreshnessWaitIdle(t)
	snap := claudeCacheSnapshot(t, cache)
	if snap.NextAttemptAtMs == 0 || snap.RefreshOwedAttempts != 1 {
		t.Fatalf("the failed trailing attempt must be charged and book a rung: %+v", snap)
	}

	refuse.Store(false)
	simulateClaudeAgentRestart(t)
	before := atomic.LoadInt64(calls)
	payOwedClaudeUsageRefreshAt(time.Now())
	if got := atomic.LoadInt64(calls) - before; got != 0 {
		t.Fatalf("the replay attempted %d times before its rung was due", got)
	}
	if !claudeRunDebtRetryPending() {
		t.Fatal("the replay did not re-arm the persisted rung")
	}

	waitForClaudeDebtPaid(t, cache, 10*time.Second)
	claudeFreshnessWaitIdle(t)
	if got := atomic.LoadInt64(calls) - before; got != 1 {
		t.Fatalf("the re-armed rung spent %d requests, want 1", got)
	}
	assertClaudeRowsNumericAfter(t, claudePublishedRows(t, context.Background()), runEnded)
}

// 429 hold: the ladder waits the Retry-After out, sends nothing inside it, and
// pays after it.
func TestClaudeStaleRegression_LadderWaitsOutA429Hold(t *testing.T) {
	resets := time.Now().Add(2 * time.Hour)
	var (
		mu    sync.Mutex
		times []time.Time
	)
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		times = append(times, time.Now())
		first := len(times) == 1
		mu.Unlock()
		if first {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		claudeAllRowsResponse(w, 12, 34, 3, resets)
	})
	pinClaudeRunDebtLadder(t, []time.Duration{50 * time.Millisecond}, 50*time.Millisecond)
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))

	runEnded := time.Now()
	triggerClaudeUsageProbeAfterRun()
	waitForClaudeDebtPaid(t, cache, 10*time.Second)
	claudeFreshnessWaitIdle(t)

	if got := atomic.LoadInt64(calls); got != 2 {
		t.Fatalf("request count=%d, want 2 (the 429, then one request after the hold)", got)
	}
	mu.Lock()
	gap := times[1].Sub(times[0])
	mu.Unlock()
	// Retry-After is whole seconds and the deadline is taken off the response's
	// own clock, so allow the sub-second rounding below one second.
	if gap < 900*time.Millisecond {
		t.Fatalf("the ladder retried %s after the 429, inside the 1s hold", gap)
	}
	assertClaudeRowsNumericAfter(t, claudePublishedRows(t, context.Background()), runEnded)
}
