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
	}, now)

	metrics := claudeCodeMetricsFromCache(now)
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

	metrics := claudeCodeMetricsFromCache(time.Now())
	if len(metrics) != 2 {
		t.Fatalf("want 2 metrics, got %d", len(metrics))
	}
	for _, m := range metrics {
		if !m.Unknown {
			t.Errorf("metric %q should be Unknown without a cache", m.Kind)
		}
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
