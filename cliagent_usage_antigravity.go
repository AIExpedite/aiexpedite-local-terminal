// cliagent_usage_antigravity.go — Antigravity (`agy`) usage parser.
//
// Antigravity stores per-user config under ~/.agy/config.json. The provider
// does not currently expose request- or token-level quotas, only the active
// account/plan. Capacity rows are flagged Unknown because there is no
// observable counter to plot.
package main

import "time"

type antigravityUsageParser struct{}

func (antigravityUsageParser) Provider() string { return "antigravity" }

type antigravityConfig struct {
	Account string `json:"account"`
	Email   string `json:"email"`
	Plan    string `json:"plan"`
	Tier    string `json:"tier"`
}

func (p antigravityUsageParser) Parse(home string, detected detectedCLIAgent, now time.Time) (*cliAgentUsage, bool) {
	base := expandHome(home, ".agy")
	if base == "" {
		return nil, false
	}

	usage := &cliAgentUsage{
		Provider:    p.Provider(),
		Name:        firstNonEmpty(detected.Name, "Antigravity"),
		Version:     detected.Version,
		Path:        detected.Path,
		DataSource:  "~/.agy",
		CollectedAt: now.UTC().Format(time.RFC3339),
	}

	cfg := antigravityConfig{}
	if readJSONFile(expandHome(base, "config.json"), &cfg) {
		usage.Account = firstNonEmpty(cfg.Email, cfg.Account)
		usage.Plan = firstNonEmpty(cfg.Plan, cfg.Tier)
	}
	usage.AccountFingerprint = fingerprintAccount(p.Provider(), usage.Account)

	usage.Metrics = []cliAgentUsageMetric{
		{
			Kind:    limitKindMonthly,
			Label:   "Monthly quota",
			Unit:    "requests",
			Unknown: true,
		},
	}
	return usage, true
}
