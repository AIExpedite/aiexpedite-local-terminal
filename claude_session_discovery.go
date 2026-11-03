// claude_session_discovery.go — answers terminal-service's
// __cli_sessions_discover__ command: the Claude Code sessions this computer
// knows that AI Expedite may NOT have started (a tab in VS Code / Insiders /
// Cursor, a session started in a terminal), by title, so the owner can adopt
// one into the web chat without knowing its session id.
//
// Contract: shared-constants src/constants/cliSessionDiscovery.js
// (CLI_SESSION_DEVICE_COMMANDS / CLI_SESSION_DEVICE_RESULT_TYPES, sources,
// title sources, skip reasons). terminal-service matches titles; this file
// only reports what each local source says.
//
// READ-ONLY by construction: every source is opened read-only (SQLite
// `mode=ro&immutable=1`, plain os.Open on files) and nothing is ever written,
// locked or renamed. The editors' storage is not a documented interface, so
// every reader parses through an allowlist and reports a source it cannot
// read as skipped (`unreadable` / `unrecognised` / `timeout`) rather than
// guessing; one bad source never fails the request.
//
// Privacy: a row carries a title or a short, masked prompt-based fallback
// label — never conversation content, tool output or file contents.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/pubsub/v2"
)

// Device protocol (shared-constants CLI_SESSION_DEVICE_COMMANDS / _RESULT_TYPES).
const (
	cliSessionsDiscoverCommand    = "__cli_sessions_discover__"
	cliSessionProbeCommand        = "__cli_session_probe__"
	cliSessionsDiscoverResultType = "__cli_sessions_discover_result__"
	cliSessionProbeResultType     = "__cli_session_probe_result__"
	claudeOutsideWriterResultType = "claude_native_outside_writer"
)

// The one engine this build discovers (shared-constants CLI_SESSION_ENGINES).
const cliSessionEngineClaudeCode = "claudeCode"

// cliSessionDiscoveryProtocol is advertised in capabilitiesInfo.
const cliSessionDiscoveryProtocol = 1

// Sources (shared-constants CLI_SESSION_SOURCES).
const (
	cliSessionSourceVSCode         = "vscode"
	cliSessionSourceVSCodeInsiders = "vscodeInsiders"
	cliSessionSourceCursor         = "cursor"
	cliSessionSourceClaudeRegistry = "claudeRegistry"
	cliSessionSourceClaudeStore    = "claudeStore"
)

// Title sources (shared-constants CLI_SESSION_TITLE_SOURCES). "none" is not in
// the shared list on purpose: readers treat any unknown value as a fallback.
const (
	cliSessionTitleEditorTab   = "editorTab"
	cliSessionTitleSavedTitle  = "savedTitle"
	cliSessionTitleLastPrompt  = "lastPrompt"
	cliSessionTitleFirstPrompt = "firstPrompt"
	cliSessionTitleNone        = "none"
)

// Skip reasons (shared-constants CLI_SESSION_SOURCE_SKIP_REASONS).
const (
	cliSessionSkipNotFound     = "notFound"
	cliSessionSkipUnreadable   = "unreadable"
	cliSessionSkipUnrecognised = "unrecognised"
	cliSessionSkipTimeout      = "timeout"
)

// Error codes on a result that could not run at all.
const (
	cliSessionErrEngineNotSupported = "engineNotSupported"
	cliSessionErrBadRequest         = "badRequest"
	cliSessionErrInternal           = "internal"
)

const (
	// Bounds (shared-constants CLI_SESSION_DISCOVERY_LIMITS). The server sends
	// its own values; these are the defaults and the ceilings.
	cliSessionDefaultLookBackDays = 30
	cliSessionMaxLookBackDays     = 365
	cliSessionDefaultMaxRows      = 500
	cliSessionMaxRows             = 500
	// The whole discovery answers inside this budget (terminal-service waits 8 s).
	cliSessionDiscoverBudget = 6 * time.Second
	// A prompt-based fallback label is cut to this many characters (after masking).
	cliSessionFallbackLabelMaxChars = 80
	// A title of any kind is cut to this many characters.
	cliSessionTitleMaxChars = 200
	// Keeps the published result well under the Pub/Sub message ceiling.
	cliSessionMaxResultBytes = 900 * 1024
	// Sessions named by an editor tab or a running process but outside the
	// parsed store window are looked up individually, at most this many.
	cliSessionMaxExtraLookups = 200
)

// claudeDefaultSessionTitle is the title every new Claude Code tab starts with
// (shared-constants DEFAULT_CLI_SESSION_TITLE): never treated as a real title.
const claudeDefaultSessionTitle = "Claude Code"

// claudeSessionIDPattern accepts Claude's UUID session ids (and nothing that
// could walk a path).
var claudeSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{7,63}$`)

func isClaudeSessionID(id string) bool { return claudeSessionIDPattern.MatchString(id) }

/* -------------------------------------------------------------------------- */
/* Wire types                                                                 */
/* -------------------------------------------------------------------------- */

type cliSessionRunning struct {
	Status     string `json:"status"`
	Entrypoint string `json:"entrypoint,omitempty"`
	PID        int    `json:"pid"`
}

type cliSessionEditorRef struct {
	Editor       string `json:"editor"`
	WindowFolder string `json:"windowFolder,omitempty"`
	SeenAt       int64  `json:"seenAt"`
}

type cliSessionRow struct {
	SessionID           string                `json:"sessionId"`
	Cwd                 string                `json:"cwd,omitempty"`
	Title               string                `json:"title"`
	TitleSource         string                `json:"titleSource"`
	TitleSavedAt        int64                 `json:"titleSavedAt"`
	LastActiveAt        int64                 `json:"lastActiveAt"`
	Running             *cliSessionRunning    `json:"running"`
	Editors             []cliSessionEditorRef `json:"editors"`
	StartedByAiExpedite bool                  `json:"startedByAiExpedite"`
	Resumable           bool                  `json:"resumable"`
}

type cliSessionSkip struct {
	Source string `json:"source"`
	Reason string `json:"reason"`
}

type cliSessionsDiscoverResult struct {
	Engine         string           `json:"engine"`
	Rows           []cliSessionRow  `json:"rows"`
	SourcesChecked []string         `json:"sourcesChecked"`
	SourcesSkipped []cliSessionSkip `json:"sourcesSkipped"`
	Truncated      bool             `json:"truncated"`
	ScannedAt      int64            `json:"scannedAt"`
	Error          string           `json:"error,omitempty"`
}

/* -------------------------------------------------------------------------- */
/* Sessions this agent started                                                */
/* -------------------------------------------------------------------------- */

// claudeOwnSessions records the Claude session ids claude_native drove (its
// resume argument and the init frame's session_id), and the pids of its live
// children, so discovery flags `startedByAiExpedite` and the probe / watcher
// never count AI Expedite's own process as "another writer". In memory only:
// terminal-service also knows its own sessions from the ledger.
var claudeOwnSessions = struct {
	sync.Mutex
	ids  map[string]bool
	pids map[int]bool
}{ids: map[string]bool{}, pids: map[int]bool{}}

func recordClaudeOwnSessionID(id string) {
	if !isClaudeSessionID(id) {
		return
	}
	claudeOwnSessions.Lock()
	defer claudeOwnSessions.Unlock()
	if len(claudeOwnSessions.ids) > 5000 {
		claudeOwnSessions.ids = map[string]bool{}
	}
	claudeOwnSessions.ids[id] = true
}

func isClaudeOwnSessionID(id string) bool {
	claudeOwnSessions.Lock()
	defer claudeOwnSessions.Unlock()
	return claudeOwnSessions.ids[id]
}

func recordClaudeOwnPID(pid int, live bool) {
	if pid <= 0 {
		return
	}
	claudeOwnSessions.Lock()
	defer claudeOwnSessions.Unlock()
	if live {
		claudeOwnSessions.pids[pid] = true
	} else {
		delete(claudeOwnSessions.pids, pid)
	}
}

func isClaudeOwnPID(pid int) bool {
	claudeOwnSessions.Lock()
	defer claudeOwnSessions.Unlock()
	return claudeOwnSessions.pids[pid]
}

// claudeResumeIDFromArgs returns the session id a `--resume=<id>` /
// `--resume <id>` argument names, or "".
func claudeResumeIDFromArgs(args []string) string {
	for i, a := range args {
		if strings.HasPrefix(a, "--resume=") {
			return strings.TrimPrefix(a, "--resume=")
		}
		if a == "--resume" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// claudeInitSessionID returns the session_id of a stream-json system/init
// frame, or "" for any other line.
func claudeInitSessionID(line string) string {
	if !strings.Contains(line, `"init"`) {
		return ""
	}
	var frame struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal([]byte(line), &frame) != nil || frame.Type != "system" || frame.Subtype != "init" {
		return ""
	}
	return frame.SessionID
}

/* -------------------------------------------------------------------------- */
/* Labels                                                                     */
/* -------------------------------------------------------------------------- */

// cliSessionLabel masks credentials, collapses whitespace to one line and caps
// the text at max runes ("…" when cut).
func cliSessionLabel(text string, max int) string {
	if text == "" {
		return ""
	}
	masked := redactAgentSecrets(text)
	line := strings.Join(strings.Fields(masked), " ")
	if utf8.RuneCountInString(line) <= max {
		return line
	}
	runes := []rune(line)
	return strings.TrimSpace(string(runes[:max-1])) + "…"
}

func isDefaultClaudeTitle(title string) bool {
	return strings.EqualFold(strings.Join(strings.Fields(title), " "), claudeDefaultSessionTitle)
}

// editorTitleExtends reports whether full is the untruncated form of an editor
// tab title VS Code saved cut short ("PR #284 feedback iterati…").
func editorTitleExtends(tabTitle, full string) bool {
	prefix, cut := strings.CutSuffix(strings.TrimSpace(tabTitle), "…")
	if !cut {
		return false
	}
	prefix = strings.ToLower(strings.Join(strings.Fields(prefix), " "))
	f := strings.ToLower(strings.Join(strings.Fields(full), " "))
	return prefix != "" && strings.HasPrefix(f, prefix) && len(f) > len(prefix)
}

/* -------------------------------------------------------------------------- */
/* Discovery                                                                  */
/* -------------------------------------------------------------------------- */

// claudeDiscoveryEnv is everything discovery reads from, injectable for tests.
type claudeDiscoveryEnv struct {
	configDir    string            // Claude's config dir (~/.claude or CLAUDE_CONFIG_DIR)
	editorRoots  map[string]string // source -> the editor's user-data root ("…/Code")
	now          time.Time
	lookBack     time.Duration
	maxRows      int
	isOwnSession func(string) bool
	isOwnPID     func(int) bool
}

func defaultClaudeDiscoveryEnv(lookBackDays, maxRows int) claudeDiscoveryEnv {
	if lookBackDays <= 0 {
		lookBackDays = cliSessionDefaultLookBackDays
	}
	if lookBackDays > cliSessionMaxLookBackDays {
		lookBackDays = cliSessionMaxLookBackDays
	}
	if maxRows <= 0 || maxRows > cliSessionMaxRows {
		maxRows = cliSessionDefaultMaxRows
	}
	home := userHomeDir()
	return claudeDiscoveryEnv{
		configDir:    claudeConfigDir(home),
		editorRoots:  defaultEditorUserDataRoots(home),
		now:          time.Now(),
		lookBack:     time.Duration(lookBackDays) * 24 * time.Hour,
		maxRows:      maxRows,
		isOwnSession: isClaudeOwnSessionID,
		isOwnPID:     isClaudeOwnPID,
	}
}

// claudeSourceOutcome is one source reader's answer.
type claudeSourceOutcome struct {
	source  string
	checked bool
	skip    string // a skip reason, "" when the source read cleanly
	tabs    []claudeEditorTab
	running []claudeRegistryEntry
	store   map[string]*claudeTranscriptInfo
	// Every transcript the store scan listed (any age): session id -> newest
	// path. Lets a session an editor or process names be found without
	// another walk of every project folder.
	paths map[string]string
	// pathsComplete: the listing finished, so an id missing from paths has no
	// transcript at all.
	pathsComplete bool
	trunc         bool
}

// discoverClaudeSessions runs every source under ctx, merges by session id and
// returns the result rows newest first, capped at env.maxRows.
func discoverClaudeSessions(ctx context.Context, env claudeDiscoveryEnv) cliSessionsDiscoverResult {
	type job struct {
		source string
		run    func() claudeSourceOutcome
	}
	jobs := []job{}
	for _, src := range []string{cliSessionSourceVSCode, cliSessionSourceVSCodeInsiders, cliSessionSourceCursor} {
		src := src
		root := env.editorRoots[src]
		jobs = append(jobs, job{src, func() claudeSourceOutcome {
			return readEditorClaudeTabs(ctx, src, root, env.now.Add(-env.lookBack))
		}})
	}
	jobs = append(jobs,
		job{cliSessionSourceClaudeRegistry, func() claudeSourceOutcome { return readClaudeRegistry(env.configDir) }},
		job{cliSessionSourceClaudeStore, func() claudeSourceOutcome {
			return readClaudeStore(ctx, env.configDir, env.now.Add(-env.lookBack), env.maxRows)
		}},
	)

	results := make(chan claudeSourceOutcome, len(jobs))
	for _, j := range jobs {
		j := j
		go func() {
			defer func() {
				if r := recover(); r != nil {
					results <- claudeSourceOutcome{source: j.source, skip: cliSessionSkipUnreadable}
				}
			}()
			results <- j.run()
		}()
	}
	outcomes := map[string]claudeSourceOutcome{}
collect:
	for range jobs {
		select {
		case o := <-results:
			outcomes[o.source] = o
		case <-ctx.Done():
			break collect
		}
	}

	res := cliSessionsDiscoverResult{
		Engine:         cliSessionEngineClaudeCode,
		Rows:           []cliSessionRow{},
		SourcesChecked: []string{},
		SourcesSkipped: []cliSessionSkip{},
		ScannedAt:      env.now.UnixMilli(),
	}
	for _, j := range jobs {
		o, ok := outcomes[j.source]
		if !ok {
			res.SourcesSkipped = append(res.SourcesSkipped, cliSessionSkip{j.source, cliSessionSkipTimeout})
			continue
		}
		if o.checked {
			res.SourcesChecked = append(res.SourcesChecked, j.source)
		}
		if o.skip != "" {
			res.SourcesSkipped = append(res.SourcesSkipped, cliSessionSkip{j.source, o.skip})
		}
	}

	store := outcomes[cliSessionSourceClaudeStore].store
	if store == nil {
		store = map[string]*claudeTranscriptInfo{}
	}
	res.Truncated = outcomes[cliSessionSourceClaudeStore].trunc
	tabsByID := map[string][]claudeEditorTab{}
	for _, src := range []string{cliSessionSourceVSCode, cliSessionSourceVSCodeInsiders, cliSessionSourceCursor} {
		for _, t := range outcomes[src].tabs {
			tabsByID[t.SessionID] = append(tabsByID[t.SessionID], t)
		}
	}
	runningByID := map[string][]claudeRegistryEntry{}
	for _, r := range outcomes[cliSessionSourceClaudeRegistry].running {
		runningByID[r.SessionID] = append(runningByID[r.SessionID], r)
	}

	// A session an editor tab or a running process names but the windowed
	// store scan did not parse is looked up on its own (bounded): an open tab
	// is offered whatever its age.
	ids := map[string]bool{}
	for id := range store {
		ids[id] = true
	}
	lookups := 0
	for _, set := range []map[string]bool{keysOf(tabsByID), keysOf(runningByID)} {
		for id := range set {
			if ids[id] {
				continue
			}
			ids[id] = true
			if _, ok := store[id]; ok || lookups >= cliSessionMaxExtraLookups || ctx.Err() != nil {
				continue
			}
			lookups++
			if info := lookupClaudeTranscript(env.configDir, outcomes[cliSessionSourceClaudeStore], id); info != nil {
				store[id] = info
			}
		}
	}

	for id := range ids {
		row, ok := buildClaudeSessionRow(id, store[id], tabsByID[id], runningByID[id], env)
		if ok {
			res.Rows = append(res.Rows, row)
		}
	}
	sort.SliceStable(res.Rows, func(i, j int) bool {
		if res.Rows[i].LastActiveAt != res.Rows[j].LastActiveAt {
			return res.Rows[i].LastActiveAt > res.Rows[j].LastActiveAt
		}
		return res.Rows[i].SessionID < res.Rows[j].SessionID
	})
	if len(res.Rows) > env.maxRows {
		res.Rows = res.Rows[:env.maxRows]
		res.Truncated = true
	}
	return res
}

// lookupClaudeTranscript parses one session's transcript: from the store
// scan's path index when the scan ran (no further directory walk), else by
// searching every project folder (the scan timed out or failed).
func lookupClaudeTranscript(configDir string, store claudeSourceOutcome, id string) *claudeTranscriptInfo {
	if store.pathsComplete {
		path, ok := store.paths[id]
		if !ok {
			return nil
		}
		info, err := parseClaudeTranscript(id, path)
		if err != nil {
			return nil
		}
		return info
	}
	return findClaudeTranscript(configDir, id)
}

func keysOf[V any](m map[string]V) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// buildClaudeSessionRow merges one session's sources. ok is false for a
// title with no session behind it: no transcript and no live process.
func buildClaudeSessionRow(id string, info *claudeTranscriptInfo, tabs []claudeEditorTab, running []claudeRegistryEntry, env claudeDiscoveryEnv) (cliSessionRow, bool) {
	live := []claudeRegistryEntry{}
	for _, r := range running {
		if env.isOwnPID == nil || !env.isOwnPID(r.PID) {
			live = append(live, r)
		}
	}
	if info == nil && len(running) == 0 {
		return cliSessionRow{}, false
	}
	row := cliSessionRow{
		SessionID: id,
		Editors:   []cliSessionEditorRef{},
		Resumable: info != nil,
	}
	if env.isOwnSession != nil && env.isOwnSession(id) {
		row.StartedByAiExpedite = true
	}
	if info != nil {
		row.Cwd = info.Cwd
		row.LastActiveAt = info.ModifiedAt
	}
	if len(live) > 0 {
		r := newestRegistryEntry(live)
		row.Running = &cliSessionRunning{Status: r.Status, Entrypoint: r.Entrypoint, PID: r.PID}
		if row.Cwd == "" {
			row.Cwd = r.Cwd
		}
		if r.UpdatedAt > row.LastActiveAt {
			row.LastActiveAt = r.UpdatedAt
		}
	} else if len(running) > 0 && row.Cwd == "" {
		row.Cwd = running[0].Cwd
	}
	for _, t := range tabs {
		row.Editors = append(row.Editors, cliSessionEditorRef{Editor: t.Editor, WindowFolder: t.WindowFolder, SeenAt: t.SeenAt})
		if row.Cwd == "" {
			row.Cwd = t.Cwd
		}
	}
	row.Title, row.TitleSource, row.TitleSavedAt = claudeSessionTitle(info, tabs, running)
	return row, true
}

func newestRegistryEntry(entries []claudeRegistryEntry) claudeRegistryEntry {
	best := entries[0]
	for _, e := range entries[1:] {
		if e.UpdatedAt > best.UpdatedAt {
			best = e
		}
	}
	return best
}

// claudeSessionTitle applies the title precedence: a non-default editor tab
// title, a title saved in the session (custom / AI title, summary, or a
// registry name Claude did not derive), the last prompt, the first prompt.
// VS Code saves long tab titles cut short with "…"; when the session's saved
// title is the full form of that cut title, the full one is used.
func claudeSessionTitle(info *claudeTranscriptInfo, tabs []claudeEditorTab, running []claudeRegistryEntry) (title, source string, savedAt int64) {
	saved, savedAtMs := "", int64(0)
	if info != nil && info.SavedTitle != "" {
		saved, savedAtMs = info.SavedTitle, info.TitleSavedAt
	}
	if saved == "" {
		for _, r := range running {
			if r.Name != "" && r.NameSource != "" && r.NameSource != "derived" {
				saved, savedAtMs = r.Name, r.UpdatedAt
				break
			}
		}
	}
	for _, t := range tabs {
		if t.Title == "" || isDefaultClaudeTitle(t.Title) {
			continue
		}
		if saved != "" && editorTitleExtends(t.Title, saved) {
			return cliSessionLabel(saved, cliSessionTitleMaxChars), cliSessionTitleSavedTitle, savedAtMs
		}
		return cliSessionLabel(t.Title, cliSessionTitleMaxChars), cliSessionTitleEditorTab, t.SeenAt
	}
	if saved != "" {
		return cliSessionLabel(saved, cliSessionTitleMaxChars), cliSessionTitleSavedTitle, savedAtMs
	}
	if info != nil && info.LastPrompt != "" {
		return cliSessionLabel(info.LastPrompt, cliSessionFallbackLabelMaxChars), cliSessionTitleLastPrompt, 0
	}
	if info != nil && info.FirstPrompt != "" {
		return cliSessionLabel(info.FirstPrompt, cliSessionFallbackLabelMaxChars), cliSessionTitleFirstPrompt, 0
	}
	return "", cliSessionTitleNone, 0
}

/* -------------------------------------------------------------------------- */
/* Command handlers                                                           */
/* -------------------------------------------------------------------------- */

// parseDiscoverArgs reads the signed args [engine, lookBackDays, maxRows].
func parseDiscoverArgs(args []string) (engine string, lookBackDays, maxRows int) {
	engine = cliSessionEngineClaudeCode
	if len(args) > 0 && args[0] != "" {
		engine = args[0]
	}
	if len(args) > 1 {
		lookBackDays, _ = strconv.Atoi(args[1])
	}
	if len(args) > 2 {
		maxRows, _ = strconv.Atoi(args[2])
	}
	return engine, lookBackDays, maxRows
}

func cliSessionResultBase(cmd commandMsg, cfg *Config, resultType string) resultMsg {
	agentID := cmd.AgentID
	if cfg != nil && cfg.AgentID != "" {
		agentID = cfg.AgentID
	}
	return resultMsg{
		ID:          cmd.ID,
		WorkspaceID: cmd.WorkspaceID,
		UID:         cmd.UID,
		AgentID:     agentID,
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		Type:        resultType,
		RefreshID:   cmd.RefreshID,
		Status:      "success",
	}
}

// fitDiscoverResult drops the oldest rows until the marshaled result fits the
// publish budget (marked truncated).
func fitDiscoverResult(res *cliSessionsDiscoverResult) {
	for len(res.Rows) > 0 {
		b, err := json.Marshal(res)
		if err == nil && len(b) <= cliSessionMaxResultBytes {
			return
		}
		cut := len(res.Rows) / 10
		if cut < 1 {
			cut = 1
		}
		res.Rows = res.Rows[:len(res.Rows)-cut]
		res.Truncated = true
	}
}

// handleCliSessionsDiscoverCommand ALWAYS publishes one correlated result,
// even after a panic, so terminal-service's pending request never sticks.
func handleCliSessionsDiscoverCommand(ctx context.Context, topic *pubsub.Publisher, cmd commandMsg, cfg *Config) (publishErr error) {
	publish := func(payload cliSessionsDiscoverResult, status string) error {
		res := cliSessionResultBase(cmd, cfg, cliSessionsDiscoverResultType)
		res.Status = status
		res.CliSessions = &payload
		return publishMsg(ctx, topic, res)
	}
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("%s[pubsub] panic in cli session discovery: %v%s\n", colorRed, r, colorReset)
			publishErr = publish(cliSessionsDiscoverResult{
				Engine: cliSessionEngineClaudeCode, Rows: []cliSessionRow{},
				SourcesChecked: []string{}, SourcesSkipped: []cliSessionSkip{},
				ScannedAt: time.Now().UnixMilli(), Error: cliSessionErrInternal,
			}, "error")
		}
	}()

	engine, lookBackDays, maxRows := parseDiscoverArgs(cmd.Args)
	if engine != cliSessionEngineClaudeCode {
		return publish(cliSessionsDiscoverResult{
			Engine: engine, Rows: []cliSessionRow{},
			SourcesChecked: []string{}, SourcesSkipped: []cliSessionSkip{},
			ScannedAt: time.Now().UnixMilli(), Error: cliSessionErrEngineNotSupported,
		}, "error")
	}
	runCtx, cancel := context.WithTimeout(ctx, cliSessionDiscoverBudget)
	defer cancel()
	res := discoverClaudeSessions(runCtx, defaultClaudeDiscoveryEnv(lookBackDays, maxRows))
	fitDiscoverResult(&res)
	if err := publish(res, "success"); err != nil {
		fmt.Printf("%s[pubsub] Failed to publish cli session discovery: %v%s\n", colorRed, err, colorReset)
		return err
	}
	return nil
}
