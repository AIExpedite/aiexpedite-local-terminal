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
	"fmt"
	"os/exec"
	"sync"
	"sync/atomic"
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

// errEndStaleSession marks an End whose session pointer was replaced under it:
// a concurrent End removed the session and a new Start re-took the ID while
// this End was still draining. The end itself succeeded — the session it
// captured really is gone — but its caller MUST NOT publish the terminal
// `*_ended` frame, because that frame is keyed only by session ID and would
// now be read as shutdown evidence for the REPLACEMENT (Codex P2, round 4).
// The antigravity/opencode handlers, which publish ended for ordinary End
// errors as idempotent teardown, publish NOTHING at all for this one: every
// frame they emit carries the session ID, so an error frame would be
// misattributed to the live replacement just as an ended frame would.
var errEndStaleSession = errors.New("end raced a replacement session")

// staleEndError is the shared verdict for an End whose session pointer was
// replaced under it, so both turn managers word it identically and neither can
// forget the sentinel.
func staleEndError(kind string, id string) error {
	return fmt.Errorf("%s session %s was replaced by a new session before this end completed: %w", kind, id, errEndStaleSession)
}

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

// terminalPublishState is embedded in every session struct whose exit watcher
// owns the terminal `*_ended` frame. It records that a terminal publish has
// been LAUNCHED but not yet DELIVERED, which is what reserves the session ID
// for the duration of the Pub/Sub round-trip.
//
// Testing identity at launch time is not sufficient on its own: the launch only
// spawns a goroutine, and publishFn can then sit in network I/O for ~30 s. A
// replacement Start registering inside that window would receive the old frame
// as its own shutdown evidence — the very race the identity test exists to
// close (Codex P2, round 4). So the removals that would free the ID
// (removeSessionIfSame) refuse while a publish is in flight, and the publisher
// itself performs the release once publishFn returns.
//
// Retaining is the fail-safe direction: a publishFn that never returns leaves
// the session in the map and the device fenced — visibly wedged — rather than
// freeing an ID a frame is still travelling under.
type terminalPublishState struct {
	publishInFlight atomic.Bool
}

// terminalPublishInFlight reports whether a terminal frame has been launched
// for this session and has not yet been delivered.
func (t *terminalPublishState) terminalPublishInFlight() bool { return t.publishInFlight.Load() }

// publishTerminalIfCurrent publishes a session's terminal (`*_ended`) frame
// only while id still maps to THIS session — or to no session at all — and
// reports whether it published. Once publishFn returns it runs release, which
// is the ONLY thing that drops a session whose terminal publish is in flight.
//
// Why the guard exists: a tombstone that resolves through verified process
// absence frees its ID for reuse while the wedged exit watcher may still be
// blocked in stream drain or artifact collection. removeSessionIfSame already
// stops that watcher from DELETING the replacement, but its terminal publish
// was unconditional (Codex P2, round 3) — and that frame is the shutdown
// evidence terminal-service releases the device claim on, so emitting it under
// a replacement's ID tears down a session that is still running.
//
// Both the identity test and the in-flight reservation are taken under the
// manager's own map lock — the same lock Start takes to register a session — so
// the ID is reserved atomically with the decision to publish. A replacement can
// register neither between those two steps nor during delivery, because
// removeSessionIfSame will not free a reserved ID (Codex P2, round 4). An
// UNCLAIMED id still publishes: that is the ordinary teardown (End removes the
// session on the "ended" status fast-path while the watcher is still finishing
// its artifact scan), and a late frame for a session the server already
// considers gone is idempotent.
func publishTerminalIfCurrent[S comparable](
	mu *sync.RWMutex,
	sessions map[string]S,
	id string,
	s S,
	state *terminalPublishState,
	publishFn PublishFunc,
	msg resultMsg,
	release func(),
) bool {
	mu.Lock()
	if cur, ok := sessions[id]; ok && cur != s {
		mu.Unlock()
		return false
	}
	state.publishInFlight.Store(true)
	mu.Unlock()

	// Cheap here: this only spawns the publisher goroutine. The ID stays
	// reserved until that goroutine's publishFn returns.
	publishTerminalResultAsync(func(m resultMsg) {
		defer func() {
			state.publishInFlight.Store(false)
			if release != nil {
				release()
			}
		}()
		publishFn(m)
	}, msg)
	return true
}
