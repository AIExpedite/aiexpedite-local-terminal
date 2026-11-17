package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// shortenAntigravityMonitor makes the monitor's bounds test-sized.
func shortenAntigravityMonitor(t *testing.T, mode string) {
	t.Helper()
	prev := []any{antigravityInteractionStopMode, antigravityStallBound, antigravityMonitorTick, antigravityStopKillGrace, antigravityPermissionSettle}
	antigravityInteractionStopMode = mode
	antigravityStallBound = 1 * time.Second
	antigravityMonitorTick = 100 * time.Millisecond
	antigravityStopKillGrace = 200 * time.Millisecond
	antigravityPermissionSettle = 500 * time.Millisecond
	t.Cleanup(func() {
		antigravityInteractionStopMode = prev[0].(string)
		antigravityStallBound = prev[1].(time.Duration)
		antigravityMonitorTick = prev[2].(time.Duration)
		antigravityStopKillGrace = prev[3].(time.Duration)
		antigravityPermissionSettle = prev[4].(time.Duration)
	})
}

const (
	askQuestionStep    = `{"event":"step_update","step_update":{"step_index":2,"state":"ACTIVE","step_type":"tool","tool_name":"ask_question"}}`
	askPermissionStep  = `{"event":"step_update","step_update":{"step_index":3,"state":"ACTIVE","step_type":"tool","tool_name":"ask_permission","title":"Allow editing package.json?"}}`
	askPermissionDone  = `{"event":"step_update","step_update":{"step_index":3,"state":"DONE","step_type":"tool","tool_name":"ask_permission"}}`
	editStepActive     = `{"event":"step_update","step_update":{"step_index":4,"state":"ACTIVE","step_type":"tool","tool_name":"edit_file"}}`
	editStepDone       = `{"event":"step_update","step_update":{"step_index":4,"state":"DONE","step_type":"tool","tool_name":"edit_file"}}`
	successResultEvent = `{"event":"result","result":{"status":"SUCCESS","response":"done"}}`
)

func echoLines(lines ...string) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString("printf '%s\\n' '" + l + "'\n")
	}
	return b.String()
}

func sendAndTime(t *testing.T, m *AntigravityNativeManager, id string, fl *agyFrameLog) (error, time.Duration) {
	start := time.Now()
	err := m.Send(id, "hello", fl.publish, 60*time.Second)
	return err, time.Since(start)
}

func TestAntigravityMonitor_StallAfterInitStopsAsStalled(t *testing.T) {
	shortenAntigravityMonitor(t, antigravityInteractionObserve)
	installFakeAgy(t, initEventScript()+"sleep 30\n")
	m := NewAntigravityNativeManager(nil)
	s := injectAntigravitySession(t, m, "s-stall", "")
	var fl agyFrameLog
	err, took := sendAndTime(t, m, s.ID, &fl)
	if err == nil || took > 8*time.Second {
		t.Fatalf("err=%v took=%s", err, took)
	}
	errs := fl.ofType("antigravity_native_error")
	if len(errs) != 1 || errs[0].FailureCategory != antigravityFailureStalled ||
		!strings.HasPrefix(errs[0].Output, "Antigravity produced nothing for 3 minutes after starting. Send the message again") {
		t.Fatalf("want exactly one stalled failure, got %+v", errs)
	}
	if s.Status() != "idle" {
		t.Fatalf("status = %s", s.Status())
	}
}

func TestAntigravityMonitor_OutputAfterInitIsNeverStalled(t *testing.T) {
	shortenAntigravityMonitor(t, antigravityInteractionObserve)
	installFakeAgy(t, initEventScript()+echoLines(editStepActive)+"sleep 2\n"+echoLines(editStepDone, successResultEvent))
	m := NewAntigravityNativeManager(nil)
	s := injectAntigravitySession(t, m, "s-not-stalled", "")
	var fl agyFrameLog
	if err, _ := sendAndTime(t, m, s.ID, &fl); err != nil {
		t.Fatalf("a turn with output after init must not stall: %v", err)
	}
}

func TestAntigravityMonitor_ObserveModeOnlyLogsAskQuestion(t *testing.T) {
	shortenAntigravityMonitor(t, antigravityInteractionObserve)
	installFakeAgy(t, initEventScript()+echoLines(askQuestionStep)+"sleep 1\n"+echoLines(successResultEvent))
	m := NewAntigravityNativeManager(nil)
	s := injectAntigravitySession(t, m, "s-observe", "")
	var fl agyFrameLog
	if err, _ := sendAndTime(t, m, s.ID, &fl); err != nil {
		t.Fatalf("observe mode must let the turn continue: %v", err)
	}
	if len(fl.ofType("antigravity_native_error")) != 0 {
		t.Fatal("observe mode published a failure")
	}
}

func TestAntigravityMonitor_EnforceAskQuestionStopsWithinBound(t *testing.T) {
	shortenAntigravityMonitor(t, antigravityInteractionEnforce)
	installFakeAgy(t, initEventScript()+echoLines(askQuestionStep)+"sleep 30\n")
	m := NewAntigravityNativeManager(nil)
	s := injectAntigravitySession(t, m, "s-ask", "")
	var fl agyFrameLog
	err, took := sendAndTime(t, m, s.ID, &fl)
	if err == nil || took > 5*time.Second {
		t.Fatalf("err=%v took=%s, want interaction_required within 5s", err, took)
	}
	errs := fl.ofType("antigravity_native_error")
	if len(errs) != 1 || errs[0].FailureCategory != antigravityFailureInteraction ||
		!strings.HasPrefix(errs[0].Output, `Antigravity asked "ask_question", which can't be answered in this unattended session. Rephrase the task`) {
		t.Fatalf("frames = %+v", errs)
	}
}

func TestAntigravityMonitor_EnforcePermissionResolvedWithinSettleContinues(t *testing.T) {
	shortenAntigravityMonitor(t, antigravityInteractionEnforce)
	installFakeAgy(t, initEventScript()+echoLines(askPermissionStep)+"sleep 0.1\n"+
		echoLines(askPermissionDone, editStepActive, editStepDone)+"sleep 1\n"+echoLines(successResultEvent))
	m := NewAntigravityNativeManager(nil)
	s := injectAntigravitySession(t, m, "s-perm-ok", "")
	var fl agyFrameLog
	if err, _ := sendAndTime(t, m, s.ID, &fl); err != nil {
		t.Fatalf("an auto-approved permission must not stop the turn: %v", err)
	}
}

func TestAntigravityMonitor_EnforceUnresolvedPermissionStops(t *testing.T) {
	shortenAntigravityMonitor(t, antigravityInteractionEnforce)
	installFakeAgy(t, initEventScript()+echoLines(askPermissionStep)+"sleep 30\n")
	m := NewAntigravityNativeManager(nil)
	s := injectAntigravitySession(t, m, "s-perm-stuck", "")
	var fl agyFrameLog
	err, took := sendAndTime(t, m, s.ID, &fl)
	if err == nil || took > 5*time.Second {
		t.Fatalf("err=%v took=%s", err, took)
	}
	errs := fl.ofType("antigravity_native_error")
	if len(errs) != 1 || errs[0].FailureCategory != antigravityFailureInteraction ||
		!strings.Contains(errs[0].Output, `"Allow editing package.json?"`) {
		t.Fatalf("frames = %+v", errs)
	}
}

func TestAntigravityMonitor_EnforcePendingLineInOwnLogStops(t *testing.T) {
	shortenAntigravityMonitor(t, antigravityInteractionEnforce)
	installFakeAgy(t, initEventScript()+echoLines(editStepActive)+
		`agylog "ask_permission pending: waiting for approval"`+"\nsleep 30\n")
	m := NewAntigravityNativeManager(nil)
	s := injectAntigravitySession(t, m, "s-log-pending", "")
	var fl agyFrameLog
	err, took := sendAndTime(t, m, s.ID, &fl)
	if err == nil || took > 6*time.Second {
		t.Fatalf("err=%v took=%s", err, took)
	}
	errs := fl.ofType("antigravity_native_error")
	if len(errs) != 1 || errs[0].FailureCategory != antigravityFailureInteraction {
		t.Fatalf("frames = %+v", errs)
	}
}

func TestAntigravityMonitor_NeighbourBlockPendingLineIsIgnored(t *testing.T) {
	shortenAntigravityMonitor(t, antigravityInteractionEnforce)
	base := filepath.Join(t.TempDir(), ".gemini", "antigravity-cli")
	if err := os.MkdirAll(antigravityLogDir(base), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := antigravityRunLogBase
	antigravityRunLogBase = func() string { return base }
	resetAntigravityLogIndex()
	t.Cleanup(func() { antigravityRunLogBase = prev; resetAntigravityLogIndex() })

	now := time.Now()
	stamp := now.Format("0102 15:04:05")
	body := fmt.Sprintf("I%s.000000 111 main.go:1] Starting language server process with pid 111\n"+
		"E%s.000000 111 x.go:1] ask_question pending: waiting for an answer\n"+
		"I%s.000000 222 main.go:1] Starting language server process with pid 222\n"+
		"I%s.000000 222 x.go:1] working\n", stamp, stamp, stamp, stamp)
	name := "cli-" + now.In(antigravityLogZone()).Format("20060102_150405") + ".log"
	if err := os.WriteFile(filepath.Join(antigravityLogDir(base), name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for pid, wantStop := range map[int]bool{222: false, 111: true} {
		mon := newAntigravityTurnMonitor("s", now.Add(-time.Second), 1024, nil, nil)
		mon.pid = pid
		mon.followLog(now)
		if mon.stopped.Load() != wantStop {
			t.Fatalf("pid %d: stopped=%v, want %v", pid, mon.stopped.Load(), wantStop)
		}
	}
}

// A process that ignores the interrupt is force-killed, and is gone when the
// failure is published.
func TestAntigravityMonitor_IgnoredInterruptIsForceKilled(t *testing.T) {
	shortenAntigravityMonitor(t, antigravityInteractionEnforce)
	env := installFakeAgy(t, "echo $$ > \"$(dirname \"$0\")/pid\"\ntrap '' INT TERM\n"+initEventScript()+echoLines(askQuestionStep)+
		"while true; do sleep 1; done\n")
	m := NewAntigravityNativeManager(nil)
	s := injectAntigravitySession(t, m, "s-ignore", "")
	var fl agyFrameLog
	err, took := sendAndTime(t, m, s.ID, &fl)
	if err == nil || took > 5*time.Second {
		t.Fatalf("err=%v took=%s", err, took)
	}
	raw, _ := os.ReadFile(filepath.Join(env.bin, "pid"))
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	if pid <= 0 {
		t.Fatal("no pid recorded")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("the stopped process must be gone when the failure is published (kill -0: %v)", err)
	}
}

// installUnkillableTurnTree makes the force-kill a no-op until restore runs,
// so a stopped turn's tree outlives it.
func installUnkillableTurnTree(t *testing.T) (restore func()) {
	t.Helper()
	prev := antigravityKillTurnTree
	var disabled = true
	antigravityKillTurnTree = func(cmd *exec.Cmd) {
		if !disabled {
			prev(cmd)
		}
	}
	t.Cleanup(func() { antigravityKillTurnTree = prev })
	return func() { disabled = false }
}

func TestAntigravityLingering_SurvivingTreeStillPublishesThenShutsDownNextSend(t *testing.T) {
	shortenAntigravityMonitor(t, antigravityInteractionEnforce)
	restore := installUnkillableTurnTree(t)
	prevWait := antigravityLingeringWait
	antigravityLingeringWait = 1 * time.Second
	t.Cleanup(func() { antigravityLingeringWait = prevWait })

	marker := filepath.Join(t.TempDir(), "first-done")
	installFakeAgy(t, fmt.Sprintf(`if [ ! -f '%s' ]; then
  : > '%s'
  trap '' INT TERM
  %s  %s  while true; do sleep 1; done
fi
%s`, marker, marker, initEventScript(), echoLines(askQuestionStep), echoLines(successResultEvent)))
	m := NewAntigravityNativeManager(nil)
	s := injectAntigravitySession(t, m, "s-linger", "")
	s.NativeConversationID = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	var fl agyFrameLog

	// 1. The stopped turn's process survives the (disabled) force-kill: the
	// failure is still published within the bound.
	err, took := sendAndTime(t, m, s.ID, &fl)
	if err == nil || took > 5*time.Second {
		t.Fatalf("err=%v took=%s, want a failure within 5s", err, took)
	}

	// 2. The next send waits only for that tree; its own wait runs out.
	err, took = sendAndTime(t, m, s.ID, &fl)
	if err == nil || err.Error() != antigravityShuttingDownText {
		t.Fatalf("want the shutting-down failure, got %v (after %s)", err, took)
	}
	errs := fl.ofType("antigravity_native_error")
	last := errs[len(errs)-1]
	if last.Output != antigravityShuttingDownText || last.FailureCategory != "" {
		t.Fatalf("shutting-down frame = %q / %q", last.Output, last.FailureCategory)
	}
	if s.NativeConversationID != "cccccccc-cccc-cccc-cccc-cccccccccccc" || len(s.Transcript) != 0 || s.Status() != "idle" {
		t.Fatalf("state must be unchanged: id=%q transcript=%d status=%s", s.NativeConversationID, len(s.Transcript), s.Status())
	}

	// 3. Once the kill works again, a later send's own wait sees the tree go
	// and starts normally.
	restore()
	if err, _ := sendAndTime(t, m, s.ID, &fl); err != nil {
		t.Fatalf("the send after the tree is gone must start normally: %v", err)
	}
	if len(fl.ofType("antigravity_native_message")) != 1 {
		t.Fatal("no response published")
	}
}

func TestAntigravityLingering_UnrelatedAgyIsNotWaitedFor(t *testing.T) {
	env := installFakeAgy(t, echoLines(successResultEvent))
	// An unrelated agy (a usage probe) running on the computer.
	probe := exec.Command("sleep", "30")
	if err := probe.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = probe.Process.Kill(); _ = probe.Wait() })
	_ = env
	m := NewAntigravityNativeManager(nil)
	s := injectAntigravitySession(t, m, "s-unrelated", "")
	var fl agyFrameLog
	err, took := sendAndTime(t, m, s.ID, &fl)
	if err != nil || took > 3*time.Second {
		t.Fatalf("err=%v took=%s — an unrelated process must not hold the turn", err, took)
	}
}
