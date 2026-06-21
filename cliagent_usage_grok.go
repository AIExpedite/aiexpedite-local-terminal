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

	// xAI exposes no numeric quota, so the bars stay Unknown — but Grok DOES
	// push a discrete usage-limit warning on the streaming-json output, which
	// captureGrokUsageLimitLine caches. Surface the latest live state as a
	// card-level notice (approaching → warning banner, reached → error banner).
	if state, ok := loadGrokUsageLimitState(usage.AccountFingerprint, now); ok {
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
