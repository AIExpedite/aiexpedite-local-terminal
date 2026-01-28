//go:build windows
// +build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	_ "embed"

	"golang.org/x/sys/windows/registry"
)

//go:embed assets/aiexpedite-tray-icon.ico
var iconData []byte

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleWindow      = kernel32.NewProc("GetConsoleWindow")
	procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
	procAllocConsole          = kernel32.NewProc("AllocConsole")
	procFreeConsole           = kernel32.NewProc("FreeConsole")
	procGetStdHandle          = kernel32.NewProc("GetStdHandle")
	procShowWindow            = user32.NewProc("ShowWindow") // user32 declared in approval_windows.go

	// For setting console window icon
	procSendMessageW                = user32.NewProc("SendMessageW")
	procLookupIconIdFromDirectoryEx = user32.NewProc("LookupIconIdFromDirectoryEx")
	procCreateIconFromResourceEx    = user32.NewProc("CreateIconFromResourceEx")
)

// Standard handle constants for GetStdHandle
const (
	STD_OUTPUT_HANDLE = ^uintptr(10) // -11
	STD_ERROR_HANDLE  = ^uintptr(11) // -12
)

// Track if we've allocated a console
var consoleAllocated = false

const (
	SW_HIDE            = 0
	SW_SHOWNORMAL      = 1
	SW_SHOW            = 5
	SW_MINIMIZE        = 6
	SW_SHOWNOACTIVATE  = 8
	SW_RESTORE         = 9
	SW_SHOWDEFAULT     = 10

	// System menu constants for disabling close button
	SC_CLOSE     = 0xF060
	MF_BYCOMMAND = 0x00000000

	// Icon constants for WM_SETICON
	WM_SETICON      = 0x0080
	ICON_SMALL      = 0
	ICON_BIG        = 1
	LR_DEFAULTCOLOR = 0x00000000
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
	procGetSystemMenu       = user32.NewProc("GetSystemMenu")
	procDeleteMenu          = user32.NewProc("DeleteMenu")
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

// disableConsoleCloseButton removes the close button from the console window.
// This prevents users from accidentally closing the app via the console X button,
// since Windows forcibly terminates console apps on CTRL_CLOSE_EVENT regardless
// of whether the handler returns TRUE.
func disableConsoleCloseButton() {
	hwnd := getConsoleWindow()
	if hwnd == 0 {
		return
	}

	// Get the system menu (window menu with Close, Minimize, etc.)
	// The second parameter (0) means get a copy we can modify
	hMenu, _, _ := procGetSystemMenu.Call(hwnd, 0)
	if hMenu == 0 {
		return
	}

	// Delete the Close menu item - this also grays out the X button
	procDeleteMenu.Call(hMenu, SC_CLOSE, MF_BYCOMMAND)
}

// getConsoleWindow returns the handle to the console window
func getConsoleWindow() uintptr {
	ret, _, _ := procGetConsoleWindow.Call()
	return ret
}

// createIconFromData creates an icon handle from .ico file data
// Uses LookupIconIdFromDirectoryEx to find the best matching icon size,
// then CreateIconFromResourceEx to create the actual icon handle.
func createIconFromData(data []byte, width, height int) uintptr {
	if len(data) < 6 {
		return 0
	}

	// LookupIconIdFromDirectoryEx finds the best matching icon in the .ico file
	id, _, _ := procLookupIconIdFromDirectoryEx.Call(
		uintptr(unsafe.Pointer(&data[0])),
		1, // TRUE = icon (not cursor)
		uintptr(width),
		uintptr(height),
		LR_DEFAULTCOLOR,
	)
	if id == 0 {
		return 0
	}

	// Find the offset to the icon data in the .ico file
	// ICO format: 6-byte header + (16-byte entries * count)
	// Each entry has: width, height, colors, reserved, planes, bitcount, size, offset
	numIcons := int(data[4]) | int(data[5])<<8
	var offset, size uint32
	for i := 0; i < numIcons; i++ {
		entryOffset := 6 + i*16
		if entryOffset+16 > len(data) {
			break
		}
		// The ID returned by LookupIconIdFromDirectoryEx is 1-based index
		if i+1 == int(id) {
			size = uint32(data[entryOffset+8]) | uint32(data[entryOffset+9])<<8 |
				uint32(data[entryOffset+10])<<16 | uint32(data[entryOffset+11])<<24
			offset = uint32(data[entryOffset+12]) | uint32(data[entryOffset+13])<<8 |
				uint32(data[entryOffset+14])<<16 | uint32(data[entryOffset+15])<<24
			break
		}
	}

	if offset == 0 || size == 0 || int(offset+size) > len(data) {
		return 0
	}

	// CreateIconFromResourceEx creates the actual icon
	hIcon, _, _ := procCreateIconFromResourceEx.Call(
		uintptr(unsafe.Pointer(&data[offset])),
		uintptr(size),
		1,          // TRUE = icon
		0x00030000, // Icon version (required)
		uintptr(width),
		uintptr(height),
		LR_DEFAULTCOLOR,
	)
	return hIcon
}

// setConsoleIcon sets both small and large icons on the console window
func setConsoleIcon() {
	hwnd := getConsoleWindow()
	if hwnd == 0 {
		return
	}

	// Create small icon (16x16) for title bar and taskbar
	hIconSmall := createIconFromData(iconData, 16, 16)
	if hIconSmall != 0 {
		procSendMessageW.Call(hwnd, WM_SETICON, ICON_SMALL, hIconSmall)
	}

	// Create large icon (32x32) for Alt+Tab
	hIconBig := createIconFromData(iconData, 32, 32)
	if hIconBig != 0 {
		procSendMessageW.Call(hwnd, WM_SETICON, ICON_BIG, hIconBig)
	}
}

// allocateConsole creates a new console window for this GUI process
func allocateConsole() error {
	if consoleAllocated {
		return nil
	}

	// Clear Windows Terminal environment variables to prevent WT from
	// intercepting AllocConsole() and creating a wrapper window.
	// When these are set, Windows routes console allocation through Windows Terminal,
	// which creates a separate terminal window that the app cannot control.
	// By clearing them, we force Windows to use the legacy conhost.exe instead.
	os.Unsetenv("WT_SESSION")
	os.Unsetenv("WT_PROFILE_ID")

	ret, _, err := procAllocConsole.Call()
	if ret == 0 {
		return fmt.Errorf("AllocConsole failed: %v", err)
	}

	// Get the new console's stdout/stderr handles
	stdout, _, _ := procGetStdHandle.Call(STD_OUTPUT_HANDLE)
	stderr, _, _ := procGetStdHandle.Call(STD_ERROR_HANDLE)

	// Update Go's os.Stdout and os.Stderr to use the new console
	os.Stdout = os.NewFile(stdout, "stdout")
	os.Stderr = os.NewFile(stderr, "stderr")

	consoleAllocated = true

	// Disable the close button on the new console
	disableConsoleCloseButton()

	// Set up console control handler
	initConsoleHandler()

	// Set the console window icon
	setConsoleIcon()

	// Print startup banner now that we have a console
	fmt.Println("")
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Printf("║          %s %s\n", EnvDisplayName, Version)
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println("")

	return nil
}

// freeConsole detaches from the console
func freeConsole() {
	if !consoleAllocated {
		return
	}
	procFreeConsole.Call()
	consoleAllocated = false
}

// showConsoleWindow shows or hides the console window.
// When built as a GUI app (-H=windowsgui), this allocates a console on-demand.
func showConsoleWindow(show bool) {
	if show {
		// Allocate console if we don't have one (GUI app mode)
		if !consoleAllocated {
			if err := allocateConsole(); err != nil {
				// Can't print error - no console yet
				return
			}
		}

		hwnd := getConsoleWindow()
		if hwnd != 0 {
			procShowWindow.Call(hwnd, SW_RESTORE)
			procShowWindow.Call(hwnd, SW_SHOW)
			procSetForegroundWindow.Call(hwnd)
		}
	} else {
		// Just hide the window - don't free the console.
		// Using freeConsole() causes an infinite loop because:
		// 1. freeConsole() invalidates stdout/stderr handles
		// 2. Any fmt.Println() after that causes Windows to auto-reallocate a console
		// 3. The new console triggers close handlers, which call showConsoleWindow(false)
		// 4. Loop continues indefinitely
		// SW_HIDE keeps the console allocated but hidden, avoiding this issue.
		hwnd := getConsoleWindow()
		if hwnd != 0 {
			procShowWindow.Call(hwnd, SW_HIDE)
		}
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
		"DisplayVersion":  Version[1:], // Strip the "v" prefix from version
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
