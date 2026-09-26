package main

// The acceptance criterion for "Claude Code utilization capture remains stale
// after update": observedAt must ADVANCE and must SURVIVE the agent replacing
// itself. Mirrors cliagent_usage_codex_update_survival_test.go — the cache file
// is the only thing that carries over a restart, so everything else is reset.

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// simulateClaudeAgentRestart discards every in-memory trace of the previous
// process — the single-flight latch, the throttle, the debt, the 429 hold — and
// arms a fresh one, exactly as StartAgent does after a self-update. The pinned
// cache file (AIEXPEDITE_CLAUDE_RL_CACHE, still set by armClaudeUsageProbe) is
// all that survives.
func simulateClaudeAgentRestart(t *testing.T) {
	t.Helper()
	resetClaudeUsageProbeGate()
	SetClaudeUsageProbeDisabled(false)
}

// claudeObservedAt is the stalest row the card would show, which is what the
// ticket's "observedAt" refers to.
func claudeObservedAt(t *testing.T, now time.Time) time.Time {
	t.Helper()
	return claudeSnapshotFreshness(loadMergedClaudeRateLimitView(currentClaudeAccountFingerprint()), now)
}

// claudePublishedSessionMetric is the 5-hour row as the CLI Agents card
// actually receives it — through the parser, not read off the cache. The
// acceptance criterion is about the PUBLISHED observedAt, and everything
// between the cache and that field (row selection, the unknown-login
// downgrade, RFC3339 formatting) is part of what has to keep working.
func claudePublishedSessionMetric(t *testing.T) cliAgentUsageMetric {
	t.Helper()
	usage, ok := claudeCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{}, time.Now())
	if !ok {
		t.Fatal("the Claude usage parser produced nothing to publish")
	}
	for _, metric := range usage.Metrics {
		if metric.Kind == limitKindSession {
			return metric
		}
	}
	t.Fatalf("no session row in the published metrics: %+v", usage.Metrics)
	return cliAgentUsageMetric{}
}

// The reported regression, end to end and through the REAL trigger: a Claude
// run finishes, the agent is replaced before the trailing probe can pay the
// debt, and the new process's startup replay pays it — so the published
// observedAt advances past the run instead of serving the pre-update reading
// for the whole claudeUsageProbeStaleAfter window.
func TestClaudeOwedRefresh_SurvivesAgentRestart(t *testing.T) {
	now := time.Now()
	// Refuse while the run's own trailing probe runs, so the debt is still
	// standing when the simulated self-update lands — that is the case under
	// test. The replay afterwards gets a real answer.
	var refuse atomic.Bool
	refuse.Store(true)
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		if refuse.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"limits":[{"kind":"session","percent":52,"resets_at":%d}]}`,
			now.Add(time.Hour).Unix())
	})
	// A pre-update reading: recent enough that an unaware gather would trust it
	// for the whole staleness TTL.
	preRun := now.Add(-time.Second)
	seedClaudeProbeReading(t, cache, preRun)

	// The turn ends. This is the production entry point — claude_native.go and
	// session.go call exactly this — so the test covers the wiring from a
	// finished run to the durable debt, not just the replay that reads it.
	runEnded := time.Now()
	triggerClaudeUsageProbeAfterRun()
	waitForClaudeDebt(t, cache, 5*time.Second)
	claudeFreshnessWaitIdle(t)
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs < runEnded.Truncate(time.Millisecond).UnixMilli() {
		t.Fatalf("the run's debt was not persisted at its completion: %+v", snap)
	}
	before := atomic.LoadInt64(calls)

	refuse.Store(false)
	simulateClaudeAgentRestart(t)
	payOwedClaudeUsageRefreshAt(time.Now())
	claudeFreshnessWaitIdle(t)

	if got := atomic.LoadInt64(calls) - before; got != 1 {
		t.Fatalf("replay spent %d requests, want exactly 1", got)
	}
	if got := claudeObservedAt(t, time.Now()); !got.After(preRun) || got.Before(runEnded.Truncate(time.Millisecond)) {
		t.Fatalf("observedAt %v did not advance past the run %v (pre-update reading %v)", got, runEnded, preRun)
	}
	// The criterion as the ticket states it, on the surface it names: the
	// PUBLISHED row's observedAt, not the cache value behind it.
	published := claudePublishedSessionMetric(t)
	if published.Unknown {
		t.Fatalf("the session row published as unknown after the replay: %+v", published)
	}
	if got := metricObservedAt(t, published); got.Before(runEnded.Truncate(time.Second)) {
		t.Fatalf("published observedAt %v still predates the run %v", got, runEnded)
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("replayed debt not cleared: %+v", snap)
	}
}

// A smoke that spent a turn just before the update owes exactly like a run, and
// is paid by the same replay.
func TestClaudeOwedRefresh_SmokeDebtSurvivesAgentRestart(t *testing.T) {
	now := time.Now()
	// Refuse while the smoke's own trailing probe runs, so the debt it records
	// is still standing when the simulated self-update lands — that is the case
	// under test. The replay afterwards gets a real answer.
	var refuse atomic.Bool
	refuse.Store(true)
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		if refuse.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"limits":[{"kind":"session","percent":61,"resets_at":%d}]}`,
			now.Add(time.Hour).Unix())
	})
	preSmoke := now.Add(-time.Second)
	seedClaudeProbeReading(t, cache, preSmoke)

	settleOrDisarmClaudeSmokeRun(true)
	waitForClaudeDebt(t, cache, 5*time.Second)
	claudeFreshnessWaitIdle(t)
	smokeDebt := claudeCacheSnapshot(t, cache).RefreshOwedAtMs
	if smokeDebt == 0 {
		t.Fatal("a smoke that reached inference must owe a refresh")
	}
	before := atomic.LoadInt64(calls)

	refuse.Store(false)
	simulateClaudeAgentRestart(t)
	payOwedClaudeUsageRefreshAt(time.Now())
	claudeFreshnessWaitIdle(t)

	if got := atomic.LoadInt64(calls) - before; got != 1 {
		t.Fatalf("replay spent %d requests, want exactly 1", got)
	}
	if got := claudeObservedAt(t, time.Now()); got.Before(time.UnixMilli(smokeDebt)) {
		t.Fatalf("observedAt %v still predates the smoke turn %v", got, time.UnixMilli(smokeDebt))
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("the smoke's debt was not settled by the replay: %+v", snap)
	}
}

// The replay is exactly ONE attempt when it finds nothing; the remainder is
// left to the next run, refresh or routine gather under the ordinary bounds.
func TestClaudeOwedRefresh_RestartReplayIsExactlyOneAttempt(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	claudeOweRunRefresh(now.Add(-time.Minute))

	simulateClaudeAgentRestart(t)
	payOwedClaudeUsageRefreshAt(now)
	claudeFreshnessWaitIdle(t)

	if atomic.LoadInt64(calls) != 1 {
		t.Fatalf("replay spent %d requests, want exactly 1", atomic.LoadInt64(calls))
	}
	snap := claudeCacheSnapshot(t, cache)
	if snap.RefreshOwedAtMs == 0 {
		t.Fatal("an unpaid debt must survive the replay")
	}
	if snap.RefreshOwedAttempts != 1 {
		t.Fatalf("RefreshOwedAttempts=%d, want exactly 1", snap.RefreshOwedAttempts)
	}
}

// Repeated restarts retire the debt at the attempt cap rather than issuing one
// request per start for the whole of claudeRefreshOwedMaxAge.
func TestClaudeOwedRefresh_RepeatedRestartsRetireAtTheAttemptCap(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	claudeOweRunRefresh(now.Add(-time.Minute))

	for i := 0; i < claudeUsageProbeAfterRunMaxAttempts+3; i++ {
		simulateClaudeAgentRestart(t)
		payOwedClaudeUsageRefreshAt(now)
		claudeFreshnessWaitIdle(t)
	}

	if atomic.LoadInt64(calls) != claudeUsageProbeAfterRunMaxAttempts {
		t.Fatalf("request count=%d, want the cap %d", atomic.LoadInt64(calls), claudeUsageProbeAfterRunMaxAttempts)
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("a debt at the attempt cap must be retired, paid or not: %+v", snap)
	}
}

// A debt past claudeRefreshOwedMaxAge is neither replayed nor rewritten.
func TestClaudeOwedRefresh_ExpiredDebtIsNotReplayed(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-2*time.Hour))
	claudeOweRunRefresh(now.Add(-claudeRefreshOwedMaxAge - 10*time.Minute))

	simulateClaudeAgentRestart(t)
	payOwedClaudeUsageRefreshAt(now)
	claudeFreshnessWaitIdle(t)

	if atomic.LoadInt64(calls) != 0 {
		t.Fatalf("an expired debt must not spend a request (%d sent)", atomic.LoadInt64(calls))
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("an expired debt must be retired: %+v", snap)
	}
}

// PINNED AS THE ACCEPTED GAP, not as coverage. A process killed MID-turn
// persists nothing (this design records the debt only when the turn returns),
// so the next start finds no debt and issues no request. Anyone later adding a
// persisted active-run floor will see this expectation change deliberately.
func TestClaudeOwedRefresh_TurnKilledMidFlightLeavesNothingToReplay(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	before, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}

	// The run started but never returned: nothing called claudeOweRunRefresh.
	simulateClaudeAgentRestart(t)
	payOwedClaudeUsageRefreshAt(now)
	claudeFreshnessWaitIdle(t)

	if atomic.LoadInt64(calls) != 0 {
		t.Fatalf("with no persisted debt the replay must issue nothing (%d sent)", atomic.LoadInt64(calls))
	}
	after, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a replay with nothing to pay must leave the cache byte-identical")
	}
}

// StartAgent MUST call payOwedClaudeUsageRefresh, and must call it AFTER the
// opt-out is applied and AFTER the offline state is published.
//
// Deleting that one line leaves every behavioural test in this file green — the
// replay is invoked directly by all of them — while the feature silently stops
// working on real devices, which is the whole regression the ticket describes.
// The two orderings are equally invisible: arming the gate later would make the
// replay see an unarmed probe and CLEAR the debt instead of paying it, and
// publishing isOffline later would let a disconnected agent make the outbound
// call it was told not to. None of that can be pinned behaviourally, because
// StartAgent also brings up ttyd, tmux and the Pub/Sub worker; the invariant is
// positional, so the assertion is too — the same reasoning as
// TestStartSession_AttributesGrokBillingBeforeSpawn.
func TestStartAgent_ReplaysTheOwedClaudeRefreshAfterArmingAndOffline(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "agent.go", nil, 0)
	if err != nil {
		t.Fatalf("parse agent.go: %v", err)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "StartAgent" {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("StartAgent not found in agent.go")
	}

	replay, arm, offline := token.NoPos, token.NoPos, token.NoPos
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			ident, ok := node.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			if ident.Name == "payOwedClaudeUsageRefresh" && !replay.IsValid() {
				replay = node.Pos()
			}
			if ident.Name == "SetClaudeUsageProbeDisabled" && !arm.IsValid() {
				arm = node.Pos()
			}
		case *ast.AssignStmt:
			// `isOffline = cfg.OfflineMode`, the publish the probe's begin()
			// reads through IsOffline().
			for _, lhs := range node.Lhs {
				if ident, ok := lhs.(*ast.Ident); ok && ident.Name == "isOffline" && !offline.IsValid() {
					offline = node.Pos()
				}
			}
		}
		return true
	})

	if !replay.IsValid() {
		t.Fatal("StartAgent no longer replays the owed Claude refresh — a run or smoke that finished before a self-update is never paid, which is the reported defect")
	}
	if !arm.IsValid() {
		t.Fatal("StartAgent no longer applies the Claude usage-probe opt-out")
	}
	if !offline.IsValid() {
		t.Fatal("StartAgent no longer publishes isOffline")
	}
	if replay < arm {
		t.Error("payOwedClaudeUsageRefresh runs before SetClaudeUsageProbeDisabled; an unarmed gate makes the replay CLEAR the debt instead of paying it")
	}
	if replay < offline {
		t.Error("payOwedClaudeUsageRefresh runs before isOffline is published; a disconnected agent would make the outbound call it was told not to")
	}
}
