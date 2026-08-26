// cliagent_usage.go — discovers per-provider utilization for CLI agents
// installed on this device.
//
// The CLI agents in scope (Claude Code, Codex, Antigravity) do not
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
	"context"
	"fmt"
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
	// ObservedAt is when the provider emitted this utilization value. It is
	// deliberately distinct from CollectedAt, which is only the cache-read time.
	ObservedAt string `json:"observedAt,omitempty"`
	Model      string `json:"model,omitempty"`
	Unknown    bool   `json:"unknown,omitempty"`
}

// cliAgentUsage is the per-provider snapshot embedded in `cliAgents[]`. One
// entry per detected agent on this device — multi-device aggregation is the
// frontend hook's job, not ours.
type cliAgentUsage struct {
	CliAgentID string `json:"cliAgentId,omitempty"`
	Provider   string `json:"provider"`
	Name       string `json:"name,omitempty"`
	Version    string `json:"version,omitempty"`
	Path       string `json:"path,omitempty"`
	Account    string `json:"account,omitempty"`
	Plan       string `json:"plan,omitempty"`
	// Model is the DEVICE-LEVEL default model this agent would use, when the
	// agent can report one. A project-level config in the session cwd can
	// override it, so the card labels this as the device default rather than
	// "the model in use". Additive on an existing snapshot shape — older
	// backends and frontends that do not read it are unaffected, and the four
	// pre-existing parsers simply never populate it.
	Model string `json:"model,omitempty"`
	// Models are the model ids this agent can currently reach, when it can
	// enumerate them. OpenCode is the case that needs it: it brokers whatever
	// providers the user configured and exposes no quota of its own, so the list
	// of reachable models IS the useful content of its card. Additive, like
	// Model — parsers that cannot enumerate simply leave it nil.
	Models             []string              `json:"models,omitempty"`
	AccountFingerprint string                `json:"accountFingerprint,omitempty"`
	Metrics            []cliAgentUsageMetric `json:"metrics,omitempty"`
	CollectedAt        string                `json:"collectedAt"`
	DataSource         string                `json:"dataSource,omitempty"`
	// Authentication is deliberately separate from access-token expiry. A
	// refreshable access token expiring is not a logged-out session.
	Authenticated        *bool  `json:"authenticated,omitempty"`
	AuthState            string `json:"authState,omitempty"`
	LoginExpiresAt       string `json:"loginExpiresAt,omitempty"`
	LoginExpirationState string `json:"loginExpirationState,omitempty"`
	// Notice is a card-level banner shown above the capacity bars — used when a
	// provider exposes a discrete usage-limit WARNING rather than a numeric
	// quota (e.g. Grok's server-pushed `usage_limit_reached` / access-gate).
	// NoticeSeverity is "warning" (approaching) or "error" (reached); NoticeURL
	// is an optional upgrade/billing link. Omitted when no limit is active.
	Notice         string `json:"notice,omitempty"`
	NoticeSeverity string `json:"noticeSeverity,omitempty"`
	NoticeURL      string `json:"noticeUrl,omitempty"`
}

const (
	loginExpirationKnown       = "known"
	loginExpirationRefreshable = "refreshable"
	loginExpirationNotReported = "not_reported"
)

func authBoolPtr(v bool) *bool { return &v }

// cliAgentUsageParser is the small interface every provider parser
// implements. Parsers MUST be best-effort: a missing config dir, unreadable
// JSON, or absent CLI binary all return (nil, false) without an error — the
// "no usage info available" state is normal, not a failure.
type cliAgentUsageParser interface {
	Provider() string
	Parse(home string, detected detectedCLIAgent, now time.Time) (*cliAgentUsage, bool)
}

// cliAgentUsageContextParser is implemented only by providers that perform
// optional bounded I/O during refresh. Existing parsers keep the small Parse
// contract unchanged.
type cliAgentUsageContextParser interface {
	ParseContext(context.Context, string, detectedCLIAgent, time.Time) (*cliAgentUsage, bool)
}

// cliAgentUsageError carries a per-provider failure record for the
// terminal-results subscriber. The Go agent emits one entry per provider
// that panicked, timed out, or couldn't gather. The backend's
// handleCliUsageRefreshResult uses these to advance cliUsageLastFailedAt
// when zero providers returned data.
type cliAgentUsageError struct {
	Provider string `json:"provider"`
	Message  string `json:"message"`
}

// gatherCLIAgentUsage walks the active catalog and returns the slice the
// auth/token request embeds as `cliAgents[]`. Catalog entries with no
// specialized parser still emit a baseline snapshot so newly configured CLI
// agents appear in utilization surfaces without a local terminal code change.
// Order is stable (alphabetical by provider) so byte-equal payloads produce
// byte-equal Firestore writes, which the terminal-service delta-skip relies on.
func gatherCLIAgentUsage(detected map[string]detectedCLIAgent, now time.Time) []cliAgentUsage {
	if len(detected) == 0 {
		return []cliAgentUsage{}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	host, err := os.Hostname()
	if err != nil {
		host = ""
	}

	parsers := cliAgentUsageParserIndex()
	out := make([]cliAgentUsage, 0, len(detected))
	for _, agent := range activeCLIAgentCatalog() {
		if !cliAgentCatalogSupportsUtilization(agent) {
			continue
		}
		entry, ok := detected[agent.ID]
		if !ok || !entry.Detected {
			continue
		}
		parser := parsers[cliAgentCatalogParserKey(agent)]
		provider := agent.ID
		var usage *cliAgentUsage
		var parsed bool
		if parser != nil {
			provider = parser.Provider()
			usage, parsed = parser.Parse(home, entry, now)
		}
		if !parsed || usage == nil {
			// Even when we can't enrich, still emit a baseline entry so the UI
			// shows the agent on the CLI Agents tab with "metrics unknown"
			// rather than dropping it entirely.
			usage = &cliAgentUsage{
				CliAgentID:  agent.ID,
				Provider:    provider,
				Name:        entry.Name,
				Version:     entry.Version,
				Path:        entry.Path,
				CollectedAt: now.UTC().Format(time.RFC3339),
			}
		}
		if usage.CliAgentID == "" {
			usage.CliAgentID = agent.ID
		}
		if usage.AccountFingerprint == "" {
			usage.AccountFingerprint = fallbackUnknownAccountFingerprint(usage.Provider, host, entry)
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

// GatherCLIAgentUsageOnly runs the CLI-usage probe in isolation — no CPU,
// memory, GPU, runtime, or shell probing. Used by the demand-driven
// __cli_usage_refresh__ handler in pubsub.go so an Active wake-up
// triggers a cheap re-poll of provider quotas without dragging the full
// 6-hour machine-info gather along with it.
//
// Per-provider gather runs under a 10s parent context. Any panic inside
// a parser is recovered into errs and the provider is omitted from the
// returned slice (rather than crashing the whole gather). Returns
// `(usage, errs)` — caller decides success vs. handled_failure based on
// `len(usage)` plus the presence of errors.
func GatherCLIAgentUsageOnly(ctx context.Context) ([]cliAgentUsage, []cliAgentUsageError) {
	gatherCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	detected := gatherCLIAgents()
	if len(detected) == 0 {
		return []cliAgentUsage{}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	host, err := os.Hostname()
	if err != nil {
		host = ""
	}
	now := time.Now()

	parsers := cliAgentUsageParserIndex()
	out := make([]cliAgentUsage, 0, len(detected))
	var errs []cliAgentUsageError

	for _, agent := range activeCLIAgentCatalog() {
		if !cliAgentCatalogSupportsUtilization(agent) {
			continue
		}
		entry, ok := detected[agent.ID]
		if !ok || !entry.Detected {
			continue
		}
		parser := parsers[cliAgentCatalogParserKey(agent)]
		provider := agent.ID
		// Each provider parser runs under defer-recover so a panic in one
		// parser cannot orphan the in-flight marker on the server side.
		// The recovered failure becomes an explicit error entry and the
		// provider is omitted from the success slice.
		var usage *cliAgentUsage
		if parser != nil {
			provider = parser.Provider()
			var errEntry *cliAgentUsageError
			usage, errEntry = runProviderParseSafely(gatherCtx, parser, home, entry, now)
			if errEntry != nil {
				errs = append(errs, *errEntry)
				continue
			}
		}
		if usage == nil {
			usage = &cliAgentUsage{
				CliAgentID:  agent.ID,
				Provider:    provider,
				Name:        entry.Name,
				Version:     entry.Version,
				Path:        entry.Path,
				CollectedAt: now.UTC().Format(time.RFC3339),
			}
		}
		if usage.CliAgentID == "" {
			usage.CliAgentID = agent.ID
		}
		if usage.AccountFingerprint == "" {
			usage.AccountFingerprint = fallbackUnknownAccountFingerprint(usage.Provider, host, entry)
		}
		out = append(out, *usage)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out, errs
}

// runProviderParseSafely invokes a parser under defer-recover and the
// parent context cancellation. If the parent context is canceled
// (e.g., the 10s timeout fired), it returns an error entry so the
// caller can attribute the failure rather than silently dropping the
// provider.
func runProviderParseSafely(
	ctx context.Context,
	parser cliAgentUsageParser,
	home string,
	entry detectedCLIAgent,
	now time.Time,
) (usage *cliAgentUsage, errEntry *cliAgentUsageError) {
	defer func() {
		if r := recover(); r != nil {
			usage = nil
			errEntry = &cliAgentUsageError{
				Provider: parser.Provider(),
				Message:  fmt.Sprintf("panic: %v", r),
			}
		}
	}()
	select {
	case <-ctx.Done():
		return nil, &cliAgentUsageError{
			Provider: parser.Provider(),
			Message:  "gather context canceled",
		}
	default:
	}
	var parsed *cliAgentUsage
	var ok bool
	if contextParser, supportsContext := parser.(cliAgentUsageContextParser); supportsContext {
		parsed, ok = contextParser.ParseContext(ctx, home, entry, now)
	} else {
		parsed, ok = parser.Parse(home, entry, now)
	}
	if ctx.Err() != nil {
		return nil, &cliAgentUsageError{
			Provider: parser.Provider(),
			Message:  "gather context canceled",
		}
	}
	if !ok || parsed == nil {
		// Non-fatal "we couldn't enrich this provider" — caller will
		// still emit the baseline entry. Don't surface as an error.
		return nil, nil
	}
	return parsed, nil
}
