package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_opencode_direct_sqlite_test.go — the SQLite adapter reads a
   temporary opencode.db with OpenCode's schema: read-only, fail-closed on a
   schema it does not know, unblocked by OpenCode's own writer, and capped.
   ------------------------------------------------------------------------ */

// openCodeSQLiteTestSchema is the subset of OpenCode's tables the reader uses,
// as OpenCode declares them.
const openCodeSQLiteTestSchema = `
CREATE TABLE session (id text PRIMARY KEY, project_id text NOT NULL, slug text NOT NULL, directory text NOT NULL, title text NOT NULL, version text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL);
CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
CREATE TABLE part (id text PRIMARY KEY, message_id text NOT NULL, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
CREATE INDEX message_session_time_created_id_idx ON message (session_id, time_created, id);
CREATE INDEX part_message_id_id_idx ON part (message_id, id);`

// openCodeSQLiteTestStore builds opencode.db under root in WAL mode, as
// OpenCode runs it, and returns a writable handle (OpenCode's side).
func openCodeSQLiteTestStore(t *testing.T, root, schema string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, "opencode.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;` + schema); err != nil {
		t.Fatal(err)
	}
	return db
}

func insertOpenCodeSQLiteMessage(t *testing.T, db interface {
	Exec(string, ...any) (sql.Result, error)
}, session, id string, createdMs, completedMs, updatedMs int64, steps [][3]int64, cost float64) {
	t.Helper()
	if _, err := db.Exec(`INSERT OR IGNORE INTO session VALUES (?, 'p', 's', '/w', 'secret title', '1', ?, ?)`, session, createdMs, updatedMs); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE session SET time_updated = max(time_updated, ?) WHERE id = ?`, updatedMs, session); err != nil {
		t.Fatal(err)
	}
	completed := ""
	if completedMs > 0 {
		completed = fmt.Sprintf(`,"completed":%d`, completedMs)
	}
	last := [3]int64{}
	if len(steps) > 0 {
		last = steps[len(steps)-1]
	}
	data := fmt.Sprintf(`{"role":"assistant","parentID":"msg_u","path":{"cwd":"/w"},"cost":%g,"tokens":{"total":0,"input":%d,"output":%d,"reasoning":%d,"cache":{"read":0,"write":0}},"time":{"created":%d%s}}`,
		cost, last[0], last[1], last[2], createdMs, completed)
	if _, err := db.Exec(`INSERT INTO message VALUES (?, ?, ?, ?, ?)`, id, session, createdMs, updatedMs, data); err != nil {
		t.Fatal(err)
	}
	for i, st := range steps {
		part := fmt.Sprintf(`{"type":"step-finish","reason":"stop","cost":%g,"tokens":{"input":%d,"output":%d,"reasoning":%d,"cache":{"read":0,"write":0}}}`, cost/float64(len(steps)), st[0], st[1], st[2])
		if _, err := db.Exec(`INSERT INTO part VALUES (?, ?, ?, ?, ?, ?)`, fmt.Sprintf("prt_%s_%d", id, i), id, session, createdMs, updatedMs, part); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO part VALUES (?, ?, ?, ?, ?, '{"type":"text","text":"model output"}')`, "prt_text_"+id, id, session, createdMs, updatedMs); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodeDirectSQLite_ReadsAssistantMessagesSince(t *testing.T) {
	root := t.TempDir()
	db := openCodeSQLiteTestStore(t, root, openCodeSQLiteTestSchema)
	base := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC).UnixMilli()
	insertOpenCodeSQLiteMessage(t, db, "ses_old", "msg_old", base-7200_000, base-7100_000, base-7100_000, [][3]int64{{9, 0, 0}}, 0)
	insertOpenCodeSQLiteMessage(t, db, "ses_a", "msg_b", base+20_000, base+30_000, base+30_000, [][3]int64{{2, 1, 0}}, 0.01)
	insertOpenCodeSQLiteMessage(t, db, "ses_a", "msg_a", base, base+10_000, base+10_000, [][3]int64{{10, 2, 0}, {12, 3, 1}}, 0.05)
	insertOpenCodeSQLiteMessage(t, db, "ses_a", "msg_run", base+40_000, 0, base+40_000, nil, 0)
	if _, err := db.Exec(`INSERT INTO message VALUES ('msg_user', 'ses_a', ?, ?, '{"role":"user","time":{"created":1}}')`, base+5_000, base+5_000); err != nil {
		t.Fatal(err)
	}

	read, err := readOpenCodeDirectSQLite(context.Background(), root, base, openCodeDirectTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Messages) != 3 || read.Messages[0].ID != "msg_a" || read.Messages[1].ID != "msg_b" || read.Messages[2].ID != "msg_run" {
		t.Fatalf("messages = %+v", read.Messages)
	}
	if u := read.Messages[0].Usage; !read.Messages[0].Valid || u.Input != 22 || u.Output != 5 || u.Reasoning != 1 || u.Cost < 0.0499 {
		t.Fatalf("summed usage = %+v", u)
	}
	if read.Messages[2].CompletedMs != 0 {
		t.Fatalf("a running message read as complete: %+v", read.Messages[2])
	}
}

// A row whose tokens are not an object is an invalid message, not a failed
// query: the rows after it are still read.
func TestOpenCodeDirectSQLite_AMalformedTokensValueDoesNotFailTheRead(t *testing.T) {
	root := t.TempDir()
	db := openCodeSQLiteTestStore(t, root, openCodeSQLiteTestSchema)
	base := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC).UnixMilli()
	insertOpenCodeSQLiteMessage(t, db, "ses_a", "msg_ok", base, base+10, base+10, [][3]int64{{7, 0, 0}}, 0)
	if _, err := db.Exec(`INSERT INTO message VALUES ('msg_bad', 'ses_a', ?, ?, ?)`, base, base+5,
		fmt.Sprintf(`{"role":"assistant","tokens":"lots","cost":0,"time":{"created":%d,"completed":%d}}`, base, base+5)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO part VALUES ('prt_bad', 'msg_ok', 'ses_a', ?, ?, '{"type":"step-finish","tokens":"lots","cost":0}')`, base, base+10); err != nil {
		t.Fatal(err)
	}

	read, err := readOpenCodeDirectSQLite(context.Background(), root, base, openCodeDirectTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Messages) != 2 || read.Messages[0].ID != "msg_bad" || read.Messages[0].Valid {
		t.Fatalf("messages = %+v, want the malformed row read as invalid", read.Messages)
	}
	if m := read.Messages[1]; m.ID != "msg_ok" || !m.Valid || m.Usage.Input != 7 {
		t.Fatalf("valid row after the malformed one = %+v", m)
	}
}

// The adapter opens read-only: a write through its DSN fails.
func TestOpenCodeDirectSQLite_OpensReadOnly(t *testing.T) {
	root := t.TempDir()
	openCodeSQLiteTestStore(t, root, openCodeSQLiteTestSchema)
	ro, err := sql.Open("sqlite", openCodeSQLiteReadOnlyDSN(filepath.Join(root, "opencode.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.Exec(`INSERT INTO session VALUES ('ses_x', 'p', 's', '/w', 't', '1', 1, 1)`); err == nil {
		t.Fatal("a write through the read-only DSN succeeded")
	}
	var n int
	if err := ro.QueryRow(`SELECT count(*) FROM session`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("count = %d, %v", n, err)
	}
}

// A schema missing a column the reader needs fails closed.
func TestOpenCodeDirectSQLite_ASchemaMismatchIsUnknown(t *testing.T) {
	root := t.TempDir()
	openCodeSQLiteTestStore(t, root, `
CREATE TABLE session (id text PRIMARY KEY, time_updated integer NOT NULL);
CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL, payload text NOT NULL);
CREATE TABLE part (id text PRIMARY KEY, message_id text NOT NULL, data text NOT NULL);`)
	if _, err := readOpenCodeDirectSQLite(context.Background(), root, 0, openCodeDirectTestLimits()); !errors.Is(err, errOpenCodeDirectLayoutUnknown) {
		t.Fatalf("err = %v, want layout unknown", err)
	}
}

// OpenCode holding a write transaction does not block or fail the read.
func TestOpenCodeDirectSQLite_ReadsWhileOpenCodeHoldsAWriteTransaction(t *testing.T) {
	root := t.TempDir()
	db := openCodeSQLiteTestStore(t, root, openCodeSQLiteTestSchema)
	base := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC).UnixMilli()
	insertOpenCodeSQLiteMessage(t, db, "ses_a", "msg_a", base, base+1, base+1, [][3]int64{{5, 0, 0}}, 0)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO message VALUES ('msg_pending', 'ses_a', ?, ?, '{"role":"assistant","time":{"created":1,"completed":2},"tokens":{"input":99}}')`, base+2, base+2); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	read, err := readOpenCodeDirectSQLite(context.Background(), root, base, openCodeDirectTestLimits())
	if err != nil || len(read.Messages) != 1 || read.Messages[0].ID != "msg_a" {
		t.Fatalf("read = %+v, %v", read, err)
	}
	if took := time.Since(start); took > openCodeSQLiteBusyTimeout {
		t.Fatalf("the read waited %s on the writer", took)
	}
}

// LIMIT caps truncate at the first unread record; the session cap at the first
// unread session.
func TestOpenCodeDirectSQLite_CapsTruncate(t *testing.T) {
	root := t.TempDir()
	db := openCodeSQLiteTestStore(t, root, openCodeSQLiteTestSchema)
	base := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC).UnixMilli()
	for i := int64(0); i < 3; i++ {
		insertOpenCodeSQLiteMessage(t, db, "ses_a", fmt.Sprintf("msg_%d", i), base+i*1000, base+i*1000+1, base+i*1000+1, [][3]int64{{1, 0, 0}}, 0)
	}
	limits := openCodeDirectTestLimits()
	limits.MaxRecords = 2
	read, err := readOpenCodeDirectSQLite(context.Background(), root, base, limits)
	if err != nil || len(read.Messages) != 2 || !read.Truncated || read.ThroughMs != base+2001 {
		t.Fatalf("record-capped read = %+v, %v", read, err)
	}

	insertOpenCodeSQLiteMessage(t, db, "ses_b", "msg_b", base+5000, base+5001, base+5001, [][3]int64{{1, 0, 0}}, 0)
	limits = openCodeDirectTestLimits()
	limits.MaxSessions = 1
	read, err = readOpenCodeDirectSQLite(context.Background(), root, base, limits)
	if err != nil || !read.Truncated || read.ThroughMs != base+5001 || len(read.Messages) != 3 {
		t.Fatalf("session-capped read = %+v, %v", read, err)
	}

	limits = openCodeDirectTestLimits()
	limits.MaxRecordBytes = 64
	read, err = readOpenCodeDirectSQLite(context.Background(), root, base, limits)
	if err != nil || read.Skipped != 4 || len(read.Messages) != 0 {
		t.Fatalf("byte-capped read = %+v, %v", read, err)
	}
}

// A message's step parts are capped before SQLite aggregates them, as the JSON
// adapter caps its part listing: a turn with more parts reads as an under-count.
func TestOpenCodeDirectSQLite_CapsTheStepPartsOfOneMessage(t *testing.T) {
	root := t.TempDir()
	db := openCodeSQLiteTestStore(t, root, openCodeSQLiteTestSchema)
	base := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC).UnixMilli()
	steps := make([][3]int64, openCodeDirectMaxPartsPerMessage+5)
	for i := range steps {
		steps[i] = [3]int64{1, 0, 0}
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	insertOpenCodeSQLiteMessage(t, tx, "ses_a", "msg_long", base, base+10, base+10, steps, 0)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	read, err := readOpenCodeDirectSQLite(context.Background(), root, base, openCodeDirectTestLimits())
	if err != nil || len(read.Messages) != 1 {
		t.Fatalf("read = %+v, %v", read, err)
	}
	if m := read.Messages[0]; !m.Valid || m.Usage.Input != int64(openCodeDirectMaxPartsPerMessage) {
		t.Fatalf("usage = %+v, want the first %d step parts summed", m.Usage, openCodeDirectMaxPartsPerMessage)
	}
}

// A session-capped continuation over SQLite reads the sessions it has not
// listed yet from the capped scan's record floor: rows OpenCode touched without
// touching their messages keep those messages countable.
func TestOpenCodeDirectSQLite_ASessionContinuationKeepsTheRecordFloor(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2102)
	db := openCodeSQLiteTestStore(t, store.root, openCodeSQLiteTestSchema)
	for i := 0; i < 3; i++ {
		at := clock.ms(time.Duration(i-30) * time.Minute)
		insertOpenCodeSQLiteMessage(t, db, fmt.Sprintf("ses_%d", i), fmt.Sprintf("msg_%d", i), at-1000, at, at, [][3]int64{{1, 0, 0}}, 0)
	}
	if _, err := db.Exec(`UPDATE session SET time_updated = ?`, clock.ms(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	openCodeDirectMaxSessions = 1
	for i := 0; i < 3; i++ {
		scanOpenCodeDirect(t)
	}
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 3 {
		t.Fatalf("bucket = %+v, want every session's earlier message counted", b)
	}
}

// A continuation's tie-breaker drops the rows the capped read took at its cut
// time, so rows sharing one millisecond past the cap are reached.
func TestOpenCodeDirectSQLite_TheTieBreakerResumesInsideOneMillisecond(t *testing.T) {
	root := t.TempDir()
	db := openCodeSQLiteTestStore(t, root, openCodeSQLiteTestSchema)
	base := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC).UnixMilli()
	for i := 0; i < 5; i++ {
		insertOpenCodeSQLiteMessage(t, db, "ses_a", fmt.Sprintf("msg_%d", i), base, base+1, base+1, [][3]int64{{1, 0, 0}}, 0)
	}
	limits := openCodeDirectTestLimits()
	limits.MaxRecords = 2
	seen := map[string]bool{}
	floor := base
	for i := 0; i < 3; i++ {
		read, err := readOpenCodeDirectSQLite(context.Background(), root, floor, limits)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range read.Messages {
			if seen[m.ID] {
				t.Fatalf("pass %d re-read %s", i, m.ID)
			}
			seen[m.ID] = true
		}
		if !read.Truncated {
			break
		}
		floor = read.ThroughMs
		limits.Skip = openCodeDirectSkip{AtMs: read.ThroughMs, N: read.ThroughSkip}
	}
	if len(seen) != 5 {
		t.Fatalf("read %d of 5 rows sharing one millisecond", len(seen))
	}
}

// The reader end to end over SQLite: the layout is detected and today's spend
// lands in the ledger.
func TestOpenCodeDirectSQLite_ScanCommitsToTheLedger(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2101)
	db := openCodeSQLiteTestStore(t, store.root, openCodeSQLiteTestSchema)
	insertOpenCodeSQLiteMessage(t, db, "ses_a", "msg_a", clock.ms(-time.Hour), clock.ms(-time.Hour+time.Second), clock.ms(-time.Hour+time.Second), [][3]int64{{40, 2, 0}}, 0.02)
	if layout, _ := openCodeStoreLayout(); layout != openCodeStoreLayoutSQLite {
		t.Fatalf("layout = %q", layout)
	}
	if label := scanOpenCodeDirect(t); label != "direct_scanned" {
		t.Fatalf("scan = %q", label)
	}
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 42 || b.CostUsd != 0.02 {
		t.Fatalf("bucket = %+v", b)
	}
}
