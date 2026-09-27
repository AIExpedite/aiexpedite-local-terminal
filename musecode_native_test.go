package main

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseMuseCodeEventLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		ok   bool
		want oneShotEvent
	}{
		{"delta", museFrameDeltaHello, true, oneShotEvent{TextDelta: "Hello", SessionID: "0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11"}},
		{"delta field variant", `{"payload_type":"run.output.delta","payload":{"delta":"Hi"}}`, true, oneShotEvent{TextDelta: "Hi"}},
		{"delta keeps a lone space", `{"payload_type":"run.output.delta","payload":{"delta":" "}}`, true, oneShotEvent{TextDelta: " "}},
		{"delta text fallback", `{"payload_type":"run.output.delta","payload":{"text":"hi"}}`, true, oneShotEvent{TextDelta: "hi"}},
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
