package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// codexFreshnessFixture is an isolated CODEX_HOME + rate-limit cache with a
// signed-in account, and the freshness gate armed with test-sized bounds.
type codexFreshnessFixture struct {
	home  string
	cache string
	fp    string
}

func newCodexFreshnessFixture(t *testing.T, loginAt time.Time) codexFreshnessFixture {
	t.Helper()
	home := t.TempDir()
	cache := filepath.Join(t.TempDir(), "codex_rate_limits.json")
	t.Setenv("CODEX_HOME", home)
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	helperCodexAuthAt(t, home, "dev@example.com", loginAt)

	// Drain any worker first: the bounds below are plain vars its goroutine reads.
	resetCodexUsageRefreshGate()
	prevRetry, prevInterval := codexRefreshAfterRunRetryDelay, codexForcedReconcileMinInterval
	codexRefreshAfterRunRetryDelay = 10 * time.Millisecond
	codexForcedReconcileMinInterval = 20 * time.Millisecond
	SetCodexUsageRefreshEnabled(true)
	// Registered after the t.Setenv calls, so it runs BEFORE they are undone:
	// no worker can outlive this test's CODEX_HOME / cache path.
	t.Cleanup(func() {
		resetCodexUsageRefreshGate()
		codexRefreshAfterRunRetryDelay, codexForcedReconcileMinInterval = prevRetry, prevInterval
	})
	return codexFreshnessFixture{home: home, cache: cache, fp: currentCodexAccountFingerprint()}
}

// seedPreRunReading writes a numeric reading observed at observedAt, with both
// windows still live, as the cache the run starts from.
func (f codexFreshnessFixture) seedPreRunReading(t *testing.T, observedAt, now time.Time) {
	t.Helper()
	mergeCodexRateLimitCache(f.cache, map[string]codexRateLimitBucket{
		codexWindowPrimary: {
			UsedPercentage: 10, ResetsAtMs: now.Add(3 * time.Hour).UnixMilli(), ObservedAtMs: observedAt.UnixMilli(),
			WindowMinutes: 300, usageKnown: true, resetKnown: true,
		},
		codexWindowSecondary: {
			UsedPercentage: 40, ResetsAtMs: now.Add(72 * time.Hour).UnixMilli(), ObservedAtMs: observedAt.UnixMilli(),
			WindowMinutes: 10080, usageKnown: true, resetKnown: true,
		},
	}, nil, now, f.fp)
}

// advanceCursorPast moves the persisted rollout scan cursor to cursorAt, the
// state in which a routine scan no longer reopens a rollout written earlier.
func (f codexFreshnessFixture) advanceCursorPast(t *testing.T, cursorAt, now time.Time) {
	t.Helper()
	ok := codexRateLimitCacheTransaction(context.Background(), f.cache, now, true, func(snap *codexRateLimitSnapshot) bool {
		snap.RolloutHighWaterMtimeNs = cursorAt.UnixNano()
		snap.RolloutHighWaterMtimeMs = cursorAt.UnixMilli()
		snap.RolloutRootFingerprint = codexRolloutRootFingerprint(f.home)
		return true
	})
	if !ok {
		t.Fatal("advance cursor: transaction failed")
	}
}

func (f codexFreshnessFixture) snapshot(t *testing.T) codexRateLimitSnapshot {
	t.Helper()
	snap, ok := loadCodexRateLimitSnapshot(f.cache)
	if !ok {
		t.Fatal("cache snapshot missing")
	}
	return snap
}

// writeCodexRunRollout writes a rollout for a session that started at
// sessionStart and whose token_count frames were emitted at frameAt. stamped
// false omits the envelope `timestamp` from every telemetry frame — the shape a
// Codex build that renamed that key produces. The file's mtime is set to mtime.
func writeCodexRunRollout(t *testing.T, base, name string, sessionStart, frameAt, mtime time.Time, stamped bool, frames []map[string]any, extraLines ...map[string]any) string {
	t.Helper()
	dir := filepath.Join(base, "sessions", sessionStart.UTC().Format("2006"), sessionStart.UTC().Format("01"), sessionStart.UTC().Format("02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var b strings.Builder
	b.WriteString(codexRolloutHeaderLine(t, sessionStart.UTC().Format(time.RFC3339Nano)))
	writeLine := func(v map[string]any) {
		line, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal rollout line: %v", err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	for _, extra := range extraLines {
		writeLine(extra)
	}
	for _, rl := range frames {
		line := map[string]any{
			"type": "event_msg",
			"payload": map[string]any{
				"type":        "token_count",
				"info":        map[string]any{"total_token_usage": map[string]any{"total_tokens": 1}},
				"rate_limits": rl,
			},
		}
		if stamped {
			line["timestamp"] = frameAt.UTC().Format(time.RFC3339Nano)
		}
		writeLine(line)
	}
	path := filepath.Join(dir, "rollout-"+name+".jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes rollout: %v", err)
	}
	return path
}

func waitCodexUsageRefreshIdle(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		codexUsageRefresh.waitIdle()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("codex usage refresh workers did not finish")
	}
}

func codexSessionMetric(t *testing.T, metrics []cliAgentUsageMetric) cliAgentUsageMetric {
	t.Helper()
	for _, m := range metrics {
		if m.Kind == limitKindSession && m.Model == "" {
			return m
		}
	}
	t.Fatalf("no session metric in %+v", metrics)
	return cliAgentUsageMetric{}
}

func metricObservedAt(t *testing.T, m cliAgentUsageMetric) time.Time {
	t.Helper()
	observed, err := time.Parse(time.RFC3339, m.ObservedAt)
	if err != nil {
		t.Fatalf("metric %+v has no parseable observedAt: %v", m, err)
	}
	return observed
}

// The reported regression: a Codex run completes, its rollout carries fresh
// numeric telemetry, but the persisted scan cursor already sits past that
// rollout — so every routine refresh keeps serving the pre-run observedAt. The
// post-run reconcile must scan from the run floor and advance it.
func TestCodexRefreshAfterRun_ObservedAtAdvancesAtOrAfterRunStart(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-2 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	preRun := now.Add(-time.Hour)
	f.seedPreRunReading(t, preRun, now)
	writeCodexRunRollout(t, f.home, "run", runStart, runStart.Add(30*time.Second), runStart.Add(40*time.Second), true,
		[]map[string]any{codexRateLimitFrame(55, 61, now)})
	f.advanceCursorPast(t, now.Add(-10*time.Second), now)

	// Precondition: the routine, cursor-gated scan cannot see the run.
	routine, _, _ := codexReconcileFromRollout(context.Background(), f.home, f.fp, time.Now())
	if got := metricObservedAt(t, codexSessionMetric(t, routine)); got.After(runStart) {
		t.Fatalf("precondition: routine scan already advanced observedAt to %s", got)
	}

	triggerCodexUsageRefreshAfterRun(runStart)
	waitCodexUsageRefreshIdle(t)

	session := codexSessionMetric(t, codexMetricsFromCache(time.Now(), f.fp))
	if got := metricObservedAt(t, session); got.UnixMilli() < runStart.UnixMilli() {
		t.Fatalf("observedAt %s still predates run start %s", got, runStart)
	}
	if session.Consumed == nil || *session.Consumed != 55 {
		t.Fatalf("session Consumed = %v, want the run's 55%%", session.Consumed)
	}
	snap := f.snapshot(t)
	if snap.RefreshOwedAtMs != 0 || snap.RefreshOwedAttempts != 0 {
		t.Fatalf("paid debt not cleared: owedAt=%d attempts=%d", snap.RefreshOwedAtMs, snap.RefreshOwedAttempts)
	}
	if snap.RunFloorMs != runStart.UnixMilli() {
		t.Fatalf("RunFloorMs = %d, want %d", snap.RunFloorMs, runStart.UnixMilli())
	}
}

// Live capture that already covered the run settles the debt inside the very
// transaction that records it: no rollout scan is spent.
func TestCodexRefreshAfterRun_LiveCaptureAlreadyCoveringRunSettlesWithoutScan(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-10*time.Second), now)

	triggerCodexUsageRefreshAfterRun(runStart)
	waitCodexUsageRefreshIdle(t)

	if snap := f.snapshot(t); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("covered run left a debt: %+v", snap)
	}
	codexUsageRefresh.mu.Lock()
	_, scanned := codexUsageRefresh.lastRun[f.fp]
	codexUsageRefresh.mu.Unlock()
	if scanned {
		t.Fatal("a run already covered by live capture must not spend a forced reconcile")
	}
}

// A Codex build whose rollout frames carry no parseable envelope timestamp used
// to be dropped wholesale. A forced post-run reconcile anchors them at the
// file's mtime (clamped to [floor, now]) and flags them Inferred; a routine scan
// still drops them.
func TestCodexRefreshAfterRun_InfersObservationForTimestamplessFrames(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-2 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	preRun := now.Add(-time.Hour)
	f.seedPreRunReading(t, preRun, now)
	mtime := runStart.Add(40 * time.Second)
	writeCodexRunRollout(t, f.home, "unstamped", runStart, time.Time{}, mtime, false,
		[]map[string]any{codexRateLimitFrame(72, 64, now)})

	routine, _, _ := codexReconcileFromRollout(context.Background(), f.home, f.fp, time.Now())
	if got := metricObservedAt(t, codexSessionMetric(t, routine)); got.UnixMilli() != preRun.UnixMilli() {
		t.Fatalf("routine scan must still drop timestamp-less frames; observedAt=%s want %s", got, preRun)
	}

	triggerCodexUsageRefreshAfterRun(runStart)
	waitCodexUsageRefreshIdle(t)

	session := codexSessionMetric(t, codexMetricsFromCache(time.Now(), f.fp))
	if got := metricObservedAt(t, session); got.UnixMilli() < runStart.UnixMilli() || got.After(time.Now()) {
		t.Fatalf("inferred observedAt %s outside [run start %s, now]", got, runStart)
	}
	if session.Consumed == nil || *session.Consumed != 72 {
		t.Fatalf("session Consumed = %v, want 72", session.Consumed)
	}
	inferred := false
	for _, limits := range f.snapshot(t).Contributors {
		for _, b := range limits {
			if b.ObservedAtMs >= runStart.UnixMilli() {
				inferred = inferred || b.Inferred
				if b.ObservedAtMs != mtime.UnixMilli() {
					t.Errorf("inferred observation %d, want the rollout mtime %d", b.ObservedAtMs, mtime.UnixMilli())
				}
			}
		}
	}
	if !inferred {
		t.Fatal("file-anchored observation must be flagged Inferred")
	}
}

// When the run left both a stated reading and a later timestamp-less one, the
// stated reading wins: an inferred time only stands in where Codex said nothing.
func TestCodexRefreshAfterRun_StatedReadingBeatsLaterInferredOne(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-2 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	writeCodexRunRollout(t, f.home, "stamped", runStart, runStart.Add(20*time.Second), runStart.Add(30*time.Second), true,
		[]map[string]any{codexRateLimitFrame(21, 31, now)})
	writeCodexRunRollout(t, f.home, "unstamped", runStart.Add(time.Second), time.Time{}, runStart.Add(50*time.Second), false,
		[]map[string]any{codexRateLimitFrame(99, 99, now)})

	triggerCodexUsageRefreshAfterRun(runStart)
	waitCodexUsageRefreshIdle(t)

	session := codexSessionMetric(t, codexMetricsFromCache(time.Now(), f.fp))
	if session.Consumed == nil || *session.Consumed != 21 {
		t.Fatalf("session Consumed = %v, want the stated 21%%", session.Consumed)
	}
	if got := metricObservedAt(t, session); got.UnixMilli() != runStart.Add(20*time.Second).UnixMilli() {
		t.Fatalf("observedAt = %s, want the stated frame time", got)
	}
}

// A rollout last written before the run floor cannot describe the run: its
// timestamp-less frames stay dropped even under a forced reconcile.
func TestCodexRolloutInferredObservation_RejectsFilesOlderThanFloor(t *testing.T) {
	now := time.Now()
	floor := now.Add(-time.Minute)
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	forced := withCodexForcedReconcile(context.Background(), floor)

	for _, tc := range []struct {
		name  string
		ctx   context.Context
		mtime time.Time
		want  time.Time
	}{
		{"routine scan never infers", context.Background(), now.Add(-10 * time.Second), time.Time{}},
		{"forced without a floor never infers", withCodexForcedReconcile(context.Background(), time.Time{}), now.Add(-10 * time.Second), time.Time{}},
		{"written before the floor", forced, floor.Add(-time.Second), time.Time{}},
		{"written after the floor", forced, now.Add(-10 * time.Second), now.Add(-10 * time.Second)},
		{"future mtime clamps to now", forced, now.Add(time.Hour), now},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Chtimes(path, tc.mtime, tc.mtime); err != nil {
				t.Fatal(err)
			}
			got := codexRolloutInferredObservation(tc.ctx, file, now)
			if !got.Equal(tc.want) && got.Sub(tc.want).Abs() > time.Second {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
			if tc.want.IsZero() != got.IsZero() {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// No numeric evidence anywhere: the reading is published unchanged, the debt
// stops after exactly codexRefreshAfterRunMaxAttempts, and the card says its
// observation predates the run instead of silently looking fresh.
func TestCodexRefreshAfterRun_NoEvidenceKeepsReadingAndWarns(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-2 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	preRun := now.Add(-time.Hour)
	f.seedPreRunReading(t, preRun, now)

	triggerCodexUsageRefreshAfterRun(runStart)
	waitCodexUsageRefreshIdle(t)

	snap := f.snapshot(t)
	if snap.RefreshOwedAtMs == 0 {
		t.Fatal("an unpaid debt must stay recorded")
	}
	if snap.RefreshOwedAttempts != codexRefreshAfterRunMaxAttempts {
		t.Fatalf("attempts = %d, want exactly %d", snap.RefreshOwedAttempts, codexRefreshAfterRunMaxAttempts)
	}

	usage, ok := codexUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{}, time.Now())
	if !ok {
		t.Fatal("parse failed")
	}
	session := codexSessionMetric(t, usage.Metrics)
	if session.Consumed == nil || *session.Consumed != 10 {
		t.Fatalf("numeric reading must be published unchanged; Consumed=%v", session.Consumed)
	}
	if got := metricObservedAt(t, session); got.UnixMilli() != preRun.UnixMilli() {
		t.Fatalf("observedAt = %s, want the untouched pre-run %s", got, preRun)
	}
	if usage.NoticeSeverity != "warning" {
		t.Fatalf("NoticeSeverity = %q, want warning (notice %q)", usage.NoticeSeverity, usage.Notice)
	}
	for _, want := range []string{preRun.UTC().Format("2006-01-02 15:04 UTC"), runStart.UTC().Format("2006-01-02 15:04 UTC")} {
		if !strings.Contains(usage.Notice, want) {
			t.Errorf("notice %q missing %q", usage.Notice, want)
		}
	}
}

// Before the post-run attempts are spent the card is not flagged: a refresh in
// the middle of the settle window must not warn about a debt still being paid.
func TestCodexStaleRunNotice_OnlyAfterAttemptsAreSpent(t *testing.T) {
	floor := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	state := codexRunFreshnessState{floor: floor, owed: true, attempts: 1, latest: floor.Add(-time.Hour)}
	if n := codexStaleRunNotice(state); n != "" {
		t.Fatalf("notice before attempts were spent: %q", n)
	}
	state.attempts = codexRefreshAfterRunMaxAttempts
	if n := codexStaleRunNotice(state); !strings.Contains(n, "09:00 UTC") || !strings.Contains(n, "10:00 UTC") {
		t.Fatalf("notice %q must name both timestamps", n)
	}
	state.latest = time.Time{}
	if n := codexStaleRunNotice(state); !strings.HasPrefix(n, "No Codex utilization reading") {
		t.Fatalf("notice without any observation = %q", n)
	}
	state.owed = false
	if n := codexStaleRunNotice(state); n != "" {
		t.Fatalf("paid debt must not warn: %q", n)
	}
}

// The routine scan skips its commit when the cache lock is contended; the
// forced reconcile waits for it instead, so the run's evidence lands.
func TestCodexForcedReconcile_WaitsForContendedCacheLock(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-2 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	writeCodexRunRollout(t, f.home, "run", runStart, runStart.Add(30*time.Second), runStart.Add(40*time.Second), true,
		[]map[string]any{codexRateLimitFrame(33, 44, now)})

	codexRateLimitMu.Lock()
	routine, _, _ := codexReconcileFromRollout(context.Background(), f.home, f.fp, time.Now())
	if got := metricObservedAt(t, codexSessionMetric(t, routine)); !got.Before(runStart) {
		codexRateLimitMu.Unlock()
		t.Fatalf("precondition: routine scan committed under a held lock (observedAt %s)", got)
	}

	var res codexReconcileResult
	var ran bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		res, ran, _, _ = codexRunGatedReconcile(ctx, f.home, f.fp, runStart, true, time.Now())
	}()
	time.Sleep(150 * time.Millisecond)
	codexRateLimitMu.Unlock()
	<-done

	if !ran {
		t.Fatal("forced reconcile did not run")
	}
	if got := metricObservedAt(t, codexSessionMetric(t, res.metrics)); got.UnixMilli() < runStart.UnixMilli() {
		t.Fatalf("contended forced reconcile dropped the run's evidence; observedAt=%s", got)
	}
}

// Single flight holds for every caller, including a user-forced refresh; the
// per-account interval holds only for callers that do not bypass it.
func TestCodexUsageRefreshGate_SingleFlightAndInterval(t *testing.T) {
	prev := codexForcedReconcileMinInterval
	codexForcedReconcileMinInterval = 20 * time.Second
	t.Cleanup(func() { codexForcedReconcileMinInterval = prev })

	g := newCodexUsageRefreshGate()
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)

	done, busy, _ := g.begin("acct", t0, false)
	if done == nil || busy != nil {
		t.Fatal("first reconcile must be admitted")
	}
	if d, b, _ := g.begin("acct", t0, true); d != nil || b == nil {
		t.Fatal("a forced refresh must join, not duplicate, the in-flight reconcile")
	}
	if d, _, _ := g.begin("other", t0, false); d == nil {
		t.Fatal("another account has its own flight")
	}
	g.finish("acct", done)
	select {
	case <-busy:
	default:
	}

	d, b, wait := g.begin("acct", t0.Add(5*time.Second), false)
	if d != nil || b != nil || wait != 15*time.Second {
		t.Fatalf("inside the interval: done=%v busy=%v wait=%s, want a 15s wait", d != nil, b != nil, wait)
	}
	d, _, _ = g.begin("acct", t0.Add(5*time.Second), true)
	if d == nil {
		t.Fatal("a user-forced refresh bypasses the interval")
	}
	g.finish("acct", d)
	if d, _, _ := g.begin("acct", t0.Add(26*time.Second), false); d == nil {
		t.Fatal("past the interval the reconcile is admitted again")
	}
}

// Only a user-forced refresh bypasses the interval: a routine gather inside it
// falls back to the cursor-gated scan (which cannot see the run here), while a
// forced one — what __cli_usage_refresh__ sends — reconciles from the floor.
func TestCodexParse_ForcedRefreshBypassesIntervalRoutineDoesNot(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-2 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	codexForcedReconcileMinInterval = time.Hour
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	writeCodexRunRollout(t, f.home, "run", runStart, runStart.Add(30*time.Second), runStart.Add(40*time.Second), true,
		[]map[string]any{codexRateLimitFrame(81, 50, now)})
	f.advanceCursorPast(t, now.Add(-10*time.Second), now)
	codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		codexOweRunRefresh(snap, runStart, now.Add(-30*time.Second))
	})
	codexUsageRefresh.mu.Lock()
	codexUsageRefresh.lastRun[f.fp] = time.Now()
	codexUsageRefresh.mu.Unlock()

	usage, _ := codexUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{}, time.Now())
	if got := metricObservedAt(t, codexSessionMetric(t, usage.Metrics)); !got.Before(runStart) {
		t.Fatalf("a routine gather inside the interval must not force a reconcile; observedAt=%s", got)
	}

	usage, _ = codexUsageParser{}.ParseContext(WithCodexUsageForceRefresh(context.Background()), "", detectedCLIAgent{}, time.Now())
	session := codexSessionMetric(t, usage.Metrics)
	if got := metricObservedAt(t, session); got.UnixMilli() < runStart.UnixMilli() {
		t.Fatalf("forced refresh observedAt %s predates run start %s", got, runStart)
	}
	if usage.Notice != "" {
		t.Fatalf("a paid debt must not warn: %q", usage.Notice)
	}
}

// codexRunHookRecorder replaces the two session-manager lifecycle hooks so the
// manager tests can assert when, and how often, they fire.
type codexRunHookRecorder struct {
	mu      sync.Mutex
	started []time.Time
	settled []time.Time
}

func recordCodexRunHooks(t *testing.T) *codexRunHookRecorder {
	t.Helper()
	rec := &codexRunHookRecorder{}
	prev := codexRunHookOverride.Swap(&codexRunHooks{
		started: func(at time.Time) {
			rec.mu.Lock()
			rec.started = append(rec.started, at)
			rec.mu.Unlock()
		},
		settled: func(at time.Time) {
			rec.mu.Lock()
			rec.settled = append(rec.settled, at)
			rec.mu.Unlock()
		},
	})
	t.Cleanup(func() { codexRunHookOverride.Store(prev) })
	return rec
}

func (r *codexRunHookRecorder) counts() (started, settled int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.started), len(r.settled)
}

// waitSettled polls until at least n settle calls were recorded (the hook runs
// on the manager's exit goroutine) and returns the recorded floors.
func (r *codexRunHookRecorder) waitSettled(t *testing.T, n int) []time.Time {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		if len(r.settled) >= n {
			out := append([]time.Time(nil), r.settled...)
			r.mu.Unlock()
			return out
		}
		r.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("settle hook fired fewer than %d times", n)
	return nil
}

// The mtime anchor describes the file's newest append, so it may only stand in
// for a frame that can honestly claim it. A multi-turn rollout whose other
// frames state their times keeps a stale timestamp-less frame dropped — even
// under a forced reconcile whose floor the mtime clears — instead of
// republishing that old percentage as post-run evidence.
func TestCodexBucketsFromRolloutFile_InferredAnchorScope(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	floor := now.Add(-time.Minute)
	stale := `{"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":91,"window_minutes":300}}}}`
	statedAt := now.Add(-2 * time.Hour)
	stated := `{"timestamp":"` + statedAt.UTC().Format(time.RFC3339Nano) +
		`","type":"event_msg","payload":{"type":"token_count","rate_limits":{"secondary":{"used_percent":40,"window_minutes":10080}}}}`
	fresher := `{"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":12,"window_minutes":300}}}}`
	completion := `{"type":"event_msg","payload":{"type":"turn.completed","turn_id":"t-1"}}`
	// Mentions rate_limits (so it passes the prefilter) but carries no usable
	// telemetry — still a later physical record.
	unparseable := `{"type":"event_msg","payload":{"type":"agent_reasoning","text":"checking rate_limits"}}`

	for _, tc := range []struct {
		name         string
		lines        []string
		wantInferred bool
		wantPercent  float64
	}{
		// The renamed-envelope-key build this fallback exists for: nothing in
		// the file states a time, so the mtime is the only anchor there is.
		{"timestamp-less file anchors its last frame", []string{stale, fresher}, true, 12},
		// A build that does stamp its envelopes: the timestamp-less frame is an
		// anomaly from an earlier turn, not the finished run's evidence.
		{"a stated frame anywhere disables inference", []string{stale, stated}, false, 0},
		{"a stated frame before the candidate disables it too", []string{stated, stale}, false, 0},
		// The mtime belongs to the file's newest append. When that append is a
		// non-metric record — a turn completion, a reasoning item — the numeric
		// frame before it is NOT what the mtime describes, so nothing may be
		// inferred and the run's debt stays owed.
		{"a non-metric last record invalidates the candidate", []string{stale, fresher, completion}, false, 0},
		{"a non-telemetry rate-limit line invalidates it too", []string{stale, fresher, unparseable}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rollout.jsonl")
			if err := os.WriteFile(path, []byte(strings.Join(tc.lines, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			mtime := now.Add(-10 * time.Second)
			if err := os.Chtimes(path, mtime, mtime); err != nil {
				t.Fatal(err)
			}

			buckets, _, _, _, _ := codexBucketsFromRolloutFile(
				withCodexForcedReconcile(context.Background(), floor), path, now)
			b, found := buckets[codexWindowPrimary][codexLegacyLimitID]
			if !tc.wantInferred {
				if found {
					t.Fatalf("primary=%+v, want the timestamp-less frame dropped", b)
				}
				return
			}
			if !found {
				t.Fatalf("primary missing, want the last frame anchored at the mtime")
			}
			if b.UsedPercentage != tc.wantPercent {
				t.Fatalf("used=%v, want %v — only the final frame may claim the mtime", b.UsedPercentage, tc.wantPercent)
			}
			if !b.Inferred {
				t.Fatalf("bucket=%+v, want Inferred so it cannot outrank a stated observation", b)
			}
			if b.ObservedAtMs != mtime.UnixMilli() {
				t.Fatalf("observedAt=%d, want the file mtime %d", b.ObservedAtMs, mtime.UnixMilli())
			}
		})
	}
}

// A forced reconcile runs inside a gather that owns a deadline. A wedged holder
// of either cache lock must cost that gather the bounded wait and no more —
// the transaction gives up (leaving the cursor unadvanced and the debt owed)
// rather than blocking the refresh receipt and every later provider.
// Bounded cache locking deliberately lets a settle's write be REFUSED. The debt
// must then be RETAINED and written by a later attempt: dropping it retires a
// finished run with neither a refresh nor the persisted marker a restart pays.
func TestCodexRefreshAfterRun_RetainsDebtWhenTheCacheWriteIsRefused(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-2 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	// No rollout evidence exists, so nothing but the retry can record the debt.
	codexRefreshAfterRunRetryDelay = 300 * time.Millisecond

	prevWait, prevPoll := codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll
	codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = 50*time.Millisecond, time.Millisecond
	t.Cleanup(func() { codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = prevWait, prevPoll })

	// Wedge the in-process gate across the settle's write, then free it well
	// inside the worker's retry window.
	codexRateLimitMu.Lock()
	go func() {
		time.Sleep(80 * time.Millisecond)
		codexRateLimitMu.Unlock()
	}()

	triggerCodexUsageRefreshAfterRun(runStart)
	waitCodexUsageRefreshIdle(t)

	snap := f.snapshot(t)
	if snap.RefreshOwedAtMs == 0 {
		t.Fatalf("refused settle lost the debt: %+v", snap)
	}
	if snap.RunFloorMs != runStart.UnixMilli() {
		t.Fatalf("retained debt recorded floor %d, want the run start %d", snap.RunFloorMs, runStart.UnixMilli())
	}
}

func TestCodexRateLimitCacheTransaction_BoundedWaitOnWedgedLocks(t *testing.T) {
	now := time.Now()
	cache := filepath.Join(t.TempDir(), "codex_rate_limits.json")

	prevWait, prevPoll := codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll
	codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = 80*time.Millisecond, time.Millisecond
	t.Cleanup(func() { codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = prevWait, prevPoll })

	t.Run("in-process gate", func(t *testing.T) {
		codexRateLimitMu.Lock()
		defer codexRateLimitMu.Unlock()
		start := time.Now()
		ran := false
		ok := codexRateLimitCacheTransaction(context.Background(), cache, now, true, func(*codexRateLimitSnapshot) bool {
			ran = true
			return true
		})
		if ok || ran {
			t.Fatalf("ok=%v ran=%v, want the wedged gate to be given up on", ok, ran)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("waited %s, want the bounded %s", elapsed, codexRateLimitCacheLockWait)
		}
	})

	t.Run("caller deadline clamps the wait", func(t *testing.T) {
		codexRateLimitCacheLockWait = 30 * time.Second
		codexRateLimitMu.Lock()
		defer codexRateLimitMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		if codexRateLimitCacheTransaction(ctx, cache, now, true, func(*codexRateLimitSnapshot) bool { return true }) {
			t.Fatal("want the transaction to give up rather than write")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("waited %s, want the wait clamped to the caller's deadline", elapsed)
		}
	})
}

// The app-server spells its turn-completion notification differently across
// builds: bare event, the `codex/event/` prefix, and — in Codex 0.144's
// generated JSON-RPC schema — the all-slash `turn/completed` with no nested
// event type at all. All three must settle the turn.
func TestCodexRunCompletionFrame_AcceptsSlashFormMethod(t *testing.T) {
	completing := []string{
		`{"type":"turn.completed"}`,
		`{"type":"thread.completed"}`,
		`{"jsonrpc":"2.0","method":"codex/event/turn.completed","params":{"msg":{"type":"turn.completed"}}}`,
		// Codex 0.144.0-alpha.4 `app-server generate-json-schema`.
		`{"jsonrpc":"2.0","method":"turn/completed","params":{"turnId":"turn_1"}}`,
		`{"jsonrpc":"2.0","method":"thread/completed","params":{"threadId":"th_1"}}`,
		`{"jsonrpc":"2.0","method":"codex/event/turn/completed","params":{}}`,
	}
	for _, line := range completing {
		if !codexRunCompletionFrame(line) {
			t.Errorf("want a completion frame: %s", line)
		}
	}
	notCompleting := []string{
		`{"jsonrpc":"2.0","method":"turn/started","params":{}}`,
		`{"type":"item.completed"}`,
		`{"jsonrpc":"2.0","method":"item/completed","params":{}}`,
		`{"jsonrpc":"2.0","method":"turn/failed","params":{}}`,
		`not json, completed`,
	}
	for _, line := range notCompleting {
		if codexRunCompletionFrame(line) {
			t.Errorf("must not settle a turn: %s", line)
		}
	}
}

// A run that starts while an OLDER run's debt is still standing parks its own
// floor. Settling that older debt with evidence taken before this run began
// must promote the parked floor rather than drop it — otherwise a crash before
// the newer run settles leaves startup unable to see it was interrupted.
func TestCodexArmRunFloor_ParkedFloorSurvivesAnOlderDebtSettling(t *testing.T) {
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	runA, runB := t0, t0.Add(5*time.Minute)
	snap := codexRateLimitSnapshot{}
	codexArmRunFloor(&snap, runA)
	codexOweRunRefresh(&snap, runA, t0.Add(4*time.Minute))
	codexArmRunFloor(&snap, runB)
	if snap.RunFloorMs != runA.UnixMilli() || snap.ActiveRunFloorMs != runB.UnixMilli() {
		t.Fatalf("the debt keeps runA's floor and runB parks its own: %+v", snap)
	}
	// Evidence from between the two starts pays runA's debt but says nothing
	// about runB.
	snap.Contributors = map[string]map[string]codexRateLimitBucket{
		"5h": {"primary": {ObservedAtMs: t0.Add(2 * time.Minute).UnixMilli(), UsedPercentage: 40}},
	}
	codexSettleRunFreshness(&snap, t0.Add(6*time.Minute))
	if snap.RefreshOwedAtMs != 0 {
		t.Fatalf("runA's debt is covered and must clear: %+v", snap)
	}
	if snap.RunFloorMs != runB.UnixMilli() || snap.ActiveRunFloorMs != 0 {
		t.Fatalf("runB's floor must be promoted, not forgotten: %+v", snap)
	}
	// A crash here: startup must still classify runB as interrupted.
	state := codexRunFreshnessFromView(codexCacheView{
		contributors: snap.Contributors,
		runFloorMs:   snap.RunFloorMs,
	}, t0.Add(7*time.Minute))
	if !state.interrupted || state.owed {
		t.Fatalf("runB must read as interrupted after a restart: %+v", state)
	}
}

// Settling the parked floor must not resurrect a run the caller already
// settled, and an expired debt still hands the active run its floor.
func TestCodexActiveRunFloor_ClearedWhenItsOwnRunSettles(t *testing.T) {
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	runA, runB := t0, t0.Add(5*time.Minute)

	settled := codexRateLimitSnapshot{}
	codexArmRunFloor(&settled, runA)
	codexOweRunRefresh(&settled, runA, t0.Add(4*time.Minute))
	codexArmRunFloor(&settled, runB)
	codexOweRunRefresh(&settled, runB, t0.Add(6*time.Minute))
	if settled.RunFloorMs != runB.UnixMilli() || settled.ActiveRunFloorMs != 0 {
		t.Fatalf("runB settling coalesces onto its own floor: %+v", settled)
	}

	expired := codexRateLimitSnapshot{}
	codexArmRunFloor(&expired, runA)
	codexOweRunRefresh(&expired, runA, t0.Add(4*time.Minute))
	codexArmRunFloor(&expired, runB)
	codexSettleRunFreshness(&expired, t0.Add(4*time.Minute).Add(codexRefreshOwedMaxAge+time.Minute))
	if expired.RefreshOwedAtMs != 0 || expired.RunFloorMs != runB.UnixMilli() || expired.ActiveRunFloorMs != 0 {
		t.Fatalf("an expired debt drops its own floor but hands over runB's: %+v", expired)
	}
}

// Every arm is written by its own spawned goroutine, so two starts can reach the
// snapshot out of order. The late, OLDER write must not lower the floor: a crash
// before the newer run settled would otherwise be covered by evidence that
// predates it, and that run would never be classified as interrupted.
func TestCodexArmRunFloor_NeverMovesTheFloorBackwards(t *testing.T) {
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	runA, runB := t0, t0.Add(5*time.Minute)

	// No debt outstanding: B lands first, A's delayed write must be a no-op.
	reordered := codexRateLimitSnapshot{}
	codexArmRunFloor(&reordered, runB)
	codexArmRunFloor(&reordered, runA)
	if reordered.RunFloorMs != runB.UnixMilli() || reordered.ActiveRunFloorMs != 0 {
		t.Fatalf("the newest start must hold the floor: %+v", reordered)
	}
	state := codexRunFreshnessFromView(codexCacheView{
		contributors: map[string]map[string]codexRateLimitBucket{
			"5h": {"primary": {ObservedAtMs: t0.Add(2 * time.Minute).UnixMilli(), UsedPercentage: 40}},
		},
		runFloorMs: reordered.RunFloorMs,
	}, t0.Add(6*time.Minute))
	if !state.interrupted {
		t.Fatalf("evidence taken before runB started cannot cover it: %+v", state)
	}

	// A debt outstanding: the same reordering must not lower the owed floor
	// either, and the older start is already covered by it, so nothing parks.
	owed := codexRateLimitSnapshot{}
	codexArmRunFloor(&owed, runB)
	codexOweRunRefresh(&owed, runB, t0.Add(6*time.Minute))
	codexArmRunFloor(&owed, runA)
	if owed.RunFloorMs != runB.UnixMilli() || owed.ActiveRunFloorMs != 0 {
		t.Fatalf("a late older start must not lower an owed floor: %+v", owed)
	}

	// A floor parked behind a debt that has since cleared is folded in, not lost,
	// when the next run arms.
	parked := codexRateLimitSnapshot{}
	codexArmRunFloor(&parked, runA)
	codexOweRunRefresh(&parked, runA, t0.Add(4*time.Minute))
	codexArmRunFloor(&parked, runB)
	parked.RefreshOwedAtMs, parked.RefreshOwedAttempts = 0, 0
	codexArmRunFloor(&parked, runB.Add(time.Minute))
	if parked.RunFloorMs != runB.Add(time.Minute).UnixMilli() || parked.ActiveRunFloorMs != 0 {
		t.Fatalf("a parked floor is folded in once nothing is owed: %+v", parked)
	}
}

// Startup pays one owed debt. If that reconcile finds evidence covering the
// older run and PROMOTES a newer floor parked behind it, that newer run — whose
// process is equally gone — must become debt of its own: routine gathers force
// only on `owed`, so an interrupted floor left un-owed is never refreshed.
func TestPayOwedCodexUsageRefresh_OwesAPromotedInterruptedRun(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	runA, runB := now.Add(-10*time.Minute), now.Add(-2*time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	// The reading on disk predates runA, so the seeded debt is genuinely unpaid.
	f.seedPreRunReading(t, now.Add(-15*time.Minute), now)
	// The only evidence the startup reconcile can find sits BETWEEN the two
	// starts: it covers runA and says nothing about runB.
	writeCodexRunRollout(t, f.home, "between", runA, now.Add(-6*time.Minute), now.Add(-6*time.Minute), true,
		[]map[string]any{{"primary": map[string]any{"used_percent": 55, "window_minutes": 300, "resets_in_seconds": 3600}}})
	if !codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		snap.RunFloorMs = runA.UnixMilli()
		snap.RefreshOwedAtMs = now.Add(-9 * time.Minute).UnixMilli()
		snap.ActiveRunFloorMs = runB.UnixMilli()
	}) {
		t.Fatal("seeding the owed + parked state was refused")
	}
	if snap := f.snapshot(t); snap.RefreshOwedAtMs == 0 || snap.ActiveRunFloorMs != runB.UnixMilli() {
		t.Fatalf("fixture must start with runA owed and runB parked: %+v", snap)
	}

	payOwedCodexUsageRefresh()
	waitCodexUsageRefreshIdle(t)

	snap := f.snapshot(t)
	if snap.RunFloorMs != runB.UnixMilli() {
		t.Fatalf("runB's parked floor must be promoted by the reconcile: %+v", snap)
	}
	if snap.RefreshOwedAtMs == 0 {
		t.Fatalf("the promoted interrupted run must be owed a refresh of its own: %+v", snap)
	}
	if snap.ActiveRunFloorMs != 0 {
		t.Fatalf("nothing is parked behind runB's own debt: %+v", snap)
	}
	// Sanity: the reconcile really did land runA's evidence.
	if got := codexLatestContributorObservation(snap.Contributors); got.Before(now.Add(-7 * time.Minute)) {
		t.Fatalf("latest observation %s, want the rollout frame at %s", got, now.Add(-6*time.Minute))
	}
}

// The floor a turn is measured against is the moment the turn was REQUESTED.
// Recognizing the request shapes keeps codex_appserver.go free of JSON-RPC
// semantics, exactly like codexRunCompletionFrame.
func TestCodexRunStartFrame(t *testing.T) {
	for _, line := range []string{
		`{"jsonrpc":"2.0","id":3,"method":"turn/start","params":{"threadId":"thr","input":[]}}`,
		`{"jsonrpc":"2.0","id":3,"method":"turn.start","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"codex/turn/start","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"sendUserTurn","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"sendUserMessage","params":{}}`,
	} {
		if !codexRunStartFrame(line) {
			t.Errorf("codexRunStartFrame(%s) = false, want true", line)
		}
	}
	for _, line := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{}}`,
		`{"jsonrpc":"2.0","method":"turn/completed","params":{}}`,
		`{"jsonrpc":"2.0","id":4,"method":"account/rateLimits/read","params":{}}`,
		`not json`,
		``,
	} {
		if codexRunStartFrame(line) {
			t.Errorf("codexRunStartFrame(%s) = true, want false", line)
		}
	}
}

// Only a frame that DEMONSTRATES a turn may open a utilization run the
// transport never saw requested. Responses and notifications that arrive while
// no turn is running — above all the between-turn `account/rateLimits/read`
// reply, whose numeric reading is captured a moment before this check — must
// not: a run opened on one anchors its floor after that reading and, when the
// client then closes the app-server, settles into a debt no rollout can pay.
func TestCodexRunProgressFrame(t *testing.T) {
	for _, line := range []string{
		`{"jsonrpc":"2.0","method":"item/started","params":{"item":{"type":"agent_message"}}}`,
		`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"delta":"hi"}}`,
		`{"jsonrpc":"2.0","method":"turn/started","params":{"turnId":"turn_1"}}`,
		`{"jsonrpc":"2.0","id":7,"method":"item/commandExecution/requestApproval","params":{}}`,
		`{"jsonrpc":"2.0","method":"codex/event/agent_message","params":{"msg":{"type":"agent_message"}}}`,
		`{"jsonrpc":"2.0","method":"codex/event/task_started","params":{"msg":{"type":"task_started"}}}`,
		`{"type":"item.completed","item":{"type":"agent_message"}}`,
	} {
		if !codexRunProgressFrame(line) {
			t.Errorf("codexRunProgressFrame(%s) = false, want true", line)
		}
	}
	for _, line := range []string{
		// The between-turn reading the fallback must never open a run on.
		`{"jsonrpc":"2.0","id":99,"result":{"rateLimits":{"primary":{"used_percent":7,"window_minutes":300}}}}`,
		`{"jsonrpc":"2.0","method":"account/rateLimits/updated","params":{"rateLimits":{}}}`,
		`{"jsonrpc":"2.0","method":"codex/event/token_count","params":{"msg":{"type":"token_count"}}}`,
		// Initialization and thread traffic.
		`{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"codex","version":"0.144.0"}}}`,
		`{"jsonrpc":"2.0","method":"initialized","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thr_1"}}}`,
		`{"jsonrpc":"2.0","method":"thread/started","params":{}}`,
		// Plain responses carry no method; a turn/start ack is not the turn.
		`{"jsonrpc":"2.0","id":3,"result":{"turnId":"turn_1"}}`,
		`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`,
		// Completions settle a run; they never open one.
		`{"jsonrpc":"2.0","method":"turn/completed","params":{}}`,
		`{"type":"thread.completed"}`,
		`not json`,
		``,
	} {
		if codexRunProgressFrame(line) {
			t.Errorf("codexRunProgressFrame(%s) = true, want false", line)
		}
	}
}

// A run start whose cache write the bounded locks refuse must be RETRIED, not
// dropped: an unpersisted floor is a run startup cannot classify as
// interrupted, so a crash or self-update before it settles loses its refresh
// for good.
func TestArmCodexUsageRunFloor_RetriesARefusedWrite(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)

	prevDelay := codexRunFloorWriteRetryDelay
	codexRunFloorWriteRetryDelay = 50 * time.Millisecond
	prevWait, prevPoll := codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll
	codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = 30*time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		codexRunFloorWriteRetryDelay = prevDelay
		codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = prevWait, prevPoll
	})

	// Wedge the in-process gate across the first write, then free it inside the
	// retry window.
	codexRateLimitMu.Lock()
	go func() {
		time.Sleep(60 * time.Millisecond)
		codexRateLimitMu.Unlock()
	}()

	armCodexUsageRunFloor(runStart)
	waitCodexUsageRefreshIdle(t)

	if snap := f.snapshot(t); snap.RunFloorMs != runStart.UnixMilli() {
		t.Fatalf("armed floor %d, want the retried run start %d", snap.RunFloorMs, runStart.UnixMilli())
	}
}

// A forced reconcile whose attempt-counter write is refused must keep the
// attempt: codexStaleRunNotice only warns once RefreshOwedAttempts reaches
// codexRefreshAfterRunMaxAttempts, so an uncounted attempt leaves a run whose
// telemetry never appears reconciling on every gather while the card never
// explains why it looks old.
func TestCodexRecordRefreshAttempt_RetainsARefusedCount(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-2 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	if !codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		codexOweRunRefresh(snap, runStart, now)
	}) {
		t.Fatal("seeding the debt failed")
	}

	prevWait, prevPoll := codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll
	codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = 30*time.Millisecond, time.Millisecond
	t.Cleanup(func() { codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = prevWait, prevPoll })

	func() {
		codexRateLimitMu.Lock()
		defer codexRateLimitMu.Unlock()
		codexRecordRefreshAttempt(f.fp, 1)
	}()
	if snap := f.snapshot(t); snap.RefreshOwedAttempts != 0 {
		t.Fatalf("refused write recorded %d attempts, want none on disk", snap.RefreshOwedAttempts)
	}

	// The next write folds the retained attempt in, so the run reaches the
	// notice threshold on the attempts it actually spent.
	codexRecordRefreshAttempt(f.fp, 1)
	snap := f.snapshot(t)
	if snap.RefreshOwedAttempts != 2 {
		t.Fatalf("RefreshOwedAttempts=%d, want the refused attempt folded in (2)", snap.RefreshOwedAttempts)
	}
	if notice := codexStaleRunNotice(codexRunFreshnessForAccount(f.fp, now)); notice == "" {
		t.Fatal("want the stale-run notice once every attempt is counted")
	}
}
