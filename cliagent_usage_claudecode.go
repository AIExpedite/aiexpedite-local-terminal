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
	base := firstNonEmpty(os.Getenv("CLAUDE_CONFIG_DIR"), expandHome(home, ".claude"))
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

	usage.Metrics = claudeCodeMetricsFromCache(now)
	return usage, true
}

// claudeCodeMetricsFromCache builds the metric rows from the rate-limit cache,
// falling back to the Unknown placeholders when a window hasn't been observed.
// Two rows are always shown so the card layout is stable: the 5-hour session
// window and the weekly window.
func claudeCodeMetricsFromCache(now time.Time) []cliAgentUsageMetric {
	snap, ok := loadClaudeRateLimitSnapshot(claudeRateLimitCachePath())
	buckets := map[string]claudeRateLimitBucket{}
	if ok {
		buckets = snap.Buckets
	}

	session := observedMetricOrUnknown(
		buckets, []string{claudeWindowFiveHour}, limitKindSession, "5-hour session window", now)
	// Weekly is reported under seven_day; some plans split it per-model.
	weekly := observedMetricOrUnknown(
		buckets,
		[]string{claudeWindowSevenDay, claudeWindowSevenDaySonnet, claudeWindowSevenDayOpus},
		limitKindWeekly, "Weekly quota", now)

	return []cliAgentUsageMetric{session, weekly}
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
