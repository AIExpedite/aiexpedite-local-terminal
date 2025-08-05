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

const version = "v0.2.0" // bumped after Pub/Sub support

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
	fmt.Println("ttyd listening on 127.0.0.1:", port)

	/* 3. Start Pub/Sub loop (non‑blocking) -------------------------------- */

	go StartPubSubLoop(cfg)

	/* 4. Optional auto‑update -------------------------------------------- */

	if cfg.AutoUpdate {
		go func() {
			time.Sleep(5 * time.Second) // give the UI a chance to appear first
			if err := checkForUpdate(); err != nil {
				fmt.Println("Update check error:", err)
			}
		}()
	}
}
