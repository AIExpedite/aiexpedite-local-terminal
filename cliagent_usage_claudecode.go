// cliagent_usage_claudecode.go — Claude Code usage parser.
//
// Claude Code does not currently expose a public utilization API. What we
// CAN observe locally is ~/.claude/.credentials.json (account + plan) for
// fingerprinting and plan display. The real 5h-window counters are not in
// any local file, so the session/daily metrics are flagged Unknown — the UI
// renders a dashed gauge so operators know the metric exists but is
// unobservable.
package main

import "time"

type claudeCodeUsageParser struct{}

func (claudeCodeUsageParser) Provider() string { return "claudeCode" }

type claudeCredentials struct {
	Account      string `json:"account"`
	Email        string `json:"email"`
	Organization string `json:"organization"`
	Plan         string `json:"plan"`
	Subscription string `json:"subscription"`
}

func (p claudeCodeUsageParser) Parse(home string, detected detectedCLIAgent, now time.Time) (*cliAgentUsage, bool) {
	base := expandHome(home, ".claude")
	if base == "" {
		return nil, false
	}

	usage := &cliAgentUsage{
		Provider:    p.Provider(),
		Name:        firstNonEmpty(detected.Name, "Claude Code"),
		Version:     detected.Version,
		Path:        detected.Path,
		DataSource:  "~/.claude",
		CollectedAt: now.UTC().Format(time.RFC3339),
	}

	creds := claudeCredentials{}
	if readJSONFile(expandHome(base, ".credentials.json"), &creds) {
		usage.Account = firstNonEmpty(creds.Email, creds.Account, creds.Organization)
		usage.Plan = firstNonEmpty(creds.Plan, creds.Subscription)
	}
	usage.AccountFingerprint = fingerprintAccount(p.Provider(), usage.Account)

	usage.Metrics = []cliAgentUsageMetric{
		{
			Kind:    limitKindSession,
			Label:   "5-hour session window",
			Unit:    "messages",
			Unknown: true,
		},
		{
			Kind:    limitKindDaily,
			Label:   "Daily quota",
			Unit:    "messages",
			Unknown: true,
		},
	}
	return usage, true
}
