package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Codex round 4 on #147: the Grok list probe runs under the same credential
// surface as an ACP session, a Codex install without a models cache still
// reports its configured model, and TOML literal strings are read.

func TestGrokModelListEnvKeepsTheAPIKeyOnlyWhenOptedIn(t *testing.T) {
	env := []string{"PATH=/bin", "GROK_HOME=/custom/grok", "XAI_API_KEY=xai-secret", "GROK_CURSOR_RULES_ENABLED=1"}
	has := func(list []string, prefix string) bool {
		for _, entry := range list {
			if strings.HasPrefix(entry, prefix) {
				return true
			}
		}
		return false
	}
	prev := shutdownConfig
	t.Cleanup(func() { shutdownConfig = prev })

	// Default (no config, or opt-in off): the key is stripped, as for a
	// session without AllowAPIKeyFallback; GROK_HOME always survives.
	shutdownConfig = nil
	out := sanitizeGrokModelListEnv(env)
	if has(out, "XAI_API_KEY=") {
		t.Fatal("XAI_API_KEY must be stripped without the opt-in")
	}
	if !has(out, "GROK_HOME=/custom/grok") {
		t.Fatal("GROK_HOME must survive the smoke sanitizer")
	}
	shutdownConfig = &Config{EnableGrokAPIKeyFallback: false}
	if has(sanitizeGrokModelListEnv(env), "XAI_API_KEY=") {
		t.Fatal("opt-in false must strip the key")
	}

	// Opted in: the list runs with the same key the ACP session would.
	shutdownConfig = &Config{EnableGrokAPIKeyFallback: true}
	out = sanitizeGrokModelListEnv(env)
	if !has(out, "XAI_API_KEY=xai-secret") {
		t.Fatal("XAI_API_KEY must be kept when the user opted into API-key auth")
	}
	if !has(out, "GROK_HOME=/custom/grok") {
		t.Fatal("GROK_HOME must still survive")
	}
	// The rest of the smoke sanitizer's stripping stands.
	if has(out, "GROK_CURSOR_RULES_ENABLED=1") {
		t.Fatal("other GROK_* vars are still neutralised")
	}
}

func TestDiscoverCodexModelsReportsTheConfiguredModelWithoutACache(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", "")
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("model = \"gpt-6-astra\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No models_cache.json at all: a fresh install or a cleared cache.
	got, ok := discoverCodexModels(home, "codex-cli 0.153.4")
	if !ok {
		t.Fatal("a configured model is a conclusive one-model floor")
	}
	if got.Exhaustive || got.DefaultModel != "gpt-6-astra" || len(got.Models) != 1 || got.Models[0].ID != "gpt-6-astra" {
		t.Fatalf("got %#v", got)
	}
	if got.Models[0].Efforts != nil || got.Models[0].NoEffort {
		t.Fatal("the configured model's scale is unknown, not empty")
	}
	// An unreadable cache is the same case.
	if err := os.WriteFile(filepath.Join(dir, "models_cache.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok = discoverCodexModels(home, "codex-cli 0.153.4")
	if !ok || got.Exhaustive || got.DefaultModel != "gpt-6-astra" {
		t.Fatalf("unreadable cache: ok=%v %#v", ok, got)
	}
	// A readable cache with no listable model still yields the floor.
	if err := os.WriteFile(filepath.Join(dir, "models_cache.json"), []byte(`{"client_version":"0.153.4","models":[{"slug":"gpt-hidden","visibility":"hide"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok = discoverCodexModels(home, "codex-cli 0.153.4")
	if !ok || got.Exhaustive || len(got.Models) != 1 || got.Models[0].ID != "gpt-6-astra" {
		t.Fatalf("empty listable cache: ok=%v %#v", ok, got)
	}
	// And with neither a cache nor a configured model, still inconclusive.
	if _, ok := discoverCodexModels(t.TempDir(), "codex-cli 0.153.4"); ok {
		t.Fatal("nothing configured and no cache is inconclusive")
	}
}

func TestReadCodexConfiguredModelAcceptsTOMLLiteralStrings(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", "")
	for _, tc := range []struct{ toml, want string }{
		{"model = 'gpt-6-astra'\n", "gpt-6-astra"},
		{"model='gpt-5.6-sol'\n[profiles.x]\nmodel = \"other\"\n", "gpt-5.6-sol"},
		{"model = \"gpt-basic\"\n", "gpt-basic"},
		{"model = 'with \"quotes\" inside'\n", "with \"quotes\" inside"},
		{"model = gpt-bare\n", ""},
	} {
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(tc.toml), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := readCodexConfiguredModel(home); got != tc.want {
			t.Fatalf("%q → %q, want %q", tc.toml, got, tc.want)
		}
	}
}
