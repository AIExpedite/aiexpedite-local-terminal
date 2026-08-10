package main

import (
	"context"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

// healthyMachine returns a MachineInfo that should evaluate to "ready".
func healthyMachine() *MachineInfo {
	return &MachineInfo{
		Architecture:    "arm64",
		CPU:             &cpuInfo{Cores: 10, Threads: 10},
		Memory:          &memoryInfo{TotalGB: 32, AvailableGB: 20},
		Disk:            []diskEntry{{Drive: "/", SizeGB: 500, FreeGB: 200}},
		Runtimes:        map[string]string{"node": "20.0.0"},
		PackageManagers: map[string]string{"npm": "10.0.0"},
		Tools:           map[string]string{"git": "2.42.0"},
		DetectedCliAgents: map[string]detectedCLIAgent{
			"codex": {Detected: true, Version: "1.0.0"},
		},
		DockerRunning: boolPtr(true),
	}
}

func findFinding(findings []ReadinessFinding, code string) *ReadinessFinding {
	for i := range findings {
		if findings[i].Code == code {
			return &findings[i]
		}
	}
	return nil
}

func findAction(actions []ReadinessAction, code string) *ReadinessAction {
	for i := range actions {
		if actions[i].FindingCode == code {
			return &actions[i]
		}
	}
	return nil
}

func requireNoReadinessActions(t *testing.T, report ReadinessReport) {
	t.Helper()
	if report.Actions == nil || len(report.Actions) != 0 {
		t.Fatalf("expected a non-nil empty actions array, got %+v", report.Actions)
	}
}

func TestEvaluateReadiness_ReadyMachine(t *testing.T) {
	report := evaluateReadiness(healthyMachine())
	if report.State != ReadinessReady {
		t.Fatalf("expected ready, got %s (findings: %+v)", report.State, report.Findings)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("expected no findings for a healthy machine, got %+v", report.Findings)
	}
	requireNoReadinessActions(t, report)
	if report.Specs == nil {
		t.Fatalf("readiness report should carry the specs for progressive disclosure")
	}
}

func TestEvaluateReadiness_NilMachineIsBlocked(t *testing.T) {
	report := evaluateReadiness(nil)
	if report.State != ReadinessBlocked {
		t.Fatalf("expected blocked for nil machine info, got %s", report.State)
	}
	if findFinding(report.Findings, "inspection_failed") == nil {
		t.Fatalf("expected an inspection_failed finding")
	}
	requireNoReadinessActions(t, report)
}

func TestEvaluateReadiness_MissingToolsNeedsSetup(t *testing.T) {
	m := healthyMachine()
	delete(m.Tools, "git")
	delete(m.Runtimes, "node")
	m.DetectedCliAgents = map[string]detectedCLIAgent{}

	report := evaluateReadiness(m)
	if report.State != ReadinessNeedsSetup {
		t.Fatalf("expected needs_setup, got %s", report.State)
	}
	for _, code := range []string{"missing_git", "missing_node", "missing_codex"} {
		f := findFinding(report.Findings, code)
		if f == nil {
			t.Fatalf("expected finding %s", code)
		}
		if f.Severity != FindingWarning {
			t.Fatalf("expected %s to be a warning, got %s", code, f.Severity)
		}
		a := findAction(report.Actions, code)
		if a == nil || a.ID != code || a.Kind != ReadinessActionKindSoftwareUpdate || !a.RequiresUserAction {
			t.Fatalf("expected a paired software action for %s, got %+v", code, a)
		}
	}
	if len(report.Actions) != 3 {
		t.Fatalf("expected exactly three ordered software actions, got %+v", report.Actions)
	}
	for i, code := range []string{"missing_git", "missing_node", "missing_codex"} {
		if report.Actions[i].ID != code {
			t.Fatalf("expected action %d to be %s, got %+v", i, code, report.Actions)
		}
	}
}

func TestEvaluateReadiness_NodeWithoutNpmWarns(t *testing.T) {
	// Split-package distros can have node present but npm absent; the
	// npm-based setup steps would then fail on a machine reported "ready".
	m := healthyMachine()
	delete(m.PackageManagers, "npm")

	report := evaluateReadiness(m)
	if report.State != ReadinessNeedsSetup {
		t.Fatalf("expected needs_setup when npm is missing, got %s (findings: %+v)", report.State, report.Findings)
	}
	f := findFinding(report.Findings, "missing_npm")
	if f == nil {
		t.Fatalf("expected a missing_npm finding when node is present but npm is not")
	}
	if f.Severity != FindingWarning {
		t.Fatalf("expected missing_npm to be a warning, got %s", f.Severity)
	}
	if a := findAction(report.Actions, "missing_npm"); a == nil || a.Label != "Install npm" {
		t.Fatalf("expected paired npm action, got %+v", a)
	}
	// When node itself is missing we warn on node, not npm (npm-comes-with-node).
	if findFinding(report.Findings, "missing_node") != nil {
		t.Fatalf("did not expect missing_node when node is installed")
	}
}

func TestEvaluateReadiness_MissingNodeDoesNotAlsoWarnNpm(t *testing.T) {
	m := healthyMachine()
	delete(m.Runtimes, "node")
	delete(m.PackageManagers, "npm")

	report := evaluateReadiness(m)
	if findFinding(report.Findings, "missing_node") == nil {
		t.Fatalf("expected missing_node finding")
	}
	// npm warning is redundant when node is missing — installing node brings npm.
	if findFinding(report.Findings, "missing_npm") != nil {
		t.Fatalf("did not expect a separate missing_npm finding when node is absent")
	}
}

func TestEvaluateReadiness_LowRAMIsUnderpowered(t *testing.T) {
	m := healthyMachine()
	m.Memory = &memoryInfo{TotalGB: 4}

	report := evaluateReadiness(m)
	if report.State != ReadinessUnderpower {
		t.Fatalf("expected underpowered for 4GB RAM, got %s", report.State)
	}
	f := findFinding(report.Findings, "low_memory")
	if f == nil || f.Severity != FindingBlocker {
		t.Fatalf("expected a blocker low_memory finding, got %+v", f)
	}
	requireNoReadinessActions(t, report)
}

func TestEvaluateReadiness_LowDiskIsBlocked(t *testing.T) {
	m := healthyMachine()
	m.Disk = []diskEntry{{Drive: "/", SizeGB: 500, FreeGB: 2}}

	report := evaluateReadiness(m)
	if report.State != ReadinessBlocked {
		t.Fatalf("expected blocked for 2GB free disk, got %s", report.State)
	}
	f := findFinding(report.Findings, "low_disk")
	if f == nil || f.Severity != FindingBlocker {
		t.Fatalf("expected a blocker low_disk finding, got %+v", f)
	}
	requireNoReadinessActions(t, report)
}

func TestEvaluateReadiness_LowCPUIsUnderpoweredWithoutActions(t *testing.T) {
	m := healthyMachine()
	m.CPU = &cpuInfo{Cores: 1, Threads: 1}

	report := evaluateReadiness(m)
	if report.State != ReadinessUnderpower {
		t.Fatalf("expected underpowered for one CPU core, got %s", report.State)
	}
	if f := findFinding(report.Findings, "low_cpu"); f == nil || f.Severity != FindingBlocker {
		t.Fatalf("expected a blocker low_cpu finding, got %+v", f)
	}
	requireNoReadinessActions(t, report)
}

func TestEvaluateReadiness_BlockedBeatsUnderpowered(t *testing.T) {
	// A machine that is both low on disk (blocker) and low on RAM
	// (underpowered) must report the more serious "blocked" state.
	m := healthyMachine()
	m.Disk = []diskEntry{{Drive: "/", SizeGB: 500, FreeGB: 1}}
	m.Memory = &memoryInfo{TotalGB: 4}

	report := evaluateReadiness(m)
	if report.State != ReadinessBlocked {
		t.Fatalf("expected blocked to win over underpowered, got %s", report.State)
	}
}

func TestEvaluateReadiness_LowDiskWarningOnly(t *testing.T) {
	// Capacity advisories must NOT force needs_setup — that maps to onboarding
	// setup_required and blocks work routing. ready_with_warnings keeps the
	// finding visible while leaving the device assignable.
	m := healthyMachine()
	m.Disk = []diskEntry{{Drive: "/", SizeGB: 500, FreeGB: 12}}

	report := evaluateReadiness(m)
	if report.State != ReadinessReadyWithWarnings {
		t.Fatalf("expected ready_with_warnings for a low-disk warning, got %s", report.State)
	}
	f := findFinding(report.Findings, "low_disk")
	if f == nil || f.Severity != FindingWarning {
		t.Fatalf("expected a warning low_disk finding, got %+v", f)
	}
	requireNoReadinessActions(t, report)
}

func TestEvaluateReadiness_LowRAMWarningOnly(t *testing.T) {
	// True mid-range RAM (below the 16 GB marketed class even with slack)
	// is an advisory, not a setup requirement.
	m := healthyMachine()
	m.Memory = &memoryInfo{TotalGB: 12.0, AvailableGB: 6.0}

	report := evaluateReadiness(m)
	if report.State != ReadinessReadyWithWarnings {
		t.Fatalf("expected ready_with_warnings for 12 GB RAM advisory, got %s (findings: %+v)", report.State, report.Findings)
	}
	f := findFinding(report.Findings, "low_memory")
	if f == nil || f.Severity != FindingWarning {
		t.Fatalf("expected a warning low_memory finding, got %+v", f)
	}
	requireNoReadinessActions(t, report)
}

func TestEvaluateReadiness_Nominal16GBReportsAs15xIsReady(t *testing.T) {
	// OS-reported usable RAM for a marketed 16 GB stick is typically ~15.x
	// (firmware reserved). With marketed-size slack this must not warn, or
	// every 16 GB laptop looks under-spec'd.
	m := healthyMachine()
	m.Memory = &memoryInfo{TotalGB: 15.7, AvailableGB: 5.8}

	report := evaluateReadiness(m)
	if report.State != ReadinessReady {
		t.Fatalf("expected ready for nominal 16 GB (reported 15.7), got %s (findings: %+v)", report.State, report.Findings)
	}
	if findFinding(report.Findings, "low_memory") != nil {
		t.Fatalf("did not expect low_memory finding for reported 15.7 GB (marketed 16 GB class)")
	}
}

func TestEvaluateReadiness_Nominal32GBReportsAs31xIsReady(t *testing.T) {
	m := healthyMachine()
	m.Memory = &memoryInfo{TotalGB: 31.2, AvailableGB: 20.0}

	report := evaluateReadiness(m)
	if report.State != ReadinessReady {
		t.Fatalf("expected ready for nominal 32 GB (reported 31.2), got %s (findings: %+v)", report.State, report.Findings)
	}
	if findFinding(report.Findings, "low_memory") != nil {
		t.Fatalf("did not expect low_memory finding for reported 31.2 GB")
	}
}

func TestEvaluateReadiness_CapacityWarningPlusMissingToolNeedsSetup(t *testing.T) {
	// Installable gaps outrank capacity advisories: the machine still needs
	// setup, even if it also has a low-disk warning.
	m := healthyMachine()
	m.Disk = []diskEntry{{Drive: "/", SizeGB: 500, FreeGB: 12}}
	delete(m.Tools, "git")

	report := evaluateReadiness(m)
	if report.State != ReadinessNeedsSetup {
		t.Fatalf("expected needs_setup when a tool is missing (capacity advisory present), got %s", report.State)
	}
	if findFinding(report.Findings, "low_disk") == nil {
		t.Fatalf("expected low_disk finding to still be present")
	}
	if findFinding(report.Findings, "missing_git") == nil {
		t.Fatalf("expected missing_git finding")
	}
	if len(report.Actions) != 1 || report.Actions[0].FindingCode != "missing_git" {
		t.Fatalf("expected only the missing-tool action, got %+v", report.Actions)
	}
}

func TestEvaluateReadiness_RefreshDropsCompletedActions(t *testing.T) {
	missing := healthyMachine()
	delete(missing.Tools, "git")
	first := evaluateReadiness(missing)
	if len(first.Actions) != 1 {
		t.Fatalf("expected initial missing Git action, got %+v", first.Actions)
	}

	refreshed := evaluateReadiness(healthyMachine())
	if refreshed.State != ReadinessReady || len(refreshed.Actions) != 0 {
		t.Fatalf("expected refreshed machine to have no residual actions, got %+v", refreshed)
	}
}

func TestEvaluateReadiness_Nominal8GBReportsAs7xNotUnderpowered(t *testing.T) {
	// Same marketed-size slack applies at the underpowered floor: ~7.x usable
	// is a marketed 8 GB stick, not underpowered (it may still warn if under
	// the 16 GB class).
	m := healthyMachine()
	m.Memory = &memoryInfo{TotalGB: 7.5, AvailableGB: 3.0}

	report := evaluateReadiness(m)
	if report.State == ReadinessUnderpower {
		t.Fatalf("expected marketed 8 GB (reported 7.5) not underpowered, got %s", report.State)
	}
	// 7.5 + 1.0 slack = 8.5, still below the 16 GB warn tier → advisory.
	if report.State != ReadinessReadyWithWarnings {
		t.Fatalf("expected ready_with_warnings for ~8 GB marketed class, got %s (findings: %+v)", report.State, report.Findings)
	}
}

func TestMaxFreeDiskGB_PicksRoomiestVolume(t *testing.T) {
	disks := []diskEntry{
		{Drive: "C:", FreeGB: 5},
		{Drive: "D:", FreeGB: 120},
		{Drive: "E:", FreeGB: 30},
	}
	if got := maxFreeDiskGB(disks); got != 120 {
		t.Fatalf("expected 120, got %v", got)
	}
	if got := maxFreeDiskGB(nil); got != 0 {
		t.Fatalf("expected 0 for no disks, got %v", got)
	}
}

func TestCliAgentDetected_MatchesEitherShape(t *testing.T) {
	viaMap := &MachineInfo{
		DetectedCliAgents: map[string]detectedCLIAgent{"codex": {Detected: true}},
	}
	if !cliAgentDetected(viaMap, "codex") {
		t.Fatalf("expected codex detected via DetectedCliAgents map")
	}
	viaSlice := &MachineInfo{CliAgents: []cliAgentUsage{{Provider: "Codex"}}}
	if !cliAgentDetected(viaSlice, "codex") {
		t.Fatalf("expected codex detected via CliAgents slice (case-insensitive)")
	}
	if cliAgentDetected(&MachineInfo{}, "codex") {
		t.Fatalf("expected codex NOT detected on empty machine")
	}
}

func TestGatherReadinessOnly_CanceledContextIsBlocked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report := GatherReadinessOnly(ctx)
	// A canceled context returns the nil-machine (blocked) report rather than
	// hanging on the gather.
	if report.State != ReadinessBlocked {
		t.Fatalf("expected blocked on canceled context, got %s", report.State)
	}
}
