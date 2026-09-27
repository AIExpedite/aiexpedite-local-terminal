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

	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("live reads = %d, want exactly one per invocation however many callers raced", got)
	}
	snap := f.snapshot(t)
	if snap.RefreshLiveReads != 1 {
		t.Fatalf("read counter = %d, want exactly one outbound read charged", snap.RefreshLiveReads)
	}
	if snap.RefreshFallbackState != codexFallbackDeferred {
		t.Fatalf("fallback state = %q, want deferred with %d of %d reads left",
			snap.RefreshFallbackState, codexRefreshLiveReadMaxAttempts-snap.RefreshLiveReads, codexRefreshLiveReadMaxAttempts)
	}
}

// The read budget is per DEBT GENERATION and per read that actually reached
// OpenAI — two distinct failed reads across two rungs, and only the second makes
// the debt `exhausted`. The old final-`spent` state made this impossible: the
// first failure ended the debt's network recovery and the two-read budget was a
// fiction.
func TestCodexLiveFallback_SecondReadRunsOnTheNextRung(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	calls := stubCodexFallbackRead(t, func(context.Context, string) string { return liveProbeOutcomeRPCError })
	state := f.oweExhaustedDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute))

	if kind := codexLiveUsageFallback(f.fp, state); kind != codexRetryAfterRead {
		t.Fatalf("a read that reached OpenAI and failed must book afterRead, got %v", kind)
	}
	first := codexRunFreshnessForAccount(f.fp, time.Now())
	if first.liveReads != 1 || first.fallback != codexFallbackDeferred {
		t.Fatalf("after read 1: %+v, want 1 read spent and deferred", first)
	}
	if codexStaleRunNotice(first) != "" {
		t.Fatal("a deferred debt with budget left must not warn yet")
	}

	codexLiveUsageFallback(f.fp, first)
	second := codexRunFreshnessForAccount(f.fp, time.Now())
	if second.liveReads != codexRefreshLiveReadMaxAttempts || second.fallback != codexFallbackExhausted {
		t.Fatalf("after read 2: %+v, want the budget spent and exhausted", second)
	}
	if got := atomic.LoadInt32(calls); got != int32(codexRefreshLiveReadMaxAttempts) {
		t.Fatalf("live reads = %d, want %d", got, codexRefreshLiveReadMaxAttempts)
	}

	// Exhausted is the ONE state a later pass cannot re-enter.
	codexLiveUsageFallback(f.fp, second)
	if got := atomic.LoadInt32(calls); got != int32(codexRefreshLiveReadMaxAttempts) {
		t.Fatalf("an exhausted debt spent another read (total %d)", got)
	}
}

// Every live-probe outcome is classified once, and all of that classification is
// load-bearing: whether the read cost a budget slot, and which rung the debt
// waits for next. A result that sent nothing must never cost a read; an
// unrecognised one from a newer producer must never loop for free; and an `ok`
// ends the ladder only when the reading it carried actually settled the debt.
func TestCodexLiveFallback_OutcomeClassification(t *testing.T) {
	for _, tc := range []struct {
		name     string
		outcome  string
		settled  bool
		outbound bool
		kind     codexRunDebtRetryKind
	}{
		{"ok_settled", liveProbeOutcomeOK, true, true, codexRetryNone},
		// The probe reports the RPC, not the merge: a window refused as stale or
		// reset-only answers `ok` with the debt still owed, and that read has to
		// book the next rung rather than retire the ladder.
		{"ok_not_merged", liveProbeOutcomeOK, false, true, codexRetryAfterRead},
		{"rpc_error", liveProbeOutcomeRPCError, false, true, codexRetryAfterRead},
		{"no_reading", liveProbeOutcomeNoReading, false, true, codexRetryAfterRead},
		{"timeout", liveProbeOutcomeTimeout, false, true, codexRetryAfterRead},
		{"not_attributable", liveProbeOutcomeNotSigned, false, true, codexRetryAfterRead},
		{"spawn_failed", liveProbeOutcomeSpawnFailed, false, false, codexRetryFree},
		{"account_changed", liveProbeOutcomeAccountChanged, false, false, codexRetryFree},
		{"gated", liveProbeOutcomeGated, false, false, codexRetryFree},
		{"cooldown", liveProbeOutcomeCooldown, false, false, codexRetrySpacing},
		{"unknown", "some_future_outcome", false, true, codexRetryAfterRead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexLiveReadOutbound(tc.outcome); got != tc.outbound {
				t.Fatalf("outbound = %v, want %v", got, tc.outbound)
			}
			if got := codexLiveReadRetryKind(tc.outcome, tc.settled); got != tc.kind {
				t.Fatalf("retry kind = %v, want %v", got, tc.kind)
			}
		})
	}
}

// An `ok` whose reading never landed keeps the ladder walking. codexLiveProbeConverse
// hands its frame to captureCodexRateLimitLineFromProducer and returns
// liveProbeOutcomeOK without consulting the boolean that says whether the reading
// was merged, so a window refused as stale or reset-only used to end the debt's
// only network recovery on its FIRST read: the worker booked nothing and resolved
// the outstanding fallback to `exhausted`, and the second read the budget promises
// was never run.
func TestCodexLiveFallback_OKThatDidNotMergeKeepsTheLadderWalking(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	// The stub answers `ok` and merges NOTHING — the refused-capture case.
	var calls int32
	stubCodexFallbackRead(t, func(context.Context, string) string {
		atomic.AddInt32(&calls, 1)
		return liveProbeOutcomeOK
	})
	state := f.oweExhaustedDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute))

	if kind := codexLiveUsageFallback(f.fp, state); kind != codexRetryAfterRead {
		t.Fatalf("first read booked %v, want afterRead", kind)
	}
	state = codexRunFreshnessForAccount(f.fp, time.Now())
	if !state.owed {
		t.Fatal("the debt was cleared by a reading that never merged")
	}
	if state.liveReads != 1 {
		t.Fatalf("liveReads = %d, want 1 (the read reached OpenAI)", state.liveReads)
	}
	if state.fallback != codexFallbackDeferred {
		t.Fatalf("fallback = %q, want deferred", state.fallback)
	}

	// The promised second read runs, and only then is the budget spent.
	if kind := codexLiveUsageFallback(f.fp, state); kind != codexRetryAfterRead {
		t.Fatalf("second read booked %v, want afterRead", kind)
	}
	if got := atomic.LoadInt32(&calls); got != int32(codexRefreshLiveReadMaxAttempts) {
		t.Fatalf("live reads = %d, want %d", got, codexRefreshLiveReadMaxAttempts)
	}
	state = codexRunFreshnessForAccount(f.fp, time.Now())
	if state.fallback != codexFallbackExhausted {
		t.Fatalf("fallback = %q, want exhausted once the budget is spent", state.fallback)
	}
}

// A refusal that sent nothing costs no read slot at all, however many times it
// happens: the debt keeps its whole budget for the rung that finally gets out.
func TestCodexLiveFallback_RefusalsThatSentNothingCostNoRead(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	stubCodexFallbackRead(t, func(context.Context, string) string { return liveProbeOutcomeSpawnFailed })
	state := f.oweExhaustedDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute))

	for i := 0; i < 3; i++ {
		if kind := codexLiveUsageFallback(f.fp, state); kind != codexRetryFree {
			t.Fatalf("pass %d booked %v, want free", i, kind)
		}
		state = codexRunFreshnessForAccount(f.fp, time.Now())
	}
	if state.liveReads != 0 || state.fallback == codexFallbackExhausted {
		t.Fatalf("a refusal that sent nothing must not drain the budget: %+v", state)
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

// Every way a read that REACHED OpenAI can fail leaves the debt standing —
// nothing is booked as observed — and, once the read budget is spent, reaches the
// stale notice (or, after an upgrade, the drift notice) rather than holding it
// back. liveProbeOutcomeSpawnFailed is deliberately absent: it sent nothing, so
// it costs no read and keeps trying on a free rung
// (TestCodexLiveFallback_RefusalsThatSentNothingCostNoRead).
func TestCodexLiveFallback_FailuresLeaveTheDebtAndReachTheNotice(t *testing.T) {
	for _, outcome := range []string{liveProbeOutcomeRPCError, liveProbeOutcomeNoReading, liveProbeOutcomeTimeout, liveProbeOutcomeNotSigned} {
		t.Run(outcome, func(t *testing.T) {
			now := time.Now()
			f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
			f.seedPreRunReading(t, now.Add(-time.Hour), now)
			// The reading on the card came from the previous binary.
			codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) { snap.CodexVersion = "codex-cli 0.149.0" })
			f.oweExhaustedDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute))
			stubCodexFallbackRead(t, func(context.Context, string) string { return outcome })

			codexPayRunRefresh(f.home, f.fp)

			// Drain the whole read budget: the notice is only due once nothing is
			// left to try (`exhausted`), not on the first failure.
			for i := 1; i < codexRefreshLiveReadMaxAttempts+1; i++ {
				codexPayRunRefresh(f.home, f.fp)
			}
			state := codexRunFreshnessForAccount(f.fp, time.Now())
			if !state.owed || state.fallback != codexFallbackExhausted {
				t.Fatalf("state = %+v, want the debt still owed and the fallback exhausted", state)
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
