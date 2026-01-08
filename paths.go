package main

import (
	"os"
	"path/filepath"
	"runtime"
)

var baseDir string

func init() {
	// Determine a default base directory for config and data, depending on OS.
	// EnvConfigSuffix is set via ldflags at build time (e.g., "-Dev", "-Stg", "-Beta", "" for prod)
	home := os.Getenv("HOME")
	dirName := "AIExpedite" + EnvConfigSuffix

	switch runtime.GOOS {
	case "windows":
		// On Windows, use %APPDATA% for config (roaming).
		appdata := os.Getenv("APPDATA")
		if appdata == "" && home != "" {
			// Fallback to HOME if APPDATA not set (unlikely).
			appdata = filepath.Join(home, "AppData", "Roaming")
		}
		baseDir = filepath.Join(appdata, dirName)
	case "darwin":
		// On macOS, use ~/Library/Application Support/.
		if home == "" {
			baseDir = "./" // fallback to current directory
		} else {
			baseDir = filepath.Join(home, "Library", "Application Support", dirName)
		}
	default:
		// On Linux/Unix, use XDG_CONFIG_HOME or ~/.config.
		configHome := os.Getenv("XDG_CONFIG_HOME")
		if configHome == "" && home != "" {
			configHome = filepath.Join(home, ".config")
		}
		if configHome == "" {
			baseDir = "./"
		} else {
			baseDir = filepath.Join(configHome, dirName)
		}
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
