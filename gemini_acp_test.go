// gemini_acp_test.go
// -----------------------------------------------------------------------------
// Unit + lifecycle tests for GeminiACPManager. Unit tests pin the argv builder
// / env sanitizer / command classifier. The lifecycle tests drive a real
// GeminiACPManager against the test binary in TEST_MOCK_CLI_MODE=gemini-acp-*
// modes so we don't need a real `gemini` install on the test host.
//
// Shape mirrors grok_acp_test.go — both managers ride the shared ACP core, so
// the same battery of invariants must hold for each (no dropped frames,
// fail-fast on malformed input, terminal `_ended` frame, stdin-close graceful
// exit, typed startup errors, …). The core-level invariants (Seq ordering,
// oversize frames, publish-queue stalls, waitForExit races) are pinned once in
// grok_acp_test.go against the same code; this file focuses on the
// Gemini-specific seams and the gemini_acp_* result-type mapping.
// -----------------------------------------------------------------------------

package main

import (
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

// runMockGeminiACPServer is the gemini-branded twin of runMockGrokACPServer:
// same JSON-RPC echo loop (the ACP wire protocol is identical), different
// stderr banner so the stderr-forwarding assertion can prove which mock ran.
func runMockGeminiACPServer() {
	fmt.Fprintln(os.Stderr, "[mock-gemini] ready, listening on stdio")
	runMockACPEchoLoop()
}

/* --------------------------------------------------------------------------
   argv builder + classifier
   -------------------------------------------------------------------------- */

func TestIsGeminiACPCommand(t *testing.T) {
	cases := map[string]bool{
		"gemini_acp_start": true,
		"gemini_acp_send":  true,
		"gemini_acp_end":   true,
		"grok_acp_start":   false,
		"session_start":    false,
		"execute":          false,
		"":                 false,
		"gemini_acp_other": false,
	}
	for in, want := range cases {
		if got := isGeminiACPCommand(in); got != want {
			t.Errorf("isGeminiACPCommand(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestBuildGeminiACPArgs pins the argv contract: `--experimental-acp` always
// leads, mode-switching prompt flags are stripped (any surviving -p/-i would
// flip gemini OUT of ACP stdio mode and break the orchestrator's JSON-RPC
// handshake), and benign flags pass through for gemini itself to validate.
func TestBuildGeminiACPArgs(t *testing.T) {
	cases := []struct {
		name  string
		extra []string
		want  []string
	}{
		{
			"no_extras",
			nil,
			[]string{"--experimental-acp"},
		},
		{
			"model_passthrough",
			[]string{"--model", "gemini-3-pro"},
			[]string{"--experimental-acp", "--model", "gemini-3-pro"},
		},
		{
			"duplicate_transport_flag_dropped",
			[]string{"--experimental-acp", "--model", "gemini-3-pro"},
			[]string{"--experimental-acp", "--model", "gemini-3-pro"},
		},
		{
			"prompt_flag_and_value_stripped",
			[]string{"-p", "hello there", "--model", "gemini-3-pro"},
			[]string{"--experimental-acp", "--model", "gemini-3-pro"},
		},
		{
			"prompt_equals_form_stripped",
			[]string{"--prompt=hello", "--model=gemini-3-pro"},
			[]string{"--experimental-acp", "--model=gemini-3-pro"},
		},
		{
			"prompt_interactive_stripped",
			[]string{"--prompt-interactive", "hi", "-i", "hi2", "-i=hi3"},
			[]string{"--experimental-acp"},
		},
		{
			// Positional tokens after `--` are a prompt, which flips gemini
			// out of ACP mode exactly like `-p` — the delimiter AND its tail
			// must both be dropped or the handshake never starts.
			"double_dash_delimiter_and_tail_dropped",
			[]string{"--", "positional prompt words"},
			[]string{"--experimental-acp"},
		},
		{
			"flags_before_double_dash_survive",
			[]string{"--model=gemini-3-pro", "--", "trailing prompt"},
			[]string{"--experimental-acp", "--model=gemini-3-pro"},
		},
		{
			// `-y`/`--yolo` and `--approval-mode` auto-approve tool calls,
			// bypassing the orchestrator-driven session/request_permission
			// flow — a signed gemini_acp_start must not be able to smuggle
			// them in through extras.
			"yolo_and_approval_mode_stripped",
			[]string{"-y", "--yolo", "--approval-mode", "yolo", "--model", "gemini-3-pro"},
			[]string{"--experimental-acp", "--model", "gemini-3-pro"},
		},
		{
			// `--include-directories` would widen the workspace beyond the
			// WorkspaceRoot containment Start enforces; the separate-token
			// value must be consumed with the flag.
			"include_directories_and_value_stripped",
			[]string{"--include-directories", "/outside", "--model=gemini-3-pro"},
			[]string{"--experimental-acp", "--model=gemini-3-pro"},
		},
		{
			// `--policy`/`--admin-policy` load extra policy-engine files
			// whose `allow` rules auto-approve tools without confirmation —
			// the same bypass as `--allowed-tools`, which gemini's own docs
			// deprecate in favor of the policy engine. Separate-token values
			// must be consumed with the flag.
			"policy_files_and_values_stripped",
			[]string{"--policy", "/tmp/allow.toml", "--admin-policy", "/tmp/admin", "--model", "gemini-3-pro"},
			[]string{"--experimental-acp", "--model", "gemini-3-pro"},
		},
		{
			// `--skip-trust` trusts the current workspace for the session
			// without prompting, re-enabling the project `.gemini/settings.json`
			// (tool auto-acceptance / project policies) this sanitizer blocks.
			// It is a boolean flag, so nothing follows it.
			"skip_trust_stripped",
			[]string{"--skip-trust", "--model", "gemini-3-pro"},
			[]string{"--experimental-acp", "--model", "gemini-3-pro"},
		},
		{
			"privileged_equals_forms_stripped",
			[]string{"--yolo=true", "--skip-trust=true", "--approval-mode=auto_edit", "--include-directories=/outside", "--allowed-tools=run_shell_command", "--policy=/tmp/allow.toml", "--admin-policy=/tmp/admin"},
			[]string{"--experimental-acp"},
		},
		{
			// gemini's yargs parser accepts camelCase spellings of every
			// kebab-case flag — the deny list has to catch those too.
			"privileged_camelcase_spellings_stripped",
			[]string{"--approvalMode", "yolo", "--skipTrust", "--includeDirectories", "/outside", "--allowedTools", "run_shell_command", "--adminPolicy", "/tmp/admin"},
			[]string{"--experimental-acp"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildGeminiACPArgs(c.extra)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("buildGeminiACPArgs(%v) = %#v, want %#v", c.extra, got, c.want)
			}
		})
	}
}

// TestSanitizeGeminiACPEnv pins the nested-agent strip: CLAUDECODE / CLAUDE_*
// / CODEX_IDE_* must not leak into the gemini child, while the rest of the
// shell environment (including GEMINI_*/GOOGLE_*) survives by omission.
func TestSanitizeGeminiACPEnv(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"CLAUDECODE=1",
		"CLAUDE_CODE_ENTRYPOINT=cli",
		"CODEX_IDE_VERSION=0.1.0",
		"GEMINI_API_KEY=g-key",
		"GOOGLE_CLOUD_PROJECT=proj",
		"HOME=/home/user",
	}
	got := sanitizeGeminiACPEnv(in)
	for _, w := range []string{"PATH=/usr/bin", "GEMINI_API_KEY=g-key", "GOOGLE_CLOUD_PROJECT=proj", "HOME=/home/user"} {
		if !envContains(got, w) {
			t.Errorf("expected env to retain %q; got %v", w, got)
		}
	}
	for _, w := range []string{"CLAUDECODE=1", "CLAUDE_CODE_ENTRYPOINT=cli", "CODEX_IDE_VERSION=0.1.0"} {
		if envContains(got, w) {
			t.Errorf("expected env to strip %q; got %v", w, got)
		}
	}
}

/* --------------------------------------------------------------------------
   workspace settings screen
   -------------------------------------------------------------------------- */

// writeGeminiSettings writes cwd/.gemini/settings.json with the given body.
func writeGeminiSettings(t *testing.T, cwd, body string) {
	t.Helper()
	dir := filepath.Join(cwd, ".gemini")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir .gemini: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}
}

func TestScreenGeminiWorkspaceSettings_Allows(t *testing.T) {
	// No file, and benign / non-privileged files, must all pass.
	cases := map[string]string{
		"no file":             "",
		"empty object":        `{}`,
		"malformed json":      `{ not valid`,
		"benign default mode": `{"general":{"defaultApprovalMode":"default"}}`,
		"manual mode":         `{"general":{"defaultApprovalMode":"manual"}}`,
		"autoAccept false":    `{"tools":{"autoAccept":false}}`,
		"empty includeDirs":   `{"context":{"includeDirectories":[]}}`,
		"unrelated settings":  `{"ui":{"theme":"dark"},"context":{"fileName":"AGENTS.md"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			cwd := t.TempDir()
			if body != "" {
				writeGeminiSettings(t, cwd, body)
			}
			if err := screenGeminiWorkspaceSettings(cwd); err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

func TestScreenGeminiWorkspaceSettings_Blocks(t *testing.T) {
	// Each of these positively grants a privilege the argv sanitizer strips,
	// via nested, camelCase, snake_case and legacy-flat spellings.
	cases := map[string]string{
		"nested yolo mode":   `{"general":{"defaultApprovalMode":"yolo"}}`,
		"auto_edit mode":     `{"general":{"defaultApprovalMode":"auto_edit"}}`,
		"camelCase approval": `{"approvalMode":"yolo"}`,
		"autoAccept true":    `{"tools":{"autoAccept":true}}`,
		"legacy flat yolo":   `{"yolo":true}`,
		"includeDirectories": `{"context":{"includeDirectories":["/etc","/root"]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			cwd := t.TempDir()
			writeGeminiSettings(t, cwd, body)
			err := screenGeminiWorkspaceSettings(cwd)
			if err == nil {
				t.Fatalf("expected privilege-escalation error for %s, got nil", body)
			}
			if !strings.Contains(err.Error(), "settings.json") {
				t.Errorf("error should name the offending file; got %v", err)
			}
		})
	}
}

/* --------------------------------------------------------------------------
   Send / Start validation (gemini_acp result-type + noun mapping)
   -------------------------------------------------------------------------- */

func TestGeminiACPManager_Send_NotFound(t *testing.T) {
	m := NewGeminiACPManager()
	err := m.Send("missing", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if err == nil || !strings.Contains(err.Error(), "gemini acp session missing not found") {
		t.Fatalf("expected gemini-branded `not found` error; got %v", err)
	}
}

func TestGeminiACPManager_Send_EndedSession(t *testing.T) {
	m := NewGeminiACPManager()
	id := "ended-fixture"
	fixture := &GeminiACPSession{ID: id, status: "ended", done: make(chan struct{}), streamDone: make(chan struct{})}
	close(fixture.done)
	close(fixture.streamDone)
	m.sessions[id] = fixture

	err := m.Send(id, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if err == nil || !strings.Contains(err.Error(), "has ended") {
		t.Fatalf("expected `has ended` error; got %v", err)
	}
}

// TestGeminiACPLifecycle_StartFailsWhenBinaryMissing pins the startup error
// mapping: a gemini binary that isn't on PATH must surface as a synchronous
// Start error (which the dispatcher publishes as `gemini_acp_error`) naming
// the transport, with no session left registered.
func TestGeminiACPLifecycle_StartFailsWhenBinaryMissing(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("PATH", tmpDir)

	m := NewGeminiACPManager()
	publishFn := func(resultMsg) {}
	err := m.Start("missing-bin", tmpDir, nil, "ws", "uid", GeminiStartOptions{}, publishFn)
	if err == nil {
		t.Fatal("expected start error when gemini binary is not on PATH")
	}
	if !strings.Contains(err.Error(), "failed to start gemini --experimental-acp") {
		t.Errorf("expected error to name the gemini transport; got %q", err.Error())
	}
	if m.ActiveCount() != 0 {
		t.Errorf("manager should have 0 sessions after failed start; got %d", m.ActiveCount())
	}
}

/* --------------------------------------------------------------------------
   Lifecycle against the mock gemini ACP server
   -------------------------------------------------------------------------- */

// startMockGeminiACP copies the test binary into a tempdir as `gemini`,
// points PATH at it and selects the given TEST_MOCK_CLI_MODE. Returns the
// tempdir used as the session cwd.
func startMockGeminiACP(t *testing.T, mode string) string {
	t.Helper()
	testExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	tmpDir := t.TempDir()
	mockName := "gemini"
	if runtime.GOOS == "windows" {
		mockName += ".exe"
	}
	mockPath := filepath.Join(tmpDir, mockName)
	if err := copyTestBinary(testExe, mockPath); err != nil {
		t.Fatalf("copy mock binary: %v", err)
	}
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(mockCLIEnvVar, mode)
	return tmpDir
}

// waitForResultType polls captured until a message of the given type shows up
// or the deadline passes.
func waitForResultType(mu *sync.Mutex, captured *[]resultMsg, msgType string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, msg := range *captured {
			if msg.Type == msgType {
				mu.Unlock()
				return true
			}
		}
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// TestGeminiACPLifecycle_StartSendEnd drives the full ACP handshake through
// the Gemini driver: initialize → session/new → session/prompt → streaming
// session/update → final response → end. Pins the gemini_acp_* result-type
// mapping the orchestrator relies on (verbatim `gemini_acp_message` frames,
// `gemini_acp_stderr` forwarding, terminal `gemini_acp_ended`).
func TestGeminiACPLifecycle_StartSendEnd(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("integration test only runs on win/linux/darwin")
	}
	tmpDir := startMockGeminiACP(t, "gemini-acp-echo")

	m := NewGeminiACPManager()
	id := fmt.Sprintf("gemini-test-%d", time.Now().UnixNano())

	var mu sync.Mutex
	var captured []resultMsg
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	if err := m.Start(id, tmpDir, nil, "ws", "uid", GeminiStartOptions{}, publishFn); err != nil {
		t.Fatalf("Start: %v", err)
	}

	initFrame := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientCapabilities":{"fs":{"readTextFile":true,"writeTextFile":true}}}}`
	if err := m.Send(id, initFrame); err != nil {
		t.Fatalf("Send initialize: %v", err)
	}
	cwdJSON, err := json.Marshal(tmpDir)
	if err != nil {
		t.Fatalf("marshal cwd: %v", err)
	}
	sessFrame := fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":%s,"mcpServers":[]}}`, cwdJSON)
	if err := m.Send(id, sessFrame); err != nil {
		t.Fatalf("Send session/new: %v", err)
	}
	promptFrame := `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"sess_mock","prompt":[{"type":"text","text":"hi"}]}}`
	if err := m.Send(id, promptFrame); err != nil {
		t.Fatalf("Send session/prompt: %v", err)
	}

	// Wait for responses ids 1..3 plus the session/update notification.
	deadline := time.Now().Add(15 * time.Second)
	requiredIDs := map[float64]bool{1: false, 2: false, 3: false}
	gotSessionUpdate := false
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, msg := range captured {
			if msg.Type != "gemini_acp_message" {
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
			if method, ok := probe["method"].(string); ok && method == "session/update" {
				gotSessionUpdate = true
			}
		}
		mu.Unlock()
		allDone := gotSessionUpdate
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

	for rid, got := range requiredIDs {
		if !got {
			t.Errorf("missing JSON-RPC response for id=%v", rid)
		}
	}
	if !gotSessionUpdate {
		t.Errorf("missing `session/update` notification — streaming path not exercised")
	}

	mu.Lock()
	sawStderr := false
	for _, msg := range captured {
		if msg.Type == "gemini_acp_stderr" && strings.Contains(msg.Output, "mock-gemini") {
			sawStderr = true
			break
		}
	}
	mu.Unlock()
	if !sawStderr {
		t.Errorf("expected `gemini_acp_stderr` message containing `mock-gemini`")
	}

	if err := m.End(id); err != nil {
		t.Fatalf("End: %v", err)
	}

	if !waitForResultType(&mu, &captured, "gemini_acp_ended", 5*time.Second) {
		t.Fatalf("no gemini_acp_ended frame after End; got types %v", extractTypes(captured))
	}

	mu.Lock()
	defer mu.Unlock()
	last := captured[len(captured)-1]
	if last.Type != "gemini_acp_ended" {
		t.Errorf("expected final message to be gemini_acp_ended; got %q", last.Type)
	}
	if last.SessionID != id {
		t.Errorf("expected SessionID=%q on ended frame; got %q", id, last.SessionID)
	}
	if m.ActiveCount() != 0 {
		t.Errorf("expected 0 active sessions after End; got %d", m.ActiveCount())
	}
}

// TestGeminiACPLifecycle_ForwardsBadFrameAsError pins the error mapping for a
// child that emits non-JSON on stdout — the classic failure shape of an old
// gemini build rejecting `--experimental-acp` with a plain-text usage error.
// The manager must surface a `gemini_acp_error` (not silently forward the
// garbage) and still deliver the terminal `gemini_acp_ended`.
func TestGeminiACPLifecycle_ForwardsBadFrameAsError(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("integration test only runs on win/linux/darwin")
	}
	tmpDir := startMockGeminiACP(t, "gemini-acp-bad-frame")

	m := NewGeminiACPManager()
	id := fmt.Sprintf("gemini-badframe-test-%d", time.Now().UnixNano())

	var mu sync.Mutex
	var captured []resultMsg
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	if err := m.Start(id, tmpDir, nil, "ws", "uid", GeminiStartOptions{}, publishFn); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !waitForResultType(&mu, &captured, "gemini_acp_ended", 10*time.Second) {
		t.Fatalf("no gemini_acp_ended frame; got types %v", extractTypes(captured))
	}

	mu.Lock()
	defer mu.Unlock()
	sawError := false
	for _, msg := range captured {
		if msg.Type == "gemini_acp_error" && strings.Contains(msg.Output, "non-JSON frame on gemini acp stdout") {
			sawError = true
			if msg.Status != "error" {
				t.Errorf("expected Status=error on bad-frame surface; got %q", msg.Status)
			}
		}
	}
	if !sawError {
		t.Errorf("expected `gemini_acp_error` surfacing the non-JSON frame; got types %v",
			extractTypes(captured))
	}
}

// TestGeminiACPLifecycle_CapturesQuotaFromStream pins the ACP-path bridge to
// captureGeminiUsageLimitLine: multi-turn Gemini chat runs through
// `gemini_acp_start` / the core readStream rather than the raw
// `session_start` path in session.go that already wires the hook, so without
// the spec.captureLine hook a 429/RESOURCE_EXHAUSTED frame never populates
// `gemini_usage_limit.json` and the CLI Agents card stays silent for the
// primary Gemini flow.
func TestGeminiACPLifecycle_CapturesQuotaFromStream(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("integration test only runs on win/linux/darwin")
	}
	tmpDir := startMockGeminiACP(t, "gemini-acp-quota")

	cachePath := filepath.Join(tmpDir, "gemini_usage_limit.json")
	t.Setenv("AIEXPEDITE_GEMINI_LIMIT_CACHE", cachePath)

	m := NewGeminiACPManager()
	id := fmt.Sprintf("gemini-quota-test-%d", time.Now().UnixNano())

	var mu sync.Mutex
	var captured []resultMsg
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	if err := m.Start(id, tmpDir, nil, "ws", "uid", GeminiStartOptions{}, publishFn); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !waitForResultType(&mu, &captured, "gemini_acp_ended", 10*time.Second) {
		t.Fatalf("no gemini_acp_ended frame; got types %v", extractTypes(captured))
	}

	mu.Lock()
	sawQuotaFrame := false
	for _, msg := range captured {
		if msg.Type == "gemini_acp_message" && strings.Contains(msg.Output, "RESOURCE_EXHAUSTED") {
			sawQuotaFrame = true
			break
		}
	}
	mu.Unlock()
	if !sawQuotaFrame {
		t.Fatalf("expected the RESOURCE_EXHAUSTED frame to be forwarded as gemini_acp_message; got types %v",
			extractTypes(captured))
	}

	raw, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("usage-limit cache was never written at %s: %v", cachePath, err)
	}
	var state geminiUsageLimitState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("usage-limit cache is not valid JSON: %v\n%s", err, raw)
	}
	if state.Severity != geminiLimitReached {
		t.Errorf("expected severity=%q in cache, got %q", geminiLimitReached, state.Severity)
	}
	if !strings.Contains(state.Message, "Quota exceeded") {
		t.Errorf("expected the quota message to be captured, got %q", state.Message)
	}
}

/* --------------------------------------------------------------------------
   First-frame watchdog error mapping
   -------------------------------------------------------------------------- */

// TestGeminiACPManager_WatchFirstFrame_FiresAuthErrorOnSilence pins the
// gemini branding of the shared watchdog fail-fast: a session that never
// emits a stdout frame within the budget must produce exactly one
// `gemini_acp_error` whose message points the user at re-authenticating with
// `gemini`, instead of hanging at "Gemini ACP started" forever.
func TestGeminiACPManager_WatchFirstFrame_FiresAuthErrorOnSilence(t *testing.T) {
	m := NewGeminiACPManager()
	session := &GeminiACPSession{
		ID:          "gemini-silent-1",
		WorkspaceID: "ws",
		UID:         "uid",
		status:      "running",
		done:        make(chan struct{}),
		streamDone:  make(chan struct{}),
		firstFrame:  make(chan struct{}),
	}

	var mu sync.Mutex
	var captured []resultMsg
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	m.watchFirstFrame(session, publishFn, 30*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("expected exactly 1 published frame, got %d: %+v", len(captured), captured)
	}
	got := captured[0]
	if got.Type != "gemini_acp_error" {
		t.Fatalf("expected gemini_acp_error, got %q", got.Type)
	}
	if got.Status != "error" {
		t.Fatalf("expected status error, got %q", got.Status)
	}
	if !strings.Contains(got.Output, "no output") || !strings.Contains(got.Output, "gemini") {
		t.Fatalf("expected actionable auth message, got %q", got.Output)
	}
}
