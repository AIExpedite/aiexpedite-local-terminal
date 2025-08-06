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
	// Ensure auto‑start at login (Windows)
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

	// Background workers
	go StartAgent(cfg)

	// Tray UI
	systray.Run(onTrayReady(cfg), onTrayExit)
}

/* ---------- tray helpers ---------- */

func onTrayReady(cfg *Config) func() {
	return func() {
		systray.SetIcon(iconData)
		systray.SetTitle("TrayAgent")
		systray.SetTooltip("TrayAgent – Remote Terminal")

		mOpen := systray.AddMenuItem("Open Terminal", "Open terminal in browser")
		mCheck := systray.AddMenuItem("Check for Updates", "Check for a new version")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Quit", "Exit the agent")

		go func() {
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

				case <-mQuit.ClickedCh:
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
		_, _ = ttydCmd.Process.Wait() // ← ensures no zombie
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

/* openBrowser unchanged – omitted for brevity */
