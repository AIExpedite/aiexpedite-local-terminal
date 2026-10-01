package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// waitForClaudeCondition fails the test unless cond holds within `within` —
// for effects a retry rung produces on the timer's goroutine.
func waitForClaudeCondition(t *testing.T, within time.Duration, msg string, cond func() bool) {
	t.Helper()
	if !waitUntil(time.Now().Add(within), cond) {
		t.Fatal(msg)
	}
}

// pinClaudeRunDebtLadder shrinks the retry ladder and the free rung so a test
// can watch rungs fire without waiting minutes.
func pinClaudeRunDebtLadder(t *testing.T, ladder []time.Duration, free, slack time.Duration) {
	t.Helper()
	origLadder, origFree, origSlack := claudeRunDebtRetryLadder, claudeRunDebtFreeRetryDelay, claudeRunDebtRungSlack
	claudeRunDebtRetryLadder, claudeRunDebtFreeRetryDelay, claudeRunDebtRungSlack = ladder, free, slack
	t.Cleanup(func() {
		stopClaudeRunDebtRetry()
		claudeRunDebtRetryLadder, claudeRunDebtFreeRetryDelay, claudeRunDebtRungSlack = origLadder, origFree, origSlack
	})
}

// The budgeted ladder: 15 s after the first request, then 60 s and 4 min; the
// request that spends the budget retires the debt instead of booking a rung.
// The ladder's last rung (15 min) is the free rung's ceiling.
func TestClaudeRunDebtRung_BudgetedLadder(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	// Whole milliseconds, the resolution NextAttemptAtMs is stored at.
	now := time.UnixMilli(time.Now().UnixMilli())
	owed := now.Add(-time.Minute)
	for attempts, want := range map[int]time.Duration{1: 15 * time.Second, 2: time.Minute, 3: 4 * time.Minute} {
		seedClaudeRefreshDebt(t, cache, "", owed, attempts, time.Time{})
		if !claudeBookRunDebtRung("", owed, now, claudeRungBudgeted, 0) {
			t.Fatalf("attempts=%d: nothing booked", attempts)
		}
		if got := time.UnixMilli(claudeCacheSnapshot(t, cache).NextAttemptAtMs).Sub(now); got != want {
			t.Errorf("attempts=%d: rung in %v, want %v", attempts, got, want)
		}
	}
	claudeUsageProbe.recordOwed(owed)
	seedClaudeRefreshDebt(t, cache, "", owed, claudeRefreshOwedMaxRequests, time.Time{})
	if claudeBookRunDebtRung("", owed, now, claudeRungBudgeted, 0) {
		t.Error("a debt that spent its budget must not book another rung")
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 || snap.NextAttemptAtMs != 0 {
		t.Errorf("a spent budget must retire the debt: %+v", snap)
	}
	if !claudeUsageProbe.owedObservation().IsZero() {
		t.Error("retirement must drop the in-memory debt too, or the gather re-pays it uncharged")
	}
}

// Free rungs back off with the debt's age — at least the free floor, at most
// the longest rung — and never land past the age-out.
func TestClaudeRunDebtRung_FreeRungBacksOffWithAge(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	// Whole milliseconds, the resolution NextAttemptAtMs is stored at.
	now := time.UnixMilli(time.Now().UnixMilli())
	for owedFor, want := range map[time.Duration]time.Duration{
		5 * time.Second: claudeRunDebtFreeRetryDelay,
		2 * time.Minute: 2 * time.Minute,
		2 * time.Hour:   15 * time.Minute,
	} {
		owed := now.Add(-owedFor)
		seedClaudeRefreshDebt(t, cache, "", owed, 0, time.Time{})
		claudeBookRunDebtRung("", owed, now, claudeRungFree, 0)
		if got := time.UnixMilli(claudeCacheSnapshot(t, cache).NextAttemptAtMs).Sub(now); got != want {
			t.Errorf("owed for %v: free rung in %v, want %v", owedFor, got, want)
		}
	}
	owed := now.Add(-claudeRefreshOwedMaxAge + time.Minute)
	seedClaudeRefreshDebt(t, cache, "", owed, 0, time.Time{})
	claudeBookRunDebtRung("", owed, now, claudeRungFree, 0)
	if got, deadline := claudeCacheSnapshot(t, cache).NextAttemptAtMs, owed.Add(claudeRefreshOwedMaxAge).UnixMilli(); got > deadline+1 {
		t.Errorf("rung %d lands past the age-out %d", got, deadline)
	}
}

// A held debt's rung lands at the hold's end (plus slack), not on the ladder.
func TestClaudeRunDebtRung_HeldDebtWaitsForTheHold(t *testing.T) {
	_, _ = armClaudeUsageProbe(t, unreachableProbeHandler)
	claudeUsageProbe.holdUntil(time.Now().Add(10 * time.Minute))
	for _, result := range []claudeProbeResult{
		{code: claudeProbeHeld},
		{code: claudeProbeHTTP429, issued: true},
	} {
		kind, delay := claudeRunDebtRungFor(result)
		if kind != claudeRungAfter || delay < 10*time.Minute-time.Second || delay > 10*time.Minute+2*claudeRunDebtRungSlack {
			t.Errorf("%s: rung %v in %v, want at the hold's end", result.code, kind, delay)
		}
	}
}

// A stale generation's callback does nothing: a timer replaced or stopped
// after it fired but before its callback took the lock must not attempt.
func TestClaudeRunDebtRetry_StaleGenerationIsANoOp(t *testing.T) {
	_, _ = armClaudeUsageProbe(t, unreachableProbeHandler)
	var ran atomic.Int64
	claudeArmRunDebtRetryFn(time.Hour, func() { ran.Add(1) })
	claudeRunDebtRetryTimer.mu.Lock()
	stale := claudeRunDebtRetryTimer.gen
	claudeRunDebtRetryTimer.mu.Unlock()
	stopClaudeRunDebtRetry()

	claudeRunDebtRetryFired(stale, func() { ran.Add(1) })
	if ran.Load() != 0 {
		t.Error("a stopped generation's callback ran")
	}
	if claudeRunDebtRetryPending() {
		t.Error("stop must leave no timer armed")
	}

	claudeArmRunDebtRetryFn(0, func() { ran.Add(1) })
	waitForClaudeCondition(t, 5*time.Second, "the live generation never fired", func() bool { return ran.Load() == 1 })
}

// gracefulShutdown stops the Claude retry timer beside Codex's and
// Antigravity's, so a rung cannot fire into a process handing off to an update.
func TestGracefulShutdown_StopsTheClaudeRunDebtRetry(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "shutdown.go", nil, 0)
	if err != nil {
		t.Fatalf("parse shutdown.go: %v", err)
	}
	// Call order matters: the shutdown owe must follow the stop, so a re-owe
	// rung cannot fire into the exiting process after its debt was persisted.
	stopAt, persistAt := token.NoPos, token.NoPos
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "gracefulShutdown" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if ident, ok := call.Fun.(*ast.Ident); ok {
					switch ident.Name {
					case "stopClaudeRunDebtRetry":
						stopAt = call.Pos()
					case "persistClaudeRunDebtForShutdown":
						persistAt = call.Pos()
					}
				}
			}
			return true
		})
	}
	if stopAt == token.NoPos {
		t.Fatal("gracefulShutdown no longer stops the Claude run-debt retry timer")
	}
	if persistAt == token.NoPos || persistAt < stopAt {
		t.Fatal("gracefulShutdown must persist an in-memory Claude run debt after stopping its retry timer")
	}
}

// A run whose debt the cache locks refused lives only in memory behind its
// re-owe rung. Shutdown cancels that rung, so it must put the debt on disk
// itself; otherwise the next process publishes the pre-run utilization.
func TestPersistClaudeRunDebtForShutdown_PersistsARefusedRunDebt(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))
	pinClaudeRunDebtLadder(t, claudeRunDebtRetryLadder, time.Hour, time.Hour)

	original := claudeRateLimitBestEffortGateWait
	claudeRateLimitBestEffortGateWait = 20 * time.Millisecond
	t.Cleanup(func() { claudeRateLimitBestEffortGateWait = original })

	runEnded := time.Now()
	lockClaudeRateLimitCache()
	claudeUsageProbeAfterRun(runEnded)
	unlockClaudeRateLimitCache()
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("RefreshOwedAtMs=%d before shutdown, want the refused owe to leave nothing on disk", snap.RefreshOwedAtMs)
	}
	if !claudeRunDebtRetryPending() {
		t.Fatal("a refused owe must arm its re-owe rung")
	}

	stopClaudeRunDebtRetry()
	persistClaudeRunDebtForShutdown()

	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != runEnded.UnixMilli() {
		t.Fatalf("RefreshOwedAtMs=%d after shutdown, want the run's debt %d on disk", snap.RefreshOwedAtMs, runEnded.UnixMilli())
	}
}

// A debt already on disk is left byte-identical: the shutdown owe never
// rewrites an unchanged baseline, so its spent budget and rung survive.
func TestPersistClaudeRunDebtForShutdown_LeavesAPersistedDebtAlone(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	runEnded := time.Now()
	claudeUsageProbe.recordOwed(runEnded)
	claudeOweRunRefresh(runEnded)
	mutateClaudeRateLimitSnapshot(cache, fp, func(snap *claudeRateLimitSnapshot) bool {
		snap.RefreshOwedAttempts = 2
		snap.NextAttemptAtMs = runEnded.Add(4 * time.Minute).UnixMilli()
		return true
	})
	before, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}

	persistClaudeRunDebtForShutdown()

	after, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("shutdown rewrote a persisted debt:\nbefore %s\nafter  %s", before, after)
	}
}

// A proven credential's durable clear the cache refused lives only in memory
// behind its background retry, which stands down at shutdown. Shutdown must
// write it itself; otherwise the next process restores the rejected wait and
// refuses the proven credential as expired.
func TestPersistClaudeRunDebtForShutdown_FlushesAPendingCredentialClear(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	stamp := claudeCredStamp{modNs: 100, size: 10}
	fp := currentClaudeAccountFingerprint()
	mutateClaudeRateLimitSnapshot(cache, fp, func(snap *claudeRateLimitSnapshot) bool {
		snap.AuthWaitCredStampNs, snap.AuthWaitCredSize = stamp.modNs, stamp.size
		return true
	})
	if snap := claudeCacheSnapshot(t, cache); snap.AuthWaitCredStampNs != stamp.modNs {
		t.Fatalf("seeded wait=%d, want %d", snap.AuthWaitCredStampNs, stamp.modNs)
	}
	prevDelays := claudePendingAuthClearRetryDelays
	t.Cleanup(func() { claudePendingAuthClearRetryDelays = prevDelays })
	claudePendingAuthClearRetryDelays = nil
	claudeQueuePendingAuthClear(fp, stamp)
	if pending, _ := claudeUsageProbe.pendingAuthClear(); pending != stamp {
		t.Fatalf("pending clear=%+v, want %+v queued", pending, stamp)
	}

	stopClaudeRunDebtRetry()
	persistClaudeRunDebtForShutdown()

	snap := claudeCacheSnapshot(t, cache)
	if snap.AuthWaitCredStampNs != 0 || snap.AuthWaitCredSize != 0 {
		t.Fatalf("persisted wait=%d/%d after shutdown, want the pending clear written",
			snap.AuthWaitCredStampNs, snap.AuthWaitCredSize)
	}
	if pending, _ := claudeUsageProbe.pendingAuthClear(); !pending.isZero() {
		t.Fatalf("pending clear=%+v after shutdown, want it settled", pending)
	}
}

// A rewrite seen while no rung is pending (the previous attempt is still
// finishing and has not booked its next rung) must not use up the nudge: the
// first gather after the rung is booked still makes it due.
func TestNudgeClaudeCredentialChanged_NoPendingRungKeepsTheNudge(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, claudeProbeOKHandler)
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	owed := now.Add(-time.Minute)
	claudeOweRunRefresh(owed)
	claudeUsageProbe.noteAuthWait(claudeCredStamp{modNs: 1, size: 1})
	stopClaudeRunDebtRetry()

	rewritten := claudeCredStamp{modNs: 2, size: 1}
	nudgeClaudeCredentialChanged(rewritten)
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("a nudge with no rung pending sent %d requests, want 0", got)
	}
	if claudeRunDebtRetryPending() {
		t.Fatal("a nudge with no rung pending armed a retry")
	}

	claudeBookRunDebtRung("", owed, now, claudeRungAfter, time.Hour)
	nudgeClaudeCredentialChanged(rewritten)
	waitForClaudeCondition(t, 5*time.Second, "the rewrite seen before the rung was booked never nudged it", func() bool {
		snap, ok := loadClaudeRateLimitSnapshot(cache)
		return ok && snap.RefreshOwedAtMs == 0
	})
}

// A credential rewrite the schedule is waiting on makes the pending rung due
// at once — one attempt per new stamp, not one per gather.
func TestNudgeClaudeCredentialChanged_OneAttemptPerRewrite(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, claudeProbeOKHandler)
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	owed := now.Add(-time.Minute)
	claudeOweRunRefresh(owed)
	waitedOn := claudeCredStamp{modNs: 1, size: 1}
	claudeUsageProbe.noteAuthWait(waitedOn)
	claudeBookRunDebtRung("", owed, now, claudeRungAfter, time.Hour)

	nudgeClaudeCredentialChanged(waitedOn)
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("the credential still being waited on nudged %d requests, want 0", got)
	}

	// The real fixture file is what the attempt reads; its stamp differs from
	// the one waited on, which is exactly a rewrite.
	rewritten := claudeCredStamp{modNs: 2, size: 1}
	nudgeClaudeCredentialChanged(rewritten)
	waitForClaudeCondition(t, 5*time.Second, "the nudge never paid the debt", func() bool {
		snap, ok := loadClaudeRateLimitSnapshot(cache)
		return ok && snap.RefreshOwedAtMs == 0
	})

	// A second gather seeing the same rewrite earns nothing more.
	later := time.Now()
	claudeOweRunRefresh(later)
	claudeUsageProbe.noteAuthWait(waitedOn)
	claudeUsageProbe.takeCredentialNudge(rewritten)
	claudeBookRunDebtRung("", later, later, claudeRungAfter, time.Hour)
	nudgeClaudeCredentialChanged(rewritten)
	nudgeClaudeCredentialChanged(rewritten)
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("request count=%d, want one attempt for one rewrite", got)
	}
}

// A write the cache locks refused retries promptly for a FRESH debt (a lock
// race with its own turn's stream capture) — at the floor's end when the gate
// is spacing, else after the rung slack — and on the age backoff once the debt
// is older, so a wedged cache is not re-checked every second for hours.
func TestClaudeUnpersistedRetryDelay(t *testing.T) {
	_, _ = armClaudeUsageProbe(t, unreachableProbeHandler)
	if got := claudeUnpersistedRetryDelay(time.Second); got != claudeRunDebtRungSlack {
		t.Errorf("fresh debt, gate open: retry in %v, want the rung slack %v", got, claudeRunDebtRungSlack)
	}
	t.Setenv(claudeUsageProbeMinIntervalEnv, "5000")
	claudeUsageProbe.mu.Lock()
	claudeUsageProbe.lastAttempt = time.Now()
	claudeUsageProbe.mu.Unlock()
	if got := claudeUnpersistedRetryDelay(time.Second); got < 5*time.Second || got > 5*time.Second+2*claudeRunDebtRungSlack {
		t.Errorf("fresh debt inside the floor: retry in %v, want the floor's end", got)
	}
	if got, want := claudeUnpersistedRetryDelay(2*time.Hour), claudeFreeRetryDelay(2*time.Hour); got != want {
		t.Errorf("old debt: retry in %v, want the age backoff %v", got, want)
	}
}

// A credential rewritten before a restart makes the persisted rung due on the
// startup replay: the wait is restored from disk first, so the rewrite can
// claim its nudge instead of the future rung standing for its full delay.
func TestNudgeClaudeCredentialChanged_RestartAfterARewritePaysTheRung(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, claudeProbeOKHandler)
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	owed := now.Add(-time.Minute)
	claudeOweRunRefresh(owed)
	// A stamp the fixture credential file does not carry: it was rewritten.
	claudeUsageProbe.noteAuthWait(claudeCredStamp{modNs: 1, size: 1})
	claudeBookRunDebtRung("", owed, now, claudeRungAfter, 10*time.Minute)
	if snap, ok := loadClaudeRateLimitSnapshot(cache); !ok || snap.AuthWaitCredStampNs != 1 || snap.NextAttemptAtMs <= now.UnixMilli() {
		t.Fatalf("snap=%+v, want the wait and a future rung persisted", snap)
	}

	// Restart: nothing in memory survives.
	stopClaudeRunDebtRetry()
	resetClaudeUsageProbeGate()
	SetClaudeUsageProbeDisabled(false)

	payOwedClaudeUsageRefreshAt(time.Now())
	waitForClaudeCondition(t, 5*time.Second, "the restart left the rewritten credential's rung standing", func() bool {
		snap, ok := loadClaudeRateLimitSnapshot(cache)
		return ok && snap.RefreshOwedAtMs == 0
	})
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("request count=%d, want one attempt for the rewrite", got)
	}
}
