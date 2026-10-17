package main

// Tests for the generic-session half of the shutdown teardown
// (PARALLEL_RUNS_AND_COMPUTER_MAINTENANCE_PLAN.md §2 "Process hygiene", §9.5
// "Orphan processes"): a tray Quit must end the session_start children THIS
// process started.
//
// SessionManager.ShutdownAllSessions existed and documented itself as "called
// during application shutdown", but nothing called it, so every pipe/PTY CLI
// survived the Quit. On Windows the session's kill-on-close Job Object closed
// with the agent; on macOS and Linux nothing contained it, and the CLI kept
// drawing on the account's allowance until a later boot's ledger reap found it.
//
// Run against the unfixed code first: TestTearDownSubprocessesEndsGenericCLISessions
// fails (the child is still running afterwards) and TestShutdownAllSessionsIsBounded
// overshoots its bound on the serial EndSession loop — the red-then-green
// transition is the evidence.

import (
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// shortenSessionShutdownBudget keeps these tests quick without changing what
// they prove: one shared budget is spent, then the stragglers are killed.
//
// Only this one knob is shortened. The end-confirm timeouts are deliberately
// left alone: the EndSession goroutines ShutdownAllSessions abandons when the
// budget runs out still read killConfirmTimeout, and no test here can wait
// for them, so shortening it would mean restoring the global under a live
// reader — a data race, and -race runs this package in CI.
func shortenSessionShutdownBudget(t *testing.T, d time.Duration) {
	t.Helper()
	prev := sessionShutdownBudget
	sessionShutdownBudget = d
	t.Cleanup(func() { sessionShutdownBudget = prev })
}

// withGlobalSessionManager installs sm as the process-wide manager
// tearDownSubprocesses reads, and restores the previous one.
func withGlobalSessionManager(t *testing.T, sm *SessionManager) {
	t.Helper()
	prev := globalSessionManager
	globalSessionManager = sm
	t.Cleanup(func() { globalSessionManager = prev })
}

// isolateTearDownGlobals detaches the process-wide singletons
// tearDownSubprocesses would otherwise tear down, so calling it here cannot
// end a resident CLI session or close a ttyd another test in this package set
// up. Only the generic session path is left connected — that is what is under
// test.
func isolateTearDownGlobals(t *testing.T) {
	t.Helper()
	codex, grok, claude := globalCodexAppServerManager, globalGrokACPManager, globalClaudeNativeManager
	agy, opencode, muse := globalAntigravityNativeManager, globalOpenCodeNativeManager, globalMuseCodeNativeManager
	ttyd := ttydCmd
	globalCodexAppServerManager, globalGrokACPManager, globalClaudeNativeManager = nil, nil, nil
	globalAntigravityNativeManager, globalOpenCodeNativeManager, globalMuseCodeNativeManager = nil, nil, nil
	ttydCmd = nil
	t.Cleanup(func() {
		globalCodexAppServerManager, globalGrokACPManager, globalClaudeNativeManager = codex, grok, claude
		globalAntigravityNativeManager, globalOpenCodeNativeManager, globalMuseCodeNativeManager = agy, opencode, muse
		ttydCmd = ttyd
	})
}

// reapedTestProcess starts a real long-lived child and reaps it in the
// background, the way waitForExit does in production. The returned channel
// closes when the child is actually gone, which is the only cross-platform
// liveness answer available here: on Unix a killed-but-unreaped child is a
// zombie that processHandleGone still reads as alive.
func reapedTestProcess(t *testing.T) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "ping", "-n", "60", "127.0.0.1")
	} else {
		cmd = exec.Command("sleep", "60")
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start helper process: %v", err)
	}
	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-reaped
	})
	return cmd, reaped
}

// liveSession registers a real long-lived child as a running session whose
// exit watcher never signals, so only an actual kill ends it.
func liveSession(t *testing.T, sm *SessionManager, id string) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	cmd, reaped := reapedTestProcess(t)
	session := &CLISession{
		ID:            id,
		Process:       cmd,
		StartedAt:     time.Now(),
		Status:        "running",
		processExited: make(chan struct{}),
		done:          make(chan struct{}),
		streamDone:    make(chan struct{}),
	}
	sm.mu.Lock()
	sm.sessions[id] = session
	sm.mu.Unlock()
	return cmd, reaped
}

func goneWithin(reaped <-chan struct{}, within time.Duration) bool {
	select {
	case <-reaped:
		return true
	case <-time.After(within):
		return false
	}
}

// The wiring itself: tearDownSubprocesses is what every gracefulShutdown
// entry point reaches, and before this change it never touched a generic
// session. Its remaining steps are nil-safe with no agent running
// (ShutdownPowerShell, CloseStorageClient, a tmux kill-session that simply
// fails), so the live child is the only thing under test.
func TestTearDownSubprocessesEndsGenericCLISessions(t *testing.T) {
	shortenSessionShutdownBudget(t, 200*time.Millisecond)
	isolateTearDownGlobals(t)
	sm := NewSessionManager(nil)
	withGlobalSessionManager(t, sm)
	cmd, reaped := liveSession(t, sm, "quit-orphan")

	tearDownSubprocesses()

	if !goneWithin(reaped, 10*time.Second) {
		t.Fatalf("tray Quit left the session's CLI child (pid %d) running", cmd.Process.Pid)
	}
}

// A Quit that hangs is worse than an orphan: EndSession alone can spend
// gracefulShutdownTimeout + killConfirmTimeout per session, and the Windows
// console handlers give the WHOLE shutdown 4s (close) or 2s (logoff). So the
// interrupts go out together, share one budget, and the stragglers are killed
// after it with no further wait.
func TestShutdownAllSessionsIsBounded(t *testing.T) {
	shortenSessionShutdownBudget(t, 200*time.Millisecond)
	sm := NewSessionManager(nil)
	reaped := make([]<-chan struct{}, 0, 3)
	for _, id := range []string{"straggler-a", "straggler-b", "straggler-c"} {
		_, r := liveSession(t, sm, id)
		reaped = append(reaped, r)
	}

	start := time.Now()
	returned := make(chan struct{})
	go func() {
		sm.ShutdownAllSessions()
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(gracefulShutdownTimeout + 30*time.Second):
		t.Fatalf("ShutdownAllSessions never returned — the unbounded serial loop is back")
	}
	// Three sessions share ONE budget rather than taking one each, and no
	// post-kill confirmation wait is stacked on top.
	if elapsed := time.Since(start); elapsed > gracefulShutdownTimeout {
		t.Fatalf("ShutdownAllSessions overshot its budget: %s for %d sessions", elapsed, len(reaped))
	}
	for i, r := range reaped {
		if !goneWithin(r, 10*time.Second) {
			t.Fatalf("session %d's child survived the bounded shutdown", i)
		}
	}
}

// Ownership discipline (plan §5.5): only the sessions this process started and
// still holds are ended. A live child the manager does not hold is never
// reached, because nothing here enumerates the process table.
func TestShutdownAllSessionsTouchesNothingItDoesNotHold(t *testing.T) {
	shortenSessionShutdownBudget(t, 200*time.Millisecond)
	sm := NewSessionManager(nil)
	stranger, strangerReaped := reapedTestProcess(t)

	start := time.Now()
	sm.ShutdownAllSessions()
	if elapsed := time.Since(start); elapsed >= sessionShutdownBudget {
		t.Fatalf("a manager holding no session must return at once, took %s", elapsed)
	}
	if goneWithin(strangerReaped, 500*time.Millisecond) {
		t.Fatalf("a process the agent never started was ended (pid %d)", stranger.Process.Pid)
	}
}
