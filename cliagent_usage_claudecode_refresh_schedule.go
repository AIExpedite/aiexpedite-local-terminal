// cliagent_usage_claudecode_refresh_schedule.go — the clock behind the Claude
// Code run-completion refresh debt.
//
// After an under-quota headless run (direct chat, terminal-managed, a
// `__cli_smoke__` turn, or any run the `SessionEnd` hook observed —
// claude_run_end_hook.go) the OAuth usage probe is the ONLY numeric source, so a
// run's debt has to reach that endpoint. It used to get one immediate attempt,
// a trailing wait of at most five minutes held in a goroutine, and one startup
// replay — and a pre-update 429 hold, an expired stored token, or a probe that
// lost the single-flight race left the debt with nothing scheduled until it
// aged out. The CLI Agents card then showed "usage unobservable" beside a
// passing smoke. This file gives a kept debt the ladder Codex and Antigravity
// already walk (cliagent_usage_codex_refresh_schedule.go):
//
//   - claudeBookRunDebtRung persists NextAttemptAtMs beside the debt and arms
//     ONE process-wide timer; the timer runs claudeRunDebtAttemptAt, and the
//     attempt's result (claudeProbeResult) books the next rung. A restart or
//     self-update re-arms the persisted rung (payOwedClaudeUsageRefresh)
//     instead of spending it early or dropping it.
//   - The rung comes from the result: a refusal that sent nothing waits for
//     the thing that refused it (the 60 s floor, the 429 hold, a credential
//     rewrite) or backs off with the debt's age; a request that failed walks
//     the budgeted ladder.
//   - nudgeClaudeCredentialChanged makes the rung due at once when Claude Code
//     rewrites a credential the schedule was waiting on.
//
// Cost: at most claudeRefreshOwedMaxRequests endpoint requests per unpaid run,
// counted by one persisted counter (RefreshOwedAttempts) that every automatic
// path reserves from durably before it sends; rungs after a refusal are local
// checks and spend nothing. A device with no unpaid run makes no request and
// arms no timer.
//
// Redaction: the persisted additions are numbers only (NextAttemptAtMs and the
// credential stamp), and every log line carries a fixed code, counters and
// durations — never a token, a path, an account or a response body.
package main

import (
	"fmt"
	"sync"
	"time"
)

// Vars rather than consts so tests can pin them small.
var (
	// claudeRunDebtRetryLadder is the delay before the next BUDGETED attempt,
	// indexed by the requests the debt has already cost (clamped at the last
	// rung). Its longest rung is also the ceiling of the free-rung age backoff.
	claudeRunDebtRetryLadder = []time.Duration{
		15 * time.Second, time.Minute, 4 * time.Minute, 15 * time.Minute,
	}
	// claudeRunDebtFreeRetryDelay is the floor of the age backoff after a
	// refusal that spent no budget and may keep recurring (offline, no or
	// expired credential, a refused reservation).
	claudeRunDebtFreeRetryDelay = 15 * time.Second
	// claudeRunDebtRungSlack pads a rung booked at a known instant — the end of
	// the 60 s floor, of a 429 hold — so a timer firing a hair early is not
	// refused and re-booked.
	claudeRunDebtRungSlack = time.Second
)

// claudeRunDebtTrigger names what started an attempt. The timer, the startup
// replay and an observed run (a debt the SessionEnd hook wrote, adopted by the
// usage tick) honour a rung booked in the future; a finished run and a
// credential nudge are reasons to try NOW (the gate still spaces them). An
// observed debt is written due, so honouring the rung only matters when the
// tick adopts a debt whose rung was already booked — spending it early there
// would collapse the ladder.
type claudeRunDebtTrigger int

const (
	claudeDebtTriggerRun claudeRunDebtTrigger = iota
	claudeDebtTriggerTimer
	claudeDebtTriggerStartup
	claudeDebtTriggerNudge
	claudeDebtTriggerObserved
)

func (t claudeRunDebtTrigger) honoursRung() bool {
	return t == claudeDebtTriggerTimer || t == claudeDebtTriggerStartup || t == claudeDebtTriggerObserved
}

// claudeRunDebtRungKind says how the next rung's delay is derived.
type claudeRunDebtRungKind int

const (
	// claudeRungNone: nothing to book — paid, retired, opted out, or cancelled.
	claudeRungNone claudeRunDebtRungKind = iota
	// claudeRungBudgeted: a request was issued and did not pay the debt. The
	// ladder indexed by RefreshOwedAttempts; the debt retires at the cap.
	claudeRungBudgeted
	// claudeRungFree: a refusal that spent nothing and may recur. Backs off with
	// the debt's age; the age-out is what finally stops it.
	claudeRungFree
	// claudeRungAfter: a refusal that ends at a known instant (spacing, a hold,
	// a probe in flight). The caller supplies the delay.
	claudeRungAfter
)

// claudeRunDebtRetryTimer is the single pending retry. gen invalidates a timer
// that was replaced or stopped after it had already fired but before its
// callback took the lock.
var claudeRunDebtRetryTimer struct {
	mu    sync.Mutex
	timer *time.Timer
	gen   uint64
}

// claudeFreeRetryDelay is the free rung for a debt owed for `owedFor`. Shared
// arithmetic: cliagent_usage_refresh_ladder.go.
func claudeFreeRetryDelay(owedFor time.Duration) time.Duration {
	return refreshFreeRetryDelay(owedFor, claudeRunDebtFreeRetryDelay, claudeRunDebtRetryLadder)
}

// claudeUnpersistedRetryDelay is the in-process retry for a debt or rung whose
// write the cache locks refused. A FRESH run usually lost a lock race to the
// stream capture of its own turn, so it retries as soon as a probe could go out
// anyway (the floor's end, or the rung slack) rather than on the free rung's
// 15 s floor — a burst of turns must not delay its refresh past the window the
// old trailing probe covered. Past that first window the age backoff applies,
// so a cache wedged for hours is re-checked a couple of dozen times, not every
// second.
func claudeUnpersistedRetryDelay(owedFor time.Duration) time.Duration {
	retry := claudeFreeRetryDelay(owedFor)
	if owedFor >= claudeRunDebtFreeRetryDelay {
		return retry
	}
	short := claudeRunDebtRungSlack
	if kind, d := claudeRunDebtRungFor(claudeProbeResult{code: claudeUsageProbe.refusal(time.Now(), false)}); kind == claudeRungAfter {
		short = d
	}
	if short < retry {
		retry = short
	}
	return retry
}

// claudeRunDebtRetryHorizon is the furthest ahead a legitimately booked rung
// can sit: a maximum-length 429 hold plus its slack, or the longest ladder
// rung, plus the skew the debt itself tolerates. Anything further is a
// backwards clock step, and is treated as due rather than waited out.
func claudeRunDebtRetryHorizon() time.Duration {
	longest := claudeUsageProbeMaxRetryAfter + claudeRunDebtRungSlack
	if n := len(claudeRunDebtRetryLadder); n > 0 && claudeRunDebtRetryLadder[n-1] > longest {
		longest = claudeRunDebtRetryLadder[n-1]
	}
	return longest + claudeRefreshOwedLocalSkew
}

// claudeRunDebtRungFor maps one attempt result onto the rung it books — the
// contract's table, in one place. Gate instants are measured against the wall
// clock, which is the clock the gate stamped them with.
func claudeRunDebtRungFor(result claudeProbeResult) (claudeRunDebtRungKind, time.Duration) {
	spacingEnd, heldUntil := claudeUsageProbe.spacingEnd()
	wall := time.Now()
	untilHold := time.Duration(0)
	if heldUntil.After(wall) {
		untilHold = heldUntil.Sub(wall) + claudeRunDebtRungSlack
	}
	if !result.issued {
		switch result.code {
		case claudeProbeUnarmed, claudeProbeOptedOut, claudeProbeCancelled:
			return claudeRungNone, 0
		case claudeProbeInFlight:
			// The holder's reading did not cover the debt (or the race left no
			// time to join it): try once the floor it just charged has passed.
			return claudeRungAfter, claudeUsageProbeMinIntervalValue() + claudeRunDebtRungSlack
		case claudeProbeSpacing:
			wait := spacingEnd.Sub(wall)
			if wait < 0 {
				wait = 0
			}
			return claudeRungAfter, wait + claudeRunDebtRungSlack
		case claudeProbeHeld:
			if untilHold > 0 {
				return claudeRungAfter, untilHold
			}
		}
		// offline, no_credential, credential_expired (the nudge brings a
		// credential rewrite forward), bad_override, reserve_failed, and any
		// refusal the contract does not name.
		return claudeRungFree, 0
	}
	switch result.code {
	case claudeProbeHTTP429:
		// The hold already spaces the next attempt; the rung lands at its end.
		if untilHold > 0 {
			return claudeRungAfter, untilHold
		}
	case claudeProbeHTTP401:
		// With a credential file to watch, wait for Claude Code to rewrite it,
		// with the age backoff as the fallback. Without one (Keychain), the
		// failure backoff and the budget bound it like any other failure.
		if result.credStamped {
			return claudeRungFree, 0
		}
	}
	return claudeRungBudgeted, 0
}

// claudeBookRunDebtRungFor books the rung `result` calls for. See
// claudeBookRunDebtRung.
func claudeBookRunDebtRungFor(fp string, owed, now time.Time, result claudeProbeResult) bool {
	kind, delay := claudeRunDebtRungFor(result)
	return claudeBookRunDebtRung(fp, owed, now, kind, delay)
}

// claudeBookRunDebtRung persists the next rung for the debt `owed` under the
// account `fp` as NextAttemptAtMs and arms the process-wide timer — replacing
// any pending one, so concurrent attempts cannot multiply rungs. The credential
// stamp the gate is waiting on (if any) is persisted in the same write, so a
// restart keeps waiting on it rather than re-sending the same token.
//
// Returns false when nothing was booked: nothing to book, the debt is gone or
// was replaced by a newer run (whose own attempt books it), the budget is spent
// (the debt is retired), or the process is shutting down / disarmed — in which
// case a rung already on disk is left alone for the next process to re-arm.
//
// A write the cache locks refuse leaves no rung on disk; one is armed in this
// process anyway, on the free rung's age backoff, because a dropped rung is the
// failure this file exists to fix. Nothing persisted means a restart falls back
// to the startup replay, which treats the debt as due.
func claudeBookRunDebtRung(fp string, owed, now time.Time, kind claudeRunDebtRungKind, delay time.Duration) bool {
	if kind == claudeRungNone || owed.IsZero() {
		return false
	}
	if IsShutdownInProgress() || !claudeUsageProbe.armedForProbe() {
		return false
	}
	var wait claudeCredStamp
	pendingClear, _ := claudeUsageProbe.pendingAuthClear()
	owedMs := owed.UnixMilli()
	var next time.Time
	attempts := 0
	rejected, retired := false, false
	committed := mutateClaudeRateLimitSnapshot(claudeRateLimitCachePath(), fp, func(snap *claudeRateLimitSnapshot) bool {
		if snap.RefreshOwedAtMs != owedMs {
			rejected = true
			return false
		}
		switch kind {
		case claudeRungBudgeted:
			rung, ok := refreshRetryDelayForAttempt(snap.RefreshOwedAttempts, claudeRefreshOwedMaxRequests, claudeRunDebtRetryLadder)
			if !ok {
				// Budget spent: retire through the same clear the startup replay
				// uses. Only the debt fields go — the buckets and the hold stay.
				retired = true
				return clearClaudeRefreshDebt(snap)
			}
			delay = rung
			// The rung has to CLEAR the failure backoff the request that just
			// failed may have grown, or it fires into a spacing refusal.
			if spacingEnd, _ := claudeUsageProbe.spacingEnd(); !spacingEnd.IsZero() {
				if backoff := time.Until(spacingEnd) + claudeRunDebtRungSlack; backoff > delay {
					delay = backoff
				}
			}
		case claudeRungFree:
			delay = claudeFreeRetryDelay(now.Sub(owed))
		}
		if delay < 0 {
			delay = 0
		}
		next = now.Add(delay)
		// Never past the first instant the debt is retired: a rung landing there
		// retires it instead of booking another.
		if deadline := owed.Add(claudeRefreshOwedMaxAge + time.Millisecond); next.After(deadline) {
			next = deadline
		}
		snap.NextAttemptAtMs = next.UnixMilli()
		// Sampled under the cache lock, not before it: a concurrent 2xx that
		// cleared the wait in memory before this write found nothing on disk to
		// clear, so persisting the earlier sample would resurrect a wait on a
		// credential just proven valid. One that clears after this write finds
		// the stamp on disk and clears it (or queues the clear).
		wait = claudeUsageProbe.authWaitStamp()
		snap.AuthWaitCredStampNs, snap.AuthWaitCredSize = wait.modNs, wait.size
		attempts = snap.RefreshOwedAttempts
		return true
	})
	if committed && !retired {
		claudeUsageProbe.markAuthWaitPersisted(wait)
		// The booking wrote this process's wait over the one a pending clear
		// was owed for, which settles it.
		if pendingClear != wait {
			claudeUsageProbe.settlePendingAuthClear(pendingClear)
		}
	}
	if retired {
		claudeUsageProbe.dropOwedThrough(owed)
		fmt.Printf("%s[claude-usage] run refresh retired: request budget spent (%d/%d)%s\n",
			colorYellow, claudeRefreshOwedMaxRequests, claudeRefreshOwedMaxRequests, colorReset)
		return false
	}
	if rejected {
		return false
	}
	if !committed {
		retry := claudeUnpersistedRetryDelay(now.Sub(owed))
		claudeArmRunDebtRetry(retry, claudeDebtTriggerTimer)
		fmt.Printf("%s[claude-usage] run refresh rung not persisted (cache busy); retrying in %ds%s\n",
			colorYellow, int(retry.Round(time.Second).Seconds()), colorReset)
		return true
	}
	until := next.Sub(now)
	claudeArmRunDebtRetry(until, claudeDebtTriggerTimer)
	fmt.Printf("%s[claude-usage] run refresh retry scheduled in %ds (requests=%d/%d)%s\n",
		colorCyan, int(until.Round(time.Second).Seconds()), attempts, claudeRefreshOwedMaxRequests, colorReset)
	return true
}

// claudeArmRunDebtRetry replaces the pending timer with one that fires after
// delay and runs one attempt as `trigger`.
func claudeArmRunDebtRetry(delay time.Duration, trigger claudeRunDebtTrigger) {
	claudeArmRunDebtRetryFn(delay, func() { claudeRunDebtAttemptAt(time.Now(), trigger) })
}

// claudeArmRunDebtReowe arms the timer for a run whose debt never reached disk
// (the cache locks refused claudeOweRunRefresh): when it fires it owes the run
// again and, if that lands, makes the run's attempt.
func claudeArmRunDebtReowe(completedAt time.Time, delay time.Duration) {
	claudeArmRunDebtRetryFn(delay, func() { claudeUsageProbePayRecordedRun(completedAt) })
}

func claudeArmRunDebtRetryFn(delay time.Duration, run func()) {
	if delay < 0 {
		delay = 0
	}
	t := &claudeRunDebtRetryTimer
	t.mu.Lock()
	defer t.mu.Unlock()
	claudeArmRunDebtRetryLocked(delay, run)
}

// claudeArmRunDebtRetryLocked replaces the pending retry with `run` after
// `delay`. The caller holds claudeRunDebtRetryTimer.mu.
func claudeArmRunDebtRetryLocked(delay time.Duration, run func()) {
	t := &claudeRunDebtRetryTimer
	if t.timer != nil {
		t.timer.Stop()
	}
	t.gen++
	gen := t.gen
	t.timer = time.AfterFunc(delay, func() { claudeRunDebtRetryFired(gen, run) })
}

// claudeRunDebtRetryFired is the timer's callback: one attempt per firing, and
// nothing at all for a generation that was replaced or stopped. The attempt
// re-reads the debt from disk, so a rung that fires after another route paid
// it writes nothing.
func claudeRunDebtRetryFired(gen uint64, run func()) {
	t := &claudeRunDebtRetryTimer
	t.mu.Lock()
	if gen != t.gen {
		t.mu.Unlock()
		return
	}
	t.timer = nil
	// Counted as settling BEFORE the timer lock is released, so a reset's
	// stopClaudeRunDebtRetry followed by its drain either cancels this callback
	// or waits it out — it never writes into the next test's cache.
	claudeUsageProbe.beginSettling()
	t.mu.Unlock()
	defer claudeUsageProbe.endSettling()
	defer func() { _ = recover() }()
	if IsShutdownInProgress() || !claudeUsageProbe.armedForProbe() {
		return
	}
	run()
}

// stopClaudeRunDebtRetry cancels the pending retry, if any. Called from
// gracefulShutdown so a rung cannot fire into a process that is exiting
// (including one handing off to an update), and from resetClaudeUsageProbeGate.
// A booked rung is on disk and the next process re-arms it; a re-owe rung's
// debt is not, so gracefulShutdown follows this with
// persistClaudeRunDebtForShutdown.
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

// persistClaudeRunDebtForShutdown is gracefulShutdown's last chance for a run
// whose debt never reached disk: the cache locks refused claudeOweRunRefresh,
// so the only thing that would have recovered it is the in-process re-owe rung
// stopClaudeRunDebtRetry just cancelled. One synchronous owe of the gate's
// in-memory debt puts RefreshOwedAtMs on disk for the next process to re-arm.
// It is a no-op for a debt already on disk (the owe never lowers or rewrites
// an unchanged baseline), and sends nothing over the network.
//
// A proven credential's durable clear the cache refused is the same kind of
// in-memory-only state: its background retry stands down at shutdown, and the
// next process would restore the rejected wait from disk. It gets one
// synchronous retry here too.
func persistClaudeRunDebtForShutdown() {
	if !claudeRetryPendingAuthClear() {
		fmt.Printf("%s[claude-usage] credential wait clear not persisted at shutdown (cache busy)%s\n",
			colorYellow, colorReset)
	}
	owed := claudeUsageProbe.owedObservation()
	if owed.IsZero() || time.Since(owed) >= claudeRefreshOwedMaxAge {
		return
	}
	if _, onDisk := claudeOweRunRefresh(owed); !onDisk {
		fmt.Printf("%s[claude-usage] run refresh debt not persisted at shutdown (cache busy)%s\n",
			colorYellow, colorReset)
	}
}

// claudeRunDebtRetryPending reports whether a retry timer is armed.
func claudeRunDebtRetryPending() bool {
	t := &claudeRunDebtRetryTimer
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.timer != nil
}

// nudgeClaudeCredentialChanged is the gather's trigger. When the schedule is
// waiting on a credential rewrite (an expired token, or a 401) and the file
// the gather just read carries a DIFFERENT stamp, the pending rung is made due
// at once — still subject to single flight, the 60 s floor and a hold. One
// nudge per new stamp, not one per gather: the gate remembers it.
//
// The nudge is claimed only while a rung is pending, and the claim and the
// re-arm happen under the timer lock. With no timer (an attempt is still
// finishing and has not booked its next rung yet) the stamp stays unclaimed,
// so the first gather after that rung is booked still makes it due.
//
// No stamp (a Keychain login) never nudges; the expiry pre-check re-reads
// expiresAt on every attempt there, and the age backoff covers the rest.
func nudgeClaudeCredentialChanged(stamp claudeCredStamp) {
	if stamp.isZero() {
		return
	}
	t := &claudeRunDebtRetryTimer
	t.mu.Lock()
	defer t.mu.Unlock()
	// Lock order timer → gate, as in claudeRunDebtRetryFired.
	if t.timer == nil || !claudeUsageProbe.takeCredentialNudge(stamp) {
		return
	}
	claudeArmRunDebtRetryLocked(0, func() { claudeRunDebtAttemptAt(time.Now(), claudeDebtTriggerNudge) })
}

// claudeUsageRefreshForceReason picks how a __cli_usage_refresh__ forces
// Claude's probe: `click` for the signed live-probe arg, `debt` for an
// automatic refresh while a run's refresh is owed, and none otherwise — so an
// automatic refresh with nothing owed follows the staleness TTL.
func claudeUsageRefreshForceReason(cmd commandMsg) claudeForceReason {
	if cliUsageRefreshWantsLiveProbe(cmd) {
		return claudeForceClick
	}
	if !claudeUsageProbe.owedObservation().IsZero() {
		return claudeForceDebt
	}
	return claudeForceNone
}

/* ───────────────────────── gate: credential wait ─────────────────────────── */

// awaitingCredentialChange reports whether `stamp` is the very credential an
// expired token or a 401 was seen under. A zero stamp never matches: with no
// file to watch there is nothing to wait for.
func (g *claudeUsageProbeGate) awaitingCredentialChange(stamp claudeCredStamp) bool {
	if stamp.isZero() {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.authWait == stamp
}

// noteAuthWait records the credential stamp to wait on. A zero stamp records
// nothing. A new stamp re-opens the nudge.
//
// The wait is this process's own evidence from here on, even when the stamp is
// the one restored from disk: a 401 seen now is newer than a peer's on-disk
// clear that a later seed may still read, and that clear must not drop it.
func (g *claudeUsageProbeGate) noteAuthWait(stamp claudeCredStamp) {
	if stamp.isZero() {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.authWait != stamp {
		g.authWait, g.authWaitNudged = stamp, claudeCredStamp{}
	}
	g.authWaitRestored = claudeCredStamp{}
	g.authWaitSeq++
	// A fresh rejection of the credential an earlier 2xx proved supersedes that
	// proof's pending durable clear.
	if g.authClearPending == stamp {
		g.authClearPending, g.authClearFingerprint = claudeCredStamp{}, ""
	}
}

// notePendingAuthClear records a proven credential whose persisted wait could
// not be cleared, and reports whether the caller should start the retry loop
// (false when one is already running) along with that loop's generation.
func (g *claudeUsageProbeGate) notePendingAuthClear(fingerprint string, stamp claudeCredStamp) (uint64, bool) {
	if stamp.isZero() {
		return 0, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.authClearPending, g.authClearFingerprint = stamp, fingerprint
	if g.authClearRetrying {
		return 0, false
	}
	g.authClearRetrying = true
	g.authClearGen++
	return g.authClearGen, true
}

// authClearRetryCurrent reports whether the retry loop of generation `gen` is
// still the live one.
func (g *claudeUsageProbeGate) authClearRetryCurrent(gen uint64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.authClearRetrying && g.authClearGen == gen
}

// pendingAuthClear returns the proven credential whose durable clear is still
// owed, zero for none, and the account it was proved under.
func (g *claudeUsageProbeGate) pendingAuthClear() (claudeCredStamp, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.authClearPending, g.authClearFingerprint
}

// settlePendingAuthClear drops the pending clear for `stamp`, leaving a newer
// one recorded since alone.
func (g *claudeUsageProbeGate) settlePendingAuthClear(stamp claudeCredStamp) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.authClearPending == stamp {
		g.authClearPending, g.authClearFingerprint = claudeCredStamp{}, ""
	}
}

// endPendingAuthClearRetry marks the background retry loop of generation
// `gen` as finished; a loop a reset already replaced changes nothing.
func (g *claudeUsageProbeGate) endPendingAuthClearRetry(gen uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.authClearGen == gen {
		g.authClearRetrying = false
	}
}

// authWaitReadSeq is taken BEFORE reading the persisted wait and handed to
// restoreAuthWait with what was read, so a read that started before a local
// change to the wait (a 401 noted, the wait persisted, a success clearing it)
// cannot be applied over it.
func (g *claudeUsageProbeGate) authWaitReadSeq() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.authWaitSeq
}

// markAuthWaitPersisted records that the local wait on `stamp` reached disk.
// From then on the on-disk copy is this wait, so a later read finding it
// cleared — a peer probed successfully with that credential after this write —
// drops it, as it would a restored one. A wait replaced since the booking read
// it is left alone.
func (g *claudeUsageProbeGate) markAuthWaitPersisted(stamp claudeCredStamp) {
	if stamp.isZero() {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.authWait == stamp {
		g.authWaitRestored = stamp
		g.authWaitSeq++
	}
}

// restoreAuthWait mirrors the credential wait persisted in the cache. A
// non-zero stamp is recorded as noteAuthWait does and remembered as restored. A
// zero one means the persisted wait was cleared — another process's probe
// succeeded with that credential — so the in-memory wait is dropped too, but
// only while it is still the one restored from disk: a wait this process
// recorded itself (a 401 never persisted without a debt, or not persisted yet)
// is newer evidence and stands.
//
// The same rule holds for a non-zero stamp: it replaces the in-memory wait only
// when there is none, when the wait is still the one restored before, or when
// the persisted wait is on a strictly newer credential write. A gather that
// read the cache before a concurrent 401 on a rewritten credential recorded its
// stamp must not put the older stamp back — the failing attempt would persist
// it, and every later automatic attempt would then mistake the still-rejected
// credential for a changed one and resend it until the budget ran out.
//
// readSeq is authWaitReadSeq taken before the read. A read that started before
// a local change to the wait is dropped whole: it cannot say anything newer
// than what this process just recorded. Restoring the stamp a local wait
// already holds leaves that wait local until the wait is persisted
// (markAuthWaitPersisted), so a read of a peer's on-disk clear made before this
// process wrote the wait cannot drop it, while one made after can.
func (g *claudeUsageProbeGate) restoreAuthWait(stamp claudeCredStamp, readSeq uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if readSeq != g.authWaitSeq {
		return
	}
	// A wait for a credential this process proved, whose durable clear is
	// still pending, is stale on disk: read it as cleared.
	if !stamp.isZero() && stamp == g.authClearPending {
		stamp = claudeCredStamp{}
	}
	if stamp.isZero() {
		if !g.authWaitRestored.isZero() && g.authWait == g.authWaitRestored {
			g.authWait, g.authWaitNudged = claudeCredStamp{}, claudeCredStamp{}
		}
		g.authWaitRestored = claudeCredStamp{}
		return
	}
	if g.authWait == stamp && g.authWaitRestored != stamp {
		return
	}
	if g.authWait != stamp &&
		(g.authWait.isZero() || g.authWait == g.authWaitRestored || stamp.modNs > g.authWait.modNs) {
		g.authWait, g.authWaitNudged = stamp, claudeCredStamp{}
	}
	g.authWaitRestored = stamp
}

// authWaitStamp returns the stamp being waited on, zero for none.
func (g *claudeUsageProbeGate) authWaitStamp() claudeCredStamp {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.authWait
}

// takeCredentialNudge claims the one nudge a credential rewrite earns: true
// when a wait is recorded, `stamp` differs from it, and no gather has nudged
// for `stamp` yet.
func (g *claudeUsageProbeGate) takeCredentialNudge(stamp claudeCredStamp) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.authWait.isZero() || stamp == g.authWait || stamp == g.authWaitNudged {
		return false
	}
	g.authWaitNudged = stamp
	return true
}

// dropOwedThrough clears an in-memory debt the durable one was retired for,
// compared at the cache's millisecond resolution so a strictly newer run this
// process owes survives.
func (g *claudeUsageProbeGate) dropOwedThrough(owed time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.owedBaseline.IsZero() && g.owedBaseline.UnixMilli() <= owed.UnixMilli() {
		g.owedBaseline = time.Time{}
	}
}
