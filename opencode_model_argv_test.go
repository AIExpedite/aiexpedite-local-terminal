// opencode_model_argv_test.go
// -----------------------------------------------------------------------------
// The `--model` forwarding contract for orchestrated OpenCode sessions.
//
// An OpenCode preference entry is a (binary, model) pair: the orchestrator's CLI
// resolver composes `opencode --model <id>` and hands it to the device as a
// session_start. Three things have to line up for that to run headlessly, and
// each fails silently on its own:
//
//  1. buildOpenCodeInteractiveArgs must forward `--model` WITH ITS VALUE into
//     the forced `run --format json --model <m> <prompt>` order. A value left
//     behind is read as prompt text and the model silently reverts to
//     OpenCode's default — the run completes, on the wrong model.
//  2. gateSessionEntryCommand must gate against that SYNTHESISED argv, not the
//     raw one, so the narrow `opencode run --format json *` allowlist entry
//     matches the argv the device will actually exec. Gating raw args instead
//     hangs a headless run at the approval dialog with nobody there to answer.
//  3. The shaped argv must actually reach the binary in that order.
//
// CI has no real `opencode` binary, so (3) is verified against the stub
// executable's argv contract rather than a live model switch — an upstream
// change to OpenCode's own `--model` semantics would not be caught here. The
// OPENCODE_NATIVE_MIN_VERSION gate and the manual resume matrix remain the
// runtime mitigations.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const openCodeTestModel = "anthropic/claude-sonnet-4-5"

func TestBuildOpenCodeInteractiveArgs_ForwardsModel(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "spaced --model keeps its value and lands before the prompt",
			in:   []string{"--model", openCodeTestModel, "implement the feature"},
			want: []string{"run", "--format", "json", "--model", openCodeTestModel, "implement the feature"},
		},
		{
			name: "inline --model=value is forwarded as one token",
			in:   []string{"--model=" + openCodeTestModel, "implement the feature"},
			want: []string{"run", "--format", "json", "--model=" + openCodeTestModel, "implement the feature"},
		},
		{
			name: "short -m keeps its value",
			in:   []string{"-m", openCodeTestModel, "implement the feature"},
			want: []string{"run", "--format", "json", "-m", openCodeTestModel, "implement the feature"},
		},
		{
			name: "a colon-bearing model id survives intact",
			in:   []string{"--model", "ollama/llama3:8b", "implement the feature"},
			want: []string{"run", "--format", "json", "--model", "ollama/llama3:8b", "implement the feature"},
		},
		{
			name: "a caller-supplied `run` is not duplicated",
			in:   []string{"run", "--model", openCodeTestModel, "implement the feature"},
			want: []string{"run", "--format", "json", "--model", openCodeTestModel, "implement the feature"},
		},
		{
			name: "a multi-word prompt stays trailing, after the flags",
			in:   []string{"--model", openCodeTestModel, "implement", "the", "feature"},
			want: []string{"run", "--format", "json", "--model", openCodeTestModel, "implement", "the", "feature"},
		},
		{
			name: "no model pins OpenCode's own default",
			in:   []string{"implement the feature"},
			want: []string{"run", "--format", "json", "implement the feature"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildOpenCodeInteractiveArgs(tc.in)
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("buildOpenCodeInteractiveArgs(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBuildOpenCodeInteractiveArgs_DiagnosticsPassThroughUnshaped(t *testing.T) {
	// Reshaping a diagnostic into `run` would turn an information request into
	// a model call — and `opencode --version` / `opencode models` are exactly
	// how the capability probe and the usage parser query the CLI.
	for _, args := range [][]string{
		{"--version"},
		{"models"},
		{"auth", "list"},
		{"serve"},
	} {
		got := buildOpenCodeInteractiveArgs(args)
		if strings.Join(got, "\x00") != strings.Join(args, "\x00") {
			t.Errorf("diagnostic %q was reshaped to %q; want verbatim", args, got)
		}
	}
}

func TestOpenCodeSessionStartGate_MatchesSynthesisedArgv(t *testing.T) {
	// The composition gateSessionEntryCommand performs for a session_start:
	// shape the caller's args, and approve the synthesised shape inside
	// the session_start gate while keeping the shared execute allowlist closed.
	dir := t.TempDir()
	al := &AllowList{configPath: filepath.Join(dir, "allow.txt")}
	if err := al.CreateDefault(); err != nil {
		t.Fatalf("CreateDefault: %v", err)
	}
	if err := al.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	defaultAllowList = al
	signedCfg := &Config{EnableAllowList: true, CommandSecret: "sec-123"}
	unsignedCfg := &Config{EnableAllowList: true, CommandSecret: ""}

	prevDialog := commandApprovalDialogFn
	commandApprovalDialogFn = func(string, []string, int) ApprovalResult { return ApprovalDeny }
	t.Cleanup(func() { commandApprovalDialogFn = prevDialog })

	rawArgs := []string{"--model", openCodeTestModel, "implement the feature"}
	shaped := buildOpenCodeInteractiveArgs(rawArgs)

	// Inbound execute request MUST be gated by approval dialog in both modes.
	if !shouldGateExecuteCommand(signedCfg, al, "opencode", shaped) {
		t.Fatalf("raw execute with shaped OpenCode argv skipped gating — it must stay gated")
	}

	// Signed session start with synthesised argv MUST be approved without dialog.
	cmd := commandMsg{Type: "session_start", Command: "opencode", Args: rawArgs}
	if !gateSessionEntryCommand(nil, nil, nil, cmd, signedCfg) {
		t.Fatalf("signed session_start for synthesised argv %q was not approved", shaped)
	}

	// Unsigned session start MUST stay dialog-gated (fails because stub returns ApprovalDeny).
	if gateSessionEntryCommand(nil, nil, nil, cmd, unsignedCfg) {
		t.Fatalf("unsigned session_start for synthesised argv %q was auto-approved; it must require local approval", shaped)
	}

	// Signed opencode_native_start MUST be approved without dialog.
	nativeCmd := commandMsg{Type: "opencode_native_start", Command: "opencode"}
	if !gateSessionEntryCommand(nil, nil, nil, nativeCmd, signedCfg) {
		t.Fatalf("signed opencode_native_start was not approved")
	}

	// Unsigned opencode_native_start MUST stay dialog-gated.
	if gateSessionEntryCommand(nil, nil, nil, nativeCmd, unsignedCfg) {
		t.Fatalf("unsigned opencode_native_start was auto-approved; it must require local approval")
	}

	// Unshaped diagnostic session start (e.g. `opencode serve`) stays dialog-gated even when signed.
	serveCmd := commandMsg{Type: "session_start", Command: "opencode", Args: []string{"serve"}}
	if gateSessionEntryCommand(nil, nil, nil, serveCmd, signedCfg) {
		t.Errorf("`opencode serve` session_start was auto-approved; it must stay dialog-gated")
	}
}

func TestOpenCodeStub_ReceivesShapedModelArgv(t *testing.T) {
	// End-to-end against the stub executable: the shaped argv is what the
	// process actually receives, in order.
	installOpenCodeStub(t)

	argvLog := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("OPENCODE_STUB_ARGV_LOG", argvLog)

	shaped := buildOpenCodeInteractiveArgs(
		[]string{"--model", openCodeTestModel, "implement the feature"},
	)
	if err := exec.Command("opencode", shaped...).Run(); err != nil {
		t.Fatalf("stub run failed: %v", err)
	}

	logged, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	got := strings.TrimSpace(string(logged))

	for _, want := range []string{
		"run",
		"--format json",
		"--model " + openCodeTestModel,
		"implement the feature",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stub argv %q is missing %q", got, want)
		}
	}
	// Order matters: the prompt must trail the flags, or OpenCode reads the
	// model id as prompt text.
	if strings.Index(got, openCodeTestModel) > strings.Index(got, "implement the feature") {
		t.Errorf("stub argv %q put the prompt before the --model value", got)
	}
}

func TestOpenCodeSession_SanitizesUnrelatedCredentials(t *testing.T) {
	rawEnv := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk-ant-secret",
		"ANTHROPIC_AUTH_TOKEN=auth-token",
		"CODEX_API_KEY=codex-secret",
		"XAI_API_KEY=xai-secret",
		"GROK_API_KEY=grok-secret",
		"CLAUDE_CODE_OAUTH_TOKEN=claude-oauth",
		"OPENCODE_API_KEY=opencode-secret",
	}

	filtered, stripped := sanitizeClaudeChildEnv("opencode", rawEnv)

	filteredMap := make(map[string]bool)
	for _, e := range filtered {
		filteredMap[strings.Split(e, "=")[0]] = true
	}

	for _, denied := range []string{
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"CODEX_API_KEY",
		"XAI_API_KEY",
		"GROK_API_KEY",
		"CLAUDE_CODE_OAUTH_TOKEN",
	} {
		if filteredMap[denied] {
			t.Errorf("filtered env retained unrelated credential %q", denied)
		}
	}

	if !filteredMap["OPENCODE_API_KEY"] || !filteredMap["PATH"] {
		t.Errorf("filtered env dropped legitimate vars: %v", filtered)
	}

	if len(stripped) != 6 {
		t.Errorf("expected 6 stripped vars, got %v", stripped)
	}
}
