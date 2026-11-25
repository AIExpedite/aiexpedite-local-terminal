package main

// Shared isolation helpers for the Claude usage probe's process-wide gate.
//
// The gate (claudeUsageProbe), its retry rung and the offline / shutdown flags
// are package-level, and `go test ./...` runs every test of the package in one
// process. Work one test leaves running — a probe or settlement the reset could
// not drain, a direct run's exit path, a rung booked against a ladder cleanup
// already restored — changes that state after the next test has re-armed the
// gate, and the next test's post-run probe is then refused for spacing. These
// helpers turn such a leak into a named failure instead of a timed-out wait.
//
// Redaction: claudeProbeStateForTest prints instants, counters, probe codes and
// booleans only. It never prints a token, a fingerprint, a path or a body.

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// claudeGateLeak names the first condition that makes the gate unfit to arm a
// new test on, or "" when the gate is quiet. Flags come first: they are a
// previous test's cleanup that never ran, and no amount of waiting clears them.
func claudeGateLeak() string {
	if IsOffline() {
		return "the agent is still marked offline"
	}
	if IsShutdownInProgress() {
		return "a shutdown is still marked in progress"
	}
	claudeUsageProbe.mu.Lock()
	settling, inFlight, authClear := claudeUsageProbe.settling, claudeUsageProbe.inFlight, claudeUsageProbe.authClearRetrying
	claudeUsageProbe.mu.Unlock()
	switch {
	case settling > 0:
		return fmt.Sprintf("%d run-debt settlement(s) still running", settling)
	case inFlight:
		return "a probe still holds the single-flight latch"
	case authClear:
		return "the credential-wait auth-clear retry loop is still running"
	}
	if _, armed := claudeRunDebtRetryDue(); armed {
		return "a run-debt retry rung is still armed"
	}
	return ""
}

// claudeGateIdleLeak is claudeGateLeak without the rung: the in-test idle wait
// (claudeFreshnessWaitIdle) runs while a test legitimately has a rung booked.
func claudeGateIdleLeak() string {
	claudeUsageProbe.mu.Lock()
	defer claudeUsageProbe.mu.Unlock()
	switch {
	case claudeUsageProbe.settling > 0:
		return fmt.Sprintf("%d run-debt settlement(s) still running", claudeUsageProbe.settling)
	case claudeUsageProbe.inFlight:
		return "a probe still holds the single-flight latch"
	}
	return ""
}

// waitClaudeGate polls `leak` until it reports nothing or `within` passes, and
// returns the last condition still holding ("" when quiet).
func waitClaudeGate(within time.Duration, leak func() string) string {
	deadline := time.Now().Add(within)
	for {
		reason := leak()
		if reason == "" || !time.Now().Before(deadline) {
			return reason
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// requireClaudeGateQuiet waits up to claudeUsageProbeDrainTimeout for a quiet
// gate, and fails the CURRENT test naming the condition that still holds. It is
// the next test that notices a leak, so the message points back at the one
// before it.
func requireClaudeGateQuiet(t *testing.T) {
	t.Helper()
	if reason := waitClaudeGate(claudeUsageProbeDrainTimeout, claudeGateLeak); reason != "" {
		t.Fatalf("the Claude usage probe gate is not quiet: %s — a previous test's cleanup did not run "+
			"or did not drain [%s]", reason, claudeProbeStateForTest(os.Getenv("AIEXPEDITE_CLAUDE_RL_CACHE")))
	}
}

// requireClaudeResetDrained resets the gate and fails the test when the reset
// could not drain the work still running. Registered as a cleanup, that fails
// the test that LEAKED rather than the one that runs next.
func requireClaudeResetDrained(t *testing.T) {
	t.Helper()
	if !resetClaudeUsageProbeGate() {
		t.Errorf("resetClaudeUsageProbeGate could not drain a probe or settlement this test started; "+
			"the straggler was cancelled and its gate state left standing [%s]",
			claudeProbeStateForTest(os.Getenv("AIEXPEDITE_CLAUDE_RL_CACHE")))
	}
}

// claudeProbeStateForTest is the redacted diagnostic a failing freshness wait
// prints: the gate's throttle and debt, the armed rung, and the persisted
// schedule and observations of `cache`. Unix milliseconds, counters, probe
// codes and booleans only.
func claudeProbeStateForTest(cache string) string {
	ms := func(at time.Time) int64 {
		if at.IsZero() {
			return 0
		}
		return at.UnixMilli()
	}
	claudeUsageProbe.mu.Lock()
	gate := fmt.Sprintf("gate{armed=%v inFlight=%v settling=%d failures=%d lastAttemptMs=%d heldUntilMs=%d owedBaselineMs=%d authClearRetrying=%v}",
		claudeUsageProbe.armed, claudeUsageProbe.inFlight, claudeUsageProbe.settling, claudeUsageProbe.failures,
		ms(claudeUsageProbe.lastAttempt), ms(claudeUsageProbe.heldUntil), ms(claudeUsageProbe.owedBaseline),
		claudeUsageProbe.authClearRetrying)
	claudeUsageProbe.mu.Unlock()

	rung := "rung{armed=false}"
	if due, armed := claudeRunDebtRetryDue(); armed {
		rung = fmt.Sprintf("rung{armed=true dueMs=%d}", due.UnixMilli())
	}
	claudeUsageProbeLog.mu.Lock()
	lastCode := claudeUsageProbeLog.category
	claudeUsageProbeLog.mu.Unlock()

	parts := []string{fmt.Sprintf("nowMs=%d", time.Now().UnixMilli()), gate, rung, "lastLogged=" + lastCode}
	if cache == "" {
		return strings.Join(append(parts, "snapshot{unpinned}"), " ")
	}
	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok {
		return strings.Join(append(parts, "snapshot{missing}"), " ")
	}
	parts = append(parts, fmt.Sprintf(
		"snapshot{refreshOwedAtMs=%d refreshOwedAttempts=%d nextAttemptAtMs=%d attemptClaimedUntilMs=%d lastProbeObservedAtMs=%d heldUntilMs=%d}",
		snap.RefreshOwedAtMs, snap.RefreshOwedAttempts, snap.NextAttemptAtMs, snap.AttemptClaimedUntilMs,
		snap.LastProbeObservedAtMs, snap.HeldUntilMs))
	windows := make([]string, 0, len(snap.Buckets))
	for window := range snap.Buckets {
		windows = append(windows, window)
	}
	sort.Strings(windows)
	for _, window := range windows {
		b := snap.Buckets[window]
		parts = append(parts, fmt.Sprintf("bucket{%s observedAtMs=%d source=%s}", window, b.ObservedAtMs, b.Source))
	}
	return strings.Join(parts, " ")
}
