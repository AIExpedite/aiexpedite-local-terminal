//go:build !windows
// +build !windows

// File: pty_session_manager_unix_test.go
// End-to-end tests for the interactive PTY session_start path: an eligible agy
// session (tty=true) runs under a real PTY, its output is normalized before it
// is streamed, and a prompt that goes unanswered aborts the session.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// runAgyPTYSession copies the test binary to an `agy` on PATH (so the PTY
// eligibility allowlist matches), starts a tty=true session in the given mock
// mode, and returns every published frame once the session ends.
func runAgyPTYSession(t *testing.T, mockMode string) []resultMsg {
	t.Helper()
	testExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	tmpDir := t.TempDir()
	if err := copyTestBinary(testExe, filepath.Join(tmpDir, "agy")); err != nil {
		t.Fatalf("copy test binary: %v", err)
	}
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(mockCLIEnvVar, mockMode)

	sm := NewSessionManager(nil)
	id := fmt.Sprintf("pty-agy-%d", time.Now().UnixNano())
	var mu sync.Mutex
	var captured []resultMsg
	publishFn := func(res resultMsg) {
		mu.Lock()
		captured = append(captured, res)
		mu.Unlock()
	}

	if err := sm.StartSession(id, "agy", nil, tmpDir, "ws", "uid", 30000, true, publishFn); err != nil {
		t.Fatalf("StartSession(tty=true): %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ended := false
		for _, m := range captured {
			if m.Type == "session_ended" {
				ended = true
			}
		}
		mu.Unlock()
		if ended {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	out := make([]resultMsg, len(captured))
	copy(out, captured)
	return out
}

func streamedAndEnded(msgs []resultMsg) (string, bool) {
	var sb strings.Builder
	ended := false
	for _, m := range msgs {
		if m.Type == "stream" {
			sb.WriteString(m.Output)
			sb.WriteByte('\n')
		}
		if m.Type == "session_ended" {
			ended = true
		}
	}
	return sb.String(), ended
}

func TestStartSession_PTY_NormalizesAgyTUIOutput(t *testing.T) {
	out, ended := streamedAndEnded(runAgyPTYSession(t, "agy-tty-frames"))
	if !ended {
		t.Fatalf("session never ended; streamed=%q", out)
	}
	if strings.ContainsRune(out, '\x1b') {
		t.Errorf("ANSI ESC leaked into streamed frames: %q", out)
	}
	if !strings.Contains(out, "done") {
		t.Errorf("final rendered line 'done' missing from frames: %q", out)
	}
	if strings.Contains(out, "step 1") {
		t.Errorf("intermediate CR frame 'step 1' leaked into model-facing frames: %q", out)
	}
}

func TestStartSession_PTY_AbortsOnQuietAfterPrompt(t *testing.T) {
	old := ptySessionPromptTimeout
	ptySessionPromptTimeout = 500 * time.Millisecond
	defer func() { ptySessionPromptTimeout = old }()

	start := time.Now()
	out, ended := streamedAndEnded(runAgyPTYSession(t, "agy-prompt-hang"))
	if !ended {
		t.Fatalf("session never ended (should abort on prompt); streamed=%q", out)
	}
	if time.Since(start) > 12*time.Second {
		t.Errorf("abort took too long (%v); quiet-after-prompt timeout not enforced", time.Since(start))
	}
	if !strings.Contains(out, "aborted") {
		t.Errorf("expected a captured abort message in the streamed frames, got %q", out)
	}
}
