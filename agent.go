package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/getlantern/systray"
)

const version = "v0.2.0" // bumped after Pub/Sub support

var (
	ttydCmd      *exec.Cmd   // ttyd process (killed on exit)
	shutdownChan = make(chan struct{})
	updatePath   string
	updatePending bool
)

/* ────────────────────────────── Launcher ───────────────────────────────── */

func StartAgent(cfg *Config) {
	/* 1. Ensure ttyd + (optional) tmux are present */
	useTmux := true
	if err := ensureTmux(); err != nil {
		if runtime.GOOS == "windows" {
			useTmux = false
			fmt.Println("Warning:", err, "- running without tmux.")
		} else {
			fmt.Println("Fatal:", err)
			return
		}
	}
	if err := ensureTtyd(); err != nil {
		fmt.Println("Fatal:", err)
		return
	}
	if useTmux {
		if err := startTmuxSession(); err != nil {
			fmt.Println("Fatal:", err)
			return
		}
	}

	/* 2. Spawn ttyd */
	var shellCmd []string
	if useTmux {
		shellCmd = []string{"tmux", "attach", "-t", tmuxSessionName}
	} else {
		sh := os.Getenv("SHELL")
		if sh == "" {
			if runtime.GOOS == "windows" {
				sh = "powershell"
			} else {
				sh = "/bin/bash"
			}
		}
		shellCmd = []string{sh}
	}
	port := cfg.LocalTtydPort
	if port == 0 {
		port = 7681
	}
	args := append([]string{"-p", fmt.Sprintf("%d", port), "-i", "127.0.0.1"}, shellCmd...)
	ttydCmd = exec.Command("ttyd", args...)
	if err := ttydCmd.Start(); err != nil {
		fmt.Println("Fatal: cannot start ttyd –", err)
		return
	}
	fmt.Println("ttyd listening on 127.0.0.1:", port)

	/* 3. Start Pub/Sub background loop (non‑blocking) */
	go StartPubSubLoop(cfg)
}

/* ────────────────────── System‑tray bootstrap (unchanged) ──────────────── */

func main() {
	if runtime.GOOS == "windows" {
		_ = ensureAutoStart()
	}

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
	if err := cfg.Validate(); err != nil {
		fmt.Println("Config error:", err)
	}

	go StartAgent(cfg)

	systray.Run(onTrayReady(cfg), onTrayExit)
}

/* ---------------- tray helpers & shutdown (identical to previous) -------- */

func onTrayReady(cfg *Config) func() {
	return func() {
		systray.SetIcon(iconData)
		systray.SetTitle("AI Expedite Terminal")
		systray.SetTooltip("Remote Terminal Agent")

		mOpen := systray.AddMenuItem("Open Terminal", "Open terminal in browser")
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
				case <-mQuit.ClickedCh:
					systray.Quit()
					return
				}
			}
		}()
	}
}

func onTrayExit() {
	close(shutdownChan)
	if ttydCmd != nil && ttydCmd.Process != nil {
		_ = ttydCmd.Process.Kill()
	}
	_ = exec.Command("tmux", "kill-session", "-t", tmuxSessionName).Run()
	if updatePending && updatePath != "" {
		_ = exec.Command(updatePath).Start()
		time.Sleep(1 * time.Second)
	}
}

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
	default:
		cmd = "xdg-open"
		args = []string{url}
	}
	_ = exec.Command(cmd, args...).Start()
}
