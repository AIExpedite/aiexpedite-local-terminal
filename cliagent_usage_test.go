package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
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

func TestCodexUsageParser_KnownTokenLimit(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	home := t.TempDir()
	helperWriteJSON(t, filepath.Join(home, ".codex", "auth.json"), map[string]any{
		"email":       "carol@example.com",
		"plan":        "pro",
		"token_limit": 250000,
	})
	usage, ok := codexUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, time.Now())
	if !ok {
		t.Fatalf("expected usage")
	}
	if usage.Plan != "pro" {
		t.Errorf("Plan=%q, want pro", usage.Plan)
	}
	tokens := usage.Metrics[0]
	if tokens.Kind != limitKindTokens {
		t.Errorf("first metric kind=%q, want %q", tokens.Kind, limitKindTokens)
	}
	if tokens.Total == nil || *tokens.Total != 250000 {
		t.Errorf("expected Total=250000, got %v", tokens.Total)
	}
	if !tokens.Unknown {
		t.Errorf("Unknown should remain true — we know cap, not consumed")
	}
}

func TestCodexUsageParser_HonorsCodexHome(t *testing.T) {
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
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
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

func TestGatherCLIAgentUsage_UnknownAccountGetsDeviceScopedFingerprint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")

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
	out := gatherCLIAgentUsage(map[string]detectedCLIAgent{}, time.Now())
	if out == nil {
		t.Fatalf("expected non-nil empty slice so auth can send cliAgents: []")
	}
	if len(out) != 0 {
		t.Fatalf("expected no entries, got %d", len(out))
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
