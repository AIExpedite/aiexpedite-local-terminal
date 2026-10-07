// cliagent_usage_opencode_capture_test.go — the OpenCode usage tap and ledger.
//
// Every row drives the real ledger file through its env seam, so nothing
// touches the machine's own config dir, and the reconcile seam is stubbed so
// no test spawns `opencode`. Run with -race.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

/* ───────────────────────────── shared fixture ──────────────────────────── */

// The instants the recorded run streams in testdata/opencode_usage carry. A
// test that feeds one pins its clock to the stream's own day: the ledger
// attributes a message to the local day of its SOURCE event time, so a clock
// elsewhere in the week would (correctly) drop every row as out of retention —
// and the assertion would be about retention rather than the tap.
var (
	openCodeFixtureRunAt       = time.UnixMilli(1790500000200)
	openCodeFixtureStringRunAt = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
)

// openCodeUsageFixture isolates the ledger and freshness files, pins the clock,
// enables the refresh, and stubs the CLI so no test can reach a real install.
// The returned function moves the pinned clock.
func openCodeUsageFixture(t *testing.T, at time.Time) func(time.Time) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(openCodeUsageLedgerEnv, filepath.Join(dir, "opencode_usage.json"))
	t.Setenv(openCodeUsageFreshnessEnv, filepath.Join(dir, "opencode_usage_freshness.json"))
	// And the session store the discovery walk stats: without this, a row that
	// drives the parser reads the REAL machine's ~/.local/share/opencode, and a
	// developer who has used OpenCode today gets a nudge — which starts a
	// background pass that writes this test's ledger while it is asserting on
	// it. A row that wants a populated store sets OPENCODE_DATA itself.
	t.Setenv("OPENCODE_DATA", t.TempDir())
	t.Setenv("XDG_DATA_HOME", "")

	now := at
	prevNow := openCodeUsageNow
	openCodeUsageNow = func() time.Time { return now }

	prevRun, prevBinary := openCodeRunCommand, openCodeUsageBinary
	// Nothing resolves a real binary unless a row says so: a developer machine
	// with OpenCode installed must not have its session store exported by the
	// unit suite.
	openCodeUsageBinary = func() string { return "" }
	openCodeRunCommand = func(context.Context, string, []string, int) ([]byte, bool, error) {
		t.Fatalf("a test spawned the OpenCode CLI without stubbing openCodeRunCommand")
		return nil, false, nil
	}

	resetOpenCodeUsageLedgerForTests()
	openCodeUsageRefreshEnabled.Store(true)
	openCodeResetRunState()

	t.Cleanup(func() {
		stopOpenCodeRunDebtRetry()
		openCodeUsageRefreshWaitFor(2 * time.Second)
		openCodeUsageRefreshEnabled.Store(false)
		openCodeUsageNow = prevNow
		openCodeRunCommand, openCodeUsageBinary = prevRun, prevBinary
		openCodeResetRunState()
		resetOpenCodeUsageLedgerForTests()
	})
	return func(next time.Time) { now = next }
}

// openCodeResetRunState clears the in-process run and nudge bookkeeping tests
// share through package globals.
func openCodeResetRunState() {
	openCodeLiveRunsMu.Lock()
	openCodeLiveRuns = map[int64]int{}
	openCodeLiveRunsMu.Unlock()
	openCodeReconcileNudge.mu.Lock()
	openCodeReconcileNudge.lastAt = time.Time{}
	openCodeReconcileNudge.mu.Unlock()
	openCodeWorkerMu.Lock()
	openCodeWorkerRunning, openCodeWorkerRearm = false, false
	openCodeWorkerMu.Unlock()
}

// observeFixture feeds a testdata stream into a handle, line by line, exactly
// as the native and pipe readers do.
func observeFixture(t *testing.T, handle *openCodeRunUsage, name string) {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", "opencode_usage", name))
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		handle.Observe(scanner.Text())
	}
}

// todayTotals sums the ledger's rows for the pinned day.
func todayTotals(t *testing.T, now time.Time) (tokens, costMicros int64, rows int) {
	t.Helper()
	day := readOpenCodeUsageLedger().Days[openCodeDayKey(now)]
	if day == nil {
		return 0, 0, 0
	}
	for _, usage := range day.Messages {
		tokens += usage.In + usage.Out + usage.Reasoning
		costMicros += usage.CostMicros
	}
	return tokens, costMicros, len(day.Messages)
}

/* ──────────────────────────────── the tap ──────────────────────────────── */

func TestOpenCodeTap_SumsEveryStepOfOneMessage(t *testing.T) {
	now := openCodeFixtureRunAt
	openCodeUsageFixture(t, now)

	handle := armOpenCodeUsageRun("unit")
	if handle == nil {
		t.Fatal("arm returned nil with the refresh enabled")
	}
	observeFixture(t, handle, "run_two_steps.jsonl")
	if owed := handle.Finish(true); owed {
		t.Fatal("a clean stream carrying tokens must not owe a reconcile")
	}
	openCodeUsageRefreshWaitFor(2 * time.Second)

	// 12+9+1 then 30+11+0 — one messageID, two steps. Cache read/write are
	// deliberately NOT counted: they would make a cached turn look many times
	// larger than it was.
	tokens, costMicros, rows := todayTotals(t, now)
	if tokens != 63 || rows != 1 {
		t.Fatalf("tokens=%d rows=%d, want 63/1", tokens, rows)
	}
	if costMicros != 5000 {
		t.Fatalf("costMicros = %d, want 5000 (0.002 + 0.003)", costMicros)
	}
}

func TestOpenCodeTap_ReadsStringAndNullSpellings(t *testing.T) {
	// OpenCode has spelled these fields as strings, numbers and null across
	// releases; an unreadable field is dropped on its own rather than failing
	// the whole frame.
	now := openCodeFixtureStringRunAt
	openCodeUsageFixture(t, now)

	handle := armOpenCodeUsageRun("unit")
	observeFixture(t, handle, "run_string_shapes.jsonl")
	handle.Finish(true)
	openCodeUsageRefreshWaitFor(2 * time.Second)

	// `"input":"7"` and `"write":"x"` are not numbers, so only reasoning=5
	// survives; `"cost":"0.01"` is likewise dropped.
	tokens, costMicros, rows := todayTotals(t, now)
	if tokens != 5 || costMicros != 0 || rows != 1 {
		t.Fatalf("tokens=%d cost=%d rows=%d, want 5/0/1", tokens, costMicros, rows)
	}
}

func TestOpenCodeTap_AnUnattributedStepIsNotCountedAndOwesAReconcile(t *testing.T) {
	now := openCodeFixtureRunAt
	openCodeUsageFixture(t, now)

	handle := armOpenCodeUsageRun("unit")
	observeFixture(t, handle, "run_unattributed_step.jsonl")
	if owed := handle.Finish(true); !owed {
		t.Fatal("a step with no messageID must leave the run owing a reconcile")
	}
	openCodeUsageRefreshWaitFor(2 * time.Second)

	if tokens, _, rows := todayTotals(t, now); tokens != 0 || rows != 0 {
		t.Fatalf("tokens=%d rows=%d, want nothing counted", tokens, rows)
	}
}

func TestOpenCodeTap_ACutOffStreamOwesAReconcileAndKeepsWhatItSaw(t *testing.T) {
	now := openCodeFixtureRunAt
	openCodeUsageFixture(t, now)

	handle := armOpenCodeUsageRun("unit")
	observeFixture(t, handle, "run_two_steps.jsonl")
	// cleanEnd=false is a timeout, a kill or an overflow: the figures we DID
	// see still count, and the run owes one reconcile for what we did not.
	if owed := handle.Finish(false); !owed {
		t.Fatal("a cut-off stream must owe a reconcile")
	}
	openCodeUsageRefreshWaitFor(2 * time.Second)
	if tokens, _, _ := todayTotals(t, now); tokens != 63 {
		t.Fatalf("tokens = %d, want the 63 the stream did report", tokens)
	}
}

func TestOpenCodeTap_AnErrorOnlyStreamCountsNothing(t *testing.T) {
	now := openCodeFixtureRunAt
	openCodeUsageFixture(t, now)

	handle := armOpenCodeUsageRun("unit")
	observeFixture(t, handle, "run_error.jsonl")
	if owed := handle.Finish(true); !owed {
		t.Fatal("a stream with no step_finish must owe a reconcile")
	}
	if _, _, rows := todayTotals(t, now); rows != 0 {
		t.Fatalf("rows = %d, want none", rows)
	}
}

func TestOpenCodeTap_ANilHandleIsANoOp(t *testing.T) {
	// Every arm site relies on this: a disabled refresh, another command, or a
	// spend-free subcommand returns nil and the site needs no branch.
	var handle *openCodeRunUsage
	handle.Observe(`{"type":"step_finish","part":{"messageID":"m","tokens":{"input":5}}}`)
	handle.Disarm()
	if handle.Finish(true) {
		t.Fatal("a nil handle must owe nothing")
	}
}

func TestOpenCodeArmForCommand_SkipsSpendFreeSubcommands(t *testing.T) {
	openCodeUsageFixture(t, time.Now())
	for _, args := range [][]string{
		{"models"}, {"auth", "list"}, {"--version"}, {"session", "list", "--format", "json"},
		{"export", "ses_1"}, {"--help"},
	} {
		if handle := armOpenCodeUsageForCommand("unit", "opencode", args); handle != nil {
			handle.Disarm()
			t.Fatalf("armed a usage run for the spend-free invocation %v", args)
		}
	}
	// A reconcile's own two commands are in that list, so a pass can never
	// create the debt it exists to pay.
	if handle := armOpenCodeUsageForCommand("unit", "opencode", []string{"run", "--format", "json"}); handle == nil {
		t.Fatal("a real run must arm")
	} else {
		handle.Disarm()
	}
	if handle := armOpenCodeUsageForCommand("unit", "codex", []string{"exec"}); handle != nil {
		t.Fatal("armed a usage run for another CLI")
	}
}

func TestOpenCodeArm_DisabledRefreshArmsNothing(t *testing.T) {
	openCodeUsageFixture(t, time.Now())
	openCodeUsageRefreshEnabled.Store(false)
	if handle := armOpenCodeUsageRun("unit"); handle != nil {
		t.Fatal("armed a run with the refresh disabled — a test must never spawn the CLI")
	}
}

/* ─────────────────────────────── the merge ─────────────────────────────── */

func TestOpenCodeMerge_KeepsThePerFieldMaximum(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)

	// The stream's summed figure, then an export of the SAME message. Neither
	// source may be added to the other.
	merge := func(usage openCodeMessageUsage) bool {
		changed := false
		updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
			changed = openCodeMergeObservations(l, []openCodeObservedMessage{{
				SessionID: "ses_1", MessageID: "msg_1", EventAt: now, Usage: usage,
			}}, now.UnixMilli())
			return openCodeLedgerEdit{TotalsChanged: changed}
		})
		return changed
	}
	merge(openCodeMessageUsage{In: 42, Out: 20, CostMicros: 4000})
	if !merge(openCodeMessageUsage{In: 42, Out: 31, CostMicros: 4000}) {
		t.Fatal("a higher output must raise the row")
	}
	if tokens, cost, rows := todayTotals(t, now); tokens != 73 || cost != 4000 || rows != 1 {
		t.Fatalf("tokens=%d cost=%d rows=%d, want 73/4000/1", tokens, cost, rows)
	}

	// A source reporting LESS leaves the higher figure standing. That can
	// over-count the one message, which is the documented trade against
	// dropping steps — pinned here so a change to it is deliberate.
	if merge(openCodeMessageUsage{In: 1, Out: 1, CostMicros: 1}) {
		t.Fatal("a lower observation must not move the generation")
	}
	if tokens, _, _ := todayTotals(t, now); tokens != 73 {
		t.Fatalf("tokens = %d, want the earlier, higher 73", tokens)
	}
}

func TestOpenCodeMerge_OneMessageKeepsOneDayAcrossMidnight(t *testing.T) {
	// A turn that starts at 23:59 and is exported after midnight must not be
	// counted on both days.
	before := time.Date(2026, 9, 30, 23, 59, 0, 0, time.Local)
	after := before.Add(2 * time.Minute)
	setNow := openCodeUsageFixture(t, before)

	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		return openCodeLedgerEdit{TotalsChanged: openCodeMergeObservations(l, []openCodeObservedMessage{{
			SessionID: "ses_1", MessageID: "msg_1", EventAt: before,
			Usage: openCodeMessageUsage{In: 10},
		}}, before.UnixMilli())}
	})
	setNow(after)
	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		return openCodeLedgerEdit{TotalsChanged: openCodeMergeObservations(l, []openCodeObservedMessage{{
			// The export's own time.created, which is still yesterday.
			SessionID: "ses_1", MessageID: "msg_1", EventAt: before,
			Usage: openCodeMessageUsage{In: 10, Out: 4},
		}}, after.UnixMilli())}
	})

	ledger := readOpenCodeUsageLedger()
	yesterday := ledger.Days[openCodeDayKey(before)]
	today := ledger.Days[openCodeDayKey(after)]
	if yesterday == nil || len(yesterday.Messages) != 1 {
		t.Fatalf("the message left its own day: %+v", yesterday)
	}
	if today != nil && len(today.Messages) != 0 {
		t.Fatalf("the message was counted twice: %+v", today)
	}
	for _, usage := range yesterday.Messages {
		if usage.In != 10 || usage.Out != 4 {
			t.Fatalf("row = %+v, want the merged max", usage)
		}
		// observedAtMs is the COMMIT time, so a row never claims to be older
		// than the run it covers.
		if usage.ObservedAtMs != after.UnixMilli() {
			t.Fatalf("observedAtMs = %d, want the commit time %d", usage.ObservedAtMs, after.UnixMilli())
		}
	}
}

func TestOpenCodeMerge_ClampsAndDropsNonsenseValues(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		return openCodeLedgerEdit{TotalsChanged: openCodeMergeObservations(l, []openCodeObservedMessage{{
			SessionID: "s", MessageID: "m", EventAt: now,
			Usage: openCodeMessageUsage{In: -5, Out: openCodeUsageValueCeiling * 4, Reasoning: 3},
		}}, now.UnixMilli())}
	})
	tokens, _, _ := todayTotals(t, now)
	if tokens != openCodeUsageValueCeiling+3 {
		t.Fatalf("tokens = %d, want the clamped ceiling plus 3 (the negative dropped)", tokens)
	}
}

func TestOpenCodeLedger_RowCapMarksTheDayPartial(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)

	observations := make([]openCodeObservedMessage, 0, openCodeLedgerMaxMessagesPerDay+2)
	for i := 0; i < openCodeLedgerMaxMessagesPerDay+2; i++ {
		observations = append(observations, openCodeObservedMessage{
			SessionID: "ses_1", MessageID: "msg_" + strings.Repeat("x", i%7) + strconv.Itoa(i),
			EventAt: now, Usage: openCodeMessageUsage{In: 1},
		})
	}
	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		return openCodeLedgerEdit{TotalsChanged: openCodeMergeObservations(l, observations, now.UnixMilli())}
	})

	day := readOpenCodeUsageLedger().Days[openCodeDayKey(now)]
	if len(day.Messages) != openCodeLedgerMaxMessagesPerDay {
		t.Fatalf("rows = %d, want the cap %d", len(day.Messages), openCodeLedgerMaxMessagesPerDay)
	}
	if !day.Partial {
		t.Fatal("past the row cap the day must be a lower bound, not a silently low number")
	}
}

func TestOpenCodeLedger_PrunesDaysOutsideRetention(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	stale := openCodeDayKey(now.AddDate(0, 0, -9))
	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		openCodeEnsureDay(l, stale).Messages["deadbeefdeadbeef"] = openCodeMessageUsage{In: 1}
		openCodeEnsureDay(l, openCodeDayKey(now)).Messages["cafebabecafebabe"] = openCodeMessageUsage{In: 2}
		return openCodeLedgerEdit{Changed: true}
	})
	ledger := readOpenCodeUsageLedger()
	if _, held := ledger.Days[stale]; held {
		t.Fatal("a day outside the two-day window survived a persist")
	}
	if len(ledger.Days) != 1 {
		t.Fatalf("days = %d, want 1", len(ledger.Days))
	}
}

/* ───────────────────────────── the generation ──────────────────────────── */

func TestOpenCodeGeneration_MovesOnlyWhenADayTotalChanges(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)

	before := readOpenCodeUsageLedger().Generation
	// A pass stamp changes the file but not what the card renders.
	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		l.LastPassStartedAtMs = now.UnixMilli()
		return openCodeLedgerEdit{Changed: true}
	})
	if got := readOpenCodeUsageLedger().Generation; got != before {
		t.Fatalf("generation moved on a non-totals write: %+v -> %+v", before, got)
	}

	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		return openCodeLedgerEdit{TotalsChanged: openCodeMergeObservations(l, []openCodeObservedMessage{{
			SessionID: "s", MessageID: "m", EventAt: now, Usage: openCodeMessageUsage{In: 7},
		}}, now.UnixMilli())}
	})
	after := readOpenCodeUsageLedger().Generation
	if after.Counter != 1 || after.Epoch != openCodeProcessGenerationEpoch.Load() {
		t.Fatalf("generation = %+v, want counter 1 under this process's epoch", after)
	}
}

func TestOpenCodeGeneration_EpochStaysJavaScriptSafe(t *testing.T) {
	// terminal-service bounds the field by Number.MAX_SAFE_INTEGER, so an
	// epoch above it would be rejected at the route and the reading never
	// fetched.
	const maxSafe = int64(1)<<53 - 1
	for i := 0; i < 200; i++ {
		epoch := openCodeDrawGenerationEpoch()
		if epoch < 1 || epoch > maxSafe {
			t.Fatalf("epoch %d outside [1, 2^53-1]", epoch)
		}
	}
}

func TestOpenCodeRotateGenerationEpoch_RepublishesAPreviousProcessReading(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)

	// An empty ledger has never published anything: nothing to rotate.
	if rotated, refused := openCodeRotateGenerationEpoch(now); rotated || refused {
		t.Fatalf("empty ledger: rotated=%v refused=%v, want false/false", rotated, refused)
	}

	// A reading the PREVIOUS process committed, under its own epoch.
	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		openCodeEnsureDay(l, openCodeDayKey(now)).Messages["0123456789abcdef"] =
			openCodeMessageUsage{In: 10, ObservedAtMs: now.UnixMilli()}
		l.Generation = cliUsageGeneration{Epoch: 4242, Counter: 9}
		return openCodeLedgerEdit{Changed: true}
	})
	openCodeGenerationRotated.Store(false)

	if rotated, refused := openCodeRotateGenerationEpoch(now); !rotated || refused {
		t.Fatalf("rotated=%v refused=%v, want true/false", rotated, refused)
	}
	got := readOpenCodeUsageLedger().Generation
	if got.Epoch != openCodeProcessGenerationEpoch.Load() || got.Counter != 1 {
		t.Fatalf("generation = %+v, want counter 1 under this process's epoch", got)
	}
	// And the recovery check now has a numeric reading to hint.
	if openCodeUsageRecoveryGeneration() == nil {
		t.Fatal("a rotated, numeric ledger must produce a recovery hint")
	}
}

/* ───────────────────────────── published rows ──────────────────────────── */

func TestOpenCodeLedgerMetrics_PublishesTokensAndCostAsDailyCounters(t *testing.T) {
	now := openCodeFixtureRunAt
	openCodeUsageFixture(t, now)

	handle := armOpenCodeUsageRun("unit")
	observeFixture(t, handle, "run_two_steps.jsonl")
	handle.Finish(true)
	openCodeUsageRefreshWaitFor(2 * time.Second)

	metrics, generation, partial := openCodeLedgerMetrics(now)
	if partial {
		t.Fatal("a fully counted day must not carry the lower-bound notice")
	}
	if generation == nil {
		t.Fatal("a committed reading must publish its generation")
	}
	if len(metrics) != 2 {
		t.Fatalf("metrics = %+v, want Tokens today and Cost today", metrics)
	}
	tokens, cost := metrics[0], metrics[1]
	if tokens.Label != "Tokens today" || tokens.Kind != limitKindDaily || tokens.Unit != "tokens" {
		t.Fatalf("tokens row = %+v", tokens)
	}
	// A counter, not a gauge: no total (OpenCode has no limit) and no remaining.
	if tokens.Total != nil || tokens.Remaining != nil || tokens.Unknown {
		t.Fatalf("tokens row must be consumed-only: %+v", tokens)
	}
	if tokens.Consumed == nil || *tokens.Consumed != 63 {
		t.Fatalf("tokens consumed = %v, want 63", tokens.Consumed)
	}
	if want := openCodeNextLocalMidnight(now).UTC().Format(time.RFC3339); tokens.ResetAt != want {
		t.Fatalf("resetAt = %q, want the next local midnight %q", tokens.ResetAt, want)
	}
	// observedAt is the commit time of the merge, so it is never earlier than
	// the run it covers. RFC3339 drops the sub-second part, so compare at the
	// resolution the wire actually carries.
	at, err := time.Parse(time.RFC3339, tokens.ObservedAt)
	if err != nil || at.Before(now.Truncate(time.Second)) {
		t.Fatalf("observedAt = %q (err %v), want at or after the run", tokens.ObservedAt, err)
	}
	if cost.Label != "Cost today" || cost.Unit != "USD" || cost.Consumed == nil || *cost.Consumed != 0.005 {
		t.Fatalf("cost row = %+v, want 0.005 USD", cost)
	}
}

func TestOpenCodeLedgerMetrics_OmitsCostWhenTheProviderReportedNone(t *testing.T) {
	// A subscription provider reports cost 0, and "$0.00 used" would mislead.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		return openCodeLedgerEdit{TotalsChanged: openCodeMergeObservations(l, []openCodeObservedMessage{{
			SessionID: "s", MessageID: "m", EventAt: now, Usage: openCodeMessageUsage{In: 5, Out: 5},
		}}, now.UnixMilli())}
	})
	metrics, _, _ := openCodeLedgerMetrics(now)
	if len(metrics) != 1 || metrics[0].Label != "Tokens today" {
		t.Fatalf("metrics = %+v, want only the token row", metrics)
	}
}

func TestOpenCodeLedgerMetrics_ZeroRowOnlyAfterAPostMidnightSuccess(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)

	// Nothing known: no row, and never an "unknown" placeholder.
	if metrics, _, _ := openCodeLedgerMetrics(now); len(metrics) != 0 {
		t.Fatalf("metrics = %+v, want none before any pass", metrics)
	}

	// A success from YESTERDAY says nothing about today.
	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		l.LastPassOutcome = openCodeReconcileOK
		l.LastSuccessfulReconcileAtMs = openCodeLocalMidnight(now).Add(-time.Hour).UnixMilli()
		return openCodeLedgerEdit{Changed: true}
	})
	if metrics, _, _ := openCodeLedgerMetrics(now); len(metrics) != 0 {
		t.Fatalf("metrics = %+v, want none for a pre-midnight success", metrics)
	}

	// A post-midnight success with nothing queued: 0 is a statement we can
	// stand behind.
	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		l.LastSuccessfulReconcileAtMs = now.UnixMilli()
		return openCodeLedgerEdit{Changed: true}
	})
	metrics, _, _ := openCodeLedgerMetrics(now)
	if len(metrics) != 1 || metrics[0].Consumed == nil || *metrics[0].Consumed != 0 {
		t.Fatalf("metrics = %+v, want one zero token row", metrics)
	}

	// A LATER `more` suppresses it again: today's sessions are still queued.
	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		l.LastPassOutcome = openCodeReconcileMore
		l.ContinuationDue = true
		return openCodeLedgerEdit{Changed: true}
	})
	if metrics, _, _ := openCodeLedgerMetrics(now); len(metrics) != 0 {
		t.Fatalf("metrics = %+v, want none while a continuation is booked", metrics)
	}
}

func TestOpenCodeLedgerMetrics_PartialDayCarriesTheLowerBoundNotice(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		openCodeMergeObservations(l, []openCodeObservedMessage{{
			SessionID: "s", MessageID: "m", EventAt: now, Usage: openCodeMessageUsage{In: 9},
		}}, now.UnixMilli())
		openCodeMarkTodayPartial(l, now)
		return openCodeLedgerEdit{Changed: true, TotalsChanged: true}
	})
	metrics, _, partial := openCodeLedgerMetrics(now)
	if !partial || len(metrics) == 0 {
		t.Fatalf("partial=%v metrics=%+v, want a notice beside a numeric row", partial, metrics)
	}
}

/* ───────────────────────────── frame decoding ──────────────────────────── */

func TestParseOpenCodeUsageFrame_ReadsOnlyWhatTheLedgerStores(t *testing.T) {
	openCodeUsageFixture(t, time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local))
	line := `{"type":"step_finish","timestamp":1790500000200,"sessionID":"ses_1",` +
		`"part":{"messageID":"msg_1","type":"step-finish","reason":"stop","cost":0.25,` +
		`"tokens":{"input":3,"output":4,"reasoning":5,"cache":{"read":6,"write":7}}}}`
	frame, ok := parseOpenCodeUsageFrame(line)
	if !ok || !frame.StepFinish {
		t.Fatalf("frame = %+v ok=%v", frame, ok)
	}
	if frame.SessionID != "ses_1" || frame.MessageID != "msg_1" {
		t.Fatalf("ids = %q/%q", frame.SessionID, frame.MessageID)
	}
	want := openCodeMessageUsage{In: 3, Out: 4, Reasoning: 5, CacheRead: 6, CacheWrite: 7, CostMicros: 250_000}
	if frame.Usage != want {
		t.Fatalf("usage = %+v, want %+v", frame.Usage, want)
	}
	if got, want := frame.EventAt.UnixMilli(), int64(1790500000200); got != want {
		t.Fatalf("eventAt = %d, want %d", got, want)
	}

	if _, ok := parseOpenCodeUsageFrame("not json"); ok {
		t.Fatal("a non-object line must not decode")
	}
	if frame, _ := parseOpenCodeUsageFrame(`{"type":"text","part":{"messageID":"m"}}`); frame.StepFinish {
		t.Fatal("a text frame is not a step completion")
	}
}

func TestIsOpenCodeStepFinishType_MatchesBothSpellings(t *testing.T) {
	// `--format json` has spelled this `step_finish` (event) and `step-finish`
	// (part) across releases; an exact allowlist would silently stop counting
	// on the next rename.
	for _, spelling := range []string{"step_finish", "step-finish", "step.finish", "STEP_FINISH"} {
		if !isOpenCodeStepFinishType(spelling) {
			t.Fatalf("%q must read as a step completion", spelling)
		}
	}
	for _, other := range []string{"step_start", "text", "session.completed", ""} {
		if isOpenCodeStepFinishType(other) {
			t.Fatalf("%q must not read as a step completion", other)
		}
	}
}

func TestOpenCodeLedger_CorruptFileReadsAsEmpty(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	if err := os.WriteFile(openCodeUsageLedgerPath(), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ledger := readOpenCodeUsageLedger(); len(ledger.Days) != 0 || ledger.Generation.Counter != 0 {
		t.Fatalf("ledger = %+v, want empty", ledger)
	}
	// And it is replaced rather than left corrupt by the next commit.
	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		return openCodeLedgerEdit{TotalsChanged: openCodeMergeObservations(l, []openCodeObservedMessage{{
			SessionID: "s", MessageID: "m", EventAt: now, Usage: openCodeMessageUsage{In: 1},
		}}, now.UnixMilli())}
	})
	raw, err := os.ReadFile(openCodeUsageLedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	var decoded openCodeUsageLedger
	if json.Unmarshal(raw, &decoded) != nil || len(decoded.Days) != 1 {
		t.Fatalf("ledger on disk = %s", raw)
	}
}

func TestOpenCodeMerge_AMessageWithNoFiguresAddsNoRow(t *testing.T) {
	// A turn whose only event named a messageID (a step_start, a text delta)
	// reports nothing. A row of zeros would count against the day's row cap and
	// move the generation, earning the backend a hint with nothing to fetch.
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)

	handle := armOpenCodeUsageRun("unit")
	handle.Observe(`{"type":"step_start","timestamp":` + strconv.FormatInt(now.UnixMilli(), 10) +
		`,"sessionID":"ses_1","part":{"messageID":"msg_1","type":"step-start"}}`)
	handle.Observe(`{"type":"text","timestamp":` + strconv.FormatInt(now.UnixMilli(), 10) +
		`,"sessionID":"ses_1","part":{"messageID":"msg_1","type":"text","text":"hi"}}`)
	// No step_finish at all, so the run also owes a reconcile.
	if !handle.Finish(true) {
		t.Fatal("a run that reported no tokens must owe a reconcile")
	}
	openCodeUsageRefreshWaitFor(2 * time.Second)

	if _, _, rows := todayTotals(t, now); rows != 0 {
		t.Fatalf("rows = %d, want none", rows)
	}
	if generation := readOpenCodeUsageLedger().Generation; generation.Counter != 0 {
		t.Fatalf("generation = %+v, want it untouched", generation)
	}
}

func TestOpenCodeMerge_AReplayedObservationMovesNothing(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	merge := func() {
		updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
			return openCodeLedgerEdit{TotalsChanged: openCodeMergeObservations(l, []openCodeObservedMessage{{
				SessionID: "s", MessageID: "m", EventAt: now,
				Usage: openCodeMessageUsage{In: 9, Out: 2},
			}}, now.UnixMilli())}
		})
	}
	merge()
	first := readOpenCodeUsageLedger().Generation
	merge()
	if again := readOpenCodeUsageLedger().Generation; again != first {
		t.Fatalf("generation moved on a replay: %+v -> %+v", first, again)
	}
}

func TestOpenCodeLedgerMetrics_UnitsAreTheOnesTheCardFormatsOn(t *testing.T) {
	// CapacityBar BRANCHES on the USD unit (two decimals rather than a whole
	// number), so this is a cross-service contract, not display text: a rename
	// here silently reverts "0.37 USD used" to "0 USD used".
	if usageUnitUSD != "USD" || usageUnitTokens != "tokens" {
		t.Fatalf("units = %q/%q, want USD/tokens — see CapacityBar.jsx USD_UNIT",
			usageUnitUSD, usageUnitTokens)
	}
	now := openCodeFixtureRunAt
	openCodeUsageFixture(t, now)
	handle := armOpenCodeUsageRun("unit")
	observeFixture(t, handle, "run_two_steps.jsonl")
	handle.Finish(true)
	openCodeUsageRefreshWaitFor(2 * time.Second)

	metrics, _, _ := openCodeLedgerMetrics(now)
	if len(metrics) != 2 || metrics[0].Unit != usageUnitTokens || metrics[1].Unit != usageUnitUSD {
		t.Fatalf("metrics = %+v, want the tokens then USD units", metrics)
	}
}

func TestOpenCodeArmForCommand_SeesThroughWrapperTransports(t *testing.T) {
	// terminal-service ships an operator-joined command to the execute and PTY
	// paths as a shell wrapper, so matching only the base program armed nothing
	// for exactly the two paths whose figures can come from NOWHERE but a
	// reconcile — their output is never tapped.
	openCodeUsageFixture(t, time.Now())

	armed := func(cmd string, args ...string) bool {
		handle := armOpenCodeUsageForCommand("local execute", cmd, args)
		if handle != nil {
			handle.Disarm()
		}
		return handle != nil
	}

	for _, tc := range []struct {
		name string
		cmd  string
		args []string
	}{
		{"posix bash -c", "bash", []string{"-c", `cd /repo && opencode run --format json`}},
		{"a login shell", "/bin/sh", []string{"-lc", `opencode run -m x`}},
		{"powershell -Command", "powershell", []string{"-Command", `cd C:\repo; opencode run`}},
		{"cmd /c", "cmd", []string{"/c", `opencode run --format json`}},
		{"an env prefix before the CLI", "bash", []string{"-c", `FOO=bar opencode run`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !armed(tc.cmd, tc.args...) {
				t.Fatalf("a wrapped OpenCode run armed nothing: %s %v", tc.cmd, tc.args)
			}
		})
	}

	// A wrapper that runs something else, and a MENTION rather than a program,
	// must not arm: a spurious debt is cheap but not free.
	for _, tc := range []struct {
		name string
		cmd  string
		args []string
	}{
		{"another CLI", "bash", []string{"-c", "codex exec hello"}},
		{"a mention in an argument", "bash", []string{"-c", `git log --grep opencode`}},
		{"no script at all", "bash", []string{"-c", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if armed(tc.cmd, tc.args...) {
				t.Fatalf("armed a usage run for %s %v", tc.cmd, tc.args)
			}
		})
	}
}

func TestCommandRunsCLI_KeepsEachCLIsAnswerSeparate(t *testing.T) {
	// One scan, two predicates: a second agent must not inherit the first's
	// answer just because they share the wrapper machinery.
	wrapped := []string{"-c", "opencode run --format json"}
	if !commandRunsOpenCode("bash", wrapped) {
		t.Fatal("commandRunsOpenCode missed a wrapped opencode")
	}
	if commandRunsAntigravity("bash", wrapped) {
		t.Fatal("commandRunsAntigravity matched a wrapped opencode")
	}
	agy := []string{"-c", "agy -p hi"}
	if !commandRunsAntigravity("bash", agy) {
		t.Fatal("commandRunsAntigravity missed a wrapped agy")
	}
	if commandRunsOpenCode("bash", agy) {
		t.Fatal("commandRunsOpenCode matched a wrapped agy")
	}
	// An oversized payload classifies as "not this CLI" rather than growing the
	// decode budget — the bound is shared, so prove it still holds.
	huge := []string{"-c", strings.Repeat("x", wrappedCommandClassifyMaxPayloadBytes+1) + "; opencode run"}
	if commandRunsOpenCode("bash", huge) {
		t.Fatal("an oversized payload must classify as not-this-CLI")
	}
}
