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

// claudeAfterFirstRung is a start instant past the first retry rung a failed
// run attempt books: a restart BEFORE it re-arms the rung rather than spending
// it early, which is its own test.
func claudeAfterFirstRung() time.Time {
	return time.Now().Add(claudeRunDebtRetryLadder[0] + claudeRunDebtRungSlack + time.Second)
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
	payOwedClaudeUsageRefreshAt(claudeAfterFirstRung())
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
	payOwedClaudeUsageRefreshAt(claudeAfterFirstRung())
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
// left to the rung it books, under the shared request budget.
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

// Repeated restarts retire the debt at the request cap rather than issuing one
// request per start for the whole of claudeRefreshOwedMaxAge.
func TestClaudeOwedRefresh_RepeatedRestartsRetireAtTheAttemptCap(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	claudeOweRunRefresh(now.Add(-time.Minute))

	// Each start lands after the rung the previous one booked, so every start
	// is due; only the budget stops them.
	for i := 0; i < claudeRefreshOwedMaxRequests+3; i++ {
		simulateClaudeAgentRestart(t)
		payOwedClaudeUsageRefreshAt(now.Add(time.Duration(i) * 20 * time.Minute))
		claudeFreshnessWaitIdle(t)
	}

	if atomic.LoadInt64(calls) != claudeRefreshOwedMaxRequests {
		t.Fatalf("request count=%d, want the cap %d", atomic.LoadInt64(calls), claudeRefreshOwedMaxRequests)
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

/* --------------------------------------------------------------------------
   The retry ladder across an update hand-off
   -------------------------------------------------------------------------- */

// The debt, its request counter, its rung, the 429 hold and the credential
// stamp being waited on all survive a restart — and the new process re-arms
// the rung rather than spending it or re-sending the 401'd token.
func TestClaudeOwedRefresh_ScheduleStateSurvivesRestart(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	claudeOweRunRefresh(now.Add(-time.Minute))
	if result := claudeRunDebtAttemptAt(now, claudeDebtTriggerRun); result.code != claudeProbeHTTP401 {
		t.Fatalf("precondition: result=%+v, want http_401", result)
	}
	hold := now.Add(30 * time.Minute)
	claudeHoldUsageProbe("", hold)
	before := claudeCacheSnapshot(t, cache)
	if before.RefreshOwedAttempts != 1 || before.NextAttemptAtMs == 0 || before.HeldUntilMs == 0 ||
		before.AuthWaitCredStampNs == 0 || before.AuthWaitCredSize == 0 {
		t.Fatalf("precondition: schedule state not persisted: %+v", before)
	}

	simulateClaudeAgentRestart(t)
	payOwedClaudeUsageRefreshAt(time.Now())
	claudeFreshnessWaitIdle(t)

	after := claudeCacheSnapshot(t, cache)
	if after.RefreshOwedAtMs != before.RefreshOwedAtMs || after.RefreshOwedAttempts != 1 ||
		after.HeldUntilMs != before.HeldUntilMs || after.AuthWaitCredStampNs != before.AuthWaitCredStampNs ||
		after.AuthWaitCredSize != before.AuthWaitCredSize {
		t.Errorf("schedule state changed across the restart:\nbefore %+v\n after %+v", before, after)
	}
	if after.NextAttemptAtMs < before.NextAttemptAtMs || !claudeRunDebtRetryPending() {
		t.Errorf("rung %d -> %d pending=%v, want it re-armed, not spent", before.NextAttemptAtMs, after.NextAttemptAtMs, claudeRunDebtRetryPending())
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("request count=%d, want no request from the restart", got)
	}
	if !claudeUsageProbe.awaitingCredentialChange(claudeCredStamp{modNs: after.AuthWaitCredStampNs, size: after.AuthWaitCredSize}) {
		t.Error("the new process must still be waiting on the 401'd credential")
	}
}

// A 429 hold persisted before the update is paid when the hold ends — the
// replay books a rung at the hold's end instead of returning with nothing
// scheduled.
func TestClaudeOwedRefresh_PreUpdateHoldIsPaidWhenItEnds(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, claudeProbeOKHandler)
	pinClaudeRunDebtLadder(t, []time.Duration{50 * time.Millisecond}, 50*time.Millisecond, 20*time.Millisecond)
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	seedClaudeRefreshDebt(t, cache, "", now.Add(-time.Minute), 0, now.Add(300*time.Millisecond))

	simulateClaudeAgentRestart(t)
	if result := claudeRunDebtAttemptAt(time.Now(), claudeDebtTriggerStartup); result.code != claudeProbeHeld {
		t.Fatalf("result=%+v, want the replay held", result)
	}
	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("a held replay sent %d requests", got)
	}
	waitForClaudeCondition(t, 10*time.Second, "the held debt was never paid once the hold ended", func() bool {
		snap, ok := loadClaudeRateLimitSnapshot(cache)
		return ok && snap.RefreshOwedAtMs == 0
	})
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("request count=%d, want exactly the one request after the hold", got)
	}
}

// An expired stored token plus a credential rewrite after the restart
// converges without ever sending the expired token (no 401).
func TestClaudeOwedRefresh_ExpiredTokenConvergesOnCredentialRewrite(t *testing.T) {
	const expiredToken, freshToken = "sk-ant-oat-expired", "sk-ant-oat-refreshed-by-claude-code"
	var unauthorized int64
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+freshToken {
			atomic.AddInt64(&unauthorized, 1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		claudeProbeOKHandler(w, r)
	})
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	writeClaudeProbeCredentialExpiring(t, configDir, expiredToken, time.Now().Add(-time.Hour))
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	claudeOweRunRefresh(now.Add(-time.Minute))

	simulateClaudeAgentRestart(t)
	if result := claudeRunDebtAttemptAt(now, claudeDebtTriggerStartup); result.code != claudeProbeCredentialExpired {
		t.Fatalf("result=%+v, want the expired token held back", result)
	}

	// Claude Code's next run refreshes the token and rewrites the file; the
	// next gather sees the new stamp and nudges the pending rung.
	writeClaudeProbeCredentialExpiring(t, configDir, freshToken, time.Now().Add(8*time.Hour))
	_, stamp, ok := readClaudeCredentialsRawStamped(context.Background(), configDir)
	if !ok {
		t.Fatal("the rewritten credential is unreadable")
	}
	nudgeClaudeCredentialChanged(stamp)
	waitForClaudeCondition(t, 10*time.Second, "the rewritten credential never paid the debt", func() bool {
		snap, ok := loadClaudeRateLimitSnapshot(cache)
		return ok && snap.RefreshOwedAtMs == 0
	})
	if got := atomic.LoadInt64(&unauthorized); got != 0 {
		t.Errorf("the expired token reached the endpoint %d times, want 0", got)
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("request count=%d, want 1", got)
	}
}

// Explicit offline mode at start books a free rung rather than leaving the debt
// with nothing scheduled, and that rung pays the debt after SetOffline(false).
func TestClaudeOwedRefresh_OfflineStartIsPaidAfterReconnect(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, claudeProbeOKHandler)
	pinClaudeRunDebtLadder(t, []time.Duration{50 * time.Millisecond}, 50*time.Millisecond, 10*time.Millisecond)
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	claudeOweRunRefresh(now.Add(-time.Minute))
	SetOffline(true)
	t.Cleanup(func() { SetOffline(false) })

	simulateClaudeAgentRestart(t)
	payOwedClaudeUsageRefreshAt(time.Now())
	claudeFreshnessWaitIdle(t)
	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("an offline start sent %d requests", got)
	}
	if !claudeRunDebtRetryPending() || claudeCacheSnapshot(t, cache).NextAttemptAtMs == 0 {
		t.Fatal("an offline start must book a rung")
	}

	SetOffline(false)
	waitForClaudeCondition(t, 10*time.Second, "the offline rung never paid the debt after reconnecting", func() bool {
		snap, ok := loadClaudeRateLimitSnapshot(cache)
		return ok && snap.RefreshOwedAtMs == 0
	})
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("request count=%d, want 1", got)
	}
}

/* --------------------------------------------------------------------------
   The usage HINT survives the hand-off too: a reading the old process captured
   but never hinted is rotated onto the new epoch, recovered and hinted once
   (plus its follow-up), and the receipt names that generation.
   ------------------------------------------------------------------------ */

func numericClaudeEndpoint(w http.ResponseWriter, _ *http.Request) {
	reset := time.Now().Add(3 * time.Hour).Unix()
	fmt.Fprintf(w, `{"limits":[{"kind":"session","percent":41,"resets_at":%d},`+
		`{"kind":"weekly_all","percent":17,"resets_at":%d}]}`, reset, reset+86400)
}

func TestClaudeUpdateSurvival_APendingHintIsRecoveredByTheNextProcess(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, numericClaudeEndpoint)
	withCodexGenerationEpoch(t, 7301)
	rec, cfg := propagatorFixture(t)
	cliUsageHintDebounce = time.Hour // the hand-off wins the race
	startCLIUsagePropagator(cfg)

	// The old process: a probe reading commits and its hint is pending.
	payOwedClaudeUsageRefreshAt(time.Now()) // nothing owed: a no-op
	claudeUsageProbeAfterRun(time.Now())
	claudeFreshnessWaitIdle(t)
	if g := claudeGenerationOf(t, cache); g.Epoch != 7301 || g.Counter < 1 {
		t.Fatalf("the old process did not stamp its reading: %+v", g)
	}
	stopCLIUsagePropagator()
	if n := len(rec.all()); n != 0 {
		t.Fatalf("the old process sent %d hints; the scenario needs the hand-off to win", n)
	}

	// The new process: a new epoch, the same cache, no new reading.
	cliUsageHintDebounce = 20 * time.Millisecond
	simulateCodexProcessRestart(t, 7302)
	simulateClaudeAgentRestart(t)
	startCLIUsagePropagator(cfg)
	hints := waitHints(t, rec, 2, 2*cliUsageHintSpacing)
	if len(hints) != 2 {
		t.Fatalf("got %d hints, want the recovery hint and its one follow-up", len(hints))
	}
	h := hints[0].hint
	if h.Provider != claudeUsageProvider || h.GenerationEpoch != 7302 {
		t.Fatalf("hint = %+v, want claudeCode under the new epoch", h)
	}
	usage, _ := claudeCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{}, time.Now())
	if usage.UsageGeneration == nil || *usage.UsageGeneration != gen(h.GenerationEpoch, h.Generation) {
		t.Fatalf("receipt generation %+v does not match the hint %+v", usage.UsageGeneration, h)
	}
}

func TestClaudeUpdateSurvival_ASmokeDebtIsPaidAndHintedByTheNextProcess(t *testing.T) {
	cache, healthy := armPostUpdateEndpoint(t)
	seedStaleClaudeObservation(t, cache)
	withCodexGenerationEpoch(t, 7303)
	rec, cfg := propagatorFixture(t)

	// The old process owes the smoke's turn and is replaced before paying it.
	claudeOweRunRefreshNow(time.Now())
	claudeFreshnessWaitIdle(t)
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs == 0 {
		t.Fatal("the smoke's debt is not on disk")
	}

	healthy.Store(true)
	simulateCodexProcessRestart(t, 7304)
	simulateClaudeAgentRestart(t)
	startCLIUsagePropagator(cfg)
	payOwedClaudeUsageRefreshAt(claudeAfterFirstRung())
	waitForClaudeCondition(t, 10*time.Second, "the new process did not pay the smoke's debt", func() bool {
		snap, ok := loadClaudeRateLimitSnapshot(cache)
		return ok && snap.RefreshOwedAtMs == 0
	})
	claudeFreshnessWaitIdle(t)
	h := waitHints(t, rec, 1, 0)[0].hint
	if h.Provider != claudeUsageProvider || h.GenerationEpoch != 7304 {
		t.Fatalf("hint = %+v, want the paid reading under the new epoch", h)
	}
	requireNumericClaudeRows(t)
}
