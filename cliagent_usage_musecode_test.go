package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMuseCodeUsageParser_ReadinessStates(t *testing.T) {
	isolateMuseCode(t)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	detected := detectedCLIAgent{Name: "Muse Code", Detected: true, Version: "1.4.0", Path: "/usr/local/bin/muse"}

	t.Run("detected, signed out (no evidence) reads unknown, never Login-required", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("META_API_KEY", "")
		t.Setenv("XDG_CONFIG_HOME", "")
		usage, ok := museCodeUsageParser{}.Parse(home, detected, now)
		if !ok || usage == nil {
			t.Fatal("the probe must always emit an entry so the agent is listed")
		}
		if usage.AuthState != museCodeAuthUnknown || usage.Authenticated != nil {
			t.Fatalf("got authState=%q authenticated=%v", usage.AuthState, usage.Authenticated)
		}
		// No reading yet: both quota rows exist but read Unknown (dashed bars).
		if usage.CliAgentID != "museCode" || usage.Version != "1.4.0" || len(usage.Metrics) != 2 ||
			!usage.Metrics[0].Unknown || !usage.Metrics[1].Unknown {
			t.Fatalf("unexpected snapshot %#v", usage)
		}
	})

	t.Run("API key in the environment reads ready", func(t *testing.T) {
		t.Setenv("META_API_KEY", "k")
		usage, _ := museCodeUsageParser{}.Parse(t.TempDir(), detected, now)
		if usage.AuthState != museCodeAuthReady || usage.Authenticated == nil || !*usage.Authenticated {
			t.Fatalf("got %#v", usage)
		}
	})

	t.Run("a credential file from muse login reads ready", func(t *testing.T) {
		t.Setenv("META_API_KEY", "")
		t.Setenv("XDG_CONFIG_HOME", "")
		home := t.TempDir()
		dir := filepath.Join(home, ".config", "muse")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"t":"x"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		usage, _ := museCodeUsageParser{}.Parse(home, detected, now)
		if usage.AuthState != museCodeAuthReady {
			t.Fatalf("got %q", usage.AuthState)
		}
	})

	t.Run("an empty credential file is not evidence", func(t *testing.T) {
		t.Setenv("META_API_KEY", "")
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		_ = os.MkdirAll(filepath.Join(xdg, "muse"), 0o700)
		_ = os.WriteFile(filepath.Join(xdg, "muse", "auth.json"), nil, 0o600)
		usage, _ := museCodeUsageParser{}.Parse(t.TempDir(), detected, now)
		if usage.AuthState != museCodeAuthUnknown {
			t.Fatalf("got %q", usage.AuthState)
		}
	})
}

func TestMuseCodeUsage_GatherListsDetectedAgent(t *testing.T) {
	isolateMuseCode(t)
	t.Setenv("META_API_KEY", "")
	SetCLIAgentCatalog(nil)
	t.Cleanup(func() { SetCLIAgentCatalog(nil) })
	cliAgentCatalogMu.Lock()
	cliAgentCatalogConfigured = false
	cliAgentCatalogMu.Unlock()

	now := time.Now()
	out := gatherCLIAgentUsage(map[string]detectedCLIAgent{
		"museCode": {Name: "Muse Code", Detected: true, Version: "1.4.0", Path: "/x/muse"},
	}, now)
	if len(out) != 1 || out[0].CliAgentID != "museCode" || out[0].Provider != "museCode" {
		t.Fatalf("a detected Muse Code must produce exactly its own entry, got %#v", out)
	}
	if out[0].AccountFingerprint == "" {
		t.Fatal("the baseline entry still needs a per-device fingerprint bucket")
	}
}
