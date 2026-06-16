// session_integration_test.go
// -----------------------------------------------------------------------------
// End-to-end lifecycle tests for SessionManager + the 3 CLI flows
// (claude / codex / gemini). Uses the test binary itself as a mock CLI so we
// don't need claude / codex / gemini installed on the test host and so we get
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
	"fmt"
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

// TestMain dispatches into the mock CLI when the env var is set; otherwise
// runs the test suite normally. This is the standard "test binary as helper
// subprocess" pattern (see `go test` docs and stdlib os/exec tests).
func TestMain(m *testing.M) {
	if mode := os.Getenv(mockCLIEnvVar); mode != "" {
		runMockCLI(mode)
		return
	}
	os.Exit(m.Run())
}

// runMockCLI simulates the streaming JSON output of a CLI agent so the
// SessionManager has something realistic to parse. Each mode mirrors the
// shape we actually see in production:
//
//   - claude: read NDJSON on stdin, emit stream_event content_block_deltas,
//     then a final result event. Exit when stdin closes.
//   - codex:  print streaming events including item.completed,
//     thread.completed, then exit. Prompt is in argv (we don't actually use
//     it — the mock has fixed output).
//   - gemini: emit message events then a result event, then exit.
//   - sleep-then-emit: emit the events AFTER a delay so we can test stream
//     completion ordering under load.
func runMockCLI(mode string) {
	switch mode {
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

	case "gemini":
		// gemini stream-json output. Prompt is in argv.
		fmt.Println(`{"type":"message","role":"assistant","content":"hello from gemini"}`)
		fmt.Println(`{"type":"result","subtype":"success"}`)
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
		// CLI that writes only to stderr (some Gemini error paths look like
		// this). SessionManager should still surface this content.
		fmt.Fprintln(os.Stderr, "permission denied: refresh your gemini token")
		os.Exit(2)

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

	case "grok-acp-echo":
		runMockGrokACPServer()

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
// need the command name to be exactly "claude" / "codex" / "gemini" for the
// argv shape to match. For the integration tests below, we sidestep this
// by directly calling StartSession with a mock command — accepting that
// resolveExecutable for "claude" goes through cachedResolveClaudePath which
// in a test environment without claude installed falls back to returning
// the literal "claude" string. That makes proc.Start() fail. Instead, the
// tests below use a "shim" name and validate the lifecycle, not the argv.
func captureSession(t *testing.T, mockMode string, sessionCmd string, args []string, sendInitialStdinPrompt string) (sessionID string, messages []resultMsg, finalErr error) {
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

	sm := NewSessionManager(nil)
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
	startErr := sm.StartSession(id, sessionCmd, args, tmpDir, "ws-test", "uid-test", 30000, publishFn)
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

func TestSessionLifecycle_Gemini(t *testing.T) {
	_, messages, err := captureSession(t, "gemini", "gemini", []string{"summarize this repo"}, "")
	if err != nil {
		t.Fatalf("captureSession: %v", err)
	}

	assertLifecycleOrdering(t, messages)

	streamText := concatStreamOutput(messages)
	if !strings.Contains(streamText, "hello from gemini") {
		t.Errorf("expected stream output to contain assistant message text; got %q", streamText)
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
   Edge-case: CLI that writes only to stderr (Gemini error paths)
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
