package main

import (
	"reflect"
	"testing"
)

func TestBuildCodexInteractiveArgs_PromptOnly(t *testing.T) {
	got := buildCodexInteractiveArgs([]string{"implement this"})
	want := []string{
		"exec",
		"--json",
		"--dangerously-bypass-approvals-and-sandbox",
		"implement this",
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildCodexInteractiveArgs() = %#v, want %#v", got, want)
	}
}

func TestBuildCodexInteractiveArgs_StripsDuplicateExecAndAutomationFlags(t *testing.T) {
	got := buildCodexInteractiveArgs([]string{
		"exec",
		"--json",
		"--full-auto",
		"--dangerously-bypass-approvals-and-sandbox",
		"implement this",
	})
	want := []string{
		"exec",
		"--json",
		"--dangerously-bypass-approvals-and-sandbox",
		"implement this",
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildCodexInteractiveArgs() = %#v, want %#v", got, want)
	}
}

func TestBuildCodexInteractiveArgs_StripsConflictingSandboxAndApprovalFlags(t *testing.T) {
	got := buildCodexInteractiveArgs([]string{
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
	want := []string{
		"exec",
		"--json",
		"--dangerously-bypass-approvals-and-sandbox",
		"--model", "gpt-5.4",
		"implement this",
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildCodexInteractiveArgs() = %#v, want %#v", got, want)
	}
}

func TestShouldCloseStdinAfterStart(t *testing.T) {
	cases := []struct {
		name        string
		command     string
		stdinPrompt string
		want        bool
	}{
		{name: "claude_with_stdin_prompt_stays_open", command: "claude", stdinPrompt: "do work", want: false},
		{name: "claude_without_prompt_closes", command: "claude", want: true},
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
