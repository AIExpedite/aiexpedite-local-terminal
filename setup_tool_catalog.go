// File: setup_tool_catalog.go
// -----------------------------------------------------------------------------
// Catalog-driven tool detection for the computer setup checklist.
//
// terminal-service returns `setupToolCatalog: [{ id, command, versionArgs,
// versionPattern? }]` in the /auth/token response (every enabled setupTools
// entry plus every enabled cliAgent's detect entry). It is stored exactly like
// cliAgentCatalog — persisted in the config, applied to a process-wide active
// copy — and probed as `command versionArgs…`, reported under
// MachineInfo.tools[id] as a raw output line (selectVersionLine).
//
// Why a separate cached pass rather than part of gatherMachineInfo:
//   - The first gather races the first /auth/token request; a first-time pairing
//     only starts onboarding from a request that carries machine info
//     (auth.go tokenSourceAwaitingMachineInfo). The catalog is database-driven
//     and grows with every tool the product adds — two dozen `--version`
//     children, several of them npm shims costing 1-3 s each on Windows — so
//     folding it into the gather would lengthen exactly the window that race
//     loses.
//   - On a fresh pairing the catalog only exists AFTER the first token response,
//     so it could not be part of the first gather anyway.
// So: the gather merges the LAST pass's results (no spawn); a pass runs after
// every periodic gather lands and whenever a token response changes the
// catalog; and an __env_inspect__ (the setup flow's source of truth) runs a
// fresh pass concurrently with its gather and merges it before replying, so a
// re-inspection after an install always sees the new tool.
//
// Entries that name a CLI agent the agent already detects (codex, claude, …) are
// never spawned here: gatherCLIAgents already probed them with the shim-aware
// probes, and their version is copied into tools[id].
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
)

// setupToolCatalogEntry is one probe from the /auth/token setupToolCatalog.
type setupToolCatalogEntry struct {
	ID             string   `json:"id"`
	Command        string   `json:"command"`
	VersionArgs    []string `json:"versionArgs,omitempty"`
	VersionPattern string   `json:"versionPattern,omitempty"`
}

const (
	maxSetupToolCatalogEntries = 64
	maxSetupToolPatternLength  = 256
	// setupToolPassBudget bounds a whole catalog pass. An __env_inspect__ allows
	// 20 s in total; the pass runs beside the gather, so it must finish inside.
	setupToolPassBudget = 15 * time.Second
)

var (
	setupToolIDPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	setupToolCommandPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
)

// setupToolVersionArgForms is the COMPLETE set of argument lists a catalog probe
// may run. The catalog comes from our own authenticated backend, but its only
// job is detection: a probe must never be able to run anything but a version
// query (`npm uninstall -g codex` is a perfectly valid bare command plus flags).
// An entry asking for any other form is skipped and logged, never executed.
// Covers every form the db-content setupTools / cliAgents catalog uses today
// (`--version`, `version`, `-version`) plus the other common spellings; a new
// form means an agent release, on purpose.
var setupToolVersionArgForms = [][]string{
	{"--version"},
	{"-v"},
	{"-V"},
	{"version"},
	{"-version"},
	{"--version", "--json"},
	{"version", "--short"},
}

func isAllowedVersionArgs(args []string) bool {
	for _, form := range setupToolVersionArgForms {
		if len(form) != len(args) {
			continue
		}
		match := true
		for i := range form {
			if form[i] != args[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// Defense in depth on top of the version-args allowlist: a shell or launcher is
// never a detected tool, so it is never probed.
var setupToolDeniedCommands = map[string]bool{
	"cmd": true, "powershell": true, "pwsh": true, "bash": true, "sh": true,
	"zsh": true, "fish": true, "dash": true, "ksh": true, "csh": true, "tcsh": true,
	"wsl": true, "env": true, "sudo": true, "doas": true, "osascript": true,
	"rundll32": true, "mshta": true, "wscript": true, "cscript": true, "start": true,
	"xargs": true, "nohup": true, "open": true,
}

// normalizeSetupToolCatalog validates and canonicalizes entries: ids unique
// (case-insensitive, first wins), command a bare program name (no path
// separators), versionArgs exactly one of setupToolVersionArgForms (default
// ["--version"]), an over-long pattern dropped. Invalid entries are skipped,
// never an error.
func normalizeSetupToolCatalog(entries []setupToolCatalogEntry) []setupToolCatalogEntry {
	out, _ := normalizeSetupToolCatalogReport(entries)
	return out
}

// normalizeSetupToolCatalogReport is normalizeSetupToolCatalog that also says
// why each skipped entry was skipped.
func normalizeSetupToolCatalogReport(entries []setupToolCatalogEntry) (out []setupToolCatalogEntry, skipped []string) {
	out = make([]setupToolCatalogEntry, 0, len(entries))
	seen := map[string]bool{}
	for _, raw := range entries {
		if len(out) >= maxSetupToolCatalogEntries {
			skipped = append(skipped, fmt.Sprintf("%q: catalog exceeds %d entries", raw.ID, maxSetupToolCatalogEntries))
			continue
		}
		id := strings.TrimSpace(raw.ID)
		command := strings.TrimSpace(raw.Command)
		if !setupToolIDPattern.MatchString(id) {
			skipped = append(skipped, fmt.Sprintf("%q: invalid id", raw.ID))
			continue
		}
		if !setupToolCommandPattern.MatchString(command) {
			skipped = append(skipped, fmt.Sprintf("%q: command %q is not a bare program name", id, command))
			continue
		}
		if setupToolDeniedCommands[strings.ToLower(commandBaseName(command))] {
			skipped = append(skipped, fmt.Sprintf("%q: command %q is never a detected tool", id, command))
			continue
		}
		key := strings.ToLower(id)
		if seen[key] {
			skipped = append(skipped, fmt.Sprintf("%q: duplicate id", id))
			continue
		}
		args := raw.VersionArgs
		if len(args) == 0 {
			args = []string{"--version"}
		}
		if !isAllowedVersionArgs(args) {
			skipped = append(skipped, fmt.Sprintf("%q: versionArgs %q is not a version query", id, args))
			continue
		}
		pattern := raw.VersionPattern
		if len(pattern) > maxSetupToolPatternLength {
			pattern = ""
		}
		seen[key] = true
		out = append(out, setupToolCatalogEntry{
			ID:             id,
			Command:        command,
			VersionArgs:    append([]string(nil), args...),
			VersionPattern: pattern,
		})
	}
	return out, skipped
}

func cloneSetupToolCatalog(entries []setupToolCatalogEntry) []setupToolCatalogEntry {
	if entries == nil {
		return nil
	}
	out := make([]setupToolCatalogEntry, len(entries))
	for i, e := range entries {
		out[i] = e
		out[i].VersionArgs = append([]string(nil), e.VersionArgs...)
	}
	return out
}

var (
	setupToolCatalogMu     sync.RWMutex
	activeSetupToolEntries []setupToolCatalogEntry
)

// SetSetupToolCatalog replaces the process-wide active catalog.
func SetSetupToolCatalog(entries []setupToolCatalogEntry) {
	normalized := normalizeSetupToolCatalog(entries)
	setupToolCatalogMu.Lock()
	activeSetupToolEntries = normalized
	setupToolCatalogMu.Unlock()
}

func activeSetupToolCatalog() []setupToolCatalogEntry {
	setupToolCatalogMu.RLock()
	defer setupToolCatalogMu.RUnlock()
	return cloneSetupToolCatalog(activeSetupToolEntries)
}

// UpdateSetupToolCatalog stores a catalog received from the backend on cfg and
// activates it; reports whether it differs from the stored one. Callers hold
// the config persistence lock (MutateAndSave).
func (cfg *Config) UpdateSetupToolCatalog(entries []setupToolCatalogEntry) bool {
	if entries == nil {
		return false
	}
	normalized := normalizeSetupToolCatalog(entries)
	changed := !reflect.DeepEqual(cfg.SetupToolCatalog, normalized)
	cfg.SetupToolCatalog = cloneSetupToolCatalog(normalized)
	SetSetupToolCatalog(normalized)
	return changed
}

// refreshSetupToolsAfterCatalogUpdate runs a catalog pass after the catalog
// changed. A variable so tests can observe it without spawning.
var refreshSetupToolsAfterCatalogUpdate = func() {
	go refreshSetupToolProbesInCache()
}

// persistSetupToolCatalogUpdate persists a catalog from a token response and,
// when it changed, re-probes. nil (the field was absent — an older backend)
// leaves the stored catalog alone.
func persistSetupToolCatalogUpdate(cfg *Config, entries []setupToolCatalogEntry, logScope, source string) bool {
	if cfg == nil || entries == nil {
		return false
	}
	if _, skipped := normalizeSetupToolCatalogReport(entries); len(skipped) > 0 {
		for _, reason := range skipped {
			fmt.Printf("%s[%s] setup-tool catalog entry skipped (never probed): %s%s\n", colorYellow, logScope, reason, colorReset)
		}
	}
	changed := false
	if err := cfg.MutateAndSave(ConfigPath(), func() {
		changed = cfg.UpdateSetupToolCatalog(entries)
	}); err != nil {
		fmt.Printf("%s[%s] Failed to persist setup-tool catalog from %s: %v%s\n", colorYellow, logScope, source, err, colorReset)
	}
	if !changed {
		return false
	}
	refreshSetupToolsAfterCatalogUpdate()
	return true
}

/* --------------------------------------------------------------------------
   Probe pass + cache
   -------------------------------------------------------------------------- */

// setupToolProbeCache is the last pass's results, keyed to the catalog they
// were probed for so a catalog change never reports a stale id.
var setupToolProbeCache struct {
	mu      sync.Mutex
	catalog []setupToolCatalogEntry
	results map[string]string
}

// setupToolPassMu serializes passes.
var setupToolPassMu sync.Mutex

// isCLIAgentToolID reports whether id names a CLI agent in the active CLI-agent
// catalog (case-insensitive). Those are probed by gatherCLIAgents.
func isCLIAgentToolID(id string) bool {
	for _, a := range activeCLIAgentCatalog() {
		if strings.EqualFold(a.ID, id) {
			return true
		}
	}
	return false
}

// probeSetupToolCatalog probes every entry except those skip excludes, with
// bounded parallelism and setupProbeTimeout each, and returns id → reported
// line for the ones that answered.
func probeSetupToolCatalog(ctx context.Context, entries []setupToolCatalogEntry, skip func(id string) bool, run setupProbeRunner) map[string]string {
	values := make([]string, len(entries))
	runProbesParallel(ctx, len(entries), setupProbeWorkers, func(ctx context.Context, i int) {
		e := entries[i]
		if skip != nil && skip(e.ID) {
			return
		}
		env := nodeCLIProbeEnv
		if strings.EqualFold(commandBaseName(e.Command), "terraform") {
			env = append(append([]string(nil), nodeCLIProbeEnv...), terraformProbeEnv...)
		}
		if out, ok := run(ctx, e.Command, e.VersionArgs, env, setupProbeTimeout); ok {
			values[i] = selectVersionLine(out, e.VersionPattern)
		}
	})
	results := map[string]string{}
	for i, e := range entries {
		if values[i] != "" {
			results[e.ID] = values[i]
		}
	}
	return results
}

// runSetupToolProbePass probes the active catalog and caches the results.
func runSetupToolProbePass(ctx context.Context) map[string]string {
	setupToolPassMu.Lock()
	defer setupToolPassMu.Unlock()

	entries := activeSetupToolCatalog()
	if len(entries) == 0 {
		storeSetupToolResults(entries, map[string]string{})
		return map[string]string{}
	}
	refreshCommandPath()
	pctx, cancel := context.WithTimeout(ctx, setupToolPassBudget)
	defer cancel()
	results := probeSetupToolCatalog(pctx, entries, isCLIAgentToolID, runSetupProbe)
	storeSetupToolResults(entries, results)
	return results
}

func storeSetupToolResults(catalog []setupToolCatalogEntry, results map[string]string) {
	setupToolProbeCache.mu.Lock()
	setupToolProbeCache.catalog = cloneSetupToolCatalog(catalog)
	setupToolProbeCache.results = copyStringMap(results)
	setupToolProbeCache.mu.Unlock()
}

// cachedSetupToolResults returns the last pass's results when they were probed
// for the currently active catalog, else nil.
func cachedSetupToolResults() map[string]string {
	current := activeSetupToolCatalog()
	setupToolProbeCache.mu.Lock()
	defer setupToolProbeCache.mu.Unlock()
	if setupToolProbeCache.results == nil || !reflect.DeepEqual(setupToolProbeCache.catalog, current) {
		return nil
	}
	return copyStringMap(setupToolProbeCache.results)
}

// withSetupToolResults recomputes info.Tools from the gather's own tool probes
// (baseTools) plus the catalog results and the catalog's CLI-agent entries. The
// base gather wins a key collision: its values are raw lines too, and older
// consumers already read them.
func withSetupToolResults(info *MachineInfo, catalog []setupToolCatalogEntry, results map[string]string) {
	if info == nil {
		return
	}
	tools := copyStringMap(info.baseTools)
	if tools == nil {
		tools = copyStringMap(info.Tools)
	}
	if tools == nil {
		tools = map[string]string{}
	}
	for _, e := range catalog {
		if _, ok := tools[e.ID]; ok {
			continue
		}
		if v := results[e.ID]; v != "" {
			tools[e.ID] = v
			continue
		}
		for key, a := range info.DetectedCliAgents {
			if strings.EqualFold(key, e.ID) && a.Detected && a.Version != "" {
				tools[e.ID] = truncateProbeLine(a.Version)
				break
			}
		}
	}
	info.Tools = tools
}

// refreshSetupToolProbesInCache runs a pass and folds it into the cached
// MachineInfo, so the next /auth/token request carries it.
func refreshSetupToolProbesInCache() {
	results := runSetupToolProbePass(context.Background())
	catalog := activeSetupToolCatalog()
	machineInfoMu.Lock()
	if machineInfoCache == nil {
		machineInfoMu.Unlock()
		return // the first gather merges the cached results itself
	}
	updated := *machineInfoCache
	withSetupToolResults(&updated, catalog, results)
	machineInfoCache = &updated
	machineInfoMu.Unlock()
	if onMachineInfoStored != nil {
		onMachineInfoStored()
	}
}

func copyStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
