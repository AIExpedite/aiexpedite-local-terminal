package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// capabilities.mspDrivers lists museCode only for a Muse Code 1.4.18 or newer
// whose own schema has the surface; it is always present, an empty list
// otherwise.
func TestMSPDriversListMuseCodeOnlyWhenItQualifies(t *testing.T) {
	pinACPDriverBinaries(t, nil)
	cases := []struct {
		name string
		b    mspDriverBinary
		want []string
	}{
		{"not installed", mspDriverBinary{}, []string{}},
		{"1.4.18 with the surface", qualifyingMuse("/m/muse-bin-1.4.18", "1.4.18"), []string{"museCode"}},
		{"1.4.20 with the surface", qualifyingMuse("/m/muse-bin-1.4.20", "1.4.20"), []string{"museCode"}},
		{"1.4.17", qualifyingMuse("/m/muse-bin-1.4.17", "1.4.17"), []string{}},
		{"1.4.20 without the surface", mspDriverBinary{path: "/m/muse", found: true, version: "1.4.20"}, []string{}},
		{"a bare Windows launcher", mspDriverBinary{path: `C:\m\muse.cmd`, found: true, shim: true, version: "1.4.20", surface: true}, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pinMSPDriverBinary(t, c.b)
			got := newMSPDriversCapability()
			if got.Protocol != 2 || got.Agents == nil || !reflect.DeepEqual(got.Agents, c.want) {
				t.Fatalf("mspDrivers = %+v, want protocol 2 agents %v", got, c.want)
			}
			caps := newMachineCapabilities()
			if !reflect.DeepEqual(caps.MSPDrivers, got) {
				t.Fatal("newMachineCapabilities does not report the MSP driver")
			}
			// Never in acpDrivers: the two drivers are reported apart.
			if containsString(caps.ACPDrivers.Agents, museCodeCliAgentID) {
				t.Fatal("museCode was listed as an ACP driver")
			}
			raw, _ := json.Marshal(caps)
			if !strings.Contains(string(raw), `"mspDrivers":{"protocol":2,"agents":[`) {
				t.Fatalf("capabilities JSON = %s", raw)
			}
		})
	}
}

// The MSP driver is re-read with the ACP ones and sent when it changes.
func TestMSPDriversAreRepublishedWhenTheyChange(t *testing.T) {
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
	pinMSPDriverBinary(t, mspDriverBinary{})
	machineInfoMu.Lock()
	machineInfoCache = &MachineInfo{Capabilities: newMachineCapabilities()}
	machineInfoMu.Unlock()

	if refreshACPDriversCapability() || sent != 0 {
		t.Fatal("an unchanged report was sent again")
	}
	pinMSPDriverBinary(t, qualifyingMuse("/m/muse-bin-1.4.20", "1.4.20"))
	if !refreshACPDriversCapability() || sent != 1 {
		t.Fatalf("a Muse upgrade was not sent (sent %d)", sent)
	}
	if got := GetMachineInfo().Capabilities.MSPDrivers.Agents; !reflect.DeepEqual(got, []string{"museCode"}) {
		t.Fatalf("cached MSP drivers = %v", got)
	}
	pinMSPDriverBinary(t, qualifyingMuse("/m/muse-bin-1.4.14", "1.4.14"))
	if !refreshACPDriversCapability() || sent != 2 {
		t.Fatal("a downgrade was not sent")
	}
	if got := GetMachineInfo().Capabilities.MSPDrivers.Agents; len(got) != 0 {
		t.Fatalf("cached MSP drivers after the downgrade = %v", got)
	}
}

// The follow-up ledger's installed Muse Code version is the MSP driver's, and
// Muse Code is certified at 1.4.20 (its muselive run, 2026-11-16): it is
// advertised for live follow-ups only where the MSP driver runs that version
// or newer, and only such a session gets a tracker.
func TestMuseCodeIsALiveFollowUpAgentOnlyOnItsDriverAtTheCertifiedVersion(t *testing.T) {
	if got := liveFollowUpCertified[museCodeCliAgentID]; got != "1.4.20" {
		t.Fatalf("museCode certified at %q, want 1.4.20 (the version its run passed at)", got)
	}
	pinMSPDriverBinary(t, qualifyingMuse("/m/muse-bin-1.4.20", "1.4.20"))
	if v := liveFollowUpInstalledVersion(museCodeCliAgentID); v != "1.4.20" {
		t.Fatalf("installed = %q", v)
	}
	if !liveFollowUpKind(museCodeCliAgentID) || newLiveFollowUpTracker(museCodeCliAgentID, "1.4.20") == nil {
		t.Fatal("a certified Muse Code on its MSP driver takes no live follow-ups")
	}
	// The driver runs 1.4.19, which never passed the run.
	pinMSPDriverBinary(t, qualifyingMuse("/m/muse-bin-1.4.19", "1.4.19"))
	if liveFollowUpKind(museCodeCliAgentID) || newLiveFollowUpTracker(museCodeCliAgentID, "1.4.19") != nil {
		t.Fatal("a Muse Code below the certified version would take live follow-ups")
	}
	// A Muse the driver does not run has no live version at all.
	pinMSPDriverBinary(t, qualifyingMuse("/m/muse-bin-1.4.14", "1.4.14"))
	if v := liveFollowUpInstalledVersion(museCodeCliAgentID); v != "" {
		t.Fatalf("a Muse the driver does not run reads as live version %q", v)
	}
	if liveFollowUpKind(museCodeCliAgentID) {
		t.Fatal("a Muse Code off its MSP driver would take live follow-ups")
	}
}
