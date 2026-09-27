package main

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseMuseCodeEventLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		ok   bool
		want oneShotEvent
	}{
		{"delta", museFrameDeltaHello, true, oneShotEvent{Coalesce: true, TextDelta: "Hello", SessionID: "0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11"}},
		{"delta field variant", `{"payload_type":"run.output.delta","payload":{"delta":"Hi"}}`, true, oneShotEvent{Coalesce: true, TextDelta: "Hi"}},
		{"delta keeps a lone space", `{"payload_type":"run.output.delta","payload":{"delta":" "}}`, true, oneShotEvent{Coalesce: true, TextDelta: " "}},
		{"delta text fallback", `{"payload_type":"run.output.delta","payload":{"text":"hi"}}`, true, oneShotEvent{Coalesce: true, TextDelta: "hi"}},
		{"completed", museFrameCompleted, true, oneShotEvent{FinalText: "Hello world", SessionID: "0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11"}},
		{"failed with status", museFrameFailed402, true, oneShotEvent{
			Failure: "[Muse Code turn failed: billing_not_configured (status 402): Billing is not configured for this account.]"}},
		{"failed without status", `{"payload_type":"run.terminal.failed","payload":{"error_kind":"max_model_steps","reason":"step budget exhausted"}}`, true,
			oneShotEvent{Failure: "[Muse Code turn failed: max_model_steps: step budget exhausted]"}},
		{"cancelled", `{"payload_type":"run.terminal.cancelled","payload":{}}`, true,
			oneShotEvent{Failure: "[Muse Code turn failed: cancelled: the run was cancelled before it completed]"}},
		{"task activity contributes no text", museFrameTool, true, oneShotEvent{SessionID: "0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11"}},
		{"the echoed prompt is internal", museFrameUserInput, true, oneShotEvent{SessionID: "0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11", Internal: true}},
		{"task scheduling chatter is internal", museFrameScheduled, true, oneShotEvent{SessionID: "0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11", Internal: true}},
		{"a run stream id is not a session id", `{"payload_type":"run.lifecycle.started","stream":{"kind":"run","id":"14e6347f-3772-4c4b-a46d-a1284380b941"},"payload":{}}`, true, oneShotEvent{Internal: true}},
		{"session id is read", `{"payload_type":"run.started","payload":{"session_id":"0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11"}}`, true,
			oneShotEvent{SessionID: "0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11"}},
		{"banner line", `Muse Code 1.4.0`, false, oneShotEvent{}},
		{"malformed json is skipped", `{"payload_type":`, false, oneShotEvent{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseMuseCodeEventLine(tc.line)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("got (%#v, %v), want (%#v, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestFormatMuseCodeFailureCapsTheReason(t *testing.T) {
	got := formatMuseCodeFailure("server_error", 503, strings.Repeat("r", 1000))
	if !strings.HasPrefix(got, "[Muse Code turn failed: server_error (status 503): ") || len(got) > 480 {
		t.Fatalf("unexpected frame (%d bytes): %.80s", len(got), got)
	}
	if got := formatMuseCodeFailure("error", 0, "  "); got != "[Muse Code turn failed: error: no reason reported]" {
		t.Fatalf("got %q", got)
	}
}

func TestBuildMuseCodeNativeArgs(t *testing.T) {
	if got := strings.Join(buildMuseCodeNativeArgs("", ""), " "); got != "exec --json --disable-approval" {
		t.Fatalf("gate shape: %q", got)
	}
	got := strings.Join(buildMuseCodeNativeArgs("id-1", "/tmp/p.txt"), " ")
	if got != "exec --json --disable-approval --session-id id-1 --prompt-file /tmp/p.txt" {
		t.Fatalf("turn shape: %q", got)
	}
}

func TestBuildMuseCodeInteractiveArgs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"bare prompt is forced headless", []string{"fix the bug"},
			"exec --json --disable-approval -- fix the bug"},
		{"model and effort are forwarded with values", []string{"--model", "muse-spark-1.3", "--reasoning-effort", "high", "go"},
			"exec --json --disable-approval --model muse-spark-1.3 --reasoning-effort high -- go"},
		{"output, session and sandbox controls are stripped", []string{
			"exec", "--json", "--yolo", "--disable-sandbox", "--session-id", "abc", "--resume",
			"--prompt-file", "/etc/passwd", "--approval-mode=never", "--allow-workspace-switch",
			"--base-url", "https://evil.example", "--provider=echo", "--api-key-stdin", "--trust-workspace", "do it"},
			"exec --json --disable-approval -- do it"},
		{"a prompt that starts with a dash stays a prompt", []string{"--", "-rf is dangerous"},
			"exec --json --disable-approval -- -rf is dangerous"},
		{"no prompt", []string{"--model", "m"}, "exec --json --disable-approval --model m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Join(buildMuseCodeInteractiveArgs(tc.in), " "); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	for _, diag := range [][]string{{"--version"}, {"login"}, {"auth", "status"}, {"exec", "--help"}} {
		if got := buildMuseCodeInteractiveArgs(diag); strings.Join(got, " ") != strings.Join(diag, " ") {
			t.Fatalf("diagnostic %v must pass through verbatim, got %v", diag, got)
		}
	}
	if !isMuseCodeSynthesizedRun(buildMuseCodeInteractiveArgs([]string{"x"})) {
		t.Fatal("the synthesised shape must be recognized by the gate")
	}
}

func TestIsMuseCodeCommand(t *testing.T) {
	for _, c := range []string{"muse", "/home/u/.local/bin/muse", `C:\Users\u\AppData\Local\Programs\muse\muse.exe`} {
		if !isMuseCodeCommand(c) {
			t.Errorf("%q should route to Muse Code", c)
		}
	}
	for _, c := range []string{"museum", "musescore", "opencode", ""} {
		if isMuseCodeCommand(c) {
			t.Errorf("%q must not route to Muse Code", c)
		}
	}
	if !isResidentAgentSessionCommand("muse") {
		t.Fatal("muse must be classified as a resident agent so it is never handed TUI argv")
	}
	if !isMuseCodeNativeCommand("musecode_native_send") || isMuseCodeNativeCommand("opencode_native_send") {
		t.Fatal("native command family mismatch")
	}
	if rejectionResultType("musecode_native_start") != "musecode_native_error" {
		t.Fatal("a rejected Muse Code command must answer with musecode_native_error")
	}
}

func TestMuseCodeLegacyPathStdinTerminalEventAndEnv(t *testing.T) {
	if !shouldCloseStdinAfterStart("muse", nil) {
		t.Fatal("muse exec holds no stdin protocol; stdin must close after start")
	}
	if !detectCLITerminalEvent("muse", museFrameCompleted) ||
		!detectCLITerminalEvent("muse", museFrameFailed402) ||
		detectCLITerminalEvent("muse", museFrameDeltaHello) {
		t.Fatal("run.terminal.* marks the end of a muse turn; deltas do not")
	}
	env, stripped := sanitizeClaudeChildEnv("muse", []string{"OPENAI_API_KEY=x", "META_API_KEY=keep", "ANTHROPIC_API_KEY=x"})
	if strings.Join(env, ",") != "META_API_KEY=keep" || len(stripped) != 2 {
		t.Fatalf("got env=%v stripped=%v", env, stripped)
	}
	cliArgs, prompt := buildInteractiveCLIArgs("muse", []string{"hello"}, false)
	if prompt != nil || strings.Join(cliArgs, " ") != "exec --json --disable-approval -- hello" {
		t.Fatalf("got %v %v", cliArgs, prompt)
	}
}

func TestMuseCodeConversationResumeSeedAndCapture(t *testing.T) {
	id := "0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11"
	shaped := buildMuseCodeInteractiveArgs([]string{"--model", "m", "continue"})
	seeded, err := applyCliResumeSeed("muse", []string{"--model", "m", "continue"}, shaped, id, false)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := strings.Join(seeded, " "); got != "exec --json --disable-approval --session-id "+id+" --model m -- continue" {
		t.Fatalf("got %q", got)
	}
	if _, err := applyCliResumeSeed("muse", []string{"x"}, shaped, "ses_notauuid123", false); err == nil {
		t.Fatal("a non-UUID id must be refused for muse")
	}
	if _, err := applyCliResumeSeed("muse", []string{"--version"}, []string{"--version"}, id, false); err == nil {
		t.Fatal("a diagnostic invocation cannot be resumed")
	}
	line := `{"payload_type":"run.started","payload":{"session_id":"` + id + `"}}`
	if got := extractCliConversationID("muse", line); got != id {
		t.Fatalf("capture: got %q", got)
	}
	if got := extractCliConversationID("muse", `{"payload_type":"run.started","payload":{"session_id":"-rf"}}`); got != "" {
		t.Fatalf("an unsafe id must never be captured, got %q", got)
	}
}

func TestMuseCodeInstallerBinDir(t *testing.T) {
	override := t.TempDir()
	t.Setenv("MUSE_INSTALL_DIR", override)
	if got := installerBinDirFor("muse"); got != override {
		t.Fatalf("MUSE_INSTALL_DIR must win, got %q", got)
	}
	t.Setenv("MUSE_INSTALL_DIR", "")
	got := installerBinDirFor("muse")
	if runtime.GOOS == "windows" {
		if !strings.HasSuffix(got, filepath.Join("Programs", "muse")) {
			t.Fatalf("windows installer dir: %q", got)
		}
	} else if !strings.HasSuffix(got, filepath.Join(".local", "bin")) {
		t.Fatalf("unix installer dir: %q", got)
	}
}

func TestMuseCodeBuiltInCatalogRow(t *testing.T) {
	for _, entry := range defaultCLIAgentCatalog() {
		if entry.ID != "museCode" {
			continue
		}
		if entry.Command != "muse" || entry.DisplayOrder != 70 || cliAgentCatalogParserKey(entry) != "museCode" {
			t.Fatalf("unexpected built-in row: %#v", entry)
		}
		if !isAllowedSetupToolProbe("muse", []string{"--version"}) {
			t.Fatal("setup must be able to probe muse --version")
		}
		return
	}
	t.Fatal("museCode missing from the built-in catalog")
}

func TestMuseCodeCredentialIsRedactedFromPublishedFrames(t *testing.T) {
	got := redactAgentSecrets("auth failed: META_API_KEY=mk-live-0123456789abcdef")
	if strings.Contains(got, "0123456789abcdef") {
		t.Fatalf("META_API_KEY value leaked: %q", got)
	}
}

func TestSplitRedactionCarry(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		wantEmit  string
		wantCarry string
	}{
		// The carry starts where redactAgentSecrets' own `api[_-]?key` pattern
		// would match, so the held tail is exactly what the mask needs.
		{"a dangling credential key waits for its value", "tool output: META_API_KEY=", "tool output: META_", "API_KEY="},
		{"a partial value is held with its key", "echo api_key: mk-live", "echo ", "api_key: mk-live"},
		{"a long opaque tail could still grow into a blob", "token is " + strings.Repeat("a", 20), "token is ", strings.Repeat("a", 20)},
		{"ordinary prose streams straight through", "the plan is ready now", "the plan is ready now", ""},
		{"a short trailing word is not held back", "thinking", "thinking", ""},
		// A window that holds nothing BUT the ambiguous tail is exactly what the
		// carry exists for: publishing the label alone strands its value in a
		// frame the per-frame pass can no longer match.
		{"a whole-buffer dangling key is carried, not published", "api_key=", "", "api_key="},
		{"a whole-buffer opaque run is carried", strings.Repeat("a", 20), "", strings.Repeat("a", 20)},
		{"a whole-buffer tail at the bound publishes rather than stalling", strings.Repeat("x", agentSecretCarryMaxBytes), strings.Repeat("x", agentSecretCarryMaxBytes), ""},
		{"an oversize run is not buffered", "x " + strings.Repeat("y", agentSecretCarryMaxBytes+1), "x " + strings.Repeat("y", agentSecretCarryMaxBytes+1), ""},
		{"empty text", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			emit, carry := splitRedactionCarry(tc.text)
			if emit != tc.wantEmit || carry != tc.wantCarry {
				t.Fatalf("got emit=%q carry=%q, want emit=%q carry=%q", emit, carry, tc.wantEmit, tc.wantCarry)
			}
			if emit+carry != tc.text {
				t.Fatalf("the split must be lossless: %q + %q != %q", emit, carry, tc.text)
			}
		})
	}
}

// A credential pair split across two coalescer flushes must not publish the
// value verbatim: the later masked completion frame cannot retract a leaked
// stream frame.
func TestDeltaCoalescer_CredentialSplitAcrossFlushesIsNotPublishedRaw(t *testing.T) {
	var (
		mu  sync.Mutex
		out []string
	)
	c := newDeltaCoalescer(
		func(text string) string { return text },
		func(line string) { mu.Lock(); out = append(out, redactAgentSecrets(line)); mu.Unlock() },
	)
	c.add("running tool: META_API_KEY=")
	// The 250ms timer fires with only the key buffered.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(out)
		mu.Unlock()
		if n > 0 || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.add("mk-live-0123456789abcdef\n")
	c.close()

	mu.Lock()
	joined := strings.Join(out, "")
	mu.Unlock()
	if strings.Contains(joined, "0123456789abcdef") {
		t.Fatalf("the split credential value leaked: %q", joined)
	}
	if !strings.Contains(joined, "running tool: ") {
		t.Fatalf("the safe prefix must still stream: %q", joined)
	}
}

func TestMaskOpaqueContinuation(t *testing.T) {
	prefix, rest := strings.Repeat("a", 10), strings.Repeat("b", 75)
	cases := []struct {
		name     string
		text     string
		prior    int
		wantText string
		wantRun  int
	}{
		{"a continuation that completes a blob is masked", rest + " done", 10, "[REDACTED] done", 4},
		{"a whole-text continuation is masked and the run keeps growing", rest, 10, "[REDACTED]", 85},
		{"a short continuation of a short run streams", "ing now", 5, "ing now", 3},
		{"no prior run leaves the text alone", rest + " x", 0, rest + " x", 1},
		{"a leading separator breaks the run", " " + rest, 10, " " + rest, 75},
		{"a whole-text run below the shape accumulates", prefix, 10, prefix, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, run := maskOpaqueContinuation(tc.text, tc.prior)
			if got != tc.wantText || run != tc.wantRun {
				t.Fatalf("got (%q, %d), want (%q, %d)", got, run, tc.wantText, tc.wantRun)
			}
		})
	}
}

// An 85-char opaque token split with fewer than 16 chars in the first timer
// window: the short prefix is below the carry floor and publishes, the rest is
// carried to the final flush. Neither half matches the 80-char blob pattern on
// its own, so the continuation must be masked by run length.
func TestDeltaCoalescer_ShortOpaquePrefixDoesNotLeakTheRest(t *testing.T) {
	var (
		mu  sync.Mutex
		out []string
	)
	c := newDeltaCoalescer(
		func(text string) string { return text },
		func(line string) { mu.Lock(); out = append(out, redactAgentSecrets(line)); mu.Unlock() },
	)
	prefix, rest := "tok0123456", strings.Repeat("Z", 75)
	c.add("value " + prefix)
	time.Sleep(750 * time.Millisecond)
	c.add(rest)
	c.close()

	mu.Lock()
	joined := strings.Join(out, "")
	mu.Unlock()
	if strings.Contains(joined, rest[:40]) {
		t.Fatalf("the carried continuation leaked: %q", joined)
	}
	if !strings.Contains(joined, "[REDACTED]") {
		t.Fatalf("the continuation must be masked: %q", joined)
	}
}

// A timer window that holds NOTHING but the credential label must not publish
// it: the value arrives in a later frame that, on its own, matches no
// per-frame pattern, so the pair would reach the cloud complete across two
// frames that the masked completion frame cannot retract.
func TestDeltaCoalescer_WholeBufferCredentialLabelIsCarried(t *testing.T) {
	var (
		mu  sync.Mutex
		out []string
	)
	c := newDeltaCoalescer(
		func(text string) string { return text },
		func(line string) { mu.Lock(); out = append(out, redactAgentSecrets(line)); mu.Unlock() },
	)
	c.add("api_key=")
	// Give the 250ms timer time to fire on a buffer that is only the label.
	time.Sleep(750 * time.Millisecond)
	mu.Lock()
	flushed := len(out)
	mu.Unlock()
	if flushed != 0 {
		t.Fatalf("the label must stay buffered, got %d frame(s): %v", flushed, out)
	}
	c.add("mk-live-0123456789abcdef\n")
	c.close()

	mu.Lock()
	joined := strings.Join(out, "")
	mu.Unlock()
	if strings.Contains(joined, "0123456789abcdef") {
		t.Fatalf("the split credential value leaked: %q", joined)
	}
	if !strings.Contains(joined, "[REDACTED]") {
		t.Fatalf("the rejoined pair must be masked: %q", joined)
	}
}

// A publishable lifecycle/tool frame between the two halves of a credential
// must not release the carry. If it did, the label would stream verbatim, the
// event would follow, and the value's own short delta would match neither the
// key/value nor the opaque-token pattern — so the pair would reach the cloud
// complete across frames the masked completion frame cannot retract.
func TestDeltaCoalescer_CarrySurvivesAnInterveningNonDeltaFrame(t *testing.T) {
	var (
		mu  sync.Mutex
		out []string
	)
	c := newDeltaCoalescer(
		func(text string) string { return "text:" + text },
		func(line string) { mu.Lock(); out = append(out, redactAgentSecrets(line)); mu.Unlock() },
	)
	c.add("running tool: META_API_KEY=")
	// The non-delta frame arrives between the label and its value.
	c.publish(`{"type":"tool.start","name":"Bash"}`)
	c.add("mk-live-0123456789abcdef\n")
	c.close()

	mu.Lock()
	frames := append([]string(nil), out...)
	mu.Unlock()
	joined := strings.Join(frames, "")
	if strings.Contains(joined, "0123456789abcdef") {
		t.Fatalf("the credential value leaked across the intervening frame: %q", frames)
	}
	if !strings.Contains(joined, "[REDACTED]") {
		t.Fatalf("the rejoined pair must be masked: %q", frames)
	}
	// The intervening frame is still emitted, and still ahead of the deferred
	// tail — only the held fragment moves, never a lifecycle frame.
	eventIdx, tailIdx := -1, -1
	for i, f := range frames {
		if strings.Contains(f, "tool.start") {
			eventIdx = i
		}
		if strings.Contains(f, "[REDACTED]") {
			tailIdx = i
		}
	}
	if eventIdx < 0 {
		t.Fatalf("the non-delta frame must still be published: %q", frames)
	}
	if tailIdx < eventIdx {
		t.Fatalf("the carry must flush after the frame it was held across: %q", frames)
	}
	if !strings.Contains(joined, "running tool: ") {
		t.Fatalf("the safe prefix must still stream: %q", frames)
	}
}

func TestMuseCodeProbeVersion_PrefersTheLauncherVersionFile(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "muse.cmd")
	if err := os.WriteFile(shim, []byte("@echo off\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, museCodeVersionFile), []byte("1.4.0-R4302.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No child is spawned: the shim would not even run as a probe.
	if got := museCodeProbeVersion(shim); got != "1.4.0-R4302.1" {
		t.Fatalf("got %q", got)
	}
	if got := parseCLIVersionTriple(museCodeProbeVersion(shim)); got != "1.4.0" {
		t.Fatalf("the capability probe must parse it, got %q", got)
	}
	for name, content := range map[string]string{
		"garbage":       "not a version",
		"multi-token":   "1.4.0 extra",
		"oversize junk": strings.Repeat("9", 200),
	} {
		if err := os.WriteFile(filepath.Join(dir, museCodeVersionFile), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := readMuseCodeVersionFile(shim); got != "" {
			t.Errorf("%s: got %q, want \"\"", name, got)
		}
	}
	if got := readMuseCodeVersionFile("muse"); got != "" {
		t.Fatalf("a relative path must never be resolved, got %q", got)
	}
}

func TestOneShotNative_CapabilityProbeUsesTheVersionFile(t *testing.T) {
	m := installMuseCodeStub(t) // stub `--version` prints 1.4.0
	exe := resolveMuseCodeExecutable()
	if err := os.WriteFile(filepath.Join(filepath.Dir(exe), museCodeVersionFile), []byte("1.1.0-R1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.probeCapability(); err != nil {
		t.Fatal(err)
	}
	if m.supportsNativeResume() {
		t.Fatal("the launcher's recorded version (below the floor) must win over a spawned probe")
	}
}

// The generic session_start path (what iOS drives, and the takeover route)
// renders `muse exec --json` records as the assistant's text instead of raw
// JSON: deltas join as one text, the terminal text is used only when nothing
// streamed, failures use the turn-failed wrapper, and bookkeeping is silent.
func TestReadOutputStream_MuseCodeRendersAssistantText(t *testing.T) {
	render := func(lines ...string) string {
		session := &CLISession{
			ID:             "muse-legacy",
			Command:        "muse",
			Stdout:         io.NopCloser(strings.NewReader(strings.Join(lines, "\n") + "\n")),
			Stderr:         io.NopCloser(strings.NewReader("")),
			streamDone:     make(chan struct{}),
			firstRealFrame: make(chan struct{}),
		}
		var out strings.Builder
		NewSessionManager(nil).readOutputStream(session, func(msg resultMsg) {
			if msg.Type == "stream" {
				out.WriteString(msg.Output)
			}
		})
		return out.String()
	}

	if got := render(museFrameUserInput, museFrameDeltaHello, museFrameDeltaWorld, museFrameTool, museFrameScheduled, museFrameCompleted); got != "Hello world" {
		t.Fatalf("streamed turn rendered %q", got)
	}
	if got := render(museFrameCompleted); got != "Hello world" {
		t.Fatalf("a turn with no deltas must render its terminal text, got %q", got)
	}
	got := render(museFrameDeltaHello, museFrameFailed402)
	if !strings.Contains(got, "Hello") || !strings.Contains(got, "[Muse Code turn failed: billing_not_configured (status 402):") {
		t.Fatalf("failed turn rendered %q", got)
	}
	if got := render("Muse Code: update available"); !strings.Contains(got, "update available") {
		t.Fatalf("a plain line must pass through, got %q", got)
	}
}

// StartSession keeps every synthesized Muse prompt off argv (process listings
// expose it), and the Windows `.cmd` shim launch routes argv through cmd.exe,
// whose single expansion pass cannot carry an operand holding a quote or a line
// break — so the legacy positional prompt moves into a --prompt-file first.
func TestRewriteMuseCodePromptToFile(t *testing.T) {
	shaped := buildMuseCodeInteractiveArgs([]string{"--model", "m", "review \"this\"\nplease"})
	rewritten, path, err := rewriteMuseCodePromptToFile(shaped)
	if err != nil {
		t.Fatalf("staging failed: %v", err)
	}
	if path == "" {
		t.Fatalf("expected the prompt to be staged; argv=%v", shaped)
	}
	defer os.Remove(path)

	joined := strings.Join(rewritten, " ")
	if strings.Contains(joined, "review") || strings.Contains(joined, "please") {
		t.Fatalf("prompt text must leave argv: %q", joined)
	}
	for _, a := range rewritten {
		if a == "--" {
			t.Fatalf("the positional separator must go with the prompt: %v", rewritten)
		}
	}
	if rewritten[len(rewritten)-2] != "--prompt-file" || rewritten[len(rewritten)-1] != path {
		t.Fatalf("argv must end with --prompt-file <path>: %v", rewritten)
	}
	if !isMuseCodeSynthesizedRun(rewritten) {
		t.Fatalf("rewritten argv lost the forced headless shape: %v", rewritten)
	}
	if !strings.Contains(strings.Join(rewritten, " "), "--model m") {
		t.Fatalf("forwarded flags dropped: %v", rewritten)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "review \"this\"\nplease" {
		t.Fatalf("prompt file content changed: %q", body)
	}
	if info, statErr := os.Stat(path); statErr == nil && runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("prompt file must be owner-only, got %v", perm)
		}
	}

	// A diagnostic invocation and a promptless argv are returned untouched.
	diag := []string{"--version"}
	if got, p, _ := rewriteMuseCodePromptToFile(diag); p != "" || strings.Join(got, " ") != "--version" {
		t.Fatalf("diagnostic argv must pass through: %v %q", got, p)
	}
	bare := buildMuseCodeInteractiveArgs([]string{"--model", "m"})
	if got, p, _ := rewriteMuseCodePromptToFile(bare); p != "" || strings.Join(got, " ") != strings.Join(bare, " ") {
		t.Fatalf("promptless argv must pass through: %v %q", got, p)
	}
}

// A staging failure must surface as an error — StartSession then refuses the
// launch — never as a silent fallback that leaves the prompt on argv.
func TestRewriteMuseCodePromptToFileFailsClosed(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Both the per-user prompt dir and the OS temp fallback sit under a file.
	for _, k := range []string{"HOME", "USERPROFILE", "TMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, blocker)
	}
	shaped := buildMuseCodeInteractiveArgs([]string{"secret prompt"})
	got, path, err := rewriteMuseCodePromptToFile(shaped)
	if err == nil {
		os.Remove(path)
		t.Fatalf("expected a staging error, got argv=%v path=%q", got, path)
	}
	if path != "" {
		t.Fatalf("no cleanup path on failure, got %q", path)
	}
}
