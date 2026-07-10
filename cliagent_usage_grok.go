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
	"os"
	"path/filepath"
	"sort"
	"time"
)

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

	usage.Metrics = []cliAgentUsageMetric{
		{
			Kind:    limitKindRequests,
			Label:   "Daily requests",
			Unit:    "requests",
			Unknown: true,
		},
		{
			Kind:    limitKindTokens,
			Label:   "Daily tokens",
			Unit:    "tokens",
			Unknown: true,
		},
	}

	// Notice priority: an expired/missing local Grok login blocks every request
	// AND can't be re-established headlessly (grok agent stdio falls back to an
	// interactive device-code sign-in the terminal can't show), so surface it
	// FIRST — ahead of the usage-limit banner. Grok's token is short-lived
	// relative to the other agents' credentials, which is exactly why we fold
	// this into the regular usage check instead of only failing at session
	// start. Fall back to the live usage-limit state only when auth is healthy.
	//
	// xAI exposes no numeric quota, so the capacity bars stay Unknown — but Grok
	// DOES push a discrete usage-limit warning on the streaming-json output,
	// which captureGrokUsageLimitLine caches (approaching → warning, reached →
	// error).
	if authNotice, authSeverity := grokAuthNotice(base, usage.Account, now); authNotice != "" {
		usage.Notice = authNotice
		usage.NoticeSeverity = authSeverity
	} else if state, ok := loadGrokUsageLimitState(usage.AccountFingerprint, now); ok {
		usage.Notice = grokNoticeText(state)
		usage.NoticeURL = state.UpgradeURL
		if state.Severity == grokLimitReached {
			usage.NoticeSeverity = "error"
		} else {
			usage.NoticeSeverity = "warning"
		}
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
// fall back to. For the scoped format we tie the expiry to the SAME entry
// readGrokAccountAndPlan/readGrokScopedAuthClaims select — the first key in
// sorted order — rather than aggregating siblings: Grok presents that one scoped
// token, so a fresher sibling must not mask it when it is stale (and a re-login
// rewrites the selected entry, so keying off it still clears a resolved warning).
// Best-effort: (zero, false) when nothing is readable.
func readGrokAuthExpiry(base string) (time.Time, bool) {
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
	// struct), so we fall through to the flat shape below. Walk keys in the same
	// sorted order readGrokScopedAuthClaims uses to pick the account, and return
	// the expiry of the FIRST entry that carries one — the scope Grok will
	// actually present — instead of the max across unrelated siblings.
	var scoped map[string]struct {
		ExpiresAt   string `json:"expires_at"`
		Key         string `json:"key"`
		Token       string `json:"token"`
		IDToken     string `json:"id_token"`
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(raw, &scoped) == nil && len(scoped) > 0 {
		keys := make([]string, 0, len(scoped))
		for k := range scoped {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := scoped[k]
			if t, ok := fromRFC3339(v.ExpiresAt); ok {
				return t, true
			}
			if t, ok := fromJWT(firstNonEmpty(v.Key, v.Token, v.IDToken, v.AccessToken)); ok {
				return t, true
			}
		}
	}

	// Flat / legacy format: top-level `expires_at` and/or a cached_token JWT —
	// a single account, so preferring `expires_at` then the JWT is unambiguous.
	var flat struct {
		ExpiresAt   string `json:"expires_at"`
		CachedToken struct {
			IDToken     string `json:"id_token"`
			AccessToken string `json:"access_token"`
		} `json:"cached_token"`
	}
	if json.Unmarshal(raw, &flat) == nil {
		if t, ok := fromRFC3339(flat.ExpiresAt); ok {
			return t, true
		}
		if t, ok := fromJWT(firstNonEmpty(flat.CachedToken.IDToken, flat.CachedToken.AccessToken)); ok {
			return t, true
		}
	}

	return time.Time{}, false
}

// grokAuthNotice returns a card-level notice + severity when the local Grok
// login is missing or (nearly) expired, so the dashboard can prompt a re-run of
// `grok login` on the terminal computer BEFORE a chat session stalls on an
// un-showable browser sign-in. Returns ("", "") when auth looks healthy.
func grokAuthNotice(base, account string, now time.Time) (string, string) {
	expiry, ok := readGrokAuthExpiry(base)
	if !ok {
		// No parseable token. Only call it "not signed in" when there's also no
		// identity — a readable account with an unusual token layout shouldn't
		// raise a false alarm.
		if account == "" {
			return "Grok is not signed in on this computer — run `grok login` on the terminal computer to authenticate.", "error"
		}
		return "", ""
	}
	if !expiry.After(now) {
		return "Grok login has expired — run `grok login` on the terminal computer to re-authenticate.", "error"
	}
	if expiry.Before(now.Add(grokAuthExpiryWarnWindow)) {
		return "Grok login expires soon — run `grok login` on the terminal computer to avoid an interrupted session.", "warning"
	}
	return "", ""
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
// JWT under `key`. Returns the JWT claims from the first non-empty token.
func readGrokScopedAuthClaims(path string) (grokIDTokenClaims, bool) {
	var claims grokIDTokenClaims
	raw, err := os.ReadFile(path)
	if err != nil {
		return claims, false
	}
	var scoped map[string]struct {
		Key   string `json:"key"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &scoped); err != nil {
		return claims, false
	}
	keys := make([]string, 0, len(scoped))
	for k := range scoped {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := scoped[k]
		token := firstNonEmpty(v.Key, v.Token)
		if token == "" {
			continue
		}
		if parseJWTClaims(token, &claims) {
			return claims, true
		}
	}
	return claims, false
}
