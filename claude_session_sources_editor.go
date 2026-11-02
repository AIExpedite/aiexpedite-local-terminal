// claude_session_sources_editor.go — the Claude Code tabs VS Code, VS Code
// Insiders and Cursor remember, for session discovery
// (claude_session_discovery.go).
//
// Each editor keeps one SQLite database per window workspace at
// <userData>/User/workspaceStorage/<hash>/state.vscdb. The Claude Code
// extension stores, under ItemTable key `Anthropic.claude-code`:
//
//	{ "panelTabSessions": [{ "sessionId", "title", "fullEditor" }],
//	  "storedStartupConfig": { "cwd" } , … }
//
// and the window's folder sits beside it in workspace.json. Verified on
// macOS 2026-11-02; long tab titles are saved CUT SHORT with "…" (the merge
// prefers the session's own full title then).
//
// Undocumented storage: parsed through an allowlist. Opened read-only and
// immutable (never locks or writes the editor's file). A database that cannot
// be opened is `unreadable`; a value that is not the shape above is
// `unrecognised`; a missing key just means no Claude tabs in that window.
package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	claudeEditorStateKey     = "Anthropic.claude-code"
	claudeEditorBusyTimeout  = time.Second
	claudeEditorMaxWorkspace = 2000
)

// claudeEditorAppDirs maps a source to the editor's user-data folder name.
var claudeEditorAppDirs = map[string]string{
	cliSessionSourceVSCode:         "Code",
	cliSessionSourceVSCodeInsiders: "Code - Insiders",
	cliSessionSourceCursor:         "Cursor",
}

// defaultEditorUserDataRoots resolves each editor's user-data root for this OS.
func defaultEditorUserDataRoots(home string) map[string]string {
	base := ""
	switch runtime.GOOS {
	case "windows":
		base = os.Getenv("APPDATA")
		if base == "" && home != "" {
			base = filepath.Join(home, "AppData", "Roaming")
		}
	case "darwin":
		if home != "" {
			base = filepath.Join(home, "Library", "Application Support")
		}
	default:
		base = os.Getenv("XDG_CONFIG_HOME")
		if base == "" && home != "" {
			base = filepath.Join(home, ".config")
		}
	}
	roots := map[string]string{}
	if base == "" {
		return roots
	}
	for src, dir := range claudeEditorAppDirs {
		roots[src] = filepath.Join(base, dir)
	}
	return roots
}

// claudeEditorTab is one Claude Code tab an editor window remembers.
type claudeEditorTab struct {
	SessionID    string
	Title        string
	Cwd          string
	WindowFolder string
	Editor       string
	SeenAt       int64 // the state database's last write (ms)
}

type claudeEditorState struct {
	PanelTabSessions []struct {
		SessionID string `json:"sessionId"`
		Title     string `json:"title"`
	} `json:"panelTabSessions"`
	StoredStartupConfig *struct {
		Cwd string `json:"cwd"`
	} `json:"storedStartupConfig"`
}

var errClaudeEditorUnrecognised = errors.New("unrecognised editor state")

// claudeEditorReadOnlyDSN opens a state database read-only AND immutable, so
// SQLite takes no lock on a file the running editor owns.
func claudeEditorReadOnlyDSN(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // a Windows drive path: file:///C:/…
	}
	u := url.URL{Scheme: "file", Path: p}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Set("immutable", "1")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", claudeEditorBusyTimeout.Milliseconds()))
	q.Add("_pragma", "query_only(1)")
	u.RawQuery = q.Encode()
	return u.String()
}

// readEditorStateValue returns the extension's raw value ("" when the window
// has no Claude state).
func readEditorStateValue(dbPath string) (string, error) {
	db, err := sql.Open("sqlite", claudeEditorReadOnlyDSN(dbPath))
	if err != nil {
		return "", err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var value sql.NullString
	err = db.QueryRow(`SELECT value FROM ItemTable WHERE key = ?`, claudeEditorStateKey).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return value.String, nil
}

// parseEditorClaudeState reads the allowlisted fields of the extension's value.
func parseEditorClaudeState(raw string) (claudeEditorState, error) {
	var st claudeEditorState
	if strings.TrimSpace(raw) == "" {
		return st, nil
	}
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return st, errClaudeEditorUnrecognised
	}
	return st, nil
}

// editorWindowFolder reads workspace.json's folder (or workspace file) as a
// local path, "" when absent or not a file URI.
func editorWindowFolder(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "workspace.json"))
	if err != nil {
		return ""
	}
	var ws struct {
		Folder    string `json:"folder"`
		Workspace string `json:"workspace"`
	}
	if json.Unmarshal(b, &ws) != nil {
		return ""
	}
	raw := ws.Folder
	if raw == "" {
		raw = ws.Workspace
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	p := u.Path
	if runtime.GOOS == "windows" {
		p = strings.TrimPrefix(p, "/")
	}
	return filepath.FromSlash(p)
}

// readEditorClaudeTabs reads every window of one editor.
func readEditorClaudeTabs(source, root string) claudeSourceOutcome {
	out := claudeSourceOutcome{source: source}
	storage := filepath.Join(root, "User", "workspaceStorage")
	if root == "" {
		out.skip = cliSessionSkipNotFound
		return out
	}
	entries, err := os.ReadDir(storage)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			out.skip = cliSessionSkipNotFound
		} else {
			out.skip = cliSessionSkipUnreadable
		}
		return out
	}
	read, unreadable, unrecognised := 0, 0, 0
	for i, e := range entries {
		if i >= claudeEditorMaxWorkspace || !e.IsDir() {
			continue
		}
		dir := filepath.Join(storage, e.Name())
		dbPath := filepath.Join(dir, "state.vscdb")
		info, err := os.Stat(dbPath)
		if err != nil {
			continue // a window with no state database yet
		}
		raw, err := readEditorStateValue(dbPath)
		if err != nil {
			unreadable++
			continue
		}
		st, err := parseEditorClaudeState(raw)
		if err != nil {
			unrecognised++
			continue
		}
		read++
		if len(st.PanelTabSessions) == 0 {
			continue
		}
		cwd := ""
		if st.StoredStartupConfig != nil && filepath.IsAbs(st.StoredStartupConfig.Cwd) {
			cwd = st.StoredStartupConfig.Cwd
		}
		folder := editorWindowFolder(dir)
		if cwd == "" {
			cwd = folder
		}
		for _, t := range st.PanelTabSessions {
			if !isClaudeSessionID(t.SessionID) {
				continue
			}
			out.tabs = append(out.tabs, claudeEditorTab{
				SessionID:    t.SessionID,
				Title:        t.Title,
				Cwd:          cwd,
				WindowFolder: folder,
				Editor:       source,
				SeenAt:       info.ModTime().UnixMilli(),
			})
		}
	}
	out.checked = read > 0 || (unreadable == 0 && unrecognised == 0)
	switch {
	case unrecognised > 0:
		out.skip = cliSessionSkipUnrecognised
	case unreadable > 0:
		out.skip = cliSessionSkipUnreadable
	}
	return out
}
