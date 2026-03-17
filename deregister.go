// File: deregister.go
// Notifies the terminal-service that this agent is going offline.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// notifyOffline sends a POST /device/{agentId}/offline request to the
// terminal-service so it stops routing commands to this agent immediately.
// Called during graceful shutdown; best-effort — errors are logged but not fatal.
func notifyOffline(ctx context.Context, cfg *Config) {
	if cfg.AgentID == "" || cfg.CommandSecret == "" {
		return
	}

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

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		fmt.Printf("%s[offline] Failed to create request: %v%s\n", colorRed, err, colorReset)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
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
