//go:build darwin
// +build darwin

package main

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
)

//go:embed assets/icon.png
var iconData []byte

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

// unregisterApp is a no-op on macOS (Windows registry only)
func unregisterApp() error {
	return nil
}
