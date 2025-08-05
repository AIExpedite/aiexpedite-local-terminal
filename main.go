package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/getlantern/systray"
)

func main() {
	// Ensure the program auto-starts at login (Windows)
	if runtime.GOOS == "windows" {
		if err := ensureAutoStart(); err != nil {
			fmt.Println("AutoStart setup failed:", err)
		}
	}
	// Load or create configuration
	cfg, err := LoadConfig(ConfigPath())
	if err != nil {
		if os.IsNotExist(err) {
			cfg = DefaultConfig()
			_ = cfg.Save(ConfigPath())
			fmt.Println("Created default config at", ConfigPath())
		} else {
			fmt.Println("Error loading config:", err)
			cfg = DefaultConfig()
		}
	}
	// Launch agent components in background
	go StartAgent(cfg)
	// Run the system tray UI
	systray.Run(onTrayReady(cfg), onTrayExit)
}

// onTrayReady sets up the tray icon and menu actions.
func onTrayReady(cfg *Config) func() {
	return func() {
		systray.SetIcon(iconData) // iconData is embedded per OS
		systray.SetTitle("TrayAgent")
		systray.SetTooltip("TrayAgent – Remote Terminal")
		mOpen := systray.AddMenuItem("Open Terminal", "Open terminal in browser")
		mCheck := systray.AddMenuItem("Check for Updates", "Check for a new version")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Quit", "Exit the agent")
		// Handle menu item clicks in a separate goroutine
		go func() {
			for {
				select {
				case <-mOpen.ClickedCh:
					// Open the web UI in the default browser
					url := fmt.Sprintf("http://127.0.0.1:%d", cfg.LocalTtydPort)
					if cfg.LocalTtydPort == 0 {
						url = "http://127.0.0.1:7681"
					}
					openBrowser(url)
				case <-mCheck.ClickedCh:
					// Manually check for updates
					go func() {
						if err := checkForUpdate(); err != nil {
							fmt.Println("Update check failed:", err)
						}
					}()
				case <-mQuit.ClickedCh:
					systray.Quit()
					return
				}
			}
		}()
	}
}

// onTrayExit is called after the systray exits; it performs cleanup.
func onTrayExit() {
	// Signal background goroutines to stop
	close(shutdownChan)
	// Stop ttyd process
	if ttydCmd != nil && ttydCmd.Process != nil {
		_ = ttydCmd.Process.Kill()
	}
	// Terminate tmux session if running
	_ = exec.Command("tmux", "kill-session", "-t", tmuxSessionName).Run()
	// If an update was downloaded, launch it now
	if updatePending && updatePath != "" {
		fmt.Println("Launching updated version...")
		_ = exec.Command(updatePath).Start()
		// small delay to allow process spawn
		time.Sleep(1 * time.Second)
	}
}

// openBrowser opens the specified URL in the default web browser of the OS.
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
