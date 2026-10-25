// cliagent_usage_opencode_direct_sqlite.go — the direct-run reader's adapter for
// OpenCode's SQLite store, opencode.db (cliagent_usage_opencode_direct.go).
//
// The database is opened read-only (mode=ro, query_only) through the pure-Go
// modernc.org/sqlite driver — no CGO, so every signed agent build keeps its
// current toolchain — with a busy timeout, and closed after each scan. The
// reader never writes to, locks, migrates or vacuums OpenCode's store.
//
// Schema family: OpenCode 2 keeps opencode.db but writes its sessions and
// messages to new tables (session_v2 / session_message, no part table) and
// leaves the V1 session / message / part tables in place, FROZEN. Those still
// pass the V1 shape check, so a reader that only knew V1 would read nothing new
// on every scan and still renew today's coverage. OpenCode 1.19 is the mirror
// case: it stages an EMPTY session_message beside the V1 tables it still writes
// every turn to, both families sharing the session heads table, so a reader
// that took that table's presence for V2 reads nothing new either. Each read
// therefore elects one family (openCodeSQLiteFamily) by where the newest
// sessions' messages are, never by which tables exist. The V2 reader lives in
// cliagent_usage_opencode_direct_sqlite_v2.go; read.Layout names the family read.
//
// Shape check: PRAGMA table_info must list every column the family's queries
// use; a store neither family fits is errOpenCodeDirectLayoutUnknown (fail
// closed). Only assistant messages last updated at or after the floor are
// selected, in ascending time_updated order with LIMIT caps. JSON inside a row
// is narrowed IN SQLite — json_extract of tokens, cost and completed time, and
// (V1) the step-finish parts' tokens and cost — so message text and tool output
// never reach this process.
//
// This adapter stands alone: dropping it (and the driver) leaves the JSON
// adapter and the rest of the reader untouched.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// openCodeSQLiteBusyTimeout bounds a wait on OpenCode's own writer. In WAL mode
// a reader rarely waits at all.
const openCodeSQLiteBusyTimeout = 500 * time.Millisecond

// openCodeSQLiteColumns is every column the V1 queries below read, by table.
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
// of its step-finish parts' figures. tokens is read with -> (always JSON text,
// even for a string), so a malformed value reaches Go as an invalid message
// rather than failing json() and the whole query. A message's step parts are
// capped before they are aggregated, as the JSON adapter caps its part listing,
// so one long agentic turn cannot build an unbounded steps value. Every size
// check casts data to a blob first: length() over TEXT counts characters, so a
// record holding multibyte text would otherwise pass a byte cap up to four
// times its limit.
const openCodeSQLiteMessageQuery = `
SELECT m.id, m.session_id, m.time_created, m.time_updated,
       length(CAST(m.data AS BLOB)) > ? AS oversized,
       CASE WHEN length(CAST(m.data AS BLOB)) <= ? AND json_valid(m.data) THEN json_object(
         'tokens', json(m.data -> '$.tokens'),
         'cost', json_extract(m.data, '$.cost'),
         'completed', json_extract(m.data, '$.time.completed')) END AS info,
       CASE WHEN length(CAST(m.data AS BLOB)) <= ? AND json_valid(m.data) THEN (
         SELECT json_group_array(json_object(
           'type', 'step-finish',
           'tokens', json(p.data -> '$.tokens'),
           'cost', json_extract(p.data, '$.cost')))
         FROM (
           SELECT p.data FROM part p
           WHERE p.message_id = m.id
             AND length(CAST(p.data AS BLOB)) <= ?
             AND CASE WHEN json_valid(p.data) THEN json_extract(p.data, '$.type') END = 'step-finish'
           LIMIT ?
         ) p
       ) END AS steps
FROM message m
WHERE m.session_id IN (%s)
  AND m.time_updated >= ?
  AND (length(CAST(m.data AS BLOB)) > ? OR CASE WHEN json_valid(m.data) THEN json_extract(m.data, '$.role') END = 'assistant')
ORDER BY m.time_updated, m.id
LIMIT ?`

// openCodeSQLiteInfo is a message reduced by a family's message query.
type openCodeSQLiteInfo struct {
	Tokens    json.RawMessage `json:"tokens"`
	Cost      json.RawMessage `json:"cost"`
	Completed json.RawMessage `json:"completed"`
}

// openCodeSQLiteFamilyRead is one schema family's reader: the session heads
// table it lists, the table its messages live in, and how it reads the
// messages of the listed sessions.
type openCodeSQLiteFamilyRead struct {
	Layout       string
	Heads        string
	MessageTable string
	Messages     func(ctx context.Context, db *sql.DB, sessions []any, floorMs int64, limits openCodeDirectLimits, read *openCodeDirectRead) error
}

// openOpenCodeSQLite opens root's opencode.db read-only.
func openOpenCodeSQLite(root string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", openCodeSQLiteReadOnlyDSN(filepath.Join(root, "opencode.db")))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func readOpenCodeDirectSQLite(ctx context.Context, root string, floorMs int64, limits openCodeDirectLimits) (openCodeDirectRead, error) {
	var read openCodeDirectRead
	db, err := openOpenCodeSQLite(root)
	if err != nil {
		return read, err
	}
	defer db.Close()

	family, _, err := openCodeSQLiteFamily(ctx, db)
	if err != nil {
		return read, err
	}
	read.Layout = family.Layout
	sessions, err := listOpenCodeSQLiteSessions(ctx, db, family.Heads, floorMs, limits, &read)
	if err != nil || len(sessions) == 0 {
		return read, err
	}
	return read, family.Messages(ctx, db, sessions, floorMs, limits, &read)
}

// listOpenCodeSQLiteSessions lists the sessions written since the session floor
// (less the slack: a session row can be touched just before its last message),
// oldest first. A session past the cap stops the cursor at its own last write.
// A continuation's tie-breaker drops the sessions it listed already. heads is
// one of the fixed table names a family names, never input.
func listOpenCodeSQLiteSessions(ctx context.Context, db *sql.DB, heads string, floorMs int64, limits openCodeDirectLimits, read *openCodeDirectRead) ([]any, error) {
	skip := limits.skipFor(true)
	rows, err := db.QueryContext(ctx,
		fmt.Sprintf(`SELECT id, time_updated FROM %q WHERE time_updated >= ? ORDER BY time_updated, id LIMIT ?`, heads),
		limits.sessionsSinceMs(floorMs), limits.MaxSessions+1+skip.N)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []any
	var ties openCodeDirectTies
	for rows.Next() {
		var id string
		var at int64
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		before := ties.see(at)
		if skip.take(at) {
			continue
		}
		if len(sessions) == limits.MaxSessions {
			read.truncateSessionsAt(at, before)
			break
		}
		sessions = append(sessions, id)
	}
	return sessions, rows.Err()
}

// readOpenCodeSQLiteV1Messages reads the V1 family's assistant messages of the
// listed sessions, summing each one's step-finish parts.
func readOpenCodeSQLiteV1Messages(ctx context.Context, db *sql.DB, sessions []any, floorMs int64, limits openCodeDirectLimits, read *openCodeDirectRead) error {
	maxBytes := limits.MaxRecordBytes
	query := fmt.Sprintf(openCodeSQLiteMessageQuery, openCodeSQLitePlaceholders(len(sessions)))
	args := []any{maxBytes, maxBytes, maxBytes, openCodeDirectMaxPartBytes, openCodeDirectMaxPartsPerMessage}
	args = append(args, sessions...)
	skip := limits.skipFor(false)
	args = append(args, floorMs, maxBytes, limits.MaxRecords+1+skip.N)
	return readOpenCodeSQLiteMessageRows(ctx, db, query, args, floorMs, limits, read, func(rows *sql.Rows) (openCodeDirectMessage, bool, error) {
		var (
			m                   openCodeDirectMessage
			oversized           bool
			infoText, stepsText sql.NullString
		)
		if err := rows.Scan(&m.ID, &m.SessionID, &m.CreatedMs, &m.WrittenMs, &oversized, &infoText, &stepsText); err != nil {
			return m, false, err
		}
		info, ok := parseOpenCodeSQLiteInfo(m, oversized, infoText)
		if !ok {
			return m, false, nil
		}
		if completed, ok := openCodeUsageMillis(info.Completed); ok {
			m.CompletedMs = completed
			var parts []openCodeExportPart
			if stepsText.Valid {
				_ = json.Unmarshal([]byte(stepsText.String), &parts)
			}
			m.Usage, m.Valid = openCodeExportMessageUsage(openCodeExportMessage{Parts: parts}, openCodeExportMessageInfo{Tokens: info.Tokens, Cost: info.Cost})
		}
		return m, true, nil
	})
}

// readOpenCodeSQLiteMessageRows runs a family's message query and folds its
// rows into read under the record cap, the time budget and the continuation's
// tie-breaker every family shares. decode scans one row; ok=false passes an
// oversized or unparseable record over for good (read.Skipped).
func readOpenCodeSQLiteMessageRows(ctx context.Context, db *sql.DB, query string, args []any, floorMs int64, limits openCodeDirectLimits, read *openCodeDirectRead,
	decode func(*sql.Rows) (openCodeDirectMessage, bool, error)) error {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	skip := limits.skipFor(false)
	lastMs := floorMs
	n := 0
	var ties openCodeDirectTies
	for rows.Next() {
		m, ok, err := decode(rows)
		if err != nil {
			return err
		}
		before := ties.see(m.WrittenMs)
		if skip.take(m.WrittenMs) {
			lastMs = m.WrittenMs
			continue
		}
		if n == limits.MaxRecords || ctx.Err() != nil {
			read.truncateAt(m.WrittenMs, before)
			return nil
		}
		n++
		lastMs = m.WrittenMs
		if !ok {
			read.Skipped++
			continue
		}
		read.Messages = append(read.Messages, m)
	}
	if err := rows.Err(); err != nil {
		if ctx.Err() != nil {
			// The budget interrupted the query: everything up to the last row
			// was read whole.
			read.truncateAt(lastMs, ties.through(lastMs))
			return nil
		}
		return err
	}
	return nil
}

// parseOpenCodeSQLiteInfo decodes a row's narrowed info; ok=false is a record
// to pass over for good: oversized, unparseable, or with no creation time.
func parseOpenCodeSQLiteInfo(m openCodeDirectMessage, oversized bool, infoText sql.NullString) (info openCodeSQLiteInfo, ok bool) {
	if oversized || !infoText.Valid || json.Unmarshal([]byte(infoText.String), &info) != nil || m.CreatedMs <= 0 {
		return info, false
	}
	return info, true
}

// openCodeSQLitePlaceholders is n comma-separated "?".
func openCodeSQLitePlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

/* --------------------------------------------------------------------------
   Schema family
   -------------------------------------------------------------------------- */

// openCodeSQLiteFamily elects the schema family a read uses. V2 is a candidate
// when session_message has every column its reader needs and a heads table does
// (session_v2, else session); V1 when the V1 shape check passes.
//
//   - Distinct heads: the family whose heads were written most recently wins,
//     and a tie (both empty included) goes to V2, the family a fresh write
//     comes from.
//   - Shared heads (both on session): the table set decides nothing, since
//     OpenCode 1.19 creates session_message empty and keeps writing V1, while a
//     pre-release V2 store writes session_message under V1's session table.
//     The family holding the newest session's messages wins
//     (openCodeSQLiteSharedHeadsFamily).
//
// The scan logs direct_family_suppressed when the losing family holds a
// session written since local midnight too, since its spend goes uncounted.
// Neither candidate, or a query that fails for any reason but a busy store or
// the budget, fails closed. other is the losing candidate (Layout "" when there
// is none): a debt's read probes it for a session the elected family lacks.
func openCodeSQLiteFamily(ctx context.Context, db *sql.DB) (elected, other openCodeSQLiteFamilyRead, err error) {
	v1 := openCodeSQLiteFamilyRead{Layout: openCodeStoreLayoutSQLite, Heads: "session", MessageTable: "message", Messages: readOpenCodeSQLiteV1Messages}
	v2 := openCodeSQLiteFamilyRead{Layout: openCodeStoreLayoutSQLiteV2, MessageTable: "session_message", Messages: readOpenCodeSQLiteV2Messages}
	tables, err := openCodeSQLiteTables(ctx, db)
	if err != nil {
		return v1, openCodeSQLiteFamilyRead{}, openCodeSQLiteElectionErr(ctx, err)
	}
	hasV2, err := openCodeSQLiteV2Candidate(ctx, db, tables)
	if err != nil {
		return v1, openCodeSQLiteFamilyRead{}, openCodeSQLiteElectionErr(ctx, err)
	}
	if hasV2 == "" && tables["session_message"] {
		// Only V2 writes session_message: a store holding one this reader cannot
		// read is V2 in a shape it does not know, and the V1 tables beside it
		// are frozen. Electing them would renew coverage on stale data.
		return v1, openCodeSQLiteFamilyRead{}, errOpenCodeDirectLayoutUnknown
	}
	v2.Heads = hasV2
	hasV1 := false
	if tables["session"] && tables["message"] && tables["part"] {
		switch err := checkOpenCodeSQLiteSchema(ctx, db); {
		case err == nil:
			hasV1 = true
		case !errors.Is(err, errOpenCodeDirectLayoutUnknown):
			return v1, openCodeSQLiteFamilyRead{}, openCodeSQLiteElectionErr(ctx, err)
		}
	}
	switch {
	case v2.Heads == "" && !hasV1:
		return v1, openCodeSQLiteFamilyRead{}, errOpenCodeDirectLayoutUnknown
	case v2.Heads == "":
		return v1, openCodeSQLiteFamilyRead{}, nil
	case !hasV1:
		return v2, openCodeSQLiteFamilyRead{}, nil
	case v2.Heads == v1.Heads:
		elected, other, err := openCodeSQLiteSharedHeadsFamily(ctx, db, v1, v2)
		if err != nil {
			return v1, openCodeSQLiteFamilyRead{}, openCodeSQLiteElectionErr(ctx, err)
		}
		return elected, other, nil
	}
	v1Ms, err := openCodeSQLiteNewestHead(ctx, db, v1.Heads)
	if err != nil {
		return v1, openCodeSQLiteFamilyRead{}, openCodeSQLiteElectionErr(ctx, err)
	}
	v2Ms, err := openCodeSQLiteNewestHead(ctx, db, v2.Heads)
	if err != nil {
		return v1, openCodeSQLiteFamilyRead{}, openCodeSQLiteElectionErr(ctx, err)
	}
	if midnight := openCodeLocalMidnight(openCodeDirectNow()).UnixMilli(); v1Ms >= midnight && v2Ms >= midnight {
		logOpenCodeUsageCapture("direct_family_suppressed")
	}
	if v1Ms > v2Ms {
		return v1, v2, nil
	}
	return v2, v1, nil
}

// openCodeSQLiteSharedHeadsProbe is how many of the most recently updated
// sessions a shared-heads election probes.
var openCodeSQLiteSharedHeadsProbe = 8

// openCodeSQLiteSharedHeadsElected is the family the last shared-heads election
// in this process chose, so direct_family_shared_heads_v1 is logged when the
// election changes rather than on every tick.
var openCodeSQLiteSharedHeadsElected atomic.Value

// openCodeSQLiteSharedHeadsFamily elects between two families listing the same
// session heads table by the newest sessions' messages. The probed sessions are
// walked newest first: the first one exactly one family holds decides, one both
// hold goes to V2 as a tie does on distinct heads, and one neither holds (no
// message yet) is passed over. When none of the probe decides, V2 wins if
// session_message has any row at all, else V1. Every lookup is one indexed
// EXISTS by session_id; session ids are bound parameters, never logged or kept.
func openCodeSQLiteSharedHeadsFamily(ctx context.Context, db *sql.DB, v1, v2 openCodeSQLiteFamilyRead) (elected, other openCodeSQLiteFamilyRead, err error) {
	sessions, err := openCodeSQLiteNewestSessions(ctx, db, v1.Heads, openCodeSQLiteSharedHeadsProbe)
	if err != nil {
		return v1, openCodeSQLiteFamilyRead{}, err
	}
	decided := -1
	for i, s := range sessions {
		inV2, err := openCodeSQLiteHoldsSession(ctx, db, v2.MessageTable, s.ID)
		if err != nil {
			return v1, openCodeSQLiteFamilyRead{}, err
		}
		if inV2 {
			elected, other, decided = v2, v1, i
			break
		}
		inV1, err := openCodeSQLiteHoldsSession(ctx, db, v1.MessageTable, s.ID)
		if err != nil {
			return v1, openCodeSQLiteFamilyRead{}, err
		}
		if inV1 {
			elected, other, decided = v1, v2, i
			break
		}
	}
	if decided < 0 {
		var anyV2 bool
		if err := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %q)`, v2.MessageTable)).Scan(&anyV2); err != nil {
			return v1, openCodeSQLiteFamilyRead{}, err
		}
		elected, other = v1, v2
		if anyV2 {
			elected, other = v2, v1
		}
	}
	// The loser's spend goes uncounted: say so when it holds one of the probed
	// sessions written since local midnight. An undecided probe holds none.
	midnight := openCodeLocalMidnight(openCodeDirectNow()).UnixMilli()
	for _, s := range sessions[decided+1:] {
		if decided < 0 {
			break
		}
		if s.UpdatedMs < midnight {
			break
		}
		held, err := openCodeSQLiteHoldsSession(ctx, db, other.MessageTable, s.ID)
		if err != nil {
			return v1, openCodeSQLiteFamilyRead{}, err
		}
		if held {
			logOpenCodeUsageCapture("direct_family_suppressed")
			break
		}
	}
	if previous := openCodeSQLiteSharedHeadsElected.Swap(elected.Layout); elected.Layout == openCodeStoreLayoutSQLite && previous != elected.Layout {
		logOpenCodeUsageCapture("direct_family_shared_heads_v1")
	}
	return elected, other, nil
}

// openCodeSQLiteHead is one session head: its id and last write.
type openCodeSQLiteHead struct {
	ID        string
	UpdatedMs int64
}

// openCodeSQLiteNewestSessions lists table's n most recently updated sessions,
// newest first.
func openCodeSQLiteNewestSessions(ctx context.Context, db *sql.DB, table string, n int) ([]openCodeSQLiteHead, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT id, time_updated FROM %q ORDER BY time_updated DESC, id DESC LIMIT ?`, table), n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var heads []openCodeSQLiteHead
	for rows.Next() {
		var h openCodeSQLiteHead
		if err := rows.Scan(&h.ID, &h.UpdatedMs); err != nil {
			return nil, err
		}
		heads = append(heads, h)
	}
	return heads, rows.Err()
}

// openCodeSQLiteHoldsSession reports whether table, a family's message table,
// holds a message of the session.
func openCodeSQLiteHoldsSession(ctx context.Context, db *sql.DB, table, sessionID string) (bool, error) {
	var held bool
	err := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %q WHERE session_id = ?)`, table), sessionID).Scan(&held)
	return held, err
}

// openCodeSQLiteElectionErr is what a failed election query reports: a busy
// store or a spent budget is a read to retry, anything else an unknown layout.
func openCodeSQLiteElectionErr(ctx context.Context, err error) error {
	if ctx.Err() != nil || isOpenCodeSQLiteBusy(err) {
		return err
	}
	return errOpenCodeDirectLayoutUnknown
}

// isOpenCodeSQLiteBusy reports an error OpenCode's own writer caused by holding
// a lock past the busy timeout.
func isOpenCodeSQLiteBusy(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code() & 0xff { // the primary code of an extended one
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
		return true
	}
	return false
}

// openCodeSQLiteTables is the set of the store's table names.
func openCodeSQLiteTables(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables[name] = true
	}
	return tables, rows.Err()
}

// openCodeSQLiteNewestHead is the newest session head write in table, 0 when
// it is empty.
func openCodeSQLiteNewestHead(ctx context.Context, db *sql.DB, table string) (int64, error) {
	var ms int64
	err := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COALESCE(MAX(time_updated), 0) FROM %q`, table)).Scan(&ms)
	return ms, err
}

// openCodeSQLiteHasColumns reports whether table lists every column in want.
func openCodeSQLiteHasColumns(ctx context.Context, db *sql.DB, table string, want []string) (bool, error) {
	rows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return false, err
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
		return false, err
	}
	for _, column := range want {
		if !have[column] {
			return false, nil
		}
	}
	return true, nil
}

// checkOpenCodeSQLiteSchema fails closed unless every V1 table lists every
// column the V1 reader needs.
func checkOpenCodeSQLiteSchema(ctx context.Context, db *sql.DB) error {
	for table, want := range openCodeSQLiteColumns {
		ok, err := openCodeSQLiteHasColumns(ctx, db, table, want)
		if err != nil {
			return err
		}
		if !ok {
			return errOpenCodeDirectLayoutUnknown
		}
	}
	return nil
}

/* --------------------------------------------------------------------------
   One session's usage, for an owed debt
   -------------------------------------------------------------------------- */

// openCodeStoreSessionQuery is one owed debt's turn: the session's assistant
// messages created in [FromMs, ToMs]. ToMs 0 has no fixed end; UntilIdle ends
// the turn at the session's first V2 idle row created at or after FromMs too.
// A read with neither bound reads nothing (Unbounded): it would pay later turns
// of a resumed session.
type openCodeStoreSessionQuery struct {
	SessionID string
	FromMs    int64
	ToMs      int64
	UntilIdle bool
}

// openCodeStoreSessionRead is what the store says about one debt's turn.
type openCodeStoreSessionRead struct {
	// Layout is the family read; "" when there is no SQLite store to read.
	Layout string
	// Steps are the turn's complete messages, keyed as the export keys them.
	Steps []openCodeUsageStep
	// Incomplete: a message in the turn has no completed time yet.
	Incomplete bool
	// Truncated: a cap or the budget stopped the read before the turn's end.
	Truncated bool
	// Unbounded: the turn has no end to read up to yet.
	Unbounded bool
	// SessionKnown: a family holds the session — the elected one, else the
	// other: a V1 client writing after a managed V2 run wins the election
	// without holding that run's session. A debt from before an upgrade names
	// a session only V1 has. On distinct heads a family holds a session its
	// heads list; on shared heads, one its message table holds.
	SessionKnown bool
}

// readOpenCodeStoreSessionUsage reads one debt's turn from opencode.db, under
// the same read-only open, family election and caps as a scan. Each message is
// keyed sha256(session, "message:"+id) — the export's key — so the store and
// the export cannot pay one message twice. A JSON or absent store, and every
// read while the reader is off (tests), answer Layout "".
func readOpenCodeStoreSessionUsage(ctx context.Context, root string, q openCodeStoreSessionQuery) (openCodeStoreSessionRead, error) {
	var out openCodeStoreSessionRead
	if !openCodeDirectScanEnabled || root == "" {
		return out, nil
	}
	db, err := openOpenCodeSQLite(root)
	if err != nil {
		return out, err
	}
	defer db.Close()
	family, other, err := openCodeSQLiteFamily(ctx, db)
	if err != nil {
		return out, err
	}
	// One indexed lookup per family: the election follows the newest write
	// store-wide, but the debt's turn lives in whichever family wrote its
	// session. On distinct heads that is the family whose heads list it; on
	// shared heads both list every session, so it is the family whose message
	// table holds it. A session neither wrote has nothing to read, and says
	// nothing about whether the turn paid.
	shared := family.Heads == other.Heads
	for _, f := range []openCodeSQLiteFamilyRead{family, other} {
		if f.Layout == "" {
			continue
		}
		out.Layout = f.Layout
		var err error
		if shared {
			out.SessionKnown, err = openCodeSQLiteHoldsSession(ctx, db, f.MessageTable, q.SessionID)
		} else {
			err = db.QueryRowContext(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %q WHERE id = ?)`, f.Heads), q.SessionID).Scan(&out.SessionKnown)
		}
		if err != nil {
			return out, err
		}
		if out.SessionKnown {
			family = f
			break
		}
	}
	if !out.SessionKnown {
		out.Layout = family.Layout
		return out, nil
	}
	toMs := q.ToMs
	if q.UntilIdle && family.Layout == openCodeStoreLayoutSQLiteV2 {
		idleMs, err := openCodeSQLiteV2FirstIdle(ctx, db, q.SessionID, q.FromMs)
		if err != nil {
			return out, err
		}
		toMs = minNonZero(toMs, idleMs)
	}
	if toMs == 0 {
		out.Unbounded = true
		return out, nil
	}
	read := openCodeDirectRead{Layout: family.Layout}
	limits := openCodeDirectLimits{MaxSessions: 1, MaxRecords: openCodeDirectMaxRecords, MaxRecordBytes: openCodeDirectMaxRecordBytes}
	// A message is last written no earlier than it was created, so the floor
	// keeps every message of the turn; later turns are dropped below.
	if err := family.Messages(ctx, db, []any{q.SessionID}, q.FromMs, limits, &read); err != nil {
		return out, err
	}
	out.Truncated = read.Truncated
	for _, m := range read.Messages {
		if m.CreatedMs < q.FromMs || m.CreatedMs > toMs {
			continue
		}
		if m.CompletedMs == 0 {
			out.Incomplete = true
			continue
		}
		if !m.Valid {
			continue
		}
		step := m.Usage
		step.AtMs = m.CompletedMs
		step.Key = openCodeUsageStepKey(q.SessionID, "message:"+m.ID)
		out.Steps = append(out.Steps, step)
	}
	return out, nil
}
