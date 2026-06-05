// cliagent_usage_codex.go — Codex (OpenAI) usage parser.
//
// Reads CODEX_HOME/auth.json (or ~/.codex/auth.json) for the active account
// and any local plan/quota
// hints. Emits Unknown=true for daily-tokens because the remaining counter
// is enforced at the API gateway and is not on disk; the total cap is
// surfaced when the auth file records it.
package main

import (
	"os"
	"time"
)

type codexUsageParser struct{}

func (codexUsageParser) Provider() string { return "codex" }

type codexAuth struct {
	Account    string `json:"account"`
	Email      string `json:"email"`
	UserID     string `json:"user_id"`
	Plan       string `json:"plan"`
	PlanType   string `json:"plan_type"`
	OrgID      string `json:"org_id"`
	TokenLimit *int   `json:"token_limit"`
	Tokens     struct {
		IDToken   string `json:"id_token"`
		AccountID string `json:"account_id"`
	} `json:"tokens"`
}

type codexIDTokenClaims struct {
	Account  string `json:"account"`
	Email    string `json:"email"`
	UserID   string `json:"user_id"`
	Subject  string `json:"sub"`
	Plan     string `json:"plan"`
	PlanType string `json:"plan_type"`
	OrgID    string `json:"org_id"`
}

func (p codexUsageParser) Parse(home string, detected detectedCLIAgent, now time.Time) (*cliAgentUsage, bool) {
	base := firstNonEmpty(os.Getenv("CODEX_HOME"), expandHome(home, ".codex"))
	if base == "" {
		return nil, false
	}

	usage := &cliAgentUsage{
		Provider:    p.Provider(),
		Name:        firstNonEmpty(detected.Name, "Codex"),
		Version:     detected.Version,
		Path:        detected.Path,
		DataSource:  "~/.codex",
		CollectedAt: now.UTC().Format(time.RFC3339),
	}

	auth := codexAuth{}
	if readJSONFile(expandHome(base, "auth.json"), &auth) {
		claims := codexIDTokenClaims{}
		parseJWTClaims(auth.Tokens.IDToken, &claims)
		usage.Account = firstNonEmpty(
			auth.Email,
			auth.Account,
			auth.UserID,
			claims.Email,
			claims.Account,
			claims.UserID,
			auth.Tokens.AccountID,
			claims.Subject,
		)
		usage.Plan = firstNonEmpty(auth.Plan, auth.PlanType, claims.Plan, claims.PlanType)
	}
	usage.AccountFingerprint = fingerprintAccount(p.Provider(), usage.Account)

	dailyTokens := cliAgentUsageMetric{
		Kind:    limitKindTokens,
		Label:   "Daily tokens",
		Unit:    "tokens",
		Unknown: true,
	}
	if auth.TokenLimit != nil && *auth.TokenLimit > 0 {
		dailyTokens.Total = floatPtr(float64(*auth.TokenLimit))
	}
	usage.Metrics = []cliAgentUsageMetric{
		dailyTokens,
		{
			Kind:    limitKindRequests,
			Label:   "Daily requests",
			Unit:    "requests",
			Unknown: true,
		},
	}
	return usage, true
}
