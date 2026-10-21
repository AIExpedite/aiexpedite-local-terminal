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
	"os"
	"path/filepath"
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

// A recognized DAILY window with no parseable end cannot present itself as
// current for the full three-day TTL — that period has certainly rolled over,
// and downstream would read Unknown + ObservedAt as current billing evidence.
func TestGrokConfirmedUnmeteredExpiresWithinItsOwnPeriodWhenTheEndIsUnparseable(t *testing.T) {
	observed := time.Date(2026, 8, 17, 23, 2, 12, 0, time.UTC)
	snap := grokBillingSnapshot{
		ObservedAt: observed,
		PeriodType: "USAGE_PERIOD_TYPE_DAILY",
		// No HasPeriodEnd: the record's end was missing or malformed.
	}

	within := grokBillingMetrics(snap, observed.Add(23*time.Hour))
	if len(within) != 1 || !within[0].Unknown || within[0].ObservedAt != "2026-08-17T23:02:12Z" {
		t.Fatalf("inside the daily window the observation must stand: %+v", within)
	}

	rolled := grokBillingMetrics(snap, observed.Add(25*time.Hour))
	if len(rolled) != 1 || !rolled[0].Unknown {
		t.Fatalf("want a single unknown row, got %+v", rolled)
	}
	if rolled[0].ObservedAt != "" {
		t.Fatalf("ObservedAt = %q — a rolled-over daily period must not read as confirmed",
			rolled[0].ObservedAt)
	}
	if rolled[0].ResetAt != "" {
		t.Fatalf("ResetAt = %q — a nominal length is not an observed boundary", rolled[0].ResetAt)
	}
}

// The narrower bound applies ONLY where we can name the window. A weekly or
// monthly period is longer than the TTL, and an unrecognized one gives us no
// length at all, so both keep the global TTL as their only bound.
func TestGrokConfirmedUnmeteredKeepsTheGlobalTTLForLongerAndUnnamedWindows(t *testing.T) {
	observed := time.Date(2026, 8, 17, 23, 2, 12, 0, time.UTC)
	for _, periodType := range []string{"USAGE_PERIOD_TYPE_WEEKLY", "USAGE_PERIOD_TYPE_MONTHLY", "USAGE_PERIOD_TYPE_UNSPECIFIED"} {
		snap := grokBillingSnapshot{ObservedAt: observed, PeriodType: periodType}
		metrics := grokBillingMetrics(snap, observed.Add(grokBillingObservationTTL-time.Minute))
		if len(metrics) != 1 || metrics[0].ObservedAt != "2026-08-17T23:02:12Z" {
			t.Fatalf("%s: want the observation retained to the TTL, got %+v", periodType, metrics)
		}
	}
}

// A window whose end IS parseable and still ahead keeps the shipped lifecycle —
// the period-length bound must not clip a window the record itself dated.
func TestGrokConfirmedUnmeteredWithAParseableEndIsNotClippedByThePeriodLength(t *testing.T) {
	observed := time.Date(2026, 8, 17, 23, 2, 12, 0, time.UTC)
	now := observed.Add(30 * time.Hour)
	metrics := grokBillingMetrics(grokBillingSnapshot{
		ObservedAt:   observed,
		PeriodType:   "USAGE_PERIOD_TYPE_DAILY",
		PeriodEnd:    now.Add(time.Hour),
		HasPeriodEnd: true,
	}, now)
	if len(metrics) != 1 || !metrics[0].Unknown || metrics[0].ObservedAt != "2026-08-17T23:02:12Z" {
		t.Fatalf("a dated, still-open window must keep its observation: %+v", metrics)
	}
}

/* --------------------------------------------------------------------------
   Newest-source selection (post-update freshness)
   --------------------------------------------------------------------------
   The card publishes ONE Grok observation: the newest of the persistent log
   tail (a direct run's record, or a smoke/ACP record merged there by
   persistGrokManagedBillingSnapshot) and the live billing cache. An older
   source must never pin a stale observedAt, and the account gate is never
   widened by the choice.
   ------------------------------------------------------------------------ */

// grokFreshnessHome seeds a home with a login for `email`, a producer identity
// naming that login, and one billing record observed at `logAt`. Returns the
// home and the fingerprint the live cache is keyed by.
func grokFreshnessHome(t *testing.T, email string, logAt time.Time) (string, string) {
	t.Helper()
	home := t.TempDir()
	helperGrokScopedAuth(t, home, map[string]any{
		"key":        unsignedJWT(t, map[string]any{"email": email, "sub": "user-1"}),
		"email":      email,
		"expires_at": time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339),
	})
	if err := appendGrokBillingIdentity(home); err != nil {
		t.Fatal(err)
	}
	appendGrokBillingRecordAt(t, home, logAt)
	account, _ := readGrokAccountAndPlan(home)
	return home, fingerprintAccount("grok", account)
}

func grokFreshnessLive(t *testing.T, fingerprint string, at time.Time) {
	t.Helper()
	t.Setenv("AIEXPEDITE_GROK_BILLING_LIVE_CACHE", filepath.Join(t.TempDir(), "grok_billing_live.json"))
	if !saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
		ObservedAt: at, PeriodType: "USAGE_PERIOD_TYPE_WEEKLY", UsedPercent: 42, HasUsedPercent: true,
	}, fingerprint)) {
		t.Fatal("live cache not saved")
	}
}

func TestGrokNewestBillingObservation_MergedSmokeRecordNewerThanLiveCacheWins(t *testing.T) {
	logAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	home, fingerprint := grokFreshnessHome(t, "ada@example.com", logAt)
	grokFreshnessLive(t, fingerprint, logAt.Add(-time.Hour))

	snap, ok, _ := grokNewestBillingObservation(home, fingerprint, logAt)
	if !ok || !snap.ObservedAt.Equal(logAt) {
		t.Fatalf("observation = %+v ok=%t, want the newer log record at %s", snap, ok, logAt)
	}
	if snap.SubscriptionTier != "SuperGrok" {
		t.Errorf("log record's tier not carried: %+v", snap)
	}
}

func TestGrokNewestBillingObservation_LiveCacheNewerThanLogWinsAndKeepsTheTier(t *testing.T) {
	logAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	home, fingerprint := grokFreshnessHome(t, "ada@example.com", logAt)
	liveAt := logAt.Add(time.Hour)
	grokFreshnessLive(t, fingerprint, liveAt)

	snap, ok, _ := grokNewestBillingObservation(home, fingerprint, logAt)
	if !ok || !snap.ObservedAt.Equal(liveAt) {
		t.Fatalf("observation = %+v ok=%t, want the newer live reading at %s", snap, ok, liveAt)
	}
	if snap.UsedPercent != 42 || snap.SubscriptionTier != "SuperGrok" {
		t.Errorf("live reading must carry its own percentage and the log's tier: %+v", snap)
	}
}

// The account-fingerprint gate overrides recency in BOTH directions: a newer
// log record produced by another account is refused (and does not fall back
// to an older matching one), and a live entry cached under another account's
// fingerprint is never replayed.
func TestGrokNewestBillingObservation_ForeignSourcesNeverAgeTheObservationForward(t *testing.T) {
	logAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	home, fingerprint := grokFreshnessHome(t, "ada@example.com", logAt)

	// A newer record produced by someone else, logged after our identity.
	if err := appendGrokBillingIdentityValue(home, "intruder@example.com"); err != nil {
		t.Fatal(err)
	}
	appendGrokBillingRecordAt(t, home, logAt.Add(2*time.Hour))
	// And a newer live reading cached for someone else.
	grokFreshnessLive(t, fingerprintAccount("grok", "intruder@example.com"), logAt.Add(3*time.Hour))

	if snap, ok, _ := grokNewestBillingObservation(home, fingerprint, logAt); ok {
		t.Fatalf("foreign sources produced an observation for the current account: %+v", snap)
	}

	// Our own live reading, older than the foreign record, is still the answer
	// for our account — recency never lets the foreign record through.
	grokFreshnessLive(t, fingerprint, logAt.Add(time.Hour))
	snap, ok, _ := grokNewestBillingObservation(home, fingerprint, logAt)
	if !ok || !snap.ObservedAt.Equal(logAt.Add(time.Hour)) {
		t.Fatalf("observation = %+v ok=%t, want our own live reading", snap, ok)
	}
}

/* --------------------------------------------------------------------------
   Same-period numeric preference
   --------------------------------------------------------------------------
   Grok ≥ 1.0.40 logs percent-less records, so a newer unmetered record used to
   push the live read's number off the card. Inside the same OPEN period the
   number wins with its own observedAt, and a refresh is owed. Across a
   rollover the newer record still wins.
   ------------------------------------------------------------------------ */

// grokSamePeriodEnd is a period end as Grok's log writes it (microseconds).
const grokSamePeriodEnd = "2026-09-24T22:28:32.746607+00:00"

// appendGrokUnmeteredRecordAt appends a percent-less (Grok ≥ 1.0.40) record
// naming a weekly period that ends at end.
func appendGrokUnmeteredRecordAt(t *testing.T, home string, observedAt time.Time, end string) {
	t.Helper()
	f, err := os.OpenFile(grokBillingLogPath(home), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, `{"ts":%q,"msg":"billing: fetched credits config","ctx":{"config":{`+
		`"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":%q}},"subscriptionTier":"SuperGrok"}}`+"\n",
		observedAt.UTC().Format(time.RFC3339Nano), end); err != nil {
		t.Fatal(err)
	}
}

// grokFreshnessLiveInPeriod caches a numeric live reading for a weekly period
// ending at end (stored, as production does, to the second).
func grokFreshnessLiveInPeriod(t *testing.T, fingerprint string, at time.Time, end string) {
	t.Helper()
	t.Setenv("AIEXPEDITE_GROK_BILLING_LIVE_CACHE", filepath.Join(t.TempDir(), "grok_billing_live.json"))
	periodEnd, err := time.Parse(time.RFC3339Nano, end)
	if err != nil {
		t.Fatal(err)
	}
	if !saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
		ObservedAt: at, PeriodType: "USAGE_PERIOD_TYPE_WEEKLY", UsedPercent: 36, HasUsedPercent: true,
		PeriodEnd: periodEnd, HasPeriodEnd: true,
	}, fingerprint)) {
		t.Fatal("live cache not saved")
	}
}

func TestGrokNewestBillingObservation_NumericBeatsNewerUnmeteredInTheSameOpenPeriod(t *testing.T) {
	logAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	home, fingerprint := grokFreshnessHome(t, "ada@example.com", logAt.Add(-48*time.Hour))
	liveAt := logAt.Add(-time.Hour)
	grokFreshnessLiveInPeriod(t, fingerprint, liveAt, grokSamePeriodEnd)
	appendGrokUnmeteredRecordAt(t, home, logAt, grokSamePeriodEnd)

	now := logAt.Add(time.Minute)
	snap, ok, owes := grokNewestBillingObservation(home, fingerprint, now)
	if !ok || !snap.HasUsedPercent || snap.UsedPercent != 36 {
		t.Fatalf("observation = %+v ok=%t, want the numeric live reading", snap, ok)
	}
	if !snap.ObservedAt.Equal(liveAt) {
		t.Errorf("ObservedAt = %s, want the live reading's own %s", snap.ObservedAt, liveAt)
	}
	if snap.SubscriptionTier != "SuperGrok" {
		t.Errorf("tier not inherited from the log: %+v", snap)
	}
	if !owes {
		t.Error("a newer unmetered record must owe a refresh")
	}
	metrics := grokBillingMetrics(snap, now)
	if len(metrics) != 1 || metrics[0].Unknown || metrics[0].ObservedAt != liveAt.Format(time.RFC3339) {
		t.Fatalf("card row = %+v, want the numeric reading with its own observedAt", metrics)
	}
}

func TestGrokBillingSnapshotSamePeriod_SecondsMatchMicroseconds(t *testing.T) {
	micro, _ := time.Parse(time.RFC3339Nano, grokSamePeriodEnd)
	seconds, _ := time.Parse(time.RFC3339, "2026-09-24T22:28:32Z")
	a := grokBillingSnapshot{PeriodType: "USAGE_PERIOD_TYPE_WEEKLY", PeriodEnd: micro, HasPeriodEnd: true}
	b := grokBillingSnapshot{PeriodType: "USAGE_PERIOD_TYPE_WEEKLY", PeriodEnd: seconds, HasPeriodEnd: true}
	if !a.samePeriod(b) || !b.samePeriod(a) {
		t.Fatal("the same instant at second and microsecond precision must be the same period")
	}
	c := b
	c.PeriodEnd = seconds.Add(time.Second)
	if a.samePeriod(c) {
		t.Fatal("a different end is a different period")
	}
	d := b
	d.PeriodType = "USAGE_PERIOD_TYPE_DAILY"
	if a.samePeriod(d) {
		t.Fatal("a different period type is a different period")
	}
	if (grokBillingSnapshot{}).samePeriod(grokBillingSnapshot{}) {
		t.Fatal("two unnamed periods are not the same period")
	}
}

func TestGrokNewestBillingObservation_NewerUnmeteredWinsAcrossARollover(t *testing.T) {
	logAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	t.Run("different period end", func(t *testing.T) {
		home, fingerprint := grokFreshnessHome(t, "ada@example.com", logAt.Add(-48*time.Hour))
		grokFreshnessLiveInPeriod(t, fingerprint, logAt.Add(-time.Hour), grokSamePeriodEnd)
		appendGrokUnmeteredRecordAt(t, home, logAt, "2026-10-01T22:28:32.746607+00:00")
		snap, ok, _ := grokNewestBillingObservation(home, fingerprint, logAt.Add(time.Minute))
		if !ok || snap.HasUsedPercent || !snap.ObservedAt.Equal(logAt) {
			t.Fatalf("observation = %+v, want the newer unmetered record of the new period", snap)
		}
	})
	t.Run("period has ended", func(t *testing.T) {
		home, fingerprint := grokFreshnessHome(t, "ada@example.com", logAt.Add(-48*time.Hour))
		grokFreshnessLiveInPeriod(t, fingerprint, logAt.Add(-time.Hour), grokSamePeriodEnd)
		appendGrokUnmeteredRecordAt(t, home, logAt, grokSamePeriodEnd)
		end, _ := time.Parse(time.RFC3339Nano, grokSamePeriodEnd)
		snap, ok, _ := grokNewestBillingObservation(home, fingerprint, end.Add(time.Minute))
		if !ok || snap.HasUsedPercent || !snap.ObservedAt.Equal(logAt) {
			t.Fatalf("observation = %+v, want the newer record once the period ended", snap)
		}
	})
}

func TestGrokNewestBillingObservation_NewerNumericLogRecordStillWins(t *testing.T) {
	logAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	home, fingerprint := grokFreshnessHome(t, "ada@example.com", logAt)
	grokFreshnessLiveInPeriod(t, fingerprint, logAt.Add(-time.Hour), grokSamePeriodEnd)

	snap, ok, owes := grokNewestBillingObservation(home, fingerprint, logAt.Add(time.Minute))
	if !ok || !snap.ObservedAt.Equal(logAt) || !snap.HasUsedPercent {
		t.Fatalf("observation = %+v, want the newer numeric log record", snap)
	}
	if owes {
		t.Error("a numeric record owes no refresh")
	}
}

// The live endpoint can answer with a period and no percentage. A NEWER
// percent-less live reading must not outrank an older numeric log record for
// the same open period either.
func TestGrokNewestBillingObservation_NumericLogBeatsNewerUnmeteredLiveInTheSamePeriod(t *testing.T) {
	logAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	home, fingerprint := grokFreshnessHome(t, "ada@example.com", logAt)
	logSnap, ok := readGrokBillingSnapshot(home, grokIdentityCandidates(home))
	if !ok || !logSnap.HasUsedPercent || !logSnap.HasPeriodEnd {
		t.Fatalf("fixture log record = %+v", logSnap)
	}
	t.Setenv("AIEXPEDITE_GROK_BILLING_LIVE_CACHE", filepath.Join(t.TempDir(), "grok_billing_live.json"))
	liveAt := logAt.Add(time.Hour)
	if !saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
		ObservedAt: liveAt, PeriodType: logSnap.PeriodType, PeriodEnd: logSnap.PeriodEnd, HasPeriodEnd: true,
	}, fingerprint)) {
		t.Fatal("live cache not saved")
	}

	snap, ok, _ := grokNewestBillingObservation(home, fingerprint, liveAt.Add(time.Minute))
	if !ok || !snap.HasUsedPercent || !snap.ObservedAt.Equal(logAt) {
		t.Fatalf("observation = %+v, want the numeric log record with its own observedAt", snap)
	}

	// Once the period has ended, the newer reading wins as before.
	snap, _, _ = grokNewestBillingObservation(home, fingerprint, logSnap.PeriodEnd.Add(time.Minute))
	if snap.HasUsedPercent || !snap.ObservedAt.Equal(liveAt) {
		t.Fatalf("observation after the period = %+v, want the newer live reading", snap)
	}
}

// The live cache can keep a percentage from t1 beneath an unmetered answer at
// t3 that carried a fresh on-demand pool. A numeric log record from t2 wins
// the credits row, but the t3 pool still publishes: the pools are compared by
// their own times. The log's percentage was never versioned by the live
// generation, so the merged reading publishes unversioned.
func TestGrokNewestBillingObservation_ANewerLiveOnDemandPoolRidesANewerLogPercentage(t *testing.T) {
	withCodexGenerationEpoch(t, 8120)
	logAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	home, fingerprint := grokFreshnessHome(t, "ada@example.com", logAt)
	logSnap, ok := readGrokBillingSnapshot(home, grokIdentityCandidates(home))
	if !ok || !logSnap.HasUsedPercent || !logSnap.HasPeriodEnd {
		t.Fatalf("fixture log record = %+v", logSnap)
	}
	t.Setenv("AIEXPEDITE_GROK_BILLING_LIVE_CACHE", filepath.Join(t.TempDir(), "grok_billing_live.json"))
	keptAt, poolAt := logAt.Add(-time.Hour), logAt.Add(time.Hour)
	saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
		ObservedAt: keptAt, PeriodType: logSnap.PeriodType, PeriodEnd: logSnap.PeriodEnd, HasPeriodEnd: true,
		UsedPercent: 36, HasUsedPercent: true,
	}, fingerprint))
	saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
		ObservedAt: poolAt, PeriodType: logSnap.PeriodType, PeriodEnd: logSnap.PeriodEnd, HasPeriodEnd: true,
		OnDemandCap: 50, OnDemandUsed: 12, HasOnDemand: true, HasOnDemandUsed: true,
	}, fingerprint))

	now := poolAt.Add(time.Minute)
	obs := grokBillingObservationFor(home, fingerprint, now)
	if obs.source != grokObservationLog || obs.generation != nil || obs.cleared {
		t.Fatalf("source=%d generation=%+v cleared=%v, want an unversioned log-led reading", obs.source, obs.generation, obs.cleared)
	}
	snap := obs.snap
	if !snap.HasUsedPercent || snap.UsedPercent != logSnap.UsedPercent || !snap.ObservedAt.Equal(logAt) {
		t.Fatalf("credits = %+v, want the newer log percentage at its own time", snap)
	}
	if !snap.HasOnDemandUsed || snap.OnDemandUsed != 12 || !snap.OnDemandObservedAt.Equal(poolAt) {
		t.Fatalf("on-demand = %+v, want the live pool at its own newer time", snap)
	}

	// A log record newer than the live pool keeps its own pool and no generation.
	appendGrokBillingRecordAt(t, home, poolAt.Add(time.Minute))
	obs = grokBillingObservationFor(home, fingerprint, poolAt.Add(2*time.Minute))
	if obs.source != grokObservationLog || obs.snap.OnDemandUsed == 12 {
		t.Fatalf("observation = %+v source=%d, want the newest log record whole", obs.snap, obs.source)
	}
}

// The reverse direction: a kept live percentage wins the credits row over a
// newer percent-less log record, but that record's fresher on-demand pool
// still publishes, at its own time. The merged reading holds log content the
// live generation never versioned, so it publishes unversioned.
func TestGrokNewestBillingObservation_ANewerLogOnDemandPoolRidesAKeptLivePercentage(t *testing.T) {
	withCodexGenerationEpoch(t, 8121)
	logAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	home, fingerprint := grokFreshnessHome(t, "ada@example.com", logAt)
	logSnap, ok := readGrokBillingSnapshot(home, grokIdentityCandidates(home))
	if !ok || !logSnap.HasPeriodEnd {
		t.Fatalf("fixture log record = %+v", logSnap)
	}
	t.Setenv("AIEXPEDITE_GROK_BILLING_LIVE_CACHE", filepath.Join(t.TempDir(), "grok_billing_live.json"))
	liveAt, poolAt := logAt.Add(time.Hour), logAt.Add(2*time.Hour)
	saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
		ObservedAt: liveAt, PeriodType: logSnap.PeriodType, PeriodEnd: logSnap.PeriodEnd, HasPeriodEnd: true,
		UsedPercent: 36, HasUsedPercent: true,
		OnDemandCap: 50, OnDemandUsed: 5, HasOnDemand: true, HasOnDemandUsed: true,
	}, fingerprint))
	f, err := os.OpenFile(grokBillingLogPath(home), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(f, `{"ts":%q,"msg":"billing: fetched credits config","ctx":{"config":{`+
		`"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":%q,"end":%q},"onDemandCap":{"val":50},"onDemandUsed":{"val":20}},"subscriptionTier":"SuperGrok"}}`+"\n",
		poolAt.UTC().Format(time.RFC3339Nano), logAt.Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		logAt.Add(7*24*time.Hour).UTC().Format(time.RFC3339Nano))
	f.Close()
	if err != nil {
		t.Fatal(err)
	}

	obs := grokBillingObservationFor(home, fingerprint, poolAt.Add(time.Minute))
	if obs.source != grokObservationLive || obs.generation != nil || obs.cleared {
		t.Fatalf("source=%d generation=%+v cleared=%v, want an unversioned merged reading", obs.source, obs.generation, obs.cleared)
	}
	snap := obs.snap
	if !snap.HasUsedPercent || snap.UsedPercent != 36 || !snap.ObservedAt.Equal(liveAt) {
		t.Fatalf("credits = %+v, want the kept live percentage at its own time", snap)
	}
	if !snap.HasOnDemandUsed || snap.OnDemandUsed != 20 || !snap.OnDemandObservedAt.Equal(poolAt) {
		t.Fatalf("on-demand = %+v, want the newer log pool at its own time", snap)
	}

	// A live pool newer than the log record's keeps the live pool.
	saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
		ObservedAt: poolAt.Add(time.Minute), PeriodType: logSnap.PeriodType, PeriodEnd: logSnap.PeriodEnd, HasPeriodEnd: true,
		UsedPercent: 37, HasUsedPercent: true,
		OnDemandCap: 50, OnDemandUsed: 25, HasOnDemand: true, HasOnDemandUsed: true,
	}, fingerprint))
	obs = grokBillingObservationFor(home, fingerprint, poolAt.Add(2*time.Minute))
	if obs.source != grokObservationLive || obs.snap.OnDemandUsed != 25 || obs.snap.UsedPercent != 37 {
		t.Fatalf("observation = %+v source=%d, want the newest live reading whole", obs.snap, obs.source)
	}
	if obs.generation == nil || *obs.generation != gen(8121, 2) {
		t.Fatalf("generation = %+v, want the live commit's own {8121,2}", obs.generation)
	}
}

// A newer live answer that omitted the on-demand pool dates that removal: an
// older pool still in the log is never merged back over it.
func TestGrokNewestBillingObservation_ANewerLiveRemovalIsNotRefilledByAnOlderLogPool(t *testing.T) {
	withCodexGenerationEpoch(t, 8122)
	logAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	home, fingerprint := grokFreshnessHome(t, "ada@example.com", logAt)
	logSnap, ok := readGrokBillingSnapshot(home, grokIdentityCandidates(home))
	if !ok || !logSnap.HasPeriodEnd {
		t.Fatalf("fixture log record = %+v", logSnap)
	}
	t.Setenv("AIEXPEDITE_GROK_BILLING_LIVE_CACHE", filepath.Join(t.TempDir(), "grok_billing_live.json"))
	liveAt, poolAt, dropAt := logAt.Add(time.Hour), logAt.Add(2*time.Hour), logAt.Add(3*time.Hour)
	saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
		ObservedAt: liveAt, PeriodType: logSnap.PeriodType, PeriodEnd: logSnap.PeriodEnd, HasPeriodEnd: true,
		UsedPercent: 36, HasUsedPercent: true,
		OnDemandCap: 50, OnDemandUsed: 5, HasOnDemand: true, HasOnDemandUsed: true,
	}, fingerprint))
	f, err := os.OpenFile(grokBillingLogPath(home), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(f, `{"ts":%q,"msg":"billing: fetched credits config","ctx":{"config":{`+
		`"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":%q,"end":%q},"onDemandCap":{"val":50},"onDemandUsed":{"val":20}},"subscriptionTier":"SuperGrok"}}`+"\n",
		poolAt.UTC().Format(time.RFC3339Nano), logAt.Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		logAt.Add(7*24*time.Hour).UTC().Format(time.RFC3339Nano))
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	// The newest answer is unmetered and carries no pool: the percentage is
	// kept, the pool's removal is versioned.
	saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
		ObservedAt: dropAt, PeriodType: logSnap.PeriodType, PeriodEnd: logSnap.PeriodEnd, HasPeriodEnd: true,
	}, fingerprint))

	obs := grokBillingObservationFor(home, fingerprint, dropAt.Add(time.Minute))
	if obs.source != grokObservationLive || obs.generation == nil || *obs.generation != gen(8122, 2) {
		t.Fatalf("source=%d generation=%+v, want the removal's live generation {8122,2}", obs.source, obs.generation)
	}
	if snap := obs.snap; !snap.HasUsedPercent || snap.UsedPercent != 36 || snap.HasOnDemand {
		t.Fatalf("observation = %+v, want the kept percentage with the pool removed", snap)
	}
	for _, m := range grokBillingMetrics(obs.snap, dropAt.Add(time.Minute)) {
		if m.Label == "On-demand credits" {
			t.Fatalf("a removed pool still renders: %+v", m)
		}
	}
}

// A live cache keeps a percentage from t1 beneath an unmetered answer at t3
// that removed the on-demand pool; a numeric log record at t2 with no pool
// wins the credits row. Neither final snapshot holds a pool, yet the t3
// removal is still the newest pool state, so the reading carries it. The
// log's percentage was never versioned by that generation, so the reading
// publishes unversioned.
func TestGrokNewestBillingObservation_ANewerLiveRemovalRidesANewerLogPercentage(t *testing.T) {
	withCodexGenerationEpoch(t, 8123)
	logAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	home, fingerprint := grokFreshnessHome(t, "ada@example.com", logAt)
	logSnap, ok := readGrokBillingSnapshot(home, grokIdentityCandidates(home))
	if !ok || !logSnap.HasUsedPercent || logSnap.HasOnDemand || !logSnap.HasPeriodEnd {
		t.Fatalf("fixture log record = %+v", logSnap)
	}
	t.Setenv("AIEXPEDITE_GROK_BILLING_LIVE_CACHE", filepath.Join(t.TempDir(), "grok_billing_live.json"))
	keptAt, dropAt := logAt.Add(-time.Hour), logAt.Add(time.Hour)
	saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
		ObservedAt: keptAt, PeriodType: logSnap.PeriodType, PeriodEnd: logSnap.PeriodEnd, HasPeriodEnd: true,
		UsedPercent: 36, HasUsedPercent: true,
		OnDemandCap: 50, OnDemandUsed: 5, HasOnDemand: true, HasOnDemandUsed: true,
	}, fingerprint))
	saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
		ObservedAt: dropAt, PeriodType: logSnap.PeriodType, PeriodEnd: logSnap.PeriodEnd, HasPeriodEnd: true,
	}, fingerprint))

	obs := grokBillingObservationFor(home, fingerprint, dropAt.Add(time.Minute))
	if obs.source != grokObservationLog || obs.generation != nil || obs.cleared {
		t.Fatalf("source=%d generation=%+v cleared=%v, want an unversioned log-led reading", obs.source, obs.generation, obs.cleared)
	}
	snap := obs.snap
	if !snap.HasUsedPercent || snap.UsedPercent != logSnap.UsedPercent || !snap.ObservedAt.Equal(logAt) {
		t.Fatalf("credits = %+v, want the newer log percentage at its own time", snap)
	}
	if snap.HasOnDemand || !snap.OnDemandObservedAt.Equal(dropAt) {
		t.Fatalf("on-demand = %+v, want the live removal at its own newer time", snap)
	}

	// A log record newer than the live removal keeps its own row, with no
	// generation riding along.
	appendGrokBillingRecordAt(t, home, dropAt.Add(time.Minute))
	obs = grokBillingObservationFor(home, fingerprint, dropAt.Add(2*time.Minute))
	if obs.source != grokObservationLog {
		t.Fatalf("observation = %+v source=%d, want the newest log record whole", obs.snap, obs.source)
	}
}

// A pool stamped beyond the clock-skew limit (the clock stepped back) is
// neither rendered under the percentage's older stamp nor counted as covering
// a run; the trusted percentage still is.
func TestGrokBillingMetrics_AFutureOnDemandPoolIsSuppressed(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	snap := grokBillingSnapshot{
		ObservedAt: now.Add(-time.Hour), PeriodType: "USAGE_PERIOD_TYPE_WEEKLY",
		PeriodEnd: now.Add(48 * time.Hour), HasPeriodEnd: true,
		UsedPercent: 36, HasUsedPercent: true,
		OnDemandCap: 50, OnDemandUsed: 20, HasOnDemand: true, HasOnDemandUsed: true,
		OnDemandObservedAt: now.Add(time.Hour),
	}
	metrics := grokBillingMetrics(snap, now)
	if len(metrics) != 1 || metrics[0].Unknown {
		t.Fatalf("metrics = %+v, want the numeric credits row alone", metrics)
	}
	if grokSnapshotCovers(snap, now.Add(-time.Minute).UnixMilli(), now) {
		t.Fatal("a future pool covered a run the trusted percentage predates")
	}
	if !grokSnapshotCovers(snap, now.Add(-2*time.Hour).UnixMilli(), now) {
		t.Fatal("the trusted percentage stopped covering an older run")
	}

	// The same pool inside the skew limit renders at its own time.
	snap.OnDemandObservedAt = now.Add(time.Minute)
	metrics = grokBillingMetrics(snap, now)
	if len(metrics) != 2 || metrics[1].Unknown || metrics[1].ObservedAt != snap.OnDemandObservedAt.UTC().Format(time.RFC3339) {
		t.Fatalf("metrics = %+v, want the pool at its own time", metrics)
	}
	if !grokSnapshotCovers(snap, now.UnixMilli(), now) {
		t.Fatal("a trusted pool stopped covering a run")
	}
}

/* ------------------- completion-based nudge (direct runs) ------------------- */

// A nudge carries the turn's own completion: a numeric reading taken since
// pays the debt for free.
func TestGrokNudge_ACompletionBeforeTheNumericReadingIsCoveredWithNoRequest(t *testing.T) {
	h := newGrokDebtHarness(t)
	saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
		ObservedAt: h.clock(), PeriodType: "USAGE_PERIOD_TYPE_WEEKLY", UsedPercent: 12, HasUsedPercent: true,
	}, "fp-ada"))
	h.advance(5 * time.Second)
	if !nudgeGrokUsageRefreshFor(h.clock(), h.clock().Add(-10*time.Second), "fp-ada") {
		t.Fatal("the nudge must open the debt")
	}
	h.idle()
	if h.reads.Load() != 0 || h.state().owed() {
		t.Fatalf("reads = %d state=%+v, want the debt settled as covered", h.reads.Load(), h.state())
	}
	if got, _ := grokLastRefreshOutcome.Load().(string); got != "covered" {
		t.Fatalf("outcome = %q, want covered", got)
	}
}

// Markers arriving inside the cooldown, or while the budget is exhausted and
// cooling, send no extra read.
func TestGrokNudge_RepeatedMarkersInsideTheCooldownOrExhaustionSendNothing(t *testing.T) {
	h := newGrokDirectHarness(t)
	h.outcome.Store(grokLiveOutcomeHTTPError)
	h.appendRaw(grokTurnMarkerLine(h.clock().Add(-time.Second)))
	h.scan()
	for i := 0; i < 3; i++ {
		h.advance(10 * time.Second)
		h.appendRaw(grokTurnMarkerLine(h.clock().Add(-time.Second)))
		h.scan()
	}
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want one inside the cooldown", h.reads.Load())
	}
	updateGrokUsageFreshness(func(s *grokUsageFreshness) {
		s.Attempts, s.LastAttemptAtMs, s.NextAttemptAtMs = grokRunDebtMaxAttempts, h.clock().UnixMilli(), 0
	})
	for i := 0; i < 3; i++ {
		h.advance(2 * grokRefreshNudgeCooldown)
		h.appendRaw(grokTurnMarkerLine(h.clock().Add(-time.Second)))
		h.scan()
	}
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want none while the exhausted budget cools", h.reads.Load())
	}
}

// A debt an auth refusal idled is not re-opened by markers: the auth notice
// owns it until a run settles or the user clicks Refresh.
func TestGrokNudge_AnAuthIdledDebtIsNotReopenedByMarkers(t *testing.T) {
	h := newGrokDirectHarness(t)
	h.outcome.Store(grokLiveOutcomeUnauthorized)
	h.appendRaw(grokTurnMarkerLine(h.clock().Add(-time.Second)))
	h.scan()
	if h.reads.Load() != 1 || h.state().NextAttemptAtMs != 0 {
		t.Fatalf("reads = %d state=%+v, want one read and an idle debt", h.reads.Load(), h.state())
	}
	for i := 0; i < 3; i++ {
		h.advance(2 * grokRefreshNudgeCooldown)
		h.appendRaw(grokTurnMarkerLine(h.clock().Add(-time.Second)))
		h.scan()
	}
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want markers never to re-open an auth-idled debt", h.reads.Load())
	}
}

/* ------------------- cross-process claim of a direct turn ------------------- */

// A debt another agent process already opened for the same account (written
// to the shared freshness file, its worker on it) is claimed: this process's
// nudge for the same turn sends no read of its own.
func TestGrokNudge_ADebtAnotherProcessOpenedIsNotReadTwice(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.write(grokUsageFreshness{
		OwedAtMs: h.clock().UnixMilli(), CompletionMs: h.clock().Add(-time.Second).UnixMilli(), AccountFingerprint: "fp-ada",
	})
	if nudgeGrokUsageRefreshFor(h.clock(), h.clock().Add(-time.Second), "fp-ada") {
		t.Fatal("want the nudge to find the debt claimed")
	}
	h.idle()
	if h.reads.Load() != 0 {
		t.Fatalf("reads = %d, want none for a debt another process owns", h.reads.Load())
	}
}

// A due rung is claimed by whichever start commits first: the nudge clears it
// in the same write, so the rung's timer (here or in another process) firing
// afterwards sends no second read.
func TestGrokNudge_ADueRungIsClaimedOnce(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.outcome.Store(grokLiveOutcomeHTTPError)
	if !nudgeGrokUsageRefreshFor(h.clock(), h.clock().Add(-time.Second), "fp-ada") {
		t.Fatal("the nudge must open the debt")
	}
	h.idle()
	if h.reads.Load() != 1 || h.state().NextAttemptAtMs == 0 {
		t.Fatalf("reads = %d state=%+v, want one read and a booked rung", h.reads.Load(), h.state())
	}
	stopGrokRunDebtRetry()
	id := h.state().debtID()
	h.advance(10 * time.Minute)
	h.block = make(chan struct{})
	if !nudgeGrokUsageRefreshFor(h.clock(), h.clock().Add(-time.Second), "fp-ada") {
		t.Fatal("want the nudge to start the due rung")
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.reads.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := h.state().NextAttemptAtMs; got != 0 {
		t.Fatalf("NextAttemptAtMs = %d, want the rung claimed by the nudge", got)
	}
	grokRunDebtRetryFired(func() uint64 {
		grokRunDebtRetryTimer.mu.Lock()
		defer grokRunDebtRetryTimer.mu.Unlock()
		return grokRunDebtRetryTimer.gen
	}(), id)
	grokRefreshWorkerMu.Lock()
	rearmed := grokRefreshWorkerRearm
	grokRefreshWorkerMu.Unlock()
	close(h.block)
	h.idle()
	if rearmed || h.reads.Load() != 2 {
		t.Fatalf("reads = %d rearmed=%t, want the claimed rung paid once", h.reads.Load(), rearmed)
	}
}

// Another agent process holding the freshness file's sibling lock refuses the
// transaction: the nudge opens nothing and sends no read, and the same turn
// is owed once the holder releases.
func TestGrokNudge_AnotherProcessHoldingTheFreshnessLockDefersTheNudge(t *testing.T) {
	h := newGrokDebtHarness(t)
	prevWait := grokUsageFreshnessLockWait
	grokUsageFreshnessLockWait = 20 * time.Millisecond
	t.Cleanup(func() { grokUsageFreshnessLockWait = prevWait })
	if err := os.MkdirAll(filepath.Dir(grokUsageFreshnessPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(grokUsageFreshnessPath()+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockFileExclusive(held); err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = unlockFile(held)
			_ = held.Close()
		}
	}
	t.Cleanup(release)

	if nudgeGrokUsageRefreshFor(h.clock(), h.clock().Add(-time.Second), "fp-ada") {
		t.Fatal("want the nudge refused while another process holds the lock")
	}
	h.idle()
	if h.reads.Load() != 0 || h.state().owed() {
		t.Fatalf("reads = %d state=%+v, want nothing opened while locked", h.reads.Load(), h.state())
	}

	release()
	if !nudgeGrokUsageRefreshFor(h.clock(), h.clock().Add(-time.Second), "fp-ada") {
		t.Fatal("want the nudge to open the debt once released")
	}
	h.idle()
	if h.reads.Load() != 1 || h.state().owed() {
		t.Fatalf("reads = %d state=%+v, want one read that paid the debt", h.reads.Load(), h.state())
	}
}

// holdGrokFreshnessLock takes the freshness file's sibling lock as another
// agent process would, with the transaction's wait and the refused-mutation
// retry pinned short. The returned func releases it (also run at cleanup).
func holdGrokFreshnessLock(t *testing.T) func() {
	t.Helper()
	prevWait, prevDelay := grokUsageFreshnessLockWait, grokFreshnessRefusedRetryDelay
	grokUsageFreshnessLockWait, grokFreshnessRefusedRetryDelay = 20*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { grokUsageFreshnessLockWait, grokFreshnessRefusedRetryDelay = prevWait, prevDelay })
	if err := os.MkdirAll(filepath.Dir(grokUsageFreshnessPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(grokUsageFreshnessPath()+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockFileExclusive(held); err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = unlockFile(held)
			_ = held.Close()
		}
	}
	t.Cleanup(release)
	return release
}

// A settle refused by another process's lock has already released its run's
// floor, so it retries the debt until it lands and then pays it — never
// leaving the run with neither a persisted debt nor a worker.
func TestGrokRunDebt_ASettleRefusedByTheFreshnessLockOpensTheDebtOnceReleased(t *testing.T) {
	h := newGrokDebtHarness(t)
	release := holdGrokFreshnessLock(t)
	floor := armGrokUsageRunFloor(h.clock())
	h.advance(time.Second)
	if !grokUsageRunSettled(floor) {
		t.Fatal("want the settled run reported owed")
	}
	if h.reads.Load() != 0 || h.state().owed() {
		t.Fatalf("reads = %d state=%+v, want nothing written while locked", h.reads.Load(), h.state())
	}
	release()
	h.idle()
	if h.reads.Load() != 1 || h.state().owed() {
		t.Fatalf("reads = %d state=%+v, want the retried debt opened and paid once", h.reads.Load(), h.state())
	}
}

// A settle the lock refused, interrupted by shutdown while its retry waits,
// still persists its debt: the update hand-off is exactly what the retry
// exists to survive, and the run's in-memory floor is already released, so
// dropping the mutation would leave the next process nothing to adopt. The
// dying process starts no read for it.
func TestGrokRunDebt_ASettleRefusedByTheFreshnessLockPersistsAtShutdown(t *testing.T) {
	h := newGrokDebtHarness(t)
	release := holdGrokFreshnessLock(t)
	// Long enough that the retry is still waiting when shutdown begins; the
	// wait's slices are what notice it.
	prevDelay := grokFreshnessRefusedRetryDelay
	grokFreshnessRefusedRetryDelay = 2 * time.Second
	t.Cleanup(func() { grokFreshnessRefusedRetryDelay = prevDelay })

	floor := armGrokUsageRunFloor(h.clock())
	h.advance(time.Second)
	if !grokUsageRunSettled(floor) {
		t.Fatal("want the settled run reported owed")
	}
	if h.state().owed() {
		t.Fatalf("state=%+v, want nothing written while locked", h.state())
	}

	shutdownInProgress.Store(true)
	t.Cleanup(func() { shutdownInProgress.Store(false) })
	release()
	h.idle()
	if !h.state().owed() {
		t.Fatalf("state=%+v, want the debt persisted for the next process to adopt", h.state())
	}
	if h.reads.Load() != 0 {
		t.Fatalf("reads = %d, want no read started by a process that is exiting", h.reads.Load())
	}
}

// A rung booking refused by another process's lock is retried until it lands,
// then armed, so a failed read is never left with no retry.
func TestGrokRunDebt_ARungRefusedByTheFreshnessLockIsBookedOnceReleased(t *testing.T) {
	h := newGrokDebtHarness(t)
	now := h.clock()
	h.write(grokUsageFreshness{
		OwedAtMs: now.UnixMilli(), CompletionMs: now.UnixMilli(), Attempts: 1,
		LastAttemptAtMs: now.UnixMilli(), AccountFingerprint: "fp-ada",
	})
	release := holdGrokFreshnessLock(t)
	if !grokScheduleRunDebtRetry(h.state(), now, grokRetryAfterRead) {
		t.Fatal("want the refused booking handed to the retry")
	}
	if h.state().NextAttemptAtMs != 0 || grokRunDebtRetryPending() {
		t.Fatalf("state=%+v, want no rung booked while locked", h.state())
	}
	release()
	h.idle()
	if h.state().NextAttemptAtMs == 0 || !grokRunDebtRetryPending() {
		t.Fatalf("state=%+v pending=%t, want the rung booked and armed once released", h.state(), grokRunDebtRetryPending())
	}
}

// A settle refused by the lock stands down when another account signed in and
// opened a newer debt before the retry landed: that debt is kept, not replaced
// unpaid by one for the stale account that its worker could only retire.
func TestGrokRunDebt_ARefusedSettleKeepsANewerDebtFromAnotherAccount(t *testing.T) {
	h := newGrokDebtHarness(t)
	release := holdGrokFreshnessLock(t)
	floor := armGrokUsageRunFloor(h.clock())
	h.advance(time.Second)
	if !grokUsageRunSettled(floor) {
		t.Fatal("want the settled run reported owed")
	}
	// While the lock is held the other process signs in as bob and commits
	// bob's run debt, newer than ada's settle.
	h.signIn("fp-bob")
	h.advance(time.Second)
	bob := grokUsageFreshness{
		SchemaVersion: grokUsageFreshnessSchema, OwedAtMs: h.clock().UnixMilli(),
		CompletionMs: h.clock().UnixMilli(), AccountFingerprint: "fp-bob",
	}
	if !writeJSONFileAtomic(grokUsageFreshnessPath(), bob) {
		t.Fatal("write bob debt")
	}
	release()
	h.idle()
	got := h.state()
	if h.reads.Load() != 0 || !got.owed() || got.AccountFingerprint != "fp-bob" || got.OwedAtMs != bob.OwedAtMs {
		t.Fatalf("reads = %d state=%+v, want bob's debt kept and no read for ada", h.reads.Load(), got)
	}
}

// A due rung whose claim the lock refuses is retried until it lands, so the
// debt is still paid even though this process's timer is already spent.
func TestGrokRunDebt_ARungClaimRefusedByTheFreshnessLockIsPaidOnceReleased(t *testing.T) {
	h := newGrokDebtHarness(t)
	now := h.clock()
	h.write(grokUsageFreshness{
		OwedAtMs: now.UnixMilli(), CompletionMs: now.UnixMilli(), Attempts: 1,
		LastAttemptAtMs: now.Add(-2 * time.Minute).UnixMilli(), NextAttemptAtMs: now.UnixMilli(), AccountFingerprint: "fp-ada",
	})
	id := h.state().debtID()
	release := holdGrokFreshnessLock(t)
	grokRunDebtRetryFired(func() uint64 {
		grokRunDebtRetryTimer.mu.Lock()
		defer grokRunDebtRetryTimer.mu.Unlock()
		return grokRunDebtRetryTimer.gen
	}(), id)
	if h.reads.Load() != 0 || h.state().NextAttemptAtMs == 0 {
		t.Fatalf("reads = %d state=%+v, want the rung unclaimed while locked", h.reads.Load(), h.state())
	}
	release()
	h.idle()
	if h.reads.Load() != 1 || h.state().owed() {
		t.Fatalf("reads = %d state=%+v, want the retried claim to pay the debt once", h.reads.Load(), h.state())
	}
}

// The charge for a finished read must land: when another process's lock
// refuses it, the attempt (and the rung the ladder owes) is retried until it
// does, so a failed billing request is never left uncharged — which would let
// the four-read budget be overspent.
func TestGrokRunDebt_AReadChargeRefusedByTheFreshnessLockLandsOnceReleased(t *testing.T) {
	h := newGrokDebtHarness(t)
	now := h.clock()
	h.write(grokUsageFreshness{
		OwedAtMs: now.UnixMilli(), CompletionMs: now.UnixMilli(), AccountFingerprint: "fp-ada",
	})
	h.outcome.Store(grokLiveOutcomeHTTPError)
	release := holdGrokFreshnessLock(t)

	if _, retry := grokPayRunDebtPass(false); retry != grokRetryNone {
		t.Fatalf("retry = %v, want nothing booked against an uncharged state", retry)
	}
	if h.reads.Load() != 1 || h.state().Attempts != 0 {
		t.Fatalf("reads = %d state=%+v, want the read sent and nothing charged while locked", h.reads.Load(), h.state())
	}

	release()
	h.idle()
	got := h.state()
	if got.Attempts != 1 || got.LastOutcome != grokLiveOutcomeHTTPError {
		t.Fatalf("state = %+v, want the read charged once released", got)
	}
	if got.NextAttemptAtMs == 0 || !grokRunDebtRetryPending() {
		t.Fatalf("state = %+v pending=%t, want the rung booked and armed with the charge", got, grokRunDebtRetryPending())
	}
}

// Startup adoption must land too: a run the previous process was cut off in
// leaves a floor with no debt, and the state on disk under another process's
// lock is not that adoption — so it is retried rather than read as "nothing
// owed", which would leave the run unpaid.
func TestGrokRunDebt_AnAdoptionRefusedByTheFreshnessLockIsPaidOnceReleased(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.write(grokUsageFreshness{
		RunFloorMs: h.clock().Add(-time.Minute).UnixMilli(), RunFloorAccount: "fp-ada",
	})
	release := holdGrokFreshnessLock(t)

	adoptAndPayOwedGrokRunDebt(h.clock())
	if h.reads.Load() != 0 || h.state().owed() {
		t.Fatalf("reads = %d state=%+v, want nothing adopted while locked", h.reads.Load(), h.state())
	}

	release()
	h.idle()
	if h.reads.Load() != 1 || h.state().owed() {
		t.Fatalf("reads = %d state=%+v, want the inherited run adopted and paid once", h.reads.Load(), h.state())
	}
}

// An arm the lock refused must still persist its floor: a run cut off before
// it settles would otherwise leave the replacement process nothing to adopt,
// and the shutdown drain runs before active sessions end, so the exit cannot
// repair it.
func TestGrokRunDebt_ARunFloorArmRefusedByTheFreshnessLockPersistsOnceReleased(t *testing.T) {
	h := newGrokDebtHarness(t)
	release := holdGrokFreshnessLock(t)

	floor := armGrokUsageRunFloor(h.clock())
	// The arm persists off the caller's goroutine, so the lock is held for
	// several of the transaction's waits to be sure its first attempt was
	// refused rather than merely late.
	time.Sleep(5 * grokUsageFreshnessLockWait)
	if got := h.state(); got.RunFloorMs != 0 {
		t.Fatalf("state = %+v, want no floor written while locked", got)
	}

	release()
	h.idle()
	got := h.state()
	if got.RunFloorMs != floor.UnixMilli() || got.RunFloorAccount != "fp-ada" {
		t.Fatalf("state = %+v, want the arm's floor persisted once released", got)
	}
}

// A retirement the lock refused must land as well: the read already paid the
// debt, but the charge that follows an `ok` books no rung, so a debt left
// owed is one a later direct turn's nudge cannot wake.
func TestGrokRunDebt_ARefusedRetirementOfAPaidDebtLandsOnceReleased(t *testing.T) {
	h := newGrokDebtHarness(t)
	now := h.clock()
	h.write(grokUsageFreshness{
		OwedAtMs: now.UnixMilli(), CompletionMs: now.UnixMilli(), Attempts: 1,
		LastAttemptAtMs: now.UnixMilli(), AccountFingerprint: "fp-ada",
	})
	release := holdGrokFreshnessLock(t)

	settleGrokRunFreshness(now.UnixMilli(), "fp-ada")
	if !h.state().owed() {
		t.Fatalf("state = %+v, want the debt untouched while locked", h.state())
	}

	release()
	h.idle()
	if got := h.state(); got.owed() {
		t.Fatalf("state = %+v, want the reading's retirement to land once released", got)
	}
}

// A debt adopted at startup carries no rung to claim, so its claim is
// persisted: a second agent process sharing the file finds the resumed debt
// taken and starts no second read for it.
func TestGrokRunDebt_AResumedDebtIsClaimedAcrossProcesses(t *testing.T) {
	h := newGrokDebtHarness(t)
	now := h.clock()
	h.block = make(chan struct{})
	h.write(grokUsageFreshness{
		OwedAtMs: now.UnixMilli(), CompletionMs: now.UnixMilli(), AccountFingerprint: "fp-ada",
	})
	state := h.state()

	grokResumeAdoptedDebt(state, now)
	claim := h.state().WorkerClaimAtMs
	if claim != now.UnixMilli() {
		t.Fatalf("state = %+v, want the resumed debt claimed at %d", h.state(), now.UnixMilli())
	}

	// The other process resumes the same debt a second later.
	h.advance(time.Second)
	grokResumeAdoptedDebt(state, h.clock())
	if got := h.state().WorkerClaimAtMs; got != claim {
		t.Fatalf("claim = %d, want the first process's claim %d kept", got, claim)
	}

	// A claim older than one payment pass is free again, so a process that
	// died mid-pass cannot strand the debt.
	h.advance(grokResumeClaimTTL)
	grokResumeAdoptedDebt(state, h.clock())
	if got, want := h.state().WorkerClaimAtMs, h.clock().UnixMilli(); got != want {
		t.Fatalf("claim = %d, want the expired claim retaken at %d", got, want)
	}

	close(h.block)
	h.idle()
	if got := h.reads.Load(); got != 1 {
		t.Fatalf("reads = %d, want one read for one debt", got)
	}
	if h.state().owed() {
		t.Fatalf("state = %+v, want the paid debt cleared", h.state())
	}
}
