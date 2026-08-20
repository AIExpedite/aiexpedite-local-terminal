// cliagent_ratelimit_rollover_test.go
// -----------------------------------------------------------------------------
// A Claude window that rolls over must not freeze the cache.
//
// The incident: a Mac's claude_rate_limits.json held three buckets whose resets
// were all in the past, while `updatedAt` advanced on every status-line render.
// That combination is only possible one way — observations were arriving and
// being discarded — and the card read "Usage unobservable / Last observed
// 8/17" indefinitely, because every heartbeat was compared against the same
// expired prior and dropped.
//
// The rule these tests pin: a heartbeat carries no usage, so it may never
// publish a percentage — but it DID observe the window's reset, and recording
// that is what lets the cache move on.
// -----------------------------------------------------------------------------

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func rlPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "claude_rate_limits.json")
}

func readSnapshot(t *testing.T, path string) claudeRateLimitSnapshot {
	t.Helper()
	snap, ok := loadClaudeRateLimitSnapshot(path)
	if !ok {
		t.Fatalf("snapshot at %s is unreadable", path)
	}
	return snap
}

func heartbeat(resetsAtMs int64, status string) claudeRateLimitBucket {
	return claudeRateLimitBucket{ResetsAtMs: resetsAtMs, Status: status, usageKnown: false}
}

// reading mirrors what bucketFromInfo produces for a usage-bearing event: the
// percentage AND the moment it was observed.
func reading(used float64, resetsAtMs, observedAtMs int64) claudeRateLimitBucket {
	return claudeRateLimitBucket{
		UsedPercentage: used, ResetsAtMs: resetsAtMs, ObservedAtMs: observedAtMs, usageKnown: true,
	}
}

/* ───────────────────────────── the merge rule ───────────────────────────── */

// The regression: prior window rolled over, heartbeat advertises the NEW one.
func TestHeartbeatRecordsANewWindowAfterTheOldOneRolledOver(t *testing.T) {
	path := rlPath(t)
	now := time.Unix(1_800_000_000, 0)
	oldReset := now.Add(-48 * time.Hour).UnixMilli()
	newReset := now.Add(72 * time.Hour).UnixMilli()

	mergeClaudeRateLimitCache(path, map[string]claudeRateLimitBucket{
		"seven_day": reading(92, oldReset, now.Add(-72*time.Hour).UnixMilli()),
	}, now.Add(-72*time.Hour), "")

	mergeClaudeRateLimitCache(path, map[string]claudeRateLimitBucket{
		"seven_day": heartbeat(newReset, "allowed"),
	}, now, "")

	got := readSnapshot(t, path).Buckets["seven_day"]
	if got.ResetsAtMs != newReset {
		t.Fatalf("ResetsAtMs = %d, want the new window %d — the cache is still stuck on the rolled-over one",
			got.ResetsAtMs, newReset)
	}
	if got.hasObservedUsage() {
		t.Fatal("a heartbeat carries no usage reading; the new window must be marked unobserved")
	}
	if got.UsedPercentage != 0 {
		t.Fatalf("UsedPercentage = %v, want the stale reading dropped, not replayed under the new reset",
			got.UsedPercentage)
	}
}

// The behaviour the old guard was protecting, which must survive: a heartbeat
// must never republish a prior window's percentage under a NEW reset.
func TestHeartbeatNeverReplaysAStalePercentageUnderANewWindow(t *testing.T) {
	path := rlPath(t)
	now := time.Unix(1_800_000_000, 0)

	mergeClaudeRateLimitCache(path, map[string]claudeRateLimitBucket{
		"seven_day": reading(92, now.Add(-1*time.Hour).UnixMilli(), now.Add(-48*time.Hour).UnixMilli()),
	}, now.Add(-48*time.Hour), "")
	mergeClaudeRateLimitCache(path, map[string]claudeRateLimitBucket{
		"seven_day": heartbeat(now.Add(72*time.Hour).UnixMilli(), "allowed"),
	}, now, "")

	metrics := claudeCodeMetricsFromCacheAt(t, path, now)
	weekly := metricByLabel(t, metrics, "Weekly quota")
	if !weekly.Unknown {
		t.Fatalf("weekly metric = %+v, want Unknown — 92%% belonged to a window that has ended", weekly)
	}
	if weekly.Consumed != nil {
		t.Fatalf("weekly Consumed = %v, want nil", *weekly.Consumed)
	}
}

// Same live window: the prior reading is still valid and must be carried, which
// is what keeps a percentage on screen between real readings.
func TestHeartbeatCarriesTheReadingForwardWithinTheSameWindow(t *testing.T) {
	path := rlPath(t)
	now := time.Unix(1_800_000_000, 0)
	reset := now.Add(24 * time.Hour).UnixMilli()
	observedAt := now.Add(-2 * time.Hour)

	mergeClaudeRateLimitCache(path, map[string]claudeRateLimitBucket{
		"seven_day": reading(41, reset, observedAt.UnixMilli()),
	}, observedAt, "")
	mergeClaudeRateLimitCache(path, map[string]claudeRateLimitBucket{
		"seven_day": heartbeat(reset, "allowed"),
	}, now, "")

	got := readSnapshot(t, path).Buckets["seven_day"]
	if got.UsedPercentage != 41 {
		t.Fatalf("UsedPercentage = %v, want the live window's reading carried forward", got.UsedPercentage)
	}
	if !got.hasObservedUsage() {
		t.Fatal("a carried-forward reading is still an observed reading")
	}
	if got.ObservedAtMs != observedAt.UnixMilli() {
		t.Fatalf("ObservedAtMs = %d, want the reading's own timestamp %d — a heartbeat must not make stale usage look fresh",
			got.ObservedAtMs, observedAt.UnixMilli())
	}
}

// A heartbeat with no reset time and no usage says nothing the cache does not
// already hold, and must not seed a fake 0% row.
func TestHeartbeatWithoutAResetIsIgnored(t *testing.T) {
	path := rlPath(t)
	now := time.Unix(1_800_000_000, 0)

	mergeClaudeRateLimitCache(path, map[string]claudeRateLimitBucket{
		"five_hour": heartbeat(0, "allowed"),
	}, now, "")

	if _, ok := loadClaudeRateLimitSnapshot(path); ok {
		if snap := readSnapshot(t, path); len(snap.Buckets) != 0 {
			t.Fatalf("buckets = %+v, want none seeded from a contentless heartbeat", snap.Buckets)
		}
	}
}

// Repeated heartbeats on a rolled-over window converge instead of thrashing.
func TestRepeatedHeartbeatsConvergeOnTheCurrentWindow(t *testing.T) {
	path := rlPath(t)
	now := time.Unix(1_800_000_000, 0)
	reset := now.Add(48 * time.Hour).UnixMilli()

	mergeClaudeRateLimitCache(path, map[string]claudeRateLimitBucket{
		"five_hour": reading(88, now.Add(-96*time.Hour).UnixMilli(), now.Add(-100*time.Hour).UnixMilli()),
	}, now.Add(-100*time.Hour), "")
	for i := 0; i < 3; i++ {
		mergeClaudeRateLimitCache(path, map[string]claudeRateLimitBucket{
			"five_hour": heartbeat(reset, "allowed"),
		}, now.Add(time.Duration(i)*time.Minute), "")
	}

	got := readSnapshot(t, path).Buckets["five_hour"]
	if got.ResetsAtMs != reset || got.hasObservedUsage() {
		t.Fatalf("bucket = %+v, want the current window with usage unobserved", got)
	}
}

// A real reading after the window moved must land normally — this is the whole
// point of unsticking the cache.
func TestAReadingAfterTheWindowMovedIsRecorded(t *testing.T) {
	path := rlPath(t)
	now := time.Unix(1_800_000_000, 0)
	reset := now.Add(48 * time.Hour).UnixMilli()

	mergeClaudeRateLimitCache(path, map[string]claudeRateLimitBucket{
		"seven_day": reading(92, now.Add(-24*time.Hour).UnixMilli(), now.Add(-48*time.Hour).UnixMilli()),
	}, now.Add(-48*time.Hour), "")
	mergeClaudeRateLimitCache(path, map[string]claudeRateLimitBucket{
		"seven_day": heartbeat(reset, "allowed"),
	}, now, "")
	mergeClaudeRateLimitCache(path, map[string]claudeRateLimitBucket{
		"seven_day": reading(17, reset, now.Add(time.Minute).UnixMilli()),
	}, now.Add(time.Minute), "")

	metrics := claudeCodeMetricsFromCacheAt(t, path, now.Add(2*time.Minute))
	weekly := metricByLabel(t, metrics, "Weekly quota")
	if weekly.Unknown || weekly.Consumed == nil || *weekly.Consumed != 17 {
		t.Fatalf("weekly metric = %+v, want a real 17%% reading", weekly)
	}
}

/* ──────────────────────────── on-disk compatibility ─────────────────────── */

// Caches written before UsageObserved existed carried a reading in every
// bucket. An absent flag must keep meaning "observed", or every existing
// install's numbers would blank on upgrade.
func TestBucketsFromAnOlderCacheCountAsObserved(t *testing.T) {
	path := rlPath(t)
	legacy := `{"updatedAt":"2026-08-19T00:00:00Z","buckets":{"seven_day":{"usedPercentage":33,` +
		`"resetsAtMs":1800086400000,"status":"allowed","observedAtMs":1799990000000}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	snap := readSnapshot(t, path)
	if !snap.Buckets["seven_day"].hasObservedUsage() {
		t.Fatal("a bucket with no usageObserved field must be treated as observed")
	}
	metrics := claudeCodeMetricsFromCacheAt(t, path, time.UnixMilli(1_800_000_000_000))
	weekly := metricByLabel(t, metrics, "Weekly quota")
	if weekly.Unknown || weekly.Consumed == nil || *weekly.Consumed != 33 {
		t.Fatalf("weekly metric = %+v, want the legacy 33%% reading preserved", weekly)
	}
}

func TestUsageObservedRoundTripsThroughTheCacheFile(t *testing.T) {
	path := rlPath(t)
	now := time.Unix(1_800_000_000, 0)
	mergeClaudeRateLimitCache(path, map[string]claudeRateLimitBucket{
		"seven_day": heartbeat(now.Add(24*time.Hour).UnixMilli(), "allowed"),
	}, now, "")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk struct {
		Buckets map[string]struct {
			UsageObserved *bool `json:"usageObserved"`
		} `json:"buckets"`
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("cache is not JSON: %v", err)
	}
	flag := onDisk.Buckets["seven_day"].UsageObserved
	if flag == nil || *flag {
		t.Fatalf("usageObserved on disk = %v, want an explicit false — a reload would otherwise publish 0%%", flag)
	}
}

/* ───────────────────────────── what the card shows ──────────────────────── */

// The Mac's card, before and after: a current window with an honest reset beats
// a rolled-over one advertising a six-day-old observation.
func TestALiveUnobservedWindowReportsItsResetInsteadOfAStaleObservation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	reset := now.Add(36 * time.Hour)
	buckets := map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			ResetsAtMs:    reset.UnixMilli(),
			ObservedAtMs:  now.Add(-6 * 24 * time.Hour).UnixMilli(),
			UsageObserved: usageObservedPtr(false),
		},
	}

	metric := observedMetricOrUnknown(buckets, []string{claudeWindowFiveHour}, limitKindSession, "5-hour session window", now)
	if !metric.Unknown {
		t.Fatalf("metric = %+v, want Unknown", metric)
	}
	if metric.ResetAt != reset.UTC().Format(time.RFC3339) {
		t.Fatalf("ResetAt = %q, want the live window's reset %q", metric.ResetAt, reset.UTC().Format(time.RFC3339))
	}
	if metric.ObservedAt != "" {
		t.Fatalf("ObservedAt = %q, want empty — the usage was never observed", metric.ObservedAt)
	}
}

func TestWeeklyAggregateReportsALiveResetWhenNoSubWindowWasObserved(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	reset := now.Add(60 * time.Hour)
	buckets := map[string]claudeRateLimitBucket{
		claudeWindowSevenDay: {ResetsAtMs: reset.UnixMilli(), UsageObserved: usageObservedPtr(false)},
	}

	metric := aggregateWeeklyMetric(buckets, now)
	if !metric.Unknown {
		t.Fatalf("metric = %+v, want Unknown", metric)
	}
	if metric.ResetAt != reset.UTC().Format(time.RFC3339) {
		t.Fatalf("ResetAt = %q, want the live reset", metric.ResetAt)
	}
}

// An unobserved sub-window must not drag an observed one down to Unknown: the
// card's "≥ N%" is a lower bound, and the observed bucket is a real one.
func TestWeeklyAggregateStillReportsAnObservedSubWindow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	reset := now.Add(60 * time.Hour).UnixMilli()
	buckets := map[string]claudeRateLimitBucket{
		claudeWindowSevenDay:       {ResetsAtMs: reset, UsageObserved: usageObservedPtr(false)},
		claudeWindowSevenDayOpus:   {UsedPercentage: 64, ResetsAtMs: reset, ObservedAtMs: now.UnixMilli()},
		claudeWindowSevenDaySonnet: {UsedPercentage: 12, ResetsAtMs: reset, ObservedAtMs: now.UnixMilli()},
	}

	metric := aggregateWeeklyMetric(buckets, now)
	if metric.Unknown || metric.Consumed == nil || *metric.Consumed != 64 {
		t.Fatalf("metric = %+v, want the worst observed sub-window (64%%)", metric)
	}
}

/* ─────────────────────────────── helpers ────────────────────────────────── */

func claudeCodeMetricsFromCacheAt(t *testing.T, path string, now time.Time) []cliAgentUsageMetric {
	t.Helper()
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", path)
	return claudeCodeMetricsFromCache(now, "")
}

func metricByLabel(t *testing.T, metrics []cliAgentUsageMetric, label string) cliAgentUsageMetric {
	t.Helper()
	for _, m := range metrics {
		if m.Label == label {
			return m
		}
	}
	t.Fatalf("no metric labelled %q in %+v", label, metrics)
	return cliAgentUsageMetric{}
}
