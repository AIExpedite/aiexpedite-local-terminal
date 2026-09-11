package main

import (
	"context"
	"testing"
	"time"
)

// Codex round 19 on #147: `claude auth status --json` ran on its own 3s clock
// from context.Background(), so after the usage request Claude's SECOND probe
// could still hold the gather past its shared deadline — the discovery reserve
// covers one utilization probe, not two. The probe now derives from the
// caller's context; the earlier deadline wins.

func TestClaudeAuthStatusProbeDerivesItsDeadlineFromTheCaller(t *testing.T) {
	prev := runClaudeAuthStatusCommand
	t.Cleanup(func() { runClaudeAuthStatusCommand = prev })
	var seen time.Time
	var bounded bool
	runClaudeAuthStatusCommand = func(ctx context.Context, _ string, _ []string) ([]byte, error) {
		seen, bounded = ctx.Deadline()
		return []byte(`{"loggedIn":true}`), nil
	}

	// No gather: the probe keeps its own cap.
	start := time.Now()
	if loggedIn, known := claudeAuthStatusProbe(context.Background(), "/bin/claude"); !known || !loggedIn {
		t.Fatalf("the probe must still read the status payload: loggedIn=%v known=%v", loggedIn, known)
	}
	if !bounded || seen.Sub(start) > machineInfoProbeTimeout+200*time.Millisecond {
		t.Fatalf("probe deadline %v after start, want the probe's own cap %v", seen.Sub(start), machineInfoProbeTimeout)
	}

	// A gather with less time than the cap: the probe ends with the gather.
	gather, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start = time.Now()
	claudeAuthStatusProbe(gather, "/bin/claude")
	if !bounded || seen.Sub(start) > 1200*time.Millisecond {
		t.Fatalf("probe deadline %v after start, must not outlive the gather's %v", seen.Sub(start), time.Second)
	}

	// An expired gather: inconclusive at once, never a verdict.
	expired, cancelExpired := context.WithCancel(context.Background())
	cancelExpired()
	runClaudeAuthStatusCommand = func(ctx context.Context, _ string, _ []string) ([]byte, error) {
		return nil, ctx.Err()
	}
	if _, known := claudeAuthStatusProbe(expired, "/bin/claude"); known {
		t.Fatal("a probe under an expired gather is inconclusive, not a login verdict")
	}
}
