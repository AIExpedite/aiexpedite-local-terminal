// cliagent_usage_antigravity.go — Antigravity (`agy`) usage parser.
//
// Antigravity CLI stores per-user settings under
// ~/.gemini/antigravity-cli/settings.json. The provider does not currently
// expose request- or token-level quotas, only the active account/plan.
// Capacity rows are flagged Unknown because there is no observable counter to
// plot.
package main

import (
	"path/filepath"
	"time"
)

type antigravityUsageParser struct{}

func (antigravityUsageParser) Provider() string { return "antigravity" }

type antigravityConfig struct {
	Account string `json:"account"`
	Email   string `json:"email"`
	Plan    string `json:"plan"`
	Tier    string `json:"tier"`
}

func (p antigravityUsageParser) Parse(home string, detected detectedCLIAgent, now time.Time) (*cliAgentUsage, bool) {
	base := expandHome(home, filepath.Join(".gemini", "antigravity-cli"))
	if base == "" {
		return nil, false
	}

	usage := &cliAgentUsage{
		Provider:    p.Provider(),
		Name:        firstNonEmpty(detected.Name, "Antigravity"),
		Version:     detected.Version,
		Path:        detected.Path,
		DataSource:  "~/.gemini/antigravity-cli",
		CollectedAt: now.UTC().Format(time.RFC3339),
	}

	cfg := antigravityConfig{}
	if readJSONFile(expandHome(base, "settings.json"), &cfg) {
		usage.Account = firstNonEmpty(cfg.Email, cfg.Account)
		usage.Plan = firstNonEmpty(cfg.Plan, cfg.Tier)
	} else if legacyBase := expandHome(home, ".agy"); readJSONFile(expandHome(legacyBase, "config.json"), &cfg) {
		usage.DataSource = "~/.agy"
		usage.Account = firstNonEmpty(cfg.Email, cfg.Account)
		usage.Plan = firstNonEmpty(cfg.Plan, cfg.Tier)
	}
	usage.AccountFingerprint = fingerprintAccount(p.Provider(), usage.Account)
	// Antigravity keeps its renewable session in the OS keyring and does not
	// publish a durable session deadline. Do not reinterpret an access token or
	// the settings-file mtime as login expiration.
	usage.LoginExpirationState = loginExpirationNotReported

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
