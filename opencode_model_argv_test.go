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
	// shape the caller's args, THEN match the allowlist. Gating the raw args
	// instead leaves the narrow default entry unmatched.
	dir := t.TempDir()
	al := &AllowList{configPath: filepath.Join(dir, "allow.txt")}
	if err := al.CreateDefault(); err != nil {
		t.Fatalf("CreateDefault: %v", err)
	}
	if err := al.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	rawArgs := []string{"--model", openCodeTestModel, "implement the feature"}

	if al.IsAllowed("opencode", rawArgs) {
		t.Fatalf("raw orchestration args were allowlisted directly — the shaping step is doing nothing")
	}
	if !al.IsAllowed("opencode", buildOpenCodeInteractiveArgs(rawArgs)) {
		t.Fatalf("synthesised argv %q is not allowlisted; a headless run would hang at the approval dialog",
			buildOpenCodeInteractiveArgs(rawArgs))
	}

	// The gate must not become a blanket `opencode *`: a raw interactive
	// invocation stays dialog-gated even after shaping (it is a diagnostic, so
	// shaping passes it through untouched).
	if al.IsAllowed("opencode", buildOpenCodeInteractiveArgs([]string{"serve"})) {
		t.Errorf("`opencode serve` became pre-approved; it must stay dialog-gated")
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
