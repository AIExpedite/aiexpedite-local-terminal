package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_opencode_freshness_test.go — the freshness regressions: a
   successful smoke becomes a number on the card, a stream with no usage owes a
   bounded export, and the ladder is bounded in attempts, age and time.
   ------------------------------------------------------------------------ */

// stubOpenCodeExport replaces the export seam; calls counts invocations.
func stubOpenCodeExport(t *testing.T, fn func(ctx context.Context, sessionID string) ([]byte, bool)) *int {
	t.Helper()
	calls := 0
	prev := runOpenCodeExport
	runOpenCodeExport = func(ctx context.Context, _, _, sessionID string) ([]byte, bool) {
		calls++
		return fn(ctx, sessionID)
	}
	t.Cleanup(func() { runOpenCodeExport = prev })
	return &calls
}

// stubOpenCodeExecutable makes resolveOpenCodeExecutable find a binary (never
// run: the export seam is stubbed).
func stubOpenCodeExecutable(t *testing.T) {
	t.Helper()
	installOpenCodeStub(t)
}

// openCodeExportJSON is `opencode export` output with one assistant message.
func openCodeExportJSON(messageID string, createdMs int64, input, output int64, cost float64) []byte {
	return []byte(fmt.Sprintf(`{"info":{"id":"ses_x","title":"secret prompt title"},"messages":[`+
		`{"info":{"id":"msg_user","role":"user","time":{"created":%d}},"parts":[{"type":"text","text":"user prompt text"}]},`+
		`{"info":{"id":%q,"role":"assistant","cost":%g,"tokens":{"input":%d,"output":%d,"reasoning":0,"cache":{"read":1,"write":0}},"time":{"created":%d,"completed":%d}},"parts":[]}]}`,
		createdMs-1, messageID, cost, input, output, createdMs, createdMs+10))
}

func TestOpenCodeUsage_SuccessfulSmokeThenGatherShowsTokensToday(t *testing.T) {
	openCodeSmokeEnv(t)
	openCodeUsageFixture(t, 1001)
	path := stubOpenCodeBinary(t)
	stubOpenCodeReadiness(t, "anthropic/claude-sonnet-4-5\n", true)
	smokeStart := time.Now()
	stubOpenCodeSmokeExec(t, func(_ context.Context, launch openCodeLaunch) ([]byte, []byte, error) {
		marker := openCodeMarkerFromLaunch(t, launch)
		frames := string(openCodeSuccessFrames(marker))
		frames += openCodeStepFinish("ses_probe", "prt_smoke", 1200, 34, 5, "0.0123", time.Now().UnixMilli()) + "\n"
		return []byte(frames), nil, nil
	})

	if result := runOpenCodeSmoke(context.Background(), path, "1.2.0"); result.Status != cliSmokeStatusSuccess {
		t.Fatalf("smoke = %+v, want success", result)
	}

	usage, ok := openCodeUsageParser{}.ParseContext(context.Background(), t.TempDir(), detectedCLIAgent{Path: path}, time.Now())
	if !ok {
		t.Fatal("parser reported nothing")
	}
	tokens := openCodeMetricByLabel(t, usage.Metrics, "Tokens today (agent runs)")
	if tokens.Consumed == nil || *tokens.Consumed != 1239 || tokens.Total != nil || tokens.Remaining != nil {
		t.Fatalf("tokens row = %+v, want consumed 1239 and no total", tokens)
	}
	observed, err := time.Parse(time.RFC3339, tokens.ObservedAt)
	if err != nil || observed.Before(smokeStart.Truncate(time.Second)) {
		t.Fatalf("observedAt %q is not at or after the smoke (%s)", tokens.ObservedAt, smokeStart)
	}
	if cost := openCodeMetricByLabel(t, usage.Metrics, "Cost today"); cost.Unit != usageUnitUSD || *cost.Consumed != 0.0123 {
		t.Fatalf("cost row = %+v", cost)
	}
	if usage.UsageGeneration == nil || usage.UsageGeneration.Epoch != 1001 || usage.UsageGeneration.Counter != 1 {
		t.Fatalf("usage generation = %+v, want {1001,1}", usage.UsageGeneration)
	}
	// The hint is pending for the propagator.
	cliUsagePropagator.mu.Lock()
	pending := cliUsagePropagator.pending[openCodeUsageProvider]
	cliUsagePropagator.mu.Unlock()
	if pending == nil || pending.generation != *usage.UsageGeneration {
		t.Fatalf("pending hint = %+v, want the committed generation", pending)
	}
}

func openCodeMetricByLabel(t *testing.T, metrics []cliAgentUsageMetric, label string) cliAgentUsageMetric {
	t.Helper()
	for _, m := range metrics {
		if m.Label == label {
			return m
		}
	}
	t.Fatalf("no %q row in %+v", label, metrics)
	return cliAgentUsageMetric{}
}

func TestOpenCodeUsage_NoUsageInTheStreamOwesAnExportPaidOnRungTwo(t *testing.T) {
	sched := openCodeUsageFixture(t, 1002)
	stubOpenCodeExecutable(t)
	var now time.Time
	attempt := 0
	calls := stubOpenCodeExport(t, func(context.Context, string) ([]byte, bool) {
		attempt++
		if attempt == 1 {
			return []byte(`{"messages":[]}`), true // not flushed yet
		}
		return openCodeExportJSON("msg_a", now.UnixMilli(), 300, 40, 0), true
	})

	run := armOpenCodeUsageRun("opencode", t.TempDir(), "fp-a")
	now = time.Now() // the run's assistant message: after its floor, before its settle
	captureOpenCodeUsageLine(run, `{"type":"session.created","sessionID":"ses_owed"}`)
	openCodeUsageInFlight.Wait()
	settleOpenCodeUsageRun(run, "")

	ledger := loadOpenCodeUsageLedger()
	if len(ledger.Debts) != 1 || !ledger.Debts[0].owed() || ledger.Debts[0].SessionID != "ses_owed" {
		t.Fatalf("debts = %+v, want one owed debt written at settle", ledger.Debts)
	}
	if d := sched.delays(); len(d) != 1 || d[0] != 15*time.Second {
		t.Fatalf("booked %v, want the first rung at 15s", d)
	}

	sched.fireNext() // rung 1: export has nothing yet
	if d := sched.delays(); len(d) != 2 || d[1] != time.Minute {
		t.Fatalf("booked %v, want the second rung at 1m", d)
	}
	sched.fireNext() // rung 2 pays it
	if *calls != 2 {
		t.Fatalf("export ran %d times, want 2", *calls)
	}
	if left := loadOpenCodeUsageLedger().Debts; len(left) != 0 {
		t.Fatalf("debt survived payment: %+v", left)
	}
	if b, ok, _ := openCodeUsageBucketForDay("fp-a", now); !ok || b.tokens() != 340 {
		t.Fatalf("bucket = %+v, want the exported 340 tokens", b)
	}
}

// A turn whose only step_finish reports zeros produces no number, so it has not
// been paid: the debt stands and the export pays it. Without that, an install
// that reports its spend late (or in a shape the stream does not carry) would
// retire its debt against nothing and leave the card numberless.
func TestOpenCodeUsage_AZeroOnlyStepIsNotPaymentAndTheExportPaysIt(t *testing.T) {
	sched := openCodeUsageFixture(t, 1009)
	stubOpenCodeExecutable(t)
	var now time.Time
	calls := stubOpenCodeExport(t, func(context.Context, string) ([]byte, bool) {
		return openCodeExportJSON("msg_z", now.UnixMilli(), 500, 60, 0.5), true
	})

	run := armOpenCodeUsageRun("opencode", t.TempDir(), "fp-z")
	now = time.Now()
	captureOpenCodeUsageLine(run, openCodeZeroStepFinish("ses_zero", "prt_z", now.UnixMilli()))
	openCodeUsageInFlight.Wait()
	settleOpenCodeUsageRun(run, "")

	ledger := loadOpenCodeUsageLedger()
	if len(ledger.Debts) != 1 || !ledger.Debts[0].owed() || ledger.Debts[0].SessionID != "ses_zero" {
		t.Fatalf("debts = %+v, want the zero-only turn still owed", ledger.Debts)
	}
	sched.fireNext()
	if *calls != 1 {
		t.Fatalf("export ran %d times, want one attempt", *calls)
	}
	if b, ok, _ := openCodeUsageBucketForDay("fp-z", now); !ok || b.tokens() != 560 {
		t.Fatalf("bucket = %+v, want the exported 560 tokens", b)
	}
	if left := loadOpenCodeUsageLedger().Debts; len(left) != 0 {
		t.Fatalf("debt survived payment: %+v", left)
	}
}

func TestOpenCodeUsage_ExportCountsOnlyThisRunsMessages(t *testing.T) {
	floor := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC).UnixMilli()
	debt := openCodeUsageDebt{SessionID: "ses_x", RunFloorMs: floor, SettledAtMs: floor + 60_000}
	out := []byte(fmt.Sprintf(`{"messages":[`+
		`{"info":{"id":"m_before","role":"assistant","tokens":{"input":1},"time":{"created":%d}}},`+
		`{"info":{"id":"m_in","role":"assistant","tokens":{"input":2},"time":{"created":%d}}},`+
		`{"info":{"id":"m_after","role":"assistant","tokens":{"input":4},"time":{"created":%d}}},`+
		`{"id":"m_flat","role":"assistant","tokens":{"input":8},"time":{"created":%d}},`+
		`{"info":{"id":"m_user","role":"user","tokens":{"input":16},"time":{"created":%d}}}]}`,
		floor-1, floor+1, floor+120_000, floor+2, floor+3))
	var total int64
	for _, s := range parseOpenCodeExportUsage(out, debt) {
		total += s.Input
	}
	if total != 2+8 {
		t.Fatalf("export paid %d, want only this run's assistant messages (10)", total)
	}
	if steps := parseOpenCodeExportUsage([]byte(`{"unexpected":"shape"}`), debt); len(steps) != 0 {
		t.Fatalf("an unknown shape yielded %+v, want nothing", steps)
	}
}

func TestOpenCodeUsage_ExportSumsAMultiStepMessagesParts(t *testing.T) {
	floor := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC).UnixMilli()
	debt := openCodeUsageDebt{SessionID: "ses_x", RunFloorMs: floor, SettledAtMs: floor + 60_000}
	// info.tokens is the LAST step's (OpenCode overwrites it per step); the
	// step-finish parts carry each step's own figures; info.cost is cumulative.
	out := []byte(fmt.Sprintf(`{"messages":[`+
		`{"info":{"id":"m_multi","role":"assistant","tokens":{"input":30,"output":3},"cost":0.06,"time":{"created":%d}},`+
		`"parts":[{"type":"text"},`+
		`{"type":"step-finish","tokens":{"input":10,"output":1},"cost":0.02},`+
		`{"type":"tool"},`+
		`{"type":"step-finish","tokens":{"input":20,"output":2},"cost":0.01},`+
		`{"type":"step-finish","tokens":{"input":30,"output":3},"cost":0.03}]},`+
		`{"info":{"id":"m_info_only","role":"assistant","tokens":{"input":7},"time":{"created":%d}}}]}`,
		floor+1, floor+2))
	steps := parseOpenCodeExportUsage(out, debt)
	if len(steps) != 2 {
		t.Fatalf("got %d steps, want 2: %+v", len(steps), steps)
	}
	multi := steps[0]
	if multi.Input != 60 || multi.Output != 6 {
		t.Fatalf("multi-step message = %d in / %d out, want the summed parts 60 / 6", multi.Input, multi.Output)
	}
	if multi.Cost < 0.0599 || multi.Cost > 0.0601 {
		t.Fatalf("multi-step cost = %v, want 0.06", multi.Cost)
	}
	if steps[1].Input != 7 {
		t.Fatalf("a message without parts = %d in, want its info figure 7", steps[1].Input)
	}
}

func TestOpenCodeUsage_OfflineRefusalsSpendNoAttempts(t *testing.T) {
	sched := openCodeUsageFixture(t, 1003)
	stubOpenCodeExecutable(t)
	calls := stubOpenCodeExport(t, func(context.Context, string) ([]byte, bool) { return nil, false })
	setCodexTestOffline(t, true)

	run := armOpenCodeUsageRun("opencode", "", "fp-a")
	settleOpenCodeUsageRun(run, "ses_resumed")
	for i := 0; i < 5; i++ {
		sched.fireNext()
	}
	if *calls != 0 {
		t.Fatalf("export ran %d times while offline", *calls)
	}
	ledger := loadOpenCodeUsageLedger()
	if len(ledger.Debts) != 1 || ledger.Debts[0].Attempts != 0 {
		t.Fatalf("debts = %+v, want the debt kept with no attempt spent", ledger.Debts)
	}
	for _, d := range sched.delays()[1:] {
		if d < openCodeUsageFreeFloor || d > openCodeUsageDebtLadder[len(openCodeUsageDebtLadder)-1] {
			t.Fatalf("free retry booked at %s, outside [floor, longest rung]", d)
		}
	}
}

func TestOpenCodeUsage_AttemptsAreBoundedAndTheDebtAgesOut(t *testing.T) {
	sched := openCodeUsageFixture(t, 1004)
	stubOpenCodeExecutable(t)
	calls := stubOpenCodeExport(t, func(context.Context, string) ([]byte, bool) { return nil, false })

	run := armOpenCodeUsageRun("opencode", "", "fp-a")
	settleOpenCodeUsageRun(run, "ses_never")
	for sched.fireNext() {
	}
	if *calls != openCodeUsageDebtMaxAttempts {
		t.Fatalf("export ran %d times, want the %d-attempt budget", *calls, openCodeUsageDebtMaxAttempts)
	}
	if want := []time.Duration{15 * time.Second, time.Minute, 5 * time.Minute}; fmt.Sprint(sched.delays()) != fmt.Sprint(want) {
		t.Fatalf("ladder = %v, want %v", sched.delays(), want)
	}
	if left := loadOpenCodeUsageLedger().Debts; len(left) != 0 {
		t.Fatalf("an exhausted debt survived: %+v", left)
	}

	// Age-out: a debt owed for longer than the limit is retired without a read.
	aged := armOpenCodeUsageRun("opencode", "", "fp-a")
	settleOpenCodeUsageRun(aged, "ses_old")
	openCodeUsageFreshnessNow = func() time.Time { return time.Now().Add(openCodeUsageDebtMaxAge + time.Minute) }
	before := *calls
	sched.fireNext()
	if *calls != before {
		t.Fatal("an aged-out debt was still exported")
	}
	if left := loadOpenCodeUsageLedger().Debts; len(left) != 0 {
		t.Fatalf("an aged-out debt survived: %+v", left)
	}
}

func TestOpenCodeUsage_TheExportIsKilledAtItsTimeoutAndNeverBlocksSettle(t *testing.T) {
	sched := openCodeUsageFixture(t, 1005)
	stubOpenCodeExecutable(t)
	prevTimeout := openCodeExportTimeout
	openCodeExportTimeout = 50 * time.Millisecond
	t.Cleanup(func() { openCodeExportTimeout = prevTimeout })
	stubOpenCodeExport(t, func(ctx context.Context, _ string) ([]byte, bool) {
		<-ctx.Done() // a hung export
		return nil, false
	})

	run := armOpenCodeUsageRun("opencode", "", "fp-a")
	settled := make(chan struct{})
	go func() {
		settleOpenCodeUsageRun(run, "ses_hung")
		close(settled)
	}()
	select {
	case <-settled:
	case <-time.After(2 * time.Second):
		t.Fatal("settle waited on the export")
	}
	start := time.Now()
	sched.fireNext()
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("attempt ran %s past its %s timeout", took, openCodeExportTimeout)
	}
	if d := loadOpenCodeUsageLedger().Debts; len(d) != 1 || d[0].Attempts != 1 {
		t.Fatalf("debts = %+v, want one attempt spent on the killed export", d)
	}
}

func TestOpenCodeUsage_AStepAfterTheTerminalEventStillCommits(t *testing.T) {
	openCodeUsageFixture(t, 1007)
	session := &CLISession{Command: "opencode"}
	session.openCodeUsageRun = armOpenCodeUsageRun("opencode", "", "fp-a")
	now := time.Now()
	captureOpenCodeUsageLine(session.openCodeUsageRun, openCodeStepFinish("ses_a", "prt_1", 10, 0, 0, "0", now.UnixMilli()))
	session.settleCLIUsageRun(true) // terminal event, off the stream goroutine
	openCodeUsageInFlight.Wait()
	captureOpenCodeUsageLine(session.openCodeUsageRun, openCodeStepFinish("ses_a", "prt_2", 5, 0, 0, "0", now.UnixMilli()))
	session.settleCLIUsageRun(false) // exit
	session.settleCLIUsageRun(false) // a repeated exit path adds nothing
	if b, _, _ := openCodeUsageBucketForDay("fp-a", now); b.InputTokens != 15 {
		t.Fatalf("input = %d, want 15", b.InputTokens)
	}
	if debts := loadOpenCodeUsageLedger().Debts; len(debts) != 0 {
		t.Fatalf("the run's debt was not retired: %+v", debts)
	}
}

// A terminal session settled on its terminal event can stream another step
// before it exits: the streamed step is keyed by its part id and an exported
// message by its message id, so an export that paid the debt meanwhile would
// count the same turn twice. The export stands down while the run is live
// (spending no attempt) and is released by the exit path.
func TestOpenCodeUsage_TheExportStandsDownWhileTheRunCanStillCommit(t *testing.T) {
	sched := openCodeUsageFixture(t, 1012)
	stubOpenCodeExecutable(t)
	now := time.Now()
	calls := stubOpenCodeExport(t, func(context.Context, string) ([]byte, bool) {
		return openCodeExportJSON("msg_1", now.UnixMilli(), 300, 40, 0.5), true
	})

	session := &CLISession{Command: "opencode"}
	session.openCodeUsageRun = armOpenCodeUsageRun("opencode", "", "fp-live")
	run := session.openCodeUsageRun
	captureOpenCodeUsageLine(run, `{"type":"session.created","sessionID":"ses_live"}`)
	session.settleCLIUsageRun(true) // terminal event: nothing captured yet, so the turn owes
	openCodeUsageInFlight.Wait()
	if d := loadOpenCodeUsageLedger().Debts; len(d) != 1 || !d[0].owed() {
		t.Fatalf("debts = %+v, want one owed debt", d)
	}

	sched.fireNext() // the export's first rung, with the run still streaming
	if *calls != 0 {
		t.Fatalf("export ran %d times while the run was live", *calls)
	}
	if d := loadOpenCodeUsageLedger().Debts; len(d) != 1 || d[0].Attempts != 0 {
		t.Fatalf("debts = %+v, want the deferral to spend no attempt", d)
	}

	// The step the terminal event raced lands, and the exit path commits it.
	captureOpenCodeUsageLine(run, openCodeStepFinish("ses_live", "prt_late", 300, 40, 0, "0.5", now.UnixMilli()))
	session.settleCLIUsageRun(false)
	if d := loadOpenCodeUsageLedger().Debts; len(d) != 0 {
		t.Fatalf("debts = %+v, want the stream's commit to retire the debt", d)
	}
	// The deferred attempt is re-booked at the ladder head and finds nothing owed.
	for sched.fireNext() {
	}
	if *calls != 0 {
		t.Fatalf("export ran %d times for a turn the stream already paid", *calls)
	}
	if b, ok, _ := openCodeUsageBucketForDay("fp-live", now); !ok || b.tokens() != 340 || b.CostUsd != 0.5 {
		t.Fatalf("bucket = %+v, want the turn counted exactly once", b)
	}
}

// Once the run is over, the same debt is exported: the deferral is a wait on
// the live stream, never a refusal to pay.
func TestOpenCodeUsage_AReleasedRunExportsOnTheRebookedRung(t *testing.T) {
	sched := openCodeUsageFixture(t, 1013)
	stubOpenCodeExecutable(t)
	var now time.Time
	calls := stubOpenCodeExport(t, func(context.Context, string) ([]byte, bool) {
		return openCodeExportJSON("msg_1", now.UnixMilli(), 300, 40, 0), true
	})

	session := &CLISession{Command: "opencode"}
	session.openCodeUsageRun = armOpenCodeUsageRun("opencode", "", "fp-released")
	now = time.Now() // the run's assistant message: after the floor the arm just took
	captureOpenCodeUsageLine(session.openCodeUsageRun, `{"type":"session.created","sessionID":"ses_released"}`)
	session.settleCLIUsageRun(true)
	openCodeUsageInFlight.Wait()
	sched.fireNext() // deferred: the run is live
	session.settleCLIUsageRun(false)
	sched.fireNext() // released: the export pays the debt
	if *calls != 1 {
		t.Fatalf("export ran %d times, want 1 once the run was released", *calls)
	}
	if d := loadOpenCodeUsageLedger().Debts; len(d) != 0 {
		t.Fatalf("debts = %+v, want the export to have paid it", d)
	}
	if b, ok, _ := openCodeUsageBucketForDay("fp-released", now); !ok || b.tokens() != 340 {
		t.Fatalf("bucket = %+v, want the exported 340 tokens", b)
	}
}

// The session id comes from the child's own stdout and ends up on the export's
// argv — through cmd.exe when OpenCode is an npm shim. One shaped like a flag or
// carrying shell syntax is never stored, so nothing is owed for it and nothing
// is exported.
func TestOpenCodeUsage_AHostileSessionIDNeverReachesTheExport(t *testing.T) {
	sched := openCodeUsageFixture(t, 1008)
	stubOpenCodeExecutable(t)
	calls := stubOpenCodeExport(t, func(context.Context, string) ([]byte, bool) { return nil, false })
	for _, hostile := range []string{"--format", "ses_a&calc", `ses_b" | del`, "ses c", strings.Repeat("a", 200)} {
		run := armOpenCodeUsageRun("opencode", "", "fp-a")
		line, _ := json.Marshal(map[string]string{"type": "session.created", "sessionID": hostile})
		captureOpenCodeUsageLine(run, string(line))
		openCodeUsageInFlight.Wait()
		settleOpenCodeUsageRun(run, hostile)
	}
	if debts := loadOpenCodeUsageLedger().Debts; len(debts) != 0 {
		t.Fatalf("debts = %+v, want hostile ids dropped as unattributable", debts)
	}
	for sched.fireNext() {
	}
	if *calls != 0 {
		t.Fatalf("export ran %d times for a hostile id", *calls)
	}
	// A ledger that already holds one (hand-edited, an older build) is retired
	// at its attempt, never exported.
	openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		ledger.Debts = append(ledger.Debts, openCodeUsageDebt{RunID: "r", RunFloorMs: 1, SessionID: "-x", OwedAtMs: time.Now().UnixMilli()})
		return true, false
	})
	attemptOpenCodeUsageDebt("r")
	if *calls != 0 || len(loadOpenCodeUsageLedger().Debts) != 0 {
		t.Fatalf("a stored hostile id was exported (%d) or kept", *calls)
	}
	if !isValidOpenCodeSessionID("ses_01JXYZabc-9") {
		t.Fatal("a real OpenCode id must stay valid")
	}
}
