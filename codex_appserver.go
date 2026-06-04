// File: codex_appserver.go
// -----------------------------------------------------------------------------
// CodexAppServerManager — long-lived `codex app-server --listen stdio://`
// sessions used by AI Expedite to drive Codex via its JSON-RPC 2.0 IDE
// protocol (newline-delimited JSON over the child process' stdio).
//
// The transport-only design keeps this file deliberately small: we spawn
// codex, validate that every outbound frame is a single JSON object so the
// wire's JSONL framing stays intact, and forward every inbound stdout line
// verbatim to the orchestrator as a `codex_appserver_message` result. The
// orchestrator owns all JSON-RPC semantics — request/response correlation,
// the `initialize` handshake, approval responses, capability negotiation —
// so this agent doesn't need to know about codex method names or evolve in
// lockstep with the upstream protocol.
//
// Per the OpenAI Codex IDE documentation
// (https://developers.openai.com/codex/app-server) the stdio transport is the
// default. We pass `--listen stdio://` explicitly anyway so the chosen
// transport is visible in process listings and a future change to codex's
// default cannot silently swap us onto WebSocket or Unix sockets.
//
// Lifecycle: Start spawns the child, registers it in the global process
// registry (so the orphan scanner doesn't kill it), and launches stdout +
// stderr reader goroutines. End closes stdin first (codex's documented
// graceful-shutdown path) and falls through to interrupt + kill on timeout.
// waitForExit publishes a `codex_appserver_ended` frame and removes the
// session from the manager.
// -----------------------------------------------------------------------------

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// codexAppServerMaxLineSize caps a single JSONL frame on stdout. Approval
	// requests can carry large patch payloads, so we mirror the 30 MB cap
	// used by session.go's CLI stream scanner.
	codexAppServerMaxLineSize = 30 * 1024 * 1024

	// codexAppServerStdinWriteTimeout is the upper bound on a single stdin
	// write before we declare the pipe stalled. Matches session.go's
	// SendInput budget — pipe buffer should never legitimately back up for
	// this long.
	codexAppServerStdinWriteTimeout = 10 * time.Second

	// codexAppServerGracefulShutdownTimeout is how long End waits after
	// closing stdin (codex's documented exit path) before escalating to
	// SIGINT and ultimately SIGKILL.
	codexAppServerGracefulShutdownTimeout = 5 * time.Second

	// codexAppServerStreamDrainTimeout is how long waitForExit waits for the
	// stdout/stderr readers to finish draining buffered frames before
	// publishing `codex_appserver_ended`. Generous to avoid racing the last
	// JSON-RPC response with the exit notification.
	codexAppServerStreamDrainTimeout = 30 * time.Second

	// codexAppServerMaxLifetime caps how long a session may stay open before
	// CleanupStale ends it. Matches SessionManager's 6 h ceiling so an
	// orchestrator crash can't leak codex children indefinitely (each child
	// holds an OpenAI auth session that keeps billing).
	codexAppServerMaxLifetime = 6 * time.Hour

	// codexAppServerCleanupInterval is how often the stale cleanup goroutine
	// scans for expired sessions.
	codexAppServerCleanupInterval = 60 * time.Second

	// codexAppServerPublishConcurrency caps the per-session pub/sub publish
	// fan-out. Each publish can take up to 30 s on a slow network; running
	// them serially would back-pressure the stdout pipe and stall codex.
	// Mirrors the 5-slot semaphore used by session.go's stream batcher.
	codexAppServerPublishConcurrency = 5

	// codexAppServerPublishQueueTimeout drops a publish if no semaphore slot
	// frees within this window — prevents goroutine buildup when Pub/Sub is
	// fully wedged. Mirrors session.go's 5 s drop threshold.
	codexAppServerPublishQueueTimeout = 5 * time.Second
)

/* --------------------------------------------------------------------------
   CodexAppServerSession — one running `codex app-server` process
   -------------------------------------------------------------------------- */

// CodexAppServerSession holds a single child process and its stdio pipes.
// All public state is guarded by mu; stdin writes are serialised separately
// by stdinMu so concurrent Send callers cannot interleave JSONL frames.
type CodexAppServerSession struct {
	ID          string
	Process     *exec.Cmd
	Stdin       io.WriteCloser
	Stdout      io.ReadCloser
	Stderr      io.ReadCloser
	StartedAt   time.Time
	WorkspaceID string
	UID         string
	Cwd         string

	mu         sync.Mutex
	status     string // "running" | "ended"
	exitCode   int
	stdinMu    sync.Mutex
	stdinClose sync.Once
	done       chan struct{}
	streamDone chan struct{}
	seq        int64
}

// Status returns the current lifecycle status under the session mutex so
// callers don't race with waitForExit's transition to "ended".
func (s *CodexAppServerSession) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// closeStdin is idempotent: the End fallback path calls it after stdin may
// already have been closed by waitForExit, and the second close would
// otherwise return an `io: already closed` error.
func (s *CodexAppServerSession) closeStdin() {
	s.stdinClose.Do(func() {
		_ = s.Stdin.Close()
	})
}

/* --------------------------------------------------------------------------
   CodexAppServerManager — tracks every active session
   -------------------------------------------------------------------------- */

// CodexAppServerManager owns the active codex app-server processes. One
// manager handles many concurrent sessions, mirroring SessionManager's shape.
type CodexAppServerManager struct {
	sessions map[string]*CodexAppServerSession
	mu       sync.RWMutex
	Config   *Config
}

// NewCodexAppServerManager creates a fresh manager. cfg is retained so future
// per-workspace settings can be threaded through without breaking callers.
func NewCodexAppServerManager(cfg *Config) *CodexAppServerManager {
	return &CodexAppServerManager{
		sessions: make(map[string]*CodexAppServerSession),
		Config:   cfg,
	}
}

// Start launches `codex app-server --listen stdio://` in cwd. extraArgs are
// passed through after the built-in transport flags so the orchestrator can
// supply `-c model="gpt-5.4"` or `-c profile="work"` without us special-casing
// every codex config knob.
//
// publishFn receives:
//   - `codex_appserver_message` for every JSONL frame codex emits on stdout
//   - `codex_appserver_stderr`  for every line codex emits on stderr
//   - `codex_appserver_error`   when codex emits a non-JSON line (protocol bug)
//   - `codex_appserver_ended`   when the process exits, carrying ExitCode
func (m *CodexAppServerManager) Start(id, cwd string, extraArgs []string, workspaceID, uid string, publishFn PublishFunc) error {
	if id == "" {
		return fmt.Errorf("sessionID is required")
	}
	if publishFn == nil {
		return fmt.Errorf("publishFn is required")
	}

	m.mu.Lock()
	if _, exists := m.sessions[id]; exists {
		m.mu.Unlock()
		return fmt.Errorf("codex app-server session %s already exists", id)
	}
	m.mu.Unlock()

	executable := resolveExecutable("codex")
	args := buildCodexAppServerArgs(extraArgs)

	fmt.Printf("%s[codex-appserver] Starting session %s: %s %s%s\n",
		colorCyan, id, executable, strings.Join(args, " "), colorReset)

	proc := exec.Command(executable, args...)
	hideWindow(proc)
	if cwd != "" {
		proc.Dir = cwd
	}
	proc.Env = sanitizeCodexAppServerEnv(os.Environ())

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
		return fmt.Errorf("failed to start codex app-server (is `codex` on PATH?): %w", err)
	}

	session := &CodexAppServerSession{
		ID:          id,
		Process:     proc,
		Stdin:       stdin,
		Stdout:      stdout,
		Stderr:      stderr,
		StartedAt:   time.Now(),
		WorkspaceID: workspaceID,
		UID:         uid,
		Cwd:         cwd,
		status:      "running",
		done:        make(chan struct{}),
		streamDone:  make(chan struct{}),
	}

	m.mu.Lock()
	m.sessions[id] = session
	m.mu.Unlock()

	if proc.Process != nil {
		globalProcessRegistry.Register(proc.Process.Pid, "codex-appserver:"+id)
	}

	go m.readStream(session, publishFn)
	go m.waitForExit(session, publishFn)

	fmt.Printf("%s[codex-appserver] Session %s started (PID: %d)%s\n",
		colorGreen, id, proc.Process.Pid, colorReset)
	return nil
}

// Send writes a single JSON-RPC 2.0 frame to the child's stdin. payload must
// be a self-contained JSON object — typically a request, response or
// notification per the OpenAI Codex IDE app-server protocol. Send validates
// that the payload is parseable JSON and contains no embedded newlines (which
// would corrupt the JSONL framing on the wire) but never edits its content,
// so callers retain exact control over JSON-RPC ids and method names.
func (m *CodexAppServerManager) Send(id string, payload string) error {
	session := m.Get(id)
	if session == nil {
		return fmt.Errorf("codex app-server session %s not found", id)
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

	if session.Status() == "ended" {
		return fmt.Errorf("codex app-server session %s has ended", id)
	}

	session.stdinMu.Lock()
	defer session.stdinMu.Unlock()

	writeDone := make(chan error, 1)
	go func() {
		_, err := fmt.Fprintln(session.Stdin, trimmed)
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		if err != nil {
			return fmt.Errorf("failed to write to codex app-server session %s stdin: %w", id, err)
		}
	case <-time.After(codexAppServerStdinWriteTimeout):
		return fmt.Errorf("timeout writing to codex app-server session %s stdin (pipe buffer full)", id)
	}

	fmt.Printf("%s[codex-appserver] → %s (%d bytes)%s\n",
		colorBlue, id, len(trimmed), colorReset)
	return nil
}

// End shuts down a session. First it closes stdin, codex's documented exit
// path. If the process is still alive after codexAppServerGracefulShutdownTimeout
// we interrupt it, and if that also fails we SIGKILL.
func (m *CodexAppServerManager) End(id string) error {
	session := m.Get(id)
	if session == nil {
		return fmt.Errorf("codex app-server session %s not found", id)
	}

	if session.Status() == "ended" {
		m.removeSession(id)
		return nil
	}

	fmt.Printf("%s[codex-appserver] Ending session %s gracefully...%s\n",
		colorYellow, id, colorReset)

	session.closeStdin()

	select {
	case <-session.done:
	case <-time.After(codexAppServerGracefulShutdownTimeout):
		fmt.Printf("%s[codex-appserver] Stdin close didn't exit %s — interrupting%s\n",
			colorYellow, id, colorReset)
		_ = interruptProcess(session.Process)
		select {
		case <-session.done:
		case <-time.After(codexAppServerGracefulShutdownTimeout):
			fmt.Printf("%s[codex-appserver] Force killing session %s%s\n",
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
func (m *CodexAppServerManager) Get(id string) *CodexAppServerSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

// ActiveCount returns the number of currently tracked sessions.
func (m *CodexAppServerManager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// CleanupStale runs periodically to end sessions that exceed maxAge. Call as
// a goroutine: `go m.CleanupStale(codexAppServerMaxLifetime)`. Without this,
// an orchestrator crash that drops the `codex_appserver_end` signal would
// leak codex children indefinitely — each child holds an OpenAI auth session
// and keeps billing until the OS reaps the process. Mirrors
// SessionManager.CleanupStale.
func (m *CodexAppServerManager) CleanupStale(maxAge time.Duration) {
	ticker := time.NewTicker(codexAppServerCleanupInterval)
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
func (m *CodexAppServerManager) endStaleSessions(maxAge time.Duration) {
	m.mu.RLock()
	var staleIDs []string
	for id, session := range m.sessions {
		if time.Since(session.StartedAt) > maxAge {
			staleIDs = append(staleIDs, id)
		}
	}
	m.mu.RUnlock()

	for _, id := range staleIDs {
		fmt.Printf("%s[codex-appserver] Cleaning up stale session %s (exceeded %v)%s\n",
			colorYellow, id, maxAge, colorReset)
		_ = m.End(id)
	}
}

// ShutdownAll ends every active session. Called during agent shutdown so
// codex children don't outlive the agent and silently consume tokens.
func (m *CodexAppServerManager) ShutdownAll() {
	m.mu.RLock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	if len(ids) > 0 {
		fmt.Printf("%s[codex-appserver] Shutting down %d active session(s)...%s\n",
			colorYellow, len(ids), colorReset)
	}
	for _, id := range ids {
		_ = m.End(id)
	}
}

func (m *CodexAppServerManager) removeSession(id string) {
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

// readStream forwards every codex stdout frame as `codex_appserver_message`
// and every stderr line as `codex_appserver_stderr`. stdout frames are
// validated as JSON (so a malformed frame is surfaced as
// `codex_appserver_error` instead of being silently passed through), but the
// original line text is forwarded verbatim — we never edit codex's wire
// format.
//
// Publishing is fan-out async (mirroring session.go's stream batcher): each
// publishFn call goes through a 5-slot semaphore so a slow Pub/Sub network
// cannot back-pressure the stdout pipe and stall codex. The returned
// asyncPublish + its waitgroup are exposed to waitForExit so the
// `codex_appserver_ended` frame is published AFTER every queued message
// frame — without that ordering, terminal-service would see exit before the
// final JSON-RPC response and the orchestrator would receive a truncated
// stream.
func (m *CodexAppServerManager) readStream(session *CodexAppServerSession, publishFn PublishFunc) {
	defer close(session.streamDone)

	publishSem := make(chan struct{}, codexAppServerPublishConcurrency)
	var publishWg sync.WaitGroup
	// Wait for all in-flight publishes to settle before signalling streamDone.
	// waitForExit blocks on streamDone before publishing `_ended`, so this
	// chain preserves message ordering across the slow-network edge case.
	defer publishWg.Wait()

	asyncPublish := func(msg resultMsg) {
		publishWg.Add(1)
		select {
		case publishSem <- struct{}{}:
			go func() {
				defer publishWg.Done()
				defer func() { <-publishSem }()
				publishFn(msg)
			}()
		case <-time.After(codexAppServerPublishQueueTimeout):
			publishWg.Done()
			fmt.Printf("%s[codex-appserver] Publish queue full, dropping frame for %s (type=%s)%s\n",
				colorYellow, session.ID, msg.Type, colorReset)
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(session.Stdout)
		scanner.Buffer(make([]byte, 0, 256*1024), codexAppServerMaxLineSize)
		fmt.Printf("%s[codex-appserver] stdout scanner started for %s%s\n",
			colorCyan, session.ID, colorReset)
		lineCount := 0
		for scanner.Scan() {
			lineCount++
			line := scanner.Text()
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			seq := atomic.AddInt64(&session.seq, 1)

			var probe json.RawMessage
			if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
				// Non-JSON output on stdout is a protocol violation. Surface
				// it to the orchestrator with a clear type so it isn't
				// confused with a legitimate JSON-RPC frame.
				fmt.Printf("%s[codex-appserver] Non-JSON stdout frame on %s: %v (line=%s)%s\n",
					colorRed, session.ID, err, truncateString(trimmed, 200), colorReset)
				asyncPublish(resultMsg{
					ID:          session.ID,
					WorkspaceID: session.WorkspaceID,
					UID:         session.UID,
					Output:      fmt.Sprintf("non-JSON frame on codex app-server stdout: %v", err),
					Status:      "error",
					Ts:          time.Now().UnixMilli(),
					Version:     Version,
					Type:        "codex_appserver_error",
					SessionID:   session.ID,
					Seq:         int(seq),
				})
				continue
			}
			if lineCount <= 3 {
				fmt.Printf("%s[codex-appserver] stdout[%d] %s: %s%s\n",
					colorCyan, lineCount, session.ID, truncateString(trimmed, 200), colorReset)
			}
			asyncPublish(resultMsg{
				ID:          session.ID,
				WorkspaceID: session.WorkspaceID,
				UID:         session.UID,
				Output:      trimmed,
				Status:      "success",
				Ts:          time.Now().UnixMilli(),
				Version:     Version,
				Type:        "codex_appserver_message",
				SessionID:   session.ID,
				Seq:         int(seq),
			})
		}
		if err := scanner.Err(); err != nil {
			fmt.Printf("%s[codex-appserver] stdout scanner error for %s: %v%s\n",
				colorRed, session.ID, err, colorReset)
		}
		fmt.Printf("%s[codex-appserver] stdout scanner done for %s (%d lines)%s\n",
			colorYellow, session.ID, lineCount, colorReset)
	}()

	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(session.Stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		fmt.Printf("%s[codex-appserver] stderr scanner started for %s%s\n",
			colorCyan, session.ID, colorReset)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.TrimSpace(line) == "" {
				continue
			}
			seq := atomic.AddInt64(&session.seq, 1)
			fmt.Printf("%s[codex-appserver] stderr %s: %s%s\n",
				colorYellow, session.ID, truncateString(line, 200), colorReset)
			asyncPublish(resultMsg{
				ID:          session.ID,
				WorkspaceID: session.WorkspaceID,
				UID:         session.UID,
				Output:      line,
				Status:      "success",
				Ts:          time.Now().UnixMilli(),
				Version:     Version,
				Type:        "codex_appserver_stderr",
				SessionID:   session.ID,
				Seq:         int(seq),
			})
		}
		if err := scanner.Err(); err != nil {
			fmt.Printf("%s[codex-appserver] stderr scanner error for %s: %v%s\n",
				colorRed, session.ID, err, colorReset)
		}
	}()

	wg.Wait()
}

func (m *CodexAppServerManager) waitForExit(session *CodexAppServerSession, publishFn PublishFunc) {
	err := session.Process.Wait()

	// Explicitly close pipes so the scanner goroutines receive EOF; mirrors
	// session.go's behaviour where Windows pipe handles can otherwise linger
	// past process exit.
	session.Stdout.Close()
	session.Stderr.Close()

	select {
	case <-session.streamDone:
	case <-time.After(codexAppServerStreamDrainTimeout):
		fmt.Printf("%s[codex-appserver] Stream drain timed out for %s%s\n",
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

	close(session.done)

	seq := atomic.AddInt64(&session.seq, 1)
	publishFn(resultMsg{
		ID:          session.ID,
		WorkspaceID: session.WorkspaceID,
		UID:         session.UID,
		Output:      fmt.Sprintf("codex app-server ended (exit code: %d)", exit),
		Status:      "success",
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		Type:        "codex_appserver_ended",
		SessionID:   session.ID,
		ExitCode:    exit,
		Seq:         int(seq),
	})

	fmt.Printf("%s[codex-appserver] Session %s ended (exit code: %d)%s\n",
		colorYellow, session.ID, exit, colorReset)

	m.removeSession(session.ID)
}

/* --------------------------------------------------------------------------
   argv + env builders
   -------------------------------------------------------------------------- */

// buildCodexAppServerArgs constructs argv for `codex app-server`. Stdio is
// codex's default transport; we still pass `--listen stdio://` explicitly so
// the transport is visible in `ps` output and any future change to codex's
// default cannot silently swap us onto WebSocket or Unix sockets.
//
// extraArgs are forwarded after the built-in transport flags so callers can
// supply codex config overrides (e.g. `-c model="gpt-5.4"`). Tokens that
// would conflict with our transport contract (`app-server`, `--listen`) are
// stripped to keep the orchestrator forgiving.
func buildCodexAppServerArgs(extraArgs []string) []string {
	args := []string{"app-server", "--listen", "stdio://"}
	args = append(args, sanitizeCodexAppServerExtraArgs(extraArgs)...)
	return args
}

func sanitizeCodexAppServerExtraArgs(extraArgs []string) []string {
	cleaned := make([]string, 0, len(extraArgs))
	skipNext := false
	for i, a := range extraArgs {
		if skipNext {
			skipNext = false
			continue
		}
		lower := strings.ToLower(a)
		switch {
		case lower == "app-server":
			continue
		case lower == "--listen":
			// `--listen ws://...` would swap us onto WebSocket transport,
			// breaking the JSONL-over-stdio contract this manager assumes.
			// Drop the flag and its value.
			if i+1 < len(extraArgs) {
				skipNext = true
			}
			continue
		case strings.HasPrefix(lower, "--listen="):
			continue
		}
		cleaned = append(cleaned, a)
	}
	return cleaned
}

// sanitizeCodexAppServerEnv strips environment variables that would confuse
// codex inside a nested session. We preserve CODEX_HOME and OPENAI_API_KEY
// so the user's existing login / API-key configuration carries over (per the
// OpenAI Codex IDE auth docs).
func sanitizeCodexAppServerEnv(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		upper := strings.ToUpper(e)
		// CLAUDE_*: if this agent was launched from a Claude Code shell, the
		// parent Claude env would otherwise leak in and confuse downstream
		// telemetry. CODEX_IDE_*: set by the official VS Code / JetBrains
		// extensions and would tell codex it is running embedded inside that
		// IDE, which is not true here.
		if strings.HasPrefix(upper, "CLAUDECODE=") ||
			strings.HasPrefix(upper, "CLAUDE_") ||
			strings.HasPrefix(upper, "CODEX_IDE_") {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered
}
