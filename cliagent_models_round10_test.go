package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Codex round 10 on #147: the isolated list home is seeded with the ACP
// default runtime model so a per-model persisted key rides along under the
// opt-in, a key under another model's table makes the catalog a floor, and
// OpenCode's repeated ids collapse before they become detail rows.

func TestDiscoverGrokModelsSeedsTheDefaultRuntimeModelKeyAndFloorsOnOthers(t *testing.T) {
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
	prev := cliAgentModelProbeRunner
	t.Cleanup(func() { cliAgentModelProbeRunner = prev })
	var seenConfig string
	cliAgentModelProbeRunner = func(_ context.Context, _ string, env []string, _ ...string) (string, bool) {
		for _, entry := range env {
			if name, value, ok := strings.Cut(entry, "="); ok && name == "GROK_HOME" {
				data, _ := os.ReadFile(filepath.Join(value, "config.toml"))
				seenConfig = string(data)
			}
		}
		return realGrokModelsLoggedOut, true
	}
	detected := detectedCLIAgent{Detected: true, Path: "/x/grok", Version: "grok 1.0.13"}

	// The key lives ONLY under the default runtime model's table: with the
	// opt-in it must reach the child (a session for that model authenticates
	// with it), and the list is still the whole catalog for that model.
	perDefault := "[model." + grokACPDefaultModel + "]\napi_key = \"xai-per-model\"\n"
	if err := os.WriteFile(filepath.Join(real, "config.toml"), []byte(perDefault), 0o600); err != nil {
		t.Fatal(err)
	}
	shutdownConfig = &Config{EnableGrokAPIKeyFallback: true}
	got, ok := discoverGrokModels(context.Background(), detected, home)
	if !ok {
		t.Fatal("expected a conclusive discovery")
	}
	if !strings.Contains(seenConfig, "api_key = \"xai-per-model\"") {
		t.Fatalf("the default runtime model's key must be carried into the isolated home:\n%s", seenConfig)
	}
	// It IS a per-model key, so one list run cannot vouch for every model.
	if got.Exhaustive {
		t.Fatal("a per-model key makes the catalog a floor")
	}

	// Without the opt-in the key never reaches the child, whatever table it is in.
	shutdownConfig = nil
	seenConfig = ""
	if _, ok := discoverGrokModels(context.Background(), detected, home); !ok {
		t.Fatal("expected a conclusive discovery")
	}
	if strings.Contains(seenConfig, "api_key") {
		t.Fatalf("no key without the opt-in:\n%s", seenConfig)
	}

	// A plain `[model] api_key` (not per-model) keeps the list's own verdict.
	if err := os.WriteFile(filepath.Join(real, "config.toml"), []byte("[model]\napi_key = \"xai-plain\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	shutdownConfig = &Config{EnableGrokAPIKeyFallback: true}
	plain, ok := discoverGrokModels(context.Background(), detected, home)
	if !ok || !plain.Exhaustive {
		t.Fatalf("a plain key reproduces the session's auth, so the list stays exhaustive: ok=%v %#v", ok, plain)
	}
}

func TestGrokConfigHasPerModelAPIKey(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		body string
		want bool
	}{
		{"[model]\napi_key = \"k\"\n", false},
		{"[model.grok-build]\napi_key = \"k\"\n", true},
		{"[model.\"grok.4\"]\n  api_key = \"k\"\n", true},
		{"[model.grok-build]\nname = \"x\"\n\n[cli]\napi_key = \"not a model\"\n", false},
		{"[cli]\ninstaller = \"internal\"\n", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := grokConfigHasPerModelAPIKey(write(tc.body)); got != tc.want {
			t.Fatalf("%q → %v, want %v", tc.body, got, tc.want)
		}
	}
	if grokConfigHasPerModelAPIKey(filepath.Join(dir, "missing.toml")) {
		t.Fatal("a missing file has no per-model key")
	}
}

func TestAttachCLIAgentModelDiscoveryOpenCodeCollapsesRepeatedIDs(t *testing.T) {
	usage := &cliAgentUsage{
		Provider: "opencode", CollectedAt: "now",
		Models: []string{"opencode/big-pickle", "ollama/qwen3-coder:30b", "opencode/big-pickle", "ollama/qwen3-coder:30b"},
	}
	attachCLIAgentModelDiscovery(context.Background(), "opencode", detectedCLIAgent{Detected: true}, usage, "", time.Now())
	if len(usage.Models) != 2 || len(usage.ModelDetails) != 2 {
		t.Fatalf("repeats must collapse in both lists: %v / %#v", usage.Models, usage.ModelDetails)
	}
	// A repeat is not a lost model: the catalog is still the whole list.
	if usage.ModelsExhaustive == nil || !*usage.ModelsExhaustive {
		t.Fatal("deduplication alone must not make the catalog non-exhaustive")
	}
	if _, _, _, err := canonicalCLIUsageRefreshReceipt("r", 1, true, []cliAgentUsage{*usage}, nil); err != nil {
		t.Fatalf("must canonicalize: %v", err)
	}
}
