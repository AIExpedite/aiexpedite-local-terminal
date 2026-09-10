package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Codex round 11 on #147: with GROK_CONFIG_PATH set the list cannot prove it
// saw what a session sees (an external config may carry per-model keys the
// probe cannot reproduce; a relative path resolves against a session's cwd,
// which the probe has none of), so its catalog is a floor.

func TestDiscoverGrokModelsIsAFloorUnderAnExternalConfigPath(t *testing.T) {
	home := t.TempDir()
	real := filepath.Join(home, ".grok")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "models_cache.json"), []byte(realGrokModelsCache), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", "")
	prevCfg := shutdownConfig
	t.Cleanup(func() { shutdownConfig = prevCfg })
	shutdownConfig = nil
	prev := cliAgentModelProbeRunner
	t.Cleanup(func() { cliAgentModelProbeRunner = prev })
	cliAgentModelProbeRunner = func(context.Context, string, []string, ...string) (string, bool) {
		return realGrokModelsLoggedOut, true
	}
	detected := detectedCLIAgent{Detected: true, Path: "/x/grok", Version: "grok 1.0.13"}

	// Without the override the list's own verdict stands.
	t.Setenv("GROK_CONFIG_PATH", "")
	base, ok := discoverGrokModels(context.Background(), detected, home)
	if !ok || !base.Exhaustive {
		t.Fatalf("baseline must be exhaustive: ok=%v %#v", ok, base)
	}
	// An absolute external config (which may keep per-model keys the probe
	// cannot reproduce) and a relative one (resolved against a session's cwd)
	// both make the catalog a floor, whatever the file says.
	for _, path := range []string{filepath.Join(t.TempDir(), "grok.toml"), "config/grok.toml"} {
		t.Setenv("GROK_CONFIG_PATH", path)
		got, ok := discoverGrokModels(context.Background(), detected, home)
		if !ok {
			t.Fatalf("%q: expected a conclusive discovery", path)
		}
		if got.Exhaustive {
			t.Fatalf("%q: an external config path must make the catalog a floor", path)
		}
		if len(got.Models) < 2 || got.Models[0].ID != "grok-4.6" {
			t.Fatalf("%q: the list itself is still reported: %#v", path, got.Models)
		}
	}
}
