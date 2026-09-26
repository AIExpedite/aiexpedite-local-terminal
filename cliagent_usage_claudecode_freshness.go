// cliagent_usage_claudecode_freshness.go — the DURABLE half of Claude Code's
// post-run utilization refresh.
//
// Why this exists:
//
//	cliagent_usage_claudecode_probe.go already knows that a finished run needs a
//	reading newer than it: triggerClaudeUsageProbeAfterRun records the debt on
//	claudeUsageProbeGate.owedBaseline and schedules a bounded trailing probe.
//	That debt is a struct field, and it dies with the process. A run (or a
//	`__cli_smoke__` turn) that finished moments before an agent self-update
//	therefore left nothing behind: the new process started with an empty gate,
//	the next gather found a reading younger than claudeUsageProbeStaleAfter, and
//	the CLI Agents card kept showing a PRE-update utilization for the whole TTL
//	— a passing post-update smoke beside a stale card, which is exactly the
//	symptom reported.
//
//	Codex (cliagent_usage_codex_freshness.go) and Antigravity
//	(cliagent_usage_antigravity_freshness.go) already close this loop by
//	persisting the debt and replaying it once from StartAgent. This file is the
//	Claude equivalent, reusing the existing probe, gate, merge and lock ladder
//	rather than adding a second mechanism.
//
// Lifecycle:
//
//   - Owe — a terminal run, an abnormal session end, or a smoke that reached
//     inference records RefreshOwedAtMs (claudeOweRunRefresh). Only ever raises
//     the instant, so a burst of runs coalesces into one debt.
//   - Count — RefreshOwedAttempts resets only when the instant actually
//     advances. A repeat owe for the same baseline and the startup replay's
//     re-seeding both leave it alone, or the cap would be reset out of
//     existence on every restart.
//   - Pay — the in-process trailing probe pays it first (unchanged).
//     payOwedClaudeUsageRefresh is the backstop for when the process died.
//   - Settle — cleared inside mergeClaudeRateLimitCacheSerialized, in the same
//     locked write that carries the covering reading. There is deliberately no
//     claudeSettleOwedRefresh here: a second clear path could disagree with the
//     merge.
//   - Hold — a 429 Retry-After is mirrored to HeldUntilMs so a restart inside
//     the window does not re-storm an account-scoped endpoint.
//   - Retire — aged out, at the attempt cap, stamped implausibly far ahead, or
//     opted out: cleared without spending a request.
//
// Accepted gap: there is no persisted active-run FLOOR (Codex's RunFloorMs). A
// debt is recorded when the turn RETURNS, so a process killed mid-turn leaves
// nothing to replay and the device stays stale until the next run, refresh or
// routine gather. Adding an armed-floor field is a second state machine
// (arm / disarm / adopt) for a strictly rarer case than the one this fixes.
//
// Nothing new is published: the debt is three integers in a file the agent
// already writes. No credential, path, config fragment or identity crosses a
// new boundary.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

const (
	// claudeRefreshOwedMaxAge retires a debt no reading ever paid, matching
	// codexRefreshOwedMaxAge. Past it the run is far enough back that a fresh
	// reading is worth no more than the next routine gather's.
	claudeRefreshOwedMaxAge = 30 * time.Minute
	// claudeRefreshOwedLocalSkew is the clock-skew ceiling on a persisted
	// instant, mirroring antigravityRunFloorLocalSkew. A debt stamped further
	// ahead than this is a backwards clock step, not skew: left in place it
	// would be unpayable by any reading and would pin the card's staleness for
	// the whole age window.
	claudeRefreshOwedLocalSkew = 30 * time.Second
)

/* ─────────────────────────────── persistence ─────────────────────────────── */

// mutateClaudeRateLimitSnapshot applies fn to the on-disk snapshot under the
// SAME gate + flock ladder every other cache writer takes
// (withClaudeRateLimitCacheLocked), and writes only when fn reports a change —
// so a read-only or already-satisfied mutation leaves the cache byte-identical
// and terminal-service's payload-hash delta-skip still holds.
//
// New helper because nothing existing mutates the snapshot without supplying
// buckets: the merge's contract is "here are readings", and passing it an empty
// update map is rejected outright.
//
// It takes the fingerprint for the same reason the merge does: a snapshot
// belonging to a DIFFERENT account is reset before fn runs, so a debt can never
// be recorded onto, or read back off, an obsolete login's cache.
//
// Best-effort, like every other non-verified writer: on gate or lock contention
// the mutation is DROPPED rather than retried. A dropped debt degrades to the
// behaviour this file replaces (the in-memory debt still stands for this
// process) — never to something worse.
//
// UpdatedAt is deliberately left alone on an existing cache: it records when
// BUCKETS were merged, and a debt marker is not an observation. A snapshot this
// helper CREATES is stamped, so a debt-only file never reaches an operator (or
// a diagnostics upload) carrying an empty timestamp.
func mutateClaudeRateLimitSnapshot(path, fingerprint string, fn func(*claudeRateLimitSnapshot) bool) bool {
	if path == "" || fn == nil {
		return false
	}
	wrote := false
	_, _ = withClaudeRateLimitCacheLocked(path, time.Time{}, func() (time.Time, error) {
		snap := claudeRateLimitSnapshot{Buckets: map[string]claudeRateLimitBucket{}}
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &snap)
			if snap.Buckets == nil {
				snap.Buckets = map[string]claudeRateLimitBucket{}
			}
		}
		if snap.AccountFingerprint != fingerprint {
			// Any fingerprint transition is an account boundary, unscoped <->
			// scoped flips included — the same rule the merge applies.
			snap.Buckets = map[string]claudeRateLimitBucket{}
			snap.LastProbeObservedAtMs = 0
			snap.RefreshOwedAtMs, snap.RefreshOwedAttempts, snap.HeldUntilMs = 0, 0, 0
			snap.AccountFingerprint = fingerprint
		}
		if !fn(&snap) {
			return time.Time{}, nil
		}
		if snap.UpdatedAt == "" {
			snap.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		}
		out, err := json.MarshalIndent(snap, "", "  ")
		if err != nil {
			return time.Time{}, err
		}
		// Write-then-rename, with the same pid+nanosecond suffix the merge uses,
		// so a concurrent reader never observes a half-written file and two
		// writers cannot collide on the intermediate one.
		tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), time.Now().UnixNano())
		if err := claudeRateLimitCacheWriteFile(tmp, out, 0o600); err != nil {
			return time.Time{}, err
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return time.Time{}, err
		}
		wrote = true
		return time.Time{}, nil
	})
	return wrote
}

/* ────────────────────────────────── owe ──────────────────────────────────── */

// claudeOweRunRefresh mirrors an in-memory post-run debt to disk.
//
// Called from the goroutine triggerClaudeUsageProbeAfterRun already spawns —
// OFF claudeUsageProbeGate.mu and off the session stdout path. recordOwed stays
// a pure in-memory update under that mutex: a cache flock wait taken while
// holding it would stall frame handling and every other gate caller.
//
// Cost, stated plainly: this runs once per COMPLETED TURN, so a chatty session
// pays one fingerprint resolve plus one read-modify-rename of a few KB per
// turn, where the probe itself stays throttled to one request a minute. That is
// deliberate — the debt's whole value is being on disk before the process can
// die, so it cannot be debounced without reopening the window this file
// closes — and it is within the budget the same path already spends:
// captureClaudeRateLimitLine resolves the fingerprint and merges the cache on
// every rate_limit_event line of the same turn.
//
// An unchanged (or older) instant is a no-op: it neither rewrites the cache nor
// touches RefreshOwedAttempts. Resetting the counter on an unchanged instant
// would hand the same debt a fresh pair of attempts on every restart and
// silently undo the cap.
//
// A kill between the run's end and this write loses the debt. That is the same
// accepted gap as a turn killed mid-flight, not a new one.
func claudeOweRunRefresh(baseline time.Time) {
	if baseline.IsZero() {
		return
	}
	// When the user has opted out, nothing in this process (or the next) can ever
	// pay a debt, so recording one would leave a permanent unpayable marker.
	if !claudeUsageProbe.armedForProbe() {
		return
	}
	baselineMs := baseline.UnixMilli()
	mutateClaudeRateLimitSnapshot(claudeRateLimitCachePath(), currentClaudeAccountFingerprint(),
		func(snap *claudeRateLimitSnapshot) bool {
			if snap.RefreshOwedAtMs >= baselineMs {
				return false
			}
			snap.RefreshOwedAtMs = baselineMs
			// The instant genuinely advanced: a newer run is owed, so this debt
			// gets its own budget.
			snap.RefreshOwedAttempts = 0
			return true
		})
}

// claudeRunRefreshOwedFor reports the debt the previous process left for
// `fingerprint`, and how many replay attempts it has already cost. Zero when
// there is none, when the cache belongs to another account, or when the file is
// missing or corrupt — this is a freshness optimisation, and the next run
// rewrites it.
//
// The account is a PARAMETER, never resolved here: the one caller
// (claudeUsageProbeGate.seedOwedFromCache, on the gather path) already holds the
// fingerprint its gather decoded, and resolving a second one would cost a macOS
// `security` spawn on a path that is contractually free of credential reads.
//
// Read WITHOUT the cache lock: the snapshot is only ever replaced by rename, so
// a reader sees a whole file or the previous whole file, and taking the gate for
// a read would queue this behind writers on the gather path.
func claudeRunRefreshOwedFor(fingerprint string) (time.Time, int) {
	snap, ok := loadClaudeRateLimitSnapshot(claudeRateLimitCachePath())
	if !ok || snap.RefreshOwedAtMs == 0 || snap.AccountFingerprint != fingerprint {
		return time.Time{}, 0
	}
	return time.UnixMilli(snap.RefreshOwedAtMs), snap.RefreshOwedAttempts
}

/* ────────────────────────────────── hold ─────────────────────────────────── */

// claudeHoldUsageProbe mirrors a 429 Retry-After to disk beside the gate's own
// in-memory heldUntil, so a restart inside the hold window still sees it.
//
// Takes the fingerprint the caller already resolved: probeClaudeUsageAdmitted
// reads the credential exactly once per probe, and a second read here would
// spend a macOS `security` spawn to learn what it is holding.
//
// Monotonic — a shorter hold never shortens a longer one already on record —
// and never clears or charges a debt: backpressure suppresses the replay, it
// does not retire what the replay was going to pay.
func claudeHoldUsageProbe(fingerprint string, deadline time.Time) {
	if deadline.IsZero() {
		return
	}
	deadlineMs := deadline.UnixMilli()
	mutateClaudeRateLimitSnapshot(claudeRateLimitCachePath(), fingerprint,
		func(snap *claudeRateLimitSnapshot) bool {
			if snap.HeldUntilMs >= deadlineMs {
				return false
			}
			snap.HeldUntilMs = deadlineMs
			return true
		})
}

// retireClaudeRefreshDebtAt clears the debt ONLY while it is still the instant
// the caller judged. Every retire decision in payOwedClaudeUsageRefreshAt is
// made from an unlocked read, and that read can be overtaken by a run of this
// process finishing; scoping the clear to the judged instant is what stops a
// verdict about an old debt from being applied to a new one.
func retireClaudeRefreshDebtAt(owed time.Time) func(*claudeRateLimitSnapshot) bool {
	owedMs := owed.UnixMilli()
	return func(snap *claudeRateLimitSnapshot) bool {
		if snap.RefreshOwedAtMs != owedMs {
			return false
		}
		return clearClaudeRefreshDebt(snap)
	}
}

// dropClaudeSkewedHold clears a 429 hold ONLY while it is still parked beyond
// the ceiling a Retry-After could legitimately reach. Sibling of
// retireClaudeRefreshDebtAt and there for the same reason: the read that
// spotted the skew is unlocked, so a probe that took a real 429 in the meantime
// may already have replaced the value, and clearing on the stale read would
// throw away live backpressure and send the next probe straight back at an
// endpoint that just refused us.
func dropClaudeSkewedHold(ceilingMs int64) func(*claudeRateLimitSnapshot) bool {
	return func(snap *claudeRateLimitSnapshot) bool {
		if snap.HeldUntilMs == 0 || snap.HeldUntilMs <= ceilingMs {
			return false
		}
		snap.HeldUntilMs = 0
		return true
	}
}

/* ────────────────────────────────── pay ──────────────────────────────────── */

// payOwedClaudeUsageRefresh replays, at most once per agent start, the refresh a
// previous process owed — a Claude run or smoke that finished just before a
// crash, restart or self-update. Called from StartAgent beside
// payOwedCodexUsageRefresh and payOwedAntigravityUsageRefresh, and AFTER
// isOffline is published so the attempt honours offline mode.
//
// Runs on its own goroutine: it reads the credential store (a `security` spawn
// on macOS), the cache, and possibly the network, and StartAgent must wait for
// none of them. Bracketed by begin/endSettling so resetClaudeUsageProbeGate's
// drain — and the tests — can see it as in flight.
//
// Fleet cost: at most one api.anthropic.com/api/oauth/usage request per agent
// start per DEVICE. The cross-process dedupe (claudeUsageProbeObservedSince) is
// per-device, so a fleet-wide auto-update of N devices sharing one Anthropic
// account issues up to N requests inside the same minute against an
// account-scoped limit. Fine at tens of devices; past roughly a hundred on one
// account a 429 Retry-After parks the probe for up to claudeUsageProbeMaxRetryAfter
// — which is precisely why that hold is persisted rather than re-learned by
// re-storming the endpoint. The next lever, if the fleet grows, is a small random
// start-up jitter, deliberately not added now.
func payOwedClaudeUsageRefresh() {
	// Counted on the CALLER's goroutine so a reset (or a test) that samples the
	// gate immediately after this returns sees the replay as running.
	claudeUsageProbe.beginSettling()
	go func() {
		defer claudeUsageProbe.endSettling()
		defer func() { _ = recover() }()
		payOwedClaudeUsageRefreshAt(time.Now())
	}()
}

// payOwedClaudeUsageRefreshAt is payOwedClaudeUsageRefresh's body, off the boot
// goroutine and with the clock injected so the bounds are testable without wall
// time.
func payOwedClaudeUsageRefreshAt(now time.Time) {
	path := claudeRateLimitCachePath()
	fingerprint := currentClaudeAccountFingerprint()

	// Opted out (`disable_claude_usage_probe`): the gate is never armed, so
	// nothing in this process or any later one can pay a debt. A debt written
	// before the user opted out is therefore CLEARED rather than left standing —
	// toggling the setting must not strand a marker nothing will ever retire.
	// This is the one early return that still writes.
	if !claudeUsageProbe.armedForProbe() {
		mutateClaudeRateLimitSnapshot(path, fingerprint, clearClaudeRefreshDebt)
		return
	}

	snap, ok := loadClaudeRateLimitSnapshot(path)
	if !ok || snap.AccountFingerprint != fingerprint {
		// No cache, or one belonging to an account this device is no longer
		// signed in to. Either way there is nothing this process may pay.
		return
	}

	// A hold stamped further ahead than a Retry-After could legitimately reach is
	// a clock step, not backpressure — the same ceiling retryAfterDeadline puts
	// on the live value. Left in place it would disable utilization for days.
	held := time.Time{}
	switch {
	case snap.HeldUntilMs <= 0:
	case snap.HeldUntilMs > now.Add(claudeUsageProbeMaxRetryAfter).UnixMilli():
		// Re-apply the ceiling INSIDE the lock. The read above is unlocked, so a
		// probe that took a legitimate 429 in the meantime may already have
		// replaced the skewed value — clearing on the stale read would throw
		// away live backpressure and send the next probe straight back at an
		// endpoint that just refused us.
		mutateClaudeRateLimitSnapshot(path, fingerprint,
			dropClaudeSkewedHold(now.Add(claudeUsageProbeMaxRetryAfter).UnixMilli()))
	default:
		held = time.UnixMilli(snap.HeldUntilMs)
		// Carry the surviving hold into this process's gate too, so the ordinary
		// gather and trailing-probe paths honour it just as the replay does.
		claudeUsageProbe.holdUntil(held)
	}

	if snap.RefreshOwedAtMs == 0 {
		return
	}
	owed := time.UnixMilli(snap.RefreshOwedAtMs)

	// Retire without spending a request: stamped in the future beyond the skew
	// ceiling, older than the age limit, or already at the attempt cap. The debt
	// is retired at the cap whether or not it was ever actually paid — that is
	// what stops a crash-looping agent from issuing a request per restart.
	if snap.RefreshOwedAtMs > now.Add(claudeRefreshOwedLocalSkew).UnixMilli() ||
		now.Sub(owed) > claudeRefreshOwedMaxAge ||
		snap.RefreshOwedAttempts >= claudeUsageProbeAfterRunMaxAttempts {
		// Retire the debt this replay JUDGED, never whatever is on disk by the
		// time the lock is granted. This runs on a spawned goroutine, so a
		// session of this process can finish and record a newer, perfectly
		// payable debt between the unlocked read above and here; a blanket
		// clear would silently discard it and leave that run's card stale —
		// the very failure this file exists to fix.
		mutateClaudeRateLimitSnapshot(path, fingerprint, retireClaudeRefreshDebtAt(owed))
		return
	}

	// Coverage pre-check, BEFORE anything is charged. A settlement write can be
	// dropped under contention, so a standing RefreshOwedAtMs is not proof that
	// nothing was observed: the covering reading may already be on disk. Probing
	// on the debt alone would spend one OAuth request per restart for an answer
	// we are holding.
	if observed := claudeSnapshotFreshness(loadMergedClaudeRateLimitView(fingerprint), now); claudeUsageObservationCovers(observed, owed) {
		// Same rule: the reading covers the debt we read, and says nothing about
		// a newer one recorded since.
		mutateClaudeRateLimitSnapshot(path, fingerprint, retireClaudeRefreshDebtAt(owed))
		return
	}

	// Seed the in-memory gate from the persisted value so refreshClaudeUsageIfStale's
	// `owing` branch — and any run that finishes in this process — sees a debt
	// this process did not record. recordOwed, never an owe: re-owing would reset
	// RefreshOwedAttempts and undo the cap.
	claudeUsageProbe.recordOwed(owed)

	// Offline, or a live hold: return AHEAD of the charge and leave the debt and
	// its counter exactly as found, so a device coming back online inside the age
	// window can still pay. Charging here would spend the cap on restarts that
	// never asked the endpoint anything.
	if IsOffline() || (!held.IsZero() && now.Before(held)) {
		return
	}

	// Charged at exactly one point: after every refusal has been cleared and
	// immediately before the request goes out. A crash mid-request still costs
	// the attempt, so a restart loop cannot replay the same debt forever.
	mutateClaudeRateLimitSnapshot(path, fingerprint, func(s *claudeRateLimitSnapshot) bool {
		if s.RefreshOwedAtMs != owed.UnixMilli() {
			// A newer run was owed between the read and here; that debt has its
			// own budget and its own replay.
			return false
		}
		s.RefreshOwedAttempts++
		return true
	})

	// ONE bounded attempt, through the ordinary single-flight probe. Whatever it
	// finds (or fails to find) is left to the ordinary gather/refresh bounds; the
	// merge that carries a covering reading settles the debt in its own write.
	if claudeUsageProbeAttempt(owed) {
		return
	}
	// The gate never admitted it — a concurrent gather held the single-flight
	// slot — so no request left this process and the charge above bought
	// nothing. Refund it, or two unlucky starts would retire a debt that was
	// never once put to the endpoint, leaving exactly the stale card this path
	// exists to clear. Safe in the crash direction: a crash between the charge
	// and the refund keeps the charge, which only ever spends the budget
	// faster.
	mutateClaudeRateLimitSnapshot(path, fingerprint, func(s *claudeRateLimitSnapshot) bool {
		if s.RefreshOwedAtMs != owed.UnixMilli() || s.RefreshOwedAttempts == 0 {
			return false
		}
		s.RefreshOwedAttempts--
		return true
	})
}
