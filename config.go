package main

import (
	"encoding/json"
	"errors"
	"io/ioutil"
	"os"
)

// Config holds configuration for the agent, loaded from a JSON file.
type Config struct {
	RelayURL      string `json:"relay_url"`        // WSS endpoint for cloud relay
	JWT           string `json:"jwt"`              // Authentication token (JWT)
	CertFile      string `json:"cert_file,omitempty"` // Client certificate for mTLS (optional)
	KeyFile       string `json:"key_file,omitempty"`  // Client key for mTLS (optional)
	CAFile        string `json:"ca_file,omitempty"`   // CA certificate for server verification (optional)
	LocalTtydPort int    `json:"local_ttyd_port,omitempty"` // Port for local ttyd (0 = default 7681)
	AutoUpdate    bool   `json:"auto_update,omitempty"`    // Enable auto-update check
}

// LoadConfig reads configuration from the given file path (if it exists).
func LoadConfig(path string) (*Config, error) {
	cfg := &Config{}
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Save writes the configuration to the given file path in JSON format.
func (cfg *Config) Save(path string) error {
	data, err := json.MarshalIndent(cfg, "", "    ")
	if err != nil {
		return err
	}
	// Ensure directory exists
	if err := os.MkdirAll(GetConfigDir(), 0755); err != nil {
		return err
	}
	return ioutil.WriteFile(path, data, 0600)
}

// DefaultConfig returns a Config with default values and placeholders.
func DefaultConfig() *Config {
	return &Config{
		RelayURL:      "wss://your-relay.example.com/agent", // placeholder URL
		JWT:           "CHANGE_ME",       // placeholder token (to be set by user)
		CertFile:      "",
		KeyFile:       "",
		CAFile:        "",
		LocalTtydPort: 7681,
		AutoUpdate:    true,
	}
}

// Validate checks if required fields in config are set.
func (cfg *Config) Validate() error {
	if cfg.RelayURL == "" {
		return errors.New("RelayURL is not set in config")
	}
	if cfg.JWT == "" {
		return errors.New("JWT token is not set in config")
	}
	// CertFile/KeyFile are optional (only needed if using mTLS)
	return nil
}
