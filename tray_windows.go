//go:build windows
// +build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	_ "embed"

	"golang.org/x/sys/windows/registry"
)

//go:embed assets/aiexpedite-tray-icon.ico
var iconData []byte

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleWindow   = kernel32.NewProc("GetConsoleWindow")
	procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
	procShowWindow         = user32.NewProc("ShowWindow") // user32 declared in approval_windows.go
)

const (
	SW_HIDE            = 0
	SW_SHOWNORMAL      = 1
	SW_SHOW            = 5
	SW_MINIMIZE        = 6
	SW_SHOWNOACTIVATE  = 8
	SW_RESTORE         = 9
	SW_SHOWDEFAULT     = 10
)

// Console control event types
const (
	CTRL_C_EVENT        = 0
	CTRL_BREAK_EVENT    = 1
	CTRL_CLOSE_EVENT    = 2
	CTRL_LOGOFF_EVENT   = 5
	CTRL_SHUTDOWN_EVENT = 6
)

var (
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
)

// allowAppExit is set to true when user clicks "Quit" from tray menu
var allowAppExit = false

// SetAllowExit enables the app to actually exit (called from tray Quit)
func SetAllowExit() {
	allowAppExit = true
}

// consoleCtrlHandler handles console control events (like clicking X button)
// Returns true to indicate the event was handled (prevents app exit)
func consoleCtrlHandler(ctrlType uint) uintptr {
	switch ctrlType {
	case CTRL_CLOSE_EVENT:
		// User clicked X on console window - just hide it instead of exiting
		if !allowAppExit {
			fmt.Println("[console] Close button clicked - hiding console (use tray menu to quit)")
			showConsoleWindow(false)
			return 1 // TRUE - event handled, don't exit
		}
		// If allowAppExit is true, let the app exit normally
		return 0
	case CTRL_C_EVENT, CTRL_BREAK_EVENT:
		// Ctrl+C or Ctrl+Break - hide console instead of exiting
		if !allowAppExit {
			fmt.Println("[console] Ctrl+C/Break detected - hiding console (use tray menu to quit)")
			showConsoleWindow(false)
			return 1 // TRUE - event handled, don't exit
		}
		return 0
	case CTRL_LOGOFF_EVENT, CTRL_SHUTDOWN_EVENT:
		// System is logging off or shutting down - allow exit
		return 0
	}
	return 0
}

// initConsoleHandler sets up the console control handler to intercept close events
func initConsoleHandler() {
	// Create callback for SetConsoleCtrlHandler
	cb := syscall.NewCallback(consoleCtrlHandler)
	ret, _, err := procSetConsoleCtrlHandler.Call(cb, 1) // 1 = Add handler
	if ret == 0 {
		fmt.Printf("[console] Warning: Failed to set console handler: %v\n", err)
	}
}

// getConsoleWindow returns the handle to the console window
func getConsoleWindow() uintptr {
	ret, _, _ := procGetConsoleWindow.Call()
	return ret
}

// showConsoleWindow shows or hides the console window
func showConsoleWindow(show bool) {
	hwnd := getConsoleWindow()
	if hwnd == 0 {
		return // No console window
	}

	if show {
		// Restore window if minimized, then show and bring to foreground
		procShowWindow.Call(hwnd, SW_RESTORE)
		procShowWindow.Call(hwnd, SW_SHOW)
		procSetForegroundWindow.Call(hwnd)
	} else {
		// Completely hide the console window (not just minimize)
		procShowWindow.Call(hwnd, SW_HIDE)
	}
}

// ensureAutoStart adds/updates HKCU\Software\Microsoft\Windows\CurrentVersion\Run
// so that the agent launches automatically when the user logs in.
func ensureAutoStart() error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	// Quote the path in case it contains spaces.
	quotedPath := `"` + exePath + `"`

	key, err := registry.OpenKey(
		registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Run`,
		registry.QUERY_VALUE|registry.SET_VALUE,
	)
	if err != nil {
		return err
	}
	defer key.Close()

	// Use environment-specific registry key name (e.g., "AIExpedite-Dev" for dev)
	keyName := "AIExpedite" + EnvConfigSuffix
	return key.SetStringValue(keyName, quotedPath)
}

// copyIconToConfig writes the embedded icon to the config directory
// so Windows can reliably display it in Installed Apps
func copyIconToConfig(destPath string) error {
	// Ensure config directory exists
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}
	// Write the embedded icon data to file
	return os.WriteFile(destPath, iconData, 0o644)
}

// ensureAppRegistration registers the app in Windows "Installed Apps" (Add/Remove Programs)
// so users can easily uninstall it from Windows Settings
func ensureAppRegistration() error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}

	// Create/open the Uninstall registry key for this app
	// Use environment-specific key path (e.g., "AIExpediteTerminal-Dev" for dev)
	keyPath := `Software\Microsoft\Windows\CurrentVersion\Uninstall\AIExpediteTerminal` + EnvConfigSuffix
	key, _, err := registry.CreateKey(
		registry.CURRENT_USER,
		keyPath,
		registry.ALL_ACCESS,
	)
	if err != nil {
		return fmt.Errorf("failed to create uninstall key: %w", err)
	}
	defer key.Close()

	// Get the install directory (parent of executable)
	exeDir := filepath.Dir(exePath)

	// Copy icon to config directory for reliable display
	iconPath := filepath.Join(GetConfigDir(), "icon.ico")
	if err := copyIconToConfig(iconPath); err != nil {
		// Fall back to executable icon if copy fails
		iconPath = exePath + ",0"
	}

	// Set required registry values for Add/Remove Programs
	// Use environment-specific display name (e.g., "AI Expedite Terminal (Dev)" for dev)
	values := map[string]string{
		"DisplayName":     EnvDisplayName,
		"DisplayVersion":  version[1:], // Strip the "v" prefix from version
		"Publisher":       "AI Expedite",
		"InstallLocation": exeDir,
		"DisplayIcon":     iconPath,
		"UninstallString": `"` + exePath + `" --uninstall`,
		"QuietUninstallString": `"` + exePath + `" --uninstall --quiet`,
		"NoModify":        "",
		"NoRepair":        "",
		"Comments":        "Remote terminal access with security features",
		"URLInfoAbout":    "https://aiexpedite.com",
	}

	for name, value := range values {
		if name == "NoModify" || name == "NoRepair" {
			// These are DWORD values
			if err := key.SetDWordValue(name, 1); err != nil {
				return fmt.Errorf("failed to set %s: %w", name, err)
			}
		} else {
			if err := key.SetStringValue(name, value); err != nil {
				return fmt.Errorf("failed to set %s: %w", name, err)
			}
		}
	}

	// Set estimated size (in KB)
	if err := key.SetDWordValue("EstimatedSize", 15000); err != nil { // ~15 MB
		return fmt.Errorf("failed to set EstimatedSize: %w", err)
	}

	return nil
}

// unregisterApp removes the app from Windows "Installed Apps" and auto-start
func unregisterApp() error {
	// Use environment-specific key names
	runKeyName := "AIExpedite" + EnvConfigSuffix
	uninstallKeyPath := `Software\Microsoft\Windows\CurrentVersion\Uninstall\AIExpediteTerminal` + EnvConfigSuffix

	// Remove from auto-start
	runKey, err := registry.OpenKey(
		registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Run`,
		registry.SET_VALUE,
	)
	if err == nil {
		_ = runKey.DeleteValue(runKeyName)
		runKey.Close()
	}

	// Remove from Installed Apps
	err = registry.DeleteKey(
		registry.CURRENT_USER,
		uninstallKeyPath,
	)
	if err != nil && err != registry.ErrNotExist {
		return fmt.Errorf("failed to remove uninstall key: %w", err)
	}

	return nil
}
