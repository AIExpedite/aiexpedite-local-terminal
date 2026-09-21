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

func TestBuildCodexAppServerArgs_PreservesListenAsValuedFlagValue(t *testing.T) {
	// Regression: a `--listen` token in VALUE position (the value of a valued
	// flag like -c / --config / --enable) is user data, NOT a transport
	// override — it must survive sanitization, and the token after it must not
	// be eaten. The old sanitizer treated the `--listen` value of `-c` as the
	// transport flag, stripping it AND skipping the following token, corrupting
	// the override.
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{
			"config_value_is_listen",
			[]string{"-c", "--listen", "-c", `model="x"`},
			[]string{"app-server", "--listen", "stdio://", "-c", "--listen", "-c", `model="x"`},
		},
		{
			"enable_value_is_listen",
			[]string{"--enable", "--listen", "-c", `model="x"`},
			[]string{"app-server", "--listen", "stdio://", "--enable", "--listen", "-c", `model="x"`},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildCodexAppServerArgs(c.args)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("buildCodexAppServerArgs mangled a valued-flag `--listen` value: got %#v, want %#v", got, c.want)
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
	m := NewCodexAppServerManager(nil)
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
	m := NewCodexAppServerManager(nil)
	err := m.Send("missing", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected `not found` error; got %v", err)
	}
}

func TestCodexAppServerManager_Send_EndedSession(t *testing.T) {
	m := NewCodexAppServerManager(nil)
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
	m := NewCodexAppServerManager(nil)
	id := "dupe-fixture"
	m.sessions[id] = &CodexAppServerSession{ID: id, status: "running", done: make(chan struct{}), streamDone: make(chan struct{})}

	publishFn := func(resultMsg) {}
	err := m.Start(id, t.TempDir(), nil, "ws", "uid", publishFn)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected `already exists` error; got %v", err)
	}
}

func TestCodexAppServerManager_StartRequiresIDAndPublish(t *testing.T) {
	m := NewCodexAppServerManager(nil)
	cwd := t.TempDir()
	if err := m.Start("", cwd, nil, "ws", "uid", func(resultMsg) {}); err == nil {
		t.Fatalf("expected error for empty sessionID")
	}
	if err := m.Start("x", cwd, nil, "ws", "uid", nil); err == nil {
		t.Fatalf("expected error for nil publishFn")
	}
}

// TestCodexAppServerManager_StartRequiresValidCwd pins the cwd validation —
// missing/relative/non-existent cwd must all reject before any process is
// spawned, so a malformed orchestrator command can't accidentally launch
// codex against the agent's process working directory and edit unintended
// files (e.g. C:\Program Files\AI Expedite on Windows).
func TestCodexAppServerManager_StartRequiresValidCwd(t *testing.T) {
	m := NewCodexAppServerManager(nil)
	publishFn := func(resultMsg) {}

	t.Run("empty_cwd_rejected", func(t *testing.T) {
		err := m.Start("a", "", nil, "ws", "uid", publishFn)
		if err == nil || !strings.Contains(err.Error(), "cwd is required") {
			t.Fatalf("expected `cwd is required` error; got %v", err)
		}
	})

	t.Run("relative_cwd_rejected", func(t *testing.T) {
		err := m.Start("b", "./relative/path", nil, "ws", "uid", publishFn)
		if err == nil || !strings.Contains(err.Error(), "absolute path") {
			t.Fatalf("expected `absolute path` error; got %v", err)
		}
	})

	t.Run("missing_dir_rejected", func(t *testing.T) {
		// Use an absolute path that almost certainly does not exist.
		missing := filepath.Join(t.TempDir(), "definitely-missing-dir-xyz123")
		err := m.Start("c", missing, nil, "ws", "uid", publishFn)
		if err == nil || !strings.Contains(err.Error(), "not accessible") {
			t.Fatalf("expected `not accessible` error; got %v", err)
		}
	})

	t.Run("file_instead_of_dir_rejected", func(t *testing.T) {
		// Create a regular file and try to use its path as cwd.
		dir := t.TempDir()
		filePath := filepath.Join(dir, "afile.txt")
		if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		err := m.Start("d", filePath, nil, "ws", "uid", publishFn)
		if err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("expected `not a directory` error; got %v", err)
		}
	})

	if m.ActiveCount() != 0 {
		t.Errorf("no session should have been registered after rejected Start calls; got %d", m.ActiveCount())
	}
}

/* --------------------------------------------------------------------------
   Stale-session cleanup
   -------------------------------------------------------------------------- */

// TestCodexAppServerLifecycle_StallingPublisherTerminatesSession exercises
// the never-drop policy: a Pub/Sub publisher that blocks indefinitely must
// not cause stdout frames to be dropped silently — the manager has to
// publish a codex_appserver_error surface and force-kill the child so the
// orchestrator sees a clear failure instead of a silently-truncated stream.
//
// Pretty heavy test (drives a real mock CLI and relies on the
// codexAppServerEnqueueTimeout cap), so we shrink the cap via a build-tag-
// free swap pattern: we override the constants through the public package
// vars used in production. Because the constants ARE constants here, we
// instead point the mock at a tight "echo many frames" mode and assert the
// fatal error surface is published.
func TestCodexAppServerLifecycle_StallingPublisherTerminatesSession(t *testing.T) {
	// Build a publishFn that blocks forever after a handful of messages so
	// the queue fills, then assert we see codex_appserver_error indicating
	// the queue stalled.
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
	t.Setenv(mockCLIEnvVar, "codex-appserver-burst")

	m := NewCodexAppServerManager(nil)
	id := fmt.Sprintf("stall-test-%d", time.Now().UnixNano())

	// publishFn: accept the first few frames, then block forever. This
	// simulates a wedged Pub/Sub network and is enough to fill the bounded
	// publish queue. The fatal-publish path bypasses the wedged queue with a
	// synchronous publishFn call (so `_error` is delivered before the
	// downstream `_ended` can race ahead), so we capture those frames via a
	// separate, non-blocking sink that always accepts them.
	var captureMu sync.Mutex
	var captured []resultMsg
	const liveSlots = 2
	live := make(chan struct{}, liveSlots)
	for i := 0; i < liveSlots; i++ {
		live <- struct{}{}
	}
	stall := make(chan struct{})
	defer close(stall)

	publishFn := func(res resultMsg) {
		// Fatal errors and the terminal `_ended` frame are published via the
		// fail-fast path that bypasses the wedged queue. Accept them
		// unconditionally so the test can observe them even when normal live
		// slots are exhausted. Other frames consume a "live" slot or block on
		// stall.
		if res.Type == "codex_appserver_error" || res.Type == "codex_appserver_ended" {
			captureMu.Lock()
			captured = append(captured, res)
			captureMu.Unlock()
			return
		}
		select {
		case <-live:
			captureMu.Lock()
			captured = append(captured, res)
			captureMu.Unlock()
		case <-stall:
		}
	}

	if err := m.Start(id, tmpDir, nil, "ws", "uid", publishFn); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait up to enqueue timeout + a margin for the fatal escalation to fire.
	deadline := time.Now().Add(codexAppServerEnqueueTimeout + 15*time.Second)
	for time.Now().Before(deadline) {
		captureMu.Lock()
		sawFatal := false
		for _, msg := range captured {
			if msg.Type == "codex_appserver_error" && strings.Contains(msg.Output, "queue stalled") {
				sawFatal = true
				break
			}
		}
		captureMu.Unlock()
		if sawFatal {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	captureMu.Lock()
	defer captureMu.Unlock()
	sawFatal := false
	for _, msg := range captured {
		if msg.Type == "codex_appserver_error" && strings.Contains(msg.Output, "queue stalled") {
			sawFatal = true
		}
	}
	if !sawFatal {
		types := make([]string, 0, len(captured))
		for _, msg := range captured {
			types = append(types, msg.Type)
		}
		t.Errorf("expected fatal `codex_appserver_error` with `queue stalled`; got types=%v", types)
	}
}

// A stale session whose process exited remains watcher-owned until stream
// drain, artifact collection, and terminal publication finish. GC must not
// free that ID directly; a fresh session is untouched as usual.
func TestCodexAppServerManager_EndStaleSessions_RetainsWatcherOwnedSession(t *testing.T) {
	m := NewCodexAppServerManager(nil)

	now := time.Now()
	old := &CodexAppServerSession{
		ID:            "old",
		StartedAt:     now.Add(-2 * time.Hour),
		status:        "running",
		processExited: make(chan struct{}),
		done:          make(chan struct{}),
		streamDone:    make(chan struct{}),
	}
	close(old.processExited)
	young := &CodexAppServerSession{
		ID:            "young",
		StartedAt:     now,
		status:        "running",
		processExited: make(chan struct{}),
		done:          make(chan struct{}),
		streamDone:    make(chan struct{}),
	}
	close(young.processExited)

	m.sessions["old"] = old
	m.sessions["young"] = young

	m.endStaleSessions(30 * time.Minute)

	if got := m.sessions["old"]; got != old {
		t.Errorf("stale session `old` must remain reserved for its watcher")
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
		case "account/rateLimits/read":
			// A between-turn reading: real app-servers answer this while no turn
			// is running, and the reply carries numeric utilization.
			if hasID {
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result": map[string]any{"rateLimits": map[string]any{
						"primary": map[string]any{"used_percent": 7, "window_minutes": 300},
					}},
				})
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
				// Real app-servers announce the end of the turn and stay up for
				// the next one. Emitted last so the manager's per-turn settle
				// sees the turn's frames first. This is the shape Codex
				// 0.144's generated JSON-RPC schema defines: a slash-form
				// method with no nested event type.
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
					"jsonrpc": "2.0",
					"method":  "turn/completed",
					"params":  map[string]any{"turnId": "turn_mock"},
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

	m := NewCodexAppServerManager(nil)
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

	// Verify stderr forwarding picked up the mock's startup warning. stdout and
	// stderr are scanned by INDEPENDENT goroutines, so completing the stdout
	// wait above says nothing about whether the stderr line has been published
	// yet — asserting immediately raced that goroutine and flaked on macOS.
	// Poll instead, like the `_ended` wait below.
	stderrDeadline := time.Now().Add(5 * time.Second)
	sawStderr := false
	for !sawStderr && time.Now().Before(stderrDeadline) {
		mu.Lock()
		for _, msg := range captured {
			if msg.Type == "codex_appserver_stderr" && strings.Contains(msg.Output, "mock-codex") {
				sawStderr = true
				break
			}
		}
		mu.Unlock()
		if sawStderr {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
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

// TestCodexAppServerLifecycle_OversizeFrameTerminatesSession pins Finding #4
// from the secondary review: stdout frames larger than
// codexAppServerMaxFrameSize cannot survive a Pub/Sub publish (10 MB hard
// limit), so the manager MUST fail-fast — surface a codex_appserver_error
// with `oversize_frame` plus session_ended — rather than enqueue a frame
// the publisher can't deliver. Silent drops would deadlock the
// orchestrator's JSON-RPC state machine on the missing response.
func TestCodexAppServerLifecycle_OversizeFrameTerminatesSession(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("integration test only runs on win/linux/darwin")
	}
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
	t.Setenv(mockCLIEnvVar, "codex-appserver-oversize")

	m := NewCodexAppServerManager(nil)
	id := fmt.Sprintf("oversize-test-%d", time.Now().UnixNano())

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

	// Wait for the fail-fast codex_appserver_error AND the trailing
	// session_ended. Generous deadline because the mock emits ~9 MB before
	// the scanner sees the newline.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		sawFatal := false
		for _, msg := range captured {
			if msg.Type == "codex_appserver_error" && strings.Contains(msg.Output, "exceeding the") {
				sawFatal = true
				break
			}
		}
		mu.Unlock()
		if sawFatal {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	sawFatal := false
	for _, msg := range captured {
		if msg.Type == "codex_appserver_error" && strings.Contains(msg.Output, "exceeding the") {
			sawFatal = true
			if msg.Status != "error" {
				t.Errorf("expected Status=error on oversize-frame surface; got %q", msg.Status)
			}
		}
		// Critically: the manager must NOT have enqueued the oversize frame as
		// codex_appserver_message — Pub/Sub would reject it and the orchestrator
		// would silently lose protocol state. Verify by checking no captured
		// frame exceeds the cap (with envelope overhead margin).
		if msg.Type == "codex_appserver_message" && len(msg.Output) > codexAppServerMaxFrameSize {
			t.Errorf("oversize frame leaked through as codex_appserver_message (len=%d)", len(msg.Output))
		}
	}
	if !sawFatal {
		types := make([]string, 0, len(captured))
		for _, msg := range captured {
			types = append(types, msg.Type)
		}
		t.Errorf("expected fatal `codex_appserver_error` for oversize frame; got types=%v", types)
	}
}

// TestCodexAppServerLifecycle_EscapeAmplifiedFrameTerminatesSession pins the
// marshaled-envelope size check: a frame whose raw line is UNDER
// codexAppServerMaxFrameSize but whose Output field doubles on JSON marshal
// (heavy in '"' / '\') can still produce an envelope larger than Pub/Sub's
// 10 MB ceiling. The manager MUST fail-fast in that case too — silently
// publishing a frame Pub/Sub rejects would leave the orchestrator waiting
// for a JSON-RPC response that never arrives.
func TestCodexAppServerLifecycle_EscapeAmplifiedFrameTerminatesSession(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("integration test only runs on win/linux/darwin")
	}
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
	t.Setenv(mockCLIEnvVar, "codex-appserver-oversize-escaped")

	m := NewCodexAppServerManager(nil)
	id := fmt.Sprintf("escaped-oversize-test-%d", time.Now().UnixNano())

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

	// Generous deadline — the mock emits ~6 MB of escape sequences before
	// the newline, and the manager has to read, build, and marshal it before
	// the size check fires.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		sawFatal := false
		for _, msg := range captured {
			if msg.Type == "codex_appserver_error" && strings.Contains(msg.Output, "after JSON escaping") {
				sawFatal = true
				break
			}
		}
		mu.Unlock()
		if sawFatal {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	sawFatal := false
	for _, msg := range captured {
		if msg.Type == "codex_appserver_error" && strings.Contains(msg.Output, "after JSON escaping") {
			sawFatal = true
			if msg.Status != "error" {
				t.Errorf("expected Status=error on escape-amplified surface; got %q", msg.Status)
			}
		}
		// The oversize frame must NOT have leaked through as a normal
		// codex_appserver_message — Pub/Sub would reject the envelope and
		// the orchestrator would silently lose the response.
		if msg.Type == "codex_appserver_message" && len(msg.Output) > codexAppServerMaxFrameSize/2 {
			t.Errorf("escape-amplified frame leaked through as codex_appserver_message (len=%d)", len(msg.Output))
		}
	}
	if !sawFatal {
		types := make([]string, 0, len(captured))
		for _, msg := range captured {
			types = append(types, msg.Type)
		}
		t.Errorf("expected fatal `codex_appserver_error` for escape-amplified frame; got types=%v", types)
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

	m := NewCodexAppServerManager(nil)
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

	m := NewCodexAppServerManager(nil)
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

// startCodexAppServerEchoMock starts a session against the echo mock and
// returns the manager, the session id, and a func reporting whether the
// codex_appserver_ended frame has been published.
func startCodexAppServerEchoMock(t *testing.T) (*CodexAppServerManager, string, func() bool) {
	t.Helper()
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("integration test only runs on win/linux/darwin")
	}
	testExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	tmpDir := t.TempDir()
	mockName := "codex"
	if runtime.GOOS == "windows" {
		mockName += ".exe"
	}
	if err := copyTestBinary(testExe, filepath.Join(tmpDir, mockName)); err != nil {
		t.Fatalf("copy mock binary: %v", err)
	}
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(mockCLIEnvVar, "codex-appserver-echo")

	m := NewCodexAppServerManager(nil)
	id := fmt.Sprintf("appsrv-freshness-%d", time.Now().UnixNano())
	var mu sync.Mutex
	ended := false
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		if res.Type == "codex_appserver_ended" {
			ended = true
		}
	}
	if err := m.Start(id, tmpDir, nil, "ws", "uid", publishFn); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return m, id, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return ended
	}
}

func waitCodexAppServerEnded(t *testing.T, ended func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ended() {
		if time.Now().After(deadline) {
			t.Fatal("codex_appserver_ended was never published")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Starting an app-server is not running a turn: Start arms NO utilization run,
// and an IDE that launches one and closes it after initialization settles
// nothing. Arming at Start — as the first cut did — owed a refresh no telemetry
// could pay, which aged out into a stale-utilization warning for a run that
// never happened. A turn that IS requested arms and settles exactly once.
func TestCodexAppServerLifecycle_ArmsAndSettlesUsageFreshnessOnce(t *testing.T) {
	rec := recordCodexRunHooks(t)
	m, id, ended := startCodexAppServerEchoMock(t)
	if m.Get(id) == nil {
		t.Fatal("session not registered")
	}
	if started, settled := rec.counts(); started != 0 || settled != 0 {
		t.Fatalf("after Start: started=%d settled=%d, want no run armed (0/0)", started, settled)
	}

	requested := time.Now()
	if err := m.Send(id, `{"jsonrpc":"2.0","id":1,"method":"turn/start","params":{"threadId":"thr_mock","input":[{"type":"text","text":"hi"}]}}`); err != nil {
		t.Fatalf("Send turn: %v", err)
	}
	settled := rec.waitSettled(t, 1)
	if settled[0].UnixMilli() < requested.UnixMilli() {
		t.Fatalf("settled with floor %s, want one at/after the turn request %s", settled[0], requested)
	}

	// The turn already settled, so process exit must not settle again.
	if err := m.End(id); err != nil {
		t.Fatalf("End: %v", err)
	}
	waitCodexAppServerEnded(t, ended)
	time.Sleep(50 * time.Millisecond)
	if started, n := rec.counts(); started != 1 || n != 1 {
		t.Fatalf("started=%d settled=%d, want exactly one run for the one turn", started, n)
	}
}

// An app-server launched and closed without ever running a turn owes nothing:
// no floor is armed, so waitForExit has no phantom run to settle.
func TestCodexAppServerLifecycle_NoTurnNoUsageRun(t *testing.T) {
	rec := recordCodexRunHooks(t)
	m, id, ended := startCodexAppServerEchoMock(t)
	if err := m.Send(id, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`); err != nil {
		t.Fatalf("Send initialize: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if err := m.End(id); err != nil {
		t.Fatalf("End: %v", err)
	}
	waitCodexAppServerEnded(t, ended)
	time.Sleep(200 * time.Millisecond)
	if started, settled := rec.counts(); started != 0 || settled != 0 {
		t.Fatalf("started=%d settled=%d after initialize-only session, want none", started, settled)
	}
}

// The app-server is long-lived and serves MANY turns per process, so every
// completed turn settles its own utilization run. Waiting for process exit —
// as the first cut did — leaves the card pinned to a pre-turn reading for as
// long as an IDE session stays open.
func TestCodexAppServerLifecycle_SettlesUsageFreshnessPerTurn(t *testing.T) {
	rec := recordCodexRunHooks(t)
	m, id, ended := startCodexAppServerEchoMock(t)
	session := m.Get(id)
	if session == nil {
		t.Fatal("session not registered")
	}
	turn := func(n int) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"turn/start","params":{"threadId":"thr_mock","input":[{"type":"text","text":"hi"}]}}`, n)
	}
	requested := time.Now()
	if err := m.Send(id, turn(1)); err != nil {
		t.Fatalf("Send turn 1: %v", err)
	}
	first := rec.waitSettled(t, 1)
	// The floor is the moment the TURN was requested, not the session start: a
	// rate-limit frame delivered during initialization must not pass as this
	// turn's reading. Floors are persisted in milliseconds, so compare there.
	if first[0].UnixMilli() < requested.UnixMilli() {
		t.Fatalf("first turn settled with floor %s, want one at/after the turn request %s", first[0], requested)
	}

	if err := m.Send(id, turn(2)); err != nil {
		t.Fatalf("Send turn 2: %v", err)
	}
	second := rec.waitSettled(t, 2)
	if second[1].UnixMilli() <= first[0].UnixMilli() {
		t.Fatalf("second turn settled with floor %s, want one after the first turn's settle", second[1])
	}

	// The last turn already settled, so process exit must not settle again.
	if err := m.End(id); err != nil {
		t.Fatalf("End: %v", err)
	}
	waitCodexAppServerEnded(t, ended)
	time.Sleep(200 * time.Millisecond)
	if _, n := rec.counts(); n != 2 {
		t.Fatalf("settled %d times, want one settle per completed turn", n)
	}
}

// A failed Start never arms a floor for a run that did not happen.
func TestCodexAppServerLifecycle_FailedStartDoesNotArmUsageFreshness(t *testing.T) {
	rec := recordCodexRunHooks(t)
	tmpDir := t.TempDir()
	t.Setenv("PATH", tmpDir)
	if err := NewCodexAppServerManager(nil).Start("missing-bin", tmpDir, nil, "ws", "uid", func(resultMsg) {}); err == nil {
		t.Fatal("expected start error")
	}
	if started, settled := rec.counts(); started != 0 || settled != 0 {
		t.Fatalf("started=%d settled=%d, want none", started, settled)
	}
}

// The real freshness path failing — here the cache cannot even be created —
// must neither panic nor delay the ended publication.
func TestCodexAppServerLifecycle_UsageFreshnessFailureNeverBlocksEnded(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(blocker, "codex_rate_limits.json"))
	resetCodexUsageRefreshGate()
	SetCodexUsageRefreshEnabled(true)
	t.Cleanup(resetCodexUsageRefreshGate)

	m, id, ended := startCodexAppServerEchoMock(t)
	if err := m.End(id); err != nil {
		t.Fatalf("End: %v", err)
	}
	waitCodexAppServerEnded(t, ended)
	waitCodexUsageRefreshIdle(t)
	if _, err := os.Stat(filepath.Join(blocker, "codex_rate_limits.json")); err == nil {
		t.Fatal("cache unexpectedly written under a file")
	}
}

// The floor a turn is measured against must be anchored when the turn is
// REQUESTED. Deriving it from the previous turn's completion — as the first
// per-turn cut did — lets a rate-limit frame delivered between turns (an
// `account/rateLimits/read` reply, or a notification during initialization)
// count as the next turn's reading, so that turn skips its post-run refresh.
func TestCodexAppServerLifecycle_AnchorsUsageFloorAtEachTurnRequest(t *testing.T) {
	rec := recordCodexRunHooks(t)
	m, id, ended := startCodexAppServerEchoMock(t)
	t.Cleanup(func() {
		_ = m.End(id)
		waitCodexAppServerEnded(t, ended)
	})
	session := m.Get(id)
	if session == nil {
		t.Fatal("session not registered")
	}
	turn := func(n int) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"turn/start","params":{"threadId":"thr_mock","input":[{"type":"text","text":"hi"}]}}`, n)
	}

	if err := m.Send(id, turn(1)); err != nil {
		t.Fatalf("Send turn 1: %v", err)
	}
	rec.waitSettled(t, 1)
	// Between the turns: a numeric reading arrives while no turn is running. It
	// must not become the floor the NEXT turn is measured against, or that turn
	// would read as already paid and skip its post-run refresh.
	if err := m.Send(id, `{"jsonrpc":"2.0","id":99,"method":"account/rateLimits/read","params":{}}`); err != nil {
		t.Fatalf("Send between-turn request: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	between := time.Now()

	if err := m.Send(id, turn(2)); err != nil {
		t.Fatalf("Send turn 2: %v", err)
	}
	settled := rec.waitSettled(t, 2)
	if settled[1].UnixMilli() < between.UnixMilli() {
		t.Fatalf("second turn settled with floor %s, want one at/after the between-turn reading (before %s)", settled[1], between)
	}
}

// Between-turn traffic — the supported `account/rateLimits/read` reply above
// all — must not open a utilization run. Its reading is captured a moment
// before the run check, so a run opened on it would anchor a floor LATER than
// that reading; a client that then closes the app-server without another turn
// would settle that phantom run into a debt no rollout can pay, and the card
// would warn that utilization is stale after a run that never happened.
func TestCodexAppServerLifecycle_BetweenTurnReadingDoesNotOpenARun(t *testing.T) {
	rec := recordCodexRunHooks(t)
	m, id, ended := startCodexAppServerEchoMock(t)
	if err := m.Send(id, `{"jsonrpc":"2.0","id":1,"method":"turn/start","params":{"threadId":"thr_mock","input":[{"type":"text","text":"hi"}]}}`); err != nil {
		t.Fatalf("Send turn: %v", err)
	}
	rec.waitSettled(t, 1)

	// No turn is running: the reply must leave the session with nothing open.
	if err := m.Send(id, `{"jsonrpc":"2.0","id":99,"method":"account/rateLimits/read","params":{}}`); err != nil {
		t.Fatalf("Send between-turn request: %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	if err := m.End(id); err != nil {
		t.Fatalf("End: %v", err)
	}
	waitCodexAppServerEnded(t, ended)
	time.Sleep(200 * time.Millisecond)
	if started, settled := rec.counts(); settled != 1 || started != 1 {
		t.Fatalf("started=%d settled=%d after a between-turn reading and exit, want exactly the one turn (1/1)", started, settled)
	}
}
