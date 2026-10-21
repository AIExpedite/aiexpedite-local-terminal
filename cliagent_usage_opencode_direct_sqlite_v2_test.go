package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_opencode_direct_sqlite_v2_test.go — the SQLite adapter reads
   OpenCode 2's schema family (session_v2 / session_message) from a temporary
   opencode.db: assistant rows by type, completion by completed time or a later
   idle row, fail-closed on a shape it does not know, the family election on
   mixed stores, and the caps V1 keeps. No captured user rows: every store is
   built here.
   ------------------------------------------------------------------------ */

// openCodeSQLiteV2TestSchema is OpenCode 2's session and message tables, as an
// OpenCode 2.0.x store declares the columns the reader uses.
const openCodeSQLiteV2TestSchema = `
CREATE TABLE session_v2 (id text PRIMARY KEY, project_id text NOT NULL, directory text NOT NULL, title text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL);
CREATE TABLE session_message (id text PRIMARY KEY, session_id text NOT NULL, seq integer NOT NULL, type text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);`

// openCodeV2TestStore writes OpenCode 2 rows: each one takes its session's next
// seq and moves the session head's time_updated forward.
type openCodeV2TestStore struct {
	t     *testing.T
	db    *sql.DB
	heads string
	seq   map[string]int
}

func newOpenCodeV2TestStore(t *testing.T, root string) *openCodeV2TestStore {
	t.Helper()
	return &openCodeV2TestStore{t: t, db: openCodeSQLiteTestStore(t, root, openCodeSQLiteV2TestSchema), heads: "session_v2", seq: map[string]int{}}
}

// row inserts one session_message row of type typ with raw data.
func (s *openCodeV2TestStore) row(session, id, typ string, createdMs, updatedMs int64, data string) {
	s.t.Helper()
	cols := "(id, project_id, directory, title, time_created, time_updated) VALUES (?, 'p', '/secret/project/path', 'secret session title', ?, ?)"
	if s.heads == "session" {
		cols = "VALUES (?, 'p', 's', '/secret/project/path', 'secret session title', '2', ?, ?)"
	}
	if _, err := s.db.Exec(fmt.Sprintf(`INSERT OR IGNORE INTO %q `, s.heads)+cols, session, createdMs, updatedMs); err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.db.Exec(fmt.Sprintf(`UPDATE %q SET time_updated = max(time_updated, ?) WHERE id = ?`, s.heads), updatedMs, session); err != nil {
		s.t.Fatal(err)
	}
	s.seq[session]++
	if _, err := s.db.Exec(`INSERT INTO session_message (id, session_id, seq, type, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, session, s.seq[session], typ, createdMs, updatedMs, data); err != nil {
		s.t.Fatal(err)
	}
}

// assistant inserts an assistant message; completedMs 0 leaves it running.
func (s *openCodeV2TestStore) assistant(session, id string, createdMs, completedMs int64, in, out, reasoning int64, cost float64) {
	s.t.Helper()
	timeJSON := fmt.Sprintf(`{"created":%d}`, createdMs)
	updated := createdMs
	if completedMs > 0 {
		timeJSON = fmt.Sprintf(`{"created":%d,"completed":%d}`, createdMs, completedMs)
		updated = completedMs
	}
	s.row(session, id, "assistant", createdMs, updated, fmt.Sprintf(
		`{"agent":"build","model":{"providerID":"anthropic","id":"secret-model"},"text":"secret model output","cost":%g,"tokens":{"input":%d,"output":%d,"reasoning":%d,"cache":{"read":3,"write":0}},"time":%s}`,
		cost, in, out, reasoning, timeJSON))
}

// idle marks the session idle after its latest row.
func (s *openCodeV2TestStore) idle(session, id string, atMs int64) {
	s.t.Helper()
	s.row(session, id, "idle", atMs, atMs, `{}`)
}

func TestOpenCodeDirectSQLiteV2_ReadsAssistantRowsByType(t *testing.T) {
	root := t.TempDir()
	store := newOpenCodeV2TestStore(t, root)
	base := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC).UnixMilli()
	store.row("ses_a", "msg_user", "user", base, base, `{"text":"secret user prompt"}`)
	store.assistant("ses_a", "msg_a", base+10, base+20, 30, 4, 1, 0.03)
	store.row("ses_a", "msg_shell", "shell", base+30, base+30, `{"command":"cat ~/.ssh/id_rsa","tokens":{"input":999}}`)
	store.idle("ses_a", "msg_idle", base+40)

	read, err := readOpenCodeDirectSQLite(context.Background(), root, base, openCodeDirectTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	if read.Layout != openCodeStoreLayoutSQLiteV2 {
		t.Fatalf("layout = %q, want %q", read.Layout, openCodeStoreLayoutSQLiteV2)
	}
	if len(read.Messages) != 1 || read.Messages[0].ID != "msg_a" {
		t.Fatalf("messages = %+v, want the assistant row only", read.Messages)
	}
	m := read.Messages[0]
	if !m.Valid || m.CompletedMs != base+20 || m.Usage.Input != 30 || m.Usage.Output != 4 || m.Usage.Reasoning != 1 || m.Usage.Cost != 0.03 {
		t.Fatalf("message = %+v", m)
	}
}

// A message with no completed time is complete once a later idle row exists in
// its session; until then it reads as running.
func TestOpenCodeDirectSQLiteV2_ALaterIdleRowCompletesAMessage(t *testing.T) {
	root := t.TempDir()
	store := newOpenCodeV2TestStore(t, root)
	base := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC).UnixMilli()
	store.assistant("ses_a", "msg_a", base, 0, 7, 0, 0, 0)
	// An idle row in ANOTHER session, or before the message, completes nothing.
	store.idle("ses_b", "idle_b", base+5)

	read, err := readOpenCodeDirectSQLite(context.Background(), root, base, openCodeDirectTestLimits())
	if err != nil || len(read.Messages) != 1 || read.Messages[0].CompletedMs != 0 {
		t.Fatalf("running read = %+v, %v", read, err)
	}

	store.idle("ses_a", "idle_a", base+50)
	read, err = readOpenCodeDirectSQLite(context.Background(), root, base, openCodeDirectTestLimits())
	if err != nil || len(read.Messages) != 1 {
		t.Fatalf("read = %+v, %v", read, err)
	}
	if m := read.Messages[0]; m.CompletedMs != base || !m.Valid || m.Usage.Input != 7 {
		t.Fatalf("idled message = %+v, want complete at its last write", m)
	}
}

// End to end: a running message pins the cursor, later messages are still
// counted past it, and one older than the debt age-out stops pinning.
func TestOpenCodeDirectSQLiteV2_AnIncompleteMessagePinsUntilItAgesOut(t *testing.T) {
	_, fixture, clock := openCodeDirectFixture(t, 2201)
	store := newOpenCodeV2TestStore(t, fixture.root)
	running := clock.ms(-2 * time.Hour)
	store.assistant("ses_a", "msg_running", running, 0, 50, 0, 0, 0)
	store.assistant("ses_b", "msg_done", clock.ms(-time.Hour), clock.ms(-time.Hour+time.Second), 9, 1, 0, 0.01)

	if label := scanOpenCodeDirect(t); label != "direct_scanned" {
		t.Fatalf("scan = %q", label)
	}
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 10 {
		t.Fatalf("bucket = %+v, want the later message counted past the pin", b)
	}
	if c := loadOpenCodeUsageLedger().DirectCursor; c == nil || c.Layout != openCodeStoreLayoutSQLiteV2 || c.ThroughMs > running {
		t.Fatalf("cursor = %+v, want it pinned at or before %d", c, running)
	}

	clock.now = clock.now.Add(openCodeUsageDebtMaxAge) // still the same day: 18:00
	scanOpenCodeDirect(t)
	if c := loadOpenCodeUsageLedger().DirectCursor; c == nil || c.ThroughMs <= running {
		t.Fatalf("cursor = %+v, want the aged-out message to release it", c)
	}
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 10 {
		t.Fatalf("bucket = %+v, want the aged-out message never counted", b)
	}
}

func TestOpenCodeDirectSQLiteV2_AnUnknownShapeFailsClosed(t *testing.T) {
	for name, schema := range map[string]string{
		"no seq column": `
CREATE TABLE session_v2 (id text PRIMARY KEY, time_updated integer NOT NULL);
CREATE TABLE session_message (id text PRIMARY KEY, session_id text NOT NULL, type text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);`,
		"no heads table": `
CREATE TABLE session_message (id text PRIMARY KEY, session_id text NOT NULL, seq integer NOT NULL, type text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);`,
		"heads without time_updated": `
CREATE TABLE session_v2 (id text PRIMARY KEY, updated integer NOT NULL);
CREATE TABLE session_message (id text PRIMARY KEY, session_id text NOT NULL, seq integer NOT NULL, type text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);`,
		// A V1 store whose message table was dropped is not V2: it has no
		// session_message.
		"v1 without message": `
CREATE TABLE session (id text PRIMARY KEY, time_updated integer NOT NULL);
CREATE TABLE part (id text PRIMARY KEY, message_id text NOT NULL, data text NOT NULL);`,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			openCodeSQLiteTestStore(t, root, schema)
			if _, err := readOpenCodeDirectSQLite(context.Background(), root, 0, openCodeDirectTestLimits()); !errors.Is(err, errOpenCodeDirectLayoutUnknown) {
				t.Fatalf("err = %v, want layout unknown", err)
			}
		})
	}
}

// A pre-release store keeps its V2 heads in V1's own session table beside
// session_message: only V2 can have written session_message, so it reads as
// V2 even with V1's message and part tables present and newer.
func TestOpenCodeDirectSQLiteV2_PreReleaseHeadsInSessionReadAsV2(t *testing.T) {
	root := t.TempDir()
	v1 := openCodeSQLiteTestStore(t, root, openCodeSQLiteTestSchema)
	if _, err := v1.Exec(`CREATE TABLE session_message (id text PRIMARY KEY, session_id text NOT NULL, seq integer NOT NULL, type text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	store := &openCodeV2TestStore{t: t, db: v1, heads: "session", seq: map[string]int{}}
	base := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC).UnixMilli()
	store.assistant("ses_v2", "msg_v2", base, base+10, 11, 0, 0, 0)
	insertOpenCodeSQLiteMessage(t, v1, "ses_v1", "msg_v1", base+100, base+110, base+110, [][3]int64{{99, 0, 0}}, 0)

	read, err := readOpenCodeDirectSQLite(context.Background(), root, base, openCodeDirectTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	if read.Layout != openCodeStoreLayoutSQLiteV2 || len(read.Messages) != 1 || read.Messages[0].ID != "msg_v2" {
		t.Fatalf("read = %+v, want the V2 message only", read)
	}
}

// Both families on distinct heads: the newest heads win, a tie (both empty
// included) goes to V2, and both written since midnight logs the suppression.
func TestOpenCodeDirectSQLiteV2_TheFamilyElection(t *testing.T) {
	noon := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	prevNow, prevLoc := openCodeDirectNow, openCodeUsageLocation
	openCodeDirectNow = func() time.Time { return noon }
	openCodeUsageLocation = time.UTC
	t.Cleanup(func() { openCodeDirectNow, openCodeUsageLocation = prevNow, prevLoc })
	mixed := func(t *testing.T) (string, *sql.DB, *openCodeV2TestStore) {
		root := t.TempDir()
		db := openCodeSQLiteTestStore(t, root, openCodeSQLiteTestSchema+openCodeSQLiteV2TestSchema)
		return root, db, &openCodeV2TestStore{t: t, db: db, heads: "session_v2", seq: map[string]int{}}
	}
	elect := func(t *testing.T, root string) string {
		t.Helper()
		read, err := readOpenCodeDirectSQLite(context.Background(), root, 0, openCodeDirectTestLimits())
		if err != nil {
			t.Fatal(err)
		}
		return read.Layout
	}
	morning := noon.Add(-2 * time.Hour).UnixMilli()
	yesterday := noon.Add(-24 * time.Hour).UnixMilli()

	t.Run("both empty is a tie V2 wins", func(t *testing.T) {
		root, _, _ := mixed(t)
		if got := elect(t, root); got != openCodeStoreLayoutSQLiteV2 {
			t.Fatalf("layout = %q", got)
		}
	})
	t.Run("V2 heads newer", func(t *testing.T) {
		root, db, v2 := mixed(t)
		insertOpenCodeSQLiteMessage(t, db, "ses_1", "msg_1", yesterday, yesterday+1, yesterday+1, [][3]int64{{1, 0, 0}}, 0)
		v2.assistant("ses_2", "msg_2", morning, morning+1, 1, 0, 0, 0)
		var got string
		out := captureStdout(t, func() { got = elect(t, root) })
		if got != openCodeStoreLayoutSQLiteV2 {
			t.Fatalf("layout = %q", got)
		}
		if strings.Contains(out, "direct_family_suppressed") {
			t.Fatalf("logged a suppression with V1 idle since yesterday: %q", out)
		}
	})
	t.Run("only V1 heads written is V1 even with V2 tables empty", func(t *testing.T) {
		root, db, _ := mixed(t)
		insertOpenCodeSQLiteMessage(t, db, "ses_1", "msg_1", morning, morning+1, morning+1, [][3]int64{{1, 0, 0}}, 0)
		if got := elect(t, root); got != openCodeStoreLayoutSQLite {
			t.Fatalf("layout = %q", got)
		}
	})
	t.Run("both written today logs the suppression", func(t *testing.T) {
		root, db, v2 := mixed(t)
		insertOpenCodeSQLiteMessage(t, db, "ses_1", "msg_1", morning, morning+1, morning+1, [][3]int64{{1, 0, 0}}, 0)
		v2.assistant("ses_2", "msg_2", morning+5, morning+6, 1, 0, 0, 0)
		var got string
		out := captureStdout(t, func() { got = elect(t, root) })
		if got != openCodeStoreLayoutSQLiteV2 || !strings.Contains(out, "direct_family_suppressed") {
			t.Fatalf("layout = %q, log = %q", got, out)
		}
	})
}

// The record cap and the one-millisecond tie-breaker truncate and resume as in
// V1; the session cap stops at the first unread session.
func TestOpenCodeDirectSQLiteV2_CapsAndTheTieBreakerResume(t *testing.T) {
	root := t.TempDir()
	store := newOpenCodeV2TestStore(t, root)
	base := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC).UnixMilli()
	for i := 0; i < 5; i++ {
		store.assistant("ses_a", fmt.Sprintf("msg_%d", i), base, base+1, 1, 0, 0, 0)
	}
	limits := openCodeDirectTestLimits()
	limits.MaxRecords = 2
	seen := map[string]bool{}
	floor := base
	for i := 0; i < 4; i++ {
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
		if read.ThroughMs != base+1 {
			t.Fatalf("pass %d cut at %d, want %d", i, read.ThroughMs, base+1)
		}
		floor = read.ThroughMs
		limits.Skip = openCodeDirectSkip{AtMs: read.ThroughMs, N: read.ThroughSkip}
	}
	if len(seen) != 5 {
		t.Fatalf("read %d of 5 rows sharing one millisecond", len(seen))
	}

	store.assistant("ses_b", "msg_b", base+5000, base+5001, 1, 0, 0, 0)
	limits = openCodeDirectTestLimits()
	limits.MaxSessions = 1
	read, err := readOpenCodeDirectSQLite(context.Background(), root, base, limits)
	if err != nil || !read.Truncated || !read.AtSession || read.ThroughMs != base+5001 || len(read.Messages) != 5 {
		t.Fatalf("session-capped read = %+v, %v", read, err)
	}
}

// A spent budget truncates instead of failing: nothing past the cut is counted.
func TestOpenCodeDirectSQLiteV2_ASpentBudgetTruncates(t *testing.T) {
	root := t.TempDir()
	store := newOpenCodeV2TestStore(t, root)
	base := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC).UnixMilli()
	store.assistant("ses_a", "msg_a", base, base+1, 1, 0, 0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	read, err := readOpenCodeDirectSQLite(ctx, root, base, openCodeDirectTestLimits())
	if err == nil && len(read.Messages) != 0 {
		t.Fatalf("read = %+v, want nothing counted after the budget", read)
	}
}

// Byte caps count bytes, not characters, and a malformed tokens value skips
// its own row, never the read.
func TestOpenCodeDirectSQLiteV2_ByteCapsAndMalformedRows(t *testing.T) {
	root := t.TempDir()
	store := newOpenCodeV2TestStore(t, root)
	base := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC).UnixMilli()
	big := fmt.Sprintf(`{"text":%q,"cost":0,"tokens":{"input":5,"output":0,"reasoning":0,"cache":{"read":0,"write":0}},"time":{"created":%d,"completed":%d}}`,
		strings.Repeat("€", 300), base, base+10)
	store.row("ses_a", "msg_big", "assistant", base, base+10, big)
	store.row("ses_a", "msg_bad", "assistant", base, base+11, fmt.Sprintf(`{"tokens":"lots","cost":0,"time":{"created":%d,"completed":%d}}`, base, base+11))
	store.assistant("ses_a", "msg_ok", base, base+12, 7, 0, 0, 0)

	limits := openCodeDirectTestLimits()
	limits.MaxRecordBytes = int64(len([]rune(big))) + 1 // over in bytes, under in characters
	read, err := readOpenCodeDirectSQLite(context.Background(), root, base, limits)
	if err != nil || read.Skipped != 1 {
		t.Fatalf("read = %+v, %v, want the multibyte row skipped", read, err)
	}
	if len(read.Messages) != 2 || read.Messages[0].ID != "msg_bad" || read.Messages[0].Valid {
		t.Fatalf("messages = %+v, want the malformed row read as invalid", read.Messages)
	}
	if m := read.Messages[1]; m.ID != "msg_ok" || !m.Valid || m.Usage.Input != 7 {
		t.Fatalf("row after the malformed one = %+v", m)
	}
}

// OpenCode holding a write transaction does not block or fail the read.
func TestOpenCodeDirectSQLiteV2_ReadsWhileOpenCodeHoldsAWriteTransaction(t *testing.T) {
	root := t.TempDir()
	store := newOpenCodeV2TestStore(t, root)
	base := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC).UnixMilli()
	store.assistant("ses_a", "msg_a", base, base+1, 5, 0, 0, 0)
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO session_message VALUES ('msg_pending', 'ses_a', 9, 'assistant', ?, ?, '{"tokens":{"input":99},"time":{"completed":2}}')`, base+2, base+2); err != nil {
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

// One debt's turn: the window bounds it, a later idle row ends it when the
// window is gone, and a turn with no end reads nothing.
func TestOpenCodeStoreSessionUsage_ReadsOneTurn(t *testing.T) {
	prevEnabled := openCodeDirectScanEnabled
	openCodeDirectScanEnabled = true
	t.Cleanup(func() { openCodeDirectScanEnabled = prevEnabled })
	root := t.TempDir()
	store := newOpenCodeV2TestStore(t, root)
	base := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC).UnixMilli()
	store.assistant("ses_a", "msg_before", base-100, base-50, 1000, 0, 0, 0)
	store.assistant("ses_a", "msg_1", base+10, base+20, 30, 4, 0, 0.02)
	store.idle("ses_a", "idle_1", base+30)
	store.assistant("ses_a", "msg_2", base+100, base+110, 500, 0, 0, 0) // a later, resumed turn

	read, err := readOpenCodeStoreSessionUsage(context.Background(), root, openCodeStoreSessionQuery{SessionID: "ses_a", FromMs: base, ToMs: base + 30})
	if err != nil || read.Layout != openCodeStoreLayoutSQLiteV2 || len(read.Steps) != 1 {
		t.Fatalf("windowed read = %+v, %v", read, err)
	}
	if s := read.Steps[0]; s.Input != 30 || s.Output != 4 || s.AtMs != base+20 || s.Key != openCodeUsageStepKey("ses_a", "message:msg_1") {
		t.Fatalf("step = %+v, want msg_1 under the export's key", s)
	}

	read, err = readOpenCodeStoreSessionUsage(context.Background(), root, openCodeStoreSessionQuery{SessionID: "ses_a", FromMs: base, UntilIdle: true})
	if err != nil || len(read.Steps) != 1 || read.Steps[0].Input != 30 {
		t.Fatalf("idle-bounded read = %+v, %v, want the first turn only", read, err)
	}

	read, err = readOpenCodeStoreSessionUsage(context.Background(), root, openCodeStoreSessionQuery{SessionID: "ses_a", FromMs: base + 50, UntilIdle: true})
	if err != nil || !read.Unbounded || len(read.Steps) != 0 {
		t.Fatalf("unbounded read = %+v, %v, want nothing read", read, err)
	}
}
