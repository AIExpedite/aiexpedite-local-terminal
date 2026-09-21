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
	mu       sync.Mutex
	started  []time.Time
	settled  []time.Time
	disarmed []codexRunDisarm
}

// codexRunDisarm is one recorded codexUsageRunDisarmed call.
type codexRunDisarm struct {
	floor, fallback time.Time
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
		disarmed: func(floor, fallback time.Time) {
			rec.mu.Lock()
			rec.disarmed = append(rec.disarmed, codexRunDisarm{floor: floor, fallback: fallback})
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

// A refused settle write is a lock race, not a reconcile: landing the retained
// debt gets its own write budget and must not spend the two reconcile attempts,
// or the worker retires with the debt still only in memory — never retried
// without another Codex run, and dropped outright by a restart.
func TestCodexRefreshAfterRun_RetainedDebtOutlivesTheReconcileAttempts(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-2 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	prevAttempts, prevDelay := codexRunFloorWriteAttempts, codexRunFloorWriteRetryDelay
	codexRunFloorWriteAttempts, codexRunFloorWriteRetryDelay = 3, 100*time.Millisecond
	prevWait, prevPoll := codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll
	codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = 50*time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		codexRunFloorWriteAttempts, codexRunFloorWriteRetryDelay = prevAttempts, prevDelay
		codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = prevWait, prevPoll
	})

	// Wedge the in-process gate past both reconcile attempts (the fixture's
	// 10ms retry) but inside the write budget's third try.
	codexRateLimitMu.Lock()
	go func() {
		time.Sleep(250 * time.Millisecond)
		codexRateLimitMu.Unlock()
	}()

	triggerCodexUsageRefreshAfterRun(runStart)
	waitCodexUsageRefreshIdle(t)

	snap := f.snapshot(t)
	if snap.RefreshOwedAtMs == 0 || snap.RunFloorMs != runStart.UnixMilli() {
		t.Fatalf("retained debt never landed once the reconcile attempts were spent: %+v", snap)
	}
	if _, still := codexUsageRefresh.takeDebt(f.fp); still {
		t.Fatal("debt landed on disk but is still retained in the gate")
	}
}

// A gather that spends a forced reconcile ON a debt counts it as one of that
// debt's bounded attempts. Without the count, a debt carried over from a
// previous process — whose startup replay spends one attempt and then retires
// — would be rescanned by every later gather while never reaching the
// stale-notice threshold, so the card would age it out without ever saying why
// the reading looks old.
func TestCodexReconcileForGather_CountsAnAttemptOnTheDebt(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-2 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	// The state a startup replay leaves behind: still owed, one attempt spent.
	if !codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		codexOweRunRefresh(snap, runStart, now)
		codexCountRefreshAttempt(snap, codexDebtID{floorMs: runStart.UnixMilli(), owedAtMs: now.UnixMilli()})
	}) {
		t.Fatal("seeding the debt failed")
	}

	// No rollout telemetry exists for this run, so the reconcile comes up empty
	// and the debt stays owed — the case the notice exists for.
	ctx, cancel := context.WithTimeout(context.Background(), codexForcedReconcileBudget)
	defer cancel()
	codexReconcileForGather(ctx, codexHomeBase(), f.fp, now, false)

	snap := f.snapshot(t)
	if snap.RefreshOwedAtMs == 0 {
		t.Fatalf("debt was cleared without any covering observation: %+v", snap)
	}
	if snap.RefreshOwedAttempts != codexRefreshAfterRunMaxAttempts {
		t.Fatalf("RefreshOwedAttempts=%d, want the gather's reconcile counted (%d)",
			snap.RefreshOwedAttempts, codexRefreshAfterRunMaxAttempts)
	}
	if notice := codexStaleRunNotice(codexRunFreshnessForAccount(f.fp, now)); notice == "" {
		t.Fatal("want the stale-run notice once every bounded attempt is spent")
	}
}

// A reconcile that PAYS the debt must not leave an attempt behind it: the
// clearing transaction zeroes the count, and counting it afterwards would
// re-create a debt-less count the next run's notice could trip over.
func TestCodexReconcileForGather_PaidDebtKeepsNoAttempt(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-2 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	writeCodexRunRollout(t, f.home, "run", runStart, runStart.Add(30*time.Second), runStart.Add(40*time.Second), true,
		[]map[string]any{codexRateLimitFrame(61, 64, now)})
	if !codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		codexOweRunRefresh(snap, runStart, now)
	}) {
		t.Fatal("seeding the debt failed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), codexForcedReconcileBudget)
	defer cancel()
	codexReconcileForGather(ctx, codexHomeBase(), f.fp, now, false)

	snap := f.snapshot(t)
	if snap.RefreshOwedAtMs != 0 || snap.RefreshOwedAttempts != 0 {
		t.Fatalf("paid debt left bookkeeping behind: %+v", snap)
	}
}

// A debt the worker could not land at all is flushed by the next gather: the
// parser reads the debt off disk, so a debt retained only in memory would read
// as paid and the gather would neither force a scan nor warn.
func TestCodexReconcileForGather_LandsARetainedDebt(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-2 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	codexUsageRefresh.rememberDebt(f.fp, codexPendingRunDebt{floor: runStart, completedAt: now})

	ctx, cancel := context.WithTimeout(context.Background(), codexForcedReconcileBudget)
	defer cancel()
	codexReconcileForGather(ctx, codexHomeBase(), f.fp, now, false)

	state := codexRunFreshnessForAccount(f.fp, now)
	if !state.owed || state.floor.UnixMilli() != runStart.UnixMilli() {
		t.Fatalf("gather left the retained debt off disk: %+v", state)
	}
	if !codexGateHasRun(f.fp) {
		t.Fatal("gather did not force a reconcile on the debt it had just landed")
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

// An observation that covers BOTH the owed run and the parked run behind it
// must watermark the promoted floor too. The contributor carrying that
// observation can be dropped by an authoritative empty snapshot before the
// newer run settles; without the watermark a restart in that window would
// read the already-observed run as interrupted and owe a false debt.
func TestCodexSettleRunFreshness_WatermarksPromotedFloor(t *testing.T) {
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	runA, runB := t0, t0.Add(5*time.Minute)
	snap := codexRateLimitSnapshot{}
	codexArmRunFloor(&snap, runA)
	codexOweRunRefresh(&snap, runA, t0.Add(4*time.Minute))
	codexArmRunFloor(&snap, runB)
	// One observation from inside runB covers runA's debt and runB's start.
	snap.Contributors = map[string]map[string]codexRateLimitBucket{
		"5h": {"primary": {ObservedAtMs: runB.Add(time.Minute).UnixMilli(), UsedPercentage: 40}},
	}
	codexSettleRunFreshness(&snap, t0.Add(7*time.Minute))
	if snap.RefreshOwedAtMs != 0 || snap.RunFloorMs != runB.UnixMilli() || snap.ActiveRunFloorMs != 0 {
		t.Fatalf("runA's debt clears and runB's floor is promoted: %+v", snap)
	}
	if snap.RunFloorPaidMs != runB.UnixMilli() {
		t.Fatalf("the promoted floor is covered by the same observation and must be watermarked: %+v", snap)
	}

	// The contributor is dropped while runB is still open, then the agent
	// restarts: runB was observed and must not read as interrupted.
	snap.Contributors = map[string]map[string]codexRateLimitBucket{}
	codexSettleRunFreshness(&snap, t0.Add(8*time.Minute))
	state := codexRunFreshnessFromView(codexCacheView{
		contributors:   snap.Contributors,
		runFloorMs:     snap.RunFloorMs,
		runFloorPaidMs: snap.RunFloorPaidMs,
	}, t0.Add(9*time.Minute))
	if state.owed || state.interrupted {
		t.Fatalf("an observed promoted run must not resurrect: %+v", state)
	}
	// And when runB settles, its debt is paid on the spot.
	codexOweRunRefresh(&snap, runB, t0.Add(10*time.Minute))
	codexSettleRunFreshness(&snap, t0.Add(10*time.Minute))
	if snap.RefreshOwedAtMs != 0 {
		t.Fatalf("runB's debt is already covered by the watermark: %+v", snap)
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

// A paid run stays paid. Contributors are not permanent — an authoritative
// empty full snapshot drops them — and the retained floor must not then read as
// unobserved, resurrecting the run as interrupted and, after a restart, as a
// fresh debt with a false stale-run warning behind it.
func TestCodexSettleRunFreshness_PaidFloorSurvivesDroppedContributors(t *testing.T) {
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	snap := codexRateLimitSnapshot{}
	codexArmRunFloor(&snap, t0)
	codexOweRunRefresh(&snap, t0, t0.Add(time.Minute))
	snap.Contributors = map[string]map[string]codexRateLimitBucket{
		"5h": {"primary": {ObservedAtMs: t0.Add(90 * time.Second).UnixMilli(), UsedPercentage: 40}},
	}
	codexSettleRunFreshness(&snap, t0.Add(2*time.Minute))
	if snap.RefreshOwedAtMs != 0 || snap.RunFloorPaidMs != t0.UnixMilli() {
		t.Fatalf("a covering observation pays the debt and watermarks the floor: %+v", snap)
	}

	// A later authoritative snapshot legitimately removes every contributor.
	snap.Contributors = map[string]map[string]codexRateLimitBucket{}
	codexSettleRunFreshness(&snap, t0.Add(3*time.Minute))
	state := codexRunFreshnessFromView(codexCacheView{
		contributors:   snap.Contributors,
		runFloorMs:     snap.RunFloorMs,
		runFloorPaidMs: snap.RunFloorPaidMs,
	}, t0.Add(4*time.Minute))
	if state.owed || state.interrupted {
		t.Fatalf("a paid run must not be resurrected when its contributor is dropped: %+v", state)
	}

	// A NEWER run's floor is not covered by the old watermark.
	codexArmRunFloor(&snap, t0.Add(10*time.Minute))
	fresh := codexRunFreshnessFromView(codexCacheView{
		contributors:   snap.Contributors,
		runFloorMs:     snap.RunFloorMs,
		runFloorPaidMs: snap.RunFloorPaidMs,
	}, t0.Add(11*time.Minute))
	if !fresh.interrupted {
		t.Fatalf("the watermark must only cover floors at or below it: %+v", fresh)
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

	debt := codexDebtID{floorMs: runStart.UnixMilli(), owedAtMs: now.UnixMilli()}
	prevWait, prevPoll := codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll
	codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = 30*time.Millisecond, time.Millisecond
	t.Cleanup(func() { codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = prevWait, prevPoll })

	func() {
		codexRateLimitMu.Lock()
		defer codexRateLimitMu.Unlock()
		codexRecordRefreshAttempt(f.fp, debt, 1)
	}()
	if snap := f.snapshot(t); snap.RefreshOwedAttempts != 0 {
		t.Fatalf("refused write recorded %d attempts, want none on disk", snap.RefreshOwedAttempts)
	}

	// The next write folds the retained attempt in, so the run reaches the
	// notice threshold on the attempts it actually spent.
	codexRecordRefreshAttempt(f.fp, debt, 1)
	snap := f.snapshot(t)
	if snap.RefreshOwedAttempts != 2 {
		t.Fatalf("RefreshOwedAttempts=%d, want the refused attempt folded in (2)", snap.RefreshOwedAttempts)
	}
	if notice := codexStaleRunNotice(codexRunFreshnessForAccount(f.fp, now)); notice == "" {
		t.Fatal("want the stale-run notice once every attempt is counted")
	}
}

// A turn request whose write never reached the child is withdrawn from the
// PERSISTED floor too — left there, the next process start would read it as an
// interrupted run and owe a refresh no telemetry can pay. Only the exact floor
// rolls back: it returns to the newest turn its manager still has open, a
// newer floor belongs to another run and stays, and a start parked behind a
// debt is unparked without touching the debt.
func TestCodexDisarmRunFloor_RollsBackOnlyTheFailedArm(t *testing.T) {
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	runA, runB, runC := t0, t0.Add(5*time.Minute), t0.Add(10*time.Minute)

	// The failed arm coalesced onto runB with runA still open: back to runA.
	open := codexRateLimitSnapshot{}
	codexArmRunFloor(&open, runA)
	codexArmRunFloor(&open, runB)
	if !codexDisarmRunFloor(&open, runB.UnixMilli(), runA.UnixMilli()) || open.RunFloorMs != runA.UnixMilli() {
		t.Fatalf("the failed arm must fall back to the turn still open: %+v", open)
	}

	// Nothing else open: nothing is left to classify as interrupted.
	alone := codexRateLimitSnapshot{}
	codexArmRunFloor(&alone, runB)
	if !codexDisarmRunFloor(&alone, runB.UnixMilli(), 0) || alone.RunFloorMs != 0 {
		t.Fatalf("a lone failed arm must leave no floor: %+v", alone)
	}
	if state := codexRunFreshnessFromView(codexCacheView{runFloorMs: alone.RunFloorMs}, runB.Add(time.Minute)); state.interrupted {
		t.Fatalf("a withdrawn run must not read as interrupted: %+v", state)
	}

	// A newer run armed since: its floor is not this arm's to roll back.
	newer := codexRateLimitSnapshot{}
	codexArmRunFloor(&newer, runB)
	codexArmRunFloor(&newer, runC)
	if codexDisarmRunFloor(&newer, runB.UnixMilli(), 0) || newer.RunFloorMs != runC.UnixMilli() {
		t.Fatalf("a newer run's floor must survive an older arm's withdrawal: %+v", newer)
	}

	// Parked behind runA's debt: the debt keeps its floor, the park is undone.
	parked := codexRateLimitSnapshot{}
	codexArmRunFloor(&parked, runA)
	codexOweRunRefresh(&parked, runA, t0.Add(4*time.Minute))
	codexArmRunFloor(&parked, runB)
	if !codexDisarmRunFloor(&parked, runB.UnixMilli(), 0) || parked.ActiveRunFloorMs != 0 || parked.RunFloorMs != runA.UnixMilli() || parked.RefreshOwedAtMs == 0 {
		t.Fatalf("withdrawing a parked start must leave the debt alone: %+v", parked)
	}
	// Parked with an older turn of its own still open: that turn takes the park.
	parkedOpen := codexRateLimitSnapshot{}
	codexArmRunFloor(&parkedOpen, runA)
	codexOweRunRefresh(&parkedOpen, runA, t0.Add(4*time.Minute))
	codexArmRunFloor(&parkedOpen, runB)
	codexArmRunFloor(&parkedOpen, runC)
	if !codexDisarmRunFloor(&parkedOpen, runC.UnixMilli(), runB.UnixMilli()) || parkedOpen.ActiveRunFloorMs != runB.UnixMilli() {
		t.Fatalf("the open turn must take over the park: %+v", parkedOpen)
	}
	// A debt that coalesced onto the failed arm (its turn settled after the
	// failed request) falls back to that settled turn's floor, so the debt
	// waits on evidence covering the run that actually happened.
	coalesced := codexRateLimitSnapshot{}
	codexArmRunFloor(&coalesced, runA)
	codexArmRunFloor(&coalesced, runB)
	codexOweRunRefresh(&coalesced, runA, t0.Add(6*time.Minute))
	if !codexDisarmRunFloor(&coalesced, runB.UnixMilli(), runA.UnixMilli()) || coalesced.RunFloorMs != runA.UnixMilli() || coalesced.RefreshOwedAtMs == 0 {
		t.Fatalf("the debt must fall back to the turn that ran: %+v", coalesced)
	}
}

// Arms are written by their own retried goroutines, so a withdrawal can reach
// the cache BEFORE the arm it withdraws. The late arm must then be dropped:
// the run never happened, and a floor for it would be read as an interrupted
// run at the next start. And a withdrawal the bounded locks refuse is retried,
// like the arm itself, so the floor does not linger on disk.
func TestDisarmCodexUsageRunFloor_DropsALateArmAndRetriesARefusedRollback(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)

	// Withdrawal first, then the arm it withdraws: nothing may reach disk.
	disarmCodexUsageRunFloor(runStart, time.Time{})
	waitCodexUsageRefreshIdle(t)
	armCodexUsageRunFloor(runStart)
	waitCodexUsageRefreshIdle(t)
	if snap := f.snapshot(t); snap.RunFloorMs != 0 {
		t.Fatalf("a withdrawn arm reached disk: RunFloorMs=%d", snap.RunFloorMs)
	}
	if codexUsageRefresh.takeDisarmed(f.fp, runStart.UnixMilli()) {
		t.Fatal("the late arm must consume its withdrawal mark")
	}

	// Arm first, then a withdrawal whose first write is refused.
	armCodexUsageRunFloor(runStart)
	waitCodexUsageRefreshIdle(t)
	if snap := f.snapshot(t); snap.RunFloorMs != runStart.UnixMilli() {
		t.Fatalf("armed floor %d, want %d", snap.RunFloorMs, runStart.UnixMilli())
	}
	prevDelay := codexRunFloorWriteRetryDelay
	codexRunFloorWriteRetryDelay = 50 * time.Millisecond
	prevWait, prevPoll := codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll
	codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = 30*time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		codexRunFloorWriteRetryDelay = prevDelay
		codexRateLimitCacheLockWait, codexRateLimitCacheLockPoll = prevWait, prevPoll
	})
	codexRateLimitMu.Lock()
	go func() {
		time.Sleep(60 * time.Millisecond)
		codexRateLimitMu.Unlock()
	}()
	disarmCodexUsageRunFloor(runStart, time.Time{})
	waitCodexUsageRefreshIdle(t)
	if snap := f.snapshot(t); snap.RunFloorMs != 0 {
		t.Fatalf("the retried withdrawal left RunFloorMs=%d on disk", snap.RunFloorMs)
	}
	if codexUsageRefresh.takeDisarmed(f.fp, runStart.UnixMilli()) {
		t.Fatal("a withdrawal that rolled back the landed arm must clear its mark")
	}
	if state := codexRunFreshnessForAccount(f.fp, now); state.interrupted || state.owed {
		t.Fatalf("a withdrawn run must owe nothing after a restart: %+v", state)
	}
}

// Floors are millisecond-truncated, so two turns armed in the same millisecond
// share a key. When both writes fail before their arm transactions land, the
// two withdrawals must be counted separately: a single mark would be consumed
// by the first late arm, and the second would persist a floor for a turn that
// never reached Codex — read back as an interrupted run at the next start.
func TestDisarmCodexUsageRunFloor_CountsWithdrawalsThatShareAFloor(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)

	// Two withdrawals at one floor, then the two late arms they withdraw.
	disarmCodexUsageRunFloor(runStart, time.Time{})
	disarmCodexUsageRunFloor(runStart, time.Time{})
	waitCodexUsageRefreshIdle(t)
	armCodexUsageRunFloor(runStart)
	armCodexUsageRunFloor(runStart)
	waitCodexUsageRefreshIdle(t)
	if snap := f.snapshot(t); snap.RunFloorMs != 0 || snap.ActiveRunFloorMs != 0 {
		t.Fatalf("a withdrawn arm reached disk: RunFloorMs=%d ActiveRunFloorMs=%d", snap.RunFloorMs, snap.ActiveRunFloorMs)
	}
	if codexUsageRefresh.takeDisarmed(f.fp, runStart.UnixMilli()) {
		t.Fatal("each late arm must consume exactly one withdrawal, leaving none")
	}
	if state := codexRunFreshnessForAccount(f.fp, now); state.interrupted || state.owed {
		t.Fatalf("withdrawn runs must owe nothing after a restart: %+v", state)
	}

	// The count is exact: a third arm at the same floor is a real run and
	// must land.
	armCodexUsageRunFloor(runStart)
	waitCodexUsageRefreshIdle(t)
	if snap := f.snapshot(t); snap.RunFloorMs != runStart.UnixMilli() {
		t.Fatalf("an arm with no withdrawal left must land: RunFloorMs=%d, want %d", snap.RunFloorMs, runStart.UnixMilli())
	}
}

// A run armed under one account must never be settled as the NEXT account's
// debt: the cache holds a single account's readings, so booking account A's
// finished run against B gives B a run floor for a run it never made — which
// ages into a false stale-utilization warning on B's card — while A's promised
// refresh is dropped. The run's account is captured when the floor is armed
// and carried into settlement.
func TestCodexRefreshAfterRun_CredentialsSwapDoesNotBookTheDebtOnTheNextAccount(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)

	codexUsageRunStarted(runStart)
	waitCodexUsageRefreshIdle(t)
	if snap := f.snapshot(t); snap.RunFloorMs != runStart.UnixMilli() {
		t.Fatalf("armed floor %d, want run start %d", snap.RunFloorMs, runStart.UnixMilli())
	}

	// Sign in as a different account mid-run, and let that account take over the
	// cache the way any of its own telemetry would (the rescope drops account
	// A's floor with the rest of A's state).
	helperCodexAuthAt(t, f.home, "other@example.com", now.Add(-time.Minute))
	next := currentCodexAccountFingerprint()
	if next == f.fp {
		t.Fatal("credentials swap did not change the account fingerprint")
	}
	mergeCodexRateLimitCache(f.cache, map[string]codexRateLimitBucket{
		codexWindowPrimary: {
			UsedPercentage: 5, ResetsAtMs: now.Add(3 * time.Hour).UnixMilli(), ObservedAtMs: now.Add(-30 * time.Second).UnixMilli(),
			WindowMinutes: 300, usageKnown: true, resetKnown: true,
		},
	}, nil, now, next)

	codexUsageRunSettled(runStart)
	waitCodexUsageRefreshIdle(t)

	snap := f.snapshot(t)
	if snap.AccountFingerprint != next {
		t.Fatalf("cache rescoped to %q, want the account that is signed in now", snap.AccountFingerprint)
	}
	if snap.RefreshOwedAtMs != 0 {
		t.Fatalf("debt %d booked on the next account, want none", snap.RefreshOwedAtMs)
	}
	if snap.RunFloorMs != 0 || snap.ActiveRunFloorMs != 0 {
		t.Fatalf("run floor %d/%d inherited by the next account, want none", snap.RunFloorMs, snap.ActiveRunFloorMs)
	}
}

// The same binding applies to a withdrawal: the rollback must target the
// account whose floor was armed, not whoever is signed in when the turn write
// turns out to have failed.
func TestDisarmCodexUsageRunFloor_RollsBackTheArmingAccount(t *testing.T) {
	now := time.Now()
	runStart := now.Add(-time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)

	codexUsageRunStarted(runStart)
	waitCodexUsageRefreshIdle(t)

	helperCodexAuthAt(t, f.home, "other@example.com", now.Add(-time.Minute))
	codexUsageRunDisarmed(runStart, time.Time{})
	waitCodexUsageRefreshIdle(t)

	snap := f.snapshot(t)
	if snap.AccountFingerprint != f.fp {
		t.Fatalf("rollback rescoped the cache to %q, want the arming account", snap.AccountFingerprint)
	}
	if snap.RunFloorMs != 0 || snap.ActiveRunFloorMs != 0 {
		t.Fatalf("floor %d/%d survived the rollback, want it withdrawn", snap.RunFloorMs, snap.ActiveRunFloorMs)
	}
}

// An attempt belongs to the debt it was spent on. A gather reads the debt,
// reconciles it, and only then writes the count; a run settling in that window
// installs a NEW debt whose counter was deliberately reset. Counting the older
// reconcile there would march a brand-new run towards the stale warning on
// scans that never looked at its floor — and the retained-count path must not
// carry an increment across generations either.
func TestCodexRecordRefreshAttempt_CountsOnlyTheReconciledDebt(t *testing.T) {
	now := time.Now()
	runA, runB := now.Add(-5*time.Minute), now.Add(-time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	if !codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		codexOweRunRefresh(snap, runA, now)
	}) {
		t.Fatal("seeding debt A failed")
	}
	debtA := codexRunFreshnessForAccount(f.fp, now).debtID()

	// Run B settles before the attempt spent on A is recorded.
	settledB := now.Add(time.Second)
	if !codexRecordRunFreshness(f.fp, settledB, func(snap *codexRateLimitSnapshot) {
		codexOweRunRefresh(snap, runB, settledB)
	}) {
		t.Fatal("settling run B failed")
	}
	codexRecordRefreshAttempt(f.fp, debtA, 1)
	if snap := f.snapshot(t); snap.RefreshOwedAttempts != 0 {
		t.Fatalf("RefreshOwedAttempts=%d after an attempt spent on the previous debt, want 0", snap.RefreshOwedAttempts)
	}

	// The same holds for a count retained by a refused write: it may only land
	// on its own generation, so B's first real attempt counts once.
	codexUsageRefresh.rememberAttempts(f.fp, debtA, 1)
	debtB := codexRunFreshnessForAccount(f.fp, settledB).debtID()
	if debtB == debtA || !debtB.valid() {
		t.Fatalf("debtB=%+v must be a new generation (debtA=%+v)", debtB, debtA)
	}
	codexRecordRefreshAttempt(f.fp, debtB, 1)
	snap := f.snapshot(t)
	if snap.RefreshOwedAttempts != 1 {
		t.Fatalf("RefreshOwedAttempts=%d, want only B's own attempt (1)", snap.RefreshOwedAttempts)
	}
	if notice := codexStaleRunNotice(codexRunFreshnessForAccount(f.fp, settledB)); notice != "" {
		t.Fatalf("stale notice reached on a single attempt: %q", notice)
	}
}

// Every caller truncates its run start to a millisecond, so two overlapping
// turns can be armed at the SAME floor. Each still needs its own account
// binding: if the first settlement consumed a single shared entry, the second
// would read as unbound and — after a mid-run credentials swap — book run A's
// debt against whoever is signed in now.
func TestCodexUsageRefreshGate_ArmedBindingsQueuePerFloor(t *testing.T) {
	g := newCodexUsageRefreshGate()
	floor := time.Now().UnixMilli()
	g.rememberArmedAccount(floor, "acct-a")
	g.rememberArmedAccount(floor, "acct-b")

	first, ok := g.takeArmedAccount(floor)
	if !ok || first != "acct-a" {
		t.Fatalf("first settlement got %q/%v, want acct-a bound", first, ok)
	}
	second, ok := g.takeArmedAccount(floor)
	if !ok || second != "acct-b" {
		t.Fatalf("second settlement got %q/%v, want acct-b bound", second, ok)
	}
	if fp, ok := g.takeArmedAccount(floor); ok {
		t.Fatalf("a third settlement got %q, want the floor forgotten", fp)
	}

	// The cap counts bindings, not keys, so several runs sharing one floor
	// cannot grow the map past its bound; the oldest floor is evicted first.
	for i := 0; i <= codexArmedAccountCap; i++ {
		g.rememberArmedAccount(floor+int64(i%2), "acct-c")
	}
	g.mu.Lock()
	total := g.armedCountLocked()
	g.mu.Unlock()
	if total > codexArmedAccountCap {
		t.Fatalf("retained %d bindings, want at most %d", total, codexArmedAccountCap)
	}
	if _, ok := g.takeArmedAccount(floor + 1); !ok {
		t.Fatal("the newest floor was evicted, want oldest-first eviction")
	}
}

// A backwards clock step (NTP correction, VM resume, manual set) leaves the
// floor, its paid watermark and the observation that paid it dated in the
// future. Every later run then starts BEHIND them, so its start is discarded as
// older, its debt is retired as already paid, and utilization stays stale until
// wall time catches back up. A run start rebases that state onto `now` instead.
func TestCodexArmRunFloor_RebasesStateLeftAfterAClockRollback(t *testing.T) {
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	rolledBack := t0.Add(-time.Hour)

	future := codexRateLimitSnapshot{}
	codexArmRunFloor(&future, t0)
	future.Contributors = map[string]map[string]codexRateLimitBucket{
		"5h": {"primary": {ObservedAtMs: t0.UnixMilli(), UsedPercentage: 40}},
	}
	codexSettleRunFreshness(&future, t0.Add(time.Minute))
	if future.RunFloorPaidMs != t0.UnixMilli() {
		t.Fatalf("the pre-rollback run must be paid before the clock moves: %+v", future)
	}

	// Without the rebase this arm is a no-op and the debt below is born paid.
	if !codexRebaseFutureRunFreshness(&future, rolledBack, rolledBack) {
		t.Fatal("state an hour ahead of the clock must be rebased")
	}
	codexArmRunFloor(&future, rolledBack)
	if future.RunFloorMs != rolledBack.UnixMilli() || future.RunFloorPaidMs != 0 {
		t.Fatalf("the new start must own the floor with nothing paid against it: %+v", future)
	}
	if observed := future.Contributors["5h"]["primary"].ObservedAtMs; observed >= rolledBack.UnixMilli() {
		t.Fatalf("a future-dated observation must be re-dated before the run it predates: %d", observed)
	}

	// The run finishes: its debt now stands, so the post-run worker can pay it.
	codexOweRunRefresh(&future, rolledBack, rolledBack.Add(time.Minute))
	codexSettleRunFreshness(&future, rolledBack.Add(time.Minute))
	state := codexRunFreshnessFromView(codexCacheView{
		contributors:    future.Contributors,
		runFloorMs:      future.RunFloorMs,
		runFloorPaidMs:  future.RunFloorPaidMs,
		refreshOwedAtMs: future.RefreshOwedAtMs,
	}, rolledBack.Add(2*time.Minute))
	if !state.owed {
		t.Fatalf("a run started after the rollback still owes a refresh: %+v %+v", state, future)
	}

	// Ordinary forward-stamped telemetry sits inside the skew tolerance and must
	// NOT be back-dated: that would manufacture a debt on every single run.
	skewed := codexRateLimitSnapshot{
		RunFloorMs:     t0.UnixMilli(),
		RunFloorPaidMs: t0.UnixMilli(),
		Contributors: map[string]map[string]codexRateLimitBucket{
			"5h": {"primary": {ObservedAtMs: t0.Add(codexRunFloorClockSkew - time.Minute).UnixMilli(), UsedPercentage: 40}},
		},
	}
	if codexRebaseFutureRunFreshness(&skewed, t0, t0) {
		t.Fatalf("state within the skew tolerance must be left alone: %+v", skewed)
	}
}

// A backwards correction SMALLER than the provider skew tolerance is still a
// rollback: floors and the debt marker are stamped only from the local clock,
// so one dated ahead of `now` can never be explained by a Codex-stamped
// envelope. Applying the wide tolerance to them left a paid floor parked ahead
// of every new run — and the observation that paid it able to cover them — for
// the whole rollback.
func TestCodexArmRunFloor_RebasesAfterASubSkewClockRollback(t *testing.T) {
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	rolledBack := t0.Add(-2 * time.Minute) // inside codexRunFloorClockSkew.

	snap := codexRateLimitSnapshot{}
	codexArmRunFloor(&snap, t0)
	snap.Contributors = map[string]map[string]codexRateLimitBucket{
		"5h": {"primary": {ObservedAtMs: t0.UnixMilli(), UsedPercentage: 40}},
	}
	codexSettleRunFreshness(&snap, t0.Add(time.Second))
	if snap.RunFloorPaidMs != t0.UnixMilli() {
		t.Fatalf("the pre-rollback run must be paid before the clock moves: %+v", snap)
	}

	if !codexRebaseFutureRunFreshness(&snap, rolledBack, rolledBack) {
		t.Fatal("a floor ahead of the local clock is a rollback whatever the provider skew tolerance allows")
	}
	codexArmRunFloor(&snap, rolledBack)
	if snap.RunFloorMs != rolledBack.UnixMilli() || snap.RunFloorPaidMs != 0 {
		t.Fatalf("the new start must own the floor with nothing paid against it: %+v", snap)
	}
	// The proven rollback tightens the observation ceiling too: left at t0 it
	// would cover — and instantly retire the debt of — the run armed below.
	if observed := snap.Contributors["5h"]["primary"].ObservedAtMs; observed >= rolledBack.UnixMilli() {
		t.Fatalf("a future-dated observation must be re-dated before the run it predates: %d", observed)
	}
	codexOweRunRefresh(&snap, rolledBack, rolledBack.Add(time.Second))
	codexSettleRunFreshness(&snap, rolledBack.Add(time.Second))
	state := codexRunFreshnessFromView(codexCacheView{
		contributors:    snap.Contributors,
		runFloorMs:      snap.RunFloorMs,
		runFloorPaidMs:  snap.RunFloorPaidMs,
		refreshOwedAtMs: snap.RefreshOwedAtMs,
	}, rolledBack.Add(time.Minute))
	if !state.owed {
		t.Fatalf("a run started after the rollback still owes a refresh: %+v %+v", state, snap)
	}

	// A floor a hair ahead of a caller's `now` is the benign in-flight case —
	// `now` was read before a concurrent run armed — and must NOT be rebased.
	inFlight := codexRateLimitSnapshot{RunFloorMs: t0.Add(codexRunFloorLocalSkew - time.Second).UnixMilli()}
	if codexRebaseFutureRunFreshness(&inFlight, t0, t0) {
		t.Fatalf("a floor inside the in-flight tolerance must be left alone: %+v", inFlight)
	}
	if codexRunFreshnessInFuture(codexCacheView{runFloorMs: inFlight.RunFloorMs}, t0) {
		t.Fatal("the read-side test must agree with the rebase it gates")
	}
	if !codexRunFreshnessInFuture(codexCacheView{runFloorMs: t0.Add(codexRunFloorLocalSkew + time.Second).UnixMilli()}, t0) {
		t.Fatal("a floor past the in-flight tolerance must be read as a rollback")
	}
}

// A clock rollback while a debt is outstanding, with no run start since to
// rebase it, would otherwise keep every gather forcing a reconcile against a
// future floor for the debt's whole age-out window — and the stale notice
// would name a run time that has not happened yet. The gather rebases first.
func TestCodexReconcileForGather_RebasesDebtLeftInTheFutureByAClockRollback(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	future := now.Add(time.Hour)
	if !codexRecordRunFreshness(f.fp, future, func(snap *codexRateLimitSnapshot) {
		codexArmRunFloor(snap, future)
		codexOweRunRefresh(snap, future, future.Add(time.Minute))
	}) {
		t.Fatal("seeding the debt failed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), codexForcedReconcileBudget)
	defer cancel()
	codexReconcileForGather(ctx, codexHomeBase(), f.fp, now, false)

	if codexGateHasRun(f.fp) {
		t.Fatal("a routine gather must not force a reconcile onto a future floor")
	}
	snap := f.snapshot(t)
	if snap.RunFloorMs != 0 || snap.RefreshOwedAtMs != 0 || snap.RefreshOwedAttempts != 0 {
		t.Fatalf("future-dated debt must be dropped by the gather: %+v", snap)
	}
	if notice := codexStaleRunNotice(codexRunFreshnessForAccount(f.fp, now)); notice != "" {
		t.Fatalf("no stale notice may name a run that has not happened: %q", notice)
	}
}
