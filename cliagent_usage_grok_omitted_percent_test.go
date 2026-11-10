// cliagent_usage_grok_omitted_percent_test.go
// -----------------------------------------------------------------------------
// xAI omits a ZERO creditUsagePercent.
//
// On 2026-11-09 every live read carried a percentage up to the weekly reset
// (22:28:32Z) and none after it, while Grok's own TUI drew that percent-less
// answer as "Weekly limit (SuperGrok) 0%". The card showed an empty dashed bar
// ("reported no usage figure") for a week that had simply not been used yet.
//
// An ABSENT key is that zero. A key that is present but unusable (null, the
// wrong type, out of range) still says nothing, and stays Unknown.
// -----------------------------------------------------------------------------

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// grokOmittedBillingBody is the live answer seen on 2026-11-09 after the
// weekly reset: the period and the wrapped pools, no creditUsagePercent.
func grokOmittedBillingBody(end time.Time) string {
	return `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-11-09T22:28:32.746607+00:00",` +
		`"end":"` + end.UTC().Format(time.RFC3339Nano) + `"},"onDemandCap":{"val":0},"onDemandUsed":{"val":0},` +
		`"prepaidBalance":{"val":0},"isUnifiedBillingUser":true}}`
}

func TestProbeGrokBillingLive_OmittedPercentReadsAsZeroUsed(t *testing.T) {
	home := isolateGrok(t)
	now := time.Now()
	writeGrokAuth(t, home, "access-A", "dan@example.com", now.Add(5*time.Hour))
	grokBillingServer(t, func(string) (int, string) {
		return http.StatusOK, grokOmittedBillingBody(now.Add(72 * time.Hour))
	})

	if got := probeGrokBillingLive(context.Background(), "", time.Now); got != grokLiveOutcomeOK {
		t.Fatalf("outcome=%q, want ok", got)
	}
	if entry := readGrokLiveCacheFile(t); entry.UsedPercent != nil || !entry.UsageOmitted {
		t.Fatalf("cache = %+v, want no percentage, recorded as omitted", entry)
	}
	usage, ok := grokUsageParser{}.Parse(filepath.Dir(home), detectedCLIAgent{Detected: true}, time.Now())
	if !ok || len(usage.Metrics) != 1 {
		t.Fatalf("usage = %+v, want the weekly credits row", usage)
	}
	if m := usage.Metrics[0]; m.Label != "Weekly credits" || !grokZeroUsedRow(m, m.ObservedAt) || m.ObservedAt == "" {
		t.Fatalf("row = %+v, want Weekly credits at 0%% with the live read's time and the period's reset", m)
	}
}

func TestProbeGrokBillingLive_NullPercentStaysUnknown(t *testing.T) {
	home := isolateGrok(t)
	now := time.Now()
	writeGrokAuth(t, home, "access-A", "dan@example.com", now.Add(5*time.Hour))
	grokBillingServer(t, func(string) (int, string) {
		return http.StatusOK, strings.Replace(grokOmittedBillingBody(now.Add(72*time.Hour)),
			`{"config":{`, `{"config":{"creditUsagePercent":null,`, 1)
	})

	if got := probeGrokBillingLive(context.Background(), "", time.Now); got != grokLiveOutcomeOK {
		t.Fatalf("outcome=%q, want ok", got)
	}
	if entry := readGrokLiveCacheFile(t); entry.UsedPercent != nil || entry.UsageOmitted {
		t.Fatalf("cache = %+v, want no percentage and NOT omitted", entry)
	}
	usage, ok := grokUsageParser{}.Parse(filepath.Dir(home), detectedCLIAgent{Detected: true}, time.Now())
	if !ok || len(usage.Metrics) != 1 || !usage.Metrics[0].Unknown || usage.Metrics[0].ObservedAt == "" {
		t.Fatalf("usage = %+v, want a confirmed Unknown row", usage)
	}
}

// Inside one open period a real number outranks a newer percent-less answer:
// usage never falls within a period, so an omitted figure there cannot hide it.
func TestGrokLiveCache_OmittedAnswerKeepsTheSamePeriodsNumber(t *testing.T) {
	h := newGrokDebtHarness(t)
	withCodexGenerationEpoch(t, 8120)
	at := h.clock()
	end := at.Add(72 * time.Hour)
	saveGrokBillingLive(grokLiveEntry("fp-ada", at, floatPtr(20), end))
	omitted := grokBillingSnapshot{ObservedAt: at.Add(10 * time.Minute), PeriodType: "USAGE_PERIOD_TYPE_WEEKLY",
		PeriodEnd: end, HasPeriodEnd: true, UsageOmitted: true}
	saveGrokBillingLive(grokBillingLiveFromSnapshot(omitted, "fp-ada"))

	entry := readGrokLiveCacheFile(t)
	if entry.UsedPercent == nil || *entry.UsedPercent != 20 || entry.UsageOmitted {
		t.Fatalf("cache = %+v, want 20%% kept and the omission not recorded over it", entry)
	}
	snap, ok := loadGrokBillingLiveSnapshot("fp-ada", at.Add(15*time.Minute))
	if !ok || !snap.HasUsedPercent || snap.UsedPercent != 20 || snap.UsageOmitted {
		t.Fatalf("snap = %+v, want the kept 20%%", snap)
	}
}

// Across a weekly reset the omitted answer is the new period's 0%: a reading
// versioned like a number (never a clear), published under its generation so
// terminal-service applies it.
func TestGrokUsage_ARolloverToAnOmittedPercentPublishesAVersionedZero(t *testing.T) {
	a := newGrokAcceptance(t, 7320)
	fingerprint := grokAccountFingerprintFor(a.home)
	a.start()
	saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
		ObservedAt: time.Now().Add(-2 * time.Hour), PeriodType: "USAGE_PERIOD_TYPE_WEEKLY",
		UsedPercent: 100, HasUsedPercent: true, PeriodEnd: time.Now().Add(-time.Hour), HasPeriodEnd: true,
	}, fingerprint))
	saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
		ObservedAt: time.Now().Add(-time.Minute), PeriodType: "USAGE_PERIOD_TYPE_WEEKLY",
		PeriodEnd: time.Now().Add(7 * 24 * time.Hour), HasPeriodEnd: true, UsageOmitted: true,
	}, fingerprint))
	if entry := readGrokLiveCacheFile(t); entry.Cleared || !entry.UsageOmitted || entry.Generation == nil || *entry.Generation != gen(7320, 2) {
		t.Fatalf("cache = %+v, want the omitted answer versioned as a reading (not a clear) at {7320,2}", entry)
	}

	usage, ok := grokUsageParser{}.Parse("", detectedCLIAgent{Detected: true}, time.Now())
	if !ok || len(usage.Metrics) != 1 || !grokZeroUsedRow(usage.Metrics[0], usage.Metrics[0].ObservedAt) {
		t.Fatalf("usage = %+v, want the new period at 0%%", usage)
	}
	if usage.UsageGeneration == nil || *usage.UsageGeneration != gen(7320, 2) {
		t.Fatalf("usageGeneration = %+v, want {7320,2} published with the 0%%", usage.UsageGeneration)
	}
}

// Every omitted zero after the first is a reading too: a newer one in the same
// period and a second rollover each advance and are hinted, so a new reset
// never rides the generation terminal-service already applied. A number kept
// over a later omitted answer in its own period advances nothing.
func TestGrokUsage_ConsecutiveOmittedZerosAcrossPeriodsAdvanceAndHint(t *testing.T) {
	a := newGrokAcceptance(t, 7330)
	fingerprint := grokAccountFingerprintFor(a.home)
	a.start()
	now := time.Now()
	weekB := now.Add(7 * 24 * time.Hour)
	weekC := now.Add(14 * 24 * time.Hour)
	save := func(at time.Time, end time.Time, percent *float64) {
		t.Helper()
		snap := grokBillingSnapshot{ObservedAt: at, PeriodType: "USAGE_PERIOD_TYPE_WEEKLY",
			PeriodEnd: end, HasPeriodEnd: true, UsageOmitted: percent == nil}
		if percent != nil {
			snap.UsedPercent, snap.HasUsedPercent = *percent, true
		}
		saveGrokBillingLive(grokBillingLiveFromSnapshot(snap, fingerprint))
	}
	wantGen := func(counter int64, why string) {
		t.Helper()
		entry := readGrokLiveCacheFile(t)
		if entry.Generation == nil || *entry.Generation != gen(7330, counter) || entry.Cleared {
			t.Fatalf("%s: cache = %+v, want {7330,%d} and no clear", why, entry, counter)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			hints := a.grokHints()
			if len(hints) > 0 && hints[len(hints)-1].Generation == counter {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: grok hints = %+v, want generation %d hinted", why, hints, counter)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	save(now.Add(-3*time.Hour), now.Add(-time.Hour), floatPtr(100)) // week A, spent
	wantGen(1, "week A's number")
	save(now.Add(-50*time.Minute), weekB, nil)
	wantGen(2, "the rollover to week B's zero")
	save(now.Add(-40*time.Minute), weekB, nil)
	wantGen(3, "a newer zero in week B")
	save(now.Add(-30*time.Minute), weekC, nil)
	wantGen(4, "a second rollover, to week C's zero")

	usage, ok := grokUsageParser{}.Parse("", detectedCLIAgent{Detected: true}, time.Now())
	if !ok || len(usage.Metrics) != 1 || !grokZeroUsedRow(usage.Metrics[0], usage.Metrics[0].ObservedAt) ||
		usage.Metrics[0].ResetAt != weekC.UTC().Format(time.RFC3339) {
		t.Fatalf("usage = %+v, want week C at 0%%", usage)
	}
	if usage.UsageGeneration == nil || *usage.UsageGeneration != gen(7330, 4) {
		t.Fatalf("usageGeneration = %+v, want week C's zero published as {7330,4}", usage.UsageGeneration)
	}

	// A real number in week C, then an omitted answer in the same period: the
	// number is kept and the omitted answer advances nothing.
	save(now.Add(-20*time.Minute), weekC, floatPtr(12))
	wantGen(5, "week C's number")
	save(now.Add(-10*time.Minute), weekC, nil)
	if entry := readGrokLiveCacheFile(t); entry.UsedPercent == nil || *entry.UsedPercent != 12 ||
		entry.Generation == nil || *entry.Generation != gen(7330, 5) {
		t.Fatalf("cache = %+v, want 12%% kept at {7330,5}", entry)
	}
}

// A cache written before UsageOmitted existed cannot say which kind of
// percent-less answer it holds, so it stays Unknown until the next read.
func TestGrokLiveCache_AnEntryFromBeforeTheFlagStaysUnknown(t *testing.T) {
	h := newGrokDebtHarness(t)
	withCodexGenerationEpoch(t, 8121)
	at := h.clock()
	saveGrokBillingLive(grokLiveEntry("fp-ada", at, nil, at.Add(72*time.Hour)))
	snap, ok := loadGrokBillingLiveSnapshot("fp-ada", at.Add(time.Minute))
	if !ok || snap.HasUsedPercent || snap.UsageOmitted {
		t.Fatalf("snap = %+v, want a percent-less reading NOT read as omitted", snap)
	}
	if m := grokBillingMetrics(snap, at.Add(time.Minute)); len(m) != 1 || !m[0].Unknown {
		t.Fatalf("metrics = %+v, want Unknown", m)
	}
}

// The managed merge re-writes a run's record into the persistent log. An
// omitted figure must come back omitted, and an unusable one must not turn
// into xAI's zero on the way.
func TestGrokManagedBillingPayload_KeepsOmittedAndUnusableApart(t *testing.T) {
	end := time.Date(2126, 8, 24, 22, 28, 32, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		omitted bool
	}{{"omitted", true}, {"unusable", false}} {
		t.Run(tc.name, func(t *testing.T) {
			snap := grokBillingSnapshot{ObservedAt: time.Date(2026, 8, 17, 23, 2, 12, 0, time.UTC),
				PeriodType: "USAGE_PERIOD_TYPE_WEEKLY", PeriodEnd: end, HasPeriodEnd: true, UsageOmitted: tc.omitted}
			payload, err := grokManagedBillingPayload("acct-1", snap)
			if err != nil {
				t.Fatal(err)
			}
			lines := bytes.Split(bytes.TrimSpace(payload), []byte("\n"))
			var rec grokBillingRecord
			if err := json.Unmarshal(lines[len(lines)-1], &rec); err != nil {
				t.Fatal(err)
			}
			back, ok := grokBillingSnapshotFromRecord(rec)
			if !ok || back.HasUsedPercent || back.UsageOmitted != tc.omitted {
				t.Fatalf("round trip = %+v, want UsageOmitted=%t", back, tc.omitted)
			}
		})
	}
}
