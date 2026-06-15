package main

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func TestCaptureClaudeRateLimit_NestedInfo_SecondsAndUtilization(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	resetSec := now.Add(2 * time.Hour).Unix() // seconds, as the status-line/SDK uses
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rate_limit_type":"five_hour","utilization":0.235,"resets_at":` +
		itoa(resetSec) + `}}`

	rejected := captureClaudeRateLimitLine(line, now)
	if rejected != nil {
		t.Fatalf("status allowed must not be reported as rejected")
	}

	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache to be written")
	}
	b, ok := snap.Buckets[claudeWindowFiveHour]
	if !ok {
		t.Fatalf("expected five_hour bucket, got %+v", snap.Buckets)
	}
	if b.UsedPercentage < 23.4 || b.UsedPercentage > 23.6 {
		t.Errorf("UsedPercentage=%v, want ~23.5 (utilization 0.235 -> %%)", b.UsedPercentage)
	}
	if b.ResetsAtMs != resetSec*1000 {
		t.Errorf("ResetsAtMs=%d, want %d (seconds normalised to ms)", b.ResetsAtMs, resetSec*1000)
	}
}

func TestCaptureClaudeRateLimit_RateLimitsMapShape_MsAndPercentage(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	fiveReset := now.Add(time.Hour).UnixMilli() // already ms
	weekReset := now.Add(72 * time.Hour).UnixMilli()
	line := `{"type":"result","rate_limits":{"five_hour":{"used_percentage":41.2,"resets_at":` +
		itoa(fiveReset) + `},"seven_day":{"used_percentage":88,"resets_at":` + itoa(weekReset) + `}}}`

	captureClaudeRateLimitLine(line, now)
	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache write")
	}
	if got := snap.Buckets[claudeWindowFiveHour]; got.UsedPercentage != 41.2 || got.ResetsAtMs != fiveReset {
		t.Errorf("five_hour=%+v, want used 41.2 reset %d", got, fiveReset)
	}
	if got := snap.Buckets[claudeWindowSevenDay]; got.UsedPercentage != 88 || got.ResetsAtMs != weekReset {
		t.Errorf("seven_day=%+v, want used 88 reset %d", got, weekReset)
	}
}

func TestCaptureClaudeRateLimit_RejectedSurfacesResetLine(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	reset := now.Add(28 * time.Minute)
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rate_limit_type":"five_hour","utilization":1.0,"resets_at":` +
		itoa(reset.Unix()) + `}}`

	rejected := captureClaudeRateLimitLine(line, now)
	if rejected == nil {
		t.Fatalf("rejected window must be surfaced")
	}
	notice := formatClaudeLimitLine(*rejected)
	// Must satisfy agent-orchestrator-service's iso-timestamp matcher:
	// a usage-limit cue + "resets at <ISO-8601>".
	if !strings.Contains(notice, "usage limit") || !strings.Contains(notice, "resets at ") {
		t.Errorf("limit line %q missing cue/reset phrasing", notice)
	}
	if !strings.Contains(notice, reset.UTC().Format(time.RFC3339)) {
		t.Errorf("limit line %q missing exact reset timestamp", notice)
	}
}

func TestClaudeCodeMetricsFromCache_ObservedAndPastReset(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	// five_hour observed and live; seven_day's reset already passed.
	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {UsedPercentage: 23.5, ResetsAtMs: now.Add(time.Hour).UnixMilli(), Status: "allowed"},
		claudeWindowSevenDay: {UsedPercentage: 90, ResetsAtMs: now.Add(-time.Hour).UnixMilli(), Status: "allowed"},
	}, now, "")

	metrics := claudeCodeMetricsFromCache(now, "")
	if len(metrics) != 2 {
		t.Fatalf("want 2 metrics, got %d", len(metrics))
	}
	session := metrics[0]
	if session.Unknown {
		t.Errorf("session metric should be observed, got Unknown")
	}
	if session.Consumed == nil || *session.Consumed < 23.4 || *session.Consumed > 23.6 {
		t.Errorf("session Consumed=%v, want ~23.5", session.Consumed)
	}
	if session.ResetAt == "" {
		t.Errorf("live window should carry a ResetAt")
	}
	weekly := metrics[1]
	if weekly.Consumed == nil || *weekly.Consumed != 0 {
		t.Errorf("weekly past-reset Consumed=%v, want 0 (rolled over)", weekly.Consumed)
	}
	if weekly.ResetAt != "" {
		t.Errorf("past-reset window should not advertise a stale ResetAt")
	}
}

func TestClaudeCodeMetricsFromCache_NoCacheFallsBackToUnknown(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "absent.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)

	metrics := claudeCodeMetricsFromCache(time.Now(), "")
	if len(metrics) != 2 {
		t.Fatalf("want 2 metrics, got %d", len(metrics))
	}
	for _, m := range metrics {
		if !m.Unknown {
			t.Errorf("metric %q should be Unknown without a cache", m.Kind)
		}
	}
}

// Claude Code's rejected rate_limit_event emits camelCase keys upstream
// (`rateLimitType`, `resetsAt`, `usedPercentage`). The capture path must
// recognise that shape too — otherwise a real limit hit yields an empty
// window, no rejected bucket, and the orchestrator never gets the synthetic
// "resets at" line that drives auto-defer.
func TestCaptureClaudeRateLimit_CamelCase_RejectedSurfacesResetLine(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	reset := now.Add(45 * time.Minute)
	line := `{"type":"rate_limit_event","rateLimitInfo":{"status":"rejected","rateLimitType":"five_hour","utilization":1.0,"resetsAt":` +
		itoa(reset.Unix()) + `}}`

	rejected := captureClaudeRateLimitLine(line, now)
	if rejected == nil {
		t.Fatalf("rejected camelCase window must be surfaced")
	}
	notice := formatClaudeLimitLine(*rejected)
	if !strings.Contains(notice, "usage limit") || !strings.Contains(notice, "resets at ") {
		t.Errorf("limit line %q missing cue/reset phrasing", notice)
	}
	if !strings.Contains(notice, reset.UTC().Format(time.RFC3339)) {
		t.Errorf("limit line %q missing exact reset timestamp", notice)
	}
	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache to be written from camelCase event")
	}
	b, ok := snap.Buckets[claudeWindowFiveHour]
	if !ok {
		t.Fatalf("expected five_hour bucket, got %+v", snap.Buckets)
	}
	if b.Status != claudeRateLimitStatusRejected {
		t.Errorf("Status=%q, want rejected", b.Status)
	}
	if b.ResetsAtMs != reset.Unix()*1000 {
		t.Errorf("ResetsAtMs=%d, want %d", b.ResetsAtMs, reset.Unix()*1000)
	}
}

func TestCaptureClaudeRateLimit_CamelCase_RateLimitsMapShape(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	fiveReset := now.Add(time.Hour).UnixMilli()
	line := `{"type":"result","rateLimits":{"five_hour":{"usedPercentage":62.5,"resetsAt":` +
		itoa(fiveReset) + `}}}`

	captureClaudeRateLimitLine(line, now)
	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache write")
	}
	got := snap.Buckets[claudeWindowFiveHour]
	if got.UsedPercentage != 62.5 || got.ResetsAtMs != fiveReset {
		t.Errorf("five_hour=%+v, want used 62.5 reset %d", got, fiveReset)
	}
}

// Split weekly windows must aggregate to the worst observed sub-window so an
// exhausted Opus quota isn't hidden behind a healthier Sonnet number.
func TestClaudeCodeMetricsFromCache_SplitWeeklyAggregatesConservatively(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	sonnetReset := now.Add(96 * time.Hour).UnixMilli()
	opusReset := now.Add(36 * time.Hour).UnixMilli() // sooner — Opus exhausted first
	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowSevenDaySonnet: {UsedPercentage: 32, ResetsAtMs: sonnetReset, Status: "allowed"},
		claudeWindowSevenDayOpus:   {UsedPercentage: 99, ResetsAtMs: opusReset, Status: "allowed"},
	}, now, "")

	metrics := claudeCodeMetricsFromCache(now, "")
	weekly := metrics[1]
	if weekly.Unknown {
		t.Fatalf("weekly should be observed")
	}
	if weekly.Consumed == nil || *weekly.Consumed != 99 {
		t.Errorf("weekly Consumed=%v, want 99 (worst of sonnet/opus)", weekly.Consumed)
	}
	wantReset := time.UnixMilli(opusReset).UTC().Format(time.RFC3339)
	if weekly.ResetAt != wantReset {
		t.Errorf("weekly ResetAt=%q, want %q (soonest of sonnet/opus)", weekly.ResetAt, wantReset)
	}
}

// When the cache was captured under a different account fingerprint, the
// display must not attribute those buckets to the current account.
func TestClaudeCodeMetricsFromCache_IgnoresOtherAccountSnapshot(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {UsedPercentage: 80, ResetsAtMs: now.Add(time.Hour).UnixMilli(), Status: "allowed"},
	}, now, "fp-previous-account")

	metrics := claudeCodeMetricsFromCache(now, "fp-current-account")
	for _, m := range metrics {
		if !m.Unknown {
			t.Errorf("metric %q must be Unknown when snapshot belongs to another account", m.Kind)
		}
	}
}

// Merging telemetry under a new account fingerprint must drop the previous
// account's buckets — otherwise a stale weekly reset survives the switch.
func TestMergeClaudeRateLimitCache_DropsOtherAccountBuckets(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowSevenDay: {UsedPercentage: 70, ResetsAtMs: now.Add(72 * time.Hour).UnixMilli(), Status: "allowed"},
	}, now, "fp-A")

	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {UsedPercentage: 10, ResetsAtMs: now.Add(time.Hour).UnixMilli(), Status: "allowed"},
	}, now, "fp-B")

	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected snapshot")
	}
	if snap.AccountFingerprint != "fp-B" {
		t.Errorf("AccountFingerprint=%q, want fp-B", snap.AccountFingerprint)
	}
	if _, present := snap.Buckets[claudeWindowSevenDay]; present {
		t.Errorf("seven_day bucket from previous account must not survive the switch")
	}
	if _, present := snap.Buckets[claudeWindowFiveHour]; !present {
		t.Errorf("five_hour bucket from current account should be present")
	}
}

// When the current account cannot be identified (creds removed or
// unreadable), a scoped snapshot from a previous account must NOT be trusted
// — otherwise gatherCLIAgentUsage attributes the prior user's reset windows
// to the device-scoped fallback entry on the CLI Agents tab.
func TestClaudeCodeMetricsFromCache_IgnoresScopedSnapshotWhenAccountUnknown(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {UsedPercentage: 80, ResetsAtMs: now.Add(time.Hour).UnixMilli(), Status: "allowed"},
		claudeWindowSevenDay: {UsedPercentage: 91, ResetsAtMs: now.Add(48 * time.Hour).UnixMilli(), Status: "allowed"},
	}, now, "fp-prior-signed-in-account")

	metrics := claudeCodeMetricsFromCache(now, "")
	for _, m := range metrics {
		if !m.Unknown {
			t.Errorf("metric %q must be Unknown when current account is unidentifiable (scoped cache must not leak)", m.Kind)
		}
	}
}

// An unscoped snapshot (legacy or test-written without a fingerprint) is
// accepted by an unscoped reader — preserves the "no creds anywhere" path
// used by capture-path tests and pre-scoping caches.
func TestClaudeCodeMetricsFromCache_UnscopedSnapshotWithUnknownAccount(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {UsedPercentage: 25, ResetsAtMs: now.Add(time.Hour).UnixMilli(), Status: "allowed"},
	}, now, "")

	metrics := claudeCodeMetricsFromCache(now, "")
	if metrics[0].Unknown {
		t.Errorf("unscoped reader must accept unscoped snapshot, got Unknown")
	}
	if metrics[0].Consumed == nil || *metrics[0].Consumed != 25 {
		t.Errorf("session Consumed=%v, want 25", metrics[0].Consumed)
	}
}

// A rejected rate_limit_event without an explicit usage field must be treated
// as fully exhausted — otherwise the cache renders 0% consumed alongside the
// future reset, contradicting the hard-limit auto-defer session.go just fired.
func TestCaptureClaudeRateLimit_RejectedWithoutUsageMarksExhausted(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	reset := now.Add(30 * time.Minute)
	line := `{"type":"rate_limit_event","rateLimitInfo":{"status":"rejected","rateLimitType":"five_hour","resetsAt":` +
		itoa(reset.Unix()) + `}}`

	rejected := captureClaudeRateLimitLine(line, now)
	if rejected == nil {
		t.Fatalf("rejected window must be surfaced even without usage field")
	}
	if rejected.UsedPercentage != 100 {
		t.Errorf("rejected bucket UsedPercentage=%v, want 100 (exhausted by default)", rejected.UsedPercentage)
	}
	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache to be written")
	}
	b := snap.Buckets[claudeWindowFiveHour]
	if b.UsedPercentage != 100 {
		t.Errorf("cached five_hour UsedPercentage=%v, want 100", b.UsedPercentage)
	}
}

// A transition between unscoped and scoped fingerprints is an account
// boundary too: buckets cached while creds were unreadable must not survive
// into the new account's snapshot, or the next account would inherit a stray
// reset window.
func TestMergeClaudeRateLimitCache_DropsUnscopedBucketsOnAccountSignIn(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowSevenDay: {UsedPercentage: 88, ResetsAtMs: now.Add(72 * time.Hour).UnixMilli(), Status: "allowed"},
	}, now, "")

	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {UsedPercentage: 5, ResetsAtMs: now.Add(time.Hour).UnixMilli(), Status: "allowed"},
	}, now, "fp-new-account")

	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected snapshot")
	}
	if snap.AccountFingerprint != "fp-new-account" {
		t.Errorf("AccountFingerprint=%q, want fp-new-account", snap.AccountFingerprint)
	}
	if _, present := snap.Buckets[claudeWindowSevenDay]; present {
		t.Errorf("unscoped seven_day bucket must not be carried into the signed-in account snapshot")
	}
	if _, present := snap.Buckets[claudeWindowFiveHour]; !present {
		t.Errorf("five_hour bucket from the signed-in account should be present")
	}
}

func TestNormalizeResetMs(t *testing.T) {
	if got := normalizeResetMs(1781544600); got != 1781544600000 {
		t.Errorf("seconds: got %d, want 1781544600000", got)
	}
	if got := normalizeResetMs(1781544600000); got != 1781544600000 {
		t.Errorf("ms: got %d, want 1781544600000", got)
	}
	if got := normalizeResetMs(0); got != 0 {
		t.Errorf("zero: got %d, want 0", got)
	}
}
