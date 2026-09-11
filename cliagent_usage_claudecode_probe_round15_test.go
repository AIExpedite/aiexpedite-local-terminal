package main

import (
	"testing"
	"time"
)

// Codex round 15 on #147: the session trigger records a run's debt once,
// synchronously; the goroutine it spawns only PAYS that debt and never records
// again, so a gather that settled the baseline in the gap cannot have its
// settlement undone (which would schedule a trailing OAuth request for a run
// already covered).

func TestClaudeUsageProbePayRecordedRunNeverResurrectsASettledDebt(t *testing.T) {
	resetClaudeUsageProbeGate() // unarmed: eligibility refuses, nothing probes
	t.Cleanup(resetClaudeUsageProbeGate)

	completed := time.Now().Add(-time.Second)
	// The trigger's synchronous record, then a gather observes at/after the
	// baseline and settles it before the goroutine gets to run.
	claudeUsageProbe.recordOwed(completed)
	if claudeUsageProbe.owedObservation().IsZero() {
		t.Fatal("the synchronous record must leave a debt on the gate")
	}
	claudeUsageProbe.settleOwed(completed)
	if !claudeUsageProbe.owedObservation().IsZero() {
		t.Fatal("settlement must clear the debt")
	}

	// The goroutine body: settle-only, so the paid debt stays paid.
	claudeUsageProbePayRecordedRun(completed)
	if owed := claudeUsageProbe.owedObservation(); !owed.IsZero() {
		t.Fatalf("the settlement half resurrected a paid debt: %s", owed.UTC().Format(time.RFC3339Nano))
	}

	// The direct entry point still records first, for its own callers.
	claudeUsageProbeAfterRun(completed)
	if claudeUsageProbe.owedObservation().IsZero() {
		t.Fatal("claudeUsageProbeAfterRun must still record the debt it is asked to pay")
	}
}
