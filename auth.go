// File: auth.go
// Workload Identity Federation authentication for GCP services.
// Exchanges agent credentials for short-lived GCP access tokens.
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"
)

// authHTTPClient is used for all WIF token-exchange HTTP calls.
// http.DefaultClient has no timeout and can hang indefinitely on slow endpoints.
var authHTTPClient = &http.Client{Timeout: 30 * time.Second}

var refreshMachineInfoAfterCatalogUpdate = func() {
	go RefreshMachineInfoNow()
}

/* --------------------------------------------------------------------------
   WIF Token Source - Implements oauth2.TokenSource for GCP authentication
   -------------------------------------------------------------------------- */

// WIFTokenSource provides OAuth2 tokens via Workload Identity Federation.
// It exchanges agent credentials (agentId + secret) for:
// 1. An OIDC token from our backend
// 2. A GCP access token via the Security Token Service
type WIFTokenSource struct {
	cfg *Config

	// Mutex for thread-safe token refresh
	mu sync.Mutex

	// Cached token to avoid unnecessary refreshes
	cachedToken *oauth2.Token

	// sfg ensures only one token refresh is in-flight at a time.
	// When multiple goroutines all find the token expired simultaneously,
	// singleflight collapses them into a single HTTP round-trip and shares
	// the result, preventing a thundering herd of WIF refresh calls.
	sfg singleflight.Group
}

// NewWIFTokenSource creates a new token source for Workload Identity Federation.
func NewWIFTokenSource(cfg *Config) *WIFTokenSource {
	return &WIFTokenSource{cfg: cfg}
}

// Token returns a valid OAuth2 token, refreshing if necessary.
// This implements the oauth2.TokenSource interface.
//
// The mutex is held only for the cache check and the final store, not across
// the HTTP calls.  Holding the lock during two sequential remote round-trips
// (each with a 30 s timeout) would block every concurrent caller — including
// the Pub/Sub client's internal token refresh — for up to 60 seconds.
func (ts *WIFTokenSource) Token() (*oauth2.Token, error) {
	// Fast path: return cached token under read-equivalent check.
	ts.mu.Lock()
	if ts.cachedToken != nil && ts.cachedToken.Valid() {
		if time.Until(ts.cachedToken.Expiry) > 5*time.Minute {
			tok := ts.cachedToken
			ts.mu.Unlock()
			return tok, nil
		}
	}
	ts.mu.Unlock()

	// Slow path: use singleflight so that only one goroutine performs the two
	// sequential HTTP round-trips (OIDC + STS) when multiple callers find the
	// token expired at the same time.  Without this, MaxOutstandingMessages=5
	// goroutines could each fire 3 HTTP calls simultaneously — a thundering herd
	// that spikes backend load and wastes WIF quota.
	v, err, _ := ts.sfg.Do("refresh", func() (interface{}, error) {
		// Step 1: Get OIDC token from our backend
		idToken, err := ts.getOIDCToken()
		if err != nil {
			return nil, fmt.Errorf("failed to get OIDC token: %w", err)
		}

		// Step 2: Exchange OIDC token for GCP access token via STS
		accessToken, expiry, err := ts.exchangeForGCPToken(idToken)
		if err != nil {
			return nil, fmt.Errorf("failed to exchange token: %w", err)
		}

		newToken := &oauth2.Token{
			AccessToken: accessToken,
			TokenType:   "Bearer",
			Expiry:      expiry,
		}

		// Store under lock; keep whichever token expires later so we don't
		// regress to a shorter-lived one if two refreshes somehow raced.
		ts.mu.Lock()
		if ts.cachedToken == nil || newToken.Expiry.After(ts.cachedToken.Expiry) {
			ts.cachedToken = newToken
		} else {
			newToken = ts.cachedToken
		}
		ts.mu.Unlock()

		return newToken, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*oauth2.Token), nil
}

/* --------------------------------------------------------------------------
   Step 1: Get OIDC Token from Backend
   -------------------------------------------------------------------------- */

// oidcTokenResponse is the response from POST /auth/token
type oidcTokenResponse struct {
	IDToken         string                 `json:"id_token"`
	ExpiresIn       int                    `json:"expires_in"`
	TokenType       string                 `json:"token_type"`
	Error           string                 `json:"error,omitempty"`
	ErrorDesc       string                 `json:"error_description,omitempty"`
	CliAgentCatalog []cliAgentCatalogEntry `json:"cliAgentCatalog,omitempty"`
}

// getOIDCToken requests an OIDC ID token from our backend.
// Authentication is done via HMAC signature of agentId:timestamp.
func (ts *WIFTokenSource) getOIDCToken() (string, error) {
	if ts.cfg.TokenEndpoint == "" {
		return "", fmt.Errorf("token endpoint not configured")
	}
	if ts.cfg.AgentID == "" || ts.cfg.CommandSecret == "" {
		return "", fmt.Errorf("agent credentials not configured")
	}

	// Generate timestamp and signature
	timestamp := time.Now().UnixMilli()
	signature := generateHMAC(fmt.Sprintf("%s:%d", ts.cfg.AgentID, timestamp), ts.cfg.CommandSecret)

	// Build request payload. Include the agent's current OS, hostname, and
	// version so the backend can detect when credentials have been moved to a
	// different machine (e.g. config copied from a Windows box to a Mac). The
	// /auth/token route refreshes these on the global agent doc and flags the
	// per-workspace discovery cache as stale when platform actually flips —
	// otherwise the LLM keeps reading the original OS from Firestore and
	// generates the wrong shell syntax.
	payload := map[string]interface{}{
		"agentId":    ts.cfg.AgentID,
		"timestamp":  timestamp,
		"signature":  signature,
		"platform":   runtime.GOOS,
		"deviceName": getDeviceName(),
		"version":    Version,
	}

	// Embed machine-level metadata (CPU/RAM/disk/runtimes/shell/cliAgents/
	// architecture) gathered by systemInfo.go's background goroutine. The
	// backend lifts these fields onto the top-level `terminalAgents/{id}`
	// doc — Phase 2 of the platform/systemInfo consolidation. Before this,
	// machine info was gathered by an LLM-driven discovery probe that was
	// non-deterministic across devices (the missing-RAM-on-some-devices
	// bug). The Go-side gather is deterministic.
	//
	// The cache may be nil if the first gather hasn't completed yet (rare —
	// only on the very first /auth/token after startup, before the gather
	// goroutine returns). In that case we send the request without these
	// fields and terminal-service falls back to the legacy workspace
	// systemInfo path.
	if mi := GetMachineInfo(); mi != nil {
		payload["architecture"] = mi.Architecture
		if mi.CPU != nil {
			payload["cpu"] = mi.CPU
		}
		if mi.Memory != nil {
			payload["memory"] = mi.Memory
		}
		if len(mi.Disk) > 0 {
			payload["disk"] = mi.Disk
		}
		if len(mi.Runtimes) > 0 {
			payload["runtimes"] = mi.Runtimes
		}
		if len(mi.PackageManagers) > 0 {
			payload["packageManagers"] = mi.PackageManagers
		}
		if len(mi.Tools) > 0 {
			payload["tools"] = mi.Tools
		}
		if mi.Shell != nil {
			payload["shell"] = mi.Shell
		}
		if len(mi.DetectedCliAgents) > 0 {
			payload["detectedCliAgents"] = mi.DetectedCliAgents
		}
		if mi.CliAgents != nil {
			// Richer per-provider usage shape consumed by the CLI Agents tab.
			// Send explicit [] snapshots too so the backend can clear stale
			// utilization when a device no longer detects any CLI agents.
			// Sent alongside detectedCliAgents (not in place of it) so legacy
			// clients keep rendering the About-tab chip strip.
			payload["cliAgents"] = mi.CliAgents
		}
		if mi.Capabilities != nil {
			payload["capabilities"] = mi.Capabilities
		}
		if mi.CollectedAt != "" {
			payload["collectedAt"] = mi.CollectedAt
		}
		// Tier 1+2 additions (machine task-routability hints).
		if len(mi.GPU) > 0 {
			payload["gpu"] = mi.GPU
		}
		if mi.Battery != nil {
			payload["battery"] = mi.Battery
		}
		if mi.Live != nil {
			payload["live"] = mi.Live
		}
		if mi.DockerRunning != nil {
			payload["dockerRunning"] = *mi.DockerRunning
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	// Make request to backend
	resp, err := authHTTPClient.Post(ts.cfg.TokenEndpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response (64 KB cap — OIDC token responses are always small)
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	var tokenResp oidcTokenResponse
	if err := json.Unmarshal(respBody, &tokenResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	// Check for errors
	if resp.StatusCode != http.StatusOK {
		if tokenResp.Error != "" {
			return "", fmt.Errorf("%s: %s", tokenResp.Error, tokenResp.ErrorDesc)
		}
		return "", fmt.Errorf("token request failed with status %d", resp.StatusCode)
	}

	if ts.cfg.UpdateCLIAgentCatalog(tokenResp.CliAgentCatalog) {
		if err := ts.cfg.Save(ConfigPath()); err != nil {
			fmt.Printf("%s[auth] Failed to persist CLI-agent catalog from token response: %v%s\n", colorYellow, err, colorReset)
		}
		refreshMachineInfoAfterCatalogUpdate()
	}

	if tokenResp.IDToken == "" {
		return "", fmt.Errorf("empty id_token in response")
	}

	return tokenResp.IDToken, nil
}

/* --------------------------------------------------------------------------
   Step 2: Exchange OIDC Token for GCP Access Token via STS
   -------------------------------------------------------------------------- */

// stsTokenResponse is the response from GCP Security Token Service
type stsTokenResponse struct {
	AccessToken     string `json:"access_token"`
	IssuedTokenType string `json:"issued_token_type"`
	TokenType       string `json:"token_type"`
	ExpiresIn       int    `json:"expires_in"`
	Error           string `json:"error,omitempty"`
	ErrorDesc       string `json:"error_description,omitempty"`
}

// exchangeForGCPToken exchanges an OIDC token for a GCP access token.
// This is a two-step process:
// 1. Exchange OIDC token for a federated STS token
// 2. Use the federated token to impersonate the service account
func (ts *WIFTokenSource) exchangeForGCPToken(idToken string) (string, time.Time, error) {
	if ts.cfg.WIFAudience == "" {
		return "", time.Time{}, fmt.Errorf("WIF audience not configured")
	}
	if ts.cfg.WIFServiceAccount == "" {
		return "", time.Time{}, fmt.Errorf("WIF service account not configured")
	}

	// Step 1: Exchange OIDC token for federated STS token
	federatedToken, err := ts.getFederatedToken(idToken)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to get federated token: %w", err)
	}

	// Step 2: Impersonate service account using federated token
	accessToken, expiry, err := ts.impersonateServiceAccount(federatedToken)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to impersonate service account: %w", err)
	}

	return accessToken, expiry, nil
}

// getFederatedToken exchanges an OIDC token for a federated STS token.
func (ts *WIFTokenSource) getFederatedToken(idToken string) (string, error) {
	// GCP STS endpoint
	stsURL := "https://sts.googleapis.com/v1/token"

	// Build form data for token exchange
	formData := url.Values{
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"audience":             {ts.cfg.WIFAudience},
		"subject_token_type":   {"urn:ietf:params:oauth:token-type:jwt"},
		"subject_token":        {idToken},
		"requested_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"scope":                {"https://www.googleapis.com/auth/cloud-platform"},
	}

	// Make request to STS
	resp, err := authHTTPClient.PostForm(stsURL, formData)
	if err != nil {
		return "", fmt.Errorf("STS request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response (64 KB cap — STS responses are always small)
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("failed to read STS response: %w", err)
	}

	var stsResp stsTokenResponse
	if err := json.Unmarshal(respBody, &stsResp); err != nil {
		return "", fmt.Errorf("failed to decode STS response: %w", err)
	}

	// Check for errors
	if resp.StatusCode != http.StatusOK {
		if stsResp.Error != "" {
			return "", fmt.Errorf("STS error: %s - %s", stsResp.Error, stsResp.ErrorDesc)
		}
		return "", fmt.Errorf("STS request failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	if stsResp.AccessToken == "" {
		return "", fmt.Errorf("empty access_token in STS response")
	}

	return stsResp.AccessToken, nil
}

// impersonateServiceAccount uses a federated token to get an access token for a service account.
func (ts *WIFTokenSource) impersonateServiceAccount(federatedToken string) (string, time.Time, error) {
	// IAM Credentials API endpoint for generating access tokens
	impersonateURL := fmt.Sprintf(
		"https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/%s:generateAccessToken",
		ts.cfg.WIFServiceAccount,
	)

	// Request body
	reqBody := map[string]interface{}{
		"scope": []string{"https://www.googleapis.com/auth/cloud-platform"},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create request with federated token as bearer
	req, err := http.NewRequest("POST", impersonateURL, bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+federatedToken)
	req.Header.Set("Content-Type", "application/json")

	// Make request
	resp, err := authHTTPClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("impersonation request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response (64 KB cap — impersonation responses are always small)
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to read response: %w", err)
	}

	// Check for errors
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("impersonation failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	// Parse response
	var impersonateResp struct {
		AccessToken string `json:"accessToken"`
		ExpireTime  string `json:"expireTime"` // RFC3339 format
	}
	if err := json.Unmarshal(respBody, &impersonateResp); err != nil {
		return "", time.Time{}, fmt.Errorf("failed to decode response: %w", err)
	}

	if impersonateResp.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("empty access token in response")
	}

	// Parse expiry time
	expiry, err := time.Parse(time.RFC3339, impersonateResp.ExpireTime)
	if err != nil {
		// Default to 1 hour if parsing fails
		expiry = time.Now().Add(1 * time.Hour)
	}

	return impersonateResp.AccessToken, expiry, nil
}

/* --------------------------------------------------------------------------
   Helper Functions
   -------------------------------------------------------------------------- */

// generateHMAC creates an HMAC-SHA256 signature for authentication.
func generateHMAC(data, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

// IsWIFConfigured returns true if Workload Identity Federation is configured.
func IsWIFConfigured(cfg *Config) bool {
	return cfg.TokenEndpoint != "" &&
		cfg.WIFAudience != "" &&
		cfg.WIFServiceAccount != "" &&
		cfg.AgentID != "" &&
		cfg.CommandSecret != ""
}
