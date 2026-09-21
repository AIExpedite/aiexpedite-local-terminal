package main

import (
	"bytes"
	"os"
	"testing"
	"time"
)

// simulateCodexAgentRestart discards every in-memory trace of the previous
// process — single flight, interval, workers — and arms a fresh one, as
// StartAgent does after a restart or self-update. The cache file (same config
// dir) is all that carries over.
func simulateCodexAgentRestart(t *testing.T) {
	t.Helper()
	resetCodexUsageRefreshGate()
	SetCodexUsageRefreshEnabled(true)
}

func codexGateHasRun(fp string) bool {
	codexUsageRefresh.mu.Lock()
	defer codexUsageRefresh.mu.Unlock()
	_, ran := codexUsageRefresh.lastRun[fp]
	return ran
}

// A run finished and recorded its debt, then the agent was replaced before the
// post-run reconcile could pay it. The new process's startup replay pays it:
// observedAt advances past the floor and the debt clears.
func TestCodexOwedRefresh_SurvivesAgentRestart(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-3 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	writeCodexRunRollout(t, f.home, "run", runStart, runStart.Add(time.Minute), runStart.Add(70*time.Second), true,
		[]map[string]any{codexRateLimitFrame(47, 52, now)})
	f.advanceCursorPast(t, now.Add(-10*time.Second), now)
	codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		codexOweRunRefresh(snap, runStart, now.Add(-time.Minute))
	})

	simulateCodexAgentRestart(t)
	payOwedCodexUsageRefresh()
	waitCodexUsageRefreshIdle(t)

	session := codexSessionMetric(t, codexMetricsFromCache(time.Now(), f.fp))
	if got := metricObservedAt(t, session); got.UnixMilli() < runStart.UnixMilli() {
		t.Fatalf("observedAt %s still predates the run floor %s after the restart replay", got, runStart)
	}
	if snap := f.snapshot(t); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("replayed debt not cleared: %+v", snap)
	}
}

// The replay is exactly ONE bounded reconcile, even when it finds nothing: the
// remainder is left to the next run or refresh under the ordinary bounds.
func TestCodexOwedRefresh_RestartReplayIsExactlyOneReconcile(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-3 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		codexOweRunRefresh(snap, runStart, now.Add(-time.Minute))
	})

	simulateCodexAgentRestart(t)
	payOwedCodexUsageRefresh()
	waitCodexUsageRefreshIdle(t)

	snap := f.snapshot(t)
	if snap.RefreshOwedAtMs == 0 {
		t.Fatal("an unpaid debt must survive the replay")
	}
	if snap.RefreshOwedAttempts != 1 {
		t.Fatalf("replay spent %d reconciles, want exactly 1", snap.RefreshOwedAttempts)
	}
}

// A debt older than codexRefreshOwedMaxAge is not replayed: no reconcile, no
// write — the cache is byte-for-byte what the previous process left.
func TestCodexOwedRefresh_ExpiredMarkerIsNotReplayed(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-40 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	codexRecordRunFreshness(f.fp, now.Add(-35*time.Minute), func(snap *codexRateLimitSnapshot) {
		codexOweRunRefresh(snap, runStart, now.Add(-35*time.Minute))
	})
	before, err := os.ReadFile(f.cache)
	if err != nil {
		t.Fatal(err)
	}

	simulateCodexAgentRestart(t)
	payOwedCodexUsageRefresh()
	waitCodexUsageRefreshIdle(t)

	if codexGateHasRun(f.fp) {
		t.Fatal("an expired debt must not spend a reconcile")
	}
	after, err := os.ReadFile(f.cache)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("an expired debt must not rewrite the cache")
	}
	if state := codexRunFreshnessForAccount(f.fp, time.Now()); state.owed || state.interrupted {
		t.Fatalf("expired debt still reads as outstanding: %+v", state)
	}
}

// The process died mid-run (armed floor, never settled): at startup that run
// is over, so the replay records it as owed and spends its one reconcile.
func TestCodexOwedRefresh_InterruptedRunIsOwedAtStartup(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-5 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	codexRecordRunFreshness(f.fp, runStart, func(snap *codexRateLimitSnapshot) {
		codexArmRunFloor(snap, runStart)
	})
	if state := codexRunFreshnessForAccount(f.fp, now); !state.interrupted || state.owed {
		t.Fatalf("armed, unsettled floor should read as interrupted: %+v", state)
	}

	simulateCodexAgentRestart(t)
	payOwedCodexUsageRefresh()
	waitCodexUsageRefreshIdle(t)

	snap := f.snapshot(t)
	if snap.RefreshOwedAtMs == 0 || snap.RunFloorMs != runStart.UnixMilli() {
		t.Fatalf("interrupted run must become an owed debt at its floor: %+v", snap)
	}
	if snap.RefreshOwedAttempts != 1 {
		t.Fatalf("attempts = %d, want the replay's 1", snap.RefreshOwedAttempts)
	}
}

// With the freshness path unarmed (every test binary that has not called
// SetCodexUsageRefreshEnabled) nothing is replayed or scanned.
func TestCodexOwedRefresh_UnarmedProcessDoesNothing(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		codexOweRunRefresh(snap, now.Add(-time.Minute), now)
	})
	SetCodexUsageRefreshEnabled(false)

	payOwedCodexUsageRefresh()
	triggerCodexUsageRefreshAfterRun(now.Add(-time.Minute))
	armCodexUsageRunFloor(now)
	waitCodexUsageRefreshIdle(t)

	if codexGateHasRun(f.fp) {
		t.Fatal("an unarmed process must not reconcile")
	}
}

// Bounded cache locking may REFUSE the write that converts an interrupted run
// into an owed debt at startup. The run must not be left merely `interrupted`:
// routine gathers force a reconcile only on `owed`, so it would never be
// refreshed and never warn. The debt is retained and retried through the same
// worker a refused post-run settle uses.
func TestCodexOwedRefresh_RefusedInterruptedConversionIsRetried(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-5 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	codexRecordRunFreshness(f.fp, runStart, func(snap *codexRateLimitSnapshot) {
		codexArmRunFloor(snap, runStart)
	})
	// Leave the worker's retry inside the wedge below, but the flush after it.
	codexRefreshAfterRunRetryDelay = 300 * time.Millisecond
	prevWait, prevPoll := codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll
	codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = 50*time.Millisecond, time.Millisecond
	t.Cleanup(func() { codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = prevWait, prevPoll })

	simulateCodexAgentRestart(t)
	// Wedge the in-process gate across the whole startup replay — conversion,
	// reconcile, attempt count and the promoted-run check — then free it.
	codexRateLimitMu.Lock()
	go func() {
		time.Sleep(500 * time.Millisecond)
		codexRateLimitMu.Unlock()
	}()
	payOwedCodexUsageRefresh()
	waitCodexUsageRefreshIdle(t)

	snap := f.snapshot(t)
	if snap.RefreshOwedAtMs == 0 {
		t.Fatalf("refused startup conversion left the interrupted run un-owed: %+v", snap)
	}
	if snap.RunFloorMs != runStart.UnixMilli() {
		t.Fatalf("retried debt recorded floor %d, want the interrupted run's %d", snap.RunFloorMs, runStart.UnixMilli())
	}
	if state := codexRunFreshnessForAccount(f.fp, time.Now()); !state.owed || state.interrupted {
		t.Fatalf("interrupted run must read as owed after the retry: %+v", state)
	}
}
