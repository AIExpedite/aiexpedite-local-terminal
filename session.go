// File: session.go
// -----------------------------------------------------------------------------
// SessionManager manages long-lived interactive CLI agent sessions (claude,
// codex, gemini).  Each session holds a process with stdin/stdout/stderr pipes.
// Output is streamed back via a publish function, and stdin input can be sent
// by session ID.
// -----------------------------------------------------------------------------

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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
)

/* --------------------------------------------------------------------------
   CLISession — one interactive CLI agent process
   -------------------------------------------------------------------------- */

// CLISession represents a single interactive CLI agent process with
// bidirectional I/O pipes and streaming output.
type CLISession struct {
	ID        string
	Command   string // "claude", "codex", "gemini"
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

	mu         sync.Mutex
	done       chan struct{} // closed when process exits
	streamDone chan struct{} // closed when stdout/stderr and stream publishes finish
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
func (sm *SessionManager) StartSession(id, command string, args []string, cwd, workspaceID, uid string, timeoutMs int64, publishFn PublishFunc) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if _, exists := sm.sessions[id]; exists {
		return fmt.Errorf("session %s already exists", id)
	}

	// Build the CLI command with appropriate flags for structured streaming.
	// stdinPrompt is non-empty for Claude — the prompt is sent as NDJSON on stdin.
	cliArgs, stdinPrompt := buildInteractiveCLIArgs(command, args)

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

	// Set up pipes
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

	// Start the process
	if err := proc.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		stderr.Close()
		return fmt.Errorf("failed to start %s: %w", command, err)
	}

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
		done:        make(chan struct{}),
		streamDone:  make(chan struct{}),
	}

	sm.sessions[id] = session

	// Register the PID so the orphan scanner knows this process is backed by
	// an active session. removeSession() deregisters it on exit.
	if proc.Process != nil {
		globalProcessRegistry.Register(proc.Process.Pid, "session:"+id)
	}

	// Start output reader goroutines
	go sm.readOutputStream(session, publishFn)

	// Start process waiter (detects exit)
	go sm.waitForExit(session, publishFn)

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
		}
	}

	// Close stdin for one-shot sessions. Codex exec appends piped stdin to the
	// prompt, so leaving the pipe open makes it wait indefinitely for EOF.
	if shouldCloseStdinAfterStart(command, stdinPrompt) {
		session.Stdin.Close()
		fmt.Printf("%s[session] Closed stdin for one-shot session %s (%s)%s\n",
			colorYellow, id, command, colorReset)
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

	// Reset status from waiting_input back to running
	if session.Status == "waiting_input" {
		session.Status = "running"
	}

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
//   - codex and gemini are one-shot AND receive their prompt via stdin (codex
//     via the `-` positional placeholder; gemini via its interactive piped
//     stdin) so multi-KB briefs don't overflow Windows' command-line cap. Both
//     read stdin to completion before/at the start of inference, so we ALWAYS
//     close stdin after the prompt write — leaving it open hangs the process
//     indefinitely waiting for EOF. (Gemini's stdin was already closed right
//     after launch on the old argv path, so this is no multi-turn regression.)
//
//   - agy and all non-CLI commands (powershell, bash, git, ...) keep the
//     pre-existing rule: close stdin iff no stdinPrompt was queued. agy gets
//     its prompt on argv (it ignores piped stdin), so stdinPrompt is empty.
func shouldCloseStdinAfterStart(command string, stdinPrompt string) bool {
	// Route through commandBaseName so absolute/relative paths like
	// `/opt/bin/codex` or `C:\tools\gemini.cmd` follow the same stdin policy
	// as bare names — otherwise the argv builder would shape them as stdin-
	// fed codex/gemini sessions while this function left the pipe open,
	// hanging the child waiting for EOF.
	base := commandBaseName(command)
	switch {
	case strings.HasPrefix(base, "claude"):
		return false
	case strings.HasPrefix(base, "codex"), strings.HasPrefix(base, "gemini"):
		return true
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
// a CLI agent turn — Claude "result", Codex "thread.completed"/"turn.completed",
// or Gemini "result". Used to flush any pending stream batch before the CLI
// process exits, so the final text chunk does not race with session_ended.
//
// Returning true means: "this CLI has just announced it is done; flush now and
// expect process exit very soon." Unlike detectResultEvent, this does NOT cause
// stdin to be closed (only Claude needs that — codex/gemini exit on their own
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
	case strings.HasPrefix(base, "gemini"):
		return eventType == "result"
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
			}

			// Detect CLI-terminal events (Claude "result", Codex
			// "thread.completed"/"turn.completed", Gemini "result"). When we see
			// one we flush any buffered text BEFORE the CLI process exits — the
			// process-exit path also flushes via the !ok branch, but on a fast
			// exit (Claude after stdin close, codex/gemini after final event)
			// the timing race can leave the final batch in flight while
			// session_ended is already being published. Flushing here guarantees
			// the last chunk is enqueued for publish before the exit cascade.
			if detectCLITerminalEvent(session.Command, line.text) {
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
			} else {
				displayText := extractDisplayText(session.Command, line.text)
				if displayText != "" {
					batch = append(batch, displayText)
				}
			}

		case <-batchTimer.C:
			flushBatch()
		}
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

	// Explicitly close pipes so the scanner goroutines in readOutputStream
	// receive EOF and can exit.  On Windows, process exit does not always
	// immediately release the pipe handles, so the scanners could block on
	// Read() indefinitely without this.
	session.Stdout.Close()
	session.Stderr.Close()

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
	var uploadedFiles []FileInfo
	var uploadErrors []UploadError
	cfg := sm.Config
	if cfg != nil && cfg.EnableFileUpload && session.WorkspaceID != "" {
		effectiveDir := session.Process.Dir
		if effectiveDir == "" {
			effectiveDir = getTrackedCwd()
		}
		if effectiveDir == "" {
			// Without a workdir we cannot scope the upload scan, so we skip
			// it entirely. Surface the reason — silently dropping every
			// screenshot from a session is the kind of thing operators need
			// to see, not have to git-blame for.
			fmt.Printf("[session-file-upload] Skipping upload for session %s — no effective workdir (Process.Dir and trackedCwd both empty)\n", session.ID)
		} else {
			files := detectOutputFilesSince(effectiveDir, session.StartedAt)
			if len(files) > 0 {
				fmt.Printf("[session-file-upload] Detected %d output files, uploading to GCS (workspace: %s)...\n", len(files), session.WorkspaceID)

				uploadCtx, uploadCancel := context.WithTimeout(context.Background(), 5*time.Minute)
				storageClient, storageErr := GetStorageClient(uploadCtx)
				if storageErr != nil {
					uploadCancel()
					fmt.Printf("[session-file-upload] Failed to get storage client: %v\n", storageErr)
				} else {
					logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
					uploadResult := UploadFiles(
						uploadCtx,
						storageClient,
						cfg.StorageBucket,
						files,
						session.WorkspaceID,
						session.ID,
						logger,
					)
					uploadCancel()

					uploadedFiles = uploadResult.Successful
					uploadErrors = uploadResult.Failed

					fmt.Printf("[session-file-upload] Upload complete: %d successful, %d failed\n",
						len(uploadResult.Successful), len(uploadResult.Failed))
				}
			}
		}
	}

	// Publish session_ended in a goroutine: publishFn blocks up to 30 s on
	// Pub/Sub network I/O.  Calling it directly here would delay removeSession
	// (and therefore free the session slot for reuse) by up to 30 s, and would
	// race the async stream publishes already in-flight from readOutputStream —
	// the session_ended message could arrive at the client before the last
	// streamed lines despite having a higher sequence number.
	go publishFn(resultMsg{
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
// The billing-var strip is gated on isClaudeCommand(command): codex / gemini
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
// Non-claude commands (codex / gemini / shells) get only the sanitise step;
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
//   - gemini:      stdinPrompt is the prompt, written as raw text; gemini runs
//     in its default INTERACTIVE mode (no -p) and reads the prompt
//     from the piped stdin, so multi-KB briefs don't overflow
//     Windows' cmd.exe command-line cap. Over a pipe gemini reads
//     to EOF, so stdin is closed after the write (one-shot).
//   - antigravity: prompt as positional argv via `--print`. agy does NOT read
//     piped stdin (verified against agy 1.0.4: ignored in both
//     interactive and --print modes — it needs a real TTY), so the
//     prompt must stay on argv. agy resolves to a native `agy.exe`
//     (NOT a cmd.exe shim), so the relevant cap is the 32KB
//     CreateProcess limit, not gemini's 8191-char cmd.exe cap.
//   - other:       prompt stays in args
//
// The caller (StartSession) uses stdinPromptFormat() to decide how to wrap
// the stdinPrompt before writing it to the process stdin.
func buildInteractiveCLIArgs(command string, args []string) ([]string, string) {
	base := commandBaseName(command)

	switch {
	case strings.HasPrefix(base, "claude"):
		return buildClaudeInteractiveArgs(args)
	case strings.HasPrefix(base, "codex"):
		return buildCodexInteractiveArgs(args)
	case strings.HasPrefix(base, "gemini"):
		return buildGeminiInteractiveArgs(args)
	case strings.HasPrefix(base, "agy"):
		return buildAntigravityInteractiveArgs(args), ""
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
	case strings.HasPrefix(base, "codex"), strings.HasPrefix(base, "gemini"):
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
// one-shot prompt execution. The prompt stays a POSITIONAL argv.
//
// agy v1.0.x ships claude-code-shaped flags (--print / --prompt-interactive /
// --dangerously-skip-permissions) but does NOT expose --output-format or
// stream-json input — so we cannot drive it as a multi-turn streaming session
// like claude. We run a one-shot `--print` with the prompt as a positional arg.
//
// WHY NOT STDIN (unlike gemini/codex): agy does NOT read a piped stdin —
// verified live against agy 1.0.4, piped input is ignored in BOTH interactive
// and --print modes (agy appears to require a real TTY for interactive input),
// so the prompt MUST be passed on argv. agy also resolves to a NATIVE `agy.exe`
// (not an npm cmd.exe/.ps1 shim), so it is launched directly via CreateProcess —
// the relevant ceiling is the ~32KB CreateProcess command-line cap, NOT gemini's
// 8191-char cmd.exe cap. So agy does NOT have the "command line is too long."
// failure for normal (≤ ~32KB) briefs; only briefs approaching 32KB would risk
// it, which would need a TTY/ACP-style redesign rather than the stdin trick.
func buildAntigravityInteractiveArgs(args []string) []string {
	result := make([]string, 0, len(args)+2)
	result = append(result, "--print")
	result = append(result, "--dangerously-skip-permissions")
	result = append(result, args...)
	return result
}

// buildGeminiInteractiveArgs builds Gemini CLI args, routing the prompt via
// STDIN instead of argv and running gemini in its default INTERACTIVE mode
// (no -p/--print).
//
// WHY STDIN: on Windows, `gemini` resolves to the npm `gemini.cmd` / `gemini.ps1`
// shim, which cmd.exe executes. cmd.exe rejects command lines longer than 8191
// characters with "The command line is too long." A multi-KB kickoff brief
// passed as a positional arg blew past that limit — the CLI exited 1 with no
// work done (observed as a failing "kickoff tertiary: gemini with 9 KB brief").
//
// WHY NO -p: gemini's interactive mode reads the prompt from a piped (non-TTY)
// stdin just like headless `-p` does — verified against gemini 0.32.1: piped
// stdin becomes the user prompt and `-o stream-json` output is emitted as
// expected — so we run it WITHOUT -p per the chosen integration style.
// `-o stream-json` keeps the structured output the stream parser understands;
// `--approval-mode auto_edit` preserves the prior autonomy posture. A
// caller-supplied `-p`/`--prompt` is still stripped from argv and folded into
// the stdin prompt below (we never want -p on argv — it would switch gemini to
// headless mode and collide with the interactive piped-stdin contract).
//
// LIFECYCLE: over a pipe, gemini reads stdin to EOF and treats it as a SINGLE
// prompt (verified: two newline-separated lines arrived as one user message),
// so StartSession writes the brief then CLOSES stdin to let gemini start — see
// shouldCloseStdinAfterStart (returns true for gemini). This matches gemini's
// pre-existing one-shot lifecycle on the old argv path. True multi-turn over a
// pipe is not possible here (gemini would block waiting for EOF); that would
// require gemini's experimental ACP stdio protocol — a separate, larger change.
//
// Returns (cliArgs, stdinPrompt):
//   - cliArgs carries only flags (no prompt on argv, no -p).
//   - stdinPrompt is the joined positional + caller -p/--prompt text.
func buildGeminiInteractiveArgs(args []string) ([]string, string) {
	// Gemini flags that consume the next argument as their value — without this
	// e.g. `--model gemini-3-pro` would treat "gemini-3-pro" as a prompt word.
	// The `--flag=value` form is a single token and doesn't need an entry here.
	// -p/--prompt is intentionally NOT in this map: when the caller supplies a
	// prompt flag, we route its value into stdinPrompt below instead of leaving
	// a stray `-p VALUE` on argv (any -p would switch gemini to headless mode).
	valuedFlags := map[string]bool{
		"-m": true, "--model": true,
		"-o": true, "--output-format": true,
		"-e": true, "--extensions": true,
		"-r": true, "--resume": true,
		"-i": true, "--prompt-interactive": true,
		"-w": true, "--worktree": true,
		"--approval-mode": true, "--policy": true,
		"--allowed-tools": true, "--allowed-mcp-server-names": true,
		"--include-directories": true, "--delete-session": true,
	}

	// Split user-provided flags (with their values) from positional prompt
	// parts. Positional parts become the stdin prompt; flags stay on argv.
	// User-supplied `-p`/`--prompt` is converted into stdin prompt input — we
	// must never leave a `-p` on argv because it switches gemini out of the
	// interactive piped-stdin mode this builder relies on.
	var flagArgs []string
	var promptParts []string
	skipNext := false
	captureNextAsPrompt := false
	for i, a := range args {
		if skipNext {
			skipNext = false
			flagArgs = append(flagArgs, a)
			continue
		}
		if captureNextAsPrompt {
			captureNextAsPrompt = false
			if a != "" {
				promptParts = append(promptParts, a)
			}
			continue
		}
		if strings.HasPrefix(a, "-") {
			if a == "-p" || a == "--prompt" {
				if i+1 < len(args) {
					captureNextAsPrompt = true
				}
				continue
			}
			if strings.HasPrefix(a, "-p=") {
				if v := strings.TrimPrefix(a, "-p="); v != "" {
					promptParts = append(promptParts, v)
				}
				continue
			}
			if strings.HasPrefix(a, "--prompt=") {
				if v := strings.TrimPrefix(a, "--prompt="); v != "" {
					promptParts = append(promptParts, v)
				}
				continue
			}
			flagArgs = append(flagArgs, a)
			if !strings.Contains(a, "=") && valuedFlags[a] && i+1 < len(args) {
				skipNext = true
			}
			continue
		}
		promptParts = append(promptParts, a)
	}

	result := make([]string, 0, len(flagArgs)+4)
	result = append(result, flagArgs...)
	result = append(result, "-o", "stream-json")
	result = append(result, "--approval-mode", "auto_edit")
	// No -p: gemini runs in interactive mode and reads the prompt from the
	// piped stdin (StartSession writes it, then closes stdin so gemini starts).

	return result, strings.Join(promptParts, " ")
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

	// For codex and gemini, try PATH lookup
	if path, err := exec.LookPath(command); err == nil {
		return path
	}
	if path, err := exec.LookPath(command + ".exe"); err == nil {
		return path
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
	case strings.HasPrefix(base, "gemini"):
		return detectGeminiPrompt(event)
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

// detectGeminiPrompt detects approval requests in Gemini stream-json output.
func detectGeminiPrompt(event map[string]interface{}) *promptInfo {
	eventType, _ := event["type"].(string)

	switch eventType {
	case "toolCallApproval", "approval_request":
		toolName, _ := event["toolName"].(string)
		text := "Gemini CLI is requesting tool approval"
		if toolName != "" {
			text = fmt.Sprintf("Approve tool: %s", toolName)
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
