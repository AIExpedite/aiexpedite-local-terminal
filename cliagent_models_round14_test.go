package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Codex round 14 on #147: Grok discovery is a floor outright — the account a
// session runs as is chosen per session (project `.grok/config.toml` found
// upward from the session's cwd, an external config path, per-model keys) and
// a device-level probe has no session cwd — and discovery inside a bounded
// gather gets its own slice of the deadline so one slow list cannot starve
// the providers behind it.

func TestDiscoverGrokModelsIsNeverExhaustive(t *testing.T) {
	home := t.TempDir()
	real := filepath.Join(home, ".grok")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", "")
	t.Setenv("GROK_CONFIG_PATH", "")
	prevCfg := shutdownConfig
	t.Cleanup(func() { shutdownConfig = prevCfg })
	shutdownConfig = nil
	prev := cliAgentModelProbeRunner
	t.Cleanup(func() { cliAgentModelProbeRunner = prev })
	// The strongest case for "complete": a conclusive signed-in list AND a
	// cache the list itself just refreshed, from the installed build.
	cliAgentModelProbeRunner = func(_ context.Context, _ string, env []string, _ ...string) (string, bool) {
		for _, entry := range env {
			if name, value, ok := strings.Cut(entry, "="); ok && name == "GROK_HOME" {
				_ = os.WriteFile(filepath.Join(value, "models_cache.json"), []byte(realGrokModelsCache), 0o644)
			}
		}
		return realGrokModelsLoggedOut, true
	}
	got, ok := discoverGrokModels(context.Background(), detectedCLIAgent{Detected: true, Path: "/x/grok", Version: "grok 1.0.13"}, home)
	if !ok || len(got.Models) < 2 || got.DefaultModel != "grok-4.6" {
		t.Fatalf("the list is still reported in full: ok=%v %#v", ok, got)
	}
	if got.Exhaustive {
		t.Fatal("Grok discovery must never veto: the session's cwd decides the account, and the probe has none")
	}
	// mergeGrokDiscovery itself can never raise the flag, whatever it is fed.
	merged := mergeGrokDiscovery(cliAgentModelDiscovery{Exhaustive: true, Models: []cliAgentModelDetail{{ID: "grok-4.6"}}}, true, grokModelsCacheFile{}, false, "")
	if merged.Exhaustive {
		t.Fatal("mergeGrokDiscovery must not pass an exhaustive flag through")
	}
}

func TestAttachCLIAgentModelDiscoveryBudgetsAProbeInsideABoundedGather(t *testing.T) {
	resetCLIAgentModelProbeCache()
	t.Cleanup(resetCLIAgentModelProbeCache)
	prev := cliAgentModelProbeRunner
	t.Cleanup(func() { cliAgentModelProbeRunner = prev })
	var seenDeadline time.Time
	var seenBounded bool
	cliAgentModelProbeRunner = func(ctx context.Context, _ string, _ []string, _ ...string) (string, bool) {
		seenDeadline, seenBounded = ctx.Deadline()
		return realAntigravityModels, true
	}
	detected := detectedCLIAgent{Detected: true, Path: "/bin/agy", Version: "1.1.27"}

	// The demand-driven refresh: one 10s deadline for every provider. A
	// probe gets at most cliAgentModelProbeGatherBudget of it, so `agy models`
	// on a slow network can never spend the budget the later providers need.
	gatherCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	before := time.Now()
	attachCLIAgentModelDiscovery(gatherCtx, "antigravity", detected, &cliAgentUsage{Provider: "antigravity"}, "", time.Now())
	if !seenBounded {
		t.Fatal("a probe under a bounded gather must carry a deadline")
	}
	if remaining := seenDeadline.Sub(before); remaining > cliAgentModelProbeGatherBudget+500*time.Millisecond || remaining > 5*time.Second {
		t.Fatalf("probe deadline %v after start, want at most the gather budget %v", remaining, cliAgentModelProbeGatherBudget)
	}

	// The periodic gather passes no deadline: the probe keeps its own full
	// timeout (applied later by runCLIAgentModelProbe), so here it sees none.
	resetCLIAgentModelProbeCache()
	seenBounded = true
	attachCLIAgentModelDiscovery(context.Background(), "antigravity", detected, &cliAgentUsage{Provider: "antigravity"}, "", time.Now())
	if seenBounded {
		t.Fatal("an unbounded gather must not impose the refresh budget on the probe")
	}

	// A gather that is ALREADY nearly out of time does not probe at all: what
	// is left is the reserve for the providers still to be polled (round 16),
	// and the budget is a cap, never an extension.
	resetCLIAgentModelProbeCache()
	probed := false
	cliAgentModelProbeRunner = func(ctx context.Context, _ string, _ []string, _ ...string) (string, bool) {
		probed = true
		return realAntigravityModels, true
	}
	shortCtx, cancelShort := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancelShort()
	usage := &cliAgentUsage{Provider: "antigravity"}
	attachCLIAgentModelDiscovery(shortCtx, "antigravity", detected, usage, "", time.Now())
	if probed {
		t.Fatalf("a gather with only %v left must not spend it on a model probe", 300*time.Millisecond)
	}
	if len(usage.ModelDetails) != 0 {
		t.Fatalf("no probe and a cold cache must report nothing, got %#v", usage.ModelDetails)
	}
}
