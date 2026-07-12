//go:build darwin
// +build darwin

package main

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"text/template"

	"github.com/getlantern/systray"
)

//go:embed assets/icon.png
var iconData []byte

//go:embed assets/icon-disconnected.png
var iconDataDisconnected []byte

// applyStandardTrayIcon swaps the tray icon back to the normal template icon
// after the user toggles "Reconnect to cloud" from the tray menu.
func applyStandardTrayIcon() {
	if !IsSystrayReady() {
		return
	}
	systray.SetTemplateIcon(iconData, iconData)
}

// applyDisconnectedTrayIcon swaps the tray icon to the "disconnected" variant
// (with a red/grey dot overlay) so the offline state is visible at a glance.
// The disconnected PNG is NOT a template icon — we want the red overlay to
// stay red instead of being auto-inverted by macOS.
func applyDisconnectedTrayIcon() {
	if !IsSystrayReady() {
		return
	}
	systray.SetIcon(iconDataDisconnected)
}

// When launched by launchd or Finder, a GUI app inherits a minimal PATH
// (typically /usr/bin:/bin:/usr/sbin:/sbin) that excludes the common tool
// directories a developer uses from a login shell. That means
// `exec.LookPath("ttyd")`, `claude`, `codex`, `brew`, etc. can
// all fail to resolve even when they are installed. Prepend the
// standard locations so PATH-based resolution matches a typical terminal.
func init() {
	extraDirs := []string{
		"/opt/homebrew/bin", // Homebrew on Apple Silicon
		"/opt/homebrew/sbin",
		"/usr/local/bin", // Homebrew on Intel + many manual installs
		"/usr/local/sbin",
	}
	if home := os.Getenv("HOME"); home != "" {
		// Common per-user tool locations (Volta, nvm shims, asdf, pipx, etc.)
		extraDirs = append(extraDirs,
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, "bin"),
		)
	}

	current := os.Getenv("PATH")
	have := make(map[string]bool)
	for _, p := range filepath.SplitList(current) {
		have[p] = true
	}

	var prepend []string
	for _, d := range extraDirs {
		if !have[d] {
			prepend = append(prepend, d)
		}
	}
	if len(prepend) == 0 {
		return
	}

	newPath := prepend
	if current != "" {
		newPath = append(newPath, current)
	}

	sep := string(os.PathListSeparator)
	joined := ""
	for i, p := range newPath {
		if i > 0 {
			joined += sep
		}
		joined += p
	}
	_ = os.Setenv("PATH", joined)
}

// Channel to notify main.go when console visibility changes (no-op on macOS, but needed for compilation)
var ConsoleHiddenChan = make(chan bool, 1)

// Channel to notify main.go when registration is invalidated (agent deleted from backend)
var RegistrationInvalidChan = make(chan bool, 1)

// SetAllowExit is a no-op on macOS (Windows console close handling)
func SetAllowExit() {}

// launchAgentTemplate is the plist content for macOS login-item auto-start.
var launchAgentTemplate = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.ExePath}}</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<false/>
</dict>
</plist>
`))

// ensureAutoStart creates a LaunchAgent plist so the app starts at login.
func ensureAutoStart() error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	// Resolve symlinks so the plist points to the real binary
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	agentsDir := filepath.Join(homeDir, "Library", "LaunchAgents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		return err
	}

	// Use environment-specific label (e.g., "com.aiexpedite.terminal-Dev" for dev)
	label := "com.aiexpedite.terminal" + EnvConfigSuffix
	plistPath := filepath.Join(agentsDir, label+".plist")

	f, err := os.Create(plistPath)
	if err != nil {
		return err
	}
	defer f.Close()

	data := struct {
		Label   string
		ExePath string
	}{
		Label:   label,
		ExePath: exePath,
	}

	if err := launchAgentTemplate.Execute(f, data); err != nil {
		return err
	}

	fmt.Printf("[autostart] Created LaunchAgent: %s\n", plistPath)
	return nil
}

// showConsoleWindow is a no-op on macOS (console visibility is handled differently)
func showConsoleWindow(show bool) {
	// No-op on macOS
}

// allocateConsole is a no-op on macOS (Windows-only console allocation)
func allocateConsole() error {
	return nil
}

// ensureAppRegistration is a no-op on macOS (Windows registry only)
func ensureAppRegistration() error {
	return nil
}

// unregisterApp removes the macOS LaunchAgent plist installed by
// ensureAutoStart, unloading it first so launchd stops the running process
// cleanly. A missing plist is not an error — uninstall may run twice.
func unregisterApp() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	label := "com.aiexpedite.terminal" + EnvConfigSuffix
	plistPath := filepath.Join(home, "Library", "LaunchAgents", label+".plist")

	// Best-effort unload before deletion; ignore errors from already-unloaded
	// or never-loaded agents.
	_ = exec.Command("launchctl", "unload", plistPath).Run()

	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove LaunchAgent plist: %w", err)
	}
	return nil
}
