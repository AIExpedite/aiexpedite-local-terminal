package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func TestCaptureClaudeRateLimit_NestedInfo_SecondsAndUtilization(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // isolate from the real ~/.claude hook

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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // isolate from the real ~/.claude hook

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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // isolate from the real ~/.claude hook

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

// When a single telemetry event carries multiple rejected windows (e.g. the
// 5-hour and the weekly Opus bucket are both exhausted), the surfaced reset
// time must be the LATEST of the rejected windows. Picking the soonest would
// wake the orchestrator while the longer window is still blocked, causing an
// immediate re-rejection from Claude.
func TestCaptureClaudeRateLimit_MultiRejectedPicksLatestReset(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // isolate from the real ~/.claude hook

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	soonReset := now.Add(28 * time.Minute)
	lateReset := now.Add(72 * time.Hour)
	line := `{"rate_limits":{` +
		`"five_hour":{"status":"rejected","utilization":1.0,"resets_at":` + itoa(soonReset.Unix()) + `},` +
		`"seven_day_opus":{"status":"rejected","utilization":1.0,"resets_at":` + itoa(lateReset.Unix()) + `}` +
		`}}`

	rejected := captureClaudeRateLimitLine(line, now)
	if rejected == nil {
		t.Fatalf("rejected window must be surfaced")
	}
	if rejected.ResetsAtMs != lateReset.UnixMilli() {
		t.Errorf("ResetsAtMs=%d, want %d (latest of the rejected buckets)", rejected.ResetsAtMs, lateReset.UnixMilli())
	}
}

func TestClaudeCodeMetricsFromCache_ObservedAndPastReset(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // isolate from the real ~/.claude hook

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	// five_hour observed and live; seven_day's reset already passed.
	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {UsedPercentage: 23.5, ResetsAtMs: now.Add(time.Hour).UnixMilli(), ObservedAtMs: now.UnixMilli(), Status: "allowed", usageKnown: true},
		claudeWindowSevenDay: {UsedPercentage: 90, ResetsAtMs: now.Add(-time.Hour).UnixMilli(), ObservedAtMs: now.UnixMilli(), Status: "allowed", usageKnown: true},
	}, now, "")

	metrics := claudeCodeMetricsFromCache(now, "")
	if len(metrics) != 3 {
		t.Fatalf("want 3 metrics, got %d", len(metrics))
	}
	if fable := metrics[2]; fable.Label != "Weekly Fable" || !fable.Unknown {
		t.Errorf("fable metric=%+v, want an Unknown \"Weekly Fable\" row (no fable bucket cached)", fable)
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
	if !weekly.Unknown || weekly.Consumed != nil {
		t.Errorf("weekly past-reset metric=%+v, want unobservable", weekly)
	}
	if weekly.ResetAt != "" {
		t.Errorf("past-reset window should not advertise a stale ResetAt")
	}
	if weekly.ObservedAt == "" || session.ObservedAt == "" {
		t.Errorf("metrics must preserve observation time: session=%+v weekly=%+v", session, weekly)
	}
}

func TestClaudeCodeMetricsFromCache_NoCacheFallsBackToUnknown(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "absent.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // isolate from the real ~/.claude hook

	metrics := claudeCodeMetricsFromCache(time.Now(), "")
	if len(metrics) != 3 {
		t.Fatalf("want 3 metrics, got %d", len(metrics))
	}
	for _, m := range metrics {
		if !m.Unknown {
			t.Errorf("metric %q should be Unknown without a cache", m.Label)
		}
	}
	if fable := metrics[2]; fable.Label != "Weekly Fable" || fable.Kind != limitKindWeekly {
		t.Errorf("fable metric=%+v, want the Unknown \"Weekly Fable\" weekly row", fable)
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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // isolate from the real ~/.claude hook

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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // isolate from the real ~/.claude hook

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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // isolate from the real ~/.claude hook

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	sonnetReset := now.Add(96 * time.Hour).UnixMilli()
	opusReset := now.Add(36 * time.Hour).UnixMilli() // sooner — Opus exhausted first
	fableReset := now.Add(120 * time.Hour).UnixMilli()
	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowSevenDaySonnet: {UsedPercentage: 32, ResetsAtMs: sonnetReset, Status: "allowed", usageKnown: true},
		claudeWindowSevenDayOpus:   {UsedPercentage: 99, ResetsAtMs: opusReset, Status: "allowed", usageKnown: true},
		// Fable is metered separately and must NOT move the aggregate below,
		// even though its 100% is the worst number in the cache.
		claudeWindowSevenDayFable: {UsedPercentage: 100, ResetsAtMs: fableReset, Status: "rejected", usageKnown: true},
	}, now, "")

	metrics := claudeCodeMetricsFromCache(now, "")
	weekly := metrics[1]
	if weekly.Unknown {
		t.Fatalf("weekly should be observed")
	}
	if weekly.Consumed == nil || *weekly.Consumed != 99 {
		t.Errorf("weekly Consumed=%v, want 99 (worst of sonnet/opus; fable must not be folded in)", weekly.Consumed)
	}
	wantReset := time.UnixMilli(opusReset).UTC().Format(time.RFC3339)
	if weekly.ResetAt != wantReset {
		t.Errorf("weekly ResetAt=%q, want %q (soonest of sonnet/opus)", weekly.ResetAt, wantReset)
	}
	fable := metrics[2]
	if fable.Consumed == nil || *fable.Consumed != 100 {
		t.Fatalf("fable Consumed=%v, want 100 (its own row)", fable.Consumed)
	}
	if wantFableReset := time.UnixMilli(fableReset).UTC().Format(time.RFC3339); fable.ResetAt != wantFableReset {
		t.Errorf("fable ResetAt=%q, want %q", fable.ResetAt, wantFableReset)
	}
}

// When the cache was captured under a different account fingerprint, the
// display must not attribute those buckets to the current account.
func TestClaudeCodeMetricsFromCache_IgnoresOtherAccountSnapshot(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // isolate from the real ~/.claude hook

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {UsedPercentage: 80, ResetsAtMs: now.Add(time.Hour).UnixMilli(), Status: "allowed", usageKnown: true},
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
		claudeWindowSevenDay: {UsedPercentage: 70, ResetsAtMs: now.Add(72 * time.Hour).UnixMilli(), Status: "allowed", usageKnown: true},
	}, now, "fp-A")

	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {UsedPercentage: 10, ResetsAtMs: now.Add(time.Hour).UnixMilli(), Status: "allowed", usageKnown: true},
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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // isolate from the real ~/.claude hook

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {UsedPercentage: 80, ResetsAtMs: now.Add(time.Hour).UnixMilli(), Status: "allowed", usageKnown: true},
		claudeWindowSevenDay: {UsedPercentage: 91, ResetsAtMs: now.Add(48 * time.Hour).UnixMilli(), Status: "allowed", usageKnown: true},
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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // isolate from the real ~/.claude hook

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {UsedPercentage: 25, ResetsAtMs: now.Add(time.Hour).UnixMilli(), Status: "allowed", usageKnown: true},
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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // isolate from the real ~/.claude hook

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
		claudeWindowSevenDay: {UsedPercentage: 88, ResetsAtMs: now.Add(72 * time.Hour).UnixMilli(), Status: "allowed", usageKnown: true},
	}, now, "")

	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {UsedPercentage: 5, ResetsAtMs: now.Add(time.Hour).UnixMilli(), Status: "allowed", usageKnown: true},
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

// An allowed heartbeat (status + reset time, no usage field) must NOT clobber
// a previously observed UsedPercentage with a zero default — Claude Code emits
// such heartbeats every session and the CLI Agents tab would otherwise decay
// to 0% consumed after each run.
func TestCaptureClaudeRateLimit_AllowedHeartbeatPreservesPriorUsage(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // isolate from the real ~/.claude hook

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	firstReset := now.Add(2 * time.Hour).Unix()
	first := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rate_limit_type":"five_hour","utilization":0.42,"resets_at":` +
		itoa(firstReset) + `}}`
	captureClaudeRateLimitLine(first, now)

	later := now.Add(15 * time.Minute)
	heartbeatReset := later.Add(105 * time.Minute).Unix()
	heartbeat := `{"type":"rate_limit_event","rateLimitInfo":{"status":"allowed","rateLimitType":"five_hour","resetsAt":` +
		itoa(heartbeatReset) + `}}`
	captureClaudeRateLimitLine(heartbeat, later)

	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache write")
	}
	b := snap.Buckets[claudeWindowFiveHour]
	if b.UsedPercentage < 41.9 || b.UsedPercentage > 42.1 {
		t.Errorf("UsedPercentage=%v, want preserved ~42 (heartbeat must not zero it out)", b.UsedPercentage)
	}
	if b.ResetsAtMs != heartbeatReset*1000 {
		t.Errorf("ResetsAtMs=%d, want updated %d from the heartbeat", b.ResetsAtMs, heartbeatReset*1000)
	}
	if b.ObservedAtMs != now.UnixMilli() {
		t.Errorf("ObservedAtMs=%d, want original usage observation %d (heartbeat did not observe utilization)", b.ObservedAtMs, now.UnixMilli())
	}
}

// A first-ever "allowed" heartbeat with no utilization must not seed the cache
// with a fake 0%/100%-remaining bucket — there's no prior usage to preserve,
// so the snapshot should stay empty and the metrics fall back to Unknown.
func TestCaptureClaudeRateLimit_AllowedHeartbeatWithoutPriorUsageDoesNotPersist(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // isolate from the real ~/.claude hook

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	resetSec := now.Add(2 * time.Hour).Unix()
	line := `{"type":"rate_limit_event","rateLimitInfo":{"status":"allowed","rateLimitType":"five_hour","resetsAt":` +
		itoa(resetSec) + `}}`
	captureClaudeRateLimitLine(line, now)

	// The heartbeat DOES record the window it named — that is how the cache
	// learns about a window it has never seen a reading for. What it must never
	// do is let the zero UsedPercentage read back as an observation, which is
	// what usageObserved:false pins.
	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok {
		t.Fatal("heartbeat naming a reset must persist its window")
	}
	bucket, present := snap.Buckets[claudeWindowFiveHour]
	if !present {
		t.Fatal("heartbeat naming a reset must persist its window")
	}
	if bucket.hasObservedUsage() {
		t.Fatal("a heartbeat carries no usage; the bucket must be marked unobserved")
	}
	metrics := claudeCodeMetricsFromCache(now, "")
	for _, m := range metrics {
		if !m.Unknown {
			t.Errorf("metric %q want Unknown until a usage reading arrives, got Consumed=%v", m.Kind, m.Consumed)
		}
	}
}

// A rejected rate_limit_event may omit the window id entirely — Claude Code's
// SDKRateLimitEvent carries status/resetsAt/utilization with no rate_limit_type.
// It can't be cached per-window, but it MUST still drive auto-defer, so
// captureClaudeRateLimitLine has to surface it.
func TestCaptureClaudeRateLimit_WindowlessRejectedSurfacesResetLine(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	reset := now.Add(28 * time.Minute)
	// No rate_limit_type / rateLimitType anywhere in the event.
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","utilization":1.0,"resets_at":` +
		itoa(reset.Unix()) + `}}`

	rejected := captureClaudeRateLimitLine(line, now)
	if rejected == nil {
		t.Fatalf("windowless rejected event must still be surfaced for auto-defer")
	}
	notice := formatClaudeLimitLine(*rejected)
	if !strings.Contains(notice, "usage limit") || !strings.Contains(notice, reset.UTC().Format(time.RFC3339)) {
		t.Errorf("limit line %q missing cue / exact reset", notice)
	}
	// It must NOT be written to the cache under a guessed window.
	if snap, ok := loadClaudeRateLimitSnapshot(cache); ok && len(snap.Buckets) != 0 {
		t.Errorf("windowless event must not be cached, got buckets %+v", snap.Buckets)
	}
}

// A no-usage "allowed" heartbeat that arrives AFTER the prior window has rolled
// over (its reset has passed) must not replay the old percentage under the
// heartbeat's new, future reset. The metric should reflect the rolled-over
// window, not a stale high-water mark.
func TestCaptureClaudeRateLimit_RolledOverHeartbeatDropsStaleUsage(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // no creds -> unscoped fingerprint ""

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	firstReset := now.Add(30 * time.Minute)
	first := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rate_limit_type":"five_hour","utilization":0.95,"resets_at":` +
		itoa(firstReset.Unix()) + `}}`
	captureClaudeRateLimitLine(first, now)

	// 31 min later: the first window has reset. Heartbeat advertises a NEW reset
	// five hours out, with no usage reading.
	later := now.Add(31 * time.Minute)
	heartbeat := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rate_limit_type":"five_hour","resets_at":` +
		itoa(later.Add(5*time.Hour).Unix()) + `}}`
	captureClaudeRateLimitLine(heartbeat, later)

	metrics := claudeCodeMetricsFromCache(later, "")
	session := metrics[0]
	if session.Consumed != nil && *session.Consumed > 0 {
		t.Errorf("rolled-over window Consumed=%v, want 0 — stale 95%% must not carry under the new reset", *session.Consumed)
	}
	if !session.Unknown {
		t.Error("the new window has no reading yet, so the row must be Unknown")
	}
	// The heartbeat's reset IS reported. Pairing a STALE PERCENTAGE with a fresh
	// reset is the hazard; reporting the current window with no percentage is
	// not. Dropping the reset too is what froze the cache on the expired
	// window — nothing could then replace it.
	if want := later.Add(5 * time.Hour).UTC().Format(time.RFC3339); session.ResetAt != want {
		t.Errorf("session ResetAt = %q, want the heartbeat's current window %q", session.ResetAt, want)
	}
}

// The weekly aggregate's reset must come from the bucket that produced
// worstUsed, not from an unrelated healthier bucket that resets sooner.
func TestClaudeCodeMetricsFromCache_WeeklyResetTracksWorstBucket(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // isolate from the real ~/.claude hook

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	sonnetReset := now.Add(24 * time.Hour).UnixMilli()   // sooner, but healthy
	opusReset := now.Add(7 * 24 * time.Hour).UnixMilli() // later, constraining
	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowSevenDaySonnet: {UsedPercentage: 10, ResetsAtMs: sonnetReset, Status: "allowed", usageKnown: true},
		claudeWindowSevenDayOpus:   {UsedPercentage: 99, ResetsAtMs: opusReset, Status: "allowed", usageKnown: true},
	}, now, "")

	metrics := claudeCodeMetricsFromCache(now, "")
	weekly := metrics[1]
	if weekly.Consumed == nil || *weekly.Consumed != 99 {
		t.Fatalf("weekly Consumed=%v, want 99", weekly.Consumed)
	}
	wantReset := time.UnixMilli(opusReset).UTC().Format(time.RFC3339)
	if weekly.ResetAt != wantReset {
		t.Errorf("weekly ResetAt=%q, want %q (must follow the constraining Opus bucket, not the sooner Sonnet reset)", weekly.ResetAt, wantReset)
	}
}

// When two weekly sub-windows tie at the highest usage (e.g. both Sonnet
// and Opus rejected at 100%), the aggregate reset must follow the LATER of
// the two buckets — the rate limit doesn't clear until BOTH have rolled
// over. Picking the sooner one would tell operators they can retry while
// another tied bucket is still exhausted.
func TestClaudeCodeMetricsFromCache_WeeklyTieBreaksOnLaterReset(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // isolate from the real ~/.claude hook

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	sonnetReset := now.Add(24 * time.Hour).UnixMilli() // sooner
	opusReset := now.Add(96 * time.Hour).UnixMilli()   // later — still exhausted then
	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowSevenDaySonnet: {UsedPercentage: 100, ResetsAtMs: sonnetReset, Status: "rejected", usageKnown: true},
		claudeWindowSevenDayOpus:   {UsedPercentage: 100, ResetsAtMs: opusReset, Status: "rejected", usageKnown: true},
	}, now, "")

	metrics := claudeCodeMetricsFromCache(now, "")
	weekly := metrics[1]
	if weekly.Consumed == nil || *weekly.Consumed != 100 {
		t.Fatalf("weekly Consumed=%v, want 100", weekly.Consumed)
	}
	wantReset := time.UnixMilli(opusReset).UTC().Format(time.RFC3339)
	if weekly.ResetAt != wantReset {
		t.Errorf("weekly ResetAt=%q, want %q (later of two tied buckets)", weekly.ResetAt, wantReset)
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

// A machine with two agent channels installed shares ONE ~/.claude/settings.json,
// so only the agent that booted last owns the status-line hook — and the hook
// pins the cache to THAT agent's config dir. The reader must follow the pinned
// path, or the losing channel's Claude card freezes at its last capture and ages
// indefinitely while the device is online.
func TestClaudeCodeMetricsFromCache_ReadsCachePinnedByTheInstalledHook(t *testing.T) {
	ownCache := filepath.Join(t.TempDir(), "own", "rl.json")
	pinnedCache := filepath.Join(t.TempDir(), "pinned", "rl.json")
	configDir := t.TempDir()
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", ownCache)
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	helperWriteJSON(t, filepath.Join(configDir, "settings.json"), map[string]any{
		"statusLine": map[string]any{
			"type": "command",
			"command": "AIEXPEDITE_CLAUDE_RL_CACHE=" + posixSingleQuote(pinnedCache) +
				" AIEXPEDITE_CLAUDE_STATUSLINE_PREV='/tmp/prev.json'" +
				" '/opt/aiexpedite/aiexpedite-terminal' " + statusLineHookArg,
		},
	})

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	// Our own cache holds an OLD capture from before the other channel took over.
	mergeClaudeRateLimitCache(ownCache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 10,
			ResetsAtMs:     now.Add(time.Hour).UnixMilli(),
			ObservedAtMs:   now.Add(-72 * time.Hour).UnixMilli(),
			usageKnown:     true,
		},
	}, now.Add(-72*time.Hour), "")
	// The hook has been writing here ever since.
	mergeClaudeRateLimitCache(pinnedCache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 91,
			ResetsAtMs:     now.Add(time.Hour).UnixMilli(),
			ObservedAtMs:   now.Add(-5 * time.Minute).UnixMilli(),
			usageKnown:     true,
		},
	}, now, "")

	session := claudeCodeMetricsFromCache(now, "")[0]
	if session.Unknown {
		t.Fatalf("5-hour window should be observed from the pinned cache, got %+v", session)
	}
	if session.Consumed == nil || *session.Consumed != 91 {
		t.Errorf("Consumed=%v, want 91 (freshest observation across both caches)", session.Consumed)
	}
	wantObserved := observedAtRFC3339(now.Add(-5 * time.Minute).UnixMilli())
	if session.ObservedAt != wantObserved {
		t.Errorf("ObservedAt=%q, want %q", session.ObservedAt, wantObserved)
	}
}

// A cache pinned by a hook that is NOT ours must not be read: the value would be
// attributed to this account with no evidence it came from our capture at all.
func TestClaudeCodeMetricsFromCache_IgnoresForeignStatusLineCommand(t *testing.T) {
	ownCache := filepath.Join(t.TempDir(), "own", "rl.json")
	foreignCache := filepath.Join(t.TempDir(), "foreign", "rl.json")
	configDir := t.TempDir()
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", ownCache)
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	helperWriteJSON(t, filepath.Join(configDir, "settings.json"), map[string]any{
		"statusLine": map[string]any{
			"type": "command",
			"command": "AIEXPEDITE_CLAUDE_RL_CACHE=" + posixSingleQuote(foreignCache) +
				" ~/.claude/my-own-statusline.sh",
		},
	})

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	mergeClaudeRateLimitCache(foreignCache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 91,
			ResetsAtMs:     now.Add(time.Hour).UnixMilli(),
			ObservedAtMs:   now.UnixMilli(),
			usageKnown:     true,
		},
	}, now, "")

	if session := claudeCodeMetricsFromCache(now, "")[0]; !session.Unknown {
		t.Errorf("5-hour window should stay unobserved, got %+v", session)
	}
}

// A probe reading arrives with usageKnown=true, so it takes the fresh-reading
// path: it overwrites the percentage a same-window heartbeat carried forward AND
// advances ObservedAtMs. This is the whole mechanism by which a successful,
// under-quota run makes latestObservedAt move.
func TestMergeClaudeRateLimitCache_ProbeBucketOverridesCarriedHeartbeat(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)

	// A real reading from the stream, four hours ago.
	seeded := now.Add(-4 * time.Hour)
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 30, ResetsAtMs: reset.UnixMilli(),
			ObservedAtMs: seeded.UnixMilli(), Status: "allowed", usageKnown: true,
		},
	}, seeded, "", claudeRateLimitSourceStream)

	// A usage-less heartbeat on the same live window: carries the percentage AND
	// its original observation time forward, deliberately.
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {ResetsAtMs: reset.UnixMilli(), Status: "allowed"},
	}, now.Add(-time.Minute), "", claudeRateLimitSourceStream)

	carried, _ := loadClaudeRateLimitSnapshot(cache)
	if got := carried.Buckets[claudeWindowFiveHour]; got.ObservedAtMs != seeded.UnixMilli() {
		t.Fatalf("precondition: heartbeat advanced ObservedAtMs to %d, want the carried %d",
			got.ObservedAtMs, seeded.UnixMilli())
	}

	// The probe's reading must win on both counts.
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 71, ResetsAtMs: reset.UnixMilli(),
			ObservedAtMs: now.UnixMilli(), Status: "allowed", usageKnown: true,
		},
	}, now, "", claudeRateLimitSourceProbe)

	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok {
		t.Fatal("expected a cache write")
	}
	got := snap.Buckets[claudeWindowFiveHour]
	if got.UsedPercentage != 71 {
		t.Errorf("UsedPercentage=%v, want 71 from the probe", got.UsedPercentage)
	}
	if got.ObservedAtMs != now.UnixMilli() {
		t.Errorf("ObservedAtMs=%d, want the probe's %d", got.ObservedAtMs, now.UnixMilli())
	}
	if got.Source != claudeRateLimitSourceProbe {
		t.Errorf("Source=%q, want %q", got.Source, claudeRateLimitSourceProbe)
	}
}

// A heartbeat that carries a prior reading forward must also carry that
// reading's provenance: it observed the reset, not the percentage, so claiming
// its own source for a number it did not measure would misreport where the
// value came from.
func TestMergeClaudeRateLimitCache_HeartbeatKeepsCarriedSource(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)

	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 44, ResetsAtMs: reset.UnixMilli(),
			ObservedAtMs: now.Add(-time.Hour).UnixMilli(), Status: "allowed", usageKnown: true,
		},
	}, now.Add(-time.Hour), "", claudeRateLimitSourceProbe)

	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {ResetsAtMs: reset.UnixMilli(), Status: "allowed"},
	}, now, "", claudeRateLimitSourceStream)

	snap, _ := loadClaudeRateLimitSnapshot(cache)
	if got := snap.Buckets[claudeWindowFiveHour].Source; got != claudeRateLimitSourceProbe {
		t.Errorf("Source=%q, want the carried %q", got, claudeRateLimitSourceProbe)
	}
}

// Source round-trips through the on-disk snapshot, and a legacy cache written
// before the field existed still loads with its existing semantics (no source,
// and — per the nil UsageObserved rule — still an observed reading).
func TestClaudeRateLimitBucketSource_RoundTripsAndLegacyCacheLoads(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowSevenDay: {
			UsedPercentage: 12, ResetsAtMs: now.Add(96 * time.Hour).UnixMilli(),
			ObservedAtMs: now.UnixMilli(), usageKnown: true,
		},
	}, now, "", claudeRateLimitSourceStatusLine)

	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok {
		t.Fatal("expected a cache write")
	}
	if got := snap.Buckets[claudeWindowSevenDay].Source; got != claudeRateLimitSourceStatusLine {
		t.Errorf("Source=%q, want %q after a disk round-trip", got, claudeRateLimitSourceStatusLine)
	}

	// A snapshot with no `source` key — every cache written before this change.
	legacy := filepath.Join(t.TempDir(), "legacy.json")
	body := `{"updatedAt":"2026-08-29T08:00:06Z","buckets":{"five_hour":{` +
		`"usedPercentage":63,"resetsAtMs":` + itoa(now.Add(time.Hour).UnixMilli()) +
		`,"observedAtMs":` + itoa(now.Add(-time.Hour).UnixMilli()) + `,"status":"allowed"}}}`
	if err := os.WriteFile(legacy, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, ok := loadClaudeRateLimitSnapshot(legacy)
	if !ok {
		t.Fatal("a legacy cache with no source key must still load")
	}
	bucket := loaded.Buckets[claudeWindowFiveHour]
	if bucket.Source != "" {
		t.Errorf("Source=%q, want empty for a legacy bucket", bucket.Source)
	}
	if !bucket.hasObservedUsage() || bucket.UsedPercentage != 63 {
		t.Errorf("legacy bucket semantics changed: %+v", bucket)
	}
}

// A reading may only ever replace an OLDER one. The utilization probe stamps the
// instant its gather started and then spends up to three seconds on the wire, so
// a status-line render that lands mid-flight is genuinely newer — persisting the
// probe's answer over it would move latestObservedAt BACKWARDS and swap a fresh
// percentage for a stale one, which is the exact staleness this feature removes.
func TestMergeClaudeRateLimitCache_OlderReadingCannotOverwriteNewer(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	newer := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	older := newer.Add(-time.Minute) // the probe's pre-request `now`
	reset := newer.Add(2 * time.Hour)

	// The status-line hook writes while the probe's request is still in flight.
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			ObservedAtMs: newer.UnixMilli(), ResetsAtMs: reset.UnixMilli(),
			UsedPercentage: 80, Status: "allowed", usageKnown: true,
		},
	}, newer, "", claudeRateLimitSourceStatusLine)

	// The probe's response then arrives carrying the older observation time.
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			ObservedAtMs: older.UnixMilli(), ResetsAtMs: reset.UnixMilli(),
			UsedPercentage: 40, Status: "allowed", usageKnown: true,
		},
	}, older, "", claudeRateLimitSourceProbe)

	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok {
		t.Fatal("expected a cache snapshot")
	}
	five := snap.Buckets[claudeWindowFiveHour]
	if five.ObservedAtMs != newer.UnixMilli() {
		t.Errorf("ObservedAtMs=%d, want the newer %d — freshness must never regress",
			five.ObservedAtMs, newer.UnixMilli())
	}
	if five.UsedPercentage != 80 {
		t.Errorf("UsedPercentage=%v, want the newer reading's 80", five.UsedPercentage)
	}
	if five.Source != claudeRateLimitSourceStatusLine {
		t.Errorf("Source=%q, want the newer writer's %q", five.Source, claudeRateLimitSourceStatusLine)
	}
}

// A same-instant rewrite still lands: equal timestamps carry no ordering, and
// refusing them would make re-merging one observation depend on arrival order.
func TestMergeClaudeRateLimitCache_SameInstantReadingStillApplies(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)
	bucket := func(pct float64) map[string]claudeRateLimitBucket {
		return map[string]claudeRateLimitBucket{claudeWindowFiveHour: {
			ObservedAtMs: now.UnixMilli(), ResetsAtMs: reset.UnixMilli(),
			UsedPercentage: pct, Status: "allowed", usageKnown: true,
		}}
	}
	mergeClaudeRateLimitCacheFromSource(cache, bucket(40), now, "", claudeRateLimitSourceStatusLine)
	mergeClaudeRateLimitCacheFromSource(cache, bucket(55), now, "", claudeRateLimitSourceProbe)

	snap, _ := loadClaudeRateLimitSnapshot(cache)
	if got := snap.Buckets[claudeWindowFiveHour].UsedPercentage; got != 55 {
		t.Errorf("UsedPercentage=%v, want 55 — an equal timestamp must not block the write", got)
	}
}

// The merge is reachable from the probe, whose caller runs under a deadline, so
// lock acquisition must be bounded: one wedged status-line process must not hang
// the signed refresh handler or pin the probe's single-flight latch forever.
//
// Bounded means "gives up", and what a writer does after giving up depends on
// what it promised. A best-effort writer proceeds unlocked — it has no caller to
// mislead and the next event rewrites the window anyway.
func TestMergeClaudeRateLimitCache_BoundedByHeldCrossProcessLock(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	held, ok := acquireCrossProcessCacheLock(cache)
	if !ok {
		t.Skip("advisory locking unavailable on this filesystem")
	}
	defer func() {
		_ = unlockFile(held)
		_ = held.Close()
	}()

	prevWait := claudeRateLimitCacheLockWait
	claudeRateLimitCacheLockWait = 50 * time.Millisecond
	t.Cleanup(func() { claudeRateLimitCacheLockWait = prevWait })

	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	done := make(chan struct{})
	go func() {
		defer close(done)
		mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
			claudeWindowFiveHour: {
				ObservedAtMs: now.UnixMilli(), ResetsAtMs: now.Add(time.Hour).UnixMilli(),
				UsedPercentage: 12, Status: "allowed", usageKnown: true,
			},
		}, now, "", claudeRateLimitSourceStream)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("merge wedged behind a held cross-process lock instead of giving up on it")
	}

	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok || snap.Buckets[claudeWindowFiveHour].UsedPercentage != 12 {
		t.Error("the best-effort merge must still persist its reading on the degraded path")
	}
}

// ...but a VERIFIED merge must not. Its return value settles a post-run debt and
// feeds a signed receipt, and an unlocked read-modify-rename is not a persisted
// write: the holder we gave up on may have read the old snapshot before pausing,
// and renames its stale copy over ours the moment it resumes. Reporting success
// there would clear the failure backoff, throttle the retry that would have
// fixed it, and sign for an observation the cache no longer holds.
func TestMergeClaudeRateLimitCacheChecked_RefusesWhenTheCrossProcessLockIsHeld(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	seeded := map[string]claudeRateLimitBucket{claudeWindowFiveHour: {
		ObservedAtMs: now.UnixMilli(), ResetsAtMs: now.Add(time.Hour).UnixMilli(),
		UsedPercentage: 12, Status: "allowed", usageKnown: true,
	}}
	mergeClaudeRateLimitCacheFromSource(cache, seeded, now, "", claudeRateLimitSourceStream)
	before, err := os.ReadFile(cache)
	if err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	held, ok := acquireCrossProcessCacheLock(cache)
	if !ok {
		t.Skip("advisory locking unavailable on this filesystem")
	}
	defer func() {
		_ = unlockFile(held)
		_ = held.Close()
	}()

	prevWait := claudeRateLimitCacheLockWait
	claudeRateLimitCacheLockWait = 50 * time.Millisecond
	t.Cleanup(func() { claudeRateLimitCacheLockWait = prevWait })

	later := now.Add(time.Minute)
	type result struct {
		observed time.Time
		err      error
	}
	done := make(chan result, 1)
	go func() {
		observed, err := mergeClaudeRateLimitCacheChecked(cache, map[string]claudeRateLimitBucket{
			claudeWindowFiveHour: {
				ObservedAtMs: later.UnixMilli(), ResetsAtMs: later.Add(time.Hour).UnixMilli(),
				UsedPercentage: 44, Status: "allowed", usageKnown: true,
			},
		}, later, "", claudeRateLimitSourceProbe)
		done <- result{observed, err}
	}()

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatal("checked merge reported success while another writer held the lock")
		}
		if !got.observed.IsZero() {
			t.Errorf("observed=%s, want zero — nothing was persisted", got.observed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("checked merge wedged behind a held cross-process lock")
	}

	after, err := os.ReadFile(cache)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	if string(after) != string(before) {
		t.Error("a refused merge must leave the cache byte-identical")
	}
}

// The in-process gate has no queue bound: every rate-limit line a Claude session
// prints merges through it, and each writer may hold it for its own full
// cross-process lock wait. A verified merge therefore cannot assume it is behind
// at most one of them — it must enforce its own end-to-end ceiling, or the join
// deadline derived from that ceiling stops covering the probe it is waiting on.
func TestMergeClaudeRateLimitCacheChecked_BoundedByQueuedInProcessWriters(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	prevBudget := claudeRateLimitVerifiedPersistBudget
	claudeRateLimitVerifiedPersistBudget = 100 * time.Millisecond
	t.Cleanup(func() { claudeRateLimitVerifiedPersistBudget = prevBudget })

	// Stand in for the queued stream-capture writers by holding the gate itself:
	// what the probe sees is the same either way, and this pins the wait instead
	// of racing a real writer.
	lockClaudeRateLimitCache()
	released := false
	defer func() {
		if !released {
			unlockClaudeRateLimitCache()
		}
	}()

	done := make(chan error, 1)
	started := time.Now()
	go func() {
		_, err := mergeClaudeRateLimitCacheChecked(cache, map[string]claudeRateLimitBucket{
			claudeWindowFiveHour: {
				ObservedAtMs: now.UnixMilli(), ResetsAtMs: now.Add(time.Hour).UnixMilli(),
				UsedPercentage: 44, Status: "allowed", usageKnown: true,
			},
		}, now, "", claudeRateLimitSourceProbe)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("checked merge reported success without ever holding the cache gate")
		}
		if waited := time.Since(started); waited > 5*time.Second {
			t.Errorf("waited %s, want the merge to give up near its %s budget",
				waited, claudeRateLimitVerifiedPersistBudget)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("checked merge queued behind the gate with no deadline of its own")
	}

	if _, ok := loadClaudeRateLimitSnapshot(cache); ok {
		t.Error("a merge that never held the gate must not have written the cache")
	}

	// Once the gate frees up the same merge lands, so the refusal above is a
	// bounded wait and not a permanently closed door.
	unlockClaudeRateLimitCache()
	released = true
	if _, err := mergeClaudeRateLimitCacheChecked(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			ObservedAtMs: now.UnixMilli(), ResetsAtMs: now.Add(time.Hour).UnixMilli(),
			UsedPercentage: 44, Status: "allowed", usageKnown: true,
		},
	}, now, "", claudeRateLimitSourceProbe); err != nil {
		t.Fatalf("merge after the gate was released: %v", err)
	}
}

// The checked merge answers two different questions, and the second one is what
// a post-run debt-holder needs: "did the write land" is not "what observation
// does the cache now hold for these windows". A writer whose stamp is refused by
// the newer-wins guard succeeds having changed nothing it can claim credit for,
// so it must be told the incumbent's instant, not its own.
func TestMergeClaudeRateLimitCacheChecked_ReportsTheObservationTheCacheHolds(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	newer := now.Add(30 * time.Second)

	bucket := func(at time.Time, pct float64) map[string]claudeRateLimitBucket {
		return map[string]claudeRateLimitBucket{
			claudeWindowFiveHour: {
				ObservedAtMs: at.UnixMilli(), ResetsAtMs: now.Add(time.Hour).UnixMilli(),
				UsedPercentage: pct, Status: "allowed", usageKnown: true,
			},
		}
	}

	// A reading that lands reports its own stamp.
	observed, err := mergeClaudeRateLimitCacheChecked(cache, bucket(newer, 40), newer, "",
		claudeRateLimitSourceStatusLine)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !observed.Equal(newer) {
		t.Errorf("observed=%s, want the reading it wrote at %s", observed, newer)
	}

	// One that is refused as older reports the incumbent it lost to, so its
	// caller cannot mistake a successful write for a fresh observation.
	observed, err = mergeClaudeRateLimitCacheChecked(cache, bucket(now, 61), now, "",
		claudeRateLimitSourceProbe)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !observed.Equal(newer) {
		t.Errorf("observed=%s, want the newer incumbent %s that the merge kept", observed, newer)
	}
	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok || snap.Buckets[claudeWindowFiveHour].UsedPercentage != 40 {
		t.Error("the newer reading must survive the older merge")
	}
}

// The verified merge's budget has to cover the FILESYSTEM work, not just the two
// lock waits it starts with. Go cannot cancel a syscall: on a wedged mount an
// unreachable share, a hung network drive, a disk that stopped answering the
// write blocks for as long as the kernel takes, observing neither context nor
// deadline. That matters here because the join deadline a signed refresh waits
// on is DERIVED from this budget, so a merge that outlives it strands the
// refresh handler and the probe's single-flight slot behind a filesystem that
// may never answer.
func TestMergeClaudeRateLimitCacheChecked_BoundedByStalledFilesystem(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	prevBudget := claudeRateLimitVerifiedPersistBudget
	claudeRateLimitVerifiedPersistBudget = 100 * time.Millisecond
	t.Cleanup(func() { claudeRateLimitVerifiedPersistBudget = prevBudget })

	// Stall the snapshot write the way an unreachable mount does: enter, then
	// block indefinitely with no deadline of its own.
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unstall := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unstall)

	prevWrite := claudeRateLimitCacheWriteFile
	t.Cleanup(func() { claudeRateLimitCacheWriteFile = prevWrite })
	claudeRateLimitCacheWriteFile = func(name string, data []byte, perm os.FileMode) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return prevWrite(name, data, perm)
	}

	updates := map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			ObservedAtMs: now.UnixMilli(), ResetsAtMs: now.Add(time.Hour).UnixMilli(),
			UsedPercentage: 44, Status: "allowed", usageKnown: true,
		},
	}

	done := make(chan error, 1)
	started := time.Now()
	go func() {
		_, err := mergeClaudeRateLimitCacheChecked(cache, updates, now, "", claudeRateLimitSourceProbe)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("checked merge reported success while its write was still stalled")
		}
		if waited := time.Since(started); waited > 5*time.Second {
			t.Errorf("waited %s, want the merge to give up near its %s budget",
				waited, claudeRateLimitVerifiedPersistBudget)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("checked merge blocked in a filesystem call with no deadline of its own")
	}

	select {
	case <-entered:
	default:
		t.Fatal("the merge never reached the stalled write, so this test proved nothing")
	}
	if _, ok := loadClaudeRateLimitSnapshot(cache); ok {
		t.Error("a merge abandoned mid-write must not have published a snapshot yet")
	}

	// Abandoning the write is safe rather than lossy: the caller reported a
	// failure and signed nothing, and the write itself completes once the
	// filesystem answers, leaving the reading for the next gather. Waiting on the
	// gate is also the sync point that proves the abandoned merge released both
	// locks instead of stranding every later writer behind it.
	unstall()
	if !lockClaudeRateLimitCacheUntil(time.Now().Add(10 * time.Second)) {
		t.Fatal("the abandoned merge never released the cache gate")
	}
	unlockClaudeRateLimitCache()
	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok || snap.Buckets[claudeWindowFiveHour].UsedPercentage != 44 {
		t.Error("the abandoned write must still land once the filesystem recovers")
	}
}
