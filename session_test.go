package main

import (
	"reflect"
	"testing"
)

// Codex's prompt now flows through stdin via the `-` positional placeholder
// instead of as a multi-KB argv element. These tests assert the new argv
// shape: prompt is pulled OUT of args (returned as stdinPrompt for the caller
// to write through the pipe) and `-` is appended as the last argv element.
func TestBuildCodexInteractiveArgs_PromptOnly(t *testing.T) {
	gotArgs, gotPrompt := buildCodexInteractiveArgs([]string{"implement this"})
	wantArgs := []string{
		"exec",
		"--json",
		"--dangerously-bypass-approvals-and-sandbox",
		"-",
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("buildCodexInteractiveArgs() args = %#v, want %#v", gotArgs, wantArgs)
	}
	if gotPrompt != "implement this" {
		t.Fatalf("buildCodexInteractiveArgs() prompt = %q, want %q", gotPrompt, "implement this")
	}
}

func TestBuildCodexInteractiveArgs_StripsDuplicateExecAndAutomationFlags(t *testing.T) {
	gotArgs, gotPrompt := buildCodexInteractiveArgs([]string{
		"exec",
		"--json",
		"--full-auto",
		"--dangerously-bypass-approvals-and-sandbox",
		"implement this",
	})
	wantArgs := []string{
		"exec",
		"--json",
		"--dangerously-bypass-approvals-and-sandbox",
		"-",
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("buildCodexInteractiveArgs() args = %#v, want %#v", gotArgs, wantArgs)
	}
	if gotPrompt != "implement this" {
		t.Fatalf("buildCodexInteractiveArgs() prompt = %q, want %q", gotPrompt, "implement this")
	}
}

func TestBuildCodexInteractiveArgs_StripsConflictingSandboxAndApprovalFlags(t *testing.T) {
	gotArgs, gotPrompt := buildCodexInteractiveArgs([]string{
		"exec",
		"--sandbox", "workspace-write",
		"--sandbox=read-only",
		"-s", "danger-full-access",
		"--approval-policy", "never",
		"--approval-policy=on-request",
		"--ask-for-approval", "never",
		"--ask-for-approval=on-request",
		"-a", "never",
		"--model", "gpt-5.4",
		"implement this",
	})
	wantArgs := []string{
		"exec",
		"--json",
		"--dangerously-bypass-approvals-and-sandbox",
		"--model", "gpt-5.4",
		"-",
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("buildCodexInteractiveArgs() args = %#v, want %#v", gotArgs, wantArgs)
	}
	if gotPrompt != "implement this" {
		t.Fatalf("buildCodexInteractiveArgs() prompt = %q, want %q", gotPrompt, "implement this")
	}
}

func TestShouldCloseStdinAfterStart(t *testing.T) {
	cases := []struct {
		name        string
		command     string
		stdinPrompt string
		want        bool
	}{
		// Claude in --input-format stream-json mode reads NDJSON from stdin
		// in a loop. Sessions are interactive — keep stdin open so the
		// orchestrator can SendInput follow-ups (whether or not an initial
		// prompt was queued via args). Closing stdin = claude EOFs and exits
		// in ~3s, which is the prod failure mode we're patching here.
		{name: "claude_with_stdin_prompt_stays_open", command: "claude", stdinPrompt: "do work", want: false},
		{name: "claude_without_prompt_stays_open", command: "claude", want: false},
		// Codex / gemini and shells are one-shot by design — they need stdin
		// closed when no prompt was queued (codex exec specifically blocks
		// on EOF before producing output).
		{name: "codex_exec_closes", command: "codex", want: true},
		{name: "gemini_exec_closes", command: "gemini", want: true},
		{name: "powershell_closes", command: "powershell", want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldCloseStdinAfterStart(tc.command, tc.stdinPrompt)
			if got != tc.want {
				t.Fatalf("shouldCloseStdinAfterStart(%q, %q) = %v, want %v",
					tc.command, tc.stdinPrompt, got, tc.want)
			}
		})
	}
}
