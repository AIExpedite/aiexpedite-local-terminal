package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_opencode_confirmed_test.go — "Tokens today" freshness. The
   direct reader CONFIRMS the store — nothing was written that the ledger does
   not hold — after a scan that read it whole and on every unchanged poll after
   one, in memory only. The card publishes the confirmation as observedAt while
   the day is covered and settled, so an idle computer's reading does not age
   into "Stale"; nothing incomplete confirms, and nothing reaches the ledger.
   ------------------------------------------------------------------------ */

// openCodeConfirmedFixture is openCodeDirectFixture with the poll's gates open
// (OpenCode detected) and no settle window: the test store's directories carry
// the real clock's mtime, which would otherwise hold every first scan open.
func openCodeConfirmedFixture(t *testing.T, epoch int64) (*openCodeTestStore, *openCodeDirectTestClock) {
	t.Helper()
	_, store, clock := openCodeDirectFixture(t, epoch)
	prevDetected, prevSettle := openCodeDirectDetected, openCodeDirectSettleWindow
	openCodeDirectDetected = func() bool { return true }
	openCodeDirectSettleWindow = 0
	t.Cleanup(func() { openCodeDirectDetected, openCodeDirectSettleWindow = prevDetected, prevSettle })
	return store, clock
}

// writeOpenCodeConfirmedSpend stores one completed assistant message that
// finished at stepMs, and returns stepMs.
func writeOpenCodeConfirmedSpend(store *openCodeTestStore, session, id string, stepMs, tokens int64) int64 {
	store.write(openCodeTestMessage{session: session, id: id, createdMs: stepMs - 1000, completedMs: stepMs, steps: [][3]int64{{tokens, 0, 0}}})
	return stepMs
}

// openCodeConfirmedRows is what the card's gather publishes for OpenCode.
func openCodeConfirmedRows(t *testing.T, now time.Time) []cliAgentUsageMetric {
	t.Helper()
	usage, _ := openCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{Path: "opencode"}, now)
	return usage.Metrics
}

func openCodeRFC3339(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

// A scan that reads the store whole confirms it as of the scan's start, a scan
// that finds nothing new confirms again, and so does every poll that finds the
// marker unchanged after one. The card publishes the newest confirmation on
// both rows instead of the hour-old step.
func TestOpenCodeConfirmed_ACompleteScanAndTheUnchangedPollsAfterItConfirm(t *testing.T) {
	store, clock := openCodeConfirmedFixture(t, 5101)
	stubOpenCodeReadiness(t, "anthropic/claude-sonnet-4-5\n", true)
	stepMs := writeOpenCodeConfirmedSpend(store, "ses_a", "msg_a", clock.ms(-time.Hour), 40)
	store.write(openCodeTestMessage{session: "ses_a", id: "msg_paid", createdMs: stepMs, completedMs: stepMs, steps: [][3]int64{{2, 0, 0}}, cost: 0.03})

	if label := scanOpenCodeDirect(t); label != "direct_scanned" {
		t.Fatalf("scan = %q", label)
	}
	if got := openCodeDirectConfirmedAt(); got != clock.ms(0) {
		t.Fatalf("confirmed at %d, want the complete scan's start %d", got, clock.ms(0))
	}
	rows := openCodeConfirmedRows(t, clock.now)
	for _, label := range []string{"Tokens today", "Cost today"} {
		if got := openCodeMetricByLabel(t, rows, label).ObservedAt; got != openCodeRFC3339(clock.ms(0)) {
			t.Fatalf("%s observedAt = %s, want the confirmation %s, not the step %s", label, got, openCodeRFC3339(clock.ms(0)), openCodeRFC3339(stepMs))
		}
	}

	// A scan a minute later finds nothing new: direct_unchanged confirms too.
	clock.now = clock.now.Add(time.Minute)
	if label := scanOpenCodeDirect(t); label != "direct_unchanged" {
		t.Fatalf("idle scan = %q", label)
	}
	if got := openCodeDirectConfirmedAt(); got != clock.ms(0) {
		t.Fatalf("confirmed at %d after an unchanged scan, want %d", got, clock.ms(0))
	}

	// An idle hour of polls: each one finds the marker unchanged and confirms.
	for i := 0; i < 3; i++ {
		clock.now = clock.now.Add(20 * time.Minute)
		openCodeDirectScanIfChanged()
		if got := openCodeDirectConfirmedAt(); got != clock.ms(0) {
			t.Fatalf("poll %d: confirmed at %d, want %d", i, got, clock.ms(0))
		}
	}
	if got := openCodeMetricByLabel(t, openCodeConfirmedRows(t, clock.now), "Tokens today").ObservedAt; got != openCodeRFC3339(clock.ms(0)) {
		t.Fatalf("observedAt after an idle hour = %s, want the last poll %s", got, openCodeRFC3339(clock.ms(0)))
	}
}

// An unknown-layout scan reads nothing and leaves no work, so the marker stays
// put afterwards — the trap a "fresh" signal must not fall into. It confirms
// nothing, and neither do the unchanged polls after it: the card keeps the
// last real confirmation and ages from there.
func TestOpenCodeConfirmed_AnUnknownLayoutAfterACoveredScanConfirmsNothing(t *testing.T) {
	store, clock := openCodeConfirmedFixture(t, 5102)
	stubOpenCodeReadiness(t, "anthropic/claude-sonnet-4-5\n", true)
	writeOpenCodeConfirmedSpend(store, "ses_a", "msg_a", clock.ms(-time.Hour), 40)
	if label := scanOpenCodeDirect(t); label != "direct_scanned" {
		t.Fatalf("covered scan = %q", label)
	}
	confirmed := clock.ms(0)

	// Records in a shape no adapter knows (cliagent_usage_opencode_direct_test.go).
	clock.now = clock.now.Add(time.Minute)
	store.writeFile(filepath.Join(store.root, "storage", "message", "ses_b", "msg_b.json"), `{"kind":"something else"}`, clock.ms(-time.Second))
	if label := scanOpenCodeDirect(t); label != "direct_layout_unknown" {
		t.Fatalf("scan = %q, want the unknown layout", label)
	}
	for i := 0; i < 3; i++ {
		clock.now = clock.now.Add(20 * time.Minute)
		openCodeDirectScanIfChanged()
	}
	if got := openCodeDirectConfirmedAt(); got != confirmed {
		t.Fatalf("confirmed at %d, want the covered scan's %d", got, confirmed)
	}
	if got := openCodeMetricByLabel(t, openCodeConfirmedRows(t, clock.now), "Tokens today").ObservedAt; got != openCodeRFC3339(confirmed) {
		t.Fatalf("observedAt = %s, want the last real confirmation %s", got, openCodeRFC3339(confirmed))
	}
}

// A scan that did not read the store whole, or left work, confirms nothing.
func TestOpenCodeConfirmed_AnIncompleteScanConfirmsNothing(t *testing.T) {
	cases := []struct {
		name string
		// setup prepares the store and returns the scan to run.
		setup     func(t *testing.T, store *openCodeTestStore, clock *openCodeDirectTestClock) func() string
		wantLabel string
		// idleAfter: the scan leaves no work, so the next poll finds the marker
		// unchanged — and must not confirm either.
		idleAfter bool
	}{
		{"a capped scan", func(t *testing.T, store *openCodeTestStore, clock *openCodeDirectTestClock) func() string {
			for i, id := range []string{"msg_0", "msg_1", "msg_2"} {
				writeOpenCodeConfirmedSpend(store, "ses_a", id, clock.ms(time.Duration(i-10)*time.Minute), 1)
			}
			openCodeDirectMaxRecords = 2
			return func() string { return scanOpenCodeDirect(t) }
		}, "direct_truncated", false},
		{"a skipped record", func(t *testing.T, store *openCodeTestStore, clock *openCodeDirectTestClock) func() string {
			store.writeFile(filepath.Join(store.root, "storage", "message", "ses_a", "msg_garbage.json"), `{not json`, clock.ms(-time.Hour))
			writeOpenCodeConfirmedSpend(store, "ses_a", "msg_ok", clock.ms(-time.Hour), 11)
			return func() string { return scanOpenCodeDirect(t) }
		}, "direct_truncated", true},
		{"a held armed run", func(t *testing.T, store *openCodeTestStore, clock *openCodeDirectTestClock) func() string {
			run := armOpenCodeUsageRun("opencode", "", "")
			t.Cleanup(func() { disarmOpenCodeUsageRun(run) })
			clock.now = clock.now.Add(2 * time.Second)
			writeOpenCodeConfirmedSpend(store, "ses_other", "msg_other", clock.ms(0), 20)
			return func() string { return scanOpenCodeDirect(t) }
		}, "direct_scanned", false},
		{"an incomplete message", func(t *testing.T, store *openCodeTestStore, clock *openCodeDirectTestClock) func() string {
			store.write(openCodeTestMessage{session: "ses_a", id: "msg_run", createdMs: clock.ms(-time.Minute), writtenMs: clock.ms(-time.Minute), steps: [][3]int64{{60, 0, 0}}})
			return func() string { return scanOpenCodeDirect(t) }
		}, "direct_scanned", false},
		{"a read failure", func(t *testing.T, store *openCodeTestStore, clock *openCodeDirectTestClock) func() string {
			writeOpenCodeConfirmedSpend(store, "ses_a", "msg_a", clock.ms(-time.Hour), 5)
			prev := openCodeDirectReaders[openCodeStoreLayoutJSON]
			openCodeDirectReaders[openCodeStoreLayoutJSON] = func(context.Context, string, int64, openCodeDirectLimits) (openCodeDirectRead, error) {
				return openCodeDirectRead{}, errors.New("store busy")
			}
			t.Cleanup(func() { openCodeDirectReaders[openCodeStoreLayoutJSON] = prev })
			return func() string { return scanOpenCodeDirect(t) }
		}, "direct_read_failed", false},
		{"a busy ledger", func(t *testing.T, store *openCodeTestStore, clock *openCodeDirectTestClock) func() string {
			prevWait := openCodeUsageLockWait
			openCodeUsageLockWait = 20 * time.Millisecond
			t.Cleanup(func() { openCodeUsageLockWait = prevWait })
			writeOpenCodeConfirmedSpend(store, "ses_a", "msg_a", clock.ms(-time.Hour), 5)
			holdOpenCodeLedgerLock(t)
			return func() string { return scanOpenCodeDirect(t) }
		}, "direct_lock_busy", false},
		{"an incomplete marker listing", func(t *testing.T, store *openCodeTestStore, clock *openCodeDirectTestClock) func() string {
			writeOpenCodeConfirmedSpend(store, "ses_a", "msg_a", clock.ms(-time.Hour), 5)
			return func() string { return openCodeDirectScanShared(0) }
		}, "direct_truncated", false},
		{"a store still settling", func(t *testing.T, store *openCodeTestStore, clock *openCodeDirectTestClock) func() string {
			// The session directory was just written on the real clock.
			openCodeDirectSettleWindow = time.Hour
			writeOpenCodeConfirmedSpend(store, "ses_a", "msg_a", clock.ms(-time.Hour), 5)
			return func() string { return scanOpenCodeDirect(t) }
		}, "direct_scanned", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store, clock := openCodeConfirmedFixture(t, 5103)
			scan := c.setup(t, store, clock)
			if label := scan(); label != c.wantLabel {
				t.Fatalf("scan = %q, want %q", label, c.wantLabel)
			}
			if got := openCodeDirectConfirmedAt(); got != 0 {
				t.Fatalf("an incomplete scan confirmed the store at %d", got)
			}
			if c.idleAfter {
				clock.now = clock.now.Add(20 * time.Minute)
				openCodeDirectScanIfChanged()
				if got := openCodeDirectConfirmedAt(); got != 0 {
					t.Fatalf("a poll after an incomplete scan confirmed the store at %d", got)
				}
			}
		})
	}
}

// Every gate the poll checks before its stat confirms nothing, and neither does
// a marker listing the poll's budget cut short.
func TestOpenCodeConfirmed_ThePollsGatesConfirmNothing(t *testing.T) {
	cases := []struct {
		name  string
		block func(t *testing.T)
	}{
		{"offline", func(t *testing.T) { setTestOffline(t, true) }},
		{"draining", func(t *testing.T) {
			drain.mu.Lock()
			drain.draining = true
			drain.mu.Unlock()
			t.Cleanup(func() { drain.mu.Lock(); drain.draining = false; drain.mu.Unlock() })
		}},
		{"not detected", func(*testing.T) { openCodeDirectDetected = func() bool { return false } }},
		{"scan disabled", func(*testing.T) { openCodeDirectScanEnabled = false }},
		{"an incomplete marker listing", func(*testing.T) { openCodeDirectTickBudget = 0 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store, clock := openCodeConfirmedFixture(t, 5104)
			writeOpenCodeConfirmedSpend(store, "ses_a", "msg_a", clock.ms(-time.Hour), 5)
			if label := scanOpenCodeDirect(t); label != "direct_scanned" {
				t.Fatalf("scan = %q", label)
			}
			confirmed := clock.ms(0)
			c.block(t)
			clock.now = clock.now.Add(20 * time.Minute)
			openCodeDirectScanIfChanged()
			if got := openCodeDirectConfirmedAt(); got != confirmed {
				t.Fatalf("confirmed at %d through a closed gate, want %d", got, confirmed)
			}
		})
	}
}

// The confirmation vouches for the day only while every run's spend is in the
// bucket: an armed or owed debt, a run whose debt write was refused, or pending
// spend a refused adoption left out keeps the newest step's time.
func TestOpenCodeConfirmed_ALiveDebtOrARefusedAdoptionKeepsTheStepTime(t *testing.T) {
	store, clock := openCodeConfirmedFixture(t, 5105)
	stubOpenCodeReadiness(t, "anthropic/claude-sonnet-4-5\n", true)
	// A first gather names the account, so the scan banks under it.
	openCodeConfirmedRows(t, clock.now)
	stepMs := writeOpenCodeConfirmedSpend(store, "ses_a", "msg_a", clock.ms(-time.Hour), 40)
	if label := scanOpenCodeDirect(t); label != "direct_scanned" {
		t.Fatalf("scan = %q", label)
	}
	confirmed := clock.ms(0)
	observedAt := func(t *testing.T) string {
		t.Helper()
		return openCodeMetricByLabel(t, openCodeConfirmedRows(t, clock.now), "Tokens today").ObservedAt
	}
	if got := observedAt(t); got != openCodeRFC3339(confirmed) {
		t.Fatalf("settled observedAt = %s, want the confirmation", got)
	}

	withDebt := func(t *testing.T, debt openCodeUsageDebt) {
		t.Helper()
		openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
			ledger.Debts = append(ledger.Debts, debt)
			return true, false
		})
		if got := observedAt(t); got != openCodeRFC3339(stepMs) {
			t.Fatalf("observedAt with a live debt = %s, want the step %s", got, openCodeRFC3339(stepMs))
		}
		openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
			ledger.Debts = nil
			return true, false
		})
		if got := observedAt(t); got != openCodeRFC3339(confirmed) {
			t.Fatalf("observedAt once the debt is gone = %s, want the confirmation", got)
		}
	}
	t.Run("an armed debt", func(t *testing.T) {
		withDebt(t, openCodeUsageDebt{RunID: "armed", RunFloorMs: clock.ms(-time.Minute)})
	})
	t.Run("an owed debt", func(t *testing.T) {
		withDebt(t, openCodeUsageDebt{RunID: "owed", RunFloorMs: clock.ms(-time.Minute), SessionID: "ses_owed", OwedAtMs: clock.ms(-30 * time.Second)})
	})
	t.Run("a run whose debt write was refused", func(t *testing.T) {
		release := holdOpenCodeLedgerLock(t)
		run := armOpenCodeUsageRun("opencode", "", "")
		release()
		if !run.armRefused.Load() || len(loadOpenCodeUsageLedger().Debts) != 0 {
			t.Fatal("the arm was not refused")
		}
		if got := observedAt(t); got != openCodeRFC3339(stepMs) {
			t.Fatalf("observedAt with a refused arm = %s, want the step", got)
		}
		disarmOpenCodeUsageRun(run)
		if got := observedAt(t); got != openCodeRFC3339(confirmed) {
			t.Fatalf("observedAt once the run is gone = %s, want the confirmation", got)
		}
	})
	t.Run("a refused adoption", func(t *testing.T) {
		prevWait := openCodeUsageLockWait
		openCodeUsageLockWait = 20 * time.Millisecond
		t.Cleanup(func() { openCodeUsageLockWait = prevWait })
		openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
			today, _ := openCodeLocalDay(clock.now)
			ledger.Buckets = append(ledger.Buckets, openCodeUsageBucket{LocalDate: today, InputTokens: 3, ObservedAtMs: stepMs - 1000})
			return true, false
		})
		release := holdOpenCodeLedgerLock(t)
		if got := observedAt(t); got != openCodeRFC3339(stepMs) {
			t.Fatalf("observedAt with pending spend left out = %s, want the step", got)
		}
		release()
		if got := observedAt(t); got != openCodeRFC3339(confirmed) {
			t.Fatalf("observedAt once adopted = %s, want the confirmation", got)
		}
	})
}

// The confirmation counts only for a covered reading, only when it is later
// than the newest step, only from today's local midnight and never from the
// future; every other case publishes the step's time.
func TestOpenCodeConfirmed_ObservedAtRule(t *testing.T) {
	prev := openCodeUsageLocation
	openCodeUsageLocation = time.UTC
	t.Cleanup(func() { openCodeUsageLocation = prev })
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	midnight := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC).UnixMilli()
	stepMs := ms(-2 * time.Hour)
	// The midnight rows isolate the midnight rule: their step is earlier still,
	// so the confirmation is later than the step either way.
	yesterdayStepMs := midnight - (2 * time.Hour).Milliseconds()
	for _, c := range []struct {
		name      string
		stepMs    int64
		direct    bool
		confirmed int64
		want      int64
	}{
		{"none", stepMs, true, 0, stepMs},
		{"later than the step", stepMs, true, ms(-time.Minute), ms(-time.Minute)},
		{"exactly now", stepMs, true, ms(0), ms(0)},
		{"not covered by the store", stepMs, false, ms(-time.Minute), stepMs},
		{"no later than the step", stepMs, true, stepMs, stepMs},
		{"before the step", stepMs, true, stepMs - 1, stepMs},
		{"before local midnight", yesterdayStepMs, true, midnight - 1, yesterdayStepMs},
		{"at local midnight", yesterdayStepMs, true, midnight, midnight},
		{"in the future", stepMs, true, ms(time.Second), stepMs},
	} {
		t.Run(c.name, func(t *testing.T) {
			bucket := openCodeUsageBucket{InputTokens: 7, CostUsd: 0.2, ObservedAtMs: c.stepMs}
			if got := openCodeUsageObservedAtMs(bucket, c.direct, c.confirmed, now); got != c.want {
				t.Fatalf("observedAt = %d, want %d", got, c.want)
			}
			rows := openCodeUsageMetrics(bucket, true, c.direct, c.confirmed, now)
			if len(rows) != 2 {
				t.Fatalf("rows = %+v, want tokens and cost", rows)
			}
			for _, row := range rows {
				if row.ObservedAt != openCodeRFC3339(c.want) {
					t.Fatalf("%s observedAt = %s, want %s", row.Label, row.ObservedAt, openCodeRFC3339(c.want))
				}
			}
		})
	}
}

// Confirming never touches the ledger: no rewrite, the bucket keeps its step
// time, the scan floor stays where it was, and no generation advances — so the
// propagator has nothing new to hint.
func TestOpenCodeConfirmed_PollsWriteAdvanceAndHintNothing(t *testing.T) {
	store, clock := openCodeConfirmedFixture(t, 5106)
	resetCLIUsagePropagator()
	t.Cleanup(resetCLIUsagePropagator)
	stubOpenCodeReadiness(t, "anthropic/claude-sonnet-4-5\n", true)
	stepMs := writeOpenCodeConfirmedSpend(store, "ses_a", "msg_a", clock.ms(-time.Hour), 40)
	if label := scanOpenCodeDirect(t); label != "direct_scanned" {
		t.Fatalf("scan = %q", label)
	}
	// The gather adopts the spend the scan banked before any account was named;
	// every write after this one would be the confirmation's.
	openCodeConfirmedRows(t, clock.now)

	path := openCodeUsageCachePath()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ledger := loadOpenCodeUsageLedger()
	generation, floor := ledger.Generation, openCodeDirectScanFloor(ledger, clock.now)
	cliUsagePropagator.mu.Lock()
	pending := cliUsagePropagator.pending[openCodeUsageProvider]
	seen := cliUsagePropagator.seen[openCodeUsageProvider]
	cliUsagePropagator.mu.Unlock()

	for i := 0; i < 5; i++ {
		clock.now = clock.now.Add(10 * time.Minute)
		openCodeDirectScanIfChanged()
		openCodeConfirmedRows(t, clock.now)
	}
	if got := openCodeDirectConfirmedAt(); got != clock.ms(0) {
		t.Fatalf("confirmed at %d, want the last poll %d", got, clock.ms(0))
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("confirming rewrote the ledger")
	}
	ledger = loadOpenCodeUsageLedger()
	for _, b := range ledger.Buckets {
		if b.ObservedAtMs != stepMs {
			t.Fatalf("bucket observedAtMs = %d, want the step %d: a confirmation leaked into the ledger", b.ObservedAtMs, stepMs)
		}
	}
	if got := openCodeDirectScanFloor(ledger, clock.now); got != floor {
		t.Fatalf("scan floor = %d, want %d", got, floor)
	}
	if ledger.Generation != generation {
		t.Fatalf("generation = %+v, want %+v: an idle poll advanced it", ledger.Generation, generation)
	}
	cliUsagePropagator.mu.Lock()
	defer cliUsagePropagator.mu.Unlock()
	if got := cliUsagePropagator.pending[openCodeUsageProvider]; got != pending {
		t.Fatalf("pending OpenCode hint = %+v, want the scan's own %+v", got, pending)
	}
	if got := cliUsagePropagator.seen[openCodeUsageProvider]; got != seen {
		t.Fatalf("seen OpenCode generation = %+v, want %+v", got, seen)
	}
}
