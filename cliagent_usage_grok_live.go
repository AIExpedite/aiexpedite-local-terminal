// cliagent_usage_grok_live.go — reads Grok Build's credit pool straight from
// xAI when the user clicks Refresh on the CLI Agents card.
//
// Why this exists:
//
//	Grok's only local record of its credit pool is the `billing: fetched credits
//	config` line the interactive TUI writes to unified.jsonl. Headless runs
//	(`grok -p`, ACP sessions) never fetch it, so a device that only runs Grok
//	through AI Expedite reports the same week-old line forever — the card showed
//	a 9-day-old reading for a billing period that had already ended.
//
//	The TUI gets that figure from one request, and this file makes the same
//	request with the login the CLI already stored: GET
//	https://cli-chat-proxy.grok.com/v1/billing?format=credits. The response is
//	the same `config` object the log line carries, so it is decoded with the
//	log's own typed struct and rendered by the log's own metric builder.
//
// Boundaries:
//
//   - Only the signed live-probe refresh calls it (a user click), never the
//     periodic gather or a page load.
//   - The endpoint is a pinned HTTPS constant; no proxy inheritance, redirects
//     refused, 64 KB body cap, decoded into an allow-listed struct.
//   - The token is the one Grok's own resolver presents (same scope precedence
//     as grokAuthExpiry) and is never logged, cached or published.
//   - The agent never mints tokens itself. When the access token has expired
//     (or xAI answers 401/403), `grok models` runs once against the real home
//     so GROK renews its login, and the request is retried once with whatever
//     Grok wrote. The renewed file is then reconciled into every live copy of
//     the login (grok_login_keeper.go), which is also what keeps the login
//     alive between clicks.
//   - The probe presents the freshest unexpired token across the real home
//     AND every live copy (an ACP session that renewed ITS copy holds a
//     fresher credential; reading rotates nothing), and takes its renewal turn
//     for at most grokLoginRenewGap when the keeper is mid-renewal: a probe
//     that spends its budget waiting returns a transport error for what is
//     really "the login is busy".
//   - What persists is the normalized reading only (percent, period, on-demand
//     pool), scoped to the account fingerprint, in grok_billing_live.json.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	grokBillingLiveEndpoint = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
	grokBillingLiveTimeout  = 8 * time.Second
	grokBillingLiveMaxBody  = 64 * 1024
	// grokLoginRenewTimeout bounds the one `grok models` run that lets Grok
	// renew an expired access token.
	grokLoginRenewTimeout = 15 * time.Second
	// grokTokenExpirySkew treats a token this close to expiry as expired, so
	// the request is not sent with a credential that dies in flight.
	grokTokenExpirySkew = time.Minute
	// grokLoginRenewGap bounds how long the probe waits for its turn to renew
	// when another renewal (the keeper's) is in flight: waiting the whole
	// probe budget would only turn "busy" into a timeout.
	grokLoginRenewGap = 2 * time.Second

	grokBillingLiveSchemaVersion = 1
)

// grokLiveProbe outcome codes. Closed set: they are logged on the device and
// never carry vendor text.
const (
	grokLiveOutcomeOK           = "ok"
	grokLiveOutcomeNoLogin      = "no_login"
	grokLiveOutcomeUnauthorized = "unauthorized"
	grokLiveOutcomeHTTPError    = "http_error"
	grokLiveOutcomeBadResponse  = "bad_response"
	grokLiveOutcomeNoAccount    = "no_account"
	grokLiveOutcomeWriteFailed  = "write_failed"
	// The only usable token was refused or expired, and a renewal could not
	// run because another renewal of the login was in flight for the whole
	// gap. Nothing is wrong with the login; the next click reads it renewed.
	grokLiveOutcomeLoginBusy = "login_busy"
)

// grokBillingLiveCache is the persisted reading. Every field is normalized;
// nothing from the response beyond the credit pool survives.
type grokBillingLiveCache struct {
	SchemaVersion      int      `json:"schemaVersion"`
	ObservedAt         string   `json:"observedAt"`
	AccountFingerprint string   `json:"accountFingerprint"`
	UsedPercent        *float64 `json:"usedPercent,omitempty"`
	PeriodType         string   `json:"periodType,omitempty"`
	PeriodEnd          string   `json:"periodEnd,omitempty"`
	OnDemandCap        *float64 `json:"onDemandCap,omitempty"`
	OnDemandUsed       *float64 `json:"onDemandUsed,omitempty"`
}

var grokBillingLiveCacheMu sync.Mutex

// grokBillingLiveCachePath is the cache location inside the agent's data dir.
// AIEXPEDITE_GROK_BILLING_LIVE_CACHE overrides it (tests isolate from the real
// machine cache).
func grokBillingLiveCachePath() string {
	if p := os.Getenv("AIEXPEDITE_GROK_BILLING_LIVE_CACHE"); p != "" {
		return p
	}
	return filepath.Join(GetConfigDir(), "grok_billing_live.json")
}

// grokBillingLiveURL is a var so tests can point the probe at a loopback
// server. Production never reassigns it.
var grokBillingLiveURL = grokBillingLiveEndpoint

// runGrokLoginRenewal lets Grok renew its own login by running `grok models`
// against the real home. The output is discarded: this run exists only for the
// side effect Grok performs on its own credential. A var so tests never spawn
// the real CLI.
var runGrokLoginRenewal = renewGrokLoginWithCLI

// renewGrokLoginWithCLI is the production renewal: one `grok models` run on
// the given home, which makes the CLI redeem its refresh token and rewrite
// auth.json.
func renewGrokLoginWithCLI(ctx context.Context, grokPath, base string) {
	if grokPath == "" {
		return
	}
	renewCtx, cancel := context.WithTimeout(ctx, grokLoginRenewTimeout)
	defer cancel()
	// grokLoginRenewEnv: the maintenance-smoke sanitizer strips every GROK_*
	// sink and XAI_API_KEY, so the only credential this child can use is the
	// cached login it is here to renew; GROK_HOME is pinned to the home the
	// probe reads; and the CLI's early-invalidation horizon is raised so it
	// refreshes now, not only within five minutes of expiry.
	_, _ = cliAgentModelProbeRunner(renewCtx, grokPath, grokLoginRenewEnv(base), "models")
}

// grokPresentedToken returns the access token Grok's own resolver would send,
// walked in the same scope precedence as grokAuthExpiry, and its expiry when
// the entry states one. It reads the same files as grokHasUsableToken
// (`auth.json`, else the legacy `cached_token.json`) and falls back to the
// id_token exactly as that resolver does, so a login the card counts as signed
// in is never reported here as no_login.
func grokPresentedToken(base string) (token string, expiresAt time.Time, hasExpiry bool) {
	raw, err := os.ReadFile(filepath.Join(base, "auth.json"))
	if err != nil {
		raw, err = os.ReadFile(filepath.Join(base, "cached_token.json"))
		if err != nil {
			return "", time.Time{}, false
		}
	}
	withExpiry := func(token, expires string) (string, time.Time, bool) {
		if t, err := time.Parse(time.RFC3339, expires); err == nil {
			return token, t, true
		}
		return token, time.Time{}, false
	}

	var scoped map[string]struct {
		ExpiresAt   string `json:"expires_at"`
		Key         string `json:"key"`
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
	}
	if json.Unmarshal(raw, &scoped) == nil && len(scoped) > 0 {
		keys := make([]string, 0, len(scoped))
		for k := range scoped {
			keys = append(keys, k)
		}
		for _, k := range grokScopeKeysByPrecedence(keys) {
			entry := scoped[k]
			// Same rule as grokHasUsableToken: a scope without a token is
			// skipped, and the first token-bearing scope is the one Grok
			// presents, picked in grokScopedCredential's order.
			token = grokScopedCredential(entry.Key, entry.Token, entry.AccessToken, entry.IDToken)
			if token == "" {
				continue
			}
			return withExpiry(token, entry.ExpiresAt)
		}
	}

	// Flat / legacy layout (a nested `cached_token` object also lands here: it
	// unmarshals into the scoped map as a key no scope precedence selects): one
	// account, access credential before id_token, the order grokAuthExpiry uses.
	var flat struct {
		ExpiresAt   string `json:"expires_at"`
		Key         string `json:"key"`
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
		CachedToken struct {
			AccessToken string `json:"access_token"`
			IDToken     string `json:"id_token"`
		} `json:"cached_token"`
	}
	if json.Unmarshal(raw, &flat) != nil {
		return "", time.Time{}, false
	}
	token = grokFlatCredential(
		flat.AccessToken, flat.Token, flat.Key,
		flat.CachedToken.AccessToken,
		flat.IDToken, flat.CachedToken.IDToken,
	)
	if token == "" {
		return "", time.Time{}, false
	}
	return withExpiry(token, flat.ExpiresAt)
}

// grokFreshestPresentedToken returns the token to present for base's account:
// the real home's, or a LIVE isolated copy's when that copy holds a later
// expiry for the same account. A copy is a byte copy of the real home's login
// taken at session start (setupIsolatedGrokHome), and only Grok's own renewal
// inside that session moves its expiry forward — so a copy with a later expiry
// is the same login, renewed. Copies are read only; nothing here rotates a
// token. Only copies whose credential names the same account as base are
// candidates, so a session that re-logged in as someone else never lends its
// token to this account's reading.
func grokFreshestPresentedToken(base, fingerprint string, now time.Time) (token string, expiresAt time.Time, hasExpiry bool) {
	token, expiresAt, hasExpiry = grokPresentedToken(base)
	unexpired := func(exp time.Time) bool { return now.Add(grokTokenExpirySkew).Before(exp) }
	for _, copyHome := range grokLogin.liveCopies() {
		if copyHome == base || grokAccountFingerprintFor(copyHome) != fingerprint {
			continue
		}
		ct, cexp, chas := grokPresentedToken(copyHome)
		if ct == "" || !chas {
			// A copy that states no expiry cannot be shown to be fresher than
			// what the real home holds; leave it.
			continue
		}
		switch {
		case token == "":
			token, expiresAt, hasExpiry = ct, cexp, true
		case !hasExpiry:
			// The real home's token states no expiry; a copy that is
			// demonstrably unexpired outranks a credential of unknown age.
			if unexpired(cexp) {
				token, expiresAt, hasExpiry = ct, cexp, true
			}
		case cexp.After(expiresAt):
			token, expiresAt, hasExpiry = ct, cexp, true
		}
	}
	return token, expiresAt, hasExpiry
}

// grokBillingLiveClient refuses redirects and proxies, mirroring the Claude
// usage probe: the only host this request may reach is the pinned endpoint.
func grokBillingLiveClient() *http.Client {
	return &http.Client{
		Timeout:   grokBillingLiveTimeout,
		Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

var errGrokBillingUnauthorized = errors.New("grok billing: unauthorized")

// fetchGrokBillingLive performs one request and decodes the credit pool.
func fetchGrokBillingLive(ctx context.Context, client *http.Client, token string) (grokBillingConfig, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, grokBillingLiveURL, nil)
	if err != nil {
		return grokBillingConfig{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return grokBillingConfig{}, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, grokBillingLiveMaxBody))
		_ = resp.Body.Close()
	}()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return grokBillingConfig{}, errGrokBillingUnauthorized
	case resp.StatusCode != http.StatusOK:
		return grokBillingConfig{}, fmt.Errorf("grok billing: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, grokBillingLiveMaxBody))
	if err != nil {
		return grokBillingConfig{}, err
	}
	var decoded struct {
		Config grokBillingConfig `json:"config"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return grokBillingConfig{}, err
	}
	return decoded.Config, nil
}

// probeGrokBillingLive reads the credit pool from xAI and caches it for the
// gather that follows. Returns a closed outcome code.
func probeGrokBillingLive(ctx context.Context, grokPath string, now func() time.Time) string {
	base := grokPersistentHome()
	if base == "" {
		return grokLiveOutcomeNoLogin
	}
	fingerprint := grokAccountFingerprintFor(base)
	if fingerprint == "" {
		return grokLiveOutcomeNoAccount
	}
	client := grokBillingLiveClient()
	defer client.CloseIdleConnections()

	// renewed: a renewal was attempted (it runs at most once per probe).
	// renewalBlocked: the one renewal could not run because a copy of the
	// login was live for the whole gap — the distinction between "xAI refused
	// this login" and "this login is busy".
	renewed, renewalBlocked := false, false
	renew := func() {
		renewed = true
		// One renewal of this login at a time: the keeper (grok_login_keeper.go)
		// may be renewing it right now, and two rotations of one refresh token
		// minutes apart sign the loser out. The wait for the turn is bounded by
		// grokLoginRenewGap (and by the probe's own budget).
		gapCtx, cancel := context.WithTimeout(ctx, grokLoginRenewGap)
		defer cancel()
		release, ok := grokLogin.beginRenewal(gapCtx)
		if !ok {
			renewalBlocked = true
			return
		}
		defer release()
		// A live copy may already hold the account's newer credential; write it
		// back first, so the renewal below never redeems a refresh token that
		// copy superseded (past the grace window that signs the real home out).
		// If that reconciliation produced a token the request can use, the
		// renewal is not needed at all.
		if reconcileGrokLoginLocked(base) > 0 {
			if _, expiresAt, hasExpiry := grokFreshestPresentedToken(base, fingerprint, now()); hasExpiry && now().Add(grokTokenExpirySkew).Before(expiresAt) {
				return
			}
		}
		runGrokLoginRenewal(ctx, grokPath, base)
		// Every live copy of the login now holds a superseded refresh token;
		// hand them the renewed file before xAI's grace window closes — still
		// under the lock, so nothing renews between the rotation and the fan-out.
		reconcileGrokLoginLocked(base)
	}
	token, expiresAt, hasExpiry := grokFreshestPresentedToken(base, fingerprint, now())
	if token == "" {
		return grokLiveOutcomeNoLogin
	}
	if hasExpiry && !now().Add(grokTokenExpirySkew).Before(expiresAt) {
		renew()
		if token, _, _ = grokFreshestPresentedToken(base, fingerprint, now()); token == "" {
			return grokLiveOutcomeNoLogin
		}
	}
	config, err := fetchGrokBillingLive(ctx, client, token)
	if errors.Is(err, errGrokBillingUnauthorized) && !renewed {
		renew()
		if token, _, _ = grokFreshestPresentedToken(base, fingerprint, now()); token == "" {
			return grokLiveOutcomeNoLogin
		}
		config, err = fetchGrokBillingLive(ctx, client, token)
	}
	switch {
	case errors.Is(err, errGrokBillingUnauthorized) && renewalBlocked:
		return grokLiveOutcomeLoginBusy
	case errors.Is(err, errGrokBillingUnauthorized):
		return grokLiveOutcomeUnauthorized
	case err != nil:
		var syntax *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &syntax) || errors.As(err, &typeErr) {
			return grokLiveOutcomeBadResponse
		}
		return grokLiveOutcomeHTTPError
	}

	observedAt := now().UTC()
	var rec grokBillingRecord
	// Nanosecond precision: Grok's own log stamps carry milliseconds, and the
	// parser's newest-wins comparison must not see a same-second log record as
	// newer than this reading because the fraction was truncated.
	rec.TS = observedAt.Format(time.RFC3339Nano)
	rec.Ctx.Config = config
	snap, ok := grokBillingSnapshotFromRecord(rec)
	if !ok || (!snap.HasUsedPercent && snap.PeriodType == "") {
		return grokLiveOutcomeBadResponse
	}
	// The account could have changed while the request was in flight; a reading
	// is only cached under the account that is still signed in.
	if grokAccountFingerprintFor(base) != fingerprint {
		return grokLiveOutcomeNoAccount
	}
	if !saveGrokBillingLive(grokBillingLiveFromSnapshot(snap, fingerprint)) {
		return grokLiveOutcomeWriteFailed
	}
	return grokLiveOutcomeOK
}

func grokBillingLiveFromSnapshot(snap grokBillingSnapshot, fingerprint string) grokBillingLiveCache {
	out := grokBillingLiveCache{
		SchemaVersion:      grokBillingLiveSchemaVersion,
		ObservedAt:         snap.ObservedAt.UTC().Format(time.RFC3339Nano),
		AccountFingerprint: fingerprint,
		PeriodType:         clampAntigravityQuotaField(snap.PeriodType, antigravityQuotaMaxBucketFieldBytes),
	}
	if snap.HasUsedPercent {
		out.UsedPercent = floatPtr(snap.UsedPercent)
	}
	if snap.HasPeriodEnd {
		out.PeriodEnd = snap.PeriodEnd.UTC().Format(time.RFC3339)
	}
	if snap.HasOnDemand {
		out.OnDemandCap = floatPtr(snap.OnDemandCap)
		if snap.HasOnDemandUsed {
			out.OnDemandUsed = floatPtr(snap.OnDemandUsed)
		}
	}
	return out
}

// saveGrokBillingLive atomically replaces the cache (write-then-rename).
func saveGrokBillingLive(entry grokBillingLiveCache) bool {
	grokBillingLiveCacheMu.Lock()
	defer grokBillingLiveCacheMu.Unlock()
	path := grokBillingLiveCachePath()
	if path == "" || entry.AccountFingerprint == "" {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false
	}
	out, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return false
	}
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return false
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false
	}
	return true
}

// loadGrokBillingLiveSnapshot returns the cached live reading for the account
// that is signed in now. A reading from another account is never replayed.
func loadGrokBillingLiveSnapshot(fingerprint string) (grokBillingSnapshot, bool) {
	if fingerprint == "" {
		return grokBillingSnapshot{}, false
	}
	var entry grokBillingLiveCache
	if !readJSONFile(grokBillingLiveCachePath(), &entry) || entry.AccountFingerprint != fingerprint {
		return grokBillingSnapshot{}, false
	}
	observed, err := time.Parse(time.RFC3339Nano, entry.ObservedAt)
	if err != nil {
		return grokBillingSnapshot{}, false
	}
	snap := grokBillingSnapshot{ObservedAt: observed, PeriodType: entry.PeriodType}
	if entry.UsedPercent != nil && grokFiniteNumber(*entry.UsedPercent) {
		snap.UsedPercent = clampPercent(*entry.UsedPercent)
		snap.HasUsedPercent = true
	}
	if end, err := time.Parse(time.RFC3339, entry.PeriodEnd); err == nil {
		snap.PeriodEnd = end
		snap.HasPeriodEnd = true
	}
	if entry.OnDemandCap != nil && *entry.OnDemandCap > 0 && grokFiniteNumber(*entry.OnDemandCap) {
		snap.HasOnDemand = true
		snap.OnDemandCap = *entry.OnDemandCap
		if entry.OnDemandUsed != nil && grokFiniteNumber(*entry.OnDemandUsed) &&
			*entry.OnDemandUsed >= 0 && *entry.OnDemandUsed <= *entry.OnDemandCap {
			snap.HasOnDemandUsed = true
			snap.OnDemandUsed = *entry.OnDemandUsed
		}
	}
	return snap, true
}
