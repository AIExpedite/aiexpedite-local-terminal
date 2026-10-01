// cliagent_usage_claudecode_freshness.go — the DURABLE half of Claude Code's
// post-run utilization refresh.
//
// Why this exists:
//
//	cliagent_usage_claudecode_probe.go already knows that a finished run needs a
//	reading newer than it: triggerClaudeUsageProbeAfterRun records the debt on
//	claudeUsageProbeGate.owedBaseline. That debt is a struct field, and it dies
//	with the process. A run (or a `__cli_smoke__` turn) that finished moments
//	before an agent self-update therefore left nothing behind: the new process
//	started with an empty gate, the next gather found a reading younger than
//	claudeUsageProbeStaleAfter, and the CLI Agents card kept showing a
//	PRE-update utilization for the whole TTL — a passing post-update smoke
//	beside a stale card, which is exactly the symptom reported.
//
//	Codex (cliagent_usage_codex_freshness.go) and Antigravity
//	(cliagent_usage_antigravity_freshness.go) already close this loop by
//	persisting the debt and walking a retry ladder that survives a restart. This
//	file is the Claude equivalent, reusing the existing probe, gate, merge and
//	lock ladder rather than adding a second mechanism; the ladder's clock is
//	cliagent_usage_claudecode_refresh_schedule.go.
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
//   - Pay — every automatic attempt goes through claudeRunDebtAttemptAt
//     (cliagent_usage_claudecode_refresh_schedule.go): the run's own immediate
//     attempt, the persisted retry rung, and payOwedClaudeUsageRefresh at start.
//     Each one reserves a slot of the shared request budget before it sends.
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
//   - Retire — aged out, at the request cap, stamped implausibly far ahead, or
//     opted out: cleared without spending a request.
//
// Accepted gap: there is no persisted active-run FLOOR (Codex's RunFloorMs). A
// debt is recorded when the turn RETURNS, so a process killed mid-turn leaves
// nothing to replay and the device stays stale until the next run, refresh or
// routine gather. Adding an armed-floor field is a second state machine
// (arm / disarm / adopt) for a strictly rarer case than the one this fixes.
//
// Nothing new is published: the debt and its schedule are integers in a file
// the agent already writes. No credential, path, config fragment or identity
// crosses a new boundary.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

const (
	// claudeRefreshOwedMaxAge retires a debt no reading ever paid. The debt walks
	// a retry ladder that has to outlive an agent self-update, a 429 hold and an
	// expired token waiting on Claude Code's next refresh, so it matches the
	// Codex and Antigravity age-out rather than the old single-replay bound.
	claudeRefreshOwedMaxAge = 6 * time.Hour
	// claudeRefreshOwedMaxRequests caps the endpoint requests one debt may
	// cost across EVERY automatic path — the run's immediate attempt, the
	// retry ladder, the startup replay and a debt-forced refresh — counted by
	// RefreshOwedAttempts. A user's click is not counted.
	claudeRefreshOwedMaxRequests = 4
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

// mutateClaudeRateLimitSnapshotScoped is mutateClaudeRateLimitSnapshot with the
// merge's scope guard (claudeCacheScopeRejects) applied under the SAME lock,
// for the one caller whose marker was earned long before it is written: the 429
// hold, which belongs to the account the in-flight request was issued under. A
// `/login` landing while that request is out re-scopes the cache, and writing
// the old account's hold over it would take the new login's fresh buckets down
// as a transition — for backpressure that account never earned.
//
// `allowedScopes` names the scopes the marker may still be written over besides
// its own fingerprint; empty disables the guard, which is what every caller
// that resolves its identity immediately before writing wants.
func mutateClaudeRateLimitSnapshotScoped(path, fingerprint string, allowedScopes []string, fn func(*claudeRateLimitSnapshot) bool) bool {
	wrote, _, _ := mutateClaudeRateLimitSnapshotStampedScoped(path, fingerprint, allowedScopes, fn)
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
	return mutateClaudeRateLimitSnapshotStampedScoped(path, fingerprint, nil, fn)
}

// mutateClaudeRateLimitSnapshotStampedScoped is the implementation of both, with
// the optional under-lock scope guard mutateClaudeRateLimitSnapshotScoped
// documents.
func mutateClaudeRateLimitSnapshotStampedScoped(path, fingerprint string, allowedScopes []string, fn func(*claudeRateLimitSnapshot) bool) (bool, int64, int64) {
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
		// A marker earned under an account the cache has since moved off is
		// refused outright — here, under the lock, for the same reason the
		// empty-fingerprint check is. See mutateClaudeRateLimitSnapshotScoped.
		if claudeCacheScopeRejects(snap.AccountFingerprint, fingerprint, allowedScopes) {
			return time.Time{}, nil
		}
		if snap.AccountFingerprint != fingerprint {
			// Any other fingerprint transition is an account boundary, including
			// an unscoped -> scoped flip — the same rule the merge applies.
			resetClaudeProbeAccountState(&snap)
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
// would hand the same debt a fresh budget on every restart and silently undo
// the cap.
//
// Reports the fingerprint the debt was written under and whether a debt at or
// after `baseline` is on disk when it returns, so a caller can book its rung
// under the same account and arm an in-process rung for a write the cache locks
// dropped.
//
// A kill between the run's end and this write loses the debt. That is the same
// accepted gap as a turn killed mid-flight, not a new one.
func claudeOweRunRefresh(baseline time.Time) (fingerprint string, onDisk bool) {
	if baseline.IsZero() {
		return "", false
	}
	// When the user has opted out, nothing in this process (or the next) can ever
	// pay a debt, so recording one would leave a permanent unpayable marker.
	if !claudeUsageProbe.armedForProbe() {
		return "", false
	}
	path := claudeRateLimitCachePath()
	// Sampled BEFORE the credential read, not after: it names the scope this debt
	// is entitled to write over. A `/login` to B landing between this sample and
	// the locked write leaves onDisk == B, which is not in the allow-list, so the
	// mutation is refused instead of treating B -> A as an account transition —
	// which would clear the newly signed-in account's fresh buckets and hold and
	// then stamp A's debt onto its replacement state. Sampling AFTER the resolve
	// would defeat that: a login already landed would be sampled as B and then
	// allowed. See mutateClaudeRateLimitSnapshotScoped, which judges it under the
	// same lock the write takes.
	scopeBefore := claudeRateLimitCacheScope()
	fingerprint = currentClaudeAccountFingerprint()
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
	// A genuine logout also resolves to "", and there the live writers (stream
	// capture, status-line hook, probe) reset the scope on their next reading,
	// after which an owe under "" proceeds normally. A scoped -> other-scoped flip
	// still proceeds when the cache is where this debt left it (onDisk is the
	// sampled scope or the resolved fingerprint); a flip that overtook the sample
	// is refused by the allow-list above.
	baselineMs := baseline.UnixMilli()
	wrote := mutateClaudeRateLimitSnapshotScoped(path, fingerprint, []string{scopeBefore},
		func(snap *claudeRateLimitSnapshot) bool {
			if snap.RefreshOwedAtMs >= baselineMs {
				onDisk = true
				return false
			}
			snap.RefreshOwedAtMs = baselineMs
			// The instant genuinely advanced: a newer run is owed, so this debt
			// gets its own budget, and is due at once rather than on the rung an
			// older debt booked.
			snap.RefreshOwedAttempts = 0
			snap.NextAttemptAtMs = 0
			return true
		})
	return fingerprint, wrote || onDisk
}

// claudeOweRunRefreshNow is the SYNCHRONOUS owe for a `__cli_smoke__` turn,
// mirroring persistCodexSmokeRunDebt: a smoke is often the last thing that
// happens before an update hand-off replaces the process, so its debt — and
// the first rung — must be on disk before runClaudeCodeSmoke returns, not on a
// goroutine the hand-off can outrun. The in-memory debt is recorded too, so
// the immediate attempt the caller triggers next pays this run.
func claudeOweRunRefreshNow(baseline time.Time) {
	if baseline.IsZero() || !claudeUsageProbe.armedForProbe() {
		return
	}
	claudeUsageProbe.recordOwed(baseline)
	fingerprint, onDisk := claudeOweRunRefresh(baseline)
	if !onDisk {
		return
	}
	// The first rung. The immediate attempt the caller triggers next replaces it
	// with whatever its result books; a hand-off that lands before that attempt
	// finishes leaves this one for the next process to re-arm.
	now := time.Now()
	claudeBookRunDebtRung(fingerprint, baseline, now, claudeRungAfter, claudeRunDebtRetryLadder[0])
}

// claudePersistedProbeStateFor reports what a previous process left on the
// snapshot for `fingerprint`: the outstanding debt, how many requests it has
// already cost, and any 429 hold still on record. All zero when there is
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
	owed, attempts, held, _, _ = claudePersistedProbeStateWithWaitFor(fingerprint)
	return owed, attempts, held
}

// claudePersistedProbeStateWithWaitFor is claudePersistedProbeStateFor plus the
// persisted credential wait (AuthWaitCredStampNs / AuthWaitCredSize), from the
// same single unlocked read. The gather's seed restores that wait before it can
// be admitted, as the startup replay does: a first gather that beats the replay
// after a restart would otherwise re-send the very token the endpoint answered
// 401 for, and a few such starts would spend the debt's whole budget on it.
//
// `rung` is the booked retry rung (NextAttemptAtMs), zero when none, for the
// same reason: a first gather that beats the replay must re-arm a future rung
// rather than spend it early, as claudeRunDebtAttemptAt does.
func claudePersistedProbeStateWithWaitFor(fingerprint string) (owed time.Time, attempts int, held time.Time, wait claudeCredStamp, rung time.Time) {
	snap, ok := loadClaudeRateLimitSnapshot(claudeRateLimitCachePath())
	if !ok || snap.AccountFingerprint != fingerprint {
		return time.Time{}, 0, time.Time{}, claudeCredStamp{}, time.Time{}
	}
	if snap.RefreshOwedAtMs != 0 {
		owed, attempts = time.UnixMilli(snap.RefreshOwedAtMs), snap.RefreshOwedAttempts
	}
	if snap.HeldUntilMs > 0 {
		held = time.UnixMilli(snap.HeldUntilMs)
	}
	wait = claudeCredStamp{modNs: snap.AuthWaitCredStampNs, size: snap.AuthWaitCredSize}
	if snap.NextAttemptAtMs > 0 {
		rung = time.UnixMilli(snap.NextAttemptAtMs)
	}
	return owed, attempts, held, wait, rung
}

// claudeRefreshDebtRetired reports whether a persisted debt is past paying:
// stamped in the future beyond the skew ceiling, older than the age limit, or
// already at the request cap. The ONE rule for both readers of the durable debt
// — the startup replay, which retires it, and the gather's seed, which must then
// refuse to adopt it — so a gather that reaches the seed before the replay
// cannot issue an uncharged request for a debt the cap has already retired.
func claudeRefreshDebtRetired(owed time.Time, attempts int, now time.Time) bool {
	return owed.After(now.Add(claudeRefreshOwedLocalSkew)) ||
		now.Sub(owed) > claudeRefreshOwedMaxAge ||
		attempts >= claudeRefreshOwedMaxRequests
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
// Takes the fingerprint the caller already resolved: probeClaudeUsageResult
// reads the credential exactly once per probe, and a second read here would
// spend a macOS `security` spawn to learn what it is holding.
//
// Monotonic — a shorter hold never shortens a longer one already on record —
// and never clears or charges a debt: backpressure suppresses the replay, it
// does not retire what the replay was going to pay.
//
// `allowedScopes` carries the scope the caller sampled before its request went
// out: a hold earned by account A must not be written over a cache a `/login`
// re-scoped to B while the request was in flight, since that write would clear
// B's fresh buckets as an account transition. Judged under the lock — see
// mutateClaudeRateLimitSnapshotScoped.
func claudeHoldUsageProbe(fingerprint string, deadline time.Time, allowedScopes ...string) {
	if deadline.IsZero() {
		return
	}
	deadlineMs := deadline.UnixMilli()
	mutateClaudeRateLimitSnapshotScoped(claudeRateLimitCachePath(), fingerprint, allowedScopes,
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
			(delta > 0 && next > claudeRefreshOwedMaxRequests) {
			return false
		}
		snap.RefreshOwedAttempts += delta
		return true
	}
}

// claimClaudeRunDebtRungAt reserves one budget slot for the debt at `owed` AND
// claims the persisted rung rungMs the caller read, in one locked write, so two
// processes that both found the same rung due issue ONE request between them.
//
// The claim leaves a durable lease (AttemptClaimedUntilMs) rather than just
// clearing the rung: a process that loads the snapshot AFTER the claim reads
// NextAttemptAtMs == 0, which on its own is indistinguishable from a debt that
// was never booked, and would reserve a second slot while the first request is
// still out. While a live lease stands every claim is refused and
// *inFlightUntil names when it ends, so the caller can look again then — the
// owner's process alone holds the timer for what follows, and it may crash.
//
// When the debt still stands but its rung is no longer rungMs (another trigger
// claimed it, or a newer booking replaced it), it writes nothing and sets
// *claimedElsewhere. Any other refusal is adjustClaudeRefreshAttemptsAt's. On
// success *leaseMs is the lease written, for releaseClaudeRunDebtClaim.
func claimClaudeRunDebtRungAt(owed time.Time, rungMs int64, claimedElsewhere *bool, inFlightUntil *time.Time, leaseMs *int64) func(*claudeRateLimitSnapshot) bool {
	reserve := adjustClaudeRefreshAttemptsAt(owed, +1)
	owedMs := owed.UnixMilli()
	return func(snap *claudeRateLimitSnapshot) bool {
		// Judged at lock time: the lease measures a real request in flight.
		now := time.Now()
		if until, live := claudeRunDebtLeaseLive(snap, now); live {
			*claimedElsewhere = true
			*inFlightUntil = until
			return false
		}
		if snap.RefreshOwedAtMs == owedMs && snap.NextAttemptAtMs != rungMs {
			*claimedElsewhere = true
			return false
		}
		if !reserve(snap) {
			return false
		}
		snap.NextAttemptAtMs = 0
		*leaseMs = claudePublishRunDebtClaim(snap, now)
		return true
	}
}

// claudeRunDebtLeaseLive reports whether another attempt's claim on the debt
// (AttemptClaimedUntilMs) is still in flight at `now`, and until when. Every
// path that charges the debt's budget honours it, not only the rung claim: a
// gather or seed that reserved past it would send a second request while the
// owner's is still out. A lease beyond any this code writes is a backwards
// clock step, not an attempt in flight, and is ignored rather than honoured
// for hours.
func claudeRunDebtLeaseLive(snap *claudeRateLimitSnapshot, now time.Time) (time.Time, bool) {
	held := snap.AttemptClaimedUntilMs
	if held <= now.UnixMilli() || held > now.Add(claudeRunDebtClaimLease()).UnixMilli() {
		return time.Time{}, false
	}
	return time.UnixMilli(held), true
}

// claudeRunDebtClaimLease bounds how long a claimed attempt may be in flight:
// the whole probe (credential read, request, verified persist) plus slack.
// Shorter than the first ladder rung, so a crashed owner delays nothing it
// booked.
func claudeRunDebtClaimLease() time.Duration {
	return claudeUsageProbeWholeTimeout + claudeRunDebtRungSlack
}

// releaseClaudeRunDebtClaim ends the lease leaseMs once its attempt is over,
// ONLY while it is still that lease — another process may have taken a new one
// after this one lapsed. Best-effort: a dropped release just expires.
func releaseClaudeRunDebtClaim(leaseMs int64) func(*claudeRateLimitSnapshot) bool {
	return func(snap *claudeRateLimitSnapshot) bool {
		if snap.AttemptClaimedUntilMs != leaseMs {
			return false
		}
		snap.AttemptClaimedUntilMs = 0
		return true
	}
}

// claudeReserveRunDebtSlot charges one request to the debt at `owed`, durably,
// for a gather about to probe for it. `onDisk` reports whether that debt was on
// disk to charge at all, so a refused reservation (the cap, a contended lock,
// another process's live claim) can be told apart from a debt only this
// process holds.
//
// A granted reservation also PUBLISHES a claim lease, as the rung claim does:
// honouring other processes' leases is only half of it, and a reservation that
// left AttemptClaimedUntilMs unset would let a second process's gather or rung
// reserve too before either response landed — two requests in one wave on an
// account-scoped endpoint. On success leaseMs is the lease written, for the
// caller to hand to releaseClaudeRunDebtClaim once its attempt is over.
//
// `leasedElsewhere` reports a refusal by ANOTHER process's live claim lease:
// that process has a request for this debt on the wire now, so the caller must
// send nothing of its own until it lands — not even a routine stale-TTL probe,
// which the cross-process dedupe cannot see coming because the leased request
// has not persisted anything yet.
func claudeReserveRunDebtSlot(fingerprint string, owed time.Time) (reserved, onDisk, leasedElsewhere bool, leaseMs int64) {
	charge := adjustClaudeRefreshAttemptsAt(owed, +1)
	owedMs := owed.UnixMilli()
	locked := false
	reserved = mutateClaudeRateLimitSnapshot(claudeRateLimitCachePath(), fingerprint, func(snap *claudeRateLimitSnapshot) bool {
		locked = true
		onDisk = snap.RefreshOwedAtMs == owedMs
		now := time.Now()
		if _, live := claudeRunDebtLeaseLive(snap, now); live {
			leasedElsewhere = true
			return false
		}
		if !charge(snap) {
			return false
		}
		leaseMs = claudePublishRunDebtClaim(snap, now)
		return true
	})
	if !reserved {
		leaseMs = 0
	}
	// The lock was never granted: the debt may well be on disk, so this is a
	// refused reservation, not an untracked debt.
	return reserved, onDisk || !locked, leasedElsewhere, leaseMs
}

// claudePublishRunDebtClaim writes the claim lease for an attempt that has just
// charged the debt's budget under the cache lock, and returns it. Every path
// that charges the budget publishes one, so no other process can reserve for
// the same debt while this attempt's request is out.
func claudePublishRunDebtClaim(snap *claudeRateLimitSnapshot, now time.Time) int64 {
	snap.AttemptClaimedUntilMs = now.Add(claudeRunDebtClaimLease()).UnixMilli()
	return snap.AttemptClaimedUntilMs
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

// claudeHoldSkewCeilingRebased is claudeHoldSkewCeiling for a caller that did
// not time the work between sampling `now` and judging the hold, but knows that
// work happened — the gather samples `now` at the top of ParseContext and only
// reaches the seed after a credential read (a `security` spawn under a 3s
// timeout on macOS) and the rest of the scan.
//
// Another process recording a maximum-length Retry-After off its own, later
// clock anywhere inside that gap writes a perfectly legitimate hold that sits
// just past now+claudeUsageProbeMaxRetryAfter, and a ceiling measured from the
// pre-read instant would read it as a clock step and ignore it — admitting a
// request into a backoff window on a limit every device on the account shares.
// Rebasing on the live clock widens the ceiling by exactly the elapsed time, the
// same correction identityTook makes for the replay, and by ~0 when the caller's
// instant is already current — so an injected clock stays deterministic and a
// `now` sampled AHEAD of the wall clock is never narrowed.
func claudeHoldSkewCeilingRebased(now time.Time) int64 {
	took := time.Since(now)
	if took < 0 {
		took = 0
	}
	return claudeHoldSkewCeiling(now, took)
}

/* ────────────────────────────────── pay ──────────────────────────────────── */

// payOwedClaudeUsageRefresh resumes, once per agent start, the refresh a
// previous process owed — a Claude run or smoke that finished just before a
// crash, restart or self-update. Called from StartAgent beside
// payOwedCodexUsageRefresh and payOwedAntigravityUsageRefresh, and AFTER
// isOffline is published so the attempt honours offline mode. A rung booked in
// the future is re-armed on the timer rather than spent early; a due one makes
// one attempt; a refusal (a persisted hold, an expired credential, offline
// mode) books the matching rung instead of leaving nothing scheduled. Pub/Sub
// connectivity plays no part: the timer runs whether or not the subscription
// has reconnected.
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
	claudeRunDebtAttemptAt(now, claudeDebtTriggerStartup)
}

// claudeRunDebtAttemptAt is the ONE automatic attempt on the persisted run debt,
// shared by the run's immediate attempt, a retry rung firing, a credential
// nudge and the startup replay — so the budget, the refusals and the rung
// booking cannot differ between them. It returns the attempt result (zero when
// nothing was attempted: nothing owed, retired, already covered, or a future
// rung re-armed).
//
// Order: resolve the identity once; retire what is past paying; refuse locally
// (booking a free rung) whatever the gate or the credential already rules out;
// reserve one budget slot DURABLY; attempt; refund the slot if nothing was sent;
// book the rung the result calls for.
func claudeRunDebtAttemptAt(now time.Time, trigger claudeRunDebtTrigger) claudeProbeResult {
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
		return claudeProbeResult{code: claudeUsageProbe.refusal(now, false)}
	}

	claudeRetryPendingAuthClear()
	authWaitSeq := claudeUsageProbe.authWaitReadSeq()
	snap, ok := loadClaudeRateLimitSnapshot(path)
	if !ok || snap.AccountFingerprint != fingerprint {
		// No cache, or one belonging to an account this device is no longer
		// signed in to. Either way there is nothing this process may pay.
		return claudeProbeResult{}
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
		// gather and run-debt paths honour it just as the replay does.
		claudeUsageProbe.holdUntil(held)
	}

	if snap.RefreshOwedAtMs == 0 {
		return claudeProbeResult{}
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
		claudeUsageProbe.dropOwedThrough(owed)
		return claudeProbeResult{}
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
		claudeUsageProbe.settleOwed(owed)
		return claudeProbeResult{}
	}

	// Seed the in-memory gate from the persisted value so refreshClaudeUsageIfStale's
	// `owing` branch — and any run that finishes in this process — sees a debt
	// this process did not record. recordOwed, never an owe: re-owing would reset
	// RefreshOwedAttempts and undo the cap.
	claudeUsageProbe.recordOwed(owed)

	// A credential wait a previous attempt (or process) recorded survives the
	// restart, so the same expired or 401'd token is not re-sent on start; one
	// another process has since cleared drops the wait it was restored into.
	claudeUsageProbe.restoreAuthWait(claudeCredStamp{modNs: snap.AuthWaitCredStampNs, size: snap.AuthWaitCredSize}, authWaitSeq)

	// A rung booked in the future stands: the timer and the startup replay
	// re-arm it rather than spending it early. One stamped beyond any rung the
	// schedule can book is a backwards clock step and is treated as due.
	if trigger.honoursRung() && snap.NextAttemptAtMs > now.UnixMilli() {
		if until := time.UnixMilli(snap.NextAttemptAtMs).Sub(now); until <= claudeRunDebtRetryHorizon() {
			claudeArmRunDebtRetry(until, claudeDebtTriggerTimer)
			// The wait was restored just above, so a credential rewritten before
			// the restart can now claim its nudge and make that rung due.
			nudgeClaudeCredentialChanged(identity.credStamp)
			return claudeProbeResult{}
		}
	}

	// Refusals AHEAD of the reservation, so a refusal costs no budget and no
	// charge-then-refund pair of best-effort writes: the gate (offline, a live
	// hold, the 60 s floor or failure backoff), no readable token, or a token
	// this device can see has expired — or was answered 401 while the
	// credential file is unchanged. Each books its rung instead of returning
	// with nothing scheduled, which is how a pre-update hold or an expired
	// token used to strand the debt until it aged out.
	//
	// The empty token is decided HERE, from the identity pinned at the top: the
	// attempt would reach probeClaudeUsageResult's no_credential exit anyway,
	// and a transiently unreadable credential store needs no writes at all.
	refusal := claudeUsageProbe.refusal(now, false)
	if refusal == "" && identity.token == "" {
		refusal = claudeProbeNoCredential
	}
	if refusal == "" && (identity.tokenExpired(now) || claudeUsageProbe.awaitingCredentialChange(identity.credStamp)) {
		claudeUsageProbe.noteAuthWait(identity.credStamp)
		refusal = claudeProbeCredentialExpired
	}
	if refusal != "" {
		return claudeRunDebtRefused(fingerprint, owed, now, refusal)
	}

	// Reserve one slot of the debt's request budget, DURABLY, before any
	// network I/O. A crash or an update hand-off between here and the response
	// leaves the slot spent, so no sequence of restarts can exceed the cap — the
	// counter is the only thing bounding a crash-looping agent against an
	// account-scoped endpoint. A reservation the cache refuses (contention, an
	// I/O error, a debt replaced while we read, or a count two overlapping
	// processes both passed the unlocked retirement test on) sends nothing and
	// books a free rung.
	//
	// The same write CLAIMS the rung this attempt read (clears NextAttemptAtMs),
	// as codexClaimRunDebtRung does. The gate is process-local, so two agent
	// processes sharing this cache (overlapping release / dev channels) both
	// re-arm the same persisted rung and both find it due; without the claim
	// each would reserve a slot and send, spending the budget in parallel waves
	// on an account-scoped endpoint. A rung that is no longer the one read was
	// claimed by another trigger or replaced by a newer booking, whose owner
	// schedules what follows, so this attempt does nothing.
	//
	// The claim also leaves a lease, so a process that reads the snapshot after
	// the rung was cleared still sees the attempt in flight. That process looks
	// again once the lease ends: by then the owner has settled the debt or
	// booked the next rung, which the timer honours — or it crashed, and the
	// rung it never booked is due.
	claimedElsewhere := false
	var inFlightUntil time.Time
	var leaseMs int64
	if !mutateClaudeRateLimitSnapshot(path, fingerprint, claimClaudeRunDebtRungAt(owed, snap.NextAttemptAtMs, &claimedElsewhere, &inFlightUntil, &leaseMs)) {
		if claimedElsewhere {
			if !inFlightUntil.IsZero() {
				claudeArmRunDebtRetry(time.Until(inFlightUntil)+claudeRunDebtRungSlack, claudeDebtTriggerTimer)
			}
			return claudeProbeResult{}
		}
		// Usually a lock race with the stream capture of the very turn that owed
		// this debt, so a fresh debt retries promptly rather than on the free
		// rung's floor — see claudeUnpersistedRetryDelay.
		result := claudeProbeResult{code: claudeProbeReserveFailed}
		logClaudeUsageProbeResult(result)
		claudeBookRunDebtRung(fingerprint, owed, now, claudeRungAfter, claudeUnpersistedRetryDelay(now.Sub(owed)))
		return result
	}

	defer mutateClaudeRateLimitSnapshot(path, fingerprint, releaseClaudeRunDebtClaim(leaseMs))

	// ONE bounded attempt, through the ordinary single-flight probe, issued with
	// the identity this attempt reserved under — never a freshly resolved one.
	result, settled := claudeUsageProbeAttemptAs(owed, identity)
	if !result.issued {
		// Nothing left this process, so the slot bought nothing: the gate
		// refused in the gap since the pre-check, or the attempt was admitted
		// and returned before asking anything (a rejected endpoint override).
		// Safe in the crash direction: a crash between the reservation and this
		// refund keeps the slot spent, which only spends the budget faster.
		mutateClaudeRateLimitSnapshot(path, fingerprint, adjustClaudeRefreshAttemptsAt(owed, -1))
		// A probe that took the slot since the pre-check is joined exactly like
		// one the pre-check saw.
		if result.code == claudeProbeInFlight {
			return claudeRunDebtRefused(fingerprint, owed, now, result.code)
		}
	}
	if settled {
		// The covering merge settled the debt in its own write; this clears a
		// debt a SHARED covering reading answered (another writer's partial
		// write cannot settle it there) and the credential wait the request
		// proved over. A no-op when both are already gone.
		mutateClaudeRateLimitSnapshot(path, fingerprint, settleClaudeRunDebtAt(owed, result, identity.credStamp))
		return result
	}
	claudeBookRunDebtRungFor(fingerprint, owed, now, result)
	return result
}

// claudeRunDebtRefused handles a refusal that sent nothing: it books the rung
// the refusal calls for. A probe in flight is JOINED first — the holder never
// owns the debt schedule, and a routine probe admitted before this run finished
// persists a PRE-run reading, so the debt settles only on a covering one.
// Needs no credential: the fingerprint and the debt are the caller's.
func claudeRunDebtRefused(fingerprint string, owed, now time.Time, refusal claudeProbeCode) claudeProbeResult {
	result := claudeProbeResult{code: refusal}
	if refusal == claudeProbeInFlight {
		ctx, cancel := context.WithTimeout(context.Background(), claudeUsageProbeJoinTimeout)
		_, joinedAt := claudeUsageProbe.joinInFlight(ctx, fingerprint)
		cancel()
		if claudeUsageObservationCovers(joinedAt, owed) {
			claudeUsageProbe.settleOwed(owed)
			mutateClaudeRateLimitSnapshot(claudeRateLimitCachePath(), fingerprint, retireClaudeRefreshDebtAt(owed))
			return result
		}
	}
	logClaudeUsageProbeResult(result)
	claudeBookRunDebtRungFor(fingerprint, owed, now, result)
	return result
}

// settleClaudeRunDebtAt clears the debt judged at `owed` (if still standing)
// together with the credential wait the request proved over: only when the
// endpoint answered `probed` with a 2xx, and only while the persisted wait is
// still for that very stamp. Another process can persist a 401 wait for a
// REWRITTEN credential (under a newer debt) while this attempt for the older
// one is finishing; clearing that wait would let a restart resend the newly
// rejected token on the replacement debt's bounded budget. A shared reading
// proves nothing about this process's credential and clears no wait.
func settleClaudeRunDebtAt(owed time.Time, result claudeProbeResult, probed claudeCredStamp) func(*claudeRateLimitSnapshot) bool {
	retire := retireClaudeRefreshDebtAt(owed)
	clearWait := clearClaudeProvenAuthWait(probed)
	return func(snap *claudeRateLimitSnapshot) bool {
		changed := retire(snap)
		if result.credProven && clearWait(snap) {
			changed = true
		}
		return changed
	}
}

// clearClaudeProvenAuthWait drops the persisted credential wait only while it
// is still for `probed`, the credential the endpoint just answered with a 2xx.
// A wait persisted for any other (rewritten) credential is left alone, and a
// cache with no matching wait is not rewritten.
func clearClaudeProvenAuthWait(probed claudeCredStamp) func(*claudeRateLimitSnapshot) bool {
	return func(snap *claudeRateLimitSnapshot) bool {
		if probed.isZero() || snap.AuthWaitCredStampNs != probed.modNs || snap.AuthWaitCredSize != probed.size {
			return false
		}
		snap.AuthWaitCredStampNs, snap.AuthWaitCredSize = 0, 0
		return true
	}
}

// claudeClearProvenAuthWaitDurably applies clearClaudeProvenAuthWait and
// reports whether the cache no longer holds a wait for `probed`: true when the
// clear was written or there was nothing to clear, false when the cache
// refused the write (the gate or file lock was busy, or the write failed).
func claudeClearProvenAuthWaitDurably(fingerprint string, allowedScopes []string, probed claudeCredStamp) bool {
	if probed.isZero() {
		return true
	}
	ran, matched := false, false
	clear := clearClaudeProvenAuthWait(probed)
	wrote := mutateClaudeRateLimitSnapshotScoped(claudeRateLimitCachePath(), fingerprint, allowedScopes,
		func(snap *claudeRateLimitSnapshot) bool {
			ran = true
			matched = clear(snap)
			return matched
		})
	return wrote || (ran && !matched)
}

// claudePendingAuthClearRetryDelays spaces the background retries of a proven
// credential's durable clear. Bounded: once they run out, the seed and replay
// reads (claudeRetryPendingAuthClear) keep retrying for the life of the process.
var claudePendingAuthClearRetryDelays = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute}

// claudeQueuePendingAuthClear remembers a durable clear the cache refused and
// starts the one background loop that retries it.
func claudeQueuePendingAuthClear(fingerprint string, probed claudeCredStamp) {
	if !claudeUsageProbe.notePendingAuthClear(fingerprint, probed) {
		return
	}
	go func() {
		defer claudeUsageProbe.endPendingAuthClearRetry()
		for _, delay := range claudePendingAuthClearRetryDelays {
			time.Sleep(delay)
			if IsShutdownInProgress() || claudeRetryPendingAuthClear() {
				return
			}
		}
	}()
}

// claudeRetryPendingAuthClear retries a pending durable clear, true once none
// is pending. A cache that no longer carries that wait for that account (a
// peer cleared it, a booking overwrote it, or the account moved on) settles it
// without a write.
func claudeRetryPendingAuthClear() bool {
	stamp, fingerprint := claudeUsageProbe.pendingAuthClear()
	if stamp.isZero() {
		return true
	}
	snap, ok := loadClaudeRateLimitSnapshot(claudeRateLimitCachePath())
	if !ok || snap.AccountFingerprint != fingerprint ||
		snap.AuthWaitCredStampNs != stamp.modNs || snap.AuthWaitCredSize != stamp.size ||
		claudeClearProvenAuthWaitDurably(fingerprint, nil, stamp) {
		claudeUsageProbe.settlePendingAuthClear(stamp)
		return true
	}
	return false
}
