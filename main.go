package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/getlantern/systray"
)

func main() {
	// Handle --uninstall command line argument (Windows)
	if runtime.GOOS == "windows" && len(os.Args) > 1 {
		if os.Args[1] == "--uninstall" {
			handleUninstall()
			return
		}
	}

	// Handle --update-from=<original_path> argument (self-replacement after update)
	// When the app is launched from a temp path after an auto-update, this flag
	// tells it to copy itself over the original exe and re-launch from there.
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "--update-from=") {
			originalPath := strings.TrimPrefix(arg, "--update-from=")
			if err := performSelfReplace(originalPath); err != nil {
				fmt.Printf("[update] Self-replace failed: %v\n", err)
				// Fall through to run normally from temp path as fallback
			} else {
				return // Successfully re-launched from install path; exit this temp process
			}
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
					if runtime.GOOS == "windows" {
						ShowErrorDialog("Registration Failed", err.Error())
					}
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

				// Show dialog to user (Windows only for now)
				if runtime.GOOS == "windows" {
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
				} else {
					// Non-Windows: just download automatically like before
					if err := downloadAndApplyUpdate(info); err != nil {
						fmt.Println("[update] Download failed:", err)
					}
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
					if runtime.GOOS == "windows" {
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
					}

				case <-mCheck.ClickedCh:
					go func() {
						// Manual check - always check even if we have a pending update
						info, err := checkForNewVersion()
						if err != nil {
							fmt.Println("Update check failed:", err)
							if runtime.GOOS == "windows" {
								ShowErrorDialog("Update Check Failed", err.Error())
							}
							return
						}

						if !info.Available {
							fmt.Println("No update available; current", Version)
							if runtime.GOOS == "windows" {
								ShowInfoDialog("No Update Available",
									fmt.Sprintf("You're running the latest version (%s).", Version))
							}
							return
						}

						// Show dialog for manual check too
						if runtime.GOOS == "windows" {
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
						} else {
							// Non-Windows: download automatically
							if err := downloadAndApplyUpdate(info); err != nil {
								fmt.Println("Update failed:", err)
							}
						}
					}()

				case <-mInstallUpdate.ClickedCh:
					// Install pending update
					if info := GetPendingUpdate(); info != nil {
						fmt.Printf("[update] Installing pending update: %s\n", info.LatestVersion)
						if err := downloadAndApplyUpdate(info); err != nil {
							fmt.Println("[update] Failed:", err)
							if runtime.GOOS == "windows" {
								ShowErrorDialog("Update Failed", err.Error())
							}
						}
					}

				case <-mRegister.ClickedCh:
					// If already registered, show info dialog
					if cfg.IsRegistered() {
						if runtime.GOOS == "windows" {
							ShowInfoDialog("Device Registered",
								fmt.Sprintf("This device is already registered.\n\nAgent ID: %s\nUser: %s",
									cfg.AgentID, cfg.UserID))
						}
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
							if runtime.GOOS == "windows" {
								ShowErrorDialog("Registration Failed", err.Error())
							}
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

	// IMPORTANT: Launch update FIRST, before any cleanup that might kill processes
	// The update process needs to be started before we kill our process tree
	if path, pending := GetUpdateReady(); pending && path != "" {
		fmt.Println("Launching updated version…")

		// Pass our own exe path so the new process can replace us at the install location
		originalExe, err := os.Executable()
		if err != nil {
			fmt.Printf("Warning: could not determine own exe path: %v\n", err)
			originalExe = "" // Fall back to running from temp without replacement
		}

		args := []string{}
		if originalExe != "" {
			args = append(args, fmt.Sprintf("--update-from=%s", originalExe))
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

// aggressiveCleanup forcefully kills child processes that might be stuck.
// This is necessary for updates when long-running commands (like claude -p) are hanging.
// IMPORTANT: Only kills processes spawned by this application, not other user processes.
func aggressiveCleanup() {
	// Get our own PID - we'll kill our process tree
	myPID := os.Getpid()
	fmt.Printf("[cleanup] Killing child processes of PID %d...\n", myPID)

	// Use taskkill /T to kill our entire process tree (all children)
	// This includes any PowerShell, claude, ttyd processes we spawned
	cmd := exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprintf("%d", myPID))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	// Note: This will fail to kill ourselves (which is fine - we're exiting anyway)
	// but it WILL kill all our child processes
	_ = cmd.Run()

	// Also kill ttyd specifically if we have its PID (belt and suspenders)
	if ttydCmd != nil && ttydCmd.Process != nil {
		pid := ttydCmd.Process.Pid
		fmt.Printf("[cleanup] Killing ttyd process tree (PID %d)...\n", pid)
		cmd := exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprintf("%d", pid))
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		_ = cmd.Run()
	}

	// Brief pause to let processes terminate
	time.Sleep(500 * time.Millisecond)
}

/* ---------- self-replace (auto-update) ---------- */

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
	myPath, _ = filepath.EvalSymlinks(myPath)
	resolvedOriginal, _ := filepath.EvalSymlinks(originalPath)

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
		return fmt.Errorf("failed to overwrite %s after retries: %w", originalPath, copyErr)
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
// Uses a temporary file + rename for atomic replacement where possible.
func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	// Write to a temp file in the same directory as dst, then rename
	// This avoids partial writes if the process is interrupted
	dstDir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dstDir, "update_*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	if _, err := io.Copy(tmp, srcFile); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}

	// Make executable
	_ = os.Chmod(tmpName, 0o755)

	// Rename over the target (atomic on most filesystems)
	if err := os.Rename(tmpName, dst); err != nil {
		// On Windows, Rename can fail if dst is still locked. Try direct overwrite.
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

	_, err = io.Copy(dstFile, srcFile)
	return err
}

// cleanupUpdateTempFiles removes leftover agent_update_*.exe files from the temp directory.
// These are created by downloadAndApplyUpdate and may linger after a successful update.
func cleanupUpdateTempFiles() {
	pattern := filepath.Join(os.TempDir(), "agent_update_*")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return
	}

	myPath, _ := os.Executable()
	myPath, _ = filepath.EvalSymlinks(myPath)

	for _, m := range matches {
		resolved, _ := filepath.EvalSymlinks(m)
		// Don't delete ourselves if we're still running from temp (fallback case)
		if strings.EqualFold(resolved, myPath) {
			continue
		}
		if err := os.Remove(m); err == nil {
			fmt.Printf("[update] Cleaned up temp file: %s\n", filepath.Base(m))
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
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow: true,
	}

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
