// File: end_confirm.go
// -----------------------------------------------------------------------------
// Bounded end-confirmation helpers shared by every resident CLI manager
// (codex app-server, claude native, grok ACP, antigravity native, opencode
// native) and the PTY SessionManager.
//
// Why these exist (prod incident 2026-08-27, HOMETHEATRE): every End path
// used to finish with an UNBOUNDED wait — `<-session.done` after the final
// force kill, or a bare `turnMu.Lock()` drain barrier. When the exit watcher
// wedges (a Process.Wait that never returns, a stalled artifact upload, a
// publish stuck on Pub/Sub), that wait never completes, so the `*_end`
// command handler never returns and never answers the server. Worse, each
// retried END from terminal-service's reaper (one every 5 minutes) parked
// ANOTHER Pub/Sub handler on the same channel until the subscription's
// MaxOutstandingMessages slots (10) were exhausted — at which point the
// device consumed NO commands at all while its HTTP ping kept it looking
// online. One wedged session took the whole device out for 11+ hours.
//
// The contract these helpers enforce: an End path may block only for a
// BOUNDED time after it has issued its final kill. On expiry the caller
//   1. RETAINS the session as a kill-unconfirmed TOMBSTONE — the map entry
//      (and its PID registration) stays, so the session ID cannot be reused
//      while the wedged exit watcher might still act under it, and
//   2. returns an error that does NOT read "session <id> not found". The
//      server treats that exact answer as shutdown evidence and releases the
//      device fence; at the instant the bound expires the process may still
//      be alive (a failed Kill, an unkillable child), so manufacturing the
//      absence answer here would invite two agents into one working tree —
//      the very hazard the fence exists to prevent (Codex P1 on the first
//      revision of this fix, which deregistered immediately).
// A LATER End on the tombstone probes OS-level process absence
// (probeProcessGone): only once the child is VERIFIABLY gone does the
// manager drop the session and report the true absence answer, which
// terminal-service accepts under its existing guarded rules
// (finalizeAbsentDeviceSession). While the probe cannot confirm absence, the
// End keeps re-killing and re-reporting kill-unconfirmed — the device stays
// fenced and VISIBLY wedged (the server escalates TERMINAL_REAPING_WEDGED),
// never silently freed.
// -----------------------------------------------------------------------------

package main

import (
	"errors"
	"os/exec"
	"sync"
	"time"
)

// errEndUnconfirmed marks an End that gave up within its bound while the
// process may still be alive (kill or turn drain unconfirmed; session
// retained as a tombstone). Callers that mirror End outcomes to the cloud
// must check errors.Is against this and MUST NOT publish the `*_ended` frame
// for it — that frame is shutdown evidence, and terminal-service releases
// the device claim on it. Publishing it here would be the same manufactured
// evidence the tombstone exists to prevent, one layer up (Codex P1, round
// 2: the antigravity/opencode end handlers publish ended for EVERY End
// error as idempotent teardown, and stale GC ignored the error entirely).
var errEndUnconfirmed = errors.New("end unconfirmed")

// killConfirmTimeout is how long an End path waits, after issuing the final
// force kill, for the exit watcher to confirm the process is gone (close of
// session.done). A killed process normally exits within milliseconds; this
// bound only matters when the watcher itself is wedged. Declared as a var so
// tests can shorten it.
var killConfirmTimeout = 30 * time.Second

// turnDrainConfirmTimeout bounds the turnMu drain barrier used by the
// turn-per-process managers (antigravity, opencode). Same rationale as
// killConfirmTimeout: an in-flight turn normally unwinds promptly once its
// process is cancelled/killed, and blocking forever when it does not is how
// a device wedges. Declared as a var so tests can shorten it.
var turnDrainConfirmTimeout = 30 * time.Second

// turnBarrierPollInterval is how often waitTurnBarrier retries TryLock.
const turnBarrierPollInterval = 50 * time.Millisecond

// waitTurnBarrier acquires-and-releases mu as a drain barrier, giving up
// after timeout. Returns true when the barrier was passed (the in-flight
// turn has drained), false when the deadline expired with the turn still
// holding the mutex.
func waitTurnBarrier(mu *sync.Mutex, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if mu.TryLock() {
			mu.Unlock()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(turnBarrierPollInterval)
	}
}

// waitDoneConfirm waits for done to close, giving up after timeout. Returns
// true when the exit watcher confirmed (done closed), false on expiry.
func waitDoneConfirm(done <-chan struct{}, timeout time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// probeProcessGone reports whether cmd's child process is VERIFIABLY no
// longer running. This is the only thing allowed to convert a
// kill-unconfirmed tombstone into the "session not found" absence answer the
// server releases a device fence on, so it must FAIL CLOSED: any state it
// cannot interpret reads as "still alive", which keeps the fence up and the
// wedge visible rather than silently freeing a checkout a live process may
// still be writing to.
//
// True when:
//   - there is no process at all (never started), or
//   - Process.Wait has completed (ProcessState is set — the child is reaped;
//     only the session's own watcher calls Wait, so a set state is ours), or
//   - the platform probe (processHandleGone) confirms the OS no longer runs
//     it.
//
// Known fail-closed cases, deliberately accepted: a killed-but-unreaped
// child is a zombie on Unix and Signal(0) still reaches it, so the probe
// says "alive" until the wedged watcher's Wait eventually reaps it; a PID
// recycled to an unrelated process reads "alive" on Unix. Both keep the
// fence up — the visible failure — and resolve through the watcher
// recovering, the agent restarting (shuttingDown evidence), or ops action.
// On Windows the check runs against the process OBJECT (kept alive by the
// os.Process handle), so PID recycling cannot fool it and a terminated
// child reads gone even while unreaped.
//
// Declared as a var so tests can pin the verdict and exercise both tombstone
// outcomes deterministically; defaultProbeProcessGone has its own tests
// against real processes.
var probeProcessGone = defaultProbeProcessGone

func defaultProbeProcessGone(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Process == nil {
		return true
	}
	if cmd.ProcessState != nil {
		return true
	}
	return processHandleGone(cmd.Process)
}
