package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func enableTestGrokLogin(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	seedGrokHomeWithLogin(t, home)
	t.Setenv("GROK_HOME", home)
	t.Setenv("XAI_API_KEY", "")
}

func seedGrokHomeWithLogin(t *testing.T, base string) {
	t.Helper()
	helperGrokScopedAuth(t, base, map[string]any{
		"key":           unsignedJWT(t, map[string]any{"email": "ada@example.com", "sub": "user-1"}),
		"email":         "ada@example.com",
		"refresh_token": "refresh-token-value",
		"expires_at":    time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339),
	})
}

func TestAssessGrokAuth_Missing(t *testing.T) {
	base := t.TempDir()
	got := assessGrokAuth(base, time.Now(), false, "grok-build")
	if got.Authenticated {
		t.Fatalf("empty home must be unauthenticated: %+v", got)
	}
	if got.AuthState != grokAuthStateMissing {
		t.Errorf("AuthState=%q, want missing", got.AuthState)
	}
	if got.ReasonCode != grokNotAuthenticatedCode {
		t.Errorf("ReasonCode=%q", got.ReasonCode)
	}
	if strings.Contains(strings.ToLower(got.Reason), "refresh-token") ||
		strings.Contains(got.Reason, "xai-") {
		t.Errorf("assessment leaked a secret: %q", got.Reason)
	}
}

func TestAssessGrokAuth_ValidCachedLogin(t *testing.T) {
	base := t.TempDir()
	seedGrokHomeWithLogin(t, base)
	got := assessGrokAuth(base, time.Now(), false, "grok-build")
	if !got.Authenticated || got.Source != grokAuthSourceCachedLogin {
		t.Fatalf("want cached-login authenticated, got %+v", got)
	}
	if got.ReasonCode != "" {
		t.Errorf("healthy login must not carry ReasonCode: %+v", got)
	}
}

func TestAssessGrokAuth_ExpiredWithoutRefresh(t *testing.T) {
	base := t.TempDir()
	helperGrokScopedAuth(t, base, map[string]any{
		"key":        unsignedJWT(t, map[string]any{"email": "ada@example.com"}),
		"email":      "ada@example.com",
		"expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	})
	got := assessGrokAuth(base, time.Now(), false, "grok-build")
	if got.Authenticated || got.AuthState != grokAuthStateExpired {
		t.Fatalf("want expired, got %+v", got)
	}
	if got.ReasonCode != grokNotAuthenticatedCode {
		t.Errorf("ReasonCode=%q", got.ReasonCode)
	}
}

func TestAssessGrokAuth_APIKeyFallback(t *testing.T) {
	base := t.TempDir()
	cfg := "[cli]\ninstaller = \"internal\"\n\n[model]\napi_key = \"xai-test-secret-key\"\n"
	if err := os.WriteFile(filepath.Join(base, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	got := assessGrokAuth(base, time.Now(), true, "grok-build")
	if !got.Authenticated || got.Source != grokAuthSourceAPIKey {
		t.Fatalf("want api-key authenticated, got %+v", got)
	}
	if strings.Contains(got.Reason, "xai-test-secret-key") || strings.Contains(got.Source, "xai-") {
		t.Fatalf("api key leaked: %+v", got)
	}
}

func TestAssessGrokAuth_APIKeyDisabledDoesNotAuthenticate(t *testing.T) {
	base := t.TempDir()
	cfg := "[model]\napi_key = \"xai-test-secret-key\"\n"
	if err := os.WriteFile(filepath.Join(base, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	got := assessGrokAuth(base, time.Now(), false, "grok-build")
	if got.Authenticated {
		t.Fatalf("fallback disabled must not use api key: %+v", got)
	}
}

func TestAssessIsolatedGrokLaunch_UnknownFailsClosed(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "auth.json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := assessIsolatedGrokLaunch(base, time.Now(), false, "grok-build")
	if got.Authenticated || got.ReasonCode != grokNotAuthenticatedCode {
		t.Fatalf("unknown format must fail closed at launch: %+v", got)
	}
}

func TestGrokACPManager_StartRefusesUnauthenticatedWithoutSpawn(t *testing.T) {
	emptyHome := t.TempDir()
	t.Setenv("GROK_HOME", emptyHome)
	t.Setenv("XAI_API_KEY", "")

	m := NewGrokACPManager()
	err := m.Start("no-auth", t.TempDir(), nil, "ws", "uid", GrokStartOptions{}, func(resultMsg) {})
	if err == nil {
		t.Fatal("expected GROK_NOT_AUTHENTICATED")
	}
	typed := grokAuthErrorFrom(err)
	if typed == nil || typed.Code != grokNotAuthenticatedCode {
		t.Fatalf("want typed grok auth error, got %v", err)
	}
	if m.ActiveCount() != 0 {
		t.Errorf("session must not be registered: %d", m.ActiveCount())
	}
}
