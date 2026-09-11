package main

import (
	"context"
	"testing"
	"time"
)

// Codex round 20 on #147: a discovery probe held back one probe's worth for
// the providers behind it, but several utilization parsers may still follow.
// The reserve is now summed over the parsers actually left in the run, and
// only the ones that perform bounded I/O count.

func TestGatherReservesSumOverTheParsersStillToRun(t *testing.T) {
	// The default catalog order with every CLI installed: antigravity and grok
	// read local state; claude, codex and opencode perform bounded I/O.
	parsers := cliAgentUsageParserIndex()
	run := []cliAgentUsageParser{
		parsers["antigravity"], parsers["claudeCode"], parsers["codex"], parsers["opencode"], parsers["grok"],
	}
	for i, parser := range run {
		if parser == nil {
			t.Fatalf("parser %d missing from the index", i)
		}
	}
	probe := cliAgentModelDiscoveryGatherReserve
	want := []time.Duration{3 * probe, 2 * probe, probe, 0, 0}
	got := cliAgentUsageGatherReservesAfter(run)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reserve after position %d = %v, want %v (all: %v)", i, got[i], want[i], got)
		}
	}
	if cliAgentUsageGatherReserve(nil) != 0 {
		t.Fatal("a provider without a parser reserves nothing")
	}
	if len(cliAgentUsageGatherReservesAfter(nil)) != 0 {
		t.Fatal("an empty run has no reserves")
	}
}

func TestDiscoveryProbeLeavesTheWholeSequenceItsReserve(t *testing.T) {
	// Antigravity first in a 10s refresh with three I/O parsers behind it:
	// nine seconds are theirs, so its probe gets about one.
	parent, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	budget := newCLIAgentDiscoveryBudget(parent)
	start := time.Now()
	probeCtx, release, ok := budget.probeContext(parent, 3*cliAgentModelDiscoveryGatherReserve)
	if !ok {
		t.Fatal("a second to spare must still afford a short probe")
	}
	deadline, _ := probeCtx.Deadline()
	if got := deadline.Sub(start); got > 1200*time.Millisecond {
		t.Fatalf("probe deadline %v after start, must leave %v to the three parsers behind it", got, 3*cliAgentModelDiscoveryGatherReserve)
	}
	release()

	// With four I/O parsers behind it nothing is affordable: cache only.
	if _, _, ok := budget.probeContext(parent, 4*cliAgentModelDiscoveryGatherReserve); ok {
		t.Fatal("a probe must not start when the parsers behind it need the whole parent")
	}

	// The last provider has nobody behind it and gets the per-probe cap.
	start = time.Now()
	probeCtx, release, ok = budget.probeContext(parent, 0)
	if !ok {
		t.Fatal("the last provider must afford its probe")
	}
	deadline, _ = probeCtx.Deadline()
	if got := deadline.Sub(start); got > cliAgentModelProbeGatherBudget+200*time.Millisecond {
		t.Fatalf("last probe deadline %v after start, want the per-probe cap %v", got, cliAgentModelProbeGatherBudget)
	}
	release()
}
