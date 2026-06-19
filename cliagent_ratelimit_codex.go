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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Codex rate-limit window identifiers, as emitted by the app-server's
// `token_count.rate_limits` map. We normalise upstream aliases (`5h`,
// `7d`/`weekly`) onto these stable internal keys so cliagent_usage_codex.go
// can lookup-by-id without re-implementing the alias table.
const (
	codexWindowPrimary   = "primary"
	codexWindowSecondary = "secondary"
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
		if f, ok := numAsFloat(v); ok {
			b.UsedPercentage = clampPercent(f)
			usageObserved = true
		}
	} else if v, ok := info["utilization"]; ok {
		if f, ok := numAsFloat(v); ok {
			b.UsedPercentage = clampPercent(f * 100)
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
	out := map[string]map[string]codexRateLimitBucket{}
	clears := map[string]bool{}
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
		// typed event payloads carried inside a JSON-RPC frame.
		if v, ok := m["msg"]; ok {
			if mm, ok := v.(map[string]interface{}); ok {
				candidates = append(candidates, candidate{src: mm, fullSnapshot: fullSnap})
			}
		}
	}

	for _, c := range candidates {
		if v, ok := pickField(c.src, "rate_limits", "rateLimits"); ok {
			if rl, ok := v.(map[string]interface{}); ok {
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

	return out, clears
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
// so codexObservedMetricOrUnknown still recognises the rollover at display
// time when no other contributor is live.
func aggregateCodexBuckets(perLimit map[string]map[string]codexRateLimitBucket, now time.Time) map[string]codexRateLimitBucket {
	out := map[string]codexRateLimitBucket{}
	nowMs := now.UnixMilli()
	for window, contributors := range perLimit {
		for _, b := range contributors {
			if b.ResetsAtMs > 0 && nowMs >= b.ResetsAtMs {
				b.UsedPercentage = 0
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
	}

	usageTie := prev.usageKnown && b.UsedPercentage == prev.UsedPercentage
	switch {
	case b.resetKnown && prev.resetKnown:
		if b.ResetsAtMs > prev.ResetsAtMs {
			merged.ResetsAtMs = b.ResetsAtMs
		} else {
			merged.ResetsAtMs = prev.ResetsAtMs
		}
		merged.resetKnown = true
	case usageTie:
		// Same exhaustion, only one side has a reset hint — don't promise a
		// time the unknown side can't confirm; render "—" instead.
		merged.ResetsAtMs = 0
		merged.resetKnown = false
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
	updates, clears := extractCodexRateLimitBuckets(raw, now)
	if len(updates) == 0 && len(clears) == 0 {
		return
	}
	mergeCodexRateLimitCachePerLimit(codexRateLimitCachePath(), updates, clears, now, currentCodexAccountFingerprint())
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
		perLimit[window] = map[string]codexRateLimitBucket{codexLegacyLimitID: bucket}
	}
	mergeCodexRateLimitCachePerLimit(path, perLimit, clears, now, fingerprint)
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
func mergeCodexRateLimitCachePerLimit(
	path string,
	perLimit map[string]map[string]codexRateLimitBucket,
	clears map[string]bool,
	now time.Time,
	fingerprint string,
) {
	if path == "" || (len(perLimit) == 0 && len(clears) == 0) {
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
			priorStillLive := hadPrev && prev.ResetsAtMs > nowMs
			sameLiveWindow := priorStillLive && (!bucket.resetKnown || resetsWithinJitter(bucket.ResetsAtMs, prev.ResetsAtMs))
			if !bucket.usageKnown && sameLiveWindow {
				bucket.UsedPercentage = prev.UsedPercentage
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
	// Recompute the flat aggregate from contributors so callers reading the
	// cache (codexMetricsFromCache, tests) see the most-constrained view.
	snap.Buckets = aggregateCodexBuckets(snap.Contributors, now)
	snap.UpdatedAt = now.UTC().Format(time.RFC3339)
	snap.AccountFingerprint = fingerprint

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
// the same fingerprint codexUsageParser.Parse would attach to a usage
// snapshot. Used by the capture path to scope the rate-limit cache to the
// active account. Returns "" when no auth is readable, in which case the
// cache is unscoped (best-effort, matches the Claude analog).
func currentCodexAccountFingerprint() string {
	home, _ := os.UserHomeDir()
	base := firstNonEmpty(os.Getenv("CODEX_HOME"), expandHome(home, ".codex"))
	if base == "" {
		return ""
	}
	auth := codexAuth{}
	if !readJSONFile(expandHome(base, "auth.json"), &auth) {
		return ""
	}
	claims := codexIDTokenClaims{}
	parseJWTClaims(auth.Tokens.IDToken, &claims)
	account := firstNonEmpty(
		auth.Email,
		auth.Account,
		auth.UserID,
		claims.Email,
		claims.Account,
		claims.UserID,
		auth.Tokens.AccountID,
		claims.Subject,
	)
	return fingerprintAccount("codex", account)
}

// codexMetricsFromCache builds the metric rows from the rate-limit cache,
// falling back to the Unknown placeholders when a window hasn't been observed.
// Two rows are always shown so the card layout is stable: the 5-hour session
// window (primary) and the weekly window (secondary).
//
// The cache is trusted only when its `accountFingerprint` exactly matches the
// caller-supplied one — otherwise a previous account's windows could surface
// under the current account after a credentials swap.
func codexMetricsFromCache(now time.Time, currentFingerprint string) []cliAgentUsageMetric {
	snap, ok := loadCodexRateLimitSnapshot(codexRateLimitCachePath())
	buckets := map[string]codexRateLimitBucket{}
	if ok && snap.AccountFingerprint == currentFingerprint {
		// Prefer Contributors when present so a sparse update for one limit
		// can't shadow a stricter prior contributor that the sparse frame
		// never restated. Fall back to the flat aggregate for any legacy
		// cache file written before the contributors map existed.
		if len(snap.Contributors) > 0 {
			reflagged := make(map[string]map[string]codexRateLimitBucket, len(snap.Contributors))
			for w, contribs := range snap.Contributors {
				windowMap := make(map[string]codexRateLimitBucket, len(contribs))
				for limit, b := range contribs {
					windowMap[limit] = reflagPersistedCodexBucket(b)
				}
				reflagged[w] = windowMap
			}
			buckets = aggregateCodexBuckets(reflagged, now)
		} else {
			buckets = snap.Buckets
		}
	}

	session := codexObservedMetricOrUnknown(
		buckets, codexWindowPrimary, limitKindSession, "5-hour session window", now)
	weekly := codexObservedMetricOrUnknown(
		buckets, codexWindowSecondary, limitKindWeekly, "Weekly quota", now)

	return []cliAgentUsageMetric{session, weekly}
}

// codexRolloutScanFileCap bounds how many of the most-recent rollout logs the
// fallback opens before giving up, so a sessions directory holding thousands of
// files can't turn a usage refresh into a long scan. The newest log carrying a
// populated rate_limits frame wins, so in practice this resolves on the first
// file or two.
const codexRolloutScanFileCap = 16

// codexBackfillUnknownFromRollout fills any Unknown window in `metrics` from
// Codex's own on-disk session rollout logs.
//
// Why this exists: our live cache (codex_rate_limits.json) is fed ONLY by the
// passive app-server capture in codex_appserver.go, so it stays empty whenever
// Codex is driven through its own TUI (which never streams through our
// app-server). Codex persists the exact same `token_count.rate_limits`
// telemetry to its rollout logs, so a window we never observed live can still
// be reported from disk — the same source Codex's own `/status` reads.
//
// A live captured bucket is authoritative: only windows that came back Unknown
// are backfilled, and the rollout scan runs at most once per call. Returns the
// input slice unchanged when nothing is Unknown or no rollout telemetry exists.
func codexBackfillUnknownFromRollout(metrics []cliAgentUsageMetric, base string, now time.Time) []cliAgentUsageMetric {
	anyUnknown := false
	for _, m := range metrics {
		if m.Unknown {
			anyUnknown = true
			break
		}
	}
	if !anyUnknown {
		return metrics
	}

	buckets, ok := codexRolloutFallbackBuckets(base, now)
	if !ok {
		return metrics
	}

	for i, m := range metrics {
		if !m.Unknown {
			continue
		}
		var windowID string
		switch m.Kind {
		case limitKindSession:
			windowID = codexWindowPrimary
		case limitKindWeekly:
			windowID = codexWindowSecondary
		default:
			continue
		}
		if _, present := buckets[windowID]; !present {
			continue
		}
		// Reuse the cache-path renderer so rollout-sourced metrics get the same
		// window-label derivation and reset-passed → 0% rollover handling. The
		// metric's existing label is the default Codex window label, which is
		// the right fallback when the frame carried no window_minutes hint.
		metrics[i] = codexObservedMetricOrUnknown(buckets, windowID, m.Kind, m.Label, now)
	}
	return metrics
}

// codexRolloutFallbackBuckets reads Codex's session rollout logs
// (CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl) for the most recent populated
// `rate_limits` frame and returns aggregated display-window buckets.
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
func codexRolloutFallbackBuckets(base string, now time.Time) (map[string]codexRateLimitBucket, bool) {
	if base == "" {
		return nil, false
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
	// filepath.Glob returns lexically-sorted paths; every segment is zero-padded
	// and the filename is ISO-timestamp-prefixed, so lexical order is
	// chronological order and the tail of the slice is the newest session.
	matches, err := filepath.Glob(filepath.Join(base, "sessions", "*", "*", "*", "rollout-*.jsonl"))
	if err != nil || len(matches) == 0 {
		return nil, false
	}
	// Accumulate across files newest-first: a window the newest log never
	// restated (e.g. it only carried `primary`) can still be filled from a
	// slightly older log that did carry `secondary`. The newest reading for a
	// given window wins, so once a window is in `acc` an older file never
	// overwrites it.
	acc := map[string]codexRateLimitBucket{}
	for i, scanned := len(matches)-1, 0; i >= 0 && scanned < codexRolloutScanFileCap; i, scanned = i-1, scanned+1 {
		buckets, sessionStart, ok := codexBucketsFromRolloutFile(matches[i], now)
		if !ok {
			continue
		}
		// Reject logs whose session began before the current login (a possible
		// prior account). A log with no parseable start time can't be scoped, so
		// keep it (best-effort, matches the unscoped unknown-account path).
		if !authMod.IsZero() && !sessionStart.IsZero() && sessionStart.Before(authMod) {
			continue
		}
		for w, b := range buckets {
			if _, exists := acc[w]; !exists {
				acc[w] = b
			}
		}
		_, hasPrimary := acc[codexWindowPrimary]
		_, hasSecondary := acc[codexWindowSecondary]
		if hasPrimary && hasSecondary {
			break
		}
	}
	if len(acc) == 0 {
		return nil, false
	}
	return acc, true
}

// codexBucketsFromRolloutFile returns the aggregated buckets from the LAST
// populated `rate_limits` frame in a single rollout log, plus the session's
// start time (the first line's `timestamp`). Codex emits `rate_limits: null` on
// most token_count events and the real object only periodically, so the last
// non-empty extraction — not the first — is the live reading. The start time is
// used by the caller to scope logs to the current account. Best-effort: returns
// ok=false when the file holds no usable frame or can't be read; the returned
// start time is zero when no line carried a parseable timestamp.
func codexBucketsFromRolloutFile(path string, now time.Time) (map[string]codexRateLimitBucket, time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, time.Time{}, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Rollout lines embed full model turns and can be large; match the
	// app-server's ceiling so a single big line doesn't abort the scan mid-file.
	scanner.Buffer(make([]byte, 0, 64*1024), codexAppServerMaxLineSize)

	var sessionStart time.Time
	acc := map[string]map[string]codexRateLimitBucket{}
	for scanner.Scan() {
		line := scanner.Text()
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
			continue
		}
		var raw map[string]interface{}
		if json.Unmarshal([]byte(line), &raw) != nil {
			continue
		}
		// Anchor relative reset fields (`resets_in_seconds`) to the moment the
		// line was EMITTED, not the usage-refresh time. A historical rollout
		// line saying "resets in 3600s" reset an hour after it was written; with
		// the refresh time as the anchor it would falsely look like it resets an
		// hour from now, masking a window that has long since rolled over.
		// Absolute `resets_at` fields ignore this anchor, so the fallback is
		// when the line carries no parseable timestamp.
		eventTime := now
		if ts, ok := raw["timestamp"].(string); ok {
			if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
				eventTime = parsed
			}
		}
		// The rollout shape nests telemetry under `payload`
		// ({"type":"event_msg","payload":{"type":"token_count","rate_limits":…}}),
		// which extractCodexRateLimitBuckets already unwraps. fullSnapshot is
		// false for that envelope, so null windows are ignored rather than
		// treated as clears — exactly what we want when mining for live usage.
		if updates, _ := extractCodexRateLimitBuckets(raw, eventTime); len(updates) > 0 {
			// Merge, don't replace: token_count notifications are sparse, so a
			// later frame restating only `primary` must not drop a `secondary`
			// reading an earlier frame in this same file already captured.
			mergeCodexRolloutFrame(acc, updates)
		}
	}
	if len(acc) == 0 {
		return nil, sessionStart, false
	}
	// Roll over against the real current time: aggregateCodexBuckets zeroes any
	// window whose reset is already in the past as of now, so a stale relative
	// reset anchored above correctly clears instead of showing old usage.
	return aggregateCodexBuckets(acc, now), sessionStart, true
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
//   - A reset-only update within the SAME window (new reset within jitter of the
//     prior) carries the prior usage forward; a usage-only update keeps the
//     prior reset. Window-length hints survive from either side.
//   - A bucket with no known usage is NEVER stored as a standalone observed
//     contributor — the live path ignores reset-only updates that have no prior
//     same-window usage to merge into, and so do we. Storing one would make
//     codexObservedMetricOrUnknown report a bogus 0% and block an older file
//     from filling the real usage.
//   - When a reset-only update jumps to a NEW window (reset beyond jitter), the
//     prior reading is stale: drop it so it can't keep rendering an expired
//     percentage, and leave the window unobserved until a real usage frame lands.
//
// Windows/limits the frame doesn't mention are left untouched (rollout frames
// never clear).
func mergeCodexRolloutFrame(acc, updates map[string]map[string]codexRateLimitBucket) {
	for window, contributors := range updates {
		for limit, b := range contributors {
			var prev codexRateLimitBucket
			hadPrev := false
			if acc[window] != nil {
				prev, hadPrev = acc[window][limit]
			}
			sameWindow := hadPrev && (!b.resetKnown || !prev.resetKnown ||
				resetsWithinJitter(b.ResetsAtMs, prev.ResetsAtMs))

			if !b.usageKnown && hadPrev && sameWindow && prev.usageKnown {
				b.UsedPercentage = prev.UsedPercentage
				b.usageKnown = true
			}
			if !b.resetKnown && hadPrev && prev.resetKnown {
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
	m := int(minutes + 0.5)
	switch {
	case m >= 295 && m <= 305:
		return "5-hour session window"
	case m >= 10020 && m <= 10140:
		return "Weekly quota"
	}
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

// codexObservedMetricOrUnknown returns a real percentage metric for the given
// window id, or an Unknown placeholder when it is unobserved. A window whose
// reset time has already passed is reported as 0% used (the window rolled
// over), matching Claude's observedMetricOrUnknown behavior.
func codexObservedMetricOrUnknown(
	buckets map[string]codexRateLimitBucket,
	windowID, kind, defaultLabel string,
	now time.Time,
) cliAgentUsageMetric {
	b, ok := buckets[windowID]
	if !ok {
		return cliAgentUsageMetric{Kind: kind, Label: defaultLabel, Unit: "%", Unknown: true}
	}
	used := b.UsedPercentage
	var resetAt string
	if b.ResetsAtMs > 0 {
		if now.UnixMilli() >= b.ResetsAtMs {
			used = 0
		} else {
			resetAt = time.UnixMilli(b.ResetsAtMs).UTC().Format(time.RFC3339)
		}
	}
	used = clampPercent(used)
	return cliAgentUsageMetric{
		Kind:      kind,
		Label:     codexWindowLabel(b.WindowMinutes, defaultLabel),
		Unit:      "%",
		Total:     floatPtr(100),
		Consumed:  floatPtr(used),
		Remaining: floatPtr(100 - used),
		ResetAt:   resetAt,
	}
}
