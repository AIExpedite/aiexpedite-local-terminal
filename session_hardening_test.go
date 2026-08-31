// File: session_hardening_test.go
// Tests for the pipe-based session_start hardening + agent classification:
//   - a NON-resident utility session (bash/sh/git/…) gets the authoritative
//     headless git/editor/credential env so prompts can't hang it (finding 1),
//   - resident CLI agents are classified so they keep their interactive env,
//   - the `antigravity` alias routes through the same one-shot argv shaping as
//     `agy` (finding 4).
package main

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// runUtilitySession starts a non-agent `sh -c <script>` session (tty=false) and
// returns the concatenated streamed stdout once the session ends. Unix-only.
func runUtilitySession(t *testing.T, script string) string {
	t.Helper()
	sm := NewSessionManager(nil)
	id := fmt.Sprintf("util-%d", time.Now().UnixNano())

	var mu sync.Mutex
	var out strings.Builder
	ended := false
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		if res.Type == "stream" {
			out.WriteString(res.Output)
			out.WriteByte('\n')
		}
		if res.Type == "session_ended" {
			ended = true
		}
	}

	if err := sm.StartSession(id, "sh", []string{"-c", script}, "", "ws", "uid", 30000, false, publishFn); err != nil {
		t.Fatalf("StartSession(sh): %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := ended
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	return out.String()
}

// A non-agent utility session must receive the authoritative non-interactive
// git/editor overlay from hardenNonAgentCommand — proving the overlay reaches
// the child on the session_start path (the one the orchestrator actually uses
// for ordinary commands), not just the one-shot execute path.
func TestStartSession_UtilityGetsHeadlessGitEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses `sh`; Windows session hardening covered via env-overlay unit tests")
	}
	out := runUtilitySession(t,
		`printf 'GTP=[%s] EDITOR=[%s] MERGE=[%s]\n' "$GIT_TERMINAL_PROMPT" "$GIT_EDITOR" "$GIT_MERGE_AUTOEDIT"`)
	for _, want := range []string{"GTP=[0]", "EDITOR=[true]", "MERGE=[no]"} {
		if !strings.Contains(out, want) {
			t.Errorf("utility session missing forced env %q; streamed=%q", want, out)
		}
	}
}

func TestIsResidentAgentSessionCommand(t *testing.T) {
	resident := []string{
		"claude", "claude --print x", "/usr/local/bin/claude", "claude.cmd",
		"codex", "codex exec", "grok", "grok agent stdio",
		"agy", "agy --print", "antigravity", "/opt/bin/antigravity", "antigravity.exe",
	}
	utility := []string{
		"bash", "bash -c 'git push'", "sh", "git", "git fetch --all",
		"powershell", "pwsh", "npm test", "pytest", "node app.js", "",
		// Gemini's CLI-agent router was removed, so a stale/manual `gemini`
		// session_start must fall through to headless hardening, not stay resident.
		"gemini", "gemini --model x", "/usr/local/bin/gemini", "gemini.cmd",
	}
	for _, c := range resident {
		if !isResidentAgentSessionCommand(c) {
			t.Errorf("isResidentAgentSessionCommand(%q) = false, want true", c)
		}
	}
	for _, c := range utility {
		if isResidentAgentSessionCommand(c) {
			t.Errorf("isResidentAgentSessionCommand(%q) = true, want false", c)
		}
	}
}

// `antigravity` aliases must route through the same managed stream-json stdin
// transport as `agy`; otherwise the alias would retain the long prompt on argv
// and reproduce the Windows CreateProcess failure.
func TestBuildInteractiveCLIArgs_AntigravityAlias(t *testing.T) {
	for _, cmd := range []string{"agy", "antigravity", "/usr/local/bin/antigravity", "antigravity.exe"} {
		got, stdinPrompt := buildInteractiveCLIArgs(cmd, []string{"do the thing"}, false)
		if stdinPrompt != "do the thing" {
			t.Errorf("%s: stdin prompt = %q, want %q", cmd, stdinPrompt, "do the thing")
		}
		joined := strings.Join(got, " ")
		if !strings.Contains(joined, "--input-format stream-json") ||
			!strings.Contains(joined, "--output-format stream-json") ||
			!strings.Contains(joined, "--dangerously-skip-permissions") {
			t.Errorf("%s: args %q missing Antigravity stream flags", cmd, joined)
		}
		if strings.Contains(joined, "do the thing") || strings.Contains(joined, "--print") {
			t.Errorf("%s: prompt leaked onto argv: %q", cmd, joined)
		}
	}
}
