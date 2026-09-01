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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Claude rate-limit window identifiers, as emitted by Claude Code's
// rate_limit_event.rate_limit_type and the status-line rate_limits map keys.
//
// The per-model seven-day splits (Sonnet/Opus) aggregate into the single
// "Weekly quota" row, because they meter the SAME weekly allowance. The Fable
// window does not: Claude Code's own /usage panel reports it as a separate
// "Weekly Fable" meter, so cliagent_usage_claudecode.go reads it as its own row
// rather than folding it into that aggregate.
const (
	claudeWindowFiveHour       = "five_hour"
	claudeWindowSevenDay       = "seven_day"
	claudeWindowSevenDayOpus   = "seven_day_opus"
	claudeWindowSevenDaySonnet = "seven_day_sonnet"
	claudeWindowSevenDayFable  = "seven_day_fable"
	// claudeWindowSevenDayOverageIncluded is what Claude Code ACTUALLY calls the
	// weekly Fable window on the wire. Its own limit-label table (Claude Code
	// 2.1.237) reads:
	//
	//	five_hour: "session limit", seven_day: "weekly limit",
	//	seven_day_opus: "Opus limit", seven_day_sonnet: "Sonnet limit",
	//	seven_day_overage_included: "Fable 5 limit"
	//
	// so this key — not the extrapolated `seven_day_fable` — is the one a real
	// rate_limit_event carries for the meter /usage draws as "Weekly Fable".
	claudeWindowSevenDayOverageIncluded = "seven_day_overage_included"
)

// claudeRateLimitStatusRejected is the status value Claude Code sets once a
// window's limit is exhausted and further requests are blocked.
const claudeRateLimitStatusRejected = "rejected"

// Provenance values for claudeRateLimitBucket.Source — which writer produced a
// bucket's reading. Recorded so an operator reading the cache (or a test) can
// tell an interactive status-line render from a stream capture from the
// bounded usage probe, all three of which write the same windows.
//
// Diagnostic only: no read path branches on it, and it is deliberately absent
// from the signed refresh receipt (canonicalCLIUsageMetric carries no such
// field), so adding it cannot change what the backend sees.
const (
	claudeRateLimitSourceStream     = "stream"
	claudeRateLimitSourceStatusLine = "statusline"
	claudeRateLimitSourceProbe      = "probe"
)

// claudeRateLimitBucket is one window's observed state. UsedPercentage is
// normalised to 0..100 regardless of source shape (utilization 0..1 vs
// used_percentage 0..100). ResetsAtMs is unix epoch milliseconds (0 = unknown).
type claudeRateLimitBucket struct {
	UsedPercentage float64 `json:"usedPercentage"`
	ResetsAtMs     int64   `json:"resetsAtMs"`
	Status         string  `json:"status,omitempty"`
	ObservedAtMs   int64   `json:"observedAtMs"`
	// UsageObserved records whether UsedPercentage is a real reading.
	//
	// PERSISTED, unlike usageKnown below, because a bucket can now be written
	// from a heartbeat that named a window's reset time while carrying no
	// utilization — the only way to record that a NEW window exists without
	// inventing a percentage for it. Readers must render such a bucket as
	// "unobservable", never as 0%.
	//
	// A nil pointer means "observed": every bucket written before this field
	// existed carried a reading, so an older cache keeps its meaning.
	UsageObserved *bool `json:"usageObserved,omitempty"`
	// usageKnown is true when this bucket carries a fresh usage reading
	// (utilization / used_percentage) or a synthesized 100% from a rejected
	// status. False marks an "allowed heartbeat" that updated only the reset
	// time / status — merge must NOT let its 0 default overwrite a previously
	// observed UsedPercentage. Per-event and not persisted; UsageObserved is
	// the durable form of the same fact.
	usageKnown bool `json:"-"`
	// Source names the writer that produced this bucket (see the
	// claudeRateLimitSource* constants). Omitted when empty so a cache written
	// before this field existed round-trips byte-identically.
	Source string `json:"source,omitempty"`
}

// hasObservedUsage reports whether UsedPercentage came from an actual reading.
// Absent (nil) is "observed" so caches written before UsageObserved existed —
// where every persisted bucket did carry a reading — keep their meaning.
func (b claudeRateLimitBucket) hasObservedUsage() bool {
	return b.UsageObserved == nil || *b.UsageObserved
}

func usageObservedPtr(v bool) *bool { return &v }

// claudeRateLimitSnapshot is the on-disk cache, keyed by window id.
// AccountFingerprint pins the snapshot to the Claude account that produced it
// — when the local creds change, a stale weekly/future-reset bucket must NOT
// be attributed to the new account (the CLI Agents tab would otherwise show
// another user's capacity until the new account emits its own telemetry).
type claudeRateLimitSnapshot struct {
	UpdatedAt          string                           `json:"updatedAt"`
	AccountFingerprint string                           `json:"accountFingerprint,omitempty"`
	Buckets            map[string]claudeRateLimitBucket `json:"buckets"`
	// LastProbeObservedAtMs is the newest observation instant the utilization
	// PROBE has persisted a reading at, for this account. Monotonic within an
	// account and cleared with the buckets on a fingerprint change.
	//
	// Recorded at the SNAPSHOT level, deliberately, rather than being derived
	// from the surviving `source: "probe"` buckets. Per-window provenance says
	// who wrote a window LAST, so a single interactive status-line render
	// overwrites five_hour/seven_day with `source: "statusline"` and erases every
	// trace that a probe ever ran — taking with it the evidence that the endpoint
	// does not supply some other row (an unsupported Fable window, say). That row
	// would then read as stale again on the next gather, and the routine probe
	// would pay for the same answer forever. The fact being recorded is "a probe
	// sampled the account at T", which belongs to the snapshot, not to whichever
	// window happens to still carry the probe's byline.
	//
	// Omitted when zero so a cache written before this field existed round-trips
	// byte-identically; readers fall back to the per-bucket provenance for those.
	LastProbeObservedAtMs int64 `json:"lastProbeObservedAtMs,omitempty"`
}

// claudeRateLimitGate serialises the read-modify-write of the cache file
// in-process. Cross-process serialization is handled separately by an advisory
// file lock — Claude Code can render the status line from multiple windows in
// parallel, each spawning its own `aiexpedite statusline-hook` process, and a
// process-local mutex doesn't help across those boundaries.
//
// A one-slot channel rather than a sync.Mutex because the VERIFIED merge has to
// bound its own wait, and a mutex offers no deadline. The queue behind this gate
// has no depth limit: every rate-limit line a Claude session prints merges here,
// and each writer may spend the full cross-process lock wait while holding it.
// So "one queued writer" is not a bound — with several concurrent sessions the
// probe's persist step would drift arbitrarily far past the deadline its caller
// (and the joiner waiting on it) was promised. See
// claudeRateLimitVerifiedPersistBudget.
var claudeRateLimitGate = make(chan struct{}, 1)

// lockClaudeRateLimitCache takes the in-process gate, waiting as long as it
// takes.
//
// NOTHING in the merge path uses this any more — every writer, best-effort
// included, now bounds its wait (see claudeRateLimitBestEffortGateWait). It
// remains as the tests' way to pin the gate held, which is the only place an
// unbounded acquisition is safe.
func lockClaudeRateLimitCache() { claudeRateLimitGate <- struct{}{} }

// lockClaudeRateLimitCacheUntil takes the gate with a deadline, reporting
// whether it was acquired. Nothing is held on a false return.
func lockClaudeRateLimitCacheUntil(deadline time.Time) bool {
	select {
	case claudeRateLimitGate <- struct{}{}:
		return true
	default:
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case claudeRateLimitGate <- struct{}{}:
		return true
	case <-timer.C:
		return false
	}
}

func unlockClaudeRateLimitCache() { <-claudeRateLimitGate }

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
		mergeClaudeRateLimitCacheFromSource(claudeRateLimitCachePath(), updates, now,
			currentClaudeAccountFingerprint(), claudeRateLimitSourceStream)
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
//
// Concurrent Claude Code windows each spawn their own `aiexpedite
// statusline-hook` process, so an in-process mutex doesn't serialize this RMW.
// We take an advisory file lock on a sibling `.lock` file across the read,
// merge, and rename — without it, two writers can both read the same old
// snapshot, then race their tmp writes and renames, dropping one window's
// fresh five-hour/seven-day update. The tmp file is also given a per-process
// unique suffix so even if the lock is unavailable (some odd filesystem) two
// writers can't clobber each other's intermediate state.
func mergeClaudeRateLimitCache(path string, updates map[string]claudeRateLimitBucket, now time.Time, fingerprint string) {
	mergeClaudeRateLimitCacheFromSource(path, updates, now, fingerprint, "")
}

// mergeClaudeRateLimitCacheFromSource is mergeClaudeRateLimitCache with the
// writer's provenance stamped onto every bucket it persists (see the
// claudeRateLimitSource* constants). A carried-forward heartbeat keeps the
// source of the reading it carries — the heartbeat observed the reset, not the
// percentage, so claiming its own provenance for a percentage it did not
// measure would be wrong.
func mergeClaudeRateLimitCacheFromSource(path string, updates map[string]claudeRateLimitBucket, now time.Time, fingerprint, source string) {
	_, _ = mergeClaudeRateLimitCacheInto(context.Background(), path, updates, now, fingerprint, source, false)
}

// mergeClaudeRateLimitCacheChecked is the same merge, but REPORTS whether the
// snapshot actually reached disk, and WHICH observation the cache ends up
// holding for the windows it was asked to write.
//
// The fire-and-forget wrappers above are right for the stream and status-line
// writers: they run in hot paths, fire constantly, and a dropped write is
// corrected by the next event moments later. The utilization probe is not like
// that. It runs at most once per interval, and its caller's return value feeds a
// SIGNED refresh receipt — so silently absorbing an unwritable cache dir, a
// transient Windows sharing violation, or a failed rename would let the probe
// report a fresh observation it never persisted, clear its failure backoff, and
// throttle the retry that would have fixed it.
//
// The returned instant is the OLDEST observed reading standing in the merged
// snapshot across the update's own windows — this writer's stamp where it
// landed, and the incumbent's where the newer-wins guard below kept it instead.
// "The write succeeded" is not the same question as "the cache now holds a
// reading at least as new as the run that earned this write": a caller settling
// a post-run debt needs the second one, and every window of an update can
// legitimately be refused as older while the merge still returns success.
//
// The oldest rather than the newest, because a debt is a claim about EVERY row
// this write covers, and a maximum settles it on the strength of one. A
// status-line render landing while the probe is in flight legitimately keeps its
// newer five_hour reading, so the merge returns success with five_hour at a
// post-run instant and seven_day (plus any per-model weekly window) still
// carrying the probe's pre-request stamp. Reporting the newest would clear the
// debt on the one window that did not need it and leave the others displaying a
// pre-run observation for the whole staleness TTL — the exact staleness the
// probe exists to remove. Reporting the constraining window makes the answer
// true of every row the write touched, and costs at most one extra probe.
//
// A window the merged snapshot holds no observed reading for — refused as a
// heartbeat, or never persisted at all — collapses the result to zero. There is
// no instant at which that row was measured, so no debt can be paid with it.
//
// It is also the only merge that is BOUNDED end to end — by its own persist
// budget AND by the caller's ctx, so it cannot outlive the gather that asked for
// it — and that treats an unserialized write as a failure. See
// mergeClaudeRateLimitCacheInto.
func mergeClaudeRateLimitCacheChecked(ctx context.Context, path string, updates map[string]claudeRateLimitBucket, now time.Time, fingerprint, source string) (time.Time, error) {
	return mergeClaudeRateLimitCacheInto(ctx, path, updates, now, fingerprint, source, true)
}

// mergeClaudeRateLimitCacheInto is the shared implementation. `verified` selects
// between the two contracts the callers need, and they differ in exactly three
// places:
//
//   - The wait. Both are bounded, but by different clocks and for different
//     reasons. A verified writer gives up after
//     claudeRateLimitVerifiedPersistBudget, which covers the gate, the
//     cross-process lock AND the filesystem work those two protect, so its total
//     cost is a constant its caller's join deadline can be derived from rather
//     than a function of how many other writers happen to be queued — or of how
//     long the mount holding the cache takes to answer. A best-effort writer is
//     under no deadline of its own, but runs inside a Claude stdout scanner, so
//     it waits claudeRateLimitBestEffortGateWait for the gate and then DROPS the
//     merge rather than stall the scanner behind an abandoned, unkillable write.
//   - What a refusal COSTS. Both modes refuse a CONFIRMED holder (see below);
//     they differ in what the caller is told. A best-effort writer drops the
//     merge silently — the next rate-limit line rewrites the same windows
//     moments later. A verified one returns the failure, so the probe backs off,
//     retries, and signs nothing in the meantime.
//
// Confirmed contention refuses in BOTH modes, and that is a correctness rule,
// not a politeness one. An unlocked read-modify-rename behind a holder is not
// "one lost window update": the best-effort writer reads the snapshot the holder
// has not replaced yet, and renames that stale copy over the holder's committed
// result — deleting probe-only windows and LastProbeObservedAtMs AFTER the probe
// reported success, settled the post-run debt, and signed a receipt for them.
// The status-line hook runs in its own process on exactly this path, so the
// window is real whenever a verified merge outlives claudeRateLimitCacheLockWait.
// A dropped best-effort merge costs one reading; a resurrected stale snapshot
// costs the feature.
//
// A lock we could not even OPEN is a different fact and keeps the degraded path
// in both modes: there is no evidence of a competing holder, only of a
// filesystem that will not give us the lock file (a read-only data dir fails the
// write below anyway, which the verified caller does see).
func mergeClaudeRateLimitCacheInto(ctx context.Context, path string, updates map[string]claudeRateLimitBucket, now time.Time, fingerprint, source string, verified bool) (time.Time, error) {
	if path == "" || len(updates) == 0 {
		return time.Time{}, fmt.Errorf("claude rate-limit cache: nothing to merge")
	}
	if !verified {
		return mergeClaudeRateLimitCacheSerialized(path, updates, now, fingerprint, source, time.Time{})
	}
	// A verified merge is bounded END TO END, not merely across its two lock
	// waits. Everything past them — MkdirAll, ReadFile, WriteFile, Rename — is a
	// filesystem syscall, and Go offers no way to cancel one: on a wedged mount
	// (an unreachable SMB share, a hung network drive, a disk that stopped
	// answering) they block for as long as the kernel takes, observing neither
	// context nor deadline. Waiting on them in the CALLER's goroutine would let a
	// single stalled write outlive the probe timeout, the gather deadline, and
	// the join deadline derived from this very budget — pinning the probe's
	// single-flight slot and the signed refresh handler behind a filesystem that
	// may never answer, which is the wedge the bounded lock waits were added to
	// prevent, one step further down.
	//
	// So the serialized merge runs on its own goroutine and the caller waits for
	// it only until the budget expires — or until ctx ends, whichever comes
	// first, so this write can never push the gather past its own deadline. Abandoning it is safe in both directions:
	// the result channel is buffered, so a late answer is dropped rather than
	// parking that goroutine forever; the goroutine keeps BOTH locks until its
	// syscall returns, so no other writer can interleave a read-modify-rename
	// with the one still in flight; and the caller reports a persist FAILURE, so
	// the probe backs off and signs nothing. A write that lands after we gave up
	// on it is just a reading the next gather picks up — never a receipt claiming
	// an observation the cache did not hold.
	//
	// What abandoning it does NOT do is release the gate, and that is precisely
	// why every other writer bounds its own wait for it. A stalled holder we
	// cannot interrupt would otherwise be free to stop the Claude stdout scanners
	// that merge synchronously on this path — see
	// claudeRateLimitBestEffortGateWait.
	deadline := time.Now().Add(claudeRateLimitVerifiedPersistBudget)
	type persistResult struct {
		observed time.Time
		err      error
	}
	done := make(chan persistResult, 1)
	go func() {
		observed, err := mergeClaudeRateLimitCacheSerialized(path, updates, now, fingerprint, source, deadline)
		done <- persistResult{observed: observed, err: err}
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case res := <-done:
		return res.observed, res.err
	case <-ctx.Done():
		// The caller's own deadline is the tighter one. A verified merge is
		// reached at the END of a probe that already spent its request timeout
		// (and possibly a join wait) inside the SHARED gather budget, so a
		// persist budget measured from time.Now() can outlive the gather that
		// asked for it — delaying every provider queued behind Claude and the
		// refresh result itself. Stop waiting here; the goroutine keeps its full
		// budget and its locks, so the reading still lands for the next gather.
		return time.Time{}, fmt.Errorf("claude rate-limit cache: persist abandoned before the %s budget: %w",
			claudeRateLimitVerifiedPersistBudget, ctx.Err())
	case <-timer.C:
		return time.Time{}, fmt.Errorf("claude rate-limit cache: persist exceeded the %s budget",
			claudeRateLimitVerifiedPersistBudget)
	}
}

// mergeClaudeRateLimitCacheSerialized runs the read-merge-rename under the
// in-process gate and the cross-process file lock. A zero budgetDeadline selects
// the best-effort contract (wait claudeRateLimitBestEffortGateWait for the gate,
// then drop the merge); a non-zero one selects the verified contract, clamping
// both waits to what is left of the caller's budget and reporting the failure
// rather than dropping it. Neither contract writes behind a confirmed holder.
func mergeClaudeRateLimitCacheSerialized(path string, updates map[string]claudeRateLimitBucket, now time.Time, fingerprint, source string, budgetDeadline time.Time) (time.Time, error) {
	verified := !budgetDeadline.IsZero()
	if verified {
		if !lockClaudeRateLimitCacheUntil(budgetDeadline) {
			return time.Time{}, fmt.Errorf("claude rate-limit cache: busy")
		}
	} else if !lockClaudeRateLimitCacheUntil(time.Now().Add(claudeRateLimitBestEffortGateWait)) {
		return time.Time{}, fmt.Errorf("claude rate-limit cache: gate held past %s, merge dropped",
			claudeRateLimitBestEffortGateWait)
	}
	defer unlockClaudeRateLimitCache()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return time.Time{}, err
	}
	// Cross-process exclusive lock. Best-effort: if the lock file can't be
	// created (read-only data dir) we still proceed — the in-process mutex
	// keeps THIS process consistent, and a single-Claude-window install never
	// hits the cross-process race anyway.
	//
	// BOUNDED rather than blocking, because this write is now reachable from the
	// utilization probe, whose caller runs under a deadline (the 3s probe timeout
	// nested in the gather's own). A blocking flock observes neither, so one
	// wedged `statusline-hook` process — a stalled cache filesystem is enough —
	// would hang the signed refresh handler and pin the probe's single-flight
	// latch indefinitely. The wait also bounds the in-process gate, which is held
	// only here: without it a probe could queue behind an in-process writer that
	// is itself blocked on the file lock, wedging one level removed.
	//
	// A verified merge additionally clamps the wait to whatever is left of its
	// own budget, so the gate wait and the lock wait cannot sum past it.
	lockDeadline := time.Now().Add(claudeRateLimitCacheLockWait)
	if verified && budgetDeadline.Before(lockDeadline) {
		lockDeadline = budgetDeadline
	}
	lockFile, lockOutcome := acquireClaudeRateLimitCacheLock(path, lockDeadline)
	switch lockOutcome {
	case claudeRateLimitLockAcquired:
		defer func() {
			_ = unlockFile(lockFile)
			_ = lockFile.Close()
		}()
	case claudeRateLimitLockContended:
		// Confirmed contention refuses in BOTH modes. See the contract note on
		// mergeClaudeRateLimitCacheInto: an unlocked read-modify-rename here is
		// not "one lost window update", it is a stale snapshot renamed over a
		// verified writer's committed result.
		return time.Time{}, fmt.Errorf("claude rate-limit cache: held by another writer")
	}

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
		// Probe evidence is an observation about ONE account's quota, so it
		// crosses an account boundary no more than a bucket does.
		snap.LastProbeObservedAtMs = 0
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
			if sameLiveWindow {
				if bucket.ResetsAtMs == 0 {
					bucket.ResetsAtMs = prev.ResetsAtMs
				}
				bucket.UsedPercentage = prev.UsedPercentage
				// The heartbeat observed the reset/status, not utilization. Keep
				// the carried percentage paired with its original observation so
				// repeated heartbeats cannot make stale usage appear fresh
				// indefinitely.
				bucket.ObservedAtMs = prev.ObservedAtMs
				bucket.UsageObserved = usageObservedPtr(prev.hasObservedUsage())
				bucket.Source = prev.Source
				snap.Buckets[window] = bucket
				continue
			}
			// Not the same live window. The percentage is unknown here — but the
			// heartbeat still OBSERVED this window's reset time and status, and
			// dropping the whole update (which is what this code used to do)
			// leaves a rolled-over bucket in the cache that nothing can ever
			// replace: the next heartbeat is compared against the same expired
			// prior and discarded too. A Mac sat like that for days, its card
			// reading "Usage unobservable" under a reset date that had passed,
			// while fresh heartbeats arrived on every status-line render.
			//
			// So record the window WITHOUT a usage reading. UsageObserved=false
			// is persisted, so a reload cannot mistake the zero UsedPercentage
			// for an observation, and the readers render the row from ResetsAtMs
			// as "unobservable, resets <date>". The next usage-bearing event then
			// lands on a CURRENT window instead of being weighed against an
			// expired one.
			if bucket.ResetsAtMs == 0 {
				// No reset and no usage: nothing the cache does not already hold,
				// and seeding a bucket here would invent a 0% row.
				continue
			}
			bucket.UsedPercentage = 0
			bucket.UsageObserved = usageObservedPtr(false)
			bucket.Source = source
			snap.Buckets[window] = bucket
			continue
		}
		// A reading may only ever REPLACE an older one. `now` is captured by the
		// caller before its work, so a writer that took time to produce its
		// answer can arrive holding a timestamp that another writer has already
		// passed: the utilization probe stamps the instant the gather started and
		// then spends up to 3s in flight, while the status-line hook writes the
		// moment Claude renders. Overwriting there would move latestObservedAt
		// BACKWARDS and swap a newer percentage for an older one — the exact
		// staleness this feature exists to remove, reintroduced by a race.
		//
		// Strictly-newer, so a same-instant rewrite still lands: equal timestamps
		// carry no ordering, and refusing them would make a re-merge of the same
		// observation depend on which writer got there first.
		if prev, ok := snap.Buckets[window]; ok &&
			prev.hasObservedUsage() && prev.ObservedAtMs > bucket.ObservedAtMs {
			continue
		}
		bucket.UsageObserved = usageObservedPtr(true)
		bucket.Source = source
		snap.Buckets[window] = bucket
	}
	// Record that the probe sampled this account, independently of which writer
	// ends up owning each window (see LastProbeObservedAtMs). Only readings
	// count: a probe response we could not plot is not an observation. Monotonic
	// — a probe stamping an older instant than one already on record cannot walk
	// the evidence backwards.
	if source == claudeRateLimitSourceProbe {
		for _, bucket := range updates {
			if bucket.usageKnown && bucket.ObservedAtMs > snap.LastProbeObservedAtMs {
				snap.LastProbeObservedAtMs = bucket.ObservedAtMs
			}
		}
	}
	snap.UpdatedAt = now.UTC().Format(time.RFC3339)
	snap.AccountFingerprint = fingerprint

	// What the cache HOLDS for these windows once the merge has run — which is
	// this writer's stamp only where it actually won. Read from snap.Buckets
	// rather than from `updates` so a window the newer-wins guard refused
	// reports the incumbent observation that beat it, not the one we tried to
	// write; and only from observed readings, so a heartbeat row recorded
	// without usage cannot pass for one.
	//
	// Aggregated as a MINIMUM: the caller asks whether the cache now holds a
	// recent enough reading for every window this write covers, and one window
	// left behind is the whole answer. See mergeClaudeRateLimitCacheChecked.
	observed := time.Time{}
	for window := range updates {
		b, ok := snap.Buckets[window]
		if !ok || !b.hasObservedUsage() {
			// A window carrying no reading at all cannot cover anything, and it
			// cannot be averaged away by the windows that do.
			observed = time.Time{}
			break
		}
		if observed.IsZero() || b.ObservedAtMs < observed.UnixMilli() {
			observed = time.UnixMilli(b.ObservedAtMs)
		}
	}

	out, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return time.Time{}, err
	}
	// Write-then-rename so a concurrent loadClaudeRateLimitSnapshot reader never
	// observes a half-written file. The PID + nanosecond suffix keeps two
	// concurrent writers (or a stale tmp from a crashed prior run) from
	// colliding on the intermediate file even outside the lock.
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), now.UnixNano())
	if err := claudeRateLimitCacheWriteFile(tmp, out, 0o600); err != nil {
		return time.Time{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return time.Time{}, err
	}
	return observed, nil
}

// acquireCrossProcessCacheLock opens (and exclusively locks) a sibling file of
// the cache path so concurrent `aiexpedite statusline-hook` processes serialize
// the read-modify-write. Returns the lock file and whether the lock was
// acquired — callers MUST unlock + close when true, and skip when false.
func acquireCrossProcessCacheLock(cachePath string) (*os.File, bool) {
	lockPath := cachePath + ".lock"
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false
	}
	if err := lockFileExclusive(f); err != nil {
		_ = f.Close()
		return nil, false
	}
	return f, true
}

// claudeRateLimitCacheLockWait bounds how long a Claude cache writer waits for
// the cross-process lock before proceeding without it, and
// claudeRateLimitCacheLockPoll is the retry cadence inside that wait. Vars so
// the contention test can pin them small — the suite runs under `go test -race`
// and must not spend seconds of wall clock asserting one fallback.
//
// Two seconds is generous for a read-merge-rename of a few KB of JSON: an
// ordinary contending writer is gone in milliseconds, so the wait only expires
// when the holder is genuinely wedged — which is precisely when giving up is
// the right move.
var (
	claudeRateLimitCacheLockWait = 2 * time.Second
	claudeRateLimitCacheLockPoll = 10 * time.Millisecond
)

// claudeRateLimitVerifiedPersistBudget is the ENFORCED ceiling on a verified
// merge, end to end: the wait for the in-process gate, the wait for the
// cross-process file lock, AND the filesystem work those two protect. It is a
// ceiling the merge itself observes, not an estimate of how long the merge is
// likely to take — the queue on the gate is unbounded, so anything derived from
// "our wait plus one other writer's" would understate it the moment two Claude
// sessions print rate-limit lines at once, and a syscall on a wedged mount has
// no upper bound to estimate at all.
//
// Two lock waits plus a margin: enough for one holder to be waited out and the
// gate to be handed over, without letting a caller's join deadline (which is
// derived from this) grow into something a user waiting on Refresh would notice.
// The margin also covers the read-merge-rename itself, which on a healthy
// filesystem is a few KB of JSON — microseconds, not milliseconds.
var claudeRateLimitVerifiedPersistBudget = 2*claudeRateLimitCacheLockWait + 500*time.Millisecond

// claudeRateLimitBestEffortGateWait bounds how long a fire-and-forget writer
// (stream capture, status-line hook) waits for the in-process gate before
// DROPPING its merge.
//
// Waiting forever there was a session-level hazard, not merely an untidy one.
// Go cannot cancel a filesystem syscall, so a verified merge whose write stalls
// on a wedged mount is abandoned by its caller while its goroutine keeps the
// gate until the kernel answers — possibly never. captureClaudeRateLimitLine
// merges SYNCHRONOUSLY inside both Claude stdout scanners (claude_native.go's
// readStream and session.go's readOutputStream), so an unbounded wait there
// stops output publication, fills the child's pipe and hangs sessions that have
// nothing to do with the cache. Bounding the waiters is the only lever we have:
// the stalled holder itself cannot be interrupted.
//
// Dropping is the right trade for these writers and only for them. They fire on
// every rate-limit line, the next event rewrites the same windows moments later,
// and the rejection captureClaudeRateLimitLine returns is derived from the
// parsed event rather than from the cache — so a dropped merge costs one
// reading, never a missed rate-limit rejection. The verified merge keeps its own
// stricter contract and reports the failure instead.
//
// The bound is the verified merge's whole persist budget: that is the longest a
// HEALTHY holder can legitimately keep the gate, so a wait that long only ever
// expires behind one that is genuinely wedged.
var claudeRateLimitBestEffortGateWait = claudeRateLimitVerifiedPersistBudget

// claudeRateLimitCacheWriteFile is os.WriteFile behind a seam, so a test can
// stall the snapshot write the way an unreachable network mount does. That is
// the failure mode the budget above exists to survive, and the one no portable
// filesystem can be made to reproduce on demand.
var claudeRateLimitCacheWriteFile = os.WriteFile

// claudeRateLimitLockOutcome distinguishes the two ways the cross-process lock
// can end up not held, because they are not the same fact. "Another writer has
// it" says a competing read-modify-rename is in flight and ours may be undone by
// it; "we could not open the lock file at all" says only that this filesystem
// will not give us one, with no evidence of a competitor. Every merge refuses
// the first and tolerates the second.
type claudeRateLimitLockOutcome int

const (
	claudeRateLimitLockAcquired claudeRateLimitLockOutcome = iota
	claudeRateLimitLockUnavailable
	claudeRateLimitLockContended
)

// acquireClaudeRateLimitCacheLock takes the sibling `.lock` file, waiting until
// `deadline` for a contending holder to release it.
//
// Only claudeRateLimitLockUnavailable takes the degraded unlocked path, and only
// because it carries no evidence of a competitor. claudeRateLimitLockContended
// does, so every caller refuses it: proceeding would rename a snapshot read
// while the holder was mid-write over whatever the holder went on to commit.
// Callers MUST unlock + close on claudeRateLimitLockAcquired, and skip otherwise.
func acquireClaudeRateLimitCacheLock(cachePath string, deadline time.Time) (*os.File, claudeRateLimitLockOutcome) {
	lockPath := cachePath + ".lock"
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, claudeRateLimitLockUnavailable
	}
	for {
		locked, err := tryLockFileExclusive(f)
		if err != nil {
			_ = f.Close()
			return nil, claudeRateLimitLockUnavailable
		}
		if locked {
			return f, claudeRateLimitLockAcquired
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, claudeRateLimitLockContended
		}
		time.Sleep(claudeRateLimitCacheLockPoll)
	}
}

// tryAcquireCrossProcessCacheLock acquires the same sibling lock without
// waiting. Optional/background persistence uses this path so lock contention
// cannot consume the caller's remaining deadline; the unchanged cache cursor
// makes the evidence eligible for retry on the next refresh.
func tryAcquireCrossProcessCacheLock(cachePath string) (*os.File, bool) {
	lockPath := cachePath + ".lock"
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false
	}
	locked, err := tryLockFileExclusive(f)
	if err != nil || !locked {
		_ = f.Close()
		return nil, false
	}
	return f, true
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
