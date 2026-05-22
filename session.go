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

	// Strip CLAUDECODE and CLAUDE_* env vars so Claude Code doesn't detect a
	// nested session or inherit IDE-specific settings (CLAUDE_CODE_ENTRYPOINT,
	// CLAUDE_AGENT_SDK_VERSION, etc.).  The Go agent may inherit these if
	// launched from within a Claude Code or VSCode context.
	cleanEnv := os.Environ()
	filtered := make([]string, 0, len(cleanEnv))
	var strippedVars []string
	for _, e := range cleanEnv {
		upper := strings.ToUpper(e)
		if strings.HasPrefix(upper, "CLAUDECODE=") || strings.HasPrefix(upper, "CLAUDE_") {
			strippedVars = append(strippedVars, e[:strings.Index(e, "=")])
			continue
		}
		filtered = append(filtered, e)
	}
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
	cmdLower := strings.ToLower(session.Command)
	if cmdLower == "claude" || strings.HasPrefix(cmdLower, "claude") {
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
//   - codex is one-shot by design AND now receives its prompt via stdin
//     (the `-` positional placeholder writes the brief through the pipe so
//     multi-KB briefs don't overflow Windows' CreateProcess command-line
//     cap). codex reads stdin to completion before starting inference, so
//     we ALWAYS close stdin after the prompt write — leaving it open hangs
//     codex indefinitely waiting for EOF.
//
//   - gemini and all non-CLI commands (powershell, bash, git, ...) keep the
//     pre-existing rule: close stdin iff no stdinPrompt was queued. Gemini
//     stays on argv-passed prompts for now (its stdin contract for
//     --output-format stream-json is undocumented enough to defer the
//     switch to a follow-up).
func shouldCloseStdinAfterStart(command string, stdinPrompt string) bool {
	// Normalize: claude / claude.exe / claude.cmd should all match.
	cmd := strings.ToLower(command)
	cmd = strings.TrimSuffix(cmd, ".exe")
	cmd = strings.TrimSuffix(cmd, ".cmd")
	cmd = strings.TrimSuffix(cmd, ".bat")
	cmd = strings.TrimSuffix(cmd, ".ps1")
	if cmd == "claude" {
		return false
	}
	if cmd == "codex" {
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

	cmdLower := strings.ToLower(command)
	switch {
	case cmdLower == "claude" || strings.HasPrefix(cmdLower, "claude"):
		return eventType == "result"
	case cmdLower == "codex" || strings.HasPrefix(cmdLower, "codex"):
		// Codex emits turn.completed when the turn is done. thread.completed is
		// the very last event before the process exits.
		return eventType == "thread.completed" || eventType == "turn.completed"
	case cmdLower == "gemini" || strings.HasPrefix(cmdLower, "gemini"):
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
//   - gemini:      prompt stays positional (current behavior — gemini's stdin
//     contract for `--output-format stream-json` is undocumented
//     enough that switching pre-emptively risks regressions)
//   - antigravity: prompt as positional argv via `--print`; v1.0.1 has no
//     --output-format flag and no documented stdin protocol —
//     switch to stdin once agy ships those
//   - other:       prompt stays in args
//
// The caller (StartSession) uses stdinPromptFormat() to decide how to wrap
// the stdinPrompt before writing it to the process stdin.
func buildInteractiveCLIArgs(command string, args []string) ([]string, string) {
	cmdLower := strings.ToLower(command)

	switch {
	case cmdLower == "claude" || strings.HasPrefix(cmdLower, "claude"):
		return buildClaudeInteractiveArgs(args)
	case cmdLower == "codex" || strings.HasPrefix(cmdLower, "codex"):
		return buildCodexInteractiveArgs(args)
	case cmdLower == "gemini" || strings.HasPrefix(cmdLower, "gemini"):
		return buildGeminiInteractiveArgs(args), ""
	case cmdLower == "agy" || strings.HasPrefix(cmdLower, "agy"):
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
	cmd := strings.ToLower(command)
	cmd = strings.TrimSuffix(cmd, ".exe")
	cmd = strings.TrimSuffix(cmd, ".cmd")
	cmd = strings.TrimSuffix(cmd, ".bat")
	cmd = strings.TrimSuffix(cmd, ".ps1")
	switch cmd {
	case "claude":
		return "ndjson"
	case "codex":
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
	// -p / --print are stripped — we never want claude in print/one-shot mode
	// on this path; it would exit after the first turn and break cross-step
	// session.sendInput.
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
func buildCodexInteractiveArgs(args []string) ([]string, string) {
	cleanedArgs := sanitizeCodexExecArgs(args)

	// Split into flag args (with their values) and positional prompt parts.
	// Codex options that consume the next argument as their value — without
	// this, e.g. `--model o3` would treat "o3" as a prompt word.
	valuedFlags := map[string]bool{
		"-c": true, "--config": true,
		"-m": true, "--model": true,
		"-i": true, "--image": true,
		"--enable": true, "--disable": true,
		"--cd": true, "-C": true,
	}

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
			if valuedFlags[a] && i+1 < len(cleanedArgs) {
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
// agy v1.0.1 ships claude-code-shaped flags (--print / --prompt-interactive /
// --dangerously-skip-permissions) but does NOT expose --output-format or
// stream-json input — so we cannot yet drive it as a multi-turn streaming
// session like claude. For now we run a one-shot --print with the prompt as
// a positional arg.
//
// KNOWN LIMITATION (tracked for follow-up): until agy ships a stdin protocol
// for the prompt, multi-KB briefs on Windows risk the same CreateProcess
// ~32KB command-line cap that bit codex. Switch to stdin delivery as soon
// as agy ships --output-format stream-json or a `-` positional placeholder.
func buildAntigravityInteractiveArgs(args []string) []string {
	result := make([]string, 0, len(args)+2)
	result = append(result, "--print")
	result = append(result, "--dangerously-skip-permissions")
	result = append(result, args...)
	return result
}

// buildGeminiInteractiveArgs builds Gemini CLI args for interactive streaming.
// Prompt is a positional arg (must come first), then -o stream-json for structured
// streaming output, and --approval-mode auto_edit to auto-approve file edits
// (stdin relay is not possible when Gemini streams).
func buildGeminiInteractiveArgs(args []string) []string {
	result := make([]string, 0, len(args)+4)
	result = append(result, args...) // prompt as positional arg first
	result = append(result, "-o", "stream-json")
	result = append(result, "--approval-mode", "auto_edit")
	return result
}

/* --------------------------------------------------------------------------
   Executable resolution
   -------------------------------------------------------------------------- */

// resolveExecutable resolves the full path to a CLI executable.
func resolveExecutable(command string) string {
	cmdLower := strings.ToLower(command)

	if cmdLower == "claude" || strings.HasPrefix(cmdLower, "claude") {
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

	cmdLower := strings.ToLower(command)

	switch {
	case cmdLower == "claude" || strings.HasPrefix(cmdLower, "claude"):
		return detectClaudePrompt(event)
	case cmdLower == "codex" || strings.HasPrefix(cmdLower, "codex"):
		return detectCodexPrompt(event)
	case cmdLower == "gemini" || strings.HasPrefix(cmdLower, "gemini"):
		return detectGeminiPrompt(event)
	}

	return nil
}

// detectResultEvent returns true if the line is a Claude "result" event,
// signalling that the current turn is complete and stdin can be closed.
// Only applies to Claude sessions using --output-format stream-json.
func detectResultEvent(command, line string) bool {
	cmdLower := strings.ToLower(command)
	if cmdLower != "claude" && !strings.HasPrefix(cmdLower, "claude") {
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
