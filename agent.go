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
	"sync"
)

// Version is the current terminal app version (exported for use in registration and results)
const Version = "v0.6.23"

var (
	ttydCmd       *exec.Cmd // ttyd process (killed on exit)
	shutdownChan  = make(chan struct{})
	offlineChan   = make(chan bool, 1) // Send true to go offline, false to come online
	isOffline     bool                 // Current offline state
	offlineMutex  sync.RWMutex
	updatePath    string
	updatePending bool
	updateMutex   sync.RWMutex

	// Pending update state (when user clicks "Later")
	pendingUpdateInfo  *UpdateInfo
	pendingUpdateMutex sync.RWMutex

	// Systray ready state - prevents calling systray functions before initialization
	systrayReady      bool
	systrayReadyMutex sync.RWMutex
)

// SetSystrayReady marks the systray as initialized and safe to use
func SetSystrayReady() {
	systrayReadyMutex.Lock()
	systrayReady = true
	systrayReadyMutex.Unlock()
}

// IsSystrayReady returns true if systray is initialized
func IsSystrayReady() bool {
	systrayReadyMutex.RLock()
	defer systrayReadyMutex.RUnlock()
	return systrayReady
}

// SetOffline enables or disables offline mode, signaling the Pub/Sub loop
func SetOffline(offline bool) {
	offlineMutex.Lock()
	isOffline = offline
	offlineMutex.Unlock()

	// Non-blocking send to signal the change.
	// Drain any stale value first, then attempt to send — all non-blocking so
	// concurrent callers cannot deadlock on the capacity-1 channel.
	select {
	case <-offlineChan:
	default:
	}
	select {
	case offlineChan <- offline:
	default:
	}
}

// IsOffline returns the current offline state
func IsOffline() bool {
	offlineMutex.RLock()
	defer offlineMutex.RUnlock()
	return isOffline
}

// SetPendingUpdate stores update info for later installation (user clicked "Later")
func SetPendingUpdate(info *UpdateInfo) {
	pendingUpdateMutex.Lock()
	pendingUpdateInfo = info
	pendingUpdateMutex.Unlock()
}

// GetPendingUpdate returns pending update info or nil if none
func GetPendingUpdate() *UpdateInfo {
	pendingUpdateMutex.RLock()
	defer pendingUpdateMutex.RUnlock()
	return pendingUpdateInfo
}

// HasPendingUpdate returns true if an update is waiting to be installed
func HasPendingUpdate() bool {
	return GetPendingUpdate() != nil
}

// ClearPendingUpdate removes the pending update info
func ClearPendingUpdate() {
	pendingUpdateMutex.Lock()
	pendingUpdateInfo = nil
	pendingUpdateMutex.Unlock()
}

// SetUpdateReady stores the path of the downloaded update binary and marks the
// update as pending.  Called from the auto-update goroutine before systray.Quit().
func SetUpdateReady(path string) {
	updateMutex.Lock()
	updatePath = path
	updatePending = true
	updateMutex.Unlock()
}

// GetUpdateReady returns the pending update path and whether an update is ready.
// Safe to call from any goroutine.
func GetUpdateReady() (path string, pending bool) {
	updateMutex.RLock()
	defer updateMutex.RUnlock()
	return updatePath, updatePending
}

/*──────────────────────────────  StartAgent  ──────────────────────────────*/

// StartAgent prepares the local environment (tmux + ttyd) and launches the
// Pub/Sub worker.  If cfg.AutoUpdate is true we also run a delayed update check.
func StartAgent(cfg *Config) {
	/* 0. Initialize storage config for WIF authentication ----------------- */
	SetStorageConfig(cfg)

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
	hideWindow(ttydCmd)
	if err := ttydCmd.Start(); err != nil {
		fmt.Println("Fatal: cannot start ttyd –", err)
		return
	}
	fmt.Printf("→ ttyd listening on http://127.0.0.1:%d\n", port)

	/* 3. Pre-warm persistent PowerShell (Windows only) -------------------- */

	if runtime.GOOS == "windows" {
		go func() {
			if _, err := GetPowerShell(); err != nil {
				fmt.Printf("[aiexpedite] Failed to pre-warm PowerShell: %v\n", err)
			} else {
				fmt.Println("[aiexpedite] PowerShell ready")
			}
		}()
	}

	/* 3b. Initialize Session Manager for interactive CLI agents ----------- */

	globalSessionManager = NewSessionManager(cfg)
	go globalSessionManager.CleanupStale(sessionMaxLifetime)
	fmt.Println("[aiexpedite] Session manager ready")

	/* 4. Start Pub/Sub loop (non‑blocking) -------------------------------- */

	go StartPubSubLoop(cfg)

	/* 5. Display connection instructions ---------------------------------- */

	showConnectionInstructions(cfg, port)

	// Note: Auto-update is now handled in main.go with proactive dialog
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
