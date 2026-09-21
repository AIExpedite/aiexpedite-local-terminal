// cliagent_usage_codex_freshness.go — run-completion freshness for Codex
// utilization.
//
// Why this exists:
//
//	Codex utilization is only ever observed passively — a `token_count` frame
//	sniffed off a managed run's stdout (captureCodexRateLimitLine), or the same
//	telemetry mined later from Codex's rollout logs (codexReconcileFromRollout).
//	Neither is tied to a run FINISHING. A run whose stdout carried only
//	reset-only heartbeats (which the merge correctly refuses to treat as fresh),
//	or whose rollout sat behind an already-advanced scan cursor, or whose frames
//	lacked a parseable envelope timestamp, left the CLI Agents card pinned to a
//	pre-run `observedAt` through every later refresh. Claude closes the same gap
//	with triggerClaudeUsageProbeAfterRun and Antigravity with its in-run capture
//	poller; this is Codex's equivalent.
//
// Lifecycle (both the direct app-server path and terminal `codex` sessions):
//
//   - Arm: a run start persists RunFloorMs (armCodexUsageRunFloor).
//   - Settle: run completion persists RefreshOwedAtMs, then a bounded worker
//     spends at most codexRefreshAfterRunMaxAttempts FORCED reconciles on it:
//     each scans the rollout tree from just below the floor instead of the
//     persisted cursor, accepts file-anchored observation times for frames with
//     no envelope timestamp, and commits through the blocking merge.
//   - Clear: codexSettleRunFreshness, run inside EVERY cache transaction,
//     clears the debt the moment any contributor observation lands at/after the
//     floor, or once it is older than codexRefreshOwedMaxAge.
//   - Survive: the debt lives in codex_rate_limits.json, so StartAgent's
//     payOwedCodexUsageRefresh pays one bounded reconcile for a run that ended —
//     or was cut off by — the previous process (crash, restart, self-update).
//   - Report: a debt that outlived every attempt surfaces as a timestamps-only
//     warning notice (codexStaleRunNotice) instead of a card that looks fresh.
//
// Every path here is silent and bounded: it runs off session teardown and must
// never delay, block or break a session.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// codexRefreshAfterRunMaxAttempts bounds the forced reconciles one run's
	// debt may spend (immediate, then one retry after
	// codexRefreshAfterRunRetryDelay). Mirrors claudeUsageProbeAfterRunMaxAttempts.
	codexRefreshAfterRunMaxAttempts = 2
	// codexForcedReconcileBudget is the whole budget of one background forced
	// reconcile (discovery + ordering + reads + the blocking merge).
	codexForcedReconcileBudget = 8 * time.Second
	// codexRefreshOwedMaxAge retires a debt nothing could pay, so it can never
	// pin work (or a stale notice) forever.
	codexRefreshOwedMaxAge = 30 * time.Minute
	// codexRunFloorGrace widens the forced scan below the floor to absorb
	// coarse filesystem mtimes and a rollout created just before the run start
	// was recorded.
	codexRunFloorGrace = 5 * time.Second
)

// Vars rather than consts so tests can pin them small.
var (
	codexRefreshAfterRunRetryDelay = 5 * time.Second
	// codexForcedReconcileMinInterval spaces forced reconciles per account. A
	// user-initiated refresh bypasses it; nothing bypasses the single flight.
	codexForcedReconcileMinInterval = 20 * time.Second
	codexUsageFreshnessNow          = time.Now
)

// codexRunHooks lets the manager tests observe the two lifecycle calls without
// running a reconcile. Atomic because a previous test's exit watcher can still
// be reaching codexUsageRunSettled while the next test installs its recorder.
type codexRunHooks struct {
	started func(time.Time)
	settled func(time.Time)
}

var codexRunHookOverride atomic.Pointer[codexRunHooks]

// codexUsageRunStarted is what the session managers call when a Codex run
// starts; codexUsageRunSettled when it finishes.
func codexUsageRunStarted(startedAt time.Time) {
	if hooks := codexRunHookOverride.Load(); hooks != nil {
		hooks.started(startedAt)
		return
	}
	armCodexUsageRunFloor(startedAt)
}

func codexUsageRunSettled(floor time.Time) {
	if hooks := codexRunHookOverride.Load(); hooks != nil {
		hooks.settled(floor)
		return
	}
	triggerCodexUsageRefreshAfterRun(floor)
}

// codexUsageRefreshGate holds the process-local bounds. Keyed by account
// fingerprint so a credentials swap never inherits another account's throttle.
type codexUsageRefreshGate struct {
	mu sync.Mutex
	// enabled is set only by StartAgent (SetCodexUsageRefreshEnabled): tests
	// that drive a codex session must not scan the developer's real ~/.codex in
	// the background.
	enabled  bool
	inFlight map[string]chan struct{}
	lastRun  map[string]time.Time
	// workers tracks the one post-run worker per account; the value is its
	// "re-arm" flag, set when another run finished while it was working so that
	// run gets its own attempts instead of being folded into a nearly spent loop.
	workers map[string]bool
	// pending holds a settle whose cache write lost the bounded race for the
	// cache lock. Bounded locking deliberately lets that write be REFUSED, and
	// an unrecorded debt is a run whose refresh — and whose restart marker — is
	// silently lost, so it is retried by the account's worker instead.
	pending map[string]codexPendingRunDebt
	// cancel wakes every sleeping worker when the gate is reset (tests).
	cancel chan struct{}
	// active counts the tracked goroutines spawn started; idle broadcasts when
	// it reaches zero. A sync.WaitGroup cannot serve here: a run settling at
	// process/session teardown legitimately spawns while a waiter is already
	// blocked, and Add racing a Wait that has just seen zero is exactly the
	// reuse the race detector reports. Both sides live under mu instead.
	active int
	idle   *sync.Cond
}

// codexPendingRunDebt is one finished run whose debt is not on disk yet.
type codexPendingRunDebt struct {
	floor       time.Time
	completedAt time.Time
}

var codexUsageRefresh = newCodexUsageRefreshGate()

func newCodexUsageRefreshGate() *codexUsageRefreshGate {
	return &codexUsageRefreshGate{
		inFlight: map[string]chan struct{}{},
		lastRun:  map[string]time.Time{},
		workers:  map[string]bool{},
		pending:  map[string]codexPendingRunDebt{},
		cancel:   make(chan struct{}),
	}
}

// SetCodexUsageRefreshEnabled arms (or disarms) the post-run freshness path for
// this process. Called from StartAgent.
func SetCodexUsageRefreshEnabled(enabled bool) {
	codexUsageRefresh.mu.Lock()
	codexUsageRefresh.enabled = enabled
	codexUsageRefresh.mu.Unlock()
}

func (g *codexUsageRefreshGate) isEnabled() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.enabled
}

// spawn runs fn on a tracked goroutine that can never take the process down.
func (g *codexUsageRefreshGate) spawn(fn func()) {
	g.mu.Lock()
	g.active++
	g.mu.Unlock()
	go func() {
		defer g.spawnDone()
		defer func() { _ = recover() }()
		fn()
	}()
}

func (g *codexUsageRefreshGate) spawnDone() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	if g.active <= 0 {
		g.idleCond().Broadcast()
	}
}

// waitIdle blocks until no tracked goroutine is running. Callers that need a
// deadline run it on a goroutine of their own.
func (g *codexUsageRefreshGate) waitIdle() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for g.active > 0 {
		g.idleCond().Wait()
	}
}

// idleCond must be called with mu held: the cond it returns is bound to mu.
func (g *codexUsageRefreshGate) idleCond() *sync.Cond {
	if g.idle == nil {
		g.idle = sync.NewCond(&g.mu)
	}
	return g.idle
}

// begin claims the single flight for fp. It returns the flight's done channel
// when the caller may run; otherwise busy (a flight to wait on) or wait (how
// long until the interval admits a non-bypassing caller).
func (g *codexUsageRefreshGate) begin(fp string, now time.Time, bypassInterval bool) (done chan struct{}, busy <-chan struct{}, wait time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if ch := g.inFlight[fp]; ch != nil {
		return nil, ch, 0
	}
	if last, ok := g.lastRun[fp]; ok && !bypassInterval {
		if since := now.Sub(last); since >= 0 && since < codexForcedReconcileMinInterval {
			return nil, nil, codexForcedReconcileMinInterval - since
		}
	}
	done = make(chan struct{})
	g.inFlight[fp] = done
	g.lastRun[fp] = now
	return done, nil, 0
}

func (g *codexUsageRefreshGate) finish(fp string, done chan struct{}) {
	g.mu.Lock()
	if g.inFlight[fp] == done {
		delete(g.inFlight, fp)
	}
	g.mu.Unlock()
	close(done)
}

// claimWorker makes the caller fp's post-run worker. When one is already
// running it is re-armed instead, and the caller returns: the running worker
// re-reads the (now newer) debt from disk on its next attempt.
func (g *codexUsageRefreshGate) claimWorker(fp string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, running := g.workers[fp]; running {
		g.workers[fp] = true
		return false
	}
	g.workers[fp] = false
	return true
}

// takeRearm reports and clears a pending re-arm for fp's worker.
func (g *codexUsageRefreshGate) takeRearm(fp string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	rearm := g.workers[fp]
	g.workers[fp] = false
	return rearm
}

// releaseWorker retires fp's worker unless a run finished since its last
// check, in which case it reports false and the worker keeps going.
func (g *codexUsageRefreshGate) releaseWorker(fp string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.workers[fp] {
		g.workers[fp] = false
		return false
	}
	delete(g.workers, fp)
	return true
}

// rememberDebt retains a settle whose cache write was refused. Concurrent
// unrecorded runs coalesce the same way the snapshot does: the NEWEST floor,
// so one observation has to cover every run the retained debt represents.
func (g *codexUsageRefreshGate) rememberDebt(fp string, debt codexPendingRunDebt) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if cur, ok := g.pending[fp]; ok {
		if cur.floor.After(debt.floor) {
			debt.floor = cur.floor
		}
		if cur.completedAt.After(debt.completedAt) {
			debt.completedAt = cur.completedAt
		}
	}
	g.pending[fp] = debt
}

// takeDebt removes and returns fp's retained debt, if any.
func (g *codexUsageRefreshGate) takeDebt(fp string) (codexPendingRunDebt, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	debt, ok := g.pending[fp]
	delete(g.pending, fp)
	return debt, ok
}

func (g *codexUsageRefreshGate) cancelCh() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cancel
}

// sleep waits d, returning false when the process is shutting down or the
// gate was reset — the environment the caller would wake into is gone.
func (g *codexUsageRefreshGate) sleep(d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-shutdownChan:
		return false
	case <-g.cancelCh():
		return false
	}
}

// resetCodexUsageRefreshGate cancels sleeping workers, waits for every tracked
// goroutine and clears the gate. Test-only seam, mirroring
// resetClaudeUsageProbeGate: a worker that outlived its test would otherwise
// write into the next test's cache.
func resetCodexUsageRefreshGate() {
	codexUsageRefresh.mu.Lock()
	close(codexUsageRefresh.cancel)
	codexUsageRefresh.mu.Unlock()
	codexUsageRefresh.waitIdle()
	codexUsageRefresh.mu.Lock()
	codexUsageRefresh.enabled = false
	codexUsageRefresh.inFlight = map[string]chan struct{}{}
	codexUsageRefresh.lastRun = map[string]time.Time{}
	codexUsageRefresh.workers = map[string]bool{}
	codexUsageRefresh.pending = map[string]codexPendingRunDebt{}
	codexUsageRefresh.cancel = make(chan struct{})
	codexUsageRefresh.mu.Unlock()
}

/* ─────────────────────────── context markers ─────────────────────────── */

// codexUsageForceRefreshKey / codexForcedReconcileKey are unexported struct{}
// types, so nothing outside this package can forge them onto a context.
type codexUsageForceRefreshKey struct{}
type codexForcedReconcileKey struct{}

// WithCodexUsageForceRefresh marks ctx as a user- or service-initiated
// __cli_usage_refresh__: the Codex parser then runs a forced reconcile that
// bypasses the per-account interval (never the single flight). Set by the
// refresh handler next to WithClaudeUsageForceProbe.
func WithCodexUsageForceRefresh(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, codexUsageForceRefreshKey{}, true)
}

func codexUsageForceRefresh(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	forced, _ := ctx.Value(codexUsageForceRefreshKey{}).(bool)
	return forced
}

// withCodexForcedReconcile marks a rollout reconcile as forced, carrying the
// run floor it must cover (zero when no run is owed a refresh).
func withCodexForcedReconcile(ctx context.Context, floor time.Time) context.Context {
	return context.WithValue(ctx, codexForcedReconcileKey{}, floor)
}

func codexForcedReconcileFrom(ctx context.Context) (time.Time, bool) {
	if ctx == nil {
		return time.Time{}, false
	}
	floor, forced := ctx.Value(codexForcedReconcileKey{}).(time.Time)
	return floor, forced
}

/* ─────────────────────────── persisted state ─────────────────────────── */

// codexRunFreshnessState is the account's run-freshness bookkeeping as read
// from the cache, evaluated at `now`.
type codexRunFreshnessState struct {
	floor    time.Time
	owedAt   time.Time
	attempts int
	latest   time.Time
	// owed: a finished run's utilization is still unobserved and the debt has
	// not aged out.
	owed bool
	// interrupted: a run was armed but never settled and is still unobserved.
	// Only meaningful at process start, where no run of this process can be in
	// flight — the previous process died (or was replaced) mid-run.
	interrupted bool
}

func codexRunFreshnessFromView(view codexCacheView, now time.Time) codexRunFreshnessState {
	state := codexRunFreshnessState{attempts: view.refreshOwedAttempts}
	if view.runFloorMs <= 0 {
		return state
	}
	state.floor = time.UnixMilli(view.runFloorMs)
	state.latest = codexLatestContributorObservation(view.contributors)
	unobserved := state.latest.Before(state.floor)
	if view.refreshOwedAtMs > 0 {
		state.owedAt = time.UnixMilli(view.refreshOwedAtMs)
		state.owed = unobserved && now.Sub(state.owedAt) <= codexRefreshOwedMaxAge
		return state
	}
	state.interrupted = unobserved && now.Sub(state.floor) <= codexRefreshOwedMaxAge
	return state
}

func codexRunFreshnessForAccount(fingerprint string, now time.Time) codexRunFreshnessState {
	return codexRunFreshnessFromView(codexCacheViewForAccount(fingerprint), now)
}

// codexSettleRunFreshness clears a debt that is paid (a contributor observation
// at/after the floor) or too old to pay. Runs inside every cache transaction,
// so whichever writer lands the covering observation — live capture, a routine
// gather, the post-run worker — settles it in the same atomic write.
func codexSettleRunFreshness(snap *codexRateLimitSnapshot, now time.Time) {
	if snap.RefreshOwedAtMs <= 0 {
		return
	}
	paid := snap.RunFloorMs <= 0 || codexLatestContributorObservation(snap.Contributors).UnixMilli() >= snap.RunFloorMs
	expired := now.Sub(time.UnixMilli(snap.RefreshOwedAtMs)) > codexRefreshOwedMaxAge
	if !paid && !expired {
		return
	}
	snap.RefreshOwedAtMs, snap.RefreshOwedAttempts = 0, 0
	if !paid {
		snap.RunFloorMs = 0
	}
	codexPromoteActiveRunFloor(snap)
}

// codexPromoteActiveRunFloor moves a floor parked by codexArmRunFloor into
// RunFloorMs once the older debt that forced it aside is gone. Without it,
// evidence taken before a still-running run started would settle the previous
// run's debt AND drop that run's floor, so a crash before it settles would
// leave startup unable to classify it as interrupted.
func codexPromoteActiveRunFloor(snap *codexRateLimitSnapshot) {
	if snap.ActiveRunFloorMs > snap.RunFloorMs {
		snap.RunFloorMs = snap.ActiveRunFloorMs
	}
	snap.ActiveRunFloorMs = 0
}

// codexRecordRunFreshness applies mutate to the account's snapshot through the
// blocking cache transaction.
func codexRecordRunFreshness(fingerprint string, now time.Time, mutate func(snap *codexRateLimitSnapshot)) bool {
	return codexRateLimitCacheTransaction(context.Background(), codexRateLimitCachePath(), now, true, func(snap *codexRateLimitSnapshot) bool {
		codexScopeSnapshotToAccount(snap, fingerprint)
		mutate(snap)
		return true
	})
}

// codexArmRunFloor records a run start. An unpaid debt keeps its (older)
// floor: evidence covering the earlier run's start is what that debt waits on,
// and moving the floor later would make it stricter than the run it describes.
// The newer start is parked in ActiveRunFloorMs instead, so settling the older
// debt promotes it rather than forgetting this run ever began.
//
// A start NEVER moves a floor backwards. Every arm is written by its own
// spawned goroutine, so two runs starting at A then B can reach this mutation
// in B-then-A order; letting the late A write lower the floor would let
// evidence that predates B be read as covering it, and a crash before B
// settled would then not classify B as interrupted. An older start is already
// covered by the newer floor, so dropping it loses nothing.
func codexArmRunFloor(snap *codexRateLimitSnapshot, startedAt time.Time) {
	startMs := startedAt.UnixMilli()
	if snap.RefreshOwedAtMs == 0 {
		// Nothing owed, so nothing can still be parked behind a debt: fold any
		// leftover parked floor in before coalescing this start onto the newest.
		codexPromoteActiveRunFloor(snap)
		if startMs > snap.RunFloorMs {
			snap.RunFloorMs = startMs
		}
		return
	}
	if startMs > snap.RunFloorMs && startMs > snap.ActiveRunFloorMs {
		snap.ActiveRunFloorMs = startMs
	}
}

// codexOweRunRefresh records that a run which started at `floor` has finished.
// Concurrent runs coalesce onto the NEWEST floor: one debt stands for every run
// it absorbed, so it must only be cleared by an observation that covers the
// latest of them. Keeping the oldest would let evidence taken while the first
// run was still going satisfy a second run that had not even started.
func codexOweRunRefresh(snap *codexRateLimitSnapshot, floor, completedAt time.Time) {
	floorMs := floor.UnixMilli()
	if snap.RunFloorMs == 0 || floorMs > snap.RunFloorMs {
		snap.RunFloorMs = floorMs
	}
	snap.RefreshOwedAtMs = completedAt.UnixMilli()
	snap.RefreshOwedAttempts = 0
	if snap.ActiveRunFloorMs <= snap.RunFloorMs {
		snap.ActiveRunFloorMs = 0
	}
}

func codexCountRefreshAttempt(snap *codexRateLimitSnapshot) {
	if snap.RefreshOwedAtMs > 0 {
		snap.RefreshOwedAttempts++
	}
}

/* ─────────────────────────── lifecycle hooks ─────────────────────────── */

// armCodexUsageRunFloor records that a Codex run started at startedAt. The
// write takes the cache's blocking cross-process lock, so it runs off the
// caller's goroutine — both managers call this while holding their own mutex.
func armCodexUsageRunFloor(startedAt time.Time) {
	if !codexUsageRefresh.isEnabled() {
		return
	}
	codexUsageRefresh.spawn(func() {
		fp := codexAccountFingerprintAtBase(codexHomeBase())
		codexRecordRunFreshness(fp, codexUsageFreshnessNow(), func(snap *codexRateLimitSnapshot) {
			codexArmRunFloor(snap, startedAt)
		})
	})
}

// triggerCodexUsageRefreshAfterRun settles a finished Codex run that started
// at `floor`: it records the debt, then spends at most
// codexRefreshAfterRunMaxAttempts forced reconciles paying it. Asynchronous and
// panic-proof, so it can never delay frame ordering, waitForExit or the ended
// publication.
func triggerCodexUsageRefreshAfterRun(floor time.Time) {
	if !codexUsageRefresh.isEnabled() || floor.IsZero() {
		return
	}
	completedAt := codexUsageFreshnessNow()
	codexUsageRefresh.spawn(func() {
		codexRefreshAfterRun(floor, completedAt)
	})
}

func codexRefreshAfterRun(floor, completedAt time.Time) {
	base := codexHomeBase()
	fp := codexAccountFingerprintAtBase(base)
	// Record the debt FIRST: every later step may be refused or fail, and a
	// debt that was never recorded is a run whose refresh is silently lost —
	// including across a restart, which is what the persisted marker is for.
	// When live capture already observed the run, the settle inside this very
	// transaction clears it and the loop below exits without scanning.
	if !codexRecordRunFreshness(fp, completedAt, func(snap *codexRateLimitSnapshot) {
		codexOweRunRefresh(snap, floor, completedAt)
	}) {
		// The bounded cache locks refused the write. Retain the debt rather
		// than walking into a worker that would read `owed == false` off disk
		// and retire a run that was never actually recorded.
		codexUsageRefresh.rememberDebt(fp, codexPendingRunDebt{floor: floor, completedAt: completedAt})
	}
	codexRunDebtWorker(base, fp)
}

// codexRunDebtWorker makes the caller fp's post-run worker and pays whatever
// debt is outstanding — on disk or retained in the gate — until no run has
// finished since its last check. Returns at once when a worker is already
// running: that worker was re-armed and re-reads the newest debt itself.
func codexRunDebtWorker(base, fp string) {
	if !codexUsageRefresh.claimWorker(fp) {
		return
	}
	for {
		codexPayRunRefresh(base, fp)
		if codexUsageRefresh.releaseWorker(fp) {
			return
		}
	}
}

// codexPayRunRefresh spends up to codexRefreshAfterRunMaxAttempts forced
// reconciles on fp's outstanding debt, restarting the count when another run
// finishes meanwhile.
func codexPayRunRefresh(base, fp string) {
	for attempt := 0; attempt < codexRefreshAfterRunMaxAttempts; attempt++ {
		if attempt > 0 && !codexUsageRefresh.sleep(codexRefreshAfterRunRetryDelay) {
			return
		}
		if codexUsageRefresh.takeRearm(fp) {
			attempt = 0
		}
		if !codexFlushPendingRunDebt(fp) {
			continue // still unrecorded; retry the write on the next attempt
		}
		state := codexRunFreshnessForAccount(fp, codexUsageFreshnessNow())
		if !state.owed {
			return // paid by capture, a gather, or an earlier attempt
		}
		if !codexAwaitGatedReconcile(base, fp, state.floor) {
			return
		}
		codexRecordRunFreshness(fp, codexUsageFreshnessNow(), codexCountRefreshAttempt)
	}
}

// codexFlushPendingRunDebt retries a settle whose write was refused by the
// bounded cache locks. Reports whether fp's debt is on disk: false means it is
// still retained and the caller must not treat the account as debt-free.
func codexFlushPendingRunDebt(fp string) bool {
	debt, ok := codexUsageRefresh.takeDebt(fp)
	if !ok {
		return true
	}
	if codexRecordRunFreshness(fp, debt.completedAt, func(snap *codexRateLimitSnapshot) {
		codexOweRunRefresh(snap, debt.floor, debt.completedAt)
	}) {
		return true
	}
	codexUsageRefresh.rememberDebt(fp, debt)
	return false
}

// codexAwaitGatedReconcile runs one background forced reconcile, waiting (at
// most once each) for an in-flight reconcile to finish or for the interval to
// admit it. Reports false only when the process is shutting down or the gate
// was reset.
func codexAwaitGatedReconcile(base, fp string, floor time.Time) bool {
	for try := 0; try < 3; try++ {
		ctx, cancel := context.WithTimeout(context.Background(), codexForcedReconcileBudget)
		_, ran, busy, wait := codexRunGatedReconcile(ctx, base, fp, floor, false, codexUsageFreshnessNow())
		cancel()
		switch {
		case ran:
			return true
		case busy != nil:
			select {
			case <-busy:
			case <-shutdownChan:
				return false
			case <-codexUsageRefresh.cancelCh():
				return false
			}
		default:
			if !codexUsageRefresh.sleep(wait) {
				return false
			}
		}
	}
	return true // counted as an attempt; the debt stays for the next one
}

// codexReconcileResult is what one rollout reconcile hands the parser.
type codexReconcileResult struct {
	metrics           []cliAgentUsageMetric
	limit             codexUsageLimitEvidence
	latestObservation time.Time
}

// codexRunGatedReconcile runs one forced rollout reconcile for fp under the
// single flight and the per-account interval. ran=false means it did not run:
// busy is the in-flight reconcile to wait on, or wait is how long the interval
// still holds a non-bypassing caller back.
func codexRunGatedReconcile(ctx context.Context, base, fp string, floor time.Time, bypassInterval bool, now time.Time) (res codexReconcileResult, ran bool, busy <-chan struct{}, wait time.Duration) {
	done, busy, wait := codexUsageRefresh.begin(fp, now, bypassInterval)
	if done == nil {
		return codexReconcileResult{}, false, busy, wait
	}
	defer codexUsageRefresh.finish(fp, done)
	metrics, limit, latest := codexReconcileFromRollout(withCodexForcedReconcile(ctx, floor), base, fp, now)
	return codexReconcileResult{metrics: metrics, limit: limit, latestObservation: latest}, true, nil, 0
}

// payOwedCodexUsageRefresh pays, once, a debt the previous agent process left
// behind: a run that finished just before a restart or self-update (owed), or
// one the process died in the middle of (interrupted — nothing of this process
// can be running yet at startup). Exactly one bounded forced reconcile; any
// remainder is paid by the next run or refresh under the ordinary bounds.
func payOwedCodexUsageRefresh() {
	if !codexUsageRefresh.isEnabled() {
		return
	}
	codexUsageRefresh.spawn(func() {
		base := codexHomeBase()
		fp := codexAccountFingerprintAtBase(base)
		now := codexUsageFreshnessNow()
		state := codexRunFreshnessForAccount(fp, now)
		if !state.owed && !state.interrupted {
			return
		}
		if state.interrupted {
			// The run is over — its process is gone — so it is owed from now on:
			// the stale notice and the age-out both need a completion time.
			codexOweInterruptedRun(fp, state.floor, now)
		}
		ctx, cancel := context.WithTimeout(context.Background(), codexForcedReconcileBudget)
		defer cancel()
		// Bypass the interval: nothing in this fresh process has reconciled yet.
		if _, ran, _, _ := codexRunGatedReconcile(ctx, base, fp, state.floor, true, now); ran {
			codexRecordRunFreshness(fp, codexUsageFreshnessNow(), codexCountRefreshAttempt)
		}
		// The reconcile may have PAID the debt it was sent to pay and, in the
		// same transaction, promoted a floor that was parked behind it — a
		// second, newer run this dead process never settled. That run is over
		// too, so turn the promoted floor into debt of its own: routine gathers
		// force only on `owed`, so an interrupted floor left un-owed would
		// never be refreshed and would simply age out.
		now = codexUsageFreshnessNow()
		if promoted := codexRunFreshnessForAccount(fp, now); promoted.interrupted {
			codexOweInterruptedRun(fp, promoted.floor, now)
		}
		// Either conversion may have been REFUSED by the bounded cache locks.
		// A run left merely `interrupted` is never refreshed — routine gathers
		// force only on `owed` — and never warns, so the retained debt is
		// handed to the account's worker, the same retry a settle whose write
		// was refused gets (codexFlushPendingRunDebt). Nothing to do when the
		// flush lands here, or when nothing was retained.
		if !codexFlushPendingRunDebt(fp) {
			codexRunDebtWorker(base, fp)
		}
	})
}

// codexOweInterruptedRun converts a run the previous process died in the
// middle of into a debt completed at `now`. When the bounded cache locks
// refuse the write, the debt is RETAINED in the gate rather than dropped, so
// payOwedCodexUsageRefresh can retry it through the worker.
func codexOweInterruptedRun(fp string, floor, now time.Time) bool {
	if codexRecordRunFreshness(fp, now, func(snap *codexRateLimitSnapshot) {
		codexOweRunRefresh(snap, floor, now)
	}) {
		return true
	}
	codexUsageRefresh.rememberDebt(fp, codexPendingRunDebt{floor: floor, completedAt: now})
	return false
}

/* ───────────────────────────── gather side ───────────────────────────── */

// codexReconcileForGather is the parser's rollout reconcile. A forced refresh,
// or an outstanding run debt, takes the gated forced path first; when the gate
// refuses (another reconcile in flight, or inside the interval) it falls back to
// the routine cursor-based reconcile — after waiting out an in-flight one for a
// forced refresh, so the reading it returns includes that reconcile's result.
func codexReconcileForGather(ctx context.Context, base, fp string, now time.Time, forced bool) codexReconcileResult {
	state := codexRunFreshnessForAccount(fp, now)
	if forced || state.owed {
		var floor time.Time
		if state.owed {
			floor = state.floor
		}
		res, ran, busy, _ := codexRunGatedReconcile(ctx, base, fp, floor, forced, now)
		if ran {
			return res
		}
		if forced && busy != nil {
			select {
			case <-busy:
			case <-ctx.Done():
			}
		}
	}
	metrics, limit, latest := codexReconcileFromRollout(ctx, base, fp, now)
	return codexReconcileResult{metrics: metrics, limit: limit, latestObservation: latest}
}

// codexStaleRunNotice explains a card whose newest observation predates the
// most recent Codex run, once the bounded post-run attempts have all come up
// empty. Timestamps only — never a path, an account or log text.
func codexStaleRunNotice(state codexRunFreshnessState) string {
	if !state.owed || state.attempts < codexRefreshAfterRunMaxAttempts {
		return ""
	}
	const layout = "2006-01-02 15:04 UTC"
	last := "No Codex utilization reading has been observed"
	if !state.latest.IsZero() {
		last = "Codex utilization was last observed " + state.latest.UTC().Format(layout)
	}
	return fmt.Sprintf("%s, before the most recent Codex run started (%s); it will update once that run's telemetry is found.",
		last, state.floor.UTC().Format(layout))
}

// codexRunCompletionFrame reports whether a Codex stdout line announces the end
// of a TURN (`turn.completed`, or the `thread.completed` that follows it). It
// exists so the long-lived app-server — which serves many turns per process and
// must stay free of JSON-RPC semantics — can settle each finished turn instead
// of only the process exit. The envelope shapes mirror
// isRecognizedCodexRateLimitEnvelope: a bare event, one nested under
// `msg`/`payload`/`params`, or a JSON-RPC notification whose method carries the
// event name. detectCLITerminalEvent covers the same two names for terminal
// `codex` sessions, which speak the bare-event dialect only.
func codexRunCompletionFrame(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") || !strings.Contains(trimmed, "completed") {
		return false
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return false
	}
	if method, _ := raw["method"].(string); codexCompletionEventName(method) {
		return true
	}
	if eventType, _ := raw["type"].(string); codexCompletionEventName(eventType) {
		return true
	}
	for _, key := range []string{"msg", "payload", "params", "result"} {
		nested, ok := raw[key].(map[string]interface{})
		if !ok {
			continue
		}
		if eventType, _ := nested["type"].(string); codexCompletionEventName(eventType) {
			return true
		}
		if msg, ok := nested["msg"].(map[string]interface{}); ok {
			if eventType, _ := msg["type"].(string); codexCompletionEventName(eventType) {
				return true
			}
		}
	}
	return false
}

// codexRunStartFrame reports whether a line the CLIENT wrote to a long-lived
// app-server REQUESTS a turn. It is the mirror of codexRunCompletionFrame and
// exists for the same reason: the transport file must stay free of JSON-RPC
// semantics, and the floor a turn is measured against has to be the moment that
// turn was asked for. Anchoring it at the previous turn's completion instead
// would let a rate-limit frame delivered between turns (an
// `account/rateLimits/read` reply, or a notification during initialization)
// count as the new turn's reading.
func codexRunStartFrame(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return false
	}
	method, _ := raw["method"].(string)
	switch codexNormalizeCompletionName(method) {
	// Codex 0.144's generated schema spells it `turn/start`; older app-server
	// builds send the turn as `sendUserTurn` / `sendUserMessage`.
	case "turn.start", "sendUserTurn", "sendUserMessage":
		return true
	}
	return false
}

// codexRunProgressFrame reports whether a Codex stdout line DEMONSTRATES a turn
// in progress: a `turn.*`/`item.*` event or request (any spelling
// codexNormalizeCompletionName accepts, including server-initiated approval
// requests such as `item/commandExecution/requestApproval`), or one of the
// legacy `codex/event/<name>` task events. It is the only frame that may open a
// utilization run the transport did not see requested — the fallback for a
// client dialect codexRunStartFrame does not recognize.
//
// Everything else is deliberately excluded: responses (`id` + `result`, no
// method) such as the between-turn `account/rateLimits/read` reply,
// `account/rateLimits/*` notifications, `initialize`/`thread/*` traffic and
// the initialization heartbeat. Opening a run on those anchors a floor LATER
// than the reading captured a moment earlier from the very same frame, and a
// client that then closes the app-server without another turn settles that
// phantom run into a debt no rollout can pay — a false stale-utilization
// notice.
func codexRunProgressFrame(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return false
	}
	if isRecognizedCodexRateLimitEnvelope(raw) {
		return false
	}
	for _, name := range codexFrameEventNames(raw) {
		if codexCompletionEventName(name) {
			return false
		}
		if codexTurnScopedEventName(name) {
			return true
		}
	}
	return false
}

// codexFrameEventNames collects the method or event names a frame carries in
// the shapes codexRunCompletionFrame reads: the JSON-RPC method, a bare event
// `type`, and one nested under `msg`/`payload`/`params`/`result`.
func codexFrameEventNames(raw map[string]interface{}) []string {
	var names []string
	add := func(v interface{}) {
		if name, _ := v.(string); name != "" {
			names = append(names, name)
		}
	}
	add(raw["method"])
	add(raw["type"])
	for _, key := range []string{"msg", "payload", "params", "result"} {
		nested, ok := raw[key].(map[string]interface{})
		if !ok {
			continue
		}
		add(nested["type"])
		if msg, ok := nested["msg"].(map[string]interface{}); ok {
			add(msg["type"])
		}
	}
	return names
}

// codexTurnScopedEventName reports whether an event or method name belongs to
// a turn: the `turn`/`item` subjects of the current app-server schema, or a
// legacy task event name.
func codexTurnScopedEventName(name string) bool {
	// The subject is the FIRST segment once the `codex/event/` prefix is gone:
	// `item/agentMessage/delta` and `item/commandExecution/requestApproval`
	// nest deeper than the two-part names codexNormalizeCompletionName reduces.
	trimmed := strings.TrimPrefix(name, "codex/event/")
	subject := trimmed
	if idx := strings.IndexAny(trimmed, "/."); idx >= 0 {
		subject = trimmed[:idx]
	}
	switch subject {
	case "turn", "item":
		return true
	}
	// Legacy `codex/event/<name>` builds nest the bare name under params.msg.
	bare := trimmed
	if idx := strings.LastIndexAny(bare, "/."); idx >= 0 {
		bare = bare[idx+1:]
	}
	switch bare {
	case "task_started", "task_complete", "agent_message", "agent_message_delta",
		"agent_reasoning", "agent_reasoning_delta", "exec_command_begin", "exec_command_end",
		"patch_apply_begin", "patch_apply_end", "mcp_tool_call_begin", "mcp_tool_call_end",
		"exec_approval_request", "apply_patch_approval_request":
		return true
	}
	return false
}

// codexCompletionEventName matches a turn-completion event name, tolerating the
// `codex/event/<name>` prefix app-server notifications may carry.
func codexCompletionEventName(name string) bool {
	switch codexNormalizeCompletionName(name) {
	case "turn.completed", "thread.completed":
		return true
	}
	return false
}

// codexNormalizeCompletionName reduces a method or event name to its bare
// `<subject>.<verb>` form. App-server builds spell the same notification three
// ways — `turn.completed`, `codex/event/turn.completed`, and (Codex 0.144's
// generated JSON-RPC schema) the all-slash `turn/completed` — so the last
// separator is only stripped when what follows already carries the subject.
func codexNormalizeCompletionName(name string) string {
	idx := strings.LastIndex(name, "/")
	if idx < 0 {
		return name
	}
	head, tail := name[:idx], name[idx+1:]
	if strings.Contains(tail, ".") {
		return tail
	}
	if hidx := strings.LastIndex(head, "/"); hidx >= 0 {
		head = head[hidx+1:]
	}
	return head + "." + tail
}

/* ───────────────────────────── rollout side ──────────────────────────── */

// codexRolloutInferredObservation is the observation time a FORCED reconcile
// may assign to rollout telemetry that carries no parseable envelope
// timestamp: the rollout file's mtime clamped to [run floor, now]. Zero — keep
// dropping such frames — outside a forced reconcile, with no floor, or when the
// file was last written before the run floor (its frames cannot describe it).
func codexRolloutInferredObservation(ctx context.Context, f *os.File, now time.Time) time.Time {
	floor, forced := codexForcedReconcileFrom(ctx)
	if !forced || floor.IsZero() {
		return time.Time{}
	}
	info, err := f.Stat()
	if err != nil {
		return time.Time{}
	}
	mtime := info.ModTime()
	if mtime.Before(floor) {
		return time.Time{}
	}
	if mtime.After(now) {
		return now
	}
	return mtime
}

func codexMarkContributorsInferred(contributors map[string]map[string]codexRateLimitBucket) {
	for window, limits := range contributors {
		for limitID, bucket := range limits {
			bucket.Inferred = true
			contributors[window][limitID] = bucket
		}
	}
}
