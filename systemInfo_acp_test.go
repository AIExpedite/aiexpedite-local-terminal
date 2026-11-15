package main

import (
	"reflect"
	"testing"
	"time"
)

func TestACPDriversListOnlyAgentsWhoseCLIQualifies(t *testing.T) {
	cases := []struct {
		name string
		bins map[string]acpDriverBinary
		want []string
	}{
		{"nothing installed", nil, []string{}},
		{"opencode 2.2.7", map[string]acpDriverBinary{opencodeCliAgentID: installedACPBinary("/b/opencode", "2.2.7")}, []string{"opencode"}},
		{"opencode 2.2.6", map[string]acpDriverBinary{opencodeCliAgentID: installedACPBinary("/b/opencode", "2.2.6")}, []string{}},
		{"opencode 1.19.5", map[string]acpDriverBinary{opencodeCliAgentID: installedACPBinary("/b/opencode", "1.19.5")}, []string{}},
		{"opencode npm launcher", map[string]acpDriverBinary{opencodeCliAgentID: {path: `C:\n\opencode.cmd`, found: true, shim: true, version: "2.3.0"}}, []string{}},
		{"agy 1.5.4", map[string]acpDriverBinary{antigravityCliAgentID: installedACPBinary("/b/agy", "1.5.4")}, []string{"antigravity"}},
		{"agy 1.5.3", map[string]acpDriverBinary{antigravityCliAgentID: installedACPBinary("/b/agy", "1.5.3")}, []string{}},
		{"both", map[string]acpDriverBinary{
			opencodeCliAgentID:    installedACPBinary("/b/opencode", "2.3.0"),
			antigravityCliAgentID: installedACPBinary("/b/agy", "1.5.9"),
		}, []string{"opencode", "antigravity"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pinACPDriverBinaries(t, c.bins)
			got := newACPDriversCapability()
			if got.Protocol != 1 || got.Agents == nil || !reflect.DeepEqual(got.Agents, c.want) {
				t.Fatalf("acpDrivers = %+v, want protocol 1 agents %v", got, c.want)
			}
			if !reflect.DeepEqual(newMachineCapabilities().ACPDrivers, got) {
				t.Fatal("newMachineCapabilities does not report the drivers")
			}
		})
	}
}

// The owner's decision of 2026-11-14 includes Antigravity, under its
// mitigation; turning the constant off is the device-side way out.
func TestAntigravityACPDriverIsIncludedByTheOwnersDecision(t *testing.T) {
	if !antigravityACPDriverIncluded {
		t.Fatal("antigravityACPDriverIncluded is off: ANTIGRAVITY_ACP_RISK_ACCEPTANCE.md includes it")
	}
	profiles := acpDriverProfiles()
	if len(profiles) != 2 || profiles[0] != openCodeACPProfile || profiles[1] != antigravityACPProfile {
		t.Fatalf("drivers = %v", profiles)
	}
}

func TestACPDriversAreRepublishedWhenTheListChanges(t *testing.T) {
	prevCache := machineInfoCache
	prevHook := onMachineCapabilityAdded
	t.Cleanup(func() {
		machineInfoMu.Lock()
		machineInfoCache = prevCache
		machineInfoMu.Unlock()
		onMachineCapabilityAdded = prevHook
	})
	sent := 0
	onMachineCapabilityAdded = func() { sent++ }
	pinACPDriverBinaries(t, nil)
	machineInfoMu.Lock()
	machineInfoCache = &MachineInfo{Capabilities: newMachineCapabilities()}
	machineInfoMu.Unlock()

	if refreshACPDriversCapability() || sent != 0 {
		t.Fatal("an unchanged list was sent again")
	}
	pinACPDriverBinaries(t, map[string]acpDriverBinary{opencodeCliAgentID: installedACPBinary("/b/opencode", "2.2.7")})
	if !refreshACPDriversCapability() || sent != 1 {
		t.Fatalf("an OpenCode upgrade was not sent (sent %d)", sent)
	}
	if got := GetMachineInfo().Capabilities.ACPDrivers.Agents; !reflect.DeepEqual(got, []string{"opencode"}) {
		t.Fatalf("cached drivers = %v", got)
	}
	pinACPDriverBinaries(t, map[string]acpDriverBinary{opencodeCliAgentID: installedACPBinary("/b/opencode", "1.19.5")})
	if !refreshACPDriversCapability() || sent != 2 {
		t.Fatal("a downgrade was not sent")
	}
	if got := GetMachineInfo().Capabilities.ACPDrivers.Agents; len(got) != 0 {
		t.Fatalf("cached drivers after the downgrade = %v", got)
	}
}

// The follow-up ledger's installed version for OpenCode and Antigravity is the
// ACP driver's: a CLI the driver does not run has no live version at all.
func TestLiveFollowUpInstalledVersionReadsTheACPDriver(t *testing.T) {
	pinACPDriverBinaries(t, map[string]acpDriverBinary{
		opencodeCliAgentID:    installedACPBinary("/b/opencode", "2.3.0"),
		antigravityCliAgentID: installedACPBinary("/b/agy", "1.5.9"),
	})
	if v := liveFollowUpInstalledVersion(opencodeCliAgentID); v != "2.3.0" {
		t.Fatalf("opencode = %q", v)
	}
	if v := liveFollowUpInstalledVersion(antigravityCliAgentID); v != "1.5.9" {
		t.Fatalf("antigravity = %q", v)
	}
	pinACPDriverBinaries(t, map[string]acpDriverBinary{opencodeCliAgentID: installedACPBinary("/b/opencode", "1.19.5")})
	if v := liveFollowUpInstalledVersion(opencodeCliAgentID); v != "" {
		t.Fatalf("a 1.x OpenCode reads as live version %q", v)
	}
}

// pinACPDriversRefresh counts the capability re-reads scheduled during a test.
func pinACPDriversRefresh(t *testing.T) *int {
	t.Helper()
	n := 0
	prev := scheduleACPDriversRefresh
	scheduleACPDriversRefresh = func() { n++ }
	t.Cleanup(func() { scheduleACPDriversRefresh = prev })
	return &n
}

// A START that finds the CLI below its driver's minimum re-reads the drivers,
// so terminal-service stops choosing a driver this computer cannot run.
func TestATooOldStartRepublishesTheDrivers(t *testing.T) {
	scheduled := pinACPDriversRefresh(t)
	pinACPDriverBinaries(t, map[string]acpDriverBinary{opencodeCliAgentID: installedACPBinary("/b/opencode", "2.3.0")})
	if _, err := prepareACPDriverBinary(openCodeACPProfile, "OpenCode"); err != nil || *scheduled != 0 {
		t.Fatalf("an eligible CLI: %v, %d re-reads", err, *scheduled)
	}
	pinACPDriverBinaries(t, map[string]acpDriverBinary{opencodeCliAgentID: installedACPBinary("/b/opencode", "2.2.6")})
	if _, err := prepareACPDriverBinary(openCodeACPProfile, "OpenCode"); err == nil || *scheduled != 1 {
		t.Fatalf("a too-old CLI: %v, %d re-reads", err, *scheduled)
	}
}

// A successful CLI usage refresh re-read every CLI's version, so it re-reads
// the drivers too.
func TestAUsageRefreshRepublishesTheDrivers(t *testing.T) {
	scheduled := pinACPDriversRefresh(t)
	if _, _, _, err := prepareCLIUsageRefreshResult("secret", "refresh-1", time.Now().UnixMilli(), true, []cliAgentUsage{}, nil); err != nil {
		t.Fatal(err)
	}
	if *scheduled != 1 {
		t.Fatalf("%d re-reads after a usage refresh", *scheduled)
	}
	if _, _, _, err := prepareCLIUsageRefreshResult("secret", "refresh-2", time.Now().UnixMilli(), false, nil, nil); err != nil {
		t.Fatal(err)
	}
	if *scheduled != 1 {
		t.Fatal("a failed usage refresh re-read the drivers")
	}
}
