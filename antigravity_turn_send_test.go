package main

// End-to-end failed-turn contract through SendTurn with a fake `agy`: what the
// person reads, the failureCategory, the stderr frame's exit code, the local
// diagnostics, and the session / conversation state afterwards.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeAgyEnv struct {
	base    string
	diagDir string
	bin     string
}

// installFakeAgy puts a shell `agy` on PATH whose body is script. The body
// can call `agylog "<message>"` to append a glog line to this run's own PID
// block (in a per-run `cli-<stamp>.log`, as agy 1.5.10 writes it).
func installFakeAgy(t *testing.T, script string) *fakeAgyEnv {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell shim for fake agy is unix-oriented")
	}
	env := &fakeAgyEnv{
		base:    filepath.Join(t.TempDir(), ".gemini", "antigravity-cli"),
		diagDir: t.TempDir(),
		bin:     t.TempDir(),
	}
	if err := os.MkdirAll(antigravityLogDir(env.base), 0o755); err != nil {
		t.Fatal(err)
	}
	prevBase, prevDiag := antigravityRunLogBase, antigravityTurnDiagnosticsDir
	antigravityRunLogBase = func() string { return env.base }
	antigravityTurnDiagnosticsDir = func() string { return env.diagDir }
	resetAntigravityLogIndex()
	t.Cleanup(func() {
		antigravityRunLogBase, antigravityTurnDiagnosticsDir = prevBase, prevDiag
		resetAntigravityLogIndex()
	})
	header := fmt.Sprintf(`#!/bin/sh
LOG='%s'/cli-$(date +%%Y%%m%%d_%%H%%M%%S).log
agylog() { printf 'E%%s %%s.000000 %%7d x.go:1] %%s\n' "$(date +%%m%%d)" "$(date +%%H:%%M:%%S)" $$ "$1" >> "$LOG"; }
printf 'I%%s %%s.000000 %%7d main.go:1] Starting language server process with pid %%d\n' "$(date +%%m%%d)" "$(date +%%H:%%M:%%S)" $$ $$ >> "$LOG"
`, antigravityLogDir(env.base))
	if err := os.WriteFile(filepath.Join(env.bin, "agy"), []byte(header+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", env.bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return env
}

func injectAntigravitySession(t *testing.T, m *AntigravityNativeManager, id, nativeID string) *AntigravityNativeSession {
	t.Helper()
	s := &AntigravityNativeSession{
		ID: id, Cwd: t.TempDir(), WorkspaceID: "ws", UID: "uid",
		StartedAt: time.Now(), status: "idle", NativeConversationID: nativeID,
	}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	t.Cleanup(func() { _ = m.End(id) })
	return s
}

type agyFrameLog struct {
	mu     sync.Mutex
	frames []resultMsg
}

func (f *agyFrameLog) publish(r resultMsg) {
	f.mu.Lock()
	f.frames = append(f.frames, r)
	f.mu.Unlock()
}

func (f *agyFrameLog) ofType(typ string) []resultMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []resultMsg
	for _, r := range f.frames {
		if r.Type == typ {
			out = append(out, r)
		}
	}
	return out
}

func initEventScript() string {
	return "printf '%s\\n' '" + aie2InitLine() + "'\n"
}

func TestAntigravitySend_InitThenExit3PublishesUnknownNotInit(t *testing.T) {
	env := installFakeAgy(t, initEventScript()+"exit 3\n")
	m := NewAntigravityNativeManager(nil)
	s := injectAntigravitySession(t, m, "s-init-exit3", "")
	var fl agyFrameLog
	if err := m.Send(s.ID, "hello", fl.publish, 10*time.Second); err == nil {
		t.Fatal("expected a failed turn")
	}
	errs := fl.ofType("antigravity_native_error")
	if len(errs) != 1 {
		t.Fatalf("want exactly one error frame, got %d", len(errs))
	}
	want := "Antigravity stopped with exit code 3 before reporting a reason, so this message did not finish. " +
		"Send the message again; if it repeats, check Antigravity on that computer. " +
		"The session is still open — send another message to try again."
	if errs[0].Output != want || errs[0].FailureCategory != antigravityFailureUnknown {
		t.Fatalf("frame = %q / %q", errs[0].Output, errs[0].FailureCategory)
	}
	if s.Status() != "idle" || s.NativeConversationID != "" || len(s.Transcript) != 0 {
		t.Fatalf("session after a failed first turn: status=%s id=%q transcript=%d", s.Status(), s.NativeConversationID, len(s.Transcript))
	}
	// Diagnostics: one file, no init tool list, no prompt.
	files, _ := os.ReadDir(env.diagDir)
	if len(files) != 1 {
		t.Fatalf("diagnostics files = %d, want 1", len(files))
	}
	raw, _ := os.ReadFile(filepath.Join(env.diagDir, files[0].Name()))
	if strings.Contains(string(raw), "browser_tool") || strings.Contains(string(raw), "hello") {
		t.Fatalf("diagnostics kept the init tool list or the prompt: %s", raw)
	}
	var d antigravityTurnDiagnostics
	if err := json.Unmarshal(raw, &d); err != nil || d.ExitCode != 3 || d.Category != antigravityFailureUnknown || d.Source != "none" {
		t.Fatalf("diagnostics = %+v (%v)", d, err)
	}
}

func TestAntigravitySend_QuotaFromRunsOwnLogPublishesWrapper(t *testing.T) {
	// The AIE2 shape: init, then the two 1.5.10 failure lines in the run's
	// own log, exit 3 within seconds — before any in-run poll.
	installFakeAgy(t, initEventScript()+
		`agylog "agent executor error: generating and executing: `+aie2QuotaReason+`"`+"\n"+
		`agylog "Print mode: run ended with error and no response: `+aie2QuotaReason+`"`+"\n"+
		"exit 3\n")
	m := NewAntigravityNativeManager(nil)
	s := injectAntigravitySession(t, m, "s-quota", "")
	var fl agyFrameLog
	start := time.Now()
	_ = m.Send(s.ID, "hello", fl.publish, 10*time.Second)
	if time.Since(start) > 5*time.Second {
		t.Fatalf("publish took %s, want within 5s of exit", time.Since(start))
	}
	errs := fl.ofType("antigravity_native_error")
	if len(errs) != 1 {
		t.Fatalf("want exactly one error frame, got %d", len(errs))
	}
	if !strings.HasPrefix(errs[0].Output, "[Antigravity turn failed: "+aie2QuotaReason+"] Wait until the quota resets") ||
		strings.Count(errs[0].Output, antigravityQuotaWrapperPrefix) != 1 ||
		errs[0].FailureCategory != antigravityFailureQuota {
		t.Fatalf("frame = %q / %q", errs[0].Output, errs[0].FailureCategory)
	}
}

func TestAntigravitySend_StderrSignedOutBeatsGenericResult(t *testing.T) {
	installFakeAgy(t, initEventScript()+
		`echo '{"event":"result","result":{"status":"ERROR","error":"turn failed"}}'`+"\n"+
		"echo 'Error: not logged in' >&2\nexit 1\n")
	m := NewAntigravityNativeManager(nil)
	s := injectAntigravitySession(t, m, "s-signedout", "")
	var fl agyFrameLog
	_ = m.Send(s.ID, "hello", fl.publish, 10*time.Second)
	errs := fl.ofType("antigravity_native_error")
	if len(errs) != 1 || errs[0].FailureCategory != antigravityFailureSignedOut ||
		!strings.HasPrefix(errs[0].Output, "Antigravity is not signed in on "+getDeviceName()+". Sign in to Antigravity on that computer") {
		t.Fatalf("frames = %+v", errs)
	}
	stderr := fl.ofType("antigravity_native_stderr")
	if len(stderr) != 1 || stderr[0].ExitCode != 1 {
		t.Fatalf("the stderr frame must carry the exit code: %+v", stderr)
	}
}

func TestAntigravitySend_UnrecognisedStderrAndPlainStdoutAreCLIDefect(t *testing.T) {
	cases := map[string]struct{ script, want string }{
		"stderr only":  {"echo 'segfault in planner' >&2\nexit 2\n", "Antigravity failed: segfault in planner "},
		"plain stdout": {"echo 'fatal: cannot open db'\nexit 2\n", "Antigravity failed: fatal: cannot open db "},
		"log only":     {`agylog "Print mode: run ended with error and no response: planner exploded"` + "\nexit 3\n", "Antigravity failed: planner exploded "},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			installFakeAgy(t, initEventScript()+tc.script)
			m := NewAntigravityNativeManager(nil)
			s := injectAntigravitySession(t, m, "s-defect", "")
			var fl agyFrameLog
			_ = m.Send(s.ID, "hello", fl.publish, 10*time.Second)
			errs := fl.ofType("antigravity_native_error")
			if len(errs) != 1 || errs[0].FailureCategory != antigravityFailureCLIDefect || !strings.HasPrefix(errs[0].Output, tc.want) {
				t.Fatalf("frames = %+v", errs)
			}
		})
	}
}

func TestAntigravitySend_SuccessResultWithNonzeroExitIsFailure(t *testing.T) {
	installFakeAgy(t, initEventScript()+
		`echo '{"event":"result","result":{"status":"SUCCESS","response":"looks done","conversation_id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}}'`+"\nexit 4\n")
	m := NewAntigravityNativeManager(nil)
	s := injectAntigravitySession(t, m, "s-success-nonzero", "")
	var fl agyFrameLog
	if err := m.Send(s.ID, "hello", fl.publish, 10*time.Second); err == nil {
		t.Fatal("a non-zero exit is a failure even with a successful result")
	}
	if len(fl.ofType("antigravity_native_message")) != 0 || s.NativeConversationID != "" {
		t.Fatalf("no response published and no id stored: id=%q", s.NativeConversationID)
	}
	errs := fl.ofType("antigravity_native_error")
	if len(errs) != 1 || strings.Contains(errs[0].Output, "looks done") {
		t.Fatalf("assistant text must not be the error: %+v", errs)
	}
}

func TestAntigravitySend_FailureAfterSuccessKeepsConversationID(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran-once")
	installFakeAgy(t, fmt.Sprintf(`if [ ! -f '%s' ]; then
  : > '%s'
  echo '{"event":"init","conversation_id":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}'
  echo '{"event":"result","result":{"status":"SUCCESS","response":"first answer","conversation_id":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}}'
  exit 0
fi
echo '{"event":"result","result":{"status":"ERROR","error":"backend unavailable"}}'
exit 3
`, marker, marker))
	m := NewAntigravityNativeManager(nil)
	s := injectAntigravitySession(t, m, "s-keep-id", "")
	var fl agyFrameLog
	if err := m.Send(s.ID, "first", fl.publish, 10*time.Second); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if s.NativeConversationID != "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb" || len(s.Transcript) != 2 {
		t.Fatalf("after success: id=%q transcript=%d", s.NativeConversationID, len(s.Transcript))
	}
	if err := m.Send(s.ID, "second", fl.publish, 10*time.Second); err == nil {
		t.Fatal("second turn should fail")
	}
	if s.NativeConversationID != "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb" || len(s.Transcript) != 2 || s.Status() != "idle" {
		t.Fatalf("a failure must keep the earlier id and transcript: id=%q transcript=%d status=%s",
			s.NativeConversationID, len(s.Transcript), s.Status())
	}
}

func TestAntigravitySend_NoCategoryRowsEndWithNewSessionSentence(t *testing.T) {
	installFakeAgy(t, initEventScript()+"echo 'error: unknown model \"gemini-9\"' >&2\nexit 2\n")
	m := NewAntigravityNativeManager(nil)
	s := injectAntigravitySession(t, m, "s-launch", "")
	var fl agyFrameLog
	_ = m.Send(s.ID, "hello", fl.publish, 10*time.Second)
	errs := fl.ofType("antigravity_native_error")
	if len(errs) != 1 || errs[0].FailureCategory != antigravityFailureInvalidLaunchOption ||
		!strings.HasSuffix(errs[0].Output, "Start a new session with another model or effort. "+antigravityClosingNewSession) {
		t.Fatalf("frames = %+v", errs)
	}
}

func TestAntigravityRunOneShot_SpawnedArgvEqualsApprovedGateArgs(t *testing.T) {
	env := installFakeAgy(t, `printf '%s\n' "$@" > "$(dirname "$0")/argv"
echo '{"event":"result","result":{"status":"SUCCESS","response":"ok"}}'
`)
	launch := []string{"--model", "gemini-4-hydrogen", "--effort", "medium"}
	s := &AntigravityNativeSession{ID: "argv", status: "idle", LaunchArgs: launch}
	run := NewAntigravityNativeManager(nil).runOneShot(s, t.TempDir(), filepath.Join(env.bin, "agy"), "p", "", 10*time.Second, "test:argv")
	if run.startErr != nil || run.resultText != "ok" {
		t.Fatalf("run = %+v", run)
	}
	raw, _ := os.ReadFile(filepath.Join(env.bin, "argv"))
	got := strings.Fields(string(raw))
	want := buildAntigravityNativeGateArgs(launch)
	if strings.Join(got, " ") != strings.Join(want, " ") || strings.Join(run.args, " ") != strings.Join(want, " ") {
		t.Fatalf("spawned %q, approved %q", got, want)
	}
}
