package main

// The full-suite ordering, reproduced inside ONE test function. The Windows
// non-race job runs every test of the package in one process, and the
// post-run freshness tests failed there while each passed alone: a previous
// run's exit path, or an attempt a reset could not drain, was still writing
// the process-wide probe gate when the next test armed it. Each pair of
// subtests below runs a straggling run first and a freshness assertion right
// after it, so a leak shows up here, in order, rather than depending on which
// file the suite happens to schedule next.

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClaudePostRunIsolation_StragglersDoNotReachTheNextRun(t *testing.T) {
	skipIfUnsupportedOS(t)

	// A run killed mid-turn never settles its turn, so its exit path makes the
	// abnormal-exit attempt — the work that used to outlive the test.
	killedRun := func(t *testing.T, start func(t *testing.T) (end func() error)) {
		cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{"five_hour":{"utilization":72,"resets_at":%d,"status":"allowed"}}`,
				time.Now().Add(3*time.Hour).Unix())
		})
		seeded := seedStaleClaudeObservation(t, cache)
		end := start(t)
		time.Sleep(200 * time.Millisecond)
		if err := end(); err != nil {
			t.Fatalf("end: %v", err)
		}
		waitForClaudeObservationAfter(t, cache, seeded)
	}

	t.Run("direct run killed mid-turn", func(t *testing.T) {
		killedRun(t, func(t *testing.T) func() error {
			m, id, _ := startNativeClaudeRun(t, "claude-heartbeat-hang")
			return func() error { return m.End(id) }
		})
	})
	t.Run("pre-run observation after the direct straggler", func(t *testing.T) {
		assertRecentPreRunObservationDoesNotSuppressPostRunProbe(t)
	})

	t.Run("managed run killed mid-turn", func(t *testing.T) {
		killedRun(t, func(t *testing.T) func() error {
			sm, id := startManagedClaudeSession(t, "claude-heartbeat-hang")
			return func() error { return sm.EndSession(id) }
		})
	})
	t.Run("pre-run observation after the managed straggler", func(t *testing.T) {
		assertRecentPreRunObservationDoesNotSuppressPostRunProbe(t)
	})

	// A pre-run probe throttles the run, so its debt is paid by the spacing
	// rung — and that rung's attempt reaches the endpoint but cannot take the
	// cache lock for its verified write, so it answers persist_failed and books
	// a budgeted rung. The bounded wait must still see the refresh land after
	// the run.
	t.Run("throttled run whose first persist is refused", func(t *testing.T) {
		var served atomic.Int64
		var unlock sync.Once
		locked := atomic.Bool{}
		release := func() {
			if locked.Load() {
				unlock.Do(unlockClaudeRateLimitCache)
			}
		}
		_, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
			if served.Add(1) == 2 {
				// The rung's attempt: hold the cache lock across its merge, and
				// give it back once the verified budget has certainly expired.
				lockClaudeRateLimitCache()
				locked.Store(true)
				time.AfterFunc(400*time.Millisecond, release)
			}
			fmt.Fprintf(w, `{"limits":[{"kind":"session","percent":63,"resets_at":%d}]}`,
				time.Now().Add(time.Hour).Unix())
		})
		t.Cleanup(release)
		prevBudget := claudeRateLimitVerifiedPersistBudget
		claudeRateLimitVerifiedPersistBudget = 100 * time.Millisecond
		t.Cleanup(func() { claudeRateLimitVerifiedPersistBudget = prevBudget })
		const interval = 500 * time.Millisecond
		t.Setenv(claudeUsageProbeMinIntervalEnv, "500")
		bound := trailingProbeRungBound(pinTrailingProbeRungLadder(t), interval)

		if !refreshClaudeUsageIfStale(t.Context(), time.Now(), time.Time{}, probeTestToken, "") {
			t.Fatal("precondition: the pre-run gather should probe")
		}
		waitPastObservationMillisecond(t, latestClaudeObservation(loadMergedClaudeRateLimitBuckets("")))

		runCompleted := time.Now()
		triggerClaudeUsageProbeAfterRun()

		waitForPostRunObservation(t, runCompleted, bound,
			"the refresh never landed after the rung's persist was refused")
		if !locked.Load() {
			t.Fatal("precondition: the rung's attempt never met the held cache lock")
		}
		// The pre-run probe, the refused rung attempt, and its retry.
		if got := atomic.LoadInt64(calls); got < 3 || got > 4 {
			t.Errorf("request count=%d, want 3 (at most 4)", got)
		}
	})

	// The run's attempt hangs at the endpoint when the agent is replaced. The
	// restart's reset cannot drain it, reports that, and cancels it; the new
	// process's replay then pays the debt with exactly one request.
	t.Run("post-update replay after an undrained restart", func(t *testing.T) {
		var served atomic.Int64
		block := make(chan struct{})
		var unblock sync.Once
		releaseHandler := func() { unblock.Do(func() { close(block) }) }
		// Registered before anything can block, so the handler is released on
		// every exit path and httptest.Server.Close cannot hang.
		cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
			if served.Add(1) == 1 {
				<-block
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			reset := time.Now().Add(3 * time.Hour).Unix()
			fmt.Fprintf(w, `{"limits":[{"kind":"session","percent":41,"resets_at":%d},`+
				`{"kind":"weekly_all","percent":17,"resets_at":%d}]}`, reset, reset+86400)
		})
		t.Cleanup(releaseHandler)
		seedStaleClaudeObservation(t, cache)
		pinClaudeDrainTimeout(t, 200*time.Millisecond)
		pinTrailingProbeRungLadder(t)

		runCompleted := time.Now()
		triggerClaudeUsageProbeAfterRun()
		waitForClaudeProbeInFlight(t)
		waitForClaudeDebt(t, cache, 10*time.Second)

		if resetClaudeUsageProbeGate() {
			t.Fatal("the restart's reset reported a drain while the run's attempt was hung at the endpoint")
		}
		releaseHandler()
		claudeFreshnessWaitIdle(t)
		if !resetClaudeUsageProbeGate() {
			t.Fatalf("the reset still could not drain once the hung attempt finished [%s]", claudeProbeStateForTest(cache))
		}
		SetClaudeUsageProbeDisabled(false)

		before := atomic.LoadInt64(calls)
		payOwedClaudeUsageRefreshAt(claudeAfterFirstRung())
		waitForClaudeCondition(t, 10*time.Second, "the replay never paid the debt the hung attempt left", func() bool {
			snap, ok := loadClaudeRateLimitSnapshot(cache)
			return ok && snap.RefreshOwedAtMs == 0
		})
		claudeFreshnessWaitIdle(t)
		if got := atomic.LoadInt64(calls) - before; got != 1 {
			t.Errorf("the replay sent %d request(s), want exactly 1 admitted", got)
		}
		requireNumericClaudeRows(t)
		row := claudePublishedSessionMetric(t)
		observed, err := time.Parse(time.RFC3339, row.ObservedAt)
		if err != nil {
			t.Fatalf("published observedAt %q: %v", row.ObservedAt, err)
		}
		if observed.Before(runCompleted.Truncate(time.Millisecond)) {
			t.Errorf("published observedAt %s is before the run %s", row.ObservedAt, runCompleted.UTC().Format(time.RFC3339Nano))
		}
		requireClaudeResetDrained(t)
	})
}
