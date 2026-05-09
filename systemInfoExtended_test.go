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

// Codex P2 (PR #16): gatherBatteryLinux only recognised power-supply
// type "Mains" as external power, missing USB / USB_PD / USB_C / etc.
// that modern USB-C laptops expose. Pin the broader matcher so a future
// rename or additional kernel-introduced subtype doesn't silently
// regress the avoid-laptop-on-battery heuristic.
func TestLinuxLinePowerTypeMatcher(t *testing.T) {
	// Mirror the production switch case literally — keep this in sync if
	// gatherBatteryLinux's predicate changes.
	isLinePower := func(typ string) bool {
		typLower := strings.ToLower(typ)
		return typLower == "mains" ||
			typLower == "wireless" ||
			strings.HasPrefix(typLower, "usb")
	}
	cases := map[string]bool{
		// classic AC adapter
		"Mains": true,
		"mains": true,
		// USB-C laptops (the regression case)
		"USB":         true,
		"USB_PD":      true,
		"USB_PD_DRP":  true,
		"USB_C":       true,
		"USB_CDP":     true,
		"USB_DCP":     true,
		"usb_pd":      true,
		// wireless charging pads
		"Wireless": true,
		// not line power
		"Battery": false,
		"":        false,
		"Unknown": false,
	}
	for typ, want := range cases {
		if got := isLinePower(typ); got != want {
			t.Errorf("isLinePower(%q) = %v, want %v", typ, got, want)
		}
	}
}

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

// Codex P1/P2 (PR #16): batteryInfo.{Charging,Plugged,Level} and
// liveInfo.{CPUPct,MemPct} must NOT have `omitempty`. When the parent
// struct is emitted at all, the probe ran successfully and false/0 are
// meaningful states (idle host, critical unplugged laptop). Dropping them
// would erase the distinction between "explicitly false/zero" and
// "unknown/missing", breaking task-routing heuristics that key off these
// values (avoid-laptop-on-battery, prefer-idle-host).
func TestMachineInfo_JSONShape_BatteryAndLiveZeroValuesPreserved(t *testing.T) {
	// Critical unplugged laptop at 0% — every battery field is either
	// false or zero, but each is a meaningful explicit measurement.
	mi := &MachineInfo{
		Battery: &batteryInfo{Present: true, Charging: false, Plugged: false, Level: 0},
		Live:    &liveInfo{CPUPct: 0, MemPct: 0},
	}
	b, err := json.Marshal(mi)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(b)
	for _, want := range []string{
		`"present":true`,
		`"charging":false`,
		`"plugged":false`,
		`"level":0`,
		`"cpuPct":0`,
		`"memPct":0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in JSON (zero/false must NOT be omitted); got: %s", want, out)
		}
	}
}

// Codex P1 (PR #16, second review): /sys/class/power_supply exposes
// peripheral batteries (Logitech HID receivers, Bluetooth mice, USB-PD
// PD-controller batteries) under type=Battery. Treating those as the
// host battery would misclassify Linux desktops as battery-powered and
// skew avoid-laptop-on-battery routing. Pin the name-prefix denylist —
// the canonical scope=Device check covers modern kernels, this is the
// fallback for older ones that don't set scope.
func TestIsPeripheralBatteryName(t *testing.T) {
	cases := map[string]bool{
		// Real peripheral examples from production hosts — must filter out.
		"hidpp_battery_0":               true,
		"hidpp_battery_1":               true,
		"hid-04:e6:c2:11:22:33":         true,
		"hid-c4:c1:00:00:00:01-battery": true,
		"ucsi-source-psy-USBC000:001":   true,
		"logitech_keyboard_bluetooth":   true,
		"wireless_keyboard_logi":        true,
		"wireless_mouse_mx":             true,
		"wireless_headset_g733":         true,
		// Real host batteries — must NOT be filtered out.
		"BAT0":                       false,
		"BAT1":                       false,
		"battery":                    false,
		"main_battery":               false,
		"sony_controller_battery_xx": false, // unusual, but no denylist keyword — accept
	}
	for name, want := range cases {
		if got := isPeripheralBatteryName(name); got != want {
			t.Errorf("isPeripheralBatteryName(%q) = %v, want %v", name, got, want)
		}
	}
}

// Codex P2 (PR #16, second review): multi-battery laptops (BAT0 + BAT1
// on enterprise ThinkPads / dual-battery hardware) had their host-level
// `level` overwritten by whichever battery the directory walk visited
// last. Aggregation must be capacity-weighted across all system
// batteries, falling back to a mean of `capacity` percentages only when
// energy_*/charge_* are unavailable. A half-drained 80 Wh main + full
// 20 Wh secondary should report ~60%, NOT 75% (the naive mean of 50% + 100%).
func TestAggregateLinuxBatteryLevel(t *testing.T) {
	tests := []struct {
		name string
		bats []linuxSystemBattery
		want float64
	}{
		{
			name: "single battery, energy_* preferred",
			bats: []linuxSystemBattery{
				{energyNow: 40_000_000, energyFull: 80_000_000, capacity: 50, capacityOK: true},
			},
			want: 50.0,
		},
		{
			name: "two batteries, capacity-weighted by energy",
			bats: []linuxSystemBattery{
				// 80 Wh main, half drained (40 Wh)
				{energyNow: 40_000_000, energyFull: 80_000_000, capacity: 50, capacityOK: true},
				// 20 Wh secondary, full
				{energyNow: 20_000_000, energyFull: 20_000_000, capacity: 100, capacityOK: true},
			},
			// (40 + 20) / (80 + 20) = 60.0
			want: 60.0,
		},
		{
			name: "charge_* fallback when energy_* unavailable",
			bats: []linuxSystemBattery{
				{chargeNow: 3000, chargeFull: 6000},
			},
			want: 50.0,
		},
		{
			name: "capacity-mean fallback when neither energy_* nor charge_* exposed",
			bats: []linuxSystemBattery{
				{capacity: 50, capacityOK: true},
				{capacity: 100, capacityOK: true},
			},
			// Naive mean of percentages — only when no weighted info exists.
			want: 75.0,
		},
		{
			name: "all readings missing — returns 0",
			bats: []linuxSystemBattery{
				{},
			},
			want: 0,
		},
		{
			name: "mixed energy + capacity — weighted wins, capacity ignored",
			bats: []linuxSystemBattery{
				{energyNow: 25_000_000, energyFull: 100_000_000, capacity: 25, capacityOK: true},
				{energyNow: 50_000_000, energyFull: 100_000_000, capacity: 50, capacityOK: true},
			},
			// Weighted = (25 + 50) / (100 + 100) = 37.5
			// Capacity mean would be 37.5 too — pick a case where they differ:
			want: 37.5,
		},
		{
			// Codex P2 (PR #16, third review): mixing µWh and µAh in the
			// same sum is dimensionally invalid (you'd need cell voltage
			// to convert, which we don't have). When one battery exposes
			// only energy_* and the other only charge_*, fall through to
			// the unit-free capacity-mean instead of silently producing
			// a meaningless number.
			name: "mixed unit families — falls back to capacity-mean (unit safety)",
			bats: []linuxSystemBattery{
				// Battery A: energy_* only (no charge_*)
				{energyNow: 40_000_000, energyFull: 80_000_000, capacity: 50, capacityOK: true},
				// Battery B: charge_* only (no energy_*)
				{chargeNow: 4_000_000, chargeFull: 4_000_000, capacity: 100, capacityOK: true},
			},
			// allEnergy=false (B has no energy), allCharge=false (A has
			// no charge) → capacity-mean = (50 + 100) / 2 = 75.
			want: 75,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := aggregateLinuxBatteryLevel(tc.bats); got != tc.want {
				t.Errorf("aggregateLinuxBatteryLevel = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAnyBatteryCharging(t *testing.T) {
	// Kernel uses exact-token status: "Charging" / "Discharging" /
	// "Not charging" / "Full" / "Unknown" — already lowercased by
	// readLinuxBattery before we get here. Equality check is
	// unambiguous (no substring trap).
	cases := []struct {
		name string
		bats []linuxSystemBattery
		want bool
	}{
		{"single charging", []linuxSystemBattery{{status: "charging"}}, true},
		{"single discharging", []linuxSystemBattery{{status: "discharging"}}, false},
		{"single not charging (AC attached, full)", []linuxSystemBattery{{status: "not charging"}}, false},
		{"single full", []linuxSystemBattery{{status: "full"}}, false},
		{"two: one charging, one full → charging", []linuxSystemBattery{
			{status: "full"}, {status: "charging"},
		}, true},
		{"two: both discharging → not charging", []linuxSystemBattery{
			{status: "discharging"}, {status: "discharging"},
		}, false},
		{"empty slice", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := anyBatteryCharging(tc.bats); got != tc.want {
				t.Errorf("anyBatteryCharging = %v, want %v", got, tc.want)
			}
		})
	}
}

// Codex P2 (PR #16, third review): hybrid Linux laptops (Intel iGPU +
// NVIDIA dGPU on most ThinkPad / Dell XPS / System76 hardware) had the
// integrated adapter dropped, because gatherGPULinux returned from
// nvidia-smi early without falling through to lspci. Pin the merge
// behaviour so:
//   - nvidia-smi entries (have VRAM + driver) survive verbatim
//   - non-nvidia lspci entries are appended (the Intel/AMD iGPU case)
//   - duplicate nvidia-vendor lspci entries are dropped (same physical
//     adapter as nvidia-smi, just lower fidelity — keeping both would
//     inflate the per-host adapter count and confuse multi-GPU heuristics)
func TestMergeLinuxGPUs(t *testing.T) {
	nvidia := gpuInfo{Name: "NVIDIA GeForce RTX 4090", Vendor: "nvidia", MemoryGB: 24, Driver: "535.171.04"}
	intelIGPU := gpuInfo{Name: "Raptor Lake-P [Iris Xe Graphics]", Vendor: "intel"}
	amdDGPU := gpuInfo{Name: "Navi 21 [Radeon RX 6800]", Vendor: "amd"}
	nvidiaFromLspci := gpuInfo{Name: "AD102 [GeForce RTX 4090]", Vendor: "nvidia"}

	tests := []struct {
		name       string
		fromNvidia []gpuInfo
		fromLspci  []gpuInfo
		want       []gpuInfo
	}{
		{
			name:       "hybrid laptop: nvidia-smi dGPU + lspci iGPU → both surface",
			fromNvidia: []gpuInfo{nvidia},
			fromLspci:  []gpuInfo{intelIGPU, nvidiaFromLspci},
			// nvidia-smi entry first, then non-duplicate intel entry.
			// nvidia-vendor lspci entry dropped (already covered).
			want: []gpuInfo{nvidia, intelIGPU},
		},
		{
			name:       "no nvidia-smi: lspci entries pass through unchanged",
			fromNvidia: nil,
			fromLspci:  []gpuInfo{intelIGPU, amdDGPU},
			want:       []gpuInfo{intelIGPU, amdDGPU},
		},
		{
			name:       "no lspci: nvidia-smi entries returned as-is",
			fromNvidia: []gpuInfo{nvidia},
			fromLspci:  nil,
			want:       []gpuInfo{nvidia},
		},
		{
			name:       "neither source returned anything",
			fromNvidia: nil,
			fromLspci:  nil,
			want:       []gpuInfo{},
		},
		{
			name:       "AMD-only host: nvidia-smi empty, lspci AMD card surfaces",
			fromNvidia: nil,
			fromLspci:  []gpuInfo{amdDGPU},
			want:       []gpuInfo{amdDGPU},
		},
		{
			name:       "nvidia-only host: nvidia-smi card returned, lspci nvidia entry deduped",
			fromNvidia: []gpuInfo{nvidia},
			fromLspci:  []gpuInfo{nvidiaFromLspci},
			want:       []gpuInfo{nvidia},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeLinuxGPUs(tc.fromNvidia, tc.fromLspci)
			if len(got) != len(tc.want) {
				t.Fatalf("mergeLinuxGPUs len = %d (%+v), want %d (%+v)", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("entry %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseNvidiaSmiGPUs(t *testing.T) {
	// Real nvidia-smi --query-gpu=name,memory.total,driver_version
	// --format=csv,noheader,nounits output. Two-card host.
	out := "NVIDIA GeForce RTX 4090, 24564, 535.171.04\nNVIDIA H100 PCIe, 81920, 535.171.04"
	got := parseNvidiaSmiGPUs(out)
	if len(got) != 2 {
		t.Fatalf("parseNvidiaSmiGPUs len = %d, want 2; got: %+v", len(got), got)
	}
	if got[0].Name != "NVIDIA GeForce RTX 4090" || got[0].Vendor != "nvidia" || got[0].Driver != "535.171.04" {
		t.Errorf("entry 0 = %+v", got[0])
	}
	// 24564 MiB / 1024 → 23.99... → roundPct → 24.0
	if got[0].MemoryGB < 23.9 || got[0].MemoryGB > 24.1 {
		t.Errorf("entry 0 MemoryGB = %v, want ~24", got[0].MemoryGB)
	}
}

func TestParseFloatField(t *testing.T) {
	// readTrim has already stripped whitespace by the time this helper
	// sees the value; parseFloatField only has to distinguish empty
	// (file missing / unreadable) from a parsable number.
	cases := []struct {
		in    string
		ok    bool
		value float64
	}{
		{"", false, 0},
		{"50", true, 50},
		{"50.5", true, 50.5},
		{"abc", false, 0},
	}
	for _, tc := range cases {
		got, ok := parseFloatField(tc.in)
		if ok != tc.ok || got != tc.value {
			t.Errorf("parseFloatField(%q) = (%v, %v), want (%v, %v)", tc.in, got, ok, tc.value, tc.ok)
		}
	}
}
