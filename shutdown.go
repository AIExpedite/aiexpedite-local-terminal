// File: shutdown.go
// Centralized graceful-shutdown orchestration for the local terminal agent.
//
// gracefulShutdown is invoked from several entry points:
//   - tray "Quit" handler (onTrayExit in main.go)
//   - OS signal handlers for SIGINT / SIGTERM (Unix, Windows)
//   - Windows console close handler (consoleCtrlHandler in tray_windows.go)
//
// The teardown order is intentional and must be preserved:
//  1. SetOffline(true)        — flips the local IsOffline flag and signals
//     the Pub/Sub loop to suspend, so any in-flight
//     __ping__ messages are dropped instead of
//     producing a stale pong.
//  2. notifyOffline(ctx, cfg) — synchronously notifies the backend so
//     offlineSince is persisted and lastSeen reset.
//     Retries within the supplied context budget.
//  3. Sub-process teardown    — only after the backend is informed do we
//     kill ttyd, tmux, and the persistent
//     PowerShell helper. This avoids a race where
//     killing children stops the goroutine before
//     the HTTP round-trip completes.
package main

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"sync/atomic"
	"time"
)

// shutdownInProgress guards against re-entrant invocations of
// gracefulShutdown. The OS signal handler, Windows console close handler,
// and tray "Quit" path can all fire in quick succession (or even
// simultaneously, e.g. user clicks Quit and the system also sends
// CTRL_CLOSE_EVENT); a second pass through teardown would race the first.
var shutdownInProgress atomic.Bool

// gracefulShutdownBudget is the total time we're willing to spend telling
// the backend we've gone offline. 5 seconds is generous enough for the
// retry wrapper inside notifyOffline (2 attempts × 2s HTTP timeout +
// 250ms backoff) to complete on a normal connection while still keeping
// app exit responsive on a fully-broken network.
const gracefulShutdownBudget = 5 * time.Second

// gracefulShutdown runs the canonical disconnect sequence. It is safe to
// call from multiple places concurrently — the first call wins and
// subsequent calls return immediately.
//
// The provided ctx is the caller's overall deadline (e.g. systray.Quit
// might give us no time at all, in which case we still try with a default
// budget). When ctx is nil we use a fresh background context with the
// gracefulShutdownBudget deadline.
func gracefulShutdown(ctx context.Context, cfg *Config) {
	if !shutdownInProgress.CompareAndSwap(false, true) {
		fmt.Println("[shutdown] gracefulShutdown already in progress, skipping duplicate call")
		return
	}

	fmt.Println("[shutdown] Starting graceful shutdown sequence")

	// Step 1: Flip local offline state and tell the Pub/Sub loop to stop.
	// Do not persist OfflineMode here — process exit shouldn't permanently
	// mark the user as disconnected; persistence is reserved for explicit
	// tray toggles. SetOffline(true) without a config arg only flips the
	// in-memory flag and signals offlineChan.
	SetOffline(true)

	// Give the receive loop a brief window to actually drain. The signal
	// goroutine inside runPubSubConnection will cancel the subscription
	// context and return; we don't strictly need to wait, but a tiny pause
	// reduces the chance of a pong being mid-flight when notifyOffline
	// races with a ping that arrived microseconds earlier.
	time.Sleep(50 * time.Millisecond)

	// Step 2: Notify the backend synchronously. We want this to complete
	// before we kill the sub-processes that the connection relies on.
	if cfg != nil && cfg.AgentID != "" && cfg.CommandSecret != "" {
		shutdownCtx := ctx
		var cancel context.CancelFunc
		if shutdownCtx == nil {
			shutdownCtx, cancel = context.WithTimeout(context.Background(), gracefulShutdownBudget)
		} else {
			// Always layer our budget on top so a pre-cancelled parent
			// doesn't leave us with literally no time to talk to the API.
			shutdownCtx, cancel = context.WithTimeout(shutdownCtx, gracefulShutdownBudget)
		}
		defer cancel()

		if err := notifyOffline(shutdownCtx, cfg); err != nil {
			fmt.Printf("%s[shutdown] notifyOffline failed: %v%s\n", colorYellow, err, colorReset)
			// Fall through — best-effort. The backend's heartbeat staleness
			// check (5min lastSeen window) is the safety net.
		} else {
			fmt.Println("[shutdown] Backend notified, proceeding to teardown")
		}
	} else {
		fmt.Println("[shutdown] No registration credentials, skipping backend notify")
	}

	// Step 3: Tear down sub-processes. Errors here are best-effort — the
	// app is exiting anyway and the OS will reap anything we miss.
	tearDownSubprocesses()

	fmt.Println("[shutdown] Graceful shutdown sequence complete")
}

// tearDownSubprocesses kills the children spawned by the agent: ttyd,
// the tmux session, and (on Windows) the persistent PowerShell helper.
// Idempotent and safe to call multiple times.
func tearDownSubprocesses() {
	if runtime.GOOS == "windows" {
		// Cleanup persistent PowerShell process.
		ShutdownPowerShell()
	}

	// Gracefully end any in-flight Codex app-server sessions before we tear
	// down outbound network so codex children don't outlive the agent and
	// silently consume tokens. Best-effort: each End() call has its own
	// timeout cascade (stdin close → SIGINT → SIGKILL).
	if globalCodexAppServerManager != nil {
		globalCodexAppServerManager.ShutdownAll()
	}

	// Same rationale for the Grok ACP manager — children hold a Grok auth
	// session and may keep billing if the agent dies without ending them.
	if globalGrokACPManager != nil {
		globalGrokACPManager.ShutdownAll()
	}

	// Cleanup GCS storage client (closes idle HTTP/2 streams cleanly).
	CloseStorageClient()

	// Stop ttyd cleanly so the local web terminal port is released.
	if ttydCmd != nil && ttydCmd.Process != nil {
		_ = ttydCmd.Process.Kill()
		_, _ = ttydCmd.Process.Wait() // avoid zombie
	}

	// Kill the tmux session (ignore error if it never existed).
	_ = exec.Command("tmux", "kill-session", "-t", tmuxSessionName).Run()
}

// IsShutdownInProgress is exposed for tests and for the receive loop's
// post-cancel return path so it can distinguish "user-initiated shutdown"
// from "subscription failure" without re-checking IsOffline.
func IsShutdownInProgress() bool {
	return shutdownInProgress.Load()
}
