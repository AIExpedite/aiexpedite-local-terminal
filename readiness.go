package main

import (
	"context"
	"fmt"
	"runtime"
	"strings"
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
			Message:  "We couldn't read this computer's details. Try reconnecting the Terminal agent and inspecting again.",
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

	// Node/npm runtime (needed for most AIExpedite dev workflows).
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

	// Codex CLI — the launch install workflow.
	if !cliAgentDetected(info, "codex") {
		add("missing_codex", FindingWarning, "Codex CLI isn't set up yet. AIExpedite can install and sign you in with your permission.", softwareAction("missing_codex", "Install Codex", "Create a setup plan to install Codex CLI.", "Install Codex CLI, then follow its sign-in prompt."))
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
// Capacity/utilization warnings (low_memory, low_disk) are advisories only —
// they become ready_with_warnings so terminal-service keeps the device
// routable for work. Missing-but-installable tooling becomes needs_setup.
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
			if _, capacity := capacityAdvisoryCodes[f.Code]; capacity {
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

// GatherReadinessOnly performs a full machine gather and returns the derived
// readiness report. Read-only; safe to run without user consent. Runs under a
// bounded context so a hung probe can't stall the inspection round trip.
func GatherReadinessOnly(ctx context.Context) ReadinessReport {
	done := make(chan *MachineInfo, 1)
	go func() {
		done <- gatherMachineInfo()
	}()

	select {
	case info := <-done:
		// Keep the shared cache fresh so the next /auth/token POST reflects
		// what the user just saw in the readiness card.
		machineInfoMu.Lock()
		machineInfoCache = info
		machineInfoMu.Unlock()
		return evaluateReadiness(info)
	case <-ctx.Done():
		return evaluateReadiness(nil)
	}
}
