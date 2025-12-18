package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	fmt.Println("║          AI Expedite Terminal - Starting...                ║")
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
		systray.SetTitle("AI Expedite Terminal")
		systray.SetTooltip("AI Expedite Terminal – Remote Command Execution")

		mOpen := systray.AddMenuItem("Open Terminal", "Open terminal in browser")
		mCheck := systray.AddMenuItem("Check for Updates", "Check for a new version")
		systray.AddSeparator()
		mConsole := systray.AddMenuItemCheckbox("Show Console", "Toggle console window visibility", false)
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Quit", "Exit the agent")

		go func() {
			consoleVisible := false
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
		fmt.Println("║          AI Expedite Terminal - Uninstalling...            ║")
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

	if !quiet {
		fmt.Println("")
		fmt.Println("╔════════════════════════════════════════════════════════════╗")
		fmt.Println("║          Uninstall complete!                               ║")
		fmt.Println("║                                                            ║")
		fmt.Println("║  You can now delete the executable file manually.          ║")
		fmt.Println("╚════════════════════════════════════════════════════════════╝")
		fmt.Println("")

		// Show dialog to user
		exePath, _ := os.Executable()
		exeDir := filepath.Dir(exePath)
		ShowInfoDialog(
			"Uninstall Complete",
			"AI Expedite Terminal has been uninstalled.\n\n"+
				"Registry entries and configuration have been removed.\n\n"+
				"You can now delete the executable from:\n"+exeDir,
		)
	}
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
