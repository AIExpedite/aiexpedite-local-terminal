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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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

// capabilitiesInfo is `terminalAgents/{agentId}.capabilities`. It carries two
// different kinds of fact and they must not share a fate:
//
//   - concurrency HINTS derived from the CPU and memory probes — best-effort,
//     absent when a probe fails;
//   - PROTOCOL flags — what this build of the agent can speak. These are true
//     by construction (the code is compiled in) and must be reported whether
//     or not any hardware probe succeeded.
//
// Until the voice relay this struct was only allocated when BOTH probes
// succeeded, and auth.go forwards it only when non-nil. A machine where
// gopsutil could not read the CPU (a locked-down VM, WMI broken on Windows)
// would then report no relay capabilities at all, and terminal-service would
// refuse every relay to it with DEVICE_UPDATE_REQUIRED — telling the user to
// update an agent that is already current. gatherMachineInfo therefore
// allocates it (newMachineCapabilities) unconditionally before the probes run,
// and applyConcurrencyHints merges the hints in only when they land.
type capabilitiesInfo struct {
	RecommendedConcurrentTests  int `json:"recommendedConcurrentTests,omitempty"`
	RecommendedConcurrentBuilds int `json:"recommendedConcurrentBuilds,omitempty"`

	// RelayTurnInbox (shared-constants DEVICE_RELAY_CAPABILITIES.TURN_INBOX):
	// a session command whose envelope id carries the relay prefix goes
	// through the durable per-session turn inbox (relay_turn_inbox.go), so a
	// redelivered relay SEND runs at most once after it was accepted.
	// REQUIRED by the relay. Never omitempty: a future build that reports
	// `false` must not read as "field absent".
	RelayTurnInbox bool `json:"relayTurnInbox"`
	// RelayAckWatermark (DEVICE_RELAY_CAPABILITIES.ACK_WATERMARK): the agent
	// verifies a SIGNED `ackedTurnIds` watermark on a session command and
	// deletes exactly those inbox rows. terminal-service attaches the field
	// only when this is true — an older agent would reject the unknown signed
	// field as a bad signature.
	RelayAckWatermark bool `json:"relayAckWatermark"`
	// RelayPermissionPrompts (DEVICE_RELAY_CAPABILITIES.PERMISSION_PROMPTS):
	// a Claude session started with `--permission-prompt-tool stdio` launches
	// WITHOUT --dangerously-skip-permissions and accepts a signed
	// claude_native_control (a human's control_response). Without it a voice
	// relay must not start Claude at all: an older agent would add the
	// auto-approve flag regardless.
	RelayPermissionPrompts bool `json:"relayPermissionPrompts"`
}

// newMachineCapabilities returns the capabilities every build of this agent
// reports regardless of hardware probes: the relay protocol flags.
func newMachineCapabilities() *capabilitiesInfo {
	return &capabilitiesInfo{
		RelayTurnInbox:         true,
		RelayAckWatermark:      true,
		RelayPermissionPrompts: true,
	}
}

// applyConcurrencyHints merges the CPU/RAM-derived concurrency hints into
// caps when both probes landed; otherwise caps keeps only its protocol flags.
// Recommendations cap at min(threads/cores, RAM/2 or RAM/4).
func applyConcurrencyHints(caps *capabilitiesInfo, c *cpuInfo, m *memoryInfo) {
	if caps == nil || c == nil || m == nil || m.TotalGB <= 0 {
		return
	}
	caps.RecommendedConcurrentTests = minInt(c.Threads, int(m.TotalGB/2))
	caps.RecommendedConcurrentBuilds = minInt(c.Cores, int(m.TotalGB/4))
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
	// CliAgents is the richer per-agent shape introduced with the CLI Agents
	// tab — one entry per provider with account/plan/capacity metrics. The
	// older DetectedCliAgents map stays for backward compatibility with the
	// About-tab chip strip (legacy clients render it directly).
	CliAgents    []cliAgentUsage   `json:"cliAgents,omitempty"`
	Capabilities *capabilitiesInfo `json:"capabilities,omitempty"`
	CollectedAt  string            `json:"collectedAt,omitempty"`
	// Virtualization is Windows-only (nil elsewhere, and when neither signal
	// could be read): whether hardware virtualization is usable and whether
	// WSL2 is set up — Docker Desktop needs both (systemInfo_setup.go).
	Virtualization *virtualizationInfo `json:"virtualization,omitempty"`
	// PowerShell is Windows-only (nil elsewhere, and when no host could be
	// probed): each host's execution policy and whether it blocks local
	// scripts such as npm.ps1 (systemInfo_powershell.go).
	PowerShell *powerShellPolicyInfo `json:"powerShell,omitempty"`

	// baseTools is Tools as the gather's own probes left it, before the
	// setupToolCatalog results were merged in, so a later catalog pass can
	// recompute Tools without keeping a stale or removed catalog id
	// (setup_tool_catalog.go withSetupToolResults). Not serialized.
	baseTools map[string]string

	// gatherSeq orders gathers by when they STARTED (nextMachineInfoGatherSeq,
	// assigned before any probe runs). CollectedAt has one-second resolution,
	// so two gathers started in the same second carry the same stamp; the
	// sequence still tells them apart (storeMachineInfoIfNotOlder). 0 = not
	// from a gather (fixtures). Not serialized.
	gatherSeq uint64
}

// machineInfoGatherSeq is the last sequence handed to a gather.
var machineInfoGatherSeq atomic.Uint64

// nextMachineInfoGatherSeq returns the sequence for a gather starting now.
func nextMachineInfoGatherSeq() uint64 {
	return machineInfoGatherSeq.Add(1)
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

// SetCachedCLIAgents updates only the CliAgents slice on the cached
// MachineInfo so the next /auth/token request sends the freshly polled
// usage instead of the stale 6h-gather snapshot. Called by the
// demand-driven __cli_usage_refresh__ handler after a successful
// lightweight gather; without it a WIF/token refresh that races with
// an Active-loop poll would POST the old cliAgents array and revert
// the backend to stale quota/account data.
//
// No-op when the cache hasn't been populated yet (first gather still
// in flight) — the next full gather will overwrite this slice anyway.
func SetCachedCLIAgents(usage []cliAgentUsage) {
	machineInfoMu.Lock()
	defer machineInfoMu.Unlock()
	if machineInfoCache == nil {
		return
	}
	machineInfoCache.CliAgents = usage
}

// RefreshMachineInfoNow runs a synchronous, off-cycle gather and updates the
// cache. Now ONLY triggered by internal callers that need an immediate
// machine-info refresh; the backend's demand-driven CLI-usage path goes
// through GatherCLIAgentUsageOnly (cliagent_usage.go) which probes ONLY
// the provider quotas — no CPU, memory, GPU, runtime, or shell gather.
// Without this separation, every Active-loop tick (~5min) would drag
// the full machine-info probe along with it and cost ~10x what the
// frontend actually needs to refresh.
//
// The 6h periodic gather started by StartMachineInfoGathering remains
// the source of truth for the full MachineInfo cache.
func RefreshMachineInfoNow() {
	storeMachineInfoIfNotOlder(gatherMachineInfo())
}

// storeMachineInfo replaces the cached MachineInfo. Every writer goes through
// it so a token request that went out before the first gather finished is
// re-sent with the data as soon as ANY gather lands.
func storeMachineInfo(info *MachineInfo) {
	machineInfoMu.Lock()
	machineInfoCache = info
	machineInfoMu.Unlock()
	machineInfoStored(info)
}

// machineInfoStored runs the post-store hook for a gather that landed.
func machineInfoStored(info *MachineInfo) {
	if info != nil && onMachineInfoStored != nil {
		onMachineInfoStored()
	}
}

// onMachineInfoStored runs after a gather lands in the cache. It is wired in
// auth.go's init() rather than called directly: a direct call would close a
// package-initialization cycle through refreshMachineInfoAfterCatalogUpdate.
var onMachineInfoStored func()

// machineInfoGathersInFlight counts full machine gathers running on a
// background goroutine. A gather is not cancellable once started —
// GatherReadinessOnly returns on its context while the gather it launched runs
// on — and its last act is every CLI's usage parser, which reads process-global
// state (PATH, the Claude cache path and probe endpoint). The counter lets a
// test drain such a gather before it switches those fixtures, so a gather from
// one test can never probe against the next test's.
var (
	machineInfoGathersMu       sync.Mutex
	machineInfoGathersInFlight int
)

// trackMachineInfoGather counts one background gather (or the part of one that
// outlives its caller, such as an inspection's CLI usage pass) in
// machineInfoGathersInFlight until the returned release runs.
func trackMachineInfoGather() (release func()) {
	machineInfoGathersMu.Lock()
	machineInfoGathersInFlight++
	machineInfoGathersMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			machineInfoGathersMu.Lock()
			machineInfoGathersInFlight--
			machineInfoGathersMu.Unlock()
		})
	}
}

// machineInfoCollectedAt parses MachineInfo.CollectedAt (stamped when the
// gather STARTED). ok is false for nil info or an unparseable stamp.
func machineInfoCollectedAt(info *MachineInfo) (time.Time, bool) {
	if info == nil {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, info.CollectedAt)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// machineInfoStartedAfter reports whether gather info started after the one
// in current, i.e. may replace it. Gathers are ordered by gatherSeq; the
// second-resolution CollectedAt is only the fallback for info not made by a
// gather (gatherSeq 0), and a tie there keeps current.
func machineInfoStartedAfter(info, current *MachineInfo) bool {
	if current == nil || info == current {
		return true
	}
	if info.gatherSeq != 0 && current.gatherSeq != 0 {
		return info.gatherSeq > current.gatherSeq
	}
	curAt, ok := machineInfoCollectedAt(current)
	if !ok {
		return true // nothing to order against
	}
	newAt, ok := machineInfoCollectedAt(info)
	return ok && newAt.After(curAt)
}

// storeMachineInfoIfNotOlder stores info unless the cache already holds a
// gather that started later (machineInfoStartedAfter). A gather can finish
// after a newer one was cached — an inspection's gather left running past its
// deadline, or a periodic gather overlapping a refresh; the late one must not
// roll the cache back or fire the post-store hook with older details. The
// check and the swap happen under one lock. Reports whether it stored.
func storeMachineInfoIfNotOlder(info *MachineInfo) bool {
	if info == nil {
		return false
	}
	machineInfoMu.Lock()
	if !machineInfoStartedAfter(info, machineInfoCache) {
		machineInfoMu.Unlock()
		return false
	}
	machineInfoCache = info
	machineInfoMu.Unlock()
	machineInfoStored(info)
	return true
}

// drainMachineInfoGathers waits until no background gather is running, or
// until timeout passes, and reports whether it drained. Test seam: a fixture
// that switches process-global state a gather's parsers read must call it
// first.
func drainMachineInfoGathers(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		machineInfoGathersMu.Lock()
		inFlight := machineInfoGathersInFlight
		machineInfoGathersMu.Unlock()
		if inFlight == 0 {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// StartMachineInfoGathering launches the background goroutine that
// populates the cache. Safe to call once at startup; the loop runs for the
// process lifetime, refreshing every machineInfoRefreshInterval.
//
// First gather runs immediately (not after the first sleep). It usually beats
// the first /auth/token request; when it does not, storeMachineInfo re-sends
// that request once the data exists.
func StartMachineInfoGathering() {
	go func() {
		for {
			storeMachineInfoIfNotOlder(gatherMachineInfo())
			// The setupToolCatalog pass runs AFTER the gather is stored, never
			// inside it, so it cannot delay the data the first /auth/token
			// request is waiting for (setup_tool_catalog.go).
			refreshSetupToolProbesInCache()
			time.Sleep(machineInfoRefreshInterval)
		}
	}()
}

/* --------------------------------------------------------------------------
   Gather
   -------------------------------------------------------------------------- */

func gatherMachineInfo() *MachineInfo {
	return gatherMachineInfoBounded(0)
}

// inspectionCLIUsageBudget bounds the CLI usage pass inside an inspection's
// gather (__env_inspect__). That pass is the slow part of a gather: every
// detected CLI's usage parser runs one after another, several with network
// reads (Claude usage probe, Grok billing + login renew, Antigravity Code
// Assist) and model discovery, so with six CLIs it alone can outlast the 20 s
// inspection budget. Readiness reads none of it (evaluateReadiness looks only
// at hardware, tools and which CLIs are detected), so an inspection waits this
// long for it and otherwise reports the last known usage per CLI.
const inspectionCLIUsageBudget = 8 * time.Second

// gatherMachineInfoBounded is the full gather. usageBudget > 0 bounds how long
// it waits for the CLI usage pass (see inspectionCLIUsageBudget); 0 waits for
// it, which the periodic gather does.
func gatherMachineInfoBounded(usageBudget time.Duration) *MachineInfo {
	// A tool installed since the last gather (a setup step, or the user) must be
	// visible to the probes below without an agent restart (path_refresh.go).
	refreshCommandPath()

	info := &MachineInfo{
		Architecture:    runtime.GOARCH,
		Runtimes:        map[string]string{},
		PackageManagers: map[string]string{},
		Tools:           map[string]string{},
		CollectedAt:     time.Now().UTC().Format(time.RFC3339),
		gatherSeq:       nextMachineInfoGatherSeq(),
	}

	// The setup-checklist probes (gh / firebase / terraform, the OS package
	// managers, Windows virtualization + nvidia-smi VRAM) run on their own
	// goroutines while the serial gather below proceeds, and are joined at the
	// end, so they add no wall time to the gather (systemInfo_setup.go).
	extrasCh := make(chan setupExtras, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*setupProbeTimeout)
		defer cancel()
		extrasCh <- gatherSetupExtras(ctx, currentGOOS(), runSetupProbe)
	}()

	// Protocol capabilities first, before any probe can fail — see
	// capabilitiesInfo. The concurrency hints are merged in at the end.
	info.Capabilities = newMachineCapabilities()

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

	// CLI agents — database-backed catalog entries when the backend provides
	// them, with a built-in default catalog for older services. exec.LookPath
	// honors the PATH that tray_darwin.go's init() augmented with
	// /opt/homebrew/bin etc., so brew-installed agents are visible.
	info.DetectedCliAgents = gatherCLIAgents()
	// Richer per-provider utilization snapshot — feeds the CLI Agents tab.
	// Stable order so byte-equal Firestore payloads produce a delta-skip on
	// the terminal-service side rather than a write per gather cycle. It runs
	// beside the GPU / battery / live-load probes below and is joined after
	// them (bounded by usageBudget when one is set).
	usageNow := time.Now()
	usageStarted := usageNow
	usageCh := make(chan []cliAgentUsage, 1)
	releaseUsage := trackMachineInfoGather()
	go func(detected map[string]detectedCLIAgent) {
		defer releaseUsage()
		usageCh <- gatherCLIAgentUsage(detected, usageNow)
	}(info.DetectedCliAgents)

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

	info.CliAgents = awaitCLIAgentUsage(usageCh, usageStarted, usageBudget, info.DetectedCliAgents, usageNow)

	// Join the parallel setup probes (normally long finished by now), then
	// record the gather's own tools before folding in the last setupToolCatalog
	// pass (no spawn here — see setup_tool_catalog.go).
	applySetupExtras(info, <-extrasCh)
	info.baseTools = copyStringMap(info.Tools)
	withSetupToolResults(info, activeSetupToolCatalog(), cachedSetupToolResults())

	// Capability hints — cheap derivation from the gathered numbers; the
	// LLM uses these to decide whether to run tests/builds in parallel.
	// A failed CPU or memory probe leaves only the protocol flags set.
	applyConcurrencyHints(info.Capabilities, info.CPU, info.Memory)

	return info
}

// awaitCLIAgentUsage joins the gather's CLI usage pass. With no budget it
// waits for it. With one, a pass still running at usageStarted+budget is left
// to finish on its own (still counted in machineInfoGathersInFlight; its
// result is dropped, since the usage refresh paths keep usage current) and
// the gather reports the last known usage instead (cliAgentUsageFallback).
func awaitCLIAgentUsage(usageCh <-chan []cliAgentUsage, usageStarted time.Time, budget time.Duration, detected map[string]detectedCLIAgent, now time.Time) []cliAgentUsage {
	if budget <= 0 {
		return <-usageCh
	}
	remaining := budget - time.Since(usageStarted)
	if remaining <= 0 {
		select {
		case usage := <-usageCh:
			return usage
		default:
		}
	} else {
		timer := time.NewTimer(remaining)
		defer timer.Stop()
		select {
		case usage := <-usageCh:
			return usage
		case <-timer.C:
		}
	}
	envInspectLogf(colorYellow, "CLI usage pass still running after %s; reporting the last known usage per CLI", budget)
	return cliAgentUsageFallback(detected, GetMachineInfo(), now)
}

// cliAgentUsageFallback is the CliAgents slice for a gather whose usage pass
// missed its budget: for each detected CLI, the entry the cache already holds
// (the last full gather, kept current by the usage refresh paths), or else the
// same baseline entry gatherCLIAgentUsage emits for a CLI its parser could not
// read. Same order as gatherCLIAgentUsage.
func cliAgentUsageFallback(detected map[string]detectedCLIAgent, cached *MachineInfo, now time.Time) []cliAgentUsage {
	out := []cliAgentUsage{}
	if len(detected) == 0 {
		return out
	}
	known := map[string]cliAgentUsage{}
	if cached != nil {
		machineInfoMu.RLock()
		for _, u := range cached.CliAgents {
			if u.CliAgentID != "" {
				known[u.CliAgentID] = u
			}
		}
		machineInfoMu.RUnlock()
	}
	host, err := os.Hostname()
	if err != nil {
		host = ""
	}
	parsers := cliAgentUsageParserIndex()
	for _, agent := range activeCLIAgentCatalog() {
		if !cliAgentCatalogSupportsUtilization(agent) {
			continue
		}
		entry, ok := detected[agent.ID]
		if !ok || !entry.Detected {
			continue
		}
		if u, ok := known[agent.ID]; ok {
			// Usage and account are last known; where the CLI is and which
			// version it is are what this gather just detected.
			if entry.Version != "" {
				u.Version = entry.Version
			}
			if entry.Path != "" {
				u.Path = entry.Path
			}
			out = append(out, u)
			continue
		}
		provider := agent.ID
		if parser := parsers[cliAgentCatalogParserKey(agent)]; parser != nil {
			provider = parser.Provider()
		}
		u := cliAgentUsage{
			CliAgentID:  agent.ID,
			Provider:    provider,
			Name:        entry.Name,
			Version:     entry.Version,
			Path:        entry.Path,
			CollectedAt: now.UTC().Format(time.RFC3339),
		}
		u.AccountFingerprint = fallbackUnknownAccountFingerprint(u.Provider, host, entry)
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
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
	return probeVersionArgsWithEnv(cmd, nil, args...)
}

// probeVersionArgsWithEnv is the environment-controlled variant used by
// callers that must not let a diagnostic probe inherit agent-specific routing,
// logging, or extension-discovery overrides. A nil env preserves the ordinary
// os.Environ inheritance used by machine-info probes.
func probeVersionArgsWithEnv(cmd string, env []string, args ...string) string {
	if _, err := exec.LookPath(cmd); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), machineInfoProbeTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, cmd, args...)
	hideWindow(c) // prevent console flash on Windows
	if env != nil {
		c.Env = env
	}
	out, err := c.CombinedOutput()
	if err != nil {
		return ""
	}
	// Some tools print version on stderr (java) and others print multiple
	// lines (`go version go1.x` is one line; `bash --version` is many).
	// First non-empty line is what we want.
	return firstNonEmptyLine(out)
}

// firstNonEmptyLine returns the first non-blank, trimmed line of a probe's
// combined output, or "" when there is none.
func firstNonEmptyLine(out []byte) string {
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
	for _, a := range activeCLIAgentCatalog() {
		path, err := exec.LookPath(a.Command)
		if err != nil {
			// Several CLI agents ship an official installer that drops the
			// binary in a per-user directory and only updates shell rc files
			// when it cannot symlink into an already-on-PATH directory. macOS
			// GUI/launchd agents inherit a sparse PATH that frequently excludes
			// those dirs, so a perfectly functional install would otherwise
			// report missing — and the later native start would fail via the
			// same lookup. Probe the installer's default bin dir before giving
			// up. See installerBinDirFallbacks.
			fallback := resolveInstallerBinary(a.Command, installerBinDirFor(a.Command))
			if fallback == "" {
				continue
			}
			path = fallback
		}
		entry := detectedCLIAgent{
			Detected: true,
			Path:     path,
			Name:     a.DisplayName,
		}
		// Probe via the resolved absolute path so fallback-located binaries
		// (e.g. grok in ~/.grok/bin) still report a version; probeVersionArgs
		// runs `exec.LookPath` internally and that accepts absolute paths.
		var version string
		if a.ID == "grok" {
			// The one shim-aware Grok probe (cliagent_smoke_grok.go). A
			// maintenance cycle refreshes usage immediately after its smoke, so
			// this --version child stays on the same deny-by-default env policy
			// — inherited RUST_LOG/GROK_LOG_FILE cannot add raw diagnostics or
			// persist them while the signed usage result is being assembled —
			// and on Windows an npm `grok.cmd` shim goes through cmd.exe rather
			// than caching a failed direct launch's "" under the shared
			// (path, mtime, size) key the smoke's own precheck reads.
			version = grokProbeVersion(path)
		} else if a.ID == "codex" {
			// The one shim-aware Codex probe (cliagent_smoke_codex.go), for the
			// same reason: a plain spawn of an npm `codex.cmd` would cache ""
			// under the shared key and pin the smoke's binary_missing.
			version = codexProbeVersion(path)
		} else if a.ID == "museCode" {
			// Muse Code's Windows `muse.cmd` is a PowerShell launcher that never
			// changes across upgrades, so the shared (path, mtime, size) key
			// would pin a cold-start timeout's "" (or a pre-upgrade version)
			// for good. See museCodeProbeVersion.
			version = museCodeProbeVersion(path)
		} else if a.ID == "opencode" {
			// The shim-aware OpenCode probe (cliagent_smoke_opencode.go): a plain
			// spawn of an npm `opencode.cmd` would cache "" under the shared key
			// the smoke and native capability check read, reporting the CLI as
			// missing.
			version = openCodeProbeVersion(path)
		} else {
			version = cachedProbeVersion(path)
		}
		if version != "" {
			entry.Version = version
		}
		agents[a.ID] = entry
	}
	return agents
}

/* --------------------------------------------------------------------------
   Installer bin-dir fallbacks
   -------------------------------------------------------------------------- */

// installerBinDirFallbacks maps a catalog command to the directory its official
// installer writes the binary to when it cannot symlink onto PATH, plus the
// environment variable that overrides that directory (empty when the installer
// has none). Table-driven rather than a chain of `if command == ...` branches so
// a sixth agent with the same installer shape is one row, not a new branch.
//
//   - grok:     https://x.ai/cli/install.sh writes $GROK_BIN_DIR else
//     $HOME/.grok/bin (%USERPROFILE%\.grok\bin on Windows).
//   - opencode: the official install script writes $HOME/.opencode/bin, which
//     macOS launchd/GUI-spawned agents do not inherit -- exactly the failure the
//     grok fallback exists for.
//   - muse:     https://dev.meta.ai/install.sh writes $MUSE_INSTALL_DIR else
//     $HOME/.local/bin; install.ps1 writes $MUSE_INSTALL_DIR else
//     %LOCALAPPDATA%\Programs\muse, hence the Windows-specific root.
var installerBinDirFallbacks = map[string]struct {
	EnvVar string
	Rel    []string
	// WindowsLocalAppDataRel, when set, replaces Rel on Windows and is
	// resolved under %LOCALAPPDATA% instead of the home directory.
	WindowsLocalAppDataRel []string
}{
	"grok":     {EnvVar: "GROK_BIN_DIR", Rel: []string{".grok", "bin"}},
	"opencode": {EnvVar: "", Rel: []string{".opencode", "bin"}},
	"muse":     {EnvVar: "MUSE_INSTALL_DIR", Rel: []string{".local", "bin"}, WindowsLocalAppDataRel: []string{"Programs", "muse"}},
}

// installerBinDirFor returns the installer bin dir for a command, or "" when
// the command has no known installer fallback (the common case).
func installerBinDirFor(command string) string {
	spec, ok := installerBinDirFallbacks[commandBaseName(command)]
	if !ok {
		return ""
	}
	if spec.EnvVar != "" {
		if d := strings.TrimSpace(os.Getenv(spec.EnvVar)); d != "" {
			return d
		}
	}
	if runtime.GOOS == "windows" && len(spec.WindowsLocalAppDataRel) > 0 {
		base := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
		if base == "" {
			return ""
		}
		return filepath.Join(append([]string{base}, spec.WindowsLocalAppDataRel...)...)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(append([]string{home}, spec.Rel...)...)
}

// resolveInstallerBinary returns the absolute path to `command` inside dir, or
// "" when dir is empty or holds no such executable. PATH-independent fallback
// used by both CLI detection and the native managers so a launchd/GUI-spawned
// agent process with a sparse PATH still finds the user's install.
func resolveInstallerBinary(command, dir string) string {
	if dir == "" || command == "" {
		return ""
	}
	base := commandBaseName(command)
	candidates := []string{base}
	if runtime.GOOS == "windows" {
		// The installers drop a native .exe; keep the extensionless spelling as
		// a later candidate for shim-style installs.
		candidates = []string{base + ".exe", base + ".cmd", base}
	}
	for _, name := range candidates {
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

// resolveGrokInstallerBinary returns the absolute path to a `grok` binary
// installed by the official installer, or "" when none exists. Thin wrapper
// over the generalized table above so the Grok ACP manager's existing call
// site keeps working.
func resolveGrokInstallerBinary() string {
	return resolveInstallerBinary("grok", installerBinDirFor("grok"))
}

// resolveOpenCodeInstallerBinary returns the absolute path to an `opencode`
// binary installed by the official install script, or "" when none exists.
// Used by the OpenCode native manager for the same reason the Grok one exists.
func resolveOpenCodeInstallerBinary() string {
	return resolveInstallerBinary("opencode", installerBinDirFor("opencode"))
}

/* --------------------------------------------------------------------------
   Version-probe cache
   -------------------------------------------------------------------------- */

// gatherCLIAgents runs one `<bin> --version` child per catalog entry on every
// machine-info gather AND every __cli_usage_refresh__ wake, all inside the
// single 10-second GatherCLIAgentUsageOnly context. On Windows with real-time
// AV, a cold --version on a large compiled binary runs in the hundreds of
// milliseconds, so at 5-6 agents the serialized probes consume a visible
// fraction of that budget and produce intermittent "no utilization reported"
// cards.
//
// A version string is a pure function of the binary, so the probe is cached on
// (path, mtime, size): a refresh cycle re-probes only when the binary actually
// changed (upgrade, reinstall). This is deliberately NOT the right key for
// readiness/auth probes, which change without the binary changing -- see
// cliagent_usage_opencode.go, which uses a short time-based TTL instead.
type versionProbeKey struct {
	Path    string
	ModUnix int64
	Size    int64
}

var (
	versionProbeMu    sync.Mutex
	versionProbeCache = map[versionProbeKey]string{}
)

// cachedProbeVersion returns `<path> --version` output, reusing a prior result
// when the binary is unchanged by (path, mtime, size). Falls through to an
// uncached probe when the binary cannot be stat'ed.
func cachedProbeVersion(path string) string {
	return cachedProbeVersionWithEnv(path, nil)
}

func cachedProbeVersionWithEnv(path string, env []string) string {
	return cachedProbeVersionFunc(path, func() string {
		return probeVersionArgsWithEnv(path, env, "--version")
	})
}

// peekCachedProbeVersion returns a version cachedProbeVersion already recorded
// for the binary as it is now (same path, mtime, size), without spawning
// anything. ok is false when no probe of this exact binary is cached.
func peekCachedProbeVersion(path string) (string, bool) {
	v, _, ok := peekCachedProbeVersionIdentity(path)
	return v, ok
}

// peekCachedProbeVersionIdentity is peekCachedProbeVersion plus the identity
// the reading was validated against, so a caller that KEEPS the version past
// the peek can re-check later whether that exact binary is still on disk. An
// installer never participates in any of our locks, so identity is the only
// thing that survives the interval.
func peekCachedProbeVersionIdentity(path string) (string, versionProbeKey, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return "", versionProbeKey{}, false
	}
	key := versionProbeKey{Path: path, ModUnix: info.ModTime().UnixNano(), Size: info.Size()}
	versionProbeMu.Lock()
	defer versionProbeMu.Unlock()
	v, ok := versionProbeCache[key]
	return v, key, ok
}

// versionProbeIdentityCurrent reports whether the binary key was recorded for
// is still the file on disk. A zero key (no identity was ever validated) is
// not current: there is nothing to confirm.
func versionProbeIdentityCurrent(key versionProbeKey) bool {
	if key.Path == "" {
		return false
	}
	info, err := os.Stat(key.Path)
	return err == nil && info.ModTime().UnixNano() == key.ModUnix && info.Size() == key.Size
}

// cachedProbeVersionFunc is the caching half on its own, for a caller whose
// binary cannot be spawned by a plain exec.Command -- notably the Grok
// maintenance smoke on Windows, where an npm `grok.cmd` shim has to go through
// cmd.exe. The cache key and pruning rule are identical; only how the version
// is obtained differs.
func cachedProbeVersionFunc(path string, probe func() string) string {
	info, err := os.Stat(path)
	if err != nil {
		return probe()
	}
	key := versionProbeKey{Path: path, ModUnix: info.ModTime().UnixNano(), Size: info.Size()}

	versionProbeMu.Lock()
	cached, ok := versionProbeCache[key]
	versionProbeMu.Unlock()
	if ok {
		return cached
	}

	v := probe()
	// Re-stat before touching the map. A probe that raced a binary replacement
	// answers for a file that is no longer there, so neither its value nor its
	// pruning may land: storing it would key an installed-binary lookup to a
	// version that is gone, and the prune below would DELETE the entry a
	// concurrent probe of the new build had already recorded. Return the reading
	// to this caller uncached and leave the map describing only what is on disk.
	if after, err := os.Stat(path); err != nil ||
		after.ModTime().UnixNano() != key.ModUnix || after.Size() != key.Size {
		return v
	}
	versionProbeMu.Lock()
	// Cache negatives too: a binary that reliably fails --version would
	// otherwise re-spawn a doomed child on every single gather, which is the
	// exact cost this cache exists to remove. A reinstall changes mtime and
	// invalidates the entry. Drop stale entries for the same path first, since
	// an upgrade leaves the old (mtime,size) key behind and nothing else prunes
	// this map.
	for k := range versionProbeCache {
		if k.Path == path && k != key {
			delete(versionProbeCache, k)
		}
	}
	versionProbeCache[key] = v
	versionProbeMu.Unlock()
	return v
}

// lookupCachedProbeVersion returns the version a prior cachedProbeVersion
// recorded for the binary as it is on disk now, without spawning anything. ok
// is false when the binary cannot be stat'ed or has changed (an upgrade leaves
// a new mtime/size) since it was last probed.
func lookupCachedProbeVersion(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	key := versionProbeKey{Path: path, ModUnix: info.ModTime().UnixNano(), Size: info.Size()}
	versionProbeMu.Lock()
	defer versionProbeMu.Unlock()
	v, ok := versionProbeCache[key]
	return v, ok
}

// forgetCachedProbeVersion drops every cached reading for a path, so the next
// cachedProbeVersion* call re-probes the binary as it is now. The cache stores
// FAILURES too (a reliably dead binary must not re-spawn a doomed child on
// every gather), which is right for a version that is a pure function of the
// bytes on disk and wrong for a reading that failed for a reason the bytes do
// not explain -- a launcher that was momentarily unable to start the child.
// A caller that can tell those apart calls this to let its negative expire; see
// openCodeProbeVersion.
func forgetCachedProbeVersion(path string) {
	if path == "" {
		return
	}
	versionProbeMu.Lock()
	for k := range versionProbeCache {
		if k.Path == path {
			delete(versionProbeCache, k)
		}
	}
	versionProbeMu.Unlock()
}

// resetVersionProbeCache clears the cache. Test-only seam so a case that
// rewrites a stub binary in place can assert re-probe behaviour deterministically.
func resetVersionProbeCache() {
	versionProbeMu.Lock()
	versionProbeCache = map[versionProbeKey]string{}
	versionProbeMu.Unlock()
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
