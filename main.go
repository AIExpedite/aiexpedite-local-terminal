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

	// Set up console close handler early (Windows)
	// This prevents clicking X on console from closing the app
	if runtime.GOOS == "windows" {
		initConsoleHandler()
	}

	// Show startup message (console visible initially for first-time setup)
	fmt.Println("")
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Printf("║          %s %s - Starting...\n", EnvDisplayName, version)
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println("")

	// Ensure auto‑start at login (Windows)
	if runtime.GOOS == "windows" {
		if err := ensureAutoStart(); err != nil {
			fmt.Println("AutoStart setup failed:", err)
		}
		// Register app in Windows "Installed Apps" for easy uninstall
		if err := ensureAppRegistration(); err != nil {
			fmt.Println("App registration failed:", err)
		}
	}

	// Load or create configuration
	cfg, err := LoadConfig(ConfigPath())
	if err != nil {
		if os.IsNotExist(err) {
			cfg = DefaultConfig()
			_ = cfg.Save(ConfigPath())
			fmt.Println("→ Created default config at", ConfigPath())
		} else {
			fmt.Println("Error loading config:", err)
			cfg = DefaultConfig()
		}
	} else {
		fmt.Println("→ Loaded config from", ConfigPath())
	}

	// Initialize command allow list for security
	if cfg.EnableAllowList {
		al, err := InitAllowList()
		if err != nil {
			fmt.Println("Warning: Failed to initialize allow list:", err)
		} else {
			fmt.Println("→ Command allow list loaded from", al.GetConfigPath())
		}
	} else {
		fmt.Println("Warning: Command allow list is DISABLED - all commands will execute without validation")
	}

	// Check registration status
	if cfg.IsRegistered() {
		fmt.Printf("→ Device registered as: %s\n", cfg.AgentID)
		fmt.Printf("→ Connected to user: %s\n", cfg.UserID)
	} else {
		fmt.Println("")
		fmt.Println("⚠ This device is not registered.")
		fmt.Println("  Use the 'Register Device' option in the system tray menu")
		fmt.Println("  to connect to your AI Expedite account.")
		fmt.Println("")
	}

	// Background workers (includes ttyd installation check)
	go StartAgent(cfg)

	// Hide console after successful startup (Windows)
	// User can show it via tray menu
	if runtime.GOOS == "windows" {
		// Small delay to let user see startup messages
		time.Sleep(500 * time.Millisecond)
		showConsoleWindow(false)
	}

	// Tray UI
	systray.Run(onTrayReady(cfg), onTrayExit)
}

/* ---------- tray helpers ---------- */

func onTrayReady(cfg *Config) func() {
	return func() {
		systray.SetIcon(iconData)
		systray.SetTitle(EnvDisplayName)
		systray.SetTooltip(EnvDisplayName + " – Remote Command Execution")

		mOpen := systray.AddMenuItem("Open Terminal", "Open terminal in browser")
		mCheck := systray.AddMenuItem("Check for Updates", "Check for a new version")
		systray.AddSeparator()

		// Add Register Device option (only shown when not registered)
		var mRegister *systray.MenuItem
		if !cfg.IsRegistered() {
			mRegister = systray.AddMenuItem("Register Device", "Connect to your AI Expedite account")
			systray.AddSeparator()
		}

		mConsole := systray.AddMenuItemCheckbox("Show Console", "Toggle console window visibility", false)
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Quit", "Exit the agent")

		go func() {
			consoleVisible := false
			registering := false

			for {
				select {
				case <-mOpen.ClickedCh:
					url := fmt.Sprintf("http://127.0.0.1:%d", cfg.LocalTtydPort)
					if cfg.LocalTtydPort == 0 {
						url = "http://127.0.0.1:7681"
					}
					openBrowser(url)

				case <-mCheck.ClickedCh:
					go func() {
						if err := checkForUpdate(); err != nil {
							fmt.Println("Update check failed:", err)
						}
					}()

				case <-func() chan struct{} {
					if mRegister != nil {
						return mRegister.ClickedCh
					}
					// Return a channel that never receives if mRegister is nil
					return make(chan struct{})
				}():
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
							// Registration successful - hide the menu item
							if mRegister != nil {
								mRegister.Hide()
							}
							// Update tooltip to show registered status
							systray.SetTooltip(fmt.Sprintf("%s – Connected as %s", EnvDisplayName, cfg.AgentID))

							// Start Pub/Sub loop with new credentials
							// (The initial StartAgent call started it before registration, so it exited early)
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

	// Remove config directory
	configDir := GetConfigDir()
	if err := os.RemoveAll(configDir); err != nil {
		if !quiet {
			fmt.Println("Warning: Failed to remove config directory:", err)
		}
	} else {
		if !quiet {
			fmt.Println("→ Removed config directory:", configDir)
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
				"All files and settings have been removed.",
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
