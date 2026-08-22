// File: session.go
// -----------------------------------------------------------------------------
// SessionManager manages long-lived interactive CLI agent sessions (claude,
// codex).  Each session holds a process with stdin/stdout/stderr pipes.
// Output is streamed back via a publish function, and stdin input can be sent
// by session ID.
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

/* --------------------------------------------------------------------------
   Constants
   -------------------------------------------------------------------------- */

const (
	// sessionMaxLifetime is the maximum time a session can run before cleanup.
	// Set to 6 hours to support long-running CLI agent sessions (Claude, Codex, etc.).
	sessionMaxLifetime = 6 * time.Hour

	// sessionCleanupInterval is how often the cleanup goroutine runs
	sessionCleanupInterval = 60 * time.Second

	// streamBatchInterval is how often output is batched and published
	streamBatchInterval = 200 * time.Millisecond

	// gracefulShutdownTimeout is how long to wait after interrupt before force killing
	gracefulShutdownTimeout = 5 * time.Second

	// sessionStreamDrainTimeout is how long waitForExit waits for the scanner
	// goroutines to reach EOF and drain the pipe buffer after the child exits
	// before force-closing the owned read ends to unblock a wedged scanner
	// (e.g. a lingering Windows pipe handle). Mirrors the Claude/Grok/Codex
	// native managers' drain timeouts.
	sessionStreamDrainTimeout = 30 * time.Second
)

/* --------------------------------------------------------------------------
   CLISession — one interactive CLI agent process
   -------------------------------------------------------------------------- */

// CLISession represents a single interactive CLI agent process with
// bidirectional I/O pipes and streaming output.
type CLISession struct {
	ID        string
	Command   string // "claude", "codex"
	Process   *exec.Cmd
	Stdin     io.WriteCloser
	Stdout    io.ReadCloser
	Stderr    io.ReadCloser
	StartedAt time.Time
	Status    string // "running" | "waiting_input" | "ended"
	ExitCode  int
	Seq       int64 // atomic sequence counter for output ordering

	// Metadata for result messages
	WorkspaceID string
	UID         string
	TimeoutMs   int64 // Per-session timeout in ms (0 = no timeout, use stale cleanup)

	// promptFile is the path to a temp file holding grok's prompt, set when
	// rewriteGrokPromptToFile relocated the argv `-p <prompt>` pair to
	// `--prompt-file <path>` to dodge the Windows command-line-length cap.
	// Empty for every other command (and for grok subcommand carve-outs).
	// Removed exactly once after the process exits (see waitForExit).
	promptFile string

	// deferredStdinClose marks a one-shot, stdin-fed CLI (codex) that
	// was started with NO prompt — the chat-direct flow opens the session
	// eagerly and delivers the first message later via SendInput. Stdin is left
	// open at start (see shouldCloseStdinAfterStart) and closed by SendInput
	// immediately after the first prompt is written, giving the child its
	// prompt + EOF in the order codex exec requires. Reset to false once the
	// pipe is closed so a second SendInput doesn't double-close.
	deferredStdinClose bool

	// firstRealFrame is closed exactly once (via firstRealFrameOnce) the moment
	// a claude session emits its first genuine assistant output — a stream-json
	// text/thinking delta or a tool_use. The claude no-output watchdog
	// (watchClaudeFirstFrame) blocks on it to tell a healthy (if slow) session
	// apart from one that launched, emitted only its `system/init`, and then
	// stalled — the signature of a Claude Code that is not signed in on the
	// terminal computer. Created for every session; only claude arms the
	// watchdog and only claude signals it.
	firstRealFrame     chan struct{}
	firstRealFrameOnce sync.Once

	// claudeWatchdogOnce guards the no-output watchdog goroutine so it is armed
	// at most once per claude session. The arm point is deferred to the moment
	// a prompt is actually delivered to claude (StartSession's initial stdin
	// write, or the first SendInput on a session opened without an initial
	// prompt) — arming at process start would kill the supported no-initial-
	// prompt flow (chat-direct/session.sendInput) where claude legitimately
	// sits idle until the first SendInput lands.
	claudeWatchdogOnce sync.Once
	// publishFn is captured at StartSession so deferred arming from SendInput
	// can publish the fail-fast error frame without threading it through.
	publishFn PublishFunc

	mu         sync.Mutex
	done       chan struct{} // closed when process exits
	streamDone chan struct{} // closed when stdout/stderr and stream publishes finish
}

// claudeFirstFrameTimeout bounds how long a freshly-started claude session may
// go without producing any real assistant output before the watchdog assumes
// it is not signed in (or wedged at startup) and kills it. Generous on purpose:
// a healthy claude streams a thinking/text delta well inside this window even
// on a large repo, so the only sessions this catches are ones that emit nothing
// but their `system/init`. Mirrors the grok ACP first-frame watchdog.
const claudeFirstFrameTimeout = 120 * time.Second

// signalFirstRealFrame marks that this session has produced genuine assistant
// output, disarming the no-output watchdog. Idempotent and nil-safe.
func (s *CLISession) signalFirstRealFrame() {
	if s.firstRealFrame == nil {
		return
	}
	s.firstRealFrameOnce.Do(func() { close(s.firstRealFrame) })
}

/* --------------------------------------------------------------------------
   SessionManager — manages all active sessions
   -------------------------------------------------------------------------- */

// SessionManager tracks and manages active interactive CLI sessions.
type SessionManager struct {
	sessions map[string]*CLISession
	mu       sync.RWMutex
	Config   *Config // Config for file upload settings
}

// NewSessionManager creates a new SessionManager.
func NewSessionManager(cfg *Config) *SessionManager {
	return &SessionManager{
		sessions: make(map[string]*CLISession),
		Config:   cfg,
	}
}

// PublishFunc is the callback signature for publishing result messages.
type PublishFunc func(res resultMsg)

/* --------------------------------------------------------------------------
   StartSession — spawn a new interactive CLI process
   -------------------------------------------------------------------------- */

// StartSession creates and starts a new interactive CLI session. The process
// is spawned with stdin/stdout/stderr pipes and output is streamed via publishFn.
func (sm *SessionManager) StartSession(id, command string, args []string, cwd, workspaceID, uid string, timeoutMs int64, tty bool, publishFn PublishFunc) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if _, exists := sm.sessions[id]; exists {
		return fmt.Errorf("session %s already exists", id)
	}

	// Build the CLI command with appropriate flags for structured streaming.
	// stdinPrompt is non-empty for Claude — the prompt is sent as NDJSON on stdin.
	enableGrokAlwaysApprove := sm.Config != nil && sm.Config.EnableGrokAlwaysApprove
	cliArgs, stdinPrompt := buildInteractiveCLIArgs(command, args, enableGrokAlwaysApprove)

	// OpenCode's LEGACY session_start / PTY path carries its prompt as a
	// trailing positional (the resident chat path in opencode_native.go writes
	// it to a temp file consumed on stdin instead), so it is subject to the
	// Windows CreateProcess ~32KB command-line ceiling. Refuse above the cap
	// rather than letting CreateProcess fail with an opaque "command line too
	// long", and measure in BYTES — a character-count check passes a multibyte
	// prompt that still exceeds the real limit. grok solves the same problem by
	// relocating its prompt to --prompt-file below; opencode's legacy path has
	// no such flag, so failing closed is the honest answer.
	if isOpenCodeCommand(command) {
		if n := argvByteLen(cliArgs); n > openCodeInteractiveMaxPromptBytes {
			return fmt.Errorf(
				"opencode arguments are %d bytes, exceeding the %d-byte limit for a one-shot session; use an OpenCode chat session for long prompts",
				n, openCodeInteractiveMaxPromptBytes)
		}
	}

	// Opt-in PTY path for recognized resident TUI agents (agy/antigravity) that
	// require a real terminal. macOS/Linux only — startPTYSession rejects on
	// Windows (ConPTY deferred). PTY output is merged (stdout+stderr) and
	// normalized before streaming; the JSON-protocol agents and all utilities
	// stay on the pipe path below, so tty is a no-op for anything not on the
	// allowlist. See EXECUTION_LIVENESS_REDESIGN.md → PTY mode.
	if tty && isPTYEligibleCommand(command, args) {
		// buildInteractiveCLIArgs shapes a DIRECT agy/antigravity invocation into
		// its one-shot `--print --dangerously-skip-permissions <prompt>` form, but
		// a shell-wrapped single-agent payload (`bash -c "agy …"`, how
		// terminal-service ships operator-joined commands) falls through its
		// default branch unshaped — the base command is the shell, not agy. Apply
		// the same shell-payload shaping the execute path (shapePTYExecArgs) uses
		// so the inner agy reaches its non-interactive `--print` path and returns a
		// one-shot result instead of dropping into the interactive TUI and hanging
		// until the prompt timeout. Direct agy argv is already shaped and passes
		// through unchanged (shellDashCPayload only matches a shell -c wrapper).
		ptyArgs := shapeShellWrappedPTYArgs(command, cliArgs)
		return sm.startPTYSession(id, command, ptyArgs, cwd, workspaceID, uid, timeoutMs, publishFn)
	}

	// grok's headless mode takes its prompt on argv (`-p <prompt>`) and does NOT
	// read a piped stdin, so — unlike claude/codex, which route a long
	// prompt through stdinPrompt — a long review brief would blow past the
	// Windows command-line-length cap ("command line too long"). grok accepts
	// `--prompt-file <path>` as a drop-in for `-p <prompt>`, so relocate the
	// prompt into a temp file. promptFile is removed after the process exits.
	var promptFile string
	if isGrokCommand(command) {
		cliArgs, promptFile = rewriteGrokPromptToFile(cliArgs)
	}

	// rewriteGrokPromptToFile may have created an on-disk temp file. waitForExit
	// owns the eventual removal, but it only runs once proc.Start() has
	// succeeded AND a CLISession has been registered — every pipe/Start error
	// path below this point returns early and would otherwise leak the file.
	// Disarm this defer once the session has taken ownership.
	sessionOwnsPromptFile := false
	defer func() {
		if promptFile != "" && !sessionOwnsPromptFile {
			_ = os.Remove(promptFile)
		}
	}()

	// Resolve executable path
	executable := resolveExecutable(command)

	fmt.Printf("%s[session] Starting %s session %s: %s %s%s\n",
		colorCyan, command, id, executable, strings.Join(cliArgs, " "), colorReset)

	proc := exec.Command(executable, cliArgs...)
	hideWindow(proc)
	if cwd != "" {
		proc.Dir = cwd
	}

	filtered, strippedVars := prepareClaudeChildEnv(command, os.Environ())
	proc.Env = filtered
	if len(strippedVars) > 0 {
		fmt.Printf("%s[session] Stripped env vars from session %s: %s%s\n",
			colorYellow, id, strings.Join(strippedVars, ", "), colorReset)
	}

	// Headless hardening for NON-resident utility session_start commands.
	// Ordinary orchestrator commands (bash/sh/git/PowerShell/test runners) are
	// dispatched through session_start (terminal.execute.command/runAndWait),
	// NOT the one-shot execute path — so without this a git/editor/credential
	// prompt would escape the captured pipes via /dev/tty and hang until the
	// session timeout. hardenNonAgentCommand applies the authoritative
	// non-interactive git/editor/credential env on top of the sanitized proc.Env
	// prepared above (preserving the stripped CLAUDE_* filtering rather than
	// reverting to os.Environ()), detaches the controlling terminal, and layers
	// the test-runner defaults for recognized runners. Resident CLI agents
	// (claude/codex/gemini/grok/agy) keep their interactive-capable env + stdin.
	// See EXECUTION_LIVENESS_REDESIGN.md → headless hardening.
	utilitySession := !isResidentAgentSessionCommand(command)
	if utilitySession {
		hardenNonAgentCommand(proc, effectiveCommandLine(command, cliArgs))
	}

	// Set up pipes
	stdin, err := proc.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdin pipe: %w", err)
	}
	// Own the stdout/stderr READ ends ourselves via os.Pipe rather than
	// proc.StdoutPipe()/StderrPipe(). With the exec-owned pipes, Process.Wait()
	// closes the read end the instant the child is reaped — discarding any bytes
	// still buffered in the pipe that the scanner goroutine hasn't read yet.
	// Reaping is independent of draining, so a background Wait() or a fixed
	// grace can't avoid the loss. For a fast one-shot (`sh -c 'printf …'`) the
	// child can exit before the scanner is even scheduled, dropping its entire
	// output — observed as the flaky TestStartSession_UtilityGetsHeadlessGitEnv
	// `streamed=""` failure under -race. Owning the read ends means only the
	// child's exit (closing the write end) produces EOF, so the scanner always
	// drains every byte first; Wait() no longer touches these fds. waitForExit
	// closes them only after the scanner reaches EOF (gated on streamDone), and
	// force-closes early solely to unblock a scanner stuck on a lingering Windows
	// pipe handle after a drain timeout (see its comment).
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdin.Close()
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdin.Close()
		stdoutR.Close()
		stdoutW.Close()
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}
	proc.Stdout = stdoutW
	proc.Stderr = stderrW
	stdout := stdoutR
	stderr := stderrR

	// Start the process
	if err := proc.Start(); err != nil {
		stdin.Close()
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		return fmt.Errorf("failed to start %s: %w", command, err)
	}

	// The child now holds its own dup of the write ends; close the parent's
	// copies so the read ends see EOF when (and only when) the child exits.
	// Without this the parent stays a writer and the scanners never EOF. Must
	// run after Start (that's where the fds are handed to the child).
	stdoutW.Close()
	stderrW.Close()

	session := &CLISession{
		ID:          id,
		Command:     command,
		Process:     proc,
		Stdin:       stdin,
		Stdout:      stdout,
		Stderr:      stderr,
		StartedAt:   time.Now(),
		Status:      "running",
		WorkspaceID: workspaceID,
		UID:         uid,
		TimeoutMs:   timeoutMs,
		promptFile:  promptFile,
		// A stdin-fed one-shot CLI (codex) started without a prompt keeps
		// its stdin open so the first SendInput can deliver the prompt; that
		// SendInput then closes the pipe. Mirrors shouldCloseStdinAfterStart.
		deferredStdinClose: stdinPromptFormat(command) == "plain" && stdinPrompt == "",
		firstRealFrame:     make(chan struct{}),
		done:               make(chan struct{}),
		streamDone:         make(chan struct{}),
		publishFn:          publishFn,
	}

	sm.sessions[id] = session

	// Register the PID so the orphan scanner knows this process is backed by
	// an active session. removeSession() deregisters it on exit.
	if proc.Process != nil {
		globalProcessRegistry.Register(proc.Process.Pid, "session:"+id)
	}

	// Start output reader goroutines
	go sm.readOutputStream(session, publishFn)

	// Claude no-output watchdog is armed lazily: only once a prompt has actually
	// been delivered to claude. See armClaudeFirstFrameWatchdog — arming at
	// process start would incorrectly kill the supported no-initial-prompt
	// flow (chat-direct/session.sendInput) where claude sits idle until the
	// first SendInput. StartSession arms it below when it writes the initial
	// prompt; SendInput arms it on the first send otherwise.

	// Start process waiter (detects exit)
	go sm.waitForExit(session, publishFn)

	// waitForExit is now responsible for removing promptFile after the process
	// exits — disarm the early-return cleanup defer above.
	sessionOwnsPromptFile = true

	fmt.Printf("%s[session] Session %s started (PID: %d)%s\n",
		colorGreen, id, proc.Process.Pid, colorReset)

	// Deliver the initial prompt on stdin, framed per the target CLI's protocol.
	//
	//   "ndjson" — claude's --input-format stream-json mode. Wrap the prompt
	//              in the `{type:"user", message:{...}}` envelope and keep
	//              stdin open so the orchestrator can send follow-up turns
	//              via SendInput. Stdin closes when claude emits a "result"
	//              event (detected in readOutputStream).
	//   "plain"  — codex exec's stdin protocol (also used via the `-`
	//              positional placeholder). Write the prompt verbatim plus
	//              a trailing newline. codex reads stdin to completion
	//              before starting inference, then exits — stdin is closed
	//              right after the write via shouldCloseStdinAfterStart.
	//   ""       — no stdin prompt (positional argv path, or no prompt
	//              expected at all).
	if stdinPrompt != "" {
		var line string
		switch stdinPromptFormat(command) {
		case "ndjson":
			line = fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%s},"session_id":"%s","parent_tool_use_id":null}`,
				jsonEscapeString(stdinPrompt), id)
		case "plain":
			line = stdinPrompt
		default:
			// Defensive: a CLI router returned a stdinPrompt for a command
			// with no documented stdin format. Treat as plain text rather
			// than dropping the prompt silently.
			line = stdinPrompt
		}
		if _, err := fmt.Fprintln(session.Stdin, line); err != nil {
			fmt.Printf("%s[session] Failed to send initial prompt to %s: %v%s\n",
				colorRed, id, err, colorReset)
		} else {
			fmt.Printf("%s[session] Sent initial prompt to %s (%d chars, format=%s)%s\n",
				colorGreen, id, len(stdinPrompt), stdinPromptFormat(command), colorReset)
			// Prompt delivered — arm the claude no-output watchdog now.
			sm.armClaudeFirstFrameWatchdog(session, claudeFirstFrameTimeout)
		}
	}

	// Close stdin for one-shot sessions. Codex exec appends piped stdin to the
	// prompt, so leaving the pipe open makes it wait indefinitely for EOF.
	if shouldCloseStdinAfterStart(command, stdinPrompt) {
		session.Stdin.Close()
		fmt.Printf("%s[session] Closed stdin for one-shot session %s (%s)%s\n",
			colorYellow, id, command, colorReset)
	} else if utilitySession {
		// A utility never reads interactive stdin; close it so a child that does
		// read stdin sees EOF immediately instead of blocking on the open pipe.
		// (The /dev/tty prompt vector is already closed by the TTY detachment in
		// hardenNonAgentCommand.) Mutually exclusive with the one-shot close
		// above, which only fires for resident stdin-fed agents.
		session.Stdin.Close()
	}

	return nil
}

/* --------------------------------------------------------------------------
   SendInput — write to a session's stdin
   -------------------------------------------------------------------------- */

// SendInput writes text to the stdin of the specified session.
// For Claude sessions using --input-format stream-json, the text is wrapped
// in an NDJSON user message envelope.  For other CLIs it is sent as raw text.
func (sm *SessionManager) SendInput(id, text string) error {
	sm.mu.RLock()
	session, exists := sm.sessions[id]
	sm.mu.RUnlock()

	if !exists {
		return fmt.Errorf("session %s not found", id)
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	if session.Status == "ended" {
		return fmt.Errorf("session %s has ended", id)
	}

	// For Claude sessions, wrap input in NDJSON user message envelope
	payload := text
	if isClaudeCommand(session.Command) {
		payload = fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%s},"session_id":"%s","parent_tool_use_id":null}`,
			jsonEscapeString(text), id)
	}

	// Write input with timeout to prevent deadlock if the CLI process's
	// stdin pipe buffer is full (e.g., process is stalled or blocked).
	writeDone := make(chan error, 1)
	go func() {
		_, err := fmt.Fprintln(session.Stdin, payload)
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		if err != nil {
			return fmt.Errorf("failed to write to session %s stdin: %w", id, err)
		}
	case <-time.After(10 * time.Second):
		return fmt.Errorf("timeout writing to session %s stdin (pipe buffer full)", id)
	}

	// One-shot, stdin-fed CLIs (codex) started without a prompt held
	// their stdin open waiting for this first message. codex exec reads stdin
	// to EOF before running, so close the pipe now that the prompt is written —
	// otherwise the child waits forever for EOF. Done once: a subsequent
	// SendInput hits an already-ended one-shot session.
	if session.deferredStdinClose {
		session.deferredStdinClose = false
		session.Stdin.Close()
		fmt.Printf("%s[session] Closed stdin after first prompt for one-shot session %s (%s)%s\n",
			colorYellow, id, session.Command, colorReset)
	}

	// Reset status from waiting_input back to running
	if session.Status == "waiting_input" {
		session.Status = "running"
	}

	// Arm the claude no-output watchdog on the first prompt delivered via
	// SendInput. For sessions that started with an initial argv/stdin prompt
	// this is a no-op (sync.Once already fired in StartSession); for the
	// no-initial-prompt flow this is where the watchdog first arms — we now
	// have a prompt outstanding, so a subsequent stall means the same
	// "launched but not signed in" failure StartSession's arm would have
	// caught.
	sm.armClaudeFirstFrameWatchdog(session, claudeFirstFrameTimeout)

	fmt.Printf("%s[session] Input sent to %s: %s%s\n",
		colorBlue, id, truncateString(text, 80), colorReset)

	return nil
}

/* --------------------------------------------------------------------------
   SignalSession — send OS signal to a session's process
   -------------------------------------------------------------------------- */

// SignalSession sends an interrupt or kill signal to the session's process.
func (sm *SessionManager) SignalSession(id, signal string) error {
	sm.mu.RLock()
	session, exists := sm.sessions[id]
	sm.mu.RUnlock()

	if !exists {
		return fmt.Errorf("session %s not found", id)
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	if session.Status == "ended" {
		return fmt.Errorf("session %s has already ended", id)
	}

	switch signal {
	case "interrupt":
		// Send interrupt (Ctrl+C equivalent)
		if err := interruptProcess(session.Process); err != nil {
			return fmt.Errorf("failed to interrupt session %s: %w", id, err)
		}
		fmt.Printf("%s[session] Interrupt sent to %s%s\n", colorYellow, id, colorReset)
	case "kill":
		// Force kill
		if err := session.Process.Process.Kill(); err != nil {
			return fmt.Errorf("failed to kill session %s: %w", id, err)
		}
		fmt.Printf("%s[session] Kill sent to %s%s\n", colorRed, id, colorReset)
	default:
		return fmt.Errorf("unknown signal: %s (expected 'interrupt' or 'kill')", signal)
	}

	return nil
}

/* --------------------------------------------------------------------------
   EndSession — graceful shutdown with force kill fallback
   -------------------------------------------------------------------------- */

// EndSession attempts a graceful shutdown of the session, falling back to
// force kill after gracefulShutdownTimeout.
func (sm *SessionManager) EndSession(id string) error {
	sm.mu.RLock()
	session, exists := sm.sessions[id]
	sm.mu.RUnlock()

	if !exists {
		return fmt.Errorf("session %s not found", id)
	}

	session.mu.Lock()
	if session.Status == "ended" {
		session.mu.Unlock()
		// Already ended — just clean up from map
		sm.removeSession(id)
		return nil
	}
	session.mu.Unlock()

	fmt.Printf("%s[session] Ending session %s gracefully...%s\n", colorYellow, id, colorReset)

	// Try graceful interrupt first
	_ = interruptProcess(session.Process)

	// Wait for exit or timeout
	select {
	case <-session.done:
		// Process exited gracefully
	case <-time.After(gracefulShutdownTimeout):
		// Force kill after timeout
		fmt.Printf("%s[session] Force killing session %s (graceful shutdown timed out)%s\n",
			colorRed, id, colorReset)
		_ = session.Process.Process.Kill()
		<-session.done // Wait for exit after kill
	}

	sm.removeSession(id)
	return nil
}

/* --------------------------------------------------------------------------
   GetSession — lookup a session by ID
   -------------------------------------------------------------------------- */

// GetSession returns the session with the given ID, or nil if not found.
func (sm *SessionManager) GetSession(id string) *CLISession {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.sessions[id]
}

/* --------------------------------------------------------------------------
   CleanupStale — periodic goroutine to kill old sessions
   -------------------------------------------------------------------------- */

// CleanupStale runs periodically to kill sessions that exceed maxAge.
// Call this as a goroutine: go sm.CleanupStale(sessionMaxLifetime)
func (sm *SessionManager) CleanupStale(maxAge time.Duration) {
	ticker := time.NewTicker(sessionCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			sm.mu.RLock()
			var staleIDs []string
			for id, session := range sm.sessions {
				if time.Since(session.StartedAt) > maxAge {
					staleIDs = append(staleIDs, id)
				}
			}
			sm.mu.RUnlock()

			for _, id := range staleIDs {
				fmt.Printf("%s[session] Cleaning up stale session %s (exceeded %v)%s\n",
					colorYellow, id, maxAge, colorReset)
				_ = sm.EndSession(id)
			}
		case <-shutdownChan:
			return
		}
	}
}

/* --------------------------------------------------------------------------
   ActiveSessionCount — returns the number of active sessions
   -------------------------------------------------------------------------- */

// ActiveSessionCount returns the number of currently active sessions.
func (sm *SessionManager) ActiveSessionCount() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.sessions)
}

/* --------------------------------------------------------------------------
   Internal helpers
   -------------------------------------------------------------------------- */

// removeSession removes a session from the map and deregisters its PID from
// the orphan-scanner registry.
func (sm *SessionManager) removeSession(id string) {
	sm.mu.Lock()
	if s, ok := sm.sessions[id]; ok && s.Process != nil && s.Process.Process != nil {
		globalProcessRegistry.Deregister(s.Process.Process.Pid)
	}
	delete(sm.sessions, id)
	sm.mu.Unlock()
}

// shouldCloseStdinAfterStart decides whether to close the child process's
// stdin right after launch.
//
//   - Claude in `--input-format stream-json` mode reads NDJSON messages from
//     stdin in a loop. Closing stdin makes claude EOF and exit immediately —
//     which is the right call for one-shot launches (we sent the prompt as
//     args, claude responds, done), but the WRONG call for SESSION launches
//     where the orchestrator wants to keep talking to claude via SendInput.
//
//     Before: anyone calling `terminal({command: "claude"})` with no args had
//     stdin closed immediately → session ended in ~3s with empty output (the
//     same failure mode PR #133 patched for the bundled-kickoff path, but
//     that fix only forced a non-empty args[0]; plain TerminalTool with empty
//     args still hit the trap). Observed in chatDefault test runs where the
//     AI launched claude without an initial prompt and saw status=ended
//     with an empty output a few seconds later.
//
//     Now: keep stdin open for claude regardless of whether an initial prompt
//     was sent. If an initial prompt was provided, claude processes it on
//     start; either way subsequent NDJSON via SendInput keeps the conversation
//     going. The session is closed when claude itself exits (the "result"
//     event detection in readOutputStream) or when the orchestrator ends the
//     session explicitly.
//
//   - codex is one-shot AND receives its prompt via stdin (via the `-`
//     positional placeholder) so multi-KB briefs don't overflow Windows'
//     command-line cap. It reads stdin to completion before/at the start of
//     inference, so we ALWAYS close stdin after the prompt write — leaving it
//     open hangs the process indefinitely waiting for EOF.
//
//   - agy and all non-CLI commands (powershell, bash, git, ...) keep the
//     pre-existing rule: close stdin iff no stdinPrompt was queued. agy gets
//     its prompt on argv (it ignores piped stdin), so stdinPrompt is empty.
func shouldCloseStdinAfterStart(command string, stdinPrompt string) bool {
	// Route through commandBaseName so absolute/relative paths like
	// `/opt/bin/codex` follow the same stdin policy as bare names — otherwise
	// the argv builder would shape them as stdin-fed codex sessions while this
	// function left the pipe open, hanging the child waiting for EOF.
	base := commandBaseName(command)
	switch {
	case strings.HasPrefix(base, "claude"):
		return false
	case isOpenCodeCommand(command):
		// One-shot with the prompt on argv and NO stdin protocol: leaving the
		// pipe open would hand `opencode run` an input stream it waits on.
		// Close unconditionally — unlike codex there is no "session opened with
		// no prompt yet" case here, because this legacy path always carries its
		// prompt positionally.
		return true
	case strings.HasPrefix(base, "codex"):
		// One-shot, stdin-fed CLIs: close stdin right after start ONLY when a
		// prompt was delivered at start (the delegate/one-shot path). When the
		// session is started with NO prompt — the chat-direct flow eagerly
		// opens the session on model selection and delivers the first message
		// later via SendInput — closing here would hand the child an immediate
		// EOF with no prompt. codex v0.140+ then exits 1 with "No prompt
		// provided via stdin." before the user's first message ever arrives.
		// Keep stdin open in that case; SendInput closes it after writing the
		// first (and only) prompt. See deferredStdinClose.
		return stdinPrompt != ""
	}
	return stdinPrompt == ""
}

func waitForStreamCompletion(session *CLISession, timeout time.Duration) {
	if session.streamDone == nil {
		return
	}

	select {
	case <-session.streamDone:
	case <-time.After(timeout):
		fmt.Printf("%s[session] Timed out waiting for stream publish completion for %s%s\n",
			colorYellow, session.ID, colorReset)
	}
}

// detectCLITerminalEvent returns true if the JSON line marks the natural end of
// a CLI agent turn — Claude "result", Codex "thread.completed"/"turn.completed".
// Used to flush any pending stream batch before the CLI
// process exits, so the final text chunk does not race with session_ended.
//
// Returning true means: "this CLI has just announced it is done; flush now and
// expect process exit very soon." Unlike detectResultEvent, this does NOT cause
// stdin to be closed (only Claude needs that — codex exits on its own
// once stdin is closed at start). Detection here is best-effort: if a CLI emits
// a terminal event we don't recognise, the existing process-exit path still
// triggers the flush in the !ok branch of readOutputStream.
func detectCLITerminalEvent(command, line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	var event map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &event); err != nil {
		return false
	}
	eventType, _ := event["type"].(string)
	if eventType == "" {
		return false
	}

	base := commandBaseName(command)
	switch {
	case strings.HasPrefix(base, "claude"):
		return eventType == "result"
	case strings.HasPrefix(base, "codex"):
		// Codex emits turn.completed when the turn is done. thread.completed is
		// the very last event before the process exits.
		return eventType == "thread.completed" || eventType == "turn.completed"
	case strings.HasPrefix(base, "grok"):
		// Grok's `--output-format streaming-json` emits per-event frames
		// (thought / text / end); `end` marks the natural end of the turn,
		// right before the headless `-p` process exits.
		return eventType == "end"
	case isOpenCodeCommand(command):
		// `opencode run --format json` closes a turn with a session/step
		// completion event, immediately before the one-shot process exits.
		// Matched by suffix because the exact type name has moved across
		// releases; an unrecognised terminal event is harmless (the
		// process-exit path still flushes), a false positive is not, so
		// `error` is excluded.
		lowered := strings.ToLower(eventType)
		if strings.Contains(lowered, "error") {
			return false
		}
		return strings.HasSuffix(lowered, "completed") ||
			strings.HasSuffix(lowered, "done") ||
			lowered == "finish" ||
			lowered == "session.idle"
	}
	return false
}

// readOutputStream reads stdout and stderr from the session and publishes
// output chunks via the publishFn. It parses JSON events from structured
// output modes to detect permission/approval prompts.
func (sm *SessionManager) readOutputStream(session *CLISession, publishFn PublishFunc) {
	defer close(session.streamDone)

	// Merge stdout and stderr into a single channel
	lines := make(chan streamLine, 100)
	var wg sync.WaitGroup

	// PowerShell sessions can emit CLIXML blocks on stderr when -OutputFormat Text
	// is missing OR when the child process (e.g. a nested powershell.exe) ignores
	// the parent's format flag. A CLIXML block spans multiple lines:
	//   #< CLIXML
	//   <Objs ...>...</Objs>
	// The block must be dropped as a unit or the UI shows raw XML error records.
	// Each stream (stdout, stderr) gets its own filter state so an unclosed block
	// on one stream doesn't swallow lines on the other.
	var clixmlActive [2]bool // [0]=stdout, [1]=stderr
	skipCLIXML := func(slot int, text string) bool {
		trimmed := strings.TrimSpace(text)
		if clixmlActive[slot] {
			if strings.HasPrefix(trimmed, "</Objs>") || strings.HasSuffix(trimmed, "</Objs>") {
				clixmlActive[slot] = false
			}
			return true
		}
		if strings.HasPrefix(trimmed, "#< CLIXML") {
			clixmlActive[slot] = true
			return true
		}
		return false
	}

	wg.Add(2)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(session.Stdout)
		scanner.Buffer(make([]byte, 0, 256*1024), 30*1024*1024) // 30MB max line (large CLI agent output, encoded content)
		fmt.Printf("%s[session] stdout scanner started for %s%s\n", colorCyan, session.ID, colorReset)
		lineCount := 0
		for scanner.Scan() {
			lineCount++
			text := scanner.Text()
			if skipCLIXML(0, text) {
				continue
			}
			if lineCount <= 3 {
				fmt.Printf("%s[session] stdout[%d] %s: %s%s\n",
					colorCyan, lineCount, session.ID, truncateString(text, 120), colorReset)
			}
			lines <- streamLine{text: text, source: "stdout"}
		}
		if err := scanner.Err(); err != nil {
			fmt.Printf("%s[session] stdout scanner error for %s: %v%s\n", colorRed, session.ID, err, colorReset)
		}
		fmt.Printf("%s[session] stdout scanner done for %s (%d lines)%s\n", colorYellow, session.ID, lineCount, colorReset)
	}()
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(session.Stderr)
		scanner.Buffer(make([]byte, 0, 256*1024), 30*1024*1024) // 30MB max line (large CLI agent output, encoded content)
		fmt.Printf("%s[session] stderr scanner started for %s%s\n", colorCyan, session.ID, colorReset)
		lineCount := 0
		for scanner.Scan() {
			lineCount++
			text := scanner.Text()
			if skipCLIXML(1, text) {
				continue
			}
			if lineCount <= 3 {
				fmt.Printf("%s[session] stderr[%d] %s: %s%s\n",
					colorYellow, lineCount, session.ID, truncateString(text, 120), colorReset)
			}
			lines <- streamLine{text: text, source: "stderr"}
		}
		if err := scanner.Err(); err != nil {
			fmt.Printf("%s[session] stderr scanner error for %s: %v%s\n", colorRed, session.ID, err, colorReset)
		}
		fmt.Printf("%s[session] stderr scanner done for %s (%d lines)%s\n", colorYellow, session.ID, lineCount, colorReset)
	}()

	// Close lines channel when both readers are done
	go func() {
		wg.Wait()
		close(lines)
	}()

	// Batch output and publish periodically.
	// publishFn blocks for up to 30 s on Pub/Sub network I/O.  Calling it
	// directly inside the select loop would stall the consumer goroutine,
	// filling the lines channel (capacity 100) and eventually blocking the
	// scanner goroutines that feed it — starving the CLI process's pipe buffer.
	// Instead we publish in a fire-and-forget goroutine so the select loop
	// always stays free to drain incoming lines.  The publish wait group lets
	// waitForExit avoid marking the session ended before final chunks are sent.
	publishSem := make(chan struct{}, 5) // max 5 concurrent publishes per session
	var publishWg sync.WaitGroup
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
		case <-time.After(5 * time.Second):
			publishWg.Done()
			// All publish slots busy for 5s — drop to prevent goroutine buildup
			fmt.Printf("%s[session] Publish timeout, dropping batch for %s%s\n",
				colorYellow, session.ID, colorReset)
		}
	}

	var batch []string
	batchTimer := time.NewTicker(streamBatchInterval)
	defer batchTimer.Stop()

	flushBatch := func() {
		if len(batch) == 0 {
			return
		}
		output := strings.Join(batch, "\n")
		seq := atomic.AddInt64(&session.Seq, 1)

		asyncPublish(resultMsg{
			ID:          session.ID,
			WorkspaceID: session.WorkspaceID,
			UID:         session.UID,
			Output:      output,
			Status:      "success",
			Ts:          time.Now().UnixMilli(),
			Version:     Version,
			Type:        "stream",
			SessionID:   session.ID,
			Seq:         int(seq),
		})

		batch = batch[:0]
	}

	appendDisplayText := func(lineText string) {
		displayText := extractDisplayText(session.Command, lineText)
		if displayText == "" {
			return
		}
		batch = append(batch, displayText)
		// Genuine assistant output (text/thinking delta or tool_use)
		// — the session is alive and producing, so disarm the claude
		// no-output watchdog. No-op for non-claude sessions (they
		// never arm it). Deliberately NOT signalled from init/system
		// or successful result events: those can fire for a stalled
		// claude and the parser swallows them. Prompt detection
		// (permission_request/input_request) also disarms the watchdog
		// — see the sibling branch below.
		//
		// Gate on isClaudeStructuredStreamLine so a not-signed-in
		// claude that prints a plain stderr/login banner (which
		// extractDisplayText passes through verbatim) does NOT disarm
		// the fail-fast watchdog — otherwise a stalled session with a
		// banner would hang until stale GC.
		if isClaudeCommand(session.Command) && isClaudeStructuredStreamLine(lineText) {
			session.signalFirstRealFrame()
		}
	}

	for {
		select {
		case line, ok := <-lines:
			if !ok {
				// All readers done — flush remaining
				flushBatch()
				return
			}

			// Claude rate-limit telemetry: passively cache the structured
			// per-window snapshot (used by the CLI Agents tab) from the
			// rate_limit_event stream we already read. When a window is
			// hard-rejected (limit hit), also emit a synthetic, orchestrator-
			// parseable limit line carrying the exact reset time — Claude's own
			// rejection text is zone-less prose ("resets 3:45pm"), but the
			// rate_limit_event gives us an unambiguous epoch. This is what lets
			// agent-orchestrator-service auto-defer + resume instead of
			// completing the step empty.
			// Codex rate-limit telemetry: token_count frames from `codex exec
			// --json` carry the same primary/secondary rate_limits payload as
			// the app-server stream. Without this hook, normal terminal Codex
			// sessions never populate codex_rate_limits.json and the CLI Agents
			// card stays Unknown for users who don't go through app-server.
			if isCodexCommand(session.Command) {
				captureCodexRateLimitLine(line.text, time.Now())
			}

			// Grok usage-limit telemetry: xAI exposes no numeric quota, but the
			// server volunteers a discrete `usage_limit_reached` / credit-limit /
			// access-gate frame on the streaming-json output when you near or hit
			// the cap. Capture it (best-effort) so the CLI Agents card can show a
			// warning instead of a permanently-Unknown bar.
			if isGrokCommand(session.Command) {
				captureGrokUsageLimitLine(line.text, time.Now())
			}

			if isClaudeCommand(session.Command) {
				if rejected := captureClaudeRateLimitLine(line.text, time.Now()); rejected != nil {
					flushBatch()
					seq := atomic.AddInt64(&session.Seq, 1)
					asyncPublish(resultMsg{
						ID:          session.ID,
						WorkspaceID: session.WorkspaceID,
						UID:         session.UID,
						Output:      formatClaudeLimitLine(*rejected) + "\n",
						Status:      "success",
						Ts:          time.Now().UnixMilli(),
						Version:     Version,
						Type:        "stream",
						SessionID:   session.ID,
						Seq:         int(seq),
					})
					fmt.Printf("%s[session] Claude rate limit rejected — %s resets %s%s\n",
						colorYellow, session.ID,
						time.UnixMilli(rejected.ResetsAtMs).UTC().Format(time.RFC3339), colorReset)
				}

				// Any Claude rate_limit_event — allowed heartbeat OR handled
				// rejection — proves Claude's stream reached us: it is signed
				// in and emitting per-window telemetry. extractDisplayText
				// treats rate_limit_event as metadata so the sibling disarm
				// below never fires, leaving the no-output watchdog armed to
				// publish a misleading /login error 120s later and kill a
				// healthy (or correctly rate-limited) turn where the only
				// early output was heartbeats. captureClaudeRateLimitLine
				// only returns rejected buckets, so the allowed-heartbeat
				// case needs its own detector. Disarm here so the fail-fast
				// path stays scoped to genuine startup stalls.
				if isClaudeRateLimitEventLine(line.text) {
					session.signalFirstRealFrame()
				}

				// Fail fast when Claude Code reports it cannot authenticate. The
				// driver strips env-based credentials for claude to force the
				// user's `/login` subscription, so an expired/absent login here
				// surfaces as an api_retry(authentication_failed) or a
				// result(is_error) "…/login" message and the session would
				// otherwise idle until the 6h stale GC and complete empty. Publish
				// an actionable error and kill the child so waitForExit emits the
				// ended frame. Seq is reserved before Kill so error orders before
				// ended. Mirrors the grok "not signed in" fail-fast.
				if authFail := detectClaudeAuthFailure(line.text); authFail != nil {
					flushBatch()
					seq := atomic.AddInt64(&session.Seq, 1)
					asyncPublish(resultMsg{
						ID:          session.ID,
						WorkspaceID: session.WorkspaceID,
						UID:         session.UID,
						Output: fmt.Sprintf(
							"claude is not signed in / not authorized on this computer (%s) — run `claude` then "+
								"`/login` in a terminal on the terminal computer to sign in again. Session terminated.",
							authFail.Category,
						),
						Status:    "error",
						Ts:        time.Now().UnixMilli(),
						Version:   Version,
						Type:      "stream",
						SessionID: session.ID,
						Seq:       int(seq),
					})
					fmt.Printf("%s[session] Claude auth failure (%s) — %s terminated%s\n",
						colorRed, authFail.Category, session.ID, colorReset)
					if session.Process != nil && session.Process.Process != nil {
						_ = session.Process.Process.Kill()
					}
					continue
				}
			}

			// Detect CLI-terminal events (Claude "result", Codex
			// "thread.completed"/"turn.completed"). When we see
			// one we flush any buffered text BEFORE the CLI process exits — the
			// process-exit path also flushes via the !ok branch, but on a fast
			// exit (Claude after stdin close, codex after final event)
			// the timing race can leave the final batch in flight while
			// session_ended is already being published. Flushing here guarantees
			// the last chunk is enqueued for publish before the exit cascade.
			terminalEvent := detectCLITerminalEvent(session.Command, line.text)
			if terminalEvent {
				// A failed Claude result carries its only useful error text in
				// the terminal event itself. Add it before flushing so its stream
				// sequence precedes the turn_complete prompt sequence.
				appendDisplayText(line.text)
				flushBatch()
			}

			// For Claude stream-json: detect the "result" event that signals
			// the turn is complete. Keep the session alive — claude was launched
			// WITHOUT `-p`, so after emitting result it will sit on stdin waiting
			// for the next NDJSON user message. We flag the in-memory status as
			// "waiting_input" and publish a `prompt`-typed message so the
			// terminal-service pubsub consumer flips the Firestore session doc
			// to `status: "waiting_input"` — which is what the AOS
			// terminal.session.sendInput "settle" wait listens for.
			//
			// NOTE: stdin stays open intentionally. The previous behaviour
			// (close stdin on result → claude exits → next sendInput hits
			// "session already ended") broke the kickoff-then-sendInput pattern
			// that codeImplementation relies on across steps 11→14→15.
			if detectResultEvent(session.Command, line.text) {
				session.mu.Lock()
				session.Status = "waiting_input"
				session.mu.Unlock()
				seq := atomic.AddInt64(&session.Seq, 1)
				asyncPublish(resultMsg{
					ID:          session.ID,
					WorkspaceID: session.WorkspaceID,
					UID:         session.UID,
					Output:      "",
					Status:      "success",
					Ts:          time.Now().UnixMilli(),
					Version:     Version,
					Type:        "prompt",
					SessionID:   session.ID,
					PromptText:  "",
					PromptType:  "turn_complete",
					Seq:         int(seq),
				})
				fmt.Printf("%s[session] Result event — turn complete, %s waiting_input%s\n",
					colorGreen, session.ID, colorReset)

				// A Claude result event means the turn reached terminal state —
				// Claude ran through to completion (even an empty/non-auth
				// is_error result still proves the child was responsive). Auth
				// failures are caught upstream by detectClaudeAuthFailure, so
				// anything reaching here is a healthy turn. Disarm the
				// no-output watchdog so it doesn't later publish a misleading
				// /login error and kill a session that already completed.
				session.signalFirstRealFrame()
			}

			// Try to parse as JSON event for structured detection
			if promptInfo := detectPromptFromJSON(session.Command, line.text); promptInfo != nil {
				// Flush any buffered output first
				flushBatch()

				// Update session status
				session.mu.Lock()
				session.Status = "waiting_input"
				session.mu.Unlock()

				seq := atomic.AddInt64(&session.Seq, 1)

				asyncPublish(resultMsg{
					ID:          session.ID,
					WorkspaceID: session.WorkspaceID,
					UID:         session.UID,
					Output:      line.text,
					Status:      "success",
					Ts:          time.Now().UnixMilli(),
					Version:     Version,
					Type:        "prompt",
					SessionID:   session.ID,
					PromptText:  promptInfo.Text,
					PromptType:  promptInfo.Type,
					Seq:         int(seq),
				})

				fmt.Printf("%s[session] Prompt detected in %s: %s%s\n",
					colorMagenta, session.ID, truncateString(promptInfo.Text, 80), colorReset)

				// A claude permission/input prompt means the session is healthy
				// and blocked on the user — not stalled. Disarm the no-output
				// watchdog so it doesn't kill a session that's legitimately
				// waiting on a response with a misleading sign-in error.
				if isClaudeCommand(session.Command) {
					session.signalFirstRealFrame()
				}
			} else if !terminalEvent {
				appendDisplayText(line.text)
			}

		case <-batchTimer.C:
			flushBatch()
		}
	}
}

// armClaudeFirstFrameWatchdog starts the no-output watchdog exactly once per
// claude session, on the first call. Called from StartSession right after the
// initial prompt is written, and from SendInput on the first user message when
// the session was opened without an initial prompt (chat-direct flow). No-op
// for non-claude sessions and when publishFn was not captured.
func (sm *SessionManager) armClaudeFirstFrameWatchdog(session *CLISession, timeout time.Duration) {
	if session == nil || !isClaudeCommand(session.Command) || session.publishFn == nil {
		return
	}
	session.claudeWatchdogOnce.Do(func() {
		go sm.watchClaudeFirstFrame(session, session.publishFn, timeout)
	})
}

// watchClaudeFirstFrame fails a claude session fast when it produces no genuine
// assistant output within `timeout` of starting. This is the "launched but not
// signed in / wedged at startup" case: the child emits its system/init and then
// nothing, so without this watchdog the session hangs until the 6h stale GC and
// the design step completes empty. Mirrors the grok ACP first-frame watchdog.
//
// It returns early when the session proves itself healthy (firstRealFrame) or
// has already exited (done — waitForExit owns the ended frame). `timeout` is a
// parameter so unit tests can drive the fail-fast path with a sub-second budget.
//
// The error is PUBLISHED before Kill so this frame lands on publishFn strictly
// before waitForExit's session_ended: Kill unblocks Process.Wait(), and only
// after Wait returns does waitForExit publish session_ended. Because publishFn
// here is a synchronous call outside readOutputStream's publishWg, publishing
// after Kill would race — a slow publish could arrive after session_ended,
// violating the session_integration_test.go invariant that nothing is
// published after session_ended. Seq is still reserved before publish so the
// orchestrator can also order by Seq.
func (sm *SessionManager) watchClaudeFirstFrame(session *CLISession, publishFn PublishFunc, timeout time.Duration) {
	select {
	case <-session.firstRealFrame:
		return
	case <-session.done:
		return
	case <-time.After(timeout):
	}

	// Lost the race against a frame/exit that landed as the timer fired?
	// Re-check both non-blockingly so we never kill a session that just proved
	// itself alive (or already terminated on its own).
	select {
	case <-session.firstRealFrame:
		return
	case <-session.done:
		return
	default:
	}
	session.mu.Lock()
	ended := session.Status == "ended"
	session.mu.Unlock()
	if ended {
		return
	}

	seq := atomic.AddInt64(&session.Seq, 1)
	fmt.Printf("%s[session] Claude produced no output within %v — assuming not signed in / startup stall, killing %s%s\n",
		colorYellow, timeout, session.ID, colorReset)
	publishFn(resultMsg{
		ID:          session.ID,
		WorkspaceID: session.WorkspaceID,
		UID:         session.UID,
		Output: fmt.Sprintf(
			"claude produced no output within %v of starting — it is most likely not signed in on this computer "+
				"(run `claude` then `/login` in a terminal on the terminal computer to sign in again) or wedged at "+
				"startup. Session terminated.",
			timeout,
		),
		Status:    "error",
		Ts:        time.Now().UnixMilli(),
		Version:   Version,
		Type:      "stream",
		SessionID: session.ID,
		Seq:       int(seq),
	})
	if session.Process != nil && session.Process.Process != nil {
		_ = session.Process.Process.Kill()
	}
}

// waitForExit waits for the session's process to exit and publishes a
// session_ended result.
func (sm *SessionManager) waitForExit(session *CLISession, publishFn PublishFunc) {
	// Set up per-session timeout — kill the process if it exceeds timeoutMs
	var timeoutTimer *time.Timer
	if session.TimeoutMs > 0 {
		timeoutTimer = time.AfterFunc(time.Duration(session.TimeoutMs)*time.Millisecond, func() {
			fmt.Printf("%s[session] Session %s timed out after %dms — killing%s\n",
				colorYellow, session.ID, session.TimeoutMs, colorReset)
			if session.Process.Process != nil {
				session.Process.Process.Kill()
			}
		})
	}

	err := session.Process.Wait()

	if timeoutTimer != nil {
		timeoutTimer.Stop()
	}

	// Drain the readOutputStream scanners BEFORE closing the owned os.Pipe read
	// ends. With parent-owned read ends, Process.Wait() returning does NOT close
	// them, so force-closing here immediately (as this used to) races the
	// scanner: a fast-exiting child (`sh -c 'printf …'`) can be reaped before the
	// scanner has drained the kernel pipe buffer, discarding its final output —
	// the exact loss the owned-read-end change (see StartSession pipe comment)
	// was meant to fix. The child's write ends are already closed (Wait only
	// returns post-exit), so the scanners reach EOF naturally; a wedged scanner
	// (e.g. a lingering Windows pipe handle that never releases on exit) falls
	// through to the drain timeout and we force-close to unblock it. Same pattern
	// as the Claude/Grok/Codex native managers.
	select {
	case <-session.streamDone:
	case <-time.After(sessionStreamDrainTimeout):
		fmt.Printf("%s[session] Stream drain timed out for %s — forcing pipe close%s\n",
			colorYellow, session.ID, colorReset)
		session.Stdout.Close()
		session.Stderr.Close()
	}

	// Mop up the parent read ends. Close-after-Close returns ErrClosed with no
	// side effects, so repeating the force-close branch above is safe.
	session.Stdout.Close()
	session.Stderr.Close()

	// Remove the grok prompt temp file now that the process has exited — doing
	// it before Wait() returned would risk pulling the file out from under a
	// still-running grok. Empty for every non-grok session and grok subcommand
	// carve-outs, so this is a no-op there. Runs exactly once per session.
	if session.promptFile != "" {
		_ = os.Remove(session.promptFile)
	}

	// 120s rather than 45s — publishFn can block up to 30s per pubsub.Publish
	// and the asyncPublish semaphore has 5 slots, so a fully-loaded queue at
	// exit can legitimately need ~30s to drain. 45s was tight enough that a
	// single slow Publish would time us out and let session_ended race the
	// final stream chunk, which is what produced the "agent didn't wait for
	// terminal response — calls cross between steps" report on documentDesign.
	waitForStreamCompletion(session, 120*time.Second)

	session.mu.Lock()
	session.Status = "ended"
	if err != nil {
		// Try to extract exit code
		if exitErr, ok := err.(*exec.ExitError); ok {
			session.ExitCode = exitErr.ExitCode()
		} else {
			session.ExitCode = -1
		}
	}
	session.mu.Unlock()

	close(session.done)

	seq := atomic.AddInt64(&session.Seq, 1)

	// Detect and upload output files (screenshots, test artifacts) before
	// publishing session_ended so that file metadata is included in the
	// message. Note: we intentionally do NOT gate on session.ExitCode == 0 —
	// a UI test that screenshots right before crashing is the case where we
	// MOST want the image to reach the orchestrator.
	// Shared with every bundled-CLI manager (see session_artifacts.go): the
	// scan used to live inline here, which is why only this PTY path ever
	// uploaded anything.
	uploadedFiles, uploadErrors := collectSessionArtifacts(
		sm.Config,
		session.ID,
		session.WorkspaceID,
		session.Process.Dir,
		session.StartedAt,
	)

	// Publish session_ended in a goroutine: publishFn blocks up to 30 s on
	// Pub/Sub network I/O.  Calling it directly here would delay removeSession
	// (and therefore free the session slot for reuse) by up to 30 s, and would
	// race the async stream publishes already in-flight from readOutputStream —
	// the session_ended message could arrive at the client before the last
	// streamed lines despite having a higher sequence number.
	publishTerminalResultAsync(publishFn, resultMsg{
		ID:           session.ID,
		WorkspaceID:  session.WorkspaceID,
		UID:          session.UID,
		Output:       fmt.Sprintf("Session ended (exit code: %d)", session.ExitCode),
		Status:       "success",
		Ts:           time.Now().UnixMilli(),
		Version:      Version,
		Type:         "session_ended",
		SessionID:    session.ID,
		ExitCode:     session.ExitCode,
		Seq:          int(seq),
		Files:        uploadedFiles,
		UploadErrors: uploadErrors,
	})

	fmt.Printf("%s[session] Session %s ended (exit code: %d)%s\n",
		colorYellow, session.ID, session.ExitCode, colorReset)

	// Remove from session map
	sm.removeSession(session.ID)
}

/* --------------------------------------------------------------------------
   Child-process env sanitisation
   -------------------------------------------------------------------------- */

// claudeAlwaysStripped is the set of env-var prefixes we drop from every
// spawned session, regardless of command. They identify or configure a
// surrounding Claude Code / Claude IDE context (CLAUDECODE,
// CLAUDE_CODE_ENTRYPOINT, CLAUDE_AGENT_SDK_VERSION, …) that, if leaked into
// the child, makes claude believe it is nested inside another Claude session
// or an IDE that isn't actually present.
//
// CLAUDE_CODE_OAUTH_TOKEN intentionally falls under this prefix sweep. The
// integration relies on the user's interactive `/login` credentials stored
// in ~/.claude/.credentials.json — there is no current code path that needs
// a headless OAuth token injected via env. If a future maintainer wants
// subscription-safe headless token support they should add an explicit
// whitelist here rather than discovering the strip by accident.
var claudeAlwaysStripped = []string{
	"CLAUDECODE=",
	"CLAUDE_",
}

// claudeBillingStripped is the set of env-var prefixes that would silently
// redirect a spawned Claude Code session away from the user's `/login`
// subscription credentials and onto API-key / OAuth-token billing. Anthropic
// SDK precedence puts these ahead of the stored subscription token, so a
// developer who keeps ANTHROPIC_API_KEY in their shell for unrelated SDK
// work would otherwise unknowingly bill their company API wallet for every
// interactive session this driver launches.
//
// Policy (per CLI_AGENT_INTEGRATION.md): force subscription billing — no
// opt-in API-key escape hatch. A user who genuinely wants API-key billing
// can run `claude` directly outside the driver.
var claudeBillingStripped = []string{
	"ANTHROPIC_API_KEY=",
	"ANTHROPIC_AUTH_TOKEN=",
}

// sanitizeClaudeChildEnv returns env with any variable that would confuse a
// spawned CLI agent (or, for claude specifically, would override the user's
// subscription credentials) removed. The second return value lists the names
// of the stripped variables in the order they appeared, suitable for an
// auditable [session] log line.
//
// The billing-var strip is gated on isClaudeCommand(command): codex
// / arbitrary shells are unaffected so they keep working with whatever auth
// the user has configured for those tools.
func sanitizeClaudeChildEnv(command string, env []string) ([]string, []string) {
	stripClaudeBilling := isClaudeCommand(command)

	filtered := make([]string, 0, len(env))
	var stripped []string
	for _, e := range env {
		upper := strings.ToUpper(e)

		drop := false
		for _, p := range claudeAlwaysStripped {
			if strings.HasPrefix(upper, p) {
				drop = true
				break
			}
		}
		if !drop && stripClaudeBilling {
			for _, p := range claudeBillingStripped {
				if strings.HasPrefix(upper, p) {
					drop = true
					break
				}
			}
		}
		if drop {
			if eq := strings.Index(e, "="); eq > 0 {
				stripped = append(stripped, e[:eq])
			}
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered, stripped
}

// prepareClaudeChildEnv sanitises the parent environment for a spawned CLI
// session and, for claude specifically, pins CLAUDE_CODE_ENTRYPOINT=cli.
//
// sanitizeClaudeChildEnv has already stripped any inherited
// CLAUDE_CODE_ENTRYPOINT — it could be "claude-vscode" / "sdk-ts" if this
// agent was itself launched from a host IDE or the Agent SDK. We then set the
// honest "cli" value so the spawned session self-identifies as an interactive
// CLI session, which is the launch shape this driver actually uses.
//
// This is load-bearing for billing. Starting 2026-06-15 Anthropic routes
// `claude -p` / Agent SDK usage on Pro/Max/Team subscriptions through a
// separate Agent SDK credit pool, classified in part by entrypoint. Pinning
// "cli" makes the favourable interactive classification deterministic instead
// of relying on claude's default-when-unset. It is the truthful tag for an
// interactive session — NOT spoofing (we never set "claude-vscode"/"sdk-ts").
//
// Non-claude commands (codex / shells) get only the sanitise step;
// no entrypoint is injected.
func prepareClaudeChildEnv(command string, env []string) ([]string, []string) {
	filtered, stripped := sanitizeClaudeChildEnv(command, env)
	if isClaudeCommand(command) {
		filtered = append(filtered, "CLAUDE_CODE_ENTRYPOINT=cli")
	}
	return filtered, stripped
}

// commandBaseName returns the lowercased basename of command with any
// Windows shim extension (.exe / .cmd / .bat / .ps1) stripped. Backslashes
// are normalized to forward slashes first so Windows-style paths
// (e.g. `C:\Users\u\AppData\Roaming\npm\claude.cmd`) resolve correctly on
// non-Windows builds where `filepath.Base` only treats `/` as a separator.
//
// All command-routing sites (argv builder, executable resolver, prompt
// detector, stdin envelope writer, billing-var strip) MUST classify
// commands through this helper so the routing predicate stays single-
// sourced — otherwise an absolute path like `/usr/local/bin/claude` could
// fall through one site but not another and silently regain API-key
// billing (or skip the stream-json shaping that the driver depends on).
func commandBaseName(command string) string {
	if command == "" {
		return ""
	}
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(command, `\`, "/")))
	for _, ext := range []string{".exe", ".cmd", ".bat", ".ps1"} {
		base = strings.TrimSuffix(base, ext)
	}
	return base
}

// isClaudeCommand reports whether command would be routed to the `claude`
// CLI by buildInteractiveCLIArgs. Accepts bare names, Windows shims
// (.exe/.cmd/.bat/.ps1), and absolute / relative paths. Uses the same
// basename-prefix predicate as the router so the billing-var strip can't
// drift out of sync — if a command gets claude argv shaping, it MUST also
// get the claude env policy, or a `claude-edge` (or any future `claude*`
// variant) would silently regain API-key billing.
// Used to gate the ANTHROPIC_* billing-var strip in sanitizeClaudeChildEnv.
func isClaudeCommand(command string) bool {
	return strings.HasPrefix(commandBaseName(command), "claude")
}

// isCodexCommand reports whether command would be routed to the `codex` CLI
// by buildInteractiveCLIArgs. Used to gate the Codex rate-limit cache writer
// in the session output loop so `token_count` frames from `codex exec --json`
// populate codex_rate_limits.json just like the app-server reader does.
func isCodexCommand(command string) bool {
	return strings.HasPrefix(commandBaseName(command), "codex")
}

// isGrokCommand reports whether command would be routed to the `grok` CLI by
// buildInteractiveCLIArgs. Used to gate the Grok usage-limit capture in the
// session output loop so a `usage_limit_reached` / gate frame from
// `grok --output-format streaming-json` populates grok_usage_limit.json.
func isGrokCommand(command string) bool {
	return strings.HasPrefix(commandBaseName(command), "grok")
}

// isAntigravityCommand reports whether command routes to the Antigravity CLI.
// Accepts BOTH the `agy` binary and the `antigravity` alias — the PTY
// eligibility allowlist (isPTYEligibleCommand) admits both, so both MUST get
// the same one-shot argv shaping (`--dangerously-skip-permissions --print
// <prompt>`) in buildInteractiveCLIArgs; otherwise a `tty=true` `antigravity`
// session would start under a PTY with no prompt flag and hang. Robust to
// paths / .exe shims.
func isAntigravityCommand(command string) bool {
	base := commandBaseName(command)
	return strings.HasPrefix(base, "agy") || strings.HasPrefix(base, "antigravity")
}

// isOpenCodeCommand reports whether command routes to the OpenCode CLI.
// Robust to paths / .exe / .cmd shims, like its siblings.
//
// Prefix-matching `opencode` (rather than an exact compare) keeps the argv
// shaping, the resident-agent classification and the stdin policy in lockstep
// for any future `opencode-nightly`-style variant — a variant that got the
// shaping but not the classification would be headless-hardened and then handed
// interactive-mode argv.
func isOpenCodeCommand(command string) bool {
	return strings.HasPrefix(commandBaseName(command), "opencode")
}

// isOpenCodeSynthesizedRun reports whether args match the forced one-shot
// `run --format json ...` shape that buildOpenCodeInteractiveArgs produces.
func isOpenCodeSynthesizedRun(args []string) bool {
	return len(args) >= 3 && args[0] == "run" && args[1] == "--format" && args[2] == "json"
}

// isResidentAgentSessionCommand reports whether a session_start command is a
// resident CLI agent that keeps its interactive-capable env. These are exactly
// the commands buildInteractiveCLIArgs shapes (claude/codex/grok) plus the
// PTY-only TUI agents (agy/antigravity). Everything else is a one-shot utility
// (bash/sh/git/PowerShell/test runner) that MUST be headless-hardened on the
// pipe session path — see StartSession.
//
// Gemini is intentionally NOT listed: its CLI-agent router was removed, so a
// stale or manually approved `gemini` session_start now falls through to
// generic execution and MUST be headless-hardened like any other non-agent
// command (otherwise it would keep the interactive terminal behavior #69
// removed from generic session commands).
func isResidentAgentSessionCommand(command string) bool {
	base := commandBaseName(command)
	switch {
	case strings.HasPrefix(base, "claude"),
		strings.HasPrefix(base, "codex"),
		strings.HasPrefix(base, "grok"):
		return true
	}
	// OpenCode belongs here for the same reason `grok` does, and the failure
	// mode if it is omitted is the one grok_acp.go documents: a bare `opencode`
	// launches the interactive TUI, which on a headless remote session produces
	// escape-sequence noise and never exits. Classifying it as a resident agent
	// is what routes it through buildOpenCodeInteractiveArgs, which forces
	// `run --format json` and can never fall through to the TUI.
	return isAntigravityCommand(command) || isOpenCodeCommand(command)
}

/* --------------------------------------------------------------------------
   CLI argument builders
   -------------------------------------------------------------------------- */

// buildInteractiveCLIArgs builds CLI arguments for interactive streaming mode.
// Each CLI agent has different flags for structured JSON output.
// Returns (cliArgs, stdinPrompt) — stdinPrompt is non-empty when the prompt
// is routed via stdin rather than argv. Per-CLI conventions:
//   - claude:      stdinPrompt is the prompt, sent as NDJSON via stream-json
//     input mode; multi-turn (stdin stays open)
//   - codex:       stdinPrompt is the prompt, written as raw text; codex exec
//     reads stdin to completion (`-` positional placeholder)
//     then exits; one-shot per process
//   - antigravity: prompt as the VALUE of `--print` (agy ≥ 1.1.x; verified
//     1.1.2 / 1.1.11). Order is `--dangerously-skip-permissions --print
//     <prompt>` — a bare `--print` followed by another flag makes agy treat
//     that flag as the prompt. agy does NOT read piped stdin (verified
//     against agy 1.0.4: ignored in both interactive and --print modes —
//     it needs a real TTY), so the prompt must stay on argv. agy resolves
//     to a native `agy.exe` (NOT a cmd.exe shim), so the relevant cap is
//     the 32KB CreateProcess limit, not an 8191-char cmd.exe cap.
//   - opencode:    forced `run --format json` with the prompt as a trailing
//     positional. `opencode run` is one-shot and does not
//     hold a stdin protocol open, so stdinPrompt is "" and
//     stdin is closed right after start. (The RESIDENT chat
//     path in opencode_native.go delivers the prompt on stdin
//     from a temp file instead; this legacy session_start /
//     PTY path keeps it positional, which is why it enforces
//     a byte cap the resident path does not need.)
//   - other:       prompt stays in args
//
// The caller (StartSession) uses stdinPromptFormat() to decide how to wrap
// the stdinPrompt before writing it to the process stdin.
func buildInteractiveCLIArgs(command string, args []string, enableGrokAlwaysApprove bool) ([]string, string) {
	base := commandBaseName(command)

	switch {
	case strings.HasPrefix(base, "claude"):
		return buildClaudeInteractiveArgs(args)
	case strings.HasPrefix(base, "codex"):
		return buildCodexInteractiveArgs(args)
	case isAntigravityCommand(command):
		return buildAntigravityInteractiveArgs(args), ""
	case isOpenCodeCommand(command):
		return buildOpenCodeInteractiveArgs(args), ""
	case strings.HasPrefix(base, "grok"):
		return buildGrokInteractiveArgs(args, enableGrokAlwaysApprove), ""
	default:
		return args, ""
	}
}

// stdinPromptFormat returns how the initial stdinPrompt should be written
// to the process stdin in StartSession. Empty string means no prompt is
// expected on stdin (positional argv path).
//
//	"ndjson" — wrap as `{"type":"user","message":{...}}` for claude's
//	           --input-format stream-json mode
//	"plain"  — write the prompt text verbatim followed by a newline
//	"" (default) — no stdin prompt; nothing to write
//
// Centralises the per-CLI knowledge so the StartSession write path doesn't
// hard-code claude's protocol against any non-empty stdinPrompt.
func stdinPromptFormat(command string) string {
	base := commandBaseName(command)
	switch {
	case strings.HasPrefix(base, "claude"):
		return "ndjson"
	case strings.HasPrefix(base, "codex"):
		return "plain"
	}
	return ""
}

// buildClaudeInteractiveArgs builds Claude Code CLI args for bidirectional
// stream-json mode.  The prompt is NOT passed as a CLI arg — it is returned
// separately so the caller can send it as an NDJSON message on stdin.
// Returns (cliArgs, promptText).
//
// IMPORTANT: this path is for INTERACTIVE multi-turn sessions. We never add
// `-p` / `--print` — that flag puts claude in one-shot mode and exits after
// the first response, killing the session before a follow-up `session.sendInput`
// can land. Any user-supplied `-p`/`--print` is stripped for the same reason.
// Callers that want one-shot claude must use the non-session execute.runAndWait
// path (which builds its own argv outside this function).
func buildClaudeInteractiveArgs(args []string) ([]string, string) {
	result := []string{
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--dangerously-skip-permissions",
	}

	// Claude flags that consume the next argument as their value.
	// Without this, "--model sonnet" would treat "sonnet" as a prompt word.
	valuedFlags := map[string]bool{
		"--model": true, "--system-prompt": true, "--append-system-prompt": true,
		"--permission-mode": true, "--max-budget-usd": true, "--effort": true,
		"--agent": true, "--agents": true, "--session-id": true,
		"--mcp-config": true, "--settings": true, "--json-schema": true,
		"--fallback-model": true, "--debug-file": true, "--setting-sources": true,
	}

	// Separate user-provided flags from prompt words.
	// -p / --print (and the equals-form variants -p=... / --print=...) are
	// stripped — we never want claude in print/one-shot mode on this path; it
	// would exit after the first turn and break cross-step session.sendInput.
	// Keeping the interactive launch shape is also load-bearing for billing:
	// starting 2026-06-15 Anthropic routes `claude -p` / Agent SDK usage on
	// Pro/Max/Team subscriptions through a separate Agent SDK credit pool,
	// while plain interactive Claude Code keeps drawing from the normal
	// subscription allowance. See CLI_AGENT_INTEGRATION.md.
	var flags []string
	var promptParts []string
	skipNext := false
	for i, a := range args {
		if skipNext {
			skipNext = false
			flags = append(flags, a)
			continue
		}
		if a == "-p" || a == "--print" {
			continue
		}
		// Equals-form: strip the print/one-shot mode flag but keep the inline
		// prompt text (e.g. `claude --print=hello` → prompt "hello") so the
		// interactive launch still answers the caller's query instead of
		// hanging on empty stdin.
		if strings.HasPrefix(a, "-p=") {
			if v := strings.TrimPrefix(a, "-p="); v != "" {
				promptParts = append(promptParts, v)
			}
			continue
		}
		if strings.HasPrefix(a, "--print=") {
			if v := strings.TrimPrefix(a, "--print="); v != "" {
				promptParts = append(promptParts, v)
			}
			continue
		}
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			// If this flag expects a value and there's a next arg, consume it too
			if valuedFlags[a] && i+1 < len(args) {
				skipNext = true
			}
			continue
		}
		promptParts = append(promptParts, a)
	}

	result = append(result, flags...)

	return result, strings.Join(promptParts, " ")
}

// buildCodexInteractiveArgs builds Codex CLI args for JSONL streaming.
//
// Codex exec is one-shot (reads stdin to completion, runs the turn, exits),
// and its prompt can come in via either a positional argv or the `-`
// placeholder which redirects to stdin. We use the `-` form so multi-KB
// briefs don't hit the Windows CreateProcess ~32KB command-line cap
// (the failure that surfaced as `fork/exec ... codex.cmd: The filename or
// extension is too long.` when a 56KB review brief was passed as argv).
//
// Returns (cliArgs, stdinPrompt):
//   - cliArgs ends with "-" to signal codex to read the prompt from stdin.
//   - stdinPrompt is the joined positional prompt text. StartSession writes
//     it verbatim (plain text — codex does NOT parse NDJSON like claude),
//     then closes stdin so codex stops waiting for more.
//
// Empty stdinPrompt is allowed (codex will error if no prompt is provided,
// but that surfaces as a normal start error rather than a cmdline overflow).
//
// Subcommand carve-out: `codex exec` also supports `resume` / `review` / `help`
// subcommands (e.g. `codex exec resume --last "follow-up"`). For those we keep
// the entire user-supplied argv intact and skip stdin routing, because the
// positional grammar after a subcommand differs (resume has SESSION_ID before
// PROMPT) and rewriting it would corrupt the call. Top-level invocations — the
// originating Windows-cmdline-overflow case — still use the stdin path.
func buildCodexInteractiveArgs(args []string) ([]string, string) {
	cleanedArgs := sanitizeCodexExecArgs(args)

	// Codex options that consume the next argument as their value — without
	// this, e.g. `--model o3` would treat "o3" as a prompt word. Keep this
	// in sync with `codex exec --help` (and the resume/review subcommands).
	// The `--flag=value` form is one token and doesn't need entries here.
	valuedFlags := map[string]bool{
		// Top-level `codex exec` options
		"-c": true, "--config": true,
		"-m": true, "--model": true,
		"-i": true, "--image": true,
		"-p": true, "--profile": true,
		"-C": true, "--cd": true,
		"-o": true, "--output-last-message": true,
		"--enable": true, "--disable": true,
		"--local-provider": true,
		"--add-dir":        true,
		"--output-schema":  true,
		"--color":          true,
		// `codex exec review` adds these (harmless when passed through in the
		// subcommand path; listed for completeness so the value isn't
		// misclassified if a future change brings reviews into the parser).
		"--base":   true,
		"--commit": true,
		"--title":  true,
	}

	// Detect whether the user invoked an `exec` subcommand (resume/review/help)
	// by scanning until the first non-flag positional token. Anything past that
	// token belongs to the subcommand and must be forwarded verbatim.
	knownSubcommands := map[string]bool{
		"resume": true,
		"review": true,
		"help":   true,
	}
	hasSubcommand := false
	{
		skipNext := false
		for i, a := range cleanedArgs {
			if skipNext {
				skipNext = false
				continue
			}
			if strings.HasPrefix(a, "-") {
				if !strings.Contains(a, "=") && valuedFlags[a] && i+1 < len(cleanedArgs) {
					skipNext = true
				}
				continue
			}
			if knownSubcommands[strings.ToLower(a)] {
				hasSubcommand = true
			}
			break // first non-flag positional decides
		}
	}

	if hasSubcommand {
		// Subcommand path: keep argv shape intact, no stdin routing.
		// `resume` / `review` accept `-` as a stdin placeholder for PROMPT,
		// but we can't safely rewrite without also knowing whether a leading
		// positional is SESSION_ID vs prompt, so we pass through verbatim.
		// Multi-KB prompts in this path may still hit the Windows cmdline cap.
		result := make([]string, 0, len(cleanedArgs)+3)
		result = append(result, "exec")
		result = append(result, "--json")
		result = append(result, "--dangerously-bypass-approvals-and-sandbox")
		result = append(result, cleanedArgs...)
		return result, ""
	}

	// Top-level path: split flag args (with their values) from positional
	// prompt parts, then route the prompt through stdin via `-`.
	var flagArgs []string
	var promptParts []string
	skipNext := false
	for i, a := range cleanedArgs {
		if skipNext {
			skipNext = false
			flagArgs = append(flagArgs, a)
			continue
		}
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			if !strings.Contains(a, "=") && valuedFlags[a] && i+1 < len(cleanedArgs) {
				skipNext = true
			}
			continue
		}
		promptParts = append(promptParts, a)
	}

	result := make([]string, 0, len(flagArgs)+4)
	result = append(result, "exec")
	result = append(result, "--json")
	result = append(result, "--dangerously-bypass-approvals-and-sandbox")
	result = append(result, flagArgs...)
	// "-" tells codex to read the prompt from stdin. Always append, even if
	// promptParts is empty — keeps the args shape predictable and codex will
	// surface a useful error if it hits EOF on stdin with no content.
	result = append(result, "-")

	return result, strings.Join(promptParts, " ")
}

func sanitizeCodexExecArgs(args []string) []string {
	cleaned := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]
		lowerArg := strings.ToLower(arg)

		if i == 0 && lowerArg == "exec" {
			continue
		}

		switch {
		case lowerArg == "--json" ||
			lowerArg == "--full-auto" ||
			lowerArg == "--dangerously-bypass-approvals-and-sandbox":
			continue
		case lowerArg == "--sandbox" ||
			lowerArg == "-s" ||
			lowerArg == "--approval-policy" ||
			lowerArg == "--ask-for-approval" ||
			lowerArg == "-a":
			if i+1 < len(args) {
				i++
			}
			continue
		case strings.HasPrefix(lowerArg, "--sandbox=") ||
			strings.HasPrefix(lowerArg, "--approval-policy=") ||
			strings.HasPrefix(lowerArg, "--ask-for-approval="):
			continue
		default:
			cleaned = append(cleaned, arg)
		}
	}

	return cleaned
}

// buildAntigravityInteractiveArgs builds Antigravity CLI (`agy`) args for
// one-shot prompt execution.
//
// agy ≥ 1.1.x: `--print` / `-p` / `--prompt` takes the prompt as its FLAG
// VALUE (not a trailing positional). Verified against agy 1.1.2 and 1.1.11.
// The native-chat path (buildAntigravityNativeArgs) already uses this contract;
// the session_start / PTY one-shot path must match or tertiary Review kickoffs
// (and any other terminalWithFeatureDetails agy role) answer a question about
// `--dangerously-skip-permissions` and ignore the real brief:
//
//	WRONG: agy --print --dangerously-skip-permissions <brief>
//	       → --print's value is "--dangerously-skip-permissions"
//	RIGHT: agy --dangerously-skip-permissions --print <brief>
//
// agy ships claude-code-shaped flags but does NOT expose stream-json input, so
// we cannot drive it as a multi-turn streaming session like claude. We run a
// one-shot `--print` per session_start.
//
// WHY NOT STDIN (unlike codex): agy does NOT read a piped stdin —
// verified live against agy 1.0.4, piped input is ignored in BOTH interactive
// and --print modes (agy appears to require a real TTY for interactive input),
// so the prompt MUST be passed on argv. agy also resolves to a NATIVE `agy.exe`
// (not an npm cmd.exe/.ps1 shim), so it is launched directly via CreateProcess —
// the relevant ceiling is the ~32KB CreateProcess command-line cap, NOT an
// 8191-char cmd.exe cap. So agy does NOT have the "command line is too long."
// failure for normal (≤ ~32KB) briefs; only briefs approaching 32KB would risk
// it, which would need a TTY/ACP-style redesign rather than the stdin trick.
func buildAntigravityInteractiveArgs(args []string) []string {
	// Diagnostic invocations are not prompts: `agy --version` prints 1.1.11 and
	// is exactly how probeAntigravityNativeCapabilityUncached queries the CLI,
	// but shaping would turn it into `--print --version` and burn a model run.
	// Same for a lone `--help` / `-h` or a bare subcommand (`agy models`).
	if isAntigravityDiagnosticInvocation(args) {
		return args
	}
	if barePrint, invalidBool, diagnostic := scanAntigravityCallerArgs(args); barePrint || invalidBool || diagnostic {
		return args
	}
	result := make([]string, 0, len(args)+3)
	// Permission skip first — never immediately after bare --print.
	result = append(result, "--dangerously-skip-permissions")

	flags, prompt, trailing, hasPrompt := partitionAntigravityCallerArgs(args)
	result = append(result, flags...)
	// Distinguish "no prompt" from an *explicit* empty prompt (args [""],
	// --print=, or --print with no value). Omitting --print for the latter
	// drops agy into the interactive TUI and hangs the PTY until timeout.
	if hasPrompt {
		result = append(result, "--print", prompt)
	}
	result = append(result, trailing...)
	return result
}

// buildOpenCodeInteractiveArgs shapes a one-shot `opencode` invocation for the
// legacy session_start / PTY path.
//
// `run --format json` is ALWAYS forced and any caller token that would re-enter
// the interactive TUI — a bare `opencode`, a second `run`, or a `--format`
// override — is stripped. A bare `opencode` on a headless remote session starts
// the TUI, which emits escape-sequence noise and never exits: the identical
// trap grok_acp.go documents for bare `grok`.
//
// Diagnostic invocations (`--version`, `--help`, `models`, `auth …`) are
// returned verbatim: shaping them would turn an information request into a
// model run, and `opencode --version` is exactly how the capability probe and
// the usage parser query the CLI.
//
// The prompt stays a trailing POSITIONAL here (unlike the resident chat path in
// opencode_native.go, which writes it to a temp file consumed on stdin), so
// this path is subject to the Windows CreateProcess argv ceiling — see
// openCodeInteractiveMaxPromptBytes.
func buildOpenCodeInteractiveArgs(args []string) []string {
	if isOpenCodeDiagnosticInvocation(args) {
		return args
	}

	forwarded := make([]string, 0, len(args)+3)
	prompt := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		name := a
		inlineValue := false
		if idx := strings.Index(a, "="); idx > 0 {
			name = a[:idx]
			inlineValue = true
		}
		if openCodeStrippedFlags[name] {
			if !inlineValue && openCodeValuedStrippedFlags[name] {
				i++
			}
			continue
		}
		if a == "run" && len(forwarded) == 0 && len(prompt) == 0 {
			// Already forced below; a second `run` parses as prompt text.
			continue
		}
		if strings.HasPrefix(a, "-") {
			forwarded = append(forwarded, a)
			// Flags the manager forwards that consume the next token.
			if openCodeForwardedValuedFlags[name] && !inlineValue && i+1 < len(args) {
				i++
				forwarded = append(forwarded, args[i])
			}
			continue
		}
		prompt = append(prompt, a)
	}

	result := append([]string{"run", "--format", "json"}, forwarded...)
	return append(result, prompt...)
}

// openCodeForwardedValuedFlags are caller flags the manager passes through that
// take the NEXT argv token as their value. Without this the value would be
// mistaken for prompt text and reordered behind the flags.
var openCodeForwardedValuedFlags = map[string]bool{
	"--model": true,
	"-m":      true,
	"--agent": true,
	"--port":  true,
	"--host":  true,
}

// openCodeDiagnosticTokens are invocations that ask OpenCode for information
// instead of running a prompt. Reshaping any of these into `run` would burn a
// model call the caller never asked for.
var openCodeDiagnosticTokens = map[string]bool{
	"--version": true, "-version": true, "-v": true,
	"--help": true, "-help": true, "-h": true,
	"auth": true, "models": true, "upgrade": true, "serve": true,
	"github": true, "mcp": true, "agent": true, "stats": true,
}

// isOpenCodeDiagnosticInvocation reports an invocation OpenCode answers with
// information rather than a model run. Like agy, OpenCode pre-scans its whole
// command line for `--help` / `--version`, so those are matched wherever they
// appear; subcommands only count as the FIRST token.
func isOpenCodeDiagnosticInvocation(args []string) bool {
	for i, a := range args {
		lowered := strings.ToLower(strings.TrimSpace(a))
		if name, _, ok := strings.Cut(lowered, "="); ok {
			lowered = name
		}
		switch lowered {
		case "--version", "-version", "-v", "--help", "-help", "-h":
			return true
		}
		if i == 0 && openCodeDiagnosticTokens[lowered] && !strings.HasPrefix(lowered, "-") {
			return true
		}
	}
	return false
}

// argvByteLen totals the bytes an argv slice contributes to the command line,
// including the single separator each token needs. Used for the CreateProcess
// ceiling check, which is a limit on the assembled command line rather than on
// any one argument.
func argvByteLen(args []string) int {
	n := 0
	for _, a := range args {
		n += len(a) + 1
	}
	return n
}

// openCodeInteractiveMaxPromptBytes caps the prompt on the LEGACY positional
// path only. Measured in BYTES, not characters: Windows CreateProcess caps the
// whole command line near 32KB and a multibyte prompt passes a character-count
// check while exceeding the real limit. The resident chat path
// (opencode_native.go) is exempt — its prompt goes to a temp file consumed on
// stdin and never touches argv.
const openCodeInteractiveMaxPromptBytes = 24 * 1024

// antigravityDiagnosticTokens are single-token invocations that ask the CLI for
// information instead of running a prompt. Subcommands are from `agy --help` on
// 1.1.11; `--version` prints the version there.
//
// `-v` and the equals spellings are included so their *errors* survive rather
// than becoming prompts — on 1.1.11 `agy -v` reports "flag needs an argument:
// -v" (it is an int flag, not a version alias), `agy -v=true` reports an
// invalid value, and `agy --version=true` prints nothing at all. None of those
// should turn into a permission-skipping model run.
var antigravityDiagnosticTokens = map[string]bool{
	"--version": true, "-version": true, "-v": true,
	"--help": true, "-help": true, "-h": true,
	"agent": true, "agents": true, "changelog": true, "help": true,
	"install": true, "models": true, "plugin": true, "plugins": true,
	"update": true,
}

// antigravityGlobalDiagnosticTokens are handled wherever they appear in argv.
// agy pre-scans its whole command line for these, ahead of Go's flag parsing:
// on 1.1.12 `agy review --help` and even `agy explain the --help flag please`
// print the usage banner, and `agy review -version` prints the version, instead
// of running the prompt. Reshaping those into a --print value would launch a
// permission-skipping model run the caller would never have got.
//
// `-v` is deliberately absent: it is an ordinary int flag, so `agy review -v
// now` really does run the prompt.
var antigravityGlobalDiagnosticTokens = map[string]bool{
	"--version": true, "-version": true,
	"--help": true, "-help": true, "-h": true,
}

// isAntigravityDiagnosticInvocation reports an invocation agy answers with
// information instead of a model run.
//
// Two shapes: a global diagnostic flag anywhere in argv (see above), or a lone
// diagnostic token in the bare or `flag=value` spelling. The lone-token rule
// covers the bare subcommands and `-v`, which only act in the leading flag
// region — a brief can legitimately start with a word like "help" or "update".
func isAntigravityDiagnosticInvocation(args []string) bool {
	for _, a := range args {
		if antigravityGlobalDiagnosticTokens[a] {
			return true
		}
	}
	if len(args) != 1 {
		return false
	}
	if antigravityDiagnosticTokens[args[0]] {
		return true
	}
	name, _, ok := splitAntigravityEqualsFlag(args[0])
	return ok && antigravityDiagnosticTokens[name]
}

// antigravityValuedFlags are agy flags whose next argv token is their value.
// Only exact known flags are peeled off the front of caller args so a brief
// that happens to start with "-" (markdown hr, bullet) is still treated as
// prompt text.
var antigravityValuedFlags = map[string]bool{
	"--add-dir": true, "--agent": true, "--conversation": true,
	"--effort": true, "--json-schema": true, "--log-file": true,
	"--mode": true, "--model": true, "--output-format": true,
	"--print-timeout": true, "--project": true,
	"--prompt-interactive": true, "-i": true,
}

// antigravityBoolFlags are agy flags that take no value. Injected duplicates
// of --dangerously-skip-permissions are stripped; --continue/-c are stripped
// (cross-chat contamination risk — same rule as native chat).
var antigravityBoolFlags = map[string]bool{
	"--dangerously-skip-permissions": true,
	"--disable-slash-commands":       true,
	"--new-project":                  true,
	"--sandbox":                      true,
	"--continue":                     true,
	"-c":                             true,
}

// partitionAntigravityCallerArgs peels known leading agy flags off args and
// returns a single --print value. When the caller already used --print /
// --print=value, only that one value is the prompt — subsequent recognized
// flags (e.g. --conversation <id> from buildAntigravityNativeArgs) stay
// options, not prompt text. Without an explicit --print, the remainder is
// joined as the prompt. Unknown tokens (including anything that merely starts
// with "-") begin the prompt when no --print was seen.
//
// hasPrompt is true when the caller supplied a print value — including an
// *empty* one ("" / --print= / bare --print). It is false only when no prompt
// material was present at all (so the builder should omit --print).
func partitionAntigravityCallerArgs(args []string) (flags []string, prompt string, trailing []string, hasPrompt bool) {
	i := 0
	var printVal string
	gotPrint := false
	// dangling holds a recognized valued flag that arrived without its operand.
	// It must stay behind the prompt so it cannot consume --print.
	var dangling []string
	// postFlags keeps flags that followed the explicit print value in that same
	// position. Hoisting them ahead of the prompt would reverse shell expansion
	// order: `agy --print "${x:=review}" --add-dir "$x"` passes review to both,
	// but `--add-dir "$x" --print "${x:=review}"` passes an empty --add-dir
	// (verified in bash 5.2.21 with a stub agy). agy itself accepts options
	// after the print value -- that is the native resume shape.
	var postFlags []string
	addFlag := func(tokens ...string) {
		if gotPrint {
			postFlags = append(postFlags, tokens...)
			return
		}
		flags = append(flags, tokens...)
	}
	for i < len(args) {
		// Classify on the canonical spelling (Go accepts -flag and --flag alike)
		// but keep the caller's own token when re-emitting it.
		raw := args[i]
		a := canonicalAntigravityFlag(raw)
		// `--` ends flag parsing and is itself dropped, so the implicit prompt
		// starts after it. After an explicit print value it must stay on the
		// command line: dropping it would expose the remaining positionals to
		// flag parsing again, turning `agy --print review -- --model gemini`
		// into a real --model selection.
		if a == antigravityFlagTerminator {
			if !gotPrint {
				i++
			}
			break
		}
		// Match only exact lowercase CLI spellings (agy flag names are
		// case-sensitive). Uppercase forms like --PRINT are prompt text.

		// Equals-form: --flag=value / -p=value
		if name, val, ok := splitAntigravityEqualsFlag(a); ok {
			name = canonicalAntigravityFlag(name)
			if isAntigravityPrintFlag(name) {
				// Explicit print value (possibly empty). Keep peeling flags
				// after it so `--print=hi --conversation id` preserves options.
				printVal = val
				gotPrint = true
				i++
				continue
			}
			// `-c` is agy's documented short alias for --continue, and Go's
			// flag package accepts `-c=true` for booleans, so the equals form
			// must be stripped here too or the cross-chat-contamination guard
			// is bypassed.
			if name == "--dangerously-skip-permissions" || name == "--continue" || name == "-c" {
				i++
				continue
			}
			if antigravityValuedFlags[name] || antigravityBoolFlags[name] {
				addFlag(a)
				i++
				continue
			}
			// Unknown --foo=bar → prompt starts here (only if no --print yet).
			break
		}

		if isAntigravityPrintFlag(a) {
			// --print / -p / --prompt takes exactly one value token. Bare
			// --print with no following token is still an explicit empty print.
			i++
			gotPrint = true
			if i < len(args) {
				printVal = args[i]
				i++
			} else {
				printVal = ""
			}
			continue
		}

		if antigravityBoolFlags[a] {
			// Strip injected / unsafe flags; keep other booleans.
			if a == "--dangerously-skip-permissions" || a == "--continue" || a == "-c" {
				i++
				continue
			}
			addFlag(raw)
			i++
			continue
		}

		if antigravityValuedFlags[a] {
			if i+1 >= len(args) {
				// Recognized valued flag with no operand (always the last
				// token). Hoisting it into flags would emit
				// `--conversation --print review`, where agy takes `--print` as
				// the conversation value and the one-shot silently disappears.
				// Keep it last so --print <prompt> stays intact.
				dangling = append(dangling, raw)
				i++
				continue
			}
			addFlag(raw, args[i+1])
			i += 2
			continue
		}

		// First non-flag token → prompt starts here (may begin with "-" or be "").
		break
	}
	if gotPrint {
		// Anything not recognized after an explicit print value must remain on
		// argv so agy can preserve its own positional/unknown-option behavior.
		trailing := append([]string{}, postFlags...)
		trailing = append(trailing, args[i:]...)
		return flags, printVal, append(trailing, dangling...), true
	}
	if i < len(args) {
		return flags, strings.Join(args[i:], " "), dangling, true
	}
	return flags, "", dangling, false
}

// scanAntigravityCallerArgs walks the caller's tokens the same way the
// partitioner does — consuming each recognized flag's operand and stopping at
// the first token that starts the prompt — and reports two invocations that
// must reach agy untouched instead of being reshaped.
//
// barePrint: a `--print` / `-p` / `--prompt` that runs out of argv before its
// operand. agy's string flag then has no value and the CLI exits with `flag
// needs an argument: -print` (1.1.11), so inventing an empty prompt would turn
// a caller error into a permission-skipping model run. Passing the argv through
// preserves that error and — unlike omitting --print — cannot drop agy into the
// TUI. Note `agy --print --print` is NOT bare: the second token is the first
// flag's value, a valid one-shot with the prompt "--print".
//
// invalidBool: an equals-form spelling of a flag the safety guard strips whose
// value is not a Go boolean. `agy --continue=maybe review` exits with `invalid
// boolean value "maybe" for -continue`, so stripping the token would run a
// prompt the caller never got. Flags we forward rather than strip need no check
// — they reach agy and produce their own error.
//
// Both stop at the prompt boundary: in `agy review --continue=maybe` the flag
// text is prompt material (Go's flag parsing already stopped at `review`), so
// it must not block the reshape.
func scanAntigravityCallerArgs(args []string) (barePrint, invalidBool, diagnostic bool) {
	stripped := func(name string) bool {
		return name == "--dangerously-skip-permissions" || name == "--continue" || name == "-c"
	}
	for i := 0; i < len(args); i++ {
		a := canonicalAntigravityFlag(args[i])
		if a == antigravityFlagTerminator {
			return barePrint, invalidBool, diagnostic // `--` ends flag parsing
		}
		if name, val, ok := splitAntigravityEqualsFlag(a); ok {
			name = canonicalAntigravityFlag(name)
			// An equals-form probe in the flag region is answered (or rejected)
			// by agy rather than run: `agy --model gemini -v=true` reports an
			// invalid value for the int flag `-v`, so folding it into a --print
			// value would start a model run the caller never got.
			if antigravityDiagnosticTokens[name] {
				diagnostic = true
				return barePrint, invalidBool, diagnostic
			}
			// `--print=` carries a value (possibly empty).
			if isAntigravityPrintFlag(name) {
				continue
			}
			if antigravityValuedFlags[name] || antigravityBoolFlags[name] {
				if stripped(name) {
					if _, err := strconv.ParseBool(val); err != nil {
						invalidBool = true
					}
				}
				continue
			}
			return barePrint, invalidBool, diagnostic // unknown token: the prompt starts here
		}
		if isAntigravityPrintFlag(a) {
			if i+1 >= len(args) {
				barePrint = true
				return barePrint, invalidBool, diagnostic
			}
			i++ // the next token is this flag's value, whatever it looks like
			continue
		}
		if antigravityBoolFlags[a] {
			continue
		}
		if antigravityValuedFlags[a] {
			if i+1 >= len(args) {
				return barePrint, invalidBool, diagnostic // dangling flag: handled by the partitioner
			}
			i++
			continue
		}
		// A flag-shaped diagnostic token in the leading flag region is answered
		// or rejected by agy rather than run: `agy --model gemini -v` reports
		// `flag needs an argument: -v` on 1.1.12, so folding it into a --print
		// value would start a model run the caller never got. (Bare words such
		// as `models` are not included here — past the first flag they are
		// ordinary prompt text.)
		if strings.HasPrefix(a, "-") && antigravityDiagnosticTokens[a] {
			diagnostic = true
			return barePrint, invalidBool, diagnostic
		}
		return barePrint, invalidBool, diagnostic // first non-flag token: the prompt starts here
	}
	return barePrint, invalidBool, diagnostic
}

// canonicalAntigravityFlag normalizes a single-dash long flag to its double-dash
// spelling. Go's flag package treats `-flag` and `--flag` as the same option —
// verified on agy 1.1.12, where `agy -model` reports `flag needs an argument:
// -model` — so classification must accept both. Short flags (-p, -c, -h) and
// non-flag tokens are returned unchanged, and callers keep the caller's own
// spelling when re-emitting the token.
func canonicalAntigravityFlag(tok string) string {
	if len(tok) < 3 || !strings.HasPrefix(tok, "-") || strings.HasPrefix(tok, "--") {
		return tok
	}
	double := "-" + tok
	if antigravityValuedFlags[double] || antigravityBoolFlags[double] || isAntigravityPrintFlag(double) {
		return double
	}
	return tok
}

// antigravityFlagTerminator is Go's `--`: it ends flag parsing and is dropped
// from the argument list, so `agy -- review` has the prompt `review`, not
// `-- review`.
const antigravityFlagTerminator = "--"

func isAntigravityPrintFlag(name string) bool {
	return name == "--print" || name == "-p" || name == "--prompt"
}

// splitAntigravityEqualsFlag returns (flagName, value, true) for --flag=value
// forms. The flag name is matched case-sensitively against known CLI spellings;
// the value keeps the original token's casing.
func splitAntigravityEqualsFlag(raw string) (name, val string, ok bool) {
	eq := strings.IndexByte(raw, '=')
	if eq <= 0 {
		return "", "", false
	}
	name = raw[:eq]
	if !strings.HasPrefix(name, "-") {
		return "", "", false
	}
	return name, raw[eq+1:], true
}

// grokKnownSubcommands are the `grok <cmd>` subcommands whose argv grammar must
// be forwarded verbatim — running them through the headless prompt builder
// (which injects `-p`) would corrupt the call. Mirrors the codex resume/review
// carve-out. A bare `grok "<prompt>"` (no subcommand) is the prompt path.
var grokKnownSubcommands = map[string]bool{
	"agent": true, "completions": true, "dashboard": true, "export": true,
	"help": true, "import": true, "inspect": true, "leader": true,
	"login": true, "logout": true, "mcp": true, "memory": true,
	"models": true, "plugin": true, "sessions": true, "setup": true,
	"trace": true, "update": true, "version": true, "worktree": true,
}

// grokSubcommandActions are second-positional words that, when paired with a
// known leading subcommand, indicate a documented two-word subcommand grammar
// rather than a prose prompt. Without this gate, the pre-scan would treat any
// two-word input whose first word matches a Grok subcommand as a verbatim
// argv call — including prose prompts like `grok help me`, `grok sessions
// stuck`, or `grok models broken` whose second word is plainly not a CLI
// verb. Those would short-circuit to raw argv, Grok would reject the unknown
// second token (`unrecognized subcommand 'me'`), and we'd reintroduce the
// tokenisation failure the headless `-p` path exists to fix. Restricting the
// 2-positional carve-out to recognised verbs (`list`, `install`, `add`, …)
// keeps documented grammars (`sessions list`, `mcp install`, `models list`,
// `agent stdio`) verbatim while letting prose fall through to the managed
// `-p` builder.
var grokSubcommandActions = map[string]bool{
	"list": true, "ls": true,
	"add": true, "remove": true, "rm": true, "delete": true, "del": true,
	"install": true, "uninstall": true,
	"update": true, "upgrade": true,
	"show": true, "get": true, "info": true, "inspect": true,
	"set": true, "unset": true,
	"enable": true, "disable": true,
	"start": true, "stop": true, "restart": true, "status": true,
	"run": true, "exec": true, "stdio": true,
	"create": true, "new": true, "init": true,
	"login": true, "logout": true,
	"clear": true, "reset": true, "purge": true,
	"import": true, "export": true,
	"sync": true, "switch": true, "use": true,
}

// rewriteGrokPromptToFile relocates grok's argv prompt from the inline
// `-p <prompt>` pair to `--prompt-file <tempPath>`, returning the rewritten
// args and the path of the temp file the caller must remove after the process
// exits (empty when no rewrite happened).
//
// WHY: buildGrokInteractiveArgs always appends the prompt on argv as `-p
// <prompt>` (the LAST two tokens). claude/codex sidestep the OS
// command-line-length cap by piping long prompts through stdin, but grok's
// headless mode does NOT read piped stdin — so a long review brief overflows
// the Windows command line ("command line too long" / "powershell path too
// long"). grok accepts `--prompt-file <path>` as a validated drop-in for `-p
// <prompt>`, so a temp file is the file-based analog of the others' stdin route.
//
// grokPromptTempDir returns the directory the grok prompt temp file is written
// into: `<home>/.ai-expedite/grok-prompts/`. This co-locates it under the same
// AI Expedite scratch root the documentRequirements / documentDesign agents use
// (`~/.ai-expedite/requirements/<featureID>/...`) — a stable, non-repo location
// (so a repo `git stash` during sync can't wipe it) outside the OS temp dir.
// Returns "" (→ CreateTemp uses the OS temp dir) when the home can't be resolved
// or the directory can't be created, so the rewrite is always best-effort.
func grokPromptTempDir() string {
	return cliPromptTempDir("grok-prompts")
}

// Best-effort: on any temp-file write error, or when the argv was NOT produced
// by buildGrokInteractiveArgs' managed `-p` path (subcommand carve-outs return
// user args verbatim — see the carve-out `return args` at the top of
// buildGrokInteractiveArgs), the original args are returned untouched and
// cleanupPath is "" so the launch falls back to `-p` rather than failing.
//
// The managed-path gate matters: buildGrokInteractiveArgs hands back user argv
// verbatim for the documented `grok mcp add <name> -- <cmd> [args...]` grammar
// (and the other subcommand carve-outs), so a legitimate child command ending
// in its OWN `-p <value>` — e.g. `grok mcp add svc -- python server.py -p 8000`
// — would otherwise have its trailing `-p 8000` rewritten to `--prompt-file
// <temp>` and the registered MCP server command would be corrupted. The
// managed path always emits the streaming-json + no-auto-update sentinels at
// the head of argv, so anchor the rewrite to that signature.
func rewriteGrokPromptToFile(cliArgs []string) (newArgs []string, cleanupPath string) {
	// Managed-path gate: only the buildGrokInteractiveArgs managed `-p` path
	// emits `--output-format streaming-json --no-auto-update` as the first
	// three tokens (see the unconditional prepend in that function). The
	// subcommand carve-out returns user args verbatim and does NOT carry these
	// sentinels, so a verbatim argv ending in `-p <value>` (e.g. a child
	// command's own flag in `grok mcp add svc -- python server.py -p 8000`)
	// falls through unchanged.
	if len(cliArgs) < 3 ||
		cliArgs[0] != "--output-format" ||
		cliArgs[1] != "streaming-json" ||
		cliArgs[2] != "--no-auto-update" {
		return cliArgs, ""
	}
	// buildGrokInteractiveArgs always emits the separate-value form with the
	// prompt as the final token, i.e. cliArgs[n-2] == "-p", cliArgs[n-1] == value.
	n := len(cliArgs)
	if n < 2 || cliArgs[n-2] != "-p" {
		return cliArgs, ""
	}
	prompt := cliArgs[n-1]

	// Write under the AI Expedite scratch root (`~/.ai-expedite/grok-prompts/`)
	// — the same parent the documentRequirements / documentDesign agents use for
	// their feature scratch files — rather than the OS temp dir. grokPromptTempDir
	// returns "" on any failure, in which case CreateTemp falls back to the OS
	// temp dir (still works; just not co-located with the other scratch files).
	f, err := os.CreateTemp(grokPromptTempDir(), "grok-prompt-*.txt")
	if err != nil {
		return cliArgs, ""
	}
	tempPath := f.Name()
	// The file holds only the prompt; lock it down to the owner (0600). On
	// Windows the perm bits are advisory, but we keep them consistent with the
	// Unix builds and other per-session temp resources.
	if chmodErr := f.Chmod(0o600); chmodErr != nil {
		// Non-fatal — proceed with the default perms.
		_ = chmodErr
	}
	if _, writeErr := f.WriteString(prompt); writeErr != nil {
		f.Close()
		_ = os.Remove(tempPath)
		return cliArgs, ""
	}
	if closeErr := f.Close(); closeErr != nil {
		_ = os.Remove(tempPath)
		return cliArgs, ""
	}

	// Replace the trailing `-p <value>` pair with `--prompt-file <tempPath>`.
	rewritten := make([]string, 0, n)
	rewritten = append(rewritten, cliArgs[:n-2]...)
	rewritten = append(rewritten, "--prompt-file", tempPath)
	return rewritten, tempPath
}

// buildGrokInteractiveArgs builds Grok Build CLI (`grok`) args for a one-shot
// headless turn streamed as JSON.
//
// WHY HEADLESS (-p) + streaming-json: a bare `grok <prompt>` launches Grok's
// interactive TUI, which never exits in our non-TTY session — the process
// hangs until the 6h cap (observed as a terminal card stuck on "Running"). And
// an UNQUOTED multi-word prompt is tokenised so Grok parses the second word as
// a subcommand (`error: unrecognized subcommand 'a'`, exit 1). Forcing
// `-p/--single` runs the prompt once and exits; `--output-format
// streaming-json` gives the stream parser the same per-event shape
// (thought / text / end) it reads for the other agents, with `end` as the turn
// terminal (see detectCLITerminalEvent).
//
// WHY ARGV (no stdin): grok resolves to a native `grok.exe` (~/.grok/bin),
// launched directly via CreateProcess, so the prompt rides on argv as the value
// of `-p` (the ~32KB CreateProcess cap applies, like agy — not a cmd.exe shim's
// 8191-char cap). The headless `-p` process exits on its own after one turn, so
// no stdin routing is needed.
//
// Caller-supplied prompt-delivery / output-format flags are stripped so they
// can't collide with the managed contract: `-p`/`--single` (we always inject
// one; an inline value is folded into the prompt), `--output-format`,
// `--prompt-file`, `--prompt-json`. Other flags (`--model`, `--effort`,
// `--max-turns`, …) pass through.
//
// Returns cliArgs only — there is no stdin prompt (stdinPromptFormat returns ""
// for grok), the prompt is the value of `-p`.
func buildGrokInteractiveArgs(args []string, enableGrokAlwaysApprove bool) []string {
	// Grok flags that consume the NEXT token as their value — without this,
	// e.g. `--model grok-4` would treat "grok-4" as a prompt word. The
	// `--flag=value` form is one token and needs no entry here.
	valuedFlags := map[string]bool{
		"-m": true, "--model": true,
		"--effort": true, "--reasoning-effort": true,
		"--max-turns": true, "--agent": true, "--agents": true,
		"--cwd": true, "--permission-mode": true, "--sandbox": true,
		"--compaction-mode": true, "--compaction-detail": true,
		"--rules": true, "--system-prompt-override": true,
		"--leader-socket": true, "--debug-file": true,
		// Plugin discovery: xAI's plugin docs list `--plugin-dir <PATH>` as a
		// separate-value flag. Without this entry, `grok --plugin-dir /tmp/p
		// fix bug` would land "/tmp/p" in promptParts and the bare `--plugin-dir`
		// would slot in immediately before the appended managed `-p` — Grok
		// would then consume `-p` as the plugin directory value, dropping the
		// managed prompt delivery.
		"--plugin-dir": true,
		// Per-process config override: xAI's enterprise-deployment docs spell
		// out `-c|--config <key>=value` as the config-override surface (the
		// repo's Grok ACP builder also emits separate-value `--config <key>=`
		// pairs). Without this entry, `grok --config log.level=debug fix bug`
		// would land "log.level=debug" in promptParts and the bare `--config`
		// would slot in immediately before the appended managed `-p` — Grok
		// would then consume `-p` as the config value, dropping the managed
		// prompt delivery. We intentionally pin only the long form here: `-c`
		// is documented as ambiguous in some Grok CLI contexts (continue vs.
		// config), so we leave the short form alone rather than risk swapping
		// a `--continue` short-form value into the prompt path.
		"--config": true,
		// Permission-policy rule flags: xAI's enterprise headless docs document
		// `--allow <pattern>` / `--deny <pattern>` as per-tool policy rules
		// (e.g. `--allow "Bash(git *)" --deny "Bash(rm -rf *)"`). Without these
		// entries the loop would treat the rule value as a prompt word and the
		// bare flag would slot in immediately before the appended `-p` — Grok
		// would then consume `-p` as the rule value, dropping the managed
		// prompt and/or the intended allow/deny rule.
		"--allow": true, "--deny": true,
		// Session continuation: xAI's docs list -s/--session-id and -r/--resume
		// as value-taking common flags. Without them, e.g. `grok --resume abc
		// continue work` would land "abc" in promptParts, then the appended
		// `-p` would have "--resume" as its preceding token — Grok would see
		// `--resume -p` and consume the `-p` flag as the resume ID, breaking
		// resumed/named headless sessions.
		"-s": true, "--session-id": true,
		"-r": true, "--resume": true,
		// Prompt-delivery flags also take the next token as a value (the
		// prompt). The main loop below handles `-p`/`--single` via its own
		// captureNextAsPrompt branch BEFORE the generic valuedFlags check, so
		// adding them here is a no-op for the main loop — but the subcommand
		// pre-scan below shares this same map, and without these entries
		// `grok -p help me fix tests` would treat the prompt's first word
		// "help" as a subcommand and return the raw argv, reintroducing the
		// tokenisation failure this builder exists to fix.
		"-p": true, "--single": true,
	}

	// Subcommand carve-out: keep `grok models`, `grok sessions list`, etc.
	// intact — those are not prompt invocations. Skip valuedFlag values during
	// the scan so e.g. `grok --cwd sessions fix bug` keeps "sessions" as the
	// `--cwd` value (per xAI's headless flag docs) and still routes through the
	// managed `-p` builder, instead of treating "sessions" as a subcommand and
	// returning early.
	//
	// Carve out only when the shape is unambiguously a subcommand invocation,
	// gated on the positionals BEFORE any POSIX `--` end-of-options separator:
	//   (a) exactly ONE positional before `--` (or before end-of-args) that
	//       matches a known subcommand (`grok models`, `grok sessions`,
	//       `grok login`, `grok mcp -- foo`). A single subcommand token can
	//       only be a subcommand call.
	//   (b) two-plus positionals before `--` (or before end-of-args, capped
	//       at two for the no-`--` case) where the first is a known
	//       subcommand AND the second is a recognised action verb
	//       (`grok sessions list`, `grok mcp install`, `grok mcp add <name>
	//       -- <cmd>`). The action-verb gate is what makes the multi-
	//       positional case unambiguous — without it, prose like `grok help
	//       me` or `grok help me -- explain this` (where "help" matches a
	//       subcommand name but "me" is not a verb) would land on raw argv
	//       and Grok would reject the unknown second token, reintroducing
	//       the tokenisation failure this builder exists to fix.
	// Anything else — including a `--` that follows prose positionals — is
	// treated as a prose prompt and folded into managed `-p` delivery.
	{
		skipNext := false
		positionalCount := 0
		positionalsBeforeDoubleDash := 0
		hasDoubleDash := false
		var firstPositional, secondPositional string
		for i, a := range args {
			if skipNext {
				skipNext = false
				continue
			}
			if a == "--" {
				if !hasDoubleDash {
					positionalsBeforeDoubleDash = positionalCount
					hasDoubleDash = true
				}
				continue
			}
			if strings.HasPrefix(a, "-") {
				if !strings.Contains(a, "=") && valuedFlags[a] && i+1 < len(args) {
					skipNext = true
				}
				continue
			}
			switch positionalCount {
			case 0:
				firstPositional = strings.ToLower(a)
			case 1:
				secondPositional = strings.ToLower(a)
			}
			positionalCount++
		}
		// Effective positional count for the carve-out gate: positionals
		// BEFORE the `--`. When no `--` is present, use the total count.
		effectivePositionals := positionalCount
		if hasDoubleDash {
			effectivePositionals = positionalsBeforeDoubleDash
		}
		if firstPositional != "" && grokKnownSubcommands[firstPositional] {
			if effectivePositionals == 1 ||
				(effectivePositionals >= 2 && grokSubcommandActions[secondPositional]) {
				return args
			}
		}
	}

	var flagArgs []string
	var promptParts []string
	skipNext := false            // next token is a passthrough flag's value
	dropNext := false            // next token is a stripped flag's value — drop it
	captureNextAsPrompt := false // next token is a stripped -p/--single value — keep as prompt
	for i, a := range args {
		switch {
		case dropNext:
			dropNext = false
			continue
		case skipNext:
			skipNext = false
			flagArgs = append(flagArgs, a)
			continue
		case captureNextAsPrompt:
			captureNextAsPrompt = false
			promptParts = append(promptParts, a)
			continue
		}

		// Prompt-delivery flags: strip (we always inject our own -p). Fold an
		// inline / following value into the prompt so the turn still runs.
		if a == "-p" || a == "--single" {
			captureNextAsPrompt = true
			continue
		}
		if v, ok := strings.CutPrefix(a, "-p="); ok {
			if v != "" {
				promptParts = append(promptParts, v)
			}
			continue
		}
		if v, ok := strings.CutPrefix(a, "--single="); ok {
			if v != "" {
				promptParts = append(promptParts, v)
			}
			continue
		}
		// Output-format / alternate prompt sources: strip flag AND its value —
		// they collide with the managed streaming-json + `-p` contract.
		if a == "--output-format" || a == "--prompt-file" || a == "--prompt-json" {
			if i+1 < len(args) {
				dropNext = true
			}
			continue
		}
		if strings.HasPrefix(a, "--output-format=") ||
			strings.HasPrefix(a, "--prompt-file=") ||
			strings.HasPrefix(a, "--prompt-json=") {
			continue
		}
		// Auto-update toggles: strip both forms so the unconditional injection
		// below owns the policy. The xAI headless/scripting docs recommend
		// `--no-auto-update` for automated children because the background
		// update worker can race protocol output — in this streaming-json path,
		// an update notice on stdout/stderr would be read by readOutputStream
		// as session output and pollute the user's response stream. Mirrors
		// the unconditional injection + caller-supplied dedupe in
		// buildGrokACPArgs / sanitizeGrokACPExtraArgs (grok_acp.go).
		if a == "--no-auto-update" || a == "--auto-update" ||
			strings.HasPrefix(a, "--no-auto-update=") ||
			strings.HasPrefix(a, "--auto-update=") {
			continue
		}
		// Approval-bypass equals-forms: strip wholesale so a caller-supplied
		// `--always-approve=false` / `--auto-approve=false` cannot disable the
		// bypass while still slipping through to Grok in flagArgs (the dedupe
		// check below would also see the equals-form and suppress the
		// injection, so Grok would run with `=false` and the headless `-p`
		// turn would stall on the first tool/file-edit prompt — StartSession
		// closes Grok's stdin and detectPromptFromJSON has no Grok approval
		// branch). Dropping every equals-form here lets the bare form remain
		// the ONLY caller-supplied signal that genuinely opts into the bypass
		// (and dedupes the injection); equivalent to grok_acp.go's
		// sanitizeGrokInteractiveExtraArgs dropping `--always-approve=*`
		// wholesale.
		lowerEq := strings.ToLower(a)
		if strings.HasPrefix(lowerEq, "--always-approve=") ||
			strings.HasPrefix(lowerEq, "--auto-approve=") {
			continue
		}
		// Gate-off strip: when the workspace has NOT opted into
		// Config.EnableGrokAlwaysApprove, drop any caller-supplied bare
		// `--always-approve` / `--auto-approve` from flagArgs too. Otherwise a
		// signed `session_start` could ferry the approval bypass in via argv
		// and silently skip Grok's permission prompts in the default (opt-out)
		// configuration — exactly what the ACP path's stripGrokAlwaysApprove
		// flow refuses to allow.
		if !enableGrokAlwaysApprove &&
			(lowerEq == "--always-approve" || lowerEq == "--auto-approve") {
			continue
		}
		// Gate-off strip for the OTHER permission-bypass surfaces xAI
		// documents — `--permission-mode bypassPermissions`, `--allow
		// <pattern>`, and `--config approval.*=bypass`. Equals-form is
		// dropped here; the separate-value pairs flow into flagArgs and are
		// stripped by the trailing sweeps below (mirrors the ACP path's
		// sanitizeGrokACPExtraArgs speculative-admit / trailing-sweep
		// pattern). Without this, a signed `session_start` could ferry the
		// bypass in via `--permission-mode=bypassPermissions` /
		// `--allow="Bash(*)"` / `--config=approval.permission_mode=bypass`
		// and silently skip Grok's per-tool prompts in the default opt-out
		// configuration even though the ACP path refuses to allow it.
		if !enableGrokAlwaysApprove {
			if strings.HasPrefix(lowerEq, "--permission-mode=") ||
				strings.HasPrefix(lowerEq, "--permission_mode=") {
				if eq := strings.IndexByte(a, '='); eq >= 0 &&
					isGrokPermissionModeBypassValue(a[eq+1:]) {
					continue
				}
			}
			if strings.HasPrefix(lowerEq, "--allow=") {
				continue
			}
			if strings.HasPrefix(lowerEq, "--config=") {
				if eq := strings.IndexByte(a, '='); eq >= 0 &&
					isGrokApprovalConfigKV(a[eq+1:]) {
					continue
				}
			}
		}
		// Standalone `--` in the prose-prompt path: fold into the prompt
		// rather than passing it through to Grok. The subcommand pre-scan
		// above already carved out the documented `grok <subcmd> ... --
		// <args...>` grammars (xAI changelog: `grok mcp add <name> -- <cmd>`)
		// via a raw-argv return, so reaching this point means we're building
		// a managed `-p` headless turn. Letting `--` survive into flagArgs
		// would emit `... -- -p "<prompt>"` to Grok, and the POSIX
		// end-of-options separator would make Grok treat `-p` as a
		// positional rather than the prompt-delivery flag — the injected
		// `-p` would be dropped and the turn would fall back to the
		// interactive TUI this builder exists to avoid (the
		// `grok explain git checkout -- file` case). Folding `--` into
		// promptParts preserves the user's prose intent.
		if a == "--" {
			promptParts = append(promptParts, a)
			continue
		}

		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			if !strings.Contains(a, "=") && valuedFlags[a] && i+1 < len(args) {
				skipNext = true
			}
			continue
		}
		promptParts = append(promptParts, a)
	}

	// Gate-off trailing sweeps: drop `--permission-mode <bypass>`, `--allow
	// <pattern>`, and `--config <approval-kv>` separate-value pairs that the
	// main loop admitted speculatively via valuedFlags. Mirrors
	// sanitizeGrokACPExtraArgs — equals-forms were dropped above, these
	// sweeps finish off the separate-value pairs. Without them, a signed
	// session_start could bypass the workspace's per-tool approval gate via
	// `--permission-mode bypassPermissions` / `--allow "Bash(*)"` /
	// `--config approval.permission_mode=bypass` even though the ACP path
	// refuses the same surfaces under the same gate.
	if !enableGrokAlwaysApprove {
		flagArgs = stripGrokPermissionModePairs(flagArgs)
		flagArgs = stripGrokAllowRulePairs(flagArgs)
		flagArgs = stripGrokApprovalConfigPairs(flagArgs)
	}

	// Autonomous-tool-execution flag for the managed headless turn. Grok's
	// documented default permission mode is `ask`, which prompts on every tool
	// invocation / file edit. StartSession closes Grok's stdin after launch
	// (shouldCloseStdinAfterStart returns true via the default branch when
	// stdinPrompt is empty — grok's prompt rides on argv, not stdin) and
	// detectPromptFromJSON has no Grok approval branch, so an approval prompt
	// from inside the headless `-p` turn cannot be answered — the session
	// would stall or fail despite the TUI-hang fix. xAI documents
	// `--always-approve` as the canonical flag (`--auto-approve` is the legacy
	// synonym — see isGrokAlwaysApproveArg in grok_acp.go).
	//
	// GATED behind Config.EnableGrokAlwaysApprove — the same per-workspace
	// opt-in the Grok ACP path uses to strip equivalent caller-supplied bypass
	// flags. Without this gate, an approved/signed `session_start` targeting
	// `grok` would silently skip Grok's permission prompts in the default
	// (opt-out) configuration even though the ACP path refuses to. When the
	// gate is off the headless `-p` turn may stall on the first tool/file-edit
	// prompt — that is the intentional conservative posture; flip the gate to
	// accept the autonomous-execution tradeoff.
	//
	// Skipped only when the caller already supplied an equivalent flag — a
	// duplicate boolean flag is harmless but the cleaner argv aids debugging.
	// Equals-form spellings (`--always-approve=true`, `--always-approve=false`,
	// and the `--auto-approve=…` synonyms) were stripped wholesale in the
	// flag-folding loop above, so only the bare form can reach this dedupe
	// check — this prevents a caller-supplied `=false` from suppressing the
	// injection while leaving the disabled flag in flagArgs (which would
	// stall the headless `-p` turn on the first tool/file-edit prompt).
	injectAlwaysApprove := enableGrokAlwaysApprove
	if injectAlwaysApprove {
		for _, a := range args {
			lower := strings.ToLower(a)
			if lower == "--always-approve" || lower == "--auto-approve" {
				injectAlwaysApprove = false
				break
			}
		}
	}

	result := make([]string, 0, len(flagArgs)+6)
	result = append(result, "--output-format", "streaming-json", "--no-auto-update")
	if injectAlwaysApprove {
		result = append(result, "--always-approve")
	}
	result = append(result, flagArgs...)
	// Always append `-p <prompt>`, even when empty — grok surfaces a useful
	// "no prompt" error rather than hanging on the interactive TUI.
	result = append(result, "-p", strings.Join(promptParts, " "))
	return result
}

/* --------------------------------------------------------------------------
   Executable resolution
   -------------------------------------------------------------------------- */

// resolveExecutable resolves the full path to a CLI executable.
func resolveExecutable(command string) string {
	// If the caller supplied an explicit path (absolute or relative), honor
	// it instead of falling back to the cached PATH-resolved `claude`. This
	// matters for test shims and side-by-side installs like
	// `/opt/claude-nightly/claude` — the basename still routes through
	// isClaudeCommand for argv shaping and the billing-var strip, but the
	// binary that gets exec'd is the one the caller actually pointed at.
	if isExplicitPath(command) {
		// Relative explicit paths (e.g. `./claude`, `bin/claude`) must be
		// resolved against the session's cwd, not the agent's. exec.LookPath
		// resolves against the agent's current directory, so calling it here
		// would silently launch the agent-cwd binary and ignore the caller's
		// requested workspace. Pass the relative path through unchanged —
		// exec.Command + proc.Dir then resolves it inside the child's cwd
		// at exec time, matching the pre-PR behaviour for relative paths.
		if isRelativeExplicitPath(command) {
			return command
		}
		if path, err := exec.LookPath(command); err == nil {
			return path
		}
		return command
	}

	if isClaudeCommand(command) {
		return cachedResolveClaudePath()
	}

	// For codex, try PATH lookup
	if path, err := exec.LookPath(command); err == nil {
		return path
	}
	if path, err := exec.LookPath(command + ".exe"); err == nil {
		return path
	}

	// Grok's installer drops the binary in ~/.grok/bin and only touches shell
	// rc, which macOS GUI/launchd agent processes never source. Mirror the ACP
	// manager's fallback (grok_acp.go) so catalog-name `grok` sessions launched
	// from StartSession can still find the official install on a PATH miss.
	if isGrokCommand(command) {
		if p := resolveGrokInstallerBinary(); p != "" {
			return p
		}
	}

	return command
}

// isExplicitPath reports whether command was supplied as an explicit
// filesystem path (contains a separator) rather than a bare name to be
// resolved against PATH.
func isExplicitPath(command string) bool {
	return strings.ContainsAny(command, `/\`)
}

// isRelativeExplicitPath reports whether command is an explicit path that
// must be resolved against the child's working directory rather than the
// agent's. Used so resolveExecutable can skip exec.LookPath (which would
// resolve against the agent's cwd) and let exec.Command + proc.Dir do the
// resolution at child-exec time. Recognises Windows drive-letter paths
// cross-platform so a `C:\tools\claude.cmd` arriving on a non-Windows
// runtime is still treated as absolute, not cwd-relative.
func isRelativeExplicitPath(command string) bool {
	if !isExplicitPath(command) {
		return false
	}
	if filepath.IsAbs(command) {
		return false
	}
	// Unix-style absolute path (leading "/") — treat as absolute even on
	// Windows, where filepath.IsAbs returns false for "/usr/local/bin/claude"
	// (Windows absolute paths need a drive letter or UNC prefix). Symmetric
	// with the drive-letter check below, which recognises "C:\..." as absolute
	// on non-Windows runtimes.
	if strings.HasPrefix(command, "/") {
		return false
	}
	if len(command) >= 2 && command[1] == ':' {
		c := command[0]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			return false
		}
	}
	return true
}

/* --------------------------------------------------------------------------
   Prompt detection from structured JSON output
   -------------------------------------------------------------------------- */

// streamLine is a single line read from stdout or stderr.
type streamLine struct {
	text   string
	source string // "stdout" | "stderr"
}

// promptInfo holds parsed prompt/approval information.
type promptInfo struct {
	Text string // The prompt text to display to the user
	Type string // "permission" | "question" | "unknown"
}

// detectPromptFromJSON parses a JSON output line and detects if the CLI is
// requesting user input (permission, approval, question). Returns nil if the
// line is not a prompt.
func detectPromptFromJSON(command, line string) *promptInfo {
	// Quick check: must look like JSON
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return nil
	}

	// Parse as generic JSON
	var event map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &event); err != nil {
		return nil
	}

	base := commandBaseName(command)

	switch {
	case strings.HasPrefix(base, "claude"):
		return detectClaudePrompt(event)
	case strings.HasPrefix(base, "codex"):
		return detectCodexPrompt(event)
	}

	return nil
}

// detectResultEvent returns true if the line is a Claude "result" event,
// signalling that the current turn is complete and stdin can be closed.
// Only applies to Claude sessions using --output-format stream-json.
func detectResultEvent(command, line string) bool {
	if !isClaudeCommand(command) {
		return false
	}
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	var event map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &event); err != nil {
		return false
	}
	eventType, _ := event["type"].(string)
	return eventType == "result"
}

// detectClaudePrompt detects permission requests in Claude's stream-json output.
// Claude emits events like {"type": "tool_use", ...} and permission-related events.
func detectClaudePrompt(event map[string]interface{}) *promptInfo {
	eventType, _ := event["type"].(string)

	// Claude stream-json permission events
	// The exact format depends on Claude Code's implementation, but common patterns:
	switch eventType {
	case "permission_request":
		// Direct permission request
		toolName, _ := event["tool"].(string)
		description, _ := event["description"].(string)
		text := fmt.Sprintf("Permission requested for tool: %s", toolName)
		if description != "" {
			text = description
		}
		return &promptInfo{Text: text, Type: "permission"}

	case "input_request":
		// Generic input request
		prompt, _ := event["prompt"].(string)
		if prompt == "" {
			prompt = "Claude Code is waiting for input"
		}
		return &promptInfo{Text: prompt, Type: "question"}
	}

	return nil
}

// detectCodexPrompt detects approval requests in Codex JSONL output.
func detectCodexPrompt(event map[string]interface{}) *promptInfo {
	eventType, _ := event["type"].(string)

	switch eventType {
	case "approval_request", "command_approval":
		command, _ := event["command"].(string)
		text := "Codex is requesting approval"
		if command != "" {
			text = fmt.Sprintf("Approve command: %s", command)
		}
		return &promptInfo{Text: text, Type: "permission"}
	}

	return nil
}

/* --------------------------------------------------------------------------
   Process signal helpers (platform-specific interrupt is in session_signal_*.go)
   -------------------------------------------------------------------------- */

// jsonEscapeString returns s as a JSON-encoded string literal (with surrounding quotes).
// Used to safely embed user text inside hand-built JSON messages.
func jsonEscapeString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// interruptProcess sends an interrupt signal to the process.
// On Windows this uses GenerateConsoleCtrlEvent, on Unix it sends SIGINT.
func interruptProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return fmt.Errorf("process not started")
	}
	return sendInterrupt(cmd.Process)
}

/* --------------------------------------------------------------------------
   Shutdown helper
   -------------------------------------------------------------------------- */

// ShutdownAllSessions gracefully ends all active sessions.
// Called during application shutdown.
func (sm *SessionManager) ShutdownAllSessions() {
	sm.mu.RLock()
	ids := make([]string, 0, len(sm.sessions))
	for id := range sm.sessions {
		ids = append(ids, id)
	}
	sm.mu.RUnlock()

	if len(ids) > 0 {
		fmt.Printf("%s[session] Shutting down %d active session(s)...%s\n",
			colorYellow, len(ids), colorReset)
	}

	for _, id := range ids {
		_ = sm.EndSession(id)
	}
}

/* --------------------------------------------------------------------------
   Grok raw-CLI permission/approval flag helpers
   --------------------------------------------------------------------------
   These helpers gate Grok's autonomous-execution / permission-bypass flag
   surfaces for the RAW `session_start` path (the managed headless `-p` turn
   built above), independent of the ACP path in grok_acp.go. They were moved
   here when grok 0.2.59 removed `grok agent`'s `--config` flag and the ACP
   driver's `--config`-based neutralizer machinery was deleted — these were
   the only members of that set still referenced (by buildGrokFlagArgs's
   gate-off strips), so they live with their sole remaining consumer.
   -------------------------------------------------------------------------- */

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
