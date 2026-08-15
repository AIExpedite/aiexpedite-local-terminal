// File: pubsub_draining_test.go
// Verifies the draining admission decision and the AGENT_DRAINING rejection
// shape the pubsub receive loop publishes for a refused work start.
package main

import "testing"

func TestDraining_RefusesStartAdmitsContinuation(t *testing.T) {
	resetDrainState(t)
	closeAdmission("attempt-x")
	t.Cleanup(func() { resetDrainState(t) })

	// A new session start is refused while draining.
	start := commandMsg{ID: "c1", Type: "session_start", SessionID: "s1"}
	if admitted, release := admitWork(start); admitted || release == nil {
		t.Fatal("session_start must be refused while draining")
	} else {
		release()
	}

	// Its continuation input is still delivered.
	input := commandMsg{ID: "c2", Type: "session_input", SessionID: "s1"}
	if admitted, _ := admitWork(input); !admitted {
		t.Fatal("session_input for an accepted session must be admitted while draining")
	}
}

func TestMakeRejectionResult_AgentDraining(t *testing.T) {
	// A refused session start comes back tagged with the family error type and
	// session id so the orchestrator can route it, carrying AGENT_DRAINING.
	cmd := commandMsg{
		ID:        "c1",
		Type:      "session_start",
		SessionID: "sess-1",
		Command:   "claude",
	}
	res := makeRejectionResult(cmd, "agent-1", "draining", "AGENT_DRAINING",
		"Device is finishing current work before updating; new work is temporarily unavailable. Please retry.")

	if res.Status != "draining" {
		t.Fatalf("Status = %q, want draining", res.Status)
	}
	if res.RejectionReason != "AGENT_DRAINING" {
		t.Fatalf("RejectionReason = %q, want AGENT_DRAINING", res.RejectionReason)
	}
	if res.Type != "session_error" {
		t.Fatalf("Type = %q, want session_error", res.Type)
	}
	if res.SessionID != "sess-1" {
		t.Fatalf("SessionID = %q, want sess-1", res.SessionID)
	}
}
