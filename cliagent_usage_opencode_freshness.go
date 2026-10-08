// cliagent_usage_opencode_freshness.go — the run lifecycle that gets an
// OpenCode turn's spend into the ledger (cliagent_usage_opencode_capture.go),
// and the bounded `opencode export` fallback for a turn whose stream reported
// none.
//
//   - Arm, when the turn's child spawns: the run floor is now, and an ARMED
//     debt is written synchronously. A crash or self-update mid-turn drops the
//     in-memory accumulator; the armed debt (once a frame named the session) is
//     what the next process pays from.
//   - Capture: every streamed line goes to captureOpenCodeUsageLine — memory
//     only. The first session id is recorded on the debt, one write per run,
//     off the stream goroutine.
//   - Settle, once per run (terminal event or exit, whichever comes first):
//     captured steps commit and retire the debt in one write; a refused write is
//     booked on the commit ladder, because the smoke and runOneShot have no later
//     flush. A run with no step that spent anything but with a session id keeps
//     the debt, OWED, written synchronously before an update hand-off can replace
//     the process; that write is booked on the same commit ladder when refused,
//     because nothing else revisits a settled run. With no session id nothing can
//     be exported: the debt is dropped ("unattributable").
//   - Coordinate: a run settled on its terminal event (a terminal session whose
//     child still runs) stays LIVE until its exit path finishes it. Its export
//     stands down while it is live, spending no attempt: a streamed step is
//     keyed by its part id and an exported message by its message id, so paying
//     both would count the turn twice.
//   - Ladder: attempts at 15 s, 1 m and 5 m, each one `opencode export <id>`
//     with a 5 s timeout. A refusal that spent nothing (offline, binary
//     missing) books refreshFreeRetryDelay without consuming an attempt. A
//     debt is retired when it is paid, its attempts are spent, or it is 6 h old.
//   - Survive: StartAgent calls payOwedOpenCodeUsage, which re-arms the ladder
//     for the previous process's debts — including armed ones it never
//     settled.
//   - Own: once a run names its session, the write that records the id also
//     opens the run's ownership window (openCodeOwnedRun) at the run floor; the
//     direct-run reader (cliagent_usage_opencode_direct.go) skips every message
//     of that session inside it. The window closes when the run's stream is
//     over — a closing settle (native, smoke, a session's exit), finish,
//     disarm or retire — never on a terminal-event settle, whose flush can
//     still commit late steps. Until a run has a window on disk it is an
//     ARMED run the reader holds back for (openCodeUsageArmedHoldMs).
//
// It is a module of its own rather than a fork of the Codex or Grok freshness
// paths: those are bound to their own caches and generation types. Only the
// ladder arithmetic (cliagent_usage_refresh_ladder.go) is shared.
//
// Logs are fixed labels only: never a session id, path or export output.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Vars so tests can pin them small.
var (
	openCodeUsageDebtLadder = []time.Duration{15 * time.Second, time.Minute, 5 * time.Minute}
	openCodeUsageDebtMaxAge = 6 * time.Hour
	openCodeExportTimeout   = 5 * time.Second
	openCodeUsageFreeFloor  = 15 * time.Second
	// openCodeUsageCommitLadder paces the retries of a REFUSED ledger write.
	// Those spend no outbound read, only a local read-modify-rename, so they
	// start far shorter than the export ladder.
	openCodeUsageCommitLadder = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute}
	// openCodeUsageCommitMaxAttempts bounds those retries: rung 1 is the ladder's
	// head, so the budget is its length plus the settle's own attempt.
	openCodeUsageCommitMaxAttempts = len(openCodeUsageCommitLadder) + 1
	openCodeUsageAfterFunc         = time.AfterFunc
	openCodeUsageFreshnessNow      = time.Now
)

const (
	openCodeUsageDebtMaxAttempts = 3
	// openCodeExportMaxBytes bounds the export capture: a long session's
	// export carries every tool output. One that does not fit is unreadable,
	// never a partial count.
	openCodeExportMaxBytes = 32 << 20
)

var (
	// openCodeUsageInFlight tracks the one-off background work this file starts
	// (a session-id write, the startup adoption) so tests can wait for it.
	openCodeUsageInFlight sync.WaitGroup
	// openCodeUsageTimers holds each debt's booked attempt, by run id.
	openCodeUsageTimersMu sync.Mutex
	openCodeUsageTimers   = map[string]*time.Timer{}
	// openCodeUsageLiveRuns holds the runs THIS process could still commit
	// from after their settle, by run id: one settled on its terminal event
	// whose stream has not closed, or whose steps a refused write still owes
	// the ledger. A streamed step is keyed by its
	// part id and an exported message by its message id, so the two paths
	// cannot dedup against each other — the export stands down instead for as
	// long as the run might still pay the same turn. A debt adopted from an
	// earlier process has no entry here, so it exports at once.
	openCodeUsageLiveMu   sync.Mutex
	openCodeUsageLiveRuns = map[string]*openCodeUsageRun{}
	// openCodeUsageArmedRuns holds this process's runs that have no ownership
	// window on disk yet, by run id — the in-memory half of the direct reader's
	// hold, which also covers a run whose armed-debt write was refused. Guarded
	// by openCodeUsageLiveMu.
	openCodeUsageArmedRuns = map[string]*openCodeUsageRun{}
)

// releaseOpenCodeUsageArmedRun drops the run from the reader's hold: its window
// is on disk, or it can never record one.
func releaseOpenCodeUsageArmedRun(run *openCodeUsageRun) {
	if run == nil {
		return
	}
	openCodeUsageLiveMu.Lock()
	delete(openCodeUsageArmedRuns, run.id)
	openCodeUsageLiveMu.Unlock()
}

// openCodeUsageArmedHoldMs is the floor of the oldest run this process armed
// that has no ownership window yet, or 0. A run older than the debt age-out no
// longer holds: a stuck run must not freeze direct capture.
func openCodeUsageArmedHoldMs(now time.Time) int64 {
	cutoff := now.Add(-openCodeUsageDebtMaxAge).UnixMilli()
	openCodeUsageLiveMu.Lock()
	defer openCodeUsageLiveMu.Unlock()
	hold := int64(0)
	for _, run := range openCodeUsageArmedRuns {
		if run.floorMs >= cutoff && (hold == 0 || run.floorMs < hold) {
			hold = run.floorMs
		}
	}
	return hold
}

// settleOpenCodeUsageRunOnStream settles a run whose stream may outlive its
// settle: a terminal session's turn, settled on its terminal event while the
// child still runs. The run is registered as live first, so an export booked by
// that settle stands down until finishOpenCodeUsageRun says the stream is over.
func settleOpenCodeUsageRunOnStream(run *openCodeUsageRun, sessionID string) {
	if run == nil {
		return
	}
	openCodeUsageLiveMu.Lock()
	openCodeUsageLiveRuns[run.id] = run
	openCodeUsageLiveMu.Unlock()
	settleOpenCodeUsageRunOnce(run, sessionID)
}

// releaseOpenCodeUsageLiveRun drops the run once it can commit nothing more:
// its stream is closed and no step is waiting on a write. Called from every
// path that ends a run or finishes a ledger write, so the map holds only runs
// that are genuinely still open.
func releaseOpenCodeUsageLiveRun(run *openCodeUsageRun) {
	if run == nil || !run.streamClosed.Load() || run.pendingCommit() {
		return
	}
	openCodeUsageLiveMu.Lock()
	delete(openCodeUsageLiveRuns, run.id)
	openCodeUsageLiveMu.Unlock()
}

// openCodeUsageLiveRun returns the run a debt belongs to, if this process still
// holds it open.
func openCodeUsageLiveRun(runID string) *openCodeUsageRun {
	openCodeUsageLiveMu.Lock()
	defer openCodeUsageLiveMu.Unlock()
	return openCodeUsageLiveRuns[runID]
}

// finishOpenCodeUsageRun closes a run: its stream is over, so nothing else can
// join or re-offer its steps. Callers settle (or flush) FIRST and finish last,
// so a late step is already committed — or booked on the commit ladder, which
// keeps the run live — before the export fallback is released. An export that
// stood down while the run was open is re-booked at the ladder's head rather
// than left on its grown free delay.
func finishOpenCodeUsageRun(run *openCodeUsageRun) {
	if run == nil {
		return
	}
	run.streamClosed.Store(true)
	releaseOpenCodeUsageLiveRun(run)
	run.closeOwnedWindow(1)
	if run.exportDeferred.CompareAndSwap(true, false) {
		scheduleOpenCodeUsageAttempt(run.id, openCodeUsageDebtLadder[0])
	}
}

/* --------------------------------------------------------------------------
   Ownership windows
   -------------------------------------------------------------------------- */

// streamOver reports that no further step can join the run: a closing settle
// or finish said so.
func (run *openCodeUsageRun) streamOver() bool {
	return run.streamClosed.Load() || run.closingSettle.Load()
}

// syncOwnedWindow records the run's ownership window on ledger, inside the
// caller's transaction: opened at the run floor for sessionID, closed at nowMs
// once the stream is over. It reports whether the window is closed. No session
// id, no window — the run owns nothing the reader could tell apart.
func (run *openCodeUsageRun) syncOwnedWindow(ledger *openCodeUsageLedger, sessionID string, nowMs int64) (closed bool) {
	if sessionID == "" {
		return false
	}
	w := openCodeOwnedRunByID(ledger, run.id)
	if w == nil {
		ledger.OwnedRuns = append(ledger.OwnedRuns, openCodeOwnedRun{
			RunID:      run.id,
			SessionKey: openCodeUsageSessionKey(sessionID),
			FromMs:     run.floorMs,
		})
		w = &ledger.OwnedRuns[len(ledger.OwnedRuns)-1]
	}
	if w.open() && run.streamOver() {
		w.ToMs = max(nowMs, w.FromMs)
	}
	closed = !w.open()
	pruneOpenCodeOwnedRuns(ledger, nowMs)
	return closed
}

// noteOwnedWindow records what a committed syncOwnedWindow left on disk.
func (run *openCodeUsageRun) noteOwnedWindow(sessionID string, closed bool) {
	if sessionID == "" {
		return
	}
	releaseOpenCodeUsageArmedRun(run)
	if closed {
		run.windowClosed.Store(true)
	}
}

// closeOwnedWindow closes the run's window once its stream is over, retrying a
// refused write on the commit ladder. A run with no session id has none.
func (run *openCodeUsageRun) closeOwnedWindow(attempt int) {
	sessionID := run.session()
	if sessionID == "" {
		releaseOpenCodeUsageArmedRun(run)
		return
	}
	if run.windowClosed.Load() {
		return
	}
	nowMs := openCodeUsageFreshnessNow().UnixMilli()
	closed := false
	committed, _, _ := openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		closed = run.syncOwnedWindow(ledger, sessionID, nowMs)
		return true, false
	})
	if committed {
		run.noteOwnedWindow(sessionID, closed)
		return
	}
	delay, more := refreshRetryDelayForAttempt(attempt, openCodeUsageCommitMaxAttempts, openCodeUsageCommitLadder)
	if !more {
		// The window stays open on disk until the next process's adoption
		// closes it: the reader under-counts that session meanwhile, never
		// double counts it.
		releaseOpenCodeUsageArmedRun(run)
		logOpenCodeUsageCapture("window_abandoned")
		return
	}
	logOpenCodeUsageCapture("window_retry")
	openCodeUsageAfterFunc(delay, func() { run.closeOwnedWindow(attempt + 1) })
}

func openCodeOwnedRunByID(ledger *openCodeUsageLedger, runID string) *openCodeOwnedRun {
	for i := range ledger.OwnedRuns {
		if ledger.OwnedRuns[i].RunID == runID {
			return &ledger.OwnedRuns[i]
		}
	}
	return nil
}

// closeOpenCodeOwnedRunByID closes the run's window, if it has an open one.
func closeOpenCodeOwnedRunByID(ledger *openCodeUsageLedger, runID string, nowMs int64) bool {
	w := openCodeOwnedRunByID(ledger, runID)
	if w == nil || !w.open() {
		return false
	}
	w.ToMs = max(nowMs, w.FromMs)
	return true
}

// pruneOpenCodeOwnedRuns drops closed windows that ended before local midnight —
// the reader never scans before it — then the oldest closed windows past the
// cap. An open window is never dropped: its run may still commit, and the reader
// would count its session again.
func pruneOpenCodeOwnedRuns(ledger *openCodeUsageLedger, nowMs int64) {
	midnight := openCodeLocalMidnight(time.UnixMilli(nowMs)).UnixMilli()
	kept := ledger.OwnedRuns[:0]
	for _, w := range ledger.OwnedRuns {
		if !w.open() && w.ToMs < midnight {
			continue
		}
		kept = append(kept, w)
	}
	ledger.OwnedRuns = kept
	for len(ledger.OwnedRuns) > openCodeUsageMaxOwnedRuns {
		oldest := -1
		for i, w := range ledger.OwnedRuns {
			if !w.open() && (oldest < 0 || w.ToMs < ledger.OwnedRuns[oldest].ToMs) {
				oldest = i
			}
		}
		if oldest < 0 {
			return
		}
		ledger.OwnedRuns = append(ledger.OwnedRuns[:oldest], ledger.OwnedRuns[oldest+1:]...)
	}
}

/* --------------------------------------------------------------------------
   Arm / settle
   -------------------------------------------------------------------------- */

// armOpenCodeUsageRun starts a run at the moment its child spawns. fingerprint
// may be "" (resolved again at settle); executable and dir are where the
// export fallback would run. The armed debt is tried before returning, and
// retried in the background if that write is refused.
func armOpenCodeUsageRun(executable, dir, fingerprint string) *openCodeUsageRun {
	now := openCodeUsageFreshnessNow()
	run := &openCodeUsageRun{
		id:          newOpenCodeUsageRunID(),
		floorMs:     now.UnixMilli(),
		fingerprint: fingerprint,
		executable:  executable,
		dir:         dir,
	}
	// Held by the direct reader until its window is on disk: from here the
	// child may write messages the stream will pay.
	openCodeUsageLiveMu.Lock()
	openCodeUsageArmedRuns[run.id] = run
	openCodeUsageLiveMu.Unlock()
	// Tried once, never waited on: a session arms under the session manager's
	// lock. A refused arm still settles (and owes) normally, but the armed debt
	// is the crash marker — without it a crash or self-update mid-turn leaves the
	// next process nothing to export — so the arm is retried off that lock.
	if !run.writeArmedDebt(0) {
		run.armRefused.Store(true)
		scheduleOpenCodeUsageArm(run, 1)
	}
	return run
}

// writeArmedDebt adds the run's ARMED debt, reporting whether the arm needs no
// further attempt. A run that settled first is declined: its settle already
// decided the debt.
func (run *openCodeUsageRun) writeArmedDebt(lockWait time.Duration) bool {
	// A contended lock never runs mutate, so track the decline, not the intent.
	declined := false
	committed, _, _ := openCodeUsageTransactionWithin(lockWait, func(ledger *openCodeUsageLedger) (bool, bool) {
		if run.settled.Load() || openCodeUsageDebtByID(ledger, run.id) != nil {
			declined = true
			return false, false
		}
		ledger.Debts = append(ledger.Debts, openCodeUsageDebt{
			RunID:              run.id,
			RunFloorMs:         run.floorMs,
			AccountFingerprint: run.fingerprint,
			Dir:                run.dir,
		})
		capOpenCodeUsageDebts(ledger)
		return true, false
	})
	return committed || declined
}

// scheduleOpenCodeUsageArm books another attempt at a refused arm on the commit
// ladder, `attempt` having already been booked. Settling stops it: a settle
// that owes writes the debt itself. Once the arm lands, a session id the stream
// named meanwhile — skipped for want of a debt — is recorded on it.
func scheduleOpenCodeUsageArm(run *openCodeUsageRun, attempt int) {
	delay, more := refreshRetryDelayForAttempt(attempt, openCodeUsageCommitMaxAttempts, openCodeUsageCommitLadder)
	if !more {
		logOpenCodeUsageCapture("arm_abandoned")
		return
	}
	logOpenCodeUsageCapture("arm_retry")
	openCodeUsageAfterFunc(delay, func() {
		if run.settled.Load() {
			return
		}
		if !run.writeArmedDebt(openCodeUsageLockWait) {
			scheduleOpenCodeUsageArm(run, attempt+1)
			return
		}
		run.armRefused.Store(false)
		if _, sessionID := run.snapshot(); sessionID != "" {
			run.persistSessionIDAsync(sessionID)
		}
	})
}

// armOpenCodeUsageRunForExecutable arms with the account the readiness cache
// last named for executable — no probe.
func armOpenCodeUsageRunForExecutable(executable, dir string) *openCodeUsageRun {
	return armOpenCodeUsageRun(executable, dir, openCodeKnownAccountFingerprint(executable))
}

// persistSessionIDAsync records the run's session id on its armed debt.
func (run *openCodeUsageRun) persistSessionIDAsync(sessionID string) {
	// A refused arm left no debt to name: the settle writes the id if it owes.
	if run == nil || sessionID == "" || run.armRefused.Load() || !run.persistedSession.CompareAndSwap(false, true) {
		return
	}
	openCodeUsageInFlight.Add(1)
	go func() {
		defer openCodeUsageInFlight.Done()
		run.persistSessionID(sessionID, 1)
	}()
}

// persistSessionID writes the session id onto the armed debt, retrying a
// REFUSED write on the commit ladder: only the first frame naming the session
// asks, so an unretried refusal would leave the debt id-less, and a crash
// before settle would then drop it as unattributable. Settling stops the
// retries — a settle that owes writes the id itself.
func (run *openCodeUsageRun) persistSessionID(sessionID string, attempt int) {
	if run.settled.Load() {
		return
	}
	// A contended lock never runs mutate, so track the decline, not the intent.
	declined, closed := false, false
	nowMs := openCodeUsageFreshnessNow().UnixMilli()
	committed, _, _ := openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		debt := openCodeUsageDebtByID(ledger, run.id)
		if debt == nil || debt.SessionID != "" || debt.owed() {
			declined = true
			return false, false
		}
		debt.SessionID = sessionID
		// The same write opens the run's ownership window, so the direct reader
		// never sees the session named but unowned.
		closed = run.syncOwnedWindow(ledger, sessionID, nowMs)
		return true, false
	})
	if committed {
		run.noteOwnedWindow(sessionID, closed)
	}
	if committed || declined {
		return
	}
	delay, more := refreshRetryDelayForAttempt(attempt, openCodeUsageCommitMaxAttempts, openCodeUsageCommitLadder)
	if !more {
		logOpenCodeUsageCapture("session_abandoned")
		return
	}
	logOpenCodeUsageCapture("session_retry")
	openCodeUsageAfterFunc(delay, func() { run.persistSessionID(sessionID, attempt+1) })
}

// settleOpenCodeUsageRun settles the run exactly once, after its stream is over
// (the native and smoke turns, a session's exit): the settle's write also
// closes the run's ownership window. sessionID is any id the caller learned
// outside the stream (a resume id); the stream's own wins.
func settleOpenCodeUsageRun(run *openCodeUsageRun, sessionID string) {
	if run == nil {
		return
	}
	run.closingSettle.Store(true)
	settleOpenCodeUsageRunOnce(run, sessionID)
}

func settleOpenCodeUsageRunOnce(run *openCodeUsageRun, sessionID string) {
	if !run.settled.CompareAndSwap(false, true) {
		return
	}
	run.commitMu.Lock()
	defer run.commitMu.Unlock()
	if !isValidOpenCodeSessionID(sessionID) {
		sessionID = ""
	}
	// A resume id stands in for a stream that named none, for every later write
	// of this run (its window, its commit retries).
	run.adoptSession(sessionID)
	steps, sessionID, from := run.takeUncommitted()
	fingerprint := run.settleFingerprint()

	if openCodeUsageStepsCarryUsage(steps) {
		if !commitOpenCodeUsageSteps(run, fingerprint, steps) {
			// The steps go back to the run, and the commit is retried: the native
			// and smoke paths settle once, and an exit-only session has no later
			// flush, so without a booking these tokens would sit in memory until
			// the process ended.
			run.untake(from, len(steps))
			scheduleOpenCodeUsageCommit(run, fingerprint, 1)
		}
		return
	}
	// Steps that contribute nothing — a valid step_finish whose every token and
	// cost is zero — are not payment: the turn still owes a reading, and its
	// export may carry one.
	if sessionID == "" {
		releaseOpenCodeUsageArmedRun(run)
		retireOpenCodeUsageDebt(run.id, "unattributable")
		return
	}
	if !oweOpenCodeUsageRun(run, sessionID, fingerprint) {
		// Same reason the commit branch books a retry: the run is settled, so no
		// later path revisits this transition. Without a booking a refused owed
		// write leaves the export unasked until some future process adopts the
		// ARMED debt — and if the arm was refused too, there is nothing to adopt.
		scheduleOpenCodeUsageOwe(run, sessionID, fingerprint, 1)
	}
}

// oweOpenCodeUsageRun marks the run's debt OWED and books its first export
// attempt, reporting whether the write landed.
func oweOpenCodeUsageRun(run *openCodeUsageRun, sessionID, fingerprint string) bool {
	now := openCodeUsageFreshnessNow()
	closed := false
	committed, _, _ := openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		closed = run.syncOwnedWindow(ledger, sessionID, now.UnixMilli())
		debt := openCodeUsageDebtByID(ledger, run.id)
		if debt == nil {
			ledger.Debts = append(ledger.Debts, openCodeUsageDebt{RunID: run.id, RunFloorMs: run.floorMs, Dir: run.dir})
			capOpenCodeUsageDebts(ledger)
			if debt = openCodeUsageDebtByID(ledger, run.id); debt == nil {
				return false, false
			}
		}
		debt.SessionID = sessionID
		debt.AccountFingerprint = fingerprint
		debt.OwedAtMs = now.UnixMilli()
		debt.SettledAtMs = now.UnixMilli()
		debt.Attempts = 0
		return true, false
	})
	if !committed {
		return false
	}
	run.noteOwnedWindow(sessionID, closed)
	logOpenCodeUsageCapture("owed")
	scheduleOpenCodeUsageAttempt(run.id, openCodeUsageDebtLadder[0])
	return true
}

// scheduleOpenCodeUsageOwe books another attempt at the OWED transition on the
// commit ladder — a refused local read-modify-rename, same as a refused commit.
// Once the budget is spent the run is dropped: its ARMED debt (if the arm
// landed) stays in the ledger for the next process to adopt.
func scheduleOpenCodeUsageOwe(run *openCodeUsageRun, sessionID, fingerprint string, attempt int) {
	delay, more := refreshRetryDelayForAttempt(attempt, openCodeUsageCommitMaxAttempts, openCodeUsageCommitLadder)
	if !more {
		releaseOpenCodeUsageArmedRun(run)
		logOpenCodeUsageCapture("owe_abandoned")
		return
	}
	logOpenCodeUsageCapture("owe_retry")
	openCodeUsageTimersMu.Lock()
	defer openCodeUsageTimersMu.Unlock()
	if t := openCodeUsageTimers[run.id]; t != nil {
		t.Stop()
	}
	openCodeUsageTimers[run.id] = openCodeUsageAfterFunc(delay, func() {
		if steps, _ := run.snapshot(); openCodeUsageStepsCarryUsage(steps) {
			// A late step landed after settle and paid the turn; flush owns it.
			clearOpenCodeUsageTimer(run.id)
			flushOpenCodeUsageRun(run)
			return
		}
		if oweOpenCodeUsageRun(run, sessionID, fingerprint) {
			return
		}
		scheduleOpenCodeUsageOwe(run, sessionID, fingerprint, attempt+1)
	})
}

// flushOpenCodeUsageRun commits steps a settled run captured after its settle
// — a session whose terminal event was followed by another step before it
// exited. No debt changes: the settle already decided those.
func flushOpenCodeUsageRun(run *openCodeUsageRun) {
	if run == nil || !run.settled.Load() {
		return
	}
	fingerprint := run.settleFingerprint()
	run.commitMu.Lock()
	defer run.commitMu.Unlock()
	steps, _, from := run.takeUncommitted()
	if !openCodeUsageStepsCarryUsage(steps) {
		return
	}
	if !commitOpenCodeUsageSteps(run, fingerprint, steps) {
		run.untake(from, len(steps))
		scheduleOpenCodeUsageCommit(run, fingerprint, 1)
		return
	}
	releaseOpenCodeUsageLiveRun(run)
}

// scheduleOpenCodeUsageCommit books another attempt at committing a settled
// run's captured steps, `attempt` having already been booked. A refused write
// spent nothing durable (another agent process held the ledger across an update
// hand-off, or the rename failed), so the retries are free and short — and
// bounded: once they are spent the run's ARMED debt is still in the ledger (the
// commit that would have retired it never landed), so the next process adopts it
// and pays it by export instead.
func scheduleOpenCodeUsageCommit(run *openCodeUsageRun, fingerprint string, attempt int) {
	delay, more := refreshRetryDelayForAttempt(attempt, openCodeUsageCommitMaxAttempts, openCodeUsageCommitLadder)
	if !more {
		releaseOpenCodeUsageArmedRun(run)
		logOpenCodeUsageCapture("commit_abandoned")
		// The steps stay in memory, but no write will offer them again. A run
		// that never committed still has its ARMED debt, which the next process
		// adopts and exports. A run whose earlier commit retired that debt gets
		// no new one: an export pays whole messages, so it would count the
		// steps already committed a second time. Those late steps go uncounted
		// — "Tokens today" is a floor, never a guess.
		run.takeUncommitted()
		releaseOpenCodeUsageLiveRun(run)
		// This ladder took over the run's timer. A debt the settle already
		// marked OWED (no commit landed, or it would have retired the debt)
		// had its export attempt there, so that attempt is booked again. The
		// attempt does nothing when no owed debt remains.
		scheduleOpenCodeUsageAttempt(run.id, openCodeUsageDebtLadder[0])
		return
	}
	logOpenCodeUsageCapture("commit_retry")
	openCodeUsageTimersMu.Lock()
	defer openCodeUsageTimersMu.Unlock()
	if t := openCodeUsageTimers[run.id]; t != nil {
		t.Stop()
	}
	openCodeUsageTimers[run.id] = openCodeUsageAfterFunc(delay, func() {
		retryOpenCodeUsageCommit(run, fingerprint, attempt)
	})
}

func retryOpenCodeUsageCommit(run *openCodeUsageRun, fingerprint string, attempt int) {
	run.commitMu.Lock()
	defer run.commitMu.Unlock()
	steps, _, from := run.takeUncommitted()
	if !openCodeUsageStepsCarryUsage(steps) {
		clearOpenCodeUsageTimer(run.id)
		releaseOpenCodeUsageLiveRun(run)
		return
	}
	if commitOpenCodeUsageSteps(run, fingerprint, steps) {
		clearOpenCodeUsageTimer(run.id)
		releaseOpenCodeUsageLiveRun(run)
		return
	}
	run.untake(from, len(steps))
	scheduleOpenCodeUsageCommit(run, fingerprint, attempt+1)
}

// commitOpenCodeUsageSteps folds steps into the ledger and retires the run's
// debt in one write, reporting whether it committed.
func commitOpenCodeUsageSteps(run *openCodeUsageRun, fingerprint string, steps []openCodeUsageStep) bool {
	sessionID := run.session()
	nowMs := openCodeUsageFreshnessNow().UnixMilli()
	closed := false
	committed, changed, generation := openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		removeOpenCodeUsageDebt(ledger, run.id)
		closed = run.syncOwnedWindow(ledger, sessionID, nowMs)
		return true, mergeOpenCodeUsageSteps(ledger, fingerprint, steps)
	})
	noteOpenCodeUsageAdvanced(committed, changed, generation)
	if committed {
		run.noteOwnedWindow(sessionID, closed)
		logOpenCodeUsageCapture("committed")
	}
	return committed
}

// settleFingerprint is the account the run counts toward: the one known at
// arm, else what the readiness cache names now.
func (run *openCodeUsageRun) settleFingerprint() string {
	if run.fingerprint != "" {
		return run.fingerprint
	}
	return openCodeKnownAccountFingerprint(run.executable)
}

// settleOrDisarmOpenCodeSmokeRun settles a smoke run. A FAILED smoke with no
// completed step owes nothing — the turn may never have reached a model, and
// an export cannot tell. Completed steps still commit: those tokens were spent.
func settleOrDisarmOpenCodeSmokeRun(run *openCodeUsageRun, succeeded bool) {
	if run == nil {
		return
	}
	if steps, _ := run.snapshot(); !succeeded && !openCodeUsageStepsCarryUsage(steps) {
		disarmOpenCodeUsageRun(run)
		return
	}
	settleOpenCodeUsageRun(run, "")
}

// disarmOpenCodeUsageRun drops a run whose child never started, or a failed
// smoke that owes nothing. A refused removal is retried like a retirement:
// left on disk, the armed debt — named, once a frame reached it — would be
// adopted and exported by the next process despite owing nothing.
func disarmOpenCodeUsageRun(run *openCodeUsageRun) {
	if run != nil && run.settled.CompareAndSwap(false, true) {
		releaseOpenCodeUsageArmedRun(run)
		retireOpenCodeUsageDebt(run.id, "disarmed")
	}
}

// disarmOpenCodeUsageRunID removes the run's debt and closes its ownership
// window, reporting whether the ledger on disk no longer holds either: a debt
// that was already absent counts as gone, a refused write (another process
// holding the ledger, or a failed rename) does not. Every caller is past the
// run's stream — a disarmed spawn, a retired debt.
func disarmOpenCodeUsageRunID(runID string) bool {
	absent := false
	nowMs := openCodeUsageFreshnessNow().UnixMilli()
	committed, _, _ := openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		removed := removeOpenCodeUsageDebt(ledger, runID)
		closed := closeOpenCodeOwnedRunByID(ledger, runID, nowMs)
		if !removed && !closed {
			absent = true
			return false, false
		}
		return true, false
	})
	return committed || absent
}

func openCodeUsageDebtByID(ledger *openCodeUsageLedger, runID string) *openCodeUsageDebt {
	for i := range ledger.Debts {
		if ledger.Debts[i].RunID == runID {
			return &ledger.Debts[i]
		}
	}
	return nil
}

func removeOpenCodeUsageDebt(ledger *openCodeUsageLedger, runID string) bool {
	for i := range ledger.Debts {
		if ledger.Debts[i].RunID == runID {
			ledger.Debts = append(ledger.Debts[:i], ledger.Debts[i+1:]...)
			return true
		}
	}
	return false
}

// capOpenCodeUsageDebts drops the oldest runs past the cap.
func capOpenCodeUsageDebts(ledger *openCodeUsageLedger) {
	for len(ledger.Debts) > openCodeUsageMaxDebts {
		oldest := 0
		for i, d := range ledger.Debts {
			if d.RunFloorMs < ledger.Debts[oldest].RunFloorMs {
				oldest = i
			}
		}
		ledger.Debts = append(ledger.Debts[:oldest], ledger.Debts[oldest+1:]...)
	}
}

func newOpenCodeUsageRunID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return time.Now().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(b)
}

// openCodeKnownAccountFingerprint is the account fingerprint the card would
// publish for executable, from the providers the readiness cache last named —
// never a probe. The capacity view keys the install by this, so a run counts
// toward the bucket the card reads. When the cache knows exactly one install
// (the usual case) its names stand in for an executable spelled differently
// (`opencode` vs the resolved path).
func openCodeKnownAccountFingerprint(executable string) string {
	openCodeReadinessMu.Lock()
	providers, ok := openCodeLastProviders[executable]
	if !ok && len(openCodeLastProviders) == 1 {
		for _, only := range openCodeLastProviders {
			providers = only
		}
	}
	openCodeReadinessMu.Unlock()
	return openCodeAccountFingerprintFor(providers)
}

// openCodeAccountFingerprintFor mirrors ParseContext: the joined provider
// names are the Account.
func openCodeAccountFingerprintFor(providers []string) string {
	account := ""
	if len(providers) > 0 {
		account = joinOpenCodeProviders(providers)
	}
	return fingerprintAccount(openCodeUsageProvider, account)
}

/* --------------------------------------------------------------------------
   Ladder
   -------------------------------------------------------------------------- */

// scheduleOpenCodeUsageAttempt books the debt's next attempt after d,
// replacing any earlier booking.
func scheduleOpenCodeUsageAttempt(runID string, d time.Duration) {
	openCodeUsageTimersMu.Lock()
	defer openCodeUsageTimersMu.Unlock()
	if t := openCodeUsageTimers[runID]; t != nil {
		t.Stop()
	}
	openCodeUsageTimers[runID] = openCodeUsageAfterFunc(d, func() { attemptOpenCodeUsageDebt(runID) })
}

func clearOpenCodeUsageTimer(runID string) {
	openCodeUsageTimersMu.Lock()
	if t := openCodeUsageTimers[runID]; t != nil {
		t.Stop()
	}
	delete(openCodeUsageTimers, runID)
	openCodeUsageTimersMu.Unlock()
}

// attemptOpenCodeUsageDebt makes one export attempt for a debt.
func attemptOpenCodeUsageDebt(runID string) {
	now := openCodeUsageFreshnessNow()
	ledger := loadOpenCodeUsageLedger()
	found := openCodeUsageDebtByID(&ledger, runID)
	if found == nil || !found.owed() {
		clearOpenCodeUsageTimer(runID)
		return
	}
	debt := *found
	owedFor := now.Sub(time.UnixMilli(debt.OwedAtMs))
	if owedFor > openCodeUsageDebtMaxAge {
		retireOpenCodeUsageDebt(runID, "aged_out")
		return
	}
	if !isValidOpenCodeSessionID(debt.SessionID) {
		// A ledger edited (or written by a build without the check) never puts
		// an unvetted id on argv.
		retireOpenCodeUsageDebt(runID, "unattributable")
		return
	}
	if live := openCodeUsageLiveRun(runID); live != nil {
		// The run is still open here: a step that lands (or a refused write that
		// retries) would commit this same turn under its streamed part key,
		// which no exported message key can dedup against. Spend nothing, and
		// let finishOpenCodeUsageRun re-book the attempt when the run is quiet.
		live.exportDeferred.Store(true)
		logOpenCodeUsageCapture("deferred_run_live")
		scheduleOpenCodeUsageAttempt(runID, refreshFreeRetryDelay(owedFor, openCodeUsageFreeFloor, openCodeUsageDebtLadder))
		return
	}
	executable := resolveOpenCodeExecutable()
	ledgerBusy := !openCodeUsageLedgerFree()
	if IsOffline() || executable == "" || ledgerBusy {
		// Spent nothing: no attempt consumed, the delay grows with the debt. A
		// ledger another process holds would refuse whatever the export paid,
		// and a refused attempt cannot be counted either — so the export is not
		// spawned at all, rather than re-run on every retry for as long as the
		// lock stays held.
		label := "deferred_offline"
		switch {
		case executable == "":
			label = "deferred_binary_missing"
		case ledgerBusy:
			label = "deferred_ledger_busy"
		}
		logOpenCodeUsageCapture(label)
		scheduleOpenCodeUsageAttempt(runID, refreshFreeRetryDelay(owedFor, openCodeUsageFreeFloor, openCodeUsageDebtLadder))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), openCodeExportTimeout)
	steps, ok := readOpenCodeExportUsage(ctx, executable, debt)
	cancel()
	// gone: the debt was paid or retired meanwhile. A write refused for any
	// other reason (another process holding the ledger) spent nothing durable,
	// so it is retried rather than leaving the debt with no attempt booked.
	gone := false
	if ok && openCodeUsageStepsCarryUsage(steps) {
		committed, changed, generation := openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
			if openCodeUsageDebtByID(ledger, runID) == nil {
				gone = true
				return false, false
			}
			removeOpenCodeUsageDebt(ledger, runID)
			return true, mergeOpenCodeUsageSteps(ledger, debt.AccountFingerprint, steps)
		})
		noteOpenCodeUsageAdvanced(committed, changed, generation)
		switch {
		case committed:
			clearOpenCodeUsageTimer(runID)
			logOpenCodeUsageCapture("export_paid")
		case gone:
			clearOpenCodeUsageTimer(runID)
		default:
			scheduleOpenCodeUsageAttempt(runID, refreshFreeRetryDelay(owedFor, openCodeUsageFreeFloor, openCodeUsageDebtLadder))
		}
		return
	}

	// The export ran and paid nothing (unreadable, or the session holds no
	// assistant message yet): an attempt is spent.
	attempts := debt.Attempts + 1
	committed, _, _ := openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		d := openCodeUsageDebtByID(ledger, runID)
		if d == nil {
			gone = true
			return false, false
		}
		d.Attempts = attempts
		return true, false
	})
	if gone {
		clearOpenCodeUsageTimer(runID)
		return
	}
	if !committed {
		// The attempt is not on disk, so it is not spent: try again later.
		scheduleOpenCodeUsageAttempt(runID, refreshFreeRetryDelay(owedFor, openCodeUsageFreeFloor, openCodeUsageDebtLadder))
		return
	}
	// The ladder is indexed by attempts already MADE: the head (15 s) was the
	// settle's booking, so attempt n books ladder[n]. refreshRetryDelayForAttempt
	// treats rung 0 as the settle's own attempt, hence the +1.
	if delay, more := refreshRetryDelayForAttempt(attempts+1, openCodeUsageDebtMaxAttempts+1, openCodeUsageDebtLadder); more {
		logOpenCodeUsageCapture("export_retry")
		scheduleOpenCodeUsageAttempt(runID, delay)
		return
	}
	retireOpenCodeUsageDebt(runID, "exhausted")
}

// openCodeUsageLedgerFree reports whether no other process holds the ledger's
// cross-process lock right now — a probe, released at once. A filesystem that
// offers no lock answers true, as the transaction would proceed there too.
func openCodeUsageLedgerFree() bool {
	path := openCodeUsageCachePath()
	lock, outcome := acquireCrossProcessCacheLockUntil(path, time.Now())
	if lock != nil {
		_ = unlockFile(lock)
		_ = lock.Close()
	}
	return outcome != crossProcessLockContended
}

// retireOpenCodeUsageDebt drops a debt no attempt can pay. The timer is cleared
// only once the removal is on disk: this is the run's terminal path, so a
// refused write with the timer already gone would leave the debt holding one of
// the ledger's slots for the rest of the process, evicting newer recoverable
// runs — hence the same bounded commit ladder a refused owed write books.
func retireOpenCodeUsageDebt(runID, label string) {
	if !disarmOpenCodeUsageRunID(runID) {
		scheduleOpenCodeUsageRetire(runID, label, 1)
		return
	}
	clearOpenCodeUsageTimer(runID)
	logOpenCodeUsageCapture(label)
}

// scheduleOpenCodeUsageRetire books another attempt at removing a retired
// debt, `attempt` having already been booked. Once the budget is spent the
// timer goes anyway: the debt is still owed on disk, so the next process adopts
// it and either pays it by export or ages it out.
func scheduleOpenCodeUsageRetire(runID, label string, attempt int) {
	delay, more := refreshRetryDelayForAttempt(attempt, openCodeUsageCommitMaxAttempts, openCodeUsageCommitLadder)
	if !more {
		clearOpenCodeUsageTimer(runID)
		logOpenCodeUsageCapture("retire_abandoned")
		return
	}
	logOpenCodeUsageCapture("retire_retry")
	openCodeUsageTimersMu.Lock()
	defer openCodeUsageTimersMu.Unlock()
	if t := openCodeUsageTimers[runID]; t != nil {
		t.Stop()
	}
	openCodeUsageTimers[runID] = openCodeUsageAfterFunc(delay, func() {
		retireOpenCodeUsageDebtAttempt(runID, label, attempt)
	})
}

func retireOpenCodeUsageDebtAttempt(runID, label string, attempt int) {
	if !disarmOpenCodeUsageRunID(runID) {
		scheduleOpenCodeUsageRetire(runID, label, attempt+1)
		return
	}
	clearOpenCodeUsageTimer(runID)
	logOpenCodeUsageCapture(label)
}

// payOwedOpenCodeUsage runs at StartAgent: every debt left by an earlier
// process is re-armed on the ladder. An armed debt (a run cut off before it
// settled) becomes owed when it named a session and is dropped otherwise. Debts
// this process armed (a smoke racing startup) are left to their own settle.
func payOwedOpenCodeUsage() {
	startedAt := openCodeUsageFreshnessNow()
	openCodeUsageInFlight.Add(1)
	go func() {
		defer openCodeUsageInFlight.Done()
		adoptOwedOpenCodeUsage(startedAt)
	}()
}

func adoptOwedOpenCodeUsage(startedAt time.Time) {
	now := openCodeUsageFreshnessNow()
	var owed []openCodeUsageDebt
	var dropped, aged int
	ran, write := false, false
	committed, _, _ := openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		ran = true
		// A window an earlier process left open belongs to a stream that died
		// with it: it closes at this process's start, and a resume after that
		// is direct use (or a new managed run with its own window).
		for i := range ledger.OwnedRuns {
			if w := &ledger.OwnedRuns[i]; w.open() && w.FromMs < startedAt.UnixMilli() {
				w.ToMs = startedAt.UnixMilli()
				write = true
			}
		}
		kept := ledger.Debts[:0]
		for _, d := range ledger.Debts {
			switch {
			case !d.owed() && d.RunFloorMs >= startedAt.UnixMilli():
				kept = append(kept, d) // this process's own run
				continue
			case !d.owed() && d.SessionID == "":
				dropped++
				write = true
				continue
			case !d.owed():
				d.OwedAtMs = now.UnixMilli()
				// The run died before this process started: a turn resumed on
				// the same session after startedAt streams and pays its own way.
				if d.SettledAtMs == 0 {
					d.SettledAtMs = startedAt.UnixMilli()
				}
				write = true
			case now.Sub(time.UnixMilli(d.OwedAtMs)) > openCodeUsageDebtMaxAge:
				aged++
				write = true
				continue
			}
			kept = append(kept, d)
			owed = append(owed, d)
		}
		ledger.Debts = kept
		return write, false
	})
	if !ran || (write && !committed) {
		// Another process held the ledger (the one this update replaces, still
		// exiting), or the write failed: none of this is on disk, so adopt again
		// once it is free rather than leave the debts with nothing booked.
		logOpenCodeUsageCapture("adopt_retry")
		openCodeUsageTimersMu.Lock()
		openCodeUsageAfterFunc(openCodeUsageFreeFloor, func() { adoptOwedOpenCodeUsage(startedAt) })
		openCodeUsageTimersMu.Unlock()
		return
	}
	for range dropped {
		logOpenCodeUsageCapture("unattributable")
	}
	for range aged {
		logOpenCodeUsageCapture("aged_out")
	}
	for _, d := range owed {
		delay := openCodeUsageDebtLadder[0]
		if next, more := refreshRetryDelayForAttempt(d.Attempts+1, openCodeUsageDebtMaxAttempts+1, openCodeUsageDebtLadder); more && d.Attempts > 0 {
			delay = next
		}
		logOpenCodeUsageCapture("resumed")
		scheduleOpenCodeUsageAttempt(d.RunID, delay)
	}
}

/* --------------------------------------------------------------------------
   Export fallback
   -------------------------------------------------------------------------- */

// runOpenCodeExport runs `opencode export <sessionID>` and returns its stdout.
// A var so tests drive the fallback without a binary.
var runOpenCodeExport = func(ctx context.Context, executable, dir, sessionID string) ([]byte, bool) {
	cmd, err := newOpenCodeCmd(ctx, openCodeLaunch{
		Path: executable,
		Args: []string{"export", sessionID},
		Env:  sanitizeOpenCodeEnv(os.Environ()),
		Dir:  dir,
		// A background read must not become the CLI's self-update.
		Maintenance: true,
	})
	if err != nil {
		return nil, false
	}
	out := &boundedBuffer{limit: openCodeExportMaxBytes}
	cmd.Stdout = out
	if cmd.Run() != nil || out.buf.Len() >= openCodeExportMaxBytes {
		return nil, false
	}
	return out.buf.Bytes(), true
}

// readOpenCodeExportUsage pays a debt from the session's export: the assistant
// messages created at or after the run floor (and, for a settled run, no later
// than its settle), one step each, keyed by message id so a second debt on the
// same session never counts a message twice. ok=false means the export could
// not be read; an unknown shape yields no steps, never a guessed number. The
// export bytes go no further than this function.
func readOpenCodeExportUsage(ctx context.Context, executable string, debt openCodeUsageDebt) ([]openCodeUsageStep, bool) {
	dir := debt.Dir
	if dir != "" {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			// The smoke's per-run directory is gone; any non-repository
			// directory resolves to the same global project.
			dir = os.TempDir()
		}
	}
	out, ok := runOpenCodeExport(ctx, executable, dir, debt.SessionID)
	if !ok {
		return nil, false
	}
	return parseOpenCodeExportUsage(out, debt), true
}

// openCodeExportMessage is one exported message, read permissively: the
// fields sit under `info` in current releases and at the top level in some.
type openCodeExportMessage struct {
	Info *openCodeExportMessageInfo `json:"info"`
	openCodeExportMessageInfo
	// Parts holds each step's own figures. OpenCode overwrites info.tokens on
	// every step (it ends as the last step's), while each step-finish part
	// keeps that step's usage, so a multi-step turn is only whole when its
	// parts are summed, as the stream tap sums step_finish events.
	Parts []openCodeExportPart `json:"parts"`
}

// openCodeExportPart is the narrow view of a message part: only a step-finish
// part's figures are read, never its text or tool output. The direct-run
// reader decodes OpenCode's stored parts into the same shape.
type openCodeExportPart struct {
	Type   string          `json:"type"`
	Tokens json.RawMessage `json:"tokens"`
	Cost   json.RawMessage `json:"cost"`
}

type openCodeExportMessageInfo struct {
	ID     string          `json:"id"`
	Role   string          `json:"role"`
	Tokens json.RawMessage `json:"tokens"`
	Cost   json.RawMessage `json:"cost"`
	Time   struct {
		Created   json.RawMessage `json:"created"`
		Completed json.RawMessage `json:"completed"`
	} `json:"time"`
}

func parseOpenCodeExportUsage(out []byte, debt openCodeUsageDebt) []openCodeUsageStep {
	var export struct {
		Messages []openCodeExportMessage `json:"messages"`
	}
	if json.Unmarshal(out, &export) != nil {
		return nil
	}
	var steps []openCodeUsageStep
	for _, m := range export.Messages {
		info := m.openCodeExportMessageInfo
		if m.Info != nil {
			info = *m.Info
		}
		if info.Role != "assistant" || info.ID == "" {
			continue
		}
		created, ok := openCodeUsageMillis(info.Time.Created)
		if !ok || created < debt.RunFloorMs {
			continue
		}
		if debt.SettledAtMs > 0 && created > debt.SettledAtMs {
			continue // a later turn on the same session pays its own debt
		}
		step, valid := openCodeExportMessageUsage(m, info)
		if !valid {
			continue
		}
		step.AtMs = created
		if completed, ok := openCodeUsageMillis(info.Time.Completed); ok {
			step.AtMs = completed
		}
		step.Key = openCodeUsageStepKey(debt.SessionID, "message:"+info.ID)
		steps = append(steps, step)
	}
	return steps
}

// openCodeExportMessageUsage is one exported assistant message's usage: the
// per-field max of its summed step-finish parts and its info. info.tokens alone
// is the last step's, but it is all an export without parts carries, and
// info.cost is already cumulative. valid is false when neither carries a
// readable figure.
func openCodeExportMessageUsage(m openCodeExportMessage, info openCodeExportMessageInfo) (openCodeUsageStep, bool) {
	var summed openCodeUsageStep
	stepsValid := false
	for _, part := range m.Parts {
		if !isOpenCodeStepFinishType(part.Type) {
			continue
		}
		s, ok := openCodeUsageFromTokens(part.Tokens, part.Cost)
		if !ok {
			continue
		}
		stepsValid = true
		summed.Input = addOpenCodeUsageCount(summed.Input, s.Input)
		summed.Output = addOpenCodeUsageCount(summed.Output, s.Output)
		summed.Reasoning = addOpenCodeUsageCount(summed.Reasoning, s.Reasoning)
		summed.CacheRead = addOpenCodeUsageCount(summed.CacheRead, s.CacheRead)
		summed.CacheWrite = addOpenCodeUsageCount(summed.CacheWrite, s.CacheWrite)
		summed.Cost = addOpenCodeUsageCost(summed.Cost, s.Cost)
	}
	fromInfo, infoValid := openCodeUsageFromTokens(info.Tokens, info.Cost)
	switch {
	case stepsValid && infoValid:
		return openCodeUsageStep{
			Input:      max(summed.Input, fromInfo.Input),
			Output:     max(summed.Output, fromInfo.Output),
			Reasoning:  max(summed.Reasoning, fromInfo.Reasoning),
			CacheRead:  max(summed.CacheRead, fromInfo.CacheRead),
			CacheWrite: max(summed.CacheWrite, fromInfo.CacheWrite),
			Cost:       max(summed.Cost, fromInfo.Cost),
		}, true
	case stepsValid:
		return summed, true
	default:
		return fromInfo, infoValid
	}
}

// resetOpenCodeUsageFreshness stops every booked attempt (tests).
func resetOpenCodeUsageFreshness() {
	openCodeUsageTimersMu.Lock()
	for id, t := range openCodeUsageTimers {
		t.Stop()
		delete(openCodeUsageTimers, id)
	}
	openCodeUsageTimersMu.Unlock()
	openCodeUsageLiveMu.Lock()
	openCodeUsageLiveRuns = map[string]*openCodeUsageRun{}
	openCodeUsageArmedRuns = map[string]*openCodeUsageRun{}
	openCodeUsageLiveMu.Unlock()
	openCodeUsageInFlight.Wait()
}
