// paths_test.go
// -----------------------------------------------------------------------------
// Two things are pinned here.
//
//  1. resolveBaseDir — the per-OS config/data directory rules. They used to live
//     inline in init(), where only the branch matching the test host's GOOS
//     could ever be exercised; as a pure function all three are testable
//     everywhere.
//
//  2. The suite-wide sandbox. These are guard tests, not unit tests: they fail
//     if TestMain ever stops redirecting the config directory, because the
//     consequence of that regression is not a red test — it is `go test ./...`
//     silently overwriting the live agent registration on the machine running
//     it. See sandboxTestConfigDir in test_environment_test.go for the incident.
// -----------------------------------------------------------------------------

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

/* ─────────────────────────────── resolveBaseDir ─────────────────────────── */

func TestResolveBaseDir(t *testing.T) {
	tests := []struct {
		name          string
		goos          string
		suffix        string
		home          string
		appdata       string
		xdgConfigHome string
		want          string
	}{
		{
			name: "darwin uses Application Support",
			goos: "darwin", home: "/Users/dev",
			want: filepath.Join("/Users/dev", "Library", "Application Support", "AIExpedite"),
		},
		{
			name: "darwin without HOME falls back to the working directory",
			goos: "darwin", home: "",
			want: "./",
		},
		{
			name: "windows prefers APPDATA",
			goos: "windows", home: `C:\Users\dev`, appdata: `C:\Users\dev\AppData\Roaming`,
			want: filepath.Join(`C:\Users\dev\AppData\Roaming`, "AIExpedite"),
		},
		{
			name: "windows without APPDATA derives it from HOME",
			goos: "windows", home: `C:\Users\dev`,
			want: filepath.Join(`C:\Users\dev`, "AppData", "Roaming", "AIExpedite"),
		},
		{
			name: "linux prefers XDG_CONFIG_HOME",
			goos: "linux", home: "/home/dev", xdgConfigHome: "/home/dev/.xdg",
			want: filepath.Join("/home/dev/.xdg", "AIExpedite"),
		},
		{
			name: "linux without XDG_CONFIG_HOME falls back to ~/.config",
			goos: "linux", home: "/home/dev",
			want: filepath.Join("/home/dev", ".config", "AIExpedite"),
		},
		{
			name: "linux without any home falls back to the working directory",
			goos: "linux",
			want: "./",
		},
		{
			name: "an unrecognised GOOS takes the unix branch",
			goos: "freebsd", home: "/home/dev",
			want: filepath.Join("/home/dev", ".config", "AIExpedite"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveBaseDir(tc.goos, tc.suffix, tc.home, tc.appdata, tc.xdgConfigHome)
			if got != tc.want {
				t.Fatalf("resolveBaseDir(%q, %q, %q, %q, %q) = %q, want %q",
					tc.goos, tc.suffix, tc.home, tc.appdata, tc.xdgConfigHome, got, tc.want)
			}
		})
	}
}

// Each release channel gets its own directory, so a dev build can never read or
// write the prod build's registration on the same machine.
func TestResolveBaseDirKeepsChannelsApart(t *testing.T) {
	seen := map[string]string{}
	for _, suffix := range []string{"", "-Dev", "-Stg", "-Beta"} {
		for _, goos := range []string{"darwin", "windows", "linux"} {
			dir := resolveBaseDir(goos, suffix, "/home/dev", `C:\AppData`, "")
			key := goos + "|" + dir
			if other, dup := seen[key]; dup {
				t.Fatalf("suffix %q collides with %q on %s: both resolve to %s", suffix, other, goos, dir)
			}
			seen[key] = suffix
			if !strings.HasSuffix(dir, "AIExpedite"+suffix) {
				t.Errorf("suffix %q on %s: %q does not end in AIExpedite%s", suffix, goos, dir, suffix)
			}
		}
	}
}

// init() must agree with the pure function for the host it actually runs on —
// otherwise the tests above would pin rules production does not follow.
func TestInitUsesResolveBaseDir(t *testing.T) {
	want := resolveBaseDir(
		runtime.GOOS, EnvConfigSuffix,
		os.Getenv("HOME"), os.Getenv("APPDATA"), os.Getenv("XDG_CONFIG_HOME"),
	)
	if GetConfigDir() != want {
		t.Fatalf("GetConfigDir() = %q, want %q — init() and resolveBaseDir disagree",
			GetConfigDir(), want)
	}
}

/* ──────────────────────────── suite sandbox guards ──────────────────────── */

func TestSuiteRunsInsideASandbox(t *testing.T) {
	if testSandboxDir == "" {
		t.Fatal("testSandboxDir is empty — TestMain is not calling sandboxTestConfigDir")
	}
	for _, tc := range []struct {
		name string
		path string
	}{
		{"config dir", GetConfigDir()},
		{"config.json", ConfigPath()},
		{"bin dir", BinDir()},
		{"log file", agentLogPath()},
	} {
		if !isPathSafeUnder(tc.path, testSandboxDir) {
			t.Errorf("%s resolves to %q, which is outside the sandbox %q", tc.name, tc.path, testSandboxDir)
		}
	}
}

// The sandbox is worthless if it happens to sit inside the directory it is
// supposed to protect.
func TestSandboxIsNotTheRealAgentInstall(t *testing.T) {
	if realConfigDirAtStartup == "" {
		t.Skip("no startup config dir recorded")
	}
	if isPathSafeUnder(ConfigPath(), realConfigDirAtStartup) || ConfigPath() == filepath.Join(realConfigDirAtStartup, "config.json") {
		t.Fatalf("ConfigPath() = %q is inside the real agent directory %q", ConfigPath(), realConfigDirAtStartup)
	}
	// On Windows the OS temp dir lives inside the profile
	// (%LOCALAPPDATA%\Temp), so a sandbox under the real temp root is the
	// expected placement there, not a leak into the home's dotfiles.
	underRealTemp := runtime.GOOS == "windows" && realTempAtStartup != "" && isPathSafeUnder(testSandboxDir, realTempAtStartup)
	if realHomeAtStartup != "" && !underRealTemp && isPathSafeUnder(testSandboxDir, realHomeAtStartup) {
		t.Fatalf("sandbox %q is inside the developer's real home %q", testSandboxDir, realHomeAtStartup)
	}
}

// The real home must be KNOWN, whatever shell launched the suite: the
// real-binary harness restores it to find the CLIs' caches and logins, and an
// empty record silently turns that into an empty sandbox (Windows PowerShell
// has no $HOME — the profile is %USERPROFILE%).
func TestRealHomeIsRecordedBeforeTheSandbox(t *testing.T) {
	if realHomeAtStartup == "" {
		t.Fatal("realHomeAtStartup is empty — sandboxTestConfigDir must record os.UserHomeDir() before redirecting it")
	}
	if isPathSafeUnder(realHomeAtStartup, testSandboxDir) || realHomeAtStartup == testSandboxDir {
		t.Fatalf("recorded home %q is the sandbox %q, not the developer's", realHomeAtStartup, testSandboxDir)
	}
}

// Home lookups that bypass GetConfigDir() must be sandboxed too — plenty of
// production code reaches for os.UserHomeDir() directly.
func TestSuiteHomeLookupsAreSandboxed(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}
	if home != testSandboxDir {
		t.Fatalf("os.UserHomeDir() = %q, want the sandbox %q", home, testSandboxDir)
	}
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME"} {
		v := os.Getenv(key)
		if v == "" {
			t.Errorf("%s is unset — a child process would resolve the real user directory", key)
			continue
		}
		if !isPathSafeUnder(v, testSandboxDir) && v != testSandboxDir {
			t.Errorf("%s = %q is outside the sandbox %q", key, v, testSandboxDir)
		}
	}
}

// The regression pin for the incident itself: SetOffline persists the config
// through ConfigPath(), and that write must land in the sandbox. This is the
// exact call TestExitDrain_CancelsDrainRecoveryOnDisconnect makes — the one
// that overwrote a developer's live registration.
func TestPersistingConfigWritesIntoTheSandbox(t *testing.T) {
	resetShutdownState(t)

	cfg := DefaultConfig()
	cfg.AgentID = "sandbox-probe-agent"
	cfg.CommandSecret = "sandbox-probe-secret"

	SetOffline(true, cfg)
	t.Cleanup(func() { SetOffline(false, nil) })

	raw, err := os.ReadFile(ConfigPath())
	if err != nil {
		t.Fatalf("config was not persisted inside the sandbox: %v", err)
	}
	var persisted map[string]any
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("persisted config is not JSON: %v", err)
	}
	if persisted["agent_id"] != "sandbox-probe-agent" {
		t.Fatalf("sandbox config.json agent_id = %v, want the probe value — the write went somewhere else",
			persisted["agent_id"])
	}
	if !isPathSafeUnder(ConfigPath(), testSandboxDir) {
		t.Fatalf("ConfigPath() = %q escaped the sandbox", ConfigPath())
	}
}

// Redirecting the in-process variable is not enough on its own: tests re-exec
// the test binary (the mock CLI, the installer probes), and a child resolves
// its own baseDir in init() from the inherited environment. This spawns a real
// child and asserts the directory it computed at startup is sandboxed.
func TestChildProcessesResolveASandboxedConfigDir(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a child test binary")
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperReportsStartupConfigDir", "-test.v")
	cmd.Env = append(os.Environ(), helperReportConfigDirEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper child failed: %v\n%s", err, out)
	}

	const marker = "STARTUP_CONFIG_DIR="
	var childDir string
	for _, line := range strings.Split(string(out), "\n") {
		if idx := strings.Index(line, marker); idx >= 0 {
			childDir = strings.TrimSpace(line[idx+len(marker):])
		}
	}
	if childDir == "" {
		t.Fatalf("helper child did not report its startup config dir:\n%s", out)
	}
	if !isPathSafeUnder(childDir, testSandboxDir) {
		t.Fatalf("child resolved %q at startup, outside the sandbox %q — the environment is not redirected",
			childDir, testSandboxDir)
	}
}

const helperReportConfigDirEnv = "AIX_TEST_REPORT_STARTUP_CONFIG_DIR"

// TestHelperReportsStartupConfigDir is not a test; it is the child half of
// TestChildProcessesResolveASandboxedConfigDir. realConfigDirAtStartup holds
// whatever init() resolved before TestMain installed this child's own sandbox,
// which is exactly the value under test.
func TestHelperReportsStartupConfigDir(t *testing.T) {
	if os.Getenv(helperReportConfigDirEnv) == "" {
		t.Skip("helper process only")
	}
	t.Logf("STARTUP_CONFIG_DIR=%s", realConfigDirAtStartup)
}
