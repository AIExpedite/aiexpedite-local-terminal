package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestBuildGrokInteractiveArgs_UnquotedPromptScreenshotCase reproduces the
// failing screenshot: `grok Have a short, simple conversation ...` tokenised so
// Grok parsed "a" as a subcommand. The builder must fold the whole prompt into
// a single `-p` value under managed streaming-json, exiting after one turn.
func TestBuildGrokInteractiveArgs_UnquotedPromptScreenshotCase(t *testing.T) {
	got := buildGrokInteractiveArgs([]string{"Have", "a", "short", "conversation"})
	want := []string{"--output-format", "streaming-json", "--always-approve", "-p", "Have a short conversation"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildGrokInteractiveArgs_StripsManagedFlagsAndPassesModel(t *testing.T) {
	// User-supplied -p, --output-format (+value), --prompt-file (+value) must be
	// stripped; --model must pass through; positional words become the prompt.
	got := buildGrokInteractiveArgs([]string{
		"--model", "grok-4", "--output-format", "json", "-p", "fix the bug",
	})
	want := []string{"--output-format", "streaming-json", "--always-approve", "--model", "grok-4", "-p", "fix the bug"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildGrokInteractiveArgs_InlinePromptFlagValue(t *testing.T) {
	got := buildGrokInteractiveArgs([]string{"--single=hello there"})
	want := []string{"--output-format", "streaming-json", "--always-approve", "-p", "hello there"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildGrokInteractiveArgs_SubcommandCarveOut(t *testing.T) {
	// `grok models` is not a prompt — pass through verbatim, no injected -p.
	in := []string{"models"}
	got := buildGrokInteractiveArgs(in)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("subcommand should be untouched: got %#v", got)
	}
}

// TestBuildGrokInteractiveArgs_SubcommandPreScanSkipsValuedFlagValues guards
// the pre-scan that decides "is this a subcommand invocation or a prompt?":
// the scan must skip a valued flag's value so the value can't be mistaken for
// a subcommand. Without skipping, `grok --cwd sessions fix bug` would treat
// the `--cwd` value "sessions" as the `sessions` subcommand and return the
// argv untouched, sending the call down the unmanaged path instead of
// injecting `-p`.
func TestBuildGrokInteractiveArgs_SubcommandPreScanSkipsValuedFlagValues(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "--cwd value happens to be a subcommand name",
			in:   []string{"--cwd", "sessions", "fix", "bug"},
			want: []string{"--output-format", "streaming-json", "--always-approve", "--cwd", "sessions", "-p", "fix bug"},
		},
		{
			name: "--model value happens to be a subcommand name",
			in:   []string{"--model", "agent", "do", "thing"},
			want: []string{"--output-format", "streaming-json", "--always-approve", "--model", "agent", "-p", "do thing"},
		},
		{
			name: "-r value happens to be a subcommand name",
			in:   []string{"-r", "memory", "continue"},
			want: []string{"--output-format", "streaming-json", "--always-approve", "-r", "memory", "-p", "continue"},
		},
		{
			name: "--cwd=value equals form (single token) still routes correctly",
			in:   []string{"--cwd=sessions", "fix", "bug"},
			want: []string{"--output-format", "streaming-json", "--always-approve", "--cwd=sessions", "-p", "fix bug"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGrokInteractiveArgs(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestBuildGrokInteractiveArgs_InjectsAlwaysApproveByDefault guards the
// unconditional injection of `--always-approve` on managed headless turns.
// Without it, Grok's default `ask` permission mode would prompt for tool
// execution / file edits, but StartSession closes Grok's stdin after launch
// and detectPromptFromJSON has no Grok approval branch — the prompt cannot
// be answered and the headless session stalls or fails.
func TestBuildGrokInteractiveArgs_InjectsAlwaysApproveByDefault(t *testing.T) {
	got := buildGrokInteractiveArgs([]string{"do", "the", "thing"})
	hasFlag := false
	for _, a := range got {
		if a == "--always-approve" {
			hasFlag = true
			break
		}
	}
	if !hasFlag {
		t.Fatalf("expected --always-approve in managed argv, got %#v", got)
	}
}

// TestBuildGrokInteractiveArgs_DoesNotDuplicateUserAlwaysApprove guards the
// dedupe path: when the caller already passed an approval-bypass flag (any of
// the documented spellings), the builder must not append its own copy.
func TestBuildGrokInteractiveArgs_DoesNotDuplicateUserAlwaysApprove(t *testing.T) {
	cases := []struct {
		name string
		in   []string
	}{
		{name: "bare --always-approve", in: []string{"--always-approve", "do", "thing"}},
		{name: "--always-approve=true", in: []string{"--always-approve=true", "do", "thing"}},
		{name: "--auto-approve synonym", in: []string{"--auto-approve", "do", "thing"}},
		{name: "--auto-approve=true synonym", in: []string{"--auto-approve=true", "do", "thing"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGrokInteractiveArgs(tc.in)
			count := 0
			for _, a := range got {
				lower := strings.ToLower(a)
				if lower == "--always-approve" || lower == "--auto-approve" ||
					strings.HasPrefix(lower, "--always-approve=") ||
					strings.HasPrefix(lower, "--auto-approve=") {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("expected exactly one approval flag in argv, got %d: %#v", count, got)
			}
		})
	}
}

// TestBuildGrokInteractiveArgs_PreservesResumeAndSessionIDValues guards the
// xAI Headless & Scripting common flags `-r/--resume <ID>` and
// `-s/--session-id <ID>`: without an entry in valuedFlags the next token (the
// ID) would land in promptParts and Grok would then see `--resume -p` (the
// managed `-p` flag swallowed as the resume ID), breaking resumed sessions.
func TestBuildGrokInteractiveArgs_PreservesResumeAndSessionIDValues(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "--resume keeps its ID and prompt is preserved",
			in:   []string{"--resume", "abc", "continue", "work"},
			want: []string{"--output-format", "streaming-json", "--always-approve", "--resume", "abc", "-p", "continue work"},
		},
		{
			name: "-r short form",
			in:   []string{"-r", "abc", "ship", "it"},
			want: []string{"--output-format", "streaming-json", "--always-approve", "-r", "abc", "-p", "ship it"},
		},
		{
			name: "--session-id keeps its ID",
			in:   []string{"--session-id", "sess-42", "next", "step"},
			want: []string{"--output-format", "streaming-json", "--always-approve", "--session-id", "sess-42", "-p", "next step"},
		},
		{
			name: "-s short form",
			in:   []string{"-s", "sess-42", "go"},
			want: []string{"--output-format", "streaming-json", "--always-approve", "-s", "sess-42", "-p", "go"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGrokInteractiveArgs(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestBuildGrokInteractiveArgs_SubcommandPreScanSkipsPromptFlagValues guards
// the same pre-scan against `-p`/`--single`: their value is the first word of
// the prompt, which can collide with a subcommand name (`help`, `models`,
// `sessions`, etc.). Without skipping that value the pre-scan returns the raw
// argv early, the managed `-p` folding never runs, and Grok sees only "help"
// as the prompt while the rest of the words are parsed as stray
// positionals — exactly the tokenisation failure this builder exists to fix.
func TestBuildGrokInteractiveArgs_SubcommandPreScanSkipsPromptFlagValues(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "-p value starts with a subcommand-name word",
			in:   []string{"-p", "help", "me", "fix", "tests"},
			want: []string{"--output-format", "streaming-json", "--always-approve", "-p", "help me fix tests"},
		},
		{
			name: "--single value starts with a subcommand-name word",
			in:   []string{"--single", "models", "in", "this", "repo"},
			want: []string{"--output-format", "streaming-json", "--always-approve", "-p", "models in this repo"},
		},
		{
			name: "-p value equals exactly a subcommand name",
			in:   []string{"-p", "sessions"},
			want: []string{"--output-format", "streaming-json", "--always-approve", "-p", "sessions"},
		},
		{
			name: "-p=value inline form (single token) still routes correctly",
			in:   []string{"-p=help", "me", "out"},
			want: []string{"--output-format", "streaming-json", "--always-approve", "-p", "help me out"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGrokInteractiveArgs(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestDetectCLITerminalEvent_Grok(t *testing.T) {
	if !detectCLITerminalEvent("grok", `{"type":"end"}`) {
		t.Fatal(`grok "end" event should be terminal`)
	}
	if detectCLITerminalEvent("grok", `{"type":"text"}`) {
		t.Fatal(`grok "text" event should NOT be terminal`)
	}
}

func TestGrokLimitStateFromFrame_Reached(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	frame := map[string]interface{}{
		"type":         "usage_limit_reached",
		"gate_message": "You've hit your Grok limit.",
		"gate_url":     "https://grok.com/supergrok",
	}
	st, ok := grokLimitStateFromFrame(frame, now)
	if !ok || st.Severity != grokLimitReached {
		t.Fatalf("want reached, got ok=%v sev=%q", ok, st.Severity)
	}
	if st.Message == "" || !strings.Contains(st.UpgradeURL, "supergrok") {
		t.Fatalf("message/url not captured: %+v", st)
	}
}

func TestGrokLimitStateFromFrame_Approaching(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	frame := map[string]interface{}{
		"event": "credit_limit_upsell_shown",
	}
	st, ok := grokLimitStateFromFrame(frame, now)
	if !ok || st.Severity != grokLimitApproaching {
		t.Fatalf("want approaching, got ok=%v sev=%q", ok, st.Severity)
	}
}

func TestGrokLimitStateFromFrame_NoSignal(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	if _, ok := grokLimitStateFromFrame(map[string]interface{}{"type": "text", "text": "hi"}, now); ok {
		t.Fatal("ordinary frame must not produce a limit state")
	}
}
