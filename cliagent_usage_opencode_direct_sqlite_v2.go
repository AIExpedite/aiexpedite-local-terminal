// cliagent_usage_opencode_direct_sqlite_v2.go — the SQLite adapter's reader for
// OpenCode 2's schema family (cliagent_usage_opencode_direct_sqlite.go elects
// it): session heads in session_v2 (session in a pre-release layout) and every
// message in session_message, with no part table.
//
//   - An assistant message is a row of type 'assistant': V2 data carries no
//     role, and every other type (user, idle, shell, synthetic, compaction,
//     switches) is excluded in SQL.
//   - data is narrowed in SQLite to its tokens, cost and completed time, under
//     the same byte cap the V1 query keeps; text, tool input/output and model
//     objects never leave SQLite.
//   - A message is complete once it has a completed time or, where data carries
//     none, once a later idle row (higher seq) exists in its session. Until then
//     it pins the cursor as V1's running turns do.
//   - With no part table, a message's tokens are all it reports. If V2
//     overwrites them on every step, a multi-step turn is under-counted to its
//     last step — an under-count, never inflated.
//
// The column list and the completed-time path come from OpenCode 2.0.x stores;
// a store that names them differently fails closed in the election.
//
// Dropping V2 support means deleting this file and the family's line in
// openCodeSQLiteFamily.
package main

import (
	"context"
	"database/sql"
	"fmt"
)

// openCodeSQLiteV2Columns is every column the V2 queries read: the message
// table's, and any session heads table's.
var openCodeSQLiteV2Columns = map[string][]string{
	"session_message": {"id", "session_id", "seq", "type", "time_created", "time_updated", "data"},
	"heads":           {"id", "time_updated"},
}

// openCodeSQLiteV2HeadTables are the V2 session heads tables, preferred first.
var openCodeSQLiteV2HeadTables = []string{"session_v2", "session"}

// openCodeSQLiteV2MessageQuery selects the assistant messages of the given
// sessions (%s: their placeholders), oversized rows flagged rather than parsed.
// idled is computed only for a row with no completed time, so the correlated
// lookup costs nothing for finished turns.
const openCodeSQLiteV2MessageQuery = `
SELECT m.id, m.session_id, m.time_created, m.time_updated,
       length(CAST(m.data AS BLOB)) > ? AS oversized,
       CASE WHEN length(CAST(m.data AS BLOB)) <= ? AND json_valid(m.data) THEN json_object(
         'tokens', json(m.data -> '$.tokens'),
         'cost', json_extract(m.data, '$.cost'),
         'completed', json_extract(m.data, '$.time.completed')) END AS info,
       CASE WHEN length(CAST(m.data AS BLOB)) <= ? AND json_valid(m.data)
                 AND json_extract(m.data, '$.time.completed') IS NULL THEN EXISTS (
         SELECT 1 FROM session_message i
         WHERE i.session_id = m.session_id AND i.type = 'idle' AND i.seq > m.seq
       ) ELSE 0 END AS idled
FROM session_message m
WHERE m.session_id IN (%s)
  AND m.time_updated >= ?
  AND m.type = 'assistant'
ORDER BY m.time_updated, m.id
LIMIT ?`

// openCodeSQLiteV2Candidate returns the V2 heads table when the store holds the
// V2 family whole, or "".
func openCodeSQLiteV2Candidate(ctx context.Context, db *sql.DB, tables map[string]bool) (string, error) {
	if !tables["session_message"] {
		return "", nil
	}
	ok, err := openCodeSQLiteHasColumns(ctx, db, "session_message", openCodeSQLiteV2Columns["session_message"])
	if err != nil || !ok {
		return "", err
	}
	for _, heads := range openCodeSQLiteV2HeadTables {
		if !tables[heads] {
			continue
		}
		ok, err := openCodeSQLiteHasColumns(ctx, db, heads, openCodeSQLiteV2Columns["heads"])
		if err != nil || !ok {
			return "", err
		}
		return heads, nil
	}
	return "", nil
}

// readOpenCodeSQLiteV2Messages reads the V2 family's assistant messages of the
// listed sessions.
func readOpenCodeSQLiteV2Messages(ctx context.Context, db *sql.DB, sessions []any, floorMs int64, limits openCodeDirectLimits, read *openCodeDirectRead) error {
	maxBytes := limits.MaxRecordBytes
	query := fmt.Sprintf(openCodeSQLiteV2MessageQuery, openCodeSQLitePlaceholders(len(sessions)))
	args := []any{maxBytes, maxBytes, maxBytes}
	args = append(args, sessions...)
	skip := limits.skipFor(false)
	args = append(args, floorMs, limits.MaxRecords+1+skip.N)
	return readOpenCodeSQLiteMessageRows(ctx, db, query, args, floorMs, limits, read, func(rows *sql.Rows) (openCodeDirectMessage, bool, error) {
		var (
			m         openCodeDirectMessage
			oversized bool
			idled     bool
			infoText  sql.NullString
		)
		if err := rows.Scan(&m.ID, &m.SessionID, &m.CreatedMs, &m.WrittenMs, &oversized, &infoText, &idled); err != nil {
			return m, false, err
		}
		info, ok := parseOpenCodeSQLiteInfo(m, oversized, infoText)
		if !ok {
			return m, false, nil
		}
		completed, ok := openCodeUsageMillis(info.Completed)
		if !ok && idled {
			// The session went idle after it: the turn is over, and its last
			// write is when it finished.
			completed, ok = m.WrittenMs, true
		}
		if ok {
			m.CompletedMs = completed
			m.Usage, m.Valid = openCodeUsageFromTokens(info.Tokens, info.Cost)
		}
		return m, true, nil
	})
}

// openCodeSQLiteV2FirstIdle is when the session first went idle at or after
// fromMs — the end of the turn begun there — or 0.
func openCodeSQLiteV2FirstIdle(ctx context.Context, db *sql.DB, sessionID string, fromMs int64) (int64, error) {
	var ms int64
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MIN(time_created), 0) FROM session_message WHERE session_id = ? AND type = 'idle' AND time_created >= ?`,
		sessionID, fromMs).Scan(&ms)
	return ms, err
}
