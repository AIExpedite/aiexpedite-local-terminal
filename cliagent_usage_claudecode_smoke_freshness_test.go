package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_claudecode_smoke_freshness_test.go — the acceptance regression.
   --------------------------------------------------------------------------
   The field report: a Claude Code smoke PASSED during CLI maintenance on a
   Windows device, and the three Claude rows on the CLI Agents card kept their
   pre-smoke observedAt — still stale after the 90s refresh and the 20s retry
   that followed. Two causes, both covered here:
     1. runCLISmoke spent a real turn and recorded no freshness obligation at
        all, so nothing ever asked for a post-smoke reading;
     2. the obligation the OTHER run paths do record lived only in process
        memory, so the agent restart in the middle of the CLI-maintenance
        window discarded it too.
   Each test states the user-visible claim, not the mechanism.
   ------------------------------------------------------------------------ */

// smokeFreshnessEnv arms everything a smoke + gather needs: the isolated cache
// and pending-run record, a stubbed binary and auth probe, and an OAuth probe
// endpoint whose request count is observable.
func smokeFreshnessEnv(t *testing.T, handler http.HandlerFunc) (cache string, requests *int64) {
	t.Helper()
	cache, requests = armClaudeUsageProbe(t, handler)
	smokeEnvOver(t, cache)
	path := stubClaudeBinary(t)
	stubSmokePath(t, path)
	seedProbeVersion(t, path, "2.1.251 (Claude Code)")
	stubAuthProbe(t, true, true)
	return cache, requests
}

// smokeEnvOver is smokeEnv's isolation WITHOUT its cache override: the probe
// harness already owns the cache path, and this test needs the smoke and the
// probe writing into the SAME one.
func smokeEnvOver(t *testing.T, cache string) {
	t.Helper()
	t.Setenv("AIEXPEDITE_CLAUDE_STATUSLINE_PREV", t.TempDir()+"/prev.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	resetClaudeSmokeState()
	resetVersionProbeCache()
	t.Cleanup(func() {
		resetClaudeSmokeState()
		resetVersionProbeCache()
	})
}

// seedPreSmokeObservations puts a reading on each of the three DISPLAYED rows —
// five-hour, weekly and weekly-Fable — taken well before the smoke, and returns
// that instant. This is the exact state the report described: three metrics
// pre-dating the smoke.
func seedPreSmokeObservations(t *testing.T, cache string) time.Time {
	t.Helper()
	seeded := time.Now().Add(-2 * time.Hour)
	reset := time.Now().Add(3 * time.Hour).UnixMilli()
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 21, ResetsAtMs: reset, ObservedAtMs: seeded.UnixMilli(),
			Status: "allowed", usageKnown: true,
		},
		claudeWindowSevenDay: {
			UsedPercentage: 33, ResetsAtMs: reset, ObservedAtMs: seeded.UnixMilli(),
			Status: "allowed", usageKnown: true,
		},
		claudeWindowSevenDayFable: {
			UsedPercentage: 8, ResetsAtMs: reset, ObservedAtMs: seeded.UnixMilli(),
			Status: "allowed", usageKnown: true,
		},
	}, seeded, "", claudeRateLimitSourceStream)
	return seeded
}

// smokeEnvelopeWithRateLimits is what a healthy `claude --print --output-format
// json` returns on a subscription login: the marker echo plus the account's own
// rate_limits map.
func smokeEnvelopeWithRateLimits(marker string, fiveHour, weekly, fable float64) []byte {
	reset := time.Now().Add(3 * time.Hour).Unix()
	payload, _ := json.Marshal(map[string]any{
		"type": "result", "subtype": "success", "is_error": false,
		"result": marker, "num_turns": 1,
		"rate_limits": map[string]any{
			claudeWindowFiveHour:      map[string]any{"used_percentage": fiveHour, "resets_at": reset, "status": "allowed"},
			claudeWindowSevenDay:      map[string]any{"used_percentage": weekly, "resets_at": reset, "status": "allowed"},
			claudeWindowSevenDayFable: map[string]any{"used_percentage": fable, "resets_at": reset, "status": "allowed"},
		},
	})
	return payload
}

// assertEveryRowObservedAtOrAfter is the acceptance criterion itself: EVERY row
// the card displays must carry an observation at or after the run start. Asked
// per row, not as "the newest reading anywhere" — the original failure was three
// rows behind while the cache as a whole looked recent.
func assertEveryRowObservedAtOrAfter(t *testing.T, runStart time.Time) {
	t.Helper()
	now := time.Now()
	metrics := claudeCodeMetricsFromCache(now, "")
	if len(metrics) != 3 {
		t.Fatalf("card shows %d rows, want the 3 the report named", len(metrics))
	}
	for _, m := range metrics {
		observed := observedAtMsFromRFC3339(m.ObservedAt)
		if observed == 0 {
			t.Errorf("row %q carries no observation at all", m.Label)
			continue
		}
		if observed < runStart.Truncate(time.Millisecond).UnixMilli() {
			t.Errorf("row %q observedAt=%s still predates the run start %s",
				m.Label, m.ObservedAt, runStart.UTC().Format(time.RFC3339Nano))
		}
	}
}

// The headline case: a passing smoke advances every displayed row, using the
// telemetry the turn's OWN envelope carried — no OAuth request needed.
func TestCLISmoke_AdvancesEveryClaudeRowFromItsOwnEnvelope(t *testing.T) {
	cache, requests := smokeFreshnessEnv(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the run's own envelope carried the reading; no probe should have been needed")
	})
	seeded := seedPreSmokeObservations(t, cache)
	stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
		return smokeEnvelopeWithRateLimits(markerFromPrompt(prompt), 44, 55, 66), nil, nil
	})

	runStart := time.Now()
	result, replayed := runCLISmoke(context.Background(), "claudeCode")
	if result.Status != cliSmokeStatusSuccess || replayed {
		t.Fatalf("smoke did not run and pass: %+v (replayed=%v)", result, replayed)
	}

	// The harvest covers the debt the same turn recorded, so the post-run
	// goroutine settles without a request. Waiting for that keeps its writes
	// inside the test's own lifetime.
	waitForNoOutstandingClaudeUsageDebt(t, 5*time.Second, 250*time.Millisecond)

	assertEveryRowObservedAtOrAfter(t, runStart)
	if latest := latestClaudeObservation(loadMergedClaudeRateLimitBuckets("")); !latest.After(seeded) {
		t.Fatalf("observation %v never advanced past the pre-smoke reading %v", latest, seeded)
	}
	if got := atomic.LoadInt64(requests); got != 0 {
		t.Errorf("spent %d OAuth requests for a reading the run already gave us", got)
	}
	// Retention: the marker, the prompt and the envelope's text must not reach
	// the cache the harvest writes.
	raw, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{claudeSmokeMarkerPrefix, "Reply with exactly", "num_turns"} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("cache leaked %q", banned)
		}
	}
}

// The probe-unavailable device (API-key auth, disable_claude_usage_probe): the
// envelope harvest alone still advances all three rows, which is the whole
// reason the harvest runs before the debt is recorded.
func TestCLISmoke_AdvancesRowsEvenWhenTheProbeIsDisabled(t *testing.T) {
	cache, requests := smokeFreshnessEnv(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a disabled probe must issue no request")
	})
	seedPreSmokeObservations(t, cache)
	stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
		return smokeEnvelopeWithRateLimits(markerFromPrompt(prompt), 71, 72, 73), nil, nil
	})
	SetClaudeUsageProbeDisabled(true)

	runStart := time.Now()
	if result, _ := runCLISmoke(context.Background(), "claudeCode"); result.Status != cliSmokeStatusSuccess {
		t.Fatalf("smoke failed: %+v", result)
	}

	assertEveryRowObservedAtOrAfter(t, runStart)
	if got := atomic.LoadInt64(requests); got != 0 {
		t.Errorf("a disabled probe issued %d requests", got)
	}
	// An unarmed gate records no debt at all, so there is nothing to settle.
	if owed := claudeUsageProbe.owedObservation(); !owed.IsZero() {
		t.Errorf("an opted-out device recorded a debt (%v)", owed)
	}
}

// The update-survival case. A smoke whose envelope carried NO telemetry leaves a
// debt, and the trailing probe cannot pay it (the device is offline — the same
// shape as an agent that is about to be replaced mid-window). The agent then
// self-replaces, modelled by resetting the gate to what a fresh process starts
// from. The first gather after the restart must inherit that debt FROM DISK and
// pay it — exactly once — and a second gather must issue nothing.
func TestCLISmoke_PostRunDebtSurvivesAnAgentUpdate(t *testing.T) {
	cache, requests := smokeFreshnessEnv(t, func(w http.ResponseWriter, r *http.Request) {
		// Every window the three displayed rows read, so "the card caught up" is
		// asserted per row rather than on the five-hour one alone.
		reset := time.Now().Add(time.Hour).Unix()
		fmt.Fprintf(w, `{"limits":[`+
			`{"kind":"session","percent":64,"resets_at":%d},`+
			`{"kind":"weekly_all","percent":65,"resets_at":%d},`+
			`{"kind":"weekly_scoped","scope":{"model":{"display_name":"Fable"}},"percent":66,"resets_at":%d}]}`,
			reset, reset, reset)
	})
	pendingRecord := os.Getenv(claudeUsagePendingRunEnv)
	seedPreSmokeObservations(t, cache)
	stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
		return successEnvelope(markerFromPrompt(prompt)), nil, nil // no rate_limits to harvest
	})
	// The trailing probe is refused, so the debt is still outstanding when the
	// process goes away — which is the state the field report was in.
	SetOffline(true)

	runStart := time.Now()
	if result, _ := runCLISmoke(context.Background(), "claudeCode"); result.Status != cliSmokeStatusSuccess {
		t.Fatalf("smoke failed: %+v", result)
	}
	// The record is written by the post-run goroutine, off the smoke's path.
	waitForPendingRunRecord(t, pendingRecord, 5*time.Second)
	if got := atomic.LoadInt64(requests); got != 0 {
		t.Fatalf("the refused trailing probe still issued %d requests", got)
	}

	// === the agent updates and restarts: in-memory state is gone ===
	restarted := freshClaudeUsageProcess(t)
	SetOffline(false)
	t.Cleanup(func() { SetOffline(false) })

	if owed := claudeUsageProbe.owedObservation(); !owed.IsZero() {
		t.Fatalf("the restarted process started with an in-memory debt (%v)", owed)
	}
	// The first gather after the restart. `latest` here is the pre-smoke reading,
	// comfortably inside the staleness TTL — an unaware gather stands down at
	// that check, which is exactly what left the card stale in the field.
	view := loadMergedClaudeRateLimitView("")
	if !refreshClaudeUsageIfStale(restarted, time.Now(), claudeSnapshotFreshness(view, time.Now()), probeTestToken, "") {
		t.Fatal("the first gather after the update did not pay the inherited debt")
	}
	assertEveryRowObservedAtOrAfter(t, runStart)
	if got := atomic.LoadInt64(requests); got != 1 {
		t.Fatalf("inherited debt cost %d requests, want exactly 1", got)
	}
	if owed := claudeUsageProbe.owedObservation(); !owed.IsZero() {
		t.Errorf("the inherited debt is still outstanding (%v) after a successful probe", owed)
	}
	if _, err := os.Stat(pendingRecord); !os.IsNotExist(err) {
		t.Errorf("the settled debt was left on disk (err=%v)", err)
	}

	// A SECOND gather issues nothing: the debt is paid and the reading is fresh.
	view = loadMergedClaudeRateLimitView("")
	refreshClaudeUsageIfStale(context.Background(), time.Now(),
		claudeSnapshotFreshness(view, time.Now()), probeTestToken, "")
	if got := atomic.LoadInt64(requests); got != 1 {
		t.Errorf("a second gather issued another request (total=%d), want 1", got)
	}
}

// A cooldown replay spent no turn, so it must leave no debt and no record —
// otherwise the CLI-maintenance flow's own retries would each buy an OAuth
// request that refreshes nothing.
func TestCLISmoke_ReplayedVerdictRecordsNoDebt(t *testing.T) {
	cache, requests := smokeFreshnessEnv(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the run's own envelope carried the reading; nothing should probe")
	})
	pendingRecord := os.Getenv(claudeUsagePendingRunEnv)
	seedPreSmokeObservations(t, cache)
	stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
		return smokeEnvelopeWithRateLimits(markerFromPrompt(prompt), 31, 32, 33), nil, nil
	})

	// First call spends the turn; its own envelope settles the debt it records.
	if _, replayed := runCLISmoke(context.Background(), "claudeCode"); replayed {
		t.Fatal("the first smoke should have run")
	}
	waitForNoOutstandingClaudeUsageDebt(t, 5*time.Second, 100*time.Millisecond)
	if _, err := os.Stat(pendingRecord); !os.IsNotExist(err) {
		t.Fatalf("a settled debt left its record behind (err=%v)", err)
	}

	// Second call is served by the cooldown — no turn, so no new obligation.
	if _, replayed := runCLISmoke(context.Background(), "claudeCode"); !replayed {
		t.Fatal("the second smoke should have been replayed by the cooldown")
	}
	if owed := claudeUsageProbe.owedObservation(); !owed.IsZero() {
		t.Errorf("a replayed verdict recorded a debt (%v)", owed)
	}
	if _, err := os.Stat(pendingRecord); !os.IsNotExist(err) {
		t.Errorf("a replayed verdict wrote a durable record (err=%v)", err)
	}
	if got := atomic.LoadInt64(requests); got != 0 {
		t.Errorf("a replay issued %d requests", got)
	}
}

// A logged-out device short-circuits before the ladder: no turn, no debt, and no
// bucket written — the pre-check's whole purpose.
func TestCLISmoke_NotLoggedInSpendsNothingAndWritesNothing(t *testing.T) {
	cache, requests := smokeFreshnessEnv(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a logged-out device must not probe")
	})
	pendingRecord := os.Getenv(claudeUsagePendingRunEnv)
	seeded := seedPreSmokeObservations(t, cache)
	stubAuthProbe(t, false, true)
	calls := stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
		t.Fatal("a logged-out device must never spawn the CLI")
		return nil, nil, nil
	})

	result, _ := runCLISmoke(context.Background(), "claudeCode")
	if result.Diagnostic != cliSmokeDiagnosticNotLoggedIn {
		t.Fatalf("diagnostic = %q, want %q", result.Diagnostic, cliSmokeDiagnosticNotLoggedIn)
	}
	if *calls != 0 {
		t.Errorf("exec seam ran %d times", *calls)
	}
	if owed := claudeUsageProbe.owedObservation(); !owed.IsZero() {
		t.Errorf("a free pre-check verdict recorded a debt (%v)", owed)
	}
	if _, err := os.Stat(pendingRecord); !os.IsNotExist(err) {
		t.Errorf("a free pre-check verdict wrote a durable record (err=%v)", err)
	}
	if got := atomic.LoadInt64(requests); got != 0 {
		t.Errorf("a free pre-check verdict issued %d requests", got)
	}
	if latest := latestClaudeObservation(loadMergedClaudeRateLimitBuckets("")); latest.After(seeded) {
		t.Error("a run that never happened advanced the cached observation")
	}
}

/* ---------------------------------- helpers -------------------------------- */

// freshClaudeUsageProcess models the agent restarting mid-maintenance: every
// in-memory latch is gone, the arm state is re-applied from config, and the only
// thing carried over is what is on disk. Returns a plain gather context.
func freshClaudeUsageProcess(t *testing.T) context.Context {
	t.Helper()
	// resetClaudeUsageProbeGate clears the durable record as a TEST seam, so the
	// bytes are preserved across it — a real restart leaves them on disk.
	record := os.Getenv(claudeUsagePendingRunEnv)
	saved, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("no pending-run record to carry across the restart: %v", err)
	}
	resetClaudeUsageProbeGate()
	if err := os.WriteFile(record, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	SetClaudeUsageProbeDisabled(false)
	return context.Background()
}

func waitForPendingRunRecord(t *testing.T, path string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no durable post-run record appeared at %s within %s", path, within)
}
