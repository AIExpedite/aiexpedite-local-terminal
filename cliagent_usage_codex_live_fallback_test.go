package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_codex_live_fallback_test.go — the one live
   `account/rateLimits/read` a Codex run debt may spend once its rollout
   attempts are exhausted (codexLiveUsageFallback). Every row drives the read
   through codexLiveUsageFallbackRead, so no row starts a real app-server.
   ------------------------------------------------------------------------ */

// stubCodexFallbackRead replaces the fallback's live read and counts calls.
func stubCodexFallbackRead(t *testing.T, fn func(ctx context.Context, fp string) string) *int32 {
	t.Helper()
	var calls int32
	original := codexLiveUsageFallbackRead
	codexLiveUsageFallbackRead = func(ctx context.Context, fp string) string {
		atomic.AddInt32(&calls, 1)
		return fn(ctx, fp)
	}
	t.Cleanup(func() {
		// Drain every worker first: one still running would read the seam
		// while it is being restored.
		resetCodexUsageRefreshGate()
		codexLiveUsageFallbackRead = original
	})
	return &calls
}

// codexLiveReadEnvelope is an `account/rateLimits/read` response as the probe
// re-wraps it before capture.
func codexLiveReadEnvelope(primaryPct, secondaryPct int, now time.Time) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"result":{"rateLimits":{`+
		`"primary":{"usedPercent":%d,"windowDurationMins":300,"resetsAt":%d},`+
		`"secondary":{"usedPercent":%d,"windowDurationMins":10080,"resetsAt":%d}}}}`,
		primaryPct, now.Add(2*time.Hour).Unix(), secondaryPct, now.Add(70*time.Hour).Unix())
}

// capturingFallbackRead is a live read that lands a fresh reading for fp.
func capturingFallbackRead(ctx context.Context, fp string) string {
	captureCodexRateLimitLineForAccount(codexLiveReadEnvelope(33, 44, time.Now()), time.Now(), fp)
	return liveProbeOutcomeOK
}

// oweExhaustedDebt records a finished run whose rollout attempts are already
// spent, i.e. the state in which the fallback becomes owed.
func (f codexFreshnessFixture) oweExhaustedDebt(t *testing.T, runStart, completedAt time.Time) codexRunFreshnessState {
	t.Helper()
	if !codexRecordRunFreshness(f.fp, completedAt, func(snap *codexRateLimitSnapshot) {
		codexOweRunRefresh(snap, runStart, completedAt)
		snap.RefreshOwedAttempts = codexRefreshAfterRunMaxAttempts
	}) {
		t.Fatal("recording the debt failed")
	}
	state := codexRunFreshnessForAccount(f.fp, time.Now())
	if !state.owed {
		t.Fatalf("fixture debt must be owed: %+v", state)
	}
	return state
}

func setCodexTestOffline(t *testing.T, offline bool) {
	t.Helper()
	offlineMutex.Lock()
	was := isOffline
	isOffline = offline
	offlineMutex.Unlock()
	t.Cleanup(func() {
		offlineMutex.Lock()
		isOffline = was
		offlineMutex.Unlock()
	})
}

// Two callers settling onto the SAME debt generation spend one live read, not
// two: the gate admits one fallback per account, and the loser finds the
// generation resolved.
func TestCodexLiveFallback_ConcurrentCallersOnOneDebtSpendOneRead(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	state := f.oweExhaustedDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute))
	release := make(chan struct{})
	calls := stubCodexFallbackRead(t, func(ctx context.Context, fp string) string {
		<-release
		return liveProbeOutcomeNoReading
	})

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codexLiveUsageFallback(f.fp, state)
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	// A later caller for the same generation finds it spent.
	codexLiveUsageFallback(f.fp, codexRunFreshnessForAccount(f.fp, time.Now()))

	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("live reads = %d, want exactly one per debt generation", got)
	}
	if snap := f.snapshot(t); snap.RefreshFallbackState != codexFallbackSpent {
		t.Fatalf("fallback state = %q, want spent", snap.RefreshFallbackState)
	}
}

// A newer run finishing opens a NEW generation (codexOweRunRefresh resets the
// fallback with the attempt counter), and that debt legitimately gets its own
// read. The bound is per generation, not per worker or per account.
func TestCodexLiveFallback_NewGenerationGetsItsOwnRead(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	calls := stubCodexFallbackRead(t, func(context.Context, string) string { return liveProbeOutcomeNoReading })

	first := f.oweExhaustedDebt(t, now.Add(-4*time.Minute), now.Add(-3*time.Minute))
	codexLiveUsageFallback(f.fp, first)
	second := f.oweExhaustedDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute))
	if second.fallback != codexFallbackUnset {
		t.Fatalf("a new debt generation must reset the fallback, got %q", second.fallback)
	}
	codexLiveUsageFallback(f.fp, second)

	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("live reads = %d, want one per generation (2)", got)
	}
}

// Every way the read can fail leaves the debt standing — nothing is booked as
// observed — and resolves the fallback, so the stale notice (or, after an
// upgrade, the drift notice) is reached rather than held back.
func TestCodexLiveFallback_FailuresLeaveTheDebtAndReachTheNotice(t *testing.T) {
	for _, outcome := range []string{liveProbeOutcomeSpawnFailed, liveProbeOutcomeRPCError, liveProbeOutcomeNoReading, liveProbeOutcomeTimeout} {
		t.Run(outcome, func(t *testing.T) {
			now := time.Now()
			f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
			f.seedPreRunReading(t, now.Add(-time.Hour), now)
			// The reading on the card came from the previous binary.
			codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) { snap.CodexVersion = "codex-cli 0.149.0" })
			f.oweExhaustedDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute))
			stubCodexFallbackRead(t, func(context.Context, string) string { return outcome })

			codexPayRunRefresh(f.home, f.fp)

			state := codexRunFreshnessForAccount(f.fp, time.Now())
			if !state.owed || state.fallback != codexFallbackSpent {
				t.Fatalf("state = %+v, want the debt still owed and the fallback spent", state)
			}
			if notice := codexStaleRunNotice(state); notice == "" {
				t.Fatal("a failed fallback must let the stale notice through")
			}
			if notice := codexRunFreshnessNotice(state, "codex-cli 0.150.0"); !strings.Contains(notice, "0.150.0") || !strings.Contains(notice, "0.149.0") {
				t.Fatalf("after an upgrade the card must name the capture drift, got %q", notice)
			}
		})
	}
}

// A credentials swap between the debt and the read drops the reading instead
// of booking the new account's figures against the old account's run.
func TestCodexLiveFallback_AccountChangeDropsTheReading(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	state := f.oweExhaustedDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute))
	helperCodexAuthAt(t, f.home, "someone.else@example.com", now.Add(-time.Hour))
	if currentCodexAccountFingerprint() == f.fp {
		t.Fatal("fixture must sign another account in")
	}
	// The real read, with a probe that would capture if it were ever reached.
	var probed int32
	original := probeCodexRateLimitsLiveFn
	probeCodexRateLimitsLiveFn = func(ctx context.Context, path, want string) string {
		atomic.AddInt32(&probed, 1)
		return capturingFallbackRead(ctx, currentCodexAccountFingerprint())
	}
	t.Cleanup(func() { probeCodexRateLimitsLiveFn = original })
	resetCodexLiveRateLimitRead()
	stubCodexFallbackRead(t, func(ctx context.Context, fp string) string { return codexLiveRateLimitRead(ctx, "", fp) })

	codexLiveUsageFallback(f.fp, state)

	if atomic.LoadInt32(&probed) != 0 {
		t.Fatal("the probe must refuse before spawning when another account is signed in")
	}
	snap := f.snapshot(t)
	if snap.AccountFingerprint != f.fp || snap.RefreshOwedAtMs == 0 {
		t.Fatalf("the old account's debt must be left as it was, not rescoped or paid: %+v", snap)
	}
}

// A read in flight when the gate is reset (shutdown / test teardown) is
// abandoned: nothing is written into a cache that may now belong to the next
// generation of the process.
func TestCodexLiveFallback_ResetDuringReadWritesNothing(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	state := f.oweExhaustedDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute))
	entered := make(chan struct{})
	stubCodexFallbackRead(t, func(ctx context.Context, fp string) string {
		close(entered)
		<-ctx.Done()
		return liveProbeOutcomeTimeout
	})

	codexUsageRefresh.spawn(func() { codexLiveUsageFallback(f.fp, state) })
	<-entered
	resetCodexUsageRefreshGate()

	if snap := f.snapshot(t); snap.RefreshFallbackState != codexFallbackOutstanding {
		t.Fatalf("a cancelled read must not resolve the fallback, got %q", snap.RefreshFallbackState)
	}
}
