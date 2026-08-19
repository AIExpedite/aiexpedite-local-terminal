// registration_recovery_test.go
// -----------------------------------------------------------------------------
// A registration that cannot be used must always leave the user a way back.
//
// The trap this pins: IsRegistered() is true when agent_id and command_secret
// are set, so the tray shows "Register Device" ticked and DISABLED. If the rest
// of the registration is missing, StartPubSubLoop printed
// "disabled – project_id empty" and returned — the device accepted no work, the
// tray claimed it was registered, and nothing in the UI could clear it. A real
// device sat like that for two days.
//
// The service-disowned path ("Unknown agent") already recovered correctly; these
// tests pin that BOTH paths now go through invalidateRegistration, and that a
// never-registered device is left alone.
// -----------------------------------------------------------------------------

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func registeredTransportCfg() *Config {
	cfg := DefaultConfig()
	cfg.AgentID = "JieUsRGg-1776531354324-bdd43572"
	cfg.CommandSecret = "secret"
	cfg.UserID = "uid-1"
	cfg.ProjectID = "aix-core-prod-app-c7o2"
	cfg.CommandsSubscription = "terminal-commands-agent-JieUsRGg-1776531354324-bdd43572"
	cfg.ResultsTopic = "terminal-results"
	cfg.TokenEndpoint = "https://api.aiexpedite.com/auth/token"
	cfg.WIFAudience = "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/x"
	cfg.WIFServiceAccount = "terminal-workload@example.iam.gserviceaccount.com"
	return cfg
}

func TestUnusableRegistrationReason(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*Config)
		unusable bool
	}{
		{"a complete registration is usable", func(*Config) {}, false},
		{"missing project id", func(c *Config) { c.ProjectID = "" }, true},
		{"missing command subscription", func(c *Config) { c.CommandsSubscription = "" }, true},
		{"missing results topic", func(c *Config) { c.ResultsTopic = "" }, true},
		{
			// The exact shape a test fixture left on a real device: credentials
			// present, every transport field defaulted away.
			"a fixture-clobbered config",
			func(c *Config) {
				c.AgentID = "agent-1"
				c.CommandSecret = "secret"
				c.ProjectID = ""
			},
			true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := registeredTransportCfg()
			tc.mutate(cfg)
			reason, unusable := unusableRegistrationReason(cfg)
			if unusable != tc.unusable {
				t.Fatalf("unusable = %v, want %v (reason %q)", unusable, tc.unusable, reason)
			}
			if unusable && reason == "" {
				t.Fatal("an unusable registration must carry a reason to show the user")
			}
			if !unusable && reason != "" {
				t.Fatalf("a usable registration must carry no reason, got %q", reason)
			}
		})
	}
}

// A device that never registered is NOT broken: the tray already offers
// registration, and clearing here would discard a half-written credential pair
// mid-registration.
func TestUnusableRegistrationLeavesAnUnregisteredDeviceAlone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"never registered", func(c *Config) { c.AgentID = ""; c.CommandSecret = ""; c.ProjectID = "" }},
		{"agent id without a secret", func(c *Config) { c.CommandSecret = ""; c.ProjectID = "" }},
		{"secret without an agent id", func(c *Config) { c.AgentID = ""; c.ProjectID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := registeredTransportCfg()
			tc.mutate(cfg)
			if _, unusable := unusableRegistrationReason(cfg); unusable {
				t.Fatal("an unregistered device must not be treated as an unusable registration")
			}
		})
	}
	if _, unusable := unusableRegistrationReason(nil); unusable {
		t.Fatal("a nil config must not be treated as an unusable registration")
	}
}

func TestInvalidateRegistrationClearsCredentialsAndSignalsTheTray(t *testing.T) {
	drainRegistrationInvalidChan()
	cfg := registeredTransportCfg()
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfigForTest(t, cfg, path)

	invalidateRegistration(cfg, "test reason")

	// Every credential goes together: a partial set is what produces the
	// ticked-but-broken tray state this function exists to escape.
	for name, got := range map[string]string{
		"AgentID":           cfg.AgentID,
		"CommandSecret":     cfg.CommandSecret,
		"UserID":            cfg.UserID,
		"RegisteredAt":      cfg.RegisteredAt,
		"TokenEndpoint":     cfg.TokenEndpoint,
		"WIFAudience":       cfg.WIFAudience,
		"WIFServiceAccount": cfg.WIFServiceAccount,
	} {
		if got != "" {
			t.Errorf("%s = %q, want it cleared", name, got)
		}
	}
	if cfg.IsRegistered() {
		t.Fatal("IsRegistered() still true — the tray would keep Register Device disabled")
	}

	select {
	case <-RegistrationInvalidChan:
	default:
		t.Fatal("no signal on RegistrationInvalidChan — the tray never re-enables Register Device")
	}
}

// The signal send is non-blocking: a full channel (nobody draining, e.g. the
// tray not up yet) must not wedge the caller.
func TestInvalidateRegistrationDoesNotBlockOnAFullChannel(t *testing.T) {
	drainRegistrationInvalidChan()
	RegistrationInvalidChan <- true
	t.Cleanup(drainRegistrationInvalidChan)

	cfg := registeredTransportCfg()
	writeConfigForTest(t, cfg, filepath.Join(t.TempDir(), "config.json"))

	done := make(chan struct{})
	go func() {
		invalidateRegistration(cfg, "test reason")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("invalidateRegistration blocked on a full RegistrationInvalidChan")
	}
}

func TestInvalidateRegistrationIsNilSafe(t *testing.T) {
	invalidateRegistration(nil, "test reason") // must not panic
}

// The credentials must be gone from DISK too — otherwise the next boot reloads
// them and lands straight back in the dead end.
func TestInvalidateRegistrationPersistsTheClearedCredentials(t *testing.T) {
	drainRegistrationInvalidChan()
	t.Cleanup(drainRegistrationInvalidChan)

	cfg := registeredTransportCfg()
	// invalidateRegistration saves through ConfigPath(); the suite sandbox
	// (TestMain) already points that at a temp directory.
	invalidateRegistration(cfg, "test reason")

	raw, err := os.ReadFile(ConfigPath())
	if err != nil {
		t.Fatalf("config was not persisted: %v", err)
	}
	var persisted map[string]any
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("persisted config is not JSON: %v", err)
	}
	for _, key := range []string{"agent_id", "command_secret", "user_id", "token_endpoint", "wif_audience", "wif_service_account"} {
		if v, present := persisted[key]; present && v != "" {
			t.Errorf("persisted %s = %v, want it absent or empty", key, v)
		}
	}
}

func drainRegistrationInvalidChan() {
	for {
		select {
		case <-RegistrationInvalidChan:
		default:
			return
		}
	}
}

func writeConfigForTest(t *testing.T, cfg *Config, path string) {
	t.Helper()
	if err := cfg.MutateAndSave(path, func() {}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
}
