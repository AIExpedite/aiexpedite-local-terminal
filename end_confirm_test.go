package main

// Tests for the bounded end-confirmation contract (end_confirm.go): an End
// path must return within a bound after its final force kill even when the
// exit watcher never confirms, and a hung artifact collection must not
// suppress the `*_ended` publish. Both are regression tests for the
// 2026-08-27 HOMETHEATRE wedge, where one unbounded `<-session.done` parked a
// Pub/Sub handler per retried END until the device consumed nothing at all.
//
// These tests were run against the unfixed code first: with the unbounded
// waits restored, TestCodexAppServerEnd_KillUnconfirmed_ReturnsBounded and
// TestAntigravityEnd_TurnDrainUnconfirmed_ReturnsBounded hang until the test
// binary's own deadline kills them — the red-then-green transition is the
// evidence (rule 23).

import (
	"errors"
	"io"
	"os"
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
// (interruptProcess and Kill both error and both errors are ignored/logged).
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

// TestCodexAppServerEnd_KillUnconfirmed_ReturnsBounded is the direct
// regression test for the HOMETHEATRE wedge: session.done never closes (the
// exit watcher is wedged), and End must still return — with an error that is
// NOT the "session <id> not found" absence answer — and deregister the
// session so the next END produces the real absence answer.
func TestCodexAppServerEnd_KillUnconfirmed_ReturnsBounded(t *testing.T) {
	shortenEndConfirmTimeouts(t)
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

	// End escalates: stdin close (5s) -> interrupt (5s) -> kill -> bounded wait.
	resultCh := make(chan error, 1)
	go func() { resultCh <- m.End("wedged") }()

	var err error
	select {
	case err = <-resultCh:
	case <-time.After(2*codexAppServerGracefulShutdownTimeout + 30*time.Second):
		t.Fatalf("End never returned — the unbounded post-kill wait is back")
	}
	if err == nil {
		t.Fatalf("expected a kill-unconfirmed error, got nil")
	}
	if strings.Contains(err.Error(), "not found") {
		t.Fatalf("kill-unconfirmed error must NOT read as the absence answer (device may still hold the process): %v", err)
	}
	if !strings.Contains(err.Error(), "kill unconfirmed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Get("wedged") != nil {
		t.Fatalf("session must be deregistered so the next END reports absence")
	}

	// The follow-up END is the absence answer the server accepts as evidence.
	err = m.End("wedged")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("follow-up End should report the session absent; got %v", err)
	}
}

// TestSessionManagerEndSession_KillUnconfirmed_ReturnsBounded covers the PTY
// path's identical contract.
func TestSessionManagerEndSession_KillUnconfirmed_ReturnsBounded(t *testing.T) {
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
	if strings.Contains(err.Error(), "not found") {
		t.Fatalf("kill-unconfirmed error must not read as the absence answer: %v", err)
	}
	if sm.GetSession("wedged-pty") != nil {
		t.Fatalf("session must be deregistered so the next end reports absence")
	}
}

// TestAntigravityEnd_TurnDrainUnconfirmed_ReturnsBounded covers the
// turn-per-process managers' barrier: a turn that never drains must not hold
// the END handler hostage.
func TestAntigravityEnd_TurnDrainUnconfirmed_ReturnsBounded(t *testing.T) {
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
	if err == nil || !strings.Contains(err.Error(), "turn drain unconfirmed") {
		t.Fatalf("expected turn-drain-unconfirmed error, got %v", err)
	}
	if strings.Contains(err.Error(), "not found") {
		t.Fatalf("drain-unconfirmed error must not read as the absence answer: %v", err)
	}
	if m.Get("wedged-ag") != nil {
		t.Fatalf("session must be deregistered so the next END reports absence")
	}
}

func TestOpenCodeEnd_TurnDrainUnconfirmed_ReturnsBounded(t *testing.T) {
	shortenEndConfirmTimeouts(t)
	m := NewOpenCodeNativeManager()
	session := &OpenCodeNativeSession{
		ID:        "wedged-oc",
		StartedAt: time.Now(),
		status:    "ended",
	}
	session.turnMu.Lock()
	defer session.turnMu.Unlock()
	m.sessions["wedged-oc"] = session

	resultCh := make(chan error, 1)
	go func() { resultCh <- m.End("wedged-oc") }()

	var err error
	select {
	case err = <-resultCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("End never returned — the unbounded turn barrier is back")
	}
	if err == nil || !strings.Contains(err.Error(), "turn drain unconfirmed") {
		t.Fatalf("expected turn-drain-unconfirmed error, got %v", err)
	}
	if m.Get("wedged-oc") != nil {
		t.Fatalf("session must be deregistered so the next END reports absence")
	}
}

// Silence unused-import lint if platform branches compile some helpers out.
var _ = errors.New
var _ io.WriteCloser = nopWriteCloser{}
var _ = os.Getpid
