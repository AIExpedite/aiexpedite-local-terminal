package main

// The Claude run-refresh retry ladder (cliagent_usage_claudecode_refresh_schedule.go):
// the rung each kind books, the request budget, the single process-wide timer,
// and its cancellation. Every case runs against an httptest stand-in pinned
// through AIEXPEDITE_CLAUDE_USAGE_PROBE_URL, so nothing reaches api.anthropic.com.
// Ladders are pinned small where the timer must actually fire; the pins are
// package vars restored in cleanup, as in the Antigravity suite, so these cases
// do not call t.Parallel().

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// pinClaudeRunDebtLadder swaps the ladder and the free rung for the test.
func pinClaudeRunDebtLadder(t *testing.T, ladder []time.Duration, free time.Duration) {
	t.Helper()
	origLadder, origFree := claudeRunDebtRetryLadder, claudeRunDebtFreeRetryDelay
	t.Cleanup(func() { claudeRunDebtRetryLadder, claudeRunDebtFreeRetryDelay = origLadder, origFree })
	claudeRunDebtRetryLadder, claudeRunDebtFreeRetryDelay = ladder, free
}

// waitForClaudeCalls polls until the stand-in has served at least `want`
// requests.
func waitForClaudeCalls(t *testing.T, calls *int64, want int64, within time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(within); time.Now().Before(deadline); {
		if atomic.LoadInt64(calls) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("request count=%d after %s, want >= %d", atomic.LoadInt64(calls), within, want)
}

func TestClaudeRetryDelayForAttempt_Ladder(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
		ok       bool
	}{
		{0, time.Minute, true},
		{1, time.Minute, true},
		{2, 2 * time.Minute, true},
		{3, 5 * time.Minute, true},
		{4, 0, false},
		{9, 0, false},
	}
	for _, c := range cases {
		got, ok := claudeRetryDelayForAttempt(c.attempts)
		if got != c.want || ok != c.ok {
			t.Errorf("attempts=%d: got (%s, %v), want (%s, %v)", c.attempts, got, ok, c.want, c.ok)
		}
	}
	// The whole ladder sits well inside the debt's age-out, so only a long hold
	// or a free backoff can run into it.
	total := time.Duration(0)
	for _, rung := range claudeRunDebtRetryLadder {
		total += rung
	}
	if total >= claudeRefreshOwedMaxAge {
		t.Errorf("ladder sums to %s, want well inside the %s age-out", total, claudeRefreshOwedMaxAge)
	}
	if claudeRunDebtRetryLadder[0] != claudeUsageProbeMinInterval {
		t.Errorf("first rung %s, want the minimum interval %s so it cannot fire into it",
			claudeRunDebtRetryLadder[0], claudeUsageProbeMinInterval)
	}
}

// Each kind books its own rung, and a rung the age-out would overtake retires
// the debt instead.
func TestClaudeRunDebtRetryAt_EachKindBooksItsRung(t *testing.T) {
	// Millisecond-aligned, as every persisted instant is.
	now := time.Now().Truncate(time.Millisecond)
	slack := claudeUsageProbeTrailingSlack
	snapFor := func(owed time.Time, attempts int, heldUntil time.Time) *claudeRateLimitSnapshot {
		snap := &claudeRateLimitSnapshot{RefreshOwedAtMs: owed.UnixMilli(), RefreshOwedAttempts: attempts}
		if !heldUntil.IsZero() {
			snap.HeldUntilMs = heldUntil.UnixMilli()
		}
		return snap
	}
	owed := now.Add(-10 * time.Second)

	cases := []struct {
		name     string
		kind     claudeRunDebtRetryKind
		snap     *claudeRateLimitSnapshot
		owed     time.Time
		gateNext time.Time
		want     time.Time
		ok       bool
	}{
		{"after a request, first rung", claudeRetryAfterRequest, snapFor(owed, 1, time.Time{}), owed, now,
			now.Add(time.Minute + slack), true},
		{"after a request, third rung", claudeRetryAfterRequest, snapFor(owed, 3, time.Time{}), owed, now,
			now.Add(5*time.Minute + slack), true},
		{"after a request, budget spent", claudeRetryAfterRequest, snapFor(owed, claudeRefreshDebtMaxAttempts, time.Time{}), owed, now,
			time.Time{}, false},
		{"after a request, never inside the failure backoff", claudeRetryAfterRequest, snapFor(owed, 1, time.Time{}), owed,
			now.Add(4 * time.Minute), now.Add(4*time.Minute + slack), true},
		{"spacing waits the free rung plus the interval left", claudeRetrySpacing, snapFor(owed, 1, time.Time{}), owed,
			now.Add(20 * time.Second), now.Add(claudeRunDebtFreeRetryDelay + 20*time.Second + slack), true},
		{"held waits the hold out", claudeRetryHeld, snapFor(owed, 1, now.Add(3*time.Minute)), owed, now,
			now.Add(3*time.Minute + slack), true},
		{"held past the age-out retires", claudeRetryHeld, snapFor(now.Add(-25*time.Minute), 1, now.Add(10*time.Minute)),
			now.Add(-25 * time.Minute), now, time.Time{}, false},
		{"free floors at the free rung", claudeRetryFree, snapFor(owed, 0, time.Time{}), owed, now,
			now.Add(claudeRunDebtFreeRetryDelay + slack), true},
		{"free backs off with the debt's age", claudeRetryFree, snapFor(now.Add(-3*time.Minute), 0, time.Time{}),
			now.Add(-3 * time.Minute), now, now.Add(3*time.Minute + slack), true},
		{"free is capped at the longest rung", claudeRetryFree, snapFor(now.Add(-20*time.Minute), 0, time.Time{}),
			now.Add(-20 * time.Minute), now, now.Add(5*time.Minute + slack), true},
	}
	for _, c := range cases {
		got, ok := claudeRunDebtRetryAt(c.kind, c.snap, c.owed, c.gateNext, now)
		if ok != c.ok || !got.Equal(c.want) {
			t.Errorf("%s: got (%v, %v), want (%v, %v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

// A booked rung is persisted beside the debt and armed as the one timer.
func TestClaudeScheduleRunDebtRetry_PersistsTheRungAndArmsTheTimer(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	now := time.Now()
	owed := now.Add(-time.Second)
	seedClaudeRefreshDebt(t, cache, fp, owed, 1, time.Time{})

	if !claudeScheduleRunDebtRetry(fp, owed, claudeRetryAfterRequest, now) {
		t.Fatal("a kept debt with budget left must book a rung")
	}
	snap := claudeCacheSnapshot(t, cache)
	if want := now.Add(claudeRunDebtRetryLadder[0] + claudeUsageProbeTrailingSlack).UnixMilli(); snap.NextAttemptAtMs != want {
		t.Fatalf("NextAttemptAtMs=%d, want the first rung %d", snap.NextAttemptAtMs, want)
	}
	if !claudeRunDebtRetryPending() {
		t.Fatal("no timer armed for the booked rung")
	}
	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("booking a rung sent %d requests, want 0", got)
	}
}

// The budget stops at claudeRefreshDebtMaxAttempts requests: a debt that has
// spent it is retired rather than booked again.
func TestClaudeScheduleRunDebtRetry_BudgetStopsAtFourRequests(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	now := time.Now()
	owed := now.Add(-time.Second)
	seedClaudeRefreshDebt(t, cache, fp, owed, claudeRefreshDebtMaxAttempts, time.Time{})

	if claudeScheduleRunDebtRetry(fp, owed, claudeRetryAfterRequest, now) {
		t.Fatal("a debt that spent its budget must not book another rung")
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 || snap.NextAttemptAtMs != 0 {
		t.Fatalf("a spent debt must be retired: %+v", snap)
	}
	if claudeRunDebtRetryPending() {
		t.Fatal("a retired debt left a timer armed")
	}
}

// End to end against a stand-in that always fails: the trailing attempt plus
// the ladder's rungs spend exactly the budget, then the debt retires.
func TestClaudeRunDebtLadder_SpendsExactlyTheBudgetThenRetires(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	pinClaudeRunDebtLadder(t, []time.Duration{20 * time.Millisecond}, 20*time.Millisecond)
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))

	triggerClaudeUsageProbeAfterRun()
	waitForClaudeCalls(t, calls, claudeRefreshDebtMaxAttempts, 10*time.Second)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if snap, ok := loadClaudeRateLimitSnapshot(cache); ok && snap.RefreshOwedAtMs == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Give a stray extra rung the chance to show itself before counting.
	time.Sleep(150 * time.Millisecond)
	claudeFreshnessWaitIdle(t)

	if got := atomic.LoadInt64(calls); got != claudeRefreshDebtMaxAttempts {
		t.Fatalf("request count=%d, want exactly the budget %d", got, claudeRefreshDebtMaxAttempts)
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 || snap.NextAttemptAtMs != 0 {
		t.Fatalf("an exhausted debt must be retired: %+v", snap)
	}
	if claudeRunDebtRetryPending() {
		t.Fatal("an exhausted debt left a timer armed")
	}
}

// A burst of settlements keeps ONE timer: every booking replaces the pending
// one, so the rung fires once.
func TestClaudeScheduleRunDebtRetry_ABurstArmsOneTimer(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	pinClaudeRunDebtLadder(t, []time.Duration{80 * time.Millisecond}, 80*time.Millisecond)
	fp := currentClaudeAccountFingerprint()
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))
	owed := time.Now().Add(-time.Second)
	// One attempt left, so whatever fires books nothing further and the count
	// below is exactly the number of rungs that fired.
	seedClaudeRefreshDebt(t, cache, fp, owed, claudeRefreshDebtMaxAttempts-1, time.Time{})

	for i := 0; i < 8; i++ {
		claudeScheduleRunDebtRetry(fp, owed, claudeRetryAfterRequest, time.Now())
	}
	waitForClaudeCalls(t, calls, 1, 5*time.Second)
	time.Sleep(300 * time.Millisecond)
	claudeFreshnessWaitIdle(t)

	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("request count=%d, want 1 — a burst must coalesce onto one timer", got)
	}
}

// A newer run's debt is never booked by a pass judging an older one.
func TestClaudeScheduleRunDebtRetry_NeverBooksANewerDebt(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	older := time.Now().Add(-time.Minute)
	claudeOweRunRefresh(older)
	newer := time.Now()
	claudeOweRunRefresh(newer)

	if claudeScheduleRunDebtRetry(fp, older, claudeRetryAfterRequest, time.Now()) {
		t.Fatal("a pass for a replaced debt booked a rung")
	}
	snap := claudeCacheSnapshot(t, cache)
	if snap.NextAttemptAtMs != 0 || snap.RefreshOwedAtMs != newer.UnixMilli() {
		t.Fatalf("the newer debt was touched: %+v", snap)
	}
	if claudeRunDebtRetryPending() {
		t.Fatal("a refused booking armed a timer")
	}
}

// A timer generation that was replaced is ignored when it fires.
func TestClaudeRunDebtRetryFired_IgnoresAStaleGeneration(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	seedClaudeRefreshDebt(t, cache, fp, time.Now().Add(-time.Second), 1, time.Time{})

	claudeArmRunDebtRetry(time.Hour)
	claudeRunDebtRetryTimer.mu.Lock()
	stale := claudeRunDebtRetryTimer.gen
	claudeRunDebtRetryTimer.mu.Unlock()
	claudeArmRunDebtRetry(time.Hour)

	claudeRunDebtRetryFired(stale)
	claudeFreshnessWaitIdle(t)
	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("a stale generation sent %d requests", got)
	}
	if !claudeRunDebtRetryPending() {
		t.Fatal("a stale generation cleared the live timer")
	}
}

// stopClaudeRunDebtRetry cancels the pending rung; the debt itself stays on
// disk for the next process.
func TestStopClaudeRunDebtRetry_CancelsThePendingRung(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	pinClaudeRunDebtLadder(t, []time.Duration{50 * time.Millisecond}, 50*time.Millisecond)
	fp := currentClaudeAccountFingerprint()
	owed := time.Now().Add(-time.Second)
	seedClaudeRefreshDebt(t, cache, fp, owed, 1, time.Time{})

	if !claudeScheduleRunDebtRetry(fp, owed, claudeRetryAfterRequest, time.Now()) {
		t.Fatal("precondition: the rung was not booked")
	}
	stopClaudeRunDebtRetry()
	time.Sleep(200 * time.Millisecond)
	claudeFreshnessWaitIdle(t)

	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("a cancelled rung sent %d requests", got)
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != owed.UnixMilli() || snap.NextAttemptAtMs == 0 {
		t.Fatalf("stopping the timer must leave the persisted debt and rung alone: %+v", snap)
	}
}

// A process that is shutting down books nothing; gracefulShutdown stops the
// pending rung before it hands off.
func TestClaudeScheduleRunDebtRetry_ShutdownCancels(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	owed := time.Now().Add(-time.Second)
	seedClaudeRefreshDebt(t, cache, fp, owed, 1, time.Time{})

	shutdownInProgress.Store(true)
	t.Cleanup(func() { shutdownInProgress.Store(false) })
	if claudeScheduleRunDebtRetry(fp, owed, claudeRetryAfterRequest, time.Now()) {
		t.Fatal("a shutting-down process booked a rung")
	}
	shutdownInProgress.Store(false)

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "shutdown.go", nil, 0)
	if err != nil {
		t.Fatalf("parse shutdown.go: %v", err)
	}
	stops := false
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "gracefulShutdown" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "stopClaudeRunDebtRetry" {
					stops = true
				}
			}
			return true
		})
		return false
	})
	if !stops {
		t.Fatal("gracefulShutdown no longer stops the Claude refresh rung; it could fire into a process handing off to an update")
	}
}

// The gate reset — the test drain, and the in-process stand-in for a handoff —
// stops the timer too.
func TestResetClaudeUsageProbeGate_StopsThePendingRung(t *testing.T) {
	armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	claudeArmRunDebtRetry(time.Hour)
	resetClaudeUsageProbeGate()
	if claudeRunDebtRetryPending() {
		t.Fatal("resetClaudeUsageProbeGate left the rung armed")
	}
}

// A rung that fires after its debt was paid elsewhere (a Refresh click, a
// gather) sends nothing.
func TestClaudeRunDebtRetryFired_PaidDebtSendsNothing(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))

	claudeArmRunDebtRetry(time.Hour)
	claudeRunDebtRetryTimer.mu.Lock()
	gen := claudeRunDebtRetryTimer.gen
	claudeRunDebtRetryTimer.mu.Unlock()
	claudeRunDebtRetryFired(gen)
	claudeFreshnessWaitIdle(t)

	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("a rung with no debt left sent %d requests", got)
	}
	if claudeRunDebtRetryPending() {
		t.Fatal("a rung with no debt left re-armed itself")
	}
}

// A Refresh click inside a 429 hold does not override it: nothing is sent, and
// the log says the card is held rather than leaving a stale card unexplained.
func TestClaudeForcedRefreshDuringAHoldLogsHeld(t *testing.T) {
	_, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	claudeUsageProbe.holdUntil(time.Now().Add(time.Hour))

	out := captureStdout(t, func() {
		refreshClaudeUsageIfStale(WithClaudeUsageForceProbe(context.Background()), time.Now(), time.Time{},
			probeTestToken, currentClaudeAccountFingerprint())
	})
	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("a forced refresh inside a hold sent %d requests", got)
	}
	if !strings.Contains(out, "refresh outcome=held") {
		t.Fatalf("a forced refresh inside a hold did not log held: %q", out)
	}
}

// A rung pays the PERSISTED (millisecond) instant of a debt the gate recorded at
// nanosecond precision; paying it must clear the gate's debt too, or the paid
// run is still "owed" in memory.
func TestClaudeRunDebtRung_PayingTheDebtClearsTheGate(t *testing.T) {
	resets := time.Now().Add(time.Hour)
	var fail atomic.Bool
	fail.Store(true)
	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprintf(w, `{"limits":[{"kind":"session","percent":5,"resets_at":%d}]}`, resets.Unix())
	})
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))

	triggerClaudeUsageProbeAfterRun()
	claudeFreshnessWaitIdle(t)
	snap := claudeCacheSnapshot(t, cache)
	if snap.NextAttemptAtMs == 0 {
		t.Fatalf("precondition: no rung booked: %+v", snap)
	}
	stopClaudeRunDebtRetry()

	fail.Store(false)
	payOwedClaudeUsageRefreshAt(time.UnixMilli(snap.NextAttemptAtMs))
	claudeFreshnessWaitIdle(t)

	if owed := claudeUsageProbe.owedObservation(); !owed.IsZero() {
		t.Fatalf("the paid debt is still on the gate: %v", owed)
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("the paid debt is still on disk: %+v", snap)
	}
}

// Two agent processes that armed the same persisted rung both judge it due on
// an unlocked read. The locked claim lets exactly one of them charge: the other
// finds the counter and the rung moved, and is told it lost rather than that
// the write was dropped — so it neither charges, sends, nor books a rung.
func TestClaimClaudeRefreshRungAt_OnlyOneProcessClaimsARung(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	now := time.Now()
	owed := now.Add(-2 * time.Minute)
	seedClaudeRefreshDebt(t, cache, fp, owed, 1, time.Time{})
	due := now.Add(-time.Second).UnixMilli()
	if !mutateClaudeRateLimitSnapshot(cache, fp, func(s *claudeRateLimitSnapshot) bool { s.NextAttemptAtMs = due; return true }) {
		t.Fatal("seeding the rung did not write the cache")
	}
	lease := now.Add(time.Minute).UnixMilli()

	// Both judged {attempts: 1, next: due}; the first one to the lock wins.
	var lostA, lostB bool
	if !mutateClaudeRateLimitSnapshot(cache, fp, claimClaudeRefreshRungAt(owed, 1, due, lease, &lostA)) || lostA {
		t.Fatalf("the first claim must land (lost=%v)", lostA)
	}
	if mutateClaudeRateLimitSnapshot(cache, fp, claimClaudeRefreshRungAt(owed, 1, due, lease, &lostB)) {
		t.Fatal("a second process claimed a rung already claimed — two requests at one ladder instant")
	}
	if !lostB {
		t.Fatal("the losing claim must report lost, or it would book a rung of its own")
	}
	snap := claudeCacheSnapshot(t, cache)
	if snap.RefreshOwedAttempts != 2 || snap.NextAttemptAtMs != lease {
		t.Fatalf("want one charge and the in-flight lease, got %+v", snap)
	}

	// A refusal that is not a lost race stays distinguishable: a newer debt, or
	// a debt already at the cap.
	var lostC bool
	if mutateClaudeRateLimitSnapshot(cache, fp, claimClaudeRefreshRungAt(owed.Add(time.Second), 2, lease, lease, &lostC)) || lostC {
		t.Fatalf("a claim on another debt instant must refuse without reporting lost (lost=%v)", lostC)
	}
	seedClaudeRefreshDebt(t, cache, fp, owed, claudeRefreshDebtMaxAttempts, time.Time{})
	var lostD bool
	if mutateClaudeRateLimitSnapshot(cache, fp, claimClaudeRefreshRungAt(owed, claudeRefreshDebtMaxAttempts, lease, lease, &lostD)) || lostD {
		t.Fatalf("a claim past the cap must refuse without reporting lost (lost=%v)", lostD)
	}
}

// While one process's claimed attempt is in flight, a replay that reads the
// file finds the lease in the future: it arms its timer for it and sends and
// charges nothing.
func TestPayOwedClaudeUsageRefresh_InFlightLeaseSendsNothingElse(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusBadGateway)
	})
	fp := currentClaudeAccountFingerprint()
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))
	seedClaudeRefreshDebt(t, cache, fp, time.Now().Add(-time.Minute), 1, time.Time{})

	done := make(chan struct{})
	go func() { defer close(done); payOwedClaudeUsageRefreshAt(time.Now()) }()
	waitForClaudeCalls(t, calls, 1, 5*time.Second)

	snap := claudeCacheSnapshot(t, cache)
	if snap.RefreshOwedAttempts != 2 || snap.NextAttemptAtMs <= time.Now().UnixMilli() {
		t.Fatalf("the in-flight attempt must hold a charge and a future lease: %+v", snap)
	}
	payOwedClaudeUsageRefreshAt(time.Now())
	if got := claudeCacheSnapshot(t, cache).RefreshOwedAttempts; got != 2 {
		t.Fatalf("attempts=%d, want 2 — a replay during the lease charged the budget", got)
	}
	if !claudeRunDebtRetryPending() {
		t.Fatal("a replay that found the lease must arm its timer for it")
	}

	close(release)
	<-done
	claudeFreshnessWaitIdle(t)
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("request count=%d, want 1", got)
	}
}

// seedClaudeBookedRung records a debt at `owed` with `attempts` spent and a
// rung booked for `next`, behind a reading taken before the run but inside the
// staleness TTL — so only the debt's `owing` override could send a request.
func seedClaudeBookedRung(t *testing.T, cache, fp string, latest, owed time.Time, attempts int, next time.Time) {
	t.Helper()
	seedClaudeProbeReading(t, cache, latest)
	seedClaudeRefreshDebt(t, cache, fp, owed, attempts, time.Time{})
	mutateClaudeRateLimitSnapshot(cache, fp, func(snap *claudeRateLimitSnapshot) bool {
		snap.NextAttemptAtMs = next.UnixMilli()
		return true
	})
}

// After a restart that re-armed a future rung, a routine gather does not pay
// the debt the replay handed the gate ahead of the ladder; once the rung is
// due it does.
func TestClaudeGather_WaitsForTheReplayedRung(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	now := time.Now()
	latest, owed, next := now.Add(-2*time.Minute), now.Add(-time.Minute), now.Add(2*time.Minute)
	seedClaudeBookedRung(t, cache, fp, latest, owed, 1, next)

	simulateClaudeAgentRestart(t)
	payOwedClaudeUsageRefreshAt(now)
	claudeFreshnessWaitIdle(t)
	refreshClaudeUsageIfStale(context.Background(), now, latest, probeTestToken, fp)
	claudeFreshnessWaitIdle(t)

	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("request count=%d, want 0 — the gather paid the debt before its rung", got)
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAttempts != 1 || snap.NextAttemptAtMs != next.UnixMilli() {
		t.Fatalf("the gather charged or moved the booked rung: %+v", snap)
	}

	due := next.Add(time.Second)
	refreshClaudeUsageIfStale(context.Background(), due, latest, probeTestToken, fp)
	claudeFreshnessWaitIdle(t)
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("request count=%d, want 1 once the rung was due", got)
	}
}

// Without the replay — a gather of a fresh process (or of another agent
// channel) reaching the seed first — a persisted future rung is neither adopted
// nor charged.
func TestClaudeGather_SeedDoesNotAdoptADebtWithAFutureRung(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	now := time.Now()
	latest, owed, next := now.Add(-2*time.Minute), now.Add(-time.Minute), now.Add(2*time.Minute)
	seedClaudeBookedRung(t, cache, fp, latest, owed, 1, next)

	simulateClaudeAgentRestart(t)
	refreshClaudeUsageIfStale(context.Background(), now, latest, probeTestToken, fp)
	claudeFreshnessWaitIdle(t)

	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("request count=%d, want 0 — the seed paid the debt before its rung", got)
	}
	if got := claudeCacheSnapshot(t, cache).RefreshOwedAttempts; got != 1 {
		t.Fatalf("attempts=%d, want 1 — the seed charged a debt the ladder owns", got)
	}
	if got := claudeUsageProbe.owedObservation(); !got.IsZero() {
		t.Fatalf("the seed adopted a debt whose rung is not due: %v", got)
	}
}

// Retiring a spent debt clears the gate's copy too, so later routine gathers
// stop sending uncharged requests for it.
func TestClaudeScheduleRunDebtRetry_RetiringClearsTheGateDebt(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	fp := currentClaudeAccountFingerprint()
	now := time.Now()
	latest, owed := now.Add(-2*time.Minute), now.Add(-time.Minute)
	seedClaudeProbeReading(t, cache, latest)
	seedClaudeRefreshDebt(t, cache, fp, owed, claudeRefreshDebtMaxAttempts, time.Time{})
	claudeUsageProbe.recordOwed(owed)

	if claudeScheduleRunDebtRetry(fp, owed, claudeRetryAfterRequest, now) {
		t.Fatal("a debt that spent its budget must not book another rung")
	}
	if got := claudeUsageProbe.owedObservation(); !got.IsZero() {
		t.Fatalf("gate debt=%v after retirement, want none", got)
	}
	refreshClaudeUsageIfStale(context.Background(), now, latest, probeTestToken, fp)
	claudeFreshnessWaitIdle(t)
	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("request count=%d, want 0 — a gather paid a retired debt", got)
	}
}
