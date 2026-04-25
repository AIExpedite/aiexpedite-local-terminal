// File: deregister.go
// Notifies the terminal-service that this agent is going offline.
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

// notifyOfflineMutex serializes concurrent calls so a tray toggle racing with
// app shutdown (or double-clicks on the disconnect item) cannot issue
// overlapping requests. Each call is still cheap and short-lived — this just
// keeps the server log clean and avoids duplicate writes to offlineSince.
var notifyOfflineMutex sync.Mutex

// notifyOfflineMaxAttempts and notifyOfflineRetryBackoff control the retry
// behavior of notifyOffline. The offline signal is critical to the disconnect
// flow — a single transient HTTP failure (TCP reset, brief DNS hiccup, server
// restart) must not silently leave the backend with an online device.
const (
	notifyOfflineMaxAttempts   = 2
	notifyOfflineRetryBackoff  = 250 * time.Millisecond
	notifyOfflinePerAttemptHTTP = 2 * time.Second
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
// up to notifyOfflineMaxAttempts times within the caller's context budget.
// Returns the last error encountered, or nil on success. Callers (especially
// shutdown routines) should await this so they can log success/failure before
// tearing down sub-processes.
func notifyOffline(ctx context.Context, cfg *Config) error {
	if cfg == nil || cfg.AgentID == "" || cfg.CommandSecret == "" {
		return fmt.Errorf("notifyOffline: missing agent credentials")
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

	notifyOfflineMutex.Lock()
	defer notifyOfflineMutex.Unlock()

	baseURL := getRegistrationURL()
	if baseURL == "" {
		return fmt.Errorf("notifyOffline: registration URL not configured")
	}

	// If the caller didn't provide a context, fall back to a 5s budget so
	// tray-click callers don't have to remember to wrap.
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}

	url := fmt.Sprintf("%s/device/%s/offline", baseURL, cfg.AgentID)

	var lastErr error
	for attempt := 1; attempt <= notifyOfflineMaxAttempts; attempt++ {
		// Honor caller-cancellation between attempts so shutdown's 5s budget
		// isn't squandered on retries the caller has already given up on.
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("notifyOffline aborted (%w) after %v", ctx.Err(), lastErr)
			}
			return ctx.Err()
		default:
		}

		err := sendOfflineRequest(ctx, url, cfg)
		if err == nil {
			fmt.Printf("%s[offline] Server notified of shutdown (attempt %d)%s\n",
				colorGreen, attempt, colorReset)
			return nil
		}
		lastErr = err

		if attempt < notifyOfflineMaxAttempts {
			fmt.Printf("%s[offline] Notify attempt %d/%d failed: %v — retrying%s\n",
				colorYellow, attempt, notifyOfflineMaxAttempts, err, colorReset)
			// Backoff between attempts, but never beyond ctx.Done().
			select {
			case <-ctx.Done():
				return fmt.Errorf("notifyOffline aborted during backoff (%w) after %v", ctx.Err(), lastErr)
			case <-time.After(notifyOfflineRetryBackoff):
			}
		}
	}

	fmt.Printf("%s[offline] Failed to notify server after %d attempts: %v%s\n",
		colorRed, notifyOfflineMaxAttempts, lastErr, colorReset)
	return lastErr
}

// sendOfflineRequest performs a single HTTP POST attempt of the offline
// signal. It is intentionally side-effect-free apart from the network call so
// the retry loop in notifyOffline can call it repeatedly within one mutex
// hold and ctx budget.
func sendOfflineRequest(ctx context.Context, url string, cfg *Config) error {
	// Build HMAC-signed payload (same auth scheme as /auth/token)
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

	client := &http.Client{Timeout: notifyOfflinePerAttemptHTTP}
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
