package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"
)

// fakeProbeRunner answers probes from a table keyed by the command name and
// records every call.
type fakeProbeRunner struct {
	mu      sync.Mutex
	answers map[string]string // cmd -> output; absent = not installed
	calls   []string
	envs    map[string][]string
}

func (f *fakeProbeRunner) run(ctx context.Context, cmd string, args, env []string, timeout time.Duration) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cmd+" "+strings.Join(args, " "))
	if f.envs == nil {
		f.envs = map[string][]string{}
	}
	f.envs[cmd] = env
	out, ok := f.answers[cmd]
	return out, ok
}

func stubSetupLookPath(t *testing.T, present ...string) {
	t.Helper()
	prev := setupProbeLookPath
	set := map[string]bool{}
	for _, p := range present {
		set[p] = true
	}
	setupProbeLookPath = func(file string) (string, error) {
		if set[file] {
			return "/fake/" + file, nil
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { setupProbeLookPath = prev })
}

func TestSetupProbeSpecsFor(t *testing.T) {
	keys := func(goos string) []string {
		var out []string
		for _, s := range setupProbeSpecsFor(goos) {
			out = append(out, s.Target+"."+s.Key)
		}
		return out
	}
	common := []string{"tools.gh", "tools.firebase", "tools.terraform"}
	if got := keys("windows"); !reflect.DeepEqual(got, append(common, "packageManagers.winget", "packageManagers.choco", "packageManagers.scoop")) {
		t.Fatalf("windows %v", got)
	}
	if got := keys("darwin"); !reflect.DeepEqual(got, append(common, "packageManagers.brew")) {
		t.Fatalf("darwin %v", got)
	}
	if got := keys("linux"); !reflect.DeepEqual(got, append(common, "packageManagers.apt", "packageManagers.brew")) {
		t.Fatalf("linux %v", got)
	}
}

func TestGatherSetupExtras_Windows(t *testing.T) {
	stubSetupLookPath(t, "scoop")
	f := &fakeProbeRunner{answers: map[string]string{
		"gh":         "gh version 2.60.0 (2024-10-10)\nhttps://github.com/cli/cli/releases/tag/v2.60.0\n",
		"terraform":  "Terraform v1.9.5\non windows_amd64\n",
		"winget":     "v1.9.25200\n",
		"powershell": `{"h":false,"f":[true]}`,
		"wsl":        string(utf16le("Default Distribution: Ubuntu\r\nDefault Version: 2\r\n")),
		"nvidia-smi": "NVIDIA GeForce RTX 4090, 24564, 566.36\n",
	}}
	x := gatherSetupExtras(context.Background(), "windows", f.run)

	wantTools := map[string]string{"gh": "gh version 2.60.0 (2024-10-10)", "terraform": "Terraform v1.9.5"}
	if !reflect.DeepEqual(x.Tools, wantTools) {
		t.Fatalf("tools %v", x.Tools)
	}
	wantPMs := map[string]string{"winget": "v1.9.25200", "scoop": setupProbePresent}
	if !reflect.DeepEqual(x.PackageManagers, wantPMs) {
		t.Fatalf("packageManagers %v", x.PackageManagers)
	}
	if x.Virtualization == nil || x.Virtualization.Enabled == nil || !*x.Virtualization.Enabled ||
		x.Virtualization.WSL2 == nil || !*x.Virtualization.WSL2 {
		t.Fatalf("virtualization %+v", x.Virtualization)
	}
	if len(x.NvidiaGPUs) != 1 || x.NvidiaGPUs[0].MemoryGB != 24 {
		t.Fatalf("nvidia %+v", x.NvidiaGPUs)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "scoop") {
			t.Fatalf("scoop is presence-only and must not be spawned: %v", f.calls)
		}
	}
	if !reflect.DeepEqual(f.envs["terraform"], terraformProbeEnv) || !reflect.DeepEqual(f.envs["firebase"], nodeCLIProbeEnv) {
		t.Fatalf("update checks not disabled: %v", f.envs)
	}
}

func TestGatherSetupExtras_NonWindowsHasNoVirtualization(t *testing.T) {
	stubSetupLookPath(t)
	f := &fakeProbeRunner{answers: map[string]string{"brew": "Homebrew 4.4.0\n", "firebase": "13.22.0\n"}}
	x := gatherSetupExtras(context.Background(), "darwin", f.run)
	if x.Virtualization != nil || x.NvidiaGPUs != nil {
		t.Fatalf("darwin must not probe virtualization / nvidia: %+v", x)
	}
	if x.PackageManagers["brew"] != "Homebrew 4.4.0" || x.Tools["firebase"] != "13.22.0" {
		t.Fatalf("%+v", x)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "wsl") || strings.HasPrefix(c, "powershell") || strings.HasPrefix(c, "nvidia-smi") || strings.HasPrefix(c, "apt") {
			t.Fatalf("unexpected probe on darwin: %v", f.calls)
		}
	}
}

func TestApplySetupExtras_ExistingKeysWin(t *testing.T) {
	info := &MachineInfo{
		Tools:           map[string]string{"git": "git version 2.55.0", "gh": "from base"},
		PackageManagers: map[string]string{"npm": "10.9.0"},
		GPU:             []gpuInfo{{Name: "NVIDIA GeForce GTX 1050 Ti", Vendor: "nvidia", MemoryGB: 4}},
	}
	applySetupExtras(info, setupExtras{
		Tools:           map[string]string{"gh": "from extras", "terraform": "Terraform v1.9.5"},
		PackageManagers: map[string]string{"winget": "v1.9"},
		NvidiaGPUs:      []gpuInfo{{Name: "NVIDIA GeForce GTX 1050 Ti", Vendor: "nvidia", MemoryGB: 4.0}},
	})
	if info.Tools["gh"] != "from base" || info.Tools["terraform"] != "Terraform v1.9.5" || info.PackageManagers["winget"] != "v1.9" {
		t.Fatalf("%+v", info)
	}
	if info.Virtualization != nil {
		t.Fatal("no virtualization reading must leave the field nil")
	}
}

func TestApplyNvidiaVRAM(t *testing.T) {
	wmi := []gpuInfo{
		{Name: "Intel(R) UHD Graphics 770", Vendor: "intel", MemoryGB: 1},
		{Name: "NVIDIA GeForce RTX 3090", Vendor: "nvidia", MemoryGB: 4, Driver: "32.0.15.6636"},
		{Name: "NVIDIA RTX A6000", Vendor: "nvidia", MemoryGB: 4},
	}
	nv := []gpuInfo{
		{Name: "NVIDIA RTX A6000", Vendor: "nvidia", MemoryGB: 48},
		{Name: "NVIDIA GeForce RTX 3090", Vendor: "nvidia", MemoryGB: 24},
	}
	got := applyNvidiaVRAM(wmi, nv)
	if got[0].MemoryGB != 1 || got[1].MemoryGB != 24 || got[2].MemoryGB != 48 || len(got) != 3 {
		t.Fatalf("%+v", got)
	}
	if got[1].Driver != "32.0.15.6636" {
		t.Fatal("the WMI driver version is kept")
	}
	if wmi[1].MemoryGB != 4 {
		t.Fatal("input slice must not be mutated")
	}
	// Names that differ fall back to order; unmatched nvidia-smi rows are appended.
	got = applyNvidiaVRAM([]gpuInfo{{Name: "NVIDIA GeForce GTX 1050 Ti", Vendor: "nvidia", MemoryGB: 4}},
		[]gpuInfo{{Name: "GeForce GTX 1050 Ti", Vendor: "nvidia", MemoryGB: 4}, {Name: "Tesla T4", Vendor: "nvidia", MemoryGB: 15.6}})
	if len(got) != 2 || got[0].MemoryGB != 4 || got[1].Name != "Tesla T4" {
		t.Fatalf("%+v", got)
	}
	// Exact names are matched across ALL adapters before any order fallback:
	// the first adapter (no exact row) must not consume the second adapter's
	// exact row just because it comes first.
	got = applyNvidiaVRAM(
		[]gpuInfo{
			{Name: "NVIDIA GeForce RTX 3060 Laptop GPU", Vendor: "nvidia", MemoryGB: 4},
			{Name: "NVIDIA RTX A6000", Vendor: "nvidia", MemoryGB: 4},
		},
		[]gpuInfo{
			{Name: "NVIDIA RTX A6000", Vendor: "nvidia", MemoryGB: 48},
			{Name: "NVIDIA GeForce RTX 3060", Vendor: "nvidia", MemoryGB: 6},
		})
	if len(got) != 2 || got[0].MemoryGB != 6 || got[1].MemoryGB != 48 {
		t.Fatalf("exact-first matching: %+v", got)
	}
	if got := applyNvidiaVRAM(wmi, nil); !reflect.DeepEqual(got, wmi) {
		t.Fatal("no nvidia-smi rows leaves WMI untouched")
	}
}

func TestParseWindowsVirtualizationJSON(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		in   string
		want *bool
	}{
		{`{"h":true,"f":[false]}`, &yes}, // Hyper-V running hides VT-x from the root partition
		{`{"h":false,"f":[true,true]}`, &yes},
		{`{"h":false,"f":true}`, &yes}, // a single value, not an array
		{`{"h":false,"f":[false]}`, &no},
		{`{"h":false,"f":[null]}`, nil},
		{`{"h":null,"f":[false]}`, nil},
		{`{"f":[]}`, nil},
		{`not json`, nil},
		{``, nil},
	}
	for _, c := range cases {
		got := parseWindowsVirtualizationJSON(c.in)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%q: got %v want %v", c.in, ptrStr(got), ptrStr(c.want))
		}
	}
}

func ptrStr(b *bool) string {
	if b == nil {
		return "nil"
	}
	if *b {
		return "true"
	}
	return "false"
}

func utf16le(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 0, 2*len(u))
	for _, r := range u {
		b = append(b, byte(r), byte(r>>8))
	}
	return b
}

func TestWSLStatusWSL2(t *testing.T) {
	if got := wslStatusWSL2(string(utf16le("Default Version: 2\r\n")), true); got == nil || !*got {
		t.Fatal("WSL2 default should be true")
	}
	bom := append([]byte{0xFF, 0xFE}, utf16le("Default Version: 1\r\n")...)
	if got := wslStatusWSL2(string(bom), true); got == nil || *got {
		t.Fatal("WSL 1 default should be false")
	}
	if got := wslStatusWSL2("", false); got == nil || *got {
		t.Fatal("a failing wsl --status means WSL2 is not set up")
	}
	if got := wslStatusWSL2("Default Version: 2", true); got == nil || !*got {
		t.Fatal("plain UTF-8 output is accepted too")
	}
	if decodeMaybeUTF16LE([]byte("plain")) != "plain" {
		t.Fatal("ASCII passes through")
	}
}

func TestSelectVersionLine(t *testing.T) {
	cases := []struct{ out, pattern, want string }{
		{"\n  git version 2.55.0.windows.1\n", "", "git version 2.55.0.windows.1"},
		{"Picked up _JAVA_OPTIONS: -Xmx1g\nopenjdk version \"21.0.2\" 2024-01-16\n", `version "(\d+[^"]*)"`, `openjdk version "21.0.2" 2024-01-16`},
		{"13.22.0\n", `(\d+\.\d+\.\d+)`, "13.22.0"},
		{"first\nsecond\n", `nomatch`, "first"},
		{"first\n", `(?<=js-only)`, "first"}, // uncompilable in Go: first line
		{"", "", ""},
		{strings.Repeat("x", 300), "", strings.Repeat("x", maxProbeLineLength)},
	}
	for _, c := range cases {
		if got := selectVersionLine(c.out, c.pattern); got != c.want {
			t.Errorf("selectVersionLine(%q, %q) = %q, want %q", c.out, c.pattern, got, c.want)
		}
	}
}

func TestRunProbesParallel_BoundedAndComplete(t *testing.T) {
	var inFlight, peak, done atomic.Int32
	runProbesParallel(context.Background(), 20, 3, func(ctx context.Context, i int) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-1)
		done.Add(1)
	})
	if done.Load() != 20 || peak.Load() > 3 {
		t.Fatalf("done=%d peak=%d", done.Load(), peak.Load())
	}
}

/* --------------------------------------------------------------------------
   setupToolCatalog
   -------------------------------------------------------------------------- */

func TestNormalizeSetupToolCatalog(t *testing.T) {
	got, skipped := normalizeSetupToolCatalogReport([]setupToolCatalogEntry{
		{ID: "git", Command: "git"},
		{ID: "GIT", Command: "git"}, // duplicate id
		{ID: "java", Command: "java", VersionArgs: []string{"--version"}, VersionPattern: `version "([^"]+)"`},
		{ID: "kubectl", Command: "kubectl", VersionArgs: []string{"version", "--short"}}, // not an allowlisted tool
		{ID: "java8", Command: "java", VersionArgs: []string{"-version"}},                // not java's allowlisted form
		{ID: "gpu", Command: "nvidia-smi", VersionArgs: []string{"--query-gpu=driver_version", "--format=csv,noheader"}},
		{ID: "evil1", Command: "powershell", VersionArgs: []string{"--version"}},
		{ID: "evil2", Command: "node", VersionArgs: []string{"-e", "process.exit"}},
		{ID: "evil3", Command: "python", VersionArgs: []string{"-c", "import os"}},
		{ID: "evil4", Command: "../bin/git"},
		{ID: "evil5", Command: "git", VersionArgs: []string{"--version;rm"}},
		{ID: "", Command: "git"},
		{ID: "long", Command: "go", VersionArgs: []string{"version"}, VersionPattern: strings.Repeat("a", maxSetupToolPatternLength+1)},
	})
	want := []setupToolCatalogEntry{
		{ID: "git", Command: "git", VersionArgs: []string{"--version"}},
		{ID: "java", Command: "java", VersionArgs: []string{"--version"}, VersionPattern: `version "([^"]+)"`},
		{ID: "long", Command: "go", VersionArgs: []string{"version"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	if len(skipped) != 10 {
		t.Fatalf("expected 10 skipped entries with reasons, got %q", skipped)
	}
}

// A catalog probe can only ever be a version query: anything that would run a
// different operation, however plausible its flags, is refused.
func TestNormalizeSetupToolCatalog_RefusesNonVersionCommands(t *testing.T) {
	refused := []setupToolCatalogEntry{
		{ID: "codex", Command: "npm", VersionArgs: []string{"uninstall", "-g", "codex"}},
		{ID: "codex2", Command: "npm uninstall -g codex"},
		{ID: "rm", Command: "rm", VersionArgs: []string{"version"}},
		{ID: "mkdir", Command: "mkdir", VersionArgs: []string{"--version"}},
		{ID: "npm", Command: "npm", VersionArgs: []string{"--version"}}, // not in the catalog, so not allowlisted
		{ID: "go-wrong-form", Command: "go", VersionArgs: []string{"--version"}},
		{ID: "git-v", Command: "git", VersionArgs: []string{"-v"}},
		{ID: "case", Command: "Git", VersionArgs: []string{"--version"}},
		{ID: "a", Command: "npm", VersionArgs: []string{"install", "-g", "x"}},
		{ID: "b", Command: "git", VersionArgs: []string{"--version", "--exec-path=/tmp"}},
		{ID: "c", Command: "gh", VersionArgs: []string{"auth", "logout"}},
		{ID: "d", Command: "docker", VersionArgs: []string{"system", "prune", "-f"}},
		{ID: "e", Command: "winget", VersionArgs: []string{"uninstall", "Git.Git"}},
		{ID: "f", Command: "node", VersionArgs: []string{"--version", "-e"}},
		{ID: "g", Command: "git", VersionArgs: []string{"--VERSION"}},
		{ID: "h", Command: "git", VersionArgs: []string{"-version", "--json"}},
		{ID: "i", Command: `C:\Windows\System32\cmd`, VersionArgs: []string{"--version"}},
		{ID: "j", Command: "/usr/bin/git"},
		{ID: "k", Command: "sh", VersionArgs: []string{"--version"}},
	}
	for _, e := range refused {
		got, skipped := normalizeSetupToolCatalogReport([]setupToolCatalogEntry{e})
		if len(got) != 0 || len(skipped) != 1 {
			t.Errorf("%+v was accepted (got %+v)", e, got)
		}
	}
	// A refused entry is never spawned by a pass.
	f := &fakeProbeRunner{answers: map[string]string{"npm": "removed 1 package"}}
	if res := probeSetupToolCatalog(context.Background(), normalizeSetupToolCatalog(refused), nil, f.run); len(res) != 0 || len(f.calls) != 0 {
		t.Fatalf("refused entries ran: %v %v", res, f.calls)
	}
}

// Every detect entry the db-content catalog ships today (dev/setupTools/*.json
// `detect`, plus the cliAgents' commands with the default --version, as
// terminal-service sends them) is accepted unchanged. Transcribed from
// db-content origin/main on 2026-09-26; a new versionArgs form there needs an
// agent release adding it to setupToolProbeAllowlist first. Every allowlisted
// command is exercised, so the allowlist cannot hold a pair nothing uses.
func TestNormalizeSetupToolCatalog_AcceptsRealCatalog(t *testing.T) {
	real := []setupToolCatalogEntry{
		{ID: "cuda-toolkit", Command: "nvcc", VersionArgs: []string{"--version"}, VersionPattern: `release (\d+\.\d+)`},
		{ID: "docker-desktop", Command: "docker", VersionArgs: []string{"--version"}, VersionPattern: `version (\d+\.\d+(?:\.\d+)?)`},
		{ID: "firebase-tools", Command: "firebase", VersionArgs: []string{"--version"}, VersionPattern: `(\d+\.\d+(?:\.\d+)?)`},
		{ID: "flutter", Command: "flutter", VersionArgs: []string{"--version"}, VersionPattern: `Flutter (\d+\.\d+(?:\.\d+)?)`},
		{ID: "gh", Command: "gh", VersionArgs: []string{"--version"}, VersionPattern: `gh version (\d+\.\d+(?:\.\d+)?)`},
		{ID: "git", Command: "git", VersionArgs: []string{"--version"}, VersionPattern: `git version (\d+\.\d+(?:\.\d+)?)`},
		{ID: "go", Command: "go", VersionArgs: []string{"version"}, VersionPattern: `go(\d+\.\d+(?:\.\d+)?)`},
		{ID: "godot", Command: "godot", VersionArgs: []string{"--version"}, VersionPattern: `(\d+\.\d+(?:\.\d+)?)`},
		{ID: "java", Command: "java", VersionArgs: []string{"--version"}, VersionPattern: `(?:openjdk|java) (\d+\.\d+(?:\.\d+)?)`},
		{ID: "node", Command: "node", VersionArgs: []string{"--version"}, VersionPattern: `v?(\d+\.\d+(?:\.\d+)?)`},
		{ID: "playwright", Command: "playwright", VersionArgs: []string{"--version"}, VersionPattern: `(\d+\.\d+(?:\.\d+)?)`},
		{ID: "python", Command: "python3", VersionArgs: []string{"--version"}, VersionPattern: `Python (\d+\.\d+(?:\.\d+)?)`},
		{ID: "python-windows", Command: "python", VersionArgs: []string{"--version"}, VersionPattern: `Python (\d+\.\d+(?:\.\d+)?)`},
		{ID: "terraform", Command: "terraform", VersionArgs: []string{"--version"}, VersionPattern: `Terraform v(\d+\.\d+(?:\.\d+)?)`},
		{ID: "uv", Command: "uv", VersionArgs: []string{"--version"}, VersionPattern: `uv (\d+\.\d+(?:\.\d+)?)`},
		{ID: "vscode", Command: "code", VersionArgs: []string{"--version"}, VersionPattern: `(\d+\.\d+(?:\.\d+)?)`},
		{ID: "xcode", Command: "xcodebuild", VersionArgs: []string{"-version"}, VersionPattern: `Xcode (\d+\.\d+(?:\.\d+)?)`},
		{ID: "antigravity", Command: "agy", VersionArgs: []string{"--version"}},
		{ID: "claudeCode", Command: "claude", VersionArgs: []string{"--version"}},
		{ID: "codex", Command: "codex", VersionArgs: []string{"--version"}},
		{ID: "grok", Command: "grok", VersionArgs: []string{"--version"}},
		{ID: "opencode", Command: "opencode"}, // no versionArgs: defaults to --version
	}
	got, skipped := normalizeSetupToolCatalogReport(real)
	if len(skipped) != 0 || len(got) != len(real) {
		t.Fatalf("real catalog entries refused: %q", skipped)
	}
	used := map[string]bool{}
	for _, e := range real {
		used[e.Command] = true
	}
	for cmd := range setupToolProbeAllowlist {
		if !used[cmd] {
			t.Errorf("allowlisted command %q is not used by the catalog", cmd)
		}
	}
	for i := range real {
		if got[i].Command != real[i].Command || got[i].VersionPattern != real[i].VersionPattern {
			t.Fatalf("entry %d changed: %+v -> %+v", i, real[i], got[i])
		}
	}
}

func TestProbeSetupToolCatalog(t *testing.T) {
	f := &fakeProbeRunner{answers: map[string]string{
		"git":       "git version 2.55.0\n",
		"java":      "Picked up JAVA_TOOL_OPTIONS\nopenjdk version \"21.0.2\"\n",
		"terraform": "Terraform v1.9.5\n",
	}}
	entries := normalizeSetupToolCatalog([]setupToolCatalogEntry{
		{ID: "git", Command: "git"},
		{ID: "java", Command: "java", VersionArgs: []string{"--version"}, VersionPattern: `version "([^"]+)"`},
		{ID: "terraform", Command: "terraform"},
		{ID: "firebase-tools", Command: "firebase"}, // not installed
		{ID: "codex", Command: "codex"},             // a CLI agent: skipped
	})
	got := probeSetupToolCatalog(context.Background(), entries, func(id string) bool { return id == "codex" }, f.run)
	want := map[string]string{"git": "git version 2.55.0", "java": `openjdk version "21.0.2"`, "terraform": "Terraform v1.9.5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "codex") {
			t.Fatal("a CLI agent entry must not be spawned by the catalog pass")
		}
	}
	if env := f.envs["terraform"]; !strings.Contains(strings.Join(env, " "), "CHECKPOINT_DISABLE=1") {
		t.Fatalf("terraform env %v", env)
	}
}

func TestWithSetupToolResults(t *testing.T) {
	info := &MachineInfo{
		baseTools: map[string]string{"git": "git version 2.55.0", "docker": "Docker version 27"},
		Tools:     map[string]string{"git": "git version 2.55.0", "docker": "Docker version 27", "stale-id": "old"},
		DetectedCliAgents: map[string]detectedCLIAgent{
			"codex":      {Detected: true, Version: "codex-cli 0.40.0"},
			"claudeCode": {Detected: true, Version: ""},
		},
	}
	catalog := []setupToolCatalogEntry{{ID: "git"}, {ID: "terraform"}, {ID: "codex"}, {ID: "claudeCode"}, {ID: "gh"}}
	withSetupToolResults(info, catalog, map[string]string{"git": "should not win", "terraform": "Terraform v1.9.5"})
	want := map[string]string{
		"git":       "git version 2.55.0",
		"docker":    "Docker version 27",
		"terraform": "Terraform v1.9.5",
		"codex":     "codex-cli 0.40.0",
	}
	if !reflect.DeepEqual(info.Tools, want) {
		t.Fatalf("tools %v", info.Tools)
	}
}

func TestSetupToolResultsCacheIsKeyedToCatalog(t *testing.T) {
	SetSetupToolCatalog([]setupToolCatalogEntry{{ID: "git", Command: "git"}})
	t.Cleanup(func() { SetSetupToolCatalog(nil); storeSetupToolResults(nil, nil) })
	storeSetupToolResults(activeSetupToolCatalog(), map[string]string{"git": "git version 2.55.0"})
	if got := cachedSetupToolResults(); got["git"] != "git version 2.55.0" {
		t.Fatalf("cache miss: %v", got)
	}
	SetSetupToolCatalog([]setupToolCatalogEntry{{ID: "git", Command: "git"}, {ID: "gh", Command: "gh"}})
	if got := cachedSetupToolResults(); got != nil {
		t.Fatalf("results probed for another catalog must not be served: %v", got)
	}
}

func TestGetOIDCToken_PersistsSetupToolCatalog(t *testing.T) {
	oldBaseDir := baseDir
	baseDir = t.TempDir()
	t.Cleanup(func() { baseDir = oldBaseDir })
	SetSetupToolCatalog(nil)
	t.Cleanup(func() { SetSetupToolCatalog(nil) })

	origCLI := refreshMachineInfoAfterCatalogUpdate
	refreshMachineInfoAfterCatalogUpdate = func() {}
	t.Cleanup(func() { refreshMachineInfoAfterCatalogUpdate = origCLI })
	orig := refreshSetupToolsAfterCatalogUpdate
	var refreshes atomic.Int32
	refreshSetupToolsAfterCatalogUpdate = func() { refreshes.Add(1) }
	t.Cleanup(func() { refreshSetupToolsAfterCatalogUpdate = orig })

	respond := func(catalog any) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body := map[string]any{"id_token": "oidc-token", "expires_in": 3600, "token_type": "Bearer"}
			if catalog != nil {
				body["setupToolCatalog"] = catalog
			}
			_ = json.NewEncoder(w).Encode(body)
		}))
	}
	catalog := []map[string]any{
		{"id": "gh", "command": "gh", "versionArgs": []string{"--version"}},
		{"id": "java", "command": "java", "versionArgs": []string{"--version"}, "versionPattern": `version "([^"]+)"`},
	}
	srv := respond(catalog)
	t.Cleanup(srv.Close)
	cfg := &Config{AgentID: "agent-1", CommandSecret: "secret", TokenEndpoint: srv.URL}
	if _, err := NewWIFTokenSource(cfg).getOIDCToken(); err != nil {
		t.Fatal(err)
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes=%d, want 1", refreshes.Load())
	}
	if got := activeSetupToolCatalog(); len(got) != 2 || got[1].VersionPattern != `version "([^"]+)"` {
		t.Fatalf("active catalog %+v", got)
	}
	// Same catalog again: no re-probe.
	if _, err := NewWIFTokenSource(cfg).getOIDCToken(); err != nil {
		t.Fatal(err)
	}
	if refreshes.Load() != 1 {
		t.Fatalf("an unchanged catalog re-probed: %d", refreshes.Load())
	}
	// A response without the field (older backend) leaves it alone.
	srv2 := respond(nil)
	t.Cleanup(srv2.Close)
	cfg.TokenEndpoint = srv2.URL
	if _, err := NewWIFTokenSource(cfg).getOIDCToken(); err != nil {
		t.Fatal(err)
	}
	if len(activeSetupToolCatalog()) != 2 {
		t.Fatal("an absent setupToolCatalog cleared the stored one")
	}

	// Persisted, and re-activated on load.
	SetSetupToolCatalog(nil)
	loaded, err := LoadConfig(ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.SetupToolCatalog) != 2 || len(activeSetupToolCatalog()) != 2 {
		t.Fatalf("persisted %+v active %+v", loaded.SetupToolCatalog, activeSetupToolCatalog())
	}
}

func TestGetOIDCToken_SendsVirtualization(t *testing.T) {
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		got.Store(payload)
		_ = json.NewEncoder(w).Encode(map[string]any{"id_token": "t", "expires_in": 3600})
	}))
	t.Cleanup(srv.Close)

	machineInfoMu.Lock()
	prev := machineInfoCache
	yes := true
	machineInfoCache = &MachineInfo{
		Architecture:   "amd64",
		Tools:          map[string]string{"gh": "gh version 2.60.0"},
		Virtualization: &virtualizationInfo{Enabled: &yes},
	}
	machineInfoMu.Unlock()
	t.Cleanup(func() {
		machineInfoMu.Lock()
		machineInfoCache = prev
		machineInfoMu.Unlock()
	})

	cfg := &Config{AgentID: "agent-1", CommandSecret: "secret", TokenEndpoint: srv.URL}
	if _, err := NewWIFTokenSource(cfg).getOIDCToken(); err != nil {
		t.Fatal(err)
	}
	payload, _ := got.Load().(map[string]any)
	v, ok := payload["virtualization"].(map[string]any)
	if !ok || v["enabled"] != true || v["wsl2"] != nil {
		t.Fatalf("virtualization payload %#v", payload["virtualization"])
	}
	if _, present := v["wsl2"]; !present {
		t.Fatal("an unknown wsl2 is sent as null, not omitted")
	}
	if tools, _ := payload["tools"].(map[string]any); tools["gh"] != "gh version 2.60.0" {
		t.Fatalf("tools %#v", payload["tools"])
	}
}
