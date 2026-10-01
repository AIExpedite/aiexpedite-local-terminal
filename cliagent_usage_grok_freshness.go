// cliagent_usage_grok_freshness.go — the bounded run-completion refresh that
// keeps Grok's credit reading current after a run.
//
// Why this exists:
//
//	Grok ≥ 1.0.40 logs its `billing: fetched credits config` record without a
//	usage percentage, and headless runs (the maintenance smoke, ACP sessions,
//	PTY `grok -p`) log no billing record at all — every managed exit on a real
//	device reported `managed billing snapshot: no-record`. The only source that
//	still returns a number is the live billing read (cliagent_usage_grok_live.go),
//	and only a Refresh click used to send it, so a green smoke left the card on
//	a days-old reading or on Unknown.
//
// What it does, mirroring Codex and Antigravity:
//
//   - Arm. Every spawn path that can spend credits (the smoke, an ACP session,
//     a PTY `grok -p`) arms a floor before the child starts.
//   - Settle. At exit the run is covered when a reading for the signed-in
//     account was taken at or after it COMPLETED; otherwise it owes one live
//     billing read. One pending debt at a time: a newer run moves the debt's
//     completion forward but keeps its attempt count, so runs finishing during
//     an outage still stop at grokRunDebtMaxAttempts reads.
//   - Pay. A process-wide single-flight worker spends at most
//     grokRunDebtMaxAttempts outbound reads per debt on the shared ladder
//     (cliagent_usage_refresh_ladder.go), spaced by grokRefreshMinInterval;
//     the read itself shares grokBillingReadOnce's per-account flight with the
//     Refresh click, so the two never send two requests.
//   - Survive. The debt and its next rung are persisted; StartAgent re-arms
//     them (payOwedGrokUsageRefresh), and a floor whose run the previous
//     process never settled is owed one read.
//   - Discover. The gather's parser nudges the same worker when the log holds a
//     record newer than every reading (a `grok` the user ran in their own
//     shell), on a cooldown.
//
// Redaction: the state file holds epoch-millisecond integers, a counter, the
// hashed account fingerprint and a closed outcome code. Log lines carry fixed
// labels and counters — never a path, an account, a token or a response body.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// grokRunDebtMaxAttempts is a debt's lifetime budget of outbound billing
	// reads, across the settle's own attempt and every scheduled rung.
	grokRunDebtMaxAttempts = 4
	// grokRunDebtMaxAge retires a debt nothing could pay, so it never pins a
	// schedule forever.
	grokRunDebtMaxAge = 6 * time.Hour

	grokUsageFreshnessSchema = 1
	// grokUsageFreshnessEnv relocates the state file (tests isolate from the
	// real machine; mirrors AIEXPEDITE_GROK_BILLING_LIVE_CACHE).
	grokUsageFreshnessEnv = "AIEXPEDITE_GROK_USAGE_FRESHNESS"

	// grokRefreshOutcomeUnmetered is the debt's own closed label for an `ok`
	// read that carried no percentage: it pays nothing and spends budget.
	grokRefreshOutcomeUnmetered = "unmetered"
)

// Vars rather than consts so tests can pin them small.
var (
	// grokRunDebtRetryLadder is the delay before the next attempt, by the
	// attempts already booked. The first rung equals grokRefreshMinInterval so
	// a rung never fires only to defer on the spacing.
	grokRunDebtRetryLadder = []time.Duration{
		time.Minute, 2 * time.Minute, 8 * time.Minute, 30 * time.Minute,
	}
	// grokRunDebtFreeRetryDelay is the floor of the age backoff after a refusal
	// that spent no budget (offline, login_busy, write_failed) and the short
	// rung after a spacing deferral.
	grokRunDebtFreeRetryDelay = 30 * time.Second
	// grokRefreshMinInterval spaces any two outbound debt reads. Only the
	// smoke's first attempt bypasses it.
	grokRefreshMinInterval = time.Minute
	// grokRefreshNudgeCooldown bounds how often the gather may arm the worker.
	grokRefreshNudgeCooldown = time.Minute
	// grokRunDebtReadTimeout bounds one debt read, renewal included: a token
	// refused on the first GET waits its turn (in-process, then the
	// cross-process renewal lock), renews, and sends a second GET, so both
	// GETs fit inside it rather than the retry being cut off.
	grokRunDebtReadTimeout = 2*grokBillingLiveTimeout + grokLoginRenewTimeout + grokLoginRenewGap + grokAuthLockWait
	// grokRunDebtReadWaitSlack is how much longer a waiter gives the shared
	// read than the read gives itself, so the read's own deadline — a real
	// outcome — fires before the waiter gives up with a `timeout`.
	grokRunDebtReadWaitSlack = 5 * time.Second
	grokUsageFreshnessNow    = time.Now
	// grokUsageRefreshGrokPath resolves the CLI the read may renew the login
	// with; a seam so tests never touch a real install.
	grokUsageRefreshGrokPath = grokLoginKeeperBinary
	// grokUsageCurrentFingerprint is the account signed in now.
	grokUsageCurrentFingerprint = func() string { return grokAccountFingerprintFor(grokPersistentHome()) }
	// grokUsageHomeFingerprint is the account an explicit Grok home (a run's
	// isolated copy of the login) is signed in as.
	grokUsageHomeFingerprint = grokAccountFingerprintFor
)

// grokRunDebtPassWait is how long a debt pass waits for its read. A
// same-account flight sent BEFORE the run completed (a Refresh click) cannot
// pay the debt, yet the pass must outwait it before sending its own — one
// request at a time — so the wait covers two whole reads, not one.
func grokRunDebtPassWait() time.Duration {
	return 2*grokRunDebtReadTimeout + grokRunDebtReadWaitSlack
}

// grokUsageFreshness is the persisted state. Every field is a number, the
// hashed fingerprint, or a closed outcome code.
type grokUsageFreshness struct {
	SchemaVersion int `json:"schemaVersion,omitempty"`
	// RunFloorMs is the newest armed floor not yet settled, or the pending
	// debt's floor. A restart that finds it with no debt beside it owes the
	// interrupted run one read.
	RunFloorMs int64 `json:"runFloorMs,omitempty"`
	// RunFloorAccount is the account RunFloorMs's run was spawned under, so a
	// restart never adopts that run's debt against a different login.
	RunFloorAccount string `json:"runFloorAccount,omitempty"`
	// CompletionMs is the instant a reading must be taken at or after to pay
	// the debt, and OwedAtMs when the debt was created (0 = nothing owed).
	CompletionMs int64 `json:"completionMs,omitempty"`
	OwedAtMs     int64 `json:"owedAtMs,omitempty"`
	// NextAttemptAtMs is the booked rung (0 = none), persisted so a restart
	// re-arms the same schedule.
	NextAttemptAtMs int64 `json:"nextAttemptAtMs,omitempty"`
	// LastAttemptAtMs is the last OUTBOUND read, which the spacing measures.
	LastAttemptAtMs int64 `json:"lastAttemptAtMs,omitempty"`
	Attempts        int   `json:"attempts,omitempty"`
	// AccountFingerprint is the account the debt was raised under; a debt is
	// never paid against another login.
	AccountFingerprint string `json:"accountFingerprint,omitempty"`
	// LastOutcome is one of the closed grokLiveOutcome* codes.
	LastOutcome string `json:"lastOutcome,omitempty"`
}

func (state grokUsageFreshness) owed() bool { return state.OwedAtMs != 0 }

// setRunFloor moves the restart marker, always together with its account.
func (state *grokUsageFreshness) setRunFloor(floorMs int64, fingerprint string) {
	state.RunFloorMs, state.RunFloorAccount = floorMs, fingerprint
	if floorMs == 0 {
		state.RunFloorAccount = ""
	}
}

func (state *grokUsageFreshness) clearDebt() {
	state.CompletionMs, state.OwedAtMs, state.NextAttemptAtMs = 0, 0, 0
	state.Attempts, state.AccountFingerprint, state.LastOutcome = 0, "", ""
}

// grokDebtID names one generation of the debt, so a rung booked for a debt
// that was since paid or replaced writes nothing.
type grokDebtID struct{ owedAtMs, completionMs int64 }

func (state grokUsageFreshness) debtID() grokDebtID {
	return grokDebtID{owedAtMs: state.OwedAtMs, completionMs: state.CompletionMs}
}

var (
	grokFreshnessMu sync.Mutex
	// grokUsageRefreshEnabled is set only by StartAgent: tests that settle a
	// Grok run without opting in must never reach xAI.
	grokUsageRefreshEnabled atomic.Bool
	// grokFreshnessInFlight counts the worker and timer callbacks, so the
	// smoke's bounded wait and tests can wait them out.
	grokFreshnessInFlight atomic.Int64

	grokRefreshWorkerMu      sync.Mutex
	grokRefreshWorkerRunning bool
	grokRefreshWorkerRearm   bool
	// grokRefreshWorkerBypass is the pending pass's spacing bypass, taken by
	// whichever pass runs next.
	grokRefreshWorkerBypass bool

	grokLiveRunsMu sync.Mutex
	// grokLiveRuns holds the armed floors; each live run has its own
	// (armGrokUsageRunFloorFor), so the count is 0 or 1.
	grokLiveRuns = map[int64]int{}
	// grokLiveRunAccounts is the account each armed floor was spawned under,
	// frozen at arm: an isolated child keeps its copied login for its whole
	// life, so its debt belongs to that account even if the persistent login
	// changes before it exits.
	grokLiveRunAccounts = map[int64]string{}

	// grokLastRefreshOutcome is the newest pass's closed outcome, for the
	// smoke's log line.
	grokLastRefreshOutcome atomic.Value

	grokRunDebtRetryTimer struct {
		mu    sync.Mutex
		timer *time.Timer
		gen   uint64
	}
	grokRefreshNudge struct {
		mu     sync.Mutex
		lastAt time.Time
	}
)

// SetGrokUsageRefreshEnabled arms (or disarms) the run-completion refresh for
// this process. Called from StartAgent.
func SetGrokUsageRefreshEnabled(enabled bool) { grokUsageRefreshEnabled.Store(enabled) }

func grokUsageFreshnessPath() string {
	if p := os.Getenv(grokUsageFreshnessEnv); p != "" {
		return p
	}
	return filepath.Join(GetConfigDir(), "grok_usage_freshness.json")
}

// updateGrokUsageFreshness applies mutate under the freshness lock and
// persists the result when it changed (temp-file + rename, removed once empty).
// A missing or corrupt file reads as "nothing owed". Nothing under this lock
// may take the billing cache lock.
func updateGrokUsageFreshness(mutate func(*grokUsageFreshness)) grokUsageFreshness {
	grokFreshnessMu.Lock()
	defer grokFreshnessMu.Unlock()
	path := grokUsageFreshnessPath()
	var state grokUsageFreshness
	if !readJSONFile(path, &state) {
		state = grokUsageFreshness{}
	}
	before := state
	mutate(&state)
	if state == before || path == "" {
		return state
	}
	if state.RunFloorMs == 0 && !state.owed() && state.LastAttemptAtMs == 0 {
		_ = os.Remove(path)
		return state
	}
	state.SchemaVersion = grokUsageFreshnessSchema
	// Best-effort: a read-only data dir costs a refresh, never a run.
	_ = writeJSONFileAtomic(path, state)
	return state
}

// grokRebaseFutureFreshness rebases timestamps a backwards clock step left in
// the future (mirrors antigravityRebaseFutureFreshness): a floor or completion
// further ahead than grokBillingMaxClockSkew is pulled back to now, and a rung
// further ahead than the longest one is due now.
func grokRebaseFutureFreshness(state *grokUsageFreshness, now time.Time) {
	nowMs := now.UnixMilli()
	ceiling := now.Add(grokBillingMaxClockSkew).UnixMilli()
	for _, ts := range []*int64{&state.RunFloorMs, &state.CompletionMs, &state.OwedAtMs, &state.LastAttemptAtMs} {
		if *ts > ceiling {
			*ts = nowMs
		}
	}
	if state.NextAttemptAtMs > now.Add(grokRunDebtRetryLadder[len(grokRunDebtRetryLadder)-1]+grokBillingMaxClockSkew).UnixMilli() {
		state.NextAttemptAtMs = nowMs
	}
}

// grokDebtExhausted reports a debt whose read budget is spent. It stays on
// disk (so later runs cannot re-open the budget) until the longest rung has
// passed since its last read; a run settling after that gets a fresh budget.
func grokDebtExhausted(state grokUsageFreshness, now time.Time) bool {
	if state.Attempts < grokRunDebtMaxAttempts {
		return false
	}
	cooling := grokRunDebtRetryLadder[len(grokRunDebtRetryLadder)-1]
	return now.Sub(time.UnixMilli(state.LastAttemptAtMs)) < cooling
}

// grokObservationCovers reports whether a NUMERIC reading for fingerprint was
// taken at or after completionMs — the live read or a log record. A reading
// without a percentage pays nothing: the card would still have no number, and
// retiring the debt on it would stop the ladder that can still get one.
func grokObservationCovers(fingerprint string, completionMs int64) bool {
	if live, ok := loadGrokBillingLiveSnapshot(fingerprint); ok && live.HasUsedPercent && live.ObservedAt.UnixMilli() >= completionMs {
		return true
	}
	base := grokPersistentHome()
	if base == "" || grokAccountFingerprintFor(base) != fingerprint {
		return false
	}
	snap, ok := readGrokBillingSnapshot(base, grokIdentityCandidates(base))
	return ok && snap.HasUsedPercent && snap.ObservedAt.UnixMilli() >= completionMs
}

/* ──────────────────────────────── arm ──────────────────────────────── */

// armGrokUsageRunFloor records that a run which can spend credits is starting
// against the persistent login and returns its floor; zero when the refresh
// is disabled.
func armGrokUsageRunFloor(now time.Time) time.Time {
	return armGrokUsageRunFloorFor(now, "")
}

// armGrokUsageRunFloorFor is armGrokUsageRunFloor for a run whose child bills
// the login copied into credentialHome (an isolated smoke / ACP / PTY home):
// the floor is bound to THAT account, not to whatever the persistent home is
// signed in as when the arm runs. An empty home means the persistent login.
//
// The floor is registered as live on the caller's goroutine, but persisted off
// it, as armAntigravityUsageRunFloor does: the ACP manager arms while holding
// its own mutex across the spawn, and a slow disk must not stall that. A
// persist that lands after the run already settled is skipped, so it can
// never leave a stale floor for the next start to adopt.
func armGrokUsageRunFloorFor(now time.Time, credentialHome string) time.Time {
	if !grokUsageRefreshEnabled.Load() {
		return time.Time{}
	}
	fingerprint := grokUsageCurrentFingerprint()
	if credentialHome != "" {
		fingerprint = grokUsageHomeFingerprint(credentialHome)
	}
	// Every live run gets a floor of its own, so each keeps the account it
	// was spawned under: two runs armed in the same millisecond (isolated
	// copies of different logins) must not share one binding. A taken
	// millisecond moves the floor EARLIER, never later — a floor is a lower
	// bound, and an earlier one only makes a reading prove less.
	floor := now
	grokLiveRunsMu.Lock()
	for grokLiveRuns[floor.UnixMilli()] > 0 {
		floor = floor.Add(-time.Millisecond)
	}
	floorMs := floor.UnixMilli()
	grokLiveRuns[floorMs] = 1
	grokLiveRunAccounts[floorMs] = fingerprint
	grokLiveRunsMu.Unlock()
	grokFreshnessInFlight.Add(1)
	go func() {
		defer grokFreshnessInFlight.Add(-1)
		updateGrokUsageFreshness(func(state *grokUsageFreshness) {
			if !grokRunIsLive(floorMs) {
				return
			}
			grokRebaseFutureFreshness(state, now)
			if floorMs > state.RunFloorMs {
				state.setRunFloor(floorMs, fingerprint)
			}
		})
	}()
	return floor
}

// grokLiveRunAccount is the account an armed floor was spawned under; false
// when the floor is not armed in this process.
func grokLiveRunAccount(floorMs int64) (string, bool) {
	grokLiveRunsMu.Lock()
	defer grokLiveRunsMu.Unlock()
	if grokLiveRuns[floorMs] == 0 {
		return "", false
	}
	fingerprint, ok := grokLiveRunAccounts[floorMs]
	return fingerprint, ok
}

func grokRunIsLive(floorMs int64) bool {
	grokLiveRunsMu.Lock()
	defer grokLiveRunsMu.Unlock()
	return grokLiveRuns[floorMs] > 0
}

// grokLiveFloor is an armed floor and the account it was spawned under.
type grokLiveFloor struct {
	floorMs     int64
	fingerprint string
}

// grokOldestLiveFloor is the earliest run still armed (zero when none).
// Lock order: freshness -> live runs; never the reverse.
func grokOldestLiveFloor() grokLiveFloor {
	grokLiveRunsMu.Lock()
	defer grokLiveRunsMu.Unlock()
	return grokOldestLiveFloorLocked()
}

func grokOldestLiveFloorLocked() grokLiveFloor {
	oldest := int64(0)
	for f := range grokLiveRuns {
		if oldest == 0 || f < oldest {
			oldest = f
		}
	}
	return grokLiveFloor{floorMs: oldest, fingerprint: grokLiveRunAccounts[oldest]}
}

// grokReleaseLiveRun forgets an armed floor and returns the oldest one still
// live (zero when none).
func grokReleaseLiveRun(floorMs int64) grokLiveFloor {
	grokLiveRunsMu.Lock()
	defer grokLiveRunsMu.Unlock()
	delete(grokLiveRuns, floorMs)
	delete(grokLiveRunAccounts, floorMs)
	return grokOldestLiveFloorLocked()
}

// grokDropSettledFloor rolls the restart marker back once its run is
// accounted for — to the oldest run still live, or the debt's own floor.
func grokDropSettledFloor(state *grokUsageFreshness, floorMs int64, oldestLive grokLiveFloor) {
	if state.RunFloorMs == 0 || state.RunFloorMs > floorMs || state.owed() {
		return
	}
	state.setRunFloor(oldestLive.floorMs, oldestLive.fingerprint)
}

// disarmGrokUsageRunFloor withdraws a run that never reached inference (a
// refused flag, an auth refusal, a failed spawn): it spent nothing and owes
// nothing.
func disarmGrokUsageRunFloor(floor time.Time) {
	if floor.IsZero() {
		return
	}
	oldest := grokReleaseLiveRun(floor.UnixMilli())
	updateGrokUsageFreshness(func(state *grokUsageFreshness) {
		grokDropSettledFloor(state, floor.UnixMilli(), oldest)
	})
}

/* ─────────────────────────────── settle ─────────────────────────────── */

// grokUsageRunSettled decides, when a run ends, whether it owes a read, and
// starts the worker when it does. Returns true when a debt is owed.
func grokUsageRunSettled(floor time.Time) bool {
	return grokSettleRun(floor, false)
}

// grokUsageRunSettledAsync settles off the caller's goroutine — an exit path
// that must not wait on the log read — counted so waiters see it in flight.
func grokUsageRunSettledAsync(floor time.Time) {
	if floor.IsZero() {
		return
	}
	grokFreshnessInFlight.Add(1)
	go func() {
		defer grokFreshnessInFlight.Add(-1)
		grokUsageRunSettled(floor)
	}()
}

// grokSettleRun is grokUsageRunSettled with the smoke's spacing bypass.
func grokSettleRun(floor time.Time, bypassInterval bool) bool {
	if floor.IsZero() {
		return false
	}
	now := grokUsageFreshnessNow()
	floorMs := floor.UnixMilli()
	completionMs := grokCompletionMs(now)
	if completionMs < floorMs {
		completionMs = floorMs
	}
	armedFingerprint, armed := grokLiveRunAccount(floorMs)
	oldest := grokReleaseLiveRun(floorMs)
	fingerprint := grokUsageCurrentFingerprint()
	if armed && armedFingerprint != fingerprint {
		// The run spent the account it was spawned under, which is no longer
		// signed in. The live read can only use the current login, so a debt
		// now would refresh — and spend budget on — the wrong account.
		updateGrokUsageFreshness(func(state *grokUsageFreshness) {
			grokDropSettledFloor(state, floorMs, oldest)
		})
		fmt.Printf("%s[cli-usage] grok refresh: skipped account_changed%s\n", colorYellow, colorReset)
		return false
	}
	if fingerprint == "" || grokObservationCovers(fingerprint, completionMs) {
		// Covered, or no account a read could be scoped to (the auth notice
		// owns that case).
		updateGrokUsageFreshness(func(state *grokUsageFreshness) {
			grokDropSettledFloor(state, floorMs, oldest)
		})
		return false
	}
	state := updateGrokUsageFreshness(func(state *grokUsageFreshness) {
		grokOweRead(state, now, completionMs, fingerprint)
		if state.RunFloorMs < floorMs {
			state.setRunFloor(floorMs, fingerprint)
		}
	})
	fmt.Printf("%s[cli-usage] grok refresh: owed attempts=%d%s\n", colorCyan, state.Attempts, colorReset)
	grokStartRunDebtWorker(bypassInterval)
	return true
}

// grokCompletionMs is a completion instant rounded UP to the millisecond.
// Readings are compared at millisecond precision with their start truncated,
// so a read that began earlier inside the same millisecond as a completion
// (or between two completions sharing one) must never compare as covering it:
// observedMs >= grokCompletionMs(t) holds only for a read started at or after t.
func grokCompletionMs(t time.Time) int64 {
	ms := t.UnixMilli()
	if t.Sub(time.UnixMilli(ms)) > 0 {
		ms++
	}
	return ms
}

// grokOweRead opens a debt for a reading at or after completionMs, or moves
// the pending one forward. The attempt count survives a newer run: only an
// `ok` (which clears the debt) or a cooled-off exhausted budget resets it.
func grokOweRead(state *grokUsageFreshness, now time.Time, completionMs int64, fingerprint string) {
	grokRebaseFutureFreshness(state, now)
	if state.owed() && (state.AccountFingerprint != fingerprint ||
		now.Sub(time.UnixMilli(state.OwedAtMs)) > grokRunDebtMaxAge) {
		state.clearDebt()
	}
	if state.owed() && state.Attempts >= grokRunDebtMaxAttempts && !grokDebtExhausted(*state, now) {
		state.clearDebt()
	}
	if !state.owed() {
		state.OwedAtMs = now.UnixMilli()
		state.AccountFingerprint = fingerprint
	}
	if completionMs > state.CompletionMs {
		state.CompletionMs = completionMs
	}
}

// settleGrokRunFreshness retires whatever a landed reading covers: the debt,
// when the reading was taken at or after its completion under the same
// account, and the restart marker when no run is still live.
func settleGrokRunFreshness(observedMs int64, fingerprint string) {
	if observedMs <= 0 {
		return
	}
	oldest := grokOldestLiveFloor()
	updateGrokUsageFreshness(func(state *grokUsageFreshness) {
		if state.owed() && state.AccountFingerprint == fingerprint && observedMs >= state.CompletionMs {
			state.clearDebt()
		}
		if !state.owed() && state.RunFloorMs != 0 && observedMs >= state.RunFloorMs {
			state.setRunFloor(oldest.floorMs, oldest.fingerprint)
		}
	})
}

/* ───────────────────────────────── pay ──────────────────────────────── */

type grokRunDebtRetryKind int

const (
	grokRetryNone      grokRunDebtRetryKind = iota
	grokRetryAfterRead                      // a read spent budget and failed
	grokRetryFree                           // a refusal that spent no budget
	grokRetrySpacing                        // deferred by grokRefreshMinInterval
)

// grokStartRunDebtWorker runs the payment under a process-wide single flight.
// A request that finds the flight held records a re-arm, so the running worker
// takes another pass instead of the new debt going unworked.
func grokStartRunDebtWorker(bypassInterval bool) {
	grokFreshnessInFlight.Add(1)
	grokRefreshWorkerMu.Lock()
	grokRefreshWorkerBypass = grokRefreshWorkerBypass || bypassInterval
	if grokRefreshWorkerRunning {
		grokRefreshWorkerRearm = true
		grokRefreshWorkerMu.Unlock()
		grokFreshnessInFlight.Add(-1)
		return
	}
	grokRefreshWorkerRunning, grokRefreshWorkerRearm = true, false
	grokRefreshWorkerMu.Unlock()
	go func() {
		defer grokFreshnessInFlight.Add(-1)
		for {
			grokRefreshWorkerMu.Lock()
			bypass := grokRefreshWorkerBypass
			grokRefreshWorkerBypass = false
			grokRefreshWorkerMu.Unlock()

			state, retry := grokPayRunDebtPass(bypass)
			if retry != grokRetryNone {
				grokScheduleRunDebtRetry(state, grokUsageFreshnessNow(), retry)
			}

			grokRefreshWorkerMu.Lock()
			if !grokRefreshWorkerRearm {
				grokRefreshWorkerRunning = false
				grokRefreshWorkerMu.Unlock()
				return
			}
			grokRefreshWorkerRearm = false
			grokRefreshWorkerMu.Unlock()
		}
	}()
}

// grokUsageRefreshWaitFor waits at most d for the worker (and any rung firing)
// to go idle, and reports whether it did.
func grokUsageRefreshWaitFor(d time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return grokUsageRefreshWaitIdle(ctx)
}

// grokUsageRefreshWaitIdle waits until nothing of this feature is in flight,
// or ctx ends; it reports whether it went idle.
func grokUsageRefreshWaitIdle(ctx context.Context) bool {
	for grokFreshnessInFlight.Load() != 0 {
		if ctx.Err() != nil {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

// grokPendingDebt returns the unpaid debt, rebasing a rolled-back clock and
// retiring one past grokRunDebtMaxAge.
func grokPendingDebt(now time.Time) (grokUsageFreshness, bool) {
	oldest := grokOldestLiveFloor()
	state := updateGrokUsageFreshness(func(state *grokUsageFreshness) {
		grokRebaseFutureFreshness(state, now)
		if state.owed() && now.Sub(time.UnixMilli(state.OwedAtMs)) > grokRunDebtMaxAge {
			state.clearDebt()
			state.setRunFloor(oldest.floorMs, oldest.fingerprint)
		}
	})
	return state, state.owed()
}

// grokRetireRunDebt drops the debt generation id for a case no retry can fix.
// A debt a newer run opened since id was inspected is left alone. The restart
// marker falls back to the oldest run still live (possibly under another
// account), so retiring an old debt never forgets a run in progress.
func grokRetireRunDebt(id grokDebtID, label string, attempts int) {
	oldest := grokOldestLiveFloor()
	updateGrokUsageFreshness(func(state *grokUsageFreshness) {
		if !state.owed() || state.debtID() != id {
			return
		}
		state.clearDebt()
		state.setRunFloor(oldest.floorMs, oldest.fingerprint)
	})
	grokLastRefreshOutcome.Store(label)
	fmt.Printf("%s[cli-usage] grok refresh: retired %s attempts=%d%s\n", colorYellow, label, attempts, colorReset)
}

// grokPayRunDebtPass spends at most one outbound read on the pending debt and
// says what the schedule should book next.
func grokPayRunDebtPass(bypassInterval bool) (grokUsageFreshness, grokRunDebtRetryKind) {
	now := grokUsageFreshnessNow()
	state, owed := grokPendingDebt(now)
	if !owed {
		return state, grokRetryNone
	}
	fingerprint := grokUsageCurrentFingerprint()
	if fingerprint == "" || fingerprint != state.AccountFingerprint {
		// Never paid against another login; the new one owes nothing yet.
		grokRetireRunDebt(state.debtID(), "account_changed", state.Attempts)
		return state, grokRetryNone
	}
	if grokObservationCovers(fingerprint, state.CompletionMs) {
		settleGrokRunFreshness(state.CompletionMs, fingerprint)
		grokLastRefreshOutcome.Store("covered")
		return state, grokRetryNone
	}
	if state.Attempts >= grokRunDebtMaxAttempts && !(bypassInterval && state.CompletionMs > state.LastAttemptAtMs) {
		// Kept until it cools off (grokDebtExhausted), so a burst of runs
		// during an outage cannot re-open the budget; nothing more is booked.
		// The one exception is a smoke that finished after the last read: it
		// gets a single read, so a transient refusal earlier cannot leave the
		// card stale after a green smoke. Smokes are rare and cooldown-bound,
		// so this cannot turn into a loop.
		grokLastRefreshOutcome.Store("exhausted")
		return state, grokRetryNone
	}
	if IsOffline() {
		grokLastRefreshOutcome.Store("offline")
		return state, grokRetryFree
	}
	if !bypassInterval && state.LastAttemptAtMs > 0 {
		if since := now.Sub(time.UnixMilli(state.LastAttemptAtMs)); since >= 0 && since < grokRefreshMinInterval {
			grokLastRefreshOutcome.Store("deferred")
			return state, grokRetrySpacing
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), grokRunDebtPassWait())
	outcome := grokBillingReadOnce(ctx, grokUsageRefreshGrokPath(), fingerprint, grokUsageFreshnessNow, time.UnixMilli(state.CompletionMs))
	cancel()
	if outcome == liveProbeOutcomeAccountChanged {
		// The login switched after the check above; the probe stopped before
		// presenting the new account's credential, so nothing was spent.
		grokRetireRunDebt(state.debtID(), "account_changed", state.Attempts)
		return state, grokRetryNone
	}
	if outcome == grokLiveOutcomeOK && !grokObservationCovers(fingerprint, state.CompletionMs) {
		// xAI answered with a period but no percentage: nothing for the card,
		// so the read pays nothing and the ladder tries again.
		outcome = grokRefreshOutcomeUnmetered
	}
	id := state.debtID()
	booked := updateGrokUsageFreshness(func(s *grokUsageFreshness) {
		s.LastAttemptAtMs = grokUsageFreshnessNow().UnixMilli()
		if s.debtID() != id {
			// Paid (an ok that landed through the shared flight) or replaced
			// by a newer run while this read was out: nothing to charge.
			return
		}
		s.LastOutcome = outcome
		if grokAuthOutcome(outcome) {
			// Idle until a new run settles: drop any rung an earlier failed read
			// booked, or a gather (or a restart) would find it due and read again.
			s.NextAttemptAtMs = 0
		}
		if grokOutcomeSpendsBudget(outcome) && s.Attempts < grokRunDebtMaxAttempts {
			s.Attempts++
		}
	})
	attempts := booked.Attempts
	if booked.debtID() != id {
		attempts = state.Attempts
	}
	grokLastRefreshOutcome.Store(outcome)
	fmt.Printf("%s[cli-usage] grok refresh: %s attempts=%d%s\n", colorCyan, outcome, attempts, colorReset)

	switch outcome {
	case grokLiveOutcomeOK:
		// grokBillingReadOnce already retired the debt through the settle.
		return booked, grokRetryNone
	case grokLiveOutcomeNoLogin, grokLiveOutcomeNoAccount, grokLiveOutcomeUnauthorized:
		// The auth notice covers these. Each one spends a single attempt and
		// books NO rung: the debt stays open but idle, so the gather's nudge
		// (which only starts a booked, due rung) cannot re-open it — a rejected
		// login would otherwise cost a billing GET, and for `unauthorized` a
		// `grok models` renewal, on every gather. The next run that settles
		// gets one read (a smoke even inside the spacing); a click clears it.
		// The rung an earlier failed read armed is cancelled with it.
		if booked.debtID() == id {
			stopGrokRunDebtRetry()
			// A start that arrived while this read was out — the old rung
			// firing, or a gather that saw its timestamp — asked to pay the
			// debt this refusal just idled. Taking it would land inside the
			// spacing and book a fresh rung, i.e. another read (and renewal)
			// with no new run. A new run's settle moves the debt id, so it
			// never reaches here; a smoke's bypass is kept for its pass.
			grokRefreshWorkerMu.Lock()
			if !grokRefreshWorkerBypass {
				grokRefreshWorkerRearm = false
			}
			grokRefreshWorkerMu.Unlock()
		}
		return booked, grokRetryNone
	case grokLiveOutcomeLoginBusy, grokLiveOutcomeWriteFailed, liveProbeOutcomeTimeout:
		// Local conditions, or this caller stopped waiting while the shared
		// flight goes on (its own ok still retires the debt): no budget spent.
		return booked, grokRetryFree
	default:
		return booked, grokRetryAfterRead
	}
}

// grokOutcomeSpendsBudget: a read that reached xAI and could not pay the
// debt. login_busy, write_failed and a caller timeout are local conditions
// and book free rungs.
func grokOutcomeSpendsBudget(outcome string) bool {
	switch outcome {
	case grokLiveOutcomeHTTPError, grokLiveOutcomeBadResponse, grokRefreshOutcomeUnmetered,
		grokLiveOutcomeNoLogin, grokLiveOutcomeNoAccount, grokLiveOutcomeUnauthorized:
		return true
	}
	return false
}

/* ────────────────────────────── schedule ────────────────────────────── */

// grokScheduleRunDebtRetry books the next rung for the debt generation state
// names, persists it and arms the one process-wide timer.
func grokScheduleRunDebtRetry(state grokUsageFreshness, now time.Time, kind grokRunDebtRetryKind) bool {
	if IsShutdownInProgress() {
		return false
	}
	id := state.debtID()
	var next time.Time
	booked := updateGrokUsageFreshness(func(s *grokUsageFreshness) {
		if !s.owed() || s.debtID() != id {
			return
		}
		delay, ok := refreshRetryDelayForAttempt(s.Attempts, grokRunDebtMaxAttempts, grokRunDebtRetryLadder)
		if !ok {
			s.NextAttemptAtMs = 0
			return
		}
		switch kind {
		case grokRetryFree:
			delay = refreshFreeRetryDelay(now.Sub(time.UnixMilli(s.OwedAtMs)), grokRunDebtFreeRetryDelay, grokRunDebtRetryLadder)
		case grokRetrySpacing:
			delay = grokRunDebtFreeRetryDelay
		}
		next = now.Add(delay)
		if s.LastAttemptAtMs > 0 {
			if spaced := time.UnixMilli(s.LastAttemptAtMs).Add(grokRefreshMinInterval); spaced.After(next) && spaced.Sub(now) <= grokRefreshMinInterval {
				next = spaced
			}
		}
		s.NextAttemptAtMs = next.UnixMilli()
	})
	if next.IsZero() {
		return false
	}
	grokArmRunDebtRetry(id, next.Sub(now))
	fmt.Printf("%s[cli-usage] grok refresh: scheduled attempts=%d%s\n", colorCyan, booked.Attempts, colorReset)
	return true
}

// grokArmRunDebtRetry replaces the pending timer with one for generation id.
func grokArmRunDebtRetry(id grokDebtID, delay time.Duration) {
	if delay < 0 {
		delay = 0
	}
	t := &grokRunDebtRetryTimer
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timer != nil {
		t.timer.Stop()
	}
	t.gen++
	gen := t.gen
	t.timer = time.AfterFunc(delay, func() { grokRunDebtRetryFired(gen, id) })
}

// grokRunDebtRetryFired re-reads the debt before doing anything, so a rung for
// a debt that was paid or replaced meanwhile is a no-op.
func grokRunDebtRetryFired(gen uint64, id grokDebtID) {
	t := &grokRunDebtRetryTimer
	t.mu.Lock()
	if gen != t.gen {
		t.mu.Unlock()
		return
	}
	t.timer = nil
	grokFreshnessInFlight.Add(1)
	t.mu.Unlock()
	defer grokFreshnessInFlight.Add(-1)
	if IsShutdownInProgress() {
		return
	}
	if state, owed := grokPendingDebt(grokUsageFreshnessNow()); !owed || state.debtID() != id {
		return
	}
	grokStartRunDebtWorker(false)
}

// stopGrokRunDebtRetry cancels the pending rung. Called from gracefulShutdown;
// the schedule is on disk and the next process re-arms it.
func stopGrokRunDebtRetry() {
	t := &grokRunDebtRetryTimer
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	t.gen++
}

// grokRunDebtRetryPending reports whether a rung is armed. Test seam.
func grokRunDebtRetryPending() bool {
	t := &grokRunDebtRetryTimer
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.timer != nil
}

/* ─────────────────────────── nudge & restart ────────────────────────── */

// nudgeGrokUsageRefresh is the gather's trigger, called when the log holds a
// record for this account newer than every reading (a direct run the agent did
// not spawn). It owes one read — or starts the worker for a pending debt whose
// rung is due — at most once per grokRefreshNudgeCooldown, and never while a
// run of this process is live (that run settles itself). Never blocks the
// gather.
func nudgeGrokUsageRefresh(now time.Time, fingerprint string) bool {
	if !grokUsageRefreshEnabled.Load() || fingerprint == "" || IsShutdownInProgress() || IsOffline() {
		return false
	}
	if grokOldestLiveFloor().floorMs != 0 {
		return false
	}
	n := &grokRefreshNudge
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.lastAt.IsZero() && !now.Before(n.lastAt) && now.Sub(n.lastAt) < grokRefreshNudgeCooldown {
		return false
	}
	start := false
	updateGrokUsageFreshness(func(state *grokUsageFreshness) {
		grokRebaseFutureFreshness(state, now)
		if state.owed() && state.AccountFingerprint == fingerprint &&
			now.Sub(time.UnixMilli(state.OwedAtMs)) <= grokRunDebtMaxAge {
			if state.Attempts < grokRunDebtMaxAttempts {
				// One debt at a time: only a booked rung that is due. Any other
				// is owned by its timer or by the worker already on it.
				start = state.NextAttemptAtMs != 0 && state.NextAttemptAtMs <= now.UnixMilli()
				return
			}
			if grokDebtExhausted(*state, now) {
				return
			}
		}
		grokOweRead(state, now, grokCompletionMs(now), fingerprint)
		start = true
	})
	if !start {
		return false
	}
	n.lastAt = now
	grokStartRunDebtWorker(false)
	return true
}

// payOwedGrokUsageRefresh resumes what the previous process left behind: a
// booked rung is re-armed for its remainder; a debt with no rung (a run that
// settled just before the hand-off) gets one attempt now; a floor no process
// settled is owed one read. Offline keeps the debt and sends nothing.
// Asynchronous — StartAgent never waits on it.
func payOwedGrokUsageRefresh() {
	startedAt := grokUsageFreshnessNow()
	grokFreshnessInFlight.Add(1)
	go func() {
		defer grokFreshnessInFlight.Add(-1)
		adoptAndPayOwedGrokRunDebt(startedAt)
	}()
}

func adoptAndPayOwedGrokRunDebt(startedAt time.Time) {
	now := grokUsageFreshnessNow()
	fingerprint := grokUsageCurrentFingerprint()
	state := updateGrokUsageFreshness(func(state *grokUsageFreshness) {
		floorBefore := state.RunFloorMs
		grokRebaseFutureFreshness(state, now)
		// A floor the rebase moved was armed before a clock step back, so by
		// the previous process: this one arms at its own now, never past the
		// skew ceiling. Without this the rebased floor (now) would read as
		// armed after startedAt and the interrupted run would go unpaid.
		inherited := state.RunFloorMs != floorBefore || state.RunFloorMs < startedAt.UnixMilli()
		if state.owed() || state.RunFloorMs == 0 || !inherited {
			return
		}
		// A floor with no debt beside it belongs to a run the previous process
		// was cut off in. It owes a reading taken from now on, but only under
		// the account it was spawned under: the read can use only the current
		// login, so a run armed under another one is dropped, never paid
		// against this one.
		if fingerprint == "" || fingerprint != state.RunFloorAccount ||
			now.Sub(time.UnixMilli(state.RunFloorMs)) > grokRunDebtMaxAge {
			state.setRunFloor(0, "")
			return
		}
		grokOweRead(state, now, grokCompletionMs(now), fingerprint)
	})
	if !state.owed() {
		return
	}
	if state.NextAttemptAtMs > now.UnixMilli() {
		grokArmRunDebtRetry(state.debtID(), time.UnixMilli(state.NextAttemptAtMs).Sub(now))
		fmt.Printf("%s[cli-usage] grok refresh: resumed attempts=%d%s\n", colorCyan, state.Attempts, colorReset)
		return
	}
	if state.NextAttemptAtMs == 0 && grokAuthOutcome(state.LastOutcome) {
		// Idle after an auth refusal, by design: a restart is not a new run,
		// so it must not spend another read (or `grok models` renewal) on a
		// login xAI just refused. The next run that settles reads. Keyed on the
		// debt's own LastOutcome — cleared with the debt — rather than the
		// spacing clock, which outlives debts and would also idle a fresh debt
		// a run settled just before the hand-off.
		return
	}
	grokStartRunDebtWorker(false)
}

// grokAuthOutcome reports the outcomes the auth notice owns: no read can pay
// them until the login changes.
func grokAuthOutcome(outcome string) bool {
	switch outcome {
	case grokLiveOutcomeNoLogin, grokLiveOutcomeNoAccount, grokLiveOutcomeUnauthorized:
		return true
	}
	return false
}

/* ──────────────────────────────── smoke ─────────────────────────────── */

// grokSmokeUsageSettleBudget bounds how long a passing smoke waits for its
// first read, so the signed refresh that follows the smoke result already
// finds the fresh number. It covers the WHOLE pass (grokRunDebtPassWait):
// outwaiting a Refresh flight sent just before the smoke completed, then its
// own read, login renewal included — on a device whose access token expired
// the read must run `grok models` first, and terminal-service only asks for
// Grok usage on its own wakes, so a reading that lands after the smoke result
// is not seen until the next one. The common case returns as soon as the read
// does; still bounded by the smoke's own context. A var for tests.
var grokSmokeUsageSettleBudget = grokRunDebtPassWait()

// settleOrDisarmGrokSmokeRun settles the smoke's run when a rung may have
// spent credits (grokSmokeRungMaySpend) — and then waits, bounded by
// grokSmokeUsageSettleBudget and ctx, for its first read — or disarms it when
// every rung failed before inference. Returns a closed label for the smoke's
// log line; the smoke verdict never depends on it. The caller arms the floor,
// so a zero floor (refresh disabled) never reaches here.
func settleOrDisarmGrokSmokeRun(ctx context.Context, floor time.Time, maySpend bool) string {
	if !maySpend {
		disarmGrokUsageRunFloor(floor)
		return "not_owed"
	}
	grokLastRefreshOutcome.Store("")
	if !grokSettleRun(floor, true) {
		return "covered"
	}
	waitCtx, cancel := context.WithTimeout(ctx, grokSmokeUsageSettleBudget)
	defer cancel()
	if !grokUsageRefreshWaitIdle(waitCtx) {
		// Still a debt on disk: the worker and its ladder keep paying it.
		return "pending"
	}
	if outcome, _ := grokLastRefreshOutcome.Load().(string); outcome != "" {
		return outcome
	}
	return "pending"
}

// drainGrokUsageWrites waits, bounded, for this feature's background work —
// an arm persisting its floor, a settle writing its debt — so an update
// hand-off does not exit before the debt the next process must adopt is on
// disk. Same budget rule as drainAntigravityUsageWrites (at most half of what
// is left of the shutdown deadline). A read still in flight past it is
// abandoned: its debt was written before the read started.
func drainGrokUsageWrites(ctx context.Context) {
	budget := antigravityShutdownDrainBudget(ctx, time.Now())
	if !grokUsageRefreshWaitFor(budget) {
		fmt.Printf("%s[cli-usage] grok refresh: shutdown drain timed out inFlight=%d%s\n",
			colorYellow, grokFreshnessInFlight.Load(), colorReset)
	}
}
