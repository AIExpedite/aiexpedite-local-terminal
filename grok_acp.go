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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"golang.org/x/mod/semver"
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
	// WorkspaceRoot is the session's own symlink-resolved start cwd. Send uses
	// it to pin later JSON-RPC session-setup frames (`session/new` and
	// `session/load`) whose `params.cwd` could otherwise re-point the session
	// anywhere: those frames originate in the orchestrator's LLM tool loop,
	// which accepts a model-supplied cwd, so "the session runs where the start
	// said" is the invariant this field enforces. It is deliberately NOT the
	// agent's configured WorkingDirectory — comparing a server-derived cwd
	// against that unrelated root is the two-sources-of-truth drift that
	// refused every repo checked out outside the device home (the same defect
	// class as terminal-service §8.5). Always set on a started session.
	WorkspaceRoot string
	// IsolatedHome is the per-session temp dir Start points the child's
	// GROK_HOME at: copied auth, a minimal clean config, a persistent sessions
	// link, and a session-private billing log. It is removed through
	// removeIsolatedGrokHome exactly once after the child exits, which unlinks
	// the persistent store before deleting the ephemeral remainder. Always set
	// on a successfully started session because Start fails closed when
	// isolation cannot be built.
	IsolatedHome string
	// PersistentHome is the real Grok home captured alongside the auth copy.
	// waitForExit merges only the session's normalized account-bound billing
	// snapshot here; the child never receives this path as its log directory.
	PersistentHome string

	mu            sync.Mutex
	status        string // "running" | "ended"
	exitCode      int
	stdinMu       sync.Mutex
	stdinClose    sync.Once
	processExited chan struct{}
	done          chan struct{}
	streamDone    chan struct{}
	seq           int64
	timeoutTimer  *time.Timer // armed only when TimeoutMs > 0
	// firstFrame is closed (exactly once, via firstFrameOnce) the moment the
	// stdout reader sees grok's first frame. The first-frame watchdog
	// (watchFirstFrame) selects on it to disarm itself the instant grok
	// proves it is alive and producing output.
	firstFrame     chan struct{}
	firstFrameOnce sync.Once
	// killUnconfirmed marks a session whose End escalated to Kill and then
	// timed out waiting for the exit watcher. The session is RETAINED as a
	// tombstone (see end_confirm.go): only probeProcessGone may convert it
	// into the "not found" absence answer the server frees a device on.
	killUnconfirmed bool
	// terminalPublishState reserves this session's ID while its grok_acp_ended
	// frame is in flight — see end_confirm.go.
	terminalPublishState
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

func (s *GrokACPSession) isKillUnconfirmed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.killUnconfirmed
}

func (s *GrokACPSession) markKillUnconfirmed() {
	s.mu.Lock()
	s.killUnconfirmed = true
	s.mu.Unlock()
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
	// Config is needed at session end to scan for and upload the media the
	// session produced (see collectSessionArtifacts). Without it this
	// manager silently drops every artifact its CLI wrote.
	Config *Config
	mu     sync.RWMutex
}

// NewGrokACPManager creates a fresh manager.
func NewGrokACPManager(cfg *Config) *GrokACPManager {
	return &GrokACPManager{
		sessions: make(map[string]*GrokACPSession),
		Config:   cfg,
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

	// Resolve the start cwd through symlinks; the resolved value becomes the
	// session's OWN workspace root, which the Send path uses to pin later
	// `session/new`/`session/load` frames (model-supplied `params.cwd`) to
	// the directory this session was started in.
	//
	// This deliberately replaces the earlier check against the agent's
	// configured WorkingDirectory (secondary-review finding #1). That check
	// compared a cwd the SERVER derived (repo mapping → owner default →
	// inferred ancestor; see terminal-service contributedPaths.util.js)
	// against a root only the DEVICE knows — two sources of truth that were
	// guaranteed to disagree for any repo checked out outside the device
	// home, refusing directories the server itself had chosen (observed in
	// prod 2026-08-13: every grok review session against C:\aiexpedite died
	// at start on a device whose home is C:\Users\dkupi). It also defended
	// against nothing: the same signed command channel starts claude/codex
	// sessions and plain exec commands in arbitrary directories with no such
	// jail, so a hostile publisher never needed grok to escape. What IS worth
	// enforcing on this device is "the session stays where the start said" —
	// hence the session-rooted gate in Send.
	resolvedCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return fmt.Errorf("cwd %q symlink resolution failed: %w", cwd, err)
	}
	resolvedRoot := resolvedCwd
	args, err := buildGrokACPArgs(extraArgs, opts.AllowAlwaysApprove)
	if err != nil {
		return err
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
	// contains only copied auth, a minimal clean config.toml, a narrow link to
	// persistent conversations, and a private billing log. By NOT copying the
	// user's real config.toml /
	// requirements.toml we neutralise every persisted-config vector by
	// omission: no `api_key` billing override, no auto-approve / permission
	// bypass, no pinned requirements layer. The cached-token handshake still
	// works because the auth file is the one piece we deliberately copy in.
	// Fail closed if isolation can't be established: with `--config` gone, the
	// argv has no neutralizers, so launching with the inherited (potentially
	// unsafe) GROK_HOME would silently bypass the workspace's opt-in gates.
	persistentHome := grokPersistentHome()
	// No persistent-home attribution is written here. A managed session needs
	// none: persistGrokManagedBillingSnapshot merges its identity and its
	// record as one atomic pair, so the merged record binds to the identity
	// directly above it whatever else is in the log. The append this replaced
	// only helped a LATER direct run — which names its own account at its own
	// session start anyway — while naming, at ACP-start time, an account that
	// may not be the one a live direct run is writing records under. That made
	// starting an ACP session after a `grok login` enough to publish the live
	// direct run's utilization as the newly signed-in account's.
	isolatedHome, err := setupIsolatedGrokHomeFrom(opts.AllowAPIKeyFallback, resolvedModel, persistentHome)
	if err != nil {
		return fmt.Errorf("grok ACP isolation setup failed; refusing to spawn with inherited GROK_HOME: %w", err)
	}

	// Authoritative pre-flight against the isolated environment the child
	// would inherit. Refuse before spawn so we never wait on interactive
	// browser OAuth or the first-frame watchdog for a missing login.
	authAssessment := assessIsolatedGrokLaunch(isolatedHome, time.Now(), opts.AllowAPIKeyFallback, resolvedModel)
	if !authAssessment.Authenticated {
		cleanupIsolatedGrokHome(isolatedHome, id)
		reason := authAssessment.Reason
		if reason == "" {
			reason = "Grok is not signed in on this computer — run `grok login` on the terminal computer to authenticate."
		}
		fmt.Printf("%s[grok-acp] Refusing session %s: %s (source=%s state=%s)%s\n",
			colorYellow, id, reason, authAssessment.Source, authAssessment.AuthState, colorReset)
		return newGrokAuthError(reason)
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

	// cleanupFailedStart removes the per-session temp dir on any pre-spawn
	// failure path. Once the child is successfully started, ownership of the
	// dir transfers to waitForExit (which removes it after the process exits),
	// so we must NOT call this after a successful proc.Start().
	cleanupFailedStart := func() {
		cleanupIsolatedGrokHome(isolatedHome, id)
	}

	stdin, err := proc.StdinPipe()
	if err != nil {
		cleanupFailedStart()
		return fmt.Errorf("failed to create stdin pipe: %w", err)
	}
	stdout, err := proc.StdoutPipe()
	if err != nil {
		stdin.Close()
		cleanupFailedStart()
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderr, err := proc.StderrPipe()
	if err != nil {
		stdin.Close()
		stdout.Close()
		cleanupFailedStart()
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := proc.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		stderr.Close()
		cleanupFailedStart()
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
		ID:             id,
		Process:        proc,
		Stdin:          stdin,
		Stdout:         stdout,
		Stderr:         stderr,
		StartedAt:      time.Now(),
		WorkspaceID:    workspaceID,
		UID:            uid,
		TimeoutMs:      timeoutMs,
		WorkspaceRoot:  resolvedRoot,
		IsolatedHome:   isolatedHome,
		PersistentHome: persistentHome,
		status:         "running",
		done:           make(chan struct{}),
		processExited:  make(chan struct{}),
		streamDone:     make(chan struct{}),
		firstFrame:     make(chan struct{}),
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

	// Pin ACP session-setup frames (`session/new` and `session/load`) to the
	// directory the session was started in. Both setup verbs can carry their
	// own `params.cwd` that Grok will use as the session root, and that value
	// originates in the orchestrator's LLM tool loop (model-suppliable) — so
	// without this check a later signed grok_acp_send (including one that
	// resumes a prior session) could re-point Grok at any local path and
	// bypass the server's cwd derivation entirely. The root is the session's
	// own symlink-resolved start cwd (always set on a started session; the
	// empty-guard covers only synthetic test fixtures). Frames that omit
	// `params.cwd` pass through.
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
	processExited := processExitSignal(session.processExited, session.done)
	select {
	case <-processExited:
		// The watcher owns artifact collection, terminal publication and removal.
		return nil
	default:
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
			if !streamDrainConfirmed(session.streamDone) {
				return fmt.Errorf("grok acp session %s process absence verified but old stream publishers have not drained; session retained: %w", id, errEndUnconfirmed)
			}
			if !m.removeSessionIfSame(id, session) {
				// See CodexAppServerManager.End — the ID is not ours to free.
				return fmt.Errorf("grok acp session %s could not be released — a terminal frame is still in flight or the ID was re-taken: %w", id, errEndUnconfirmed)
			}
			return fmt.Errorf("grok acp session %s not found", id)
		}
		if session.Process.Process != nil {
			if killErr := session.Process.Process.Kill(); killErr != nil {
				fmt.Printf("%s[grok-acp] Re-kill failed for %s: %v%s\n",
					colorRed, id, killErr, colorReset)
			}
		}
		if waitDoneConfirm(processExited, killConfirmTimeout) {
			return nil
		}
		return fmt.Errorf("grok acp session %s kill unconfirmed after %s; session retained pending process-absence verification: %w", id, killConfirmTimeout, errEndUnconfirmed)
	}

	fmt.Printf("%s[grok-acp] Ending session %s gracefully...%s\n",
		colorYellow, id, colorReset)

	session.closeStdin()

	select {
	case <-processExited:
	case <-time.After(grokACPGracefulShutdownTimeout):
		fmt.Printf("%s[grok-acp] Stdin close didn't exit %s — interrupting%s\n",
			colorYellow, id, colorReset)
		_ = interruptProcess(session.Process)
		select {
		case <-processExited:
		case <-time.After(grokACPGracefulShutdownTimeout):
			fmt.Printf("%s[grok-acp] Force killing session %s%s\n",
				colorRed, id, colorReset)
			if session.Process.Process != nil {
				if killErr := session.Process.Process.Kill(); killErr != nil {
					fmt.Printf("%s[grok-acp] Kill failed for %s: %v%s\n",
						colorRed, id, killErr, colorReset)
				}
			}
			// BOUNDED wait — see end_confirm.go for why blocking here
			// indefinitely wedged an entire device (2026-08-27).
			if !waitDoneConfirm(processExited, killConfirmTimeout) {
				fmt.Printf("%s[grok-acp] Kill unconfirmed for %s after %s — retaining tombstone; a later end verifies process absence%s\n",
					colorRed, id, killConfirmTimeout, colorReset)
				session.markKillUnconfirmed()
				// Deliberately NOT "session <id> not found": the process may
				// still be alive, and the server must keep the device fenced
				// until absence is VERIFIED (see the tombstone branch above).
				return fmt.Errorf("grok acp session %s kill unconfirmed after %s; session retained pending process-absence verification: %w", id, killConfirmTimeout, errEndUnconfirmed)
			}
		}
	}

	// The watcher retains the exited session until terminal publication is
	// reserved, so a replacement cannot steal the old frame's session ID.
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
func (m *GrokACPManager) removeSessionIfSame(id string, s *GrokACPSession) bool {
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
	publisherDone := startTrackedTerminalPublisher(queue, publishFn)
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

			// Valid JSON-RPC frame from grok — disarm the first-frame
			// watchdog. We deliberately wait for a parseable frame here
			// instead of signaling on any stdout: grok's auth/updater paths
			// can print non-JSON banners or prompts and then stall waiting
			// for input. Treating those as proof-of-life would disarm the
			// watchdog while the ACP handshake is still hung. The non-JSON
			// branch above only reports a `grok_acp_error` and keeps
			// scanning, so the watchdog must stay armed until we see a real
			// frame.
			session.signalFirstFrame()

			if lineCount <= 3 {
				fmt.Printf("%s[grok-acp] stdout[%d] %s: %s%s\n",
					colorCyan, lineCount, session.ID, truncateString(trimmed, 200), colorReset)
			}

			// Grok notice telemetry: ACP frames are deliberately notice-only.
			// Numeric and confirmed-unmetered usage comes from the account-bound
			// billing log, never arbitrary response or tool-result fields. The ACP
			// transport is the primary path
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
	closeProcessExited(session.processExited)

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

	// The child wrote billing evidence only inside its account-frozen isolated
	// home. Merge one normalized snapshot before publishing the terminal frame,
	// so a refresh triggered by that frame observes it. A concurrent `grok
	// login` can change the real home's account but cannot relabel this record:
	// persistGrokManagedBillingSnapshot writes the copied producer identity and
	// billing record together.
	if outcome, err := persistGrokManagedBillingSnapshot(session.IsolatedHome, session.PersistentHome); err != nil {
		fmt.Printf("%s[grok-acp] managed billing snapshot not persisted (%s): %v%s\n",
			colorYellow, outcome, err, colorReset)
	} else {
		// Report the typed outcome: a session whose child never fetched credits
		// must not read as a successful merge in the logs.
		fmt.Printf("%s[grok-acp] managed billing snapshot: %s%s\n",
			colorCyan, outcome, colorReset)
	}

	// Scan for and upload whatever media this session wrote before announcing
	// the end, so the metadata rides along on the ended frame exactly as it
	// does on the PTY path's session_ended. Skipping this is what made every
	// capture run through a bundled CLI report NO_MEDIA_UPLOADED while the
	// recording sat on the device (prod 2026-08-20, video project vp_a72774c5).
	// BOUNDED: a hung scan/upload here used to suppress the ended frame. End
	// callers now unblock on processExited above, while session.done remains a
	// later watcher/publication lifecycle signal.
	var uploadedFiles []FileInfo
	var uploadErrors []UploadError
	if reserveSessionForTerminalWork(&m.mu, m.sessions, session.ID, session, &session.terminalPublishState) {
		uploadedFiles, uploadErrors, _ = collectSessionArtifactsBounded(m.Config, session.ID, session.WorkspaceID, session.Process.Dir, session.StartedAt, sessionArtifactCollectTimeout)
	}

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
		Output:       fmt.Sprintf("grok agent stdio ended (exit code: %d)", exit),
		Status:       "success",
		Ts:           time.Now().UnixMilli(),
		Version:      Version,
		Type:         "grok_acp_ended",
		SessionID:    session.ID,
		ExitCode:     exit,
		Seq:          int(seq),
		Files:        uploadedFiles,
		UploadErrors: uploadErrors,
	}, func() { m.removeSessionIfSame(session.ID, session) }) {
		fmt.Printf("%s[grok-acp] Suppressed stale grok_acp_ended for %s — the ID now belongs to a replacement session%s\n",
			colorYellow, session.ID, colorReset)
	}

	close(session.done)

	// Remove the per-session isolated GROK_HOME now that the child has
	// exited (Process.Wait above returned). Doing it here — and only here —
	// guarantees we never delete the copied auth.json / config.toml out from
	// under a still-running grok process, and the work runs exactly once per
	// session because waitForExit fires exactly once. Best-effort: a leftover
	// temp dir is harmless and the OS temp reaper will eventually collect it.
	if session.IsolatedHome != "" {
		cleanupIsolatedGrokHome(session.IsolatedHome, session.ID)
	}

	fmt.Printf("%s[grok-acp] Session %s ended (exit code: %d)%s\n",
		colorYellow, session.ID, exit, colorReset)

	// No-op while the ended frame is still in flight — the publisher's release
	// callback owns the removal in that case (end_confirm.go).
	m.removeSessionIfSame(session.ID, session)
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
func buildGrokACPArgs(extraArgs []string, allowAlwaysApprove bool) ([]string, error) {
	model, sanitized, err := sanitizeGrokACPExtraArgs(extraArgs, grokACPDefaultModel, allowAlwaysApprove)
	if err != nil {
		return nil, err
	}

	args := []string{"agent", "--model", model}
	if allowAlwaysApprove {
		args = append(args, "--always-approve")
	}
	// Any remaining sanitized extras are valid `grok agent` flags the
	// orchestrator chose to pass through; they must precede the `stdio`
	// subcommand (constraint #2 above).
	args = append(args, sanitized...)
	args = append(args, "stdio")
	return args, nil
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
	var rootSection, rootValue string
	var perModelSection, perModelValue string
	perModelMatch := ""
	if runtimeModel != "" {
		// Built through the SAME encoding the sweep hands back, so a model
		// whose name needs a quoted key (`grok.4`) is compared as
		// `model."grok.4".api_key` rather than as the three-segment
		// `model.grok.4.api_key` it is not.
		perModelMatch = encodeGrokTOMLKeyPath([]string{"model", strings.ToLower(runtimeModel), "api_key"})
	}
	walkGrokTOMLAssignments(path, func(key, value string) bool {
		// A value that OPENS a multiline string is only its first line here, so
		// carrying it over would write a truncated `api_key = """` into the
		// isolated config and break the whole file for the child's parser. The
		// key is left unreadable instead; grokConfigPinsCredential still
		// CONTESTS it, so the run loses its carryover, never its attribution.
		if grokTOMLValueOpensMultiline(value) {
			return true
		}
		if key == "model.api_key" && rootValue == "" {
			rootSection = "model"
			rootValue = value
			return true
		}
		if perModelMatch != "" && key == perModelMatch && perModelValue == "" {
			// Re-ENCODE rather than re-join: the sweep hands back decoded
			// segments, and setupIsolatedGrokHomeWithSessionStore writes this
			// string straight into a `[...]` header. A model whose name needs a
			// quoted key (`grok.4`) would otherwise be emitted as
			// `[model.grok.4]` — a nested table, not that model — so the copied
			// config would pass the line-based preflight and start the child
			// with no usable credential.
			perModelSection = encodeGrokTOMLKeyPath([]string{"model", strings.ToLower(runtimeModel)})
			perModelValue = value
		}
		return true
	})
	if perModelValue != "" {
		return perModelSection, perModelValue
	}
	return rootSection, rootValue
}

// walkGrokTOMLAssignments runs `visit` for every `key = value` assignment in a
// Grok TOML config, with the active section folded into a dotted key
// (`model.api_key`, `model.grok-build.api_key`) and the raw right-hand side
// preserved exactly as written. Returning false from `visit` stops the sweep.
//
// Single-sourced so readGrokPersistedAPIKey (which wants ONE key for ONE model)
// and grokConfigPinsCredential (which wants "is any credential pinned at all") cannot
// disagree about what counts as an assignment — a config whose per-model key
// one of them parses and the other misses would either carry a credential over
// silently or leave a credential override undetected by the billing-attribution
// guard. Missing/unreadable files are skipped, matching every other reader of
// these optional layers. Same line-oriented sweep as
// detectPinnedSystemGrokRequirementsFile: inline `#` strip and the same
// array-of-tables guard, not a TOML parser.
func walkGrokTOMLAssignments(path string, visit func(key, value string) bool) (complete bool) {
	if path == "" {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		// A layer that is simply ABSENT is nothing to read, so the sweep saw
		// all of it. A file that exists and cannot be opened is a scan this
		// process could not perform over content the child's parser may still
		// load, which is the incomplete case callers must fail closed on.
		return os.IsNotExist(err)
	}
	defer f.Close()

	const maxBytes = 1 << 20
	counted := &countingReader{r: io.LimitReader(f, maxBytes+1)}
	scanner := bufio.NewScanner(counted)
	scanner.Buffer(make([]byte, 64*1024), 256*1024)
	var currentSection string
	// openMultiline holds the delimiter of a multiline string whose body is
	// still running. Those lines are CONTENT, not configuration: a `[other]`
	// inside one is text grok's own parser never applies, and reading it
	// re-scoped every FOLLOWING assignment under a table that does not exist —
	// a root `model.api_key` classified as `other.model.api_key` reads as no
	// pinned credential, which is exactly the misattribution this sweep exists
	// to prevent.
	var openMultiline string
	for scanner.Scan() {
		raw := scanner.Text()
		if openMultiline != "" {
			if grokTOMLMultilineCloserIndex(raw, openMultiline) >= 0 {
				// TOML permits nothing but a comment after a closing
				// delimiter, so the rest of this line carries no assignment.
				openMultiline = ""
			}
			continue
		}
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = grokTOMLStripInlineComment(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			// An ARRAY-OF-TABLES header (`[[version_overrides]]`) opens a
			// section exactly as `[name]` does. Skipping it left the entry's
			// keys attributed to the PREVIOUS table, which both invented a
			// prefix that is not in the file and hid the documented
			// `[[version_overrides]]` + `[version_overrides.model]` spelling of
			// a pinned credential — effectiveGrokSystemConfigForVersion applies
			// that patch, so the child bills the API-key account while
			// attribution named the cached login.
			body := line[1 : len(line)-1]
			if strings.HasPrefix(line, "[[") {
				if !strings.HasSuffix(line, "]]") {
					// Not a header at all; nothing here to scope by.
					continue
				}
				body = line[2 : len(line)-2]
			}
			currentSection = grokTOMLKeyPath(body)
			continue
		}
		eq := grokTOMLAssignmentIndex(line)
		if eq <= 0 {
			continue
		}
		key := grokTOMLKeyPath(line[:eq])
		if currentSection != "" {
			// A dotted key inside a table is RELATIVE to that table: under
			// `[model]`, `grok-4.api_key` is `model.grok-4.api_key`. Prefixing
			// only single-segment keys left the per-model credential looking
			// unscoped, so the attribution guard saw no pinned credential and
			// named the cached login for an API-key-billed run.
			key = currentSection + "." + key
		}
		value := strings.TrimSpace(line[eq+1:])
		openMultiline = grokTOMLMultilineOpener(value)
		if openMultiline == "" {
			// A COMPOSITE value (a multi-line array, or an array of inline
			// tables) is READ, not skipped. Its continuation lines are value
			// body, so the sweep must not read them as configuration — a body
			// line like `[1, 2]` satisfies the section-header test above
			// exactly — but skipping them also hid every credential written
			// inside one: a documented `version_overrides = [\n { model = {
			// api_key = "..." } }\n]` patch is applied by grok's own parser
			// before it bills, while this sweep reported no pinned credential
			// and named the cached login. Joining the body into one logical
			// value keeps the section tracking correct AND lets the callers'
			// inline-table descent see what is in there.
			//
			// A composite can OPEN a multiline string on its own first line
			// (`tools = [ """`), which grokTOMLMultilineOpener does not see
			// because the value does not START with the delimiter. Both states
			// come out of the same scan so the accumulation inherits them.
			// The line was comment-stripped before the split, so the scan's
			// own stripped text is `value` verbatim here.
			_, depth, running := grokTOMLCompositeLineDepth(value, "")
			switch {
			case depth > 0:
				joined, ok := accumulateGrokTOMLCompositeValue(scanner, value, depth, running)
				if !ok {
					// An unterminated or oversized composite is a scan this
					// process could not finish over content the child's parser
					// still loads — the incomplete case callers fail closed on.
					return false
				}
				value = joined
			case running != "":
				// A string body opened without any bracket to close: nothing
				// left on this line is configuration, so it is skipped like any
				// other multiline value.
				openMultiline = running
			}
		}
		if !visit(key, value) {
			// A caller that stops early has the answer it came for, so the
			// unread remainder is not a gap in what it decided.
			return true
		}
	}
	// A line longer than the scanner's buffer, or a read error, ends the sweep
	// with content still unread; so does hitting the tail bound. So does a
	// multiline string that never closes: everything after its opener was
	// skipped as body, so an assignment the child's parser still applies may sit
	// in the part this sweep never classified. An unterminated COMPOSITE value
	// returns false from the accumulation above, at the point it gives up.
	return scanner.Err() == nil && counted.n <= maxBytes && openMultiline == ""
}

// countingReader counts the bytes it has handed on, so walkGrokTOMLAssignments
// can tell "the whole file fitted in the tail bound" from "the bound cut it
// off" — the difference between a credential that is absent and one this
// process merely never saw.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// splitGrokTOMLKeyPath splits a TOML key path — a section header's body or the
// left-hand side of an assignment — into its lower-cased segments, honouring
// TOML's quoted keys.
//
// `[model]` / `"api_key" = "xai-..."`, `"model" = { api_key = "xai-..." }` and
// `model."grok-4".env_key` are all valid configs grok's own parser applies. The
// previous raw-lowercase normalization kept the quote characters in the key, so
// none of them matched grokModelCredentialTOMLKey / grokModelScopedTOMLKey and
// the billing-attribution guard reported "no pinned credential" for a run the
// API-key account is actually billed for — publishing one subscription's spend
// under another's. A dot INSIDE quotes is part of the segment, not a separator,
// so `"model.api_key" = 1` stays the single unrelated key it is rather than
// impersonating the credential.
func splitGrokTOMLKeyPath(raw string) []string {
	var segments []string
	var cur strings.Builder
	var quote byte
	flush := func() {
		segments = append(segments, strings.ToLower(strings.TrimSpace(cur.String())))
		cur.Reset()
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if quote != 0 {
			if c == '\\' && quote == '"' && i+1 < len(raw) {
				if r, width, ok := decodeGrokTOMLEscape(raw[i+1:]); ok {
					cur.WriteRune(r)
					i += width
					continue
				}
				i++
				cur.WriteByte(raw[i])
				continue
			}
			if c == quote {
				quote = 0
				continue
			}
			cur.WriteByte(c)
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
		case '.':
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return segments
}

// grokTOMLKeyPath normalises a raw TOML key path — a section header's body or
// the left-hand side of an assignment — into the dotted, lower-cased form every
// key rule in this file compares against.
//
// It re-ENCODES the split segments rather than joining them raw, because a
// decoded segment can itself contain the separator: the single quoted key
// `"model.api_key" = 1` decodes to ONE segment whose text is `model.api_key`,
// and joining it plainly produced the exact string the two-segment
// `model.api_key = "xai-..."` produces. That collapse made an unrelated key
// impersonate the credential — grokConfigPinsCredential contested an honest
// direct run's attribution, and readGrokPersistedAPIKey would promote the
// unrelated value into a real `[model] api_key` in the isolated child config.
// Quoting a segment that is not a bare key keeps the boundary, so only keys the
// child's own parser reads as a credential match one.
func grokTOMLKeyPath(raw string) string {
	return encodeGrokTOMLKeyPath(splitGrokTOMLKeyPath(raw))
}

// encodeGrokTOMLKeyPath re-encodes decoded key segments into a TOML key path,
// quoting any segment that is not a bare key. It is the inverse of
// splitGrokTOMLKeyPath and exists because a decoded segment is only safe to
// COMPARE — emitting one verbatim into a `[...]` header turns a model name
// containing a dot, a space or a quote into a different table entirely.
func encodeGrokTOMLKeyPath(segments []string) string {
	encoded := make([]string, 0, len(segments))
	for _, seg := range segments {
		encoded = append(encoded, encodeGrokTOMLKeySegment(seg))
	}
	return strings.Join(encoded, ".")
}

// encodeGrokTOMLKeySegment returns `seg` as a TOML key: bare when every rune is
// in TOML's bare-key set (A-Za-z0-9_-), otherwise a basic-quoted string with
// the two characters that can escape it re-escaped.
func encodeGrokTOMLKeySegment(seg string) string {
	if seg == "" {
		return `""`
	}
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		bare := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-'
		if !bare {
			replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
			return `"` + replacer.Replace(seg) + `"`
		}
	}
	return seg
}

// grokTOMLValueOpensMultiline reports whether a raw right-hand side begins a
// TOML multiline string (`"""` / `”'`) that does not also close on the same
// line — the one value shape this line-oriented sweep cannot read, because the
// rest of it lives on lines the sweep sees as unrelated.
func grokTOMLValueOpensMultiline(value string) bool {
	return grokTOMLMultilineOpener(value) != ""
}

// grokTOMLMultilineOpener returns the delimiter of the multiline string a raw
// right-hand side OPENS without closing, or "" when the value is complete on
// its own line. Single-sourced with grokTOMLValueOpensMultiline so the sweep's
// "skip this body" decision and the callers' "contest this value" decision
// cannot disagree about which values run past their line.
func grokTOMLMultilineOpener(value string) string {
	v := strings.TrimSpace(value)
	for _, delim := range []string{`"""`, "'''"} {
		if strings.HasPrefix(v, delim) && grokTOMLMultilineCloserIndex(v[len(delim):], delim) < 0 {
			return delim
		}
	}
	return ""
}

// grokTOMLMultilineCloserIndex returns the index at which `s` closes an already
// OPEN multiline string delimited by `delim`, or -1 when the delimiter never
// appears unescaped.
//
// A basic (three-double-quote) multiline string still honours backslash
// escapes, so a `\"""` in its body decodes to a literal quote followed by two
// content quotes — never the terminator. Reading it as one ended the string
// early, and the body lines that followed were then read as configuration: a
// section-shaped body line re-scoped every LATER assignment, so a root
// `model.api_key` surfaced as `other.model.api_key` and the attribution guard
// reported no pinned credential for a run grok's own parser bills by API key. A
// literal (three-single-quote) multiline string defines no escapes, so a
// backslash in one is ordinary content.
//
// Only the FIRST unescaped occurrence is reported. A caller that RESUMES
// scanning after the close must use grokTOMLMultilineCloserSpan instead, which
// also reports how many bytes the terminator occupies.
func grokTOMLMultilineCloserIndex(s, delim string) int {
	idx, _ := grokTOMLMultilineCloserSpan(s, delim)
	return idx
}

// grokTOMLMultilineCloserSpan reports the index at which `s` closes an open
// multiline string delimited by `delim` AND the length of that terminator, or
// (-1, 0) when it never closes.
//
// TOML lets up to two extra quotes sit against a closing delimiter: in
// `"""foo""""` the content is `foo"` and the terminator is the LAST three
// quotes, so the whole run is what ends the string. Reporting just the first
// triple left the extra quote for the caller to resume ON, which opened a fresh
// single-quoted string in the composite and inline-table scanners; both then
// finished with quote state still open and reported the scan INCOMPLETE, and
// grokConfigPinsCredential reads an incomplete scan as a pin — contesting a
// direct run over a config that pins no credential and rejecting its billing
// observation. A run of six or more quotes is not a single terminator (it is a
// close immediately followed by a new opener), so the span is capped at five
// bytes rather than swallowing the next string's delimiter.
func grokTOMLMultilineCloserSpan(s, delim string) (int, int) {
	escapes := delim == `"""`
	quote := delim[0]
	for i := 0; i < len(s); i++ {
		if escapes && s[i] == '\\' {
			i++
			continue
		}
		if !strings.HasPrefix(s[i:], delim) {
			continue
		}
		run := len(delim)
		for i+run < len(s) && s[i+run] == quote {
			run++
		}
		if run > len(delim)+2 {
			// Six or more quotes in a row is a terminator immediately followed
			// by a new opener, not one long terminator; consuming the run
			// whole would swallow the next string's delimiter and leave the
			// scan reading its body as value syntax.
			run = len(delim)
		}
		return i, run
	}
	return -1, 0
}

// grokTOMLAssignmentIndex returns the index of the `=` separating a TOML
// assignment's key path from its value, or -1 when the text carries no
// assignment separator outside its quoted key segments.
//
// A QUOTED key segment may contain an `=` of its own: under `[model]`,
// `"foo=bar".api_key = "xai-..."` is a valid pin grok's own parser applies.
// Taking the first `=` in the line split that key as `model.foo`, so the
// credential never matched grokModelCredentialTOMLKey and an API-key-billed run
// was attributed to the cached login. Quote handling matches
// splitGrokTOMLKeyPath — backslash escapes inside a basic-quoted segment, none
// inside a literal one — so the two cannot disagree about where a key ends. An
// unterminated quote has no separator at this level and reports -1, which every
// caller skips.
func grokTOMLAssignmentIndex(s string) int {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' && quote == '"' && i+1 < len(s) {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
		case '=':
			return i
		}
	}
	return -1
}

// decodeGrokTOMLEscape decodes ONE escape sequence from the body of a TOML
// basic-quoted key, returning the rune, how many bytes of `s` it consumed and
// whether `s` opened with an escape TOML actually defines.
//
// Grok's own parser resolves `"api_\u006bey"` to `api_key`; merely dropping the
// backslash produced `api_u006bey`, which matched no credential key, so the
// billing-attribution guard reported "no pinned credential" and published an
// API-key run's spend under the cached-login account. An UNDEFINED escape is
// reported as such rather than guessed at — the caller keeps its previous
// literal-next-byte behaviour there, which is what a best-effort sweep over a
// possibly-invalid config should do.
func decodeGrokTOMLEscape(s string) (rune, int, bool) {
	if s == "" {
		return 0, 0, false
	}
	switch s[0] {
	case 'b':
		return '\b', 1, true
	case 't':
		return '\t', 1, true
	case 'n':
		return '\n', 1, true
	case 'f':
		return '\f', 1, true
	case 'r':
		return '\r', 1, true
	case '"':
		return '"', 1, true
	case '\\':
		return '\\', 1, true
	case 'u', 'U':
		width := 4
		if s[0] == 'U' {
			width = 8
		}
		if len(s) < 1+width {
			return 0, 0, false
		}
		v, err := strconv.ParseUint(s[1:1+width], 16, 32)
		if err != nil {
			return 0, 0, false
		}
		r := rune(v)
		if !utf8.ValidRune(r) {
			return 0, 0, false
		}
		return r, 1 + width, true
	}
	return 0, 0, false
}

// grokModelCredentialKeySuffixes are the `[model]` assignments that hand a Grok
// child a credential of its own. `api_key` carries the key inline; `env_key`
// names the environment variable the key is read from — a different spelling of
// the same credential, which classifyGrokSystemSemanticValue already treats as
// one. Listing both here is what keeps the semantic classifier and the
// billing-attribution guard from disagreeing about what a credential is.
var grokModelCredentialKeySuffixes = []string{"api_key", "env_key"}

// grokConfigPinsCredential reports whether a Grok TOML config pins a credential
// for ANY model, not just the one a given session runs under.
//
// readGrokPersistedAPIKey answers "which key would this model use", which needs
// a runtime model. A DIRECT (PTY) run has no resolved model — the user picks it
// inside the CLI — so the attribution guard has to assume any pinned credential
// could be the one that ends up billed, and a per-model key it could not see
// would be exactly the misattribution the guard exists to prevent.
//
// An `env_key` pin counts even when the variable it names is unset in this
// process: the child resolves that variable in its OWN environment, per turn,
// and the guard is conservative by construction — over-reporting costs one
// session's observability, under-reporting publishes one account's spend as
// another's.
// An INLINE TABLE counts too. `model = { api_key = "xai-..." }` and
// `[model] grok-4 = { env_key = "OTHER" }` are valid TOML that grok's own parser
// honours, but the line-oriented sweep only ever exposes the OUTER key (`model`,
// `model.grok-4`), so the credential lives entirely inside the value. Descending
// into the braces is what keeps a config written in that shape from silently
// attributing an API-key run to the cached login.
// A scan this process could not COMPLETE — an oversized config truncated at the
// tail bound, a line past the scanner's buffer, an unreadable file — reports
// pinned as well. The child's own parser reads the whole layer regardless, so a
// credential past the boundary would be billed to the API-key account while
// direct attribution named the cached login. Contesting costs that session its
// observability; the alternative publishes one subscription's spend as another's.
func grokConfigPinsCredential(path string) bool {
	found := false
	complete := walkGrokTOMLAssignments(path, func(key, value string) bool {
		if grokAssignmentPinsAccount(key, value) {
			found = true
			return false
		}
		// The sweep is line-oriented, so an INLINE table exposes only its outer
		// key: `model = { api_key = "..." }` and
		// `auth = { oidc = { issuer = "..." } }` both hide the assignment that
		// matters inside the value. Descending with the outer key as the prefix
		// lets one set of key rules decide both spellings, so the dotted and
		// the inline form of the same setting cannot disagree.
		if grokInlineTableMatches(key, value, grokAssignmentPinsAccount) {
			found = true
			return false
		}
		return true
	})
	return found || !complete
}

// grokAssignmentPinsAccount reports whether ONE resolved assignment takes the
// child off the cached `$GROK_HOME` login — a pinned model credential, or a
// setting that authenticates through a provider of its own.
//
// A multiline opener reads as empty to this line-oriented sweep while grok's
// own parser resolves the whole value and bills the account it names, so it is
// CONTESTED rather than dismissed. An empty value is not a pin and must not
// cost an honest run its observability.
func grokAssignmentPinsAccount(key, value string) bool {
	if !grokAttributionKeyLeavesCachedLogin(key) {
		return false
	}
	return grokTOMLValueOpensMultiline(value) ||
		strings.TrimSpace(strings.Trim(value, `"'`)) != ""
}

// grokAttributionKeyLeavesCachedLogin reports whether a dotted key names a
// model credential or an external auth provider, in the file's own scope or
// inside a `version_overrides` patch.
//
// Grok applies those patches (effectiveGrokSystemConfigForVersion) into the
// effective config BEFORE anything classifies it, so a credential written there
// is a credential the child bills with — in the inline
// `version_overrides = [{ model = { api_key = "..." } }]` spelling AND in the
// documented `[[version_overrides]]` + `[version_overrides.model]` one, which
// reaches this function as `version_overrides.model.api_key`. Applicability is
// deliberately NOT resolved: which entries apply depends on the child's own
// version, and this guard is conservative by construction — over-reporting
// costs one session's observability, under-reporting publishes one account's
// spend as another's.
func grokAttributionKeyLeavesCachedLogin(key string) bool {
	if grokModelCredentialTOMLKey(key) || grokTOMLKeyNamesExternalAuthProvider(key) {
		return true
	}
	relative, ok := grokVersionOverrideRelativeKey(key)
	if !ok {
		return false
	}
	// Inside an override entry the credential may be written without the
	// `model.` scope the root config needs, so the looser inline-table spelling
	// counts here too.
	return grokModelCredentialTOMLKey(relative) ||
		grokInlineTableCredentialKey(relative) ||
		grokTOMLKeyNamesExternalAuthProvider(relative)
}

// grokVersionOverrideRelativeKey returns the part of a dotted key that sits
// INSIDE a `version_overrides` entry, and whether the key is inside one at all.
// The array index has no spelling in either form — an inline element keeps the
// outer key, an `[[version_overrides]]` header names the table — so the
// remainder after the segment is the key as the patched config would read it.
func grokVersionOverrideRelativeKey(key string) (string, bool) {
	segments := strings.Split(key, ".")
	for i, segment := range segments {
		if segment == "version_overrides" && i+1 < len(segments) {
			return strings.Join(segments[i+1:], "."), true
		}
	}
	return "", false
}

// grokTOMLKeyNamesExternalAuthProvider reports whether a dotted key names one of
// the settings that make a Grok child authenticate through a provider of its
// own rather than the cached `$GROK_HOME` login. Kept to the same shapes
// classifyGrokSystemSemanticValue treats as external providers
// (`auth_provider_command`, and the `[auth.oidc]` issuer/client pair), so the
// semantic classifier and the billing-attribution guard cannot disagree about
// which layers take the child off the cached account.
func grokTOMLKeyNamesExternalAuthProvider(key string) bool {
	segments := strings.Split(key, ".")
	last := segments[len(segments)-1]
	if last == "auth_provider_command" {
		return true
	}
	if last != "issuer" && last != "client_id" {
		return false
	}
	return len(segments) >= 3 &&
		segments[len(segments)-2] == "oidc" &&
		segments[len(segments)-3] == "auth"
}

// grokInlineTableMaxDepth bounds the descent into nested inline tables. Deep
// enough for any config a human writes, finite so a pathological or hostile
// value cannot turn a session start into unbounded recursion.
const grokInlineTableMaxDepth = 8

// grokInlineTableMatches reports whether any assignment nested inside an inline
// TOML value satisfies `match`, which is handed the assignment's FULL dotted
// key — the outer key this value was assigned to, plus the path inside the
// table — and its raw value.
//
// Composing the full key is what lets the dotted and the inline spelling of the
// same setting share one set of key rules: `[auth] auth_provider_command = ...`
// and `auth = { auth_provider_command = ... }` both reach `match` as
// `auth.auth_provider_command`. A value that is not an inline table (a bare
// string, number or scalar array) has nothing nested in it and reports false —
// the caller has already judged the assignment itself. Quoted strings are
// skipped over while splitting so a `,`, `{` or `}` inside a value cannot
// desynchronise the scan and hide the assignment that follows it.
func grokInlineTableMatches(key, value string, match func(key, value string) bool) bool {
	return grokInlineTableMatchesAt(key, value, match, 0)
}

func grokInlineTableMatchesAt(key, value string, match func(key, value string) bool, depth int) bool {
	if depth >= grokInlineTableMaxDepth {
		// Deeper than any real config nests. Fail CLOSED: we cannot rule the
		// credential out, and over-reporting costs one session's observability
		// while under-reporting publishes one account's spend as another's.
		return true
	}
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		// An ARRAY of inline tables is the documented shape of
		// `version_overrides`, and a `[model] grok-4 = [{ api_key = ... }]` is
		// valid TOML too. An array index has no key of its own, so elements
		// keep the outer key; an array of plain scalars simply has no element
		// that is an inline table and reports false.
		elements, split := splitGrokInlineTableFields(value[1 : len(value)-1])
		if !split {
			// The body ends inside a string or bracket, so the fragments are
			// not the elements grok's own parser sees. Same fail-CLOSED reason
			// as the depth bound above.
			return true
		}
		for _, element := range elements {
			if grokInlineTableMatchesAt(key, element, match, depth+1) {
				return true
			}
		}
		return false
	}
	if !strings.HasPrefix(value, "{") || !strings.HasSuffix(value, "}") {
		return false
	}
	fields, split := splitGrokInlineTableFields(value[1 : len(value)-1])
	if !split {
		return true
	}
	for _, field := range fields {
		eq := grokTOMLAssignmentIndex(field)
		if eq <= 0 {
			continue
		}
		fieldKey := grokTOMLKeyPath(field[:eq])
		if key != "" {
			fieldKey = key + "." + fieldKey
		}
		inner := strings.TrimSpace(field[eq+1:])
		if match(fieldKey, inner) {
			return true
		}
		if grokInlineTableMatchesAt(fieldKey, inner, match, depth+1) {
			return true
		}
	}
	return false
}

// grokInlineTableCredentialKey reports whether a key written INSIDE an inline
// table names a credential, in either the bare (`api_key`) or dotted
// (`grok-4.api_key`) spelling TOML allows there.
func grokInlineTableCredentialKey(key string) bool {
	for _, suffix := range grokModelCredentialKeySuffixes {
		if key == suffix || strings.HasSuffix(key, "."+suffix) {
			return true
		}
	}
	return false
}

// splitGrokInlineTableFields splits the BODY of an inline table on its
// top-level commas, ignoring commas inside quoted strings or nested
// inline tables / arrays so a nested value cannot swallow the field after it.
//
// Multiline (triple-quoted) strings are tracked with the SAME closer rules the
// line sweep uses, because the body handed here is a composite value the sweep
// already JOINED across lines: `note = ”'it's, text”', model = { api_key =
// "xai-..." }` is one field plus another, and reading the apostrophe in `it's`
// as a terminator split it mid-string, left `model.api_key` inside a falsely
// open quote and lost the pin. The second result reports whether every string
// and bracket closed; a body that ends inside one cannot be split faithfully,
// and the caller fails CLOSED rather than trusting the fragments.
func splitGrokInlineTableFields(body string) ([]string, bool) {
	var fields []string
	depth := 0
	var quote byte
	start := 0
	for i := 0; i < len(body); i++ {
		c := body[i]
		if quote != 0 {
			if c == '\\' && quote == '"' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			if delim := grokTOMLMultilineDelimiterAt(body[i:]); delim != "" {
				closer, closerLen := grokTOMLMultilineCloserSpan(body[i+len(delim):], delim)
				if closer < 0 {
					// Never closes: the rest of the body is string content, so
					// no further field boundary is knowable.
					return append(fields, body[start:]), false
				}
				i += len(delim) + closer + closerLen - 1
				continue
			}
			quote = c
		case '{', '[':
			depth++
		case '}', ']':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				fields = append(fields, body[start:i])
				start = i + 1
			}
		}
	}
	return append(fields, body[start:]), quote == 0 && depth == 0
}

// grokTOMLMultilineDelimiterAt returns the triple-quote delimiter `s` opens
// with, or "" when it opens with a single-quote of either kind (or nothing).
func grokTOMLMultilineDelimiterAt(s string) string {
	for _, delim := range []string{`"""`, "'''"} {
		if strings.HasPrefix(s, delim) {
			return delim
		}
	}
	return ""
}

// grokModelCredentialTOMLKey reports whether a dotted TOML key from
// walkGrokTOMLAssignments names a `[model]` credential, in either the root
// (`model.api_key`) or the documented per-model (`model.<name>.env_key`) form.
func grokModelCredentialTOMLKey(key string) bool {
	if !strings.HasPrefix(key, "model.") {
		return false
	}
	for _, suffix := range grokModelCredentialKeySuffixes {
		if key == "model."+suffix || strings.HasSuffix(key, "."+suffix) {
			return true
		}
	}
	return false
}

// grokUserConfigFileNames are the TOML layers xAI's loader reads out of
// GROK_HOME. All three are user-level — GROK_HOME redirects them, which is what
// setupIsolatedGrokHome relies on to neutralise them for MANAGED sessions — but
// a DIRECT (PTY) run reads the user's REAL home, so a credential pinned in any
// of them is a credential the child can bill. Scanning only `config.toml` would
// attribute a `managed_config.toml`/`requirements.toml` API-key run to the
// cached login and publish that spend under the wrong subscription.
//
// The system-level twins (`/etc/grok/...`) are a separate list because they are
// NOT redirected by GROK_HOME and so apply to managed sessions too.
var grokUserConfigFileNames = []string{"config.toml", "managed_config.toml", "requirements.toml"}

// grokAnyPinnedCredential reports whether a credential is pinned by any user
// config layer in the home `base`, or by a system config layer that GROK_HOME
// cannot redirect.
func grokAnyPinnedCredential(base string) bool {
	for _, p := range grokSystemConfigPathsFn() {
		if grokConfigPinsCredential(p) {
			return true
		}
	}
	if base == "" {
		return false
	}
	for _, name := range grokUserConfigFileNames {
		if grokConfigPinsCredential(filepath.Join(base, name)) {
			return true
		}
	}
	return false
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

// grokSystemConfigPathsFn enumerates the system-level Grok TOML layers that
// survive the per-session GROK_HOME isolation. Held as a var so tests can
// point it at a temp dir; production reads the two documented paths.
var grokSystemConfigPathsFn = func() []string {
	return []string{grokSystemRequirementsPath, grokSystemManagedConfigPath}
}

// grokSystemPinnedAPIKey reports whether a system-level layer pins a non-empty
// `[model] api_key` (or the per-model `[model.<name>]` form). Those files are
// NOT redirected by GROK_HOME, so the child grok process reads them even under
// the isolated home — which makes such a key a REAL credential for the launch
// pre-flight, not just something to gate on. Only consulted once the workspace
// has opted into EnableGrokAPIKeyFallback; without that opt-in
// detectPinnedSystemGrokRequirements has already refused the spawn.
func grokSystemPinnedAPIKey(runtimeModel string) bool {
	for _, p := range grokSystemConfigPathsFn() {
		if _, value := readGrokPersistedAPIKey(p, runtimeModel); strings.TrimSpace(strings.Trim(value, `"'`)) != "" {
			return true
		}
	}
	return false
}

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
// per-process layer. Missing TOML files are skipped; the shared Claude JSON
// inspector separately fails closed when an existing managed file cannot be
// read completely or parsed safely.
func detectPinnedSystemGrokRequirements(allowAPIKey, allowAlwaysApprove bool) error {
	if allowAPIKey && allowAlwaysApprove {
		return nil
	}
	for _, p := range grokSystemConfigPathsFn() {
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
			ok, err := inspectClaudeManagedSettingsAllowRule(p)
			if err != nil {
				return fmt.Errorf("grok cannot safely inspect Claude managed settings; refusing to spawn")
			}
			if ok {
				return fmt.Errorf("grok imports Claude Code's managed-settings.json permission rules and the managed policy contains a `permissions.allow` entry; the per-session isolated GROK_HOME cannot override an imported Claude allow rule — set Config.EnableGrokAlwaysApprove=true to opt in, or remove the imported allow rule")
			}
		}
	}
	return nil
}

// detectGrokMaintenanceSmokeSystemConfig applies the stricter system-layer
// posture required by the subscription-only, no-tools maintenance smoke.
// GROK_HOME isolation cannot hide xAI's system requirements/managed-config
// layers, so decode their TOML semantics and refuse pinned credentials,
// permissive approval, external-tool definitions, or settings that re-enable
// vendor MCP discovery.
func detectGrokMaintenanceSmokeSystemConfig(grokVersion string) error {
	for _, path := range grokSystemConfigPathsFn() {
		finding, ok, err := inspectGrokSystemConfigSemantic(path, grokVersion)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if finding.credential || finding.externalProvider || finding.permissiveApproval {
			return fmt.Errorf("grok system configuration pins credentials, external provider routing, or a permissive approval policy; refusing no-tools maintenance smoke")
		}
		if finding.toolCategory != "" {
			return fmt.Errorf("grok system configuration contains disallowed %q settings; refusing no-tools maintenance smoke", finding.toolCategory)
		}
	}
	// Claude's managed-settings compatibility layer is JSON rather than TOML,
	// so keep its dedicated semantic reader alongside the TOML traversal.
	for _, path := range claudeManagedSettingsPathsFn() {
		ok, err := inspectClaudeManagedSettingsAllowRule(path)
		if err != nil {
			return fmt.Errorf("grok system configuration cannot be inspected safely; refusing no-tools maintenance smoke")
		}
		if ok {
			return fmt.Errorf("grok system configuration pins credentials or a permissive approval policy; refusing no-tools maintenance smoke")
		}
	}
	return nil
}

type grokSystemSemanticFinding struct {
	credential         bool
	externalProvider   bool
	permissiveApproval bool
	toolCategory       string
}

// inspectGrokSystemConfigSemantic parses one system TOML layer and walks its
// decoded key tree. Inline tables, quoted keys, dotted keys, and table syntax
// therefore share the same normalized semantic paths instead of relying on
// lexical spellings. No caller-controlled key, section, value, or path is ever
// returned: maintenance refusals publish only fixed classifications.
func inspectGrokSystemConfigSemantic(path, grokVersion string) (grokSystemSemanticFinding, bool, error) {
	var finding grokSystemSemanticFinding
	if path == "" {
		return finding, false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return finding, false, nil
		}
		return finding, false, fmt.Errorf("grok system configuration is unreadable; refusing no-tools maintenance smoke")
	}
	defer f.Close()

	const maxBytes = 1 << 20
	if info, statErr := f.Stat(); statErr != nil {
		return finding, false, fmt.Errorf("grok system configuration cannot be inspected; refusing no-tools maintenance smoke")
	} else if info.Size() > maxBytes {
		return finding, false, fmt.Errorf("grok system configuration exceeds the inspection limit; refusing no-tools maintenance smoke")
	}
	data, readErr := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if readErr != nil {
		return finding, false, fmt.Errorf("grok system configuration cannot be inspected; refusing no-tools maintenance smoke")
	}
	if len(data) > maxBytes {
		return finding, false, fmt.Errorf("grok system configuration exceeds the inspection limit; refusing no-tools maintenance smoke")
	}
	var decoded map[string]any
	if _, decodeErr := toml.Decode(string(data), &decoded); decodeErr != nil {
		return finding, false, fmt.Errorf("grok system configuration cannot be parsed safely; refusing no-tools maintenance smoke")
	}
	effective, effectiveErr := effectiveGrokSystemConfigForVersion(decoded, grokVersion)
	if effectiveErr != nil {
		return finding, false, fmt.Errorf("grok system configuration version overrides cannot be evaluated safely; refusing no-tools maintenance smoke")
	}
	walkGrokSystemConfigSemantic(effective, nil, &finding)
	return finding, true, nil
}

// normalizeGrokConfigVersion extracts the semver reported by `grok --version`
// and normalizes it for x/mod comparisons. Grok's output is prefixed (for
// example, "grok 1.0.13"), while semver expects a leading v.
func normalizeGrokConfigVersion(raw string) string {
	match := semverRe.FindString(strings.TrimSpace(raw))
	if match == "" {
		return ""
	}
	version := "v" + match
	if !semver.IsValid(version) {
		return ""
	}
	return version
}

func normalizeGrokOverrideBound(value any) string {
	raw, ok := value.(string)
	if !ok {
		return ""
	}
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "v")
	if raw == "" {
		return ""
	}
	version := "v" + raw
	if !semver.IsValid(version) {
		return ""
	}
	return version
}

// effectiveGrokSystemConfigForVersion applies Grok's documented inclusive
// [[version_overrides]] patches before semantic classification. Each entry has
// required minimum_version/maximum_version selectors and carries its patch keys
// directly. Inactive historical/future entries are deliberately not walked;
// malformed selectors fail closed because their applicability is unknowable.
func effectiveGrokSystemConfigForVersion(decoded map[string]any, grokVersion string) (map[string]any, error) {
	effective := cloneGrokSemanticTable(decoded)
	var rawOverrides any
	overrideKeys := 0
	for rawKey, value := range effective {
		if normalizeGrokSemanticKey(rawKey) == "version_overrides" {
			rawOverrides = value
			delete(effective, rawKey)
			overrideKeys++
		}
	}
	if overrideKeys == 0 {
		return effective, nil
	}
	if overrideKeys != 1 {
		return nil, fmt.Errorf("ambiguous version overrides")
	}
	runtimeVersion := normalizeGrokConfigVersion(grokVersion)
	if runtimeVersion == "" {
		// The ACP policy preflight historically scans every entry conservatively
		// and does not know its CLI version. Preserve that behavior; maintenance
		// smokes always supply the probed installed version above.
		return decoded, nil
	}
	overrides, ok := grokVersionOverrideTables(rawOverrides)
	if !ok {
		return nil, fmt.Errorf("invalid version overrides")
	}
	for _, override := range overrides {
		var minimum, maximum string
		minimumKeys, maximumKeys := 0, 0
		patch := make(map[string]any, len(override))
		for rawKey, value := range override {
			switch normalizeGrokSemanticKey(rawKey) {
			case "minimum_version":
				minimumKeys++
				minimum = normalizeGrokOverrideBound(value)
			case "maximum_version":
				maximumKeys++
				maximum = normalizeGrokOverrideBound(value)
			default:
				patch[rawKey] = value
			}
		}
		if minimumKeys != 1 || maximumKeys != 1 || minimum == "" || maximum == "" || semver.Compare(minimum, maximum) > 0 {
			return nil, fmt.Errorf("invalid version override bounds")
		}
		if semver.Compare(runtimeVersion, minimum) < 0 || semver.Compare(runtimeVersion, maximum) > 0 {
			continue
		}
		mergeGrokSemanticTable(effective, patch)
	}
	return effective, nil
}

func grokVersionOverrideTables(value any) ([]map[string]any, bool) {
	switch entries := value.(type) {
	case []map[string]any:
		return entries, true
	case []any:
		out := make([]map[string]any, 0, len(entries))
		for _, entry := range entries {
			table, ok := entry.(map[string]any)
			if !ok {
				return nil, false
			}
			out = append(out, table)
		}
		return out, true
	default:
		return nil, false
	}
}

func cloneGrokSemanticTable(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		if table, ok := value.(map[string]any); ok {
			clone[key] = cloneGrokSemanticTable(table)
		} else {
			clone[key] = value
		}
	}
	return clone
}

// mergeGrokSemanticTable follows Grok's recursive config-patch behavior. TOML
// keys remain case-sensitive during the merge; the later classifier still
// normalizes every surviving spelling conservatively. Treating differently
// cased keys as the same merge target could let an ignored lookalike overwrite
// the real effective security setting before classification.
func mergeGrokSemanticTable(destination, patch map[string]any) {
	for patchKey, patchValue := range patch {
		destinationKey := patchKey
		patchTable, patchIsTable := patchValue.(map[string]any)
		destinationTable, destinationIsTable := destination[destinationKey].(map[string]any)
		if patchIsTable && destinationIsTable {
			mergeGrokSemanticTable(destinationTable, patchTable)
			continue
		}
		if patchIsTable {
			destination[destinationKey] = cloneGrokSemanticTable(patchTable)
		} else {
			destination[destinationKey] = patchValue
		}
	}
}

func normalizeGrokSemanticKey(key string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(key)), "-", "_")
}

func walkGrokSystemConfigSemantic(value any, path []string, finding *grokSystemSemanticFinding) {
	walkGrokSystemConfigSemanticScoped(value, path, false, finding)
}

// walkGrokSystemConfigSemanticScoped carries whether the current node lives
// underneath an explicitly disabled definition. Grok keeps a disabled server or
// plugin's full body in place — `[mcp_servers.foo]` with `enabled = false` still
// declares its command/args/env — so descending without that context makes every
// retained leaf look like a live tool and refuses maintenance smokes on hosts
// where nothing can actually load. Only the tool-category classification is
// suppressed; credential, provider, approval, and telemetry findings still fail
// closed inside a disabled block because those values remain readable.
func walkGrokSystemConfigSemanticScoped(value any, path []string, withinDisabled bool, finding *grokSystemSemanticFinding) {
	switch node := value.(type) {
	case map[string]any:
		for rawKey, child := range node {
			key := normalizeGrokSemanticKey(rawKey)
			childPath := append(append([]string(nil), path...), key)
			classifyGrokSystemSemanticValue(childPath, child, withinDisabled, finding)
			walkGrokSystemConfigSemanticScoped(child, childPath, withinDisabled || grokSemanticDefinitionDisabled(child), finding)
		}
	case []map[string]any:
		for _, child := range node {
			walkGrokSystemConfigSemanticScoped(child, path, withinDisabled || grokSemanticDefinitionDisabled(child), finding)
		}
	case []any:
		for _, child := range node {
			walkGrokSystemConfigSemanticScoped(child, path, withinDisabled || grokSemanticDefinitionDisabled(child), finding)
		}
	}
}

// grokSemanticDefinitionDisabled reports whether a configuration table carries an
// explicit boolean disablement (`enabled = false` / `disabled = true`). Only a
// literal boolean counts: Grok's managed layers support environment expansion, so
// `enabled = "$FLAG"` cannot be proven disabled lexically and must keep failing
// closed. Every enablement key present has to agree, so a contradictory pair
// (`enabled = false` next to `disabled = false`, or both true) is undecidable
// rather than off — and the verdict does not depend on map iteration order.
func grokSemanticDefinitionDisabled(value any) bool {
	node, ok := value.(map[string]any)
	if !ok {
		return false
	}
	disabled := false
	for rawKey, child := range node {
		flag, isBool := child.(bool)
		switch normalizeGrokSemanticKey(rawKey) {
		case "enabled":
			// Any enablement key that is not a literal `false` leaves the
			// definition unproven — an expanded string, or a contradicting
			// `enabled = true`, must not be read as switched off.
			if !isBool || flag {
				return false
			}
			disabled = true
		case "disabled":
			if !isBool || !flag {
				return false
			}
			disabled = true
		}
	}
	return disabled
}

func classifyGrokSystemSemanticValue(path []string, value any, withinDisabled bool, finding *grokSystemSemanticFinding) {
	if len(path) == 0 {
		return
	}
	last := path[len(path)-1]
	if grokSemanticExternalTelemetryEnabled(path, value) {
		// Grok's external OTEL stream is independent of product telemetry and can
		// be enabled from managed/requirements layers that GROK_HOME isolation
		// cannot hide. Reject with a fixed category: exporter endpoints,
		// certificate paths, and content gates can contain private values that must
		// never be reflected in a published maintenance error.
		finding.toolCategory = "telemetry"
		return
	}
	if last == "api_key" || last == "env_key" {
		if strings.TrimSpace(fmt.Sprint(value)) != "" {
			finding.credential = true
		}
	}
	// The maintenance smoke must use the copied xAI subscription login and
	// xAI's default provider. System requirements outrank the isolated home, so
	// any external auth command, endpoint/header override, or default-model pin
	// must fail closed even when it contains no literal api_key/env_key field.
	if semanticGrokValueNonEmpty(value) {
		switch {
		case last == "auth_provider_command":
			finding.externalProvider = true
		case (last == "issuer" || last == "client_id") && len(path) >= 3 &&
			path[len(path)-2] == "oidc" && path[len(path)-3] == "auth":
			finding.externalProvider = true
		case last == "base_url" || last == "api_base_url" || last == "models_base_url" || last == "models_list_url":
			finding.externalProvider = true
		case last == "extra_headers":
			finding.externalProvider = true
		case last == "default_model":
			finding.externalProvider = true
		case last == "default" && len(path) >= 2 && path[len(path)-2] == "models":
			finding.externalProvider = true
		}
	}
	if (last == "always_approve" || last == "auto_approve" || last == "yolo") && semanticGrokBool(value) {
		finding.permissiveApproval = true
	}
	if last == "permission_mode" || last == "approval_mode" || last == "approval" ||
		(last == "mode" && (len(path) == 1 || grokSemanticPathContains(path, "approval"))) {
		if isGrokPermissionModeBypassValue(fmt.Sprint(value)) {
			finding.permissiveApproval = true
		}
	}
	if grokSemanticAllowPath(path) && semanticGrokValueNonEmpty(value) {
		finding.permissiveApproval = true
	}
	if grokSemanticPermissionRulesPath(path) && semanticGrokPermissionRulesHasAllow(value) {
		finding.permissiveApproval = true
	}

	if withinDisabled {
		// Everything below decides whether a tool can load, and an ancestor has
		// already answered that with an explicit disablement.
		return
	}

	if grokSemanticVendorMCPPath(path) {
		if enabled, ok := value.(bool); !ok || enabled {
			finding.toolCategory = "vendor-mcp"
		}
		return
	}
	for _, component := range path {
		switch {
		case component == "mcp" || component == "mcps" || component == "mcpservers" ||
			strings.HasPrefix(component, "mcp_") || strings.HasPrefix(component, "mcpserver"):
			if finding.toolCategory == "" && semanticGrokToolEnabled(value) {
				finding.toolCategory = "mcp"
			}
		case component == "plugin" || component == "plugins" ||
			strings.HasPrefix(component, "plugin_") || strings.HasPrefix(component, "installed_plugin"):
			if finding.toolCategory == "" && semanticGrokToolEnabled(value) {
				finding.toolCategory = "plugin"
			}
		}
	}
}

// grokSemanticExternalTelemetryEnabled recognizes the external OpenTelemetry
// controls Grok 1.0.13 accepts under [telemetry]. Explicitly disabled values
// remain valid; anything that activates an exporter/content gate or supplies an
// external endpoint/certificate/key path fails closed. Non-boolean/non-string
// values are rejected because Grok's managed layers support environment
// expansion and their effective meaning cannot be proven safe lexically.
func grokSemanticExternalTelemetryEnabled(path []string, value any) bool {
	if len(path) < 2 || path[len(path)-2] != "telemetry" {
		return false
	}
	last := path[len(path)-1]
	switch last {
	case "otel_enabled", "otel_log_user_prompts", "otel_log_tool_details":
		enabled, ok := value.(bool)
		return !ok || enabled
	case "otel_metrics_exporter", "otel_logs_exporter":
		exporter, ok := value.(string)
		if !ok {
			return true
		}
		exporter = strings.TrimSpace(exporter)
		return exporter != "" && !strings.EqualFold(exporter, "none")
	case "otel_endpoint", "otel_certificate", "otel_client_certificate", "otel_client_key":
		return semanticGrokValueNonEmpty(value)
	default:
		return false
	}
}

func grokSemanticPathContains(path []string, want string) bool {
	for _, component := range path {
		if component == want {
			return true
		}
	}
	return false
}

func grokSemanticVendorMCPPath(path []string) bool {
	n := len(path)
	return n >= 3 && path[n-3] == "compat" &&
		(path[n-2] == "cursor" || path[n-2] == "claude") && path[n-1] == "mcps"
}

func grokSemanticAllowPath(path []string) bool {
	if len(path) == 0 {
		return false
	}
	last := path[len(path)-1]
	if last == "allow_rules" || last == "allowlist" {
		return true
	}
	if last != "allow" || grokSemanticPathWithinPermissionRules(path) {
		return false
	}
	// A key literally named `allow` at any ordinary configuration scope is an
	// allow list. The exception is a field nested inside permission_rules: in
	// that structured grammar the rule's `action` decides allow vs deny, and a
	// matcher field must not turn a deny-only policy into a false positive.
	return true
}

func grokSemanticPathWithinPermissionRules(path []string) bool {
	for i, component := range path {
		if component == "permission_rules" {
			return true
		}
		if component == "rules" && i > 0 && path[i-1] == "permission" {
			return true
		}
	}
	return false
}

func grokSemanticPermissionRulesPath(path []string) bool {
	if len(path) == 0 {
		return false
	}
	if path[len(path)-1] == "permission_rules" {
		return true
	}
	return len(path) >= 2 && path[len(path)-2] == "permission" && path[len(path)-1] == "rules"
}

func semanticGrokPermissionRulesHasAllow(value any) bool {
	switch node := value.(type) {
	case map[string]any:
		for rawKey, child := range node {
			if normalizeGrokSemanticKey(rawKey) == "action" && strings.EqualFold(strings.TrimSpace(fmt.Sprint(child)), "allow") {
				return true
			}
			// A string nested inside a structured rule is commonly a tool or
			// pattern value, including on deny-only rules. Only recurse into
			// additional containers here; bare strings are legacy allow rules
			// only when they are direct entries in the rules value/array.
			switch child.(type) {
			case map[string]any, []map[string]any, []any:
				if semanticGrokPermissionRulesHasAllow(child) {
					return true
				}
			}
		}
	case []map[string]any:
		for _, child := range node {
			if semanticGrokPermissionRulesHasAllow(child) {
				return true
			}
		}
	case []any:
		for _, child := range node {
			if semanticGrokPermissionRulesHasAllow(child) {
				return true
			}
		}
	case string:
		// Legacy string permission_rules entries are allow rules; structured
		// deny rules use an explicit action field and are handled above.
		return strings.TrimSpace(node) != ""
	}
	return false
}

func semanticGrokValueNonEmpty(value any) bool {
	switch node := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(node) != ""
	case []any:
		return len(node) > 0
	case []map[string]any:
		return len(node) > 0
	case map[string]any:
		return len(node) > 0
	default:
		return true
	}
}

func semanticGrokBool(value any) bool {
	enabled, ok := value.(bool)
	return ok && enabled
}

func semanticGrokToolEnabled(value any) bool {
	switch node := value.(type) {
	case nil:
		return false
	case bool:
		return node
	case string:
		return strings.TrimSpace(node) != ""
	case []any:
		for _, child := range node {
			if semanticGrokToolEnabled(child) {
				return true
			}
		}
		return false
	case []map[string]any:
		for _, child := range node {
			if semanticGrokToolEnabled(child) {
				return true
			}
		}
		return false
	case []string:
		for _, child := range node {
			if strings.TrimSpace(child) != "" {
				return true
			}
		}
		return false
	case map[string]any:
		if len(node) == 0 {
			return false
		}
		if grokSemanticDefinitionDisabled(node) {
			// A retained definition body (command/args/env/path) does not make a
			// disabled entry loadable; the explicit flag outranks its siblings.
			return false
		}
		hasExplicitEnablement := false
		isEnabled := false
		hasOtherDefinitions := false
		for rawKey, child := range node {
			key := normalizeGrokSemanticKey(rawKey)
			switch key {
			case "enabled":
				hasExplicitEnablement = true
				if b, ok := child.(bool); ok {
					if b {
						isEnabled = true
					}
				} else if semanticGrokToolEnabled(child) {
					isEnabled = true
				}
			case "disabled":
				hasExplicitEnablement = true
				if b, ok := child.(bool); ok {
					if !b {
						isEnabled = true
					}
				}
			default:
				if semanticGrokToolEnabled(child) {
					hasOtherDefinitions = true
				}
			}
		}
		if hasExplicitEnablement && !isEnabled && !hasOtherDefinitions {
			return false
		}
		return isEnabled || hasOtherDefinitions
	default:
		return true
	}
}

// detectPinnedSystemGrokRequirementsFile is the per-path scanner that backs
// detectPinnedSystemGrokRequirements. Split out so the system layers can be
// iterated cleanly and so tests can target a single path. Missing/unreadable/
// empty path ⇒ nil (best-effort).
func detectPinnedSystemGrokRequirementsFile(path string, allowAPIKey, allowAlwaysApprove bool) error {
	if path == "" {
		return nil
	}
	// Decode semantic paths first so valid inline tables and quoted keys are
	// classified identically to their section/dotted-key equivalents. Preserve
	// this ACP preflight's historical best-effort behavior on unreadable or
	// malformed files; the stricter maintenance smoke calls the same inspector
	// directly and fails closed on those errors.
	if finding, ok, semanticErr := inspectGrokSystemConfigSemantic(path, ""); semanticErr == nil && ok {
		if !allowAPIKey && finding.credential {
			return fmt.Errorf("grok requirements pin API-key auth in %s; refusing to spawn — set Config.EnableGrokAPIKeyFallback=true to opt in, or remove the pinned credential", path)
		}
		if !allowAlwaysApprove && finding.permissiveApproval {
			return fmt.Errorf("grok requirements pin a permissive approval policy in %s; refusing to spawn — set Config.EnableGrokAlwaysApprove=true to opt in, or remove the pinned policy", path)
		}
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
	return claudeManagedSettingsPathsForOS(runtime.GOOS, os.Getenv)
}

// claudeManagedSettingsPathsForOS keeps the production path mapping directly
// testable. Claude Code's current Windows managed-policy directory is under
// Program Files; the retired ProgramData location is deliberately excluded
// because current Claude releases no longer read it.
func claudeManagedSettingsPathsForOS(goos string, getenv func(string) string) []string {
	paths := make([]string, 0, 1)
	switch goos {
	case "darwin":
		paths = append(paths, "/Library/Application Support/ClaudeCode/managed-settings.json")
	case "windows":
		programFiles := strings.TrimSpace(getenv("ProgramFiles"))
		if programFiles == "" {
			programFiles = `C:\Program Files`
		}
		paths = append(paths, filepath.Join(programFiles, "ClaudeCode", "managed-settings.json"))
	default:
		paths = append(paths, "/etc/claude-code/managed-settings.json")
	}
	return paths
}

// inspectClaudeManagedSettingsAllowRule reports whether the Claude
// `managed-settings.json` at `path` contains a non-empty `permissions.allow`
// array. A missing path is benign, but an existing file that cannot be read in
// full or decoded must fail closed: Grok may still consume a policy that this
// preflight otherwise skipped. Errors are fixed strings and never include the
// managed path or file contents.
func inspectClaudeManagedSettingsAllowRule(path string) (bool, error) {
	if path == "" {
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("claude managed settings cannot be read safely")
	}
	defer f.Close()
	const maxBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return false, fmt.Errorf("claude managed settings cannot be read safely")
	}
	if len(data) > maxBytes {
		return false, fmt.Errorf("claude managed settings exceed the inspection limit")
	}
	var parsed struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return false, fmt.Errorf("claude managed settings cannot be parsed safely")
	}
	for _, rule := range parsed.Permissions.Allow {
		if strings.TrimSpace(rule) != "" {
			return true, nil
		}
	}
	return false, nil
}

// grokTOMLStripInlineComment removes a trailing `# ...` comment from a TOML
// line, honoring `"..."` / `'...'` string contents so a `#` inside a quoted
// pattern (`pattern = "Bash(#magic)"`) is preserved. Without this the line-
// oriented requirements scanner would corrupt valid pinned values that
// embed `#` in a pattern literal — silently letting them route past the
// approval gate.
//
// TRIPLE-quoted strings are recognised BEFORE the single-quote toggles, because
// a same-line multiline value whose body contains an odd number of quote
// characters (`note = """contains " # text"""`) would otherwise leave the
// toggle "outside a string" at the `#` and take the real closing delimiter with
// the comment. walkGrokTOMLAssignments then reads the mutilated line as an
// UNCLOSED multiline opener, runs to EOF with `openMultiline` set and reports
// the credential scan incomplete — so grokConfigPinsCredential contests a
// cached-login run over a config that pins nothing. A triple-quoted body is
// content, so no `#` inside one is a comment; when the body does not close on
// this line the rest of the line is body and nothing is stripped.
func grokTOMLStripInlineComment(line string) string {
	inDouble := false
	inSingle := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if !inDouble && !inSingle {
			if delim := grokTOMLMultilineDelimiterAt(line[i:]); delim != "" {
				body := line[i+len(delim):]
				closer, closerLen := grokTOMLMultilineCloserSpan(body, delim)
				if closer < 0 {
					// The body runs past this line: everything after the
					// opener is string content, never a comment.
					return line
				}
				i += len(delim) + closer + closerLen - 1
				continue
			}
		}
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

// grokTOMLCompositeLineDepth counts the net bracket depth one line contributes
// to a composite value (a multi-line array, or an array of inline tables) while
// carrying multiline STRING state across lines, and returns the delimiter of a
// string body still running at the end of the line ("" when none is).
//
// grokTOMLBracketDepth alone is not enough for a composite BODY. A multiline
// string nested inside an array is valid TOML, and its body is text: a `]` in it
// is not the array's close, and a following section-shaped line like `[other]`
// is not a table. Counting the string's `]` ended the composite early, so that
// body line re-scoped every FOLLOWING assignment — a root `model.api_key` read
// as `other.model.api_key`, which the attribution guard sees as no pinned
// credential for a run grok's own parser bills by API key.
//
// `openMultiline` is the delimiter already running when the line starts; the
// scan resumes after its close and only then reads the rest of the line as
// value syntax.
func grokTOMLCompositeLineDepth(s, openMultiline string) (string, int, string) {
	i := 0
	if openMultiline != "" {
		idx, closerLen := grokTOMLMultilineCloserSpan(s, openMultiline)
		if idx < 0 {
			// The whole line is still string body.
			return s, 0, openMultiline
		}
		i = idx + closerLen
		openMultiline = ""
	}
	depth := 0
	inDouble := false
	inSingle := false
	for ; i < len(s); i++ {
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
		case '"', '\'':
			delim := strings.Repeat(string(c), 3)
			if strings.HasPrefix(s[i:], delim) {
				rest := s[i+len(delim):]
				closed, closerLen := grokTOMLMultilineCloserSpan(rest, delim)
				if closed < 0 {
					// Everything after this opener is body, on this line and
					// on the ones that follow.
					return s, depth, delim
				}
				// Resume at the first byte after the closing delimiter; the
				// loop's own i++ carries it there.
				i += len(delim) + closed + closerLen - 1
				continue
			}
			if c == '"' {
				inDouble = true
			} else {
				inSingle = true
			}
		case '#':
			// Outside every string body a `#` starts a comment, and the rest
			// of the line is not value syntax. Stripping it HERE rather than
			// before the scan is what makes a comment on the line that CLOSES
			// a multiline string count as a comment: the caller cannot strip
			// that line up front (a `#` before the closing delimiter is body),
			// and leaving it unstripped let a trailing `# ]` close the
			// composite early, so the next element line was read as a table
			// header and every following root key was re-scoped.
			return strings.TrimRight(s[:i], " \t"), depth, ""
		case '[':
			depth++
		case ']':
			depth--
		}
	}
	return s, depth, ""
}

// grokTOMLCompositeMaxContinuationLines bounds how far walkGrokTOMLAssignments
// will follow one composite value. Generous for anything a human writes, finite
// so a corrupted file with no closing `]` cannot make one assignment consume the
// whole scan. Past the bound the sweep reports INCOMPLETE rather than resuming
// mid-body, because resuming would read value body as configuration — the
// re-scoping bug this accumulation exists to avoid.
const grokTOMLCompositeMaxContinuationLines = 256

// accumulateGrokTOMLCompositeValue joins the continuation lines of a composite
// value into one logical right-hand side, carrying both the bracket depth and
// any multiline STRING body across lines. Reports false when the value neither
// closes within grokTOMLCompositeMaxContinuationLines nor before EOF.
func accumulateGrokTOMLCompositeValue(
	scanner *bufio.Scanner,
	initial string,
	depth int,
	openMultiline string,
) (string, bool) {
	parts := []string{initial}
	for i := 0; i < grokTOMLCompositeMaxContinuationLines && scanner.Scan(); i++ {
		// Comment stripping is left to grokTOMLCompositeLineDepth, the only
		// scan that knows where the value syntax on this line actually starts.
		// Pre-stripping was wrong inside a string body (a `#` there is
		// content), and skipping the strip for the WHOLE line whenever it
		// began in one was wrong once the delimiter closed mid-line: a
		// trailing `# ]` after the close was then counted as the composite's
		// bracket.
		line := strings.TrimSpace(scanner.Text())
		var delta int
		line, delta, openMultiline = grokTOMLCompositeLineDepth(line, openMultiline)
		depth += delta
		parts = append(parts, line)
		if depth <= 0 && openMultiline == "" {
			return strings.Join(parts, " "), true
		}
	}
	return "", false
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
	if (key == "approval" || strings.HasSuffix(key, ".approval")) && strings.HasPrefix(strings.TrimSpace(val), "{") {
		compact := strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "", "-", "_").Replace(val)
		for _, bypass := range []string{`mode="always"`, `mode='always'`, `mode="auto"`, `mode='auto'`,
			`mode="always_approve"`, `mode='always_approve'`, `mode="auto_approve"`, `mode='auto_approve'`} {
			if strings.Contains(compact, bypass) {
				return true
			}
		}
	}
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

// setEnvVar returns env with the `KEY=value` entry for key replaced or appended
// when absent. Windows environment keys are case-insensitive, so every casing
// of key must be removed there; retaining a differently-cased inherited entry
// would leave CreateProcess to choose between conflicting GROK_HOME or junction
// values. Unix keeps its native case-sensitive semantics.
func setEnvVar(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, e := range env {
		separator := strings.IndexByte(e, '=')
		matches := separator >= 0 && e[:separator] == key
		if runtime.GOOS == "windows" && separator >= 0 {
			matches = strings.EqualFold(e[:separator], key)
		}
		if matches {
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
//     duplicate entry tokens (`agent`/`stdio`/`chat`/`tui`/`run`) and the POSIX
//     `--` end-of-options delimiter. Root-only one-shot flags (`--tools`,
//     `--max-turns`, prompt/output selectors, etc.) are rejected rather than
//     stripped: silently dropping `--tools ""` would turn a requested no-tools
//     smoke into a fully tooled ACP session. `--allow <pattern>` / `--allow=…`
//     are xAI's documented pre-prompt allow rules (matching tools auto-approve
//     BEFORE the per-tool prompt runs) — stripped when allowAlwaysApprove is
//     false, mirroring the raw `session_start` path's stripGrokAllowRulePairs
//     sweep so a signed grok_acp_start cannot route around the per-tool prompt
//     by handing `--allow Bash(*)` through extras. `--deny` is policy-tightening
//     and is preserved on both sides of the gate.
func sanitizeGrokACPExtraArgs(extraArgs []string, defaultModel string, allowAlwaysApprove bool) (string, []string, error) {
	if arg, ok := grokACPRootOnlyArg(extraArgs); ok {
		return defaultModel, nil, fmt.Errorf(
			"grok agent stdio does not support root-only option %q; use session_start for grok -p/no-tools smoke invocations",
			arg,
		)
	}
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
	return model, cleaned, nil
}

// grokACPRootOnlyArg reports the first root-command option that cannot be
// represented by `grok agent stdio`. These options must fail closed instead of
// being silently discarded, especially `--tools ""`: discarding that operand
// changes a tool-free smoke into an ordinary ACP session with built-in tools.
func grokACPRootOnlyArg(args []string) (string, bool) {
	valueFlags := map[string]bool{
		"--tools": true, "--disallowed-tools": true, "--max-turns": true,
		"--agent": true, "--agents": true, "--output-format": true,
		"--json-schema": true, "--prompt-file": true, "--prompt-json": true,
		"--rules": true, "--system-prompt-override": true, "--sandbox": true,
		"--worktree-ref": true, "--ref": true, "-p": true, "--single": true,
	}
	boolFlags := map[string]bool{
		"--disable-web-search": true, "--no-subagents": true, "--no-plan": true,
		"--verbatim": true, "--include-partial-messages": true,
		"--fork-session": true, "--restore-code": true,
	}
	for _, arg := range args {
		lower := strings.ToLower(arg)
		if valueFlags[lower] || boolFlags[lower] {
			return arg, true
		}
		if eq := strings.IndexByte(lower, '='); eq > 0 && valueFlags[lower[:eq]] {
			return arg[:eq], true
		}
	}
	return "", false
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
// `params.cwd` to resolve inside root — the session's own symlink-resolved
// start cwd — so the in-protocol session cwd cannot re-point the session
// outside the directory the start named.
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
		return fmt.Errorf("%s params.cwd %q is outside the session's workspace root %q (the directory the session was started in)", probe.Method, resolved, resolvedRoot)
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
