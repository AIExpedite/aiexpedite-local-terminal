package main

import (
	"context"
	"testing"
	"time"
)

// Codex round 16 on #147: the per-probe cap alone still granted EVERY provider
// its own two seconds, and with all the CLIs installed those slices plus the
// utilization probes between them exceed the refresh's shared deadline. One
// discovery budget is now shared across the gather, and a probe never eats the
// reserve the providers behind it need.

func TestCLIAgentDiscoveryBudgetIsSharedAcrossProbes(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	budget := newCLIAgentDiscoveryBudget(parent)
	if budget == nil {
		t.Fatal("a bounded gather must get a discovery budget")
	}
	if newCLIAgentDiscoveryBudget(context.Background()) != nil {
		t.Fatal("an unbounded gather must not ration its probes")
	}

	// First probe: the per-probe cap, since both the budget and the parent
	// (10s less the reserve) have more than that.
	start := time.Now()
	probeCtx, release, ok := budget.probeContext(parent)
	if !ok {
		t.Fatal("a fresh budget under a fresh gather must afford a probe")
	}
	deadline, bounded := probeCtx.Deadline()
	if !bounded || deadline.Sub(start) > cliAgentModelProbeGatherBudget+200*time.Millisecond {
		t.Fatalf("first probe deadline %v after start, want the per-probe cap %v", deadline.Sub(start), cliAgentModelProbeGatherBudget)
	}
	release()

	// The budget charges what a probe SPENT, not what it was handed: a fast
	// answer leaves nearly the whole budget for the providers behind it.
	budget.mu.Lock()
	left := budget.remaining
	budget.mu.Unlock()
	if left < cliAgentModelDiscoveryGatherBudget-time.Second {
		t.Fatalf("an instant probe was charged %v of the budget", cliAgentModelDiscoveryGatherBudget-left)
	}

	// With less than a per-probe cap left, the next probe gets the remainder…
	budget.mu.Lock()
	budget.remaining = 500 * time.Millisecond
	budget.mu.Unlock()
	start = time.Now()
	probeCtx, release, ok = budget.probeContext(parent)
	if !ok {
		t.Fatal("a partly spent budget must still afford a shorter probe")
	}
	deadline, _ = probeCtx.Deadline()
	if got := deadline.Sub(start); got > 700*time.Millisecond {
		t.Fatalf("probe deadline %v after start, want the budget's remaining %v", got, 500*time.Millisecond)
	}
	release()

	// …and once it is spent, no probe at all: the gather answers from cache.
	budget.mu.Lock()
	budget.remaining = 0
	budget.mu.Unlock()
	if _, _, ok := budget.probeContext(parent); ok {
		t.Fatal("a spent budget must refuse the probe")
	}
}

func TestCLIAgentDiscoveryBudgetLeavesTheReserveToLaterProviders(t *testing.T) {
	// A parent with a little more than the reserve left affords only the
	// difference; one with the reserve or less affords nothing.
	parent, cancel := context.WithTimeout(context.Background(), cliAgentModelDiscoveryGatherReserve+time.Second)
	defer cancel()
	budget := newCLIAgentDiscoveryBudget(parent)
	start := time.Now()
	probeCtx, release, ok := budget.probeContext(parent)
	if !ok {
		t.Fatal("a gather with the reserve plus a second left must afford a one-second probe")
	}
	deadline, _ := probeCtx.Deadline()
	if got := deadline.Sub(start); got > 1200*time.Millisecond {
		t.Fatalf("probe deadline %v after start, must leave the %v reserve to the later providers", got, cliAgentModelDiscoveryGatherReserve)
	}
	release()

	tight, cancelTight := context.WithTimeout(context.Background(), cliAgentModelDiscoveryGatherReserve-500*time.Millisecond)
	defer cancelTight()
	if _, _, ok := newCLIAgentDiscoveryBudget(tight).probeContext(tight); ok {
		t.Fatal("a gather inside its reserve must not probe")
	}
}

func TestAttachCLIAgentModelDiscoveryServesTheCacheWhenItCannotProbe(t *testing.T) {
	resetCLIAgentModelProbeCache()
	t.Cleanup(resetCLIAgentModelProbeCache)
	prev := cliAgentModelProbeRunner
	t.Cleanup(func() { cliAgentModelProbeRunner = prev })
	probes := 0
	cliAgentModelProbeRunner = func(_ context.Context, _ string, _ []string, _ ...string) (string, bool) {
		probes++
		return realAntigravityModels, true
	}
	detected := detectedCLIAgent{Detected: true, Path: "/bin/agy", Version: "1.1.27"}
	now := time.Now()

	// Warm the cache from a gather that could afford the probe.
	first := &cliAgentUsage{Provider: "antigravity"}
	attachCLIAgentModelDiscovery(context.Background(), "antigravity", detected, first, "", now)
	if probes != 1 || len(first.ModelDetails) == 0 {
		t.Fatalf("the warming gather must probe once and report models: probes=%d details=%d", probes, len(first.ModelDetails))
	}

	// A spent budget cannot probe, but the cached list still reaches the
	// receipt — the refresh loses nothing it already knew.
	spent := &cliAgentDiscoveryBudget{}
	second := &cliAgentUsage{Provider: "antigravity"}
	attachCLIAgentModelDiscoveryBudgeted(context.Background(), spent, "antigravity", detected, second, "", now)
	if probes != 1 {
		t.Fatalf("a spent budget must not probe, ran %d probes", probes)
	}
	if len(second.ModelDetails) != len(first.ModelDetails) {
		t.Fatalf("the cached list must still be served: got %d details, want %d", len(second.ModelDetails), len(first.ModelDetails))
	}

	// Two providers under ONE spent budget: neither probes, so the gather's
	// discovery as a whole — not one probe — is what the budget bounds.
	third := &cliAgentUsage{Provider: "claudecode"}
	attachCLIAgentModelDiscoveryBudgeted(context.Background(), spent, "claudecode", detectedCLIAgent{Detected: true, Path: "/bin/claude", Version: "2.0.0"}, third, "", now)
	if probes != 1 || len(third.ModelDetails) != 0 {
		t.Fatalf("a second provider on the same spent budget must not probe either: probes=%d details=%d", probes, len(third.ModelDetails))
	}
}
