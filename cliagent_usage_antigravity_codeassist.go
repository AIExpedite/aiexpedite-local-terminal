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
//   - Called by the Refresh click once the loopback route is known to be gated,
//     and by the run-completion debt worker (cliagent_usage_antigravity_freshness.go).
//     The read never renews the login itself: `agy` refreshes the keyring token
//     on its own runs, so an expired token is renewed by running `agy models`
//     as the agent's own child (renewAntigravityStoredLogin) and reading again.
//     The token is never refreshed with agy's client secret.
//   - The reply's account is resolved BEFORE its buckets are converted, so
//     in-memory managed-run exhaustion evidence applies only to that account.
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
	"sync"
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
	// flight. 15 s covers the 8 s request plus the userinfo call. It must stay
	// close to the margin `agy` itself renews at (Go's oauth2 refreshes about
	// 10 s before expiry): a wider band makes a token `agy` still considers
	// valid "expired" here, and no `agy models` run would renew it.
	antigravityTokenExpirySkew = 15 * time.Second

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

// antigravityKeyringBase64Prefix marks a value the Go `go-keyring` library
// (which `agy` stores its login through) base64-encoded before writing it to
// the macOS Keychain. `security find-generic-password -w` hands that envelope
// back verbatim — "go-keyring-base64:eyJ0b2tlbiI6…" — so the JSON is only
// reachable after stripping the prefix and decoding. Windows Credential
// Manager and Linux secret-service receive the JSON unencoded, so the decode
// is prefix-driven and harmless where the prefix never appears.
const antigravityKeyringBase64Prefix = "go-keyring-base64:"

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
	raw = decodeAntigravityKeyringEnvelope(raw)
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

// decodeAntigravityKeyringEnvelope unwraps the go-keyring base64 envelope the
// macOS Keychain returns (see antigravityKeyringBase64Prefix). Any other value
// is returned as read; an envelope that does not decode is returned as read
// too, so the shape log names it rather than an empty string.
func decodeAntigravityKeyringEnvelope(raw []byte) []byte {
	trimmed := bytes.TrimSpace(raw)
	if !bytes.HasPrefix(trimmed, []byte(antigravityKeyringBase64Prefix)) {
		return raw
	}
	decoded, err := base64.StdEncoding.DecodeString(string(trimmed[len(antigravityKeyringBase64Prefix):]))
	if err != nil {
		return raw
	}
	return decoded
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

// fetchAntigravityQuotaCodeAssist performs the one quota request and returns
// the raw groups: conversion waits until the reply's account is known, because
// exhaustion evidence applies only to that account. The response is the same
// QuotaSummary shape the loopback server relays, either bare or under a
// "response" envelope; both are accepted.
func fetchAntigravityQuotaCodeAssist(ctx context.Context, client *http.Client, accessToken, version string) ([]antigravityQuotaGroupWire, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		antigravityPinnedURL(antigravityCodeAssistURLEnv, antigravityCodeAssistEndpoint), bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, liveProbeOutcomeCodeAssistHTTPError
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", antigravityCodeAssistUserAgent(version))
	resp, err := client.Do(req)
	if err != nil {
		return nil, liveProbeOutcomeCodeAssistHTTPError
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, antigravityCodeAssistMaxBody))
		_ = resp.Body.Close()
	}()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// 401 (the token) and 403 (the licence / client identity) are worth
		// telling apart on the device: a 403 on a valid token is the client
		// rule above, not the login. Only the status code is logged — never
		// Google's status / reason strings.
		fmt.Printf("%s[cli-usage] Antigravity Code Assist quota request refused with %d%s\n", colorYellow, resp.StatusCode, colorReset)
		return nil, liveProbeOutcomeCodeAssistUnauthorized
	case resp.StatusCode != http.StatusOK:
		// The status class is the one diagnostic worth having on the device
		// (a 400 says the request shape moved, a 5xx says Google did), and it
		// carries nothing of the user's.
		fmt.Printf("%s[cli-usage] Antigravity Code Assist quota request answered %d%s\n", colorYellow, resp.StatusCode, colorReset)
		return nil, liveProbeOutcomeCodeAssistHTTPError
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, antigravityCodeAssistMaxBody))
	if err != nil {
		return nil, liveProbeOutcomeCodeAssistBadResponse
	}
	var wrapped struct {
		Groups   []antigravityQuotaGroupWire `json:"groups"`
		Response struct {
			Groups []antigravityQuotaGroupWire `json:"groups"`
		} `json:"response"`
	}
	if json.Unmarshal(body, &wrapped) != nil {
		return nil, liveProbeOutcomeCodeAssistBadResponse
	}
	groups := wrapped.Groups
	if len(groups) == 0 {
		groups = wrapped.Response.Groups
	}
	return groups, liveProbeOutcomeCodeAssistOK
}

// probeAntigravityQuotaCodeAssist reads the quota from Google with the stored
// login and caches it under the account that login names. Returns a closed
// outcome code.
//
// An empty version is resolved here (antigravityCodeAssistBuildVersion), and
// only once a usable login is known to exist: that resolution can spawn
// `<agy> --version` on a cold cache, and the two local refusals above it must
// stay free — a debt nothing on this machine can pay (no login, an expired
// token) never starts a child.
//
// The account is resolved BEFORE the buckets are converted, so managed-run
// exhaustion evidence (in memory, cliagent_usage_antigravity_freshness.go) is
// applied only to the account it was recorded for. A 200 with nothing
// chartable stays codeassist_bad_response on disk; the finer "unplottable"
// fact is noted in memory for the notice wording only.
func probeAntigravityQuotaCodeAssist(ctx context.Context, version string, now func() time.Time) string {
	tok, ok := antigravityStoredToken(ctx)
	if !ok {
		return liveProbeOutcomeCodeAssistNoLogin
	}
	if !tok.Expiry.IsZero() && !now().Add(antigravityTokenExpirySkew).Before(tok.Expiry) {
		// The expiry travels beside the closed outcome, in memory only, so the
		// debt worker can tell a token inside the skew band (book a retry at its
		// expiry) from one already past it (renew it) without a second read.
		noteAntigravityCodeAssistTokenExpiry(ctx, tok.Expiry.UnixMilli())
		return liveProbeOutcomeCodeAssistTokenExpired
	}
	version = antigravityCodeAssistBuildVersion(version)
	client := antigravityCodeAssistClient()
	defer client.CloseIdleConnections()

	groups, outcome := fetchAntigravityQuotaCodeAssist(ctx, client, tok.AccessToken, version)
	if outcome != liveProbeOutcomeCodeAssistOK {
		return outcome
	}
	// Identity from the same credential, in the same probe: the quota and the
	// account it is cached under can never come from two different logins.
	account := antigravityCodeAssistIdentity(ctx, client, tok)
	fingerprint := fingerprintAccount("antigravity", account)
	var evidence []antigravityExhaustionEvent
	if fingerprint != "" {
		evidence = antigravityExhaustionEvidenceFn(now(), fingerprint)
	}
	snap, shape, plottable := antigravitySnapshotFromGroups(groups, now(), evidence)
	logAntigravityQuotaShapeOnce(shape)
	if !plottable {
		noteAntigravityCodeAssistAttempt(ctx, fingerprint, true)
		return liveProbeOutcomeCodeAssistBadResponse
	}
	noteAntigravityCodeAssistAttempt(ctx, fingerprint, false)
	snap.Account = account
	// Attest the route on the cached reading itself, not only in memory: the
	// gather that replays it may belong to a later process (a restart or a
	// self-update), and a settings.json naming another account needs the
	// attestation to replay this reading at all.
	snap.StoredLoginRead = true
	persisted, _ := antigravityCapturePersist(snap)
	if !persisted {
		return liveProbeOutcomeCodeAssistNotSigned
	}
	noteAntigravityLiveProducer(fingerprint, now())
	return liveProbeOutcomeCodeAssistOK
}

// antigravityQuotaShapesLogged bounds logAntigravityQuotaShapeOnce.
const antigravityQuotaShapesMax = 8

var antigravityQuotaShapesLogged struct {
	mu   sync.Mutex
	seen map[antigravityQuotaShape]bool
}

// logAntigravityQuotaShapeOnce prints a reply's shape counters — integers
// only, never a key, window, status string, reason or account — once per
// distinct shape per process, for at most antigravityQuotaShapesMax shapes.
// It is how a reply that stopped charting (every bucket omitted, a renamed
// window) becomes diagnosable from agent.log without logging the reply.
func logAntigravityQuotaShapeOnce(shape antigravityQuotaShape) {
	logged := &antigravityQuotaShapesLogged
	logged.mu.Lock()
	if logged.seen == nil {
		logged.seen = map[antigravityQuotaShape]bool{}
	}
	if logged.seen[shape] || len(logged.seen) >= antigravityQuotaShapesMax {
		logged.mu.Unlock()
		return
	}
	logged.seen[shape] = true
	logged.mu.Unlock()
	fmt.Printf("%s[antigravity-quota] quota reply shape: %s%s\n", colorCyan, shape, colorReset)
}

// antigravityCodeAssistAttemptNote is what one Code Assist read learned that
// the closed outcome code cannot carry: the account it resolved, whether a 200
// had nothing chartable, and — for codeassist_token_expired — the stored
// token's expiry (epoch ms). In memory only.
type antigravityCodeAssistAttemptNote struct {
	fingerprint   string
	unplottable   bool
	tokenExpiryMs int64
}

// antigravityCodeAssistAttempt holds the note of ONE probe. It travels in that
// probe's context (withAntigravityCodeAssistAttempt), never in a process-wide
// slot: a Refresh click and the run-debt worker can read at the same time, and
// a shared latest-note slot would let one take — or clear — the other's skew
// expiry, so a debt could renew while `agy` still holds the token valid.
type antigravityCodeAssistAttempt struct {
	mu   sync.Mutex
	note antigravityCodeAssistAttemptNote
}

type antigravityCodeAssistAttemptKey struct{}

// withAntigravityCodeAssistAttempt scopes a note to the probe run with the
// returned context; take reads it once the probe returns.
func withAntigravityCodeAssistAttempt(ctx context.Context) (context.Context, *antigravityCodeAssistAttempt) {
	if ctx == nil {
		ctx = context.Background()
	}
	attempt := &antigravityCodeAssistAttempt{}
	return context.WithValue(ctx, antigravityCodeAssistAttemptKey{}, attempt), attempt
}

// take returns the probe's note.
func (a *antigravityCodeAssistAttempt) take() antigravityCodeAssistAttemptNote {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.note
}

// setAntigravityCodeAssistAttempt records note on the probe ctx carries; a
// probe run without one notes nothing.
func setAntigravityCodeAssistAttempt(ctx context.Context, note antigravityCodeAssistAttemptNote) {
	if ctx == nil {
		return
	}
	attempt, _ := ctx.Value(antigravityCodeAssistAttemptKey{}).(*antigravityCodeAssistAttempt)
	if attempt == nil {
		return
	}
	attempt.mu.Lock()
	attempt.note = note
	attempt.mu.Unlock()
}

func noteAntigravityCodeAssistAttempt(ctx context.Context, fingerprint string, unplottable bool) {
	setAntigravityCodeAssistAttempt(ctx, antigravityCodeAssistAttemptNote{fingerprint: fingerprint, unplottable: unplottable})
}

func noteAntigravityCodeAssistTokenExpiry(ctx context.Context, expiryMs int64) {
	setAntigravityCodeAssistAttempt(ctx, antigravityCodeAssistAttemptNote{tokenExpiryMs: expiryMs})
}

/* ─────────────────────────── login renewal ─────────────────────────── */

// Login renewal outcomes — a closed set, logged on the device only.
const (
	antigravityLoginRenewed      = "renewed"
	antigravityLoginStillExpired = "still_expired"
	antigravityLoginSpaced       = "spaced"
	antigravityLoginUnavailable  = "unavailable"
)

// antigravityLoginRenewMinInterval spaces renewals across the whole agent
// process: the debt worker and the Refresh click share it, so the two together
// never start more than one renewal `agy models` per interval. A var so tests
// can pin it.
var antigravityLoginRenewMinInterval = 5 * time.Minute

// Seams so tests drive the renewal without a real `agy`.
var (
	renewAntigravityStoredLoginFn   = renewAntigravityStoredLogin
	refreshCLIAgentModelDiscoveryFn = refreshCLIAgentModelDiscovery
	antigravityLoginRenewDetectedFn = antigravityLoginRenewDetected
)

// antigravityLoginRenewal is the process-wide single flight and spacing clock.
var antigravityLoginRenewal struct {
	mu       sync.Mutex
	lastAt   time.Time
	inFlight chan struct{}
	result   string
}

// renewAntigravityStoredLogin makes `agy` renew the login it keeps in the OS
// keyring by running `agy models` — the child model discovery already runs
// every half hour, which spends no model turn — and reports, from the keyring's
// own expiry before and after, whether the stored token is now usable:
//
//   - renewed: valid past antigravityTokenExpirySkew (already before the child
//     when a concurrent `agy` run renewed it — nothing is spawned then);
//   - still_expired: the child ran and the stored token is still expired;
//   - spaced: a renewal started within antigravityLoginRenewMinInterval;
//   - unavailable: no executable or home, or the child failed to start or
//     timed out without the token changing.
//
// The child goes through refreshCLIAgentModelDiscovery → discoverCLIAgentModels,
// so it is registered as the agent's own child (beginAntigravityOwnChild), its
// run log never owes a refresh, and the fresh list lands in the model-probe
// cache. A caller arriving while a renewal runs shares its result.
//
// It reads nothing of the token but its expiry, and never logs or persists it.
func renewAntigravityStoredLogin(ctx context.Context, now time.Time) string {
	if ctx == nil {
		ctx = context.Background()
	}
	r := &antigravityLoginRenewal
	r.mu.Lock()
	if wait := r.inFlight; wait != nil {
		r.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return antigravityLoginUnavailable
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.result
	}
	if !r.lastAt.IsZero() && !now.Before(r.lastAt) && now.Sub(r.lastAt) < antigravityLoginRenewMinInterval {
		r.mu.Unlock()
		return antigravityLoginSpaced
	}
	r.lastAt = now
	done := make(chan struct{})
	r.inFlight = done
	r.mu.Unlock()

	result := antigravityLoginUnavailable
	defer func() {
		r.mu.Lock()
		r.result, r.inFlight = result, nil
		r.mu.Unlock()
		close(done)
	}()
	result = renewAntigravityStoredLoginOnce(ctx, now)
	return result
}

func renewAntigravityStoredLoginOnce(ctx context.Context, now time.Time) string {
	if antigravityStoredLoginUsable(ctx, now) {
		return antigravityLoginRenewed
	}
	detected, ok := antigravityLoginRenewDetectedFn()
	home, err := os.UserHomeDir()
	if !ok || err != nil || home == "" {
		return antigravityLoginUnavailable
	}
	started := time.Now()
	_, probed := refreshCLIAgentModelDiscoveryFn(ctx, "antigravity", detected, home, now)
	// The caller's clock, moved on by the time the child actually took.
	if antigravityStoredLoginUsable(ctx, now.Add(time.Since(started))) {
		return antigravityLoginRenewed
	}
	if !probed {
		return antigravityLoginUnavailable
	}
	return antigravityLoginStillExpired
}

// antigravityStoredLoginUsable reports whether the keyring holds a token the
// Code Assist read would send at now: present, and not inside the skew.
func antigravityStoredLoginUsable(ctx context.Context, now time.Time) bool {
	tok, ok := antigravityStoredToken(ctx)
	return ok && (tok.Expiry.IsZero() || now.Add(antigravityTokenExpirySkew).Before(tok.Expiry))
}

// antigravityLoginRenewDetected is the installed CLI as gatherCLIAgents detects
// it (resolved path, cached --version), so the list the renewal stores lands
// under the model-probe cache key the gather reads.
func antigravityLoginRenewDetected() (detectedCLIAgent, bool) {
	path := antigravityExecutablePath()
	if path == "" {
		return detectedCLIAgent{}, false
	}
	return detectedCLIAgent{Detected: true, Path: path, Name: "Antigravity", Version: cachedProbeVersion(path)}, true
}

// resetAntigravityLoginRenewal forgets the spacing clock. Tests only.
func resetAntigravityLoginRenewal() {
	r := &antigravityLoginRenewal
	r.mu.Lock()
	r.lastAt, r.result = time.Time{}, ""
	r.mu.Unlock()
}
