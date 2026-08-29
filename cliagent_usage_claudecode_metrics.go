// cliagent_usage_claudecode_metrics.go — turns the on-disk Claude rate-limit
// cache into the metric rows the CLI Agents tab renders.
//
// Split out of cliagent_usage_claudecode.go, which now keeps only the
// credential / auth-state concerns. Everything here moved VERBATIM apart from
// latestClaudeObservation, which is new: it is the single place "how old is the
// freshest reading we hold" is computed, shared by the usage parser's
// stale-gather trigger and the bounded utilization probe's gate
// (cliagent_usage_claudecode_probe.go).
//
// The writers that fill this cache live in cliagent_ratelimit.go (stream
// capture), statusline_hook.go (interactive renders) and the probe.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// utilizationMetricsUnknown preserves metric identity and provenance while
// removing numeric values that cannot be trusted for an unavailable login.
func utilizationMetricsUnknown(metrics []cliAgentUsageMetric) []cliAgentUsageMetric {
	out := make([]cliAgentUsageMetric, len(metrics))
	for i, metric := range metrics {
		out[i] = cliAgentUsageMetric{
			Kind: metric.Kind, Label: metric.Label, Unit: metric.Unit,
			ResetAt: metric.ResetAt, ObservedAt: metric.ObservedAt,
			Model: metric.Model, Unknown: true,
		}
	}
	return out
}

// installedClaudeRateLimitCachePath returns the cache path the CURRENTLY
// INSTALLED status-line hook writes to, or "" when Claude has no hook of ours.
//
// `ourStatusLineCommand` pins the path to the installing binary's
// `GetConfigDir()`, but Claude's settings.json is a single machine-wide file:
// with two channels installed (release + dev), whichever agent booted last owns
// the hook, and the other one keeps reading a cache nothing writes to any more.
// Its Claude card then freezes at whatever was observed before the handover —
// the reported observation ages by days while the device is plainly online.
func installedClaudeRateLimitCachePath(home string) string {
	base := claudeConfigDir(home)
	if base == "" {
		return ""
	}
	settings := map[string]json.RawMessage{}
	if !readJSONFile(filepath.Join(base, "settings.json"), &settings) {
		return ""
	}
	raw, ok := settings["statusLine"]
	if !ok {
		return ""
	}
	var sl claudeStatusLine
	if json.Unmarshal(raw, &sl) != nil || !isOurStatusLineCommand(sl.Command) {
		return ""
	}
	return extractInstalledPinnedPath(sl.Command, "RL_CACHE")
}

// loadMergedClaudeRateLimitBuckets reads every cache this machine could have
// captured into — our own config dir plus the one the installed hook pins — and
// returns the freshest observation per window.
//
// Merging rather than preferring one path: the agent's own stream capture writes
// to OUR path while the status-line hook writes to the PINNED one, so on a
// dual-channel box each file holds a genuine subset of the observations. Taking
// the higher ObservedAtMs per window is the same precedence the cache itself
// applies on merge, so this can only move a window forward in time.
//
// A snapshot whose `accountFingerprint` doesn't match the current account is
// skipped entirely — see claudeCodeMetricsFromCache for why that scoping is
// exact rather than best-effort.
func loadMergedClaudeRateLimitBuckets(currentFingerprint string) map[string]claudeRateLimitBucket {
	home, _ := os.UserHomeDir()
	paths := []string{claudeRateLimitCachePath()}
	if pinned := installedClaudeRateLimitCachePath(home); pinned != "" && pinned != paths[0] {
		paths = append(paths, pinned)
	}

	merged := map[string]claudeRateLimitBucket{}
	for _, path := range paths {
		snap, ok := loadClaudeRateLimitSnapshot(path)
		if !ok || snap.AccountFingerprint != currentFingerprint {
			continue
		}
		for window, bucket := range snap.Buckets {
			prev, seen := merged[window]
			if !seen || bucket.ObservedAtMs > prev.ObservedAtMs {
				merged[window] = bucket
			}
		}
	}
	return merged
}

// claudeCodeMetricsFromCache builds the metric rows from the rate-limit cache,
// falling back to the Unknown placeholders when a window hasn't been observed.
// Three rows are always shown so the card layout is stable: the 5-hour session
// window, the weekly window, and the weekly Fable window.
//
// The cache is trusted only when its `accountFingerprint` exactly matches the
// caller-supplied one. Two empty fingerprints match (no creds + unscoped
// snapshot — the test/legacy default), but a scoped snapshot is ignored when
// the current account cannot be identified: otherwise `gatherCLIAgentUsage`
// would attribute a previous user's reset windows to the device-scoped
// fallback entry after the local credentials were removed.
func claudeCodeMetricsFromCache(now time.Time, currentFingerprint string) []cliAgentUsageMetric {
	buckets := loadMergedClaudeRateLimitBuckets(currentFingerprint)

	session := observedMetricOrUnknown(
		buckets, []string{claudeWindowFiveHour}, limitKindSession, "5-hour session window", now)
	// Weekly is reported under seven_day; some plans split it per-model. When
	// both per-model buckets are present we aggregate CONSERVATIVELY so an
	// exhausted Opus quota isn't hidden behind a healthier Sonnet number.
	weekly := aggregateWeeklyMetric(buckets, now)
	// Fable is metered SEPARATELY from the weekly quota above — Claude Code's
	// own /usage panel shows it as its own meter — so it is deliberately NOT
	// folded into the aggregate. Always emitted: observedMetricOrUnknown returns
	// the Unknown placeholder when no Fable window has been observed, which the
	// card renders as the dashed "Usage unobservable" bar rather than a 0% bar
	// that would read as "plenty of Fable quota left".
	fable := observedMetricOrUnknown(
		buckets, claudeFableWindowIDs(buckets), limitKindWeekly, "Weekly Fable", now)

	return []cliAgentUsageMetric{session, weekly, fable}
}

// claudeFableWindowIDs returns the cache keys that may hold the weekly Fable
// window, in the order observedMetricOrUnknown should try them: the canonical
// `seven_day_fable` first, then Claude Code's actual wire key for the meter
// (`seven_day_overage_included` — see the constant), then any other observed
// key naming Fable, sorted.
//
// Note on freshness: Claude Code's status-line payload carries ONLY `five_hour`
// and `seven_day` (verified against 2.1.237), so unlike those two rows the Fable
// row can never be refreshed from the status-line hook. It fills only from a
// stream `rate_limit_event`, which Claude emits when a window crosses a warning
// threshold or is rejected — so a comfortably-unused Fable quota legitimately
// stays unobservable. That is a reporting gap upstream, not one we can close
// here; showing a 0% bar instead would be worse (see below).
//
// The tolerant tail exists because the canonical key is an extrapolation from
// the keys Claude Code is known to emit — a rename or a suffixed variant
// upstream would otherwise silently drop the row from every shipped agent.
// Order is load-bearing twice over: it is observedMetricOrUnknown's TIE-BREAK
// among equally-fresh live buckets (canonical-first, so a real `seven_day_fable`
// wins over a same-timestamp variant — plain sorting would not, `fable_weekly`
// sorts ahead of it), and sorting the tail makes that tie-break deterministic
// where Go map iteration is not. Precedence is only a tie-break: observedMetric-
// OrUnknown skips rolled-over candidates first and then prefers the freshest live
// observation, so neither a stale nor a rolled-over canonical bucket left behind
// by a rename can mask a live variant. Non-determinism would not just be untidy:
// terminal-service delta-skips writes by hashing the marshalled payload, so a row
// flipping between two fable-ish buckets across polls would churn a Firestore
// write plus a 7-day history document each time.
//
// five_hour* keys are excluded: Claude already splits the weekly window
// per-model, so a symmetric `five_hour_fable` is plausible, and surfacing a
// session-window bucket under a row labelled "Weekly Fable" would misreport it.
func claudeFableWindowIDs(buckets map[string]claudeRateLimitBucket) []string {
	variants := make([]string, 0, len(buckets))
	for id := range buckets {
		lower := strings.ToLower(id)
		if id == claudeWindowSevenDayFable ||
			id == claudeWindowSevenDayOverageIncluded ||
			!strings.Contains(lower, "fable") ||
			strings.HasPrefix(lower, claudeWindowFiveHour) {
			continue
		}
		variants = append(variants, id)
	}
	sort.Strings(variants)
	return append([]string{claudeWindowSevenDayFable, claudeWindowSevenDayOverageIncluded}, variants...)
}

// aggregateWeeklyMetric reports the worst observed seven-day window: the
// highest used percentage and the soonest reset across the unified `seven_day`
// bucket and any per-model split (`seven_day_sonnet`, `seven_day_opus`). This
// prevents a healthy Sonnet bucket from masking a depleted Opus bucket on
// plans that emit them separately.
func aggregateWeeklyMetric(buckets map[string]claudeRateLimitBucket, now time.Time) cliAgentUsageMetric {
	windowIDs := []string{claudeWindowSevenDay, claudeWindowSevenDaySonnet, claudeWindowSevenDayOpus}
	var (
		observed      bool
		worstUsed     float64
		worstReset    int64
		worstObserved int64
		worstCurrent  bool
	)
	// Live windows whose usage was never observed: they cannot contribute a
	// percentage, but their reset is current and is the right thing to show when
	// nothing else was observed.
	liveUnobservedReset := int64(0)
	for _, id := range windowIDs {
		b, ok := buckets[id]
		if !ok {
			continue
		}
		if !b.hasObservedUsage() {
			if b.ResetsAtMs > 0 && now.UnixMilli() < b.ResetsAtMs && b.ResetsAtMs > liveUnobservedReset {
				liveUnobservedReset = b.ResetsAtMs
			}
			continue
		}
		used := b.UsedPercentage
		liveReset := b.ResetsAtMs > 0 && now.UnixMilli() < b.ResetsAtMs
		current := b.ResetsAtMs <= 0 || liveReset
		if !current {
			used = 0 // this sub-window has already rolled over
		}
		used = clampPercent(used)
		// Pair the reset with the bucket that produced worstUsed so the UI
		// reports when the *constraining* quota clears, not when an unrelated
		// healthier bucket happens to reset first. On a tie (e.g. both Sonnet
		// and Opus rejected at 100%), both buckets are equally constraining
		// and the limit doesn't clear until BOTH have reset — track the later
		// reset so we don't tell operators they can retry while another tied
		// bucket is still exhausted.
		switch {
		case !observed || used > worstUsed:
			worstUsed = used
			worstObserved = b.ObservedAtMs
			worstCurrent = current
			if liveReset {
				worstReset = b.ResetsAtMs
			} else {
				worstReset = 0
			}
		case used == worstUsed:
			wasCurrent := worstCurrent
			if current && (!worstCurrent || b.ObservedAtMs > worstObserved) {
				worstObserved = b.ObservedAtMs
			}
			worstCurrent = worstCurrent || current
			switch {
			case !current:
				// An expired tied bucket no longer constrains the live aggregate.
			case !wasCurrent && liveReset:
				worstReset = b.ResetsAtMs
			case !wasCurrent:
				worstReset = 0
			case !liveReset:
				// New tied bucket has no known live reset, so we can't say
				// when the combined constraint clears — surface Unknown.
				worstReset = 0
			case worstReset > 0 && b.ResetsAtMs > worstReset:
				worstReset = b.ResetsAtMs
			}
		}
		observed = true
	}
	if !observed {
		metric := cliAgentUsageMetric{Kind: limitKindWeekly, Label: "Weekly quota", Unit: "%", Unknown: true}
		if liveUnobservedReset > 0 {
			metric.ResetAt = time.UnixMilli(liveUnobservedReset).UTC().Format(time.RFC3339)
		}
		return metric
	}
	observedAt := observedAtRFC3339(worstObserved)
	if !worstCurrent {
		return cliAgentUsageMetric{
			Kind: limitKindWeekly, Label: "Weekly quota", Unit: "%",
			ObservedAt: observedAt, Unknown: true,
		}
	}
	var resetAt string
	if worstReset > 0 {
		resetAt = time.UnixMilli(worstReset).UTC().Format(time.RFC3339)
	}
	return cliAgentUsageMetric{
		Kind: limitKindWeekly, Label: "Weekly quota", Unit: "%",
		Total: floatPtr(100), Consumed: floatPtr(worstUsed), Remaining: floatPtr(100 - worstUsed),
		ResetAt: resetAt, ObservedAt: observedAt,
	}
}

// observedMetricOrUnknown returns a real percentage metric for the first window
// id present in the cache that still describes a LIVE window, or an Unknown
// placeholder when none is observed. A window whose reset time has passed is
// unobservable: assuming 0% ignores usage that may already have happened on
// another computer.
//
// Liveness is checked BEFORE candidate precedence, which matters only for a
// multi-id list (the Fable row): the cache never prunes a window id, so an
// upstream rename leaves the old canonical bucket behind forever. Returning its
// rolled-over Unknown on sight would make the tolerant variant fallback inert in
// exactly the rename case it exists for — the row would stay unobservable while
// fresh variant telemetry kept arriving. A rolled-over bucket is still the
// answer when NO candidate is live, and the first such bucket in candidate order
// supplies the ObservedAt so the result stays deterministic.
//
// Among LIVE candidates the freshest observation wins, not merely the first in
// candidate order: a rename can leave the old canonical bucket with a still-future
// reset (live) while newer telemetry flows to the variant, and canonical-first
// would then pin the row to the stale percentage until the retained bucket finally
// rolls over. Ranking by ObservedAtMs lets the actively-updated bucket win; equal
// timestamps fall back to candidate order (canonical-first), so the result stays
// deterministic for terminal-service's payload-hash delta-skip.
func observedMetricOrUnknown(
	buckets map[string]claudeRateLimitBucket,
	windowIDs []string,
	kind, label string,
	now time.Time,
) cliAgentUsageMetric {
	rolledOver := ""
	freshestLive := ""
	// A live window we know the reset for but not the usage. Recorded from a
	// heartbeat, so it carries no percentage — but its reset is current, which
	// is strictly more useful than falling back to a rolled-over bucket's stale
	// "last observed".
	liveUnobserved := ""
	for _, id := range windowIDs {
		b, ok := buckets[id]
		if !ok {
			continue
		}
		if b.ResetsAtMs > 0 && now.UnixMilli() >= b.ResetsAtMs {
			if rolledOver == "" {
				rolledOver = id
			}
			continue
		}
		if !b.hasObservedUsage() {
			if liveUnobserved == "" || b.ResetsAtMs > buckets[liveUnobserved].ResetsAtMs {
				liveUnobserved = id
			}
			continue
		}
		// Strictly-greater keeps the earlier (canonical-first) candidate on a tie.
		if freshestLive == "" || b.ObservedAtMs > buckets[freshestLive].ObservedAtMs {
			freshestLive = id
		}
	}
	if freshestLive != "" {
		b := buckets[freshestLive]
		var resetAt string
		if b.ResetsAtMs > 0 {
			resetAt = time.UnixMilli(b.ResetsAtMs).UTC().Format(time.RFC3339)
		}
		used := clampPercent(b.UsedPercentage)
		return cliAgentUsageMetric{
			Kind: kind, Label: label, Unit: "%",
			Total: floatPtr(100), Consumed: floatPtr(used), Remaining: floatPtr(100 - used),
			ResetAt: resetAt, ObservedAt: observedAtRFC3339(b.ObservedAtMs),
		}
	}
	// A current window with no reading beats a rolled-over one with a stale
	// reading: "unobservable, resets Friday" is true, where "last observed six
	// days ago" describes a window that no longer exists. No ObservedAt — the
	// usage was never observed, and claiming otherwise is what made a stuck card
	// look merely stale.
	if liveUnobserved != "" {
		b := buckets[liveUnobserved]
		return cliAgentUsageMetric{
			Kind: kind, Label: label, Unit: "%",
			ResetAt: time.UnixMilli(b.ResetsAtMs).UTC().Format(time.RFC3339), Unknown: true,
		}
	}
	if rolledOver != "" {
		return cliAgentUsageMetric{
			Kind: kind, Label: label, Unit: "%",
			ObservedAt: observedAtRFC3339(buckets[rolledOver].ObservedAtMs), Unknown: true,
		}
	}
	return cliAgentUsageMetric{Kind: kind, Label: label, Unit: "%", Unknown: true}
}

func observedAtRFC3339(ms int64) string {
	if ms <= 0 {
		return ""
	}
	// Preserve the bucket's millisecond precision. Besides making the displayed
	// observation truthful, freshness checks must be able to order two events
	// that happened within the same second.
	return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
}

// latestClaudeObservation returns the newest per-window observation time held
// in the cache, or the zero time when no window carries a real reading.
//
// Buckets whose usage was never observed (heartbeat-only rows —
// hasObservedUsage() false) are skipped deliberately: their ObservedAtMs
// records when the reset time was seen, not when a percentage was measured, so
// counting them would report the card as fresh in exactly the state this
// staleness check exists to detect.
func latestClaudeObservation(buckets map[string]claudeRateLimitBucket) time.Time {
	newest := int64(0)
	for _, b := range buckets {
		if !b.hasObservedUsage() {
			continue
		}
		if b.ObservedAtMs > newest {
			newest = b.ObservedAtMs
		}
	}
	if newest <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(newest).UTC()
}
