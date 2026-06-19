package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func helperWriteJSON(t *testing.T, path string, payload any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func helperJWT(t *testing.T, claims any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal jwt claims: %v", err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

func TestClaudeCodeUsageParser_FullCredentials(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	// Isolate from any real rate-limit cache on the dev machine so this asserts
	// the no-telemetry (Unknown) fallback deterministically.
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))
	home := t.TempDir()
	helperWriteJSON(t, filepath.Join(home, ".claude", ".credentials.json"), map[string]any{
		"email": "ada@example.com",
		"plan":  "max",
	})

	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	usage, ok := claudeCodeUsageParser{}.Parse(home, detectedCLIAgent{
		Detected: true,
		Version:  "2.1.0",
		Path:     "/opt/homebrew/bin/claude",
		Name:     "Claude Code",
	}, now)
	if !ok || usage == nil {
		t.Fatalf("expected usage, got ok=%v", ok)
	}
	if usage.Account != "ada@example.com" {
		t.Errorf("Account=%q, want ada@example.com", usage.Account)
	}
	if usage.Plan != "max" {
		t.Errorf("Plan=%q, want max", usage.Plan)
	}
	if usage.AccountFingerprint == "" {
		t.Errorf("expected fingerprint for known account")
	}
	if len(usage.Metrics) != 2 {
		t.Fatalf("expected 2 metrics, got %d", len(usage.Metrics))
	}
	for _, m := range usage.Metrics {
		if !m.Unknown {
			t.Errorf("metric %q should be Unknown (no observable counter)", m.Kind)
		}
	}
}

func TestClaudeCodeUsageParser_HonorsClaudeConfigDir(t *testing.T) {
	home := t.TempDir()
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	helperWriteJSON(t, filepath.Join(configDir, ".credentials.json"), map[string]any{
		"email": "profile@example.com",
		"plan":  "team",
	})

	usage, ok := claudeCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, time.Now())
	if !ok || usage == nil {
		t.Fatalf("expected usage from CLAUDE_CONFIG_DIR")
	}
	if usage.Account != "profile@example.com" {
		t.Errorf("Account=%q, want profile@example.com", usage.Account)
	}
	if usage.Plan != "team" {
		t.Errorf("Plan=%q, want team", usage.Plan)
	}
}

func TestClaudeCodeUsageParser_MissingCredentials(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home := t.TempDir()
	usage, ok := claudeCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, time.Now())
	if !ok || usage == nil {
		t.Fatalf("parser must return a baseline entry even without credentials")
	}
	if usage.AccountFingerprint != "" {
		t.Errorf("fingerprint should be empty when account is unknown")
	}
}

func TestCodexUsageParser_IdentityFromAuth(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))
	home := t.TempDir()
	helperWriteJSON(t, filepath.Join(home, ".codex", "auth.json"), map[string]any{
		"email": "carol@example.com",
		"plan":  "pro",
	})
	usage, ok := codexUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, time.Now())
	if !ok {
		t.Fatalf("expected usage")
	}
	if usage.Account != "carol@example.com" {
		t.Errorf("Account=%q, want carol@example.com", usage.Account)
	}
	if usage.Plan != "pro" {
		t.Errorf("Plan=%q, want pro", usage.Plan)
	}
	if len(usage.Metrics) != 2 {
		t.Fatalf("expected 2 metrics (session + weekly), got %d", len(usage.Metrics))
	}
	for _, m := range usage.Metrics {
		if !m.Unknown {
			t.Errorf("metric %q should be Unknown without an observed app-server frame", m.Kind)
		}
	}
}

func TestCodexUsageParser_PopulatesObservedMetricsFromCache(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	home := t.TempDir()
	helperWriteJSON(t, filepath.Join(home, ".codex", "auth.json"), map[string]any{
		"email": "carol@example.com",
		"plan":  "pro",
	})

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	fp := fingerprintAccount("codex", "carol@example.com")
	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary:   {UsedPercentage: 42, ResetsAtMs: now.Add(time.Hour).UnixMilli(), usageKnown: true, resetKnown: true},
		codexWindowSecondary: {UsedPercentage: 13, ResetsAtMs: now.Add(72 * time.Hour).UnixMilli(), usageKnown: true, resetKnown: true},
	}, nil, now, fp)

	usage, ok := codexUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("expected usage")
	}
	if len(usage.Metrics) != 2 {
		t.Fatalf("expected 2 metrics, got %d", len(usage.Metrics))
	}
	session, weekly := usage.Metrics[0], usage.Metrics[1]
	if session.Kind != limitKindSession || session.Unknown {
		t.Errorf("session metric=%+v, want observed limitKindSession", session)
	}
	if session.Consumed == nil || *session.Consumed != 42 {
		t.Errorf("session Consumed=%v, want 42", session.Consumed)
	}
	if weekly.Kind != limitKindWeekly || weekly.Unknown {
		t.Errorf("weekly metric=%+v, want observed limitKindWeekly", weekly)
	}
	if weekly.Consumed == nil || *weekly.Consumed != 13 {
		t.Errorf("weekly Consumed=%v, want 13", weekly.Consumed)
	}
}

func TestCodexUsageParser_HonorsCodexHome(t *testing.T) {
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))
	home := t.TempDir()
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	helperWriteJSON(t, filepath.Join(codexHome, "auth.json"), map[string]any{
		"email": "codex-profile@example.com",
		"plan":  "pro",
	})

	usage, ok := codexUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, time.Now())
	if !ok || usage == nil {
		t.Fatalf("expected usage from CODEX_HOME")
	}
	if usage.Account != "codex-profile@example.com" {
		t.Errorf("Account=%q, want codex-profile@example.com", usage.Account)
	}
	if usage.Plan != "pro" {
		t.Errorf("Plan=%q, want pro", usage.Plan)
	}
}

func TestCodexUsageParser_NestedAuthDotJsonClaims(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))
	home := t.TempDir()
	helperWriteJSON(t, filepath.Join(home, ".codex", "auth.json"), map[string]any{
		"tokens": map[string]any{
			"id_token": helperJWT(t, map[string]any{
				"email": "oauth@example.com",
				"plan":  "plus",
				"sub":   "user-subject",
			}),
			"account_id": "acct_fallback",
		},
	})

	usage, ok := codexUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, time.Now())
	if !ok || usage == nil {
		t.Fatalf("expected usage")
	}
	if usage.Account != "oauth@example.com" {
		t.Errorf("Account=%q, want oauth@example.com", usage.Account)
	}
	if usage.Plan != "plus" {
		t.Errorf("Plan=%q, want plus", usage.Plan)
	}
	if usage.AccountFingerprint == "" {
		t.Errorf("expected fingerprint for nested auth account")
	}
}

// helperWriteRolloutLog writes a Codex session rollout log
// (CODEX_HOME/sessions/2026/06/<day>/rollout-<name>.jsonl) made of token_count
// event frames. Each entry in frames is the `rate_limits` object for one line;
// a nil entry emits the `rate_limits: null` heartbeat Codex sends between real
// readings.
func helperWriteRolloutLog(t *testing.T, base, day, name string, frames []map[string]any) {
	t.Helper()
	dir := filepath.Join(base, "sessions", "2026", "06", day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	var b strings.Builder
	for _, rl := range frames {
		line, err := json.Marshal(map[string]any{
			"timestamp": "2026-06-19T11:00:00.000Z",
			"type":      "event_msg",
			"payload": map[string]any{
				"type":        "token_count",
				"info":        map[string]any{"total_token_usage": map[string]any{"total_tokens": 1}},
				"rate_limits": rl,
			},
		})
		if err != nil {
			t.Fatalf("marshal rollout line: %v", err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-"+name+".jsonl"), []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write rollout log: %v", err)
	}
}

func codexRateLimitFrame(primaryPct, secondaryPct float64, now time.Time) map[string]any {
	return map[string]any{
		"primary":   map[string]any{"used_percent": primaryPct, "window_minutes": 300.0, "resets_at": float64(now.Add(time.Hour).Unix())},
		"secondary": map[string]any{"used_percent": secondaryPct, "window_minutes": 10080.0, "resets_at": float64(now.Add(72 * time.Hour).Unix())},
	}
}

// When no live app-server frame has been captured, the parser should backfill
// both windows from Codex's own rollout logs (the TUI-only path).
func TestCodexUsageParser_BackfillsFromRolloutLogs(t *testing.T) {
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "absent.json"))
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	helperWriteJSON(t, filepath.Join(codexHome, "auth.json"), map[string]any{
		"email": "carol@example.com",
		"plan":  "pro",
	})

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	helperWriteRolloutLog(t, codexHome, "19", "2026-06-19T11-00-00-aaaa", []map[string]any{
		codexRateLimitFrame(27, 10, now),
	})

	usage, ok := codexUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("expected usage")
	}
	session, weekly := usage.Metrics[0], usage.Metrics[1]
	if session.Unknown || session.Consumed == nil || *session.Consumed != 27 {
		t.Errorf("session metric=%+v, want observed 27%% from rollout", session)
	}
	if session.Label != "5-hour session window" {
		t.Errorf("session Label=%q, want 5-hour session window", session.Label)
	}
	if weekly.Unknown || weekly.Consumed == nil || *weekly.Consumed != 10 {
		t.Errorf("weekly metric=%+v, want observed 10%% from rollout", weekly)
	}
}

// A live captured bucket is authoritative: the rollout fallback must only fill
// windows that came back Unknown, never override an observed live reading.
func TestCodexUsageParser_RolloutOnlyFillsUnknownWindows(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	helperWriteJSON(t, filepath.Join(codexHome, "auth.json"), map[string]any{
		"email": "carol@example.com",
		"plan":  "pro",
	})

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	fp := fingerprintAccount("codex", "carol@example.com")
	// Live cache observed only the primary (5-hour) window.
	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary: {UsedPercentage: 42, ResetsAtMs: now.Add(time.Hour).UnixMilli(), usageKnown: true, resetKnown: true},
	}, nil, now, fp)
	// Rollout carries both — a conflicting primary (must be ignored) and the
	// only source for the weekly window.
	helperWriteRolloutLog(t, codexHome, "19", "2026-06-19T11-00-00-bbbb", []map[string]any{
		codexRateLimitFrame(99, 33, now),
	})

	usage, ok := codexUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("expected usage")
	}
	session, weekly := usage.Metrics[0], usage.Metrics[1]
	if session.Consumed == nil || *session.Consumed != 42 {
		t.Errorf("session Consumed=%v, want 42 (live cache wins over rollout)", session.Consumed)
	}
	if weekly.Unknown || weekly.Consumed == nil || *weekly.Consumed != 33 {
		t.Errorf("weekly metric=%+v, want 33 from rollout backfill", weekly)
	}
}

// Codex emits rate_limits: null between real readings; the parser must take the
// LAST populated frame, ignoring nulls and earlier values.
func TestCodexUsageParser_RolloutPrefersLastPopulatedFrame(t *testing.T) {
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "absent.json"))
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	helperWriteJSON(t, filepath.Join(codexHome, "auth.json"), map[string]any{
		"email": "carol@example.com",
	})

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	helperWriteRolloutLog(t, codexHome, "19", "2026-06-19T11-00-00-cccc", []map[string]any{
		codexRateLimitFrame(20, 5, now),
		nil, // heartbeat: rate_limits: null
		codexRateLimitFrame(55, 18, now),
	})

	usage, ok := codexUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("expected usage")
	}
	session := usage.Metrics[0]
	if session.Consumed == nil || *session.Consumed != 55 {
		t.Errorf("session Consumed=%v, want 55 (last populated frame)", session.Consumed)
	}
}

// A rollout log written before the current auth.json (i.e. by a previously
// signed-in account that shared this CODEX_HOME) must NOT backfill the new
// account's windows — that would leak the prior account's quota.
func TestCodexUsageParser_RolloutRejectsPreLoginLogs(t *testing.T) {
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "absent.json"))
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	// Rollout produced by the previous account.
	helperWriteRolloutLog(t, codexHome, "19", "2026-06-19T11-00-00-old", []map[string]any{
		codexRateLimitFrame(80, 70, now),
	})
	oldLog := filepath.Join(codexHome, "sessions", "2026", "06", "19", "rollout-2026-06-19T11-00-00-old.jsonl")
	past := time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	if err := os.Chtimes(oldLog, past, past); err != nil {
		t.Fatalf("chtimes rollout: %v", err)
	}
	// New account signs in AFTER that log was written.
	authPath := filepath.Join(codexHome, "auth.json")
	helperWriteJSON(t, authPath, map[string]any{"email": "newuser@example.com"})
	login := time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC)
	if err := os.Chtimes(authPath, login, login); err != nil {
		t.Fatalf("chtimes auth: %v", err)
	}

	usage, ok := codexUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("expected usage")
	}
	for _, m := range usage.Metrics {
		if !m.Unknown {
			t.Errorf("metric %q=%+v should stay Unknown; pre-login rollout must not backfill", m.Kind, m)
		}
	}
}

func TestGeminiUsageParser_FreeTierFramesTotal(t *testing.T) {
	home := t.TempDir()
	helperWriteJSON(t, filepath.Join(home, ".gemini", "settings.json"), map[string]any{
		"email": "bob@example.com",
		"tier":  "free",
	})
	usage, _ := geminiUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, time.Now())
	if usage == nil {
		t.Fatalf("expected usage")
	}
	if usage.Plan != "free" {
		t.Errorf("Plan=%q, want free", usage.Plan)
	}
	if len(usage.Metrics) != 1 || usage.Metrics[0].Total == nil || *usage.Metrics[0].Total != 1000 {
		t.Errorf("expected daily free cap of 1000 requests, got %+v", usage.Metrics)
	}
}

func TestGeminiUsageParser_OAuthCredentialsAccount(t *testing.T) {
	home := t.TempDir()
	helperWriteJSON(t, filepath.Join(home, ".gemini", "oauth_creds.json"), map[string]any{
		"id_token": helperJWT(t, map[string]any{
			"email": "gemini-oauth@example.com",
			"sub":   "gemini-subject",
		}),
	})
	helperWriteJSON(t, filepath.Join(home, ".gemini", "settings.json"), map[string]any{
		"tier": "free",
	})

	usage, _ := geminiUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, time.Now())
	if usage == nil {
		t.Fatalf("expected usage")
	}
	if usage.Account != "gemini-oauth@example.com" {
		t.Errorf("Account=%q, want gemini-oauth@example.com", usage.Account)
	}
	if usage.AccountFingerprint == "" {
		t.Errorf("expected fingerprint for OAuth account")
	}
	if usage.Plan != "free" {
		t.Errorf("Plan=%q, want free", usage.Plan)
	}
	if len(usage.Metrics) != 1 || usage.Metrics[0].Total == nil || *usage.Metrics[0].Total != 1000 {
		t.Errorf("expected daily free cap of 1000 requests, got %+v", usage.Metrics)
	}
}

func TestGeminiUsageParser_MissingTierKeepsTotalUnknown(t *testing.T) {
	home := t.TempDir()
	helperWriteJSON(t, filepath.Join(home, ".gemini", "settings.json"), map[string]any{
		"email": "paid-or-unknown@example.com",
	})
	usage, _ := geminiUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, time.Now())
	if usage == nil {
		t.Fatalf("expected usage")
	}
	if len(usage.Metrics) != 1 {
		t.Fatalf("expected one metric, got %d", len(usage.Metrics))
	}
	if usage.Metrics[0].Total != nil {
		t.Errorf("missing tier should leave Total unknown, got %v", *usage.Metrics[0].Total)
	}
}

func TestAntigravityUsageParser_PlanFromSettings(t *testing.T) {
	home := t.TempDir()
	helperWriteJSON(t, filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"), map[string]any{
		"email": "eve@example.com",
		"tier":  "team",
	})
	usage, _ := antigravityUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, time.Now())
	if usage == nil {
		t.Fatalf("expected usage")
	}
	if usage.Plan != "team" {
		t.Errorf("Plan=%q, want team", usage.Plan)
	}
	if usage.DataSource != "~/.gemini/antigravity-cli" {
		t.Errorf("DataSource=%q, want documented Antigravity settings path", usage.DataSource)
	}
}

func TestGrokUsageParser_CachedTokenAuthFile(t *testing.T) {
	t.Setenv("GROK_HOME", "")
	home := t.TempDir()
	helperWriteJSON(t, filepath.Join(home, ".grok", "auth.json"), map[string]any{
		"email":        "rick@example.com",
		"plan":         "premium",
		"subscription": "x-premium",
	})

	usage, ok := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true, Version: "0.4.1"}, time.Now())
	if !ok || usage == nil {
		t.Fatalf("expected usage entry from ~/.grok/auth.json")
	}
	if usage.Account != "rick@example.com" {
		t.Errorf("Account=%q, want rick@example.com", usage.Account)
	}
	if usage.Plan != "premium" {
		t.Errorf("Plan=%q, want premium", usage.Plan)
	}
	if usage.AccountFingerprint == "" {
		t.Errorf("expected fingerprint for known account")
	}
	if usage.DataSource != "~/.grok" {
		t.Errorf("DataSource=%q, want ~/.grok", usage.DataSource)
	}
	if len(usage.Metrics) != 2 {
		t.Fatalf("expected 2 metrics (requests + tokens), got %d", len(usage.Metrics))
	}
	for _, m := range usage.Metrics {
		if !m.Unknown {
			t.Errorf("metric %q should be Unknown (no observable counter)", m.Kind)
		}
	}
}

func TestGrokUsageParser_LegacyCachedTokenLayout(t *testing.T) {
	t.Setenv("GROK_HOME", "")
	home := t.TempDir()
	helperWriteJSON(t, filepath.Join(home, ".grok", "cached_token.json"), map[string]any{
		"cached_token": map[string]any{
			"id_token": helperJWT(t, map[string]any{
				"email":     "oauth-grok@example.com",
				"sub":       "grok-subject",
				"plan_type": "team",
			}),
		},
	})

	usage, ok := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, time.Now())
	if !ok || usage == nil {
		t.Fatalf("expected usage from legacy cached_token.json layout")
	}
	if usage.Account != "oauth-grok@example.com" {
		t.Errorf("Account=%q, want oauth-grok@example.com (from JWT claims)", usage.Account)
	}
	if usage.Plan != "team" {
		t.Errorf("Plan=%q, want team", usage.Plan)
	}
	if usage.AccountFingerprint == "" {
		t.Errorf("expected fingerprint for OAuth account")
	}
}

func TestGrokUsageParser_ScopedInstallerAuthFile(t *testing.T) {
	t.Setenv("GROK_HOME", "")
	home := t.TempDir()
	// Layout written by https://x.ai/cli/install.sh — top-level keys are auth
	// scopes, values wrap the JWT under `key`. Without scoped-format support
	// the parser would silently report a logged-in user as no-account.
	helperWriteJSON(t, filepath.Join(home, ".grok", "auth.json"), map[string]any{
		"grok-cli": map[string]any{
			"key": helperJWT(t, map[string]any{
				"email":     "scoped-grok@example.com",
				"sub":       "scoped-sub",
				"plan_type": "premium",
			}),
		},
	})

	usage, ok := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, time.Now())
	if !ok || usage == nil {
		t.Fatalf("expected usage from scoped installer auth.json")
	}
	if usage.Account != "scoped-grok@example.com" {
		t.Errorf("Account=%q, want scoped-grok@example.com (from scoped JWT)", usage.Account)
	}
	if usage.Plan != "premium" {
		t.Errorf("Plan=%q, want premium", usage.Plan)
	}
	if usage.AccountFingerprint == "" {
		t.Errorf("expected fingerprint for scoped account")
	}
}

func TestGrokUsageParser_HonorsGrokHomeEnv(t *testing.T) {
	home := t.TempDir()
	grokHome := t.TempDir()
	t.Setenv("GROK_HOME", grokHome)
	helperWriteJSON(t, filepath.Join(grokHome, "auth.json"), map[string]any{
		"email": "profile-grok@example.com",
		"plan":  "pro",
	})

	usage, ok := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, time.Now())
	if !ok || usage == nil {
		t.Fatalf("expected usage from $GROK_HOME")
	}
	if usage.Account != "profile-grok@example.com" {
		t.Errorf("Account=%q, want profile-grok@example.com", usage.Account)
	}
	if usage.DataSource != "$GROK_HOME" {
		t.Errorf("DataSource=%q, want $GROK_HOME marker so the UI distinguishes overrides", usage.DataSource)
	}
}

// TestGrokUsageParser_MissingCredentialsKeepsBaselineEntry pins the behaviour
// that lets the CLI Agents tab surface "Grok installed but not logged in" —
// the parser returns a baseline entry with unknown metrics so the user can
// see they need to run `grok login` rather than the agent silently dropping
// the row.
func TestGrokUsageParser_MissingCredentialsKeepsBaselineEntry(t *testing.T) {
	t.Setenv("GROK_HOME", "")
	home := t.TempDir()
	usage, ok := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, time.Now())
	if !ok || usage == nil {
		t.Fatalf("parser must return a baseline entry even without credentials so the UI can prompt the user to `grok login`")
	}
	if usage.Account != "" {
		t.Errorf("expected empty account without cached token; got %q", usage.Account)
	}
	if usage.AccountFingerprint != "" {
		t.Errorf("fingerprint should be empty when account is unknown")
	}
}

func TestGatherCLIAgentUsage_StableOrderAndOptIn(t *testing.T) {
	SetCLIAgentCatalog(nil)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", filepath.Join(t.TempDir(), "claude_rl.json"))
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "codex_rl.json"))
	helperWriteJSON(t, filepath.Join(home, ".claude", ".credentials.json"), map[string]any{
		"email": "ada@example.com",
	})
	detected := map[string]detectedCLIAgent{
		"claudeCode": {Detected: true, Name: "Claude Code", Version: "2.0"},
		"codex":      {Detected: true, Name: "Codex"},
	}
	out := gatherCLIAgentUsage(detected, time.Now())
	if len(out) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(out))
	}
	if out[0].Provider != "claudeCode" || out[1].Provider != "codex" {
		t.Errorf("ordering changed: %s, %s", out[0].Provider, out[1].Provider)
	}
	for _, u := range out {
		if u.Provider == "geminiCli" || u.Provider == "antigravity" {
			t.Errorf("unexpected entry for undetected agent: %s", u.Provider)
		}
	}
}

func TestGatherCLIAgentUsage_IncludesConfiguredAgentWithoutParser(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	SetCLIAgentCatalog([]cliAgentCatalogEntry{
		{
			ID:           "futureAgent",
			DisplayName:  "Future Agent",
			DisplayOrder: 10,
			Command:      "future-agent",
		},
	})
	t.Cleanup(func() { SetCLIAgentCatalog(nil) })

	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	out := gatherCLIAgentUsage(map[string]detectedCLIAgent{
		"futureAgent": {
			Detected: true,
			Name:     "Future Agent",
			Version:  "1.2.3",
			Path:     filepath.Join(home, "bin", "future-agent"),
		},
	}, now)

	if len(out) != 1 {
		t.Fatalf("expected one generic usage entry, got %d: %#v", len(out), out)
	}
	if out[0].Provider != "futureAgent" {
		t.Errorf("Provider=%q, want futureAgent", out[0].Provider)
	}
	if out[0].CliAgentID != "futureAgent" {
		t.Errorf("CliAgentID=%q, want futureAgent", out[0].CliAgentID)
	}
	if out[0].Name != "Future Agent" {
		t.Errorf("Name=%q, want Future Agent", out[0].Name)
	}
	if out[0].Version != "1.2.3" {
		t.Errorf("Version=%q, want 1.2.3", out[0].Version)
	}
	if out[0].AccountFingerprint == "" {
		t.Errorf("expected fallback device-scoped fingerprint for generic catalog agent")
	}
}

func TestGatherCLIAgentUsage_RespectsUtilizationDisabledCatalogEntry(t *testing.T) {
	SetCLIAgentCatalog([]cliAgentCatalogEntry{
		{
			ID:           "futureAgent",
			DisplayName:  "Future Agent",
			Command:      "future-agent",
			Capabilities: json.RawMessage(`{"utilization":false}`),
		},
	})
	t.Cleanup(func() { SetCLIAgentCatalog(nil) })

	out := gatherCLIAgentUsage(map[string]detectedCLIAgent{
		"futureAgent": {Detected: true, Name: "Future Agent"},
	}, time.Now())
	if len(out) != 0 {
		t.Fatalf("expected utilization-disabled agent to be omitted, got %#v", out)
	}
}

func TestGatherCLIAgentUsage_UnknownAccountGetsDeviceScopedFingerprint(t *testing.T) {
	SetCLIAgentCatalog(nil)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))

	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	out := gatherCLIAgentUsage(map[string]detectedCLIAgent{
		"codex": {
			Detected: true,
			Name:     "Codex",
			Version:  "0.14.0",
			Path:     filepath.Join(home, "bin", "codex"),
		},
	}, now)

	if len(out) != 1 {
		t.Fatalf("expected one entry, got %d", len(out))
	}
	if out[0].Account != "" {
		t.Fatalf("unknown account should not expose account text, got %q", out[0].Account)
	}
	if out[0].AccountFingerprint == "" {
		t.Fatalf("expected fallback fingerprint for unknown account")
	}
}

func TestGatherCLIAgentUsage_EmptyDetectedReturnsExplicitEmptySlice(t *testing.T) {
	SetCLIAgentCatalog(nil)
	out := gatherCLIAgentUsage(map[string]detectedCLIAgent{}, time.Now())
	if out == nil {
		t.Fatalf("expected non-nil empty slice so auth can send cliAgents: []")
	}
	if len(out) != 0 {
		t.Fatalf("expected no entries, got %d", len(out))
	}
}

// GatherCLIAgentUsageOnly is the lightweight per-tick entry point the
// pubsub.go __cli_usage_refresh__ handler calls. We exercise the
// happy-path empty-detect case and the context-cancel case here; the
// per-provider parsing logic is already covered by the per-provider
// tests above.
func TestGatherCLIAgentUsageOnly_EmptyWhenNoAgentsDetected(t *testing.T) {
	SetCLIAgentCatalog(nil)
	// Confine HOME so no real providers are picked up off the host.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", "/nonexistent")
	usage, errs := GatherCLIAgentUsageOnly(context.Background())
	if usage == nil {
		t.Fatalf("expected non-nil empty slice")
	}
	if len(usage) != 0 {
		t.Fatalf("expected empty usage when no agents are on PATH, got %d entries", len(usage))
	}
	if len(errs) != 0 {
		t.Fatalf("expected no errors when no providers were detected, got %d", len(errs))
	}
}

func TestGatherCLIAgentUsageOnly_RespectsCanceledContext(t *testing.T) {
	SetCLIAgentCatalog(nil)
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// The function should return promptly without panic.
	usage, _ := GatherCLIAgentUsageOnly(ctx)
	if usage == nil {
		t.Fatalf("expected empty slice on canceled context, got nil")
	}
}

func TestFingerprintAccount_StableAndProviderScoped(t *testing.T) {
	a := fingerprintAccount("claudeCode", "ada@example.com")
	b := fingerprintAccount("claudeCode", "ada@example.com")
	c := fingerprintAccount("codex", "ada@example.com")
	if a == "" || a != b {
		t.Errorf("expected stable hash for same input: a=%q b=%q", a, b)
	}
	if a == c {
		t.Errorf("expected different hash for different provider, got %q == %q", a, c)
	}
	if fingerprintAccount("claudeCode", "") != "" {
		t.Errorf("empty account must produce empty fingerprint")
	}
	if len(a) != 24 {
		t.Errorf("fingerprint length=%d, want 24", len(a))
	}
}

func TestReadJSONFile_GracefulOnMissing(t *testing.T) {
	var into map[string]any
	if readJSONFile(filepath.Join(t.TempDir(), "does-not-exist.json"), &into) {
		t.Errorf("readJSONFile should return false on missing file")
	}
}
