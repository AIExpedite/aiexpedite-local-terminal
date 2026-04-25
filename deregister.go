// File: deregister.go
// Notifies the terminal-service that this agent is going offline or coming
// back online. The two HMAC RPCs (POST /device/:id/offline and
// POST /device/:id/online) are intentionally paired here so the auth scheme,
// retry policy, and serializing mutex stay in lockstep — if you change one,
// you almost certainly need to change the other too.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// notifyConnectivityMutex serializes both notifyOffline and notifyOnline so a
// rapid tray toggle (Disconnect → Reconnect) or shutdown racing with a boot-time
// online ping cannot issue overlapping state-flip requests. The backend route
// is "last write wins" by arrival order, so serializing here makes the wire
// order match the user's click order — otherwise a delayed offline could
// overwrite a just-issued online and leave the device stuck grey, which is
// exactly the failure mode this whole feature was added to prevent.
var notifyConnectivityMutex sync.Mutex

// Retry behavior shared by notifyOffline and notifyOnline. Connectivity
// signals are critical — a single transient HTTP failure (TCP reset, brief
// DNS hiccup, server restart) must not silently leave the backend with stale
// state.
const (
	notifyConnectivityMaxAttempts    = 2
	notifyConnectivityRetryBackoff   = 250 * time.Millisecond
	notifyConnectivityPerAttemptHTTP = 2 * time.Second
)

// notifyOffline sends a POST /device/{agentId}/offline request to the
// terminal-service so it stops routing commands to this agent immediately.
//
// Safe to call from multiple places:
//   - User toggles "Disconnect from cloud" in the tray (main.go)
//   - Graceful shutdown (shutdown.go gracefulShutdown)
//   - OS signal handlers (SIGINT/SIGTERM)
//
// The signal is critical to disconnect correctness, so the request is retried
// up to notifyConnectivityMaxAttempts times within the caller's context budget.
// Returns the last error encountered, or nil on success. Callers (especially
// shutdown routines) should await this so they can log success/failure before
// tearing down sub-processes.
func notifyOffline(ctx context.Context, cfg *Config) error {
	return notifyConnectivity(ctx, cfg, "offline")
}

// notifyOnline sends a POST /device/{agentId}/online request to the
// terminal-service so it clears any `offlineSince` set by a previous
// shutdown and refreshes lastSeen — making the device immediately
// eligible for reservation routing again.
//
// Called from:
//   - Tray "Reconnect to cloud" handler in main.go (mirror of the
//     notifyOffline call on the disconnect path)
//   - StartAgent boot in agent.go, when the device is registered AND
//     not in user-toggled offline mode. This is the path that fixes
//     graceful-shutdown stickiness: any clean exit (SIGTERM, console
//     close, tray Quit) called notifyOffline; on next launch the
//     backend still has offlineSince set, but the agent has no
//     persisted reason to be offline. Calling /online here clears
//     the stranded state.
//
// The request is idempotent server-side — calling /online on an
// already-online device returns 200 with alreadyOnline:true and writes
// nothing, so the boot-path call is cheap on the common case.
func notifyOnline(ctx context.Context, cfg *Config) error {
	return notifyConnectivity(ctx, cfg, "online")
}

// notifyConnectivity is the shared retry/auth/HTTP wrapper for
// notifyOffline and notifyOnline. Both endpoints accept the same HMAC
// payload and follow the same retry policy, so we factor the only
// difference (the URL suffix) into the path argument.
//
// path must be either "offline" or "online" — anything else is a
// programming error caught at call time. We don't validate further
// because the value is package-private and only set by the two
// wrapper functions above.
func notifyConnectivity(ctx context.Context, cfg *Config, path string) error {
	if cfg == nil || cfg.AgentID == "" || cfg.CommandSecret == "" {
		return fmt.Errorf("notify%s: missing agent credentials", capitalize(path))
	}
	// Respect caller-provided cancellation early (e.g., the app is quitting
	// while the user also clicked "Disconnect"; no reason to make two calls
	// fight for the HTTP client timeout).
	if ctx != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}

	// Shared mutex serializes offline AND online RPCs so a rapid tray
	// toggle cannot interleave them — see notifyConnectivityMutex docstring.
	notifyConnectivityMutex.Lock()
	defer notifyConnectivityMutex.Unlock()

	baseURL := getRegistrationURL()
	if baseURL == "" {
		return fmt.Errorf("notify%s: registration URL not configured", capitalize(path))
	}

	// If the caller didn't provide a context, fall back to a 5s budget so
	// tray-click callers don't have to remember to wrap.
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}

	url := fmt.Sprintf("%s/device/%s/%s", baseURL, cfg.AgentID, path)

	var lastErr error
	for attempt := 1; attempt <= notifyConnectivityMaxAttempts; attempt++ {
		// Honor caller-cancellation between attempts so shutdown's 5s budget
		// isn't squandered on retries the caller has already given up on.
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("notify%s aborted (%w) after %v", capitalize(path), ctx.Err(), lastErr)
			}
			return ctx.Err()
		default:
		}

		err := sendConnectivityRequest(ctx, url, cfg)
		if err == nil {
			// "shutdown" reads more naturally than "offline" in the offline
			// path's success log; "reconnect" reads better than "online" in
			// the online path's. Pick the user-facing word per route.
			verb := "shutdown"
			if path == "online" {
				verb = "reconnect"
			}
			fmt.Printf("%s[%s] Server notified of %s (attempt %d)%s\n",
				colorGreen, path, verb, attempt, colorReset)
			return nil
		}
		lastErr = err

		if attempt < notifyConnectivityMaxAttempts {
			fmt.Printf("%s[%s] Notify attempt %d/%d failed: %v — retrying%s\n",
				colorYellow, path, attempt, notifyConnectivityMaxAttempts, err, colorReset)
			// Backoff between attempts, but never beyond ctx.Done().
			select {
			case <-ctx.Done():
				return fmt.Errorf("notify%s aborted during backoff (%w) after %v", capitalize(path), ctx.Err(), lastErr)
			case <-time.After(notifyConnectivityRetryBackoff):
			}
		}
	}

	fmt.Printf("%s[%s] Failed to notify server after %d attempts: %v%s\n",
		colorRed, path, notifyConnectivityMaxAttempts, lastErr, colorReset)
	return lastErr
}

// notifyOnlineIfApplicable is the boot-path helper for StartAgent. It
// gates notifyOnline behind the same conditions that gate the Pub/Sub
// loop: the device must be registered (otherwise we have no credentials
// to authenticate the call) and not currently in user-toggled offline
// mode (otherwise we'd be silently overriding the user's explicit
// disconnect). Extracted so the boot-path logic is unit-testable in
// isolation from StartAgent's many side effects (ttyd, tmux, etc.).
//
// Best-effort: errors are logged but never bubble up. Boot must not
// block on a transient backend failure — the next graceful shutdown +
// restart cycle will retry, and a manual tray Reconnect always works.
func notifyOnlineIfApplicable(ctx context.Context, cfg *Config) {
	if cfg == nil || !cfg.IsRegistered() || cfg.OfflineMode {
		return
	}
	if err := notifyOnline(ctx, cfg); err != nil {
		fmt.Printf("%s[online] Boot-time notifyOnline failed: %v%s\n",
			colorYellow, err, colorReset)
	}
}

// capitalize returns the input with the first byte uppercased. Used
// only for error messages, so ASCII-only input ("offline"/"online") is
// the entire concern.
func capitalize(s string) string {
	if len(s) == 0 {
		return s
	}
	if s[0] >= 'a' && s[0] <= 'z' {
		return string(s[0]-32) + s[1:]
	}
	return s
}

// sendConnectivityRequest performs a single HTTP POST attempt of an
// offline-or-online signal. It is intentionally side-effect-free apart
// from the network call so the retry loop in notifyConnectivity can
// call it repeatedly within one mutex hold and ctx budget.
func sendConnectivityRequest(ctx context.Context, url string, cfg *Config) error {
	// Build HMAC-signed payload (same auth scheme as /auth/token and
	// /device/:id/offline — the backend's /online route accepts an
	// identical body shape).
	timestamp := time.Now().UnixMilli()
	signature := generateHMAC(fmt.Sprintf("%s:%d", cfg.AgentID, timestamp), cfg.CommandSecret)

	payload := map[string]interface{}{
		"timestamp": timestamp,
		"signature": signature,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: notifyConnectivityPerAttemptHTTP}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("cancelled: %w", ctx.Err())
		}
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned status %d", resp.StatusCode)
	}
	return nil
}
