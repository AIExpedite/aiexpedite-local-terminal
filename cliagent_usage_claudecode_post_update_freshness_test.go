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
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
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

/* ------------------- the stale-mirror sequence, end to end ------------------ */

// fakeObservedHintService stands in for terminal-service: it verifies each
// signed hint, skips one whose generation it already applied, and otherwise
// dispatches the SAME signed refresh the agent answers in production
// (handleCLIUsageRefreshCommand), recording what that refresh published.
// reject makes it refuse a published receipt (stale, or zero mirrors), so it
// applies no watermark.
type fakeObservedHintService struct {
	t   *testing.T
	cfg *Config

	mu         sync.Mutex
	hints      []cliUsageObservedHint
	dispatches int
	published  []resultMsg
	applied    map[string]cliUsageGeneration
	reject     func(resultMsg) bool
	wg         sync.WaitGroup
}

func newFakeObservedHintService(t *testing.T, cfg *Config) *fakeObservedHintService {
	t.Helper()
	f := &fakeObservedHintService{t: t, cfg: cfg, applied: map[string]cliUsageGeneration{}}
	prevSend, prevPublish, prevGather := sendCLIUsageObservedHint, publishMsg, gatherCLIUsageForRefresh
	sendCLIUsageObservedHint = func(_ context.Context, url string, h cliUsageObservedHint) int {
		msg := buildCLIUsageObservedSignedMessage(cfg.AgentID, h.Timestamp, h.Provider, gen(h.GenerationEpoch, h.Generation))
		if h.Signature != generateHMAC(msg, cfg.CommandSecret) || !strings.Contains(url, "/device/"+cfg.AgentID+"/cli-usage/observed") {
			t.Errorf("hint failed HMAC verification: %+v", h)
			return 401
		}
		if h.Provider != "claudeCode" || h.GenerationEpoch <= 0 || h.Generation <= 0 {
			t.Errorf("hint = %+v, want a claudeCode generation", h)
			return 400
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.hints = append(f.hints, h)
		if a, ok := f.applied[h.Provider]; ok && a.covers(gen(h.GenerationEpoch, h.Generation)) {
			return 202 // already applied: no refresh
		}
		f.dispatches++
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.refresh()
		}()
		return 202
	}
	// The Claude provider alone, through its real parser: the device-level
	// detection a full gather needs is not what these rows exercise.
	gatherCLIUsageForRefresh = func(ctx context.Context) ([]cliAgentUsage, []cliAgentUsageError) {
		usage, ok := claudeCodeUsageParser{}.ParseContext(ctx, "", detectedCLIAgent{}, time.Now())
		if !ok {
			return []cliAgentUsage{}, nil
		}
		return []cliAgentUsage{*usage}, nil
	}
	publishMsg = func(_ context.Context, _ *pubsub.Publisher, res resultMsg) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.published = append(f.published, res)
		if f.reject != nil && f.reject(res) {
			return nil
		}
		for _, agent := range res.CliAgents {
			if agent.UsageGeneration != nil {
				f.applied[agent.Provider] = *agent.UsageGeneration
			}
		}
		return nil
	}
	t.Cleanup(func() {
		stopCLIUsagePropagator()
		f.wg.Wait()
		sendCLIUsageObservedHint, publishMsg, gatherCLIUsageForRefresh = prevSend, prevPublish, prevGather
	})
	return f
}

// refresh is one backend-dispatched `__cli_usage_refresh__`.
func (f *fakeObservedHintService) refresh() {
	f.mu.Lock()
	n := len(f.published) + 1
	f.mu.Unlock()
	cmd := commandMsg{ID: fmt.Sprintf("cmd-%d", n), RefreshID: fmt.Sprintf("refresh-%d", n), Ts: time.Now().UnixMilli()}
	if err := handleCLIUsageRefreshCommand(context.Background(), nil, cmd, f.cfg); err != nil {
		f.t.Errorf("refresh: %v", err)
	}
}

func (f *fakeObservedHintService) snapshot() (hints []cliUsageObservedHint, dispatches int, published []resultMsg) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cliUsageObservedHint(nil), f.hints...), f.dispatches, append([]resultMsg(nil), f.published...)
}

// claudeRowsOf returns the five-hour and weekly rows a published result
// carried for Claude.
func claudeRowsOf(t *testing.T, res resultMsg) (session, weekly cliAgentUsageMetric, generation *cliUsageGeneration) {
	t.Helper()
	for _, agent := range res.CliAgents {
		if agent.Provider != claudeUsageProvider {
			continue
		}
		for _, m := range agent.Metrics {
			switch {
			case m.Kind == limitKindSession:
				session = m
			case m.Kind == limitKindWeekly && m.Model == "":
				weekly = m
			}
		}
		return session, weekly, agent.UsageGeneration
	}
	t.Fatalf("published result carries no Claude entry: %+v", res)
	return
}

func requireRowCovers(t *testing.T, label string, row cliAgentUsageMetric, floor time.Time) {
	t.Helper()
	observed, err := time.Parse(time.RFC3339, row.ObservedAt)
	if row.Unknown || row.Consumed == nil || err != nil || observed.Before(floor.Truncate(time.Second)) {
		t.Fatalf("%s row = %+v, want a numeric reading observed at or after the run floor %s", label, row, floor.UTC().Format(time.RFC3339))
	}
}

// claudeStaleMirrorFixture is the reported device: a numeric reading from
// before the run that the backend mirror already applied, and the propagator
// running against a fake terminal-service. preRun is that reading's generation;
// the startup recovery's hint for it is answered without a refresh.
func claudeStaleMirrorFixture(t *testing.T) (cache string, svc *fakeObservedHintService, preRun cliUsageGeneration) {
	t.Helper()
	var healthy *atomic.Bool
	cache, healthy = armPostUpdateEndpoint(t)
	healthy.Store(true)
	// A run whose own attempt lost a race books a real retry rung: never let
	// it fire inside a later test.
	t.Cleanup(func() {
		stopClaudeRunDebtRetry()
		claudeFreshnessWaitIdle(t)
	})
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Minute))
	snap := claudeCacheSnapshot(t, cache)
	preRun = gen(snap.GenerationEpoch, snap.Generation)
	_, cfg := propagatorFixture(t)
	svc = newFakeObservedHintService(t, cfg)
	svc.applied[claudeUsageProvider] = preRun
	startCLIUsagePropagator(cfg)
	return cache, svc, preRun
}

// hintsAfter keeps the hints for generations newer than g.
func hintsAfter(hints []cliUsageObservedHint, g cliUsageGeneration) []cliUsageObservedHint {
	var out []cliUsageObservedHint
	for _, h := range hints {
		if !g.covers(gen(h.GenerationEpoch, h.Generation)) {
			out = append(out, h)
		}
	}
	return out
}

// runStaleMirrorSequence is the sequence the 09:00:04Z observation describes:
// a session-start refresh publishes the pre-run cache, then the run's covering
// probe commits — and that commit must wake ONE signed refresh by itself, with
// no further user action, whose rows are observed at or after the run.
func runStaleMirrorSequence(t *testing.T, run func()) {
	t.Helper()
	_, svc, preRun := claudeStaleMirrorFixture(t)

	svc.refresh() // session start / dispatch: the pre-run reading
	if _, _, published := svc.snapshot(); len(published) != 1 {
		t.Fatalf("session-start refresh published %d results", len(published))
	}

	floor := time.Now()
	run()
	waitForClaudeCondition(t, 15*time.Second, "the covering probe never woke a signed refresh", func() bool {
		_, dispatches, published := svc.snapshot()
		return dispatches >= 1 && len(published) >= 2
	})
	claudeFreshnessWaitIdle(t)
	time.Sleep(cliUsageHintSpacing / 2)
	all, dispatches, published := svc.snapshot()
	hints := hintsAfter(all, preRun)
	if len(hints) == 0 {
		t.Fatalf("no hint for a post-run generation: %+v", all)
	}
	first := hints[0]
	initial := 0
	for _, h := range hints {
		if time.UnixMilli(h.Timestamp).Before(time.UnixMilli(first.Timestamp).Add(cliUsageHintSpacing - 5*time.Millisecond)) {
			initial++
		}
	}
	if initial != 1 || dispatches != 1 {
		t.Fatalf("hints before the follow-up boundary = %d, dispatches = %d; want exactly one of each: %+v", initial, dispatches, hints)
	}
	session, weekly, generation := claudeRowsOf(t, published[len(published)-1])
	requireRowCovers(t, "five-hour", session, floor)
	requireRowCovers(t, "weekly", weekly, floor)
	if generation == nil || generation.Epoch != first.GenerationEpoch || generation.Counter < first.Generation {
		t.Fatalf("receipt generation %+v does not cover the hinted %d/%d", generation, first.GenerationEpoch, first.Generation)
	}
}

func TestClaudeStaleMirror_DirectRunCommitWakesOneSignedRefresh(t *testing.T) {
	skipIfUnsupportedOS(t)
	runStaleMirrorSequence(t, func() {
		tmpDir := installMockClaude(t, "claude-heartbeat-result")
		m := NewClaudeNativeManager(nil)
		id := fmt.Sprintf("claude-stale-mirror-%d", time.Now().UnixNano())
		if err := m.Start(id, tmpDir, nil, "hello", "ws", "uid", func(resultMsg) {}, nil); err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(func() { _ = m.End(id) })
	})
}

func TestClaudeStaleMirror_TerminalManagedRunCommitWakesOneSignedRefresh(t *testing.T) {
	skipIfUnsupportedOS(t)
	runStaleMirrorSequence(t, func() {
		sm, id := startManagedClaudeSession(t, "claude-heartbeat-result")
		t.Cleanup(func() { _ = sm.EndSession(id) })
	})
}

func TestClaudeStaleMirror_TerminalManagedAbnormalExitCommitWakesOneSignedRefresh(t *testing.T) {
	skipIfUnsupportedOS(t)
	runStaleMirrorSequence(t, func() {
		sm, id := startManagedClaudeSession(t, "claude-heartbeat-hang")
		time.Sleep(200 * time.Millisecond)
		if err := sm.EndSession(id); err != nil {
			t.Fatalf("EndSession: %v", err)
		}
	})
}

func TestClaudeStaleMirror_PassingSmokeCommitWakesOneSignedRefresh(t *testing.T) {
	runStaleMirrorSequence(t, func() {
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
	})
}

// A status-line commit (made by another process) is discovered by a signed
// refresh and travels in its receipt — which terminal-service then refuses
// (stale, or no mirror took it). No hint goes out at once; after the cooldown
// the ONE confirmation wakes a new refresh, and that one is applied.
func TestClaudeStaleMirror_ARejectedReceiptIsRecoveredByItsConfirmation(t *testing.T) {
	cache, svc, preRun := claudeStaleMirrorFixture(t)
	// Let the startup recovery's hint and follow-up for the pre-run reading
	// settle first.
	waitForClaudeCondition(t, 5*time.Second, "no startup recovery hint", func() bool {
		hints, _, _ := svc.snapshot()
		return len(hints) >= 2
	})
	svc.mu.Lock()
	svc.reject = func(resultMsg) bool { return len(svc.published) == 1 } // the first receipt only
	svc.mu.Unlock()

	// The hook's commit, written by "another process": the resident agent has
	// not seen it.
	snap := claudeCacheSnapshot(t, cache)
	observed := time.Now()
	snap.Buckets[claudeWindowFiveHour] = claudeRateLimitBucket{UsedPercentage: 52, ResetsAtMs: observed.Add(2 * time.Hour).UnixMilli(), ObservedAtMs: observed.UnixMilli()}
	snap.Buckets[claudeWindowSevenDay] = claudeRateLimitBucket{UsedPercentage: 23, ResetsAtMs: observed.Add(90 * time.Hour).UnixMilli(), ObservedAtMs: observed.UnixMilli()}
	snap.Generation++
	body, _ := json.Marshal(snap)
	if err := os.WriteFile(cache, body, 0o600); err != nil {
		t.Fatal(err)
	}
	external := gen(snap.GenerationEpoch, snap.Generation)

	publishedAt := time.Now()
	svc.refresh()
	// "Not hinted at once" is asserted by timestamp below (the confirmation
	// went at least one spacing after the publish); sampling after a fixed
	// sleep failed on slow runners once the confirmation itself was due.
	waitForClaudeCondition(t, 10*time.Second, "the confirmation never recovered the rejected receipt", func() bool {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		a, ok := svc.applied[claudeUsageProvider]
		return ok && a.covers(external)
	})
	time.Sleep(2 * cliUsageHintSpacing)
	all, dispatches, published := svc.snapshot()
	hints := hintsAfter(all, preRun)
	if len(hints) != 1 || dispatches != 1 || len(published) != 2 {
		t.Fatalf("hints=%d dispatches=%d published=%d, want one confirmation, one refresh, two results", len(hints), dispatches, len(published))
	}
	if at := time.UnixMilli(hints[0].Timestamp); at.Sub(publishedAt) < cliUsageHintSpacing-5*time.Millisecond {
		t.Fatalf("the confirmation went %s after the rejected receipt, want >= %s", at.Sub(publishedAt), cliUsageHintSpacing)
	}
	session, _, _ := claudeRowsOf(t, published[1])
	if session.Consumed == nil || *session.Consumed != 52 {
		t.Fatalf("recovered five-hour row = %+v, want the status-line reading", session)
	}
}
