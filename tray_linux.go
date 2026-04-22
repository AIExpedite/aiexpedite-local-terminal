//go:build linux
// +build linux

package main

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	"github.com/getlantern/systray"
)

// Embed the icon (PNG format) for Linux tray icon (if supported).
//
//go:embed assets/icon.png
var iconData []byte

//go:embed assets/icon-disconnected.png
var iconDataDisconnected []byte

// applyStandardTrayIcon restores the normal tray icon on reconnect.
func applyStandardTrayIcon() {
	if !IsSystrayReady() {
		return
	}
	systray.SetIcon(iconData)
}

// applyDisconnectedTrayIcon swaps the tray icon to the "disconnected" variant
// so users can see the agent is offline at a glance.
func applyDisconnectedTrayIcon() {
	if !IsSystrayReady() {
		return
	}
	systray.SetIcon(iconDataDisconnected)
}

// Channel to notify main.go when console visibility changes (no-op on Linux, but needed for compilation)
var ConsoleHiddenChan = make(chan bool, 1)

// Channel to notify main.go when registration is invalidated (agent deleted from backend)
var RegistrationInvalidChan = make(chan bool, 1)

// SetAllowExit is a no-op on Linux (Windows console close handling)
func SetAllowExit() {}

// ensureAutoStart creates an XDG autostart .desktop file so the app starts at login.
func ensureAutoStart() error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	// Resolve symlinks so the .desktop file points to the real binary
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}

	// XDG autostart directory
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		configDir = filepath.Join(homeDir, ".config")
	}

	autostartDir := filepath.Join(configDir, "autostart")
	if err := os.MkdirAll(autostartDir, 0o755); err != nil {
		return err
	}

	// Use environment-specific filename (e.g., "aiexpedite-terminal-Dev.desktop" for dev)
	filename := "aiexpedite-terminal" + EnvConfigSuffix + ".desktop"
	desktopPath := filepath.Join(autostartDir, filename)

	// Use environment-specific display name
	content := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=%s
Exec=%s
Icon=aiexpedite-terminal
Terminal=false
StartupNotify=false
Comment=AI Expedite - Remote command execution agent
Categories=Development;Utility;
X-GNOME-Autostart-enabled=true
`, EnvDisplayName, exePath)

	if err := os.WriteFile(desktopPath, []byte(content), 0o644); err != nil {
		return err
	}

	fmt.Printf("[autostart] Created XDG autostart: %s\n", desktopPath)
	return nil
}

// showConsoleWindow is a no-op on Linux (console visibility is handled differently)
func showConsoleWindow(show bool) {
	// No-op on Linux
}

// allocateConsole is a no-op on Linux (Windows-only console allocation)
func allocateConsole() error {
	return nil
}

// ensureAppRegistration is a no-op on Linux (Windows registry only)
func ensureAppRegistration() error {
	return nil
}

// unregisterApp is a no-op on Linux (Windows registry only)
func unregisterApp() error {
	return nil
}
