// cliagent_ratelimit_codex.go — passively captures Codex app-server's
// `token_count` JSON-RPC notification (carrying the `rate_limits` object) off
// the stdout we already scan in codex_appserver.go, and caches the latest
// per-window snapshot to disk.
//
// Why this exists:
//
//	Codex does NOT expose a usage API or an on-disk usage file. It DOES emit a
//	`token_count` notification on the `codex app-server` stdout stream while a
//	session is active, carrying:
//	    rate_limits: {
//	      primary:   { used_percent | utilization, resets_in_seconds | window_minutes },
//	      secondary: { used_percent | utilization, resets_in_seconds | window_minutes },
//	    }
//	primary is the rolling 5-hour window, secondary is the weekly window — the
//	direct analog of Claude Code's `rate_limit_event`. We accept both
//	`used_percent` (0..100) and `utilization` (0..1), and tolerate the
//	`5h`/`7d`/`weekly` window aliases so a minor Codex schema rename does not
//	silently zero the card.
//
// One consumer reads the cache this writes:
//  1. cliagent_usage_codex.go — turns the snapshot into the real five-hour /
//     weekly capacity metrics shown on the CLI Agents tab (replacing the old
//     "usage unobservable" dashed bars).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Codex rate-limit window identifiers, as emitted by the app-server's
// `token_count.rate_limits` map. We normalise upstream aliases (`5h`,
// `7d`/`weekly`) onto these stable internal keys so cliagent_usage_codex.go
// can lookup-by-id without re-implementing the alias table.
const (
	codexWindowPrimary                      = "primary"
	codexWindowSecondary                    = "secondary"
	codexProviderObservationFutureTolerance = 5 * time.Minute
	// FAT-style filesystems can report modification times in two-second
	// increments. When a selected rollout advances beyond the gather's start
	// time, retain that full overlap so a concurrently created file whose mtime
	// rounded down before discovery cannot fall permanently below the cursor.
	codexRolloutCoarseMtimeOverlap = 2 * time.Second
)

// codexLegacyLimitID is the synthetic contributor id for buckets coming from
// the legacy aggregate `rate_limits` view (no `rateLimitsByLimitId` key). We
// track every contributor separately so a sparse `account/rateLimits/updated`
// that only mentions one metered limit (e.g. `codex_primary`) does not silently
// clobber a stricter prior contributor (e.g. `codex_other`) that the sparse
// frame did not restate.
const codexLegacyLimitID = "__legacy__"

// codexResetJitterMs is how much two reset timestamps may differ and still be
// treated as the same quota window. A sparse reset-only frame recomputes
// ResetsAtMs from the local receive time plus `resets_in_seconds`, so the
// same live window can drift by milliseconds (clock skew) or a rounded second
// (Codex emits the relative reset at second precision) between consecutive
// frames. A real rollover, in contrast, jumps by at least the window length
// (5 hours for primary, 1 week for secondary), so any sub-minute tolerance
// safely separates jitter from a fresh window. Picked larger than expected
// jitter (~1s) and far smaller than the next real Codex window length (the
// 4-hour primary alternative at 14400s).
const codexResetJitterMs int64 = 60_000

// resetsWithinJitter reports whether two ResetsAtMs values likely describe the
// same quota window despite small local-time drift. Used to keep a heartbeat
// `resets_in_seconds` frame from flipping the card to Unknown just because the
// recomputed absolute reset slipped by a second.
func resetsWithinJitter(a, b int64) bool {
	delta := a - b
	if delta < 0 {
		delta = -delta
	}
	return delta <= codexResetJitterMs
}

// codexRateLimitBucket is one window's observed state. UsedPercentage is
// normalised to 0..100 regardless of source shape (utilization 0..1 vs
// used_percent 0..100). ResetsAtMs is unix epoch milliseconds (0 = unknown).
// WindowMinutes records the documented length of the rolling window when
// Codex advertises it (`window_minutes` / `windowDurationMins`), so the metric
// label can be derived from the actual duration rather than assuming
// primary == 5h: a future plan whose primary window is e.g. 15 minutes
// would otherwise still be reported as a "5-hour session window".
type codexRateLimitBucket struct {
	UsedPercentage float64 `json:"usedPercentage"`
	ResetsAtMs     int64   `json:"resetsAtMs"`
	ObservedAtMs   int64   `json:"observedAtMs"`
	WindowMinutes  float64 `json:"windowMinutes,omitempty"`
	// Inferred marks an observation whose time Codex did NOT state: a rollout
	// frame with no parseable envelope `timestamp`, accepted only by a forced
	// post-run reconcile and anchored at the rollout file's mtime clamped to
	// [run floor, now] (codexRolloutInferredObservation). It loses every tie
	// against a stated observation and never displaces one that already covers
	// the run floor.
	Inferred bool `json:"inferred,omitempty"`
	// usageKnown/resetKnown mark which fields were freshly observed in this
	// update. Codex's account/rateLimits/updated is sparse — a notification
	// may carry only the new reset time, or only a new used_percent, and the
	// other field must be preserved from the prior snapshot rather than
	// silently overwritten with zero. Not persisted: every loaded bucket is
	// treated as fully observed.
	usageKnown bool `json:"-"`
	resetKnown bool `json:"-"`
	rolledOver bool `json:"-"`
}

// codexRateLimitSnapshot is the on-disk cache, keyed by window id (primary /
// secondary). AccountFingerprint pins the snapshot to the Codex account that
// produced it — when the local creds change, a stale window must NOT be
// attributed to the new account (the CLI Agents tab would otherwise show
// another user's capacity until the new account emits its own telemetry).
//
// Buckets is the aggregated most-constrained view of each display window
// (primary / secondary) and is what cliagent_usage_codex.go renders. Contributors
// remembers the per-(window, limit-id) buckets that feed that aggregate — a
// later sparse `account/rateLimits/updated` for only one metered limit can
// therefore replace just that limit's slot without losing a stricter prior
// limit's bucket. The aggregate is recomputed from Contributors on every write.
type codexRateLimitSnapshot struct {
	UpdatedAt          string                                     `json:"updatedAt"`
	AccountFingerprint string                                     `json:"accountFingerprint,omitempty"`
	Buckets            map[string]codexRateLimitBucket            `json:"buckets"`
	Contributors       map[string]map[string]codexRateLimitBucket `json:"contributors,omitempty"`
	// LimitNames maps a metered limit id to the display name Codex gives it
	// (`rateLimitsByLimitId.<id>.limitName`). A named limit is an independent
	// model pool (e.g. `codex_bengalfox` → "GPT-5.3-Codex-Spark") and renders as
	// its own rows; an unnamed limit is the account's main pool.
	LimitNames map[string]string `json:"limitNames,omitempty"`
	// FullSnapshotAtMs is when an authoritative `account/rateLimits/read` last
	// restated every window that applies to the account. Only after one has been
	// seen may a main-pool window the account does not report be left off the
	// card rather than rendered as an unobserved placeholder.
	FullSnapshotAtMs int64 `json:"fullSnapshotAtMs,omitempty"`
	// RolloutHighWaterMtimeMs is filesystem scan progress, not provider
	// observation time. It advances after every selected rollout file was either
	// handled or recorded by redacted identity for retry, allowing completed
	// siblings to rotate through a capped backlog.
	RolloutHighWaterMtimeMs int64 `json:"rolloutHighWaterMtimeMs,omitempty"`
	// RolloutHighWaterMtimeNs preserves the filesystem's full timestamp
	// precision. The millisecond field remains for backwards compatibility with
	// snapshots written before same-millisecond appends were handled.
	RolloutHighWaterMtimeNs int64 `json:"rolloutHighWaterMtimeNs,omitempty"`
	// RolloutHighWaterBoundaryFingerprint identifies only the files and sizes at
	// the current mtime boundary. It lets a coarse-resolution filesystem expose
	// an append whose mtime stayed exactly equal to the high-water without
	// persisting rollout paths or reopening unchanged files.
	RolloutHighWaterBoundaryFingerprint string `json:"rolloutHighWaterBoundaryFingerprint,omitempty"`
	// RolloutHighWaterBoundaryCursor is the SHA-256 digest of the last boundary
	// entry opened when more equal-mtime files exist than one capped pass can
	// consume. The digest resumes deterministic ordering without persisting a
	// rollout path; including file size makes an equal-mtime append reset safely.
	RolloutHighWaterBoundaryCursor string `json:"rolloutHighWaterBoundaryCursor,omitempty"`
	// A capped newest-first pass can leave older, distinct-mtime candidates above
	// the completed high-water. Track the redacted cohort and last opened rank so
	// later refreshes consume the rest before the main watermark advances. The
	// cohort ceiling lets an already-consumed active rollout become eligible again
	// when an append advances its mtime, without restarting the older backlog, and
	// scopes the fingerprint so a rollout created above that ceiling joins the scan
	// without discarding progress. RolloutBacklogCohortSize records how many
	// identities the fingerprint covered so a member that left the cohort upward is
	// recognised as a removal rather than a membership change.
	RolloutBacklogFingerprint string `json:"rolloutBacklogFingerprint,omitempty"`
	RolloutBacklogCursor      string `json:"rolloutBacklogCursor,omitempty"`
	RolloutBacklogMtimeNs     int64  `json:"rolloutBacklogMtimeNs,omitempty"`
	RolloutBacklogCohortSize  int    `json:"rolloutBacklogCohortSize,omitempty"`
	// RolloutRetryEntries contains only SHA-256 identities for rollout files
	// whose last read was retryable. Keeping these redacted identities separate
	// lets completed siblings advance the capped backlog while failed files are
	// re-offered without persisting paths or raw rollout contents.
	RolloutRetryEntries []string `json:"rolloutRetryEntries,omitempty"`
	// A retry-only overflow would otherwise reselect the same newest 16 identities
	// every refresh. The redacted cursor/fingerprint rotate that cohort past the
	// first capped batch so a recovered older failure can be reopened.
	RolloutRetryCursor      string `json:"rolloutRetryCursor,omitempty"`
	RolloutRetryFingerprint string `json:"rolloutRetryFingerprint,omitempty"`
	// Future-dated rollout mtimes are invalid normal progress: advancing the main
	// watermark to them could hide normally timestamped files written after a
	// clock rollback. Track their redacted membership and capped-batch position
	// separately so unchanged anomalous files are not reopened on every refresh.
	// The anchor fixes the cohort definition across wall-clock catch-up while an
	// unfinished capped batch is resumed, and the floor/ceiling close it around the
	// members that existed when the anchor was saved: an ordinary rollout written
	// afterwards is also newer than the anchor, so without those bounds it joins
	// the cohort, changes its fingerprint and discards a cursor whose capped scan
	// never finished. RolloutFutureMtimeCohortSize records how many members the
	// fingerprint covered so one leaving the bounds reads as a removal rather than
	// a membership change.
	RolloutFutureMtimeAnchorNs    int64  `json:"rolloutFutureMtimeAnchorNs,omitempty"`
	RolloutFutureMtimeFloorNs     int64  `json:"rolloutFutureMtimeFloorNs,omitempty"`
	RolloutFutureMtimeCeilingNs   int64  `json:"rolloutFutureMtimeCeilingNs,omitempty"`
	RolloutFutureMtimeFingerprint string `json:"rolloutFutureMtimeFingerprint,omitempty"`
	RolloutFutureMtimeCursor      string `json:"rolloutFutureMtimeCursor,omitempty"`
	RolloutFutureMtimeCohortSize  int    `json:"rolloutFutureMtimeCohortSize,omitempty"`
	RolloutFutureMtimeComplete    bool   `json:"rolloutFutureMtimeComplete,omitempty"`
	// RolloutRootFingerprint scopes filesystem progress to the CODEX_HOME tree
	// that produced it. It is a hash of the normalized root, never the raw path.
	RolloutRootFingerprint string `json:"rolloutRootFingerprint,omitempty"`
	// Run freshness (cliagent_usage_codex_freshness.go). RunFloorMs is when the
	// oldest Codex run whose utilization is still unobserved started;
	// RefreshOwedAtMs is when a run finished without that observation, i.e. a
	// post-run refresh is owed; RefreshOwedAttempts counts the bounded post-run
	// reconciles spent on it. Persisted beside the evidence that settles them so
	// a run that finished just before an agent restart or self-update is still
	// paid. Numeric only; zero on snapshots written before these fields existed,
	// which reads as "nothing owed".
	RunFloorMs          int64 `json:"runFloorMs,omitempty"`
	RefreshOwedAtMs     int64 `json:"refreshOwedAtMs,omitempty"`
	RefreshOwedAttempts int   `json:"refreshOwedAttempts,omitempty"`
	// ActiveRunFloorMs parks the start of a run that began while an OLDER
	// run's debt was still standing. RunFloorMs must keep the older floor
	// (that is what the debt waits on), but settling that debt would
	// otherwise erase every trace of the newer run and skip its crash
	// recovery; the parked floor is promoted into RunFloorMs when the older
	// debt clears.
	ActiveRunFloorMs int64 `json:"activeRunFloorMs,omitempty"`
	// RunFloorPaidMs is the newest floor already covered by a contributor
	// observation. Contributors can legitimately disappear later (an empty
	// authoritative full snapshot after a quota reset drops them), and without
	// this watermark the surviving RunFloorMs would read as unobserved again
	// and resurrect a settled run as "interrupted". A floor at or below it is
	// paid for good.
	RunFloorPaidMs int64 `json:"runFloorPaidMs,omitempty"`
	// RefreshFallbackState tracks the one live `account/rateLimits/read` a
	// debt may spend once its rollout attempts are exhausted
	// (codexLiveUsageFallback): "" = not owed yet, then outstanding / spent /
	// skipped. Reset with the attempt counter, so it is per debt generation.
	RefreshFallbackState string `json:"refreshFallbackState,omitempty"`
	// CodexVersion is the `--version` of the Codex binary that produced the
	// newest contributor observation; RolloutCursorVersion is the binary in
	// use when the rollout scan cursor was last reset or first established.
	// They answer different questions and must not be collapsed — see
	// cliagent_usage_codex_capture_stamp.go.
	CodexVersion         string `json:"codexVersion,omitempty"`
	RolloutCursorVersion string `json:"rolloutCursorVersion,omitempty"`
}

// RefreshFallbackState values (codexLiveUsageFallback).
const (
	codexFallbackUnset       = ""
	codexFallbackOutstanding = "outstanding"
	codexFallbackSpent       = "spent"
	codexFallbackSkipped     = "skipped"
)

// codexRateLimitMu serialises the read-modify-write of the cache file
// in-process. Cross-process serialization is handled separately by an
// advisory file lock (the cache may be touched by a future Codex statusline
// hook running in a different process, same as Claude's).
var codexRateLimitMu sync.Mutex

// codexRateLimitCacheLockWait bounds how long a WAITING Codex cache writer
// blocks on each of the two locks the transaction takes — the in-process gate
// and the cross-process advisory file lock — and codexRateLimitCacheLockPoll is
// the retry cadence inside that wait. Vars so a contention test can pin them
// small instead of spending seconds of wall clock.
//
// Bounded rather than blocking, because both waiters sit on deadlines they do
// not own. captureCodexRateLimitLine merges SYNCHRONOUSLY inside the Codex
// stdout scanners, so an unbounded wait there stops output publication and
// hangs the session; a FORCED post-run reconcile runs inside
// handleCLIUsageRefreshCommand's gather, whose 10s budget a wedged holder would
// otherwise consume whole, starving the refresh receipt and every later
// provider. Go cannot cancel a filesystem syscall, so bounding the waiters is
// the only lever available — the stalled holder itself cannot be interrupted.
//
// Two seconds is generous for a read-merge-rename of a few KB of JSON: an
// ordinary contending writer is gone in milliseconds, so the wait only expires
// when the holder is genuinely wedged, which is exactly when giving up is
// right. Giving up costs at most one reading: the cursor is left unadvanced, so
// the same evidence is offered again on the next refresh, and an unsettled
// freshness debt simply stays owed.
var (
	codexRateLimitCacheLockWait = 2 * time.Second
	codexRateLimitCacheLockPoll = 10 * time.Millisecond
)

// codexAcquireCacheGate takes the in-process gate, waiting no later than
// deadline. Reports whether it was acquired; the caller MUST unlock when true.
func codexAcquireCacheGate(deadline time.Time) bool {
	for {
		if codexRateLimitMu.TryLock() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(codexRateLimitCacheLockPoll)
	}
}

// codexCacheLockDeadline is how long this transaction may spend waiting on
// locks: codexRateLimitCacheLockWait, clamped to the caller's own deadline so a
// forced reconcile can never outlive the gather that is waiting on it.
func codexCacheLockDeadline(ctx context.Context) time.Time {
	deadline := time.Now().Add(codexRateLimitCacheLockWait)
	if ctx != nil {
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			return d
		}
	}
	return deadline
}

// codexRateLimitCachePath is the cache location inside the agent's data dir.
// AIEXPEDITE_CODEX_RL_CACHE overrides it (tests isolate from the real machine
// cache; ops can relocate it if the data dir is read-only).
func codexRateLimitCachePath() string {
	if p := os.Getenv("AIEXPEDITE_CODEX_RL_CACHE"); p != "" {
		return p
	}
	return filepath.Join(GetConfigDir(), "codex_rate_limits.json")
}

// codexWindowAliases maps upstream window keys to our stable internal ids.
// Codex's app-server today uses `primary`/`secondary`; older docs and a few
// schema sketches use `5h`/`7d`/`weekly`. Accept both so a rename doesn't
// silently zero the card.
var codexWindowAliases = map[string]string{
	"primary":   codexWindowPrimary,
	"5h":        codexWindowPrimary,
	"five_hour": codexWindowPrimary,
	"secondary": codexWindowSecondary,
	"7d":        codexWindowSecondary,
	"weekly":    codexWindowSecondary,
	"seven_day": codexWindowSecondary,
}

// codexBucketFromInfo builds a normalised bucket from a single Codex rate-limit
// window object. Accepts both `used_percent` (0..100) and `utilization` (0..1),
// and reset times expressed as `resets_in_seconds` (relative) or
// `window_minutes` (window size — used only when no explicit reset is given).
func codexBucketFromInfo(info map[string]interface{}, now time.Time) (codexRateLimitBucket, bool) {
	b := codexRateLimitBucket{ObservedAtMs: now.UnixMilli()}

	usageObserved := false
	if v, ok := pickField(info, "used_percent", "usedPercent", "used_percentage", "usedPercentage"); ok {
		if f, ok := numAsFloat(v); ok && !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 && f <= 100 {
			b.UsedPercentage = f
			usageObserved = true
		}
	} else if v, ok := info["utilization"]; ok {
		if f, ok := numAsFloat(v); ok && !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 && f <= 1 {
			b.UsedPercentage = f * 100
			usageObserved = true
		}
	}

	if v, ok := pickField(info, "resets_at", "resetsAt"); ok {
		if f, ok := numAsFloat(v); ok {
			b.ResetsAtMs = normalizeResetMs(f)
		}
	}
	if b.ResetsAtMs == 0 {
		if v, ok := pickField(info, "resets_in_seconds", "resetsInSeconds"); ok {
			if f, ok := numAsFloat(v); ok && f > 0 {
				b.ResetsAtMs = now.Add(time.Duration(f * float64(time.Second))).UnixMilli()
			}
		}
	}
	// Always record the documented window length when present, even when an
	// explicit reset was given — the label ("5-hour session window") is
	// derived from this, not from the position in the rate_limits map.
	// Note: window_minutes is the rolling window LENGTH, not the time until
	// reset, so it must never be used to fabricate ResetsAtMs — Codex emits
	// a real resetsAt/resets_in_seconds for that, and a sparse update without
	// either field should leave the reset Unknown rather than overwrite a
	// previously correct reset with one hours or days too late.
	if v, ok := pickField(info, "window_minutes", "windowMinutes", "windowDurationMins"); ok {
		if f, ok := numAsFloat(v); ok && f > 0 {
			b.WindowMinutes = f
		}
	}
	b.usageKnown = usageObserved
	b.resetKnown = b.ResetsAtMs > 0

	if !b.usageKnown && !b.resetKnown {
		return b, false
	}
	return b, true
}

// extractCodexRateLimitBuckets pulls every window it can find from a decoded
// Codex app-server frame. The payload may sit under `params` (notifications:
// `token_count`, `account/rateLimits/updated`), `result` (response to
// `account/rateLimits/read`), `params.msg` / `result.msg` (typed event
// envelope under a JSON-RPC frame), or — for the JSONL event-envelope shape
// the app-server emits outside of a JSON-RPC request/response pair
// (`{"id":"…","msg":{"type":"token_count", …}}` and the session-event
// `{"payload":{…}}` variant) — under a top-level `msg` / `payload`. Both
// snake_case (`rate_limits`) and camelCase (`rateLimits`) are accepted so a
// schema rename doesn't silently zero the card.
//
// The second return value names windows the frame explicitly cleared. A full
// `account/rateLimits/read` response (carried under `result`) can return
// `secondary: null` for accounts without a weekly window; that null is a
// statement that the window does not exist, so any previously-cached bucket
// for it must be dropped rather than left to render stale numbers until its
// old reset time passes. Sparse `account/rateLimits/updated` / `token_count`
// notifications under `params` are NOT full snapshots — a missing or null
// window there means "no update for this window," not "clear it," so we
// only honour clears that arrive via the `result` path.
func extractCodexRateLimitBuckets(raw map[string]interface{}, now time.Time) (map[string]map[string]codexRateLimitBucket, map[string]bool) {
	out, clears, _, _, _ := extractCodexRateLimitBucketsFull(raw, now)
	return out, clears
}

// extractCodexRateLimitBucketsFull is extractCodexRateLimitBuckets plus the two
// signals the cache-write reconciler needs: whether this frame was a FULL
// snapshot (an `account/rateLimits/read` response carried under `result`, which
// states every window that currently applies) and, if so, the set of display
// windows the full snapshot referenced (`present`). A full snapshot that omits a
// previously-cached window is declaring that window gone, so the merger drops it
// — but only for full snapshots; sparse notifications never clear an omitted
// window. `present` includes both windows carrying a bucket and windows the
// snapshot explicitly nulled, so a reclassified/omitted window is reconciled
// against the complete picture. Callers that only need the buckets/clears use
// the thin extractCodexRateLimitBuckets wrapper above.
func extractCodexRateLimitBucketsFull(raw map[string]interface{}, now time.Time) (map[string]map[string]codexRateLimitBucket, map[string]bool, bool, map[string]bool, bool) {
	out := map[string]map[string]codexRateLimitBucket{}
	clears := map[string]bool{}
	fullSnapshot := false
	present := map[string]bool{}
	// sawEmptyFullContainer records that a full-snapshot rate-limit container
	// (`rateLimits` / `rateLimitsByLimitId`) was present but literally empty
	// (`{}`). That is the ONLY shape that authoritatively declares "this account
	// now has zero quota windows" and may clear the cache. A full snapshot whose
	// container is non-empty but yields nothing we recognise (unknown window
	// keys, unparseable `primary:{}` bucket objects, forward-compatible fields)
	// must NOT be treated as authoritative-empty — that would let a partial or
	// forward-compatible read erase live observations. Such frames fall back to
	// the same no-op the old early-return produced.
	//
	// sawNonEmptyFullContainer records that SOME full-snapshot container was
	// non-empty (len>0). A dual-container read (`rateLimits:{}` alongside a
	// non-empty `rateLimitsByLimitId` that happens to recognise nothing) carried
	// real content and must not count as authoritative-empty just because one of
	// its containers was `{}`.
	sawEmptyFullContainer := false
	sawNonEmptyFullContainer := false
	addContributor := func(window, limit string, b codexRateLimitBucket) {
		if out[window] == nil {
			out[window] = map[string]codexRateLimitBucket{}
		}
		// Same (window, limit) appearing twice in a single frame (e.g. once via
		// `rate_limits` aggregate and once via nested `rateLimitsByLimitId`)
		// is folded into the most constrained view, matching how the previous
		// flat extractor handled intra-frame conflicts.
		prev, exists := out[window][limit]
		if !exists {
			out[window][limit] = b
			return
		}
		merged := map[string]codexRateLimitBucket{limit: prev}
		mergeCodexBucketMostConstrained(merged, limit, b)
		out[window][limit] = merged[limit]
	}

	type candidate struct {
		src          map[string]interface{}
		fullSnapshot bool
	}
	candidates := []candidate{{src: raw, fullSnapshot: false}}
	// Top-level `msg` / `payload` envelopes: the app-server emits typed events
	// outside of a JSON-RPC request/response pair as
	// `{"id":"…","msg":{"type":"token_count", … "rate_limits": …}}` and a
	// session-event variant `{"payload":{…}}`. These never carry a full
	// account/rateLimits/read snapshot (those arrive under `result`), so
	// fullSnapshot stays false — a `null` window here means "no update," not
	// "clear it," matching the params-side notification semantics.
	for _, key := range []string{"msg", "payload"} {
		if v, ok := raw[key]; ok {
			if m, ok := v.(map[string]interface{}); ok {
				candidates = append(candidates, candidate{src: m, fullSnapshot: false})
			}
		}
	}
	for _, key := range []string{"params", "result"} {
		v, ok := raw[key]
		if !ok {
			continue
		}
		m, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		fullSnap := key == "result"
		candidates = append(candidates, candidate{src: m, fullSnapshot: fullSnap})
		// `params.msg` / `result.msg` is the shape codex's app-server uses for
		// typed event payloads (e.g. `token_count`) carried inside a JSON-RPC
		// frame. Such a `msg` payload is an inherently SPARSE typed event — it
		// only restates the window(s) it currently observed. The authoritative
		// `account/rateLimits/read` snapshot puts `rate_limits` DIRECTLY under
		// `result`, not under `result.msg`; so a `msg` payload must stay
		// fullSnapshot=false even when it rides inside a `result` envelope,
		// otherwise a sparse `result.msg` `token_count` that only restates
		// `primary` would wrongly prune a live cached weekly window via the
		// omission reconcile.
		if v, ok := m["msg"]; ok {
			if mm, ok := v.(map[string]interface{}); ok {
				candidates = append(candidates, candidate{src: mm, fullSnapshot: false})
			}
		}
	}

	for _, c := range candidates {
		if v, ok := pickField(c.src, "rate_limits", "rateLimits"); ok {
			if rl, ok := v.(map[string]interface{}); ok {
				if c.fullSnapshot {
					fullSnapshot = true
					if len(rl) == 0 {
						sawEmptyFullContainer = true
					} else {
						sawNonEmptyFullContainer = true
					}
				}
				for window, val := range rl {
					id, ok := codexWindowAliases[window]
					if !ok {
						continue
					}
					if val == nil {
						if c.fullSnapshot {
							clears[id] = true
						}
						continue
					}
					info, ok := val.(map[string]interface{})
					if !ok {
						continue
					}
					b, ok := codexBucketFromInfo(info, now)
					if !ok {
						continue
					}
					addContributor(id, codexLegacyLimitID, b)
				}
			}
		}
		// Multi-bucket shape: `rateLimitsByLimitId` is the documented per-
		// metered-limit view (`codex_primary`, `codex_secondary`, `codex_other`,
		// …). When present, each entry may carry a tighter quota than the
		// legacy aggregate `rate_limits` view — e.g. an `codex_other` bucket
		// constraining the same 5-hour or weekly window — so we must fold
		// these into our two display windows and keep the HIGHEST observed
		// utilisation per window. Skipping this would silently understate
		// usage when the legacy view is the looser of the two.
		if v, ok := pickField(c.src, "rate_limits_by_limit_id", "rateLimitsByLimitId"); ok {
			if rl, ok := v.(map[string]interface{}); ok {
				if c.fullSnapshot {
					fullSnapshot = true
					if len(rl) == 0 {
						sawEmptyFullContainer = true
					} else {
						sawNonEmptyFullContainer = true
					}
				}
				for limitKey, val := range rl {
					info, ok := val.(map[string]interface{})
					if !ok {
						continue
					}
					// Documented shape: each `rateLimitsByLimitId` entry is an
					// object that nests window buckets under `primary` /
					// `secondary` (e.g. `codex_other.primary.usedPercent`).
					// When those nested keys are present, iterate them and
					// classify via the window alias table so a strict
					// `codex_other.primary` bucket actually constrains our
					// primary display window. Fall back to treating the entry
					// as a flat bucket (legacy/observed shape) only when no
					// nested window key matched, so neither shape silently
					// goes ignored.
					nestedMatched := false
					for nestedKey, nestedVal := range info {
						id, isWindow := codexWindowAliases[strings.ToLower(nestedKey)]
						if !isWindow {
							continue
						}
						// `rateLimitsByLimitId.<limit>.secondary: null` in a full
						// snapshot means this metered limit no longer constrains
						// that window. Treat it as a clear so a previously-cached
						// bucket can't keep rendering stale usage. Skip nulls in
						// non-snapshot frames (notifications are sparse — null
						// there means "no update," not "clear it").
						if nestedVal == nil {
							if c.fullSnapshot {
								nestedMatched = true
								clears[id] = true
							}
							continue
						}
						nestedInfo, ok := nestedVal.(map[string]interface{})
						if !ok {
							continue
						}
						nb, ok := codexBucketFromInfo(nestedInfo, now)
						if !ok {
							continue
						}
						nestedMatched = true
						addContributor(id, limitKey, nb)
					}
					if nestedMatched {
						continue
					}
					b, ok := codexBucketFromInfo(info, now)
					if !ok {
						continue
					}
					id := classifyCodexByLimitBucket(limitKey, b.WindowMinutes)
					if id == "" {
						continue
					}
					addContributor(id, limitKey, b)
				}
			}
		}
	}

	// On a full snapshot, the present-set is every (IDENTITY, limit id) contributor
	// the frame actually reported a bucket for. It is keyed by identity — not
	// storage slot — so the merger can drop a stale contributor that migrated into a
	// slot the snapshot still uses for a DIFFERENT identity (e.g. a lingering weekly
	// copy under `primary` while the snapshot only restated the primary session).
	// It is ALSO keyed by limit id, not identity alone: a full snapshot enumerates
	// every metered limit that currently constrains a window, so an authoritative
	// read that restates weekly limit `codex_weekly_b` but omits a previously-cached
	// `codex_weekly_a` is declaring `a` gone — identity-only keying would keep `a`
	// (its `weekly` identity is still present via `b`) and let most-constrained
	// folding resurrect its stale usage. A contributor absent from the full snapshot
	// is being omitted by a complete picture, so it is reconciled as "gone". A nulled
	// window is NOT added to present: its clear already removes the slot, and leaving
	// it out lets any copy that migrated elsewhere be dropped too. Sparse frames leave
	// present empty (unused) — they never enumerate the full set of limits.
	if fullSnapshot {
		for slot, contribs := range out {
			for limit, b := range contribs {
				present[codexWindowIdentity(b.WindowMinutes, slot)+"\x00"+limit] = true
			}
		}
	}
	// Authoritative-empty: a full snapshot carried an empty container, NO non-empty
	// container, and produced no buckets and no explicit clears. Only then may the
	// merger clear the whole cache. A dual-container read whose other container was
	// non-empty (even if unrecognised), or any frame that extracted buckets/clears,
	// drives reconciliation through those signals instead of clearing everything.
	emptyAuthoritative := sawEmptyFullContainer && !sawNonEmptyFullContainer &&
		len(out) == 0 && len(clears) == 0
	return out, clears, fullSnapshot, present, emptyAuthoritative
}

// aggregateCodexBuckets folds per-(window, limit) contributors into a single
// most-constrained bucket per window. Used both by tests of the extractor and
// at write/read time to derive the flat `Buckets` field rendered on the card.
//
// A contributor whose reset is strictly in the past is treated as having
// rolled over to 0% used: its prior utilisation is stale (Codex would emit a
// fresh telemetry frame at the start of the new window), and leaving the
// stale percentage in the merge would let it shadow a live contributor that
// has a smaller usage but a real future reset. The reset itself is preserved
// so codexMetricFromBucket still recognises the rollover at display
// time when no other contributor is live.
func aggregateCodexBuckets(perLimit map[string]map[string]codexRateLimitBucket, now time.Time) map[string]codexRateLimitBucket {
	out := map[string]codexRateLimitBucket{}
	nowMs := now.UnixMilli()
	for window, contributors := range perLimit {
		for _, b := range contributors {
			if b.ResetsAtMs > 0 && nowMs >= b.ResetsAtMs {
				b.UsedPercentage = 0
				b.rolledOver = true
			}
			mergeCodexBucketMostConstrained(out, window, b)
		}
	}
	return out
}

// mergeCodexBucketMostConstrained folds `b` into `out[id]`, keeping the most
// constrained view of the display window across multiple metered buckets:
//
//   - UsedPercentage is the MAX of the two (the user feels the strictest
//     bucket's throttle right now);
//   - ResetsAtMs is the LATER of the two when both are known — the display
//     window is only fully cleared once EVERY contributing bucket has reset,
//     so expiring at the earlier reset would zero the window while the
//     runner-up bucket is still live and contributing usage. On a usage tie
//     with only one reset known, the reset is dropped (we can't promise a
//     time the unknown side can't confirm). When the bucket driving the
//     displayed usage has no reset of its own, we drop the reset entirely
//     rather than borrowing the lower-usage bucket's reset — otherwise the
//     UI would clear the stricter bucket at a time it hasn't confirmed.
//
// Window-length hints are preserved from either side.
func mergeCodexBucketMostConstrained(out map[string]codexRateLimitBucket, id string, b codexRateLimitBucket) {
	prev, exists := out[id]
	if !exists {
		out[id] = b
		return
	}
	if !b.usageKnown {
		return
	}

	merged := prev
	usageDrivenByB := !prev.usageKnown || b.UsedPercentage > prev.UsedPercentage
	if usageDrivenByB {
		merged.UsedPercentage = b.UsedPercentage
		merged.usageKnown = true
		merged.ObservedAtMs = b.ObservedAtMs
		merged.rolledOver = b.rolledOver
	}

	usageTie := prev.usageKnown && b.UsedPercentage == prev.UsedPercentage
	switch {
	case b.resetKnown && prev.resetKnown:
		if b.ResetsAtMs > prev.ResetsAtMs {
			merged.ResetsAtMs = b.ResetsAtMs
		} else if prev.ResetsAtMs > b.ResetsAtMs {
			merged.ResetsAtMs = prev.ResetsAtMs
		} else {
			merged.ResetsAtMs = prev.ResetsAtMs
		}
		merged.resetKnown = true
		if usageTie {
			// The later reset controls when the aggregate clears, but all live
			// tied contributors are equivalent evidence for the displayed usage.
			// Keep the freshest live observation; never borrow freshness from a
			// contributor whose window has already rolled over.
			switch {
			case prev.rolledOver && !b.rolledOver:
				merged.ObservedAtMs = b.ObservedAtMs
			case !prev.rolledOver && b.rolledOver:
				merged.ObservedAtMs = prev.ObservedAtMs
			case b.ObservedAtMs > prev.ObservedAtMs:
				merged.ObservedAtMs = b.ObservedAtMs
			default:
				merged.ObservedAtMs = prev.ObservedAtMs
			}
			// Preserve aggregate provenance for later contributors: a tied
			// aggregate is rolled over only when every contributor is.
			merged.rolledOver = prev.rolledOver && b.rolledOver
		}
	case usageTie:
		// Same exhaustion, only one side has a reset hint — don't promise a
		// time the unknown side can't confirm; render "—" instead. Keep the
		// observation paired with the live contributor when the other side has
		// rolled over. Otherwise both values are current evidence, so use the
		// freshest tied observation.
		merged.ResetsAtMs = 0
		merged.resetKnown = false
		switch {
		case prev.rolledOver && !b.rolledOver:
			merged.ObservedAtMs = b.ObservedAtMs
		case !prev.rolledOver && b.rolledOver:
			merged.ObservedAtMs = prev.ObservedAtMs
		case b.ObservedAtMs > prev.ObservedAtMs:
			merged.ObservedAtMs = b.ObservedAtMs
		}
		// The aggregate remains rolled over only when every equally
		// constrained contributor is rolled over. This provenance must fold
		// alongside the timestamp so later contributors see the true state.
		merged.rolledOver = prev.rolledOver && b.rolledOver
	case usageDrivenByB && b.resetKnown:
		merged.ResetsAtMs = b.ResetsAtMs
		merged.resetKnown = true
	case !usageDrivenByB && prev.resetKnown:
		merged.ResetsAtMs = prev.ResetsAtMs
		merged.resetKnown = true
	default:
		// Stricter bucket has no reset of its own; do not borrow the
		// lower-usage bucket's reset.
		merged.ResetsAtMs = 0
		merged.resetKnown = false
	}

	if merged.WindowMinutes == 0 {
		switch {
		case prev.WindowMinutes > 0:
			merged.WindowMinutes = prev.WindowMinutes
		case b.WindowMinutes > 0:
			merged.WindowMinutes = b.WindowMinutes
		}
	}

	out[id] = merged
}

// classifyCodexByLimitBucket maps a `rateLimitsByLimitId` entry onto one of our
// two display windows (primary = 5-hour, secondary = weekly) using its key and
// window length. Unknown buckets (e.g. `codex_other` with no window hint) are
// classified by length: <= 6h → primary, > 6h → secondary; entries without any
// window hint are dropped rather than misattributed.
func classifyCodexByLimitBucket(limitKey string, windowMinutes float64) string {
	k := strings.ToLower(limitKey)
	switch {
	case strings.Contains(k, "primary"), strings.Contains(k, "5h"), strings.Contains(k, "five_hour"), strings.Contains(k, "session"):
		return codexWindowPrimary
	case strings.Contains(k, "secondary"), strings.Contains(k, "weekly"), strings.Contains(k, "7d"), strings.Contains(k, "seven_day"):
		return codexWindowSecondary
	}
	if windowMinutes > 0 {
		if windowMinutes <= 360 {
			return codexWindowPrimary
		}
		return codexWindowSecondary
	}
	return ""
}

// Window-length bands shared by codexWindowLabel (display text) and
// codexWindowIdentity (metric identity). Codex's `token_count` JSONL often
// reports the canonical windows with a floored/rounded minute count (299 for
// the 5-hour window, 10079 for the weekly window — see openai/codex#14728), so
// each canonical window is matched by a small tolerant band rather than an exact
// value. The bands are disjoint from any neighbouring real Codex window
// (4h=240, 6h=360, 6-day=8640, biweekly=20160) so a genuinely different plan
// length is NOT collapsed into session/weekly. This is deliberately separate
// from classifyCodexByLimitBucket's coarse ≤360-minute STORAGE-slot routing:
// that decides which on-disk slot a metered limit lands in; these bands decide
// which metric a reading actually IS for dedupe, labelling, and row placement.
const (
	codexSessionBandMinMinutes = 295
	codexSessionBandMaxMinutes = 305
	codexWeeklyBandMinMinutes  = 10020
	codexWeeklyBandMaxMinutes  = 10140
)

// codexMinutesInSessionBand reports whether a window length is the canonical
// rolling 5-hour session window (within the tolerant band above).
func codexMinutesInSessionBand(minutes float64) bool {
	if minutes <= 0 {
		return false
	}
	m := int(minutes + 0.5)
	return m >= codexSessionBandMinMinutes && m <= codexSessionBandMaxMinutes
}

// codexMinutesInWeeklyBand reports whether a window length is the canonical
// weekly window (within the tolerant band above).
func codexMinutesInWeeklyBand(minutes float64) bool {
	if minutes <= 0 {
		return false
	}
	m := int(minutes + 0.5)
	return m >= codexWeeklyBandMinMinutes && m <= codexWeeklyBandMaxMinutes
}

// Metric identities used to reconcile duplicate observations and to place the
// two Claude-aligned rows. Identity is authoritative for rows, labels,
// newest-wins, and cache supersession; storage slot (primary/secondary) is only
// where a reading physically sits on disk. A non-canonical window keeps a
// distinct `duration:<minutes>` identity so two different off-spec plans don't
// collapse into one.
const (
	codexIdentitySession        = "session"
	codexIdentityWeekly         = "weekly"
	codexDurationIdentityPrefix = "duration:"
)

// codexWindowIdentity classifies one observed window into its metric identity.
// Canonical session/weekly bands win first; a positive but non-canonical length
// keeps its own `duration:<minutes>` identity (so distinct off-spec plans stay
// separate); a length-less reading falls back to its storage slot's default
// (primary → session, secondary → weekly) because Codex legitimately omits
// `window_minutes` on primary/secondary frames — that keeps the mainline
// "5-hour session window" / "Weekly quota" path intact. Note only a POSITIVELY
// weekly-band (or otherwise non-session) reading is barred from the session row
// (AC4); a duration-less primary is still a known session by slot-default.
func codexWindowIdentity(windowMinutes float64, slot string) string {
	switch {
	case codexMinutesInSessionBand(windowMinutes):
		return codexIdentitySession
	case codexMinutesInWeeklyBand(windowMinutes):
		return codexIdentityWeekly
	}
	if windowMinutes > 0 {
		return fmt.Sprintf("%s%d", codexDurationIdentityPrefix, int(windowMinutes+0.5))
	}
	if slot == codexWindowSecondary {
		return codexIdentityWeekly
	}
	return codexIdentitySession
}

// codexIdentityContribution is one on-disk contributor tagged with the metric
// identity it belongs to and the storage slot it physically sits in. Display
// and cache reconciliation partition these by identity — never by raw slot — so
// a weekly reading that migrated to the `primary` slot is still recognised as
// the same weekly metric as the one under `secondary`.
type codexIdentityContribution struct {
	slot     string
	limitID  string
	identity string
	bucket   codexRateLimitBucket
}

// codexPartitionByIdentity walks every contributor across both storage slots and
// groups them by metric identity. Legacy flat caches surface as a single
// `__legacy__` contributor per slot before reaching here, so both cache shapes
// partition identically.
func codexPartitionByIdentity(contributors map[string]map[string]codexRateLimitBucket) map[string][]codexIdentityContribution {
	parts := map[string][]codexIdentityContribution{}
	for slot, contribs := range contributors {
		for limitID, b := range contribs {
			id := codexWindowIdentity(b.WindowMinutes, slot)
			parts[id] = append(parts[id], codexIdentityContribution{
				slot:     slot,
				limitID:  limitID,
				identity: id,
				bucket:   b,
			})
		}
	}
	return parts
}

// codexPlacementBeats gives a total order for two placements of the SAME
// (identity, limit id) sitting under different storage slots: the freshest
// observation wins; on an equal timestamp a stated observation beats an inferred
// one, then the higher usage wins; then a fixed primary-over-secondary slot
// precedence; finally a known-usage reading beats an unknown one. Freshness
// dominates so an older duplicate is discarded even when its used % is higher
// (AC1 / stale-observation), and no branch depends on Go map iteration order.
func codexPlacementBeats(aBucket codexRateLimitBucket, aSlot string, bBucket codexRateLimitBucket, bSlot string) bool {
	if aBucket.ObservedAtMs != bBucket.ObservedAtMs {
		return aBucket.ObservedAtMs > bBucket.ObservedAtMs
	}
	if aBucket.Inferred != bBucket.Inferred {
		return !aBucket.Inferred
	}
	if aBucket.UsedPercentage != bBucket.UsedPercentage {
		return aBucket.UsedPercentage > bBucket.UsedPercentage
	}
	if aSlot != bSlot {
		return aSlot == codexWindowPrimary
	}
	return aBucket.usageKnown && !bBucket.usageKnown
}

// codexSurvivingContributions reconciles one metric identity's contributors down
// to the placements that should feed display and remain on disk. Two rules,
// applied together, keep it sparse-safe:
//
//   - Per limit id, the newest cross-slot PLACEMENT wins. When the SAME metered
//     limit is restated under a new storage slot (a migration), the old-slot copy
//     of that limit is superseded — but a DISTINCT limit id that only exists on
//     the other slot is NOT retracted (sparse frames never drop a limit they
//     didn't mention), so it still contributes to the most-constrained fold.
//   - The coarse `__legacy__` aggregate contributor is dropped only when a
//     STRICTLY NEWER non-legacy placement of the same identity exists: once a
//     named `codex_*` limit restates the metric more recently, the aggregate
//     view is a stale duplicate. Within a single frame (equal ObservedAtMs) the
//     aggregate and named views coexist and fold most-constrained, so a genuinely
//     stricter aggregate is never silently discarded.
//
// Returned in sorted-limit-id order so downstream folding is deterministic and
// never depends on Go map iteration order.
func codexSurvivingContributions(contribs []codexIdentityContribution) []codexIdentityContribution {
	winners := map[string]codexIdentityContribution{}
	for _, c := range contribs {
		cur, ok := winners[c.limitID]
		if !ok || codexPlacementBeats(c.bucket, c.slot, cur.bucket, cur.slot) {
			winners[c.limitID] = c
		}
	}
	if legacy, ok := winners[codexLegacyLimitID]; ok {
		for id, w := range winners {
			if id == codexLegacyLimitID {
				continue
			}
			if w.bucket.ObservedAtMs > legacy.bucket.ObservedAtMs {
				delete(winners, codexLegacyLimitID)
				break
			}
		}
	}
	ids := make([]string, 0, len(winners))
	for id := range winners {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]codexIdentityContribution, 0, len(ids))
	for _, id := range ids {
		out = append(out, winners[id])
	}
	return out
}

// codexAggregateIdentity folds one identity partition into a single display
// bucket. Contributors are first reconciled by codexSurvivingContributions
// (newest placement per limit id; stale __legacy__ aggregate dropped), then the
// survivors fold most-constrained across DISTINCT metered limits so a stricter
// concurrent limit still tightens the window — even when the surviving limits sit
// on different storage slots. A contributor whose reset has already passed is
// zeroed for the comparison, matching aggregateCodexBuckets, so a stale-but-high
// reading can't shadow a live one.
func codexAggregateIdentity(contribs []codexIdentityContribution, now time.Time) (codexRateLimitBucket, bool) {
	if len(contribs) == 0 {
		return codexRateLimitBucket{}, false
	}
	const key = "identity"
	out := map[string]codexRateLimitBucket{}
	nowMs := now.UnixMilli()
	for _, c := range codexSurvivingContributions(contribs) {
		b := c.bucket
		if b.ResetsAtMs > 0 && nowMs >= b.ResetsAtMs {
			b.UsedPercentage = 0
			b.rolledOver = true
		}
		mergeCodexBucketMostConstrained(out, key, b)
	}
	res, ok := out[key]
	return res, ok
}

// codexIdentityDisplayBucket selects the bucket for one Claude-aligned row. It
// prefers the canonical band / slot-default identity for the row (session or
// weekly). When neither exists, a non-canonical `duration:*` observation sourced
// from this row's storage slot may fill it — preserving the existing
// duration-derived labels (e.g. a 15-minute primary window) without ever
// promoting a weekly-band reading into the session row. The layout kind is fixed
// by the caller; only the label reflects the real duration.
func codexIdentityDisplayBucket(parts map[string][]codexIdentityContribution, identity, fallbackSlot string, now time.Time) (codexRateLimitBucket, bool) {
	if contribs, ok := parts[identity]; ok && len(contribs) > 0 {
		return codexAggregateIdentity(contribs, now)
	}
	bestID := ""
	var bestMs int64 = -1
	for id, contribs := range parts {
		if !strings.HasPrefix(id, codexDurationIdentityPrefix) {
			continue
		}
		for _, c := range contribs {
			if c.slot != fallbackSlot {
				continue
			}
			if c.bucket.ObservedAtMs > bestMs ||
				(c.bucket.ObservedAtMs == bestMs && (bestID == "" || id < bestID)) {
				bestMs = c.bucket.ObservedAtMs
				bestID = id
			}
		}
	}
	if bestID == "" {
		return codexRateLimitBucket{}, false
	}
	scoped := make([]codexIdentityContribution, 0, len(parts[bestID]))
	for _, c := range parts[bestID] {
		if c.slot == fallbackSlot {
			scoped = append(scoped, c)
		}
	}
	return codexAggregateIdentity(scoped, now)
}

// codexReconcileIdentitySupersession removes stale duplicates of the same metric
// so no two versions of a window ever linger in the cache. It partitions every
// contributor by identity and keeps exactly the placements
// codexSurvivingContributions selects, so display and on-disk state agree:
//
//   - the SAME limit id restated under a new storage slot supersedes its old-slot
//     copy (newest placement wins);
//   - a stale `__legacy__` aggregate is dropped once a strictly-newer named limit
//     of the same identity exists (the cross-shape migration case).
//
// Crucially it is SPARSE-SAFE: a DISTINCT metered limit that this frame did not
// mention — e.g. weekly limit A under `secondary` while a sparse frame only
// restated weekly limit B under `primary` — is left in place, so it still
// contributes to the most-constrained fold instead of being silently retracted.
// Walks slots/limit ids in sorted order so the outcome never depends on Go map
// iteration order.
func codexReconcileIdentitySupersession(contributors map[string]map[string]codexRateLimitBucket) {
	slots := make([]string, 0, len(contributors))
	for slot := range contributors {
		slots = append(slots, slot)
	}
	sort.Strings(slots)

	byIdentity := map[string][]codexIdentityContribution{}
	for _, slot := range slots {
		contribs := contributors[slot]
		limitIDs := make([]string, 0, len(contribs))
		for id := range contribs {
			limitIDs = append(limitIDs, id)
		}
		sort.Strings(limitIDs)
		for _, limitID := range limitIDs {
			b := contribs[limitID]
			id := codexWindowIdentity(b.WindowMinutes, slot)
			byIdentity[id] = append(byIdentity[id], codexIdentityContribution{
				slot: slot, limitID: limitID, identity: id, bucket: b,
			})
		}
	}

	survive := map[string]bool{}
	for _, contribs := range byIdentity {
		for _, s := range codexSurvivingContributions(contribs) {
			survive[s.slot+"\x00"+s.limitID] = true
		}
	}
	for slot, contribs := range contributors {
		for limitID := range contribs {
			if !survive[slot+"\x00"+limitID] {
				delete(contribs, limitID)
			}
		}
		if len(contribs) == 0 {
			delete(contributors, slot)
		}
	}
}

// captureCodexRateLimitLine parses one stdout line from a Codex app-server
// session and, if it carries `token_count` rate-limit telemetry, merges it
// into the on-disk cache. Best-effort: every failure is silent (this runs in
// the hot streaming path and must never break a session). Reports whether a
// capture landed (captureCodexRateLimitLineForAccount).
func captureCodexRateLimitLine(line string, now time.Time) bool {
	return captureCodexRateLimitLineForAccount(line, now, currentCodexAccountFingerprint())
}

// captureCodexRateLimitLineForAccount is captureCodexRateLimitLine with the
// account named by the caller rather than re-read from disk at receipt. The
// live probe uses it to pin a reading to the account its child was spawned
// under, and the smoke to the account its turn ran under.
//
// Reports true when the merge committed a numeric window that advanced an
// observation, or an authoritative clear; false for a dropped, unrecognised,
// stale or reset-only frame. The session paths ignore it; the smoke settles
// its run on it.
func captureCodexRateLimitLineForAccount(line string, now time.Time, fingerprint string) bool {
	return captureCodexRateLimitLineFromProducer(line, now, fingerprint, currentCodexUsageCaptureVersion())
}

// captureCodexRateLimitLineFromProducer is captureCodexRateLimitLineForAccount
// with the producing binary's version named by the caller. A long-lived child
// (a managed session or app-server) outlives a Codex upgrade, so its frames
// must carry the version pinned when it was spawned, not whatever build a
// later gather published; "" (unknown producer) leaves the stamp untouched.
func captureCodexRateLimitLineFromProducer(line string, now time.Time, fingerprint, producerVersion string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	if !codexRateLimitLineCandidate(trimmed) {
		return false
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return false
	}
	if !isRecognizedCodexRateLimitEnvelope(raw) {
		return false
	}
	anchor, observedAt := codexObservationTimes(raw, now, true)
	updates, clears, fullSnapshot, present, emptyAuthoritative := extractCodexRateLimitBucketsFull(raw, anchor)
	codexStampContributorObservations(updates, observedAt)
	updates = codexCanonicalizeContributors(updates)
	// A full snapshot must be processed even when it carries no buckets and no
	// explicit nulls IF it is authoritative-empty: an `account/rateLimits/read`
	// response whose container is literally `{}` declares the account now has NO
	// quota windows, so every cached observation has to be reconciled away. A full
	// snapshot that is non-empty but yields nothing recognised (unknown keys,
	// unparseable buckets, forward-compatible fields) is NOT authoritative-empty
	// and is dropped here, exactly like a sparse frame with nothing to say — it
	// must never erase live observations.
	if len(updates) == 0 && len(clears) == 0 && !emptyAuthoritative {
		return false
	}
	committed, advanced := mergeCodexRateLimitCacheObserved(
		context.Background(), codexRateLimitCachePath(), updates, clears, fullSnapshot, present, emptyAuthoritative,
		now, fingerprint, nil, "", true, extractCodexLimitNames(raw), producerVersion)
	return advanced || (committed && (len(clears) > 0 || emptyAuthoritative))
}

// codexRateLimitLineCandidate is the cheap prefilter in front of the JSON
// decode: only a line that could plausibly carry rate-limit telemetry is worth
// parsing. `token_count` wraps the legacy notification;
// `account/rateLimits/{read,updated}` is the newer JSON-RPC surface;
// `rate_limits`/`rateLimits` cover both payload key spellings. Anything else
// can't carry a window update for us. Shared with the smoke, which applies it
// before retaining a stdout line at all.
func codexRateLimitLineCandidate(line string) bool {
	return strings.Contains(line, "token_count") ||
		strings.Contains(line, "rateLimits") ||
		strings.Contains(line, "rate_limit")
}

// extractCodexLimitNames returns the display name of every metered limit a
// frame describes under `rateLimitsByLimitId`, keyed by limit id. A limit whose
// name is explicitly null or empty maps to "" so an authoritative snapshot can
// forget a name the provider stopped sending; a limit that omits the field is
// left out entirely, so a sparse update that does not restate the name keeps
// the cached one. Only JSON strings are accepted and each is bounded, so an
// unexpected value can never reach a metric label.
func extractCodexLimitNames(raw map[string]interface{}) map[string]string {
	names := map[string]string{}
	containers := []map[string]interface{}{raw}
	for _, key := range []string{"params", "result", "msg", "payload"} {
		if m, ok := raw[key].(map[string]interface{}); ok {
			containers = append(containers, m)
			if msg, ok := m["msg"].(map[string]interface{}); ok {
				containers = append(containers, msg)
			}
		}
	}
	for _, c := range containers {
		v, ok := pickField(c, "rate_limits_by_limit_id", "rateLimitsByLimitId")
		if !ok {
			continue
		}
		limits, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		for limitID, entry := range limits {
			info, ok := entry.(map[string]interface{})
			if !ok {
				continue
			}
			v, ok := pickField(info, "limit_name", "limitName")
			if !ok {
				continue
			}
			// Only an explicit null or empty string clears a cached name. A value
			// of any other type (a future schema change) is not evidence the pool
			// lost its name, so skip it rather than folding a live named pool back
			// into the main one.
			switch s := v.(type) {
			case nil:
				names[limitID] = ""
			case string:
				names[limitID] = clampAntigravityQuotaField(strings.TrimSpace(s), codexLimitNameMaxBytes)
			}
		}
	}
	return names
}

// codexLimitNameMaxBytes bounds a pool name before it is composed into a metric
// label ("<name> — Weekly quota"), which the receipt caps at 256 bytes.
const codexLimitNameMaxBytes = 96

// isRecognizedCodexRateLimitEnvelope fails closed before typed extraction.
// Rate-limit-looking fields inside arbitrary tool output, prompts, or response
// bodies must never become provider evidence.
func isRecognizedCodexRateLimitEnvelope(raw map[string]interface{}) bool {
	if method, _ := raw["method"].(string); method == "token_count" || strings.HasPrefix(method, "account/rateLimits/") {
		return true
	}
	if eventType, _ := raw["type"].(string); eventType == "token_count" {
		return true
	}
	for _, key := range []string{"msg", "payload"} {
		if nested, ok := raw[key].(map[string]interface{}); ok {
			if eventType, _ := nested["type"].(string); eventType == "token_count" {
				return true
			}
		}
	}
	if params, ok := raw["params"].(map[string]interface{}); ok {
		if msg, ok := params["msg"].(map[string]interface{}); ok {
			if eventType, _ := msg["type"].(string); eventType == "token_count" {
				return true
			}
		}
	}
	// A direct result container is the response shape for
	// account/rateLimits/read. Requiring JSON-RPC framing prevents an arbitrary
	// application object containing `result.rateLimits` from being trusted.
	if raw["jsonrpc"] == "2.0" {
		if result, ok := raw["result"].(map[string]interface{}); ok {
			if _, ok := pickField(result, "rate_limits", "rateLimits", "rate_limits_by_limit_id", "rateLimitsByLimitId"); ok {
				return true
			}
			if msg, ok := result["msg"].(map[string]interface{}); ok {
				if eventType, _ := msg["type"].(string); eventType == "token_count" {
					return true
				}
			}
		}
	}
	return false
}

// codexObservationTimes returns the event-time anchor used for relative reset
// conversion and the safe publication timestamp. Live envelopes commonly omit
// an event time, so allowFallback uses receive time. Rollout callers pass false:
// numeric evidence without an enclosing event timestamp is unrankable and must
// be discarded. Provider clocks up to five minutes ahead are accepted as reset
// anchors while their published observation is clamped to receive time.
func codexObservationTimes(raw map[string]interface{}, now time.Time, allowFallback bool) (time.Time, time.Time) {
	ts, _ := raw["timestamp"].(string)
	if ts == "" {
		if allowFallback {
			return now, now
		}
		return time.Time{}, time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil || parsed.After(now.Add(codexProviderObservationFutureTolerance)) {
		if allowFallback {
			return now, now
		}
		return time.Time{}, time.Time{}
	}
	if parsed.After(now) {
		return parsed, now
	}
	return parsed, parsed
}

func codexStampContributorObservations(contributors map[string]map[string]codexRateLimitBucket, observedAt time.Time) {
	if observedAt.IsZero() {
		return
	}
	for window, limits := range contributors {
		for limitID, bucket := range limits {
			bucket.ObservedAtMs = observedAt.UnixMilli()
			contributors[window][limitID] = bucket
		}
	}
}

// codexCanonicalizeContributors normalizes the two displayed identities
// before the slot-keyed cache merge. Codex can report a weekly bucket under
// `primary` (and vice versa); merging that physical key directly can overwrite a
// cached session contributor with the same limit id before identity reconciliation
// gets a chance to preserve both. Apply this to each live or rollout frame before
// sparse merging so successive frames cannot collapse distinct identities first.
func codexCanonicalizeContributors(contributors map[string]map[string]codexRateLimitBucket) map[string]map[string]codexRateLimitBucket {
	type placement struct {
		sourceSlot string
		bucket     codexRateLimitBucket
	}
	placed := map[string]map[string]placement{}
	for sourceSlot, limits := range contributors {
		for limitID, bucket := range limits {
			targetSlot := sourceSlot
			switch codexWindowIdentity(bucket.WindowMinutes, sourceSlot) {
			case codexIdentitySession:
				targetSlot = codexWindowPrimary
			case codexIdentityWeekly:
				targetSlot = codexWindowSecondary
			}
			if placed[targetSlot] == nil {
				placed[targetSlot] = map[string]placement{}
			}
			previous, exists := placed[targetSlot][limitID]
			if !exists || codexPlacementBeats(bucket, sourceSlot, previous.bucket, previous.sourceSlot) {
				placed[targetSlot][limitID] = placement{sourceSlot: sourceSlot, bucket: bucket}
			}
		}
	}

	out := make(map[string]map[string]codexRateLimitBucket, len(placed))
	for slot, limits := range placed {
		out[slot] = make(map[string]codexRateLimitBucket, len(limits))
		for limitID, p := range limits {
			out[slot][limitID] = p.bucket
		}
	}
	return out
}

// mergeCodexRateLimitCache is the flat-shape entry point preserved for callers
// (and tests) that already aggregated their updates by display window. Each
// flat entry is recorded as a single contributor under codexLegacyLimitID so
// the on-disk schema stays per-limit and the per-(window, limit) sparse-merge
// logic still applies.
func mergeCodexRateLimitCache(path string, updates map[string]codexRateLimitBucket, clears map[string]bool, now time.Time, fingerprint string) {
	if len(updates) == 0 && len(clears) == 0 {
		return
	}
	perLimit := make(map[string]map[string]codexRateLimitBucket, len(updates))
	for window, bucket := range updates {
		if bucket.ObservedAtMs <= 0 {
			bucket.ObservedAtMs = now.UnixMilli()
		}
		perLimit[window] = map[string]codexRateLimitBucket{codexLegacyLimitID: bucket}
	}
	// Flat callers are pre-aggregated sparse updates, never full snapshots, so
	// no full-snapshot omission reconcile applies (present/emptyAuthoritative unused).
	mergeCodexRateLimitCachePerLimit(path, perLimit, clears, false, nil, false, now, fingerprint)
}

// mergeCodexRateLimitCachePerLimit read-modify-writes the cache, preserving
// the per-(window, limit-id) contributors map so a sparse
// `account/rateLimits/updated` for one metered limit (e.g. `codex_primary`)
// does not silently clobber a stricter prior contributor (e.g. `codex_other`)
// the sparse frame never restated. The flat `Buckets` field is re-aggregated
// from the contributors on every write so the read path stays unchanged.
//
// Windows named in `clears` are dropped entirely (every contributor) —
// Codex uses `secondary: null` in a full account/rateLimits/read response to
// mean "this window does not apply to the account." When the existing
// snapshot was captured under a different account fingerprint, all buckets
// and contributors are discarded so a previous account's reset times can't
// bleed into the new account's display.
// After the per-field merges, two identity-level reconciliations run so no two
// versions of the same metric ever linger: same-identity supersession removes a
// stale duplicate of the same window that migrated storage slots (sparse-safe),
// and — for full snapshots only — omission reconcile drops any cached window the
// authoritative complete picture no longer mentions. `fullSnapshot`/`present`
// come from extractCodexRateLimitBucketsFull; sparse callers pass
// (false, nil, false). `emptyAuthoritative` marks a full snapshot whose container
// was literally `{}` — the only shape allowed to clear the whole cache.
func mergeCodexRateLimitCachePerLimit(
	path string,
	perLimit map[string]map[string]codexRateLimitBucket,
	clears map[string]bool,
	fullSnapshot bool,
	present map[string]bool,
	emptyAuthoritative bool,
	now time.Time,
	fingerprint string,
) {
	mergeCodexRateLimitCachePerLimitProgress(path, perLimit, clears, fullSnapshot, present, emptyAuthoritative, now, fingerprint, nil, "")
}

// mergeCodexRateLimitCachePerLimitProgress is the shared live/rollout
// reconciler. rolloutHighWater is nil for live capture, which deliberately
// leaves filesystem progress untouched; a non-nil value is committed in the
// same atomic write as newly reconciled rollout contributors.
func mergeCodexRateLimitCachePerLimitProgress(
	path string,
	perLimit map[string]map[string]codexRateLimitBucket,
	clears map[string]bool,
	fullSnapshot bool,
	present map[string]bool,
	emptyAuthoritative bool,
	now time.Time,
	fingerprint string,
	rolloutHighWater *codexRolloutScanProgress,
	rolloutAccountBase string,
) {
	mergeCodexRateLimitCachePerLimitProgressWithLock(
		context.Background(), path, perLimit, clears, fullSnapshot, present, emptyAuthoritative,
		now, fingerprint, rolloutHighWater, rolloutAccountBase, true, nil,
	)
}

// tryMergeCodexRateLimitCachePerLimitProgress performs the rollout cache
// transaction only when both its process-local and cross-process locks are
// immediately available. Rollout scans are optional work under a bounded
// gather; leaving the old cursor untouched is safe because the same normalized
// evidence will be offered again on the next refresh.
func tryMergeCodexRateLimitCachePerLimitProgress(
	path string,
	perLimit map[string]map[string]codexRateLimitBucket,
	clears map[string]bool,
	fullSnapshot bool,
	present map[string]bool,
	emptyAuthoritative bool,
	now time.Time,
	fingerprint string,
	rolloutHighWater *codexRolloutScanProgress,
	rolloutAccountBase string,
) bool {
	return mergeCodexRateLimitCachePerLimitProgressWithLock(
		context.Background(), path, perLimit, clears, fullSnapshot, present, emptyAuthoritative,
		now, fingerprint, rolloutHighWater, rolloutAccountBase, false, nil,
	)
}

func mergeCodexRateLimitCachePerLimitProgressWithLock(
	ctx context.Context,
	path string,
	perLimit map[string]map[string]codexRateLimitBucket,
	clears map[string]bool,
	fullSnapshot bool,
	present map[string]bool,
	emptyAuthoritative bool,
	now time.Time,
	fingerprint string,
	rolloutHighWater *codexRolloutScanProgress,
	rolloutAccountBase string,
	waitForLocks bool,
	limitNames map[string]string,
) bool {
	committed, _ := mergeCodexRateLimitCacheObserved(ctx, path, perLimit, clears, fullSnapshot, present, emptyAuthoritative,
		now, fingerprint, rolloutHighWater, rolloutAccountBase, waitForLocks, limitNames, currentCodexUsageCaptureVersion())
	return committed
}

// mergeCodexRateLimitCacheObserved is the merge transaction itself. advanced
// reports whether the committed write moved a contributor observation forward
// — the one moment the snapshot's CodexVersion stamp is (re)written, so live
// capture, the rollout scan and the live probe all stamp identically, each
// with the producerVersion of the binary that produced the evidence.
func mergeCodexRateLimitCacheObserved(
	ctx context.Context,
	path string,
	perLimit map[string]map[string]codexRateLimitBucket,
	clears map[string]bool,
	fullSnapshot bool,
	present map[string]bool,
	emptyAuthoritative bool,
	now time.Time,
	fingerprint string,
	rolloutHighWater *codexRolloutScanProgress,
	rolloutAccountBase string,
	waitForLocks bool,
	limitNames map[string]string,
	producerVersion string,
) (committed, advanced bool) {
	if path == "" || (len(perLimit) == 0 && len(clears) == 0 && !emptyAuthoritative && rolloutHighWater == nil) {
		return false, false
	}
	committed = codexRateLimitCacheTransaction(ctx, path, now, waitForLocks, func(snap *codexRateLimitSnapshot) bool {
		advanced = false
		// A rollout scan validates the active account before it starts, but auth
		// can change while filesystem I/O is in progress. Revalidate inside the
		// cache transaction locks so a newly signed-in account's live capture
		// cannot be cleared and replaced by the earlier account's rollout
		// contributors. Live capture passes an empty base because its evidence is
		// scoped at receive time and must remain able to initialize or replace the
		// cache.
		if rolloutAccountBase != "" && codexAccountFingerprintAtBase(rolloutAccountBase) != fingerprint {
			return false
		}
		codexScopeSnapshotToAccount(snap, fingerprint)
		// Taken from the same projection the merge migrates a pre-Contributors
		// cache into, so a legacy bucket that is merely migrated is not
		// mistaken for a fresh observation and restamped.
		before := codexContributorObservationTimes(codexContributorsFromSnapshot(*snap))
		codexMergeContributorsIntoSnapshot(snap, perLimit, clears, fullSnapshot, present, emptyAuthoritative, now, fingerprint, rolloutHighWater, rolloutAccountBase, limitNames)
		advanced = codexStampCaptureVersion(snap, before, fullSnapshot && (len(clears) > 0 || emptyAuthoritative), producerVersion)
		return true
	})
	return committed, committed && advanced
}

// codexRateLimitCacheTransaction runs mutate as one read-modify-write of the
// cache file under the in-process mutex and the cross-process advisory lock,
// then writes the result atomically (temp file + rename). mutate returning
// false aborts without writing. Every writer of codex_rate_limits.json goes
// through here — the contributor merge and the run-freshness bookkeeping alike
// — so codexSettleRunFreshness sees, and settles against, every mutation.
//
// waitForLocks=false is the optional-work mode: when either lock is held the
// transaction is skipped rather than waited for.
func codexRateLimitCacheTransaction(ctx context.Context, path string, now time.Time, waitForLocks bool, mutate func(snap *codexRateLimitSnapshot) bool) bool {
	if path == "" {
		return false
	}
	lockDeadline := codexCacheLockDeadline(ctx)
	if waitForLocks {
		if !codexAcquireCacheGate(lockDeadline) {
			return false
		}
	} else if !codexRateLimitMu.TryLock() {
		return false
	}
	defer codexRateLimitMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false
	}
	var lockFile *os.File
	var locked bool
	if waitForLocks {
		// Reuses the shared bounded acquirer (it is generic over the cache path;
		// only its name is Claude's). Confirmed contention REFUSES rather than
		// proceeding unlocked: renaming a snapshot read while another writer was
		// mid-write would clobber whatever that writer went on to commit. Only
		// Unavailable — which carries no evidence of a competitor — takes the
		// degraded unlocked path, matching the previous behaviour on lock error.
		var outcome claudeRateLimitLockOutcome
		lockFile, outcome = acquireClaudeRateLimitCacheLock(path, lockDeadline)
		switch outcome {
		case claudeRateLimitLockAcquired:
			locked = true
		case claudeRateLimitLockContended:
			return false
		}
	} else {
		lockFile, locked = tryAcquireCrossProcessCacheLock(path)
		if !locked {
			return false
		}
	}
	if locked {
		defer func() {
			_ = unlockFile(lockFile)
			_ = lockFile.Close()
		}()
	}

	snap := codexRateLimitSnapshot{
		Buckets:      map[string]codexRateLimitBucket{},
		Contributors: map[string]map[string]codexRateLimitBucket{},
	}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &snap)
		if snap.Buckets == nil {
			snap.Buckets = map[string]codexRateLimitBucket{}
		}
		if snap.Contributors == nil {
			snap.Contributors = map[string]map[string]codexRateLimitBucket{}
		}
		snap.RolloutRetryEntries = codexRolloutRetryList(codexRolloutRetrySet(snap.RolloutRetryEntries))
	}
	if !mutate(&snap) {
		return false
	}
	codexSettleRunFreshness(&snap, now)

	out, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return false
	}
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), now.UnixNano())
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return false
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false
	}
	return true
}

// codexScopeSnapshotToAccount discards everything a snapshot holds for another
// account: its telemetry, its rollout scan progress and its run-freshness
// bookkeeping. A credentials swap must never surface — or owe a refresh for —
// the previous account's quota.
func codexScopeSnapshotToAccount(snap *codexRateLimitSnapshot, fingerprint string) {
	if snap.AccountFingerprint == fingerprint {
		return
	}
	snap.Buckets = map[string]codexRateLimitBucket{}
	snap.Contributors = map[string]map[string]codexRateLimitBucket{}
	snap.LimitNames = nil
	snap.FullSnapshotAtMs = 0
	codexClearRolloutProgress(snap)
	snap.RolloutRootFingerprint = ""
	snap.RunFloorMs = 0
	snap.RefreshOwedAtMs = 0
	snap.RefreshOwedAttempts = 0
	snap.ActiveRunFloorMs = 0
	snap.RunFloorPaidMs = 0
	snap.RefreshFallbackState = codexFallbackUnset
	// The observations it stamped are gone. RolloutCursorVersion is kept: the
	// progress it scoped was just cleared, so any cursor written from here on
	// is written by the binary it already names.
	snap.CodexVersion = ""
	snap.AccountFingerprint = fingerprint
}

// codexMergeContributorsIntoSnapshot is the body of the contributor merge,
// applied to a snapshot already loaded and scoped to `fingerprint` inside
// codexRateLimitCacheTransaction.
func codexMergeContributorsIntoSnapshot(
	snap *codexRateLimitSnapshot,
	perLimit map[string]map[string]codexRateLimitBucket,
	clears map[string]bool,
	fullSnapshot bool,
	present map[string]bool,
	emptyAuthoritative bool,
	now time.Time,
	fingerprint string,
	rolloutHighWater *codexRolloutScanProgress,
	rolloutAccountBase string,
	limitNames map[string]string,
) {
	// Migrate legacy cache files written before Contributors existed: each
	// pre-existing aggregated bucket becomes a single __legacy__ contributor
	// so subsequent sparse updates can merge against it instead of starting
	// fresh.
	for window, b := range snap.Buckets {
		if _, ok := snap.Contributors[window]; ok {
			continue
		}
		snap.Contributors[window] = map[string]codexRateLimitBucket{
			codexLegacyLimitID: reflagPersistedCodexBucket(b),
		}
	}
	// Restore the provenance flags on every loaded contributor — they're
	// json:"-" and so come back as false. Without this, the final
	// aggregateCodexBuckets pass would treat a still-live prior contributor
	// as "no usage known" and let a freshly updated sparse contributor with
	// lower usage replace it — exactly the bug this Contributors map is
	// supposed to prevent.
	for window, contribs := range snap.Contributors {
		for limit, b := range contribs {
			snap.Contributors[window][limit] = reflagPersistedCodexBucket(b)
		}
	}
	// Drop windows the frame explicitly cleared (null in a full read response)
	// before applying updates: a clear in this snapshot wins over any cached
	// state for that window — leaving it would render stale usage/reset until
	// the old reset passes. Updates apply afterwards so a single full snapshot
	// that both clears one window and refreshes another behaves correctly.
	for window := range clears {
		delete(snap.Buckets, window)
		delete(snap.Contributors, window)
	}
	nowMs := now.UnixMilli()
	for window, contributors := range perLimit {
		for limit, bucket := range contributors {
			// Codex's account/rateLimits/updated is sparse PER LIMIT: a
			// notification may carry only a fresh used_percent OR only a
			// fresh reset time for one (window, limit) pair, and any other
			// contributor for the same display window must be left alone.
			// Merge per field so a usage-only update doesn't clobber the
			// live reset and a reset-only update doesn't reset Consumed to
			// 0%. A prior reading is only carried forward when it still
			// describes a LIVE window for THIS limit (prior reset in the
			// future); otherwise it's stale and we let the partial new
			// bucket stand.
			windowContribs := snap.Contributors[window]
			prev, hadPrev := codexRateLimitBucket{}, false
			if windowContribs != nil {
				prev, hadPrev = windowContribs[limit]
				if hadPrev {
					prev = reflagPersistedCodexBucket(prev)
				}
			}
			// Reprocessing after a partial scan is idempotent. Older provider
			// evidence for the same contributor can never replace a newer cache
			// observation, while equal-percentage evidence at a newer timestamp
			// still advances freshness.
			if hadPrev && bucket.ObservedAtMs < prev.ObservedAtMs {
				continue
			}
			// An inferred observation time is a stand-in for one Codex did not
			// state, so it never displaces a stated reading it ties with, nor one
			// that already covers the current run floor.
			if hadPrev && bucket.Inferred && !prev.Inferred &&
				(bucket.ObservedAtMs == prev.ObservedAtMs || (snap.RunFloorMs > 0 && prev.ObservedAtMs >= snap.RunFloorMs)) {
				continue
			}
			priorStillLive := hadPrev && prev.ResetsAtMs > nowMs
			sameLiveWindow := priorStillLive && (!bucket.resetKnown || resetsWithinJitter(bucket.ResetsAtMs, prev.ResetsAtMs))
			if !bucket.usageKnown && sameLiveWindow {
				bucket.UsedPercentage = prev.UsedPercentage
				// A reset-only frame did not re-observe utilization. Preserve the
				// usage timestamp with the carried percentage instead of stamping it
				// with this sparse frame's receive time.
				bucket.ObservedAtMs = prev.ObservedAtMs
				bucket.Inferred = prev.Inferred
			}
			if !bucket.resetKnown && priorStillLive {
				bucket.ResetsAtMs = prev.ResetsAtMs
			}
			if bucket.WindowMinutes == 0 && hadPrev && prev.WindowMinutes > 0 {
				bucket.WindowMinutes = prev.WindowMinutes
			}
			if !bucket.usageKnown && !sameLiveWindow {
				if hadPrev && bucket.resetKnown && !resetsWithinJitter(bucket.ResetsAtMs, prev.ResetsAtMs) {
					delete(snap.Contributors[window], limit)
					if len(snap.Contributors[window]) == 0 {
						delete(snap.Contributors, window)
					}
				}
				continue
			}
			if snap.Contributors[window] == nil {
				snap.Contributors[window] = map[string]codexRateLimitBucket{}
			}
			snap.Contributors[window][limit] = bucket
		}
	}
	// Remove cross-slot duplicates of the same metric (a weekly reading that
	// migrated slots leaving a stale copy behind); keep only the newest. Runs on
	// every frame — it only ever deletes a genuine same-identity+limit duplicate,
	// so sparse frames stay non-destructive to unrelated windows.
	codexReconcileIdentitySupersession(snap.Contributors)
	// A full snapshot states every metric+limit that currently applies; drop any
	// cached contributor the snapshot omitted (reclassified elsewhere, or the plan
	// lost it). Keyed by (identity, limit id), not slot: so a stale weekly copy that
	// migrated into the same slot the snapshot still uses for the session is
	// reconciled away rather than preserved by the slot surviving, AND a stale weekly
	// limit (`codex_weekly_a`) is dropped even when the snapshot restated a DIFFERENT
	// weekly limit (`codex_weekly_b`) — the `weekly` identity surviving via `b` must
	// not shield `a` from an authoritative omission. This runs only
	// when the snapshot is AUTHORITATIVE about its contents: either it reported at
	// least one recognised metric (present non-empty) or it was authoritative-empty
	// (container literally `{}`, clearing every cached observation). A full frame
	// that merely failed to yield anything recognisable (unknown keys / unparseable
	// buckets) is NOT authoritative and must not wipe live data. Sparse frames
	// never trigger this — an unmentioned metric there just means "no update."
	//
	// A clear-only full snapshot (`len(clears) > 0` with no restated buckets, e.g.
	// `result.rateLimits: {"secondary": null}`) is ALSO authoritative: the explicit
	// null is a positive declaration that the window is gone. The slot-keyed clears
	// pass above only deletes the physical `secondary` slot, so a stale weekly-band
	// contributor that had migrated into `primary` would otherwise survive and keep
	// rendering the retired window. Running the identity-keyed omission pass with an
	// empty present-set drops every contributor the authoritative snapshot did not
	// restate, clearing the migrated copy too.
	if fullSnapshot && (len(present) > 0 || emptyAuthoritative || len(clears) > 0) {
		for slot, contribs := range snap.Contributors {
			for limit, b := range contribs {
				if !present[codexWindowIdentity(b.WindowMinutes, slot)+"\x00"+limit] {
					delete(contribs, limit)
				}
			}
			if len(contribs) == 0 {
				delete(snap.Contributors, slot)
			}
		}
		snap.FullSnapshotAtMs = nowMs
	}
	snap.LimitNames = codexMergeLimitNames(snap.LimitNames, limitNames, fullSnapshot, snap.Contributors)
	// Recompute the flat aggregate from contributors so callers reading the
	// cache (codexMetricsFromCache, tests) see the most-constrained view.
	snap.Buckets = aggregateCodexBuckets(snap.Contributors, now)
	snap.UpdatedAt = now.UTC().Format(time.RFC3339)
	snap.AccountFingerprint = fingerprint
	rolloutRootFingerprint := codexRolloutRootFingerprint(rolloutAccountBase)
	if rolloutHighWater != nil && snap.RolloutRootFingerprint != rolloutRootFingerprint {
		// A cursor from another CODEX_HOME is not meaningful in this sessions
		// tree. Clear it before comparing mtimes so a lower-mtime rollout in the
		// new root can establish its own completed progress.
		codexClearRolloutProgress(snap)
		snap.RolloutRootFingerprint = rolloutRootFingerprint
	}
	storedRolloutCursorIsFuture := snap.RolloutHighWaterMtimeNs > now.UnixNano() ||
		(snap.RolloutHighWaterMtimeNs == 0 && snap.RolloutHighWaterMtimeMs > now.UnixMilli())
	if rolloutHighWater != nil && (storedRolloutCursorIsFuture || rolloutHighWater.mtimeNs >= snap.RolloutHighWaterMtimeNs) {
		snap.RolloutHighWaterMtimeNs = rolloutHighWater.mtimeNs
		snap.RolloutHighWaterMtimeMs = time.Unix(0, rolloutHighWater.mtimeNs).UnixMilli()
		snap.RolloutHighWaterBoundaryFingerprint = rolloutHighWater.boundaryFingerprint
		snap.RolloutHighWaterBoundaryCursor = rolloutHighWater.boundaryCursor
		snap.RolloutBacklogFingerprint = rolloutHighWater.backlogFingerprint
		snap.RolloutBacklogCursor = rolloutHighWater.backlogCursor
		snap.RolloutBacklogMtimeNs = rolloutHighWater.backlogMtimeNs
		snap.RolloutBacklogCohortSize = rolloutHighWater.backlogCohortSize
		snap.RolloutRetryEntries = append([]string(nil), rolloutHighWater.retryEntries...)
		snap.RolloutRetryCursor = rolloutHighWater.retryCursor
		snap.RolloutRetryFingerprint = rolloutHighWater.retryFingerprint
		snap.RolloutFutureMtimeAnchorNs = rolloutHighWater.futureAnchorNs
		snap.RolloutFutureMtimeFloorNs = rolloutHighWater.futureFloorNs
		snap.RolloutFutureMtimeCeilingNs = rolloutHighWater.futureCeilingNs
		snap.RolloutFutureMtimeFingerprint = rolloutHighWater.futureFingerprint
		snap.RolloutFutureMtimeCursor = rolloutHighWater.futureCursor
		snap.RolloutFutureMtimeCohortSize = rolloutHighWater.futureCohortSize
		snap.RolloutFutureMtimeComplete = rolloutHighWater.futureComplete
		snap.RolloutRootFingerprint = rolloutRootFingerprint
	}
}

// reflagPersistedCodexBucket restores the usageKnown / resetKnown provenance
// flags that aren't persisted (they're json:"-"). On load both are false; we
// upgrade based on whether values are present, because mergeCodexBucketMost-
// Constrained refuses to treat a prior bucket as a real contributor unless
// its usageKnown flag is set.
func reflagPersistedCodexBucket(b codexRateLimitBucket) codexRateLimitBucket {
	if !b.usageKnown {
		// A bucket only reaches the on-disk snapshot once usage was observed
		// — the write path skips buckets without a known used_percent — so
		// it is safe to mark loaded entries as having known usage.
		b.usageKnown = true
	}
	if !b.resetKnown {
		b.resetKnown = b.ResetsAtMs > 0
	}
	return b
}

// loadCodexRateLimitSnapshot reads the cache. Returns (zero, false) when the
// file is absent or unreadable — the normal "no telemetry observed yet" state.
func loadCodexRateLimitSnapshot(path string) (codexRateLimitSnapshot, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return codexRateLimitSnapshot{}, false
	}
	var snap codexRateLimitSnapshot
	if err := json.Unmarshal(b, &snap); err != nil || snap.Buckets == nil {
		return codexRateLimitSnapshot{}, false
	}
	return snap, true
}

// currentCodexAccountFingerprint reads the Codex auth.json on disk and returns
// the same workspace-scoped fingerprint codexUsageParser.Parse would attach to
// a usage snapshot. Used by the capture path to scope the rate-limit cache to
// the active account. Returns "" when no auth is readable, in which case the
// cache is unscoped (best-effort, matches the Claude analog).
func currentCodexAccountFingerprint() string {
	return codexAccountFingerprintAtBase(codexHomeBase())
}

// codexHomeBase is the active CODEX_HOME (or ~/.codex), resolved the same way
// codexUsageParser.ParseContext resolves it for the default home.
func codexHomeBase() string {
	home, _ := os.UserHomeDir()
	return firstNonEmpty(os.Getenv("CODEX_HOME"), expandHome(home, ".codex"))
}

func codexAccountFingerprintAtBase(base string) string {
	if base == "" {
		return ""
	}
	auth := codexAuth{}
	if !readJSONFile(expandHome(base, "auth.json"), &auth) {
		return ""
	}
	claims := codexIDTokenClaims{}
	parseJWTClaims(auth.Tokens.IDToken, &claims)
	return fingerprintAccount("codex", codexAccountScope(auth, claims))
}

// codexContributorsForAccount loads the rate-limit cache and returns the
// per-(slot, limit) contributors for `currentFingerprint`, or an empty map when
// the cache is missing or pinned to a different account. Provenance flags are
// restored so the identity partitioning treats loaded readings as observed. A
// legacy cache written before the Contributors map existed surfaces each flat
// slot bucket as a single `__legacy__` contributor, so both shapes reconcile
// identically.
func codexContributorsForAccount(currentFingerprint string) map[string]map[string]codexRateLimitBucket {
	snap, ok := loadCodexRateLimitSnapshot(codexRateLimitCachePath())
	if !ok || snap.AccountFingerprint != currentFingerprint {
		return map[string]map[string]codexRateLimitBucket{}
	}
	return codexContributorsFromSnapshot(snap)
}

func codexContributorsFromSnapshot(snap codexRateLimitSnapshot) map[string]map[string]codexRateLimitBucket {
	if len(snap.Contributors) > 0 {
		reflagged := make(map[string]map[string]codexRateLimitBucket, len(snap.Contributors))
		for w, contribs := range snap.Contributors {
			windowMap := make(map[string]codexRateLimitBucket, len(contribs))
			for limit, b := range contribs {
				windowMap[limit] = reflagPersistedCodexBucket(b)
			}
			reflagged[w] = windowMap
		}
		return reflagged
	}
	legacy := make(map[string]map[string]codexRateLimitBucket, len(snap.Buckets))
	for w, b := range snap.Buckets {
		legacy[w] = map[string]codexRateLimitBucket{codexLegacyLimitID: reflagPersistedCodexBucket(b)}
	}
	return legacy
}

// codexMergeLimitNames folds a frame's limit names into the cached ones and
// forgets the name of every limit that no longer has a contributor, so a pool
// the account lost cannot keep labelling rows. An empty incoming name removes
// the entry (the provider now reports that limit unnamed) only when the frame
// is an authoritative snapshot; a sparse update's null name says nothing about
// the pool and keeps the cached one.
func codexMergeLimitNames(cached, incoming map[string]string, authoritative bool, contributors map[string]map[string]codexRateLimitBucket) map[string]string {
	merged := map[string]string{}
	for id, name := range cached {
		merged[id] = name
	}
	for id, name := range incoming {
		if name == "" {
			if authoritative {
				delete(merged, id)
			}
			continue
		}
		merged[id] = name
	}
	live := map[string]bool{}
	for _, contribs := range contributors {
		for id := range contribs {
			live[id] = true
		}
	}
	for id := range merged {
		if !live[id] {
			delete(merged, id)
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// codexCacheView is the account-scoped slice of the cache the card renders.
type codexCacheView struct {
	contributors     map[string]map[string]codexRateLimitBucket
	limitNames       map[string]string
	fullSnapshotAtMs int64
	// Run freshness (codexRunFreshnessFromView).
	runFloorMs          int64
	activeRunFloorMs    int64
	runFloorPaidMs      int64
	refreshOwedAtMs     int64
	refreshOwedAttempts int
	refreshFallback     string
	// Capture stamps (cliagent_usage_codex_capture_stamp.go).
	codexVersion         string
	rolloutCursorVersion string
}

// codexCacheViewForAccount is codexContributorsForAccount plus the pool names,
// the time of the last authoritative full snapshot and the run-freshness
// bookkeeping — all from one read of the cache file.
func codexCacheViewForAccount(currentFingerprint string) codexCacheView {
	snap, ok := loadCodexRateLimitSnapshot(codexRateLimitCachePath())
	if !ok || snap.AccountFingerprint != currentFingerprint {
		return codexCacheView{contributors: map[string]map[string]codexRateLimitBucket{}}
	}
	return codexCacheViewFromSnapshot(snap)
}

// codexCacheViewFromSnapshot is codexCacheViewForAccount's projection without
// the read, so a writer already inside a cache transaction can classify the
// snapshot it is about to persist with the same helpers the read side uses.
func codexCacheViewFromSnapshot(snap codexRateLimitSnapshot) codexCacheView {
	return codexCacheView{
		contributors:         codexContributorsFromSnapshot(snap),
		limitNames:           snap.LimitNames,
		fullSnapshotAtMs:     snap.FullSnapshotAtMs,
		runFloorMs:           snap.RunFloorMs,
		activeRunFloorMs:     snap.ActiveRunFloorMs,
		runFloorPaidMs:       snap.RunFloorPaidMs,
		refreshOwedAtMs:      snap.RefreshOwedAtMs,
		refreshOwedAttempts:  snap.RefreshOwedAttempts,
		refreshFallback:      snap.RefreshFallbackState,
		codexVersion:         snap.CodexVersion,
		rolloutCursorVersion: snap.RolloutCursorVersion,
	}
}

// codexSplitContributorsByPool separates the account's main pool (every limit
// Codex leaves unnamed, including the legacy aggregate) from each named model
// pool. Named pools are returned keyed by display name.
func codexSplitContributorsByPool(view codexCacheView) (map[string]map[string]codexRateLimitBucket, map[string]map[string]map[string]codexRateLimitBucket) {
	main := map[string]map[string]codexRateLimitBucket{}
	pools := map[string]map[string]map[string]codexRateLimitBucket{}
	for slot, contribs := range view.contributors {
		for limitID, b := range contribs {
			target := main
			if name := view.limitNames[limitID]; name != "" {
				if pools[name] == nil {
					pools[name] = map[string]map[string]codexRateLimitBucket{}
				}
				target = pools[name]
			}
			if target[slot] == nil {
				target[slot] = map[string]codexRateLimitBucket{}
			}
			target[slot][limitID] = b
		}
	}
	return main, pools
}

// codexPoolModelID turns a pool's display name into the model id it meters
// ("GPT-5.3-Codex-Spark" → "gpt-5.3-codex-spark"), the same spelling Codex
// lists the model under, so the row names the model it limits.
func codexPoolModelID(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), "-"))
}

// codexMetricsFromCache builds the card rows from the rate-limit cache.
//
// The account's MAIN pool renders first as the Claude-aligned pair — the 5-hour
// session window, then the weekly quota. Rows are selected by metric IDENTITY
// (partitioning every contributor across both storage slots) rather than by
// "slot == row", so duplicate weekly buckets collapse to the newest, a
// migrated/swapped placement still renders in the right row, and a weekly-band
// reading is never promoted into the session row (AC4). A missing window keeps
// an Unknown placeholder until an authoritative full snapshot has shown the
// account's actual windows; after that, a window the account does not have is
// left off (a plan that meters weekly only shows one row), while an account
// reporting no window at all still shows both placeholders — that is the
// spent-quota shape codexUsageLimitNotice explains.
//
// Each NAMED model pool Codex reports (`limitName`, e.g. "GPT-5.3-Codex-Spark")
// follows with its own rows for exactly the windows it has, labelled with the
// pool name and carrying the model id. Folding a pool into the main rows would
// let its 0% session window stand in for a main pool that has none.
//
// The cache is trusted only when its `accountFingerprint` exactly matches the
// caller-supplied one — otherwise a previous account's windows could surface
// under the current account after a credentials swap.
func codexMetricsFromCache(now time.Time, currentFingerprint string) []cliAgentUsageMetric {
	view := codexCacheViewForAccount(currentFingerprint)
	mainContributors, pools := codexSplitContributorsByPool(view)
	parts := codexPartitionByIdentity(mainContributors)

	sessionBucket, sessionOK := codexIdentityDisplayBucket(parts, codexIdentitySession, codexWindowPrimary, now)
	weeklyBucket, weeklyOK := codexIdentityDisplayBucket(parts, codexIdentityWeekly, codexWindowSecondary, now)

	windowsKnown := view.fullSnapshotAtMs > 0 && (sessionOK || weeklyOK)
	metrics := make([]cliAgentUsageMetric, 0, 2+2*len(pools))
	if sessionOK || !windowsKnown {
		metrics = append(metrics, codexMetricFromBucket(sessionBucket, sessionOK, limitKindSession, "5-hour session window", now))
	}
	if weeklyOK || !windowsKnown {
		metrics = append(metrics, codexMetricFromBucket(weeklyBucket, weeklyOK, limitKindWeekly, "Weekly quota", now))
	}

	names := make([]string, 0, len(pools))
	for name := range pools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		poolParts := codexPartitionByIdentity(pools[name])
		for _, row := range []struct {
			identity, slot, kind, label string
		}{
			{codexIdentitySession, codexWindowPrimary, limitKindSession, "5-hour session window"},
			{codexIdentityWeekly, codexWindowSecondary, limitKindWeekly, "Weekly quota"},
		} {
			bucket, ok := codexIdentityDisplayBucket(poolParts, row.identity, row.slot, now)
			if !ok {
				continue
			}
			metric := codexMetricFromBucket(bucket, true, row.kind, row.label, now)
			metric.Label = name + " — " + metric.Label
			metric.Model = codexPoolModelID(name)
			metrics = append(metrics, metric)
		}
	}
	if len(metrics) > cliUsageMaxMetricsPerProvider {
		metrics = metrics[:cliUsageMaxMetricsPerProvider]
	}
	return metrics
}

// codexWindowLabel renders a human label for a window of the given length.
// We special-case the canonical Codex windows (300 min = 5 hours, 10080 min =
// weekly) so the long-standing labels stay identical, and derive a neutral
// "Nm/Nh/Nd window" string otherwise so an off-spec plan still shows the right
// quota window context instead of the wrong hard-coded one.
//
// Codex's `token_count` JSONL often reports the canonical windows with a
// floored/rounded minute count (e.g. window_minutes: 299 for the 5-hour window
// and 10079 for the weekly window — see openai/codex#14728), so we tolerate a
// small band around 300 and 10080 before falling back to the generic label.
// The bands are disjoint from any neighboring real Codex window (4h=240,
// 6h=360, 6-day=8640, biweekly=20160), so a legitimately different quota
// length still renders as the neutral "N-…" string.
func codexWindowLabel(minutes float64, fallback string) string {
	if minutes <= 0 {
		return fallback
	}
	// Canonical windows share their band definition with codexWindowIdentity so
	// label and identity never drift apart (single source of truth).
	switch {
	case codexMinutesInSessionBand(minutes):
		return "5-hour session window"
	case codexMinutesInWeeklyBand(minutes):
		return "Weekly quota"
	}
	m := int(minutes + 0.5)
	switch {
	case m < 60:
		return fmt.Sprintf("%d-minute window", m)
	case m%60 == 0 && m < 60*24:
		return fmt.Sprintf("%d-hour window", m/60)
	case m%(60*24) == 0:
		return fmt.Sprintf("%d-day window", m/(60*24))
	}
	return fmt.Sprintf("%.1f-hour window", float64(m)/60)
}

// codexMetricFromBucket renders an ALREADY-SELECTED window bucket into a metric
// row for `kind` (the fixed layout kind — session or weekly), or an Unknown
// placeholder when no bucket was selected (`ok == false`). The label is derived
// from the bucket's own WindowMinutes so a migrated/non-canonical window still
// reads correctly, falling back to `defaultLabel` when no length is known. A
// window whose reset has passed becomes unobservable, matching Claude's
// behaviour; assuming 0% ignores usage that may occur on another computer.
//
// Selection is now identity-based (a session row may be sourced from the
// `secondary` slot and vice-versa), so unlike the old windowID lookup this
// helper takes the resolved bucket directly and never consults the storage slot.
func codexMetricFromBucket(b codexRateLimitBucket, ok bool, kind, defaultLabel string, now time.Time) cliAgentUsageMetric {
	if !ok {
		return cliAgentUsageMetric{Kind: kind, Label: defaultLabel, Unit: "%", Unknown: true}
	}
	used := b.UsedPercentage
	var resetAt string
	if b.ResetsAtMs > 0 {
		if now.UnixMilli() >= b.ResetsAtMs {
			return cliAgentUsageMetric{
				Kind: kind, Label: codexWindowLabel(b.WindowMinutes, defaultLabel), Unit: "%",
				ObservedAt: observedAtRFC3339(b.ObservedAtMs), Unknown: true,
			}
		} else {
			resetAt = time.UnixMilli(b.ResetsAtMs).UTC().Format(time.RFC3339)
		}
	}
	used = clampPercent(used)
	return cliAgentUsageMetric{
		Kind: kind, Label: codexWindowLabel(b.WindowMinutes, defaultLabel), Unit: "%",
		Total: floatPtr(100), Consumed: floatPtr(used), Remaining: floatPtr(100 - used),
		ResetAt: resetAt, ObservedAt: observedAtRFC3339(b.ObservedAtMs),
	}
}
