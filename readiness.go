package main

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Readiness evaluation for the Environment Setup / IT Assistant capability.
//
// The backend requests a read-only inspection via the __env_inspect__ demand
// command (see pubsub.go); the agent gathers full machine info, derives a
// friendly readiness verdict, and publishes an __env_inspect_result__ message.
// Nothing here mutates the workstation — inspection never requires user
// consent. Thresholds are product-defined defaults; they are intentionally
// conservative so a non-technical user is warned early rather than hitting a
// failed build halfway through setup.

// Readiness overall states. Mirrors the product vocabulary consumed by the
// frontend readiness card and terminal-service onboarding mapping
// (ready | ready_with_warnings | needs_setup | underpowered | blocked).
//
// ready_with_warnings = required tools present, only capacity/utilization
// advisories (e.g. low RAM warning). Backend mapReadinessStateToOnboarding
// treats this as onboarding "ready" so the device stays routable for work.
const (
	ReadinessReady             = "ready"
	ReadinessReadyWithWarnings = "ready_with_warnings"
	ReadinessNeedsSetup        = "needs_setup"
	ReadinessUnderpower        = "underpowered"
	ReadinessBlocked           = "blocked"
)

// Finding severities, ordered from least to most serious.
const (
	FindingInfo    = "info"
	FindingWarning = "warning"
	FindingBlocker = "blocker"
)

const ReadinessActionKindSoftwareUpdate = "software_update"

// Product-defined readiness thresholds (GB / cores). Not user-configurable at
// launch. A machine below the "blocker" disk floor cannot safely clone/build;
// below the "underpowered" RAM/CPU floor dev work will thrash.
//
// RAM comparisons use marketed tiers (8 / 16 GB) with ramMarketedSlackGB so
// OS-reported usable memory — which is almost always a bit under the stick
// size (16 GB → ~15.x, 32 GB → ~31.x because firmware reserves pages) — is
// classified against the marketed class rather than the raw float.
const (
	diskBlockerFreeGB  = 5.0  // below this, cloning/building will fail
	diskWarnFreeGB     = 20.0 // below this, warn before large clones/builds
	ramUnderpowerGB    = 8.0  // marketed tier: below this → underpowered
	ramWarnGB          = 16.0 // marketed tier: below this → advisory warning
	ramMarketedSlackGB = 1.0  // OS under-reports usable RAM vs marketed stick
	cpuUnderpowerCore  = 2    // below this, treat as underpowered
)

// capacityAdvisoryCodes are warning findings that do NOT require installable
// setup steps. They must not flip the overall state to needs_setup (that would
// map to onboarding setup_required and block work routing). See deriveReadinessState.
var capacityAdvisoryCodes = map[string]struct{}{
	"low_memory": {},
	"low_disk":   {},
}

// informationalSoftwareCodes are software findings the agent still reports —
// with their install action — but that no longer decide readiness. Whether a
// computer can do the workspace's work is the server's call now: it knows which
// repositories need Node and which coding agents are signed in
// (COMPUTER_SETUP_CHECKLIST_PLAN.md §6 "Enablement"). A hard-coded Node / npm /
// Codex gate made a computer needs_setup — unable to be enabled — for a
// workspace with no Node repository and a signed-in Claude Code. The agent's
// own verdict keeps only hardware and Git. Like the capacity advisories they
// cap the state at ready_with_warnings.
var informationalSoftwareCodes = map[string]struct{}{
	"missing_node":  {},
	"missing_npm":   {},
	"missing_codex": {},
	// Advisory like missing_node: the server offers the per-user fix and gates
	// npm steps on it, but a computer whose coding agent runs is not taken off
	// routing because PowerShell blocks npm.ps1.
	"powershell_scripts_blocked": {},
}

// marketedComparableRAM returns reported usable RAM plus slack so thresholds
// express marketed stick sizes. 15.7 GB usable → 16.7 comparable ≥ 16 (no warn).
func marketedComparableRAM(reportedGB float64) float64 {
	return reportedGB + ramMarketedSlackGB
}

// ReadinessFinding is a single user-friendly observation about the machine.
// Message is always plain language — never raw tool output — so it can be
// shown directly in chat.
type ReadinessFinding struct {
	Code     string `json:"code"`     // stable machine key, e.g. "low_disk", "missing_git"
	Severity string `json:"severity"` // info | warning | blocker
	Message  string `json:"message"`  // friendly, user-facing explanation
}

// ReadinessAction describes user-visible work discovered by inspection. It is
// intentionally presentation-only: terminal-service remains responsible for
// constructing and approving executable commands from the trusted report.
type ReadinessAction struct {
	ID                 string `json:"id"`
	FindingCode        string `json:"findingCode"`
	Kind               string `json:"kind"`
	Label              string `json:"label"`
	Instruction        string `json:"instruction"`
	ManualInstruction  string `json:"manualInstruction,omitempty"`
	RequiresUserAction bool   `json:"requiresUserAction"`
}

// ReadinessReport is the structured result of a workstation inspection. Specs
// carries the full MachineInfo so the frontend card can offer progressive
// disclosure ("Show details") without a second round trip.
type ReadinessReport struct {
	State       string             `json:"state"`
	Findings    []ReadinessFinding `json:"findings"`
	Actions     []ReadinessAction  `json:"actions"`
	Specs       *MachineInfo       `json:"specs,omitempty"`
	OS          string             `json:"os"`
	CollectedAt string             `json:"collectedAt"`
}

// maxFreeDiskGB returns the free space (GB) of the roomiest mounted volume.
// Dev repos and package caches can live on any drive, so the most permissive
// volume is the fair capacity signal; 0 when no disk info was gathered.
func maxFreeDiskGB(disks []diskEntry) float64 {
	maxFree := 0.0
	for _, d := range disks {
		if d.FreeGB > maxFree {
			maxFree = d.FreeGB
		}
	}
	return maxFree
}

// evaluateReadiness derives a readiness verdict from gathered machine info.
// Pure and deterministic (given info) so it is straightforward to unit-test.
// Ranking (most serious wins):
//
//	blocked > underpowered > needs_setup > ready_with_warnings > ready
func evaluateReadiness(info *MachineInfo) ReadinessReport {
	report := ReadinessReport{
		State:       ReadinessReady,
		Findings:    []ReadinessFinding{},
		Actions:     []ReadinessAction{},
		Specs:       info,
		OS:          runtime.GOOS,
		CollectedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if info == nil {
		report.State = ReadinessBlocked
		report.Findings = append(report.Findings, ReadinessFinding{
			Code:     "inspection_failed",
			Severity: FindingBlocker,
			Message:  inspectionFailedMessage,
		})
		return report
	}

	add := func(code, severity, message string, action *ReadinessAction) {
		report.Findings = append(report.Findings, ReadinessFinding{Code: code, Severity: severity, Message: message})
		if action != nil {
			report.Actions = append(report.Actions, *action)
		}
	}
	softwareAction := func(code, label, instruction, manualInstruction string) *ReadinessAction {
		return &ReadinessAction{
			ID: code, FindingCode: code, Kind: ReadinessActionKindSoftwareUpdate, Label: label,
			Instruction: instruction, ManualInstruction: manualInstruction, RequiresUserAction: true,
		}
	}

	// Disk capacity.
	freeGB := maxFreeDiskGB(info.Disk)
	if len(info.Disk) > 0 {
		switch {
		case freeGB < diskBlockerFreeGB:
			add("low_disk", FindingBlocker, fmt.Sprintf("Very low free disk space (%.0f GB). Free up space before installing tools or cloning repos.", freeGB), nil)
		case freeGB < diskWarnFreeGB:
			add("low_disk", FindingWarning, fmt.Sprintf("Low free disk space (%.0f GB). Cloning large repos or building may run out of room.", freeGB), nil)
		}
	}

	// Memory — compare against marketed tiers with slack so a "16 GB" stick
	// that reports 15.7 usable is not flagged, while a true ~12–14 GB class
	// still gets the heavy-workload advisory.
	if info.Memory != nil && info.Memory.TotalGB > 0 {
		comparable := marketedComparableRAM(info.Memory.TotalGB)
		switch {
		case comparable < ramUnderpowerGB:
			add("low_memory", FindingBlocker, fmt.Sprintf("This computer has %.0f GB of RAM, which is below the recommended minimum for development work.", info.Memory.TotalGB), nil)
		case comparable < ramWarnGB:
			add("low_memory", FindingWarning, fmt.Sprintf("This computer has %.0f GB of RAM. Heavier builds and multiple tools at once may be slow.", info.Memory.TotalGB), nil)
		}
	}

	// CPU cores.
	if info.CPU != nil && info.CPU.Cores > 0 && info.CPU.Cores < cpuUnderpowerCore {
		add("low_cpu", FindingBlocker, fmt.Sprintf("This computer has %d CPU core(s), which is below the recommended minimum for development work.", info.CPU.Cores), nil)
	}

	// Git tooling.
	if _, ok := info.Tools["git"]; !ok {
		add("missing_git", FindingWarning, "Git isn't installed yet. It's needed to clone and work with repositories.", softwareAction("missing_git", "Install Git", "Create a setup plan to install Git.", "Install Git using the recommended package for this operating system."))
	}

	// Node/npm runtime. Informational (informationalSoftwareCodes): the server
	// decides whether this workspace needs it.
	_, hasNode := info.Runtimes["node"]
	if !hasNode {
		add("missing_node", FindingWarning, "Node.js isn't installed yet. Many development workflows need it.", softwareAction("missing_node", "Install Node.js", "Create a setup plan to install Node.js and npm.", "Install the current Node.js LTS release, including npm."))
	} else if _, ok := info.PackageManagers["npm"]; !ok {
		// On distros where node and npm are split packages, node can be
		// present without npm. The Codex/setup workflows install via npm, so
		// surface this rather than reporting the machine ready and failing on
		// the first install step.
		add("missing_npm", FindingWarning, "npm isn't installed yet. It comes with Node.js on most systems and is needed to install CLI tools like Codex.", softwareAction("missing_npm", "Install npm", "Create a setup plan to install npm.", "Install npm for the detected Node.js runtime."))
	}

	// Codex CLI. Informational too: work readiness needs a signed-in coding
	// agent, which the server checks.
	if !cliAgentDetected(info, "codex") {
		add("missing_codex", FindingWarning, "Codex CLI isn't set up yet. AIExpedite can install and sign you in with your permission.", softwareAction("missing_codex", "Install Codex", "Create a setup plan to install Codex CLI.", "Install Codex CLI, then follow its sign-in prompt."))
	}

	// PowerShell execution policy (Windows only; nil elsewhere). A blocking
	// policy stops npm.ps1 and other local scripts, so installs would fail
	// inside npm. The per-user fix is offered only when it lifts the block.
	if info.PowerShell != nil && info.PowerShell.BlocksLocalScripts {
		message, fixable := powerShellPolicyFinding(info.PowerShell)
		var action *ReadinessAction
		if fixable {
			action = softwareAction("powershell_scripts_blocked", "Allow PowerShell scripts for your user", "Create a setup plan to allow PowerShell scripts for your user account (execution policy RemoteSigned, current user only).", info.PowerShell.manualCommand())
		}
		add("powershell_scripts_blocked", FindingWarning, message, action)
	}

	report.State = deriveReadinessState(report.Findings)
	return report
}

// cliAgentDetected reports whether a CLI agent id was detected on the machine.
func cliAgentDetected(info *MachineInfo, id string) bool {
	if info == nil {
		return false
	}
	if agent, ok := info.DetectedCliAgents[id]; ok && agent.Detected {
		return true
	}
	for _, a := range info.CliAgents {
		if strings.EqualFold(a.Provider, id) {
			return true
		}
	}
	return false
}

// deriveReadinessState folds the findings into a single overall state, most
// serious wins:
//
//	blocked > underpowered > needs_setup > ready_with_warnings > ready
//
// Capacity/utilization warnings (low_memory, low_disk) and the informational
// software findings (missing_node / missing_npm / missing_codex) are advisories
// only — they become ready_with_warnings so terminal-service keeps the device
// routable for work. Any other missing-but-installable tooling (Git) becomes
// needs_setup.
// Collapsing every warning into needs_setup previously blocked assignment for
// nominal 16 GB laptops that only carried a RAM advisory.
func deriveReadinessState(findings []ReadinessFinding) string {
	state := ReadinessReady
	for _, f := range findings {
		switch {
		case f.Severity == FindingBlocker && (f.Code == "low_memory" || f.Code == "low_cpu"):
			// Hardware can't be fixed by setup — underpowered unless something
			// harder (blocked) already applies.
			if state != ReadinessBlocked {
				state = ReadinessUnderpower
			}
		case f.Severity == FindingBlocker:
			state = ReadinessBlocked
		case f.Severity == FindingWarning:
			_, capacity := capacityAdvisoryCodes[f.Code]
			_, informational := informationalSoftwareCodes[f.Code]
			// A report from cached details says nothing new about the
			// machine; it only caps a ready verdict at ready_with_warnings.
			cachedDetails := f.Code == inspectionCachedFindingCode
			if capacity || informational || cachedDetails {
				// Advisory only — do not demote past ready_with_warnings, and
				// never override needs_setup / underpowered / blocked.
				if state == ReadinessReady {
					state = ReadinessReadyWithWarnings
				}
			} else {
				// Installable tooling gap — setup required.
				// May promote from ready or ready_with_warnings; never override
				// underpowered / blocked.
				if state == ReadinessReady || state == ReadinessReadyWithWarnings {
					state = ReadinessNeedsSetup
				}
			}
		}
	}
	return state
}

// inspection_failed messages. The code stays inspectionFailedFindingCode (the
// server and shared-constants key on it); the advice depends on why it failed.
const (
	inspectionFailedFindingCode = "inspection_failed"
	inspectionFailedMessage     = "We couldn't read this computer's details. Inspect again, and if it keeps failing, restart the Terminal agent."
	inspectionTimedOutMessage   = "Checking this computer's details took too long, so the inspection couldn't finish. Inspect again in a moment."
)

// inspection_cached: the advisory warning on a report built from the cached
// MachineInfo because the fresh gather missed its deadline (cachedReadiness).
const (
	inspectionCachedFindingCode    = "inspection_cached"
	inspectionCachedFindingMessage = "These details are from %s (%s ago) because the fresh check took too long. Inspect again for current details."
)

// inspectionCacheMaxAge is how old the cached MachineInfo may be for an
// inspection whose fresh gather missed its deadline to report it instead of
// inspection_failed. Hardware and installed tools rarely change within it; an
// older cache (or none) keeps the inspection_failed report.
const inspectionCacheMaxAge = 30 * time.Minute

// inspectionCacheFutureSkew is how far in the future a cached gather's stamp
// may be (clock adjusted since) and still count as recent.
const inspectionCacheFutureSkew = time.Minute

// Test seams for GatherReadinessOnly: the inspection's machine gather (with
// its CLI usage pass bounded, see inspectionCLIUsageBudget) and its fresh
// setupToolCatalog pass.
var (
	inspectionGather = func() *MachineInfo {
		return gatherMachineInfoBounded(inspectionCLIUsageBudget)
	}
	inspectionSetupToolPass = runSetupToolProbePass
)

// envInspectLogf writes one [env-inspect] line in the agent's log style.
func envInspectLogf(color, format string, args ...any) {
	fmt.Printf("%s[env-inspect] %s%s\n", color, fmt.Sprintf(format, args...), colorReset)
}

// timedOutReadiness is the inspection_failed report for an inspection whose
// gather missed its deadline with no recent cache to fall back on.
func timedOutReadiness() ReadinessReport {
	report := evaluateReadiness(nil)
	for i := range report.Findings {
		if report.Findings[i].Code == inspectionFailedFindingCode {
			report.Findings[i].Message = inspectionTimedOutMessage
		}
	}
	return report
}

// cachedReadiness evaluates the cached MachineInfo for an inspection whose
// fresh gather missed its deadline. ok is false when there is no cache or it
// is older than inspectionCacheMaxAge; collectedAt is the cache's stamp when
// it has one. The verdict is the cached data's own plus an advisory warning
// that it is not fresh, so it is never more permissive than the cached data:
// a ready machine reads ready_with_warnings, anything worse keeps its state.
func cachedReadiness(now time.Time) (report ReadinessReport, collectedAt time.Time, ok bool) {
	machineInfoMu.RLock()
	cached := machineInfoCache
	var snapshot MachineInfo
	if cached != nil {
		snapshot = *cached
	}
	machineInfoMu.RUnlock()
	if cached == nil {
		return ReadinessReport{}, time.Time{}, false
	}
	at, parsed := machineInfoCollectedAt(&snapshot)
	if !parsed {
		return ReadinessReport{}, time.Time{}, false
	}
	age := now.Sub(at)
	if age > inspectionCacheMaxAge || age < -inspectionCacheFutureSkew {
		return ReadinessReport{}, at, false
	}
	if age < 0 {
		age = 0
	}
	report = evaluateReadiness(&snapshot)
	report.Findings = append(report.Findings, ReadinessFinding{
		Code:     inspectionCachedFindingCode,
		Severity: FindingWarning,
		Message:  fmt.Sprintf(inspectionCachedFindingMessage, at.UTC().Format("2006-01-02 15:04 UTC"), minutesAgo(age)),
	})
	report.State = deriveReadinessState(report.Findings)
	// The report is as old as the data in it, so the server's report-staleness
	// checks (setup plans) see its real age.
	report.CollectedAt = snapshot.CollectedAt
	return report, at, true
}

// GatherReadinessOnly performs a full machine gather and returns the derived
// readiness report. Read-only; safe to run without user consent. Runs under a
// bounded context so a hung probe can't stall the inspection round trip. When
// the gather misses ctx, a recent cached MachineInfo is reported instead
// (cachedReadiness), and the gather, left to finish, refreshes the cache.
// Logs one [env-inspect] line with the duration and outcome.
func GatherReadinessOnly(ctx context.Context) ReadinessReport {
	if ctx == nil {
		ctx = context.Background()
	}
	started := time.Now()
	// A gather is heavyweight and, once started, runs to completion on its own
	// goroutine whatever happens to ctx below. An inspection that is already
	// over must not spend one: the gather would outlive the request, detect
	// whatever is on PATH by the time it finishes, and run every CLI's usage
	// parser — the Claude probe included — against the process-global state of
	// that later moment (the Windows CI flake in the settled-turn session tests).
	if ctx.Err() != nil {
		envInspectLogf(colorYellow, "inspection skipped: its deadline had already passed; reporting inspection_failed")
		return timedOutReadiness()
	}

	// Exactly one side owns the gathered info: the waiter below (the gather
	// finished in time) or the gather goroutine (the waiter gave up, so the
	// goroutine stores the late result in the cache). handoff makes that
	// decision atomic. The goroutine stays counted in
	// machineInfoGathersInFlight until the late store is done.
	var (
		handoff   sync.Mutex
		abandoned bool
	)
	done := make(chan *MachineInfo, 1)
	release := trackMachineInfoGather()
	go func() {
		defer release()
		info := inspectionGather()
		handoff.Lock()
		if !abandoned {
			done <- info
			handoff.Unlock()
			return
		}
		handoff.Unlock()
		elapsed := time.Since(started).Round(10 * time.Millisecond)
		switch {
		case info == nil:
		case storeMachineInfoIfNotOlder(info):
			envInspectLogf(colorYellow, "late gather finished after %s; cached details refreshed", elapsed)
		default:
			envInspectLogf(colorYellow, "late gather finished after %s; a newer gather is already cached", elapsed)
		}
	}()
	// A fresh setupToolCatalog pass runs beside the gather: the setup flow
	// re-inspects right after installing, and the cached pass may predate the
	// install. The pass is bounded by setupToolPassBudget (inside the 20 s
	// inspection budget), so waiting for it after the gather costs at most the
	// difference.
	toolsDone := make(chan map[string]string, 1)
	go func() {
		toolsDone <- inspectionSetupToolPass(ctx)
	}()

	var info *MachineInfo
	select {
	case info = <-done:
	case <-ctx.Done():
		handoff.Lock()
		select {
		case info = <-done:
			// It finished as the deadline passed: use it.
		default:
			abandoned = true
		}
		handoff.Unlock()
		if abandoned {
			return timedOutInspection(started)
		}
	}
	if info == nil {
		envInspectLogf(colorRed, "inspection gathered no details in %s; reporting inspection_failed", time.Since(started).Round(10*time.Millisecond))
		return evaluateReadiness(nil)
	}
	select {
	case results := <-toolsDone:
		withSetupToolResults(info, activeSetupToolCatalog(), results)
	case <-ctx.Done():
		// Keep the gather's merge of the previous pass.
	}
	// Keep the shared cache fresh so the next /auth/token POST reflects
	// what the user just saw in the readiness card.
	storeMachineInfoIfNotOlder(info)
	report := evaluateReadiness(info)
	envInspectLogf(colorGreen, "inspection ok in %s (state %s)", time.Since(started).Round(10*time.Millisecond), report.State)
	return report
}

// timedOutInspection is GatherReadinessOnly's report when the gather missed
// the deadline: the recent cache's verdict, or inspection_failed.
func timedOutInspection(started time.Time) ReadinessReport {
	elapsed := time.Since(started).Round(10 * time.Millisecond)
	report, at, ok := cachedReadiness(time.Now())
	switch {
	case ok:
		envInspectLogf(colorYellow, "inspection timed out after %s; reporting cached details from %s (state %s)", elapsed, at.UTC().Format(time.RFC3339), report.State)
		return report
	case !at.IsZero():
		envInspectLogf(colorRed, "inspection timed out after %s; cached details from %s are not recent; reporting inspection_failed", elapsed, at.UTC().Format(time.RFC3339))
	default:
		envInspectLogf(colorRed, "inspection timed out after %s; no cached details; reporting inspection_failed", elapsed)
	}
	return timedOutReadiness()
}

// minutesAgo renders a cache age for the inspection_cached message.
func minutesAgo(age time.Duration) string {
	minutes := int(age / time.Minute)
	switch {
	case minutes < 1:
		return "less than a minute"
	case minutes == 1:
		return "1 minute"
	default:
		return fmt.Sprintf("%d minutes", minutes)
	}
}
