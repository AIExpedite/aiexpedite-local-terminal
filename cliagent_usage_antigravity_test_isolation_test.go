package main

// Shared isolation helpers for Antigravity usage's process-wide state.
//
// The run settle, the debt worker, its retry rung, the discovery tick and the
// capture poller are package-level, and `go test ./...` runs every test of the
// package in one process. Each of them resolves the freshness file, the quota
// cache and the Code Assist / userinfo URLs from the environment at the moment
// it writes or calls, so a straggler that outlives its test lands in the NEXT
// test's t.Setenv paths and hits that test's loopback quota server. These
// helpers turn such a leak into a named failure in the test that leaked (or,
// failing that, the one that runs next) instead of a miscounted read later on.
// Same shape as cliagent_usage_claudecode_test_isolation_test.go.
//
// Redaction: antigravityStateForTest prints Unix milliseconds, counters,
// closed-set outcome codes and booleans only. It never prints a token, an
// account, a fingerprint, an email, a path, a URL or a reply body.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// antigravityStateQuietBound is how long requireAntigravityStateQuiet waits
// for a condition to clear before reporting it: the same 30 s bound
// antigravityUsageRefreshWaitIdle gives in-flight writes. A test that induces a
// leak on purpose shortens it.
var antigravityStateQuietBound = 30 * time.Second

// antigravityStateLeak names the first condition that makes the process unfit
// to arm a new Antigravity test on, or "" when it is quiet. Flags come first:
// they are a previous test's cleanup that never ran, and no amount of waiting
// clears them.
func antigravityStateLeak() string {
	if antigravityDiscoveryRunning() {
		return "the discovery tick is still running"
	}
	select {
	case <-antigravityCaptureStopped():
	default:
		return "a capture poller is still running"
	}
	antigravityRefreshWorkerMu.Lock()
	running, rearm := antigravityRefreshWorkerRunning, antigravityRefreshWorkerRearm
	antigravityRefreshWorkerMu.Unlock()
	switch {
	case running:
		return "the debt worker still holds its claim"
	case rearm:
		return "a debt worker re-arm is still pending"
	}
	if n := antigravityFreshnessInFlight.Load(); n > 0 {
		return fmt.Sprintf("%d background write(s) in flight", n)
	}
	if antigravityRunDebtRetryPending() {
		return "a run-debt retry rung is still armed"
	}
	return ""
}

// waitAntigravityQuiet polls `leak` until it reports nothing or `within`
// passes, and returns the last condition still holding ("" when quiet).
func waitAntigravityQuiet(within time.Duration, leak func() string) string {
	deadline := time.Now().Add(within)
	for {
		reason := leak()
		if reason == "" || !time.Now().Before(deadline) {
			return reason
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// requireAntigravityStateQuiet waits up to antigravityStateQuietBound for a
// quiet process and fails the CURRENT test naming the condition that still
// holds. Callers stop the refresh schedule first
// (helperStopAntigravityRefreshSchedule), so an armed rung is reported only
// when one was booked during that stop's own wait.
func requireAntigravityStateQuiet(tb testing.TB) {
	tb.Helper()
	if reason := waitAntigravityQuiet(antigravityStateQuietBound, antigravityStateLeak); reason != "" {
		tb.Fatalf("Antigravity usage state is not quiet: %s — a previous test's cleanup did not run "+
			"or did not drain [%s]", reason, antigravityStateForTest(antigravityQuotaCachePath()))
	}
}

// requireAntigravityDrained is the cleanup form: it stops the schedule (the
// same stop every Antigravity fixture's cleanup does, so a rung a test left
// armed for that cleanup to cancel is not reported) and fails the test that
// LEAKED when anything is still in flight, a worker claim is still held or the
// tick or a poller is still running.
func requireAntigravityDrained(tb testing.TB) {
	tb.Helper()
	helperStopAntigravityRefreshSchedule()
	if reason := waitAntigravityQuiet(antigravityStateQuietBound, antigravityStateLeak); reason != "" {
		tb.Errorf("this test left Antigravity usage state running past its end: %s [%s]",
			reason, antigravityStateForTest(antigravityQuotaCachePath()))
	}
}

// resetAntigravityProcessMemory forgets the in-memory notes a new process
// would not have: the debt generation's account and route, the latest attempt
// mark, the live probe's producer and the login-renewal spacing clock. None is
// persisted, and each is keyed or timed in a way a later test can collide
// with. antigravityQuotaShapesLogged is left alone: the redaction tests reset
// it for themselves.
func resetAntigravityProcessMemory() {
	debtAccount := &antigravityDebtAccount
	debtAccount.mu.Lock()
	debtAccount.debtID, debtAccount.fingerprint = antigravityDebtID{}, ""
	debtAccount.mu.Unlock()

	debtRoute := &antigravityDebtRoute
	debtRoute.mu.Lock()
	debtRoute.debtID, debtRoute.route, debtRoute.closed = antigravityDebtID{}, "", false
	debtRoute.mu.Unlock()

	attempt := &antigravityAttempt
	attempt.mu.Lock()
	attempt.current = antigravityCurrentAttempt{}
	attempt.mu.Unlock()

	antigravityLiveProducer.mu.Lock()
	antigravityLiveProducer.fingerprint, antigravityLiveProducer.at = "", time.Time{}
	antigravityLiveProducer.mu.Unlock()

	resetAntigravityLoginRenewal()
}

// antigravityStateForTest is the redacted diagnostic a failing wait prints:
// the process-wide flags and counters, the persisted debt (read from
// antigravityFreshnessPath()) and the cached snapshot at `cache`.
func antigravityStateForTest(cache string) string {
	antigravityRefreshWorkerMu.Lock()
	running, rearm := antigravityRefreshWorkerRunning, antigravityRefreshWorkerRearm
	antigravityRefreshWorkerMu.Unlock()
	poller := false
	select {
	case <-antigravityCaptureStopped():
	default:
		poller = true
	}
	parts := []string{
		fmt.Sprintf("nowMs=%d", time.Now().UnixMilli()),
		fmt.Sprintf("process{inFlight=%d rungArmed=%v workerRunning=%v workerRearm=%v tickRunning=%v pollerRunning=%v}",
			antigravityFreshnessInFlight.Load(), antigravityRunDebtRetryPending(), running, rearm,
			antigravityDiscoveryRunning(), poller),
	}

	var state antigravityUsageFreshness
	if readJSONFile(antigravityFreshnessPath(), &state) {
		parts = append(parts, fmt.Sprintf(
			"debt{refreshOwedAtMs=%d refreshOwedFloorMs=%d runFloorMs=%d attempts=%d nextAttemptAtMs=%d lastPaidAtMs=%d gated=%v outcome=%s}",
			state.RefreshOwedAtMs, state.RefreshOwedFloorMs, state.RunFloorMs, state.Attempts,
			state.NextAttemptAtMs, state.LastPaidAtMs, state.Gated, firstNonEmpty(state.Outcome, "none")))
	} else {
		parts = append(parts, "debt{missing}")
	}

	if cache == "" {
		return strings.Join(append(parts, "snapshot{unpinned}"), " ")
	}
	var snap antigravityQuotaSnapshot
	if !readJSONFile(cache, &snap) {
		return strings.Join(append(parts, "snapshot{missing}"), " ")
	}
	parts = append(parts, fmt.Sprintf(
		"snapshot{observedAtMs=%d schemaVersion=%d buckets=%d disabledBuckets=%d unobservedBuckets=%d}",
		antigravitySnapshotObservedMs(snap), snap.SchemaVersion, len(snap.Buckets),
		len(snap.DisabledBuckets), len(snap.UnobservedBuckets)))
	return strings.Join(parts, " ")
}
