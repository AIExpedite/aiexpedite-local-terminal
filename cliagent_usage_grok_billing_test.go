package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func assertGrokMetricsJSON(t *testing.T, metrics []cliAgentUsageMetric, want string) {
	t.Helper()
	got, err := json.Marshal(metrics)
	if err != nil {
		t.Fatalf("marshal metrics: %v", err)
	}
	if string(got) != want {
		t.Fatalf("metrics JSON = %s\nwant         = %s", got, want)
	}
}

func helperWriteGrokLog(t *testing.T, base string, lines ...string) {
	t.Helper()
	dir := filepath.Join(base, "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "unified.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatalf("write unified.jsonl: %v", err)
	}
}

func grokBillingLine(ts string, percent float64, periodType, start, end string) string {
	return fmt.Sprintf(
		`{"ts":%q,"src":"shell","lvl":"info","msg":%q,"ctx":{"config":{"creditUsagePercent":%v,`+
			`"currentPeriod":{"type":%q,"start":%q,"end":%q},"onDemandCap":{"val":0},`+
			`"onDemandUsed":{"val":0},"prepaidBalance":{"val":0}},"subscriptionTier":"SuperGrok"}}`,
		ts, grokBillingLogMessage, percent, periodType, start, end,
	)
}

func TestReadGrokBillingSnapshot_TakesNewestRecord(t *testing.T) {
	base := t.TempDir()
	helperWriteGrokLog(t, base,
		`{"ts":"2026-08-03T22:00:00Z","msg":"session start","ctx":{"user_id":"acct-1"}}`,
		grokBillingLine("2026-08-03T22:30:00Z", 12, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-03T22:28:32Z", "2026-08-10T22:28:32Z"),
		`{"ts":"2026-08-04T10:00:00Z","msg":"chat: turn complete","ctx":{}}`,
		grokBillingLine("2026-08-10T17:08:10Z", 52, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-03T22:28:32Z", "2026-08-10T22:28:32Z"),
		`{"ts":"2026-08-10T17:09:00Z","msg":"chat: turn complete","ctx":{}}`,
	)

	snap, ok := readGrokBillingSnapshot(base, []string{"acct-1"})
	if !ok {
		t.Fatalf("expected a billing snapshot")
	}
	if snap.UsedPercent != 52 {
		t.Errorf("UsedPercent=%v, want 52 (newest record)", snap.UsedPercent)
	}
	if got := snap.ObservedAt.UTC().Format(time.RFC3339); got != "2026-08-10T17:08:10Z" {
		t.Errorf("ObservedAt=%q, want the record's own ts", got)
	}
	if snap.SubscriptionTier != "SuperGrok" {
		t.Errorf("SubscriptionTier=%q, want SuperGrok", snap.SubscriptionTier)
	}
	if !snap.HasPeriodEnd {
		t.Errorf("period end should be parsed")
	}
}

// The tail almost always begins mid-line. A truncated leading record must be
// skipped, not mistaken for a malformed log that aborts the whole read.
func TestReadGrokBillingSnapshot_SkipsTruncatedLeadingLine(t *testing.T) {
	base := t.TempDir()
	helperWriteGrokLog(t, base,
		`sagePercent":99.0,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY"}}}`,
		`{"ts":"2026-08-03T22:00:00Z","msg":"session start","ctx":{"user_id":"acct-1"}}`,
		grokBillingLine("2026-08-10T17:08:10Z", 41, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-03T22:28:32Z", "2026-08-10T22:28:32Z"),
	)

	snap, ok := readGrokBillingSnapshot(base, []string{"acct-1"})
	if !ok {
		t.Fatalf("expected a billing snapshot")
	}
	if snap.UsedPercent != 41 {
		t.Errorf("UsedPercent=%v, want 41", snap.UsedPercent)
	}
}

func TestReadGrokBillingSnapshot_DoesNotScanOutsideBoundedTail(t *testing.T) {
	base := t.TempDir()
	helperWriteGrokLog(t, base,
		`{"ts":"2026-08-03T22:00:00Z","msg":"session start","ctx":{"user_id":"acct-1"}}`,
		grokBillingLine("2026-08-10T17:08:10Z", 41, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-03T22:28:32Z", "2026-08-10T22:28:32Z"),
		strings.Repeat("x", grokBillingLogTailBytes),
	)

	if _, ok := readGrokBillingSnapshot(base, []string{"acct-1"}); ok {
		t.Fatal("a billing record older than the bounded tail must not be revived")
	}
}

func TestReadGrokBillingSnapshot_RejectsRecordWithoutObservationTime(t *testing.T) {
	base := t.TempDir()
	helperWriteGrokLog(t, base,
		fmt.Sprintf(`{"msg":%q,"ctx":{"config":{"creditUsagePercent":52.0}}}`, grokBillingLogMessage),
	)

	if _, ok := readGrokBillingSnapshot(base, nil); ok {
		t.Errorf("a percentage with no ts must be rejected — it cannot be aged")
	}
}

func TestReadGrokBillingSnapshot_NewestInvalidTimestampBlocksOlderPercentage(t *testing.T) {
	base := t.TempDir()
	newest := fmt.Sprintf(`{"ts":"not-a-time","msg":%q,"ctx":{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-08-17T22:28:32Z"}}}}`, grokBillingLogMessage)
	helperWriteGrokLog(t, base,
		`{"ts":"2026-08-10T17:00:00Z","msg":"session start","ctx":{"user_id":"acct-1"}}`,
		grokBillingLine("2026-08-10T17:08:10Z", 33, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-10T22:28:32Z", "2026-08-17T22:28:32Z"),
		newest,
	)

	if _, ok := readGrokBillingSnapshot(base, []string{"acct-1"}); ok {
		t.Fatal("newest exact-message record must block an older percentage even when its ts is invalid")
	}
}

func TestReadGrokBillingSnapshot_TruncatedNewestRecordBlocksOlderPercentage(t *testing.T) {
	base := t.TempDir()
	helperWriteGrokLog(t, base,
		`{"ts":"2026-08-10T17:00:00Z","msg":"session start","ctx":{"user_id":"acct-1"}}`,
		grokBillingLine("2026-08-10T17:08:10Z", 33, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-10T22:28:32Z", "2026-08-17T22:28:32Z"),
	)
	path := filepath.Join(base, "logs", "unified.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString(`{"ts":"2026-08-17T23:02:12Z","msg":"billing: fetched credits config","ctx":{"config":`)
	closeErr := f.Close()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}

	if _, ok := readGrokBillingSnapshot(base, []string{"acct-1"}); ok {
		t.Fatal("a partially appended newest billing record must block an older percentage")
	}
}

func TestReadGrokBillingSnapshot_LookalikeMessageDoesNotSupersedeBilling(t *testing.T) {
	base := t.TempDir()
	lookalike := `{"ts":"2026-08-17T23:02:12Z","msg":"not billing: fetched credits config","ctx":{"config":{"creditUsagePercent":99,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-08-24T22:28:32Z"}}}}`
	helperWriteGrokLog(t, base,
		`{"ts":"2026-08-10T17:00:00Z","msg":"session start","ctx":{"user_id":"acct-1"}}`,
		grokBillingLine("2026-08-13T15:49:37Z", 33, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-10T22:28:32Z", "2026-08-24T22:28:32Z"),
		lookalike,
	)

	snap, ok := readGrokBillingSnapshot(base, []string{"acct-1"})
	if !ok || !snap.HasUsedPercent || snap.UsedPercent != 33 {
		t.Fatalf("lookalike message superseded the exact billing record: %+v, ok=%v", snap, ok)
	}
}

func TestReadGrokBillingSnapshot_MissingLogIsNotAnError(t *testing.T) {
	if _, ok := readGrokBillingSnapshot(t.TempDir(), nil); ok {
		t.Errorf("absent log should report no snapshot")
	}
}

func TestGrokBillingMetrics_LiveWindowPlotsCredits(t *testing.T) {
	now := time.Date(2026, 8, 10, 18, 0, 0, 0, time.UTC)
	snap := grokBillingSnapshot{
		UsedPercent:    52,
		HasUsedPercent: true,
		ObservedAt:     now.Add(-time.Hour),
		PeriodType:     "USAGE_PERIOD_TYPE_WEEKLY",
		PeriodEnd:      now.Add(4 * time.Hour),
		HasPeriodEnd:   true,
	}

	metrics := grokBillingMetrics(snap, now)
	if len(metrics) != 1 {
		t.Fatalf("len(metrics)=%d, want 1", len(metrics))
	}
	m := metrics[0]
	if m.Kind != limitKindWeekly {
		t.Errorf("Kind=%q, want %q", m.Kind, limitKindWeekly)
	}
	if m.Consumed == nil || *m.Consumed != 52 || m.Remaining == nil || *m.Remaining != 48 {
		t.Errorf("Consumed/Remaining=%v/%v, want 52/48", m.Consumed, m.Remaining)
	}
	if m.ResetAt != snap.PeriodEnd.UTC().Format(time.RFC3339) {
		t.Errorf("ResetAt=%q, want the period end", m.ResetAt)
	}
	if m.ObservedAt != snap.ObservedAt.UTC().Format(time.RFC3339) {
		t.Errorf("ObservedAt=%q, want the provider observation time", m.ObservedAt)
	}
}

// A rolled-over pool must not read as 0% used: usage may have happened on
// another computer under the same account since Grok last looked.
func TestGrokBillingMetrics_RolledOverPeriodIsUnobservable(t *testing.T) {
	now := time.Date(2026, 8, 11, 18, 0, 0, 0, time.UTC)
	snap := grokBillingSnapshot{
		UsedPercent:    99,
		HasUsedPercent: true,
		ObservedAt:     now.Add(-26 * time.Hour),
		PeriodType:     "USAGE_PERIOD_TYPE_WEEKLY",
		PeriodEnd:      now.Add(-2 * time.Hour),
		HasPeriodEnd:   true,
	}

	m := grokBillingMetrics(snap, now)[0]
	if !m.Unknown {
		t.Errorf("rolled-over pool must be Unknown, got %+v", m)
	}
	if m.Consumed != nil || m.ResetAt != "" {
		t.Errorf("rolled-over pool must carry neither a value nor a past reset: %+v", m)
	}
	if m.ObservedAt == "" {
		t.Errorf("observation time must survive so the card can age the row")
	}
}

// A rolled-over Grok 1.0 record (no creditUsagePercent) must not date-stamp the
// row: nothing was observed, so a "Last observed" date would recreate the stale
// reading this change removes for the still-live unmetered window.
func TestGrokBillingMetrics_RolledOverUnmeteredPeriodHasNoObservationTime(t *testing.T) {
	now := time.Date(2026, 8, 11, 18, 0, 0, 0, time.UTC)
	snap := grokBillingSnapshot{
		HasUsedPercent: false,
		ObservedAt:     now.Add(-26 * time.Hour),
		PeriodType:     "USAGE_PERIOD_TYPE_WEEKLY",
		PeriodEnd:      now.Add(-2 * time.Hour),
		HasPeriodEnd:   true,
	}

	m := grokBillingMetrics(snap, now)[0]
	if !m.Unknown {
		t.Errorf("rolled-over unmetered pool must be Unknown, got %+v", m)
	}
	if m.Consumed != nil {
		t.Errorf("unmetered pool must carry no value: %+v", m)
	}
	if m.ObservedAt != "" {
		t.Errorf("no percentage was observed, so ObservedAt must be empty: %q", m.ObservedAt)
	}
}

// An unrecognized period must not be plotted under a guessed window — but the
// record was still read, so its freshness must survive. Discarding the whole
// observation is what produced observableMetricCount 0 with no latestObservedAt.
func TestGrokBillingMetrics_UnknownPeriodTypeIsNotGuessed(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	snap := grokBillingSnapshot{
		UsedPercent:    52,
		HasUsedPercent: true,
		ObservedAt:     now.Add(-time.Hour),
		PeriodType:     "USAGE_PERIOD_TYPE_SOMETHING_NEW",
		PeriodEnd:      now.Add(48 * time.Hour),
		HasPeriodEnd:   true,
	}
	assertGrokMetricsJSON(t, grokBillingMetrics(snap, now),
		`[{"kind":"weekly","label":"Weekly credits","unit":"%","observedAt":"2026-08-19T11:00:00Z","unknown":true}]`)
}

func TestGrokBillingMetrics_FutureRecordFailsClosedBeforeEveryPool(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	metrics := grokBillingMetrics(grokBillingSnapshot{
		UsedPercent:      33,
		HasUsedPercent:   true,
		ObservedAt:       now.Add(grokBillingMaxClockSkew + time.Second),
		PeriodType:       "USAGE_PERIOD_TYPE_UNKNOWN",
		PeriodEnd:        now.Add(7 * 24 * time.Hour),
		HasPeriodEnd:     true,
		HasOnDemand:      true,
		HasOnDemandUsed:  true,
		OnDemandCap:      50,
		OnDemandUsed:     12,
		SubscriptionTier: "secret-lookalike",
	}, now)

	assertGrokMetricsJSON(t, metrics,
		`[{"kind":"weekly","label":"Weekly credits","unit":"%","unknown":true}]`)
}

func TestGrokUsageParser_FutureNewestRecordBlocksOlderPercentage(t *testing.T) {
	base := t.TempDir()
	t.Setenv("GROK_HOME", base)
	helperWriteJSON(t, filepath.Join(base, "auth.json"), map[string]any{"user_id": "acct-1"})
	newest := `{"ts":"2026-08-19T12:05:01Z","msg":"billing: fetched credits config","ctx":{"config":{"creditUsagePercent":80,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-08-24T22:28:32Z"},"onDemandCap":{"val":50},"onDemandUsed":{"val":12}}}}`
	helperWriteGrokLog(t, base,
		`{"ts":"2026-08-10T17:00:00Z","msg":"session start","ctx":{"user_id":"acct-1"}}`,
		grokBillingLine("2026-08-13T15:49:37Z", 33, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-10T22:28:32Z", "2026-08-17T22:28:32Z"),
		newest,
	)

	usage, ok := grokUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true},
		time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("Parse failed")
	}
	assertGrokMetricsJSON(t, usage.Metrics,
		`[{"kind":"weekly","label":"Weekly credits","unit":"%","unknown":true}]`)
}

func TestGrokBillingMetrics_CurrentUnmeteredPreservesFreshOnDemand(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	metrics := grokBillingMetrics(grokBillingSnapshot{
		ObservedAt:      time.Date(2026, 8, 17, 23, 2, 12, 0, time.UTC),
		PeriodType:      "USAGE_PERIOD_TYPE_WEEKLY",
		PeriodEnd:       time.Date(2026, 8, 24, 22, 28, 32, 0, time.UTC),
		HasPeriodEnd:    true,
		HasOnDemand:     true,
		HasOnDemandUsed: true,
		OnDemandCap:     50,
		OnDemandUsed:    12,
	}, now)

	assertGrokMetricsJSON(t, metrics,
		`[{"kind":"weekly","label":"Weekly credits","unit":"%","resetAt":"2026-08-24T22:28:32Z","observedAt":"2026-08-17T23:02:12Z","unknown":true},{"kind":"tokens","label":"On-demand credits","unit":"credits","total":50,"remaining":38,"consumed":12,"observedAt":"2026-08-17T23:02:12Z"}]`)
}

func TestGrokBillingSnapshot_InvalidNumbersAreOmittedIndependently(t *testing.T) {
	for _, invalid := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		rec := grokBillingRecord{TS: "2026-08-17T23:02:12Z"}
		rec.Ctx.Config.CreditUsagePercent = grokBillingNumber{Value: invalid, Valid: true}
		rec.Ctx.Config.CurrentPeriod.Type = "USAGE_PERIOD_TYPE_WEEKLY"
		rec.Ctx.Config.CurrentPeriod.End = "2026-08-24T22:28:32Z"
		cap, used := 50.0, 12.0
		rec.Ctx.Config.OnDemandCap.Val = grokBillingNumber{Value: cap, Valid: true}
		rec.Ctx.Config.OnDemandUsed.Val = grokBillingNumber{Value: used, Valid: true}

		snap, ok := grokBillingSnapshotFromRecord(rec)
		if !ok || snap.HasUsedPercent || !snap.HasOnDemandUsed {
			t.Fatalf("invalid percentage %v must become unmetered without suppressing on-demand: %+v", invalid, snap)
		}
	}

	for _, invalidUsed := range []float64{-1, 51, math.NaN(), math.Inf(1)} {
		rec := grokBillingRecord{TS: "2026-08-17T23:02:12Z"}
		cap := 50.0
		rec.Ctx.Config.OnDemandCap.Val = grokBillingNumber{Value: cap, Valid: true}
		rec.Ctx.Config.OnDemandUsed.Val = grokBillingNumber{Value: invalidUsed, Valid: true}
		snap, ok := grokBillingSnapshotFromRecord(rec)
		if !ok || !snap.HasOnDemand || snap.HasOnDemandUsed {
			t.Fatalf("invalid on-demand usage %v must leave a capped unknown pool: %+v", invalidUsed, snap)
		}
	}
}

func TestReadGrokBillingSnapshot_InvalidJSONNumbersBlockOlderAndPreserveOnDemand(t *testing.T) {
	for _, invalidPercent := range []string{`1e999`, `"NaN"`, `"Infinity"`, `{}`} {
		t.Run(invalidPercent, func(t *testing.T) {
			base := t.TempDir()
			newest := fmt.Sprintf(`{"ts":"2026-08-17T23:02:12Z","msg":%q,"ctx":{"config":{"creditUsagePercent":%s,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-08-24T22:28:32Z"},"onDemandCap":{"val":50},"onDemandUsed":{"val":12}}}}`,
				grokBillingLogMessage, invalidPercent)
			helperWriteGrokLog(t, base,
				`{"ts":"2026-08-10T17:00:00Z","msg":"session start","ctx":{"user_id":"acct-1"}}`,
				grokBillingLine("2026-08-13T15:49:37Z", 33, "USAGE_PERIOD_TYPE_WEEKLY",
					"2026-08-10T22:28:32Z", "2026-08-17T22:28:32Z"),
				newest,
			)

			snap, ok := readGrokBillingSnapshot(base, []string{"acct-1"})
			if !ok {
				t.Fatal("newest record must remain usable when only its percentage is invalid")
			}
			if snap.HasUsedPercent || !snap.HasOnDemandUsed || snap.OnDemandUsed != 12 {
				t.Fatalf("invalid percentage must be omitted without reviving 33%% or dropping on-demand: %+v", snap)
			}
			assertGrokMetricsJSON(t, grokBillingMetrics(snap, time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)),
				`[{"kind":"weekly","label":"Weekly credits","unit":"%","resetAt":"2026-08-24T22:28:32Z","observedAt":"2026-08-17T23:02:12Z","unknown":true},{"kind":"tokens","label":"On-demand credits","unit":"credits","total":50,"remaining":38,"consumed":12,"observedAt":"2026-08-17T23:02:12Z"}]`)
		})
	}
}

func TestGrokBillingSnapshot_PercentageBoundariesClampSafely(t *testing.T) {
	for _, tc := range []struct {
		in, want float64
	}{{-1, 0}, {0, 0}, {100, 100}, {101, 100}} {
		rec := grokBillingRecord{TS: "2026-08-17T23:02:12Z"}
		rec.Ctx.Config.CreditUsagePercent = grokBillingNumber{Value: tc.in, Valid: true}
		snap, ok := grokBillingSnapshotFromRecord(rec)
		if !ok || !snap.HasUsedPercent || snap.UsedPercent != tc.want {
			t.Fatalf("percentage %v normalized to %+v, want %v", tc.in, snap, tc.want)
		}
	}
}

func TestGrokUsageParser_UnknownNewestRecordBlocksOlderAndRedactsLookalikes(t *testing.T) {
	base := t.TempDir()
	t.Setenv("GROK_HOME", base)
	helperWriteJSON(t, filepath.Join(base, "auth.json"), map[string]any{"user_id": "acct-1"})
	newest := `{"ts":"2026-08-17T23:02:12Z","msg":"billing: fetched credits config","access_token":"credential-sentinel","prompt":"prompt-sentinel","ctx":{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_NEW","end":"2026-08-24T22:28:32Z"},"nested":{"creditUsagePercent":91,"used":47},"rawConfig":"raw-config-sentinel"},"toolResult":{"credits":999}}}`
	helperWriteGrokLog(t, base,
		`{"ts":"2026-08-10T17:00:00Z","msg":"session start","ctx":{"user_id":"acct-1"}}`,
		grokBillingLine("2026-08-13T15:49:37Z", 33, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-10T22:28:32Z", "2026-08-17T22:28:32Z"),
		newest,
	)

	usage, ok := grokUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true},
		time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("Parse failed")
	}
	// Unknown window, no guessed reset, no scraped number — but the record was
	// read, so the confirmed observation time is published.
	assertGrokMetricsJSON(t, usage.Metrics,
		`[{"kind":"weekly","label":"Weekly credits","unit":"%","observedAt":"2026-08-17T23:02:12Z","unknown":true}]`)
	out, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"credential-sentinel", "prompt-sentinel", "raw-config-sentinel", "toolResult", "999", "91", "47"} {
		if strings.Contains(string(out), secret) {
			t.Fatalf("published usage leaked non-allowlisted log content %q: %s", secret, out)
		}
	}
}

func TestGrokBillingMetrics_OnDemandOnlyWhenCapped(t *testing.T) {
	now := time.Date(2026, 8, 10, 18, 0, 0, 0, time.UTC)
	base := grokBillingSnapshot{
		UsedPercent:    10,
		HasUsedPercent: true,
		ObservedAt:     now,
		PeriodType:     "USAGE_PERIOD_TYPE_WEEKLY",
		PeriodEnd:      now.Add(time.Hour),
		HasPeriodEnd:   true,
	}
	if got := len(grokBillingMetrics(base, now)); got != 1 {
		t.Errorf("uncapped on-demand must not add a 0-of-0 row, got %d rows", got)
	}

	base.HasOnDemand = true
	base.HasOnDemandUsed = true
	base.OnDemandCap = 50
	base.OnDemandUsed = 20
	metrics := grokBillingMetrics(base, now)
	if len(metrics) != 2 {
		t.Fatalf("len(metrics)=%d, want 2", len(metrics))
	}
	if metrics[1].Total == nil || *metrics[1].Total != 50 ||
		metrics[1].Consumed == nil || *metrics[1].Consumed != 20 {
		t.Errorf("on-demand row=%+v, want 20 of 50", metrics[1])
	}
}

// End-to-end through the parser: the card's rows come from the log, and the
// subscription tier fills in the plan chip.
func TestGrokUsageParser_PlotsCreditsFromCliLog(t *testing.T) {
	home := t.TempDir()
	base := filepath.Join(home, ".grok")
	t.Setenv("GROK_HOME", "")
	helperWriteJSON(t, filepath.Join(base, "auth.json"), map[string]any{
		"email":   "ada@example.com",
		"user_id": "acct-1",
	})
	helperWriteGrokLog(t, base,
		`{"ts":"2026-08-03T22:00:00Z","msg":"session start","ctx":{"user_id":"acct-1"}}`,
		grokBillingLine("2026-08-10T17:08:10Z", 52, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-03T22:28:32Z", "2026-08-10T22:28:32Z"),
	)

	now := time.Date(2026, 8, 10, 18, 0, 0, 0, time.UTC)
	usage, ok := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !ok || usage == nil {
		t.Fatalf("Parse failed")
	}
	if len(usage.Metrics) != 1 || usage.Metrics[0].Unknown {
		t.Fatalf("expected one observed credit row, got %+v", usage.Metrics)
	}
	if usage.Metrics[0].Consumed == nil || *usage.Metrics[0].Consumed != 52 {
		t.Errorf("Consumed=%v, want 52", usage.Metrics[0].Consumed)
	}
	if usage.Plan != "SuperGrok" {
		t.Errorf("Plan=%q, want SuperGrok from the billing record", usage.Plan)
	}
}

func TestGrokUsageParser_KeepsPlaceholderRowWithoutBillingLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GROK_HOME", "")
	if err := os.MkdirAll(filepath.Join(home, ".grok"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	usage, ok := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, time.Now())
	if !ok || usage == nil {
		t.Fatalf("Parse failed")
	}
	if len(usage.Metrics) != 1 || !usage.Metrics[0].Unknown {
		t.Errorf("want a single unobservable placeholder row, got %+v", usage.Metrics)
	}
	// The inferred placeholder must stay timestamp-less: a populated ObservedAt
	// on an Unknown row is what distinguishes a CONFIRMED-unmetered reading from
	// "we have never read a usable record", so this must not acquire one.
	if usage.Metrics[0].ObservedAt != "" {
		t.Errorf("ObservedAt = %q, want empty on the no-record placeholder", usage.Metrics[0].ObservedAt)
	}
}

// $GROK_HOME relocates the whole tree, log included.
func TestReadGrokBillingSnapshot_HonorsRelocatedHome(t *testing.T) {
	relocated := t.TempDir()
	helperWriteJSON(t, filepath.Join(relocated, "auth.json"), map[string]any{
		"user_id": "acct-1",
	})
	helperWriteGrokLog(t, relocated,
		`{"ts":"2026-08-03T22:00:00Z","msg":"session start","ctx":{"user_id":"acct-1"}}`,
		grokBillingLine("2026-08-10T17:08:10Z", 7, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-03T22:28:32Z", "2026-08-10T22:28:32Z"),
	)
	t.Setenv("GROK_HOME", relocated)

	usage, ok := grokUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true},
		time.Date(2026, 8, 10, 18, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatalf("Parse failed")
	}
	if usage.Metrics[0].Consumed == nil || *usage.Metrics[0].Consumed != 7 {
		t.Errorf("Consumed=%v, want 7 from $GROK_HOME", usage.Metrics[0].Consumed)
	}
}

// The log outlives a logout. A billing record left by a previous account carries
// no identity of its own, so it must not be republished as the current one's.
// The binding is per-record: what matters is who the CLI was acting as when the
// record was written, not who is named most recently in the log.
func TestReadGrokBillingSnapshot_RejectsRecordFromAnotherAccount(t *testing.T) {
	base := t.TempDir()
	helperWriteGrokLog(t, base,
		`{"ts":"2026-08-10T17:00:00Z","msg":"session start","ctx":{"user_id":"old-account-uuid"}}`,
		grokBillingLine("2026-08-10T17:08:10Z", 52, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-03T22:28:32Z", "2026-08-10T22:28:32Z"),
		`{"ts":"2026-08-11T09:00:00Z","msg":"chat: turn start","ctx":{"user_id":"new-account-uuid"}}`,
	)

	// The new account has not fetched billing yet, so the newest record is still
	// the previous account's — publishing it as the new account's quota is
	// exactly the bug this guards.
	if _, ok := readGrokBillingSnapshot(base, []string{"new-account-uuid"}); ok {
		t.Errorf("a record produced by a previous account must not be attributed to the new one")
	}
	// For the account that actually produced it, it is still readable.
	if _, ok := readGrokBillingSnapshot(base, []string{"old-account-uuid"}); !ok {
		t.Errorf("the producing account must still be able to read its own record")
	}
}

// A record with no identity before it cannot be shown to belong to the current
// login — refuse rather than attribute it.
func TestReadGrokBillingSnapshot_RejectsUnattributableRecordFollowedByLogin(t *testing.T) {
	base := t.TempDir()
	helperWriteGrokLog(t, base,
		grokBillingLine("2026-08-10T17:08:10Z", 52, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-03T22:28:32Z", "2026-08-10T22:28:32Z"),
		`{"ts":"2026-08-11T09:00:00Z","msg":"session start","ctx":{"user_id":"new-account-uuid"}}`,
	)

	if _, ok := readGrokBillingSnapshot(base, []string{"new-account-uuid"}); ok {
		t.Errorf("a record that predates the only identity in the log must not be attributed to it")
	}
}

// The log's identity may be a user id while the credential's display account is
// an email — matching against every candidate keeps that case working.
func TestReadGrokBillingSnapshot_MatchesAnyIdentityCandidate(t *testing.T) {
	base := t.TempDir()
	helperWriteGrokLog(t, base,
		`{"ts":"2026-08-10T17:00:00Z","msg":"session start","ctx":{"user_id":"abc-123"}}`,
		grokBillingLine("2026-08-10T17:08:10Z", 52, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-03T22:28:32Z", "2026-08-10T22:28:32Z"),
	)

	if _, ok := readGrokBillingSnapshot(base, []string{"ada@example.com", "abc-123"}); !ok {
		t.Errorf("a user_id match must be enough even when the display account is an email")
	}
}

func TestReadGrokBillingSnapshot_AcceptsCamelCaseIdentityEnvelope(t *testing.T) {
	base := t.TempDir()
	helperWriteGrokLog(t, base,
		`{"ts":"2026-08-10T17:00:00Z","msg":"session start","ctx":{"userId":"abc-123"}}`,
		grokBillingLine("2026-08-10T17:08:10Z", 52, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-03T22:28:32Z", "2026-08-10T22:28:32Z"),
	)

	if _, ok := readGrokBillingSnapshot(base, []string{"abc-123"}); !ok {
		t.Fatal("the observed camelCase identity field must bind the billing record")
	}
}

func TestReadGrokBillingSnapshot_RejectsMalformedNewestIdentityEnvelope(t *testing.T) {
	base := t.TempDir()
	helperWriteGrokLog(t, base,
		`{"ts":"2026-08-10T16:00:00Z","msg":"session start","ctx":{"user_id":"acct-1"}}`,
		`{"ts":"2026-08-10T17:00:00Z","msg":"session start","ctx":{"user_id":"acct-1"}`,
		grokBillingLine("2026-08-10T17:08:10Z", 52, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-03T22:28:32Z", "2026-08-10T22:28:32Z"),
	)

	if _, ok := readGrokBillingSnapshot(base, []string{"acct-1"}); ok {
		t.Fatal("a malformed newest identity envelope must not fall back to older identity evidence")
	}
}

func TestReadGrokBillingSnapshot_RejectsConflictingIdentityFields(t *testing.T) {
	base := t.TempDir()
	helperWriteGrokLog(t, base,
		`{"ts":"2026-08-10T17:00:00Z","msg":"session start","user_id":"acct-1","ctx":{"userId":"other-account"}}`,
		grokBillingLine("2026-08-10T17:08:10Z", 52, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-03T22:28:32Z", "2026-08-10T22:28:32Z"),
	)

	for _, identity := range []string{"acct-1", "other-account"} {
		if _, ok := readGrokBillingSnapshot(base, []string{identity}); ok {
			t.Fatalf("conflicting identity fields must not authenticate %q", identity)
		}
	}
}

func TestReadGrokBillingSnapshot_IgnoresNestedIdentityLookalike(t *testing.T) {
	base := t.TempDir()
	helperWriteGrokLog(t, base,
		`{"ts":"2026-08-10T16:00:00Z","msg":"session start","ctx":{"user_id":"acct-1"}}`,
		`{"ts":"2026-08-10T17:00:00Z","msg":"tool result","ctx":{"toolResult":{"user_id":"other-account"}}}`,
		grokBillingLine("2026-08-10T17:08:10Z", 52, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-03T22:28:32Z", "2026-08-10T22:28:32Z"),
	)

	if _, ok := readGrokBillingSnapshot(base, []string{"acct-1"}); !ok {
		t.Fatal("a non-allowlisted nested lookalike must not replace valid account evidence")
	}
	if _, ok := readGrokBillingSnapshot(base, []string{"other-account"}); ok {
		t.Fatal("a nested tool-result value must not authenticate the billing record")
	}
}

// A log that names an account while the local credentials resolve to nothing is
// the signed-out case: refuse rather than publish somebody else's pool.
func TestReadGrokBillingSnapshot_RejectsIdentifiedLogWithoutCredentials(t *testing.T) {
	base := t.TempDir()
	helperWriteGrokLog(t, base,
		`{"ts":"2026-08-10T17:00:00Z","msg":"session start","ctx":{"user_id":"someone"}}`,
		grokBillingLine("2026-08-10T17:08:10Z", 52, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-03T22:28:32Z", "2026-08-10T22:28:32Z"),
	)

	if _, ok := readGrokBillingSnapshot(base, nil); ok {
		t.Errorf("an identified log with no readable credentials must not be trusted")
	}
}

// A CLI old enough never to log an identity leaves the previous account's
// credits in the shared log for the next login to publish as its own. Absence of
// contradiction is not evidence of ownership: refuse, and let the card show
// "unobservable" as it did before this source existed.
func TestReadGrokBillingSnapshot_RejectsLogWithoutAnyIdentity(t *testing.T) {
	base := t.TempDir()
	helperWriteGrokLog(t, base,
		grokBillingLine("2026-08-10T17:08:10Z", 52, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-03T22:28:32Z", "2026-08-10T22:28:32Z"),
	)

	if _, ok := readGrokBillingSnapshot(base, []string{"ada@example.com"}); ok {
		t.Errorf("a record whose producer cannot be identified must not be attributed")
	}
}

func TestReadGrokBillingSnapshot_RejectsSelfAttributedBillingRecord(t *testing.T) {
	base := t.TempDir()
	helperWriteGrokLog(t, base,
		`{"ts":"2026-08-10T17:08:10Z","msg":"billing: fetched credits config","user_id":"acct-1","ctx":{"config":{"creditUsagePercent":52,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-08-17T22:28:32Z"}}}}`,
	)

	if _, ok := readGrokBillingSnapshot(base, []string{"acct-1"}); ok {
		t.Fatal("a billing record must not satisfy its own preceding-identity requirement")
	}
}

func TestGrokIdentityCandidates_CollectsEveryAccountField(t *testing.T) {
	base := t.TempDir()
	helperWriteJSON(t, filepath.Join(base, "auth.json"), map[string]any{
		"email":    "ada@example.com",
		"user_id":  "abc-123",
		"username": "ada",
	})

	got := grokIdentityCandidates(base)
	for _, want := range []string{"ada@example.com", "abc-123", "ada"} {
		found := false
		for _, candidate := range got {
			if candidate == want {
				found = true
			}
		}
		if !found {
			t.Errorf("candidates %v missing %q", got, want)
		}
	}
}

// A configured cap whose usage the record never reported must not read as an
// untouched pool — the row exists, its value does not.
func TestGrokBillingMetrics_OnDemandWithoutUsageIsUnobservable(t *testing.T) {
	now := time.Date(2026, 8, 10, 18, 0, 0, 0, time.UTC)
	snap := grokBillingSnapshot{
		UsedPercent:    10,
		HasUsedPercent: true,
		ObservedAt:     now,
		PeriodType:     "USAGE_PERIOD_TYPE_WEEKLY",
		PeriodEnd:      now.Add(time.Hour),
		HasPeriodEnd:   true,
		HasOnDemand:    true,
		OnDemandCap:    50,
		// HasOnDemandUsed deliberately false: `onDemandUsed` was absent or null.
	}

	metrics := grokBillingMetrics(snap, now)
	if len(metrics) != 2 {
		t.Fatalf("len(metrics)=%d, want 2", len(metrics))
	}
	onDemand := metrics[1]
	if !onDemand.Unknown {
		t.Errorf("on-demand row must be Unknown without an observed usage: %+v", onDemand)
	}
	if onDemand.Consumed != nil || onDemand.Remaining != nil {
		t.Errorf("unobserved on-demand must carry no value: %+v", onDemand)
	}
	if onDemand.Total == nil || *onDemand.Total != 50 {
		t.Errorf("the configured cap should still be reported: %+v", onDemand)
	}
}

// A null onDemandUsed decodes to a nil pointer, not 0 — the record never said
// the pool was untouched.
func TestReadGrokBillingSnapshot_NullOnDemandUsedIsNotZero(t *testing.T) {
	base := t.TempDir()
	helperWriteGrokLog(t, base,
		`{"ts":"2026-08-10T17:00:00Z","msg":"session start","ctx":{"user_id":"acct-1"}}`,
		`{"ts":"2026-08-10T17:08:10Z","msg":"`+grokBillingLogMessage+`","ctx":{"config":{`+
			`"creditUsagePercent":52.0,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY",`+
			`"start":"2026-08-03T22:28:32Z","end":"2026-08-10T22:28:32Z"},`+
			`"onDemandCap":{"val":50},"onDemandUsed":{"val":null}},"subscriptionTier":"SuperGrok"}}`,
	)

	snap, ok := readGrokBillingSnapshot(base, []string{"acct-1"})
	if !ok {
		t.Fatalf("expected a billing snapshot")
	}
	if !snap.HasOnDemand {
		t.Errorf("a positive cap should still register the pool")
	}
	if snap.HasOnDemandUsed {
		t.Errorf("a null onDemandUsed must not count as an observed 0")
	}
}

// An unrecognized period that NAMES an end already in the past is a definitive
// rollover, and it outranks grokBillingObservationTTL: the record describes a
// window that is over, so date-stamping the Unknown row would present it as a
// confirmed reading of the LIVE window for up to the whole TTL. Mirrors the
// downgrade TestGrokBillingMetrics_RolledOverUnmeteredPeriodHasNoObservationTime
// pins for a recognized period.
func TestGrokBillingMetrics_EndedUnknownPeriodDropsFreshness(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	base := grokBillingSnapshot{
		ObservedAt:   now.Add(-time.Hour),
		PeriodType:   "USAGE_PERIOD_TYPE_SOMETHING_NEW",
		PeriodEnd:    now.Add(-time.Minute),
		HasPeriodEnd: true,
	}
	metered := base
	metered.UsedPercent = 52
	metered.HasUsedPercent = true

	for name, snap := range map[string]grokBillingSnapshot{
		"unmetered": base,
		"metered":   metered,
	} {
		t.Run(name, func(t *testing.T) {
			assertGrokMetricsJSON(t, grokBillingMetrics(snap, now),
				`[{"kind":"weekly","label":"Weekly credits","unit":"%","unknown":true}]`)
		})
	}
}

// The same record with an end still in the FUTURE keeps its freshness — the
// rollover check above must not swallow the confirmed-unmetered state this
// feature exists to produce.
func TestGrokBillingMetrics_LiveUnknownPeriodKeepsFreshness(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	assertGrokMetricsJSON(t, grokBillingMetrics(grokBillingSnapshot{
		ObservedAt:   now.Add(-time.Hour),
		PeriodType:   "USAGE_PERIOD_TYPE_SOMETHING_NEW",
		PeriodEnd:    now.Add(time.Hour),
		HasPeriodEnd: true,
	}, now),
		`[{"kind":"weekly","label":"Weekly credits","unit":"%","observedAt":"2026-08-19T11:00:00Z","unknown":true}]`)
}

/* ------------------------- live cache: commits ------------------------- */

// grokLiveCacheFixture isolates the live cache on a pinned process epoch.
func grokLiveCacheFixture(t *testing.T, epoch int64) {
	t.Helper()
	withCodexGenerationEpoch(t, epoch)
	t.Setenv("AIEXPEDITE_GROK_BILLING_LIVE_CACHE", filepath.Join(t.TempDir(), "grok_billing_live.json"))
}

func grokLiveEntry(fingerprint string, at time.Time, percent *float64, periodEnd time.Time) grokBillingLiveCache {
	snap := grokBillingSnapshot{ObservedAt: at, PeriodType: "USAGE_PERIOD_TYPE_WEEKLY", PeriodEnd: periodEnd, HasPeriodEnd: !periodEnd.IsZero()}
	if percent != nil {
		snap.UsedPercent, snap.HasUsedPercent = *percent, true
	}
	return grokBillingLiveFromSnapshot(snap, fingerprint)
}

func readGrokLiveCacheFile(t *testing.T) grokBillingLiveCache {
	t.Helper()
	var entry grokBillingLiveCache
	if !readJSONFile(grokBillingLiveCachePath(), &entry) {
		t.Fatal("no live cache on disk")
	}
	return entry
}

func TestGrokLiveCache_UnmeteredInTheSameOpenPeriodKeepsTheNumber(t *testing.T) {
	grokLiveCacheFixture(t, 8101)
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	end := at.Add(72 * time.Hour)
	saveGrokBillingLive(grokLiveEntry("fp-ada", at, floatPtr(36), end))
	before := readGrokLiveCacheFile(t).Generation
	if before == nil || *before != gen(8101, 1) {
		t.Fatalf("generation = %+v, want {8101,1}", before)
	}

	saveGrokBillingLive(grokLiveEntry("fp-ada", at.Add(time.Hour), nil, end))
	entry := readGrokLiveCacheFile(t)
	if entry.Generation == nil || *entry.Generation != *before {
		t.Fatalf("generation = %+v, want an unmetered read never to bump it", entry.Generation)
	}
	snap, ok := loadGrokBillingLiveSnapshot("fp-ada", at.Add(2*time.Hour))
	if !ok || !snap.HasUsedPercent || snap.UsedPercent != 36 || !snap.ObservedAt.Equal(at) {
		t.Fatalf("snap = %+v ok=%t, want 36%% at its own older observedAt", snap, ok)
	}
}

func TestGrokLiveCache_ANewerNumericReadWithTheSamePercentBumps(t *testing.T) {
	grokLiveCacheFixture(t, 8102)
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	end := at.Add(72 * time.Hour)
	saveGrokBillingLive(grokLiveEntry("fp-ada", at, floatPtr(36), end))
	saveGrokBillingLive(grokLiveEntry("fp-ada", at.Add(time.Minute), floatPtr(36), end))
	if g := readGrokLiveCacheFile(t).Generation; g == nil || *g != gen(8102, 2) {
		t.Fatalf("generation = %+v, want a newer numeric observedAt to bump to {8102,2}", g)
	}
	// The same reading again commits nothing new.
	saveGrokBillingLive(grokLiveEntry("fp-ada", at.Add(time.Minute), floatPtr(36), end))
	if g := readGrokLiveCacheFile(t).Generation; *g != gen(8102, 2) {
		t.Fatalf("generation = %+v, want an identical read to keep {8102,2}", g)
	}
	// Another epoch's cache restarts at counter 1 on this one.
	codexProcessGenerationEpoch.Store(8103)
	saveGrokBillingLive(grokLiveEntry("fp-ada", at.Add(2*time.Minute), floatPtr(40), end))
	if g := readGrokLiveCacheFile(t).Generation; *g != gen(8103, 1) {
		t.Fatalf("generation = %+v, want {8103,1}", g)
	}
}

// Another agent process holding the cache's sibling lock serializes both the
// commit and the rotation: neither renames over what the holder may commit,
// and both go through once it releases.
func TestGrokLiveCache_AnotherProcessHoldingTheLockRefusesCommitAndRotation(t *testing.T) {
	grokLiveCacheFixture(t, 8110)
	prevWait := grokBillingLiveCacheLockWait
	grokBillingLiveCacheLockWait = 20 * time.Millisecond
	t.Cleanup(func() { grokBillingLiveCacheLockWait = prevWait })
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	end := at.Add(72 * time.Hour)
	if !saveGrokBillingLive(grokLiveEntry("fp-ada", at, floatPtr(36), end)) {
		t.Fatal("want the unlocked commit to land")
	}

	held, err := os.OpenFile(grokBillingLiveCachePath()+".lock", os.O_RDWR|os.O_CREATE, 0o600)
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

	if saveGrokBillingLive(grokLiveEntry("fp-ada", at.Add(time.Minute), floatPtr(40), end)) {
		t.Fatal("want a commit under another holder's lock refused")
	}
	if entry := readGrokLiveCacheFile(t); *entry.UsedPercent != 36 || *entry.Generation != gen(8110, 1) {
		t.Fatalf("entry = %+v, want the cache untouched while locked", entry)
	}
	codexProcessGenerationEpoch.Store(8111)
	if g, refused := rotateGrokBillingLiveGeneration("fp-ada"); g != nil || !refused {
		t.Fatalf("rotation = %+v refused=%t, want it refused for a retry while locked", g, refused)
	}

	release()
	if g, refused := rotateGrokBillingLiveGeneration("fp-ada"); refused || g == nil || *g != gen(8111, 1) {
		t.Fatalf("rotation = %+v refused=%t, want {8111,1} once released", g, refused)
	}
	if !saveGrokBillingLive(grokLiveEntry("fp-ada", at.Add(time.Minute), floatPtr(40), end)) {
		t.Fatal("want the commit to land once released")
	}
	if g := readGrokLiveCacheFile(t).Generation; *g != gen(8111, 2) {
		t.Fatalf("generation = %+v, want {8111,2}", g)
	}
}

// Two processes' requests overlap outside the cache lock: an older same-account
// answer that finishes last is superseded, never written over the newer cache
// or its generation — whether its figures, its period or its kind differ.
func TestGrokLiveCache_AnOlderSameAccountAnswerNeverReplacesANewerOne(t *testing.T) {
	grokLiveCacheFixture(t, 8113)
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	end := at.Add(72 * time.Hour)
	saveGrokBillingLive(grokLiveEntry("fp-ada", at.Add(time.Minute), floatPtr(40), end))
	want := readGrokLiveCacheFile(t)

	for _, older := range []grokBillingLiveCache{
		grokLiveEntry("fp-ada", at, floatPtr(36), end),
		grokLiveEntry("fp-ada", at, floatPtr(36), end.Add(-time.Hour)),
		grokLiveEntry("fp-ada", at, nil, end),
		grokLiveOnDemandEntry("fp-ada", at, nil, end, 50, 5),
	} {
		committed, advanced, _ := commitGrokBillingLive(older)
		if !committed || advanced {
			t.Fatalf("committed=%t advanced=%t, want an older answer superseded without an advance", committed, advanced)
		}
		if got := readGrokLiveCacheFile(t); !reflect.DeepEqual(got, want) {
			t.Fatalf("cache = %+v, want the newer answer %+v untouched", got, want)
		}
	}

	// Another account's answer is not compared against this one's time.
	if _, advanced, _ := commitGrokBillingLive(grokLiveEntry("fp-bob", at, floatPtr(10), end)); !advanced {
		t.Fatal("want another account's numeric read to replace and advance")
	}
}

// A cached answer a forward clock jump stamped beyond grokBillingMaxClockSkew
// ahead is untrusted once the clock is corrected: the next read replaces it
// (with an advance, even with matching figures) instead of being superseded
// until wall time catches up.
func TestGrokLiveCache_ACorrectedClockReadReplacesAFutureCacheEntry(t *testing.T) {
	grokLiveCacheFixture(t, 8114)
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	end := at.Add(72 * time.Hour)
	saveGrokBillingLive(grokLiveEntry("fp-ada", at.Add(6*time.Hour), floatPtr(40), end))

	// Within the skew an earlier answer is still an overlapping request.
	if committed, advanced, _ := commitGrokBillingLive(grokLiveEntry("fp-ada", at.Add(6*time.Hour-grokBillingMaxClockSkew), floatPtr(40), end)); !committed || advanced {
		t.Fatalf("committed=%t advanced=%t, want a near-earlier answer superseded", committed, advanced)
	}
	committed, advanced, _ := commitGrokBillingLive(grokLiveEntry("fp-ada", at, floatPtr(40), end))
	if !committed || !advanced {
		t.Fatalf("committed=%t advanced=%t, want the corrected read to replace and advance", committed, advanced)
	}
	if got := readGrokLiveCacheFile(t); got.ObservedAt != at.Format(time.RFC3339Nano) || *got.UsedPercent != 40 {
		t.Fatalf("cache = %+v, want the corrected read", got)
	}
}

func TestGrokLiveCache_AcrossARolloverTheUnmeteredReadWins(t *testing.T) {
	grokLiveCacheFixture(t, 8104)
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	end := at.Add(time.Hour)
	saveGrokBillingLive(grokLiveEntry("fp-ada", at, floatPtr(80), end))
	// A new period, unmetered: the old figure is not borrowed.
	saveGrokBillingLive(grokLiveEntry("fp-ada", end.Add(time.Minute), nil, end.Add(7*24*time.Hour)))
	if snap, ok := loadGrokBillingLiveSnapshot("fp-ada", end.Add(2*time.Minute)); !ok || snap.HasUsedPercent {
		t.Fatalf("snap = %+v, want the new period's unmetered answer", snap)
	}
}

// grokLiveOnDemandEntry is grokLiveEntry carrying an on-demand pool.
func grokLiveOnDemandEntry(fingerprint string, at time.Time, percent *float64, periodEnd time.Time, cap, used float64) grokBillingLiveCache {
	entry := grokLiveEntry(fingerprint, at, percent, periodEnd)
	entry.OnDemandCap, entry.OnDemandUsed = floatPtr(cap), floatPtr(used)
	return entry
}

func TestGrokLiveCache_AnUnmeteredReadWithANumericOnDemandPoolBumps(t *testing.T) {
	grokLiveCacheFixture(t, 8108)
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	end := at.Add(72 * time.Hour)
	saveGrokBillingLive(grokLiveOnDemandEntry("fp-ada", at, floatPtr(36), end, 50, 5))

	// Credits unmetered, on-demand moved: the pool is a fresh numeric reading.
	later := at.Add(time.Hour)
	saveGrokBillingLive(grokLiveOnDemandEntry("fp-ada", later, nil, end, 50, 12))
	entry := readGrokLiveCacheFile(t)
	if entry.Generation == nil || *entry.Generation != gen(8108, 2) {
		t.Fatalf("generation = %+v, want the on-demand pool to bump to {8108,2}", entry.Generation)
	}
	now := at.Add(2 * time.Hour)
	snap, ok := loadGrokBillingLiveSnapshot("fp-ada", now)
	if !ok || snap.UsedPercent != 36 || !snap.ObservedAt.Equal(at) || !snap.OnDemandObservedAt.Equal(later) {
		t.Fatalf("snap = %+v ok=%t, want 36%% at its own time and the pool at the newer one", snap, ok)
	}
	metrics := grokBillingMetrics(snap, now)
	if len(metrics) != 2 || metrics[1].ObservedAt != later.Format(time.RFC3339) || *metrics[1].Consumed != 12 {
		t.Fatalf("metrics = %+v, want the on-demand row stamped with its own read", metrics)
	}

	// The same pool again advances nothing; a changed pool does.
	saveGrokBillingLive(grokLiveOnDemandEntry("fp-ada", later, nil, end, 50, 12))
	if g := readGrokLiveCacheFile(t).Generation; *g != gen(8108, 2) {
		t.Fatalf("generation = %+v, want an identical pool to keep {8108,2}", g)
	}
	saveGrokBillingLive(grokLiveOnDemandEntry("fp-ada", later, nil, end, 50, 20))
	if g := readGrokLiveCacheFile(t).Generation; *g != gen(8108, 3) {
		t.Fatalf("generation = %+v, want changed figures to bump to {8108,3}", g)
	}
}

// A newer unmetered answer that drops the on-demand pool under a kept
// percentage removes the card's on-demand row, so the removal is versioned.
func TestGrokLiveCache_DroppingTheOnDemandPoolUnderAKeptPercentageBumps(t *testing.T) {
	grokLiveCacheFixture(t, 8110)
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	end := at.Add(72 * time.Hour)
	saveGrokBillingLive(grokLiveOnDemandEntry("fp-ada", at, floatPtr(36), end, 50, 5))

	later := at.Add(time.Hour)
	saveGrokBillingLive(grokLiveEntry("fp-ada", later, nil, end))
	entry := readGrokLiveCacheFile(t)
	if entry.Generation == nil || *entry.Generation != gen(8110, 2) || entry.Cleared {
		t.Fatalf("generation = %+v cleared=%t, want the removal to bump to {8110,2} without a clear mark", entry.Generation, entry.Cleared)
	}
	snap, ok := loadGrokBillingLiveSnapshot("fp-ada", later)
	if !ok || snap.UsedPercent != 36 || !snap.ObservedAt.Equal(at) || snap.HasOnDemand {
		t.Fatalf("snap = %+v ok=%t, want the kept 36%% and no on-demand pool", snap, ok)
	}

	// Nothing left to remove: the next unmetered answer advances nothing.
	saveGrokBillingLive(grokLiveEntry("fp-ada", later.Add(time.Hour), nil, end))
	if g := readGrokLiveCacheFile(t).Generation; *g != gen(8110, 2) {
		t.Fatalf("generation = %+v, want {8110,2} kept", g)
	}
}

// An unmetered read for another account replaces the cached number: the card
// moves from that figure to Unknown, so the replacement is a versioned clear.
func TestGrokLiveCache_AnUnmeteredReadReplacingAnotherAccountsNumberClears(t *testing.T) {
	grokLiveCacheFixture(t, 8112)
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	end := at.Add(72 * time.Hour)
	saveGrokBillingLive(grokLiveEntry("fp-ada", at, floatPtr(36), end))

	saveGrokBillingLive(grokLiveEntry("fp-bob", at.Add(time.Minute), nil, end))
	entry := readGrokLiveCacheFile(t)
	if entry.Generation == nil || *entry.Generation != gen(8112, 2) || !entry.Cleared {
		t.Fatalf("generation = %+v cleared=%t, want the replacement versioned as a clear {8112,2}", entry.Generation, entry.Cleared)
	}
	view, ok := loadGrokBillingLive("fp-bob", at.Add(2*time.Minute))
	if !ok || !view.cleared || view.generation == nil || *view.generation != gen(8112, 2) {
		t.Fatalf("view = %+v ok=%t, want fp-bob's clear published with its generation", view, ok)
	}

	// fp-bob's next unmetered answer keeps the mark without advancing again.
	saveGrokBillingLive(grokLiveEntry("fp-bob", at.Add(time.Hour), nil, end))
	if entry := readGrokLiveCacheFile(t); *entry.Generation != gen(8112, 2) || !entry.Cleared {
		t.Fatalf("generation = %+v cleared=%t, want {8112,2} kept with the mark", entry.Generation, entry.Cleared)
	}
}

func TestGrokLiveCache_AnUnmeteredReadWithoutUsageOnTheCapNeverBumps(t *testing.T) {
	grokLiveCacheFixture(t, 8109)
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	end := at.Add(72 * time.Hour)
	entry := grokLiveEntry("fp-ada", at, nil, end)
	entry.OnDemandCap = floatPtr(50) // a cap with no usage renders Unknown
	saveGrokBillingLive(entry)
	if g := readGrokLiveCacheFile(t).Generation; g != nil {
		t.Fatalf("generation = %+v, want an Unknown pool never versioned", g)
	}
}

func TestGrokRecovery_RecoversAnOnDemandOnlyReading(t *testing.T) {
	h := newGrokDebtHarness(t)
	withCodexGenerationEpoch(t, 8110)
	at := h.clock().Add(-time.Minute)
	end := h.clock().Add(48 * time.Hour)
	saveGrokBillingLive(grokLiveOnDemandEntry("fp-ada", at, nil, end, 50, 7))
	codexProcessGenerationEpoch.Store(8111)
	grokGenerationRotated.Store(false)
	g, refused := grokRecoveryGeneration(h.clock())
	if refused || g == nil || *g != gen(8111, 1) {
		t.Fatalf("recovery = %+v refused=%t, want the numeric on-demand pool recovered as {8111,1}", g, refused)
	}
}

func TestGrokLiveCache_AKeptNumberPastItsPeriodEndIsNeverLiveCoverage(t *testing.T) {
	h := newGrokDebtHarness(t)
	withCodexGenerationEpoch(t, 8105)
	at := h.clock()
	end := at.Add(time.Hour)
	saveGrokBillingLive(grokLiveEntry("fp-ada", at, floatPtr(50), end))
	saveGrokBillingLive(grokLiveEntry("fp-ada", at.Add(30*time.Minute), nil, end))
	if snap, ok := loadGrokBillingLiveSnapshot("fp-ada", at.Add(45*time.Minute)); !ok || !snap.HasUsedPercent {
		t.Fatalf("inside the period: snap = %+v, want the kept number", snap)
	}
	h.advance(2 * time.Hour) // past the period end
	snap, ok := loadGrokBillingLiveSnapshot("fp-ada", h.clock())
	if !ok || snap.HasUsedPercent || !snap.ObservedAt.Equal(at.Add(30*time.Minute)) {
		t.Fatalf("past the end: snap = %+v, want the unmetered answer only", snap)
	}
	if grokObservationCovers("fp-ada", at.UnixMilli()) {
		t.Fatal("a rolled-over kept number must not cover a debt")
	}
	if g, refused := grokRecoveryGeneration(h.clock()); g != nil || refused {
		t.Fatalf("recovery = %+v refused=%t, want nothing recovered past the period end", g, refused)
	}
}

func TestGrokRecovery_SkipsAnotherAccountAndAnUnknownReading(t *testing.T) {
	h := newGrokDebtHarness(t)
	withCodexGenerationEpoch(t, 8106)
	at := h.clock().Add(-time.Minute)
	end := h.clock().Add(48 * time.Hour)

	// Unknown first: over another account's number it would be a clear.
	saveGrokBillingLive(grokLiveEntry("fp-ada", at, nil, end))
	if g, _ := grokRecoveryGeneration(h.clock()); g != nil {
		t.Fatalf("recovered %+v for an unmetered (Unknown) reading", g)
	}
	saveGrokBillingLive(grokLiveEntry("fp-other", at, floatPtr(20), end))
	if g, _ := grokRecoveryGeneration(h.clock()); g != nil {
		t.Fatalf("recovered %+v for another account", g)
	}

	// A numeric reading an earlier process committed is moved onto this
	// epoch in one write.
	saveGrokBillingLive(grokLiveEntry("fp-ada", at, floatPtr(20), end))
	codexProcessGenerationEpoch.Store(8107)
	grokGenerationRotated.Store(false)
	g, refused := grokRecoveryGeneration(h.clock())
	if refused || g == nil || *g != gen(8107, 1) || !grokGenerationRotated.Load() {
		t.Fatalf("recovery = %+v refused=%t rotated=%t, want {8107,1}", g, refused, grokGenerationRotated.Load())
	}
	if on := readGrokLiveCacheFile(t).Generation; on == nil || *on != gen(8107, 1) {
		t.Fatalf("cache generation = %+v, want the rotation on disk", on)
	}
}

// A pool grokBillingMetrics suppresses as future-dated must not be published
// under a generation that versions it: the backend would apply that
// generation and then keep omitting the pool once the clock catches up and it
// renders again, with no commit left to version it.
func TestGrokUsageParser_WithholdsTheGenerationWhileAPoolIsFutureDated(t *testing.T) {
	grokLiveCacheFixture(t, 8130)
	home := t.TempDir()
	base := filepath.Join(home, ".grok")
	t.Setenv("GROK_HOME", "")
	helperWriteJSON(t, filepath.Join(base, "auth.json"), map[string]any{
		"email": "ada@example.com", "user_id": "acct-1",
	})
	fingerprint := grokAccountFingerprintFor(base)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	end := now.Add(72 * time.Hour)
	saveGrokBillingLive(grokLiveEntry(fingerprint, now.Add(-time.Hour), floatPtr(36), end))
	// The next answer carries an on-demand pool and a clock an hour ahead:
	// the percentage is kept at its own trusted time, the pool is not.
	saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
		ObservedAt: now.Add(time.Hour), PeriodType: "USAGE_PERIOD_TYPE_WEEKLY",
		PeriodEnd: end, HasPeriodEnd: true,
		HasOnDemand: true, OnDemandCap: 50, HasOnDemandUsed: true, OnDemandUsed: 20,
	}, fingerprint))
	if g := readGrokLiveCacheFile(t).Generation; g == nil || *g != gen(8130, 2) {
		t.Fatalf("generation = %+v, want the pool's commit at {8130,2}", g)
	}
	grokGenerationRotated.Store(true)

	usage, ok := grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !ok || len(usage.Metrics) != 1 || usage.Metrics[0].Unknown {
		t.Fatalf("usage = %+v, want the percentage alone while the pool is future-dated", usage)
	}
	if usage.UsageGeneration != nil {
		t.Fatalf("usageGeneration = %+v, want none while the pool it versions is withheld", usage.UsageGeneration)
	}

	// Once the clock catches up the pool renders, and the generation that
	// versions it is published with it.
	later := now.Add(2 * time.Hour)
	usage, ok = grokUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, later)
	if !ok || len(usage.Metrics) != 2 || usage.Metrics[1].Consumed == nil || *usage.Metrics[1].Consumed != 20 {
		t.Fatalf("usage = %+v, want the pool rendered once its time has passed", usage)
	}
	if usage.UsageGeneration == nil || *usage.UsageGeneration != gen(8130, 2) {
		t.Fatalf("usageGeneration = %+v, want {8130,2} published with the pool", usage.UsageGeneration)
	}
}

// Startup recovery must apply the same withholding rule as ParseContext: a
// generation that versions a future-dated pool is rotated (so the next gather
// can publish it) but not hinted, because the hint would be spent on a
// refresh whose receipt carries no generation and nothing would hint it again
// once the clock catches up.
func TestGrokRecovery_WithholdsAHintWhileAPoolIsFutureDated(t *testing.T) {
	h := newGrokDebtHarness(t)
	withCodexGenerationEpoch(t, 8132)
	end := h.clock().Add(72 * time.Hour)
	saveGrokBillingLive(grokLiveEntry("fp-ada", h.clock().Add(-time.Hour), floatPtr(36), end))
	// The newest answer's pool is stamped an hour ahead: the percentage is
	// kept at its own trusted time, the pool is withheld until then.
	saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
		ObservedAt: h.clock().Add(time.Hour), PeriodType: "USAGE_PERIOD_TYPE_WEEKLY",
		PeriodEnd: end, HasPeriodEnd: true,
		HasOnDemand: true, OnDemandCap: 50, HasOnDemandUsed: true, OnDemandUsed: 20,
	}, "fp-ada"))
	codexProcessGenerationEpoch.Store(8133)
	grokGenerationRotated.Store(false)

	g, refused := grokRecoveryGeneration(h.clock())
	if g != nil || !refused {
		t.Fatalf("recovery = %+v refused=%t, want the hint withheld and retried while the pool is future-dated", g, refused)
	}
	if on := readGrokLiveCacheFile(t).Generation; on == nil || *on != gen(8133, 1) {
		t.Fatalf("cache generation = %+v, want the rotation committed anyway", on)
	}

	// Once the clock catches up the pool renders, and the recovery hints it.
	h.advance(2 * time.Hour)
	g, refused = grokRecoveryGeneration(h.clock())
	if refused || g == nil || *g != gen(8133, 1) {
		t.Fatalf("recovery = %+v refused=%t, want {8133,1} hinted once the pool renders", g, refused)
	}
}

// The same rule must reach a cache whose ONLY figure is the future-dated pool:
// it renders Unknown on its own, so the metrics gate must not skip it, or
// nothing is rotated, no retry is armed, and the generation stays unhintable
// even after the clock catches up.
func TestGrokRecovery_RetriesAPoolOnlyFutureDatedCache(t *testing.T) {
	h := newGrokDebtHarness(t)
	withCodexGenerationEpoch(t, 8134)
	end := h.clock().Add(72 * time.Hour)
	saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
		ObservedAt: h.clock().Add(time.Hour), PeriodType: "USAGE_PERIOD_TYPE_WEEKLY",
		PeriodEnd: end, HasPeriodEnd: true,
		HasOnDemand: true, OnDemandCap: 50, HasOnDemandUsed: true, OnDemandUsed: 20,
	}, "fp-ada"))
	codexProcessGenerationEpoch.Store(8135)
	grokGenerationRotated.Store(false)

	g, refused := grokRecoveryGeneration(h.clock())
	if g != nil || !refused {
		t.Fatalf("recovery = %+v refused=%t, want the hint withheld and retried for a pool-only future cache", g, refused)
	}
	if on := readGrokLiveCacheFile(t).Generation; on == nil || *on != gen(8135, 1) {
		t.Fatalf("cache generation = %+v, want the rotation committed anyway", on)
	}
	if !grokGenerationRotated.Load() {
		t.Fatal("grokGenerationRotated = false, want the rotation latched so the next gather can publish")
	}

	// Once the clock catches up the pool renders, and the recovery hints it.
	h.advance(2 * time.Hour)
	g, refused = grokRecoveryGeneration(h.clock())
	if refused || g == nil || *g != gen(8135, 1) {
		t.Fatalf("recovery = %+v refused=%t, want {8135,1} hinted once the pool renders", g, refused)
	}
}
