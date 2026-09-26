// cliagent_usage_antigravity_refresh_schedule.go — the clock behind the
// Antigravity run-completion refresh debt.
//
// The debt worker (cliagent_usage_antigravity_freshness.go) used to have no
// clock of its own: a minimum-interval deferral, an offline agent, an expired
// stored login and a transient miss all left the debt on disk with nothing
// scheduled to come back to it, and the age-out then discarded it unpaid. A
// maintenance pass that ran `agy` twice inside a minute therefore ended green
// with a day-old reading. This file gives a kept debt two ways back:
//
//   - A bounded retry ladder. antigravityScheduleRunDebtRetry persists
//     NextAttemptAtMs beside the debt and arms ONE process-wide timer; the
//     timer runs a single attempt, and whatever that attempt keeps is
//     scheduled again. A restart or self-update re-arms the persisted
//     schedule (adoptAndPayOwedAntigravityRunDebt).
//   - A state-independent nudge from the gather. When the CLI's newest run log
//     postdates the cached reading, nudgeAntigravityUsageRefresh arms the same
//     worker, so a run converges even when no spawn path classified it (a run
//     the user started in their own shell).
//
// Cost: at most antigravityRefreshDebtMaxAttempts outbound Code Assist reads per
// unpaid run, spaced by the ladder and by antigravityRefreshMinInterval; rungs
// after a refusal that sent nothing (offline, expired login) are local checks
// and spend no budget. A device with no unpaid run makes no request at all.
//
// Redaction: the only persisted addition is one epoch-millisecond int, and the
// log lines below carry fixed labels, counters and durations — never a path, an
// account, a port or log text.
package main

import (
	"fmt"
	"sync"
	"time"
)

// Vars rather than consts so tests can pin them small.
var (
	// antigravityRunDebtRetryLadder is the delay before the next attempt, by
	// the number of attempts already booked (clamped at the last rung). The
	// first rung equals antigravityRefreshMinInterval on purpose: a shorter one
	// would fire, hit the interval, defer and reschedule itself for the rest.
	antigravityRunDebtRetryLadder = []time.Duration{
		time.Minute, 2 * time.Minute, 8 * time.Minute, 30 * time.Minute,
	}
	// antigravityRunDebtFreeRetryDelay is the short rung after a refusal that
	// spent no outbound read, where there is no read to space from: as-is for a
	// minimum-interval deferral (antigravityRetrySpacing), and as the floor of
	// the age backoff for offline or an expired stored login
	// (antigravityFreeRetryDelay). The minimum interval still applies to the
	// attempt it leads to.
	antigravityRunDebtFreeRetryDelay = 30 * time.Second
	// antigravityRefreshNudgeCooldown bounds how often a gather may arm the
	// worker, on top of the minimum interval the worker itself honours.
	antigravityRefreshNudgeCooldown = time.Minute
)

// antigravityRunDebtRetryTimer is the single pending retry. gen invalidates a
// timer that was replaced or stopped after it had already fired but before its
// callback took the lock.
var antigravityRunDebtRetryTimer struct {
	mu    sync.Mutex
	timer *time.Timer
	gen   uint64
}

// antigravityRefreshNudge is the per-process nudge cooldown.
var antigravityRefreshNudge struct {
	mu     sync.Mutex
	lastAt time.Time
}

// antigravityRunDebtRetryKind says what a payment pass left behind for the
// schedule, and so which rung the next attempt waits for.
type antigravityRunDebtRetryKind int

const (
	// antigravityRetryNone: nothing to book — paid, retired, terminal
	// (no_login) or out of budget.
	antigravityRetryNone antigravityRunDebtRetryKind = iota
	// antigravityRetryAfterRead: a read reached Google and failed; the ladder
	// rung for the attempts booked so far.
	antigravityRetryAfterRead
	// antigravityRetryFree: a refusal that sent nothing and may keep
	// recurring (offline, an expired stored login). Spends no budget, so its
	// rung backs off with the debt's age (antigravityFreeRetryDelay).
	antigravityRetryFree
	// antigravityRetrySpacing: deferred only because the minimum interval
	// since the last outbound read has not lapsed. One-off by construction —
	// once it lapses the next attempt cannot defer on it again — so it waits
	// just the short rung and the interval's remainder, never an age backoff.
	antigravityRetrySpacing
)

// antigravityRetryDelayForAttempt is the ladder: the delay before the next
// attempt of a debt that has already booked `attempts`, or false once the
// debt's budget is spent.
func antigravityRetryDelayForAttempt(attempts int) (time.Duration, bool) {
	if attempts >= antigravityRefreshDebtMaxAttempts || len(antigravityRunDebtRetryLadder) == 0 {
		return 0, false
	}
	rung := attempts - 1
	if rung < 0 {
		rung = 0
	}
	if rung >= len(antigravityRunDebtRetryLadder) {
		rung = len(antigravityRunDebtRetryLadder) - 1
	}
	return antigravityRunDebtRetryLadder[rung], true
}

// antigravityFreeRetryDelay is the rung after a refusal that spent no outbound
// read, for a debt owed for `owedFor`. Such refusals spend no budget, so the
// budget cannot bound them; the delay grows with the debt's age instead (at
// least the free rung, at most the longest rung). Without that, a device that
// stays offline, or whose stored login stays expired, would re-check every 30 s
// for the whole age-out — hundreds of log lines, and on macOS/Linux hundreds of
// `security` / `secret-tool` keyring children — where this costs a couple of
// dozen checks across six hours.
func antigravityFreeRetryDelay(owedFor time.Duration) time.Duration {
	delay := antigravityRunDebtFreeRetryDelay
	if owedFor > delay {
		delay = owedFor
	}
	if n := len(antigravityRunDebtRetryLadder); n > 0 && delay > antigravityRunDebtRetryLadder[n-1] {
		delay = antigravityRunDebtRetryLadder[n-1]
	}
	return delay
}

// antigravityRunDebtRetryHorizon is the furthest ahead a legitimately booked
// NextAttemptAtMs can sit: the longest delay the schedule can pick (a rung, or
// the minimum interval it is pushed out to), plus the local skew the floors
// already tolerate. Anything further is a backwards clock step.
func antigravityRunDebtRetryHorizon() time.Duration {
	longest := antigravityRunDebtFreeRetryDelay
	if antigravityRefreshMinInterval > longest {
		longest = antigravityRefreshMinInterval
	}
	for _, rung := range antigravityRunDebtRetryLadder {
		if rung > longest {
			longest = rung
		}
	}
	return longest + antigravityRunFloorLocalSkew
}

// antigravityScheduleRunDebtRetry books the next attempt for the debt state
// names, persists it as NextAttemptAtMs and arms the process-wide timer —
// replacing any pending one, so concurrent settles cannot multiply attempts.
// kind picks the rung (antigravityRunDebtRetryKind).
// Returns false when nothing was booked: the debt is gone or was replaced by a
// newer generation (whose own pass books it), its budget is spent (it keeps its
// Outcome until the age-out so the notice can explain it), or the process is
// shutting down.
func antigravityScheduleRunDebtRetry(state antigravityUsageFreshness, now time.Time, kind antigravityRunDebtRetryKind) bool {
	if IsShutdownInProgress() {
		return false
	}
	id := state.debtID()
	var next time.Time
	booked := updateAntigravityUsageFreshness(func(state *antigravityUsageFreshness) {
		if state.RefreshOwedAtMs == 0 || state.debtID() != id {
			return
		}
		delay, ok := antigravityRetryDelayForAttempt(state.Attempts)
		if !ok {
			state.NextAttemptAtMs = 0
			return
		}
		switch kind {
		case antigravityRetryFree:
			delay = antigravityFreeRetryDelay(now.Sub(time.UnixMilli(state.RefreshOwedAtMs)))
		case antigravityRetrySpacing:
			delay = antigravityRunDebtFreeRetryDelay
		}
		next = now.Add(delay)
		// Never earlier than the minimum interval allows, or the attempt would
		// only defer and reschedule itself. A spacing clock from the future is
		// a clock step and spaces nothing.
		if state.LastPaidAtMs > 0 {
			spaced := time.UnixMilli(state.LastPaidAtMs).Add(antigravityRefreshMinInterval)
			if spaced.After(next) && spaced.Sub(now) <= antigravityRefreshMinInterval {
				next = spaced
			}
		}
		state.NextAttemptAtMs = next.UnixMilli()
	})
	if next.IsZero() {
		return false
	}
	delay := next.Sub(now)
	antigravityArmRunDebtRetry(id, delay)
	fmt.Printf("%s[antigravity-freshness] Run refresh retry scheduled in %ds (attempts=%d/%d)%s\n",
		colorCyan, int(delay.Round(time.Second).Seconds()), booked.Attempts, antigravityRefreshDebtMaxAttempts, colorReset)
	return true
}

// antigravityArmRunDebtRetry replaces the pending timer with one that fires
// after delay for the debt generation id.
func antigravityArmRunDebtRetry(id antigravityDebtID, delay time.Duration) {
	if delay < 0 {
		delay = 0
	}
	t := &antigravityRunDebtRetryTimer
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timer != nil {
		t.timer.Stop()
	}
	t.gen++
	gen := t.gen
	t.timer = time.AfterFunc(delay, func() { antigravityRunDebtRetryFired(gen, id) })
}

// antigravityRunDebtRetryFired is the timer's callback. It re-reads the debt
// before doing anything, so a rung that fires after another route already paid
// — or for a generation a newer run replaced — writes nothing; the same rule
// the settle goroutine follows. One attempt per firing: the ladder alone spaces
// them.
func antigravityRunDebtRetryFired(gen uint64, id antigravityDebtID) {
	t := &antigravityRunDebtRetryTimer
	t.mu.Lock()
	if gen != t.gen {
		t.mu.Unlock()
		return
	}
	t.timer = nil
	// Counted under the lock, so stopAntigravityRunDebtRetry followed by
	// antigravityUsageRefreshWaitIdle either cancels this callback or waits it
	// out — a shutdown (or a test's cleanup) never has it write after its
	// owner is gone.
	antigravityFreshnessInFlight.Add(1)
	t.mu.Unlock()
	defer antigravityFreshnessInFlight.Add(-1)

	if IsShutdownInProgress() {
		return
	}
	if state, owed := antigravityPendingDebt(antigravityUsageFreshnessNow()); !owed || state.debtID() != id {
		return
	}
	antigravityStartRunDebtWorker(1, false)
}

// stopAntigravityRunDebtRetry cancels the pending retry, if any. Called from
// gracefulShutdown so a rung cannot fire into a process that is exiting; the
// schedule itself is on disk and the next process re-arms it.
func stopAntigravityRunDebtRetry() {
	t := &antigravityRunDebtRetryTimer
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	t.gen++
}

// antigravityRunDebtRetryPending reports whether a retry timer is armed. Test
// seam; production never needs to ask.
func antigravityRunDebtRetryPending() bool {
	t := &antigravityRunDebtRetryTimer
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.timer != nil
}

// nudgeAntigravityUsageRefresh is the gather's trigger. It arms the debt worker
// when a pending debt's booked attempt is due, or — with no debt pending —
// creates one floored at the newest run log when that log postdates the cached
// reading (antigravityRunBehindObservation with no slack). That second half is
// what makes a run converge when no spawn path classified it.
//
// Bounds: nothing while the minimum interval since the last outbound read
// (including a Refresh click's) is running, nothing more than once per
// antigravityRefreshNudgeCooldown, and nothing for a log younger than the
// minimum interval — that settle guard stands in for the slack the diagnostic
// keeps, so a run still writing its log, or a Refresh click's own `agy`, is not
// mistaken for a missed one. A nudge never touches a debt that already exists
// (one pending debt at a time) and never lowers RunFloorMs; a log mtime is only
// ever an owed floor, and the debt it creates is bounded by the age-out from
// the moment it was created.
//
// Never blocks the gather: one small state-file read-modify-write, and the
// worker runs on its own goroutine.
func nudgeAntigravityUsageRefresh(now time.Time, observedAt string, newestLog time.Time) bool {
	if IsShutdownInProgress() || IsOffline() {
		return false
	}
	n := &antigravityRefreshNudge
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.lastAt.IsZero() && !now.Before(n.lastAt) && now.Sub(n.lastAt) < antigravityRefreshNudgeCooldown {
		return false
	}

	// Never observed at all is behind any run; otherwise the shared predicate.
	// The card's observedAt is whole seconds, and the debt worker reads within
	// a second of a run ending, so the log is compared at the same resolution:
	// a log from the reading's own second is the run that reading covered, not
	// a missed one.
	behind := observedAt == "" && !newestLog.IsZero()
	if observedAt != "" {
		_, behind = antigravityRunBehindObservation(observedAt, newestLog.Truncate(time.Second), 0)
	}
	settled := behind && now.Sub(newestLog) >= antigravityRefreshMinInterval
	// A run of this process that is still going settles itself when it ends.
	liveRun := antigravityOldestLiveRunFloorMs() != 0

	start, created := false, false
	updateAntigravityUsageFreshness(func(state *antigravityUsageFreshness) {
		antigravityRebaseFutureFreshness(state, now)
		if state.LastPaidAtMs > 0 {
			if since := now.Sub(time.UnixMilli(state.LastPaidAtMs)); since >= 0 && since < antigravityRefreshMinInterval {
				return
			}
		}
		if state.RefreshOwedAtMs != 0 && now.Sub(time.UnixMilli(state.RefreshOwedAtMs)) > antigravityRefreshOwedMaxAge {
			// Aged out. A terminal debt (no_login, an exhausted ladder) has no
			// timer to retire it, and left in place it would block every later
			// run log from ever owing a refresh — so retire it here and judge
			// the newest log as if no debt were pending.
			state.clearDebt()
		}
		if state.RefreshOwedAtMs != 0 {
			// Only a debt whose booked attempt is due; its own timer (or the
			// settle's pass) owns every other one.
			if state.Attempts >= antigravityRefreshDebtMaxAttempts ||
				state.NextAttemptAtMs == 0 || state.NextAttemptAtMs > now.UnixMilli() {
				return
			}
			start = true
			return
		}
		if !settled || liveRun {
			return
		}
		state.clearDebt()
		state.RefreshOwedFloorMs, state.RefreshOwedAtMs = newestLog.UnixMilli(), now.UnixMilli()
		_, state.Gated = antigravityQuotaGateFor("", now)
		start, created = true, true
	})
	if !start {
		return false
	}
	n.lastAt = now
	if created {
		fmt.Printf("%s[antigravity-freshness] A run finished after the cached reading with no refresh owed (behindBy=%s); refresh owed%s\n",
			colorYellow, antigravityBehindBy(observedAt, newestLog), colorReset)
	}
	antigravityStartRunDebtWorker(1, false)
	return true
}

// antigravityBehindBy renders how far a run log postdates the reading, for the
// nudge's log line; "never" when nothing was ever observed.
func antigravityBehindBy(observedAt string, newestLog time.Time) string {
	observed, err := time.Parse(time.RFC3339, observedAt)
	if err != nil {
		return "never"
	}
	return newestLog.Sub(observed).Round(time.Second).String()
}
