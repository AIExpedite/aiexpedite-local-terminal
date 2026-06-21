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
	"runtime"
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
	// WorkspaceRoot is the symlink-resolved containment root captured at Start
	// (empty when the dispatcher didn't configure one). Send uses it to
	// re-enforce containment on later JSON-RPC session-setup frames
	// (`session/new` and `session/load`) whose `params.cwd` would otherwise
	// bypass the gate Start applied to the process-level cwd.
	WorkspaceRoot string

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

	// AllowAlwaysApprove, when false, strips `--always-approve` /
	// `--auto-approve` (and equivalent `-c approval.mode=always|auto` /
	// `-c tools.always_approve=true` / `-c tools.auto_approve=true` config
	// overrides) from the spawn argv. Default false enforces the feature
	// brief's conservative approval posture — autonomous tool execution
	// must be an explicit per-workspace opt-in, not something a signed
	// `grok_acp_start` can flip via extra args. Sourced from
	// Config.EnableGrokAlwaysApprove.
	AllowAlwaysApprove bool

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
	var resolvedRoot string
	if opts.WorkspaceRoot != "" {
		resolvedRoot, err = filepath.EvalSymlinks(opts.WorkspaceRoot)
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

	// Fail-closed when a Grok requirements.toml layer pins auth or approval
	// policy in a direction the workspace's opt-in flags do not allow. xAI's
	// enterprise docs document requirements.toml as PINNED — its values
	// override later `-c|--config <key>=` flags rather than the other way
	// round — so the neutralizers buildGrokACPArgs emits would silently fail
	// open if the pinned layer already selected `model.api_key` or a
	// permissive `[permission] rules` table. Refusing the launch here, with
	// a message naming the pinned key and file, is safer than spawning a
	// session whose auth/approval posture we cannot honour.
	if err := detectPinnedGrokRequirements(opts.AllowAPIKeyFallback, opts.AllowAlwaysApprove); err != nil {
		return err
	}

	executable := resolveExecutable("grok")
	// PATH lookup miss is the common failure mode for macOS GUI/launchd
	// agents — Grok's installer drops the binary in ~/.grok/bin and only
	// touches shell rc, which the agent process never sources. Fall back to
	// the installer's default location before failing so a logged-in user
	// doesn't have to manually re-export PATH.
	if executable == "grok" {
		if p := resolveGrokInstallerBinary(); p != "" {
			executable = p
		}
	}
	args := buildGrokACPArgs(extraArgs, opts.AllowAPIKeyFallback, opts.AllowAlwaysApprove)

	fmt.Printf("%s[grok-acp] Starting session %s: %s %s%s\n",
		colorCyan, id, executable, strings.Join(redactGrokACPArgsForLog(args), " "), colorReset)

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
		return fmt.Errorf("failed to start grok agent stdio (is `grok` on PATH or in ~/.grok/bin? run `grok login` to authenticate): %w", err)
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
		ID:            id,
		Process:       proc,
		Stdin:         stdin,
		Stdout:        stdout,
		Stderr:        stderr,
		StartedAt:     time.Now(),
		WorkspaceID:   workspaceID,
		UID:           uid,
		TimeoutMs:     timeoutMs,
		WorkspaceRoot: resolvedRoot,
		status:        "running",
		done:          make(chan struct{}),
		streamDone:    make(chan struct{}),
	}

	m.sessions[id] = session

	if proc.Process != nil {
		globalProcessRegistry.Register(proc.Process.Pid, "grok-acp:"+id)
	}

	// Per-session deadline. Reserve the typed-error Seq BEFORE Kill() and
	// publish AFTER. The reservation ordering matters because Kill() races
	// with waitForExit: a fast exit can publish `grok_acp_ended` (which
	// increments session.seq) before this callback would otherwise allocate
	// its own Seq, letting the orchestrator order the terminal _ended frame
	// before the timeout error or drop the error as post-terminal. Taking
	// the AddInt64 first nails down a Seq strictly less than whatever
	// _ended ends up with, so even though both publishes are asynchronous
	// the orchestrator sees timeout → ended causal order. The publish is
	// still deferred until after Kill() because the production
	// newSessionPublishFn can block for the full Pub/Sub publish timeout
	// (~30s) when Pub/Sub is slow or unavailable; killing first guarantees
	// the orchestrator sees the child terminate on schedule and the
	// natural exit publishes `grok_acp_ended` via waitForExit even if this
	// diagnostic publish itself ultimately fails. Timer is Stop()'d in
	// waitForExit on natural exit so a freshly-exited session can't
	// double-fire the timeout publish.
	if session.TimeoutMs > 0 {
		session.timeoutTimer = time.AfterFunc(time.Duration(session.TimeoutMs)*time.Millisecond, func() {
			if session.Status() == "ended" {
				return
			}
			seq := atomic.AddInt64(&session.seq, 1)
			fmt.Printf("%s[grok-acp] Session %s timed out after %dms — killing%s\n",
				colorYellow, session.ID, session.TimeoutMs, colorReset)
			if session.Process != nil && session.Process.Process != nil {
				_ = session.Process.Process.Kill()
			}
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
	// ACP stdio frames are individual JSON-RPC 2.0 messages — a
	// request, notification or response, each a single JSON object per
	// line. Top-level arrays (JSON-RPC batch form) and scalars are out
	// of spec for ACP and must be rejected here rather than passed to
	// the child: validateGrokACPSendCwd's session-setup containment
	// gate only inspects object-shaped frames, so a batched
	// `[{"method":"session/new", "params":{"cwd":"/outside"}}, ...]`
	// would otherwise skip the cwd check and reach Grok unfiltered.
	if trimmed[0] != '{' {
		return fmt.Errorf("payload must be a single JSON-RPC object; batch arrays and scalar frames are not supported on ACP stdio")
	}

	// Re-enforce workspace containment on ACP session-setup frames
	// (`session/new` and `session/load`). Start already gated the
	// process-level cwd, but both setup verbs can carry their own
	// `params.cwd` that Grok will use as the session root — without this
	// check a later signed grok_acp_send (including one that resumes a
	// prior session) could point Grok at a path outside the configured
	// workspace and bypass the original Start gate. Skipped when the
	// session was launched without a containment root (mirrors Start's
	// behaviour) or when the frame omits `params.cwd`.
	if session.WorkspaceRoot != "" {
		if err := validateGrokACPSendCwd(trimmed, session.WorkspaceRoot); err != nil {
			return err
		}
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

			// Grok usage-limit telemetry: the ACP transport is the primary path
			// for normal Grok sessions (`grok_acp_start`), and xAI surfaces the
			// `usage_limit_reached` / `credit_limit_*` / `allow_access:false`
			// signals as `session/update` notifications on this same stdout
			// stream. The raw `session_start` path in session.go already calls
			// captureGrokUsageLimitLine; without mirroring it here, the CLI
			// Agents card stays Unknown for the primary Grok flow.
			captureGrokUsageLimitLine(trimmed, time.Now())

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
	// Reap the child via os.Process.Wait, NOT exec.Cmd.Wait. Per the
	// StdoutPipe docs, "it is incorrect to call Wait before all reads from
	// the pipe have completed" — exec.Cmd.Wait closes the parent ends of
	// StdoutPipe/StderrPipe the moment the child exits, which can truncate
	// the final JSON-RPC frame still buffered in the bufio.Scanner. When
	// grok writes a response and exits in quick succession, that final
	// frame is the one the orchestrator needs to complete the in-flight
	// ACP request; losing it leaves the request stuck. Splitting exit
	// detection (Process.Wait) from pipe cleanup (manual Close below,
	// gated on streamDone) preserves the final frame while keeping the
	// status-flip race fix intact.
	state, _ := session.Process.Process.Wait()

	// Flip status to "ended" and record exitCode BEFORE the stream-drain wait.
	// The deadline timer's AfterFunc gates its publish+Kill on
	// Status() == "ended"; if we postponed this flip until after drain
	// (which can be slow under back-pressure), a timer that fires while we
	// are draining would see status=="running", publish a spurious
	// grok_acp_error AND Kill an already-exited PID. Both are observable
	// upstream — the orchestrator would surface a phantom timeout error for
	// a session that exited normally. Order is fixed: status flip → timer
	// Stop → stream drain → pipe close. Stop() additionally elides a
	// not-yet-fired timer, but it cannot interrupt an in-flight callback,
	// which is why the status flip has to come first.
	session.mu.Lock()
	session.status = "ended"
	if state != nil {
		session.exitCode = state.ExitCode()
	} else {
		session.exitCode = -1
	}
	exit := session.exitCode
	session.mu.Unlock()

	if session.timeoutTimer != nil {
		session.timeoutTimer.Stop()
	}

	// Drain the stdout/stderr scanner goroutines BEFORE closing pipes. The
	// child's write ends are already closed (Process.Wait above only returns
	// post-exit), so the scanners hit EOF naturally once they catch up on
	// the OS pipe buffer — including the final JSON-RPC frame. If a scanner
	// is wedged, the drain timeout falls through to a force-close that
	// unblocks it; that path accepts the (rare) truncation because hanging
	// this goroutine forever is strictly worse.
	select {
	case <-session.streamDone:
	case <-time.After(grokACPStreamDrainTimeout):
		fmt.Printf("%s[grok-acp] Stream drain timed out for %s — forcing pipe close%s\n",
			colorYellow, session.ID, colorReset)
		session.Stdout.Close()
		session.Stderr.Close()
	}

	// Close the parent ends of every pipe. exec.Cmd.Wait would do this for
	// us; since we bypassed it above we have to mop up ourselves to avoid
	// leaking fds. closeStdin is idempotent (sync.Once), and Stdout/Stderr
	// Close after a prior Close is documented as returning ErrClosed
	// without side effects.
	session.closeStdin()
	session.Stdout.Close()
	session.Stderr.Close()

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
// `--no-auto-update` is always passed so a background update check can't
// race the ACP handshake. The xAI headless/scripting docs explicitly
// recommend this for automated ACP children; without it, any non-protocol
// stdout from the update worker would surface as a `grok_acp_error` and
// fail the in-flight initialize call. A caller-supplied `--no-auto-update`
// is deduped, and a caller-supplied `--auto-update` is stripped — we own
// this knob, the orchestrator doesn't.
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
//
// When allowAlwaysApprove is false the sanitiser additionally drops any
// caller-supplied `--always-approve` / `--auto-approve` flags, the
// `--permission-mode bypassPermissions` selector (xAI's enterprise-docs
// equivalent for skipping per-tool prompts), the `--allow <pattern>`
// permission-policy rules (xAI enterprise docs describe policy rules as
// evaluated BEFORE the per-tool prompt, so even with `--permission-mode
// default` pinned a single `--allow "Bash(*)"` would auto-approve matching
// tool calls), AND the equivalent `-c approval.mode=always|auto` /
// `-c tools.always_approve=true` / `-c tools.auto_approve=true` /
// `-c approval.permission_mode=bypassPermissions` /
// `-c permission_rules=…` / `-c policy.allow=…` config overrides. The
// feature brief makes approval behaviour conservative by default —
// autonomous tool execution has to be a per-workspace opt-in
// (Config.EnableGrokAlwaysApprove), not something a signed `grok_acp_start`
// can flip via extra args.
//
// When allowAlwaysApprove is false AND the sanitized extras don't already
// pin a `--permission-mode`, we additionally inject `--permission-mode default`
// onto the argv. The argv flag has higher precedence than `~/.grok/
// config.toml` (or `$GROK_HOME/config.toml`), so a host where the user has
// `[ui] permission_mode = "always-approve"` persisted cannot silently flip
// the spawned ACP child into auto-approval mode. Without this, the strip
// posture above would only cover the argv surface and leave the persistent
// config bypass open. We deliberately only inject when no caller-supplied
// `--permission-mode` survived sanitisation: by that point any bypass-valued
// caller arg has already been dropped, so a survivor is a conservative
// selector (`default`, `plan`, etc.) the orchestrator explicitly chose — we
// don't second-guess it. `default` is the xAI-documented CLI value for the
// prompt-on-every-tool mode (the `[ui] permission_mode = "ask"` config key
// has no matching CLI selector — xAI's enterprise docs enumerate `default`,
// `dontAsk`, `acceptEdits`, `bypassPermissions`, `plan`), so `default` is
// what we pin to keep the launch from failing argv validation.
func buildGrokACPArgs(extraArgs []string, allowAPIKey, allowAlwaysApprove bool) []string {
	args := []string{"agent", "stdio", "--no-auto-update"}
	sanitized := sanitizeGrokACPExtraArgs(extraArgs, allowAPIKey, allowAlwaysApprove)
	args = append(args, sanitized...)
	// Persistent-config neutralizers come AFTER the sanitized extras so that
	// later command-line flags win against earlier same-key values per Grok's
	// last-wins precedence — i.e. a caller-supplied `--config model.api_key=
	// xai-...` (which only survives sanitisation when allowAPIKey=true) does
	// not get clobbered by a default neutralizer. The sanitiser already
	// drops the POSIX `--` end-of-options delimiter from extras, so the
	// trailing flags here are guaranteed to be parsed as options rather than
	// silently demoted to positionals.
	if !allowAlwaysApprove {
		// Neutralize any auto-approval policy persisted in `~/.grok/config.toml`
		// (or `$GROK_HOME/config.toml`). The argv-level `--permission-mode default`
		// pin only covers the documented prompt-mode selector — xAI's enterprise
		// docs separately describe `[permission] rules` as a rule list evaluated
		// BEFORE the prompt gate, so a persisted `permission_rules = ["Bash(*)"]`
		// would still auto-approve matching tool calls on every ACP launch
		// despite the workspace not opting into `EnableGrokAlwaysApprove`. The
		// `--config key=` empty override is the documented per-process clear; we
		// MUST issue it by default and MUST omit it once the workspace has
		// opted in (otherwise the opt-in path's persisted policy would be
		// silently cleared). `permission_rules` / `permission.rules` are
		// conditionally cleared by the second slice below so deny-only host
		// policies survive untouched.
		args = append(args, grokPersistedAllowRuleNeutralizingConfigArgs()...)
		args = append(args, grokPolicyNeutralizingConfigArgs()...)
	}
	if !allowAPIKey {
		// Neutralize any API-key credential persisted in the user's
		// `~/.grok/config.toml` (or `$GROK_HOME/config.toml`). xAI's CLI treats
		// `model.api_key` / `model.env_key` as model-credential overrides that
		// take precedence over the active `grok login` session token, so just
		// stripping `XAI_API_KEY` from env and `--api-key` from argv is not
		// enough — a host where the user once ran `grok config set model.api_key`
		// would still silently bill the API-key account on every ACP launch.
		// The `--config key=` form (empty value) is the xAI override that
		// clears a config-file value for the duration of one process. We use
		// the long-form spelling deliberately: xAI's headless/scripting docs
		// list `-c` as the short alias for `--continue` (resume-session), and
		// only the enterprise-deployment docs spell out `-c|--config` as the
		// config-override surface. Pinning `--config` avoids any chance of
		// the neutralizer being mis-parsed as `--continue <session-id>` on a
		// host where the alias mapping went the other way. We also
		// neutralize the `xai.*` aliases the design doc enumerates so the
		// orchestrator's `cached_token` selection is the only auth surface left.
		args = append(args, grokAuthNeutralizingConfigArgs()...)
		// Per-model-scope neutralizers: xAI's enterprise docs document
		// persistent API-key credentials as model-scoped TOML sections
		// (`[model.<scope>] api_key = "..."`) that take precedence over the
		// active `cached_token` for that model. The static neutralizer slice
		// above clears the top-level and the documented `model.grok-build`
		// scope, but a caller-supplied `--model <other-scope>` would still
		// resolve to whatever credential `[model.<other-scope>]` holds in
		// `~/.grok/config.toml`. Scan the sanitised extras for `--model`
		// selectors and emit matching `--config model.<scope>.{api_key,env_key}=`
		// clears so the cached-token posture survives a non-default model
		// selection. Mirrors the argv-side gate in `isGrokAuthConfigKV`,
		// which already matches any `model.<scope>.{api_key,env_key}` shape
		// supplied via `-c|--config`.
		// Per-model-scope neutralizers also need to cover scopes the orchestrator
		// did NOT name on argv: xAI's enterprise docs document `[models] default
		// = "<scope>"` as the persisted active-model selector, so a host with
		// `[models] default = "custom"` + `[model.custom] api_key = "..."` would
		// resolve to `[model.custom]`'s API key on every ACP launch even though
		// no `--model` was passed. Read the persisted config to discover any
		// `[model.<scope>]` section with an api_key/env_key and the `[models]
		// default` scope, then emit clears for each. Filtered through
		// isSafeGrokModelScope and capped so a hostile/corrupted config can't
		// balloon argv.
		args = append(args, grokModelScopeAuthNeutralizingConfigArgs(
			mergeGrokModelScopes(
				extractGrokModelScopes(sanitized),
				persistedGrokModelScopesWithAPIKey(),
			),
		)...)
	}
	if !allowAlwaysApprove && !grokExtraArgsPinPermissionMode(sanitized) {
		args = append(args, "--permission-mode", "default")
	}
	return args
}

// grokAuthNeutralizingConfigArgs returns the `--config <key>=` overrides that
// empty out any API-key credential persisted in `~/.grok/config.toml` /
// `$GROK_HOME/config.toml`. Used by buildGrokACPArgs when
// Config.EnableGrokAPIKeyFallback is false to ensure the orchestrator's
// `cached_token` auth selection cannot be silently shadowed by a config-file
// API-key. Keys mirror isGrokAuthConfigKV's gated set so the strip-from-argv
// and override-config-file surfaces stay in lockstep.
//
// We deliberately spell the flag as `--config` rather than the `-c` short
// alias: xAI's headless/scripting docs use `-c` for `--continue`
// (resume-session) while only the enterprise-deployment docs document the
// `-c|--config` config-override surface. Using the long form removes the
// ambiguity so the neutralizer cannot be mis-parsed as a `--continue
// <session-id>` flag if a future Grok release narrows the short alias.
func grokAuthNeutralizingConfigArgs() []string {
	return []string{
		"--config", "model.api_key=",
		"--config", "model.env_key=",
		// Documented model-scoped form from xAI's enterprise docs (`[model.grok-build]
		// api_key = "..."`). Clearing the documented scope closes the practical
		// bypass; the argv-side gate (`isGrokAuthConfigKV` matches any
		// `model.<scope>.{api_key,env_key}`) blocks an orchestrator from
		// re-routing through a different scope name via `-c|--config`.
		"--config", "model.grok-build.api_key=",
		"--config", "model.grok-build.env_key=",
		"--config", "xai.api_key=",
		"--config", "xai.env_key=",
	}
}

// grokModelScopeAuthNeutralizingConfigArgs returns `--config
// model.<scope>.{api_key,env_key}=` clears for each caller-selected model
// scope. Used by buildGrokACPArgs when EnableGrokAPIKeyFallback is false so
// a `--model <scope>` that resolves to a `[model.<scope>] api_key = "..."`
// section in `~/.grok/config.toml` cannot silently reroute the launch off
// the cached-token posture — xAI documents per-model API-key credentials as
// taking precedence over the active session token, and the static slice in
// grokAuthNeutralizingConfigArgs only covers the top-level and documented
// `model.grok-build` scope. Scopes are filtered through isSafeGrokModelScope
// to keep the emitted `--config` arg shape sound; an unsafe-looking name
// is dropped here and the launch falls back to the static neutralizers.
func grokModelScopeAuthNeutralizingConfigArgs(scopes []string) []string {
	if len(scopes) == 0 {
		return nil
	}
	args := make([]string, 0, 4*len(scopes))
	for _, scope := range scopes {
		// `grok-build` is already in the static slice; skipping it here keeps
		// the emitted argv free of duplicate clears for the documented scope.
		if scope == "" || scope == "grok-build" {
			continue
		}
		if !isSafeGrokModelScope(scope) {
			continue
		}
		args = append(args,
			"--config", "model."+scope+".api_key=",
			"--config", "model."+scope+".env_key=",
		)
	}
	return args
}

// extractGrokModelScopes returns the unique model-scope names selected via
// `--model <name>` / `--model=<name>` flags in the sanitised argv. Both
// forms are recognised so the orchestrator can pass either style; values
// that fail isSafeGrokModelScope are skipped here rather than upstream so
// the safety filter and neutralizer emission stay in lockstep. Caller
// supplies args after sanitizeGrokACPExtraArgs has already stripped
// subcommands, `--`, and other tokens that would otherwise be mis-read as
// a model selector.
func extractGrokModelScopes(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	var scopes []string
	seen := map[string]bool{}
	keepNext := false
	for _, a := range args {
		if keepNext {
			keepNext = false
			if !seen[a] && isSafeGrokModelScope(a) {
				seen[a] = true
				scopes = append(scopes, a)
			}
			continue
		}
		lower := strings.ToLower(a)
		if lower == "--model" {
			keepNext = true
			continue
		}
		if strings.HasPrefix(lower, "--model=") {
			v := a[len("--model="):]
			if !seen[v] && isSafeGrokModelScope(v) {
				seen[v] = true
				scopes = append(scopes, v)
			}
		}
	}
	return scopes
}

// persistedGrokModelScopesWithAPIKey returns scope names from every Grok
// config layer that either appear as `[model.<scope>]` sections containing
// an `api_key` / `env_key` line, or are named as `[models] default =
// "<scope>"` (the persisted active-model selector documented in xAI's
// enterprise docs). Used by buildGrokACPArgs to ensure the cached-token
// posture is not silently bypassed by a host whose persisted default model
// resolves to a `[model.<scope>] api_key = "..."` section without any
// `--model` arg from the orchestrator. Missing / unreadable config files
// yield nil — best-effort by design.
//
// xAI's enterprise docs document a layered loader: the per-user
// `~/.grok/config.toml` (or `$GROK_HOME/config.toml`) is one source, but
// `[managed_]config.toml` and `requirements.toml` under `~/.grok` and
// `/etc/grok` are also consumed, and `model.api_key` / `model.env_key`
// values in any of those layers take precedence over the active session
// token. We scan every layer and merge the discovered scopes so a host
// whose managed layer selects a custom `[model.<scope>]` with credentials
// is neutralised the same as one whose user config does.
func persistedGrokModelScopesWithAPIKey() []string {
	var merged []string
	for _, p := range persistedGrokConfigPaths() {
		merged = mergeGrokModelScopes(merged, parsePersistedGrokModelScopesWithAPIKey(p))
	}
	return merged
}

// persistedGrokConfigPaths enumerates every config layer xAI's Grok loader
// is documented to consume, in precedence-agnostic order. The caller (the
// neutraliser path) only cares about the union of scopes-with-credentials
// across layers, not which layer wins, so order does not affect output —
// it only keeps the emitted `--config` args deterministic. Missing files
// are silently skipped downstream by parsePersistedGrokModelScopesWithAPIKey.
func persistedGrokConfigPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	userBase := firstNonEmpty(os.Getenv("GROK_HOME"), expandHome(home, ".grok"))
	systemBase := "/etc/grok"

	paths := make([]string, 0, 6)
	if userBase != "" {
		paths = append(paths,
			filepath.Join(userBase, "config.toml"),
			filepath.Join(userBase, "managed_config.toml"),
			filepath.Join(userBase, "requirements.toml"),
		)
	}
	paths = append(paths,
		filepath.Join(systemBase, "managed_config.toml"),
		filepath.Join(systemBase, "config.toml"),
		filepath.Join(systemBase, "requirements.toml"),
	)
	return paths
}

// grokRequirementsConfigPaths enumerates the Grok requirements.toml layers
// xAI's enterprise loader treats as PINNED — values in these files override
// later `-c|--config <key>=` flags rather than the other way round. The
// detectPinnedGrokRequirements gate uses this list (and not the broader
// persistedGrokConfigPaths set) because per-user/system config.toml and
// managed_config.toml CAN be neutralised by the existing `--config <key>=`
// emitter; only the requirements layer needs to fail-closed.
func grokRequirementsConfigPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	userBase := firstNonEmpty(os.Getenv("GROK_HOME"), expandHome(home, ".grok"))
	systemBase := "/etc/grok"

	paths := make([]string, 0, 2)
	if userBase != "" {
		paths = append(paths, filepath.Join(userBase, "requirements.toml"))
	}
	paths = append(paths, filepath.Join(systemBase, "requirements.toml"))
	return paths
}

// detectPinnedGrokRequirements scans every requirements.toml layer and
// returns an error when the host pins an auth credential the agent has not
// opted into (`allowAPIKey` false) or a permissive approval policy the
// agent has not opted into (`allowAlwaysApprove` false). Used by Start to
// fail-closed rather than spawning a session whose persisted posture
// silently bypasses the workspace's opt-in flags.
//
// Missing/unreadable files yield nil — best-effort by design. The first
// pinned key found determines the error so the operator gets actionable
// pointer (file + dotted key) rather than a generic refusal.
func detectPinnedGrokRequirements(allowAPIKey, allowAlwaysApprove bool) error {
	if allowAPIKey && allowAlwaysApprove {
		return nil
	}
	for _, p := range grokRequirementsConfigPaths() {
		if err := detectPinnedGrokRequirementsFile(p, allowAPIKey, allowAlwaysApprove); err != nil {
			return err
		}
	}
	if !allowAlwaysApprove {
		// Grok enterprise loader documents importing Claude Code's
		// `managed-settings.json` and evaluating its `permissions.allow`
		// rules BEFORE the per-tool prompt
		// (https://docs.x.ai/build/enterprise#permissions). The Grok
		// `-c permission_rules=` neutralizer above can only clear Grok's
		// own permission_rules — a Claude-imported allow rule survives
		// the per-process override, so fail closed when MDM has set one
		// and the workspace has not opted into EnableGrokAlwaysApprove.
		// Mirrors the requirements.toml pinned-policy path: the operator
		// either removes the imported allow rule or opts in explicitly.
		for _, p := range claudeManagedSettingsPaths() {
			if path, ok := detectClaudeManagedSettingsAllowRule(p); ok {
				return fmt.Errorf("grok imports Claude Code's managed-settings.json permission rules and %s contains a `permissions.allow` entry; the per-process --config neutralizer cannot override an imported Claude allow rule — set Config.EnableGrokAlwaysApprove=true to opt in, or remove the imported allow rule", path)
			}
		}
	}
	return nil
}

// claudeManagedSettingsPaths enumerates the Claude Code managed-settings.json
// locations xAI's Grok enterprise loader is documented to import permission
// rules from. Per-OS system paths only — user-scope ~/.claude/settings.json is
// intentionally NOT scanned because Grok's enterprise import is documented as
// the MDM-managed layer; treating ad-hoc user settings as pinned would
// over-fail-closed for the common single-user dev box.
func claudeManagedSettingsPaths() []string {
	paths := make([]string, 0, 2)
	switch runtime.GOOS {
	case "darwin":
		paths = append(paths, "/Library/Application Support/ClaudeCode/managed-settings.json")
	case "windows":
		programData := os.Getenv("ProgramData")
		if programData == "" {
			programData = `C:\ProgramData`
		}
		paths = append(paths, filepath.Join(programData, "ClaudeCode", "managed-settings.json"))
	default:
		paths = append(paths, "/etc/claude-code/managed-settings.json")
	}
	return paths
}

// detectClaudeManagedSettingsAllowRule reports whether the Claude
// managed-settings.json at `path` contains a non-empty `permissions.allow`
// array. Returns the path on hit so the error message can point the operator
// at the exact file. Missing/unreadable/malformed files yield false —
// best-effort by design, matching detectPinnedGrokRequirementsFile's
// tolerance for missing config layers.
func detectClaudeManagedSettingsAllowRule(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	const maxBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(f, maxBytes))
	if err != nil {
		return "", false
	}
	var parsed struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", false
	}
	for _, rule := range parsed.Permissions.Allow {
		if strings.TrimSpace(rule) != "" {
			return path, true
		}
	}
	return "", false
}

// detectPinnedGrokRequirementsFile is the file-path-injectable
// implementation of detectPinnedGrokRequirements. Reuses the same
// line-oriented TOML scanner shape as parsePersistedGrokModelScopesWithAPIKey
// (1 MiB read cap, regex-free section + key=value parse, scoped to the
// keys isGrokAuthConfigKV / isGrokApprovalConfigKV already enumerate so the
// pinned-detection surface stays in lockstep with the argv strip surface).
func detectPinnedGrokRequirementsFile(path string, allowAPIKey, allowAlwaysApprove bool) error {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	const maxBytes = 1 << 20
	reader := io.LimitReader(f, maxBytes)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 256*1024)

	currentSection := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if name, ok := parseTOMLSectionHeader(line); ok {
			currentSection = name
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		// Strip inline `# comment` from the value before quote-trimming so
		// pinned `always_approve = true # managed` reduces to `true` (the
		// gate's exact boolean match) instead of `true # managed` (which
		// silently misses and lets requirements.toml route past the
		// approval policy). For multi-line TOML arrays (the documented
		// `permission_rules = [ ... ]` form) we accumulate continuation
		// lines so a deny-only list isn't misread as the legacy bare-
		// pattern allow shortcut.
		val := strings.TrimSpace(stripTOMLInlineComment(line[eq+1:]))
		if bracketDepthOutsideStrings(val) > 0 {
			val = strings.TrimSpace(accumulateTOMLMultilineArray(scanner, val))
		}
		// Render as the dotted-path form the isGrok*ConfigKV gates
		// already understand so detection stays in lockstep with the
		// argv strip surface.
		dotted := key
		if currentSection != "" {
			dotted = currentSection + "." + key
		}
		kv := dotted + "=" + trimTOMLString(val)
		if !allowAPIKey && isGrokAuthConfigKV(kv) {
			return fmt.Errorf("grok requirements.toml pins API-key auth via %q in %s; the per-process --config neutralizer cannot override a requirements.toml pin — set Config.EnableGrokAPIKeyFallback=true to opt in, or remove the pinned credential", dotted, path)
		}
		if !allowAlwaysApprove && isGrokApprovalConfigKV(kv) {
			return fmt.Errorf("grok requirements.toml pins permissive approval policy via %q in %s; the per-process --config neutralizer cannot override a requirements.toml pin — set Config.EnableGrokAlwaysApprove=true to opt in, or remove the pinned policy", dotted, path)
		}
	}
	return nil
}

// parsePersistedGrokModelScopesWithAPIKey is the file-path-injectable
// implementation of persistedGrokModelScopesWithAPIKey. Kept separate so
// tests can point at a fixture without monkey-patching $GROK_HOME. The
// parser is intentionally line-oriented and regex-free: Grok's config.toml
// is human-edited TOML and a fully spec-compliant parser is overkill for
// the narrow goal of discovering scope-credential sections. We cap the
// read at 1 MiB and the scope count at 32 so a hostile/corrupted file
// can't balloon argv or stall the launch.
func parsePersistedGrokModelScopesWithAPIKey(path string) []string {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	scopes := make([]string, 0, 4)
	seen := map[string]bool{}
	addScope := func(name string) {
		if name == "" || name == "grok-build" {
			return
		}
		if !isSafeGrokModelScope(name) {
			return
		}
		if seen[name] {
			return
		}
		seen[name] = true
		scopes = append(scopes, name)
	}

	const maxBytes = 1 << 20
	reader := io.LimitReader(f, maxBytes)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 256*1024)

	currentSection := ""
	currentScope := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if name, ok := parseTOMLSectionHeader(line); ok {
			currentSection = name
			currentScope = ""
			if strings.HasPrefix(currentSection, "model.") {
				currentScope = currentSection[len("model."):]
			}
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		switch currentSection {
		case "models":
			if key == "default" {
				addScope(trimTOMLString(val))
			}
		case "":
			// Top-level dotted keys: a user who ran `grok config set
			// models.default custom` (or hand-edited the file without
			// section headers) lands with `currentSection == ""` and a
			// dotted `key`. Honor the same two shapes the [models] /
			// [model.<scope>] branches do so the scope discovery does not
			// silently miss a persisted per-model API key.
			if key == "models.default" {
				addScope(trimTOMLString(val))
				break
			}
			if strings.HasPrefix(key, "model.") {
				rest := key[len("model."):]
				dot := strings.LastIndexByte(rest, '.')
				if dot > 0 && (rest[dot+1:] == "api_key" || rest[dot+1:] == "env_key") {
					addScope(rest[:dot])
				}
			}
		default:
			if currentScope != "" && (key == "api_key" || key == "env_key") {
				addScope(currentScope)
			}
		}
		if len(scopes) >= 32 {
			break
		}
	}
	return scopes
}

// trimTOMLString unwraps a TOML scalar of the form `"..."` or `'...'`.
// Returns the input unchanged when no matching quotes are present so
// non-string scalars don't get truncated; isSafeGrokModelScope will reject
// anything that isn't a valid scope identifier downstream regardless.
func trimTOMLString(v string) string {
	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// stripTOMLInlineComment removes a trailing `# ...` comment from a TOML
// scalar value. TOML permits inline comments after a value on the same
// line, so a pinned `always_approve = true # managed` (or
// `[tools] always_approve = true  # managed`) would otherwise leave our
// line-oriented requirements/config scanners with the raw value
// `true # managed`, which neither the boolean nor string gate matches —
// silently routing past the approval policy.
//
// Quotes are tracked so a `#` inside a TOML basic ("...") or literal
// ('...') string is preserved. Backslash escapes inside basic strings are
// skipped so `\"` does not look like a closing quote. The caller is
// expected to re-trim trailing whitespace, but we also do it here for
// safety.
func stripTOMLInlineComment(v string) string {
	inDouble := false
	inSingle := false
	for i := 0; i < len(v); i++ {
		c := v[i]
		if inDouble {
			if c == '\\' && i+1 < len(v) {
				i++
				continue
			}
			if c == '"' {
				inDouble = false
			}
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
		case '\'':
			inSingle = true
		case '#':
			return strings.TrimRight(v[:i], " \t")
		}
	}
	return v
}

// parseTOMLSectionHeader recognises a line as a TOML section header,
// tolerating a trailing inline comment such as `[tools] # managed` that the
// TOML spec permits but a naive `HasSuffix(line, "]")` check rejects. Without
// this, a pinned `requirements.toml` whose table headers carry a managed-by
// comment would silently skip section tracking, and the following
// `api_key = ...` / `always_approve = true` would be evaluated as bare top-
// level keys that the dotted-form `isGrok*ConfigKV` gates never match —
// letting a pinned host bypass the API-key and approval opt-ins. Returns the
// trimmed section name and ok=true only when the closing `]` is followed by
// nothing but whitespace and an optional `# comment`.
func parseTOMLSectionHeader(line string) (string, bool) {
	if !strings.HasPrefix(line, "[") {
		return "", false
	}
	end := strings.IndexByte(line, ']')
	if end < 0 {
		return "", false
	}
	tail := strings.TrimSpace(stripTOMLInlineComment(line[end+1:]))
	if tail != "" {
		return "", false
	}
	return strings.TrimSpace(line[1:end]), true
}

// bracketDepthOutsideStrings counts the net `[` minus `]` characters that
// sit OUTSIDE TOML basic and literal strings. Used by
// accumulateTOMLMultilineArray to decide whether a `permission_rules = [`
// opening still needs more continuation lines before the value is
// classifiable. Quote tracking mirrors stripTOMLInlineComment so a
// `pattern = "Bash[*]"` literal doesn't confuse the depth count.
func bracketDepthOutsideStrings(s string) int {
	depth := 0
	inDouble := false
	inSingle := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inDouble {
			if c == '\\' && i+1 < len(s) {
				i++
				continue
			}
			if c == '"' {
				inDouble = false
			}
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
		case '\'':
			inSingle = true
		case '[':
			depth++
		case ']':
			depth--
		}
	}
	return depth
}

// accumulateTOMLMultilineArray reads continuation lines from the scanner
// while the running bracket depth (starting at the depth of `initial`) is
// still positive, joining them into a single logical value. xAI's
// enterprise docs document `permission_rules` as a TOML array, which is
// commonly hand-written across multiple lines:
//
//	permission_rules = [
//	  { action = "deny", pattern = "Bash(rm -rf*)" }
//	]
//
// Without accumulation the line scanner sees the value `[` on the first
// line, hits grokPermissionRulesValueHasAllowAction, finds no `action`
// substring, and misclassifies the deny-only list as a legacy bare-pattern
// allow shortcut. That wipes an MDM-style deny rule via the neutralizer
// AND rejects an otherwise stricter deny-only policy in the requirements
// gate. We bound the read at 256 lines so a corrupted file with no
// closing `]` can't stall the launch.
func accumulateTOMLMultilineArray(scanner *bufio.Scanner, initial string) string {
	depth := bracketDepthOutsideStrings(initial)
	if depth <= 0 {
		return initial
	}
	parts := make([]string, 0, 4)
	parts = append(parts, initial)
	const maxContinuationLines = 256
	for i := 0; i < maxContinuationLines && depth > 0 && scanner.Scan(); i++ {
		ln := stripTOMLInlineComment(scanner.Text())
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		depth += bracketDepthOutsideStrings(ln)
		parts = append(parts, ln)
	}
	return strings.Join(parts, " ")
}

// mergeGrokModelScopes returns the union of two scope slices, preserving
// the first slice's order and appending only new entries from the second.
// Used by buildGrokACPArgs to combine argv-derived scopes (the caller's
// `--model` selectors) with config-file-derived scopes (the persisted
// default model + any scope-with-credential discovered in config.toml)
// without emitting duplicate `--config model.<scope>.{api_key,env_key}=`
// clears.
func mergeGrokModelScopes(a, b []string) []string {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	out := make([]string, 0, len(a)+len(b))
	seen := map[string]bool{}
	for _, s := range a {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, s := range b {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// isSafeGrokModelScope reports whether `name` is a conservative model
// identifier safe to splice into a `--config model.<name>.api_key=`
// neutralizer. Allowed characters are [A-Za-z0-9_-]; periods would change
// the dotted-path the neutralizer targets, `=` would split the config arg
// into multiple kvs, and whitespace would corrupt the TOML key form.
// Unknown shapes fall back to the static top-level/`model.grok-build`/
// `xai.*` clears in grokAuthNeutralizingConfigArgs — fail-closed when in
// doubt.
func isSafeGrokModelScope(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// grokPolicyNeutralizingConfigArgs returns the `--config <key>=` overrides
// that empty out any auto-approval policy persisted in `~/.grok/config.toml`
// (or `$GROK_HOME/config.toml`). Used by buildGrokACPArgs when
// Config.EnableGrokAlwaysApprove is false to ensure the conservative
// per-tool prompt default cannot be silently shadowed by a config-file
// policy rule or approval-mode toggle.
//
// xAI's enterprise docs describe the `[permission] rules` TOML table as a
// permission-policy rule list that is evaluated BEFORE the per-tool prompt
// gate — so a single persisted `permission_rules = ["Bash(*)"]` would
// auto-approve matching tool calls even with `--permission-mode default`
// pinned on argv. The dotted-path neutralizers below clear the documented
// keys via the same `--config <key>=` empty-value override the auth
// neutralizer uses; keys mirror `isGrokApprovalConfigKV`'s gated set so the
// strip-from-argv and override-config-file surfaces stay in lockstep.
//
// `approval.mode=` / `approval.permission_mode=` cleared to empty falls back
// to Grok's documented default (per-tool prompt), so an explicit empty
// override is the right neutralizer for a persisted `always-approve` /
// `bypassPermissions` selector. `tools.always_approve=false` /
// `tools.auto_approve=false` pin the boolean toggles to the conservative
// value (an empty value is ambiguous for booleans). Long-form `--config`
// is used for the same `-c` vs `--continue` ambiguity reason
// `grokAuthNeutralizingConfigArgs` cites.
//
// `permission_rules` / `permission.rules` are intentionally NOT blanket-
// cleared here: xAI's enterprise docs treat the array as a heterogeneous
// rule list where `action = "deny"` entries tighten the policy (deny
// takes precedence) and `action = "allow"` entries loosen it. An
// unconditional `-c permission_rules=` clear would also wipe an MDM-set
// deny rule — degrading the host's security posture in pursuit of an
// allow-rule neutralizer. Persisted allow rules in these keys are instead
// caught conditionally by grokPersistedAllowRuleNeutralizingConfigArgs,
// which reads the documented config layers and emits the clear only when
// an allow rule is actually present; argv `-c permission_rules=…`
// injections of allow rules are still gated by isGrokApprovalConfigKV via
// the sanitiser.
func grokPolicyNeutralizingConfigArgs() []string {
	return []string{
		"--config", "policy.allow=",
		"--config", "permissions.allow=",
		"--config", "tools.allow=",
		"--config", "approval.mode=",
		"--config", "approval.permission_mode=",
		// `ui.permission_mode` is xAI's documented persisted-config key for
		// the same selector (the `[ui] permission_mode` TOML section). Without
		// the explicit clear, a host with `[ui] permission_mode =
		// "always-approve"` persisted would silently shadow the conservative
		// default — the argv `--permission-mode default` pin only covers the
		// flag surface, not this config-file key.
		"--config", "ui.permission_mode=",
		"--config", "tools.always_approve=false",
		"--config", "tools.auto_approve=false",
		// Legacy spellings xAI's Modes and Commands page still accepts.
		// `approval_mode` is the undotted variant of `approval.mode`, and
		// `yolo = true` desugars to the same always-approve posture as
		// `tools.always_approve = true`. Mirrors isGrokApprovalConfigKV's
		// gated set so the argv-strip and persisted-config-clear surfaces
		// stay symmetric — a persisted `~/.grok/config.toml` with either
		// key would otherwise route past the per-tool prompt despite the
		// workspace not opting into EnableGrokAlwaysApprove.
		"--config", "approval_mode=",
		"--config", "yolo=false",
	}
}

// grokPersistedAllowRuleNeutralizingConfigArgs returns the `--config
// permission_rules=` / `--config permission.rules=` empty-value overrides
// when (and only when) at least one documented Grok config layer contains
// a `permission_rules` / `permission.rules` entry with an explicit
// `action = "allow"` selector OR a legacy bare-pattern allow shortcut.
// Returns an empty slice when no allow rule is present so a deny-only
// policy survives untouched.
//
// Used by buildGrokACPArgs when Config.EnableGrokAlwaysApprove is false so
// the cached-token + per-tool-prompt posture can't be silently shadowed by
// a persisted `permission_rules = ["Bash(*)"]`, while an MDM-style
// `permission_rules = [{action = "deny", pattern = "Bash(rm -rf*)"}]` is
// left in place. Missing/unreadable files yield no clears — best-effort by
// design, mirroring persistedGrokModelScopesWithAPIKey's tolerance.
func grokPersistedAllowRuleNeutralizingConfigArgs() []string {
	for _, p := range persistedGrokConfigPaths() {
		if parsePersistedGrokPermissionRulesHasAllowAction(p) {
			return []string{
				"--config", "permission_rules=",
				"--config", "permission.rules=",
			}
		}
	}
	return nil
}

// parsePersistedGrokPermissionRulesHasAllowAction is the file-path-
// injectable implementation of grokPersistedAllowRuleNeutralizingConfigArgs'
// per-layer scan. Reuses the same line-oriented TOML scanner shape as
// parsePersistedGrokModelScopesWithAPIKey (1 MiB read cap, regex-free
// section + key=value parse). Recognises the inline forms
// `permission_rules = ["pattern"]` (legacy bare-pattern allow shortcut)
// and `permission_rules = [{action = "allow", ...}]` (table form with
// explicit allow action), plus the dotted `[permission] rules = …`
// section/key spelling.
func parsePersistedGrokPermissionRulesHasAllowAction(path string) bool {
	if path == "" {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	const maxBytes = 1 << 20
	reader := io.LimitReader(f, maxBytes)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 256*1024)

	currentSection := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if name, ok := parseTOMLSectionHeader(line); ok {
			currentSection = name
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		// Strip inline comments and accumulate multi-line array continuations
		// before classification so a deny-only `permission_rules = [\n  {action="deny",…}\n]`
		// is not misread as the legacy bare-pattern allow shortcut on the
		// first-line value `[`. Matching detectPinnedGrokRequirementsFile's
		// approach keeps the user-config neutralizer and the requirements
		// gate symmetric on multi-line TOML.
		val := strings.TrimSpace(stripTOMLInlineComment(line[eq+1:]))
		if bracketDepthOutsideStrings(val) > 0 {
			val = strings.TrimSpace(accumulateTOMLMultilineArray(scanner, val))
		}
		dotted := key
		if currentSection != "" {
			dotted = currentSection + "." + key
		}
		if (dotted == "permission_rules" || dotted == "permission.rules") &&
			grokPermissionRulesValueHasAllowAction(val) {
			return true
		}
	}
	return false
}

// grokExtraArgsPinPermissionMode reports whether any sanitized caller-
// supplied arg already pins the Grok permission mode (via `--permission-mode`
// or the `-c|--config approval.permission_mode=...` / `permission_mode=...`
// config-knob form). Used by buildGrokACPArgs to decide whether to inject
// the `--permission-mode default` argv override that defeats `~/.grok/config.toml`
// based bypasses — when the caller already pinned a mode, we don't stack a
// second one on top. Both the explicit-flag and `-c|--config` knob surfaces
// must be considered because the bypass-mode strip in sanitizeGrokACPExtraArgs
// only removes bypass VALUES; any non-bypass selector survives, and we treat
// a surviving selector as an explicit caller choice.
func grokExtraArgsPinPermissionMode(args []string) bool {
	for i, a := range args {
		lower := strings.ToLower(a)
		if isGrokPermissionModeArg(lower) {
			return true
		}
		if isGrokConfigOverrideArg(lower) {
			if eq := strings.IndexByte(lower, '='); eq >= 0 {
				if grokConfigKVTargetsPermissionMode(lower[eq+1:]) {
					return true
				}
				continue
			}
			if i+1 < len(args) && grokConfigKVTargetsPermissionMode(strings.ToLower(args[i+1])) {
				return true
			}
		}
	}
	return false
}

// grokConfigKVTargetsPermissionMode reports whether a `-c|--config` value
// targets the `permission_mode` key (top-level, namespaced under `approval.`,
// or namespaced under `ui.` — the xAI persisted-config form). Companion to
// grokExtraArgsPinPermissionMode — we only care that the key is being set,
// not what value it is set to (bypass values were already stripped upstream).
// Caller normalises to lower-case.
func grokConfigKVTargetsPermissionMode(kv string) bool {
	if kv == "" {
		return false
	}
	key := kv
	if eq := strings.IndexByte(kv, '='); eq >= 0 {
		key = kv[:eq]
	}
	key = strings.TrimSpace(key)
	return key == "approval.permission_mode" || key == "permission_mode" || key == "ui.permission_mode"
}

func sanitizeGrokACPExtraArgs(extraArgs []string, allowAPIKey, allowAlwaysApprove bool) []string {
	// Conservative valued-flag list — we don't currently know every flag
	// `grok` accepts, but covering the common config family lets callers
	// pass values whose token happens to look like a stripped subcommand
	// (`-c stdio=true` style) without us eating them. Keep in lockstep with
	// the codex manager's valuedFlags map.
	valuedFlags := map[string]bool{
		"-c": true, "--config": true,
		"--model":           true,
		"--permission-mode": true, "--permission_mode": true,
		// `--allow` is a permission-policy rule selector (e.g.
		// `--allow "Bash(*)"`). It always takes a value; admit the pair
		// speculatively here so the trailing stripGrokAllowRulePairs sweep
		// can drop both tokens when always-approve is opt-in.
		"--allow": true,
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
		case "--no-auto-update", "--auto-update":
			// buildGrokACPArgs always injects --no-auto-update; dedupe any
			// caller-supplied copy AND drop --auto-update so a caller can't
			// re-enable the background update worker that would race the
			// ACP handshake on stdout.
			continue
		case "--":
			// POSIX Utility Syntax Guideline 10 defines a standalone `--` as
			// the end-of-options delimiter — any subsequent tokens are treated
			// as operands rather than flags. buildGrokACPArgs appends the
			// auth/policy `--config <key>=` neutralizers and the
			// `--permission-mode default` pin AFTER the sanitised extras, so a
			// surviving `--` from caller args would silently demote those
			// policy-enforcing flags to positionals and re-open every gate
			// they're meant to close. ACP startup args have no documented use
			// for the delimiter (`agent stdio` already supplies the positional
			// subcommand and ACP frames travel via stdin, not argv), so the
			// fail-closed posture is to drop the token unconditionally.
			continue
		}
		// xAI's headless docs document `--cwd <PATH>` as setting Grok's
		// working directory, which would override the `proc.Dir` value Start
		// just validated against WorkspaceRoot. Drop the flag in both forms
		// (separate-value and `--cwd=...`) so a signed `grok_acp_start` can't
		// escape the symlink/containment checks via an extra-args side door —
		// the manager already pins the child to the validated cwd via proc.Dir.
		if lower == "--cwd" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(lower, "--cwd=") {
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
		if !allowAlwaysApprove && isGrokAlwaysApproveArg(lower) {
			// `--always-approve` / `--auto-approve` are boolean flags in
			// every form xAI has documented — there's no separate-value to
			// skip. Equals-form (`--always-approve=true`) is dropped wholesale
			// because re-admitting `--always-approve=false` here would let a
			// caller toggle the value back on via subsequent flag ordering.
			continue
		}
		if !allowAlwaysApprove && isGrokPermissionModeArg(lower) {
			// `--permission-mode bypassPermissions` is xAI's documented escape
			// hatch for the per-tool prompt gate (enterprise docs, ACP
			// scripting mode). Inline equals-form (`--permission-mode=
			// bypassPermissions`) is dropped here once both halves are
			// visible in a single token; non-bypass values like `ask` fall
			// through unchanged. Separate-value form (`--permission-mode
			// bypassPermissions`) is admitted speculatively via the
			// valuedFlags branch below and the trailing
			// stripGrokPermissionModePairs sweep drops the pair only when
			// the value resolves to a bypass selector.
			if eq := strings.IndexByte(a, '='); eq >= 0 {
				if isGrokPermissionModeBypassValue(a[eq+1:]) {
					continue
				}
			}
		}
		if !allowAlwaysApprove && isGrokAllowRuleArg(lower) {
			// `--allow <pattern>` adds a permission-policy rule (xAI
			// enterprise docs). Rules are evaluated BEFORE the per-tool
			// prompt, so a single `--allow "Bash(*)"` would auto-approve
			// matching tool calls even with `--permission-mode default`
			// pinned. Unlike `--permission-mode`, EVERY value to `--allow`
			// is autonomous-execution-shaped, so we don't second-guess the
			// value here — drop inline equals-form wholesale and let the
			// trailing stripGrokAllowRulePairs sweep finish off the
			// separate-value pair admitted via the valuedFlags branch.
			if strings.HasPrefix(lower, "--allow=") {
				continue
			}
		}
		// Inline -c/--config form: `--config=auth.method=xai.api_key` or
		// `--config=approval.mode=always`. Without inspection these would
		// survive the explicit-flag strip and let the orchestrator escape the
		// default opt-in gates.
		if isGrokConfigOverrideArg(lower) {
			if eq := strings.IndexByte(a, '='); eq >= 0 {
				kv := a[eq+1:]
				if !allowAPIKey && isGrokAuthConfigKV(kv) {
					continue
				}
				if !allowAlwaysApprove && isGrokApprovalConfigKV(kv) {
					continue
				}
			}
		}
		if valuedFlags[lower] {
			// Separate-value -c/--config form: peek the value and drop the
			// pair when it touches a gated config key. Same fail-closed
			// posture as the inline form above; the trailing sweep below
			// finishes the job once both tokens are visible.
			if (!allowAPIKey || !allowAlwaysApprove) && isGrokConfigOverrideArg(lower) {
				// Defer the decision to the next iteration via a closure
				// over the next token: we cannot index forward here without
				// duplicating the loop's skip/keep bookkeeping, so flag the
				// pair via a dedicated keepNext sibling.
				keepNext = true
				cleaned = append(cleaned, a)
				// Special-case: if the very next token would be a gated
				// config kv, undo both appends. Implemented by scanning
				// ahead inline rather than introducing a third state flag.
				continue
			}
			keepNext = true
		}
		cleaned = append(cleaned, a)
	}
	// Second pass: drop any `-c|--config <gated-kv>` pair that the loop above
	// admitted because the kv decision needed both tokens. Keeping this as a
	// trailing sweep avoids growing the loop's state machine and keeps the
	// happy path (no config args) zero-cost.
	if !allowAPIKey {
		cleaned = stripGrokAuthConfigPairs(cleaned)
	}
	if !allowAlwaysApprove {
		cleaned = stripGrokApprovalConfigPairs(cleaned)
		cleaned = stripGrokPermissionModePairs(cleaned)
		cleaned = stripGrokAllowRulePairs(cleaned)
	}
	return cleaned
}

// isGrokConfigOverrideArg reports whether `lower` is the `-c` / `--config`
// flag (in either bare or equals form). Case-insensitive; callers normalise
// via strings.ToLower first.
func isGrokConfigOverrideArg(lower string) bool {
	return lower == "-c" || lower == "--config" ||
		strings.HasPrefix(lower, "-c=") || strings.HasPrefix(lower, "--config=")
}

// isGrokAuthConfigKV reports whether a `-c`/`--config` value would let the
// caller switch Grok off the default cached-token flow when API-key fallback
// is opt-in. Three cases trigger the gate:
//
//   - The key references an API-key credential at the top-level — `model.api_key`,
//     `model.env_key`, `xai.api_key`, `xai.env_key` — i.e. supplying the key
//     itself or pointing at the env var that holds it.
//   - The key references an API-key credential under a model-scoped section —
//     xAI documents persistent API-key config as `[model.grok-build] api_key =
//     "..."` (enterprise docs), which in dotted-path form is
//     `model.<scope>.api_key` / `model.<scope>.env_key` for any scope name.
//     The model-scoped form takes precedence over the active cached-token, so
//     a `--config model.grok-build.api_key=xai-...` (or any other scope) must
//     be gated the same way as the top-level form.
//   - The auth-method selector picks the API-key path —
//     `auth.method=xai.api_key` or `auth=xai.api_key`. `auth.method=cached_token`
//     is the default we want to keep working, so we only strip values that
//     actually escape to api-key auth.
func isGrokAuthConfigKV(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	if lower == "" {
		return false
	}
	key := lower
	val := ""
	if eq := strings.IndexByte(lower, '='); eq >= 0 {
		key = lower[:eq]
		val = lower[eq+1:]
	}
	// TOML accepts whitespace around the `=`, e.g. `model.api_key = "xai-..."`.
	// Strip it after the split so `key` and `val` match the bare dotted-path
	// and value compared below — otherwise a `--config 'model.api_key = ...'`
	// override would survive the gate despite EnableGrokAPIKeyFallback being
	// false.
	key = strings.TrimSpace(key)
	val = strings.TrimSpace(val)
	apiKeyKeys := []string{
		"model.api_key", "model.env_key",
		"xai.api_key", "xai.env_key",
	}
	for _, k := range apiKeyKeys {
		if key == k {
			return true
		}
	}
	// Model-scoped form (`model.<scope>.api_key` / `model.<scope>.env_key`).
	// xAI's enterprise docs document persistent API-key config as a model-
	// scoped TOML section like `[model.grok-build] api_key = "..."`, which is
	// rendered as a `model.<scope>.api_key` dotted-path in `-c`/`--config`
	// args. We can't enumerate every model scope a host might have configured
	// (or that xAI might ship in future releases), so any `model.<anything>
	// .{api_key,env_key}` shape is treated as the same credential class as the
	// top-level form. The first segment must be `model` and the last segment
	// must be one of the credential keys — that lets `model.<scope>.foo` flow
	// through if it ever maps to a non-credential field.
	if strings.HasPrefix(key, "model.") {
		if last := strings.LastIndexByte(key, '.'); last > len("model.")-1 && last < len(key)-1 {
			tail := key[last+1:]
			if tail == "api_key" || tail == "env_key" {
				return true
			}
		}
	}
	// Auth-method selector escaping to api-key flow. We deliberately do NOT
	// strip the default `cached_token` selection — it's the path the feature
	// brief mandates and existing extra-args tests rely on.
	if (key == "auth.method" || key == "auth") && val != "" {
		if strings.Contains(val, "api_key") {
			return true
		}
	}
	return false
}

// stripGrokAuthConfigPairs removes `-c|--config <auth-kv>` pairs (separate-
// value form) that survived sanitizeGrokACPExtraArgs' main loop. The loop
// admits the pair speculatively because the kv decision needs both tokens;
// this sweep drops it when the value targets an auth config key.
func stripGrokAuthConfigPairs(in []string) []string {
	out := make([]string, 0, len(in))
	i := 0
	for i < len(in) {
		lower := strings.ToLower(in[i])
		if (lower == "-c" || lower == "--config") && i+1 < len(in) {
			if isGrokAuthConfigKV(in[i+1]) {
				i += 2
				continue
			}
		}
		out = append(out, in[i])
		i++
	}
	return out
}

// isGrokAllowRuleArg reports whether `lower` is the `--allow` policy-rule
// flag (bare or equals form). xAI's enterprise docs describe `--allow` as a
// permission-policy rule (e.g. `--allow "Bash(*)"`) whose rules are evaluated
// BEFORE the per-tool prompt — so a single `--allow` survival is enough to
// auto-approve matching tool calls even when `--permission-mode default` is
// still pinned. Match is case-insensitive; callers normalise via
// strings.ToLower first. `--deny` is intentionally NOT recognised here —
// deny rules tighten the policy and are safe to admit on the conservative
// default path.
func isGrokAllowRuleArg(lower string) bool {
	return lower == "--allow" || strings.HasPrefix(lower, "--allow=")
}

// stripGrokAllowRulePairs removes `--allow <pattern>` pairs (separate-value
// form) that survived sanitizeGrokACPExtraArgs' main loop. The loop admits
// the pair speculatively via the valuedFlags branch because the strip
// decision needs both tokens; this sweep drops the pair when
// Config.EnableGrokAlwaysApprove is false. Mirrors stripGrokPermissionModePairs
// — same speculative-admit / trailing-sweep pattern.
func stripGrokAllowRulePairs(in []string) []string {
	out := make([]string, 0, len(in))
	i := 0
	for i < len(in) {
		lower := strings.ToLower(in[i])
		if lower == "--allow" && i+1 < len(in) {
			i += 2
			continue
		}
		out = append(out, in[i])
		i++
	}
	return out
}

// isGrokAlwaysApproveArg reports whether a caller-supplied arg would let
// Grok skip per-tool permission prompts. xAI documents `--always-approve`
// as the canonical autonomous-execution flag; `--auto-approve` is the
// equivalent name used by the design doc and other CLI agents. Each known
// flag is enumerated explicitly — a broader prefix match would silently
// strip flags we don't know about (e.g. a hypothetical `--always-approve-
// for-readonly`) and risk breaking unrelated args future Grok releases
// might ship. Match is case-insensitive; callers normalise via
// strings.ToLower first.
func isGrokAlwaysApproveArg(lower string) bool {
	approveFlags := []string{
		"--always-approve",
		"--auto-approve",
	}
	for _, f := range approveFlags {
		if lower == f || strings.HasPrefix(lower, f+"=") {
			return true
		}
	}
	return false
}

// isGrokApprovalConfigKV reports whether a `-c`/`--config` value would let
// the caller switch Grok off the per-tool prompt flow when always-approve is
// opt-in. Three cases trigger the gate:
//
//   - `approval.mode=always|auto` (or `approval=always|auto`) — flips the
//     top-level approval selector to autonomous execution.
//   - `tools.always_approve=true` / `tools.auto_approve=true` — the boolean
//     toggle that the documented flag desugars to.
//
// `approval.mode=ask` (or any non-always selector) is left intact so callers
// can still re-pin the conservative default explicitly.
func isGrokApprovalConfigKV(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	if lower == "" {
		return false
	}
	key := lower
	val := ""
	if eq := strings.IndexByte(lower, '='); eq >= 0 {
		key = lower[:eq]
		val = lower[eq+1:]
	}
	// Mirror isGrokAuthConfigKV: trim whitespace around the `=` so a
	// `--config 'approval_mode = always-approve'` (TOML-style spacing) is
	// classified the same as the bare `approval_mode=always-approve` form.
	key = strings.TrimSpace(key)
	val = strings.TrimSpace(val)
	// TOML accepts string values wrapped in `"…"` or `'…'`, and Grok's `-c`
	// form preserves the quotes verbatim when xAI's docs spell the value as a
	// quoted string (e.g. `--config permission_mode="bypassPermissions"`).
	// Strip a single matched pair of surrounding quotes before the value
	// comparisons below so a quoted bypass selector cannot survive
	// sanitization just because the key=val text retained its TOML quoting.
	val = trimGrokTOMLStringQuotes(val)
	if (key == "tools.always_approve" || key == "tools.auto_approve") && (val == "true" || val == "1" || val == "yes" || val == "on") {
		return true
	}
	// xAI's Modes and Commands page documents a `yolo = true` legacy
	// shortcut that desugars to the same `always-approve` posture as
	// `tools.always_approve = true`. Without this branch, a host with
	// `/etc/grok/requirements.toml` pinning `yolo = true` would route past
	// the approval gate (detectPinnedGrokRequirementsFile would not flag
	// the line, and the per-process `--config yolo=false` neutralizer in
	// grokPolicyNeutralizingConfigArgs would not be emitted) despite
	// EnableGrokAlwaysApprove being false.
	if key == "yolo" && (val == "true" || val == "1" || val == "yes" || val == "on") {
		return true
	}
	// `approval_mode` is the legacy spelling of `approval.mode` xAI keeps
	// accepting for backward compat. Same gated value set so a persisted
	// `approval_mode = "always-approve"` cannot silently shadow the
	// per-tool prompt selector.
	if (key == "approval.mode" || key == "approval" || key == "approval_mode") && val != "" {
		if val == "always" || val == "auto" || val == "auto-approve" || val == "always-approve" {
			return true
		}
	}
	// Config-form of `--permission-mode bypassPermissions` (the xAI enterprise
	// docs name `bypassPermissions` explicitly; common variants share intent).
	// `approval.permission_mode=ask` is the conservative default and is
	// deliberately left intact. `ui.permission_mode` is xAI's documented
	// persisted-config key for the same selector (the `[ui] permission_mode`
	// TOML section), so we gate it on the same bypass-value set — otherwise a
	// `-c ui.permission_mode=always-approve` would route around the
	// `approval.permission_mode` / `permission_mode` gate and silently flip
	// the spawned ACP child into auto-approval despite the workspace not
	// opting into `EnableGrokAlwaysApprove`.
	if (key == "approval.permission_mode" || key == "permission_mode" || key == "ui.permission_mode") && isGrokPermissionModeBypassValue(val) {
		return true
	}
	// Config-form of `--allow <rule>` — xAI's enterprise docs describe the
	// permission_rules TOML array (and its dotted `permission.rules` cousin)
	// as a rule list `--allow` appends to. The list is heterogeneous though:
	// xAI documents `action = "deny"` rules as policy-tightening (deny takes
	// precedence) and `action = "allow"` rules as policy-loosening — only the
	// latter routes around the per-tool prompt. Differentiate via
	// grokPermissionRulesValueHasAllowAction so a deny-only rule (e.g. an MDM
	// policy denying dangerous Bash patterns) is left intact on the
	// conservative default path. The `policy.allow` / `permissions.allow` /
	// `tools.allow` cousins are explicit allow lists by name — any non-empty
	// value is by definition an allow rule and gates unconditionally.
	switch key {
	case "permission_rules", "permission.rules":
		if grokPermissionRulesValueHasAllowAction(val) {
			return true
		}
	case "policy.allow", "permissions.allow", "tools.allow":
		if val != "" {
			return true
		}
	}
	return false
}

// grokPermissionRulesValueHasAllowAction reports whether a serialised
// `permission_rules` / `permission.rules` TOML value contains at least one
// allow rule that would route around the per-tool prompt gate. Returns
// false for empty values, empty arrays, and deny-only table forms — xAI's
// enterprise docs treat `action = "deny"` rules as policy tightening
// (deny takes precedence), so they are safe to preserve even when the
// workspace has not opted into always-approve.
//
// The check is intentionally a substring scan over the lower-cased,
// whitespace-stripped value rather than a full TOML parse: callers feed
// us either an argv `-c key=value` string or a single TOML line from the
// line-oriented requirements.toml scanner, and both can be answered
// without reconstructing the full TOML AST. The legacy
// `permission_rules = ["Bash(*)"]` form (string-only patterns, no
// `action` field) is treated as allow because xAI documents bare string
// patterns as allow shortcuts.
func grokPermissionRulesValueHasAllowAction(value string) bool {
	v := strings.TrimSpace(value)
	if v == "" {
		return false
	}
	// Empty TOML arrays in either bracket form are not allow rules.
	if v == "[]" || v == "[ ]" {
		return false
	}
	lower := strings.ToLower(v)
	// Detect an `action = ...` KEY outside any quoted-string pattern. The
	// prior heuristic looked for the substring `action` anywhere, which
	// misclassifies a legacy bare-pattern array like `["Bash(*action*)"]`
	// as table form and then — finding no `action="allow"` — returns false
	// (i.e. treats it as safe). A bare-pattern array MUST be treated as an
	// allow shortcut. So: walk the raw (case-folded) value, track whether
	// we are inside `"..."` or `'...'`, and look for the literal `action`
	// token followed (after optional whitespace) by `=`. If no such key
	// exists at TOML level, the value is bare-pattern shorthand → allow.
	if !lowerHasActionKey(lower) {
		return true
	}
	// Table form with explicit action= entries. Flag only when at least one
	// action is the documented allow selector. Strip whitespace so
	// `action = "allow"`, `action="allow"`, and `action  =  "allow"` all
	// match the same compact form. Tabs are also stripped so the scanner can
	// answer for TOML hand-formatted with tab indentation.
	compact := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, lower)
	return strings.Contains(compact, `action="allow"`) ||
		strings.Contains(compact, "action='allow'") ||
		strings.Contains(compact, "action=allow,") ||
		strings.Contains(compact, "action=allow}") ||
		strings.HasSuffix(compact, "action=allow")
}

// lowerHasActionKey reports whether `lower` (already case-folded) contains
// an `action` TOML key — i.e. the literal token `action` appearing outside
// any single- or double-quoted string and followed (after optional
// whitespace/tabs) by `=`. This distinguishes a real table form like
// `{action = "allow", ...}` from a bare pattern such as
// `["Bash(*action*)"]` where the word `action` is only part of a quoted
// pattern. TOML basic strings (double-quoted) DO support escape sequences
// like `\"` and `\\`, so a naive paired-quote toggle would close the string
// early on `"x\" action = deny"` and then mistake the trailing `action =`
// for a real TOML key — letting a legacy bare-pattern allow rule slip past
// the gate. Inside double-quoted strings we honour the TOML escape rule and
// skip the byte after a backslash. TOML literal strings (single-quoted) do
// NOT process escapes, so the single-quote branch stays a simple toggle.
func lowerHasActionKey(lower string) bool {
	inDouble := false
	inSingle := false
	for i := 0; i < len(lower); i++ {
		c := lower[i]
		if inDouble {
			if c == '\\' && i+1 < len(lower) {
				i++
				continue
			}
			if c == '"' {
				inDouble = false
			}
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
			continue
		case '\'':
			inSingle = true
			continue
		}
		if c != 'a' || i+6 > len(lower) || lower[i:i+6] != "action" {
			continue
		}
		// Token boundary on the left: the byte before `action` must not be a
		// continuation of an identifier — otherwise we'd match `reaction`.
		if i > 0 {
			p := lower[i-1]
			if (p >= 'a' && p <= 'z') || (p >= '0' && p <= '9') || p == '_' || p == '-' || p == '.' {
				continue
			}
		}
		j := i + 6
		for j < len(lower) && (lower[j] == ' ' || lower[j] == '\t') {
			j++
		}
		if j < len(lower) && lower[j] == '=' {
			return true
		}
	}
	return false
}

// isGrokPermissionModeArg reports whether `lower` is the `--permission-mode`
// flag (bare or equals form). xAI's enterprise docs surface this flag as a
// permission gate selector that, when set to `bypassPermissions`, disables
// per-tool prompts — i.e. the same intent as `--always-approve` but routed
// through a different surface. Recognised here so the always-approve gate
// can fail closed on it; benign selectors like `default` or `plan` are left
// intact.
func isGrokPermissionModeArg(lower string) bool {
	return lower == "--permission-mode" || lower == "--permission_mode" ||
		strings.HasPrefix(lower, "--permission-mode=") || strings.HasPrefix(lower, "--permission_mode=")
}

// trimGrokTOMLStringQuotes strips a single matched pair of surrounding
// TOML string quotes (`"…"` or `'…'`) from `s`. Used so a quoted bypass
// selector like `permission_mode="bypassPermissions"` — which Grok's
// `-c|--config` form preserves verbatim — is normalised to the bare
// `bypassPermissions` token the sanitization gates compare against.
// Mismatched / unbalanced quotes are returned unchanged so a value with a
// stray quote does not silently lose a character.
func trimGrokTOMLStringQuotes(s string) string {
	if len(s) >= 2 {
		first, last := s[0], s[len(s)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// isGrokPermissionModeBypassValue reports whether a `--permission-mode` value
// would let the caller bypass per-tool permission prompts. `bypassPermissions`
// is the canonical name from xAI's enterprise docs; the bare `bypass`,
// `auto*`, and `always*` synonyms share the same intent and are gated to
// fail closed too. `acceptEdits` is also gated because xAI's enterprise docs
// describe it as auto-approving file edits without per-tool prompts —
// strictly narrower than full `bypassPermissions` but still an auto-approval
// surface that must stay behind Config.EnableGrokAlwaysApprove. Case- and
// separator-insensitive.
func isGrokPermissionModeBypassValue(value string) bool {
	v := strings.ToLower(trimGrokTOMLStringQuotes(strings.TrimSpace(value)))
	switch v {
	case "bypasspermissions", "bypass-permissions", "bypass_permissions", "bypass",
		"auto", "auto-approve", "auto_approve",
		"always", "always-approve", "always_approve",
		"acceptedits", "accept-edits", "accept_edits":
		return true
	}
	return false
}

// stripGrokPermissionModePairs removes `--permission-mode <bypass-value>`
// pairs (separate-value form) that survived sanitizeGrokACPExtraArgs' main
// loop. The loop admits the pair speculatively via the valuedFlags branch
// because the bypass decision needs both tokens; this sweep drops it only
// when the value resolves to a bypass selector. Non-bypass values such as
// `default` or `plan` flow through unchanged so callers can still pin a
// conservative selector explicitly.
func stripGrokPermissionModePairs(in []string) []string {
	out := make([]string, 0, len(in))
	i := 0
	for i < len(in) {
		lower := strings.ToLower(in[i])
		if (lower == "--permission-mode" || lower == "--permission_mode") && i+1 < len(in) {
			if isGrokPermissionModeBypassValue(in[i+1]) {
				i += 2
				continue
			}
		}
		out = append(out, in[i])
		i++
	}
	return out
}

// stripGrokApprovalConfigPairs removes `-c|--config <approval-kv>` pairs
// (separate-value form) that survived sanitizeGrokACPExtraArgs' main loop.
// Mirrors stripGrokAuthConfigPairs — same speculative-admit / trailing-sweep
// pattern, just gated on the approval kv set instead of the auth kv set.
func stripGrokApprovalConfigPairs(in []string) []string {
	out := make([]string, 0, len(in))
	i := 0
	for i < len(in) {
		lower := strings.ToLower(in[i])
		if (lower == "-c" || lower == "--config") && i+1 < len(in) {
			if isGrokApprovalConfigKV(in[i+1]) {
				i += 2
				continue
			}
		}
		out = append(out, in[i])
		i++
	}
	return out
}

// redactGrokACPArgsForLog masks credential-bearing values before the startup
// banner is printed. Necessary because when EnableGrokAPIKeyFallback is true,
// buildGrokACPArgs deliberately preserves `--api-key{,-env}` / `--auth{,-method}`
// verbatim — without this the next-arg value (e.g. `xai-...`) would leak into
// stdout/log files unlike the normal shell-command path which routes through
// redactCommandForLog. Equals-form values are masked inline; separate-value
// form masks the following token. Output is passed through redactArgs so any
// other secret pattern (bearer tokens, AWS keys, etc.) the per-arg regex
// recognises in caller-supplied extra args is also caught.
func redactGrokACPArgsForLog(args []string) []string {
	out := make([]string, len(args))
	maskNext := false
	for i, a := range args {
		if maskNext {
			out[i] = "[REDACTED]"
			maskNext = false
			continue
		}
		lower := strings.ToLower(a)
		if isGrokAuthOverrideArg(lower) {
			if eq := strings.IndexByte(a, '='); eq >= 0 {
				out[i] = a[:eq+1] + "[REDACTED]"
			} else {
				out[i] = a
				maskNext = true
			}
			continue
		}
		out[i] = a
	}
	return redactArgs(out)
}

// validateGrokACPSendCwd inspects a JSON-RPC frame and, if it is an ACP
// session-setup request (`session/new` or `session/load`), requires its
// `params.cwd` to resolve inside root. Mirrors the containment check Start
// applies to the process-level cwd so the in-protocol session cwd cannot
// escape it.
//
// Both methods are covered because ACP exposes `session/load` as the
// session-setup alternative to `session/new` for resumed sessions, and Grok
// ACP clients pass `cwd` on loads too — gating only `session/new` would let
// a later signed grok_acp_send that resumes a session point Grok at a
// directory outside the workspace root.
//
// Frames whose method is neither setup verb, or that omit `params.cwd`, are
// accepted unchanged — ACP carries many other request shapes whose params we
// must not interpret. Errors are returned only when we are certain we have a
// setup frame with a cwd that fails containment; transient parse hiccups
// fall through to acceptance because Send has already established the frame
// is valid top-level JSON.
func validateGrokACPSendCwd(frame, resolvedRoot string) error {
	var probe struct {
		Method string `json:"method"`
		Params struct {
			Cwd string `json:"cwd"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(frame), &probe); err != nil {
		return nil
	}
	if !isGrokACPSessionSetupMethod(probe.Method) || probe.Params.Cwd == "" {
		return nil
	}
	cwd := probe.Params.Cwd
	if !filepath.IsAbs(cwd) {
		return fmt.Errorf("%s params.cwd must be an absolute path; got %q", probe.Method, cwd)
	}
	resolved, err := resolveCwdForContainment(cwd)
	if err != nil {
		return fmt.Errorf("%s params.cwd %q could not be safely resolved: %w", probe.Method, cwd, err)
	}
	if !pathInsideRoot(resolved, resolvedRoot) {
		return fmt.Errorf("%s params.cwd %q is outside the configured workspace root %q", probe.Method, resolved, resolvedRoot)
	}
	return nil
}

// isGrokACPSessionSetupMethod reports whether method is one of the ACP
// session-setup verbs whose `params.cwd` (when present) anchors the session
// to a workspace path and therefore must be containment-checked.
func isGrokACPSessionSetupMethod(method string) bool {
	return method == "session/new" || method == "session/load"
}

// resolveCwdForContainment resolves cwd through any symlinks so a later
// containment check sees the OS's view, not the caller's lexical view.
//
// The honest case: cwd exists, EvalSymlinks succeeds, we return the
// resolved path.
//
// The attack case: cwd is something like `$root/link/../new` where `link`
// is a symlink under the workspace pointing at `/outside` and `new` does
// not exist yet. EvalSymlinks fails on the whole path because of the
// missing tail. We must NOT lexically Clean the input first — Clean would
// collapse `link/..` to nothing, hiding a symlink whose OS-resolved
// target (`/outside`) is the parent that `..` actually pops from. The
// previous walk-up-from-cleaned-input approach had this exact bug: it
// accepted `$root/link/../new` as `$root/new`.
//
// Instead, walk the path FORWARD from the volume root, applying one
// component at a time:
//
//   - `..` pops one component off the OS-resolved prefix (matching how
//     the kernel evaluates the path after symlink resolution).
//   - any other name is appended and re-resolved via EvalSymlinks so a
//     symlink on the existing portion takes effect before a subsequent
//     `..` is applied.
//
// Once we hit the first component that can't be resolved (because it
// doesn't exist yet), everything after it is necessarily fictional —
// there are no more symlinks to follow on the unreachable suffix — so we
// can lexically Join the remainder over the OS-resolved prefix.
//
// If even the volume root can't be resolved we refuse the path outright —
// fail-closed matches the rest of the desktop's workspace-safety stance.
func resolveCwdForContainment(cwd string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		return resolved, nil
	}
	vol := filepath.VolumeName(cwd)
	sep := string(filepath.Separator)
	root := vol + sep
	cur, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("no resolvable ancestor for %q: %w", cwd, err)
	}
	parts := strings.Split(strings.TrimPrefix(cwd[len(vol):], sep), sep)
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, part)
		if resolved, err := filepath.EvalSymlinks(next); err == nil {
			cur = resolved
			continue
		}
		// First non-resolvable component → suffix is fictional. Lexical
		// Join over the OS-resolved prefix is safe because there are no
		// more symlinks to follow on the unreachable subtree.
		remaining := append([]string{cur}, parts[i:]...)
		return filepath.Join(remaining...), nil
	}
	return cur, nil
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
