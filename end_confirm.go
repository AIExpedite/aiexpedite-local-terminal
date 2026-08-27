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
// BOUNDED time after it has issued its final kill. On expiry the caller must
//   1. deregister the session from its manager (removeSession — which also
//      drops the PID from the process registry, so the orphan scanner can
//      reap a truly unkillable survivor), and
//   2. return an error that does NOT read "session <id> not found".
// The next retried END then finds no session and answers with the real
// "not found" message, which terminal-service accepts as shutdown evidence
// under its guarded rules (finalizeAbsentDeviceSession). That two-step
// convergence keeps the server's evidence protocol intact: at the instant
// the bound expires the process may still be alive, so the device must stay
// fenced until absence is actually reported — the error message wording is
// load-bearing.
// -----------------------------------------------------------------------------

package main

import (
	"sync"
	"time"
)

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
