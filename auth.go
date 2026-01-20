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
	"sync"
	"time"

	"golang.org/x/oauth2"
)

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
}

// NewWIFTokenSource creates a new token source for Workload Identity Federation.
func NewWIFTokenSource(cfg *Config) *WIFTokenSource {
	return &WIFTokenSource{cfg: cfg}
}

// Token returns a valid OAuth2 token, refreshing if necessary.
// This implements the oauth2.TokenSource interface.
func (ts *WIFTokenSource) Token() (*oauth2.Token, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	// Return cached token if still valid (with 5 minute buffer)
	if ts.cachedToken != nil && ts.cachedToken.Valid() {
		if time.Until(ts.cachedToken.Expiry) > 5*time.Minute {
			return ts.cachedToken, nil
		}
	}

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

	// Cache the token
	ts.cachedToken = &oauth2.Token{
		AccessToken: accessToken,
		TokenType:   "Bearer",
		Expiry:      expiry,
	}

	return ts.cachedToken, nil
}

/* --------------------------------------------------------------------------
   Step 1: Get OIDC Token from Backend
   -------------------------------------------------------------------------- */

// oidcTokenResponse is the response from POST /auth/token
type oidcTokenResponse struct {
	IDToken   string `json:"id_token"`
	ExpiresIn int    `json:"expires_in"`
	TokenType string `json:"token_type"`
	Error     string `json:"error,omitempty"`
	ErrorDesc string `json:"error_description,omitempty"`
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

	// Build request payload
	payload := map[string]interface{}{
		"agentId":   ts.cfg.AgentID,
		"timestamp": timestamp,
		"signature": signature,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	// Make request to backend
	resp, err := http.Post(ts.cfg.TokenEndpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response
	respBody, err := io.ReadAll(resp.Body)
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
	resp, err := http.PostForm(stsURL, formData)
	if err != nil {
		return "", fmt.Errorf("STS request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response
	respBody, err := io.ReadAll(resp.Body)
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("impersonation request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response
	respBody, err := io.ReadAll(resp.Body)
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
