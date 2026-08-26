// cliagent_usage_grok.go — xAI Grok Build CLI usage parser.
//
// Grok Build CLI stores account/auth state under $GROK_HOME or ~/.grok (the
// directory it writes after `grok login`). The cached-token JSON file is the
// authoritative signal that local subscription auth is available to the ACP
// `authenticate` flow — when present, AI Expedite's orchestrator selects
// `cached_token` and usage ties to the terminal computer user's Grok / X
// account.
//
// xAI does not expose request- or token-level quotas via the on-disk
// credential files, so the capacity rows are flagged Unknown — the dashboard
// shows them as a dashed gauge ("metric exists but unobservable") rather than
// dropping them entirely.
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Grok's own token resolver (read_grok_token in https://x.ai/cli/install.sh)
// prefers the OIDC scope and only falls back to the legacy sign-in scope, so we
// must read the expiry/identity from the SAME scoped token Grok will present.
// A plain alphabetical sort is wrong here: "accounts.x.ai" (legacy) sorts before
// "auth.x.ai" (OIDC), which would pick the legacy sibling and let a stale OIDC
// token slip through — the very case a re-login warning exists to catch.
const (
	// grokExactOIDCScope is the CLI's own OIDC scope — the exact key
	// read_grok_token resolves first (OIDC_SCOPE in https://x.ai/cli/install.sh).
	// We rank this exact match ahead of everything so a stale token for a
	// DIFFERENT xAI client that happens to share the "https://auth.x.ai" host
	// (a sibling "https://auth.x.ai::<other-client>" entry) can't sort ahead of
	// the credential Grok will actually present.
	grokExactOIDCScope   = "https://auth.x.ai::b1a00492-073a-47ea-816f-4c329264a828"
	grokExactLegacyScope = "https://accounts.x.ai/sign-in"
)

// grokScopeKeysByPrecedence returns only scopes Grok's resolver can present:
// the exact CLI OIDC scope first, then the exact legacy sign-in scope.
// read_grok_token resolves only OIDC_SCOPE then LEGACY_SCOPE
// (https://x.ai/cli/install.sh); it never scans arbitrary siblings. Filtering
// here prevents another xAI client's credential from making this Grok CLI look
// authenticated or from supplying a misleading expiry/identity.
func grokScopeKeysByPrecedence(keys []string) []string {
	hasExactOIDC := false
	hasExactLegacy := false
	for _, key := range keys {
		switch {
		case key == grokExactOIDCScope:
			hasExactOIDC = true
		case key == grokExactLegacyScope:
			hasExactLegacy = true
		}
	}
	ordered := make([]string, 0, 2)
	if hasExactOIDC {
		ordered = append(ordered, grokExactOIDCScope)
	}
	if hasExactLegacy {
		ordered = append(ordered, grokExactLegacyScope)
	}
	return ordered
}

type grokUsageParser struct{}

func (grokUsageParser) Provider() string { return "grok" }

// grokAuthFile mirrors the fields Grok Build CLI writes under
// $GROK_HOME/auth.json (or the cached_token sibling). Only the identity /
// plan fields are needed for dedup + plan display.
type grokAuthFile struct {
	Account      string `json:"account"`
	Email        string `json:"email"`
	UserID       string `json:"user_id"`
	UserName     string `json:"username"`
	Plan         string `json:"plan"`
	PlanType     string `json:"plan_type"`
	Tier         string `json:"tier"`
	Subscription string `json:"subscription"`
	OrgID        string `json:"org_id"`
	CachedToken  struct {
		IDToken     string `json:"id_token"`
		AccessToken string `json:"access_token"`
		Account     string `json:"account"`
		Email       string `json:"email"`
		Subject     string `json:"sub"`
	} `json:"cached_token"`
}

type grokIDTokenClaims struct {
	Account  string `json:"account"`
	Email    string `json:"email"`
	UserID   string `json:"user_id"`
	UserName string `json:"username"`
	Subject  string `json:"sub"`
	Plan     string `json:"plan"`
	PlanType string `json:"plan_type"`
	OrgID    string `json:"org_id"`
}

func (p grokUsageParser) Parse(home string, detected detectedCLIAgent, now time.Time) (*cliAgentUsage, bool) {
	base := firstNonEmpty(os.Getenv("GROK_HOME"), expandHome(home, ".grok"))
	if base == "" {
		return nil, false
	}

	dataSource := "~/.grok"
	if os.Getenv("GROK_HOME") != "" {
		dataSource = "$GROK_HOME"
	}

	usage := &cliAgentUsage{
		Provider:    p.Provider(),
		Name:        firstNonEmpty(detected.Name, "Grok Build"),
		Version:     detected.Version,
		Path:        detected.Path,
		DataSource:  dataSource,
		CollectedAt: now.UTC().Format(time.RFC3339),
	}

	account, plan := readGrokAccountAndPlan(base)
	usage.Account = account
	usage.Plan = plan
	usage.AccountFingerprint = fingerprintAccount(p.Provider(), usage.Account)

	// Grok's real quota is a credit pool over a billing period, not the
	// request/token counters this card used to guess at. The CLI logs the pool
	// it fetched for itself; when that log has a usable record we plot it and
	// take the subscription tier from the same record (the auth file rarely
	// carries a plan).
	if snap, ok := readGrokBillingSnapshot(base, grokIdentityCandidates(base)); ok {
		if metrics := grokBillingMetrics(snap, now); len(metrics) > 0 {
			usage.Metrics = metrics
			usage.Plan = firstNonEmpty(usage.Plan, snap.SubscriptionTier)
		}
	}
	if len(usage.Metrics) == 0 {
		// No billing line yet (fresh install, or the CLI has not fetched credits
		// since the log rotated). Keep a placeholder row so the agent still
		// appears on the card as "limit exists, value unobservable" rather than
		// vanishing.
		usage.Metrics = []cliAgentUsageMetric{grokUnknownCreditsMetric()}
	}

	// Notice priority is by whether the condition blocks requests RIGHT NOW, not
	// simply auth-before-limit:
	//   1. expired/missing login  — blocks every request AND can't be
	//      re-established headlessly (grok agent stdio falls back to an
	//      interactive device-code sign-in the terminal can't show);
	//   2. reached usage limit     — quota is exhausted, so requests are blocked
	//      until it resets;
	//   3. login expires soon      — a heads-up only; the token still works, so a
	//      request-blocking limit must not be hidden behind it;
	//   4. approaching usage limit — the other heads-up.
	// Grok's token is short-lived relative to the other agents' credentials,
	// which is why we fold the auth check into the regular usage check instead of
	// only failing at session start.
	//
	// xAI exposes no numeric quota, so the capacity bars stay Unknown — but Grok
	// DOES push a discrete usage-limit warning on the streaming-json output,
	// which captureGrokUsageLimitLine caches (approaching → warning, reached →
	// error).
	authNotice, authSeverity := grokAuthNotice(base, now)
	// Same classifier the ACP launch pre-flight uses, so the computer
	// snapshot and the spawn check cannot disagree on missing/expired.
	assessment := assessGrokAuth(base, now, false, "")
	if assessment.Authenticated {
		usage.Authenticated = authBoolPtr(true)
		usage.AuthState = grokAuthStateAuthenticated
		if assessment.Refreshable {
			usage.LoginExpirationState = loginExpirationRefreshable
			// Same as Codex: surface the access-token expiry even though the
			// token renews, so the row says something. NOT a logout date, and
			// authState stays authenticated past it — the refresh happens on
			// next use, and Grok's real sign-out shows up through the auth
			// notice path (an interactive `grok login`), not this timestamp.
			if expiry, ok := readGrokAccessTokenExpiry(base); ok {
				usage.LoginExpiresAt = expiry.UTC().Format(time.RFC3339)
			}
		} else if expiry, ok := readGrokAuthExpiry(base); ok {
			usage.LoginExpirationState = loginExpirationKnown
			usage.LoginExpiresAt = expiry.UTC().Format(time.RFC3339)
		} else {
			usage.LoginExpirationState = loginExpirationNotReported
		}
	} else if assessment.AuthState == grokAuthStateExpired {
		usage.Authenticated = authBoolPtr(false)
		usage.AuthState = grokAuthStateExpired
		if expiry, ok := readGrokAuthExpiry(base); ok {
			usage.LoginExpirationState = loginExpirationKnown
			usage.LoginExpiresAt = expiry.UTC().Format(time.RFC3339)
		}
	} else if assessment.ReasonCode != "" && assessment.AuthState != grokAuthStateUnknown {
		usage.Authenticated = authBoolPtr(false)
		usage.AuthState = grokAuthStateMissing
	}
	limitState, hasLimit := loadGrokUsageLimitState(usage.AccountFingerprint, now)
	applyLimitNotice := func() {
		usage.Notice = grokNoticeText(limitState)
		usage.NoticeURL = limitState.UpgradeURL
		if limitState.Severity == grokLimitReached {
			usage.NoticeSeverity = "error"
		} else {
			usage.NoticeSeverity = "warning"
		}
	}
	switch {
	case authNotice != "" && authSeverity == "error":
		// Expired/missing login — surface ahead of any usage-limit banner.
		usage.Notice = authNotice
		usage.NoticeSeverity = authSeverity
	case hasLimit && limitState.Severity == grokLimitReached:
		// A reached quota blocks requests now, whereas an expiring-soon auth
		// warning does not — don't let the re-login heads-up hide it.
		applyLimitNotice()
	case authNotice != "":
		// Expiring-soon auth warning: nudge a re-login before the token lapses.
		usage.Notice = authNotice
		usage.NoticeSeverity = authSeverity
	case hasLimit:
		// Only an approaching (warning) usage limit is left.
		applyLimitNotice()
	}

	return usage, true
}

// grokNoticeText renders the card banner copy for a captured limit state,
// preferring Grok's own gate message when present.
func grokNoticeText(state grokUsageLimitState) string {
	if state.Message != "" {
		return state.Message
	}
	if state.Severity == grokLimitReached {
		return "Grok usage limit reached — new requests may be blocked until your quota resets."
	}
	return "Approaching your Grok usage limit."
}

// grokAuthExpiryWarnWindow is how far ahead of expiry we surface a heads-up.
// Grok access tokens are short-lived relative to the other agents' credentials,
// and once expired `grok agent stdio` falls back to an interactive device-code
// sign-in it cannot complete over its headless stdio pipe — so we warn before
// the ACP `authenticate` step would start failing mid-session.
const grokAuthExpiryWarnWindow = 24 * time.Hour

// grokTokenExpClaims pulls the standard `exp` (seconds since epoch) out of a
// JWT when the on-disk credential carries no explicit `expires_at`.
type grokTokenExpClaims struct {
	Exp int64 `json:"exp"`
}

// readGrokAuthExpiry returns the access-token expiry Grok wrote under
// $GROK_HOME/auth.json. The current CLI keys the file by
// "<oidc_issuer>::<client_id>" with an RFC3339 `expires_at` per entry; older
// layouts store a flat top-level `expires_at` or only a JWT whose `exp` claim we
// fall back to. For the scoped format we tie the expiry to the SAME entry Grok's
// resolver selects — the OIDC scope first, then the legacy sign-in scope (see
// grokScopeKeysByPrecedence) — rather than aggregating siblings: Grok presents
// that one scoped token, so a fresher sibling must not mask it when it is stale
// (and a re-login rewrites the selected entry, so keying off it still clears a
// resolved warning). Best-effort: (zero, false) when nothing is readable.
//
// A credential carrying a refresh token reports NOTHING here: its access-token
// deadline is not a login deadline, and every caller of this function treats
// what it returns as one — warning the user, and flipping authState to
// "expired". Use readGrokAccessTokenExpiry for the raw timestamp.
func readGrokAuthExpiry(base string) (time.Time, bool) {
	return grokAuthExpiry(base, false)
}

// readGrokAccessTokenExpiry returns the selected scope's access-token expiry
// EVEN WHEN a refresh token is present — the case readGrokAuthExpiry refuses.
//
// Display only. The value is a rolling deadline the CLI renews without an
// interactive sign-in (Grok's is ~6 hours), so it must never drive a warning,
// an authState of "expired", or anything else that tells the user to sign in.
// It exists so the card can show when the live credential was last renewed
// against instead of leaving the row permanently blank.
func readGrokAccessTokenExpiry(base string) (time.Time, bool) {
	return grokAuthExpiry(base, true)
}

// grokAuthExpiry is the shared reader. `includeRefreshable` selects between the
// two contracts above: false = authoritative login deadline (absent whenever
// the credential can renew itself), true = the raw access-token expiry.
func grokAuthExpiry(base string, includeRefreshable bool) (time.Time, bool) {
	raw, err := os.ReadFile(filepath.Join(base, "auth.json"))
	if err != nil {
		raw, err = os.ReadFile(filepath.Join(base, "cached_token.json"))
		if err != nil {
			return time.Time{}, false
		}
	}

	fromRFC3339 := func(s string) (time.Time, bool) {
		if s == "" {
			return time.Time{}, false
		}
		t, perr := time.Parse(time.RFC3339, s)
		return t, perr == nil
	}
	fromJWT := func(token string) (time.Time, bool) {
		if token == "" {
			return time.Time{}, false
		}
		var claims grokTokenExpClaims
		if parseJWTClaims(token, &claims) && claims.Exp > 0 {
			return time.Unix(claims.Exp, 0).UTC(), true
		}
		return time.Time{}, false
	}

	// Scoped/keyed format (current): a top-level map of issuer::client → entry.
	// A flat auth.json fails this unmarshal (its string values don't fit the
	// struct), so we fall through to the flat shape below. Walk keys in Grok's
	// own resolution order (OIDC scope, then legacy) and return the expiry of the
	// FIRST entry that carries one — the scope Grok will actually present —
	// instead of the max across unrelated siblings.
	var scoped map[string]struct {
		ExpiresAt    string `json:"expires_at"`
		Key          string `json:"key"`
		Token        string `json:"token"`
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if json.Unmarshal(raw, &scoped) == nil && len(scoped) > 0 {
		keys := make([]string, 0, len(scoped))
		for k := range scoped {
			keys = append(keys, k)
		}
		for _, k := range grokScopeKeysByPrecedence(keys) {
			v := scoped[k]
			// A refresh token is renewal metadata, not the credential Grok
			// presents. Skip refresh-only preferred scopes just as Grok's
			// resolver does, allowing it to fall back to a token-bearing scope.
			token := firstNonEmpty(v.Key, v.Token, v.AccessToken, v.IDToken)
			if token == "" {
				continue
			}
			// Grok v1.0 stores a short-lived access JWT plus an opaque refresh
			// token. The access expiry is not the login expiry: Grok can refresh it
			// without another interactive sign-in. Treat the deadline as unknown
			// while refresh auth is present instead of warning every six hours —
			// unless the caller explicitly asked for the raw access-token expiry
			// to display, which never drives a warning.
			if v.RefreshToken != "" && !includeRefreshable {
				return time.Time{}, false
			}
			// Mirror read_grok_token (and readGrokScopedAuthClaims): a scope is only
			// usable when it carries a non-empty token. Skip metadata/empty-key
			// entries so a tokenless preferred (OIDC) scope can't surface its
			// `expires_at` and mask the health of the legacy scope Grok will
			// actually present. Prefer the access token over the id_token — that's
			// the credential Grok presents on each request — so a stale
			// `access_token` paired with a later-expiring `id_token` can't read as
			// healthy (this also covers a nested `cached_token` object, which
			// unmarshals into this scoped map as a single entry).
			// This is the scope Grok's resolver stops at — the token it will
			// present. Its expiry is the only one that matters, so read it and
			// STOP: don't fall through to lower-precedence siblings whose token
			// Grok will never use. If this scope's expiry is unreadable (opaque
			// key, no `expires_at` and no JWT `exp`), report unknown rather than
			// borrowing an unrelated sibling's — which could surface a stale
			// legacy token's expiry as a false expired-login error.
			if t, ok := fromRFC3339(v.ExpiresAt); ok {
				return t, true
			}
			if t, ok := fromJWT(token); ok {
				return t, true
			}
			return time.Time{}, false
		}
	}

	// Flat / legacy format: top-level `expires_at` and/or a cached_token JWT —
	// a single account, so preferring `expires_at` then the JWT is unambiguous.
	var flat struct {
		ExpiresAt    string `json:"expires_at"`
		Key          string `json:"key"`
		Token        string `json:"token"`
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		CachedToken  struct {
			IDToken      string `json:"id_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"cached_token"`
	}
	if json.Unmarshal(raw, &flat) == nil {
		// A refresh token makes the access-token deadline unsuitable as a login
		// deadline in legacy layouts too: Grok can renew it headlessly.
		if firstNonEmpty(flat.RefreshToken, flat.CachedToken.RefreshToken) != "" &&
			!includeRefreshable {
			return time.Time{}, false
		}
		if t, ok := fromRFC3339(flat.ExpiresAt); ok {
			return t, true
		}
		// Prefer the access token — that's the credential Grok actually presents
		// on each request — and only fall back to the id_token when no access
		// token is present. Otherwise a stale `access_token` paired with a
		// later-expiring `id_token` would report the login as healthy and hide
		// the impending stall.
		if t, ok := fromJWT(firstNonEmpty(
			flat.AccessToken, flat.Token, flat.Key,
			flat.CachedToken.AccessToken,
			flat.IDToken, flat.CachedToken.IDToken,
		)); ok {
			return t, true
		}
	}

	return time.Time{}, false
}

// grokHasUsableToken reports whether Grok's auth file carries a non-empty
// credential Grok's resolver would present — regardless of whether that token's
// expiry or identity is parseable. grokAuthNotice uses it to avoid a false "not
// signed in" error for a usable-but-opaque token (unreadable expiry AND
// unreadable account): Grok would still present it, so surfacing a login error
// contradicts the best-effort intent — warn on a DEFINITE stale login, stay
// quiet when the layout is merely unparseable.
func grokHasUsableToken(base string) bool {
	raw, err := os.ReadFile(filepath.Join(base, "auth.json"))
	if err != nil {
		raw, err = os.ReadFile(filepath.Join(base, "cached_token.json"))
		if err != nil {
			return false
		}
	}

	// Scoped/keyed format (current): inspect only the entry Grok's resolver
	// would select. Unrelated sibling credentials are not usable by this CLI.
	var scoped map[string]struct {
		Key          string `json:"key"`
		Token        string `json:"token"`
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if json.Unmarshal(raw, &scoped) == nil && len(scoped) > 0 {
		keys := make([]string, 0, len(scoped))
		for key := range scoped {
			keys = append(keys, key)
		}
		for _, key := range grokScopeKeysByPrecedence(keys) {
			v := scoped[key]
			if firstNonEmpty(v.Key, v.Token, v.AccessToken, v.IDToken) != "" {
				return true
			}
		}
	}

	// Flat / legacy cached_token format.
	var flat struct {
		Key          string `json:"key"`
		Token        string `json:"token"`
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		CachedToken  struct {
			IDToken      string `json:"id_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"cached_token"`
	}
	if json.Unmarshal(raw, &flat) == nil {
		if firstNonEmpty(
			flat.Key, flat.Token, flat.AccessToken, flat.IDToken,
			flat.CachedToken.AccessToken, flat.CachedToken.IDToken,
		) != "" {
			return true
		}
	}

	return false
}

// grokHasRefreshToken follows the same selected-scope precedence as Grok's
// token resolver. A refresh token makes access-token expires_at unsuitable as
// a login deadline.
func grokHasRefreshToken(base string) bool {
	raw, err := os.ReadFile(filepath.Join(base, "auth.json"))
	if err != nil {
		raw, err = os.ReadFile(filepath.Join(base, "cached_token.json"))
		if err != nil {
			return false
		}
	}
	var scoped map[string]struct {
		Key          string `json:"key"`
		Token        string `json:"token"`
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if json.Unmarshal(raw, &scoped) == nil && len(scoped) > 0 {
		keys := make([]string, 0, len(scoped))
		for key := range scoped {
			keys = append(keys, key)
		}
		for _, key := range grokScopeKeysByPrecedence(keys) {
			entry := scoped[key]
			if firstNonEmpty(entry.Key, entry.Token, entry.AccessToken, entry.IDToken) != "" {
				return entry.RefreshToken != ""
			}
		}
	}
	var flat struct {
		RefreshToken string `json:"refresh_token"`
		CachedToken  struct {
			RefreshToken string `json:"refresh_token"`
		} `json:"cached_token"`
	}
	return json.Unmarshal(raw, &flat) == nil && firstNonEmpty(flat.RefreshToken, flat.CachedToken.RefreshToken) != ""
}

// grokAuthNotice returns a card-level notice + severity when the local Grok
// login is missing or (nearly) expired, so the dashboard can prompt a re-run of
// `grok login` on the terminal computer BEFORE a chat session stalls on an
// un-showable browser sign-in. Returns ("", "") when auth looks healthy.
func grokAuthNotice(base string, now time.Time) (string, string) {
	expiry, ok := readGrokAuthExpiry(base)
	if !ok {
		// No parseable expiry. Authentication depends on a credential Grok can
		// present, not stale identity metadata left in the file. An opaque but
		// usable token stays quiet; identity without a presented token is missing.
		if !grokHasUsableToken(base) {
			return "Grok is not signed in on this computer — run `grok login` on the terminal computer to authenticate.", "error"
		}
		return "", ""
	}
	if !expiry.After(now) {
		return "Grok login has expired — run `grok login` on the terminal computer to re-authenticate.", "error"
	}
	if expiry.Before(now.Add(grokAuthExpiryWarnWindow)) {
		// Surface the exact expiry instant (in the terminal computer's local time,
		// where `grok login` must be re-run) plus a coarse time-remaining hint, then
		// the re-login instruction on its own line. The frontend Alert renders the
		// notice with `white-space: pre-line`, so the `\n` becomes a real line break.
		when := expiry.Local().Format("1/2/2006, 3:04 PM")
		return fmt.Sprintf(
			"Grok login expires %s (%s)\nRun `grok login` on the terminal computer to avoid an interrupted session.",
			when, humanizeGrokRemaining(expiry.Sub(now)),
		), "warning"
	}
	return "", ""
}

// humanizeGrokRemaining renders a coarse "time left" hint for the expiry banner
// — minutes under an hour, whole hours up to two days, whole days beyond — so
// the notice reads e.g. "(45 min)", "(2 hrs)", "(3 days)". Always at least
// "1 min" so a token expiring within the current minute never reads "(0 min)".
func humanizeGrokRemaining(d time.Duration) string {
	switch {
	case d < time.Hour:
		m := int(math.Round(d.Minutes()))
		if m < 1 {
			m = 1
		}
		return fmt.Sprintf("%d min", m)
	case d < 48*time.Hour:
		h := int(math.Round(d.Hours()))
		if h == 1 {
			return "1 hr"
		}
		return fmt.Sprintf("%d hrs", h)
	default:
		days := int(math.Round(d.Hours() / 24))
		if days == 1 {
			return "1 day"
		}
		return fmt.Sprintf("%d days", days)
	}
}

// readGrokAccountAndPlan extracts the account identifier and plan from the Grok
// cached-token JSON under base. Grok writes one of a few layouts depending on
// CLI version: the official installer's scoped `auth.json`
// (`{scope: {key: <jwt>}}` — what `read_grok_token` in
// https://x.ai/cli/install.sh consumes), a flat `auth.json`, and a sibling
// `cached_token.json` (legacy). Try all three before giving up. Shared by the
// usage parser and the usage-limit capture so both compute the SAME account
// fingerprint (a mismatch would orphan a captured limit from the card).
func readGrokAccountAndPlan(base string) (string, string) {
	authPath := filepath.Join(base, "auth.json")
	auth := grokAuthFile{}
	loaded := readJSONFile(authPath, &auth)
	if !loaded {
		loaded = readJSONFile(filepath.Join(base, "cached_token.json"), &auth)
	}
	account, plan := "", ""
	if loaded {
		claims := grokIDTokenClaims{}
		parseJWTClaims(firstNonEmpty(auth.CachedToken.IDToken, auth.CachedToken.AccessToken), &claims)
		account = firstNonEmpty(
			auth.Email,
			auth.Account,
			auth.UserName,
			auth.UserID,
			auth.CachedToken.Email,
			auth.CachedToken.Account,
			claims.Email,
			claims.Account,
			claims.UserName,
			claims.UserID,
			auth.CachedToken.Subject,
			claims.Subject,
		)
		plan = firstNonEmpty(
			auth.Plan,
			auth.PlanType,
			auth.Tier,
			auth.Subscription,
			claims.Plan,
			claims.PlanType,
		)
	}
	// Scoped fallback: the installer-produced `auth.json` does not match the
	// flat shape above — every top-level key is an auth scope whose value is a
	// `{key: <jwt>}` envelope. When the flat parse left identity fields empty,
	// reparse the file as the scoped map and pull claims from the first JWT.
	if account == "" {
		if scopedClaims, ok := readGrokScopedAuthClaims(authPath); ok {
			account = firstNonEmpty(
				scopedClaims.Email,
				scopedClaims.Account,
				scopedClaims.UserName,
				scopedClaims.UserID,
				scopedClaims.Subject,
			)
			plan = firstNonEmpty(
				plan,
				scopedClaims.Plan,
				scopedClaims.PlanType,
			)
		}
	}
	return account, plan
}

// grokIdentityCandidates returns EVERY account value the current credentials
// resolve to — email, username, user id, subject — from both the flat and the
// scoped auth layouts.
//
// readGrokAccountAndPlan picks just one by precedence, which is right for
// display but wrong for matching the log: the log records `user_id`, while the
// display account may well be an email. Comparing against the whole set lets the
// billing-log check confirm the record belongs to this login regardless of which
// field happened to win.
func grokIdentityCandidates(base string) []string {
	authPath := filepath.Join(base, "auth.json")
	auth := grokAuthFile{}
	if !readJSONFile(authPath, &auth) {
		readJSONFile(filepath.Join(base, "cached_token.json"), &auth)
	}
	claims := grokIDTokenClaims{}
	parseJWTClaims(firstNonEmpty(auth.CachedToken.IDToken, auth.CachedToken.AccessToken), &claims)
	scoped, _ := readGrokScopedAuthClaims(authPath)

	candidates := []string{
		auth.Email, auth.Account, auth.UserName, auth.UserID,
		auth.CachedToken.Email, auth.CachedToken.Account, auth.CachedToken.Subject,
		claims.Email, claims.Account, claims.UserName, claims.UserID, claims.Subject,
		scoped.Email, scoped.Account, scoped.UserName, scoped.UserID, scoped.Subject,
	}
	out := make([]string, 0, len(candidates))
	seen := map[string]bool{}
	for _, candidate := range candidates {
		trimmed := strings.TrimSpace(candidate)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	return out
}

// currentGrokAccountFingerprint reads the Grok auth on disk and returns the
// same fingerprint grokUsageParser.Parse attaches to a usage snapshot. Used by
// the usage-limit capture path to scope its cache to the active account.
// Returns "" when no auth is readable (best-effort, matches the codex analog).
func currentGrokAccountFingerprint() string {
	home, _ := os.UserHomeDir()
	base := firstNonEmpty(os.Getenv("GROK_HOME"), expandHome(home, ".grok"))
	if base == "" {
		return ""
	}
	account, _ := readGrokAccountAndPlan(base)
	return fingerprintAccount("grok", account)
}

// readGrokScopedAuthClaims decodes the installer-produced `auth.json`, whose
// top level is keyed by auth scope (e.g. `grok-cli`) and whose values wrap a
// JWT under `key`. Returns the identity for the first entry carrying a token,
// walked in Grok's own scope-resolution order (grokScopeKeysByPrecedence) so
// identity and expiry come from the SAME entry Grok will present.
//
// The entry stores the signed-in identity as plain sibling fields (`email`,
// `user_id`, …) NEXT TO the token, and Grok's JWT carries no email claim — so
// reading claims alone left the card showing a bare `sub` UUID where every
// other provider shows an address. Both sources are merged, JWT first: a claim
// is signed, a sibling field is not, so the token wins any disagreement and the
// plain fields only fill what it omits.
func readGrokScopedAuthClaims(path string) (grokIDTokenClaims, bool) {
	var claims grokIDTokenClaims
	raw, err := os.ReadFile(path)
	if err != nil {
		return claims, false
	}
	var scoped map[string]struct {
		Key      string `json:"key"`
		Token    string `json:"token"`
		Email    string `json:"email"`
		UserID   string `json:"user_id"`
		UserName string `json:"username"`
		Account  string `json:"account"`
		Plan     string `json:"plan"`
		PlanType string `json:"plan_type"`
	}
	if err := json.Unmarshal(raw, &scoped); err != nil {
		return claims, false
	}
	keys := make([]string, 0, len(scoped))
	for k := range scoped {
		keys = append(keys, k)
	}
	for _, k := range grokScopeKeysByPrecedence(keys) {
		v := scoped[k]
		token := firstNonEmpty(v.Key, v.Token)
		if token == "" {
			continue
		}
		// A fresh value per iteration: a partial decode must not leak fields
		// from an entry we then walk past.
		var entry grokIDTokenClaims
		parsed := parseJWTClaims(token, &entry)
		entry.Email = firstNonEmpty(entry.Email, v.Email)
		entry.UserID = firstNonEmpty(entry.UserID, v.UserID)
		entry.UserName = firstNonEmpty(entry.UserName, v.UserName)
		entry.Account = firstNonEmpty(entry.Account, v.Account)
		entry.Plan = firstNonEmpty(entry.Plan, v.Plan)
		entry.PlanType = firstNonEmpty(entry.PlanType, v.PlanType)
		// An unparseable token is not automatically an unusable entry: the
		// sibling fields still name the account, and reporting that beats
		// falling through to a later scope that describes a different login.
		hasIdentity := firstNonEmpty(
			entry.Email, entry.Account, entry.UserName, entry.UserID, entry.Subject,
		) != ""
		if parsed || hasIdentity {
			return entry, true
		}
	}
	return claims, false
}
