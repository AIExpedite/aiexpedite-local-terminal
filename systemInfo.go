// File: systemInfo.go
// -----------------------------------------------------------------------------
// Gathers machine-level metadata (CPU / RAM / disk / runtimes / shell /
// detected CLI agents / OS architecture) at startup and on a 6-hour cadence,
// caches it in memory, and exposes it to auth.go for inclusion in every
// /auth/token request.
//
// Phase 2 of the platform/systemInfo consolidation. Before this file existed,
// machine-level info was gathered by the LLM-driven `terminalRepoDiscovery`
// agent on user-initiated discovery — which produced wildly inconsistent
// results across devices (one agent had full CPU/RAM/runtimes, another had
// only `shell.defaultShell`). The Settings → Terminal UI's missing-RAM
// symptom traces directly to that variance.
//
// This implementation is deterministic: gopsutil reads CPU / RAM / disk via
// the OS's native APIs (no shell-out), and `exec.LookPath` + `<cmd>
// --version` probes find runtimes/tools that the user has installed. The
// result is cached server-side on `terminalAgents/{id}` (top-level), where
// terminal-service / mergeMachineInfo can read it without depending on a
// per-workspace LLM probe.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
)

// Per-probe timeout. Most version probes return in <100ms but occasionally
// a runtime spawns a JIT or hits a slow filesystem; cap to keep startup
// snappy and the periodic refresh predictable.
const machineInfoProbeTimeout = 3 * time.Second

// Refresh cadence for the cached machine info. Hardware doesn't change
// without a reboot, runtimes rarely change, so a 6h refresh is plenty.
// First gather runs immediately at startup so the very first /auth/token
// can usually carry full data after a few seconds.
const machineInfoRefreshInterval = 6 * time.Hour

/* --------------------------------------------------------------------------
   Wire types — must match terminal-service `terminalAgents/{id}` schema
   -------------------------------------------------------------------------- */

type cpuInfo struct {
	Name    string `json:"name,omitempty"`
	Cores   int    `json:"cores,omitempty"`
	Threads int    `json:"threads,omitempty"`
	Speed   string `json:"speed,omitempty"`
}

type memoryInfo struct {
	TotalGB     float64 `json:"totalGB,omitempty"`
	AvailableGB float64 `json:"availableGB,omitempty"`
}

type diskEntry struct {
	Drive  string  `json:"drive,omitempty"`
	SizeGB float64 `json:"sizeGB,omitempty"`
	FreeGB float64 `json:"freeGB,omitempty"`
}

type shellInfo struct {
	PowerShellVersion string `json:"powershellVersion,omitempty"`
	PwshVersion       string `json:"pwshVersion,omitempty"`
	BashVersion       string `json:"bashVersion,omitempty"`
	ZshVersion        string `json:"zshVersion,omitempty"`
	DefaultShell      string `json:"defaultShell,omitempty"`
}

type detectedCLIAgent struct {
	Detected bool   `json:"detected"`
	Version  string `json:"version,omitempty"`
	Path     string `json:"path,omitempty"`
	Name     string `json:"name,omitempty"`
}

type capabilitiesInfo struct {
	RecommendedConcurrentTests  int `json:"recommendedConcurrentTests,omitempty"`
	RecommendedConcurrentBuilds int `json:"recommendedConcurrentBuilds,omitempty"`
}

// gpuInfo describes a single GPU adapter. Multi-GPU machines emit one
// entry per adapter (laptop with integrated + discrete; workstation with
// multiple discrete cards). Vendor distinguishes Apple-Silicon integrated
// from NVIDIA / AMD / Intel for task-routing decisions (CUDA-only ML
// jobs need NVIDIA; Metal jobs need Apple).
type gpuInfo struct {
	Name     string  `json:"name,omitempty"`
	Vendor   string  `json:"vendor,omitempty"`   // "apple" | "nvidia" | "amd" | "intel" | "other"
	MemoryGB float64 `json:"memoryGB,omitempty"` // VRAM (or unified memory share on Apple Silicon)
	Driver   string  `json:"driver,omitempty"`   // optional driver/runtime version (e.g. "535.171.04" on NVIDIA)
}

// batteryInfo describes the host's battery state. Absent (returned nil)
// when the host has no battery (most desktops). The agent's task-routing
// can use this to avoid scheduling long-running work onto a laptop on
// battery — discharge-killing the agent mid-task is the worst failure mode.
//
// No `omitempty` on Charging/Plugged/Level: when batteryInfo is emitted at
// all, the probe ran successfully and `Present=true` was already set, so
// the false/0 values for these fields are meaningful states (e.g. critical
// unplugged laptop = `{present:true, charging:false, plugged:false, level:3.0}`).
// Dropping them would make "explicitly on battery / nearly empty"
// indistinguishable from "unknown" and break the avoid-laptop-on-battery
// heuristic on the routing side.
type batteryInfo struct {
	Present  bool    `json:"present"`
	Charging bool    `json:"charging"` // true when plugged in AND not full
	Plugged  bool    `json:"plugged"`  // AC connected (independent of charging state)
	Level    float64 `json:"level"`    // 0.0 - 100.0 percent remaining
}

// liveInfo carries near-real-time load metrics. Refreshed on every gather
// (every 6h by default; can be re-gathered on demand later if we add an
// idle-aware task router). cpu.Percent runs over a 1-second window so
// the value is meaningful, not the cumulative-since-boot average.
//
// No `omitempty`: composeLiveInfo only returns a non-nil *liveInfo when at
// least one probe succeeded, so an emitted liveInfo with cpuPct=0/memPct=0
// represents a genuinely-idle host, not a missing measurement. omitempty
// would erase that distinction on the wire and downstream routing logic
// could no longer treat the machine as a known-idle candidate.
type liveInfo struct {
	CPUPct float64 `json:"cpuPct"` // 0.0 - 100.0
	MemPct float64 `json:"memPct"` // 0.0 - 100.0
}

// MachineInfo is the full payload sent to terminal-service. The backend's
// /auth/token route lifts these fields onto the top-level
// `terminalAgents/{id}` doc; terminal-service mergeMachineInfo reads them
// when assembling the LLM device block.
type MachineInfo struct {
	Architecture      string                      `json:"architecture,omitempty"`
	CPU               *cpuInfo                    `json:"cpu,omitempty"`
	Memory            *memoryInfo                 `json:"memory,omitempty"`
	Disk              []diskEntry                 `json:"disk,omitempty"`
	GPU               []gpuInfo                   `json:"gpu,omitempty"`
	Battery           *batteryInfo                `json:"battery,omitempty"`
	Live              *liveInfo                   `json:"live,omitempty"`
	Runtimes          map[string]string           `json:"runtimes,omitempty"`
	PackageManagers   map[string]string           `json:"packageManagers,omitempty"`
	Tools             map[string]string           `json:"tools,omitempty"`
	DockerRunning     *bool                       `json:"dockerRunning,omitempty"` // pointer so JSON omits when probe didn't run; false = installed but daemon down
	Shell             *shellInfo                  `json:"shell,omitempty"`
	DetectedCliAgents map[string]detectedCLIAgent `json:"detectedCliAgents,omitempty"`
	Capabilities      *capabilitiesInfo           `json:"capabilities,omitempty"`
	CollectedAt       string                      `json:"collectedAt,omitempty"`
}

/* --------------------------------------------------------------------------
   Cache
   -------------------------------------------------------------------------- */

var (
	machineInfoCache *MachineInfo
	machineInfoMu    sync.RWMutex
)

// GetMachineInfo returns the most recently gathered MachineInfo, or nil if
// the first gather hasn't completed yet. Callers (auth.go's getOIDCToken)
// must tolerate nil — we'd rather send a token request without machine
// info than block on the gather.
func GetMachineInfo() *MachineInfo {
	machineInfoMu.RLock()
	defer machineInfoMu.RUnlock()
	return machineInfoCache
}

// StartMachineInfoGathering launches the background goroutine that
// populates the cache. Safe to call once at startup; the loop runs for the
// process lifetime, refreshing every machineInfoRefreshInterval.
//
// First gather runs immediately (not after the first sleep), so within a
// few seconds of agent start the first /auth/token refresh has full data.
func StartMachineInfoGathering() {
	go func() {
		for {
			info := gatherMachineInfo()
			machineInfoMu.Lock()
			machineInfoCache = info
			machineInfoMu.Unlock()
			time.Sleep(machineInfoRefreshInterval)
		}
	}()
}

/* --------------------------------------------------------------------------
   Gather
   -------------------------------------------------------------------------- */

func gatherMachineInfo() *MachineInfo {
	info := &MachineInfo{
		Architecture:    runtime.GOARCH,
		Runtimes:        map[string]string{},
		PackageManagers: map[string]string{},
		Tools:           map[string]string{},
		CollectedAt:     time.Now().UTC().Format(time.RFC3339),
	}

	// CPU — gopsutil reads /proc/cpuinfo, sysctl, or WMI as appropriate.
	if cpus, err := cpu.Info(); err == nil && len(cpus) > 0 {
		physical, _ := cpu.Counts(false)
		logical, _ := cpu.Counts(true)
		speed := ""
		if cpus[0].Mhz > 0 {
			speed = fmt.Sprintf("%.0f MHz", cpus[0].Mhz)
		}
		info.CPU = &cpuInfo{
			Name:    cpus[0].ModelName,
			Cores:   physical,
			Threads: logical,
			Speed:   speed,
		}
	}

	// Memory — round to one decimal so the UI/LLM display matches the
	// "15.7 GB RAM" shape that the existing LLM probe used.
	if vmem, err := mem.VirtualMemory(); err == nil {
		info.Memory = &memoryInfo{
			TotalGB:     roundGB(vmem.Total),
			AvailableGB: roundGB(vmem.Available),
		}
	}

	// Disk — only physical partitions; gopsutil's `all=false` filters out
	// pseudo / virtual filesystems (proc, sysfs, overlayfs, tmpfs) on Linux
	// so we don't pollute the response with container-layer mounts.
	if parts, err := disk.Partitions(false); err == nil {
		for _, p := range parts {
			if usage, err := disk.Usage(p.Mountpoint); err == nil && usage.Total > 0 {
				info.Disk = append(info.Disk, diskEntry{
					Drive:  p.Device,
					SizeGB: roundGB(usage.Total),
					FreeGB: roundGB(usage.Free),
				})
			}
		}
	}

	// Runtimes — same set the legacy LLM probe used. Probe with the
	// platform-conventional command name (e.g. `python3` on Unix, `python`
	// on Windows) and store under a stable key.
	pythonCmd := "python3"
	if runtime.GOOS == "windows" {
		pythonCmd = "python"
	}
	for _, rt := range []struct{ Key, Cmd, Flag string }{
		{"node", "node", "--version"},
		{"python", pythonCmd, "--version"},
		{"go", "go", "version"},
		{"dotnet", "dotnet", "--version"},
		{"ruby", "ruby", "--version"},
	} {
		if v := probeVersion(rt.Cmd, rt.Flag); v != "" {
			info.Runtimes[rt.Key] = v
		}
	}

	// Java is special-cased: `java --version` (two dashes) only works on
	// Java 9+; Java 8 treats it as a class name lookup, fails to load,
	// and exits non-zero. Without the fallback, every JRE/JDK 8 host
	// (still common on enterprise / older dev boxes) would be reported
	// as missing Java even when it's installed.  Try the modern flag
	// first since most hosts are on 11/17/21 now, fall back to the
	// legacy single-dash form on failure.
	if v := probeVersion("java", "--version"); v != "" {
		info.Runtimes["java"] = v
	} else if v := probeVersion("java", "-version"); v != "" {
		info.Runtimes["java"] = v
	}

	// Tools + package managers
	for _, t := range []struct{ Key, Cmd, Flag string }{
		{"git", "git", "--version"},
		{"docker", "docker", "--version"},
	} {
		if v := probeVersion(t.Cmd, t.Flag); v != "" {
			info.Tools[t.Key] = v
		}
	}
	pipCmd := "pip3"
	if runtime.GOOS == "windows" {
		pipCmd = "pip"
	}
	for _, pm := range []struct{ Key, Cmd, Flag string }{
		{"npm", "npm", "--version"},
		{"yarn", "yarn", "--version"},
		{"pip", pipCmd, "--version"},
	} {
		if v := probeVersion(pm.Cmd, pm.Flag); v != "" {
			info.PackageManagers[pm.Key] = v
		}
	}

	// Shell info is platform-shaped — see helper.
	info.Shell = gatherShellInfo()

	// Docker daemon — distinct from `tools.docker` which only proves the
	// CLI binary exists. Containerized tasks (build images, run a
	// containerized test suite) need the daemon up. `docker info` exits
	// non-zero when the daemon isn't running, even if the CLI is on PATH.
	if dr := gatherDockerRunning(); dr != nil {
		info.DockerRunning = dr
	}

	// CLI agents — claude/codex/gemini detection. Same trio as the legacy
	// LLM probe. exec.LookPath honors the PATH that tray_darwin.go's
	// init() augmented with /opt/homebrew/bin etc., so brew-installed
	// agents are visible.
	info.DetectedCliAgents = gatherCLIAgents()

	// GPU — multi-adapter support; per-platform shell-out (system_profiler
	// on macOS, WMI on Windows, nvidia-smi/lspci on Linux). Best-effort:
	// returns nil on probe failure or no detectable GPU. Without this,
	// task routers can't distinguish "Apple Silicon agent for Metal jobs"
	// from "NVIDIA agent for CUDA jobs" from "no GPU at all".
	if gpus := gatherGPU(); len(gpus) > 0 {
		info.GPU = gpus
	}

	// Battery — laptops only; desktops return nil. Lets the task router
	// avoid scheduling a 6h training job onto a laptop running on battery
	// (it'll discharge mid-run and the agent dies with the OS).
	info.Battery = gatherBattery()

	// Live load — 1-second cpu sample + instantaneous mem percent. The
	// 1s sample adds a small fixed cost to the gather but produces a
	// meaningful number (without it, gopsutil's first call returns 0
	// and subsequent calls average over 6 hours, which smooths out any
	// signal we'd actually act on).
	info.Live = gatherLiveLoad()

	// Capability hints — cheap derivation from the gathered numbers; the
	// LLM uses these to decide whether to run tests/builds in parallel.
	if info.CPU != nil && info.Memory != nil && info.Memory.TotalGB > 0 {
		info.Capabilities = &capabilitiesInfo{
			RecommendedConcurrentTests:  minInt(info.CPU.Threads, int(info.Memory.TotalGB/2)),
			RecommendedConcurrentBuilds: minInt(info.CPU.Cores, int(info.Memory.TotalGB/4)),
		}
	}

	return info
}

/* --------------------------------------------------------------------------
   Helpers
   -------------------------------------------------------------------------- */

// probeVersion runs `<cmd> <flag>` with a short timeout and returns the
// first line of output, trimmed. Returns "" on any failure (binary not in
// PATH, non-zero exit, timeout, signal). Never panics.
//
// `flag` may be split into multiple args via spaces ("version" stays one
// arg; "--version" stays one arg). Callers wanting multiple args should
// use probeVersionArgs instead.
func probeVersion(cmd, flag string) string {
	return probeVersionArgs(cmd, flag)
}

func probeVersionArgs(cmd string, args ...string) string {
	if _, err := exec.LookPath(cmd); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), machineInfoProbeTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, cmd, args...)
	hideWindow(c) // prevent console flash on Windows
	out, err := c.CombinedOutput()
	if err != nil {
		return ""
	}
	// Some tools print version on stderr (java) and others print multiple
	// lines (`go version go1.x` is one line; `bash --version` is many).
	// First non-empty line is what we want.
	for _, line := range strings.Split(string(out), "\n") {
		s := strings.TrimSpace(line)
		if s != "" {
			return s
		}
	}
	return ""
}

func gatherShellInfo() *shellInfo {
	s := &shellInfo{}
	if runtime.GOOS == "windows" {
		s.DefaultShell = "powershell"
		// `powershell -NoProfile -Command "$PSVersionTable.PSVersion.ToString()"`
		// is the canonical version probe — `--version` doesn't exist for
		// Windows PowerShell 5.x.
		if v := probeVersionArgs("powershell", "-NoProfile", "-Command", "$PSVersionTable.PSVersion.ToString()"); v != "" {
			s.PowerShellVersion = v
		}
		if v := probeVersionArgs("pwsh", "--version"); v != "" {
			s.PwshVersion = v
		}
		return s
	}

	// macOS / Linux — derive default shell from $SHELL when set (preserves
	// the user's interactive choice, e.g. zsh on macOS 10.15+) and fall
	// back to the existence check.
	if envShell := filepath.Base(strings.TrimSpace(getenv("SHELL"))); envShell != "." && envShell != "" {
		s.DefaultShell = envShell
	} else if _, err := exec.LookPath("zsh"); err == nil {
		s.DefaultShell = "zsh"
	} else {
		s.DefaultShell = "bash"
	}
	if v := probeVersionArgs("bash", "--version"); v != "" {
		s.BashVersion = v
	}
	if v := probeVersionArgs("zsh", "--version"); v != "" {
		s.ZshVersion = v
	}
	return s
}

func gatherCLIAgents() map[string]detectedCLIAgent {
	agents := map[string]detectedCLIAgent{}
	for _, a := range []struct{ Key, Bin string }{
		{"claudeCode", "claude"},
		{"codex", "codex"},
		{"geminiCli", "gemini"},
	} {
		path, err := exec.LookPath(a.Bin)
		if err != nil {
			continue
		}
		entry := detectedCLIAgent{
			Detected: true,
			Path:     path,
			Name:     a.Bin,
		}
		if v := probeVersionArgs(a.Bin, "--version"); v != "" {
			entry.Version = v
		}
		agents[a.Key] = entry
	}
	return agents
}

func roundGB(bytes uint64) float64 {
	gb := float64(bytes) / float64(1<<30)
	// One decimal — matches the existing LLM-probe output shape.
	return float64(int(gb*10+0.5)) / 10
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// getenv is a tiny indirection so tests can override $SHELL detection
// without depending on the test runner's environment.
var getenv = os.Getenv
