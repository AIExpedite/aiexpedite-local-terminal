// cliagent_usage_grok_unified_billing_test.go
// -----------------------------------------------------------------------------
// Grok 1.0 stopped emitting `creditUsagePercent`.
//
// From a real machine's ~/.grok/logs/unified.jsonl, the same `msg` across an
// upgrade — every other key identical, one gone:
//
//   ts=2026-08-13T15:49:37Z ver=0.2.118  [... creditUsagePercent, currentPeriod, ...]
//   ts=2026-08-17T23:02:12Z ver=1.0.3    [... currentPeriod, ...]              ← no percent
//
// The parser required that field, so the newest record was rejected as "not a
// billing record" and the back-to-front scan kept walking until it found a
// PRE-UPGRADE one. The card then published a four-day-old reading whose billing
// period had since ended, rendering as "Usage unobservable — Last observed
// 8/13 (6 days ago)" while a current period sat unread in the same file.
// -----------------------------------------------------------------------------

package main

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Verbatim shapes from the real log (identity line included, since a record is
// only trusted when it can be tied to the current account).
const (
	grokIdentityLine = `{"ts":"2026-08-17T23:02:00.000Z","msg":"session start","ctx":{"user_id":"acct-1"}}`

	grokLegacyBillingLine = `{"ts":"2026-08-13T15:49:37.229Z","msg":"billing: fetched credits config",` +
		`"ver":"0.2.118","ctx":{"config":{"creditUsagePercent":33,` +
		`"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-08-10T22:28:32.746607+00:00",` +
		`"end":"2026-08-17T22:28:32.746607+00:00"},"onDemandCap":{"val":0},"onDemandUsed":{"val":0},` +
		`"prepaidBalance":{"val":0},"isUnifiedBillingUser":true,"historyLen":0},"subscriptionTier":"SuperGrok"}}`

	grokUnifiedBillingLine = `{"ts":"2026-08-17T23:02:12.510Z","msg":"billing: fetched credits config",` +
		`"ver":"1.0.3","ctx":{"config":{` +
		`"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-08-17T22:28:32.746607+00:00",` +
		`"end":"2026-08-24T22:28:32.746607+00:00"},"onDemandCap":{"val":0},"onDemandUsed":{"val":0},` +
		`"prepaidBalance":{"val":0},"isUnifiedBillingUser":true,"historyLen":0},"subscriptionTier":"SuperGrok"}}`
)

func writeGrokLog(t *testing.T, lines ...string) string {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "unified.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return base
}

// The regression, end to end over the real log shape.
func TestGrokBillingReadsTheNewestRecordEvenWithoutAUsagePercent(t *testing.T) {
	base := writeGrokLog(t, grokIdentityLine, grokLegacyBillingLine, grokUnifiedBillingLine)

	snap, ok := readGrokBillingSnapshot(base, []string{"acct-1"})
	if !ok {
		t.Fatal("a 1.0 record naming a current period must still be a usable snapshot")
	}
	if snap.HasUsedPercent {
		t.Fatal("the 1.0 record carries no percentage; HasUsedPercent must be false")
	}
	wantObserved := "2026-08-17T23:02:12Z"
	if got := snap.ObservedAt.UTC().Format(time.RFC3339); got != wantObserved {
		t.Fatalf("ObservedAt = %s, want the NEWEST record %s — the scan walked back to a pre-upgrade one",
			got, wantObserved)
	}
	if !snap.HasPeriodEnd || snap.PeriodEnd.UTC().Format(time.RFC3339) != "2026-08-24T22:28:32Z" {
		t.Fatalf("PeriodEnd = %v, want the current 1.0 period", snap.PeriodEnd)
	}
	if snap.SubscriptionTier != "SuperGrok" {
		t.Fatalf("SubscriptionTier = %q, want it still read from the 1.0 record", snap.SubscriptionTier)
	}
}

// The card: a current period with an honest reset, and no invented number.
func TestGrokCardReportsTheCurrentPeriodWithUsageUnobservable(t *testing.T) {
	base := writeGrokLog(t, grokIdentityLine, grokLegacyBillingLine, grokUnifiedBillingLine)
	now := time.Date(2026, 8, 19, 23, 0, 0, 0, time.UTC) // inside the 1.0 period

	snap, ok := readGrokBillingSnapshot(base, []string{"acct-1"})
	if !ok {
		t.Fatal("snapshot unexpectedly refused")
	}
	metrics := grokBillingMetrics(snap, now)
	if len(metrics) != 1 {
		t.Fatalf("len(metrics) = %d, want 1", len(metrics))
	}
	m := metrics[0]
	if !m.Unknown {
		t.Fatalf("metric = %+v, want Unknown — Grok 1.0 publishes no usage figure", m)
	}
	if m.Consumed != nil {
		t.Fatalf("Consumed = %v, want nil", *m.Consumed)
	}
	if want := "2026-08-24T22:28:32Z"; m.ResetAt != want {
		t.Fatalf("ResetAt = %q, want the CURRENT period %q", m.ResetAt, want)
	}
	// Nothing was observed, so nothing may claim to have been: the stale
	// "Last observed 6 days ago" is exactly the symptom this fixes.
	if m.ObservedAt != "" {
		t.Fatalf("ObservedAt = %q, want empty for an unobserved period", m.ObservedAt)
	}
	if m.Label != "Weekly credits" || m.Kind != limitKindWeekly {
		t.Fatalf("metric label/kind = %q/%q, want the weekly credits row", m.Label, m.Kind)
	}
}

// A legacy-only log must be unaffected — installs that have not upgraded keep
// their real number.
func TestGrokBillingStillPlotsALegacyRecord(t *testing.T) {
	base := writeGrokLog(t, grokIdentityLine, grokLegacyBillingLine)
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC) // inside the legacy period

	snap, ok := readGrokBillingSnapshot(base, []string{"acct-1"})
	if !ok {
		t.Fatal("legacy snapshot refused")
	}
	if !snap.HasUsedPercent || snap.UsedPercent != 33 {
		t.Fatalf("UsedPercent = %v (has=%v), want 33", snap.UsedPercent, snap.HasUsedPercent)
	}
	m := grokBillingMetrics(snap, now)[0]
	if m.Unknown || m.Consumed == nil || *m.Consumed != 33 {
		t.Fatalf("metric = %+v, want a real 33%% reading", m)
	}
	if m.ObservedAt != "2026-08-13T15:49:37Z" {
		t.Fatalf("ObservedAt = %q, want the record's own timestamp", m.ObservedAt)
	}
}

// An ended period stays Unknown and keeps its observation time — that row is
// reporting on a window that really was observed, just no longer current.
func TestGrokEndedPeriodKeepsItsObservationTime(t *testing.T) {
	base := writeGrokLog(t, grokIdentityLine, grokLegacyBillingLine)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC) // past the legacy period end

	snap, _ := readGrokBillingSnapshot(base, []string{"acct-1"})
	m := grokBillingMetrics(snap, now)[0]
	if !m.Unknown {
		t.Fatalf("metric = %+v, want Unknown for an ended period", m)
	}
	if m.ObservedAt != "2026-08-13T15:49:37Z" {
		t.Fatalf("ObservedAt = %q, want the ended window's real observation", m.ObservedAt)
	}
	if m.ResetAt != "" {
		t.Fatalf("ResetAt = %q, want none — that period has already ended", m.ResetAt)
	}
}

// The account gate still governs the newest record: making 1.0 records usable
// must not let another account's credits through.
func TestGrokUnifiedRecordStillHonoursTheAccountGate(t *testing.T) {
	base := writeGrokLog(t, grokIdentityLine, grokUnifiedBillingLine)

	if _, ok := readGrokBillingSnapshot(base, []string{"someone-else"}); ok {
		t.Fatal("a record produced by another account must still be refused")
	}
}

// On-demand rows are independent of the usage percent and must survive the
// shape change.
func TestGrokOnDemandSurvivesAMissingUsagePercent(t *testing.T) {
	line := `{"ts":"2026-08-17T23:02:12.510Z","msg":"billing: fetched credits config","ctx":{"config":{` +
		`"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-08-17T22:28:32Z","end":"2026-08-24T22:28:32Z"},` +
		`"onDemandCap":{"val":50},"onDemandUsed":{"val":12}},"subscriptionTier":"SuperGrok"}}`
	base := writeGrokLog(t, grokIdentityLine, line)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	snap, ok := readGrokBillingSnapshot(base, []string{"acct-1"})
	if !ok {
		t.Fatal("snapshot refused")
	}
	metrics := grokBillingMetrics(snap, now)
	if len(metrics) != 2 {
		t.Fatalf("len(metrics) = %d, want the credits row plus on-demand", len(metrics))
	}
	onDemand := metrics[1]
	if onDemand.Consumed == nil || *onDemand.Consumed != 12 || onDemand.Total == nil || *onDemand.Total != 50 {
		t.Fatalf("on-demand = %+v, want 12 of 50", onDemand)
	}
	if !metrics[0].Unknown {
		t.Fatalf("credits row = %+v, want Unknown", metrics[0])
	}
}

// A NaN percentage is not a reading. Guarding it here keeps a malformed line
// from plotting a nonsense bar.
func TestGrokNaNUsagePercentIsNotAReading(t *testing.T) {
	rec := grokBillingRecord{TS: "2026-08-17T23:02:12.510Z", Msg: grokBillingLogMessage}
	nan := math.NaN()
	rec.Ctx.Config.CreditUsagePercent = &nan
	rec.Ctx.Config.CurrentPeriod.Type = "USAGE_PERIOD_TYPE_WEEKLY"

	snap, ok := grokBillingSnapshotFromRecord(rec)
	if !ok {
		t.Fatal("a record with a current period is still usable")
	}
	if snap.HasUsedPercent {
		t.Fatal("NaN is not a usage reading")
	}
}

// A record with no parseable timestamp is still refused: without `ts` there is
// no observation time, and the scan must keep looking.
func TestGrokRecordWithoutATimestampIsRefused(t *testing.T) {
	rec := grokBillingRecord{Msg: grokBillingLogMessage}
	rec.Ctx.Config.CurrentPeriod.Type = "USAGE_PERIOD_TYPE_WEEKLY"

	if _, ok := grokBillingSnapshotFromRecord(rec); ok {
		t.Fatal("a record with no ts must be refused")
	}
}
