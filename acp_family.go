// File: acp_family.go
// -----------------------------------------------------------------------------
// ACPManager — the ACP family's long-lived drivers: one child process per
// session, speaking the Agent Client Protocol (JSON-RPC 2.0, one JSON object
// per line) over its stdio. Three coding agents run on it, each through its
// own acpAgentProfile and its own manager instance:
//
//   - Grok Build: `grok agent stdio` (grok_acp.go; always this driver).
//   - OpenCode:   `opencode acp`, OpenCode 2.2.7 and newer (acp_profiles.go).
//   - Antigravity: `agy acp`, agy 1.5.4 and newer (acp_profiles.go), only
//     while antigravityACPDriverIncluded says so.
//
// The transport, framing, fail-fast policy, lifecycle and cleanup below were
// GrokACPManager's, and are unchanged for Grok. What differs per agent lives
// in the profile: how the binary is found and launched (argv, env, version
// gate), the result frame types, which client frames are allowed through
// (acp_fence.go), and the agent's own housekeeping hooks (Grok's isolated
// home and billing snapshot).
//
//   - We only enforce wire framing (a single JSON object per line, no embedded
//     newlines, never silently drop a frame). The JSON-RPC semantics —
//     `initialize`, `session/new`, `session/prompt`, `session/update`
//     streaming, approval answers — live in terminal-service, which builds
//     every frame. Two narrow exceptions read the envelope of the frames going
//     both ways, never their content:
//       - the client-frame fence (acp_fence.go), OpenCode and Antigravity
//         only: a frame outside the family's allowlist, or a working
//         directory outside the session's folder, is refused before it
//         reaches the CLI;
//       - a live follow-up (relay_follow_up_jsonrpc.go): the device writes
//         that request itself, under its follow-up ledger, so a small tracker
//         pairs the ids the device chose and keeps the open client prompts
//         and permission requests and the ACP session the child is on. It
//         exists only on a session launched from a version certified for
//         live follow-ups.
//
//   - Frames are forwarded VERBATIM as `<kind>_acp_message` results. Non-JSON
//     lines become `<kind>_acp_error` so terminal-service's state machine
//     sees a clear failure instead of misreading a malformed line. Grok's
//     consecutive reply chunks are merged first (grok_acp_chunk_merge.go);
//     a Grok prompt answer can be held while a stranded interjection runs
//     (L1/L2 spec §2.8). Two optional fields can ride on a forwarded frame's
//     result (never in its text): `acpHandoverTo` and `acpSteerOutcome`
//     (relay_follow_up_jsonrpc.go).
//
//   - Stderr is surfaced as `<kind>_acp_stderr` for diagnostics but is not
//     protocol-critical.
//
// Lifecycle: Start spawns the child, registers it in the global process
// registry, and launches stdout + stderr reader goroutines. End closes stdin
// first (ACP's documented graceful-shutdown path) and falls through to
// interrupt + kill on timeout. waitForExit publishes a `<kind>_acp_ended`
// frame and removes the session from the manager.
// -----------------------------------------------------------------------------

package main

import (
	"bufio"
	"encoding/json"
	"errors"
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
	// acpFamilyMaxLineSize caps the bufio.Scanner buffer for a single JSONL
	// frame. Matches the Codex manager's ceiling — session.go's CLI stream
	// scanner uses the same 30 MB cap. The downstream Pub/Sub message limit
	// is 10 MB, so frames bigger than acpFamilyMaxFrameSize cannot survive a
	// publish and are session-fatal.
	acpFamilyMaxLineSize = 30 * 1024 * 1024

	// acpFamilyMaxFrameSize is a cheap pre-check on the raw stdout line.
	// Frames bigger than this cannot possibly fit Pub/Sub's 10 MB envelope
	// before JSON escaping, so we reject them without building a resultMsg.
	// The authoritative gate is acpFamilyMaxPublishSize below — the Output
	// field is JSON-string-escaped on marshal, so a frame heavy in '"' / '\'
	// can roughly double in size, meaning a line that passes this pre-check
	// can still fail the marshaled-envelope check. Frames that fail either
	// check are session-fatal — silently dropping one would deadlock the
	// orchestrator's JSON-RPC state machine waiting for a response that never
	// arrives.
	acpFamilyMaxFrameSize = 8 * 1024 * 1024

	// acpFamilyMaxPublishSize is GCP Pub/Sub's documented per-message publish
	// limit. After building each resultMsg, the stream reader marshals it and
	// rejects envelopes that exceed this ceiling.
	acpFamilyMaxPublishSize = 10_000_000

	// acpFamilyStdinWriteTimeout is the upper bound on a single stdin write
	// before we declare the pipe stalled. Matches session.go's SendInput
	// budget.
	acpFamilyStdinWriteTimeout = 10 * time.Second

	// acpFamilyGracefulShutdownTimeout is how long End waits after closing
	// stdin (ACP's documented exit path) before escalating to SIGINT and
	// ultimately SIGKILL.
	acpFamilyGracefulShutdownTimeout = 5 * time.Second

	// acpFamilyStreamDrainTimeout is how long waitForExit waits for the
	// stdout/stderr readers to finish draining buffered frames before
	// publishing the ended frame. Generous to avoid racing the last JSON-RPC
	// response with the exit notification.
	acpFamilyStreamDrainTimeout = 30 * time.Second

	// acpFamilyMaxLifetime caps how long a session may stay open before
	// CleanupStale ends it. Matches SessionManager's 6 h ceiling so an
	// orchestrator crash can't leak children indefinitely (each child holds a
	// signed-in CLI and may keep billing).
	acpFamilyMaxLifetime = 6 * time.Hour

	// acpFamilyCleanupInterval is how often the stale cleanup goroutine scans
	// for expired sessions.
	acpFamilyCleanupInterval = 60 * time.Second

	// acpFamilyPublishQueueSize bounds the per-session publish backlog. Every
	// ACP frame is a stateful JSON-RPC message (request id, session/update
	// notification, approval request) — losing one corrupts the session, so
	// frames are enqueued in order and drained by a single publisher
	// goroutine. If the queue fills we surface a fatal error and kill the
	// child rather than silently dropping.
	acpFamilyPublishQueueSize = 256

	// acpFamilyEnqueueTimeout is the upper bound for the stream readers to
	// wait when the publish queue is full. Hitting this means Pub/Sub has
	// stalled for a sustained period; the session cannot continue safely so
	// the manager publishes an error frame and force-kills the child to
	// fail-fast.
	acpFamilyEnqueueTimeout = 30 * time.Second

	// acpFamilyFirstFrameTimeout bounds how long we wait, AFTER the started
	// ack publish completes, for the CLI to emit its FIRST stdout frame. A
	// healthy ACP child answers terminal-service's `initialize` request within
	// about a second, so the first message frame normally lands a second or
	// two after the ack (one Pub/Sub round-trip for terminal-service to send
	// `initialize`). A child that stays completely silent past this window is
	// almost always blocked needing INTERACTIVE re-authentication it cannot
	// present over headless stdio. The dispatcher arms this timer via
	// ArmFirstFrameWatchdog after the ack publish so the up-to-30s
	// newSessionPublishFn timeout never eats into the budget.
	acpFamilyFirstFrameTimeout = 45 * time.Second
)

/* --------------------------------------------------------------------------
   Profiles — what differs per coding agent
   -------------------------------------------------------------------------- */

// acpFrameTypes are one agent's device command or result frame types.
type acpCommandTypes struct{ Start, Send, End string }

type acpResultTypes struct{ Started, Message, Stderr, Error, Ended string }

// ACPStartOptions bundles the per-session policy knobs the dispatcher reads
// from Config + commandMsg before spawning an ACP child. Bundled rather than
// threading 4+ positional args so future policy additions don't churn every
// call site. The two Allow* knobs are Grok's (its profile reads them); the
// OpenCode and Antigravity profiles ignore them.
type ACPStartOptions struct {
	// TimeoutMs is the backend-requested per-session deadline. 0 means
	// "no deadline" and the session lives until the 6h stale GC, End(),
	// orchestrator-driven cancellation, or the child's natural exit. Values
	// above acpFamilyMaxLifetime are clamped to it — a runaway orchestrator
	// can't request a longer session than our GC would tolerate anyway.
	TimeoutMs int64

	// AllowAPIKeyFallback (Grok), when false, strips XAI_API_KEY from the
	// child env and any `--api-key*` / `--auth*` extra args. Default false
	// enforces the feature-brief invariant that API-key auth is OPT-IN only —
	// otherwise a user with `export XAI_API_KEY=...` in their shell rc would
	// silently bill their xAI API wallet for every Grok session this
	// integration launches. Sourced from Config.EnableGrokAPIKeyFallback.
	AllowAPIKeyFallback bool

	// AllowAlwaysApprove (Grok), when false, strips `--always-approve` /
	// `--auto-approve` (and equivalent `-c approval.mode=always|auto` /
	// `-c tools.always_approve=true` / `-c tools.auto_approve=true` config
	// overrides) from the spawn argv. Default false enforces the feature
	// brief's conservative approval posture — autonomous tool execution
	// must be an explicit per-workspace opt-in, not something a signed
	// `grok_acp_start` can flip via extra args. Sourced from
	// Config.EnableGrokAlwaysApprove.
	AllowAlwaysApprove bool
}

// acpLaunchRequest is what a profile's prepare receives: the session id, the
// caller's start cwd and the argv buildArgs produced.
type acpLaunchRequest struct {
	ID   string
	Cwd  string
	Args []string
	Opts ACPStartOptions
}

// acpLaunch is a profile's answer to prepare: the binary, its environment,
// the version it was read at, and the agent's own pre-spawn state.
type acpLaunch struct {
	executable string
	env        []string
	// version / versionKey are the binary's version as read before the spawn
	// and the identity it was read from (cliLaunchVersion), confirmed once
	// the child has started (confirmCliLaunchVersion).
	version    string
	versionKey versionProbeKey

	// Grok only: the isolated GROK_HOME and its housekeeping (grok_acp.go).
	isolatedHome      string
	persistentHome    string
	limitNoticeScope  grokLimitNoticeScope
	producerContested bool

	// cleanup undoes prepare when the child is never spawned.
	cleanup func()
	// armSpawn runs right before the spawn (Grok: its usage run floor). It
	// returns how to undo it when the spawn fails, and how to bind it to the
	// session once one exists. nil: nothing to arm.
	armSpawn func() (undo func(), bind func(*ACPSession))
}

// acpAgentProfile is one coding agent's driver on the ACP family.
type acpAgentProfile struct {
	// cliAgentID is shared-constants CLI_AGENT_IDS (`grok`, `opencode`,
	// `antigravity`).
	cliAgentID string
	// noun names a session in every error the device publishes ("<noun>
	// session <id> not found", "failed to start <noun>: …"): terminal-service
	// matches both shapes (RESIDENT_START_REFUSAL_RE, sessionAbsentOnDevice).
	noun string
	// startNoun names the launch in the start-time errors.
	startNoun string
	// cliName is the CLI's own name in diagnostics ("grok emitted a …").
	cliName string
	// startedOutput is the started ack's text.
	startedOutput string
	// logTag prefixes every log line; processLabel tags the process registry.
	logTag       string
	processLabel string

	commandTypes acpCommandTypes
	resultTypes  acpResultTypes

	// mergeReplyChunks: consecutive reply chunks are merged before they are
	// published (grok_acp_chunk_merge.go). Grok only; every other agent's
	// frames are forwarded one line per frame.
	mergeReplyChunks bool

	// buildArgs turns the START's args into the child's argv.
	buildArgs func(extraArgs []string, opts ACPStartOptions) ([]string, error)
	// redactArgs is what the start log shows of that argv.
	redactArgs func([]string) []string
	// prepare resolves the binary and builds the launch (version gate, env).
	prepare func(req acpLaunchRequest) (acpLaunch, error)
	// spawnFailed is the error a failed spawn returns.
	spawnFailed func(err error) error
	// fenceFrames: every client frame goes through the family's allowlist
	// (acp_fence.go). Off for Grok, whose frames are fenced only on their
	// session-setup cwd (validateGrokACPSendCwd), as they always were.
	fenceFrames bool
	// onStdoutFrame sees every valid stdout frame before it is published.
	onStdoutFrame func(s *ACPSession, line string)
	// onExit runs once the child has exited, before the ended frame.
	onExit func(s *ACPSession)
	// afterEnded runs once the ended frame is out.
	afterEnded func(s *ACPSession)
	// endedOutput is the ended frame's text.
	endedOutput func(exit int) string
	// firstFrameStall is the error a silent child is failed with.
	firstFrameStall func(timeout time.Duration) string
}

// typePrefix is the stem of the profile's frame types (`grok_acp`), used in
// log labels only.
func (p *acpAgentProfile) typePrefix() string {
	return strings.TrimSuffix(p.resultTypes.Message, "_message")
}

// acpFrameAnnotation is what the device adds to a forwarded MESSAGE result,
// beside the CLI's verbatim line (shared-constants ACP_HANDOVER_FIELD /
// ACP_STEER_OUTCOME_FIELD). Empty for every frame but the two it describes.
type acpFrameAnnotation struct {
	// handoverTo (Antigravity): on the answer to a prompt P that a follow-up
	// prompt the device wrote took over, that prompt's id.
	handoverTo string
	// steerOutcome (OpenCode): on the answer to a device-written
	// `_session/steering`, the follow-up ledger's outcome for it.
	steerOutcome string
}

/* --------------------------------------------------------------------------
   ACPSession — one running child
   -------------------------------------------------------------------------- */

// ACPSession holds a single child process and its stdio pipes. All public
// state is guarded by mu; stdin writes are serialised separately by stdinMu
// so concurrent Send callers cannot interleave JSONL frames.
type ACPSession struct {
	ID          string
	Process     *exec.Cmd
	Stdin       io.WriteCloser
	Stdout      io.ReadCloser
	Stderr      io.ReadCloser
	StartedAt   time.Time
	WorkspaceID string
	UID         string
	TimeoutMs   int64 // 0 = no per-session timeout (rely on acpFamilyMaxLifetime stale GC)
	// WorkspaceRoot is the session's own symlink-resolved start cwd. Send uses
	// it to pin later JSON-RPC session-setup frames whose `params.cwd` could
	// otherwise re-point the session anywhere, so "the session runs where the
	// start said" is the invariant this field enforces. It is deliberately NOT
	// the agent's configured WorkingDirectory — comparing a server-derived cwd
	// against that unrelated root is the two-sources-of-truth drift that
	// refused every repo checked out outside the device home (the same defect
	// class as terminal-service §8.5). Always set on a started session.
	WorkspaceRoot string
	// IsolatedHome (Grok) is the per-session temp dir Start points the child's
	// GROK_HOME at: copied auth, a minimal clean config, a persistent sessions
	// link, and a session-private billing log. It is removed through
	// removeIsolatedGrokHome exactly once after the child exits, which unlinks
	// the persistent store before deleting the ephemeral remainder. Always set
	// on a successfully started Grok session because Start fails closed when
	// isolation cannot be built.
	IsolatedHome string
	// PersistentHome (Grok) is the real Grok home captured alongside the auth
	// copy. waitForExit merges only the session's normalized account-bound
	// billing snapshot here; the child never receives this path as its log
	// directory.
	PersistentHome string
	// limitNoticeScope (Grok) is the account this session's limit notices may
	// be cached under, read ONCE from IsolatedHome at Start. The child bills
	// the login copied into that home for its whole life, so a `grok login`
	// against PersistentHome mid-session must not re-file this session's
	// notices under the new account.
	limitNoticeScope grokLimitNoticeScope
	// producerContested (Grok) freezes, from that same credential surface,
	// whether the child may bill an account the copied login does not name.
	// waitForExit refuses to merge this session's billing record when it is
	// set — the billing twin of limitNoticeScope's refusal to cache its
	// notices.
	producerContested bool
	// grokUsageFloorMs is the run floor armed before the child spawned
	// (armGrokUsageRunFloor); grokUsageSettled makes its settle happen once,
	// whichever exit path gets there first.
	grokUsageFloorMs atomic.Int64
	grokUsageSettled atomic.Bool
	// launchedVersion is the major.minor.patch of the binary this child was
	// spawned from ("" when it could not be established). Live follow-ups
	// are gated on it, never on the version installed now. Set once at Start.
	launchedVersion string
	// liveFollowUps is the live follow-up tracker (relay_follow_up_jsonrpc.go),
	// nil unless launchedVersion is certified. Set once at Start.
	liveFollowUps *liveFollowUpTracker
	// fence is the client-frame fence (acp_fence.go), nil unless the
	// profile fences frames. Set once at Start.
	fence *acpClientFence

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
	// stdout reader sees the child's first frame. The first-frame watchdog
	// (watchFirstFrame) selects on it to disarm itself the instant the child
	// proves it is alive and producing output.
	firstFrame     chan struct{}
	firstFrameOnce sync.Once
	// killUnconfirmed marks a session whose End escalated to Kill and then
	// timed out waiting for the exit watcher. The session is RETAINED as a
	// tombstone (see end_confirm.go): only probeProcessGone may convert it
	// into the "not found" absence answer the server frees a device on.
	killUnconfirmed bool
	// terminalPublishState reserves this session's ID while its ended frame
	// is in flight — see end_confirm.go.
	terminalPublishState
}

// Status returns the current lifecycle status under the session mutex so
// callers don't race with waitForExit's transition to "ended".
func (s *ACPSession) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// closeStdin is idempotent: the End fallback path calls it after stdin may
// already have been closed by waitForExit, and the second close would
// otherwise return an `io: already closed` error.
func (s *ACPSession) closeStdin() {
	s.stdinClose.Do(func() {
		_ = s.Stdin.Close()
	})
}

func (s *ACPSession) isKillUnconfirmed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.killUnconfirmed
}

func (s *ACPSession) markKillUnconfirmed() {
	s.mu.Lock()
	s.killUnconfirmed = true
	s.mu.Unlock()
}

// signalFirstFrame records that the child has emitted its first stdout frame.
// It is idempotent (sync.Once) so the per-line hot path in the stdout reader
// can call it unconditionally without a guard. Closing the channel — rather
// than setting a flag — lets the first-frame watchdog block on it directly
// and wake the instant the child proves it is alive.
func (s *ACPSession) signalFirstFrame() {
	s.firstFrameOnce.Do(func() {
		close(s.firstFrame)
	})
}

/* --------------------------------------------------------------------------
   ACPManager — tracks every active session of one agent
   -------------------------------------------------------------------------- */

// ACPManager owns the active children of ONE agent (its profile). One manager
// handles many concurrent sessions, mirroring SessionManager's shape.
type ACPManager struct {
	sessions map[string]*ACPSession
	// Config is needed at session end to scan for and upload the media the
	// session produced (see collectSessionArtifacts). Without it this
	// manager silently drops every artifact its CLI wrote.
	Config  *Config
	profile *acpAgentProfile
	mu      sync.RWMutex
}

// newACPManager creates a fresh manager for one agent's profile.
func newACPManager(cfg *Config, profile *acpAgentProfile) *ACPManager {
	return &ACPManager{
		sessions: make(map[string]*ACPSession),
		Config:   cfg,
		profile:  profile,
	}
}

// acpStartError is a typed refusal of an ACP start or frame: its Code is a
// shared-constants ACP_START_ERROR_CODES value, published as the error
// frame's errorCode so terminal-service never reads it from free text.
type acpStartError struct {
	Code    string
	Message string
}

func (e *acpStartError) Error() string { return e.Message }

// acpTypedErrorCode is the errorCode an ACP start or send failure carries, or
// "" for an untyped one.
func acpTypedErrorCode(err error) string {
	if typed := grokAuthErrorFrom(err); typed != nil {
		return typed.Code
	}
	var acpErr *acpStartError
	if errors.As(err, &acpErr) {
		return acpErr.Code
	}
	return ""
}

// Start launches the profile's CLI in cwd.
//
// publishFn receives:
//   - `<kind>_acp_message` for every JSONL frame the CLI emits on stdout
//   - `<kind>_acp_stderr`  for every line the CLI emits on stderr
//   - `<kind>_acp_error`   when the CLI emits a non-JSON line (protocol bug)
//     OR when opts.TimeoutMs fires before the child exits naturally
//   - `<kind>_acp_ended`   when the process exits, carrying ExitCode
func (m *ACPManager) Start(id, cwd string, extraArgs []string, workspaceID, uid string, opts ACPStartOptions, publishFn PublishFunc) error {
	p := m.profile
	if id == "" {
		return fmt.Errorf("sessionID is required")
	}
	if publishFn == nil {
		return fmt.Errorf("publishFn is required")
	}

	// cwd is required and must point at an existing directory. Falling back
	// to the agent's working directory would silently run the CLI against a
	// surprise path (e.g. C:\Program Files\AI Expedite on Windows), exposing
	// or editing files unrelated to the requested workspace. The local
	// terminal's workspace/path safety rules treat this as session-fatal at
	// startup rather than tolerating an empty cwd as the legacy CLI path
	// does.
	if cwd == "" {
		return fmt.Errorf("cwd is required for %s (must point at a workspace directory)", p.startNoun)
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
	// session-setup frames (`params.cwd`) to the directory this session was
	// started in.
	//
	// This deliberately replaces an earlier check against the agent's
	// configured WorkingDirectory (secondary-review finding #1). That check
	// compared a cwd the SERVER derived (repo mapping → owner default →
	// inferred ancestor; see terminal-service contributedPaths.util.js)
	// against a root only the DEVICE knows — two sources of truth that were
	// guaranteed to disagree for any repo checked out outside the device
	// home, refusing directories the server itself had chosen (observed in
	// prod 2026-08-13: every grok review session against C:\aiexpedite died
	// at start on a device whose home is C:\Users\dkupi). What IS worth
	// enforcing on this device is "the session stays where the start said" —
	// hence the session-rooted gate in Send.
	resolvedCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return fmt.Errorf("cwd %q symlink resolution failed: %w", cwd, err)
	}
	resolvedRoot := resolvedCwd
	args, err := p.buildArgs(extraArgs, opts)
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
		return fmt.Errorf("%s session %s already exists", p.noun, id)
	}

	launch, err := p.prepare(acpLaunchRequest{ID: id, Cwd: cwd, Args: args, Opts: opts})
	if err != nil {
		return err
	}

	fmt.Printf("%s%s Starting session %s: %s %s%s\n",
		colorCyan, p.logTag, id, launch.executable, strings.Join(p.redactArgs(args), " "), colorReset)

	proc := exec.Command(launch.executable, args...)
	hideWindow(proc)
	if cwd != "" {
		proc.Dir = cwd
	}
	proc.Env = launch.env

	// cleanupFailedStart undoes prepare on any pre-spawn failure path. Once
	// the child is successfully started, ownership of what prepare built
	// transfers to waitForExit, so we must NOT call this after a successful
	// proc.Start().
	cleanupFailedStart := func() {
		if launch.cleanup != nil {
			launch.cleanup()
		}
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

	var undoArm func()
	var bindArm func(*ACPSession)
	if launch.armSpawn != nil {
		undoArm, bindArm = launch.armSpawn()
	}
	beginSessionSpawn(id, proc)
	if err := proc.Start(); err != nil {
		abortSessionSpawn(id)
		if undoArm != nil {
			undoArm()
		}
		stdin.Close()
		stdout.Close()
		stderr.Close()
		cleanupFailedStart()
		return p.spawnFailed(err)
	}
	trackSessionProcess(id, proc)
	launchedVersion := confirmCliLaunchVersion(launch.executable, launch.version, launch.versionKey)

	// Clamp the requested timeout at our stale-GC ceiling. A misbehaving
	// orchestrator that requests TimeoutMs > acpFamilyMaxLifetime would
	// otherwise outlive the GC anyway; clamping makes the effective deadline
	// predictable and keeps the timer firing meaningful.
	timeoutMs := opts.TimeoutMs
	if timeoutMs < 0 {
		timeoutMs = 0
	}
	if max := int64(acpFamilyMaxLifetime / time.Millisecond); timeoutMs > max {
		timeoutMs = max
	}

	session := &ACPSession{
		ID:                id,
		Process:           proc,
		Stdin:             stdin,
		Stdout:            stdout,
		Stderr:            stderr,
		StartedAt:         time.Now(),
		WorkspaceID:       workspaceID,
		UID:               uid,
		TimeoutMs:         timeoutMs,
		WorkspaceRoot:     resolvedRoot,
		IsolatedHome:      launch.isolatedHome,
		PersistentHome:    launch.persistentHome,
		limitNoticeScope:  launch.limitNoticeScope,
		producerContested: launch.producerContested,
		launchedVersion:   launchedVersion,
		liveFollowUps:     newLiveFollowUpTracker(p.cliAgentID, launchedVersion),
		status:            "running",
		done:              make(chan struct{}),
		processExited:     make(chan struct{}),
		streamDone:        make(chan struct{}),
		firstFrame:        make(chan struct{}),
	}
	if p.fenceFrames {
		session.fence = newACPClientFence(p)
	}
	if bindArm != nil {
		bindArm(session)
	}

	m.sessions[id] = session

	if proc.Process != nil {
		globalProcessRegistry.Register(proc.Process.Pid, p.processLabel+":"+id)
	}

	// Per-session deadline. Reserve the typed-error Seq BEFORE Kill() and
	// publish AFTER. The reservation ordering matters because Kill() races
	// with waitForExit: a fast exit can publish the ended frame (which
	// increments session.seq) before this callback would otherwise allocate
	// its own Seq, letting the orchestrator order the terminal ended frame
	// before the timeout error or drop the error as post-terminal. Taking the
	// AddInt64 first nails down a Seq strictly less than whatever the ended
	// frame ends up with, so even though both publishes are asynchronous the
	// orchestrator sees timeout → ended causal order. The publish is still
	// deferred until after Kill() because the production newSessionPublishFn
	// can block for the full Pub/Sub publish timeout (~30s) when Pub/Sub is
	// slow or unavailable; killing first guarantees the orchestrator sees the
	// child terminate on schedule and the natural exit publishes the ended
	// frame via waitForExit even if this diagnostic publish itself ultimately
	// fails. Timer is Stop()'d in waitForExit on natural exit so a
	// freshly-exited session can't double-fire the timeout publish.
	if session.TimeoutMs > 0 {
		session.timeoutTimer = time.AfterFunc(time.Duration(session.TimeoutMs)*time.Millisecond, func() {
			if session.Status() == "ended" {
				return
			}
			seq := atomic.AddInt64(&session.seq, 1)
			fmt.Printf("%s%s Session %s timed out after %dms — killing%s\n",
				colorYellow, p.logTag, session.ID, session.TimeoutMs, colorReset)
			if session.Process != nil && session.Process.Process != nil {
				_ = session.Process.Process.Kill()
			}
			publishFn(resultMsg{
				ID:          session.ID,
				WorkspaceID: session.WorkspaceID,
				UID:         session.UID,
				Output:      fmt.Sprintf("%s session timed out after %dms — terminated by per-session deadline", p.noun, session.TimeoutMs),
				Status:      "error",
				Ts:          time.Now().UnixMilli(),
				Version:     Version,
				Type:        p.resultTypes.Error,
				BootID:      agentBootID,
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
	// waiting on terminal-service's `initialize` frame.

	fmt.Printf("%s%s Session %s started (PID: %d)%s\n",
		colorGreen, p.logTag, id, proc.Process.Pid, colorReset)
	return nil
}

// ArmFirstFrameWatchdog starts the first-frame watchdog for an already-started
// session. Split out from Start so the dispatcher can call it AFTER the
// synchronous started ack publish completes — that publish can take up to 30s
// on a slow Pub/Sub, and including it in the watchdog budget would risk
// killing a healthy child that is just waiting on terminal-service's
// `initialize` frame. No-op if the session is unknown (already removed) or
// already exited.
func (m *ACPManager) ArmFirstFrameWatchdog(id string, publishFn PublishFunc) {
	session := m.Get(id)
	if session == nil {
		return
	}
	if session.Status() == "ended" {
		return
	}
	go m.watchFirstFrame(session, publishFn, acpFamilyFirstFrameTimeout)
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
func (m *ACPManager) Send(id string, payload string) error {
	return m.SendAccepting(id, payload, nil)
}

// SendAccepting is Send that calls accept (when non-nil) at the ownership
// boundary: under stdinMu, after every refusal, immediately BEFORE the turn is written.
// A relayed voice turn records `accepted` in the durable turn inbox there
// (relay_turn_inbox.go); an accept error refuses the turn.
func (m *ACPManager) SendAccepting(id string, payload string, accept func() error) error {
	p := m.profile
	session := m.Get(id)
	if session == nil {
		return fmt.Errorf("%s session %s not found", p.noun, id)
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
	// ACP stdio frames are individual JSON-RPC 2.0 messages — a request,
	// notification or response, each a single JSON object per line.
	// Top-level arrays (JSON-RPC batch form) and scalars are out of spec for
	// ACP and must be rejected here rather than passed to the child: the
	// session-setup containment gate only inspects object-shaped frames, so a
	// batched `[{"method":"session/new", "params":{"cwd":"/outside"}}, ...]`
	// would otherwise skip the cwd check and reach the CLI unfiltered.
	if trimmed[0] != '{' {
		return fmt.Errorf("payload must be a single JSON-RPC object; batch arrays and scalar frames are not supported on ACP stdio")
	}

	// Pin ACP session-setup frames to the directory the session was started
	// in. Each setup verb can carry its own `params.cwd` the CLI will use as
	// the session root, so without this check a later signed send could
	// re-point the CLI at any local path and bypass the server's cwd
	// derivation entirely. The root is the session's own symlink-resolved
	// start cwd (always set on a started session; the empty-guard covers only
	// synthetic test fixtures). For OpenCode and Antigravity the whole frame
	// goes through the family's allowlist too (acp_fence.go); Grok keeps its
	// setup-cwd gate alone.
	var fenceCheck acpFenceCheck
	if session.fence != nil {
		check, err := session.fence.check(trimmed, session.WorkspaceRoot)
		if err != nil {
			return err
		}
		fenceCheck = check
	} else if session.WorkspaceRoot != "" {
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
		return fmt.Errorf("%s session %s has ended", p.noun, id)
	}
	if err := acceptRelayDelivery(accept); err != nil {
		return err
	}
	// The live follow-up tracker (relay_follow_up_jsonrpc.go) reads the frame
	// before it reaches the child and learns, on return, whether it did.
	sent := session.liveFollowUps.outbound(trimmed)
	wrote := false
	defer func() {
		sent.done(wrote)
		if wrote {
			session.fence.written(fenceCheck)
		}
	}()

	writeDone := make(chan error, 1)
	go func() {
		_, err := fmt.Fprintln(session.Stdin, trimmed)
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		if err != nil {
			return fmt.Errorf("failed to write to %s session %s stdin: %w", p.noun, id, err)
		}
	case <-time.After(acpFamilyStdinWriteTimeout):
		// Fatal: a stalled write is a signal that the CLI isn't draining
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
		return fmt.Errorf("timeout writing to %s session %s stdin — session terminated to prevent frame interleave", p.noun, id)
	}
	wrote = true

	fmt.Printf("%s%s → %s (%d bytes)%s\n",
		colorBlue, p.logTag, id, len(trimmed), colorReset)
	return nil
}

// End shuts down a session. First it closes stdin (ACP's documented exit
// path). If the process is still alive after acpFamilyGracefulShutdownTimeout
// we interrupt it, and if that also fails we SIGKILL.
func (m *ACPManager) End(id string) error {
	p := m.profile
	session := m.Get(id)
	if session == nil {
		return fmt.Errorf("%s session %s not found", p.noun, id)
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
				return fmt.Errorf("%s session %s process absence verified but old stream publishers have not drained; session retained: %w", p.noun, id, errEndUnconfirmed)
			}
			if !m.removeSessionIfSame(id, session) {
				// See CodexAppServerManager.End — the ID is not ours to free.
				return fmt.Errorf("%s session %s could not be released — a terminal frame is still in flight or the ID was re-taken: %w", p.noun, id, errEndUnconfirmed)
			}
			return fmt.Errorf("%s session %s not found", p.noun, id)
		}
		if session.Process.Process != nil {
			if killErr := session.Process.Process.Kill(); killErr != nil {
				fmt.Printf("%s%s Re-kill failed for %s: %v%s\n",
					colorRed, p.logTag, id, killErr, colorReset)
			}
		}
		if waitDoneConfirm(processExited, killConfirmTimeout) {
			return nil
		}
		return fmt.Errorf("%s session %s kill unconfirmed after %s; session retained pending process-absence verification: %w", p.noun, id, killConfirmTimeout, errEndUnconfirmed)
	}

	fmt.Printf("%s%s Ending session %s gracefully...%s\n",
		colorYellow, p.logTag, id, colorReset)

	session.closeStdin()

	select {
	case <-processExited:
	case <-time.After(acpFamilyGracefulShutdownTimeout):
		fmt.Printf("%s%s Stdin close didn't exit %s — interrupting%s\n",
			colorYellow, p.logTag, id, colorReset)
		_ = interruptProcess(session.Process)
		select {
		case <-processExited:
		case <-time.After(acpFamilyGracefulShutdownTimeout):
			fmt.Printf("%s%s Force killing session %s%s\n",
				colorRed, p.logTag, id, colorReset)
			if session.Process.Process != nil {
				if killErr := session.Process.Process.Kill(); killErr != nil {
					fmt.Printf("%s%s Kill failed for %s: %v%s\n",
						colorRed, p.logTag, id, killErr, colorReset)
				}
			}
			// BOUNDED wait — see end_confirm.go for why blocking here
			// indefinitely wedged an entire device (2026-08-27).
			if !waitDoneConfirm(processExited, killConfirmTimeout) {
				fmt.Printf("%s%s Kill unconfirmed for %s after %s — retaining tombstone; a later end verifies process absence%s\n",
					colorRed, p.logTag, id, killConfirmTimeout, colorReset)
				session.markKillUnconfirmed()
				// Deliberately NOT "session <id> not found": the process may
				// still be alive, and the server must keep the device fenced
				// until absence is VERIFIED (see the tombstone branch above).
				return fmt.Errorf("%s session %s kill unconfirmed after %s; session retained pending process-absence verification: %w", p.noun, id, killConfirmTimeout, errEndUnconfirmed)
			}
		}
	}

	// The watcher retains the exited session until terminal publication is
	// reserved, so a replacement cannot steal the old frame's session ID.
	return nil
}

// Get returns the session for id, or nil if it does not exist.
func (m *ACPManager) Get(id string) *ACPSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

// ActiveCount returns the number of currently tracked sessions.
func (m *ACPManager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// CleanupStale runs periodically to end sessions that exceed maxAge. Call as
// a goroutine: `go m.CleanupStale(acpFamilyMaxLifetime)`. Without this, an
// orchestrator crash that drops the END would leak children indefinitely.
func (m *ACPManager) CleanupStale(maxAge time.Duration) {
	ticker := time.NewTicker(acpFamilyCleanupInterval)
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
func (m *ACPManager) endStaleSessions(maxAge time.Duration) {
	m.mu.RLock()
	var staleIDs []string
	for id, session := range m.sessions {
		if time.Since(session.StartedAt) > maxAge {
			staleIDs = append(staleIDs, id)
		}
	}
	m.mu.RUnlock()

	for _, id := range staleIDs {
		fmt.Printf("%s%s Cleaning up stale session %s (exceeded %v)%s\n",
			colorYellow, m.profile.logTag, id, maxAge, colorReset)
		_ = m.End(id)
	}
}

// ShutdownAll ends every active session. Called during agent shutdown so
// children don't outlive the agent and silently consume tokens.
func (m *ACPManager) ShutdownAll() {
	m.mu.RLock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	if len(ids) > 0 {
		fmt.Printf("%s%s Shutting down %d active session(s)...%s\n",
			colorYellow, m.profile.logTag, len(ids), colorReset)
	}
	for _, id := range ids {
		_ = m.End(id)
	}
}

func (m *ACPManager) removeSession(id string) {
	m.mu.Lock()
	if s, ok := m.sessions[id]; ok && s.Process != nil && s.Process.Process != nil {
		globalProcessRegistry.Deregister(s.Process.Process.Pid)
	}
	delete(m.sessions, id)
	m.mu.Unlock()
	releaseLedgerSession(id)
}

// removeSessionIfSame removes id only while it still maps to THIS session —
// see CodexAppServerManager.removeSessionIfSame for the reused-ID watcher
// race this prevents (Codex P2).
//
// It also refuses while a terminal frame is in flight for s: the frame is
// already travelling under this ID, so freeing the ID now would let a
// replacement Start receive it as its own shutdown evidence (Codex P2, round
// 4). The publisher performs the removal itself once delivery completes.
//
// Returns whether id is free of s afterwards — false means either a
// replacement already owns the ID or the release is deferred to the in-flight
// publisher, and in both cases the caller must NOT report this session's
// absence.
func (m *ACPManager) removeSessionIfSame(id string, s *ACPSession) bool {
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
	releaseLedgerSession(id)
	return true
}

/* --------------------------------------------------------------------------
   Stream + exit handling
   -------------------------------------------------------------------------- */

// readStream forwards every stdout frame as `<kind>_acp_message` and every
// stderr line as `<kind>_acp_stderr`. stdout frames are validated as JSON (so
// a malformed frame is surfaced as `<kind>_acp_error` instead of being
// silently passed through), and the original line text is forwarded verbatim,
// with ONE exception for Grok: consecutive reply chunks are merged into one
// frame (grok_acp_chunk_merge.go), so the prompt answer that ends a turn no
// longer queues behind every one of them. One frame can be delayed but never
// edited: a Grok prompt answer the live follow-up tracker holds while a
// stranded interjection runs as a turn of its own (relay_follow_up_jsonrpc.go).
//
// Publishing uses a single ordered consumer goroutine that drains a bounded
// queue. Every frame of the ACP stdio protocol is a stateful JSON-RPC message
// (a response correlated by id, a session/update notification, an approval
// request); silently dropping one would corrupt terminal-service's session
// state. Both readers enqueue through the merger's lock, and a frame takes its
// seq there, so the seqs follow the queue order with no gaps.
func (m *ACPManager) readStream(session *ACPSession, publishFn PublishFunc) {
	p := m.profile
	defer close(session.streamDone)

	queue := make(chan resultMsg, acpFamilyPublishQueueSize)
	publisherDone := startTrackedTerminalPublisher(queue, publishFn)
	defer func() {
		close(queue)
		<-publisherDone
	}()

	enqueue := func(msg resultMsg) bool {
		select {
		case queue <- msg:
			return true
		case <-time.After(acpFamilyEnqueueTimeout):
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
		fmt.Printf("%s%s Publish queue stalled for %s — failing session (dropped %s)%s\n",
			colorRed, p.logTag, session.ID, droppedType, colorReset)
		seq := atomic.AddInt64(&session.seq, 1)
		publishFn(resultMsg{
			ID:          session.ID,
			WorkspaceID: session.WorkspaceID,
			UID:         session.UID,
			Output:      reason,
			Status:      "error",
			Ts:          time.Now().UnixMilli(),
			Version:     Version,
			Type:        p.resultTypes.Error,
			BootID:      agentBootID,
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
				fmt.Sprintf("%s envelope failed to marshal: %v — session terminated", p.noun, err),
				droppedType,
			)
			return false
		}
		if len(encoded) > acpFamilyMaxPublishSize {
			failSessionFatally(
				fmt.Sprintf(
					"%s envelope marshaled to %d bytes after JSON escaping, exceeding the %d-byte publishable limit — session terminated to avoid silent Pub/Sub rejection",
					p.noun, len(encoded), acpFamilyMaxPublishSize,
				),
				droppedType,
			)
			return false
		}
		if !enqueue(msg) {
			failSessionFatally(p.noun+" publish queue stalled — terminating session to avoid dropping JSON-RPC frames", droppedType)
			return false
		}
		return true
	}

	// Every frame of the session goes through the merger: a Grok message
	// chunk may wait in it (up to grokChunkMergeDelay), and any other frame
	// is published after it. A frame's seq is taken where it is published,
	// under the merger's lock, so seqs follow the queue order — and a prompt
	// answer the live follow-up tracker held back takes one later than every
	// frame read before its release.
	publishAnnotated := func(line string, ann acpFrameAnnotation) bool {
		seq := atomic.AddInt64(&session.seq, 1)
		return publishOrFail(resultMsg{
			ID:              session.ID,
			WorkspaceID:     session.WorkspaceID,
			UID:             session.UID,
			Output:          line,
			Status:          "success",
			Ts:              time.Now().UnixMilli(),
			Version:         Version,
			Type:            p.resultTypes.Message,
			SessionID:       session.ID,
			Seq:             int(seq),
			AcpHandoverTo:   ann.handoverTo,
			AcpSteerOutcome: ann.steerOutcome,
		}, p.resultTypes.Message)
	}
	publishMessage := func(line string) bool { return publishAnnotated(line, acpFrameAnnotation{}) }
	merger := newGrokChunkMerger(publishMessage)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		// Whatever the reader stops on, the held text goes out first.
		defer merger.close()
		// Live follow-ups (relay_follow_up_jsonrpc.go): the tracker reads every
		// frame that is not a reply chunk, and may hold one Grok prompt answer
		// back while a stranded interjection runs (L1/L2 spec §2.8). It
		// releases it through the merger, after any reply text read before
		// it. At EOF it publishes what it still holds, so before the ended
		// frame; after a fatal stop it publishes nothing more.
		session.liveFollowUps.attachPublisher(func(line string) bool {
			return merger.emit(func() bool { return publishMessage(line) })
		})
		defer session.liveFollowUps.readerDone(false)
		scanner := bufio.NewScanner(session.Stdout)
		scanner.Buffer(make([]byte, 0, 256*1024), acpFamilyMaxLineSize)
		fmt.Printf("%s%s stdout scanner started for %s%s\n",
			colorCyan, p.logTag, session.ID, colorReset)
		lineCount := 0
		for scanner.Scan() {
			lineCount++
			line := scanner.Text()
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}

			if len(trimmed) > acpFamilyMaxFrameSize {
				merger.close()
				failSessionFatally(
					fmt.Sprintf(
						"%s emitted a %d-byte frame exceeding the %d-byte publishable limit — session terminated to avoid silent drop",
						p.cliName, len(trimmed), acpFamilyMaxFrameSize,
					),
					p.typePrefix()+"_oversize_frame",
				)
				return
			}

			var probe json.RawMessage
			if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
				fmt.Printf("%s%s Non-JSON stdout frame on %s: %v (line=%s)%s\n",
					colorRed, p.logTag, session.ID, err, truncateString(trimmed, 200), colorReset)
				if !merger.emit(func() bool {
					seq := atomic.AddInt64(&session.seq, 1)
					return publishOrFail(resultMsg{
						ID:          session.ID,
						WorkspaceID: session.WorkspaceID,
						UID:         session.UID,
						Output:      fmt.Sprintf("non-JSON frame on %s stdout: %v", p.noun, err),
						Status:      "error",
						Ts:          time.Now().UnixMilli(),
						Version:     Version,
						Type:        p.resultTypes.Error,
						BootID:      agentBootID,
						SessionID:   session.ID,
						Seq:         int(seq),
					}, p.resultTypes.Error)
				}) {
					return
				}
				continue
			}

			// Valid JSON-RPC frame — disarm the first-frame watchdog. We
			// deliberately wait for a parseable frame here instead of
			// signaling on any stdout: a CLI's auth/updater paths can print
			// non-JSON banners or prompts and then stall waiting for input.
			// Treating those as proof-of-life would disarm the watchdog while
			// the ACP handshake is still hung. The non-JSON branch above only
			// reports an error frame and keeps scanning, so the watchdog must
			// stay armed until we see a real frame.
			session.signalFirstFrame()

			if lineCount <= 3 {
				fmt.Printf("%s%s stdout[%d] %s: %s%s\n",
					colorCyan, p.logTag, lineCount, session.ID, truncateString(trimmed, 200), colorReset)
			}

			if p.onStdoutFrame != nil {
				p.onStdoutFrame(session, trimmed)
			}
			// The client-frame fence learns which agent-to-client requests are
			// open, so it lets through only answers to those (acp_fence.go).
			session.fence.inbound(trimmed)

			if p.mergeReplyChunks {
				if chunk, ok := parseGrokMessageChunk(trimmed); ok {
					if !merger.add(chunk) {
						return
					}
					continue
				}
			}
			hold, ann := session.liveFollowUps.inboundAnnotated(trimmed)
			if hold {
				continue
			}
			if !merger.emit(func() bool { return publishAnnotated(trimmed, ann) }) {
				return
			}
			if !session.liveFollowUps.publishReleased() {
				return
			}
		}
		session.liveFollowUps.readerDone(true)
		if err := scanner.Err(); err != nil {
			fmt.Printf("%s%s stdout scanner error for %s: %v%s\n",
				colorRed, p.logTag, session.ID, err, colorReset)
			merger.close()
			failSessionFatally(
				fmt.Sprintf("%s stdout scanner error: %v — session terminated", p.cliName, err),
				p.typePrefix()+"_scanner_error",
			)
		}
		merger.close()
		merged, frames := merger.counts()
		fmt.Printf("%s%s stdout scanner done for %s (%d lines; %d reply chunks merged into %d frames)%s\n",
			colorYellow, p.logTag, session.ID, lineCount, merged, frames, colorReset)
	}()

	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(session.Stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		fmt.Printf("%s%s stderr scanner started for %s%s\n",
			colorCyan, p.logTag, session.ID, colorReset)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.TrimSpace(line) == "" {
				continue
			}
			fmt.Printf("%s%s stderr %s: %s%s\n",
				colorYellow, p.logTag, session.ID, truncateString(line, 200), colorReset)
			// After any reply text read before it, never ahead of it. Once the
			// session has failed nothing more is published, but stderr is
			// still drained to EOF so nothing writing to it can block.
			if !merger.emit(func() bool {
				seq := atomic.AddInt64(&session.seq, 1)
				return publishOrFail(resultMsg{
					ID:          session.ID,
					WorkspaceID: session.WorkspaceID,
					UID:         session.UID,
					Output:      line,
					Status:      "success",
					Ts:          time.Now().UnixMilli(),
					Version:     Version,
					Type:        p.resultTypes.Stderr,
					SessionID:   session.ID,
					Seq:         int(seq),
				}, p.resultTypes.Stderr)
			}) {
				continue
			}
		}
		if err := scanner.Err(); err != nil {
			// stderr is diagnostic, not protocol-critical: a lost/truncated
			// stderr line cannot deadlock the orchestrator's JSON-RPC state
			// machine the way a dropped stdout frame would. Log and continue.
			fmt.Printf("%s%s stderr scanner error for %s: %v%s\n",
				colorRed, p.logTag, session.ID, err, colorReset)
		}
	}()

	wg.Wait()
}

func (m *ACPManager) waitForExit(session *ACPSession, publishFn PublishFunc) {
	p := m.profile
	// Reap the child via os.Process.Wait, NOT exec.Cmd.Wait. Per the
	// StdoutPipe docs, "it is incorrect to call Wait before all reads from
	// the pipe have completed" — exec.Cmd.Wait closes the parent ends of
	// StdoutPipe/StderrPipe the moment the child exits, which can truncate
	// the final JSON-RPC frame still buffered in the bufio.Scanner. When the
	// CLI writes a response and exits in quick succession, that final frame
	// is the one the orchestrator needs to complete the in-flight ACP
	// request; losing it leaves the request stuck. Splitting exit detection
	// (Process.Wait) from pipe cleanup (manual Close below, gated on
	// streamDone) preserves the final frame while keeping the status-flip
	// race fix intact.
	state, _ := session.Process.Process.Wait()
	closeProcessExited(session.processExited)

	// Flip status to "ended" and record exitCode BEFORE the stream-drain wait.
	// The deadline timer's AfterFunc gates its publish+Kill on
	// Status() == "ended"; if we postponed this flip until after drain
	// (which can be slow under back-pressure), a timer that fires while we
	// are draining would see status=="running", publish a spurious error
	// frame AND Kill an already-exited PID. Both are observable upstream —
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
	case <-time.After(acpFamilyStreamDrainTimeout):
		fmt.Printf("%s%s Stream drain timed out for %s — forcing pipe close%s\n",
			colorYellow, p.logTag, session.ID, colorReset)
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

	if p.onExit != nil {
		p.onExit(session)
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
		Output:       p.endedOutput(exit),
		Status:       "success",
		Ts:           time.Now().UnixMilli(),
		Version:      Version,
		Type:         p.resultTypes.Ended,
		SessionID:    session.ID,
		ExitCode:     exit,
		Seq:          int(seq),
		Files:        uploadedFiles,
		UploadErrors: uploadErrors,
	}, func() { m.removeSessionIfSame(session.ID, session) }) {
		fmt.Printf("%s%s Suppressed stale %s for %s — the ID now belongs to a replacement session%s\n",
			colorYellow, p.logTag, p.resultTypes.Ended, session.ID, colorReset)
	}

	close(session.done)

	if p.afterEnded != nil {
		p.afterEnded(session)
	}

	fmt.Printf("%s%s Session %s ended (exit code: %d)%s\n",
		colorYellow, p.logTag, session.ID, exit, colorReset)

	// No-op while the ended frame is still in flight — the publisher's release
	// callback owns the removal in that case (end_confirm.go).
	m.removeSessionIfSame(session.ID, session)
}

// watchFirstFrame fails a session fast when the CLI spawns but never produces
// any stdout — the signature of a child blocked on interactive
// re-authentication or otherwise wedged at startup. Without it such a session
// sits at "started" until the optional per-session deadline (often none) or
// the 6h stale GC.
//
// It blocks until one of three things happens:
//   - session.firstFrame closes — the CLI emitted a frame; healthy, disarm.
//   - session.done closes — the child already exited (e.g. immediate crash);
//     waitForExit owns the terminal ended frame, so do nothing.
//   - timeout elapses — declare the startup stalled, publish an actionable
//     error frame, and kill the child so waitForExit emits the ended frame.
//
// timeout is a parameter (rather than the package constant) so unit tests can
// drive the fail-fast path with a sub-second budget and no real process.
//
// Seq ordering mirrors the per-session deadline timer: reserve the error's Seq
// (atomic increment) BEFORE Kill and publish AFTER, so the watchdog error is
// strictly ordered before the ended frame waitForExit allocates once the
// killed child is reaped — the orchestrator therefore sees error → ended.
func (m *ACPManager) watchFirstFrame(session *ACPSession, publishFn PublishFunc, timeout time.Duration) {
	p := m.profile
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
	fmt.Printf("%s%s Session %s produced no output within %v — assuming auth/startup stall, killing%s\n",
		colorYellow, p.logTag, session.ID, timeout, colorReset)
	if session.Process != nil && session.Process.Process != nil {
		_ = session.Process.Process.Kill()
	}
	publishFn(resultMsg{
		ID:          session.ID,
		WorkspaceID: session.WorkspaceID,
		UID:         session.UID,
		Output:      p.firstFrameStall(timeout),
		Status:      "error",
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		Type:        p.resultTypes.Error,
		BootID:      agentBootID,
		SessionID:   session.ID,
		Seq:         int(seq),
	})
}
