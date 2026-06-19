// cliagent_usage_codex.go — Codex (OpenAI) usage parser.
//
// Reads CODEX_HOME/auth.json (or ~/.codex/auth.json) for the active account
// and any local plan/quota hints. Live utilization comes from the
// `token_count` JSON-RPC notification the Codex app-server emits on stdout
// while a session is active — codex_appserver.go forwards every line to
// captureCodexRateLimitLine, which writes the latest primary (5-hour) /
// secondary (weekly) windows to an on-disk cache keyed by account
// fingerprint. This parser turns the latest snapshot into real percentage
// metrics for the CLI Agents tab, falling back to Unknown placeholders when
// no Codex session has been observed yet under the current account.
package main

import (
	"os"
	"time"
)

type codexUsageParser struct{}

func (codexUsageParser) Provider() string { return "codex" }

type codexAuth struct {
	Account  string `json:"account"`
	Email    string `json:"email"`
	UserID   string `json:"user_id"`
	Plan     string `json:"plan"`
	PlanType string `json:"plan_type"`
	OrgID    string `json:"org_id"`
	Tokens   struct {
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
		DataSource:  "token_count",
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

	usage.Metrics = codexMetricsFromCache(now, usage.AccountFingerprint)
	return usage, true
}
