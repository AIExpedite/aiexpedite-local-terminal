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

func DefaultConfig() *Config {
	return &Config{
		ProjectID:            "", // MUST be filled by the user for Pub/Sub mode
		CommandsSubscription: "terminal-commands-sub",
		ResultsTopic:         "terminal-results",

		LocalTtydPort: 7681,
		AutoUpdate:    true,
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
