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
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// errorsIs aliases errors.Is for brevity in assertions.
var errorsIs = errors.Is

// nopWriteCloser satisfies io.WriteCloser for fake session stdin pipes.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// exitedTestProcess starts and waits a real short-lived process, so End's
// interrupt/kill escalation targets a process that has already finished
// (interruptProcess and Kill both error and both errors are logged/ignored),
// and defaultProbeProcessGone sees OS-level absence.
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

// lockTurn simulates a wedged in-flight turn holding the drain barrier and
// returns an idempotent release, so a test can drain the barrier mid-way and
// still defer the cleanup unconditionally.
func lockTurn(mu *sync.Mutex) func() {
	mu.Lock()
	var once sync.Once
	return func() { once.Do(mu.Unlock) }
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
	// Exited and reaped → gone.
	if !defaultProbeProcessGone(exitedTestProcess(t)) {
		t.Fatalf("exited+waited process should probe gone")
	}
	// Genuinely running, unreaped → alive (fail closed).
	if defaultProbeProcessGone(liveTestProcess(t)) {
		t.Fatalf("live process must NOT probe gone")
	}
}

func TestDefaultProbeProcessGone_ConcurrentWait(t *testing.T) {
	// Tombstone probing may race the exit watcher's Cmd.Wait. Keep probing
	// while Wait assigns its internal ProcessState; under -race this catches
	// any regression that reads exec.Cmd internals instead of os.Process.
	cmd := liveTestProcess(t)
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("failed to kill helper process: %v", err)
	}
	for {
		_ = defaultProbeProcessGone(cmd)
		select {
		case <-done:
			if !defaultProbeProcessGone(cmd) {
				t.Fatalf("waited process should probe gone")
			}
			return
		default:
		}
	}
}

func TestBoundedArtifactCollect_TimesOutAndPassesThrough(t *testing.T) {
	// Pass-through: a fast collector's results survive intact.
	files, errs, timedOut := boundedArtifactCollect(func(context.Context) ([]FileInfo, []UploadError) {
		return []FileInfo{{Name: "a.png"}}, []UploadError{{File: "b.png", Error: "x"}}
	}, time.Second, "s1")
	if timedOut || len(files) != 1 || len(errs) != 1 {
		t.Fatalf("pass-through failed: files=%d errs=%d timedOut=%v", len(files), len(errs), timedOut)
	}

	// Timeout: a wedged collector must not block the ended publish path.
	block := make(chan struct{})
	defer close(block)
	start := time.Now()
	files, errs, timedOut = boundedArtifactCollect(func(context.Context) ([]FileInfo, []UploadError) {
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
// the END handler hostage — but an absent turn PROCESS is not on its own
// evidence that the session is inert. The turn runner clears activeProcess
// and keeps holding turnMu while it publishes its final frames, so resolving
// here would free the ID for a replacement Start the old goroutine could then
// publish under (Codex P2, round 3). The tombstone must hold until the
// barrier itself drains.
func TestAntigravityEnd_TurnDrainUnconfirmed_NoProcessStillRetains(t *testing.T) {
	shortenEndConfirmTimeouts(t)
	m := NewAntigravityNativeManager(nil)
	session := &AntigravityNativeSession{
		ID:        "wedged-ag",
		StartedAt: time.Now(),
		status:    "ended", // already-ended path still drains on turnMu
	}
	session.turnMu.Lock() // a wedged in-flight turn holds the barrier
	m.sessions["wedged-ag"] = session

	resultCh := make(chan error, 1)
	go func() { resultCh <- m.End("wedged-ag") }()

	var err error
	select {
	case err = <-resultCh:
	case <-time.After(30 * time.Second):
		session.turnMu.Unlock()
		t.Fatalf("End never returned — the unbounded turn barrier is back")
	}
	if err == nil || !errorsIs(err, errEndUnconfirmed) {
		session.turnMu.Unlock()
		t.Fatalf("a held barrier must stay unconfirmed even with no turn process, got %v", err)
	}
	if strings.Contains(err.Error(), "not found") {
		session.turnMu.Unlock()
		t.Fatalf("absence answer manufactured while the turn goroutine still holds the barrier: %v", err)
	}
	if m.Get("wedged-ag") != session {
		session.turnMu.Unlock()
		t.Fatalf("tombstone must be retained until the barrier drains")
	}

	// The turn goroutine finally unwinds: the next End drains the barrier and
	// the session is released.
	session.turnMu.Unlock()
	if err := m.End("wedged-ag"); err != nil {
		t.Fatalf("drained barrier should end cleanly, got %v", err)
	}
	if m.Get("wedged-ag") != nil {
		t.Fatalf("session should be removed once the barrier drained")
	}
}

// Same contract on the other turn manager — the branch Codex flagged exists
// identically in OpenCodeNativeManager.
func TestOpenCodeEnd_TurnDrainUnconfirmed_NoProcessStillRetains(t *testing.T) {
	shortenEndConfirmTimeouts(t)
	m := NewOpenCodeNativeManager()
	session := &OpenCodeNativeSession{
		ID:        "wedged-oc",
		StartedAt: time.Now(),
		status:    "ended",
	}
	session.turnMu.Lock()
	m.sessions["wedged-oc"] = session

	err := m.End("wedged-oc")
	if err == nil || !errorsIs(err, errEndUnconfirmed) {
		session.turnMu.Unlock()
		t.Fatalf("a held barrier must stay unconfirmed even with no turn process, got %v", err)
	}
	if m.Get("wedged-oc") != session {
		session.turnMu.Unlock()
		t.Fatalf("tombstone must be retained until the barrier drains")
	}

	session.turnMu.Unlock()
	if err := m.End("wedged-oc"); err != nil {
		t.Fatalf("drained barrier should end cleanly, got %v", err)
	}
	if m.Get("wedged-oc") != nil {
		t.Fatalf("session should be removed once the barrier drained")
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
	m.sessions["wedged-ag"] = session

	err := m.End("wedged-ag")
	if err == nil || !strings.Contains(err.Error(), "turn drain unconfirmed") {
		session.turnMu.Unlock()
		t.Fatalf("expected turn-drain-unconfirmed error, got %v", err)
	}
	if strings.Contains(err.Error(), "not found") {
		session.turnMu.Unlock()
		t.Fatalf("absence answer manufactured while probe says alive: %v", err)
	}
	if m.Get("wedged-ag") != session {
		session.turnMu.Unlock()
		t.Fatalf("tombstone must be retained while the turn process is alive")
	}

	// The process going away is NOT enough on its own: the turn goroutine
	// still holds the barrier and can act under this ID (Codex P2, round 3).
	stubProbe(t, true)
	err = m.End("wedged-ag")
	if err == nil || !errorsIs(err, errEndUnconfirmed) {
		session.turnMu.Unlock()
		t.Fatalf("held barrier must keep the end unconfirmed, got %v", err)
	}
	if m.Get("wedged-ag") != session {
		session.turnMu.Unlock()
		t.Fatalf("tombstone must be retained until the barrier drains")
	}

	// Process gone AND the barrier drained → the session is released.
	session.turnMu.Unlock()
	if err := m.End("wedged-ag"); err != nil {
		t.Fatalf("drained barrier should end cleanly, got %v", err)
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
	m.sessions["wedged-oc"] = session

	err := m.End("wedged-oc")
	if err == nil || !strings.Contains(err.Error(), "turn drain unconfirmed") {
		session.turnMu.Unlock()
		t.Fatalf("expected turn-drain-unconfirmed error, got %v", err)
	}
	if m.Get("wedged-oc") != session {
		session.turnMu.Unlock()
		t.Fatalf("tombstone must be retained while the turn process is alive")
	}

	// Process gone but the barrier still held → still unconfirmed.
	stubProbe(t, true)
	err = m.End("wedged-oc")
	if err == nil || !errorsIs(err, errEndUnconfirmed) {
		session.turnMu.Unlock()
		t.Fatalf("held barrier must keep the end unconfirmed, got %v", err)
	}
	if m.Get("wedged-oc") != session {
		session.turnMu.Unlock()
		t.Fatalf("tombstone must be retained until the barrier drains")
	}

	session.turnMu.Unlock()
	if err := m.End("wedged-oc"); err != nil {
		t.Fatalf("drained barrier should end cleanly, got %v", err)
	}
	if m.Get("wedged-oc") != nil {
		t.Fatalf("session should be removed once resolved absent")
	}
}

// Every unconfirmed-end error must carry the errEndUnconfirmed sentinel:
// the antigravity/opencode end handlers publish `*_ended` for ordinary End
// errors (idempotent teardown), and only errors.Is lets them withhold that
// shutdown evidence for a possibly-alive process (Codex P1, round 2).
func TestUnconfirmedEndErrorsCarrySentinel(t *testing.T) {
	shortenEndConfirmTimeouts(t)
	stubProbe(t, false)

	// Process-manager shape (codex, via the tombstone retry branch).
	cm, cs := newWedgedCodexSession(t)
	cs.markKillUnconfirmed()
	if err := cm.End("wedged"); !errorsIs(err, errEndUnconfirmed) {
		t.Fatalf("codex kill-unconfirmed error must wrap errEndUnconfirmed: %v", err)
	}

	// Turn-manager shape (antigravity, drain barrier held + live process).
	am := NewAntigravityNativeManager(nil)
	as := &AntigravityNativeSession{ID: "ag", StartedAt: time.Now(), status: "ended"}
	as.activeProcess = exitedTestProcess(t) // probe stub says alive anyway
	as.turnMu.Lock()
	defer as.turnMu.Unlock()
	am.sessions["ag"] = as
	if err := am.End("ag"); !errorsIs(err, errEndUnconfirmed) {
		t.Fatalf("antigravity drain-unconfirmed error must wrap errEndUnconfirmed: %v", err)
	}

	// The verified-absence answer must NOT carry it — that one is the real
	// absence evidence and SHOULD flow into the idempotent ended publish.
	stubProbe(t, true)
	if err := cm.End("wedged"); err == nil || errorsIs(err, errEndUnconfirmed) {
		t.Fatalf("verified absence must not carry the unconfirmed sentinel: %v", err)
	}
}

// Stale GC must withhold the `*_ended` frame while an end is unconfirmed —
// publishing it would hand the server shutdown evidence it does not have.
func TestAntigravityStaleGC_WithholdsEndedWhileUnconfirmed(t *testing.T) {
	shortenEndConfirmTimeouts(t)
	stubProbe(t, false)
	m := NewAntigravityNativeManager(nil)
	var published []resultMsg
	session := &AntigravityNativeSession{
		ID:        "ag-stale",
		StartedAt: time.Now().Add(-12 * time.Hour), // past any maxAge
		status:    "ended",
		publishFn: func(msg resultMsg) { published = append(published, msg) },
	}
	session.activeProcess = exitedTestProcess(t) // probe stub says alive anyway
	session.turnMu.Lock()
	defer session.turnMu.Unlock()
	m.sessions["ag-stale"] = session

	m.endStaleSessions(6 * time.Hour)

	for _, msg := range published {
		if msg.Type == "antigravity_native_ended" {
			t.Fatalf("stale GC published ended for an unconfirmed end: %+v", msg)
		}
	}
	if m.Get("ag-stale") != session {
		t.Fatalf("tombstone must be retained for the next GC tick")
	}
}

// The same withholding on the GC path for a STALE reap: a reap that raced a
// replacement Start must not publish ended either, because that frame is keyed
// only by the session ID the replacement now owns (Codex P2, round 4 — the
// `*_end` handler withheld it, the reaper still published).
func TestTurnManagerStaleGC_WithholdsEndedForReplacedSession(t *testing.T) {
	shortenEndConfirmTimeouts(t)

	t.Run("antigravity", func(t *testing.T) {
		m := NewAntigravityNativeManager(nil)
		var mu sync.Mutex
		var published []resultMsg
		old := &AntigravityNativeSession{
			ID:        "ag-gc",
			StartedAt: time.Now().Add(-12 * time.Hour), // past any maxAge
			status:    "ended",
			publishFn: func(msg resultMsg) { mu.Lock(); published = append(published, msg); mu.Unlock() },
		}
		replacement := &AntigravityNativeSession{ID: "ag-gc", StartedAt: time.Now(), status: "running"}
		m.sessions["ag-gc"] = old

		unlockTurn := lockTurn(&old.turnMu)
		done := make(chan struct{})
		go func() { m.endStaleSessions(6 * time.Hour); close(done) }()

		// The reap's End is parked on the barrier: swap the ID underneath it,
		// then let the turn drain — exactly the window the handler-side test
		// uses.
		time.Sleep(100 * time.Millisecond)
		m.mu.Lock()
		m.sessions["ag-gc"] = replacement
		m.mu.Unlock()
		unlockTurn()

		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("stale GC never finished")
		}
		mu.Lock()
		defer mu.Unlock()
		for _, msg := range published {
			if msg.Type == "antigravity_native_ended" {
				t.Fatalf("stale GC published ended under a replacement's ID: %+v", msg)
			}
		}
		if m.Get("ag-gc") != replacement {
			t.Fatalf("the replacement session must survive a stale reap")
		}
	})

	t.Run("opencode", func(t *testing.T) {
		m := NewOpenCodeNativeManager()
		var mu sync.Mutex
		var published []resultMsg
		old := &OpenCodeNativeSession{
			ID:        "oc-gc",
			StartedAt: time.Now().Add(-12 * time.Hour),
			status:    "ended",
			publishFn: func(msg resultMsg) { mu.Lock(); published = append(published, msg); mu.Unlock() },
		}
		replacement := &OpenCodeNativeSession{ID: "oc-gc", StartedAt: time.Now(), status: "running"}
		m.sessions["oc-gc"] = old

		unlockTurn := lockTurn(&old.turnMu)
		done := make(chan struct{})
		go func() { m.endStaleSessions(6 * time.Hour); close(done) }()

		time.Sleep(100 * time.Millisecond)
		m.mu.Lock()
		m.sessions["oc-gc"] = replacement
		m.mu.Unlock()
		unlockTurn()

		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("stale GC never finished")
		}
		mu.Lock()
		defer mu.Unlock()
		for _, msg := range published {
			if msg.Type == "opencode_native_ended" {
				t.Fatalf("stale GC published ended under a replacement's ID: %+v", msg)
			}
		}
		if m.Get("oc-gc") != replacement {
			t.Fatalf("the replacement session must survive a stale reap")
		}
	})
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

// The stale terminal frame half of the reused-ID race (Codex P2, round 3):
// removeSessionIfSame stops a recovering watcher from DELETING a replacement,
// but its `*_ended` publish was unconditional — and that frame is the
// shutdown evidence terminal-service releases the device claim on, so under a
// replacement's ID it tears down a session that is still running.
func TestPublishTerminalIfCurrent_SuppressesFrameForReplacedID(t *testing.T) {
	m := NewCodexAppServerManager(nil)
	old := &CodexAppServerSession{ID: "s", status: "ended", done: make(chan struct{})}
	replacement := &CodexAppServerSession{ID: "s", status: "running", done: make(chan struct{})}

	var mu sync.Mutex
	var published []resultMsg
	publishFn := func(msg resultMsg) {
		mu.Lock()
		published = append(published, msg)
		mu.Unlock()
	}
	frame := resultMsg{ID: "s", SessionID: "s", Type: "codex_appserver_ended"}
	publishedCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(published)
	}

	// Still ours → published (the ordinary teardown).
	m.sessions["s"] = old
	if !publishTerminalIfCurrent(&m.mu, m.sessions, "s", old, &old.terminalPublishState, publishFn, frame, nil) {
		t.Fatalf("terminal frame must publish while the ID is still this session's")
	}
	deadline := time.Now().Add(2 * time.Second)
	for (publishedCount() < 1 || old.terminalPublishInFlight()) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if publishedCount() != 1 || old.terminalPublishInFlight() {
		t.Fatalf("initial terminal frame did not finish before the next scenario")
	}

	// ID re-taken by a replacement → suppressed.
	m.sessions["s"] = replacement
	if publishTerminalIfCurrent(&m.mu, m.sessions, "s", old, &old.terminalPublishState, publishFn, frame, nil) {
		t.Fatalf("stale terminal frame published under a replacement session's ID")
	}

	// Unclaimed ID → published only after atomically restoring the old
	// session as a delivery tombstone. Otherwise Start could reuse the ID while
	// the frame is in flight because the state bit lives outside the map.
	delete(m.sessions, "s")
	publishEntered := make(chan struct{})
	releasePublish := make(chan struct{})
	if !publishTerminalIfCurrent(&m.mu, m.sessions, "s", old, &old.terminalPublishState, func(msg resultMsg) {
		publishFn(msg)
		close(publishEntered)
		<-releasePublish
	}, frame, func() { m.removeSessionIfSame("s", old) }) {
		t.Fatalf("terminal frame must publish when the ID is unclaimed")
	}
	<-publishEntered
	if m.Get("s") != old {
		t.Fatalf("unclaimed ID must be re-reserved until terminal delivery completes")
	}
	if m.removeSessionIfSame("s", old) {
		t.Fatalf("delivery tombstone was freed while its terminal frame was in flight")
	}
	close(releasePublish)

	// The publishes are async; wait for exactly the two allowed frames.
	deadline = time.Now().Add(2 * time.Second)
	for publishedCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := publishedCount(); got != 2 {
		t.Fatalf("expected exactly 2 published frames (current + unclaimed), got %d", got)
	}
	deadline = time.Now().Add(2 * time.Second)
	for m.Get("s") != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if m.Get("s") != nil {
		t.Fatalf("delivery tombstone was not released after terminal delivery")
	}
}

// Round 4 of the same finding: the identity test only guards the LAUNCH of the
// publish. publishFn then sits in network I/O for up to ~30 s, and a
// replacement Start registering inside that window would receive the old
// frame. The ID must therefore stay reserved until delivery completes, and the
// publisher — not the watcher — performs the release.
func TestPublishTerminalIfCurrent_ReservesIDUntilDelivered(t *testing.T) {
	m := NewCodexAppServerManager(nil)
	session := &CodexAppServerSession{ID: "s", status: "ended", done: make(chan struct{})}
	m.sessions["s"] = session

	releasePublish := make(chan struct{})
	publishFn := func(resultMsg) { <-releasePublish }

	released := make(chan struct{})
	if !publishTerminalIfCurrent(&m.mu, m.sessions, "s", session, &session.terminalPublishState,
		publishFn, resultMsg{SessionID: "s", Type: "codex_appserver_ended"},
		func() {
			m.removeSessionIfSame("s", session)
			close(released)
		}) {
		t.Fatalf("terminal frame must publish while the ID is still this session's")
	}

	// While the frame is in flight the ID is NOT free — a removal must not
	// hand it to a replacement Start.
	if !session.terminalPublishInFlight() {
		t.Fatalf("the publish reservation must be taken before the launch returns")
	}
	if m.removeSessionIfSame("s", session) {
		t.Fatalf("removeSessionIfSame freed an ID whose terminal frame is still in flight")
	}
	if m.Get("s") != session {
		t.Fatalf("session must be retained until its terminal frame is delivered")
	}

	// Delivery completes → the publisher releases the ID.
	close(releasePublish)
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatalf("publisher never released the session after delivery")
	}
	if m.Get("s") != nil {
		t.Fatalf("session should be removed once its terminal frame was delivered")
	}
}

// An End that resolves a tombstone while the terminal frame is still in flight
// must not answer absence — the ID is not free, so the server would release
// the fence for an ID a frame is still travelling under.
func TestEnd_WithholdsAbsenceWhileTerminalFrameInFlight(t *testing.T) {
	stubProbe(t, true)
	m := NewCodexAppServerManager(nil)
	session := &CodexAppServerSession{
		ID:              "s",
		StartedAt:       time.Now(),
		status:          "running",
		killUnconfirmed: true,
		done:            make(chan struct{}),
		Process:         exitedTestProcess(t),
	}
	session.publishInFlight.Store(true)
	m.sessions["s"] = session

	err := m.End("s")
	if err == nil || strings.Contains(err.Error(), "not found") {
		t.Fatalf("absence answered while a terminal frame is in flight: %v", err)
	}
	if !errorsIs(err, errEndUnconfirmed) {
		t.Fatalf("expected the unconfirmed sentinel so the handler withholds ended, got %v", err)
	}
	if m.Get("s") != session {
		t.Fatalf("session must be retained while its terminal frame is in flight")
	}
}

// The turn managers publish their terminal frame from the END HANDLER, not
// from a watcher, so their protection is the End RESULT: an End whose session
// was replaced under it must report staleness rather than success, or the
// handler publishes *_ended under the replacement's ID (Codex P2, round 4).
// The barrier is the injection point — it is exactly where the real End sits
// while the concurrent End + replacement Start land.
func TestTurnManagerEnd_ReportsStaleWhenSessionReplaced(t *testing.T) {
	t.Run("antigravity", func(t *testing.T) {
		m := NewAntigravityNativeManager(nil)
		old := &AntigravityNativeSession{ID: "s", StartedAt: time.Now(), status: "ended"}
		replacement := &AntigravityNativeSession{ID: "s", StartedAt: time.Now(), status: "running"}
		m.sessions["s"] = old

		unlockTurn := lockTurn(&old.turnMu)
		resultCh := make(chan error, 1)
		go func() { resultCh <- m.End("s") }()

		// End has captured `old` and is parked on the barrier; swap the ID to a
		// replacement underneath it, then let the turn drain.
		time.Sleep(100 * time.Millisecond)
		m.mu.Lock()
		m.sessions["s"] = replacement
		m.mu.Unlock()
		unlockTurn()

		var err error
		select {
		case err = <-resultCh:
		case <-time.After(30 * time.Second):
			t.Fatalf("End never returned")
		}
		if !errorsIs(err, errEndStaleSession) {
			t.Fatalf("expected the stale sentinel, got %v", err)
		}
		if m.Get("s") != replacement {
			t.Fatalf("the replacement session must survive a stale End")
		}
	})

	t.Run("opencode", func(t *testing.T) {
		m := NewOpenCodeNativeManager()
		old := &OpenCodeNativeSession{ID: "s", StartedAt: time.Now(), status: "ended"}
		replacement := &OpenCodeNativeSession{ID: "s", StartedAt: time.Now(), status: "running"}
		m.sessions["s"] = old

		unlockTurn := lockTurn(&old.turnMu)
		resultCh := make(chan error, 1)
		go func() { resultCh <- m.End("s") }()

		time.Sleep(100 * time.Millisecond)
		m.mu.Lock()
		m.sessions["s"] = replacement
		m.mu.Unlock()
		unlockTurn()

		var err error
		select {
		case err = <-resultCh:
		case <-time.After(30 * time.Second):
			t.Fatalf("End never returned")
		}
		if !errorsIs(err, errEndStaleSession) {
			t.Fatalf("expected the stale sentinel, got %v", err)
		}
		if m.Get("s") != replacement {
			t.Fatalf("the replacement session must survive a stale End")
		}
	})
}

// The bounded collector must CANCEL the work it stops waiting for, so an
// upload that would otherwise finish after the deadline never writes objects
// the ended frame cannot reference (Codex P2, round 4).
func TestBoundedArtifactCollect_CancelsAbandonedWork(t *testing.T) {
	cancelled := make(chan struct{})
	files, _, timedOut := boundedArtifactCollect(func(ctx context.Context) ([]FileInfo, []UploadError) {
		<-ctx.Done()
		close(cancelled)
		// One upload finalized before a sibling stalled. Its metadata must not
		// be discarded when cancellation unwinds the batch.
		return []FileInfo{{Name: "completed.png"}}, nil
	}, 50*time.Millisecond, "s-cancel")
	if !timedOut {
		t.Fatalf("expected the collect to time out")
	}
	if len(files) != 1 || files[0].Name != "completed.png" {
		t.Fatalf("completed upload metadata was discarded: %#v", files)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatalf("abandoned collect was never cancelled")
	}
}
