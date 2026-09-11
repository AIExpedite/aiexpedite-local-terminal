package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Codex round 8 on #147: the Grok list runs against an isolated home built
// like a session's (persisted API key copied only under the opt-in), and a
// catalog entry with its own id still reaches discovery through its parser key.

func TestDiscoverGrokModelsListsAgainstAnIsolatedHomeLikeASession(t *testing.T) {
	home := t.TempDir()
	real := filepath.Join(home, ".grok")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	// The real home: a cached login, a persisted API key the user did NOT
	// opt into, and a cache for the fallback path.
	if err := os.WriteFile(filepath.Join(real, "auth.json"), []byte(`{"token":"cached-login"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "config.toml"), []byte("[model]\napi_key = \"xai-persisted\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "models_cache.json"), []byte(realGrokModelsCache), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", "")
	prevCfg := shutdownConfig
	t.Cleanup(func() { shutdownConfig = prevCfg })
	prev := cliAgentModelProbeRunner
	t.Cleanup(func() { cliAgentModelProbeRunner = prev })

	var seenHome string
	var seenConfig string
	var seenAuth bool
	cliAgentModelProbeRunner = func(_ context.Context, _ string, env []string, _ ...string) (string, bool) {
		for _, entry := range env {
			if name, value, ok := strings.Cut(entry, "="); ok && name == "GROK_HOME" {
				seenHome = value
			}
		}
		if seenHome != "" {
			data, _ := os.ReadFile(filepath.Join(seenHome, "config.toml"))
			seenConfig = string(data)
			_, err := os.Stat(filepath.Join(seenHome, "auth.json"))
			seenAuth = err == nil
		}
		return realGrokModelsLoggedOut, true
	}

	// Opt-in OFF: the child sees an isolated home with the cached login but
	// WITHOUT the persisted key — exactly what a managed session gets.
	shutdownConfig = nil
	got, ok := discoverGrokModels(context.Background(), detectedCLIAgent{Detected: true, Path: "/x/grok", Version: "grok 1.0.13"}, home)
	if !ok {
		t.Fatal("expected a conclusive discovery")
	}
	if seenHome == "" || seenHome == real || strings.HasPrefix(seenHome, real) {
		t.Fatalf("grok models must run against an isolated home, ran with GROK_HOME=%q (real %q)", seenHome, real)
	}
	if !seenAuth {
		t.Fatal("the cached login must be copied into the isolated home")
	}
	if strings.Contains(seenConfig, "api_key") {
		t.Fatalf("a persisted api_key must not reach the list without the opt-in:\n%s", seenConfig)
	}
	if _, err := os.Stat(seenHome); !os.IsNotExist(err) {
		t.Fatalf("the isolated home must be removed after the probe: %v", err)
	}
	// The real home's cache is still the fallback when the child wrote none.
	if len(got.Models) < 2 || got.Models[0].ID != "grok-4.6" || got.Models[0].Label != "Grok 4.6" {
		t.Fatalf("real-home cache must enrich the list: %#v", got.Models)
	}

	// Opt-in ON: the persisted key rides along, as it does for a session.
	shutdownConfig = &Config{EnableGrokAPIKeyFallback: true}
	seenHome, seenConfig = "", ""
	if _, ok := discoverGrokModels(context.Background(), detectedCLIAgent{Detected: true, Path: "/x/grok", Version: "grok 1.0.13"}, home); !ok {
		t.Fatal("expected a conclusive discovery")
	}
	if !strings.Contains(seenConfig, "api_key = \"xai-persisted\"") {
		t.Fatalf("the opted-in persisted key must be carried into the isolated home:\n%s", seenConfig)
	}
}

func TestDiscoverGrokModelsPrefersTheCacheTheListJustRefreshed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GROK_HOME", "")
	prevCfg := shutdownConfig
	t.Cleanup(func() { shutdownConfig = prevCfg })
	shutdownConfig = nil
	prev := cliAgentModelProbeRunner
	t.Cleanup(func() { cliAgentModelProbeRunner = prev })
	// A signed-in list refreshes the cache under the home the child ran
	// with; the stub writes one there, as grok would.
	cliAgentModelProbeRunner = func(_ context.Context, _ string, env []string, _ ...string) (string, bool) {
		for _, entry := range env {
			if name, value, ok := strings.Cut(entry, "="); ok && name == "GROK_HOME" {
				_ = os.WriteFile(filepath.Join(value, "models_cache.json"), []byte(realGrokModelsCache), 0o644)
			}
		}
		return realGrokModelsLoggedOut, true
	}
	got, ok := discoverGrokModels(context.Background(), detectedCLIAgent{Detected: true, Path: "/x/grok", Version: "grok 1.0.13"}, home)
	if !ok || len(got.Models) < 2 || got.Models[0].Label != "Grok 4.6" || got.Models[0].DefaultEffort != "high" {
		t.Fatalf("the freshly written cache must enrich the list with no real-home cache at all: ok=%v %#v", ok, got.Models)
	}
}

func TestGatherCLIAgentUsage_DiscoversModelsThroughTheCatalogParserKey(t *testing.T) {
	// A backend catalog entry with its own id names a built-in parser; the
	// usage parser is selected by that key, and so must discovery be.
	SetCLIAgentCatalog([]cliAgentCatalogEntry{
		{
			ID:           "geminiBuild",
			DisplayName:  "Gemini Build",
			Command:      "agy",
			Capabilities: json.RawMessage(`{"utilization":{"parserKey":"antigravity"}}`),
		},
	})
	t.Cleanup(func() { SetCLIAgentCatalog(nil) })
	resetCLIAgentModelProbeCache()
	t.Cleanup(resetCLIAgentModelProbeCache)
	prev := cliAgentModelProbeRunner
	cliAgentModelProbeRunner = func(context.Context, string, []string, ...string) (string, bool) { return realAntigravityModels, true }
	t.Cleanup(func() { cliAgentModelProbeRunner = prev })
	home := t.TempDir()
	isolateTestUserHome(t, home)

	out := gatherCLIAgentUsage(map[string]detectedCLIAgent{
		"geminiBuild": {Detected: true, Name: "Gemini Build", Version: "1.1.27", Path: filepath.Join(home, "bin", "agy")},
	}, time.Now())
	if len(out) != 1 {
		t.Fatalf("expected one snapshot, got %d", len(out))
	}
	if out[0].CliAgentID != "geminiBuild" {
		t.Fatalf("snapshot keeps the catalog id: %q", out[0].CliAgentID)
	}
	if len(out[0].ModelDetails) != 5 || out[0].ModelsExhaustive == nil || !*out[0].ModelsExhaustive {
		t.Fatalf("discovery must run through the parser key: %#v", out[0])
	}
}
