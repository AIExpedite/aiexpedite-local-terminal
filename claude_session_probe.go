// claude_session_probe.go — the pre-adoption probe of ONE Claude Code session
// (shared-constants cliSessionDiscovery.js), Claude Code's half of
// __cli_session_probe__ (cli_session_discovery.go dispatches it by engine).
//
//	__cli_session_probe__  args [engine, sessionId, cwd, includeHistory "1"|"0"]
//	                       answers whether the session still exists and can be
//	                       resumed, its title now, who else holds it (running
//	                       registry entries other than this agent's own
//	                       children), whether an editor tab has it open, when
//	                       its transcript was last written, and — only when
//	                       includeHistory is "1" (the adopt flow's one probe) —
//	                       up to 5 recent turns, masked and capped.
//
// The outside-writer watcher of a session resumed in place, Claude Code's and
// every other engine's, is cli_session_outside_writer.go.
//
// READ-ONLY: nothing here locks, writes or renames an editor or Claude Code
// file. The transcript is located by session id (never by the caller's cwd),
// so a probe can only read Claude's own store.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

const (
	// Prior turns carried into an adopted session (shared-constants priorTurns).
	cliSessionProbeMaxTurns = 5
	// One prior turn's text is cut to this many characters (after masking).
	cliSessionProbeTurnMaxChars = 2000
	// Bytes read from the transcript's tail for the recent turns.
	cliSessionProbeTailBytes = 256 * 1024
)

// userHomeDir is the home directory the agent runs as ("" when unknown).
func userHomeDir() string {
	home, _ := os.UserHomeDir()
	return home
}

// cliSessionDiscoveryCapability is the capabilitiesInfo value.
type cliSessionDiscoveryCapability struct {
	Engines  []string `json:"engines"`
	Protocol int      `json:"protocol"`
}

type cliSessionWriter struct {
	PID        int    `json:"pid"`
	Status     string `json:"status"`
	Entrypoint string `json:"entrypoint,omitempty"`
	Editor     string `json:"editor,omitempty"`
}

type cliSessionTurn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type cliSessionProbeResult struct {
	SessionID           string             `json:"sessionId"`
	Exists              bool               `json:"exists"`
	Resumable           bool               `json:"resumable"`
	Title               string             `json:"title"`
	TitleSource         string             `json:"titleSource"`
	Writers             []cliSessionWriter `json:"writers"`
	OpenInEditors       []string           `json:"openInEditors"`
	TranscriptWrittenAt int64              `json:"transcriptWrittenAt"`
	RecentTurns         []cliSessionTurn   `json:"recentTurns,omitempty"`
	// As on a discovery row (cliSessionRow.StartedVia / LastVia). Protocol 2.
	StartedVia string `json:"startedVia,omitempty"`
	LastVia    string `json:"lastVia,omitempty"`
	Error      string `json:"error,omitempty"`
}

type cliSessionOutsideWriter struct {
	State      string `json:"state"`
	Entrypoint string `json:"entrypoint,omitempty"`
	Editor     string `json:"editor,omitempty"`
	At         int64  `json:"at"`
}

// Claude Code's own `entrypoint` values for the apps that host it
// (shared-constants CLI_SESSION_ENTRYPOINTS). Undocumented storage: a value
// not listed here maps to no editor and is still reported raw.
const (
	claudeEntrypointVSCode  = "claude-vscode"
	claudeEntrypointDesktop = "claude-desktop"
)

// cliSessionEditorClaudeDesktop names the Claude desktop app as the program
// holding a session (shared-constants CODING_SESSION_STARTED_FROM.CLAUDE_DESKTOP).
// It is never a discovery source: the desktop app keeps no tab list this
// agent reads.
const cliSessionEditorClaudeDesktop = "claudeDesktop"

// claudeEditorForEntrypoint names the app a registry entrypoint stands for, ""
// when it is not one (cli, sdk-cli, …). The VS Code extension reports
// claude-vscode inside Insiders and Cursor too, so all three read as VS Code.
func claudeEditorForEntrypoint(entrypoint string) string {
	switch entrypoint {
	case claudeEntrypointVSCode:
		return cliSessionSourceVSCode
	case claudeEntrypointDesktop:
		return cliSessionEditorClaudeDesktop
	}
	return ""
}

// claudeOtherWriters are the live registry entries holding sessionID that are
// not one of this agent's own children.
func claudeOtherWriters(entries []claudeRegistryEntry, sessionID string, isOwnPID func(int) bool) []cliSessionWriter {
	out := []cliSessionWriter{}
	for _, e := range entries {
		if e.SessionID != sessionID || (isOwnPID != nil && isOwnPID(e.PID)) {
			continue
		}
		out = append(out, cliSessionWriter{
			PID: e.PID, Status: e.Status, Entrypoint: e.Entrypoint,
			Editor: claudeEditorForEntrypoint(e.Entrypoint),
		})
	}
	return out
}

// claudeRecentTurns reads the transcript tail's last n user/assistant text
// turns, masked and capped. Tool results and other content are skipped.
func claudeRecentTurns(path string, n int) []cliSessionTurn {
	head, tail, _, err := claudeTranscriptChunks(path, cliSessionProbeTailBytes)
	if err != nil {
		return nil
	}
	// A small transcript is all head (no tail is read); a large one keeps
	// its newest lines in the tail.
	tail = append(head, tail...)
	turns := []cliSessionTurn{}
	for i := len(tail) - 1; i >= 0 && len(turns) < n; i-- {
		var rec claudeTranscriptRecord
		if json.Unmarshal([]byte(tail[i]), &rec) != nil || rec.IsMeta {
			continue
		}
		if rec.Type != "user" && rec.Type != "assistant" {
			continue
		}
		role, text := claudeRecordText(rec.Message)
		if text == "" || (role == "user" && isClaudeSyntheticPrompt(text)) {
			continue
		}
		if role == "" {
			role = rec.Type
		}
		turns = append(turns, cliSessionTurn{Role: role, Text: cliSessionLabel(text, cliSessionProbeTurnMaxChars)})
	}
	// Oldest first.
	for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
		turns[i], turns[j] = turns[j], turns[i]
	}
	return turns
}

// probeClaudeSession answers the probe from env's sources.
func probeClaudeSession(ctx context.Context, env claudeDiscoveryEnv, sessionID string, includeHistory bool) cliSessionProbeResult {
	res := cliSessionProbeResult{SessionID: sessionID, Writers: []cliSessionWriter{}, OpenInEditors: []string{}}
	if !isClaudeSessionID(sessionID) {
		res.Error = cliSessionErrBadRequest
		return res
	}
	entries, _ := readClaudeRegistryEntries(env.configDir)
	res.Writers = claudeOtherWriters(entries, sessionID, env.isOwnPID)

	var tabs []claudeEditorTab
	for source, root := range env.editorRoots {
		out := readEditorClaudeTabs(ctx, source, root, env.now.Add(-env.lookBack))
		for _, t := range out.tabs {
			if t.SessionID == sessionID {
				tabs = append(tabs, t)
				res.OpenInEditors = appendUnique(res.OpenInEditors, t.Editor)
			}
		}
	}

	path := claudeTranscriptPath(env.configDir, sessionID)
	var info *claudeTranscriptInfo
	unrecognised := false
	if path != "" {
		if fi, err := os.Stat(path); err == nil {
			res.TranscriptWrittenAt = fi.ModTime().UnixMilli()
		}
		var err error
		info, err = parseClaudeTranscript(sessionID, path)
		unrecognised = errors.Is(err, errClaudeTranscriptUnrecognised)
	}
	// An unrecognised transcript still exists but is not resumable, so the
	// adopt flow answers NOT_RESUMABLE rather than CANDIDATE_GONE.
	res.Exists = info != nil || unrecognised || len(res.Writers) > 0
	res.Resumable = info != nil && filepath.IsAbs(path)
	var running []claudeRegistryEntry
	for _, e := range entries {
		if e.SessionID == sessionID {
			running = append(running, e)
		}
	}
	res.Title, res.TitleSource, _ = claudeSessionTitle(info, tabs, running)
	if info != nil {
		res.StartedVia, res.LastVia = info.FirstEntrypoint, info.LastEntrypoint
	}
	if includeHistory && info != nil {
		res.RecentTurns = claudeRecentTurns(path, cliSessionProbeMaxTurns)
	}
	return res
}

func appendUnique(list []string, v string) []string {
	if v == "" {
		return list
	}
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}
