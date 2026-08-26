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
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
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
	// RolloutHighWaterMtimeMs is filesystem scan progress, not provider
	// observation time. It advances only after every selected rollout file was
	// handled, allowing unchanged refreshes to remain cache-only.
	RolloutHighWaterMtimeMs int64 `json:"rolloutHighWaterMtimeMs,omitempty"`
	// RolloutHighWaterMtimeNs preserves the filesystem's full timestamp
	// precision. The millisecond field remains for backwards compatibility with
	// snapshots written before same-millisecond appends were handled.
	RolloutHighWaterMtimeNs int64 `json:"rolloutHighWaterMtimeNs,omitempty"`
	// RolloutHighWaterBoundaryFingerprint identifies only the files and sizes at
	// the completed mtime boundary. It lets a coarse-resolution filesystem expose
	// an append whose mtime stayed exactly equal to the high-water without
	// persisting rollout paths or reopening unchanged files.
	RolloutHighWaterBoundaryFingerprint string `json:"rolloutHighWaterBoundaryFingerprint,omitempty"`
}

// codexRateLimitMu serialises the read-modify-write of the cache file
// in-process. Cross-process serialization is handled separately by an
// advisory file lock (the cache may be touched by a future Codex statusline
// hook running in a different process, same as Claude's).
var codexRateLimitMu sync.Mutex

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
// observation wins; on an equal timestamp the higher usage wins; then a fixed
// primary-over-secondary slot precedence; finally a known-usage reading beats an
// unknown one. Freshness dominates so an older duplicate is discarded even when
// its used % is higher (AC1 / stale-observation), and no branch depends on Go
// map iteration order.
func codexPlacementBeats(aBucket codexRateLimitBucket, aSlot string, bBucket codexRateLimitBucket, bSlot string) bool {
	if aBucket.ObservedAtMs != bBucket.ObservedAtMs {
		return aBucket.ObservedAtMs > bBucket.ObservedAtMs
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
// the hot streaming path and must never break a session).
func captureCodexRateLimitLine(line string, now time.Time) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return
	}
	// Cheap prefilter: only attempt the JSON decode when the line could
	// plausibly carry rate-limit telemetry. `token_count` wraps the legacy
	// notification; `account/rateLimits/{read,updated}` is the newer
	// JSON-RPC surface; `rate_limits`/`rateLimits` cover both payload key
	// spellings. Anything else can't carry a window update for us.
	if !strings.Contains(trimmed, "token_count") &&
		!strings.Contains(trimmed, "rateLimits") &&
		!strings.Contains(trimmed, "rate_limit") {
		return
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return
	}
	if !isRecognizedCodexRateLimitEnvelope(raw) {
		return
	}
	anchor, observedAt := codexObservationTimes(raw, now, true)
	updates, clears, fullSnapshot, present, emptyAuthoritative := extractCodexRateLimitBucketsFull(raw, anchor)
	codexStampContributorObservations(updates, observedAt)
	updates = codexCanonicalizeLiveContributors(updates)
	// A full snapshot must be processed even when it carries no buckets and no
	// explicit nulls IF it is authoritative-empty: an `account/rateLimits/read`
	// response whose container is literally `{}` declares the account now has NO
	// quota windows, so every cached observation has to be reconciled away. A full
	// snapshot that is non-empty but yields nothing recognised (unknown keys,
	// unparseable buckets, forward-compatible fields) is NOT authoritative-empty
	// and is dropped here, exactly like a sparse frame with nothing to say — it
	// must never erase live observations.
	if len(updates) == 0 && len(clears) == 0 && !emptyAuthoritative {
		return
	}
	mergeCodexRateLimitCachePerLimit(codexRateLimitCachePath(), updates, clears, fullSnapshot, present, emptyAuthoritative, now, currentCodexAccountFingerprint())
}

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

// codexCanonicalizeLiveContributors normalizes the two displayed identities
// before the slot-keyed cache merge. Codex can report a weekly bucket under
// `primary` (and vice versa); merging that physical key directly can overwrite a
// cached session contributor with the same limit id before identity reconciliation
// gets a chance to preserve both. Rollout contributors are normalized at their
// reconstruction boundary for the same reason.
func codexCanonicalizeLiveContributors(contributors map[string]map[string]codexRateLimitBucket) map[string]map[string]codexRateLimitBucket {
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
	mergeCodexRateLimitCachePerLimitProgress(path, perLimit, clears, fullSnapshot, present, emptyAuthoritative, now, fingerprint, nil)
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
) {
	if path == "" || (len(perLimit) == 0 && len(clears) == 0 && !emptyAuthoritative && rolloutHighWater == nil) {
		return
	}
	codexRateLimitMu.Lock()
	defer codexRateLimitMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	lockFile, locked := acquireCrossProcessCacheLock(path)
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
	}
	if snap.AccountFingerprint != fingerprint {
		snap.Buckets = map[string]codexRateLimitBucket{}
		snap.Contributors = map[string]map[string]codexRateLimitBucket{}
		snap.RolloutHighWaterMtimeMs = 0
		snap.RolloutHighWaterMtimeNs = 0
		snap.RolloutHighWaterBoundaryFingerprint = ""
	}
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
			priorStillLive := hadPrev && prev.ResetsAtMs > nowMs
			sameLiveWindow := priorStillLive && (!bucket.resetKnown || resetsWithinJitter(bucket.ResetsAtMs, prev.ResetsAtMs))
			if !bucket.usageKnown && sameLiveWindow {
				bucket.UsedPercentage = prev.UsedPercentage
				// A reset-only frame did not re-observe utilization. Preserve the
				// usage timestamp with the carried percentage instead of stamping it
				// with this sparse frame's receive time.
				bucket.ObservedAtMs = prev.ObservedAtMs
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
	}
	// Recompute the flat aggregate from contributors so callers reading the
	// cache (codexMetricsFromCache, tests) see the most-constrained view.
	snap.Buckets = aggregateCodexBuckets(snap.Contributors, now)
	snap.UpdatedAt = now.UTC().Format(time.RFC3339)
	snap.AccountFingerprint = fingerprint
	if rolloutHighWater != nil && (rolloutHighWater.mtimeNs > snap.RolloutHighWaterMtimeNs ||
		(rolloutHighWater.mtimeNs == snap.RolloutHighWaterMtimeNs &&
			rolloutHighWater.boundaryFingerprint != snap.RolloutHighWaterBoundaryFingerprint)) {
		snap.RolloutHighWaterMtimeNs = rolloutHighWater.mtimeNs
		snap.RolloutHighWaterMtimeMs = time.Unix(0, rolloutHighWater.mtimeNs).UnixMilli()
		snap.RolloutHighWaterBoundaryFingerprint = rolloutHighWater.boundaryFingerprint
	}

	out, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return
	}
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), now.UnixNano())
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
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
	home, _ := os.UserHomeDir()
	base := firstNonEmpty(os.Getenv("CODEX_HOME"), expandHome(home, ".codex"))
	return codexAccountFingerprintAtBase(base)
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

// codexMetricsFromCache builds the two Claude-aligned metric rows from the
// rate-limit cache: the 5-hour session window first, the weekly quota second.
// Rows are selected by metric IDENTITY (partitioning every contributor across
// both storage slots) rather than by "slot == row", so duplicate weekly buckets
// collapse to the newest, a migrated/swapped placement still renders in the
// right row, and a weekly-band reading is never promoted into the session row
// (AC4). Both rows are always emitted — Unknown when unobserved — so the layout
// stays aligned with the Claude Code card even when a window is missing.
//
// The cache is trusted only when its `accountFingerprint` exactly matches the
// caller-supplied one — otherwise a previous account's windows could surface
// under the current account after a credentials swap.
func codexMetricsFromCache(now time.Time, currentFingerprint string) []cliAgentUsageMetric {
	parts := codexPartitionByIdentity(codexContributorsForAccount(currentFingerprint))

	sessionBucket, sessionOK := codexIdentityDisplayBucket(parts, codexIdentitySession, codexWindowPrimary, now)
	weeklyBucket, weeklyOK := codexIdentityDisplayBucket(parts, codexIdentityWeekly, codexWindowSecondary, now)

	session := codexMetricFromBucket(sessionBucket, sessionOK, limitKindSession, "5-hour session window", now)
	weekly := codexMetricFromBucket(weeklyBucket, weeklyOK, limitKindWeekly, "Weekly quota", now)

	return []cliAgentUsageMetric{session, weekly}
}

// codexRolloutScanFileCap bounds how many of the most-recent rollout logs the
// fallback opens before giving up, so a sessions directory holding thousands of
// files can't turn a usage refresh into a long scan. Every in-cap log is folded
// so distinct per-(identity, limit) contributors that newer logs omitted are
// still gathered; newest-first (identity, limit) dedup keeps the freshest
// reading per contributor.
const codexRolloutScanFileCap = 16

// Keep part of the optional rollout budget for consuming candidates found by
// discovery. Otherwise a slow sessions-tree walk can expire the shared child
// context after finding today's rollout but before opening it, repeating that
// starvation on every refresh.
const codexRolloutCandidateReadReserve = time.Second

// codexReconcileFromRollout merges direct-run rollout evidence into the same
// normalized contributor cache populated by terminal-managed stdout. It is
// intentionally not Unknown-only: a still-live cached percentage may be older
// than a successful direct run. Newest evidence wins per metric identity and
// limit id, then the existing most-constrained aggregate determines the row and
// its observation timestamp.
func codexReconcileFromRollout(ctx context.Context, base, currentFingerprint string, now time.Time) ([]cliAgentUsageMetric, codexUsageLimitEvidence, time.Time) {
	cursor := codexRolloutScanCursorForAccount(currentFingerprint, now)
	contribs, limit, latestObservation, highWater, ok := codexRolloutFallbackBuckets(ctx, base, now, cursor)
	// Authentication can change while filesystem I/O is in progress. Never let
	// an old-account scan clear or overwrite a live capture already scoped to the
	// newly active account; the next refresh will reconcile under that account.
	if codexAccountFingerprintAtBase(base) != currentFingerprint {
		return codexMetricsFromCache(now, currentFingerprint), codexUsageLimitEvidence{}, time.Time{}
	}
	if ok || highWater != nil {
		mergeCodexRateLimitCachePerLimitProgress(
			codexRateLimitCachePath(), contribs, nil, false, nil, false,
			now, currentFingerprint, highWater,
		)
	}
	return codexMetricsFromCache(now, currentFingerprint), limit, latestObservation
}

type codexRolloutScanCursor struct {
	mtimeNs             int64
	boundaryFingerprint string
}

type codexRolloutScanProgress struct {
	mtimeNs             int64
	boundaryFingerprint string
}

// codexRolloutScanCursorForAccount separates filesystem progress from provider event
// time. New caches use the completed-scan high-water. Legacy caches fall back
// to the oldest observable aggregate so a newer weekly-bearing file is not
// hidden by a fresher session row; empty/all-Unknown caches scan from zero.
func codexRolloutScanCursorForAccount(currentFingerprint string, now time.Time) codexRolloutScanCursor {
	snap, ok := loadCodexRateLimitSnapshot(codexRateLimitCachePath())
	if !ok || snap.AccountFingerprint != currentFingerprint {
		return codexRolloutScanCursor{}
	}
	if snap.RolloutHighWaterMtimeNs > 0 {
		return codexRolloutScanCursor{
			mtimeNs:             snap.RolloutHighWaterMtimeNs,
			boundaryFingerprint: snap.RolloutHighWaterBoundaryFingerprint,
		}
	}
	if snap.RolloutHighWaterMtimeMs > 0 {
		return codexRolloutScanCursor{mtimeNs: time.UnixMilli(snap.RolloutHighWaterMtimeMs).UnixNano()}
	}
	parts := codexPartitionByIdentity(codexContributorsForAccount(currentFingerprint))
	oldest := int64(0)
	for _, row := range []struct{ identity, slot string }{
		{codexIdentitySession, codexWindowPrimary},
		{codexIdentityWeekly, codexWindowSecondary},
	} {
		b, present := codexIdentityDisplayBucket(parts, row.identity, row.slot, now)
		if !present || b.ObservedAtMs <= 0 || (b.ResetsAtMs > 0 && now.UnixMilli() >= b.ResetsAtMs) {
			// A legacy snapshot has no completed-scan watermark. If either
			// display row is absent, scanning from the populated row's timestamp
			// could permanently hide an older rollout that supplies the missing
			// identity. Start from zero until both rows are observable.
			return codexRolloutScanCursor{}
		}
		if oldest == 0 || b.ObservedAtMs < oldest {
			oldest = b.ObservedAtMs
		}
	}
	return codexRolloutScanCursor{mtimeNs: time.UnixMilli(oldest).UnixNano()}
}

type codexRolloutCandidate struct {
	path       string
	boundaryID string
	mtime      time.Time
	size       int64
}

func codexRolloutBoundaryFingerprint(candidates []codexRolloutCandidate, mtimeNs int64) string {
	entries := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.mtime.UnixNano() == mtimeNs {
			entries = append(entries, fmt.Sprintf("%s\x00%d", candidate.boundaryID, candidate.size))
		}
	}
	if len(entries) == 0 {
		return ""
	}
	sort.Strings(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return fmt.Sprintf("%x", sum[:])
}

// codexDiscoverRolloutCandidates walks the fixed YYYY/MM/DD rollout layout
// newest-first while checking the optional scan context between filesystem
// entries. Starting with the newest date prevents a large history from starving
// today's direct-run evidence when the Codex child budget expires. Unlike
// filepath.Glob, discovery also stops promptly after cancellation. complete is
// false when cancellation or a metadata error means scan progress must not
// advance.
func codexDiscoverRolloutCandidates(ctx context.Context, base string, cursor codexRolloutScanCursor) ([]codexRolloutCandidate, bool) {
	root := filepath.Join(base, "sessions")
	candidates := []codexRolloutCandidate{}
	boundaryCandidates := []codexRolloutCandidate{}
	complete := true
	var walkDateLayout func(string, int)
	walkDateLayout = func(dir string, depth int) {
		if ctx.Err() != nil {
			complete = false
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			if depth == 0 && os.IsNotExist(err) {
				return
			}
			complete = false
			return
		}
		// os.ReadDir sorts by filename. Codex's zero-padded YYYY/MM/DD layout
		// therefore becomes chronological when traversed in reverse.
		for i := len(entries) - 1; i >= 0; i-- {
			if ctx.Err() != nil {
				complete = false
				return
			}
			entry := entries[i]
			path := filepath.Join(dir, entry.Name())
			if depth < 3 {
				if entry.IsDir() {
					walkDateLayout(path, depth+1)
				}
				continue
			}
			if entry.IsDir() {
				continue
			}
			matched, err := filepath.Match("rollout-*.jsonl", entry.Name())
			if err != nil || !matched {
				continue
			}
			info, err := os.Stat(path)
			if err != nil {
				// A transient metadata failure leaves this discovery pass incomplete.
				// We may still consume other candidates, but must not advance progress
				// past a file whose mtime could not be evaluated.
				complete = false
				continue
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				complete = false
				continue
			}
			candidate := codexRolloutCandidate{path: path, boundaryID: rel, mtime: info.ModTime(), size: info.Size()}
			switch mtimeNs := info.ModTime().UnixNano(); {
			case mtimeNs > cursor.mtimeNs:
				candidates = append(candidates, candidate)
			case cursor.mtimeNs > 0 && mtimeNs == cursor.mtimeNs:
				boundaryCandidates = append(boundaryCandidates, candidate)
			}
		}
	}
	walkDateLayout(root, 0)
	// Exact-mtime equality normally means the boundary is unchanged. On coarse
	// filesystems, however, an append can increase a rollout's size without
	// changing its reported mtime. Compare a redacted metadata fingerprint and
	// rescan equality only when that boundary changed (or once for a legacy cache
	// that predates fingerprints).
	if len(boundaryCandidates) > 0 &&
		(cursor.boundaryFingerprint == "" ||
			codexRolloutBoundaryFingerprint(boundaryCandidates, cursor.mtimeNs) != cursor.boundaryFingerprint) {
		candidates = append(candidates, boundaryCandidates...)
	}
	return candidates, complete
}

func codexRolloutDiscoveryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return ctx, func() {}
	}
	remaining := time.Until(deadline)
	reserve := codexRolloutCandidateReadReserve
	if remaining <= reserve {
		reserve = remaining / 2
	}
	return context.WithDeadline(ctx, deadline.Add(-reserve))
}

// codexUsageLimitNotice renders the card notice for a quota refusal, or "" when
// none should be shown.
//
// Two guards keep the notice honest:
//
//   - It is only shown while a window is still unobservable. Any window we can
//     actually report supersedes the refusal — a real percentage is strictly
//     more informative than "you were refused a while ago".
//   - The evidence must be newer than every observation behind those metrics.
//     Codex writes a rollout log on every attempt, so a still-exhausted account
//     re-evidences itself the moment the user tries again; ranking by time means
//     an old refusal can never shout over telemetry that arrived after it.
//
// The age cap exists because a refusal carries no window and therefore no reset:
// Codex says only "try again at <time>" in prose we deliberately do not parse.
// Expiring the notice can understate a multi-day weekly exhaustion, which is the
// safe direction — the next attempt re-evidences it — whereas an unbounded
// notice would keep declaring a limit that cleared days ago.
func codexUsageLimitNotice(metrics []cliAgentUsageMetric, limit codexUsageLimitEvidence, latestRolloutObservation, now time.Time) string {
	if limit.At.IsZero() || now.Sub(limit.At) > codexUsageLimitNoticeMaxAge {
		return ""
	}
	if !latestRolloutObservation.IsZero() && !latestRolloutObservation.Before(limit.At) {
		return ""
	}
	anyUnknown := false
	for _, m := range metrics {
		if m.Unknown {
			anyUnknown = true
		}
		observed, err := time.Parse(time.RFC3339, m.ObservedAt)
		if err == nil && !observed.Before(limit.At) {
			return ""
		}
	}
	if !anyUnknown {
		return ""
	}
	notice := "Codex refused a run because this account's usage limit was reached, so its capacity is unreported until Codex sends a fresh window."
	return notice
}

// codexUsageLimitNoticeMaxAge is how long a quota refusal keeps explaining an
// unobservable card. Sized to comfortably outlast Codex's 5-hour window while
// staying far short of the weekly one, so a cleared limit stops being announced
// without waiting days for the truth to catch up.
const codexUsageLimitNoticeMaxAge = 12 * time.Hour

// codexRolloutFallbackBuckets reads Codex's session rollout logs
// (CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl) for the most recent populated
// `rate_limits` frame and returns its per-(window, limit) contributors. The
// contributors are returned un-collapsed so the caller can partition them by
// metric identity — a slot that carried two distinct-identity limits (e.g. a
// session and a weekly reading during bucket migration) must not be flattened to
// a single bucket here or one row would be lost.
//
// Account scoping: rollout logs carry no account identity of their own, so they
// can't be fingerprint-matched the way the on-disk cache is. Instead we reject
// any log written BEFORE auth.json's current mtime — a fresh `codex login`
// rewrites auth.json, so its mtime advances past every rollout log the
// previous account produced. This prevents the credentials-swap bleed the cache
// path's fingerprint check already guards against (showing a prior account's
// quota under the new account). When auth.json is missing/unreadable the guard
// is disabled, matching the best-effort unscoped behaviour the parser already
// uses when the account is unknown. Best-effort: returns (nil, false) on any
// problem.
func codexRolloutFallbackBuckets(ctx context.Context, base string, now time.Time, cursor codexRolloutScanCursor) (map[string]map[string]codexRateLimitBucket, codexUsageLimitEvidence, time.Time, *codexRolloutScanProgress, bool) {
	if base == "" {
		return nil, codexUsageLimitEvidence{}, time.Time{}, nil, false
	}
	// auth.json mtime is the account-login watermark (zero = missing → guard
	// off). A fresh `codex login` rewrites auth.json, so its mtime marks when
	// the current account took over this CODEX_HOME.
	//
	// We scope rollout logs by their SESSION START time (the first line's
	// timestamp), NOT the file mtime: a previous account's session that is still
	// running when a new account logs in keeps appending to its log, pushing the
	// file mtime past the login even though the session — and its quota — belong
	// to the old account. The start time, fixed when the session began, stays on
	// the correct side of the login. (Residual caveat: a token refresh that
	// rewrites auth.json mid-session can over-reject same-account logs that
	// started earlier; that degrades to Unknown, never to cross-account bleed.)
	var authMod time.Time
	if info, err := os.Stat(expandHome(base, "auth.json")); err == nil {
		authMod = info.ModTime()
	}
	// Date-nested layout: sessions/YYYY/MM/DD/rollout-<ISO-timestamp>-*.jsonl.
	discoveryCtx, cancelDiscovery := codexRolloutDiscoveryContext(ctx)
	candidates, discoveryComplete := codexDiscoverRolloutCandidates(discoveryCtx, base, cursor)
	cancelDiscovery()
	if len(candidates) == 0 {
		return nil, codexUsageLimitEvidence{}, time.Time{}, nil, false
	}
	// Rank candidates by file mtime descending, NOT by filename (= session
	// start time). When sessions overlap — e.g. an older still-active session
	// runs alongside a newer-started but idle one, or a long-lived session is
	// resumed after newer files exist — filename order treats the stale session
	// as the "newest" reading. mtime tracks the last append, so the file being
	// written most recently (the live source of truth) is considered first.
	// Ties fall back to filename order so a deterministic chronological tiebreak
	// applies when two files share an mtime.
	sort.SliceStable(candidates, func(i, j int) bool {
		if !candidates[i].mtime.Equal(candidates[j].mtime) {
			return candidates[i].mtime.After(candidates[j].mtime)
		}
		return candidates[i].path > candidates[j].path
	})
	// Accumulate across files newest-first, keyed by (identity, limit id) — NOT by
	// physical slot. Keying on the slot would let a newer log that only carried a
	// migrated weekly contributor under `primary` block an older log's `primary`
	// session contributor, leaving the session identity Unknown even though a
	// slightly older log still holds it. Deduping by identity+limit keeps the
	// newest reading per contributor while still backfilling identities/limits the
	// newer logs never restated, so downstream identity partitioning sees every
	// distinct reading. Newest-first iteration means the first-seen reading of a
	// given (identity, limit) wins.
	type rolloutContribution struct {
		slot    string
		limitID string
		bucket  codexRateLimitBucket
	}
	winners := map[string]rolloutContribution{}
	var limit codexUsageLimitEvidence
	var latestObservation time.Time
	selected := candidates
	if len(selected) > codexRolloutScanFileCap {
		selected = selected[:codexRolloutScanFileCap]
	}
	allHandled := discoveryComplete
	maxSelectedMtimeNs := int64(0)
	for _, c := range selected {
		if c.mtime.UnixNano() > maxSelectedMtimeNs {
			maxSelectedMtimeNs = c.mtime.UnixNano()
		}
	}
	for _, c := range selected {
		if ctx.Err() != nil {
			allHandled = false
			break
		}
		buckets, sessionStart, fileLimit, handled, ok := codexBucketsFromRolloutFile(ctx, c.path, now)
		if !handled {
			allHandled = false
		}
		// Reject logs whose session began before the current login (a possible
		// prior account). A log with no parseable start time can't be scoped, so
		// keep it (best-effort, matches the unscoped unknown-account path).
		// Applied BEFORE the no-buckets skip so exhaustion evidence is scoped to
		// the current account exactly as usage readings are.
		if !authMod.IsZero() && !sessionStart.IsZero() && sessionStart.Before(authMod) {
			continue
		}
		if fileLimit.At.After(limit.At) {
			limit = fileLimit
		}
		if !ok && !fileLimit.At.IsZero() && now.Sub(fileLimit.At) <= codexUsageLimitNoticeMaxAge {
			// A refusal-only file has no normalized contributor to persist. Keep
			// it above the completed-scan watermark while its notice is live so
			// an unchanged refresh re-reads the bounded rollout set instead of
			// losing the only explanation for otherwise-Unknown usage rows.
			allHandled = false
		}
		if !ok {
			continue
		}
		for w, limits := range buckets {
			for limitID, b := range limits {
				if observed := time.UnixMilli(b.ObservedAtMs); b.ObservedAtMs > 0 && observed.After(latestObservation) {
					latestObservation = observed
				}
				key := codexWindowIdentity(b.WindowMinutes, w) + "\x00" + limitID
				if prev, exists := winners[key]; !exists || b.ObservedAtMs > prev.bucket.ObservedAtMs {
					winners[key] = rolloutContribution{slot: w, limitID: limitID, bucket: b}
				}
			}
		}
		// Do NOT stop once both display identities are present. Winners are keyed by
		// (identity, limit id), and this accumulation exists precisely to backfill
		// distinct metered limits that newer logs never restated. Breaking on
		// identity presence would stop before an older log's separate, stricter
		// weekly limit is seen, dropping it from the weekly identity's
		// most-constrained aggregate and understating usage. The scan is already
		// bounded by codexRolloutScanFileCap, so keep folding every in-cap log and
		// let newest-first (identity, limit) dedup keep the freshest reading per
		// contributor.
	}
	var highWater *codexRolloutScanProgress
	if allHandled {
		highWater = &codexRolloutScanProgress{
			mtimeNs:             maxSelectedMtimeNs,
			boundaryFingerprint: codexRolloutBoundaryFingerprint(candidates, maxSelectedMtimeNs),
		}
	}
	if len(winners) == 0 {
		// No usable window anywhere in the scanned logs — but a quota refusal
		// found along the way still explains WHY, so it is reported even though
		// there is nothing to backfill.
		return nil, limit, latestObservation, highWater, false
	}
	// Rebuild the slot-keyed contributor map downstream expects. Normalize the
	// displayed identities onto their canonical cache slots while preserving the
	// provider's real limit id. Codex can transiently emit both a session and
	// weekly reading under `primary`; inventing a synthetic key for one would no
	// longer match a later sparse update for the real limit, leaving stale evidence
	// behind. Non-canonical durations retain their source slot so the display path
	// can apply its slot-scoped fallback.
	acc := map[string]map[string]codexRateLimitBucket{}
	for _, c := range winners {
		slot := c.slot
		switch codexWindowIdentity(c.bucket.WindowMinutes, c.slot) {
		case codexIdentitySession:
			slot = codexWindowPrimary
		case codexIdentityWeekly:
			slot = codexWindowSecondary
		}
		slotMap := acc[slot]
		if slotMap == nil {
			slotMap = map[string]codexRateLimitBucket{}
			acc[slot] = slotMap
		}
		limitKey := c.limitID
		if _, taken := slotMap[limitKey]; taken {
			// Only non-canonical duration identities can still collide after the
			// displayed rows were normalized above. Preserve both rather than
			// silently dropping one; these plan-specific fallback rows have no
			// stable canonical slot available.
			limitKey += "\x00" + codexWindowIdentity(c.bucket.WindowMinutes, c.slot)
		}
		slotMap[limitKey] = c.bucket
	}
	return acc, limit, latestObservation, highWater, true
}

// codexBucketsFromRolloutFile returns the per-(window, limit) contributors from
// the LAST populated `rate_limits` frame in a single rollout log, plus the
// session's start time (the first line's `timestamp`). Codex emits
// `rate_limits: null` on most token_count events and the real object only
// periodically, so the last non-empty extraction — not the first — is the live
// reading. Contributors are returned un-aggregated so the caller can partition
// them by identity; reset-passed rollover to 0% is applied downstream per
// contributor (codexAggregateIdentity), matching aggregateCodexBuckets. The
// start time is used by the caller to scope logs to the current account.
// Best-effort: returns ok=false when the file holds no usable frame or can't be
// read; the returned start time is zero when no line carried a parseable
// timestamp.
func codexBucketsFromRolloutFile(ctx context.Context, path string, now time.Time) (map[string]map[string]codexRateLimitBucket, time.Time, codexUsageLimitEvidence, bool, bool) {
	f, err := os.Open(path)
	if err != nil {
		// Only a file that definitively vanished is handled progress. Permission
		// failures, descriptor exhaustion, and other open errors can be transient;
		// advancing the watermark past them would suppress a later successful read.
		return nil, time.Time{}, codexUsageLimitEvidence{}, codexRolloutOpenFailureHandled(err), false
	}
	defer f.Close()

	var sessionStart time.Time
	var limit codexUsageLimitEvidence
	acc := map[string]map[string]codexRateLimitBucket{}
	consumeLine := func(line string) {
		// Exhaustion evidence is collected from the SAME pass, ahead of the
		// rate-limit prefilter: a refused turn carries no window at all (Codex
		// sends `primary: null, secondary: null` once the limit is reached), so
		// these lines are exactly the ones the bucket scan discards.
		if ev, ok := codexUsageLimitEvidenceFromLine(line); ok && ev.At.After(limit.At) {
			limit = ev
		}
		// The first line carrying a timestamp is the session_meta header, i.e.
		// when this session STARTED — recorded before any rate-limit frame, so
		// it's captured ahead of the prefilter below.
		if sessionStart.IsZero() {
			if ts, ok := codexRolloutLineTimestamp(line); ok {
				sessionStart = ts
			}
		}
		// Cheap prefilter: only decode lines that could carry a window update.
		// Mirror captureCodexRateLimitLine's gate exactly so camelCase frames
		// (`rateLimits` / `rateLimitsByLimitId`) the extractor supports aren't
		// dropped — `rate_limit` alone wouldn't match the camelCase spelling.
		if !strings.Contains(line, "token_count") &&
			!strings.Contains(line, "rateLimits") &&
			!strings.Contains(line, "rate_limit") {
			return
		}
		var raw map[string]interface{}
		if json.Unmarshal([]byte(line), &raw) != nil {
			return
		}
		if !isRecognizedCodexRateLimitEnvelope(raw) {
			return
		}
		// Anchor relative reset fields (`resets_in_seconds`) to the moment the
		// line was EMITTED, not the usage-refresh time. A historical rollout
		// line saying "resets in 3600s" reset an hour after it was written; with
		// the refresh time as the anchor it would falsely look like it resets an
		// hour from now, masking a window that has long since rolled over.
		// Absolute `resets_at` fields ignore this anchor, so the fallback is
		// when the line carries no parseable timestamp.
		eventTime, observedAt := codexObservationTimes(raw, now, false)
		if eventTime.IsZero() {
			// Numeric rollout telemetry without an enclosing event time cannot
			// advance freshness. Drop this object only and continue later lines.
			return
		}
		// The rollout shape nests telemetry under `payload`
		// ({"type":"event_msg","payload":{"type":"token_count","rate_limits":…}}),
		// which extractCodexRateLimitBuckets already unwraps. fullSnapshot is
		// false for that envelope, so null windows are ignored rather than
		// treated as clears — exactly what we want when mining for live usage.
		if updates, _ := extractCodexRateLimitBuckets(raw, eventTime); len(updates) > 0 {
			codexStampContributorObservations(updates, observedAt)
			// Merge, don't replace: token_count notifications are sparse, so a
			// later frame restating only `primary` must not drop a `secondary`
			// reading an earlier frame in this same file already captured.
			// Liveness for sparse-merge is judged at the frame's own event
			// time so an expired prior reset isn't carried onto fresh usage.
			mergeCodexRolloutFrame(acc, updates, eventTime)
		}
	}

	// Scanner cannot recover after an oversized token and cannot be canceled
	// while accumulating it. Read in bounded fragments instead: complete JSONL
	// objects up to the existing 30 MB ceiling are consumed, oversized objects
	// are discarded through their newline, and cancellation preserves earlier
	// complete evidence while leaving the file unhandled for retry.
	reader := bufio.NewReaderSize(f, 64*1024)
	lineBytes := make([]byte, 0, 64*1024)
	oversized := false
	for {
		if ctx.Err() != nil {
			return acc, sessionStart, limit, false, len(acc) > 0
		}
		fragment, readErr := reader.ReadSlice('\n')
		if !oversized {
			if len(lineBytes)+len(fragment) > codexAppServerMaxLineSize {
				lineBytes = lineBytes[:0]
				oversized = true
			} else {
				lineBytes = append(lineBytes, fragment...)
			}
		}
		if readErr == bufio.ErrBufferFull {
			continue
		}
		if readErr != nil && readErr != io.EOF {
			return acc, sessionStart, limit, false, len(acc) > 0
		}
		if !oversized && len(lineBytes) > 0 {
			for len(lineBytes) > 0 && (lineBytes[len(lineBytes)-1] == '\n' || lineBytes[len(lineBytes)-1] == '\r') {
				lineBytes = lineBytes[:len(lineBytes)-1]
			}
			if len(lineBytes) > 0 {
				consumeLine(string(lineBytes))
			}
		}
		lineBytes = lineBytes[:0]
		oversized = false
		if readErr == io.EOF {
			break
		}
	}
	if ctx.Err() != nil {
		return acc, sessionStart, limit, false, len(acc) > 0
	}
	if len(acc) == 0 {
		// ok=false means "no usable window here", NOT "nothing here": a log whose
		// every turn was refused for quota is precisely the case that produces no
		// buckets AND the evidence the card needs, so the evidence is returned
		// alongside the miss.
		return nil, sessionStart, limit, true, false
	}
	// Return the per-limit contributors un-collapsed. Rollover-to-0% for a window
	// whose reset already passed as of `now` is applied by the display path
	// (codexAggregateIdentity), so a stale relative reset anchored above still
	// clears instead of showing old usage — without flattening two distinct
	// identities that share a storage slot into one bucket here.
	return acc, sessionStart, limit, true, true
}

func codexRolloutOpenFailureHandled(err error) bool {
	return os.IsNotExist(err)
}

// codexUsageLimitEvidence records that Codex refused a turn because the
// account's quota was exhausted, as seen in a rollout log. `At` is the event's
// own timestamp (zero = no evidence); `Message` is Codex's own user-facing text,
// which names the retry time and the top-up link.
type codexUsageLimitEvidence struct {
	At      time.Time
	Message string
}

// codexUsageLimitErrorCode is the `codex_error_info` value Codex attaches to a
// turn it refused because the account is out of quota.
const codexUsageLimitErrorCode = "usage_limit_exceeded"

// codexUsageLimitMessageMaxLen bounds how much of Codex's message the card may
// carry. Long enough for the real text ("You've hit your usage limit. Visit
// <url> to purchase more credits or try again at 11:28 PM."), short enough that
// a future upstream paragraph can't bloat every device document.
const codexUsageLimitMessageMaxLen = 240

// codexUsageLimitEvidenceFromLine extracts quota-exhaustion evidence from one
// rollout JSONL line. Codex reports it on the turn's terminal event:
//
//	{"timestamp":…,"type":"event_msg","payload":{"type":"task_complete",
//	  "error":{"message":"You've hit your usage limit…",
//	           "codex_error_info":"usage_limit_exceeded"}}}
//
// The code — not the prose — is what we match on, so a reworded message keeps
// working and a message merely MENTIONING a limit is never mistaken for one.
func codexUsageLimitEvidenceFromLine(line string) (codexUsageLimitEvidence, bool) {
	// Cheap prefilter: the code is a literal, so a line that doesn't contain it
	// cannot be evidence and is never decoded.
	if !strings.Contains(line, codexUsageLimitErrorCode) {
		return codexUsageLimitEvidence{}, false
	}
	var raw map[string]interface{}
	if json.Unmarshal([]byte(line), &raw) != nil {
		return codexUsageLimitEvidence{}, false
	}
	at, hasTime := codexRolloutLineTimestamp(line)
	if !hasTime {
		// Without a timestamp the evidence can't be ranked against a usage
		// reading, and stale exhaustion must never outrank fresh telemetry.
		return codexUsageLimitEvidence{}, false
	}
	// The rollout envelope nests the event under `payload`; the app-server
	// streams the same object at the top level.
	scopes := []map[string]interface{}{raw}
	if payload, ok := raw["payload"].(map[string]interface{}); ok {
		scopes = append(scopes, payload)
	}
	for _, scope := range scopes {
		errObj, ok := scope["error"].(map[string]interface{})
		if !ok {
			continue
		}
		code, _ := pickField(errObj, "codex_error_info", "codexErrorInfo")
		if s, _ := code.(string); s != codexUsageLimitErrorCode {
			continue
		}
		message, _ := errObj["message"].(string)
		message = strings.TrimSpace(message)
		if len(message) > codexUsageLimitMessageMaxLen {
			message = strings.TrimSpace(message[:codexUsageLimitMessageMaxLen])
		}
		return codexUsageLimitEvidence{At: at, Message: message}, true
	}
	return codexUsageLimitEvidence{}, false
}

// codexRolloutLineTimestamp extracts the top-level `timestamp` (RFC3339) from
// one rollout JSONL line. Used to read a session's start time without decoding
// the whole line. ok=false when the line has no parseable timestamp.
func codexRolloutLineTimestamp(line string) (time.Time, bool) {
	var probe struct {
		Timestamp string `json:"timestamp"`
	}
	if json.Unmarshal([]byte(line), &probe) != nil || probe.Timestamp == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, probe.Timestamp)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// mergeCodexRolloutFrame folds one frame's per-(window, limit) contributors into
// the accumulated snapshot for a single rollout file, latest-wins, mirroring the
// live cache's sparse-merge semantics:
//
//   - A reset-only update within the SAME LIVE window (prior reset still in the
//     future as of this frame's event time, and within jitter of the new reset)
//     carries the prior usage forward; a usage-only update keeps the prior reset
//     only while it's still live. Window-length hints survive from either side.
//   - A bucket with no known usage is NEVER stored as a standalone observed
//     contributor — the live path ignores reset-only updates that have no prior
//     same-window usage to merge into, and so do we. Storing one would make
//     codexMetricFromBucket report a bogus 0% and block an older file
//     from filling the real usage.
//   - When a reset-only update jumps to a NEW window (reset beyond jitter), the
//     prior reading is stale: drop it so it can't keep rendering an expired
//     percentage, and leave the window unobserved until a real usage frame lands.
//   - A usage-only update arriving after the prior window has already expired
//     stands on its own — copying the expired prev reset would make
//     codexMetricFromBucket zero out the fresh usage as rolled over.
//
// Windows/limits the frame doesn't mention are left untouched (rollout frames
// never clear). `frameTime` is the line's own timestamp so liveness is judged
// at the moment the frame was emitted, not at refresh time.
func mergeCodexRolloutFrame(acc, updates map[string]map[string]codexRateLimitBucket, frameTime time.Time) {
	frameMs := frameTime.UnixMilli()
	for window, contributors := range updates {
		for limit, b := range contributors {
			var prev codexRateLimitBucket
			hadPrev := false
			if acc[window] != nil {
				prev, hadPrev = acc[window][limit]
			}
			priorStillLive := hadPrev && prev.resetKnown && prev.ResetsAtMs > frameMs
			sameWindow := hadPrev && (!b.resetKnown || !prev.resetKnown ||
				resetsWithinJitter(b.ResetsAtMs, prev.ResetsAtMs))
			sameLiveWindow := priorStillLive && (!b.resetKnown || resetsWithinJitter(b.ResetsAtMs, prev.ResetsAtMs))

			if !b.usageKnown && sameLiveWindow && prev.usageKnown {
				b.UsedPercentage = prev.UsedPercentage
				// The sparse frame re-observed the reset, not utilization. Keep the
				// carried percentage paired with its original observation time so
				// repeated rollout heartbeats cannot make stale usage appear fresh.
				b.ObservedAtMs = prev.ObservedAtMs
				b.usageKnown = true
			}
			if !b.resetKnown && priorStillLive {
				b.ResetsAtMs = prev.ResetsAtMs
				b.resetKnown = true
			}
			if b.WindowMinutes == 0 && hadPrev && prev.WindowMinutes > 0 {
				b.WindowMinutes = prev.WindowMinutes
			}

			if !b.usageKnown {
				// Reset-only update with no usage to anchor it. Drop a stale
				// prior when the window rolled over; otherwise ignore.
				if hadPrev && b.resetKnown && !sameWindow {
					delete(acc[window], limit)
					if len(acc[window]) == 0 {
						delete(acc, window)
					}
				}
				continue
			}
			if acc[window] == nil {
				acc[window] = map[string]codexRateLimitBucket{}
			}
			acc[window][limit] = b
		}
	}
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
