package main

// Tests for the bounded end-confirmation contract (end_confirm.go): an End
// path must return within a bound after its final force kill even when the
// exit watcher never confirms, the session must be RETAINED as a tombstone
// until process absence is VERIFIED (never manufacturing the "not found"
// answer the server frees a device on), and a hung artifact collection must
// not suppress the `*_ended` publish. Regression tests for the 2026-08-27
// HOMETHEATRE wedge, where one unbounded `<-session.done` parked a Pub/Sub
// handler per retried END until the device consumed nothing at all.
//
// These tests were run against the unfixed code first: with the unbounded
// waits restored, the End tests hang until the test binary's own deadline
// kills them — the red-then-green transition is the evidence (rule 23).

import (
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// nopWriteCloser satisfies io.WriteCloser for fake session stdin pipes.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// exitedTestProcess starts and waits a real short-lived process, so End's
// interrupt/kill escalation targets a process that has already finished
// (interruptProcess and Kill both error and both errors are logged/ignored),
// and defaultProbeProcessGone sees a reaped ProcessState.
func exitedTestProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "exit", "0")
	} else {
		cmd = exec.Command("true")
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start helper process: %v", err)
	}
	_ = cmd.Wait()
	return cmd
}

// liveTestProcess starts a real long-lived process WITHOUT waiting it, so the
// OS-level probe sees a genuinely running child. Cleaned up on test exit.
func liveTestProcess(t *testing.T) *exec.Cmd {
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
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

func shortenEndConfirmTimeouts(t *testing.T) {
	t.Helper()
	prevKill, prevTurn := killConfirmTimeout, turnDrainConfirmTimeout
	killConfirmTimeout = 200 * time.Millisecond
	turnDrainConfirmTimeout = 200 * time.Millisecond
	t.Cleanup(func() {
		killConfirmTimeout = prevKill
		turnDrainConfirmTimeout = prevTurn
	})
}

// stubProbe pins probeProcessGone's verdict so tombstone resolution can be
// exercised deterministically in both directions.
func stubProbe(t *testing.T, gone bool) {
	t.Helper()
	prev := probeProcessGone
	probeProcessGone = func(_ *exec.Cmd) bool { return gone }
	t.Cleanup(func() { probeProcessGone = prev })
}

func TestWaitDoneConfirm(t *testing.T) {
	closed := make(chan struct{})
	close(closed)
	if !waitDoneConfirm(closed, time.Second) {
		t.Fatalf("closed channel should confirm immediately")
	}
	open := make(chan struct{})
	start := time.Now()
	if waitDoneConfirm(open, 100*time.Millisecond) {
		t.Fatalf("open channel must not confirm")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("waitDoneConfirm overshot its bound: %s", time.Since(start))
	}
}

func TestWaitTurnBarrier(t *testing.T) {
	var free sync.Mutex
	if !waitTurnBarrier(&free, time.Second) {
		t.Fatalf("free mutex should pass the barrier")
	}
	// Barrier must have RELEASED the mutex, not kept it.
	if !free.TryLock() {
		t.Fatalf("barrier leaked the mutex lock")
	}
	free.Unlock()

	var held sync.Mutex
	held.Lock()
	defer held.Unlock()
	start := time.Now()
	if waitTurnBarrier(&held, 150*time.Millisecond) {
		t.Fatalf("held mutex must time out")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("waitTurnBarrier overshot its bound: %s", time.Since(start))
	}
}

func TestDefaultProbeProcessGone(t *testing.T) {
	// No process at all → gone.
	if !defaultProbeProcessGone(nil) || !defaultProbeProcessGone(&exec.Cmd{}) {
		t.Fatalf("nil/unstarted cmd should probe gone")
	}
	// Exited and reaped (ProcessState set) → gone.
	if !defaultProbeProcessGone(exitedTestProcess(t)) {
		t.Fatalf("exited+waited process should probe gone")
	}
	// Genuinely running, unreaped → alive (fail closed).
	if defaultProbeProcessGone(liveTestProcess(t)) {
		t.Fatalf("live process must NOT probe gone")
	}
}

func TestBoundedArtifactCollect_TimesOutAndPassesThrough(t *testing.T) {
	// Pass-through: a fast collector's results survive intact.
	files, errs, timedOut := boundedArtifactCollect(func() ([]FileInfo, []UploadError) {
		return []FileInfo{{Name: "a.png"}}, []UploadError{{File: "b.png", Error: "x"}}
	}, time.Second, "s1")
	if timedOut || len(files) != 1 || len(errs) != 1 {
		t.Fatalf("pass-through failed: files=%d errs=%d timedOut=%v", len(files), len(errs), timedOut)
	}

	// Timeout: a wedged collector must not block the ended publish path.
	block := make(chan struct{})
	defer close(block)
	start := time.Now()
	files, errs, timedOut = boundedArtifactCollect(func() ([]FileInfo, []UploadError) {
		<-block
		return nil, nil
	}, 100*time.Millisecond, "s2")
	if !timedOut || files != nil || errs != nil {
		t.Fatalf("expected timeout with nil results; got files=%v errs=%v timedOut=%v", files, errs, timedOut)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("boundedArtifactCollect overshot its bound: %s", time.Since(start))
	}
}

func newWedgedCodexSession(t *testing.T) (*CodexAppServerManager, *CodexAppServerSession) {
	t.Helper()
	m := NewCodexAppServerManager(nil)
	session := &CodexAppServerSession{
		ID:         "wedged",
		Process:    exitedTestProcess(t),
		Stdin:      nopWriteCloser{},
		StartedAt:  time.Now(),
		status:     "running",
		done:       make(chan struct{}), // never closed — wedged exit watcher
		streamDone: make(chan struct{}),
	}
	m.sessions["wedged"] = session
	return m, session
}

// The direct regression test for the HOMETHEATRE wedge: session.done never
// closes, and End must still return within a bound — RETAINING the session as
// a tombstone (an earlier revision deregistered immediately, which
// manufactured the absence answer for a possibly-alive child — Codex P1).
// A follow-up End then VERIFIES process absence (the helper process here has
// exited and been reaped) and only then reports "not found".
func TestCodexAppServerEnd_KillUnconfirmed_TombstoneThenVerifiedAbsence(t *testing.T) {
	shortenEndConfirmTimeouts(t)
	m, session := newWedgedCodexSession(t)

	// End escalates: stdin close (5s) -> interrupt (5s) -> kill -> bounded wait.
	resultCh := make(chan error, 1)
	go func() { resultCh <- m.End("wedged") }()

	var err error
	select {
	case err = <-resultCh:
	case <-time.After(2*codexAppServerGracefulShutdownTimeout + 30*time.Second):
		t.Fatalf("End never returned — the unbounded post-kill wait is back")
	}
	if err == nil || !strings.Contains(err.Error(), "kill unconfirmed") {
		t.Fatalf("expected a kill-unconfirmed error, got %v", err)
	}
	if strings.Contains(err.Error(), "not found") {
		t.Fatalf("kill-unconfirmed error must NOT read as the absence answer: %v", err)
	}
	// The session is RETAINED — no absence evidence was manufactured.
	if m.Get("wedged") != session {
		t.Fatalf("session must be retained as a tombstone after an unconfirmed kill")
	}
	if !session.isKillUnconfirmed() {
		t.Fatalf("session should be marked kill-unconfirmed")
	}

	// Follow-up End: the child is verifiably gone (exited + reaped), so the
	// tombstone resolves into the true absence answer and the session drops.
	err = m.End("wedged")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("follow-up End should report verified absence; got %v", err)
	}
	if m.Get("wedged") != nil {
		t.Fatalf("session should be removed once absence is verified")
	}
}

// While the probe says the child is still alive, repeated Ends must keep the
// fence up: kill-unconfirmed again, session retained, never "not found".
func TestCodexAppServerEnd_TombstoneHeldWhileProcessAlive(t *testing.T) {
	shortenEndConfirmTimeouts(t)
	stubProbe(t, false)
	m, session := newWedgedCodexSession(t)
	session.markKillUnconfirmed()

	for i := 0; i < 2; i++ {
		err := m.End("wedged")
		if err == nil || !strings.Contains(err.Error(), "kill unconfirmed") {
			t.Fatalf("attempt %d: expected kill-unconfirmed, got %v", i, err)
		}
		if strings.Contains(err.Error(), "not found") {
			t.Fatalf("attempt %d: absence answer manufactured while probe says alive: %v", i, err)
		}
		if m.Get("wedged") != session {
			t.Fatalf("attempt %d: tombstone must be retained while the process is alive", i)
		}
	}
}

func TestSessionManagerEndSession_KillUnconfirmed_TombstoneThenVerifiedAbsence(t *testing.T) {
	shortenEndConfirmTimeouts(t)
	sm := NewSessionManager(nil)
	session := &CLISession{
		ID:        "wedged-pty",
		Process:   exitedTestProcess(t),
		StartedAt: time.Now(),
		Status:    "running",
		done:      make(chan struct{}), // never closed
	}
	sm.mu.Lock()
	sm.sessions["wedged-pty"] = session
	sm.mu.Unlock()

	resultCh := make(chan error, 1)
	go func() { resultCh <- sm.EndSession("wedged-pty") }()

	var err error
	select {
	case err = <-resultCh:
	case <-time.After(gracefulShutdownTimeout + 30*time.Second):
		t.Fatalf("EndSession never returned — the unbounded post-kill wait is back")
	}
	if err == nil || !strings.Contains(err.Error(), "kill unconfirmed") {
		t.Fatalf("expected kill-unconfirmed error, got %v", err)
	}
	if sm.GetSession("wedged-pty") != session {
		t.Fatalf("session must be retained as a tombstone after an unconfirmed kill")
	}

	// Verified absence on the follow-up end.
	err = sm.EndSession("wedged-pty")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("follow-up EndSession should report verified absence; got %v", err)
	}
	if sm.GetSession("wedged-pty") != nil {
		t.Fatalf("session should be removed once absence is verified")
	}
}

// Turn-per-process managers: a drain barrier that never clears must not hold
// the END handler hostage. With NO recorded turn process, nothing can be
// writing to the checkout, so the tombstone resolves immediately.
func TestAntigravityEnd_TurnDrainUnconfirmed_NoProcessResolvesAbsent(t *testing.T) {
	shortenEndConfirmTimeouts(t)
	m := NewAntigravityNativeManager(nil)
	session := &AntigravityNativeSession{
		ID:        "wedged-ag",
		StartedAt: time.Now(),
		status:    "ended", // already-ended path still drains on turnMu
	}
	session.turnMu.Lock() // a wedged in-flight turn holds the barrier forever
	defer session.turnMu.Unlock()
	m.sessions["wedged-ag"] = session

	resultCh := make(chan error, 1)
	go func() { resultCh <- m.End("wedged-ag") }()

	var err error
	select {
	case err = <-resultCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("End never returned — the unbounded turn barrier is back")
	}
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("no recorded turn process — expected verified-absent resolution, got %v", err)
	}
	if m.Get("wedged-ag") != nil {
		t.Fatalf("session should be removed once resolved absent")
	}
}

// With a live recorded turn process, the tombstone must HOLD: drain-unconfirmed
// error, session retained, no absence answer until the process is gone.
func TestAntigravityEnd_TurnDrainUnconfirmed_LiveProcessRetains(t *testing.T) {
	shortenEndConfirmTimeouts(t)
	stubProbe(t, false)
	m := NewAntigravityNativeManager(nil)
	session := &AntigravityNativeSession{
		ID:        "wedged-ag",
		StartedAt: time.Now(),
		status:    "ended",
	}
	session.activeProcess = exitedTestProcess(t) // probe stub says alive anyway
	session.turnMu.Lock()
	defer session.turnMu.Unlock()
	m.sessions["wedged-ag"] = session

	err := m.End("wedged-ag")
	if err == nil || !strings.Contains(err.Error(), "turn drain unconfirmed") {
		t.Fatalf("expected turn-drain-unconfirmed error, got %v", err)
	}
	if strings.Contains(err.Error(), "not found") {
		t.Fatalf("absence answer manufactured while probe says alive: %v", err)
	}
	if m.Get("wedged-ag") != session {
		t.Fatalf("tombstone must be retained while the turn process is alive")
	}

	// Once the process is verifiably gone, the next End resolves.
	stubProbe(t, true)
	err = m.End("wedged-ag")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected verified-absent resolution, got %v", err)
	}
	if m.Get("wedged-ag") != nil {
		t.Fatalf("session should be removed once resolved absent")
	}
}

func TestOpenCodeEnd_TurnDrainUnconfirmed_LiveProcessRetains(t *testing.T) {
	shortenEndConfirmTimeouts(t)
	stubProbe(t, false)
	m := NewOpenCodeNativeManager()
	session := &OpenCodeNativeSession{
		ID:        "wedged-oc",
		StartedAt: time.Now(),
		status:    "ended",
	}
	session.activeProcess = exitedTestProcess(t)
	session.turnMu.Lock()
	defer session.turnMu.Unlock()
	m.sessions["wedged-oc"] = session

	err := m.End("wedged-oc")
	if err == nil || !strings.Contains(err.Error(), "turn drain unconfirmed") {
		t.Fatalf("expected turn-drain-unconfirmed error, got %v", err)
	}
	if m.Get("wedged-oc") != session {
		t.Fatalf("tombstone must be retained while the turn process is alive")
	}

	stubProbe(t, true)
	err = m.End("wedged-oc")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected verified-absent resolution, got %v", err)
	}
	if m.Get("wedged-oc") != nil {
		t.Fatalf("session should be removed once resolved absent")
	}
}

// The reused-ID watcher race (Codex P2): once a tombstone resolves and its ID
// is re-used by a new Start, the OLD wedged exit watcher must not delete the
// replacement session when it eventually recovers.
func TestRemoveSessionIfSame_DoesNotDeleteReplacement(t *testing.T) {
	m := NewCodexAppServerManager(nil)
	old := &CodexAppServerSession{ID: "s", status: "running", done: make(chan struct{})}
	replacement := &CodexAppServerSession{ID: "s", status: "running", done: make(chan struct{})}
	m.sessions["s"] = replacement

	// The old watcher's removal must be a no-op against the replacement…
	m.removeSessionIfSame("s", old)
	if m.Get("s") != replacement {
		t.Fatalf("stale watcher removal deleted the replacement session")
	}
	// …while the replacement's own removal still works.
	m.removeSessionIfSame("s", replacement)
	if m.Get("s") != nil {
		t.Fatalf("identity-matched removal should remove the session")
	}
}
