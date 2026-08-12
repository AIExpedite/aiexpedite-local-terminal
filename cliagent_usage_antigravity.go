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

	legacyBase := expandHome(home, ".agy")
	// Quota discovery reads the CLI's logs, which live under whichever install
	// is actually in use. Probe the configured one first and the other second:
	// selecting only the config-bearing tree would miss a running server whose
	// install has no config file yet, and probing only the modern tree would
	// never find a legacy install's `~/.agy/log`.
	quotaBases := []string{base, legacyBase}

	cfg := antigravityConfig{}
	if readJSONFile(expandHome(base, "settings.json"), &cfg) {
		usage.Account = firstNonEmpty(cfg.Email, cfg.Account)
		usage.Plan = firstNonEmpty(cfg.Plan, cfg.Tier)
	} else if readJSONFile(expandHome(legacyBase, "config.json"), &cfg) {
		usage.DataSource = "~/.agy"
		usage.Account = firstNonEmpty(cfg.Email, cfg.Account)
		usage.Plan = firstNonEmpty(cfg.Plan, cfg.Tier)
		quotaBases = []string{legacyBase, base}
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
	var fresh antigravityQuotaSnapshot
	var gotFresh bool
	for _, quotaBase := range quotaBases {
		if quotaBase == "" {
			continue
		}
		if fresh, gotFresh = fetchAntigravityQuota(ctx, quotaBase, now); gotFresh {
			break
		}
	}
	// A live reading may ONLY be published under an identity the server itself
	// reported. settings.json can hold an account from a previous login, so
	// falling back to it here would publish the current server's quota under the
	// old account — the fingerprint that dedups this snapshot across devices —
	// and a transient GetUserStatus failure is enough to trigger it. When the
	// server answers quota but not identity, the entry goes out unattributed:
	// gatherCLIAgentUsage then scopes it to this device, which is exactly what
	// "we don't know whose quota this is" should look like.
	if gotFresh {
		usage.Account = fresh.Account
		usage.Plan = fresh.Plan
	}
	usage.AccountFingerprint = fingerprintAccount(p.Provider(), usage.Account)

	snap := fresh
	if gotFresh {
		snap.AccountFingerprint = usage.AccountFingerprint
		// An unattributable reading is still worth DISPLAYING — it came from the
		// server about to run the work — but caching it would let an unscoped
		// snapshot be replayed under a later account.
		if usage.AccountFingerprint != "" {
			saveAntigravityQuotaSnapshot(snap)
		}
	} else if cached, ok := loadAntigravityQuotaSnapshot(usage.AccountFingerprint); ok {
		// No `agy` running right now. Replay the last reading with its ORIGINAL
		// observation time so the card ages it honestly instead of presenting a
		// day-old pool as current.
		snap = cached
	} else if usage.AccountFingerprint == "" {
		// settings.json names nobody — the usual case, since the account lives in
		// the OS keyring. Replay under the identity that PRODUCED the reading
		// rather than dropping it: there is no current identity for it to
		// conflict with, and the card then names the account the quota belongs
		// to instead of implying it is whoever is signed in now.
		if cached, ok := loadAntigravityQuotaSnapshotByProducer(); ok {
			snap = cached
			usage.Account = cached.Account
			usage.Plan = firstNonEmpty(cached.Plan, usage.Plan)
			usage.AccountFingerprint = cached.AccountFingerprint
		}
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
