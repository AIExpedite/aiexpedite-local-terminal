// codex_appserver_test.go
// -----------------------------------------------------------------------------
// Unit + lifecycle tests for CodexAppServerManager. Unit tests pin the argv
// builder / env sanitizer / Send validation. The lifecycle test drives a real
// CodexAppServerManager against the test binary running in
// `TEST_MOCK_CLI_MODE=codex-appserver-echo` so we don't need a real Codex
// install on the test host.
// -----------------------------------------------------------------------------

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   argv builder
   -------------------------------------------------------------------------- */

func TestBuildCodexAppServerArgs_DefaultsToStdio(t *testing.T) {
	got := buildCodexAppServerArgs(nil)
	want := []string{"app-server", "--listen", "stdio://"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildCodexAppServerArgs(nil) = %#v, want %#v", got, want)
	}
}

func TestBuildCodexAppServerArgs_ForwardsExtraArgs(t *testing.T) {
	got := buildCodexAppServerArgs([]string{"-c", `model="gpt-5.4"`, "-c", `profile="work"`})
	want := []string{"app-server", "--listen", "stdio://", "-c", `model="gpt-5.4"`, "-c", `profile="work"`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildCodexAppServerArgs = %#v, want %#v", got, want)
	}
}

func TestBuildCodexAppServerArgs_StripsDuplicateAppServerToken(t *testing.T) {
	got := buildCodexAppServerArgs([]string{"app-server", "-c", `model="gpt-5.4"`})
	want := []string{"app-server", "--listen", "stdio://", "-c", `model="gpt-5.4"`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildCodexAppServerArgs = %#v, want %#v", got, want)
	}
}

func TestBuildCodexAppServerArgs_StripsCallerListenOverride(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"separate_value", []string{"--listen", "ws://127.0.0.1:4500", "-c", `model="gpt-5.4"`}},
		{"equals_form", []string{"--listen=ws://127.0.0.1:4500", "-c", `model="gpt-5.4"`}},
		{"unix_socket", []string{"--listen", "unix:///tmp/codex.sock"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildCodexAppServerArgs(c.args)
			// The result must always start with --listen stdio:// and must
			// never contain the caller's --listen override anywhere downstream.
			if len(got) < 3 || got[0] != "app-server" || got[1] != "--listen" || got[2] != "stdio://" {
				t.Fatalf("expected built-in `app-server --listen stdio://` prefix; got %#v", got)
			}
			for _, a := range got[3:] {
				lower := strings.ToLower(a)
				if lower == "--listen" || strings.HasPrefix(lower, "--listen=") {
					t.Fatalf("--listen override leaked into final argv: %#v", got)
				}
				if strings.HasPrefix(lower, "ws://") || strings.HasPrefix(lower, "unix://") {
					t.Fatalf("caller transport value leaked into final argv: %#v", got)
				}
			}
		})
	}
}

/* --------------------------------------------------------------------------
   env sanitizer
   -------------------------------------------------------------------------- */

func TestSanitizeCodexAppServerEnv_StripsConflictingVars(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"CLAUDECODE=1",
		"CLAUDE_CODE_ENTRYPOINT=cli",
		"CODEX_IDE_VERSION=0.1.0",
		"CODEX_HOME=/home/user/.codex",
		"OPENAI_API_KEY=sk-abc",
		"HOME=/home/user",
	}
	got := sanitizeCodexAppServerEnv(in)
	wantPresent := []string{"PATH=/usr/bin", "CODEX_HOME=/home/user/.codex", "OPENAI_API_KEY=sk-abc", "HOME=/home/user"}
	wantAbsent := []string{"CLAUDECODE=1", "CLAUDE_CODE_ENTRYPOINT=cli", "CODEX_IDE_VERSION=0.1.0"}

	for _, w := range wantPresent {
		if !envContains(got, w) {
			t.Errorf("expected env to retain %q; got %v", w, got)
		}
	}
	for _, w := range wantAbsent {
		if envContains(got, w) {
			t.Errorf("expected env to strip %q; got %v", w, got)
		}
	}
}

func envContains(env []string, target string) bool {
	for _, e := range env {
		if e == target {
			return true
		}
	}
	return false
}

/* --------------------------------------------------------------------------
   isCodexAppServerCommand
   -------------------------------------------------------------------------- */

func TestIsCodexAppServerCommand(t *testing.T) {
	cases := map[string]bool{
		"codex_appserver_start": true,
		"codex_appserver_send":  true,
		"codex_appserver_end":   true,
		"session_start":         false,
		"execute":               false,
		"":                      false,
		"codex_appserver_other": false,
	}
	for in, want := range cases {
		if got := isCodexAppServerCommand(in); got != want {
			t.Errorf("isCodexAppServerCommand(%q) = %v, want %v", in, got, want)
		}
	}
}

/* --------------------------------------------------------------------------
   Send validation (no process required)
   -------------------------------------------------------------------------- */

func TestCodexAppServerManager_Send_RejectsInvalidPayloads(t *testing.T) {
	m := NewCodexAppServerManager()
	// We don't even start a session — Send should reject before looking at
	// the pipe for an unknown session id, but the empty/newline checks fire
	// on a known session. Use a placeholder fixture session that's already
	// "ended" so Send() short-circuits without trying to actually write.
	id := "test-fixture"
	fixture := &CodexAppServerSession{
		ID:         id,
		status:     "ended",
		done:       make(chan struct{}),
		streamDone: make(chan struct{}),
	}
	close(fixture.done)
	close(fixture.streamDone)
	m.sessions[id] = fixture

	cases := []struct {
		name    string
		payload string
		wantErr string
	}{
		{"empty", "", "payload is empty"},
		{"whitespace_only", "   \t  ", "payload is empty"},
		{"embedded_newline", `{"jsonrpc":"2.0","id":1` + "\n" + `,"method":"initialize"}`, "must be a single line"},
		{"embedded_crlf", `{"jsonrpc":"2.0"}` + "\r\n" + `{"method":"x"}`, "must be a single line"},
		{"not_json", `oops not json`, "not valid JSON"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := m.Send(id, c.payload)
			if err == nil {
				t.Fatalf("expected error containing %q; got nil", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("expected error containing %q; got %q", c.wantErr, err.Error())
			}
		})
	}
}

func TestCodexAppServerManager_Send_NotFound(t *testing.T) {
	m := NewCodexAppServerManager()
	err := m.Send("missing", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected `not found` error; got %v", err)
	}
}

func TestCodexAppServerManager_Send_EndedSession(t *testing.T) {
	m := NewCodexAppServerManager()
	id := "ended-fixture"
	fixture := &CodexAppServerSession{ID: id, status: "ended", done: make(chan struct{}), streamDone: make(chan struct{})}
	close(fixture.done)
	close(fixture.streamDone)
	m.sessions[id] = fixture

	err := m.Send(id, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if err == nil || !strings.Contains(err.Error(), "has ended") {
		t.Fatalf("expected `has ended` error; got %v", err)
	}
}

/* --------------------------------------------------------------------------
   Manager registry
   -------------------------------------------------------------------------- */

func TestCodexAppServerManager_StartRejectsDuplicateID(t *testing.T) {
	m := NewCodexAppServerManager()
	id := "dupe-fixture"
	m.sessions[id] = &CodexAppServerSession{ID: id, status: "running", done: make(chan struct{}), streamDone: make(chan struct{})}

	publishFn := func(resultMsg) {}
	err := m.Start(id, "", nil, "ws", "uid", publishFn)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected `already exists` error; got %v", err)
	}
}

func TestCodexAppServerManager_StartRequiresIDAndPublish(t *testing.T) {
	m := NewCodexAppServerManager()
	if err := m.Start("", "", nil, "ws", "uid", func(resultMsg) {}); err == nil {
		t.Fatalf("expected error for empty sessionID")
	}
	if err := m.Start("x", "", nil, "ws", "uid", nil); err == nil {
		t.Fatalf("expected error for nil publishFn")
	}
}

/* --------------------------------------------------------------------------
   Stale-session cleanup
   -------------------------------------------------------------------------- */

// TestCodexAppServerManager_EndStaleSessions_OldOnly verifies the GC logic
// that protects against orchestrator crashes leaking codex children — only
// sessions older than maxAge get ended, and a session younger than maxAge
// must survive.
func TestCodexAppServerManager_EndStaleSessions_OldOnly(t *testing.T) {
	m := NewCodexAppServerManager()

	now := time.Now()
	old := &CodexAppServerSession{
		ID:         "old",
		StartedAt:  now.Add(-2 * time.Hour),
		status:     "ended", // pre-mark as ended so End() short-circuits without touching pipes
		done:       make(chan struct{}),
		streamDone: make(chan struct{}),
	}
	close(old.done)
	close(old.streamDone)
	young := &CodexAppServerSession{
		ID:         "young",
		StartedAt:  now,
		status:     "ended",
		done:       make(chan struct{}),
		streamDone: make(chan struct{}),
	}
	close(young.done)
	close(young.streamDone)

	m.sessions["old"] = old
	m.sessions["young"] = young

	m.endStaleSessions(30 * time.Minute)

	if _, ok := m.sessions["old"]; ok {
		t.Errorf("stale session `old` should have been removed; still present")
	}
	if _, ok := m.sessions["young"]; !ok {
		t.Errorf("fresh session `young` should still be present; was removed")
	}
}

/* --------------------------------------------------------------------------
   End-to-end lifecycle against a mock codex app-server
   -------------------------------------------------------------------------- */

// runMockCodexAppServer is dispatched from runMockCLI (session_integration_test.go)
// when TEST_MOCK_CLI_MODE=codex-appserver-echo. It mimics the codex JSON-RPC
// stdio protocol just enough to validate the manager:
//   - replies to every `initialize` request with a fake init result + an
//     `initialized` notification
//   - replies to every `thread/start` with a fake threadId
//   - emits an `item/started` notification for every `turn/start`
//   - exits cleanly when stdin closes (codex's documented exit path)
//   - emits one warning line on stderr at startup so the stderr forwarding
//     path is exercised
func runMockCodexAppServer() {
	fmt.Fprintln(os.Stderr, "[mock-codex] ready, listening on stdio")

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			// Echo a JSON-RPC parse-error response so the test can assert the
			// manager forwards it.
			fmt.Println(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`)
			continue
		}
		method, _ := msg["method"].(string)
		id, hasID := msg["id"]
		switch method {
		case "initialize":
			if hasID {
				resp := map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result":  map[string]any{"serverInfo": map[string]string{"name": "mock-codex", "version": "0.0.0-test"}},
				}
				_ = json.NewEncoder(os.Stdout).Encode(resp)
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
					"jsonrpc": "2.0",
					"method":  "initialized",
					"params":  map[string]any{},
				})
			}
		case "thread/start":
			if hasID {
				resp := map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result":  map[string]any{"thread": map[string]string{"id": "thr_mock"}},
				}
				_ = json.NewEncoder(os.Stdout).Encode(resp)
			}
		case "turn/start":
			if hasID {
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
					"jsonrpc": "2.0",
					"method":  "item/started",
					"params":  map[string]any{"item": map[string]string{"type": "agent_message"}},
				})
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result":  map[string]any{"turnId": "turn_mock"},
				})
			}
		}
	}
	// Stdin closed → exit cleanly. Codex's stdio app-server contract.
	os.Exit(0)
}

func TestCodexAppServerLifecycle_StartSendEnd(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("integration test only runs on win/linux/darwin")
	}

	// Locate the test binary and copy it into a tempdir with the name
	// `codex`/`codex.exe` so resolveExecutable("codex") finds the mock via
	// exec.LookPath.
	testExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	tmpDir := t.TempDir()
	mockName := "codex"
	if runtime.GOOS == "windows" {
		mockName += ".exe"
	}
	mockPath := filepath.Join(tmpDir, mockName)
	if err := copyTestBinary(testExe, mockPath); err != nil {
		t.Fatalf("copy mock binary: %v", err)
	}

	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(mockCLIEnvVar, "codex-appserver-echo")

	m := NewCodexAppServerManager()
	id := fmt.Sprintf("appsrv-test-%d", time.Now().UnixNano())

	var mu sync.Mutex
	var captured []resultMsg
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	if err := m.Start(id, tmpDir, nil, "ws", "uid", publishFn); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Send `initialize`, `thread/start`, `turn/start` and assert we see the
	// matching responses on the publish stream.
	initFrame := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"test"}}}`
	if err := m.Send(id, initFrame); err != nil {
		t.Fatalf("Send initialize: %v", err)
	}
	threadFrame := `{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{}}`
	if err := m.Send(id, threadFrame); err != nil {
		t.Fatalf("Send thread/start: %v", err)
	}
	turnFrame := `{"jsonrpc":"2.0","id":3,"method":"turn/start","params":{"threadId":"thr_mock","input":[{"type":"text","text":"hi"}]}}`
	if err := m.Send(id, turnFrame); err != nil {
		t.Fatalf("Send turn/start: %v", err)
	}

	// Wait until we've collected the responses for ids 1, 2, 3 plus the
	// initialized + item/started notifications, then end the session.
	deadline := time.Now().Add(15 * time.Second)
	requiredIDs := map[float64]bool{1: false, 2: false, 3: false}
	gotInitNotif := false
	gotItemStarted := false
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, msg := range captured {
			if msg.Type != "codex_appserver_message" {
				continue
			}
			var probe map[string]any
			if err := json.Unmarshal([]byte(msg.Output), &probe); err != nil {
				continue
			}
			if rawID, ok := probe["id"]; ok {
				if n, ok := rawID.(float64); ok {
					if _, want := requiredIDs[n]; want {
						requiredIDs[n] = true
					}
				}
			}
			if method, ok := probe["method"].(string); ok {
				if method == "initialized" {
					gotInitNotif = true
				}
				if method == "item/started" {
					gotItemStarted = true
				}
			}
		}
		mu.Unlock()
		allDone := gotInitNotif && gotItemStarted
		for _, v := range requiredIDs {
			if !v {
				allDone = false
				break
			}
		}
		if allDone {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	for id, got := range requiredIDs {
		if !got {
			t.Errorf("missing JSON-RPC response for id=%v", id)
		}
	}
	if !gotInitNotif {
		t.Errorf("missing `initialized` notification")
	}
	if !gotItemStarted {
		t.Errorf("missing `item/started` notification")
	}

	// Verify stderr forwarding picked up the mock's startup warning.
	mu.Lock()
	sawStderr := false
	for _, msg := range captured {
		if msg.Type == "codex_appserver_stderr" && strings.Contains(msg.Output, "mock-codex") {
			sawStderr = true
			break
		}
	}
	mu.Unlock()
	if !sawStderr {
		t.Errorf("expected `codex_appserver_stderr` message containing `mock-codex`")
	}

	// End the session. Mock exits when stdin closes, so we should see a
	// `codex_appserver_ended` message shortly afterwards. The _ended publish
	// is fire-and-forget (matches session.go's session_ended pattern), so we
	// poll instead of asserting immediately — otherwise this test races the
	// publish goroutine.
	if err := m.End(id); err != nil {
		t.Fatalf("End: %v", err)
	}

	endedDeadline := time.Now().Add(5 * time.Second)
	var last resultMsg
	for time.Now().Before(endedDeadline) {
		mu.Lock()
		if len(captured) > 0 {
			last = captured[len(captured)-1]
		}
		mu.Unlock()
		if last.Type == "codex_appserver_ended" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captured) == 0 {
		t.Fatal("no messages captured")
	}
	// Last message must be `codex_appserver_ended` (mirrors session_ended
	// invariant in session.go — orchestrator relies on this being terminal).
	last = captured[len(captured)-1]
	if last.Type != "codex_appserver_ended" {
		t.Errorf("expected final message to be codex_appserver_ended; got %q", last.Type)
	}
	if last.SessionID != id {
		t.Errorf("expected SessionID=%q on ended frame; got %q", id, last.SessionID)
	}
	// Manager should have removed the session.
	if m.ActiveCount() != 0 {
		t.Errorf("expected 0 active sessions after End; got %d", m.ActiveCount())
	}
}

// TestCodexAppServerLifecycle_ForwardsBadFrameAsError pins the documented
// `codex_appserver_error` behaviour: when codex (or a buggy proxy) emits a
// non-JSON line on stdout, the manager forwards it as a clearly-typed error
// frame so the orchestrator can fail the in-flight call instead of treating
// the protocol-violating line as a legitimate JSON-RPC message.
func TestCodexAppServerLifecycle_ForwardsBadFrameAsError(t *testing.T) {
	testExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	tmpDir := t.TempDir()
	mockName := "codex"
	if runtime.GOOS == "windows" {
		mockName += ".exe"
	}
	mockPath := filepath.Join(tmpDir, mockName)
	if err := copyTestBinary(testExe, mockPath); err != nil {
		t.Fatalf("copy mock binary: %v", err)
	}

	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(mockCLIEnvVar, "codex-appserver-bad-frame")

	m := NewCodexAppServerManager()
	id := fmt.Sprintf("badframe-test-%d", time.Now().UnixNano())

	var mu sync.Mutex
	var captured []resultMsg
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	if err := m.Start(id, tmpDir, nil, "ws", "uid", publishFn); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Mock emits one non-JSON line then exits. Wait for codex_appserver_ended.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ended := false
		for _, msg := range captured {
			if msg.Type == "codex_appserver_ended" {
				ended = true
				break
			}
		}
		mu.Unlock()
		if ended {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	sawError := false
	for _, msg := range captured {
		if msg.Type == "codex_appserver_error" && strings.Contains(msg.Output, "non-JSON frame") {
			sawError = true
			if msg.Status != "error" {
				t.Errorf("expected Status=error on bad-frame surface; got %q", msg.Status)
			}
		}
	}
	if !sawError {
		t.Errorf("expected `codex_appserver_error` surfacing the non-JSON frame; got types %v",
			extractTypes(captured))
	}
}

func extractTypes(msgs []resultMsg) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Type)
	}
	return out
}

func TestCodexAppServerLifecycle_StartFailsWhenBinaryMissing(t *testing.T) {
	// Point PATH at an empty dir so resolveExecutable("codex") returns the
	// literal "codex" which exec.Start cannot find.
	tmpDir := t.TempDir()
	t.Setenv("PATH", tmpDir)

	m := NewCodexAppServerManager()
	publishFn := func(resultMsg) {}
	err := m.Start("missing-bin", tmpDir, nil, "ws", "uid", publishFn)
	if err == nil {
		t.Fatal("expected start error when codex binary is not on PATH")
	}
	if !strings.Contains(err.Error(), "codex app-server") {
		t.Errorf("expected error to mention codex app-server; got %q", err.Error())
	}
	if m.ActiveCount() != 0 {
		t.Errorf("manager should have 0 sessions after failed start; got %d", m.ActiveCount())
	}
}
