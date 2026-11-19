// antigravity_session_sources.go — Antigravity's conversations, for discovery
// and the adoption probe (cli_session_discovery.go), and the writer evidence
// of a conversation AI Expedite continues (cli_session_outside_writer.go).
//
// The Antigravity CLI keeps them under antigravityHomeBase()
// (~/.gemini/antigravity-cli), the home antigravity_native.go resumes from:
//
//	conversations/<uuid>.db          one SQLite database per conversation
//	                                 (protobuf steps: never read here)
//	conversation_summaries.db        conversation_summaries(conversation_id,
//	                                 title, last_modified_time, workspace_uris,
//	                                 parent_conversation_id, nesting_depth, …)
//	cache/conversation_metadata.json {"conversations":{<uuid>:{"summary":
//	                                 {"Title","WorkspaceURIs",…},"is_internal"}}}
//	cache/last_conversations.json    {<cwd>: <uuid>}, the newest per folder
//	history.jsonl                    {"display","timestamp","workspace",
//	                                 "conversationId"?,"type"?} prompt history
//
// Checked against the store agy 1.x writes. A conversation's title is the one
// Antigravity shows (conversation_summaries, else the metadata cache); many
// have none, and history.jsonl names the conversation of only some prompts,
// so a title falls back to the newest of those prompts, and otherwise to
// none: the conversation is still listed (by folder and recency) and never
// given a title of the device's own making. Sub-conversations and internal
// ones are not listed. Read-only: the one SQLite store opened, the summaries
// database, is read through cliSessionSQLiteDSN (nothing created or locked);
// a conversation's own database is only ever stat-ed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// The metadata cache and the prompt history are read up to these sizes.
	antigravityMetadataMaxBytes = 32 << 20
	antigravityHistoryMaxBytes  = 8 << 20
	antigravityHistoryMaxLines  = 200000
	// antigravitySlashCommand is history.jsonl's type for a slash command:
	// never a label.
	antigravitySlashCommand = "slash_command"
)

var antigravitySessionReader = &cliSessionReader{
	engine:  cliSessionEngineAntigravity,
	command: "agy",
	sources: []string{cliSessionSourceAntigravityStore},
	discover: func(ctx context.Context, req cliSessionDiscoverRequest) cliSessionsDiscoverResult {
		return discoverAntigravitySessions(ctx, antigravityHomeBase(), req)
	},
	probe: func(ctx context.Context, req cliSessionProbeRequest) cliSessionProbeResult {
		return probeAntigravitySession(ctx, antigravityHomeBase(), req)
	},
}

// antigravityWriterEvidence: a live `agy` naming the conversation
// (`agy --conversation <uuid>`) holds it open; its database written within
// writerQuietMs is a writer mid-turn.
var antigravityWriterEvidence = cliSessionWriterEvidence{
	validID:        isCliSessionUUID,
	isAgentProcess: cliSessionNameIn("agy", "antigravity"),
	storeClock: func(conversationID string) func() int64 {
		db := antigravityConversationDB(antigravityHomeBase(), conversationID)
		return func() int64 { return antigravityConversationWrittenAt(db) }
	},
}

// antigravityConversationDB is a conversation's database path ("" for none).
func antigravityConversationDB(base, id string) string {
	if base == "" || !isCliSessionUUID(id) {
		return ""
	}
	return filepath.Join(base, "conversations", id+".db")
}

// antigravityConversationWrittenAt is the newest write to a conversation's
// database or its write-ahead log.
func antigravityConversationWrittenAt(db string) int64 {
	if db == "" {
		return 0
	}
	return newestFileMs(db, db+"-wal")
}

// antigravityMeta is what the metadata sources say about one conversation.
type antigravityMeta struct {
	title, cwd              string
	titleAt                 int64
	lastPrompt, firstPrompt string
	hidden                  bool // a sub-conversation or an internal one
}

// antigravityMetadata reads every metadata source once per request.
type antigravityMetadata struct {
	byID map[string]*antigravityMeta
}

func (m antigravityMetadata) of(id string) *antigravityMeta {
	if e := m.byID[id]; e != nil {
		return e
	}
	return &antigravityMeta{}
}

func (m antigravityMetadata) entry(id string) *antigravityMeta {
	e := m.byID[id]
	if e == nil {
		e = &antigravityMeta{}
		m.byID[id] = e
	}
	return e
}

// readAntigravityMetadata reads the summaries database, the metadata cache,
// the last-conversation map and the prompt history. A source that is missing
// or in a shape this build does not know is passed over: the conversations
// are still listed from their databases.
func readAntigravityMetadata(ctx context.Context, base string) antigravityMetadata {
	m := antigravityMetadata{byID: map[string]*antigravityMeta{}}
	readAntigravitySummaries(ctx, filepath.Join(base, "conversation_summaries.db"), m)

	var cache struct {
		Conversations map[string]struct {
			Summary struct {
				Title         string   `json:"Title"`
				WorkspaceURIs []string `json:"WorkspaceURIs"`
				UpdatedAt     string   `json:"UpdatedAt"`
			} `json:"summary"`
			IsInternal bool `json:"is_internal"`
		} `json:"conversations"`
	}
	if readJSONFileWithin(filepath.Join(base, "cache", "conversation_metadata.json"), antigravityMetadataMaxBytes, &cache) {
		for id, c := range cache.Conversations {
			if !isCliSessionUUID(id) {
				continue
			}
			e := m.entry(id)
			e.hidden = e.hidden || c.IsInternal
			if e.title == "" {
				e.title, e.titleAt = strings.TrimSpace(c.Summary.Title), parseCliSessionTime(c.Summary.UpdatedAt)
			}
			if e.cwd == "" && len(c.Summary.WorkspaceURIs) > 0 {
				e.cwd = cliSessionPathFromURI(c.Summary.WorkspaceURIs[0])
			}
		}
	}

	var last map[string]string
	if readJSONFileWithin(filepath.Join(base, "cache", "last_conversations.json"), antigravityMetadataMaxBytes, &last) {
		for cwd, id := range last {
			if isCliSessionUUID(id) && filepath.IsAbs(cwd) {
				if e := m.entry(id); e.cwd == "" {
					e.cwd = cwd
				}
			}
		}
	}

	// Prompts, oldest first: the first one seen is the first prompt, the last
	// one the newest.
	_ = readJSONLHead(filepath.Join(base, "history.jsonl"), antigravityHistoryMaxBytes, antigravityHistoryMaxLines, func(line []byte) bool {
		var h struct {
			Display        string `json:"display"`
			Workspace      string `json:"workspace"`
			ConversationID string `json:"conversationId"`
			Type           string `json:"type"`
		}
		if json.Unmarshal(line, &h) != nil || !isCliSessionUUID(h.ConversationID) {
			return true
		}
		text := strings.TrimSpace(h.Display)
		if h.Type == antigravitySlashCommand || strings.HasPrefix(text, "/") || cliSessionSyntheticPrompt(text) {
			return true
		}
		e := m.entry(h.ConversationID)
		if e.firstPrompt == "" {
			e.firstPrompt = text
		}
		e.lastPrompt = text
		if e.cwd == "" && filepath.IsAbs(h.Workspace) {
			e.cwd = h.Workspace
		}
		return true
	})
	return m
}

// readAntigravitySummaries reads conversation_summaries (the titles
// Antigravity shows) into m.
func readAntigravitySummaries(ctx context.Context, path string, m antigravityMetadata) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	db, err := openCliSessionSQLite(ctx, path)
	if err != nil {
		return
	}
	defer db.Close()
	cols := sqliteColumns(ctx, db, "conversation_summaries")
	if !hasAll(cols, "conversation_id", "title", "last_modified_time", "workspace_uris") {
		return
	}
	nested := "0"
	if hasAll(cols, "parent_conversation_id", "nesting_depth") {
		nested = `(parent_conversation_id <> '' OR nesting_depth > 0)`
	}
	rows, err := db.QueryContext(ctx, `SELECT conversation_id, title, CAST(last_modified_time AS TEXT), workspace_uris, `+nested+` FROM conversation_summaries LIMIT 20000`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, title, modified, uris string
		var hidden bool
		if rows.Scan(&id, &title, &modified, &uris, &hidden) != nil || !isCliSessionUUID(id) {
			continue
		}
		e := m.entry(id)
		e.title, e.titleAt, e.hidden = strings.TrimSpace(title), parseCliSessionTime(modified), hidden
		var list []string
		if json.Unmarshal([]byte(uris), &list) == nil && len(list) > 0 {
			e.cwd = cliSessionPathFromURI(list[0])
		}
	}
}

// antigravityRowTitle is a conversation's title by the shared precedence.
func antigravityRowTitle(e *antigravityMeta) (string, string, int64) {
	return cliSessionTitleOf(e.title, e.titleAt, e.lastPrompt, e.firstPrompt)
}

// antigravityConversation is one conversation database the listing found.
type antigravityConversation struct {
	id       string
	modified int64
}

// discoverAntigravitySessions lists the conversations written within the
// request's look-back, newest first.
func discoverAntigravitySessions(ctx context.Context, base string, req cliSessionDiscoverRequest) cliSessionsDiscoverResult {
	res := newCliSessionsResult(cliSessionEngineAntigravity, req.now)
	src := cliSessionSourceAntigravityStore
	if base == "" {
		res.skip(src, cliSessionSkipNotFound)
		return res
	}
	if _, err := os.Stat(base); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			res.skip(src, cliSessionSkipNotFound)
		} else {
			res.skip(src, cliSessionSkipUnreadable)
		}
		return res
	}
	entries, err := os.ReadDir(filepath.Join(base, "conversations"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			res.SourcesChecked = append(res.SourcesChecked, src) // no conversation yet
		} else {
			res.skip(src, cliSessionSkipUnreadable)
		}
		return res
	}
	res.SourcesChecked = append(res.SourcesChecked, src)
	since := req.since().UnixMilli()
	// The conversations found are read under ctx itself: a listing cut short
	// still answers the ones it found.
	listCtx, cancelList := cliSessionListingContext(ctx)
	defer cancelList()
	found, known, other, timedOut := []antigravityConversation{}, 0, 0, false
	for _, e := range entries {
		if listCtx.Err() != nil {
			timedOut = true
			break
		}
		id, isDB := strings.CutSuffix(e.Name(), ".db")
		if e.IsDir() || !isDB || !isCliSessionUUID(id) {
			if !strings.HasSuffix(e.Name(), "-wal") && !strings.HasSuffix(e.Name(), "-shm") && !strings.HasSuffix(e.Name(), "-journal") {
				other++
			}
			continue
		}
		known++
		at := antigravityConversationWrittenAt(filepath.Join(base, "conversations", e.Name()))
		if at >= since {
			found = append(found, antigravityConversation{id: id, modified: at})
		}
	}
	if known == 0 && other > 0 && !timedOut {
		// Files are there, none a conversation database this build knows.
		res.skip(src, cliSessionSkipUnrecognised)
		return res
	}
	sort.Slice(found, func(i, j int) bool { return found[i].modified > found[j].modified })
	meta := readAntigravityMetadata(ctx, base)
	running := cliSessionRunningByID(antigravityWriterEvidence)
	for _, c := range found {
		if ctx.Err() != nil {
			timedOut = true
			break
		}
		e := meta.of(c.id)
		if e.hidden {
			continue
		}
		if len(res.Rows) == req.maxRows {
			res.Truncated = true
			break
		}
		row := cliSessionRow{
			SessionID:    c.id,
			Cwd:          e.cwd,
			LastActiveAt: c.modified,
			Editors:      []cliSessionEditorRef{},
			Resumable:    true,
			Running:      cliSessionRowRunning(running[c.id], c.modified, req.now),
		}
		row.Title, row.TitleSource, row.TitleSavedAt = antigravityRowTitle(e)
		res.Rows = append(res.Rows, row)
	}
	if timedOut {
		res.Truncated = true
		res.skip(src, cliSessionSkipTimeout)
	}
	return res
}

// probeAntigravitySession answers the probe of one conversation.
func probeAntigravitySession(ctx context.Context, base string, req cliSessionProbeRequest) cliSessionProbeResult {
	res := cliSessionProbeResult{SessionID: req.sessionID, Writers: []cliSessionWriter{}, OpenInEditors: []string{}, TitleSource: cliSessionTitleNone}
	if !isCliSessionUUID(req.sessionID) {
		res.Error = cliSessionErrBadRequest
		return res
	}
	res.Writers = cliSessionProcessWriters(antigravityWriterEvidence, req.sessionID)
	db := antigravityConversationDB(base, req.sessionID)
	if fi, err := os.Stat(db); db != "" && err == nil && fi.Mode().IsRegular() {
		res.Exists, res.Resumable = true, true
		res.TranscriptWrittenAt = antigravityConversationWrittenAt(db)
		res.Title, res.TitleSource, _ = antigravityRowTitle(readAntigravityMetadata(ctx, base).of(req.sessionID))
	}
	res.Exists = res.Exists || len(res.Writers) > 0
	return res
}
