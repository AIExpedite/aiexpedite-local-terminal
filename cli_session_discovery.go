// cli_session_discovery.go — answers terminal-service's
// __cli_sessions_discover__ command: the coding-agent sessions this computer
// knows that AI Expedite may NOT have started (a Claude Code tab in VS Code /
// Insiders / Cursor, a Codex or Grok session started in a terminal), by
// title, so the owner can adopt one into the web chat without knowing its
// session id. One request asks for ONE engine (args [engine, lookBackDays,
// maxRows]); the engine-keyed reader registry (cliSessionReaders) answers it:
// Claude Code's reader is in this file (its sources in
// claude_session_sources_*.go), every other engine's in
// <engine>_session_sources.go.
//
// Contract: shared-constants src/constants/cliSessionDiscovery.js
// (CLI_SESSION_DEVICE_COMMANDS / CLI_SESSION_DEVICE_RESULT_TYPES, engines,
// sources, title sources, skip reasons). terminal-service matches titles;
// this file only reports what each local source says.
//
// READ-ONLY by construction: every source is opened read-only (SQLite
// `mode=ro`, and `immutable` unless its owner's write-ahead log is live,
// cliSessionSQLiteDSN; plain os.Open on files) and nothing is ever written,
// created, renamed or locked against its owner. No agent documents its
// session storage, so every reader parses through an allowlist and reports a
// source it cannot read as skipped (`unreadable` / `unrecognised` /
// `timeout`) rather than guessing; one bad source never fails the request.
// Every reader shares the one budget (cliSessionDiscoverBudget,
// cliSessionMaxRows): a scan the budget cuts short answers what it read,
// marked `truncated`, and a reader that does not answer at all is reported
// timed out instead of holding the answer.
//
// Privacy: a row carries a title or a short, masked prompt-based fallback
// label — never conversation content, tool output or file contents. Only the
// session ids and folders terminal-service needs to adopt a session leave
// the computer, and transcripts never do.
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
	"unicode"
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

// Engines (shared-constants CLI_SESSION_ENGINES): each is the coding agent's
// CLI_AGENT_IDS value, so they are derived from those, never re-listed.
const (
	cliSessionEngineClaudeCode  = claudeCodeCliAgentID
	cliSessionEngineCodex       = codexCliAgentID
	cliSessionEngineGrok        = grokCliAgentID
	cliSessionEngineOpenCode    = opencodeCliAgentID
	cliSessionEngineAntigravity = antigravityCliAgentID
	cliSessionEngineMuseCode    = museCodeCliAgentID
)

// cliSessionDiscoveryProtocol is advertised in capabilitiesInfo
// (shared-constants CLI_SESSION_DISCOVERY_PROTOCOL; a reader accepts any value
// at or above its own). 2 added, all additive:
//   - rows (and the probe) carry `startedVia` / `lastVia`, the raw entrypoint
//     Claude Code recorded when the session started and when it was last driven;
//   - a scan cut short by the time budget keeps what it read and says
//     `truncated: true`;
//   - the claude_native_outside_writer frame names the agent (`agentId`,
//     `bootId`), and a --fork-session copy never watches the original.
//
// 3 is shared-constants CLI_SESSION_MULTI_ENGINE_PROTOCOL, the first at which
// the cloud asks for an engine other than Claude Code:
//   - every engine the capability lists has a reader (cliSessionReaders), and
//     each request's `truncated` / `sourcesSkipped` are that engine's own;
//   - every row carries `engine`;
//   - the outside-writer frame is published for every engine a session of
//     which AI Expedite continues in place (cli_session_outside_writer.go),
//     and a watcher's current state is published again on every Pub/Sub
//     reconnect, so a missed `idle` never holds a message for good.
const cliSessionDiscoveryProtocol = 3

// Sources (shared-constants CLI_SESSION_SOURCES).
const (
	cliSessionSourceVSCode         = "vscode"
	cliSessionSourceVSCodeInsiders = "vscodeInsiders"
	cliSessionSourceCursor         = "cursor"
	cliSessionSourceClaudeRegistry = "claudeRegistry"
	cliSessionSourceClaudeStore    = "claudeStore"
	// One source per other engine: its own session store or listing.
	cliSessionSourceCodexStore       = "codexStore"
	cliSessionSourceGrokStore        = "grokStore"
	cliSessionSourceAntigravityStore = "antigravityStore"
	cliSessionSourceOpenCodeStore    = "opencodeStore"
	cliSessionSourceMuseListing      = "museListing"
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
	// The provider itself offers no list this build can read (a Muse Code host
	// without MSP `session/list`), and the agent's CLI is not on this computer:
	// the spellings of shared-constants CLI_SESSION_DISCOVERY_STATUS_REASONS
	// PROVIDER_UNSUPPORTED / NOT_INSTALLED, so terminal-service reports the
	// engine `unsupported` for the right reason instead of "no sessions".
	cliSessionErrProviderUnsupported = "providerUnsupported"
	cliSessionErrNotInstalled        = "notInstalled"
	// A probe that could not answer inside the budget: never read as "gone".
	cliSessionErrTimeout = "timeout"
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
	// The source readers stop this long before the budget ends (at most a
	// quarter of what is left), so the collector, which waits for the whole
	// budget, still receives what each one read in time instead of abandoning
	// a slow transcript scan's partial answer at the deadline.
	cliSessionSourceDeadlineGrace = 750 * time.Millisecond
	// A `startedVia` / `lastVia` value is cut to this many characters.
	cliSessionViaMaxChars = 40
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
	// Engine is the reader's (shared-constants CLI_SESSION_ENGINES), stamped on
	// every row by runCliSessionDiscover: one conversation two sources list is
	// one candidate, keyed (engine, sessionId). Protocol 3.
	Engine              string                `json:"engine"`
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
	// Where the session was started and where it was last driven, as Claude
	// Code recorded it in the transcript (`entrypoint`: "claude-vscode",
	// "claude-desktop", "cli", "sdk-cli", …), raw and capped; absent when the
	// transcript names none. A value this build does not know passes through
	// unchanged: readers map it through their own allowlist (shared-constants
	// CLI_SESSION_ENTRYPOINTS). Protocol 2.
	StartedVia string `json:"startedVia,omitempty"`
	LastVia    string `json:"lastVia,omitempty"`
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

// claudeForkSessionFromArgs reports whether a start COPIES the session it
// resumes (`--fork-session`, the adopt flow's "continue from a copy",
// residentChatStartRoute.js): Claude then runs a NEW session id whose history
// starts as the original's, and the original is left alone.
func claudeForkSessionFromArgs(args []string) bool {
	for _, a := range args {
		if a == "--fork-session" || a == "--fork-session=true" {
			return true
		}
	}
	return false
}

// claudeResumedInPlaceID is the session id a start continues IN PLACE — its
// --resume id — or "" for a fresh start and for a --fork-session copy. Only a
// session continued in place is AI Expedite's to claim and watch for another
// writer; a copy's own id is learnt from its init frame like a fresh one's.
func claudeResumedInPlaceID(args []string) string {
	if claudeForkSessionFromArgs(args) {
		return ""
	}
	return claudeResumeIDFromArgs(args)
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

// cliSessionViaValue is an entrypoint value as a row reports it: trimmed and
// cut to cliSessionViaMaxChars, "" for an empty value or one that is not a
// single printable line (never a code). Any other value passes through as
// Claude wrote it — there is no allowlist here, so a new entrypoint reaches
// the server unchanged.
func cliSessionViaValue(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" || !utf8.ValidString(v) {
		return ""
	}
	for _, r := range v {
		if !unicode.IsPrint(r) {
			return ""
		}
	}
	if utf8.RuneCountInString(v) > cliSessionViaMaxChars {
		v = string([]rune(v)[:cliSessionViaMaxChars])
	}
	return v
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

// claudeStoreReader reads Claude's transcript store (readClaudeStore).
type claudeStoreReader func(ctx context.Context, configDir string, since time.Time, maxRows int) claudeSourceOutcome

// claudeDiscoveryEnv is everything discovery reads from, injectable for tests.
type claudeDiscoveryEnv struct {
	configDir    string            // Claude's config dir (~/.claude or CLAUDE_CONFIG_DIR)
	editorRoots  map[string]string // source -> the editor's user-data root ("…/Code")
	now          time.Time
	lookBack     time.Duration
	maxRows      int
	isOwnSession func(string) bool
	isOwnPID     func(int) bool
	// readStore reads the transcript store; nil is readClaudeStore. A seam
	// for tests (a scan still running at the deadline).
	readStore claudeStoreReader
}

func defaultClaudeDiscoveryEnv(lookBackDays, maxRows int) claudeDiscoveryEnv {
	return claudeDiscoveryEnvFor(newCliSessionDiscoverRequest(lookBackDays, maxRows))
}

// claudeDiscoveryEnvFor is the real computer's Claude sources for one request.
func claudeDiscoveryEnvFor(req cliSessionDiscoverRequest) claudeDiscoveryEnv {
	home := userHomeDir()
	return claudeDiscoveryEnv{
		configDir:    claudeConfigDir(home),
		editorRoots:  defaultEditorUserDataRoots(home),
		now:          req.now,
		lookBack:     req.lookBack,
		maxRows:      req.maxRows,
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

// claudeSourceContext is the context the source readers run under: it ends
// cliSessionSourceDeadlineGrace (at most a quarter of the time left) before
// ctx does, so a reader that stops on it still has time to hand its partial
// outcome to the collector, which waits on ctx itself. Without a deadline on
// ctx the readers simply share its cancellation.
func claudeSourceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	grace := cliSessionSourceDeadlineGrace
	if quarter := time.Until(deadline) / 4; quarter < grace {
		grace = quarter
	}
	if grace < 0 {
		grace = 0
	}
	return context.WithDeadline(ctx, deadline.Add(-grace))
}

// discoverClaudeSessions runs every source under ctx, merges by session id and
// returns the result rows newest first, capped at env.maxRows.
//
// A scan that does not finish inside the budget still answers with what it
// read: the readers stop a little before ctx ends (claudeSourceContext) and
// the collector keeps waiting for their partial outcomes until ctx does. The
// answer then says `truncated: true`, so a reader never takes "no match" in a
// partial scan for "no such session".
func discoverClaudeSessions(ctx context.Context, env claudeDiscoveryEnv) cliSessionsDiscoverResult {
	srcCtx, cancelSources := claudeSourceContext(ctx)
	defer cancelSources()
	readStore := env.readStore
	if readStore == nil {
		readStore = readClaudeStore
	}

	type job struct {
		source string
		run    func() claudeSourceOutcome
	}
	jobs := []job{}
	for _, src := range []string{cliSessionSourceVSCode, cliSessionSourceVSCodeInsiders, cliSessionSourceCursor} {
		src := src
		root := env.editorRoots[src]
		jobs = append(jobs, job{src, func() claudeSourceOutcome {
			return readEditorClaudeTabs(srcCtx, src, root, env.now.Add(-env.lookBack))
		}})
	}
	jobs = append(jobs,
		job{cliSessionSourceClaudeRegistry, func() claudeSourceOutcome { return readClaudeRegistry(env.configDir) }},
		job{cliSessionSourceClaudeStore, func() claudeSourceOutcome {
			return readStore(srcCtx, env.configDir, env.now.Add(-env.lookBack), env.maxRows)
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

	storeOutcome, storeAnswered := outcomes[cliSessionSourceClaudeStore]
	store := storeOutcome.store
	if store == nil {
		store = map[string]*claudeTranscriptInfo{}
	}
	// The store is the only source that lists every session, so an answer
	// without all of it (no answer at all, a scan cut by the budget, or more
	// transcripts than maxRows) may be missing sessions.
	res.Truncated = !storeAnswered || storeOutcome.trunc || storeOutcome.skip == cliSessionSkipTimeout
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
			if lookups >= cliSessionMaxExtraLookups || ctx.Err() != nil {
				// Unread: its row is dropped, or reported not resumable.
				res.Truncated = true
				continue
			}
			lookups++
			if info := lookupClaudeTranscript(env.configDir, storeOutcome, id); info != nil {
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
		row.StartedVia, row.LastVia = info.FirstEntrypoint, info.LastEntrypoint
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
/* Reader registry                                                            */
/* -------------------------------------------------------------------------- */

// cliSessionDiscoverRequest is one discovery's bounds, already clamped.
type cliSessionDiscoverRequest struct {
	now      time.Time
	lookBack time.Duration
	maxRows  int
}

// since is the oldest last activity the request asks for.
func (r cliSessionDiscoverRequest) since() time.Time { return r.now.Add(-r.lookBack) }

// newCliSessionDiscoverRequest clamps the server's bounds to this build's
// defaults and ceilings (shared-constants CLI_SESSION_DISCOVERY_LIMITS).
func newCliSessionDiscoverRequest(lookBackDays, maxRows int) cliSessionDiscoverRequest {
	if lookBackDays <= 0 {
		lookBackDays = cliSessionDefaultLookBackDays
	}
	if lookBackDays > cliSessionMaxLookBackDays {
		lookBackDays = cliSessionMaxLookBackDays
	}
	if maxRows <= 0 || maxRows > cliSessionMaxRows {
		maxRows = cliSessionDefaultMaxRows
	}
	return cliSessionDiscoverRequest{
		now:      time.Now(),
		lookBack: time.Duration(lookBackDays) * 24 * time.Hour,
		maxRows:  maxRows,
	}
}

// cliSessionProbeRequest is one probe's arguments.
type cliSessionProbeRequest struct {
	now            time.Time
	sessionID      string
	cwd            string
	includeHistory bool
}

// cliSessionReader is one engine's discovery: what lists its sessions and
// what answers a probe of one of them. Every reader is read-only, stops when
// its ctx ends (cliSessionSourceDeadlineGrace before the collector stops
// waiting) with what it read marked truncated, keeps to req.maxRows, and
// reports a source it could not read as skipped rather than guessing.
type cliSessionReader struct {
	engine string
	// sources are the sources discover reads, reported timed out when it
	// never answers.
	sources  []string
	discover func(ctx context.Context, req cliSessionDiscoverRequest) cliSessionsDiscoverResult
	probe    func(ctx context.Context, req cliSessionProbeRequest) cliSessionProbeResult
	// selfBudgeted: discover and probe keep the budget themselves (Claude
	// Code's collector, unchanged since protocol 2), so they are called
	// directly rather than through the runner's own wait.
	selfBudgeted bool
}

// claudeSessionReader is Claude Code's reader: the sources above, as protocol
// 2 read them.
var claudeSessionReader = &cliSessionReader{
	engine: cliSessionEngineClaudeCode,
	sources: []string{
		cliSessionSourceVSCode, cliSessionSourceVSCodeInsiders, cliSessionSourceCursor,
		cliSessionSourceClaudeRegistry, cliSessionSourceClaudeStore,
	},
	selfBudgeted: true,
	discover: func(ctx context.Context, req cliSessionDiscoverRequest) cliSessionsDiscoverResult {
		return discoverClaudeSessions(ctx, claudeDiscoveryEnvFor(req))
	},
	probe: func(ctx context.Context, req cliSessionProbeRequest) cliSessionProbeResult {
		return probeClaudeSession(ctx, defaultClaudeDiscoveryEnv(0, 0), req.sessionID, req.includeHistory)
	},
}

// cliSessionReaders is every engine this build discovers, Claude Code first
// (the capability lists them in this order), then shared-constants
// CLI_SESSION_ENGINES' order. A var so tests can stand a reader in.
var cliSessionReaders = []*cliSessionReader{
	claudeSessionReader,
	codexSessionReader,
	grokSessionReader,
	openCodeSessionReader,
	antigravitySessionReader,
	museCodeSessionReader,
}

// cliSessionReaderFor is the engine's reader, nil for one this build has none for.
func cliSessionReaderFor(engine string) *cliSessionReader {
	for _, r := range cliSessionReaders {
		if r.engine == engine {
			return r
		}
	}
	return nil
}

// newCliSessionDiscoveryCapability is capabilitiesInfo.CliSessionDiscovery:
// every engine this build has a reader for, Claude Code first, at this
// build's protocol, whether or not the agent's CLI is installed here (the
// shared contract's six). terminal-service answers "not installed"
// (CLAUDE_NOT_INSTALLED / AGENT_NOT_INSTALLED) from the CLI Agents report,
// which its demand-driven refresh keeps current, but only for an engine
// listed here: one missing from this list at protocol 3 reads as an agent
// whose sessions this app cannot read at all (providerUnsupported), and this
// list is gathered again only every machineInfoRefreshInterval, so a CLI
// installed in between would read that way for hours. A reader whose agent is
// missing still says so itself (Muse Code's `notInstalled`).
func newCliSessionDiscoveryCapability() *cliSessionDiscoveryCapability {
	engines := make([]string, 0, len(cliSessionReaders))
	for _, r := range cliSessionReaders {
		engines = append(engines, r.engine)
	}
	return &cliSessionDiscoveryCapability{Engines: engines, Protocol: cliSessionDiscoveryProtocol}
}

// newCliSessionsResult is an empty answer for engine, scanned at now.
func newCliSessionsResult(engine string, now time.Time) cliSessionsDiscoverResult {
	return cliSessionsDiscoverResult{
		Engine:         engine,
		Rows:           []cliSessionRow{},
		SourcesChecked: []string{},
		SourcesSkipped: []cliSessionSkip{},
		ScannedAt:      now.UnixMilli(),
	}
}

// skip records that source was not read (reason: a cliSessionSkip* value).
func (res *cliSessionsDiscoverResult) skip(source, reason string) {
	res.SourcesSkipped = append(res.SourcesSkipped, cliSessionSkip{Source: source, Reason: reason})
}

// failedResult is the answer for a reader that did not answer inside the
// budget (reason timeout) or panicked (unreadable): nothing read, so it may
// be missing every session.
func (r *cliSessionReader) failedResult(now time.Time, reason string) cliSessionsDiscoverResult {
	res := newCliSessionsResult(r.engine, now)
	res.Truncated = true
	for _, s := range r.sources {
		res.skip(s, reason)
	}
	return res
}

// finishCliSessionRows sorts rows newest first (by session id within one
// instant, so the answer is stable) and keeps at most maxRows, marking the
// answer truncated when it drops one.
func finishCliSessionRows(res *cliSessionsDiscoverResult, maxRows int) {
	sort.SliceStable(res.Rows, func(i, j int) bool {
		if res.Rows[i].LastActiveAt != res.Rows[j].LastActiveAt {
			return res.Rows[i].LastActiveAt > res.Rows[j].LastActiveAt
		}
		return res.Rows[i].SessionID < res.Rows[j].SessionID
	})
	if maxRows > 0 && len(res.Rows) > maxRows {
		res.Rows = res.Rows[:maxRows]
		res.Truncated = true
	}
}

// runCliSessionDiscover answers one request through reader under ctx (the
// request's budget). A reader that keeps no budget of its own reads under a
// context that ends cliSessionSourceDeadlineGrace earlier (claudeSourceContext),
// so its partial answer still lands; one that has not answered when ctx ends
// is reported timed out, and its goroutine's late answer is dropped. Every
// row leaves stamped with the reader's engine.
func runCliSessionDiscover(ctx context.Context, reader *cliSessionReader, req cliSessionDiscoverRequest) cliSessionsDiscoverResult {
	var res cliSessionsDiscoverResult
	if reader.selfBudgeted {
		res = reader.discover(ctx, req)
	} else {
		srcCtx, cancel := claudeSourceContext(ctx)
		defer cancel()
		answer := make(chan cliSessionsDiscoverResult, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Printf("%s[cli-sessions] %s reader panicked: %v%s\n", colorRed, reader.engine, r, colorReset)
					answer <- reader.failedResult(req.now, cliSessionSkipUnreadable)
				}
			}()
			answer <- reader.discover(srcCtx, req)
		}()
		select {
		case res = <-answer:
		case <-ctx.Done():
			res = reader.failedResult(req.now, cliSessionSkipTimeout)
		}
		finishCliSessionRows(&res, req.maxRows)
	}
	res.Engine = reader.engine
	if res.Rows == nil {
		res.Rows = []cliSessionRow{}
	}
	if res.SourcesChecked == nil {
		res.SourcesChecked = []string{}
	}
	if res.SourcesSkipped == nil {
		res.SourcesSkipped = []cliSessionSkip{}
	}
	if res.ScannedAt == 0 {
		res.ScannedAt = req.now.UnixMilli()
	}
	for i := range res.Rows {
		res.Rows[i].Engine = reader.engine
		if res.Rows[i].Editors == nil {
			res.Rows[i].Editors = []cliSessionEditorRef{}
		}
	}
	return res
}

// runCliSessionProbe answers one probe through reader under ctx, the same
// way: a probe that does not answer in time says so (cliSessionErrTimeout)
// rather than "no such session".
func runCliSessionProbe(ctx context.Context, reader *cliSessionReader, req cliSessionProbeRequest) cliSessionProbeResult {
	var res cliSessionProbeResult
	if reader.selfBudgeted {
		res = reader.probe(ctx, req)
	} else {
		answer := make(chan cliSessionProbeResult, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Printf("%s[cli-sessions] %s probe panicked: %v%s\n", colorRed, reader.engine, r, colorReset)
					answer <- cliSessionProbeResult{SessionID: req.sessionID, Error: cliSessionErrInternal}
				}
			}()
			answer <- reader.probe(ctx, req)
		}()
		select {
		case res = <-answer:
		case <-ctx.Done():
			res = cliSessionProbeResult{SessionID: req.sessionID, Error: cliSessionErrTimeout}
		}
	}
	if res.SessionID == "" {
		res.SessionID = req.sessionID
	}
	if res.Writers == nil {
		res.Writers = []cliSessionWriter{}
	}
	if res.OpenInEditors == nil {
		res.OpenInEditors = []string{}
	}
	return res
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
	engine, lookBackDays, maxRows := parseDiscoverArgs(cmd.Args)
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("%s[pubsub] panic in cli session discovery: %v%s\n", colorRed, r, colorReset)
			failed := newCliSessionsResult(engine, time.Now())
			failed.Error = cliSessionErrInternal
			publishErr = publish(failed, "error")
		}
	}()

	reader := cliSessionReaderFor(engine)
	if reader == nil {
		unsupported := newCliSessionsResult(engine, time.Now())
		unsupported.Error = cliSessionErrEngineNotSupported
		return publish(unsupported, "error")
	}
	runCtx, cancel := context.WithTimeout(ctx, cliSessionDiscoverBudget)
	defer cancel()
	res := runCliSessionDiscover(runCtx, reader, newCliSessionDiscoverRequest(lookBackDays, maxRows))
	fitDiscoverResult(&res)
	if err := publish(res, "success"); err != nil {
		fmt.Printf("%s[pubsub] Failed to publish cli session discovery: %v%s\n", colorRed, err, colorReset)
		return err
	}
	return nil
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
	engine, sessionID, cwd, includeHistory := parseProbeArgs(cmd.Args)
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
	reader := cliSessionReaderFor(engine)
	if reader == nil || reader.probe == nil {
		return publish(cliSessionProbeResult{
			SessionID: sessionID, Writers: []cliSessionWriter{}, OpenInEditors: []string{},
			Error: cliSessionErrEngineNotSupported,
		}, "error")
	}
	probeCtx, cancel := context.WithTimeout(ctx, cliSessionDiscoverBudget)
	defer cancel()
	res := runCliSessionProbe(probeCtx, reader, cliSessionProbeRequest{
		now: time.Now(), sessionID: sessionID, cwd: cwd, includeHistory: includeHistory,
	})
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
