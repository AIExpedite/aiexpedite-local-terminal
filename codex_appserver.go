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
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// codexAppServerMaxLineSize caps the bufio.Scanner buffer for a single
	// JSONL frame. A generous 30 MB ceiling matches session.go's CLI stream
	// scanner — but the downstream Pub/Sub message limit is 10 MB, so frames
	// bigger than codexAppServerMaxFrameSize cannot survive a publish and
	// are session-fatal (see codexAppServerMaxFrameSize below).
	codexAppServerMaxLineSize = 30 * 1024 * 1024

	// codexAppServerMaxFrameSize is a cheap pre-check on the raw stdout line.
	// Frames bigger than this cannot possibly fit Pub/Sub's 10 MB envelope
	// even before JSON escaping, so we reject them without paying the cost of
	// building/marshaling a resultMsg. The authoritative gate is
	// codexAppServerMaxPublishSize below: the Output field is JSON-string-
	// escaped on marshal, and a frame heavy in '"' / '\' can roughly double
	// in size, so a line that passes this pre-check can still fail the
	// marshaled-envelope check. Frames that fail either check are session-
	// fatal — silently dropping one would deadlock the orchestrator's
	// JSON-RPC state machine waiting for a response that never arrives.
	// Addresses Finding #4 (raw cap) and the follow-up escape-amplification
	// finding from the secondary review.
	codexAppServerMaxFrameSize = 8 * 1024 * 1024

	// codexAppServerMaxPublishSize is GCP Pub/Sub's documented per-message
	// publish limit (10_000_000 bytes). After building each resultMsg, the
	// stream reader marshals it and rejects envelopes that exceed this
	// ceiling — that catches the case where Codex emits a frame whose raw
	// bytes are under codexAppServerMaxFrameSize but whose JSON-string-
	// escaped Output field marshals beyond what Pub/Sub will accept. A
	// silently-rejected publish would leave the orchestrator waiting for a
	// JSON-RPC response that never arrives, so oversize envelopes trigger
	// the same fail-fast (`codex_appserver_error` + kill child) as oversize
	// raw frames.
	codexAppServerMaxPublishSize = 10_000_000

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

	// codexAppServerPublishQueueSize bounds the per-session publish backlog.
	// Frames are enqueued in order and drained by a single publisher
	// goroutine, so codex stays decoupled from Pub/Sub latency without
	// risking reordering — unlike session.go's stream batcher, which is free
	// to drop unstructured stream chunks under overload, every codex
	// app-server frame is a stateful JSON-RPC message (request id, turn
	// completion, approval request). Losing one corrupts the session. If the
	// backlog fills the publisher is too slow to keep up; we surface a
	// fatal error and kill the child rather than silently dropping (see
	// codexAppServerEnqueueTimeout below).
	codexAppServerPublishQueueSize = 256

	// codexAppServerEnqueueTimeout is the upper bound for the stream readers
	// to wait when the publish queue is full. Hitting this means Pub/Sub has
	// stalled for a sustained period; the session cannot continue safely
	// (silently dropped frames would break the JSON-RPC state machine on the
	// orchestrator), so the manager publishes a `codex_appserver_error`
	// surface and force-kills the child to fail-fast.
	codexAppServerEnqueueTimeout = 30 * time.Second
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
}

// NewCodexAppServerManager creates a fresh manager.
func NewCodexAppServerManager() *CodexAppServerManager {
	return &CodexAppServerManager{
		sessions: make(map[string]*CodexAppServerSession),
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

	// cwd is required and must point at an existing directory. Falling back
	// to the agent's working directory would silently run codex against a
	// surprise path (e.g. C:\Program Files\AI Expedite on Windows), exposing
	// or editing files unrelated to the requested workspace. session.go's
	// session_start handler accepts an empty cwd because legacy CLI agents
	// did, but the IDE/app-server path is new and can require it.
	if cwd == "" {
		return fmt.Errorf("cwd is required for codex app-server (must point at a workspace directory)")
	}
	if !filepath.IsAbs(cwd) {
		return fmt.Errorf("cwd must be an absolute path; got %q", cwd)
	}
	if info, err := os.Stat(cwd); err != nil {
		return fmt.Errorf("cwd %q is not accessible: %w", cwd, err)
	} else if !info.IsDir() {
		return fmt.Errorf("cwd %q is not a directory", cwd)
	}

	// Hold the manager mutex across the entire spawn so two concurrent Start
	// calls for the same id can't both pass the existence check and double-
	// spawn (the previous check-release-insert pattern had that TOCTOU race —
	// the first session's process would be silently orphaned because the
	// manager's map only kept the second). proc.Start() returns in tens of
	// milliseconds, so serialising starts has no real cost. Mirrors
	// SessionManager.StartSession in session.go which also holds its mutex
	// across exec.Start.
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.sessions[id]; exists {
		return fmt.Errorf("codex app-server session %s already exists", id)
	}

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
		status:      "running",
		done:        make(chan struct{}),
		streamDone:  make(chan struct{}),
	}

	m.sessions[id] = session

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
//
// Timeout policy is fail-fast: on stdin write timeout the manager kills the
// child and closes stdin BEFORE returning. This guarantees no later Send can
// race with the abandoned write goroutine — the next Send sees Status=ended
// and rejects, and the orphaned write (if it ever wakes up) lands on a closed
// pipe. Without that guarantee, two timed-out Sends could interleave their
// JSON-RPC frames on the wire and break request/response correlation.
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

	session.stdinMu.Lock()
	defer session.stdinMu.Unlock()

	// Re-check Status under stdinMu so we cannot pass the gate after another
	// goroutine has already started tearing the session down via End() or
	// the timeout-fail path below.
	if session.Status() == "ended" {
		return fmt.Errorf("codex app-server session %s has ended", id)
	}

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
		// Fatal: a stalled write is a signal that codex isn't draining stdin.
		// Continuing would let the next Send acquire stdinMu and interleave
		// its frame with the abandoned write's eventual completion. Close
		// stdin to unblock the abandoned goroutine immediately, transition
		// to "ended" so concurrent/subsequent Sends short-circuit, and kill
		// the child so waitForExit publishes codex_appserver_ended.
		session.closeStdin()
		session.mu.Lock()
		session.status = "ended"
		session.mu.Unlock()
		if session.Process != nil && session.Process.Process != nil {
			_ = session.Process.Process.Kill()
		}
		return fmt.Errorf("timeout writing to codex app-server session %s stdin — session terminated to prevent frame interleave", id)
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
// Publishing uses a single ordered consumer goroutine that drains a bounded
// queue. Every frame in the Codex IDE stdio protocol is a stateful JSON-RPC
// message (a response correlated by id, a turn-completion notification, an
// approval request); silently dropping one would corrupt the orchestrator's
// session state. So:
//   - Frames are NEVER dropped under normal operation.
//   - Every frame carries a monotonic `Seq`, and the orchestrator MUST order
//     and dedup by `Seq`, NOT by Pub/Sub arrival order. Publish order can
//     legitimately differ from `Seq` order: the stdout and stderr scanners each
//     assign `Seq` (atomic) *before* enqueuing, so the two producers can win the
//     channel race out of `Seq` order, and the fatal-error path below publishes
//     synchronously, bypassing the queue. The only invariants the orchestrator
//     relies on are on `Seq` itself (e.g. `_ended` always carries the highest
//     `Seq`, assigned after streamDone), never on arrival order.
//   - If Pub/Sub stalls for codexAppServerEnqueueTimeout while the queue is
//     full, the manager publishes a fatal codex_appserver_error and force-
//     kills the child. Failing-fast is the only safe response: continuing
//     with dropped frames produces silently-broken sessions.
func (m *CodexAppServerManager) readStream(session *CodexAppServerSession, publishFn PublishFunc) {
	defer close(session.streamDone)

	// Single ordered publisher goroutine. The buffered channel preserves the
	// order frames are *enqueued* and the publisher drains serially, so Pub/Sub
	// retries can't reorder what's already queued. Note this is enqueue order,
	// which can differ from `Seq` order (the two producers race seq-assign vs
	// enqueue); the orchestrator sorts by `Seq`, not by arrival.
	queue := make(chan resultMsg, codexAppServerPublishQueueSize)
	publisherDone := startTrackedTerminalPublisher(queue, publishFn)
	// Close queue after both scanners finish so the publisher goroutine drains
	// every remaining frame before signalling streamDone.
	defer func() {
		close(queue)
		<-publisherDone
	}()

	// enqueue blocks up to codexAppServerEnqueueTimeout for queue space.
	// Returns false if the queue is wedged — caller MUST treat that as
	// session-fatal and stop reading (further reads can't be published in
	// order anyway, and dropping silently breaks the JSON-RPC contract).
	enqueue := func(msg resultMsg) bool {
		select {
		case queue <- msg:
			return true
		case <-time.After(codexAppServerEnqueueTimeout):
			return false
		}
	}

	// failSessionFatally surfaces a queue-stall diagnostic via a synchronous
	// publish (bypassing the wedged queue) and kills the child so waitForExit
	// publishes codex_appserver_ended. The publish is synchronous so the
	// diagnostic is actually emitted before the session is torn down; ordering
	// against the async `_ended` is guaranteed by `Seq`, not by this being
	// synchronous (`_error` here gets a lower `Seq` than `_ended`, which is
	// assigned after streamDone), so the orchestrator can always reconstruct
	// "_error then _ended" regardless of arrival order.
	//
	// CAVEAT: this path's trigger is a wedged/slow Pub/Sub, and this synchronous
	// publish uses the SAME publishFn. If Pub/Sub is genuinely DOWN (not merely
	// slow), this call also blocks for the full publishFn timeout (~30 s) and
	// then fails — so the `_error` diagnostic may itself be dropped and the
	// child-kill is delayed until that timeout elapses (worst case roughly
	// enqueueTimeout + publish timeout before the session dies). The diagnostic
	// only reliably lands when Pub/Sub is responsive-but-slow; a true outage
	// degrades to "child killed late, no diagnostic" — still safe (no dropped
	// JSON-RPC frame is mistaken for a live session), just not observable.
	failSessionFatally := func(reason string, droppedType string) {
		fmt.Printf("%s[codex-appserver] Publish queue stalled for %s — failing session (dropped %s)%s\n",
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
			Type:        "codex_appserver_error",
			SessionID:   session.ID,
			Seq:         int(seq),
		})
		if session.Process != nil && session.Process.Process != nil {
			_ = session.Process.Process.Kill()
		}
	}

	// publishOrFail is the single gate for non-fatal stream frames. It
	// marshals msg first so we can verify the resulting envelope fits
	// Pub/Sub's 10 MB ceiling (the raw-line pre-check above can pass while
	// JSON-string escaping doubles a frame heavy in '"' / '\' beyond what
	// Pub/Sub will accept). On either an oversize envelope or a stalled
	// publish queue it triggers failSessionFatally and returns false so the
	// scanner stops — silently continuing would risk dropping subsequent
	// JSON-RPC frames the orchestrator is correlating against. The double-
	// marshal (here and again in newSessionPublishFn) is the price of
	// keeping PublishFunc's signature unchanged; marshaling a sub-10 MB
	// envelope is cheap relative to the session-fatal cost of a silent drop.
	publishOrFail := func(msg resultMsg, droppedType string) bool {
		encoded, err := json.Marshal(msg)
		if err != nil {
			failSessionFatally(
				fmt.Sprintf("codex app-server envelope failed to marshal: %v — session terminated", err),
				droppedType,
			)
			return false
		}
		if len(encoded) > codexAppServerMaxPublishSize {
			failSessionFatally(
				fmt.Sprintf(
					"codex app-server envelope marshaled to %d bytes after JSON escaping, exceeding the %d-byte publishable limit — session terminated to avoid silent Pub/Sub rejection",
					len(encoded), codexAppServerMaxPublishSize,
				),
				droppedType,
			)
			return false
		}
		if !enqueue(msg) {
			failSessionFatally("codex app-server publish queue stalled — terminating session to avoid dropping JSON-RPC frames", droppedType)
			return false
		}
		return true
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

			// Pre-publish size check (Finding #4 in the secondary review):
			// Pub/Sub messages have a 10 MB ceiling. A frame larger than
			// codexAppServerMaxFrameSize cannot survive a publish — Pub/Sub
			// rejects oversize messages and the orchestrator would never see
			// that JSON-RPC response (turn-completion, approval request, …),
			// leaving its state machine deadlocked waiting for a frame that
			// never arrives. Fail-fast so the orchestrator gets a diagnostic
			// + session_ended instead of silent hangs.
			if len(trimmed) > codexAppServerMaxFrameSize {
				failSessionFatally(
					fmt.Sprintf(
						"codex emitted a %d-byte frame exceeding the %d-byte publishable limit — session terminated to avoid silent drop",
						len(trimmed), codexAppServerMaxFrameSize,
					),
					"codex_appserver_oversize_frame",
				)
				return
			}

			seq := atomic.AddInt64(&session.seq, 1)

			var probe json.RawMessage
			if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
				// Non-JSON output on stdout is a protocol violation. Surface
				// it to the orchestrator with a clear type so it isn't
				// confused with a legitimate JSON-RPC frame.
				fmt.Printf("%s[codex-appserver] Non-JSON stdout frame on %s: %v (line=%s)%s\n",
					colorRed, session.ID, err, truncateString(trimmed, 200), colorReset)
				if !publishOrFail(resultMsg{
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
				}, "codex_appserver_error") {
					return
				}
				continue
			}
			if lineCount <= 3 {
				fmt.Printf("%s[codex-appserver] stdout[%d] %s: %s%s\n",
					colorCyan, lineCount, session.ID, truncateString(trimmed, 200), colorReset)
			}
			// Passive utilization capture: extract token_count rate_limits
			// into the per-account cache cliagent_usage_codex.go reads from.
			// Side-effect only — does not alter framing or block publish.
			captureCodexRateLimitLine(trimmed, time.Now())
			if !publishOrFail(resultMsg{
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
			}, "codex_appserver_message") {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			// ErrTooLong (a single line exceeding codexAppServerMaxLineSize)
			// is session-fatal — the scanner is dead after this and any
			// subsequent JSON-RPC frame would be lost. Same fail-fast
			// applies to any other unexpected scanner error so we don't
			// leave the orchestrator waiting for frames that never come.
			fmt.Printf("%s[codex-appserver] stdout scanner error for %s: %v%s\n",
				colorRed, session.ID, err, colorReset)
			failSessionFatally(
				fmt.Sprintf("codex stdout scanner error: %v — session terminated", err),
				"codex_appserver_scanner_error",
			)
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
			if !publishOrFail(resultMsg{
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
			}, "codex_appserver_stderr") {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			// Unlike the stdout scanner (which fails the session on a scanner
			// error), a stderr error — including ErrTooLong on a line past the
			// 1 MB cap set above — is deliberately NOT session-fatal. stderr is
			// diagnostic, not protocol-critical: a lost or truncated stderr line
			// cannot deadlock the orchestrator's JSON-RPC state machine the way a
			// dropped stdout frame would. Log and let the session continue.
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

	// Wait for readStream to finish draining + flushing every queued publish.
	// readStream's deferred publishWg.Wait() runs before its deferred
	// close(streamDone), so by the time we unblock here the in-flight
	// message frames have already been delivered to publishFn. That preserves
	// the invariant the orchestrator relies on: codex_appserver_ended is
	// strictly the last frame for this session and carries the highest Seq.
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

	seq := atomic.AddInt64(&session.seq, 1)

	// Publish codex_appserver_ended in a goroutine: publishFn can block up to
	// 30 s on Pub/Sub network I/O. Calling it directly here would hold the
	// session slot (delaying removeSession below) and slow EndSession-style
	// callers waiting on session.done. Mirrors session.go's session_ended
	// publish pattern.
	publishTerminalResultAsync(publishFn, resultMsg{
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

	// Closing `done` after the publish goroutine has been launched (but
	// without waiting on it) means End() can unblock and the manager can
	// reclaim the session slot in parallel with the Pub/Sub round-trip.
	close(session.done)

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
	// codex flags whose NEXT token is their value, not a positional. We must not
	// interpret such a value as a transport override: a caller passing
	// `-c --listen` means the config KEY "--listen", not a real `--listen` flag.
	// Without this guard the old code stripped that value AND skipped the token
	// after it, corrupting the `-c` override and silently dropping an arg.
	valuedFlags := map[string]bool{
		"-c": true, "--config": true,
		"--enable": true, "--disable": true,
		"--ws-auth": true,
	}
	cleaned := make([]string, 0, len(extraArgs))
	skipNext := false // skip the value of a stripped --listen
	keepNext := false // pass through the value of a valued flag verbatim
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
		switch {
		case lower == "app-server":
			continue
		case lower == "--listen":
			// `--listen ws://...` would swap us onto WebSocket transport,
			// breaking the JSONL-over-stdio contract this manager assumes.
			// Drop the flag and its value (skipNext is harmless if it's last).
			skipNext = true
			continue
		case strings.HasPrefix(lower, "--listen="):
			continue
		}
		if valuedFlags[lower] {
			keepNext = true
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
