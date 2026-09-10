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
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
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
var Version = "v1.0.17"

var (
	ttydCmd      *exec.Cmd // ttyd process (killed on exit)
	shutdownChan = make(chan struct{})
	offlineChan  = make(chan bool, 1) // Send true to go offline, false to come online
	isOffline    bool                 // Current offline state
	offlineMutex sync.RWMutex

	// Systray ready state - prevents calling systray functions before initialization
	systrayReady      bool
	systrayReadyMutex sync.RWMutex
)

// NOTE: the update-state accessors (updatePath / updatePending /
// pendingUpdateInfo and their Set/Get/Has/Clear helpers) live in autoupdate.go,
// next to the scheduler and attempt state machine that own them.

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
	if offline {
		// A disconnect that follows a reconnect attempt releases the temporary
		// drain reservation; an explicitly offline agent cannot be routed work.
		completeCloudReconnectDrain()
	}
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
		if err := cfg[0].MutateAndSave(ConfigPath(), func() {
			cfg[0].OfflineMode = offline
		}); err != nil {
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

	// Wire Claude Code's status line to our hook so the CLI Agents tab's
	// rate-limit metrics stay fresh from interactive usage (not just the
	// agent's automation sessions). Best-effort and idempotent — refreshed on
	// every startup so the binary path stays current across auto-updates. When
	// the user opts out, undo any prior install (restore the stashed third-
	// party command, or drop the key) so the toggle actually takes effect.
	hookHome, _ := os.UserHomeDir()
	// Publish both Claude opt-outs to the per-run paths that cannot reach the
	// config: the status-line reconcile that repairs the hook after a Claude
	// Code update, and the bounded utilization probe.
	SetClaudeStatusLineHookDisabled(cfg.DisableClaudeStatusLineHook)
	SetClaudeUsageProbeDisabled(cfg.DisableClaudeUsageProbe)
	if cfg.DisableClaudeStatusLineHook {
		if changed, err := removeClaudeStatusLineHook(hookHome); err != nil {
			fmt.Printf("%s[statusline] Could not remove Claude status-line hook: %v%s\n",
				colorYellow, err, colorReset)
		} else if changed {
			fmt.Printf("%s[statusline] Removed Claude status-line hook (opt-out)%s\n",
				colorGreen, colorReset)
		}
	} else {
		if changed, err := ensureClaudeStatusLineHook(hookHome); err != nil {
			fmt.Printf("%s[statusline] Could not install Claude status-line hook: %v%s\n",
				colorYellow, err, colorReset)
		} else if changed {
			fmt.Printf("%s[statusline] Installed Claude status-line hook for live rate-limit metrics%s\n",
				colorGreen, colorReset)
		}
	}

	/* 1. Ensure prerequisites (tmux + ttyd) exist ------------------------- */

	// The local web terminal (tmux + ttyd) is a convenience; the cloud
	// connection below is the job. Nothing in this section may abort
	// StartAgent: an early return here happens AFTER the previous instance
	// told the backend it was shutting down and BEFORE this one says it is
	// back, so the tray stays up while the device shows Disconnected — the
	// 2026-09-09 prod outage, when a tmux name collision with the dev agent
	// did exactly that for four hours. A missing tmux is the same class of
	// abort on Unix (where it used to be fatal) as it is on Windows, so it
	// degrades to a plain shell on every platform.
	useTmux := true
	if err := ensureTmux(); err != nil {
		useTmux = false
		fmt.Println("Warning:", err, "- running the local terminal without tmux.")
	}

	localTerminal := true
	if err := ensureTtyd(); err != nil {
		localTerminal = false
		fmt.Println("Warning:", err, "- local web terminal disabled; cloud connection continues.")
	}

	// Git is a warning-level dependency (see readiness.go): offer to install it
	// with guided recovery, but never block startup on a decline or failure.
	ensureGit()

	if useTmux {
		if err := startTmuxSession(); err != nil {
			useTmux = false
			fmt.Println("Warning:", err, "- running the local terminal without tmux.")
		}
	}

	/* 2. Spawn ttyd ------------------------------------------------------- */

	var shellCmd []string
	if useTmux {
		shellCmd = []string{"tmux", "attach", "-t", tmuxTarget()}
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

	port := resolvedTtydPort(cfg.LocalTtydPort)
	if localTerminal {
		// ttyd reports a busy port only on its own stderr, after Start() has
		// already succeeded, so probe the bind ourselves and say which port
		// and why — the usual cause is another channel's agent on this
		// machine with `local_ttyd_port` overridden onto the same number.
		if probe, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); err != nil {
			localTerminal = false
			fmt.Printf("Warning: local terminal port %d is already in use (%v) - local web terminal disabled; "+
				"set a different local_ttyd_port in %s. Cloud connection continues.\n", port, err, ConfigPath())
		} else {
			_ = probe.Close()
		}
	}
	if localTerminal {
		args := append([]string{"-p", strconv.Itoa(port), "-i", "127.0.0.1"}, shellCmd...)
		ttydCmd = exec.Command("ttyd", args...)
		hideWindow(ttydCmd)
		if err := ttydCmd.Start(); err != nil {
			ttydCmd = nil
			// The flag is what showConnectionInstructions reads, so clearing
			// only ttydCmd would still advertise a loopback URL nothing is
			// listening on.
			localTerminal = false
			fmt.Println("Warning: cannot start ttyd –", err, "- local web terminal disabled; cloud connection continues.")
		} else {
			fmt.Printf("→ ttyd listening on http://127.0.0.1:%d\n", port)
		}
	}

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

	/* 3b'. Initialize Codex app-server manager (JSON-RPC over stdio) ----- */
	// Drives `codex app-server --listen stdio://` for AI Expedite's Codex IDE
	// integration. Independent of the CLI session manager above because the
	// protocol is fundamentally different (JSON-RPC 2.0 framing vs. one-shot
	// stream-json output).

	globalCodexAppServerManager = NewCodexAppServerManager(cfg)
	go globalCodexAppServerManager.CleanupStale(codexAppServerMaxLifetime)
	fmt.Println("[aiexpedite] Codex app-server manager ready")

	/* 3b''. Initialize Grok ACP manager (JSON-RPC over stdio) ------------ */
	// Drives `grok agent stdio` for AI Expedite's xAI Grok Build CLI ACP
	// integration. Same shape as the Codex app-server manager (JSON-RPC 2.0
	// stdio with `cached_token`-first auth) but distinct because the
	// orchestrator dispatches the two families on different command Types
	// and keeps independent JSON-RPC state machines per provider.

	globalGrokACPManager = NewGrokACPManager(cfg)
	go globalGrokACPManager.CleanupStale(grokACPMaxLifetime)
	fmt.Println("[aiexpedite] Grok ACP manager ready")

	/* 3b'''. Initialize Claude native manager (stream-json over stdio) ---- */
	// Drives `claude --output-format stream-json` for Claude Code native-chat
	// rendering. Same manager shape as codex/grok, but forwards Claude's
	// stream-json frames verbatim as claude_native_* chunks.

	globalClaudeNativeManager = NewClaudeNativeManager(cfg)
	go globalClaudeNativeManager.CleanupStale(claudeNativeMaxLifetime)
	fmt.Println("[aiexpedite] Claude native manager ready")

	/* 3b4. Initialize Antigravity native manager (stream-json + resume) ------ */
	// Drives agy stream-json stdin with exact --conversation <id> resume for
	// Antigravity Chat. Logical sessions persist between one-shot processes;
	// never uses ambiguous --continue.

	globalAntigravityNativeManager = NewAntigravityNativeManager(cfg)
	go globalAntigravityNativeManager.CleanupStale(antigravityNativeMaxAge)
	fmt.Println("[aiexpedite] Antigravity native manager ready")

	/* 3b5. Initialize OpenCode native manager (one-shot run --format json) -- */
	// Drives `opencode run --format json` with exact `--session <id>` resume for
	// OpenCode Chat. Same logical-session model as Antigravity (no resident
	// process between turns), but the JSON events are incremental, so each one
	// is published as its own chunk. Never uses ambiguous --continue.

	globalOpenCodeNativeManager = NewOpenCodeNativeManager()
	go globalOpenCodeNativeManager.CleanupStale(openCodeNativeMaxAge)
	fmt.Println("[aiexpedite] OpenCode native manager ready")

	/* 3b6. Register in-flight work sources for the update-drain accounting -- */
	// ActiveWork() must see every accepted session before an automatic update
	// may install. Register one contributor per manager now that they exist so
	// a drain waits for interactive sessions to finish (drain.go).
	registerDrainWorkSource(func() int {
		if globalSessionManager != nil {
			return globalSessionManager.ActiveSessionCount()
		}
		return 0
	})
	registerDrainWorkSource(func() int {
		if globalCodexAppServerManager != nil {
			return globalCodexAppServerManager.ActiveCount()
		}
		return 0
	})
	registerDrainWorkSource(func() int {
		if globalGrokACPManager != nil {
			return globalGrokACPManager.ActiveCount()
		}
		return 0
	})
	registerDrainWorkSource(func() int {
		if globalClaudeNativeManager != nil {
			return globalClaudeNativeManager.ActiveCount()
		}
		return 0
	})
	registerDrainWorkSource(func() int {
		if globalAntigravityNativeManager != nil {
			return globalAntigravityNativeManager.ActiveCount()
		}
		return 0
	})
	registerDrainWorkSource(func() int {
		if globalOpenCodeNativeManager != nil {
			return globalOpenCodeNativeManager.ActiveCount()
		}
		return 0
	})

	/* 3c. Begin gathering machine info for /auth/token uploads ------------ */
	// Runs in a background goroutine so we don't block startup. The first
	// gather typically completes within a few seconds, comfortably before
	// the first token request needs it. If it hasn't finished yet,
	// auth.go's getOIDCToken sees a nil cache and sends the request without
	// the new fields — terminal-service handles the absence gracefully.
	StartMachineInfoGathering()

	/* 3d. Resolve any interrupted auto-update attempt BEFORE accepting work */
	// If a previous run crashed mid-drain (or restarted after installing), the
	// crash-recovery marker is reconciled here: reopen admission and either
	// report ready on the new version or abandon the attempt and become
	// routable. This must happen before the Pub/Sub loop can accept work so the
	// agent never comes back believing it is still draining. See autoupdate.go.
	updateReconciliationPending := resolveInterruptedAttempt(cfg)
	if updateReconciliationPending {
		go retryInterruptedAttemptReconciliation(cfg)
	}

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
		if !updateReconciliationPending {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				notifyOnlineIfApplicable(ctx, cfg)
			}()
		}
	}

	/* 4b. Start orphan-process scanner (kills detached CLI agents) -------- */

	StartOrphanScanner(cfg)

	/* 5. Display connection instructions ---------------------------------- */

	showConnectionInstructions(cfg, port, localTerminal)

	// Note: Auto-update is now handled in main.go with proactive dialog
}

/*──────────────────────────  showConnectionInstructions  ──────────────────────────*/

// showConnectionInstructions displays how to connect to the terminal.
// localTerminal is false when ttyd was disabled during startup (missing
// binary, busy port, failed spawn) — nothing is listening on port, so
// printing the URL would send the user to a connection-refused page.
func showConnectionInstructions(cfg *Config, port int, localTerminal bool) {
	fmt.Println("")
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║                    Ready to Connect!                       ║")
	fmt.Println("╠════════════════════════════════════════════════════════════╣")
	if localTerminal {
		fmt.Printf("║  Local Terminal:  http://127.0.0.1:%-24d║\n", port)
	} else {
		fmt.Println("║  Local Terminal:  Unavailable (see warnings above)         ║")
	}
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
	if localTerminal {
		fmt.Println("║  Tip: Right-click tray icon → Open Terminal                ║")
	}
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
