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
	ProjectID            string `json:"project_id,omitempty"`             // GCP project
	CommandsSubscription string `json:"commands_subscription,omitempty"`   // e.g. terminal‑commands‑sub
	ResultsTopic         string `json:"results_topic,omitempty"`          // e.g. terminal‑results

	/* ─── (optional) legacy relay mode ──────────────── */
	RelayURL   string `json:"relay_url,omitempty"` // WSS endpoint (legacy)
	JWT        string `json:"jwt,omitempty"`       // Auth token for relay

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
		/* Pub/Sub defaults – change if you renamed the topics */
		ProjectID:            "",                       // MUST be filled by the user
		CommandsSubscription: "terminal-commands-sub",
		ResultsTopic:         "terminal-results",

		LocalTtydPort: 7681,
		AutoUpdate:    true,
	}
}

func (cfg *Config) Validate() error {
	if cfg.ProjectID == "" {
		return errors.New("project_id is not set in config")
	}
	if cfg.CommandsSubscription == "" || cfg.ResultsTopic == "" {
		return errors.New("Pub/Sub subscription or topic not configured")
	}
	return nil
}
