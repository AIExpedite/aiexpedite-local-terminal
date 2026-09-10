package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The frame every case below feeds the capture path: a reached usage-limit
// signal in the shape Grok's streaming-json / ACP stdout carries it.
const grokLimitScopeTestFrame = `{"type":"session/update","update":{"sessionUpdate":"usage_limit_reached","gate_message":"You've hit your Grok limit."}}`

// TestGrokLimitNoticeScope_ContestedProducerCachesNothing pins the reason the
// scope exists: a direct run whose producer grokDirectRunBillingIdentity
// contests (an inherited XAI_API_KEY, a key pinned in argv or config) bills an
// account the cached login does not name. Filing its limit under that login
// would show one account's limit on another's card for grokLimitNoticeTTL.
func TestGrokLimitNoticeScope_ContestedProducerCachesNothing(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "grok_usage_limit.json")
	t.Setenv("AIEXPEDITE_GROK_LIMIT_CACHE", cache)

	scope := grokDirectRunLimitNoticeScope(grokContestedBillingIdentity, t.TempDir())
	if scope.cacheable {
		t.Fatalf("a contested direct arm must not be cacheable: %+v", scope)
	}

	captureGrokUsageLimitLine(grokLimitScopeTestFrame, time.Now(), scope)
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("contested producer must write no notice cache: %v", err)
	}
}

// TestGrokLimitNoticeScope_ContestedRunLeavesALiveNoticeIntact is the second
// half of the same hazard, and the reason a contested run drops the notice
// rather than caching it under some other key: writeGrokUsageLimitState
// discards prior state whose fingerprint differs, so caching a contested notice
// would also blank the still-live reached state the signed-in account earned.
func TestGrokLimitNoticeScope_ContestedRunLeavesALiveNoticeIntact(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "grok_usage_limit.json")
	t.Setenv("AIEXPEDITE_GROK_LIMIT_CACHE", cache)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	writeGrokUsageLimitState(cache, grokUsageLimitState{
		Severity:     grokLimitReached,
		Message:      "signed-in account's own limit",
		ObservedAt:   now.Format(time.RFC3339),
		ObservedAtMs: now.UnixMilli(),
	}, "signed-in-fingerprint")

	captureGrokUsageLimitLine(grokLimitScopeTestFrame, now.Add(time.Minute), grokLimitNoticeScope{})

	got, ok := loadGrokUsageLimitState("signed-in-fingerprint", now.Add(time.Minute))
	if !ok {
		t.Fatalf("the signed-in account's live notice must survive a contested run")
	}
	if got.Message != "signed-in account's own limit" {
		t.Fatalf("notice was overwritten by a contested run: %+v", got)
	}
}

// TestGrokLimitNoticeScope_FrozenAtSessionStart pins that the scope names the
// home the child actually runs under and does not follow a later `grok login`.
// A managed ACP session keeps billing the login copied into its isolated home
// for its whole life, so a mid-session re-login of the persistent home must not
// re-file this session's notices under the new account.
func TestGrokLimitNoticeScope_FrozenAtSessionStart(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "grok_usage_limit.json")
	t.Setenv("AIEXPEDITE_GROK_LIMIT_CACHE", cache)

	producer := t.TempDir()
	seedGrokHomeWithLogin(t, producer)
	scope := grokAccountLimitNoticeScope(producer)
	if !scope.cacheable || scope.fingerprint == "" {
		t.Fatalf("a resolvable producer home must yield a cacheable scope: %+v", scope)
	}
	want := scope.fingerprint

	// The ambient home is re-pointed at a DIFFERENT account after the freeze —
	// exactly what an out-of-band `grok login` does mid-session.
	other := t.TempDir()
	helperGrokScopedAuth(t, other, map[string]any{
		"key":           unsignedJWT(t, map[string]any{"email": "grace@example.com", "sub": "user-2"}),
		"email":         "grace@example.com",
		"refresh_token": "refresh-token-value",
		"expires_at":    time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339),
	})
	t.Setenv("GROK_HOME", other)
	if live := currentGrokAccountFingerprint(); live == want {
		t.Fatalf("test setup: the two accounts must fingerprint differently (%q)", live)
	}

	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	captureGrokUsageLimitLine(grokLimitScopeTestFrame, now, scope)

	if _, ok := loadGrokUsageLimitState(currentGrokAccountFingerprint(), now); ok {
		t.Fatalf("the notice must not be filed under the account that logged in after the freeze")
	}
	got, ok := loadGrokUsageLimitState(want, now)
	if !ok {
		t.Fatalf("the notice must be filed under the producing account")
	}
	if got.Severity != grokLimitReached {
		t.Fatalf("Severity=%q, want %q", got.Severity, grokLimitReached)
	}
}
