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
// Read-only (mode=ro, query_only).
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
	command: "opencode",
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
			if s, ok := readOpenCodeSession(ctx, root, sessionID); ok {
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

// openCodeSQLiteSessions reads the session tables of opencode.db: those
// updated at or after sinceMs, newest first, at most limit per table (merged
// by id). recognised is false when the database has neither table in a
// shape this build knows.
func openCodeSQLiteSessions(ctx context.Context, dbPath string, sinceMs int64, limit int, onlyID string) (sessions map[string]openCodeSession, recognised bool, err error) {
	db, err := openCliSessionSQLite(ctx, dbPath)
	if err != nil {
		return nil, false, err
	}
	defer db.Close()
	sessions = map[string]openCodeSession{}
	for _, table := range openCodeSessionTables {
		cols := sqliteColumns(ctx, db, table)
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
			args = append(args, sinceMs, limit)
		}
		rows, qerr := db.QueryContext(ctx, query, args...)
		if qerr != nil {
			if ctx.Err() != nil {
				return sessions, recognised, ctx.Err()
			}
			continue
		}
		for rows.Next() {
			var s openCodeSession
			if rows.Scan(&s.id, &s.title, &s.directory, &s.created, &s.updated, &s.child, &s.archived) != nil || !isOpenCodeSessionID(s.id) {
				continue
			}
			if prev, seen := sessions[s.id]; !seen || s.updated > prev.updated {
				sessions[s.id] = s
			}
		}
		rerr := rows.Err()
		rows.Close()
		if rerr != nil && ctx.Err() != nil {
			return sessions, recognised, ctx.Err()
		}
	}
	return sessions, recognised, nil
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

// readOpenCodeSession is one session from whichever layout holds it.
func readOpenCodeSession(ctx context.Context, root, id string) (openCodeSession, bool) {
	if root == "" || !isOpenCodeSessionID(id) {
		return openCodeSession{}, false
	}
	var best openCodeSession
	found := false
	if dbPath := filepath.Join(root, "opencode.db"); fileExists(dbPath) {
		if rows, _, err := openCodeSQLiteSessions(ctx, dbPath, 0, 1, id); err == nil {
			if s, ok := rows[id]; ok {
				best, found = s, true
			}
		}
	}
	legacy, _, _ := openCodeLegacySessions(ctx, root, id)
	if s, ok := legacy[id]; ok && (!found || s.updated > best.updated) {
		best, found = s, true
	}
	return best, found
}

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
	haveDB, recognised, timedOut := fileExists(dbPath), false, false
	if haveDB {
		// One past the cap tells a store holding more sessions than maxRows.
		rows, ok, err := openCodeSQLiteSessions(ctx, dbPath, since, req.maxRows+1, "")
		switch {
		case err != nil && ctx.Err() != nil:
			timedOut = true
			recognised = ok
			add(rows)
		case err != nil:
			res.skip(src, cliSessionSkipUnreadable)
			return res
		default:
			recognised = ok
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
	if len(sessions) > req.maxRows {
		sessions = sessions[:req.maxRows]
		res.Truncated = true
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

// probeOpenCodeSession answers the probe of one session.
func probeOpenCodeSession(ctx context.Context, root string, req cliSessionProbeRequest) cliSessionProbeResult {
	res := cliSessionProbeResult{SessionID: req.sessionID, Writers: []cliSessionWriter{}, OpenInEditors: []string{}, TitleSource: cliSessionTitleNone}
	if !isOpenCodeSessionID(req.sessionID) {
		res.Error = cliSessionErrBadRequest
		return res
	}
	res.Writers = cliSessionProcessWriters(openCodeWriterEvidence, req.sessionID)
	if s, ok := readOpenCodeSession(ctx, root, req.sessionID); ok {
		res.Exists = true
		res.Resumable = !s.archived
		res.TranscriptWrittenAt = s.updated
		res.Title, res.TitleSource, _ = openCodeRowTitle(s)
	}
	res.Exists = res.Exists || len(res.Writers) > 0
	return res
}
