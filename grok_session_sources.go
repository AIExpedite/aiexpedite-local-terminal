// grok_session_sources.go — Grok Build's sessions, for discovery and the
// adoption probe (cli_session_discovery.go), and the writer evidence of a
// session AI Expedite loads (cli_session_outside_writer.go).
//
// Grok keeps every conversation under the person's own GROK_HOME
// (grokPersistentHome: $GROK_HOME, else ~/.grok):
//
//	sessions/<cwd, percent-encoded>/<session uuid>/  updates.jsonl, summary.json, …
//	sessions/session_search.sqlite                   session_docs(session_id, cwd, updated_at, title, …)
//
// Checked against the store Grok Build writes (and
// GROK_CONVERSATION_STORE_AND_RESUME.md): a session folder holds
// summary.json ({"info":{"id","cwd"}, "session_summary" — the title Grok
// shows, the same text as session_docs.title — "created_at", "updated_at",
// "last_active_at"}) and updates.jsonl, one ACP `session/update` per line
// ({"method", "params":{"sessionId", "update":{"sessionUpdate":
// "user_message_chunk", "content":{"type":"text","text"}}}, "timestamp"}).
// A title Grok made from the start of the first prompt reads as that prompt
// (cliSessionTitleOf).
//
// AI Expedite's own Grok sessions are NOT here: they live in the per-device
// store its isolated homes share (grok_session_store.go), so everything this
// reader lists was started outside AI Expedite. Loading one of them needs its
// folder linked into that store first (linkAdoptedGrokSession). The probe of
// a session whose folder could not be read, or that a store it could not
// search in full does not show, is an error, never "gone". Read-only.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// grokSummaryMaxBytes bounds one session's summary.json.
	grokSummaryMaxBytes = 1 << 20
	// updates.jsonl is read up to this much / these many records for the
	// first prompt.
	grokUpdatesHeadMaxBytes = 512 * 1024
	grokUpdatesHeadMaxLines = 64
	// grokFirstPromptMaxBytes: a first prompt split into chunks is joined up
	// to this much (a label needs eighty characters).
	grokFirstPromptMaxBytes = 4096
	// grokFolderSearchEvery: a watcher that has not found its session's folder
	// looks again this often.
	grokFolderSearchEvery = 30 * time.Second
)

// grokSessionFiles are the files of a session folder Grok writes during a
// turn: the newest of them is when the session was last written.
var grokSessionFiles = []string{"updates.jsonl", "chat_history.jsonl", "events.jsonl", "summary.json"}

var grokSessionReader = &cliSessionReader{
	engine:  cliSessionEngineGrok,
	sources: []string{cliSessionSourceGrokStore},
	discover: func(ctx context.Context, req cliSessionDiscoverRequest) cliSessionsDiscoverResult {
		return discoverGrokSessions(ctx, grokPersistentHome(), req)
	},
	probe: func(ctx context.Context, req cliSessionProbeRequest) cliSessionProbeResult {
		return probeGrokSession(ctx, grokPersistentHome(), req)
	},
}

// grokWriterEvidence: a live `grok` naming the session holds it open; its
// folder written within writerQuietMs is a writer mid-turn.
var grokWriterEvidence = cliSessionWriterEvidence{
	validID:        isCliSessionUUID,
	isAgentProcess: cliSessionNameIn("grok"),
	storeClock: func(sessionID string) func() int64 {
		home := grokPersistentHome()
		folder, searched := "", time.Time{}
		return func() int64 {
			if folder == "" && time.Since(searched) >= grokFolderSearchEvery {
				searched = time.Now()
				folder = findGrokSessionFolder(home, sessionID, "")
			}
			return grokFolderWrittenAt(folder)
		}
	},
}

// grokFolderWrittenAt is the newest write among a session folder's files.
func grokFolderWrittenAt(folder string) int64 {
	if folder == "" {
		return 0
	}
	paths := make([]string, 0, len(grokSessionFiles))
	for _, f := range grokSessionFiles {
		paths = append(paths, filepath.Join(folder, f))
	}
	return newestFileMs(paths...)
}

// grokSessionFolder is one session folder the listing found.
type grokSessionFolder struct {
	sessionID string
	path      string
	// cwd is the folder name's decoded path ("" when it does not decode).
	cwd      string
	modified int64
}

// listGrokSessionFolders lists every session folder under root. complete is
// false when ctx ended first.
func listGrokSessionFolders(ctx context.Context, root string) (folders []grokSessionFolder, complete bool, err error) {
	cwdDirs, err := os.ReadDir(root)
	if err != nil {
		return nil, false, err
	}
	for _, d := range cwdDirs {
		if ctx.Err() != nil {
			return folders, false, nil
		}
		if !d.IsDir() {
			continue // session_search.sqlite and its journals
		}
		cwd, _ := url.PathUnescape(d.Name())
		if !filepath.IsAbs(cwd) {
			cwd = ""
		}
		sessions, _ := os.ReadDir(filepath.Join(root, d.Name()))
		for _, s := range sessions {
			if !s.IsDir() || !isCliSessionUUID(s.Name()) {
				continue
			}
			path := filepath.Join(root, d.Name(), s.Name())
			at := grokFolderWrittenAt(path)
			if at == 0 {
				at = newestModTime(path).UnixMilli()
			}
			folders = append(folders, grokSessionFolder{sessionID: s.Name(), path: path, cwd: cwd, modified: at})
		}
	}
	return folders, ctx.Err() == nil, nil
}

// grokSessionDoc is a session's row of session_search.sqlite.
type grokSessionDoc struct {
	cwd       string
	title     string
	updatedAt int64
}

// readGrokSessionDocs reads session_search.sqlite's titles (none when the
// index is missing or its shape unknown: summary.json holds the same title).
func readGrokSessionDocs(ctx context.Context, root string) map[string]grokSessionDoc {
	out := map[string]grokSessionDoc{}
	path := filepath.Join(root, "session_search.sqlite")
	if _, err := os.Stat(path); err != nil {
		return out
	}
	db, err := openCliSessionSQLite(ctx, path)
	if err != nil {
		return out
	}
	defer db.Close()
	if !hasAll(sqliteColumns(ctx, db, "session_docs"), "session_id", "cwd", "updated_at", "title") {
		return out
	}
	rows, err := db.QueryContext(ctx, `SELECT session_id, cwd, updated_at, title FROM session_docs LIMIT 20000`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, cwd, title string
		var updated float64
		if rows.Scan(&id, &cwd, &updated, &title) != nil || !isCliSessionUUID(id) {
			continue
		}
		out[id] = grokSessionDoc{cwd: cwd, title: title, updatedAt: cliSessionEpochMs(updated)}
	}
	return out
}

// grokSessionInfo is what a session folder says about itself.
type grokSessionInfo struct {
	id          string
	cwd         string
	title       string
	firstPrompt string
	resumable   bool
}

// readGrokSessionFolder reads a folder's summary.json and its first prompt.
// errCliSessionUnrecognised when neither names this session; updates.jsonl's
// read error when it is there but could not be read (its permissions, an I/O
// error) and named no session before the error, since it is what says
// whether the session loads: that is then unknown, never no.
func readGrokSessionFolder(folder, sessionID string) (grokSessionInfo, error) {
	info := grokSessionInfo{id: sessionID}
	var summary struct {
		Info struct {
			ID  string `json:"id"`
			Cwd string `json:"cwd"`
		} `json:"info"`
		SessionSummary string `json:"session_summary"`
	}
	summaryOK := readJSONFileWithin(filepath.Join(folder, "summary.json"), grokSummaryMaxBytes, &summary) && summary.Info.ID == sessionID
	if summaryOK {
		info.cwd, info.title = summary.Info.Cwd, strings.TrimSpace(summary.SessionSummary)
	}
	updates := filepath.Join(folder, "updates.jsonl")
	updatesOK := false
	var prompt strings.Builder
	err := readJSONLHead(updates, grokUpdatesHeadMaxBytes, grokUpdatesHeadMaxLines, func(line []byte) bool {
		var rec struct {
			Params struct {
				SessionID string `json:"sessionId"`
				Update    struct {
					SessionUpdate string `json:"sessionUpdate"`
					Content       struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"update"`
			} `json:"params"`
		}
		if json.Unmarshal(line, &rec) != nil || rec.Params.SessionID != sessionID {
			return true
		}
		updatesOK = true
		if rec.Params.Update.SessionUpdate != "user_message_chunk" {
			// The first prompt ends where the agent's first update starts.
			return prompt.Len() == 0
		}
		if rec.Params.Update.Content.Type == "text" && prompt.Len() < grokFirstPromptMaxBytes {
			prompt.WriteString(rec.Params.Update.Content.Text)
		}
		return true
	})
	if !updatesOK && err != nil && !errors.Is(err, os.ErrNotExist) {
		return info, err
	}
	if !summaryOK && !updatesOK {
		return info, errCliSessionUnrecognised
	}
	if p := strings.TrimSpace(prompt.String()); p != "" && !cliSessionSyntheticPrompt(p) {
		info.firstPrompt = p
	}
	info.resumable = updatesOK
	return info, nil
}

// grokRowTitle is a session's title: the one Grok shows, unless it is only
// the start of the first prompt.
func grokRowTitle(info grokSessionInfo, doc grokSessionDoc) (string, string, int64) {
	saved := info.title
	if saved == "" {
		saved = strings.TrimSpace(doc.title)
	}
	return cliSessionTitleOf(saved, doc.updatedAt, "", info.firstPrompt)
}

// discoverGrokSessions lists the sessions written within the request's
// look-back, newest first.
func discoverGrokSessions(ctx context.Context, home string, req cliSessionDiscoverRequest) cliSessionsDiscoverResult {
	res := newCliSessionsResult(cliSessionEngineGrok, req.now)
	src := cliSessionSourceGrokStore
	if home == "" {
		res.skip(src, cliSessionSkipNotFound)
		return res
	}
	if _, err := os.Stat(home); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			res.skip(src, cliSessionSkipNotFound)
		} else {
			res.skip(src, cliSessionSkipUnreadable)
		}
		return res
	}
	root := filepath.Join(home, grokSessionsDirName)
	// The folders are read under ctx itself: a listing cut short still
	// answers the sessions it found.
	listCtx, cancelList := cliSessionListingContext(ctx)
	folders, complete, err := listGrokSessionFolders(listCtx, root)
	cancelList()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			res.SourcesChecked = append(res.SourcesChecked, src) // no session yet
		} else {
			res.skip(src, cliSessionSkipUnreadable)
		}
		return res
	}
	res.SourcesChecked = append(res.SourcesChecked, src)
	since := req.since().UnixMilli()
	kept := folders[:0]
	for _, f := range folders {
		if f.modified >= since {
			kept = append(kept, f)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].modified > kept[j].modified })
	if len(kept) > req.maxRows {
		kept = kept[:req.maxRows]
		res.Truncated = true
	}
	docs := readGrokSessionDocs(ctx, root)
	running := cliSessionRunningByID(grokWriterEvidence)
	timedOut := !complete
	seen := map[string]bool{}
	parsed, unrecognised, failed := 0, 0, 0
	for _, f := range kept {
		if ctx.Err() != nil {
			timedOut = true
			break
		}
		if seen[f.sessionID] {
			continue
		}
		info, err := readGrokSessionFolder(f.path, f.sessionID)
		switch {
		case errors.Is(err, errCliSessionUnrecognised):
			unrecognised++
			continue
		case err != nil:
			failed++
			continue
		}
		parsed++
		seen[f.sessionID] = true
		doc := docs[f.sessionID]
		row := cliSessionRow{
			SessionID:    f.sessionID,
			Cwd:          firstNonEmpty(info.cwd, doc.cwd, f.cwd),
			LastActiveAt: f.modified,
			Editors:      []cliSessionEditorRef{},
			Resumable:    info.resumable,
			Running:      cliSessionRowRunning(running[f.sessionID], f.modified, req.now),
		}
		row.Title, row.TitleSource, row.TitleSavedAt = grokRowTitle(info, doc)
		res.Rows = append(res.Rows, row)
	}
	switch {
	case timedOut:
		res.Truncated = true
		res.skip(src, cliSessionSkipTimeout)
	case parsed > 0:
	case unrecognised > 0:
		res.skip(src, cliSessionSkipUnrecognised)
	case failed > 0:
		res.skip(src, cliSessionSkipUnreadable)
	}
	return res
}

// findGrokSessionFolder is sessionID's folder in the store under home: under
// cwd's own encoding when cwd is given and has it, else the newest folder of
// that id under any cwd. "" when none.
func findGrokSessionFolder(home, sessionID, cwd string) string {
	folder, _ := searchGrokSessionFolder(home, sessionID, cwd)
	return folder
}

// searchGrokSessionFolder is findGrokSessionFolder with the search's error:
// the first cwd folder of the store, or folder of the session, that is there
// but could not be read (its permissions, an I/O error). The session may be
// kept there, so not finding it proves nothing; a folder found elsewhere
// still answers. No sessions folder is no error: no session was saved.
func searchGrokSessionFolder(home, sessionID, cwd string) (best string, err error) {
	if home == "" || !isCliSessionUUID(sessionID) {
		return "", nil
	}
	root := filepath.Join(home, grokSessionsDirName)
	if filepath.IsAbs(cwd) {
		p := filepath.Join(root, grokSessionCwdDirName(cwd), sessionID)
		if fi, statErr := os.Stat(p); statErr == nil && fi.IsDir() {
			return p, nil
		}
	}
	names, err := cliSessionDirNames(root)
	bestAt := int64(-1)
	for _, name := range names {
		// A cwd folder (session_search.sqlite and its journals are files).
		cwdDir := filepath.Join(root, name)
		if fi, statErr := os.Stat(cwdDir); statErr != nil || !fi.IsDir() {
			cliSessionNoteReadErr(&err, statErr)
			continue
		}
		p := filepath.Join(cwdDir, sessionID)
		fi, statErr := os.Stat(p)
		if statErr != nil || !fi.IsDir() {
			cliSessionNoteReadErr(&err, statErr)
			continue
		}
		if at := grokFolderWrittenAt(p); at > bestAt {
			best, bestAt = p, at
		}
	}
	return best, err
}

// probeGrokSession answers the probe of one session: a folder whose files
// name it is there (resumable when updates.jsonl does), one naming no
// session is there but not resumable, and none in a store searched in full
// is gone. A folder whose files could not be read, or no folder found where a
// cwd folder of the store could not be searched (searchGrokSessionFolder's
// error), says so instead: whether the session is kept, and loads, is
// unknown, so the answer is an error, which terminal-service reads as a
// session it could not check, never as one that is gone.
func probeGrokSession(ctx context.Context, home string, req cliSessionProbeRequest) cliSessionProbeResult {
	res := cliSessionProbeResult{SessionID: req.sessionID, Writers: []cliSessionWriter{}, OpenInEditors: []string{}, TitleSource: cliSessionTitleNone}
	if !isCliSessionUUID(req.sessionID) {
		res.Error = cliSessionErrBadRequest
		return res
	}
	res.Writers = cliSessionProcessWriters(grokWriterEvidence, req.sessionID)
	folder, err := searchGrokSessionFolder(home, req.sessionID, req.cwd)
	if folder != "" {
		res.TranscriptWrittenAt = grokFolderWrittenAt(folder)
		var info grokSessionInfo
		info, err = readGrokSessionFolder(folder, req.sessionID)
		switch {
		case err == nil:
			res.Exists, res.Resumable = true, info.resumable
			doc := readGrokSessionDocs(ctx, filepath.Join(home, grokSessionsDirName))[req.sessionID]
			res.Title, res.TitleSource, _ = grokRowTitle(info, doc)
		case errors.Is(err, errCliSessionUnrecognised):
			res.Exists, err = true, nil // a folder is there, not one this build can resume
		}
	}
	if err != nil {
		res.Error = cliSessionErrInternal
		return res
	}
	res.Exists = res.Exists || len(res.Writers) > 0
	return res
}
