package main

// OpenCodeNativeManager — logical multi-turn chat sessions backed by one-shot
// `opencode run --format json` processes with exact native resume via
// `--session <id>`.
//
// WHY THIS SHAPE
//
//   - `opencode run` is a ONE-SHOT command, not a resident stdio server, so the
//     process model is Antigravity's (one short-lived child per turn, logical
//     session in between) rather than Claude/Codex/Grok's (a resident child
//     speaking a framed protocol).
//
//   - `opencode serve` is OpenCode's HTTP + SSE interface and would give a
//     richer event stream, but it needs a loopback TCP listener, port
//     allocation, bind-address hardening and a firewall story — a new attack
//     surface with no precedent here. Every existing driver in this package is
//     a stdio/argv child under globalProcessRegistry. Per-tool approval
//     round-trips via `serve` would be a follow-up behind this same session
//     kind.
//
//   - Unlike `agy --print` (complete-only output), `opencode run --format json`
//     emits INCREMENTAL JSON events on stdout. Each line is published as its
//     own opencode_native_message frame so the frontend renders text deltas and
//     tool activity in near real time, and the assistant text is also
//     accumulated locally for the bounded replay transcript.
//
//   - `--continue` resumes "the most recent session" globally and is NEVER
//     used: it would cross-wire a chat with whatever the user last ran in their
//     own TUI. Resume is exact-id only, and it is version-gated (upstream has
//     open reports of `--session` misbehaving headlessly). Below
//     openCodeNativeMinVersion the manager does not pass `--session` at all and
//     goes straight to bounded transcript replay, which keeps context rather
//     than silently starting a stateless conversation.
//
// Threat model for the prompt file and the auto-approve flag:
//   Remote AIExpedite users cannot answer local interactive permission prompts
//   on the workstation, so a headless turn must not block on one. The launch is
//   gated by: (1) per-device "Enable OpenCode chat sessions" opt-in,
//   (2) workspace membership + device ownership checks in terminal-service,
//   (3) parent orchestration approval for terminal access, (4) working-directory
//   scope re-verified on every turn (containedCwd), (5) process registration +
//   orphan cleanup limited to AIExpedite-owned PIDs, (6) redacted telemetry (no
//   prompts/secrets). The prompt is delivered on stdin from an owner-only
//   (0600) temp file rather than argv, so it never appears in a process listing
//   and is not subject to the Windows CreateProcess ~32KB argv ceiling.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	openCodeNativeMaxStdout = 8 * 1024 * 1024
	openCodeNativeMaxStderr = 1 * 1024 * 1024
	// Bounded copy of raw stdout kept purely for failure diagnosis (the
	// missing-session probe). It is NEVER used as assistant text — that stays
	// the parsed-event accumulation — so a banner or a plain-text CLI error
	// cannot reach the transcript through it.
	openCodeNativeMaxRawStdout = 64 * 1024
	// Longest single JSON event line accepted from stdout. A frame beyond this
	// is a hard turn error rather than a silent truncation — a half-parsed
	// event would render as a corrupt assistant message.
	openCodeNativeMaxFrameBytes = 4 * 1024 * 1024
	// Soft ceiling for a single turn.
	openCodeNativeDefaultTurnTimeout = 10 * time.Minute
	openCodeNativeGracefulKillWait   = 3 * time.Second
	openCodeNativeMaxAge             = 6 * time.Hour
	openCodeNativeCleanupInterval    = 60 * time.Second
	// Replay bounds when native resume is unavailable. Mirrors
	// shared-constants CLI_NATIVE_REPLAY_BOUNDS.
	openCodeReplayMaxMessages = 24
	openCodeReplayMaxChars    = 48_000
	// Minimum `opencode` version that is passed `--session <id>`. Mirrors
	// shared-constants OPENCODE_NATIVE_MIN_VERSION.
	openCodeNativeMinVersion = "0.4.0"
	// The prompt goes to a temp file consumed on stdin, not argv, so the
	// CreateProcess argv ceiling does not apply. This cap exists only so a
	// runaway caller cannot ask the device to buffer an unbounded prompt.
	openCodeNativeMaxPromptBytes = 1024 * 1024
	// Cache capability probes so Start does not spawn `opencode --version` on
	// every chat open. Invalidated after this TTL or when a probe fails.
	openCodeCapabilityCacheTTL = 5 * time.Minute
	// GCP Pub/Sub documented per-message publish ceiling. Authoritative gate
	// after marshaling the resultMsg envelope (JSON escaping can inflate a
	// frame that already fits openCodeNativeMaxFrameBytes).
	openCodeNativeMaxPublishSize = 10_000_000
)

/* --------------------------------------------------------------------------
   Session
   -------------------------------------------------------------------------- */

// OpenCodeNativeSession is a logical multi-turn conversation. Like the
// Antigravity manager and unlike Claude/Grok/Codex, there is no resident
// process between turns — each Send spawns a one-shot `opencode run` and waits
// for completion.
type OpenCodeNativeSession struct {
	ID string
	// NativeSessionID is OpenCode's own session id, captured from the event
	// stream (or the storage-dir snapshot fallback) after the first successful
	// turn and passed as `--session` on every later turn.
	NativeSessionID string
	Cwd             string
	// WorkspaceRoot is the session's own symlink-resolved start cwd, retained
	// so each turn can re-verify Cwd right before launch. See containedCwd for
	// why the check has to run per turn rather than only at Start.
	WorkspaceRoot string
	WorkspaceID   string
	UID           string
	StartedAt     time.Time
	// Bounded transcript for replay recovery (user/assistant pairs).
	Transcript []openCodeTurn

	mu            sync.Mutex
	status        string // "idle" | "running" | "ended"
	activeProcess *exec.Cmd
	activeCancel  func()
	seq           int64
	// publishFn is the Pub/Sub publisher bound at Start (refreshed on Send).
	// Stale GC uses it to emit opencode_native_ended when reaping — End alone
	// does not publish (the explicit opencode_native_end handler does).
	publishFn PublishFunc
	// turnMu serialises Send so two concurrent turns cannot race on the same
	// native session or interleave transcript updates.
	turnMu sync.Mutex
	// endDrainUnconfirmed marks a session whose End cancelled/killed its turn
	// and then timed out on the turnMu drain barrier. The session is RETAINED
	// as a tombstone (see end_confirm.go): only a verified-absent turn process
	// may convert it into the "not found" absence answer.
	endDrainUnconfirmed bool
}

type openCodeTurn struct {
	Role    string // "user" | "assistant"
	Content string
	At      time.Time
}

func (s *OpenCodeNativeSession) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *OpenCodeNativeSession) setStatus(st string) {
	s.mu.Lock()
	s.status = st
	s.mu.Unlock()
}

// beginTurn atomically transitions an idle session to "running". Returns false
// if End marked the session "ended" in the race between the caller's prior
// Status() check and this transition — so a turn cannot overwrite an "ended"
// status back to "running" and thereby hide the cancellation, leaving an
// `opencode` process alive after a Stop.
func (s *OpenCodeNativeSession) beginTurn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == "ended" {
		return false
	}
	s.status = "running"
	return true
}

// setActiveProcess registers the in-flight one-shot process. If End already
// marked the session ended (race before activeCancel was stored), the cancel
// callback is invoked immediately so a late-started child cannot keep running
// tools after the chat was stopped.
func (s *OpenCodeNativeSession) setActiveProcess(cmd *exec.Cmd, cancel func()) {
	s.mu.Lock()
	ended := s.status == "ended"
	s.activeProcess = cmd
	s.activeCancel = cancel
	s.mu.Unlock()
	if ended && cancel != nil {
		cancel()
	}
}

func (s *OpenCodeNativeSession) clearActiveProcess() {
	s.mu.Lock()
	s.activeProcess = nil
	s.activeCancel = nil
	s.mu.Unlock()
}

/* --------------------------------------------------------------------------
   Manager
   -------------------------------------------------------------------------- */

// OpenCodeNativeManager owns logical OpenCode chat sessions.
type OpenCodeNativeManager struct {
	sessions map[string]*OpenCodeNativeSession
	mu       sync.RWMutex
}

// NewOpenCodeNativeManager creates a fresh manager.
func NewOpenCodeNativeManager() *OpenCodeNativeManager {
	return &OpenCodeNativeManager{
		sessions: make(map[string]*OpenCodeNativeSession),
	}
}

// Start registers a logical session. No CLI process is launched until Send.
// publishFn is retained so stale GC can emit opencode_native_ended when the
// cloud never sends opencode_native_end (idle expiry / dropped end command).
// onStarted is invoked after the session is registered so callers can publish
// opencode_native_started before any later frames.
// resumeSessionID seeds NativeSessionID so the FIRST turn of a NEW terminal
// session continues an EXISTING OpenCode session (`--session <id>`), which is
// what makes conversation-scoped resume possible after the previous terminal
// session's device claim was reclaimed: the cloud reads the id off the durable
// pointer it committed from a prior completion frame and hands it back here.
// Empty means "start a fresh session" — the pre-existing behaviour.
//
// Not validated against the local store: a stale id is recoverable at turn
// time (Send's looksLikeMissingSession → bounded transcript replay), which is a
// better answer than refusing the start. Same contract as Antigravity's seed.
func (m *OpenCodeNativeManager) Start(id, cwd string, workspaceID, uid, resumeSessionID string, publishFn PublishFunc, onStarted func()) error {
	if id == "" {
		return fmt.Errorf("sessionID is required")
	}
	if cwd == "" {
		return fmt.Errorf("cwd is required for opencode native (must point at a workspace directory)")
	}
	if !filepath.IsAbs(cwd) {
		return fmt.Errorf("cwd must be an absolute path; got %q", cwd)
	}
	if info, err := os.Stat(cwd); err != nil {
		return fmt.Errorf("cwd %q is not accessible: %w", cwd, err)
	} else if !info.IsDir() {
		return fmt.Errorf("cwd %q is not a directory", cwd)
	}

	// Resolve the start cwd through symlinks; the resolved value becomes the
	// session's OWN workspace root, which each later Send re-resolves against.
	resolvedCwd, err := containedCwd(cwd, "")
	if err != nil {
		return err
	}

	// Idempotent redelivery first: if the session is already registered, re-ack
	// without re-probing. A transient `opencode --version` failure (cache
	// expiry, upgrade, PATH blip) must not turn a usable local session into a
	// cloud-side error and desync Pub/Sub retry state from the manager.
	m.mu.Lock()
	if existing, exists := m.sessions[id]; exists {
		err := ackExistingOpenCodeSession(existing, id, publishFn, onStarted)
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()

	if err := probeOpenCodeNativeCapability(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// Re-check after probe: a concurrent Start for the same id may have won.
	if existing, exists := m.sessions[id]; exists {
		return ackExistingOpenCodeSession(existing, id, publishFn, onStarted)
	}

	session := &OpenCodeNativeSession{
		ID:            id,
		Cwd:           cwd,
		WorkspaceRoot: resolvedCwd,
		WorkspaceID:   workspaceID,
		UID:           uid,
		StartedAt:     time.Now(),
		status:        "idle",
		publishFn:     publishFn,
		// Seeded in the literal, before registration, so a Send racing the tail
		// of Start cannot observe a startable session not yet told which
		// conversation it continues (that window silently starts fresh).
		NativeSessionID: resumeSessionID,
	}
	m.sessions[id] = session

	fmt.Printf("%s[opencode-native] Session %s registered (cwd=%s)%s\n",
		colorCyan, id, cwd, colorReset)

	if onStarted != nil {
		onStarted()
	}
	return nil
}

// ackExistingOpenCodeSession refreshes the publisher binding and re-acks
// started for an already-registered logical session. Caller must hold m.mu.
func ackExistingOpenCodeSession(existing *OpenCodeNativeSession, id string, publishFn PublishFunc, onStarted func()) error {
	// Pub/Sub redelivery / terminal-service retry of the same start must re-ack
	// started without ending the still-usable local session. Emitting
	// opencode_native_ended here would release the cloud reservation while the
	// manager still holds the session, breaking later Sends.
	existing.mu.Lock()
	if existing.status == "ended" || existing.endDrainUnconfirmed {
		existing.mu.Unlock()
		return fmt.Errorf("opencode native session %s is ending; retained tombstone cannot acknowledge start", id)
	}
	if publishFn != nil {
		existing.publishFn = publishFn
	}
	existing.mu.Unlock()
	fmt.Printf("%s[opencode-native] Session %s already registered — idempotent start ack%s\n",
		colorCyan, id, colorReset)
	if onStarted != nil {
		onStarted()
	}
	return nil
}

// Send runs one user turn: spawn `opencode run --format json` (with `--session`
// when a native id is known and the binary is new enough), stream each stdout
// JSON event back as its own opencode_native_message frame, capture the native
// session id, and append to the bounded transcript. The logical session stays
// open (idle) after a turn so the user can follow up or retry.
//
// Replay recovery runs only when native resume was active AND the CLI reports a
// recognized missing/stale session — never on generic non-zero exits (auth,
// timeout, tool failures), which would burn a second model call and silently
// start a new conversation.
func (m *OpenCodeNativeManager) Send(id, text string, publishFn PublishFunc, turnTimeout time.Duration) error {
	if publishFn == nil {
		return fmt.Errorf("publishFn is required")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("input is empty")
	}
	if turnTimeout <= 0 {
		turnTimeout = openCodeNativeDefaultTurnTimeout
	}

	session := m.Get(id)
	if session == nil {
		return fmt.Errorf("opencode native session %s not found", id)
	}
	if session.Status() == "ended" {
		return fmt.Errorf("opencode native session %s has ended", id)
	}

	// One active turn per logical conversation. A second Send arriving while a
	// child is still running must NOT spawn a second `opencode` against the same
	// session id — OpenCode would interleave two turns into one transcript.
	if !session.turnMu.TryLock() {
		return fmt.Errorf("opencode native session %s already has a turn in flight", id)
	}
	defer session.turnMu.Unlock()

	if session.Status() == "ended" {
		return fmt.Errorf("opencode native session %s has ended", id)
	}

	// Keep the GC publisher current (Send always has a live Pub/Sub binding).
	session.mu.Lock()
	session.publishFn = publishFn
	session.mu.Unlock()

	// Fail closed with a published error frame so the chat UI cannot stay stuck
	// in "running" after the HTTP send already returned 200.
	if len(text) > openCodeNativeMaxPromptBytes {
		return m.publishTurnError(session, publishFn,
			fmt.Sprintf("prompt exceeds maximum size of %d bytes", openCodeNativeMaxPromptBytes))
	}

	// Conditional idle→running under the session mutex: if End won the race
	// after the check above, do not revive the ended session.
	if !session.beginTurn() {
		return fmt.Errorf("opencode native session %s has ended", id)
	}
	defer func() {
		if session.Status() != "ended" {
			session.setStatus("idle")
		}
		session.clearActiveProcess()
	}()

	executable := resolveOpenCodeExecutable()

	// Version gate: a binary below the resume floor never receives `--session`.
	// Its follow-ups go straight to bounded replay, which keeps context rather
	// than silently starting a stateless conversation on a CLI whose resume is
	// known-unreliable headlessly.
	resumeSupported := openCodeSupportsNativeResume()
	nativeID := session.NativeSessionID
	if !resumeSupported {
		nativeID = ""
		session.NativeSessionID = ""
	}
	useNativeResume := nativeID != ""
	usedReplay := false

	fmt.Printf("%s[opencode-native] Turn on %s (resume=%v)%s\n",
		colorCyan, id, useNativeResume, colorReset)

	// Re-resolve the cwd for this turn: symlink-free and workspace-contained.
	// Resolving per turn just before launch closes the start→send symlink-swap
	// TOCTOU that Start's earlier check cannot (no process launches until now).
	runDir, cwdErr := containedCwd(session.Cwd, session.WorkspaceRoot)
	if cwdErr != nil {
		return m.publishTurnError(session, publishFn,
			fmt.Sprintf("cwd containment revalidation failed: %v", cwdErr))
	}

	// No usable native id but prior turns exist (never captured, lost across a
	// device restart, or the binary predates resume): replay the bounded
	// transcript so the follow-up keeps context. A first turn (empty transcript)
	// still sends the bare prompt.
	promptToSend, replayForContinuity := openCodeTurnPrompt(nativeID, session.Transcript, text)
	if replayForContinuity {
		usedReplay = true
	}

	// Snapshot the storage dir so a session id created by THIS run can be told
	// apart from one a concurrent local TUI created — see captureOpenCodeNativeID.
	beforeIDs := listOpenCodeSessionIDs(runDir)

	result := m.runOneShot(session, runDir, executable, promptToSend, nativeID, turnTimeout,
		"opencode-native:"+id, publishFn)
	if result.err != nil {
		return m.publishTurnError(session, publishFn, result.err.Error())
	}
	m.publishStderrIfAny(session, publishFn, result.stderr)
	if result.frameOverflow {
		// Requirements: an oversize/garbage frame must surface a fatal error
		// rather than be silently dropped as if the turn had completed.
		return m.publishTurnError(session, publishFn,
			"OpenCode emitted an event larger than the maximum frame size")
	}

	if session.Status() == "ended" {
		// Cancelled mid-turn — do not promote late output to a completion.
		return fmt.Errorf("session ended during turn")
	}
	if result.timedOut {
		return m.publishTurnError(session, publishFn, "OpenCode turn timed out")
	}

	// Exact-id resume failed with a recognized missing/stale session. Require a
	// non-zero exit so ordinary assistant text mentioning "session not found"
	// cannot trigger a costly false-positive replay. At most ONE replay per turn.
	if useNativeResume && result.exitCode != 0 &&
		looksLikeMissingOpenCodeSession(result.rawStdout, result.stderr) {
		fmt.Printf("%s[opencode-native] Native resume failed for %s — replaying bounded transcript%s\n",
			colorYellow, id, colorReset)
		session.NativeSessionID = ""
		replayPrompt := buildOpenCodeReplayPrompt(session.Transcript, text)
		beforeIDs = listOpenCodeSessionIDs(runDir)

		result2 := m.runOneShot(session, runDir, executable, replayPrompt, "", turnTimeout,
			"opencode-native:"+id+":replay", publishFn)
		if result2.err != nil {
			return m.publishTurnError(session, publishFn, fmt.Sprintf("replay failed: %v", result2.err))
		}
		m.publishStderrIfAny(session, publishFn, result2.stderr)
		if session.Status() == "ended" {
			return fmt.Errorf("session ended during turn")
		}
		if result2.timedOut {
			return m.publishTurnError(session, publishFn, "OpenCode replay recovery timed out")
		}
		if result2.frameOverflow {
			return m.publishTurnError(session, publishFn,
				"OpenCode replay emitted an event larger than the maximum frame size")
		}
		result = result2
		usedReplay = true
		// A non-zero exit after replay falls through to the unified gate below.
	}

	// After missing-session replay is handled, remaining non-zero exits (auth,
	// quota, invalid flags, tool failures) surface as turn errors even when
	// stdout carried diagnostic text — never claim success or append the
	// diagnostics to the transcript as an assistant turn.
	if result.exitCode != 0 {
		detail := strings.TrimSpace(result.text)
		if detail == "" {
			detail = strings.TrimSpace(result.stderr)
		}
		if detail != "" {
			if len(detail) > 400 {
				detail = detail[:400] + "…"
			}
			return m.publishTurnError(session, publishFn,
				fmt.Sprintf("OpenCode exited with code %d: %s", result.exitCode, detail))
		}
		return m.publishTurnError(session, publishFn,
			fmt.Sprintf("OpenCode exited with code %d and produced no response", result.exitCode))
	}
	if strings.TrimSpace(result.text) == "" {
		return m.publishTurnError(session, publishFn,
			"OpenCode produced no response (empty completion is not treated as success)")
	}

	// Adopt the native session id only after the turn is known to have
	// succeeded. A run that creates a session and then exits non-zero is
	// rejected above without appending to the transcript, so capturing its id
	// earlier would resume a conversation containing a failed/hidden turn the
	// UI and replay never saw.
	if resumeSupported && session.NativeSessionID == "" {
		if captured := firstNonEmpty(result.sessionID, captureOpenCodeNativeID(runDir, beforeIDs)); captured != "" {
			session.NativeSessionID = captured
		} else {
			fmt.Printf("%s[opencode-native] Warning: could not capture native session ID for %s%s\n",
				colorYellow, id, colorReset)
		}
	}

	// Terminal completion frame. Individual deltas were already streamed as
	// their own opencode_native_message frames during runOneShot; this frame
	// carries the coalesced assistant text so a client that missed a delta (or
	// reconnected mid-turn) still renders a complete turn.
	seq := int(atomic.AddInt64(&session.seq, 1))
	msg := resultMsg{
		ID:          session.ID,
		WorkspaceID: session.WorkspaceID,
		UID:         session.UID,
		Output:      openCodeCompletionFrame(result.text, usedReplay),
		Status:      "success",
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		Type:        "opencode_native_message",
		SessionID:   session.ID,
		Seq:         seq,
		ExitCode:    result.exitCode,
		// Durable conversation id for conversation-scoped resume, published on
		// the completion frame only — after the success gate and after the
		// capture block adopted it — so the cloud never commits an id whose
		// latest turn did not land in the transcript. Empty when capture failed
		// or the binary is below the resume floor; omitempty drops it and the
		// cloud leaves any previously committed pointer intact.
		ConversationID: session.NativeSessionID,
	}
	// Preflight marshaled size: JSON escaping can push an under-cap payload over
	// Pub/Sub's 10 MB limit, and newSessionPublishFn only LOGS publish failures.
	// Treat oversize as an explicit turn error before claiming success.
	if err := openCodeNativeEnvelopePublishable(msg); err != nil {
		return m.publishTurnError(session, publishFn, err.Error())
	}

	// Append transcript only after the completion is known to be publishable.
	session.Transcript = appendOpenCodeTranscript(session.Transcript, "user", text)
	session.Transcript = appendOpenCodeTranscript(session.Transcript, "assistant", result.text)

	publishFn(msg)

	fmt.Printf("%s[opencode-native] Turn complete on %s (chars=%d replay=%v)%s\n",
		colorGreen, id, len(result.text), usedReplay, colorReset)
	return nil
}

// openCodeCompletionFrame wraps the coalesced assistant text in the terminal
// completion envelope the frontend normalizer recognizes. The replay marker is
// a structured token stripped before rendering (same contract as Antigravity's).
func openCodeCompletionFrame(text string, usedReplay bool) string {
	payload := map[string]any{
		"type":  "aiexpedite.turn_complete",
		"text":  text,
		"final": true,
	}
	if usedReplay {
		payload["replayRecovery"] = true
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		// Marshaling a map of plain strings cannot realistically fail; fall back
		// to the raw text rather than dropping the completion.
		return text
	}
	return string(encoded)
}

/* --------------------------------------------------------------------------
   One-shot turn execution
   -------------------------------------------------------------------------- */

// openCodeRunResult is one `opencode run` invocation's outcome. `text` is the
// assistant content coalesced from the streamed JSON events; `sessionID` is the
// native id if any event carried one.
type openCodeRunResult struct {
	text          string
	rawStdout     string
	stderr        string
	sessionID     string
	exitCode      int
	timedOut      bool
	frameOverflow bool
	err           error
}

// killOpenCodeProcessTree force-kills the `opencode` process and its tool
// descendants so children that inherited the stdout/stderr pipes die too
// (otherwise they keep the pipes open and block the drain past the timeout). On
// unix it kills the whole process group (the child is the Setsid group leader);
// on Windows killProcessGroup is a no-op, so KillProcessTree uses taskkill /F /T
// to reap the tree. The tree kill runs BEFORE Process.Kill() because severing
// the parent first lets Windows re-parent the tool children out of reach.
func killOpenCodeProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	_ = KillProcessTree(pid)
	_ = cmd.Process.Kill()
	_ = killProcessGroup(pid)
}

// runOneShot spawns one `opencode run --format json` process, streams stdout
// line-by-line publishing each JSON event as its own frame, drains stderr
// concurrently (required to avoid pipe deadlock), and waits for exit.
func (m *OpenCodeNativeManager) runOneShot(
	session *OpenCodeNativeSession,
	runDir, executable, prompt, nativeID string,
	turnTimeout time.Duration,
	registryLabel string,
	publishFn PublishFunc,
) openCodeRunResult {
	// The prompt is delivered on stdin from an owner-only temp file rather than
	// argv: it never appears in a process listing, is not subject to the
	// Windows CreateProcess argv ceiling, and — unlike an in-process pipe writer
	// — cannot deadlock if the child is slow to drain stdin.
	promptPath, promptFile, promptErr := writeOpenCodePromptFile(prompt)
	if promptErr != nil {
		return openCodeRunResult{err: fmt.Errorf("could not stage the OpenCode prompt: %w", promptErr)}
	}
	// Cleanup on EVERY path (success, error, timeout, kill).
	defer func() {
		_ = promptFile.Close()
		_ = os.Remove(promptPath)
	}()

	args := buildOpenCodeNativeArgs(nativeID)
	cmd := exec.Command(executable, args...)
	// Setsid on unix (hides the console window on Windows) so the child becomes
	// its own process-group leader. A tool `opencode` spawns can outlive it and
	// keep the stdout/stderr pipes open past the timeout; because the unix
	// orphan scanner is a no-op, cancel/timeout must reap the whole group.
	detachControllingTTY(cmd)
	cmd.Dir = runDir
	cmd.Env = sanitizeOpenCodeEnv(os.Environ())
	cmd.Stdin = promptFile

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return openCodeRunResult{err: fmt.Errorf("stdout pipe: %w", err)}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return openCodeRunResult{err: fmt.Errorf("stderr pipe: %w", err)}
	}

	// Avoid spawning when End already won the race during turn setup.
	if session.Status() == "ended" {
		return openCodeRunResult{err: fmt.Errorf("session ended during turn")}
	}

	if err := cmd.Start(); err != nil {
		return openCodeRunResult{
			err: fmt.Errorf("failed to start opencode (is OpenCode installed?): %w", err),
		}
	}
	if cmd.Process != nil {
		globalProcessRegistry.Register(cmd.Process.Pid, registryLabel)
		defer globalProcessRegistry.Deregister(cmd.Process.Pid)
	}

	var timedOutFlag atomic.Bool
	timer := time.AfterFunc(turnTimeout, func() {
		timedOutFlag.Store(true)
		_ = interruptProcess(cmd)
		time.AfterFunc(openCodeNativeGracefulKillWait, func() {
			killOpenCodeProcessTree(cmd)
		})
	})
	// setActiveProcess kills immediately if End already marked the session
	// ended before cancel was stored (including the cmd.Start window).
	session.setActiveProcess(cmd, func() {
		timer.Stop()
		_ = interruptProcess(cmd)
		killOpenCodeProcessTree(cmd)
	})
	defer func() {
		timer.Stop()
		session.clearActiveProcess()
	}()

	// Drain both pipes concurrently — sequential reads deadlock when the child
	// fills the unread pipe buffer (Go exec docs).
	var (
		stream    openCodeStreamState
		stderrBuf *limitedBuffer
		wg        sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		stream = m.streamOpenCodeEvents(session, stdout, publishFn)
	}()
	go func() {
		defer wg.Done()
		stderrBuf = captureLimited(stderr, openCodeNativeMaxStderr)
	}()
	wg.Wait()

	exitCode := 0
	if waitErr := cmd.Wait(); waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			return openCodeRunResult{
				timedOut: timedOutFlag.Load(),
				err:      fmt.Errorf("opencode process error: %w", waitErr),
			}
		}
	}

	errOut := ""
	if stderrBuf != nil {
		errOut = strings.TrimSpace(stderrBuf.b.String())
	}
	return openCodeRunResult{
		text:          strings.TrimSpace(stream.text.String()),
		rawStdout:     strings.TrimSpace(stream.raw.String()),
		stderr:        errOut,
		sessionID:     stream.sessionID,
		exitCode:      exitCode,
		timedOut:      timedOutFlag.Load(),
		frameOverflow: stream.overflow,
	}
}

// openCodeStreamState accumulates what the stdout scan learned about a turn.
type openCodeStreamState struct {
	text strings.Builder
	// raw is a bounded verbatim copy of stdout. `opencode` reports a stale
	// `--session` id as a PLAIN line ("Error: Session not found"), not as a
	// JSON event, so the parsed text is empty on exactly the run that needs
	// replay recovery. Detection reads this; the transcript never does.
	raw       strings.Builder
	sessionID string
	overflow  bool
	// bytes counts raw stdout consumed so a pathological run cannot buffer
	// unbounded assistant text into memory.
	bytes int
}

// streamOpenCodeEvents scans `opencode run --format json` stdout line by line,
// publishes each JSON event verbatim as its own opencode_native_message frame,
// and accumulates the assistant text plus any native session id it carries.
//
// Non-JSON lines are forwarded too (a build that prefixes a banner should not
// black-hole the turn) but contribute no assistant text — the completion frame
// would otherwise mix log noise into the conversation transcript.
func (m *OpenCodeNativeManager) streamOpenCodeEvents(
	session *OpenCodeNativeSession,
	r interface{ Read([]byte) (int, error) },
	publishFn PublishFunc,
) openCodeStreamState {
	var state openCodeStreamState
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), openCodeNativeMaxFrameBytes)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		state.bytes += len(line)
		if state.bytes > openCodeNativeMaxStdout {
			state.overflow = true
			break
		}
		if state.raw.Len() < openCodeNativeMaxRawStdout {
			state.raw.WriteString(line)
			state.raw.WriteString("\n")
		}
		if text, sid, ok := parseOpenCodeEventLine(line); ok {
			if text != "" {
				state.text.WriteString(text)
			}
			if sid != "" && state.sessionID == "" {
				state.sessionID = sid
			}
		}
		m.publishEventFrame(session, publishFn, line)
	}
	if err := scanner.Err(); err != nil {
		// bufio.ErrTooLong (a single event beyond the frame cap) and any read
		// error both mean the turn's output cannot be trusted as complete. Fail
		// closed — the caller turns this into a fatal opencode_native_error
		// plus a terminal ended frame rather than a silent drop.
		state.overflow = true
	}
	// Drain anything left so the child never blocks on a full pipe (which would
	// deadlock cmd.Wait) after we stopped scanning.
	if state.overflow {
		_, _ = drainRemaining(r)
	}
	return state
}

// publishEventFrame emits one streamed OpenCode JSON event. Oversize envelopes
// are dropped with a warning rather than crashing the turn: the coalesced
// completion frame published at the end of Send still carries the full text,
// and that one IS size-checked before success is claimed.
func (m *OpenCodeNativeManager) publishEventFrame(session *OpenCodeNativeSession, publishFn PublishFunc, line string) {
	if publishFn == nil {
		return
	}
	seq := int(atomic.AddInt64(&session.seq, 1))
	msg := resultMsg{
		ID:          session.ID,
		WorkspaceID: session.WorkspaceID,
		UID:         session.UID,
		Output:      redactOpenCodeSecrets(line),
		Status:      "success",
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		Type:        "opencode_native_message",
		SessionID:   session.ID,
		Seq:         seq,
	}
	if err := openCodeNativeEnvelopePublishable(msg); err != nil {
		fmt.Printf("%s[opencode-native] Dropping oversize stream frame on %s: %v%s\n",
			colorYellow, session.ID, err, colorReset)
		return
	}
	publishFn(msg)
}

func drainRemaining(r interface{ Read([]byte) (int, error) }) (int, error) {
	buf := make([]byte, 32*1024)
	total := 0
	for {
		n, err := r.Read(buf)
		total += n
		if err != nil {
			return total, err
		}
	}
}

/* --------------------------------------------------------------------------
   Event parsing
   -------------------------------------------------------------------------- */

// openCodeEvent is the subset of `opencode run --format json` event fields this
// driver reads. The shape is deliberately permissive: OpenCode's event schema
// is upstream-owned and CI has no real binary, so an unrecognized event must
// degrade to "forward it, contribute no text" rather than fail the turn.
type openCodeEvent struct {
	Type string `json:"type"`
	// Text delta variants seen across `--format json` releases.
	Text  string `json:"text"`
	Delta string `json:"delta"`
	Part  struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"part"`
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	// Session id variants.
	SessionID string `json:"sessionID"`
	SessionId string `json:"sessionId"`
	Session   struct {
		ID string `json:"id"`
	} `json:"session"`
	Info struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionID"`
	} `json:"info"`
}

// parseOpenCodeEventLine extracts assistant text and any native session id from
// one JSON event line. Returns ok=false for a line that is not a JSON object,
// so a stray banner is forwarded to the UI but never treated as model output.
//
// Only ASSISTANT text is accumulated. Tool-activity and lifecycle events are
// still published (the UI renders them), but folding their payloads into the
// completion text would put tool output into the conversation transcript, which
// then gets replayed back to the model as if the assistant had said it.
func parseOpenCodeEventLine(line string) (text, sessionID string, ok bool) {
	if !strings.HasPrefix(line, "{") {
		return "", "", false
	}
	var ev openCodeEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return "", "", false
	}

	sessionID = firstNonEmpty(ev.SessionID, ev.SessionId, ev.Session.ID, ev.Info.SessionID, ev.Info.ID)

	switch {
	case isOpenCodeTextEventType(ev.Type):
		text = firstNonEmptyRaw(ev.Text, ev.Delta, ev.Part.Text)
	case ev.Part.Type == "text":
		text = ev.Part.Text
	case ev.Message.Role == "assistant":
		text = ev.Message.Content
	}
	return text, sessionID, true
}

// isOpenCodeTextEventType reports whether an event type carries assistant text.
// Matched by suffix/substring rather than an exact allowlist because the
// `--format json` type names have shifted across releases (`text`,
// `message.part.updated`, `part.updated`, `text-delta`, …) and an exact list
// would silently produce empty completions on the next rename.
func isOpenCodeTextEventType(t string) bool {
	t = strings.ToLower(strings.TrimSpace(t))
	if t == "" {
		return false
	}
	// Tool and lifecycle events must never match: their payloads are not the
	// assistant speaking.
	if strings.Contains(t, "tool") || strings.Contains(t, "error") {
		return false
	}
	return t == "text" ||
		strings.HasSuffix(t, "text") ||
		strings.Contains(t, "text-delta") ||
		strings.Contains(t, "text_delta")
}

// firstNonEmptyRaw is firstNonEmpty without the TrimSpace: a text DELTA may be
// a single meaningful space or newline, and trimming it would silently glue
// words together in the coalesced completion.
func firstNonEmptyRaw(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func looksLikeMissingOpenCodeSession(stdout, stderr string) bool {
	combined := strings.ToLower(stdout + "\n" + stderr)
	needles := []string{
		"session not found",
		"unknown session",
		"no such session",
		"invalid session",
		"could not resume",
		"failed to resume",
	}
	for _, n := range needles {
		if strings.Contains(combined, n) {
			return true
		}
	}
	return false
}

/* --------------------------------------------------------------------------
   Frames / lifecycle
   -------------------------------------------------------------------------- */

func (m *OpenCodeNativeManager) publishStderrIfAny(session *OpenCodeNativeSession, publishFn PublishFunc, errText string) {
	if errText == "" || publishFn == nil {
		return
	}
	seq := int(atomic.AddInt64(&session.seq, 1))
	publishFn(resultMsg{
		ID:          session.ID,
		WorkspaceID: session.WorkspaceID,
		UID:         session.UID,
		Output:      redactOpenCodeSecrets(errText),
		// "success" matches the documented resultMsg status enum and every
		// sibling *_stderr frame — a diagnostic stderr line is a valid
		// (non-error) frame, and consumers switching on the known status values
		// would drop an "info".
		Status:    "success",
		Ts:        time.Now().UnixMilli(),
		Version:   Version,
		Type:      "opencode_native_stderr",
		SessionID: session.ID,
		Seq:       seq,
	})
}

func (m *OpenCodeNativeManager) publishTurnError(session *OpenCodeNativeSession, publishFn PublishFunc, msg string) error {
	if publishFn != nil {
		seq := int(atomic.AddInt64(&session.seq, 1))
		publishFn(resultMsg{
			ID:          session.ID,
			WorkspaceID: session.WorkspaceID,
			UID:         session.UID,
			Output:      redactOpenCodeSecrets(msg),
			Status:      "error",
			Ts:          time.Now().UnixMilli(),
			Version:     Version,
			Type:        "opencode_native_error",
			SessionID:   session.ID,
			Seq:         seq,
		})
	}
	return fmt.Errorf("%s", msg)
}

// End terminates any in-flight turn and removes the logical session.
//
// End BETWEEN turns — no child running, the common case for a one-shot-per-turn
// kind — still tears the logical session down. There is no process exit to
// piggyback on, so a no-op here would leave the cloud reservation held.
func (m *OpenCodeNativeManager) End(id string) error {
	session := m.Get(id)
	if session == nil {
		return fmt.Errorf("opencode native session %s not found", id)
	}

	session.mu.Lock()
	if session.status == "ended" {
		session.mu.Unlock()
		// A concurrent End (retry / double-click / stale-GC race) may have set
		// "ended" but still be draining the in-flight Send on turnMu below.
		// Block on the same barrier so this duplicate End does not return —
		// letting its handler publish opencode_native_ended — before the running
		// turn has emitted its final stderr/error frames. BOUNDED — see
		// end_confirm.go for why blocking here indefinitely wedged an entire
		// device (2026-08-27).
		if !waitTurnBarrier(&session.turnMu, turnDrainConfirmTimeout) {
			return m.retainOrResolveDrainTombstone(id, session)
		}
		// A concurrent End can have removed this session and a replacement
		// Start re-taken the ID while we were on the barrier; publishing ended
		// for it would tear the replacement down (Codex P2, round 4).
		if !m.removeSessionIfSame(id, session) {
			return staleEndError("opencode native", id)
		}
		return nil
	}
	session.status = "ended"
	cancel := session.activeCancel
	proc := session.activeProcess
	session.mu.Unlock()

	if cancel != nil {
		cancel()
	} else if proc != nil && proc.Process != nil {
		_ = interruptProcess(proc)
		time.Sleep(openCodeNativeGracefulKillWait)
		killOpenCodeProcessTree(proc)
	}

	// Wait for any in-flight Send to fully drain before returning, so the
	// terminal opencode_native_ended frame the handler publishes after End
	// returns is ordered last (no post-ended stderr on stop/cancel). Returns
	// immediately when no turn is active. BOUNDED — see end_confirm.go for
	// why blocking here indefinitely wedged an entire device (2026-08-27).
	if !waitTurnBarrier(&session.turnMu, turnDrainConfirmTimeout) {
		return m.retainOrResolveDrainTombstone(id, session)
	}

	if !m.removeSessionIfSame(id, session) {
		return staleEndError("opencode native", id)
	}
	fmt.Printf("%s[opencode-native] Session %s ended%s\n", colorYellow, id, colorReset)
	return nil
}

// retainOrResolveDrainTombstone is the shared verdict for a turn drain that
// did not confirm within its bound. The wedged turn goroutine may still act
// under this session, so the session is RETAINED as a tombstone rather than
// deregistered — an immediate removal would manufacture the "not found"
// absence answer the server frees the device on while the turn might still
// be running (Codex P1; see end_confirm.go).
//
// Resolution needs BOTH halves of "this session can no longer act":
//  1. the turn process is verifiably gone (no recorded process, or an
//     OS-level probe confirming the recorded one is absent), and
//  2. the turn barrier has actually drained.
//
// (2) is not implied by (1): the turn runner clears activeProcess and then
// keeps holding turnMu while it publishes its final frames and finishes its
// post-process bookkeeping. Resolving on process absence alone freed the ID
// for a replacement Start while that goroutine was still live, letting it
// publish and mutate conversation state under the replacement session (Codex
// P2, round 3). The barrier re-test here is non-blocking — every caller has
// already waited turnDrainConfirmTimeout on it — so this adds no delay.
//
// While either half is unmet the process is re-killed and the fence stays up
// — visibly.
func (m *OpenCodeNativeManager) retainOrResolveDrainTombstone(id string, session *OpenCodeNativeSession) error {
	session.mu.Lock()
	session.endDrainUnconfirmed = true
	proc := session.activeProcess
	session.mu.Unlock()

	if proc != nil && !probeProcessGone(proc) {
		killOpenCodeProcessTree(proc)
		fmt.Printf("%s[opencode-native] Turn drain unconfirmed for %s after %s — turn process still alive; retaining tombstone%s\n",
			colorRed, id, turnDrainConfirmTimeout, colorReset)
		// Deliberately NOT "session <id> not found" — see end_confirm.go.
		return fmt.Errorf("opencode native session %s turn drain unconfirmed after %s; session retained pending process-absence verification: %w", id, turnDrainConfirmTimeout, errEndUnconfirmed)
	}

	// Process absent (or never recorded) is only half the evidence: a turn
	// goroutine still holding turnMu can publish and mutate state under this
	// ID. Non-blocking re-test — the caller already spent the full bound here.
	if !waitTurnBarrier(&session.turnMu, 0) {
		fmt.Printf("%s[opencode-native] Turn drain unconfirmed for %s after %s — turn goroutine still holding the barrier; retaining tombstone%s\n",
			colorRed, id, turnDrainConfirmTimeout, colorReset)
		return fmt.Errorf("opencode native session %s turn drain unconfirmed after %s; session retained pending turn-drain verification: %w", id, turnDrainConfirmTimeout, errEndUnconfirmed)
	}

	if !m.removeSessionIfSame(id, session) {
		return staleEndError("opencode native", id)
	}
	return fmt.Errorf("opencode native session %s not found", id)
}

func (m *OpenCodeNativeManager) Get(id string) *OpenCodeNativeSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

func (m *OpenCodeNativeManager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

func (m *OpenCodeNativeManager) CleanupStale(maxAge time.Duration) {
	ticker := time.NewTicker(openCodeNativeCleanupInterval)
	defer ticker.Stop()
	for range ticker.C {
		m.endStaleSessions(maxAge)
	}
}

// endStaleSessions ends any session older than maxAge and publishes
// opencode_native_ended so the cloud can release reservations. Like
// Antigravity — and unlike Claude/Codex/Grok, where process exit publishes
// ended — OpenCode sessions are logical, so GC must emit the frame itself using
// the publisher bound at Start/Send.
func (m *OpenCodeNativeManager) endStaleSessions(maxAge time.Duration) {
	type staleInfo struct {
		id          string
		workspaceID string
		uid         string
		publishFn   PublishFunc
	}
	m.mu.RLock()
	var stale []staleInfo
	now := time.Now()
	for id, s := range m.sessions {
		if now.Sub(s.StartedAt) > maxAge {
			s.mu.Lock()
			stale = append(stale, staleInfo{
				id:          id,
				workspaceID: s.WorkspaceID,
				uid:         s.UID,
				publishFn:   s.publishFn,
			})
			s.mu.Unlock()
		}
	}
	m.mu.RUnlock()
	for _, ss := range stale {
		fmt.Printf("%s[opencode-native] Reaping stale session %s%s\n", colorYellow, ss.id, colorReset)
		trackTerminalPublishStart()
		// errEndStaleSession is withheld for the same reason the `*_end` handler
		// withholds it: the reap raced a replacement Start, so the ID now names
		// a LIVE session and this frame — keyed only by session ID — would be
		// read as its shutdown evidence (Codex P2, round 4). errEndUnconfirmed
		// is withheld because the tombstone was retained and the process may
		// still be alive; the next GC tick retries that one.
		if err := m.End(ss.id); errors.Is(err, errEndUnconfirmed) || errors.Is(err, errEndStaleSession) {
			fmt.Printf("%s[opencode-native] Stale reap withheld ended frame for %s — %v%s\n",
				colorRed, ss.id, err, colorReset)
			trackTerminalPublishEnd()
			continue
		}
		if ss.publishFn == nil {
			trackTerminalPublishEnd()
			continue
		}
		ss.publishFn(resultMsg{
			ID:          ss.id,
			WorkspaceID: ss.workspaceID,
			UID:         ss.uid,
			Output:      "OpenCode native session expired (stale)",
			Status:      "success",
			Ts:          time.Now().UnixMilli(),
			Version:     Version,
			Type:        "opencode_native_ended",
			SessionID:   ss.id,
			ExitCode:    0,
		})
		trackTerminalPublishEnd()
	}
}

func (m *OpenCodeNativeManager) ShutdownAll() {
	m.mu.RLock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	for _, id := range ids {
		_ = m.End(id)
	}
}

func (m *OpenCodeNativeManager) removeSession(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

// removeSessionIfSame removes id only while it still maps to THIS session —
// see CodexAppServerManager.removeSessionIfSame for the reused-ID race this
// prevents (Codex P2).//
// Returns whether id is free of s afterwards. FALSE means a replacement Start
// already re-took the ID while this caller was draining — the caller's End
// succeeded, but it must not let its handler publish the terminal frame,
// because that frame is keyed only by session ID and the server would read it
// as shutdown evidence for the live replacement (Codex P2, round 4).
func (m *OpenCodeNativeManager) removeSessionIfSame(id string, s *OpenCodeNativeSession) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.sessions[id]
	if !ok {
		return true
	}
	if cur != s {
		return false
	}
	delete(m.sessions, id)
	return true
}

// openCodeNativeEnvelopePublishable returns an error when the marshaled
// resultMsg cannot fit in a single Pub/Sub publish (or cannot marshal).
func openCodeNativeEnvelopePublishable(msg resultMsg) error {
	encoded, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("OpenCode response failed to marshal: %w", err)
	}
	if len(encoded) > openCodeNativeMaxPublishSize {
		return fmt.Errorf(
			"OpenCode response marshaled to %d bytes after JSON escaping, exceeding the %d-byte publishable limit",
			len(encoded), openCodeNativeMaxPublishSize,
		)
	}
	return nil
}

/* --------------------------------------------------------------------------
   Arg / env builders
   -------------------------------------------------------------------------- */

// openCodeStrippedFlags are caller-supplied flags the manager always removes.
//
//   - --format / --print-logs would flip the child out of the JSON mode this
//     driver's parser depends on, producing an unrenderable turn.
//   - --session / --continue / --fork would re-point the conversation at
//     something other than the session the cloud reserved, letting a caller
//     read or extend a chat that is not theirs.
//
// The manager owns these positions; everything it does NOT own is forwarded.
var openCodeStrippedFlags = map[string]bool{
	"--format":     true,
	"-f":           true,
	"--session":    true,
	"-s":           true,
	"--continue":   true,
	"-c":           true,
	"--fork":       true,
	"--print-logs": true,
}

// openCodeValuedStrippedFlags are the stripped flags that consume the NEXT argv
// token as their value, so removing the flag must also remove its value (a
// dangling `json` would otherwise land as a positional prompt token).
var openCodeValuedStrippedFlags = map[string]bool{
	"--format":  true,
	"-f":        true,
	"--session": true,
	"-s":        true,
	"--fork":    true,
}

// buildOpenCodeNativeArgs builds argv for one native-chat turn.
//
// `run` and `--format json` are ALWAYS forced. The prompt is never on argv — it
// arrives on stdin from a temp file (see runOneShot) — so a bare `opencode`
// cannot fall through to the interactive TUI, which on a headless remote
// session emits escape-sequence noise and never exits.
func buildOpenCodeNativeArgs(nativeSessionID string) []string {
	args := []string{"run", "--format", "json"}
	if nativeSessionID != "" {
		// Exact-id resume only — never --continue, which resumes whatever the
		// user last ran globally, including in their own local TUI.
		args = append(args, "--session", nativeSessionID)
	}
	return args
}

// normalizeOpenCodeCallerArgs strips manager-owned flags from a caller-supplied
// argv and returns what is safe to forward (e.g. `--model`, `--agent`).
// Exported shape mirrors terminal-service's normalizeOpenCodeArgs so the two
// ends of the wire agree on which flags a caller may set.
func normalizeOpenCodeCallerArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		name := a
		hasInlineValue := false
		if idx := strings.Index(a, "="); idx > 0 {
			name = a[:idx]
			hasInlineValue = true
		}
		if openCodeStrippedFlags[name] {
			// `--flag=value` carries its value inline; the separate-value form
			// consumes the next token as well.
			if !hasInlineValue && openCodeValuedStrippedFlags[name] {
				i++
			}
			continue
		}
		// A bare `run` from the caller is already forced by the manager; a
		// second one would be parsed as prompt text.
		if a == "run" && len(out) == 0 {
			continue
		}
		out = append(out, a)
	}
	return out
}

// sanitizeOpenCodeEnv strips unrelated provider credentials and nested-IDE
// markers that might otherwise leak into the child or the tools it spawns.
//
// OpenCode is provider-agnostic and resolves its OWN credentials from
// ~/.local/share/opencode/auth.json, its configured provider env vars, or
// `{env:…}` / `{file:…}` substitution in opencode.json — none of which this
// list touches. What IS stripped is the set of OTHER agents' credentials, which
// OpenCode has no use for and which would be readable by whatever tool it runs
// for a remote prompt. Whole provider prefixes are denied (not just `*_API_KEY`)
// so OAuth/session tokens are covered too, and the comparison is
// case-insensitive because Windows and some shells export mixed-case names.
func sanitizeOpenCodeEnv(env []string) []string {
	denyPrefixes := []string{
		"CLAUDECODE=",
		"CLAUDE_",    // CLAUDE_CODE_OAUTH_TOKEN, CLAUDE_API_KEY, …
		"ANTHROPIC_", // ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN
		"CODEX_",     // CODEX_API_KEY, CODEX_IDE_*
		"XAI_",       // XAI_API_KEY
		"GROK_",      // GROK_API_KEY
	}
	out := make([]string, 0, len(env))
	for _, e := range env {
		upper := strings.ToUpper(e)
		deny := false
		for _, p := range denyPrefixes {
			if strings.HasPrefix(upper, p) {
				deny = true
				break
			}
		}
		if !deny {
			out = append(out, e)
		}
	}
	return out
}

/* --------------------------------------------------------------------------
   Prompt file
   -------------------------------------------------------------------------- */

// cliPromptTempDir returns (creating if needed) an owner-only scratch dir under
// the AI Expedite root for a CLI's prompt files, or "" on any failure — in
// which case callers fall back to the OS temp dir via os.CreateTemp(""), which
// still works and is merely not co-located with the other scratch files.
func cliPromptTempDir(name string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	dir := filepath.Join(home, ".ai-expedite", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	return dir
}

// writeOpenCodePromptFile stages the turn prompt in an owner-only (0600) temp
// file and returns it open for reading, positioned at byte 0, ready to be
// handed to the child as stdin. The caller MUST close and remove it on every
// path — runOneShot does so in a defer that covers success, error, timeout and
// kill.
func writeOpenCodePromptFile(prompt string) (path string, handle *os.File, err error) {
	f, err := os.CreateTemp(cliPromptTempDir("opencode-prompts"), "opencode-prompt-*.txt")
	if err != nil {
		return "", nil, err
	}
	path = f.Name()
	// The file holds only the prompt; lock it down to the owner. On Windows the
	// perm bits are advisory, but keeping them consistent with the Unix builds
	// matches the other per-session temp resources.
	if chmodErr := f.Chmod(0o600); chmodErr != nil {
		// Non-fatal — proceed with the default perms.
		_ = chmodErr
	}
	if _, writeErr := f.WriteString(prompt); writeErr != nil {
		f.Close()
		_ = os.Remove(path)
		return "", nil, writeErr
	}
	// Rewind rather than reopen: the handle is already the one the child will
	// read, and reopening by path would race a concurrent unlink.
	if _, seekErr := f.Seek(0, 0); seekErr != nil {
		f.Close()
		_ = os.Remove(path)
		return "", nil, seekErr
	}
	return path, f, nil
}

/* --------------------------------------------------------------------------
   Capability probe + native ID capture
   -------------------------------------------------------------------------- */

// Capability probe cache — avoids spawning `opencode --version` on every Start.
var (
	openCodeCapabilityMu       sync.Mutex
	openCodeCapabilityOK       bool
	openCodeCapabilityChecked  time.Time
	openCodeCapabilityErr      error
	openCodeCapabilityResumeOK bool
)

// resolveOpenCodeExecutable returns the `opencode` binary to launch: PATH
// first, then the official installer's bin dir (which macOS launchd/GUI agents
// do not inherit), then the bare name so a PATH that appears at spawn time
// still works.
func resolveOpenCodeExecutable() string {
	if p := resolveExecutable("opencode"); p != "" {
		return p
	}
	if p := resolveOpenCodeInstallerBinary(); p != "" {
		return p
	}
	return "opencode"
}

func probeOpenCodeNativeCapability() error {
	openCodeCapabilityMu.Lock()
	defer openCodeCapabilityMu.Unlock()
	if openCodeCapabilityOK && time.Since(openCodeCapabilityChecked) < openCodeCapabilityCacheTTL {
		return nil
	}
	// Negative cache is short-lived so installing/auth-fixing opencode recovers
	// quickly.
	if !openCodeCapabilityOK && openCodeCapabilityErr != nil &&
		time.Since(openCodeCapabilityChecked) < 30*time.Second {
		return openCodeCapabilityErr
	}

	version, err := probeOpenCodeVersionUncached()
	openCodeCapabilityChecked = time.Now()
	if err != nil {
		openCodeCapabilityOK = false
		openCodeCapabilityErr = err
		openCodeCapabilityResumeOK = false
		return err
	}
	openCodeCapabilityOK = true
	openCodeCapabilityErr = nil
	// An unparseable-but-successful version probe leaves resume OFF. Failing
	// closed on resume costs one replay prompt; failing open would pass
	// `--session` to a binary whose resume semantics are unknown.
	openCodeCapabilityResumeOK = version != "" && compareSemver(version, openCodeNativeMinVersion) >= 0
	return nil
}

// probeOpenCodeVersionUncached runs `opencode --version` and returns the parsed
// major.minor.patch triple. A binary that is absent or exits non-zero is a hard
// error (native chat cannot run); a binary that succeeds but prints something
// unparseable returns ("", nil) — usable, but resume stays disabled.
func probeOpenCodeVersionUncached() (string, error) {
	executable := resolveOpenCodeExecutable()
	cmd := exec.Command(executable, "--version")
	hideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("OpenCode CLI not found or not runnable: install opencode (>= %s for session resume)", openCodeNativeMinVersion)
	}
	return parseOpenCodeVersion(string(out)), nil
}

// parseOpenCodeVersion extracts a major.minor.patch triple from `opencode
// --version` output. Handles bare "0.4.2", prefixed ("opencode 0.4.2",
// "v0.4.2") and suffixed ("0.5.0-beta.1", "0.5.0+build.7") spellings. Returns
// "" when nothing parses — callers treat that as "usable but no resume" rather
// than inventing a version.
func parseOpenCodeVersion(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if m := semverRe.FindString(line); m != "" {
			return m
		}
	}
	return ""
}

// openCodeSupportsNativeResume reports whether the installed binary is at or
// above the `--session` resume floor. Falls back to a fresh probe when the
// capability cache has not been populated (e.g. a Send on a session registered
// before a manager restart).
func openCodeSupportsNativeResume() bool {
	openCodeCapabilityMu.Lock()
	populated := openCodeCapabilityOK || openCodeCapabilityErr != nil
	resumeOK := openCodeCapabilityResumeOK
	openCodeCapabilityMu.Unlock()
	if populated {
		return resumeOK
	}
	if err := probeOpenCodeNativeCapability(); err != nil {
		return false
	}
	openCodeCapabilityMu.Lock()
	defer openCodeCapabilityMu.Unlock()
	return openCodeCapabilityResumeOK
}

// resetOpenCodeCapabilityCache clears the probe cache. Test-only seam.
func resetOpenCodeCapabilityCache() {
	openCodeCapabilityMu.Lock()
	openCodeCapabilityOK = false
	openCodeCapabilityErr = nil
	openCodeCapabilityChecked = time.Time{}
	openCodeCapabilityResumeOK = false
	openCodeCapabilityMu.Unlock()
}

// openCodeStorageDir returns the directory OpenCode persists session state
// under, honoring $OPENCODE_DATA / $XDG_DATA_HOME before the default.
func openCodeStorageDir() string {
	if d := strings.TrimSpace(os.Getenv("OPENCODE_DATA")); d != "" {
		return d
	}
	if d := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); d != "" {
		return filepath.Join(d, "opencode")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "share", "opencode")
}

// listOpenCodeSessionIDs snapshots the session ids currently persisted for a
// project directory. Used as a before/after window so a session id created by
// OUR run can be told apart from one the user's own TUI created concurrently.
// Returns an empty set (never nil semantics that would skip the ambiguity
// check) when storage cannot be read.
func listOpenCodeSessionIDs(projectDir string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, dir := range openCodeSessionDirs(projectDir) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := strings.TrimSuffix(e.Name(), ".json")
			if name != "" && name != e.Name() {
				out[name] = struct{}{}
			}
		}
	}
	return out
}

// openCodeSessionDirs returns the candidate directories session metadata may
// live under. OpenCode has moved this path across releases and namespaces it by
// project, so several plausible roots are probed rather than one hard-coded
// layout — a wrong guess must degrade to "no capture" (→ bounded replay), not
// to adopting an unrelated id.
func openCodeSessionDirs(projectDir string) []string {
	base := openCodeStorageDir()
	if base == "" {
		return nil
	}
	dirs := []string{
		filepath.Join(base, "storage", "session"),
		filepath.Join(base, "storage", "session", "info"),
		filepath.Join(base, "session"),
	}
	if projectDir != "" {
		// Project-scoped layouts key on a slugified absolute path.
		slug := openCodeProjectSlug(projectDir)
		if slug != "" {
			dirs = append(dirs,
				filepath.Join(base, "project", slug, "storage", "session", "info"),
				filepath.Join(base, "project", slug, "storage", "session"),
			)
		}
	}
	return dirs
}

// openCodeProjectSlug renders an absolute path the way OpenCode's project
// namespacing does: separators and drive colons replaced with "-".
func openCodeProjectSlug(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	slug := strings.NewReplacer("\\", "-", "/", "-", ":", "-").Replace(dir)
	return strings.Trim(slug, "-")
}

// captureOpenCodeNativeID resolves the native session id our run just created,
// by diffing the storage dir against the pre-run snapshot.
//
// It requires the diff to name EXACTLY ONE new session. The user's own local
// OpenCode TUI (or a second AIExpedite session) can create a session during the
// capture window, and adopting the wrong id would resume — and extend — a
// conversation that is not this chat. When the capture is ambiguous the
// function fails closed to "" and the next turn uses bounded transcript replay
// instead of guessing.
func captureOpenCodeNativeID(projectDir string, beforeIDs map[string]struct{}) string {
	after := listOpenCodeSessionIDs(projectDir)
	var fresh []string
	for id := range after {
		if _, existed := beforeIDs[id]; !existed {
			fresh = append(fresh, id)
		}
	}
	if len(fresh) != 1 {
		return ""
	}
	return fresh[0]
}

/* --------------------------------------------------------------------------
   Transcript replay
   -------------------------------------------------------------------------- */

func appendOpenCodeTranscript(t []openCodeTurn, role, content string) []openCodeTurn {
	t = append(t, openCodeTurn{Role: role, Content: content, At: time.Now()})
	// Bound by message count.
	if len(t) > openCodeReplayMaxMessages {
		t = t[len(t)-openCodeReplayMaxMessages:]
	}
	// Bound by total chars (drop oldest first, keep the current turn).
	for openCodeTranscriptChars(t) > openCodeReplayMaxChars && len(t) > 2 {
		t = t[1:]
	}
	return t
}

func openCodeTranscriptChars(t []openCodeTurn) int {
	n := 0
	for _, x := range t {
		n += len(x.Content)
	}
	return n
}

// openCodeTurnPrompt decides what text a turn sends. A turn with a usable
// native session id, or a brand-new conversation with no prior history, sends
// the bare prompt. When there IS prior history but no resumable id — capture
// failed on an earlier turn, the device restarted, or the binary predates
// resume — it sends the bounded transcript replay so the follow-up keeps
// context instead of silently becoming stateless.
func openCodeTurnPrompt(nativeID string, transcript []openCodeTurn, newUserText string) (prompt string, replay bool) {
	if nativeID == "" && len(transcript) > 0 {
		return buildOpenCodeReplayPrompt(transcript, newUserText), true
	}
	return newUserText, false
}

func buildOpenCodeReplayPrompt(transcript []openCodeTurn, newUserText string) string {
	const preamble = "You are continuing an AIExpedite OpenCode Chat conversation after native session resume was unavailable. " +
		"Prior turns (oldest first) follow. Treat them as history only. " +
		"Answer ONLY the final user message.\n\n"

	formatTurn := func(role, content string) string {
		var b strings.Builder
		switch role {
		case "user":
			b.WriteString("User: ")
		case "assistant":
			b.WriteString("Assistant: ")
		default:
			b.WriteString(role + ": ")
		}
		b.WriteString(content)
		b.WriteString("\n\n")
		return b.String()
	}

	// The current turn is never truncated. Tail-slicing the whole body could
	// drop the leading bytes of "User: <new prompt>" when that turn is close to
	// the limit, silently changing the request OpenCode sees.
	final := "User: " + newUserText + "\n"
	budget := openCodeNativeMaxPromptBytes - len(preamble)
	if budget < 0 {
		budget = 0
	}
	if len(final) > budget {
		return preamble + final
	}

	// Drop oldest prior turns (whole turns only) until history + final fit.
	history := make([]string, 0, len(transcript))
	historySize := 0
	for _, turn := range transcript {
		s := formatTurn(turn.Role, turn.Content)
		history = append(history, s)
		historySize += len(s)
	}
	start := 0
	for start < len(history) && historySize+len(final) > budget {
		historySize -= len(history[start])
		start++
	}

	var body strings.Builder
	for _, h := range history[start:] {
		body.WriteString(h)
	}
	body.WriteString(final)
	return preamble + body.String()
}

/* --------------------------------------------------------------------------
   Redaction + dispatch predicate
   -------------------------------------------------------------------------- */

// redactOpenCodeSecrets reuses the Antigravity redaction patterns — bearer
// headers, `api_key=` pairs, long opaque blobs and OAuth URLs are
// provider-agnostic shapes, and duplicating the pattern list would let the two
// drift so one agent's frames leak what the other's mask.
func redactOpenCodeSecrets(s string) string {
	return redactAntigravitySecrets(s)
}

// isOpenCodeNativeCommand reports whether a Pub/Sub command Type belongs to the
// OpenCode native family. Mirrors shared-constants
// OPENCODE_NATIVE_COMMAND_TYPES.
func isOpenCodeNativeCommand(t string) bool {
	switch t {
	case "opencode_native_start", "opencode_native_send", "opencode_native_end":
		return true
	}
	return false
}
