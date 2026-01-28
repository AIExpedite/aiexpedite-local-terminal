package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

	// NOTE: When built as a GUI app (-H=windowsgui), there's no console window
	// by default. Console will be allocated on-demand when user clicks "Show Console".

	// Ensure auto-start at login (Windows)
	if runtime.GOOS == "windows" {
		_ = ensureAutoStart()
		// Register app in Windows "Installed Apps" for easy uninstall
		_ = ensureAppRegistration()
	}

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

		// Show Console at the top
		mConsole := systray.AddMenuItemCheckbox("Show Console", "Toggle console window visibility", false)
		mAllowList := systray.AddMenuItem("Edit Allow List", "Open allow list configuration folder")
		systray.AddSeparator()

		// Register Device - always visible as checkbox showing registration status
		mRegister := systray.AddMenuItemCheckbox("Register Device", "Connect to your AI Expedite account", cfg.IsRegistered())
		if cfg.IsRegistered() {
			mRegister.Disable() // Can't re-register once registered
		}
		systray.AddSeparator()

		mDisconnect := systray.AddMenuItemCheckbox("Disconnect from cloud", "Stop cloud connection (stay running)", false)
		mCheck := systray.AddMenuItem("Check for Updates", "Check for a new version")

		// Install Update menu item - initially hidden, shown when update is pending
		mInstallUpdate := systray.AddMenuItem("", "")
		mInstallUpdate.Hide()

		systray.AddSeparator()

		// Version display (disabled, just for info)
		mVersion := systray.AddMenuItem("Version "+Version, "Current version")
		mVersion.Disable()

		mQuit := systray.AddMenuItem("Quit", "Exit the agent")

		// Auto-trigger registration on first launch (if not registered)
		autoRegistering := false
		if !cfg.IsRegistered() {
			autoRegistering = true
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
				autoRegistering = false
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

		go func() {
			consoleVisible := !cfg.IsRegistered() // Start visible if auto-registering
			registering := autoRegistering

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

					if registering {
						continue // Already registering
					}
					registering = true

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
						registering = false
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

	// Cleanup persistent PowerShell process (Windows only)
	if runtime.GOOS == "windows" {
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

	// launch downloaded update
	if updatePending && updatePath != "" {
		fmt.Println("Launching updated version…")
		_ = exec.Command(updatePath).Start()
		time.Sleep(1 * time.Second)
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

// createSelfDeleteBatch creates a batch file that waits for this process to exit,
// then deletes the executable and itself. This is the industry-standard approach
// for self-deleting executables on Windows.
func createSelfDeleteBatch(exePath string, quiet bool) error {
	// Create batch file in temp directory
	tempDir := os.TempDir()
	batchPath := filepath.Join(tempDir, "aiexpedite_uninstall_cleanup.bat")

	// Get the directory containing the executable (to delete the whole folder if it's in Program Files)
	exeDir := filepath.Dir(exePath)

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
`, exePath, exeDir)

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
