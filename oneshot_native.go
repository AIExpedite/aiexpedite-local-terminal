package main

// oneShotNativeManager — the spec-driven core for CLIs that run ONE short-lived
// child per chat turn (no resident stdio server) and resume the conversation by
// an exact native session id, with a bounded transcript replay as fallback.
//
// antigravity_native.go and opencode_native.go are two hand-written copies of
// this lifecycle (~1,700 lines each, structurally identical after renaming).
// This file is the extracted core, with Muse Code (musecode_native.go) as its
// first spec; a new one-shot CLI supplies a oneShotNativeSpec — argv, event
// parser, id rule, env strip list, version floor — and no lifecycle code.
// Migrating the two older drivers onto it is a separate change: both are
// shipped, and their suites bind to package-level internals (capability-cache
// vars, struct fields), so moving them is its own reviewed regression surface.
//
// Lifecycle invariants carried over verbatim from the OpenCode manager (see
// its comments and end_confirm.go for the incident history behind each):
//   - Start only registers a logical session; redelivery re-acks without
//     re-probing and never releases the cloud reservation.
//   - One turn at a time (turnMu); End cancels the turn, kills the whole
//     process tree, and waits on a BOUNDED drain barrier, retaining a
//     tombstone rather than manufacturing "not found" while a turn may live.
//   - The cwd is re-resolved per turn (containedCwd) to close the start→send
//     symlink-swap TOCTOU.
//   - The prompt reaches the child from an owner-only (0600) temp file, never
//     argv, and the file is removed on every path.
//   - Replay recovery runs only for a recognized missing-session failure on a
//     non-zero exit, at most once per turn.
//   - The native id is published only on the completion frame of a turn that
//     succeeded and whose completion is publishable.

import (
	"bufio"
	"context"
	"crypto/rand"
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
	oneShotNativeMaxStdout = 8 * 1024 * 1024
	oneShotNativeMaxStderr = 1 * 1024 * 1024
	// Bounded verbatim stdout kept only for failure diagnosis (missing-session
	// detection). Never assistant text.
	oneShotNativeMaxRawStdout = 64 * 1024
	// Longest single JSON event line; beyond it the turn fails rather than
	// rendering a half-parsed event.
	oneShotNativeMaxFrameBytes    = 4 * 1024 * 1024
	oneShotNativeGracefulKillWait = 3 * time.Second
	oneShotNativeMaxAge           = 6 * time.Hour
	oneShotNativeCleanupInterval  = 60 * time.Second
	// Mirrors shared-constants CLI_NATIVE_REPLAY_BOUNDS.
	oneShotReplayMaxMessages = 24
	oneShotReplayMaxChars    = 48_000
	// Caps what a caller can ask the device to buffer; the prompt is never argv.
	oneShotNativeMaxPromptBytes = 1024 * 1024
	oneShotCapabilityCacheTTL   = 5 * time.Minute
	// Negative probe cache is short so installing/fixing the CLI recovers fast.
	oneShotCapabilityNegativeTTL = 30 * time.Second
	// GCP Pub/Sub per-message publish ceiling, checked after marshaling.
	oneShotNativeMaxPublishSize = 10_000_000
	// Coalesced text deltas flush at whichever comes first: this much text,
	// this long after the first buffered delta, or the next non-delta line.
	oneShotDeltaFlushBytes = 4 * 1024
	oneShotDeltaFlushDelay = 250 * time.Millisecond
)

// oneShotVersionProbeTimeout bounds a spawned `--version` capability probe.
// A var so tests can shorten it.
var oneShotVersionProbeTimeout = 15 * time.Second

/* --------------------------------------------------------------------------
   Spec
   -------------------------------------------------------------------------- */

// oneShotEvent is what a spec's parser learned from one stdout line.
type oneShotEvent struct {
	// TextDelta is incremental assistant text to append.
	TextDelta string
	// FinalText is the CLI's own authoritative full assistant text for the
	// turn (a terminal "completed" event). Wins over the accumulated deltas.
	FinalText string
	// SessionID is a native id the event carried (capture-style CLIs).
	SessionID string
	// Failure is non-empty when the CLI reported the turn failed; it is the
	// device-wrapped failure text published as the turn error.
	Failure string
	// Internal marks CLI bookkeeping no consumer renders. It still feeds the
	// turn result and the diagnostic raw buffer but is not published: every
	// published line costs a Pub/Sub message and a Firestore chunk write.
	Internal bool
	// ToolName names a tool call the CLI reported finished (Muse Code's
	// `tool.result`). The one-shot core ignores it — the raw record is still
	// forwarded — but the generic session path renders it as a
	// `[Using tool: X]` marker: a Muse run can spend many minutes in tool
	// calls before its first text, and without a frame the orchestrator's
	// inactivity window reads that silence as a dead CLI.
	ToolName string
	// Coalesce marks a pure text-delta line that may be merged with its
	// neighbours into one published frame (spec.DeltaFrame).
	Coalesce bool
}

// nativeFrameKind names a native chat kind on the wire: the part the Pub/Sub
// handler (handleOneShotNativeCommand) needs, shared by every one-shot kind.
type nativeFrameKind struct {
	// DisplayName is used in user-facing error text ("Muse Code").
	DisplayName string
	// LogTag prefixes console logs ("[musecode-native]").
	LogTag string
	// FramePrefix is the command/result-type stem ("musecode_native" →
	// musecode_native_{start,send,end} / _{started,message,stderr,error,ended}).
	FramePrefix string
}

func (k nativeFrameKind) frameType(suffix string) string {
	return k.FramePrefix + "_" + suffix
}

// oneShotNativeSpec is everything that differs between one-shot CLIs.
type oneShotNativeSpec struct {
	nativeFrameKind
	// PromptDirName is the scratch dir under ~/.ai-expedite for prompt files;
	// PromptFilePrefix names the files in it.
	PromptDirName    string
	PromptFilePrefix string
	// MinResumeVersion is the CLI version floor for native resume; below it
	// every follow-up uses the bounded transcript replay.
	MinResumeVersion string
	// DefaultTurnTimeout applies when the SEND carries no timeout.
	DefaultTurnTimeout time.Duration
	// ResolveExecutable returns the binary to launch.
	ResolveExecutable func() string
	// ReadVersion, when set, reads the installed version without spawning the
	// CLI ("" = not available, fall back to a bounded `<cli> --version`).
	// It must not consult the shared detection cache, which also stores
	// failures and would pin a cold-start timeout as "no resume".
	ReadVersion func(executable string) string
	// BuildArgs returns argv for one turn. nativeID is "" when the turn must
	// not resume; promptPath is the owner-only prompt file (also on stdin).
	BuildArgs func(nativeID, promptPath string) []string
	// ParseEventLine parses one stdout line; ok=false for a non-JSON line,
	// which is forwarded to the UI but contributes nothing.
	ParseEventLine func(line string) (oneShotEvent, bool)
	// DeltaFrame, when set, renders merged Coalesce deltas as one event line
	// in the CLI's own shape, so a token-level stream costs one publish per
	// flush window instead of one per token.
	DeltaFrame func(text string) string
	// MintNativeID, when set, marks a CLI whose resume id is CALLER-SUPPLIED
	// (the CLI creates the session under whatever id it is given). The
	// manager mints one per logical session instead of capturing it.
	MintNativeID func() string
	// ValidSeed reports whether a cloud-provided resume seed is usable.
	ValidSeed func(id string) bool
	// LooksLikeMissingSession recognizes a stale-resume failure.
	LooksLikeMissingSession func(stdout, stderr string) bool
	// StripEnvPrefixes are upper-cased env prefixes removed from the child.
	StripEnvPrefixes []string
	// ReplayPreamble opens a transcript-replay prompt.
	ReplayPreamble string
}

/* --------------------------------------------------------------------------
   Session
   -------------------------------------------------------------------------- */

type oneShotTurn struct {
	Role    string // "user" | "assistant"
	Content string
	At      time.Time
}

// oneShotNativeSession is one logical multi-turn conversation. There is no
// process between turns.
type oneShotNativeSession struct {
	ID string
	// NativeSessionID is the CLI's conversation id passed on resume.
	NativeSessionID string
	// nativeConfirmed is true once the id is known to name a conversation the
	// CLI holds: a cloud seed (published earlier by a successful turn) or a
	// turn under it succeeded. Only a confirmed id is published.
	nativeConfirmed bool
	Cwd             string
	// WorkspaceRoot is the symlink-resolved start cwd each turn re-checks.
	WorkspaceRoot string
	WorkspaceID   string
	UID           string
	StartedAt     time.Time
	Transcript    []oneShotTurn

	mu            sync.Mutex
	status        string // "idle" | "running" | "ended"
	activeProcess *exec.Cmd
	activeCancel  func()
	seq           int64
	publishFn     PublishFunc
	turnMu        sync.Mutex
	// endDrainUnconfirmed marks a retained tombstone (see end_confirm.go).
	endDrainUnconfirmed bool
}

func (s *oneShotNativeSession) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *oneShotNativeSession) setStatus(st string) {
	s.mu.Lock()
	s.status = st
	s.mu.Unlock()
}

// beginTurn transitions idle→running unless End already marked it ended.
func (s *oneShotNativeSession) beginTurn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == "ended" {
		return false
	}
	s.status = "running"
	return true
}

// setActiveProcess registers the in-flight child, cancelling it immediately
// when End won the race before the cancel callback was stored.
func (s *oneShotNativeSession) setActiveProcess(cmd *exec.Cmd, cancel func()) {
	s.mu.Lock()
	ended := s.status == "ended"
	s.activeProcess = cmd
	s.activeCancel = cancel
	s.mu.Unlock()
	if ended && cancel != nil {
		cancel()
	}
}

func (s *oneShotNativeSession) clearActiveProcess() {
	s.mu.Lock()
	s.activeProcess = nil
	s.activeCancel = nil
	s.mu.Unlock()
}

/* --------------------------------------------------------------------------
   Manager
   -------------------------------------------------------------------------- */

type oneShotNativeManager struct {
	spec     *oneShotNativeSpec
	sessions map[string]*oneShotNativeSession
	// reaping holds ids the stale reaper has taken out of sessions but whose
	// ended frame is still unpublished. Start refuses a reserved id so that
	// frame can never be attributed to a replacement session (and release its
	// cloud reservation). Guarded by mu.
	reaping map[string]struct{}
	mu      sync.RWMutex

	capMu       sync.Mutex
	capOK       bool
	capErr      error
	capChecked  time.Time
	capResumeOK bool
	// probeVersion runs `<cli> --version`; a seam so tests need no binary.
	probeVersion func() (string, error)
}

func newOneShotNativeManager(spec *oneShotNativeSpec) *oneShotNativeManager {
	m := &oneShotNativeManager{
		spec:     spec,
		sessions: make(map[string]*oneShotNativeSession),
		reaping:  make(map[string]struct{}),
	}
	m.probeVersion = m.probeVersionUncached
	return m
}

func (m *oneShotNativeManager) logf(color, format string, args ...any) {
	fmt.Printf("%s%s "+format+"%s\n", append(append([]any{color, m.spec.LogTag}, args...), colorReset)...)
}

// Start registers a logical session; no CLI process launches until Send.
// resumeSessionID seeds the conversation to continue (conversation-scoped
// resume after the previous terminal session was reclaimed).
func (m *oneShotNativeManager) Start(id, cwd, workspaceID, uid, resumeSessionID string, publishFn PublishFunc, onStarted func()) error {
	if id == "" {
		return fmt.Errorf("sessionID is required")
	}
	if cwd == "" {
		return fmt.Errorf("cwd is required for %s native (must point at a workspace directory)", m.spec.DisplayName)
	}
	if !filepath.IsAbs(cwd) {
		return fmt.Errorf("cwd must be an absolute path; got %q", cwd)
	}
	if info, err := os.Stat(cwd); err != nil {
		return fmt.Errorf("cwd %q is not accessible: %w", cwd, err)
	} else if !info.IsDir() {
		return fmt.Errorf("cwd %q is not a directory", cwd)
	}
	resumeSessionID = strings.TrimSpace(resumeSessionID)
	if resumeSessionID != "" && m.spec.ValidSeed != nil && !m.spec.ValidSeed(resumeSessionID) {
		// Fail closed: the cloud sends only the follow-up text on a resume, so
		// running it in a fresh conversation would silently drop the context.
		return fmt.Errorf("conversation resume refused: %q is not a valid %s session id", resumeSessionID, m.spec.DisplayName)
	}

	resolvedCwd, err := containedCwd(cwd, "")
	if err != nil {
		return err
	}

	// Idempotent redelivery first, without re-probing.
	m.mu.Lock()
	if existing, exists := m.sessions[id]; exists {
		err := m.ackExisting(existing, id, publishFn, onStarted)
		m.mu.Unlock()
		return err
	}
	if err := m.reapReservedErr(id); err != nil {
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()

	if err := m.probeCapability(); err != nil {
		return err
	}
	// A seed is a promise to continue a conversation; below the resume floor
	// the CLI cannot, and the cloud sends only the follow-up text, so running
	// it would silently start a stateless chat. Fail closed, as
	// applyCliResumeSeed does on the raw session path.
	if resumeSessionID != "" && !m.supportsNativeResume() {
		return fmt.Errorf("conversation resume refused: the installed %s cannot resume a session; upgrade it to %s or later on this computer", m.spec.DisplayName, m.spec.MinResumeVersion)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, exists := m.sessions[id]; exists {
		return m.ackExisting(existing, id, publishFn, onStarted)
	}
	if err := m.reapReservedErr(id); err != nil {
		return err
	}

	nativeID, confirmed := resumeSessionID, resumeSessionID != ""
	if nativeID == "" && m.spec.MintNativeID != nil {
		nativeID = m.spec.MintNativeID()
	}
	session := &oneShotNativeSession{
		ID:            id,
		Cwd:           cwd,
		WorkspaceRoot: resolvedCwd,
		WorkspaceID:   workspaceID,
		UID:           uid,
		StartedAt:     time.Now(),
		status:        "idle",
		publishFn:     publishFn,
		// Seeded before registration so a racing Send never sees an unseeded
		// session (that window would silently start a fresh conversation).
		NativeSessionID: nativeID,
		nativeConfirmed: confirmed,
	}
	m.sessions[id] = session
	// The session lives here between its per-turn processes, so the ledger
	// keeps it: a restart must be able to certify it ended (session_ledger.go).
	openLedgerLogicalSession(id)
	m.logf(colorCyan, "Session %s registered (cwd=%s resume=%v)", id, cwd, confirmed)

	if onStarted != nil {
		onStarted()
	}
	return nil
}

// ackExisting re-acks started for an already-registered session. Caller holds m.mu.
func (m *oneShotNativeManager) ackExisting(existing *oneShotNativeSession, id string, publishFn PublishFunc, onStarted func()) error {
	existing.mu.Lock()
	if existing.status == "ended" || existing.endDrainUnconfirmed {
		existing.mu.Unlock()
		return fmt.Errorf("%s native session %s is ending; retained tombstone cannot acknowledge start", m.spec.DisplayName, id)
	}
	if publishFn != nil {
		existing.publishFn = publishFn
	}
	existing.mu.Unlock()
	m.logf(colorCyan, "Session %s already registered — idempotent start ack", id)
	if onStarted != nil {
		onStarted()
	}
	return nil
}

// Send runs one user turn and leaves the logical session idle for follow-ups.
func (m *oneShotNativeManager) Send(id, text string, publishFn PublishFunc, turnTimeout time.Duration) error {
	return m.SendTurn(id, text, publishFn, turnTimeout, nil)
}

// SendTurn is Send that also calls onAccepted (when non-nil) once this
// call OWNS the turn — the session moved idle→running for it, after every
// refusal (not found, ended, a turn already in flight, oversize prompt).
// A relayed voice turn records `accepted` in the durable turn inbox there
// (relay_turn_inbox.go): Send blocks for the whole turn, so acceptance
// after it returned would make a crash mid-turn look like a turn that
// never started, and the redelivery would run it a second time.
func (m *oneShotNativeManager) SendTurn(id, text string, publishFn PublishFunc, turnTimeout time.Duration, onAccepted func() error) error {
	if publishFn == nil {
		return fmt.Errorf("publishFn is required")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("input is empty")
	}
	if turnTimeout <= 0 {
		turnTimeout = m.spec.DefaultTurnTimeout
	}
	name := m.spec.DisplayName

	session := m.Get(id)
	if session == nil {
		return fmt.Errorf("%s native session %s not found", name, id)
	}
	if session.Status() == "ended" {
		return fmt.Errorf("%s native session %s has ended", name, id)
	}
	if !session.turnMu.TryLock() {
		return fmt.Errorf("%s native session %s already has a turn in flight", name, id)
	}
	defer session.turnMu.Unlock()
	if session.Status() == "ended" {
		return fmt.Errorf("%s native session %s has ended", name, id)
	}

	session.mu.Lock()
	session.publishFn = publishFn
	session.mu.Unlock()

	if len(text) > oneShotNativeMaxPromptBytes {
		return m.publishTurnError(session, publishFn,
			fmt.Sprintf("prompt exceeds maximum size of %d bytes", oneShotNativeMaxPromptBytes))
	}
	if !session.beginTurn() {
		return fmt.Errorf("%s native session %s has ended", name, id)
	}
	defer func() {
		if session.Status() != "ended" {
			session.setStatus("idle")
		}
		session.clearActiveProcess()
	}()
	// Recorded before anything is spawned. A failure to record it refuses
	// the turn; the deferred reset puts the session back to idle.
	if onAccepted != nil {
		if err := onAccepted(); err != nil {
			return err
		}
	}

	executable := m.spec.ResolveExecutable()

	// Below the resume floor the id is never passed; follow-ups replay instead.
	resumeSupported := m.supportsNativeResume()
	nativeID := ""
	if resumeSupported {
		nativeID = session.NativeSessionID
	}
	// A minted id that no turn has confirmed yet still names THIS chat (the
	// CLI creates it on first use), so it is resumable; only an unusable id
	// forces replay.
	useNativeResume := nativeID != "" && (session.nativeConfirmed || m.spec.MintNativeID != nil)
	if !useNativeResume {
		nativeID = ""
	}
	usedReplay := false

	m.logf(colorCyan, "Turn on %s (resume=%v)", id, useNativeResume && session.nativeConfirmed)

	runDir, cwdErr := containedCwd(session.Cwd, session.WorkspaceRoot)
	if cwdErr != nil {
		return m.publishTurnError(session, publishFn,
			fmt.Sprintf("cwd containment revalidation failed: %v", cwdErr))
	}

	promptToSend, replay := oneShotTurnPrompt(m.spec.ReplayPreamble, nativeID, session.nativeConfirmed, session.Transcript, text)
	usedReplay = replay

	result := m.runOneShot(session, runDir, executable, promptToSend, nativeID, turnTimeout,
		m.spec.FramePrefix+":"+id, publishFn)
	if result.err != nil {
		m.rotateUnconfirmedID(session)
		return m.publishTurnError(session, publishFn, result.err.Error())
	}
	m.publishStderrIfAny(session, publishFn, result.stderr)
	if result.frameOverflow {
		m.rotateUnconfirmedID(session)
		return m.publishTurnError(session, publishFn,
			name+" emitted an event larger than the maximum frame size")
	}
	if session.Status() == "ended" {
		return fmt.Errorf("session ended during turn")
	}
	if result.timedOut {
		m.rotateUnconfirmedID(session)
		return m.publishTurnError(session, publishFn, name+" turn timed out")
	}

	if session.nativeConfirmed && nativeID != "" && result.exitCode != 0 &&
		m.spec.LooksLikeMissingSession != nil &&
		m.spec.LooksLikeMissingSession(result.rawStdout, result.stderr) {
		m.logf(colorYellow, "Native resume failed for %s — replaying bounded transcript", id)
		session.nativeConfirmed = false
		session.NativeSessionID = ""
		replayID := ""
		if m.spec.MintNativeID != nil && resumeSupported {
			replayID = m.spec.MintNativeID()
			session.NativeSessionID = replayID
		}
		replayPrompt := buildOneShotReplayPrompt(m.spec.ReplayPreamble, session.Transcript, text)
		result2 := m.runOneShot(session, runDir, executable, replayPrompt, replayID, turnTimeout,
			m.spec.FramePrefix+":"+id+":replay", publishFn)
		if result2.err != nil {
			m.rotateUnconfirmedID(session)
			return m.publishTurnError(session, publishFn, fmt.Sprintf("replay failed: %v", result2.err))
		}
		m.publishStderrIfAny(session, publishFn, result2.stderr)
		if session.Status() == "ended" {
			return fmt.Errorf("session ended during turn")
		}
		if result2.timedOut {
			m.rotateUnconfirmedID(session)
			return m.publishTurnError(session, publishFn, name+" replay recovery timed out")
		}
		if result2.frameOverflow {
			m.rotateUnconfirmedID(session)
			return m.publishTurnError(session, publishFn,
				name+" replay emitted an event larger than the maximum frame size")
		}
		result = result2
		usedReplay = true
	}

	// Non-zero exits (auth, quota, usage errors, tool failures) are turn
	// errors — never a success and never appended to the transcript.
	if result.exitCode != 0 || result.failure != "" {
		m.rotateUnconfirmedID(session)
		if result.failure != "" {
			return m.publishTurnError(session, publishFn, result.failure)
		}
		detail := firstNonEmpty(result.text, result.stderr)
		if detail != "" {
			if len(detail) > 400 {
				detail = detail[:400] + "…"
			}
			return m.publishTurnError(session, publishFn,
				fmt.Sprintf("%s exited with code %d: %s", name, result.exitCode, detail))
		}
		return m.publishTurnError(session, publishFn,
			fmt.Sprintf("%s exited with code %d and produced no response", name, result.exitCode))
	}
	if strings.TrimSpace(result.text) == "" {
		m.rotateUnconfirmedID(session)
		return m.publishTurnError(session, publishFn,
			name+" produced no response (empty completion is not treated as success)")
	}

	// Adopt the id only once the turn is known to have succeeded.
	if resumeSupported {
		if m.spec.MintNativeID != nil {
			session.nativeConfirmed = session.NativeSessionID != ""
		} else if !session.nativeConfirmed && result.sessionID != "" {
			session.NativeSessionID = result.sessionID
			session.nativeConfirmed = true
		}
		if !session.nativeConfirmed {
			m.logf(colorYellow, "Warning: could not capture native session ID for %s", id)
		}
	}
	// Below the floor this turn did not run inside the seeded conversation, so
	// re-publishing the seed would point the cloud at a conversation missing it.
	conversationID := ""
	if resumeSupported && session.nativeConfirmed {
		conversationID = session.NativeSessionID
	}

	seq := int(atomic.AddInt64(&session.seq, 1))
	msg := resultMsg{
		ID:          session.ID,
		WorkspaceID: session.WorkspaceID,
		UID:         session.UID,
		Output:      oneShotCompletionFrame(result.text, usedReplay),
		Status:      "success",
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		Type:        m.spec.frameType("message"),
		SessionID:   session.ID,
		Seq:         seq,
		ExitCode:    result.exitCode,
		// Published only after the success gate so the cloud never commits an
		// id whose latest turn did not land in the transcript.
		ConversationID: conversationID,
	}
	if err := m.envelopePublishable(msg); err != nil {
		return m.publishTurnError(session, publishFn, err.Error())
	}

	session.Transcript = appendOneShotTranscript(session.Transcript, "user", text)
	session.Transcript = appendOneShotTranscript(session.Transcript, "assistant", result.text)

	publishFn(msg)
	m.logf(colorGreen, "Turn complete on %s (chars=%d replay=%v)", id, len(result.text), usedReplay)
	return nil
}

// rotateUnconfirmedID replaces a minted id that no turn has confirmed after a
// failed turn. A caller-supplied id makes the CLI persist whatever the failed
// run did under it, and resuming that would continue a conversation holding a
// turn the chat and the replay transcript never saw.
func (m *oneShotNativeManager) rotateUnconfirmedID(session *oneShotNativeSession) {
	if m.spec.MintNativeID == nil || session.nativeConfirmed {
		return
	}
	session.NativeSessionID = m.spec.MintNativeID()
}

// oneShotCompletionFrame wraps the coalesced text in the terminal completion
// envelope the frontend and ai-service recognize (same contract as OpenCode).
//
// The text is redacted here, not by the caller: this frame is the one place the
// full assistant turn leaves the device (the streamed deltas already go through
// publishEventFrame's redactAgentSecrets), so a tool that echoed an inherited
// credential would otherwise republish it verbatim. Redaction is idempotent, so
// a delta already masked upstream is unaffected.
func oneShotCompletionFrame(text string, usedReplay bool) string {
	payload := map[string]any{
		"type":  "aiexpedite.turn_complete",
		"text":  redactAgentSecrets(text),
		"final": true,
	}
	if usedReplay {
		payload["replayRecovery"] = true
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return redactAgentSecrets(text)
	}
	return string(encoded)
}

/* --------------------------------------------------------------------------
   One-shot turn execution
   -------------------------------------------------------------------------- */

type oneShotRunResult struct {
	text          string
	rawStdout     string
	stderr        string
	sessionID     string
	failure       string
	exitCode      int
	timedOut      bool
	frameOverflow bool
	err           error
}

// killOneShotProcessTree kills the child and its tool descendants; the tree
// kill runs before Process.Kill so Windows cannot re-parent the tools away.
func killOneShotProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	_ = KillProcessTree(pid)
	_ = cmd.Process.Kill()
	_ = killProcessGroup(pid)
}

// oneShotShimPathEnv and oneShotShimArgEnvPrefix name the child environment
// variables a Windows `.cmd` launch carries its shim path and variable
// operands in (see oneShotShimScript).
const (
	oneShotShimPathEnv      = "AIX_ONESHOT_SHIM_PATH"
	oneShotShimArgEnvPrefix = "AIX_ONESHOT_SHIM_ARG_"
)

// newOneShotCommand builds the child for one turn or version probe. A Windows
// `.cmd` / `.bat` launcher (Muse Code's `muse.cmd`, an npm shim) cannot be
// started by CreateProcess directly, so it goes through cmd.exe via the same
// cliSmokeShimCommand route the maintenance smokes use; a native binary, or an
// argv the shim renderer refuses, spawns directly.
func newOneShotCommand(ctx context.Context, executable string, args, env []string, dir string) *exec.Cmd {
	if isWindowsShimPath(executable) {
		if script, shimEnv, ok := oneShotShimScript(executable, args, env); ok {
			return cliSmokeShimCommand(ctx, script, shimEnv, dir)
		}
	}
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Env = env
	cmd.Dir = dir
	return cmd
}

// oneShotShimScript renders a cmd.exe script for a `.cmd` launch. Fixed tokens
// (subcommands, `--flags`) stay literal; every other operand — the session id,
// the prompt file path — rides in the returned environment and is referenced
// as a quoted `%VAR%`, so cmd.exe's single expansion pass can neither re-split
// nor re-expand it. ok=false for an operand that cannot survive that quoting
// (empty — cmd.exe leaves an empty `%VAR%` literal — or holding a quote or a
// line break).
func oneShotShimScript(executable string, args, env []string) (script string, shimEnv []string, ok bool) {
	var b strings.Builder
	b.WriteString(`"%` + oneShotShimPathEnv + `%"`)
	shimEnv = setEnvVar(env, oneShotShimPathEnv, executable)
	for i, arg := range args {
		if grokSmokeFixedFlagToken(arg) || isOneShotShimWord(arg) {
			b.WriteString(" " + arg)
			continue
		}
		if arg == "" || strings.ContainsAny(arg, "\"\r\n") {
			return "", nil, false
		}
		name := fmt.Sprintf("%s%d", oneShotShimArgEnvPrefix, i)
		shimEnv = setEnvVar(shimEnv, name, arg)
		b.WriteString(` "%` + name + `%"`)
	}
	return b.String(), shimEnv, true
}

// isOneShotShimWord reports whether arg is a bare word (`exec`, `run`) that
// cmd.exe passes through unchanged.
func isOneShotShimWord(arg string) bool {
	if arg == "" {
		return false
	}
	for _, r := range arg {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

func (m *oneShotNativeManager) runOneShot(
	session *oneShotNativeSession,
	runDir, executable, prompt, nativeID string,
	turnTimeout time.Duration,
	registryLabel string,
	publishFn PublishFunc,
) oneShotRunResult {
	promptPath, promptFile, promptErr := writeOneShotPromptFile(m.spec.PromptDirName, m.spec.PromptFilePrefix, prompt)
	if promptErr != nil {
		return oneShotRunResult{err: fmt.Errorf("could not stage the %s prompt: %w", m.spec.DisplayName, promptErr)}
	}
	defer func() {
		_ = promptFile.Close()
		_ = os.Remove(promptPath)
	}()

	cmd := newOneShotCommand(context.Background(), executable, m.spec.BuildArgs(nativeID, promptPath),
		stripEnvPrefixes(os.Environ(), m.spec.StripEnvPrefixes), runDir)
	// Own process group so cancel/timeout can reap tools that inherited pipes.
	detachControllingTTY(cmd)
	cmd.Stdin = promptFile

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return oneShotRunResult{err: fmt.Errorf("stdout pipe: %w", err)}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return oneShotRunResult{err: fmt.Errorf("stderr pipe: %w", err)}
	}
	if session.Status() == "ended" {
		return oneShotRunResult{err: fmt.Errorf("session ended during turn")}
	}
	// Recorded in the spawn ledger for this turn only, so the next boot can
	// reap a surviving child (and its tools) instead of leaving an orphan
	// holding the cloud reservation (session_ledger.go).
	beginSessionSpawn(session.ID, cmd)
	if err := cmd.Start(); err != nil {
		abortSessionSpawn(session.ID)
		return oneShotRunResult{
			err: fmt.Errorf("failed to start %s (is %s installed?): %w", commandBaseName(executable), m.spec.DisplayName, err),
		}
	}
	trackSessionProcess(session.ID, cmd)
	defer untrackSessionProcess(session.ID, cmd.Process.Pid)
	if cmd.Process != nil {
		globalProcessRegistry.Register(cmd.Process.Pid, registryLabel)
		defer globalProcessRegistry.Deregister(cmd.Process.Pid)
	}

	var timedOutFlag atomic.Bool
	timer := time.AfterFunc(turnTimeout, func() {
		timedOutFlag.Store(true)
		_ = interruptProcess(cmd)
		time.AfterFunc(oneShotNativeGracefulKillWait, func() {
			killOneShotProcessTree(cmd)
		})
	})
	session.setActiveProcess(cmd, func() {
		timer.Stop()
		_ = interruptProcess(cmd)
		killOneShotProcessTree(cmd)
	})
	defer func() {
		timer.Stop()
		session.clearActiveProcess()
	}()

	var (
		stream    oneShotStreamState
		stderrBuf *limitedBuffer
		wg        sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		stream = m.streamEvents(session, stdout, publishFn)
	}()
	go func() {
		defer wg.Done()
		stderrBuf = captureLimited(stderr, oneShotNativeMaxStderr)
	}()
	wg.Wait()

	exitCode := 0
	if waitErr := cmd.Wait(); waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			exitCode = ee.ExitCode()
		} else {
			return oneShotRunResult{
				timedOut: timedOutFlag.Load(),
				err:      fmt.Errorf("%s process error: %w", commandBaseName(executable), waitErr),
			}
		}
	}

	errOut := ""
	if stderrBuf != nil {
		errOut = strings.TrimSpace(stderrBuf.b.String())
	}
	text := stream.finalText
	if text == "" {
		text = stream.text.String()
	}
	return oneShotRunResult{
		text:          strings.TrimSpace(text),
		rawStdout:     strings.TrimSpace(stream.raw.String()),
		stderr:        errOut,
		sessionID:     stream.sessionID,
		failure:       stream.failure,
		exitCode:      exitCode,
		timedOut:      timedOutFlag.Load(),
		frameOverflow: stream.overflow,
	}
}

type oneShotStreamState struct {
	text      strings.Builder
	finalText string
	raw       strings.Builder
	sessionID string
	failure   string
	overflow  bool
	bytes     int
}

// streamEvents publishes each stdout line as its own MESSAGE frame and folds
// the parsed events into the turn result. Only assistant text accumulates —
// tool payloads folded into the transcript would be replayed to the model as
// if the assistant had said them.
func (m *oneShotNativeManager) streamEvents(
	session *oneShotNativeSession,
	r interface{ Read([]byte) (int, error) },
	publishFn PublishFunc,
) oneShotStreamState {
	var state oneShotStreamState
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), oneShotNativeMaxFrameBytes)
	deltas := newDeltaCoalescer(m.spec.DeltaFrame, func(line string) {
		m.publishEventFrame(session, publishFn, line)
	})
	defer deltas.close()

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		state.bytes += len(line)
		if state.bytes > oneShotNativeMaxStdout {
			state.overflow = true
			break
		}
		if state.raw.Len() < oneShotNativeMaxRawStdout {
			state.raw.WriteString(line)
			state.raw.WriteString("\n")
		}
		if ev, ok := m.spec.ParseEventLine(line); ok {
			state.text.WriteString(ev.TextDelta)
			if ev.FinalText != "" {
				state.finalText = ev.FinalText
			}
			if ev.SessionID != "" && state.sessionID == "" {
				state.sessionID = ev.SessionID
			}
			if ev.Failure != "" && state.failure == "" {
				state.failure = ev.Failure
			}
			if ev.Internal {
				continue
			}
			if ev.Coalesce && deltas.add(ev.TextDelta) {
				continue
			}
		}
		deltas.publish(line)
	}
	if err := scanner.Err(); err != nil {
		// A frame beyond the cap or a read error: the output cannot be trusted
		// as complete, so the caller fails the turn.
		state.overflow = true
	}
	if state.overflow {
		_, _ = drainRemaining(r)
	}
	return state
}

// publishEventFrame emits one streamed event. An oversize envelope is dropped
// with a warning: the size-checked completion frame still carries the text.
func (m *oneShotNativeManager) publishEventFrame(session *oneShotNativeSession, publishFn PublishFunc, line string) {
	if publishFn == nil {
		return
	}
	seq := int(atomic.AddInt64(&session.seq, 1))
	msg := resultMsg{
		ID:          session.ID,
		WorkspaceID: session.WorkspaceID,
		UID:         session.UID,
		Output:      redactAgentSecrets(line),
		Status:      "success",
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		Type:        m.spec.frameType("message"),
		SessionID:   session.ID,
		Seq:         seq,
	}
	if err := m.envelopePublishable(msg); err != nil {
		m.logf(colorYellow, "Dropping oversize stream frame on %s: %v", session.ID, err)
		return
	}
	publishFn(msg)
}

// deltaCoalescer merges consecutive text deltas into one published frame.
// Publishing happens under its mutex so a timer flush and the scanner's next
// line cannot reorder frames.
type deltaCoalescer struct {
	render  func(string) string
	emit    func(string)
	mu      sync.Mutex
	pending strings.Builder
	timer   *time.Timer
	closed  bool
	// publishedRun is the opaque run the last published delta text ended
	// with — see maskOpaqueContinuation.
	publishedRun int
}

func newDeltaCoalescer(render func(string) string, emit func(string)) *deltaCoalescer {
	return &deltaCoalescer{render: render, emit: emit}
}

// publish emits a non-delta line after whatever text is buffered ahead of it.
// The buffered text flushes as NON-final, so an ambiguous credential tail
// survives the intervening frame: releasing `META_API_KEY=` here would strand
// its value in a later short delta that matches neither the key/value nor the
// opaque-token pattern, and a masked completion frame cannot retract a stream
// frame already published. Lifecycle and tool frames still go out in their own
// order; only the held tail — bounded by agentSecretCarryMaxBytes — is
// deferred to the next flush, and close() always releases it.
func (c *deltaCoalescer) publish(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushLocked(false)
	c.emit(line)
}

// add buffers a delta; false means coalescing is off and the caller publishes
// the line itself.
func (c *deltaCoalescer) add(text string) bool {
	if c.render == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending.WriteString(text)
	if c.pending.Len() >= oneShotDeltaFlushBytes {
		c.flushLocked(false)
	} else if c.timer == nil {
		c.timer = time.AfterFunc(oneShotDeltaFlushDelay, func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.flushLocked(false)
		})
	}
	return true
}

// flushLocked publishes the buffered deltas. Unless final, a credential-shaped
// tail stays buffered (splitRedactionCarry) so the per-frame redaction in
// publishEventFrame sees `api_key=<value>` whole even when the pair straddles
// two flushes — the later masked completion frame cannot retract a leaked
// stream frame. The carry is released by the next delta or by close() — never
// by an intervening non-delta frame.
func (c *deltaCoalescer) flushLocked(final bool) {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if c.closed || c.pending.Len() == 0 {
		return
	}
	text := c.pending.String()
	c.pending.Reset()
	if !final {
		emit, carry := splitRedactionCarry(text)
		if carry != "" {
			c.pending.WriteString(carry)
		}
		text = emit
	}
	if text == "" {
		return
	}
	text, c.publishedRun = maskOpaqueContinuation(text, c.publishedRun)
	// The completion frame still carries the full text if a render fails.
	if frame := c.render(text); frame != "" {
		c.emit(frame)
	}
}

// close flushes what is buffered and disables the timer path.
func (c *deltaCoalescer) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushLocked(true)
	c.closed = true
}

/* --------------------------------------------------------------------------
   Frames / lifecycle
   -------------------------------------------------------------------------- */

func (m *oneShotNativeManager) publishStderrIfAny(session *oneShotNativeSession, publishFn PublishFunc, errText string) {
	if errText == "" || publishFn == nil {
		return
	}
	seq := int(atomic.AddInt64(&session.seq, 1))
	publishFn(resultMsg{
		ID:          session.ID,
		WorkspaceID: session.WorkspaceID,
		UID:         session.UID,
		Output:      redactAgentSecrets(errText),
		Status:      "success",
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		Type:        m.spec.frameType("stderr"),
		SessionID:   session.ID,
		Seq:         seq,
	})
}

func (m *oneShotNativeManager) publishTurnError(session *oneShotNativeSession, publishFn PublishFunc, msg string) error {
	if publishFn != nil {
		seq := int(atomic.AddInt64(&session.seq, 1))
		publishFn(resultMsg{
			ID:          session.ID,
			WorkspaceID: session.WorkspaceID,
			UID:         session.UID,
			Output:      redactAgentSecrets(msg),
			Status:      "error",
			Ts:          time.Now().UnixMilli(),
			Version:     Version,
			Type:        m.spec.frameType("error"),
			SessionID:   session.ID,
			Seq:         seq,
		})
	}
	return fmt.Errorf("%s", msg)
}

// End terminates any in-flight turn and removes the logical session. Between
// turns it still tears the session down: there is no process exit to wait for.
func (m *oneShotNativeManager) End(id string) error {
	kind := m.spec.DisplayName + " native"
	session := m.Get(id)
	if session == nil {
		return fmt.Errorf("%s session %s not found", kind, id)
	}
	return m.endSession(id, session)
}

// endIfSame ends id only while it still maps to the sampled session. The stale
// reaper samples under m.mu and ends after releasing it; resolving the id again
// there would tear down a replacement Start that reused it.
func (m *oneShotNativeManager) endIfSame(id string, sampled *oneShotNativeSession) error {
	if m.Get(id) != sampled {
		return staleEndError(m.spec.DisplayName+" native", id)
	}
	return m.endSession(id, sampled)
}

func (m *oneShotNativeManager) endSession(id string, session *oneShotNativeSession) error {
	kind := m.spec.DisplayName + " native"
	session.mu.Lock()
	if session.status == "ended" {
		session.mu.Unlock()
		// A concurrent End is draining the turn; wait on the same bounded
		// barrier so this duplicate cannot publish ended ahead of the turn's
		// final frames.
		if !waitTurnBarrier(&session.turnMu, turnDrainConfirmTimeout) {
			return m.retainOrResolveDrainTombstone(id, session)
		}
		if !m.removeSessionIfSame(id, session) {
			return staleEndError(kind, id)
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
		time.Sleep(oneShotNativeGracefulKillWait)
		killOneShotProcessTree(proc)
	}

	if !waitTurnBarrier(&session.turnMu, turnDrainConfirmTimeout) {
		return m.retainOrResolveDrainTombstone(id, session)
	}
	if !m.removeSessionIfSame(id, session) {
		return staleEndError(kind, id)
	}
	m.logf(colorYellow, "Session %s ended", id)
	return nil
}

// retainOrResolveDrainTombstone: resolution needs BOTH the turn process gone
// AND the turn barrier drained; until then the tombstone stays (end_confirm.go).
func (m *oneShotNativeManager) retainOrResolveDrainTombstone(id string, session *oneShotNativeSession) error {
	kind := m.spec.DisplayName + " native"
	session.mu.Lock()
	session.endDrainUnconfirmed = true
	proc := session.activeProcess
	session.mu.Unlock()

	if proc != nil && !probeProcessGone(proc) {
		killOneShotProcessTree(proc)
		m.logf(colorRed, "Turn drain unconfirmed for %s after %s — turn process still alive; retaining tombstone", id, turnDrainConfirmTimeout)
		return fmt.Errorf("%s session %s turn drain unconfirmed after %s; session retained pending process-absence verification: %w", kind, id, turnDrainConfirmTimeout, errEndUnconfirmed)
	}
	if !waitTurnBarrier(&session.turnMu, 0) {
		m.logf(colorRed, "Turn drain unconfirmed for %s after %s — turn goroutine still holding the barrier; retaining tombstone", id, turnDrainConfirmTimeout)
		return fmt.Errorf("%s session %s turn drain unconfirmed after %s; session retained pending turn-drain verification: %w", kind, id, turnDrainConfirmTimeout, errEndUnconfirmed)
	}
	if !m.removeSessionIfSame(id, session) {
		return staleEndError(kind, id)
	}
	return fmt.Errorf("%s session %s not found", kind, id)
}

func (m *oneShotNativeManager) Get(id string) *oneShotNativeSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

func (m *oneShotNativeManager) HasSession(id string) bool {
	return m.Get(id) != nil
}

func (m *oneShotNativeManager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

func (m *oneShotNativeManager) CleanupStale(maxAge time.Duration) {
	ticker := time.NewTicker(oneShotNativeCleanupInterval)
	defer ticker.Stop()
	for range ticker.C {
		m.endStaleSessions(maxAge)
	}
}

// endStaleSessions ends sessions older than maxAge and publishes ended itself:
// sessions are logical, so no process exit will.
func (m *oneShotNativeManager) endStaleSessions(maxAge time.Duration) {
	type staleInfo struct {
		id          string
		session     *oneShotNativeSession
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
			stale = append(stale, staleInfo{id: id, session: s, workspaceID: s.WorkspaceID, uid: s.UID, publishFn: s.publishFn})
			s.mu.Unlock()
		}
	}
	m.mu.RUnlock()
	for _, ss := range stale {
		m.reapStaleSession(ss.id, ss.session, ss.workspaceID, ss.uid, ss.publishFn)
	}
}

// reapStaleSession ends one sampled session and publishes its ended frame with
// the logical id reserved across BOTH steps: endIfSame frees the id before the
// publish, and the frame identifies the session only by that id, so a
// replacement Start landing in the gap would have its live reservation released
// by this stale frame.
func (m *oneShotNativeManager) reapStaleSession(id string, session *oneShotNativeSession, workspaceID, uid string, publishFn PublishFunc) {
	if !m.reserveForReap(id) {
		return
	}
	defer m.releaseReap(id)

	m.logf(colorYellow, "Reaping stale session %s", id)
	trackTerminalPublishStart()
	defer trackTerminalPublishEnd()
	// Withheld for an unconfirmed or stale end — see the *_end handler.
	if err := m.endIfSame(id, session); errors.Is(err, errEndUnconfirmed) || errors.Is(err, errEndStaleSession) {
		m.logf(colorRed, "Stale reap withheld ended frame for %s — %v", id, err)
		return
	}
	if publishFn == nil {
		return
	}
	publishFn(resultMsg{
		ID:          id,
		WorkspaceID: workspaceID,
		UID:         uid,
		Output:      m.spec.DisplayName + " native session expired (stale)",
		Status:      "success",
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		Type:        m.spec.frameType("ended"),
		SessionID:   id,
		ExitCode:    0,
	})
}

func (m *oneShotNativeManager) ShutdownAll() {
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

// reserveForReap claims id for the stale reaper before it ends the session, so
// the id stays off-limits until the ended frame has been published. Returns
// false when another reaper already holds it. Caller must releaseReap on
// every path.
func (m *oneShotNativeManager) reserveForReap(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, held := m.reaping[id]; held {
		return false
	}
	m.reaping[id] = struct{}{}
	return true
}

func (m *oneShotNativeManager) releaseReap(id string) {
	m.mu.Lock()
	delete(m.reaping, id)
	m.mu.Unlock()
}

// reapReservedErr refuses a Start whose id a reaper is still publishing an
// ended frame for. Caller holds m.mu. Fail closed and let the cloud retry:
// registering here would hand that stale ended frame — which carries only the
// logical id — to the new session and release its reservation.
func (m *oneShotNativeManager) reapReservedErr(id string) error {
	if _, held := m.reaping[id]; !held {
		return nil
	}
	return fmt.Errorf("%s native session %s is being reaped; retry once its ended frame is published", m.spec.DisplayName, id)
}

// removeSessionIfSame removes id only while it still maps to s; false means a
// replacement Start re-took the id and the caller must not publish ended.
func (m *oneShotNativeManager) removeSessionIfSame(id string, s *oneShotNativeSession) bool {
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
	releaseLedgerSession(id)
	return true
}

func (m *oneShotNativeManager) envelopePublishable(msg resultMsg) error {
	encoded, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("%s response failed to marshal: %w", m.spec.DisplayName, err)
	}
	if len(encoded) > oneShotNativeMaxPublishSize {
		return fmt.Errorf(
			"%s response marshaled to %d bytes after JSON escaping, exceeding the %d-byte publishable limit",
			m.spec.DisplayName, len(encoded), oneShotNativeMaxPublishSize,
		)
	}
	return nil
}

/* --------------------------------------------------------------------------
   Capability probe
   -------------------------------------------------------------------------- */

func (m *oneShotNativeManager) probeCapability() error {
	m.capMu.Lock()
	defer m.capMu.Unlock()
	if m.capOK && time.Since(m.capChecked) < oneShotCapabilityCacheTTL {
		return nil
	}
	if !m.capOK && m.capErr != nil && time.Since(m.capChecked) < oneShotCapabilityNegativeTTL {
		return m.capErr
	}
	version, err := m.probeVersion()
	m.capChecked = time.Now()
	if err != nil {
		m.capOK, m.capErr, m.capResumeOK = false, err, false
		return err
	}
	m.capOK, m.capErr = true, nil
	// An unparseable-but-successful probe leaves resume OFF: failing closed
	// costs one replay prompt, failing open passes a resume flag to a binary
	// whose resume semantics are unknown.
	m.capResumeOK = version != "" && compareSemver(version, m.spec.MinResumeVersion) >= 0
	return nil
}

func (m *oneShotNativeManager) probeVersionUncached() (string, error) {
	executable := m.spec.ResolveExecutable()
	notRunnable := fmt.Errorf("%s CLI not found or not runnable: install it (>= %s for session resume)", m.spec.DisplayName, m.spec.MinResumeVersion)
	if m.spec.ReadVersion != nil && filepath.IsAbs(executable) {
		if v := m.spec.ReadVersion(executable); v != "" {
			return parseCLIVersionTriple(v), nil
		}
	}
	// Bounded: Start runs this on the Pub/Sub handler goroutine, and a CLI
	// wrapper that hangs (an update check, a lock) must not wedge it.
	ctx, cancel := context.WithTimeout(context.Background(), oneShotVersionProbeTimeout)
	defer cancel()
	cmd := newOneShotCommand(ctx, executable, []string{"--version"}, os.Environ(), "")
	// Hides the console on Windows; on Unix makes the child a process-group
	// leader so the tree kill below reaches its descendants (and it cannot
	// prompt on a controlling terminal).
	detachControllingTTY(cmd)
	// CommandContext alone kills only the direct child. A Windows `.cmd`
	// launcher (Muse Code's runs PowerShell) leaves its descendants holding
	// the output pipe, and CombinedOutput would then block past the deadline.
	// Kill the whole tree, and bound the pipe drain after the kill.
	cmd.Cancel = func() error {
		killOneShotProcessTree(cmd)
		return nil
	}
	cmd.WaitDelay = oneShotNativeGracefulKillWait
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", notRunnable
	}
	return parseCLIVersionTriple(string(out)), nil
}

// supportsNativeResume reports whether the binary clears the resume floor,
// probing when the cache is cold (e.g. a Send after a manager restart).
func (m *oneShotNativeManager) supportsNativeResume() bool {
	m.capMu.Lock()
	populated := m.capOK || m.capErr != nil
	resumeOK := m.capResumeOK
	m.capMu.Unlock()
	if populated {
		return resumeOK
	}
	if err := m.probeCapability(); err != nil {
		return false
	}
	m.capMu.Lock()
	defer m.capMu.Unlock()
	return m.capResumeOK
}

// parseCLIVersionTriple extracts the first major.minor.patch in `--version`
// output ("muse 1.4.0 (1.4.0-R4161.1)", "v0.4.2", "0.5.0-beta.1"); "" when
// nothing parses — usable, but resume stays off.
func parseCLIVersionTriple(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if m := semverRe.FindString(strings.TrimSpace(line)); m != "" {
			return m
		}
	}
	return ""
}

/* --------------------------------------------------------------------------
   Env / prompt file / ids
   -------------------------------------------------------------------------- */

// stripEnvPrefixes removes variables whose upper-cased name starts with any
// prefix. Case-insensitive because Windows and some shells export mixed case.
func stripEnvPrefixes(env []string, prefixes []string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		upper := strings.ToUpper(e)
		deny := false
		for _, p := range prefixes {
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

// writeOneShotPromptFile stages the prompt in an owner-only temp file, open and
// rewound for the child's stdin. The caller closes and removes it on every path.
func writeOneShotPromptFile(dirName, filePrefix, prompt string) (path string, handle *os.File, err error) {
	f, err := os.CreateTemp(cliPromptTempDir(dirName), filePrefix+"-*.txt")
	if err != nil {
		return "", nil, err
	}
	path = f.Name()
	_ = f.Chmod(0o600) // advisory on Windows; non-fatal everywhere
	if _, writeErr := f.WriteString(prompt); writeErr != nil {
		f.Close()
		_ = os.Remove(path)
		return "", nil, writeErr
	}
	if _, seekErr := f.Seek(0, 0); seekErr != nil {
		f.Close()
		_ = os.Remove(path)
		return "", nil, seekErr
	}
	return path, f, nil
}

// newRandomUUID returns a lowercase RFC 4122 version-4 UUID.
func newRandomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on supported platforms; an empty id makes
		// the turn fall back to replay rather than share a predictable id.
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

/* --------------------------------------------------------------------------
   Transcript replay
   -------------------------------------------------------------------------- */

func appendOneShotTranscript(t []oneShotTurn, role, content string) []oneShotTurn {
	t = append(t, oneShotTurn{Role: role, Content: content, At: time.Now()})
	if len(t) > oneShotReplayMaxMessages {
		t = t[len(t)-oneShotReplayMaxMessages:]
	}
	for oneShotTranscriptChars(t) > oneShotReplayMaxChars && len(t) > 2 {
		t = t[1:]
	}
	return t
}

func oneShotTranscriptChars(t []oneShotTurn) int {
	n := 0
	for _, x := range t {
		n += len(x.Content)
	}
	return n
}

// oneShotTurnPrompt sends the bare prompt when the turn resumes a confirmed
// conversation or there is no history; otherwise the bounded replay, so a
// follow-up never silently becomes stateless.
func oneShotTurnPrompt(preamble, nativeID string, confirmed bool, transcript []oneShotTurn, newUserText string) (prompt string, replay bool) {
	if len(transcript) > 0 && (nativeID == "" || !confirmed) {
		return buildOneShotReplayPrompt(preamble, transcript, newUserText), true
	}
	return newUserText, false
}

// buildOneShotReplayPrompt drops whole oldest turns until history plus the
// never-truncated current turn fit the prompt cap.
func buildOneShotReplayPrompt(preamble string, transcript []oneShotTurn, newUserText string) string {
	formatTurn := func(role, content string) string {
		switch role {
		case "user":
			return "User: " + content + "\n\n"
		case "assistant":
			return "Assistant: " + content + "\n\n"
		}
		return role + ": " + content + "\n\n"
	}
	final := "User: " + newUserText + "\n"
	budget := oneShotNativeMaxPromptBytes - len(preamble)
	if budget < 0 {
		budget = 0
	}
	if len(final) > budget {
		return preamble + final
	}
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
