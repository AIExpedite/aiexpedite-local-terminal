//go:build windows
// +build windows

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	_ "embed"

	"github.com/getlantern/systray"
	"golang.org/x/sys/windows/registry"
)

//go:embed assets/aiexpedite-tray-icon.ico
var iconData []byte

//go:embed assets/aiexpedite-tray-icon-disconnected.ico
var iconDataDisconnected []byte

// applyStandardTrayIcon sets the tray icon to the normal "connected" icon.
// Called on reconnect from the tray "Disconnect from cloud" toggle.
func applyStandardTrayIcon() {
	if !IsSystrayReady() {
		return
	}
	systray.SetIcon(iconData)
}

// applyDisconnectedTrayIcon sets the tray icon to the "disconnected" variant
// (red prohibition-sign overlay). Called on disconnect from the tray toggle so
// the user can tell at a glance that the agent is no longer talking to the
// cloud — a corner dot read as a pending message instead.
func applyDisconnectedTrayIcon() {
	if !IsSystrayReady() {
		return
	}
	systray.SetIcon(iconDataDisconnected)
}

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

	// For detecting whether the console window is currently shown
	procIsWindowVisible = user32.NewProc("IsWindowVisible")
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
	SW_HIDE           = 0
	SW_SHOWNORMAL     = 1
	SW_SHOW           = 5
	SW_MINIMIZE       = 6
	SW_SHOWNOACTIVATE = 8
	SW_RESTORE        = 9
	SW_SHOWDEFAULT    = 10

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
		// allowAppExit is true (set by tray Quit). Run the graceful disconnect
		// sequence so the backend learns we're going offline before Windows
		// terminates the process — Windows kills console apps very quickly
		// once the handler returns on CTRL_CLOSE_EVENT, so onTrayExit may not
		// finish in time. This duplicates the work but gracefulShutdown is
		// idempotent (shutdownInProgress guard), so the second pass is a noop.
		fmt.Println("[console] Close event with allowAppExit=true — running gracefulShutdown")
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		gracefulShutdown(ctx, shutdownConfig)
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
		// System is logging off or shutting down. We have a very narrow
		// window before Windows force-kills us, but it's worth attempting
		// the offline notify so the dot flips quickly when the user logs
		// out / shuts down rather than waiting for the lastSeen staleness
		// check on the next poll.
		fmt.Println("[console] System logoff/shutdown — attempting graceful shutdown")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		gracefulShutdown(ctx, shutdownConfig)
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
	IMAGE_ICON      = 1
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

	// Point Go's stdout/stderr at the console. If the log tee is active, just
	// re-point its console mirror (os.Stdout stays the tee pipe) so diagnostics
	// keep reaching BOTH the console and agent.log; otherwise fall back to
	// replacing os.Stdout/os.Stderr directly (logtee.go).
	conoutOut := os.NewFile(uintptr(conout), "stdout")
	conoutErr := os.NewFile(uintptr(conout), "stderr")
	if !setLogTeeConsole(conoutOut, conoutErr) {
		os.Stdout = conoutOut
		os.Stderr = conoutErr
	}

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

// consoleWindowVisible reports whether a console window currently exists and is
// visible on screen, so callers can capture the pre-existing visibility and
// restore it later (e.g. around a dependency install that forces the console
// open). It queries the console window directly rather than gating on the
// consoleAllocated bookkeeping flag: a process relaunched with CREATE_NEW_CONSOLE
// (see setNewConsole) inherits a real, visible console without ever calling
// allocateConsole, so consoleAllocated can be false while a visible console
// exists. Reading the flag there would misreport that console as hidden and the
// install's deferred restore would hide an originally-visible window.
func consoleWindowVisible() bool {
	consoleVisibilityMu.Lock()
	defer consoleVisibilityMu.Unlock()
	return consoleWindowVisibleLocked()
}

func consoleWindowVisibleLocked() bool {
	hwnd := getConsoleWindow()
	if hwnd == 0 {
		return false
	}
	ret, _, _ := procIsWindowVisible.Call(hwnd)
	return ret != 0
}

// consoleVisibilityMu serializes every console visibility read/mutation so a
// snapshot of the current state and the sequence claim that follows it can be
// made atomic (see snapshotAndShowConsole). Without it there is a TOCTOU gap
// between reading consoleWindowVisible() and claiming a sequence number, and a
// concurrent showConsoleWindow(true) landing in that gap would receive an
// *earlier* sequence than the installer — invisible to both the prior-visible
// snapshot and the "changed since" check — so the deferred restore would hide a
// console the tray legitimately requested (leaving it hidden behind a checked
// "Show Console" menu item).
var consoleVisibilityMu sync.Mutex

// consoleVisibilitySeq counts every visibility request made through
// showConsoleWindow. A caller that forces the console open for the duration of
// a long operation (the dependency installer) snapshots the sequence of its own
// request and only restores the prior state if no later request arrived. The
// tray runs concurrently with StartAgent, so auto-registration or the user's
// "Show Console" menu item can legitimately ask for the console mid-install;
// without this check the installer's deferred restore would undo that newer
// request and leave a hidden window behind a checked menu item.
var consoleVisibilitySeq atomic.Uint64

// consoleVisibilityChangedSince reports whether any visibility request was made
// after the one identified by seq (as returned by showConsoleWindowSeq).
func consoleVisibilityChangedSince(seq uint64) bool {
	return consoleVisibilitySeq.Load() != seq
}

// snapshotAndShowConsole atomically captures whether the console is currently
// visible and then issues a show request, returning the prior visibility and
// the sequence number of that show request. Holding consoleVisibilityMu across
// both steps closes the TOCTOU gap the installer's deferred restore depends on:
// any concurrent showConsoleWindow request is now ordered either fully before
// this call (and thus reflected in priorVisible) or fully after it (and thus
// detectable via consoleVisibilityChangedSince), never interleaved between the
// snapshot and the sequence claim.
func snapshotAndShowConsole() (priorVisible bool, seq uint64) {
	consoleVisibilityMu.Lock()
	defer consoleVisibilityMu.Unlock()
	priorVisible = consoleWindowVisibleLocked()
	seq = showConsoleWindowSeqLocked(true)
	return priorVisible, seq
}

// restoreConsoleVisibility undoes a snapshotAndShowConsole claim: it hides the
// console only when it was hidden before the claim AND no visibility request
// arrived after ours (a newer request — auto-registration or the tray's "Show
// Console" toggle landing mid-install — wins, since hiding on our stale
// snapshot would leave a hidden window behind a checked menu item).
//
// The "changed since" check and the hide are one atomic step for the same
// reason the snapshot and claim are: checking outside the lock leaves a gap in
// which a concurrent show can land after the check and be hidden right back.
func restoreConsoleVisibility(priorVisible bool, seq uint64) {
	if priorVisible {
		return
	}
	consoleVisibilityMu.Lock()
	defer consoleVisibilityMu.Unlock()
	if consoleVisibilityChangedSince(seq) {
		return
	}
	showConsoleWindowSeqLocked(false)
}

// showConsoleWindow shows or hides the console window.
// When built as a GUI app (-H=windowsgui), this allocates a console on-demand.
func showConsoleWindow(show bool) { showConsoleWindowSeq(show) }

// showConsoleWindowSeq is showConsoleWindow plus the sequence number of this
// request, for callers that later need to know whether theirs is still the most
// recent one. The counter is bumped even when the request can't be carried out
// (no console allocatable), so a stale snapshot never wins over a newer intent.
func showConsoleWindowSeq(show bool) uint64 {
	consoleVisibilityMu.Lock()
	defer consoleVisibilityMu.Unlock()
	return showConsoleWindowSeqLocked(show)
}

// showConsoleWindowSeqLocked is the body of showConsoleWindowSeq; callers must
// already hold consoleVisibilityMu (e.g. snapshotAndShowConsole).
func showConsoleWindowSeqLocked(show bool) uint64 {
	seq := consoleVisibilitySeq.Add(1)
	if show {
		// Allocate console if we don't have one (GUI app mode)
		if !consoleAllocated {
			if err := allocateConsole(); err != nil {
				// Can't print error - no console yet
				return seq
			}
		}

		hwnd := getConsoleWindow()
		if hwnd != 0 {
			procShowWindow.Call(hwnd, SW_RESTORE)
			procShowWindow.Call(hwnd, SW_SHOW)
			procSetForegroundWindow.Call(hwnd)
		}
	} else {
		// Nothing to hide if a console was never allocated — e.g. the prod GUI
		// app that never showed a window. This makes hideAfterSetup a safe
		// no-op after setup when there is no window to minimize.
		hwnd := getConsoleWindow()
		if hwnd == 0 {
			return seq
		}
		// Just hide the window - don't free the console.
		// Using freeConsole() causes an infinite loop because:
		// 1. freeConsole() invalidates stdout/stderr handles
		// 2. Any fmt.Println() after that causes Windows to auto-reallocate a console
		// 3. The new console triggers close handlers, which call showConsoleWindow(false)
		// 4. Loop continues indefinitely
		// SW_HIDE keeps the console allocated but hidden, avoiding this issue.
		procShowWindow.Call(hwnd, SW_HIDE)
	}
	return seq
}

// hideMinimizedConsole hides the console window if it is currently minimized,
// performing the "is it minimized" check and the hide as ONE step under
// consoleVisibilityMu. Routing the minimize-driven hide through
// showConsoleWindowSeqLocked (rather than calling procShowWindow directly) is
// what keeps it part of the same serialization protocol as every other
// visibility mutation: it both takes the mutex and bumps consoleVisibilitySeq.
//
// Without that, a minimize landing inside snapshotAndShowConsole could hide the
// console after the prior-visibility read but before the installer's show — the
// installer would reopen the window, record it as previously visible, and its
// deferred restore would leave it open, silently discarding the user's minimize
// while ConsoleHiddenChan had already unchecked the tray's "Show Console" item.
// Now the hide is ordered either fully before the snapshot (so priorVisible is
// false and the restore hides again) or fully after the sequence claim (so
// consoleVisibilityChangedSince makes the restore stand down).
//
// Returns true when it actually hid the window, so the caller only notifies the
// tray on a real state change.
func hideMinimizedConsole() bool {
	consoleVisibilityMu.Lock()
	defer consoleVisibilityMu.Unlock()

	hwnd := getConsoleWindow()
	if hwnd == 0 {
		return false
	}
	// IsIconic returns non-zero if window is minimized.
	if ret, _, _ := procIsIconic.Call(hwnd); ret == 0 {
		return false
	}
	showConsoleWindowSeqLocked(false)
	return true
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

		// Window was minimized - hide it to tray instead. The check and the
		// hide happen together under consoleVisibilityMu (see
		// hideMinimizedConsole) so this mutation can't race the installer's
		// visibility snapshot.
		if !hideMinimizedConsole() {
			continue
		}

		// Notify main.go to update the checkbox state
		select {
		case ConsoleHiddenChan <- true:
		default:
			// Channel full, skip (non-blocking)
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
	// Use environment-specific display name (e.g., "AI Expedite (Dev)" for dev)
	values := map[string]string{
		"DisplayName":          EnvDisplayName,
		"DisplayVersion":       Version[1:], // Strip the "v" prefix from version
		"Publisher":            "AI Expedite",
		"InstallLocation":      exeDir,
		"DisplayIcon":          iconPath,
		"UninstallString":      `"` + exePath + `" --uninstall`,
		"QuietUninstallString": `"` + exePath + `" --uninstall --quiet`,
		"NoModify":             "",
		"NoRepair":             "",
		"Comments":             "Remote terminal access with security features",
		"URLInfoAbout":         "https://aiexpedite.com",
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

// NOTE: the UAC elevation helper (runElevatedAndWait + its SHELLEXECUTEINFOW
// plumbing) was removed with the move to a per-user install — the auto-update
// path no longer elevates. Reintroducing elevation here would re-open the exact
// UAC prompt the silent-update feature exists to eliminate.
