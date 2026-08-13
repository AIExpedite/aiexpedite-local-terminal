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
//
// notifyConnectivityHTTPBudget is the deadline the HTTP work gets AFTER the
// mutex is acquired — see notifyConnectivity for the rationale. It must be
// at least (notifyConnectivityMaxAttempts × notifyConnectivityPerAttemptHTTP)
// + (notifyConnectivityMaxAttempts-1) × notifyConnectivityRetryBackoff for
// the retry wrapper to actually finish all attempts.
const (
	notifyConnectivityMaxAttempts    = 2
	notifyConnectivityRetryBackoff   = 250 * time.Millisecond
	notifyConnectivityPerAttemptHTTP = 2 * time.Second
	notifyConnectivityHTTPBudget     = 5 * time.Second
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
	return notifyConnectivity(ctx, cfg, "offline", nil)
}

// notifyDrainEnter tells the terminal-service this agent has entered the
// update-draining state for a specific attempt/target version, so the cloud
// stops routing NEW work to it while it finishes accepted work. Idempotent
// server-side: repeated calls for the same attempt refresh the drain lease
// rather than re-entering.
func notifyDrainEnter(ctx context.Context, cfg *Config, attemptID, targetVersion string) error {
	return notifyConnectivity(ctx, cfg, "drain", map[string]any{
		"attemptId":     attemptID,
		"targetVersion": targetVersion,
	})
}

// notifyDrainConfirm re-confirms an active drain (the renewable lease). The
// service's clock is authoritative — a drain that has not been confirmed for
// the staleness window expires — so the agent calls this on a short interval
// while draining. Same endpoint as enter; the service distinguishes by attempt.
func notifyDrainConfirm(ctx context.Context, cfg *Config, attemptID, targetVersion string) error {
	return notifyConnectivity(ctx, cfg, "drain", map[string]any{
		"attemptId":     attemptID,
		"targetVersion": targetVersion,
	})
}

// notifyDrainExit clears the drain for an attempt. reason is one of
// "finished" / "deferred" / "superseded" / "preference_off". Clearing drain
// does NOT by itself restore routability server-side beyond removing the
// draining marker — the agent becomes routable again via its own online/ready
// signal (or, after a restart, notifyOnlineWithVersion).
func notifyDrainExit(ctx context.Context, cfg *Config, attemptID, reason string) error {
	return notifyConnectivity(ctx, cfg, "drain/exit", map[string]any{
		"attemptId": attemptID,
		"reason":    reason,
	})
}

// notifyOnlineWithVersion is notifyOnline plus the running version and the
// attempt id that caused the restart, so the service can reconcile the drain it
// cleared with the version actually running (and record a mismatch if the
// update did not land).
func notifyOnlineWithVersion(ctx context.Context, cfg *Config, version, attemptID string) error {
	return notifyConnectivity(ctx, cfg, "online", map[string]any{
		"version":   version,
		"attemptId": attemptID,
	})
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
	return notifyConnectivity(ctx, cfg, "online", nil)
}

// notifyConnectivity is the shared retry/auth/HTTP wrapper for
// notifyOffline and notifyOnline. Both endpoints accept the same HMAC
// payload and follow the same retry policy, so we factor the only
// difference (the URL suffix) into the path argument.
//
// path is the URL suffix under /device/{agentId}/ — "offline", "online",
// "drain", or "drain/exit". extra carries additional (unsigned) body fields
// for the drain/version-aware calls; the HMAC signature always covers only
// "agentId:timestamp", matching the server's verifier, so extra fields ride
// alongside without changing the auth scheme. The value is package-private and
// only set by the wrapper functions above.
func notifyConnectivity(ctx context.Context, cfg *Config, path string, extra map[string]any) error {
	if cfg == nil || cfg.AgentID == "" || cfg.CommandSecret == "" {
		return fmt.Errorf("notify%s: missing agent credentials", capitalize(path))
	}
	// Respect caller-provided cancellation early so a doomed call does not
	// even queue up on the mutex behind a slow predecessor — but don't
	// honor the caller's deadline past this point. See post-lock budget
	// reset below for why.
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

	// Fresh deadline budget for the HTTP work after lock acquisition.
	//
	// The mutex's purpose is wire-order serialization, not a shared
	// queue deadline. If we kept the caller's ctx for the HTTP work
	// after the mutex, a rapid Disconnect→Reconnect where the first
	// call burns most of its 5s budget on retries-while-holding-mutex
	// would leave the second caller almost no budget for its own
	// attempts, and the user's later intent would be silently lost
	// (backend ends up stuck on the earlier transition). The pre-lock
	// cancellation check above already gated entry; once we hold the
	// mutex, the HTTP work gets a clean budget.
	//
	// We deliberately do NOT propagate the caller ctx's cancellation
	// past this line. All current callers pass plain timeout contexts
	// with no explicit cancel source (tray click handlers, boot path,
	// signal handler — each constructs `context.WithTimeout(
	// context.Background(), 5s)`), so the parent ctx's only signal is
	// its deadline, which is exactly what we want to ignore here. If
	// a future caller needs to propagate explicit cancellation through
	// the mutex (e.g. a "stop everything" abort), this is the place to
	// thread it via context.AfterFunc.
	ctx, cancel := context.WithTimeout(context.Background(), notifyConnectivityHTTPBudget)
	defer cancel()

	url := fmt.Sprintf("%s/device/%s/%s", baseURL, cfg.AgentID, path)

	var lastErr error
	for attempt := 1; attempt <= notifyConnectivityMaxAttempts; attempt++ {
		// Honor the post-lock budget between attempts so we don't loop
		// past the 5s window on a fully-broken network.
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("notify%s aborted (%w) after %v", capitalize(path), ctx.Err(), lastErr)
			}
			return ctx.Err()
		default:
		}

		err := sendConnectivityRequest(ctx, url, cfg, extra)
		if err == nil {
			// Pick a user-facing verb per route for the success log.
			verb := "shutdown"
			switch {
			case path == "online":
				verb = "reconnect"
			case path == "drain":
				verb = "drain"
			case path == "drain/exit":
				verb = "drain-exit"
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
func sendConnectivityRequest(ctx context.Context, url string, cfg *Config, extra map[string]any) error {
	// Build HMAC-signed payload (same auth scheme as /auth/token and
	// /device/:id/offline). The signature covers only "agentId:timestamp";
	// any `extra` fields (drain attemptId/targetVersion/reason, online
	// version/attemptId) ride alongside unsigned, exactly as the server's
	// verifier expects.
	timestamp := time.Now().UnixMilli()
	signature := generateHMAC(fmt.Sprintf("%s:%d", cfg.AgentID, timestamp), cfg.CommandSecret)

	payload := map[string]interface{}{
		"timestamp": timestamp,
		"signature": signature,
	}
	for k, v := range extra {
		// Never let an extra field clobber the signed timestamp/signature.
		if k == "timestamp" || k == "signature" {
			continue
		}
		payload[k] = v
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
