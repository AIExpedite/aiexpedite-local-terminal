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
	"strconv"
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
				// Optional two-shape dialect with a chatty stderr: the
				// partner `thread/completed` follows the turn completion
				// after a burst of diagnostics. The pauses order the two
				// streams as the parent sees them — the turn completion is
				// consumed before the burst, the burst before the partner —
				// and leave the test a window to observe the manager's state
				// before the next turn's frames.
				if n := mockCodexStderrBurst(); n > 0 {
					time.Sleep(mockCodexStderrBurstPause)
					for i := 0; i < n; i++ {
						fmt.Fprintf(os.Stderr, "[mock-codex] diagnostic %d\n", i)
					}
					time.Sleep(mockCodexStderrBurstPause)
					_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
						"jsonrpc": "2.0",
						"method":  "thread/completed",
						"params":  map[string]any{"threadId": "thr_mock"},
					})
					time.Sleep(mockCodexStderrBurstPause)
				}
			}
		}
	}
	// Stdin closed → exit cleanly. Codex's stdio app-server contract.
	os.Exit(0)
}

// mockCodexStderrBurstEnv asks the echo mock to announce every turn's end in
// BOTH completion shapes, separated by that many stderr diagnostic lines.
const mockCodexStderrBurstEnv = "TEST_MOCK_CODEX_STDERR_BURST"

const mockCodexStderrBurstPause = 300 * time.Millisecond

func mockCodexStderrBurst() int {
	n, _ := strconv.Atoi(os.Getenv(mockCodexStderrBurstEnv))
	return n
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

// A turn's two completion shapes are paired by their position on STDOUT, not
// by the publication seq the stderr scanner also advances: a burst of
// diagnostics between `turn/completed` and its partner `thread/completed` must
// not lapse the credit, or the partner would settle the NEXT overlapping turn
// while it is still running.
func TestCodexAppServerLifecycle_StderrBurstDoesNotSplitPairedCompletions(t *testing.T) {
	rec := recordCodexRunHooks(t)
	t.Setenv(mockCodexStderrBurstEnv, strconv.Itoa(codexAppServerCompletionPairFrameSpan*4))
	m, id, ended := startCodexAppServerEchoMock(t)
	if m.Get(id) == nil {
		t.Fatal("session not registered")
	}
	turn := func(n int) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"turn/start","params":{"threadId":"thr_mock","input":[{"type":"text","text":"hi"}]}}`, n)
	}
	// Two turns open before either completes: the mock answers stdin in
	// order, so turn 2's frames follow turn 1's partner shape.
	if err := m.Send(id, turn(1)); err != nil {
		t.Fatalf("Send turn 1: %v", err)
	}
	if err := m.Send(id, turn(2)); err != nil {
		t.Fatalf("Send turn 2: %v", err)
	}
	rec.waitSettled(t, 1)
	// The partner shape lands two pauses after the first settle; the next
	// turn's completion only after a third. Sample inside that window.
	time.Sleep(2*mockCodexStderrBurstPause + mockCodexStderrBurstPause/2)
	if _, n := rec.counts(); n != 1 {
		t.Fatalf("settled %d turns after turn 1's paired completions, want 1 — the stderr burst split the pair and settled turn 2 early", n)
	}
	rec.waitSettled(t, 2)
	if err := m.End(id); err != nil {
		t.Fatalf("End: %v", err)
	}
	waitCodexAppServerEnded(t, ended)
	time.Sleep(200 * time.Millisecond)
	if _, n := rec.counts(); n != 2 {
		t.Fatalf("settled %d times, want exactly one settle per turn", n)
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

// Send serialises concurrent callers rather than rejecting an overlapping
// turn, so two turns (on different threads) can be open at once. Each keeps
// its own floor: the first completion settles the first turn only — it must
// not retire the second turn, whose own completion would then be a no-op and
// whose telemetry could be pre-empted by the first turn's tail — and the
// second completion settles the second. Exercised on the session directly:
// the echo mock completes every turn immediately, so it cannot overlap them.
func TestCodexAppServerSession_OverlappingTurnsSettleIndependently(t *testing.T) {
	frames := &frameCursor{}
	rec := recordCodexRunHooks(t)
	session := &CodexAppServerSession{}
	first := time.UnixMilli(1_700_000_000_000)
	second := first.Add(250 * time.Millisecond)
	session.armUsageRun(first)
	session.armUsageRun(second)
	if started, settled := rec.counts(); started != 2 || settled != 0 {
		t.Fatalf("started=%d settled=%d after two turn requests, want 2/0", started, settled)
	}

	session.settleUsageRun("turn.completed", frames.next())
	if _, settled := rec.counts(); settled != 1 {
		t.Fatalf("settled %d after the first completion, want 1", settled)
	}
	if got := rec.settled[0]; !got.Equal(first) {
		t.Fatalf("first completion settled floor %s, want the first turn's %s", got, first)
	}
	// The second turn is still open: a between-turn progress frame must not
	// re-anchor it, and process exit would still settle it.
	session.openUsageRun(second.Add(time.Second))
	if started, _ := rec.counts(); started != 2 {
		t.Fatalf("started=%d after a progress frame with a turn open, want no new run", started)
	}

	session.settleUsageRun("turn.completed", frames.next())
	settled := rec.settled
	if len(settled) != 2 || !settled[1].Equal(second) {
		t.Fatalf("settled=%v after the second completion, want [%s %s]", settled, first, second)
	}
	// Nothing open: a stray completion settles nothing.
	session.settleUsageRun("turn.completed", frames.next())
	session.settleOpenUsageRuns()
	if _, n := rec.counts(); n != 2 {
		t.Fatalf("settled %d after stray completions with nothing open, want 2", n)
	}
}

// A finished turn may announce itself in BOTH completion shapes back to back
// (`turn.completed`, then `thread.completed`). With two turns open, the second
// announcement is the SAME turn finishing: it must pair with the first instead
// of retiring the other turn, which is still running — otherwise that turn's
// debt is created early, its in-flight telemetry pays it, and its real
// completion is a no-op that leaves the final reading stale.
func TestCodexAppServerSession_PairedCompletionShapesSettleOneTurn(t *testing.T) {
	frames := &frameCursor{}
	rec := recordCodexRunHooks(t)
	session := &CodexAppServerSession{}
	first := time.UnixMilli(1_700_000_000_000)
	second := first.Add(250 * time.Millisecond)
	session.armUsageRun(first)
	session.armUsageRun(second)

	session.settleUsageRun("turn.completed", frames.next())
	session.settleUsageRun("thread.completed", frames.next())
	if _, settled := rec.counts(); settled != 1 {
		t.Fatalf("settled %d after one turn's paired completions, want 1 (the second turn is still running)", settled)
	}
	if got := rec.settled[0]; !got.Equal(first) {
		t.Fatalf("paired completions settled floor %s, want the first turn's %s", got, first)
	}

	session.settleUsageRun("turn.completed", frames.apart())
	session.settleUsageRun("thread.completed", frames.next())
	settled := rec.settled
	if len(settled) != 2 || !settled[1].Equal(second) {
		t.Fatalf("settled=%v after the second turn's paired completions, want [%s %s]", settled, first, second)
	}
	// Both pairs consumed: a third turn settles on its first shape again.
	third := second.Add(time.Second)
	session.armUsageRun(third)
	session.settleUsageRun("thread.completed", frames.apart())
	if got := rec.settled; len(got) != 3 || !got[2].Equal(third) {
		t.Fatalf("settled=%v after the third turn's completion, want it settled at %s", got, third)
	}
	session.settleUsageRun("turn.completed", frames.next())
	if _, n := rec.counts(); n != 3 {
		t.Fatalf("settled %d after the third turn's partner shape, want 3", n)
	}
}

// Pairing must not swallow completions from a dialect that speaks ONE shape:
// consecutive same-shape completions each settle a turn, and the credits they
// leave stay bounded.
func TestCodexAppServerSession_SingleShapeCompletionsSettleEveryTurn(t *testing.T) {
	frames := &frameCursor{}
	rec := recordCodexRunHooks(t)
	session := &CodexAppServerSession{}
	base := time.UnixMilli(1_700_000_000_000)
	const turns = codexAppServerMaxOpenUsageTurns + 8
	for i := 0; i < turns; i++ {
		session.armUsageRun(base.Add(time.Duration(i) * time.Second))
		session.settleUsageRun("turn.completed", frames.next())
	}
	if _, settled := rec.counts(); settled != turns {
		t.Fatalf("settled %d of %d single-shape turns", settled, turns)
	}
	session.usageMu.Lock()
	shape, floors, overflow := session.usageCreditShape, len(session.usageTurnFloors), session.usageOverflowTurns
	session.usageMu.Unlock()
	if shape != "turn.completed" || floors != 0 || overflow != 0 {
		t.Fatalf("credit=%q floors=%d overflow=%d after %d settled turns, want a single standing credit and nothing open",
			shape, floors, overflow, turns)
	}
}

// A turn that was ALREADY open when a credit was created announces itself much
// later, so its lone completion must settle its own floor rather than be
// mistaken for the credit-holder's partner shape. Without the credit window,
// that completion is swallowed and the turn's floor stays open until the
// process exits — no post-run reconcile, card pinned to a pre-run reading for
// the life of the app-server.
func TestCodexAppServerSession_LateCompletionDoesNotSpendStaleCredit(t *testing.T) {
	frames := &frameCursor{}
	rec := recordCodexRunHooks(t)
	session := &CodexAppServerSession{}
	first := time.UnixMilli(1_700_000_000_000)
	second := first.Add(250 * time.Millisecond)
	session.armUsageRun(first)
	session.armUsageRun(second)

	// Turn A finishes in ONE shape while turn B is still running.
	session.settleUsageRun("turn.completed", frames.next())
	if _, settled := rec.counts(); settled != 1 {
		t.Fatalf("settled %d after the first turn's only completion, want 1", settled)
	}

	// Turn B finishes in the OTHER shape, well downstream of that credit.
	session.settleUsageRun("thread.completed", frames.apart())
	settled := rec.settled
	if len(settled) != 2 || !settled[1].Equal(second) {
		t.Fatalf("settled=%v after the second turn's completion, want it settled at %s", settled, second)
	}

	// Back-to-back shapes still pair: the partner is the very next frame.
	third := second.Add(time.Minute)
	session.armUsageRun(third)
	session.settleUsageRun("turn.completed", frames.apart())
	session.settleUsageRun("thread.completed", frames.next())
	if got := rec.settled; len(got) != 3 || !got[2].Equal(third) {
		t.Fatalf("settled=%v after the third turn's paired completions, want exactly one more settle at %s", got, third)
	}
}

// Past the open-turn cap the oldest floor is collapsed into an overflow
// counter rather than forgotten: completions are matched to turns by position,
// so dropping the entry outright would settle turn N+1 on completion N and
// leave the LAST turn's completion with no floor at all — its debt created
// early (and payable by mid-run telemetry) and its real completion a no-op.
func TestCodexAppServerSession_OverflowKeepsCompletionAlignment(t *testing.T) {
	frames := &frameCursor{}
	rec := recordCodexRunHooks(t)
	session := &CodexAppServerSession{}
	base := time.UnixMilli(1_700_000_000_000)
	const turns = codexAppServerMaxOpenUsageTurns + 2
	for i := 0; i < turns; i++ {
		session.armUsageRun(base.Add(time.Duration(i) * time.Second))
	}
	session.usageMu.Lock()
	overflow, listed := session.usageOverflowTurns, len(session.usageTurnFloors)
	session.usageMu.Unlock()
	if overflow != turns-codexAppServerMaxOpenUsageTurns || listed != codexAppServerMaxOpenUsageTurns {
		t.Fatalf("overflow=%d listed=%d after %d overlapping turns, want %d/%d",
			overflow, listed, turns, turns-codexAppServerMaxOpenUsageTurns, codexAppServerMaxOpenUsageTurns)
	}

	// Every turn completes, one shape each — pairing never applies.
	for i := 0; i < turns; i++ {
		session.settleUsageRun("turn.completed", frames.next())
	}
	if _, settled := rec.counts(); settled != turns {
		t.Fatalf("settled %d of %d turns, want every completion to settle a floor", settled, turns)
	}
	// The newest turn is settled by the LAST completion, not by an earlier one
	// while it was still running.
	newest := base.Add(time.Duration(turns-1) * time.Second)
	if got := rec.settled[turns-1]; !got.Equal(newest) {
		t.Fatalf("final completion settled floor %s, want the newest turn's %s", got, newest)
	}
	session.usageMu.Lock()
	overflow, listed = session.usageOverflowTurns, len(session.usageTurnFloors)
	session.usageMu.Unlock()
	if overflow != 0 || listed != 0 {
		t.Fatalf("overflow=%d listed=%d once every turn completed, want nothing open", overflow, listed)
	}
}

// A failed turn write disarms the PERSISTED floor too, not only the queue
// entry: left on disk, the arm would be read as an interrupted run at the next
// process start and converted into a debt no telemetry can pay. The rollback
// names the newest turn still open so a concurrent run's floor is kept.
func TestCodexAppServerSession_DisarmRollsBackPersistedFloor(t *testing.T) {
	frames := &frameCursor{}
	rec := recordCodexRunHooks(t)
	session := &CodexAppServerSession{}
	first := time.UnixMilli(1_700_000_000_000)
	second := first.Add(250 * time.Millisecond)
	session.armUsageRun(first)
	failed := session.armUsageRun(second)
	session.disarmUsageRun(failed, nil)

	rec.mu.Lock()
	disarmed := append([]codexRunDisarm(nil), rec.disarmed...)
	rec.mu.Unlock()
	if len(disarmed) != 1 {
		t.Fatalf("disarmed %d floors, want 1", len(disarmed))
	}
	if !disarmed[0].floor.Equal(second) || !disarmed[0].fallback.Equal(first) {
		t.Fatalf("disarmed floor=%s fallback=%s, want floor=%s fallback=%s (the turn still open)",
			disarmed[0].floor, disarmed[0].fallback, second, first)
	}
	// Alone, the rollback names no fallback.
	session.settleUsageRun("turn.completed", frames.next())
	alone := session.armUsageRun(second.Add(time.Second))
	session.disarmUsageRun(alone, nil)
	rec.mu.Lock()
	last := rec.disarmed[len(rec.disarmed)-1]
	rec.mu.Unlock()
	if !last.floor.Equal(time.UnixMilli(alone)) || !last.fallback.IsZero() {
		t.Fatalf("disarmed floor=%s fallback=%s with nothing else open, want fallback zero", last.floor, last.fallback)
	}
}

// Turns still open at process exit settle as ONE run at the newest floor —
// the freshness layer coalesces concurrent runs onto the newest floor, so one
// settle records the same debt several would.
func TestCodexAppServerSession_ExitSettlesOpenTurnsAtNewestFloor(t *testing.T) {
	rec := recordCodexRunHooks(t)
	session := &CodexAppServerSession{}
	first := time.UnixMilli(1_700_000_000_000)
	second := first.Add(250 * time.Millisecond)
	session.armUsageRun(first)
	session.armUsageRun(second)
	session.settleOpenUsageRuns()
	if _, n := rec.counts(); n != 1 {
		t.Fatalf("settled %d at exit with two turns open, want one coalesced settle", n)
	}
	if got := rec.settled[0]; !got.Equal(second) {
		t.Fatalf("exit settled floor %s, want the newest turn's %s", got, second)
	}
}

// A turn request whose stdin write fails never reached the child: the floor
// armed before the write is disarmed, so process exit does not settle a run
// that never happened into a debt no telemetry can pay.
func TestCodexAppServerLifecycle_FailedTurnWriteDisarmsUsageRun(t *testing.T) {
	rec := recordCodexRunHooks(t)
	m, id, ended := startCodexAppServerEchoMock(t)
	session := m.Get(id)
	if session == nil {
		t.Fatal("session not registered")
	}
	// Swap the child's stdin for an already-closed pipe so the write fails
	// immediately while the child itself stays alive (it still holds the real
	// pipe, so Status stays "running" and Send reaches the write).
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	_ = w.Close()
	realStdin := session.Stdin
	session.stdinMu.Lock()
	session.Stdin = w
	session.stdinMu.Unlock()

	if err := m.Send(id, `{"jsonrpc":"2.0","id":1,"method":"turn/start","params":{"threadId":"thr_mock","input":[{"type":"text","text":"hi"}]}}`); err == nil {
		t.Fatal("Send on a closed stdin succeeded, want a write error")
	}
	if started, settled := rec.counts(); started != 1 || settled != 0 {
		t.Fatalf("started=%d settled=%d after the failed write, want the arm (1) and no settle", started, settled)
	}

	session.stdinMu.Lock()
	session.Stdin = realStdin
	session.stdinMu.Unlock()
	if err := m.End(id); err != nil {
		t.Fatalf("End: %v", err)
	}
	waitCodexAppServerEnded(t, ended)
	time.Sleep(200 * time.Millisecond)
	if _, settled := rec.counts(); settled != 0 {
		t.Fatalf("settled %d at exit after a turn request that never reached the child, want 0", settled)
	}
}

// A turn request whose stdin write STALLS never delivered a complete JSONL
// line either, so the child never read a turn. The timeout path tears the
// session down, and without the same rollback the failed-write path performs,
// waitForExit would fold that still-open floor into a refresh debt no
// telemetry can pay — surfacing later as a false "reading predates the last
// run" warning.
func TestCodexAppServerLifecycle_TimedOutTurnWriteDisarmsUsageRun(t *testing.T) {
	rec := recordCodexRunHooks(t)
	restore := codexAppServerStdinWriteBudget
	codexAppServerStdinWriteBudget = 100 * time.Millisecond
	t.Cleanup(func() { codexAppServerStdinWriteBudget = restore })

	m, id, ended := startCodexAppServerEchoMock(t)
	session := m.Get(id)
	if session == nil {
		t.Fatal("session not registered")
	}
	// Swap the child's stdin for a pipe nobody drains: the write blocks once
	// the pipe buffer fills, which is exactly the stall the timeout guards.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	session.stdinMu.Lock()
	session.Stdin = w
	session.stdinMu.Unlock()

	stall := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"turn/start","params":{"threadId":"thr_mock","input":[{"type":"text","text":%q}]}}`,
		strings.Repeat("x", 4*1024*1024))
	if err := m.Send(id, stall); err == nil {
		t.Fatal("Send on a stalled stdin succeeded, want a timeout error")
	}

	if started, settled := rec.counts(); started != 1 || settled != 0 {
		t.Fatalf("started=%d settled=%d after the stalled write, want the arm (1) and no settle", started, settled)
	}
	rec.mu.Lock()
	disarmed := len(rec.disarmed)
	rec.mu.Unlock()
	if disarmed != 1 {
		t.Fatalf("disarmed %d floors after the stalled write, want the arm rolled back", disarmed)
	}

	waitCodexAppServerEnded(t, ended)
	time.Sleep(200 * time.Millisecond)
	if _, settled := rec.counts(); settled != 0 {
		t.Fatalf("settled %d at exit after a turn request that never reached the child, want 0", settled)
	}
}

// The persisted floor is account-wide while one manager runs many concurrent
// sessions, so rolling back a failed turn write must preserve the newest turn
// still open in ANY of them. Resetting the shared floor to this session's own
// remainder would erase a sibling session's crash-recovery marker, and a crash
// before that sibling settles would lose the refresh it was promised.
func TestCodexAppServerSession_DisarmPreservesSiblingSessionFloor(t *testing.T) {
	frames := &frameCursor{}
	rec := recordCodexRunHooks(t)
	mgr := NewCodexAppServerManager(nil)
	sibling := &CodexAppServerSession{}
	session := &CodexAppServerSession{}
	mgr.sessions["sibling"] = sibling
	mgr.sessions["self"] = session

	siblingTurn := time.UnixMilli(1_700_000_000_000)
	failed := siblingTurn.Add(250 * time.Millisecond)
	sibling.armUsageRun(siblingTurn)
	armed := session.armUsageRun(failed)
	session.disarmUsageRun(armed, func() int64 { return mgr.newestOpenUsageFloor(session) })

	rec.mu.Lock()
	disarmed := append([]codexRunDisarm(nil), rec.disarmed...)
	rec.mu.Unlock()
	if len(disarmed) != 1 {
		t.Fatalf("disarmed %d floors, want 1", len(disarmed))
	}
	if !disarmed[0].floor.Equal(failed) || !disarmed[0].fallback.Equal(siblingTurn) {
		t.Fatalf("disarmed floor=%s fallback=%s, want floor=%s fallback=%s (the sibling session's open turn)",
			disarmed[0].floor, disarmed[0].fallback, failed, siblingTurn)
	}

	// With every sibling turn settled there is nothing left to preserve.
	sibling.settleUsageRun("turn.completed", frames.next())
	alone := session.armUsageRun(failed.Add(time.Second))
	session.disarmUsageRun(alone, func() int64 { return mgr.newestOpenUsageFloor(session) })
	rec.mu.Lock()
	last := rec.disarmed[len(rec.disarmed)-1]
	rec.mu.Unlock()
	if !last.fallback.IsZero() {
		t.Fatalf("disarmed fallback=%s with no turn open anywhere, want zero", last.fallback)
	}
}

// The same account-wide floor is armed by ordinary terminal `codex` sessions
// (SessionManager), not only by this manager's turns. Rolling back a failed
// app-server turn write while a terminal run is the only thing still open must
// fall back to THAT run's floor: a zero fallback would erase its
// crash-recovery marker along with the withdrawn arm.
func TestCodexAppServerSession_DisarmPreservesTerminalSessionFloor(t *testing.T) {
	rec := recordCodexRunHooks(t)
	prevTerminal := globalSessionManager
	globalSessionManager = NewSessionManager(nil)
	t.Cleanup(func() { globalSessionManager = prevTerminal })
	terminal := &CLISession{ID: "terminal", Command: "codex"}
	globalSessionManager.sessions[terminal.ID] = terminal
	mgr := NewCodexAppServerManager(nil)
	session := &CodexAppServerSession{}
	mgr.sessions["self"] = session

	terminalRun := time.UnixMilli(1_700_000_000_000)
	failed := terminalRun.Add(250 * time.Millisecond)
	terminal.armCodexUsageRun(terminalRun)
	armed := session.armUsageRun(failed)
	session.disarmUsageRun(armed, func() int64 { return mgr.newestOpenUsageFloor(session) })

	rec.mu.Lock()
	disarmed := append([]codexRunDisarm(nil), rec.disarmed...)
	rec.mu.Unlock()
	if len(disarmed) != 1 {
		t.Fatalf("disarmed %d floors, want 1", len(disarmed))
	}
	if !disarmed[0].floor.Equal(failed) || !disarmed[0].fallback.Equal(terminalRun) {
		t.Fatalf("disarmed floor=%s fallback=%s, want floor=%s fallback=%s (the terminal session's open run)",
			disarmed[0].floor, disarmed[0].fallback, failed, terminalRun)
	}

	// A terminal run that already settled is not open: nothing left to preserve.
	terminal.settleCodexUsageRun()
	alone := session.armUsageRun(failed.Add(time.Second))
	session.disarmUsageRun(alone, func() int64 { return mgr.newestOpenUsageFloor(session) })
	rec.mu.Lock()
	last := rec.disarmed[len(rec.disarmed)-1]
	rec.mu.Unlock()
	if !last.fallback.IsZero() {
		t.Fatalf("disarmed fallback=%s with no run open in any manager, want zero", last.fallback)
	}
}

// A completion credit stands for the turn that left it and nothing else. A
// server that announced one turn in a single shape must not bank a credit that
// a LATER turn's partner-shape completion spends: that turn's floor would stay
// open until the process exited, so its post-run reconcile never runs.
func TestCodexAppServerSession_CreditExpiresWhenNextTurnOpens(t *testing.T) {
	frames := &frameCursor{}
	rec := recordCodexRunHooks(t)
	session := &CodexAppServerSession{}
	first := time.UnixMilli(1_700_000_000_000)
	second := first.Add(time.Second)
	session.armUsageRun(first)
	session.settleUsageRun("turn.completed", frames.next())
	if _, settled := rec.counts(); settled != 1 {
		t.Fatalf("settled %d after the first turn's only completion, want 1", settled)
	}

	// A different turn, announced in the shape the first turn never used.
	session.armUsageRun(second)
	session.settleUsageRun("thread.completed", frames.next())
	settled := rec.settled
	if len(settled) != 2 || !settled[1].Equal(second) {
		t.Fatalf("settled=%v after the second turn's completion, want it settled at %s", settled, second)
	}

	// A progress frame that opens a run clears a stale credit too.
	third := second.Add(time.Second)
	session.settleUsageRun("turn.completed", frames.next()) // leaves a credit, nothing open
	session.openUsageRun(third)
	session.settleUsageRun("thread.completed", frames.next())
	if got := rec.settled; len(got) != 3 || !got[2].Equal(third) {
		t.Fatalf("settled=%v after the opened run's completion, want it settled at %s", got, third)
	}
}

// frameCursor hands out stream positions for settleUsageRun. next() is the
// frame immediately after the last one — what a turn's partner announcement
// looks like on the wire — while apart() leaves a full pairing span in
// between, standing in for the output another turn emitted before its own
// completion.
type frameCursor struct{ pos int64 }

func (c *frameCursor) next() int64 {
	c.pos++
	return c.pos
}

func (c *frameCursor) apart() int64 {
	c.pos += codexAppServerCompletionPairFrameSpan + 1
	return c.pos
}
