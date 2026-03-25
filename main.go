package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/getlantern/systray"
)

// shutdownConfig holds the config reference for use in onTrayExit.
// Set once in main() after config is loaded.
var shutdownConfig *Config

func main() {
	// Handle --uninstall command line argument (Windows)
	if runtime.GOOS == "windows" && len(os.Args) > 1 {
		if os.Args[1] == "--uninstall" {
			handleUninstall()
			return
		}
		if os.Args[1] == "--elevated-copy" {
			handleElevatedCopy()
			return
		}
	}

	// Handle --update-from=<original_path> argument (self-replacement after update)
	// When the app is launched from a temp path after an auto-update, this flag
	// tells it to copy itself over the original exe and re-launch from there.
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "--update-from=") {
			originalPath := strings.TrimPrefix(arg, "--update-from=")
			if originalPath == "" {
				fmt.Println("[update] Empty --update-from path, skipping self-replace")
				break
			}
			// Resolve to absolute to avoid ambiguity
			if abs, err := filepath.Abs(originalPath); err == nil {
				originalPath = abs
			}
			if err := validateUpdateTarget(originalPath); err != nil {
				fmt.Printf("[update] Rejecting --update-from path: %v\n", err)
				break // Fall through to run normally from temp path
			}
			if err := performSelfReplace(originalPath); err != nil {
				fmt.Printf("[update] Self-replace failed: %v\n", err)
				// Fall through to run normally from temp path as fallback
			} else {
				return // Successfully re-launched from install path; exit this temp process
			}
			break // Only process the first --update-from flag
		}
	}

	// NOTE: When built as a GUI app (-H=windowsgui), there's no console window
	// by default. Console will be allocated on-demand when user clicks "Show Console".

	// Ensure auto-start at login (Windows)
	if runtime.GOOS == "windows" {
		_ = ensureAutoStart()
		// Register app in Windows "Installed Apps" for easy uninstall
		_ = ensureAppRegistration()

		// Allocate console early for non-prod environments (dev, stg, beta)
		// This ensures all startup output (StartAgent, Pub/Sub) is visible
		// Production builds stay as GUI-only apps with console on-demand
		if EnvName != "prod" {
			allocateConsole()
		}
	}

	// Clean up leftover update temp files from previous updates
	go cleanupUpdateTempFiles()

	// Load or create configuration
	cfg, err := LoadConfig(ConfigPath())
	if err != nil {
		if os.IsNotExist(err) {
			cfg = DefaultConfig()
			_ = cfg.Save(ConfigPath())
		} else {
			cfg = DefaultConfig()
		}
	}

	// Store config for use in onTrayExit shutdown handler
	shutdownConfig = cfg

	// Initialize command allow list for security
	if cfg.EnableAllowList {
		_, _ = InitAllowList()
	}

	// Background workers (includes ttyd installation check)
	go StartAgent(cfg)

	// NOTE: No console hiding needed - GUI app has no console by default.
	// Console is allocated on-demand when user clicks "Show Console" in tray menu.

	// Tray UI
	systray.Run(onTrayReady(cfg), onTrayExit)
}

/* ---------- tray helpers ---------- */

func onTrayReady(cfg *Config) func() {
	return func() {
		systray.SetIcon(iconData)
		systray.SetTitle(EnvDisplayName)
		systray.SetTooltip(EnvDisplayName + " – Remote Command Execution")

		// Mark systray as ready so background goroutines can safely use it
		SetSystrayReady()

		// Show Console at the top - checked if we pre-allocated console for non-prod
		consolePreAllocated := runtime.GOOS == "windows" && EnvName != "prod"
		mConsole := systray.AddMenuItemCheckbox("Show Console", "Toggle console window visibility", consolePreAllocated)

		// Debug Mode - only visible in non-prod environments
		var mDebug *systray.MenuItem
		if EnvName != "prod" {
			mDebug = systray.AddMenuItemCheckbox("Debug Mode", "Show detailed command/response info", cfg.DebugMode)
		}
		systray.AddSeparator()

		// Register Device - always visible as checkbox showing registration status
		mRegister := systray.AddMenuItemCheckbox("Register Device", "Connect to your AI Expedite account", cfg.IsRegistered())
		if cfg.IsRegistered() {
			mRegister.Disable() // Can't re-register once registered
		}
		mDisconnect := systray.AddMenuItemCheckbox("Disconnect from cloud", "Stop cloud connection (stay running)", false)
		systray.AddSeparator()

		mAllowList := systray.AddMenuItem("Edit Allow List", "Open allow list configuration folder")
		mResetAllowList := systray.AddMenuItem("Reset Allow List", "Reset to default allowed commands")
		systray.AddSeparator()

		mCheck := systray.AddMenuItem("Check for Updates", "Check for a new version")

		// Install Update menu item - initially hidden, shown when update is pending
		mInstallUpdate := systray.AddMenuItem("", "")
		mInstallUpdate.Hide()

		// Version display (disabled, just for info)
		mVersion := systray.AddMenuItem("Version "+Version, "Current version")
		mVersion.Disable()
		systray.AddSeparator()

		mQuit := systray.AddMenuItem("Quit", "Exit the agent")

		// registering is true while a registration flow is in progress.
		// Declared here (before the auto-register block) so the goroutine below
		// can safely call registering.Store(false) without a data race.
		var registering atomic.Bool

		// Auto-trigger registration on first launch (if not registered)
		if !cfg.IsRegistered() {
			registering.Store(true)
			// Keep console visible during auto-registration
			if runtime.GOOS == "windows" {
				showConsoleWindow(true)
				mConsole.Check()
			}

			go func() {
				if err := StartRegistration(cfg); err != nil {
					fmt.Println("Registration failed:", err)
					ShowErrorDialog("Registration Failed", err.Error())
				} else {
					// Registration successful - update checkbox and disable
					mRegister.Check()
					mRegister.Disable()
					// Update tooltip to show registered status
					systray.SetTooltip(fmt.Sprintf("%s – Connected as %s", EnvDisplayName, cfg.AgentID))

					// Hide console after successful registration
					if runtime.GOOS == "windows" {
						time.Sleep(2 * time.Second) // Brief delay to let user see success message
						showConsoleWindow(false)
						mConsole.Uncheck()
					}

					// Start Pub/Sub loop with new credentials
					fmt.Println("[pubsub] Starting Pub/Sub loop after successful registration...")
					go StartPubSubLoop(cfg)
				}
				registering.Store(false)
			}()
		}

		// Proactive update check (if AutoUpdate enabled)
		if cfg.AutoUpdate {
			go func() {
				time.Sleep(3 * time.Second) // Let UI stabilize first

				info, err := checkForNewVersion()
				if err != nil {
					fmt.Println("[update] Check failed:", err)
					return
				}

				if !info.Available {
					fmt.Println("[update] No update available")
					return
				}

				// Skip if user previously chose to skip this version
				if info.LatestVersion == cfg.SkippedVersion {
					fmt.Printf("[update] Skipping version %s (user preference)\n", info.LatestVersion)
					return
				}

				fmt.Printf("[update] New version available: %s → %s\n", info.CurrentVersion, info.LatestVersion)

				// Show update dialog to user
				choice := ShowUpdateDialog(info.CurrentVersion, info.LatestVersion)

				switch choice {
				case UpdateNow:
					fmt.Println("[update] User chose: Update Now")
					if err := downloadAndApplyUpdate(info); err != nil {
						fmt.Println("[update] Download failed:", err)
						ShowErrorDialog("Update Failed", err.Error())
					}

				case UpdateLater:
					fmt.Println("[update] User chose: Later")
					SetPendingUpdate(info)
					// Show the Install Update menu item
					mInstallUpdate.SetTitle(fmt.Sprintf("Install Update (%s)", info.LatestVersion))
					mInstallUpdate.SetTooltip("Click to install the pending update")
					mInstallUpdate.Show()

				case SkipVersion:
					fmt.Printf("[update] User chose: Skip version %s\n", info.LatestVersion)
					cfg.SkippedVersion = info.LatestVersion
					_ = cfg.Save(ConfigPath())
				}
			}()
		}

		// Create debug click channel - nil channel blocks forever in select, which is what we want for prod
		var debugClickCh <-chan struct{}
		if mDebug != nil {
			debugClickCh = mDebug.ClickedCh
		}

		go func() {
			// Console is visible if: auto-registering OR pre-allocated for non-prod
			consoleVisible := !cfg.IsRegistered() || consolePreAllocated

			for {
				select {
				case <-mAllowList.ClickedCh:
					// Open the config directory in file explorer
					configDir := GetConfigDir()
					if runtime.GOOS == "windows" {
						exec.Command("explorer", configDir).Start()
					} else if runtime.GOOS == "darwin" {
						exec.Command("open", configDir).Start()
					} else {
						exec.Command("xdg-open", configDir).Start()
					}

				case <-mResetAllowList.ClickedCh:
					// Confirm with user before resetting
					if ShowYesNoDialog("Reset Allow List",
						"This will replace your allowed-commands.txt with the latest defaults.\n\n"+
							"Any custom patterns you added will be lost.\n\n"+
							"Continue?") {
						if err := ResetAllowList(); err != nil {
							ShowErrorDialog("Reset Failed", err.Error())
						} else {
							ShowInfoDialog("Allow List Reset",
								"The allow list has been reset to defaults.\n\n"+
									"New patterns are now active.")
						}
					}

				case <-mCheck.ClickedCh:
					go func() {
						// Manual check - always check even if we have a pending update
						info, err := checkForNewVersion()
						if err != nil {
							fmt.Println("Update check failed:", err)
							ShowErrorDialog("Update Check Failed", err.Error())
							return
						}

						if !info.Available {
							fmt.Println("No update available; current", Version)
							ShowInfoDialog("No Update Available",
								fmt.Sprintf("You're running the latest version (%s).", Version))
							return
						}

						// Show dialog for manual check too
						choice := ShowUpdateDialog(info.CurrentVersion, info.LatestVersion)
						switch choice {
						case UpdateNow:
							if err := downloadAndApplyUpdate(info); err != nil {
								ShowErrorDialog("Update Failed", err.Error())
							}
						case UpdateLater:
							SetPendingUpdate(info)
							mInstallUpdate.SetTitle(fmt.Sprintf("Install Update (%s)", info.LatestVersion))
							mInstallUpdate.SetTooltip("Click to install the pending update")
							mInstallUpdate.Show()
						case SkipVersion:
							cfg.SkippedVersion = info.LatestVersion
							_ = cfg.Save(ConfigPath())
						}
					}()

				case <-mInstallUpdate.ClickedCh:
					// Install pending update
					if info := GetPendingUpdate(); info != nil {
						fmt.Printf("[update] Installing pending update: %s\n", info.LatestVersion)
						if err := downloadAndApplyUpdate(info); err != nil {
							fmt.Println("[update] Failed:", err)
							ShowErrorDialog("Update Failed", err.Error())
						}
					}

				case <-mRegister.ClickedCh:
					// If already registered, show info dialog
					if cfg.IsRegistered() {
						ShowInfoDialog("Device Registered",
							fmt.Sprintf("This device is already registered.\n\nAgent ID: %s\nUser: %s",
								cfg.AgentID, cfg.UserID))
						continue
					}

					if registering.Load() {
						continue // Already registering
					}
					registering.Store(true)

					// Show console during registration
					if runtime.GOOS == "windows" {
						showConsoleWindow(true)
						mConsole.Check()
						consoleVisible = true
					}

					go func() {
						if err := StartRegistration(cfg); err != nil {
							fmt.Println("Registration failed:", err)
							ShowErrorDialog("Registration Failed", err.Error())
						} else {
							// Registration successful - update checkbox and disable
							mRegister.Check()
							mRegister.Disable()
							// Update tooltip to show registered status
							systray.SetTooltip(fmt.Sprintf("%s – Connected as %s", EnvDisplayName, cfg.AgentID))

							// Start Pub/Sub loop with new credentials
							fmt.Println("[pubsub] Starting Pub/Sub loop after successful registration...")
							go StartPubSubLoop(cfg)
						}
						registering.Store(false)
					}()

				case <-mConsole.ClickedCh:
					consoleVisible = !consoleVisible
					if consoleVisible {
						mConsole.Check()
						if runtime.GOOS == "windows" {
							showConsoleWindow(true)
						}
					} else {
						mConsole.Uncheck()
						if runtime.GOOS == "windows" {
							showConsoleWindow(false)
						}
					}

				case <-ConsoleHiddenChan:
					// Console was minimized and hidden to tray - update checkbox state
					consoleVisible = false
					mConsole.Uncheck()

				case <-RegistrationInvalidChan:
					// Registration was invalidated (terminal removed via website)
					// Enable the Register Device menu item so user can re-register
					mRegister.Uncheck()
					mRegister.Enable()

				case <-debugClickCh:
					// Toggle debug mode (only in non-prod)
					cfg.DebugMode = !cfg.DebugMode
					if cfg.DebugMode {
						mDebug.Check()
						fmt.Printf("%s[debug] Debug mode ENABLED - showing detailed command/response info%s\n", colorMagenta, colorReset)
					} else {
						mDebug.Uncheck()
						fmt.Printf("%s[debug] Debug mode DISABLED%s\n", colorMagenta, colorReset)
					}
					// Save to config so it persists
					if err := cfg.Save(ConfigPath()); err != nil {
						fmt.Printf("[debug] Failed to save config: %v\n", err)
					}

				case <-mDisconnect.ClickedCh:
					if mDisconnect.Checked() {
						mDisconnect.Uncheck()
						fmt.Println("[tray] Reconnecting to cloud...")
						SetOffline(false)
						systray.SetTooltip(EnvDisplayName + " – Online")
					} else {
						mDisconnect.Check()
						fmt.Println("[tray] Disconnecting from cloud...")
						SetOffline(true)
						systray.SetTooltip(EnvDisplayName + " – Disconnected")
					}

				case <-mQuit.ClickedCh:
					// Allow the app to actually exit now
					if runtime.GOOS == "windows" {
						SetAllowExit()
					}
					systray.Quit()
					return
				}
			}
		}()
	}
}

func onTrayExit() {
	// signal background goroutines
	close(shutdownChan)

	// Notify server that this agent is going offline (best-effort, 3s timeout)
	if shutdownConfig != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		notifyOffline(ctx, shutdownConfig)
		cancel()
	}

	// IMPORTANT: Launch update FIRST, before any cleanup that might kill processes
	// The update process needs to be started before we kill our process tree
	if path, pending := GetUpdateReady(); pending && path != "" {
		fmt.Println("Launching updated version…")

		// Pass our own exe path so the new process can replace us at the install location.
		// Resolve symlinks so the new process gets the real filesystem path.
		originalExe, err := os.Executable()
		if err != nil {
			fmt.Printf("Warning: could not determine own exe path: %v\n", err)
			originalExe = ""
		} else if resolved, err := filepath.EvalSymlinks(originalExe); err == nil {
			originalExe = resolved
		}

		// If we're already running from a temp path (chained update / previous broken update),
		// don't pass --update-from pointing at the temp file — that would replace a temp file
		// instead of the real install location. Let the new binary start without self-replace.
		args := []string{}
		if originalExe != "" && !isInTempDir(originalExe) {
			args = append(args, fmt.Sprintf("--update-from=%s", originalExe))
		} else if originalExe != "" {
			fmt.Printf("[update] Current exe is in temp dir (%s), skipping --update-from\n", originalExe)
		}

		cmd := exec.Command(path, args...)
		setNewConsole(cmd) // Ensure child process gets a fresh console with valid handles
		if err := cmd.Start(); err != nil {
			fmt.Printf("Failed to start update: %v\n", err)
		}
		// Give the new process time to fully launch before we exit
		time.Sleep(2 * time.Second)
		// Don't do aggressive cleanup after launching update - just exit
		// The new process is already running independently
		return
	}

	// Only do aggressive cleanup when NOT updating (e.g., stuck processes during normal quit)
	// When updating, we skip this to avoid killing the newly launched update process
	if runtime.GOOS == "windows" {
		// Cleanup persistent PowerShell process
		ShutdownPowerShell()
	}

	// Cleanup GCS storage client
	CloseStorageClient()

	// stop ttyd cleanly
	if ttydCmd != nil && ttydCmd.Process != nil {
		_ = ttydCmd.Process.Kill()
		_, _ = ttydCmd.Process.Wait() // avoid zombie
	}

	// kill tmux session (ignore error if it never existed)
	_ = exec.Command("tmux", "kill-session", "-t", tmuxSessionName).Run()
}

// aggressiveCleanup is defined in cleanup_windows.go / cleanup_other.go

/* ---------- elevated copy (auto-update UAC) ---------- */

// handleElevatedCopy performs a file copy with elevated privileges and exits.
// Invoked via: --elevated-copy --copy-from="<src>" --copy-to="<dst>"
// This runs as a minimal elevated process (no UI, no systray) spawned by
// requestElevatedCopy when the app cannot write to its install directory.
func handleElevatedCopy() {
	var copyFrom, copyTo string
	for _, arg := range os.Args[2:] {
		if strings.HasPrefix(arg, "--copy-from=") {
			copyFrom = strings.TrimPrefix(arg, "--copy-from=")
		}
		if strings.HasPrefix(arg, "--copy-to=") {
			copyTo = strings.TrimPrefix(arg, "--copy-to=")
		}
	}
	if copyFrom == "" || copyTo == "" {
		fmt.Println("[elevated-copy] Missing --copy-from or --copy-to")
		os.Exit(1)
	}
	if err := validateUpdateTarget(copyTo); err != nil {
		fmt.Printf("[elevated-copy] Validation failed: %v\n", err)
		os.Exit(2)
	}
	if err := copyFile(copyFrom, copyTo); err != nil {
		fmt.Printf("[elevated-copy] Copy failed: %v\n", err)
		os.Exit(3)
	}
	fmt.Printf("[elevated-copy] Successfully copied %s -> %s\n", copyFrom, copyTo)
	os.Exit(0)
}

/* ---------- self-replace (auto-update) ---------- */

// validateUpdateTarget checks that the target path for self-replacement is safe.
// Rejects paths outside the user's profile or in protected system directories.
func validateUpdateTarget(target string) error {
	abs, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("cannot resolve path: %w", err)
	}
	lower := strings.ToLower(filepath.Clean(abs))

	// Must end in .exe on Windows
	if runtime.GOOS == "windows" && !strings.HasSuffix(lower, ".exe") {
		return fmt.Errorf("target is not an executable: %s", target)
	}

	// Block known protected directories (use trailing separator to avoid
	// false positives like "c:\windowsapps").
	// NOTE: c:\program files\ is intentionally NOT blocked — Inno Setup
	// installs the app there by default ({autopf}), so auto-update must
	// be able to self-replace the exe at its install location.
	blockedPrefixes := []string{
		`c:\windows\`,
		`c:\programdata\`,
	}
	for _, prefix := range blockedPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return fmt.Errorf("target is in protected directory: %s", target)
		}
	}
	// Also block root-level executables (e.g., c:\cmd.exe)
	if filepath.Dir(lower) == `c:\` {
		return fmt.Errorf("target is in root directory: %s", target)
	}

	// Verify the target already exists and is a regular file (we're replacing, not creating)
	info, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("target does not exist: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("target is a directory: %s", target)
	}

	return nil
}

// isInTempDir returns true if the given path is inside the OS temp directory.
func isInTempDir(path string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	tmpDir, err := filepath.Abs(os.TempDir())
	if err != nil {
		return false
	}
	// Ensure trailing separator so "C:\Temp" doesn't match "C:\Temporary\..."
	if !strings.HasSuffix(tmpDir, string(filepath.Separator)) {
		tmpDir += string(filepath.Separator)
	}
	return strings.HasPrefix(strings.ToLower(absPath), strings.ToLower(tmpDir))
}

// performSelfReplace copies the currently running binary (from a temp path) over
// the original install-path exe, then re-launches from the install path.
// This is called when the app starts with --update-from=<original_path>.
//
// Flow:
//  1. Old version downloads new binary to temp, quits, launches temp binary with --update-from
//  2. This function (in the temp binary) waits for the old process to release the exe file
//  3. Copies itself over the original path
//  4. Launches the original path (now the new version) without --update-from
//  5. Returns nil so the caller can exit the temp process
func performSelfReplace(originalPath string) error {
	myPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine own path: %w", err)
	}

	// Resolve symlinks so we compare real paths
	if resolved, err := filepath.EvalSymlinks(myPath); err == nil {
		myPath = resolved
	}
	resolvedOriginal := originalPath
	if resolved, err := filepath.EvalSymlinks(originalPath); err == nil {
		resolvedOriginal = resolved
	}

	// Safety: if we're already running from the install path, skip replacement
	if strings.EqualFold(myPath, resolvedOriginal) {
		fmt.Println("[update] Already running from install path, skipping self-replace")
		return fmt.Errorf("already at install path")
	}

	fmt.Printf("[update] Self-replacing: %s → %s\n", myPath, originalPath)

	// Wait for the old process to fully exit and release the file
	// Retry copying for up to 10 seconds
	var copyErr error
	for attempt := 0; attempt < 20; attempt++ {
		copyErr = copyFile(myPath, originalPath)
		if copyErr == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if copyErr != nil {
		fmt.Printf("[update] Direct copy failed (%v), attempting elevated copy...\n", copyErr)
		if elevErr := requestElevatedCopy(myPath, originalPath); elevErr != nil {
			return fmt.Errorf("failed to overwrite %s (direct: %v, elevated: %v)", originalPath, copyErr, elevErr)
		}
	}

	fmt.Printf("[update] Successfully replaced %s\n", originalPath)

	// Re-launch from the install path (no --update-from flag this time)
	cmd := exec.Command(originalPath)
	setNewConsole(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to re-launch from install path: %w", err)
	}

	fmt.Println("[update] Re-launched from install path, exiting temp process")
	return nil
}

// copyFile copies src to dst, overwriting dst if it exists.
// Uses a temporary file + rename for near-atomic replacement where possible.
// On Windows, os.Rename fails if dst exists or if src and dst are on different
// drives, so we remove dst first and fall back to direct copy if needed.
func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	// Write to a temp file in the same directory as dst, then rename.
	// This avoids partial writes if the process is interrupted.
	dstDir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dstDir, "update_*.tmp")
	if err != nil {
		// Can't write to the install dir — fall back to direct overwrite
		return directCopy(src, dst)
	}
	tmpName := tmp.Name()

	if _, err := io.Copy(tmp, srcFile); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	// Flush to disk before close so the binary is fully written before rename
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}

	_ = os.Chmod(tmpName, 0o755)

	// On Windows, os.Rename cannot overwrite an existing file.
	// Remove dst first, then rename. If remove fails (file locked), fall back.
	if runtime.GOOS == "windows" {
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			os.Remove(tmpName)
			return directCopy(src, dst)
		}
	}

	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return directCopy(src, dst)
	}

	return nil
}

// directCopy is a fallback that overwrites dst directly (non-atomic).
func directCopy(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	if _, err = io.Copy(dstFile, srcFile); err != nil {
		return err
	}
	// Flush to disk so the binary isn't truncated if the process exits immediately
	return dstFile.Sync()
}

// cleanupUpdateTempFiles removes leftover update artifacts:
//   - agent_update_*.exe in the OS temp directory (downloaded update binaries)
//   - update_*.tmp in the install directory (partial copy artifacts from copyFile)
func cleanupUpdateTempFiles() {
	myPath, _ := os.Executable()
	myPath, _ = filepath.EvalSymlinks(myPath)

	// Pattern 1: Downloaded update binaries in OS temp dir
	cleanupGlob(filepath.Join(os.TempDir(), "agent_update_*"), myPath)

	// Pattern 2: Partial copy temp files in our install directory
	if myPath != "" {
		installDir := filepath.Dir(myPath)
		cleanupGlob(filepath.Join(installDir, "update_*.tmp"), myPath)
	}
}

// cleanupGlob removes files matching the glob pattern, skipping our own executable.
func cleanupGlob(pattern, selfPath string) {
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return
	}
	for _, m := range matches {
		resolved, _ := filepath.EvalSymlinks(m)
		if selfPath != "" && strings.EqualFold(resolved, selfPath) {
			continue // Don't delete ourselves
		}
		if err := os.Remove(m); err == nil {
			fmt.Printf("[update] Cleaned up: %s\n", filepath.Base(m))
		}
	}
}

/* ---------- util ---------- */

// handleUninstall performs the uninstall process when --uninstall flag is passed
func handleUninstall() {
	quiet := len(os.Args) > 2 && os.Args[2] == "--quiet"

	if !quiet {
		fmt.Println("")
		fmt.Println("╔════════════════════════════════════════════════════════════╗")
		fmt.Printf("║          %s - Uninstalling...\n", EnvDisplayName)
		fmt.Println("╚════════════════════════════════════════════════════════════╝")
		fmt.Println("")
	}

	// Remove registry entries (auto-start and Installed Apps)
	if err := unregisterApp(); err != nil {
		if !quiet {
			fmt.Println("Warning: Failed to remove registry entries:", err)
		}
	} else {
		if !quiet {
			fmt.Println("→ Removed from Windows Installed Apps")
			fmt.Println("→ Removed from Windows auto-start")
		}
	}

	// Remove config.json only (keep allowed-commands.txt for re-registration)
	configFile := ConfigPath()
	if err := os.Remove(configFile); err != nil && !os.IsNotExist(err) {
		if !quiet {
			fmt.Println("Warning: Failed to remove config file:", err)
		}
	} else {
		if !quiet {
			fmt.Println("→ Removed config file:", configFile)
		}
	}

	// Self-delete: Create a batch file that deletes the executable after this process exits
	exePath, err := os.Executable()
	if err == nil {
		if err := createSelfDeleteBatch(exePath, quiet); err != nil {
			if !quiet {
				fmt.Println("Warning: Could not create self-delete script:", err)
			}
		} else {
			if !quiet {
				fmt.Println("→ Scheduled executable for deletion")
			}
		}
	}

	if !quiet {
		fmt.Println("")
		fmt.Println("╔════════════════════════════════════════════════════════════╗")
		fmt.Println("║          Uninstall complete!                               ║")
		fmt.Println("╚════════════════════════════════════════════════════════════╝")
		fmt.Println("")

		// Show dialog to user
		ShowInfoDialog(
			"Uninstall Complete",
			"AI Expedite Terminal has been uninstalled.\n\n"+
				"Your allowed-commands.txt file has been preserved.",
		)
	}
}

// escapeBatchPath escapes a filesystem path for safe embedding inside a
// Windows batch file command (e.g. inside a "del /f /q "<path>" line).
// It escapes % → %% (batch variable expansion) and removes ^ and ! which
// could be interpreted as escape / delayed-expansion metacharacters.
// The path is still wrapped in double-quotes by the caller; this function
// only handles characters that are dangerous inside already-quoted strings.
func escapeBatchPath(p string) string {
	// % must be doubled so cmd.exe does not try to expand %SOMETHING% tokens.
	p = strings.ReplaceAll(p, "%", "%%")
	// ^ is the batch escape character — strip it; it should never appear in a
	// normal Windows file path produced by os.Executable / filepath.Dir.
	p = strings.ReplaceAll(p, "^", "")
	// ! is only active with SETLOCAL ENABLEDELAYEDEXPANSION (not used here),
	// but strip it defensively.
	p = strings.ReplaceAll(p, "!", "")
	return p
}

// createSelfDeleteBatch creates a batch file that waits for this process to exit,
// then deletes the executable and itself. This is the industry-standard approach
// for self-deleting executables on Windows.
func createSelfDeleteBatch(exePath string, quiet bool) error {
	// Create batch file in temp directory
	tempDir := os.TempDir()
	batchPath := filepath.Join(tempDir, "aiexpedite_uninstall_cleanup.bat")

	// Get the directory containing the executable (to delete the whole folder if it's in Program Files)
	exeDir := filepath.Dir(exePath)

	// Escape paths before embedding them in the batch script.
	// Without this, a % in the path would be interpreted as a batch variable
	// expansion token and silently break the del/rmdir commands.
	safeExePath := escapeBatchPath(exePath)
	safeExeDir := escapeBatchPath(exeDir)

	// Batch script that:
	// 1. Waits for the main process to exit (using ping for delay)
	// 2. Deletes the executable
	// 3. Attempts to remove the installation directory (if empty)
	// 4. Deletes itself
	batchContent := fmt.Sprintf(`@echo off
REM AI Expedite Terminal - Cleanup Script
REM This script deletes the executable after the uninstaller exits

REM Wait for the main process to exit (3 second delay)
ping -n 4 127.0.0.1 > nul

REM Delete the executable
del /f /q "%s" > nul 2>&1

REM Try to remove the installation directory (will only succeed if empty)
rmdir "%s" > nul 2>&1

REM Delete this batch file
del /f /q "%%~f0" > nul 2>&1
`, safeExePath, safeExeDir)

	// Write the batch file
	if err := os.WriteFile(batchPath, []byte(batchContent), 0644); err != nil {
		return err
	}

	// Execute the batch file in the background (hidden window)
	cmd := exec.Command("cmd", "/c", "start", "/b", "", batchPath)
	hideWindow(cmd)

	return cmd.Start()
}

// openBrowser opens the supplied URL in the platform's default browser.
func openBrowser(url string) {
	var cmd string
	var args []string

	switch runtime.GOOS {
	case "windows":
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", url}
	case "darwin":
		cmd = "open"
		args = []string{url}
	default: // Linux and others
		cmd = "xdg-open"
		args = []string{url}
	}
	_ = exec.Command(cmd, args...).Start()
}
