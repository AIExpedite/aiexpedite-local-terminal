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
// frontend readiness card (ready | needs_setup | underpowered | blocked).
const (
	ReadinessReady      = "ready"
	ReadinessNeedsSetup = "needs_setup"
	ReadinessUnderpower = "underpowered"
	ReadinessBlocked    = "blocked"
)

// Finding severities, ordered from least to most serious.
const (
	FindingInfo    = "info"
	FindingWarning = "warning"
	FindingBlocker = "blocker"
)

// Product-defined readiness thresholds (GB / cores). Not user-configurable at
// launch. A machine below the "blocker" disk floor cannot safely clone/build;
// below the "underpowered" RAM/CPU floor dev work will thrash.
const (
	diskBlockerFreeGB = 5.0  // below this, cloning/building will fail
	diskWarnFreeGB    = 20.0 // below this, warn before large clones/builds
	ramUnderpowerGB   = 8.0  // below this, treat as underpowered for dev work
	ramWarnGB         = 16.0 // below this, warn about heavy workloads
	cpuUnderpowerCore = 2    // below this, treat as underpowered
)

// ReadinessFinding is a single user-friendly observation about the machine.
// Message is always plain language — never raw tool output — so it can be
// shown directly in chat.
type ReadinessFinding struct {
	Code     string `json:"code"`     // stable machine key, e.g. "low_disk", "missing_git"
	Severity string `json:"severity"` // info | warning | blocker
	Message  string `json:"message"`  // friendly, user-facing explanation
}

// ReadinessReport is the structured result of a workstation inspection. Specs
// carries the full MachineInfo so the frontend card can offer progressive
// disclosure ("Show details") without a second round trip.
type ReadinessReport struct {
	State       string             `json:"state"`
	Findings    []ReadinessFinding `json:"findings"`
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
// blocked > underpowered > needs_setup > ready, with the most serious finding
// winning the overall state.
func evaluateReadiness(info *MachineInfo) ReadinessReport {
	report := ReadinessReport{
		State:       ReadinessReady,
		Findings:    []ReadinessFinding{},
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

	add := func(code, severity, message string) {
		report.Findings = append(report.Findings, ReadinessFinding{Code: code, Severity: severity, Message: message})
	}

	// Disk capacity.
	freeGB := maxFreeDiskGB(info.Disk)
	if len(info.Disk) > 0 {
		switch {
		case freeGB < diskBlockerFreeGB:
			add("low_disk", FindingBlocker, fmt.Sprintf("Very low free disk space (%.0f GB). Free up space before installing tools or cloning repos.", freeGB))
		case freeGB < diskWarnFreeGB:
			add("low_disk", FindingWarning, fmt.Sprintf("Low free disk space (%.0f GB). Cloning large repos or building may run out of room.", freeGB))
		}
	}

	// Memory.
	if info.Memory != nil && info.Memory.TotalGB > 0 {
		switch {
		case info.Memory.TotalGB < ramUnderpowerGB:
			add("low_memory", FindingBlocker, fmt.Sprintf("This computer has %.0f GB of RAM, which is below the recommended minimum for development work.", info.Memory.TotalGB))
		case info.Memory.TotalGB < ramWarnGB:
			add("low_memory", FindingWarning, fmt.Sprintf("This computer has %.0f GB of RAM. Heavier builds and multiple tools at once may be slow.", info.Memory.TotalGB))
		}
	}

	// CPU cores.
	if info.CPU != nil && info.CPU.Cores > 0 && info.CPU.Cores < cpuUnderpowerCore {
		add("low_cpu", FindingBlocker, fmt.Sprintf("This computer has %d CPU core(s), which is below the recommended minimum for development work.", info.CPU.Cores))
	}

	// Git tooling.
	if _, ok := info.Tools["git"]; !ok {
		add("missing_git", FindingWarning, "Git isn't installed yet. It's needed to clone and work with repositories.")
	}

	// Node/npm runtime (needed for most AIExpedite dev workflows).
	if _, ok := info.Runtimes["node"]; !ok {
		add("missing_node", FindingWarning, "Node.js isn't installed yet. Many development workflows need it.")
	}

	// Codex CLI — the launch install workflow.
	if !cliAgentDetected(info, "codex") {
		add("missing_codex", FindingWarning, "Codex CLI isn't set up yet. AIExpedite can install and sign you in with your permission.")
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
// serious wins. Blocker disk/CPU/memory issues that make dev work impractical
// map to blocked/underpowered; missing-but-installable tooling maps to
// needs_setup; no findings means ready.
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
			if state == ReadinessReady {
				state = ReadinessNeedsSetup
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
