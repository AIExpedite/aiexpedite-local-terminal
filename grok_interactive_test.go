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
	want := []string{"--output-format", "streaming-json", "-p", "Have a short conversation"}
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
	want := []string{"--output-format", "streaming-json", "--model", "grok-4", "-p", "fix the bug"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildGrokInteractiveArgs_InlinePromptFlagValue(t *testing.T) {
	got := buildGrokInteractiveArgs([]string{"--single=hello there"})
	want := []string{"--output-format", "streaming-json", "-p", "hello there"}
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
