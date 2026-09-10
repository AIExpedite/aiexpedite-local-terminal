// cliagent_models.go — asks each installed coding CLI which models it offers
// and which reasoning-effort levels each model accepts (Ship B5).
//
// The Ship settings card used to take both from a hand-regenerated catalog in
// the frontend. Vendors ship on their own timetable, so that copy drifted; the
// device can ask the binary it will actually run. What each CLI exposes,
// verified 2026-09-05 on the installed binaries:
//
//   - Codex keeps `~/.codex/models_cache.json` (server-fetched, etag). Each
//     model carries `slug`, `display_name`, `supported_reasoning_levels`,
//     `default_reasoning_level`, `visibility` (list | hide) and `priority`.
//   - Antigravity prints `agy models` as tab-separated `slug<TAB>name`, and the
//     slug EMBEDS the effort for the models that take one (`gemini-3.8-flash-high`,
//     `gpt-oss-120b-medium`). `--model gemini-3.8-flash --effort high` is the
//     accepted form, `--model gemini-3.8-flash-high --effort low` conflicts, and
//     `--effort` is refused outright for the third-party slugs
//     (`claude-sonnet-4-6`). So the suffixed slugs fold into one family with an
//     effort scale, and an unsuffixed slug is reported as taking no effort.
//   - Grok prints `grok models` as a bulleted list (`* grok-4.6 (default)`,
//     `- grok-4.5`) with no levels, but listing while signed in refreshes
//     `~/.grok/models_cache.json`, whose per-model `reasoning_efforts` menu is
//     what the TUI's `/model` picker shows. The list gives the order and the
//     default, the cache gives label, scale and default level.
//   - Claude Code has no list command (`claude model list` starts a session).
//     Its aliases are what a device can report, and the scale comes from the
//     `--effort <level>` line of `claude --help`. The list is NOT exhaustive —
//     full ids are accepted too — so the resolver must not veto on it.
//   - OpenCode already enumerates through `opencode models` (the readiness probe
//     in cliagent_usage_opencode.go); its ids are re-shaped here so every agent
//     reports the same structure.
//
// Every probe is best-effort and bounded: a missing file, an unparseable
// answer or a timeout leaves the snapshot without model details, and the
// backend falls back to the catalog document. Results are cached per binary so
// the periodic machine-info gather does not spawn a CLI every cycle.
package main

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// cliAgentModelDetail is one model as the CLI reports it. `Efforts` present
// means the CLI named the scale; absent with `NoEffort` set means the CLI said
// this model takes no effort flag; absent otherwise means the CLI did not say.
// The three states are deliberately distinct: the backend clamps to a named
// scale, and a later phase drops the flag for a model that refuses one.
type cliAgentModelDetail struct {
	ID            string   `json:"id"`
	Label         string   `json:"label,omitempty"`
	Efforts       []string `json:"efforts,omitempty"`
	DefaultEffort string   `json:"defaultEffort,omitempty"`
	NoEffort      bool     `json:"noEffort,omitempty"`
}

// cliAgentModelDiscovery is what one probe learned about one CLI.
type cliAgentModelDiscovery struct {
	Models []cliAgentModelDetail
	// Exhaustive is true when the list is everything the CLI will accept; a
	// model absent from an exhaustive list is "not offered". Claude Code's
	// alias set is the one non-exhaustive list today.
	Exhaustive bool
	// DefaultModel is the model the CLI marks as its own default, when it does.
	DefaultModel string
}

const (
	// cliAgentModelProbeTTL bounds how often a CLI is asked; the list changes
	// with releases, not minutes, and the gather loop runs far more often.
	cliAgentModelProbeTTL = 30 * time.Minute
	// cliAgentModelProbeTimeout caps one list command. `agy models` fetches
	// its list from the network; the others answer from disk or a socket.
	cliAgentModelProbeTimeout = 20 * time.Second
	// cliUsageMaxEffortsPerModel is the receipt bound on one model's scale.
	cliUsageMaxEffortsPerModel = 16
	// cliUsageMaxEffortLength bounds one effort token in the receipt.
	cliUsageMaxEffortLength = 32
)

// cliAgentEffortRank orders the levels every coding CLI draws from — the same
// union shared-constants calls RULE_EFFORTS, minus `auto`, which means "pass no
// flag" and is never a level a CLI names. A token outside this set is not an
// effort and is never reported as one.
var cliAgentEffortRank = map[string]int{
	"minimal": 0, "low": 1, "medium": 2, "high": 3, "xhigh": 4, "max": 5, "ultra": 6,
}

// claudeModelAliases is the alias set `claude --model` documents. It is a
// floor, not a ceiling: full model ids are accepted too, which is why Claude's
// discovery is reported as non-exhaustive.
var claudeModelAliases = []cliAgentModelDetail{
	{ID: "fable", Label: "Fable"},
	{ID: "opus", Label: "Opus"},
	{ID: "sonnet", Label: "Sonnet"},
	{ID: "haiku", Label: "Haiku"},
}

type cliAgentModelProbeEntry struct {
	At     time.Time
	Result cliAgentModelDiscovery
	OK     bool
}

var (
	cliAgentModelProbeMu    sync.Mutex
	cliAgentModelProbeCache = map[string]cliAgentModelProbeEntry{}
	// cliAgentModelProbeGeneration advances on every reset. A probe records
	// the generation it started under and stores its answer only if no reset
	// happened meanwhile — otherwise a six-hour gather already in flight when
	// a user forced a refresh would repopulate the cache with its pre-reset
	// answer, and the refresh would read that stale entry for the whole TTL.
	cliAgentModelProbeGeneration uint64
	// cliAgentModelProbeRunner is swapped by tests so a probe never spawns the
	// real CLI; production runs the bounded command below.
	cliAgentModelProbeRunner = runCLIAgentModelProbe
)

// resetCLIAgentModelProbeCache empties the cache (tests, and a forced usage
// refresh, which wants the CLI's current answer rather than a half-hour-old one)
// and invalidates every probe still in flight.
func resetCLIAgentModelProbeCache() {
	cliAgentModelProbeMu.Lock()
	cliAgentModelProbeCache = map[string]cliAgentModelProbeEntry{}
	cliAgentModelProbeGeneration++
	cliAgentModelProbeMu.Unlock()
}

// attachCLIAgentModelDiscovery adds the model list and effort scales to one
// provider's usage snapshot. Called by gatherCLIAgentUsage after the provider's
// parser ran, so it enriches whatever the parser produced (including the
// baseline entry for a parser-less agent) and never replaces a list the parser
// already established.
func attachCLIAgentModelDiscovery(ctx context.Context, agentID string, detected detectedCLIAgent, usage *cliAgentUsage, home string, now time.Time) {
	if usage == nil {
		return
	}
	if strings.EqualFold(agentID, "opencode") {
		// OpenCode enumerated through its readiness probe; re-shape only.
		if len(usage.Models) == 0 {
			return
		}
		details := make([]cliAgentModelDetail, 0, len(usage.Models))
		for _, id := range usage.Models {
			details = append(details, cliAgentModelDetail{ID: id})
		}
		usage.ModelDetails = boundedModelDetails(details)
		// The readiness probe stops reading at the receipt cap, so a list AT
		// the cap may be a truncated catalog; and a provider id the detail row
		// cannot carry (over its id bound) was listed but is not reported.
		// Either way the details are not everything OpenCode accepts, and an
		// exhaustive flag there would let routing veto a model OpenCode runs.
		capped := len(usage.Models) >= cliUsageMaxModelsPerProvider
		dropped := len(usage.ModelDetails) < len(usage.Models)
		usage.ModelsExhaustive = authBoolPtr(!capped && !dropped)
		return
	}
	discovery, ok := cachedCLIAgentModelDiscovery(ctx, agentID, detected, home, now)
	if !ok || len(discovery.Models) == 0 {
		return
	}
	details := boundedModelDetails(discovery.Models)
	usage.ModelDetails = details
	// Bounding that changed the catalog — a row past the cap or an id the
	// detail row cannot carry — means the published list is not everything
	// the CLI reported, so it cannot claim to be everything the CLI accepts.
	usage.ModelsExhaustive = authBoolPtr(discovery.Exhaustive && len(details) == len(discovery.Models))
	if len(usage.Models) == 0 {
		// From the BOUNDED details, not the raw discovery: canonicalProvider
		// rejects the whole provider when `models` exceeds the same cap, so a
		// vendor answering with more than the cap would fail the entire refresh
		// instead of publishing the first cliUsageMaxModelsPerProvider entries.
		ids := make([]string, 0, len(details))
		for _, model := range details {
			ids = append(ids, model.ID)
		}
		usage.Models = ids
	}
	// The receipt bounds `model` at 256 bytes like a detail id; a configured
	// or reported default past that is left unset rather than copied into a
	// field that would fail the whole provider in canonicalProvider.
	if usage.Model == "" && discovery.DefaultModel != "" && bounded(discovery.DefaultModel, cliUsageMaxModelDetailIDLength) {
		usage.Model = discovery.DefaultModel
	}
}

func cachedCLIAgentModelDiscovery(ctx context.Context, agentID string, detected detectedCLIAgent, home string, now time.Time) (cliAgentModelDiscovery, bool) {
	key := strings.ToLower(agentID) + "\x00" + detected.Path + "\x00" + detected.Version
	cliAgentModelProbeMu.Lock()
	entry, cached := cliAgentModelProbeCache[key]
	generation := cliAgentModelProbeGeneration
	cliAgentModelProbeMu.Unlock()
	if cached && now.Sub(entry.At) < cliAgentModelProbeTTL {
		return entry.Result, entry.OK
	}
	result, ok := discoverCLIAgentModels(ctx, agentID, detected, home)
	if !ok && ctx != nil && ctx.Err() != nil {
		// The probe lost its deadline rather than answering. Caching that miss
		// would hide this agent's models for the full TTL because one refresh
		// happened to run out of time, so leave the slot empty and let the
		// next gather ask again.
		return result, ok
	}
	cliAgentModelProbeMu.Lock()
	// A reset while this probe ran means a forced refresh wants a FRESH
	// answer: this one is returned to its own caller but never stored, so the
	// refresh cannot find it and reuse a pre-reset list for the whole TTL.
	if generation == cliAgentModelProbeGeneration {
		cliAgentModelProbeCache[key] = cliAgentModelProbeEntry{At: now, Result: result, OK: ok}
	}
	cliAgentModelProbeMu.Unlock()
	return result, ok
}

// discoverCLIAgentModels runs the one probe this agent supports. ok=false means
// the probe was inconclusive (no file, timeout, unrecognised output) and the
// caller must report nothing rather than an empty list.
func discoverCLIAgentModels(ctx context.Context, agentID string, detected detectedCLIAgent, home string) (cliAgentModelDiscovery, bool) {
	switch strings.ToLower(strings.TrimSpace(agentID)) {
	case "codex":
		return discoverCodexModels(home, detected.Version)
	case "antigravity":
		out, ok := cliAgentModelProbeRunner(ctx, detected.Path, sanitizeAntigravityEnv(os.Environ()), "models")
		if !ok {
			return cliAgentModelDiscovery{}, false
		}
		return parseAntigravityModelList(out)
	case "grok":
		return discoverGrokModels(ctx, detected, home)
	case "claudecode":
		out, ok := cliAgentModelProbeRunner(ctx, detected.Path, os.Environ(), "--help")
		if !ok {
			return cliAgentModelDiscovery{}, false
		}
		return claudeModelDiscovery(out), true
	}
	return cliAgentModelDiscovery{}, false
}

// runCLIAgentModelProbe runs `<executable> <args…>` with a short timeout and
// returns its combined output. ok=false means INCONCLUSIVE (timeout, non-zero
// exit, spawn failure); the caller must not read a verdict into that.
//
// The timeout DERIVES from the caller's gather context rather than starting a
// fresh one: GatherCLIAgentUsageOnly runs every provider serially under a
// single 10s deadline, so a probe on its own 20s clock would keep running past
// the deadline the refresh handler documents. context.WithTimeout takes the
// EARLIER of the two, so this caps a probe at 20s on the 6-hour gather path
// and at whatever the refresh has left on the demand-driven one.
func runCLIAgentModelProbe(ctx context.Context, executable string, env []string, args ...string) (string, bool) {
	if strings.TrimSpace(executable) == "" {
		return "", false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, cliAgentModelProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, executable, args...)
	hideWindow(cmd)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", false
	}
	return string(out), true
}

/* --------------------------------------------------------------------------
   Codex — ~/.codex/models_cache.json
   -------------------------------------------------------------------------- */

type codexModelsCacheFile struct {
	// ClientVersion is the Codex build that fetched the cache. After an
	// upgrade the file still describes the OLD build's lineup until the new
	// binary's first run rewrites it (observed 2026-09-05: 0.148.0's cache
	// lacked gpt-6-astra, 0.153.4's default, for the minute between the
	// install and the first `codex exec`), so a cache from another build is
	// reported as non-exhaustive rather than letting it veto a pin.
	ClientVersion string `json:"client_version"`
	Models        []struct {
		Slug                     string `json:"slug"`
		DisplayName              string `json:"display_name"`
		SupportedReasoningLevels []struct {
			Effort string `json:"effort"`
		} `json:"supported_reasoning_levels"`
		DefaultReasoningLevel string `json:"default_reasoning_level"`
		Visibility            string `json:"visibility"`
		Priority              int    `json:"priority"`
	} `json:"models"`
}

func codexModelsCachePath(home string) string {
	base := firstNonEmpty(os.Getenv("CODEX_HOME"), expandHome(home, ".codex"))
	if base == "" {
		return ""
	}
	return expandHome(base, "models_cache.json")
}

func discoverCodexModels(home, installedVersion string) (cliAgentModelDiscovery, bool) {
	path := codexModelsCachePath(home)
	if path == "" {
		return cliAgentModelDiscovery{}, false
	}
	configured := readCodexConfiguredModel(home)
	var cache codexModelsCacheFile
	if !readJSONFile(path, &cache) {
		// No cache yet (fresh install, cleared cache): the configured model
		// is still what `codex` runs on this machine, so report it as a
		// one-model, non-exhaustive floor rather than nothing at all.
		if configured == "" {
			return cliAgentModelDiscovery{}, false
		}
		return reconcileCodexDiscovery(cliAgentModelDiscovery{}, "", installedVersion, configured), true
	}
	out, ok := parseCodexModelsCache(cache)
	if !ok {
		if configured == "" {
			return out, false
		}
		return reconcileCodexDiscovery(cliAgentModelDiscovery{}, cache.ClientVersion, installedVersion, configured), true
	}
	return reconcileCodexDiscovery(out, cache.ClientVersion, installedVersion, configured), true
}

// reconcileCodexDiscovery applies the two facts the cache alone cannot carry:
// the model the user configured in config.toml is offered whether or not the
// cache lists it (it is what `codex` runs by default on this machine), and a
// cache written by a different Codex build than the one installed is not the
// whole story, so the list is marked non-exhaustive. Either way a stale cache
// can no longer empty a routing slot.
func reconcileCodexDiscovery(out cliAgentModelDiscovery, cacheVersion, installedVersion, configuredModel string) cliAgentModelDiscovery {
	if configuredModel != "" {
		out.DefaultModel = configuredModel
		if !containsModelID(out.Models, configuredModel) {
			out.Models = append([]cliAgentModelDetail{{ID: configuredModel}}, out.Models...)
			out.Exhaustive = false
		}
	}
	if !codexCacheMatchesInstalled(cacheVersion, installedVersion) {
		out.Exhaustive = false
	}
	return out
}

// codexCacheMatchesInstalled compares a cache's writer version with the
// installed binary's `--version` output ("codex-cli 0.153.4", "grok 1.0.13
// (…) [stable]"); Grok's cache reuses it. An unknown version on either side is
// treated as a match: the cache is then only as trustworthy as before, and the
// configured-model rule still applies.
func codexCacheMatchesInstalled(cacheVersion, installedVersion string) bool {
	cacheToken := versionToken(cacheVersion)
	installedToken := versionToken(installedVersion)
	if cacheToken == "" || installedToken == "" {
		return true
	}
	return cacheToken == installedToken
}

var versionTokenPattern = regexp.MustCompile(`\d+\.\d+(?:\.\d+)?`)

// versionToken extracts the first version-shaped token from a `--version`
// line or a cache field: "codex-cli 0.153.4", "grok 1.0.13 (5e9a…) [stable]"
// and "v0.153.4" all yield the bare number.
func versionToken(value string) string {
	return versionTokenPattern.FindString(value)
}

// codexConfigModelLine matches a top-level `model = "…"` or `model = '…'` in
// config.toml — TOML's basic and literal string forms are both valid there.
var codexConfigModelLine = regexp.MustCompile(`(?m)^\s*model\s*=\s*(?:"([^"]+)"|'([^']+)')`)

// codexConfigTableHeader matches the first TOML table header (`[profiles.x]`,
// `[[array]]`), on any line including the first.
var codexConfigTableHeader = regexp.MustCompile(`(?m)^\s*\[`)

// readCodexConfiguredModel returns the top-level `model` from
// CODEX_HOME/config.toml, or "" when there is none.
func readCodexConfiguredModel(home string) string {
	base := firstNonEmpty(os.Getenv("CODEX_HOME"), expandHome(home, ".codex"))
	if base == "" {
		return ""
	}
	raw, err := os.ReadFile(expandHome(base, "config.toml"))
	if err != nil {
		return ""
	}
	// Only the top-level table: a `[profiles.x]` section's model is not the
	// default this machine runs. Stop reading at the first table header —
	// including one on the very first line, which a `\n[` search would miss
	// and so read a profile's model as the default.
	text := string(raw)
	if header := codexConfigTableHeader.FindStringIndex(text); header != nil {
		text = text[:header[0]]
	}
	match := codexConfigModelLine.FindStringSubmatch(text)
	if match == nil {
		return ""
	}
	return strings.TrimSpace(firstNonEmpty(match[1], match[2]))
}

func containsModelID(models []cliAgentModelDetail, id string) bool {
	for _, model := range models {
		if model.ID == id {
			return true
		}
	}
	return false
}

// parseCodexModelsCache keeps the models Codex itself lists (`visibility:
// "list"`, or unset on older caches) in Codex's own priority order. Hidden
// models (`gpt-reserve`) are internal fallbacks the picker should not offer.
func parseCodexModelsCache(cache codexModelsCacheFile) (cliAgentModelDiscovery, bool) {
	type ranked struct {
		detail   cliAgentModelDetail
		priority int
		index    int
	}
	var kept []ranked
	seen := map[string]bool{}
	for index, model := range cache.Models {
		slug := strings.TrimSpace(model.Slug)
		if slug == "" || seen[slug] {
			continue
		}
		visibility := strings.ToLower(strings.TrimSpace(model.Visibility))
		if visibility != "" && visibility != "list" {
			continue
		}
		seen[slug] = true
		detail := cliAgentModelDetail{ID: slug, Label: strings.TrimSpace(model.DisplayName)}
		for _, level := range model.SupportedReasoningLevels {
			if effort, ok := normalizeEffortToken(level.Effort); ok {
				detail.Efforts = appendEffort(detail.Efforts, effort)
			}
		}
		if effort, ok := normalizeEffortToken(model.DefaultReasoningLevel); ok && containsString(detail.Efforts, effort) {
			detail.DefaultEffort = effort
		}
		// "Takes no effort flag" only when Codex itself listed NO levels. A
		// model whose levels are all outside the shared union (a level Codex
		// introduced that this build does not know) has an UNKNOWN scale —
		// reporting NoEffort there would make the resolver drop the flag on a
		// model that accepts one.
		if len(model.SupportedReasoningLevels) == 0 {
			detail.NoEffort = true
		}
		kept = append(kept, ranked{detail: detail, priority: model.Priority, index: index})
	}
	if len(kept) == 0 {
		return cliAgentModelDiscovery{}, false
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].priority != kept[j].priority {
			return kept[i].priority < kept[j].priority
		}
		return kept[i].index < kept[j].index
	})
	// A floor, never the whole list: this file is a server-fetched cache that
	// only a `codex` run refreshes, so Codex's catalog can gain a model with
	// no binary upgrade and the cache — same client_version and all — would
	// not know. An exhaustive claim here would let routing veto a pin to a
	// model Codex already runs. (The version comparison in reconcile still
	// drives Grok's cache, which a live list probe refreshes.)
	out := cliAgentModelDiscovery{Exhaustive: false}
	for _, item := range kept {
		out.Models = append(out.Models, boundedModelDetail(item.detail))
	}
	return out, true
}

/* --------------------------------------------------------------------------
   Antigravity — `agy models`
   -------------------------------------------------------------------------- */

// antigravityLabelSuffix strips the parenthesised effort a display name ends
// with ("Gemini 3.8 Flash (High)") once the slug's suffix has been folded.
var antigravityLabelSuffix = regexp.MustCompile(`\s*\((?i:minimal|low|medium|high|xhigh|max|ultra)\)\s*$`)

// parseAntigravityModelList folds `agy models` into families. A slug ending in
// an effort level contributes that level to its family's scale; a slug ending
// in none is a model that refuses `--effort` and is reported as such. Family
// order is first appearance, which is Antigravity's own (newest first).
func parseAntigravityModelList(output string) (cliAgentModelDiscovery, bool) {
	var order []string
	byID := map[string]*cliAgentModelDetail{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		tab := strings.IndexByte(line, '\t')
		if tab <= 0 {
			// Progress chatter ("Fetching available models...") has no tab.
			continue
		}
		slug := strings.TrimSpace(line[:tab])
		label := strings.TrimSpace(line[tab+1:])
		if slug == "" || strings.ContainsAny(slug, " \t") {
			continue
		}
		family, effort := splitEffortSuffix(slug)
		detail, exists := byID[family]
		if !exists {
			detail = &cliAgentModelDetail{ID: family}
			byID[family] = detail
			order = append(order, family)
		}
		if effort != "" {
			detail.Efforts = appendEffort(detail.Efforts, effort)
			label = antigravityLabelSuffix.ReplaceAllString(label, "")
		}
		if detail.Label == "" && label != "" {
			detail.Label = label
		}
	}
	if len(order) == 0 {
		return cliAgentModelDiscovery{}, false
	}
	out := cliAgentModelDiscovery{Exhaustive: true}
	for _, id := range order {
		detail := *byID[id]
		if len(detail.Efforts) == 0 {
			detail.NoEffort = true
		}
		out.Models = append(out.Models, boundedModelDetail(detail))
	}
	return out, true
}

// splitEffortSuffix returns (family, effort) for `<family>-<effort>` when the
// suffix is a known level, else (slug, "").
func splitEffortSuffix(slug string) (string, string) {
	dash := strings.LastIndexByte(slug, '-')
	if dash <= 0 || dash == len(slug)-1 {
		return slug, ""
	}
	effort, ok := normalizeEffortToken(slug[dash+1:])
	if !ok {
		return slug, ""
	}
	return slug[:dash], effort
}

/* --------------------------------------------------------------------------
   Grok — `grok models` + ~/.grok/models_cache.json
   -------------------------------------------------------------------------- */

// grokModelsCacheFile is the model catalog Grok fetches from its backend when
// it lists models while signed in (`origin: …/v1/models`). It is the source of
// the per-model effort menu the TUI's `/model` picker shows — `grok models`
// itself prints ids only — so the list command is run first (it refreshes the
// cache) and the cache is read second.
type grokModelsCacheFile struct {
	GrokVersion string `json:"grok_version"`
	Models      map[string]struct {
		Info struct {
			ID                      string `json:"id"`
			Name                    string `json:"name"`
			Hidden                  bool   `json:"hidden"`
			ReasoningEffort         string `json:"reasoning_effort"`
			SupportsReasoningEffort *bool  `json:"supports_reasoning_effort"`
			ReasoningEfforts        []struct {
				ID      string `json:"id"`
				Value   string `json:"value"`
				Default bool   `json:"default"`
			} `json:"reasoning_efforts"`
		} `json:"info"`
	} `json:"models"`
}

// sanitizeGrokModelListEnv is sanitizeGrokMaintenanceSmokeEnv with Grok's
// state-directory configuration put back. The smoke sanitizer drops every
// GROK_* variable so a probe cannot inherit workspace integrations — but
// GROK_HOME chooses which login and cache the CLI uses, and grokModelsCachePath
// reads that SAME override right after. Dropping it would list one home's
// models and merge another home's cache, so the snapshot would describe an
// account this device never runs.
func sanitizeGrokModelListEnv(env []string) []string {
	// The read-only list must describe the SAME service, config and login as
	// the ACP sessions it stands for: GROK_HOME (the login and cache directory
	// the merged cache is read from), the endpoint overrides
	// (GROK_MODELS_LIST_URL, GROK_MODELS_BASE_URL, GROK_API_BASE_URL,
	// XAI_API_BASE_URL), GROK_CONFIG_PATH — and XAI_API_KEY only when the user
	// opted into Config.EnableGrokAPIKeyFallback, the opt-in sanitizeGrokACPEnv
	// honours. Listing with all of those stripped would ask the default
	// service for a catalog the configured sessions never use (or list
	// logged-out on a key-authenticated host) and publish it as exhaustive.
	//
	// But NOT every GROK_* var: the maintenance smoke strips the family
	// wholesale because some of it changes what a non-interactive child DOES
	// (GROK_LOG_FILE writes raw diagnostics outside the isolated home,
	// GROK_FUTURE_EXECUTION_OVERRIDE swaps the execution path), and the smoke
	// suite pins that no such sink is ever written. So the probe starts from
	// the smoke sanitizer and restores an explicit allowlist: the variables
	// that decide WHICH service, config and login the session talks to.
	filtered := sanitizeGrokMaintenanceSmokeEnv(env)
	allowAPIKey := grokAPIKeyFallbackOptedIn()
	for _, entry := range env {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		upper := strings.ToUpper(strings.TrimSpace(name))
		if upper == "XAI_API_KEY" {
			if allowAPIKey {
				filtered = setEnvVar(filtered, "XAI_API_KEY", value)
			}
			continue
		}
		if containsString(grokModelListRoutingEnv, upper) {
			filtered = setEnvVar(filtered, upper, value)
		}
	}
	return filtered
}

// grokModelListRoutingEnv is the configuration an ACP session inherits that
// decides which service, config file and login `grok models` must describe —
// restored on top of the maintenance-smoke sanitizer, which strips them with
// the rest of GROK_*. Nothing here changes what the child does, only where it
// looks.
var grokModelListRoutingEnv = []string{
	"GROK_HOME",
	"GROK_CONFIG_PATH",
	"GROK_API_BASE_URL",
	"GROK_MODELS_BASE_URL",
	"GROK_MODELS_LIST_URL",
	"XAI_API_BASE_URL",
}

// grokAPIKeyFallbackOptedIn reads the live config's API-key opt-in — the same
// flag the ACP launch passes as AllowAPIKeyFallback. Absent config (tests, a
// pre-registration probe) means the default: opt-in only, key stripped.
func grokAPIKeyFallbackOptedIn() bool {
	cfg := shutdownConfig
	return cfg != nil && cfg.EnableGrokAPIKeyFallback
}

func grokModelsCachePath(home string) string {
	base := firstNonEmpty(os.Getenv("GROK_HOME"), expandHome(home, ".grok"))
	if base == "" {
		return ""
	}
	return expandHome(base, "models_cache.json")
}

// discoverGrokModels lists through the CLI (which refreshes the cache when
// signed in) and enriches every listed model from the cache. A missing cache
// leaves the list without scales; a missing list falls back to the cache's
// visible models.
func discoverGrokModels(ctx context.Context, detected detectedCLIAgent, home string) (cliAgentModelDiscovery, bool) {
	var listed cliAgentModelDiscovery
	listedOK := false
	// The list runs against an ISOLATED home built exactly as a managed
	// session's is (setupIsolatedGrokHomeFrom): the cached login is copied
	// in, the persisted `[model] api_key` only under the same
	// Config.EnableGrokAPIKeyFallback opt-in. Pointing `grok models` at the
	// real home would let a persisted key the user never opted into
	// authenticate the list, and the catalog of THAT account could veto the
	// models the cached-token sessions actually run.
	realBase := grokModelsHomeBase(home)
	env := sanitizeGrokModelListEnv(os.Environ())
	isolated, err := setupIsolatedGrokHomeFrom(grokAPIKeyFallbackOptedIn(), "", realBase)
	if err == nil {
		env = setEnvVar(env, "GROK_HOME", isolated)
		defer func() { _ = removeIsolatedGrokHome(isolated) }()
	}
	if out, ok := cliAgentModelProbeRunner(ctx, detected.Path, env, "models"); ok {
		listed, listedOK = parseGrokModelList(out)
	}
	// Listing while signed in refreshes the cache under the home the child
	// ran with — the isolated one — so that copy is read first; the real
	// home's cache is the fallback (a logged-out list writes none).
	var cache grokModelsCacheFile
	cacheOK := false
	if isolated != "" {
		cacheOK = readJSONFile(expandHome(isolated, "models_cache.json"), &cache)
	}
	if !cacheOK {
		if path := grokModelsCachePath(home); path != "" {
			cacheOK = readJSONFile(path, &cache)
		}
	}
	if !listedOK && !cacheOK {
		return cliAgentModelDiscovery{}, false
	}
	return mergeGrokDiscovery(listed, listedOK, cache, cacheOK, detected.Version), true
}

// grokModelsHomeBase is the real Grok state directory the isolated list home
// is seeded from: $GROK_HOME, else ~/.grok.
func grokModelsHomeBase(home string) string {
	return firstNonEmpty(os.Getenv("GROK_HOME"), expandHome(home, ".grok"))
}

// mergeGrokDiscovery keeps the list command's order (its default first) and
// takes each model's label, scale and default level from the cache. Cache-only
// models that are not hidden are appended; a cache written by another Grok
// build than the one installed marks the result non-exhaustive, as for Codex.
func mergeGrokDiscovery(listed cliAgentModelDiscovery, listedOK bool, cache grokModelsCacheFile, cacheOK bool, installedVersion string) cliAgentModelDiscovery {
	out := cliAgentModelDiscovery{Exhaustive: true, DefaultModel: listed.DefaultModel}
	seen := map[string]bool{}
	enrich := func(detail cliAgentModelDetail) cliAgentModelDetail {
		if !cacheOK {
			return detail
		}
		entry, ok := cache.Models[detail.ID]
		if !ok {
			return detail
		}
		if detail.Label == "" {
			detail.Label = strings.TrimSpace(entry.Info.Name)
		}
		if len(detail.Efforts) == 0 {
			for _, level := range entry.Info.ReasoningEfforts {
				if effort, ok := normalizeEffortToken(firstNonEmpty(level.Value, level.ID)); ok {
					detail.Efforts = appendEffort(detail.Efforts, effort)
					if level.Default && detail.DefaultEffort == "" {
						detail.DefaultEffort = effort
					}
				}
			}
			if detail.DefaultEffort == "" {
				if effort, ok := normalizeEffortToken(entry.Info.ReasoningEffort); ok && containsString(detail.Efforts, effort) {
					detail.DefaultEffort = effort
				}
			}
			if len(detail.Efforts) == 0 && entry.Info.SupportsReasoningEffort != nil && !*entry.Info.SupportsReasoningEffort {
				detail.NoEffort = true
			}
		}
		return boundedModelDetail(detail)
	}
	if listedOK {
		for _, detail := range listed.Models {
			if seen[detail.ID] {
				continue
			}
			seen[detail.ID] = true
			out.Models = append(out.Models, enrich(detail))
		}
	}
	if cacheOK {
		ids := make([]string, 0, len(cache.Models))
		for id := range cache.Models {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if seen[id] || cache.Models[id].Info.Hidden || strings.TrimSpace(id) == "" {
				continue
			}
			seen[id] = true
			out.Models = append(out.Models, enrich(cliAgentModelDetail{ID: id}))
		}
		if !codexCacheMatchesInstalled(cache.GrokVersion, installedVersion) {
			out.Exhaustive = false
		}
	}
	// Without the list command there is nothing that proves the cache is
	// current: Grok fetches this catalog from its backend, so it can gain a
	// model with no binary upgrade and the version comparison above would
	// still match. Claiming exhaustive there would let routing veto a model
	// the CLI accepts, so a cache-only answer is always a floor.
	if !listedOK {
		out.Exhaustive = false
	}
	return out
}

var grokModelLine = regexp.MustCompile(`^\s*[*\-•]\s+(\S+)(.*)$`)

// parseGrokModelList reads the bulleted list `grok models` prints. The line
// marked `(default)` is the device default; any effort levels named on a model's
// line are its scale, and a line that names none leaves the scale unknown.
func parseGrokModelList(output string) (cliAgentModelDiscovery, bool) {
	out := cliAgentModelDiscovery{Exhaustive: true}
	seen := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		match := grokModelLine.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if match == nil {
			continue
		}
		id := strings.TrimSpace(match[1])
		if id == "" || seen[id] {
			continue
		}
		rest := match[2]
		if strings.Contains(strings.ToLower(rest), "(default)") {
			out.DefaultModel = id
		}
		seen[id] = true
		detail := cliAgentModelDetail{ID: id}
		for _, token := range strings.FieldsFunc(rest, func(r rune) bool {
			return r == ' ' || r == ',' || r == '(' || r == ')' || r == '[' || r == ']' || r == ':' || r == '|' || r == '/'
		}) {
			if effort, ok := normalizeEffortToken(token); ok {
				detail.Efforts = appendEffort(detail.Efforts, effort)
			}
		}
		out.Models = append(out.Models, boundedModelDetail(detail))
	}
	if len(out.Models) == 0 {
		return cliAgentModelDiscovery{}, false
	}
	return out, true
}

/* --------------------------------------------------------------------------
   Claude Code — aliases + the scale from `claude --help`
   -------------------------------------------------------------------------- */

var claudeHelpEffortScale = regexp.MustCompile(`--effort\s+<[^>]*>[^\n]*\n?[^\n(]*\(([a-z,\s]+)\)`)

// claudeModelDiscovery reports the alias set with the scale `claude --help`
// prints on its `--effort <level>` line. Non-exhaustive by construction.
func claudeModelDiscovery(help string) cliAgentModelDiscovery {
	var scale []string
	if match := claudeHelpEffortScale.FindStringSubmatch(help); match != nil {
		for _, token := range strings.Split(match[1], ",") {
			if effort, ok := normalizeEffortToken(token); ok {
				scale = appendEffort(scale, effort)
			}
		}
	}
	out := cliAgentModelDiscovery{Exhaustive: false}
	for _, alias := range claudeModelAliases {
		detail := alias
		if len(scale) > 0 {
			detail.Efforts = append([]string{}, scale...)
		}
		out.Models = append(out.Models, detail)
	}
	return out
}

/* --------------------------------------------------------------------------
   Helpers
   -------------------------------------------------------------------------- */

// normalizeEffortToken lower-cases and trims one token and accepts it only
// when it is a level every CLI draws from. ("extra high" style labels are not
// tokens a CLI accepts on its flag and are not folded.)
func normalizeEffortToken(value string) (string, bool) {
	token := strings.ToLower(strings.TrimSpace(value))
	if _, ok := cliAgentEffortRank[token]; !ok {
		return "", false
	}
	return token, true
}

// appendEffort adds a level once and keeps the scale ordered low → high, which
// is the order the slider and the clamp both assume.
func appendEffort(scale []string, effort string) []string {
	if containsString(scale, effort) {
		return scale
	}
	scale = append(scale, effort)
	sort.SliceStable(scale, func(i, j int) bool { return cliAgentEffortRank[scale[i]] < cliAgentEffortRank[scale[j]] })
	return scale
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// boundedModelDetail applies the receipt bounds at collection time so a
// pathological CLI answer cannot invalidate an otherwise usable refresh.
func boundedModelDetail(detail cliAgentModelDetail) cliAgentModelDetail {
	if !bounded(detail.Label, 256) {
		detail.Label = ""
	}
	if len(detail.Efforts) > cliUsageMaxEffortsPerModel {
		detail.Efforts = detail.Efforts[:cliUsageMaxEffortsPerModel]
	}
	return detail
}

// cliUsageMaxModelDetailIDLength is the receipt bound on one model id in
// `modelDetails` (the legacy `models` list allows 2048; the detail row is the
// one the verifier rejects at 256).
const cliUsageMaxModelDetailIDLength = 256

// boundedModelDetails drops the rows the receipt would reject — an empty id or
// one over the id bound — and truncates the rest to the receipt cap, keeping
// the CLI's order so the first (newest / highest-priority) entries survive.
// Applied at collection time so one pathological CLI answer can never turn
// the whole refresh into `usage result rejected`.
func boundedModelDetails(details []cliAgentModelDetail) []cliAgentModelDetail {
	kept := make([]cliAgentModelDetail, 0, len(details))
	for _, detail := range details {
		if detail.ID == "" || !bounded(detail.ID, cliUsageMaxModelDetailIDLength) {
			continue
		}
		kept = append(kept, detail)
		if len(kept) == cliUsageMaxModelsPerProvider {
			break
		}
	}
	return kept
}
