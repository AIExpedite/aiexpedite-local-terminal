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
//   - After a first turn, the native conversation ID is available from
//     ~/.gemini/antigravity-cli/cache/last_conversations.json keyed by cwd,
//     with a filesystem snapshot of conversations/*.db as a race-safe fallback.
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
	"bufio"
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

func (s *AntigravityNativeSession) setActiveProcess(cmd *exec.Cmd, cancel func()) {
	s.mu.Lock()
	s.activeProcess = cmd
	s.activeCancel = cancel
	s.mu.Unlock()
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
	// firstTurnMu serialises native-ID capture across sessions so concurrent
	// first turns on the same cwd cannot steal each other's last_conversations
	// mapping.
	firstTurnMu sync.Mutex
}

// NewAntigravityNativeManager creates a fresh manager.
func NewAntigravityNativeManager() *AntigravityNativeManager {
	return &AntigravityNativeManager{
		sessions: make(map[string]*AntigravityNativeSession),
	}
}

// Start registers a logical session. No CLI process is launched until Send.
// onStarted is invoked after the session is registered so callers can publish
// antigravity_native_started before any later frames.
func (m *AntigravityNativeManager) Start(id, cwd string, workspaceID, uid string, onStarted func()) error {
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

	if err := probeAntigravityNativeCapability(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.sessions[id]; exists {
		return fmt.Errorf("antigravity native session %s already exists", id)
	}

	session := &AntigravityNativeSession{
		ID:          id,
		Cwd:         cwd,
		WorkspaceID: workspaceID,
		UID:         uid,
		StartedAt:   time.Now(),
		status:      "idle",
		Transcript:  nil,
	}
	m.sessions[id] = session

	fmt.Printf("%s[antigravity-native] Session %s registered (cwd=%s)%s\n",
		colorCyan, id, cwd, colorReset)

	if onStarted != nil {
		onStarted()
	}
	return nil
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
	if len(text) > antigravityNativeMaxPromptBytes {
		return fmt.Errorf("prompt exceeds maximum size of %d bytes", antigravityNativeMaxPromptBytes)
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

	// Snapshot conversation IDs only for the capture window — do NOT hold
	// firstTurnMu across the multi-minute process (that would serialize all
	// first turns globally).
	var beforeIDs map[string]struct{}
	if needCapture {
		m.firstTurnMu.Lock()
		beforeIDs = listAntigravityConversationIDs()
		m.firstTurnMu.Unlock()
	}

	outText, errText, exitCode, timedOut, runErr := m.runOneShot(
		session, executable, text, nativeID, turnTimeout, "antigravity-native:"+id,
	)
	if runErr != nil {
		return m.publishTurnError(session, publishFn, runErr.Error())
	}
	m.publishStderrIfAny(session, publishFn, errText)

	if session.Status() == "ended" {
		// Cancelled mid-turn — do not promote late output to a completion.
		return fmt.Errorf("session ended during turn")
	}
	if timedOut {
		return m.publishTurnError(session, publishFn, "Antigravity turn timed out")
	}

	// Exact-ID resume failed with a recognized missing/stale conversation.
	// At most one replay recovery for this turn.
	if useNativeResume && looksLikeMissingConversation(outText, errText) {
		fmt.Printf("%s[antigravity-native] Native resume failed for %s — replaying bounded transcript%s\n",
			colorYellow, id, colorReset)
		session.NativeConversationID = ""
		replayPrompt := buildAntigravityReplayPrompt(session.Transcript, text)
		if len(replayPrompt) > antigravityNativeMaxPromptBytes {
			return m.publishTurnError(session, publishFn,
				"Antigravity resume failed and replay prompt exceeds size limit")
		}
		m.firstTurnMu.Lock()
		beforeIDs = listAntigravityConversationIDs()
		m.firstTurnMu.Unlock()

		out2, err2, exit2, timed2, run2 := m.runOneShot(
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
		outText, errText, exitCode = out2, err2, exit2
		usedReplay = true
		needCapture = true
		if exitCode != 0 && outText == "" {
			return m.publishTurnError(session, publishFn,
				fmt.Sprintf("Antigravity resume failed and replay recovery failed (exit %d)", exitCode))
		}
	}

	if needCapture || session.NativeConversationID == "" {
		// Serialize only the capture read of last_conversations.json.
		m.firstTurnMu.Lock()
		captured := captureAntigravityNativeID(session.Cwd, beforeIDs)
		m.firstTurnMu.Unlock()
		if captured != "" {
			session.NativeConversationID = captured
		} else if session.NativeConversationID == "" {
			fmt.Printf("%s[antigravity-native] Warning: could not capture native conversation ID for %s%s\n",
				colorYellow, id, colorReset)
		}
	}

	if outText == "" && exitCode != 0 {
		return m.publishTurnError(session, publishFn,
			fmt.Sprintf("Antigravity exited with code %d and produced no response", exitCode))
	}
	if outText == "" {
		return m.publishTurnError(session, publishFn,
			"Antigravity produced no response (empty completion is not treated as success)")
	}

	// Append transcript (bounded).
	session.Transcript = appendAntigravityTranscript(session.Transcript, "user", text)
	session.Transcript = appendAntigravityTranscript(session.Transcript, "assistant", outText)

	messageOut := outText
	if usedReplay {
		// Structured marker for frontend/telemetry — stripped before rendering.
		messageOut = "[[antigravity_replay_recovery]]\n" + outText
	}

	seq := int(atomic.AddInt64(&session.seq, 1))
	publishFn(resultMsg{
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
	})

	fmt.Printf("%s[antigravity-native] Turn complete on %s (chars=%d replay=%v)%s\n",
		colorGreen, id, len(outText), usedReplay, colorReset)
	return nil
}

// runOneShot spawns one `agy --print` process, drains stdout/stderr concurrently
// (required to avoid pipe deadlock), and waits for exit. Does not publish frames.
func (m *AntigravityNativeManager) runOneShot(
	session *AntigravityNativeSession,
	executable, prompt, nativeID string,
	turnTimeout time.Duration,
	registryLabel string,
) (stdoutText, stderrText string, exitCode int, timedOut bool, err error) {
	args := buildAntigravityNativeArgs(prompt, nativeID, false)
	// Unattended remote chat cannot answer local permission prompts — see
	// package threat-model comment.
	args = append([]string{"--dangerously-skip-permissions"}, args...)

	cmd := exec.Command(executable, args...)
	hideWindow(cmd)
	cmd.Dir = session.Cwd
	cmd.Env = sanitizeAntigravityEnv(os.Environ())

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", "", 0, false, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", "", 0, false, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return "", "", 0, false, fmt.Errorf("failed to start agy (is Antigravity CLI installed?): %w", err)
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
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		})
	})
	session.setActiveProcess(cmd, func() {
		timer.Stop()
		_ = interruptProcess(cmd)
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
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
			return "", "", 0, timedOutFlag.Load(), fmt.Errorf("agy process error: %w", waitErr)
		}
	}
	return strings.TrimSpace(stdoutBuf.String()), strings.TrimSpace(stderrBuf.String()), exitCode, timedOutFlag.Load(), nil
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
		_ = proc.Process.Kill()
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

func (m *AntigravityNativeManager) endStaleSessions(maxAge time.Duration) {
	m.mu.RLock()
	var stale []string
	now := time.Now()
	for id, s := range m.sessions {
		if now.Sub(s.StartedAt) > maxAge {
			stale = append(stale, id)
		}
	}
	m.mu.RUnlock()
	for _, id := range stale {
		fmt.Printf("%s[antigravity-native] Reaping stale session %s%s\n", colorYellow, id, colorReset)
		_ = m.End(id)
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

/* --------------------------------------------------------------------------
   Arg / env builders
   -------------------------------------------------------------------------- */

// buildAntigravityNativeArgs builds argv for one native-chat turn.
// Prompt is always the value of --print (agy 1.1.x). Native ID, when set,
// is passed via --conversation. --continue is never emitted.
//
// The legacy one-shot builder (buildAntigravityInteractiveArgs) remains
// unchanged for generic terminal invocations.
func buildAntigravityNativeArgs(prompt, nativeConversationID string, _ bool) []string {
	args := make([]string, 0, 6)
	// --print takes the prompt as its value (verified agy 1.1.2).
	args = append(args, "--print", prompt)
	if nativeConversationID != "" {
		// Exact-ID resume only — never --continue.
		args = append(args, "--conversation", nativeConversationID)
	}
	return args
}

func sanitizeAntigravityEnv(env []string) []string {
	// Strip unrelated provider credentials that might leak into child tools.
	denyPrefixes := []string{
		"ANTHROPIC_API_KEY=",
		"OPENAI_API_KEY=",
		"XAI_API_KEY=",
		"CODEX_API_KEY=",
	}
	out := make([]string, 0, len(env))
	for _, e := range env {
		deny := false
		for _, p := range denyPrefixes {
			if strings.HasPrefix(e, p) {
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
	// Prefer last_conversations.json[cwd] — documented stable mapping.
	base := antigravityHomeBase()
	if base != "" {
		path := filepath.Join(base, "cache", "last_conversations.json")
		data, err := os.ReadFile(path)
		if err == nil {
			var m map[string]string
			if json.Unmarshal(data, &m) == nil {
				if id, ok := m[cwd]; ok && id != "" {
					// Verify the conversation db exists so we don't store a stale mapping.
					dbPath := filepath.Join(base, "conversations", id+".db")
					if _, err := os.Stat(dbPath); err == nil {
						return id
					}
				}
			}
		}
	}

	// Fallback: newly created conversation db not present in beforeIDs.
	after := listAntigravityConversationIDs()
	var newest string
	var newestMod time.Time
	convDir := filepath.Join(base, "conversations")
	for id := range after {
		if _, was := beforeIDs[id]; was {
			continue
		}
		info, err := os.Stat(filepath.Join(convDir, id+".db"))
		if err != nil {
			continue
		}
		if newest == "" || info.ModTime().After(newestMod) {
			newest = id
			newestMod = info.ModTime()
		}
	}
	return newest
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

func buildAntigravityReplayPrompt(transcript []antigravityTurn, newUserText string) string {
	var b strings.Builder
	b.WriteString("You are continuing an AIExpedite Antigravity Chat conversation after native resume failed. ")
	b.WriteString("Prior turns (oldest first) follow. Treat them as history only. ")
	b.WriteString("Answer ONLY the final user message.\n\n")
	// Use bounded transcript without the just-submitted user turn if it was already appended.
	for _, turn := range transcript {
		switch turn.Role {
		case "user":
			b.WriteString("User: ")
		case "assistant":
			b.WriteString("Assistant: ")
		default:
			b.WriteString(turn.Role + ": ")
		}
		b.WriteString(turn.Content)
		b.WriteString("\n\n")
	}
	b.WriteString("User: ")
	b.WriteString(newUserText)
	b.WriteString("\n")
	// Cap total prompt size.
	s := b.String()
	if len(s) > antigravityReplayMaxChars {
		// Keep tail (includes final user message).
		s = s[len(s)-antigravityReplayMaxChars:]
	}
	return s
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
	sc := bufio.NewScanner(r)
	// Allow large lines up to limit.
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, limit)
	for sc.Scan() {
		line := sc.Text()
		if lb.b.Len()+len(line)+1 > limit {
			lb.trunc = true
			break
		}
		if lb.b.Len() > 0 {
			lb.b.WriteByte('\n')
		}
		lb.b.WriteString(line)
	}
	return lb
}

func (lb *limitedBuffer) String() string {
	s := lb.b.String()
	if lb.trunc {
		s += "\n…[truncated]"
	}
	return s
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
