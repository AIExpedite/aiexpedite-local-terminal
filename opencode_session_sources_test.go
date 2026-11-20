package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	ocV1    = "ses_v1aaaaaaaaaaaaaaaaaaaaaaaa"
	ocBoth  = "ses_bothaaaaaaaaaaaaaaaaaaaaaa"
	ocV2    = "ses_v2aaaaaaaaaaaaaaaaaaaaaaaa"
	ocNew   = "ses_newaaaaaaaaaaaaaaaaaaaaaaa"
	ocChild = "ses_childaaaaaaaaaaaaaaaaaaaaa"
	ocArch  = "ses_archaaaaaaaaaaaaaaaaaaaaaa"
	ocOld   = "ses_oldaaaaaaaaaaaaaaaaaaaaaaa"
	ocLeg   = "ses_legacyaaaaaaaaaaaaaaaaaaaa"
)

// openCodeTestRow is one session row.
type openCodeTestRow struct {
	id, title, dir, parent string
	updated                time.Time
	archived               bool
}

// writeOpenCodeDB writes opencode.db with the given tables (the columns
// OpenCode 1.x's `session` and 2's `session_v2` have, trimmed to the ones a
// reader could touch).
func writeOpenCodeDB(t *testing.T, root string, tables map[string][]openCodeTestRow) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, "opencode.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for table, rows := range tables {
		if _, err := db.Exec(`CREATE TABLE "` + table + `" (id TEXT PRIMARY KEY, project_id TEXT, parent_id TEXT, slug TEXT,
			directory TEXT, title TEXT, version TEXT, time_created INTEGER, time_updated INTEGER, time_archived INTEGER)`); err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			var parent, archived any
			if r.parent != "" {
				parent = r.parent
			}
			if r.archived {
				archived = r.updated.UnixMilli()
			}
			if _, err := db.Exec(`INSERT INTO "`+table+`" (id, project_id, parent_id, directory, title, time_created, time_updated, time_archived)
				VALUES (?, 'prj', ?, ?, ?, ?, ?, ?)`, r.id, parent, r.dir, r.title, r.updated.Add(-time.Hour).UnixMilli(), r.updated.UnixMilli(), archived); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// writeOpenCodeLegacySession writes storage/session/<project>/<id>.json.
func writeOpenCodeLegacySession(t *testing.T, root, id, title, dir string, updated time.Time) {
	t.Helper()
	p := filepath.Join(root, "storage", "session", "prj_legacy")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"id": id, "title": title, "directory": dir, "projectID": "prj_legacy",
		"time": map[string]any{"created": updated.Add(-time.Hour).UnixMilli(), "updated": updated.UnixMilli()}})
	if err := os.WriteFile(filepath.Join(p, id+".json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func openCodeFixture(t *testing.T) (root string, now time.Time) {
	t.Helper()
	root = t.TempDir()
	now = time.Now()
	v1 := []openCodeTestRow{
		{id: ocV1, title: "Only in the old table", dir: "/w/one", updated: now.Add(-5 * time.Hour)},
		{id: ocBoth, title: "Before the migration", dir: "/w/both", updated: now.Add(-6 * time.Hour)},
		{id: ocOld, title: "Too old", dir: "/w/old", updated: now.Add(-40 * 24 * time.Hour)},
	}
	v2 := []openCodeTestRow{
		{id: ocBoth, title: "Fix the session store", dir: "/w/both", updated: now.Add(-time.Hour)},
		{id: ocV2, title: "New session - 2026-11-19T09:00:00.000Z", dir: "/w/two", updated: now.Add(-2 * time.Hour)},
		{id: ocNew, title: "Ship the parser", dir: "/w/new", updated: now.Add(-3 * time.Hour)},
		{id: ocChild, title: "A child", dir: "/w/new", parent: ocNew, updated: now.Add(-3 * time.Hour)},
		{id: ocArch, title: "Archived", dir: "/w/new", archived: true, updated: now.Add(-3 * time.Hour)},
	}
	writeOpenCodeDB(t, root, map[string][]openCodeTestRow{"session": v1, "session_v2": v2})
	writeOpenCodeLegacySession(t, root, ocLeg, "From the JSON store", "/w/legacy", now.Add(-4*time.Hour))
	return root, now
}

// Both session tables and the legacy store are read and merged by id, the
// newest write winning: OpenCode 2's rows, the 1.x rows it never migrated,
// and the JSON store's. Placeholder titles are no title; child and archived
// sessions are not listed.
func TestOpenCodeDiscoveryMergesV1V2AndLegacy(t *testing.T) {
	root, _ := openCodeFixture(t)
	t.Setenv("OPENCODE_DATA", root)
	pinCliSessionProcesses(t, func() []cliSessionProcess { return nil })
	res := runCliSessionDiscover(context.Background(), openCodeSessionReader, newCliSessionDiscoverRequest(30, 500))
	order := []string{}
	byID := map[string]cliSessionRow{}
	for _, r := range res.Rows {
		order = append(order, r.SessionID)
		byID[r.SessionID] = r
	}
	if strings.Join(order, ",") != strings.Join([]string{ocBoth, ocV2, ocNew, ocLeg, ocV1}, ",") {
		t.Fatalf("rows = %v", order)
	}
	if b := byID[ocBoth]; b.Title != "Fix the session store" || b.TitleSource != cliSessionTitleSavedTitle || b.Cwd != "/w/both" || !b.Resumable || b.Engine != cliSessionEngineOpenCode {
		t.Fatalf("merged row = %+v, want OpenCode 2's newer row", b)
	}
	if v2 := byID[ocV2]; v2.Title != "" || v2.TitleSource != cliSessionTitleNone {
		t.Fatalf("placeholder = %+v, want no title", v2)
	}
	if l := byID[ocLeg]; l.Title != "From the JSON store" || l.Cwd != "/w/legacy" {
		t.Fatalf("legacy = %+v", l)
	}
	if res.Truncated || len(res.SourcesSkipped) != 0 || res.SourcesChecked[0] != cliSessionSourceOpenCodeStore {
		t.Fatalf("result = %+v", res)
	}
	short := discoverOpenCodeSessions(context.Background(), root, newCliSessionDiscoverRequest(30, 2))
	if len(short.Rows) != 2 || !short.Truncated {
		t.Fatalf("maxRows 2 = %+v", short)
	}
}

// maxRows counts the sessions listed: child and archived rows newer than a
// person's own session are read — a newer archived row still outranks the
// stale copy of itself in the 1.x table — but never take its place, and the
// answer is truncated only when a listable session is left over.
func TestOpenCodeDiscoveryCapsListedSessionsNotRows(t *testing.T) {
	pinCliSessionProcesses(t, func() []cliSessionProcess { return nil })
	now := time.Now()
	store := func(extra ...openCodeTestRow) string {
		root := t.TempDir()
		v2 := append([]openCodeTestRow{
			{id: ocChild, title: "A child", dir: "/w/new", parent: ocNew, updated: now.Add(-time.Minute)},
			{id: ocArch, title: "Archived", dir: "/w/new", archived: true, updated: now.Add(-2 * time.Minute)},
			{id: ocBoth, title: "Archived since the migration", dir: "/w/both", archived: true, updated: now.Add(-3 * time.Minute)},
			{id: ocNew, title: "Ship the parser", dir: "/w/new", updated: now.Add(-time.Hour)},
		}, extra...)
		v1 := []openCodeTestRow{{id: ocBoth, title: "Before the migration", dir: "/w/both", updated: now.Add(-6 * time.Hour)}}
		writeOpenCodeDB(t, root, map[string][]openCodeTestRow{"session": v1, "session_v2": v2})
		return root
	}

	one := discoverOpenCodeSessions(context.Background(), store(), newCliSessionDiscoverRequest(30, 1))
	if len(one.Rows) != 1 || one.Rows[0].SessionID != ocNew || one.Truncated {
		t.Fatalf("maxRows 1 = %+v, want the person's session past the newer child and archived rows, and complete", one)
	}
	older := store(openCodeTestRow{id: ocV2, title: "An older session", dir: "/w/two", updated: now.Add(-2 * time.Hour)})
	more := discoverOpenCodeSessions(context.Background(), older, newCliSessionDiscoverRequest(30, 1))
	if len(more.Rows) != 1 || more.Rows[0].SessionID != ocNew || !more.Truncated {
		t.Fatalf("maxRows 1 with a second session = %+v, want the newest, truncated", more)
	}
	both := discoverOpenCodeSessions(context.Background(), older, newCliSessionDiscoverRequest(30, 2))
	if len(both.Rows) != 2 || both.Rows[1].SessionID != ocV2 || both.Truncated {
		t.Fatalf("maxRows 2 = %+v, want both sessions, complete", both)
	}
}

// Each layout alone: a 1.x database, and a store that only has the JSON tree.
func TestOpenCodeDiscoveryReadsEitherLayoutAlone(t *testing.T) {
	now := time.Now()
	v1 := t.TempDir()
	writeOpenCodeDB(t, v1, map[string][]openCodeTestRow{"session": {{id: ocV1, title: "Only v1", dir: "/w/one", updated: now}}})
	if res := discoverOpenCodeSessions(context.Background(), v1, newCliSessionDiscoverRequest(30, 500)); len(res.Rows) != 1 || res.Rows[0].Title != "Only v1" {
		t.Fatalf("1.x store = %+v", res)
	}
	legacy := t.TempDir()
	writeOpenCodeLegacySession(t, legacy, ocLeg, "Only JSON", "/w/legacy", now)
	if res := discoverOpenCodeSessions(context.Background(), legacy, newCliSessionDiscoverRequest(30, 500)); len(res.Rows) != 1 || res.Rows[0].SessionID != ocLeg {
		t.Fatalf("JSON store = %+v", res)
	}
}

func TestOpenCodeDiscoveryReportsMissingAndUnrecognisedStores(t *testing.T) {
	req := newCliSessionDiscoverRequest(30, 500)
	if res := discoverOpenCodeSessions(context.Background(), filepath.Join(t.TempDir(), "absent"), req); skipReason(res, cliSessionSourceOpenCodeStore) != cliSessionSkipNotFound {
		t.Fatalf("no OpenCode data = %+v", res)
	}
	if res := discoverOpenCodeSessions(context.Background(), t.TempDir(), req); len(res.SourcesChecked) != 1 || len(res.SourcesSkipped) != 0 {
		t.Fatalf("a data dir with no store yet = %+v, want checked and empty", res)
	}
	odd := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(odd, "opencode.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE conversations (id TEXT, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if res := discoverOpenCodeSessions(context.Background(), odd, req); skipReason(res, cliSessionSourceOpenCodeStore) != cliSessionSkipUnrecognised {
		t.Fatalf("a database of neither family = %+v, want unrecognised", res)
	}
	// Beside a legacy tree, its sessions are listed and the answer still says
	// the database could not be read.
	writeOpenCodeLegacySession(t, odd, ocLeg, "From the JSON store", "/w/legacy", time.Now())
	res := discoverOpenCodeSessions(context.Background(), odd, req)
	if len(res.Rows) != 1 || skipReason(res, cliSessionSourceOpenCodeStore) != cliSessionSkipUnrecognised {
		t.Fatalf("legacy beside an unknown database = %+v, want its row and unrecognised", res)
	}
}

// The probe reads one session wherever it is kept; its time_updated is when
// it was last written, which the watcher reads as well.
func TestOpenCodeProbeAndWriterEvidence(t *testing.T) {
	root, now := openCodeFixture(t)
	t.Setenv("OPENCODE_DATA", root)
	pinCliSessionProcesses(t, func() []cliSessionProcess {
		return []cliSessionProcess{{PID: 91, Name: "opencode", Args: []string{"opencode", "--session", ocBoth}}}
	})
	res := probeOpenCodeSession(context.Background(), root, cliSessionProbeRequest{sessionID: ocBoth})
	if !res.Exists || !res.Resumable || res.Title != "Fix the session store" || len(res.Writers) != 1 || res.Writers[0].PID != 91 {
		t.Fatalf("probe = %+v", res)
	}
	if got := time.UnixMilli(res.TranscriptWrittenAt); got.Sub(now.Add(-time.Hour)).Abs() > time.Second {
		t.Fatalf("transcriptWrittenAt = %v, want time_updated", got)
	}
	if res := probeOpenCodeSession(context.Background(), root, cliSessionProbeRequest{sessionID: ocArch}); !res.Exists || res.Resumable {
		t.Fatalf("an archived session = %+v, want there but not resumable", res)
	}
	if res := probeOpenCodeSession(context.Background(), root, cliSessionProbeRequest{sessionID: ocLeg}); !res.Exists || res.Title != "From the JSON store" {
		t.Fatalf("a legacy session = %+v", res)
	}
	if res := probeOpenCodeSession(context.Background(), root, cliSessionProbeRequest{sessionID: "--session"}); res.Error != cliSessionErrBadRequest {
		t.Fatalf("a malformed id = %+v", res)
	}
	if got := openCodeWriterEvidence.storeClock(ocBoth)(); got/1000 != now.Add(-time.Hour).UnixMilli()/1000 {
		t.Fatalf("store clock = %d, want the row's time_updated", got)
	}
}

// A store the probe could not read in full never says a session is gone: a
// session no layout it read shows is an error (the cloud's "could not check
// that session"), while one a layout it read does show is still confirmed, and
// a store read in full still says gone. Covers a database the driver cannot
// read (a torn or unreadable file reads as not a database), one of neither
// family, one table's row that cannot be read beside the other table, and a
// probe out of time.
func TestOpenCodeProbeOfAStoreItCouldNotReadNeverSaysGone(t *testing.T) {
	pinCliSessionProcesses(t, func() []cliSessionProcess { return nil })
	probe := func(ctx context.Context, root, id string) cliSessionProbeResult {
		return probeOpenCodeSession(ctx, root, cliSessionProbeRequest{sessionID: id})
	}
	unknown := func(what string, res cliSessionProbeResult) {
		t.Helper()
		if res.Error != cliSessionErrInternal || res.Exists || res.Resumable {
			t.Fatalf("%s = %+v, want an error (not checked), never gone or resumable", what, res)
		}
	}

	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "opencode.db"), []byte(strings.Repeat("not a database ", 64)), 0o644); err != nil {
		t.Fatal(err)
	}
	unknown("a session in a database that could not be read", probe(context.Background(), broken, ocBoth))
	writeOpenCodeLegacySession(t, broken, ocLeg, "From the JSON store", "/w/legacy", time.Now())
	if res := probe(context.Background(), broken, ocLeg); res.Error != "" || !res.Exists || !res.Resumable || res.Title != "From the JSON store" {
		t.Fatalf("a legacy session beside a database that could not be read = %+v, want confirmed", res)
	}
	unknown("a session the legacy store does not hold, beside a database that could not be read", probe(context.Background(), broken, ocBoth))

	odd := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(odd, "opencode.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE conversations (id TEXT, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	unknown("a session in a database of neither family", probe(context.Background(), odd, ocBoth))

	// OpenCode 2's copy of two sessions cannot be read (a time that does not
	// scan); the frozen 1.x table holds only the older of them.
	now := time.Now()
	torn := t.TempDir()
	writeOpenCodeDB(t, torn, map[string][]openCodeTestRow{
		"session": {{id: ocBoth, title: "Before the migration", dir: "/w/both", updated: now.Add(-6 * time.Hour)}},
		"session_v2": {
			{id: ocBoth, title: "Fix the session store", dir: "/w/both", updated: now.Add(-time.Hour)},
			{id: ocNew, title: "Ship the parser", dir: "/w/new", updated: now.Add(-time.Hour)},
		},
	})
	db, err = sql.Open("sqlite", filepath.Join(torn, "opencode.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE session_v2 SET time_updated = 'not a time'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	unknown("a session whose only row could not be read", probe(context.Background(), torn, ocNew))
	if res := probe(context.Background(), torn, ocBoth); res.Error != "" || !res.Exists || res.Title != "Before the migration" {
		t.Fatalf("a session the other table holds = %+v, want confirmed from it", res)
	}
	// A listing still skips the row it cannot read, as it always has.
	if res := discoverOpenCodeSessions(context.Background(), torn, newCliSessionDiscoverRequest(30, 500)); len(res.Rows) != 1 || res.Rows[0].SessionID != ocBoth || len(res.SourcesSkipped) != 0 {
		t.Fatalf("discovery of the torn store = %+v, want the 1.x row alone", res)
	}

	root, _ := openCodeFixture(t)
	if res := probe(context.Background(), root, "ses_goneaaaaaaaaaaaaaaaaaaaaaa"); res.Error != "" || res.Exists || res.Resumable {
		t.Fatalf("a session no layout of a store read in full holds = %+v, want gone", res)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if res := probe(ctx, root, ocBoth); res.Error != cliSessionErrTimeout || res.Exists || res.Resumable {
		t.Fatalf("a probe out of time = %+v, want timeout, never gone", res)
	}
}

// A legacy store the probe could not read in full never says a session is
// gone either: a record of the session that would not read (its permissions,
// an I/O error) or is not one this build knows (torn, naming another
// session), a store folder that would not list, and a database that would
// not stat answer an error, which terminal-service reads as a session it
// could not check. A record of the session read in another project still
// confirms it, a session no record names is still gone, and a listing passes
// over a record it could not read, as it always has.
func TestOpenCodeProbeOfALegacyStoreItCouldNotReadNeverSaysGone(t *testing.T) {
	pinCliSessionProcesses(t, func() []cliSessionProcess { return nil })
	root, now := t.TempDir(), time.Now()
	writeOpenCodeLegacySession(t, root, ocLeg, "From the JSON store", "/w/legacy", now.Add(-time.Hour))
	writeOpenCodeLegacySession(t, root, ocBoth, "Kept in another project", "/w/both", now.Add(-2*time.Hour))
	const absent = "ses_goneaaaaaaaaaaaaaaaaaaaaaa"
	probe := func(id string) cliSessionProbeResult {
		return probeOpenCodeSession(context.Background(), root, cliSessionProbeRequest{sessionID: id})
	}
	unknown := func(t *testing.T, what string, res cliSessionProbeResult) {
		t.Helper()
		if res.Error != cliSessionErrInternal || res.Exists || res.Resumable {
			t.Fatalf("%s = %+v, want an error (not checked), never gone or resumable", what, res)
		}
	}
	gone := func(t *testing.T, what string, res cliSessionProbeResult) {
		t.Helper()
		if res.Error != "" || res.Exists || res.Resumable {
			t.Fatalf("%s = %+v, want gone", what, res)
		}
	}
	gone(t, "a session a legacy store read in full does not hold", probe(absent))

	sessions := filepath.Join(root, "storage", "session")
	put := func(project, id, body string) {
		t.Helper()
		dir := filepath.Join(sessions, project)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("prj_legacy", ocV1, `{"id":"`+ocV1+`","title":"torn`)
	unknown(t, "a session whose record is torn", probe(ocV1))
	put("prj_legacy", ocV2, `{"id":"`+ocLeg+`","title":"another session's record"}`)
	unknown(t, "a session whose record names another", probe(ocV2))
	// prj_a is read first: its torn copy does not hide the record prj_legacy holds.
	put("prj_a", ocBoth, `{"id":"`+ocBoth+`","ti`)
	if res := probe(ocBoth); res.Error != "" || !res.Exists || !res.Resumable || res.Title != "Kept in another project" {
		t.Fatalf("a session a readable record holds, beside a torn copy = %+v, want confirmed", res)
	}
	gone(t, "a session no record names, beside records that could not be read", probe(absent))
	res := discoverOpenCodeSessions(context.Background(), root, newCliSessionDiscoverRequest(30, 500))
	if len(res.Rows) != 2 || res.Rows[0].SessionID != ocLeg || res.Rows[1].SessionID != ocBoth || len(res.SourcesSkipped) != 0 {
		t.Fatalf("discovery = %+v, want the two readable records alone", res)
	}

	t.Run("a record it could not read", func(t *testing.T) {
		putUnreadableFile(t, filepath.Join(sessions, "prj_legacy", ocNew+".json"))
		unknown(t, "a session whose record could not be read", probe(ocNew))
	})
	t.Run("a store folder it could not list", func(t *testing.T) {
		makeUnlistable(t, sessions)
		unknown(t, "a session kept in a store folder that could not be listed", probe(ocLeg))
	})
	t.Run("a database it could not stat", func(t *testing.T) {
		putUnstatableFile(t, filepath.Join(root, "opencode.db"))
		unknown(t, "a session no record names, beside a database that could not be stat-ed", probe(absent))
		if res := probe(ocLeg); res.Error != "" || !res.Exists || !res.Resumable || res.Title != "From the JSON store" {
			t.Fatalf("a legacy session beside a database that could not be stat-ed = %+v, want confirmed", res)
		}
	})
}

// The watcher's busy decision reads OpenCode's store through the owner's live
// write-ahead log (cliSessionSQLiteDSN): while OpenCode runs, a turn's write
// its log alone holds yet is a writer mid-turn. An immutable open would read
// the store file without it and call a busy session idle, releasing a held
// message beside its writer. Once OpenCode closes (its last connection
// checkpoints the write and removes the log) the store is read immutable,
// still says when the session was last written, and gains nothing beside it.
func TestOpenCodeWatcherReadsAWriteStillOnlyInTheOwnersLog(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OPENCODE_DATA", root)
	pinCliSessionProcesses(t, func() []cliSessionProcess {
		return []cliSessionProcess{{PID: 91, Name: "opencode", Args: []string{"opencode", "--session", ocBoth}}}
	})
	path := filepath.Join(root, "opencode.db")
	owner, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	owner.SetMaxOpenConns(1)
	defer owner.Close()
	exec := func(stmt string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(stmt, args...); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	hourAgo := time.Now().Add(-time.Hour).UnixMilli()
	exec(`PRAGMA journal_mode=WAL`)
	exec(`CREATE TABLE session_v2 (id TEXT PRIMARY KEY, title TEXT, directory TEXT, time_created INTEGER, time_updated INTEGER)`)
	exec(`INSERT INTO session_v2 VALUES (?, 'Fix the session store', '/w/app', ?, ?)`, ocBoth, hourAgo, hourAgo)
	// Everything so far is in the store file; from here on only the log holds
	// a write, until the owner closes.
	exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	exec(`PRAGMA wal_autocheckpoint=0`)

	clock := openCodeWriterEvidence.storeClock(ocBoth)
	stateNow := func() string {
		state, _, _ := claudeOutsideWriterState(cliSessionOutsideWriters(openCodeWriterEvidence, ocBoth, clock, nil, time.Now()))
		return state
	}
	if got := stateNow(); got != claudeOutsideWriterIdle {
		t.Fatalf("a session OpenCode holds open, last written an hour ago, reads %s, want idle", got)
	}

	turnAt := time.Now().UnixMilli()
	exec(`UPDATE session_v2 SET time_updated = ? WHERE id = ?`, turnAt, ocBoth)
	if !cliSessionSQLiteWALLive(path) {
		t.Fatalf("OpenCode's log is not live while it runs: %v", sqliteSideFiles(t, root))
	}
	if got := clock(); got != turnAt {
		t.Fatalf("store clock = %d, want the write only the owner's log holds (%d)", got, turnAt)
	}
	if got := stateNow(); got != claudeOutsideWriterBusy {
		t.Fatalf("a session OpenCode is writing reads %s, want busy", got)
	}

	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if got := sqliteSideFiles(t, root); len(got) != 1 {
		t.Fatalf("after OpenCode closed its folder holds %v, want the store alone", got)
	}
	if got := clock(); got != turnAt {
		t.Fatalf("store clock after OpenCode closed = %d, want the checkpointed write (%d)", got, turnAt)
	}
	if got := sqliteSideFiles(t, root); len(got) != 1 {
		t.Fatalf("the immutable read left %v beside the store", got)
	}
}
