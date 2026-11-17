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
	// Every id has exactly one row (next step + same-session column).
	if len(antigravityFailureRows) != len(pins) {
		t.Fatalf("rows=%d, want %d", len(antigravityFailureRows), len(pins))
	}
	for _, p := range pins {
		if _, ok := antigravityFailureRows[p[1]]; !ok {
			t.Errorf("no row for %q", p[1])
		}
	}
	// shared-constants ANTIGRAVITY_TURN_FAILURE_NEXT_STEPS: voice speaks the
	// same next step the card shows.
	nextSteps := map[string]string{
		"signed_out":            "Sign in to Antigravity on that computer, then send the message again.",
		"entitlement":           "Fix the Antigravity account's plan and send the message again, or start a new session with another model.",
		"quota":                 "Wait until the quota resets and send the message again, or start a new session with another model.",
		"invalid_launch_option": "Start a new session with another model or effort.",
		"cwd_project":           "Start a new session in a project folder Antigravity can open.",
		"interaction_required":  "Rephrase the task so it does not need that, then send it again.",
		"stalled":               "Send the message again; if it repeats, restart Antigravity on that computer.",
		"cli_defect":            "Update Antigravity on that computer, then send the message again.",
		"unknown":               "Send the message again; if it repeats, check Antigravity on that computer.",
	}
	for cat, want := range nextSteps {
		if got := antigravityFailureRows[cat].nextStep; got != want {
			t.Errorf("next step for %s drifted: %q, want %q", cat, got, want)
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
