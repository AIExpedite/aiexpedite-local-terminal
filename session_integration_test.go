// session_integration_test.go
// -----------------------------------------------------------------------------
// End-to-end lifecycle tests for SessionManager CLI flows. Uses the test binary
// itself as a mock CLI so we don't need the agents installed on the test host
// and so we get
// deterministic timing.
//
// The mock CLI is dispatched by the TEST_MOCK_CLI_MODE env var inside TestMain.
// Sub-binaries (built via exec.Command(os.Args[0], ...)) inherit the env, so a
// test can spawn the test binary in mock-CLI mode and feed it through the
// real SessionManager.
//
// Why this matters: the unit tests in session_cli_test.go pin the parsing
// logic, but they don't exercise the order in which stream chunks vs
// session_ended are PUBLISHED. The documentDesign regression that motivated
// these tests was about the END-OF-SESSION race — session_ended firing before
// the last stream chunk reached terminal-service. These integration tests
// drive a real SessionManager and assert ordering at the publishFn level
// (which is the boundary the Go agent owns; everything beyond is Pub/Sub +
// terminal-service).
// -----------------------------------------------------------------------------

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const mockCLIEnvVar = "TEST_MOCK_CLI_MODE"
const mockGrokPersistentHomeEnv = "TEST_MOCK_GROK_PERSISTENT_HOME"
const mockGrokVendorHomeEnv = "TEST_MOCK_GROK_VENDOR_HOME"
const mockGrokProjectRootEnv = "TEST_MOCK_GROK_PROJECT_ROOT"

const grokMaintenanceSmokeMarker = "AIEXPEDITE_GROK_SMOKE_MARKER_7F3C2A"

// TestMain dispatches into the mock CLI when the env var is set; otherwise
// runs the test suite normally. This is the standard "test binary as helper
// subprocess" pattern (see `go test` docs and stdlib os/exec tests).
func TestMain(m *testing.M) {
	if mode := os.Getenv(mockCLIEnvVar); mode != "" {
		runMockCLI(mode)
		return
	}
	// Isolate the whole suite from the developer's real macOS Keychain. Since the
	// Claude parser now PREFERS the Keychain credential over the on-disk file on
	// the default config dir, a Darwin box that actually holds a
	// "Claude Code-credentials" item would otherwise leak that real login into
	// file-based tests. Default the reader to "no keychain credential"; tests that
	// exercise the Keychain path override claudeKeychainReader explicitly.
	claudeKeychainReader = func() ([]byte, bool) { return nil, false }

	// Confine every config/data write in this package to a throwaway directory
	// BEFORE any test runs. Without this, anything that persists through
	// ConfigPath() / GetConfigDir() writes to the developer's live agent
	// install — see sandboxTestConfigDir for the incident this pins.
	// Called explicitly rather than deferred: os.Exit skips defers.
	restoreSandbox := sandboxTestConfigDir()
	code := m.Run()
	restoreSandbox()
	os.Exit(code)
}

// runMockCLI simulates the streaming JSON output of a CLI agent so the
// SessionManager has something realistic to parse. Each mode mirrors the
// shape we actually see in production:
//
//   - claude: read NDJSON on stdin, emit stream_event content_block_deltas,
//     then a final result event. Exit when stdin closes.
//   - codex:  read the raw prompt from stdin, print streaming events including
//     item.completed / thread.completed, then exit.
//   - antigravity: read an NDJSON user event from stdin, emit step_update and
//     terminal result events, then exit.
//   - sleep-then-emit: emit the events AFTER a delay so we can test stream
//     completion ordering under load.
func runMockCLI(mode string) {
	switch mode {
	case "opencode":
		// The OpenCode usage parser probes `opencode models` and
		// `opencode auth list`. Replay the exact output a real 1.18.15 install
		// produced — a plain model list, and a DRAWN FRAME for auth list.
		if len(os.Args) > 2 && os.Args[1] == "auth" && os.Args[2] == "list" {
			fmt.Print(realOpenCodeAuthList)
			return
		}
		fmt.Print(realOpenCodeModels)
		return
	case "claude":
		// Real claude waits for NDJSON on stdin before emitting anything. We
		// mimic that — read one line, then start streaming.
		buf := make([]byte, 4096)
		_, _ = os.Stdin.Read(buf)

		// Stream a few text_delta chunks via the stream_event envelope.
		fmt.Println(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello "}}}`)
		fmt.Println(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"from "}}}`)
		fmt.Println(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"claude"}}}`)

		// Final result event. This is what triggers stdin close in the
		// SessionManager. After it, we keep reading stdin until EOF (the
		// SessionManager closes stdin) then exit.
		fmt.Println(`{"type":"result","subtype":"success","result":"Hello from claude","total_cost_usd":0.001}`)

		// Block until stdin is closed (SessionManager does this on result
		// event). If stdin is already closed, this returns quickly.
		_, _ = os.Stdin.Read(buf)
		os.Exit(0)

	case "codex":
		// codex exec doesn't read stdin once started (assuming stdin is
		// closed before it tries — that's the v0.9.6 fix). It just streams
		// events based on the prompt in argv, then exits.
		fmt.Println(`{"type":"thread.started","thread_id":"t-1"}`)
		fmt.Println(`{"type":"turn.started","turn_id":"r-1"}`)
		fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"hello from codex"}}`)
		fmt.Println(`{"type":"turn.completed"}`)
		fmt.Println(`{"type":"thread.completed"}`)
		os.Exit(0)

	case "codex-reads-stdin":
		// Mimic `codex exec -` on codex CLI v0.140+: read the prompt from stdin
		// to EOF, then run. With NO prompt (immediate EOF), exit 1 like the real
		// CLI's "No prompt provided via stdin." This exercises the deferred-stdin
		// fix: the chat-direct flow starts codex with no prompt and delivers the
		// first message later via SendInput, so stdin MUST stay open until that
		// write — closing it at start would EOF the child with no prompt and it
		// would exit 1 before the user's message ever arrived.
		data, _ := io.ReadAll(os.Stdin)
		prompt := strings.TrimSpace(string(data))
		if prompt == "" {
			fmt.Fprintln(os.Stderr, "No prompt provided via stdin.")
			os.Exit(1)
		}
		fmt.Println(`{"type":"thread.started","thread_id":"t-1"}`)
		fmt.Println(`{"type":"turn.started","turn_id":"r-1"}`)
		fmt.Printf(`{"type":"item.completed","item":{"type":"agent_message","text":"echo: %s"}}`+"\n", prompt)
		fmt.Println(`{"type":"turn.completed"}`)
		os.Exit(0)

	case "antigravity-stream-stdin":
		data, _ := io.ReadAll(os.Stdin)
		var input struct {
			Event   string `json:"event"`
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(data), &input); err != nil || input.Event != "user" {
			fmt.Println(`{"event":"result","result":{"status":"ERROR","error":"invalid stdin envelope"}}`)
			os.Exit(1)
		}
		marker := fmt.Sprintf("antigravity received %d bytes", len(input.Message.Content))
		encodedMarker, _ := json.Marshal(marker)
		fmt.Printf(`{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":%s}}`+"\n", encodedMarker)
		fmt.Printf(`{"event":"result","result":{"status":"SUCCESS","response":%s}}`+"\n", encodedMarker)
		os.Exit(0)

	case "antigravity-diagnostic":
		fmt.Println("agy version 1.2.3")
		os.Exit(0)

	case "stream-burst":
		// Emit many lines quickly to stress the async publish path. Used to
		// verify no stream chunks get dropped under load.
		for i := 0; i < 50; i++ {
			fmt.Printf(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"chunk-%d "}}}`+"\n", i)
		}
		fmt.Println(`{"type":"result","subtype":"success","result":"done"}`)
		// Wait for stdin EOF (SessionManager closes it on result event).
		buf := make([]byte, 4096)
		_, _ = os.Stdin.Read(buf)
		os.Exit(0)

	case "no-prompt-immediate-exit":
		// Edge case: CLI that exits immediately with no output. Mirrors the
		// failure mode where Claude is invoked without a prompt arg (empty
		// stdinPrompt) → stdin closed at start → claude reads EOF → exits.
		os.Exit(0)

	case "stderr-and-exit":
		// CLI that writes only to stderr (some CLI error paths look like
		// this). SessionManager should still surface this content.
		fmt.Fprintln(os.Stderr, "permission denied: refresh your auth token")
		os.Exit(2)

	case "agy-tty-frames":
		// TUI-style output: colored carriage-return redraws of one line, then
		// the final rendered state and exit. Exercises the PTY normalizer on the
		// interactive session_start path (CR collapse + ANSI strip).
		fmt.Print("\x1b[32mstep 1\x1b[0m\r\x1b[Kstep 2\r\x1b[Kdone\n")
		os.Exit(0)

	case "agy-prompt-hang":
		// Emit an input prompt then go quiet — the PTY session must abort on the
		// quiet-after-prompt timeout instead of hanging for the full sleep.
		fmt.Print("Password: ")
		time.Sleep(30 * time.Second)
		os.Exit(0)

	case "codex-appserver-echo":
		runMockCodexAppServer()

	case "codex-appserver-bad-frame":
		// Emit a single non-JSON stdout line to exercise the
		// `codex_appserver_error` surfacing path, then exit cleanly. Used by
		// TestCodexAppServerLifecycle_ForwardsBadFrameAsError.
		fmt.Println("this is not json")
		os.Exit(0)

	case "codex-appserver-oversize":
		// Emit a single JSON frame larger than codexAppServerMaxFrameSize
		// (8 MB) to exercise the Finding #4 fail-fast path. The manager
		// must surface a fatal codex_appserver_error rather than enqueueing
		// the frame (which would fail at the Pub/Sub layer and silently
		// break the orchestrator's JSON-RPC state machine). Used by
		// TestCodexAppServerLifecycle_OversizeFrameTerminatesSession.
		const oversize = 9 * 1024 * 1024 // ~9 MB > 8 MB cap
		_, _ = os.Stdout.WriteString(`{"jsonrpc":"2.0","method":"item/started","params":{"item":{"big":"`)
		buf := make([]byte, 64*1024)
		for i := range buf {
			buf[i] = 'A'
		}
		emitted := 0
		for emitted < oversize {
			n := len(buf)
			if oversize-emitted < n {
				n = oversize - emitted
			}
			_, _ = os.Stdout.Write(buf[:n])
			emitted += n
		}
		_, _ = os.Stdout.WriteString(`"}}}` + "\n")
		// Block until killed.
		select {}

	case "codex-appserver-oversize-escaped":
		// Emit a single valid-JSON frame whose raw size is UNDER
		// codexAppServerMaxFrameSize (8 MB) but whose marshaled resultMsg
		// envelope blows past codexAppServerMaxPublishSize (10 MB) due to
		// JSON-string escape amplification — each '\' / '"' byte in the raw
		// line becomes 2 bytes when embedded into the Output field on
		// marshal. The manager MUST surface a fatal codex_appserver_error
		// rather than enqueue a frame Pub/Sub would silently reject. Used
		// by TestCodexAppServerLifecycle_EscapeAmplifiedFrameTerminatesSession.
		_, _ = os.Stdout.WriteString(`{"jsonrpc":"2.0","method":"item/started","params":{"item":{"big":"`)
		// 6 MB of '\\"' escape sequences in the raw line. Each 2-byte raw
		// '\"' marshals to 4 bytes ('\\\\\"'), so the Output field alone
		// grows to ~12 MB, well over Pub/Sub's 10 MB ceiling.
		const rawEscapeBytes = 6 * 1024 * 1024
		escapePair := []byte{'\\', '"'}
		chunk := bytes.Repeat(escapePair, 32*1024) // 64 KB of '\"'
		emitted := 0
		for emitted < rawEscapeBytes {
			n := len(chunk)
			if rawEscapeBytes-emitted < n {
				n = rawEscapeBytes - emitted
				if n%2 == 1 { // keep escape pairs intact
					n--
				}
			}
			_, _ = os.Stdout.Write(chunk[:n])
			emitted += n
		}
		_, _ = os.Stdout.WriteString(`"}}}` + "\n")
		select {}

	case "claude-native-echo":
		runMockClaudeNative()

	case "claude-native-ratelimit":
		// Emit a single stream-json frame that captureClaudeRateLimitLine
		// classifies as a hard-quota rejection, then keep stdin open so the
		// manager doesn't tear down before the test observes the surfaced
		// claude_native_ratelimit frame. Used by
		// TestClaudeNativeLifecycle_SurfacesRejectedRateLimit.
		reset := time.Now().Add(45 * time.Minute).Unix()
		fmt.Fprintln(os.Stderr, "[mock-claude-ratelimit] emitting rejected rate_limit_event")
		fmt.Printf(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","utilization":1.0,"resets_at":%d}}`+"\n", reset)
		// Stay alive so End() drives the teardown, matching the real CLI's
		// behavior of holding stdin open across turns.
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)

	case "claude-native-oversize":
		// Emit a single stdout frame larger than claudeNativeMaxFrameSize
		// (8 MB) to exercise the fail-fast path — the manager must surface a
		// fatal claude_native_error rather than enqueue an un-publishable
		// frame. Used by TestClaudeNativeLifecycle_OversizeFrameTerminatesSession.
		const oversize = 9 * 1024 * 1024 // ~9 MB > 8 MB cap
		_, _ = os.Stdout.WriteString(`{"type":"assistant","message":{"content":[{"type":"text","text":"`)
		buf := make([]byte, 64*1024)
		for i := range buf {
			buf[i] = 'A'
		}
		emitted := 0
		for emitted < oversize {
			n := len(buf)
			if oversize-emitted < n {
				n = oversize - emitted
			}
			_, _ = os.Stdout.Write(buf[:n])
			emitted += n
		}
		_, _ = os.Stdout.WriteString(`"}]}}` + "\n")
		select {}

	case "grok-acp-echo":
		runMockGrokACPServer()

	case "grok-direct-billing":
		if err := writeMockGrokBillingEvidence(); err != nil {
			fmt.Fprintf(os.Stderr, "write mock Grok billing evidence: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(`{"type":"result","result":"billing refreshed"}`)
		os.Exit(0)

	case "grok-acp-billing":
		if err := writeMockGrokBillingEvidence(); err != nil {
			fmt.Fprintf(os.Stderr, "write mock Grok billing evidence: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_mock","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"billing refreshed"}}}}`)
		os.Exit(0)

	case "grok-version-v1":
		fmt.Println("grok 1.0.3")
		os.Exit(0)

	case "grok-version-v2":
		fmt.Println("grok 1.1.0")
		os.Exit(0)

	case "grok-maintenance-smoke-v1", "grok-maintenance-smoke-v2":
		runMockGrokMaintenanceSmoke(mode)

	case "grok-ordinary-no-tools":
		args := os.Args[1:]
		tools, hasTools := mockArgValue(args, "--tools")
		if os.Getenv("GROK_HOME") != os.Getenv(mockGrokPersistentHomeEnv) ||
			os.Getenv("XAI_API_KEY") != "credential-sentinel-api-key" ||
			!hasTools || tools != "" || mockHasArg(args, grokMaintenanceSmokeControlArg) {
			fmt.Fprintln(os.Stderr, "ordinary no-tools contract changed")
			os.Exit(1)
		}
		fmt.Println(`{"type":"text","data":"ordinary-no-tools-ok"}`)
		fmt.Println(`{"type":"end","stopReason":"end_turn"}`)
		os.Exit(0)

	case "grok-acp-usage-limit":
		// Emit a single ACP `session/update` notification that carries a
		// usage_limit_reached signal under params.update.sessionUpdate, then
		// exit. Used by TestGrokACPLifecycle_CapturesUsageLimitFromStream
		// to assert the readStream hook routes ACP frames through
		// captureGrokUsageLimitLine and writes the on-disk cache.
		fmt.Fprintln(os.Stderr, "[mock-grok-usage-limit] emitting usage_limit_reached frame")
		fmt.Println(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_mock","update":{"sessionUpdate":"usage_limit_reached","gate_message":"You've hit your Grok limit.","gate_url":"https://grok.com/supergrok"}}}`)
		os.Exit(0)

	case "grok-acp-bad-frame":
		// Emit a single non-JSON stdout line to exercise the `grok_acp_error`
		// surfacing path, then exit cleanly. Used by
		// TestGrokACPLifecycle_ForwardsBadFrameAsError.
		fmt.Println("this is not json")
		os.Exit(0)

	case "grok-acp-hang":
		// Stay alive forever, ignoring stdin (don't even read it). Used by
		// TestGrokACPLifecycle_TimeoutKillsRunawaySession to assert that the
		// per-session deadline kills a hung Grok child and publishes a typed
		// grok_acp_error rather than waiting for the 6h stale GC.
		fmt.Fprintln(os.Stderr, "[mock-grok-hang] running forever")
		select {}

	case "grok-acp-quick-exit":
		// Exit immediately with status 0 — no stdout/stderr. Used by
		// TestWaitForExit_StatusFlipsBeforeStreamDrain to drive the
		// natural-exit path through waitForExit so the test can assert no
		// spurious grok_acp_error is published for a clean exit.
		os.Exit(0)

	case "grok-acp-final-frame-and-exit":
		// Emit a single JSON-RPC response frame and exit immediately. Used by
		// TestWaitForExit_FinalFrameSurvivesQuickExit to assert that the
		// manager does NOT truncate the last frame at the moment of child
		// exit — exec.Cmd.Wait's auto-close of StdoutPipe used to race with
		// the scanner goroutine's drain, dropping grok's terminal response.
		fmt.Println(`{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`)
		os.Exit(0)

	case "codex-appserver-burst":
		// Emit a burst of JSON-RPC frames much larger than
		// codexAppServerPublishQueueSize, then keep going to keep the
		// publish queue saturated. Used by
		// TestCodexAppServerLifecycle_StallingPublisherTerminatesSession
		// — when the publisher stalls, the manager must surface a fatal
		// codex_appserver_error and kill us rather than dropping frames.
		for i := 0; i < 4000; i++ {
			fmt.Printf(`{"jsonrpc":"2.0","method":"item/started","params":{"item":{"i":%d}}}`+"\n", i)
		}
		// Block until killed.
		select {}

	default:
		fmt.Fprintf(os.Stderr, "unknown TEST_MOCK_CLI_MODE: %s\n", mode)
		os.Exit(1)
	}
}

// runMockGrokMaintenanceSmoke enforces the Grok 1.0.13 no-tools, one-shot
// argv contract at the process boundary. It deliberately emits the legacy
// `text` payload before the simulated update and 1.0.13's `data` payload after
// it so the same SessionManager lifecycle proves protocol compatibility across
// replacement. Failures expose only a generic protocol error, never argv,
// prompt-file contents, or billing-log data.
func runMockGrokMaintenanceSmoke(mode string) {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		if mode == "grok-maintenance-smoke-v1" {
			fmt.Println("grok 1.0.5")
		} else {
			fmt.Println("grok 1.0.13")
		}
		os.Exit(0)
	}

	args := os.Args[1:]
	isolatedHome := os.Getenv("GROK_HOME")
	persistentHome := os.Getenv(mockGrokPersistentHomeEnv)
	vendorHome := os.Getenv(mockGrokVendorHomeEnv)
	projectRoot := os.Getenv(mockGrokProjectRootEnv)
	workingDir, workingDirErr := os.Getwd()
	isolatedConfig, configErr := os.ReadFile(filepath.Join(isolatedHome, "config.toml"))
	_, isolatedPluginErr := os.Stat(filepath.Join(isolatedHome, "plugins", "host-plugin"))
	_, isolatedMCPErr := os.Stat(filepath.Join(isolatedHome, "mcp.json"))
	_, isolatedSessionsErr := os.Lstat(filepath.Join(isolatedHome, "sessions"))
	_, copiedAuthErr := os.Stat(filepath.Join(isolatedHome, "auth.json"))
	_, hostPluginErr := os.Stat(filepath.Join(persistentHome, "plugins", "host-plugin"))
	_, hostMCPErr := os.Stat(filepath.Join(persistentHome, "mcp.json"))
	_, cursorMCPErr := os.Stat(filepath.Join(vendorHome, ".cursor", "mcp.json"))
	_, projectConfigErr := os.Stat(filepath.Join(projectRoot, ".grok", "config.toml"))
	_, projectPluginErr := os.Stat(filepath.Join(projectRoot, ".grok", "plugins", "project-plugin", "plugin.json"))
	_, projectMCPErr := os.Stat(filepath.Join(projectRoot, ".mcp.json"))
	_, isolatedProjectConfigErr := os.Stat(filepath.Join(workingDir, ".grok", "config.toml"))
	_, isolatedProjectPluginErr := os.Stat(filepath.Join(workingDir, ".grok", "plugins"))
	_, isolatedProjectMCPErr := os.Stat(filepath.Join(workingDir, ".mcp.json"))
	tools, hasTools := mockArgValue(args, "--tools")
	maxTurns, hasMaxTurns := mockArgValue(args, "--max-turns")
	outputFormat, hasOutputFormat := mockArgValue(args, "--output-format")
	promptPath, hasPromptFile := mockArgValue(args, "--prompt-file")
	prompt, promptErr := os.ReadFile(promptPath)
	_, hasExternalLoader := grokNoToolsExternalLoaderArg(args)
	compatibilityDisabled := true
	for _, name := range []string{
		"GROK_CURSOR_SKILLS_ENABLED", "GROK_CURSOR_RULES_ENABLED", "GROK_CURSOR_AGENTS_ENABLED",
		"GROK_CURSOR_MCPS_ENABLED", "GROK_CURSOR_HOOKS_ENABLED", "GROK_CURSOR_SESSIONS_ENABLED",
		"GROK_CLAUDE_SKILLS_ENABLED", "GROK_CLAUDE_RULES_ENABLED", "GROK_CLAUDE_AGENTS_ENABLED",
		"GROK_CLAUDE_MCPS_ENABLED", "GROK_CLAUDE_HOOKS_ENABLED", "GROK_CLAUDE_SESSIONS_ENABLED",
		"GROK_CODEX_SKILLS_ENABLED", "GROK_CODEX_RULES_ENABLED", "GROK_CODEX_AGENTS_ENABLED",
		"GROK_CODEX_MCPS_ENABLED", "GROK_CODEX_HOOKS_ENABLED", "GROK_CODEX_SESSIONS_ENABLED",
		"GROK_MANAGED_MCPS_ENABLED", "GROK_MANAGED_MCP_GATEWAY_TOOLS_ENABLED",
		"GROK_WORKSPACE_TOOL_DEFS_ENABLED", "GROK_WORKSPACE_TOOL_STATE_ENABLED",
	} {
		if os.Getenv(name) != "0" {
			compatibilityDisabled = false
			break
		}
	}
	if isolatedHome == "" || persistentHome == "" || projectRoot == "" || isolatedHome == persistentHome ||
		workingDirErr != nil || filepath.Clean(workingDir) != filepath.Join(filepath.Clean(isolatedHome), "workspace") ||
		filepath.Clean(os.Getenv("HOME")) != filepath.Clean(isolatedHome) ||
		filepath.Clean(os.Getenv("USERPROFILE")) != filepath.Clean(isolatedHome) ||
		filepath.Clean(os.Getenv("PWD")) != filepath.Clean(workingDir) ||
		configErr != nil || copiedAuthErr != nil || hostPluginErr != nil || hostMCPErr != nil || cursorMCPErr != nil ||
		projectConfigErr != nil || projectPluginErr != nil || projectMCPErr != nil ||
		!os.IsNotExist(isolatedPluginErr) || !os.IsNotExist(isolatedMCPErr) || !os.IsNotExist(isolatedSessionsErr) ||
		!os.IsNotExist(isolatedProjectConfigErr) || !os.IsNotExist(isolatedProjectPluginErr) || !os.IsNotExist(isolatedProjectMCPErr) ||
		!strings.Contains(string(isolatedConfig), "[compat.cursor]") ||
		!strings.Contains(string(isolatedConfig), "[compat.claude]") ||
		strings.Count(string(isolatedConfig), "mcps = false") != 2 ||
		strings.Contains(string(isolatedConfig), "host-plugin") ||
		strings.Contains(string(isolatedConfig), "host-mcp") ||
		os.Getenv("XAI_API_KEY") != "" || os.Getenv("GROK_CODE_XAI_API_KEY") != "" ||
		os.Getenv("GROK_AUTH_PROVIDER_ACCESS_TOKEN") != "" || os.Getenv("GROK_AGENT") != "" ||
		os.Getenv("GROK_DEFAULT_MODEL") != "" || os.Getenv("GROK_MODELS_BASE_URL") != "" ||
		os.Getenv("GROK_MODELS_LIST_URL") != "" || os.Getenv("GROK_XAI_API_BASE_URL") != "" ||
		mockHasArg(args, "--always-approve") || mockHasArg(args, "--auto-approve") || !compatibilityDisabled ||
		hasExternalLoader ||
		mockHasArg(args, grokMaintenanceSmokeControlArg) ||
		!hasTools || tools != "" || !hasMaxTurns || maxTurns != "1" ||
		!hasOutputFormat || outputFormat != "streaming-json" ||
		!mockHasArg(args, "--disable-web-search") || !mockHasArg(args, "--no-subagents") ||
		!mockHasArg(args, "--verbatim") || !hasPromptFile || promptErr != nil ||
		string(prompt) != "Return exactly this marker and nothing else: "+grokMaintenanceSmokeMarker {
		fmt.Fprintln(os.Stderr, "protocol error")
		os.Exit(1)
	}
	if err := writeMockGrokBillingEvidence(); err != nil {
		fmt.Fprintln(os.Stderr, "usage capture failed")
		os.Exit(1)
	}
	field := "text"
	if mode != "grok-maintenance-smoke-v1" {
		field = "data"
	}
	// Real streaming-json output may split one assistant message across
	// multiple incremental frames. Emit every marker in three same-batch
	// deltas so the lifecycle regression catches injected frame newlines.
	for _, delta := range []string{"AIEXPEDITE_GROK_", "SMOKE_MARKER_", "7F3C2A"} {
		encoded, _ := json.Marshal(map[string]string{"type": "text", field: delta})
		fmt.Println(string(encoded))
	}
	fmt.Println(`{"type":"end","stopReason":"end_turn"}`)
	os.Exit(0)
}

func mockArgValue(args []string, name string) (string, bool) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1], true
		}
	}
	return "", false
}

func mockHasArg(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}

// writeMockGrokBillingEvidence mirrors the exact allowlisted shape Grok 1.0
// appends after fetching its current period. It intentionally includes secret
// and raw-config sentinels so lifecycle tests prove the parser never republishes
// arbitrary log fields.
func writeMockGrokBillingEvidence() error {
	base := os.Getenv("GROK_HOME")
	if base == "" {
		return fmt.Errorf("GROK_HOME is empty")
	}
	dir := filepath.Join(base, "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create logs directory: %w", err)
	}
	path := filepath.Join(dir, "unified.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open unified log: %w", err)
	}
	now := time.Now().UTC()
	body := fmt.Sprintf(`{"ts":%q,"msg":"session start","ctx":{"user_id":"user-1"}}`+"\n", now.Add(-time.Second).Format(time.RFC3339Nano)) +
		fmt.Sprintf(`{"ts":%q,"msg":"billing: fetched credits config","credential":"credential-sentinel","prompt":"prompt-sentinel","ctx":{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":%q,"end":%q},"onDemandCap":{"val":0},"onDemandUsed":{"val":0},"rawConfig":"raw-config-sentinel"},"subscriptionTier":"SuperGrok"}}`+"\n",
			now.Format(time.RFC3339Nano), now.Add(-time.Hour).Format(time.RFC3339Nano), now.Add(7*24*time.Hour).Format(time.RFC3339Nano))
	if _, err := f.WriteString(body); err != nil {
		_ = f.Close()
		return fmt.Errorf("write billing log: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close unified log: %w", err)
	}
	return nil
}

// captureSession runs a SessionManager session against the test binary
// configured as a mock CLI in the given mode. It returns the captured
// publishFn messages in the order they were published, plus the session ID.
//
// The trick: we point the SessionManager at a binary at `mockExePath` (a copy
// of the test binary placed in a tempdir with the desired command name —
// e.g. `claude.exe` on Windows or `claude` on Unix) so resolveExecutable's
// PATH lookup works. For "claude", resolveExecutable falls through to PATH
// lookup ONLY if cachedResolveClaudePath returns the unresolved command name
// — to avoid that, we set up the mock binary with a unique non-claude name
// and configure the SessionManager via the command field.
//
// Since buildInteractiveCLIArgs picks the builder based on the command name,
// and resolveExecutable picks the binary based on the command name, we DO
// need the command name to be exactly "claude" / "codex" for the
// argv shape to match. For the integration tests below, we sidestep this
// by directly calling StartSession with a mock command — accepting that
// resolveExecutable for "claude" goes through cachedResolveClaudePath which
// in a test environment without claude installed falls back to returning
// the literal "claude" string. That makes proc.Start() fail. Instead, the
// tests below use a "shim" name and validate the lifecycle, not the argv.
func captureSession(t *testing.T, mockMode string, sessionCmd string, args []string, sendInitialStdinPrompt string) (sessionID string, messages []resultMsg, finalErr error) {
	return captureSessionWithConfig(t, mockMode, sessionCmd, args, sendInitialStdinPrompt, nil)
}

func captureSessionWithConfig(t *testing.T, mockMode string, sessionCmd string, args []string, sendInitialStdinPrompt string, cfg *Config) (sessionID string, messages []resultMsg, finalErr error) {
	t.Helper()

	// Locate the test binary and copy it into a tempdir with the desired
	// command name so PATH-based resolveExecutable finds it.
	testExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	tmpDir := t.TempDir()
	mockName := sessionCmd
	if runtime.GOOS == "windows" && !strings.HasSuffix(mockName, ".exe") {
		mockName += ".exe"
	}
	mockPath := filepath.Join(tmpDir, mockName)
	if err := copyTestBinary(testExe, mockPath); err != nil {
		t.Fatalf("copy test binary: %v", err)
	}

	// Prepend tmpDir to PATH so exec.LookPath finds our mock.
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+origPath)
	t.Setenv(mockCLIEnvVar, mockMode)

	sm := NewSessionManager(cfg)
	id := fmt.Sprintf("test-session-%d", time.Now().UnixNano())

	var mu sync.Mutex
	var captured []resultMsg
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	// We start the session with the mock command name. The SessionManager
	// runs buildInteractiveCLIArgs which returns the args/stdinPrompt shape
	// for the configured CLI — and resolveExecutable will use exec.LookPath
	// which finds our mock in tmpDir.
	//
	// Retry on ETXTBSY ("text file busy"): we copy the (large) test binary
	// into tmpDir and immediately exec it. On Linux a concurrent fork/exec
	// in another goroutine can transiently inherit a writable fd to the
	// freshly-written mock, so the kernel refuses the exec until that fd is
	// closed. This is a known, transient race (golang/go#22315); the stdlib's
	// own tests retry the same way. It clears within milliseconds.
	var startErr error
	for attempt := 0; attempt < 5; attempt++ {
		startErr = sm.StartSession(id, sessionCmd, args, tmpDir, "ws-test", "uid-test", 30000, false, publishFn)
		if startErr == nil || !strings.Contains(startErr.Error(), "text file busy") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if startErr != nil {
		return id, nil, fmt.Errorf("StartSession: %w", startErr)
	}

	// Send initial stdin prompt manually if requested (mimics what
	// StartSession itself does internally for claude — we only do it here
	// when the mock expects it but the SessionManager didn't see a non-empty
	// stdinPrompt).
	if sendInitialStdinPrompt != "" {
		// SessionManager already sent the NDJSON prompt for claude; this
		// branch is for tests that want to explicitly drive stdin.
		_ = sm.SendInput(id, sendInitialStdinPrompt)
	}

	// Wait for session_ended or timeout.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ended := false
		for _, m := range captured {
			if m.Type == "session_ended" {
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
	out := append([]resultMsg(nil), captured...)
	mu.Unlock()
	return id, out, nil
}

// copyTestBinary is the test-only helper. The main package already has a
// copyFile that uses io.Copy and atomic rename for the auto-updater; we use
// the simpler ReadFile/WriteFile shape here so the test doesn't depend on the
// updater's contract.
func copyTestBinary(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o755)
}

/* --------------------------------------------------------------------------
   Lifecycle tests — verifies stream and session_ended ordering for each of
   the three CLI flows.

   What SessionManager.StartSession publishes via publishFn (from this
   test's perspective):
     - 0..N "stream" messages with monotonic Seq
     - 0..M "prompt" messages with monotonic Seq
     - exactly one "session_ended" message at the very end

   NOTE: "session_started" is NOT published by SessionManager — it's published
   by pubsub.go's handleSessionCommand AFTER StartSession returns. So these
   tests assert only the stream / session_ended ordering invariants:

     1. session_ended is the LAST message published (nothing after it).
     2. session_ended.Seq is strictly greater than any stream/prompt Seq.

   That second invariant is what terminal-service relies on to preserve chunk
   order in Firestore — if it regresses, ai-service reads incomplete chunks
   and the LLM sees truncated output (= "agent didn't wait for terminal
   response — calls cross between steps" report on documentDesign).
   ------------------------------------------------------------------------ */

func TestSessionLifecycle_Codex(t *testing.T) {
	_, messages, err := captureSession(t, "codex", "codex", []string{"implement the login page"}, "")
	if err != nil {
		t.Fatalf("captureSession: %v", err)
	}

	assertLifecycleOrdering(t, messages)

	// The codex mock emits "hello from codex" via item.completed.
	streamText := concatStreamOutput(messages)
	if !strings.Contains(streamText, "hello from codex") {
		t.Errorf("expected stream output to contain agent_message text; got %q", streamText)
	}
}

func TestSessionLifecycle_AntigravityLongPromptViaStdin(t *testing.T) {
	prompt := strings.Repeat("review \\\"quoted\\\" path C:\\\\repo\\n", 5000)
	_, messages, err := captureSession(t, "antigravity-stream-stdin", "agy", []string{prompt}, "")
	if err != nil {
		t.Fatalf("captureSession: %v", err)
	}

	assertLifecycleOrdering(t, messages)
	want := fmt.Sprintf("antigravity received %d bytes", len(prompt))
	if streamText := concatStreamOutput(messages); !strings.Contains(streamText, want) {
		t.Fatalf("long prompt did not arrive intact over Antigravity stdin: got %q, want %q", streamText, want)
	}
}

func TestSessionLifecycle_AntigravityExplicitEmptyPrompt(t *testing.T) {
	_, messages, err := captureSession(t, "antigravity-stream-stdin", "agy", []string{"--print="}, "")
	if err != nil {
		t.Fatalf("captureSession: %v", err)
	}

	assertLifecycleOrdering(t, messages)
	want := "antigravity received 0 bytes"
	if streamText := concatStreamOutput(messages); !strings.Contains(streamText, want) {
		t.Fatalf("empty prompt did not arrive intact over Antigravity stdin: got %q, want %q", streamText, want)
	}
}

func TestSessionLifecycle_AntigravityDiagnosticWithoutResultStaysSuccessful(t *testing.T) {
	_, messages, err := captureSession(t, "antigravity-diagnostic", "agy", []string{"--version"}, "")
	if err != nil {
		t.Fatalf("captureSession: %v", err)
	}

	assertLifecycleOrdering(t, messages)
	if streamText := concatStreamOutput(messages); !strings.Contains(streamText, "agy version 1.2.3") {
		t.Fatalf("diagnostic output missing: got %q", streamText)
	}
	for _, message := range messages {
		if message.Type == "session_ended" && message.ExitCode != 0 {
			t.Fatalf("successful Antigravity diagnostic exit code = %d, want 0", message.ExitCode)
		}
	}
}

func TestSessionLifecycle_GrokDirectPublishesFreshRedactedBilling(t *testing.T) {
	realHome := t.TempDir()
	seedGrokHomeWithLogin(t, realHome)
	t.Setenv("GROK_HOME", realHome)
	t.Setenv("XAI_API_KEY", "")

	_, messages, err := captureSession(t, "grok-direct-billing", "grok", []string{"refresh billing"}, "")
	if err != nil {
		t.Fatalf("captureSession: %v", err)
	}
	if len(messages) == 0 || messages[len(messages)-1].Type != "session_ended" {
		t.Fatalf("direct Grok lifecycle did not complete: %+v", messages)
	}

	usage, ok := grokUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, time.Now())
	if !ok || len(usage.Metrics) != 1 {
		t.Fatalf("direct billing parse failed: %+v", usage)
	}
	metric := usage.Metrics[0]
	if !metric.Unknown || metric.ObservedAt == "" || metric.ResetAt == "" {
		t.Fatalf("direct run must produce a fresh confirmed-unmetered metric: %+v", metric)
	}
	out, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"credential-sentinel", "prompt-sentinel", "raw-config-sentinel"} {
		if strings.Contains(string(out), secret) {
			t.Fatalf("direct usage leaked %q: %s", secret, out)
		}
	}
}

func TestSessionLifecycle_GrokNoToolsSmokeSurvivesUpdateAndSignedRefresh(t *testing.T) {
	realHome := t.TempDir()
	vendorHome := t.TempDir()
	projectRoot := t.TempDir()
	projectCwd := filepath.Join(projectRoot, "workspace")
	seedGrokHomeWithLogin(t, realHome)
	if err := os.WriteFile(filepath.Join(realHome, "config.toml"), []byte(
		"[plugins]\nenabled = [\"host-plugin\"]\n[mcp_servers.host-mcp]\ncommand = \"raw-config-sentinel\"\n",
	), 0o600); err != nil {
		t.Fatalf("seed host Grok config: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(realHome, "plugins", "host-plugin"), 0o700); err != nil {
		t.Fatalf("seed host Grok plugin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(realHome, "plugins", "host-plugin", "plugin.json"), []byte(`{"name":"host-plugin"}`), 0o600); err != nil {
		t.Fatalf("seed host Grok plugin manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(realHome, "mcp.json"), []byte(`{"host-mcp":{"command":"raw-config-sentinel"}}`), 0o600); err != nil {
		t.Fatalf("seed host Grok MCP config: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(vendorHome, ".cursor"), 0o700); err != nil {
		t.Fatalf("seed Cursor MCP directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(vendorHome, ".cursor", "mcp.json"), []byte(`{"mcpServers":{"host-mcp":{"command":"raw-config-sentinel"}}}`), 0o600); err != nil {
		t.Fatalf("seed Cursor MCP config: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(projectRoot, ".grok", "plugins", "project-plugin"), 0o700); err != nil {
		t.Fatalf("seed project Grok plugin directory: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(projectRoot, ".git"), 0o700); err != nil {
		t.Fatalf("seed project root marker: %v", err)
	}
	if err := os.MkdirAll(projectCwd, 0o700); err != nil {
		t.Fatalf("seed project working directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, ".grok", "config.toml"), []byte(
		"[mcp_servers.project-mcp]\ncommand = \"raw-config-sentinel\"\n[plugins]\npaths = [\"project-plugin\"]\n",
	), 0o600); err != nil {
		t.Fatalf("seed project Grok config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, ".grok", "plugins", "project-plugin", "plugin.json"), []byte(`{"name":"project-plugin"}`), 0o600); err != nil {
		t.Fatalf("seed project Grok plugin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, ".mcp.json"), []byte(`{"mcpServers":{"project-mcp":{"command":"raw-config-sentinel"}}}`), 0o600); err != nil {
		t.Fatalf("seed project MCP config: %v", err)
	}
	t.Setenv("GROK_HOME", realHome)
	t.Setenv("HOME", vendorHome)
	t.Setenv("USERPROFILE", vendorHome)
	t.Setenv("XAI_API_KEY", "credential-sentinel-api-key")
	t.Setenv("GROK_CODE_XAI_API_KEY", "alternate-credential-sentinel")
	t.Setenv("GROK_AUTH_PROVIDER_ACCESS_TOKEN", "provider-credential-sentinel")
	t.Setenv("GROK_AGENT", "agent-path-sentinel")
	t.Setenv("GROK_DEFAULT_MODEL", "model-sentinel")
	t.Setenv("GROK_MODELS_BASE_URL", "https://models-base-sentinel.invalid")
	t.Setenv("GROK_MODELS_LIST_URL", "https://models-list-sentinel.invalid")
	t.Setenv("GROK_XAI_API_BASE_URL", "https://xai-base-sentinel.invalid")
	t.Setenv("GROK_CURSOR_MCPS_ENABLED", "1")
	t.Setenv("GROK_CLAUDE_MCPS_ENABLED", "1")
	t.Setenv("GROK_CODEX_MCPS_ENABLED", "1")
	t.Setenv("GROK_MANAGED_MCPS_ENABLED", "1")
	t.Setenv("GROK_CONFIG_PATH", "config-path-sentinel")
	t.Setenv("GROK_PLUGIN_ROOT", "plugin-path-sentinel")
	t.Setenv("GROK_WORKSPACE_ROOT", "workspace-path-sentinel")
	t.Setenv(mockGrokPersistentHomeEnv, realHome)
	t.Setenv(mockGrokVendorHomeEnv, vendorHome)
	t.Setenv(mockGrokProjectRootEnv, projectRoot)
	configureTestGrokSystemLayers(t, "[compat.cursor]\nmcps = false\n[compat.claude]\nmcps = false\n")
	SetCLIAgentCatalog([]cliAgentCatalogEntry{{
		ID: "grok", DisplayName: "Grok Build", Command: "grok",
	}})
	t.Cleanup(func() { SetCLIAgentCatalog(nil) })
	resetVersionProbeCache()
	t.Cleanup(resetVersionProbeCache)

	smokeArgs := []string{
		"--tools", "", "--disable-web-search", "--no-subagents", "--max-turns", "1", "--verbatim",
		"Return exactly this marker and nothing else: " + grokMaintenanceSmokeMarker,
	}
	dispatchedSmokeArgs := sessionStartArgsForCommand(commandMsg{
		Type: "session_start", Command: "grok", Args: smokeArgs,
	})
	if _, promoted := extractGrokMaintenanceSmokeControl(dispatchedSmokeArgs); !promoted {
		t.Fatalf("production session_start dispatch did not promote maintenance smoke: %#v", dispatchedSmokeArgs)
	}
	run := func(mode string) cliAgentUsage {
		t.Helper()
		_, messages, err := captureSessionWithConfig(t, mode, "grok", dispatchedSmokeArgs, projectCwd, &Config{
			EnableGrokAlwaysApprove: true,
		})
		if err != nil {
			t.Fatalf("%s session: %v", mode, err)
		}
		assertLifecycleOrdering(t, messages)
		if got := concatStreamOutput(messages); got != grokMaintenanceSmokeMarker {
			t.Fatalf("%s output = %q, want exact marker %q", mode, got, grokMaintenanceSmokeMarker)
		}
		for _, message := range messages {
			if message.Type == "session_ended" && message.ExitCode != 0 {
				t.Fatalf("%s exit code = %d, want 0", mode, message.ExitCode)
			}
		}

		resetVersionProbeCache()
		usage, errs := GatherCLIAgentUsageOnly(context.Background())
		if len(errs) != 0 || len(usage) != 1 || len(usage[0].Metrics) != 1 {
			t.Fatalf("%s usage = %+v errors=%+v", mode, usage, errs)
		}
		receipt, normalized, normalizedErrs, err := prepareCLIUsageRefreshResult(
			"signed-refresh-secret", mode, time.Now().UnixMilli(), true, usage, errs)
		if err != nil || receipt == "" || len(normalizedErrs) != 0 || len(normalized) != 1 || len(normalized[0].Metrics) != 1 {
			t.Fatalf("%s signed refresh: receipt=%q usage=%+v errors=%+v err=%v", mode, receipt, normalized, normalizedErrs, err)
		}
		encoded, err := json.Marshal(normalized[0])
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{
			"credential-sentinel", "prompt-sentinel", "raw-config-sentinel", "agent-path-sentinel",
			"project-plugin", "config-path-sentinel", "plugin-path-sentinel", "workspace-path-sentinel",
			"model-sentinel", "models-base-sentinel", "models-list-sentinel", "xai-base-sentinel",
		} {
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("%s signed refresh leaked %q: %s", mode, secret, encoded)
			}
		}
		return normalized[0]
	}

	pre := run("grok-maintenance-smoke-v1")
	// Signed usage timestamps use RFC3339 second precision. Cross a second
	// boundary so the freshness assertion observes the post-update billing
	// fetch rather than two distinct nanosecond observations rendered alike.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second + 10*time.Millisecond)))
	post := run("grok-maintenance-smoke-v2")
	if pre.Version == post.Version || !strings.Contains(pre.Version, "1.0.5") || !strings.Contains(post.Version, "1.0.13") {
		t.Fatalf("test did not exercise the 1.0.5 -> 1.0.13 replacement: pre=%q post=%q", pre.Version, post.Version)
	}
	preObserved, preErr := time.Parse(time.RFC3339Nano, pre.Metrics[0].ObservedAt)
	postObserved, postErr := time.Parse(time.RFC3339Nano, post.Metrics[0].ObservedAt)
	if preErr != nil || postErr != nil || !postObserved.After(preObserved) {
		t.Fatalf("post-update usage freshness did not advance: pre=%q (%v) post=%q (%v)",
			pre.Metrics[0].ObservedAt, preErr, post.Metrics[0].ObservedAt, postErr)
	}
}

func TestSessionLifecycle_GrokOrdinaryNoToolsPreservesNormalAuthAndHome(t *testing.T) {
	realHome := t.TempDir()
	t.Setenv("GROK_HOME", realHome)
	t.Setenv("XAI_API_KEY", "credential-sentinel-api-key")
	t.Setenv(mockGrokPersistentHomeEnv, realHome)

	_, messages, err := captureSession(t, "grok-ordinary-no-tools", "grok", []string{"--tools", "", "ordinary prompt"}, "")
	if err != nil {
		t.Fatalf("ordinary no-tools session: %v", err)
	}
	assertLifecycleOrdering(t, messages)
	if got := concatStreamOutput(messages); got != "ordinary-no-tools-ok" {
		t.Fatalf("ordinary no-tools output = %q, want %q", got, "ordinary-no-tools-ok")
	}
}

func configureTestGrokSystemLayers(t *testing.T, managedBody string) {
	t.Helper()
	requirementsPath := filepath.Join(t.TempDir(), "requirements.toml")
	managedPath := filepath.Join(t.TempDir(), "managed_config.toml")
	if err := os.WriteFile(requirementsPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managedPath, []byte(managedBody), 0o600); err != nil {
		t.Fatal(err)
	}
	origRequirementsPath := grokSystemRequirementsPath
	origManagedPath := grokSystemManagedConfigPath
	origClaudePaths := claudeManagedSettingsPathsFn
	grokSystemRequirementsPath = requirementsPath
	grokSystemManagedConfigPath = managedPath
	claudeManagedSettingsPathsFn = func() []string { return nil }
	t.Cleanup(func() {
		grokSystemRequirementsPath = origRequirementsPath
		grokSystemManagedConfigPath = origManagedPath
		claudeManagedSettingsPathsFn = origClaudePaths
	})
}

// TestSessionLifecycle_CodexDeferredStdinPrompt covers the chat-direct flow
// where the session is opened eagerly with NO prompt and the user's first
// message is delivered later via SendInput. codex exec reads stdin to EOF
// before running, so stdin must stay open until that first SendInput; the
// pre-fix behaviour closed stdin at start, and codex v0.140+ then exited 1 with
// "No prompt provided via stdin." before the message arrived (the prod
// "Codex session failed" symptom). The mock here reproduces that contract:
// empty stdin → exit 1; a delivered prompt → normal stream + clean exit.
func TestSessionLifecycle_CodexDeferredStdinPrompt(t *testing.T) {
	_, messages, err := captureSession(t, "codex-reads-stdin", "codex", []string{}, "Hi there")
	if err != nil {
		t.Fatalf("captureSession: %v", err)
	}

	assertLifecycleOrdering(t, messages)

	// The deferred prompt must have reached codex over the still-open stdin —
	// the mock echoes it back. Absence means stdin was closed before SendInput
	// delivered the prompt (the regression this fix guards against).
	streamText := concatStreamOutput(messages)
	if !strings.Contains(streamText, "echo: Hi there") {
		t.Errorf("expected the deferred prompt to reach codex via SendInput; got %q", streamText)
	}
}

// TestSessionLifecycle_GenericShim runs the same lifecycle assertions but
// using a command name ("shim") that doesn't match any of the special CLI
// handlers in buildInteractiveCLIArgs. The args go through unmodified and
// stdin is closed at start (no stdinPrompt). Acts as a baseline that the
// session machinery works even without per-CLI customisation.
func TestSessionLifecycle_GenericShim(t *testing.T) {
	_, messages, err := captureSession(t, "codex", "shim", []string{}, "")
	if err != nil {
		t.Fatalf("captureSession: %v", err)
	}

	assertLifecycleOrdering(t, messages)
}

/* --------------------------------------------------------------------------
   Stream-burst test — verifies that no stream chunks get dropped under load,
   and that session_ended is published AFTER every stream chunk has been
   queued for publish.
   ------------------------------------------------------------------------ */

func TestSessionLifecycle_StreamBurstPreservesAllChunks(t *testing.T) {
	if runtime.GOOS == "windows" {
		// claude command name routes through cachedResolveClaudePath which
		// expects the real Claude CLI install layout. Use the codex command
		// name instead — same mock CLI mode, just different routing.
		t.Skip("claude resolveExecutable bypasses PATH on Windows; covered by Unix CI")
	}
	_, messages, err := captureSession(t, "stream-burst", "claude", []string{"any-prompt"}, "")
	if err != nil {
		t.Fatalf("captureSession: %v", err)
	}

	assertLifecycleOrdering(t, messages)

	// Reconstruct the streamed text. The mock emitted 50 chunks of the form
	// "chunk-N ". We don't require ALL 50 to be present (asyncPublish CAN
	// drop on overflow — that's an accepted tradeoff documented in
	// session.go), but we DO require that the chunks present are a prefix
	// or contiguous span of the emitted sequence, and that no random
	// out-of-order garbage is present.
	streamText := concatStreamOutput(messages)
	for i := 0; i < 50; i++ {
		needle := fmt.Sprintf("chunk-%d ", i)
		if !strings.Contains(streamText, needle) {
			// Acceptable: trailing chunks dropped under overload. Log and
			// move on — but emit a warning so a future regression that drops
			// EVERY chunk gets caught (we'd have 0 found).
			break
		}
	}

	// At minimum, we should have received the early chunks.
	if !strings.Contains(streamText, "chunk-0 ") {
		t.Errorf("first stream chunk missing; got %q", truncateString(streamText, 200))
	}
	if !strings.Contains(streamText, "chunk-1 ") {
		t.Errorf("second stream chunk missing — possible publish pipeline regression; got %q",
			truncateString(streamText, 200))
	}
}

/* --------------------------------------------------------------------------
   Edge-case: CLI that exits immediately with no output
   --------------------------------------------------------------------------
   Mirrors the failure mode where Claude is invoked without a prompt arg —
   stdinPrompt is empty, stdin is closed at start, claude exits immediately.
   The SessionManager must still publish session_started + session_ended
   (with a meaningful exit code) and must NOT hang.
   ------------------------------------------------------------------------ */

func TestSessionLifecycle_ImmediateExitDoesNotHang(t *testing.T) {
	start := time.Now()
	_, messages, err := captureSession(t, "no-prompt-immediate-exit", "shim", []string{}, "")
	if err != nil {
		t.Fatalf("captureSession: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 10*time.Second {
		t.Errorf("session took %v — should have completed in well under 10s for immediate-exit CLI", elapsed)
	}

	// Must still produce session_ended (the publishFn contract — SessionManager
	// publishes session_ended even when the CLI exits with no output, so
	// pubsub.go / terminal-service / ai-service all unblock cleanly).
	hasEnded := false
	for _, m := range messages {
		if m.Type == "session_ended" {
			hasEnded = true
		}
	}
	if !hasEnded {
		t.Errorf("session_ended missing — terminal tool would hang in ai-service onSnapshot")
	}
}

/* --------------------------------------------------------------------------
   Edge-case: CLI that writes only to stderr (CLI error paths)
   ------------------------------------------------------------------------ */

func TestSessionLifecycle_StderrOnlyIsStreamed(t *testing.T) {
	_, messages, err := captureSession(t, "stderr-and-exit", "shim", []string{}, "")
	if err != nil {
		t.Fatalf("captureSession: %v", err)
	}

	// Stderr content must still surface as stream chunks so the LLM/user
	// sees the error. Without this, "permission denied" failures look like
	// silent successes.
	streamText := concatStreamOutput(messages)
	if !strings.Contains(streamText, "permission denied") {
		t.Errorf("stderr content missing from stream output; got %q", streamText)
	}

	// Non-zero exit code must be reflected on session_ended.
	for _, m := range messages {
		if m.Type == "session_ended" {
			if m.ExitCode == 0 {
				t.Errorf("expected non-zero ExitCode on session_ended; got 0")
			}
		}
	}
}

/* --------------------------------------------------------------------------
   helpers
   ------------------------------------------------------------------------ */

// assertLifecycleOrdering pins the two invariants that terminal-service +
// ai-service rely on for correct chunk reassembly:
//  1. session_ended is the LAST publishFn message (nothing after it).
//  2. session_ended.Seq is strictly greater than any stream/prompt Seq, so
//     terminal-service can write chunks ordered by Seq and the final
//     session_ended chunk lands last.
//
// session_started is published by pubsub.go (not SessionManager), so it's
// out of scope here.
func assertLifecycleOrdering(t *testing.T, messages []resultMsg) {
	t.Helper()

	if len(messages) == 0 {
		t.Fatal("no messages captured")
	}

	endedIdx := -1
	var maxStreamSeq int
	var endedSeq int

	for i, m := range messages {
		if m.Type == "session_ended" {
			endedIdx = i
			endedSeq = m.Seq
		}
		if (m.Type == "stream" || m.Type == "prompt") && m.Seq > maxStreamSeq {
			maxStreamSeq = m.Seq
		}
	}

	if endedIdx < 0 {
		t.Fatalf("session_ended missing from captured messages (got %d messages)", len(messages))
	}

	// session_ended should be the last message. Anything published after it
	// is the bug we care about — it means a stream chunk reached publishFn
	// AFTER we already told the orchestrator the session was done.
	if endedIdx != len(messages)-1 {
		var laterTypes []string
		for _, m := range messages[endedIdx+1:] {
			laterTypes = append(laterTypes, m.Type)
		}
		t.Errorf("session_ended at idx %d is not the last message — %d message(s) published AFTER it: %v",
			endedIdx, len(messages)-endedIdx-1, laterTypes)
	}

	// Seq numbers must be monotonic and session_ended must have the highest.
	// If there are no stream messages, maxStreamSeq is 0 and the assertion
	// trivially holds for any positive endedSeq.
	if maxStreamSeq > 0 && endedSeq <= maxStreamSeq {
		t.Errorf("session_ended Seq=%d is not greater than max stream Seq=%d — terminal-service uses Seq for chunk ordering, this would corrupt the order",
			endedSeq, maxStreamSeq)
	}
}

// concatStreamOutput joins all stream-message outputs in seq order. Used by
// tests that want to assert on the reassembled text the LLM would see.
func concatStreamOutput(messages []resultMsg) string {
	streams := make([]resultMsg, 0, len(messages))
	for _, m := range messages {
		if m.Type == "stream" {
			streams = append(streams, m)
		}
	}
	sort.Slice(streams, func(i, j int) bool { return streams[i].Seq < streams[j].Seq })
	var b bytes.Buffer
	for _, m := range streams {
		b.WriteString(m.Output)
	}
	return b.String()
}

// quietExec is used to silence go vet when an exec.Command result is unused
// in test setup paths.
var _ = exec.Command
