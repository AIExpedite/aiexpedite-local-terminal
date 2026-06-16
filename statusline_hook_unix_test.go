// File: statusline_hook_unix_test.go
//go:build !windows

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRenderStatusLine_TerminatesChainedChildOnSignal pins the cancellation
// behavior the chained-command Codex P2 thread asked for: when Claude cancels
// an in-flight render (SIGTERM to the wrapper), the spawned shell child must
// die with us instead of being orphaned across renders.
func TestRenderStatusLine_TerminatesChainedChildOnSignal(t *testing.T) {
	dir := t.TempDir()
	prevPath := filepath.Join(dir, "prev.json")
	pidFile := filepath.Join(dir, "child.pid")

	// Stash a command that records its pgid and then sleeps long enough that —
	// if the wrapper failed to forward SIGTERM — the test would time out. We
	// record `$$` (the shell's pid, which is also its pgid since Setpgid makes
	// it the leader) so we can verify the whole group, not just the leader,
	// dies after the signal.
	stash := `{"type":"command","command":"echo $$ > ` + pidFile + ` && sleep 60"}`
	t.Setenv("AIEXPEDITE_CLAUDE_STATUSLINE_PREV", prevPath)
	if err := savePrevStatusLine(json.RawMessage(stash)); err != nil {
		t.Fatalf("savePrevStatusLine: %v", err)
	}

	done := make(chan struct{})
	go func() {
		// Swallow the default-line stdout the wrapper would normally print after
		// the signal path — we only care about the lifecycle here.
		oldStdout := os.Stdout
		devNull, _ := os.Open(os.DevNull)
		os.Stdout = devNull
		defer func() {
			os.Stdout = oldStdout
			if devNull != nil {
				_ = devNull.Close()
			}
		}()
		renderStatusLine([]byte(`{}`))
		close(done)
	}()

	var leaderPid int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil {
			if p, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && p > 0 {
				leaderPid = p
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if leaderPid == 0 {
		t.Fatal("chained child never wrote its pid — never started?")
	}

	// SIGTERM ourselves; signal.Notify inside renderStatusLine should catch it
	// and relay to the child's process group (`kill(-pgid, SIGTERM)`).
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("self-SIGTERM: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("renderStatusLine never returned after SIGTERM — signal not caught")
	}

	// Give the kernel a beat to reap; then verify the group is gone. kill(pid, 0)
	// returns ESRCH once the process (and its group) is dead.
	gone := false
	for i := 0; i < 50; i++ {
		if err := syscall.Kill(leaderPid, 0); err != nil {
			gone = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !gone {
		// Best-effort cleanup so we don't leak a 60s sleep across the suite.
		_ = syscall.Kill(-leaderPid, syscall.SIGKILL)
		t.Errorf("chained child pgid %d still alive after wrapper got SIGTERM", leaderPid)
	}
}
