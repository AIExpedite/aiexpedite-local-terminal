package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCaptureCodexRateLimit_TokenCountNotification(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	primaryResetSec := 3600.0   // 1h
	secondaryResetSec := 604800 // 7d
	line := `{"jsonrpc":"2.0","method":"token_count","params":{"msg":{"rate_limits":{` +
		`"primary":{"used_percent":42.5,"resets_in_seconds":3600},` +
		`"secondary":{"utilization":0.18,"resets_in_seconds":604800}` +
		`}}}}`

	captureCodexRateLimitLine(line, now)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache to be written")
	}
	p, ok := snap.Buckets[codexWindowPrimary]
	if !ok {
		t.Fatalf("expected primary bucket, got %+v", snap.Buckets)
	}
	if p.UsedPercentage != 42.5 {
		t.Errorf("primary UsedPercentage=%v, want 42.5", p.UsedPercentage)
	}
	wantPrimaryReset := now.Add(time.Duration(primaryResetSec * float64(time.Second))).UnixMilli()
	if p.ResetsAtMs != wantPrimaryReset {
		t.Errorf("primary ResetsAtMs=%d, want %d", p.ResetsAtMs, wantPrimaryReset)
	}

	s := snap.Buckets[codexWindowSecondary]
	if s.UsedPercentage < 17.9 || s.UsedPercentage > 18.1 {
		t.Errorf("secondary UsedPercentage=%v, want ~18 (utilization 0.18 -> %%)", s.UsedPercentage)
	}
	wantSecondaryReset := now.Add(time.Duration(secondaryResetSec) * time.Second).UnixMilli()
	if s.ResetsAtMs != wantSecondaryReset {
		t.Errorf("secondary ResetsAtMs=%d, want %d", s.ResetsAtMs, wantSecondaryReset)
	}
}

// Codex's exact wire keys are tolerated, not assumed: the design accepts
// `5h`/`7d` window aliases and a `window_minutes` reset hint so a schema
// rename doesn't silently zero the card. Cover both at once.
func TestCaptureCodexRateLimit_AcceptsAliasesAndWindowMinutes(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	line := `{"method":"token_count","params":{"rate_limits":{` +
		`"5h":{"used_percent":12,"window_minutes":300},` +
		`"7d":{"used_percent":50,"window_minutes":10080}` +
		`}}}`

	captureCodexRateLimitLine(line, now)
	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache write under alias keys")
	}
	p, ok := snap.Buckets[codexWindowPrimary]
	if !ok || p.UsedPercentage != 12 {
		t.Fatalf("5h alias did not normalise to primary bucket: %+v", snap.Buckets)
	}
	wantPrimaryReset := now.Add(300 * time.Minute).UnixMilli()
	if p.ResetsAtMs != wantPrimaryReset {
		t.Errorf("primary ResetsAtMs from window_minutes=%d, want %d", p.ResetsAtMs, wantPrimaryReset)
	}
	s, ok := snap.Buckets[codexWindowSecondary]
	if !ok || s.UsedPercentage != 50 {
		t.Fatalf("7d alias did not normalise to secondary bucket: %+v", snap.Buckets)
	}
}

// Codex's newer surface is `account/rateLimits/read` (response under
// `result`) and `account/rateLimits/updated` (notification under `params`),
// both with camelCase `rateLimits`. The prefilter must accept that token and
// extraction must drain both `result` and `params`.
func TestCaptureCodexRateLimit_AccountRateLimitsReadResult(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	line := `{"jsonrpc":"2.0","id":7,"result":{"rateLimits":{` +
		`"primary":{"usedPercent":22,"resetsInSeconds":1800}` +
		`}}}`
	captureCodexRateLimitLine(line, now)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache write for account/rateLimits/read result")
	}
	p, ok := snap.Buckets[codexWindowPrimary]
	if !ok || p.UsedPercentage != 22 {
		t.Fatalf("primary not extracted from result.rateLimits: %+v", snap.Buckets)
	}
	if p.ResetsAtMs != now.Add(1800*time.Second).UnixMilli() {
		t.Errorf("ResetsAtMs=%d, want resetsInSeconds offset", p.ResetsAtMs)
	}
}

// account/rateLimits/updated notifications are documented as sparse. A
// reset-only follow-up must NOT zero out the prior used_percent, and a
// usage-only follow-up must NOT drop the prior reset time — otherwise the
// card oscillates between real numbers and 0%/Unknown between turns.
func TestCaptureCodexRateLimit_SparseUpdatesPreservePriorFields(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	// Initial full snapshot: 60% used, resets in 1h.
	captureCodexRateLimitLine(
		`{"method":"token_count","params":{"rate_limits":{"primary":{"used_percent":60,"resets_in_seconds":3600}}}}`,
		now,
	)

	// Sparse update #1: reset-only (used_percent omitted). Must preserve 60%.
	captureCodexRateLimitLine(
		`{"method":"account/rateLimits/updated","params":{"rateLimits":{"primary":{"resetsInSeconds":1500}}}}`,
		now,
	)
	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache")
	}
	if got := snap.Buckets[codexWindowPrimary].UsedPercentage; got != 60 {
		t.Errorf("after reset-only update UsedPercentage=%v, want 60 (preserved)", got)
	}
	if got := snap.Buckets[codexWindowPrimary].ResetsAtMs; got != now.Add(1500*time.Second).UnixMilli() {
		t.Errorf("after reset-only update ResetsAtMs=%d not refreshed", got)
	}

	// Sparse update #2: usage-only (no reset). Must preserve the live reset.
	captureCodexRateLimitLine(
		`{"method":"account/rateLimits/updated","params":{"rateLimits":{"primary":{"usedPercent":72}}}}`,
		now,
	)
	snap, _ = loadCodexRateLimitSnapshot(cache)
	if got := snap.Buckets[codexWindowPrimary].UsedPercentage; got != 72 {
		t.Errorf("after usage-only update UsedPercentage=%v, want 72", got)
	}
	if got := snap.Buckets[codexWindowPrimary].ResetsAtMs; got != now.Add(1500*time.Second).UnixMilli() {
		t.Errorf("after usage-only update ResetsAtMs=%d, want previous live reset preserved", got)
	}
}

// A reset-only update with no prior live bucket must NOT persist a zero-value
// UsedPercentage, otherwise the card would render an observed 0% used / 100%
// remaining even though no usage was ever reported. The metric should stay
// Unknown until a real used_percent/utilization is observed.
func TestCaptureCodexRateLimit_ResetOnlyWithoutPriorStaysUnknown(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	captureCodexRateLimitLine(
		`{"method":"account/rateLimits/updated","params":{"rateLimits":{"primary":{"resetsInSeconds":1500}}}}`,
		now,
	)

	if snap, ok := loadCodexRateLimitSnapshot(cache); ok {
		if _, present := snap.Buckets[codexWindowPrimary]; present {
			t.Fatalf("reset-only update with no prior bucket should not seed a 0%% bucket: %+v", snap.Buckets)
		}
	}

	metrics := codexMetricsFromCache(now, "")
	if !metrics[0].Unknown {
		t.Errorf("session metric should remain Unknown after reset-only update with no prior usage, got %+v", metrics[0])
	}
}

func TestCaptureCodexRateLimit_IgnoresUnrelatedFrames(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	// No token_count / rate_limit substring → cheap prefilter skips.
	captureCodexRateLimitLine(`{"jsonrpc":"2.0","method":"agent_message","params":{"text":"hi"}}`, now)
	// Malformed JSON → silent.
	captureCodexRateLimitLine(`{not json`, now)
	// Has the trigger word but no rate_limits map → no cache write.
	captureCodexRateLimitLine(`{"method":"token_count","params":{"msg":{"input_tokens":5}}}`, now)

	if _, ok := loadCodexRateLimitSnapshot(cache); ok {
		t.Fatalf("cache must not be written for unrelated frames")
	}
}

func TestCaptureCodexRateLimit_DropsStaleSnapshotOnAccountSwitch(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary: {UsedPercentage: 75, ResetsAtMs: now.Add(time.Hour).UnixMilli(), usageKnown: true, resetKnown: true},
	}, now, "fingerprint-A")

	// Different account writes only secondary — primary from A must be dropped.
	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowSecondary: {UsedPercentage: 10, ResetsAtMs: now.Add(48 * time.Hour).UnixMilli(), usageKnown: true, resetKnown: true},
	}, now, "fingerprint-B")

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache")
	}
	if snap.AccountFingerprint != "fingerprint-B" {
		t.Errorf("fingerprint=%q, want fingerprint-B", snap.AccountFingerprint)
	}
	if _, present := snap.Buckets[codexWindowPrimary]; present {
		t.Errorf("primary bucket from account A must be dropped on account switch")
	}
	if _, present := snap.Buckets[codexWindowSecondary]; !present {
		t.Errorf("secondary bucket from account B missing")
	}
}

func TestCodexMetricsFromCache_ObservedAndPastReset(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary:   {UsedPercentage: 31.2, ResetsAtMs: now.Add(2 * time.Hour).UnixMilli(), usageKnown: true, resetKnown: true},
		codexWindowSecondary: {UsedPercentage: 95, ResetsAtMs: now.Add(-time.Minute).UnixMilli(), usageKnown: true, resetKnown: true},
	}, now, "")

	metrics := codexMetricsFromCache(now, "")
	if len(metrics) != 2 {
		t.Fatalf("want 2 metrics, got %d", len(metrics))
	}
	session, weekly := metrics[0], metrics[1]
	if session.Kind != limitKindSession || session.Unknown {
		t.Errorf("session metric=%+v, want observed limitKindSession", session)
	}
	if session.Consumed == nil || *session.Consumed != 31.2 {
		t.Errorf("session Consumed=%v, want 31.2", session.Consumed)
	}
	if session.ResetAt == "" {
		t.Errorf("live session window should advertise a ResetAt")
	}
	if weekly.Kind != limitKindWeekly {
		t.Errorf("weekly metric kind=%q, want %q", weekly.Kind, limitKindWeekly)
	}
	if weekly.Consumed == nil || *weekly.Consumed != 0 {
		t.Errorf("past-reset weekly Consumed=%v, want 0 (rolled over)", weekly.Consumed)
	}
	if weekly.ResetAt != "" {
		t.Errorf("past-reset window must not advertise a stale ResetAt")
	}
}

func TestCodexMetricsFromCache_NoCacheFallsBackToUnknown(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "absent.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)

	metrics := codexMetricsFromCache(time.Now(), "")
	if len(metrics) != 2 {
		t.Fatalf("want 2 metrics, got %d", len(metrics))
	}
	for _, m := range metrics {
		if !m.Unknown {
			t.Errorf("metric %q should be Unknown without a cache", m.Kind)
		}
	}
}

// When Codex reports a non-canonical window length (e.g. 15-minute primary in
// the documented account/rateLimits/read example, or a future plan), the metric
// label must reflect the actual duration rather than the hard-coded
// "5-hour session window" / "Weekly quota" strings.
func TestCodexMetricsFromCache_LabelDerivedFromWindowMinutes(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary: {
			UsedPercentage: 40, ResetsAtMs: now.Add(15 * time.Minute).UnixMilli(),
			WindowMinutes: 15, usageKnown: true, resetKnown: true,
		},
		codexWindowSecondary: {
			UsedPercentage: 10, ResetsAtMs: now.Add(24 * time.Hour).UnixMilli(),
			WindowMinutes: 1440, usageKnown: true, resetKnown: true,
		},
	}, now, "")

	metrics := codexMetricsFromCache(now, "")
	if got := metrics[0].Label; got != "15-minute window" {
		t.Errorf("primary label=%q, want %q", got, "15-minute window")
	}
	if got := metrics[1].Label; got != "1-day window" {
		t.Errorf("secondary label=%q, want %q", got, "1-day window")
	}
}

// Canonical Codex windows (primary = 300 min, secondary = 10080 min) must keep
// their long-standing labels so the card text doesn't churn for existing users
// once duration-derived labels land.
func TestCodexMetricsFromCache_CanonicalWindowsKeepLabels(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary: {
			UsedPercentage: 20, ResetsAtMs: now.Add(5 * time.Hour).UnixMilli(),
			WindowMinutes: 300, usageKnown: true, resetKnown: true,
		},
		codexWindowSecondary: {
			UsedPercentage: 5, ResetsAtMs: now.Add(7 * 24 * time.Hour).UnixMilli(),
			WindowMinutes: 10080, usageKnown: true, resetKnown: true,
		},
	}, now, "")

	metrics := codexMetricsFromCache(now, "")
	if got := metrics[0].Label; got != "5-hour session window" {
		t.Errorf("primary label=%q, want canonical 5-hour session window", got)
	}
	if got := metrics[1].Label; got != "Weekly quota" {
		t.Errorf("secondary label=%q, want canonical Weekly quota", got)
	}
}

func TestCodexMetricsFromCache_FingerprintMismatchHidesSnapshot(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary: {UsedPercentage: 75, ResetsAtMs: now.Add(time.Hour).UnixMilli(), usageKnown: true, resetKnown: true},
	}, now, "fingerprint-A")

	metrics := codexMetricsFromCache(now, "fingerprint-B")
	for _, m := range metrics {
		if !m.Unknown {
			t.Errorf("metric %q should be Unknown when cache fingerprint mismatches", m.Kind)
		}
	}
}
