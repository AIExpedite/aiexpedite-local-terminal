// File: systemInfo_setup.go
// -----------------------------------------------------------------------------
// Machine-info probes added for the computer setup checklist
// (COMPUTER_SETUP_CHECKLIST_PLAN.md §6 / §9 agent row, Phase 0):
//
//   - tools:            gh, firebase, terraform
//   - packageManagers:  winget / choco / scoop (Windows), brew (macOS, Linux),
//     apt (Linux)
//   - virtualization:   { enabled, wsl2 } on Windows (absent elsewhere)
//   - GPU VRAM:         nvidia-smi overrides the WMI AdapterRAM value on
//     Windows, which a uint32 caps at ~4 GB
//
// Timing. The first gather races the first /auth/token request
// (auth.go tokenSourceAwaitingMachineInfo), so none of these may lengthen it.
// They run on their own goroutines — bounded parallelism, 5 s per probe —
// started at the top of gatherMachineInfo and joined at the end, i.e. in the
// shadow of the existing serial gather (a 1 s CPU sample alone, plus a dozen
// serial version probes and three PowerShell/WMI spawns on Windows).
//
// The catalog-driven probes (setupToolCatalog from /auth/token) are NOT part of
// the gather at all — see setup_tool_catalog.go for why.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

// setupProbeTimeout bounds one setup probe. Longer than machineInfoProbeTimeout:
// firebase / codex are npm shims whose cold Node start on Windows with
// real-time AV routinely takes 1-3 s.
const setupProbeTimeout = 5 * time.Second

// setupProbeWorkers caps concurrent probe children.
const setupProbeWorkers = 4

// setupProbePresent is the value reported for a tool whose presence is known
// but whose version cannot be read cheaply (scoop's --version runs git).
const setupProbePresent = "present"

// maxProbeLineLength caps one reported version line.
const maxProbeLineLength = 200

// virtualizationInfo is the Windows virtualization readiness Docker Desktop
// (WSL2) needs. nil fields mean "could not tell".
type virtualizationInfo struct {
	// Enabled: hardware virtualization is usable — a hypervisor is running
	// (Hyper-V / WSL2 / VBS), or the CPU reports it enabled in firmware.
	Enabled *bool `json:"enabled"`
	// WSL2: `wsl --status` succeeds and does not report WSL 1 as the default.
	WSL2 *bool `json:"wsl2"`
}

// setupProbeSpec is one fixed probe of the gather.
type setupProbeSpec struct {
	Target       string // "tools" | "packageManagers"
	Key          string
	Cmd          string
	Args         []string
	Env          []string // extra environment (appended to os.Environ())
	PresenceOnly bool     // report setupProbePresent when the command resolves; never spawn
}

// Quiet the update checks two of these CLIs make on every invocation: Terraform
// phones HashiCorp's checkpoint service, firebase-tools runs update-notifier.
var (
	terraformProbeEnv = []string{"CHECKPOINT_DISABLE=1"}
	nodeCLIProbeEnv   = []string{"NO_UPDATE_NOTIFIER=1"}
)

// setupProbeSpecsFor lists the fixed setup probes for goos.
func setupProbeSpecsFor(goos string) []setupProbeSpec {
	specs := []setupProbeSpec{
		{Target: "tools", Key: "gh", Cmd: "gh", Args: []string{"--version"}},
		{Target: "tools", Key: "firebase", Cmd: "firebase", Args: []string{"--version"}, Env: nodeCLIProbeEnv},
		{Target: "tools", Key: "terraform", Cmd: "terraform", Args: []string{"--version"}, Env: terraformProbeEnv},
	}
	switch goos {
	case "windows":
		specs = append(specs,
			setupProbeSpec{Target: "packageManagers", Key: "winget", Cmd: "winget", Args: []string{"--version"}},
			setupProbeSpec{Target: "packageManagers", Key: "choco", Cmd: "choco", Args: []string{"--version"}},
			setupProbeSpec{Target: "packageManagers", Key: "scoop", Cmd: "scoop", PresenceOnly: true},
		)
	case "darwin":
		specs = append(specs,
			setupProbeSpec{Target: "packageManagers", Key: "brew", Cmd: "brew", Args: []string{"--version"}, Env: []string{"HOMEBREW_NO_AUTO_UPDATE=1"}},
		)
	case "linux":
		specs = append(specs,
			setupProbeSpec{Target: "packageManagers", Key: "apt", Cmd: "apt", Args: []string{"--version"}},
			setupProbeSpec{Target: "packageManagers", Key: "brew", Cmd: "brew", Args: []string{"--version"}, Env: []string{"HOMEBREW_NO_AUTO_UPDATE=1"}},
		)
	}
	return specs
}

// setupProbeRunner runs `cmd args…` with a timeout and returns its combined
// output; ok is false when the command does not resolve, fails, or times out.
type setupProbeRunner func(ctx context.Context, cmd string, args, env []string, timeout time.Duration) (out string, ok bool)

// setupProbeLookPath resolves a probe's command. Seam for tests.
var setupProbeLookPath = exec.LookPath

// runSetupProbe is the production setupProbeRunner.
var runSetupProbe setupProbeRunner = func(ctx context.Context, cmd string, args, env []string, timeout time.Duration) (string, bool) {
	path, err := setupProbeLookPath(cmd)
	if err != nil {
		return "", false
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(pctx, path, args...)
	hideWindow(c)
	if len(env) > 0 {
		c.Env = append(os.Environ(), env...)
	}
	// An npm .cmd shim's Node grandchild can hold the output pipes after the
	// timeout kills cmd.exe; WaitDelay bounds the wait for them.
	c.WaitDelay = time.Second
	out, err := c.CombinedOutput()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// runProbesParallel runs n jobs with at most workers in flight and returns when
// all have finished or ctx is done (jobs still running then are abandoned; each
// is individually time-bounded).
func runProbesParallel(ctx context.Context, n, workers int, job func(ctx context.Context, i int)) {
	if n == 0 {
		return
	}
	if workers < 1 {
		workers = 1
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			job(ctx, i)
		}(i)
	}
	wg.Wait()
}

// selectVersionLine picks the line to report from a probe's output: the first
// non-empty line, unless a versionPattern is given that does not match it but
// does match a later line — then that line. It always reports a raw line (never
// a capture group), so the server applies the same versionPattern whether the
// value came from this probe, an older agent's first-line probe, or the base
// gather. Capped at maxProbeLineLength.
func selectVersionLine(out, pattern string) string {
	var first string
	var re *regexp.Regexp
	if pattern != "" {
		re, _ = regexp.Compile(pattern) // a pattern Go can't compile (JS-only syntax) falls back to the first line
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if first == "" {
			first = line
			if re == nil || re.MatchString(line) {
				break
			}
			continue
		}
		if re.MatchString(line) {
			return truncateProbeLine(line)
		}
	}
	return truncateProbeLine(first)
}

func truncateProbeLine(s string) string {
	if len(s) > maxProbeLineLength {
		return s[:maxProbeLineLength]
	}
	return s
}

// setupExtras is what the parallel setup probes found.
type setupExtras struct {
	Tools           map[string]string
	PackageManagers map[string]string
	Virtualization  *virtualizationInfo
	NvidiaGPUs      []gpuInfo // Windows only; nil when nvidia-smi is absent
}

// gatherSetupExtras runs the fixed setup probes for goos in parallel.
func gatherSetupExtras(ctx context.Context, goos string, run setupProbeRunner) setupExtras {
	specs := setupProbeSpecsFor(goos)
	values := make([]string, len(specs))

	type extraJob func(ctx context.Context)
	var extra []extraJob
	var virt *virtualizationInfo
	var nvidia []gpuInfo
	if goos == "windows" {
		extra = append(extra,
			func(ctx context.Context) { virt = gatherVirtualizationWindows(ctx, run) },
			func(ctx context.Context) {
				out, ok := run(ctx, "nvidia-smi", []string{
					"--query-gpu=name,memory.total,driver_version", "--format=csv,noheader,nounits",
				}, nil, setupProbeTimeout)
				if ok {
					nvidia = parseNvidiaSmiGPUs(strings.TrimSpace(out))
				}
			},
		)
	}

	runProbesParallel(ctx, len(specs)+len(extra), setupProbeWorkers, func(ctx context.Context, i int) {
		if i >= len(specs) {
			extra[i-len(specs)](ctx)
			return
		}
		s := specs[i]
		if s.PresenceOnly {
			if _, err := setupProbeLookPath(s.Cmd); err == nil {
				values[i] = setupProbePresent
			}
			return
		}
		if out, ok := run(ctx, s.Cmd, s.Args, s.Env, setupProbeTimeout); ok {
			values[i] = selectVersionLine(out, "")
		}
	})

	res := setupExtras{
		Tools:           map[string]string{},
		PackageManagers: map[string]string{},
		Virtualization:  virt,
		NvidiaGPUs:      nvidia,
	}
	for i, s := range specs {
		if values[i] == "" {
			continue
		}
		if s.Target == "packageManagers" {
			res.PackageManagers[s.Key] = values[i]
		} else {
			res.Tools[s.Key] = values[i]
		}
	}
	return res
}

// applySetupExtras merges the extras into info. Existing keys win (the base
// gather's own probes are authoritative for the keys it owns).
func applySetupExtras(info *MachineInfo, x setupExtras) {
	if info == nil {
		return
	}
	if info.Tools == nil {
		info.Tools = map[string]string{}
	}
	if info.PackageManagers == nil {
		info.PackageManagers = map[string]string{}
	}
	for k, v := range x.Tools {
		if _, ok := info.Tools[k]; !ok {
			info.Tools[k] = v
		}
	}
	for k, v := range x.PackageManagers {
		if _, ok := info.PackageManagers[k]; !ok {
			info.PackageManagers[k] = v
		}
	}
	if x.Virtualization != nil {
		info.Virtualization = x.Virtualization
	}
	if len(x.NvidiaGPUs) > 0 {
		info.GPU = applyNvidiaVRAM(info.GPU, x.NvidiaGPUs)
	}
}

// applyNvidiaVRAM corrects NVIDIA adapters' VRAM with nvidia-smi's readings.
// WMI's AdapterRAM is a uint32, so every card with more than 4 GB reads ~4 GB;
// nvidia-smi reports the real total. Each NVIDIA WMI entry takes the nvidia-smi
// entry with the same name, else the next unused one in order. nvidia-smi
// entries no WMI entry claimed are appended (WMI failed or missed the card).
func applyNvidiaVRAM(wmi, nvidia []gpuInfo) []gpuInfo {
	if len(nvidia) == 0 {
		return wmi
	}
	used := make([]bool, len(nvidia))
	out := make([]gpuInfo, len(wmi))
	copy(out, wmi)
	claim := func(i int) (gpuInfo, bool) {
		name := strings.ToLower(strings.TrimSpace(out[i].Name))
		for j, n := range nvidia {
			if !used[j] && strings.ToLower(strings.TrimSpace(n.Name)) == name {
				used[j] = true
				return n, true
			}
		}
		for j, n := range nvidia {
			if !used[j] {
				used[j] = true
				return n, true
			}
		}
		return gpuInfo{}, false
	}
	for i := range out {
		if out[i].Vendor != "nvidia" {
			continue
		}
		if n, ok := claim(i); ok && n.MemoryGB > 0 {
			out[i].MemoryGB = n.MemoryGB
		}
	}
	for j, n := range nvidia {
		if !used[j] {
			out = append(out, n)
		}
	}
	return out
}

/* --------------------------------------------------------------------------
   Windows virtualization
   -------------------------------------------------------------------------- */

// windowsVirtualizationScript reads the hypervisor flag and the CPUs' firmware
// virtualization flag in one PowerShell start. VirtualizationFirmwareEnabled
// reads false while Hyper-V itself is running (the hypervisor hides VT-x from
// the root partition), which is why HypervisorPresent is read too.
const windowsVirtualizationScript = `$cs = Get-CimInstance Win32_ComputerSystem; $p = @(Get-CimInstance Win32_Processor); ConvertTo-Json -Compress -InputObject @{ h = $cs.HypervisorPresent; f = @($p | ForEach-Object { $_.VirtualizationFirmwareEnabled }) }`

func gatherVirtualizationWindows(ctx context.Context, run setupProbeRunner) *virtualizationInfo {
	v := &virtualizationInfo{}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		out, ok := run(ctx, "powershell", []string{"-NoProfile", "-NonInteractive", "-Command", windowsVirtualizationScript}, nil, setupProbeTimeout)
		if ok {
			v.Enabled = parseWindowsVirtualizationJSON(out)
		}
	}()
	go func() {
		defer wg.Done()
		v.WSL2 = wslStatusWSL2(run(ctx, "wsl", []string{"--status"}, nil, setupProbeTimeout))
	}()
	wg.Wait()
	if v.Enabled == nil && v.WSL2 == nil {
		return nil
	}
	return v
}

// parseWindowsVirtualizationJSON reads windowsVirtualizationScript's output:
// true when a hypervisor is present or any CPU reports firmware virtualization
// enabled; false when both are known and neither holds; nil when unknown.
func parseWindowsVirtualizationJSON(out string) *bool {
	var raw struct {
		H *bool           `json:"h"`
		F json.RawMessage `json:"f"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &raw); err != nil {
		return nil
	}
	var firmware []*bool
	if len(raw.F) > 0 {
		if err := json.Unmarshal(raw.F, &firmware); err != nil {
			var single *bool
			if json.Unmarshal(raw.F, &single) == nil {
				firmware = []*bool{single}
			}
		}
	}
	yes, no := true, false
	if raw.H != nil && *raw.H {
		return &yes
	}
	knownFirmware := false
	for _, f := range firmware {
		if f == nil {
			continue
		}
		knownFirmware = true
		if *f {
			return &yes
		}
	}
	if raw.H != nil && knownFirmware {
		return &no
	}
	return nil
}

// wslStatusWSL2 interprets `wsl --status`: a failing or missing wsl.exe means
// WSL2 is not set up (false); success means it is, unless the output names
// WSL 1 as the default version. The output is UTF-16LE on Windows.
func wslStatusWSL2(out string, ok bool) *bool {
	yes, no := true, false
	if !ok {
		return &no
	}
	text := decodeMaybeUTF16LE([]byte(out))
	if regexp.MustCompile(`(?i)default version:\s*1\b`).MatchString(text) {
		return &no
	}
	return &yes
}

// decodeMaybeUTF16LE decodes b as UTF-16LE when it looks like it (a BOM, or NUL
// bytes in odd positions), else returns it as-is. wsl.exe writes UTF-16LE.
func decodeMaybeUTF16LE(b []byte) string {
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE {
		b = b[2:]
	} else {
		zeros := 0
		for i := 1; i < len(b); i += 2 {
			if b[i] == 0 {
				zeros++
			}
		}
		if len(b) < 2 || zeros*2 < len(b)/2 {
			return string(b)
		}
	}
	if len(b)%2 == 1 {
		b = b[:len(b)-1]
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(u))
}

// currentGOOS is runtime.GOOS behind a seam so gather tests can pick a platform.
var currentGOOS = func() string { return runtime.GOOS }
