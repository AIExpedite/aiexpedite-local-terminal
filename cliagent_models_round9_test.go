package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Codex round 9 on #147: Grok discovery fails closed when the isolated home
// cannot be built, and OpenCode's legacy `models` list is bounded too.

func TestDiscoverGrokModelsFailsClosedWithoutAnIsolatedHome(t *testing.T) {
	home := t.TempDir()
	real := filepath.Join(home, ".grok")
	if err := os.MkdirAll(real, 0o755); err != nil {
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
	shutdownConfig = nil
	// os.MkdirTemp cannot create the isolated home when the temp root does
	// not exist — the same failure a managed session fails closed on.
	missing := filepath.Join(t.TempDir(), "no-such-temp-root")
	t.Setenv("TMPDIR", missing)
	t.Setenv("TMP", missing)
	t.Setenv("TEMP", missing)

	prev := cliAgentModelProbeRunner
	t.Cleanup(func() { cliAgentModelProbeRunner = prev })
	probed := false
	cliAgentModelProbeRunner = func(context.Context, string, []string, ...string) (string, bool) {
		probed = true
		return realGrokModelsLoggedOut, true
	}
	got, ok := discoverGrokModels(context.Background(), detectedCLIAgent{Detected: true, Path: "/x/grok", Version: "grok 1.0.13"}, home)
	if probed {
		t.Fatal("grok models must not run against the real home when isolation fails")
	}
	// The cache alone answers, and cache-only is never exhaustive.
	if !ok || got.Exhaustive || len(got.Models) == 0 {
		t.Fatalf("cache-only fallback expected: ok=%v %#v", ok, got)
	}
	// With no cache either, the result is inconclusive rather than a real-home list.
	_ = os.Remove(filepath.Join(real, "models_cache.json"))
	if _, ok := discoverGrokModels(context.Background(), detectedCLIAgent{Detected: true, Path: "/x/grok", Version: "grok 1.0.13"}, home); ok || probed {
		t.Fatalf("no isolation and no cache must be inconclusive (ok=%v probed=%v)", ok, probed)
	}
}

func TestAttachCLIAgentModelDiscoveryOpenCodeDropsAnIDPastTheLegacyBound(t *testing.T) {
	huge := "custom/" + strings.Repeat("m", cliUsageMaxLegacyModelIDLength)
	usage := &cliAgentUsage{Provider: "opencode", CollectedAt: "now", Models: []string{"opencode/big-pickle", huge, "ollama/qwen3-coder:30b"}}
	attachCLIAgentModelDiscovery(context.Background(), "opencode", detectedCLIAgent{Detected: true}, usage, "", time.Now())
	if len(usage.Models) != 2 || usage.Models[0] != "opencode/big-pickle" || usage.Models[1] != "ollama/qwen3-coder:30b" {
		t.Fatalf("the over-bound id must leave the legacy list too: %v", usage.Models)
	}
	if len(usage.ModelDetails) != 2 {
		t.Fatalf("details = %#v", usage.ModelDetails)
	}
	if usage.ModelsExhaustive == nil || *usage.ModelsExhaustive {
		t.Fatal("a dropped id makes the catalog non-exhaustive")
	}
	// And the whole provider still canonicalizes — the point of dropping it.
	if _, _, _, err := canonicalCLIUsageRefreshReceipt("r", 1, true, []cliAgentUsage{*usage}, nil); err != nil {
		t.Fatalf("must canonicalize: %v", err)
	}
}
