package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
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

func TestGrokBillingMetrics_UnknownPeriodTypeIsNotGuessed(t *testing.T) {
	snap := grokBillingSnapshot{
		UsedPercent:    52,
		HasUsedPercent: true,
		ObservedAt:     time.Now(),
		PeriodType:     "USAGE_PERIOD_TYPE_SOMETHING_NEW",
	}
	if metrics := grokBillingMetrics(snap, time.Now()); metrics != nil {
		t.Errorf("an unrecognized period must not be plotted under a guessed window: %+v", metrics)
	}
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
		rec := grokBillingRecord{TS: "2026-08-17T23:02:12Z", Msg: grokBillingLogMessage}
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
		rec := grokBillingRecord{TS: "2026-08-17T23:02:12Z", Msg: grokBillingLogMessage}
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
		rec := grokBillingRecord{TS: "2026-08-17T23:02:12Z", Msg: grokBillingLogMessage}
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
	assertGrokMetricsJSON(t, usage.Metrics,
		`[{"kind":"weekly","label":"Weekly credits","unit":"%","unknown":true}]`)
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
