// cliagent_usage_gemini.go — Gemini CLI usage parser.
//
// Reads ~/.gemini/oauth_creds.json for account identity and
// ~/.gemini/settings.json for explicit tier metadata. On the explicit free
// tier we know the public 1,000 requests/day cap, so we surface it as the
// metric's total even though the remaining count isn't on disk. Unknown tiers
// stay unframed rather than assuming a free quota.
package main

import (
	"strings"
	"time"
)

type geminiUsageParser struct{}

func (geminiUsageParser) Provider() string { return "geminiCli" }

type geminiSettings struct {
	Account  string `json:"account"`
	Email    string `json:"email"`
	Plan     string `json:"plan"`
	PlanType string `json:"plan_type"`
	Tier     string `json:"tier"`
	Project  string `json:"project"`
}

type geminiCredentials struct {
	Account  string `json:"account"`
	Email    string `json:"email"`
	UserID   string `json:"user_id"`
	Subject  string `json:"sub"`
	IDToken  string `json:"id_token"`
	Plan     string `json:"plan"`
	PlanType string `json:"plan_type"`
	Tier     string `json:"tier"`
	Tokens   struct {
		IDToken   string `json:"id_token"`
		AccountID string `json:"account_id"`
	} `json:"tokens"`
}

type geminiIDTokenClaims struct {
	Account  string `json:"account"`
	Email    string `json:"email"`
	UserID   string `json:"user_id"`
	Subject  string `json:"sub"`
	Plan     string `json:"plan"`
	PlanType string `json:"plan_type"`
	Tier     string `json:"tier"`
}

func (p geminiUsageParser) Parse(home string, detected detectedCLIAgent, now time.Time) (*cliAgentUsage, bool) {
	base := expandHome(home, ".gemini")
	if base == "" {
		return nil, false
	}

	usage := &cliAgentUsage{
		Provider:    p.Provider(),
		Name:        firstNonEmpty(detected.Name, "Gemini CLI"),
		Version:     detected.Version,
		Path:        detected.Path,
		DataSource:  "~/.gemini",
		CollectedAt: now.UTC().Format(time.RFC3339),
	}

	creds := geminiCredentials{}
	claims := geminiIDTokenClaims{}
	if readJSONFile(expandHome(base, "oauth_creds.json"), &creds) {
		parseJWTClaims(firstNonEmpty(creds.IDToken, creds.Tokens.IDToken), &claims)
		usage.Account = firstNonEmpty(
			creds.Email,
			creds.Account,
			creds.UserID,
			creds.Subject,
			claims.Email,
			claims.Account,
			claims.UserID,
			creds.Tokens.AccountID,
			claims.Subject,
		)
		usage.Plan = firstNonEmpty(creds.Tier, creds.Plan, creds.PlanType, claims.Tier, claims.Plan, claims.PlanType)
	}

	cfg := geminiSettings{}
	if readJSONFile(expandHome(base, "settings.json"), &cfg) {
		usage.Account = firstNonEmpty(usage.Account, cfg.Email, cfg.Account)
		usage.Plan = firstNonEmpty(cfg.Tier, cfg.Plan, cfg.PlanType, usage.Plan)
	}
	usage.AccountFingerprint = fingerprintAccount(p.Provider(), usage.Account)

	dailyRequests := cliAgentUsageMetric{
		Kind:    limitKindRequests,
		Label:   "Daily requests",
		Unit:    "requests",
		Unknown: true,
	}
	if strings.EqualFold(strings.TrimSpace(usage.Plan), "free") {
		dailyRequests.Total = floatPtr(1000)
	}
	usage.Metrics = []cliAgentUsageMetric{dailyRequests}
	return usage, true
}
