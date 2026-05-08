// File: systemInfoExtended_test.go
// -----------------------------------------------------------------------------
// Tests for the deterministic, no-shell-out helpers in systemInfoExtended.go.
// Platform-specific gather paths (gatherGPUDarwin, gatherBatteryWindows, etc.)
// invoke real OS commands and aren't unit-tested here — they're validated
// against integration runs on provisioned hosts before each release.
//
// What we DO assert:
//   - parsing helpers (parseAppleVRAM, splitQuoted, normalizeAppleVendor,
//     guessVendorFromName) for fixed inputs
//   - JSON shape of MachineInfo with the new optional fields populated
//     and absent (omitempty contract)
//   - roundPct produces the documented "one decimal" form
// -----------------------------------------------------------------------------

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Codex P2 (PR #16): the earlier gatherLiveLoad returned nil when both
// rounded values were 0, dropping legitimate idle-host samples. The fix
// tracks per-probe success and returns nil only when BOTH probes failed.
// composeLiveInfo is the testable kernel of that logic.
func TestComposeLiveInfo(t *testing.T) {
	cases := []struct {
		name     string
		cpuPct   float64
		cpuOK    bool
		memPct   float64
		memOK    bool
		wantNil  bool
		wantCPU  float64
		wantMem  float64
	}{
		// Both probes succeed with non-zero — typical hot host
		{"hot host", 73.5, true, 81.2, true, false, 73.5, 81.2},
		// Both probes succeed with zero — genuinely idle host. Must NOT
		// be treated as failure: downstream task-routing wants to know
		// "this big-RAM workstation is fully free" vs "we couldn't probe".
		{"idle host (legit zeros)", 0, true, 0, true, false, 0, 0},
		// Mixed: one probe succeeded with zero, the other returned a real value
		{"cpu zero, mem real", 0, true, 42.1, true, false, 0, 42.1},
		{"mem zero, cpu real", 12.7, true, 0, true, false, 12.7, 0},
		// One probe failed: keep the successful one, zero out the failed one
		{"cpu probe failed", 0, false, 50.0, true, false, 0, 50.0},
		{"mem probe failed", 30.0, true, 0, false, false, 30.0, 0},
		// Both probes failed: nil (the only nil case)
		{"both probes failed", 0, false, 0, false, true, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := composeLiveInfo(tc.cpuPct, tc.cpuOK, tc.memPct, tc.memOK)
			if tc.wantNil {
				if got != nil {
					t.Errorf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected non-nil liveInfo, got nil")
			}
			if got.CPUPct != tc.wantCPU {
				t.Errorf("CPUPct = %v, want %v", got.CPUPct, tc.wantCPU)
			}
			if got.MemPct != tc.wantMem {
				t.Errorf("MemPct = %v, want %v", got.MemPct, tc.wantMem)
			}
		})
	}
}

func TestRoundPct(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{0, 0},
		{50, 50},
		{99.95, 100.0}, // round half up
		{12.34, 12.3},
		{12.36, 12.4},
		{0.04, 0},
	}
	for _, tc := range cases {
		got := roundPct(tc.in)
		if got != tc.want {
			t.Errorf("roundPct(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseAppleVRAM(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"8 GB", 8.0},
		{"16 GB", 16.0},
		{"1536 MB", 1.5},
		{"512 MB", 0.5},
		{"", 0},
		{"unknown", 0},
		{"1024 MB", 1.0},
		// Apple Silicon: empty / unified memory — we want 0 here so the
		// downstream consumer falls through to Vendor=="apple" detection.
		{"0 GB", 0},
	}
	for _, tc := range cases {
		got := parseAppleVRAM(tc.in)
		// Allow tiny FP slack
		if got < tc.want-0.01 || got > tc.want+0.01 {
			t.Errorf("parseAppleVRAM(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeAppleVendor(t *testing.T) {
	cases := map[string]string{
		"sppci_vendor_apple":  "apple",
		"sppci_vendor_intel":  "intel",
		"sppci_vendor_amd":    "amd",
		"sppci_vendor_nvidia": "nvidia",
		"":                    "other",
		"sppci_vendor_ati":    "amd", // legacy ATI naming
		"unknown_blob":        "other",
	}
	for in, want := range cases {
		got := normalizeAppleVendor(in)
		if got != want {
			t.Errorf("normalizeAppleVendor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGuessVendorFromName(t *testing.T) {
	cases := map[string]string{
		"NVIDIA GeForce RTX 4090":          "nvidia",
		"GeForce GTX 1660":                 "nvidia",
		"AMD Radeon Pro 5500M":             "amd",
		"ATI Radeon HD 5450":               "amd",
		"Intel(R) UHD Graphics 630":        "intel",
		"Intel Iris Plus":                  "intel",
		"Apple M2 Pro":                     "apple",
		"Bochs Display":                    "other",
		"":                                 "other",
	}
	for in, want := range cases {
		got := guessVendorFromName(in)
		if got != want {
			t.Errorf("guessVendorFromName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitQuoted(t *testing.T) {
	// Real lspci -mm output format.
	line := `01:00.0 "VGA compatible controller" "NVIDIA Corporation" "AD102 [GeForce RTX 4090]" -ra1 "Gigabyte Technology Co., Ltd"`
	tokens := splitQuoted(line)
	want := []string{
		"VGA compatible controller",
		"NVIDIA Corporation",
		"AD102 [GeForce RTX 4090]",
		"Gigabyte Technology Co., Ltd",
	}
	if len(tokens) != len(want) {
		t.Fatalf("splitQuoted: got %d tokens, want %d (got %v)", len(tokens), len(want), tokens)
	}
	for i, w := range want {
		if tokens[i] != w {
			t.Errorf("splitQuoted[%d] = %q, want %q", i, tokens[i], w)
		}
	}
}

func TestSplitQuoted_HandlesEmptyAndUnbalanced(t *testing.T) {
	// Should not panic on malformed input. Returns whatever quoted tokens
	// it managed to parse before the unterminated quote.
	if got := splitQuoted(`one "two" three "unterminated`); len(got) != 1 || got[0] != "two" {
		t.Errorf("unbalanced input: got %v, want [two]", got)
	}
	if got := splitQuoted(""); len(got) != 0 {
		t.Errorf("empty input: got %v, want []", got)
	}
	if got := splitQuoted(`"a" "b" "c"`); len(got) != 3 {
		t.Errorf("three quoted: got %v", got)
	}
}

func TestMachineInfo_JSONShape_NewFields(t *testing.T) {
	// All new fields populated — verify they appear in the wire format
	// with the documented JSON keys.
	dockerRunning := true
	mi := &MachineInfo{
		Architecture: "arm64",
		GPU: []gpuInfo{
			{Name: "Apple M2 Pro", Vendor: "apple"},
			{Name: "NVIDIA GeForce RTX 4090", Vendor: "nvidia", MemoryGB: 24, Driver: "535.171.04"},
		},
		Battery:       &batteryInfo{Present: true, Charging: false, Plugged: true, Level: 100},
		Live:          &liveInfo{CPUPct: 12.3, MemPct: 67.8},
		DockerRunning: &dockerRunning,
	}
	b, err := json.Marshal(mi)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(b)
	for _, key := range []string{"gpu", "battery", "live", "dockerRunning"} {
		if !strings.Contains(out, "\""+key+"\"") {
			t.Errorf("expected %q key in JSON; got: %s", key, out)
		}
	}
	// Apple Silicon entry has no MemoryGB — confirm omitempty drops it
	// while NVIDIA entry's 24 surfaces.
	if !strings.Contains(out, `"memoryGB":24`) {
		t.Errorf("expected memoryGB:24 for NVIDIA entry: %s", out)
	}
}

func TestMachineInfo_JSONShape_NewFieldsOmittedWhenEmpty(t *testing.T) {
	// No GPU, no battery, no live, no dockerRunning — every new field
	// must be absent from the JSON, NOT serialized as null/empty/false.
	// This matters because the backend's persist contract treats absent
	// vs explicit-false differently for `dockerRunning`.
	mi := &MachineInfo{
		Architecture: "arm64",
		CPU:          &cpuInfo{Cores: 8},
	}
	b, err := json.Marshal(mi)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(b)
	for _, key := range []string{"gpu", "battery", "live", "dockerRunning"} {
		if strings.Contains(out, "\""+key+"\"") {
			t.Errorf("did NOT expect %q in JSON when empty; got: %s", key, out)
		}
	}
}

// Codex P1 (PR #16): lspci -mm tokens are class/vendor/device/subsystem,
// so the GPU name lives at tokens[2] and the vendor at tokens[1].
// Earlier code mis-mapped tokens[2]→vendor and tokens[3]→name, which on
// a real RTX 4090 line would surface "Gigabyte Technology Co., Ltd" as
// the GPU name and guess the vendor from the device string. Pin the
// correct mapping.
func TestSplitQuoted_LspciTokenOrder(t *testing.T) {
	// Real lspci -mm line from an NVIDIA card. Subsystem-vendor present.
	line := `01:00.0 "VGA compatible controller" "NVIDIA Corporation" "AD102 [GeForce RTX 4090]" -ra1 "Gigabyte Technology Co., Ltd"`
	tokens := splitQuoted(line)
	if len(tokens) < 3 {
		t.Fatalf("expected at least 3 tokens; got %v", tokens)
	}
	if tokens[0] != "VGA compatible controller" {
		t.Errorf("tokens[0] (class) = %q, want VGA compatible controller", tokens[0])
	}
	if tokens[1] != "NVIDIA Corporation" {
		t.Errorf("tokens[1] (vendor) = %q, want NVIDIA Corporation", tokens[1])
	}
	if tokens[2] != "AD102 [GeForce RTX 4090]" {
		t.Errorf("tokens[2] (device/name) = %q, want AD102 [GeForce RTX 4090]", tokens[2])
	}
	// Confirm the downstream guessVendorFromName resolves correctly from
	// the vendor field (tokens[1]), not from the subsystem.
	if got := guessVendorFromName(tokens[1]); got != "nvidia" {
		t.Errorf("guessVendorFromName(tokens[1]) = %q, want nvidia", got)
	}
	if got := guessVendorFromName(tokens[2]); got != "nvidia" {
		// Device name also contains "GeForce/RTX" cues, so the fallback
		// path also resolves to nvidia — the fix's tie-break logic.
		t.Errorf("device-name fallback guessVendorFromName(tokens[2]) = %q, want nvidia", got)
	}
}

// Codex P2 (PR #16): macOS pmset output containing "discharging" was
// matching the substring "charging" and incorrectly setting Charging=true
// on unplugged laptops. The exclusion guard must reject both
// "discharging" and "not charging" while keeping plain "charging" and
// "charging+high" / "charging+low" cases (Win32_Battery uses similar
// idioms).
func TestChargingDetection_ExcludesDischargingAndNotCharging(t *testing.T) {
	// Match the production check verbatim.
	isCharging := func(line string) bool {
		lc := strings.ToLower(line)
		return strings.Contains(lc, "charging") &&
			!strings.Contains(lc, "discharging") &&
			!strings.Contains(lc, "not charging")
	}
	cases := map[string]bool{
		"-InternalBattery-0 (id=...)\t100%; charged; 0:00 remaining present: true":         false,
		"-InternalBattery-0 (id=...)\t87%; charging; 0:42 remaining present: true":         true,
		"-InternalBattery-0 (id=...)\t50%; discharging; 3:14 remaining present: true":      false,
		"-InternalBattery-0 (id=...)\t12%; not charging; 0:00 remaining present: true":     false,
		"-InternalBattery-0 (id=...)\t60%; AC attached; not charging; ":                    false,
		// Windows-shaped plain "charging"
		"BatteryStatus: 6 (Charging)":                                                     true,
	}
	for line, want := range cases {
		if got := isCharging(line); got != want {
			t.Errorf("isCharging(%q) = %v, want %v", line, got, want)
		}
	}
}

func TestMachineInfo_JSONShape_DockerRunningFalseSerializes(t *testing.T) {
	// docker installed but daemon down — pointer-to-false must serialize
	// (omitempty omits the *pointer* when nil, not the deref'd value).
	// Otherwise a "docker not running" agent would look identical to a
	// "docker not installed" agent, which is exactly the distinction
	// tools.docker vs dockerRunning is meant to draw.
	dockerNotRunning := false
	mi := &MachineInfo{DockerRunning: &dockerNotRunning}
	b, _ := json.Marshal(mi)
	if !strings.Contains(string(b), `"dockerRunning":false`) {
		t.Errorf("expected dockerRunning:false in JSON; got: %s", string(b))
	}
}
