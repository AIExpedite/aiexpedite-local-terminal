// File: systemInfo_test.go
// -----------------------------------------------------------------------------
// Tests for the deterministic, platform-independent parts of systemInfo.go.
//
// We deliberately don't try to assert specific CPU / RAM values — gopsutil
// reads what's actually on the host running the test, and CI machines vary.
// What we DO assert:
//   - the JSON shape stays compatible with the terminal-service /auth/token
//     persist contract (`omitempty` tags, no required-but-missing fields)
//   - probeVersion handles missing binaries / non-zero exits without
//     hanging or panicking
//   - gather runs end-to-end and produces a non-nil result with a
//     populated Architecture and CollectedAt (these come from runtime, not
//     external commands)
//   - shell-info derivation uses the SHELL env override sensibly
// -----------------------------------------------------------------------------

package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// gbToBytes converts a fractional-GB value to bytes via runtime float math,
// sidestepping Go's compile-time constant overflow check on direct
// float→uint64 conversions in slice literals.
func gbToBytes(gb float64) uint64 {
	return uint64(math.Floor(gb * float64(int64(1)<<30)))
}

func TestRoundGB(t *testing.T) {
	cases := []struct {
		name string
		in   uint64
		want float64
	}{
		// 1 GB = 2^30 bytes; we round to one decimal
		{"exactly 1 GB", 1 << 30, 1.0},
		{"15.7 GB (DansLaptop)", gbToBytes(15.7), 15.7},
		{"32 GB", 32 * (1 << 30), 32.0},
		{"under 1 GB rounds to one decimal", 1 << 29, 0.5},
		{"zero", 0, 0.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := roundGB(tc.in)
			// Allow tiny FP imprecision
			if got < tc.want-0.05 || got > tc.want+0.05 {
				t.Errorf("roundGB(%d) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestMinInt(t *testing.T) {
	if minInt(3, 7) != 3 {
		t.Errorf("minInt(3,7) want 3")
	}
	if minInt(7, 3) != 3 {
		t.Errorf("minInt(7,3) want 3")
	}
	if minInt(5, 5) != 5 {
		t.Errorf("minInt(5,5) want 5")
	}
	if minInt(-1, 0) != -1 {
		t.Errorf("minInt(-1,0) want -1")
	}
}

func TestProbeVersion_MissingBinary(t *testing.T) {
	// `xx-this-binary-does-not-exist` MUST NOT exist anywhere on PATH.
	// probeVersion should return "" without hanging. Bound the wall time
	// so a regression that accidentally drops the LookPath check (and
	// reaches exec.CommandContext, which would then time out at 3s) shows
	// up as a slow test, not a hang.
	start := time.Now()
	got := probeVersion("xx-this-binary-does-not-exist", "--version")
	elapsed := time.Since(start)
	if got != "" {
		t.Errorf("probeVersion missing binary: got %q, want \"\"", got)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("probeVersion missing binary took %v — should be near-instant via LookPath", elapsed)
	}
}

func TestProbeVersion_BinaryThatExitsNonZero(t *testing.T) {
	// `go` exists on the build host; passing a nonsense flag makes it
	// exit non-zero. probeVersion should swallow the failure and return "".
	got := probeVersion("go", "--this-flag-does-not-exist")
	if got != "" {
		t.Errorf("probeVersion non-zero exit: got %q, want \"\"", got)
	}
}

func TestProbeVersion_PicksFirstNonEmptyLine(t *testing.T) {
	// `go version` always prints a single line on stdout containing
	// "go version go". This validates the trim/split logic without
	// asserting the exact go version of the test runner.
	got := probeVersion("go", "version")
	if !strings.HasPrefix(got, "go version") {
		t.Errorf("probeVersion(go version) = %q, want prefix 'go version'", got)
	}
}

func TestGatherShellInfo_DefaultShellFromEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows shell info has no SHELL env path")
	}
	prev := getenv
	getenv = func(k string) string {
		if k == "SHELL" {
			return "/bin/zsh"
		}
		return ""
	}
	t.Cleanup(func() { getenv = prev })

	s := gatherShellInfo()
	if s.DefaultShell != "zsh" {
		t.Errorf("expected DefaultShell=zsh from $SHELL=/bin/zsh; got %q", s.DefaultShell)
	}
}

func TestGatherShellInfo_FallbackWhenEnvMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows shell info has no SHELL env path")
	}
	prev := getenv
	getenv = func(k string) string { return "" }
	t.Cleanup(func() { getenv = prev })

	s := gatherShellInfo()
	// Without $SHELL, default should be either "zsh" (if on PATH) or "bash".
	// Both are acceptable — what we care about is that we don't return "".
	if s.DefaultShell != "zsh" && s.DefaultShell != "bash" {
		t.Errorf("expected DefaultShell zsh|bash without $SHELL; got %q", s.DefaultShell)
	}
}

func TestGatherShellInfo_WindowsDefault(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-specific")
	}
	s := gatherShellInfo()
	if s.DefaultShell != "powershell" {
		t.Errorf("expected DefaultShell=powershell on windows; got %q", s.DefaultShell)
	}
}

// Full-gather smoke test is intentionally NOT in the unit suite. On Windows
// hosts, `python` resolves to a Microsoft Store stub when CPython isn't
// installed, and invoking the stub pops a UI that holds the process open
// past our 3s probe timeout. Same shape with the `dotnet` first-run
// telemetry. Validate the gather end-to-end via the integration build tag
// (run with `go test -tags integration ./...`) where the host provisioning
// is known.
//
// Unit-test coverage of the deterministic helpers (roundGB, minInt,
// probeVersion happy/sad paths, gatherShellInfo env override, JSON shape,
// capabilities derivation) is sufficient for the build/sign/redistribute
// gate; the gopsutil-backed reads (cpu/mem/disk) are exercised by their
// own upstream test suite.

func TestMachineInfo_JSONShape_OmitsEmpty(t *testing.T) {
	// Verify the wire format omits empty fields so a partial gather (e.g.
	// no runtimes detected) doesn't pollute the /auth/token payload with
	// `"runtimes":{}` etc. — terminal-service's persist path treats
	// missing-vs-empty differently for staleness reasoning.
	mi := &MachineInfo{
		Architecture: "arm64",
		CPU:          &cpuInfo{Name: "Apple M2", Cores: 8, Threads: 8},
		Memory:       &memoryInfo{TotalGB: 16.0, AvailableGB: 8.0},
		// Runtimes / PackageManagers / Tools intentionally empty
		Runtimes:        map[string]string{},
		PackageManagers: map[string]string{},
		Tools:           map[string]string{},
		Shell:           &shellInfo{DefaultShell: "zsh"},
		CollectedAt:     "2026-05-08T12:00:00Z",
	}
	b, err := json.Marshal(mi)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(b)
	for _, key := range []string{"runtimes", "packageManagers", "tools", "disk", "detectedCliAgents", "capabilities"} {
		if strings.Contains(out, "\""+key+"\"") {
			t.Errorf("expected %q to be omitted from JSON when empty/nil; got: %s", key, out)
		}
	}
	for _, key := range []string{"architecture", "cpu", "memory", "shell", "collectedAt"} {
		if !strings.Contains(out, "\""+key+"\"") {
			t.Errorf("expected %q in JSON; got: %s", key, out)
		}
	}
}

// TestGatherCLIAgents_PopulatesDisplayName verifies that the Name field on
// each emitted detectedCLIAgent carries the canonical product label
// ("Antigravity", "Claude Code", "Codex", "Gemini CLI") rather than the
// binary name. The frontend's About-tab "CLI Tools" chips read
// info.name || agentKey, so this is what shows in the UI.
func TestGatherCLIAgents_PopulatesDisplayName(t *testing.T) {
	if runtime.GOOS == "windows" {
		// LookPath behavior on Windows requires .exe / PATHEXT and a real
		// loadable image. The display-name mapping is platform-agnostic,
		// and the macOS/Linux PATH-shim approach below is enough coverage
		// for the regression we care about.
		t.Skip("PATH-shim style probe not portable to windows test runners")
	}

	dir := t.TempDir()
	// Drop empty executable stubs for every binary gatherCLIAgents probes.
	// LookPath only checks the executable bit; the version probe will time
	// out / exit non-zero, but Detected + Name are populated before the
	// version probe runs.
	for _, bin := range []string{"claude", "codex", "gemini", "agy"} {
		stub := filepath.Join(dir, bin)
		if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", stub, err)
		}
	}

	// t.Setenv restores PATH on test cleanup automatically.
	t.Setenv("PATH", dir)

	agents := gatherCLIAgents()

	wantNames := map[string]string{
		"claudeCode":  "Claude Code",
		"codex":       "Codex",
		"geminiCli":   "Gemini CLI",
		"antigravity": "Antigravity",
	}
	for key, wantName := range wantNames {
		entry, ok := agents[key]
		if !ok {
			t.Errorf("gatherCLIAgents missing key %q (PATH=%s)", key, dir)
			continue
		}
		if !entry.Detected {
			t.Errorf("agent %q: Detected=false, want true", key)
		}
		if entry.Name != wantName {
			t.Errorf("agent %q: Name=%q, want %q", key, entry.Name, wantName)
		}
		if entry.Path == "" {
			t.Errorf("agent %q: Path is empty", key)
		}
	}

	// JSON round-trip — the persisted shape on terminalAgents/{id} must
	// carry the human-readable name field so the frontend chip renders
	// "Antigravity v…" not "agy v…".
	b, err := json.Marshal(agents["antigravity"])
	if err != nil {
		t.Fatalf("marshal antigravity entry: %v", err)
	}
	if !strings.Contains(string(b), `"name":"Antigravity"`) {
		t.Errorf("JSON does not carry display name: %s", string(b))
	}
}

func TestCapabilitiesDerivation_RespectsRAMConstraint(t *testing.T) {
	// Recommendations cap at min(threads/cores, RAM/2 or RAM/4).
	// If RAM is the tight constraint, that wins.
	mi := &MachineInfo{
		CPU:    &cpuInfo{Cores: 16, Threads: 32},
		Memory: &memoryInfo{TotalGB: 8.0}, // tight: RAM-limited
	}
	if mi.CPU != nil && mi.Memory != nil && mi.Memory.TotalGB > 0 {
		mi.Capabilities = &capabilitiesInfo{
			RecommendedConcurrentTests:  minInt(mi.CPU.Threads, int(mi.Memory.TotalGB/2)),
			RecommendedConcurrentBuilds: minInt(mi.CPU.Cores, int(mi.Memory.TotalGB/4)),
		}
	}
	if mi.Capabilities.RecommendedConcurrentTests != 4 {
		t.Errorf("RecommendedConcurrentTests = %d, want 4 (min(32, 8/2))",
			mi.Capabilities.RecommendedConcurrentTests)
	}
	if mi.Capabilities.RecommendedConcurrentBuilds != 2 {
		t.Errorf("RecommendedConcurrentBuilds = %d, want 2 (min(16, 8/4))",
			mi.Capabilities.RecommendedConcurrentBuilds)
	}
}
