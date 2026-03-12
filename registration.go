// File: registration.go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"time"
)

const (
	// Polling configuration
	pollInterval = 2 * time.Second
	pollTimeout  = 15 * time.Minute

	// HTTP client timeout for registration API calls
	registrationHTTPTimeout = 15 * time.Second
)

// registrationClient is a shared HTTP client with a reasonable timeout.
// The default http.DefaultClient has no timeout and can hang indefinitely.
var registrationClient = &http.Client{Timeout: registrationHTTPTimeout}

// InitResponse is the response from POST /device/init
type InitResponse struct {
	DeviceCode      string `json:"deviceCode"`
	UserCode        string `json:"userCode"`
	VerificationURL string `json:"verificationUrl"`
	ExpiresIn       int    `json:"expiresIn"`
}

// PollResponse is the response from GET /device/poll
type PollResponse struct {
	Status               string `json:"status"`
	AgentID              string `json:"agentId,omitempty"`
	AgentSecret          string `json:"agentSecret,omitempty"`
	ProjectID            string `json:"projectId,omitempty"`
	UserID               string `json:"userId,omitempty"`
	CommandsSubscription string `json:"commandsSubscription,omitempty"`
	ResultsTopic         string `json:"resultsTopic,omitempty"`
	// Workload Identity Federation config
	TokenEndpoint     string `json:"tokenEndpoint,omitempty"`
	WIFAudience       string `json:"wifAudience,omitempty"`
	WIFServiceAccount string `json:"wifServiceAccount,omitempty"`
	// Storage config
	StorageBucket string `json:"storageBucket,omitempty"`
}

// ErrorResponse is the error response from the API
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// getDeviceName returns the computer name or a default
func getDeviceName() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "Unknown Device"
	}
	return hostname
}

// getRegistrationURL returns the base URL for registration API
// EnvAPIEndpoint is set via ldflags at build time for each environment
// Can be overridden via TERMINAL_SERVICE_URL env var for local development
func getRegistrationURL() string {
	// Check for environment variable override (for local dev)
	if url := os.Getenv("TERMINAL_SERVICE_URL"); url != "" {
		return url
	}
	return EnvAPIEndpoint
}

// initRegistration calls POST /device/init to start the registration flow
func initRegistration(deviceName, platform string, cfg *Config) (*InitResponse, error) {
	baseURL := getRegistrationURL()
	url := baseURL + "/device/init"

	// Use interface{} to allow mixed types (string and string)
	payload := map[string]interface{}{
		"deviceName":       deviceName,
		"platform":         platform,
		"version":          Version,
		"workingDirectory": cfg.WorkingDirectory, // Send working directory for remote command execution
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := registrationClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to call init endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp ErrorResponse
		json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&errResp)
		return nil, fmt.Errorf("init failed: %s - %s", errResp.Code, errResp.Message)
	}

	var initResp InitResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&initResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &initResp, nil
}

// pollForAuthorization polls the /device/poll endpoint until authorized or timeout
func pollForAuthorization(deviceCode string, timeout time.Duration) (*PollResponse, error) {
	baseURL := getRegistrationURL()
	pollURL := baseURL + "/device/poll?deviceCode=" + url.QueryEscape(deviceCode)

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	fmt.Print("Waiting for authorization")

	for {
		select {
		case <-ticker.C:
			if time.Now().After(deadline) {
				fmt.Println()
				return nil, fmt.Errorf("authorization timed out after %v", timeout)
			}

			resp, err := registrationClient.Get(pollURL)
			if err != nil {
				fmt.Print("!")
				continue
			}

			var pollResp PollResponse
			err = json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&pollResp)
			resp.Body.Close()

			if err != nil {
				fmt.Print("?")
				continue
			}

			switch pollResp.Status {
			case "authorized":
				fmt.Println(" ✓")
				return &pollResp, nil
			case "denied":
				fmt.Println(" ✗")
				return nil, fmt.Errorf("authorization denied by user")
			case "expired":
				fmt.Println(" !")
				return nil, fmt.Errorf("authorization code expired")
			case "claimed":
				fmt.Println(" !")
				return nil, fmt.Errorf("credentials already claimed - please try again")
			case "pending":
				fmt.Print(".")
			default:
				fmt.Print(".")
			}
		}
	}
}

// StartRegistration initiates the device registration flow
// Returns nil on success, error on failure
func StartRegistration(cfg *Config) error {
	deviceName := getDeviceName()
	platform := runtime.GOOS

	fmt.Println("")
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║                   Device Registration                      ║")
	fmt.Println("╠════════════════════════════════════════════════════════════╣")
	fmt.Println("║                                                            ║")
	fmt.Println("║  Connecting to AI Expedite...                              ║")
	fmt.Println("║                                                            ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println("")

	// Step 1: Initialize registration
	fmt.Printf("→ Device: %s (%s)\n", deviceName, platform)
	fmt.Println("→ Initiating registration...")

	initResp, err := initRegistration(deviceName, platform, cfg)
	if err != nil {
		return fmt.Errorf("failed to initiate registration: %w", err)
	}

	// Step 2: Show user code and open browser
	fmt.Println("")
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Printf("║                Your code:  %s                         ║\n", initResp.UserCode)
	fmt.Println("╠════════════════════════════════════════════════════════════╣")
	fmt.Println("║                                                            ║")
	fmt.Println("║  A browser window will open. Log in to your AI Expedite   ║")
	fmt.Println("║  account and enter the code above to authorize this       ║")
	fmt.Println("║  device.                                                   ║")
	fmt.Println("║                                                            ║")
	fmt.Printf("║  Code expires in %d minutes.                              ║\n", initResp.ExpiresIn/60)
	fmt.Println("║                                                            ║")
	fmt.Println("║  If browser doesn't open, visit:                          ║")
	fmt.Printf("║  %s\n", initResp.VerificationURL)
	fmt.Println("║                                                            ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println("")

	// Open browser to verification URL
	openBrowser(initResp.VerificationURL)

	// Step 3: Poll for authorization
	credentials, err := pollForAuthorization(initResp.DeviceCode, pollTimeout)
	if err != nil {
		return fmt.Errorf("authorization failed: %w", err)
	}

	// Step 4: Save credentials to config
	cfg.AgentID = credentials.AgentID
	cfg.CommandSecret = credentials.AgentSecret
	cfg.ProjectID = credentials.ProjectID
	cfg.UserID = credentials.UserID
	cfg.CommandsSubscription = credentials.CommandsSubscription
	cfg.ResultsTopic = credentials.ResultsTopic
	cfg.DeviceName = deviceName
	cfg.RegisteredAt = time.Now().Format(time.RFC3339)
	// Workload Identity Federation config for GCP authentication
	cfg.TokenEndpoint = credentials.TokenEndpoint
	cfg.WIFAudience = credentials.WIFAudience
	cfg.WIFServiceAccount = credentials.WIFServiceAccount
	// Override default storage bucket with environment-specific value from backend
	if credentials.StorageBucket != "" {
		cfg.StorageBucket = credentials.StorageBucket
	}

	if err := cfg.Save(ConfigPath()); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Println("")
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║          ✓ Device Registered Successfully!                ║")
	fmt.Println("╠════════════════════════════════════════════════════════════╣")
	fmt.Println("║                                                            ║")
	fmt.Printf("║  Agent ID: %s\n", cfg.AgentID)
	fmt.Printf("║  User ID:  %s\n", cfg.UserID)
	fmt.Println("║                                                            ║")
	fmt.Println("║  Your terminal is now connected to AI Expedite.           ║")
	fmt.Println("║  You can close this console window.                       ║")
	fmt.Println("║                                                            ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println("")

	return nil
}

// CancelRegistration is called when the user wants to cancel a pending registration
func CancelRegistration() {
	fmt.Println("\n→ Registration cancelled by user.")
}
