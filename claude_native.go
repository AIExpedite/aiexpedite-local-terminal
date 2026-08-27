package main

// ClaudeNativeManager — long-lived `claude --output-format stream-json
// --input-format stream-json …` processes, one per chat-direct Claude Code
// session.
//
// This mirrors CodexAppServerManager (codex_appserver.go) and GrokACPManager
// (grok_acp.go): the child speaks a structured streaming protocol and we
// forward its frames to the cloud VERBATIM as `claude_native_*` chunks, so the
// frontend's parseClaudeEvents can render native chat bubbles + activity cards.
//
// The difference from the generic session.go path is ONLY the output framing.
// The generic path parses Claude's stream-json into lossy display text and
// emits `stream` chunks (dropping tool_use inputs / tool_result / structured
// result). This bridge instead forwards each raw stream-json line untouched, so
// no structured data is lost. The launch shape is identical to the generic
// Claude path (no `-p`, stdin kept open, CLAUDE_CODE_ENTRYPOINT=cli) — that
// shape is load-bearing for multi-turn SendInput and for billing (interactive
// vs Agent SDK credit pool), so we reuse buildClaudeInteractiveArgs and
// prepareClaudeChildEnv unchanged and only swap the output forwarding.
//
// Protocol on stdin: NDJSON user-message envelopes
//   {"type":"user","message":{"role":"user","content":"…"},"session_id":"…","parent_tool_use_id":null}
// The initial prompt is delivered on Start; follow-up turns via Send. Stdin
// stays open for the life of the session (multi-turn), unlike codex exec.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// Matches the codex app-server scanner ceiling: a single stream-json line
	// can legitimately be large (a full assistant message or tool_result), but
	// the downstream Pub/Sub envelope is 10 MB, so oversized frames are
	// session-fatal (see claudeNativeMaxFrameSize).
	claudeNativeMaxLineSize = 30 * 1024 * 1024

	// Cheap raw-line pre-check: frames larger than this cannot fit Pub/Sub's
	// 10 MB envelope even before JSON-string escaping doubles a quote/backslash
	// heavy line, so we reject them without building a resultMsg.
	claudeNativeMaxFrameSize = 8 * 1024 * 1024

	// GCP Pub/Sub documented per-message publish ceiling. The authoritative
	// gate after marshaling the envelope.
	claudeNativeMaxPublishSize = 10_000_000

	// Upper bound on a single stdin write before we declare the pipe stalled.
	claudeNativeStdinWriteTimeout = 10 * time.Second

	// How long End waits after closing stdin before escalating to SIGINT then
	// SIGKILL.
	claudeNativeGracefulShutdownTimeout = 5 * time.Second

	// How long waitForExit waits for the stream readers to drain before
	// publishing claude_native_ended.
	claudeNativeStreamDrainTimeout = 30 * time.Second

	// Caps how long a session may stay open before CleanupStale ends it. Matches
	// the codex/grok 6 h ceiling so an orchestrator crash that drops
	// claude_native_end can't leak Claude children indefinitely.
	claudeNativeMaxLifetime = 6 * time.Hour

	// How often the stale cleanup goroutine scans.
	claudeNativeCleanupInterval = 60 * time.Second

	// Bounds the per-session publish backlog. Every stream-json frame is
	// stateful (message deltas, tool_use, tool_result, result) so we never drop
	// silently; a wedged queue fails the session fast.
	claudeNativePublishQueueSize = 256

	// Upper bound for the stream readers to wait when the publish queue is full
	// before declaring Pub/Sub stalled and failing the session.
	claudeNativeEnqueueTimeout = 30 * time.Second
)

/* --------------------------------------------------------------------------
   ClaudeNativeSession — one running `claude … stream-json` process
   -------------------------------------------------------------------------- */

// ClaudeNativeSession holds a single child process and its stdio pipes. Public
// state is guarded by mu; stdin writes are serialised by stdinMu so concurrent
// Send callers cannot interleave NDJSON envelopes.
type ClaudeNativeSession struct {
	ID          string
	Process     *exec.Cmd
	Stdin       io.WriteCloser
	Stdout      io.ReadCloser
	Stderr      io.ReadCloser
	StartedAt   time.Time
	WorkspaceID string
	UID         string

	mu         sync.Mutex
	status     string // "running" | "ended"
	exitCode   int
	stdinMu    sync.Mutex
	stdinClose sync.Once
	done       chan struct{}
	streamDone chan struct{}
	seq        int64
	// killUnconfirmed marks a session whose End escalated to Kill and then
	// timed out waiting for the exit watcher. The session is RETAINED as a
	// tombstone (see end_confirm.go): only probeProcessGone may convert it
	// into the "not found" absence answer the server frees a device on.
	killUnconfirmed bool
	// terminalPublishState reserves this session's ID while its claude_native_ended
	// frame is in flight — see end_confirm.go.
	terminalPublishState
}

// Status returns the current lifecycle status under the session mutex.
func (s *ClaudeNativeSession) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// closeStdin is idempotent.
func (s *ClaudeNativeSession) closeStdin() {
	s.stdinClose.Do(func() {
		_ = s.Stdin.Close()
	})
}

func (s *ClaudeNativeSession) isKillUnconfirmed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.killUnconfirmed
}

func (s *ClaudeNativeSession) markKillUnconfirmed() {
	s.mu.Lock()
	s.killUnconfirmed = true
	s.mu.Unlock()
}

// claudeUserEnvelope wraps a user turn's plain text in the NDJSON envelope
// Claude's --input-format stream-json expects, matching session.go's SendInput.
func claudeUserEnvelope(sessionID, text string) string {
	return fmt.Sprintf(
		`{"type":"user","message":{"role":"user","content":%s},"session_id":"%s","parent_tool_use_id":null}`,
		jsonEscapeString(text), sessionID,
	)
}

/* --------------------------------------------------------------------------
   ClaudeNativeManager — tracks every active session
   -------------------------------------------------------------------------- */

// ClaudeNativeManager owns the active Claude stream-json processes.
type ClaudeNativeManager struct {
	sessions map[string]*ClaudeNativeSession
	// Config is needed at session end to scan for and upload the media the
	// session produced (see collectSessionArtifacts). Without it this
	// manager silently drops every artifact its CLI wrote.
	Config *Config
	mu     sync.RWMutex
}

// NewClaudeNativeManager creates a fresh manager.
func NewClaudeNativeManager(cfg *Config) *ClaudeNativeManager {
	return &ClaudeNativeManager{
		sessions: make(map[string]*ClaudeNativeSession),
		Config:   cfg,
	}
}

// Start launches `claude … stream-json` in cwd. extraArgs are threaded through
// buildClaudeInteractiveArgs (which forces the stream-json flag set and strips
// -p/--print). initialPrompt, when non-empty, is delivered as the first NDJSON
// user turn on stdin; stdin is kept open for follow-up Send turns.
//
// publishFn receives:
//   - claude_native_message for every stdout stream-json line (verbatim)
//   - claude_native_stderr  for every stderr line
//   - claude_native_error   on a fatal stream condition (oversize / queue stall)
//   - claude_native_ended   when the process exits, carrying ExitCode
//
// onStarted, when non-nil, is invoked after the child process is spawned and
// the stream readers are wired, but BEFORE the initial prompt is written. This
// lets callers publish their `claude_native_started` ack ahead of any
// `claude_native_message` frames the readers may forward, guaranteeing the
// ordering consumers rely on when they initialize state on the started frame.
func (m *ClaudeNativeManager) Start(id, cwd string, extraArgs []string, initialPrompt, workspaceID, uid string, publishFn PublishFunc, onStarted func()) error {
	if id == "" {
		return fmt.Errorf("sessionID is required")
	}
	if publishFn == nil {
		return fmt.Errorf("publishFn is required")
	}

	// cwd is required and must be an existing absolute directory — never fall
	// back to the agent's process default (would run Claude against a surprise
	// path). Mirrors the codex app-server guard.
	if cwd == "" {
		return fmt.Errorf("cwd is required for claude native (must point at a workspace directory)")
	}
	if !filepath.IsAbs(cwd) {
		return fmt.Errorf("cwd must be an absolute path; got %q", cwd)
	}
	if info, err := os.Stat(cwd); err != nil {
		return fmt.Errorf("cwd %q is not accessible: %w", cwd, err)
	} else if !info.IsDir() {
		return fmt.Errorf("cwd %q is not a directory", cwd)
	}

	// Hold the manager mutex across the spawn so two concurrent Start calls for
	// the same id can't double-spawn (TOCTOU).
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.sessions[id]; exists {
		return fmt.Errorf("claude native session %s already exists", id)
	}

	executable := resolveExecutable("claude")
	args, extractedPrompt := buildClaudeInteractiveArgs(extraArgs)
	prompt := initialPrompt
	if prompt == "" {
		prompt = extractedPrompt
	}

	fmt.Printf("%s[claude-native] Starting session %s: %s %s%s\n",
		colorCyan, id, executable, strings.Join(args, " "), colorReset)

	proc := exec.Command(executable, args...)
	hideWindow(proc)
	proc.Dir = cwd
	// Reuse the generic Claude env prep: strips billing/nested-IDE markers and
	// pins CLAUDE_CODE_ENTRYPOINT=cli so this counts against the interactive
	// subscription allowance, not the Agent SDK credit pool.
	childEnv, _ := prepareClaudeChildEnv("claude", os.Environ())
	proc.Env = childEnv

	stdin, err := proc.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdin pipe: %w", err)
	}
	stdout, err := proc.StdoutPipe()
	if err != nil {
		stdin.Close()
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderr, err := proc.StderrPipe()
	if err != nil {
		stdin.Close()
		stdout.Close()
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := proc.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		stderr.Close()
		return fmt.Errorf("failed to start claude (is `claude` on PATH?): %w", err)
	}

	session := &ClaudeNativeSession{
		ID:          id,
		Process:     proc,
		Stdin:       stdin,
		Stdout:      stdout,
		Stderr:      stderr,
		StartedAt:   time.Now(),
		WorkspaceID: workspaceID,
		UID:         uid,
		status:      "running",
		done:        make(chan struct{}),
		streamDone:  make(chan struct{}),
	}

	m.sessions[id] = session

	if proc.Process != nil {
		globalProcessRegistry.Register(proc.Process.Pid, "claude-native:"+id)
	}

	// Fire the started ack BEFORE spawning the stdout/stderr reader goroutines
	// so `claude_native_started` is published ahead of any
	// `claude_native_message`/`claude_native_stderr` frames — even if Claude
	// emits early startup output (auth error, diagnostic) before we get to
	// writing the initial prompt. The OS pipe buffer holds any early bytes
	// until the readers below drain them, so no frame is lost.
	if onStarted != nil {
		onStarted()
	}

	go m.readStream(session, publishFn)
	go m.waitForExit(session, publishFn)

	// Deliver the first user turn (if any) after the readers are wired so we
	// never miss Claude's response frames. A failure here means the session
	// never actually got the user's first message — publish a typed error
	// frame (waitForExit still emits claude_native_ended once the child
	// exits) so the consumer sees the failure instead of a silent empty chat.
	if prompt != "" {
		if err := m.writeUserTurn(session, prompt); err != nil {
			fmt.Printf("%s[claude-native] Failed to send initial prompt to %s: %v%s\n",
				colorRed, id, err, colorReset)
			seq := atomic.AddInt64(&session.seq, 1)
			publishFn(resultMsg{
				ID:          session.ID,
				WorkspaceID: session.WorkspaceID,
				UID:         session.UID,
				Output:      fmt.Sprintf("failed to deliver initial prompt: %v", err),
				Status:      "error",
				Ts:          time.Now().UnixMilli(),
				Version:     Version,
				Type:        "claude_native_error",
				SessionID:   session.ID,
				Seq:         int(seq),
			})
			if session.Status() != "ended" {
				session.mu.Lock()
				session.status = "ended"
				session.mu.Unlock()
				if session.Process != nil && session.Process.Process != nil {
					_ = session.Process.Process.Kill()
				}
			}
		} else {
			fmt.Printf("%s[claude-native] Sent initial prompt to %s (%d chars)%s\n",
				colorGreen, id, len(prompt), colorReset)
		}
	}

	fmt.Printf("%s[claude-native] Session %s started (PID: %d)%s\n",
		colorGreen, id, proc.Process.Pid, colorReset)
	return nil
}

// Send delivers a follow-up user turn. text is the raw user message; the
// manager wraps it in the NDJSON user envelope (callers never construct the
// protocol frame themselves — unlike codex/grok which forward JSON-RPC).
func (m *ClaudeNativeManager) Send(id string, text string) error {
	session := m.Get(id)
	if session == nil {
		return fmt.Errorf("claude native session %s not found", id)
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("payload is empty")
	}
	return m.writeUserTurn(session, text)
}

// writeUserTurn serialises a single NDJSON user envelope to the child's stdin
// with a fail-fast timeout. On a stalled write it closes stdin, marks the
// session ended, and kills the child so waitForExit publishes the terminal
// frame — this guarantees a later Send cannot interleave with an abandoned
// write goroutine.
func (m *ClaudeNativeManager) writeUserTurn(session *ClaudeNativeSession, text string) error {
	envelope := claudeUserEnvelope(session.ID, text)

	session.stdinMu.Lock()
	defer session.stdinMu.Unlock()

	if session.Status() == "ended" {
		return fmt.Errorf("claude native session %s has ended", session.ID)
	}

	writeDone := make(chan error, 1)
	go func() {
		_, err := fmt.Fprintln(session.Stdin, envelope)
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		if err != nil {
			return fmt.Errorf("failed to write to claude native session %s stdin: %w", session.ID, err)
		}
	case <-time.After(claudeNativeStdinWriteTimeout):
		session.closeStdin()
		session.mu.Lock()
		session.status = "ended"
		session.mu.Unlock()
		if session.Process != nil && session.Process.Process != nil {
			_ = session.Process.Process.Kill()
		}
		return fmt.Errorf("timeout writing to claude native session %s stdin — session terminated to prevent frame interleave", session.ID)
	}

	fmt.Printf("%s[claude-native] → %s (%d bytes)%s\n",
		colorBlue, session.ID, len(envelope), colorReset)
	return nil
}

// End shuts down a session: close stdin (Claude's graceful exit), then escalate
// to SIGINT and finally SIGKILL.
func (m *ClaudeNativeManager) End(id string) error {
	session := m.Get(id)
	if session == nil {
		return fmt.Errorf("claude native session %s not found", id)
	}

	if session.Status() == "ended" {
		// The watcher owns terminal publication and removal; retain the ID until
		// it has established the in-flight publish reservation.
		return nil
	}

	// A prior End already escalated to Kill and timed out on the exit
	// watcher. The session is a retained tombstone: only VERIFIED OS-level
	// process absence may become the "not found" answer the server frees the
	// device on (Codex P1 — see end_confirm.go).
	if session.isKillUnconfirmed() {
		if probeProcessGone(session.Process) {
			if !m.removeSessionIfSame(id, session) {
				// See CodexAppServerManager.End — the ID is not ours to free.
				return fmt.Errorf("claude native session %s could not be released — a terminal frame is still in flight or the ID was re-taken: %w", id, errEndUnconfirmed)
			}
			return fmt.Errorf("claude native session %s not found", id)
		}
		if session.Process.Process != nil {
			if killErr := session.Process.Process.Kill(); killErr != nil {
				fmt.Printf("%s[claude-native] Re-kill failed for %s: %v%s\n",
					colorRed, id, killErr, colorReset)
			}
		}
		if waitDoneConfirm(session.done, killConfirmTimeout) {
			m.removeSessionIfSame(id, session)
			return nil
		}
		return fmt.Errorf("claude native session %s kill unconfirmed after %s; session retained pending process-absence verification: %w", id, killConfirmTimeout, errEndUnconfirmed)
	}

	fmt.Printf("%s[claude-native] Ending session %s gracefully...%s\n",
		colorYellow, id, colorReset)

	session.closeStdin()

	select {
	case <-session.done:
	case <-time.After(claudeNativeGracefulShutdownTimeout):
		fmt.Printf("%s[claude-native] Stdin close didn't exit %s — interrupting%s\n",
			colorYellow, id, colorReset)
		_ = interruptProcess(session.Process)
		select {
		case <-session.done:
		case <-time.After(claudeNativeGracefulShutdownTimeout):
			fmt.Printf("%s[claude-native] Force killing session %s%s\n",
				colorRed, id, colorReset)
			if session.Process.Process != nil {
				if killErr := session.Process.Process.Kill(); killErr != nil {
					fmt.Printf("%s[claude-native] Kill failed for %s: %v%s\n",
						colorRed, id, killErr, colorReset)
				}
			}
			// BOUNDED wait — see end_confirm.go for why blocking here
			// indefinitely wedged an entire device (2026-08-27).
			if !waitDoneConfirm(session.done, killConfirmTimeout) {
				fmt.Printf("%s[claude-native] Kill unconfirmed for %s after %s — retaining tombstone; a later end verifies process absence%s\n",
					colorRed, id, killConfirmTimeout, colorReset)
				session.markKillUnconfirmed()
				// Deliberately NOT "session <id> not found": the process may
				// still be alive, and the server must keep the device fenced
				// until absence is VERIFIED (see the tombstone branch above).
				return fmt.Errorf("claude native session %s kill unconfirmed after %s; session retained pending process-absence verification: %w", id, killConfirmTimeout, errEndUnconfirmed)
			}
		}
	}

	m.removeSessionIfSame(id, session)
	return nil
}

// Get returns the session for id, or nil.
func (m *ClaudeNativeManager) Get(id string) *ClaudeNativeSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

// ActiveCount returns the number of tracked sessions.
func (m *ClaudeNativeManager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// CleanupStale ends sessions older than maxAge. Run as a goroutine.
func (m *ClaudeNativeManager) CleanupStale(maxAge time.Duration) {
	ticker := time.NewTicker(claudeNativeCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.endStaleSessions(maxAge)
		case <-shutdownChan:
			return
		}
	}
}

// endStaleSessions ends any session older than maxAge. Split out for testing.
func (m *ClaudeNativeManager) endStaleSessions(maxAge time.Duration) {
	m.mu.RLock()
	var staleIDs []string
	for id, session := range m.sessions {
		if time.Since(session.StartedAt) > maxAge {
			staleIDs = append(staleIDs, id)
		}
	}
	m.mu.RUnlock()

	for _, id := range staleIDs {
		fmt.Printf("%s[claude-native] Cleaning up stale session %s (exceeded %v)%s\n",
			colorYellow, id, maxAge, colorReset)
		_ = m.End(id)
	}
}

// ShutdownAll ends every active session (agent shutdown).
func (m *ClaudeNativeManager) ShutdownAll() {
	m.mu.RLock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	if len(ids) > 0 {
		fmt.Printf("%s[claude-native] Shutting down %d active session(s)...%s\n",
			colorYellow, len(ids), colorReset)
	}
	for _, id := range ids {
		_ = m.End(id)
	}
}

func (m *ClaudeNativeManager) removeSession(id string) {
	m.mu.Lock()
	if s, ok := m.sessions[id]; ok && s.Process != nil && s.Process.Process != nil {
		globalProcessRegistry.Deregister(s.Process.Process.Pid)
	}
	delete(m.sessions, id)
	m.mu.Unlock()
}

// removeSessionIfSame removes id only while it still maps to THIS session —
// see CodexAppServerManager.removeSessionIfSame for the reused-ID watcher
// race this prevents (Codex P2).//
// It also refuses while a terminal frame is in flight for s: the frame is
// already travelling under this ID, so freeing the ID now would let a
// replacement Start receive it as its own shutdown evidence (Codex P2, round
// 4). The publisher performs the removal itself once delivery completes.
//
// Returns whether id is free of s afterwards — false means either a
// replacement already owns the ID or the release is deferred to the in-flight
// publisher, and in both cases the caller must NOT report this session's
// absence.
func (m *ClaudeNativeManager) removeSessionIfSame(id string, s *ClaudeNativeSession) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.sessions[id]
	if !ok {
		return true
	}
	if cur != s || s.terminalPublishInFlight() {
		return false
	}
	if s.Process != nil && s.Process.Process != nil {
		globalProcessRegistry.Deregister(s.Process.Process.Pid)
	}
	delete(m.sessions, id)
	return true
}

/* --------------------------------------------------------------------------
   Stream + exit handling
   -------------------------------------------------------------------------- */

// readStream forwards every stdout stream-json line as claude_native_message
// (VERBATIM — the frontend parseClaudeEvents does all semantic interpretation)
// and every stderr line as claude_native_stderr. Unlike codex we do NOT reject
// non-JSON stdout lines: Claude's --verbose mode can interleave the occasional
// non-JSON diagnostic, and the frontend parser already skips unparseable lines,
// so forwarding them verbatim is both simpler and more robust than failing the
// session. Frames are still never dropped: oversize frames or a stalled publish
// queue fail the session fast (a dropped frame would corrupt the transcript).
func (m *ClaudeNativeManager) readStream(session *ClaudeNativeSession, publishFn PublishFunc) {
	defer close(session.streamDone)

	queue := make(chan resultMsg, claudeNativePublishQueueSize)
	publisherDone := startTrackedTerminalPublisher(queue, publishFn)
	defer func() {
		close(queue)
		<-publisherDone
	}()

	enqueue := func(msg resultMsg) bool {
		select {
		case queue <- msg:
			return true
		case <-time.After(claudeNativeEnqueueTimeout):
			return false
		}
	}

	failSessionFatally := func(reason string, droppedType string) {
		fmt.Printf("%s[claude-native] Publish queue stalled for %s — failing session (dropped %s)%s\n",
			colorRed, session.ID, droppedType, colorReset)
		seq := atomic.AddInt64(&session.seq, 1)
		publishFn(resultMsg{
			ID:          session.ID,
			WorkspaceID: session.WorkspaceID,
			UID:         session.UID,
			Output:      reason,
			Status:      "error",
			Ts:          time.Now().UnixMilli(),
			Version:     Version,
			Type:        "claude_native_error",
			SessionID:   session.ID,
			Seq:         int(seq),
		})
		if session.Process != nil && session.Process.Process != nil {
			_ = session.Process.Process.Kill()
		}
	}

	publishOrFail := func(msg resultMsg, droppedType string) bool {
		encoded, err := json.Marshal(msg)
		if err != nil {
			failSessionFatally(
				fmt.Sprintf("claude native envelope failed to marshal: %v — session terminated", err),
				droppedType,
			)
			return false
		}
		if len(encoded) > claudeNativeMaxPublishSize {
			failSessionFatally(
				fmt.Sprintf(
					"claude native envelope marshaled to %d bytes after JSON escaping, exceeding the %d-byte publishable limit — session terminated to avoid silent Pub/Sub rejection",
					len(encoded), claudeNativeMaxPublishSize,
				),
				droppedType,
			)
			return false
		}
		if !enqueue(msg) {
			failSessionFatally("claude native publish queue stalled — terminating session to avoid dropping frames", droppedType)
			return false
		}
		return true
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(session.Stdout)
		scanner.Buffer(make([]byte, 0, 256*1024), claudeNativeMaxLineSize)
		fmt.Printf("%s[claude-native] stdout scanner started for %s%s\n",
			colorCyan, session.ID, colorReset)
		lineCount := 0
		for scanner.Scan() {
			lineCount++
			trimmed := strings.TrimSpace(scanner.Text())
			if trimmed == "" {
				continue
			}
			if len(trimmed) > claudeNativeMaxFrameSize {
				failSessionFatally(
					fmt.Sprintf(
						"claude emitted a %d-byte frame exceeding the %d-byte publishable limit — session terminated to avoid silent drop",
						len(trimmed), claudeNativeMaxFrameSize,
					),
					"claude_native_oversize_frame",
				)
				return
			}
			seq := atomic.AddInt64(&session.seq, 1)
			if lineCount <= 3 {
				fmt.Printf("%s[claude-native] stdout[%d] %s: %s%s\n",
					colorCyan, lineCount, session.ID, truncateString(trimmed, 200), colorReset)
			}
			// Passive utilization capture (side-effect only; does not alter
			// framing) so cliagent_usage_claudecode.go can read rate limits.
			// When the parse yields a hard-quota rejection, mirror session.go
			// and publish formatClaudeLimitLine as a dedicated frame — the
			// legacy path emits this so agent-orchestrator-service's
			// detectRateLimit can defer the execution and auto-resume at the
			// reset instant. Without this frame native sessions that hit
			// Claude limits finish/fail instead of being rescheduled.
			if rejected := captureClaudeRateLimitLine(trimmed, time.Now()); rejected != nil {
				limitSeq := atomic.AddInt64(&session.seq, 1)
				if !publishOrFail(resultMsg{
					ID:          session.ID,
					WorkspaceID: session.WorkspaceID,
					UID:         session.UID,
					Output:      formatClaudeLimitLine(*rejected),
					Status:      "success",
					Ts:          time.Now().UnixMilli(),
					Version:     Version,
					Type:        "claude_native_ratelimit",
					SessionID:   session.ID,
					Seq:         int(limitSeq),
				}, "claude_native_ratelimit") {
					return
				}
				fmt.Printf("%s[claude-native] rate limit rejected for %s — resets %s%s\n",
					colorYellow, session.ID,
					time.UnixMilli(rejected.ResetsAtMs).UTC().Format(time.RFC3339), colorReset)
			}
			if !publishOrFail(resultMsg{
				ID:          session.ID,
				WorkspaceID: session.WorkspaceID,
				UID:         session.UID,
				Output:      trimmed,
				Status:      "success",
				Ts:          time.Now().UnixMilli(),
				Version:     Version,
				Type:        "claude_native_message",
				SessionID:   session.ID,
				Seq:         int(seq),
			}, "claude_native_message") {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			fmt.Printf("%s[claude-native] stdout scanner error for %s: %v%s\n",
				colorRed, session.ID, err, colorReset)
			failSessionFatally(
				fmt.Sprintf("claude stdout scanner error: %v — session terminated", err),
				"claude_native_scanner_error",
			)
		}
		fmt.Printf("%s[claude-native] stdout scanner done for %s (%d lines)%s\n",
			colorYellow, session.ID, lineCount, colorReset)
	}()

	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(session.Stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.TrimSpace(line) == "" {
				continue
			}
			seq := atomic.AddInt64(&session.seq, 1)
			fmt.Printf("%s[claude-native] stderr %s: %s%s\n",
				colorYellow, session.ID, truncateString(line, 200), colorReset)
			if !publishOrFail(resultMsg{
				ID:          session.ID,
				WorkspaceID: session.WorkspaceID,
				UID:         session.UID,
				Output:      line,
				Status:      "success",
				Ts:          time.Now().UnixMilli(),
				Version:     Version,
				Type:        "claude_native_stderr",
				SessionID:   session.ID,
				Seq:         int(seq),
			}, "claude_native_stderr") {
				return
			}
		}
		// stderr scanner errors are diagnostic, not protocol-fatal — log only.
		if err := scanner.Err(); err != nil {
			fmt.Printf("%s[claude-native] stderr scanner error for %s: %v%s\n",
				colorRed, session.ID, err, colorReset)
		}
	}()

	wg.Wait()
}

func (m *ClaudeNativeManager) waitForExit(session *ClaudeNativeSession, publishFn PublishFunc) {
	// Reap via os.Process.Wait, NOT exec.Cmd.Wait. StdoutPipe docs: "it is
	// incorrect to call Wait before all reads from the pipe have completed" —
	// Cmd.Wait closes the parent ends of StdoutPipe/StderrPipe the moment the
	// child exits, which can truncate the final stream-json frame still
	// buffered in the scanner. When claude writes a terminal `result` frame
	// and exits immediately after (auth failure, end-of-turn flush), losing
	// that frame corrupts the transcript. Split exit detection (Process.Wait)
	// from pipe cleanup (Close below, gated on streamDone) — same pattern as
	// the Grok ACP manager.
	state, _ := session.Process.Process.Wait()

	// Drain the scanner goroutines BEFORE closing pipes so they see EOF
	// naturally on the OS pipe buffer, including the final frame. The child's
	// write ends are already closed (Process.Wait only returns post-exit); a
	// wedged scanner falls through to the drain timeout + force-close.
	select {
	case <-session.streamDone:
	case <-time.After(claudeNativeStreamDrainTimeout):
		fmt.Printf("%s[claude-native] Stream drain timed out for %s — forcing pipe close%s\n",
			colorYellow, session.ID, colorReset)
		session.Stdout.Close()
		session.Stderr.Close()
	}

	// Close parent pipe ends. Cmd.Wait would do this for us; since we
	// bypassed it above we mop up ourselves. Close-after-Close is documented
	// as returning ErrClosed without side effects; closeStdin is sync.Once.
	session.closeStdin()
	session.Stdout.Close()
	session.Stderr.Close()

	session.mu.Lock()
	session.status = "ended"
	if state != nil {
		session.exitCode = state.ExitCode()
	} else {
		session.exitCode = -1
	}
	exit := session.exitCode
	session.mu.Unlock()

	// Scan for and upload whatever media this session wrote before announcing
	// the end, so the metadata rides along on the ended frame exactly as it
	// does on the PTY path's session_ended. Skipping this is what made every
	// capture run through a bundled CLI report NO_MEDIA_UPLOADED while the
	// recording sat on the device (prod 2026-08-20, video project vp_a72774c5).
	// BOUNDED: a hung scan/upload here used to suppress the ended frame and
	// hold session.done forever — one of the two wedge shapes behind the
	// 2026-08-27 device outage (see sessionArtifactCollectTimeout).
	uploadedFiles, uploadErrors, _ := collectSessionArtifactsBounded(
		m.Config,
		session.ID,
		session.WorkspaceID,
		session.Process.Dir,
		session.StartedAt,
		sessionArtifactCollectTimeout,
	)

	seq := atomic.AddInt64(&session.seq, 1)

	// A wedged watcher can reach this point long after a verified-absence
	// End dropped the tombstone and a replacement Start re-took the ID. The
	// terminal frame is shutdown evidence: delivered while the ID belongs to
	// a replacement, it releases the server's fence for a session that is
	// still running (Codex P2, round 3 — the identity guard covered only map
	// removal). Publish it only while the ID is still THIS session's, atomically
	// re-reserving an already-unclaimed ID against Start's registration.
	if !publishTerminalIfCurrent(&m.mu, m.sessions, session.ID, session, &session.terminalPublishState, publishFn, resultMsg{
		ID:           session.ID,
		WorkspaceID:  session.WorkspaceID,
		UID:          session.UID,
		Output:       fmt.Sprintf("claude native ended (exit code: %d)", exit),
		Status:       "success",
		Ts:           time.Now().UnixMilli(),
		Version:      Version,
		Type:         "claude_native_ended",
		SessionID:    session.ID,
		ExitCode:     exit,
		Seq:          int(seq),
		Files:        uploadedFiles,
		UploadErrors: uploadErrors,
	}, func() { m.removeSessionIfSame(session.ID, session) }) {
		fmt.Printf("%s[claude-native] Suppressed stale claude_native_ended for %s — the ID now belongs to a replacement session%s\n",
			colorYellow, session.ID, colorReset)
	}

	close(session.done)

	fmt.Printf("%s[claude-native] Session %s ended (exit code: %d)%s\n",
		colorYellow, session.ID, exit, colorReset)

	// No-op while the ended frame is still in flight — the publisher's release
	// callback owns the removal in that case (end_confirm.go).
	m.removeSessionIfSame(session.ID, session)
}
