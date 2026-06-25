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

	// grokACPFirstFrameTimeout bounds how long we wait, AFTER the
	// `grok_acp_started` ack publish completes, for grok to emit its FIRST
	// stdout frame. A healthy `grok agent stdio` answers the orchestrator's
	// `initialize` request within ~1s, so the first `grok_acp_message`
	// normally lands a second or two after the ack (the gap is just one
	// Pub/Sub round-trip for the orchestrator to send `initialize`). A child
	// that stays completely silent past this window is almost always blocked
	// needing INTERACTIVE re-authentication: when grok's cached login token
	// expires it wants a browser sign-in flow it cannot present over headless
	// stdio, so it launches, we publish "Grok ACP started", and then it sits
	// forever with no output. Without this watchdog such a session hangs
	// until the optional per-session deadline (which is frequently 0 = none)
	// or the 6h stale GC — the exact silent "stuck at Grok ACP started"
	// failure this guards against. The dispatcher arms this timer via
	// ArmFirstFrameWatchdog after the ack publish so the up-to-30s
	// newSessionPublishFn timeout never eats into the budget — arming it
	// directly in Start could otherwise reduce the effective window enough
	// to kill a healthy grok waiting on `initialize` under slow Pub/Sub.
	grokACPFirstFrameTimeout = 45 * time.Second
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
	// IsolatedHome is the per-session temp dir Start points the child's
	// GROK_HOME at (a copy of the real auth file + a minimal clean
	// config.toml). It is removed best-effort exactly once, after the child
	// has exited (waitForExit), so we never delete the copied auth.json out
	// from under a running grok process. Always set on a successfully started
	// session because Start fails closed when isolation can't be established.
	IsolatedHome string

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
	// stdout reader sees grok's first frame. The first-frame watchdog
	// (watchFirstFrame) selects on it to disarm itself the instant grok
	// proves it is alive and producing output.
	firstFrame     chan struct{}
	firstFrameOnce sync.Once
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

// signalFirstFrame records that grok has emitted its first stdout frame. It is
// idempotent (sync.Once) so the per-line hot path in the stdout reader can call
// it unconditionally without a guard. Closing the channel — rather than setting
// a flag — lets the first-frame watchdog block on it directly and wake the
// instant grok proves it is alive.
func (s *GrokACPSession) signalFirstFrame() {
	s.firstFrameOnce.Do(func() {
		close(s.firstFrame)
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
	// System-level requirements.toml (`/etc/grok/requirements.toml`) is NOT
	// redirected by GROK_HOME — that's the whole point of a system layer. The
	// per-session GROK_HOME isolation below neutralises the user-level layer
	// by omission, but a managed host that pins API-key auth or an always-
	// approve policy in the system file would still bypass the workspace's
	// opt-in gates. Fail closed before we spawn rather than silently launching
	// with the unsafe pinned posture. Opt out by setting both
	// EnableGrokAPIKeyFallback and EnableGrokAlwaysApprove to acknowledge the
	// pinned posture (or remove the system requirements file).
	if err := detectPinnedSystemGrokRequirements(opts.AllowAPIKeyFallback, opts.AllowAlwaysApprove); err != nil {
		return err
	}

	args := buildGrokACPArgs(extraArgs, opts.AllowAlwaysApprove)
	// args is `{"agent", "--model", <model>, ...}` by buildGrokACPArgs's
	// validated contract (see grokACPDefaultModel block); pull args[2] so
	// setupIsolatedGrokHome carries over the matching per-model api_key when
	// the user keeps it in the `[model.<resolvedModel>]` form.
	resolvedModel := grokACPDefaultModel
	if len(args) >= 3 && args[1] == "--model" {
		resolvedModel = args[2]
	}

	// Isolated GROK_HOME (replaces the old `--config <key>=` security
	// neutralizers, which are GONE as of grok 0.2.59 — `grok agent` rejects
	// `--config` / `--permission-mode` / `--no-auto-update` with "unexpected
	// argument", so the entire persisted-config-clear-via-argv approach is
	// dead). Instead we point the child at a per-session temp dir that
	// contains ONLY a copy of the real `grok login` auth file plus a minimal
	// clean config.toml. By NOT copying the user's real config.toml /
	// requirements.toml we neutralise every persisted-config vector by
	// omission: no `api_key` billing override, no auto-approve / permission
	// bypass, no pinned requirements layer. The cached-token handshake still
	// works because the auth file is the one piece we deliberately copy in.
	// Fail closed if isolation can't be established: with `--config` gone, the
	// argv has no neutralizers, so launching with the inherited (potentially
	// unsafe) GROK_HOME would silently bypass the workspace's opt-in gates.
	isolatedHome, err := setupIsolatedGrokHome(opts.AllowAPIKeyFallback, resolvedModel)
	if err != nil {
		return fmt.Errorf("grok ACP isolation setup failed; refusing to spawn with inherited GROK_HOME: %w", err)
	}

	fmt.Printf("%s[grok-acp] Starting session %s: %s %s%s\n",
		colorCyan, id, executable, strings.Join(redactGrokACPArgsForLog(args), " "), colorReset)

	proc := exec.Command(executable, args...)
	hideWindow(proc)
	if cwd != "" {
		proc.Dir = cwd
	}
	env := sanitizeGrokACPEnv(os.Environ(), opts.AllowAPIKeyFallback)
	env = setEnvVar(env, "GROK_HOME", isolatedHome)
	proc.Env = env

	// cleanupIsolatedHome removes the per-session temp dir on any pre-spawn
	// failure path. Once the child is successfully started, ownership of the
	// dir transfers to waitForExit (which removes it after the process exits),
	// so we must NOT call this after a successful proc.Start().
	cleanupIsolatedHome := func() {
		_ = os.RemoveAll(isolatedHome)
	}

	stdin, err := proc.StdinPipe()
	if err != nil {
		cleanupIsolatedHome()
		return fmt.Errorf("failed to create stdin pipe: %w", err)
	}
	stdout, err := proc.StdoutPipe()
	if err != nil {
		stdin.Close()
		cleanupIsolatedHome()
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderr, err := proc.StderrPipe()
	if err != nil {
		stdin.Close()
		stdout.Close()
		cleanupIsolatedHome()
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := proc.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		stderr.Close()
		cleanupIsolatedHome()
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
		IsolatedHome:  isolatedHome,
		status:        "running",
		done:          make(chan struct{}),
		streamDone:    make(chan struct{}),
		firstFrame:    make(chan struct{}),
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
	// NOTE: the first-frame watchdog is NOT armed here. The dispatcher arms it
	// via ArmFirstFrameWatchdog AFTER the `grok_acp_started` ack publish
	// completes, so the budget excludes ack-publish latency (newSessionPublishFn
	// can block for up to 30s when Pub/Sub is slow). Arming here would let a
	// slow ack publish shrink the window enough to kill a healthy grok that is
	// just waiting on the orchestrator's `initialize` frame.

	fmt.Printf("%s[grok-acp] Session %s started (PID: %d)%s\n",
		colorGreen, id, proc.Process.Pid, colorReset)
	return nil
}

// ArmFirstFrameWatchdog starts the first-frame watchdog for an already-started
// session. Split out from Start so the dispatcher can call it AFTER the
// synchronous `grok_acp_started` ack publish completes — that publish can take
// up to 30s on a slow Pub/Sub, and including it in the watchdog budget would
// risk killing a healthy grok that is just waiting on the orchestrator's
// `initialize` frame. No-op if the session is unknown (already removed) or
// already exited.
func (m *GrokACPManager) ArmFirstFrameWatchdog(id string, publishFn PublishFunc) {
	session := m.Get(id)
	if session == nil {
		return
	}
	if session.Status() == "ended" {
		return
	}
	go m.watchFirstFrame(session, publishFn, grokACPFirstFrameTimeout)
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

			// grok produced output — disarm the first-frame watchdog. Done
			// before size/JSON validation on purpose: even a malformed or
			// oversize frame proves the child is alive and draining stdin, so
			// it is NOT the silent auth/startup stall the watchdog guards
			// against (those failures surface through their own fatal paths).
			session.signalFirstFrame()

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

	// Remove the per-session isolated GROK_HOME now that the child has
	// exited (Process.Wait above returned). Doing it here — and only here —
	// guarantees we never delete the copied auth.json / config.toml out from
	// under a still-running grok process, and the work runs exactly once per
	// session because waitForExit fires exactly once. Best-effort: a leftover
	// temp dir is harmless and the OS temp reaper will eventually collect it.
	if session.IsolatedHome != "" {
		_ = os.RemoveAll(session.IsolatedHome)
	}

	fmt.Printf("%s[grok-acp] Session %s ended (exit code: %d)%s\n",
		colorYellow, session.ID, exit, colorReset)

	m.removeSession(session.ID)
}

// watchFirstFrame fails a session fast when grok spawns but never produces any
// stdout — the signature of a child blocked on interactive re-authentication
// (expired cached token → grok wants a browser sign-in it can't show over
// headless stdio) or otherwise wedged at startup. Without it such a session
// sits at "Grok ACP started" until the optional per-session deadline (often
// none) or the 6h stale GC.
//
// It blocks until one of three things happens:
//   - session.firstFrame closes — grok emitted a frame; healthy, disarm.
//   - session.done closes — the child already exited (e.g. immediate crash);
//     waitForExit owns the terminal `grok_acp_ended` frame, so do nothing.
//   - timeout elapses — declare the startup stalled, publish an actionable
//     `grok_acp_error`, and kill the child so waitForExit emits the ended frame.
//
// timeout is a parameter (rather than the package constant) so unit tests can
// drive the fail-fast path with a sub-second budget and no real process.
//
// Seq ordering mirrors the per-session deadline timer: reserve the error's Seq
// (atomic increment) BEFORE Kill and publish AFTER, so the watchdog error is
// strictly ordered before the `grok_acp_ended` frame waitForExit allocates once
// the killed child is reaped — the orchestrator therefore sees error → ended.
func (m *GrokACPManager) watchFirstFrame(session *GrokACPSession, publishFn PublishFunc, timeout time.Duration) {
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
	fmt.Printf("%s[grok-acp] Session %s produced no output within %v — assuming auth/startup stall, killing%s\n",
		colorYellow, session.ID, timeout, colorReset)
	if session.Process != nil && session.Process.Process != nil {
		_ = session.Process.Process.Kill()
	}
	publishFn(resultMsg{
		ID:          session.ID,
		WorkspaceID: session.WorkspaceID,
		UID:         session.UID,
		Output: fmt.Sprintf(
			"grok produced no output within %v of starting — it is most likely not signed in on this computer "+
				"(its saved grok login/token expired; run `grok` in a terminal on the terminal computer to sign in again) "+
				"or wedged at startup. Session terminated.",
			timeout,
		),
		Status:    "error",
		Ts:        time.Now().UnixMilli(),
		Version:   Version,
		Type:      "grok_acp_error",
		SessionID: session.ID,
		Seq:       int(seq),
	})
}

/* --------------------------------------------------------------------------
   argv + env builders
   -------------------------------------------------------------------------- */

// grokACPDefaultModel is the model the ACP child runs under unless the caller
// supplies its own `--model <x>` via extraArgs. Validated live against grok
// 0.2.59's ACP handshake (initialize → authenticate{cached_token} →
// session/new → session/prompt → end_turn).
const grokACPDefaultModel = "grok-build"

// buildGrokACPArgs constructs argv for `grok agent stdio`.
//
// VALIDATED CONTRACT (grok 0.2.59): the only supported shape is
//
//	grok agent --model <model> [--always-approve] stdio
//
// Two hard constraints discovered live against `grok agent --help`:
//
//  1. `grok agent` accepts ONLY a fixed flag set (--reauth, -m/--model,
//     --reasoning-effort, --always-approve, --agent-profile, --leader/
//     --no-leader, --grok-ws-*, --cli-chat-proxy-base-url,
//     --xai-api-base-url, --debug/--debug-file, --leader-socket). It does
//     NOT accept `--config`, `--permission-mode`, or `--no-auto-update` —
//     each is rejected with "unexpected argument". The entire `--config`-
//     based security-neutralizer approach the previous implementation used
//     is therefore dead; persisted-config vectors are now neutralised by the
//     isolated GROK_HOME set up in Start (see setupIsolatedGrokHome).
//  2. Flags MUST come BEFORE the `stdio` subcommand — `stdio` itself takes no
//     options, so anything after it is mis-parsed.
//
// Model selection: the default is grok-build. A caller-supplied `--model <x>`
// (or `--model=<x>`) in extraArgs REPLACES the default. Any other extraArgs
// that aren't valid `grok agent` flags — especially the now-rejected
// `--config*` / `--permission-mode*` / `--no-auto-update`, plus `--api-key*`
// and the POSIX `--` delimiter — are stripped by sanitizeGrokACPExtraArgs so
// a signed grok_acp_start can't smuggle an incompatible flag onto the argv.
//
// `--always-approve` is appended (between `--model <x>` and `stdio`) ONLY when
// allowAlwaysApprove is true. Default false keeps autonomous tool execution an
// explicit per-workspace opt-in (Config.EnableGrokAlwaysApprove) rather than
// something a signed grok_acp_start can flip via extra args.
//
// `--api-key{,-env}` / `--auth{,-method}` are NOT in `grok agent`'s accepted
// flag set (constraint #1 above) — passing them makes the child exit with
// "unexpected argument" instead of starting the JSON-RPC handshake. So they
// are stripped unconditionally by sanitizeGrokACPExtraArgs regardless of
// Config.EnableGrokAPIKeyFallback. The opt-in fallback flows through the
// supported channels instead: XAI_API_KEY env (preserved by sanitizeGrokACPEnv
// when AllowAPIKeyFallback=true) and the persisted `[model] api_key` line that
// setupIsolatedGrokHome copies into the isolated config.toml on the same gate.
func buildGrokACPArgs(extraArgs []string, allowAlwaysApprove bool) []string {
	model, sanitized := sanitizeGrokACPExtraArgs(extraArgs, grokACPDefaultModel, allowAlwaysApprove)

	args := []string{"agent", "--model", model}
	if allowAlwaysApprove {
		args = append(args, "--always-approve")
	}
	// Any remaining sanitized extras are valid `grok agent` flags the
	// orchestrator chose to pass through; they must precede the `stdio`
	// subcommand (constraint #2 above).
	args = append(args, sanitized...)
	args = append(args, "stdio")
	return args
}

// setupIsolatedGrokHome creates a per-session temp dir to use as the child's
// GROK_HOME and seeds it with exactly two things:
//
//   - a copy of the real `grok login` auth file, so cached-token auth keeps
//     working without us inheriting anything else from the user's real
//     ~/.grok (api_key, auto-approve, pinned requirements.toml, …)
//   - a minimal clean config.toml (`[cli]\ninstaller = "internal"\nauto_update = false\n`)
//     — `auto_update = false` suppresses the headless updater check, which can
//     otherwise race `grok agent stdio` and emit non-JSON stdout that readStream
//     would treat as a fatal `grok_acp_error`
//
// This replaces the dead `--config <key>=` neutralizer machinery: grok 0.2.59
// rejects `--config` outright, so we can no longer clear persisted config via
// argv. Pointing GROK_HOME at an isolated dir that simply OMITS the dangerous
// persisted files neutralises every persisted-config vector by construction.
//
// Source auth file: `$GROK_HOME/auth.json` when GROK_HOME is set, else
// `~/.grok/auth.json`; `cached_token.json` is tried as a fallback name. A
// missing auth file is NOT fatal — we proceed with just the clean config.toml
// and let grok surface any auth error through the normal ACP handshake.
//
// allowAPIKeyFallback opts in to preserving the user's persistent
// `api_key = "..."` entry from the source `config.toml` into the isolated
// config. Without this, users who opted into API-key fallback but keep their
// key in `~/.grok/config.toml` (xAI's documented persistent form) and do NOT
// export XAI_API_KEY would silently lose API-key auth in the isolated session.
// Both the root `[model] api_key` form AND the documented per-model
// `[model.<runtimeModel>] api_key` form are carried over (the per-model match
// for the resolved runtime model wins when both exist — mirroring grok's own
// precedence in the un-isolated config). All other persisted config
// (approval/permission knobs, other model.* fields, other tables) stays
// excluded by design.
//
// Returns the temp dir path. The caller (Start) owns its lifecycle and removes
// it after the child exits (waitForExit) or on any pre-spawn failure.
func setupIsolatedGrokHome(allowAPIKeyFallback bool, runtimeModel string) (string, error) {
	dir, err := os.MkdirTemp("", "grok-acp-home-")
	if err != nil {
		return "", fmt.Errorf("create isolated grok home: %w", err)
	}

	// Real ~/.grok base: prefer an inherited GROK_HOME (so a user who
	// relocated their grok dir is still honoured), else the OS home's .grok.
	srcBase := os.Getenv("GROK_HOME")
	if srcBase == "" {
		if home, herr := os.UserHomeDir(); herr == nil {
			srcBase = filepath.Join(home, ".grok")
		}
	}

	// Copy the auth file under the first name that exists. Best-effort: a
	// missing/unreadable source is tolerated (grok surfaces the auth error
	// through the normal ACP flow).
	if srcBase != "" {
		for _, name := range []string{"auth.json", "cached_token.json"} {
			src := filepath.Join(srcBase, name)
			data, rerr := os.ReadFile(src)
			if rerr != nil {
				continue
			}
			if werr := os.WriteFile(filepath.Join(dir, name), data, 0o600); werr != nil {
				_ = os.RemoveAll(dir)
				return "", fmt.Errorf("copy grok auth file %s: %w", name, werr)
			}
		}
	}

	// Minimal clean config.toml — deliberately carries no approval/permission
	// knobs, so none of the user's real persisted policy leaks into the
	// isolated session. When allowAPIKeyFallback is true and the source
	// `config.toml` contains either `[model] api_key = "..."` OR the
	// per-model `[model.<runtimeModel>] api_key = "..."` form, that single
	// line is carried over (under the same section header it came from) so
	// the opt-in fallback also works for users whose key lives in xAI's
	// documented persistent form (not just `XAI_API_KEY`).
	// `auto_update = false` matches xAI's documented headless/scripting guidance:
	// without it, an updater check can race `grok agent stdio` and dump non-JSON
	// stdout that readStream treats as a fatal `grok_acp_error`.
	//
	// `[compat.cursor] mcps = false` + `[compat.claude] mcps = false` suppress
	// grok's vendor-MCP scan of the HOST's `~/.cursor/mcp.json` and
	// `~/.claude.json` — those files live outside $GROK_HOME so the isolated
	// dir alone can't hide them, and a slow vendor MCP (e.g. a `visualization`
	// proxy) otherwise blocks `session/new` ~10s before the ACP turn times out.
	cfg := "[cli]\ninstaller = \"internal\"\nauto_update = false\n" +
		"\n[compat.cursor]\nmcps = false\n" +
		"\n[compat.claude]\nmcps = false\n"
	if allowAPIKeyFallback && srcBase != "" {
		section, apiKey := readGrokPersistedAPIKey(filepath.Join(srcBase, "config.toml"), runtimeModel)
		if apiKey != "" {
			cfg += "\n[" + section + "]\napi_key = " + apiKey + "\n"
		}
	}
	if werr := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); werr != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("write isolated config.toml: %w", werr)
	}

	return dir, nil
}

// readGrokPersistedAPIKey returns the raw TOML value (quoted or literal, as
// found) of an `api_key` line from the given source `config.toml`, plus the
// section header it was found under (`"model"` or `"model.<runtimeModel>"`).
// Returns ("", "") when the file is missing/unreadable or no matching key is
// present. Returning the raw value preserves whichever quoting style the user
// wrote (`"xai-..."`, `'xai-...'`, or basic strings) without re-quoting
// heuristics that could corrupt embedded characters.
//
// Both the root `[model] api_key` form AND the documented per-model
// `[model.<name>] api_key` form (xAI Enterprise "API key example") are
// honoured, because users who explicitly opted into EnableGrokAPIKeyFallback
// shouldn't silently lose the fallback just because their persistent key lives
// in the per-model section that matches the model the agent runs under
// (default `grok-build`). Per-model match for `runtimeModel` takes precedence
// over the root `[model]` default — same precedence grok itself applies when
// it loads the un-isolated config — and the returned section header is mirrored
// into the isolated config so the carryover behaves identically.
//
// Tracks the active section using the same line-oriented sweep as
// detectPinnedSystemGrokRequirementsFile, with inline `#` strip and the same
// array-of-tables guard.
func readGrokPersistedAPIKey(path, runtimeModel string) (string, string) {
	if path == "" {
		return "", ""
	}
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()

	const maxBytes = 1 << 20
	scanner := bufio.NewScanner(io.LimitReader(f, maxBytes))
	scanner.Buffer(make([]byte, 64*1024), 256*1024)
	var currentSection string
	var rootSection, rootValue string
	var perModelSection, perModelValue string
	perModelMatch := ""
	if runtimeModel != "" {
		perModelMatch = "model." + strings.ToLower(runtimeModel) + ".api_key"
	}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = grokTOMLStripInlineComment(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") && !strings.HasPrefix(line, "[[") {
			currentSection = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		bareKey := strings.ToLower(strings.TrimSpace(line[:eq]))
		key := bareKey
		if currentSection != "" && !strings.Contains(bareKey, ".") {
			key = currentSection + "." + bareKey
		}
		if key == "model.api_key" && rootValue == "" {
			rootSection = "model"
			rootValue = strings.TrimSpace(line[eq+1:])
			continue
		}
		if perModelMatch != "" && key == perModelMatch && perModelValue == "" {
			perModelSection = "model." + strings.ToLower(runtimeModel)
			perModelValue = strings.TrimSpace(line[eq+1:])
		}
	}
	if perModelValue != "" {
		return perModelSection, perModelValue
	}
	return rootSection, rootValue
}

// grokSystemRequirementsPath is the documented system-level pinned-config
// layer (https://docs.x.ai/build/enterprise#configuration). Unlike user-level
// `~/.grok/requirements.toml`, it is NOT redirected by GROK_HOME — that's the
// point of a system file — so the per-session isolation in setupIsolatedGrokHome
// cannot neutralise pins set here. Operators relocate by overriding the var in
// tests; production reads it as-is.
var grokSystemRequirementsPath = "/etc/grok/requirements.toml"

// grokSystemManagedConfigPath is the second system-level layer xAI's
// enterprise loader reads. Unlike `~/.grok/managed_config.toml` (which IS
// redirected by GROK_HOME and therefore neutralised by setupIsolatedGrokHome),
// the system path is fixed and survives the isolation, so an operator pinning
// `model.api_key = "..."` or `permission_rules = ["Bash(*)"]` here would
// silently bypass `EnableGrokAPIKeyFallback` / `EnableGrokAlwaysApprove`.
// Scanned with the same line-oriented TOML logic as the requirements layer.
var grokSystemManagedConfigPath = "/etc/grok/managed_config.toml"

// claudeManagedSettingsPathsFn enumerates the Claude Code `managed-settings.json`
// locations xAI's Grok enterprise loader is documented to import
// `permissions.allow` rules from. These imports run BEFORE the per-tool prompt
// and are not redirected by GROK_HOME, so a non-empty allow list pinned here
// would route around `EnableGrokAlwaysApprove`. Per-OS system paths only —
// user-scope `~/.claude/settings.json` is intentionally NOT scanned because
// Grok's enterprise import is documented as the MDM-managed layer; treating
// ad-hoc user settings as pinned would over-fail-closed on the common
// single-user dev box. Held as a var so tests can inject paths.
var claudeManagedSettingsPathsFn = claudeManagedSettingsPaths

// detectPinnedSystemGrokRequirements refuses to start a session when a
// system-level Grok config layer pins API-key auth or a permissive approval
// policy AND the workspace has not opted into the matching gate. Both gates
// open ⇒ caller has acknowledged the pinned posture, so we let it through.
//
// Two TOML layers are scanned (`/etc/grok/requirements.toml` and
// `/etc/grok/managed_config.toml`) plus Claude Code's
// `managed-settings.json` system locations — none of these are redirected by
// GROK_HOME, so the per-session isolation in setupIsolatedGrokHome cannot
// neutralise pins set here. The TOML scan is intentionally minimal — a line-
// level keyword sweep, not a TOML parser — because the only goal here is to
// catch the dangerous markers the argv-strip surface in
// sanitizeGrokACPExtraArgs / sanitizeGrokACPEnv already neutralises at the
// per-process layer. Missing/unreadable files ⇒ skipped (best-effort by
// design; matches setupIsolatedGrokHome's tolerance for missing inputs).
func detectPinnedSystemGrokRequirements(allowAPIKey, allowAlwaysApprove bool) error {
	if allowAPIKey && allowAlwaysApprove {
		return nil
	}
	for _, p := range []string{grokSystemRequirementsPath, grokSystemManagedConfigPath} {
		if err := detectPinnedSystemGrokRequirementsFile(p, allowAPIKey, allowAlwaysApprove); err != nil {
			return err
		}
	}
	if !allowAlwaysApprove {
		// xAI's Grok enterprise loader documents importing Claude Code's
		// `managed-settings.json` and evaluating its `permissions.allow`
		// rules BEFORE the per-tool prompt
		// (https://docs.x.ai/build/enterprise#permissions). Those rules are
		// not under GROK_HOME, so the isolation cannot neutralise them; fail
		// closed when an MDM policy has set one and the workspace has not
		// opted into EnableGrokAlwaysApprove.
		for _, p := range claudeManagedSettingsPathsFn() {
			if hit, ok := detectClaudeManagedSettingsAllowRule(p); ok {
				return fmt.Errorf("grok imports Claude Code's managed-settings.json permission rules and %s contains a `permissions.allow` entry; the per-session isolated GROK_HOME cannot override an imported Claude allow rule — set Config.EnableGrokAlwaysApprove=true to opt in, or remove the imported allow rule", hit)
			}
		}
	}
	return nil
}

// detectPinnedSystemGrokRequirementsFile is the per-path scanner that backs
// detectPinnedSystemGrokRequirements. Split out so the system layers can be
// iterated cleanly and so tests can target a single path. Missing/unreadable/
// empty path ⇒ nil (best-effort).
func detectPinnedSystemGrokRequirementsFile(path string, allowAPIKey, allowAlwaysApprove bool) error {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	const maxBytes = 1 << 20
	scanner := bufio.NewScanner(io.LimitReader(f, maxBytes))
	scanner.Buffer(make([]byte, 64*1024), 256*1024)
	// Track the active TOML section so a `[permission]` header followed by a
	// bare `rules = ["Bash(*)"]` line is classified as `permission.rules` —
	// the documented section-form of the allow-list pin. Without this the
	// switch below would see the unqualified key `rules` and skip the line,
	// letting a system-layer allow rule bypass the gate. Array-of-tables
	// (`[[name]]`) is intentionally ignored: the keys we care about are all
	// scalar tables, and treating `[[arr]]` as a section would mis-prefix
	// unrelated keys inside it.
	var currentSection string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Quote-aware inline-`#` strip so `always_approve = true # managed`
		// reduces to `true` while a `pattern = "Bash(#magic)"` literal stays
		// intact — a naive strings.IndexByte('#') would corrupt the latter and
		// silently let a pinned allow rule with a `#` in its pattern route
		// past the gate.
		line = grokTOMLStripInlineComment(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") && !strings.HasPrefix(line, "[[") {
			currentSection = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			continue
		}
		lower := strings.ToLower(line)
		eq := strings.IndexByte(lower, '=')
		if eq <= 0 {
			continue
		}
		bareKey := strings.TrimSpace(lower[:eq])
		key := bareKey
		if currentSection != "" && bareKey != "" && !strings.Contains(bareKey, ".") {
			key = currentSection + "." + bareKey
		}
		// Synthesise a section-qualified `key = ...` line so the keyword
		// scanners below see `permission.rules` instead of the unqualified
		// `rules` when the file uses section-form.
		qualifiedLower := key + lower[eq:]

		if !allowAPIKey && lineMentionsGrokAuthPin(qualifiedLower) {
			return fmt.Errorf("grok requirements pin API-key auth in %s; refusing to spawn — set Config.EnableGrokAPIKeyFallback=true to opt in, or remove the pinned credential", path)
		}
		if allowAlwaysApprove {
			continue
		}
		if lineMentionsGrokApprovalPin(qualifiedLower) {
			return fmt.Errorf("grok requirements pin a permissive approval policy in %s; refusing to spawn — set Config.EnableGrokAlwaysApprove=true to opt in, or remove the pinned policy", path)
		}
		// `permission_rules` / `permission.rules` and the `policy.allow` /
		// `permissions.allow` / `tools.allow` cousins are documented xAI
		// allow-list keys that the boolean/mode-style scan above does NOT
		// catch. They have to be handled here because operators on managed
		// hosts commonly pin a `permission_rules = ["Bash(*)"]` or
		// `permission_rules = [{action = "allow", ...}]` allow rule in the
		// system layer — and that layer is NOT redirected by GROK_HOME, so
		// the isolation in setupIsolatedGrokHome cannot neutralise it.
		// Multi-line array form needs continuation accumulation before
		// classification or a `[\n {action = "allow", ...}\n]` would be read
		// as the first-line value `[` and miss the allow entry entirely.
		rawVal := strings.TrimSpace(line[eq+1:])
		switch key {
		case "permission_rules", "permission.rules":
			if grokTOMLBracketDepth(rawVal) > 0 {
				rawVal = accumulateGrokTOMLArrayContinuation(scanner, rawVal)
			}
			if grokPermissionRulesValueHasAllowAction(rawVal) {
				return fmt.Errorf("grok requirements pin a permissive permission_rules allow entry in %s; refusing to spawn — set Config.EnableGrokAlwaysApprove=true to opt in, or remove the pinned rule", path)
			}
		case "policy.allow", "permissions.allow", "tools.allow":
			cleaned := strings.TrimSpace(strings.Trim(rawVal, `"'`))
			if cleaned != "" && cleaned != "[]" && cleaned != "[ ]" {
				return fmt.Errorf("grok requirements pin a permissive %s entry in %s; refusing to spawn — set Config.EnableGrokAlwaysApprove=true to opt in, or remove the pinned rule", key, path)
			}
		}
	}
	return nil
}

// claudeManagedSettingsPaths returns the OS-specific Claude Code
// `managed-settings.json` locations. See claudeManagedSettingsPathsFn for the
// rationale on which paths are scanned and which are deliberately omitted.
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
// `managed-settings.json` at `path` contains a non-empty `permissions.allow`
// array. Returns the path on hit so the error message can point the operator
// at the exact file. Missing/unreadable/malformed/empty files yield false —
// best-effort by design, matching detectPinnedSystemGrokRequirementsFile's
// tolerance for missing inputs.
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

// grokTOMLStripInlineComment removes a trailing `# ...` comment from a TOML
// line, honoring `"..."` / `'...'` string contents so a `#` inside a quoted
// pattern (`pattern = "Bash(#magic)"`) is preserved. Without this the line-
// oriented requirements scanner would corrupt valid pinned values that
// embed `#` in a pattern literal — silently letting them route past the
// approval gate.
func grokTOMLStripInlineComment(line string) string {
	inDouble := false
	inSingle := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inDouble {
			if c == '\\' && i+1 < len(line) {
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
			return strings.TrimRight(line[:i], " \t")
		}
	}
	return line
}

// grokTOMLBracketDepth counts net `[` minus `]` characters outside TOML basic
// ("...") and literal ('...') strings, so a `pattern = "Bash[*]"` literal
// inside `permission_rules` doesn't unbalance the count.
func grokTOMLBracketDepth(s string) int {
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

// accumulateGrokTOMLArrayContinuation reads continuation lines from scanner
// while the running bracket depth (starting at the depth of `initial`) is
// still positive, joining them into a single logical value. Used by
// detectPinnedSystemGrokRequirements so a `permission_rules = [\n {action =
// "allow", ...}\n]` hand-formatted across multiple lines is classified on
// the full array value rather than the first-line `[`. Bounded at 256
// continuation lines so a corrupted file with no closing `]` can't stall
// the launch.
func accumulateGrokTOMLArrayContinuation(scanner *bufio.Scanner, initial string) string {
	depth := grokTOMLBracketDepth(initial)
	if depth <= 0 {
		return initial
	}
	parts := []string{initial}
	const maxContinuationLines = 256
	for i := 0; i < maxContinuationLines && depth > 0 && scanner.Scan(); i++ {
		ln := strings.TrimSpace(grokTOMLStripInlineComment(scanner.Text()))
		if ln == "" {
			continue
		}
		parts = append(parts, ln)
		depth += grokTOMLBracketDepth(ln)
	}
	return strings.Join(parts, " ")
}

// lineMentionsGrokAuthPin reports whether a normalised TOML line names an
// API-key credential — `api_key`/`env_key` as a key on any `model.*` / `xai.*`
// scope, with a non-empty quoted or env-style value. Kept as a flat keyword
// match (rather than a full TOML parser) because the system requirements file
// is operator-controlled and the false-positive risk on a key named "api_key"
// in another section is acceptable: failing closed is the safe direction.
func lineMentionsGrokAuthPin(lower string) bool {
	if !strings.Contains(lower, "api_key") && !strings.Contains(lower, "env_key") {
		return false
	}
	eq := strings.IndexByte(lower, '=')
	if eq < 0 {
		return false
	}
	val := strings.TrimSpace(lower[eq+1:])
	// Empty value (`api_key = ""`) is a deliberate clear — that's what the
	// old per-process `--config api_key=` neutralizer emitted, and we should
	// not refuse on it.
	if val == "" || val == `""` || val == `''` {
		return false
	}
	return true
}

// lineMentionsGrokApprovalPin reports whether a normalised TOML line pins one
// of the approval bypasses (`always_approve = true`, `auto_approve = true`,
// `approval.mode = "always"|"auto"|"always-approve"|"auto-approve"`,
// `yolo = true`, `permission_mode` matching isGrokPermissionModeBypassValue —
// the full `bypass*` / `accept-edits` / `always*` / `auto*` set — or a
// non-empty allow-list such as `policy.allow = [...]`). Bypass-value gating
// is delegated to isGrokPermissionModeBypassValue (and mirrors the
// approval-mode value set isGrokApprovalConfigKV gates argv on) so a system-
// layer pin like `permission_mode = "acceptEdits"` or `approval.mode =
// "always-approve"` trips the requirements gate identically to the argv
// `--config permission_mode=…` / `--config approval.mode=…` surface — the
// two surfaces must stay in lockstep, otherwise a managed host can route
// past the per-tool prompt despite EnableGrokAlwaysApprove=false.
func lineMentionsGrokApprovalPin(lower string) bool {
	eq := strings.IndexByte(lower, '=')
	if eq < 0 {
		return false
	}
	key := strings.TrimSpace(lower[:eq])
	val := trimGrokTOMLStringQuotes(strings.TrimSpace(lower[eq+1:]))
	switch {
	case (strings.Contains(key, "always_approve") || strings.Contains(key, "auto_approve") || key == "yolo") && val == "true":
		return true
	case strings.HasSuffix(key, "approval.mode") || key == "approval_mode" || key == "approval" || key == "mode":
		// Mirror isGrokApprovalConfigKV's approval-mode bypass-value set
		// (`always|auto` plus the documented dashed long-forms). Without the
		// long-form variants a `/etc/grok/requirements.toml` pinning
		// `approval.mode = "always-approve"` would slip past the gate while
		// the same argv `--config approval.mode=always-approve` is stripped.
		return val == "always" || val == "auto" || val == "always-approve" || val == "auto-approve"
	case strings.Contains(key, "permission_mode") || strings.Contains(key, "permission-mode"):
		return isGrokPermissionModeBypassValue(val)
	case strings.HasSuffix(key, "policy.allow") ||
		strings.HasSuffix(key, ".allow") ||
		key == "allow" || key == "allow_rules" || key == "allowlist":
		// Any non-empty allow rule auto-approves matching tools, which is
		// the same bypass surface as `always_approve = true`. Empty list /
		// empty string ⇒ deliberate clear, treat as benign.
		// `permission_rules` / `permission.rules` are NOT classified here:
		// xAI documents `action = "deny"` rules as policy-tightening (deny
		// takes precedence), so a deny-only pin from an MDM policy must not
		// trip this broad refusal. The structured switch in
		// detectPinnedSystemGrokRequirements routes `permission_rules`
		// values through grokPermissionRulesValueHasAllowAction, which only
		// fires on actual allow entries.
		return val != "" && val != "[]"
	}
	return false
}

// setEnvVar returns env with the `KEY=value` entry for key replaced (case-
// sensitive match on the key) or appended when absent. Used by Start to pin
// GROK_HOME to the isolated dir, overriding any inherited GROK_HOME the env
// sanitiser left in place.
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

// sanitizeGrokACPExtraArgs filters caller-supplied extra args down to tokens
// that are safe and valid to splice onto a `grok agent … stdio` argv, and
// extracts a caller `--model <x>` selector.
//
// It returns (model, cleaned):
//   - model is the caller's `--model` / `--model=` value when present and
//     non-empty, else defaultModel. The `--model` flag+value are consumed
//     here (not re-emitted) because buildGrokACPArgs positions `--model`
//     itself.
//   - cleaned is the remaining extras with dangerous/incompatible tokens
//     dropped: the grok-0.2.59-rejected `--config*` / `--permission-mode*` /
//     `--no-auto-update` / `--auto-update`, the credential flags `--api-key*`
//     / `--auth*` (stripped UNCONDITIONALLY — `grok agent` does not accept
//     them and would reject the argv with "unexpected argument"; the API-key
//     fallback opt-in flows through XAI_API_KEY env and the persisted
//     `[model] api_key` config.toml line instead), the `--cwd*` containment
//     side-door, `--always-approve` / `--auto-approve` (owned by buildGrokACPArgs), the
//     duplicate entry tokens (`agent`/`stdio`/`chat`/`tui`/`run`), and the
//     POSIX `--` end-of-options delimiter. `--allow <pattern>` / `--allow=…`
//     are xAI's documented pre-prompt allow rules (matching tools auto-approve
//     BEFORE the per-tool prompt runs) — stripped when allowAlwaysApprove is
//     false, mirroring the raw `session_start` path's stripGrokAllowRulePairs
//     sweep so a signed grok_acp_start cannot route around the per-tool prompt
//     by handing `--allow Bash(*)` through extras. `--deny` is policy-tightening
//     and is preserved on both sides of the gate.
func sanitizeGrokACPExtraArgs(extraArgs []string, defaultModel string, allowAlwaysApprove bool) (string, []string) {
	model := defaultModel
	cleaned := make([]string, 0, len(extraArgs))
	skipNext := false
	for i := 0; i < len(extraArgs); i++ {
		a := extraArgs[i]
		if skipNext {
			skipNext = false
			continue
		}
		lower := strings.ToLower(a)

		// Caller model selector — consume and record; buildGrokACPArgs emits
		// the `--model` flag itself.
		if lower == "--model" || lower == "-m" {
			if i+1 < len(extraArgs) {
				if v := strings.TrimSpace(extraArgs[i+1]); v != "" {
					model = v
				}
				skipNext = true
			}
			continue
		}
		if strings.HasPrefix(lower, "--model=") {
			if v := strings.TrimSpace(a[len("--model="):]); v != "" {
				model = v
			}
			continue
		}
		if strings.HasPrefix(lower, "-m=") {
			if v := strings.TrimSpace(a[len("-m="):]); v != "" {
				model = v
			}
			continue
		}

		// Duplicate entry / subcommand tokens that would re-enter the TUI
		// path or duplicate the argv we build.
		switch lower {
		case "agent", "stdio", "chat", "tui", "run":
			continue
		}

		// Flags grok 0.2.59's `grok agent` rejects outright ("unexpected
		// argument"). The previous `--config`-based neutralizers are dead;
		// these must never reach the argv. `--config` / `-c` and
		// `--permission-mode` historically took a separate value, so skip the
		// following token too when not in equals form.
		if lower == "--config" || lower == "-c" ||
			lower == "--permission-mode" || lower == "--permission_mode" {
			if !strings.Contains(a, "=") && i+1 < len(extraArgs) {
				skipNext = true
			}
			continue
		}
		if strings.HasPrefix(lower, "--config=") || strings.HasPrefix(lower, "-c=") ||
			strings.HasPrefix(lower, "--permission-mode=") || strings.HasPrefix(lower, "--permission_mode=") {
			continue
		}
		if lower == "--no-auto-update" || lower == "--auto-update" {
			continue
		}

		// Credential side-doors: `grok agent` does not accept `--api-key{,-env}`
		// / `--auth{,-method}` (constraint #1 in buildGrokACPArgs) — passing
		// them makes the child exit with "unexpected argument" instead of
		// starting the JSON-RPC handshake. Strip unconditionally, even when
		// EnableGrokAPIKeyFallback is true: the opt-in fallback flows through
		// XAI_API_KEY env (sanitizeGrokACPEnv) and the persisted
		// `[model] api_key` config.toml line (setupIsolatedGrokHome), not argv.
		if isGrokAuthOverrideArg(lower) {
			if !strings.Contains(a, "=") && i+1 < len(extraArgs) {
				skipNext = true
			}
			continue
		}

		// `--cwd` would override the proc.Dir Start validated against the
		// workspace root — drop both forms.
		if lower == "--cwd" {
			if i+1 < len(extraArgs) {
				skipNext = true
			}
			continue
		}
		if strings.HasPrefix(lower, "--cwd=") {
			continue
		}

		// `--always-approve` is owned by buildGrokACPArgs (gated on the
		// per-workspace opt-in) — never let a caller inject it directly.
		// `--auto-approve` is the documented alias on some grok builds and
		// behaves identically as an approval bypass, so it has to be
		// stripped on the same gate — otherwise extras like
		// `["--auto-approve"]` would slip past the always-approve sanitiser
		// (or hard-fail startup on versions that reject the alias).
		if lower == "--always-approve" || strings.HasPrefix(lower, "--always-approve=") ||
			lower == "--auto-approve" || strings.HasPrefix(lower, "--auto-approve=") {
			continue
		}

		// `--allow <pattern>` / `--allow=<pattern>` is xAI's documented pre-
		// prompt allow rule — matching tool calls auto-approve before the
		// per-tool prompt runs, the same bypass surface as `--always-approve`.
		// The raw `session_start` path strips it on the same gate (via
		// stripGrokAllowRulePairs); mirror that here so a signed
		// grok_acp_start passing `--allow Bash(*)` through extras cannot
		// route around the per-tool prompt when EnableGrokAlwaysApprove is
		// false. `--deny` is policy-tightening (deny takes precedence in
		// xAI's docs) and is preserved on both sides of the gate.
		if !allowAlwaysApprove {
			if lower == "--allow" {
				if i+1 < len(extraArgs) {
					skipNext = true
				}
				continue
			}
			if strings.HasPrefix(lower, "--allow=") {
				continue
			}
		}

		// POSIX end-of-options delimiter: `stdio` is appended after these
		// extras, so a surviving `--` would demote it to an operand. Drop it.
		if a == "--" {
			continue
		}

		cleaned = append(cleaned, a)
	}
	return model, cleaned
}

// redactGrokACPArgsForLog masks credential-bearing values before the startup
// banner is printed. sanitizeGrokACPExtraArgs strips `--api-key{,-env}` /
// `--auth{,-method}` from the argv unconditionally (grok agent rejects them),
// so in normal flow there is nothing to mask here. Kept as defence-in-depth in
// case a future code path appends a credential-bearing token to args without
// routing through the sanitiser; equals-form values are masked inline and
// separate-value form masks the following token. Output is passed through
// redactArgs so other secret patterns (bearer tokens, AWS keys, etc.) the
// per-arg regex recognises in caller-supplied extra args are also caught.
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
