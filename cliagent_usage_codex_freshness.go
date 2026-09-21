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
	// codexArmedAccountCap bounds the armed-run → account map. A floor that is
	// never settled individually (an app-server exit settles only the newest
	// open turn) would otherwise retain its entry for the life of the process.
	codexArmedAccountCap = 64
	// codexRunFloorGrace widens the forced scan below the floor to absorb
	// coarse filesystem mtimes and a rollout created just before the run start
	// was recorded.
	codexRunFloorGrace = 5 * time.Second
)

// Vars rather than consts so tests can pin them small.
var (
	codexRefreshAfterRunRetryDelay = 5 * time.Second
	// codexRunFloorWriteAttempts / codexRunFloorWriteRetryDelay bound the
	// retries armCodexUsageRunFloor spends getting a run start onto disk when
	// the bounded cache locks refuse the write.
	codexRunFloorWriteAttempts   = 3
	codexRunFloorWriteRetryDelay = time.Second
	// codexForcedReconcileMinInterval spaces forced reconciles per account. A
	// user-initiated refresh bypasses it; nothing bypasses the single flight.
	codexForcedReconcileMinInterval = 20 * time.Second
	codexUsageFreshnessNow          = time.Now
)

// codexRunHooks lets the manager tests observe the two lifecycle calls without
// running a reconcile. Atomic because a previous test's exit watcher can still
// be reaching codexUsageRunSettled while the next test installs its recorder.
type codexRunHooks struct {
	started  func(time.Time)
	settled  func(time.Time)
	disarmed func(floor, fallback time.Time)
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

// codexUsageRunDisarmed is what a session manager calls when a run it armed at
// `floor` never started — its request never reached the child. `fallback` is
// the newest run that manager still has open, or zero: the arm coalesced onto
// this floor, so that is what the persisted floor rolls back to.
//
// The persisted floor is ACCOUNT-WIDE while several managers arm it — the
// app-server manager and the terminal `codex` sessions of SessionManager — so
// the rollback falls back to the newest run open in ANY of them
// (codexNewestOpenRunFloor), never just the caller's own. Otherwise an
// app-server turn write failing while a terminal run is the only other thing
// open would reset the shared floor to zero and erase that run's
// crash-recovery marker.
func codexUsageRunDisarmed(floor, fallback time.Time) {
	if open := codexNewestOpenRunFloor(); open > 0 && (fallback.IsZero() || open > fallback.UnixMilli()) {
		fallback = time.UnixMilli(open)
	}
	if hooks := codexRunHookOverride.Load(); hooks != nil {
		if hooks.disarmed != nil {
			hooks.disarmed(floor, fallback)
		}
		return
	}
	disarmCodexUsageRunFloor(floor, fallback)
}

// codexNewestOpenRunFloor reports the newest Codex run floor still open across
// every run source of this process: the terminal sessions of the global
// SessionManager and the turns of the global app-server manager. Both are nil
// outside StartAgent (tests), where each manager accounts for itself. The
// caller has already withdrawn the floor being rolled back from its own
// manager, so whatever remains is honestly still open.
func codexNewestOpenRunFloor() int64 {
	newest := globalSessionManager.newestOpenCodexUsageFloor()
	if floor := globalCodexAppServerManager.newestOpenUsageFloor(nil); floor > newest {
		newest = floor
	}
	return newest
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
	// pendingAttempts holds forced reconciles that were spent but whose
	// RefreshOwedAttempts write the bounded cache locks refused, so the stale
	// notice is not withheld forever by an uncounted attempt. Each retained
	// count carries the debt GENERATION it was spent on: a run that settles
	// meanwhile resets the counter on disk, and folding an older generation's
	// attempt into it would push a brand-new debt towards the stale warning
	// on reconciles that never targeted its floor.
	pendingAttempts map[string]codexRetainedAttempts
	// armed maps a run start (floor, ms) to the account fingerprints that were
	// live when runs were armed at it, so each run's settlement is attributed
	// to the account that actually made it even if the Codex credentials
	// change while it runs. Without it a swap mid-run records account A's debt
	// against B, which can later show B a stale-run warning for a run B never
	// made while A's promised refresh is lost for good. The value is a queue
	// because every caller truncates its floor to a millisecond: overlapping
	// turns armed inside the same millisecond share a key and must each keep
	// their own binding, or the second settlement reads as unbound.
	armed map[int64][]string
	// disarmed holds run starts withdrawn (codexUsageRunDisarmed) that may not
	// have reached disk yet: an arm is written by its own retried goroutine,
	// so the withdrawal can land first, and the late arm must then be dropped
	// rather than persist a floor for a run that never happened.
	disarmed map[string]map[int64]struct{}
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

// codexDebtID identifies ONE generation of run debt — the floor it waits on
// and the completion that created it. Every codexOweRunRefresh starts a new
// generation (and resets RefreshOwedAttempts), so an attempt spent on an
// earlier one must never land on a later one.
type codexDebtID struct {
	floorMs  int64
	owedAtMs int64
}

func (id codexDebtID) valid() bool { return id.owedAtMs > 0 && id.floorMs > 0 }

// debtID names the generation this state describes, or the zero id when
// nothing is owed.
func (s codexRunFreshnessState) debtID() codexDebtID {
	if !s.owed {
		return codexDebtID{}
	}
	return codexDebtID{floorMs: s.floor.UnixMilli(), owedAtMs: s.owedAt.UnixMilli()}
}

// codexRetainedAttempts is a refused attempt count plus the debt it belongs to.
type codexRetainedAttempts struct {
	id codexDebtID
	n  int
}

// codexPendingRunDebt is one finished run whose debt is not on disk yet.
type codexPendingRunDebt struct {
	floor       time.Time
	completedAt time.Time
}

var codexUsageRefresh = newCodexUsageRefreshGate()

func newCodexUsageRefreshGate() *codexUsageRefreshGate {
	return &codexUsageRefreshGate{
		inFlight:        map[string]chan struct{}{},
		lastRun:         map[string]time.Time{},
		workers:         map[string]bool{},
		pending:         map[string]codexPendingRunDebt{},
		pendingAttempts: map[string]codexRetainedAttempts{},
		armed:           map[int64][]string{},
		disarmed:        map[string]map[int64]struct{}{},
		cancel:          make(chan struct{}),
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

// rememberAttempts retains attempts whose counter write was refused, under the
// debt generation they were spent on. A count retained for an OLDER generation
// is dropped rather than carried over: the newer debt reset the counter on
// purpose, and those attempts did not target its floor.
func (g *codexUsageRefreshGate) rememberAttempts(fp string, id codexDebtID, n int) {
	if !id.valid() || n <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if cur, ok := g.pendingAttempts[fp]; ok && cur.id == id {
		n += cur.n
	}
	g.pendingAttempts[fp] = codexRetainedAttempts{id: id, n: n}
}

// takeAttempts hands the next write the attempts retained for THIS debt; a
// count held for another generation is discarded, since it can never be
// counted anywhere.
func (g *codexUsageRefreshGate) takeAttempts(fp string, id codexDebtID) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	cur, ok := g.pendingAttempts[fp]
	if !ok {
		return 0
	}
	delete(g.pendingAttempts, fp)
	if cur.id != id {
		return 0
	}
	return cur.n
}

// takeRetainedAttempts drains whatever generation is retained for fp, for the
// flush-only path that has no debt of its own to count.
func (g *codexUsageRefreshGate) takeRetainedAttempts(fp string) (codexDebtID, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	cur := g.pendingAttempts[fp]
	delete(g.pendingAttempts, fp)
	return cur.id, cur.n
}

// rememberArmedAccount pins the account that was live when the run starting at
// floorMs was armed; takeArmedAccount hands it to that run's settlement (or
// withdrawal) and forgets it. A miss means the run predates this process — a
// floor replayed from disk at startup — and the caller falls back to the
// account that is live now, which is the only one it can reconcile anyway.
// Runs sharing a floor queue their bindings and consume them one settlement at
// a time, so overlapping turns armed in the same millisecond stay bound.
func (g *codexUsageRefreshGate) rememberArmedAccount(floorMs int64, fp string) {
	if floorMs <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.armed == nil {
		g.armed = map[int64][]string{}
	}
	g.armed[floorMs] = append(g.armed[floorMs], fp)
	// Evict oldest-first: the oldest floor is the one whose run is least
	// likely to still be open and owed a settlement. The cap counts bindings,
	// not keys, so a floor holding several overlapping runs cannot grow the
	// map past its bound.
	for total := g.armedCountLocked(); total > codexArmedAccountCap; total-- {
		oldest := int64(0)
		for ms := range g.armed {
			if oldest == 0 || ms < oldest {
				oldest = ms
			}
		}
		if rest := g.armed[oldest][1:]; len(rest) > 0 {
			g.armed[oldest] = rest
		} else {
			delete(g.armed, oldest)
		}
	}
}

// armedCountLocked totals the queued bindings; callers hold g.mu.
func (g *codexUsageRefreshGate) armedCountLocked() int {
	total := 0
	for _, fps := range g.armed {
		total += len(fps)
	}
	return total
}

func (g *codexUsageRefreshGate) takeArmedAccount(floorMs int64) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	fps := g.armed[floorMs]
	if len(fps) == 0 {
		return "", false
	}
	fp := fps[0]
	if rest := fps[1:]; len(rest) > 0 {
		g.armed[floorMs] = rest
	} else {
		delete(g.armed, floorMs)
	}
	return fp, true
}

// markDisarmed records that the run armed at floorMs was withdrawn before its
// arm was known to be on disk; takeDisarmed consumes that record. Both sides
// run inside the cache transaction, so an arm and its withdrawal are ordered
// by the cache lock: whichever lands second sees the other's work.
func (g *codexUsageRefreshGate) markDisarmed(fp string, floorMs int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	floors := g.disarmed[fp]
	if floors == nil {
		floors = map[int64]struct{}{}
		g.disarmed[fp] = floors
	}
	floors[floorMs] = struct{}{}
}

func (g *codexUsageRefreshGate) takeDisarmed(fp string, floorMs int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	floors := g.disarmed[fp]
	if _, ok := floors[floorMs]; !ok {
		return false
	}
	delete(floors, floorMs)
	if len(floors) == 0 {
		delete(g.disarmed, fp)
	}
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
	codexUsageRefresh.waitIdle()
	codexUsageRefresh.mu.Lock()
	codexUsageRefresh.enabled = false
	codexUsageRefresh.inFlight = map[string]chan struct{}{}
	codexUsageRefresh.lastRun = map[string]time.Time{}
	codexUsageRefresh.workers = map[string]bool{}
	codexUsageRefresh.pending = map[string]codexPendingRunDebt{}
	codexUsageRefresh.pendingAttempts = map[string]codexRetainedAttempts{}
	codexUsageRefresh.armed = map[int64][]string{}
	codexUsageRefresh.disarmed = map[string]map[int64]struct{}{}
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

// codexDisarmRunFloor rolls back a start codexArmRunFloor recorded for a run
// that never happened, to fallbackMs — the newest run its manager still has
// open, or zero. Only the exact floor is touched, wherever the arm left it: a
// newer floor belongs to another run and stays; an older one was never
// displaced by this arm. Reports whether anything changed.
func codexDisarmRunFloor(snap *codexRateLimitSnapshot, floorMs, fallbackMs int64) bool {
	switch {
	case floorMs <= 0:
		return false
	case snap.ActiveRunFloorMs == floorMs:
		// Parked behind a debt: the debt keeps its floor, the parked start
		// falls back to the manager's remaining open run when that is newer
		// than the debt, else there is nothing left to promote.
		if fallbackMs > snap.RunFloorMs {
			snap.ActiveRunFloorMs = fallbackMs
		} else {
			snap.ActiveRunFloorMs = 0
		}
		return true
	case snap.RunFloorMs == floorMs:
		// The arm coalesced onto this floor, so an older open run's start (or a
		// debt that later coalesced onto it) is what it stood in for. A zero
		// fallback with a debt outstanding lets codexSettleRunFreshness retire
		// that debt: no run is left for it to describe.
		snap.RunFloorMs = fallbackMs
		if snap.ActiveRunFloorMs <= snap.RunFloorMs {
			snap.ActiveRunFloorMs = 0
		}
		return true
	}
	return false
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

// codexCountRefreshAttempt counts one bounded attempt against the debt `id`
// names, and only that one. The snapshot is re-read inside the transaction, so
// a run that settled between the reconcile and this write has already replaced
// the debt (and reset the count); counting there would spend a new run's
// budget on a reconcile that never looked at its floor.
func codexCountRefreshAttempt(snap *codexRateLimitSnapshot, id codexDebtID) {
	if snap.RefreshOwedAtMs <= 0 || snap.RefreshOwedAtMs != id.owedAtMs || snap.RunFloorMs != id.floorMs {
		return
	}
	snap.RefreshOwedAttempts++
}

/* ─────────────────────────── lifecycle hooks ─────────────────────────── */

// armCodexUsageRunFloor records that a Codex run started at startedAt. The
// write takes the cache's blocking cross-process lock, so it runs off the
// caller's goroutine — both managers call this while holding their own mutex.
func armCodexUsageRunFloor(startedAt time.Time) {
	if !codexUsageRefresh.isEnabled() {
		return
	}
	// Resolved HERE, not on the spawned goroutine: this is the account that
	// made the run, and it is what settlement (or withdrawal) must be booked
	// against even if the credentials change before either lands.
	fp := codexAccountFingerprintAtBase(codexHomeBase())
	codexUsageRefresh.rememberArmedAccount(startedAt.UnixMilli(), fp)
	codexUsageRefresh.spawn(func() {
		// A refused write is RETRIED: bounded locking deliberately lets this
		// lose the race for the cache lock, and a start that never reached disk
		// is a run startup cannot classify as interrupted if the process dies
		// or is replaced before it settles — its promised refresh is then lost
		// for good. Re-applying the arm is safe at any later moment:
		// codexArmRunFloor coalesces onto the newest floor and never moves one
		// backwards, so a late write can neither lower a floor nor re-open a
		// run that has already settled.
		for attempt := 0; attempt < codexRunFloorWriteAttempts; attempt++ {
			if attempt > 0 && !codexUsageRefresh.sleep(codexRunFloorWriteRetryDelay) {
				return
			}
			if codexRecordRunFreshness(fp, codexUsageFreshnessNow(), func(snap *codexRateLimitSnapshot) {
				// Withdrawn (codexUsageRunDisarmed) before this write landed: the
				// run never happened, so its floor must not reach disk at all.
				if codexUsageRefresh.takeDisarmed(fp, startedAt.UnixMilli()) {
					return
				}
				codexArmRunFloor(snap, startedAt)
			}) {
				return
			}
		}
	})
}

// disarmCodexUsageRunFloor withdraws a run start armCodexUsageRunFloor recorded
// (or is still retrying) for a run that never started — its turn request never
// reached the child. Left on disk, that floor would be read as an interrupted
// run at the next process start and converted into a debt no telemetry can
// pay, which ages into a false stale-utilization warning. The withdrawal is
// marked in the gate BEFORE the rollback so a late arm write is dropped, and
// a rollback the bounded cache locks refuse is retried like the arm itself.
func disarmCodexUsageRunFloor(floor, fallback time.Time) {
	if !codexUsageRefresh.isEnabled() || floor.IsZero() {
		return
	}
	floorMs := floor.UnixMilli()
	var fallbackMs int64
	if !fallback.IsZero() {
		fallbackMs = fallback.UnixMilli()
	}
	fp, _ := codexUsageRefresh.takeArmedAccount(floorMs)
	if fp == "" {
		fp = codexAccountFingerprintAtBase(codexHomeBase())
	}
	codexUsageRefresh.spawn(func() {
		codexUsageRefresh.markDisarmed(fp, floorMs)
		for attempt := 0; attempt < codexRunFloorWriteAttempts; attempt++ {
			if attempt > 0 && !codexUsageRefresh.sleep(codexRunFloorWriteRetryDelay) {
				return
			}
			var reached bool
			if codexRecordRunFreshness(fp, codexUsageFreshnessNow(), func(snap *codexRateLimitSnapshot) {
				reached = true
				if codexDisarmRunFloor(snap, floorMs, fallbackMs) {
					// The arm had landed; the mark has nothing left to stop.
					codexUsageRefresh.takeDisarmed(fp, floorMs)
				}
			}) || reached {
				return
			}
		}
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
	armed, _ := codexUsageRefresh.takeArmedAccount(floor.UnixMilli())
	codexUsageRefresh.spawn(func() {
		codexRefreshAfterRun(armed, floor, completedAt)
	})
}

// codexRefreshAfterRun settles the run that started at `floor`. `armed` is the
// account that was live when the run was armed, or "" when this process never
// armed it (a floor replayed from disk at startup).
func codexRefreshAfterRun(armed string, floor, completedAt time.Time) {
	base := codexHomeBase()
	fp := codexAccountFingerprintAtBase(base)
	if armed != "" && armed != fp {
		// The Codex credentials changed while the run was in flight. Its
		// telemetry is unreachable — the rollout scan only reads the account
		// that is live now — and booking the debt either way is wrong: under
		// the live account it would show a stale-run warning for a run that
		// account never made, and under the armed one it would rescope the
		// cache and discard the live account's readings. Drop it; the armed
		// account's floor is cleared by the next write that rescopes the
		// snapshot (codexScopeSnapshotToAccount).
		return
	}
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
			// Last chance to land a debt or attempts the cache lock refused:
			// nothing else runs for this account until another run finishes
			// or a gather reconciles — and the gather side flushes whatever is
			// still retained here (codexReconcileForGather).
			codexFlushPendingRunDebt(fp)
			codexRecordRefreshAttempt(fp, codexDebtID{}, 0)
			return
		}
	}
}

// codexPayRunRefresh spends up to codexRefreshAfterRunMaxAttempts forced
// reconciles on fp's outstanding debt, restarting the count when another run
// finishes meanwhile. A debt still RETAINED in the gate is landed first under
// its own write budget (codexLandPendingRunDebt): a refused write is a lock
// race, not a reconcile, and must not consume the reconcile attempts — those
// gate the stale notice — or the worker would retire with the debt still
// unrecorded and nothing left scheduled to record it.
func codexPayRunRefresh(base, fp string) {
	for attempt := 0; attempt < codexRefreshAfterRunMaxAttempts; attempt++ {
		if attempt > 0 && !codexUsageRefresh.sleep(codexRefreshAfterRunRetryDelay) {
			return
		}
		if codexUsageRefresh.takeRearm(fp) {
			attempt = 0
		}
		if !codexLandPendingRunDebt(fp) {
			// Still unrecorded after the write budget: the process is
			// shutting down, or the lock is wedged past every bound. The
			// debt stays retained for the worker's last-chance flush and the
			// next gather; a reconcile now would read `owed == false` off
			// disk and retire a run that was never recorded.
			return
		}
		state := codexRunFreshnessForAccount(fp, codexUsageFreshnessNow())
		if !state.owed {
			return // paid by capture, a gather, or an earlier attempt
		}
		if !codexAwaitGatedReconcile(base, fp, state.floor) {
			return
		}
		codexRecordRefreshAttempt(fp, state.debtID(), 1)
	}
}

// codexRecordRefreshAttempt counts `spent` attempts against fp's debt, folding
// in any whose write the bounded cache locks previously refused. The count is
// what codexStaleRunNotice gates the warning on (attempts >=
// codexRefreshAfterRunMaxAttempts), so an attempt that was spent but never
// recorded leaves a run whose telemetry never appears reconciling on every
// later gather while the card never says why it looks old. A refused write is
// retained for the next transaction instead; spent=0 only flushes whatever
// generation is still retained.
//
// `id` binds the count to the debt the attempt was actually spent on, so a run
// that settles between the reconcile and this write keeps the full attempt
// budget its own floor is owed.
func codexRecordRefreshAttempt(fp string, id codexDebtID, spent int) {
	if spent <= 0 {
		id, spent = codexUsageRefresh.takeRetainedAttempts(fp)
	} else {
		spent += codexUsageRefresh.takeAttempts(fp, id)
	}
	if spent <= 0 || !id.valid() {
		return
	}
	if codexRecordRunFreshness(fp, codexUsageFreshnessNow(), func(snap *codexRateLimitSnapshot) {
		for i := 0; i < spent; i++ {
			codexCountRefreshAttempt(snap, id)
		}
	}) {
		return
	}
	codexUsageRefresh.rememberAttempts(fp, id, spent)
}

// codexLandPendingRunDebt retries codexFlushPendingRunDebt under the same
// bounded write budget an arm or a rollback gets (codexRunFloorWriteAttempts ×
// codexRunFloorWriteRetryDelay). Reports whether fp's debt is on disk.
func codexLandPendingRunDebt(fp string) bool {
	for attempt := 0; attempt < codexRunFloorWriteAttempts; attempt++ {
		if attempt > 0 && !codexUsageRefresh.sleep(codexRunFloorWriteRetryDelay) {
			return false
		}
		if codexFlushPendingRunDebt(fp) {
			return true
		}
	}
	return false
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
		// Either conversion below may be REFUSED by the bounded cache locks
		// and retained in the gate instead. Such a debt is handed to the
		// account's worker whatever happens to it here: the reconcile this
		// replay runs happens BEFORE the retained debt is on disk, so its
		// attempt cannot be counted against a debt that does not exist yet,
		// and the conversion that finally lands resets the count to zero.
		// Without the worker, every later gather would force a scan on the
		// owed debt but never count an attempt, so a run whose telemetry
		// never appears would never reach the stale-warning threshold.
		retained := false
		// The generation this replay's one reconcile is spent on: the debt
		// already on disk, or the one the conversion below creates (floor
		// unchanged, completed now). Naming it here keeps a run that settles
		// mid-replay from inheriting this attempt.
		debt := state.debtID()
		if state.interrupted {
			// The run is over — its process is gone — so it is owed from now on:
			// the stale notice and the age-out both need a completion time.
			retained = !codexOweInterruptedRun(fp, state.floor, now)
			debt = codexDebtID{floorMs: state.floor.UnixMilli(), owedAtMs: now.UnixMilli()}
		}
		ctx, cancel := context.WithTimeout(context.Background(), codexForcedReconcileBudget)
		defer cancel()
		// Bypass the interval: nothing in this fresh process has reconciled yet.
		if _, ran, _, _ := codexRunGatedReconcile(ctx, base, fp, state.floor, true, now); ran {
			codexRecordRefreshAttempt(fp, debt, 1)
		}
		// The reconcile may have PAID the debt it was sent to pay and, in the
		// same transaction, promoted a floor that was parked behind it — a
		// second, newer run this dead process never settled. That run is over
		// too, so turn the promoted floor into debt of its own: routine gathers
		// force only on `owed`, so an interrupted floor left un-owed would
		// never be refreshed and would simply age out.
		now = codexUsageFreshnessNow()
		if promoted := codexRunFreshnessForAccount(fp, now); promoted.interrupted {
			if !codexOweInterruptedRun(fp, promoted.floor, now) {
				retained = true
			}
		}
		// A retained conversion gets the same bounded worker a refused
		// post-run settle gets: it lands the debt (codexLandPendingRunDebt)
		// and then spends the reconcile attempts the debt is owed, so the
		// stale notice can still be reached. A run left merely `interrupted`
		// would otherwise never be refreshed — routine gathers force only on
		// `owed` — and never warn.
		if retained {
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
	// A settle the bounded cache locks refused past the worker's write budget
	// is still retained in the gate; nothing else runs for this account until
	// another run finishes, so land it here — the write is skipped when
	// nothing is retained — or the debt would read as paid off disk.
	codexFlushPendingRunDebt(fp)
	state := codexRunFreshnessForAccount(fp, now)
	if forced || state.owed {
		var floor time.Time
		if state.owed {
			floor = state.floor
		}
		res, ran, busy, _ := codexRunGatedReconcile(ctx, base, fp, floor, forced, now)
		if ran {
			// A reconcile spent ON a debt counts as one of its bounded
			// attempts, exactly like the post-run worker's. Uncounted, a debt
			// carried over from a previous process — whose startup replay
			// spends one attempt and then retires — would be rescanned by
			// every later gather while never reaching the stale-notice
			// threshold (codexStaleRunNotice gates on attempts >=
			// codexRefreshAfterRunMaxAttempts), so the card would keep looking
			// current and age the debt out without ever saying why it is old.
			// A no-op once this reconcile PAID the debt: the same transaction
			// cleared the count (codexCountRefreshAttempt).
			if state.owed {
				codexRecordRefreshAttempt(fp, state.debtID(), 1)
			}
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
	return codexRunCompletionShape(line) != ""
}

// codexRunCompletionShape is codexRunCompletionFrame's classifying form: the
// completion's normalized event name (`turn.completed` or `thread.completed`),
// or "" for any other line. A finished turn may announce itself in BOTH shapes,
// one after the other; the app-server treats the name as an opaque label so it
// can pair the second announcement with the first instead of settling another
// turn on it — without learning what either name means.
func codexRunCompletionShape(line string) string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") || !strings.Contains(trimmed, "completed") {
		return ""
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return ""
	}
	for _, name := range codexFrameEventNames(raw) {
		if codexCompletionEventName(name) {
			return codexNormalizeCompletionName(name)
		}
	}
	return ""
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
