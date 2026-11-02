// claude_session_probe.go — the pre-adoption probe of ONE Claude Code session
// and the outside-writer watcher of an adopted one (shared-constants
// cliSessionDiscovery.js).
//
//	__cli_session_probe__  args [engine, sessionId, cwd, includeHistory "1"|"0"]
//	                       answers whether the session still exists and can be
//	                       resumed, its title now, who else holds it (running
//	                       registry entries other than this agent's own
//	                       children), whether an editor tab has it open, when
//	                       its transcript was last written, and — only on the
//	                       final pre-start probe — up to 5 recent turns,
//	                       masked and capped.
//	watcher                for every claude_native session started with a
//	                       `--resume=<id>`, poll the running registry every 5 s
//	                       and publish claude_native_outside_writer only when
//	                       the state another process holds changes.
//
// Both are READ-ONLY: nothing here locks, writes or renames an editor or
// Claude Code file. The transcript is located by session id (never by the
// caller's cwd), so a probe can only read Claude's own store.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"cloud.google.com/go/pubsub/v2"
)

const (
	// Prior turns carried into an adopted session (shared-constants priorTurns).
	cliSessionProbeMaxTurns = 5
	// One prior turn's text is cut to this many characters (after masking).
	cliSessionProbeTurnMaxChars = 2000
	// Bytes read from the transcript's tail for the recent turns.
	cliSessionProbeTailBytes = 256 * 1024
	// The watcher's poll interval.
	claudeOutsideWriterPollInterval = 5 * time.Second
)

// Outside-writer states (shared-constants CLI_SESSION_OUTSIDE_WRITER_STATE).
const (
	claudeOutsideWriterBusy = "busy"
	claudeOutsideWriterIdle = "idle"
	claudeOutsideWriterGone = "gone"
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
	Error               string             `json:"error,omitempty"`
}

type cliSessionOutsideWriter struct {
	State      string `json:"state"`
	Entrypoint string `json:"entrypoint,omitempty"`
	Editor     string `json:"editor,omitempty"`
	At         int64  `json:"at"`
}

// claudeEditorForEntrypoint names the editor a registry entrypoint stands for,
// "" when it is not an editor (cli, sdk-cli, …).
func claudeEditorForEntrypoint(entrypoint string) string {
	if entrypoint == "claude-vscode" {
		return cliSessionSourceVSCode
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
	if path != "" {
		if fi, err := os.Stat(path); err == nil {
			res.TranscriptWrittenAt = fi.ModTime().UnixMilli()
		}
		info, _ = parseClaudeTranscript(sessionID, path)
	}
	res.Exists = info != nil || len(res.Writers) > 0
	res.Resumable = info != nil && filepath.IsAbs(path)
	var running []claudeRegistryEntry
	for _, e := range entries {
		if e.SessionID == sessionID {
			running = append(running, e)
		}
	}
	res.Title, res.TitleSource, _ = claudeSessionTitle(info, tabs, running)
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

// parseProbeArgs reads [engine, sessionId, cwd, includeHistory].
func parseProbeArgs(args []string) (engine, sessionID, cwd string, includeHistory bool) {
	engine = cliSessionEngineClaudeCode
	if len(args) > 0 && args[0] != "" {
		engine = args[0]
	}
	if len(args) > 1 {
		sessionID = args[1]
	}
	if len(args) > 2 {
		cwd = args[2]
	}
	if len(args) > 3 {
		includeHistory = args[3] == "1"
	}
	return
}

// handleCliSessionProbeCommand ALWAYS publishes one correlated result, even
// after a panic, so terminal-service's pending probe never sticks.
func handleCliSessionProbeCommand(ctx context.Context, topic *pubsub.Publisher, cmd commandMsg, cfg *Config) (publishErr error) {
	engine, sessionID, _, includeHistory := parseProbeArgs(cmd.Args)
	publish := func(payload cliSessionProbeResult, status string) error {
		res := cliSessionResultBase(cmd, cfg, cliSessionProbeResultType)
		res.Status = status
		res.Probe = &payload
		return publishMsg(ctx, topic, res)
	}
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("%s[pubsub] panic in cli session probe: %v%s\n", colorRed, r, colorReset)
			publishErr = publish(cliSessionProbeResult{
				SessionID: sessionID, Writers: []cliSessionWriter{}, OpenInEditors: []string{},
				Error: cliSessionErrInternal,
			}, "error")
		}
	}()
	if engine != cliSessionEngineClaudeCode {
		return publish(cliSessionProbeResult{
			SessionID: sessionID, Writers: []cliSessionWriter{}, OpenInEditors: []string{},
			Error: cliSessionErrEngineNotSupported,
		}, "error")
	}
	probeCtx, cancel := context.WithTimeout(ctx, cliSessionDiscoverBudget)
	defer cancel()
	res := probeClaudeSession(probeCtx, defaultClaudeDiscoveryEnv(0, 0), sessionID, includeHistory)
	status := "success"
	if res.Error != "" {
		status = "error"
	}
	if err := publish(res, status); err != nil {
		fmt.Printf("%s[pubsub] Failed to publish cli session probe: %v%s\n", colorRed, err, colorReset)
		return err
	}
	return nil
}

/* -------------------------------------------------------------------------- */
/* Outside-writer watcher                                                     */
/* -------------------------------------------------------------------------- */

// claudeOutsideWriterState is the state another process holds sessionID in:
// busy when any writer is mid-turn (an unknown status counts as busy), idle
// when every writer is idle, gone when none holds it.
func claudeOutsideWriterState(writers []cliSessionWriter) (state, entrypoint, editor string) {
	if len(writers) == 0 {
		return claudeOutsideWriterGone, "", ""
	}
	state = claudeOutsideWriterIdle
	for _, w := range writers {
		if w.Status != "idle" {
			state = claudeOutsideWriterBusy
		}
		if entrypoint == "" {
			entrypoint, editor = w.Entrypoint, w.Editor
		}
	}
	return state, entrypoint, editor
}

// claudeOutsideWriterWatch polls `poll` every interval until stop closes and
// calls emit whenever the outside-writer state changes. The first observation
// is emitted only when someone else already holds the session (a resumed
// session that starts alone has nothing to report).
func claudeOutsideWriterWatch(stop <-chan struct{}, interval time.Duration, poll func() []cliSessionWriter, emit func(cliSessionOutsideWriter)) {
	last := claudeOutsideWriterGone
	check := func() {
		state, entrypoint, editor := claudeOutsideWriterState(poll())
		if state == last {
			return
		}
		last = state
		emit(cliSessionOutsideWriter{State: state, Entrypoint: entrypoint, Editor: editor, At: time.Now().UnixMilli()})
	}
	check()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			check()
		}
	}
}

// startClaudeOutsideWriterWatcher watches a resumed claude_native session for
// another process holding the same Claude session id, until it exits.
func startClaudeOutsideWriterWatcher(session *ClaudeNativeSession, claudeSessionID string, publishFn PublishFunc) {
	if session == nil || publishFn == nil || !isClaudeSessionID(claudeSessionID) {
		return
	}
	configDir := claudeConfigDir(userHomeDir())
	go claudeOutsideWriterWatch(session.processExited, claudeOutsideWriterPollInterval,
		func() []cliSessionWriter {
			entries, _ := readClaudeRegistryEntries(configDir)
			return claudeOtherWriters(entries, claudeSessionID, isClaudeOwnPID)
		},
		func(w cliSessionOutsideWriter) {
			publishFn(resultMsg{
				ID:            session.ID,
				WorkspaceID:   session.WorkspaceID,
				UID:           session.UID,
				Status:        "success",
				Ts:            time.Now().UnixMilli(),
				Version:       Version,
				Type:          claudeOutsideWriterResultType,
				SessionID:     session.ID,
				OutsideWriter: &w,
			})
		})
}
