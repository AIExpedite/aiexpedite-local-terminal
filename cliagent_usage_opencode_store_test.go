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
	"strconv"
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
	// sessionsFilled answers `session list` as an overflow of its stdout cap:
	// `sessions` is the cut-off prefix that fit.
	sessionsFilled bool

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
			return []byte(s.sessions), s.sessionsFilled, nil
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
	// Pinned to the fixture's own `updated`: a session older than the ledger's
	// retention window is deliberately never exported.
	now := time.UnixMilli(1790400009000)
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
	handle := armOpenCodeUsageRun("native chat")
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

	handle := armOpenCodeUsageRun("native chat")
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
	if len(ledger.Skipped) != 1 || ledger.Skipped[0].SessionHash != openCodeUsageHash("ses_big", "") {
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
		held[entry.SessionHash] = true
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

func TestOpenCodeReconcile_ACleanSilentSessionListIsAnEmptyStore(t *testing.T) {
	// OpenCode's `session list` returns before printing when there are no
	// sessions. Reading that as `unsupported` would retire the debt and never
	// record the successful pass the zero row waits for.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	for _, tc := range []struct {
		name    string
		err     error
		want    string
		success bool
	}{
		{"a clean exit printing nothing is an empty list", nil, openCodeReconcileNoChange, true},
		{"a non-zero exit printing nothing stays unsupported", &exec.ExitError{}, openCodeReconcileUnsupported, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			openCodeUsageFixture(t, now)
			openCodeUsageBinary = func() string { return "opencode-stub" }
			openCodeRunCommand = func(_ context.Context, _ string, args []string, _ int) ([]byte, bool, error) {
				if args[0] != "session" {
					t.Fatalf("an empty store must export nothing, ran %v", args)
				}
				return []byte(" \n"), false, tc.err
			}
			if result := reconcileOpenCodeUsageOnce(context.Background(), now); result.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q", result.Outcome, tc.want)
			}
			if gotSuccess := readOpenCodeUsageLedger().LastSuccessfulReconcileAtMs != 0; gotSuccess != tc.success {
				t.Fatalf("successful reconcile recorded = %v, want %v", gotSuccess, tc.success)
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

func TestOpenCodeReconcile_ARefusedUndatableMarkerFailsThePass(t *testing.T) {
	// A listed session whose `updated` cannot be read is never exported, so the
	// partial marker is the ONLY record that the day is a lower bound. Ending
	// no_change would pay the debt and present the day as complete, with no
	// automatic retry — exactly as for the truncated list above.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(openCodeUsageLedgerEnv, filepath.Join(blocker, "opencode_usage.json"))
	stub := (&openCodeCLIStub{sessions: `[{"id":"ses_undatable"}]`}).install(t)

	result := reconcileOpenCodeUsageOnce(context.Background(), now)
	if openCodeReconcileSucceeded(result.Outcome) {
		t.Fatalf("outcome = %q: a refused lower-bound marker must not succeed", result.Outcome)
	}
	if result.Outcome != openCodeReconcileWriteError {
		t.Fatalf("outcome = %q, want write_error", result.Outcome)
	}
	if got := stub.exportCount(); got != 0 {
		t.Fatalf("exports = %d, want none for an undatable session", got)
	}
}

/* ─────────────────────── the session list's own bound ──────────────────── */

func TestOpenCodeReconcile_AsksForMoreRowsThanItConsiders(t *testing.T) {
	// OpenCode's list service defaults an unspecified limit to 100 rows, so the
	// unflagged command hides everything older than the newest 100 — and those
	// rows fall behind the cursor once the returned ones commit, with nothing
	// marking the loss. One more than the local cap is asked for, so a longer
	// list still reads as truncated.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	stub := (&openCodeCLIStub{sessions: sessionList()}).install(t)

	reconcileOpenCodeUsageOnce(context.Background(), now)
	want := "session list --format json --max-count " + strconv.Itoa(openCodeSessionListMaxSessions+1)
	if got := stub.recorded(); len(got) != 1 || got[0] != want {
		t.Fatalf("calls = %v, want %q", got, want)
	}
}

func TestOpenCodeReconcile_FallsBackToTheUnflaggedListWhenMaxCountIsRejected(t *testing.T) {
	// A build that predates `--max-count` rejects the whole command, and
	// `unsupported` RETIRES the debt — so the older spelling is worth one retry
	// before standing the feature down.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	created := now.UnixMilli()
	stub := (&openCodeCLIStub{
		sessions: sessionList(openCodeSessionRow{"ses_a", created + 1000}),
		exports: map[string]string{
			"ses_a": exportWith("ses_a", "msg_a", created, 7, 1, "0"),
		},
	}).install(t)
	modern := openCodeRunCommand
	openCodeRunCommand = func(ctx context.Context, path string, args []string, limit int) ([]byte, bool, error) {
		for _, arg := range args {
			if arg == "--max-count" {
				stub.record(args)
				return nil, false, &exec.ExitError{}
			}
		}
		return modern(ctx, path, args, limit)
	}

	result := reconcileOpenCodeUsageOnce(context.Background(), now)
	if result.Outcome != openCodeReconcileOK {
		t.Fatalf("outcome = %q, want ok through the unflagged list", result.Outcome)
	}
	if got := stub.recorded(); len(got) != 3 || !strings.Contains(got[0], "--max-count") ||
		got[1] != "session list --format json" || got[2] != "export ses_a" {
		t.Fatalf("calls = %v, want the flagged list, the unflagged retry, then the export", got)
	}
	if tokens, _, _ := todayTotals(t, now); tokens != 8 {
		t.Fatalf("tokens = %d, want the fallback list's export counted", tokens)
	}
}

/* ──────────────────────── delegated child sessions ─────────────────────── */

// exportWithChildren renders an export carrying one assistant message plus a
// `task` tool part per delegated child, the way OpenCode records a subagent.
func exportWithChildren(sessionID, messageID string, createdMs, in, out int64, children ...string) string {
	parts := make([]string, 0, len(children))
	for _, child := range children {
		parts = append(parts,
			fmt.Sprintf(`{"type":"tool","tool":"task","state":{"status":"completed",`+
				`"metadata":{"parentSessionId":%q,"sessionId":%q}}}`, sessionID, child))
	}
	return fmt.Sprintf(`{"messages":[{"info":{"id":%q,"sessionID":%q,"role":"assistant",`+
		`"time":{"created":%d},"cost":0,"tokens":{"input":%d,"output":%d,"reasoning":0,`+
		`"cache":{"read":0,"write":0}}},"parts":[%s]}]}`,
		messageID, sessionID, createdMs, in, out, strings.Join(parts, ","))
}

func TestOpenCodeReconcile_CountsDelegatedChildSessions(t *testing.T) {
	// A subagent's tokens live in a CHILD session, which `session list` never
	// returns (it asks for roots only) and the run stream drops (it keeps only
	// the root's parts). The parent export's task part is the only place a
	// reconcile can learn the id, so it is followed — one level, then the next.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	created := now.UnixMilli()
	stub := (&openCodeCLIStub{
		sessions: sessionList(openCodeSessionRow{"ses_root", created + 1000}),
		exports: map[string]string{
			"ses_root":     exportWithChildren("ses_root", "msg_root", created, 10, 1, "ses_kid"),
			"ses_kid":      exportWithChildren("ses_kid", "msg_kid", created, 20, 2, "ses_grandkid"),
			"ses_grandkid": exportWith("ses_grandkid", "msg_grandkid", created, 30, 3, "0"),
		},
	}).install(t)

	result := reconcileOpenCodeUsageOnce(context.Background(), now)
	if result.Outcome != openCodeReconcileOK {
		t.Fatalf("outcome = %q, want ok", result.Outcome)
	}
	if got := stub.recorded(); len(got) != 4 || got[1] != "export ses_root" ||
		got[2] != "export ses_kid" || got[3] != "export ses_grandkid" {
		t.Fatalf("calls = %v, want the root then its descendants", got)
	}
	if tokens, _, rows := todayTotals(t, now); tokens != 66 || rows != 3 {
		t.Fatalf("tokens=%d rows=%d, want 66/3 — the root and both subagents", tokens, rows)
	}
	if day := readOpenCodeUsageLedger().Days[openCodeDayKey(now)]; day == nil || day.Partial {
		t.Fatalf("day = %+v, want a complete day: every child was read", day)
	}
	// The descent does not consume the pass's export slots, which are the
	// LISTED sessions' budget.
	if result.Exported != 1 {
		t.Fatalf("exported = %d, want the one listed session", result.Exported)
	}
}

func TestOpenCodeReconcile_BoundsTheDescentAndSaysTheDayIsALowerBound(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	created := now.UnixMilli()

	t.Run("too many children", func(t *testing.T) {
		openCodeUsageFixture(t, now)
		children := make([]string, 0, openCodeExportMaxChildren+2)
		exports := map[string]string{}
		for i := 0; i < openCodeExportMaxChildren+2; i++ {
			id := fmt.Sprintf("ses_kid_%d", i)
			children = append(children, id)
			exports[id] = exportWith(id, "msg_"+id, created, 1, 0, "0")
		}
		exports["ses_root"] = exportWithChildren("ses_root", "msg_root", created, 5, 0, children...)
		stub := (&openCodeCLIStub{
			sessions: sessionList(openCodeSessionRow{"ses_root", created + 1000}),
			exports:  exports,
		}).install(t)

		reconcileOpenCodeUsageOnce(context.Background(), now)
		if got := stub.exportCount(); got != 1+openCodeExportMaxChildren {
			t.Fatalf("exports = %d, want the root plus %d children", got, openCodeExportMaxChildren)
		}
		if day := readOpenCodeUsageLedger().Days[openCodeDayKey(now)]; day == nil || !day.Partial {
			t.Fatalf("day = %+v, want the lower-bound notice for the children left unread", day)
		}
	})

	t.Run("too deep", func(t *testing.T) {
		openCodeUsageFixture(t, now)
		exports := map[string]string{}
		for level := 0; level <= openCodeExportMaxChildDepth; level++ {
			id := fmt.Sprintf("ses_%d", level)
			exports[id] = exportWithChildren(id, "msg_"+id, created, 1, 0, fmt.Sprintf("ses_%d", level+1))
		}
		stub := (&openCodeCLIStub{
			sessions: sessionList(openCodeSessionRow{"ses_0", created + 1000}),
			exports:  exports,
		}).install(t)

		reconcileOpenCodeUsageOnce(context.Background(), now)
		if got := stub.exportCount(); got != 1+openCodeExportMaxChildDepth {
			t.Fatalf("exports = %d, want the root plus %d levels", got, openCodeExportMaxChildDepth)
		}
		if day := readOpenCodeUsageLedger().Days[openCodeDayKey(now)]; day == nil || !day.Partial {
			t.Fatalf("day = %+v, want the lower-bound notice past the depth bound", day)
		}
	})

	t.Run("an unreadable child does not retire the debt", func(t *testing.T) {
		// `unsupported` stands the whole feature down, so a child the CLI
		// cannot export — the parent read fine — is a lower bound, not a
		// verdict on the install.
		openCodeUsageFixture(t, now)
		stub := (&openCodeCLIStub{
			sessions: sessionList(openCodeSessionRow{"ses_root", created + 1000}),
			exports: map[string]string{
				"ses_root": exportWithChildren("ses_root", "msg_root", created, 9, 1, "ses_gone"),
				"ses_gone": "not json at all",
			},
		}).install(t)

		result := reconcileOpenCodeUsageOnce(context.Background(), now)
		if result.Outcome != openCodeReconcileOK {
			t.Fatalf("outcome = %q, want ok: the listed session was read", result.Outcome)
		}
		if got := stub.exportCount(); got != 2 {
			t.Fatalf("exports = %d, want the root and the one attempt at the child", got)
		}
		if tokens, _, _ := todayTotals(t, now); tokens != 10 {
			t.Fatalf("tokens = %d, want the root's own figures kept", tokens)
		}
		if day := readOpenCodeUsageLedger().Days[openCodeDayKey(now)]; day == nil || !day.Partial {
			t.Fatalf("day = %+v, want the lower-bound notice for the unread child", day)
		}
	})
}

func TestOpenCodeReconcile_ARefusedPartialMarkerFailsThePass(t *testing.T) {
	// A truncated list whose lower-bound marker never reached disk must not end
	// as a success: no_change would pay the debt for the sessions the cut
	// dropped, with no notice on the card.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(openCodeUsageLedgerEnv, filepath.Join(blocker, "opencode_usage.json"))
	// Cut before any row arrived intact, so the plan has nothing to export.
	stub := (&openCodeCLIStub{sessions: `[{"id":"ses_`, sessionsFilled: true}).install(t)

	result := reconcileOpenCodeUsageOnce(context.Background(), now)
	if result.Outcome != openCodeReconcileWriteError {
		t.Fatalf("outcome = %q, want write_error", result.Outcome)
	}
	if got := stub.exportCount(); got != 0 {
		t.Fatalf("exports = %d, want none", got)
	}
}

func TestOpenCodeReconcile_ASessionListOverItsStdoutCapIsABacklogNotUnsupported(t *testing.T) {
	// A list past openCodeSessionListMaxStdout is cut mid-document, so it never
	// parses whole. Reading it as `unsupported` would retire the debt with no
	// notice on exactly the installs with the most history; instead the rows
	// that arrived intact are reconciled and the day is a lower bound.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	created := now.UnixMilli()
	whole := sessionList(
		openCodeSessionRow{"ses_a", created + 1000},
		openCodeSessionRow{"ses_b", created + 2000},
		openCodeSessionRow{"ses_c", created + 3000},
	)
	cut := whole[:strings.Index(whole, `"ses_c"`)+3]
	for _, spelling := range []struct{ name, list string }{
		{"bare array", cut},
		{"wrapped", `{"total":3,"sessions":` + cut},
	} {
		t.Run(spelling.name, func(t *testing.T) {
			openCodeUsageFixture(t, now)
			stub := (&openCodeCLIStub{
				sessions:       spelling.list,
				sessionsFilled: true,
				exports: map[string]string{
					"ses_a": exportWith("ses_a", "msg_a", created, 10, 1, "0"),
					"ses_b": exportWith("ses_b", "msg_b", created, 20, 2, "0"),
				},
			}).install(t)

			result := reconcileOpenCodeUsageOnce(context.Background(), now)
			if result.Outcome != openCodeReconcileOK {
				t.Fatalf("outcome = %q, want ok over the intact rows", result.Outcome)
			}
			if got := stub.exportCount(); got != 2 {
				t.Fatalf("exports = %d, want the two intact rows", got)
			}
			if tokens, _, _ := todayTotals(t, now); tokens != 33 {
				t.Fatalf("tokens = %d, want 33", tokens)
			}
			if day := readOpenCodeUsageLedger().Days[openCodeDayKey(now)]; day == nil || !day.Partial {
				t.Fatalf("day = %+v, want the lower-bound notice", day)
			}
		})
	}
}

func TestOpenCodeSessionListSalvage_KeepsOnlyIntactRows(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"nothing intact", `[{"id":"ses_a","time":{"upd`, 0},
		{"one intact", `[{"id":"ses_a","time":{"updated":1}},{"id":"ses_b"`, 1},
		{"banner then wrapped", "notice\n" + `{"sessions":[{"id":"ses_a","updated":1},{"id":`, 1},
		{"no sessions key", `{"other":[1,2,3],"more":"x`, 0},
		{"not json", `table output`, 0},
	} {
		if got := len(salvageOpenCodeSessionList([]byte(tc.body))); got != tc.want {
			t.Fatalf("%s: salvaged %d rows, want %d", tc.name, got, tc.want)
		}
	}
}

/* ───────────────── candidates that are not worth exporting ─────────────── */

func TestOpenCodeReconcile_NeverExportsASessionOlderThanTheRetentionWindow(t *testing.T) {
	// A session's `updated` is at least as new as every message in it, so every
	// figure such an export could carry would be dropped by the day check on
	// the way in — and the export is the expensive part. A first install on a
	// machine with months of history would otherwise export the whole archive
	// to learn nothing.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	oldest := openCodeRetentionFloor(now).Add(-30 * 24 * time.Hour).UnixMilli()
	newestOld := openCodeRetentionFloor(now).Add(-time.Minute).UnixMilli()
	stub := (&openCodeCLIStub{
		sessions: sessionList(
			openCodeSessionRow{"ses_ancient", oldest},
			openCodeSessionRow{"ses_last_month", newestOld},
			openCodeSessionRow{"ses_today", now.UnixMilli()},
		),
		exports: map[string]string{
			"ses_today": exportWith("ses_today", "msg_today", now.UnixMilli(), 6, 1, "0"),
		},
	}).install(t)

	if result := reconcileOpenCodeUsageOnce(context.Background(), now); result.Outcome != openCodeReconcileOK {
		t.Fatalf("outcome = %q, want ok", result.Outcome)
	}
	calls := stub.recorded()
	if len(calls) != 2 || calls[1] != "export ses_today" {
		t.Fatalf("calls = %v, want one list and only today's export", calls)
	}
	// The cursor is stepped past the archive in one write, so later passes do
	// not reconsider it either.
	if cursor := readOpenCodeUsageLedger().ReconcileCursorMs; cursor < newestOld {
		t.Fatalf("cursor = %d, want it past the newest out-of-retention session (%d)", cursor, newestOld)
	}
	if tokens, _, rows := todayTotals(t, now); tokens != 7 || rows != 1 {
		t.Fatalf("tokens=%d rows=%d, want only today's 7/1", tokens, rows)
	}
	// Nothing counted was dropped, so the day is NOT a lower bound.
	if day := readOpenCodeUsageLedger().Days[openCodeDayKey(now)]; day.Partial {
		t.Fatal("skipping an out-of-retention session must not mark the day partial")
	}
}

func TestOpenCodeReconcile_SkipsASessionWithNoReadableTimestamp(t *testing.T) {
	// There is no way to tell whether it changed, so exporting it on every pass
	// would repeat forever. It is skipped and the day says so.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	stub := (&openCodeCLIStub{
		sessions: `[{"id":"ses_undated"},{"id":"ses_today","time":{"updated":` +
			strconv.FormatInt(now.UnixMilli(), 10) + `}}]`,
		exports: map[string]string{
			"ses_today": exportWith("ses_today", "msg_today", now.UnixMilli(), 4, 1, "0"),
		},
	}).install(t)

	reconcileOpenCodeUsageOnce(context.Background(), now)
	for _, call := range stub.recorded() {
		if call == "export ses_undated" {
			t.Fatal("a session with no readable timestamp must not be exported")
		}
	}
	if day := readOpenCodeUsageLedger().Days[openCodeDayKey(now)]; day == nil || !day.Partial {
		t.Fatalf("day = %+v, want the lower-bound notice", day)
	}

	// And a second pass does not export it either — the repeat is what the skip
	// exists to prevent.
	before := stub.exportCount()
	reconcileOpenCodeUsageOnce(context.Background(), now)
	if stub.exportCount() != before {
		t.Fatalf("a second pass ran %d more exports, want none", stub.exportCount()-before)
	}
}

func TestOpenCodeReconcile_ReportsAFailedExportAsStillRemaining(t *testing.T) {
	// `remaining` is the backlog the next pass has to do, and the pass log's
	// only measure of it. An export that FAILED did not commit and its session
	// is still a candidate, so counting it as done under-reported the backlog.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	created := now.UnixMilli()
	rows := []openCodeSessionRow{
		{"ses_a", created + 1}, {"ses_b", created + 2}, {"ses_c", created + 3}, {"ses_d", created + 4},
	}
	calls := 0
	openCodeUsageBinary = func() string { return "opencode-stub" }
	openCodeRunCommand = func(_ context.Context, _ string, args []string, _ int) ([]byte, bool, error) {
		if args[0] == "session" {
			return []byte(sessionList(rows...)), false, nil
		}
		calls++
		if calls == 1 {
			return []byte(exportWith("ses_a", "msg_a", created, 5, 0, "0")), false, nil
		}
		// The second export cannot be launched at all.
		return nil, false, errors.New("exec: not found")
	}

	result := reconcileOpenCodeUsageOnce(context.Background(), now)
	if result.Outcome != openCodeReconcileLaunchError {
		t.Fatalf("outcome = %q, want launch_error", result.Outcome)
	}
	// One of four settled: the failed one and the two never attempted are all
	// still the next pass's work.
	if result.Remaining != 3 {
		t.Fatalf("remaining = %d, want 3 (the failed export plus the two unattempted)", result.Remaining)
	}
	// And what DID land is kept, with the cursor only past the committed one.
	if tokens, _, _ := todayTotals(t, now); tokens != 5 {
		t.Fatalf("tokens = %d, want the committed export's 5", tokens)
	}
	if cursor := readOpenCodeUsageLedger().ReconcileCursorMs; cursor != created+1 {
		t.Fatalf("cursor = %d, want it only past the committed session (%d)", cursor, created+1)
	}
}

func TestOpenCodeReconcile_StopsSpawningChildrenDuringShutdown(t *testing.T) {
	// A pass that started just before teardown must not keep spawning
	// `opencode` through it; the schedule is on disk for the next process.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	created := now.UnixMilli()
	exports := map[string]string{}
	rows := make([]openCodeSessionRow, 0, 3)
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("ses_%d", i)
		rows = append(rows, openCodeSessionRow{id, created + int64(i+1)})
		exports[id] = exportWith(id, fmt.Sprintf("msg_%d", i), created, 4, 0, "0")
	}
	stub := (&openCodeCLIStub{sessions: sessionList(rows...), exports: exports}).install(t)

	// Shutdown begins after the first export.
	t.Cleanup(func() { shutdownInProgress.Store(false) })
	prev := openCodeRunCommand
	openCodeRunCommand = func(ctx context.Context, path string, args []string, limit int) ([]byte, bool, error) {
		out, filled, err := prev(ctx, path, args, limit)
		if args[0] == "export" {
			shutdownInProgress.Store(true)
		}
		return out, filled, err
	}

	reconcileOpenCodeUsageOnce(context.Background(), now)
	if got := stub.exportCount(); got != 1 {
		t.Fatalf("exports = %d, want the pass to stop after the first once shutdown began", got)
	}
	// The one that committed is kept, so the restart resumes rather than redoes.
	if tokens, _, _ := todayTotals(t, now); tokens != 4 {
		t.Fatalf("tokens = %d, want the committed export's 4", tokens)
	}
}

func TestOpenCodeReconcile_ARefusedCommitIsNotReportedAsOk(t *testing.T) {
	// `committed` decides the pass outcome, and `ok` is what licenses the zero
	// row and stamps lastSuccessfulReconcileAtMs. A refused write leaves the
	// cursor where it was, so the session is still a candidate: reporting ok
	// would claim a complete reading of a day nothing was written for.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(openCodeUsageLedgerEnv, filepath.Join(blocker, "opencode_usage.json"))

	(&openCodeCLIStub{
		sessions: sessionList(openCodeSessionRow{"ses_a", now.UnixMilli()}),
		exports: map[string]string{
			"ses_a": exportWith("ses_a", "msg_a", now.UnixMilli(), 9, 1, "0"),
		},
	}).install(t)

	result := reconcileOpenCodeUsageOnce(context.Background(), now)
	// Not no_change either: that is a success too, and it would PAY the run
	// debt for figures that never reached disk, losing the automatic retry.
	if openCodeReconcileSucceeded(result.Outcome) {
		t.Fatalf("outcome = %q: a pass whose every write was refused must not succeed", result.Outcome)
	}
	if result.Outcome != openCodeReconcileWriteError {
		t.Fatalf("outcome = %q, want write_error", result.Outcome)
	}
	// The refused session is still a candidate, not settled.
	if result.Remaining != 1 {
		t.Fatalf("remaining = %d, want the refused session still counted", result.Remaining)
	}
	// And nothing is on record to license a zero row.
	if metrics, _, _ := openCodeLedgerMetrics(now); len(metrics) != 0 {
		t.Fatalf("metrics = %+v, want none", metrics)
	}
}

func TestOpenCodeReconcile_SumsEveryExportedStepOfOneMessage(t *testing.T) {
	// OpenCode overwrites info.tokens on each step, so a tool-calling turn's
	// info carries only its LAST step (20+4). The step-finish parts keep each
	// step's own figures; reading info alone would drop the first step's 10+2.
	now := time.UnixMilli(1790400000600)
	openCodeUsageFixture(t, now)
	(&openCodeCLIStub{
		sessions: sessionList(openCodeSessionRow{"ses_steps", now.UnixMilli()}),
		exports:  map[string]string{"ses_steps": openCodeFixture(t, "export_two_steps.json")},
	}).install(t)

	if result := reconcileOpenCodeUsageOnce(context.Background(), now); result.Outcome != openCodeReconcileOK {
		t.Fatalf("outcome = %q, want ok", result.Outcome)
	}
	tokens, cost, rows := todayTotals(t, now)
	if rows != 1 || tokens != 36 || cost != 2000 {
		t.Fatalf("rows=%d tokens=%d cost=%d, want 1/36/2000 (both steps)", rows, tokens, cost)
	}
}

func TestOpenCodeReconcile_KeepsEqualTimestampSessionsBeyondTheCursor(t *testing.T) {
	// Four sessions share one `updated` stamp. The first pass exports three and
	// moves the cursor to that stamp; the fourth must still be a candidate,
	// not stepped over by a strict `>` against the cursor.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	stamp := now.UnixMilli()
	stub := (&openCodeCLIStub{
		sessions: sessionList(
			openCodeSessionRow{"ses_a", stamp},
			openCodeSessionRow{"ses_b", stamp},
			openCodeSessionRow{"ses_c", stamp},
			openCodeSessionRow{"ses_d", stamp},
		),
		exports: map[string]string{
			"ses_a": exportWith("ses_a", "msg_a", stamp, 1, 0, "0"),
			"ses_b": exportWith("ses_b", "msg_b", stamp, 2, 0, "0"),
			"ses_c": exportWith("ses_c", "msg_c", stamp, 3, 0, "0"),
			"ses_d": exportWith("ses_d", "msg_d", stamp, 4, 0, "0"),
		},
	}).install(t)

	if first := reconcileOpenCodeUsageOnce(context.Background(), now); first.Outcome != openCodeReconcileMore {
		t.Fatalf("first outcome = %q, want more with one tied session left", first.Outcome)
	}
	if second := reconcileOpenCodeUsageOnce(context.Background(), now); second.Outcome != openCodeReconcileOK {
		t.Fatalf("second outcome = %q, want ok", second.Outcome)
	}
	if got := stub.recorded(); len(got) != 6 || got[5] != "export ses_d" {
		t.Fatalf("calls = %v, want the fourth tied session exported, and only it", got)
	}
	if tokens, _, rows := todayTotals(t, now); tokens != 10 || rows != 4 {
		t.Fatalf("tokens=%d rows=%d, want all four tied sessions", tokens, rows)
	}

	// The whole group is now behind the cursor: nothing is exported again.
	before := stub.exportCount()
	if third := reconcileOpenCodeUsageOnce(context.Background(), now); third.Outcome != openCodeReconcileNoChange {
		t.Fatalf("third outcome = %q, want no_change", third.Outcome)
	}
	if stub.exportCount() != before {
		t.Fatal("a tied session already committed must not be exported again")
	}
}

func TestOpenCodeReconcile_ARetryNeverCarriesTheCursorPastUnexportedFreshRows(t *testing.T) {
	// A remembered over-cap session was rewritten and is now NEWER than three
	// pending fresh rows. Exporting the retry first would move the cursor to its
	// stamp, and the two fresh rows that did not fit would fall behind it and
	// never be planned again — a silent loss, despite `more`.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	created := now.UnixMilli()

	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		openCodeRememberSkippedSession(l, "ses_retry", created)
		return openCodeLedgerEdit{Changed: true}
	})
	stub := (&openCodeCLIStub{
		sessions: sessionList(
			openCodeSessionRow{"ses_retry", created + 900},
			openCodeSessionRow{"ses_f1", created + 100},
			openCodeSessionRow{"ses_f2", created + 200},
			openCodeSessionRow{"ses_f3", created + 300},
		),
		exports: map[string]string{
			"ses_retry": exportWith("ses_retry", "msg_retry", created, 1, 0, "0"),
			"ses_f1":    exportWith("ses_f1", "msg_f1", created, 2, 0, "0"),
			"ses_f2":    exportWith("ses_f2", "msg_f2", created, 4, 0, "0"),
			"ses_f3":    exportWith("ses_f3", "msg_f3", created, 8, 0, "0"),
		},
	}).install(t)

	if first := reconcileOpenCodeUsageOnce(context.Background(), now); first.Outcome != openCodeReconcileMore {
		t.Fatalf("first outcome = %q, want more", first.Outcome)
	}
	// Oldest-first across BOTH queues: the retry is the newest candidate, so it
	// takes no slot this pass and the cursor stops at the last fresh row read.
	if cursor := readOpenCodeUsageLedger().ReconcileCursorMs; cursor != created+300 {
		t.Fatalf("cursor = %d, want %d — the newest row this pass exported", cursor, created+300)
	}
	if second := reconcileOpenCodeUsageOnce(context.Background(), now); second.Outcome != openCodeReconcileOK {
		t.Fatalf("second outcome = %q, want ok", second.Outcome)
	}
	if tokens, _, rows := todayTotals(t, now); tokens != 15 || rows != 4 {
		t.Fatalf("tokens=%d rows=%d, want every listed session counted", tokens, rows)
	}
	if skipped := readOpenCodeUsageLedger().Skipped; len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want the retry's entry removed once it exported", skipped)
	}
	if got := stub.recorded(); len(got) != 6 {
		t.Fatalf("calls = %v, want four exports over two passes", got)
	}
}

func TestOpenCodeReconcile_ARefusedSuccessStampFailsThePass(t *testing.T) {
	// For an empty store the success stamp is the ONLY state that makes the
	// zero row publishable, so a success whose stamp never reached disk must
	// not pay the debt.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	(&openCodeCLIStub{sessions: sessionList()}).install(t)
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(openCodeUsageLedgerEnv, filepath.Join(blocker, "opencode_usage.json"))

	result := reconcileOpenCodeUsageOnce(context.Background(), now)
	if result.Outcome != openCodeReconcileWriteError {
		t.Fatalf("outcome = %q, want write_error for a refused success stamp", result.Outcome)
	}
	if openCodeReconcileSucceeded(result.Outcome) {
		t.Fatal("write_error must not count as a success")
	}
	if stamp := readOpenCodeUsageLedger().LastSuccessfulReconcileAtMs; stamp != 0 {
		t.Fatalf("success stamp = %d, want none on disk", stamp)
	}
}

func TestOpenCodeReconcile_ReReadsASessionItMayHaveCaughtMidTurn(t *testing.T) {
	// A pass that overlaps a direct/TUI turn exports the session after its
	// prompt-time `updated` is set but before the turn's later messages are
	// persisted. OpenCode does not necessarily advance `updated` for them, so
	// without the booked re-read the cursor (and a `skipped` retry, keyed the
	// same way) would reject the session forever and the rest of the turn would
	// never be counted.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	advance := openCodeUsageFixture(t, now)
	stamp := now.UnixMilli()
	stub := (&openCodeCLIStub{
		sessions: sessionList(openCodeSessionRow{"ses_live", stamp}),
		exports: map[string]string{
			"ses_live": exportWith("ses_live", "msg_live", stamp, 10, 1, "0"),
		},
	}).install(t)

	if first := reconcileOpenCodeUsageOnce(context.Background(), now); first.Outcome != openCodeReconcileOK {
		t.Fatalf("first outcome = %q, want ok", first.Outcome)
	}
	if tokens, _, _ := todayTotals(t, now); tokens != 11 {
		t.Fatalf("tokens = %d, want 11 from the part of the turn that was written", tokens)
	}
	// Before the re-read is due, nothing is exported again.
	before := stub.exportCount()
	if again := reconcileOpenCodeUsageOnce(context.Background(), now); again.Outcome != openCodeReconcileNoChange {
		t.Fatalf("outcome = %q, want no_change before the re-read is due", again.Outcome)
	}
	if stub.exportCount() != before {
		t.Fatal("a re-read must wait for openCodeRecheckDelay, not run on the very next pass")
	}

	// The turn finished writing: more messages, the SAME `updated`.
	stub.exports["ses_live"] = `{"messages":[` +
		strings.TrimPrefix(strings.TrimSuffix(exportWith("ses_live", "msg_live", stamp, 10, 1, "0"), "]}"), `{"messages":[`) +
		`,` + strings.TrimSuffix(strings.TrimPrefix(exportWith("ses_live", "msg_tail", stamp, 40, 4, "0"), `{"messages":[`), "]}") +
		`]}`
	later := now.Add(openCodeRecheckDelay + time.Second)
	advance(later)
	if due := reconcileOpenCodeUsageOnce(context.Background(), later); due.Outcome != openCodeReconcileOK {
		t.Fatalf("due outcome = %q, want ok", due.Outcome)
	}
	if got := stub.exportCount(); got != before+1 {
		t.Fatalf("exports = %d, want one re-read of the session", got)
	}
	if tokens, _, rows := todayTotals(t, later); tokens != 55 || rows != 2 {
		t.Fatalf("tokens=%d rows=%d, want 55/2 once the rest of the turn is counted", tokens, rows)
	}

	// A re-read that found nothing new does NOT prove the turn ended: a model or
	// tool step can spend longer than the delay without writing anything, and
	// the rest of the turn would then be omitted for good. The record is kept.
	quiet := later.Add(openCodeRecheckDelay + time.Second)
	advance(quiet)
	if settled := reconcileOpenCodeUsageOnce(context.Background(), quiet); settled.Outcome != openCodeReconcileOK {
		t.Fatalf("settled outcome = %q, want ok for the second re-read", settled.Outcome)
	}
	spent := stub.exportCount()
	ledger := readOpenCodeUsageLedger()
	if len(ledger.Rechecks) != 1 || ledger.Rechecks[0].DueAtMs <= quiet.UnixMilli() {
		t.Fatalf("rechecks = %+v, want one quiet re-read to book another", ledger.Rechecks)
	}
	again := quiet.Add(openCodeRecheckDelay + time.Second)
	advance(again)
	if next := reconcileOpenCodeUsageOnce(context.Background(), again); next.Outcome != openCodeReconcileOK {
		t.Fatalf("next outcome = %q, want ok for the third re-read", next.Outcome)
	}
	if stub.exportCount() != spent+1 {
		t.Fatalf("exports = %d, want the quiet session read once more", stub.exportCount())
	}

	// The chain is bounded by openCodeRecheckMaxSpan from the last raised
	// figure instead, after which a quiet session is never exported again.
	final := later.Add(openCodeRecheckMaxSpan + time.Minute)
	advance(final)
	if expiring := reconcileOpenCodeUsageOnce(context.Background(), final); expiring.Outcome != openCodeReconcileOK {
		t.Fatalf("expiring outcome = %q, want ok", expiring.Outcome)
	}
	if ledger := readOpenCodeUsageLedger(); len(ledger.Rechecks) != 0 {
		t.Fatalf("rechecks = %+v, want the chain to stand down once its span ran out", ledger.Rechecks)
	}
	spent = stub.exportCount()
	beyond := final.Add(openCodeRecheckDelay + time.Second)
	advance(beyond)
	if last := reconcileOpenCodeUsageOnce(context.Background(), beyond); last.Outcome != openCodeReconcileNoChange {
		t.Fatalf("last outcome = %q, want no_change", last.Outcome)
	}
	if stub.exportCount() != spent {
		t.Fatal("an expired re-read chain must not book another")
	}
}

func TestOpenCodeReconcile_BooksNoReReadForASessionAlreadyFinished(t *testing.T) {
	// A backlog session last written long before the pass cannot have been
	// mid-turn, so it costs no second export.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	advance := openCodeUsageFixture(t, now)
	stamp := now.Add(-(openCodeSessionMaybeActiveWindow + time.Minute)).UnixMilli()
	stub := (&openCodeCLIStub{
		sessions: sessionList(openCodeSessionRow{"ses_done", stamp}),
		exports: map[string]string{
			"ses_done": exportWith("ses_done", "msg_done", stamp, 10, 1, "0"),
		},
	}).install(t)

	if first := reconcileOpenCodeUsageOnce(context.Background(), now); first.Outcome != openCodeReconcileOK {
		t.Fatalf("first outcome = %q, want ok", first.Outcome)
	}
	if ledger := readOpenCodeUsageLedger(); len(ledger.Rechecks) != 0 {
		t.Fatalf("rechecks = %d, want none for a session that was already finished", len(ledger.Rechecks))
	}
	before := stub.exportCount()
	later := now.Add(openCodeRecheckDelay + time.Minute)
	advance(later)
	if again := reconcileOpenCodeUsageOnce(context.Background(), later); again.Outcome != openCodeReconcileNoChange {
		t.Fatalf("outcome = %q, want no_change", again.Outcome)
	}
	if stub.exportCount() != before {
		t.Fatal("a finished session must not be exported a second time")
	}
}

func TestOpenCodeReconcile_ForgetsAReReadNoSessionCanEverClear(t *testing.T) {
	// A re-read record is cleared by the export that commits it. A session the
	// list no longer names — deleted, or moved out of a layout we can read —
	// will never reach that commit, and a record that cannot be cleared would
	// book a wake-up for itself on every successful pass until the retention
	// prune dropped it two days later. The pass drops it instead.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	advance := openCodeUsageFixture(t, now)
	stamp := now.UnixMilli()
	stub := (&openCodeCLIStub{
		sessions: sessionList(openCodeSessionRow{"ses_live", stamp}),
		exports: map[string]string{
			"ses_live": exportWith("ses_live", "msg_live", stamp, 10, 1, "0"),
		},
	}).install(t)

	if first := reconcileOpenCodeUsageOnce(context.Background(), now); first.Outcome != openCodeReconcileOK {
		t.Fatalf("first outcome = %q, want ok", first.Outcome)
	}
	if ledger := readOpenCodeUsageLedger(); len(ledger.Rechecks) != 1 {
		t.Fatalf("rechecks = %d, want the one booked for a session that may still be running", len(ledger.Rechecks))
	}

	// OpenCode no longer lists it, and the re-read is due.
	stub.sessions = "[]"
	later := now.Add(openCodeRecheckDelay + time.Second)
	advance(later)
	if due := reconcileOpenCodeUsageOnce(context.Background(), later); due.Outcome != openCodeReconcileNoChange {
		t.Fatalf("due outcome = %q, want no_change", due.Outcome)
	}
	if ledger := readOpenCodeUsageLedger(); len(ledger.Rechecks) != 0 {
		t.Fatalf("rechecks = %+v, want the unclearable record dropped", ledger.Rechecks)
	}
}

func TestOpenCodeReconcile_ForgetsAReReadTheSkippedRetryNowOwns(t *testing.T) {
	// The same, for a session that has since gone over the stdout cap: the
	// `skipped` queue owns it from then on, keyed on `updated`, and the
	// over-cap commit path never reaches the re-read's commit.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	advance := openCodeUsageFixture(t, now)
	stamp := now.UnixMilli()
	stub := (&openCodeCLIStub{
		sessions: sessionList(openCodeSessionRow{"ses_live", stamp}),
		exports: map[string]string{
			"ses_live": exportWith("ses_live", "msg_live", stamp, 10, 1, "0"),
		},
	}).install(t)
	if first := reconcileOpenCodeUsageOnce(context.Background(), now); first.Outcome != openCodeReconcileOK {
		t.Fatalf("first outcome = %q, want ok", first.Outcome)
	}

	// It grew past the cap and was written again, so it is a `skipped` retry.
	stub.oversize = map[string]bool{"ses_live": true}
	grown := now.Add(time.Minute)
	advance(grown)
	stub.sessions = sessionList(openCodeSessionRow{"ses_live", grown.UnixMilli()})
	if over := reconcileOpenCodeUsageOnce(context.Background(), grown); !openCodeReconcileSucceeded(over.Outcome) {
		t.Fatalf("over-cap outcome = %q, want a success", over.Outcome)
	}
	if ledger := readOpenCodeUsageLedger(); len(ledger.Skipped) != 1 {
		t.Fatalf("skipped = %+v, want the over-cap session remembered", ledger.Skipped)
	}
	due := grown.Add(openCodeRecheckDelay + time.Second)
	advance(due)
	if pass := reconcileOpenCodeUsageOnce(context.Background(), due); !openCodeReconcileSucceeded(pass.Outcome) {
		t.Fatalf("outcome = %q, want a success", pass.Outcome)
	}
	if ledger := readOpenCodeUsageLedger(); len(ledger.Rechecks) != 0 {
		t.Fatalf("rechecks = %+v, want the record left to the skipped retry dropped", ledger.Rechecks)
	}
}
