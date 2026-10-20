// cliagent_usage_grok_live.go — reads Grok Build's credit pool straight from
// xAI when the user clicks Refresh on the CLI Agents card, after a Grok run
// this agent spawned finishes (cliagent_usage_grok_freshness.go), and after a
// direct turn the user ran in their own shell (cliagent_usage_grok_direct.go).
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
//   - Only the click and the bounded run-completion debt call it, through one
//     per-account single flight (grokBillingReadOnce) — never the periodic
//     gather or a page load. The debt spends at most grokRunDebtMaxAttempts
//     reads per unpaid run.
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
//   - Every committed NUMERIC read advances the cache's capture generation
//     (on this process's epoch, as OpenCode's ledger does) and hints
//     terminal-service (cliagent_usage_propagate.go), so a reading that lands
//     after the session-end wake still reaches the card. An unmetered answer
//     inside the same open period never erases the numeric reading, and never
//     advances the generation.
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
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
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
	// NumericObservedAt is set only when UsedPercent was kept from an earlier
	// numeric read under a newer unmetered answer for the same open period
	// (ObservedAt is then that answer's): the percentage's own observation
	// time, which is what the card and every coverage check must use.
	NumericObservedAt string `json:"numericObservedAt,omitempty"`
	// Generation is the capture generation of the newest committed numeric
	// read, or of the clear that replaced it (cliUsageGeneration); additive,
	// so schema 1 readers ignore it.
	Generation *cliUsageGeneration `json:"generation,omitempty"`
	// Cleared reports that Generation versions a clear: this account's
	// numeric reading was replaced by an answer with no number (a new
	// period, or one whose kept figure has ended). The parser publishes and
	// recovery re-hints that generation although the card renders Unknown.
	Cleared bool `json:"cleared,omitempty"`
}

// numericObservedAt is when the cached percentage was observed, or zero when
// the entry carries none.
func (c grokBillingLiveCache) numericObservedAt() (time.Time, bool) {
	if c.UsedPercent == nil || !grokFiniteNumber(*c.UsedPercent) {
		return time.Time{}, false
	}
	stamp := c.ObservedAt
	if c.NumericObservedAt != "" {
		stamp = c.NumericObservedAt
	}
	t, err := time.Parse(time.RFC3339Nano, stamp)
	return t, err == nil
}

// hasNumericOnDemand reports whether the entry carries an on-demand pool that
// grokBillingMetrics renders as numbers (a cap with a usage figure inside it):
// a reading of its own, versioned even while the credits window is unmetered.
func (c grokBillingLiveCache) hasNumericOnDemand() bool {
	return grokBillingLiveCacheSnapshot(c, time.Time{}).HasOnDemandUsed
}

// sameOnDemand reports whether two entries carry the same on-demand figures.
func (c grokBillingLiveCache) sameOnDemand(other grokBillingLiveCache) bool {
	eq := func(a, b *float64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	return eq(c.OnDemandCap, other.OnDemandCap) && eq(c.OnDemandUsed, other.OnDemandUsed)
}

var (
	grokBillingLiveCacheMu sync.Mutex
	// grokGenerationRotated reports a committed live-cache write carrying this
	// process's epoch. Until then the cache may hold a generation an earlier
	// process published, so the parser omits UsageGeneration and the
	// propagator holds Grok hints (providerBlockedLocked).
	grokGenerationRotated atomic.Bool
)

// grokBillingLiveCacheLockWait bounds how long a cache transaction waits on
// another agent process holding the sibling lock. A var so tests can pin it.
var grokBillingLiveCacheLockWait = 2 * time.Second

// lockGrokBillingLiveCache takes the cache's cross-process sibling lock — the
// one the Codex and OpenCode generation transactions use — so two agent
// processes (Linux has no single-instance guard) never interleave a
// read-merge-rename or a rotation and lose a reading or an epoch. Callers hold
// grokBillingLiveCacheMu. ok is false when the lock file's directory cannot be
// made or another holder kept the lock past the wait; release must be called
// otherwise. A filesystem that offers no lock takes the unlocked path, as the
// other caches do.
func lockGrokBillingLiveCache(path string) (release func(), ok bool) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false
	}
	lock, outcome := acquireCrossProcessCacheLockUntil(path, time.Now().Add(grokBillingLiveCacheLockWait))
	if outcome == crossProcessLockContended {
		return nil, false
	}
	return func() {
		if lock != nil {
			_ = unlockFile(lock)
			_ = lock.Close()
		}
	}, true
}

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

// grokBillingReadGroup is the live read's single flight, shared by the Refresh
// click and the run-completion debt (cliagent_usage_grok_freshness.go), so the
// two never send two requests — or run two login renewals — at once. Keyed by
// account: a caller for a newly signed-in account never joins a read for the
// previous one.
var grokBillingReadGroup singleflight.Group

// grokBillingLiveReadFn is the read the flight runs, pinned to the flight's
// account; a seam so tests can count requests without a loopback server.
var grokBillingLiveReadFn = probeGrokBillingLiveFor

// grokBillingRead is one flight's result: its outcome and when it started.
type grokBillingRead struct {
	outcome   string
	startedAt time.Time
}

// grokBillingReadOnce runs one live read for fingerprint's account, joining a
// read already in flight for it. An `ok` reading retires any open run debt it
// covers. The flight runs on its own bounded context so a caller that stops
// waiting (the smoke's settle budget) never cancels a read another caller
// shares.
//
// notBefore (zero for the Refresh click) is the run completion the caller's
// read must postdate: a flight that started earlier — a click sent while the
// run was still live — cannot reflect that run, so the caller waits it out and
// sends its own, still one request at a time.
func grokBillingReadOnce(ctx context.Context, grokPath, fingerprint string, now func() time.Time, notBefore time.Time) string {
	for n := now(); notBefore.After(n); n = now() {
		if notBefore.Sub(n) > time.Millisecond {
			// A clock stepped back: a completion "in the future" must not make
			// every flight look too early.
			notBefore = n
			break
		}
		// grokCompletionMs rounded the completion up to the next millisecond;
		// a read sent before that boundary could not pay the debt, so wait
		// the sub-millisecond out instead of clamping it away.
		select {
		case <-ctx.Done():
			return liveProbeOutcomeTimeout
		case <-time.After(notBefore.Sub(n)):
		}
	}
	for {
		ch := grokBillingReadGroup.DoChan("grok:"+fingerprint, func() (any, error) {
			// Counted as this feature's in-flight work for as long as the read
			// runs, so a waiter that gave up early still sees its settle land.
			grokFreshnessInFlight.Add(1)
			defer grokFreshnessInFlight.Add(-1)
			startedAt := now()
			readCtx, cancel := context.WithTimeout(context.Background(), grokRunDebtReadTimeout)
			defer cancel()
			outcome := grokBillingLiveReadFn(readCtx, grokPath, fingerprint, now)
			if outcome == grokLiveOutcomeOK {
				// Only a numeric reading (the percentage or the on-demand pool)
				// pays a run's debt; one that names a period alone leaves the
				// card without a number. The reading is stamped with when its
				// request was sent, so it only covers runs that completed
				// before xAI could have answered.
				readAt := now()
				if snap, ok := loadGrokBillingLiveSnapshot(fingerprint, readAt); ok {
					if at, numeric := snap.numericAt(readAt); numeric {
						settleGrokRunFreshness(at.UnixMilli(), fingerprint)
					}
				}
			}
			return grokBillingRead{outcome: outcome, startedAt: startedAt}, nil
		})
		select {
		case res := <-ch:
			read, _ := res.Val.(grokBillingRead)
			if !notBefore.IsZero() && read.startedAt.Before(notBefore) {
				continue
			}
			return read.outcome
		case <-ctx.Done():
			// This caller stopped waiting; the flight goes on and its own ok
			// still settles any debt it covers. Not a failed read, so it spends
			// nothing.
			return liveProbeOutcomeTimeout
		}
	}
}

// probeGrokBillingLiveShared is the Refresh click's read: the signed-in
// account's flight.
func probeGrokBillingLiveShared(ctx context.Context, grokPath string, now func() time.Time) string {
	return grokBillingReadOnce(ctx, grokPath, grokAccountFingerprintFor(grokPersistentHome()), now, time.Time{})
}

// probeGrokBillingLive reads the credit pool from xAI for whichever account is
// signed in and caches it for the gather that follows.
func probeGrokBillingLive(ctx context.Context, grokPath string, now func() time.Time) string {
	return probeGrokBillingLiveFor(ctx, grokPath, "", now)
}

// probeGrokBillingLiveFor reads the credit pool from xAI and caches it for the
// gather that follows. Returns a closed outcome code. A non-empty expected
// pins the read to that account: if the login switched away from it, the probe
// returns account_changed before presenting — or renewing — the other
// account's credential.
func probeGrokBillingLiveFor(ctx context.Context, grokPath, expected string, now func() time.Time) string {
	base := grokPersistentHome()
	if base == "" {
		return grokLiveOutcomeNoLogin
	}
	fingerprint := grokAccountFingerprintFor(base)
	if fingerprint == "" {
		return grokLiveOutcomeNoAccount
	}
	switched := func() bool { return expected != "" && grokAccountFingerprintFor(base) != expected }
	if switched() {
		return liveProbeOutcomeAccountChanged
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
		// And across agent processes sharing this home (grokRenewLockName):
		// another agent's renewal in flight is this login being busy.
		renewLock, err := acquireGrokRenewLock(base)
		if err != nil {
			renewalBlocked = true
			return
		}
		defer releaseGrokLockFile(renewLock)
		// A live copy may already hold the account's newer credential; write it
		// back first, so the renewal below never redeems a refresh token that
		// copy superseded (past the grace window that signs the real home out).
		// Always run the CLI renewal after that write-back: this path is only
		// entered when the presented token is expired or just 401'd, so a
		// timestamp-fresh copy that xAI already rejected still needs a rotate
		// of its (newer) refresh chain, not a skip.
		reconcileGrokLoginLocked(base)
		// The write-back must have landed before the real home is renewed:
		// renewing a refresh token a live copy has superseded can sign the
		// real home out for good (grokRealHomeHoldsNewest). Until it lands,
		// this login is busy, not broken.
		// A copy of ANOTHER agent process, or an orphan one left, may hold it
		// instead; adopt it the same way. Unlike the keeper, the probe does
		// not stop at an adoption: it is here because the presented token was
		// expired or rejected, and an adopted chain that is too gets its
		// rotate NOW, in this probe — not on a later click.
		adoptForeignGrokRenewals(base)
		if !grokRealHomeHoldsNewest(base) {
			renewalBlocked = true
			return
		}
		if switched() {
			// Never rotate another account's login on this flight's behalf.
			return
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
	// The token was read from the home as it is now: a switch since the check
	// above means it is another account's.
	if switched() {
		return liveProbeOutcomeAccountChanged
	}
	// sentAt is when the request that answered was sent: xAI's figure is at
	// least that fresh, and a run that completed after it may not be in it.
	sentAt := now()
	config, err := fetchGrokBillingLive(ctx, client, token)
	if errors.Is(err, errGrokBillingUnauthorized) && !renewed {
		renew()
		if token, _, _ = grokFreshestPresentedToken(base, fingerprint, now()); token == "" {
			return grokLiveOutcomeNoLogin
		}
		if switched() {
			return liveProbeOutcomeAccountChanged
		}
		sentAt = now()
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

	observedAt := sentAt.UTC()
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

// saveGrokBillingLive commits one read to the cache and, when the commit
// advanced the capture generation, hints terminal-service about it. Reports
// whether the write committed.
func saveGrokBillingLive(entry grokBillingLiveCache) bool {
	committed, advanced, generation := commitGrokBillingLive(entry)
	if advanced {
		// Outside the cache lock: the propagator never holds its own lock
		// across a cache transaction, and this keeps the order one-way.
		noteCLIUsageObservationAdvanced(grokUsageProvider, generation)
	}
	return committed
}

// commitGrokBillingLive is the cache's read-merge-write transaction
// (write-then-rename under grokBillingLiveCacheMu and the cross-process
// sibling lock, lockGrokBillingLiveCache):
//
//   - An unmetered read for the same account and the same billing period,
//     while that period is open, keeps the earlier numeric percentage with its
//     own older observation time (NumericObservedAt) — the rule
//     grokNewestBillingObservation already applies across sources. Across a
//     rollover, or for another account, the unmetered read replaces it.
//   - A numeric read advances the generation when it commits a newer numeric
//     observation, a changed percentage, or a new billing period (another
//     account is all three). An unmetered read never advances it on the
//     percentage's behalf, except as a clear (below).
//   - A numeric on-demand pool is versioned on its own terms: a read carrying
//     one advances the generation when it is newer than the cached pool, its
//     figures changed, or there was no same-account numeric pool — so fresh
//     on-demand numbers never ride an older generation under a kept
//     percentage.
//   - A clear is versioned too: when the read brings no number and this
//     account's cached one (the percentage or the on-demand pool) is not
//     carried, the commit advances the generation and marks the entry
//     Cleared, so the card's switch to Unknown reaches terminal-service like
//     any other change. A later unmetered read for the same account keeps the
//     mark without advancing again. Dropping only this account's on-demand
//     pool under a kept percentage advances the generation the same way
//     (without the mark: the percentage still renders). Replacing another
//     account's numeric cache with an unmetered read is a clear as well.
//   - A same-account read older than the cached answer is superseded: it
//     reports committed (the cache already holds a newer answer) and writes
//     nothing.
//
// advanced reports a committed generation advance, and generation is it.
func commitGrokBillingLive(entry grokBillingLiveCache) (committed, advanced bool, generation cliUsageGeneration) {
	grokBillingLiveCacheMu.Lock()
	defer grokBillingLiveCacheMu.Unlock()
	path := grokBillingLiveCachePath()
	if path == "" || entry.AccountFingerprint == "" {
		return false, false, cliUsageGeneration{}
	}
	release, locked := lockGrokBillingLiveCache(path)
	if !locked {
		return false, false, cliUsageGeneration{}
	}
	defer release()
	var prev grokBillingLiveCache
	hasPrev := readJSONFile(path, &prev)
	sameAccount := hasPrev && prev.AccountFingerprint == entry.AccountFingerprint
	entryAt, entryErr := time.Parse(time.RFC3339Nano, entry.ObservedAt)
	if sameAccount && entryErr == nil {
		// Requests from two processes overlap outside the lock, so an older
		// answer can finish after a newer one committed: it is superseded, never
		// written over the newer cache and generation.
		if prevAt, err := time.Parse(time.RFC3339Nano, prev.ObservedAt); err == nil && entryAt.Before(prevAt) {
			return true, false, cliUsageGeneration{}
		}
	}
	prevNumericAt, prevNumeric := prev.numericObservedAt()
	// Another account's number is never kept, but replacing it still changes
	// the card (below).
	prevOtherNumeric := hasPrev && !sameAccount && (prevNumeric || prev.hasNumericOnDemand())
	prevNumeric = prevNumeric && sameAccount

	entry.NumericObservedAt = ""
	entry.Generation = nil
	entry.Cleared = false
	if hasPrev && prev.Generation != nil {
		// Counters only move forward within an epoch, whoever is signed in.
		g := *prev.Generation
		entry.Generation = &g
	}
	incomingNumeric := entry.UsedPercent != nil && grokFiniteNumber(*entry.UsedPercent)
	switch {
	case !incomingNumeric && prevNumeric && entryErr == nil && prevNumericAt.Before(entryAt):
		kept := grokBillingLiveCacheSnapshot(prev, prevNumericAt)
		incoming := grokBillingLiveCacheSnapshot(entry, entryAt)
		if kept.samePeriod(incoming) && entryAt.Before(incoming.PeriodEnd) {
			percent := *prev.UsedPercent
			entry.UsedPercent = &percent
			entry.NumericObservedAt = prevNumericAt.UTC().Format(time.RFC3339Nano)
		}
	case incomingNumeric:
		advanced = !prevNumeric ||
			(entryErr == nil && entryAt.After(prevNumericAt)) ||
			*prev.UsedPercent != *entry.UsedPercent ||
			prev.PeriodType != entry.PeriodType || prev.PeriodEnd != entry.PeriodEnd
	}
	if !advanced && entry.hasNumericOnDemand() {
		// The cached pool is stamped with the entry's ObservedAt (the newest
		// answer), never with a kept percentage's NumericObservedAt.
		prevPoolAt, prevPoolErr := time.Parse(time.RFC3339Nano, prev.ObservedAt)
		advanced = !sameAccount || !prev.hasNumericOnDemand() || prevPoolErr != nil ||
			(entryErr == nil && entryAt.After(prevPoolAt)) ||
			!prev.sameOnDemand(entry)
	}
	if !advanced && sameAccount && !entry.hasNumericOnDemand() {
		switch {
		case entry.UsedPercent == nil && (prevNumeric || prev.hasNumericOnDemand()):
			advanced, entry.Cleared = true, true
		case entry.UsedPercent == nil && prev.Cleared:
			entry.Cleared = true
		case prev.hasNumericOnDemand():
			// The pool is gone under a kept percentage: the card drops its
			// on-demand row, so that removal is versioned too.
			advanced = true
		}
	}
	if !advanced && prevOtherNumeric {
		// Incoming carries no number (a numeric read advanced above): the card
		// moves from the other account's figure to Unknown, a versioned clear.
		advanced, entry.Cleared = true, true
	}
	epoch := codexProcessGenerationEpoch.Load()
	if advanced {
		if entry.Generation == nil || entry.Generation.Epoch != epoch {
			entry.Generation = &cliUsageGeneration{Epoch: epoch, Counter: 1}
		} else {
			entry.Generation.Counter++
		}
	}
	if !writeJSONFileAtomic(path, entry) {
		return false, false, cliUsageGeneration{}
	}
	if entry.Generation != nil && entry.Generation.Epoch == epoch {
		grokGenerationRotated.Store(true)
	}
	if advanced {
		generation = *entry.Generation
	}
	return true, advanced, generation
}

// rotateGrokBillingLiveGeneration moves a cache an earlier process wrote onto
// this process's epoch in one write, when it still holds fingerprint's
// reading. It returns the generation the cache carries afterwards; refused
// reports a rotation that did not commit (the caller retries).
func rotateGrokBillingLiveGeneration(fingerprint string) (generation *cliUsageGeneration, refused bool) {
	grokBillingLiveCacheMu.Lock()
	defer grokBillingLiveCacheMu.Unlock()
	path := grokBillingLiveCachePath()
	var entry grokBillingLiveCache
	if path == "" || fingerprint == "" {
		return nil, false
	}
	release, locked := lockGrokBillingLiveCache(path)
	if !locked {
		return nil, true
	}
	defer release()
	if !readJSONFile(path, &entry) || entry.AccountFingerprint != fingerprint {
		return nil, false
	}
	epoch := codexProcessGenerationEpoch.Load()
	if entry.Generation == nil || entry.Generation.Epoch != epoch {
		entry.Generation = &cliUsageGeneration{Epoch: epoch, Counter: 1}
		if !writeJSONFileAtomic(path, entry) {
			return nil, true
		}
	}
	grokGenerationRotated.Store(true)
	g := *entry.Generation
	return &g, false
}

// Vars so tests can pin them. A refused rotation is retried on the same short
// ladder as OpenCode's ledger commit.
var (
	grokRecoveryLadder      = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute}
	grokRecoveryMaxAttempts = len(grokRecoveryLadder) + 1
	grokRecoveryAfterFunc   = time.AfterFunc
)

// grokRecoveryGeneration is the rotated generation of a live cache that still
// holds a numeric reading for the account signed in now, in a period still
// open at now (grokBillingMetrics renders it as a number) — or a versioned
// clear of one (Cleared) — or nil. A reading
// committed just before an update hand-off — whose hint died with the old
// process — is hinted again this way; the backend skips a generation it
// already applied. refused reports a rotation write that did not commit.
func grokRecoveryGeneration(now time.Time) (generation *cliUsageGeneration, refused bool) {
	fingerprint := grokUsageCurrentFingerprint()
	view, ok := loadGrokBillingLive(fingerprint, now)
	if !ok || !(view.cleared || grokBillingMetricsNumeric(grokBillingMetrics(view.snap, now))) {
		return nil, false
	}
	return rotateGrokBillingLiveGeneration(fingerprint)
}

// recoverGrokUsageGeneration runs startup recovery, hinting a recovered
// generation and retrying a refused rotation on grokRecoveryLadder — startup
// asks only once.
func recoverGrokUsageGeneration(attempt int) {
	g, refused := grokRecoveryGeneration(grokUsageFreshnessNow())
	if g != nil {
		noteCLIUsageObservationAdvanced(grokUsageProvider, *g)
	}
	if !refused {
		return
	}
	delay, more := refreshRetryDelayForAttempt(attempt, grokRecoveryMaxAttempts, grokRecoveryLadder)
	if !more {
		fmt.Printf("%s[cli-usage] grok refresh: recovery_abandoned%s\n", colorYellow, colorReset)
		return
	}
	grokRecoveryAfterFunc(delay, func() { recoverGrokUsageGeneration(attempt + 1) })
}

// grokBillingLiveView is one read of the cache for an account.
type grokBillingLiveView struct {
	// snap is the reading to publish: the kept numeric reading (with its own
	// ObservedAt) while its period is open, else the newest answer.
	snap grokBillingSnapshot
	// generation is the cache's committed capture generation (nil when none).
	generation *cliUsageGeneration
	// cleared reports that generation versions a clear (grokBillingLiveCache.Cleared).
	cleared bool
	// answeredAt is when xAI last answered, metered or not: an unmetered
	// answer still proves billing was read after an older log record.
	answeredAt time.Time
}

// loadGrokBillingLiveSnapshot returns the cached live reading for the account
// that is signed in now. A reading from another account is never replayed, and
// a numeric reading kept under a newer unmetered answer is returned only while
// its period is still open at now — a rolled-over number never covers a run.
func loadGrokBillingLiveSnapshot(fingerprint string, now time.Time) (grokBillingSnapshot, bool) {
	view, ok := loadGrokBillingLive(fingerprint, now)
	return view.snap, ok
}

// loadGrokBillingLive is loadGrokBillingLiveSnapshot with the generation.
func loadGrokBillingLive(fingerprint string, now time.Time) (grokBillingLiveView, bool) {
	if fingerprint == "" {
		return grokBillingLiveView{}, false
	}
	var entry grokBillingLiveCache
	if !readJSONFile(grokBillingLiveCachePath(), &entry) || entry.AccountFingerprint != fingerprint {
		return grokBillingLiveView{}, false
	}
	observed, err := time.Parse(time.RFC3339Nano, entry.ObservedAt)
	if err != nil {
		return grokBillingLiveView{}, false
	}
	snap := grokBillingLiveCacheSnapshot(entry, observed)
	if entry.NumericObservedAt != "" && snap.HasUsedPercent {
		numericAt, err := time.Parse(time.RFC3339Nano, entry.NumericObservedAt)
		switch {
		case err == nil && (!snap.HasPeriodEnd || now.Before(snap.PeriodEnd)):
			snap.ObservedAt = numericAt
			// The on-demand pool's state came with the newest answer, not
			// with the kept percentage, so it keeps that answer's
			// observation time — an answer that omitted the pool dates its
			// removal, so an older pool elsewhere never fills it back in.
			snap.OnDemandObservedAt = observed
		default:
			// The kept number belongs to a period that is over (or its stamp
			// is unreadable): publish the newest answer, unmetered.
			snap.UsedPercent, snap.HasUsedPercent = 0, false
		}
	}
	view := grokBillingLiveView{snap: snap, generation: entry.Generation, answeredAt: observed}
	view.cleared = entry.Cleared && entry.Generation != nil && !snap.HasUsedPercent && !snap.HasOnDemandUsed
	return view, true
}

// grokBillingLiveCacheSnapshot decodes a cache entry stamped at observed.
func grokBillingLiveCacheSnapshot(entry grokBillingLiveCache, observed time.Time) grokBillingSnapshot {
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
	return snap
}
