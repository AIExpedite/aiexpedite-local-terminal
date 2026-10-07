package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   opencode_native_usage_test.go — the resident chat (runOneShot) and a
   terminal `opencode run` session each commit their turn's step usage exactly
   once, against the compiled stub (opencode_stub_test.go).
   ------------------------------------------------------------------------ */

// openCodeUsageStubStdout is the stub's stdout for one turn: a tool-call step,
// then the final step that closes the turn.
func openCodeUsageStubStdout(session string, now time.Time) string {
	return strings.Join([]string{
		fmt.Sprintf(`{"type":"session.created","sessionID":%q}`, session),
		`{"type":"text","text":"done"}`,
		fmt.Sprintf(`{"type":"step_finish","sessionID":%q,"part":{"id":"prt_tool","type":"step-finish","reason":"tool-calls","cost":0.01,"tokens":{"input":100,"output":10,"reasoning":0,"cache":{"read":0,"write":0}},"time":{"end":%d}}}`, session, now.UnixMilli()),
		fmt.Sprintf(`{"type":"step_finish","sessionID":%q,"part":{"id":"prt_stop","type":"step-finish","reason":"stop","cost":0.02,"tokens":{"input":200,"output":20,"reasoning":5,"cache":{"read":0,"write":0}},"time":{"end":%d}}}`, session, now.UnixMilli()),
		"",
	}, "\\n")
}

func TestOpenCodeNativeUsage_AResidentTurnCommitsItsStepsOnce(t *testing.T) {
	openCodeUsageFixture(t, 1301)
	installOpenCodeStub(t)
	now := time.Now()
	t.Setenv("OPENCODE_STUB_STDOUT", openCodeUsageStubStdout("ses_native", now))

	m := NewOpenCodeNativeManager()
	injectOpenCodeSession(t, m, "sess-usage", t.TempDir())
	if err := m.Send("sess-usage", "do it", func(resultMsg) {}, 60*time.Second); err != nil {
		t.Fatalf("Send: %v", err)
	}
	openCodeUsageInFlight.Wait()

	fp := openCodeKnownAccountFingerprint(resolveOpenCodeExecutable())
	b, ok, g := openCodeUsageBucketForDay(fp, now)
	if !ok || b.tokens() != 335 || b.CostUsd < 0.0299 || b.CostUsd > 0.0301 {
		t.Fatalf("bucket = %+v, want both steps (335 tokens, $0.03)", b)
	}
	if g.Counter != 1 {
		t.Fatalf("generation = %+v, want one commit", g)
	}
	if debts := loadOpenCodeUsageLedger().Debts; len(debts) != 0 {
		t.Fatalf("debts = %+v after a captured turn", debts)
	}
}

func TestOpenCodeNativeUsage_ATerminalSessionCommitsOnceThoughBothSettlesFire(t *testing.T) {
	openCodeUsageFixture(t, 1302)
	installOpenCodeStub(t)
	now := time.Now()
	t.Setenv("OPENCODE_STUB_STDOUT", openCodeUsageStubStdout("ses_terminal", now))

	sm := NewSessionManager(nil)
	ended, publishFn := openCodeSessionEndedSignal()
	id := fmt.Sprintf("opencode-usage-%d", time.Now().UnixNano())
	if err := sm.StartSession(id, "opencode", []string{"run", "--format", "json", "hello"},
		t.TempDir(), "ws", "uid", 15000, false, publishFn); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	select {
	case <-ended:
	case <-time.After(20 * time.Second):
		t.Fatal("session never ended")
	}
	openCodeUsageInFlight.Wait()

	fp := openCodeKnownAccountFingerprint("opencode")
	b, ok, g := openCodeUsageBucketForDay(fp, now)
	if !ok || b.tokens() != 335 {
		t.Fatalf("bucket = %+v, want the session's 335 tokens", b)
	}
	if g.Counter != 1 {
		t.Fatalf("generation = %+v, want exactly one commit", g)
	}
	if debts := loadOpenCodeUsageLedger().Debts; len(debts) != 0 {
		t.Fatalf("debts = %+v after a captured session", debts)
	}
}

// A resumed terminal turn whose stream never names its session (it ended before
// any frame did) still owes its export: the resume id it was launched with is
// the session, so the debt is not dropped as unattributable.
func TestOpenCodeNativeUsage_AResumedSessionOwesUnderItsResumeID(t *testing.T) {
	openCodeUsageFixture(t, 1305)
	installOpenCodeStub(t)
	t.Setenv("OPENCODE_STUB_STDOUT", `{"type":"text","text":"done"}\n`)

	sm := NewSessionManager(nil)
	ended, publishFn := openCodeSessionEndedSignal()
	id := fmt.Sprintf("opencode-usage-resume-%d", time.Now().UnixNano())
	if err := sm.StartSessionResuming(id, "opencode", []string{"run", "--format", "json", "hello"},
		t.TempDir(), "ws", "uid", 15000, false, "ses_resumed", publishFn); err != nil {
		t.Fatalf("StartSessionResuming: %v", err)
	}
	select {
	case <-ended:
	case <-time.After(20 * time.Second):
		t.Fatal("session never ended")
	}
	openCodeUsageInFlight.Wait()

	debts := loadOpenCodeUsageLedger().Debts
	if len(debts) != 1 || !debts[0].owed() || debts[0].SessionID != "ses_resumed" {
		t.Fatalf("debts = %+v, want one owed debt under the resume id", debts)
	}
}

// A resumed resident turn records its resume id on the armed debt at spawn,
// before any frame names the session: a crash or self-update mid-turn then
// leaves a debt the next process can export, not an unattributable one.
func TestOpenCodeNativeUsage_AResumedResidentTurnPersistsItsResumeIDAtSpawn(t *testing.T) {
	openCodeUsageFixture(t, 1307)
	installOpenCodeStub(t)
	t.Setenv("OPENCODE_STUB_STDOUT", `{"type":"text","text":"done"}\n`)
	t.Setenv("OPENCODE_STUB_SLEEP_MS", "3000")

	m := NewOpenCodeNativeManager()
	sess := injectOpenCodeSession(t, m, "sess-usage-resume", t.TempDir())
	sess.NativeSessionID = "ses_known"
	sess.Transcript = []openCodeTurn{{Role: "user", Content: "earlier"}}

	sent := make(chan error, 1)
	go func() { sent <- m.Send("sess-usage-resume", "follow up", func(resultMsg) {}, 60*time.Second) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		debts := loadOpenCodeUsageLedger().Debts
		if len(debts) == 1 && !debts[0].owed() && debts[0].SessionID == "ses_known" {
			break
		}
		select {
		case err := <-sent:
			t.Fatalf("turn ended (err=%v) before its armed debt carried the resume id; debts = %+v", err, debts)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("debts = %+v, want one armed debt under the resume id mid-turn", debts)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := <-sent; err != nil {
		t.Fatalf("Send: %v", err)
	}
	openCodeUsageInFlight.Wait()
}

// A turn whose stdout passes the cumulative cap still counts every step: the
// drain keeps reading usage, so the final model step after the overflow (and
// one past an oversize frame) is not lost while the earlier tool step settles
// the run as paid.
func TestOpenCodeNativeUsage_StepsAfterAStdoutOverflowAreStillCounted(t *testing.T) {
	openCodeUsageFixture(t, 1303)
	now := time.Now().UnixMilli()
	padding := strings.Repeat("x", 3*1024*1024)
	var out strings.Builder
	out.WriteString(openCodeStepFinish("ses_overflow", "prt_tool", 100, 0, 0, "0", now) + "\n")
	for i := 0; i < 3; i++ {
		out.WriteString(padding + "\n")
	}
	out.WriteString(openCodeStepFinish("ses_overflow", "prt_final", 20, 0, 0, "0", now) + "\n")
	out.WriteString(strings.Repeat("y", openCodeNativeMaxFrameBytes+1) + "\n")
	out.WriteString(openCodeStepFinish("ses_overflow", "prt_late", 3, 0, 0, "0", now) + "\n")

	run := armOpenCodeUsageRun("opencode", "", "fp-overflow")
	m := NewOpenCodeNativeManager()
	state := m.streamOpenCodeEvents(&OpenCodeNativeSession{}, strings.NewReader(out.String()), nil, run)
	if !state.overflow {
		t.Fatal("the stream did not report its overflow")
	}
	run.mu.Lock()
	var input int64
	for _, s := range run.steps {
		input += s.Input
	}
	n := len(run.steps)
	run.mu.Unlock()
	if n != 3 || input != 123 {
		t.Fatalf("captured %d steps with %d input tokens, want all 3 (123)", n, input)
	}
}

// The step-finish frame that itself pushes stdout past the cap is counted too:
// an earlier step would otherwise settle the run as paid with this one lost.
func TestOpenCodeNativeUsage_TheStepThatCrossesTheStdoutCapIsCounted(t *testing.T) {
	openCodeUsageFixture(t, 1304)
	now := time.Now().UnixMilli()
	first := openCodeStepFinish("ses_cross", "prt_tool", 100, 0, 0, "0", now)
	crossing := openCodeStepFinish("ses_cross", "prt_cross", 7, 0, 0, "0", now)
	chunk := 3 * 1024 * 1024
	rest := openCodeNativeMaxStdout - len(first) - 2*chunk - len(crossing) + 1
	var out strings.Builder
	out.WriteString(first + "\n")
	out.WriteString(strings.Repeat("x", chunk) + "\n")
	out.WriteString(strings.Repeat("x", chunk) + "\n")
	out.WriteString(strings.Repeat("x", rest) + "\n")
	out.WriteString(crossing + "\n")

	run := armOpenCodeUsageRun("opencode", "", "fp-cross")
	m := NewOpenCodeNativeManager()
	state := m.streamOpenCodeEvents(&OpenCodeNativeSession{}, strings.NewReader(out.String()), nil, run)
	if !state.overflow {
		t.Fatal("the crossing frame did not overflow the stream")
	}
	run.mu.Lock()
	var input int64
	for _, s := range run.steps {
		input += s.Input
	}
	n := len(run.steps)
	run.mu.Unlock()
	if n != 2 || input != 107 {
		t.Fatalf("captured %d steps with %d input tokens, want both (107)", n, input)
	}
}
