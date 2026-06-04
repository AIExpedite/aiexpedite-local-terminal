// cliagent_usage_gemini.go — Gemini CLI usage parser.
//
// Reads ~/.gemini/settings.json. On the free tier we know the public 1,000
// requests/day cap, so we surface it as the metric's total even though the
// remaining count isn't on disk — operators see the gauge framed at the cap
// with Unknown=true (dashed bar) instead of nothing.
package main

import "time"

type geminiUsageParser struct{}

func (geminiUsageParser) Provider() string { return "geminiCli" }

type geminiSettings struct {
	Account string `json:"account"`
	Email   string `json:"email"`
	Tier    string `json:"tier"`
	Project string `json:"project"`
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

	cfg := geminiSettings{}
	if readJSONFile(expandHome(base, "settings.json"), &cfg) {
		usage.Account = firstNonEmpty(cfg.Email, cfg.Account)
		usage.Plan = cfg.Tier
	}
	usage.AccountFingerprint = fingerprintAccount(p.Provider(), usage.Account)

	dailyRequests := cliAgentUsageMetric{
		Kind:    limitKindRequests,
		Label:   "Daily requests",
		Unit:    "requests",
		Unknown: true,
	}
	if cfg.Tier == "" || cfg.Tier == "free" {
		dailyRequests.Total = floatPtr(1000)
	}
	usage.Metrics = []cliAgentUsageMetric{dailyRequests}
	return usage, true
}
