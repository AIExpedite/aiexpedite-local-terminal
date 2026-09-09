// cliagent_usage_grok_freshness_test.go
// -----------------------------------------------------------------------------
// Post-smoke Grok passed maintenance smoke while reporting provider_unmetered,
// observableMetricCount 0 and NO latestObservedAt — the card could not tell a
// just-confirmed unmetered period from a complete miss, because both serialized
// as a single `unknown: true` row with observedAt stripped.
//
// These tests pin the three states the parser may leave, the TTL that bounds the
// middle one, and the decode tolerance that keeps a Grok update from silently
// zeroing the only capacity source we have.
//
// Layout mirrors cliagent_usage_antigravity_freshness_test.go.
// -----------------------------------------------------------------------------

package main

import (
	"fmt"
	"testing"
	"time"
)

// grokFreshnessIdentity is the producer line every fixture below needs: a record
// with no identity logged before it is refused outright.
const grokFreshnessIdentity = `{"ts":"2026-08-17T23:00:00.000Z","msg":"session start","ctx":{"user_id":"acct-1"}}`

// grokUnmeteredLine renders a Grok 1.0 confirmed-unmetered record (no
// creditUsagePercent) under an arbitrary message spelling.
func grokUnmeteredLine(ts, msg string) string {
	return fmt.Sprintf(`{"ts":%q,"msg":%q,"ctx":{"config":{`+
		`"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2126-08-24T22:28:32Z"}},`+
		`"subscriptionTier":"SuperGrok"}}`, ts, msg)
}

// A confirmed-unmetered record inside the TTL is Unknown WITH an observation
// time; the same record past it degrades to the inferred placeholder rather
// than presenting a stale "confirmed" reading.
func TestGrokConfirmedUnmeteredObservationExpiresAtTheTTL(t *testing.T) {
	observed := time.Date(2026, 8, 17, 23, 2, 12, 0, time.UTC)
	base := writeGrokLog(t, grokFreshnessIdentity,
		grokUnmeteredLine("2026-08-17T23:02:12Z", grokBillingLogMessage))

	snap, ok := readGrokBillingSnapshot(base, []string{"acct-1"})
	if !ok {
		t.Fatal("the unmetered record must be a usable snapshot")
	}

	fresh := grokBillingMetrics(snap, observed.Add(grokBillingObservationTTL-time.Minute))
	if len(fresh) != 1 || !fresh[0].Unknown {
		t.Fatalf("want a single unknown row, got %+v", fresh)
	}
	if fresh[0].ObservedAt != "2026-08-17T23:02:12Z" {
		t.Fatalf("ObservedAt = %q, want the confirmed-unmetered observation inside the TTL",
			fresh[0].ObservedAt)
	}

	stale := grokBillingMetrics(snap, observed.Add(grokBillingObservationTTL+time.Minute))
	if len(stale) != 1 || !stale[0].Unknown {
		t.Fatalf("want a single unknown row, got %+v", stale)
	}
	if stale[0].ObservedAt != "" {
		t.Fatalf("ObservedAt = %q, want the inferred placeholder past the TTL", stale[0].ObservedAt)
	}
}

// The TTL bounds confirmed-unmetered retention ONLY. A numeric reading keeps its
// existing lifecycle (bounded by the period end), so no shipped percentage
// behaviour moves with this constant.
func TestGrokNumericReadingIsNotBoundedByTheObservationTTL(t *testing.T) {
	observed := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	now := observed.Add(grokBillingObservationTTL + 24*time.Hour)
	metrics := grokBillingMetrics(grokBillingSnapshot{
		UsedPercent:    33,
		HasUsedPercent: true,
		ObservedAt:     observed,
		PeriodType:     "USAGE_PERIOD_TYPE_WEEKLY",
		PeriodEnd:      now.Add(24 * time.Hour),
		HasPeriodEnd:   true,
	}, now)

	if len(metrics) != 1 || metrics[0].ObservedAt != "2026-08-01T12:00:00Z" {
		t.Fatalf("a numeric reading must keep its observation time: %+v", metrics)
	}
	if metrics[0].Consumed == nil || *metrics[0].Consumed != 33 {
		t.Fatalf("the percentage must still be plotted: %+v", metrics)
	}
}

// "Survives CLI updates": every message spelling in the closed alias set, and
// every snake_case field alias, decodes to the same snapshot as the camelCase
// original.
func TestGrokBillingDecodesEveryAllowlistedMessageSpelling(t *testing.T) {
	for _, msg := range grokBillingLogMessages {
		t.Run(msg, func(t *testing.T) {
			base := writeGrokLog(t, grokFreshnessIdentity,
				grokUnmeteredLine("2026-08-17T23:02:12Z", msg))
			snap, ok := readGrokBillingSnapshot(base, []string{"acct-1"})
			if !ok {
				t.Fatalf("message %q must be recognized as a credits-config record", msg)
			}
			if got := snap.ObservedAt.UTC().Format(time.RFC3339); got != "2026-08-17T23:02:12Z" {
				t.Fatalf("ObservedAt = %s, want the record timestamp", got)
			}
			if snap.PeriodType != "USAGE_PERIOD_TYPE_WEEKLY" {
				t.Fatalf("PeriodType = %q, want the weekly window", snap.PeriodType)
			}
		})
	}
}

func TestGrokBillingDecodesSnakeCaseFieldAliases(t *testing.T) {
	snake := `{"ts":"2026-08-17T23:02:12Z","msg":"billing: fetched credits config","ctx":{"config":{` +
		`"credit_usage_percent":33,` +
		`"current_period":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2126-08-24T22:28:32Z"},` +
		`"on_demand_cap":{"val":50},"on_demand_used":{"val":12}},` +
		`"subscription_tier":"SuperGrok"}}`
	camel := `{"ts":"2026-08-17T23:02:12Z","msg":"billing: fetched credits config","ctx":{"config":{` +
		`"creditUsagePercent":33,` +
		`"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2126-08-24T22:28:32Z"},` +
		`"onDemandCap":{"val":50},"onDemandUsed":{"val":12}},` +
		`"subscriptionTier":"SuperGrok"}}`

	snakeSnap, ok := readGrokBillingSnapshot(writeGrokLog(t, grokFreshnessIdentity, snake), []string{"acct-1"})
	if !ok {
		t.Fatal("a snake_case record must still decode")
	}
	camelSnap, ok := readGrokBillingSnapshot(writeGrokLog(t, grokFreshnessIdentity, camel), []string{"acct-1"})
	if !ok {
		t.Fatal("the camelCase control must decode")
	}
	if snakeSnap != camelSnap {
		t.Fatalf("snake_case snapshot %+v != camelCase snapshot %+v", snakeSnap, camelSnap)
	}
	if !snakeSnap.HasUsedPercent || snakeSnap.UsedPercent != 33 ||
		!snakeSnap.HasOnDemand || snakeSnap.OnDemandCap != 50 ||
		!snakeSnap.HasOnDemandUsed || snakeSnap.OnDemandUsed != 12 ||
		snakeSnap.SubscriptionTier != "SuperGrok" {
		t.Fatalf("snake_case aliases did not populate the snapshot: %+v", snakeSnap)
	}
}

// An unrecognized credits window must not suppress the on-demand pool: that is a
// separate pool with its own numbers, and dropping it was part of why a run
// could report observableMetricCount 0 while real figures sat in the record.
func TestGrokUnrecognizedPeriodStillPublishesTheOnDemandPool(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	metrics := grokBillingMetrics(grokBillingSnapshot{
		ObservedAt:      now.Add(-time.Hour),
		PeriodType:      "USAGE_PERIOD_TYPE_SOMETHING_NEW",
		HasOnDemand:     true,
		HasOnDemandUsed: true,
		OnDemandCap:     50,
		OnDemandUsed:    12,
	}, now)

	assertGrokMetricsJSON(t, metrics,
		`[{"kind":"weekly","label":"Weekly credits","unit":"%","observedAt":"2026-08-19T11:00:00Z","unknown":true},`+
			`{"kind":"tokens","label":"On-demand credits","unit":"credits","total":50,"remaining":38,"consumed":12,"observedAt":"2026-08-19T11:00:00Z"}]`)
}

// The regression the EXACT-string match exists to prevent: the reader fails
// closed on a newest record it cannot decode, so an unrelated high-frequency
// `billing:` line must never be mistaken for a credits-config record — it would
// permanently block the good older percentage sitting underneath it.
func TestGrokUnrelatedBillingPrefixedLineIsNotARecord(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	base := writeGrokLog(t, grokIdentityLine, grokLegacyBillingLine,
		`{"ts":"2026-08-14T10:00:00Z","msg":"billing: opening checkout","ctx":{"config":{"creditUsagePercent":99}}}`)

	snap, ok := readGrokBillingSnapshot(base, []string{"acct-1"})
	if !ok {
		t.Fatal("an unrelated billing-prefixed line must not shadow the real record")
	}
	if !snap.HasUsedPercent || snap.UsedPercent != 33 {
		t.Fatalf("want the real legacy reading (33%%), got %+v", snap)
	}
	metrics := grokBillingMetrics(snap, now)
	if metrics[0].Consumed == nil || *metrics[0].Consumed != 33 {
		t.Fatalf("the real percentage must still be plotted: %+v", metrics)
	}
}

// A newest record under a RECOGNIZED message whose body cannot be decoded still
// blocks an older percentage — no stale-value resurrection.
func TestGrokUndecodableNewestRecordBlocksOlderPercentage(t *testing.T) {
	base := writeGrokLog(t, grokIdentityLine, grokLegacyBillingLine,
		`{"ts":"not-a-timestamp","msg":"billing: fetched credits","ctx":{"config":{}}}`)

	if snap, ok := readGrokBillingSnapshot(base, []string{"acct-1"}); ok {
		t.Fatalf("an undecodable newest record must fail closed, got %+v", snap)
	}
}
