// File: config.go
package main

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
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
	LocalTtydPort int `json:"local_ttyd_port,omitempty"` // 0 = 7681

	// AutoUpdate is the "Automatically update" tray preference. It is a
	// *bool (not a plain bool) tagged WITHOUT omitempty so the value is
	// always written explicitly from this version onward — that is the only
	// way to distinguish "legacy config that predates this feature" (field
	// absent → JSON null after unmarshal → nil pointer) from a user who
	// deliberately turned auto-update off (explicit false). The old plain
	// `bool` + `omitempty` collapsed those two cases, so a legacy hand-edited
	// `auto_update: false` cannot be recovered; every legacy config is
	// therefore treated as enabled (see LoadConfig). Once this version writes
	// the field, a later "unset" can only mean a replaced/rolled-back file.
	//
	// Runtime reads on the Pub/Sub / auto-update goroutines MUST go through
	// IsAutoUpdate() — the tray goroutine writes the toggle while other
	// goroutines read it, so a direct pointer deref there is a data race.
	// autoUpdateRuntime below is the synchronised mirror kept in lockstep by
	// LoadConfig and SetAutoUpdate, mirroring the AllowAllCommands pattern.
	AutoUpdate        *bool       `json:"auto_update"`
	autoUpdateRuntime atomic.Bool // synchronised mirror of *AutoUpdate

	/* ─── File Upload (GCS) ─────────────────────────── */
	StorageBucket    string `json:"storage_bucket,omitempty"`     // Firebase bucket name
	EnableFileUpload bool   `json:"enable_file_upload,omitempty"` // Feature flag

	/* ─── Command Security (Allow List) ────────────── */
	EnableAllowList       bool   `json:"enable_allow_list"`         // Enable command validation (default: true)
	AllowListPath         string `json:"allow_list_path,omitempty"` // Custom path to allow list file (optional)
	ApprovalTimeoutSec    int    `json:"approval_timeout_sec"`      // Seconds before dialog auto-closes (default: 60)
	ApprovalTimeoutAction string `json:"approval_timeout_action"`   // "deny" or "allow" on timeout (default: "deny")
	LogDeniedCommands     bool   `json:"log_denied_commands"`       // Log denied commands for audit (default: true)
	// AllowAllCommands, when true, bypasses every allow-list check and
	// approval dialog — the agent will execute ANY incoming command
	// without prompting. This is a hard security-posture override; the
	// tray toggle requires a confirmation on enable and emits a
	// security-log event so accidental activation is loud.
	//
	// The persisted bool is the canonical on-disk value, but runtime
	// reads on the Pub/Sub goroutine MUST go through IsAllowAllCommands()
	// because the tray-menu goroutine in main.go writes the toggle while
	// Receive callbacks read it for every inbound command — a direct
	// bool access there is a data race (go test -race). The unexported
	// allowAllRuntime atomic.Bool below is the synchronised mirror;
	// LoadConfig and SetAllowAllCommands keep the two in lockstep.
	AllowAllCommands bool        `json:"allow_all_commands,omitempty"`
	allowAllRuntime  atomic.Bool // synchronised mirror of AllowAllCommands

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

	/* ─── Auto-update attempt bookkeeping (crash recovery) ─── */
	// When the automatic path is about to install, it records the in-flight
	// attempt id + target version here and Save()s, so a restart (successful
	// or crashed) can reconcile: if the running Version now equals the target
	// the update landed (report ready on the new version + attempt id);
	// otherwise the attempt was interrupted mid-drain and is abandoned so the
	// agent never comes back believing it is still draining. Cleared once
	// resolved. See resolveInterruptedAttempt in autoupdate.go.
	PendingUpdateAttemptID string `json:"pending_update_attempt_id,omitempty"`
	PendingUpdateVersion   string `json:"pending_update_version,omitempty"`

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

	/* ─── Offline Mode ───────────────────────────────── */
	// OfflineMode persists the user's "Disconnect from cloud" toggle across
	// restarts. When true, StartAgent bypasses the Pub/Sub loop so the agent
	// stays local-only until the user reconnects from the tray menu.
	OfflineMode bool `json:"offline_mode,omitempty"`

	/* ─── Grok ACP — API-key fallback ────────────────── */
	// EnableGrokAPIKeyFallback gates xAI Grok's API-key auth path. When
	// false (default), the Grok ACP manager strips XAI_API_KEY from the
	// child env AND any `--api-key*` / `--auth*` args from the spawn argv —
	// so the orchestrator's ACP authenticate flow can only resolve via the
	// local `grok login` cached token. Set to true on hosts where the user
	// has explicitly opted into API-key billing for this agent.
	EnableGrokAPIKeyFallback bool `json:"enable_grok_api_key_fallback,omitempty"`

	/* ─── Grok ACP — always-approve gate ─────────────── */
	// EnableGrokAlwaysApprove gates Grok's autonomous-tool-execution flags.
	// When false (default), the Grok ACP manager strips `--always-approve`
	// / `--auto-approve` (and equivalent `-c approval.mode=always|auto` /
	// `-c tools.always_approve=true` / `-c tools.auto_approve=true` config
	// overrides) from the spawn argv — so a signed `grok_acp_start` cannot
	// enable autonomous tool execution without an explicit per-workspace
	// opt-in. The feature brief makes approval behaviour conservative by
	// default; flip this only on workspaces where the user has actively
	// chosen to skip per-tool permission prompts.
	EnableGrokAlwaysApprove bool `json:"enable_grok_always_approve,omitempty"`

	/* ─── Claude Code status-line hook ───────────────── */
	// DisableClaudeStatusLineHook opts out of wiring Claude Code's `statusLine`
	// setting to our hook (the side channel that keeps the CLI Agents tab's
	// rate-limit metrics fresh from interactive usage). Off by default — the
	// hook installs on startup, preserving any existing status line by chaining.
	DisableClaudeStatusLineHook bool `json:"disable_claude_status_line_hook,omitempty"`

	/* ─── CLI-agent catalog ───────────────────────────── */
	// CliAgentCatalog is the backend-provided projection of the Firestore
	// cliAgent collection. When present it replaces the built-in fallback list
	// for detection and utilization, allowing new CLI agents to appear without
	// a local terminal code change.
	CliAgentCatalog   []cliAgentCatalogEntry `json:"-"`
	cliAgentCatalogMu sync.RWMutex
}

/* -------------------------------------------------------------------------- */

type configJSON Config

func (cfg *Config) MarshalJSON() ([]byte, error) {
	type configWithCatalog struct {
		*configJSON
		CliAgentCatalog *[]cliAgentCatalogEntry `json:"cliAgentCatalog,omitempty"`
	}

	out := configWithCatalog{configJSON: (*configJSON)(cfg)}
	if catalog := cfg.cliAgentCatalogSnapshot(); catalog != nil {
		out.CliAgentCatalog = &catalog
	}
	return json.Marshal(out)
}

func (cfg *Config) UnmarshalJSON(data []byte) error {
	type configWithCatalog struct {
		*configJSON
		CliAgentCatalog *[]cliAgentCatalogEntry `json:"cliAgentCatalog"`
	}

	in := configWithCatalog{configJSON: (*configJSON)(cfg)}
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	cfg.cliAgentCatalogMu.Lock()
	if in.CliAgentCatalog != nil {
		cfg.CliAgentCatalog = cloneCLIAgentCatalog(*in.CliAgentCatalog)
	} else {
		cfg.CliAgentCatalog = nil
	}
	cfg.cliAgentCatalogMu.Unlock()
	return nil
}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, err
	}
	// SkippedVersion is compared against live semver from GitHub. If the
	// on-disk value is not a well-formed semver (stale from a malformed
	// release body, hand-edited, or corrupted) it would silently suppress
	// all future update prompts because the `==` check would only match an
	// equally malformed remote value. Clear it so the user is re-prompted
	// the next time a real update is available.
	if cfg.SkippedVersion != "" && !isValidSemver(cfg.SkippedVersion) {
		cfg.SkippedVersion = ""
	}
	SetCLIAgentCatalog(cfg.cliAgentCatalogSnapshot())
	// Mirror the persisted AllowAllCommands bool into the atomic so the
	// first Pub/Sub Receive callback (which reads via IsAllowAllCommands)
	// sees the same value the user toggled in a previous session.
	cfg.allowAllRuntime.Store(cfg.AllowAllCommands)

	// Auto-update preference: a nil pointer means the field was absent — a
	// config written by a version that predates this feature. We cannot tell
	// a legacy hand-edited opt-out apart from absence (the old format omitted
	// the field when false), so absence is treated as ENABLED, matching the
	// default-on product decision. Rewrite the file so the value becomes
	// explicit from now on; a best-effort failure here is harmless (the
	// runtime mirror below still reflects the intended value, and the next
	// successful Save persists it).
	if cfg.AutoUpdate == nil {
		enabled := true
		cfg.AutoUpdate = &enabled
		if err := cfg.Save(path); err != nil {
			// Non-fatal: the in-memory value is authoritative for this run.
			_ = err
		}
	}
	cfg.autoUpdateRuntime.Store(*cfg.AutoUpdate)
	return cfg, nil
}

func (cfg *Config) UpdateCLIAgentCatalog(entries []cliAgentCatalogEntry) bool {
	if entries == nil {
		return false
	}
	normalized := normalizeCLIAgentCatalog(entries)

	cfg.cliAgentCatalogMu.Lock()
	changed := !reflect.DeepEqual(cfg.CliAgentCatalog, normalized)
	cfg.CliAgentCatalog = cloneCLIAgentCatalog(normalized)
	cfg.cliAgentCatalogMu.Unlock()

	SetCLIAgentCatalog(normalized)
	return changed
}

func (cfg *Config) cliAgentCatalogSnapshot() []cliAgentCatalogEntry {
	cfg.cliAgentCatalogMu.RLock()
	defer cfg.cliAgentCatalogMu.RUnlock()
	return cloneCLIAgentCatalog(cfg.CliAgentCatalog)
}

// IsAllowAllCommands returns the live AllowAllCommands flag using an
// atomic load. Pub/Sub Receive callbacks MUST call this rather than
// reading cfg.AllowAllCommands directly — the tray-menu goroutine in
// main.go writes the toggle, so unsynchronised reads from a different
// goroutine are a data race.
func (cfg *Config) IsAllowAllCommands() bool {
	return cfg.allowAllRuntime.Load()
}

// SetAllowAllCommands publishes a new AllowAllCommands value to
// concurrent readers AND keeps the persisted bool in sync so the next
// Save() writes the right thing. Call this from the tray goroutine
// AFTER a successful cfg.Save() so disk and in-memory state remain
// consistent on a failing write.
func (cfg *Config) SetAllowAllCommands(v bool) {
	cfg.AllowAllCommands = v
	cfg.allowAllRuntime.Store(v)
}

// IsAutoUpdate returns the live "Automatically update" preference using an
// atomic load. The auto-update scheduler and any goroutine other than the
// tray writer MUST call this rather than dereferencing cfg.AutoUpdate
// directly — the tray-menu goroutine writes the toggle, so an unsynchronised
// read from a different goroutine is a data race (go test -race).
func (cfg *Config) IsAutoUpdate() bool {
	return cfg.autoUpdateRuntime.Load()
}

// SetAutoUpdate publishes a new auto-update value to concurrent readers AND
// keeps the persisted pointer in sync so the next Save() writes the right
// thing. Call from the tray goroutine AFTER a successful cfg.Save() so disk
// and in-memory state stay consistent on a failing write — mirroring
// SetAllowAllCommands.
func (cfg *Config) SetAutoUpdate(v bool) {
	if cfg.AutoUpdate == nil {
		cfg.AutoUpdate = new(bool)
	}
	*cfg.AutoUpdate = v
	cfg.autoUpdateRuntime.Store(v)
}

func (cfg *Config) Save(path string) error {
	b, err := json.MarshalIndent(cfg, "", "    ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(GetConfigDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// IsRegistered returns true if the device has been registered with the backend.
func (cfg *Config) IsRegistered() bool {
	return cfg.AgentID != "" && cfg.CommandSecret != ""
}

func DefaultConfig() *Config {
	// Get user home directory for default working directory
	homeDir, _ := os.UserHomeDir()

	// "Automatically update" defaults ON. Uses a local so the *bool field holds
	// an explicit value (a fresh install writes the preference explicitly).
	autoUpdateDefault := true

	cfg := &Config{
		ProjectID:            "", // MUST be filled by the user for Pub/Sub mode
		CommandsSubscription: "terminal-commands-sub",
		ResultsTopic:         "terminal-results",

		LocalTtydPort: 7681,
		AutoUpdate:    &autoUpdateDefault, // "Automatically update" is enabled by default

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

		// Offline mode defaults to false (connected to cloud)
		OfflineMode: false,
	}
	// Keep the runtime mirror in lockstep with the default preference so a
	// DefaultConfig() used without a subsequent LoadConfig (fresh install,
	// tests) still reports auto-update enabled via IsAutoUpdate().
	cfg.autoUpdateRuntime.Store(cfg.AutoUpdate != nil && *cfg.AutoUpdate)
	return cfg
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
