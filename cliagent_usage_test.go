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

// The account email is NOT in .credentials.json (it holds only the OAuth token
// object) — it lives in ~/.claude.json's oauthAccount block. The parser surfaces
// it as the card's DISPLAY label, but must NOT fingerprint by it: the shared
// dotfile is mutable, so the fingerprint stays credential-only (empty here) and
// gatherCLIAgentUsage device-scopes it.
func TestClaudeCodeUsageParser_AccountFromDotJSON(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))
	home := t.TempDir()

	// macOS default-config-dir reads the shared Keychain first; stub it off so
	// the test never picks up the real dev-machine login.
	orig := claudeKeychainReader
	t.Cleanup(func() { claudeKeychainReader = orig })
	claudeKeychainReader = func() ([]byte, bool) { return nil, false }

	// .credentials.json present but carrying only the token object (no email),
	// so the ONLY email source is .claude.json below.
	helperWriteJSON(t, filepath.Join(home, ".claude", ".credentials.json"), map[string]any{
		"claudeAiOauth": map[string]any{},
	})
	helperWriteJSON(t, filepath.Join(home, ".claude.json"), map[string]any{
		"oauthAccount": map[string]any{
			"emailAddress": "grace@example.com",
			"displayName":  "Grace",
		},
	})

	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	usage, ok := claudeCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !ok || usage == nil {
		t.Fatalf("expected usage, got ok=%v", ok)
	}
	// Displayed as the account label…
	if usage.Account != "grace@example.com" {
		t.Errorf("Account=%q, want grace@example.com (from ~/.claude.json)", usage.Account)
	}
	// …but the dotfile email is NEVER fingerprinted. Credential has no account,
	// so the fingerprint is empty (gatherCLIAgentUsage then device-scopes it).
	if usage.AccountFingerprint != "" {
		t.Errorf("AccountFingerprint=%q, want empty (dotfile email must not be fingerprinted)", usage.AccountFingerprint)
	}
}

// An account email inside .credentials.json (older Claude Code layouts / API-key
// installs that populate it) wins over ~/.claude.json — the parser only falls
// back when the credential yields no account.
func TestClaudeCodeUsageParser_CredentialsAccountWinsOverDotJSON(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))
	home := t.TempDir()

	orig := claudeKeychainReader
	t.Cleanup(func() { claudeKeychainReader = orig })
	claudeKeychainReader = func() ([]byte, bool) { return nil, false }

	helperWriteJSON(t, filepath.Join(home, ".claude", ".credentials.json"), map[string]any{
		"email": "creds@example.com",
	})
	helperWriteJSON(t, filepath.Join(home, ".claude.json"), map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "dotjson@example.com"},
	})

	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	usage, _ := claudeCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if usage.Account != "creds@example.com" {
		t.Errorf("Account=%q, want creds@example.com (credential should win)", usage.Account)
	}
}

// displayName is shown as a friendly label when emailAddress is absent (e.g. an
// SSO profile that hydrated a name but not an email) — but it is NOT unique, so
// it must never become the account fingerprint (that would collapse two users
// named "Grace" into one quota). The identity, and therefore the fingerprint,
// stays empty; gatherCLIAgentUsage then device-scopes it.
func TestClaudeCodeUsageParser_DotJSONDisplayNameNotFingerprinted(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	home := t.TempDir()

	orig := claudeKeychainReader
	t.Cleanup(func() { claudeKeychainReader = orig })
	claudeKeychainReader = func() ([]byte, bool) { return nil, false }

	helperWriteJSON(t, filepath.Join(home, ".claude.json"), map[string]any{
		"oauthAccount": map[string]any{"displayName": "Grace"},
	})

	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	usage, _ := claudeCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if usage.Account != "Grace" {
		t.Errorf("Account=%q, want Grace (displayName shown as label)", usage.Account)
	}
	if usage.AccountFingerprint != "" {
		t.Errorf("AccountFingerprint=%q, want empty (a display name must not be fingerprinted)", usage.AccountFingerprint)
	}
}

// Parse runs in the local-terminal DAEMON, whose environment reflects the shell
// it was launched from — NOT the Claude sessions it drives, which strip the env
// credentials to force subscription billing. So a stray ANTHROPIC_API_KEY in the
// daemon's shell must NOT blank the stored oauthAccount: those driver sessions
// still bill the subscription, and the account line/fingerprint must reflect it.
// (The env-auth exception is applied only by the status-line hook — see
// TestCaptureClaudeRateLimitsFromStatusline_ScopesByActiveCredential.)
func TestClaudeCodeUsageParser_DaemonEnvKeyDoesNotSuppressStoredAccount(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test") // stray daemon-shell key
	home := t.TempDir()

	orig := claudeKeychainReader
	t.Cleanup(func() { claudeKeychainReader = orig })
	claudeKeychainReader = func() ([]byte, bool) { return nil, false }

	helperWriteJSON(t, filepath.Join(home, ".claude.json"), map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "grace@example.com"},
	})

	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	usage, _ := claudeCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	// The daemon still shows the stored account label despite the stray env key…
	if usage.Account != "grace@example.com" {
		t.Errorf("Account=%q, want grace@example.com (daemon must show stored account despite stray env key)", usage.Account)
	}
	// …and the fingerprint is credential-only (empty for this claude.ai login),
	// never the dotfile email.
	if usage.AccountFingerprint != "" {
		t.Errorf("AccountFingerprint=%q, want empty (credential-only fingerprint)", usage.AccountFingerprint)
	}
}

// Parse's fingerprint (published + cache-read scope) is the CREDENTIAL account
// only, never the ~/.claude.json email. A claude.ai login (no credential
// account) publishes "" and reads the "" cache it actually wrote — so the windows
// display, and env-auth / previous-account data is never pulled onto the
// email-labelled card under a mismatched identity. This is the consistency the
// P1 review required: the cache scope and the published identity are identical.
func TestClaudeCodeUsageParser_FingerprintAndCacheScopeAreCredentialOnly(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	home := t.TempDir()

	orig := claudeKeychainReader
	t.Cleanup(func() { claudeKeychainReader = orig })
	claudeKeychainReader = func() ([]byte, bool) { return nil, false }

	// claude.ai login: email only in ~/.claude.json, no account in the credential.
	helperWriteJSON(t, filepath.Join(home, ".claude.json"), map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "grace@example.com"},
	})

	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	// Seed the cache UNSCOPED (""), exactly as the status-line hook does for a
	// claude.ai login whose credential carries no account.
	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 40,
			ResetsAtMs:     now.Add(time.Hour).UnixMilli(),
			usageKnown:     true,
		},
	}, now, "")

	usage, _ := claudeCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)

	// Published fingerprint == cache scope == "" (credential-only). The email is
	// a display label only.
	if usage.Account != "grace@example.com" {
		t.Errorf("Account=%q, want grace@example.com (display label)", usage.Account)
	}
	if usage.AccountFingerprint != "" {
		t.Errorf("AccountFingerprint=%q, want empty (credential-only, matches cache scope)", usage.AccountFingerprint)
	}
	// The "" cache the login actually wrote is read back, so the window displays.
	if len(usage.Metrics) == 0 || usage.Metrics[0].Unknown {
		t.Errorf("5-hour metric should be observed from the matching-scope cache, got %+v", usage.Metrics)
	}
}

func helperWriteClaudeOAuth(t *testing.T, home string, extra map[string]any) {
	t.Helper()
	oauth := map[string]any{}
	for k, v := range extra {
		oauth[k] = v
	}
	helperWriteJSON(t, filepath.Join(home, ".claude", ".credentials.json"), map[string]any{
		"email":         "ada@example.com",
		"claudeAiOauth": oauth,
	})
}

func TestClaudeCodeUsageParser_AuthExpiredNotice(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))
	home := t.TempDir()
	now := time.Now()
	// Access token still valid, but the REFRESH token expired 2h ago → re-login.
	helperWriteClaudeOAuth(t, home, map[string]any{
		"expiresAt":             now.Add(2 * time.Hour).UnixMilli(),
		"refreshTokenExpiresAt": now.Add(-2 * time.Hour).UnixMilli(),
	})

	usage, ok := claudeCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !ok || usage == nil {
		t.Fatalf("expected usage entry")
	}
	if usage.NoticeSeverity != "error" {
		t.Errorf("NoticeSeverity=%q, want error", usage.NoticeSeverity)
	}
	if !strings.Contains(usage.Notice, "expired") {
		t.Errorf("Notice=%q, want an expired re-login prompt", usage.Notice)
	}
	if usage.Authenticated == nil || *usage.Authenticated || usage.AuthState != "expired" {
		t.Errorf("auth state = (%v, %q), want false/expired", usage.Authenticated, usage.AuthState)
	}
	if usage.LoginExpirationState != loginExpirationKnown || usage.LoginExpiresAt == "" {
		t.Errorf("login expiration = (%q, %q), want known deadline", usage.LoginExpirationState, usage.LoginExpiresAt)
	}
	for _, metric := range usage.Metrics {
		if !metric.Unknown || metric.Consumed != nil || metric.Remaining != nil {
			t.Errorf("expired login must make utilization unobservable, got %+v", metric)
		}
	}
}

func TestClaudeCodeUsageParser_AuthExpiringSoonNotice(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))
	home := t.TempDir()
	now := time.Now()
	helperWriteClaudeOAuth(t, home, map[string]any{
		"refreshTokenExpiresAt": now.Add(12 * time.Hour).UnixMilli(), // within the 48h window
	})

	usage, _ := claudeCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if usage.NoticeSeverity != "warning" {
		t.Errorf("NoticeSeverity=%q, want warning", usage.NoticeSeverity)
	}
	if !strings.Contains(usage.Notice, "expires soon") {
		t.Errorf("Notice=%q, want an expiring-soon prompt", usage.Notice)
	}
	if usage.Authenticated == nil || !*usage.Authenticated || usage.LoginExpiresAt == "" {
		t.Errorf("healthy expiring login should publish authenticated exact deadline: %+v", usage)
	}
}

func TestClaudeCodeUsageParser_AuthHealthyNoNotice(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))
	home := t.TempDir()
	now := time.Now()
	// Access token EXPIRED but the refresh token is valid for 10 days — the CLI
	// silently refreshes, so this must NOT warn.
	helperWriteClaudeOAuth(t, home, map[string]any{
		"expiresAt":             now.Add(-1 * time.Hour).UnixMilli(),
		"refreshTokenExpiresAt": now.Add(10 * 24 * time.Hour).UnixMilli(),
	})

	usage, _ := claudeCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if usage.Notice != "" {
		t.Errorf("expected no auth notice while the refresh token is valid, got %q", usage.Notice)
	}
}

func TestClaudeCodeUsageParser_ApiKeyNoOAuthNoNotice(t *testing.T) {
	// Flat / API-key creds (no claudeAiOauth) must not raise a false auth notice:
	// absence of an OAuth deadline is not a "signed out" signal.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))
	originalProbe := claudeAuthStatusProbe
	claudeAuthStatusProbe = func(string) (bool, bool) { return true, true }
	t.Cleanup(func() { claudeAuthStatusProbe = originalProbe })
	home := t.TempDir()
	helperWriteJSON(t, filepath.Join(home, ".claude", ".credentials.json"), map[string]any{
		"email": "ada@example.com",
		"plan":  "max",
	})

	usage, _ := claudeCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, time.Now())
	if usage.Notice != "" {
		t.Errorf("unexpected notice for non-OAuth credentials: %q", usage.Notice)
	}
}

func TestClaudeCodeUsageParser_AuthFromKeychainWhenNoFile(t *testing.T) {
	// On macOS Claude Code stores the credential in the Keychain, not
	// ~/.claude/.credentials.json. When the file is absent, the parser must fall
	// back to the platform reader so an expired login is still surfaced. Override
	// the reader so this exercises cross-platform.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))
	home := t.TempDir() // no .credentials.json on disk
	now := time.Now()

	orig := claudeKeychainReader
	t.Cleanup(func() { claudeKeychainReader = orig })
	raw, _ := json.Marshal(map[string]any{
		"email": "kc@example.com",
		"claudeAiOauth": map[string]any{
			"refreshTokenExpiresAt": now.Add(-time.Hour).UnixMilli(), // expired
		},
	})
	claudeKeychainReader = func() ([]byte, bool) { return raw, true }

	usage, ok := claudeCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !ok || usage == nil {
		t.Fatalf("expected usage entry")
	}
	if usage.Account != "kc@example.com" {
		t.Errorf("Account=%q, want kc@example.com (resolved from keychain)", usage.Account)
	}
	if usage.NoticeSeverity != "error" || !strings.Contains(usage.Notice, "expired") {
		t.Errorf("Notice=%q sev=%q, want expired error from the keychain credential",
			usage.Notice, usage.NoticeSeverity)
	}
}

func TestClaudeCodeUsageParser_NoFileNoKeychainNoNotice(t *testing.T) {
	// Fresh box / API-key install with neither a file nor a keychain credential
	// must not raise a false auth notice.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))
	home := t.TempDir()
	orig := claudeKeychainReader
	t.Cleanup(func() { claudeKeychainReader = orig })
	claudeKeychainReader = func() ([]byte, bool) { return nil, false }

	usage, _ := claudeCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, time.Now())
	if usage.Notice != "" {
		t.Errorf("unexpected notice with no credential source: %q", usage.Notice)
	}
}

func TestClaudeCodeUsageParser_KeychainSkippedForCustomConfigDir(t *testing.T) {
	// On macOS the "Claude Code-credentials" Keychain item is a single shared
	// entry for the DEFAULT account. When a non-default CLAUDE_CONFIG_DIR selects
	// a side-by-side profile whose .credentials.json is absent, the parser must
	// NOT fall back to that Keychain item — doing so would misattribute the
	// default account's identity/auth-expiry to the profile.
	configDir := t.TempDir() // custom profile, no .credentials.json on disk
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))
	now := time.Now()

	orig := claudeKeychainReader
	t.Cleanup(func() { claudeKeychainReader = orig })
	keychainRead := false
	raw, _ := json.Marshal(map[string]any{
		"email": "default-account@example.com",
		"claudeAiOauth": map[string]any{
			"refreshTokenExpiresAt": now.Add(-time.Hour).UnixMilli(), // expired
		},
	})
	claudeKeychainReader = func() ([]byte, bool) { keychainRead = true; return raw, true }

	usage, ok := claudeCodeUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !ok || usage == nil {
		t.Fatalf("expected baseline usage entry")
	}
	if keychainRead {
		t.Errorf("Keychain must not be consulted for a non-default CLAUDE_CONFIG_DIR profile")
	}
	if usage.Account != "" {
		t.Errorf("Account=%q, want empty (default-account keychain credential must not leak into a custom profile)", usage.Account)
	}
	if usage.Notice != "" {
		t.Errorf("Notice=%q, want empty (must not surface the default account's expiry for a custom profile)", usage.Notice)
	}
}

func TestClaudeCodeUsageParser_KeychainPreferredOverStaleFile(t *testing.T) {
	// Upgraded macOS box on the DEFAULT config dir: a stale ~/.claude/.credentials.json
	// lingers on disk (older Claude Code wrote it) while the ACTIVE login now lives
	// in the Keychain. The parser must fingerprint + expiry-check the Keychain
	// credential, not the stale file — otherwise the stale file raises a false
	// expired banner and attributes usage to the wrong account.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))
	home := t.TempDir()
	now := time.Now()

	// Stale on-disk file: a different account whose refresh token expired days ago.
	helperWriteJSON(t, filepath.Join(home, ".claude", ".credentials.json"), map[string]any{
		"email": "stale@example.com",
		"claudeAiOauth": map[string]any{
			"refreshTokenExpiresAt": now.Add(-72 * time.Hour).UnixMilli(),
		},
	})

	orig := claudeKeychainReader
	t.Cleanup(func() { claudeKeychainReader = orig })
	raw, _ := json.Marshal(map[string]any{
		"email": "active@example.com",
		"claudeAiOauth": map[string]any{
			"refreshTokenExpiresAt": now.Add(30 * 24 * time.Hour).UnixMilli(), // healthy
		},
	})
	claudeKeychainReader = func() ([]byte, bool) { return raw, true }

	usage, ok := claudeCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !ok || usage == nil {
		t.Fatalf("expected usage entry")
	}
	if usage.Account != "active@example.com" {
		t.Errorf("Account=%q, want active@example.com (keychain must win over the stale file)", usage.Account)
	}
	if usage.Notice != "" {
		t.Errorf("Notice=%q, want empty (active keychain login is healthy; the stale file must not raise a false banner)", usage.Notice)
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

func TestClaudeCodeUsageParser_InvalidCredentialsDoNotAuthenticate(t *testing.T) {
	originalProbe := claudeAuthStatusProbe
	claudeAuthStatusProbe = func(string) (bool, bool) { return false, false }
	t.Cleanup(func() { claudeAuthStatusProbe = originalProbe })

	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "empty file", raw: ""},
		{name: "malformed json", raw: "{"},
		{name: "missing credential", raw: `{"claudeAiOauth":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			cache := filepath.Join(t.TempDir(), "rl.json")
			t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
			now := time.Now()
			mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
				claudeWindowFiveHour: {
					UsedPercentage: 40,
					ResetsAtMs:     now.Add(time.Hour).UnixMilli(),
					ObservedAtMs:   now.UnixMilli(),
					usageKnown:     true,
				},
			}, now, "")

			credentialPath := filepath.Join(home, ".claude", ".credentials.json")
			if err := os.MkdirAll(filepath.Dir(credentialPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(credentialPath, []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}

			usage, _ := claudeCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
			if usage.Authenticated == nil || *usage.Authenticated || usage.AuthState != "missing" {
				t.Errorf("auth state=(%v, %q), want false/missing", usage.Authenticated, usage.AuthState)
			}
			for _, metric := range usage.Metrics {
				if !metric.Unknown || metric.Consumed != nil || metric.Remaining != nil {
					t.Errorf("invalid credentials must make cached usage unobservable: %+v", metric)
				}
			}
		})
	}
}

func TestClaudeCodeUsageParser_DefiniteLoggedOutProbeMakesUsageUnobservable(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))
	original := claudeAuthStatusProbe
	claudeAuthStatusProbe = func(string) (bool, bool) { return false, true }
	t.Cleanup(func() { claudeAuthStatusProbe = original })

	usage, _ := claudeCodeUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{
		Detected: true, Path: "claude-test",
	}, time.Now())
	if usage.Authenticated == nil || *usage.Authenticated || usage.AuthState != "missing" {
		t.Errorf("auth state = (%v, %q), want false/missing", usage.Authenticated, usage.AuthState)
	}
	for _, metric := range usage.Metrics {
		if !metric.Unknown || metric.Consumed != nil || metric.Remaining != nil {
			t.Errorf("logged-out Claude usage must be unobservable: %+v", metric)
		}
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

func TestCodexUsageParser_RefreshTokenHasNoMisleadingLoginDeadline(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))
	home := t.TempDir()
	helperWriteJSON(t, filepath.Join(home, ".codex", "auth.json"), map[string]any{
		"tokens": map[string]any{
			"id_token":      "opaque-id-token",
			"refresh_token": "opaque-refresh-token",
		},
	})
	usage, _ := codexUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, time.Now())
	if usage.Authenticated == nil || !*usage.Authenticated || usage.AuthState != "authenticated" {
		t.Errorf("auth state = (%v, %q), want true/authenticated", usage.Authenticated, usage.AuthState)
	}
	if usage.LoginExpirationState != loginExpirationRefreshable || usage.LoginExpiresAt != "" {
		t.Errorf("login expiration = (%q, %q), want refreshable without a deadline", usage.LoginExpirationState, usage.LoginExpiresAt)
	}
}

func TestCodexUsageParser_DefiniteLoggedOutProbeMakesUsageUnobservable(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))
	original := codexAuthStatusProbe
	codexAuthStatusProbe = func(string) (bool, bool) { return false, true }
	t.Cleanup(func() { codexAuthStatusProbe = original })

	usage, _ := codexUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{
		Detected: true, Path: "codex-test",
	}, time.Now())
	if usage.Authenticated == nil || *usage.Authenticated || usage.AuthState != "missing" {
		t.Errorf("auth state = (%v, %q), want false/missing", usage.Authenticated, usage.AuthState)
	}
	if usage.NoticeSeverity != "error" || !strings.Contains(usage.Notice, "not signed in") {
		t.Errorf("notice = (%q, %q), want login error", usage.Notice, usage.NoticeSeverity)
	}
	for _, metric := range usage.Metrics {
		if !metric.Unknown || metric.Consumed != nil || metric.Remaining != nil {
			t.Errorf("logged-out Codex usage must be unobservable: %+v", metric)
		}
	}
}

func TestCodexUsageParser_EnvironmentAPIKeySurvivesPersistedLogoutProbe(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	t.Setenv("OPENAI_API_KEY", "sk-environment-test")
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Now()
	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary: {
			UsedPercentage: 21,
			ResetsAtMs:     now.Add(time.Hour).UnixMilli(),
			usageKnown:     true,
			resetKnown:     true,
		},
		codexWindowSecondary: {
			UsedPercentage: 34,
			ResetsAtMs:     now.Add(24 * time.Hour).UnixMilli(),
			usageKnown:     true,
			resetKnown:     true,
		},
	}, nil, now, "")

	original := codexAuthStatusProbe
	codexAuthStatusProbe = func(string) (bool, bool) { return false, true }
	t.Cleanup(func() { codexAuthStatusProbe = original })

	usage, _ := codexUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{
		Detected: true, Path: "codex-test",
	}, now)
	if usage.Authenticated == nil || !*usage.Authenticated || usage.AuthState != "authenticated" {
		t.Errorf("environment API-key auth state = (%v, %q), want true/authenticated", usage.Authenticated, usage.AuthState)
	}
	if usage.Notice != "" {
		t.Errorf("Notice=%q, want no persisted-login warning for environment API-key auth", usage.Notice)
	}
	for _, metric := range usage.Metrics {
		if metric.Unknown {
			t.Errorf("environment API-key auth must preserve observed usage: %+v", metric)
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

// Parse composition (AC1/AC6): two same-identity weekly observations that would
// previously surface as duplicate "Weekly quota" rows collapse to a single
// weekly metric, with the 5-hour session row first (Unknown here) and the weekly
// row second.
func TestCodexUsageParser_DeduplicatesWeeklyAndKeepsClaudeOrder(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	home := t.TempDir()
	helperWriteJSON(t, filepath.Join(home, ".codex", "auth.json"), map[string]any{
		"email": "carol@example.com",
		"plan":  "pro",
	})

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	older := now.Add(-time.Minute)
	fp := fingerprintAccount("codex", "carol@example.com")

	// Older weekly under secondary; newer weekly-band reading under primary.
	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowSecondary: {
			UsedPercentage: 70, ResetsAtMs: now.Add(7 * 24 * time.Hour).UnixMilli(),
			WindowMinutes: 10080, ObservedAtMs: older.UnixMilli(), usageKnown: true, resetKnown: true,
		},
	}, nil, older, fp)
	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary: {
			UsedPercentage: 30, ResetsAtMs: now.Add(7 * 24 * time.Hour).UnixMilli(),
			WindowMinutes: 10080, ObservedAtMs: now.UnixMilli(), usageKnown: true, resetKnown: true,
		},
	}, nil, now, fp)

	usage, ok := codexUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("expected usage")
	}
	if len(usage.Metrics) != 2 {
		t.Fatalf("expected exactly 2 metrics (no duplicate weekly), got %d: %+v", len(usage.Metrics), usage.Metrics)
	}
	if usage.Metrics[0].Kind != limitKindSession || !usage.Metrics[0].Unknown {
		t.Errorf("metrics[0]=%+v, want Unknown session first", usage.Metrics[0])
	}
	weekly := usage.Metrics[1]
	if weekly.Kind != limitKindWeekly || weekly.Unknown {
		t.Errorf("metrics[1]=%+v, want single known weekly", weekly)
	}
	if weekly.Consumed == nil || *weekly.Consumed != 30 {
		t.Errorf("weekly Consumed=%v, want 30 (newest weekly wins)", weekly.Consumed)
	}
}

// Parse composition (AC4): a weekly-only cache with no 5-hour observation yields
// an Unknown session row first and a real weekly row second — never a weekly
// value mislabeled as a 5-hour/daily/shift quota. With CODEX_HOME empty there is
// no rollout backfill to fill the session row.
func TestCodexUsageParser_MissingSessionStaysUnknown(t *testing.T) {
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
		codexWindowSecondary: {
			UsedPercentage: 40, ResetsAtMs: now.Add(4 * 24 * time.Hour).UnixMilli(),
			WindowMinutes: 10080, usageKnown: true, resetKnown: true,
		},
	}, nil, now, fp)

	usage, ok := codexUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("expected usage")
	}
	if len(usage.Metrics) != 2 {
		t.Fatalf("expected 2 metrics, got %d", len(usage.Metrics))
	}
	if usage.Metrics[0].Kind != limitKindSession || !usage.Metrics[0].Unknown {
		t.Errorf("metrics[0]=%+v, want Unknown session", usage.Metrics[0])
	}
	if usage.Metrics[1].Kind != limitKindWeekly || usage.Metrics[1].Unknown {
		t.Errorf("metrics[1]=%+v, want known weekly", usage.Metrics[1])
	}
	if usage.Metrics[1].Label != "Weekly quota" {
		t.Errorf("weekly label=%q, want Weekly quota", usage.Metrics[1].Label)
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
	helperWriteRolloutLogAt(t, base, day, name, "2026-06-19T11:00:00.000Z", frames)
}

// helperWriteRolloutLogAt is helperWriteRolloutLog with an explicit per-line
// event timestamp, used to exercise relative-reset anchoring.
func helperWriteRolloutLogAt(t *testing.T, base, day, name, ts string, frames []map[string]any) {
	t.Helper()
	dir := filepath.Join(base, "sessions", "2026", "06", day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	var b strings.Builder
	for _, rl := range frames {
		line, err := json.Marshal(map[string]any{
			"timestamp": ts,
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

// helperCodexAuthAt writes auth.json for email and stamps its mtime to loginAt —
// the account-login watermark the rollout scope guard compares each session's
// start time against. Default rollout session timestamps are 2026-06-19T11:00Z,
// so a login earlier that day keeps them in scope.
func helperCodexAuthAt(t *testing.T, codexHome, email string, loginAt time.Time) {
	t.Helper()
	authPath := filepath.Join(codexHome, "auth.json")
	helperWriteJSON(t, authPath, map[string]any{"email": email})
	if err := os.Chtimes(authPath, loginAt, loginAt); err != nil {
		t.Fatalf("chtimes auth: %v", err)
	}
}

// codexTestLogin is a fixed account-login watermark before the default rollout
// session timestamps, so the scope guard is deterministic regardless of when the
// suite runs (auth.json would otherwise carry the real wall-clock mtime).
var codexTestLogin = time.Date(2026, 6, 19, 9, 0, 0, 0, time.UTC)

// When no live app-server frame has been captured, the parser should backfill
// both windows from Codex's own rollout logs (the TUI-only path).
func TestCodexUsageParser_BackfillsFromRolloutLogs(t *testing.T) {
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "absent.json"))
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	helperCodexAuthAt(t, codexHome, "carol@example.com", codexTestLogin)

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
	helperCodexAuthAt(t, codexHome, "carol@example.com", codexTestLogin)

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

// A cached live bucket whose reset has already passed rolls over to a concrete
// 0% (Unknown=false) at display time, but the user could have continued
// driving Codex through its TUI in the new window — that usage only lands in
// the rollout log, never back in our cache. Without past-reset → fillable, the
// card would show 0% forever; with it, the newer rollout reading wins.
func TestCodexUsageParser_RolloutBackfillsRolledOverCacheBucket(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	helperCodexAuthAt(t, codexHome, "carol@example.com", codexTestLogin)

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	fp := fingerprintAccount("codex", "carol@example.com")
	// Live cache once observed the primary window — but its reset has passed,
	// so codexObservedMetricOrUnknown rolls it over to 0% (Unknown=false). The
	// weekly window stays Unknown.
	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary: {UsedPercentage: 42, ResetsAtMs: now.Add(-time.Hour).UnixMilli(), usageKnown: true, resetKnown: true},
	}, nil, now.Add(-2*time.Hour), fp)
	// Rollout carries current usage from a TUI-only session in the new window.
	helperWriteRolloutLog(t, codexHome, "19", "2026-06-19T11-00-00-rollover", []map[string]any{
		codexRateLimitFrame(63, 21, now),
	})

	usage, ok := codexUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("expected usage")
	}
	session, weekly := usage.Metrics[0], usage.Metrics[1]
	if session.Unknown || session.Consumed == nil || *session.Consumed != 63 {
		t.Errorf("session metric=%+v, want 63 from rollout (cache rolled over to stale 0%%)", session)
	}
	if weekly.Unknown || weekly.Consumed == nil || *weekly.Consumed != 21 {
		t.Errorf("weekly metric=%+v, want 21 from rollout backfill", weekly)
	}
}

// AC4 in the rollout path: a rollout log carrying ONLY the weekly window must
// fill the weekly row and leave the session row Unknown — the identity-gated
// backfill must never promote a weekly reading into the 5-hour session row.
func TestCodexUsageParser_WeeklyOnlyRolloutLeavesSessionUnknown(t *testing.T) {
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "absent.json"))
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	helperCodexAuthAt(t, codexHome, "carol@example.com", codexTestLogin)

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	// Rollout frame with the weekly window only — no primary/session key at all.
	helperWriteRolloutLog(t, codexHome, "19", "2026-06-19T11-00-00-weeklyonly", []map[string]any{
		{"secondary": map[string]any{
			"used_percent":   44.0,
			"window_minutes": 10080.0,
			"resets_at":      float64(now.Add(72 * time.Hour).Unix()),
		}},
	})

	usage, ok := codexUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("expected usage")
	}
	session, weekly := usage.Metrics[0], usage.Metrics[1]
	if !session.Unknown {
		t.Errorf("session metric=%+v, want Unknown (weekly-only rollout must not fill the 5-hour row)", session)
	}
	if weekly.Unknown || weekly.Consumed == nil || *weekly.Consumed != 44 {
		t.Errorf("weekly metric=%+v, want observed 44%% from weekly-only rollout", weekly)
	}
	if weekly.Label != "Weekly quota" {
		t.Errorf("weekly Label=%q, want Weekly quota", weekly.Label)
	}
}

// helperWriteRolloutRawPayload writes a single rollout log line whose
// event_msg payload is exactly `payload` — used for shapes helperWriteRolloutLog
// can't express, such as `rate_limits_by_limit_id`.
func helperWriteRolloutRawPayload(t *testing.T, base, day, name string, payload map[string]any) {
	t.Helper()
	dir := filepath.Join(base, "sessions", "2026", "06", day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	line, err := json.Marshal(map[string]any{
		"timestamp": "2026-06-19T11:00:00.000Z",
		"type":      "event_msg",
		"payload":   payload,
	})
	if err != nil {
		t.Fatalf("marshal rollout line: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-"+name+".jsonl"), append(line, '\n'), 0o600); err != nil {
		t.Fatalf("write rollout log: %v", err)
	}
}

// A TUI-only rollout frame can carry two DISTINCT-identity limits under the same
// physical slot during bucket migration (e.g. a 5-hour session limit and a
// weekly limit both keyed under `primary`). The rollout fallback must carry each
// per-limit contributor through identity partitioning rather than collapsing the
// slot into a single most-constrained bucket, or one utilization row is lost.
func TestCodexUsageParser_RolloutPreservesMixedIdentitySameSlot(t *testing.T) {
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "absent.json"))
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	helperCodexAuthAt(t, codexHome, "carol@example.com", codexTestLogin)

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	// Both a session (300-min) and a weekly (10080-min) metered limit keyed under
	// the SAME physical `primary` slot.
	helperWriteRolloutRawPayload(t, codexHome, "19", "2026-06-19T11-00-00-mixed", map[string]any{
		"type": "token_count",
		"info": map[string]any{"total_token_usage": map[string]any{"total_tokens": 1}},
		"rate_limits_by_limit_id": map[string]any{
			"codex_session": map[string]any{"primary": map[string]any{
				"used_percent": 30.0, "window_minutes": 300.0, "resets_at": float64(now.Add(time.Hour).Unix()),
			}},
			"codex_weekly": map[string]any{"primary": map[string]any{
				"used_percent": 60.0, "window_minutes": 10080.0, "resets_at": float64(now.Add(72 * time.Hour).Unix()),
			}},
		},
	})

	usage, ok := codexUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("expected usage")
	}
	session, weekly := usage.Metrics[0], usage.Metrics[1]
	if session.Unknown || session.Consumed == nil || *session.Consumed != 30 {
		t.Errorf("session metric=%+v, want observed 30%% from mixed-slot rollout", session)
	}
	if weekly.Unknown || weekly.Consumed == nil || *weekly.Consumed != 60 {
		t.Errorf("weekly metric=%+v, want observed 60%% (must not be lost to slot aggregation)", weekly)
	}
}

// The rollout accumulator is keyed by (identity, limit id) so a distinct metered
// limit that newer logs never restated is still folded into its identity's
// most-constrained aggregate. The scan must NOT stop as soon as both display
// identities are present: an older in-cap log can hold a separate, stricter
// weekly limit that the newest log omitted, and dropping it would understate the
// weekly row.
func TestCodexUsageParser_RolloutFoldsOmittedStricterWeeklyLimit(t *testing.T) {
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "absent.json"))
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	helperCodexAuthAt(t, codexHome, "carol@example.com", codexTestLogin)

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	// Newest log restates a session limit and a weekly limit B (40%). On its own
	// this already covers both display identities, so an identity-presence break
	// would stop here.
	helperWriteRolloutRawPayload(t, codexHome, "19", "2026-06-19T11-30-00-newest", map[string]any{
		"type": "token_count",
		"info": map[string]any{"total_token_usage": map[string]any{"total_tokens": 1}},
		"rate_limits_by_limit_id": map[string]any{
			"codex_session": map[string]any{"primary": map[string]any{
				"used_percent": 30.0, "window_minutes": 300.0, "resets_at": float64(now.Add(time.Hour).Unix()),
			}},
			"codex_weekly_b": map[string]any{"secondary": map[string]any{
				"used_percent": 40.0, "window_minutes": 10080.0, "resets_at": float64(now.Add(72 * time.Hour).Unix()),
			}},
		},
	})
	// An older (still in-scope) log holds a SEPARATE, stricter weekly limit A
	// (85%) the newest log did not restate. It must still be folded so the weekly
	// row reflects the most-constrained reading.
	helperWriteRolloutRawPayload(t, codexHome, "19", "2026-06-19T11-00-00-older", map[string]any{
		"type": "token_count",
		"info": map[string]any{"total_token_usage": map[string]any{"total_tokens": 1}},
		"rate_limits_by_limit_id": map[string]any{
			"codex_weekly_a": map[string]any{"secondary": map[string]any{
				"used_percent": 85.0, "window_minutes": 10080.0, "resets_at": float64(now.Add(72 * time.Hour).Unix()),
			}},
		},
	})
	// Pin mtimes so the newest log is scanned first regardless of write order.
	sessionsDir := filepath.Join(codexHome, "sessions", "2026", "06", "19")
	if err := os.Chtimes(filepath.Join(sessionsDir, "rollout-2026-06-19T11-30-00-newest.jsonl"), now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatalf("chtimes newest: %v", err)
	}
	if err := os.Chtimes(filepath.Join(sessionsDir, "rollout-2026-06-19T11-00-00-older.jsonl"), now.Add(-10*time.Minute), now.Add(-10*time.Minute)); err != nil {
		t.Fatalf("chtimes older: %v", err)
	}

	usage, ok := codexUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("expected usage")
	}
	session, weekly := usage.Metrics[0], usage.Metrics[1]
	if session.Unknown || session.Consumed == nil || *session.Consumed != 30 {
		t.Errorf("session metric=%+v, want observed 30%% from newest rollout", session)
	}
	if weekly.Unknown || weekly.Consumed == nil || *weekly.Consumed != 85 {
		t.Errorf("weekly metric=%+v, want most-constrained 85%% from the older log's stricter weekly limit", weekly)
	}
}

// Codex emits rate_limits: null between real readings; the parser must take the
// LAST populated frame, ignoring nulls and earlier values.
func TestCodexUsageParser_RolloutPrefersLastPopulatedFrame(t *testing.T) {
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "absent.json"))
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	helperCodexAuthAt(t, codexHome, "carol@example.com", codexTestLogin)

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

// A previous account's session that STARTED before the new account's login —
// but kept being appended afterward, so its file mtime is recent — must NOT
// backfill the new account's windows. Scoping by session start (not file mtime)
// is what closes this shared-CODEX_HOME bleed.
func TestCodexUsageParser_RolloutScopesBySessionStartNotMtime(t *testing.T) {
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "absent.json"))
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	// Prior account's session started at 08:00 — before the 10:00 login below.
	helperWriteRolloutLogAt(t, codexHome, "19", "2026-06-19T08-00-00-prior", "2026-06-19T08:00:00.000Z",
		[]map[string]any{codexRateLimitFrame(80, 70, now)})
	// …but it's still running, so its file mtime is recent (after the login).
	priorLog := filepath.Join(codexHome, "sessions", "2026", "06", "19", "rollout-2026-06-19T08-00-00-prior.jsonl")
	if err := os.Chtimes(priorLog, now, now); err != nil {
		t.Fatalf("chtimes rollout: %v", err)
	}
	// New account logs in at 10:00, after the prior session started.
	helperCodexAuthAt(t, codexHome, "newuser@example.com", time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC))

	usage, ok := codexUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("expected usage")
	}
	for _, m := range usage.Metrics {
		if !m.Unknown {
			t.Errorf("metric %q=%+v should stay Unknown; a pre-login session (recent mtime) must not backfill", m.Kind, m)
		}
	}
}

// A relative reset (`resets_in_seconds`) in a historical rollout line must be
// anchored to the line's own emit time, so a window whose reset has already
// passed becomes unobservable instead of showing stale usage with a bogus
// future reset or a misleading 0%.
func TestCodexUsageParser_RolloutAnchorsRelativeResetToEventTime(t *testing.T) {
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "absent.json"))
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	// Login before the historical session's 2026-06-15 start so it stays in scope.
	helperCodexAuthAt(t, codexHome, "carol@example.com", time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC))

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	// Emitted 4 days before `now` with a relative reset only 1h out → expired
	// long ago relative to now.
	frame := map[string]any{
		"primary": map[string]any{"used_percent": 50.0, "window_minutes": 300.0, "resets_in_seconds": 3600.0},
	}
	helperWriteRolloutLogAt(t, codexHome, "15", "2026-06-15T10-00-00-old", "2026-06-15T10:00:00.000Z",
		[]map[string]any{frame})

	usage, ok := codexUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("expected usage")
	}
	session := usage.Metrics[0]
	if !session.Unknown || session.Consumed != nil {
		t.Fatalf("rolled-over session should be unobservable, got %+v", session)
	}
	if session.ResetAt != "" {
		t.Errorf("session ResetAt=%q, want empty for a rolled-over window", session.ResetAt)
	}
}

// A later sparse token_count frame that restates only `primary` must not drop
// a `secondary` reading an earlier frame in the same file already captured.
func TestCodexUsageParser_RolloutMergesSparseFrames(t *testing.T) {
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "absent.json"))
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	helperCodexAuthAt(t, codexHome, "carol@example.com", codexTestLogin)

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	helperWriteRolloutLog(t, codexHome, "19", "2026-06-19T11-00-00-sparse", []map[string]any{
		// Full frame: both windows.
		codexRateLimitFrame(20, 40, now),
		// Sparse follow-up: only primary restated (no secondary key).
		{"primary": map[string]any{"used_percent": 25.0, "window_minutes": 300.0, "resets_at": float64(now.Add(time.Hour).Unix())}},
	})

	usage, ok := codexUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("expected usage")
	}
	session, weekly := usage.Metrics[0], usage.Metrics[1]
	if session.Consumed == nil || *session.Consumed != 25 {
		t.Errorf("session Consumed=%v, want 25 (latest primary)", session.Consumed)
	}
	if weekly.Unknown || weekly.Consumed == nil || *weekly.Consumed != 40 {
		t.Errorf("weekly metric=%+v, want 40 preserved from earlier frame", weekly)
	}
}

// camelCase telemetry (`rateLimits`, `usedPercent`, …) — which the extractor
// and live capture already accept — must survive the fallback prefilter too.
func TestCodexUsageParser_RolloutAcceptsCamelCaseFrames(t *testing.T) {
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "absent.json"))
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	helperCodexAuthAt(t, codexHome, "carol@example.com", codexTestLogin)

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	dir := filepath.Join(codexHome, "sessions", "2026", "06", "19")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	line, err := json.Marshal(map[string]any{
		"timestamp": "2026-06-19T11:00:00.000Z",
		"type":      "event_msg",
		"payload": map[string]any{
			"type": "token_count",
			"rateLimits": map[string]any{
				"primary":   map[string]any{"usedPercent": 15.0, "windowMinutes": 300.0, "resetsAt": float64(now.Add(time.Hour).Unix())},
				"secondary": map[string]any{"usedPercent": 8.0, "windowMinutes": 10080.0, "resetsAt": float64(now.Add(72 * time.Hour).Unix())},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-06-19T11-00-00-camel.jsonl"), append(line, '\n'), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	usage, ok := codexUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("expected usage")
	}
	session, weekly := usage.Metrics[0], usage.Metrics[1]
	if session.Unknown || session.Consumed == nil || *session.Consumed != 15 {
		t.Errorf("session metric=%+v, want 15 from camelCase rollout frame", session)
	}
	if weekly.Unknown || weekly.Consumed == nil || *weekly.Consumed != 8 {
		t.Errorf("weekly metric=%+v, want 8 from camelCase rollout frame", weekly)
	}
}

// When the newest rollout log only restated `primary`, a window still missing
// must be filled from a slightly older log that did carry it.
func TestCodexUsageParser_RolloutAccumulatesAcrossFiles(t *testing.T) {
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "absent.json"))
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	helperCodexAuthAt(t, codexHome, "carol@example.com", codexTestLogin)

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	// Older log carries both windows.
	helperWriteRolloutLog(t, codexHome, "18", "2026-06-18T10-00-00-older", []map[string]any{
		codexRateLimitFrame(10, 50, now),
	})
	// Newest log restates only primary.
	helperWriteRolloutLog(t, codexHome, "19", "2026-06-19T11-00-00-newer", []map[string]any{
		{"primary": map[string]any{"used_percent": 30.0, "window_minutes": 300.0, "resets_at": float64(now.Add(time.Hour).Unix())}},
	})

	usage, ok := codexUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("expected usage")
	}
	session, weekly := usage.Metrics[0], usage.Metrics[1]
	if session.Consumed == nil || *session.Consumed != 30 {
		t.Errorf("session Consumed=%v, want 30 (newest log wins)", session.Consumed)
	}
	if weekly.Unknown || weekly.Consumed == nil || *weekly.Consumed != 50 {
		t.Errorf("weekly metric=%+v, want 50 from older log", weekly)
	}
}

// The rollout accumulator must key by metric IDENTITY, not physical slot. When
// the newest log carries only a MIGRATED weekly under the `primary` slot
// (weekly-band minutes), a slot-keyed accumulator would lock `primary` and drop
// an older log's real `primary` session reading, leaving the session row Unknown.
// Identity keying keeps both, so session and weekly are each backfilled.
func TestCodexUsageParser_RolloutAccumulatesByIdentityNotSlot(t *testing.T) {
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "absent.json"))
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	helperCodexAuthAt(t, codexHome, "carol@example.com", codexTestLogin)

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	// Older log: a real 5-hour session reading under the primary slot.
	helperWriteRolloutLog(t, codexHome, "18", "2026-06-18T10-00-00-session", []map[string]any{
		{"primary": map[string]any{"used_percent": 55.0, "window_minutes": 300.0, "resets_at": float64(now.Add(time.Hour).Unix())}},
	})
	// Newest log: a weekly reading MIGRATED into the primary slot, and nothing
	// else. A slot-keyed accumulator would lock `primary` to this weekly.
	helperWriteRolloutLog(t, codexHome, "19", "2026-06-19T11-00-00-weeklymigrated", []map[string]any{
		{"primary": map[string]any{"used_percent": 22.0, "window_minutes": 10080.0, "resets_at": float64(now.Add(72 * time.Hour).Unix())}},
	})

	usage, ok := codexUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("expected usage")
	}
	session, weekly := usage.Metrics[0], usage.Metrics[1]
	if session.Unknown || session.Consumed == nil || *session.Consumed != 55 {
		t.Errorf("session metric=%+v, want 55 from older log (identity keying preserves it)", session)
	}
	if weekly.Unknown || weekly.Consumed == nil || *weekly.Consumed != 22 {
		t.Errorf("weekly metric=%+v, want 22 from migrated primary weekly", weekly)
	}
}

// A reset-only sparse frame that jumps to a NEW window must not carry the prior
// window's usage onto the fresh reset — drop the stale reading so the window is
// reported Unknown rather than the old percentage.
func TestCodexUsageParser_RolloutResetOnlyNewWindowDropsStaleUsage(t *testing.T) {
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "absent.json"))
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	helperCodexAuthAt(t, codexHome, "carol@example.com", codexTestLogin)

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	helperWriteRolloutLog(t, codexHome, "19", "2026-06-19T11-00-00-rollover", []map[string]any{
		// Window A: 60% used, resets in 1h.
		{"primary": map[string]any{"used_percent": 60.0, "window_minutes": 300.0, "resets_at": float64(now.Add(time.Hour).Unix())}},
		// Reset-only frame jumping to a new window 5h out (beyond jitter), no usage.
		{"primary": map[string]any{"window_minutes": 300.0, "resets_at": float64(now.Add(5 * time.Hour).Unix())}},
	})

	usage, ok := codexUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("expected usage")
	}
	session := usage.Metrics[0]
	if !session.Unknown {
		t.Errorf("session=%+v, want Unknown (stale usage dropped on new-window reset)", session)
	}
}

// A standalone reset-only frame (no usage to anchor it) must not be reported as
// observed 0% — it must leave the window open so an older log can fill the real
// usage.
func TestCodexUsageParser_RolloutIgnoresStandaloneResetOnlyFrame(t *testing.T) {
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "absent.json"))
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	helperCodexAuthAt(t, codexHome, "carol@example.com", codexTestLogin)

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	// Newest log: only a reset-only primary frame (no usage).
	helperWriteRolloutLogAt(t, codexHome, "19", "2026-06-19T11-30-00-resetonly", "2026-06-19T11:30:00.000Z",
		[]map[string]any{
			{"primary": map[string]any{"window_minutes": 300.0, "resets_at": float64(now.Add(time.Hour).Unix())}},
		})
	// Older log: real usage for both windows.
	helperWriteRolloutLogAt(t, codexHome, "19", "2026-06-19T10-00-00-real", "2026-06-19T10:00:00.000Z",
		[]map[string]any{codexRateLimitFrame(22, 44, now)})

	usage, ok := codexUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("expected usage")
	}
	session, weekly := usage.Metrics[0], usage.Metrics[1]
	if session.Unknown || session.Consumed == nil || *session.Consumed != 22 {
		t.Errorf("session=%+v, want 22 from the older real-usage log", session)
	}
	if weekly.Unknown || weekly.Consumed == nil || *weekly.Consumed != 44 {
		t.Errorf("weekly=%+v, want 44 from the older real-usage log", weekly)
	}
}

// A usage-only sparse frame that arrives after the prior reset has already
// expired must NOT inherit the prior expired reset — copying it forward stamps
// the fresh usage with a past resets_at, which codexObservedMetricOrUnknown then
// rolls down to 0% (the window appears "already rolled over"), hiding the new
// reading. The live cache merge only carries a prior reset when it is still
// live; mirror that here.
func TestCodexUsageParser_RolloutUsageOnlyAfterExpiredResetDoesNotInheritStaleReset(t *testing.T) {
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "absent.json"))
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	helperCodexAuthAt(t, codexHome, "carol@example.com", codexTestLogin)

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	dir := filepath.Join(codexHome, "sessions", "2026", "06", "19")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Two frames in one file with different per-line timestamps:
	//   t=10:00 — primary=50%, resets at 11:00 (already past by now=12:00 AND
	//             past the second frame's 11:30 event time).
	//   t=11:30 — primary=10%, no reset (sparse usage-only update).
	frames := []map[string]any{
		{
			"timestamp": "2026-06-19T10:00:00.000Z",
			"rl": map[string]any{
				"primary": map[string]any{"used_percent": 50.0, "window_minutes": 300.0, "resets_at": float64(now.Add(-time.Hour).Unix())},
			},
		},
		{
			"timestamp": "2026-06-19T11:30:00.000Z",
			"rl": map[string]any{
				"primary": map[string]any{"used_percent": 10.0, "window_minutes": 300.0},
			},
		},
	}
	var buf strings.Builder
	for _, f := range frames {
		line, err := json.Marshal(map[string]any{
			"timestamp": f["timestamp"],
			"type":      "event_msg",
			"payload": map[string]any{
				"type":        "token_count",
				"info":        map[string]any{"total_token_usage": map[string]any{"total_tokens": 1}},
				"rate_limits": f["rl"],
			},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-06-19T10-00-00-expired.jsonl"), []byte(buf.String()), 0o600); err != nil {
		t.Fatalf("write rollout log: %v", err)
	}

	usage, ok := codexUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("expected usage")
	}
	session := usage.Metrics[0]
	if session.Unknown || session.Consumed == nil || *session.Consumed != 10 {
		t.Errorf("session=%+v, want 10%% from the fresh usage-only frame (not rolled to 0%% by inherited expired reset)", session)
	}
	if session.ResetAt != "" {
		t.Errorf("session ResetAt=%q, want empty (the only candidate reset has already expired)", session.ResetAt)
	}
}

// When sessions overlap — an older-started but still-active session next to a
// newer-started but idle one — filename order alone would let the stale session
// "win" and stop the scan before the live reading is consulted. Ranking by file
// mtime lets the still-active session (the live source of truth) be considered
// first.
func TestCodexUsageParser_RolloutRanksByMtimeNotFilename(t *testing.T) {
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "absent.json"))
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	helperCodexAuthAt(t, codexHome, "carol@example.com", codexTestLogin)

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	// "Stale" session: newer by filename / session start (11:30) but no longer
	// being written. Its frame reads 80%.
	helperWriteRolloutLogAt(t, codexHome, "19", "2026-06-19T11-30-00-stale", "2026-06-19T11:30:00.000Z",
		[]map[string]any{codexRateLimitFrame(80, 70, now)})
	stalePath := filepath.Join(codexHome, "sessions", "2026", "06", "19", "rollout-2026-06-19T11-30-00-stale.jsonl")
	staleMtime := now.Add(-30 * time.Minute)
	if err := os.Chtimes(stalePath, staleMtime, staleMtime); err != nil {
		t.Fatalf("chtimes stale: %v", err)
	}
	// "Active" session: older by filename / session start (10:00) but its file
	// is the one being appended to right now. Its latest frame reads 30%.
	helperWriteRolloutLogAt(t, codexHome, "19", "2026-06-19T10-00-00-active", "2026-06-19T10:00:00.000Z",
		[]map[string]any{codexRateLimitFrame(30, 12, now)})
	activePath := filepath.Join(codexHome, "sessions", "2026", "06", "19", "rollout-2026-06-19T10-00-00-active.jsonl")
	if err := os.Chtimes(activePath, now, now); err != nil {
		t.Fatalf("chtimes active: %v", err)
	}

	usage, ok := codexUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("expected usage")
	}
	session, weekly := usage.Metrics[0], usage.Metrics[1]
	if session.Consumed == nil || *session.Consumed != 30 {
		t.Errorf("session Consumed=%v, want 30 from the still-active session (newer mtime), not 80 from the stale newer-filename session", session.Consumed)
	}
	if weekly.Consumed == nil || *weekly.Consumed != 12 {
		t.Errorf("weekly Consumed=%v, want 12 from the still-active session (newer mtime), not 70 from the stale newer-filename session", weekly.Consumed)
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
	// A bare identity-only fixture (no expires_at / token) must not raise a
	// false auth notice — we only warn on a DEFINITE expiry.
	if usage.Notice != "" {
		t.Errorf("unexpected notice for identity-only auth: %q", usage.Notice)
	}
}

// Grok's on-disk `auth.json` (current CLI) keys entries by
// "<oidc_issuer>::<client_id>" and stamps each with an RFC3339 `expires_at`.
func helperWriteGrokScopedAuth(t *testing.T, home string, expiresAt time.Time, extra map[string]any) {
	t.Helper()
	// Real installer-written scoped entries always carry the token under `key`
	// (what read_grok_token extracts); include one so the entry is treated as a
	// usable scope. `expires_at` still drives the notice (RFC3339 is read before
	// the JWT fallback in readGrokAuthExpiry).
	entry := map[string]any{
		"email": "rick@example.com",
		"key":   helperJWT(t, map[string]any{"email": "rick@example.com"}),
	}
	if !expiresAt.IsZero() {
		entry["expires_at"] = expiresAt.UTC().Format(time.RFC3339)
	}
	for k, v := range extra {
		entry[k] = v
	}
	helperWriteJSON(t, filepath.Join(home, ".grok", "auth.json"), map[string]any{
		grokExactOIDCScope: entry,
	})
}

func TestGrokUsageParser_AuthExpiredNotice(t *testing.T) {
	t.Setenv("GROK_HOME", "")
	home := t.TempDir()
	now := time.Now()
	helperWriteGrokScopedAuth(t, home, now.Add(-14*24*time.Hour), nil) // expired 2 weeks ago

	usage, ok := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !ok || usage == nil {
		t.Fatalf("expected usage entry")
	}
	if usage.NoticeSeverity != "error" {
		t.Errorf("NoticeSeverity=%q, want error", usage.NoticeSeverity)
	}
	if !strings.Contains(usage.Notice, "expired") || !strings.Contains(usage.Notice, "grok login") {
		t.Errorf("Notice=%q, want an expired re-login prompt", usage.Notice)
	}
}

func TestGrokUsageParser_AuthExpiringSoonNotice(t *testing.T) {
	t.Setenv("GROK_HOME", "")
	home := t.TempDir()
	now := time.Now()
	expiry := now.Add(6 * time.Hour)
	helperWriteGrokScopedAuth(t, home, expiry, nil) // within the 24h warn window

	usage, _ := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if usage.NoticeSeverity != "warning" {
		t.Errorf("NoticeSeverity=%q, want warning", usage.NoticeSeverity)
	}
	// The warning now names the exact expiry instant + a coarse time-left hint,
	// then the re-login instruction on its own line.
	wantWhen := expiry.Local().Format("1/2/2006, 3:04 PM")
	if !strings.Contains(usage.Notice, "Grok login expires "+wantWhen) {
		t.Errorf("Notice=%q, want the exact expiry time %q", usage.Notice, wantWhen)
	}
	if !strings.Contains(usage.Notice, "(6 hrs)") {
		t.Errorf("Notice=%q, want a '(6 hrs)' time-remaining hint", usage.Notice)
	}
	if !strings.Contains(usage.Notice, "\nRun `grok login`") {
		t.Errorf("Notice=%q, want the re-login instruction on a new line", usage.Notice)
	}
}

// A reached usage limit blocks requests right now, while an expiring-soon login
// is only a heads-up (the token still works) — so the reached-quota error must
// win over the auth warning instead of being suppressed by it.
func TestGrokUsageParser_ReachedLimitBeatsExpiringSoonAuth(t *testing.T) {
	t.Setenv("GROK_HOME", "")
	home := t.TempDir()
	now := time.Now()
	helperWriteGrokScopedAuth(t, home, now.Add(6*time.Hour), nil) // within the 24h warn window

	cache := filepath.Join(t.TempDir(), "grok_usage_limit.json")
	t.Setenv("AIEXPEDITE_GROK_LIMIT_CACHE", cache)
	helperWriteJSON(t, cache, map[string]any{
		"severity":           grokLimitReached,
		"message":            "Grok usage limit reached — upgrade to keep going.",
		"upgradeUrl":         "https://x.ai/upgrade",
		"observedAt":         now.UTC().Format(time.RFC3339),
		"observedAtMs":       now.UnixMilli(),
		"accountFingerprint": fingerprintAccount("grok", "rick@example.com"),
	})

	usage, ok := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !ok || usage == nil {
		t.Fatalf("expected usage entry")
	}
	if usage.NoticeSeverity != "error" {
		t.Errorf("NoticeSeverity=%q, want error (reached quota must not be hidden by an expiring-soon auth warning)", usage.NoticeSeverity)
	}
	if !strings.Contains(usage.Notice, "usage limit reached") {
		t.Errorf("Notice=%q, want the reached usage-limit banner, not the re-login heads-up", usage.Notice)
	}
	if usage.NoticeURL != "https://x.ai/upgrade" {
		t.Errorf("NoticeURL=%q, want the upgrade URL preserved from the cached limit state", usage.NoticeURL)
	}
}

// Expired/missing auth DOES block every request and can't be re-established
// headlessly, so it still takes precedence even over a reached usage limit.
func TestGrokUsageParser_ExpiredAuthBeatsReachedLimit(t *testing.T) {
	t.Setenv("GROK_HOME", "")
	home := t.TempDir()
	now := time.Now()
	helperWriteGrokScopedAuth(t, home, now.Add(-time.Hour), nil) // already expired

	cache := filepath.Join(t.TempDir(), "grok_usage_limit.json")
	t.Setenv("AIEXPEDITE_GROK_LIMIT_CACHE", cache)
	helperWriteJSON(t, cache, map[string]any{
		"severity":           grokLimitReached,
		"message":            "Grok usage limit reached — upgrade to keep going.",
		"observedAt":         now.UTC().Format(time.RFC3339),
		"observedAtMs":       now.UnixMilli(),
		"accountFingerprint": fingerprintAccount("grok", "rick@example.com"),
	})

	usage, _ := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if usage.NoticeSeverity != "error" || !strings.Contains(usage.Notice, "expired") {
		t.Errorf("Notice=%q sev=%q, want the expired re-login prompt to win — it blocks requests and can't be fixed headlessly", usage.Notice, usage.NoticeSeverity)
	}
}

func TestGrokUsageParser_AuthValidNoNotice(t *testing.T) {
	t.Setenv("GROK_HOME", "")
	home := t.TempDir()
	now := time.Now()
	helperWriteGrokScopedAuth(t, home, now.Add(30*24*time.Hour), nil) // valid for a month

	usage, _ := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if usage.Notice != "" {
		t.Errorf("expected no auth notice for a valid token, got %q", usage.Notice)
	}
}

func TestGrokUsageParser_RefreshTokenMakesAccessExpiryNonAuthoritative(t *testing.T) {
	t.Setenv("GROK_HOME", "")
	home := t.TempDir()
	now := time.Now()
	// Grok v1.0: the access JWT is short-lived/expired, but the opaque refresh
	// token keeps the login renewable without another browser sign-in.
	helperWriteGrokScopedAuth(t, home, now.Add(-time.Hour), map[string]any{
		"refresh_token": "opaque-refresh-token",
	})

	usage, _ := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if usage.Notice != "" || usage.NoticeSeverity != "" {
		t.Errorf("refreshable login must not be called expired: notice=%q severity=%q", usage.Notice, usage.NoticeSeverity)
	}
	if !grokHasUsableToken(filepath.Join(home, ".grok")) {
		t.Errorf("refresh token should count as usable auth")
	}
	if usage.Authenticated == nil || !*usage.Authenticated || usage.AuthState != "authenticated" {
		t.Errorf("refreshable Grok login should be authenticated: %+v", usage)
	}
	if usage.LoginExpirationState != loginExpirationRefreshable || usage.LoginExpiresAt != "" {
		t.Errorf("access expiry must not be published as login expiry: %+v", usage)
	}
}

func TestGrokUsageParser_AuthMissingNotice(t *testing.T) {
	t.Setenv("GROK_HOME", "")
	home := t.TempDir() // no ~/.grok/auth.json at all
	now := time.Now()

	usage, ok := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !ok || usage == nil {
		t.Fatalf("expected a usage entry even without auth")
	}
	if usage.NoticeSeverity != "error" || !strings.Contains(usage.Notice, "not signed in") {
		t.Errorf("Notice=%q sev=%q, want a 'not signed in' error", usage.Notice, usage.NoticeSeverity)
	}
	if usage.Authenticated == nil || *usage.Authenticated || usage.AuthState != "missing" {
		t.Errorf("missing Grok auth state = (%v, %q), want false/missing", usage.Authenticated, usage.AuthState)
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
		grokExactOIDCScope: map[string]any{
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

// TestGrokUsageParser_UnrelatedScopedTokenDoesNotAuthenticate pins that auth
// entries belonging to another client are not credentials Grok's resolver can
// present. Their presence must not make this CLI appear signed in.
func TestGrokUsageParser_UnrelatedScopedTokenDoesNotAuthenticate(t *testing.T) {
	t.Setenv("GROK_HOME", "")
	now := time.Now()
	for name, scope := range map[string]string{
		"other OIDC client":   "https://auth.x.ai::another-client",
		"other legacy client": "https://accounts.x.ai/another-client",
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			helperWriteJSON(t, filepath.Join(home, ".grok", "auth.json"), map[string]any{
				scope: map[string]any{
					"email":      "other-client@example.com",
					"key":        helperJWT(t, map[string]any{"email": "other-client@example.com"}),
					"expires_at": now.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
				},
			})

			usage, ok := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
			if !ok || usage == nil {
				t.Fatalf("expected usage entry")
			}
			if usage.Authenticated == nil || *usage.Authenticated || usage.AuthState != "missing" {
				t.Errorf("unrelated scoped auth state = (%v, %q), want false/missing", usage.Authenticated, usage.AuthState)
			}
			if usage.NoticeSeverity != "error" || !strings.Contains(usage.Notice, "not signed in") {
				t.Errorf("Notice=%q sev=%q, want a not-signed-in prompt for an unrelated scoped token", usage.Notice, usage.NoticeSeverity)
			}
		})
	}
}

// TestGrokUsageParser_ScopedExpiryPrefersOIDCOverLegacy pins Grok's REAL scope
// precedence with the official scope keys. The legacy sign-in scope
// ("https://accounts.x.ai/sign-in") sorts alphabetically BEFORE the OIDC scope
// ("https://auth.x.ai::..."), but read_grok_token in x.ai/cli/install.sh resolves
// OIDC first and only falls back to legacy. So a valid legacy sibling must NOT
// mask an expired OIDC token: expiry must be read from the OIDC entry Grok will
// actually present, surfacing the re-login warning.
func TestGrokUsageParser_ScopedExpiryPrefersOIDCOverLegacy(t *testing.T) {
	t.Setenv("GROK_HOME", "")
	home := t.TempDir()
	now := time.Now()
	helperWriteJSON(t, filepath.Join(home, ".grok", "auth.json"), map[string]any{
		// OIDC scope (resolved first) — expired two weeks ago.
		"https://auth.x.ai::b1a00492-073a-47ea-816f-4c329264a828": map[string]any{
			"email":      "scoped-grok@example.com",
			"key":        helperJWT(t, map[string]any{"email": "scoped-grok@example.com"}),
			"expires_at": now.Add(-14 * 24 * time.Hour).UTC().Format(time.RFC3339),
		},
		// Legacy scope — sorts first alphabetically and is still valid, but Grok
		// only falls back to it when the OIDC token is absent.
		"https://accounts.x.ai/sign-in": map[string]any{
			"email":      "scoped-grok@example.com",
			"key":        helperJWT(t, map[string]any{"email": "scoped-grok@example.com"}),
			"expires_at": now.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
		},
	})

	usage, ok := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !ok || usage == nil {
		t.Fatalf("expected usage entry")
	}
	if usage.NoticeSeverity != "error" || !strings.Contains(usage.Notice, "expired") {
		t.Errorf("Notice=%q sev=%q, want an expired re-login prompt from the OIDC scope, not a healthy read from the legacy sibling", usage.Notice, usage.NoticeSeverity)
	}
}

// TestGrokUsageParser_ScopedExpirySkipsTokenlessEntry pins that a preferred
// (OIDC) scope left as metadata with an empty key does NOT get its `expires_at`
// read: read_grok_token only treats a scope as usable when it can extract a
// non-empty token, so the health must come from the legacy entry that actually
// carries the token Grok will present. Here the tokenless OIDC entry looks valid
// for a month while the real (legacy) token is expired — the expired warning
// must still surface.
func TestGrokUsageParser_ScopedExpirySkipsTokenlessEntry(t *testing.T) {
	t.Setenv("GROK_HOME", "")
	home := t.TempDir()
	now := time.Now()
	helperWriteJSON(t, filepath.Join(home, ".grok", "auth.json"), map[string]any{
		// OIDC scope (resolved first) but tokenless — only metadata + a fresh
		// expires_at. Grok can't use it, so its expiry must be ignored.
		"https://auth.x.ai::b1a00492-073a-47ea-816f-4c329264a828": map[string]any{
			"email":      "scoped-grok@example.com",
			"expires_at": now.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
		},
		// Legacy scope carries the token Grok actually presents — and it is expired.
		"https://accounts.x.ai/sign-in": map[string]any{
			"email":      "scoped-grok@example.com",
			"key":        helperJWT(t, map[string]any{"email": "scoped-grok@example.com"}),
			"expires_at": now.Add(-14 * 24 * time.Hour).UTC().Format(time.RFC3339),
		},
	})

	usage, ok := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !ok || usage == nil {
		t.Fatalf("expected usage entry")
	}
	if usage.NoticeSeverity != "error" || !strings.Contains(usage.Notice, "expired") {
		t.Errorf("Notice=%q sev=%q, want an expired re-login prompt from the token-bearing legacy scope, not a healthy read from the tokenless OIDC entry", usage.Notice, usage.NoticeSeverity)
	}
}

// TestGrokUsageParser_ScopedExpiryStopsAtSelectedScopeWithUnknownExpiry pins that
// once the preferred token-bearing scope is selected, an UNREADABLE expiry there
// (opaque key: no `expires_at`, no JWT `exp`) reports unknown instead of falling
// through to a lower-precedence sibling. read_grok_token stops at the first
// non-empty token, so consulting the legacy sibling's expired timestamp would
// surface a false expired-login error for a token Grok never presents. Unknown
// expiry + a readable account must stay quiet (no notice).
func TestGrokUsageParser_ScopedExpiryStopsAtSelectedScopeWithUnknownExpiry(t *testing.T) {
	t.Setenv("GROK_HOME", "")
	home := t.TempDir()
	now := time.Now()
	helperWriteJSON(t, filepath.Join(home, ".grok", "auth.json"), map[string]any{
		// OIDC scope (resolved first) carries an opaque, non-JWT key with no
		// `expires_at` — its expiry is unknown, but it IS the token Grok presents.
		"https://auth.x.ai::b1a00492-073a-47ea-816f-4c329264a828": map[string]any{
			"email": "scoped-grok@example.com",
			"key":   "opaque-non-jwt-access-token",
		},
		// Legacy scope is expired, but Grok never falls back to it while the OIDC
		// token exists — so its expiry must NOT drive the notice.
		"https://accounts.x.ai/sign-in": map[string]any{
			"email":      "scoped-grok@example.com",
			"key":        helperJWT(t, map[string]any{"email": "scoped-grok@example.com"}),
			"expires_at": now.Add(-14 * 24 * time.Hour).UTC().Format(time.RFC3339),
		},
	})

	usage, ok := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !ok || usage == nil {
		t.Fatalf("expected usage entry")
	}
	if usage.Notice != "" {
		t.Errorf("Notice=%q, want no notice — the selected OIDC scope's expiry is unknown, so we must not borrow the legacy sibling's expired timestamp", usage.Notice)
	}
}

// TestGrokUsageParser_OpaqueTokenWithoutIdentityStaysSignedIn pins that a scoped
// entry with a usable but opaque token — no `expires_at`, no JWT `exp`, and no
// parseable identity — does NOT surface a false "not signed in" error. Grok's
// resolver would still present the token, so the best-effort check must stay
// quiet rather than telling the user to re-run `grok login`.
func TestGrokUsageParser_OpaqueTokenWithoutIdentityStaysSignedIn(t *testing.T) {
	t.Setenv("GROK_HOME", "")
	home := t.TempDir()
	now := time.Now()
	helperWriteJSON(t, filepath.Join(home, ".grok", "auth.json"), map[string]any{
		// Opaque, non-JWT key with no `expires_at` and no identity claims: expiry
		// is unknown AND the account can't be parsed, yet the token IS present and
		// is what Grok would present on each request.
		grokExactOIDCScope: map[string]any{
			"key": "opaque-non-jwt-access-token",
		},
	})

	usage, ok := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !ok || usage == nil {
		t.Fatalf("expected usage entry")
	}
	if usage.Notice != "" {
		t.Errorf("Notice=%q, want no notice — a usable-but-opaque token must not read as 'not signed in'", usage.Notice)
	}
}

// TestGrokUsageParser_CachedTokenPrefersAccessTokenExpiry pins that a legacy
// cached_token file carrying BOTH tokens reads the expiry from the access token
// — the credential Grok actually presents on each request — not the id_token.
// Here the access_token is already expired while the id_token still has a later
// exp; the expired re-login warning must surface instead of a healthy read off
// the id_token.
func TestGrokUsageParser_CachedTokenPrefersAccessTokenExpiry(t *testing.T) {
	t.Setenv("GROK_HOME", "")
	home := t.TempDir()
	now := time.Now()
	helperWriteJSON(t, filepath.Join(home, ".grok", "cached_token.json"), map[string]any{
		"cached_token": map[string]any{
			// id_token is still valid for a month — must NOT drive the notice.
			"id_token": helperJWT(t, map[string]any{
				"email": "oauth-grok@example.com",
				"exp":   now.Add(30 * 24 * time.Hour).Unix(),
			}),
			// access_token is the credential Grok presents — and it is expired.
			"access_token": helperJWT(t, map[string]any{
				"email": "oauth-grok@example.com",
				"exp":   now.Add(-14 * 24 * time.Hour).Unix(),
			}),
		},
	})

	usage, ok := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !ok || usage == nil {
		t.Fatalf("expected usage entry")
	}
	if usage.NoticeSeverity != "error" || !strings.Contains(usage.Notice, "expired") {
		t.Errorf("Notice=%q sev=%q, want an expired re-login prompt from the presented access token, not a healthy read off the later-expiring id_token", usage.Notice, usage.NoticeSeverity)
	}
}

// TestGrokUsageParser_FlatRefreshTokenSuppressesAccessExpiry pins both legacy
// auth layouts. A renewable access-token deadline is not a login expiry and
// must never tell the user to sign in again.
func TestGrokUsageParser_FlatRefreshTokenSuppressesAccessExpiry(t *testing.T) {
	t.Setenv("GROK_HOME", "")
	now := time.Now()
	expiredAccessToken := helperJWT(t, map[string]any{
		"email": "oauth-grok@example.com",
		"exp":   now.Add(-time.Hour).Unix(),
	})

	tests := map[string]map[string]any{
		"top-level": {
			"email":         "oauth-grok@example.com",
			"access_token":  expiredAccessToken,
			"refresh_token": "renewable-top-level",
			"expires_at":    now.Add(-time.Hour).UTC().Format(time.RFC3339),
		},
		"nested cached_token": {
			"cached_token": map[string]any{
				"access_token":  expiredAccessToken,
				"refresh_token": "renewable-nested",
			},
			"expires_at": now.Add(-time.Hour).UTC().Format(time.RFC3339),
		},
	}

	for name, auth := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			helperWriteJSON(t, filepath.Join(home, ".grok", "auth.json"), auth)

			usage, ok := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
			if !ok || usage == nil {
				t.Fatalf("expected usage entry")
			}
			if usage.Authenticated == nil || !*usage.Authenticated || usage.AuthState != "authenticated" {
				t.Errorf("renewable auth state = (%v, %q), want true/authenticated", usage.Authenticated, usage.AuthState)
			}
			if usage.LoginExpirationState != loginExpirationRefreshable || usage.LoginExpiresAt != "" {
				t.Errorf("login expiry = (%q, %q), want refreshable with no deadline", usage.LoginExpirationState, usage.LoginExpiresAt)
			}
			if usage.Notice != "" {
				t.Errorf("Notice=%q, want no re-login notice for renewable credentials", usage.Notice)
			}
		})
	}
}

// TestGrokUsageParser_ScopedExpiryPrefersExactCLIScopeOverSiblingOIDCHost pins
// that the EXACT CLI OIDC scope outranks an unrelated sibling that merely shares
// the "https://auth.x.ai" host. read_grok_token resolves the CLI's own
// OIDC_SCOPE, not any auth.x.ai key, so a stale token for a different xAI client
// (e.g. "https://auth.x.ai::00000000-...", which sorts alphabetically first)
// must NOT mask the health of the CLI credential Grok will actually present.
// Here the sibling OIDC-host entry is expired while the exact CLI scope is valid
// — no notice must surface.
func TestGrokUsageParser_ScopedExpiryPrefersExactCLIScopeOverSiblingOIDCHost(t *testing.T) {
	t.Setenv("GROK_HOME", "")
	home := t.TempDir()
	now := time.Now()
	helperWriteJSON(t, filepath.Join(home, ".grok", "auth.json"), map[string]any{
		// A different xAI client sharing the auth.x.ai host — sorts first
		// alphabetically (all-zero UUID) and is expired, but Grok never presents it.
		"https://auth.x.ai::00000000-0000-0000-0000-000000000000": map[string]any{
			"email":      "scoped-grok@example.com",
			"key":        helperJWT(t, map[string]any{"email": "scoped-grok@example.com"}),
			"expires_at": now.Add(-14 * 24 * time.Hour).UTC().Format(time.RFC3339),
		},
		// The CLI's exact OIDC scope — the credential Grok presents — is valid.
		"https://auth.x.ai::b1a00492-073a-47ea-816f-4c329264a828": map[string]any{
			"email":      "scoped-grok@example.com",
			"key":        helperJWT(t, map[string]any{"email": "scoped-grok@example.com"}),
			"expires_at": now.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
		},
	})

	usage, ok := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !ok || usage == nil {
		t.Fatalf("expected usage entry")
	}
	if usage.Notice != "" {
		t.Errorf("Notice=%q, want no notice — the exact CLI OIDC scope is valid; an unrelated expired auth.x.ai sibling must not drive the expiry", usage.Notice)
	}
}

// TestGrokUsageParser_ScopedExpiryPrefersLegacyOverUnrelatedOIDCSibling pins that
// when there is NO exact CLI OIDC entry, the legacy sign-in scope outranks an
// unrelated "https://auth.x.ai::<other-client>" sibling. read_grok_token resolves
// only OIDC_SCOPE then LEGACY_SCOPE (x.ai/cli/install.sh) — it never scans other
// auth.x.ai keys — so a valid sibling for a different xAI client must NOT mask an
// expired legacy token Grok will actually fall back to. Here the sibling is fresh
// while the legacy token is expired: the expired warning must still surface.
func TestGrokUsageParser_ScopedExpiryPrefersLegacyOverUnrelatedOIDCSibling(t *testing.T) {
	t.Setenv("GROK_HOME", "")
	home := t.TempDir()
	now := time.Now()
	helperWriteJSON(t, filepath.Join(home, ".grok", "auth.json"), map[string]any{
		// A different xAI client sharing the auth.x.ai host — valid, but Grok's
		// resolver never presents it (only the exact OIDC scope, then legacy).
		"https://auth.x.ai::00000000-0000-0000-0000-000000000000": map[string]any{
			"email":      "scoped-grok@example.com",
			"key":        helperJWT(t, map[string]any{"email": "scoped-grok@example.com"}),
			"expires_at": now.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
		},
		// Legacy scope carries the token Grok falls back to — and it is expired.
		"https://accounts.x.ai/sign-in": map[string]any{
			"email":      "scoped-grok@example.com",
			"key":        helperJWT(t, map[string]any{"email": "scoped-grok@example.com"}),
			"expires_at": now.Add(-14 * 24 * time.Hour).UTC().Format(time.RFC3339),
		},
	})

	usage, ok := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !ok || usage == nil {
		t.Fatalf("expected usage entry")
	}
	if usage.NoticeSeverity != "error" || !strings.Contains(usage.Notice, "expired") {
		t.Errorf("Notice=%q sev=%q, want an expired re-login prompt from the legacy scope Grok falls back to, not a healthy read from the unrelated OIDC-host sibling", usage.Notice, usage.NoticeSeverity)
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
		if u.Provider == "antigravity" {
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
	isolateTestUserHome(t, home)
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
	isolateTestUserHome(t, t.TempDir())
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
	isolateTestUserHome(t, t.TempDir())
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

// Finding 1 (identity-gated rollout recovery): a weekly reading that migrated
// into the PRIMARY storage slot with a reset already in the past rolls over to a
// stale concrete 0% (Unknown=false). A slot-keyed rolled-over check would look
// for weekly under `secondary`, find nothing, and leave the bogus 0% on the card
// forever. The identity-gated check must recognise the migrated weekly as rolled
// over and refill it from the fresher rollout reading.
func TestCodexUsageParser_RolloutFillsRolledOverMigratedWeekly(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	helperCodexAuthAt(t, codexHome, "carol@example.com", codexTestLogin)

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	fp := fingerprintAccount("codex", "carol@example.com")
	// Weekly-band reading physically under the primary slot, reset already passed.
	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary: {
			UsedPercentage: 55, ResetsAtMs: now.Add(-time.Hour).UnixMilli(),
			WindowMinutes: 10080, usageKnown: true, resetKnown: true,
		},
	}, nil, now.Add(-2*time.Hour), fp)
	// Rollout carries fresher weekly usage in the current window (weekly only).
	helperWriteRolloutLog(t, codexHome, "19", "2026-06-19T11-00-00-migrated", []map[string]any{
		{"secondary": map[string]any{
			"used_percent":   18.0,
			"window_minutes": 10080.0,
			"resets_at":      float64(now.Add(96 * time.Hour).Unix()),
		}},
	})

	usage, ok := codexUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("expected usage")
	}
	session, weekly := usage.Metrics[0], usage.Metrics[1]
	if !session.Unknown {
		t.Errorf("session metric=%+v, want Unknown (no session reading; weekly-band must not fill it)", session)
	}
	if weekly.Unknown || weekly.Consumed == nil || *weekly.Consumed != 18 {
		t.Errorf("weekly metric=%+v, want 18 refilled from rollout (stale rolled-over migrated weekly must not stick at 0%%)", weekly)
	}
}
