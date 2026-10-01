// cliagent_usage_claudecode_refresh_schedule.go — the clock behind the Claude
// Code run-refresh debt.
//
// The debt (cliagent_usage_claudecode_freshness.go) used to have no clock of its
// own: the in-process trailing probe made one or two attempts, and whatever it
// left standing — a cold Windows TLS handshake timing out, a 5xx, a deferral too
// far out to sleep through — sat on disk until some backend-initiated gather
// happened to pay it, or the next start retired it. A passing `__cli_smoke__`
// could therefore leave the CLI Agents card on a pre-run reading well past the
// Refresh window. This file gives a kept debt the bounded retry ladder Codex
// (cliagent_usage_codex_refresh_schedule.go) and Antigravity
// (cliagent_usage_antigravity_refresh_schedule.go) already have:
//
//   - claudeScheduleRunDebtRetry persists NextAttemptAtMs beside the debt and
//     arms ONE process-wide timer. The timer runs a single attempt through the
//     startup replay's own path (payOwedClaudeUsageRefreshAt), under every
//     existing gate — single flight, the cross-process dedupe, a 429 hold,
//     offline, the opt-out — and whatever that attempt keeps is booked again.
//   - A restart or self-update re-arms the persisted rung: StartAgent's replay
//     honours a NextAttemptAtMs still in the future instead of attempting early.
//
// Cost: at most claudeRefreshDebtMaxAttempts outbound requests per unpaid debt
// — the first trailing attempt plus one per rung — all inside the 30-minute
// debt age-out. Runs coalesce into one debt, so this is per account per debt,
// not per run. Rungs after a refusal that sent nothing (offline, no stored
// token, a deferral) spend no budget. A persisted 429 hold short-circuits every
// rung, so the worst case on a busy shared account is the hold, never a storm.
//
// Redaction: the only persisted additions are integers (NextAttemptAtMs, and
// LastProbeWeeklyObservedAtMs on the metrics side), and the log lines carry a
// closed-set label and integers — never a token, path, fingerprint or response
// body.
package main

import (
	"sync"
	"time"
)

// claudeRefreshDebtMaxAttempts is the request budget of one durable debt: the
// in-process trailing attempt plus one per ladder rung. Spent by the charges in
// claudeUsageProbeChargedAttempt and payOwedClaudeUsageRefreshAt, enforced under
// the cache lock by adjustClaudeRefreshAttemptsAt, and the cap
// claudeRefreshDebtRetired retires a debt at.
const claudeRefreshDebtMaxAttempts = 4

// Vars rather than consts so tests can pin them small.
var (
	// claudeRunDebtRetryLadder is the delay before the next attempt, by the
	// attempts already charged (refreshRetryDelayForAttempt): 60 s after the
	// first, 2 m after the second, 5 m after the third, then the budget is
	// spent — about eight minutes end to end. The first rung equals
	// claudeUsageProbeMinInterval on purpose, so it cannot fire into the
	// interval and defer; the gate's failure backoff after 1, 2 and 3 failures
	// (60 s, 120 s, 240 s) never outlasts the rung it follows.
	claudeRunDebtRetryLadder = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute}
	// claudeRunDebtFreeRetryDelay is the short rung after a refusal that spent
	// no budget: as-is (plus the interval's remainder) for a deferral, and as the
	// floor of the age backoff for offline or no stored token.
	claudeRunDebtFreeRetryDelay = 15 * time.Second
)

// claudeRunDebtRetryTimer is the single pending retry. gen invalidates a timer
// that was replaced or stopped after it had already fired but before its
// callback took the lock.
var claudeRunDebtRetryTimer struct {
	mu    sync.Mutex
	timer *time.Timer
	gen   uint64
}

// claudeRunDebtRetryKind says what an attempt left behind for the schedule, and
// so which rung the next attempt waits for. Modelled on
// antigravityRunDebtRetryKind.
type claudeRunDebtRetryKind int

const (
	// claudeRetryNone: nothing to book — paid, or another holder owns the debt.
	claudeRetryNone claudeRunDebtRetryKind = iota
	// claudeRetryAfterRequest: a request went out and failed (timeout, 5xx,
	// 401, parse failure) or landed a reading that does not cover the run. The
	// ladder rung for the attempts charged so far.
	claudeRetryAfterRequest
	// claudeRetrySpacing: refused by the minimum interval, the failure backoff
	// or the single flight. Waits the free rung plus what is left of the
	// interval, never an age backoff.
	claudeRetrySpacing
	// claudeRetryHeld: a 429 is on record. Waits until the hold lifts, or
	// retires when that is past the age-out.
	claudeRetryHeld
	// claudeRetryFree: offline, or no stored token. Spends no budget and may
	// keep recurring, so it backs off with the debt's age.
	claudeRetryFree
)

// The closed set of Claude refresh outcome labels. Every refresh outcome the
// debt path reaches is logged as one of these (logClaudeUsageRefreshOutcome),
// so the next stale-card report says which gap it was.
const (
	claudeRefreshOutcomeOK           = "ok"
	claudeRefreshOutcomeHeld         = "held"
	claudeRefreshOutcomeTimeout      = "timeout"
	claudeRefreshOutcomeUnauthorized = "unauthorized"
	claudeRefreshOutcomeHTTPError    = "http_error"
	claudeRefreshOutcomeParseFailed  = "parse_failed"
	claudeRefreshOutcomeNotCovering  = "not_covering"
	claudeRefreshOutcomeRefused      = "refused"
	claudeRefreshOutcomeScheduled    = "scheduled"
	claudeRefreshOutcomeRetired      = "retired"
)

// claudeRefreshOutcomeKnown reports whether label is in the closed set.
func claudeRefreshOutcomeKnown(label string) bool {
	switch label {
	case claudeRefreshOutcomeOK, claudeRefreshOutcomeHeld, claudeRefreshOutcomeTimeout,
		claudeRefreshOutcomeUnauthorized, claudeRefreshOutcomeHTTPError, claudeRefreshOutcomeParseFailed,
		claudeRefreshOutcomeNotCovering, claudeRefreshOutcomeRefused, claudeRefreshOutcomeScheduled,
		claudeRefreshOutcomeRetired:
		return true
	}
	return false
}

// claudeRefreshOutcomeForError maps a probe failure onto the closed label set.
// A 429 carries claudeRefreshOutcomeHeld in its local-only Message; any other
// non-2xx, transport failure, rejected override or unwritable cache is an
// http_error.
func claudeRefreshOutcomeForError(probeErr *cliAgentUsageError) string {
	if probeErr == nil {
		return claudeRefreshOutcomeOK
	}
	switch {
	case probeErr.Message == claudeRefreshOutcomeHeld:
		return claudeRefreshOutcomeHeld
	case probeErr.ErrorCategory == cliUsageErrorProviderTimeout:
		return claudeRefreshOutcomeTimeout
	case probeErr.ErrorCategory == cliUsageErrorNotAuthenticated:
		return claudeRefreshOutcomeUnauthorized
	case probeErr.ErrorCategory == cliUsageErrorParseFailed:
		return claudeRefreshOutcomeParseFailed
	}
	return claudeRefreshOutcomeHTTPError
}

// claudeRunDebtRetryKindForRefusal classifies an attempt begin() did not admit:
// a live hold, offline (no budget, may recur), or a deferral (interval or the
// single flight).
func claudeRunDebtRetryKindForRefusal(now time.Time) claudeRunDebtRetryKind {
	if _, held := claudeUsageProbe.heldAt(now); held {
		return claudeRetryHeld
	}
	if IsOffline() {
		return claudeRetryFree
	}
	return claudeRetrySpacing
}

// claudeRetryDelayForAttempt is the ladder: the delay before the next attempt of
// a debt that has already charged `attempts`, or false once the budget is spent.
// Shared arithmetic: cliagent_usage_refresh_ladder.go.
func claudeRetryDelayForAttempt(attempts int) (time.Duration, bool) {
	return refreshRetryDelayForAttempt(attempts, claudeRefreshDebtMaxAttempts, claudeRunDebtRetryLadder)
}

// claudeFreeRetryDelay is the rung after a refusal that spent no budget, for a
// debt owed for `owedFor`: at least the free rung, at most the longest rung.
// Shared arithmetic: cliagent_usage_refresh_ladder.go.
func claudeFreeRetryDelay(owedFor time.Duration) time.Duration {
	return refreshFreeRetryDelay(owedFor, claudeRunDebtFreeRetryDelay, claudeRunDebtRetryLadder)
}

// claudeRunDebtRetryAt is the instant the next attempt at the debt `snap`
// carries is due, or false when the debt should retire instead: past paying
// (claudeRefreshDebtRetired), out of budget, or due only after the age-out.
//
// gateNext is when this process's gate would next admit a probe (the interval,
// the failure backoff and any in-memory hold); a rung never lands before it, or
// it would fire, be refused and book itself again.
func claudeRunDebtRetryAt(kind claudeRunDebtRetryKind, snap *claudeRateLimitSnapshot, owed, gateNext, now time.Time) (time.Time, bool) {
	if claudeRefreshDebtRetired(owed, snap.RefreshOwedAttempts, now) {
		return time.Time{}, false
	}
	var next time.Time
	switch kind {
	case claudeRetryAfterRequest:
		delay, ok := claudeRetryDelayForAttempt(snap.RefreshOwedAttempts)
		if !ok {
			return time.Time{}, false
		}
		next = now.Add(delay)
	case claudeRetrySpacing:
		next = now.Add(claudeRunDebtFreeRetryDelay)
		if gateNext.After(now) {
			next = next.Add(gateNext.Sub(now))
		}
	case claudeRetryHeld:
		// The hold itself is applied below, from whichever record is later.
		next = now
	default: // claudeRetryFree
		next = now.Add(claudeFreeRetryDelay(now.Sub(owed)))
	}
	if gateNext.After(next) {
		next = gateNext
	}
	// A persisted hold another process recorded binds this process too, within
	// the same skew ceiling every other reader applies.
	if snap.HeldUntilMs > next.UnixMilli() && snap.HeldUntilMs <= claudeHoldSkewCeilingRebased(now) {
		next = time.UnixMilli(snap.HeldUntilMs)
	}
	next = next.Add(claudeUsageProbeTrailingSlack)
	// A rung past the age-out would only find the debt retired. Retire it now
	// instead of holding a timer for it.
	if next.After(owed.Add(claudeRefreshOwedMaxAge)) {
		return time.Time{}, false
	}
	return next, true
}

// claudeScheduleRunDebtRetry books the next attempt at the debt recorded at
// `owed` under `fingerprint`, persists it as NextAttemptAtMs and arms the
// process-wide timer — replacing any pending one, so a burst of settlements
// cannot multiply attempts. `kind` picks the rung.
//
// The write is matched to the debt instant, so a pass that lost the race to a
// newer run never books a rung for that newer debt (whose own settlement books
// it), and it never re-scopes the cache (mutateClaudeRefreshDebt). A debt the
// rung cannot reach is retired in the same write.
//
// When the cache locks refuse the write, the rung is armed in this process
// anyway at the free delay: its callback re-reads the debt and books properly,
// and a restart before then falls back to the startup replay.
//
// Returns whether a timer was armed.
func claudeScheduleRunDebtRetry(fingerprint string, owed time.Time, kind claudeRunDebtRetryKind, now time.Time) bool {
	if kind == claudeRetryNone || owed.IsZero() || IsShutdownInProgress() || !claudeUsageProbe.armedForProbe() {
		return false
	}
	gateNext, _ := claudeUsageProbe.nextEligible(now)
	owedMs := owed.UnixMilli()
	var next time.Time
	ran, retired := false, false
	attempts := 0
	mutateClaudeRefreshDebt(claudeRateLimitCachePath(), fingerprint, func(snap *claudeRateLimitSnapshot) bool {
		ran = true
		if snap.RefreshOwedAtMs != owedMs {
			return false // paid, retired or replaced since — not ours to book
		}
		attempts = snap.RefreshOwedAttempts
		at, ok := claudeRunDebtRetryAt(kind, snap, owed, gateNext, now)
		if !ok {
			retired = true
			return clearClaudeRefreshDebt(snap)
		}
		next = at
		snap.NextAttemptAtMs = at.UnixMilli()
		return true
	})
	if retired {
		logClaudeUsageRefreshOutcome(claudeRefreshOutcomeRetired, attempts)
		return false
	}
	if !ran {
		next = now.Add(claudeFreeRetryDelay(now.Sub(owed)))
	}
	if next.IsZero() {
		return false
	}
	claudeArmRunDebtRetry(next.Sub(now))
	logClaudeUsageRefreshOutcome(claudeRefreshOutcomeScheduled, attempts)
	return true
}

// claudeArmRunDebtRetry replaces the pending timer with one that fires after
// delay.
func claudeArmRunDebtRetry(delay time.Duration) {
	if delay < 0 {
		delay = 0
	}
	t := &claudeRunDebtRetryTimer
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timer != nil {
		t.timer.Stop()
	}
	t.gen++
	gen := t.gen
	t.timer = time.AfterFunc(delay, func() { claudeRunDebtRetryFired(gen) })
}

// claudeRunDebtRetryFired is the timer's callback: one attempt through the
// startup replay's path, which re-reads the debt first — so a rung that fires
// after a Refresh click or a gather already paid it, or for a debt a newer run
// replaced, sends nothing — and books whatever it keeps.
func claudeRunDebtRetryFired(gen uint64) {
	t := &claudeRunDebtRetryTimer
	t.mu.Lock()
	if gen != t.gen {
		t.mu.Unlock()
		return
	}
	t.timer = nil
	// Counted under the timer lock, so stopClaudeRunDebtRetry followed by
	// resetClaudeUsageProbeGate's drain either cancels this callback or waits it
	// out — a shutdown (or a test's cleanup) never has it write after its owner
	// is gone.
	claudeUsageProbe.beginSettling()
	t.mu.Unlock()
	defer claudeUsageProbe.endSettling()
	defer func() { _ = recover() }()

	// Unarmed means a reset or shutdown is under way, not an opt-out — the
	// replay would read it as one and clear the debt. Leave it for the next start.
	if IsShutdownInProgress() || !claudeUsageProbe.armedForProbe() {
		return
	}
	// A rung that outlived its debt — paid by a Refresh click, a gather or
	// another channel — ends here, on one unlocked file read, before the replay
	// resolves the credential (a `security` spawn on macOS) to learn the same.
	if snap, ok := loadClaudeRateLimitSnapshot(claudeRateLimitCachePath()); !ok || snap.RefreshOwedAtMs == 0 {
		return
	}
	payOwedClaudeUsageRefreshAt(time.Now())
}

// stopClaudeRunDebtRetry cancels the pending retry, if any. Called from
// gracefulShutdown so a rung cannot fire into a process that is exiting
// (including one handing off to an update), and from resetClaudeUsageProbeGate.
// The schedule itself is on disk and the next process re-arms it.
func stopClaudeRunDebtRetry() {
	t := &claudeRunDebtRetryTimer
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	t.gen++
}

// claudeRunDebtRetryPending reports whether a retry timer is armed. Test seam;
// production never needs to ask.
func claudeRunDebtRetryPending() bool {
	t := &claudeRunDebtRetryTimer
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.timer != nil
}
