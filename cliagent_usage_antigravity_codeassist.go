// cliagent_usage_antigravity_codeassist.go — reads Antigravity's quota from
// Google directly, with the login `agy` keeps in the OS keyring.
//
// Why this exists: from `agy` 1.2.2 the loopback language server refuses the
// quota RPC without a per-run CSRF token nothing headless can obtain
// (cliagent_usage_antigravity_gate.go). The server was only ever a proxy: it
// answers RetrieveUserQuotaSummary by calling Google's Code Assist API with
// the account's OAuth token, and that token is on this machine — `agy` stores
// it in the OS keyring (Credential Manager `gemini:antigravity` on Windows, the
// Keychain item service `gemini` / account `antigravity` on macOS,
// secret-service on Linux) as a standard OAuth2 token JSON. So a gated build is
// read the way the Grok and Claude probes read theirs: the CLI's own stored
// credential, against the vendor's own endpoint.
//
// Boundaries, the same as those probes':
//
//   - Only the Refresh click calls it, and only once the loopback route is known
//     to be gated. It never renews the login: `agy` refreshes the keyring token
//     on its own runs, and the click's `agy models` warm-up is run first when the
//     stored token has expired. An expired token after that is reported, not
//     refreshed with agy's client secret.
//   - The endpoints are pinned HTTPS constants; a test override must be
//     loopback. No proxy inheritance, redirects refused, bodies capped, decoded
//     into allowlisted structs.
//   - The refresh token is never decoded, logged or persisted. What persists is
//     the same allowlisted snapshot the loopback route writes, scoped to the
//     account the token itself names (its id_token, else Google's userinfo) —
//     never to settings.json.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	antigravityCodeAssistEndpoint = "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary"
	antigravityUserinfoEndpoint   = "https://www.googleapis.com/oauth2/v3/userinfo"
	// Loopback-only overrides for tests (see antigravityPinnedURL).
	antigravityCodeAssistURLEnv = "AIEXPEDITE_AGY_CODEASSIST_URL"
	antigravityUserinfoURLEnv   = "AIEXPEDITE_AGY_USERINFO_URL"

	antigravityCodeAssistTimeout = 8 * time.Second
	antigravityCodeAssistMaxBody = 256 * 1024
	// antigravityTokenExpirySkew treats a token this close to expiry as
	// expired, so the request is not sent with a credential that dies in
	// flight.
	antigravityTokenExpirySkew = time.Minute

	// The keyring entry `agy` writes (service / user, joined as
	// "service:user" for the Windows credential name).
	antigravityKeyringService = "gemini"
	antigravityKeyringUser    = "antigravity"

	// antigravityCodeAssistUserAgentFallbackVersion is the build named in the
	// User-Agent when the detected agy version is unknown — the build the
	// rule below was verified on.
	antigravityCodeAssistUserAgentFallbackVersion = "1.2.3"
)

// antigravityCodeAssistUserAgent is the User-Agent the quota request MUST
// carry. Google licenses this endpoint per client, and identifies the client
// by this header: the same token with Go's default User-Agent is answered
// 403 PERMISSION_DENIED / SUBSCRIPTION_REQUIRED ("You do not have a valid
// license of this product"), which is what the agent recorded as
// codeassist_unauthorized on v1.0.24-25; with agy's own format it is answered
// 200 (verified 2026-09-15 against agy 1.2.3). The format mirrors agy's:
// "antigravity/cli/<version> (aidev_client; os_type=<os>; arch=<arch>; auth_method=consumer)".
func antigravityCodeAssistUserAgent(version string) string {
	version = clampASCII(strings.TrimSpace(version), antigravityQuotaGateMaxVersion)
	if version == "" {
		version = antigravityCodeAssistUserAgentFallbackVersion
	}
	return fmt.Sprintf("antigravity/cli/%s (aidev_client; os_type=%s; arch=%s; auth_method=consumer)", version, runtime.GOOS, runtime.GOARCH)
}

// Code Assist probe outcome codes — a closed set, logged on the device only.
const (
	liveProbeOutcomeCodeAssistOK           = "codeassist_ok"
	liveProbeOutcomeCodeAssistNoLogin      = "codeassist_no_login"
	liveProbeOutcomeCodeAssistTokenExpired = "codeassist_token_expired"
	liveProbeOutcomeCodeAssistUnauthorized = "codeassist_unauthorized"
	liveProbeOutcomeCodeAssistHTTPError    = "codeassist_http_error"
	liveProbeOutcomeCodeAssistBadResponse  = "codeassist_bad_response"
	liveProbeOutcomeCodeAssistNotSigned    = "codeassist_not_attributable"
)

// antigravityKeyringToken is the allowlisted part of the stored OAuth2 token.
// refresh_token is deliberately absent from the struct: it is never decoded.
type antigravityKeyringToken struct {
	AccessToken string    `json:"access_token"`
	TokenType   string    `json:"token_type"`
	Expiry      time.Time `json:"expiry"`
	IDToken     string    `json:"id_token"`
}

// antigravityKeyringReader is the platform keyring read
// (antigravity_keyring_*.go); a var so tests supply a token without touching
// the machine's keyring.
var antigravityKeyringReader = readAntigravityKeyringCredential

// probeAntigravityQuotaCodeAssistFn is the orchestration seam.
var probeAntigravityQuotaCodeAssistFn = probeAntigravityQuotaCodeAssist

// antigravityStoredToken returns the access token `agy` keeps in the keyring,
// with its expiry when stated, and the account the token itself names when an
// id_token is stored beside it.
//
// Two shapes are accepted. What `agy` 1.2.x actually writes (read off a real
// entry's key names on 2026-09-15) wraps the OAuth2 token:
//
//	{"auth_method": "...", "id_token": "...", "token": {"access_token", "token_type", "refresh_token", "expiry"}}
//
// and a bare OAuth2 token JSON is taken as well, in case a build stores it
// unwrapped. A present entry in neither shape is logged by its key names —
// never its values — so the next shape change is diagnosable from agent.log.
func antigravityStoredToken(ctx context.Context) (tok antigravityKeyringToken, ok bool) {
	raw, ok := antigravityKeyringReader(ctx)
	if !ok || len(bytes.TrimSpace(raw)) == 0 {
		return antigravityKeyringToken{}, false
	}
	var wrapped struct {
		IDToken string                   `json:"id_token"`
		Token   *antigravityKeyringToken `json:"token"`
	}
	if json.Unmarshal(raw, &wrapped) == nil && wrapped.Token != nil && strings.TrimSpace(wrapped.Token.AccessToken) != "" {
		tok = *wrapped.Token
		if tok.IDToken == "" {
			tok.IDToken = wrapped.IDToken
		}
		return tok, true
	}
	if json.Unmarshal(raw, &tok) == nil && strings.TrimSpace(tok.AccessToken) != "" {
		return tok, true
	}
	fmt.Printf("%s[cli-usage] Antigravity keyring entry present but in an unrecognised shape (top-level keys: %s)%s\n",
		colorYellow, antigravityJSONKeyNames(raw), colorReset)
	return antigravityKeyringToken{}, false
}

// antigravityJSONKeyNames lists a JSON object's top-level key names — the one
// thing about an unrecognised credential that is safe to log.
func antigravityJSONKeyNames(raw []byte) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return "not a JSON object"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, clampASCII(k, 40))
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// antigravityPinnedURL resolves an endpoint, honouring an override ONLY when
// it points at loopback: the request carries the user's Google bearer token,
// so an env var that could aim it at an arbitrary host would be a
// credential-exfiltration primitive (same rule as the Claude probe).
func antigravityPinnedURL(envName, fallback string) string {
	raw := os.Getenv(envName)
	if raw == "" {
		return fallback
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fallback
	}
	host := u.Hostname()
	if host == "localhost" {
		return raw
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return raw
	}
	return fallback
}

func antigravityCodeAssistClient() *http.Client {
	return &http.Client{
		Timeout: antigravityCodeAssistTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy:       nil,
			DialContext: (&net.Dialer{Timeout: antigravityCodeAssistTimeout}).DialContext,
		},
	}
}

// antigravityIDTokenEmail returns the email claim of an OpenID id_token. The
// signature is not verified: the token came from the user's own keyring and
// is used only to NAME the account a reading is cached under, exactly as the
// Grok parser reads its stored JWT's claims.
func antigravityIDTokenEmail(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return strings.TrimSpace(claims.Email)
}

// antigravityCodeAssistIdentity names the account: the id_token's email when
// one is stored, else Google's userinfo for the same access token.
func antigravityCodeAssistIdentity(ctx context.Context, client *http.Client, tok antigravityKeyringToken) string {
	if email := antigravityIDTokenEmail(tok.IDToken); email != "" {
		return email
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, antigravityPinnedURL(antigravityUserinfoURLEnv, antigravityUserinfoEndpoint), nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, antigravityCodeAssistMaxBody))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var info struct {
		Email string `json:"email"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, antigravityCodeAssistMaxBody))
	if err != nil || json.Unmarshal(body, &info) != nil {
		return ""
	}
	return strings.TrimSpace(info.Email)
}

// fetchAntigravityQuotaCodeAssist performs the one quota request. The
// response is the same QuotaSummary shape the loopback server relays, either
// bare or under a "response" envelope; both are accepted.
func fetchAntigravityQuotaCodeAssist(ctx context.Context, client *http.Client, accessToken, version string, now time.Time) (antigravityQuotaSnapshot, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		antigravityPinnedURL(antigravityCodeAssistURLEnv, antigravityCodeAssistEndpoint), bytes.NewReader([]byte("{}")))
	if err != nil {
		return antigravityQuotaSnapshot{}, liveProbeOutcomeCodeAssistHTTPError
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", antigravityCodeAssistUserAgent(version))
	resp, err := client.Do(req)
	if err != nil {
		return antigravityQuotaSnapshot{}, liveProbeOutcomeCodeAssistHTTPError
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, antigravityCodeAssistMaxBody))
		_ = resp.Body.Close()
	}()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// 401 (the token) and 403 (the licence / client identity) are worth
		// telling apart on the device: a 403 on a valid token is the client
		// rule above, not the login.
		fmt.Printf("%s[cli-usage] Antigravity Code Assist quota request refused with %d%s\n", colorYellow, resp.StatusCode, colorReset)
		return antigravityQuotaSnapshot{}, liveProbeOutcomeCodeAssistUnauthorized
	case resp.StatusCode != http.StatusOK:
		// The status class is the one diagnostic worth having on the device
		// (a 400 says the request shape moved, a 5xx says Google did), and it
		// carries nothing of the user's.
		fmt.Printf("%s[cli-usage] Antigravity Code Assist quota request answered %d%s\n", colorYellow, resp.StatusCode, colorReset)
		return antigravityQuotaSnapshot{}, liveProbeOutcomeCodeAssistHTTPError
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, antigravityCodeAssistMaxBody))
	if err != nil {
		return antigravityQuotaSnapshot{}, liveProbeOutcomeCodeAssistBadResponse
	}
	var wrapped struct {
		Groups   []antigravityQuotaGroupWire `json:"groups"`
		Response struct {
			Groups []antigravityQuotaGroupWire `json:"groups"`
		} `json:"response"`
	}
	if json.Unmarshal(body, &wrapped) != nil {
		return antigravityQuotaSnapshot{}, liveProbeOutcomeCodeAssistBadResponse
	}
	groups := wrapped.Groups
	if len(groups) == 0 {
		groups = wrapped.Response.Groups
	}
	snap, ok := antigravitySnapshotFromGroups(groups, now)
	if !ok {
		return antigravityQuotaSnapshot{}, liveProbeOutcomeCodeAssistBadResponse
	}
	return snap, liveProbeOutcomeCodeAssistOK
}

// probeAntigravityQuotaCodeAssist reads the quota from Google with the stored
// login and caches it under the account that login names. Returns a closed
// outcome code.
func probeAntigravityQuotaCodeAssist(ctx context.Context, version string, now func() time.Time) string {
	tok, ok := antigravityStoredToken(ctx)
	if !ok {
		return liveProbeOutcomeCodeAssistNoLogin
	}
	if !tok.Expiry.IsZero() && !now().Add(antigravityTokenExpirySkew).Before(tok.Expiry) {
		return liveProbeOutcomeCodeAssistTokenExpired
	}
	client := antigravityCodeAssistClient()
	defer client.CloseIdleConnections()

	snap, outcome := fetchAntigravityQuotaCodeAssist(ctx, client, tok.AccessToken, version, now())
	if outcome != liveProbeOutcomeCodeAssistOK {
		return outcome
	}
	// Identity from the same credential, in the same probe: the quota and the
	// account it is cached under can never come from two different logins.
	snap.Account = antigravityCodeAssistIdentity(ctx, client, tok)
	persisted, _ := antigravityCapturePersist(snap)
	if !persisted {
		return liveProbeOutcomeCodeAssistNotSigned
	}
	noteAntigravityLiveProducer(fingerprintAccount("antigravity", snap.Account), now())
	return liveProbeOutcomeCodeAssistOK
}
