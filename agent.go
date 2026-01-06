// File: agent.go
// -----------------------------------------------------------------------------
// Starts the local ttyd server, optional tmux session, *auto‑update* checker
// and the Pub/Sub worker that exchanges terminal commands/results with the
// back‑end service.  Tray helpers live in main.go – this file focuses on the
// background tasks.
// -----------------------------------------------------------------------------

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"
)

const version = "v0.2.1" // bumped after Pub/Sub support

var (
	ttydCmd       *exec.Cmd      // ttyd process (killed on exit)
	shutdownChan  = make(chan struct{})
	updatePath    string
	updatePending bool
)

/*──────────────────────────────  StartAgent  ──────────────────────────────*/

// StartAgent prepares the local environment (tmux + ttyd) and launches the
// Pub/Sub worker.  If cfg.AutoUpdate is true we also run a delayed update check.
func StartAgent(cfg *Config) {
	/* 1. Ensure prerequisites (tmux + ttyd) exist ------------------------- */

	useTmux := true
	if err := ensureTmux(); err != nil {
		if runtime.GOOS == "windows" {
			// Windows users may not have tmux – gracefully fall back
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

	/* 2. Spawn ttyd ------------------------------------------------------- */

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
	fmt.Printf("→ ttyd listening on http://127.0.0.1:%d\n", port)

	/* 3. Start Pub/Sub loop (non‑blocking) -------------------------------- */

	go StartPubSubLoop(cfg)

	/* 4. Display connection instructions ---------------------------------- */

	showConnectionInstructions(cfg, port)

	/* 5. Optional auto‑update -------------------------------------------- */

	if cfg.AutoUpdate {
		go func() {
			time.Sleep(5 * time.Second) // give the UI a chance to appear first
			if err := checkForUpdate(); err != nil {
				fmt.Println("Update check error:", err)
			}
		}()
	}
}

/*──────────────────────────  showConnectionInstructions  ──────────────────────────*/

// showConnectionInstructions displays how to connect to the terminal
func showConnectionInstructions(cfg *Config, port int) {
	fmt.Println("")
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║                    Ready to Connect!                       ║")
	fmt.Println("╠════════════════════════════════════════════════════════════╣")
	fmt.Printf("║  Local Terminal:  http://127.0.0.1:%-24d║\n", port)
	fmt.Println("║                                                            ║")

	if cfg.ProjectID == "" {
		fmt.Println("║  Remote Access:   Not configured                          ║")
		fmt.Println("║                                                            ║")
		fmt.Println("║  To enable remote access from AI Expedite:                ║")
		fmt.Println("║  1. Edit config: %APPDATA%\\AIExpedite\\config.json         ║")
		fmt.Println("║  2. Set \"project_id\" to your GCP project                  ║")
		fmt.Println("║  3. Set GOOGLE_APPLICATION_CREDENTIALS env var            ║")
		fmt.Println("║  4. Restart this application                              ║")
	} else {
		fmt.Println("║  Remote Access:   Enabled (Pub/Sub)                       ║")
		fmt.Printf("║  Project:         %-40s║\n", truncateString(cfg.ProjectID, 40))
	}

	fmt.Println("║                                                            ║")
	fmt.Println("║  Tip: Right-click tray icon → Open Terminal                ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println("")
}

// truncateString truncates a string to maxLen characters with ellipsis
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
