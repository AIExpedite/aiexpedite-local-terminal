package main

import (
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_grok_refresh_restart_test.go
   --------------------------------------------------------------------------
   The Grok refresh debt survives a restart or self-update: gracefulShutdown
   stops the rung (stopGrokRunDebtRetry) and StartAgent's
   payOwedGrokUsageRefresh resumes what the previous process left on disk.
   Each "new process" below is the same test process with the in-memory timer
   stopped and the state file left as the old one wrote it.
   ------------------------------------------------------------------------ */

func TestPayOwedGrokUsageRefresh_ReArmsTheSavedRung(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.outcome.Store(grokLiveOutcomeHTTPError)
	h.runAndSettle(time.Second)
	saved := h.state()
	if saved.NextAttemptAtMs == 0 {
		t.Fatalf("no rung booked before the hand-off: %+v", saved)
	}

	// Shutdown before the update hand-off.
	stopGrokRunDebtRetry()
	if grokRunDebtRetryPending() {
		t.Fatal("shutdown left a rung armed")
	}

	// The new process starts 20 s later, before the rung is due.
	h.advance(20 * time.Second)
	payOwedGrokUsageRefresh()
	h.idle()
	if !grokRunDebtRetryPending() {
		t.Fatal("startup did not re-arm the saved rung")
	}
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, a rung not yet due must not read at start", h.reads.Load())
	}
	if got := h.state(); got.NextAttemptAtMs != saved.NextAttemptAtMs || got.Attempts != saved.Attempts {
		t.Fatalf("schedule changed across the restart: before %+v after %+v", saved, got)
	}
}

func TestPayOwedGrokUsageRefresh_SettledRunWithNoRungGetsOneAttempt(t *testing.T) {
	h := newGrokDebtHarness(t)
	// The previous process settled a run and died before its worker read.
	h.write(grokUsageFreshness{
		RunFloorMs: h.clock().Add(-time.Minute).UnixMilli(), CompletionMs: h.clock().Add(-time.Second).UnixMilli(),
		OwedAtMs: h.clock().Add(-time.Second).UnixMilli(), AccountFingerprint: "fp-ada",
	})
	h.advance(5 * time.Second)
	payOwedGrokUsageRefresh()
	h.idle()
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want exactly one attempt at start", h.reads.Load())
	}
	if h.state().owed() {
		t.Fatalf("debt kept after an ok read: %+v", h.state())
	}
}

func TestPayOwedGrokUsageRefresh_InterruptedRunIsOwedOneRead(t *testing.T) {
	h := newGrokDebtHarness(t)
	// A floor armed by the old process, never settled: the update replaced
	// the process mid-run.
	h.write(grokUsageFreshness{RunFloorMs: h.clock().Add(-30 * time.Second).UnixMilli(), RunFloorAccount: "fp-ada"})
	payOwedGrokUsageRefresh()
	h.idle()
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want the interrupted run paid once", h.reads.Load())
	}
	if state := h.state(); state.owed() || state.RunFloorMs != 0 {
		t.Fatalf("state = %+v, want nothing left after an ok read", state)
	}
}

func TestPayOwedGrokUsageRefresh_OfflineSendsNothingAndKeepsTheDebt(t *testing.T) {
	h := newGrokDebtHarness(t)
	offlineMutex.Lock()
	wasOffline := isOffline
	isOffline = true
	offlineMutex.Unlock()
	t.Cleanup(func() {
		offlineMutex.Lock()
		isOffline = wasOffline
		offlineMutex.Unlock()
	})
	h.write(grokUsageFreshness{
		CompletionMs: h.clock().Add(-time.Second).UnixMilli(), OwedAtMs: h.clock().Add(-time.Second).UnixMilli(),
		AccountFingerprint: "fp-ada",
	})
	payOwedGrokUsageRefresh()
	h.idle()
	if h.reads.Load() != 0 {
		t.Fatalf("reads = %d, offline must send nothing", h.reads.Load())
	}
	if state := h.state(); !state.owed() || state.Attempts != 0 {
		t.Fatalf("state = %+v, want the debt kept with its budget", state)
	}
}
