//go:build !windows
// +build !windows

// File: cliagent_smoke_opencode_unix_test.go
// Unix-specific proof that an expired OpenCode smoke reaps the child's tool
// descendants, not just the child: the probe must start the child as its own
// process-group leader so killOpenCodeProcessTree's group kill reaches them.
package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunOpenCodeSmokeCommand_DeadlineKillsToolDescendant(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "tool.pid")
	stub := filepath.Join(dir, "opencode")
	// A fake `opencode` that spawns a long-lived "tool" child holding the
	// captured pipes, records its pid, then blocks past the deadline.
	script := "#!/bin/sh\nsleep 60 &\necho $! > \"$AIX_TOOL_PID_FILE\"\nwait\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	promptFile := filepath.Join(dir, "prompt")
	if err := os.WriteFile(promptFile, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, _, started, err := runOpenCodeSmokeCommand(ctx, openCodeLaunch{
		Path:       stub,
		Args:       []string{"run", "--format", "json"},
		Env:        append(os.Environ(), "AIX_TOOL_PID_FILE="+pidFile),
		Dir:        dir,
		PromptFile: promptFile,
	})
	if err == nil {
		t.Fatalf("expected the deadline to end the run with an error")
	}
	if !started {
		t.Fatalf("the child reached Start, so the seam must report it started")
	}

	raw, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("stub never recorded its tool pid: %v", readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if convErr != nil || pid <= 0 {
		t.Fatalf("bad tool pid %q", raw)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	// The orphaned tool is reparented and reaped asynchronously; allow a moment.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if !processAlive(pid) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("tool descendant %d survived the smoke deadline", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// processAlive reports whether pid is still running. A zombie counts as dead:
// in a container whose init does not reap, the killed orphan lingers as <defunct>.
func processAlive(pid int) bool {
	if syscall.Kill(pid, 0) == syscall.ESRCH {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	stat := strings.TrimSpace(string(out))
	return stat != "" && !strings.HasPrefix(stat, "Z")
}
