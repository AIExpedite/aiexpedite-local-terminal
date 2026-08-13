package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// stubClaudeProbes isolates a Claude parser test from the dev machine: the
// macOS Keychain never answers, and the `claude auth status` probe reports the
// given definite verdict instead of shelling out.
func stubClaudeProbes(t *testing.T, loggedIn, known bool) {
	t.Helper()
	originalKeychain := claudeKeychainReader
	originalProbe := claudeAuthStatusProbe
	t.Cleanup(func() {
		claudeKeychainReader = originalKeychain
		claudeAuthStatusProbe = originalProbe
	})
	claudeKeychainReader = func() ([]byte, bool) { return nil, false }
	claudeAuthStatusProbe = func(string) (bool, bool) { return loggedIn, known }
}

// seedClaudeRateLimitCache points the parser at an isolated cache holding the
// given buckets, scoped to the given account fingerprint.
func seedClaudeRateLimitCache(
	t *testing.T,
	buckets map[string]claudeRateLimitBucket,
	now time.Time,
	fingerprint string,
) {
	t.Helper()
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	if len(buckets) > 0 {
		mergeClaudeRateLimitCache(cache, buckets, now, fingerprint)
	}
}

// The real claude.ai login writes NO top-level `plan`/`subscription` key — the
// plan is `claudeAiOauth.subscriptionType`, and the email lives in
// ~/.claude.json rather than the credential. This is the shape the card's plan
// chip must come from, and the one the FullCredentials fixture cannot cover
// (it keeps a credential account so it can assert a fingerprint).
func TestClaudeCodeUsageParser_PlanFromOAuthSubscriptionType(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	seedClaudeRateLimitCache(t, nil, time.Time{}, "")
	stubClaudeProbes(t, true, true)

	home := t.TempDir()
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	helperWriteJSON(t, filepath.Join(home, ".claude", ".credentials.json"), map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":           "sk-ant-oat-access",
			"refreshToken":          "sk-ant-ort-refresh",
			"expiresAt":             now.Add(8 * time.Hour).UnixMilli(),
			"refreshTokenExpiresAt": now.Add(30 * 24 * time.Hour).UnixMilli(),
			"subscriptionType":      "max",
		},
	})
	helperWriteJSON(t, filepath.Join(home, ".claude.json"), map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "ada@example.com"},
	})

	usage, ok := claudeCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !ok || usage == nil {
		t.Fatalf("expected usage, got ok=%v", ok)
	}
	// Published verbatim: the card capitalizes it for display ("Max"), so
	// title-casing here would be the wrong layer.
	if usage.Plan != "max" {
		t.Errorf("Plan=%q, want max (from claudeAiOauth.subscriptionType, lowercase)", usage.Plan)
	}
	if usage.Account != "ada@example.com" {
		t.Errorf("Account=%q, want ada@example.com (display label from ~/.claude.json)", usage.Account)
	}
	// Reading the plan must not change cache scoping: the credential carries no
	// account, so the fingerprint stays empty and the metrics stay device-scoped.
	if usage.AccountFingerprint != "" {
		t.Errorf("AccountFingerprint=%q, want empty (dotfile email is never fingerprinted)", usage.AccountFingerprint)
	}
}

// An API-key install has no claudeAiOauth block at all: no plan is reported (so
// no chip), and — critically — the account identity used for fingerprinting and
// cache scoping is unchanged by the plan read.
func TestClaudeCodeUsageParser_NoOAuthBlockReportsNoPlan(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	seedClaudeRateLimitCache(t, nil, time.Time{}, "")
	stubClaudeProbes(t, true, true)

	home := t.TempDir()
	helperWriteJSON(t, filepath.Join(home, ".claude", ".credentials.json"), map[string]any{
		"email": "ada@example.com",
	})

	usage, ok := claudeCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, time.Now())
	if !ok || usage == nil {
		t.Fatalf("expected usage, got ok=%v", ok)
	}
	if usage.Plan != "" {
		t.Errorf("Plan=%q, want empty (no claudeAiOauth block)", usage.Plan)
	}
	if usage.Account != "ada@example.com" {
		t.Errorf("Account=%q, want ada@example.com", usage.Account)
	}
	if want := fingerprintAccount("claudeCode", "ada@example.com"); usage.AccountFingerprint != want {
		t.Errorf("AccountFingerprint=%q, want %q (unchanged by the plan read)", usage.AccountFingerprint, want)
	}
}

func TestClaudeCodeMetricsFromCache_FableWindowObserved(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // isolate from the real ~/.claude hook
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	reset := now.Add(120 * time.Hour).UnixMilli()
	seedClaudeRateLimitCache(t, map[string]claudeRateLimitBucket{
		claudeWindowSevenDayFable: {
			UsedPercentage: 22, ResetsAtMs: reset, ObservedAtMs: now.UnixMilli(),
			Status: "allowed", usageKnown: true,
		},
	}, now, "")

	fable := claudeCodeMetricsFromCache(now, "")[2]
	if fable.Kind != limitKindWeekly || fable.Label != "Weekly Fable" {
		t.Fatalf("fable row=(%q,%q), want (weekly, Weekly Fable)", fable.Kind, fable.Label)
	}
	if fable.Unknown {
		t.Fatalf("fable row should be observed, got %+v", fable)
	}
	if fable.Consumed == nil || *fable.Consumed != 22 {
		t.Errorf("fable Consumed=%v, want 22", fable.Consumed)
	}
	if fable.Remaining == nil || *fable.Remaining != 78 {
		t.Errorf("fable Remaining=%v, want 78", fable.Remaining)
	}
	if want := time.UnixMilli(reset).UTC().Format(time.RFC3339); fable.ResetAt != want {
		t.Errorf("fable ResetAt=%q, want %q", fable.ResetAt, want)
	}
	if want := observedAtRFC3339(now.UnixMilli()); fable.ObservedAt != want {
		t.Errorf("fable ObservedAt=%q, want %q", fable.ObservedAt, want)
	}
}

// A window whose reset has already passed is unobservable rather than replayed
// as a stale high-water mark — same rule the 5-hour and weekly rows follow.
func TestClaudeCodeMetricsFromCache_FableRolledOverWindowIsUnknown(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	observed := now.Add(-3 * time.Hour)
	seedClaudeRateLimitCache(t, map[string]claudeRateLimitBucket{
		claudeWindowSevenDayFable: {
			UsedPercentage: 88, ResetsAtMs: now.Add(-time.Hour).UnixMilli(),
			ObservedAtMs: observed.UnixMilli(), Status: "allowed", usageKnown: true,
		},
	}, now, "")

	fable := claudeCodeMetricsFromCache(now, "")[2]
	if !fable.Unknown || fable.Consumed != nil || fable.ResetAt != "" {
		t.Errorf("rolled-over fable row=%+v, want unobservable with no stale reset", fable)
	}
	if want := observedAtRFC3339(observed.UnixMilli()); fable.ObservedAt != want {
		t.Errorf("fable ObservedAt=%q, want %q (observation time must survive)", fable.ObservedAt, want)
	}
}

// Both upstream percentage shapes must land at 100 for the Fable window, the
// 0..1 `utilization` one through the key-agnostic capture path and the 0..100
// one through the read-side clamp.
func TestClaudeCodeMetricsFromCache_FablePercentClamping(t *testing.T) {
	t.Run("utilization 0..1 captured from a status-line payload", func(t *testing.T) {
		cache := filepath.Join(t.TempDir(), "rl.json")
		t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

		now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
		reset := now.Add(96 * time.Hour)
		captureClaudeRateLimitLine(
			`{"rate_limits":{"seven_day_fable":{"status":"allowed","utilization":1.0,"resets_at":`+
				itoa(reset.Unix())+`}}}`, now)

		fable := claudeCodeMetricsFromCache(now, "")[2]
		if fable.Consumed == nil || *fable.Consumed != 100 {
			t.Errorf("fable Consumed=%v, want 100 (utilization 1.0)", fable.Consumed)
		}
	})

	t.Run("used_percentage above 100", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
		now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
		seedClaudeRateLimitCache(t, map[string]claudeRateLimitBucket{
			claudeWindowSevenDayFable: {
				UsedPercentage: 137, ResetsAtMs: now.Add(96 * time.Hour).UnixMilli(),
				ObservedAtMs: now.UnixMilli(), usageKnown: true,
			},
		}, now, "")

		fable := claudeCodeMetricsFromCache(now, "")[2]
		if fable.Consumed == nil || *fable.Consumed != 100 {
			t.Errorf("fable Consumed=%v, want 100 (clamped)", fable.Consumed)
		}
		if fable.Remaining == nil || *fable.Remaining != 0 {
			t.Errorf("fable Remaining=%v, want 0", fable.Remaining)
		}
	})
}

// The two weekly rows are independent: a Fable-only cache must not make the
// "Weekly quota" row report Fable's numbers, and vice versa.
func TestClaudeCodeMetricsFromCache_FableOnlyCacheLeavesWeeklyQuotaUnknown(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	seedClaudeRateLimitCache(t, map[string]claudeRateLimitBucket{
		claudeWindowSevenDayFable: {
			UsedPercentage: 57, ResetsAtMs: now.Add(120 * time.Hour).UnixMilli(),
			ObservedAtMs: now.UnixMilli(), usageKnown: true,
		},
	}, now, "")

	metrics := claudeCodeMetricsFromCache(now, "")
	if weekly := metrics[1]; !weekly.Unknown || weekly.Consumed != nil {
		t.Errorf("weekly quota=%+v, want Unknown (no seven_day* bucket observed)", weekly)
	}
	if fable := metrics[2]; fable.Consumed == nil || *fable.Consumed != 57 {
		t.Errorf("fable Consumed=%v, want 57", fable.Consumed)
	}
}

// The canonical `seven_day_fable` key is an extrapolation from the keys Claude
// Code is known to emit, so an unexpected fable-ish key must still surface
// rather than silently dropping the row on every shipped agent.
func TestClaudeCodeMetricsFromCache_TolerantFableKeyMatching(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	seedClaudeRateLimitCache(t, map[string]claudeRateLimitBucket{
		"seven_day_fable_5": {
			UsedPercentage: 41, ResetsAtMs: now.Add(72 * time.Hour).UnixMilli(),
			ObservedAtMs: now.UnixMilli(), usageKnown: true,
		},
	}, now, "")

	fable := claudeCodeMetricsFromCache(now, "")[2]
	if fable.Unknown || fable.Consumed == nil || *fable.Consumed != 41 {
		t.Errorf("fable row=%+v, want the variant key's 41%%", fable)
	}
}

// With both the canonical key and variants cached, the canonical bucket must
// win (plain sorting would not — "fable_weekly" sorts ahead of
// "seven_day_fable"), and the result must be byte-identical across calls:
// terminal-service delta-skips writes by hashing the marshalled payload, so a
// row flipping between buckets would churn a Firestore write and a 7-day
// history document on every poll.
func TestClaudeCodeMetricsFromCache_CanonicalFableWinsDeterministically(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	reset := now.Add(120 * time.Hour).UnixMilli()
	seedClaudeRateLimitCache(t, map[string]claudeRateLimitBucket{
		claudeWindowSevenDayFable: {UsedPercentage: 22, ResetsAtMs: reset, ObservedAtMs: now.UnixMilli(), usageKnown: true},
		"fable_weekly":            {UsedPercentage: 71, ResetsAtMs: reset, ObservedAtMs: now.UnixMilli(), usageKnown: true},
		"weekly_fable":            {UsedPercentage: 93, ResetsAtMs: reset, ObservedAtMs: now.UnixMilli(), usageKnown: true},
	}, now, "")

	first := claudeCodeMetricsFromCache(now, "")
	if fable := first[2]; fable.Consumed == nil || *fable.Consumed != 22 {
		t.Fatalf("fable Consumed=%v, want 22 (canonical seven_day_fable bucket)", fable.Consumed)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal metrics: %v", err)
	}
	for i := 0; i < 5; i++ {
		nextJSON, err := json.Marshal(claudeCodeMetricsFromCache(now, ""))
		if err != nil {
			t.Fatalf("marshal metrics: %v", err)
		}
		if string(nextJSON) != string(firstJSON) {
			t.Fatalf("metrics differ across calls:\n%s\n%s", firstJSON, nextJSON)
		}
	}
}

// Claude already splits the weekly window per model, so a symmetric
// `five_hour_fable` is plausible. Surfacing it under a row labelled "Weekly
// Fable" would report a session window as a weekly one.
func TestClaudeCodeMetricsFromCache_FiveHourFableIsNotTheWeeklyFableRow(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	seedClaudeRateLimitCache(t, map[string]claudeRateLimitBucket{
		"five_hour_fable": {
			UsedPercentage: 64, ResetsAtMs: now.Add(2 * time.Hour).UnixMilli(),
			ObservedAtMs: now.UnixMilli(), usageKnown: true,
		},
	}, now, "")

	fable := claudeCodeMetricsFromCache(now, "")[2]
	if !fable.Unknown || fable.Consumed != nil {
		t.Errorf("Weekly Fable row=%+v, want Unknown (a five_hour_* bucket is not a weekly window)", fable)
	}
}

// A snapshot captured under another account must not surface on the Fable row
// any more than on the other two — a previous user's quota is never shown.
func TestClaudeCodeMetricsFromCache_IgnoresOtherAccountFableSnapshot(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	seedClaudeRateLimitCache(t, map[string]claudeRateLimitBucket{
		claudeWindowSevenDayFable: {
			UsedPercentage: 77, ResetsAtMs: now.Add(120 * time.Hour).UnixMilli(),
			ObservedAtMs: now.UnixMilli(), usageKnown: true,
		},
	}, now, "fp-previous-account")

	for _, m := range claudeCodeMetricsFromCache(now, "fp-current-account") {
		if !m.Unknown {
			t.Errorf("metric %q must be Unknown when the snapshot belongs to another account", m.Label)
		}
	}
}

// An error-severity notice (logged out) blanks every row's numbers while
// preserving its identity — the Fable row included, so the card keeps its
// stable three-row layout instead of losing a label.
func TestClaudeCodeUsageParser_ErrorNoticeBlanksFableRowKeepingLabel(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	seedClaudeRateLimitCache(t, map[string]claudeRateLimitBucket{
		claudeWindowSevenDayFable: {
			UsedPercentage: 22, ResetsAtMs: now.Add(120 * time.Hour).UnixMilli(),
			ObservedAtMs: now.UnixMilli(), usageKnown: true,
		},
	}, now, "")
	stubClaudeProbes(t, false, true) // definite logged-out verdict

	usage, ok := claudeCodeUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{
		Detected: true, Path: "claude-test",
	}, now)
	if !ok || usage == nil {
		t.Fatalf("expected usage, got ok=%v", ok)
	}
	if len(usage.Metrics) != 3 {
		t.Fatalf("want 3 metrics, got %d", len(usage.Metrics))
	}
	fable := usage.Metrics[2]
	if fable.Label != "Weekly Fable" || fable.Kind != limitKindWeekly {
		t.Errorf("fable row=(%q,%q), want (weekly, Weekly Fable)", fable.Kind, fable.Label)
	}
	if !fable.Unknown || fable.Consumed != nil || fable.Remaining != nil || fable.Total != nil {
		t.Errorf("fable row=%+v, want unobservable for an unusable login", fable)
	}
}
