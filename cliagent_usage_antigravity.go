// cliagent_usage_antigravity.go — Antigravity (`agy`) usage parser.
//
// Antigravity CLI stores per-user settings under
// ~/.gemini/antigravity-cli/settings.json. Quota is NOT on disk: it comes from
// the language server each `agy` run starts on loopback
// (cliagent_usage_antigravity_quota.go), which we read when one is up and cache
// for the gaps in between.
package main

import (
	"context"
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
	// Antigravity keeps its renewable session in the OS keyring and does not
	// publish a durable session deadline. Do not reinterpret an access token or
	// the settings-file mtime as login expiration.
	usage.LoginExpirationState = loginExpirationNotReported

	// A live language server knows the account it is signed in as; settings.json
	// usually does not. Prefer the server's identity so the fingerprint that
	// scopes the cache is the same one the quota was captured under.
	ctx, cancel := context.WithTimeout(context.Background(), antigravityQuotaTimeout)
	defer cancel()
	fresh, gotFresh := fetchAntigravityQuota(ctx, base, now)
	if gotFresh {
		usage.Account = firstNonEmpty(fresh.Account, usage.Account)
		usage.Plan = firstNonEmpty(fresh.Plan, usage.Plan)
	}
	usage.AccountFingerprint = fingerprintAccount(p.Provider(), usage.Account)

	snap := fresh
	if gotFresh {
		// Scope the cache by the identity the SERVER reported, never by the one
		// settings.json happens to carry: if GetUserStatus failed we do not know
		// which account this quota belongs to, and stamping it with a
		// settings-derived (or empty) fingerprint is how one account's pool ends
		// up replayed under another. Displaying it this once is still correct —
		// it came from the server that is about to run the work.
		snap.AccountFingerprint = fingerprintAccount(p.Provider(), fresh.Account)
		if fresh.Account != "" {
			saveAntigravityQuotaSnapshot(snap)
		}
	} else if cached, ok := loadAntigravityQuotaSnapshot(usage.AccountFingerprint); ok {
		// No `agy` running right now. Replay the last reading with its ORIGINAL
		// observation time so the card ages it honestly instead of presenting a
		// day-old pool as current.
		snap = cached
	}

	usage.Metrics = antigravityQuotaMetrics(snap, now)
	if len(usage.Metrics) == 0 {
		// Never observed on this machine (no session since install, or a plan
		// with no metered pool). Keep placeholder rows so the agent still shows
		// up as "limits exist, values unobservable".
		usage.Metrics = []cliAgentUsageMetric{
			{Kind: limitKindSession, Label: "5-hour session window", Unit: "%", Unknown: true},
			{Kind: limitKindWeekly, Label: "Weekly quota", Unit: "%", Unknown: true},
		}
	}
	return usage, true
}
