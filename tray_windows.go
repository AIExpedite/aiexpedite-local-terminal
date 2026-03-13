//go:build windows
// +build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
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
	procSetStdHandle          = kernel32.NewProc("SetStdHandle")
	procSetConsoleMode        = kernel32.NewProc("SetConsoleMode")
	procGetConsoleMode        = kernel32.NewProc("GetConsoleMode")
	procShowWindow            = user32.NewProc("ShowWindow") // user32 declared in approval_windows.go

	// For setting console window icon
	procSendMessageW = user32.NewProc("SendMessageW")

	// For detecting minimized window state
	procIsIconic = user32.NewProc("IsIconic")
)

// Standard handle constants for GetStdHandle/SetStdHandle
const (
	STD_INPUT_HANDLE  = ^uintptr(9)  // -10
	STD_OUTPUT_HANDLE = ^uintptr(10) // -11
	STD_ERROR_HANDLE  = ^uintptr(11) // -12
)

// Console mode flags for ANSI escape code support
const (
	ENABLE_VIRTUAL_TERMINAL_PROCESSING = 0x0004
)

// Track if we've allocated a console
var consoleAllocated = false

// Channel to notify main.go when console visibility changes (e.g., minimized to tray)
var ConsoleHiddenChan = make(chan bool, 1)

// Channel to notify main.go when registration is invalidated (agent deleted from backend)
var RegistrationInvalidChan = make(chan bool, 1)

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

// loadIconFromFile loads an icon from a .ico file using LoadImage API
// This is more reliable than parsing the ICO format manually
var procLoadImageW = user32.NewProc("LoadImageW")

const (
	IMAGE_ICON    = 1
	LR_LOADFROMFILE = 0x00000010
)

func loadIconFromFile(path string, width, height int) uintptr {
	pathPtr, _ := syscall.UTF16PtrFromString(path)
	hIcon, _, _ := procLoadImageW.Call(
		0, // hInstance - NULL for loading from file
		uintptr(unsafe.Pointer(pathPtr)),
		IMAGE_ICON,
		uintptr(width),
		uintptr(height),
		LR_LOADFROMFILE|LR_DEFAULTCOLOR,
	)
	return hIcon
}

// consoleIconPath stores the path to the temp icon file
var consoleIconPath string

// ensureIconFile writes the embedded icon to a temp file if not already done
func ensureIconFile() string {
	if consoleIconPath != "" {
		return consoleIconPath
	}

	// Write to config directory for persistence
	iconPath := filepath.Join(GetConfigDir(), "console-icon.ico")
	if err := os.WriteFile(iconPath, iconData, 0644); err != nil {
		return ""
	}
	consoleIconPath = iconPath
	return iconPath
}

// setConsoleIcon sets both small and large icons on the console window
func setConsoleIcon() {
	hwnd := getConsoleWindow()
	if hwnd == 0 {
		return
	}

	// Ensure icon file exists
	iconPath := ensureIconFile()
	if iconPath == "" {
		return
	}

	// Create small icon (16x16) for title bar and taskbar
	hIconSmall := loadIconFromFile(iconPath, 16, 16)
	if hIconSmall != 0 {
		procSendMessageW.Call(hwnd, WM_SETICON, ICON_SMALL, hIconSmall)
	}

	// Create large icon (32x32) for Alt+Tab
	hIconBig := loadIconFromFile(iconPath, 32, 32)
	if hIconBig != 0 {
		procSendMessageW.Call(hwnd, WM_SETICON, ICON_BIG, hIconBig)
	}
}

// setConsoleTitle sets the console window title
var procSetConsoleTitleW = kernel32.NewProc("SetConsoleTitleW")

func setConsoleTitle(title string) {
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	procSetConsoleTitleW.Call(uintptr(unsafe.Pointer(titlePtr)))
}

// allocateConsole creates a new console window for this GUI process
func allocateConsole() error {
	if consoleAllocated {
		return nil
	}

	// Check if we already have a console (e.g., created via CREATE_NEW_CONSOLE when
	// launching an updated executable). If so, we need to properly connect stdout/stderr.
	existingConsole := getConsoleWindow() != 0

	if !existingConsole {
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
	}

	// Open CONOUT$ to get a valid handle to the console output buffer.
	// This is necessary because:
	// 1. When CREATE_NEW_CONSOLE creates a console, GetStdHandle may return invalid handles
	// 2. Opening CONOUT$ directly always gives us a valid console output handle
	conout, err := syscall.Open("CONOUT$", syscall.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("failed to open CONOUT$: %v", err)
	}

	// Set the process's standard handles to point to the console
	procSetStdHandle.Call(STD_OUTPUT_HANDLE, uintptr(conout))
	procSetStdHandle.Call(STD_ERROR_HANDLE, uintptr(conout))

	// Enable ANSI escape code support for colored output
	enableANSISupport(uintptr(conout))

	// Update Go's os.Stdout and os.Stderr to use the console
	os.Stdout = os.NewFile(uintptr(conout), "stdout")
	os.Stderr = os.NewFile(uintptr(conout), "stderr")

	consoleAllocated = true

	// Disable the close button on the console
	disableConsoleCloseButton()

	// Set up console control handler
	initConsoleHandler()

	// Set the console window icon and title
	setConsoleIcon()
	setConsoleTitle(EnvDisplayName + " " + Version)

	// Print startup banner now that we have a console
	fmt.Println("")
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Printf("║          %s %s\n", EnvDisplayName, Version)
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println("")

	// Start monitoring for minimize events (hides to tray instead)
	go monitorConsoleMinimize()

	return nil
}

// enableANSISupport enables ANSI escape code processing for colored output
func enableANSISupport(handle uintptr) {
	var mode uint32
	procGetConsoleMode.Call(handle, uintptr(unsafe.Pointer(&mode)))
	mode |= ENABLE_VIRTUAL_TERMINAL_PROCESSING
	procSetConsoleMode.Call(handle, uintptr(mode))
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

// monitorConsoleMinimize watches for minimize events and hides console to tray.
// This runs as a goroutine and polls the window state periodically.
// Exits when shutdownChan is closed to prevent a leaked goroutine on app exit.
func monitorConsoleMinimize() {
	ticker := time.NewTicker(100 * time.Millisecond) // 10 checks per second
	defer ticker.Stop()

	for {
		select {
		case <-shutdownChan:
			return
		case <-ticker.C:
		}

		if !consoleAllocated {
			continue
		}

		hwnd := getConsoleWindow()
		if hwnd == 0 {
			continue
		}

		// IsIconic returns non-zero if window is minimized
		ret, _, _ := procIsIconic.Call(hwnd)
		if ret != 0 {
			// Window was minimized - hide it to tray instead
			procShowWindow.Call(hwnd, SW_HIDE)

			// Notify main.go to update the checkbox state
			select {
			case ConsoleHiddenChan <- true:
			default:
				// Channel full, skip (non-blocking)
			}
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
	// Don't register temp directory paths in auto-start — this happens when
	// auto-update falls through and runs from %TEMP% as a fallback.
	if isInTempDir(exePath) {
		return nil
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
	// Don't register in Installed Apps when running from a temp directory —
	// this happens when auto-update falls through and runs from %TEMP%.
	if isInTempDir(exePath) {
		return nil
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
