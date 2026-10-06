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
