// Tests for the OfflineMode persistence path added as part of the
// terminal-disconnect-and-status-sync feature. The contract is: when the user
// toggles "Disconnect from cloud" in the tray, SetOffline must flip the
// in-memory flag AND persist it to disk so StartAgent can respect it across
// restarts.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfigOfflineModeFalse(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.OfflineMode {
		t.Fatalf("DefaultConfig should default OfflineMode to false, got true")
	}
}

// Config roundtrip ensures the JSON tag produces a stable shape on disk and
// that a file written by one process can be read back by another — which is
// how cfg.OfflineMode survives a restart.
func TestConfigOfflineModeRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	original := DefaultConfig()
	original.ProjectID = "test-project"
	original.AgentID = "agent-123"
	original.CommandSecret = "secret-abc"
	original.OfflineMode = true

	if err := original.Save(path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	// Sanity: the JSON should contain the snake_case key — otherwise the
	// field would silently default to false on every load.
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("json parse failed: %v", err)
	}
	if parsed["offline_mode"] != true {
		t.Fatalf("expected offline_mode=true on disk, got %v", parsed["offline_mode"])
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if !loaded.OfflineMode {
		t.Fatalf("expected OfflineMode to persist as true, got false")
	}
}

// SetOffline must write the new state to disk through the provided config
// handle. Without this, a user who toggles Disconnect then quits the app
// would wake up "connected" the next launch — the opposite of what they asked.
func TestSetOfflinePersistsToConfig(t *testing.T) {
	// Redirect baseDir for the duration of this test so SetOffline's call to
	// cfg.Save(ConfigPath()) writes into a tmpdir we control rather than the
	// real user profile. baseDir is a package-level var set once in init();
	// swapping it here is safe because none of the other concurrent-goroutine
	// code (pubsub, tmux, ttyd) runs during a `go test` invocation.
	oldBaseDir := baseDir
	baseDir = t.TempDir()
	t.Cleanup(func() { baseDir = oldBaseDir })

	cfg := DefaultConfig()
	cfg.AgentID = "agent-1"
	cfg.CommandSecret = "x"

	// Baseline save so the file exists at ConfigPath().
	if err := cfg.Save(ConfigPath()); err != nil {
		t.Fatalf("baseline Save failed: %v", err)
	}

	SetOffline(true, cfg)
	if !cfg.OfflineMode {
		t.Fatalf("SetOffline(true) did not flip cfg.OfflineMode")
	}
	if !IsOffline() {
		t.Fatalf("SetOffline(true) did not flip in-memory isOffline")
	}
	// Read the file back — verify the flag really hit disk.
	reloaded, err := LoadConfig(ConfigPath())
	if err != nil {
		t.Fatalf("LoadConfig after SetOffline(true) failed: %v", err)
	}
	if !reloaded.OfflineMode {
		t.Fatalf("expected disk OfflineMode=true after SetOffline(true)")
	}

	// Drain the offlineChan so the signal from SetOffline doesn't bleed into
	// other tests or into the real pubsub loop.
	select {
	case <-offlineChan:
	default:
	}

	SetOffline(false, cfg)
	if cfg.OfflineMode {
		t.Fatalf("SetOffline(false) should have cleared cfg.OfflineMode")
	}
	if IsOffline() {
		t.Fatalf("SetOffline(false) should have cleared in-memory isOffline")
	}
	reloaded, err = LoadConfig(ConfigPath())
	if err != nil {
		t.Fatalf("LoadConfig after SetOffline(false) failed: %v", err)
	}
	if reloaded.OfflineMode {
		t.Fatalf("expected disk OfflineMode=false after SetOffline(false)")
	}

	// Drain again to keep state clean.
	select {
	case <-offlineChan:
	default:
	}
}

// Nil cfg is accepted so test paths (and any callers that don't have a live
// config handle) stay compatible with the variadic signature.
func TestSetOfflineAcceptsNilConfig(t *testing.T) {
	// Should not panic.
	SetOffline(true)
	if !IsOffline() {
		t.Fatalf("SetOffline(true) without cfg should still update in-memory state")
	}
	SetOffline(false)
	if IsOffline() {
		t.Fatalf("SetOffline(false) without cfg should still update in-memory state")
	}
	// Drain the channel.
	for i := 0; i < 2; i++ {
		select {
		case <-offlineChan:
		default:
		}
	}
}
