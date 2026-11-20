// cli_session_sources.go — what the readers of the engines without editor
// tabs share (codex_session_sources.go, grok_session_sources.go,
// antigravity_session_sources.go, opencode_session_sources.go,
// musecode_session_sources.go; the registry is cli_session_discovery.go):
// the session id shape, the title precedence and its masked, capped labels,
// the stores' timestamp spellings, a bounded head read of a JSONL file, and
// a read-only SQLite open that never leaves a file behind. Read-only.
package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

// errCliSessionUnrecognised is a readable session record in a shape a reader
// does not recognise: reported `unrecognised`, never guessed at.
var errCliSessionUnrecognised = errors.New("unrecognised session record")

// cliSessionUUIDPattern is the id shape Codex, Grok Build, Antigravity and
// Muse Code give a session (and nothing that could walk a path or be a flag).
var cliSessionUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func isCliSessionUUID(id string) bool { return cliSessionUUIDPattern.MatchString(id) }

// cliSessionListingContext is the context a reader lists its store under (the
// rollouts, folders or databases it then reads): it ends halfway through what
// is left of ctx, so a listing the budget cuts short still leaves the reader
// time to read the sessions it did list, and the answer is those, marked
// truncated, rather than none. Without a deadline on ctx the listing simply
// shares its cancellation.
func cliSessionListingContext(ctx context.Context) (context.Context, context.CancelFunc) {
	cliSessionListingContextMu.RLock()
	derive := cliSessionListingContextOf
	cliSessionListingContextMu.RUnlock()
	return derive(ctx)
}

// cliSessionListingContextOf is cliSessionListingContext's source, swappable
// by a test under its lock (a reader runs on its own goroutine).
var (
	cliSessionListingContextMu sync.RWMutex
	cliSessionListingContextOf = func(ctx context.Context) (context.Context, context.CancelFunc) {
		deadline, ok := ctx.Deadline()
		if !ok {
			return context.WithCancel(ctx)
		}
		return context.WithDeadline(ctx, deadline.Add(-time.Until(deadline)/2))
	}
)

// cliSessionPromptLabel is a prompt as a fallback label: masked, one line,
// cut to fallbackLabelMaxChars.
func cliSessionPromptLabel(prompt string) string {
	return cliSessionLabel(prompt, cliSessionFallbackLabelMaxChars)
}

// cliSessionTitleOf applies the precedence every reader without editor tabs
// shares: the title the agent itself shows for the session (savedTitle) —
// unless it is only the start of the session's first prompt, which some
// agents save as the title, and which then reads as the prompt it is — then
// the last prompt, then the first, then none.
func cliSessionTitleOf(saved string, savedAt int64, lastPrompt, firstPrompt string) (title, source string, savedAtMs int64) {
	saved = strings.TrimSpace(saved)
	if saved != "" && !cliSessionTitleIsPromptStart(saved, firstPrompt) {
		return cliSessionLabel(saved, cliSessionTitleMaxChars), cliSessionTitleSavedTitle, savedAt
	}
	if lastPrompt = strings.TrimSpace(lastPrompt); lastPrompt != "" {
		return cliSessionPromptLabel(lastPrompt), cliSessionTitleLastPrompt, 0
	}
	if firstPrompt = strings.TrimSpace(firstPrompt); firstPrompt != "" {
		return cliSessionPromptLabel(firstPrompt), cliSessionTitleFirstPrompt, 0
	}
	return "", cliSessionTitleNone, 0
}

// cliSessionTitleIsPromptStart reports whether title is the prompt's own
// beginning (whitespace and case aside, a trailing "…" or "..." dropped).
func cliSessionTitleIsPromptStart(title, prompt string) bool {
	norm := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
	t := norm(title)
	t = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(t, "…"), "..."))
	return t != "" && strings.HasPrefix(norm(prompt), t)
}

// cliSessionSyntheticPrompt reports text an agent writes where a person's
// prompt goes (a context block like <environment_context>, an AGENTS.md
// preamble): never a label.
func cliSessionSyntheticPrompt(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || strings.HasPrefix(t, "# AGENTS.md") {
		return true
	}
	if !strings.HasPrefix(t, "<") {
		return false
	}
	end := strings.IndexByte(t, '>')
	if end < 2 {
		return false
	}
	for _, r := range t[1:end] {
		if r != '_' && r != '-' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// cliSessionTimeLayouts are the timestamp spellings the stores use: RFC 3339
// (Codex, Grok Build, Muse Code) and SQLite's datetime with an offset
// (Antigravity's conversation_summaries).
var cliSessionTimeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999Z07:00",
}

// parseCliSessionTime is a store's timestamp as unix ms, 0 when it is none of
// the known spellings.
func parseCliSessionTime(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	for _, layout := range cliSessionTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

// cliSessionEpochMs reads a number as unix time: seconds below 1e11 (until
// the year 5138), milliseconds above. 0 for anything not positive.
func cliSessionEpochMs(n float64) int64 {
	switch {
	case n <= 0:
		return 0
	case n < 1e11:
		return int64(n * 1000)
	default:
		return int64(n)
	}
}

// parseCliSessionTimeRaw is a JSON timestamp (an RFC 3339 string or a unix
// number) as unix ms, 0 when it is neither.
func parseCliSessionTimeRaw(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return parseCliSessionTime(s)
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return cliSessionEpochMs(n)
	}
	return 0
}

// cliSessionSQLiteDSN opens another program's SQLite store read-only
// (mode=ro, query_only, a short busy timeout) without creating, writing or
// locking anything of its owner's:
//
//   - while the owner's write-ahead log is live beside the store (its -wal
//     and -shm both exist: the owner runs in WAL mode), the store is read
//     through that log, so the writes the owner has not checkpointed yet are
//     seen. A WAL reader's lock never blocks the owner's writer, and a
//     read-only connection never checkpoints;
//   - otherwise the store is read `immutable`, as the editors' databases are
//     (claude_session_sources_editor.go): no lock is taken, so a rollback-
//     journal owner's commit never waits on a discovery (one landing
//     mid-read can spoil only that read: unreadable, or a row a moment
//     stale). Without a live log the file holds every committed write — but
//     for a log a crash left behind, which the owner's next start recovers —
//     and a plain read-only open of a WAL store whose owner is not running
//     would CREATE its -wal and -shm (SQLite opens them read-write to read
//     through them) and leave both behind in the owner's folder.
func cliSessionSQLiteDSN(path string) string {
	dsn := openCodeSQLiteReadOnlyDSN(path)
	if !cliSessionSQLiteWALLive(path) {
		dsn += "&immutable=1"
	}
	return dsn
}

// cliSessionSQLiteWALLive reports whether path's owner keeps a write-ahead log
// beside it: both path-wal and path-shm are there.
func cliSessionSQLiteWALLive(path string) bool {
	for _, side := range []string{"-wal", "-shm"} {
		if fi, err := os.Lstat(path + side); err != nil || !fi.Mode().IsRegular() {
			return false
		}
	}
	return true
}

// openCliSessionSQLite opens path read-only, one connection; nil and an error
// when it is not a database this driver can read.
func openCliSessionSQLite(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", cliSessionSQLiteDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// sqliteColumns is the set of table's column names (empty when there is no
// such table, or when it could not be read: sqliteTableColumns tells the two
// apart).
func sqliteColumns(ctx context.Context, db *sql.DB, table string) map[string]bool {
	cols, _ := sqliteTableColumns(ctx, db, table)
	return cols
}

// sqliteTableColumns is sqliteColumns with the read's error: empty and nil
// when there is no such table, an error when the database could not be read
// for it (a file the driver cannot parse, an I/O error, ctx). The columns it
// did read are returned either way.
func sqliteTableColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	cols := map[string]bool{}
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT name FROM pragma_table_info(%s)`, sqliteQuote(table)))
	if err != nil {
		return cols, err
	}
	defer rows.Close()
	var scanErr error
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			if scanErr == nil {
				scanErr = err
			}
			continue
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		return cols, err
	}
	return cols, scanErr
}

// sqliteQuote is s as an SQL string literal.
func sqliteQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// hasAll reports whether set holds every name.
func hasAll(set map[string]bool, names ...string) bool {
	for _, n := range names {
		if !set[n] {
			return false
		}
	}
	return true
}

// readJSONLHead calls visit with each line of path's head — at most maxBytes
// and maxLines — until visit returns false. A line the byte cap cuts is
// handed over cut, so it never parses as a record.
func readJSONLHead(path string, maxBytes int64, maxLines int, visit func(line []byte) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(io.LimitReader(f, maxBytes), 64*1024)
	for n := 0; n < maxLines; n++ {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && (err == nil || errors.Is(err, io.EOF)) {
			if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 && !visit(trimmed) {
				return nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
	return nil
}

// cliSessionPathFromURI is a file:// URI's local path ("" for another scheme).
func cliSessionPathFromURI(uri string) string {
	u, err := url.Parse(strings.TrimSpace(uri))
	if err != nil || u.Scheme != "file" || u.Path == "" {
		return ""
	}
	p := u.Path
	// file:///C:/work → /C:/work on Windows.
	if len(p) > 2 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return filepath.FromSlash(p)
}
