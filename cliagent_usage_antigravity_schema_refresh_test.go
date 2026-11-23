package main

import (
	"testing"
	"time"
)

// A reading cached under quota schema 1 may chart a bucket Google disabled,
// and nothing in its figure says which. After an update, replaying one owes
// exactly one bounded refresh (antigravityOutdatedSchemaFloor), paid by the
// ordinary debt worker; a schema-2 reading owes nothing.

// helperWriteSchemaCache seeds a cached reading observed at `at` under the
// given schema version.
func helperWriteSchemaCache(t *testing.T, cache string, at time.Time, schema int) {
	t.Helper()
	helperWriteJSON(t, cache, antigravityQuotaSnapshot{
		SchemaVersion:      schema,
		ObservedAt:         at.UTC().Format(time.RFC3339),
		ObservedAtMs:       at.UnixMilli(),
		AccountFingerprint: fingerprintAccount("antigravity", "ada@example.com"),
		Account:            "ada@example.com",
		Buckets: []antigravityQuotaBucket{
			{Group: "Gemini Models", Window: "weekly", RemainingFraction: 0.4, ResetTime: "2126-08-14T00:00:00Z"},
		},
	})
}

func helperResetAntigravityNudgeCooldown(t *testing.T) {
	t.Helper()
	n := &antigravityRefreshNudge
	n.mu.Lock()
	n.lastAt = time.Time{}
	n.mu.Unlock()
}

// helperGatherAntigravity is helperGather plus waiting out any refresh the
// gather's nudge started.
func helperGatherAntigravity(t *testing.T, home string) {
	t.Helper()
	helperGather(t, home)
	antigravityUsageRefreshWaitIdle()
}

func TestAntigravityOutdatedSchemaFloor_Boundaries(t *testing.T) {
	now := time.Now()
	observed := now.Add(-time.Minute).Truncate(time.Second)
	snap := func(schema int) antigravityQuotaSnapshot {
		return antigravityQuotaSnapshot{
			SchemaVersion: schema, ObservedAt: observed.Format(time.RFC3339), ObservedAtMs: observed.UnixMilli(),
			Buckets: []antigravityQuotaBucket{{Group: "Gemini Models", Window: "weekly", RemainingFraction: 0.4}},
		}
	}
	want := antigravityOwedFloor{At: now, Proven: true, Aged: true, Outdated: true}
	for _, schema := range []int{0, 1} {
		if f := antigravityOutdatedSchemaFloor(now, snap(schema), antigravityOwedFloor{}); f != want {
			t.Errorf("schema %d: floor=%+v, want %+v", schema, f, want)
		}
	}
	if f := antigravityOutdatedSchemaFloor(now, snap(antigravityQuotaSchemaVersion), antigravityOwedFloor{}); !f.At.IsZero() {
		t.Errorf("current schema: floor=%+v, want none", f)
	}
	if f := antigravityOutdatedSchemaFloor(now, antigravityQuotaSnapshot{}, antigravityOwedFloor{}); !f.At.IsZero() {
		t.Errorf("nothing cached: floor=%+v, want none", f)
	}
	run := antigravityOwedFloor{At: observed.Add(30 * time.Second), Proven: true}
	if f := antigravityOutdatedSchemaFloor(now, snap(1), run); f != run {
		t.Errorf("run floor newer than the reading: floor=%+v, want the run's %+v", f, run)
	}
	aged := antigravityOwedFloor{At: now, Proven: true, Aged: true}
	if f := antigravityOutdatedSchemaFloor(now, snap(1), aged); f != aged {
		t.Errorf("gated aged floor: floor=%+v, want it kept", f)
	}
	covered := antigravityOwedFloor{At: observed.Add(-time.Minute)}
	if f := antigravityOutdatedSchemaFloor(now, snap(1), covered); f != want {
		t.Errorf("run floor the reading covers: floor=%+v, want the schema floor", f)
	}
}

// One replayed schema-1 reading owes one aged debt and one read; the cooldown
// keeps the next gather from asking again; a schema-2 reading retires the
// need for good.
func TestAntigravityOutdatedSchema_GatherOwesOneBoundedRefresh(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	home := t.TempDir()
	observed := time.Now().Add(-30 * time.Second).Truncate(time.Second)
	helperWriteSchemaCache(t, cache, observed, 1)
	reads := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })
	helperResetAntigravityNudgeCooldown(t)
	t.Cleanup(func() { helperResetAntigravityNudgeCooldown(t) })

	helperGatherAntigravity(t, home)
	state := helperFreshnessState(t)
	if state.RefreshOwedAtMs == 0 || state.RefreshOwedFloorMs <= observed.UnixMilli() {
		t.Fatalf("state=%+v, want a debt floored after the replayed reading", state)
	}
	if reads.Load() != 1 {
		t.Fatalf("reads=%d, want one", reads.Load())
	}
	d := &antigravityDebtRoute
	d.mu.Lock()
	route := d.route
	d.mu.Unlock()
	if route != antigravityDebtRouteAged {
		t.Errorf("route=%q, want %q", route, antigravityDebtRouteAged)
	}

	helperGatherAntigravity(t, home)
	if reads.Load() != 1 {
		t.Errorf("reads=%d after a second gather, want the cooldown to hold it at one", reads.Load())
	}

	// A reading this build writes is schema 2, and it covers the debt.
	snap, _ := cachedAntigravityQuotaSnapshot()
	snap.ObservedAt, snap.ObservedAtMs = time.Now().UTC().Format(time.RFC3339), time.Now().UnixMilli()
	saveAntigravityQuotaSnapshot(snap)
	if cached, _ := cachedAntigravityQuotaSnapshot(); cached.SchemaVersion != antigravityQuotaSchemaVersion {
		t.Fatalf("schema=%d, want %d", cached.SchemaVersion, antigravityQuotaSchemaVersion)
	}
	if state := helperFreshnessState(t); state.RefreshOwedAtMs != 0 {
		t.Fatalf("state=%+v, want the schema-2 reading to settle the debt", state)
	}
	helperResetAntigravityNudgeCooldown(t)
	helperGatherAntigravity(t, home)
	if state := helperFreshnessState(t); state.RefreshOwedAtMs != 0 || reads.Load() != 1 {
		t.Errorf("state=%+v reads=%d, want no schema debt for a schema-2 reading", state, reads.Load())
	}
}

// A terminal debt (no stored login) is never replaced by the schema floor: a
// device that cannot pay keeps its v1 rows until a run brings a new reading.
func TestAntigravityOutdatedSchema_KeepsATerminalDebt(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	home := t.TempDir()
	now := time.Now()
	helperWriteSchemaCache(t, cache, now.Add(-time.Hour).Truncate(time.Second), 1)
	owedAt := now.Add(-30 * time.Minute)
	helperOwedDebt(t, antigravityUsageFreshness{
		RefreshOwedFloorMs: owedAt.UnixMilli(), RefreshOwedAtMs: owedAt.UnixMilli(),
		Attempts: antigravityRefreshDebtMaxAttempts, Outcome: liveProbeOutcomeCodeAssistNoLogin,
	})
	reads := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistOK })
	helperResetAntigravityNudgeCooldown(t)
	t.Cleanup(func() { helperResetAntigravityNudgeCooldown(t) })

	helperGatherAntigravity(t, home)
	state := helperFreshnessState(t)
	if state.Outcome != liveProbeOutcomeCodeAssistNoLogin || state.RefreshOwedFloorMs != owedAt.UnixMilli() {
		t.Errorf("state=%+v, want the terminal debt left alone", state)
	}
	if reads.Load() != 0 {
		t.Errorf("reads=%d, want none", reads.Load())
	}
}

// A schema-2 reading, replayed with no run behind it, owes nothing.
func TestAntigravityOutdatedSchema_CurrentSchemaOwesNothing(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	home := t.TempDir()
	helperWriteSchemaCache(t, cache, time.Now().Add(-time.Hour).Truncate(time.Second), antigravityQuotaSchemaVersion)
	reads := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistOK })
	helperResetAntigravityNudgeCooldown(t)
	t.Cleanup(func() { helperResetAntigravityNudgeCooldown(t) })

	helperGatherAntigravity(t, home)
	if state := helperFreshnessState(t); state.RefreshOwedAtMs != 0 || reads.Load() != 0 {
		t.Errorf("state=%+v reads=%d, want no debt and no read", state, reads.Load())
	}
}
