package main

// AntigravityNativeManager — logical multi-turn chat sessions backed by
// one-shot `agy --print` processes with exact native resume via
// `--conversation <id>`.
//
// Capability research (agy 1.1.1 / 1.1.2):
//   - `-p` / `--print` takes the prompt as the flag value (not positional).
//   - `--conversation <uuid>` resumes the exact prior conversation.
//   - `--continue` resumes the most recent conversation globally and is
//     NEVER used (cross-chat contamination risk).
//   - After a first turn, the native conversation ID is read only from
//     ~/.gemini/antigravity-cli/cache/last_conversations.json keyed by cwd
//     (and only when that ID is new vs the pre-turn snapshot). We deliberately
//     do not adopt the newest global conversations/*.db — it may belong to
//     another cwd (local TUI or concurrent AIExpedite session). Missed capture
//     falls back to bounded transcript replay.
//   - Output is complete-only (no reliable incremental stream).
//   - Pre-chosen conversation IDs are ignored; agy always mints its own UUID.
//
// Threat model for --dangerously-skip-permissions (native chat path only):
//   Remote AIExpedite users cannot answer local interactive permission prompts
//   on the workstation. Without auto-approve the process hangs. The flag is
//   gated by: (1) per-device "Enable Antigravity chat sessions" opt-in,
//   (2) workspace membership + device ownership checks in terminal-service,
//   (3) parent orchestration approval for terminal access, (4) working-directory
//   scope on Start, (5) process registration + orphan cleanup limited to
//   AIExpedite-owned PIDs, (6) redacted telemetry (no prompts/secrets).
//   One-shot buildAntigravityInteractiveArgs is unchanged and still uses the
//   same flag for legacy terminal invocations.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	antigravityNativeMaxStdout = 8 * 1024 * 1024
	antigravityNativeMaxStderr = 1 * 1024 * 1024
	// Soft ceiling for a single print turn (CLI default is 5m).
	antigravityNativeDefaultTurnTimeout = 5 * time.Minute
	antigravityNativeGracefulKillWait   = 3 * time.Second
	antigravityNativeMaxAge             = 6 * time.Hour
	antigravityNativeCleanupInterval    = 60 * time.Second
	// Replay bounds when native resume fails (mirrored in shared-constants).
	antigravityReplayMaxMessages = 24
	antigravityReplayMaxChars    = 48_000
	// Minimum supported version for native chat (semver major.minor.patch).
	antigravityNativeMinVersion = "1.1.1"
	// Prompt is passed as `--print` flag value. Keep well under CreateProcess
	// ~32KB argv ceiling (flags + path also consume budget). Fail closed —
	// never silently truncate user input.
	antigravityNativeMaxPromptBytes = 24 * 1024
	// Cache capability probes so Start does not spawn `agy --version` on every
	// chat open. Invalidated after this TTL or when a probe fails.
	antigravityCapabilityCacheTTL = 5 * time.Minute
	// GCP Pub/Sub documented per-message publish ceiling. Authoritative gate
	// after marshaling the resultMsg envelope (JSON escaping can inflate raw
	// stdout that already fits antigravityNativeMaxStdout).
	antigravityNativeMaxPublishSize = 10_000_000
)

/* --------------------------------------------------------------------------
   Session
   -------------------------------------------------------------------------- */

// AntigravityNativeSession is a logical multi-turn conversation. Unlike
// Claude/Grok/Codex managers, there is usually no resident process between
// turns — each Send spawns a one-shot `agy --print` and waits for completion.
type AntigravityNativeSession struct {
	ID                   string
	NativeConversationID string
	Cwd                  string
	WorkspaceID          string
	UID                  string
	StartedAt            time.Time
	// Bounded transcript for replay recovery (user/assistant pairs).
	Transcript []antigravityTurn

	mu            sync.Mutex
	status        string // "idle" | "running" | "ended"
	activeProcess *exec.Cmd
	activeCancel  func()
	seq           int64
	// publishFn is the Pub/Sub publisher bound at Start (refreshed on Send).
	// Stale GC uses it to emit antigravity_native_ended when reaping — End
	// alone does not publish (the explicit antigravity_native_end handler does).
	publishFn PublishFunc
	// turnMu serialises Send so two concurrent turns cannot race on the same
	// native conversation or interleave transcript updates.
	turnMu sync.Mutex
}

type antigravityTurn struct {
	Role    string // "user" | "assistant"
	Content string
	At      time.Time
}

func (s *AntigravityNativeSession) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *AntigravityNativeSession) setStatus(st string) {
	s.mu.Lock()
	s.status = st
	s.mu.Unlock()
}

// setActiveProcess registers the in-flight one-shot process. If End already
// marked the session ended (race before activeCancel was stored), the cancel
// callback is invoked immediately so a late-started agy process cannot keep
// running tools after the chat was stopped.
func (s *AntigravityNativeSession) setActiveProcess(cmd *exec.Cmd, cancel func()) {
	s.mu.Lock()
	ended := s.status == "ended"
	s.activeProcess = cmd
	s.activeCancel = cancel
	s.mu.Unlock()
	if ended && cancel != nil {
		cancel()
	}
}

func (s *AntigravityNativeSession) clearActiveProcess() {
	s.mu.Lock()
	s.activeProcess = nil
	s.activeCancel = nil
	s.mu.Unlock()
}

/* --------------------------------------------------------------------------
   Manager
   -------------------------------------------------------------------------- */

// AntigravityNativeManager owns logical Antigravity chat sessions.
type AntigravityNativeManager struct {
	sessions map[string]*AntigravityNativeSession
	mu       sync.RWMutex
	// captureMu guards the per-cwd capture-lock registry below.
	captureMu sync.Mutex
	// cwdCaptureLocks serialises capture-scoped turns (first turn or replay
	// replacement) per cwd. It is held across the whole `agy` run — not just the
	// snapshot/capture reads — so two concurrent first turns in the same cwd
	// cannot both run `agy` in parallel and then adopt the same last-writer-wins
	// last_conversations.json mapping (which would resume each other's native
	// conversation). Different cwds still run fully in parallel.
	cwdCaptureLocks map[string]*sync.Mutex
}

// NewAntigravityNativeManager creates a fresh manager.
func NewAntigravityNativeManager() *AntigravityNativeManager {
	return &AntigravityNativeManager{
		sessions:        make(map[string]*AntigravityNativeSession),
		cwdCaptureLocks: make(map[string]*sync.Mutex),
	}
}

// cwdCaptureLock returns the shared capture mutex for a cwd, creating it once.
func (m *AntigravityNativeManager) cwdCaptureLock(cwd string) *sync.Mutex {
	m.captureMu.Lock()
	defer m.captureMu.Unlock()
	if m.cwdCaptureLocks == nil {
		m.cwdCaptureLocks = make(map[string]*sync.Mutex)
	}
	lk, ok := m.cwdCaptureLocks[cwd]
	if !ok {
		lk = &sync.Mutex{}
		m.cwdCaptureLocks[cwd] = lk
	}
	return lk
}

// Start registers a logical session. No CLI process is launched until Send.
// publishFn is retained so stale GC can emit antigravity_native_ended when the
// cloud never sends antigravity_native_end (idle expiry / dropped end command).
// onStarted is invoked after the session is registered so callers can publish
// antigravity_native_started before any later frames.
func (m *AntigravityNativeManager) Start(id, cwd string, workspaceID, uid string, publishFn PublishFunc, onStarted func()) error {
	if id == "" {
		return fmt.Errorf("sessionID is required")
	}
	if cwd == "" {
		return fmt.Errorf("cwd is required for antigravity native (must point at a workspace directory)")
	}
	if !filepath.IsAbs(cwd) {
		return fmt.Errorf("cwd must be an absolute path; got %q", cwd)
	}
	if info, err := os.Stat(cwd); err != nil {
		return fmt.Errorf("cwd %q is not accessible: %w", cwd, err)
	} else if !info.IsDir() {
		return fmt.Errorf("cwd %q is not a directory", cwd)
	}

	// Idempotent redelivery first: if the session is already registered, re-ack
	// without re-probing. A transient `agy --version` failure (cache expiry,
	// upgrade, PATH blip) must not turn a usable local session into a cloud-side
	// error and desync Pub/Sub retry state from the manager.
	m.mu.Lock()
	if existing, exists := m.sessions[id]; exists {
		ackExistingAntigravitySession(existing, id, publishFn, onStarted)
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	if err := probeAntigravityNativeCapability(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// Re-check after probe: a concurrent Start for the same id may have won.
	if existing, exists := m.sessions[id]; exists {
		ackExistingAntigravitySession(existing, id, publishFn, onStarted)
		return nil
	}

	session := &AntigravityNativeSession{
		ID:          id,
		Cwd:         cwd,
		WorkspaceID: workspaceID,
		UID:         uid,
		StartedAt:   time.Now(),
		status:      "idle",
		Transcript:  nil,
		publishFn:   publishFn,
	}
	m.sessions[id] = session

	fmt.Printf("%s[antigravity-native] Session %s registered (cwd=%s)%s\n",
		colorCyan, id, cwd, colorReset)

	if onStarted != nil {
		onStarted()
	}
	return nil
}

// ackExistingAntigravitySession refreshes the publisher binding and re-acks
// started for an already-registered logical session. Caller must hold m.mu.
func ackExistingAntigravitySession(existing *AntigravityNativeSession, id string, publishFn PublishFunc, onStarted func()) {
	// Pub/Sub redelivery / terminal-service retry of the same start must
	// re-ack started without ending the still-usable local session. Emitting
	// antigravity_native_ended here would release the cloud reservation while
	// the manager still holds the session, breaking later Sends.
	// Refresh publishFn so a newer publisher binding still reaches GC.
	if publishFn != nil {
		existing.mu.Lock()
		existing.publishFn = publishFn
		existing.mu.Unlock()
	}
	fmt.Printf("%s[antigravity-native] Session %s already registered — idempotent start ack%s\n",
		colorCyan, id, colorReset)
	if onStarted != nil {
		onStarted()
	}
}

// Send runs one user turn: spawn `agy --print <prompt>` (with --conversation
// when a native ID is known), capture complete-only stdout as the assistant
// message, update the native conversation ID, and append to the bounded
// transcript. publishFn receives message / stderr / error frames; the logical
// session stays open (idle) after a successful turn so the user can retry.
//
// Replay recovery runs only when native resume is active AND the CLI reports a
// recognized missing/stale conversation — never on generic non-zero exits
// (auth, timeout, tool failures), which would burn a second model call and
// silently start a new conversation.
func (m *AntigravityNativeManager) Send(id, text string, publishFn PublishFunc, turnTimeout time.Duration) error {
	if publishFn == nil {
		return fmt.Errorf("publishFn is required")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("input is empty")
	}
	if turnTimeout <= 0 {
		turnTimeout = antigravityNativeDefaultTurnTimeout
	}

	session := m.Get(id)
	if session == nil {
		return fmt.Errorf("antigravity native session %s not found", id)
	}
	if session.Status() == "ended" {
		return fmt.Errorf("antigravity native session %s has ended", id)
	}

	// One active turn per logical conversation.
	session.turnMu.Lock()
	defer session.turnMu.Unlock()

	if session.Status() == "ended" {
		return fmt.Errorf("antigravity native session %s has ended", id)
	}

	// Keep the GC publisher current (Send always has a live Pub/Sub binding).
	session.mu.Lock()
	session.publishFn = publishFn
	session.mu.Unlock()

	// Fail closed with a published error frame so the chat UI cannot stay
	// stuck in "running" after the HTTP send already returned 200.
	if len(text) > antigravityNativeMaxPromptBytes {
		return m.publishTurnError(session, publishFn,
			fmt.Sprintf("prompt exceeds maximum size of %d bytes", antigravityNativeMaxPromptBytes))
	}

	session.setStatus("running")
	defer func() {
		if session.Status() != "ended" {
			session.setStatus("idle")
		}
		session.clearActiveProcess()
	}()

	nativeID := session.NativeConversationID
	useNativeResume := nativeID != ""
	usedReplay := false
	needCapture := nativeID == ""

	executable := resolveExecutable("agy")
	if executable == "" {
		executable = "agy"
	}

	fmt.Printf("%s[antigravity-native] Turn on %s (resume=%v)%s\n",
		colorCyan, id, useNativeResume, colorReset)

	// Per-cwd capture lock: acquired lazily and held across the entire capturing
	// `agy` run (snapshot → run → capture) so concurrent same-cwd first turns
	// cannot misattribute each other's freshly created conversation DB. Different
	// cwds never contend, and native-resume turns (nativeID known) never acquire
	// it, so only the rare capturing turns serialize.
	var captureLock *sync.Mutex
	lockCapture := func() {
		if captureLock == nil {
			captureLock = m.cwdCaptureLock(session.Cwd)
			captureLock.Lock()
		}
	}
	defer func() {
		if captureLock != nil {
			captureLock.Unlock()
		}
	}()

	// No native ID but prior turns exist (never captured, or lost): replay the
	// bounded transcript so a follow-up keeps context instead of silently going
	// stateless. The first turn (empty transcript) still sends bare text.
	promptToSend, replayForContinuity := antigravityTurnPrompt(nativeID, session.Transcript, text)
	if replayForContinuity {
		if len(promptToSend) > antigravityNativeMaxPromptBytes {
			return m.publishTurnError(session, publishFn,
				"Antigravity has no resumable conversation and the replay prompt exceeds the size limit")
		}
		usedReplay = true
	}

	// Snapshot conversation IDs for the capture window under the per-cwd lock.
	var beforeIDs map[string]struct{}
	if needCapture {
		lockCapture()
		beforeIDs = listAntigravityConversationIDs()
	}

	outText, errText, exitCode, timedOut, truncated, runErr := m.runOneShot(
		session, executable, promptToSend, nativeID, turnTimeout, "antigravity-native:"+id,
	)
	if runErr != nil {
		return m.publishTurnError(session, publishFn, runErr.Error())
	}
	m.publishStderrIfAny(session, publishFn, errText)
	if truncated {
		// Requirements: oversize output must not be silently dropped/truncated
		// as if it were a complete assistant answer.
		return m.publishTurnError(session, publishFn,
			"Antigravity output exceeded the maximum capture size")
	}

	if session.Status() == "ended" {
		// Cancelled mid-turn — do not promote late output to a completion.
		return fmt.Errorf("session ended during turn")
	}
	if timedOut {
		return m.publishTurnError(session, publishFn, "Antigravity turn timed out")
	}

	// Exact-ID resume failed with a recognized missing/stale conversation.
	// Require a non-zero exit so ordinary assistant text that mentions
	// "conversation not found" cannot trigger a costly false-positive replay.
	// At most one replay recovery for this turn.
	if useNativeResume && exitCode != 0 && looksLikeMissingConversation(outText, errText) {
		fmt.Printf("%s[antigravity-native] Native resume failed for %s — replaying bounded transcript%s\n",
			colorYellow, id, colorReset)
		session.NativeConversationID = ""
		replayPrompt := buildAntigravityReplayPrompt(session.Transcript, text)
		if len(replayPrompt) > antigravityNativeMaxPromptBytes {
			return m.publishTurnError(session, publishFn,
				"Antigravity resume failed and replay prompt exceeds size limit")
		}
		lockCapture()
		beforeIDs = listAntigravityConversationIDs()

		out2, err2, exit2, timed2, trunc2, run2 := m.runOneShot(
			session, executable, replayPrompt, "", turnTimeout, "antigravity-native:"+id+":replay",
		)
		if run2 != nil {
			return m.publishTurnError(session, publishFn, fmt.Sprintf("replay failed: %v", run2))
		}
		m.publishStderrIfAny(session, publishFn, err2)
		if session.Status() == "ended" {
			return fmt.Errorf("session ended during turn")
		}
		if timed2 {
			return m.publishTurnError(session, publishFn, "Antigravity replay recovery timed out")
		}
		if trunc2 {
			return m.publishTurnError(session, publishFn,
				"Antigravity replay output exceeded the maximum capture size")
		}
		outText, errText, exitCode = out2, err2, exit2
		usedReplay = true
		needCapture = true
		// Non-zero exit after replay is handled by the unified exit-code gate
		// below (including stdout-bearing diagnostics such as auth/quota).
	}

	// After missing-conversation replay is handled, remaining non-zero exits
	// (auth, quota, invalid flags, tool failures) must surface as turn errors
	// even when stdout carries diagnostic text — never publish success or
	// append diagnostics into the conversation transcript as an assistant turn.
	if exitCode != 0 {
		detail := strings.TrimSpace(outText)
		if detail == "" {
			detail = strings.TrimSpace(errText)
		}
		if detail != "" {
			// Keep the user-facing message bounded; full stderr is already
			// published (redacted) via publishStderrIfAny when present.
			if len(detail) > 400 {
				detail = detail[:400] + "…"
			}
			return m.publishTurnError(session, publishFn,
				fmt.Sprintf("Antigravity exited with code %d: %s", exitCode, detail))
		}
		return m.publishTurnError(session, publishFn,
			fmt.Sprintf("Antigravity exited with code %d and produced no response", exitCode))
	}
	if outText == "" {
		return m.publishTurnError(session, publishFn,
			"Antigravity produced no response (empty completion is not treated as success)")
	}

	// Adopt the native conversation ID only after the turn is known to have
	// succeeded (exitCode == 0, non-empty output). A run that creates/updates the
	// cwd conversation mapping and then exits non-zero is rejected above without
	// appending to the transcript, so capturing its ID earlier would resume a
	// conversation containing a failed/hidden turn the UI and replay never saw.
	// Capture read runs under the per-cwd lock acquired before this turn's run.
	if needCapture || session.NativeConversationID == "" {
		lockCapture()
		captured := captureAntigravityNativeID(session.Cwd, beforeIDs)
		if captured != "" {
			session.NativeConversationID = captured
		} else if session.NativeConversationID == "" {
			fmt.Printf("%s[antigravity-native] Warning: could not capture native conversation ID for %s%s\n",
				colorYellow, id, colorReset)
		}
	}

	messageOut := outText
	if usedReplay {
		// Structured marker for frontend/telemetry — stripped before rendering.
		messageOut = "[[antigravity_replay_recovery]]\n" + outText
	}

	seq := int(atomic.AddInt64(&session.seq, 1))
	msg := resultMsg{
		ID:          session.ID,
		WorkspaceID: session.WorkspaceID,
		UID:         session.UID,
		Output:      messageOut,
		Status:      "success",
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		Type:        "antigravity_native_message",
		SessionID:   session.ID,
		Seq:         seq,
		ExitCode:    exitCode,
	}
	// Preflight marshaled size: JSON escaping of quotes/backslashes can push an
	// under-stdout-cap payload over Pub/Sub's 10 MB limit. newSessionPublishFn
	// only logs publish failures, so treat oversize as an explicit turn error
	// before appending transcript or claiming success.
	if err := antigravityNativeEnvelopePublishable(msg); err != nil {
		return m.publishTurnError(session, publishFn, err.Error())
	}

	// Append transcript only after the completion is known to be publishable.
	session.Transcript = appendAntigravityTranscript(session.Transcript, "user", text)
	session.Transcript = appendAntigravityTranscript(session.Transcript, "assistant", outText)

	publishFn(msg)

	fmt.Printf("%s[antigravity-native] Turn complete on %s (chars=%d replay=%v)%s\n",
		colorGreen, id, len(outText), usedReplay, colorReset)
	return nil
}

// killAntigravityProcessTree SIGKILLs the agy process and, on unix, its whole
// process group so tool descendants that inherited the stdout/stderr pipes die
// too (otherwise they keep the pipes open and block the drain past the timeout).
// On Windows killProcessGroup is a no-op, so this matches the prior single-
// process kill there. Errors (e.g. ESRCH when the group already exited) are
// swallowed — this is best-effort teardown.
func killAntigravityProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = killProcessGroup(cmd.Process.Pid)
}

// runOneShot spawns one `agy --print` process, drains stdout/stderr concurrently
// (required to avoid pipe deadlock), and waits for exit. Does not publish frames.
func (m *AntigravityNativeManager) runOneShot(
	session *AntigravityNativeSession,
	executable, prompt, nativeID string,
	turnTimeout time.Duration,
	registryLabel string,
) (stdoutText, stderrText string, exitCode int, timedOut, truncated bool, err error) {
	// Unattended remote chat cannot answer local permission prompts — see
	// package threat-model comment. Same prefix the approval gate displays
	// (buildAntigravityNativeGateArgs) so the two never drift.
	args := append(antigravityDangerousFlags(), buildAntigravityNativeArgs(prompt, nativeID)...)

	cmd := exec.Command(executable, args...)
	// Setsid on unix (hides the console window on Windows) so agy becomes its own
	// process-group leader. A tool `agy` spawns can outlive it and keep the stdout/
	// stderr pipes open past the timeout; because the unix orphan scanner is a
	// no-op, cancel/timeout must reap the whole group (killAntigravityProcessTree),
	// not just cmd.Process, or those descendants leak and block the drain.
	detachControllingTTY(cmd)
	cmd.Dir = session.Cwd
	cmd.Env = sanitizeAntigravityEnv(os.Environ())

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", "", 0, false, false, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", "", 0, false, false, fmt.Errorf("stderr pipe: %w", err)
	}

	// Avoid spawning when End already won the race during turn setup.
	if session.Status() == "ended" {
		return "", "", 0, false, false, fmt.Errorf("session ended during turn")
	}

	if err := cmd.Start(); err != nil {
		return "", "", 0, false, false, fmt.Errorf("failed to start agy (is Antigravity CLI installed?): %w", err)
	}
	if cmd.Process != nil {
		globalProcessRegistry.Register(cmd.Process.Pid, registryLabel)
		defer globalProcessRegistry.Deregister(cmd.Process.Pid)
	}

	var timedOutFlag atomic.Bool
	timer := time.AfterFunc(turnTimeout, func() {
		timedOutFlag.Store(true)
		_ = interruptProcess(cmd)
		time.AfterFunc(antigravityNativeGracefulKillWait, func() {
			killAntigravityProcessTree(cmd)
		})
	})
	// setActiveProcess kills immediately if End already marked the session
	// ended before cancel was stored (including the cmd.Start window).
	session.setActiveProcess(cmd, func() {
		timer.Stop()
		_ = interruptProcess(cmd)
		killAntigravityProcessTree(cmd)
	})
	defer func() {
		timer.Stop()
		session.clearActiveProcess()
	}()

	// Drain both pipes concurrently — sequential read deadlocks when the child
	// fills the unread pipe buffer (Go exec docs).
	var (
		stdoutBuf, stderrBuf *limitedBuffer
		wg                   sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		stdoutBuf = captureLimited(stdout, antigravityNativeMaxStdout)
	}()
	go func() {
		defer wg.Done()
		stderrBuf = captureLimited(stderr, antigravityNativeMaxStderr)
	}()
	wg.Wait()

	waitErr := cmd.Wait()
	exitCode = 0
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			return "", "", 0, timedOutFlag.Load(), false, fmt.Errorf("agy process error: %w", waitErr)
		}
	}
	// Fail closed only when assistant stdout was truncated. Stderr may hit its
	// smaller cap without invalidating a complete assistant response; the
	// partial stderr is still published (redacted) for diagnostics.
	out := ""
	if stdoutBuf != nil {
		out = strings.TrimSpace(stdoutBuf.b.String())
	}
	errOut := ""
	if stderrBuf != nil {
		errOut = strings.TrimSpace(stderrBuf.b.String())
	}
	trunc := stdoutBuf != nil && stdoutBuf.trunc
	return out, errOut, exitCode, timedOutFlag.Load(), trunc, nil
}

func (m *AntigravityNativeManager) publishStderrIfAny(session *AntigravityNativeSession, publishFn PublishFunc, errText string) {
	if errText == "" || publishFn == nil {
		return
	}
	seq := int(atomic.AddInt64(&session.seq, 1))
	publishFn(resultMsg{
		ID:          session.ID,
		WorkspaceID: session.WorkspaceID,
		UID:         session.UID,
		Output:      redactAntigravitySecrets(errText),
		Status:      "info",
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		Type:        "antigravity_native_stderr",
		SessionID:   session.ID,
		Seq:         seq,
	})
}

// End terminates any in-flight turn and removes the logical session.
func (m *AntigravityNativeManager) End(id string) error {
	session := m.Get(id)
	if session == nil {
		return fmt.Errorf("antigravity native session %s not found", id)
	}

	session.mu.Lock()
	if session.status == "ended" {
		session.mu.Unlock()
		m.removeSession(id)
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
		time.Sleep(antigravityNativeGracefulKillWait)
		killAntigravityProcessTree(proc)
	}

	m.removeSession(id)
	fmt.Printf("%s[antigravity-native] Session %s ended%s\n", colorYellow, id, colorReset)
	return nil
}

func (m *AntigravityNativeManager) Get(id string) *AntigravityNativeSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

func (m *AntigravityNativeManager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

func (m *AntigravityNativeManager) CleanupStale(maxAge time.Duration) {
	ticker := time.NewTicker(antigravityNativeCleanupInterval)
	defer ticker.Stop()
	for range ticker.C {
		m.endStaleSessions(maxAge)
	}
}

// endStaleSessions ends any session older than maxAge and publishes
// antigravity_native_ended so the cloud can release reservations. Unlike
// Claude/Codex/Grok (process exit → waitForExit publishes ended), Antigravity
// sessions are logical and End alone does not publish — so GC must emit the
// frame itself using the publisher bound at Start/Send.
func (m *AntigravityNativeManager) endStaleSessions(maxAge time.Duration) {
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
		fmt.Printf("%s[antigravity-native] Reaping stale session %s%s\n", colorYellow, ss.id, colorReset)
		_ = m.End(ss.id)
		if ss.publishFn == nil {
			continue
		}
		// Publish after End so a successful reaping is mirrored to the cloud.
		// Explicit antigravity_native_end still publishes from pubsub.go; this
		// path only covers idle GC when the end command never arrives.
		ss.publishFn(resultMsg{
			ID:          ss.id,
			WorkspaceID: ss.workspaceID,
			UID:         ss.uid,
			Output:      "Antigravity native session expired (stale)",
			Status:      "success",
			Ts:          time.Now().UnixMilli(),
			Version:     Version,
			Type:        "antigravity_native_ended",
			SessionID:   ss.id,
			ExitCode:    0,
		})
	}
}

func (m *AntigravityNativeManager) ShutdownAll() {
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

func (m *AntigravityNativeManager) removeSession(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

func (m *AntigravityNativeManager) publishTurnError(session *AntigravityNativeSession, publishFn PublishFunc, msg string) error {
	seq := int(atomic.AddInt64(&session.seq, 1))
	publishFn(resultMsg{
		ID:          session.ID,
		WorkspaceID: session.WorkspaceID,
		UID:         session.UID,
		Output:      redactAntigravitySecrets(msg),
		Status:      "error",
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		Type:        "antigravity_native_error",
		SessionID:   session.ID,
		Seq:         seq,
	})
	return fmt.Errorf("%s", msg)
}

// antigravityNativeEnvelopePublishable returns an error when the marshaled
// resultMsg cannot fit in a single Pub/Sub publish (or cannot marshal).
func antigravityNativeEnvelopePublishable(msg resultMsg) error {
	encoded, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("Antigravity response failed to marshal: %w", err)
	}
	if len(encoded) > antigravityNativeMaxPublishSize {
		return fmt.Errorf(
			"Antigravity response marshaled to %d bytes after JSON escaping, exceeding the %d-byte publishable limit",
			len(encoded), antigravityNativeMaxPublishSize,
		)
	}
	return nil
}

/* --------------------------------------------------------------------------
   Arg / env builders
   -------------------------------------------------------------------------- */

// buildAntigravityNativeArgs builds argv for one native-chat turn.
// Prompt is always the value of --print (agy 1.1.x). Native ID, when set,
// is passed via --conversation. --continue is never emitted.
//
// The legacy one-shot builder (buildAntigravityInteractiveArgs) remains
// unchanged for generic terminal invocations.
func buildAntigravityNativeArgs(prompt, nativeConversationID string) []string {
	args := make([]string, 0, 6)
	// --print takes the prompt as its value (verified agy 1.1.2).
	args = append(args, "--print", prompt)
	if nativeConversationID != "" {
		// Exact-ID resume only — never --continue.
		args = append(args, "--conversation", nativeConversationID)
	}
	return args
}

// buildAntigravityNativeGateArgs returns the argv shape the approval dialog and
// allowlist gate should match: exactly what runOneShot spawns for a first turn
// (--dangerously-skip-permissions prepended, prompt placeholder). Kept here so
// the gate and the real launch never drift — a narrowed allowlist must approve
// the permission-skipping flag it will actually run with.
func buildAntigravityNativeGateArgs() []string {
	return append(antigravityDangerousFlags(), buildAntigravityNativeArgs("<prompt>", "")...)
}

// antigravityDangerousFlags is the prefix runOneShot prepends before the
// per-turn argv. Centralised so the gate and the launch stay in lockstep.
func antigravityDangerousFlags() []string {
	return []string{"--dangerously-skip-permissions"}
}

func sanitizeAntigravityEnv(env []string) []string {
	// Strip unrelated provider credentials that might leak into child tools.
	// Compare case-insensitively: Windows and some shells export mixed/lower
	// names (OpenAI_API_KEY, anthropic_api_key) that would otherwise survive
	// into `agy --dangerously-skip-permissions` tool subprocesses.
	denyPrefixes := []string{
		"ANTHROPIC_API_KEY=",
		"OPENAI_API_KEY=",
		"XAI_API_KEY=",
		"CODEX_API_KEY=",
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
   Capability probe + native ID capture
   -------------------------------------------------------------------------- */

var antigravityVersionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// Capability probe cache — avoids spawning `agy --version` on every Start.
var (
	antigravityCapabilityMu      sync.Mutex
	antigravityCapabilityOK      bool
	antigravityCapabilityChecked time.Time
	antigravityCapabilityErr     error
)

func probeAntigravityNativeCapability() error {
	antigravityCapabilityMu.Lock()
	defer antigravityCapabilityMu.Unlock()
	if antigravityCapabilityOK && time.Since(antigravityCapabilityChecked) < antigravityCapabilityCacheTTL {
		return nil
	}
	// Negative cache is short-lived so installing/auth-fixing agy recovers quickly.
	if !antigravityCapabilityOK && antigravityCapabilityErr != nil &&
		time.Since(antigravityCapabilityChecked) < 30*time.Second {
		return antigravityCapabilityErr
	}

	err := probeAntigravityNativeCapabilityUncached()
	antigravityCapabilityChecked = time.Now()
	if err != nil {
		antigravityCapabilityOK = false
		antigravityCapabilityErr = err
		return err
	}
	antigravityCapabilityOK = true
	antigravityCapabilityErr = nil
	return nil
}

func probeAntigravityNativeCapabilityUncached() error {
	executable := resolveExecutable("agy")
	if executable == "" {
		// Still try bare name — may be on PATH at spawn time.
		executable = "agy"
	}
	cmd := exec.Command(executable, "--version")
	hideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Some builds print version to stdout with exit 0 only via bare invoke.
		cmd2 := exec.Command(executable, "version")
		hideWindow(cmd2)
		out2, err2 := cmd2.CombinedOutput()
		if err2 != nil {
			return fmt.Errorf("Antigravity CLI not found or not runnable: install agy ≥ %s", antigravityNativeMinVersion)
		}
		out = out2
	}
	ver := strings.TrimSpace(string(out))
	if ver == "" {
		// Empty but successful version probe — accept (rare builds).
		return nil
	}
	m := antigravityVersionRe.FindStringSubmatch(ver)
	if m == nil {
		// Unknown version format — fail closed for native chat.
		return fmt.Errorf("unsupported Antigravity CLI version %q (need ≥ %s)", ver, antigravityNativeMinVersion)
	}
	if compareSemver(m[0], antigravityNativeMinVersion) < 0 {
		return fmt.Errorf("Antigravity CLI %s is below minimum %s for native chat", m[0], antigravityNativeMinVersion)
	}
	return nil
}

// compareSemver returns -1/0/1 for a < b / a == b / a > b (major.minor.patch).
func compareSemver(a, b string) int {
	pa := antigravityVersionRe.FindStringSubmatch(a)
	pb := antigravityVersionRe.FindStringSubmatch(b)
	if pa == nil || pb == nil {
		return strings.Compare(a, b)
	}
	for i := 1; i <= 3; i++ {
		var ai, bi int
		fmt.Sscanf(pa[i], "%d", &ai)
		fmt.Sscanf(pb[i], "%d", &bi)
		if ai < bi {
			return -1
		}
		if ai > bi {
			return 1
		}
	}
	return 0
}

func antigravityHomeBase() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".gemini", "antigravity-cli")
}

func listAntigravityConversationIDs() map[string]struct{} {
	out := make(map[string]struct{})
	base := antigravityHomeBase()
	if base == "" {
		return out
	}
	dir := filepath.Join(base, "conversations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".db") && !strings.Contains(name, "-shm") && !strings.Contains(name, "-wal") {
			id := strings.TrimSuffix(name, ".db")
			out[id] = struct{}{}
		}
	}
	return out
}

func captureAntigravityNativeID(cwd string, beforeIDs map[string]struct{}) string {
	base := antigravityHomeBase()

	// When a before-snapshot is available (normal first-turn path), only accept
	// conversation IDs that appeared after the snapshot AND are mapped to this
	// cwd in last_conversations.json. Never adopt the newest global
	// conversations/*.db: another Antigravity process (local TUI or a second
	// AIExpedite session in a different cwd) can create a DB during the capture
	// window, and resuming that ID would cross-wire conversation state.
	// Same-cwd concurrent first turns are serialized by cwdCaptureLock; if the
	// cwd mapping is missing/stale after the run, return "" so Send uses
	// bounded transcript replay instead of a wrong native ID.
	if beforeIDs != nil {
		lastID := readAntigravityLastConversationID(base, cwd)
		if lastID != "" {
			if _, existed := beforeIDs[lastID]; !existed {
				if antigravityConversationDBExists(base, lastID) {
					return lastID
				}
			}
		}
		return ""
	}

	// No snapshot (tests / recovery): last_conversations only.
	return readAntigravityLastConversationID(base, cwd)
}

func readAntigravityLastConversationID(base, cwd string) string {
	if base == "" || cwd == "" {
		return ""
	}
	path := filepath.Join(base, "cache", "last_conversations.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var m map[string]string
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	id := m[cwd]
	if id == "" || !antigravityConversationDBExists(base, id) {
		return ""
	}
	return id
}

func antigravityConversationDBExists(base, id string) bool {
	if base == "" || id == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(base, "conversations", id+".db"))
	return err == nil
}

func looksLikeMissingConversation(stdout, stderr string) bool {
	combined := strings.ToLower(stdout + "\n" + stderr)
	needles := []string{
		"conversation not found",
		"unknown conversation",
		"no such conversation",
		"invalid conversation",
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
   Transcript replay
   -------------------------------------------------------------------------- */

func appendAntigravityTranscript(t []antigravityTurn, role, content string) []antigravityTurn {
	t = append(t, antigravityTurn{Role: role, Content: content, At: time.Now()})
	// Bound by message count.
	if len(t) > antigravityReplayMaxMessages {
		t = t[len(t)-antigravityReplayMaxMessages:]
	}
	// Bound by total chars (drop oldest first, keep current turn).
	for totalChars(t) > antigravityReplayMaxChars && len(t) > 2 {
		t = t[1:]
	}
	return t
}

func totalChars(t []antigravityTurn) int {
	n := 0
	for _, x := range t {
		n += len(x.Content)
	}
	return n
}

// antigravityTurnPrompt decides what text a turn sends. A turn with a known
// native conversation ID (native resume) or a brand-new conversation (no prior
// history) sends the bare prompt. When there is prior history but no resumable
// native ID — capture failed on an earlier turn, or the CLI never emitted one —
// it sends the bounded transcript replay so the follow-up keeps context instead
// of silently becoming stateless. replay reports whether replay was used.
func antigravityTurnPrompt(nativeID string, transcript []antigravityTurn, newUserText string) (prompt string, replay bool) {
	if nativeID == "" && len(transcript) > 0 {
		return buildAntigravityReplayPrompt(transcript, newUserText), true
	}
	return newUserText, false
}

func buildAntigravityReplayPrompt(transcript []antigravityTurn, newUserText string) string {
	const preamble = "You are continuing an AIExpedite Antigravity Chat conversation after native resume failed. " +
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

	// Current turn is never truncated. Tail-slicing the whole body could drop
	// the leading bytes of "User: <new prompt>" when that turn is close to the
	// send limit, silently changing the request Antigravity sees.
	final := "User: " + newUserText + "\n"
	budget := antigravityNativeMaxPromptBytes - len(preamble)
	if budget < 0 {
		budget = 0
	}

	// If preamble + current turn alone cannot fit the send limit, return that
	// oversized prompt so Send / recovery fail closed instead of rewriting the
	// user request. (Raw Send already rejects bare prompts > 24KB; preamble
	// overhead can still push a near-limit prompt over.)
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
   Helpers
   -------------------------------------------------------------------------- */

type limitedBuffer struct {
	b     strings.Builder
	limit int
	trunc bool
}

func captureLimited(r io.Reader, limit int) *limitedBuffer {
	lb := &limitedBuffer{limit: limit}
	// Byte-oriented read (not line Scanner): agy --print is complete-only text
	// and may emit long lines; Scanner would reject oversized tokens.
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			remaining := limit - lb.b.Len()
			if remaining <= 0 {
				lb.trunc = true
				// Drain the rest so the child does not block on a full pipe
				// (which would deadlock cmd.Wait).
				_, _ = io.Copy(io.Discard, r)
				return lb
			}
			if n > remaining {
				lb.b.Write(buf[:remaining])
				lb.trunc = true
				_, _ = io.Copy(io.Discard, r)
				return lb
			}
			lb.b.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
	}
	return lb
}

var antigravitySecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization:\s*bearer\s+)\S+`),
	regexp.MustCompile(`(?i)(api[_-]?key\s*[=:]\s*)\S+`),
	regexp.MustCompile(`(?i)(token\s*[=:]\s*)[A-Za-z0-9._\-]{16,}`),
	regexp.MustCompile(`https://accounts\.google\.com/[^\s]+`),
	regexp.MustCompile(`https://[^\s]*oauth[^\s]*`),
}

func redactAntigravitySecrets(s string) string {
	out := s
	for _, re := range antigravitySecretPatterns {
		out = re.ReplaceAllString(out, "${1}[REDACTED]")
	}
	// Only redact very long opaque blobs (likely tokens/JWTs), not ordinary
	// UUIDs (~36 chars) or short hashes that appear in diagnostics.
	out = regexp.MustCompile(`[A-Za-z0-9_-]{80,}`).ReplaceAllStringFunc(out, func(m string) string {
		return m[:8] + "…[REDACTED]"
	})
	return out
}

func isAntigravityNativeCommand(t string) bool {
	switch t {
	case "antigravity_native_start", "antigravity_native_send", "antigravity_native_end":
		return true
	}
	return false
}
