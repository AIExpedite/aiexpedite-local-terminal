package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_claudecode_runs_test.go — the free half of the run-freshness
   contract.
   --------------------------------------------------------------------------
   claudeUsageHarvestPrintStdout is what makes a spent turn refresh the card
   without an OAuth request, and it is the ONLY freshness source on a device
   where the probe cannot run at all. The cases below pin the three envelope
   shapes extractClaudeRateLimitBuckets supports, the non-telemetry inputs that
   must write nothing rather than error, and the bounds that keep a broken CLI's
   1 MiB spew from turning the scan into a hot loop.
   ------------------------------------------------------------------------ */

// harvestEnv isolates the rate-limit cache so a case reads only what it wrote.
func harvestEnv(t *testing.T) string {
	t.Helper()
	cache := t.TempDir() + "/rl.json"
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	return cache
}

func harvestedFiveHour(t *testing.T) (claudeRateLimitBucket, bool) {
	t.Helper()
	b, ok := loadMergedClaudeRateLimitBuckets("")[claudeWindowFiveHour]
	return b, ok
}

func TestClaudeUsageHarvestPrintStdout_RateLimitsMapEnvelope(t *testing.T) {
	harvestEnv(t)
	now := time.Now()
	stdout := []byte(fmt.Sprintf(
		`{"type":"result","subtype":"success","is_error":false,"result":"ok","rate_limits":{"five_hour":{"used_percentage":41.5,"resets_at":%d,"status":"allowed"},"seven_day":{"used_percentage":12,"resets_at":%d}}}`,
		now.Add(time.Hour).Unix(), now.Add(72*time.Hour).Unix()))

	if !claudeUsageHarvestPrintStdout(stdout, now) {
		t.Fatal("a result envelope carrying rate_limits must be harvested")
	}
	bucket, ok := harvestedFiveHour(t)
	if !ok {
		t.Fatal("five_hour window was not persisted")
	}
	if bucket.UsedPercentage < 41.4 || bucket.UsedPercentage > 41.6 {
		t.Errorf("usedPercentage = %v, want ~41.5", bucket.UsedPercentage)
	}
	if bucket.ObservedAtMs != now.UnixMilli() {
		t.Errorf("observedAtMs = %d, want the run's completion instant %d",
			bucket.ObservedAtMs, now.UnixMilli())
	}
	if _, ok := loadMergedClaudeRateLimitBuckets("")[claudeWindowSevenDay]; !ok {
		t.Error("the second window in the same envelope was dropped")
	}
}

func TestClaudeUsageHarvestPrintStdout_NestedAndFlatEventShapes(t *testing.T) {
	now := time.Now()
	for name, line := range map[string]string{
		"nested rate_limit_info": fmt.Sprintf(
			`{"type":"rate_limit_event","rate_limit_info":{"rate_limit_type":"five_hour","utilization":0.63,"resets_at":%d,"status":"allowed"}}`,
			now.Add(time.Hour).Unix()),
		"flat rate_limit_type": fmt.Sprintf(
			`{"type":"result","rate_limit_type":"five_hour","used_percentage":63,"resets_at":%d}`,
			now.Add(time.Hour).Unix()),
	} {
		t.Run(name, func(t *testing.T) {
			harvestEnv(t)
			if !claudeUsageHarvestPrintStdout([]byte(line), now) {
				t.Fatal("shape was not harvested")
			}
			bucket, ok := harvestedFiveHour(t)
			if !ok {
				t.Fatal("five_hour window was not persisted")
			}
			if bucket.UsedPercentage < 62.9 || bucket.UsedPercentage > 63.1 {
				t.Errorf("usedPercentage = %v, want ~63", bucket.UsedPercentage)
			}
		})
	}
}

// A banner line, NDJSON, and a telemetry-free envelope: the scan must find the
// reading where there is one and write nothing (silently) where there is not.
func TestClaudeUsageHarvestPrintStdout_LineTolerantAndSilentWhenEmpty(t *testing.T) {
	now := time.Now()
	telemetry := fmt.Sprintf(
		`{"type":"result","subtype":"success","rate_limits":{"five_hour":{"used_percentage":7,"resets_at":%d}}}`,
		now.Add(time.Hour).Unix())

	for name, tc := range map[string]struct {
		stdout string
		want   bool
	}{
		"banner line first":  {"Loaded plugin foo\n" + telemetry, true},
		"ndjson stream":      {`{"type":"system","subtype":"init"}` + "\n" + telemetry + "\n", true},
		"no telemetry":       {`{"type":"result","subtype":"success","result":"ok"}`, false},
		"non-JSON stdout":    {"claude: command not found", false},
		"empty stdout":       {"", false},
		"whitespace only":    {"  \n\t\n", false},
		"telemetry-ish text": {`{"note":"rate_limit mentioned but no windows"}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			harvestEnv(t)
			if got := claudeUsageHarvestPrintStdout([]byte(tc.stdout), now); got != tc.want {
				t.Fatalf("harvested = %v, want %v", got, tc.want)
			}
			_, persisted := harvestedFiveHour(t)
			if persisted != tc.want {
				t.Errorf("five_hour persisted = %v, want %v", persisted, tc.want)
			}
		})
	}
}

// The smoke retains up to claudeSmokeMaxStdout (1 MiB) from a CLI that may be
// spewing. The scan must stay bounded in lines, total bytes and per-line length
// — and must still not choke when the telemetry is out past those bounds.
func TestClaudeUsageHarvestPrintStdout_IsBounded(t *testing.T) {
	now := time.Now()
	telemetry := fmt.Sprintf(
		`{"type":"result","rate_limits":{"five_hour":{"used_percentage":55,"resets_at":%d}}}`,
		now.Add(time.Hour).Unix())

	t.Run("telemetry past the line cap is not scanned", func(t *testing.T) {
		harvestEnv(t)
		spew := strings.Repeat("noise\n", claudeUsageHarvestMaxLines+10) + telemetry
		if claudeUsageHarvestPrintStdout([]byte(spew), now) {
			t.Fatal("the scan must stop at claudeUsageHarvestMaxLines")
		}
	})

	t.Run("an oversized single line is skipped, not decoded", func(t *testing.T) {
		harvestEnv(t)
		// A syntactically valid object far past the per-line cap.
		huge := `{"type":"result","rate_limits":{"five_hour":{"used_percentage":55,"note":"` +
			strings.Repeat("x", claudeUsageHarvestMaxLineBytes) + `"}}}`
		if claudeUsageHarvestPrintStdout([]byte(huge), now) {
			t.Fatal("a line past claudeUsageHarvestMaxLineBytes must be skipped")
		}
	})

	t.Run("a 1 MiB buffer still terminates", func(t *testing.T) {
		harvestEnv(t)
		done := make(chan bool, 1)
		go func() {
			done <- claudeUsageHarvestPrintStdout(
				[]byte(strings.Repeat("{\"a\":1}\n", claudeSmokeMaxStdout/8)), now)
		}()
		select {
		case harvested := <-done:
			if harvested {
				t.Error("a buffer with no telemetry must harvest nothing")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the bounded scan did not terminate on a 1 MiB buffer")
		}
	})
}

// noteClaudeTurnSpent is the single definition of "a turn completed" shared by
// the native, managed and smoke paths. An UNARMED process (the statusline-hook
// subcommand, a one-shot verb, an opted-out user) must record nothing at all.
func TestNoteClaudeTurnSpent_RecordsTheDebtAtTheCallersInstant(t *testing.T) {
	t.Setenv(claudeUsagePendingRunEnv, t.TempDir()+"/pending.json")
	resetClaudeUsageProbeGate()
	t.Cleanup(resetClaudeUsageProbeGate)

	completed := time.Now().Add(-2 * time.Second)

	noteClaudeTurnSpent(completed, false)
	if owed := claudeUsageProbe.owedObservation(); !owed.IsZero() {
		t.Fatalf("an unarmed process recorded a debt (%s)", owed.UTC().Format(time.RFC3339Nano))
	}

	SetClaudeUsageProbeDisabled(false)
	// Offline keeps the probe itself from going anywhere; the debt is what is
	// under test, and it must be recorded regardless.
	SetOffline(true)
	t.Cleanup(func() { SetOffline(false) })

	noteClaudeTurnSpent(completed, false)
	owed := claudeUsageProbe.owedObservation()
	if !owed.Equal(completed) {
		t.Fatalf("debt baseline = %v, want the caller's completion instant %v", owed, completed)
	}

	// A turn whose own output already refreshed every displayed row owes nothing
	// — that is what keeps a healthy smoke free of an OAuth request.
	claudeUsageProbe.settleOwed(owed)
	cache := harvestEnv(t)
	later := time.Now()
	if !claudeUsageHarvestPrintStdout([]byte(fmt.Sprintf(
		`{"type":"result","rate_limits":{"five_hour":{"used_percentage":5,"resets_at":%d},`+
			`"seven_day":{"used_percentage":6,"resets_at":%d},`+
			`"seven_day_fable":{"used_percentage":7,"resets_at":%d}}}`,
		later.Add(time.Hour).Unix(), later.Add(time.Hour).Unix(), later.Add(time.Hour).Unix())), later) {
		t.Fatalf("precondition: the envelope was not harvested into %s", cache)
	}
	noteClaudeTurnSpent(later, true)
	if owed := claudeUsageProbe.owedObservation(); !owed.IsZero() {
		t.Errorf("a run covered by its own telemetry still recorded a debt (%v)", owed)
	}

	// But a PARTIAL harvest still owes: on a cache whose weekly rows already
	// carry an OLDER reading, refreshing only five_hour leaves two of the three
	// displayed rows behind the run — the row-aware half of the check.
	partialCache := harvestEnv(t)
	stale := time.Now().Add(-time.Hour)
	reset := time.Now().Add(time.Hour).UnixMilli()
	mergeClaudeRateLimitCacheFromSource(partialCache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 1, ResetsAtMs: reset, ObservedAtMs: stale.UnixMilli(), usageKnown: true,
		},
		claudeWindowSevenDay: {
			UsedPercentage: 2, ResetsAtMs: reset, ObservedAtMs: stale.UnixMilli(), usageKnown: true,
		},
		claudeWindowSevenDayFable: {
			UsedPercentage: 3, ResetsAtMs: reset, ObservedAtMs: stale.UnixMilli(), usageKnown: true,
		},
	}, stale, "", claudeRateLimitSourceStream)

	partial := time.Now()
	if !claudeUsageHarvestPrintStdout([]byte(fmt.Sprintf(
		`{"type":"result","rate_limits":{"five_hour":{"used_percentage":9,"resets_at":%d}}}`,
		partial.Add(time.Hour).Unix())), partial) {
		t.Fatal("precondition: the partial envelope was not harvested")
	}
	noteClaudeTurnSpent(partial, true)
	if owed := claudeUsageProbe.owedObservation(); owed.IsZero() {
		t.Error("a harvest that left two rows stale recorded no debt to correct them")
	}
}

// The harvest happens at the same instant the debt is recorded against, so a run
// whose own output carried telemetry is covered by its own reading — which is
// what lets a device with no usable probe still advance observedAt.
func TestClaudeUsageHarvest_CoversADebtRecordedAtTheSameInstant(t *testing.T) {
	cache := harvestEnv(t)
	now := time.Now()
	if !claudeUsageHarvestPrintStdout([]byte(fmt.Sprintf(
		`{"type":"result","rate_limits":{"five_hour":{"used_percentage":88,"resets_at":%d}}}`,
		now.Add(time.Hour).Unix())), now) {
		t.Fatal("envelope was not harvested")
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("the harvest wrote no cache: %v", err)
	}
	observed := latestClaudeObservation(loadMergedClaudeRateLimitBuckets(""))
	if !claudeUsageObservationCovers(observed, now) {
		t.Fatalf("observation %v does not cover a debt recorded at %v", observed, now)
	}
}
