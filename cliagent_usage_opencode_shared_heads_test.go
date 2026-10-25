package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_opencode_shared_heads_test.go — OpenCode 1.19 stores. They
   stage an EMPTY OpenCode 2 session_message table beside the V1 message / part
   tables every turn is still written to, both families sharing the session
   heads table. The reader elects V1 there, so a direct run, a smoke and a
   terminal-managed run all move the card, a debt is paid from message rather
   than read as store_empty, and a busy store defers rather than renewing
   coverage. Every store is built here; no value comes from a device.
   ------------------------------------------------------------------------ */

// openCodeSQLite119TestSchema is OpenCode 1.19.4's opencode.db: the table and
// column set of a real store, the empty staged session_message with its
// indexes included. Only the reader's tables and the ones holding credentials
// (kept to prove they stay unread) are listed.
const openCodeSQLite119TestSchema = `
CREATE TABLE account (id text PRIMARY KEY, email text NOT NULL, url text NOT NULL, access_token text NOT NULL, refresh_token text NOT NULL, token_expiry integer, time_created integer NOT NULL, time_updated integer NOT NULL);
CREATE TABLE control_account (email text NOT NULL, url text NOT NULL, access_token text NOT NULL, refresh_token text NOT NULL, token_expiry integer, active integer NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, CONSTRAINT control_account_pk PRIMARY KEY(email, url));
CREATE TABLE credential (id text PRIMARY KEY, integration_id text, label text NOT NULL, value text NOT NULL, connector_id text, method_id text, active integer, time_created integer NOT NULL, time_updated integer NOT NULL);
CREATE TABLE event_sequence (aggregate_id text PRIMARY KEY, seq integer NOT NULL, owner_id text);
CREATE TABLE event (id text PRIMARY KEY, aggregate_id text NOT NULL, seq integer NOT NULL, type text NOT NULL, data text NOT NULL);
CREATE TABLE project (id text PRIMARY KEY, worktree text NOT NULL, vcs text, name text, icon_url text, icon_url_override text, icon_color text, time_created integer NOT NULL, time_updated integer NOT NULL, time_initialized integer, sandboxes text NOT NULL, commands text);
CREATE TABLE session (id text PRIMARY KEY, project_id text NOT NULL, workspace_id text, parent_id text, slug text NOT NULL, directory text NOT NULL, path text, title text NOT NULL, version text NOT NULL, share_url text, summary_additions integer, summary_deletions integer, summary_files integer, summary_diffs text, metadata text, cost real DEFAULT 0 NOT NULL, tokens_input integer DEFAULT 0 NOT NULL, tokens_output integer DEFAULT 0 NOT NULL, tokens_reasoning integer DEFAULT 0 NOT NULL, tokens_cache_read integer DEFAULT 0 NOT NULL, tokens_cache_write integer DEFAULT 0 NOT NULL, revert text, permission text, agent text, model text, time_created integer NOT NULL, time_updated integer NOT NULL, time_compacting integer, time_archived integer);
CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
CREATE TABLE part (id text PRIMARY KEY, message_id text NOT NULL, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
CREATE TABLE session_message (id text PRIMARY KEY, session_id text NOT NULL, type text NOT NULL, seq integer NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
CREATE TABLE session_share (session_id text PRIMARY KEY, id text NOT NULL, secret text NOT NULL, url text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL);
CREATE INDEX message_session_time_created_id_idx ON message (session_id, time_created, id);
CREATE INDEX part_message_id_id_idx ON part (message_id, id);
CREATE INDEX part_session_idx ON part (session_id);
CREATE UNIQUE INDEX session_message_session_seq_idx ON session_message (session_id, seq);
CREATE INDEX session_message_session_time_created_id_idx ON session_message (session_id, time_created, id);
CREATE INDEX session_message_session_type_seq_idx ON session_message (session_id, type, seq);
CREATE INDEX session_message_time_created_idx ON session_message (time_created);`

// openCode119TestStore writes rows the way OpenCode 1.19 does: every turn to
// message / part, session_message left empty unless a case writes a V2 row.
type openCode119TestStore struct {
	t    *testing.T
	db   *sql.DB
	root string
	seq  map[string]int
}

func newOpenCode119TestStore(t *testing.T, root string) *openCode119TestStore {
	t.Helper()
	return &openCode119TestStore{t: t, db: openCodeSQLiteTestStore(t, root, openCodeSQLite119TestSchema), root: root, seq: map[string]int{}}
}

func (s *openCode119TestStore) exec(query string, args ...any) {
	s.t.Helper()
	if _, err := s.db.Exec(query, args...); err != nil {
		s.t.Fatal(err)
	}
}

// head inserts the session head, or moves its time_updated forward.
func (s *openCode119TestStore) head(session string, createdMs, updatedMs int64) {
	s.t.Helper()
	s.exec(`INSERT OR IGNORE INTO session (id, project_id, slug, directory, title, version, time_created, time_updated)
		VALUES (?, 'prj_1', 'slug', '/secret/project/path', 'secret session title', '1.19.4', ?, ?)`, session, createdMs, updatedMs)
	s.exec(`UPDATE session SET time_updated = max(time_updated, ?) WHERE id = ?`, updatedMs, session)
}

// v1Assistant writes a V1 assistant message and one step-finish part per step;
// completedMs 0 leaves it running.
func (s *openCode119TestStore) v1Assistant(session, id string, createdMs, completedMs int64, steps [][3]int64, cost float64) {
	s.t.Helper()
	updated, completed := createdMs, ""
	if completedMs > 0 {
		updated, completed = completedMs, fmt.Sprintf(`,"completed":%d`, completedMs)
	}
	s.head(session, createdMs, updated)
	last := [3]int64{}
	if len(steps) > 0 {
		last = steps[len(steps)-1]
	}
	s.exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`, id, session, createdMs, updated, fmt.Sprintf(
		`{"role":"assistant","parentID":"msg_u","path":{"cwd":"/secret/project/path"},"cost":%g,"tokens":{"input":%d,"output":%d,"reasoning":%d,"cache":{"read":0,"write":0}},"time":{"created":%d%s}}`,
		cost, last[0], last[1], last[2], createdMs, completed))
	s.exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?)`,
		"prt_"+id+"_text", id, session, createdMs, updated, `{"type":"text","text":"secret model output"}`)
	for i, st := range steps {
		s.exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?)`,
			fmt.Sprintf("prt_%s_%d", id, i), id, session, createdMs, updated, fmt.Sprintf(
				`{"type":"step-finish","reason":"stop","cost":%g,"tokens":{"input":%d,"output":%d,"reasoning":%d,"cache":{"read":0,"write":0}}}`,
				cost/float64(len(steps)), st[0], st[1], st[2]))
	}
}

// v2Assistant writes a completed OpenCode 2 assistant row under the shared
// session heads, as a 2.x client sharing the store would.
func (s *openCode119TestStore) v2Assistant(session, id string, createdMs, completedMs, input int64) {
	s.t.Helper()
	s.head(session, createdMs, completedMs)
	s.seq[session]++
	s.exec(`INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data) VALUES (?, ?, 'assistant', ?, ?, ?, ?)`,
		id, session, s.seq[session], createdMs, completedMs, fmt.Sprintf(
			`{"text":"secret model output","cost":0,"tokens":{"input":%d,"output":0,"reasoning":0,"cache":{"read":0,"write":0}},"time":{"created":%d,"completed":%d}}`,
			input, createdMs, completedMs))
}

// openCode119Fixture is the direct fixture over a 1.19 store, with the export
// seam counting calls and paying nothing: the store must pay first.
func openCode119Fixture(t *testing.T, epoch int64) (*openCodeUsageTestScheduler, *openCode119TestStore, *openCodeDirectTestClock, *int) {
	t.Helper()
	sched, fixture, clock := openCodeDirectFixture(t, epoch)
	store := newOpenCode119TestStore(t, fixture.root)
	exports := stubOpenCodeExport(t, func(context.Context, string) ([]byte, bool) {
		return []byte(`{"info":{"id":"ses"},"messages":[]}`), true
	})
	openCodeSQLiteSharedHeadsElected.Store("")
	return sched, store, clock, exports
}

// The 1.19 regression: a passing smoke whose stream reports no step_finish
// owes a debt, and the first rung pays it from V1's message table — never
// store_empty from the empty session_message, never the export.
func TestOpenCode119_ASmokeWithNoUsageInTheStreamIsPaidFromV1(t *testing.T) {
	sched, store, clock, exports := openCode119Fixture(t, 4101)
	openCodeSmokeEnv(t)
	path := stubOpenCodeBinary(t)
	stubOpenCodeReadiness(t, "anthropic/claude-sonnet-4-5\n", true)
	stubOpenCodeSmokeExec(t, func(_ context.Context, launch openCodeLaunch) ([]byte, []byte, error) {
		clock.now = clock.now.Add(time.Second)
		store.v1Assistant("ses_probe", "msg_smoke", clock.ms(-500*time.Millisecond), clock.ms(0), [][3]int64{{1200, 34, 5}}, 0.0123)
		return openCodeSuccessFrames(openCodeMarkerFromLaunch(t, launch)), nil, nil
	})

	if result := runOpenCodeSmoke(context.Background(), path, "1.19.4"); result.Status != cliSmokeStatusSuccess {
		t.Fatalf("smoke = %+v, want success", result)
	}
	if ledger := loadOpenCodeUsageLedger(); len(ledger.Debts) != 1 || !ledger.Debts[0].owed() {
		t.Fatalf("debts = %+v, want the smoke's owed debt", ledger.Debts)
	}
	logs := captureStdout(t, func() { sched.fireNext() })
	if strings.Contains(logs, "store_empty") {
		t.Fatalf("the debt read the empty V2 table: %q", logs)
	}
	if n := openCodeTodayTokens(t, clock.now); n != 1239 {
		t.Fatalf("tokens today = %d, want the stored 1239", n)
	}
	ledger := loadOpenCodeUsageLedger()
	if len(ledger.Debts) != 0 {
		t.Fatalf("debts = %+v after the store paid", ledger.Debts)
	}
	if !openCodeLedgerHasStep(ledger, openCodeUsageStepKey("ses_probe", "message:msg_smoke")) {
		t.Fatal("the paid step is not keyed as the export keys a message")
	}
	if *exports != 0 {
		t.Fatalf("the export ran %d times", *exports)
	}
}

// A terminal-managed `opencode run` in a PTY streams no step-finish frames: the
// session's exit settles its run, and the store pays it from V1.
func TestOpenCode119_ATerminalManagedRunIsPaidOnceItsStreamCloses(t *testing.T) {
	sched, store, clock, exports := openCode119Fixture(t, 4102)
	run := armOpenCodeUsageRun("opencode", "", "fp-119")
	captureOpenCodeUsageLine(run, `{"type":"session.created","sessionID":"ses_term"}`)
	captureOpenCodeUsageLine(run, "\x1b[32m> build\x1b[0m working…")
	openCodeUsageInFlight.Wait()
	clock.now = clock.now.Add(time.Second)
	store.v1Assistant("ses_term", "msg_t", clock.ms(-500*time.Millisecond), clock.ms(0), [][3]int64{{50, 3, 0}, {80, 8, 0}}, 0.02)
	session := &CLISession{openCodeUsageRun: run}
	session.settleOpenCodeUsageRun(false)

	for sched.fireNext() {
	}
	if n := openCodeTodayTokens(t, clock.now); n != 141 {
		t.Fatalf("tokens today = %d, want the stored steps' 141", n)
	}
	if d := openCodeDebtOf(t, run.id); d != nil {
		t.Fatalf("debt = %+v, want it paid", d)
	}
	if *exports != 0 {
		t.Fatalf("the export ran %d times", *exports)
	}
}

// A stream that carried step_finish usage commits at settle; the V1 store holds
// the same turn, and neither a click's payment nor the direct scan adds to it.
func TestOpenCode119_AStreamThatCarriedUsageIsNotPaidAgain(t *testing.T) {
	_, store, clock, _ := openCode119Fixture(t, 4103)
	run := armOpenCodeUsageRun("opencode", "", "fp-119")
	clock.now = clock.now.Add(time.Second)
	captureOpenCodeUsageLine(run, openCodeStepFinish("ses_s", "prt_1", 300, 20, 0, "0.01", clock.ms(0)))
	openCodeUsageInFlight.Wait()
	store.v1Assistant("ses_s", "msg_s", clock.ms(-500*time.Millisecond), clock.ms(0), [][3]int64{{300, 20, 0}}, 0.01)
	settleOpenCodeUsageRun(run, "")

	if paid := payOwedOpenCodeUsageFromStore(context.Background(), openCodeUsageMaxDebts); paid != 0 {
		t.Fatalf("a click paid %d debts for a settled stream", paid)
	}
	if label := scanOpenCodeDirect(t); label != "direct_scanned" && label != "direct_unchanged" {
		t.Fatalf("scan = %q", label)
	}
	if n := openCodeTodayTokens(t, clock.now); n != 320 {
		t.Fatalf("tokens today = %d, want the stream's 320 once", n)
	}
}

// A run from the user's own shell owes no debt: the tick counts it from V1 and
// the propagator hints its generation; a second tick over an unchanged store
// reads nothing and hints nothing newer.
func TestOpenCode119_ADirectRunIsCountedByTheTickOnceAndHintedOnce(t *testing.T) {
	_, store, clock, _ := openCode119Fixture(t, 4104)
	// A minute back from the real clock: a run an earlier test armed in this
	// process holds the reader from its own floor.
	clock.now = time.Now().Add(-time.Minute).Truncate(time.Second)
	rec, cfg := propagatorFixture(t)
	prevDetected, prevSettle := openCodeDirectDetected, openCodeDirectSettleWindow
	openCodeDirectDetected = func() bool { return true }
	openCodeDirectSettleWindow = 0
	t.Cleanup(func() { openCodeDirectDetected, openCodeDirectSettleWindow = prevDetected, prevSettle })
	openCodeGenerationRotated.Store(true)
	t.Cleanup(func() {
		stopCLIUsagePropagator()
		resetCLIUsagePropagator()
	})
	startCLIUsagePropagator(cfg)
	cliUsagePropagator.rotating.Wait()
	reads := countOpenCodeDirectReads(t, openCodeStoreLayoutSQLite)

	store.v1Assistant("ses_shell", "msg_shell", clock.ms(-30*time.Second), clock.ms(-29*time.Second), [][3]int64{{64, 6, 0}}, 0.02)
	openCodeDirectScanIfChanged()
	if n := openCodeTodayTokens(t, clock.now); n != 70 {
		t.Fatalf("tokens today = %d, want the direct 70", n)
	}
	ledger := loadOpenCodeUsageLedger()
	if c := ledger.DirectCoverage; c == nil || c.Layout != openCodeStoreLayoutSQLite {
		t.Fatalf("coverage = %+v, want today's V1 coverage", c)
	}
	want := ledger.Generation
	waitHintFor(t, rec, gen(want.Epoch, want.Counter))

	before := reads.Load()
	openCodeDirectScanIfChanged()
	if n := reads.Load(); n != before {
		t.Fatalf("the unchanged store was read %d more times", n-before)
	}
	time.Sleep(2 * cliUsageHintSpacing)
	if got := loadOpenCodeUsageLedger().Generation; got != want {
		t.Fatalf("generation = %+v after an unchanged tick, want %+v", got, want)
	}
	for _, r := range rec.all() {
		if r.hint.Provider == openCodeUsageProvider && (r.hint.GenerationEpoch != want.Epoch || r.hint.Generation != want.Counter) {
			t.Fatalf("hinted %+v, want only the scan's generation %+v", r.hint, want)
		}
	}
}

// A Refresh click pays an owed V1 debt from the store inside its own budget —
// before the ladder's first rung fires, so no attempt is spent.
func TestOpenCode119_ARefreshPaysOwedDebtsFromV1WithinItsBudget(t *testing.T) {
	sched, store, clock, exports := openCode119Fixture(t, 4105)
	run := armOpenCodeUsageRun("opencode", "", "fp-119")
	captureOpenCodeUsageLine(run, `{"type":"session.created","sessionID":"ses_click"}`)
	openCodeUsageInFlight.Wait()
	clock.now = clock.now.Add(time.Second)
	store.v1Assistant("ses_click", "msg_click", clock.ms(-500*time.Millisecond), clock.ms(0), [][3]int64{{40, 2, 0}}, 0)
	settleOpenCodeUsageRun(run, "")
	if d := openCodeDebtOf(t, run.id); d == nil || !d.owed() || d.Attempts != 0 {
		t.Fatalf("debt = %+v, want it owed", d)
	}

	if got := openCodeDirectScanOnRefresh(context.Background()); got != liveProbeOutcomeOK {
		t.Fatalf("refresh = %q, want %q", got, liveProbeOutcomeOK)
	}
	if d := openCodeDebtOf(t, run.id); d != nil {
		t.Fatalf("debt = %+v, want the click to have paid it", d)
	}
	if n := openCodeTodayTokens(t, clock.now); n != 42 {
		t.Fatalf("tokens today = %d, want the debt's 42 once", n)
	}
	// The rung booked at settle finds nothing left to pay.
	for sched.fireNext() {
	}
	if n := openCodeTodayTokens(t, clock.now); n != 42 || *exports != 0 {
		t.Fatalf("tokens today = %d, exports = %d after the rung", n, *exports)
	}
}

// OpenCode holding the store's write lock (a rollback-journal store, where a
// writer excludes readers) makes the election a read to retry: the cursor and
// today's coverage stay as they were.
func TestOpenCode119_ABusyStoreDefersWithoutRenewingCoverage(t *testing.T) {
	_, fixture, clock := openCodeDirectFixture(t, 4106)
	db, err := sql.Open("sqlite", filepath.Join(fixture.root, "opencode.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=DELETE;` + openCodeSQLite119TestSchema); err != nil {
		t.Fatal(err)
	}
	store := &openCode119TestStore{t: t, db: db, root: fixture.root, seq: map[string]int{}}
	store.v1Assistant("ses_busy", "msg_busy", clock.ms(-time.Hour), clock.ms(-time.Hour+time.Second), [][3]int64{{10, 0, 0}}, 0)
	yesterday, _ := openCodeLocalDay(clock.now.Add(-24 * time.Hour))
	cursor := &openCodeDirectCursor{Layout: openCodeStoreLayoutSQLite, ThroughMs: clock.ms(-2 * time.Hour)}
	coverage := &openCodeDirectCoverage{Layout: openCodeStoreLayoutSQLite, ObservedAtMs: clock.ms(-2 * time.Hour), LastOkLocalDate: yesterday}
	openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		ledger.DirectCursor, ledger.DirectCoverage = cursor, coverage
		return true, false
	})

	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	label, again := openCodeDirectScan(context.Background(), openCodeStoreLayoutSQLite, fixture.root)
	if _, err := conn.ExecContext(context.Background(), `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	if label != "direct_read_failed" || !again {
		t.Fatalf("scan = %q, again %v; want a read to retry", label, again)
	}
	ledger := loadOpenCodeUsageLedger()
	if c := ledger.DirectCursor; c == nil || !reflect.DeepEqual(*c, *cursor) {
		t.Fatalf("cursor = %+v, want it untouched", c)
	}
	if c := ledger.DirectCoverage; c == nil || *c != *coverage {
		t.Fatalf("coverage = %+v, want it untouched", c)
	}
	if n := openCodeTodayTokens(t, clock.now); n != 0 {
		t.Fatalf("tokens today = %d from a busy store", n)
	}
}

// openCodeLedgerHasStep reports whether the ledger remembers the step key.
func openCodeLedgerHasStep(ledger openCodeUsageLedger, key string) bool {
	for _, k := range ledger.SeenSteps {
		if k == key {
			return true
		}
	}
	return false
}
