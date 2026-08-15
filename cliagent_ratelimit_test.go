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

	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if ok {
		if _, present := snap.Buckets[claudeWindowFiveHour]; present {
			t.Fatalf("first heartbeat without usage must not persist a zero-usage bucket")
		}
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
	if session.ResetAt != "" {
		t.Errorf("rolled-over window must not advertise a fresh reset, got %q", session.ResetAt)
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
