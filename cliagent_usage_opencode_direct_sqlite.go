// cliagent_usage_opencode_direct_sqlite.go — the direct-run reader's adapter for
// OpenCode's SQLite store, opencode.db (cliagent_usage_opencode_direct.go).
//
// The database is opened read-only (mode=ro, query_only) through the pure-Go
// modernc.org/sqlite driver — no CGO, so every signed agent build keeps its
// current toolchain — with a busy timeout, and closed after each scan. The
// reader never writes to, locks, migrates or vacuums OpenCode's store.
//
// Shape check: PRAGMA table_info must list every column the queries use; a
// mismatch is errOpenCodeDirectLayoutUnknown (fail closed). Only assistant
// messages last updated at or after the floor are selected, in ascending
// time_updated order with LIMIT caps. JSON inside a row is narrowed IN SQLite —
// json_extract of tokens, cost and completed time, and the step-finish parts'
// tokens and cost — so message text and tool output never reach this process.
//
// This adapter stands alone: dropping it (and the driver) leaves the JSON
// adapter and the rest of the reader untouched.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// openCodeSQLiteBusyTimeout bounds a wait on OpenCode's own writer. In WAL mode
// a reader rarely waits at all.
const openCodeSQLiteBusyTimeout = 500 * time.Millisecond

// openCodeSQLiteColumns is every column the queries below read, by table.
var openCodeSQLiteColumns = map[string][]string{
	"session": {"id", "time_updated"},
	"message": {"id", "session_id", "time_created", "time_updated", "data"},
	"part":    {"message_id", "data"},
}

// openCodeSQLiteReadOnlyDSN is the driver DSN for a read-only open of path.
func openCodeSQLiteReadOnlyDSN(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // a Windows drive path: file:///C:/…
	}
	u := url.URL{Scheme: "file", Path: p}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", openCodeSQLiteBusyTimeout.Milliseconds()))
	q.Add("_pragma", "query_only(1)")
	u.RawQuery = q.Encode()
	return u.String()
}

// openCodeSQLiteMessageQuery selects the assistant messages of the given
// sessions (%s: their placeholders), oversized rows flagged rather than parsed.
// info is a narrow JSON object the row's data is reduced to; steps is the array
// of its step-finish parts' figures.
const openCodeSQLiteMessageQuery = `
SELECT m.id, m.session_id, m.time_created, m.time_updated,
       length(m.data) > ? AS oversized,
       CASE WHEN length(m.data) <= ? AND json_valid(m.data) THEN json_object(
         'tokens', json(json_extract(m.data, '$.tokens')),
         'cost', json_extract(m.data, '$.cost'),
         'completed', json_extract(m.data, '$.time.completed')) END AS info,
       CASE WHEN length(m.data) <= ? AND json_valid(m.data) THEN (
         SELECT json_group_array(json_object(
           'type', 'step-finish',
           'tokens', json(json_extract(p.data, '$.tokens')),
           'cost', json_extract(p.data, '$.cost')))
         FROM part p
         WHERE p.message_id = m.id
           AND length(p.data) <= ?
           AND CASE WHEN json_valid(p.data) THEN json_extract(p.data, '$.type') END = 'step-finish'
       ) END AS steps
FROM message m
WHERE m.session_id IN (%s)
  AND m.time_updated >= ?
  AND (length(m.data) > ? OR CASE WHEN json_valid(m.data) THEN json_extract(m.data, '$.role') END = 'assistant')
ORDER BY m.time_updated, m.id
LIMIT ?`

// openCodeSQLiteInfo is a message reduced by openCodeSQLiteMessageQuery.
type openCodeSQLiteInfo struct {
	Tokens    json.RawMessage `json:"tokens"`
	Cost      json.RawMessage `json:"cost"`
	Completed json.RawMessage `json:"completed"`
}

func readOpenCodeDirectSQLite(ctx context.Context, root string, floorMs int64, limits openCodeDirectLimits) (openCodeDirectRead, error) {
	var read openCodeDirectRead
	db, err := sql.Open("sqlite", openCodeSQLiteReadOnlyDSN(filepath.Join(root, "opencode.db")))
	if err != nil {
		return read, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if err := checkOpenCodeSQLiteSchema(ctx, db); err != nil {
		return read, err
	}

	// Sessions written since the session floor (less the slack: a session row can
	// be touched just before its last message), oldest first. A session past the
	// cap stops the cursor at its own last write.
	rows, err := db.QueryContext(ctx,
		`SELECT id, time_updated FROM session WHERE time_updated >= ? ORDER BY time_updated, id LIMIT ?`,
		limits.sessionsSinceMs(floorMs), limits.MaxSessions+1)
	if err != nil {
		return read, err
	}
	var sessions []any
	for rows.Next() {
		var id string
		var at int64
		if err := rows.Scan(&id, &at); err != nil {
			rows.Close()
			return read, err
		}
		if len(sessions) == limits.MaxSessions {
			read.truncateAt(at)
			break
		}
		sessions = append(sessions, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return read, err
	}
	if len(sessions) == 0 {
		return read, nil
	}

	maxBytes := limits.MaxRecordBytes
	query := fmt.Sprintf(openCodeSQLiteMessageQuery, strings.TrimSuffix(strings.Repeat("?,", len(sessions)), ","))
	args := []any{maxBytes, maxBytes, maxBytes, openCodeDirectMaxPartBytes}
	args = append(args, sessions...)
	args = append(args, floorMs, maxBytes, limits.MaxRecords+1)
	rows, err = db.QueryContext(ctx, query, args...)
	if err != nil {
		return read, err
	}
	defer rows.Close()
	lastMs := floorMs
	n := 0
	for rows.Next() {
		var (
			id, sessionID        string
			createdMs, writtenMs int64
			oversized            bool
			infoText, stepsText  sql.NullString
		)
		if err := rows.Scan(&id, &sessionID, &createdMs, &writtenMs, &oversized, &infoText, &stepsText); err != nil {
			return read, err
		}
		if n == limits.MaxRecords || ctx.Err() != nil {
			read.truncateAt(writtenMs)
			return read, nil
		}
		n++
		lastMs = writtenMs
		var info openCodeSQLiteInfo
		if oversized || !infoText.Valid || json.Unmarshal([]byte(infoText.String), &info) != nil || createdMs <= 0 {
			read.Skipped++
			continue
		}
		m := openCodeDirectMessage{ID: id, SessionID: sessionID, CreatedMs: createdMs, WrittenMs: writtenMs}
		if completed, ok := openCodeUsageMillis(info.Completed); ok {
			m.CompletedMs = completed
			var parts []openCodeExportPart
			if stepsText.Valid {
				_ = json.Unmarshal([]byte(stepsText.String), &parts)
			}
			m.Usage, m.Valid = openCodeExportMessageUsage(openCodeExportMessage{Parts: parts}, openCodeExportMessageInfo{Tokens: info.Tokens, Cost: info.Cost})
		}
		read.Messages = append(read.Messages, m)
	}
	if err := rows.Err(); err != nil {
		if ctx.Err() != nil {
			// The budget interrupted the query: everything up to the last row
			// was read whole.
			read.truncateAt(lastMs)
			return read, nil
		}
		return read, err
	}
	return read, nil
}

// checkOpenCodeSQLiteSchema fails closed unless every table lists every column
// the reader needs.
func checkOpenCodeSQLiteSchema(ctx context.Context, db *sql.DB) error {
	for table, want := range openCodeSQLiteColumns {
		rows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
		if err != nil {
			return err
		}
		have := map[string]bool{}
		for rows.Next() {
			var name string
			if rows.Scan(&name) == nil {
				have[name] = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, column := range want {
			if !have[column] {
				return errOpenCodeDirectLayoutUnknown
			}
		}
	}
	return nil
}
