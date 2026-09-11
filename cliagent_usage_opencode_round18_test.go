package main

import (
	"context"
	"testing"
	"time"
)

// Codex round 18 on #147: the best-effort `auth list` probe was handed the
// whole remaining gather context, so a stall there until the parent expired
// had runProviderParseSafely discard OpenCode's already-conclusive `models`
// answer and report the providers behind it as canceled. The optional probe
// now gets only what the gather can spare beyond the reserve, or is skipped.

func TestOptionalOpenCodeProbeLeavesTheGatherItsReserve(t *testing.T) {
	// Unbounded caller: passes through, nothing to ration.
	if _, release, ok := optionalOpenCodeProbeContext(context.Background()); !ok {
		t.Fatal("an unbounded gather must run the optional probe on its own cap")
	} else {
		release()
	}

	// A fresh gather: the probe's own cap, no more.
	fresh, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	probeCtx, release, ok := optionalOpenCodeProbeContext(fresh)
	if !ok {
		t.Fatal("a fresh gather must afford the optional probe")
	}
	deadline, bounded := probeCtx.Deadline()
	if !bounded || deadline.Sub(start) > openCodeProbeTimeout+200*time.Millisecond {
		t.Fatalf("optional probe deadline %v after start, want at most its cap %v", deadline.Sub(start), openCodeProbeTimeout)
	}
	release()

	// A gather with the reserve plus a second left: only the second.
	tight, cancelTight := context.WithTimeout(context.Background(), openCodeOptionalProbeReserve+time.Second)
	defer cancelTight()
	start = time.Now()
	probeCtx, release, ok = optionalOpenCodeProbeContext(tight)
	if !ok {
		t.Fatal("a gather with a second to spare must afford a one-second probe")
	}
	deadline, _ = probeCtx.Deadline()
	if got := deadline.Sub(start); got > 1200*time.Millisecond {
		t.Fatalf("optional probe deadline %v after start, must leave the %v reserve", got, openCodeOptionalProbeReserve)
	}
	release()

	// Inside the reserve: skipped outright, so the conclusive models answer
	// that preceded it is returned before the parent can expire.
	inside, cancelInside := context.WithTimeout(context.Background(), openCodeOptionalProbeReserve-500*time.Millisecond)
	defer cancelInside()
	if _, _, ok := optionalOpenCodeProbeContext(inside); ok {
		t.Fatal("a gather inside its reserve must skip the optional probe")
	}
}
