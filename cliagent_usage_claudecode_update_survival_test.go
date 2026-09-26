package main

// The acceptance criterion for "Claude Code utilization capture remains stale
// after update": observedAt must ADVANCE and must SURVIVE the agent replacing
// itself. Mirrors cliagent_usage_codex_update_survival_test.go — the cache file
// is the only thing that carries over a restart, so everything else is reset.

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// simulateClaudeAgentRestart discards every in-memory trace of the previous
// process — the single-flight latch, the throttle, the debt, the 429 hold — and
// arms a fresh one, exactly as StartAgent does after a self-update. The pinned
// cache file (AIEXPEDITE_CLAUDE_RL_CACHE, still set by armClaudeUsageProbe) is
// all that survives.
func simulateClaudeAgentRestart(t *testing.T) {
	t.Helper()
	resetClaudeUsageProbeGate()
	SetClaudeUsageProbeDisabled(false)
}

// claudeObservedAt is the stalest row the card would show, which is what the
// ticket's "observedAt" refers to.
func claudeObservedAt(t *testing.T, now time.Time) time.Time {
	t.Helper()
	return claudeSnapshotFreshness(loadMergedClaudeRateLimitView(currentClaudeAccountFingerprint()), now)
}

// A run finished and recorded its debt, then the agent was replaced before the
// trailing probe could pay it. The new process's startup replay pays it:
// observedAt advances past the run and the debt clears.
func TestClaudeOwedRefresh_SurvivesAgentRestart(t *testing.T) {
	now := time.Now()
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"limits":[{"kind":"session","percent":52,"resets_at":%d}]}`,
			now.Add(time.Hour).Unix())
	})
	// A pre-update reading: recent enough that an unaware gather would trust it
	// for the whole claudeUsageProbeStaleAfter window.
	preRun := now.Add(-time.Second)
	seedClaudeProbeReading(t, cache, preRun)

	// The run finishes, its debt is mirrored to disk, and the process dies
	// before the trailing probe lands.
	runEnded := now
	claudeOweRunRefresh(runEnded)
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != runEnded.UnixMilli() {
		t.Fatalf("the run's debt was not persisted: %+v", snap)
	}

	simulateClaudeAgentRestart(t)
	payOwedClaudeUsageRefreshAt(time.Now())
	claudeFreshnessWaitIdle(t)

	if atomic.LoadInt64(calls) != 1 {
		t.Fatalf("request count=%d, want exactly 1 replay", atomic.LoadInt64(calls))
	}
	if got := claudeObservedAt(t, time.Now()); !got.After(preRun) || got.Before(runEnded) {
		t.Fatalf("observedAt %v did not advance past the run %v (pre-update reading %v)", got, runEnded, preRun)
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("replayed debt not cleared: %+v", snap)
	}
}

// A smoke that spent a turn just before the update owes exactly like a run, and
// is paid by the same replay.
func TestClaudeOwedRefresh_SmokeDebtSurvivesAgentRestart(t *testing.T) {
	now := time.Now()
	// Refuse while the smoke's own trailing probe runs, so the debt it records
	// is still standing when the simulated self-update lands — that is the case
	// under test. The replay afterwards gets a real answer.
	var refuse atomic.Bool
	refuse.Store(true)
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		if refuse.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"limits":[{"kind":"session","percent":61,"resets_at":%d}]}`,
			now.Add(time.Hour).Unix())
	})
	preSmoke := now.Add(-time.Second)
	seedClaudeProbeReading(t, cache, preSmoke)

	settleOrDisarmClaudeSmokeRun(claudeSmokeUsageEvidence{markerSeen: true})
	waitForClaudeDebt(t, cache, 5*time.Second)
	claudeFreshnessWaitIdle(t)
	smokeDebt := claudeCacheSnapshot(t, cache).RefreshOwedAtMs
	if smokeDebt == 0 {
		t.Fatal("a smoke that reached inference must owe a refresh")
	}
	before := atomic.LoadInt64(calls)

	refuse.Store(false)
	simulateClaudeAgentRestart(t)
	payOwedClaudeUsageRefreshAt(time.Now())
	claudeFreshnessWaitIdle(t)

	if got := atomic.LoadInt64(calls) - before; got != 1 {
		t.Fatalf("replay spent %d requests, want exactly 1", got)
	}
	if got := claudeObservedAt(t, time.Now()); got.Before(time.UnixMilli(smokeDebt)) {
		t.Fatalf("observedAt %v still predates the smoke turn %v", got, time.UnixMilli(smokeDebt))
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("the smoke's debt was not settled by the replay: %+v", snap)
	}
}

// The replay is exactly ONE attempt when it finds nothing; the remainder is
// left to the next run, refresh or routine gather under the ordinary bounds.
func TestClaudeOwedRefresh_RestartReplayIsExactlyOneAttempt(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	claudeOweRunRefresh(now.Add(-time.Minute))

	simulateClaudeAgentRestart(t)
	payOwedClaudeUsageRefreshAt(now)
	claudeFreshnessWaitIdle(t)

	if atomic.LoadInt64(calls) != 1 {
		t.Fatalf("replay spent %d requests, want exactly 1", atomic.LoadInt64(calls))
	}
	snap := claudeCacheSnapshot(t, cache)
	if snap.RefreshOwedAtMs == 0 {
		t.Fatal("an unpaid debt must survive the replay")
	}
	if snap.RefreshOwedAttempts != 1 {
		t.Fatalf("RefreshOwedAttempts=%d, want exactly 1", snap.RefreshOwedAttempts)
	}
}

// Repeated restarts retire the debt at the attempt cap rather than issuing one
// request per start for the whole of claudeRefreshOwedMaxAge.
func TestClaudeOwedRefresh_RepeatedRestartsRetireAtTheAttemptCap(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	claudeOweRunRefresh(now.Add(-time.Minute))

	for i := 0; i < claudeUsageProbeAfterRunMaxAttempts+3; i++ {
		simulateClaudeAgentRestart(t)
		payOwedClaudeUsageRefreshAt(now)
		claudeFreshnessWaitIdle(t)
	}

	if atomic.LoadInt64(calls) != claudeUsageProbeAfterRunMaxAttempts {
		t.Fatalf("request count=%d, want the cap %d", atomic.LoadInt64(calls), claudeUsageProbeAfterRunMaxAttempts)
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("a debt at the attempt cap must be retired, paid or not: %+v", snap)
	}
}

// A debt past claudeRefreshOwedMaxAge is neither replayed nor rewritten.
func TestClaudeOwedRefresh_ExpiredDebtIsNotReplayed(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-2*time.Hour))
	claudeOweRunRefresh(now.Add(-claudeRefreshOwedMaxAge - 10*time.Minute))

	simulateClaudeAgentRestart(t)
	payOwedClaudeUsageRefreshAt(now)
	claudeFreshnessWaitIdle(t)

	if atomic.LoadInt64(calls) != 0 {
		t.Fatalf("an expired debt must not spend a request (%d sent)", atomic.LoadInt64(calls))
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("an expired debt must be retired: %+v", snap)
	}
}

// PINNED AS THE ACCEPTED GAP, not as coverage. A process killed MID-turn
// persists nothing (this design records the debt only when the turn returns),
// so the next start finds no debt and issues no request. Anyone later adding a
// persisted active-run floor will see this expectation change deliberately.
func TestClaudeOwedRefresh_TurnKilledMidFlightLeavesNothingToReplay(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	before, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}

	// The run started but never returned: nothing called claudeOweRunRefresh.
	simulateClaudeAgentRestart(t)
	payOwedClaudeUsageRefreshAt(now)
	claudeFreshnessWaitIdle(t)

	if atomic.LoadInt64(calls) != 0 {
		t.Fatalf("with no persisted debt the replay must issue nothing (%d sent)", atomic.LoadInt64(calls))
	}
	after, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a replay with nothing to pay must leave the cache byte-identical")
	}
}

// waitForClaudeDebt polls until a debt appears on the cache. The owe is
// persisted from the goroutine triggerClaudeUsageProbeAfterRun spawns, so
// polling is the only honest way to observe it.
func waitForClaudeDebt(t *testing.T, cache string, within time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(within); time.Now().Before(deadline); {
		if snap, ok := loadClaudeRateLimitSnapshot(cache); ok && snap.RefreshOwedAtMs != 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no owed-refresh debt reached the cache")
}
