// cliagent_ratelimit.go — passively captures Claude Code's structured
// rate-limit telemetry from the stream-json output we already parse during a
// session, and caches the latest per-window snapshot to disk.
//
// Why this exists:
//
//	Claude Code does NOT expose a usage API or an on-disk usage file. It DOES,
//	however, emit a `rate_limit_event` on its stream-json stdout (the same
//	stream session.go already reads), carrying the exact reset time and
//	utilization per limit window. The Agent SDK models this as RateLimitInfo:
//	    { status, resets_at, rate_limit_type, utilization }
//	and the status line surface emits the equivalent
//	    rate_limits.<window>.{ used_percentage, resets_at }.
//	We accept BOTH shapes so a future Claude Code version that moves the data
//	between surfaces keeps working. Field names are also matched in both
//	snake_case and camelCase: the rejected `rate_limit_event` upstream uses
//	camelCase (`rateLimitType`, `resetsAt`) while the SDK / status-line shape
//	uses snake_case — both must be parsed or rejected sessions go uncaptured.
//
// Two consumers read the cache this writes:
//  1. cliagent_usage_claudecode.go — turns the snapshot into the real
//     five-hour / weekly capacity metrics shown on the CLI Agents tab
//     (replacing the old "usage unobservable" dashed bars).
//  2. session.go — when a window reports status "rejected" (hard limit hit),
//     emits a synthetic, orchestrator-parseable limit line carrying the exact
//     reset timestamp so agent-orchestrator-service auto-defers and resumes
//     the execution after the window resets (instead of completing empty).
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Claude rate-limit window identifiers, as emitted by Claude Code's
// rate_limit_event.rate_limit_type and the status-line rate_limits map keys.
const (
	claudeWindowFiveHour       = "five_hour"
	claudeWindowSevenDay       = "seven_day"
	claudeWindowSevenDayOpus   = "seven_day_opus"
	claudeWindowSevenDaySonnet = "seven_day_sonnet"
)

// claudeRateLimitStatusRejected is the status value Claude Code sets once a
// window's limit is exhausted and further requests are blocked.
const claudeRateLimitStatusRejected = "rejected"

// claudeRateLimitBucket is one window's observed state. UsedPercentage is
// normalised to 0..100 regardless of source shape (utilization 0..1 vs
// used_percentage 0..100). ResetsAtMs is unix epoch milliseconds (0 = unknown).
type claudeRateLimitBucket struct {
	UsedPercentage float64 `json:"usedPercentage"`
	ResetsAtMs     int64   `json:"resetsAtMs"`
	Status         string  `json:"status,omitempty"`
	ObservedAtMs   int64   `json:"observedAtMs"`
	// usageKnown is true when this bucket carries a fresh usage reading
	// (utilization / used_percentage) or a synthesized 100% from a rejected
	// status. False marks an "allowed heartbeat" that updated only the reset
	// time / status — merge must NOT let its 0 default overwrite a previously
	// observed UsedPercentage. Not persisted: every loaded bucket is treated
	// as a known usage reading.
	usageKnown bool `json:"-"`
}

// claudeRateLimitSnapshot is the on-disk cache, keyed by window id.
// AccountFingerprint pins the snapshot to the Claude account that produced it
// — when the local creds change, a stale weekly/future-reset bucket must NOT
// be attributed to the new account (the CLI Agents tab would otherwise show
// another user's capacity until the new account emits its own telemetry).
type claudeRateLimitSnapshot struct {
	UpdatedAt          string                           `json:"updatedAt"`
	AccountFingerprint string                           `json:"accountFingerprint,omitempty"`
	Buckets            map[string]claudeRateLimitBucket `json:"buckets"`
}

// claudeRateLimitMu serialises the read-modify-write of the cache file. A
// single device can run several Claude sessions concurrently, each streaming
// its own rate_limit_event lines.
var claudeRateLimitMu sync.Mutex

// claudeRateLimitCachePath is the cache location inside the agent's data dir.
// AIEXPEDITE_CLAUDE_RL_CACHE overrides it (tests isolate from the real machine
// cache; ops can relocate it if the data dir is read-only).
func claudeRateLimitCachePath() string {
	if p := os.Getenv("AIEXPEDITE_CLAUDE_RL_CACHE"); p != "" {
		return p
	}
	return filepath.Join(GetConfigDir(), "claude_rate_limits.json")
}

// pickField returns the first value present under any of the given keys, plus
// whether one was found. Claude Code's stream-json emits the same fields in
// either snake_case or camelCase depending on surface and version (e.g. the
// rejected `rate_limit_event` upstream uses `rateLimitType` / `resetsAt`,
// while the status-line / SDK shape uses `rate_limit_type` / `resets_at`).
// Accepting both keeps parsing resilient across versions.
func pickField(m map[string]interface{}, keys ...string) (interface{}, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v, true
		}
	}
	return nil, false
}

// numAsFloat coerces a decoded JSON value (float64, json.Number, or numeric
// string) into a float64. Returns (0, false) for anything non-numeric.
func numAsFloat(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	}
	return 0, false
}

// normalizeResetMs converts a reset timestamp that may be expressed in seconds
// or milliseconds into milliseconds. Anything below 1e12 (≈ year 2001 in ms)
// is treated as seconds — current epoch seconds (~1.7e9) are far below that,
// while epoch ms (~1.7e12) are above it.
func normalizeResetMs(raw float64) int64 {
	if raw <= 0 {
		return 0
	}
	if raw < 1e12 {
		return int64(raw * 1000)
	}
	return int64(raw)
}

// bucketFromInfo builds a normalised bucket from a single rate-limit info
// object. Accepts both the SDK RateLimitInfo shape (utilization 0..1) and the
// status-line shape (used_percentage 0..100).
func bucketFromInfo(info map[string]interface{}, nowMs int64) (claudeRateLimitBucket, bool) {
	b := claudeRateLimitBucket{ObservedAtMs: nowMs}
	if s, ok := info["status"].(string); ok {
		b.Status = s
	}
	if v, ok := pickField(info, "resets_at", "resetsAt"); ok {
		if f, ok := numAsFloat(v); ok {
			b.ResetsAtMs = normalizeResetMs(f)
		}
	}
	// Prefer an explicit 0..100 percentage; fall back to 0..1 utilization.
	usageObserved := false
	if v, ok := pickField(info, "used_percentage", "usedPercentage"); ok {
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
	// A rejected (hard-limit) bucket may omit usedPercentage / utilization —
	// upstream sometimes emits only status + resetsAt + rateLimitType. Without
	// this, the cache would render 0% consumed / 100% remaining alongside a
	// future reset, contradicting the auto-defer session.go just emitted. Treat
	// the bucket as exhausted in that case so the UI matches reality.
	if !usageObserved && b.Status == claudeRateLimitStatusRejected {
		b.UsedPercentage = 100
		usageObserved = true
	}
	b.usageKnown = usageObserved
	// A bucket with neither a reset time nor a status nor a usage signal is
	// noise — refuse it so we never overwrite a good cache entry with nothing.
	if b.ResetsAtMs == 0 && b.Status == "" && !usageObserved {
		return b, false
	}
	return b, true
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// extractClaudeRateLimitBuckets pulls every window it can find from a decoded
// Claude stream-json event. Handles three shapes:
//   - rate_limit_event with a nested `rate_limit_info` (per-window)
//   - a flat event carrying `rate_limit_type` at top level (per-window)
//   - a `rate_limits` map of window -> { used_percentage, resets_at }
//     (status-line shape; some result events embed it too)
func extractClaudeRateLimitBuckets(raw map[string]interface{}, nowMs int64) map[string]claudeRateLimitBucket {
	out := map[string]claudeRateLimitBucket{}

	// Shape 3: a map of windows.
	if v, ok := pickField(raw, "rate_limits", "rateLimits"); ok {
		if rl, ok := v.(map[string]interface{}); ok {
			for window, v := range rl {
				if info, ok := v.(map[string]interface{}); ok {
					if b, ok := bucketFromInfo(info, nowMs); ok {
						out[window] = b
					}
				}
			}
		}
	}

	// Shapes 1 & 2: a single window carried by rate_limit_info / top level.
	info := raw
	if v, ok := pickField(raw, "rate_limit_info", "rateLimitInfo"); ok {
		if nested, ok := v.(map[string]interface{}); ok {
			info = nested
		}
	}
	window := ""
	if v, ok := pickField(info, "rate_limit_type", "rateLimitType"); ok {
		window, _ = v.(string)
	}
	if window != "" {
		if b, ok := bucketFromInfo(info, nowMs); ok {
			out[window] = b
		}
	}

	return out
}

// captureClaudeRateLimitLine parses one stdout line from a Claude session and,
// if it carries rate-limit telemetry, merges it into the on-disk cache.
// Returns the bucket that is hard-rejected (limit hit), if any, so the caller
// can surface it to the orchestrator. Best-effort: every failure is silent
// (this runs in the hot streaming path and must never break a session).
func captureClaudeRateLimitLine(line string, now time.Time) *claudeRateLimitBucket {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return nil
	}
	// Cheap prefilter: only attempt the JSON decode when the line could plausibly
	// carry rate-limit telemetry, in either snake_case or camelCase shape.
	if !strings.Contains(trimmed, "rate_limit") && !strings.Contains(trimmed, "rateLimit") {
		return nil
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return nil
	}
	nowMs := now.UnixMilli()
	updates := extractClaudeRateLimitBuckets(raw, nowMs)
	if len(updates) > 0 {
		mergeClaudeRateLimitCache(claudeRateLimitCachePath(), updates, now, currentClaudeAccountFingerprint())
	}

	// Surface the rejected window with the LATEST reset time. When multiple
	// buckets are rejected in the same event (e.g. five_hour and seven_day_opus),
	// the orchestrator must wait until every rejected window has reset before
	// resuming — picking the soonest reset would wake it while another window is
	// still blocked, causing an immediate re-rejection from Claude.
	var rejected *claudeRateLimitBucket
	consider := func(b claudeRateLimitBucket) {
		if b.Status != claudeRateLimitStatusRejected || b.ResetsAtMs <= 0 {
			return
		}
		if rejected == nil || b.ResetsAtMs > rejected.ResetsAtMs {
			bb := b
			rejected = &bb
		}
	}
	for _, b := range updates {
		consider(b)
	}
	// A rejected event may omit the window id — Claude Code's SDKRateLimitEvent
	// carries status/resetsAt/utilization with no rate_limit_type. It can't be
	// keyed into the per-window cache, but a hard limit MUST still drive the
	// auto-defer, so consider it here even though it never reached `updates`.
	if wl, ok := windowlessRejectedBucket(raw, nowMs); ok {
		consider(wl)
	}
	return rejected
}

// windowlessRejectedBucket extracts a rejected rate-limit bucket from an event
// that omits the window id (rate_limit_type / rateLimitType). Returns
// (zero, false) for anything else — a windowed event (already captured via the
// per-window path), a non-rejected status, or a missing reset time. Used only
// to drive auto-defer; such an event is intentionally NOT cached because there
// is no window to attribute it to on the CLI Agents tab.
func windowlessRejectedBucket(raw map[string]interface{}, nowMs int64) (claudeRateLimitBucket, bool) {
	info := raw
	if v, ok := pickField(raw, "rate_limit_info", "rateLimitInfo"); ok {
		if nested, ok := v.(map[string]interface{}); ok {
			info = nested
		}
	}
	if _, ok := pickField(info, "rate_limit_type", "rateLimitType"); ok {
		return claudeRateLimitBucket{}, false // windowed path already handled it
	}
	b, ok := bucketFromInfo(info, nowMs)
	if !ok || b.Status != claudeRateLimitStatusRejected || b.ResetsAtMs <= 0 {
		return claudeRateLimitBucket{}, false
	}
	return b, true
}

// mergeClaudeRateLimitCache read-modify-writes the cache, overwriting only the
// windows present in `updates` and leaving the rest intact. When the existing
// snapshot was captured under a different account fingerprint, its buckets are
// discarded — a previous account's reset times must not bleed into the new
// account's display.
func mergeClaudeRateLimitCache(path string, updates map[string]claudeRateLimitBucket, now time.Time, fingerprint string) {
	if path == "" || len(updates) == 0 {
		return
	}
	claudeRateLimitMu.Lock()
	defer claudeRateLimitMu.Unlock()

	snap := claudeRateLimitSnapshot{Buckets: map[string]claudeRateLimitBucket{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &snap)
		if snap.Buckets == nil {
			snap.Buckets = map[string]claudeRateLimitBucket{}
		}
	}
	// Scope to the current account: any fingerprint transition is an account
	// boundary, including unscoped<->scoped flips. Without dropping on those
	// transitions, buckets cached while creds were unreadable get stamped under
	// the next signed-in account's fingerprint and surface as that account's
	// reset windows on the CLI Agents tab.
	if snap.AccountFingerprint != fingerprint {
		snap.Buckets = map[string]claudeRateLimitBucket{}
	}
	nowMs := now.UnixMilli()
	for window, bucket := range updates {
		// An "allowed" heartbeat refreshes status / reset time but carries no
		// usage reading. Don't let its zero default clobber a previously
		// observed UsedPercentage — Claude Code emits these heartbeats after
		// every session, so a real percentage would otherwise decay to 0%.
		if !bucket.usageKnown {
			prev, ok := snap.Buckets[window]
			// Carry a prior reading forward ONLY when it still describes the
			// same LIVE window: the prior reset is in the future, and this
			// heartbeat either repeats that reset or omits it. If the prior
			// window already rolled over (its reset has passed) or this
			// heartbeat advertises a different/later reset, the percentage is
			// stale — skip persisting so the metric reflects the rolled-over
			// window (0% / Unknown) instead of replaying a high-water mark
			// under the new reset. Skipping also covers the no-prior case: a
			// first-ever heartbeat without usage must not seed a fake 0% row.
			sameLiveWindow := ok && prev.ResetsAtMs > nowMs &&
				(bucket.ResetsAtMs == 0 || bucket.ResetsAtMs == prev.ResetsAtMs)
			if !sameLiveWindow {
				continue
			}
			if bucket.ResetsAtMs == 0 {
				bucket.ResetsAtMs = prev.ResetsAtMs
			}
			bucket.UsedPercentage = prev.UsedPercentage
		}
		snap.Buckets[window] = bucket
	}
	snap.UpdatedAt = now.UTC().Format(time.RFC3339)
	snap.AccountFingerprint = fingerprint

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	out, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return
	}
	// Write-then-rename so a concurrent loadClaudeRateLimitSnapshot reader never
	// observes a half-written file (rename is atomic within a directory).
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

// loadClaudeRateLimitSnapshot reads the cache. Returns (zero, false) when the
// file is absent or unreadable — the normal "no telemetry observed yet" state.
func loadClaudeRateLimitSnapshot(path string) (claudeRateLimitSnapshot, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return claudeRateLimitSnapshot{}, false
	}
	var snap claudeRateLimitSnapshot
	if err := json.Unmarshal(b, &snap); err != nil || snap.Buckets == nil {
		return claudeRateLimitSnapshot{}, false
	}
	return snap, true
}

// formatClaudeLimitLine renders a rejected window as a one-line notice whose
// shape agent-orchestrator-service's detectRateLimit already recognises (the
// "iso-timestamp" matcher: a usage-limit cue + "resets at <ISO-8601>"). Emitting
// this as session output is what lets the orchestrator queue the execution and
// auto-resume at the exact reset instant.
func formatClaudeLimitLine(b claudeRateLimitBucket) string {
	if b.ResetsAtMs <= 0 {
		return "Claude usage limit reached."
	}
	iso := time.UnixMilli(b.ResetsAtMs).UTC().Format(time.RFC3339)
	return "Claude usage limit reached · resets at " + iso
}
