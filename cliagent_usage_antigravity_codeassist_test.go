package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The Code Assist route reads a gated build's quota from Google with the
// login `agy` keeps in the OS keyring. Every test here supplies that login
// through the reader seam; none touches the machine's keyring.

const antigravityCodeAssistFixture = `{"groups":[{"displayName":"Gemini Models","buckets":[
  {"bucketId":"gemini-5h","displayName":"5-hour","window":"5h","remainingFraction":0.96,"resetTime":"2126-08-12T04:37:41Z"},
  {"bucketId":"gemini-weekly","displayName":"Weekly","window":"weekly","remainingFraction":0.6,"resetTime":"2126-08-14T00:00:00Z"}]}]}`

func helperStubAntigravityKeyring(t *testing.T, tok map[string]any) {
	t.Helper()
	orig := antigravityKeyringReader
	t.Cleanup(func() { antigravityKeyringReader = orig })
	if tok == nil {
		antigravityKeyringReader = func(context.Context) ([]byte, bool) { return nil, false }
		return
	}
	raw, _ := json.Marshal(tok)
	antigravityKeyringReader = func(context.Context) ([]byte, bool) { return raw, true }
}

func helperIDToken(t *testing.T, email string) string {
	t.Helper()
	claims, _ := json.Marshal(map[string]any{"email": email, "sub": "123"})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

// helperCodeAssistServers stands up loopback stand-ins for the quota endpoint
// and userinfo, and points the probe at them.
func helperCodeAssistServers(t *testing.T, quota func(auth string) (int, string), userinfo func(auth string) (int, string)) (quotaCalls, userinfoCalls *int32) {
	t.Helper()
	var q, u int32
	qs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&q, 1)
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		// Google's rule: a client it does not know is answered 403
		// SUBSCRIPTION_REQUIRED however valid the token (verified 2026-09-15).
		if !strings.HasPrefix(r.UserAgent(), "antigravity/cli/") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(antigravityCodeAssistLicenceRefusal))
			return
		}
		status, body := quota(r.Header.Get("Authorization"))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	us := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&u, 1)
		status, body := userinfo(r.Header.Get("Authorization"))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(qs.Close)
	t.Cleanup(us.Close)
	t.Setenv(antigravityCodeAssistURLEnv, qs.URL+"/v1internal:retrieveUserQuotaSummary")
	t.Setenv(antigravityUserinfoURLEnv, us.URL+"/oauth2/v3/userinfo")
	t.Setenv("AIEXPEDITE_AGY_QUOTA_CACHE", filepath.Join(t.TempDir(), "agyq.json"))
	return &q, &u
}

func TestProbeAntigravityQuotaCodeAssist_ReadsAndCachesUnderTheTokensAccount(t *testing.T) {
	helperStubAntigravityKeyring(t, map[string]any{
		"access_token": "access-A", "token_type": "Bearer", "refresh_token": "never-read",
		"expiry": time.Now().Add(30 * time.Minute).Format(time.RFC3339Nano),
	})
	quotaCalls, userinfoCalls := helperCodeAssistServers(t,
		func(auth string) (int, string) {
			if auth != "Bearer access-A" {
				return http.StatusUnauthorized, `{}`
			}
			return http.StatusOK, antigravityCodeAssistFixture
		},
		func(auth string) (int, string) {
			if auth != "Bearer access-A" {
				return http.StatusUnauthorized, `{}`
			}
			return http.StatusOK, `{"sub":"123","email":"ada@example.com"}`
		})

	if got := probeAntigravityQuotaCodeAssist(context.Background(), "1.2.3", time.Now); got != liveProbeOutcomeCodeAssistOK {
		t.Fatalf("outcome=%q, want ok", got)
	}
	if *quotaCalls != 1 || *userinfoCalls != 1 {
		t.Errorf("quota=%d userinfo=%d requests, want one each", *quotaCalls, *userinfoCalls)
	}
	var snap antigravityQuotaSnapshot
	if !readJSONFile(os.Getenv("AIEXPEDITE_AGY_QUOTA_CACHE"), &snap) {
		t.Fatal("no snapshot cached")
	}
	if snap.Account != "ada@example.com" || snap.AccountFingerprint != fingerprintAccount("antigravity", "ada@example.com") {
		t.Errorf("snapshot account=%q fingerprint=%q, want the token's account", snap.Account, snap.AccountFingerprint)
	}
	if len(snap.Buckets) != 2 || snap.Buckets[1].RemainingFraction != 0.6 {
		t.Errorf("buckets=%+v, want both plottable windows", snap.Buckets)
	}
	raw, _ := os.ReadFile(os.Getenv("AIEXPEDITE_AGY_QUOTA_CACHE"))
	if strings.Contains(string(raw), "access-A") || strings.Contains(string(raw), "never-read") {
		t.Error("a token leaked into the cache")
	}
	if recentAntigravityLiveProducer(time.Now()) != snap.AccountFingerprint {
		t.Error("the live producer was not noted for the gather that follows")
	}
}

// The shape agy 1.2.x really writes: the OAuth2 token wrapped under "token",
// with auth_method and id_token beside it.
func TestProbeAntigravityQuotaCodeAssist_IdentityFromTheStoredIDToken(t *testing.T) {
	helperStubAntigravityKeyring(t, map[string]any{
		"auth_method": "consumer",
		"id_token":    helperIDToken(t, "bob@example.com"),
		"token": map[string]any{
			"access_token": "access-B", "token_type": "Bearer", "refresh_token": "never-read",
			"expiry": time.Now().Add(30 * time.Minute).Format(time.RFC3339Nano),
		},
	})
	_, userinfoCalls := helperCodeAssistServers(t,
		func(string) (int, string) { return http.StatusOK, `{"response":` + antigravityCodeAssistFixture + `}` },
		func(string) (int, string) { return http.StatusInternalServerError, `{}` })

	if got := probeAntigravityQuotaCodeAssist(context.Background(), "1.2.3", time.Now); got != liveProbeOutcomeCodeAssistOK {
		t.Fatalf("outcome=%q, want ok (enveloped response accepted, identity from id_token)", got)
	}
	if *userinfoCalls != 0 {
		t.Errorf("userinfo was called %d times although the id_token names the account", *userinfoCalls)
	}
	var snap antigravityQuotaSnapshot
	readJSONFile(os.Getenv("AIEXPEDITE_AGY_QUOTA_CACHE"), &snap)
	if snap.Account != "bob@example.com" {
		t.Errorf("account=%q, want the id_token's email", snap.Account)
	}
}

func TestProbeAntigravityQuotaCodeAssist_ExpiredTokenIsNamedAndNeverSent(t *testing.T) {
	helperStubAntigravityKeyring(t, map[string]any{
		"access_token": "stale", "token_type": "Bearer",
		"expiry": time.Now().Add(-time.Minute).Format(time.RFC3339Nano),
	})
	quotaCalls, _ := helperCodeAssistServers(t,
		func(string) (int, string) { return http.StatusOK, antigravityCodeAssistFixture },
		func(string) (int, string) { return http.StatusOK, `{"email":"x@example.com"}` })
	if got := probeAntigravityQuotaCodeAssist(context.Background(), "1.2.3", time.Now); got != liveProbeOutcomeCodeAssistTokenExpired {
		t.Fatalf("outcome=%q, want token_expired", got)
	}
	if *quotaCalls != 0 {
		t.Error("an expired token was sent")
	}
}

func TestProbeAntigravityQuotaCodeAssist_RefusalsNeverPersist(t *testing.T) {
	helperStubAntigravityKeyring(t, map[string]any{"access_token": "revoked", "token_type": "Bearer"})
	helperCodeAssistServers(t,
		func(string) (int, string) { return http.StatusUnauthorized, `{"error":{"status":"UNAUTHENTICATED"}}` },
		func(string) (int, string) { return http.StatusOK, `{"email":"x@example.com"}` })
	if got := probeAntigravityQuotaCodeAssist(context.Background(), "1.2.3", time.Now); got != liveProbeOutcomeCodeAssistUnauthorized {
		t.Fatalf("outcome=%q, want unauthorized", got)
	}
	if _, err := os.Stat(os.Getenv("AIEXPEDITE_AGY_QUOTA_CACHE")); !os.IsNotExist(err) {
		t.Errorf("nothing may be cached after a refusal (stat err=%v)", err)
	}

	// An unattributable reading (no id_token, userinfo down) is displayed
	// nowhere and cached nowhere — the same rule the loopback route follows.
	helperStubAntigravityKeyring(t, map[string]any{"access_token": "anon", "token_type": "Bearer"})
	helperCodeAssistServers(t,
		func(string) (int, string) { return http.StatusOK, antigravityCodeAssistFixture },
		func(string) (int, string) { return http.StatusInternalServerError, `{}` })
	if got := probeAntigravityQuotaCodeAssist(context.Background(), "1.2.3", time.Now); got != liveProbeOutcomeCodeAssistNotSigned {
		t.Fatalf("outcome=%q, want not_attributable", got)
	}
	if _, err := os.Stat(os.Getenv("AIEXPEDITE_AGY_QUOTA_CACHE")); !os.IsNotExist(err) {
		t.Errorf("an unattributable reading was cached (stat err=%v)", err)
	}
}

func TestProbeAntigravityQuotaCodeAssist_NoLoginWithoutAKeyringEntry(t *testing.T) {
	helperStubAntigravityKeyring(t, nil)
	if got := probeAntigravityQuotaCodeAssist(context.Background(), "1.2.3", time.Now); got != liveProbeOutcomeCodeAssistNoLogin {
		t.Fatalf("outcome=%q, want no_login", got)
	}
	// An entry in an unknown shape is no login either — and only its key
	// names may be logged.
	helperStubAntigravityKeyring(t, map[string]any{"credential": "opaque-secret-value"})
	if got := probeAntigravityQuotaCodeAssist(context.Background(), "1.2.3", time.Now); got != liveProbeOutcomeCodeAssistNoLogin {
		t.Fatalf("outcome=%q, want no_login for an unrecognised shape", got)
	}
	if names := antigravityJSONKeyNames([]byte(`{"token":{"access_token":"x"},"id_token":"y"}`)); names != "id_token,token" {
		t.Errorf("key names=%q", names)
	}
}

// The macOS Keychain hands the login back inside go-keyring's base64
// envelope ("go-keyring-base64:<base64 JSON>") — that is what
// `security find-generic-password -s gemini -a antigravity -w` printed on
// Daniel-Mac on 2026-09-15, and what v1.0.24-26 logged as "not a JSON object"
// on every Refresh while the Windows machine (unencoded blob) worked.
func TestAntigravityStoredToken_DecodesTheMacKeychainEnvelope(t *testing.T) {
	inner, _ := json.Marshal(map[string]any{
		"auth_method": "consumer",
		"id_token":    helperIDToken(t, "mac@example.com"),
		"token": map[string]any{
			"access_token": "ya29.mac-token", "token_type": "Bearer", "refresh_token": "1//never-decoded",
			"expiry": time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	})
	envelope := []byte(antigravityKeyringBase64Prefix + base64.StdEncoding.EncodeToString(inner) + "\n")
	orig := antigravityKeyringReader
	t.Cleanup(func() { antigravityKeyringReader = orig })
	antigravityKeyringReader = func(context.Context) ([]byte, bool) { return envelope, true }

	tok, ok := antigravityStoredToken(context.Background())
	if !ok {
		t.Fatal("the enveloped login must be read as a login")
	}
	if tok.AccessToken != "ya29.mac-token" {
		t.Errorf("access_token=%q", tok.AccessToken)
	}
	if got := antigravityIDTokenEmail(tok.IDToken); got != "mac@example.com" {
		t.Errorf("account from the enveloped id_token=%q", got)
	}

	// A prefix that does not decode is left as read, so the shape log still
	// names what was there instead of an empty string.
	bad := []byte(antigravityKeyringBase64Prefix + "%%%not-base64%%%")
	if got := decodeAntigravityKeyringEnvelope(bad); string(got) != string(bad) {
		t.Errorf("undecodable envelope changed to %q", got)
	}
	// An unenveloped value (Windows, Linux) passes through untouched.
	if got := decodeAntigravityKeyringEnvelope(inner); string(got) != string(inner) {
		t.Error("plain JSON must pass through the envelope decoder unchanged")
	}
}

func TestAntigravityPinnedURL_OverrideMustBeLoopback(t *testing.T) {
	t.Setenv(antigravityCodeAssistURLEnv, "https://evil.example.com/v1internal:retrieveUserQuotaSummary")
	if got := antigravityPinnedURL(antigravityCodeAssistURLEnv, antigravityCodeAssistEndpoint); got != antigravityCodeAssistEndpoint {
		t.Errorf("a non-loopback override was honoured: %q", got)
	}
	t.Setenv(antigravityCodeAssistURLEnv, "http://127.0.0.1:9/x")
	if got := antigravityPinnedURL(antigravityCodeAssistURLEnv, antigravityCodeAssistEndpoint); got != "http://127.0.0.1:9/x" {
		t.Errorf("a loopback override was refused: %q", got)
	}
}

// TestProbeAntigravityQuotaLiveUnlessGated_GatedBuildReadsFromGoogle: the
// click's Antigravity outcome on a gated build is the Code Assist route's, and
// an expired stored token gets one `agy` warm-up (which refreshes the keyring)
// before the single retry.
func TestProbeAntigravityQuotaLiveUnlessGated_GatedBuildReadsFromGoogle(t *testing.T) {
	helperIsolateAntigravityGate(t)
	stubLiveProbes(t)
	noteAntigravityQuotaGate("1.2.3", time.Now())
	var spawned, warmed, reads int32
	probeAntigravityQuotaLiveFn = func(context.Context, string, string) string { atomic.AddInt32(&spawned, 1); return liveProbeOutcomeOK }
	warmCLIAgentModelDiscoveryFn = func(_ context.Context, id string, _ detectedCLIAgent, _ string) {
		if id == "antigravity" {
			atomic.AddInt32(&warmed, 1)
		}
	}
	probeAntigravityQuotaCodeAssistFn = func(context.Context, string, func() time.Time) string {
		if atomic.AddInt32(&reads, 1) == 1 {
			return liveProbeOutcomeCodeAssistTokenExpired
		}
		return liveProbeOutcomeCodeAssistOK
	}
	got := probeAntigravityQuotaLiveUnlessGated(context.Background(), detectedCLIAgent{Detected: true, Path: "agy", Version: "1.2.3"}, t.TempDir())
	if got != liveProbeOutcomeCodeAssistOK {
		t.Fatalf("outcome=%q, want the Code Assist reading", got)
	}
	if spawned != 0 {
		t.Error("a gated build spawned the loopback probe")
	}
	if warmed != 1 || reads != 2 {
		t.Errorf("warm=%d reads=%d, want one agy warm-up between the expired read and its retry", warmed, reads)
	}
}

// TestAntigravityUsageParser_ReadingNewerThanTheGateHidesTheNotice: once the
// Code Assist route has produced a reading after the refusal, the card shows
// that reading, not the "cannot read" notice.
func TestAntigravityUsageParser_ReadingNewerThanTheGateHidesTheNotice(t *testing.T) {
	helperIsolateAntigravityGate(t)
	home := t.TempDir()
	cache := filepath.Join(t.TempDir(), "agyq.json")
	t.Setenv("AIEXPEDITE_AGY_QUOTA_CACHE", cache)
	now := time.Date(2026, 9, 15, 15, 0, 0, 0, time.UTC)
	noteAntigravityQuotaGate("1.2.3", now.Add(-2*time.Hour))

	snap := antigravityQuotaSnapshot{
		ObservedAt:         now.Add(-10 * time.Minute).Format(time.RFC3339),
		AccountFingerprint: fingerprintAccount("antigravity", "ada@example.com"),
		Account:            "ada@example.com",
		Buckets:            []antigravityQuotaBucket{{Group: "Gemini Models", Window: "weekly", RemainingFraction: 0.4, ResetTime: "2126-08-14T00:00:00Z"}},
	}
	body, _ := json.Marshal(snap)
	if err := os.WriteFile(cache, body, 0o600); err != nil {
		t.Fatal(err)
	}
	helperWriteJSON(t, filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"), map[string]any{"email": "ada@example.com"})

	usage, ok := antigravityUsageParser{}.Parse(home, detectedCLIAgent{Detected: true, Version: "1.2.3"}, now)
	if !ok {
		t.Fatal("Parse failed")
	}
	if usage.Notice != "" {
		t.Errorf("notice=%q, want none while a reading newer than the gate exists", usage.Notice)
	}
	if len(usage.Metrics) != 1 || usage.Metrics[0].Unknown {
		t.Errorf("metrics=%+v, want the newer reading plotted", usage.Metrics)
	}

	// The poller noting the same refusal again must not move the gate past
	// that reading.
	noteAntigravityQuotaGate("", now)
	usage, _ = antigravityUsageParser{}.Parse(home, detectedCLIAgent{Detected: true, Version: "1.2.3"}, now)
	if usage.Notice != "" {
		t.Errorf("a re-noted refusal outranked the newer reading: %q", usage.Notice)
	}
}

// antigravityCodeAssistLicenceRefusal is what Google answered the agent's
// v1.0.24-25 request (Go's default User-Agent) with, a valid token and all.
const antigravityCodeAssistLicenceRefusal = `{"error":{"code":403,"message":"You do not have a valid license of this product. Please contact your administrator to request a license. If you are not an enterprise user and believe you are receiving this message as an error, please try using the latest version and logging in again. (#3501)","status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SUBSCRIPTION_REQUIRED","domain":"cloudaicompanion.googleapis.com"}]}}`

// TestProbeAntigravityQuotaCodeAssist_IdentifiesItselfAsTheAntigravityClient:
// the request carries agy's own User-Agent, built from the detected version
// — without it Google refuses the licence (403), which is exactly what AIE2
// logged as codeassist_unauthorized after the v1.0.25 update.
func TestProbeAntigravityQuotaCodeAssist_IdentifiesItselfAsTheAntigravityClient(t *testing.T) {
	helperStubAntigravityKeyring(t, map[string]any{
		"access_token": "access-A", "token_type": "Bearer",
		"expiry": time.Now().Add(30 * time.Minute).Format(time.RFC3339Nano),
	})
	var seenUA string
	qs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUA = r.UserAgent()
		if !strings.HasPrefix(seenUA, "antigravity/cli/") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(antigravityCodeAssistLicenceRefusal))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(antigravityCodeAssistFixture))
	}))
	us := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"email":"ada@example.com"}`))
	}))
	t.Cleanup(qs.Close)
	t.Cleanup(us.Close)
	t.Setenv(antigravityCodeAssistURLEnv, qs.URL+"/v1internal:retrieveUserQuotaSummary")
	t.Setenv(antigravityUserinfoURLEnv, us.URL+"/oauth2/v3/userinfo")
	t.Setenv("AIEXPEDITE_AGY_QUOTA_CACHE", filepath.Join(t.TempDir(), "agyq.json"))

	if got := probeAntigravityQuotaCodeAssist(context.Background(), "1.2.3", time.Now); got != liveProbeOutcomeCodeAssistOK {
		t.Fatalf("outcome=%q, want ok", got)
	}
	want := "antigravity/cli/1.2.3 (aidev_client; os_type=" + runtime.GOOS + "; arch=" + runtime.GOARCH + "; auth_method=consumer)"
	if seenUA != want {
		t.Errorf("User-Agent=%q, want %q", seenUA, want)
	}
	// An unknown detected version still names a build Google accepts.
	if got := antigravityCodeAssistUserAgent(""); !strings.HasPrefix(got, "antigravity/cli/"+antigravityCodeAssistUserAgentFallbackVersion+" (") {
		t.Errorf("fallback User-Agent=%q", got)
	}
}
