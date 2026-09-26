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
//     locked write that carries the covering reading, and only for a PROBE
//     write: that is the one writer which samples every window, so it is the
//     one whose observation is a claim about every row the card shows. A debt a
//     partial write happens to cover is cleared, without a request, by
//     payOwedClaudeUsageRefreshAt's row-aware pre-check. There is deliberately
//     no claudeSettleOwedRefresh here: a second clear path could disagree with
//     the merge.
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
// be recorded onto, or read back off, an obsolete login's cache. The one
// transition it will NOT make is scoped -> unscoped, which it refuses (returning
// false) — see the note in the body.
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
	wrote, _, _ := mutateClaudeRateLimitSnapshotStamped(path, fingerprint, fn)
	return wrote
}

// mutateClaudeRateLimitSnapshotStamped is mutateClaudeRateLimitSnapshot plus the
// cache stamp (claudeRateLimitCacheStamp's pair) of the file the mutation leaves
// behind, stat'd under the SAME lock as the write.
//
// Under the lock because a caller that latches a decision to a stamp must latch
// it to the snapshot it actually read and wrote. Sampling the stamp after the
// lock is released lets another process's rename land in between: the stamp then
// describes THAT snapshot while the state the caller adopted describes the
// previous one, so the latch matches a file whose debt and Retry-After were
// never read — and keeps matching until some later write moves the file again.
//
// Zero mod/size means the stat failed (or nothing was written). A caller keeps
// whatever older stamp it already holds in that case, which errs toward
// re-opening the decision rather than latching one away.
func mutateClaudeRateLimitSnapshotStamped(path, fingerprint string, fn func(*claudeRateLimitSnapshot) bool) (bool, int64, int64) {
	if path == "" || fn == nil {
		return false, 0, 0
	}
	wrote := false
	var modUnixNano, size int64
	_, _ = withClaudeRateLimitCacheLocked(path, time.Time{}, func() (time.Time, error) {
		snap := claudeRateLimitSnapshot{Buckets: map[string]claudeRateLimitBucket{}}
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &snap)
			if snap.Buckets == nil {
				snap.Buckets = map[string]claudeRateLimitBucket{}
			}
		}
		// A scoped cache written under an EMPTY fingerprint is refused, and
		// refused HERE, under the lock: an unlocked check could be overtaken by
		// another writer creating or re-scoping the snapshot before the lock is
		// granted. A credential read that transiently fails (a macOS Keychain
		// timeout) resolves to the same "" a genuine logout does, and treating it
		// as an account boundary would wipe the buckets of a login this device is
		// still signed in to. Only the live writers (stream capture, status-line hook, probe
		// merge) may reset a scope to unscoped — they carry a reading, and a debt
		// or hold marker does not.
		if fingerprint == "" && snap.AccountFingerprint != "" {
			return time.Time{}, nil
		}
		if snap.AccountFingerprint != fingerprint {
			// Any other fingerprint transition is an account boundary, including
			// an unscoped -> scoped flip — the same rule the merge applies.
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
		if info, err := os.Stat(path); err == nil {
			modUnixNano, size = info.ModTime().UnixNano(), info.Size()
		}
		return time.Time{}, nil
	})
	return wrote, modUnixNano, size
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
	path := claudeRateLimitCachePath()
	fingerprint := currentClaudeAccountFingerprint()
	// A credential read that transiently FAILS (a macOS Keychain timeout, a config
	// dir not yet readable) resolves to exactly the same "" a genuine accountless
	// claude.ai login does, and mutateClaudeRateLimitSnapshot cannot tell the two
	// apart: it would read the downgrade as an account boundary, drop the scoped
	// buckets, and write this debt under "" — where the next start, resolving the
	// recovered fingerprint, ignores it. So the completed turn would cost a wiped
	// cache AND an unpayable marker.
	//
	// mutateClaudeRateLimitSnapshot refuses that downgrade under the cache lock,
	// so the durable write is simply skipped. The in-memory debt still stands for
	// this process, so this degrades to the behaviour this file replaces — never
	// to something worse, which is the rule every other best-effort path here
	// follows.
	//
	// Only scoped -> unscoped is refused; a scoped -> other-scoped flip is a real
	// `/login` and proceeds. A genuine logout also resolves to "", and there the
	// live writers (stream capture, status-line hook, probe) reset the scope on
	// their next reading, after which an owe under "" proceeds normally.
	baselineMs := baseline.UnixMilli()
	mutateClaudeRateLimitSnapshot(path, fingerprint,
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

// claudePersistedProbeStateFor reports what a previous process left on the
// snapshot for `fingerprint`: the outstanding debt, how many replay attempts it
// has already cost, and any 429 hold still on record. All zero when there is
// none, when the cache belongs to another account, or when the file is missing
// or corrupt — this is a freshness optimisation, and the next run rewrites it.
//
// The debt and the hold are read TOGETHER because the one caller
// (claudeUsageProbeGate.seedOwedFromCache, on the gather path) needs both from
// one unlocked read: the debt so a run that finished before a restart is not
// mistaken for "nothing owed", and the hold so no gather can be ADMITTED inside
// a window the endpoint already imposed. payOwedClaudeUsageRefresh restores that
// hold too, but from a SPAWNED goroutine — the first gather of a fresh agent can
// reach begin() while that goroutine is still reading the credential store, and
// would then put a request to an endpoint that just told this device to stop.
//
// The hold is returned exactly as recorded, ceiling unchecked: the caller
// applies the same claudeUsageProbeMaxRetryAfter bound payOwedClaudeUsageRefreshAt
// does, and only the replay may CLEAR a skewed one — this is a read on the
// gather path and writes nothing.
//
// The account is a PARAMETER, never resolved here: the caller already holds the
// fingerprint its gather decoded, and resolving a second one would cost a macOS
// `security` spawn on a path that is contractually free of credential reads.
//
// Read WITHOUT the cache lock: the snapshot is only ever replaced by rename, so
// a reader sees a whole file or the previous whole file, and taking the gate for
// a read would queue this behind writers on the gather path.
func claudePersistedProbeStateFor(fingerprint string) (owed time.Time, attempts int, held time.Time) {
	snap, ok := loadClaudeRateLimitSnapshot(claudeRateLimitCachePath())
	if !ok || snap.AccountFingerprint != fingerprint {
		return time.Time{}, 0, time.Time{}
	}
	if snap.RefreshOwedAtMs != 0 {
		owed, attempts = time.UnixMilli(snap.RefreshOwedAtMs), snap.RefreshOwedAttempts
	}
	if snap.HeldUntilMs > 0 {
		held = time.UnixMilli(snap.HeldUntilMs)
	}
	return owed, attempts, held
}

// claudeRefreshDebtRetired reports whether a persisted debt is past paying:
// stamped in the future beyond the skew ceiling, older than the age limit, or
// already at the attempt cap. The ONE rule for both readers of the durable debt
// — the startup replay, which retires it, and the gather's seed, which must then
// refuse to adopt it — so a gather that reaches the seed before the replay
// cannot issue an uncharged request for a debt the cap has already retired.
func claudeRefreshDebtRetired(owed time.Time, attempts int, now time.Time) bool {
	return owed.After(now.Add(claudeRefreshOwedLocalSkew)) ||
		now.Sub(owed) > claudeRefreshOwedMaxAge ||
		attempts >= claudeUsageProbeAfterRunMaxAttempts
}

// claudeRateLimitCacheStamp identifies the CONTENTS of the snapshot file without
// parsing it: modification time and size, both zero when there is no readable
// file. Two different contents sharing a stamp would need the same size and the
// same nanosecond mtime; when that happens the caller degrades to treating the
// file as unchanged, which is exactly today's behaviour.
//
// It exists so claudeUsageProbeGate.seedOwedFromCache's per-account latch can be
// REVALIDATED rather than believed for the process lifetime: another agent
// channel writes this same file, and a debt or a 429 hold it records after the
// latch was set would otherwise stay invisible to this process — leaving the card
// stale for the whole staleness TTL, or admitting a probe inside a window the
// endpoint already imposed on this device. A stat is cheap enough to do per
// gather on a path that already reads the same file whole.
func claudeRateLimitCacheStamp() (modUnixNano, size int64) {
	info, err := os.Stat(claudeRateLimitCachePath())
	if err != nil {
		return 0, 0
	}
	return info.ModTime().UnixNano(), info.Size()
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

// adjustClaudeRefreshAttemptsAt moves the attempt counter by delta, ONLY while
// the debt is still the instant the caller judged — the same guard
// retireClaudeRefreshDebtAt applies, for the same reason: every decision in
// payOwedClaudeUsageRefreshAt is made from an unlocked read, and a run of this
// process can record a newer debt before the lock is granted. Charging or
// refunding that one would move a budget this replay never spent.
//
// The charge and the refund share it so the two cannot drift apart: a refund
// guarded differently from its charge would leak or invent attempts, and that
// counter is the only thing bounding a crash-looping agent.
//
// A charge is also refused once it would take the counter past the cap. The
// retirement check that admitted it ran on an unlocked read, so two overlapping
// agent processes can both pass it on the same count and then serialize here;
// without the locked bound each would charge and issue, exceeding the cap the
// counter exists to enforce. A refused charge means "do not probe".
func adjustClaudeRefreshAttemptsAt(owed time.Time, delta int) func(*claudeRateLimitSnapshot) bool {
	owedMs := owed.UnixMilli()
	return func(snap *claudeRateLimitSnapshot) bool {
		next := snap.RefreshOwedAttempts + delta
		if snap.RefreshOwedAtMs != owedMs || next < 0 ||
			(delta > 0 && next > claudeUsageProbeAfterRunMaxAttempts) {
			return false
		}
		snap.RefreshOwedAttempts += delta
		return true
	}
}

// dropClaudeSkewedHold clears a 429 hold ONLY while it is still the exact value
// the caller judged skewed. Sibling of retireClaudeRefreshDebtAt and there for
// the same reason: the read that spotted the skew is unlocked, so a probe that
// took a real 429 in the meantime may already have replaced the value, and
// clearing on the stale read would throw away live backpressure and send the
// next probe straight back at an endpoint that just refused us.
//
// Keyed to the observed value rather than re-tested against the ceiling: that
// ceiling was computed from the replay's `now`, and a legitimate maximum-length
// Retry-After recorded by another process a moment later lands just past it —
// a ceiling test would clear that live hold as if it were the skewed one.
func dropClaudeSkewedHold(judgedMs int64) func(*claudeRateLimitSnapshot) bool {
	return func(snap *claudeRateLimitSnapshot) bool {
		if snap.HeldUntilMs == 0 || snap.HeldUntilMs != judgedMs {
			return false
		}
		snap.HeldUntilMs = 0
		return true
	}
}

// claudeHoldSkewCeiling is the furthest ahead a persisted HeldUntilMs may be
// stamped and still be read as backpressure rather than a clock step: the same
// claudeUsageProbeMaxRetryAfter bound retryAfterDeadline puts on the live value.
//
// credentialLookupTook widens it by however long the caller spent resolving its
// credential AFTER sampling `now`. That read can block for a whole Keychain
// timeout, and another process recording a maximum-length Retry-After off its
// own, later clock inside that window writes a perfectly legitimate hold that a
// ceiling measured from the pre-read instant would delete as skew — freeing the
// replay to call an endpoint still inside its backoff. Callers with no such gap
// pass 0.
func claudeHoldSkewCeiling(now time.Time, credentialLookupTook time.Duration) int64 {
	return now.Add(credentialLookupTook).Add(claudeUsageProbeMaxRetryAfter).UnixMilli()
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

	// ONE credential read, resolved ONCE, for both halves of this replay: the
	// fingerprint every read and write below is scoped by, and the bearer token
	// the request will carry. They must name the SAME identity.
	//
	// Deriving the fingerprint separately (currentClaudeAccountFingerprint) and
	// letting the attempt re-resolve the credential at issue time leaves a window
	// — this runs on a goroutine that reads the credential store, the cache, and
	// then the network — in which a `/login` to another account lands between the
	// two reads. The debt would be loaded and CHARGED under account A while the
	// request went out with account B's token, and the merge carrying B's reading
	// would drop A's buckets as an account transition: B's quota spent on A's
	// run, and A's readings erased. Pinning the identity also spares the
	// duplicate credential read, which on a default macOS config shells out to
	// `security` under a timeout — see claudeUsageProbeIdentity.
	//
	// An account that flips AFTER this point is refused where it already was: the
	// snapshot comparison below, and mutateClaudeRateLimitSnapshot's own
	// fingerprint check under the cache lock.
	identityAt := time.Now()
	identity := claudeUsageProbeStoredIdentity()
	fingerprint := identity.fingerprint
	// How long that credential read actually took. On a default macOS config it
	// shells out to `security` under a timeout, so this is not a rounding error:
	// it is the window in which ANOTHER process can record a legitimate
	// maximum-length Retry-After off its own, later clock. Judging that hold
	// against the `now` sampled before the read would put it above
	// now+claudeUsageProbeMaxRetryAfter and delete it as clock skew, freeing this
	// replay to call an endpoint still inside its backoff — on a limit every
	// device on the account shares. The skew ceiling is therefore raised by the
	// time the lookup spent, which is ~0 in tests, so the injected clock stays
	// deterministic. Only the CEILING moves: the debt's age and retirement bounds
	// stay on the caller's instant, where a test can pin them.
	identityTook := time.Since(identityAt)

	// Opted out (`disable_claude_usage_probe`): the gate is never armed, so
	// nothing in this process or any later one can pay a debt. A debt written
	// before the user opted out is therefore CLEARED rather than left standing —
	// toggling the setting must not strand a marker nothing will ever retire.
	// This is the one early return that still writes.
	//
	// Not, however, when the identity behind that write is unresolved: a
	// transiently empty fingerprint over a scoped cache would be read as an
	// account transition and take the cached BUCKETS — utilization the
	// status-line path supplies whether or not this probe is enabled — down with
	// the debt. Opting out of the probe must not erase someone else's readings.
	// The debt simply stays until a start that can name the account retires it,
	// which is the same degradation every other best-effort write here takes.
	// mutateClaudeRateLimitSnapshot refuses that write under the cache lock.
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
	//
	// The ceiling is measured from the clock AFTER the credential read (see
	// identityTook): a hold written during that read is legitimate and must not
	// be misread as skew just because this goroutine was blocked while it landed.
	held := time.Time{}
	holdCeilingMs := claudeHoldSkewCeiling(now, identityTook)
	switch {
	case snap.HeldUntilMs <= 0:
	case snap.HeldUntilMs > holdCeilingMs:
		// Drop the value JUDGED, never whatever is on disk by the time the lock
		// is granted — see dropClaudeSkewedHold.
		mutateClaudeRateLimitSnapshot(path, fingerprint, dropClaudeSkewedHold(snap.HeldUntilMs))
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
	if claudeRefreshDebtRetired(owed, snap.RefreshOwedAttempts, now) {
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

	// Offline, a live hold, or no readable token: return AHEAD of the charge and
	// leave the debt and its counter exactly as found, so a device coming back
	// online — or one whose Keychain answers on the next start — inside the age
	// window can still pay. Charging here would spend the cap on restarts that
	// never asked the endpoint anything.
	//
	// The empty token is decided HERE, from the identity pinned at the top,
	// rather than only refunded afterwards: the attempt would reach
	// probeClaudeUsageAdmitted's empty-token exit and return issued=false without
	// asking anything, and a charge-then-refund pair is two best-effort cache
	// writes where a transiently unreadable credential store needs none. Same
	// rule the gather's seed charge already applies before it charges.
	if IsOffline() || (!held.IsZero() && now.Before(held)) || identity.token == "" {
		return
	}

	// Charged at exactly one point: after every refusal has been cleared and
	// immediately before the request goes out. A crash mid-request still costs
	// the attempt, so a restart loop cannot replay the same debt forever.
	//
	// The charge must reach DISK before the request does. The write is
	// best-effort like every other non-verified one here, so another cache
	// writer holding the gate or the flock past claudeRateLimitBestEffortGateWait
	// drops it — and probing anyway would leave the counter untouched, so a
	// device whose cache is persistently contended would issue one startup
	// request per restart forever. That counter is the only thing bounding a
	// crash-looping agent against an account-scoped endpoint, so an uncharged
	// attempt does not go out. The debt and its counter are left exactly as
	// found, so the next start still pays it — and this process's own trailing
	// probe or gather can still pay it now, from the gate seeded just above.
	if !mutateClaudeRateLimitSnapshot(path, fingerprint, adjustClaudeRefreshAttemptsAt(owed, +1)) {
		return
	}

	// ONE bounded attempt, through the ordinary single-flight probe, issued with
	// the identity this replay charged under — never a freshly resolved one.
	// Whatever it finds (or fails to find) is left to the ordinary gather/refresh
	// bounds; the merge that carries a covering reading settles the debt in its
	// own write.
	if done, issued := claudeUsageProbeAttemptIssuedAs(owed, func() claudeUsageProbeIdentity { return identity }); done && issued {
		return
	}
	// Nothing left this process, so the charge above bought nothing: either the
	// gate never admitted the attempt — a concurrent gather held the single-flight
	// slot — or it was admitted and returned before asking anything, which with a
	// pinned non-empty token means a rejected endpoint override.
	// Refund it, or two unlucky starts would retire a debt that was never once put
	// to the endpoint, leaving exactly the stale card this path exists to clear.
	// Safe in the crash direction: a crash between the charge and the refund keeps
	// the charge, which only ever spends the budget faster.
	mutateClaudeRateLimitSnapshot(path, fingerprint, adjustClaudeRefreshAttemptsAt(owed, -1))
}
