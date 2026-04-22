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

// notifyOffline sends a POST /device/{agentId}/offline request to the
// terminal-service so it stops routing commands to this agent immediately.
//
// Safe to call from multiple places:
//   - User toggles "Disconnect from cloud" in the tray (main.go)
//   - Graceful shutdown (main.go onTrayExit)
//
// Best-effort: errors are logged, never fatal. A ctx.Done() firing before the
// HTTP round-trip completes aborts the request cleanly without leaking a
// goroutine. Idempotent on the server side — the backend just keeps
// offlineSince pinned to the latest timestamp.
func notifyOffline(ctx context.Context, cfg *Config) {
	if cfg == nil || cfg.AgentID == "" || cfg.CommandSecret == "" {
		return
	}
	// Respect caller-provided cancellation early (e.g., the app is quitting
	// while the user also clicked "Disconnect"; no reason to make two calls
	// fight for the 5s HTTP client timeout).
	if ctx != nil {
		select {
		case <-ctx.Done():
			return
		default:
		}
	}

	notifyOfflineMutex.Lock()
	defer notifyOfflineMutex.Unlock()

	baseURL := getRegistrationURL()
	if baseURL == "" {
		return
	}

	url := fmt.Sprintf("%s/device/%s/offline", baseURL, cfg.AgentID)

	// Build HMAC-signed payload (same auth scheme as /auth/token)
	timestamp := time.Now().UnixMilli()
	signature := generateHMAC(fmt.Sprintf("%s:%d", cfg.AgentID, timestamp), cfg.CommandSecret)

	payload := map[string]interface{}{
		"timestamp": timestamp,
		"signature": signature,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		fmt.Printf("%s[offline] Failed to marshal request: %v%s\n", colorRed, err, colorReset)
		return
	}

	// If the caller didn't provide a context, fall back to a 5s timeout so
	// tray-click callers don't have to remember to wrap.
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		fmt.Printf("%s[offline] Failed to create request: %v%s\n", colorRed, err, colorReset)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// Cancelled contexts look like "context canceled" errors — downgrade
		// those to a single log line so graceful shutdown doesn't look alarming.
		if ctx.Err() != nil {
			fmt.Printf("%s[offline] notify cancelled: %v%s\n", colorYellow, ctx.Err(), colorReset)
			return
		}
		fmt.Printf("%s[offline] Failed to notify server: %v%s\n", colorYellow, err, colorReset)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		fmt.Printf("%s[offline] Server notified of shutdown%s\n", colorGreen, colorReset)
	} else {
		fmt.Printf("%s[offline] Server returned status %d%s\n", colorYellow, resp.StatusCode, colorReset)
	}
}
