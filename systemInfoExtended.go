// File: systemInfoExtended.go
// -----------------------------------------------------------------------------
// Tier 1+2 machine-info gather helpers for systemInfo.go's gatherMachineInfo:
//   - gatherDockerRunning: distinct from `tools.docker` (CLI presence) — runs
//     `docker info` to check that the daemon is actually up.
//   - gatherLiveLoad:      1-second cpu sample + instantaneous mem percent
//                          via gopsutil.
//   - gatherGPU:           multi-adapter detection via system_profiler
//                          (macOS), WMI (Windows), nvidia-smi or lspci (Linux).
//                          Best-effort — returns nil on probe failure.
//   - gatherBattery:       laptop battery state via pmset / WMI / sysfs.
//                          Returns nil on desktops with no battery.
//
// All gathers shell out via probeVersionArgs (3s timeout, hidden window on
// Windows). Failures are silent — we'd rather omit a field than block the
// gather goroutine. The 6-hour cadence already smooths over transient
// probe failures.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
)

/* --------------------------------------------------------------------------
   Docker daemon
   -------------------------------------------------------------------------- */

// gatherDockerRunning returns nil when docker isn't installed (no probe
// performed, JSON omits the field), and a pointer to true/false when the
// CLI exists. `docker info` is the canonical "is the daemon up" probe —
// it returns non-zero (typically "Cannot connect to the Docker daemon")
// when Docker Desktop isn't running on Windows/macOS or the dockerd
// systemd unit is down on Linux.
func gatherDockerRunning() *bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), machineInfoProbeTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, "docker", "info")
	hideWindow(c)
	err := c.Run()
	running := err == nil
	return &running
}

/* --------------------------------------------------------------------------
   Live load
   -------------------------------------------------------------------------- */

// gatherLiveLoad returns near-real-time cpu/mem usage. CPU is sampled
// over a 1-second window so the value is meaningful — gopsutil's
// `cpu.Percent(0, false)` returns the cumulative-since-boot average,
// which masks any current load. Memory percent is instantaneous.
//
// Returns nil only when BOTH probes fail. Successful probes that return
// a literal 0 (genuine idle host on a large-RAM box) are kept — Codex
// review on PR #16 flagged that the earlier `if both==0 return nil`
// guard dropped legitimate idle samples and hid the distinction
// "probe failed" vs "host idle" from downstream consumers.
func gatherLiveLoad() *liveInfo {
	var cpuPct, memPct float64
	var cpuOK, memOK bool
	if pcts, err := cpu.Percent(time.Second, false); err == nil && len(pcts) > 0 {
		cpuPct = pcts[0]
		cpuOK = true
	}
	if vmem, err := mem.VirtualMemory(); err == nil {
		memPct = vmem.UsedPercent
		memOK = true
	}
	return composeLiveInfo(cpuPct, cpuOK, memPct, memOK)
}

// composeLiveInfo returns a populated liveInfo when AT LEAST one probe
// succeeded, even if the successful probe returned 0. Returns nil only
// when both probes failed. Split out from gatherLiveLoad so the
// success-vs-zero distinction is unit-testable without mocking
// gopsutil's package-level functions.
func composeLiveInfo(cpuPct float64, cpuOK bool, memPct float64, memOK bool) *liveInfo {
	if !cpuOK && !memOK {
		return nil
	}
	return &liveInfo{
		CPUPct: roundPct(cpuPct),
		MemPct: roundPct(memPct),
	}
}

func roundPct(v float64) float64 {
	// One decimal — UI doesn't benefit from more precision and it keeps
	// the JSON payload compact.
	return float64(int(v*10+0.5)) / 10
}

/* --------------------------------------------------------------------------
   GPU
   -------------------------------------------------------------------------- */

// gatherGPU dispatches to the platform-conventional GPU enumeration
// command. Returns nil/empty on failure. Each entry is best-effort:
// VRAM may be zero on Apple Silicon (unified memory) or on Linux
// without nvidia-smi available.
func gatherGPU() []gpuInfo {
	switch runtime.GOOS {
	case "darwin":
		return gatherGPUDarwin()
	case "windows":
		return gatherGPUWindows()
	case "linux":
		return gatherGPULinux()
	default:
		return nil
	}
}

// macOS: `system_profiler SPDisplaysDataType -json` returns
//
//	{"SPDisplaysDataType": [{"sppci_model": "...", "spdisplays_vendor": "sppci_vendor_apple", "spdisplays_vram": "8 GB"}, ...]}
//
// Apple Silicon entries don't carry a `spdisplays_vram` (unified memory),
// so MemoryGB is left as zero — Vendor=="apple" is the cue downstream.
func gatherGPUDarwin() []gpuInfo {
	out := probeOutputArgs("system_profiler", "SPDisplaysDataType", "-json")
	if out == "" {
		return nil
	}
	var resp struct {
		SPDisplaysDataType []struct {
			Model       string `json:"sppci_model"`
			Vendor      string `json:"spdisplays_vendor"`
			VRAM        string `json:"spdisplays_vram"`
			VRAMShared  string `json:"spdisplays_vram_shared"`
			MetalFamily string `json:"spdisplays_metalfamily"`
		} `json:"SPDisplaysDataType"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil
	}
	gpus := make([]gpuInfo, 0, len(resp.SPDisplaysDataType))
	for _, d := range resp.SPDisplaysDataType {
		if d.Model == "" {
			continue
		}
		entry := gpuInfo{
			Name:   d.Model,
			Vendor: normalizeAppleVendor(d.Vendor),
		}
		// Try discrete VRAM first ("8 GB", "1536 MB"), fall back to shared
		// (Intel Iris Pro and similar). Apple Silicon has neither — leave
		// MemoryGB as 0.
		if v := parseAppleVRAM(d.VRAM); v > 0 {
			entry.MemoryGB = v
		} else if v := parseAppleVRAM(d.VRAMShared); v > 0 {
			entry.MemoryGB = v
		}
		gpus = append(gpus, entry)
	}
	return gpus
}

func normalizeAppleVendor(raw string) string {
	// system_profiler reports vendors as the cryptic "sppci_vendor_apple",
	// "sppci_vendor_intel", "sppci_vendor_amd", etc. Normalize to the
	// short tokens our wire format expects so consumers can match without
	// knowing the system_profiler enum.
	r := strings.ToLower(raw)
	switch {
	case strings.Contains(r, "apple"):
		return "apple"
	case strings.Contains(r, "nvidia"):
		return "nvidia"
	case strings.Contains(r, "intel"):
		return "intel"
	case strings.Contains(r, "amd"), strings.Contains(r, "ati"):
		return "amd"
	default:
		return "other"
	}
}

func parseAppleVRAM(s string) float64 {
	// "8 GB" or "1536 MB" — parse leading float, scale by unit.
	parts := strings.Fields(strings.TrimSpace(s))
	if len(parts) < 2 {
		return 0
	}
	v, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0
	}
	switch strings.ToUpper(parts[1]) {
	case "GB":
		return v
	case "MB":
		return v / 1024
	default:
		return 0
	}
}

// Windows: `Get-CimInstance Win32_VideoController | Select Name,AdapterRAM,DriverVersion | ConvertTo-Json`
// Single-adapter machines emit a JSON object; multi-adapter emit an array
// — accept both shapes.
//
// AdapterRAM is reported as a uint32 by WMI, capping at 4 GB. Cards with
// more VRAM (every modern dGPU) read as ~4 GB. Known WMI quirk; we surface
// what we get and document the limitation in the wire-format comment so
// consumers don't trust the field for high-VRAM filtering. nvidia-smi
// would be more accurate but isn't always installed.
func gatherGPUWindows() []gpuInfo {
	ps := `$g = Get-CimInstance Win32_VideoController | Select-Object Name,AdapterRAM,DriverVersion; ConvertTo-Json -InputObject $g -Compress`
	out := probeOutputArgs("powershell", "-NoProfile", "-NonInteractive", "-Command", ps)
	if out == "" {
		return nil
	}
	// Try array first (multi-GPU), fall back to single object (most laptops/desktops with one card).
	var arr []struct {
		Name          string `json:"Name"`
		AdapterRAM    int64  `json:"AdapterRAM"`
		DriverVersion string `json:"DriverVersion"`
	}
	if err := json.Unmarshal([]byte(out), &arr); err != nil {
		var single struct {
			Name          string `json:"Name"`
			AdapterRAM    int64  `json:"AdapterRAM"`
			DriverVersion string `json:"DriverVersion"`
		}
		if err2 := json.Unmarshal([]byte(out), &single); err2 != nil {
			return nil
		}
		arr = append(arr, single)
	}
	gpus := make([]gpuInfo, 0, len(arr))
	for _, g := range arr {
		if g.Name == "" {
			continue
		}
		gpus = append(gpus, gpuInfo{
			Name:     g.Name,
			Vendor:   guessVendorFromName(g.Name),
			MemoryGB: roundGB(uint64(g.AdapterRAM)),
			Driver:   g.DriverVersion,
		})
	}
	return gpus
}

// Linux: prefer `nvidia-smi` for accurate VRAM (the only common path
// where VRAM is available without root). Fall back to lspci for
// non-NVIDIA cards (no VRAM info, just name).
func gatherGPULinux() []gpuInfo {
	if out := probeOutputArgs("nvidia-smi", "--query-gpu=name,memory.total,driver_version", "--format=csv,noheader,nounits"); out != "" {
		var gpus []gpuInfo
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.Split(line, ",")
			if len(parts) < 2 {
				continue
			}
			name := strings.TrimSpace(parts[0])
			memMiBStr := strings.TrimSpace(parts[1])
			memMiB, _ := strconv.ParseFloat(memMiBStr, 64)
			driver := ""
			if len(parts) >= 3 {
				driver = strings.TrimSpace(parts[2])
			}
			gpus = append(gpus, gpuInfo{
				Name:     name,
				Vendor:   "nvidia",
				MemoryGB: roundPct(memMiB / 1024), // MiB → GiB; reuse the one-decimal rounder
				Driver:   driver,
			})
		}
		if len(gpus) > 0 {
			return gpus
		}
	}
	// lspci fallback — name only, no VRAM. Match VGA/3D/Display class.
	if out := probeOutputArgs("lspci", "-mm"); out != "" {
		var gpus []gpuInfo
		for _, line := range strings.Split(out, "\n") {
			lower := strings.ToLower(line)
			if !strings.Contains(lower, `"vga compatible controller"`) &&
				!strings.Contains(lower, `"3d controller"`) &&
				!strings.Contains(lower, `"display controller"`) {
				continue
			}
			// lspci -mm format (with bus ID stripped — splitQuoted only
			// returns the quoted tokens):
			//   tokens[0] = class       ("VGA compatible controller")
			//   tokens[1] = vendor      ("NVIDIA Corporation")
			//   tokens[2] = device      ("AD102 [GeForce RTX 4090]")  ← what we want as Name
			//   tokens[3] = subsystem-vendor (optional, "Gigabyte Technology Co., Ltd")
			//
			// Earlier this code read tokens[2] as vendor and tokens[3] as
			// name, which mapped the GPU name to the BOARD MAKER and
			// guessed vendor from the device string — Codex review on PR #16
			// flagged that anywhere nvidia-smi was unavailable, the routing
			// signal this field provides was systematically wrong.
			tokens := splitQuoted(line)
			if len(tokens) < 3 {
				continue
			}
			vendorRaw := tokens[1]
			nameRaw := strings.TrimSpace(tokens[2])
			// Prefer the explicit vendor field; if it normalises to "other"
			// (e.g. obscure rebrand), try the device name as a tie-breaker.
			vendor := guessVendorFromName(vendorRaw)
			if vendor == "other" {
				vendor = guessVendorFromName(nameRaw)
			}
			gpus = append(gpus, gpuInfo{
				Name:   nameRaw,
				Vendor: vendor,
			})
		}
		return gpus
	}
	return nil
}

// guessVendorFromName extracts the canonical vendor token from a name
// string ("NVIDIA GeForce ...", "AMD Radeon ...", "Intel UHD ..."). Used
// by the Windows and Linux-lspci paths where the vendor isn't surfaced
// as a separate field.
func guessVendorFromName(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "nvidia"), strings.Contains(n, "geforce"), strings.Contains(n, "rtx"), strings.Contains(n, "gtx"):
		return "nvidia"
	case strings.Contains(n, "amd"), strings.Contains(n, "radeon"), strings.Contains(n, "ati"):
		return "amd"
	case strings.Contains(n, "intel"):
		return "intel"
	case strings.Contains(n, "apple"):
		return "apple"
	default:
		return "other"
	}
}

// splitQuoted parses lspci -mm-style output: tokens are double-quoted,
// separated by spaces. Returns the unquoted token strings.
func splitQuoted(line string) []string {
	var out []string
	inQuotes := false
	cur := strings.Builder{}
	for _, r := range line {
		switch {
		case r == '"':
			if inQuotes {
				out = append(out, cur.String())
				cur.Reset()
			}
			inQuotes = !inQuotes
		case inQuotes:
			cur.WriteRune(r)
		}
	}
	return out
}

/* --------------------------------------------------------------------------
   Battery
   -------------------------------------------------------------------------- */

// gatherBattery dispatches to the platform-conventional battery probe.
// Returns nil when the host has no battery (most desktops) or the probe
// fails. The wire format includes a `present` flag so consumers can
// distinguish "not a laptop" from "couldn't probe right now" (former is
// more common).
func gatherBattery() *batteryInfo {
	switch runtime.GOOS {
	case "darwin":
		return gatherBatteryDarwin()
	case "windows":
		return gatherBatteryWindows()
	case "linux":
		return gatherBatteryLinux()
	default:
		return nil
	}
}

// macOS: `pmset -g batt` output:
//
//	Now drawing from 'AC Power'
//	 -InternalBattery-0 (id=...)	100%; charged; 0:00 remaining present: true
//
// or on a desktop: just `Now drawing from 'AC Power'` with no battery
// line. We treat absent battery line as no-battery.
func gatherBatteryDarwin() *batteryInfo {
	out := probeOutputArgs("pmset", "-g", "batt")
	if out == "" {
		return nil
	}
	bi := &batteryInfo{}
	plugged := strings.Contains(out, "'AC Power'")
	bi.Plugged = plugged
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "InternalBattery") {
			continue
		}
		bi.Present = true
		// Level: first "NN%" token.
		if idx := strings.Index(line, "%"); idx > 0 {
			start := idx
			for start > 0 && (line[start-1] >= '0' && line[start-1] <= '9') {
				start--
			}
			if v, err := strconv.ParseFloat(line[start:idx], 64); err == nil {
				bi.Level = v
			}
		}
		// Charging vs charged vs discharging — the line carries one of:
		// "charging", "charged", "discharging", "AC attached".
		// Substring-match `"charging"` matches BOTH "charging" and
		// "discharging" (and "not charging"). Codex review on PR #16
		// flagged that earlier code marked unplugged laptops as charging.
		// Exclude both "discharging" and "not charging" to leave only the
		// genuine "charging" / "charging+full" cases.
		lc := strings.ToLower(line)
		if strings.Contains(lc, "charging") &&
			!strings.Contains(lc, "discharging") &&
			!strings.Contains(lc, "not charging") {
			bi.Charging = true
		}
	}
	if !bi.Present {
		return nil
	}
	return bi
}

// Windows: `Get-CimInstance Win32_Battery` → BatteryStatus + EstimatedChargeRemaining.
// BatteryStatus values:
//
//	1 = Discharging   2 = AC          3 = Fully charged   4 = Low
//	5 = Critical      6 = Charging    7 = Charging+High   8 = Charging+Low
//	9 = Charging+Crit 10 = Undefined  11 = Partially Charged
func gatherBatteryWindows() *batteryInfo {
	ps := `$b = Get-CimInstance Win32_Battery | Select-Object -First 1 BatteryStatus,EstimatedChargeRemaining; ConvertTo-Json -InputObject $b -Compress`
	out := probeOutputArgs("powershell", "-NoProfile", "-NonInteractive", "-Command", ps)
	if out == "" || out == "null" {
		return nil
	}
	var b struct {
		BatteryStatus            int     `json:"BatteryStatus"`
		EstimatedChargeRemaining float64 `json:"EstimatedChargeRemaining"`
	}
	if err := json.Unmarshal([]byte(out), &b); err != nil {
		return nil
	}
	if b.BatteryStatus == 0 {
		return nil
	}
	charging := b.BatteryStatus == 6 || b.BatteryStatus == 7 || b.BatteryStatus == 8 || b.BatteryStatus == 9
	plugged := b.BatteryStatus == 2 || b.BatteryStatus == 3 || charging
	return &batteryInfo{
		Present:  true,
		Charging: charging,
		Plugged:  plugged,
		Level:    b.EstimatedChargeRemaining,
	}
}

// Linux: read /sys/class/power_supply directly. No shell-out needed.
//
// Power-supply types we treat as "external power":
//   - "Mains"    — wall AC adapter (the legacy / classic case)
//   - "USB"      — generic USB power input
//   - "USB_PD"   — USB Power Delivery (modern USB-C laptops)
//   - "USB_PD_DRP" — USB-C dual-role port (host or device)
//   - "USB_C", "USB_CDP", "USB_DCP" — variants surfaced by some kernels
//   - "Wireless" — Qi / wireless charging pad
//
// Earlier code recognised only "Mains", so modern USB-C laptops connected
// to a USB-PD charger were reported as `plugged=false` while clearly on
// external power — Codex review on PR #16 flagged that this skews the
// task router's avoid-laptop-on-battery heuristic. Match the prefix
// "usb" plus the explicit mains/wireless tokens so any current or
// near-future USB power-delivery variant is recognised.
func gatherBatteryLinux() *batteryInfo {
	supplyDir := "/sys/class/power_supply"
	entries, err := readDir(supplyDir)
	if err != nil {
		return nil
	}
	bi := &batteryInfo{}
	for _, name := range entries {
		typ := readTrim(filepath.Join(supplyDir, name, "type"))
		typLower := strings.ToLower(typ)
		switch {
		case typLower == "battery":
			bi.Present = true
			if cap := readTrim(filepath.Join(supplyDir, name, "capacity")); cap != "" {
				if v, err := strconv.ParseFloat(cap, 64); err == nil {
					bi.Level = v
				}
			}
			st := strings.ToLower(readTrim(filepath.Join(supplyDir, name, "status")))
			if st == "charging" {
				bi.Charging = true
			}
		case typLower == "mains" || typLower == "wireless" || strings.HasPrefix(typLower, "usb"):
			if readTrim(filepath.Join(supplyDir, name, "online")) == "1" {
				bi.Plugged = true
			}
		}
	}
	if !bi.Present {
		return nil
	}
	return bi
}

/* --------------------------------------------------------------------------
   Shared helpers
   -------------------------------------------------------------------------- */

// probeOutputArgs runs cmd args and returns trimmed combined output, or
// "" on failure. Mirrors probeVersionArgs in systemInfo.go but doesn't
// pick the first line — gpu/battery probes need the full multi-line
// output for parsing.
func probeOutputArgs(cmd string, args ...string) string {
	if _, err := exec.LookPath(cmd); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), machineInfoProbeTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, cmd, args...)
	hideWindow(c)
	out, err := c.CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// readDir is os.ReadDir reduced to a name slice; lives here so the linux
// battery path doesn't need to import "os" at the top of this file (the
// Windows / macOS paths don't read the filesystem). Returns nil on any
// error so the caller can fall through cleanly.
func readDir(path string) ([]string, error) {
	cmd := exec.Command("ls", "-1", path)
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// readTrim reads the first line of a sysfs file. Returns "" on any error
// (file missing, permission denied, etc.) — every battery field is
// optional so silent failure is the right default.
func readTrim(path string) string {
	cmd := exec.Command("cat", path)
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// fmt is referenced only via a build-time guard against unused import on
// platforms where the Linux helpers above are dead code. The Go compiler
// doesn't drop unused imports automatically inside a single file.
var _ = fmt.Sprintf
