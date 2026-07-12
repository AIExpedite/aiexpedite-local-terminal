//go:build !windows
// +build !windows

// File: pty_session_unix_test.go
// Integration tests for the macOS/Linux PTY session path: a real pseudo-terminal
// is allocated, output is normalized (no ANSI/CR spam), and a prompt that goes
// unanswered aborts instead of hanging.
package main

import (
	"strings"
	"testing"
	"time"
)

func TestRunPTYCommand_AllocatesRealTTY(t *testing.T) {
	out, aborted, msg, err := runPTYCommand("sh",
		[]string{"-c", "test -t 1 && echo ISTTY || echo NOTTY"},
		"", nil, 10*time.Second, DefaultPTYPromptTimeout, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if aborted {
		t.Fatalf("unexpected abort: %s", msg)
	}
	if !strings.Contains(out, "ISTTY") {
		t.Fatalf("child was not attached to a tty; output=%q", out)
	}
}

func TestRunPTYCommand_NormalizesANSIAndCRFrames(t *testing.T) {
	// agy-style: colored redraw of a line, then the final rendered state.
	script := `printf '\033[32mstep 1\033[0m\r\033[Kstep 2\r\033[Kdone\n'`
	out, aborted, msg, err := runPTYCommand("sh", []string{"-c", script},
		"", nil, 10*time.Second, DefaultPTYPromptTimeout, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if aborted {
		t.Fatalf("unexpected abort: %s", msg)
	}
	if strings.ContainsRune(out, '\x1b') {
		t.Errorf("normalized output still contains ANSI ESC: %q", out)
	}
	if !strings.Contains(out, "done") {
		t.Errorf("expected final rendered line 'done', got %q", out)
	}
	// The intermediate redraw frames must not all survive as separate lines.
	if strings.Count(out, "step 1") > 0 {
		t.Errorf("intermediate CR frame 'step 1' leaked into model output: %q", out)
	}
}

func TestRunPTYCommand_AbortsOnQuietAfterPrompt(t *testing.T) {
	start := time.Now()
	// Emit a credential prompt then go quiet — must abort on the prompt timeout,
	// not hang for the full 30s sleep.
	out, aborted, msg, err := runPTYCommand("sh",
		[]string{"-c", "printf 'Password: '; sleep 30"},
		"", nil, 60*time.Second, 500*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !aborted {
		t.Fatalf("expected abort on quiet-after-prompt; out=%q", out)
	}
	if !strings.Contains(msg, "prompt") {
		t.Errorf("abort message should mention the prompt: %q", msg)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("abort took too long (%v); prompt timeout not enforced", time.Since(start))
	}
}

func TestEnsurePTYTerm(t *testing.T) {
	// Missing TERM is defaulted.
	got := ensurePTYTerm([]string{"PATH=/bin"})
	found := ""
	for _, e := range got {
		if strings.HasPrefix(e, "TERM=") {
			found = e
		}
	}
	if found != "TERM=xterm-256color" {
		t.Errorf("missing TERM should default to xterm-256color; got %q", found)
	}
	// A caller-supplied TERM (even dumb) is preserved untouched.
	got = ensurePTYTerm([]string{"PATH=/bin", "TERM=dumb"})
	count := 0
	for _, e := range got {
		if strings.HasPrefix(e, "TERM=") {
			count++
			if e != "TERM=dumb" {
				t.Errorf("caller TERM should be preserved; got %q", e)
			}
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one TERM entry, got %d", count)
	}
}

func TestRunPTYCommand_SeedsTERMWhenParentHasNone(t *testing.T) {
	// A PTY child launched with an env lacking TERM must still see a usable TERM,
	// so TUI/curses agents render instead of aborting. Pass an explicit env with
	// no TERM to simulate the tray/desktop app launched outside a terminal.
	out, aborted, msg, err := runPTYCommand("sh",
		[]string{"-c", "printf 'TERM=%s\\n' \"$TERM\""},
		"", []string{"PATH=/usr/bin:/bin"}, 10*time.Second, DefaultPTYPromptTimeout, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if aborted {
		t.Fatalf("unexpected abort: %s", msg)
	}
	if !strings.Contains(out, "TERM=xterm-256color") {
		t.Fatalf("PTY child did not see seeded TERM; output=%q", out)
	}
}
