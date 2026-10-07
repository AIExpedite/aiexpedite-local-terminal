// cliagent_usage_opencode_store_test.go — the bounded reconciliation through
// OpenCode's own CLI.
//
// Every row drives the pass through the openCodeRunCommand seam, so no test
// spawns a real `opencode` (CI has none) and the fixtures stand in for its
// `session list` / `export` answers. The bounds — oldest-first order, three
// exports a pass, the cursor advancing past an over-cap export, and the ONE
// retry timer per failure — are what this file pins.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

/* ───────────────────────────── the CLI double ──────────────────────────── */

// openCodeCLIStub answers `session list` and `export <id>` from per-test
// fixtures. It records every invocation so a row can assert the ORDER and the
// COUNT of exports, which is where the cost bound lives.
type openCodeCLIStub struct {
	mu sync.Mutex
	// sessions is the `session list` answer; "" means the unsupported form.
	sessions string
	// exports maps a session id to its export answer. A missing id answers the
	// empty export.
	exports map[string]string
	// oversize names sessions whose export overflows the stdout cap.
	oversize map[string]bool
	// hang names sessions whose export never returns before the deadline.
	hang map[string]bool
	// launchError makes every invocation fail the way a missing binary does.
	launchError bool
	// exitError makes every invocation exit non-zero (a build without the
	// subcommand).
	exitError bool

	calls []string
	// exportsRun counts `export` invocations, which is where the cost bound is.
	exportsRun int
}

func (s *openCodeCLIStub) record(args []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, strings.Join(args, " "))
	if args[0] == "export" {
		s.exportsRun++
	}
}

func (s *openCodeCLIStub) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *openCodeCLIStub) exportCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exportsRun
}

// install wires the stub into the pass and returns it.
func (s *openCodeCLIStub) install(t *testing.T) *openCodeCLIStub {
	t.Helper()
	if s.exports == nil {
		s.exports = map[string]string{}
	}
	openCodeUsageBinary = func() string { return "opencode-stub" }
	openCodeRunCommand = func(ctx context.Context, _ string, args []string, limit int) ([]byte, bool, error) {
		s.record(args)
		if s.launchError {
			return nil, false, errors.New("exec: not found")
		}
		if s.exitError {
			return nil, false, &exec.ExitError{}
		}
		if args[0] == "session" {
			if s.sessions == "" {
				return []byte(openCodeFixture(t, "session_list_unsupported.txt")), false, nil
			}
			return []byte(s.sessions), false, nil
		}
		id := args[1]
		if s.hang[id] {
			<-ctx.Done()
			return nil, false, ctx.Err()
		}
		if s.oversize[id] {
			// boundedBuffer stops writing AT its limit, so a filled buffer is
			// the only honest signal that bytes were dropped.
			return make([]byte, limit), true, nil
		}
		body, held := s.exports[id]
		if !held {
			body = openCodeFixture(t, "export_empty.json")
		}
		return []byte(body), false, nil
	}
	return s
}

func openCodeFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "opencode_usage", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(raw)
}

// sessionList renders a `session list --format json` answer from (id, updated)
// pairs, so a row can state the backlog it is testing inline.
func sessionList(rows ...openCodeSessionRow) string {
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		parts = append(parts, fmt.Sprintf(`{"id":%q,"time":{"updated":%d}}`, row.id, row.updatedMs))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// exportWith renders an export carrying one assistant message.
func exportWith(sessionID, messageID string, createdMs, in, out int64, cost string) string {
	return fmt.Sprintf(`{"messages":[{"info":{"id":%q,"sessionID":%q,"role":"assistant",`+
		`"time":{"created":%d},"cost":%s,"tokens":{"input":%d,"output":%d,"reasoning":0,`+
		`"cache":{"read":0,"write":0}}}}]}`, messageID, sessionID, createdMs, cost, in, out)
}

/* ───────────────────────── ordering and batching ───────────────────────── */

func TestOpenCodeReconcile_TakesChangedSessionsOldestFirstAndStopsAtThreeExports(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	created := now.UnixMilli()
	stub := (&openCodeCLIStub{
		sessions: sessionList(
			openCodeSessionRow{"ses_e", created + 5000},
			openCodeSessionRow{"ses_a", created + 1000},
			openCodeSessionRow{"ses_c", created + 3000},
			openCodeSessionRow{"ses_b", created + 2000},
			openCodeSessionRow{"ses_d", created + 4000},
		),
		exports: map[string]string{
			"ses_a": exportWith("ses_a", "msg_a", created, 10, 1, "0"),
			"ses_b": exportWith("ses_b", "msg_b", created, 20, 2, "0"),
			"ses_c": exportWith("ses_c", "msg_c", created, 30, 3, "0"),
			"ses_d": exportWith("ses_d", "msg_d", created, 40, 4, "0"),
			"ses_e": exportWith("ses_e", "msg_e", created, 50, 5, "0"),
		},
	}).install(t)

	first := reconcileOpenCodeUsageOnce(context.Background(), now)
	if first.Outcome != openCodeReconcileMore {
		t.Fatalf("outcome = %q, want %q with two sessions left", first.Outcome, openCodeReconcileMore)
	}
	if got := stub.recorded(); len(got) != 4 ||
		got[1] != "export ses_a" || got[2] != "export ses_b" || got[3] != "export ses_c" {
		t.Fatalf("calls = %v, want one list then the three OLDEST exports", got)
	}
	if tokens, _, rows := todayTotals(t, now); tokens != 66 || rows != 3 {
		t.Fatalf("tokens=%d rows=%d, want 66/3 after the first pass", tokens, rows)
	}

	// The next pass RESUMES past the cursor rather than re-exporting.
	second := reconcileOpenCodeUsageOnce(context.Background(), now)
	if second.Outcome != openCodeReconcileOK {
		t.Fatalf("second outcome = %q, want ok", second.Outcome)
	}
	if got := stub.recorded(); got[5] != "export ses_d" || got[6] != "export ses_e" {
		t.Fatalf("calls = %v, want the remaining two", got)
	}
	if tokens, _, rows := todayTotals(t, now); tokens != 165 || rows != 5 {
		t.Fatalf("tokens=%d rows=%d, want all five sessions", tokens, rows)
	}

	// Nothing changed since: a third pass exports nothing at all.
	before := stub.exportCount()
	if third := reconcileOpenCodeUsageOnce(context.Background(), now); third.Outcome != openCodeReconcileNoChange {
		t.Fatalf("third outcome = %q, want no_change", third.Outcome)
	}
	if stub.exportCount() != before {
		t.Fatal("a pass with nothing beyond the cursor must export nothing")
	}
}

func TestOpenCodeReconcile_ReadsTheWrappedSessionListShape(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	(&openCodeCLIStub{
		sessions: openCodeFixture(t, "session_list_wrapped.json"),
		exports: map[string]string{
			"ses_only": exportWith("ses_only", "msg_only", now.UnixMilli(), 7, 3, "0.5"),
		},
	}).install(t)

	if result := reconcileOpenCodeUsageOnce(context.Background(), now); result.Outcome != openCodeReconcileOK {
		t.Fatalf("outcome = %q, want ok", result.Outcome)
	}
	tokens, cost, _ := todayTotals(t, now)
	if tokens != 10 || cost != 500_000 {
		t.Fatalf("tokens=%d cost=%d, want 10/500000", tokens, cost)
	}
}

func TestOpenCodeReconcile_CountsOnlyAssistantMessages(t *testing.T) {
	// The fixture's user message carries no figures; counting it would add a
	// phantom row to the day.
	now := time.UnixMilli(1790400000600)
	openCodeUsageFixture(t, now)
	(&openCodeCLIStub{
		sessions: sessionList(openCodeSessionRow{"ses_old", now.UnixMilli()}),
		exports:  map[string]string{"ses_old": openCodeFixture(t, "export_assistant.json")},
	}).install(t)

	reconcileOpenCodeUsageOnce(context.Background(), now)
	tokens, cost, rows := todayTotals(t, now)
	if rows != 1 || tokens != 63 || cost != 4000 {
		t.Fatalf("rows=%d tokens=%d cost=%d, want 1/63/4000", rows, tokens, cost)
	}
}

func TestOpenCodeReconcile_StreamAndExportOfOneRunNeverDoubleCount(t *testing.T) {
	now := openCodeFixtureRunAt
	openCodeUsageFixture(t, now)

	// The run's own stream first (12+9+1 then 30+11 = 63 for msg_1).
	handle := armOpenCodeUsageRun("unit")
	observeFixture(t, handle, "run_two_steps.jsonl")
	handle.Finish(true)
	openCodeUsageRefreshWaitFor(2 * time.Second)

	// Then an export of the SAME session and message, reporting the same total.
	(&openCodeCLIStub{
		sessions: sessionList(openCodeSessionRow{"ses_fixture", now.UnixMilli()}),
		exports: map[string]string{
			"ses_fixture": exportWith("ses_fixture", "msg_1", now.UnixMilli(), 42, 20, "0.005"),
		},
	}).install(t)
	reconcileOpenCodeUsageOnce(context.Background(), now)

	tokens, cost, rows := todayTotals(t, now)
	if rows != 1 {
		t.Fatalf("rows = %d, want the one message both sources describe", rows)
	}
	// Per field the MAX: in 42 (export) + out 20 (stream's 20) + reasoning 1.
	if tokens != 63 || cost != 5000 {
		t.Fatalf("tokens=%d cost=%d, want the per-field max 63/5000, not a sum", tokens, cost)
	}
}

/* ────────────────────────── the over-cap export ────────────────────────── */

func TestOpenCodeReconcile_OverCapExportOfAStreamCapturedSessionJustAdvancesTheCursor(t *testing.T) {
	now := openCodeFixtureRunAt
	openCodeUsageFixture(t, now)

	handle := armOpenCodeUsageRun("unit")
	observeFixture(t, handle, "run_two_steps.jsonl")
	handle.Finish(true) // settles COVERED, recording ses_fixture
	openCodeUsageRefreshWaitFor(2 * time.Second)

	(&openCodeCLIStub{
		sessions: sessionList(openCodeSessionRow{"ses_fixture", now.UnixMilli()}),
		oversize: map[string]bool{"ses_fixture": true},
	}).install(t)
	if result := reconcileOpenCodeUsageOnce(context.Background(), now); result.Outcome != openCodeReconcileOK {
		t.Fatalf("outcome = %q, want ok", result.Outcome)
	}
	ledger := readOpenCodeUsageLedger()
	if len(ledger.Skipped) != 0 {
		t.Fatalf("skipped = %+v, want none — the stream already counted it", ledger.Skipped)
	}
	if ledger.ReconcileCursorMs != now.UnixMilli() {
		t.Fatalf("cursor = %d, want it past the session", ledger.ReconcileCursorMs)
	}
	if day := ledger.Days[openCodeDayKey(now)]; day.Partial {
		t.Fatal("a session the stream counted must not make the day a lower bound")
	}
}

func TestOpenCodeReconcile_OverCapExportElsewhereIsRememberedAndMakesTheDayPartial(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	created := now.UnixMilli()
	stub := (&openCodeCLIStub{
		sessions: sessionList(openCodeSessionRow{"ses_big", created}),
		oversize: map[string]bool{"ses_big": true},
	}).install(t)

	reconcileOpenCodeUsageOnce(context.Background(), now)
	ledger := readOpenCodeUsageLedger()
	if len(ledger.Skipped) != 1 || ledger.Skipped[0].Session != openCodeUsageHash("ses_big", "") {
		t.Fatalf("skipped = %+v, want the hashed session", ledger.Skipped)
	}
	if ledger.Skipped[0].UpdatedMs != created {
		t.Fatalf("skipped updated = %d, want %d", ledger.Skipped[0].UpdatedMs, created)
	}
	if !ledger.Days[openCodeDayKey(now)].Partial {
		t.Fatal("a session nothing counted must make the day a lower bound")
	}
	// The cursor still advanced, so a later pass cannot stall on it.
	if ledger.ReconcileCursorMs != created {
		t.Fatalf("cursor = %d, want it past the over-cap session", ledger.ReconcileCursorMs)
	}

	// A LATER write raises the session's `updated`, which is still older than
	// the cursor. It is selected again anyway.
	stub.sessions = sessionList(openCodeSessionRow{"ses_big", created + 1})
	stub.oversize = nil
	stub.exports = map[string]string{
		"ses_big": exportWith("ses_big", "msg_big", created, 11, 4, "0"),
	}
	if result := reconcileOpenCodeUsageOnce(context.Background(), now); result.Outcome != openCodeReconcileOK {
		t.Fatalf("retry outcome = %q, want ok", result.Outcome)
	}
	ledger = readOpenCodeUsageLedger()
	if len(ledger.Skipped) != 0 {
		t.Fatalf("skipped = %+v, want the entry removed once it exported", ledger.Skipped)
	}
	if tokens, _, _ := todayTotals(t, now); tokens != 15 {
		t.Fatalf("tokens = %d, want the retried session's 15", tokens)
	}
}

func TestOpenCodeReconcile_RetriesTakeAtMostOneSlotWhileTheCursorHasWorkLeft(t *testing.T) {
	// Otherwise three remembered sessions could take the whole pass forever and
	// the cursor would never move.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	created := now.UnixMilli()

	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		for _, id := range []string{"ses_r1", "ses_r2", "ses_r3"} {
			openCodeRememberSkippedSession(l, id, created)
		}
		l.ReconcileCursorMs = created + 100
		return openCodeLedgerEdit{Changed: true}
	})
	stub := (&openCodeCLIStub{
		sessions: sessionList(
			openCodeSessionRow{"ses_r1", created + 1},
			openCodeSessionRow{"ses_r2", created + 1},
			openCodeSessionRow{"ses_r3", created + 1},
			openCodeSessionRow{"ses_new1", created + 200},
			openCodeSessionRow{"ses_new2", created + 300},
		),
	}).install(t)

	result := reconcileOpenCodeUsageOnce(context.Background(), now)
	if result.Outcome != openCodeReconcileMore {
		t.Fatalf("outcome = %q, want more", result.Outcome)
	}
	calls := stub.recorded()
	retries := 0
	for _, call := range calls {
		if strings.HasPrefix(call, "export ses_r") {
			retries++
		}
	}
	if retries != 1 {
		t.Fatalf("calls = %v, want exactly one retry slot", calls)
	}
	if cursor := readOpenCodeUsageLedger().ReconcileCursorMs; cursor != created+300 {
		t.Fatalf("cursor = %d, want it past both new sessions (%d)", cursor, created+300)
	}
}

func TestOpenCodeReconcile_AFullSkippedSetEvictsItsOldestEntry(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	created := now.UnixMilli()

	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		for i := 0; i < openCodeLedgerMaxSkipped; i++ {
			openCodeRememberSkippedSession(l, fmt.Sprintf("ses_old%d", i), created+int64(i))
		}
		return openCodeLedgerEdit{Changed: true}
	})
	(&openCodeCLIStub{
		sessions: sessionList(openCodeSessionRow{"ses_head", created + 9000}),
		oversize: map[string]bool{"ses_head": true},
	}).install(t)

	reconcileOpenCodeUsageOnce(context.Background(), now)
	ledger := readOpenCodeUsageLedger()
	if len(ledger.Skipped) != openCodeLedgerMaxSkipped {
		t.Fatalf("skipped = %d entries, want the cap %d", len(ledger.Skipped), openCodeLedgerMaxSkipped)
	}
	head := openCodeUsageHash("ses_head", "")
	oldest := openCodeUsageHash("ses_old0", "")
	held := map[string]bool{}
	for _, entry := range ledger.Skipped {
		held[entry.Session] = true
	}
	if !held[head] {
		t.Fatal("the new over-cap session was not recorded")
	}
	if held[oldest] {
		t.Fatal("the oldest in-retention entry should have been evicted to make room")
	}
	// The evicted session is never retried, which is why the day stays a lower
	// bound, and the cursor still moved.
	if !ledger.Days[openCodeDayKey(now)].Partial {
		t.Fatal("the day must stay partial")
	}
	if ledger.ReconcileCursorMs != created+9000 {
		t.Fatalf("cursor = %d, want it past the over-cap session", ledger.ReconcileCursorMs)
	}
}

/* ─────────────────────────── outcome mapping ───────────────────────────── */

func TestOpenCodeReconcile_OutcomeMapping(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	created := now.UnixMilli()
	cases := []struct {
		name    string
		stub    *openCodeCLIStub
		want    string
		success bool
	}{
		{
			name: "unrecognised session-list output is unsupported",
			stub: &openCodeCLIStub{},
			want: openCodeReconcileUnsupported,
		},
		{
			name: "a non-zero exit is unsupported",
			stub: &openCodeCLIStub{exitError: true},
			want: openCodeReconcileUnsupported,
		},
		{
			name: "a launch failure is launch_error",
			stub: &openCodeCLIStub{launchError: true},
			want: openCodeReconcileLaunchError,
		},
		{
			name: "a hung export before any commit is a timeout",
			stub: &openCodeCLIStub{
				sessions: sessionList(openCodeSessionRow{"ses_hang", created}),
				hang:     map[string]bool{"ses_hang": true},
			},
			want: openCodeReconcileTimeout,
		},
		{
			name: "an empty but recognised answer is no_change",
			stub: &openCodeCLIStub{sessions: "[]"},
			want: openCodeReconcileNoChange, success: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			openCodeUsageFixture(t, now)
			// A hung child must not spend the shipped 20s budget in CI.
			prev := openCodeReconcileBudgetForTests(200 * time.Millisecond)
			t.Cleanup(prev)
			tc.stub.install(t)

			result := reconcileOpenCodeUsageOnce(context.Background(), now)
			if result.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q", result.Outcome, tc.want)
			}
			ledger := readOpenCodeUsageLedger()
			if ledger.LastPassOutcome != tc.want {
				t.Fatalf("persisted outcome = %q, want %q", ledger.LastPassOutcome, tc.want)
			}
			// lastSuccessfulReconcileAtMs is the zero row's only licence, so it
			// is set ONLY by a pass that reached the end of the list.
			if gotSuccess := ledger.LastSuccessfulReconcileAtMs != 0; gotSuccess != tc.success {
				t.Fatalf("success stamp set = %v, want %v", gotSuccess, tc.success)
			}
			// Every pass stamps its start, which is what the gather's nudge
			// compares a session directory's mtime against.
			if ledger.LastPassStartedAtMs != now.UnixMilli() {
				t.Fatalf("lastPassStartedAtMs = %d, want %d", ledger.LastPassStartedAtMs, now.UnixMilli())
			}
		})
	}
}

func TestOpenCodeReconcile_PinsSelfUpdateOffOnEverySpawn(t *testing.T) {
	// A usage read must never BE an upgrade, and a terminal-title escape on
	// stdout would sit in front of the JSON the pass decodes.
	launch := openCodeUsageLaunch(os.Args[0], []string{"session", "list", "--format", "json"})
	if !launch.Maintenance {
		t.Fatal("a reconcile command must run as a maintenance child")
	}
	cmd, err := newOpenCodeCmd(context.Background(), launch)
	if err != nil {
		t.Fatalf("newOpenCodeCmd: %v", err)
	}
	joined := strings.Join(cmd.Env, "\n")
	for _, pin := range openCodeMaintenanceEnvPins {
		if !strings.Contains(joined, pin[0]+"="+pin[1]) {
			t.Fatalf("env is missing the maintenance pin %s", pin[0])
		}
	}
	// And the prompt file / stdin are never set: a reconcile asks a question,
	// it never runs a turn.
	if launch.PromptFile != "" || launch.Stdin != nil {
		t.Fatalf("a reconcile command must carry no prompt: %+v", launch)
	}
}

func TestOpenCodeJSONBody_SkipsABannerBeforeTheJSON(t *testing.T) {
	rows, ok := parseOpenCodeSessionList([]byte("opencode 1.2.3\nupdate available\n[{\"id\":\"a\",\"updated\":5}]"))
	if !ok || len(rows) != 1 || rows[0].id != "a" || rows[0].updatedMs != 5000 {
		t.Fatalf("rows = %+v ok = %v", rows, ok)
	}
	if _, ok := parseOpenCodeSessionList([]byte("  ID   TITLE\n  a    b\n")); ok {
		t.Fatal("a human table must read as unsupported")
	}
}

/* ───────────────────── a usable answer outranks the exit ───────────────── */

func TestOpenCodeReconcile_AUsableAnswerOutranksANonZeroExit(t *testing.T) {
	// `unsupported` RETIRES the debt, so reading a transient non-zero exit (an
	// update notice, a warning) as "this build cannot answer" would drop the
	// reading permanently. The ANSWER decides.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	created := now.UnixMilli()

	openCodeUsageBinary = func() string { return "opencode-stub" }
	openCodeRunCommand = func(_ context.Context, _ string, args []string, _ int) ([]byte, bool, error) {
		if args[0] == "session" {
			return []byte(sessionList(openCodeSessionRow{"ses_a", created})), false, &exec.ExitError{}
		}
		return []byte(exportWith("ses_a", "msg_a", created, 8, 2, "0")), false, &exec.ExitError{}
	}

	if result := reconcileOpenCodeUsageOnce(context.Background(), now); result.Outcome != openCodeReconcileOK {
		t.Fatalf("outcome = %q, want ok — the output was readable", result.Outcome)
	}
	if tokens, _, _ := todayTotals(t, now); tokens != 10 {
		t.Fatalf("tokens = %d, want the readable answer's 10", tokens)
	}
}

func TestOpenCodeReconcile_AnEmptyExportObjectIsNotUnsupported(t *testing.T) {
	// `{}` is a session with no assistant turn — a real answer. Reading it as
	// unsupported would retire the debt and stand the whole feature down.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	(&openCodeCLIStub{
		sessions: sessionList(openCodeSessionRow{"ses_empty", now.UnixMilli()}),
		exports:  map[string]string{"ses_empty": "{}"},
	}).install(t)

	if result := reconcileOpenCodeUsageOnce(context.Background(), now); result.Outcome != openCodeReconcileOK {
		t.Fatalf("outcome = %q, want ok", result.Outcome)
	}
	if readOpenCodeUsageLedger().ReconcileCursorMs != now.UnixMilli() {
		t.Fatal("the cursor must still advance past an empty session")
	}
}

func TestOpenCodeReconcile_AnUnreadableAnswerKeepsItsFailureClass(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"a non-zero exit with no readable output is unsupported", &exec.ExitError{}, openCodeReconcileUnsupported},
		{"a clean exit printing nothing we recognise is unsupported", nil, openCodeReconcileUnsupported},
		{"a missing binary is launch_error", errors.New("exec: not found"), openCodeReconcileLaunchError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			openCodeUsageFixture(t, now)
			openCodeUsageBinary = func() string { return "opencode-stub" }
			openCodeRunCommand = func(context.Context, string, []string, int) ([]byte, bool, error) {
				return []byte("  ID  TITLE\n"), false, tc.err
			}
			if result := reconcileOpenCodeUsageOnce(context.Background(), now); result.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q", result.Outcome, tc.want)
			}
		})
	}
}

func TestOpenCodeReconcile_ATruncatedSessionListMakesTheDayALowerBound(t *testing.T) {
	// Past openCodeSessionListMaxSessions only the most recently active are
	// considered, so a changed session the cut dropped is never exported. The
	// totals must not read as complete.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	created := now.UnixMilli()
	rows := make([]openCodeSessionRow, 0, openCodeSessionListMaxSessions+5)
	for i := 0; i < openCodeSessionListMaxSessions+5; i++ {
		rows = append(rows, openCodeSessionRow{fmt.Sprintf("ses_%d", i), created + int64(i)})
	}
	(&openCodeCLIStub{sessions: sessionList(rows...)}).install(t)

	reconcileOpenCodeUsageOnce(context.Background(), now)
	if day := readOpenCodeUsageLedger().Days[openCodeDayKey(now)]; day == nil || !day.Partial {
		t.Fatalf("day = %+v, want the lower-bound notice", day)
	}
	// A list inside the bound says nothing of the kind.
	openCodeUsageFixture(t, now)
	(&openCodeCLIStub{sessions: sessionList(rows[:3]...)}).install(t)
	reconcileOpenCodeUsageOnce(context.Background(), now)
	if day := readOpenCodeUsageLedger().Days[openCodeDayKey(now)]; day != nil && day.Partial {
		t.Fatal("a list inside the bound must not mark the day partial")
	}
}
