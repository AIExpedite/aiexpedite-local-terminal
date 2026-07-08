// File: acp_core.go
// -----------------------------------------------------------------------------
// acpManager — the protocol-generic core for long-lived ACP (Agent Client
// Protocol) JSON-RPC 2.0 sessions driven over a child process' stdio
// (newline-delimited JSON). Extracted from the original GrokACPManager
// (grok_acp.go) so additional ACP-speaking CLIs (`gemini --experimental-acp`)
// reuse the exact same transport, framing, fail-fast policy, lifecycle and
// cleanup story instead of growing a third hand-rolled copy alongside
// codex_appserver.go.
//
// Per-agent drivers (grok_acp.go, gemini_acp.go) contribute only an acpSpec —
// result-type names, log tag, human-readable error nouns, a quota-capture
// hook — plus a prepare callback that performs agent-specific spawn work
// (argv building/sanitisation, env policy, isolated-home setup). Everything
// protocol-level lives here:
//
//   - We only enforce wire framing (a single JSON object per line, no embedded
//     newlines, never silently drop a frame). All JSON-RPC semantics — method
//     names, request/response correlation, `initialize`, `authenticate`,
//     `session/new`, `session/prompt`, `session/update` streaming, approval
//     responses — live in the orchestrator. Keeping this driver protocol-
//     agnostic means each agent's ACP can evolve without dragging us along.
//
//   - Frames are forwarded VERBATIM as the spec's message result type
//     (`grok_acp_message` / `gemini_acp_message`). Non-JSON lines become the
//     spec's error type so the orchestrator's state machine sees a clear
//     failure instead of misinterpreting a malformed line as a real JSON-RPC
//     response.
//
//   - Stderr is surfaced as the spec's stderr type for diagnostics but is not
//     protocol-critical.
//
// Lifecycle (identical shape to codex_appserver.go for operational
// consistency): start spawns the child, registers it in the global process
// registry, and launches stdout + stderr reader goroutines. End closes stdin
// first (ACP's documented graceful-shutdown path) and falls through to
// interrupt + kill on timeout. waitForExit publishes the spec's ended frame
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
	// acpMaxLineSize caps the bufio.Scanner buffer for a single JSONL
	// frame. Matches the Codex manager's ceiling — session.go's CLI stream
	// scanner uses the same 30 MB cap. The downstream Pub/Sub message limit
	// is 10 MB, so frames bigger than acpMaxFrameSize cannot survive a
	// publish and are session-fatal.
	acpMaxLineSize = 30 * 1024 * 1024

	// acpMaxFrameSize is a cheap pre-check on the raw stdout line. Frames
	// bigger than this cannot possibly fit Pub/Sub's 10 MB envelope before
	// JSON escaping, so we reject them without building a resultMsg. The
	// authoritative gate is acpMaxPublishSize below — the Output field is
	// JSON-string-escaped on marshal, so a frame heavy in '"' / '\' can
	// roughly double in size, meaning a line that passes this pre-check can
	// still fail the marshaled-envelope check. Frames that fail either check
	// are session-fatal — silently dropping one would deadlock the
	// orchestrator's JSON-RPC state machine waiting for a response that never
	// arrives.
	acpMaxFrameSize = 8 * 1024 * 1024

	// acpMaxPublishSize is GCP Pub/Sub's documented per-message publish
	// limit. After building each resultMsg, the stream reader marshals it and
	// rejects envelopes that exceed this ceiling — that catches the case
	// where the agent emits a frame whose raw bytes are under acpMaxFrameSize
	// but whose JSON-string-escaped Output field marshals beyond what Pub/Sub
	// will accept.
	acpMaxPublishSize = 10_000_000

	// acpStdinWriteTimeout is the upper bound on a single stdin write
	// before we declare the pipe stalled. Matches session.go's SendInput
	// budget.
	acpStdinWriteTimeout = 10 * time.Second

	// acpGracefulShutdownTimeout is how long End waits after closing
	// stdin (ACP's documented exit path) before escalating to SIGINT and
	// ultimately SIGKILL.
	acpGracefulShutdownTimeout = 5 * time.Second

	// acpStreamDrainTimeout is how long waitForExit waits for the
	// stdout/stderr readers to finish draining buffered frames before
	// publishing the ended frame. Generous to avoid racing the last JSON-RPC
	// response with the exit notification.
	acpStreamDrainTimeout = 30 * time.Second

	// acpMaxLifetime caps how long a session may stay open before
	// CleanupStale ends it. Matches SessionManager's 6 h ceiling so an
	// orchestrator crash can't leak ACP children indefinitely (each child
	// holds an auth session and may keep billing).
	acpMaxLifetime = 6 * time.Hour

	// acpCleanupInterval is how often the stale cleanup goroutine scans
	// for expired sessions.
	acpCleanupInterval = 60 * time.Second

	// acpPublishQueueSize bounds the per-session publish backlog. Every
	// ACP frame is a stateful JSON-RPC message (request id, session/update
	// notification, approval request) — losing one corrupts the session, so
	// frames are enqueued in order and drained by a single publisher
	// goroutine. If the queue fills we surface a fatal error and kill the
	// child rather than silently dropping.
	acpPublishQueueSize = 256

	// acpEnqueueTimeout is the upper bound for the stream readers to
	// wait when the publish queue is full. Hitting this means Pub/Sub has
	// stalled for a sustained period; the session cannot continue safely so
	// the manager publishes an error surface and force-kills the child to
	// fail-fast.
	acpEnqueueTimeout = 30 * time.Second

	// acpFirstFrameTimeout bounds how long we wait, AFTER the started ack
	// publish completes, for the agent to emit its FIRST stdout frame. A
	// healthy ACP child answers the orchestrator's `initialize` request
	// within ~1s, so the first message frame normally lands a second or two
	// after the ack (the gap is just one Pub/Sub round-trip for the
	// orchestrator to send `initialize`). A child that stays completely
	// silent past this window is almost always blocked needing INTERACTIVE
	// re-authentication: when its cached login token expires it wants a
	// browser sign-in flow it cannot present over headless stdio, so it
	// launches, we publish the started ack, and then it sits forever with no
	// output. Without this watchdog such a session hangs until the optional
	// per-session deadline (which is frequently 0 = none) or the 6h stale GC
	// — the exact silent "stuck at started" failure this guards against. The
	// dispatcher arms this timer via ArmFirstFrameWatchdog after the ack
	// publish so the up-to-30s newSessionPublishFn timeout never eats into
	// the budget — arming it directly in start could otherwise reduce the
	// effective window enough to kill a healthy child waiting on
	// `initialize` under slow Pub/Sub.
	acpFirstFrameTimeout = 45 * time.Second
)

/* --------------------------------------------------------------------------
   acpSpec — per-agent configuration over the shared core
   -------------------------------------------------------------------------- */

// acpSpec names everything that differs between two ACP-speaking agents:
// the wire result types, the console log tag, the wording of error surfaces,
// and the passive quota/rate-limit capture hook. Held by value on the manager
// and never mutated after construction.
type acpSpec struct {
	// family is the command/result type prefix ("grok_acp", "gemini_acp").
	// Used only for the internal dropped-frame diagnostics labels
	// (family+"_oversize_frame", family+"_scanner_error").
	family string
	// logTag is the bracketed console log prefix and process-registry label
	// ("grok-acp", "gemini-acp").
	logTag string
	// noun is the lowercase session noun used in error strings
	// ("grok acp" → "grok acp session %s not found").
	noun string
	// agentName is the bare CLI name used in stream/watchdog error strings
	// ("grok emitted a … frame", "grok produced no output …").
	agentName string
	// transport is the human-readable spawn description used in the
	// cwd-required, spawn-failure and ended strings ("grok agent stdio").
	transport string
	// startHint is the actionable parenthetical appended to a spawn failure
	// ("is `grok` on PATH or in ~/.grok/bin? run `grok login` to authenticate").
	startHint string
	// stallHint is the actionable middle of the first-frame watchdog error —
	// the agent-specific "how do I sign in again" guidance.
	stallHint string

	// Result types published to the orchestrator (device → backend).
	messageType string
	stderrType  string
	errorType   string
	endedType   string

	// captureLine, when non-nil, is invoked on every valid JSON stdout frame
	// so the per-agent quota/rate-limit capture (cliagent_ratelimit_*.go)
	// sees the ACP stream too, not just the raw session.go path. Must be
	// best-effort and non-blocking — it runs on the stream hot path.
	captureLine func(line string, now time.Time)

	// screenSetupCwd, when non-nil, is invoked by Send on the `params.cwd` of
	// every ACP session-setup frame (`session/new` / `session/load`) that
	// carries one, AFTER the containment check passes. It lets a per-agent
	// workspace-config screen re-run against a send-time cwd that differs from
	// the process start cwd — start only screened the launch cwd, but a signed
	// *_acp_send can root a session at another directory inside the workspace
	// whose project config would otherwise never be inspected. Returns a
	// non-nil error to reject the frame. Gemini wires this to
	// screenGeminiWorkspaceSettings; Grok leaves it nil (no per-cwd config to
	// screen).
	screenSetupCwd func(cwd, workspaceRoot string) error
}

/* --------------------------------------------------------------------------
   acpSession — one running ACP child process
   -------------------------------------------------------------------------- */

// acpSession holds a single child process and its stdio pipes. All public
// state is guarded by mu; stdin writes are serialised separately by stdinMu
// so concurrent Send callers cannot interleave JSONL frames.
type acpSession struct {
	ID          string
	Process     *exec.Cmd
	Stdin       io.WriteCloser
	Stdout      io.ReadCloser
	Stderr      io.ReadCloser
	StartedAt   time.Time
	WorkspaceID string
	UID         string
	TimeoutMs   int64 // 0 = no per-session timeout (rely on acpMaxLifetime stale GC)
	// WorkspaceRoot is the symlink-resolved containment root captured at start
	// (empty when the dispatcher didn't configure one). Send uses it to
	// re-enforce containment on later JSON-RPC session-setup frames
	// (`session/new` and `session/load`) whose `params.cwd` would otherwise
	// bypass the gate start applied to the process-level cwd.
	WorkspaceRoot string
	// CleanupDir is an optional per-session temp dir the agent driver's
	// prepare callback asked us to own (grok's isolated GROK_HOME). It is
	// removed best-effort exactly once, after the child has exited
	// (waitForExit), so we never delete agent state out from under a running
	// child. Empty when the driver needs no per-session dir.
	CleanupDir string

	mu           sync.Mutex
	status       string // "running" | "ended"
	exitCode     int
	stdinMu      sync.Mutex
	stdinClose   sync.Once
	done         chan struct{}
	streamDone   chan struct{}
	seq          int64
	timeoutTimer *time.Timer // armed only when TimeoutMs > 0
	// firstFrame is closed (exactly once, via firstFrameOnce) the moment the
	// stdout reader sees the agent's first frame. The first-frame watchdog
	// (watchFirstFrame) selects on it to disarm itself the instant the child
	// proves it is alive and producing output.
	firstFrame     chan struct{}
	firstFrameOnce sync.Once
}

// Status returns the current lifecycle status under the session mutex so
// callers don't race with waitForExit's transition to "ended".
func (s *acpSession) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// closeStdin is idempotent: the End fallback path calls it after stdin may
// already have been closed by waitForExit, and the second close would
// otherwise return an `io: already closed` error.
func (s *acpSession) closeStdin() {
	s.stdinClose.Do(func() {
		_ = s.Stdin.Close()
	})
}

// signalFirstFrame records that the child has emitted its first stdout frame.
// It is idempotent (sync.Once) so the per-line hot path in the stdout reader
// can call it unconditionally without a guard. Closing the channel — rather
// than setting a flag — lets the first-frame watchdog block on it directly
// and wake the instant the child proves it is alive.
func (s *acpSession) signalFirstFrame() {
	s.firstFrameOnce.Do(func() {
		close(s.firstFrame)
	})
}

/* --------------------------------------------------------------------------
   acpManager — tracks every active session for one agent family
   -------------------------------------------------------------------------- */

// acpManager owns the active ACP child processes for one agent family. One
// manager handles many concurrent sessions, mirroring SessionManager's shape.
// Always constructed via a per-agent New*Manager (grok_acp.go / gemini_acp.go)
// so spec is never zero.
type acpManager struct {
	spec     acpSpec
	sessions map[string]*acpSession
	mu       sync.RWMutex
}

// newACPManager creates a fresh manager for one agent family.
func newACPManager(spec acpSpec) acpManager {
	return acpManager{
		spec:     spec,
		sessions: make(map[string]*acpSession),
	}
}

// acpSpawnPlan is what an agent driver's prepare callback hands back to
// start once all agent-specific spawn work (argv building/sanitisation, env
// policy, isolated-home setup) succeeded. prepare runs under the manager
// mutex AFTER the duplicate-ID check, matching the original Grok ordering.
type acpSpawnPlan struct {
	executable string
	args       []string
	// logArgs is the redaction-safe argv echoed in the startup banner.
	logArgs []string
	// env is the child environment (nil = inherit the agent's own env).
	env []string
	// cleanupDir, when non-empty, is a per-session temp dir the core removes
	// on any pre-spawn failure and, once the child is running, after exit
	// (see acpSession.CleanupDir).
	cleanupDir string
}

// start launches the agent's ACP child in cwd. The agent driver validates and
// prepares everything agent-specific inside prepare; start owns the generic
// contract: cwd validation + workspace containment, duplicate-ID exclusion,
// pipe setup, timeout clamping, process registry, and the reader/exit
// goroutines.
//
// publishFn receives:
//   - spec.messageType for every JSONL frame the child emits on stdout
//   - spec.stderrType  for every line the child emits on stderr
//   - spec.errorType   when the child emits a non-JSON line (protocol bug) OR
//     when timeoutMs fires before the child exits naturally
//   - spec.endedType   when the process exits, carrying ExitCode
func (m *acpManager) start(id, cwd, workspaceRoot string, timeoutMs int64, workspaceID, uid string, publishFn PublishFunc, prepare func() (acpSpawnPlan, error)) error {
	if id == "" {
		return fmt.Errorf("sessionID is required")
	}
	if publishFn == nil {
		return fmt.Errorf("publishFn is required")
	}

	// cwd is required and must point at an existing directory. Falling back
	// to the agent's working directory would silently run the child against
	// a surprise path (e.g. C:\Program Files\AI Expedite on Windows),
	// exposing or editing files unrelated to the requested workspace. The
	// local terminal's workspace/path safety rules treat this as
	// session-fatal at startup rather than tolerating an empty cwd as the
	// legacy CLI path does.
	if cwd == "" {
		return fmt.Errorf("cwd is required for %s (must point at a workspace directory)", m.spec.transport)
	}
	if !filepath.IsAbs(cwd) {
		return fmt.Errorf("cwd must be an absolute path; got %q", cwd)
	}
	if info, err := os.Stat(cwd); err != nil {
		return fmt.Errorf("cwd %q is not accessible: %w", cwd, err)
	} else if !info.IsDir() {
		return fmt.Errorf("cwd %q is not a directory", cwd)
	}

	// Workspace-root containment: resolve symlinks on both sides and reject
	// anything that escapes the configured root. Without this, a signed
	// *_acp_start could launch the agent against any directory the OS user
	// can read/write, sidestepping the workspace/path safety stance the rest
	// of the agent enforces. When the dispatcher does not configure a root
	// (workspaceRoot == "") we skip the check — preserving the existing
	// absolute/exists contract for callers that have their own out-of-band
	// containment.
	resolvedCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return fmt.Errorf("cwd %q symlink resolution failed: %w", cwd, err)
	}
	var resolvedRoot string
	if workspaceRoot != "" {
		resolvedRoot, err = filepath.EvalSymlinks(workspaceRoot)
		if err != nil {
			return fmt.Errorf("workspace root %q symlink resolution failed: %w", workspaceRoot, err)
		}
		if !pathInsideRoot(resolvedCwd, resolvedRoot) {
			return fmt.Errorf("cwd %q is outside the configured workspace root %q", resolvedCwd, resolvedRoot)
		}
	}

	// Hold the manager mutex across the entire spawn so two concurrent start
	// calls for the same id can't both pass the existence check and double-
	// spawn (the previous check-release-insert pattern had that TOCTOU race).
	// Mirrors CodexAppServerManager.Start and SessionManager.StartSession.
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.sessions[id]; exists {
		return fmt.Errorf("%s session %s already exists", m.spec.noun, id)
	}

	plan, err := prepare()
	if err != nil {
		return err
	}

	fmt.Printf("%s[%s] Starting session %s: %s %s%s\n",
		colorCyan, m.spec.logTag, id, plan.executable, strings.Join(plan.logArgs, " "), colorReset)

	proc := exec.Command(plan.executable, plan.args...)
	hideWindow(proc)
	proc.Dir = cwd
	if plan.env != nil {
		proc.Env = plan.env
	}

	// cleanupDir removes the per-session temp dir on any pre-spawn failure
	// path. Once the child is successfully started, ownership of the dir
	// transfers to waitForExit (which removes it after the process exits),
	// so we must NOT call this after a successful proc.Start().
	cleanupDir := func() {
		if plan.cleanupDir != "" {
			_ = os.RemoveAll(plan.cleanupDir)
		}
	}

	stdin, err := proc.StdinPipe()
	if err != nil {
		cleanupDir()
		return fmt.Errorf("failed to create stdin pipe: %w", err)
	}
	stdout, err := proc.StdoutPipe()
	if err != nil {
		stdin.Close()
		cleanupDir()
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderr, err := proc.StderrPipe()
	if err != nil {
		stdin.Close()
		stdout.Close()
		cleanupDir()
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := proc.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		stderr.Close()
		cleanupDir()
		return fmt.Errorf("failed to start %s (%s): %w", m.spec.transport, m.spec.startHint, err)
	}

	// Clamp the requested timeout at our stale-GC ceiling. A misbehaving
	// orchestrator that requests timeoutMs > acpMaxLifetime would otherwise
	// outlive the GC anyway; clamping makes the effective deadline
	// predictable and keeps the timer firing meaningful.
	if timeoutMs < 0 {
		timeoutMs = 0
	}
	if max := int64(acpMaxLifetime / time.Millisecond); timeoutMs > max {
		timeoutMs = max
	}

	session := &acpSession{
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
		CleanupDir:    plan.cleanupDir,
		status:        "running",
		done:          make(chan struct{}),
		streamDone:    make(chan struct{}),
		firstFrame:    make(chan struct{}),
	}

	m.sessions[id] = session

	if proc.Process != nil {
		globalProcessRegistry.Register(proc.Process.Pid, m.spec.logTag+":"+id)
	}

	// Per-session deadline. Reserve the typed-error Seq BEFORE Kill() and
	// publish AFTER. The reservation ordering matters because Kill() races
	// with waitForExit: a fast exit can publish the ended frame (which
	// increments session.seq) before this callback would otherwise allocate
	// its own Seq, letting the orchestrator order the terminal ended frame
	// before the timeout error or drop the error as post-terminal. Taking
	// the AddInt64 first nails down a Seq strictly less than whatever
	// the ended frame ends up with, so even though both publishes are
	// asynchronous the orchestrator sees timeout → ended causal order. The
	// publish is still deferred until after Kill() because the production
	// newSessionPublishFn can block for the full Pub/Sub publish timeout
	// (~30s) when Pub/Sub is slow or unavailable; killing first guarantees
	// the orchestrator sees the child terminate on schedule and the
	// natural exit publishes the ended frame via waitForExit even if this
	// diagnostic publish itself ultimately fails. Timer is Stop()'d in
	// waitForExit on natural exit so a freshly-exited session can't
	// double-fire the timeout publish.
	if session.TimeoutMs > 0 {
		session.timeoutTimer = time.AfterFunc(time.Duration(session.TimeoutMs)*time.Millisecond, func() {
			if session.Status() == "ended" {
				return
			}
			seq := atomic.AddInt64(&session.seq, 1)
			fmt.Printf("%s[%s] Session %s timed out after %dms — killing%s\n",
				colorYellow, m.spec.logTag, session.ID, session.TimeoutMs, colorReset)
			if session.Process != nil && session.Process.Process != nil {
				_ = session.Process.Process.Kill()
			}
			publishFn(resultMsg{
				ID:          session.ID,
				WorkspaceID: session.WorkspaceID,
				UID:         session.UID,
				Output:      fmt.Sprintf("%s session timed out after %dms — terminated by per-session deadline", m.spec.noun, session.TimeoutMs),
				Status:      "error",
				Ts:          time.Now().UnixMilli(),
				Version:     Version,
				Type:        m.spec.errorType,
				SessionID:   session.ID,
				Seq:         int(seq),
			})
		})
	}

	go m.readStream(session, publishFn)
	go m.waitForExit(session, publishFn)
	// NOTE: the first-frame watchdog is NOT armed here. The dispatcher arms it
	// via ArmFirstFrameWatchdog AFTER the started ack publish completes, so
	// the budget excludes ack-publish latency (newSessionPublishFn can block
	// for up to 30s when Pub/Sub is slow). Arming here would let a slow ack
	// publish shrink the window enough to kill a healthy child that is just
	// waiting on the orchestrator's `initialize` frame.

	fmt.Printf("%s[%s] Session %s started (PID: %d)%s\n",
		colorGreen, m.spec.logTag, id, proc.Process.Pid, colorReset)
	return nil
}

// ArmFirstFrameWatchdog starts the first-frame watchdog for an already-started
// session. Split out from start so the dispatcher can call it AFTER the
// synchronous started ack publish completes — that publish can take up to 30s
// on a slow Pub/Sub, and including it in the watchdog budget would risk
// killing a healthy child that is just waiting on the orchestrator's
// `initialize` frame. No-op if the session is unknown (already removed) or
// already exited.
func (m *acpManager) ArmFirstFrameWatchdog(id string, publishFn PublishFunc) {
	session := m.Get(id)
	if session == nil {
		return
	}
	if session.Status() == "ended" {
		return
	}
	go m.watchFirstFrame(session, publishFn, acpFirstFrameTimeout)
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
func (m *acpManager) Send(id string, payload string) error {
	session := m.Get(id)
	if session == nil {
		return fmt.Errorf("%s session %s not found", m.spec.noun, id)
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
	// the child: validateACPSendCwd's session-setup containment gate
	// only inspects object-shaped frames, so a batched
	// `[{"method":"session/new", "params":{"cwd":"/outside"}}, ...]`
	// would otherwise skip the cwd check and reach the agent unfiltered.
	if trimmed[0] != '{' {
		return fmt.Errorf("payload must be a single JSON-RPC object; batch arrays and scalar frames are not supported on ACP stdio")
	}

	// Re-enforce workspace containment on ACP session-setup frames
	// (`session/new` and `session/load`). start already gated the
	// process-level cwd, but both setup verbs can carry their own
	// `params.cwd` that the agent will use as the session root — without
	// this check a later signed *_acp_send (including one that resumes a
	// prior session) could point the agent at a path outside the configured
	// workspace and bypass the original start gate. Skipped when the
	// session was launched without a containment root (mirrors start's
	// behaviour) or when the frame omits `params.cwd`.
	if session.WorkspaceRoot != "" {
		if err := validateACPSendCwd(trimmed, session.WorkspaceRoot); err != nil {
			return err
		}
	}

	// Re-run the per-agent workspace-config screen against the send-time cwd of
	// an ACP session-setup frame. The containment check above only proves the
	// send-time cwd stays inside WorkspaceRoot; it does NOT inspect the config
	// that cwd's directory contributes. For Gemini, a `session/new` /
	// `session/load` rooted at a subdirectory (or a re-created session) would
	// apply that directory's `.gemini/settings.json` (mcpServers, hooks,
	// approvalMode: yolo, …) which start's launch-cwd screen never saw. Screen
	// it before the frame is written and reject the send if it grants a
	// privilege the ACP approval flow must own. No-op for agents that leave
	// screenSetupCwd nil (e.g. Grok).
	if m.spec.screenSetupCwd != nil {
		if setupCwd := acpSessionSetupCwd(trimmed); setupCwd != "" {
			if err := m.spec.screenSetupCwd(setupCwd, session.WorkspaceRoot); err != nil {
				return err
			}
		}
	}

	session.stdinMu.Lock()
	defer session.stdinMu.Unlock()

	// Re-check Status under stdinMu so we cannot pass the gate after another
	// goroutine has already started tearing the session down via End() or
	// the timeout-fail path below.
	if session.Status() == "ended" {
		return fmt.Errorf("%s session %s has ended", m.spec.noun, id)
	}

	writeDone := make(chan error, 1)
	go func() {
		_, err := fmt.Fprintln(session.Stdin, trimmed)
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		if err != nil {
			return fmt.Errorf("failed to write to %s session %s stdin: %w", m.spec.noun, id, err)
		}
	case <-time.After(acpStdinWriteTimeout):
		// Fatal: a stalled write is a signal that the child isn't draining
		// stdin. Continuing would let the next Send acquire stdinMu and
		// interleave its frame with the abandoned write's eventual
		// completion. Close stdin to unblock the abandoned goroutine
		// immediately, transition to "ended" so concurrent/subsequent Sends
		// short-circuit, and kill the child so waitForExit publishes the
		// ended frame.
		session.closeStdin()
		session.mu.Lock()
		session.status = "ended"
		session.mu.Unlock()
		if session.Process != nil && session.Process.Process != nil {
			_ = session.Process.Process.Kill()
		}
		return fmt.Errorf("timeout writing to %s session %s stdin — session terminated to prevent frame interleave", m.spec.noun, id)
	}

	fmt.Printf("%s[%s] → %s (%d bytes)%s\n",
		colorBlue, m.spec.logTag, id, len(trimmed), colorReset)
	return nil
}

// End shuts down a session. First it closes stdin (ACP's documented exit
// path). If the process is still alive after acpGracefulShutdownTimeout
// we interrupt it, and if that also fails we SIGKILL.
func (m *acpManager) End(id string) error {
	session := m.Get(id)
	if session == nil {
		return fmt.Errorf("%s session %s not found", m.spec.noun, id)
	}

	if session.Status() == "ended" {
		m.removeSession(id)
		return nil
	}

	fmt.Printf("%s[%s] Ending session %s gracefully...%s\n",
		colorYellow, m.spec.logTag, id, colorReset)

	session.closeStdin()

	select {
	case <-session.done:
	case <-time.After(acpGracefulShutdownTimeout):
		fmt.Printf("%s[%s] Stdin close didn't exit %s — interrupting%s\n",
			colorYellow, m.spec.logTag, id, colorReset)
		_ = interruptProcess(session.Process)
		select {
		case <-session.done:
		case <-time.After(acpGracefulShutdownTimeout):
			fmt.Printf("%s[%s] Force killing session %s%s\n",
				colorRed, m.spec.logTag, id, colorReset)
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
func (m *acpManager) Get(id string) *acpSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

// ActiveCount returns the number of currently tracked sessions.
func (m *acpManager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// CleanupStale runs periodically to end sessions that exceed maxAge. Call as
// a goroutine: `go m.CleanupStale(acpMaxLifetime)`. Without this, an
// orchestrator crash that drops the *_acp_end signal would leak ACP
// children indefinitely.
func (m *acpManager) CleanupStale(maxAge time.Duration) {
	ticker := time.NewTicker(acpCleanupInterval)
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
func (m *acpManager) endStaleSessions(maxAge time.Duration) {
	m.mu.RLock()
	var staleIDs []string
	for id, session := range m.sessions {
		if time.Since(session.StartedAt) > maxAge {
			staleIDs = append(staleIDs, id)
		}
	}
	m.mu.RUnlock()

	for _, id := range staleIDs {
		fmt.Printf("%s[%s] Cleaning up stale session %s (exceeded %v)%s\n",
			colorYellow, m.spec.logTag, id, maxAge, colorReset)
		_ = m.End(id)
	}
}

// ShutdownAll ends every active session. Called during agent shutdown so
// ACP children don't outlive the agent and silently consume tokens.
func (m *acpManager) ShutdownAll() {
	m.mu.RLock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	if len(ids) > 0 {
		fmt.Printf("%s[%s] Shutting down %d active session(s)...%s\n",
			colorYellow, m.spec.logTag, len(ids), colorReset)
	}
	for _, id := range ids {
		_ = m.End(id)
	}
}

func (m *acpManager) removeSession(id string) {
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

// readStream forwards every stdout frame as spec.messageType and every
// stderr line as spec.stderrType. stdout frames are validated as JSON (so a
// malformed frame is surfaced as spec.errorType instead of being silently
// passed through), but the original line text is forwarded verbatim — we
// never edit the agent's wire format.
//
// Publishing uses a single ordered consumer goroutine that drains a bounded
// queue. Every frame in the ACP stdio protocol is a stateful JSON-RPC
// message (a response correlated by id, a session/update notification, an
// approval request); silently dropping one would corrupt the orchestrator's
// session state.
func (m *acpManager) readStream(session *acpSession, publishFn PublishFunc) {
	defer close(session.streamDone)

	queue := make(chan resultMsg, acpPublishQueueSize)
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
		case <-time.After(acpEnqueueTimeout):
			return false
		}
	}

	// failSessionFatally surfaces a queue-stall diagnostic via a synchronous
	// publish (bypassing the wedged queue) and kills the child so waitForExit
	// publishes the ended frame. Same caveat as the Codex manager: if Pub/Sub
	// is genuinely down (not merely slow) this synchronous publish also blocks
	// for the full publishFn timeout — the diagnostic only reliably lands when
	// Pub/Sub is responsive-but-slow; a true outage degrades to "child killed
	// late, no diagnostic" which is still safe (no dropped JSON-RPC frame is
	// mistaken for a live session), just not observable.
	failSessionFatally := func(reason string, droppedType string) {
		fmt.Printf("%s[%s] Publish queue stalled for %s — failing session (dropped %s)%s\n",
			colorRed, m.spec.logTag, session.ID, droppedType, colorReset)
		seq := atomic.AddInt64(&session.seq, 1)
		publishFn(resultMsg{
			ID:          session.ID,
			WorkspaceID: session.WorkspaceID,
			UID:         session.UID,
			Output:      reason,
			Status:      "error",
			Ts:          time.Now().UnixMilli(),
			Version:     Version,
			Type:        m.spec.errorType,
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
				fmt.Sprintf("%s envelope failed to marshal: %v — session terminated", m.spec.noun, err),
				droppedType,
			)
			return false
		}
		if len(encoded) > acpMaxPublishSize {
			failSessionFatally(
				fmt.Sprintf(
					"%s envelope marshaled to %d bytes after JSON escaping, exceeding the %d-byte publishable limit — session terminated to avoid silent Pub/Sub rejection",
					m.spec.noun, len(encoded), acpMaxPublishSize,
				),
				droppedType,
			)
			return false
		}
		if !enqueue(msg) {
			failSessionFatally(fmt.Sprintf("%s publish queue stalled — terminating session to avoid dropping JSON-RPC frames", m.spec.noun), droppedType)
			return false
		}
		return true
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(session.Stdout)
		scanner.Buffer(make([]byte, 0, 256*1024), acpMaxLineSize)
		fmt.Printf("%s[%s] stdout scanner started for %s%s\n",
			colorCyan, m.spec.logTag, session.ID, colorReset)
		lineCount := 0
		for scanner.Scan() {
			lineCount++
			line := scanner.Text()
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}

			if len(trimmed) > acpMaxFrameSize {
				failSessionFatally(
					fmt.Sprintf(
						"%s emitted a %d-byte frame exceeding the %d-byte publishable limit — session terminated to avoid silent drop",
						m.spec.agentName, len(trimmed), acpMaxFrameSize,
					),
					m.spec.family+"_oversize_frame",
				)
				return
			}

			seq := atomic.AddInt64(&session.seq, 1)

			var probe json.RawMessage
			if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
				fmt.Printf("%s[%s] Non-JSON stdout frame on %s: %v (line=%s)%s\n",
					colorRed, m.spec.logTag, session.ID, err, truncateString(trimmed, 200), colorReset)
				if !publishOrFail(resultMsg{
					ID:          session.ID,
					WorkspaceID: session.WorkspaceID,
					UID:         session.UID,
					Output:      fmt.Sprintf("non-JSON frame on %s stdout: %v", m.spec.noun, err),
					Status:      "error",
					Ts:          time.Now().UnixMilli(),
					Version:     Version,
					Type:        m.spec.errorType,
					SessionID:   session.ID,
					Seq:         int(seq),
				}, m.spec.errorType) {
					return
				}
				continue
			}

			// Valid JSON-RPC frame from the child — disarm the first-frame
			// watchdog. We deliberately wait for a parseable frame here
			// instead of signaling on any stdout: auth/updater paths can
			// print non-JSON banners or prompts and then stall waiting for
			// input. Treating those as proof-of-life would disarm the
			// watchdog while the ACP handshake is still hung. The non-JSON
			// branch above only reports an error frame and keeps scanning,
			// so the watchdog must stay armed until we see a real frame.
			session.signalFirstFrame()

			if lineCount <= 3 {
				fmt.Printf("%s[%s] stdout[%d] %s: %s%s\n",
					colorCyan, m.spec.logTag, lineCount, session.ID, truncateString(trimmed, 200), colorReset)
			}

			// Usage-limit telemetry: the ACP transport is the primary path
			// for normal sessions of this agent, and quota / rate-limit
			// signals arrive as `session/update` notifications (or error
			// frames) on this same stdout stream. The raw `session_start`
			// path in session.go already routes lines through the per-agent
			// capture; without mirroring it here, the CLI Agents card stays
			// Unknown for the primary flow.
			if m.spec.captureLine != nil {
				m.spec.captureLine(trimmed, time.Now())
			}

			if !publishOrFail(resultMsg{
				ID:          session.ID,
				WorkspaceID: session.WorkspaceID,
				UID:         session.UID,
				Output:      trimmed,
				Status:      "success",
				Ts:          time.Now().UnixMilli(),
				Version:     Version,
				Type:        m.spec.messageType,
				SessionID:   session.ID,
				Seq:         int(seq),
			}, m.spec.messageType) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			fmt.Printf("%s[%s] stdout scanner error for %s: %v%s\n",
				colorRed, m.spec.logTag, session.ID, err, colorReset)
			failSessionFatally(
				fmt.Sprintf("%s stdout scanner error: %v — session terminated", m.spec.agentName, err),
				m.spec.family+"_scanner_error",
			)
		}
		fmt.Printf("%s[%s] stdout scanner done for %s (%d lines)%s\n",
			colorYellow, m.spec.logTag, session.ID, lineCount, colorReset)
	}()

	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(session.Stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		fmt.Printf("%s[%s] stderr scanner started for %s%s\n",
			colorCyan, m.spec.logTag, session.ID, colorReset)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.TrimSpace(line) == "" {
				continue
			}
			seq := atomic.AddInt64(&session.seq, 1)
			fmt.Printf("%s[%s] stderr %s: %s%s\n",
				colorYellow, m.spec.logTag, session.ID, truncateString(line, 200), colorReset)
			if !publishOrFail(resultMsg{
				ID:          session.ID,
				WorkspaceID: session.WorkspaceID,
				UID:         session.UID,
				Output:      line,
				Status:      "success",
				Ts:          time.Now().UnixMilli(),
				Version:     Version,
				Type:        m.spec.stderrType,
				SessionID:   session.ID,
				Seq:         int(seq),
			}, m.spec.stderrType) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			// stderr is diagnostic, not protocol-critical: a lost/truncated
			// stderr line cannot deadlock the orchestrator's JSON-RPC state
			// machine the way a dropped stdout frame would. Log and continue.
			fmt.Printf("%s[%s] stderr scanner error for %s: %v%s\n",
				colorRed, m.spec.logTag, session.ID, err, colorReset)
		}
	}()

	wg.Wait()
}

func (m *acpManager) waitForExit(session *acpSession, publishFn PublishFunc) {
	// Reap the child via os.Process.Wait, NOT exec.Cmd.Wait. Per the
	// StdoutPipe docs, "it is incorrect to call Wait before all reads from
	// the pipe have completed" — exec.Cmd.Wait closes the parent ends of
	// StdoutPipe/StderrPipe the moment the child exits, which can truncate
	// the final JSON-RPC frame still buffered in the bufio.Scanner. When
	// the child writes a response and exits in quick succession, that final
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
	// are draining would see status=="running", publish a spurious typed
	// error AND Kill an already-exited PID. Both are observable upstream —
	// the orchestrator would surface a phantom timeout error for a session
	// that exited normally. Order is fixed: status flip → timer Stop →
	// stream drain → pipe close. Stop() additionally elides a not-yet-fired
	// timer, but it cannot interrupt an in-flight callback, which is why the
	// status flip has to come first.
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
	case <-time.After(acpStreamDrainTimeout):
		fmt.Printf("%s[%s] Stream drain timed out for %s — forcing pipe close%s\n",
			colorYellow, m.spec.logTag, session.ID, colorReset)
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
		Output:      fmt.Sprintf("%s ended (exit code: %d)", m.spec.transport, exit),
		Status:      "success",
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		Type:        m.spec.endedType,
		SessionID:   session.ID,
		ExitCode:    exit,
		Seq:         int(seq),
	})

	close(session.done)

	// Remove the per-session cleanup dir (grok's isolated GROK_HOME) now
	// that the child has exited (Process.Wait above returned). Doing it here
	// — and only here — guarantees we never delete per-session agent state
	// out from under a still-running child, and the work runs exactly once
	// per session because waitForExit fires exactly once. Best-effort: a
	// leftover temp dir is harmless and the OS temp reaper will eventually
	// collect it.
	if session.CleanupDir != "" {
		_ = os.RemoveAll(session.CleanupDir)
	}

	fmt.Printf("%s[%s] Session %s ended (exit code: %d)%s\n",
		colorYellow, m.spec.logTag, session.ID, exit, colorReset)

	m.removeSession(session.ID)
}

// watchFirstFrame fails a session fast when the child spawns but never
// produces any stdout — the signature of a child blocked on interactive
// re-authentication (expired cached token → the CLI wants a browser sign-in
// it can't show over headless stdio) or otherwise wedged at startup. Without
// it such a session sits at the started ack until the optional per-session
// deadline (often none) or the 6h stale GC.
//
// It blocks until one of three things happens:
//   - session.firstFrame closes — the child emitted a frame; healthy, disarm.
//   - session.done closes — the child already exited (e.g. immediate crash);
//     waitForExit owns the terminal ended frame, so do nothing.
//   - timeout elapses — declare the startup stalled, publish an actionable
//     typed error, and kill the child so waitForExit emits the ended frame.
//
// timeout is a parameter (rather than the package constant) so unit tests can
// drive the fail-fast path with a sub-second budget and no real process.
//
// Seq ordering mirrors the per-session deadline timer: reserve the error's Seq
// (atomic increment) BEFORE Kill and publish AFTER, so the watchdog error is
// strictly ordered before the ended frame waitForExit allocates once the
// killed child is reaped — the orchestrator therefore sees error → ended.
func (m *acpManager) watchFirstFrame(session *acpSession, publishFn PublishFunc, timeout time.Duration) {
	select {
	case <-session.firstFrame:
		return
	case <-session.done:
		return
	case <-time.After(timeout):
	}

	// Lost the race against a frame/exit that landed as the timer fired?
	// Re-check both non-blockingly so we never kill a session that just
	// proved itself alive (or already terminated on its own).
	select {
	case <-session.firstFrame:
		return
	case <-session.done:
		return
	default:
	}
	if session.Status() == "ended" {
		return
	}

	seq := atomic.AddInt64(&session.seq, 1)
	fmt.Printf("%s[%s] Session %s produced no output within %v — assuming auth/startup stall, killing%s\n",
		colorYellow, m.spec.logTag, session.ID, timeout, colorReset)
	if session.Process != nil && session.Process.Process != nil {
		_ = session.Process.Process.Kill()
	}
	publishFn(resultMsg{
		ID:          session.ID,
		WorkspaceID: session.WorkspaceID,
		UID:         session.UID,
		Output: fmt.Sprintf(
			"%s produced no output within %v of starting — %s Session terminated.",
			m.spec.agentName, timeout, m.spec.stallHint,
		),
		Status:    "error",
		Ts:        time.Now().UnixMilli(),
		Version:   Version,
		Type:      m.spec.errorType,
		SessionID: session.ID,
		Seq:       int(seq),
	})
}

/* --------------------------------------------------------------------------
   Send-time containment + shared path/env helpers
   -------------------------------------------------------------------------- */

// validateACPSendCwd inspects a JSON-RPC frame and, if it is an ACP
// session-setup request (`session/new` or `session/load`), requires its
// `params.cwd` to resolve inside root. Mirrors the containment check start
// applies to the process-level cwd so the in-protocol session cwd cannot
// escape it.
//
// Both methods are covered because ACP exposes `session/load` as the
// session-setup alternative to `session/new` for resumed sessions, and ACP
// clients pass `cwd` on loads too — gating only `session/new` would let
// a later signed *_acp_send that resumes a session point the agent at a
// directory outside the workspace root.
//
// Frames whose method is neither setup verb, or that omit `params.cwd`, are
// accepted unchanged — ACP carries many other request shapes whose params we
// must not interpret. Errors are returned only when we are certain we have a
// setup frame with a cwd that fails containment; transient parse hiccups
// fall through to acceptance because Send has already established the frame
// is valid top-level JSON.
func validateACPSendCwd(frame, resolvedRoot string) error {
	var probe struct {
		Method string `json:"method"`
		Params struct {
			Cwd string `json:"cwd"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(frame), &probe); err != nil {
		return nil
	}
	if !isACPSessionSetupMethod(probe.Method) || probe.Params.Cwd == "" {
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

// isACPSessionSetupMethod reports whether method is one of the ACP
// session-setup verbs whose `params.cwd` (when present) anchors the session
// to a workspace path and therefore must be containment-checked.
func isACPSessionSetupMethod(method string) bool {
	return method == "session/new" || method == "session/load"
}

// acpSessionSetupCwd returns the `params.cwd` of an ACP session-setup frame
// (`session/new` / `session/load`), or "" when the frame is not a setup verb,
// carries no cwd, or does not parse. Send uses it to drive the per-agent
// screenSetupCwd hook against the directory a setup frame anchors the session
// to. A transient parse hiccup returns "" (no screen) rather than an error
// because Send has already established the frame is valid top-level JSON.
func acpSessionSetupCwd(frame string) string {
	var probe struct {
		Method string `json:"method"`
		Params struct {
			Cwd string `json:"cwd"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(frame), &probe); err != nil {
		return ""
	}
	if !isACPSessionSetupMethod(probe.Method) {
		return ""
	}
	return probe.Params.Cwd
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

// setEnvVar returns env with the `KEY=value` entry for key replaced (case-
// sensitive match on the key) or appended when absent. Used by the Grok
// driver to pin GROK_HOME to the isolated dir, overriding any inherited
// GROK_HOME the env sanitiser left in place.
func setEnvVar(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			if !replaced {
				out = append(out, prefix+value)
				replaced = true
			}
			continue
		}
		out = append(out, e)
	}
	if !replaced {
		out = append(out, prefix+value)
	}
	return out
}
