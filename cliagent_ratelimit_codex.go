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
type codexRateLimitSnapshot struct {
	UpdatedAt          string                          `json:"updatedAt"`
	AccountFingerprint string                          `json:"accountFingerprint,omitempty"`
	Buckets            map[string]codexRateLimitBucket `json:"buckets"`
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
func extractCodexRateLimitBuckets(raw map[string]interface{}, now time.Time) (map[string]codexRateLimitBucket, map[string]bool) {
	out := map[string]codexRateLimitBucket{}
	clears := map[string]bool{}

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
					mergeCodexBucketMostConstrained(out, id, b)
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
					b, ok := codexBucketFromInfo(info, now)
					if !ok {
						continue
					}
					id := classifyCodexByLimitBucket(limitKey, b.WindowMinutes)
					if id == "" {
						continue
					}
					mergeCodexBucketMostConstrained(out, id, b)
				}
			}
		}
	}

	return out, clears
}

// mergeCodexBucketMostConstrained writes `b` into `out[id]` unless an existing
// bucket already reports a higher observed utilisation — picking the most
// constrained view keeps multi-bucket payloads from understating usage.
func mergeCodexBucketMostConstrained(out map[string]codexRateLimitBucket, id string, b codexRateLimitBucket) {
	prev, exists := out[id]
	if !exists {
		out[id] = b
		return
	}
	if !b.usageKnown {
		return
	}
	if !prev.usageKnown || b.UsedPercentage > prev.UsedPercentage {
		out[id] = b
	}
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
	mergeCodexRateLimitCache(codexRateLimitCachePath(), updates, clears, now, currentCodexAccountFingerprint())
}

// mergeCodexRateLimitCache read-modify-writes the cache, overwriting only the
// windows present in `updates` and leaving the rest intact. Windows named in
// `clears` are deleted from the cache — Codex uses `secondary: null` in a
// full account/rateLimits/read response to mean "this window does not apply
// to the account," so we must drop any stale bucket instead of leaving it to
// render until its old reset passes. When the existing snapshot was captured
// under a different account fingerprint, its buckets are discarded — a
// previous account's reset times must not bleed into the new account's
// display.
func mergeCodexRateLimitCache(path string, updates map[string]codexRateLimitBucket, clears map[string]bool, now time.Time, fingerprint string) {
	if path == "" || (len(updates) == 0 && len(clears) == 0) {
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

	snap := codexRateLimitSnapshot{Buckets: map[string]codexRateLimitBucket{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &snap)
		if snap.Buckets == nil {
			snap.Buckets = map[string]codexRateLimitBucket{}
		}
	}
	if snap.AccountFingerprint != fingerprint {
		snap.Buckets = map[string]codexRateLimitBucket{}
	}
	// Drop windows the frame explicitly cleared (null in a full read response)
	// before applying updates: a clear in this snapshot wins over any cached
	// state for that window — leaving it would render stale usage/reset until
	// the old reset passes. Updates apply afterwards so a single full snapshot
	// that both clears one window and refreshes another behaves correctly.
	for window := range clears {
		delete(snap.Buckets, window)
	}
	nowMs := now.UnixMilli()
	for window, bucket := range updates {
		// Codex's account/rateLimits/updated is sparse: a notification may
		// carry only a fresh used_percent OR only a fresh reset time. Merge
		// per field so a usage-only update doesn't clobber the live reset,
		// and a reset-only update doesn't reset Consumed to 0%. A prior
		// reading is only carried forward when it still describes a LIVE
		// window (prior reset in the future); otherwise it's stale and we
		// let the partial new bucket stand.
		prev, hadPrev := snap.Buckets[window]
		priorStillLive := hadPrev && prev.ResetsAtMs > nowMs
		// A reset-only update only describes the SAME live window when its
		// new reset matches the prior reset (or omits a reset entirely). If
		// it advances resetsAt past the prior reset, the quota window has
		// rolled over: the prior used % belongs to the old window and must
		// NOT be copied onto the fresh one — doing so makes the card keep
		// showing the previous high usage for what is actually an empty
		// new 5-hour / weekly window.
		sameLiveWindow := priorStillLive && (!bucket.resetKnown || bucket.ResetsAtMs == prev.ResetsAtMs)
		if !bucket.usageKnown && sameLiveWindow {
			bucket.UsedPercentage = prev.UsedPercentage
		}
		if !bucket.resetKnown && priorStillLive {
			bucket.ResetsAtMs = prev.ResetsAtMs
		}
		// WindowMinutes describes the rolling-window length and rarely
		// changes within an account, so carry it forward whenever the new
		// update doesn't restate it — same intent as the usage/reset
		// preservation above.
		if bucket.WindowMinutes == 0 && hadPrev && prev.WindowMinutes > 0 {
			bucket.WindowMinutes = prev.WindowMinutes
		}
		// Reset-only update with no usage carried from the same live window:
		// persisting now would seed a fake observed 0% used (the zero-value
		// UsedPercentage), so the next refresh would render the quota as
		// 0% / 100% remaining even though no usage value was ever observed
		// for this window. Leave it Unknown until a real used_percent /
		// utilization arrives. This covers both "no prior bucket" and
		// "prior bucket belonged to a now-rolled-over window" — in the
		// rolled-over case we additionally evict the stale prior bucket
		// rather than letting it linger on the card until the old reset
		// passes.
		if !bucket.usageKnown && !sameLiveWindow {
			if hadPrev && bucket.resetKnown && bucket.ResetsAtMs != prev.ResetsAtMs {
				delete(snap.Buckets, window)
			}
			continue
		}
		snap.Buckets[window] = bucket
	}
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
		buckets = snap.Buckets
	}

	session := codexObservedMetricOrUnknown(
		buckets, codexWindowPrimary, limitKindSession, "5-hour session window", now)
	weekly := codexObservedMetricOrUnknown(
		buckets, codexWindowSecondary, limitKindWeekly, "Weekly quota", now)

	return []cliAgentUsageMetric{session, weekly}
}

// codexWindowLabel renders a human label for a window of the given length.
// We special-case the canonical Codex windows (300 min = 5 hours, 10080 min =
// weekly) so the long-standing labels stay identical, and derive a neutral
// "Nm/Nh/Nd window" string otherwise so an off-spec plan still shows the right
// quota window context instead of the wrong hard-coded one.
func codexWindowLabel(minutes float64, fallback string) string {
	if minutes <= 0 {
		return fallback
	}
	m := int(minutes + 0.5)
	switch m {
	case 300:
		return "5-hour session window"
	case 10080:
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
