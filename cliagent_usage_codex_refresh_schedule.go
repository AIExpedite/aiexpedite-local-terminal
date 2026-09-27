// cliagent_usage_codex_refresh_schedule.go — the clock behind the Codex
// run-completion refresh debt.
//
// The debt worker (cliagent_usage_codex_freshness.go) used to have no clock of
// its own: two forced rollout reconciles about five seconds apart, one live
// `account/rateLimits/read`, then it retired. An empty result after an update —
// rollout not flushed yet, layout or cursor drift, IsOffline() during reconnect,
// or a live-read cooldown still hot from the pre-update gather — parked the debt
// until the age-out discarded it, and the CLI Agents card kept a day-old
// observedAt with no warning even though the smoke passed. This file gives a
// kept debt the same two ways back Antigravity got in
// cliagent_usage_antigravity_refresh_schedule.go:
//
//   - A bounded retry ladder. codexScheduleRunDebtRetry persists
//     NextAttemptAtMs beside the debt and arms ONE process-wide timer; the timer
//     runs a single attempt, and whatever that attempt keeps is booked again. A
//     restart or self-update re-enters the persisted rung
//     (payOwedCodexUsageRefresh) instead of burning or dropping it.
//   - A state-independent nudge from the gather. When the newest
//     account-eligible rollout postdates the cached reading,
//     nudgeCodexUsageRefresh creates the debt, so a run converges even when no
//     spawn path classified it (a `codex` the user started in their own shell).
//
// Cost: at most codexRefreshAfterRunMaxAttempts rollout scans (local I/O) plus
// codexRefreshLiveReadMaxAttempts outbound reads per unpaid run, on separately
// counted budgets, spaced by the ladder and by codexForcedReconcileMinInterval;
// rungs after a refusal that sent nothing are local checks and spend neither. A
// device with no unpaid run makes no request and arms no timer.
//
// Redaction: the persisted additions are numbers only (NextAttemptAtMs,
// RefreshLiveReads), and every log line below carries fixed labels, counters and
// durations — never a path, an account, a fingerprint or rollout text.
package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Vars rather than consts so tests can pin them small.
var (
	// codexRunDebtRetryLadder is the delay before the next attempt, by the
	// counter of the phase the debt is in (clamped at the last rung). The first
	// rung is much shorter than Antigravity's because Codex telemetry is
	// local-file-first: the usual reason a settle found nothing is that the
	// rollout had not been flushed yet. The tail is long enough to outlive an
	// agent self-update and a network reconnect.
	codexRunDebtRetryLadder = []time.Duration{
		15 * time.Second, time.Minute, 4 * time.Minute, 15 * time.Minute,
	}
	// codexRunDebtFreeRetryDelay is the short rung after a refusal that spent no
	// budget, where there is nothing to space from: as-is for a deferral caused
	// only by spacing (plus that spacing's remainder), and as the floor of the
	// age backoff for offline / shutdown / a disarmed gate.
	codexRunDebtFreeRetryDelay = 15 * time.Second
	// codexRefreshNudgeCooldown bounds how often a gather may nudge the worker,
	// on top of codexForcedReconcileMinInterval which the worker itself honours.
	codexRefreshNudgeCooldown = time.Minute
)

// codexRunDebtRetryTimer is the single pending retry. gen invalidates a timer
// that was replaced or stopped after it had already fired but before its
// callback took the lock.
var codexRunDebtRetryTimer struct {
	mu    sync.Mutex
	timer *time.Timer
	gen   uint64
}

// codexRefreshNudge is the per-process nudge cooldown.
var codexRefreshNudge struct {
	mu     sync.Mutex
	lastAt time.Time
}

// codexRunDebtRetryKind says what a payment pass left behind, and so which rung
// the next attempt waits for. Mirrors antigravityRunDebtRetryKind, with the
// extra phase Codex has: a rollout scan, which costs no network at all.
type codexRunDebtRetryKind int

const (
	// codexRetryNone: nothing to book — paid, retired, or out of budget on both
	// counters.
	codexRetryNone codexRunDebtRetryKind = iota
	// codexRetryAfterScan: a forced rollout reconcile ran and found nothing.
	// Indexes the ladder by RefreshOwedAttempts.
	codexRetryAfterScan
	// codexRetryAfterRead: a live read reached OpenAI and failed. Indexes the
	// ladder by RefreshLiveReads — a debt in the live-read phase asking for the
	// scan rung would be told the budget is spent and book nothing.
	codexRetryAfterRead
	// codexRetryFree: a refusal that sent nothing and may keep recurring
	// (offline, shutdown, a disarmed gate). Spends no budget, so its rung backs
	// off with the debt's age.
	codexRetryFree
	// codexRetrySpacing: deferred only because codexForcedReconcileMinInterval
	// or the live-read cooldown has not lapsed. One-off by construction, so it
	// waits the short rung plus that interval's remainder, never an age backoff.
	codexRetrySpacing
)

// codexRetryDelayForAttempt is the scan-phase ladder. Shared arithmetic:
// cliagent_usage_refresh_ladder.go.
func codexRetryDelayForAttempt(attempts int) (time.Duration, bool) {
	return refreshRetryDelayForAttempt(attempts, codexRefreshAfterRunMaxAttempts, codexRunDebtRetryLadder)
}

// codexRetryDelayForLiveRead is the live-read-phase ladder, indexed by the reads
// that actually reached OpenAI.
func codexRetryDelayForLiveRead(reads int) (time.Duration, bool) {
	return refreshRetryDelayForAttempt(reads, codexRefreshLiveReadMaxAttempts, codexRunDebtRetryLadder)
}

// codexFreeRetryDelay is the rung after a refusal that spent no budget, for a
// debt owed for `owedFor`: at least the free rung, at most the longest rung.
// Without the backoff a device that stays offline would re-check every 15 s for
// the whole 6 h age-out.
func codexFreeRetryDelay(owedFor time.Duration) time.Duration {
	return refreshFreeRetryDelay(owedFor, codexRunDebtFreeRetryDelay, codexRunDebtRetryLadder)
}

// codexRunDebtRetryHorizon is the furthest ahead a legitimately booked
// NextAttemptAtMs can sit: the longest delay the schedule can pick (a rung, the
// free rung, or the minimum interval it is pushed out to) plus the local skew
// the floors already tolerate. Anything further is a backwards clock step.
//
// It is NOT codexRunFloorLocalSkew: that constant is 30 s while legitimate rungs
// reach 15 m, so judging a rung against it would read every ordinary retry as a
// rollback, fire it immediately and collapse the ladder.
func codexRunDebtRetryHorizon() time.Duration {
	longest := codexRunDebtFreeRetryDelay
	if codexForcedReconcileMinInterval > longest {
		longest = codexForcedReconcileMinInterval
	}
	for _, rung := range codexRunDebtRetryLadder {
		if rung > longest {
			longest = rung
		}
	}
	return longest + codexRunFloorLocalSkew
}

// codexScheduleRunDebtRetry books the next attempt for the debt `state`
// describes under the account `fp`, persists it as NextAttemptAtMs and arms the
// process-wide timer — replacing any pending one, so concurrent settles cannot
// multiply attempts. `kind` picks the rung.
//
// The fingerprint is explicit because every freshness write is scoped to it and
// codexRunFreshnessState does not carry it.
//
// Returns false when nothing was booked: nothing to book, the debt is gone or
// was replaced by a newer generation (whose own pass books it), both budgets are
// spent, or the process is shutting down / the path is disarmed. In those last
// two cases the rung already on disk is left ALONE: clearing it would throw away
// the survival the field exists for, and the next process re-arms it.
func codexScheduleRunDebtRetry(fp string, state codexRunFreshnessState, now time.Time, kind codexRunDebtRetryKind) bool {
	if kind == codexRetryNone {
		return false
	}
	id := state.debtID()
	if fp == "" || !id.valid() {
		return false
	}
	if !codexUsageRefresh.isEnabled() || IsShutdownInProgress() {
		return false
	}
	var next time.Time
	var scans, reads int
	committed := codexRateLimitCacheTransaction(context.Background(), codexRateLimitCachePath(), now, true, func(snap *codexRateLimitSnapshot) bool {
		// Generation safety, the rule codexSetRefreshFallback and
		// codexRecordRefreshAttempt already follow: a credentials swap mid-wait,
		// a newer run's debt, or a floor withdrawn by codexUsageRunDisarmed all
		// mean this pass has nothing to book.
		if snap.AccountFingerprint != fp || snap.RefreshOwedAtMs != id.owedAtMs || snap.RunFloorMs != id.floorMs {
			return false
		}
		owedAt := time.UnixMilli(snap.RefreshOwedAtMs)
		if codexRunDebtExpired(snap.RefreshOwedAtMs, now) {
			// Over-age: the expiry path owns this debt, not the ladder.
			return false
		}
		delay, ok := time.Duration(0), false
		switch kind {
		case codexRetryAfterScan:
			delay, ok = codexRetryDelayForAttempt(snap.RefreshOwedAttempts)
		case codexRetryAfterRead:
			delay, ok = codexRetryDelayForLiveRead(snap.RefreshLiveReads)
		case codexRetryFree:
			// Spends neither budget, so no budget can bound it; the age-out
			// clamp below is what finally stops it.
			delay, ok = codexFreeRetryDelay(now.Sub(owedAt)), true
		case codexRetrySpacing:
			delay, ok = codexRunDebtFreeRetryDelay+codexRefreshSpacingRemaining(fp, now), true
		}
		if !ok {
			snap.NextAttemptAtMs = 0
			return true
		}
		next = now.Add(delay)
		// Never past the first instant the debt is expired, and never ON the
		// age-out boundary: codexRunFreshnessFromView keeps a debt owed while
		// now-owedAt <= maxAge, so a rung landing exactly there would fire, find
		// the debt still owed, spend a free attempt and re-book the same
		// already-due instant — an immediate-callback loop. Landing one
		// millisecond later makes the firing rung retire the debt instead.
		if deadline := codexRunDebtExpiryInstant(snap.RefreshOwedAtMs); next.After(deadline) {
			next = deadline
		}
		snap.NextAttemptAtMs = next.UnixMilli()
		scans, reads = snap.RefreshOwedAttempts, snap.RefreshLiveReads
		return true
	})
	if !committed {
		// The bounded cache locks REFUSED the write, so mutate never ran and there
		// is no rung on disk. Dropping it here is the exact failure this file
		// exists to fix, so arm one in this process anyway: its callback re-reads
		// the debt and books it properly. Nothing is persisted, so a restart
		// before then falls back to the startup replay.
		//
		// The free rung's age backoff, not a flat delay: nothing was spent, so no
		// budget bounds these retries, and a cache wedged for hours would
		// otherwise re-check (and log) every 15 s for the whole age-out.
		retry := codexFreeRetryDelay(now.Sub(state.owedAt))
		codexArmRunDebtRetry(id, fp, retry)
		fmt.Printf("%s[cli-usage] codex run refresh rung not persisted (cache busy); retrying in %ds%s\n",
			colorYellow, int(retry.Round(time.Second).Seconds()), colorReset)
		return true
	}
	if next.IsZero() {
		return false
	}
	delay := next.Sub(now)
	codexArmRunDebtRetry(id, fp, delay)
	fmt.Printf("%s[cli-usage] codex run refresh retry scheduled in %ds (scans=%d/%d reads=%d/%d)%s\n",
		colorCyan, int(delay.Round(time.Second).Seconds()),
		scans, codexRefreshAfterRunMaxAttempts, reads, codexRefreshLiveReadMaxAttempts, colorReset)
	return true
}

// codexRefreshSpacingRemaining is how long the two spacings a Codex attempt can
// defer on still hold it back: the per-account forced-reconcile interval and the
// shared live-read cooldown. The longer of the two, so the rung the attempt
// lands on can actually run.
func codexRefreshSpacingRemaining(fp string, now time.Time) time.Duration {
	remaining := codexUsageRefresh.intervalRemaining(fp, now)
	if cooldown := codexLiveRateLimitCooldownRemaining(fp, now); cooldown > remaining {
		remaining = cooldown
	}
	return remaining
}

// codexArmRunDebtRetry replaces the pending timer with one that fires after
// delay for the debt generation id under the account fp.
func codexArmRunDebtRetry(id codexDebtID, fp string, delay time.Duration) {
	if delay < 0 {
		delay = 0
	}
	t := &codexRunDebtRetryTimer
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timer != nil {
		t.timer.Stop()
	}
	t.gen++
	gen := t.gen
	t.timer = time.AfterFunc(delay, func() { codexRunDebtRetryFired(gen, id, fp) })
}

// codexRunDebtRetryFired is the timer's callback. It re-reads the debt before
// doing anything, so a rung that fires after another route already paid — or for
// a generation a newer run replaced, or an account the credentials moved on from
// — writes nothing. One attempt per firing: the ladder alone spaces them, and a
// firing that finds a worker already running RE-ARMS it (claimWorker /
// takeRearm) rather than starting a second, so the attempt budget cannot be
// spent twice.
func codexRunDebtRetryFired(gen uint64, id codexDebtID, fp string) {
	t := &codexRunDebtRetryTimer
	t.mu.Lock()
	if gen != t.gen {
		t.mu.Unlock()
		return
	}
	t.timer = nil
	// Counted as a tracked goroutine BEFORE the timer lock is released, so
	// stopCodexRunDebtRetry followed by waitIdle either cancels this callback or
	// waits it out — a shutdown (or a test's cleanup) never has it write after
	// its owner is gone.
	codexUsageRefresh.trackBegin()
	t.mu.Unlock()
	defer codexUsageRefresh.trackEnd()

	if IsShutdownInProgress() || !codexUsageRefresh.isEnabled() {
		return
	}
	base := codexHomeBase()
	if codexAccountFingerprintAtBase(base) != fp {
		return
	}
	now := codexUsageFreshnessNow()
	state := codexRunFreshnessForAccount(fp, now)
	if !state.owed {
		// An over-age debt reads as not owed. Nothing else would ever retire it,
		// so it would sit there with no marker and no warning — retire it here.
		codexRetireExpiredRunDebt(fp, now)
		return
	}
	if state.debtID() != id {
		return
	}
	codexRunDebtWorker(base, fp)
}

// stopCodexRunDebtRetry cancels the pending retry, if any. Called from
// gracefulShutdown so a rung cannot fire into a process that is exiting
// (including one handing off to an update), and from resetCodexUsageRefreshGate
// so a rung armed minutes ahead cannot fire into the next test's cache. The
// schedule itself is on disk and the next process re-arms it.
func stopCodexRunDebtRetry() {
	t := &codexRunDebtRetryTimer
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	t.gen++
}

// codexRunDebtRetryPending reports whether a retry timer is armed. Test seam;
// production never needs to ask.
func codexRunDebtRetryPending() bool {
	t := &codexRunDebtRetryTimer
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.timer != nil
}

// resetCodexRefreshNudge clears the per-process nudge cooldown. Test seam,
// called from resetCodexUsageRefreshGate.
func resetCodexRefreshNudge() {
	codexRefreshNudge.mu.Lock()
	codexRefreshNudge.lastAt = time.Time{}
	codexRefreshNudge.mu.Unlock()
}

// nudgeCodexUsageRefresh is the gather's trigger, the mirror of
// nudgeAntigravityUsageRefresh. It has two halves:
//
//   - With a debt pending it arms the worker only when the booked rung is DUE.
//     That debt's own timer owns every other rung.
//   - With no debt pending it CREATES one, inside a single cache transaction,
//     floored at `newestRollout`. Arming the worker alone would be a no-op: the
//     worker retires on owed == false.
//
// `newestRollout` is the newest ACCOUNT-ELIGIBLE rollout mtime the reconcile
// just stat-ed (codexReconcileFromRollout), never a walk of this function's own:
// the nudge runs every ~30 s, and an unbounded filepath.WalkDir there would be
// material past roughly 50 k files on a long-lived CODEX_HOME. Account
// eligibility is load-bearing rather than tidiness — a previous account's
// session keeps appending after a `codex login`, so its file's mtime advances
// while its telemetry belongs to the old account, and binding that to the live
// fingerprint would invent a debt the new account can never pay. When the
// reconcile did not run, or was cut short, the mtime is zero and this does
// nothing, which is correct: the reading did not change either.
//
// Bounds: nothing while shutting down or disarmed, nothing more than once per
// codexRefreshNudgeCooldown, nothing for a rollout younger than
// codexForcedReconcileMinInterval (a run still writing, or the gather's own
// refresh), and nothing while a run of this process is still open — that run
// settles itself. It never lowers RunFloorMs; a mtime is only ever an owed
// floor.
//
// Cost on the steady-state gather is ONE cache read and nothing else: the
// transaction below takes the blocking cross-process cache lock, and this runs
// every ~30 s, so a nudge with provably nothing to do returns before reaching it.
// The worker, when there is one, runs on its own goroutine.
func nudgeCodexUsageRefresh(base, fp string, now, newestRollout, observedAt time.Time) bool {
	if IsShutdownInProgress() || !codexUsageRefresh.isEnabled() || fp == "" {
		return false
	}
	n := &codexRefreshNudge
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.lastAt.IsZero() && !now.Before(n.lastAt) && now.Sub(n.lastAt) < codexRefreshNudgeCooldown {
		return false
	}
	// A rollout written since the newest observation, settled long enough ago
	// that it is not still being appended to.
	behind := !newestRollout.IsZero() && (observedAt.IsZero() || newestRollout.After(observedAt))
	settled := behind && now.Sub(newestRollout) >= codexForcedReconcileMinInterval
	// A run of this process that is still going settles itself when it ends.
	liveRun := codexNewestOpenRunFloor() != 0

	// Decided off the cheap cache READ: the transaction below takes the blocking
	// cross-process cache lock, and this runs every ~30 s.
	if !codexNudgeNeedsWrite(codexCacheViewForAccount(fp), now, settled, liveRun) {
		return false
	}

	start, created := false, false
	codexRateLimitCacheTransaction(context.Background(), codexRateLimitCachePath(), now, true, func(snap *codexRateLimitSnapshot) bool {
		if snap.AccountFingerprint != fp {
			return false
		}
		wrote := codexRebaseFutureRunFreshness(snap, now, now)
		if snap.RefreshOwedAtMs != 0 && codexRunDebtExpired(snap.RefreshOwedAtMs, now) {
			// Aged out. Left in place it would block every later rollout from
			// ever owing a refresh, so retire it — with its marker, so the card
			// still says why the figure is behind — and judge the rollout as if
			// no debt were pending.
			codexRetireRunDebtInSnapshot(snap, now)
			wrote = true
		}
		if snap.RefreshOwedAtMs != 0 {
			// Only a debt whose booked rung is due; its own timer (or a settle's
			// pass) owns every other one.
			if snap.NextAttemptAtMs == 0 || snap.NextAttemptAtMs > now.UnixMilli() {
				return wrote
			}
			start = true
			return wrote
		}
		// A floor THIS process armed and has not settled is the same live run seen
		// from the other side — the guard payOwedCodexUsageRefresh applies before
		// converting an "interrupted" floor. Without it a run whose manager is not
		// the global one (every test, and any future run source) would have a debt
		// nudged underneath it while it is still going.
		if !settled || liveRun || codexUsageRefresh.armedLocally(snap.RunFloorMs) {
			return wrote
		}
		// The observation the caller compared against may be stale by the time
		// this transaction runs; re-check inside it, where the contributors are
		// authoritative.
		if latest := codexLatestContributorObservation(snap.Contributors); !latest.Before(newestRollout) {
			return wrote
		}
		// codexOweRunRefresh opens the new generation itself (it clears the debt's
		// counters, fallback and rung first).
		codexOweRunRefresh(snap, newestRollout, now)
		start, created = true, true
		return true
	})
	// The cooldown is consumed whenever the transaction RAN, not only when it
	// armed the worker: a pass that took the lock and then declined — a local run
	// still open, an observation that moved on — would otherwise re-take it on
	// every gather for as long as that condition held.
	n.lastAt = now
	if !start {
		return false
	}
	if created {
		fmt.Printf("%s[cli-usage] codex telemetry was written after the cached reading with no refresh owed (behindBy=%s); refresh owed%s\n",
			colorYellow, codexBehindBy(observedAt, newestRollout), colorReset)
	}
	codexUsageRefresh.spawn(func() { codexRunDebtWorker(base, fp) })
	return true
}

// codexNudgeNeedsWrite reports whether a nudge could do anything at all, from
// the cheap cache read alone. It mirrors exactly what the transaction would
// decide, so it changes no behaviour — it only keeps the blocking
// cross-process cache lock off the steady-state gather.
//
// That matters most for the case the obvious guard misses: a debt PENDING on the
// ladder with its next rung still in the future. Its own timer owns that rung, so
// the nudge has nothing to do, yet without this it would take the lock on every
// gather for as long as the debt lives — up to codexRefreshOwedMaxAge.
func codexNudgeNeedsWrite(view codexCacheView, now time.Time, settled, liveRun bool) bool {
	// A clock rollback left state dated ahead; the rebase inside is the repair.
	if codexRunFreshnessInFuture(view, now) {
		return true
	}
	if view.refreshOwedAtMs == 0 {
		// Nothing owed: only a settled run of someone else's that nothing recorded.
		return settled && !liveRun
	}
	// Over-age: retire it and leave the marker, so a newer rollout is not blocked
	// behind it forever.
	if codexRunDebtExpired(view.refreshOwedAtMs, now) {
		return true
	}
	// Otherwise only a booked rung that is DUE arms the worker. A missing rung is
	// the transient between a payment pass returning and the worker booking what
	// it kept, and belongs to that worker.
	return view.nextAttemptAtMs != 0 && view.nextAttemptAtMs <= now.UnixMilli()
}

// codexBehindBy renders how far a rollout write postdates the reading, for the
// nudge's log line; "never" when nothing was ever observed. Durations only.
func codexBehindBy(observedAt, newestRollout time.Time) string {
	if observedAt.IsZero() {
		return "never"
	}
	return newestRollout.Sub(observedAt).Round(time.Second).String()
}
