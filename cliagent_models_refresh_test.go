package main

import (
	"context"
	"testing"
	"time"
)

// refreshCLIAgentModelDiscovery asks the CLI whatever the cache holds, and
// stores the answer by the same rules a cache miss follows.
func TestRefreshCLIAgentModelDiscovery_BypassesTheTTLAndStores(t *testing.T) {
	resetCLIAgentModelProbeCache()
	t.Cleanup(resetCLIAgentModelProbeCache)
	calls := 0
	prev := cliAgentModelProbeRunner
	t.Cleanup(func() { cliAgentModelProbeRunner = prev })
	cliAgentModelProbeRunner = func(context.Context, string, []string, ...string) (string, bool) {
		calls++
		return realAntigravityModels, true
	}
	detected := detectedCLIAgent{Detected: true, Path: "/usr/local/bin/agy", Version: "1.2.3"}
	now := time.Now()

	if _, ok := cachedCLIAgentModelDiscovery(context.Background(), "antigravity", detected, "", now); !ok || calls != 1 {
		t.Fatalf("ok=%v calls=%d, want the cold cache filled by one probe", ok, calls)
	}
	// Warm: the cached path spawns nothing, the forced one always asks.
	cachedCLIAgentModelDiscovery(context.Background(), "antigravity", detected, "", now.Add(time.Minute))
	if calls != 1 {
		t.Fatalf("calls=%d, want the warm cache served", calls)
	}
	got, ok := refreshCLIAgentModelDiscovery(context.Background(), "antigravity", detected, "", now.Add(2*time.Minute))
	if !ok || len(got.Models) == 0 || calls != 2 {
		t.Fatalf("ok=%v models=%d calls=%d, want a fresh probe inside the TTL", ok, len(got.Models), calls)
	}
	// Stored under the refresh's own instant: the TTL now runs from it.
	cachedCLIAgentModelDiscovery(context.Background(), "antigravity", detected, "", now.Add(2*time.Minute+cliAgentModelProbeTTL-time.Second))
	if calls != 2 {
		t.Errorf("calls=%d, want the refreshed entry to restart the TTL", calls)
	}
}

// A probe that lost its deadline, and one that raced a forced reset, are
// returned to their caller but never stored.
func TestRefreshCLIAgentModelDiscovery_DoesNotStoreADeadlineMissOrAPreResetResult(t *testing.T) {
	resetCLIAgentModelProbeCache()
	t.Cleanup(resetCLIAgentModelProbeCache)
	prev := cliAgentModelProbeRunner
	t.Cleanup(func() { cliAgentModelProbeRunner = prev })
	detected := detectedCLIAgent{Detected: true, Path: "/usr/local/bin/agy", Version: "1.2.3"}
	now := time.Now()

	ctx, cancel := context.WithCancel(context.Background())
	cliAgentModelProbeRunner = func(context.Context, string, []string, ...string) (string, bool) {
		cancel()
		return "", false
	}
	if _, ok := refreshCLIAgentModelDiscovery(ctx, "antigravity", detected, "", now); ok {
		t.Fatal("a deadline miss reported a list")
	}
	if _, _, cached := lookupCLIAgentModelDiscoveryEntry("antigravity", detected, now); cached {
		t.Error("a deadline miss was cached")
	}

	cliAgentModelProbeRunner = func(context.Context, string, []string, ...string) (string, bool) {
		resetCLIAgentModelProbeCache()
		return realAntigravityModels, true
	}
	if got, ok := refreshCLIAgentModelDiscovery(context.Background(), "antigravity", detected, "", now); !ok || len(got.Models) == 0 {
		t.Fatalf("ok=%v, want the caller still handed its own answer", ok)
	}
	if _, _, cached := lookupCLIAgentModelDiscoveryEntry("antigravity", detected, now); cached {
		t.Error("a pre-reset answer was stored")
	}
}
