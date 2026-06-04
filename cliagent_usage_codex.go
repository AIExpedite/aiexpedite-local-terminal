// cliagent_usage_codex.go — Codex (OpenAI) usage parser.
//
// Reads ~/.codex/auth.json for the active account and any local plan/quota
// hints. Emits Unknown=true for daily-tokens because the remaining counter
// is enforced at the API gateway and is not on disk; the total cap is
// surfaced when the auth file records it.
package main

import "time"

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
}

func (p codexUsageParser) Parse(home string, detected detectedCLIAgent, now time.Time) (*cliAgentUsage, bool) {
	base := expandHome(home, ".codex")
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
		usage.Account = firstNonEmpty(auth.Email, auth.Account, auth.UserID)
		usage.Plan = firstNonEmpty(auth.Plan, auth.PlanType)
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
