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
//     the forced `run --format json --model <m>` order, returning the prompt
//     separately for the child's stdin. A value left behind is read as prompt
//     text and the model silently reverts to OpenCode's default — the run
//     completes, on the wrong model.
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
		name       string
		in         []string
		want       []string
		wantPrompt string
	}{
		{
			name:       "spaced --model keeps its value; the prompt leaves argv",
			in:         []string{"--model", openCodeTestModel, "implement the feature"},
			want:       []string{"run", "--format", "json", "--model", openCodeTestModel},
			wantPrompt: "implement the feature",
		},
		{
			name:       "inline --model=value is forwarded as one token",
			in:         []string{"--model=" + openCodeTestModel, "implement the feature"},
			want:       []string{"run", "--format", "json", "--model=" + openCodeTestModel},
			wantPrompt: "implement the feature",
		},
		{
			name:       "short -m keeps its value",
			in:         []string{"-m", openCodeTestModel, "implement the feature"},
			want:       []string{"run", "--format", "json", "-m", openCodeTestModel},
			wantPrompt: "implement the feature",
		},
		{
			name:       "a colon-bearing model id survives intact",
			in:         []string{"--model", "ollama/llama3:8b", "implement the feature"},
			want:       []string{"run", "--format", "json", "--model", "ollama/llama3:8b"},
			wantPrompt: "implement the feature",
		},
		{
			name:       "a caller-supplied `run` is not duplicated",
			in:         []string{"run", "--model", openCodeTestModel, "implement the feature"},
			want:       []string{"run", "--format", "json", "--model", openCodeTestModel},
			wantPrompt: "implement the feature",
		},
		{
			name:       "a multi-word prompt is joined into the stdin prompt",
			in:         []string{"--model", openCodeTestModel, "implement", "the", "feature"},
			want:       []string{"run", "--format", "json", "--model", openCodeTestModel},
			wantPrompt: "implement the feature",
		},
		{
			name:       "--variant (reasoning effort) keeps its value beside --model",
			in:         []string{"--model", openCodeTestModel, "--variant", "high", "implement the feature"},
			want:       []string{"run", "--format", "json", "--model", openCodeTestModel, "--variant", "high"},
			wantPrompt: "implement the feature",
		},
		{
			name:       "no model pins OpenCode's own default",
			in:         []string{"implement the feature"},
			want:       []string{"run", "--format", "json"},
			wantPrompt: "implement the feature",
		},
		{
			// Arity is never guessed: nothing tells us whether an unlearned
			// option is boolean or consumes the next token. The FLAG is
			// forwarded; a SEPARATE operand stays with the prompt, because
			// moving it onto argv when the option turns out to be boolean would
			// split the prompt across argv and stdin and expose prompt text in a
			// process listing. A bare option OpenCode needs an operand for is
			// then refused and classifies as flag_rejected — a precise
			// diagnostic instead of a fused failure.
			name:       "an unlearned caller flag is forwarded and its separate operand stays with the prompt",
			in:         []string{"--not-a-known-flag", "someValue", "implement the feature"},
			want:       []string{"run", "--format", "json", "--not-a-known-flag"},
			wantPrompt: "someValue implement the feature",
		},
		{
			// The documented escape hatch for the case above: `--flag=value` is
			// unambiguous, so an unlearned option's value IS forwarded intact
			// and nothing leaks into the prompt. This is what a caller sends
			// (and what a reviewer should reach for) before this repo has
			// learned a newly shipped OpenCode option.
			name:       "an unlearned caller flag keeps an inline value intact",
			in:         []string{"--not-a-known-flag=someValue", "implement the feature"},
			want:       []string{"run", "--format", "json", "--not-a-known-flag=someValue"},
			wantPrompt: "implement the feature",
		},
		{
			// Manager-owned positions are still taken away with their values.
			name:       "manager-owned flags are stripped with their values",
			in:         []string{"--format", "text", "--session", "ses_someoneElse", "--continue", "--fork", "abc", "--print-logs", "do it"},
			want:       []string{"run", "--format", "json"},
			wantPrompt: "do it",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, prompt := buildOpenCodeInteractiveArgs(tc.in)
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("buildOpenCodeInteractiveArgs(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
			if prompt != tc.wantPrompt {
				t.Fatalf("stdin prompt = %q, want %q", prompt, tc.wantPrompt)
			}
			// The forced prefix is the shared builder's, never re-listed here.
			forced := buildOpenCodeRunArgs(openCodeRunShapeNoSession, "")
			if strings.Join(got[:len(forced)], "\x00") != strings.Join(forced, "\x00") {
				t.Fatalf("argv %q does not lead with the forced shape %q", got, forced)
			}
			// The prompt must never appear on argv — a process listing any local
			// user can read would otherwise carry it.
			for _, a := range got {
				if tc.wantPrompt != "" && a == tc.wantPrompt {
					t.Fatalf("prompt %q reached argv: %q", tc.wantPrompt, got)
				}
			}
		})
	}
}

func TestBuildOpenCodeInteractiveArgs_ForwardsUnknownFlagsAndStripsOwnedOnes(t *testing.T) {
	// One strip policy (openCodeStrippedCallerFlagAt): every manager-owned flag
	// goes with its value, a caller's duplicate `run` is dropped, and an
	// unlearned flag survives — its separate operand stays with the prompt,
	// never dropped and never re-ordered onto argv.
	got, prompt := buildOpenCodeInteractiveArgs([]string{
		"run", "--format", "json", "--session", "ses_x", "--fork", "abc",
		"--continue", "--print-logs", "--model", openCodeTestModel,
		"--not-a-known-flag", "someValue",
	})
	want := []string{"run", "--format", "json", "--model", openCodeTestModel, "--not-a-known-flag"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("buildOpenCodeInteractiveArgs\n got %q\nwant %q", got, want)
	}
	// `someValue` cannot be re-ordered ahead of the prompt (the flag's arity is
	// unknown), so it lands in the prompt rather than being dropped.
	if prompt != "someValue" {
		t.Fatalf("stdin prompt = %q, want the unknown flag's trailing token", prompt)
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
		got, prompt := buildOpenCodeInteractiveArgs(args)
		if strings.Join(got, "\x00") != strings.Join(args, "\x00") {
			t.Errorf("diagnostic %q was reshaped to %q; want verbatim", args, got)
		}
		if prompt != "" {
			t.Errorf("diagnostic %q produced a stdin prompt %q", args, prompt)
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
	shaped, _ := buildOpenCodeInteractiveArgs(rawArgs)

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

func TestOpenCodeStub_ReceivesShapedModelArgvAndStdinPrompt(t *testing.T) {
	// End-to-end against the stub executable: the shaped argv is what the
	// process actually receives, in order — and the prompt arrives on stdin
	// rather than on the command line.
	installOpenCodeStub(t)

	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.log")
	stdinLog := filepath.Join(dir, "stdin.log")
	t.Setenv("OPENCODE_STUB_ARGV_LOG", argvLog)
	t.Setenv("OPENCODE_STUB_STDIN_LOG", stdinLog)

	shaped, prompt := buildOpenCodeInteractiveArgs(
		[]string{"--model", openCodeTestModel, "implement the feature"},
	)
	cmd := exec.Command("opencode", shaped...)
	cmd.Stdin = strings.NewReader(prompt)
	if err := cmd.Run(); err != nil {
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
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stub argv %q is missing %q", got, want)
		}
	}
	if strings.Contains(got, "implement the feature") {
		t.Errorf("stub argv %q carries the prompt; it must travel on stdin", got)
	}

	delivered, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatalf("read stdin log: %v", err)
	}
	if strings.TrimSpace(string(delivered)) != "implement the feature" {
		t.Errorf("stub stdin = %q, want the prompt", string(delivered))
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
