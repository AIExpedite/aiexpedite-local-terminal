package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestB5ModelDiscoveryHarness runs the REAL probes against the CLIs installed
// on this machine and writes the resulting `cliAgents[]` snapshot to the path in
// AIX_B5_HARNESS_OUT. It is the Ship B5 verification aid, skipped otherwise:
//
//	AIX_B5_HARNESS_OUT=/tmp/cliAgents.json go test -run TestB5ModelDiscoveryHarness -v
func TestB5ModelDiscoveryHarness(t *testing.T) {
	out := os.Getenv("AIX_B5_HARNESS_OUT")
	if out == "" {
		t.Skip("set AIX_B5_HARNESS_OUT to run the real-binary discovery harness")
	}
	// TestMain sandboxes HOME; the harness wants the real one, where the CLIs
	// keep their logins and caches.
	useRealUserProfile(t)
	resetCLIAgentModelProbeCache()
	detected := gatherCLIAgents()
	usage := gatherCLIAgentUsage(detected, time.Now())
	payload, err := json.MarshalIndent(map[string]any{"detectedCliAgents": detected, "cliAgents": usage}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, agent := range usage {
		t.Logf("%s: %d models, exhaustive=%v", agent.Provider, len(agent.ModelDetails), agent.ModelsExhaustive != nil && *agent.ModelsExhaustive)
		for _, model := range agent.ModelDetails {
			t.Logf("  %s efforts=%v default=%q noEffort=%v", model.ID, model.Efforts, model.DefaultEffort, model.NoEffort)
		}
	}
}
