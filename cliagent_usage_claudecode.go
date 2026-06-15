// cliagent_usage_claudecode.go — Claude Code usage parser.
//
// Claude Code exposes no public utilization API and no on-disk usage file.
// What it DOES emit is a structured `rate_limit_event` on its stream-json
// stdout during a session, carrying the exact reset time and utilization per
// window. session.go captures those events into a small on-disk cache
// (cliagent_ratelimit.go); this parser turns the latest snapshot into the
// real five-hour / weekly capacity metrics shown on the CLI Agents tab.
//
// When the cache is absent (no Claude session has run since install, or the
// account isn't a Claude.ai subscription so no rate_limit_event is emitted),
// the metrics fall back to Unknown=true and the UI renders the dashed
// "usage unobservable" gauge — same as before.
//
// CLAUDE_CONFIG_DIR/.credentials.json (or ~/.claude/.credentials.json) is still
// read for account fingerprinting + plan display.
package main

import (
	"os"
	"time"
)

type claudeCodeUsageParser struct{}

func (claudeCodeUsageParser) Provider() string { return "claudeCode" }

type claudeCredentials struct {
	Account      string `json:"account"`
	Email        string `json:"email"`
	Organization string `json:"organization"`
	Plan         string `json:"plan"`
	Subscription string `json:"subscription"`
}

func (p claudeCodeUsageParser) Parse(home string, detected detectedCLIAgent, now time.Time) (*cliAgentUsage, bool) {
	base := claudeConfigDir(home)
	if base == "" {
		return nil, false
	}

	usage := &cliAgentUsage{
		Provider:    p.Provider(),
		Name:        firstNonEmpty(detected.Name, "Claude Code"),
		Version:     detected.Version,
		Path:        detected.Path,
		DataSource:  "rate_limit_event",
		CollectedAt: now.UTC().Format(time.RFC3339),
	}

	creds := claudeCredentials{}
	if readJSONFile(expandHome(base, ".credentials.json"), &creds) {
		usage.Account = firstNonEmpty(creds.Email, creds.Account, creds.Organization)
		usage.Plan = firstNonEmpty(creds.Plan, creds.Subscription)
	}
	usage.AccountFingerprint = fingerprintAccount(p.Provider(), usage.Account)

	usage.Metrics = claudeCodeMetricsFromCache(now, usage.AccountFingerprint)
	return usage, true
}

// claudeConfigDir resolves the Claude config dir using the same precedence Parse
// uses (CLAUDE_CONFIG_DIR override, then ~/.claude). Empty when neither resolves.
func claudeConfigDir(home string) string {
	return firstNonEmpty(os.Getenv("CLAUDE_CONFIG_DIR"), expandHome(home, ".claude"))
}

// currentClaudeAccountFingerprint reads the Claude credentials on disk and
// returns the same fingerprint Parse would attach to a usage snapshot. Used by
// the capture path to scope the rate-limit cache to the active account.
// Returns "" when no creds are readable, in which case the cache is unscoped
// (best-effort, matches pre-scoping behavior).
func currentClaudeAccountFingerprint() string {
	home, _ := os.UserHomeDir()
	base := claudeConfigDir(home)
	if base == "" {
		return ""
	}
	creds := claudeCredentials{}
	if !readJSONFile(expandHome(base, ".credentials.json"), &creds) {
		return ""
	}
	account := firstNonEmpty(creds.Email, creds.Account, creds.Organization)
	return fingerprintAccount("claudeCode", account)
}

// claudeCodeMetricsFromCache builds the metric rows from the rate-limit cache,
// falling back to the Unknown placeholders when a window hasn't been observed.
// Two rows are always shown so the card layout is stable: the 5-hour session
// window and the weekly window.
//
// The cache is trusted only when its `accountFingerprint` exactly matches the
// caller-supplied one. Two empty fingerprints match (no creds + unscoped
// snapshot — the test/legacy default), but a scoped snapshot is ignored when
// the current account cannot be identified: otherwise `gatherCLIAgentUsage`
// would attribute a previous user's reset windows to the device-scoped
// fallback entry after the local credentials were removed.
func claudeCodeMetricsFromCache(now time.Time, currentFingerprint string) []cliAgentUsageMetric {
	snap, ok := loadClaudeRateLimitSnapshot(claudeRateLimitCachePath())
	buckets := map[string]claudeRateLimitBucket{}
	if ok && snap.AccountFingerprint == currentFingerprint {
		buckets = snap.Buckets
	}

	session := observedMetricOrUnknown(
		buckets, []string{claudeWindowFiveHour}, limitKindSession, "5-hour session window", now)
	// Weekly is reported under seven_day; some plans split it per-model. When
	// both per-model buckets are present we aggregate CONSERVATIVELY so an
	// exhausted Opus quota isn't hidden behind a healthier Sonnet number.
	weekly := aggregateWeeklyMetric(buckets, now)

	return []cliAgentUsageMetric{session, weekly}
}

// aggregateWeeklyMetric reports the worst observed seven-day window: the
// highest used percentage and the soonest reset across the unified `seven_day`
// bucket and any per-model split (`seven_day_sonnet`, `seven_day_opus`). This
// prevents a healthy Sonnet bucket from masking a depleted Opus bucket on
// plans that emit them separately.
func aggregateWeeklyMetric(buckets map[string]claudeRateLimitBucket, now time.Time) cliAgentUsageMetric {
	windowIDs := []string{claudeWindowSevenDay, claudeWindowSevenDaySonnet, claudeWindowSevenDayOpus}
	var (
		observed    bool
		worstUsed   float64
		worstReset  int64
	)
	for _, id := range windowIDs {
		b, ok := buckets[id]
		if !ok {
			continue
		}
		used := b.UsedPercentage
		liveReset := b.ResetsAtMs > 0 && now.UnixMilli() < b.ResetsAtMs
		if b.ResetsAtMs > 0 && now.UnixMilli() >= b.ResetsAtMs {
			used = 0 // this sub-window has already rolled over
		}
		used = clampPercent(used)
		// Pair the reset with the bucket that produced worstUsed so the UI
		// reports when the *constraining* quota clears, not when an unrelated
		// healthier bucket happens to reset first.
		if !observed || used > worstUsed {
			worstUsed = used
			if liveReset {
				worstReset = b.ResetsAtMs
			} else {
				worstReset = 0
			}
		}
		observed = true
	}
	if !observed {
		return cliAgentUsageMetric{Kind: limitKindWeekly, Label: "Weekly quota", Unit: "%", Unknown: true}
	}
	var resetAt string
	if worstReset > 0 {
		resetAt = time.UnixMilli(worstReset).UTC().Format(time.RFC3339)
	}
	return cliAgentUsageMetric{
		Kind:      limitKindWeekly,
		Label:     "Weekly quota",
		Unit:      "%",
		Total:     floatPtr(100),
		Consumed:  floatPtr(worstUsed),
		Remaining: floatPtr(100 - worstUsed),
		ResetAt:   resetAt,
	}
}

// observedMetricOrUnknown returns a real percentage metric for the first window
// id present in the cache, or an Unknown placeholder when none is observed. A
// window whose reset time has already passed is reported as 0% used (the window
// rolled over), which is more honest than showing a stale high-water mark.
func observedMetricOrUnknown(
	buckets map[string]claudeRateLimitBucket,
	windowIDs []string,
	kind, label string,
	now time.Time,
) cliAgentUsageMetric {
	for _, id := range windowIDs {
		b, ok := buckets[id]
		if !ok {
			continue
		}
		used := b.UsedPercentage
		var resetAt string
		if b.ResetsAtMs > 0 {
			if now.UnixMilli() >= b.ResetsAtMs {
				used = 0 // window has reset since we last observed it
			} else {
				resetAt = time.UnixMilli(b.ResetsAtMs).UTC().Format(time.RFC3339)
			}
		}
		used = clampPercent(used)
		return cliAgentUsageMetric{
			Kind:      kind,
			Label:     label,
			Unit:      "%",
			Total:     floatPtr(100),
			Consumed:  floatPtr(used),
			Remaining: floatPtr(100 - used),
			ResetAt:   resetAt,
		}
	}
	return cliAgentUsageMetric{Kind: kind, Label: label, Unit: "%", Unknown: true}
}
