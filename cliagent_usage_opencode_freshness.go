// cliagent_usage_opencode_freshness.go — the bounded run-completion
// reconciliation that keeps OpenCode's usage reading current after a run.
//
// Why this exists:
//
//	The ledger (cliagent_usage_opencode_capture.go) is filled from a run's own
//	JSON stream, and three kinds of run never produce one we can read: an
//	`opencode` the user ran in their own shell or TUI, every `execute` and Unix
//	PTY run (both terminal-managed, never tapped), and a stream cut off by a
//	timeout, a kill or an agent self-update. Those runs owe a reconcile through
//	OpenCode's own CLI (cliagent_usage_opencode_store.go).
//
// It mirrors cliagent_usage_grok_freshness.go deliberately, minus Grok's
// account fencing (OpenCode has no login of its own, so there is nothing to
// fence a debt to) — so a fix in one ports to the other:
//
//   - Arm. Every managed spawn of `opencode` persists a floor before the child
//     starts. A spawn that fails disarms it.
//   - Settle. A run whose stream ended cleanly WITH tokens is covered; every
//     other run owes ONE reconcile. One pending debt at a time: a newer owing
//     run moves the completion forward and keeps the attempt count.
//   - Pay. A process-wide single-flight worker spends at most
//     openCodeRunDebtMaxAttempts passes per debt on the shared ladder
//     (cliagent_usage_refresh_ladder.go), spaced by openCodeReconcileMinInterval.
//   - Survive. Debt, rung and continuation are persisted; StartAgent re-arms
//     them (payOwedOpenCodeUsageRefresh), and a floor the previous process
//     never settled owes one reconcile.
//   - Discover. The gather's parser nudges the same worker when a session
//     directory changed since the last pass started, on a cooldown.
//
// Redaction: the state file holds epoch-millisecond integers, counters and a
// closed outcome code. Log lines carry fixed labels and counters only.
package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// openCodeRunDebtMaxAttempts is a debt's lifetime budget of reconcile
	// passes, across the settle's own attempt and every scheduled rung.
	openCodeRunDebtMaxAttempts = 4
	// openCodeRunDebtMaxAge retires a debt nothing could pay.
	openCodeRunDebtMaxAge = 6 * time.Hour
	// openCodeContinuationMaxFailures bounds a no-debt continuation chain's
	// consecutive failures.
	openCodeContinuationMaxFailures = 4
	// openCodeContinuationMaxPasses bounds ONE chain's total passes, successful
	// ones included: `more` requires progress, so a chain always terminates,
	// but nothing otherwise bounds HOW LONG it spawns `opencode` for. At three
	// exports a pass this covers 96 sessions; past it the day is a lower bound
	// and a new run's debt, a click or a fresh nudge starts a new chain.
	openCodeContinuationMaxPasses = 32

	openCodeUsageFreshnessSchema = 1
	// openCodeUsageFreshnessEnv relocates the state file (tests isolate from
	// the real machine).
	openCodeUsageFreshnessEnv = "AIEXPEDITE_OPENCODE_USAGE_FRESHNESS"
)

// Vars rather than consts so tests can pin them small.
var (
	// openCodeRunDebtRetryLadder is the delay before the next pass, by the
	// attempts already booked. The first rung equals the spacing so a rung
	// never fires only to defer on it.
	openCodeRunDebtRetryLadder = []time.Duration{
		time.Minute, 2 * time.Minute, 8 * time.Minute, 30 * time.Minute,
	}
	// openCodeRunDebtFreeRetryDelay is the floor of the age backoff after an
	// outcome that spent no budget (offline, draining, `more`).
	openCodeRunDebtFreeRetryDelay = 30 * time.Second
	// openCodeReconcileMinInterval spaces any two reconcile passes.
	openCodeReconcileMinInterval = time.Minute
	// openCodeReconcileNudgeCooldown bounds how often the gather may arm the
	// worker.
	openCodeReconcileNudgeCooldown = 2 * time.Minute
)

// openCodeUsageFreshness is the persisted debt state. Every field is a number
// or a closed outcome code.
type openCodeUsageFreshness struct {
	SchemaVersion int `json:"schemaVersion,omitempty"`
	// RunFloorMs is the newest armed floor not yet settled, or the pending
	// debt's floor. A restart that finds it with no debt beside it owes the
	// interrupted run one reconcile.
	RunFloorMs int64 `json:"runFloorMs,omitempty"`
	// CompletionMs is the instant a reconcile must COMPLETE after to pay the
	// debt; OwedAtMs is when the debt was created (0 = nothing owed).
	CompletionMs int64 `json:"completionMs,omitempty"`
	OwedAtMs     int64 `json:"owedAtMs,omitempty"`
	// NextAttemptAtMs is the booked rung (0 = none), persisted so a restart
	// re-arms the same schedule.
	NextAttemptAtMs int64 `json:"nextAttemptAtMs,omitempty"`
	// LastAttemptAtMs is the last pass, which the spacing measures.
	LastAttemptAtMs int64  `json:"lastAttemptAtMs,omitempty"`
	Attempts        int    `json:"attempts,omitempty"`
	LastOutcome     string `json:"lastOutcome,omitempty"`
}

func (state openCodeUsageFreshness) owed() bool { return state.OwedAtMs != 0 }

func (state *openCodeUsageFreshness) clearDebt() {
	state.CompletionMs, state.OwedAtMs, state.NextAttemptAtMs = 0, 0, 0
	state.Attempts, state.LastOutcome = 0, ""
}

// openCodeDebtID names one generation of the debt, so a rung booked for a debt
// that was since paid or replaced writes nothing.
type openCodeDebtID struct{ owedAtMs, completionMs int64 }

func (state openCodeUsageFreshness) debtID() openCodeDebtID {
	return openCodeDebtID{owedAtMs: state.OwedAtMs, completionMs: state.CompletionMs}
}

var (
	openCodeFreshnessMu sync.Mutex
	// openCodeUsageRefreshEnabled is set only by StartAgent: tests that settle
	// an OpenCode run without opting in must never spawn a CLI.
	openCodeUsageRefreshEnabled atomic.Bool
	// openCodeFreshnessInFlight counts the worker and timer callbacks, so
	// tests and the shutdown drain can wait them out.
	openCodeFreshnessInFlight atomic.Int64

	openCodeWorkerMu      sync.Mutex
	openCodeWorkerRunning bool
	openCodeWorkerRearm   bool

	openCodeLiveRunsMu sync.Mutex
	// openCodeLiveRuns holds the armed floors of this process's runs.
	openCodeLiveRuns = map[int64]int{}

	openCodeRunDebtRetryTimer struct {
		mu    sync.Mutex
		timer *time.Timer
		gen   uint64
	}
	// openCodeSettleRetry is the ONE in-memory retry of a settle whose debt
	// write was refused. It is not persisted: the file it would be persisted to
	// is the one that could not be written.
	openCodeSettleRetry struct {
		mu           sync.Mutex
		timer        *time.Timer
		gen          uint64
		floorMs      int64
		completionMs int64
		attempt      int
	}
	// openCodeRecheckBookRetry is the ONE in-memory retry of a re-read booking
	// whose continuationDue write was refused. Like openCodeSettleRetry it is
	// not persisted, because the file it would go to is the one that refused.
	openCodeRecheckBookRetry struct {
		mu    sync.Mutex
		timer *time.Timer
		gen   uint64
	}
	// openCodeContinuationBookRetry is the ONE in-memory retry of a
	// continuation booking whose continuationDue write was refused. Same reason
	// it cannot be persisted as the two above: the file is the one that
	// refused. It carries the booking the refused write was making, so the
	// retry books exactly what the outcome decided.
	openCodeContinuationBookRetry struct {
		mu               sync.Mutex
		timer            *time.Timer
		gen              uint64
		failures         int
		firstFailureAtMs int64
		delay            time.Duration
		attempt          int
	}
	openCodeReconcileNudge struct {
		mu     sync.Mutex
		lastAt time.Time
	}
)

// SetOpenCodeUsageRefreshEnabled arms (or disarms) the run-completion
// reconciliation for this process. Called from StartAgent.
func SetOpenCodeUsageRefreshEnabled(enabled bool) { openCodeUsageRefreshEnabled.Store(enabled) }

func openCodeUsageFreshnessPath() string {
	if p := os.Getenv(openCodeUsageFreshnessEnv); p != "" {
		return p
	}
	return filepath.Join(GetConfigDir(), "opencode_usage_freshness.json")
}

// readOpenCodeUsageFreshness loads the state without mutating it. A missing or
// corrupt file reads as "nothing owed".
func readOpenCodeUsageFreshness() openCodeUsageFreshness {
	openCodeFreshnessMu.Lock()
	defer openCodeFreshnessMu.Unlock()
	var state openCodeUsageFreshness
	if !readJSONFile(openCodeUsageFreshnessPath(), &state) {
		return openCodeUsageFreshness{}
	}
	return state
}

// updateOpenCodeUsageFreshness applies mutate under the freshness lock and
// persists the result when it changed (temp file + rename, removed once empty).
// A missing or corrupt file reads as "nothing owed". Nothing under this lock
// may take the ledger lock.
func updateOpenCodeUsageFreshness(mutate func(*openCodeUsageFreshness)) openCodeUsageFreshness {
	state, _ := updateOpenCodeUsageFreshnessChecked(mutate)
	return state
}

// updateOpenCodeUsageFreshnessChecked is the same mutation, and also reports
// whether the new state reached DISK — the mirror of updateOpenCodeUsageLedger.
// Only the settle needs it: every other write here is best-effort scheduling
// state, while the settle's debt is the one record that the run happened at all.
// A mutation that changed nothing, or had nothing to write, counts as persisted.
func updateOpenCodeUsageFreshnessChecked(mutate func(*openCodeUsageFreshness)) (openCodeUsageFreshness, bool) {
	openCodeFreshnessMu.Lock()
	defer openCodeFreshnessMu.Unlock()
	path := openCodeUsageFreshnessPath()
	var state openCodeUsageFreshness
	if !readJSONFile(path, &state) {
		state = openCodeUsageFreshness{}
	}
	before := state
	mutate(&state)
	if state == before {
		return state, true
	}
	if path == "" {
		return state, false
	}
	if state.RunFloorMs == 0 && !state.owed() && state.LastAttemptAtMs == 0 {
		_ = os.Remove(path)
		return state, true
	}
	state.SchemaVersion = openCodeUsageFreshnessSchema
	return state, writeJSONFileAtomic(path, state)
}

// openCodeRebaseFutureFreshness pulls back timestamps a backwards clock step
// left in the future (mirrors grokRebaseFutureFreshness).
func openCodeRebaseFutureFreshness(state *openCodeUsageFreshness, now time.Time) {
	nowMs := now.UnixMilli()
	ceiling := now.Add(openCodeUsageMaxClockSkew).UnixMilli()
	for _, ts := range []*int64{&state.RunFloorMs, &state.CompletionMs, &state.OwedAtMs, &state.LastAttemptAtMs} {
		if *ts > ceiling {
			*ts = nowMs
		}
	}
	longest := openCodeRunDebtRetryLadder[len(openCodeRunDebtRetryLadder)-1]
	if state.NextAttemptAtMs > now.Add(longest+openCodeUsageMaxClockSkew).UnixMilli() {
		state.NextAttemptAtMs = nowMs
	}
}

// openCodeUsageMaxClockSkew is how far ahead of us a persisted stamp may sit
// before it is treated as a backwards clock step rather than a real future.
const openCodeUsageMaxClockSkew = 5 * time.Minute

// openCodeDebtExhausted reports a debt whose pass budget is spent. It stays on
// disk (so later runs cannot re-open the budget) until the longest rung has
// passed since its last pass.
func openCodeDebtExhausted(state openCodeUsageFreshness, now time.Time) bool {
	if state.Attempts < openCodeRunDebtMaxAttempts {
		return false
	}
	cooling := openCodeRunDebtRetryLadder[len(openCodeRunDebtRetryLadder)-1]
	return now.Sub(time.UnixMilli(state.LastAttemptAtMs)) < cooling
}

/* ──────────────────────────────── arm ──────────────────────────────── */

// armOpenCodeUsageRunFloor persists a run floor. Called by
// armOpenCodeUsageRun; the write happens off the caller's goroutine, as
// Antigravity's and Grok's arms do, because a spawn site may hold its own mutex
// across the spawn and a slow disk must not stall it. A persist that lands
// after the run already settled is skipped.
func armOpenCodeUsageRunFloor(floor time.Time) {
	if floor.IsZero() {
		return
	}
	floorMs := floor.UnixMilli()
	openCodeLiveRunsMu.Lock()
	openCodeLiveRuns[floorMs]++
	openCodeLiveRunsMu.Unlock()
	openCodeFreshnessInFlight.Add(1)
	go func() {
		defer openCodeFreshnessInFlight.Add(-1)
		updateOpenCodeUsageFreshness(func(state *openCodeUsageFreshness) {
			if !openCodeRunIsLive(floorMs) {
				return
			}
			openCodeRebaseFutureFreshness(state, floor)
			if floorMs > state.RunFloorMs {
				state.RunFloorMs = floorMs
			}
		})
	}()
}

func openCodeRunIsLive(floorMs int64) bool {
	openCodeLiveRunsMu.Lock()
	defer openCodeLiveRunsMu.Unlock()
	return openCodeLiveRuns[floorMs] > 0
}

// openCodeReleaseLiveRun forgets an armed floor and returns the oldest one
// still live (zero when none).
func openCodeReleaseLiveRun(floorMs int64) int64 {
	openCodeLiveRunsMu.Lock()
	defer openCodeLiveRunsMu.Unlock()
	if openCodeLiveRuns[floorMs] > 1 {
		openCodeLiveRuns[floorMs]--
	} else {
		delete(openCodeLiveRuns, floorMs)
	}
	return openCodeOldestLiveFloorLocked()
}

// openCodeOldestLiveFloor is the earliest run still armed (zero when none).
// Lock order: freshness -> live runs; never the reverse.
func openCodeOldestLiveFloor() int64 {
	openCodeLiveRunsMu.Lock()
	defer openCodeLiveRunsMu.Unlock()
	return openCodeOldestLiveFloorLocked()
}

func openCodeOldestLiveFloorLocked() int64 {
	oldest := int64(0)
	for f := range openCodeLiveRuns {
		if oldest == 0 || f < oldest {
			oldest = f
		}
	}
	return oldest
}

// openCodeDropSettledFloor rolls the restart marker back once its run is
// accounted for — to the oldest run still live, or nothing.
func openCodeDropSettledFloor(state *openCodeUsageFreshness, floorMs, oldestLive int64) {
	if state.RunFloorMs == 0 || state.RunFloorMs > floorMs || state.owed() {
		return
	}
	state.RunFloorMs = oldestLive
}

// disarmOpenCodeUsageRunFloor withdraws a run that never reached inference.
func disarmOpenCodeUsageRunFloor(floor time.Time) {
	if floor.IsZero() {
		return
	}
	oldest := openCodeReleaseLiveRun(floor.UnixMilli())
	updateOpenCodeUsageFreshness(func(state *openCodeUsageFreshness) {
		openCodeDropSettledFloor(state, floor.UnixMilli(), oldest)
	})
}

/* ─────────────────────────────── settle ─────────────────────────────── */

// settleOpenCodeUsageRun decides, when a run ends, whether it owes a reconcile,
// and starts the worker when it does. Returns true when a debt is owed.
//
// A COVERED settle never touches an existing debt: only a pass that completes
// after the debt's completion time pays it. A clean smoke after an owed execute
// or direct run therefore leaves that debt in place.
func settleOpenCodeUsageRun(floor time.Time, covered bool, label string) bool {
	if floor.IsZero() {
		return false
	}
	now := openCodeUsageNow()
	floorMs := floor.UnixMilli()
	completionMs := openCodeCompletionMs(now)
	if completionMs < floorMs {
		completionMs = floorMs
	}
	oldest := openCodeReleaseLiveRun(floorMs)
	if covered {
		updateOpenCodeUsageFreshness(func(state *openCodeUsageFreshness) {
			openCodeDropSettledFloor(state, floorMs, oldest)
		})
		return false
	}
	state, persisted := openCodeWriteOwedDebt(floorMs, completionMs, now)
	if !persisted {
		// The debt is the ONLY record that this run's figures are missing, and
		// the worker reads it back from disk — so a refused write (a read-only
		// or full config dir, which is also what makes a covered run's ledger
		// write fail and land it here) would lose the run silently. The debt is
		// retried in memory until the filesystem recovers.
		logOpenCodeUsage("debt write refused by=%s", label)
		openCodeArmSettleRetry(floorMs, completionMs, label, 1)
		return false
	}
	if !state.owed() {
		return false
	}
	logOpenCodeUsage("owed by=%s attempts=%d", label, state.Attempts)
	startOpenCodeReconcileWorker()
	return true
}

// openCodeWriteOwedDebt opens (or moves forward) the pending debt for a run
// that ended uncovered, and reports whether it reached disk.
func openCodeWriteOwedDebt(floorMs, completionMs int64, now time.Time) (openCodeUsageFreshness, bool) {
	return updateOpenCodeUsageFreshnessChecked(func(state *openCodeUsageFreshness) {
		openCodeOweReconcile(state, now, completionMs)
		if state.RunFloorMs < floorMs {
			state.RunFloorMs = floorMs
		}
	})
}

// openCodeArmSettleRetry re-attempts a settle whose freshness write was
// refused, on the debt ladder's own delays and for its own attempt budget.
//
// There is ONE pending retry, because there is one pending debt: a second
// refused settle folds into it, keeping the OLDEST floor (the earliest run
// still unaccounted for) and the NEWEST completion (the pass that pays it must
// finish after the last of them) — exactly what openCodeOweReconcile would have
// merged on disk.
//
// A shutdown stops it: the state it would write is the state the next process
// adopts, and that process re-reads the same unsettled floor from the ledger
// arm — which is itself a write that failed on a filesystem this broken, so
// there is nothing left to hand over.
func openCodeArmSettleRetry(floorMs, completionMs int64, label string, attempt int) {
	delay, ok := refreshRetryDelayForAttempt(attempt, openCodeRunDebtMaxAttempts, openCodeRunDebtRetryLadder)
	if !ok || IsShutdownInProgress() {
		logOpenCodeUsage("debt write gave up attempts=%d", attempt)
		return
	}
	r := &openCodeSettleRetry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
		// Fold into the pending retry rather than racing it.
		if r.floorMs > 0 && r.floorMs < floorMs {
			floorMs = r.floorMs
		}
		if r.completionMs > completionMs {
			completionMs = r.completionMs
		}
		if r.attempt > attempt {
			attempt = r.attempt
		}
	}
	r.gen++
	gen, label := r.gen, openCodeUsageRunLabel(label)
	r.floorMs, r.completionMs, r.attempt = floorMs, completionMs, attempt
	r.timer = time.AfterFunc(delay, func() {
		openCodeRetrySettleFired(gen, floorMs, completionMs, label, attempt)
	})
}

func openCodeRetrySettleFired(gen uint64, floorMs, completionMs int64, label string, attempt int) {
	r := &openCodeSettleRetry
	r.mu.Lock()
	stale := gen != r.gen
	if !stale {
		r.timer = nil
	}
	r.mu.Unlock()
	if stale || IsShutdownInProgress() {
		return
	}
	openCodeFreshnessInFlight.Add(1)
	defer openCodeFreshnessInFlight.Add(-1)
	state, persisted := openCodeWriteOwedDebt(floorMs, completionMs, openCodeUsageNow())
	if !persisted {
		openCodeArmSettleRetry(floorMs, completionMs, label, attempt+1)
		return
	}
	if !state.owed() {
		return
	}
	logOpenCodeUsage("owed by=%s attempts=%d", label, state.Attempts)
	startOpenCodeReconcileWorker()
}

// stopOpenCodeSettleRetry withdraws a pending settle retry. Part of teardown.
func stopOpenCodeSettleRetry() {
	r := &openCodeSettleRetry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	r.gen++
	r.floorMs, r.completionMs, r.attempt = 0, 0, 0
}

// openCodeSettleRetryPending reports whether a settle retry is armed. Test seam.
func openCodeSettleRetryPending() bool {
	r := &openCodeSettleRetry
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.timer != nil
}

// settleOpenCodeUsageRunAsync settles off the caller's goroutine — an exit path
// that must not wait on a disk write — counted so waiters see it in flight.
func settleOpenCodeUsageRunAsync(handle *openCodeRunUsage, covered bool) {
	if handle == nil {
		return
	}
	openCodeFreshnessInFlight.Add(1)
	go func() {
		defer openCodeFreshnessInFlight.Add(-1)
		handle.Finish(covered)
	}()
}

// openCodeCompletionMs is a completion instant rounded UP to the millisecond,
// so `completedAt >= openCodeCompletionMs(t)` holds only for a pass that
// finished at or after t.
func openCodeCompletionMs(t time.Time) int64 {
	ms := t.UnixMilli()
	if t.Sub(time.UnixMilli(ms)) > 0 {
		ms++
	}
	return ms
}

// openCodeOweReconcile opens a debt for a pass completing at or after
// completionMs, or moves the pending one forward. The attempt count survives a
// newer run: only a paid debt or a cooled-off exhausted budget resets it.
func openCodeOweReconcile(state *openCodeUsageFreshness, now time.Time, completionMs int64) {
	openCodeRebaseFutureFreshness(state, now)
	if state.owed() && now.Sub(time.UnixMilli(state.OwedAtMs)) > openCodeRunDebtMaxAge {
		state.clearDebt()
	}
	if state.owed() && state.Attempts >= openCodeRunDebtMaxAttempts && !openCodeDebtExhausted(*state, now) {
		state.clearDebt()
	}
	if state.owed() && state.Attempts >= openCodeRunDebtMaxAttempts {
		// Budget spent and still cooling: nothing is re-opened, so the new run
		// adds no attempts (grokDebtExhausted's rule).
		return
	}
	if !state.owed() {
		state.OwedAtMs = now.UnixMilli()
	}
	if completionMs > state.CompletionMs {
		state.CompletionMs = completionMs
	}
}

// settleOpenCodeFreshnessForPass retires whatever a completed pass covers: the
// debt, when the pass finished at or after its completion, and the restart
// marker when no run is still live.
func settleOpenCodeFreshnessForPass(state *openCodeUsageFreshness, completedAtMs, oldestLive int64) {
	if state.owed() && completedAtMs >= state.CompletionMs {
		state.clearDebt()
	}
	if !state.owed() && state.RunFloorMs != 0 && completedAtMs >= state.RunFloorMs {
		state.RunFloorMs = oldestLive
	}
}

/* ───────────────────────────────── pay ──────────────────────────────── */

// openCodeRetryKind says which rung the ladder books next.
type openCodeRetryKind int

const (
	// openCodeRetryAfterPass: a pass spent budget and failed.
	openCodeRetryAfterPass openCodeRetryKind = iota
	// openCodeRetryFree: an outcome that spent no budget (offline, draining,
	// `more`, a spacing deferral), backed off by the debt's age instead.
	openCodeRetryFree
)

// startOpenCodeReconcileWorker runs the payment under a process-wide single
// flight. A request that finds the flight held records a re-arm, so the running
// worker takes another pass instead of the new debt going unworked.
func startOpenCodeReconcileWorker() {
	if !openCodeUsageRefreshEnabled.Load() {
		return
	}
	openCodeFreshnessInFlight.Add(1)
	openCodeWorkerMu.Lock()
	if openCodeWorkerRunning {
		openCodeWorkerRearm = true
		openCodeWorkerMu.Unlock()
		openCodeFreshnessInFlight.Add(-1)
		return
	}
	openCodeWorkerRunning, openCodeWorkerRearm = true, false
	openCodeWorkerMu.Unlock()
	go func() {
		defer openCodeFreshnessInFlight.Add(-1)
		for {
			openCodePayReconcile(context.Background(), false)

			openCodeWorkerMu.Lock()
			if !openCodeWorkerRearm {
				openCodeWorkerRunning = false
				openCodeWorkerMu.Unlock()
				return
			}
			openCodeWorkerRearm = false
			openCodeWorkerMu.Unlock()
		}
	}()
}

// openCodeUsageRefreshWaitFor waits at most d for the worker (and any rung
// firing) to go idle, and reports whether it did.
func openCodeUsageRefreshWaitFor(d time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	for openCodeFreshnessInFlight.Load() != 0 {
		if ctx.Err() != nil {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

// openCodePendingDebt returns the unpaid debt, rebasing a rolled-back clock and
// retiring one past openCodeRunDebtMaxAge.
func openCodePendingDebt(now time.Time) (openCodeUsageFreshness, bool) {
	oldest := openCodeOldestLiveFloor()
	agedOut := false
	state := updateOpenCodeUsageFreshness(func(state *openCodeUsageFreshness) {
		openCodeRebaseFutureFreshness(state, now)
		if state.owed() && now.Sub(time.UnixMilli(state.OwedAtMs)) > openCodeRunDebtMaxAge {
			state.clearDebt()
			agedOut = true
			state.RunFloorMs = oldest
		}
	})
	if agedOut {
		// A debt nothing could pay in six hours leaves whatever it never
		// reconciled uncounted, so today is a lower bound. Unconditionally: the
		// debt may have opened after an unrelated successful pass and never
		// have had a pass of its own, in which case the ledger shows no backlog
		// at all while the run's usage is missing.
		openCodeMarkTodayPartialNow()
	}
	return state, state.owed()
}

// openCodePayReconcile spends at most one reconcile pass and books whatever the
// outcome calls for. It is the ONE entry point for both callers: the debt
// worker (forced=false) and the Refresh click's live probe (forced=true, which
// bypasses the "nothing asked" gate and the pass spacing, as Grok's smoke
// bypass does). It returns the pass's closed outcome, or "" when no pass ran.
//
// There is ONE retry timer per failure, never two:
//
//   - With a DEBT open the ladder is the timer. A timeout / launch_error spends
//     one attempt; offline and draining take the ladder's free retry. Every
//     debt-open outcome CLEARS continuationDue and leaves continuationFailures
//     untouched, so a `more` from an earlier nudge cannot also book a pass.
//   - With NO debt open the continuation is the only timer. A failure keeps
//     continuationDue, counts it, and books one pass off the age of the current
//     run of consecutive failures; offline and draining book the same single
//     retry without counting. After openCodeContinuationMaxFailures the chain
//     stops and today is marked partial if candidates were still queued.
func openCodePayReconcile(parent context.Context, forced bool) string {
	now := openCodeUsageNow()
	state, owed := openCodePendingDebt(now)
	hasDebt := owed
	continuationDue := readOpenCodeUsageLedger().ContinuationDue
	if !forced && !hasDebt && !continuationDue {
		return ""
	}
	if !forced {
		if hasDebt && (openCodeDebtExhausted(state, now) || state.Attempts >= openCodeRunDebtMaxAttempts) {
			// Logged, not just recorded: "why has it not reconciled?" is the
			// question a stale card raises, and these two are the only
			// outcomes that answer it without a pass having run.
			logOpenCodeUsage("exhausted attempts=%d", state.Attempts)
			openCodeMarkTodayPartialNow()
			return ""
		}
		// The spacing bounds EVERY pass, not just a debt's. A continuation
		// chain is armed on the free-retry floor, so without this a backlog
		// would spawn `opencode` twice a minute for the chain's whole budget —
		// on the machine the user is working on.
		if !state.passSpacingElapsed(now) {
			logOpenCodeUsage("deferred attempts=%d", state.Attempts)
			if hasDebt {
				openCodeScheduleRunDebtRetry(state, now, openCodeRetryFree)
			} else {
				openCodeArmContinuation(openCodeSpacedDelay(state, now, openCodeRunDebtFreeRetryDelay))
			}
			return ""
		}
	}
	if IsShutdownInProgress() {
		return ""
	}
	if IsOffline() {
		logOpenCodeUsage("%s", openCodeReconcileOffline)
		openCodeBookAfterOutcome(hasDebt, openCodeReconcileOffline, now)
		return openCodeReconcileOffline
	}

	if !hasDebt {
		// Charged before the pass, so a chain cannot outlive its budget by
		// crashing between the spawn and the bookkeeping.
		updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
			if !l.ContinuationDue {
				return openCodeLedgerEdit{}
			}
			l.ContinuationPasses++
			return openCodeLedgerEdit{Changed: true}
		})
	}

	id := state.debtID()
	result, led := reconcileOpenCodeUsageLeading(parent, now)
	completedAt := openCodeUsageNow()
	outcome := result.Outcome
	if !led {
		// Joined a pass another caller is running (a Refresh click overlapping
		// the debt worker): that caller books it. Booking it here too would
		// charge one shared failure twice against the debt's attempts.
		logOpenCodeUsage("joined %s", outcome)
		openCodeBookDebtOpenedMidPass(completedAt)
		return outcome
	}

	booked := updateOpenCodeUsageFreshness(func(s *openCodeUsageFreshness) {
		s.LastAttemptAtMs = completedAt.UnixMilli()
		if !hasDebt || s.debtID() != id {
			return
		}
		s.LastOutcome = outcome
		switch {
		case openCodeReconcileSucceeded(outcome):
			settleOpenCodeFreshnessForPass(s, completedAt.UnixMilli(), openCodeOldestLiveFloor())
		case outcome == openCodeReconcileUnsupported:
			// No pass can ever pay it: retire rather than ladder.
			s.clearDebt()
			s.RunFloorMs = openCodeOldestLiveFloor()
		case outcome == openCodeReconcileMore:
			// Progress was committed but candidates remain; the rung is free.
		default:
			if s.Attempts < openCodeRunDebtMaxAttempts {
				s.Attempts++
			}
		}
	})
	logOpenCodeUsage("%s attempts=%d exported=%d remaining=%d", outcome, booked.Attempts,
		result.Exported, result.Remaining)
	openCodeBookAfterOutcome(hasDebt, outcome, completedAt)
	return outcome
}

// openCodeBookDebtOpenedMidPass books a free rung for an open debt that has
// none. A run that finishes while a forced, debt-free pass is in flight opens a
// debt whose worker then joins that pass; the leader snapshotted "no debt" and
// books nothing, so without this the debt would sit unscheduled until a restart.
func openCodeBookDebtOpenedMidPass(now time.Time) {
	if state, owed := openCodePendingDebt(now); owed && state.NextAttemptAtMs == 0 {
		openCodeScheduleRunDebtRetry(state, now, openCodeRetryFree)
	}
}

// passSpacingElapsed reports whether openCodeReconcileMinInterval has passed
// since the last pass. A stamp in the future (a clock step) does not hold a
// pass back.
func (state openCodeUsageFreshness) passSpacingElapsed(now time.Time) bool {
	if state.LastAttemptAtMs <= 0 {
		return true
	}
	since := now.Sub(time.UnixMilli(state.LastAttemptAtMs))
	return since < 0 || since >= openCodeReconcileMinInterval
}

// openCodeBookAfterOutcome applies the one-timer rule above.
func openCodeBookAfterOutcome(hadDebt bool, outcome string, now time.Time) {
	// This outcome decides the schedule, so a booking retry left over from the
	// previous one is superseded — the branches below arm a fresh one if their
	// own write is refused.
	stopOpenCodeContinuationBookRetry()
	if hadDebt {
		// The ladder owns the retry; a debt-open pass never leaves a
		// continuation booked — and never touches the chain's failure count,
		// which belongs to the no-debt path below.
		clearOpenCodeContinuationDue()
		if openCodeReconcileSucceeded(outcome) {
			// The debt is paid, so the ladder is done: a re-read the pass
			// booked for a session it may have caught mid-turn is now the only
			// thing left to run, and needs the timer the ladder gave back.
			openCodeBookRecheckWakeUp(now)
			return
		}
		if outcome == openCodeReconcileUnsupported {
			return
		}
		kind := openCodeRetryAfterPass
		if outcome == openCodeReconcileOffline || outcome == openCodeReconcileMore {
			kind = openCodeRetryFree
		}
		fresh, owed := openCodePendingDebt(now)
		if !owed {
			return
		}
		if !openCodeScheduleRunDebtRetry(fresh, now, kind) {
			// The ladder has nothing left to book, and this debt's run was
			// never reconciled — the same unconditional mark the aged-out debt
			// takes, and for the same reason.
			openCodeMarkTodayPartialNow()
		}
		return
	}
	// No debt: the continuation chain is the only timer. It reads the spacing
	// clock from the freshness state, which every pass stamps.
	freshness := readOpenCodeUsageFreshness()
	switch {
	case openCodeReconcileSucceeded(outcome), outcome == openCodeReconcileUnsupported:
		updateOpenCodeUsageLedgerContinuation(false, 0, 0)
		if openCodeReconcileSucceeded(outcome) {
			openCodeBookRecheckWakeUp(now)
		}

	case outcome == openCodeReconcileMore:
		// Progress with candidates remaining: keep draining, free of charge —
		// up to the chain's pass budget, which is the only thing bounding a
		// backlog that keeps making progress.
		if readOpenCodeUsageLedger().ContinuationPasses >= openCodeContinuationMaxPasses {
			updateOpenCodeUsageLedgerContinuation(false, 0, 0)
			openCodeMarkPartialIfBacklogged()
			return
		}
		openCodeBookContinuation(0, 0, openCodeSpacedDelay(freshness, now, openCodeRunDebtFreeRetryDelay))
	default:
		ledger := readOpenCodeUsageLedger()
		failures := ledger.ContinuationFailures
		firstFailureAtMs := ledger.ContinuationFirstFailureAtMs
		if firstFailureAtMs == 0 || firstFailureAtMs > now.UnixMilli() {
			firstFailureAtMs = now.UnixMilli()
		}
		if outcome != openCodeReconcileOffline {
			failures++
		}
		if failures >= openCodeContinuationMaxFailures {
			updateOpenCodeUsageLedgerContinuation(false, 0, 0)
			openCodeMarkPartialIfBacklogged()
			return
		}
		openCodeBookContinuation(failures, firstFailureAtMs,
			openCodeSpacedDelay(freshness, now, refreshFreeRetryDelay(
				now.Sub(time.UnixMilli(firstFailureAtMs)),
				openCodeRunDebtFreeRetryDelay, openCodeRunDebtRetryLadder)))
	}
}

// openCodeBookContinuation persists the continuation chain's state and books
// its pass. The flag has to reach DISK before the timer is worth arming: the
// fired timer (openCodeRunDebtRetryFired) and startup recovery both re-read it
// and do nothing when it reads clear, so a refused write would leave the
// remaining sessions uncounted behind a timer that reported success — and with
// no partial warning, because the pass made progress. A refusal is retried in
// memory, exactly as a refused re-read booking is.
func openCodeBookContinuation(failures int, firstFailureAtMs int64, delay time.Duration) bool {
	return openCodeBookContinuationAttempt(failures, firstFailureAtMs, delay, 1)
}

func openCodeBookContinuationAttempt(failures int, firstFailureAtMs int64, delay time.Duration, attempt int) bool {
	if !updateOpenCodeUsageLedgerContinuation(true, failures, firstFailureAtMs) {
		logOpenCodeUsage("continuation book refused")
		openCodeArmContinuationBookRetry(failures, firstFailureAtMs, delay, attempt)
		return false
	}
	stopOpenCodeContinuationBookRetry()
	openCodeArmContinuation(delay)
	return true
}

// openCodeArmContinuationBookRetry re-attempts a refused continuation booking
// on the debt ladder's delays and for its attempt budget. Giving up marks today
// a lower bound, so a backlog this process can no longer schedule is at least
// visible on the card.
func openCodeArmContinuationBookRetry(failures int, firstFailureAtMs int64, delay time.Duration, attempt int) {
	retryIn, ok := refreshRetryDelayForAttempt(attempt, openCodeRunDebtMaxAttempts, openCodeRunDebtRetryLadder)
	if !ok || IsShutdownInProgress() {
		logOpenCodeUsage("continuation book gave up attempts=%d", attempt)
		openCodeMarkPartialIfBacklogged()
		return
	}
	r := &openCodeContinuationBookRetry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
	}
	r.gen++
	gen := r.gen
	r.failures, r.firstFailureAtMs, r.delay, r.attempt = failures, firstFailureAtMs, delay, attempt
	r.timer = time.AfterFunc(retryIn, func() { openCodeContinuationBookRetryFired(gen) })
}

func openCodeContinuationBookRetryFired(gen uint64) {
	r := &openCodeContinuationBookRetry
	r.mu.Lock()
	stale := gen != r.gen
	failures, firstFailureAtMs, delay, attempt := r.failures, r.firstFailureAtMs, r.delay, r.attempt
	if !stale {
		r.timer = nil
	}
	r.mu.Unlock()
	if stale || IsShutdownInProgress() {
		return
	}
	// A debt opened in the meantime owns the one timer, and its passes drain the
	// same backlog, so the retry stands down.
	if _, owed := openCodePendingDebt(openCodeUsageNow()); owed {
		return
	}
	openCodeBookContinuationAttempt(failures, firstFailureAtMs, delay, attempt+1)
}

// stopOpenCodeContinuationBookRetry withdraws a pending booking retry. Part of
// teardown, of any booking that reached disk, and of any later outcome.
func stopOpenCodeContinuationBookRetry() {
	r := &openCodeContinuationBookRetry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	r.gen++
}

// openCodeContinuationBookRetryPending reports whether a booking retry is
// armed. Test seam.
func openCodeContinuationBookRetryPending() bool {
	r := &openCodeContinuationBookRetry
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.timer != nil
}

// openCodeBookRecheckWakeUp books one pass for the earliest re-read a
// successful pass left behind. A re-read record is consumed only by a pass that
// runs after it comes due, and a not-yet-due record is deliberately left out of
// `remaining` — so without a timer of its own a re-read would wait for whatever
// happens to trigger the next pass. A gather that nudges before the record is
// due is exactly the case that strands it: that pass ends no_change and carries
// LastPassStartedAtMs past the turn's final directory mtime, after which no
// later gather sees a store change at all.
//
// It rides the continuation, which is the no-debt timer and is already bounded
// by the chain's pass budget; only a SUCCESSFUL pass books it, so a failing
// chain that has given up cannot be restarted by a record its failures never
// consumed.
//
// A refused booking is retried in memory (openCodeArmRecheckBookRetry): the
// pass that left the record has already carried LastPassStartedAtMs past the
// store's activity, so no later gather would nudge for it, and startup recovery
// skips it while continuationDue reads clear.
func openCodeBookRecheckWakeUp(now time.Time) bool {
	return openCodeBookRecheckWakeUpAttempt(now, 1)
}

func openCodeBookRecheckWakeUpAttempt(now time.Time, attempt int) bool {
	if IsShutdownInProgress() {
		return false
	}
	ledger := readOpenCodeUsageLedger()
	dueAtMs, ok := openCodeEarliestRecheckDueMs(ledger)
	if !ok {
		return false
	}
	if ledger.ContinuationPasses >= openCodeContinuationMaxPasses {
		updateOpenCodeUsageLedgerContinuation(false, 0, 0)
		openCodeMarkPartialIfBacklogged()
		return false
	}
	// The flag has to reach DISK before the timer is worth arming: the fired
	// timer and the startup recovery both re-read it and do nothing when it is
	// clear, so a refused write would leave the re-read stranded behind a timer
	// that reports success. Say so instead of claiming a booking.
	if !updateOpenCodeUsageLedgerContinuation(true, 0, 0) {
		logOpenCodeUsage("recheck book refused")
		openCodeArmRecheckBookRetry(attempt)
		return false
	}
	stopOpenCodeRecheckBookRetry()
	// The due time is the base delay, and the pass spacing is the floor — the
	// same rule every other booking here follows.
	openCodeArmContinuation(openCodeSpacedDelay(readOpenCodeUsageFreshness(), now,
		time.UnixMilli(dueAtMs).Sub(now)))
	logOpenCodeUsage("recheck booked")
	return true
}

// openCodeArmRecheckBookRetry re-attempts a refused re-read booking on the
// debt ladder's delays and for its attempt budget. A debt opened in the
// meantime owns the one timer, and its passes consume the record, so the retry
// then stands down.
func openCodeArmRecheckBookRetry(attempt int) {
	delay, ok := refreshRetryDelayForAttempt(attempt, openCodeRunDebtMaxAttempts, openCodeRunDebtRetryLadder)
	if !ok || IsShutdownInProgress() {
		logOpenCodeUsage("recheck book gave up attempts=%d", attempt)
		// The record is durable but nothing will ever wake for it: the pass
		// that left it carried LastPassStartedAtMs past the store activity, and
		// startup recovery skips it while continuationDue reads clear. Whatever
		// the turn wrote after that export is uncounted, so today is a lower
		// bound — best effort, on the same filesystem that just refused. A
		// shutdown is not giving up: the record waits for the next process.
		if !IsShutdownInProgress() {
			openCodeMarkTodayPartialNow()
		}
		return
	}
	r := &openCodeRecheckBookRetry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
	}
	r.gen++
	gen := r.gen
	r.timer = time.AfterFunc(delay, func() { openCodeRecheckBookRetryFired(gen, attempt) })
}

func openCodeRecheckBookRetryFired(gen uint64, attempt int) {
	r := &openCodeRecheckBookRetry
	r.mu.Lock()
	stale := gen != r.gen
	if !stale {
		r.timer = nil
	}
	r.mu.Unlock()
	if stale || IsShutdownInProgress() {
		return
	}
	openCodeFreshnessInFlight.Add(1)
	defer openCodeFreshnessInFlight.Add(-1)
	now := openCodeUsageNow()
	if _, owed := openCodePendingDebt(now); owed {
		return
	}
	openCodeBookRecheckWakeUpAttempt(now, attempt+1)
}

// stopOpenCodeRecheckBookRetry withdraws a pending booking retry. Part of
// teardown, and of any booking that reached disk.
func stopOpenCodeRecheckBookRetry() {
	r := &openCodeRecheckBookRetry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	r.gen++
}

// openCodeRecheckBookRetryPending reports whether a booking retry is armed.
// Test seam.
func openCodeRecheckBookRetryPending() bool {
	r := &openCodeRecheckBookRetry
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.timer != nil
}

// clearOpenCodeContinuationDue retires the booked continuation and leaves the
// failure count where it is.
func clearOpenCodeContinuationDue() {
	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		if !l.ContinuationDue {
			return openCodeLedgerEdit{}
		}
		l.ContinuationDue = false
		return openCodeLedgerEdit{Changed: true}
	})
}

// updateOpenCodeUsageLedgerContinuation records the continuation chain's state,
// reporting whether the new state reached DISK — the schedule is read back from
// the file by the fired timer and by startup recovery, so a caller that only
// matters once it is persisted (openCodeBookRecheckWakeUp) can check.
// Zero failures clear the first-failure stamp with them.
func updateOpenCodeUsageLedgerContinuation(due bool, failures int, firstFailureAtMs int64) bool {
	_, persisted := updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		// firstFailureAtMs is NOT coupled to `failures`: an offline tick keeps
		// the chain without counting a failure, and its age is what
		// refreshFreeRetryDelay grows the backoff from. Zeroing it whenever the
		// count was zero pinned an offline device to the floor for the whole
		// outage — a timer, two reads and a write every minute, for hours. The
		// callers that mean "forget the run of failures" pass 0 explicitly.
		//
		// A chain that has ended forgets its pass budget, so the next one
		// starts fresh.
		passes := l.ContinuationPasses
		if !due {
			passes = 0
		}
		if l.ContinuationDue == due && l.ContinuationFailures == failures &&
			l.ContinuationFirstFailureAtMs == firstFailureAtMs && l.ContinuationPasses == passes {
			return openCodeLedgerEdit{}
		}
		l.ContinuationDue, l.ContinuationFailures, l.ContinuationFirstFailureAtMs =
			due, failures, firstFailureAtMs
		l.ContinuationPasses = passes
		return openCodeLedgerEdit{Changed: true}
	})
	return persisted
}

// openCodeMarkPartialIfBacklogged marks today a lower bound when a CONTINUATION
// chain gave up, so the card carries the notice rather than a silently low
// number. A chain that ended on a pass which reached the end of the candidate
// list, with nothing remembered as over-cap and no re-read still owed, left
// nothing behind.
//
// This inference is only sound for a chain, whose own last pass is the
// evidence. A debt that was ABANDONED — aged out, or out of attempts — may have
// opened after an unrelated successful pass and never have been reconciled at
// all, so its loss is not visible in the ledger: those callers use
// openCodeMarkTodayPartialNow instead.
func openCodeMarkPartialIfBacklogged() {
	ledger := readOpenCodeUsageLedger()
	if openCodeReconcileSucceeded(ledger.LastPassOutcome) &&
		len(ledger.Skipped) == 0 && len(ledger.Rechecks) == 0 {
		return
	}
	openCodeMarkTodayPartialNow()
}

// openCodeMarkTodayPartialNow marks today a lower bound unconditionally — for
// the callers that hold the evidence themselves, where the ledger's last pass
// says nothing about what was abandoned.
func openCodeMarkTodayPartialNow() {
	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		return openCodeLedgerEdit{Changed: openCodeMarkTodayPartial(l, openCodeUsageNow())}
	})
}

/* ────────────────────────────── schedule ────────────────────────────── */

// openCodeScheduleRunDebtRetry books the next rung for the debt generation
// state names, persists it and arms the one process-wide timer. False when the
// ladder has nothing left.
func openCodeScheduleRunDebtRetry(state openCodeUsageFreshness, now time.Time, kind openCodeRetryKind) bool {
	if IsShutdownInProgress() {
		return false
	}
	id := state.debtID()
	var next time.Time
	booked := updateOpenCodeUsageFreshness(func(s *openCodeUsageFreshness) {
		if !s.owed() || s.debtID() != id {
			return
		}
		delay, ok := refreshRetryDelayForAttempt(s.Attempts, openCodeRunDebtMaxAttempts, openCodeRunDebtRetryLadder)
		if !ok {
			s.NextAttemptAtMs = 0
			return
		}
		if kind == openCodeRetryFree {
			delay = refreshFreeRetryDelay(now.Sub(time.UnixMilli(s.OwedAtMs)),
				openCodeRunDebtFreeRetryDelay, openCodeRunDebtRetryLadder)
		}
		// Same spacing rule the continuation uses, so the two cannot drift.
		next = now.Add(openCodeSpacedDelay(*s, now, delay))
		s.NextAttemptAtMs = next.UnixMilli()
	})
	if next.IsZero() {
		return false
	}
	openCodeArmRunDebtRetry(id, next.Sub(now))
	logOpenCodeUsage("scheduled attempts=%d", booked.Attempts)
	return true
}

// openCodeArmRunDebtRetry replaces the pending timer with one for generation id.
func openCodeArmRunDebtRetry(id openCodeDebtID, delay time.Duration) {
	openCodeArmPass(&id, delay)
}

// openCodeSpacedDelay is `base`, pushed out to whatever is left of
// openCodeReconcileMinInterval since the last pass. The debt ladder gets this
// from openCodeScheduleRunDebtRetry; the continuation chain has no ladder, so
// it asks here — otherwise its 30-second floor would fire only to be deferred,
// churning the timer for nothing.
func openCodeSpacedDelay(state openCodeUsageFreshness, now time.Time, base time.Duration) time.Duration {
	if state.LastAttemptAtMs <= 0 {
		return base
	}
	spaced := time.UnixMilli(state.LastAttemptAtMs).Add(openCodeReconcileMinInterval).Sub(now)
	// A stamp the clock left in the future must not park the next pass
	// arbitrarily far out: the spacing can only ever add up to one interval.
	if spaced > base && spaced <= openCodeReconcileMinInterval {
		return spaced
	}
	return base
}

// openCodeArmContinuation books the one continuation pass. It carries no debt
// id: the chain's state lives in the ledger, which the pass re-reads.
func openCodeArmContinuation(delay time.Duration) {
	openCodeArmPass(nil, delay)
}

// openCodeArmPass replaces the ONE process-wide timer. `id` names the debt
// generation the rung belongs to, or nil for a continuation, whose state lives
// in the ledger. There is one timer because there is one worker: a booked debt
// rung and a booked continuation are never both outstanding
// (openCodeBookAfterOutcome picks exactly one), so a single slot cannot lose a
// schedule the other owns.
func openCodeArmPass(id *openCodeDebtID, delay time.Duration) {
	if delay < 0 {
		delay = 0
	}
	t := &openCodeRunDebtRetryTimer
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timer != nil {
		t.timer.Stop()
	}
	t.gen++
	gen := t.gen
	t.timer = time.AfterFunc(delay, func() { openCodeRunDebtRetryFired(gen, id) })
}

// openCodeRunDebtRetryFired re-reads the state before doing anything, so a rung
// for a debt that was paid or replaced — or a continuation that was cleared —
// is a no-op.
func openCodeRunDebtRetryFired(gen uint64, id *openCodeDebtID) {
	t := &openCodeRunDebtRetryTimer
	t.mu.Lock()
	if gen != t.gen {
		t.mu.Unlock()
		return
	}
	t.timer = nil
	openCodeFreshnessInFlight.Add(1)
	t.mu.Unlock()
	defer openCodeFreshnessInFlight.Add(-1)
	if IsShutdownInProgress() {
		return
	}
	if id != nil {
		if state, owed := openCodePendingDebt(openCodeUsageNow()); !owed || state.debtID() != *id {
			return
		}
	} else if !readOpenCodeUsageLedger().ContinuationDue {
		return
	}
	startOpenCodeReconcileWorker()
}

// stopOpenCodeRunDebtRetry cancels the pending rung. Called from
// gracefulShutdown; the schedule is on disk and the next process re-arms it.
func stopOpenCodeRunDebtRetry() {
	stopOpenCodeSettleRetry()
	stopOpenCodeRecheckBookRetry()
	stopOpenCodeContinuationBookRetry()
	t := &openCodeRunDebtRetryTimer
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	t.gen++
}

// openCodeRunDebtRetryPending reports whether a rung is armed. Test seam.
func openCodeRunDebtRetryPending() bool {
	t := &openCodeRunDebtRetryTimer
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.timer != nil
}

/* ─────────────────────────── nudge & restart ────────────────────────── */

// nudgeOpenCodeUsageRefresh is the gather's trigger: a session directory whose
// newest mtime is later than the last pass's START time means a run this
// process never saw. At most once per openCodeReconcileNudgeCooldown, never
// while a run of this process is live (that run settles itself), and never
// blocking the gather.
func nudgeOpenCodeUsageRefresh(now time.Time) bool {
	if !openCodeUsageRefreshEnabled.Load() || IsShutdownInProgress() || IsOffline() {
		return false
	}
	if openCodeOldestLiveFloor() != 0 {
		return false
	}
	n := &openCodeReconcileNudge
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.lastAt.IsZero() && !now.Before(n.lastAt) && now.Sub(n.lastAt) < openCodeReconcileNudgeCooldown {
		return false
	}
	state, owed := openCodePendingDebt(now)
	if owed {
		// One debt at a time: only a booked rung that is DUE. Any other is
		// owned by its timer or by the worker already on it.
		if state.Attempts >= openCodeRunDebtMaxAttempts ||
			state.NextAttemptAtMs == 0 || state.NextAttemptAtMs > now.UnixMilli() {
			return false
		}
	} else {
		// Nothing owed: the nudge itself books the continuation, so a direct
		// run is reconciled without owing a debt. A refused write leaves the
		// nudge UNCLAIMED: the worker would reread continuationDue == false and
		// exit, and claiming the cooldown would then hold the reading back
		// until it expired. The next gather retries instead.
		if !updateOpenCodeUsageLedgerContinuation(true, 0, 0) {
			logOpenCodeUsage("nudge book refused")
			return false
		}
	}
	n.lastAt = now
	// Non-blocking, and it accounts for itself in openCodeFreshnessInFlight —
	// so the gather never waits on the pass.
	startOpenCodeReconcileWorker()
	return true
}

// payOwedOpenCodeUsageRefresh resumes what the previous process left behind: a
// booked rung is re-armed for its remainder; a debt with no rung gets one pass
// now; a floor no process settled (the self-update case) is owed one reconcile;
// a set continuationDue with NO debt open books the continuation at the free
// floor. Asynchronous — StartAgent never waits on it.
//
// Unlike payOwedGrokUsageRefresh it does NOT return early on "nothing owed": a
// backlog interrupted by a restart must resume without needing a new file
// write, and the persisted lastPassStartedAtMs would otherwise suppress the
// nudge.
func payOwedOpenCodeUsageRefresh() {
	startedAt := openCodeUsageNow()
	openCodeFreshnessInFlight.Add(1)
	go func() {
		defer openCodeFreshnessInFlight.Add(-1)
		adoptAndPayOwedOpenCodeRunDebt(startedAt)
	}()
}

func adoptAndPayOwedOpenCodeRunDebt(startedAt time.Time) {
	now := openCodeUsageNow()
	abandonedFloorMs := int64(0)
	state := updateOpenCodeUsageFreshness(func(state *openCodeUsageFreshness) {
		floorBefore := state.RunFloorMs
		openCodeRebaseFutureFreshness(state, now)
		// A floor the rebase moved was armed before a clock step back, so by
		// the previous process. Without this the rebased floor (now) would read
		// as armed after startedAt and the interrupted run would go unpaid.
		inherited := state.RunFloorMs != floorBefore || state.RunFloorMs < startedAt.UnixMilli()
		if state.RunFloorMs == 0 || !inherited || state.owed() {
			return
		}
		// A floor with no debt beside it belongs to a run the previous process
		// was cut off in — a self-update, a crash. It owes one reconcile.
		if now.Sub(time.UnixMilli(state.RunFloorMs)) > openCodeRunDebtMaxAge {
			// Too old to owe, as an aged-out debt is — but the floor is the
			// only evidence the run went unreconciled, so its day becomes a
			// lower bound before the floor is dropped.
			abandonedFloorMs = state.RunFloorMs
			state.RunFloorMs = 0
			return
		}
		openCodeOweReconcile(state, now, openCodeCompletionMs(now))
	})
	if abandonedFloorMs > 0 {
		updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
			return openCodeLedgerEdit{Changed: openCodeMarkDayPartial(l, openCodeDayKey(time.UnixMilli(abandonedFloorMs)))}
		})
	}
	if state.owed() {
		if state.NextAttemptAtMs > now.UnixMilli() {
			openCodeArmRunDebtRetry(state.debtID(), time.UnixMilli(state.NextAttemptAtMs).Sub(now))
			logOpenCodeUsage("resumed attempts=%d", state.Attempts)
			return
		}
		startOpenCodeReconcileWorker()
		return
	}
	// With no debt open, a continuation the previous process booked is the only
	// thing left to resume.
	if readOpenCodeUsageLedger().ContinuationDue {
		openCodeArmContinuation(openCodeSpacedDelay(state, now, openCodeRunDebtFreeRetryDelay))
		logOpenCodeUsage("resumed continuation")
	}
}

// drainOpenCodeUsageWrites waits, bounded, for this feature's background work —
// an arm persisting its floor, a settle writing its debt — so an update
// hand-off does not exit before the state the next process must adopt is on
// disk. Same budget rule as drainGrokUsageWrites.
func drainOpenCodeUsageWrites(ctx context.Context) {
	budget := antigravityShutdownDrainBudget(ctx, time.Now())
	if !openCodeUsageRefreshWaitFor(budget) {
		logOpenCodeUsage("shutdown drain timed out inFlight=%d", openCodeFreshnessInFlight.Load())
	}
}
