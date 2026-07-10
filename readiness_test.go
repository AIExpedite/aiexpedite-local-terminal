package main

import (
	"context"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

// healthyMachine returns a MachineInfo that should evaluate to "ready".
func healthyMachine() *MachineInfo {
	return &MachineInfo{
		Architecture: "arm64",
		CPU:          &cpuInfo{Cores: 10, Threads: 10},
		Memory:       &memoryInfo{TotalGB: 32, AvailableGB: 20},
		Disk:         []diskEntry{{Drive: "/", SizeGB: 500, FreeGB: 200}},
		Runtimes:     map[string]string{"node": "20.0.0"},
		Tools:        map[string]string{"git": "2.42.0"},
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

func TestEvaluateReadiness_ReadyMachine(t *testing.T) {
	report := evaluateReadiness(healthyMachine())
	if report.State != ReadinessReady {
		t.Fatalf("expected ready, got %s (findings: %+v)", report.State, report.Findings)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("expected no findings for a healthy machine, got %+v", report.Findings)
	}
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
	m := healthyMachine()
	m.Disk = []diskEntry{{Drive: "/", SizeGB: 500, FreeGB: 12}}

	report := evaluateReadiness(m)
	if report.State != ReadinessNeedsSetup {
		t.Fatalf("expected needs_setup for a low-disk warning, got %s", report.State)
	}
	f := findFinding(report.Findings, "low_disk")
	if f == nil || f.Severity != FindingWarning {
		t.Fatalf("expected a warning low_disk finding, got %+v", f)
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
