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
	"fmt"
	"os"
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
	// cancel wakes every sleeping worker when the gate is reset (tests).
	cancel chan struct{}
	wg     sync.WaitGroup
}

var codexUsageRefresh = newCodexUsageRefreshGate()

func newCodexUsageRefreshGate() *codexUsageRefreshGate {
	return &codexUsageRefreshGate{
		inFlight: map[string]chan struct{}{},
		lastRun:  map[string]time.Time{},
		workers:  map[string]bool{},
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
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		defer func() { _ = recover() }()
		fn()
	}()
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
	codexUsageRefresh.wg.Wait()
	codexUsageRefresh.mu.Lock()
	codexUsageRefresh.enabled = false
	codexUsageRefresh.inFlight = map[string]chan struct{}{}
	codexUsageRefresh.lastRun = map[string]time.Time{}
	codexUsageRefresh.workers = map[string]bool{}
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
	if snap.RunFloorMs <= 0 || codexLatestContributorObservation(snap.Contributors).UnixMilli() >= snap.RunFloorMs {
		snap.RefreshOwedAtMs, snap.RefreshOwedAttempts = 0, 0
		return
	}
	if now.Sub(time.UnixMilli(snap.RefreshOwedAtMs)) > codexRefreshOwedMaxAge {
		snap.RunFloorMs, snap.RefreshOwedAtMs, snap.RefreshOwedAttempts = 0, 0, 0
	}
}

// codexRecordRunFreshness applies mutate to the account's snapshot through the
// blocking cache transaction.
func codexRecordRunFreshness(fingerprint string, now time.Time, mutate func(snap *codexRateLimitSnapshot)) bool {
	return codexRateLimitCacheTransaction(codexRateLimitCachePath(), now, true, func(snap *codexRateLimitSnapshot) bool {
		codexScopeSnapshotToAccount(snap, fingerprint)
		mutate(snap)
		return true
	})
}

// codexArmRunFloor records a run start. An unpaid debt keeps its (older)
// floor: evidence covering the earlier run's start is what that debt waits on,
// and moving the floor later would make it stricter than the run it describes.
func codexArmRunFloor(snap *codexRateLimitSnapshot, startedAt time.Time) {
	startMs := startedAt.UnixMilli()
	if snap.RefreshOwedAtMs == 0 || snap.RunFloorMs == 0 || startMs < snap.RunFloorMs {
		snap.RunFloorMs = startMs
	}
}

// codexOweRunRefresh records that a run which started at `floor` has finished.
// Concurrent runs coalesce onto the oldest outstanding floor.
func codexOweRunRefresh(snap *codexRateLimitSnapshot, floor, completedAt time.Time) {
	floorMs := floor.UnixMilli()
	if snap.RefreshOwedAtMs == 0 || snap.RunFloorMs == 0 || floorMs < snap.RunFloorMs {
		snap.RunFloorMs = floorMs
	}
	snap.RefreshOwedAtMs = completedAt.UnixMilli()
	snap.RefreshOwedAttempts = 0
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
	codexRecordRunFreshness(fp, completedAt, func(snap *codexRateLimitSnapshot) {
		codexOweRunRefresh(snap, floor, completedAt)
	})
	if !codexUsageRefresh.claimWorker(fp) {
		return // the running worker was re-armed and will pay the newest debt
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
			codexRecordRunFreshness(fp, now, func(snap *codexRateLimitSnapshot) {
				codexOweRunRefresh(snap, state.floor, now)
			})
		}
		ctx, cancel := context.WithTimeout(context.Background(), codexForcedReconcileBudget)
		defer cancel()
		// Bypass the interval: nothing in this fresh process has reconciled yet.
		if _, ran, _, _ := codexRunGatedReconcile(ctx, base, fp, state.floor, true, now); ran {
			codexRecordRunFreshness(fp, codexUsageFreshnessNow(), codexCountRefreshAttempt)
		}
	})
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
