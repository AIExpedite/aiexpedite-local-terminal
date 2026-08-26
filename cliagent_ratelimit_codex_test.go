package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type cancelAfterChecksContext struct {
	checks int
	after  int
	done   chan struct{}
}

func (c *cancelAfterChecksContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelAfterChecksContext) Done() <-chan struct{}       { return c.done }
func (c *cancelAfterChecksContext) Value(any) any               { return nil }
func (c *cancelAfterChecksContext) Err() error {
	c.checks++
	if c.checks >= c.after {
		select {
		case <-c.done:
		default:
			close(c.done)
		}
		return context.Canceled
	}
	return nil
}

func TestCurrentCodexAccountFingerprint_NamespacedChatGPTUserID(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	helperWriteJSON(t, filepath.Join(codexHome, "auth.json"), map[string]any{
		"tokens": map[string]any{
			"id_token": helperJWT(t, map[string]any{
				"sub": "google-oauth2|117053759699842363930",
				"https://api.openai.com/auth": map[string]any{
					"chatgpt_user_id": "user-PPrLthwR0xmzb8mFDad617BZ",
				},
			}),
		},
	})

	want := fingerprintAccount("codex", "user-PPrLthwR0xmzb8mFDad617BZ")
	if got := currentCodexAccountFingerprint(); got != want {
		t.Errorf("currentCodexAccountFingerprint()=%q, want %q", got, want)
	}
}

func TestCurrentCodexAccountFingerprint_WorkspaceAccountIDBeatsUserID(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	helperWriteJSON(t, filepath.Join(codexHome, "auth.json"), map[string]any{
		"tokens": map[string]any{
			"account_id": "workspace-B",
			"id_token": helperJWT(t, map[string]any{
				"https://api.openai.com/auth": map[string]any{
					"chatgpt_user_id":    "user-PPrLthwR0xmzb8mFDad617BZ",
					"chatgpt_account_id": "workspace-A",
				},
			}),
		},
	})

	want := fingerprintAccount("codex", "workspace-B")
	if got := currentCodexAccountFingerprint(); got != want {
		t.Errorf("currentCodexAccountFingerprint()=%q, want %q", got, want)
	}
}

// codexWindowIdentity is the linchpin classifier for dedupe/rows/labels. Cover
// its contract directly: canonical bands (incl. floored), non-canonical
// duration, and length-less slot-default — independent of any cache plumbing.
func TestCodexWindowIdentity(t *testing.T) {
	cases := []struct {
		name    string
		minutes float64
		slot    string
		want    string
	}{
		{"canonical session", 300, codexWindowPrimary, codexIdentitySession},
		{"floored session", 299, codexWindowSecondary, codexIdentitySession},
		{"canonical weekly", 10080, codexWindowSecondary, codexIdentityWeekly},
		{"floored weekly", 10079, codexWindowPrimary, codexIdentityWeekly},
		{"weekly-band under primary stays weekly", 10080, codexWindowPrimary, codexIdentityWeekly},
		{"non-canonical duration", 240, codexWindowPrimary, "duration:240"},
		{"non-canonical duration secondary", 8640, codexWindowSecondary, "duration:8640"},
		{"lengthless primary defaults session", 0, codexWindowPrimary, codexIdentitySession},
		{"lengthless secondary defaults weekly", 0, codexWindowSecondary, codexIdentityWeekly},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := codexWindowIdentity(c.minutes, c.slot); got != c.want {
				t.Errorf("codexWindowIdentity(%v, %q)=%q, want %q", c.minutes, c.slot, got, c.want)
			}
		})
	}
}

// codexPlacementBeats is the total order used to pick a winner among same-metric
// placements. Freshness dominates; on an exact freshness tie the ladder falls to
// higher usage, then primary-over-secondary, then known-over-unknown. The
// equal-freshness branches are otherwise only reachable via rare same-timestamp
// duplicates, so exercise them directly.
func TestCodexPlacementBeats_TieBreakLadder(t *testing.T) {
	base := codexRateLimitBucket{ObservedAtMs: 1000, UsedPercentage: 50, usageKnown: true}

	// Freshness dominates even when the stale side reports higher usage.
	newerLower := codexRateLimitBucket{ObservedAtMs: 2000, UsedPercentage: 10, usageKnown: true}
	if !codexPlacementBeats(newerLower, codexWindowSecondary, base, codexWindowPrimary) {
		t.Error("newer observation must beat an older one regardless of usage/slot")
	}

	// Equal freshness → higher usage wins.
	higher := codexRateLimitBucket{ObservedAtMs: 1000, UsedPercentage: 80, usageKnown: true}
	if !codexPlacementBeats(higher, codexWindowSecondary, base, codexWindowPrimary) {
		t.Error("on equal freshness the higher usage must win")
	}

	// Equal freshness and usage → primary slot precedence.
	primaryTie := codexRateLimitBucket{ObservedAtMs: 1000, UsedPercentage: 50, usageKnown: true}
	if !codexPlacementBeats(primaryTie, codexWindowPrimary, base, codexWindowSecondary) {
		t.Error("on a full tie the primary slot must take precedence")
	}
	if codexPlacementBeats(base, codexWindowSecondary, primaryTie, codexWindowPrimary) {
		t.Error("secondary must not beat primary on a full tie")
	}

	// Everything equal incl. slot → known usage beats unknown.
	known := codexRateLimitBucket{ObservedAtMs: 1000, UsedPercentage: 50, usageKnown: true}
	unknown := codexRateLimitBucket{ObservedAtMs: 1000, UsedPercentage: 50, usageKnown: false}
	if !codexPlacementBeats(known, codexWindowPrimary, unknown, codexWindowPrimary) {
		t.Error("a known-usage reading must beat an unknown one on an otherwise full tie")
	}
}

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

// A clear-only full read (`result.rateLimits: {"secondary": null}` with no
// restated bucket) is authoritative that the weekly window is gone. The
// slot-keyed clear only removes the physical `secondary` slot, so a stale
// weekly-band contributor that had migrated into `primary` must also be dropped
// by the identity-keyed omission pass — otherwise the retired weekly keeps
// rendering under primary even though the snapshot declared it gone.
func TestCaptureCodexRateLimit_ClearOnlyFullReadDropsMigratedWeekly(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	// Seed the legacy on-disk placement directly: live capture now normalizes
	// this weekly identity to secondary before merging, but an older cache may
	// still contain it under primary and the clear path must handle that shape.
	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary: {
			UsedPercentage: 90, WindowMinutes: 10080,
			ResetsAtMs: now.Add(7 * 24 * time.Hour).UnixMilli(),
			usageKnown: true, resetKnown: true,
		},
	}, nil, now, "")
	if snap, ok := loadCodexRateLimitSnapshot(cache); !ok || snap.Buckets[codexWindowPrimary].UsedPercentage != 90 {
		t.Fatalf("seed failed: %+v", snap.Buckets)
	}

	// Full read that ONLY clears secondary — no restated primary. The weekly
	// window is authoritatively gone; the migrated copy under primary must not
	// survive.
	captureCodexRateLimitLine(
		`{"jsonrpc":"2.0","id":3,"result":{"rateLimits":{"secondary":null}}}`,
		now,
	)
	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache after full read")
	}
	if b, present := snap.Buckets[codexWindowPrimary]; present {
		t.Errorf("migrated weekly under primary must be dropped by clear-only full read: %+v", b)
	}
	if _, present := snap.Contributors[codexWindowPrimary]; present {
		t.Errorf("migrated weekly contributor under primary must be reconciled away: %+v", snap.Contributors)
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

// A sparse live update can report the weekly identity under the physical
// `primary` slot. When session and weekly share the legacy limit id, normalize
// that update before the slot-keyed merge so it cannot overwrite the cached
// session contributor before identity reconciliation runs.
func TestCaptureCodexRateLimit_SparseMigratedWeeklyPreservesSessionWithSameLimitID(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	captureCodexRateLimitLine(
		`{"method":"token_count","params":{"rate_limits":{`+
			`"primary":{"used_percent":25,"window_minutes":300,"resets_in_seconds":1800},`+
			`"secondary":{"used_percent":60,"window_minutes":10080,"resets_in_seconds":604800}`+
			`}}}`,
		now,
	)
	captureCodexRateLimitLine(
		`{"method":"token_count","params":{"rate_limits":{`+
			`"primary":{"used_percent":70,"window_minutes":10080,"resets_in_seconds":604800}`+
			`}}}`,
		now.Add(time.Minute),
	)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatal("expected cache")
	}
	if session, present := snap.Buckets[codexWindowPrimary]; !present || session.UsedPercentage != 25 {
		t.Errorf("session bucket=%+v, present=%v; want preserved 25%% session", session, present)
	}
	if weekly, present := snap.Buckets[codexWindowSecondary]; !present || weekly.UsedPercentage != 70 {
		t.Errorf("weekly bucket=%+v, present=%v; want refreshed 70%% weekly", weekly, present)
	}
	if _, present := snap.Contributors[codexWindowPrimary][codexLegacyLimitID]; !present {
		t.Errorf("session contributor missing after migrated weekly update: %+v", snap.Contributors)
	}
	if _, present := snap.Contributors[codexWindowSecondary][codexLegacyLimitID]; !present {
		t.Errorf("weekly contributor missing after migrated weekly update: %+v", snap.Contributors)
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

	// Sparse update #1: reset-only restating the same live reset five minutes
	// later. It must preserve both 60% and that percentage's original
	// observation timestamp; the reset notification did not observe usage.
	later := now.Add(5 * time.Minute)
	captureCodexRateLimitLine(
		`{"method":"account/rateLimits/updated","params":{"rateLimits":{"primary":{"resetsInSeconds":3300}}}}`,
		later,
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
	if got := snap.Buckets[codexWindowPrimary].ObservedAtMs; got != now.UnixMilli() {
		t.Errorf("after reset-only update ObservedAtMs=%d, want original usage observation %d", got, now.UnixMilli())
	}

	// Sparse update #2: usage-only (no reset). Must preserve the live reset.
	captureCodexRateLimitLine(
		`{"method":"account/rateLimits/updated","params":{"rateLimits":{"primary":{"usedPercent":72}}}}`,
		later,
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

// A full `account/rateLimits/read` response can carry
// `rateLimitsByLimitId.<limit>.secondary: null` to declare that this metered
// limit no longer constrains the weekly window. When the snapshot has no
// other source of secondary data (no aggregate `rateLimits` entry, no other
// metered bucket), a previously-cached secondary must be cleared rather than
// left to render stale usage until its old reset passes.
func TestCaptureCodexRateLimit_MultiBucketNestedNullClearsWindow(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	// Seed a prior secondary so we can prove the null actually clears it.
	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowSecondary: {
			UsedPercentage: 60,
			ResetsAtMs:     now.Add(48 * time.Hour).UnixMilli(),
			usageKnown:     true,
			resetKnown:     true,
		},
	}, nil, now, "")

	// Full snapshot: nested `codex_primary.primary` carries the primary update,
	// but the only secondary mention is `codex_primary.secondary: null`.
	line := `{"jsonrpc":"2.0","result":{"rateLimitsByLimitId":{` +
		`"codex_primary":{"primary":{"usedPercent":40,"resetsInSeconds":1800,"windowDurationMins":300},"secondary":null}` +
		`}},"id":1}`
	captureCodexRateLimitLine(line, now.Add(time.Minute))

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache")
	}
	if p := snap.Buckets[codexWindowPrimary]; p.UsedPercentage != 40 {
		t.Errorf("primary UsedPercentage=%v, want 40", p.UsedPercentage)
	}
	if _, present := snap.Buckets[codexWindowSecondary]; present {
		t.Errorf("secondary bucket must be cleared by nested null in full snapshot")
	}
}

// Two metered buckets can report identical utilisation (commonly both 100%
// exhausted) for the same window, each with its own resetsAt. The merge must
// not silently keep whichever bucket the Go map happened to visit first — that
// could advertise an earlier reset while another equally-exhausted bucket
// still blocks usage. Equal-usage buckets should be merged conservatively by
// keeping the LATER reset (or dropping the reset entirely when one is
// unknown).
func TestExtractCodexRateLimitBuckets_TieKeepsLaterReset(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	earlierResetMs := now.Add(30 * time.Minute).UnixMilli()
	laterResetMs := now.Add(2 * time.Hour).UnixMilli()

	// Two codex_other-style buckets at 100% on the same primary window, with
	// different reset times. Whichever order the map iterates, the merged
	// primary must reflect the later reset.
	raw := map[string]interface{}{
		"result": map[string]interface{}{
			"rateLimitsByLimitId": map[string]interface{}{
				"codex_primary_a": map[string]interface{}{
					"usedPercent":        100.0,
					"windowDurationMins": 300.0,
					"resetsAt":           float64(earlierResetMs),
				},
				"codex_primary_b": map[string]interface{}{
					"usedPercent":        100.0,
					"windowDurationMins": 300.0,
					"resetsAt":           float64(laterResetMs),
				},
			},
		},
	}
	perLimit, _ := extractCodexRateLimitBuckets(raw, now)
	buckets := aggregateCodexBuckets(perLimit, now)
	p, ok := buckets[codexWindowPrimary]
	if !ok {
		t.Fatalf("expected primary bucket, got %+v", buckets)
	}
	if p.UsedPercentage != 100 {
		t.Errorf("primary UsedPercentage=%v, want 100", p.UsedPercentage)
	}
	if p.ResetsAtMs != laterResetMs {
		t.Errorf("primary ResetsAtMs=%v, want later reset %v (tie must not let earlier reset win)", p.ResetsAtMs, laterResetMs)
	}
}

func TestMergeCodexBucketMostConstrained_ZeroTieKeepsLiveObservation(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	expired := codexRateLimitBucket{
		UsedPercentage: 0,
		ResetsAtMs:     now.Add(-time.Hour).UnixMilli(),
		ObservedAtMs:   now.Add(-2 * time.Hour).UnixMilli(),
		usageKnown:     true,
		resetKnown:     true,
	}
	live := codexRateLimitBucket{
		UsedPercentage: 0,
		ResetsAtMs:     now.Add(time.Hour).UnixMilli(),
		ObservedAtMs:   now.Add(-time.Minute).UnixMilli(),
		usageKnown:     true,
		resetKnown:     true,
	}

	for _, tc := range []struct {
		name   string
		first  codexRateLimitBucket
		second codexRateLimitBucket
	}{
		{name: "expired first", first: expired, second: live},
		{name: "live first", first: live, second: expired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := map[string]codexRateLimitBucket{codexWindowPrimary: tc.first}
			mergeCodexBucketMostConstrained(out, codexWindowPrimary, tc.second)
			got := out[codexWindowPrimary]
			if got.ResetsAtMs != live.ResetsAtMs {
				t.Fatalf("ResetsAtMs=%d, want live reset %d", got.ResetsAtMs, live.ResetsAtMs)
			}
			if got.ObservedAtMs != live.ObservedAtMs {
				t.Errorf("ObservedAtMs=%d, want live observation %d", got.ObservedAtMs, live.ObservedAtMs)
			}
		})
	}
}

func TestMergeCodexBucketMostConstrained_LiveKnownResetTieKeepsFreshestObservation(t *testing.T) {
	freshEarlierReset := codexRateLimitBucket{
		UsedPercentage: 55,
		ResetsAtMs:     4000,
		ObservedAtMs:   3000,
		usageKnown:     true,
		resetKnown:     true,
	}
	staleLaterReset := codexRateLimitBucket{
		UsedPercentage: 55,
		ResetsAtMs:     5000,
		ObservedAtMs:   1000,
		usageKnown:     true,
		resetKnown:     true,
	}

	for _, tc := range []struct {
		name   string
		first  codexRateLimitBucket
		second codexRateLimitBucket
	}{
		{name: "fresh first", first: freshEarlierReset, second: staleLaterReset},
		{name: "stale first", first: staleLaterReset, second: freshEarlierReset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := map[string]codexRateLimitBucket{codexWindowPrimary: tc.first}
			mergeCodexBucketMostConstrained(out, codexWindowPrimary, tc.second)
			got := out[codexWindowPrimary]
			if got.ResetsAtMs != staleLaterReset.ResetsAtMs {
				t.Errorf("ResetsAtMs=%d, want later aggregate reset %d", got.ResetsAtMs, staleLaterReset.ResetsAtMs)
			}
			if got.ObservedAtMs != freshEarlierReset.ObservedAtMs {
				t.Errorf("ObservedAtMs=%d, want freshest tied live observation %d", got.ObservedAtMs, freshEarlierReset.ObservedAtMs)
			}
		})
	}
}

func TestMergeCodexRolloutFrame_ResetOnlyPreservesUsageObservationTime(t *testing.T) {
	frameTime := time.Date(2026, 6, 15, 12, 30, 0, 0, time.UTC)
	resetAt := frameTime.Add(time.Hour).UnixMilli()
	originalObservedAt := frameTime.Add(-30 * time.Minute).UnixMilli()
	acc := map[string]map[string]codexRateLimitBucket{
		codexWindowPrimary: {
			"codex_primary": {
				UsedPercentage: 64,
				ResetsAtMs:     resetAt,
				ObservedAtMs:   originalObservedAt,
				usageKnown:     true,
				resetKnown:     true,
			},
		},
	}
	updates := map[string]map[string]codexRateLimitBucket{
		codexWindowPrimary: {
			"codex_primary": {
				ResetsAtMs:   resetAt,
				ObservedAtMs: frameTime.UnixMilli(),
				resetKnown:   true,
			},
		},
	}

	mergeCodexRolloutFrame(acc, updates, frameTime)
	got := acc[codexWindowPrimary]["codex_primary"]
	if !got.usageKnown || got.UsedPercentage != 64 {
		t.Errorf("carried usage=(known=%v, used=%v), want true/64", got.usageKnown, got.UsedPercentage)
	}
	if got.ObservedAtMs != originalObservedAt {
		t.Errorf("ObservedAtMs=%d, want original usage observation %d", got.ObservedAtMs, originalObservedAt)
	}
}

func TestMergeCodexBucketMostConstrained_TieWithoutCommonResetKeepsFreshObservation(t *testing.T) {
	older := codexRateLimitBucket{
		UsedPercentage: 50,
		ResetsAtMs:     time.Now().Add(time.Hour).UnixMilli(),
		ObservedAtMs:   1000,
		usageKnown:     true,
		resetKnown:     true,
	}
	fresher := codexRateLimitBucket{
		UsedPercentage: 50,
		ObservedAtMs:   2000,
		usageKnown:     true,
		resetKnown:     false,
	}

	for _, tc := range []struct {
		name   string
		first  codexRateLimitBucket
		second codexRateLimitBucket
	}{
		{name: "known reset first", first: older, second: fresher},
		{name: "unknown reset first", first: fresher, second: older},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := map[string]codexRateLimitBucket{codexWindowPrimary: tc.first}
			mergeCodexBucketMostConstrained(out, codexWindowPrimary, tc.second)
			got := out[codexWindowPrimary]
			if got.resetKnown || got.ResetsAtMs != 0 {
				t.Errorf("reset=(%v, %d), want unknown", got.resetKnown, got.ResetsAtMs)
			}
			if got.ObservedAtMs != fresher.ObservedAtMs {
				t.Errorf("ObservedAtMs=%d, want freshest observation %d", got.ObservedAtMs, fresher.ObservedAtMs)
			}
		})
	}
}

func TestMergeCodexBucketMostConstrained_ZeroTieDoesNotBorrowExpiredObservation(t *testing.T) {
	expired := codexRateLimitBucket{
		UsedPercentage: 0,
		ResetsAtMs:     1000,
		ObservedAtMs:   3000,
		usageKnown:     true,
		resetKnown:     true,
		rolledOver:     true,
	}
	resetless := codexRateLimitBucket{
		UsedPercentage: 0,
		ObservedAtMs:   2000,
		usageKnown:     true,
		resetKnown:     false,
	}

	for _, tc := range []struct {
		name   string
		first  codexRateLimitBucket
		second codexRateLimitBucket
	}{
		{name: "expired first", first: expired, second: resetless},
		{name: "resetless first", first: resetless, second: expired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := map[string]codexRateLimitBucket{codexWindowPrimary: tc.first}
			mergeCodexBucketMostConstrained(out, codexWindowPrimary, tc.second)
			got := out[codexWindowPrimary]
			if got.ObservedAtMs != resetless.ObservedAtMs {
				t.Errorf("ObservedAtMs=%d, want reset-less observation %d", got.ObservedAtMs, resetless.ObservedAtMs)
			}
		})
	}
}

func TestMergeCodexBucketMostConstrained_LiveOneResetTieKeepsFreshObservation(t *testing.T) {
	liveKnownReset := codexRateLimitBucket{
		UsedPercentage: 40,
		ResetsAtMs:     4000,
		ObservedAtMs:   3000,
		usageKnown:     true,
		resetKnown:     true,
	}
	staleResetless := codexRateLimitBucket{
		UsedPercentage: 40,
		ObservedAtMs:   2000,
		usageKnown:     true,
		resetKnown:     false,
	}

	for _, tc := range []struct {
		name   string
		first  codexRateLimitBucket
		second codexRateLimitBucket
	}{
		{name: "known reset first", first: liveKnownReset, second: staleResetless},
		{name: "resetless first", first: staleResetless, second: liveKnownReset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := map[string]codexRateLimitBucket{codexWindowPrimary: tc.first}
			mergeCodexBucketMostConstrained(out, codexWindowPrimary, tc.second)
			if got := out[codexWindowPrimary].ObservedAtMs; got != liveKnownReset.ObservedAtMs {
				t.Errorf("ObservedAtMs=%d, want freshest live observation %d", got, liveKnownReset.ObservedAtMs)
			}
		})
	}
}

func TestMergeCodexBucketMostConstrained_MultiWayZeroTiePreservesLiveProvenance(t *testing.T) {
	live := codexRateLimitBucket{
		UsedPercentage: 0,
		ObservedAtMs:   2000,
		usageKnown:     true,
	}
	expiredA := codexRateLimitBucket{
		UsedPercentage: 0,
		ResetsAtMs:     1000,
		ObservedAtMs:   3000,
		usageKnown:     true,
		resetKnown:     true,
		rolledOver:     true,
	}
	expiredB := codexRateLimitBucket{
		UsedPercentage: 0,
		ResetsAtMs:     1500,
		ObservedAtMs:   4000,
		usageKnown:     true,
		resetKnown:     true,
		rolledOver:     true,
	}

	orders := [][]codexRateLimitBucket{
		{expiredA, live, expiredB},
		{expiredA, expiredB, live},
		{live, expiredA, expiredB},
	}
	for i, order := range orders {
		t.Run(fmt.Sprintf("order %d", i), func(t *testing.T) {
			out := map[string]codexRateLimitBucket{}
			for _, bucket := range order {
				mergeCodexBucketMostConstrained(out, codexWindowPrimary, bucket)
			}
			got := out[codexWindowPrimary]
			if got.rolledOver {
				t.Error("aggregate with a live tied contributor must not remain rolled over")
			}
			if got.ObservedAtMs != live.ObservedAtMs {
				t.Errorf("ObservedAtMs=%d, want live observation %d", got.ObservedAtMs, live.ObservedAtMs)
			}
		})
	}
}

func TestMergeCodexBucketMostConstrained_MultiWayKnownResetTiePreservesLiveProvenance(t *testing.T) {
	liveKnownReset := codexRateLimitBucket{
		UsedPercentage: 0,
		ResetsAtMs:     5000,
		ObservedAtMs:   3000,
		usageKnown:     true,
		resetKnown:     true,
	}
	liveResetless := codexRateLimitBucket{
		UsedPercentage: 0,
		ObservedAtMs:   1000,
		usageKnown:     true,
	}
	expired := codexRateLimitBucket{
		UsedPercentage: 0,
		ResetsAtMs:     2000,
		ObservedAtMs:   4000,
		usageKnown:     true,
		resetKnown:     true,
		rolledOver:     true,
	}

	orders := [][]codexRateLimitBucket{
		{expired, liveKnownReset, liveResetless},
		{expired, liveResetless, liveKnownReset},
		{liveKnownReset, expired, liveResetless},
		{liveKnownReset, liveResetless, expired},
		{liveResetless, expired, liveKnownReset},
		{liveResetless, liveKnownReset, expired},
	}
	for i, order := range orders {
		t.Run(fmt.Sprintf("order %d", i), func(t *testing.T) {
			out := map[string]codexRateLimitBucket{}
			for _, bucket := range order {
				mergeCodexBucketMostConstrained(out, codexWindowPrimary, bucket)
			}
			got := out[codexWindowPrimary]
			if got.rolledOver {
				t.Error("aggregate with live tied contributors must not remain rolled over")
			}
			if got.ObservedAtMs != liveKnownReset.ObservedAtMs {
				t.Errorf("ObservedAtMs=%d, want freshest live observation %d", got.ObservedAtMs, liveKnownReset.ObservedAtMs)
			}
		})
	}
}

// When two equally-exhausted buckets disagree on whether a reset is known, the
// safe answer is "unknown" — don't promise a reset time that the unknown side
// can't confirm.
func TestExtractCodexRateLimitBuckets_TieDropsResetWhenOneUnknown(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	knownResetMs := now.Add(30 * time.Minute).UnixMilli()

	raw := map[string]interface{}{
		"result": map[string]interface{}{
			"rateLimitsByLimitId": map[string]interface{}{
				"codex_primary_a": map[string]interface{}{
					"usedPercent":        100.0,
					"windowDurationMins": 300.0,
					"resetsAt":           float64(knownResetMs),
				},
				// Same 100% usage but no reset hint — its real reset is
				// unknown to us, so we must not promise the other bucket's.
				"codex_primary_b": map[string]interface{}{
					"usedPercent":        100.0,
					"windowDurationMins": 300.0,
				},
			},
		},
	}
	perLimit, _ := extractCodexRateLimitBuckets(raw, now)
	buckets := aggregateCodexBuckets(perLimit, now)
	p, ok := buckets[codexWindowPrimary]
	if !ok {
		t.Fatalf("expected primary bucket, got %+v", buckets)
	}
	if p.ResetsAtMs != 0 {
		t.Errorf("primary ResetsAtMs=%v, want 0 (tie with one unknown reset must drop the reset)", p.ResetsAtMs)
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

func TestRolloutMerge_RevalidatesAccountInsideCacheTransaction(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	accountBase := t.TempDir()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	fingerprintA := fingerprintAccount("codex", "workspace-A")
	fingerprintB := fingerprintAccount("codex", "workspace-B")

	// Account B has already signed in and populated the cache by the time an
	// account-A rollout scan reaches its locked read-modify-write transaction.
	helperWriteJSON(t, filepath.Join(accountBase, "auth.json"), map[string]any{
		"tokens": map[string]any{"account_id": "workspace-B"},
	})
	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowSecondary: {
			UsedPercentage: 12,
			ObservedAtMs:   now.UnixMilli(),
			usageKnown:     true,
		},
	}, nil, now, fingerprintB)

	progress := &codexRolloutScanProgress{mtimeNs: now.UnixNano()}
	mergeCodexRateLimitCachePerLimitProgress(cache, map[string]map[string]codexRateLimitBucket{
		codexWindowPrimary: {
			codexLegacyLimitID: {
				UsedPercentage: 91,
				ObservedAtMs:   now.Add(time.Minute).UnixMilli(),
				usageKnown:     true,
			},
		},
	}, nil, false, nil, false, now.Add(time.Minute), fingerprintA, progress, accountBase)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatal("expected account-B cache to remain readable")
	}
	if snap.AccountFingerprint != fingerprintB {
		t.Fatalf("fingerprint=%q, want current account B %q", snap.AccountFingerprint, fingerprintB)
	}
	if _, present := snap.Buckets[codexWindowPrimary]; present {
		t.Fatalf("old account-A rollout must not replace current cache: %+v", snap.Buckets)
	}
	if got := snap.Buckets[codexWindowSecondary].UsedPercentage; got != 12 {
		t.Fatalf("account-B weekly usage=%v, want 12", got)
	}
	if snap.RolloutHighWaterMtimeNs != 0 {
		t.Fatalf("old account-A rollout must not advance current account cursor: %d", snap.RolloutHighWaterMtimeNs)
	}
}

func TestCodexMetricsFromCache_ObservedAndPastReset(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary:   {UsedPercentage: 31.2, ResetsAtMs: now.Add(2 * time.Hour).UnixMilli(), ObservedAtMs: now.UnixMilli(), usageKnown: true, resetKnown: true},
		codexWindowSecondary: {UsedPercentage: 95, ResetsAtMs: now.Add(-time.Minute).UnixMilli(), ObservedAtMs: now.UnixMilli(), usageKnown: true, resetKnown: true},
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
	if !weekly.Unknown || weekly.Consumed != nil {
		t.Errorf("past-reset weekly metric=%+v, want unobservable", weekly)
	}
	if weekly.ResetAt != "" {
		t.Errorf("past-reset window must not advertise a stale ResetAt")
	}
	if weekly.ObservedAt == "" || session.ObservedAt == "" {
		t.Errorf("metrics must preserve observation time: session=%+v weekly=%+v", session, weekly)
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

// Codex `token_count` JSONL commonly reports the canonical primary/secondary
// quota windows with a floored minute count (e.g. window_minutes: 299 for the
// 5-hour window, 10079 for the weekly window — see openai/codex#14728). The
// label deriver must tolerate that floor instead of falling back to the
// generic "299-minute window" / "168.0-hour window" strings.
func TestCodexMetricsFromCache_RoundedCanonicalWindowsKeepLabels(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary: {
			UsedPercentage: 20, ResetsAtMs: now.Add(5 * time.Hour).UnixMilli(),
			WindowMinutes: 299, usageKnown: true, resetKnown: true,
		},
		codexWindowSecondary: {
			UsedPercentage: 5, ResetsAtMs: now.Add(7 * 24 * time.Hour).UnixMilli(),
			WindowMinutes: 10079, usageKnown: true, resetKnown: true,
		},
	}, nil, now, "")

	metrics := codexMetricsFromCache(now, "")
	if got := metrics[0].Label; got != "5-hour session window" {
		t.Errorf("primary label=%q, want canonical 5-hour session window (floored 299)", got)
	}
	if got := metrics[1].Label; got != "Weekly quota" {
		t.Errorf("secondary label=%q, want canonical Weekly quota (floored 10079)", got)
	}
}

// A window length that is clearly NOT a rounded canonical (e.g. 4-hour primary
// or 6-day weekly) must continue to render the neutral duration-derived label,
// so the tolerance band cannot silently mask a genuinely different plan.
func TestCodexMetricsFromCache_DistinctWindowsBypassCanonicalBands(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary: {
			UsedPercentage: 20, ResetsAtMs: now.Add(4 * time.Hour).UnixMilli(),
			WindowMinutes: 240, usageKnown: true, resetKnown: true,
		},
		codexWindowSecondary: {
			UsedPercentage: 5, ResetsAtMs: now.Add(6 * 24 * time.Hour).UnixMilli(),
			WindowMinutes: 8640, usageKnown: true, resetKnown: true,
		},
	}, nil, now, "")

	metrics := codexMetricsFromCache(now, "")
	if got := metrics[0].Label; got != "4-hour window" {
		t.Errorf("primary label=%q, want %q (240 min must not snap to 5-hour band)", got, "4-hour window")
	}
	if got := metrics[1].Label; got != "6-day window" {
		t.Errorf("secondary label=%q, want %q (8640 min must not snap to weekly band)", got, "6-day window")
	}
}

// When two metered buckets land on the same display window and the higher-
// utilisation one resets FIRST, the merge must carry the runner-up's later
// reset forward so codexObservedMetricOrUnknown doesn't zero the entire
// window at the earlier reset — the runner-up bucket is still live and
// contributing usage past that point.
func TestExtractCodexRateLimitBuckets_HigherUsagePreservesLaterReset(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	earlierResetMs := now.Add(30 * time.Minute).UnixMilli()
	laterResetMs := now.Add(2 * time.Hour).UnixMilli()

	raw := map[string]interface{}{
		"result": map[string]interface{}{
			"rateLimitsByLimitId": map[string]interface{}{
				// Higher utilisation but resets first.
				"codex_primary_a": map[string]interface{}{
					"usedPercent":        90.0,
					"windowDurationMins": 300.0,
					"resetsAt":           float64(earlierResetMs),
				},
				// Lower utilisation but resets later — still live past A's reset.
				"codex_primary_b": map[string]interface{}{
					"usedPercent":        40.0,
					"windowDurationMins": 300.0,
					"resetsAt":           float64(laterResetMs),
				},
			},
		},
	}
	perLimit, _ := extractCodexRateLimitBuckets(raw, now)
	buckets := aggregateCodexBuckets(perLimit, now)
	p, ok := buckets[codexWindowPrimary]
	if !ok {
		t.Fatalf("expected primary bucket, got %+v", buckets)
	}
	if p.UsedPercentage != 90 {
		t.Errorf("primary UsedPercentage=%v, want 90 (most-constrained view)", p.UsedPercentage)
	}
	if p.ResetsAtMs != laterResetMs {
		t.Errorf("primary ResetsAtMs=%v, want later reset %v (earlier reset would zero the window while runner-up is still live)", p.ResetsAtMs, laterResetMs)
	}
}

// When the stricter (higher-usage) bucket has no reset of its own, we must
// not borrow the reset from a lower-usage bucket: the UI would otherwise
// clear the displayed window at a time the stricter bucket has not confirmed.
func TestExtractCodexRateLimitBuckets_StricterBucketWithoutResetDropsBorrowedReset(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	lowerBucketResetMs := now.Add(30 * time.Minute).UnixMilli()

	// codex_other.primary at 80% with no reset; codex_primary at 20% with a
	// reset. The display should report the 80% but leave reset unknown.
	raw := map[string]interface{}{
		"result": map[string]interface{}{
			"rateLimitsByLimitId": map[string]interface{}{
				"codex_other": map[string]interface{}{
					"primary": map[string]interface{}{
						"usedPercent":        80.0,
						"windowDurationMins": 300.0,
					},
				},
				"codex_primary": map[string]interface{}{
					"primary": map[string]interface{}{
						"usedPercent":        20.0,
						"windowDurationMins": 300.0,
						"resetsAt":           float64(lowerBucketResetMs),
					},
				},
			},
		},
	}
	perLimit, _ := extractCodexRateLimitBuckets(raw, now)
	buckets := aggregateCodexBuckets(perLimit, now)
	p, ok := buckets[codexWindowPrimary]
	if !ok {
		t.Fatalf("expected primary bucket, got %+v", buckets)
	}
	if p.UsedPercentage != 80 {
		t.Errorf("primary UsedPercentage=%v, want 80 (most-constrained view)", p.UsedPercentage)
	}
	if p.resetKnown || p.ResetsAtMs != 0 {
		t.Errorf("primary reset must stay unknown when stricter bucket has no reset (got ResetsAtMs=%v resetKnown=%v)", p.ResetsAtMs, p.resetKnown)
	}
}

// A heartbeat reset-only frame recomputes ResetsAtMs from the local receive
// time plus `resets_in_seconds`. Between two consecutive frames the same live
// window's recomputed reset can drift by a second; exact equality would treat
// that as a rollover and discard the prior used %, flipping the card to
// Unknown until the next usage frame. The merge must tolerate sub-minute
// jitter so a heartbeat preserves the prior usage.
func TestCaptureCodexRateLimit_ResetOnlyJitterPreservesPriorUsage(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	first := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	// Seed: 65% used, resets in exactly 1h.
	captureCodexRateLimitLine(
		`{"method":"token_count","params":{"rate_limits":{"primary":{"used_percent":65,"resets_in_seconds":3600}}}}`,
		first,
	)

	// Heartbeat 2s later, reset still 1h away — Codex emits seconds-precision
	// `resetsInSeconds`, so the recomputed absolute reset drifts by ~2s. This
	// is the SAME live window, not a rollover.
	heartbeat := first.Add(2 * time.Second)
	captureCodexRateLimitLine(
		`{"method":"account/rateLimits/updated","params":{"rateLimits":{"primary":{"resetsInSeconds":3598}}}}`,
		heartbeat,
	)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache")
	}
	b, present := snap.Buckets[codexWindowPrimary]
	if !present {
		t.Fatalf("primary bucket must survive a jitter-only heartbeat, got %+v", snap.Buckets)
	}
	if b.UsedPercentage != 65 {
		t.Errorf("UsedPercentage=%v, want 65 (heartbeat within jitter must preserve prior usage)", b.UsedPercentage)
	}
}

// A full account/rateLimits/read can cache one window from a strict
// `codex_other` contributor (e.g. 80% on primary). A later sparse
// `account/rateLimits/updated` that only restates `codex_primary` at 20% must
// NOT lower the cached aggregate to 20% — the prior `codex_other` contributor
// is still live for the same window and the sparse frame never said it
// dropped. The on-disk per-(window, limit) Contributors map is what makes
// that preservation possible.
func TestCaptureCodexRateLimit_SparseByLimitPreservesPriorStricterContributor(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	// Full read seeds primary with two contributors: codex_primary at 20% and a
	// stricter codex_other at 80% on the same 5-hour window.
	captureCodexRateLimitLine(
		`{"jsonrpc":"2.0","id":1,"result":{"rateLimitsByLimitId":{`+
			`"codex_primary":{"primary":{"usedPercent":20,"resetsInSeconds":1800,"windowDurationMins":300}},`+
			`"codex_other":{"primary":{"usedPercent":80,"resetsInSeconds":1800,"windowDurationMins":300}}`+
			`}}}`,
		now,
	)
	if snap, ok := loadCodexRateLimitSnapshot(cache); !ok || snap.Buckets[codexWindowPrimary].UsedPercentage != 80 {
		t.Fatalf("seed should aggregate to 80%% from codex_other, got %+v", snap.Buckets)
	}

	// Later sparse update only restates codex_primary at 20% on the same live
	// window. codex_other was not mentioned but is still live, so the aggregate
	// must STAY at 80%.
	captureCodexRateLimitLine(
		`{"jsonrpc":"2.0","method":"account/rateLimits/updated","params":{"rateLimitsByLimitId":{`+
			`"codex_primary":{"primary":{"usedPercent":20,"resetsInSeconds":1800,"windowDurationMins":300}}`+
			`}}}`,
		now.Add(30*time.Second),
	)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache")
	}
	p, present := snap.Buckets[codexWindowPrimary]
	if !present {
		t.Fatalf("primary bucket missing after sparse by-limit update: %+v", snap.Buckets)
	}
	if p.UsedPercentage != 80 {
		t.Errorf("aggregate UsedPercentage=%v, want 80 (codex_other still live, sparse update for codex_primary must not lower it)", p.UsedPercentage)
	}
	// Sanity: both contributors should be present in the per-limit map.
	contribs := snap.Contributors[codexWindowPrimary]
	if got := contribs["codex_other"].UsedPercentage; got != 80 {
		t.Errorf("codex_other contributor UsedPercentage=%v, want 80", got)
	}
	if got := contribs["codex_primary"].UsedPercentage; got != 20 {
		t.Errorf("codex_primary contributor UsedPercentage=%v, want 20 (refreshed by sparse update)", got)
	}
}

// AC1 / stale-observation: when both an older and a newer 10,080-minute
// (weekly) observation are cached — the newer one having migrated to the
// `primary` storage slot while the older one lingers under `secondary` — only
// the newest weekly is displayed, even though the stale one reports a HIGHER
// used %. The session row is Unknown because no session-identity reading exists.
func TestCodexMetricsFromCache_DuplicateWeeklyKeepsNewest(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	older := now.Add(-time.Minute)

	// Older weekly under secondary at 70%.
	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowSecondary: {
			UsedPercentage: 70, ResetsAtMs: now.Add(7 * 24 * time.Hour).UnixMilli(),
			WindowMinutes: 10080, ObservedAtMs: older.UnixMilli(), usageKnown: true, resetKnown: true,
		},
	}, nil, older, "")
	// Newer weekly-band observation lands under primary at 30%.
	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary: {
			UsedPercentage: 30, ResetsAtMs: now.Add(7 * 24 * time.Hour).UnixMilli(),
			WindowMinutes: 10080, ObservedAtMs: now.UnixMilli(), usageKnown: true, resetKnown: true,
		},
	}, nil, now, "")

	metrics := codexMetricsFromCache(now, "")
	if len(metrics) != 2 {
		t.Fatalf("want 2 metrics, got %d", len(metrics))
	}
	session, weekly := metrics[0], metrics[1]
	if !session.Unknown || session.Kind != limitKindSession {
		t.Errorf("session row=%+v, want Unknown session (no 5-hour observation)", session)
	}
	if weekly.Kind != limitKindWeekly || weekly.Unknown {
		t.Errorf("weekly row=%+v, want observed weekly", weekly)
	}
	if weekly.Consumed == nil || *weekly.Consumed != 30 {
		t.Errorf("weekly Consumed=%v, want 30 (newest wins; stale 70%% discarded despite higher usage)", weekly.Consumed)
	}
	if weekly.Label != "Weekly quota" {
		t.Errorf("weekly label=%q, want Weekly quota", weekly.Label)
	}
}

// AC2 / AC6: valid 300-minute and 10,080-minute observations render as
// "5-hour session window" first and "Weekly quota" second, both known.
func TestCodexMetricsFromCache_SessionFirstThenWeekly(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary: {
			UsedPercentage: 22, ResetsAtMs: now.Add(3 * time.Hour).UnixMilli(),
			WindowMinutes: 300, usageKnown: true, resetKnown: true,
		},
		codexWindowSecondary: {
			UsedPercentage: 48, ResetsAtMs: now.Add(5 * 24 * time.Hour).UnixMilli(),
			WindowMinutes: 10080, usageKnown: true, resetKnown: true,
		},
	}, nil, now, "")

	metrics := codexMetricsFromCache(now, "")
	if len(metrics) != 2 {
		t.Fatalf("want 2 metrics, got %d", len(metrics))
	}
	if metrics[0].Kind != limitKindSession || metrics[0].Unknown || metrics[0].Label != "5-hour session window" {
		t.Errorf("metrics[0]=%+v, want known 5-hour session window first", metrics[0])
	}
	if metrics[1].Kind != limitKindWeekly || metrics[1].Unknown || metrics[1].Label != "Weekly quota" {
		t.Errorf("metrics[1]=%+v, want known Weekly quota second", metrics[1])
	}
}

// Swapped placement: a weekly-band reading physically under `primary` and a
// session-band reading under `secondary` must still emit session first / weekly
// second — identity, not storage slot, decides the row.
func TestCodexMetricsFromCache_SwappedSlotsPreserveClaudeOrder(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary: {
			UsedPercentage: 25, ResetsAtMs: now.Add(6 * 24 * time.Hour).UnixMilli(),
			WindowMinutes: 10080, usageKnown: true, resetKnown: true,
		},
		codexWindowSecondary: {
			UsedPercentage: 60, ResetsAtMs: now.Add(2 * time.Hour).UnixMilli(),
			WindowMinutes: 300, usageKnown: true, resetKnown: true,
		},
	}, nil, now, "")

	metrics := codexMetricsFromCache(now, "")
	if metrics[0].Kind != limitKindSession || metrics[0].Unknown {
		t.Errorf("metrics[0]=%+v, want known session from the secondary-slot 300-min reading", metrics[0])
	}
	if metrics[0].Consumed == nil || *metrics[0].Consumed != 60 {
		t.Errorf("session Consumed=%v, want 60 (session-band under secondary)", metrics[0].Consumed)
	}
	if metrics[1].Kind != limitKindWeekly || metrics[1].Unknown {
		t.Errorf("metrics[1]=%+v, want known weekly from the primary-slot 10080-min reading", metrics[1])
	}
	if metrics[1].Consumed == nil || *metrics[1].Consumed != 25 {
		t.Errorf("weekly Consumed=%v, want 25 (weekly-band under primary)", metrics[1].Consumed)
	}
}

// AC4: a missing 5-hour observation must never let a weekly value be mislabeled
// as a 5-hour/daily/shift quota. With only a weekly reading cached, row 0 stays
// an Unknown session and row 1 is the real weekly.
func TestCodexMetricsFromCache_MissingSessionLeavesWeeklyUnmangled(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowSecondary: {
			UsedPercentage: 40, ResetsAtMs: now.Add(4 * 24 * time.Hour).UnixMilli(),
			WindowMinutes: 10080, usageKnown: true, resetKnown: true,
		},
	}, nil, now, "")

	metrics := codexMetricsFromCache(now, "")
	if !metrics[0].Unknown || metrics[0].Kind != limitKindSession || metrics[0].Label != "5-hour session window" {
		t.Errorf("metrics[0]=%+v, want Unknown 5-hour session window", metrics[0])
	}
	if metrics[1].Kind != limitKindWeekly || metrics[1].Unknown {
		t.Errorf("metrics[1]=%+v, want known weekly", metrics[1])
	}
	if metrics[1].Label != "Weekly quota" {
		t.Errorf("weekly label=%q, want Weekly quota (never daily/shift/5-hour)", metrics[1].Label)
	}
}

// AC4 boundary: a positively weekly-band observation stored under `primary` is
// identity `weekly`; it must fill the weekly row, never the session row.
func TestCodexMetricsFromCache_WeeklyBandUnderPrimaryDoesNotFillSession(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary: {
			UsedPercentage: 50, ResetsAtMs: now.Add(6 * 24 * time.Hour).UnixMilli(),
			WindowMinutes: 10080, usageKnown: true, resetKnown: true,
		},
	}, nil, now, "")

	metrics := codexMetricsFromCache(now, "")
	if !metrics[0].Unknown || metrics[0].Kind != limitKindSession {
		t.Errorf("metrics[0]=%+v, want Unknown session (weekly-band must not be promoted to 5-hour)", metrics[0])
	}
	if metrics[1].Kind != limitKindWeekly || metrics[1].Unknown {
		t.Errorf("metrics[1]=%+v, want known weekly from the primary-slot weekly-band reading", metrics[1])
	}
	if metrics[1].Consumed == nil || *metrics[1].Consumed != 50 {
		t.Errorf("weekly Consumed=%v, want 50", metrics[1].Consumed)
	}
}

// Mainline Codex omit: a primary reading with usage but WindowMinutes==0 is a
// KNOWN session by slot-default identity (Codex legitimately omits
// window_minutes), keeping the "5-hour session window" default label.
func TestCodexMetricsFromCache_DurationlessPrimaryIsKnownSession(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	mergeCodexRateLimitCache(cache, map[string]codexRateLimitBucket{
		codexWindowPrimary: {
			UsedPercentage: 45, ResetsAtMs: now.Add(3 * time.Hour).UnixMilli(),
			usageKnown: true, resetKnown: true,
		},
	}, nil, now, "")

	metrics := codexMetricsFromCache(now, "")
	if metrics[0].Unknown || metrics[0].Kind != limitKindSession {
		t.Errorf("metrics[0]=%+v, want known session by slot-default", metrics[0])
	}
	if metrics[0].Label != "5-hour session window" {
		t.Errorf("session label=%q, want default 5-hour session window", metrics[0].Label)
	}
	if metrics[0].Consumed == nil || *metrics[0].Consumed != 45 {
		t.Errorf("session Consumed=%v, want 45", metrics[0].Consumed)
	}
}

// Multi-limit within one identity: two weekly metered limits (distinct limit
// ids) fold most-constrained — the higher usage wins — while freshness is the
// max across the weekly partition. Only one weekly row surfaces.
func TestCaptureCodexRateLimit_MultiLimitWeeklyMostConstrained(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	// No auth.json under CODEX_HOME → capture writes with an empty fingerprint,
	// matching the codexMetricsFromCache(now, "") reads below regardless of any
	// real Codex login on the dev machine.
	t.Setenv("CODEX_HOME", t.TempDir())
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	// Older, stricter weekly limit at 90%.
	captureCodexRateLimitLine(
		`{"jsonrpc":"2.0","method":"account/rateLimits/updated","params":{"rateLimitsByLimitId":{`+
			`"codex_weekly_a":{"secondary":{"usedPercent":90,"windowDurationMins":10080,"resetsInSeconds":604800}}`+
			`}}}`,
		now,
	)
	// Newer, looser weekly limit at 20%.
	captureCodexRateLimitLine(
		`{"jsonrpc":"2.0","method":"account/rateLimits/updated","params":{"rateLimitsByLimitId":{`+
			`"codex_weekly_b":{"secondary":{"usedPercent":20,"windowDurationMins":10080,"resetsInSeconds":604800}}`+
			`}}}`,
		now.Add(30*time.Second),
	)

	metrics := codexMetricsFromCache(now, "")
	if metrics[1].Kind != limitKindWeekly || metrics[1].Unknown {
		t.Fatalf("metrics[1]=%+v, want known weekly", metrics[1])
	}
	if metrics[1].Consumed == nil || *metrics[1].Consumed != 90 {
		t.Errorf("weekly Consumed=%v, want 90 (most-constrained across the two weekly limits)", metrics[1].Consumed)
	}
	if !metrics[0].Unknown {
		t.Errorf("session row=%+v, want Unknown (no session-identity reading)", metrics[0])
	}
}

// AC3: a sparse frame that reports a weekly reading under a different provider
// slot must replace the stale reading while retaining the canonical cache slot.
func TestCaptureCodexRateLimit_SameIdentitySupersessionAcrossSlots(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	t.Setenv("CODEX_HOME", t.TempDir())
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	// Seed a weekly reading under secondary.
	captureCodexRateLimitLine(
		`{"method":"token_count","params":{"rate_limits":{"secondary":{"used_percent":40,"window_minutes":10080,"resets_in_seconds":604800}}}}`,
		now,
	)
	// A newer weekly-band reading arrives under primary (bucket migration).
	captureCodexRateLimitLine(
		`{"method":"account/rateLimits/updated","params":{"rateLimits":{"primary":{"used_percent":30,"window_minutes":10080,"resets_in_seconds":604800}}}}`,
		now.Add(30*time.Second),
	)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache")
	}
	if weekly, present := snap.Contributors[codexWindowSecondary][codexLegacyLimitID]; !present || weekly.UsedPercentage != 30 {
		t.Errorf("canonical weekly contributor=%+v, present=%v; want refreshed 30%%", weekly, present)
	}
	if contribs := snap.Contributors[codexWindowPrimary]; len(contribs) > 0 {
		t.Errorf("provider primary placement must be normalized away: %+v", contribs)
	}
	metrics := codexMetricsFromCache(now.Add(30*time.Second), "")
	if metrics[1].Kind != limitKindWeekly || metrics[1].Unknown {
		t.Fatalf("metrics[1]=%+v, want single known weekly", metrics[1])
	}
	if metrics[1].Consumed == nil || *metrics[1].Consumed != 30 {
		t.Errorf("weekly Consumed=%v, want 30 (migrated primary weekly wins)", metrics[1].Consumed)
	}
}

// AC3: a full snapshot that OMITS a previously-cached window (no key at all,
// not merely null) drops that window — the complete picture is authoritative.
func TestCaptureCodexRateLimit_FullSnapshotOmissionDropsWindow(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	t.Setenv("CODEX_HOME", t.TempDir())
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	// Seed a weekly reading via a sparse notification.
	captureCodexRateLimitLine(
		`{"method":"token_count","params":{"rate_limits":{"secondary":{"used_percent":40,"resets_in_seconds":604800}}}}`,
		now,
	)
	// Full account/rateLimits/read response that mentions ONLY primary — the
	// secondary key is entirely absent, so the weekly window is gone.
	captureCodexRateLimitLine(
		`{"jsonrpc":"2.0","id":9,"result":{"rateLimits":{"primary":{"usedPercent":33,"resetsInSeconds":1800}}}}`,
		now.Add(time.Minute),
	)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache")
	}
	if _, present := snap.Buckets[codexWindowSecondary]; present {
		t.Errorf("weekly must be dropped when a full snapshot omits it: %+v", snap.Buckets)
	}
	metrics := codexMetricsFromCache(now.Add(time.Minute), "")
	if !metrics[1].Unknown {
		t.Errorf("weekly row=%+v, want Unknown after full-snapshot omission", metrics[1])
	}
	if metrics[0].Unknown || metrics[0].Kind != limitKindSession {
		t.Errorf("session row=%+v, want known session from the full snapshot", metrics[0])
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

// Finding 2 (cross-shape migration): a weekly first observed via the aggregate
// `rate_limits` view (limit id __legacy__ under secondary) then re-reported via
// the per-limit `rateLimitsByLimitId` view under the primary provider slot (a
// NAMED limit id) is the SAME weekly metric. The stale aggregate is dropped and
// the newest named reading wins in the canonical weekly cache slot — even though
// it reports a LOWER used %. Keying only on (identity, limit id) without the
// legacy-to-named supersession rule would keep both and resurrect the stale high %.
func TestCaptureCodexRateLimit_CrossShapeWeeklyMigrationNewestWins(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	t.Setenv("CODEX_HOME", t.TempDir())
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	// Older weekly via the aggregate view (secondary / __legacy__) at 70%.
	captureCodexRateLimitLine(
		`{"method":"token_count","params":{"rate_limits":{"secondary":{"used_percent":70,"window_minutes":10080,"resets_in_seconds":604800}}}}`,
		now,
	)
	// Newer weekly via the per-limit view, nested under the primary slot as a
	// NAMED limit at a LOWER 30%.
	captureCodexRateLimitLine(
		`{"method":"account/rateLimits/updated","params":{"rateLimitsByLimitId":{`+
			`"codex_secondary":{"primary":{"usedPercent":30,"windowDurationMins":10080,"resetsInSeconds":604800}}`+
			`}}}`,
		now.Add(30*time.Second),
	)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache")
	}
	weeklyContribs := snap.Contributors[codexWindowSecondary]
	if _, present := weeklyContribs[codexLegacyLimitID]; present {
		t.Errorf("stale aggregate weekly must be dropped, still present: %+v", weeklyContribs)
	}
	if named, present := weeklyContribs["codex_secondary"]; !present || named.UsedPercentage != 30 {
		t.Errorf("named weekly contributor=%+v, present=%v; want refreshed 30%%", named, present)
	}
	if contribs := snap.Contributors[codexWindowPrimary]; len(contribs) > 0 {
		t.Errorf("provider primary placement must be normalized away: %+v", contribs)
	}
	metrics := codexMetricsFromCache(now.Add(30*time.Second), "")
	if metrics[1].Kind != limitKindWeekly || metrics[1].Unknown {
		t.Fatalf("metrics[1]=%+v, want single known weekly", metrics[1])
	}
	if metrics[1].Consumed == nil || *metrics[1].Consumed != 30 {
		t.Errorf("weekly Consumed=%v, want 30 (newest wins across payload shapes despite lower usage)", metrics[1].Consumed)
	}
	if !metrics[0].Unknown {
		t.Errorf("session row=%+v, want Unknown (no session reading)", metrics[0])
	}
}

// Finding 3 (empty full snapshot): a full account/rateLimits/read response whose
// `rateLimits` object is empty declares the account now has NO quota windows, so
// every cached observation must be reconciled away — the frame must not be
// dropped just because it carries no buckets and no explicit nulls.
func TestCaptureCodexRateLimit_EmptyFullSnapshotClearsAll(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	t.Setenv("CODEX_HOME", t.TempDir())
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	captureCodexRateLimitLine(
		`{"method":"token_count","params":{"rate_limits":{`+
			`"primary":{"used_percent":30,"window_minutes":300,"resets_in_seconds":1800},`+
			`"secondary":{"used_percent":40,"window_minutes":10080,"resets_in_seconds":604800}}}}`,
		now,
	)
	if snap, ok := loadCodexRateLimitSnapshot(cache); !ok || len(snap.Buckets) != 2 {
		t.Fatalf("seed should cache two windows, got ok=%v %+v", ok, snap.Buckets)
	}

	captureCodexRateLimitLine(
		`{"jsonrpc":"2.0","id":7,"result":{"rateLimits":{}}}`,
		now.Add(time.Minute),
	)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache")
	}
	if len(snap.Buckets) != 0 || len(snap.Contributors) != 0 {
		t.Errorf("empty full snapshot must clear all cached windows, got Buckets=%+v Contributors=%+v", snap.Buckets, snap.Contributors)
	}
	metrics := codexMetricsFromCache(now.Add(time.Minute), "")
	if !metrics[0].Unknown || !metrics[1].Unknown {
		t.Errorf("both rows should be Unknown after empty full snapshot, got [%+v %+v]", metrics[0], metrics[1])
	}
}

// Finding 4 (identity-level omission): when one storage slot holds contributors
// for TWO identities — a live session and a stale weekly that migrated into the
// primary slot — a full snapshot that restates only the session must drop the
// omitted weekly identity, not preserve the whole slot. Slot-level omission would
// keep the stale weekly because the primary slot itself survives.
func TestCaptureCodexRateLimit_FullSnapshotOmitsIdentityWithinSharedSlot(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	t.Setenv("CODEX_HOME", t.TempDir())
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	// A weekly-band reading migrated into the primary slot (named limit id).
	captureCodexRateLimitLine(
		`{"method":"account/rateLimits/updated","params":{"rateLimitsByLimitId":{`+
			`"codex_secondary":{"primary":{"usedPercent":50,"windowDurationMins":10080,"resetsInSeconds":604800}}`+
			`}}}`,
		now,
	)
	// A session reading also under the primary slot (distinct named limit id), so
	// the primary slot now holds two identities.
	captureCodexRateLimitLine(
		`{"method":"account/rateLimits/updated","params":{"rateLimitsByLimitId":{`+
			`"codex_primary":{"primary":{"usedPercent":30,"windowDurationMins":300,"resetsInSeconds":1800}}`+
			`}}}`,
		now.Add(30*time.Second),
	)
	// Sanity: before the full snapshot the stale weekly is visible.
	if pre := codexMetricsFromCache(now.Add(30*time.Second), ""); pre[1].Unknown {
		t.Fatalf("precondition: weekly should be visible before the full snapshot, got %+v", pre[1])
	}

	// Full snapshot restates ONLY the session window; the weekly identity is
	// omitted entirely.
	captureCodexRateLimitLine(
		`{"jsonrpc":"2.0","id":5,"result":{"rateLimits":{"primary":{"usedPercent":33,"windowDurationMins":300,"resetsInSeconds":1800}}}}`,
		now.Add(time.Minute),
	)

	metrics := codexMetricsFromCache(now.Add(time.Minute), "")
	if metrics[0].Unknown || metrics[0].Kind != limitKindSession {
		t.Errorf("session row=%+v, want known session retained by the full snapshot", metrics[0])
	}
	if !metrics[1].Unknown {
		t.Errorf("weekly row=%+v, want Unknown — the stale weekly identity sharing the primary slot must be reconciled away", metrics[1])
	}
}

// Finding (empty-full-snapshot precision): a full account/rateLimits/read whose
// container is NON-empty but yields no recognised windows — unknown window keys,
// or an unparseable/empty bucket object like primary:{} — is NOT authoritative
// "clear all". Such forward-compatible or partial reads must preserve prior live
// observations, exactly as the old early-return did. Only a literal {} clears.
func TestCaptureCodexRateLimit_FullSnapshotUnknownKeysPreserveCache(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	t.Setenv("CODEX_HOME", t.TempDir())
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	// Seed live session + weekly observations.
	captureCodexRateLimitLine(
		`{"method":"token_count","params":{"rate_limits":{`+
			`"primary":{"used_percent":30,"window_minutes":300,"resets_in_seconds":1800},`+
			`"secondary":{"used_percent":40,"window_minutes":10080,"resets_in_seconds":604800}}}}`,
		now,
	)
	if snap, ok := loadCodexRateLimitSnapshot(cache); !ok || len(snap.Buckets) != 2 {
		t.Fatalf("seed should cache two windows, got ok=%v %+v", ok, snap.Buckets)
	}

	// Full read carrying only an UNKNOWN window key — nothing we recognise, and
	// not a literal {}. Must be a no-op, not a cache wipe.
	captureCodexRateLimitLine(
		`{"jsonrpc":"2.0","id":8,"result":{"rateLimits":{"tertiary":{"used_percent":5,"window_minutes":60,"resets_in_seconds":3600}}}}`,
		now.Add(time.Minute),
	)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache")
	}
	if p, present := snap.Buckets[codexWindowPrimary]; !present || p.UsedPercentage != 30 {
		t.Errorf("primary must survive a full read of only unknown keys: %+v", snap.Buckets)
	}
	if s, present := snap.Buckets[codexWindowSecondary]; !present || s.UsedPercentage != 40 {
		t.Errorf("secondary must survive a full read of only unknown keys: %+v", snap.Buckets)
	}
}

// Companion to the above: a full read whose recognised window carries an
// unparseable bucket object (primary:{} — no usage, no reset) extracts nothing,
// so it is not authoritative-empty and must preserve the prior cache.
func TestCaptureCodexRateLimit_FullSnapshotUnparseableBucketPreservesCache(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	t.Setenv("CODEX_HOME", t.TempDir())
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	captureCodexRateLimitLine(
		`{"method":"token_count","params":{"rate_limits":{`+
			`"primary":{"used_percent":30,"window_minutes":300,"resets_in_seconds":1800},`+
			`"secondary":{"used_percent":40,"window_minutes":10080,"resets_in_seconds":604800}}}}`,
		now,
	)

	// Full read with an empty bucket object for a recognised window: codexBucketFromInfo
	// rejects it (no usage/reset), so extraction yields nothing.
	captureCodexRateLimitLine(
		`{"jsonrpc":"2.0","id":9,"result":{"rateLimits":{"primary":{}}}}`,
		now.Add(time.Minute),
	)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache")
	}
	if p, present := snap.Buckets[codexWindowPrimary]; !present || p.UsedPercentage != 30 {
		t.Errorf("primary must survive a full read carrying only an unparseable bucket: %+v", snap.Buckets)
	}
	if s, present := snap.Buckets[codexWindowSecondary]; !present || s.UsedPercentage != 40 {
		t.Errorf("secondary must survive a full read carrying only an unparseable bucket: %+v", snap.Buckets)
	}
	metrics := codexMetricsFromCache(now.Add(time.Minute), "")
	if metrics[0].Unknown || metrics[1].Unknown {
		t.Errorf("both rows should remain known after a non-authoritative full read, got [%+v %+v]", metrics[0], metrics[1])
	}
}

// Finding (sparse-safe concurrent limits): two weekly metered limits A@90% and
// B@20% are cached under secondary; a later sparse frame restates ONLY B under
// the primary provider slot at 30%. B is refreshed in the canonical weekly
// cache slot, but A — which the sparse frame never mentioned — must NOT be
// retracted. Weekly must still display 90% (A most-constrains), not 30%.
func TestCaptureCodexRateLimit_SparseMigrationKeepsUnmentionedConcurrentLimit(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	t.Setenv("CODEX_HOME", t.TempDir())
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	// Seed weekly limits A@90 and B@20, both under secondary.
	captureCodexRateLimitLine(
		`{"method":"account/rateLimits/updated","params":{"rateLimitsByLimitId":{`+
			`"codex_weekly_a":{"secondary":{"usedPercent":90,"windowDurationMins":10080,"resetsInSeconds":604800}},`+
			`"codex_weekly_b":{"secondary":{"usedPercent":20,"windowDurationMins":10080,"resetsInSeconds":604800}}`+
			`}}}`,
		now,
	)
	// Sparse frame restates ONLY B, now under the primary slot at 30%.
	captureCodexRateLimitLine(
		`{"method":"account/rateLimits/updated","params":{"rateLimitsByLimitId":{`+
			`"codex_weekly_b":{"primary":{"usedPercent":30,"windowDurationMins":10080,"resetsInSeconds":604800}}`+
			`}}}`,
		now.Add(30*time.Second),
	)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache")
	}
	// A survives under secondary (never mentioned by the sparse frame).
	if a, present := snap.Contributors[codexWindowSecondary]["codex_weekly_a"]; !present || a.UsedPercentage != 90 {
		t.Errorf("unmentioned concurrent limit A must survive under secondary: %+v", snap.Contributors[codexWindowSecondary])
	}
	// B is refreshed in the canonical weekly slot; no physical primary copy remains.
	if b, present := snap.Contributors[codexWindowSecondary]["codex_weekly_b"]; !present || b.UsedPercentage != 30 {
		t.Errorf("migrated limit B must refresh under secondary at 30%%: %+v", snap.Contributors[codexWindowSecondary])
	}
	if contribs := snap.Contributors[codexWindowPrimary]; len(contribs) > 0 {
		t.Errorf("provider primary placement must be normalized away: %+v", contribs)
	}

	metrics := codexMetricsFromCache(now.Add(30*time.Second), "")
	if metrics[1].Kind != limitKindWeekly || metrics[1].Unknown {
		t.Fatalf("metrics[1]=%+v, want known weekly", metrics[1])
	}
	if metrics[1].Consumed == nil || *metrics[1].Consumed != 90 {
		t.Errorf("weekly Consumed=%v, want 90 (unmentioned A still most-constrains after B migrates)", metrics[1].Consumed)
	}
}

// Finding (full-snapshot omits a concurrent limit of a surviving identity): two
// weekly metered limits A@90% and B@20% are cached under secondary; a later FULL
// account/rateLimits/read restates ONLY B at 30%. The `weekly` identity survives
// via B, but A — which the authoritative snapshot omits — must be dropped, not
// shielded by the identity surviving. Identity-only omission keying would keep A
// and let most-constrained folding still display 90%.
func TestCaptureCodexRateLimit_FullSnapshotOmitsConcurrentLimitOfSurvivingIdentity(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	t.Setenv("CODEX_HOME", t.TempDir())
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	// Seed weekly limits A@90 and B@20, both under secondary, via a sparse frame.
	captureCodexRateLimitLine(
		`{"method":"account/rateLimits/updated","params":{"rateLimitsByLimitId":{`+
			`"codex_weekly_a":{"secondary":{"usedPercent":90,"windowDurationMins":10080,"resetsInSeconds":604800}},`+
			`"codex_weekly_b":{"secondary":{"usedPercent":20,"windowDurationMins":10080,"resetsInSeconds":604800}}`+
			`}}}`,
		now,
	)
	// Sanity: the aggregate most-constrains to A's 90%.
	if pre := codexMetricsFromCache(now, ""); pre[1].Consumed == nil || *pre[1].Consumed != 90 {
		t.Fatalf("precondition: weekly should most-constrain to 90, got %+v", pre[1])
	}

	// FULL account/rateLimits/read restating ONLY weekly limit B at 30%. A is
	// authoritatively omitted and must be reconciled away.
	captureCodexRateLimitLine(
		`{"jsonrpc":"2.0","id":11,"result":{"rateLimitsByLimitId":{`+
			`"codex_weekly_b":{"secondary":{"usedPercent":30,"windowDurationMins":10080,"resetsInSeconds":604800}}`+
			`}}}`,
		now.Add(time.Minute),
	)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache")
	}
	if _, present := snap.Contributors[codexWindowSecondary]["codex_weekly_a"]; present {
		t.Errorf("omitted concurrent limit A must be dropped by the full snapshot: %+v", snap.Contributors[codexWindowSecondary])
	}
	if b, present := snap.Contributors[codexWindowSecondary]["codex_weekly_b"]; !present || b.UsedPercentage != 30 {
		t.Errorf("restated limit B must survive at 30%%: %+v", snap.Contributors[codexWindowSecondary])
	}
	metrics := codexMetricsFromCache(now.Add(time.Minute), "")
	if metrics[1].Kind != limitKindWeekly || metrics[1].Unknown {
		t.Fatalf("metrics[1]=%+v, want known weekly", metrics[1])
	}
	if metrics[1].Consumed == nil || *metrics[1].Consumed != 30 {
		t.Errorf("weekly Consumed=%v, want 30 (stale A dropped by authoritative full snapshot)", metrics[1].Consumed)
	}
}

// Finding (dual-container authoritative-empty): a full read with an empty
// `rateLimits:{}` alongside a NON-empty `rateLimitsByLimitId` that recognises
// nothing (unknown limit / non-window nesting) carried real content and must NOT
// be treated as authoritative-empty — it is a no-op that preserves prior cache.
func TestCaptureCodexRateLimit_DualContainerEmptyPlusUnrecognizedPreservesCache(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	t.Setenv("CODEX_HOME", t.TempDir())
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	captureCodexRateLimitLine(
		`{"method":"token_count","params":{"rate_limits":{`+
			`"primary":{"used_percent":30,"window_minutes":300,"resets_in_seconds":1800},`+
			`"secondary":{"used_percent":40,"window_minutes":10080,"resets_in_seconds":604800}}}}`,
		now,
	)
	if snap, ok := loadCodexRateLimitSnapshot(cache); !ok || len(snap.Buckets) != 2 {
		t.Fatalf("seed should cache two windows, got ok=%v %+v", ok, snap.Buckets)
	}

	// Empty rateLimits container, but a non-empty rateLimitsByLimitId whose only
	// entry recognises no window — the frame is NOT a clear-all.
	captureCodexRateLimitLine(
		`{"jsonrpc":"2.0","id":11,"result":{"rateLimits":{},"rateLimitsByLimitId":{`+
			`"mystery_limit":{"tertiary":{"usedPercent":5,"windowDurationMins":60}}`+
			`}}}`,
		now.Add(time.Minute),
	)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache")
	}
	if p, present := snap.Buckets[codexWindowPrimary]; !present || p.UsedPercentage != 30 {
		t.Errorf("primary must survive dual-container non-authoritative read: %+v", snap.Buckets)
	}
	if s, present := snap.Buckets[codexWindowSecondary]; !present || s.UsedPercentage != 40 {
		t.Errorf("secondary must survive dual-container non-authoritative read: %+v", snap.Buckets)
	}
}

// A `token_count` typed event can ride inside a JSON-RPC `result` envelope as
// `result.msg`. Unlike the authoritative `account/rateLimits/read` response
// (whose `rate_limits` sit DIRECTLY under `result`), a `result.msg` payload is an
// inherently SPARSE typed event — it only restates the window(s) it just
// observed. It must NOT be treated as a full snapshot and prune windows it
// omitted: a `result.msg` restating only `primary` must PRESERVE a live cached
// weekly.
func TestCaptureCodexRateLimit_ResultMsgIsSparseNotFullSnapshot(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	t.Setenv("CODEX_HOME", t.TempDir())
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	// Seed a weekly reading.
	captureCodexRateLimitLine(
		`{"method":"token_count","params":{"rate_limits":{"secondary":{"used_percent":40,"window_minutes":10080,"resets_in_seconds":604800}}}}`,
		now,
	)
	// A token_count typed event wrapped under result.msg, restating ONLY primary.
	// Before the fix this inherited fullSnapshot=true and the omission reconcile
	// dropped the weekly it didn't mention.
	captureCodexRateLimitLine(
		`{"jsonrpc":"2.0","id":4,"result":{"msg":{"type":"token_count","rate_limits":{"primary":{"used_percent":33,"window_minutes":300,"resets_in_seconds":1800}}}}}`,
		now.Add(time.Minute),
	)

	metrics := codexMetricsFromCache(now.Add(time.Minute), "")
	if metrics[1].Unknown || metrics[1].Kind != limitKindWeekly {
		t.Errorf("weekly row=%+v, want preserved known weekly (result.msg is sparse, must not prune)", metrics[1])
	}
	if metrics[1].Consumed == nil || *metrics[1].Consumed != 40 {
		t.Errorf("weekly Consumed=%v, want 40 preserved", metrics[1].Consumed)
	}
	if metrics[0].Unknown || metrics[0].Kind != limitKindSession {
		t.Errorf("session row=%+v, want known session from result.msg", metrics[0])
	}
}

func TestCaptureCodexRateLimit_EqualPercentageAdvancesManagedObservation(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	first := time.Date(2026, 8, 26, 13, 47, 8, 0, time.UTC)
	second := first.Add(90 * time.Second)
	line := `{"type":"token_count","rate_limits":{"primary":{"used_percent":42,"window_minutes":300,"resets_in_seconds":3600}}}`

	captureCodexRateLimitLine(line, first)
	captureCodexRateLimitLine(line, second)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatal("expected managed capture cache")
	}
	if got := snap.Buckets[codexWindowPrimary].ObservedAtMs; got != second.UnixMilli() {
		t.Fatalf("ObservedAtMs=%d, want repeated numeric evidence at %d", got, second.UnixMilli())
	}
}

func TestCodexObservationTimes_FutureToleranceAndClamp(t *testing.T) {
	now := time.Date(2026, 8, 26, 14, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		at   time.Time
		zero bool
	}{
		{"boundary accepted and clamped", now.Add(5 * time.Minute), false},
		{"beyond boundary rejected", now.Add(5*time.Minute + time.Nanosecond), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			anchor, got := codexObservationTimes(map[string]interface{}{
				"timestamp": tc.at.Format(time.RFC3339Nano),
			}, now, false)
			if tc.zero {
				if !anchor.IsZero() || !got.IsZero() {
					t.Fatalf("anchor=%s observed=%s, want rejection", anchor, got)
				}
				return
			}
			if !anchor.Equal(tc.at) {
				t.Fatalf("anchor=%s, want provider reset anchor %s", anchor, tc.at)
			}
			if !got.Equal(now) {
				t.Fatalf("got %s, want publication clamp %s", got, now)
			}
		})
	}
}

func TestCodexBucketsFromRolloutFile_UntimestampedNumericLineDoesNotBlockLaterValidLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	missing := `{"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":91,"window_minutes":300}}}}`
	valid := `{"timestamp":"2026-08-26T14:01:02.123456789Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":17,"window_minutes":300}}}}`
	if err := os.WriteFile(path, []byte(missing+"\n"+valid+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	buckets, _, _, handled, ok := codexBucketsFromRolloutFile(context.Background(), path, time.Date(2026, 8, 26, 15, 0, 0, 0, time.UTC))
	if !handled || !ok {
		t.Fatalf("handled=%v ok=%v, want valid later object", handled, ok)
	}
	b := buckets[codexWindowPrimary][codexLegacyLimitID]
	if b.UsedPercentage != 17 || b.ObservedAtMs != time.Date(2026, 8, 26, 14, 1, 2, 123456789, time.UTC).UnixMilli() {
		t.Fatalf("bucket=%+v, want only timestamped 17%% evidence", b)
	}
}

func TestCodexRolloutOpenFailureHandled_RetriesTransientErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"vanished file", &os.PathError{Op: "open", Path: "rollout.jsonl", Err: os.ErrNotExist}, true},
		{"temporary permission failure", &os.PathError{Op: "open", Path: "rollout.jsonl", Err: os.ErrPermission}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexRolloutOpenFailureHandled(tc.err); got != tc.want {
				t.Fatalf("handled=%v, want %v for %v", got, tc.want, tc.err)
			}
		})
	}
}

func TestCodexRateLimitSnapshot_RedactsRawEnvelopeFields(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	lineMap := map[string]interface{}{
		"type": "token_count",
		"rate_limits": map[string]interface{}{
			"primary": map[string]interface{}{"used_percent": 28.0, "window_minutes": 300.0},
		},
		"access_token": "credential-sentinel",
		"raw_config":   "config-sentinel",
		"source_path":  "/secret/rollout.jsonl",
		"prompt":       "prompt-sentinel",
		"response":     "response-sentinel",
		"unknown":      "unknown-sentinel",
	}
	raw, err := json.Marshal(lineMap)
	if err != nil {
		t.Fatal(err)
	}
	captureCodexRateLimitLine(string(raw), time.Date(2026, 8, 26, 14, 0, 0, 0, time.UTC))
	persisted, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	for _, sentinel := range []string{"credential-sentinel", "config-sentinel", "/secret/rollout.jsonl", "prompt-sentinel", "response-sentinel", "unknown-sentinel"} {
		if strings.Contains(string(persisted), sentinel) {
			t.Errorf("typed cache leaked %q: %s", sentinel, persisted)
		}
	}
	if !strings.Contains(string(persisted), `"usedPercentage": 28`) {
		t.Fatalf("normalized metric missing from cache: %s", persisted)
	}
}

func TestCaptureCodexRateLimit_UnrecognizedEnvelopeCannotOverwriteCache(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	first := time.Date(2026, 8, 26, 14, 0, 0, 0, time.UTC)
	captureCodexRateLimitLine(
		`{"type":"token_count","rate_limits":{"primary":{"used_percent":21,"window_minutes":300}}}`,
		first,
	)
	captureCodexRateLimitLine(
		`{"type":"tool_output","params":{"rateLimits":{"primary":{"usedPercent":99,"windowDurationMins":300}}}}`,
		first.Add(time.Minute),
	)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatal("expected seeded cache")
	}
	b := snap.Buckets[codexWindowPrimary]
	if b.UsedPercentage != 21 || b.ObservedAtMs != first.UnixMilli() {
		t.Fatalf("bucket=%+v, arbitrary params.rateLimits must fail closed", b)
	}
}

func TestCaptureCodexRateLimit_FutureEventAnchorsResetButClampsObservation(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	now := time.Date(2026, 8, 26, 14, 0, 0, 0, time.UTC)
	providerAt := now.Add(5 * time.Minute)
	line := fmt.Sprintf(
		`{"timestamp":%q,"type":"token_count","rate_limits":{"primary":{"used_percent":32,"resets_in_seconds":60}}}`,
		providerAt.Format(time.RFC3339Nano),
	)
	captureCodexRateLimitLine(line, now)

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatal("expected cache")
	}
	b := snap.Buckets[codexWindowPrimary]
	if b.ObservedAtMs != now.UnixMilli() {
		t.Fatalf("ObservedAtMs=%d, want receive-time clamp %d", b.ObservedAtMs, now.UnixMilli())
	}
	if b.ResetsAtMs != providerAt.Add(time.Minute).UnixMilli() {
		t.Fatalf("ResetsAtMs=%d, want provider anchor %d", b.ResetsAtMs, providerAt.Add(time.Minute).UnixMilli())
	}
}

func TestCodexBucketsFromRolloutFile_SkipsOversizedNonMetricLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	first := `{"timestamp":"2026-08-26T14:00:00Z","type":"token_count","rate_limits":{"primary":{"used_percent":17,"window_minutes":300}}}`
	later := `{"timestamp":"2026-08-26T14:10:00Z","type":"token_count","rate_limits":{"primary":{"used_percent":29,"window_minutes":300}}}`
	if _, err = fmt.Fprintln(f, first); err == nil {
		_, err = fmt.Fprintln(f, strings.Repeat("x", codexAppServerMaxLineSize+1))
	}
	if err == nil {
		_, err = fmt.Fprint(f, later) // final complete token intentionally has no newline
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 8, 26, 15, 0, 0, 0, time.UTC)
	interruptCtx := &cancelAfterChecksContext{after: 3, done: make(chan struct{})}
	partial, _, _, handled, ok := codexBucketsFromRolloutFile(interruptCtx, path, now)
	if handled || !ok {
		t.Fatalf("interrupted handled=%v ok=%v, want persisted complete evidence with retry required", handled, ok)
	}
	if b := partial[codexWindowPrimary][codexLegacyLimitID]; b.UsedPercentage != 29 {
		t.Fatalf("partial bucket=%+v, want newest complete tail object despite forward interruption", b)
	}

	buckets, _, _, handled, ok := codexBucketsFromRolloutFile(context.Background(), path, now)
	if !handled || !ok {
		t.Fatalf("handled=%v ok=%v, want complete scan with numeric evidence", handled, ok)
	}
	b := buckets[codexWindowPrimary][codexLegacyLimitID]
	if b.UsedPercentage != 29 || b.ObservedAtMs != time.Date(2026, 8, 26, 14, 10, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("bucket=%+v, want later numeric object after oversized line", b)
	}
}

func TestCodexRecentRolloutLines_ContinuesPastSparseNumericFrame(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	weekly := `{"timestamp":"2026-08-26T14:00:00Z","type":"token_count","rate_limits":{"secondary":{"used_percent":41,"window_minutes":10080}}}`
	primary := `{"timestamp":"2026-08-26T14:10:00Z","type":"token_count","rate_limits":{"primary":{"used_percent":29,"window_minutes":300}}}`
	padding := `{"padding":"` + strings.Repeat("x", codexRolloutTailReadChunkSize*2) + `"}`
	if err := os.WriteFile(path, []byte(weekly+"\n"+padding+"\n"+primary), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}

	lines := codexRecentRolloutLines(context.Background(), f, info.Size(), time.Date(2026, 8, 26, 15, 0, 0, 0, time.UTC))
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, `"used_percent":29`) {
		t.Fatal("tail probe did not retain the newest sparse session frame")
	}
	if !strings.Contains(joined, `"used_percent":41`) {
		t.Fatal("tail probe stopped before the older sparse weekly frame")
	}
}

func TestCodexRolloutFallbackBuckets_StatFailurePreventsHighWaterAdvance(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "sessions", "2026", "08", "26")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	validPath := filepath.Join(dir, "rollout-valid.jsonl")
	valid := `{"timestamp":"2026-08-26T14:00:00Z","type":"token_count","rate_limits":{"primary":{"used_percent":17,"window_minutes":300}}}`
	if err := os.WriteFile(validPath, []byte(valid+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "missing-target"), filepath.Join(dir, "rollout-broken.jsonl")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	_, _, _, highWater, ok := codexRolloutFallbackBuckets(
		context.Background(), base, time.Date(2026, 8, 26, 15, 0, 0, 0, time.UTC), codexRolloutScanCursor{},
	)
	if !ok {
		t.Fatal("expected valid sibling rollout evidence")
	}
	if highWater != nil {
		t.Fatalf("highWater=%+v, want unchanged after incomplete metadata discovery", *highWater)
	}
}

func TestCodexDiscoverRolloutCandidates_StopsWhenContextCancels(t *testing.T) {
	base := t.TempDir()
	for _, day := range []string{"24", "25", "26"} {
		dir := filepath.Join(base, "sessions", "2026", "08", day)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "rollout-test.jsonl"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	ctx := &cancelAfterChecksContext{after: 5, done: make(chan struct{})}
	_, complete := codexDiscoverRolloutCandidates(ctx, base, codexRolloutScanCursor{})
	if complete {
		t.Fatal("discovery complete=true, want cancellation to stop traversal and preserve the high-water")
	}
	if ctx.checks > 8 {
		t.Fatalf("context checked %d times, want traversal to stop promptly after cancellation", ctx.checks)
	}
}

func TestCodexReadDirContext_ChecksCancellationBetweenChunks(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < codexRolloutReadDirChunkSize*2; i++ {
		path := filepath.Join(dir, fmt.Sprintf("rollout-%04d.jsonl", i))
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	ctx := &cancelAfterChecksContext{after: 2, done: make(chan struct{})}
	entries, err := codexReadDirContext(ctx, dir)
	if err != context.Canceled {
		t.Fatalf("err=%v, want context cancellation between directory chunks", err)
	}
	if len(entries) != codexRolloutReadDirChunkSize {
		t.Fatalf("entries=%d, want one %d-entry chunk before cancellation", len(entries), codexRolloutReadDirChunkSize)
	}
}

func TestCodexRolloutDiscoveryContext_ReservesCandidateReadBudget(t *testing.T) {
	for _, tc := range []struct {
		name      string
		budget    time.Duration
		wantGap   time.Duration
		tolerance time.Duration
	}{
		{name: "normal scan", budget: 5 * time.Second, wantGap: time.Second, tolerance: 100 * time.Millisecond},
		{name: "short scan", budget: 500 * time.Millisecond, wantGap: 250 * time.Millisecond, tolerance: 50 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, cancelParent := context.WithTimeout(context.Background(), tc.budget)
			defer cancelParent()
			parentDeadline, _ := parent.Deadline()

			discovery, cancelDiscovery := codexRolloutDiscoveryContext(parent)
			defer cancelDiscovery()
			discoveryDeadline, ok := discovery.Deadline()
			if !ok {
				t.Fatal("discovery context has no deadline")
			}
			gap := parentDeadline.Sub(discoveryDeadline)
			if gap < tc.wantGap-tc.tolerance || gap > tc.wantGap+tc.tolerance {
				t.Fatalf("candidate-read reserve=%v, want %v (+/-%v)", gap, tc.wantGap, tc.tolerance)
			}
		})
	}
}

func TestCodexDiscoverRolloutCandidates_VisitsNewestDateBeforeCancellation(t *testing.T) {
	base := t.TempDir()
	var newestPath string
	for _, fixture := range []struct {
		year, month, day string
	}{
		{year: "2024", month: "01", day: "01"},
		{year: "2025", month: "12", day: "31"},
		{year: "2026", month: "08", day: "26"},
	} {
		dir := filepath.Join(base, "sessions", fixture.year, fixture.month, fixture.day)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "rollout-test.jsonl")
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if fixture.year == "2026" {
			newestPath = path
		}
	}

	// Allow discovery to reach one rollout, then cancel before it can descend
	// into the next date. Restarting an interrupted scan must always prioritize
	// the newest direct-run evidence rather than replaying the oldest history.
	ctx := &cancelAfterChecksContext{after: 17, done: make(chan struct{})}
	candidates, complete := codexDiscoverRolloutCandidates(ctx, base, codexRolloutScanCursor{})
	if complete {
		t.Fatal("discovery complete=true, want cancellation after newest candidate")
	}
	if len(candidates) != 1 || candidates[0].path != newestPath {
		t.Fatalf("candidates=%+v, want only newest date %q before cancellation", candidates, newestPath)
	}
}

func TestCodexDiscoverRolloutCandidates_RetainsSameMillisecondUpdate(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "sessions", "2026", "08", "26")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-test.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 8, 26, 14, 0, 0, 100_000, time.UTC)
	second := first.Add(700 * time.Microsecond)
	if first.UnixMilli() != second.UnixMilli() {
		t.Fatal("test fixture must remain within one millisecond")
	}
	if err := os.Chtimes(path, second, second); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().UnixNano() == info.ModTime().UnixMilli()*int64(time.Millisecond) {
		t.Skip("filesystem does not preserve sub-millisecond mtimes")
	}

	candidates, complete := codexDiscoverRolloutCandidates(context.Background(), base, codexRolloutScanCursor{mtimeNs: first.UnixNano()})
	if !complete || len(candidates) != 1 || candidates[0].path != path {
		t.Fatalf("complete=%v candidates=%+v, want same-millisecond append selected", complete, candidates)
	}
}

func TestCodexDiscoverRolloutCandidates_RetainsExactMtimeAppend(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "sessions", "2026", "08", "26")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-test.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	coarseMtime := time.Date(2026, 8, 26, 14, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, coarseMtime, coarseMtime); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mtimeNs := info.ModTime().UnixNano()
	initial, complete := codexDiscoverRolloutCandidates(context.Background(), base, codexRolloutScanCursor{})
	if !complete || len(initial) != 1 {
		t.Fatalf("complete=%v candidates=%+v, want initial rollout selected", complete, initial)
	}
	cursor := codexRolloutScanCursor{
		mtimeNs:             mtimeNs,
		boundaryFingerprint: codexRolloutBoundaryFingerprint(initial, mtimeNs),
	}
	unchanged, complete := codexDiscoverRolloutCandidates(context.Background(), base, cursor)
	if !complete || len(unchanged) != 0 {
		t.Fatalf("complete=%v candidates=%+v, want unchanged boundary skipped", complete, unchanged)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("{\"type\":\"token_count\"}\n")); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// Model a coarse filesystem: the append changes size but not mtime.
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	changed, complete := codexDiscoverRolloutCandidates(context.Background(), base, cursor)
	if !complete || len(changed) != 1 || changed[0].path != path {
		t.Fatalf("complete=%v candidates=%+v, want equal-mtime append selected", complete, changed)
	}
}

func TestCodexRolloutFallbackBuckets_CanonicalizesMixedIdentitySameLimit(t *testing.T) {
	base := t.TempDir()
	now := time.Date(2026, 8, 26, 14, 0, 0, 0, time.UTC)
	helperWriteRolloutLogAt(t, base, "26", "2026-08-26T13-50-00-session", "2026-08-26T13:50:00Z",
		[]map[string]any{{
			"primary": map[string]any{
				"used_percent": 31.0, "window_minutes": 300.0,
				"resets_at": float64(now.Add(time.Hour).Unix()),
			},
		}})
	helperWriteRolloutLogAt(t, base, "26", "2026-08-26T13-55-00-weekly", "2026-08-26T13:55:00Z",
		[]map[string]any{{
			"primary": map[string]any{
				"used_percent": 47.0, "window_minutes": 10080.0,
				"resets_at": float64(now.Add(72 * time.Hour).Unix()),
			},
		}})

	contributors, _, _, _, ok := codexRolloutFallbackBuckets(context.Background(), base, now, codexRolloutScanCursor{})
	if !ok {
		t.Fatal("expected mixed-identity rollout contributors")
	}
	if got := contributors[codexWindowPrimary][codexLegacyLimitID]; got.UsedPercentage != 31 {
		t.Fatalf("primary legacy contributor=%+v, want canonical session 31%%", got)
	}
	if got := contributors[codexWindowSecondary][codexLegacyLimitID]; got.UsedPercentage != 47 {
		t.Fatalf("secondary legacy contributor=%+v, want canonical weekly 47%%", got)
	}
	for slot, limits := range contributors {
		for limitID := range limits {
			if strings.ContainsRune(limitID, '\x00') {
				t.Fatalf("slot %q contains synthetic limit id %q", slot, limitID)
			}
		}
	}
}

func TestCodexRolloutFallbackBuckets_CanonicalizesEachFrameBeforeMerge(t *testing.T) {
	base := t.TempDir()
	now := time.Date(2026, 8, 26, 14, 0, 0, 0, time.UTC)
	helperWriteRolloutLogAt(t, base, "26", "2026-08-26T13-50-00-migrated", "2026-08-26T13:50:00Z",
		[]map[string]any{
			{
				"primary": map[string]any{
					"used_percent": 31.0, "window_minutes": 300.0,
					"resets_at": float64(now.Add(time.Hour).Unix()),
				},
			},
			{
				"primary": map[string]any{
					"used_percent": 47.0, "window_minutes": 10080.0,
					"resets_at": float64(now.Add(72 * time.Hour).Unix()),
				},
			},
		})

	contributors, _, _, highWater, ok := codexRolloutFallbackBuckets(context.Background(), base, now, codexRolloutScanCursor{})
	if !ok {
		t.Fatal("expected migrated rollout contributors")
	}
	if highWater == nil {
		t.Fatal("expected completed rollout scan high-water")
	}
	if got := contributors[codexWindowPrimary][codexLegacyLimitID]; got.UsedPercentage != 31 {
		t.Fatalf("primary legacy contributor=%+v, want preserved session 31%%", got)
	}
	if got := contributors[codexWindowSecondary][codexLegacyLimitID]; got.UsedPercentage != 47 {
		t.Fatalf("secondary legacy contributor=%+v, want migrated weekly 47%%", got)
	}
}

func TestCodexRolloutFallbackBuckets_MixedRefusalPreventsHighWaterAdvance(t *testing.T) {
	base := t.TempDir()
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	helperWriteRolloutLimitLog(t, base, "19", "2026-06-19T11-00-00-mixed",
		"2026-06-19T11:00:00.000Z", []map[string]any{{
			"primary": map[string]any{
				"used_percent": 31.0, "window_minutes": 300.0,
				"resets_at": float64(now.Add(time.Hour).Unix()),
			},
		}}, "2026-06-19T11:30:00.000Z")

	contributors, limit, _, highWater, ok := codexRolloutFallbackBuckets(context.Background(), base, now, codexRolloutScanCursor{})
	if !ok || len(contributors) == 0 {
		t.Fatal("expected numeric evidence preceding the refusal")
	}
	if limit.At.IsZero() {
		t.Fatal("expected live refusal evidence")
	}
	if highWater != nil {
		t.Fatalf("highWater=%+v, want unchanged while mixed-file refusal is live", *highWater)
	}
}

func TestCodexRolloutFallbackBuckets_ValidatesFutureRefusalTime(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name          string
		refusedAt     time.Time
		wantAt        time.Time
		wantHighWater bool
	}{
		{
			name:      "five minute boundary clamps to now",
			refusedAt: now.Add(codexProviderObservationFutureTolerance),
			wantAt:    now,
		},
		{
			name:          "beyond tolerance is rejected",
			refusedAt:     now.Add(codexProviderObservationFutureTolerance + time.Nanosecond),
			wantHighWater: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			helperWriteRolloutLimitLog(t, base, "19", "2026-06-19T11-00-00-future-refusal",
				"2026-06-19T11:00:00.000Z", nil, tc.refusedAt.Format(time.RFC3339Nano))

			_, limit, _, highWater, ok := codexRolloutFallbackBuckets(context.Background(), base, now, codexRolloutScanCursor{})
			if ok {
				t.Fatal("refusal-only rollout unexpectedly produced numeric contributors")
			}
			if !limit.At.Equal(tc.wantAt) {
				t.Fatalf("refusal time=%s, want %s", limit.At, tc.wantAt)
			}
			if (highWater != nil) != tc.wantHighWater {
				t.Fatalf("highWater=%+v, want present=%v", highWater, tc.wantHighWater)
			}
		})
	}
}

func TestCodexRolloutFallbackBuckets_ClampsFutureMtimeHighWater(t *testing.T) {
	base := t.TempDir()
	now := time.Date(2026, 8, 26, 14, 0, 0, 0, time.UTC)
	helperWriteRolloutLogAt(t, base, "26", "2026-08-26T13-50-00-future-mtime", "2026-08-26T13:50:00Z",
		[]map[string]any{{
			"primary": map[string]any{
				"used_percent": 31.0, "window_minutes": 300.0,
				"resets_at": float64(now.Add(time.Hour).Unix()),
			},
		}})
	rollout := filepath.Join(base, "sessions", "2026", "06", "26", "rollout-2026-08-26T13-50-00-future-mtime.jsonl")
	future := now.Add(24 * time.Hour)
	if err := os.Chtimes(rollout, future, future); err != nil {
		t.Fatalf("chtimes rollout: %v", err)
	}

	_, _, _, highWater, ok := codexRolloutFallbackBuckets(context.Background(), base, now, codexRolloutScanCursor{})
	if !ok || highWater == nil {
		t.Fatal("expected completed numeric rollout scan")
	}
	if highWater.mtimeNs != now.UnixNano() {
		t.Fatalf("highWater mtime=%s, want clamped to %s", time.Unix(0, highWater.mtimeNs), now)
	}
}

func TestCodexRolloutFallbackBuckets_FutureMtimesDoNotHideNormalCandidateBehindCap(t *testing.T) {
	base := t.TempDir()
	now := time.Date(2026, 8, 26, 14, 0, 0, 0, time.UTC)

	for i := 0; i < codexRolloutScanFileCap+1; i++ {
		name := fmt.Sprintf("2026-08-26T12-%02d-00-future", i)
		helperWriteRolloutLogAt(t, base, "26", name, "2026-08-26T12:00:00Z",
			[]map[string]any{{
				"secondary": map[string]any{
					"used_percent": 20.0, "window_minutes": 10080.0,
					"resets_at": float64(now.Add(72 * time.Hour).Unix()),
				},
			}})
		path := filepath.Join(base, "sessions", "2026", "06", "26", "rollout-"+name+".jsonl")
		future := now.Add(time.Duration(i+1) * time.Hour)
		if err := os.Chtimes(path, future, future); err != nil {
			t.Fatalf("chtimes future rollout %d: %v", i, err)
		}
	}
	rolloutName := "2026-08-26T13-59-00-normal"
	helperWriteRolloutLogAt(t, base, "26", rolloutName, "2026-08-26T13:59:00Z",
		[]map[string]any{{
			"primary": map[string]any{
				"used_percent": 73.0, "window_minutes": 300.0,
				"resets_at": float64(now.Add(time.Hour).Unix()),
			},
		}})
	normalPath := filepath.Join(base, "sessions", "2026", "06", "26", "rollout-"+rolloutName+".jsonl")
	normalMtime := now.Add(-time.Minute)
	if err := os.Chtimes(normalPath, normalMtime, normalMtime); err != nil {
		t.Fatalf("chtimes normal rollout: %v", err)
	}

	contributors, _, _, _, ok := codexRolloutFallbackBuckets(context.Background(), base, now, codexRolloutScanCursor{})
	if !ok {
		t.Fatal("expected normal-time rollout contributor")
	}
	if got := contributors[codexWindowPrimary][codexLegacyLimitID]; got.UsedPercentage != 73 {
		t.Fatalf("primary contributor=%+v, want normal-time rollout selected ahead of future mtimes", got)
	}
}

func TestCodexRolloutFallbackBuckets_FutureAuthWatermarkLeavesProgressForRetry(t *testing.T) {
	base := t.TempDir()
	now := time.Date(2026, 8, 26, 14, 0, 0, 0, time.UTC)
	authPath := filepath.Join(base, "auth.json")
	if err := os.WriteFile(authPath, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	future := now.Add(time.Hour)
	if err := os.Chtimes(authPath, future, future); err != nil {
		t.Fatalf("chtimes future auth file: %v", err)
	}
	rolloutName := "2026-08-26T13-50-00-current"
	helperWriteRolloutLogAt(t, base, "26", rolloutName, "2026-08-26T13:50:00Z",
		[]map[string]any{{
			"primary": map[string]any{
				"used_percent": 64.0, "window_minutes": 300.0,
				"resets_at": float64(now.Add(time.Hour).Unix()),
			},
		}})

	contributors, _, _, highWater, ok := codexRolloutFallbackBuckets(context.Background(), base, now, codexRolloutScanCursor{})
	if ok || len(contributors) != 0 || highWater != nil {
		t.Fatalf("future auth watermark produced contributors=%+v highWater=%+v ok=%v", contributors, highWater, ok)
	}

	corrected := now.Add(-time.Hour)
	if err := os.Chtimes(authPath, corrected, corrected); err != nil {
		t.Fatalf("correct auth file mtime: %v", err)
	}
	contributors, _, _, highWater, ok = codexRolloutFallbackBuckets(context.Background(), base, now, codexRolloutScanCursor{})
	if !ok || highWater == nil {
		t.Fatalf("corrected auth watermark did not retry rollout: contributors=%+v highWater=%+v ok=%v", contributors, highWater, ok)
	}
	if got := contributors[codexWindowPrimary][codexLegacyLimitID]; got.UsedPercentage != 64 {
		t.Fatalf("primary contributor=%+v, want retried current rollout", got)
	}
}

func TestCodexRolloutScanCursorForAccount_ResetsPersistedFutureCursor(t *testing.T) {
	now := time.Date(2026, 8, 26, 14, 0, 0, 0, time.UTC)
	fingerprint := "account-a"

	for _, tc := range []struct {
		name string
		snap codexRateLimitSnapshot
	}{
		{
			name: "nanosecond cursor",
			snap: codexRateLimitSnapshot{
				AccountFingerprint:                  fingerprint,
				RolloutHighWaterMtimeNs:             now.Add(24 * time.Hour).UnixNano(),
				RolloutHighWaterBoundaryFingerprint: "future-boundary",
			},
		},
		{
			name: "legacy millisecond cursor",
			snap: codexRateLimitSnapshot{
				AccountFingerprint:      fingerprint,
				RolloutHighWaterMtimeMs: now.Add(24 * time.Hour).UnixMilli(),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := filepath.Join(t.TempDir(), "codex-rate-limits.json")
			t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
			helperWriteJSON(t, cache, tc.snap)

			base := t.TempDir()
			dir := filepath.Join(base, "sessions", "2026", "08", "26")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "rollout-after-clock-rollback.jsonl")
			if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			mtime := now.Add(-time.Minute)
			if err := os.Chtimes(path, mtime, mtime); err != nil {
				t.Fatal(err)
			}

			cursor := codexRolloutScanCursorForAccount(fingerprint, now)
			if cursor.mtimeNs != 0 || cursor.boundaryFingerprint != "" {
				t.Fatalf("cursor=%+v, want reset after clock rollback", cursor)
			}
			candidates, complete := codexDiscoverRolloutCandidates(context.Background(), base, cursor)
			if !complete || len(candidates) != 1 || candidates[0].path != path {
				t.Fatalf("complete=%v candidates=%+v, want post-rollback rollout selected", complete, candidates)
			}
			_, _, _, progress, _ := codexRolloutFallbackBuckets(context.Background(), base, now, cursor)
			if progress == nil {
				t.Fatal("expected completed post-rollback scan progress")
			}
			mergeCodexRateLimitCachePerLimitProgress(
				cache, nil, nil, false, nil, false, now, fingerprint, progress, "",
			)
			persisted, ok := loadCodexRateLimitSnapshot(cache)
			if !ok {
				t.Fatal("expected updated cache")
			}
			if persisted.RolloutHighWaterMtimeNs != progress.mtimeNs ||
				persisted.RolloutHighWaterBoundaryFingerprint != progress.boundaryFingerprint {
				t.Fatalf("persisted cursor=(%d, %q), want completed lower cursor=(%d, %q)",
					persisted.RolloutHighWaterMtimeNs, persisted.RolloutHighWaterBoundaryFingerprint,
					progress.mtimeNs, progress.boundaryFingerprint)
			}
		})
	}
}
