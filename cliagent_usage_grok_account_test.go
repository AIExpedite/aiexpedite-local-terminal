package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// helperGrokScopedAuth writes an auth.json in the installer's scoped shape: the
// top level is keyed by auth scope, and each value wraps a token alongside the
// plain identity fields Grok records for that login.
func helperGrokScopedAuth(t *testing.T, base string, entry map[string]any) string {
	t.Helper()
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", base, err)
	}
	body, err := json.Marshal(map[string]any{
		"https://auth.x.ai::b1a00492-073a-47ea-816f-4c329264a828": entry,
	})
	if err != nil {
		t.Fatalf("marshal auth: %v", err)
	}
	path := filepath.Join(base, "auth.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
	return path
}

// unsignedJWT builds a token whose payload carries the given claims. Grok's real
// token is signed, but nothing here verifies the signature — only the payload is
// read — so an unsigned one exercises the same decode path.
func unsignedJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." +
		enc.EncodeToString(payload) + "."
}

func TestReadGrokAccountAndPlan_PrefersEntryEmailOverTokenSubject(t *testing.T) {
	// The regression: Grok's JWT carries no email claim, so the account fell
	// through to `sub` and the card showed a bare UUID while the address sat in
	// plain sight in the same auth entry.
	base := t.TempDir()
	helperGrokScopedAuth(t, base, map[string]any{
		"key":           unsignedJWT(t, map[string]any{"sub": "baee20e5-1e5d-4dc3-aea5-224fae90e54c"}),
		"email":         "ada@example.com",
		"user_id":       "baee20e5-1e5d-4dc3-aea5-224fae90e54c",
		"refresh_token": "refresh-token-value",
		"expires_at":    "2026-08-12T18:00:00Z",
	})

	account, _ := readGrokAccountAndPlan(base)
	if account != "ada@example.com" {
		t.Errorf("account=%q, want %q", account, "ada@example.com")
	}
}

func TestReadGrokAccountAndPlan_SignedClaimWinsOverSiblingField(t *testing.T) {
	// A claim is signed; a sibling field is not. When they disagree the token
	// is the authority — otherwise an edited auth.json could relabel the card.
	base := t.TempDir()
	helperGrokScopedAuth(t, base, map[string]any{
		"key":   unsignedJWT(t, map[string]any{"email": "signed@example.com", "sub": "uuid"}),
		"email": "tampered@example.com",
	})

	account, _ := readGrokAccountAndPlan(base)
	if account != "signed@example.com" {
		t.Errorf("account=%q, want the signed claim %q", account, "signed@example.com")
	}
}

func TestReadGrokAccountAndPlan_UnparseableTokenStillNamesTheAccount(t *testing.T) {
	// An entry whose token we cannot decode still identifies its login. Falling
	// through to a later scope would describe a DIFFERENT account, which is
	// worse than reporting the one this entry names.
	base := t.TempDir()
	helperGrokScopedAuth(t, base, map[string]any{
		"key":   "not-a-jwt",
		"email": "ada@example.com",
	})

	account, _ := readGrokAccountAndPlan(base)
	if account != "ada@example.com" {
		t.Errorf("account=%q, want %q", account, "ada@example.com")
	}
}

func TestReadGrokAccountAndPlan_FallsBackToSubjectWhenNoEmailAnywhere(t *testing.T) {
	// Pre-fix behaviour must survive for auth files that genuinely carry no
	// address: a UUID is still better than an empty account.
	base := t.TempDir()
	helperGrokScopedAuth(t, base, map[string]any{
		"key": unsignedJWT(t, map[string]any{"sub": "baee20e5-1e5d-4dc3-aea5-224fae90e54c"}),
	})

	account, _ := readGrokAccountAndPlan(base)
	if account != "baee20e5-1e5d-4dc3-aea5-224fae90e54c" {
		t.Errorf("account=%q, want the subject fallback", account)
	}
}

func TestGrokIdentityCandidates_IncludesBothEmailAndUserID(t *testing.T) {
	// The billing log records `user_id` while the card displays the email, so
	// the candidate set must keep BOTH or the log-ownership check stops
	// matching once the display account becomes an address.
	base := t.TempDir()
	helperGrokScopedAuth(t, base, map[string]any{
		"key":     unsignedJWT(t, map[string]any{"sub": "subject-value"}),
		"email":   "ada@example.com",
		"user_id": "baee20e5-1e5d-4dc3-aea5-224fae90e54c",
	})

	got := map[string]bool{}
	for _, candidate := range grokIdentityCandidates(base) {
		got[candidate] = true
	}
	for _, want := range []string{"ada@example.com", "baee20e5-1e5d-4dc3-aea5-224fae90e54c"} {
		if !got[want] {
			t.Errorf("candidates missing %q, got %v", want, got)
		}
	}
}

// The bug Codex caught on PR #99: readGrokAuthExpiry bails the moment a
// refresh token is present — which is exactly the branch that wants to display
// the expiry — so populating LoginExpiresAt through it was a no-op. The
// display path reads through readGrokAccessTokenExpiry instead.
func TestReadGrokAccessTokenExpiry_ReportsExpiryDespiteRefreshToken(t *testing.T) {
	base := t.TempDir()
	helperGrokScopedAuth(t, base, map[string]any{
		"key":           unsignedJWT(t, map[string]any{"sub": "uuid"}),
		"email":         "ada@example.com",
		"expires_at":    "2126-08-12T21:04:17Z",
		"refresh_token": "refresh-token-value",
	})

	if _, ok := readGrokAuthExpiry(base); ok {
		t.Fatal("readGrokAuthExpiry must still refuse a refreshable credential — " +
			"its callers treat what it returns as a LOGIN deadline and warn on it")
	}

	got, ok := readGrokAccessTokenExpiry(base)
	if !ok {
		t.Fatal("readGrokAccessTokenExpiry returned no expiry for a refreshable credential")
	}
	want := time.Date(2126, 8, 12, 21, 4, 17, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("expiry=%v, want %v", got, want)
	}
}

func TestReadGrokAccessTokenExpiry_FallsBackToTheTokenExpClaim(t *testing.T) {
	// No `expires_at` field — the JWT's own `exp` still has to surface, or a
	// credential layout that omits the plain field shows a blank row again.
	exp := time.Date(2126, 8, 12, 21, 4, 17, 0, time.UTC)
	base := t.TempDir()
	helperGrokScopedAuth(t, base, map[string]any{
		"key":           unsignedJWT(t, map[string]any{"sub": "uuid", "exp": exp.Unix()}),
		"refresh_token": "refresh-token-value",
	})

	got, ok := readGrokAccessTokenExpiry(base)
	if !ok || !got.Equal(exp) {
		t.Errorf("expiry=%v ok=%v, want %v true", got, ok, exp)
	}
}

func TestGrokUsage_RefreshableCredentialReportsExpiryWithoutClaimingExpired(t *testing.T) {
	// End-to-end on the parser: the card gets a timestamp AND stays
	// authenticated. A past access-token expiry must never read as a logout —
	// the next request renews it.
	base := t.TempDir()
	helperGrokScopedAuth(t, base, map[string]any{
		"key":           unsignedJWT(t, map[string]any{"sub": "uuid"}),
		"email":         "ada@example.com",
		"expires_at":    "2000-01-01T00:00:00Z", // long past
		"refresh_token": "refresh-token-value",
	})
	t.Setenv("GROK_HOME", base)

	usage, ok := grokUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{}, time.Now())
	if !ok || usage == nil {
		t.Fatal("grok parser returned no usage")
	}
	if usage.LoginExpiresAt == "" {
		t.Error("LoginExpiresAt is empty — the row would stay blank")
	}
	if usage.LoginExpirationState != loginExpirationRefreshable {
		t.Errorf("LoginExpirationState=%q, want %q", usage.LoginExpirationState, loginExpirationRefreshable)
	}
	if usage.AuthState == "expired" || (usage.Authenticated != nil && !*usage.Authenticated) {
		t.Errorf("a PASSED access-token expiry must not read as a logout: authState=%q authenticated=%v",
			usage.AuthState, usage.Authenticated)
	}
}
