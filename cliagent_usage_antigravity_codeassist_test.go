package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

	if got := probeAntigravityQuotaCodeAssist(context.Background(), time.Now); got != liveProbeOutcomeCodeAssistOK {
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

func TestProbeAntigravityQuotaCodeAssist_IdentityFromTheStoredIDToken(t *testing.T) {
	helperStubAntigravityKeyring(t, map[string]any{
		"access_token": "access-B", "token_type": "Bearer",
		"expiry":   time.Now().Add(30 * time.Minute).Format(time.RFC3339Nano),
		"id_token": helperIDToken(t, "bob@example.com"),
	})
	_, userinfoCalls := helperCodeAssistServers(t,
		func(string) (int, string) { return http.StatusOK, `{"response":` + antigravityCodeAssistFixture + `}` },
		func(string) (int, string) { return http.StatusInternalServerError, `{}` })

	if got := probeAntigravityQuotaCodeAssist(context.Background(), time.Now); got != liveProbeOutcomeCodeAssistOK {
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
	if got := probeAntigravityQuotaCodeAssist(context.Background(), time.Now); got != liveProbeOutcomeCodeAssistTokenExpired {
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
	if got := probeAntigravityQuotaCodeAssist(context.Background(), time.Now); got != liveProbeOutcomeCodeAssistUnauthorized {
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
	if got := probeAntigravityQuotaCodeAssist(context.Background(), time.Now); got != liveProbeOutcomeCodeAssistNotSigned {
		t.Fatalf("outcome=%q, want not_attributable", got)
	}
	if _, err := os.Stat(os.Getenv("AIEXPEDITE_AGY_QUOTA_CACHE")); !os.IsNotExist(err) {
		t.Errorf("an unattributable reading was cached (stat err=%v)", err)
	}
}

func TestProbeAntigravityQuotaCodeAssist_NoLoginWithoutAKeyringEntry(t *testing.T) {
	helperStubAntigravityKeyring(t, nil)
	if got := probeAntigravityQuotaCodeAssist(context.Background(), time.Now); got != liveProbeOutcomeCodeAssistNoLogin {
		t.Fatalf("outcome=%q, want no_login", got)
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
	probeAntigravityQuotaCodeAssistFn = func(context.Context, func() time.Time) string {
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
