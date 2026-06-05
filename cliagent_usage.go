// cliagent_usage.go — discovers per-provider utilization for CLI agents
// installed on this device.
//
// The CLI agents in scope (Claude Code, Codex, Gemini CLI, Antigravity) do not
// today expose a public utilization API the way provider account-usage pages
// do. What they DO expose, on the local filesystem, are credential files we
// can fingerprint (so multi-device dedup can aggregate the same account), and
// — for some providers — local session/state caches that let us infer
// account/plan or rough activity.
//
// The parsers intentionally distinguish three states per metric:
//
//   - capacityRemaining: an absolute number we are sure of (e.g. tokens left)
//   - capacityConsumed:  the matching consumed value
//   - kind = "unknown":  the provider exposes the limit window but not the
//     current value, OR we can't currently parse it. The UI renders these as
//     a dashed capacity bar so operators know the metric exists but is
//     unobservable, vs. the metric being entirely absent.
//
// The shape (`cliAgentUsage`) is what terminal-service writes to Firestore as
// `cliAgents[]` — the frontend hook keys off `(provider, accountFingerprint)`
// to dedup across devices.
package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// cliAgentLimitKind mirrors shared-constants/CLI_AGENT_LIMIT_KIND. Keep the
// string values in lockstep — the frontend renders metrics by switching on
// these.
const (
	limitKindSession  = "session"
	limitKindDaily    = "daily"
	limitKindWeekly   = "weekly"
	limitKindMonthly  = "monthly"
	limitKindModel    = "model"
	limitKindRequests = "requests"
	limitKindTokens   = "tokens"
)

// cliAgentUsageMetric is one row of the capacity table the UI renders.
// `Unknown=true` means the limit exists but the current value is unobservable;
// the UI renders a dashed gauge. Total/Remaining/Consumed are intentionally
// pointers so JSON omits them when unobservable (vs. emitting 0 — "we know
// it is exactly zero remaining" reads as a hard cap hit, which would mislead).
type cliAgentUsageMetric struct {
	Kind      string   `json:"kind"`
	Label     string   `json:"label,omitempty"`
	Unit      string   `json:"unit,omitempty"`
	Total     *float64 `json:"total,omitempty"`
	Remaining *float64 `json:"remaining,omitempty"`
	Consumed  *float64 `json:"consumed,omitempty"`
	ResetAt   string   `json:"resetAt,omitempty"`
	Model     string   `json:"model,omitempty"`
	Unknown   bool     `json:"unknown,omitempty"`
}

// cliAgentUsage is the per-provider snapshot embedded in `cliAgents[]`. One
// entry per detected agent on this device — multi-device aggregation is the
// frontend hook's job, not ours.
type cliAgentUsage struct {
	Provider           string                `json:"provider"`
	Name               string                `json:"name,omitempty"`
	Version            string                `json:"version,omitempty"`
	Path               string                `json:"path,omitempty"`
	Account            string                `json:"account,omitempty"`
	Plan               string                `json:"plan,omitempty"`
	AccountFingerprint string                `json:"accountFingerprint,omitempty"`
	Metrics            []cliAgentUsageMetric `json:"metrics,omitempty"`
	CollectedAt        string                `json:"collectedAt"`
	DataSource         string                `json:"dataSource,omitempty"`
}

// cliAgentUsageParser is the small interface every provider parser
// implements. Parsers MUST be best-effort: a missing config dir, unreadable
// JSON, or absent CLI binary all return (nil, false) without an error — the
// "no usage info available" state is normal, not a failure.
type cliAgentUsageParser interface {
	Provider() string
	Parse(home string, detected detectedCLIAgent, now time.Time) (*cliAgentUsage, bool)
}

// gatherCLIAgentUsage walks the registered parsers and returns the slice the
// auth/token request embeds as `cliAgents[]`. Order is stable (alphabetical
// by provider) so byte-equal payloads produce byte-equal Firestore writes,
// which the terminal-service delta-skip relies on.
func gatherCLIAgentUsage(detected map[string]detectedCLIAgent, now time.Time) []cliAgentUsage {
	if len(detected) == 0 {
		return []cliAgentUsage{}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}

	out := make([]cliAgentUsage, 0, len(detected))
	for _, parser := range cliAgentUsageRegistry() {
		entry, ok := detected[parser.Provider()]
		if !ok || !entry.Detected {
			continue
		}
		usage, ok := parser.Parse(home, entry, now)
		if !ok || usage == nil {
			// Even when we can't enrich, still emit a baseline entry so the UI
			// shows the agent on the CLI Agents tab with "metrics unknown"
			// rather than dropping it entirely.
			usage = &cliAgentUsage{
				Provider:    parser.Provider(),
				Name:        entry.Name,
				Version:     entry.Version,
				Path:        entry.Path,
				CollectedAt: now.UTC().Format(time.RFC3339),
			}
		}
		out = append(out, *usage)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

// readJSONFile is a small helper around os.ReadFile + json.Unmarshal that
// returns (nil, false) on any error. Parsers use it so they never have to
// distinguish "file missing" (expected) from "file unreadable" (also expected
// when permissions are funky) — both downgrade the same way.
func readJSONFile(path string, into any) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return unmarshalJSON(b, into)
}

// expandHome joins a home dir with a relative path, returning "" if either
// piece is empty. Parsers call this so they never construct a usage entry
// rooted at "/.claude" (would read the system root on misconfigured boxes).
func expandHome(home, rel string) string {
	if home == "" || rel == "" {
		return ""
	}
	return filepath.Join(home, rel)
}

// firstNonEmpty returns the first non-empty string from its inputs — used by
// parsers to pick the best available label for the `Account` field.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	return ""
}

// floatPtr is a tiny helper so the parsers can construct *float64 inline. The
// metrics shape uses pointers to distinguish "we observed 0" from "we have no
// observation" — see cliAgentUsageMetric.
func floatPtr(v float64) *float64 { return &v }
