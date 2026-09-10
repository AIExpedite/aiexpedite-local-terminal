package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Codex round 3 on #147: the generic (non-OpenCode) branch must not claim an
// exhaustive catalog once bounding dropped a row, and a default model the
// receipt cannot carry must not be copied into the snapshot.

func TestAttachCLIAgentModelDiscoveryBoundedCatalogIsNotExhaustive(t *testing.T) {
	resetCLIAgentModelProbeCache()
	t.Cleanup(resetCLIAgentModelProbeCache)
	prev := cliAgentModelProbeRunner
	t.Cleanup(func() { cliAgentModelProbeRunner = prev })
	detected := detectedCLIAgent{Detected: true, Path: "/bin/agy", Version: "1.1.27"}

	// One row past the cap: the CLI reported it, the receipt cannot carry it.
	var over strings.Builder
	for i := 0; i < cliUsageMaxModelsPerProvider+1; i++ {
		over.WriteString("family-" + strings.Repeat("a", i%9) + string(rune('a'+i%26)) + strings.Repeat("z", i/26) + "\tLabel\n")
	}
	cliAgentModelProbeRunner = func(context.Context, string, []string, ...string) (string, bool) { return over.String(), true }
	capped := &cliAgentUsage{Provider: "antigravity"}
	attachCLIAgentModelDiscovery(context.Background(), "antigravity", detected, capped, "", time.Now())
	if len(capped.ModelDetails) != cliUsageMaxModelsPerProvider || len(capped.Models) != cliUsageMaxModelsPerProvider {
		t.Fatalf("details=%d models=%d", len(capped.ModelDetails), len(capped.Models))
	}
	if capped.ModelsExhaustive == nil || *capped.ModelsExhaustive {
		t.Fatal("a catalog cut at the cap is not exhaustive")
	}

	// One id over the detail bound: dropped, and the rest are not the whole list.
	resetCLIAgentModelProbeCache()
	long := strings.Repeat("l", cliUsageMaxModelDetailIDLength+1)
	cliAgentModelProbeRunner = func(context.Context, string, []string, ...string) (string, bool) {
		return "gemini-3.8-flash-high\tGemini 3.8 Flash (High)\n" + long + "\tToo long\n", true
	}
	dropped := &cliAgentUsage{Provider: "antigravity"}
	attachCLIAgentModelDiscovery(context.Background(), "antigravity", detected, dropped, "", time.Now())
	if len(dropped.ModelDetails) != 1 || dropped.ModelDetails[0].ID != "gemini-3.8-flash" {
		t.Fatalf("details = %#v", dropped.ModelDetails)
	}
	if dropped.ModelsExhaustive == nil || *dropped.ModelsExhaustive {
		t.Fatal("a catalog with a dropped id is not exhaustive")
	}
	if _, _, _, err := canonicalCLIUsageRefreshReceipt("r", 1, true, []cliAgentUsage{*dropped}, nil); err != nil {
		t.Fatalf("must canonicalize: %v", err)
	}

	// Untouched by bounding: the CLI's own verdict stands.
	resetCLIAgentModelProbeCache()
	cliAgentModelProbeRunner = func(context.Context, string, []string, ...string) (string, bool) { return realAntigravityModels, true }
	clean := &cliAgentUsage{Provider: "antigravity"}
	attachCLIAgentModelDiscovery(context.Background(), "antigravity", detected, clean, "", time.Now())
	if clean.ModelsExhaustive == nil || !*clean.ModelsExhaustive {
		t.Fatal("an unbounded exhaustive catalog stays exhaustive")
	}
}

func TestAttachCLIAgentModelDiscoveryLeavesAnUnboundableDefaultUnset(t *testing.T) {
	resetCLIAgentModelProbeCache()
	t.Cleanup(resetCLIAgentModelProbeCache)
	prev := cliAgentModelProbeRunner
	t.Cleanup(func() { cliAgentModelProbeRunner = prev })
	long := strings.Repeat("d", cliUsageMaxModelDetailIDLength+1)
	// Grok marks its default on the list line; here the default's id is past
	// the bound the receipt puts on `model`.
	cliAgentModelProbeRunner = func(context.Context, string, []string, ...string) (string, bool) {
		return "Available models:\n  * " + long + " (default)\n  - grok-4.5\n", true
	}
	usage := &cliAgentUsage{Provider: "grok"}
	attachCLIAgentModelDiscovery(context.Background(), "grok", detectedCLIAgent{Detected: true, Path: "/bin/grok", Version: "grok 1.0.13"}, usage, t.TempDir(), time.Now())
	if usage.Model != "" {
		t.Fatalf("an over-long default must not be copied: %q", usage.Model)
	}
	if len(usage.ModelDetails) != 1 || usage.ModelDetails[0].ID != "grok-4.5" || len(usage.Models) != 1 {
		t.Fatalf("snapshot = %#v", usage)
	}
	if _, _, _, err := canonicalCLIUsageRefreshReceipt("r", 1, true, []cliAgentUsage{*usage}, nil); err != nil {
		t.Fatalf("must canonicalize: %v", err)
	}
	// A bounded default is still copied, as before.
	resetCLIAgentModelProbeCache()
	cliAgentModelProbeRunner = func(context.Context, string, []string, ...string) (string, bool) {
		return realGrokModelsLoggedOut, true
	}
	plain := &cliAgentUsage{Provider: "grok"}
	attachCLIAgentModelDiscovery(context.Background(), "grok", detectedCLIAgent{Detected: true, Path: "/bin/grok", Version: "grok 1.0.13"}, plain, t.TempDir(), time.Now())
	if plain.Model != "grok-4.6" {
		t.Fatalf("default = %q", plain.Model)
	}
}
