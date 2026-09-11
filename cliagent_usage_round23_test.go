package main

import (
	"context"
	"testing"
	"time"
)

// Codex round 23 on #147: antigravityUsageParser performs a live loopback quota
// request, but it implemented only the small Parse contract — so the gather
// neither bounded that request nor held any reserve back for it. A discovery
// probe ahead of Antigravity could leave the parent near expiry, Antigravity
// would then spend its own 2s window, and runProviderParseSafely would discard
// its result and report every provider behind it canceled.

func TestAntigravityReservesAndDerivesItsQuotaRequest(t *testing.T) {
	parser := cliAgentUsageParserIndex()["antigravity"]
	if parser == nil {
		t.Fatal("antigravity missing from the parser index")
	}
	if _, bounded := parser.(cliAgentUsageContextParser); !bounded {
		t.Fatal("the antigravity parser performs a live quota request and must satisfy " +
			"cliAgentUsageContextParser — runProviderParseSafely selects ParseContext through a " +
			"type assertion that fails silently")
	}
	if got := cliAgentUsageGatherReserve(parser); got != cliAgentModelDiscoveryGatherReserve {
		t.Fatalf("antigravity reserve = %v, want the one-probe default %v", got, cliAgentModelDiscoveryGatherReserve)
	}

	// An already-expired gather must not buy the quota request a fresh window.
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, ok := parser.(cliAgentUsageContextParser).ParseContext(expired, t.TempDir(), detectedCLIAgent{Name: "Antigravity"}, time.Now()); !ok {
		t.Fatal("the parser still reports what it read from local state")
	}
	if elapsed := time.Since(start); elapsed > antigravityQuotaTimeout {
		t.Fatalf("a canceled gather still spent %v in the quota request, want an immediate return", elapsed)
	}
}
