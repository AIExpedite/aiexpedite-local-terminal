package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCaptureCodexRateLimit_TokenCountNotification(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	primaryResetSec := 3600.0   // 1h
	secondaryResetSec := 604800 // 7d
	line := `{"jsonrpc":"2.0","method":"token_count","params":{"msg":{"rate_limits":{` +
		`"primary":{"used_percent":42.5,"resets_in_seconds":3600},` +
		`"secondary":{"utilization":0.18,"resets_in_seconds":604800}` +
		`}}}}`

	captureCodexRateLimitLine(line, now)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache to be written")
	}
	p, ok := snap.Buckets[codexWindowPrimary]
	if !ok {
		t.Fatalf("expected primary bucket, got %+v", snap.Buckets)
	}
	if p.UsedPercentage != 42.5 {
		t.Errorf("primary UsedPercentage=%v, want 42.5", p.UsedPercentage)
	}
	wantPrimaryReset := now.Add(time.Duration(primaryResetSec * float64(time.Second))).UnixMilli()
	if p.ResetsAtMs != wantPrimaryReset {
		t.Errorf("primary ResetsAtMs=%d, want %d", p.ResetsAtMs, wantPrimaryReset)
	}

	s := snap.Buckets[codexWindowSecondary]
	if s.UsedPercentage < 17.9 || s.UsedPercentage > 18.1 {
		t.Errorf("secondary UsedPercentage=%v, want ~18 (utilization 0.18 -> %%)", s.UsedPercentage)
	}
	wantSecondaryReset := now.Add(time.Duration(secondaryResetSec) * time.Second).UnixMilli()
	if s.ResetsAtMs != wantSecondaryReset {
		t.Errorf("secondary ResetsAtMs=%d, want %d", s.ResetsAtMs, wantSecondaryReset)
	}
}

// Codex's exact wire keys are tolerated, not assumed: the design accepts
// `5h`/`7d` window aliases and a `window_minutes` reset hint so a schema
// rename doesn't silently zero the card. Cover both at once.
func TestCaptureCodexRateLimit_AcceptsAliasesAndWindowMinutes(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	// Pair window_minutes with a real resets_in_seconds — window_minutes is the
	// window LENGTH, not a time-until-reset, so the bucket must rely on the
	// explicit reset field for ResetsAtMs and keep window_minutes only for
	// labelling.
	line := `{"method":"token_count","params":{"rate_limits":{` +
		`"5h":{"used_percent":12,"window_minutes":300,"resets_in_seconds":1800},` +
		`"7d":{"used_percent":50,"window_minutes":10080,"resets_in_seconds":86400}` +
		`}}}`

	captureCodexRateLimitLine(line, now)
	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache write under alias keys")
	}
	p, ok := snap.Buckets[codexWindowPrimary]
	if !ok || p.UsedPercentage != 12 {
		t.Fatalf("5h alias did not normalise to primary bucket: %+v", snap.Buckets)
	}
	wantPrimaryReset := now.Add(1800 * time.Second).UnixMilli()
	if p.ResetsAtMs != wantPrimaryReset {
		t.Errorf("primary ResetsAtMs from resets_in_seconds=%d, want %d", p.ResetsAtMs, wantPrimaryReset)
	}
	if p.WindowMinutes != 300 {
		t.Errorf("primary WindowMinutes=%v, want 300 (kept for labelling)", p.WindowMinutes)
	}
	s, ok := snap.Buckets[codexWindowSecondary]
	if !ok || s.UsedPercentage != 50 {
		t.Fatalf("7d alias did not normalise to secondary bucket: %+v", snap.Buckets)
	}
}

// window_minutes is the documented rolling-window LENGTH, not a time-until-reset
// hint. When a Codex frame includes used_percent + window_minutes but omits a
// real resetsAt/resets_in_seconds, the bucket must keep window_minutes for
// labelling and leave ResetsAtMs unknown — otherwise a sparse update with no
// real reset field would overwrite a previously correct reset with one
// hours/days too late, and a brand-new window would render with a fabricated
// reset that has nothing to do with when the quota actually rolls over.
func TestCaptureCodexRateLimit_WindowMinutesAloneLeavesResetUnknown(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	// 1. Seed a live primary window with a real reset 1h out, then send a
	//    follow-up with used_percent + window_minutes but no real reset. The
	//    cached reset must be preserved; window_minutes must NOT overwrite it
	//    with `now + 300m` (4 hours too late).
	captureCodexRateLimitLine(
		`{"method":"token_count","params":{"rate_limits":{"primary":{"used_percent":40,"resets_in_seconds":3600}}}}`,
		now,
	)
	captureCodexRateLimitLine(
		`{"method":"account/rateLimits/updated","params":{"rateLimits":{"primary":{"usedPercent":55,"windowDurationMins":300}}}}`,
		now,
	)
	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache")
	}
	p := snap.Buckets[codexWindowPrimary]
	if got, want := p.ResetsAtMs, now.Add(time.Hour).UnixMilli(); got != want {
		t.Errorf("after windowDurationMins-only update ResetsAtMs=%d, want preserved %d", got, want)
	}
	if p.UsedPercentage != 55 {
		t.Errorf("after windowDurationMins-only update UsedPercentage=%v, want 55", p.UsedPercentage)
	}
	if p.WindowMinutes != 300 {
		t.Errorf("WindowMinutes=%v, want 300 retained for labelling", p.WindowMinutes)
	}

	// 2. A first-ever observation with used_percent + window_minutes but no
	//    real reset must persist usage and label hint, with ResetsAtMs = 0
	//    (unknown). Otherwise the card would advertise a stale ResetAt that
	//    has no anchor in reality.
	cache2 := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache2)
	captureCodexRateLimitLine(
		`{"method":"token_count","params":{"rate_limits":{"primary":{"used_percent":33,"window_minutes":15}}}}`,
		now,
	)
	snap2, ok := loadCodexRateLimitSnapshot(cache2)
	if !ok {
		t.Fatalf("expected cache (first observation)")
	}
	p2, ok := snap2.Buckets[codexWindowPrimary]
	if !ok {
		t.Fatalf("primary not persisted on first observation: %+v", snap2.Buckets)
	}
	if p2.UsedPercentage != 33 {
		t.Errorf("first-obs UsedPercentage=%v, want 33", p2.UsedPercentage)
	}
	if p2.WindowMinutes != 15 {
		t.Errorf("first-obs WindowMinutes=%v, want 15", p2.WindowMinutes)
	}
	if p2.ResetsAtMs != 0 {
		t.Errorf("first-obs ResetsAtMs=%d, want 0 (no real reset field present)", p2.ResetsAtMs)
	}
}

// `account/rateLimits/read` is a full snapshot: when Codex returns
// `secondary: null`, the account has no weekly window and any previously
// cached bucket must be cleared rather than left to render stale numbers
// until its old reset passes. Sparse `account/rateLimits/updated` /
// `token_count` notifications are NOT full snapshots, so a missing or null
// window there must NOT clear the cache.
func TestCaptureCodexRateLimit_FullReadNullClearsWindow(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	// Seed a live secondary (weekly) bucket.
	captureCodexRateLimitLine(
		`{"method":"token_count","params":{"rate_limits":{"secondary":{"used_percent":40,"resets_in_seconds":604800}}}}`,
		now,
	)
	if snap, ok := loadCodexRateLimitSnapshot(cache); !ok || snap.Buckets[codexWindowSecondary].UsedPercentage != 40 {
		t.Fatalf("seed failed: %+v", snap.Buckets)
	}

	// Full account/rateLimits/read response with secondary: null — clear it.
	captureCodexRateLimitLine(
		`{"jsonrpc":"2.0","id":3,"result":{"rateLimits":{"primary":{"usedPercent":10,"resetsInSeconds":1800},"secondary":null}}}`,
		now,
	)
	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache after full read")
	}
	if _, present := snap.Buckets[codexWindowSecondary]; present {
		t.Errorf("secondary bucket must be cleared by explicit null in full read response: %+v", snap.Buckets)
	}
	if p, ok := snap.Buckets[codexWindowPrimary]; !ok || p.UsedPercentage != 10 {
		t.Errorf("primary update inside the same full read should still apply: %+v", snap.Buckets)
	}
}

// A null window inside a sparse account/rateLimits/updated notification is
// "no update for this window," not "clear the window" — clearing is only the
// semantics of a full account/rateLimits/read response. A previously cached
// live bucket must therefore survive.
func TestCaptureCodexRateLimit_SparseUpdateNullDoesNotClear(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	captureCodexRateLimitLine(
		`{"method":"token_count","params":{"rate_limits":{"secondary":{"used_percent":40,"resets_in_seconds":604800}}}}`,
		now,
	)
	captureCodexRateLimitLine(
		`{"method":"account/rateLimits/updated","params":{"rateLimits":{"secondary":null,"primary":{"usedPercent":12,"resetsInSeconds":1800}}}}`,
		now,
	)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache")
	}
	s, present := snap.Buckets[codexWindowSecondary]
	if !present || s.UsedPercentage != 40 {
		t.Errorf("sparse-update null must preserve prior secondary bucket: %+v", snap.Buckets)
	}
	if p, ok := snap.Buckets[codexWindowPrimary]; !ok || p.UsedPercentage != 12 {
		t.Errorf("primary update should still apply: %+v", snap.Buckets)
	}
}

// Codex's newer surface is `account/rateLimits/read` (response under
// `result`) and `account/rateLimits/updated` (notification under `params`),
// both with camelCase `rateLimits`. The prefilter must accept that token and
// extraction must drain both `result` and `params`.
func TestCaptureCodexRateLimit_AccountRateLimitsReadResult(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	line := `{"jsonrpc":"2.0","id":7,"result":{"rateLimits":{` +
		`"primary":{"usedPercent":22,"resetsInSeconds":1800}` +
		`}}}`
	captureCodexRateLimitLine(line, now)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache write for account/rateLimits/read result")
	}
	p, ok := snap.Buckets[codexWindowPrimary]
	if !ok || p.UsedPercentage != 22 {
		t.Fatalf("primary not extracted from result.rateLimits: %+v", snap.Buckets)
	}
	if p.ResetsAtMs != now.Add(1800*time.Second).UnixMilli() {
		t.Errorf("ResetsAtMs=%d, want resetsInSeconds offset", p.ResetsAtMs)
	}
}

// account/rateLimits/updated notifications are documented as sparse. A
// reset-only follow-up that restates the SAME live reset must preserve the
// prior used_percent, and a usage-only follow-up must NOT drop the prior
// reset time — otherwise the card oscillates between real numbers and
// 0%/Unknown between turns. (Reset-only updates that ADVANCE the reset to a
// new window are covered separately by
// TestCaptureCodexRateLimit_ResetOnlyAdvanceDropsPriorUsage.)
func TestCaptureCodexRateLimit_SparseUpdatesPreservePriorFields(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	// Initial full snapshot: 60% used, resets in 1h.
	captureCodexRateLimitLine(
		`{"method":"token_count","params":{"rate_limits":{"primary":{"used_percent":60,"resets_in_seconds":3600}}}}`,
		now,
	)

	// Sparse update #1: reset-only restating the same live reset. Must
	// preserve 60% (same window, just a fresh notification).
	captureCodexRateLimitLine(
		`{"method":"account/rateLimits/updated","params":{"rateLimits":{"primary":{"resetsInSeconds":3600}}}}`,
		now,
	)
	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache")
	}
	if got := snap.Buckets[codexWindowPrimary].UsedPercentage; got != 60 {
		t.Errorf("after reset-only update UsedPercentage=%v, want 60 (preserved)", got)
	}
	if got := snap.Buckets[codexWindowPrimary].ResetsAtMs; got != now.Add(3600*time.Second).UnixMilli() {
		t.Errorf("after reset-only update ResetsAtMs=%d, want live reset preserved", got)
	}

	// Sparse update #2: usage-only (no reset). Must preserve the live reset.
	captureCodexRateLimitLine(
		`{"method":"account/rateLimits/updated","params":{"rateLimits":{"primary":{"usedPercent":72}}}}`,
		now,
	)
	snap, _ = loadCodexRateLimitSnapshot(cache)
	if got := snap.Buckets[codexWindowPrimary].UsedPercentage; got != 72 {
		t.Errorf("after usage-only update UsedPercentage=%v, want 72", got)
	}
	if got := snap.Buckets[codexWindowPrimary].ResetsAtMs; got != now.Add(3600*time.Second).UnixMilli() {
		t.Errorf("after usage-only update ResetsAtMs=%d, want previous live reset preserved", got)
	}
}

// A reset-only update with no prior live bucket must NOT persist a zero-value
// UsedPercentage, otherwise the card would render an observed 0% used / 100%
// remaining even though no usage was ever reported. The metric should stay
// Unknown until a real used_percent/utilization is observed.
func TestCaptureCodexRateLimit_ResetOnlyWithoutPriorStaysUnknown(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	captureCodexRateLimitLine(
		`{"method":"account/rateLimits/updated","params":{"rateLimits":{"primary":{"resetsInSeconds":1500}}}}`,
		now,
	)

	if snap, ok := loadCodexRateLimitSnapshot(cache); ok {
		if _, present := snap.Buckets[codexWindowPrimary]; present {
			t.Fatalf("reset-only update with no prior bucket should not seed a 0%% bucket: %+v", snap.Buckets)
		}
	}

	metrics := codexMetricsFromCache(now, "")
	if !metrics[0].Unknown {
		t.Errorf("session metric should remain Unknown after reset-only update with no prior usage, got %+v", metrics[0])
	}
}

// A sparse reset-only update that ADVANCES resetsAt past the cached reset
// describes a fresh quota window (the prior 5h/weekly bucket has rolled over).
// The previous bucket's UsedPercentage must NOT be copied onto the new window —
// otherwise the card keeps showing the old high usage on what is actually an
// empty new window until a real used_percent arrives.
func TestCaptureCodexRateLimit_ResetOnlyAdvanceDropsPriorUsage(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	// Seed a live bucket at 80% with a reset 30 minutes out.
	captureCodexRateLimitLine(
		`{"method":"token_count","params":{"rate_limits":{"primary":{"used_percent":80,"resets_in_seconds":1800}}}}`,
		now,
	)

	// Sparse reset-only update advancing resetsAt by another 5 hours — this is
	// a new window, not a refreshed reset for the same one.
	captureCodexRateLimitLine(
		`{"method":"account/rateLimits/updated","params":{"rateLimits":{"primary":{"resetsInSeconds":19800}}}}`,
		now,
	)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache")
	}
	if got, present := snap.Buckets[codexWindowPrimary]; present && got.UsedPercentage == 80 {
		t.Fatalf("reset-only advance must NOT carry 80%% used onto the new window: %+v", got)
	}

	metrics := codexMetricsFromCache(now, "")
	if !metrics[0].Unknown {
		t.Errorf("session metric should be Unknown after reset-only advance to a new window, got %+v", metrics[0])
	}
}

// Codex's app-server also emits token_count events as JSONL/event envelopes
// outside of a JSON-RPC params/result frame: `{"id":"…","msg":{"type":
// "token_count", … "rate_limits": …}}` and a session-event variant
// `{"payload":{…}}`. The extractor must descend into TOP-LEVEL msg/payload —
// not just params/result and their nested .msg — otherwise the prefilter
// passes, no buckets are emitted, and the card stays Unknown despite live
// telemetry being present.
func TestCaptureCodexRateLimit_TopLevelMsgEnvelope(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	line := `{"id":"0","msg":{"type":"token_count","rate_limits":{` +
		`"primary":{"used_percent":61,"resets_in_seconds":1800},` +
		`"secondary":{"utilization":0.42,"resets_in_seconds":259200}` +
		`}}}`

	captureCodexRateLimitLine(line, now)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache to be written from top-level msg envelope")
	}
	p, ok := snap.Buckets[codexWindowPrimary]
	if !ok {
		t.Fatalf("expected primary bucket from top-level msg envelope, got %+v", snap.Buckets)
	}
	if p.UsedPercentage != 61 {
		t.Errorf("primary UsedPercentage=%v, want 61", p.UsedPercentage)
	}
	if want := now.Add(30 * time.Minute).UnixMilli(); p.ResetsAtMs != want {
		t.Errorf("primary ResetsAtMs=%d, want %d", p.ResetsAtMs, want)
	}
	s, ok := snap.Buckets[codexWindowSecondary]
	if !ok {
		t.Fatalf("expected secondary bucket from top-level msg envelope")
	}
	if s.UsedPercentage < 41.9 || s.UsedPercentage > 42.1 {
		t.Errorf("secondary UsedPercentage=%v, want ~42 (utilization 0.42)", s.UsedPercentage)
	}
}

func TestCaptureCodexRateLimit_TopLevelPayloadEnvelope(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	line := `{"payload":{"type":"token_count","rate_limits":{` +
		`"primary":{"used_percent":25,"resets_in_seconds":900}` +
		`}}}`

	captureCodexRateLimitLine(line, now)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache to be written from top-level payload envelope")
	}
	p, ok := snap.Buckets[codexWindowPrimary]
	if !ok {
		t.Fatalf("expected primary bucket from top-level payload envelope, got %+v", snap.Buckets)
	}
	if p.UsedPercentage != 25 {
		t.Errorf("primary UsedPercentage=%v, want 25", p.UsedPercentage)
	}
}

// Top-level msg/payload envelopes are notifications, NOT full
// account/rateLimits/read snapshots — a null window there means "no update,"
// not "clear it." Otherwise a stray top-level event could silently wipe a
// freshly cached weekly bucket.
func TestCaptureCodexRateLimit_TopLevelMsgNullDoesNotClear(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	// Seed both windows via a normal params-side notification.
	captureCodexRateLimitLine(
		`{"jsonrpc":"2.0","method":"token_count","params":{"msg":{"rate_limits":{`+
			`"primary":{"used_percent":40,"resets_in_seconds":3600},`+
			`"secondary":{"used_percent":10,"resets_in_seconds":604800}}}}}`,
		now)

	// A subsequent top-level-msg event reports primary only and SETS secondary
	// null. Since this is not a full snapshot, secondary must be preserved.
	captureCodexRateLimitLine(
		`{"id":"1","msg":{"type":"token_count","rate_limits":{`+
			`"primary":{"used_percent":55,"resets_in_seconds":3600},`+
			`"secondary":null}}}`,
		now.Add(time.Minute))

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache")
	}
	if p := snap.Buckets[codexWindowPrimary]; p.UsedPercentage != 55 {
		t.Errorf("primary UsedPercentage=%v, want 55 (refreshed)", p.UsedPercentage)
	}
	if _, present := snap.Buckets[codexWindowSecondary]; !present {
		t.Errorf("secondary window must NOT be cleared by a null in a top-level msg envelope (notification, not full snapshot)")
	}
}

// account/rateLimits/read can return the documented multi-bucket shape under
// `rateLimitsByLimitId` (keyed by metered limit id, e.g. `codex_primary`,
// `codex_secondary`, `codex_other`). When a `codex_other` bucket reports a
// HIGHER utilisation than the aggregate `rateLimits` view, the cache must keep
// the more-constrained number — otherwise the UI silently understates usage.
func TestCaptureCodexRateLimit_MultiBucketKeepsMostConstrained(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	// Legacy aggregate says 5h=20%; the multi-bucket view shows codex_other on
	// the same 5h window at 80%. The merged primary must be 80%.
	line := `{"jsonrpc":"2.0","result":{"rateLimits":{` +
		`"primary":{"used_percent":20,"resets_in_seconds":1800,"window_minutes":300},` +
		`"secondary":{"used_percent":15,"resets_in_seconds":86400,"window_minutes":10080}` +
		`},"rateLimitsByLimitId":{` +
		`"codex_primary":{"used_percent":20,"window_minutes":300,"resets_in_seconds":1800},` +
		`"codex_other":{"used_percent":80,"window_minutes":300,"resets_in_seconds":1800},` +
		`"codex_secondary":{"used_percent":15,"window_minutes":10080,"resets_in_seconds":86400}` +
		`}},"id":1}`

	captureCodexRateLimitLine(line, now)
	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache write")
	}
	p, ok := snap.Buckets[codexWindowPrimary]
	if !ok {
		t.Fatalf("expected primary bucket, got %+v", snap.Buckets)
	}
	if p.UsedPercentage != 80 {
		t.Errorf("primary UsedPercentage=%v, want 80 (most constrained multi-bucket entry)", p.UsedPercentage)
	}
	s, ok := snap.Buckets[codexWindowSecondary]
	if !ok || s.UsedPercentage != 15 {
		t.Errorf("secondary=%+v, want UsedPercentage=15", s)
	}
}

// When `rateLimits` is absent and only `rateLimitsByLimitId` is present, the
// extractor must still populate both display windows — classifying entries
// without an explicit key hint by window length (<=6h → primary, otherwise
// secondary).
func TestCaptureCodexRateLimit_MultiBucketOnlyClassifiesByLength(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	line := `{"jsonrpc":"2.0","result":{"rateLimitsByLimitId":{` +
		`"some_5h_bucket":{"used_percent":33,"window_minutes":300,"resets_in_seconds":1800},` +
		`"some_weekly_bucket":{"used_percent":44,"window_minutes":10080,"resets_in_seconds":86400}` +
		`}},"id":1}`

	captureCodexRateLimitLine(line, now)
	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache write from rateLimitsByLimitId alone")
	}
	if snap.Buckets[codexWindowPrimary].UsedPercentage != 33 {
		t.Errorf("primary=%+v, want UsedPercentage=33", snap.Buckets[codexWindowPrimary])
	}
	if snap.Buckets[codexWindowSecondary].UsedPercentage != 44 {
		t.Errorf("secondary=%+v, want UsedPercentage=44", snap.Buckets[codexWindowSecondary])
	}
}

// The documented `rateLimitsByLimitId` shape nests window buckets under
// `primary`/`secondary` keys (e.g. `codex_other.primary.usedPercent`). The
// extractor must descend into those nested keys — not treat the outer entry
// as a flat bucket — so a stricter `codex_other.primary` actually constrains
// the primary display window.
func TestCaptureCodexRateLimit_MultiBucketNestedPrimarySecondary(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	// Aggregate view says primary=20%; the nested `codex_other.primary`
	// reports 77% on the same 5h window. Merge must keep 77%.
	line := `{"jsonrpc":"2.0","result":{"rateLimits":{` +
		`"primary":{"usedPercent":20,"resetsInSeconds":1800,"windowDurationMins":300},` +
		`"secondary":{"usedPercent":10,"resetsInSeconds":86400,"windowDurationMins":10080}` +
		`},"rateLimitsByLimitId":{` +
		`"codex_primary":{"primary":{"usedPercent":20,"resetsInSeconds":1800,"windowDurationMins":300}},` +
		`"codex_other":{"primary":{"usedPercent":77,"resetsInSeconds":1800,"windowDurationMins":300},` +
		`"secondary":{"usedPercent":55,"resetsInSeconds":86400,"windowDurationMins":10080}}` +
		`}},"id":1}`

	captureCodexRateLimitLine(line, now)
	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache write for nested rateLimitsByLimitId")
	}
	if snap.Buckets[codexWindowPrimary].UsedPercentage != 77 {
		t.Errorf("primary=%+v, want UsedPercentage=77 from nested codex_other.primary", snap.Buckets[codexWindowPrimary])
	}
	if snap.Buckets[codexWindowSecondary].UsedPercentage != 55 {
		t.Errorf("secondary=%+v, want UsedPercentage=55 from nested codex_other.secondary", snap.Buckets[codexWindowSecondary])
	}
}

func TestCaptureCodexRateLimit_IgnoresUnrelatedFrames(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	// No token_count / rate_limit substring → cheap prefilter skips.
	captureCodexRateLimitLine(`{"jsonrpc":"2.0","method":"agent_message","params":{"text":"hi"}}`, now)
	// Malformed JSON → silent.
	captureCodexRateLimitLine(`{not json`, now)
	// Has the trigger word but no rate_limits map → no cache write.
	captureCodexRateLimitLine(`{"method":"token_count","params":{"msg":{"input_tokens":5}}}`, now)

	if _, ok := loadCodexRateLimitSnapshot(cache); ok {
		t.Fatalf("cache must not be written for unrelated frames")
	}
}

func TestCaptureCodexRateLimit_DropsStaleSnapshotOnAccountSwitch(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary: {UsedPercentage: 75, ResetsAtMs: now.Add(time.Hour).UnixMilli(), usageKnown: true, resetKnown: true},
	}, nil, now, "fingerprint-A")

	// Different account writes only secondary — primary from A must be dropped.
	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowSecondary: {UsedPercentage: 10, ResetsAtMs: now.Add(48 * time.Hour).UnixMilli(), usageKnown: true, resetKnown: true},
	}, nil, now, "fingerprint-B")

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache")
	}
	if snap.AccountFingerprint != "fingerprint-B" {
		t.Errorf("fingerprint=%q, want fingerprint-B", snap.AccountFingerprint)
	}
	if _, present := snap.Buckets[codexWindowPrimary]; present {
		t.Errorf("primary bucket from account A must be dropped on account switch")
	}
	if _, present := snap.Buckets[codexWindowSecondary]; !present {
		t.Errorf("secondary bucket from account B missing")
	}
}

func TestCodexMetricsFromCache_ObservedAndPastReset(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary:   {UsedPercentage: 31.2, ResetsAtMs: now.Add(2 * time.Hour).UnixMilli(), usageKnown: true, resetKnown: true},
		codexWindowSecondary: {UsedPercentage: 95, ResetsAtMs: now.Add(-time.Minute).UnixMilli(), usageKnown: true, resetKnown: true},
	}, nil, now, "")

	metrics := codexMetricsFromCache(now, "")
	if len(metrics) != 2 {
		t.Fatalf("want 2 metrics, got %d", len(metrics))
	}
	session, weekly := metrics[0], metrics[1]
	if session.Kind != limitKindSession || session.Unknown {
		t.Errorf("session metric=%+v, want observed limitKindSession", session)
	}
	if session.Consumed == nil || *session.Consumed != 31.2 {
		t.Errorf("session Consumed=%v, want 31.2", session.Consumed)
	}
	if session.ResetAt == "" {
		t.Errorf("live session window should advertise a ResetAt")
	}
	if weekly.Kind != limitKindWeekly {
		t.Errorf("weekly metric kind=%q, want %q", weekly.Kind, limitKindWeekly)
	}
	if weekly.Consumed == nil || *weekly.Consumed != 0 {
		t.Errorf("past-reset weekly Consumed=%v, want 0 (rolled over)", weekly.Consumed)
	}
	if weekly.ResetAt != "" {
		t.Errorf("past-reset window must not advertise a stale ResetAt")
	}
}

func TestCodexMetricsFromCache_NoCacheFallsBackToUnknown(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "absent.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)

	metrics := codexMetricsFromCache(time.Now(), "")
	if len(metrics) != 2 {
		t.Fatalf("want 2 metrics, got %d", len(metrics))
	}
	for _, m := range metrics {
		if !m.Unknown {
			t.Errorf("metric %q should be Unknown without a cache", m.Kind)
		}
	}
}

// When Codex reports a non-canonical window length (e.g. 15-minute primary in
// the documented account/rateLimits/read example, or a future plan), the metric
// label must reflect the actual duration rather than the hard-coded
// "5-hour session window" / "Weekly quota" strings.
func TestCodexMetricsFromCache_LabelDerivedFromWindowMinutes(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary: {
			UsedPercentage: 40, ResetsAtMs: now.Add(15 * time.Minute).UnixMilli(),
			WindowMinutes: 15, usageKnown: true, resetKnown: true,
		},
		codexWindowSecondary: {
			UsedPercentage: 10, ResetsAtMs: now.Add(24 * time.Hour).UnixMilli(),
			WindowMinutes: 1440, usageKnown: true, resetKnown: true,
		},
	}, nil, now, "")

	metrics := codexMetricsFromCache(now, "")
	if got := metrics[0].Label; got != "15-minute window" {
		t.Errorf("primary label=%q, want %q", got, "15-minute window")
	}
	if got := metrics[1].Label; got != "1-day window" {
		t.Errorf("secondary label=%q, want %q", got, "1-day window")
	}
}

// Canonical Codex windows (primary = 300 min, secondary = 10080 min) must keep
// their long-standing labels so the card text doesn't churn for existing users
// once duration-derived labels land.
func TestCodexMetricsFromCache_CanonicalWindowsKeepLabels(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary: {
			UsedPercentage: 20, ResetsAtMs: now.Add(5 * time.Hour).UnixMilli(),
			WindowMinutes: 300, usageKnown: true, resetKnown: true,
		},
		codexWindowSecondary: {
			UsedPercentage: 5, ResetsAtMs: now.Add(7 * 24 * time.Hour).UnixMilli(),
			WindowMinutes: 10080, usageKnown: true, resetKnown: true,
		},
	}, nil, now, "")

	metrics := codexMetricsFromCache(now, "")
	if got := metrics[0].Label; got != "5-hour session window" {
		t.Errorf("primary label=%q, want canonical 5-hour session window", got)
	}
	if got := metrics[1].Label; got != "Weekly quota" {
		t.Errorf("secondary label=%q, want canonical Weekly quota", got)
	}
}

func TestCodexMetricsFromCache_FingerprintMismatchHidesSnapshot(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary: {UsedPercentage: 75, ResetsAtMs: now.Add(time.Hour).UnixMilli(), usageKnown: true, resetKnown: true},
	}, nil, now, "fingerprint-A")

	metrics := codexMetricsFromCache(now, "fingerprint-B")
	for _, m := range metrics {
		if !m.Unknown {
			t.Errorf("metric %q should be Unknown when cache fingerprint mismatches", m.Kind)
		}
	}
}
