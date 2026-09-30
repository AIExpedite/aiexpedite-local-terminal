package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_ratelimit_codex_limit_id_test.go — the contributor identity
   contract. Current Codex builds put `limit_id: "codex"` on the aggregate
   `rate_limits` of every token_count; older builds put nothing. A frame is
   stored under the limit it names, `__legacy__` and `codex` are one limit for
   supersession (newest wins), and a named model pool is never retracted by the
   aggregate.
   ------------------------------------------------------------------------ */

// codexWeeklyMetric returns the main pool's weekly row.
func codexWeeklyMetric(t *testing.T, metrics []cliAgentUsageMetric) cliAgentUsageMetric {
	t.Helper()
	for _, m := range metrics {
		if m.Kind == limitKindWeekly && m.Model == "" {
			return m
		}
	}
	t.Fatalf("no weekly metric in %+v", metrics)
	return cliAgentUsageMetric{}
}

// codexCurrentBuildTokenCount is the token_count line codex-cli 0.155 writes:
// the weekly window under `primary`, `secondary: null`, and the non-window keys
// the extractor must ignore.
func codexCurrentBuildTokenCount(t *testing.T, limitID any, weeklyPct float64, resetAt, stamp time.Time) string {
	t.Helper()
	rl := map[string]any{
		"primary":    map[string]any{"used_percent": weeklyPct, "window_minutes": 10080.0, "resets_at": float64(resetAt.Unix())},
		"secondary":  nil,
		"credits":    map[string]any{"has_credits": true, "unlimited": false, "balance": "123.45"},
		"plan_type":  "plus",
		"limit_name": nil,
	}
	if limitID != nil {
		rl["limit_id"] = limitID
	}
	line, err := json.Marshal(map[string]any{
		"timestamp": stamp.UTC().Format(time.RFC3339Nano),
		"type":      "event_msg",
		"payload":   map[string]any{"type": "token_count", "rate_limits": rl},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(line)
}

// seedCodexContributors writes contributors (and limit names) for f's account
// directly, the way a cache left by earlier builds looks on disk.
func seedCodexContributors(t *testing.T, f codexFreshnessFixture, contributors map[string]map[string]codexRateLimitBucket, names map[string]string, extra func(*codexRateLimitSnapshot)) {
	t.Helper()
	if !codexRateLimitCacheTransaction(context.Background(), f.cache, time.Now(), true, func(snap *codexRateLimitSnapshot) bool {
		snap.AccountFingerprint = f.fp
		snap.Contributors = contributors
		snap.LimitNames = names
		snap.Buckets = aggregateCodexBuckets(contributors, time.Now())
		if extra != nil {
			extra(snap)
		}
		return true
	}) {
		t.Fatal("seeding the cache failed")
	}
}

func weeklyBucket(pct float64, resetAt, observedAt time.Time) codexRateLimitBucket {
	return codexRateLimitBucket{
		UsedPercentage: pct, ResetsAtMs: resetAt.UnixMilli(), ObservedAtMs: observedAt.UnixMilli(), WindowMinutes: 10080,
	}
}

func TestCodexLimitID_AggregateIsKeyedUnderItsLimitID(t *testing.T) {
	now := time.Now()
	var raw map[string]any
	if err := json.Unmarshal([]byte(codexCurrentBuildTokenCount(t, "codex", 27, now.Add(96*time.Hour), now)), &raw); err != nil {
		t.Fatal(err)
	}
	out, clears := extractCodexRateLimitBuckets(raw, now)
	if len(clears) != 0 {
		t.Fatalf("a sparse token_count must not clear anything: %v", clears)
	}
	if len(out) != 1 || len(out[codexWindowPrimary]) != 1 {
		t.Fatalf("want exactly one window, one contributor; got %+v", out)
	}
	b, ok := out[codexWindowPrimary][codexDefaultLimitID]
	if !ok {
		t.Fatalf("contributor not keyed under limit_id: %+v", out)
	}
	if b.UsedPercentage != 27 || b.WindowMinutes != 10080 {
		t.Fatalf("bucket = %+v", b)
	}
}

func TestCodexLimitID_MissingEmptyOrNonStringFallsBackToLegacy(t *testing.T) {
	now := time.Now()
	for name, id := range map[string]any{
		"missing":    nil,
		"empty":      "",
		"blank":      "   ",
		"number":     float64(7),
		"object":     map[string]any{"id": "codex"},
		"null value": "__NULL__",
	} {
		t.Run(name, func(t *testing.T) {
			line := codexCurrentBuildTokenCount(t, id, 27, now.Add(96*time.Hour), now)
			if id == "__NULL__" {
				line = strings.Replace(line, `"limit_id":"__NULL__"`, `"limit_id":null`, 1)
			}
			var raw map[string]any
			if err := json.Unmarshal([]byte(line), &raw); err != nil {
				t.Fatal(err)
			}
			out, _ := extractCodexRateLimitBuckets(raw, now)
			if _, ok := out[codexWindowPrimary][codexLegacyLimitID]; !ok || len(out[codexWindowPrimary]) != 1 {
				t.Fatalf("want a single __legacy__ contributor, got %+v", out)
			}
		})
	}
}

func TestCodexLimitID_OverLongIDIsClamped(t *testing.T) {
	now := time.Now()
	long := strings.Repeat("x", 300)
	var raw map[string]any
	_ = json.Unmarshal([]byte(codexCurrentBuildTokenCount(t, long, 27, now.Add(96*time.Hour), now)), &raw)
	out, _ := extractCodexRateLimitBuckets(raw, now)
	for id := range out[codexWindowPrimary] {
		if len(id) > codexLimitNameMaxBytes {
			t.Fatalf("limit id not clamped: %d bytes", len(id))
		}
		if !strings.HasPrefix(long, id) {
			t.Fatalf("clamped id %q is not a prefix of the reported one", id)
		}
		return
	}
	t.Fatalf("no contributor extracted: %+v", out)
}

// The reported Windows cache: an older `codex` weekly reading (90%, reset R1)
// and a newer `__legacy__` one (27%, later reset R2). The fresh reading used to
// lose the max fold while the stale one borrowed its reset, so the published
// observation predated the window it claimed and the card showed nothing.
func TestCodexLimitID_NewerLegacySupersedesOlderCodexOnDisplayAndDisk(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-72*time.Hour))
	r1, r2 := now.Add(72*time.Hour), now.Add(6*24*time.Hour)
	older, newer := now.Add(-18*time.Hour), now.Add(-time.Hour)
	seedCodexContributors(t, f, map[string]map[string]codexRateLimitBucket{
		codexWindowSecondary: {
			codexDefaultLimitID: weeklyBucket(90, r1, older),
			codexLegacyLimitID:  weeklyBucket(27, r2, newer),
		},
	}, nil, func(s *codexRateLimitSnapshot) { s.FullSnapshotAtMs = older.UnixMilli() })

	weekly := codexWeeklyMetric(t, codexMetricsFromCache(now, f.fp))
	if weekly.Unknown || weekly.Consumed == nil || *weekly.Consumed != 27 {
		t.Fatalf("weekly = %+v, want the newer 27%% reading", weekly)
	}
	if got := metricObservedAt(t, weekly); got.Unix() != newer.Unix() {
		t.Fatalf("observedAt = %s, want the newer reading's %s", got, newer)
	}
	if weekly.ResetAt != time.UnixMilli(r2.UnixMilli()).UTC().Format(time.RFC3339) {
		t.Fatalf("resetAt = %s, want the newer reading's own reset", weekly.ResetAt)
	}

	// The next write prunes the dead duplicate: display and disk agree.
	f.seedPreRunReading(t, now.Add(-2*time.Hour), now) // an unrelated sparse write
	snap := f.snapshot(t)
	for slot, contribs := range snap.Contributors {
		if _, ok := contribs[codexDefaultLimitID]; ok && codexWindowIdentity(contribs[codexDefaultLimitID].WindowMinutes, slot) == codexIdentityWeekly {
			t.Fatalf("the superseded codex weekly reading survived on disk: %+v", snap.Contributors)
		}
	}
}

func TestCodexLimitID_NewerCodexSupersedesOlderLegacy(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-72*time.Hour))
	seedCodexContributors(t, f, map[string]map[string]codexRateLimitBucket{
		codexWindowSecondary: {
			codexLegacyLimitID: weeklyBucket(90, now.Add(96*time.Hour), now.Add(-5*time.Hour)),
		},
	}, nil, nil)

	if !captureCodexRateLimitLineForAccount(codexCurrentBuildTokenCount(t, "codex", 12, now.Add(96*time.Hour), now.Add(-time.Minute)), now, f.fp) {
		t.Fatal("the smoke-shaped frame did not land")
	}
	weekly := codexWeeklyMetric(t, codexMetricsFromCache(now, f.fp))
	if weekly.Consumed == nil || *weekly.Consumed != 12 {
		t.Fatalf("weekly = %+v, want the newer codex 12%%, not the stale legacy 90%%", weekly)
	}
	for _, contribs := range f.snapshot(t).Contributors {
		if _, ok := contribs[codexLegacyLimitID]; ok {
			t.Fatalf("the superseded legacy reading survived on disk: %+v", contribs)
		}
	}
}

// A named model pool is its own limit: an aggregate `codex` reading never
// retracts it, however much newer.
func TestCodexLimitID_NamedPoolIsNeverRetractedByTheAggregate(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-72*time.Hour))
	seedCodexContributors(t, f, map[string]map[string]codexRateLimitBucket{
		codexWindowSecondary: {"codex_bengalfox": weeklyBucket(55, now.Add(96*time.Hour), now.Add(-5*time.Hour))},
	}, map[string]string{"codex_bengalfox": "GPT-5.3-Codex-Spark"}, nil)

	captureCodexRateLimitLineForAccount(codexCurrentBuildTokenCount(t, "codex", 12, now.Add(96*time.Hour), now), now, f.fp)

	metrics := codexMetricsFromCache(now, f.fp)
	var pool *cliAgentUsageMetric
	for i := range metrics {
		if metrics[i].Model == "gpt-5.3-codex-spark" && metrics[i].Kind == limitKindWeekly {
			pool = &metrics[i]
		}
	}
	if pool == nil || pool.Consumed == nil || *pool.Consumed != 55 {
		t.Fatalf("the named pool's weekly row was retracted: %+v", metrics)
	}
	if weekly := codexWeeklyMetric(t, metrics); weekly.Consumed == nil || *weekly.Consumed != 12 {
		t.Fatalf("main weekly = %+v", weekly)
	}
}

// A live read carrying both views writes ONE `codex` contributor.
func TestCodexLimitID_LiveReadWithBothViewsWritesOneContributor(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-72*time.Hour))
	window := func(pct, mins int64, reset time.Time) string {
		return `{"usedPercent":` + itoa(pct) + `,"windowDurationMins":` + itoa(mins) + `,"resetsAt":` + itoa(reset.Unix()) + `}`
	}
	envelope := `{"jsonrpc":"2.0","id":2,"result":{` +
		`"rateLimits":{"limitId":"codex","limitName":null,"primary":` + window(30, 300, now.Add(2*time.Hour)) + `,"secondary":` + window(40, 10080, now.Add(70*time.Hour)) + `},` +
		`"rateLimitsByLimitId":{"codex":{"limitId":"codex","limitName":null,"primary":` + window(30, 300, now.Add(2*time.Hour)) + `,"secondary":` + window(40, 10080, now.Add(70*time.Hour)) + `}}}}`
	if !captureCodexRateLimitLineForAccount(envelope, now, f.fp) {
		t.Fatal("live read did not land")
	}
	snap := f.snapshot(t)
	for slot, contribs := range snap.Contributors {
		if len(contribs) != 1 {
			t.Fatalf("slot %s holds %d contributors, want one: %+v", slot, len(contribs), snap.Contributors)
		}
		if _, ok := contribs[codexDefaultLimitID]; !ok {
			t.Fatalf("slot %s is not keyed codex: %+v", slot, contribs)
		}
	}
	if len(snap.LimitNames) != 0 {
		t.Fatalf("the default limit must stay unnamed (main pool): %+v", snap.LimitNames)
	}
}

// Sparse `secondary: null` on a token_count never clears a window.
func TestCodexLimitID_SparseNullSecondaryNeverClears(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-72*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now) // session + weekly under __legacy__
	line, _ := json.Marshal(map[string]any{
		"type": "event_msg",
		"payload": map[string]any{"type": "token_count", "rate_limits": map[string]any{
			"limit_id":  "codex",
			"primary":   map[string]any{"used_percent": 15.0, "window_minutes": 300.0, "resets_at": float64(now.Add(2 * time.Hour).Unix())},
			"secondary": nil,
		}},
	})
	captureCodexRateLimitLineForAccount(string(line), now, f.fp)
	weekly := codexWeeklyMetric(t, codexMetricsFromCache(now, f.fp))
	if weekly.Unknown || weekly.Consumed == nil || *weekly.Consumed != 40 {
		t.Fatalf("a sparse null secondary cleared the weekly window: %+v", weekly)
	}
}

func TestCodexLimitID_AggregateLimitNameIsRecordedUnderItsID(t *testing.T) {
	raw := map[string]any{"type": "token_count", "rate_limits": map[string]any{
		"limit_id": "codex_bengalfox", "limit_name": "GPT-5.3-Codex-Spark",
		"primary": map[string]any{"used_percent": 5.0, "window_minutes": 10080.0},
	}}
	if got := extractCodexLimitNames(raw); got["codex_bengalfox"] != "GPT-5.3-Codex-Spark" || len(got) != 1 {
		t.Fatalf("names = %+v", got)
	}
	// An id-less aggregate has no limit to name.
	delete(raw["rate_limits"].(map[string]any), "limit_id")
	if got := extractCodexLimitNames(raw); len(got) != 0 {
		t.Fatalf("an id-less aggregate named a limit: %+v", got)
	}
}
