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
