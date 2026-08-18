// File: config_autoupdate_test.go
// Tests for the *bool "Automatically update" preference: absent (legacy) reads
// as enabled, explicit false is preserved, and the runtime atomic mirror stays
// in lockstep.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfig_LegacyAbsentAutoUpdateEnabled(t *testing.T) {
	// A config written by a version that predates this feature omits the field.
	p := writeConfigFile(t, `{"project_id":"x","commands_subscription":"s","results_topic":"r"}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AutoUpdate == nil || !*cfg.AutoUpdate {
		t.Fatalf("legacy absent auto_update should be treated as enabled, got %v", cfg.AutoUpdate)
	}
	if !cfg.IsAutoUpdate() {
		t.Fatal("runtime mirror should report enabled for legacy config")
	}
	// The value must be rewritten explicitly so a later "unset" can only mean a
	// replaced/rolled-back file.
	raw, _ := os.ReadFile(p)
	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if _, ok := onDisk["auto_update"]; !ok {
		t.Fatal("auto_update should be written explicitly after load")
	}
}

func TestLoadConfig_ExplicitFalsePreserved(t *testing.T) {
	p := writeConfigFile(t, `{"auto_update":false,"project_id":"x","commands_subscription":"s","results_topic":"r"}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AutoUpdate == nil || *cfg.AutoUpdate {
		t.Fatalf("explicit auto_update:false must be preserved, got %v", cfg.AutoUpdate)
	}
	if cfg.IsAutoUpdate() {
		t.Fatal("runtime mirror should report disabled for explicit false")
	}
}

func TestLoadConfig_ExplicitTruePreserved(t *testing.T) {
	p := writeConfigFile(t, `{"auto_update":true,"project_id":"x","commands_subscription":"s","results_topic":"r"}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AutoUpdate == nil || !*cfg.AutoUpdate || !cfg.IsAutoUpdate() {
		t.Fatalf("explicit auto_update:true must be preserved, got %v", cfg.AutoUpdate)
	}
}

func TestDefaultConfig_AutoUpdateEnabled(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.AutoUpdate == nil || !*cfg.AutoUpdate {
		t.Fatal("fresh install should default auto-update ON")
	}
	if !cfg.IsAutoUpdate() {
		t.Fatal("runtime mirror should report enabled on a fresh DefaultConfig")
	}
}

func TestSetAutoUpdate_RoundTripAndMirror(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SetAutoUpdate(false)
	if *cfg.AutoUpdate || cfg.IsAutoUpdate() {
		t.Fatal("SetAutoUpdate(false) should update both pointer and mirror")
	}

	p := filepath.Join(t.TempDir(), "config.json")
	if err := cfg.Save(p); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.AutoUpdate == nil || *reloaded.AutoUpdate || reloaded.IsAutoUpdate() {
		t.Fatalf("persisted disable should survive reload, got %v", reloaded.AutoUpdate)
	}
}

func TestSetAndSaveAutoUpdate_RollsBackOnSaveFailure(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.IsAutoUpdate() {
		t.Fatal("test requires auto-update to start enabled")
	}

	// Writing a file over an existing directory fails on every supported OS.
	err := cfg.SetAndSaveAutoUpdate(false, t.TempDir())
	if err == nil {
		t.Fatal("SetAndSaveAutoUpdate should report the persistence failure")
	}
	if cfg.AutoUpdate == nil || !*cfg.AutoUpdate || !cfg.IsAutoUpdate() {
		t.Fatal("failed persistence must restore both pointer and runtime preference")
	}
}
