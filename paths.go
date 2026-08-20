package main

import (
	"os"
	"path/filepath"
	"runtime"
)

// baseDir is the per-environment config/data directory that every other path
// helper hangs off: config.json, allowed-commands.txt, the CLI rate-limit
// caches, security.log, logs/, bin/, and the single-instance lock. It is
// resolved once at process start from the environment.
//
// The test suite redirects this to a throwaway directory in TestMain (see
// sandboxTestConfigDir). That redirect is load-bearing, not hygiene: because
// baseDir is resolved in init() from $HOME, a plain `go test ./...` on a
// developer's machine otherwise resolves the machine's LIVE agent directory,
// and any test that persists a Config through ConfigPath() overwrites the real
// registration. That happened on 2026-08-18 — a test fixture's agent id and a
// "test-attempt" update marker landed in the running agent's config.json,
// which unregistered the device and wedged its auto-updater.
var baseDir string

func init() {
	baseDir = resolveBaseDir(
		runtime.GOOS,
		EnvConfigSuffix,
		os.Getenv("HOME"),
		os.Getenv("APPDATA"),
		os.Getenv("XDG_CONFIG_HOME"),
	)
}

// resolveBaseDir computes the config/data directory for one OS + environment.
// suffix is EnvConfigSuffix, set via ldflags at build time ("-Dev", "-Stg",
// "-Beta", "" for prod) so each release channel keeps its own directory.
//
// Split out of init() as a pure function so every platform branch is testable
// on any host — init() can only ever exercise the branch for the OS the test
// binary happens to be running on.
func resolveBaseDir(goos, suffix, home, appdata, xdgConfigHome string) string {
	dirName := "AIExpedite" + suffix

	switch goos {
	case "windows":
		// On Windows, use %APPDATA% for config (roaming).
		if appdata == "" && home != "" {
			// Fallback to HOME if APPDATA not set (unlikely).
			appdata = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(appdata, dirName)
	case "darwin":
		// On macOS, use ~/Library/Application Support/.
		if home == "" {
			return "./" // fallback to current directory
		}
		return filepath.Join(home, "Library", "Application Support", dirName)
	default:
		// On Linux/Unix, use XDG_CONFIG_HOME or ~/.config.
		configHome := xdgConfigHome
		if configHome == "" && home != "" {
			configHome = filepath.Join(home, ".config")
		}
		if configHome == "" {
			return "./"
		}
		return filepath.Join(configHome, dirName)
	}
}

// GetConfigDir returns the directory path for configuration and data.
func GetConfigDir() string {
	return baseDir
}

// ConfigPath returns the full path to the configuration file.
func ConfigPath() string {
	return filepath.Join(baseDir, "config.json")
}

// BinDir returns the directory path for auxiliary binaries (tmux, ttyd).
func BinDir() string {
	return filepath.Join(baseDir, "bin")
}
