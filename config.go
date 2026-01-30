// File: config.go
package main

import (
	"encoding/json"
	"errors"
	"io/ioutil"
	"os"
)

// Config holds configuration for the agent, loaded from a JSON file.
type Config struct {
	/* ─── Pub/Sub integration ───────────────────────── */
	ProjectID            string `json:"project_id,omitempty"`
	CommandsSubscription string `json:"commands_subscription,omitempty"`
	ResultsTopic         string `json:"results_topic,omitempty"`

	/* ─── (optional) legacy relay mode ──────────────── */
	RelayURL string `json:"relay_url,omitempty"`
	JWT      string `json:"jwt,omitempty"`

	/* ─── mTLS (relay mode) ─────────────────────────── */
	CertFile string `json:"cert_file,omitempty"`
	KeyFile  string `json:"key_file,omitempty"`
	CAFile   string `json:"ca_file,omitempty"`

	/* ─── Local ttyd ────────────────────────────────── */
	LocalTtydPort int  `json:"local_ttyd_port,omitempty"` // 0 = 7681
	AutoUpdate    bool `json:"auto_update,omitempty"`

	/* ─── File Upload (GCS) ─────────────────────────── */
	StorageBucket    string `json:"storage_bucket,omitempty"`     // Firebase bucket name
	EnableFileUpload bool   `json:"enable_file_upload,omitempty"` // Feature flag

	/* ─── Command Security (Allow List) ────────────── */
	EnableAllowList       bool   `json:"enable_allow_list"`         // Enable command validation (default: true)
	AllowListPath         string `json:"allow_list_path,omitempty"` // Custom path to allow list file (optional)
	ApprovalTimeoutSec    int    `json:"approval_timeout_sec"`      // Seconds before dialog auto-closes (default: 60)
	ApprovalTimeoutAction string `json:"approval_timeout_action"`   // "deny" or "allow" on timeout (default: "deny")
	LogDeniedCommands     bool   `json:"log_denied_commands"`       // Log denied commands for audit (default: true)

	/* ─── Command Signature Verification ────────────── */
	AgentID       string `json:"agent_id,omitempty"`       // Unique agent identifier for signature verification
	CommandSecret string `json:"command_secret,omitempty"` // HMAC secret for command signature verification

	/* ─── Device Registration ───────────────────────── */
	UserID           string `json:"user_id,omitempty"`           // User ID from registration
	DeviceName       string `json:"device_name,omitempty"`       // Human-readable device name
	RegisteredAt     string `json:"registered_at,omitempty"`     // ISO timestamp of registration
	WorkingDirectory string `json:"working_directory,omitempty"` // Default working directory for commands

	/* ─── Update Preferences ───────────────────────── */
	SkippedVersion string `json:"skipped_version,omitempty"` // Version user chose to skip (won't prompt again)

	/* ─── Performance Tuning ─────────────────────────── */
	MaxOutstandingMessages int `json:"max_outstanding_messages,omitempty"` // Parallel message processing (default: 5)
	RateLimitPerSecond     int `json:"rate_limit_per_second,omitempty"`    // Commands per second per user (default: 10)
	RateLimitBurst         int `json:"rate_limit_burst,omitempty"`         // Burst allowance (default: 20)

	/* ─── Workload Identity Federation (GCP Auth) ───── */
	TokenEndpoint     string `json:"token_endpoint,omitempty"`      // Backend URL for OIDC token exchange
	WIFAudience       string `json:"wif_audience,omitempty"`        // GCP Workload Identity Pool audience
	WIFServiceAccount string `json:"wif_service_account,omitempty"` // GCP service account to impersonate

	/* ─── Debug Mode ─────────────────────────────────── */
	DebugMode bool `json:"debug_mode,omitempty"` // Show detailed command/response info
}

/* -------------------------------------------------------------------------- */

func LoadConfig(path string) (*Config, error) {
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (cfg *Config) Save(path string) error {
	b, err := json.MarshalIndent(cfg, "", "    ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(GetConfigDir(), 0o755); err != nil {
		return err
	}
	return ioutil.WriteFile(path, b, 0o600)
}

// IsRegistered returns true if the device has been registered with the backend.
func (cfg *Config) IsRegistered() bool {
	return cfg.AgentID != "" && cfg.CommandSecret != ""
}

func DefaultConfig() *Config {
	// Get user home directory for default working directory
	homeDir, _ := os.UserHomeDir()

	return &Config{
		ProjectID:            "", // MUST be filled by the user for Pub/Sub mode
		CommandsSubscription: "terminal-commands-sub",
		ResultsTopic:         "terminal-results",

		LocalTtydPort: 7681,
		AutoUpdate:    false, // Disabled by default (can be enabled in config file)

		// File upload defaults
		StorageBucket:    "aix-core-dev-app-s1e4.firebasestorage.app",
		EnableFileUpload: true,

		// Command security defaults
		EnableAllowList:       true,   // Security enabled by default
		ApprovalTimeoutSec:    60,     // 60 second timeout for approval dialog
		ApprovalTimeoutAction: "deny", // Deny on timeout (safer default)
		LogDeniedCommands:     true,   // Log all denied commands for audit

		// Signature verification defaults (empty = disabled until agent is registered)
		AgentID:       "",
		CommandSecret: "",

		// Working directory defaults to user home
		WorkingDirectory: homeDir,

		// Performance tuning defaults
		MaxOutstandingMessages: 5,  // Process 5 messages in parallel
		RateLimitPerSecond:     10, // 10 commands/second per user
		RateLimitBurst:         20, // Burst of 20
	}
}

// Validate makes sure *one* transport (Pub/Sub or relay) is configured.
func (cfg *Config) Validate() error {
	pubsubConfigured := cfg.ProjectID != "" &&
		cfg.CommandsSubscription != "" &&
		cfg.ResultsTopic != ""

	relayConfigured := cfg.RelayURL != ""

	switch {
	case pubsubConfigured && relayConfigured:
		return errors.New("both Pub/Sub and Relay are configured – choose one")
	case !pubsubConfigured && !relayConfigured:
		return errors.New("neither Pub/Sub nor Relay is configured")
	case pubsubConfigured:
		return nil // good
	default: // relayConfigured only
		if cfg.JWT == "" {
			return errors.New("relay_url set but jwt missing")
		}
		return nil
	}
}
