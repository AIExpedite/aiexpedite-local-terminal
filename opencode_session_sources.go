// opencode_session_sources.go — OpenCode's sessions, for discovery and the
// adoption probe (cli_session_discovery.go), and the writer evidence of a
// session AI Expedite continues (cli_session_outside_writer.go).
//
// OpenCode keeps them under openCodeStorageDir() ($OPENCODE_DATA, else
// $XDG_DATA_HOME/opencode, else ~/.local/share/opencode), in either layout
// openCodeStoreLayout() tells apart:
//
//	opencode.db               SQLite: `session` (OpenCode 1.x) and
//	                          `session_v2` (OpenCode 2, which migrated every
//	                          1.x row into it and left `session` frozen), each
//	                          (id "ses_…", title, directory, time_created,
//	                          time_updated ms, parent_id, time_archived, …)
//	storage/session/<project>/<ses_…>.json
//	                          the legacy JSON store: {"id","title","directory",
//	                          "parentID","time":{"created","updated"}}
//
// Checked against an OpenCode 2 store (both tables present, v2 a superset
// with the same times for migrated rows) and the readers in
// cliagent_usage_opencode_direct_*.go; the legacy layout against
// opencode_native.go's openCodeSessionDirs and OpenCode's own legacy
// format. Both tables and the legacy store are read and merged by id (the
// newest time_updated wins), so neither family can hide the other's
// sessions. A child session (parent_id) or an archived one is not listed.
// OpenCode's own placeholder title ("New session - <date>") is no title.
// The probe of a session a store it could not read in full does not show (a
// database that would not open or parse, or of neither family) is an error,
// never "gone". Read-only: opencode.db through cliSessionSQLiteDSN (nothing
// created or locked).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	// openCodeLegacySessionMaxBytes bounds one legacy session record.
	openCodeLegacySessionMaxBytes = 256 * 1024
	// openCodeLegacyMaxFiles bounds the legacy listing.
	openCodeLegacyMaxFiles = 20000
	// openCodeSQLiteMaxRows bounds the rows one session table is read for.
	openCodeSQLiteMaxRows = 20000
)

// openCodeStoredSessionIDPattern is the id shape of a session OpenCode keeps
// (`ses_` and its base62 tail): stricter than isValidOpenCodeSessionID's
// argv check, and never one that could walk a path or be a flag.
var openCodeStoredSessionIDPattern = regexp.MustCompile(`^ses_[A-Za-z0-9]{8,64}$`)

func isOpenCodeSessionID(id string) bool { return openCodeStoredSessionIDPattern.MatchString(id) }

// openCodePlaceholderTitle is the title OpenCode gives a session before it
// names one ("New session - 2026-11-19T10:00:00.000Z", "Child session - …").
var openCodePlaceholderTitle = regexp.MustCompile(`^(New|Child) session - \d{4}-\d{2}-\d{2}T`)

// openCodeSessionTables are the session tables, OpenCode 2's first.
var openCodeSessionTables = []string{"session_v2", "session"}

var openCodeSessionReader = &cliSessionReader{
	engine:  cliSessionEngineOpenCode,
	sources: []string{cliSessionSourceOpenCodeStore},
	discover: func(ctx context.Context, req cliSessionDiscoverRequest) cliSessionsDiscoverResult {
		return discoverOpenCodeSessions(ctx, openCodeStorageDir(), req)
	},
	probe: func(ctx context.Context, req cliSessionProbeRequest) cliSessionProbeResult {
		return probeOpenCodeSession(ctx, openCodeStorageDir(), req)
	},
}

// openCodeWriterEvidence: a live `opencode` naming the session
// (`opencode --session ses_…`) holds it open; its time_updated within
// writerQuietMs is a writer mid-turn.
var openCodeWriterEvidence = cliSessionWriterEvidence{
	validID:        isOpenCodeSessionID,
	isAgentProcess: cliSessionNameIn("opencode"),
	storeClock: func(sessionID string) func() int64 {
		root := openCodeStorageDir()
		return func() int64 {
			ctx, cancel := context.WithTimeout(context.Background(), cliSessionProcessScanBudget)
			defer cancel()
			// Not found reads 0 (unknown) whatever the reason, which
			// lastGoodStoreClock answers with the last good time.
			if s, ok, _ := readOpenCodeSession(ctx, root, sessionID); ok {
				return s.updated
			}
			return 0
		}
	},
}

// openCodeSession is one session as either layout records it.
type openCodeSession struct {
	id, title, directory string
	created, updated     int64
	child, archived      bool
}

// openCodeSQLiteSessions reads the session tables of opencode.db, merged by
// id: the session onlyID names, else those updated at or after sinceMs,
// newest first. Each table is read until it has given `listable` sessions
// discovery lists (neither a child nor archived; 0: no such cap), or
// openCodeSQLiteMaxRows rows. A child or an archived row is read as well — a
// newer one outranks a stale copy of the same session in the other table or
// the JSON store — but never counts toward the cap in a listed session's
// place. cut is true when a table was left unread past its cap. recognised is
// false when the database has neither table in a shape this build knows. err
// is a database that would not open, or ctx ending. In the read of one session
// (onlyID) it is also the first table, or row, that could not be read: that
// may be where the session is kept, so its absence from what was read proves
// nothing (the other table is still read, and a row it gives still counts). A
// listing skips such a table or row, as it always has.
func openCodeSQLiteSessions(ctx context.Context, dbPath string, sinceMs int64, listable int, onlyID string) (sessions map[string]openCodeSession, recognised, cut bool, err error) {
	db, err := openCliSessionSQLite(ctx, dbPath)
	if err != nil {
		return nil, false, false, err
	}
	defer db.Close()
	sessions = map[string]openCodeSession{}
	var unread error
	noteUnread := func(e error) {
		if onlyID != "" && unread == nil {
			unread = e
		}
	}
	for _, table := range openCodeSessionTables {
		// An absent table reads as no columns and no error; an error is a
		// table that may be there but could not be read.
		cols, cerr := sqliteTableColumns(ctx, db, table)
		if cerr != nil {
			if ctx.Err() != nil {
				return sessions, recognised, cut, ctx.Err()
			}
			noteUnread(cerr)
		}
		if !hasAll(cols, "id", "title", "directory", "time_updated") {
			continue
		}
		recognised = true
		col := func(name, fallback string) string {
			if cols[name] {
				return name
			}
			return fallback
		}
		query := fmt.Sprintf(`SELECT id, COALESCE(title, ''), COALESCE(directory, ''), COALESCE(%s, 0), COALESCE(time_updated, 0),
			%s, %s FROM %q`,
			col("time_created", "0"),
			openCodeNotEmpty(cols, "parent_id"), openCodeNotEmpty(cols, "time_archived"), table)
		args := []any{}
		if onlyID != "" {
			query += ` WHERE id = ?`
			args = append(args, onlyID)
		} else {
			query += ` WHERE time_updated >= ? ORDER BY time_updated DESC, id LIMIT ?`
			args = append(args, sinceMs, openCodeSQLiteMaxRows)
		}
		rows, qerr := db.QueryContext(ctx, query, args...)
		if qerr != nil {
			if ctx.Err() != nil {
				return sessions, recognised, cut, ctx.Err()
			}
			noteUnread(qerr)
			continue
		}
		read, listed := 0, 0
		for rows.Next() {
			read++
			var s openCodeSession
			if serr := rows.Scan(&s.id, &s.title, &s.directory, &s.created, &s.updated, &s.child, &s.archived); serr != nil {
				noteUnread(serr)
				continue
			}
			if !isOpenCodeSessionID(s.id) {
				continue
			}
			if prev, seen := sessions[s.id]; !seen || s.updated > prev.updated {
				sessions[s.id] = s
			}
			if s.child || s.archived {
				continue
			}
			listed++
			if listable > 0 && listed >= listable {
				cut = true
				break
			}
		}
		rerr := rows.Err()
		rows.Close()
		if rerr != nil && ctx.Err() != nil {
			return sessions, recognised, cut, ctx.Err()
		}
		if rerr != nil {
			noteUnread(rerr)
		}
		if onlyID == "" && read >= openCodeSQLiteMaxRows {
			cut = true
		}
	}
	return sessions, recognised, cut, unread
}

// openCodeNotEmpty is an SQL truth value for "column holds a value", or 0
// when the table has no such column.
func openCodeNotEmpty(cols map[string]bool, name string) string {
	if !cols[name] {
		return "0"
	}
	return fmt.Sprintf("(%s IS NOT NULL AND %s <> '' AND %s <> 0)", name, name, name)
}

// openCodeLegacySessions reads storage/session/<project>/<ses_…>.json (and
// the older storage/session/info/ spelling). complete is false when ctx ended
// or the listing cap was reached first.
func openCodeLegacySessions(ctx context.Context, root string, onlyID string) (sessions map[string]openCodeSession, found bool, complete bool) {
	sessions = map[string]openCodeSession{}
	base := filepath.Join(root, "storage", "session")
	projects, err := os.ReadDir(base)
	if err != nil {
		return sessions, false, true
	}
	found, complete = true, true
	files := 0
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		dir := filepath.Join(base, p.Name())
		var names []string
		if onlyID != "" {
			names = []string{onlyID + ".json"}
		} else {
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				names = append(names, e.Name())
			}
		}
		for _, name := range names {
			if ctx.Err() != nil || files >= openCodeLegacyMaxFiles {
				return sessions, found, false
			}
			id, ok := strings.CutSuffix(name, ".json")
			if !ok || !isOpenCodeSessionID(id) {
				continue
			}
			files++
			var rec struct {
				ID        string `json:"id"`
				Title     string `json:"title"`
				Directory string `json:"directory"`
				ParentID  string `json:"parentID"`
				Time      struct {
					Created  float64 `json:"created"`
					Updated  float64 `json:"updated"`
					Archived float64 `json:"archived"`
				} `json:"time"`
			}
			path := filepath.Join(dir, name)
			if !readJSONFileWithin(path, openCodeLegacySessionMaxBytes, &rec) || rec.ID != id {
				continue
			}
			s := openCodeSession{
				id: id, title: rec.Title, directory: rec.Directory,
				created: cliSessionEpochMs(rec.Time.Created), updated: cliSessionEpochMs(rec.Time.Updated),
				child: rec.ParentID != "", archived: rec.Time.Archived > 0,
			}
			if s.updated == 0 {
				s.updated = newestFileMs(path)
			}
			if prev, seen := sessions[id]; !seen || s.updated > prev.updated {
				sessions[id] = s
			}
		}
	}
	return sessions, found, complete
}

// readOpenCodeSession is one session from whichever layout holds it. err says
// a layout that could hold it is on disk but was not read in full for it: a
// database that would not open or read (its permissions, an I/O error, a file
// the driver cannot parse) or is of neither family, or a read that ctx or the
// legacy listing's cap cut short. A session such a layout does not show may
// still be kept there, so not found then proves nothing: the probe answers
// that it could not check, never that the session is gone.
func readOpenCodeSession(ctx context.Context, root, id string) (best openCodeSession, found bool, err error) {
	if root == "" || !isOpenCodeSessionID(id) {
		return openCodeSession{}, false, nil
	}
	if dbPath := filepath.Join(root, "opencode.db"); fileExists(dbPath) {
		rows, recognised, _, dbErr := openCodeSQLiteSessions(ctx, dbPath, 0, 0, id)
		if s, ok := rows[id]; ok {
			best, found = s, true
		}
		switch {
		case dbErr != nil:
			err = dbErr
		case !recognised:
			err = errCliSessionUnrecognised
		}
	}
	legacy, _, complete := openCodeLegacySessions(ctx, root, id)
	if s, ok := legacy[id]; ok && (!found || s.updated > best.updated) {
		best, found = s, true
	}
	if !complete && err == nil {
		err = ctx.Err()
		if err == nil {
			err = errOpenCodeLegacyListingCut
		}
	}
	return best, found, err
}

// errOpenCodeLegacyListingCut: the legacy store held more than
// openCodeLegacyMaxFiles to look through, so it was not read in full.
var errOpenCodeLegacyListingCut = errors.New("opencode legacy session store not read in full")

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// openCodeRowTitle is a session's title: the one OpenCode shows, unless it is
// still OpenCode's placeholder.
func openCodeRowTitle(s openCodeSession) (string, string, int64) {
	title := strings.TrimSpace(s.title)
	if openCodePlaceholderTitle.MatchString(title) {
		title = ""
	}
	return cliSessionTitleOf(title, s.updated, "", "")
}

// discoverOpenCodeSessions lists the sessions updated within the request's
// look-back, newest first.
func discoverOpenCodeSessions(ctx context.Context, root string, req cliSessionDiscoverRequest) cliSessionsDiscoverResult {
	res := newCliSessionsResult(cliSessionEngineOpenCode, req.now)
	src := cliSessionSourceOpenCodeStore
	if root == "" {
		res.skip(src, cliSessionSkipNotFound)
		return res
	}
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			res.skip(src, cliSessionSkipNotFound)
		} else {
			res.skip(src, cliSessionSkipUnreadable)
		}
		return res
	}
	since := req.since().UnixMilli()
	merged := map[string]openCodeSession{}
	add := func(rows map[string]openCodeSession) {
		for id, s := range rows {
			if prev, seen := merged[id]; !seen || s.updated > prev.updated {
				merged[id] = s
			}
		}
	}
	dbPath := filepath.Join(root, "opencode.db")
	haveDB, recognised, timedOut, capped := fileExists(dbPath), false, false, false
	if haveDB {
		// One listable session past the cap tells a store holding more of them
		// than maxRows.
		rows, ok, cut, err := openCodeSQLiteSessions(ctx, dbPath, since, req.maxRows+1, "")
		switch {
		case err != nil && ctx.Err() != nil:
			timedOut = true
			recognised = ok
			add(rows)
		case err != nil:
			res.skip(src, cliSessionSkipUnreadable)
			return res
		default:
			recognised, capped = ok, cut
			add(rows)
		}
	}
	legacy, haveLegacy, complete := openCodeLegacySessions(ctx, root, "")
	add(legacy)
	if !complete {
		timedOut = true
	}
	if !haveDB && !haveLegacy {
		res.SourcesChecked = append(res.SourcesChecked, src) // OpenCode has kept no session yet
		return res
	}
	if haveDB && !recognised {
		// A database of neither family: whatever the legacy tree holds is
		// listed, and the answer says the newest sessions may be missing.
		res.skip(src, cliSessionSkipUnrecognised)
		if !haveLegacy {
			return res
		}
	}
	res.SourcesChecked = append(res.SourcesChecked, src)
	running := cliSessionRunningByID(openCodeWriterEvidence)
	sessions := make([]openCodeSession, 0, len(merged))
	for _, s := range merged {
		if s.updated >= since && !s.child && !s.archived {
			sessions = append(sessions, s)
		}
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].updated != sessions[j].updated {
			return sessions[i].updated > sessions[j].updated
		}
		return sessions[i].id < sessions[j].id
	})
	// A table read that stopped at its cap may have left sessions unread.
	if len(sessions) > req.maxRows || capped {
		res.Truncated = true
	}
	if len(sessions) > req.maxRows {
		sessions = sessions[:req.maxRows]
	}
	for _, s := range sessions {
		row := cliSessionRow{
			SessionID:    s.id,
			Cwd:          s.directory,
			LastActiveAt: s.updated,
			Editors:      []cliSessionEditorRef{},
			Resumable:    true,
			Running:      cliSessionRowRunning(running[s.id], s.updated, req.now),
		}
		row.Title, row.TitleSource, row.TitleSavedAt = openCodeRowTitle(s)
		res.Rows = append(res.Rows, row)
	}
	if timedOut {
		res.Truncated = true
		res.skip(src, cliSessionSkipTimeout)
	}
	return res
}

// probeOpenCodeSession answers the probe of one session: found is there (and
// resumable unless archived), and not found in a store read in full is gone.
// Not found in a store that could not be read in full (readOpenCodeSession's
// error) says so instead: whether the session exists is unknown, so the
// answer is an error, which terminal-service reads as a session it could not
// check, never as one that is gone.
func probeOpenCodeSession(ctx context.Context, root string, req cliSessionProbeRequest) cliSessionProbeResult {
	res := cliSessionProbeResult{SessionID: req.sessionID, Writers: []cliSessionWriter{}, OpenInEditors: []string{}, TitleSource: cliSessionTitleNone}
	if !isOpenCodeSessionID(req.sessionID) {
		res.Error = cliSessionErrBadRequest
		return res
	}
	res.Writers = cliSessionProcessWriters(openCodeWriterEvidence, req.sessionID)
	s, found, err := readOpenCodeSession(ctx, root, req.sessionID)
	switch {
	case found:
		res.Exists = true
		res.Resumable = !s.archived
		res.TranscriptWrittenAt = s.updated
		res.Title, res.TitleSource, _ = openCodeRowTitle(s)
	case err != nil:
		res.Error = cliSessionErrInternal
		if ctx.Err() != nil {
			res.Error = cliSessionErrTimeout
		}
		return res
	}
	res.Exists = res.Exists || len(res.Writers) > 0
	return res
}
