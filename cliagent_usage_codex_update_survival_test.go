package main

import (
	"bytes"
	"context"
	"os"
	"strings"
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

// The replay spends ONE bounded reconcile itself and, when that finds nothing,
// hands the debt to the account's worker — the one place the live fallback
// runs. The worker spends only what is left of the debt's attempt budget, then
// the fallback, once: a debt carried across a (self-)update is exactly the one
// the rollout scan is most likely to miss.
func TestCodexOwedRefresh_RestartReplaySpendsTheDebtBudgetOnce(t *testing.T) {
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
	if snap.RefreshOwedAttempts != codexRefreshAfterRunMaxAttempts {
		t.Fatalf("replay + worker spent %d reconciles, want exactly the debt's %d", snap.RefreshOwedAttempts, codexRefreshAfterRunMaxAttempts)
	}
	if snap.RefreshFallbackState != codexFallbackSpent {
		t.Fatalf("the unpaid replayed debt must reach its live fallback once: state=%q", snap.RefreshFallbackState)
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
	if snap.RefreshOwedAttempts != codexRefreshAfterRunMaxAttempts {
		t.Fatalf("attempts = %d, want the replay's 1 plus the worker's remainder (%d)", snap.RefreshOwedAttempts, codexRefreshAfterRunMaxAttempts)
	}
}

// Startup replay is spawned, so a session of the NEW process can arm its floor
// before the replay reads the cache. That floor's run is still going: it must
// not be converted into a completed debt (nor spend the replay's reconcile),
// because its own settle path owes it a refresh when it finishes.
func TestCodexOwedRefresh_StartupReplaySkipsARunThisProcessStarted(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)

	simulateCodexAgentRestart(t)
	// This process's own run start, landed on disk before the replay runs.
	runStart := time.Now()
	armCodexUsageRunFloor(runStart)
	waitCodexUsageRefreshIdle(t)
	if state := codexRunFreshnessForAccount(f.fp, time.Now()); !state.interrupted {
		t.Fatalf("a live armed floor is what startup would read as interrupted: %+v", state)
	}

	payOwedCodexUsageRefresh()
	waitCodexUsageRefreshIdle(t)

	snap := f.snapshot(t)
	if snap.RefreshOwedAtMs != 0 || snap.RefreshOwedAttempts != 0 {
		t.Fatalf("a run this process started must not be settled as debt at startup: %+v", snap)
	}
	if snap.RunFloorMs != runStart.UnixMilli() {
		t.Fatalf("RunFloorMs = %d, want the live run's floor %d", snap.RunFloorMs, runStart.UnixMilli())
	}
	if codexGateHasRun(f.fp) {
		t.Fatal("the replay must not spend a reconcile on a still-running run")
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

// The startup replay reconciles BEFORE a refused interrupted-to-owed conversion
// is on disk, so that attempt is never counted against the debt, and the
// conversion that finally lands resets the count to zero. The worker must run
// whenever a conversion was retained — even when the replay's own final flush
// lands it — or routine gathers force scan after scan without ever counting an
// attempt, and a run whose telemetry never appears never reaches the warning.
func TestCodexOwedRefresh_RefusedConversionStillSpendsTheWorkerAttempts(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-5 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	codexRecordRunFreshness(f.fp, runStart, func(snap *codexRateLimitSnapshot) {
		codexArmRunFloor(snap, runStart)
	})
	prevWait, prevPoll := codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll
	codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = 50*time.Millisecond, time.Millisecond
	t.Cleanup(func() { codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = prevWait, prevPoll })

	simulateCodexAgentRestart(t)
	// Wedge the in-process gate across the conversion (refused after the 50ms
	// wait) but not the replay's trailing flush, which lands the debt.
	codexRateLimitMu.Lock()
	go func() {
		time.Sleep(150 * time.Millisecond)
		codexRateLimitMu.Unlock()
	}()
	payOwedCodexUsageRefresh()
	waitCodexUsageRefreshIdle(t)

	snap := f.snapshot(t)
	if snap.RefreshOwedAtMs == 0 {
		t.Fatalf("refused startup conversion left the interrupted run un-owed: %+v", snap)
	}
	// No rollout evidence exists, so the worker's attempts all come up empty
	// and must be COUNTED: that count is what lets the card explain itself.
	if snap.RefreshOwedAttempts < codexRefreshAfterRunMaxAttempts {
		t.Fatalf("RefreshOwedAttempts=%d, want the worker's %d attempts spent on the retained debt", snap.RefreshOwedAttempts, codexRefreshAfterRunMaxAttempts)
	}
	if notice := codexStaleRunNotice(codexRunFreshnessForAccount(f.fp, time.Now())); notice == "" {
		t.Fatal("want the stale-run notice once the retained debt's attempts are spent")
	}
}

// The clock rolled back while the previous process still had a run unpaid, and
// no run of THIS process has started yet to rebase it. Startup must not read
// the future floor as-is: nothing — not even timestamp-less rollout evidence,
// clamped to `now` — can cover a floor the wall clock has not reached, and a
// stale notice would name a run time in the future. The replay re-dates the
// state onto `now`, the only trustworthy reading, and CARRIES THE DEBT OVER:
// dropping it would forget the promised post-run refresh in exactly the
// self-update case this path exists to recover.
func TestCodexOwedRefresh_StartupRebasesFloorLeftInTheFutureByAClockRollback(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	// State the previous process wrote against a clock an hour ahead of ours.
	future := now.Add(time.Hour)
	codexRecordRunFreshness(f.fp, future, func(snap *codexRateLimitSnapshot) {
		codexArmRunFloor(snap, future)
		codexOweRunRefresh(snap, future, future.Add(time.Minute))
	})

	simulateCodexAgentRestart(t)
	payOwedCodexUsageRefresh()
	waitCodexUsageRefreshIdle(t)

	if !codexGateHasRun(f.fp) {
		t.Fatal("the carried-over debt must still be reconciled for")
	}
	snap := f.snapshot(t)
	if snap.RunFloorMs == 0 || snap.RefreshOwedAtMs == 0 {
		t.Fatalf("future-dated run state must be re-dated, not dropped: %+v", snap)
	}
	if snap.RunFloorMs > time.Now().Add(codexRunFloorLocalSkew).UnixMilli() {
		t.Fatalf("no floor may stay ahead of the wall clock: %+v", snap)
	}
	if snap.RefreshOwedAttempts != codexRefreshAfterRunMaxAttempts {
		t.Fatalf("replay + worker spent %d reconciles, want exactly %d", snap.RefreshOwedAttempts, codexRefreshAfterRunMaxAttempts)
	}
	if notice := codexStaleRunNotice(codexRunFreshnessForAccount(f.fp, time.Now())); strings.Contains(notice, future.UTC().Format("2006-01-02 15:04")) {
		t.Fatalf("no notice may name a run that has not happened: %q", notice)
	}

	// The same rollback with the run merely interrupted (armed, never settled)
	// is carried over the same way: re-dated onto `now` and owed from there.
	resetCodexUsageRefreshGate()
	SetCodexUsageRefreshEnabled(true)
	codexRecordRunFreshness(f.fp, future, func(snap *codexRateLimitSnapshot) {
		snap.RefreshOwedAtMs, snap.RefreshOwedAttempts, snap.RunFloorPaidMs = 0, 0, 0
		codexArmRunFloor(snap, future)
	})
	simulateCodexAgentRestart(t)
	payOwedCodexUsageRefresh()
	waitCodexUsageRefreshIdle(t)
	snap = f.snapshot(t)
	if snap.RunFloorMs == 0 || snap.RunFloorMs > time.Now().Add(codexRunFloorLocalSkew).UnixMilli() {
		t.Fatalf("an interrupted future-dated run must be re-dated onto now: %+v", snap)
	}
}

/* ───────────────────────── Codex binary upgrades ───────────────────────── */

const (
	codexPreUpdateVersion  = "codex-cli 0.149.0"
	codexPostUpdateVersion = "codex-cli 0.150.0"
)

// stampPreUpdateCache marks the cache as written by the previous Codex binary:
// both the reading and the scan cursor.
func (f codexFreshnessFixture) stampPreUpdateCache(t *testing.T, now time.Time) {
	t.Helper()
	if !codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		snap.CodexVersion, snap.RolloutCursorVersion = codexPreUpdateVersion, codexPreUpdateVersion
	}) {
		t.Fatal("stamping the pre-update cache failed")
	}
}

// The headline regression. The cache was written by the previous Codex, the
// new build's rollout carries its telemetry in a shape the scan cannot read,
// and a run settles: both rollout attempts come up empty, the live fallback
// pays the debt, observedAt advances — and all of it survives an agent
// restart with no stale or drift notice left behind.
func TestCodexUpgrade_ObservedAtAdvancesViaLiveFallbackAndSurvivesRestart(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-2 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	f.advanceCursorPast(t, now.Add(-10*time.Second), now)
	f.stampPreUpdateCache(t, now)
	// The post-update rollout: numbers under a key the scan does not know.
	writeCodexRunRollout(t, f.home, "post-update", runStart, runStart.Add(30*time.Second), runStart.Add(40*time.Second), true, nil,
		map[string]any{"timestamp": runStart.Add(30 * time.Second).UTC().Format(time.RFC3339Nano), "type": "event_msg",
			"payload": map[string]any{"type": "usage_snapshot", "windows": map[string]any{"five_hour": 55}}})
	setCodexCaptureVersion(t, codexPostUpdateVersion)
	calls := stubCodexFallbackRead(t, capturingFallbackRead)

	triggerCodexUsageRefreshAfterRun(runStart)
	waitCodexUsageRefreshIdle(t)

	if *calls != 1 {
		t.Fatalf("live reads = %d, want the one fallback", *calls)
	}
	observed := metricObservedAt(t, codexSessionMetric(t, codexMetricsFromCache(time.Now(), f.fp)))
	if observed.Before(runStart.Truncate(time.Second)) {
		t.Fatalf("observedAt %s still predates the post-update run %s", observed, runStart)
	}
	snap := f.snapshot(t)
	if snap.RefreshOwedAtMs != 0 || snap.CodexVersion != codexPostUpdateVersion || snap.RolloutCursorVersion != codexPostUpdateVersion {
		t.Fatalf("want the debt paid and both stamps on the new binary: %+v", snap)
	}

	simulateCodexAgentRestart(t)
	payOwedCodexUsageRefresh()
	waitCodexUsageRefreshIdle(t)

	if got := metricObservedAt(t, codexSessionMetric(t, codexMetricsFromCache(time.Now(), f.fp))); !got.Equal(observed) {
		t.Fatalf("observedAt %s did not survive the restart (was %s)", got, observed)
	}
	usage, _ := codexUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{Version: codexPostUpdateVersion}, time.Now())
	if usage.Notice != "" {
		t.Fatalf("a refreshed post-update card carries a notice: %q", usage.Notice)
	}
}

// Startup ordering: the replay runs before any gather or smoke has named the
// binary. It resolves the installed version itself, so a debt its live
// fallback pays is stamped with the NEW build — and the first gather after
// the restart shows no capture drift against a reading just refreshed.
func TestCodexUpgrade_StartupReplayStampsTheInstalledBuild(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-3 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	f.stampPreUpdateCache(t, now)
	codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		codexOweRunRefresh(snap, runStart, now.Add(-time.Minute))
	})
	original := codexInstalledVersion
	codexInstalledVersion = func() string { return codexPostUpdateVersion }
	t.Cleanup(func() { codexInstalledVersion = original })
	stubCodexFallbackRead(t, capturingFallbackRead)

	simulateCodexAgentRestart(t)
	payOwedCodexUsageRefresh()
	waitCodexUsageRefreshIdle(t)

	snap := f.snapshot(t)
	if snap.RefreshOwedAtMs != 0 || snap.CodexVersion != codexPostUpdateVersion {
		t.Fatalf("the replayed debt must be paid and stamped with the installed build: %+v", snap)
	}
	usage, _ := codexUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{Version: codexPostUpdateVersion}, time.Now())
	if usage.Notice != "" {
		t.Fatalf("first gather after the restart shows %q", usage.Notice)
	}
}

// A binary change resets the rollout scan cursor ONCE, in a write taken before
// the scan, and stamps the new binary in that same write.
func TestCodexUpgrade_CursorResetsOnceOnVersionChange(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	cursorAt := now.Add(-10 * time.Second)
	f.advanceCursorPast(t, cursorAt, now)
	f.stampPreUpdateCache(t, now)

	setCodexCaptureVersion(t, codexPreUpdateVersion)
	if cursor := codexRolloutScanCursorForAccount(f.home, f.fp, now); cursor.mtimeNs != cursorAt.UnixNano() {
		t.Fatalf("same binary must keep its cursor, got %+v", cursor)
	}

	setCodexCaptureVersion(t, codexPostUpdateVersion)
	if cursor := codexRolloutScanCursorForAccount(f.home, f.fp, now); cursor.mtimeNs != 0 {
		t.Fatalf("a cursor the previous binary wrote must read as empty, got %+v", cursor)
	}
	codexResetRolloutCursorForVersion(context.Background(), f.fp, now)
	snap := f.snapshot(t)
	if snap.RolloutHighWaterMtimeNs != 0 || snap.RolloutCursorVersion != codexPostUpdateVersion {
		t.Fatalf("reset must clear progress and stamp the new binary together: %+v", snap)
	}
	if snap.CodexVersion != codexPreUpdateVersion {
		t.Fatalf("the cursor reset must not touch the reading's stamp, got %q", snap.CodexVersion)
	}
}

// A routine (unforced) scan treats the reset as optional work, like its own
// commit: under lock contention it neither blocks nor writes, and the stale
// cursor still reads as empty so nothing is scanned against the old layout.
// A forced scan waits and lands it.
func TestCodexUpgrade_RoutineResetSkipsUnderContention(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	f.advanceCursorPast(t, now.Add(-10*time.Second), now)
	f.stampPreUpdateCache(t, now)
	setCodexCaptureVersion(t, codexPostUpdateVersion)

	codexRateLimitMu.Lock()
	done := make(chan struct{})
	go func() {
		codexResetRolloutCursorForVersion(context.Background(), f.fp, now)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		codexRateLimitMu.Unlock()
		<-done
		t.Fatal("a routine reset blocked on the cache lock")
	}
	codexRateLimitMu.Unlock()
	if snap := f.snapshot(t); snap.RolloutCursorVersion != codexPreUpdateVersion {
		t.Fatalf("a contended routine reset wrote: %+v", snap)
	}
	if cursor := codexRolloutScanCursorForAccount(f.home, f.fp, now); cursor.mtimeNs != 0 {
		t.Fatalf("the old cursor must still read as empty, got %+v", cursor)
	}

	codexResetRolloutCursorForVersion(withCodexForcedReconcile(context.Background(), now), f.fp, now)
	if snap := f.snapshot(t); snap.RolloutCursorVersion != codexPostUpdateVersion {
		t.Fatalf("a forced reset must land: %+v", snap)
	}
}

// No reset loop: a post-upgrade rescan that runs out of budget leaves partial
// progress, and the NEXT refresh resumes it instead of resetting again —
// the stamp landed at the reset, not at a completed pass.
func TestCodexUpgrade_PartialRescanIsNotResetAgain(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	f.advanceCursorPast(t, now.Add(-10*time.Second), now)
	f.stampPreUpdateCache(t, now)
	setCodexCaptureVersion(t, codexPostUpdateVersion)

	codexResetRolloutCursorForVersion(context.Background(), f.fp, now)
	// The budget-bound first pass got part way through the backlog.
	partialAt := now.Add(-time.Hour)
	codexRateLimitCacheTransaction(context.Background(), f.cache, now, true, func(snap *codexRateLimitSnapshot) bool {
		snap.RolloutHighWaterMtimeNs, snap.RolloutHighWaterMtimeMs = partialAt.UnixNano(), partialAt.UnixMilli()
		snap.RolloutBacklogCursor = strings.Repeat("a", 64)
		snap.RolloutRootFingerprint = codexRolloutRootFingerprint(f.home)
		return true
	})

	codexResetRolloutCursorForVersion(context.Background(), f.fp, now.Add(time.Minute))
	cursor := codexRolloutScanCursorForAccount(f.home, f.fp, now.Add(time.Minute))
	if cursor.mtimeNs != partialAt.UnixNano() || cursor.backlogCursor != strings.Repeat("a", 64) {
		t.Fatalf("the second refresh must resume the partial rescan, got %+v", cursor)
	}
}

// The drift notice surfaces on the card only once the debt's attempts are
// spent AND its fallback resolved, and only when the reading was produced by
// another binary than the one detected.
func TestCodexUpgrade_DriftNoticeOnlyOnceTheFallbackResolved(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	f.stampPreUpdateCache(t, now)
	state := f.oweExhaustedDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute))
	detected := detectedCLIAgent{Version: codexPostUpdateVersion}
	parse := func() *cliAgentUsage {
		// A routine gather inside the interval: no forced reconcile to count.
		codexUsageRefresh.mu.Lock()
		codexUsageRefresh.lastRun[f.fp] = time.Now()
		codexUsageRefresh.driftBypass[f.fp] = codexPostUpdateVersion
		codexUsageRefresh.mu.Unlock()
		usage, _ := codexUsageParser{}.ParseContext(context.Background(), "", detected, time.Now())
		return usage
	}

	codexSetRefreshFallback(f.fp, state.debtID(), codexFallbackOutstanding)
	if usage := parse(); usage.Notice != "" {
		t.Fatalf("no notice while the fallback is outstanding, got %q", usage.Notice)
	}
	codexSetRefreshFallback(f.fp, state.debtID(), codexFallbackSkipped)
	usage := parse()
	if usage.NoticeSeverity != "warning" || !strings.Contains(usage.Notice, codexPreUpdateVersion) || !strings.Contains(usage.Notice, codexPostUpdateVersion) {
		t.Fatalf("resolved fallback must show the capture-drift warning, got %q (%s)", usage.Notice, usage.NoticeSeverity)
	}
}
