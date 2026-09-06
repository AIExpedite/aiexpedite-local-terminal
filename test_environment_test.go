package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// isolateTestUserHome confines home- and installer-based executable/config
// discovery to a test-owned directory. HOME alone is insufficient on Windows,
// where os.UserHomeDir reads USERPROFILE, and an empty PATH alone is
// insufficient for Grok because production deliberately probes GROK_BIN_DIR.
func isolateTestUserHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GROK_BIN_DIR", filepath.Join(home, ".grok", "bin"))
}

// ─────────────────────────── suite-wide sandbox ───────────────────────────
//
// isolateTestUserHome above is opt-in, and opting in is exactly what gets
// forgotten. sandboxTestConfigDir is the package-wide backstop: TestMain calls
// it before a single test runs, so no test can reach the developer's live
// agent install even if it never thinks about paths at all.
//
// The incident it pins: on 2026-08-18 a `go test` run on a developer machine
// overwrote ~/Library/Application Support/AIExpedite/config.json with a test
// fixture (agent_id "agent-1", command_secret "secret",
// pending_update_attempt_id "test-attempt") and appended fixture entries to the
// live security.log. The write path was ordinary — TestExitDrain_* calls
// SetOffline(true, cfg), SetOffline persists through ConfigPath(), and
// ConfigPath() resolved the real directory because baseDir is derived from
// $HOME in init(). The running agent lost its registration and its auto-updater
// wedged reconciling an attempt id that never existed server-side.

var (
	// testSandboxDir is the throwaway home the whole suite runs inside.
	testSandboxDir string
	// realHomeAtStartup / realConfigDirAtStartup are the developer's actual
	// paths, captured before the redirect purely so the guard tests can assert
	// the suite is nowhere near them.
	realHomeAtStartup      string
	realConfigDirAtStartup string
	// realTempAtStartup / realAppDataAtStartup: on Windows os.MkdirTemp lands
	// under %LOCALAPPDATA%\Temp, which is INSIDE the profile by design, so the
	// guard needs the real temp root to tell "under the home" from "under the
	// home's temp"; the real-binary harness restores APPDATA for the CLIs
	// that keep state there.
	realTempAtStartup    string
	realAppDataAtStartup string
)

// sandboxTestConfigDir redirects the process-wide config/data and temp
// directories (and the environment they are derived from) into a fresh
// sandbox. It returns a cleanup func; TestMain must call it explicitly because
// os.Exit skips deferred calls.
func sandboxTestConfigDir() func() {
	// os.UserHomeDir, not $HOME: on Windows the home is %USERPROFILE% and HOME
	// is unset outside Git Bash, which left this empty from PowerShell — so the
	// harness never restored the real profile and Codex reported no models.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		realHomeAtStartup = home
	} else {
		realHomeAtStartup = os.Getenv("HOME")
	}
	realTempAtStartup = os.TempDir()
	realAppDataAtStartup = os.Getenv("APPDATA")
	realConfigDirAtStartup = baseDir

	dir, err := os.MkdirTemp("", "aix-agent-test-home-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandboxTestConfigDir: cannot create temp home: %v\n", err)
		os.Exit(1)
	}
	// macOS hands out /var/folders/... which is a symlink to /private/var/...
	// Resolve it so containment checks compare like with like — production code
	// that round-trips a path through the filesystem gets the resolved form.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	testSandboxDir = dir

	// Redirect both the resolved baseDir and the environment it came from.
	// The env vars are not redundant: production code that reaches for
	// os.UserHomeDir() instead of GetConfigDir() needs them, and so do the
	// child processes tests spawn from os.Args[0] — a child re-runs init() in a
	// fresh process and would otherwise resolve the developer's real directory
	// no matter what this process set baseDir to.
	appData := filepath.Join(dir, "AppData", "Roaming")
	xdgConfigHome := filepath.Join(dir, ".config")
	tempDir := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "sandboxTestConfigDir: cannot create %s: %v\n", tempDir, err)
		os.Exit(1)
	}
	setEnvOrDie("HOME", dir)
	setEnvOrDie("USERPROFILE", dir) // os.UserHomeDir on Windows
	setEnvOrDie("APPDATA", appData)
	setEnvOrDie("XDG_CONFIG_HOME", xdgConfigHome)
	setEnvOrDie("TMPDIR", tempDir)
	setEnvOrDie("TEMP", tempDir)
	setEnvOrDie("TMP", tempDir)

	baseDir = resolveBaseDir(runtime.GOOS, EnvConfigSuffix, dir, appData, xdgConfigHome)
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "sandboxTestConfigDir: cannot create %s: %v\n", baseDir, err)
		os.Exit(1)
	}

	return func() { _ = os.RemoveAll(dir) }
}

func setEnvOrDie(key, value string) {
	if err := os.Setenv(key, value); err != nil {
		fmt.Fprintf(os.Stderr, "sandboxTestConfigDir: cannot set %s: %v\n", key, err)
		os.Exit(1)
	}
}
