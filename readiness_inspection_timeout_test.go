package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// withInspectionSeams swaps GatherReadinessOnly's gather and setup-tool pass
// for the test, and drains whatever gather the test left running before the
// seams are restored.
func withInspectionSeams(t *testing.T, gather func() *MachineInfo, pass func(context.Context) map[string]string) {
	t.Helper()
	savedGather, savedPass := inspectionGather, inspectionSetupToolPass
	inspectionGather = gather
	inspectionSetupToolPass = pass
	t.Cleanup(func() {
		if !drainMachineInfoGathers(5 * time.Second) {
			t.Errorf("an inspection gather started by this test is still running")
		}
		inspectionGather, inspectionSetupToolPass = savedGather, savedPass
	})
}

func noSetupToolPass(context.Context) map[string]string { return nil }

// blockingGather returns a gather that waits for release and then returns info.
func blockingGather(release <-chan struct{}, info *MachineInfo) func() *MachineInfo {
	return func() *MachineInfo {
		<-release
		return info
	}
}

func cacheMachineInfo(info *MachineInfo) {
	machineInfoMu.Lock()
	machineInfoCache = info
	machineInfoMu.Unlock()
}

func cachedMachineInfo() *MachineInfo {
	machineInfoMu.RLock()
	defer machineInfoMu.RUnlock()
	return machineInfoCache
}

func stampedMachine(info *MachineInfo, at time.Time) *MachineInfo {
	info.CollectedAt = at.UTC().Format(time.RFC3339)
	return info
}

func inspectWithin(d time.Duration) ReadinessReport {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return GatherReadinessOnly(ctx)
}

func TestGatherReadinessOnly_FastGatherUnchanged(t *testing.T) {
	isolateMachineInfo(t)
	fresh := stampedMachine(healthyMachine(), time.Now())
	withInspectionSeams(t, func() *MachineInfo { return fresh }, noSetupToolPass)

	report := inspectWithin(5 * time.Second)
	if report.State != ReadinessReady {
		t.Fatalf("expected ready, got %s (%+v)", report.State, report.Findings)
	}
	if findFinding(report.Findings, inspectionCachedFindingCode) != nil {
		t.Fatalf("a gather that finished in time must not be reported as cached: %+v", report.Findings)
	}
	if report.Specs != fresh {
		t.Fatalf("the report must carry the fresh gather")
	}
	if cachedMachineInfo() != fresh {
		t.Fatalf("a successful inspection must refresh the cache")
	}
}

func TestGatherReadinessOnly_TimeoutWithFreshCacheReportsCachedVerdict(t *testing.T) {
	isolateMachineInfo(t)
	cachedAt := time.Now().Add(-10 * time.Minute)
	cacheMachineInfo(stampedMachine(healthyMachine(), cachedAt))

	release := make(chan struct{})
	late := stampedMachine(healthyMachine(), time.Now())
	withInspectionSeams(t, blockingGather(release, late), noSetupToolPass)

	report := inspectWithin(50 * time.Millisecond)
	if report.State != ReadinessReadyWithWarnings {
		t.Fatalf("a ready cache must read ready_with_warnings, got %s (%+v)", report.State, report.Findings)
	}
	if f := findFinding(report.Findings, inspectionFailedFindingCode); f != nil {
		t.Fatalf("a recent cache must not be reported as inspection_failed: %+v", f)
	}
	f := findFinding(report.Findings, inspectionCachedFindingCode)
	if f == nil || f.Severity != FindingWarning {
		t.Fatalf("expected an inspection_cached warning, got %+v", report.Findings)
	}
	if !strings.Contains(f.Message, cachedAt.UTC().Format("2006-01-02 15:04 UTC")) || !strings.Contains(f.Message, "took too long") || !strings.Contains(f.Message, "(10 minutes ago)") {
		t.Fatalf("the warning must say when the details are from and why: %q", f.Message)
	}
	if report.CollectedAt != cachedAt.UTC().Format(time.RFC3339) {
		t.Fatalf("a cached report must carry the cached collection time, got %q", report.CollectedAt)
	}
	if report.Specs == nil || report.Specs.Tools["git"] == "" {
		t.Fatalf("a cached report must carry the cached specs")
	}

	// The abandoned gather, once it finishes, refreshes the cache.
	close(release)
	if !drainMachineInfoGathers(5 * time.Second) {
		t.Fatal("the abandoned gather did not finish")
	}
	if cachedMachineInfo() != late {
		t.Fatalf("the late gather must refresh the cache")
	}
}

func TestGatherReadinessOnly_CachedVerdictNeverMorePermissive(t *testing.T) {
	cases := map[string]struct {
		mutate func(*MachineInfo)
		want   string
	}{
		"missing git stays needs_setup": {func(m *MachineInfo) { delete(m.Tools, "git") }, ReadinessNeedsSetup},
		"no free disk stays blocked": {func(m *MachineInfo) {
			m.Disk = []diskEntry{{Drive: "C:", SizeGB: 500, FreeGB: 1}}
		}, ReadinessBlocked},
		"low RAM stays underpowered":   {func(m *MachineInfo) { m.Memory = &memoryInfo{TotalGB: 4} }, ReadinessUnderpower},
		"advisory stays with warnings": {func(m *MachineInfo) { delete(m.Runtimes, "node") }, ReadinessReadyWithWarnings},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			isolateMachineInfo(t)
			info := healthyMachine()
			tc.mutate(info)
			cacheMachineInfo(stampedMachine(info, time.Now().Add(-time.Minute)))
			release := make(chan struct{})
			withInspectionSeams(t, blockingGather(release, nil), noSetupToolPass)
			t.Cleanup(func() { close(release) })

			report := inspectWithin(30 * time.Millisecond)
			if report.State != tc.want {
				t.Fatalf("cached verdict: want %s, got %s (%+v)", tc.want, report.State, report.Findings)
			}
			if direct := evaluateReadiness(info); direct.State != ReadinessReady && direct.State != report.State {
				t.Fatalf("cached report %s differs from the cached data's own verdict %s", report.State, direct.State)
			}
			if findFinding(report.Findings, inspectionCachedFindingCode) == nil {
				t.Fatalf("expected the inspection_cached warning, got %+v", report.Findings)
			}
		})
	}
}

func TestGatherReadinessOnly_TimeoutWithStaleOrNoCacheIsInspectionFailed(t *testing.T) {
	for name, cached := range map[string]*MachineInfo{
		"no cache":        nil,
		"stale cache":     stampedMachine(healthyMachine(), time.Now().Add(-inspectionCacheMaxAge-time.Minute)),
		"unstamped cache": healthyMachine(),
		"future cache":    stampedMachine(healthyMachine(), time.Now().Add(time.Hour)),
	} {
		t.Run(name, func(t *testing.T) {
			isolateMachineInfo(t)
			cacheMachineInfo(cached)
			release := make(chan struct{})
			withInspectionSeams(t, blockingGather(release, nil), noSetupToolPass)
			t.Cleanup(func() { close(release) })

			report := inspectWithin(30 * time.Millisecond)
			if report.State != ReadinessBlocked {
				t.Fatalf("expected blocked, got %s (%+v)", report.State, report.Findings)
			}
			f := findFinding(report.Findings, inspectionFailedFindingCode)
			if f == nil {
				t.Fatalf("expected inspection_failed, got %+v", report.Findings)
			}
			if f.Message != inspectionTimedOutMessage || strings.Contains(f.Message, "reconnect") {
				t.Fatalf("a timeout must say the check took too long, got %q", f.Message)
			}
			if report.Specs != nil {
				t.Fatalf("an inspection_failed report carries no specs")
			}
		})
	}
}

func TestGatherReadinessOnly_LateGatherDoesNotRollBackNewerCache(t *testing.T) {
	isolateMachineInfo(t)
	release := make(chan struct{})
	older := stampedMachine(healthyMachine(), time.Now().Add(-time.Hour))
	withInspectionSeams(t, blockingGather(release, older), noSetupToolPass)

	_ = inspectWithin(30 * time.Millisecond)
	newer := stampedMachine(healthyMachine(), time.Now())
	cacheMachineInfo(newer)
	close(release)
	if !drainMachineInfoGathers(5 * time.Second) {
		t.Fatal("the abandoned gather did not finish")
	}
	if cachedMachineInfo() != newer {
		t.Fatalf("a late gather that started earlier must not replace a newer cache")
	}
}

func TestEvaluateReadiness_NilMessageNoLongerSaysReconnect(t *testing.T) {
	f := findFinding(evaluateReadiness(nil).Findings, inspectionFailedFindingCode)
	if f == nil || strings.Contains(strings.ToLower(f.Message), "reconnect") {
		t.Fatalf("unexpected inspection_failed finding %+v", f)
	}
}

func TestAwaitCLIAgentUsage_BudgetFallsBackToCachedUsage(t *testing.T) {
	isolateMachineInfo(t)
	cached := healthyMachine()
	cached.CliAgents = []cliAgentUsage{{CliAgentID: "codex", Provider: "codex", Version: "0.1.0", Account: "me@example.com"}}
	cacheMachineInfo(cached)

	detected := map[string]detectedCLIAgent{
		"codex":      {Detected: true, Version: "0.2.0", Path: "/bin/codex", Name: "Codex"},
		"claudeCode": {Detected: true, Version: "2.0.0", Path: "/bin/claude", Name: "Claude Code"},
	}
	never := make(chan []cliAgentUsage) // a usage pass that never finishes in time
	started := time.Now()
	got := awaitCLIAgentUsage(never, started, 20*time.Millisecond, detected, time.Now())
	if waited := time.Since(started); waited > 2*time.Second {
		t.Fatalf("the budget was not honoured: waited %s", waited)
	}
	byID := map[string]cliAgentUsage{}
	for _, u := range got {
		byID[u.CliAgentID] = u
	}
	if len(byID) != 2 {
		t.Fatalf("expected one entry per detected CLI, got %+v", got)
	}
	if u := byID["codex"]; u.Account != "me@example.com" || u.Version != "0.2.0" || u.Path != "/bin/codex" {
		t.Fatalf("codex must keep its last known usage with the detected version/path, got %+v", u)
	}
	if u := byID["claudeCode"]; u.Version != "2.0.0" || u.Account != "" || u.AccountFingerprint == "" {
		t.Fatalf("an uncached CLI gets the baseline entry, got %+v", u)
	}

	// No budget: waits for the pass.
	ch := make(chan []cliAgentUsage, 1)
	want := []cliAgentUsage{{CliAgentID: "codex", Provider: "codex", Account: "fresh"}}
	ch <- want
	if got := awaitCLIAgentUsage(ch, time.Now(), 0, detected, time.Now()); len(got) != 1 || got[0].Account != "fresh" {
		t.Fatalf("an unbounded wait must return the pass's own result, got %+v", got)
	}
}

// sequencedMachine is a gather result as gatherMachineInfoBounded makes it:
// stamped at second resolution, ordered by its start sequence.
func sequencedMachine(at time.Time) *MachineInfo {
	info := stampedMachine(healthyMachine(), at)
	info.gatherSeq = nextMachineInfoGatherSeq()
	return info
}

func TestStoreMachineInfoIfNotOlder_SameSecondOrdersByStartSequence(t *testing.T) {
	isolateMachineInfo(t)
	second := time.Now().Truncate(time.Second)
	earlier := sequencedMachine(second) // started first ...
	later := sequencedMachine(second)   // ... in the same second
	if earlier.CollectedAt != later.CollectedAt {
		t.Fatalf("fixture: both gathers must carry the same second-resolution stamp")
	}

	cacheMachineInfo(later)
	if storeMachineInfoIfNotOlder(earlier) {
		t.Fatal("a gather that started earlier in the same second must not replace the newer cache")
	}
	if cachedMachineInfo() != later {
		t.Fatal("the cache was rolled back to the earlier gather")
	}

	// The other order still stores.
	cacheMachineInfo(earlier)
	if !storeMachineInfoIfNotOlder(later) || cachedMachineInfo() != later {
		t.Fatal("a gather that started later must replace the cache")
	}
}

func TestGatherReadinessOnly_LateGatherSameSecondDoesNotRollBackNewerCache(t *testing.T) {
	isolateMachineInfo(t)
	second := time.Now().Truncate(time.Second)
	release := make(chan struct{})
	late := sequencedMachine(second) // the inspection's gather starts first
	withInspectionSeams(t, blockingGather(release, late), noSetupToolPass)

	_ = inspectWithin(30 * time.Millisecond)
	newer := sequencedMachine(second) // a refresh starts later in the same second and lands first
	cacheMachineInfo(newer)
	close(release)
	if !drainMachineInfoGathers(5 * time.Second) {
		t.Fatal("the abandoned gather did not finish")
	}
	if cachedMachineInfo() != newer {
		t.Fatal("a late gather started earlier in the same second must not replace the newer cache")
	}
}
