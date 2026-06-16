// File: grok_acp.go
// -----------------------------------------------------------------------------
// GrokACPManager — long-lived `grok agent stdio` sessions used by AI Expedite
// to drive xAI Grok via its ACP (Agent Client Protocol) JSON-RPC 2.0 interface
// over the child process' stdio (newline-delimited JSON).
//
// Mirrors CodexAppServerManager (codex_appserver.go) — the transport, framing,
// fail-fast policy, lifecycle and cleanup story are identical between the two
// integrations because the same orchestrator-side state machine drives both:
//
//   - We only enforce wire framing (a single JSON object per line, no embedded
//     newlines, never silently drop a frame). All JSON-RPC semantics — method
//     names, request/response correlation, `initialize`, `authenticate`,
//     `session/new`, `session/prompt`, `session/update` streaming, approval
//     responses — live in the orchestrator. Keeping this driver protocol-
//     agnostic means Grok's ACP can evolve without dragging the agent along.
//
//   - Frames are forwarded VERBATIM as `grok_acp_message` results. Non-JSON
//     lines become `grok_acp_error` so the orchestrator's state machine sees
//     a clear failure instead of misinterpreting a malformed line as a real
//     JSON-RPC response.
//
//   - Stderr is surfaced as `grok_acp_stderr` for diagnostics but is not
//     protocol-critical.
//
// Auth posture is enforced by the orchestrator, not here: per the feature
// brief, the orchestrator's `authenticate` flow MUST prefer Grok's
// `cached_token` (the user's local `grok login`) so usage ties to the
// terminal computer user's account/subscription. An `xai.api_key` /
// `XAI_API_KEY` fallback is opt-in only. This file is responsible for
// preserving the local Grok auth state (XAI_API_KEY and the `GROK_*` config
// dir vars survive the env sanitiser) and stripping vars that would confuse
// Grok inside a nested agent (CLAUDECODE / CLAUDE_ / CODEX_IDE_*).
//
// Lifecycle (identical shape to codex_appserver.go for operational
// consistency): Start spawns the child, registers it in the global process
// registry, and launches stdout + stderr reader goroutines. End closes stdin
// first (ACP's documented graceful-shutdown path) and falls through to
// interrupt + kill on timeout. waitForExit publishes a `grok_acp_ended` frame
// and removes the session from the manager.
// -----------------------------------------------------------------------------

package main

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
	// grokACPMaxLineSize caps the bufio.Scanner buffer for a single JSONL
	// frame. Matches the Codex manager's ceiling — session.go's CLI stream
	// scanner uses the same 30 MB cap. The downstream Pub/Sub message limit
	// is 10 MB, so frames bigger than grokACPMaxFrameSize cannot survive a
	// publish and are session-fatal.
	grokACPMaxLineSize = 30 * 1024 * 1024

	// grokACPMaxFrameSize is a cheap pre-check on the raw stdout line. Frames
	// bigger than this cannot possibly fit Pub/Sub's 10 MB envelope before
	// JSON escaping, so we reject them without building a resultMsg. The
	// authoritative gate is grokACPMaxPublishSize below — the Output field is
	// JSON-string-escaped on marshal, so a frame heavy in '"' / '\' can
	// roughly double in size, meaning a line that passes this pre-check can
	// still fail the marshaled-envelope check. Frames that fail either check
	// are session-fatal — silently dropping one would deadlock the
	// orchestrator's JSON-RPC state machine waiting for a response that never
	// arrives.
	grokACPMaxFrameSize = 8 * 1024 * 1024

	// grokACPMaxPublishSize is GCP Pub/Sub's documented per-message publish
	// limit. After building each resultMsg, the stream reader marshals it and
	// rejects envelopes that exceed this ceiling — that catches the case
	// where Grok emits a frame whose raw bytes are under grokACPMaxFrameSize
	// but whose JSON-string-escaped Output field marshals beyond what Pub/Sub
	// will accept.
	grokACPMaxPublishSize = 10_000_000

	// grokACPStdinWriteTimeout is the upper bound on a single stdin write
	// before we declare the pipe stalled. Matches session.go's SendInput
	// budget.
	grokACPStdinWriteTimeout = 10 * time.Second

	// grokACPGracefulShutdownTimeout is how long End waits after closing
	// stdin (ACP's documented exit path) before escalating to SIGINT and
	// ultimately SIGKILL.
	grokACPGracefulShutdownTimeout = 5 * time.Second

	// grokACPStreamDrainTimeout is how long waitForExit waits for the
	// stdout/stderr readers to finish draining buffered frames before
	// publishing `grok_acp_ended`. Generous to avoid racing the last JSON-RPC
	// response with the exit notification.
	grokACPStreamDrainTimeout = 30 * time.Second

	// grokACPMaxLifetime caps how long a session may stay open before
	// CleanupStale ends it. Matches SessionManager's 6 h ceiling so an
	// orchestrator crash can't leak grok children indefinitely (each child
	// holds a Grok auth session and may keep billing).
	grokACPMaxLifetime = 6 * time.Hour

	// grokACPCleanupInterval is how often the stale cleanup goroutine scans
	// for expired sessions.
	grokACPCleanupInterval = 60 * time.Second

	// grokACPPublishQueueSize bounds the per-session publish backlog. Every
	// ACP frame is a stateful JSON-RPC message (request id, session/update
	// notification, approval request) — losing one corrupts the session, so
	// frames are enqueued in order and drained by a single publisher
	// goroutine. If the queue fills we surface a fatal error and kill the
	// child rather than silently dropping.
	grokACPPublishQueueSize = 256

	// grokACPEnqueueTimeout is the upper bound for the stream readers to
	// wait when the publish queue is full. Hitting this means Pub/Sub has
	// stalled for a sustained period; the session cannot continue safely so
	// the manager publishes a `grok_acp_error` surface and force-kills the
	// child to fail-fast.
	grokACPEnqueueTimeout = 30 * time.Second
)

/* --------------------------------------------------------------------------
   GrokACPSession — one running `grok agent stdio` process
   -------------------------------------------------------------------------- */

// GrokACPSession holds a single child process and its stdio pipes. All public
// state is guarded by mu; stdin writes are serialised separately by stdinMu
// so concurrent Send callers cannot interleave JSONL frames.
type GrokACPSession struct {
	ID          string
	Process     *exec.Cmd
	Stdin       io.WriteCloser
	Stdout      io.ReadCloser
	Stderr      io.ReadCloser
	StartedAt   time.Time
	WorkspaceID string
	UID         string
	TimeoutMs   int64 // 0 = no per-session timeout (rely on grokACPMaxLifetime stale GC)

	mu           sync.Mutex
	status       string // "running" | "ended"
	exitCode     int
	stdinMu      sync.Mutex
	stdinClose   sync.Once
	done         chan struct{}
	streamDone   chan struct{}
	seq          int64
	timeoutTimer *time.Timer // armed only when TimeoutMs > 0
}

// GrokStartOptions bundles the per-session policy knobs the dispatcher reads
// from Config + commandMsg before spawning a Grok ACP child. Bundled rather
// than threading 4+ positional args so future policy additions (e.g. tool
// auto-approval mode) don't churn every call site.
type GrokStartOptions struct {
	// TimeoutMs is the backend-requested per-session deadline. 0 means
	// "no deadline" and the session lives until the 6h stale GC, End(),
	// orchestrator-driven cancellation, or the child's natural exit. Values
	// above grokACPMaxLifetime are clamped to grokACPMaxLifetime — a
	// runaway-orchestrator can't request a longer session than our GC
	// would tolerate anyway.
	TimeoutMs int64

	// AllowAPIKeyFallback, when false, strips XAI_API_KEY from the child
	// env and any `--api-key*` / `--auth*` extra args. Default false enforces
	// the feature-brief invariant that API-key auth is OPT-IN only —
	// otherwise a user with `export XAI_API_KEY=...` in their shell rc would
	// silently bill their xAI API wallet for every Grok session this
	// integration launches. Sourced from Config.EnableGrokAPIKeyFallback.
	AllowAPIKeyFallback bool

	// WorkspaceRoot, when non-empty, is treated as a containment root: the
	// requested cwd must resolve (after EvalSymlinks) to a path strictly
	// inside this root. When empty, no containment check runs — but Start
	// still requires cwd to be absolute and exist. Sourced from
	// Config.WorkingDirectory at the dispatcher.
	WorkspaceRoot string
}

// Status returns the current lifecycle status under the session mutex so
// callers don't race with waitForExit's transition to "ended".
func (s *GrokACPSession) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// closeStdin is idempotent: the End fallback path calls it after stdin may
// already have been closed by waitForExit, and the second close would
// otherwise return an `io: already closed` error.
func (s *GrokACPSession) closeStdin() {
	s.stdinClose.Do(func() {
		_ = s.Stdin.Close()
	})
}

/* --------------------------------------------------------------------------
   GrokACPManager — tracks every active session
   -------------------------------------------------------------------------- */

// GrokACPManager owns the active `grok agent stdio` processes. One manager
// handles many concurrent sessions, mirroring SessionManager's shape.
type GrokACPManager struct {
	sessions map[string]*GrokACPSession
	mu       sync.RWMutex
}

// NewGrokACPManager creates a fresh manager.
func NewGrokACPManager() *GrokACPManager {
	return &GrokACPManager{
		sessions: make(map[string]*GrokACPSession),
	}
}

// Start launches `grok agent stdio` in cwd. extraArgs are passed through
// after the built-in transport argv so the orchestrator can supply
// Grok-specific config knobs (e.g. `--model grok-2-fast`) without us
// special-casing every Grok flag. opts carries the per-session policy knobs
// (timeout, API-key gating, workspace containment) the dispatcher sourced
// from Config + commandMsg.
//
// publishFn receives:
//   - `grok_acp_message` for every JSONL frame Grok emits on stdout
//   - `grok_acp_stderr`  for every line Grok emits on stderr
//   - `grok_acp_error`   when Grok emits a non-JSON line (protocol bug) OR
//     when opts.TimeoutMs fires before the child exits naturally
//   - `grok_acp_ended`   when the process exits, carrying ExitCode
func (m *GrokACPManager) Start(id, cwd string, extraArgs []string, workspaceID, uid string, opts GrokStartOptions, publishFn PublishFunc) error {
	if id == "" {
		return fmt.Errorf("sessionID is required")
	}
	if publishFn == nil {
		return fmt.Errorf("publishFn is required")
	}

	// cwd is required and must point at an existing directory. Falling back
	// to the agent's working directory would silently run grok against a
	// surprise path (e.g. C:\Program Files\AI Expedite on Windows), exposing
	// or editing files unrelated to the requested workspace. The local
	// terminal's workspace/path safety rules treat this as session-fatal at
	// startup rather than tolerating an empty cwd as the legacy CLI path
	// does.
	if cwd == "" {
		return fmt.Errorf("cwd is required for grok agent stdio (must point at a workspace directory)")
	}
	if !filepath.IsAbs(cwd) {
		return fmt.Errorf("cwd must be an absolute path; got %q", cwd)
	}
	if info, err := os.Stat(cwd); err != nil {
		return fmt.Errorf("cwd %q is not accessible: %w", cwd, err)
	} else if !info.IsDir() {
		return fmt.Errorf("cwd %q is not a directory", cwd)
	}

	// Workspace-root containment (finding #1 from secondary review):
	// resolve symlinks on both sides and reject anything that escapes the
	// configured root. Without this, a signed grok_acp_start could launch
	// Grok against any directory the OS user can read/write, sidestepping
	// the workspace/path safety stance the rest of the agent enforces. When
	// the dispatcher does not configure a root (opts.WorkspaceRoot == "")
	// we skip the check — preserving the existing absolute/exists contract
	// for callers that have their own out-of-band containment.
	resolvedCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return fmt.Errorf("cwd %q symlink resolution failed: %w", cwd, err)
	}
	if opts.WorkspaceRoot != "" {
		resolvedRoot, err := filepath.EvalSymlinks(opts.WorkspaceRoot)
		if err != nil {
			return fmt.Errorf("workspace root %q symlink resolution failed: %w", opts.WorkspaceRoot, err)
		}
		if !pathInsideRoot(resolvedCwd, resolvedRoot) {
			return fmt.Errorf("cwd %q is outside the configured workspace root %q", resolvedCwd, resolvedRoot)
		}
	}

	// Hold the manager mutex across the entire spawn so two concurrent Start
	// calls for the same id can't both pass the existence check and double-
	// spawn (the previous check-release-insert pattern had that TOCTOU race).
	// Mirrors CodexAppServerManager.Start and SessionManager.StartSession.
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.sessions[id]; exists {
		return fmt.Errorf("grok acp session %s already exists", id)
	}

	executable := resolveExecutable("grok")
	args := buildGrokACPArgs(extraArgs, opts.AllowAPIKeyFallback)

	fmt.Printf("%s[grok-acp] Starting session %s: %s %s%s\n",
		colorCyan, id, executable, strings.Join(args, " "), colorReset)

	proc := exec.Command(executable, args...)
	hideWindow(proc)
	if cwd != "" {
		proc.Dir = cwd
	}
	proc.Env = sanitizeGrokACPEnv(os.Environ(), opts.AllowAPIKeyFallback)

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
		return fmt.Errorf("failed to start grok agent stdio (is `grok` on PATH? run `grok login` to authenticate): %w", err)
	}

	// Clamp the requested timeout at our stale-GC ceiling. A misbehaving
	// orchestrator that requests TimeoutMs > grokACPMaxLifetime would
	// otherwise outlive the GC anyway; clamping makes the effective deadline
	// predictable and keeps the timer firing meaningful.
	timeoutMs := opts.TimeoutMs
	if timeoutMs < 0 {
		timeoutMs = 0
	}
	if max := int64(grokACPMaxLifetime / time.Millisecond); timeoutMs > max {
		timeoutMs = max
	}

	session := &GrokACPSession{
		ID:          id,
		Process:     proc,
		Stdin:       stdin,
		Stdout:      stdout,
		Stderr:      stderr,
		StartedAt:   time.Now(),
		WorkspaceID: workspaceID,
		UID:         uid,
		TimeoutMs:   timeoutMs,
		status:      "running",
		done:        make(chan struct{}),
		streamDone:  make(chan struct{}),
	}

	m.sessions[id] = session

	if proc.Process != nil {
		globalProcessRegistry.Register(proc.Process.Pid, "grok-acp:"+id)
	}

	// Per-session deadline (finding #2 from secondary review). On fire we
	// publish a typed grok_acp_error first so the orchestrator can fail the
	// in-flight ACP call with a clear reason BEFORE waitForExit's eventual
	// grok_acp_ended arrives — both frames carry monotonic Seq so the
	// orchestrator can reconstruct ordering even though the kill→wait→ended
	// cascade is asynchronous. Timer is Stop()'d in waitForExit on natural
	// exit so a freshly-exited session can't double-fire the timeout publish.
	if session.TimeoutMs > 0 {
		session.timeoutTimer = time.AfterFunc(time.Duration(session.TimeoutMs)*time.Millisecond, func() {
			if session.Status() == "ended" {
				return
			}
			fmt.Printf("%s[grok-acp] Session %s timed out after %dms — killing%s\n",
				colorYellow, session.ID, session.TimeoutMs, colorReset)
			seq := atomic.AddInt64(&session.seq, 1)
			publishFn(resultMsg{
				ID:          session.ID,
				WorkspaceID: session.WorkspaceID,
				UID:         session.UID,
				Output:      fmt.Sprintf("grok acp session timed out after %dms — terminated by per-session deadline", session.TimeoutMs),
				Status:      "error",
				Ts:          time.Now().UnixMilli(),
				Version:     Version,
				Type:        "grok_acp_error",
				SessionID:   session.ID,
				Seq:         int(seq),
			})
			if session.Process != nil && session.Process.Process != nil {
				_ = session.Process.Process.Kill()
			}
		})
	}

	go m.readStream(session, publishFn)
	go m.waitForExit(session, publishFn)

	fmt.Printf("%s[grok-acp] Session %s started (PID: %d)%s\n",
		colorGreen, id, proc.Process.Pid, colorReset)
	return nil
}

// Send writes a single JSON-RPC 2.0 frame to the child's stdin. payload must
// be a self-contained JSON object — typically an ACP request, response or
// notification. Send validates that the payload is parseable JSON and
// contains no embedded newlines (which would corrupt the JSONL framing on
// the wire) but never edits its content, so callers retain exact control
// over JSON-RPC ids and method names.
//
// Timeout policy is fail-fast: on stdin write timeout the manager kills the
// child and closes stdin BEFORE returning. This guarantees no later Send can
// race with the abandoned write goroutine — the next Send sees Status=ended
// and rejects, and the orphaned write (if it ever wakes up) lands on a
// closed pipe.
func (m *GrokACPManager) Send(id string, payload string) error {
	session := m.Get(id)
	if session == nil {
		return fmt.Errorf("grok acp session %s not found", id)
	}

	trimmed := strings.TrimSpace(payload)
	if trimmed == "" {
		return fmt.Errorf("payload is empty")
	}
	if strings.ContainsAny(trimmed, "\r\n") {
		return fmt.Errorf("payload must be a single line of JSON (no embedded newlines); got %d bytes", len(trimmed))
	}
	var probe json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
		return fmt.Errorf("payload is not valid JSON: %w", err)
	}

	session.stdinMu.Lock()
	defer session.stdinMu.Unlock()

	// Re-check Status under stdinMu so we cannot pass the gate after another
	// goroutine has already started tearing the session down via End() or
	// the timeout-fail path below.
	if session.Status() == "ended" {
		return fmt.Errorf("grok acp session %s has ended", id)
	}

	writeDone := make(chan error, 1)
	go func() {
		_, err := fmt.Fprintln(session.Stdin, trimmed)
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		if err != nil {
			return fmt.Errorf("failed to write to grok acp session %s stdin: %w", id, err)
		}
	case <-time.After(grokACPStdinWriteTimeout):
		// Fatal: a stalled write is a signal that grok isn't draining stdin.
		// Continuing would let the next Send acquire stdinMu and interleave
		// its frame with the abandoned write's eventual completion. Close
		// stdin to unblock the abandoned goroutine immediately, transition
		// to "ended" so concurrent/subsequent Sends short-circuit, and kill
		// the child so waitForExit publishes grok_acp_ended.
		session.closeStdin()
		session.mu.Lock()
		session.status = "ended"
		session.mu.Unlock()
		if session.Process != nil && session.Process.Process != nil {
			_ = session.Process.Process.Kill()
		}
		return fmt.Errorf("timeout writing to grok acp session %s stdin — session terminated to prevent frame interleave", id)
	}

	fmt.Printf("%s[grok-acp] → %s (%d bytes)%s\n",
		colorBlue, id, len(trimmed), colorReset)
	return nil
}

// End shuts down a session. First it closes stdin (ACP's documented exit
// path). If the process is still alive after grokACPGracefulShutdownTimeout
// we interrupt it, and if that also fails we SIGKILL.
func (m *GrokACPManager) End(id string) error {
	session := m.Get(id)
	if session == nil {
		return fmt.Errorf("grok acp session %s not found", id)
	}

	if session.Status() == "ended" {
		m.removeSession(id)
		return nil
	}

	fmt.Printf("%s[grok-acp] Ending session %s gracefully...%s\n",
		colorYellow, id, colorReset)

	session.closeStdin()

	select {
	case <-session.done:
	case <-time.After(grokACPGracefulShutdownTimeout):
		fmt.Printf("%s[grok-acp] Stdin close didn't exit %s — interrupting%s\n",
			colorYellow, id, colorReset)
		_ = interruptProcess(session.Process)
		select {
		case <-session.done:
		case <-time.After(grokACPGracefulShutdownTimeout):
			fmt.Printf("%s[grok-acp] Force killing session %s%s\n",
				colorRed, id, colorReset)
			if session.Process.Process != nil {
				_ = session.Process.Process.Kill()
			}
			<-session.done
		}
	}

	m.removeSession(id)
	return nil
}

// Get returns the session for id, or nil if it does not exist.
func (m *GrokACPManager) Get(id string) *GrokACPSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

// ActiveCount returns the number of currently tracked sessions.
func (m *GrokACPManager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// CleanupStale runs periodically to end sessions that exceed maxAge. Call as
// a goroutine: `go m.CleanupStale(grokACPMaxLifetime)`. Without this, an
// orchestrator crash that drops the `grok_acp_end` signal would leak grok
// children indefinitely.
func (m *GrokACPManager) CleanupStale(maxAge time.Duration) {
	ticker := time.NewTicker(grokACPCleanupInterval)
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

// endStaleSessions ends any session whose StartedAt is older than maxAge.
// Split out from CleanupStale so it can be unit-tested without driving the
// ticker.
func (m *GrokACPManager) endStaleSessions(maxAge time.Duration) {
	m.mu.RLock()
	var staleIDs []string
	for id, session := range m.sessions {
		if time.Since(session.StartedAt) > maxAge {
			staleIDs = append(staleIDs, id)
		}
	}
	m.mu.RUnlock()

	for _, id := range staleIDs {
		fmt.Printf("%s[grok-acp] Cleaning up stale session %s (exceeded %v)%s\n",
			colorYellow, id, maxAge, colorReset)
		_ = m.End(id)
	}
}

// ShutdownAll ends every active session. Called during agent shutdown so
// grok children don't outlive the agent and silently consume tokens.
func (m *GrokACPManager) ShutdownAll() {
	m.mu.RLock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	if len(ids) > 0 {
		fmt.Printf("%s[grok-acp] Shutting down %d active session(s)...%s\n",
			colorYellow, len(ids), colorReset)
	}
	for _, id := range ids {
		_ = m.End(id)
	}
}

func (m *GrokACPManager) removeSession(id string) {
	m.mu.Lock()
	if s, ok := m.sessions[id]; ok && s.Process != nil && s.Process.Process != nil {
		globalProcessRegistry.Deregister(s.Process.Process.Pid)
	}
	delete(m.sessions, id)
	m.mu.Unlock()
}

/* --------------------------------------------------------------------------
   Stream + exit handling
   -------------------------------------------------------------------------- */

// readStream forwards every grok stdout frame as `grok_acp_message` and
// every stderr line as `grok_acp_stderr`. stdout frames are validated as
// JSON (so a malformed frame is surfaced as `grok_acp_error` instead of
// being silently passed through), but the original line text is forwarded
// verbatim — we never edit grok's wire format.
//
// Publishing uses a single ordered consumer goroutine that drains a bounded
// queue. Every frame in the Grok ACP stdio protocol is a stateful JSON-RPC
// message (a response correlated by id, a session/update notification, an
// approval request); silently dropping one would corrupt the orchestrator's
// session state.
func (m *GrokACPManager) readStream(session *GrokACPSession, publishFn PublishFunc) {
	defer close(session.streamDone)

	queue := make(chan resultMsg, grokACPPublishQueueSize)
	publisherDone := make(chan struct{})
	go func() {
		defer close(publisherDone)
		for msg := range queue {
			publishFn(msg)
		}
	}()
	defer func() {
		close(queue)
		<-publisherDone
	}()

	enqueue := func(msg resultMsg) bool {
		select {
		case queue <- msg:
			return true
		case <-time.After(grokACPEnqueueTimeout):
			return false
		}
	}

	// failSessionFatally surfaces a queue-stall diagnostic via a synchronous
	// publish (bypassing the wedged queue) and kills the child so waitForExit
	// publishes grok_acp_ended. Same caveat as the Codex manager: if Pub/Sub
	// is genuinely down (not merely slow) this synchronous publish also blocks
	// for the full publishFn timeout — the diagnostic only reliably lands when
	// Pub/Sub is responsive-but-slow; a true outage degrades to "child killed
	// late, no diagnostic" which is still safe (no dropped JSON-RPC frame is
	// mistaken for a live session), just not observable.
	failSessionFatally := func(reason string, droppedType string) {
		fmt.Printf("%s[grok-acp] Publish queue stalled for %s — failing session (dropped %s)%s\n",
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
			Type:        "grok_acp_error",
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
				fmt.Sprintf("grok acp envelope failed to marshal: %v — session terminated", err),
				droppedType,
			)
			return false
		}
		if len(encoded) > grokACPMaxPublishSize {
			failSessionFatally(
				fmt.Sprintf(
					"grok acp envelope marshaled to %d bytes after JSON escaping, exceeding the %d-byte publishable limit — session terminated to avoid silent Pub/Sub rejection",
					len(encoded), grokACPMaxPublishSize,
				),
				droppedType,
			)
			return false
		}
		if !enqueue(msg) {
			failSessionFatally("grok acp publish queue stalled — terminating session to avoid dropping JSON-RPC frames", droppedType)
			return false
		}
		return true
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(session.Stdout)
		scanner.Buffer(make([]byte, 0, 256*1024), grokACPMaxLineSize)
		fmt.Printf("%s[grok-acp] stdout scanner started for %s%s\n",
			colorCyan, session.ID, colorReset)
		lineCount := 0
		for scanner.Scan() {
			lineCount++
			line := scanner.Text()
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}

			if len(trimmed) > grokACPMaxFrameSize {
				failSessionFatally(
					fmt.Sprintf(
						"grok emitted a %d-byte frame exceeding the %d-byte publishable limit — session terminated to avoid silent drop",
						len(trimmed), grokACPMaxFrameSize,
					),
					"grok_acp_oversize_frame",
				)
				return
			}

			seq := atomic.AddInt64(&session.seq, 1)

			var probe json.RawMessage
			if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
				fmt.Printf("%s[grok-acp] Non-JSON stdout frame on %s: %v (line=%s)%s\n",
					colorRed, session.ID, err, truncateString(trimmed, 200), colorReset)
				if !publishOrFail(resultMsg{
					ID:          session.ID,
					WorkspaceID: session.WorkspaceID,
					UID:         session.UID,
					Output:      fmt.Sprintf("non-JSON frame on grok acp stdout: %v", err),
					Status:      "error",
					Ts:          time.Now().UnixMilli(),
					Version:     Version,
					Type:        "grok_acp_error",
					SessionID:   session.ID,
					Seq:         int(seq),
				}, "grok_acp_error") {
					return
				}
				continue
			}
			if lineCount <= 3 {
				fmt.Printf("%s[grok-acp] stdout[%d] %s: %s%s\n",
					colorCyan, lineCount, session.ID, truncateString(trimmed, 200), colorReset)
			}
			if !publishOrFail(resultMsg{
				ID:          session.ID,
				WorkspaceID: session.WorkspaceID,
				UID:         session.UID,
				Output:      trimmed,
				Status:      "success",
				Ts:          time.Now().UnixMilli(),
				Version:     Version,
				Type:        "grok_acp_message",
				SessionID:   session.ID,
				Seq:         int(seq),
			}, "grok_acp_message") {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			fmt.Printf("%s[grok-acp] stdout scanner error for %s: %v%s\n",
				colorRed, session.ID, err, colorReset)
			failSessionFatally(
				fmt.Sprintf("grok stdout scanner error: %v — session terminated", err),
				"grok_acp_scanner_error",
			)
		}
		fmt.Printf("%s[grok-acp] stdout scanner done for %s (%d lines)%s\n",
			colorYellow, session.ID, lineCount, colorReset)
	}()

	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(session.Stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		fmt.Printf("%s[grok-acp] stderr scanner started for %s%s\n",
			colorCyan, session.ID, colorReset)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.TrimSpace(line) == "" {
				continue
			}
			seq := atomic.AddInt64(&session.seq, 1)
			fmt.Printf("%s[grok-acp] stderr %s: %s%s\n",
				colorYellow, session.ID, truncateString(line, 200), colorReset)
			if !publishOrFail(resultMsg{
				ID:          session.ID,
				WorkspaceID: session.WorkspaceID,
				UID:         session.UID,
				Output:      line,
				Status:      "success",
				Ts:          time.Now().UnixMilli(),
				Version:     Version,
				Type:        "grok_acp_stderr",
				SessionID:   session.ID,
				Seq:         int(seq),
			}, "grok_acp_stderr") {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			// stderr is diagnostic, not protocol-critical: a lost/truncated
			// stderr line cannot deadlock the orchestrator's JSON-RPC state
			// machine the way a dropped stdout frame would. Log and continue.
			fmt.Printf("%s[grok-acp] stderr scanner error for %s: %v%s\n",
				colorRed, session.ID, err, colorReset)
		}
	}()

	wg.Wait()
}

func (m *GrokACPManager) waitForExit(session *GrokACPSession, publishFn PublishFunc) {
	err := session.Process.Wait()

	// Stop the deadline timer so a freshly-exited session can't double-fire
	// the timeout grok_acp_error after waitForExit has already published
	// grok_acp_ended. Stop is idempotent and safe whether the timer already
	// fired, is queued, or was never armed.
	if session.timeoutTimer != nil {
		session.timeoutTimer.Stop()
	}

	session.Stdout.Close()
	session.Stderr.Close()

	select {
	case <-session.streamDone:
	case <-time.After(grokACPStreamDrainTimeout):
		fmt.Printf("%s[grok-acp] Stream drain timed out for %s%s\n",
			colorYellow, session.ID, colorReset)
	}

	session.mu.Lock()
	session.status = "ended"
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			session.exitCode = exitErr.ExitCode()
		} else {
			session.exitCode = -1
		}
	}
	exit := session.exitCode
	session.mu.Unlock()

	seq := atomic.AddInt64(&session.seq, 1)

	go publishFn(resultMsg{
		ID:          session.ID,
		WorkspaceID: session.WorkspaceID,
		UID:         session.UID,
		Output:      fmt.Sprintf("grok agent stdio ended (exit code: %d)", exit),
		Status:      "success",
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		Type:        "grok_acp_ended",
		SessionID:   session.ID,
		ExitCode:    exit,
		Seq:         int(seq),
	})

	close(session.done)

	fmt.Printf("%s[grok-acp] Session %s ended (exit code: %d)%s\n",
		colorYellow, session.ID, exit, colorReset)

	m.removeSession(session.ID)
}

/* --------------------------------------------------------------------------
   argv + env builders
   -------------------------------------------------------------------------- */

// buildGrokACPArgs constructs argv for `grok agent stdio`. The literal
// `agent stdio` prefix is the ACP entry point — `grok` itself launches the
// interactive TUI which we deliberately avoid (the feature brief mandates
// JSON-RPC ACP as the primary integration path, not TUI scraping).
//
// extraArgs are forwarded after the entry-point tokens so callers can supply
// Grok config overrides (e.g. `--model grok-2-fast`). Tokens that would
// re-enter the TUI path (`agent`, `stdio`, or alternative subcommands like
// `chat`/`tui`/`run`) are stripped to keep the orchestrator forgiving.
//
// When allowAPIKey is false the sanitiser also drops any caller-supplied
// `--api-key*` / `--auth*` flags so a misbehaving orchestrator can't override
// our env-strip by pointing Grok at an alternative key env var or auth method
// via argv. The orchestrator's ACP `authenticate` flow still resolves
// `cached_token` via the JSON-RPC handshake — no functionality loss for the
// default path.
func buildGrokACPArgs(extraArgs []string, allowAPIKey bool) []string {
	args := []string{"agent", "stdio"}
	args = append(args, sanitizeGrokACPExtraArgs(extraArgs, allowAPIKey)...)
	return args
}

func sanitizeGrokACPExtraArgs(extraArgs []string, allowAPIKey bool) []string {
	// Conservative valued-flag list — we don't currently know every flag
	// `grok` accepts, but covering the common config family lets callers
	// pass values whose token happens to look like a stripped subcommand
	// (`-c stdio=true` style) without us eating them. Keep in lockstep with
	// the codex manager's valuedFlags map.
	valuedFlags := map[string]bool{
		"-c": true, "--config": true,
		"--model": true,
	}
	cleaned := make([]string, 0, len(extraArgs))
	keepNext := false
	skipNext := false
	for _, a := range extraArgs {
		if skipNext {
			skipNext = false
			continue
		}
		if keepNext {
			keepNext = false
			cleaned = append(cleaned, a)
			continue
		}
		lower := strings.ToLower(a)
		switch lower {
		case "agent", "stdio", "chat", "tui", "run":
			// Drop tokens that would re-enter the TUI / interactive REPL
			// path or duplicate the `agent stdio` prefix we already set.
			continue
		}
		if !allowAPIKey && isGrokAuthOverrideArg(lower) {
			// Strip BOTH separate-value (--api-key foo) AND equals-form
			// (--api-key=foo) flags. For separate-value form we also skip
			// the following token so the value doesn't leak through as a
			// stray positional.
			if !strings.Contains(lower, "=") {
				skipNext = true
			}
			continue
		}
		if valuedFlags[lower] {
			keepNext = true
		}
		cleaned = append(cleaned, a)
	}
	return cleaned
}

// isGrokAuthOverrideArg reports whether a caller-supplied arg would let
// the orchestrator point Grok at an API key (or non-cached-token auth
// method) and bypass the default subscription-bound flow. Each known flag
// is enumerated explicitly — a broader `--api-key*` prefix match would
// silently strip flags we don't know about (`--api-key-foo` etc.) and risk
// breaking legitimate non-auth args future Grok releases might ship.
// Match is case-insensitive; callers normalise via strings.ToLower first.
func isGrokAuthOverrideArg(lower string) bool {
	authFlags := []string{
		"--api-key",
		"--api-key-env",
		"--auth",
		"--auth-method",
	}
	for _, f := range authFlags {
		if lower == f || strings.HasPrefix(lower, f+"=") {
			return true
		}
	}
	return false
}

// pathInsideRoot reports whether candidate (already absolute, ideally
// EvalSymlinks-resolved) is strictly inside root (same). Uses
// filepath.Rel to handle Windows drive-letter cases correctly — a plain
// strings.HasPrefix would mis-fire on `/root` vs `/rootkit`.
func pathInsideRoot(candidate, root string) bool {
	if candidate == "" || root == "" {
		return false
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	// "." is the root itself — treat as inside.
	if rel == "." {
		return true
	}
	// A relative path that starts with ".." or "../" escapes the root. On
	// Windows, a path on a different drive returns the absolute path back,
	// which also starts with a drive letter and is therefore filtered by
	// the same check.
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	if filepath.IsAbs(rel) {
		return false
	}
	return true
}

// sanitizeGrokACPEnv applies a strip list to the inherited environment
// before forwarding it to the Grok ACP child. Behaviour:
//
//   - GROK_* / GROK_HOME / PATH / HOME / locale / proxy etc. are forwarded
//     by omission — we never list them in the strip set so the child's
//     shell environment stays intact and `grok login`'s cached token under
//     $GROK_HOME / ~/.grok remains discoverable.
//   - XAI_API_KEY is stripped UNLESS allowAPIKey is true. This is the
//     finding-#3 defence: without a config-level opt-in
//     (Config.EnableGrokAPIKeyFallback), a user who has `export
//     XAI_API_KEY=...` in their shell would otherwise silently fall over
//     to API-key billing if cached-token auth ever fails, despite the
//     feature brief mandating API-key auth be opt-in only.
//   - CLAUDECODE / CLAUDE_* / CODEX_IDE_* are unconditionally stripped
//     because they would tell downstream tooling it is running embedded
//     inside another IDE / agent, which is not true here.
//
// We do NOT pin a `GROK_*` allowlist — a strip-only list keeps the child's
// shell environment intact without us having to enumerate every harmless
// variable Grok might care about.
func sanitizeGrokACPEnv(env []string, allowAPIKey bool) []string {
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		upper := strings.ToUpper(e)
		if strings.HasPrefix(upper, "CLAUDECODE=") ||
			strings.HasPrefix(upper, "CLAUDE_") ||
			strings.HasPrefix(upper, "CODEX_IDE_") {
			continue
		}
		if !allowAPIKey && strings.HasPrefix(upper, "XAI_API_KEY=") {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered
}
