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
//
// The three tests below the ownership test cover the review round on that
// change, and each was likewise run against the behaviour it replaces:
//
//   - the caller's deadline: with teardown passing sessionShutdownBudget
//     instead of sessionShutdownBudgetFor(ctx, …),
//     TestTearDownSubprocessesKillsAtOnceOnceTheCallersDeadlineIsGone spends
//     the whole 30s window past an expired deadline;
//   - the closed manager: without the sm.closed check in startSession, both
//     TestShutdownAllSessionsClosesTheManagerToNewStarts and
//     TestShutdownRefusesStartsWhileTheGracefulWindowIsStillOpen really do
//     spawn a child after the snapshot (err == nil);
//   - the manager lock: without startSession's early unlockManager(),
//     TestStartSessionReleasesTheManagerLockBeforeTheInitialPromptWrite never
//     gets an answer out of the manager while a child sits on an unread
//     prompt.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

	tearDownSubprocesses(context.Background())

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

// mockCLIOnPath copies the test binary into a tempdir under sessionCmd's name
// and prepends that dir to PATH, so resolveExecutable finds it and TestMain
// dispatches the child into runMockCLI's `mode`. Same harness as
// captureSessionWithConfig in session_integration_test.go; the returned
// directory doubles as the session's cwd.
func mockCLIOnPath(t *testing.T, sessionCmd, mode string) string {
	t.Helper()
	testExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	dir := t.TempDir()
	name := sessionCmd
	if runtime.GOOS == "windows" && !strings.HasSuffix(name, ".exe") {
		name += ".exe"
	}
	if err := copyTestBinary(testExe, filepath.Join(dir, name)); err != nil {
		t.Fatalf("copy test binary: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(mockCLIEnvVar, mode)
	// `claude` resolves through a process-wide memo (cachedResolveClaudePath),
	// so without this an earlier test's shim — whose tempdir is already gone —
	// wins over the one just installed. Drop the memo on the way in and out.
	resetCachedClaudePath()
	t.Cleanup(resetCachedClaudePath)
	return dir
}

// activeSessionCountWithin reads ActiveSessionCount from a goroutine and
// reports whether an answer arrived at all, so a manager lock held across a
// blocked child write FAILS the test rather than hanging it — which is exactly
// how the pre-fix code behaved. A short block is legitimate (startSession holds
// the write lock across the child's launch), so the caller retries.
func activeSessionCountWithin(sm *SessionManager, within time.Duration) (int, bool) {
	got := make(chan int, 1)
	go func() { got <- sm.ActiveSessionCount() }()
	select {
	case n := <-got:
		return n, true
	case <-time.After(within):
		return 0, false
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

/* --------------------------------------------------------------------------
   The caller's deadline (Codex P1)
   --------------------------------------------------------------------------
   The budget above is not the deadline. notifyOffline and the usage drains run
   BEFORE teardown and spend from the same window, and the Windows console
   handler gives gracefulShutdown 4s on CTRL_CLOSE_EVENT and only 2s on
   CTRL_LOGOFF/CTRL_SHUTDOWN. A fresh, context-independent graceful window could
   therefore run past the point where Windows kills the agent — and a force path
   that never executes leaves exactly the orphan this teardown removes.
   -------------------------------------------------------------------------- */

func TestSessionShutdownBudgetForRespectsTheCallersRemainingDeadline(t *testing.T) {
	shortenSessionShutdownBudget(t, 2*time.Second)

	// Each context is built inside its own subtest, from the same `now` the
	// assertion passes in: the arithmetic is then exact, and a context whose
	// deadline is milliseconds away cannot expire for real in the gap between
	// construction and the call.
	withDeadline := func(d time.Duration) func(*testing.T, time.Time) context.Context {
		return func(t *testing.T, now time.Time) context.Context {
			ctx, cancel := context.WithDeadline(context.Background(), now.Add(d))
			t.Cleanup(cancel)
			return ctx
		}
	}
	noDeadline := func(*testing.T, time.Time) context.Context { return context.Background() }
	cancelled := func(*testing.T, time.Time) context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	cases := []struct {
		name  string
		build func(*testing.T, time.Time) context.Context
		want  time.Duration
	}{
		{"a nil context has no deadline to respect", nil, 2 * time.Second},
		{"a context without a deadline leaves the full bound", noDeadline, 2 * time.Second},
		{"a generous deadline leaves the full bound", withDeadline(10 * time.Second), 2 * time.Second},
		// Half, so the resident CLI managers, ttyd and tmux that follow in
		// tearDownSubprocesses keep the other half — the same split
		// antigravityShutdownDrainBudget makes with notifyOffline.
		{"a tight deadline leaves half of what is left", withDeadline(2 * time.Second), time.Second},
		{"a nearly spent deadline leaves nearly nothing", withDeadline(100 * time.Millisecond), 50 * time.Millisecond},
		{"an expired deadline leaves no graceful window", withDeadline(-time.Second), 0},
		{"a cancelled context leaves no graceful window", cancelled, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			var ctx context.Context
			if tc.build != nil {
				ctx = tc.build(t, now)
			}
			if got := sessionShutdownBudgetFor(ctx, now); got != tc.want {
				t.Fatalf("sessionShutdownBudgetFor = %s, want %s", got, tc.want)
			}
		})
	}
}

// The behaviour that bound protects: once the caller's deadline is gone there
// is no window left to wait in, so teardown kills the children NOW instead of
// spending a budget this process will not survive.
//
// The graceful budget here is deliberately 30s — far longer than the assertion
// allows — so a teardown that started its own window would blow the bound
// rather than merely shave it.
func TestTearDownSubprocessesKillsAtOnceOnceTheCallersDeadlineIsGone(t *testing.T) {
	shortenSessionShutdownBudget(t, 30*time.Second)
	isolateTearDownGlobals(t)
	sm := NewSessionManager(nil)
	withGlobalSessionManager(t, sm)
	cmd, reaped := liveSession(t, sm, "logoff-orphan")

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	start := time.Now()
	tearDownSubprocesses(ctx)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("teardown spent %s past the caller's expired deadline — Windows kills the agent before the force path runs", elapsed)
	}
	if !goneWithin(reaped, 10*time.Second) {
		t.Fatalf("the session's CLI child (pid %d) survived a deadline-expired teardown", cmd.Process.Pid)
	}
}

/* --------------------------------------------------------------------------
   No session may start after the snapshot (Codex P1)
   --------------------------------------------------------------------------
   ShutdownAllSessionsWithin decides from one snapshot which children get
   ended. A Pub/Sub session_start that passed the offline guard before shutdown
   and is still in approval or preflight work can reach StartSession after it —
   and that child is in no snapshot and, on macOS and Linux, in no containment
   either, so the agent exits leaving an orphan. The manager is therefore closed
   to new starts in the SAME hold of the mutex that takes the snapshot.
   -------------------------------------------------------------------------- */

func TestShutdownAllSessionsClosesTheManagerToNewStarts(t *testing.T) {
	shortenSessionShutdownBudget(t, 200*time.Millisecond)
	// A mock that exits immediately: a run against the unfixed code really
	// does spawn this child, and must not leave one behind.
	dir := mockCLIOnPath(t, "claude", "no-prompt-immediate-exit")
	sm := NewSessionManager(nil)

	// An empty snapshot is the race's own shape: nothing to end, and the start
	// arrives afterwards.
	sm.ShutdownAllSessions()

	err := sm.StartSession("after-the-snapshot", "claude", []string{"hello"},
		dir, "ws-test", "uid-test", 0, false, func(resultMsg) {})
	if err == nil {
		t.Fatal("StartSession after the shutdown snapshot must be refused, not spawn an uncontained child")
	}
	if !errors.Is(err, errSessionManagerClosed) {
		t.Fatalf("the refusal must name the closed manager, got: %v", err)
	}
	if n := sm.ActiveSessionCount(); n != 0 {
		t.Fatalf("%d session(s) registered after the shutdown snapshot", n)
	}
}

// Closing happens WITH the snapshot, not after the graceful window: a start
// arriving while the interrupts are still out must already be refused.
// (A session whose exit watcher never signals keeps EndSession in its graceful
// wait for the whole window, which is what makes this deterministic.)
func TestShutdownRefusesStartsWhileTheGracefulWindowIsStillOpen(t *testing.T) {
	window := 2 * time.Second
	shortenSessionShutdownBudget(t, window)
	dir := mockCLIOnPath(t, "claude", "no-prompt-immediate-exit")
	sm := NewSessionManager(nil)
	liveSession(t, sm, "straggler")

	done := make(chan struct{})
	go func() {
		sm.ShutdownAllSessionsWithin(window)
		close(done)
	}()
	// Well inside the window: the snapshot has been taken and the graceful
	// interrupts are still outstanding.
	time.Sleep(200 * time.Millisecond)

	err := sm.StartSession("during-the-window", "claude", []string{"hello"},
		dir, "ws-test", "uid-test", 0, false, func(resultMsg) {})
	if !errors.Is(err, errSessionManagerClosed) {
		t.Fatalf("a start during the graceful window must be refused, got: %v", err)
	}

	select {
	case <-done:
	case <-time.After(gracefulShutdownTimeout + 30*time.Second):
		t.Fatal("ShutdownAllSessionsWithin never returned")
	}
}

/* --------------------------------------------------------------------------
   The manager lock is not held across the child's initial prompt write
   (Codex P2)
   --------------------------------------------------------------------------
   startSession held sm.mu from the duplicate-ID check through process launch
   AND the initial stdin write. A child that does not consume a prompt larger
   than its pipe buffer blocks that write for as long as it lives — and with mu
   held, every other session operation blocks with it, the shutdown snapshot
   included. So Quit could wait indefinitely despite its advertised bound.

   The deadline now also starts before the lock is acquired, so the window
   bounds the whole operation; this test pins the half that removes the starvation
   itself.
   -------------------------------------------------------------------------- */

func TestStartSessionReleasesTheManagerLockBeforeTheInitialPromptWrite(t *testing.T) {
	dir := mockCLIOnPath(t, "claude", "stdin-deaf-hang")
	sm := NewSessionManager(nil)
	// The mock sleeps for ten minutes, so a test that fails before the
	// shutdown below must not leave it behind. A zero window kills whatever
	// the manager still holds, and an empty manager returns at once. Bounded,
	// because this is also the cleanup that runs when the failure under test
	// IS a manager lock nobody will ever release.
	t.Cleanup(func() {
		swept := make(chan struct{})
		go func() {
			sm.ShutdownAllSessionsWithin(0)
			close(swept)
		}()
		select {
		case <-swept:
		case <-time.After(5 * time.Second):
		}
	})

	// 2 MiB of prompt is far past any pipe buffer (64 KiB on Linux and macOS,
	// 4 KiB by default on Windows) and this child never reads stdin, so the
	// write blocks until the child dies. Claude's prompt words travel on stdin
	// and never on argv (buildClaudeInteractiveArgs), so no CreateProcess
	// command-line cap applies.
	prompt := strings.Repeat("x", 2<<20)
	started := make(chan error, 1)
	go func() {
		var err error
		for attempt := 0; attempt < 5; attempt++ {
			err = sm.StartSession("wedged-write", "claude", []string{prompt},
				dir, "ws-test", "uid-test", 0, false, func(resultMsg) {})
			// Same transient ETXTBSY retry captureSessionWithConfig documents:
			// we exec a freshly written copy of the test binary.
			if err == nil || !strings.Contains(err.Error(), "text file busy") {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		started <- err
	}()

	// The session is registered before the prompt is written, so once the
	// manager reports it the write is in flight. Pre-fix no read ever returned,
	// because startSession held the write lock from the duplicate-ID check
	// through that write.
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case err := <-started:
			t.Fatalf("StartSession returned before its child was running: %v", err)
		default:
		}
		if n, answered := activeSessionCountWithin(sm, 2*time.Second); answered && n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the manager never reported the session: either the lock is held across the child's initial prompt write, or the child never started")
		}
		time.Sleep(25 * time.Millisecond)
	}

	// What that lock used to block: the Quit snapshot. It must complete inside
	// its window, and the wedged child must die with it.
	returned := make(chan struct{})
	go func() {
		sm.ShutdownAllSessionsWithin(300 * time.Millisecond)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(20 * time.Second):
		t.Fatal("ShutdownAllSessionsWithin never returned — the manager lock is held across the child's initial prompt write")
	}

	// The child is gone, so the abandoned write fails and the start returns.
	select {
	case <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("StartSession never returned after its child was killed")
	}
}
