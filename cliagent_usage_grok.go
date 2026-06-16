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
	"os"
	"path/filepath"
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
		Name:        firstNonEmpty(detected.Name, "Grok"),
		Version:     detected.Version,
		Path:        detected.Path,
		DataSource:  dataSource,
		CollectedAt: now.UTC().Format(time.RFC3339),
	}

	// Grok writes its cached-token JSON in one of a couple of layouts depending
	// on CLI version: a flat `auth.json` (newer) and a sibling
	// `cached_token.json` (legacy). Try both before giving up.
	auth := grokAuthFile{}
	loaded := readJSONFile(filepath.Join(base, "auth.json"), &auth)
	if !loaded {
		loaded = readJSONFile(filepath.Join(base, "cached_token.json"), &auth)
	}
	if loaded {
		claims := grokIDTokenClaims{}
		parseJWTClaims(firstNonEmpty(auth.CachedToken.IDToken, auth.CachedToken.AccessToken), &claims)
		usage.Account = firstNonEmpty(
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
		usage.Plan = firstNonEmpty(
			auth.Plan,
			auth.PlanType,
			auth.Tier,
			auth.Subscription,
			claims.Plan,
			claims.PlanType,
		)
	}
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
	return usage, true
}
