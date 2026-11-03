// claude_session_sources_store.go — Claude Code's own session sources, for
// session discovery (claude_session_discovery.go) and the adoption probe
// (claude_session_probe.go).
//
//   - Running registry: <configDir>/sessions/<pid>.json, one per live Claude
//     process: { pid, sessionId, cwd, startedAt, procStart, entrypoint
//     ("claude-vscode" / "cli" / "sdk-cli"), name, nameSource, status
//     ("busy" / "waiting" / "idle"), updatedAt }. A file outlives a crashed
//     process, so an entry counts only while its pid is alive AND that pid's
//     start time matches `procStart` (a reused pid is a different process).
//   - Session store: <configDir>/projects/<slug>/<sessionId>.jsonl, one
//     transcript per session. Only its head and tail (≤64 KB each) are read,
//     and only allowlisted records: custom-title, ai-title, summary,
//     last-prompt, plus the first `user` record as the last fallback label.
//
// configDir honours CLAUDE_CONFIG_DIR (claudeConfigDir). Read-only.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	claudeTranscriptChunkBytes = 64 * 1024
	claudeRegistryMaxEntries   = 500
	claudeRegistryMaxFileBytes = 64 * 1024
	// A registry procStart within this much of the process's own start time is
	// the same process.
	claudeProcStartTolerance = 2 * time.Second
)

/* -------------------------------------------------------------------------- */
/* Running registry                                                           */
/* -------------------------------------------------------------------------- */

type claudeRegistryEntry struct {
	PID        int
	SessionID  string
	Cwd        string
	Status     string
	Entrypoint string
	Name       string
	NameSource string
	UpdatedAt  int64
}

type claudeRegistryFile struct {
	PID        int    `json:"pid"`
	SessionID  string `json:"sessionId"`
	Cwd        string `json:"cwd"`
	StartedAt  int64  `json:"startedAt"`
	ProcStart  string `json:"procStart"`
	Entrypoint string `json:"entrypoint"`
	Name       string `json:"name"`
	NameSource string `json:"nameSource"`
	Status     string `json:"status"`
	UpdatedAt  int64  `json:"updatedAt"`
}

// claudeProcessState is what this platform can say about a pid.
type claudeProcessState int

const (
	claudeProcessUnknown claudeProcessState = iota // alive or not, unprovable here
	claudeProcessGone
	claudeProcessAlive
)

// claudeProcessStart reports a pid's state and, when the platform can tell,
// its start time (unix ms; 0 when unknown). A seam for tests.
var claudeProcessStart = defaultClaudeProcessStart

func defaultClaudeProcessStart(pid int) (claudeProcessState, int64) {
	token, err := processStartToken(pid)
	if err != nil {
		if processStartTokenErrGone(err) {
			return claudeProcessGone, 0
		}
		return claudeProcessUnknown, 0
	}
	return claudeProcessAlive, processStartTokenUnixMs(token)
}

// processStartTokenUnixMs converts a process_identity start token to unix ms
// where the token is wall-clock based (macOS, Windows), 0 otherwise (Linux
// tokens count clock ticks since boot).
func processStartTokenUnixMs(token string) int64 {
	switch {
	case strings.HasPrefix(token, "darwin:"):
		secs, usecs, _ := strings.Cut(strings.TrimPrefix(token, "darwin:"), ".")
		s, err := strconv.ParseInt(secs, 10, 64)
		if err != nil {
			return 0
		}
		u, _ := strconv.ParseInt(usecs, 10, 64)
		return s*1000 + u/1000
	case strings.HasPrefix(token, "win:"):
		ticks, err := strconv.ParseUint(strings.TrimPrefix(token, "win:"), 10, 64)
		const epochDiff = 116444736000000000 // 1601-01-01 → 1970-01-01, in 100 ns
		if err != nil || ticks < epochDiff {
			return 0
		}
		return int64((ticks - epochDiff) / 10000)
	}
	return 0
}

// parseClaudeProcStart reads a registry `procStart` (a ctime-style UTC string
// on macOS, e.g. "Thu Oct 29 12:27:18 2026"), 0 when unrecognised.
func parseClaudeProcStart(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	for _, layout := range []string{time.ANSIC, time.UnixDate, time.RFC3339Nano, time.RFC3339} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t.UnixMilli()
		}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		return n
	}
	return 0
}

// claudeRegistryEntryLive applies the liveness rule. Where the platform cannot
// read a start time (Linux tokens, other platforms) a live pid is accepted on
// liveness alone; where it cannot even tell liveness, the entry is accepted
// (the file exists and terminal-service still re-probes before adopting).
func claudeRegistryEntryLive(f claudeRegistryFile) bool {
	state, startMs := claudeProcessStart(f.PID)
	switch state {
	case claudeProcessGone:
		return false
	case claudeProcessUnknown:
		return true
	}
	if startMs == 0 {
		return true
	}
	if want := parseClaudeProcStart(f.ProcStart); want != 0 {
		diff := time.Duration(abs64(startMs-want)) * time.Millisecond
		return diff <= claudeProcStartTolerance
	}
	if f.StartedAt > 0 {
		// Claude records startedAt shortly AFTER its process started.
		lag := time.Duration(f.StartedAt-startMs) * time.Millisecond
		return lag >= -claudeProcStartTolerance && lag <= 10*time.Minute
	}
	return true
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// readClaudeRegistryEntries reads every live registry entry.
func readClaudeRegistryEntries(configDir string) ([]claudeRegistryEntry, string) {
	if configDir == "" {
		return nil, cliSessionSkipNotFound
	}
	dir := filepath.Join(configDir, "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No running sessions ever recorded: nothing to report, not a failure.
			return nil, ""
		}
		return nil, cliSessionSkipUnreadable
	}
	out := []claudeRegistryEntry{}
	bad := 0
	for i, e := range entries {
		if i >= claudeRegistryMaxEntries {
			break
		}
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		f, err := readClaudeRegistryFile(filepath.Join(dir, name))
		if err != nil {
			bad++
			continue
		}
		if f.PID <= 0 || !isClaudeSessionID(f.SessionID) {
			bad++
			continue
		}
		if !claudeRegistryEntryLive(f) {
			continue
		}
		out = append(out, claudeRegistryEntry{
			PID:        f.PID,
			SessionID:  f.SessionID,
			Cwd:        f.Cwd,
			Status:     f.Status,
			Entrypoint: f.Entrypoint,
			Name:       f.Name,
			NameSource: f.NameSource,
			UpdatedAt:  f.UpdatedAt,
		})
	}
	if bad > 0 && len(out) == 0 {
		return out, cliSessionSkipUnrecognised
	}
	return out, ""
}

func readClaudeRegistryFile(path string) (claudeRegistryFile, error) {
	var f claudeRegistryFile
	fh, err := os.Open(path)
	if err != nil {
		return f, err
	}
	defer fh.Close()
	b, err := io.ReadAll(io.LimitReader(fh, claudeRegistryMaxFileBytes+1))
	if err != nil {
		return f, err
	}
	if len(b) > claudeRegistryMaxFileBytes {
		return f, errors.New("registry file too large")
	}
	return f, json.Unmarshal(b, &f)
}

func readClaudeRegistry(configDir string) claudeSourceOutcome {
	running, skip := readClaudeRegistryEntries(configDir)
	return claudeSourceOutcome{
		source:  cliSessionSourceClaudeRegistry,
		checked: skip == "" || len(running) > 0,
		skip:    skip,
		running: running,
	}
}

/* -------------------------------------------------------------------------- */
/* Session store                                                              */
/* -------------------------------------------------------------------------- */

// claudeTranscriptInfo is what discovery takes from one transcript.
type claudeTranscriptInfo struct {
	SessionID    string
	Path         string
	Cwd          string
	SavedTitle   string
	TitleSavedAt int64
	LastPrompt   string
	FirstPrompt  string
	ModifiedAt   int64
	Recognised   bool // at least one record of a known type was read
}

type claudeTranscriptRecord struct {
	Type        string          `json:"type"`
	CustomTitle string          `json:"customTitle"`
	AITitle     string          `json:"aiTitle"`
	Summary     string          `json:"summary"`
	LastPrompt  string          `json:"lastPrompt"`
	Cwd         string          `json:"cwd"`
	IsMeta      bool            `json:"isMeta"`
	Message     json.RawMessage `json:"message"`
}

// claudeRecordText returns a user/assistant record's plain text ("" for tool
// results and other non-text content).
func claudeRecordText(raw json.RawMessage) (role, text string) {
	if len(raw) == 0 {
		return "", ""
	}
	var msg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return "", ""
	}
	var s string
	if json.Unmarshal(msg.Content, &s) == nil {
		return msg.Role, s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(msg.Content, &parts) != nil {
		return msg.Role, ""
	}
	texts := []string{}
	for _, p := range parts {
		if p.Type == "text" && p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return msg.Role, strings.Join(texts, "\n")
}

// isClaudeSyntheticPrompt filters the command / caveat wrappers Claude Code
// writes as user records: never a label.
func isClaudeSyntheticPrompt(text string) bool {
	t := strings.TrimSpace(text)
	return t == "" || strings.HasPrefix(t, "<command-") || strings.HasPrefix(t, "<local-command") ||
		strings.HasPrefix(t, "Caveat:") || strings.HasPrefix(t, "[Request interrupted")
}

// claudeTranscriptChunks reads the head and tail of a transcript (≤64 KB
// each) as complete lines: a cut line at a chunk edge is dropped.
func claudeTranscriptChunks(path string, chunk int64) (head, tail []string, size int64, err error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, nil, 0, err
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil {
		return nil, nil, 0, err
	}
	size = info.Size()
	buf := make([]byte, minInt64(size, chunk))
	n, err := io.ReadFull(fh, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, nil, size, err
	}
	head = splitLines(buf[:n], false, size > chunk)
	if size > chunk {
		start := size - chunk
		if start < chunk {
			start = chunk // never re-read the head
		}
		tbuf := make([]byte, size-start)
		if _, err := fh.ReadAt(tbuf, start); err != nil && !errors.Is(err, io.EOF) {
			return head, nil, size, err
		}
		tail = splitLines(tbuf, start > chunk, false)
	}
	return head, tail, size, nil
}

// splitLines splits buf into lines, dropping a leading partial line (dropFirst)
// and a trailing partial one (dropLast).
func splitLines(buf []byte, dropFirst, dropLast bool) []string {
	lines := []string{}
	sc := bufio.NewScanner(bytes.NewReader(buf))
	sc.Buffer(make([]byte, 0, 64*1024), len(buf)+1)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if dropFirst && len(lines) > 0 {
		lines = lines[1:]
	}
	if dropLast && len(lines) > 0 && !bytes.HasSuffix(buf, []byte("\n")) {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// parseClaudeTranscript reads one transcript's allowlisted records.
func parseClaudeTranscript(sessionID, path string) (*claudeTranscriptInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	head, tail, _, err := claudeTranscriptChunks(path, claudeTranscriptChunkBytes)
	if err != nil {
		return nil, err
	}
	out := &claudeTranscriptInfo{SessionID: sessionID, Path: path, ModifiedAt: info.ModTime().UnixMilli()}
	var custom, ai, summary string
	for _, line := range append(head, tail...) {
		if !strings.HasPrefix(strings.TrimSpace(line), "{") {
			continue
		}
		var rec claudeTranscriptRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if out.Cwd == "" && filepath.IsAbs(rec.Cwd) {
			out.Cwd = rec.Cwd
		}
		switch rec.Type {
		case "custom-title":
			out.Recognised = true
			if rec.CustomTitle != "" {
				custom = rec.CustomTitle
			}
		case "ai-title":
			out.Recognised = true
			if rec.AITitle != "" {
				ai = rec.AITitle
			}
		case "summary":
			out.Recognised = true
			if rec.Summary != "" {
				summary = rec.Summary
			}
		case "last-prompt":
			out.Recognised = true
			if rec.LastPrompt != "" {
				out.LastPrompt = rec.LastPrompt
			}
		case "user", "assistant":
			out.Recognised = true
			if rec.Type == "user" && out.FirstPrompt == "" && !rec.IsMeta {
				if _, text := claudeRecordText(rec.Message); !isClaudeSyntheticPrompt(text) {
					out.FirstPrompt = text
				}
			}
		}
	}
	for _, t := range []string{custom, ai, summary} {
		if strings.TrimSpace(t) != "" {
			out.SavedTitle = t
			out.TitleSavedAt = out.ModifiedAt
			break
		}
	}
	return out, nil
}

// findClaudeTranscript locates a session's transcript across every project
// folder by its id (never by a caller-supplied path), parsed, or nil.
func findClaudeTranscript(configDir, sessionID string) *claudeTranscriptInfo {
	path := claudeTranscriptPath(configDir, sessionID)
	if path == "" {
		return nil
	}
	info, err := parseClaudeTranscript(sessionID, path)
	if err != nil {
		return nil
	}
	return info
}

// claudeTranscriptPath returns the newest <configDir>/projects/*/<id>.jsonl, "".
func claudeTranscriptPath(configDir, sessionID string) string {
	if configDir == "" || !isClaudeSessionID(sessionID) {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(configDir, "projects", "*", sessionID+".jsonl"))
	best, bestAt := "", time.Time{}
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && !fi.IsDir() && fi.ModTime().After(bestAt) {
			best, bestAt = m, fi.ModTime()
		}
	}
	return best
}

type claudeTranscriptFile struct {
	sessionID string
	path      string
	modified  time.Time
}

// readClaudeStore lists transcripts modified since `since`, newest first, and
// parses at most maxRows of them.
func readClaudeStore(ctx context.Context, configDir string, since time.Time, maxRows int) claudeSourceOutcome {
	out := claudeSourceOutcome{
		source: cliSessionSourceClaudeStore,
		store:  map[string]*claudeTranscriptInfo{},
		paths:  map[string]string{},
	}
	newest := map[string]time.Time{}
	if configDir == "" {
		out.skip = cliSessionSkipNotFound
		return out
	}
	root := filepath.Join(configDir, "projects")
	projects, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			out.checked = true // no history yet
			return out
		}
		out.skip = cliSessionSkipUnreadable
		return out
	}
	files := []claudeTranscriptFile{}
	for _, p := range projects {
		if !p.IsDir() || ctx.Err() != nil {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, p.Name()))
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			id := strings.TrimSuffix(name, ".jsonl")
			if !isClaudeSessionID(id) {
				continue
			}
			fi, err := e.Info()
			if err != nil {
				continue
			}
			path := filepath.Join(root, p.Name(), name)
			if at, seen := newest[id]; !seen || fi.ModTime().After(at) {
				newest[id] = fi.ModTime()
				out.paths[id] = path
			}
			if fi.ModTime().Before(since) {
				continue
			}
			files = append(files, claudeTranscriptFile{id, path, fi.ModTime()})
		}
	}
	out.pathsComplete = ctx.Err() == nil
	sort.Slice(files, func(i, j int) bool { return files[i].modified.After(files[j].modified) })
	if len(files) > maxRows {
		files = files[:maxRows]
		out.trunc = true
	}
	parsed, failed := 0, 0
	for _, f := range files {
		if ctx.Err() != nil {
			out.trunc = true
			break
		}
		if prev, ok := out.store[f.sessionID]; ok && prev.ModifiedAt >= f.modified.UnixMilli() {
			continue
		}
		info, err := parseClaudeTranscript(f.sessionID, f.path)
		if err != nil {
			failed++
			continue
		}
		parsed++
		out.store[f.sessionID] = info
	}
	out.checked = true
	if failed > 0 && parsed == 0 {
		out.skip = cliSessionSkipUnreadable
	}
	return out
}
