package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Codex round 17 on #147: the OpenCode readiness probes (`models`, then
// `auth list`) ran on their own 3s clocks from context.Background(), so a
// gather that reached OpenCode with only its reserve left could still be held
// past its shared deadline and report the providers behind it as canceled.
// Both probes now derive from the gather context, and an answer the deadline
// cut is never cached.

func TestOpenCodeParserRunsUnderTheGatherContext(t *testing.T) {
	// The gather selects ParseContext through a type assertion that fails
	// silently; keep the parser on the interface.
	var parser cliAgentUsageParser = openCodeUsageParser{}
	if _, ok := parser.(cliAgentUsageContextParser); !ok {
		t.Fatal("openCodeUsageParser must implement ParseContext or the gather falls back to Parse's background context")
	}

	resetOpenCodeReadinessCache()
	t.Cleanup(resetOpenCodeReadinessCache)
	SetOpenCodeReadinessForceProbe(true)

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	executable := filepath.Join(t.TempDir(), "opencode-that-is-never-run")
	started := time.Now()
	usage, ok := openCodeUsageParser{}.ParseContext(expired, t.TempDir(), detectedCLIAgent{Detected: true, Path: executable, Version: "1.0.0"}, time.Now())
	if !ok || usage == nil {
		t.Fatal("an expired gather still yields the fail-open baseline entry")
	}
	if elapsed := time.Since(started); elapsed > openCodeProbeTimeout {
		t.Fatalf("the probes ran on their own clock for %v after the gather had expired", elapsed)
	}
	if usage.AuthState != openCodeAuthUnknown || usage.Authenticated != nil {
		t.Fatalf("a probe the deadline cut is inconclusive, got authState=%q authenticated=%v", usage.AuthState, usage.Authenticated)
	}

	// Not cached: one refresh running out of time must not pin the card to
	// "unknown" for the TTL, nor consume the user's forced re-probe.
	openCodeReadinessMu.Lock()
	_, cached := openCodeReadinessCache[executable]
	forced := openCodeForceProbe
	openCodeReadinessMu.Unlock()
	if cached {
		t.Fatal("an answer the gather deadline cut must not be cached")
	}
	if !forced {
		t.Fatal("the forced re-probe must survive a refresh that ran out of time")
	}
}

func TestRunOpenCodeProbeDerivesItsDeadlineFromTheCaller(t *testing.T) {
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if _, ok := runOpenCodeProbe(expired, filepath.Join(t.TempDir(), "opencode"), "models"); ok {
		t.Fatal("a probe under an expired context is inconclusive")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("an expired context must fail the probe at once, took %v", elapsed)
	}
}
