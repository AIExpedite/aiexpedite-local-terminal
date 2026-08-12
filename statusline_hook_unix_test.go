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

	// Swallow the default-line stdout the wrapper would normally print after
	// the signal path — we only care about the lifecycle here.
	//
	// os.Stderr is redirected for a second, load-bearing reason: renderStatusLine
	// hands the chained child `cmd.Stdout = os.Stdout` / `cmd.Stderr = os.Stderr`,
	// i.e. the *test binary's own* stdio descriptors — this is the only place in
	// the suite that does. `go test` waits for those descriptors to reach EOF
	// after the tests exit, so if the `sleep 60` here ever outlives us (the
	// group kill below is best-effort, and the assertion only probes the group
	// leader), the whole package fails with
	//
	//     *** Test I/O incomplete 30s after exiting.
	//     exec: WaitDelay expired before I/O complete
	//
	// long after every test reported PASS. Pointing both at files under
	// t.TempDir() means a straggler holds a temp file instead of the test
	// binary's pipes, so the leak can no longer take the package down with it.
	oldStdout, oldStderr := os.Stdout, os.Stderr
	childOut, err := os.Create(filepath.Join(dir, "render.out"))
	if err != nil {
		t.Fatalf("create render.out: %v", err)
	}
	childErr, err := os.Create(filepath.Join(dir, "render.err"))
	if err != nil {
		t.Fatalf("create render.err: %v", err)
	}
	os.Stdout, os.Stderr = childOut, childErr
	t.Cleanup(func() {
		os.Stdout, os.Stderr = oldStdout, oldStderr
		_ = childOut.Close()
		_ = childErr.Close()
	})

	done := make(chan struct{})
	go func() {
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
	// Unconditional backstop: reap the whole group at test end no matter which
	// way the assertions below go. The success path leaves nothing to kill, but
	// if the wrapper's group kill ever misses a member (e.g. the shell forked
	// `sleep` concurrently with kill(-pgid)), the survivor would otherwise sit
	// around for a full 60s. Registered as Cleanup rather than left to the
	// failure branch so a *passing* run can't leak it either — a leaked child is
	// invisible until the package exits and then fails it as an I/O timeout.
	t.Cleanup(func() { _ = syscall.Kill(-leaderPid, syscall.SIGKILL) })

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
		// The t.Cleanup above handles the actual reaping.
		t.Errorf("chained child pgid %d still alive after wrapper got SIGTERM", leaderPid)
	}
}
