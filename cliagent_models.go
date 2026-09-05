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
//     `- grok-4.5`), logged in or not; any effort levels it names on a line are
//     read from the same line.
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
	// cliAgentModelProbeRunner is swapped by tests so a probe never spawns the
	// real CLI; production runs the bounded command below.
	cliAgentModelProbeRunner = runCLIAgentModelProbe
)

// resetCLIAgentModelProbeCache empties the cache (tests, and a forced usage
// refresh, which wants the CLI's current answer rather than a half-hour-old one).
func resetCLIAgentModelProbeCache() {
	cliAgentModelProbeMu.Lock()
	cliAgentModelProbeCache = map[string]cliAgentModelProbeEntry{}
	cliAgentModelProbeMu.Unlock()
}

// attachCLIAgentModelDiscovery adds the model list and effort scales to one
// provider's usage snapshot. Called by gatherCLIAgentUsage after the provider's
// parser ran, so it enriches whatever the parser produced (including the
// baseline entry for a parser-less agent) and never replaces a list the parser
// already established.
func attachCLIAgentModelDiscovery(agentID string, detected detectedCLIAgent, usage *cliAgentUsage, home string, now time.Time) {
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
		usage.ModelsExhaustive = authBoolPtr(true)
		return
	}
	discovery, ok := cachedCLIAgentModelDiscovery(agentID, detected, home, now)
	if !ok || len(discovery.Models) == 0 {
		return
	}
	usage.ModelDetails = boundedModelDetails(discovery.Models)
	usage.ModelsExhaustive = authBoolPtr(discovery.Exhaustive)
	if len(usage.Models) == 0 {
		ids := make([]string, 0, len(discovery.Models))
		for _, model := range discovery.Models {
			ids = append(ids, model.ID)
		}
		usage.Models = ids
	}
	if usage.Model == "" && discovery.DefaultModel != "" {
		usage.Model = discovery.DefaultModel
	}
}

func cachedCLIAgentModelDiscovery(agentID string, detected detectedCLIAgent, home string, now time.Time) (cliAgentModelDiscovery, bool) {
	key := strings.ToLower(agentID) + "\x00" + detected.Path + "\x00" + detected.Version
	cliAgentModelProbeMu.Lock()
	entry, cached := cliAgentModelProbeCache[key]
	cliAgentModelProbeMu.Unlock()
	if cached && now.Sub(entry.At) < cliAgentModelProbeTTL {
		return entry.Result, entry.OK
	}
	result, ok := discoverCLIAgentModels(agentID, detected, home)
	cliAgentModelProbeMu.Lock()
	cliAgentModelProbeCache[key] = cliAgentModelProbeEntry{At: now, Result: result, OK: ok}
	cliAgentModelProbeMu.Unlock()
	return result, ok
}

// discoverCLIAgentModels runs the one probe this agent supports. ok=false means
// the probe was inconclusive (no file, timeout, unrecognised output) and the
// caller must report nothing rather than an empty list.
func discoverCLIAgentModels(agentID string, detected detectedCLIAgent, home string) (cliAgentModelDiscovery, bool) {
	switch strings.ToLower(strings.TrimSpace(agentID)) {
	case "codex":
		return discoverCodexModels(home, detected.Version)
	case "antigravity":
		out, ok := cliAgentModelProbeRunner(detected.Path, sanitizeAntigravityEnv(os.Environ()), "models")
		if !ok {
			return cliAgentModelDiscovery{}, false
		}
		return parseAntigravityModelList(out)
	case "grok":
		out, ok := cliAgentModelProbeRunner(detected.Path, sanitizeGrokMaintenanceSmokeEnv(os.Environ()), "models")
		if !ok {
			return cliAgentModelDiscovery{}, false
		}
		return parseGrokModelList(out)
	case "claudecode":
		out, ok := cliAgentModelProbeRunner(detected.Path, os.Environ(), "--help")
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
func runCLIAgentModelProbe(executable string, env []string, args ...string) (string, bool) {
	if strings.TrimSpace(executable) == "" {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), cliAgentModelProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
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
	var cache codexModelsCacheFile
	if !readJSONFile(path, &cache) {
		return cliAgentModelDiscovery{}, false
	}
	out, ok := parseCodexModelsCache(cache)
	if !ok {
		return out, false
	}
	return reconcileCodexDiscovery(out, cache.ClientVersion, installedVersion, readCodexConfiguredModel(home)), true
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

// codexCacheMatchesInstalled compares the cache's client_version with the
// installed binary's `--version` output ("codex-cli 0.153.4"). An unknown
// version on either side is treated as a match: the cache is then only as
// trustworthy as before, and the configured-model rule still applies.
func codexCacheMatchesInstalled(cacheVersion, installedVersion string) bool {
	cacheVersion = strings.TrimSpace(cacheVersion)
	installed := strings.TrimSpace(installedVersion)
	if cacheVersion == "" || installed == "" {
		return true
	}
	fields := strings.Fields(installed)
	installed = fields[len(fields)-1]
	return strings.TrimPrefix(installed, "v") == strings.TrimPrefix(cacheVersion, "v")
}

// codexConfigModelLine matches a top-level `model = "…"` in config.toml.
var codexConfigModelLine = regexp.MustCompile(`(?m)^\s*model\s*=\s*"([^"]+)"`)

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
	// default this machine runs. Stop reading at the first table header.
	text := string(raw)
	if header := strings.Index(text, "\n["); header >= 0 {
		text = text[:header]
	}
	match := codexConfigModelLine.FindStringSubmatch(text)
	if match == nil {
		return ""
	}
	return strings.TrimSpace(match[1])
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
		if len(detail.Efforts) == 0 {
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
	out := cliAgentModelDiscovery{Exhaustive: true}
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
   Grok — `grok models`
   -------------------------------------------------------------------------- */

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

// boundedModelDetails truncates a list to the receipt cap, keeping the CLI's
// order so the first (newest / highest-priority) entries survive.
func boundedModelDetails(details []cliAgentModelDetail) []cliAgentModelDetail {
	if len(details) > cliUsageMaxModelsPerProvider {
		return details[:cliUsageMaxModelsPerProvider]
	}
	return details
}
