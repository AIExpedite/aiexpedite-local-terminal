// File: agent.go
// -----------------------------------------------------------------------------
// Starts the local ttyd server, optional tmux session, *auto‑update* checker
// and the Pub/Sub worker that exchanges terminal commands/results with the
// back‑end service.  Tray helpers live in main.go – this file focuses on the
// background tasks.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"
)

// Version is the current terminal app version (exported for use in
// registration, results, --version output, and auto-update comparison).
//
// Declared as `var` (not `const`) so the prod release workflow can override
// it via:
//
//	go build -ldflags "-X main.Version=v0.8.5" .
//
// The Go linker can only patch package-level vars, never consts (consts are
// inlined at compile time). The default value here is what nonprod builds
// ship with; bump it before pushing to main when you want nonprod's
// `--version` and the auto-update comparison to reflect the new release.
var Version = "v0.9.9"

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

// SetOffline enables or disables offline mode, signaling the Pub/Sub loop.
// When a *Config is provided, the new OfflineMode is persisted so the toggle
// survives app restarts. Passing nil is supported for callers that cannot
// reach the live config (tests).
//
// IMPORTANT: This function only toggles local state and signals the Pub/Sub
// loop to suspend listening. It does NOT notify the backend service that this
// agent has gone offline — the caller is responsible for invoking
// notifyOffline(ctx, cfg) to make the disconnect visible server-side.
//
// The split is intentional: tray toggles, OS signal handlers, and the
// gracefulShutdown orchestration each call SetOffline first to immediately
// halt outbound Pub/Sub traffic, then await notifyOffline so the backend can
// persist offlineSince before sub-processes are torn down.
func SetOffline(offline bool, cfg ...*Config) {
	offlineMutex.Lock()
	isOffline = offline
	offlineMutex.Unlock()

	// Log transitions so operators can correlate disconnect events in the
	// console with backend offlineSince writes.
	if offline {
		fmt.Printf("%s[offline] Local offline state ENABLED — Pub/Sub will be suspended%s\n", colorYellow, colorReset)
	} else {
		fmt.Printf("%s[offline] Local offline state DISABLED — Pub/Sub will resume%s\n", colorGreen, colorReset)
	}

	// Persist across restarts so the user's explicit disconnect choice
	// survives a reboot / auto-update / crash recovery. Nil-safe: callers
	// from tests or signal handlers may not have a live config reference.
	if len(cfg) > 0 && cfg[0] != nil {
		cfg[0].OfflineMode = offline
		if err := cfg[0].Save(ConfigPath()); err != nil {
			fmt.Printf("%s[offline] Failed to save offline state: %v%s\n", colorYellow, err, colorReset)
		}
	}

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

	// Restore persisted offline state so the cloud connection respects the
	// user's last "Disconnect from cloud" toggle across restarts.
	offlineMutex.Lock()
	isOffline = cfg.OfflineMode
	offlineMutex.Unlock()

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

	/* 3c. Begin gathering machine info for /auth/token uploads ------------ */
	// Runs in a background goroutine so we don't block startup. The first
	// gather typically completes within a few seconds, comfortably before
	// the first token request needs it. If it hasn't finished yet,
	// auth.go's getOIDCToken sees a nil cache and sends the request without
	// the new fields — terminal-service handles the absence gracefully.
	StartMachineInfoGathering()

	/* 4. Start Pub/Sub loop (non‑blocking) -------------------------------- */

	// If the user disconnected from cloud in a previous session, skip the
	// Pub/Sub loop entirely on boot. The tray "Reconnect to cloud" handler
	// will start it explicitly when the user comes back online.
	if cfg.OfflineMode {
		fmt.Println("[agent] Offline mode persisted — skipping StartPubSubLoop until user reconnects")
	} else {
		go StartPubSubLoop(cfg)

		// Tell the backend we're online so any `offlineSince` left over
		// from a prior graceful shutdown (SIGTERM / console close / tray
		// Quit / OS signal) is cleared. Without this the agent pings
		// forever into a `Stale pong ignored` log on the server while the
		// frontend shows the device grey, until the user manually clicks
		// the tray Reconnect or someone clears Firestore by hand. The
		// helper guards on cfg.IsRegistered() so we don't fire before the
		// registration flow has populated AgentID/CommandSecret.
		//
		// Best-effort, non-blocking: a transient backend failure must not
		// delay agent startup. The HTTP retry wrapper inside notifyOnline
		// gives us 2 attempts × 2s within a 5s budget.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			notifyOnlineIfApplicable(ctx, cfg)
		}()
	}

	/* 4b. Start orphan-process scanner (kills detached CLI agents) -------- */

	StartOrphanScanner(cfg)

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
		fmt.Printf("║  1. Edit config: %-42s║\n", truncateString(ConfigPath(), 42))
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
