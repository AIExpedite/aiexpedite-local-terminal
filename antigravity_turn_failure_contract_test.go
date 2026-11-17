package main

import "testing"

// Contract pin. The Go agent cannot import shared-constants, so these literals
// are its copy of ANTIGRAVITY_TURN_FAILURE_CATEGORIES (shared-constants
// src/constants/antigravity.js), read by terminal-service (failure records),
// shared-resources (parseAntigravityChunk) and voice-service (spoken line). A
// rename on either side is an agent flag day: change both or neither.
func TestAntigravityTurnFailureCategoryContractPins(t *testing.T) {
	pins := [][2]string{
		{antigravityFailureSignedOut, "signed_out"},
		{antigravityFailureEntitlement, "entitlement"},
		{antigravityFailureQuota, "quota"},
		{antigravityFailureInvalidLaunchOption, "invalid_launch_option"},
		{antigravityFailureCwdProject, "cwd_project"},
		{antigravityFailureInteraction, "interaction_required"},
		{antigravityFailureStalled, "stalled"},
		{antigravityFailureCLIDefect, "cli_defect"},
		{antigravityFailureUnknown, "unknown"},
	}
	for _, p := range pins {
		if p[0] != p[1] {
			t.Errorf("category id %q drifted from shared-constants %q", p[0], p[1])
		}
	}
	if len(antigravityFailureCategoryIDs) != len(pins) || len(antigravityFailureRows) != len(pins) {
		t.Fatalf("ids=%d rows=%d, want %d", len(antigravityFailureCategoryIDs), len(antigravityFailureRows), len(pins))
	}
	for i, p := range pins {
		if antigravityFailureCategoryIDs[i] != p[1] {
			t.Errorf("table order [%d] = %q, want %q", i, antigravityFailureCategoryIDs[i], p[1])
		}
	}
	// shared-constants ANTIGRAVITY_TURN_SHUTTING_DOWN_TEXT: voice speaks it as is.
	if antigravityShuttingDownText != "Antigravity's previous run on this computer is still shutting down. Send the message again in a moment, or restart Antigravity on that computer." {
		t.Fatalf("shutting-down text drifted: %q", antigravityShuttingDownText)
	}
	// The quota wrapper the cloud anchors on (shared-constants providerOutage
	// `^\s*\[Antigravity turn failed:`).
	if antigravityQuotaWrapperPrefix != "[Antigravity turn failed: " {
		t.Fatalf("quota wrapper drifted: %q", antigravityQuotaWrapperPrefix)
	}
}
