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

/* --------------------------------------------------------------------------
   Constants
   -------------------------------------------------------------------------- */

const (
	// sessionMaxLifetime is the maximum time a session can run before cleanup
	sessionMaxLifetime = 30 * time.Minute

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

	mu   sync.Mutex
	done chan struct{} // closed when process exits
}

/* --------------------------------------------------------------------------
   SessionManager — manages all active sessions
   -------------------------------------------------------------------------- */

// SessionManager tracks and manages active interactive CLI sessions.
type SessionManager struct {
	sessions map[string]*CLISession
	mu       sync.RWMutex
}

// NewSessionManager creates a new SessionManager.
func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions: make(map[string]*CLISession),
	}
}

// PublishFunc is the callback signature for publishing result messages.
type PublishFunc func(res resultMsg)

/* --------------------------------------------------------------------------
   StartSession — spawn a new interactive CLI process
   -------------------------------------------------------------------------- */

// StartSession creates and starts a new interactive CLI session. The process
// is spawned with stdin/stdout/stderr pipes and output is streamed via publishFn.
func (sm *SessionManager) StartSession(id, command string, args []string, cwd, workspaceID, uid string, publishFn PublishFunc) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if _, exists := sm.sessions[id]; exists {
		return fmt.Errorf("session %s already exists", id)
	}

	// Build the CLI command with appropriate flags for structured streaming
	cliArgs := buildInteractiveCLIArgs(command, args)

	// Resolve executable path
	executable := resolveExecutable(command)

	fmt.Printf("%s[session] Starting %s session %s: %s %s%s\n",
		colorCyan, command, id, executable, strings.Join(cliArgs, " "), colorReset)

	proc := exec.Command(executable, cliArgs...)
	hideWindow(proc)
	if cwd != "" {
		proc.Dir = cwd
	}

	// Strip CLAUDECODE env var so Claude Code doesn't detect a nested session
	// and refuse to start. The Go agent may inherit this variable if it was
	// launched from within a Claude Code context.
	cleanEnv := os.Environ()
	filtered := make([]string, 0, len(cleanEnv))
	hadClaudeCode := false
	for _, e := range cleanEnv {
		if strings.HasPrefix(e, "CLAUDECODE=") {
			hadClaudeCode = true
			continue
		}
		filtered = append(filtered, e)
	}
	proc.Env = filtered
	if hadClaudeCode {
		fmt.Printf("%s[session] Stripped CLAUDECODE env var from session %s%s\n",
			colorYellow, id, colorReset)
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
		done:        make(chan struct{}),
	}

	sm.sessions[id] = session

	// Start output reader goroutines
	go sm.readOutputStream(session, publishFn)

	// Start process waiter (detects exit)
	go sm.waitForExit(session, publishFn)

	fmt.Printf("%s[session] Session %s started (PID: %d)%s\n",
		colorGreen, id, proc.Process.Pid, colorReset)

	return nil
}

/* --------------------------------------------------------------------------
   SendInput — write to a session's stdin
   -------------------------------------------------------------------------- */

// SendInput writes text to the stdin of the specified session.
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

	// Write input followed by newline
	_, err := fmt.Fprintln(session.Stdin, text)
	if err != nil {
		return fmt.Errorf("failed to write to session %s stdin: %w", id, err)
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

// removeSession removes a session from the map.
func (sm *SessionManager) removeSession(id string) {
	sm.mu.Lock()
	delete(sm.sessions, id)
	sm.mu.Unlock()
}

// readOutputStream reads stdout and stderr from the session and publishes
// output chunks via the publishFn. It parses JSON events from structured
// output modes to detect permission/approval prompts.
func (sm *SessionManager) readOutputStream(session *CLISession, publishFn PublishFunc) {
	// Merge stdout and stderr into a single channel
	lines := make(chan streamLine, 100)
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(session.Stdout)
		scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024) // 1MB max line
		fmt.Printf("%s[session] stdout scanner started for %s%s\n", colorCyan, session.ID, colorReset)
		lineCount := 0
		for scanner.Scan() {
			lineCount++
			text := scanner.Text()
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
		scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024) // 1MB max line
		fmt.Printf("%s[session] stderr scanner started for %s%s\n", colorCyan, session.ID, colorReset)
		lineCount := 0
		for scanner.Scan() {
			lineCount++
			text := scanner.Text()
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
	// always stays free to drain incoming lines.
	asyncPublish := func(msg resultMsg) {
		go publishFn(msg)
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
	err := session.Process.Wait()

	// Explicitly close pipes so the scanner goroutines in readOutputStream
	// receive EOF and can exit.  On Windows, process exit does not always
	// immediately release the pipe handles, so the scanners could block on
	// Read() indefinitely without this.
	session.Stdout.Close()
	session.Stderr.Close()

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

	// Publish session_ended in a goroutine: publishFn blocks up to 30 s on
	// Pub/Sub network I/O.  Calling it directly here would delay removeSession
	// (and therefore free the session slot for reuse) by up to 30 s, and would
	// race the async stream publishes already in-flight from readOutputStream —
	// the session_ended message could arrive at the client before the last
	// streamed lines despite having a higher sequence number.
	go publishFn(resultMsg{
		ID:          session.ID,
		WorkspaceID: session.WorkspaceID,
		UID:         session.UID,
		Output:      fmt.Sprintf("Session ended (exit code: %d)", session.ExitCode),
		Status:      "success",
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		Type:        "session_ended",
		SessionID:   session.ID,
		ExitCode:    session.ExitCode,
		Seq:         int(seq),
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
func buildInteractiveCLIArgs(command string, args []string) []string {
	cmdLower := strings.ToLower(command)

	switch {
	case cmdLower == "claude" || strings.HasPrefix(cmdLower, "claude"):
		return buildClaudeInteractiveArgs(args)
	case cmdLower == "codex" || strings.HasPrefix(cmdLower, "codex"):
		return buildCodexInteractiveArgs(args)
	case cmdLower == "gemini" || strings.HasPrefix(cmdLower, "gemini"):
		return buildGeminiInteractiveArgs(args)
	default:
		return args
	}
}

// buildClaudeInteractiveArgs builds Claude Code CLI args for interactive streaming.
// Uses --output-format stream-json for structured event output and -p for print mode
// (required for stream-json to work in non-TTY). The prompt words must be joined into
// a single -p value; passing them as separate positional args causes Claude Code to
// fall into interactive TUI mode which hangs without a real TTY.
func buildClaudeInteractiveArgs(args []string) []string {
	result := make([]string, 0, len(args)+4)

	// Add structured streaming output format with --verbose (required for stream-json).
	// Note: --input-format stream-json is NOT used because it causes Claude to block
	// waiting for NDJSON on stdin before producing any output.
	result = append(result, "--output-format", "stream-json")
	result = append(result, "--verbose")
	result = append(result, "--include-partial-messages")

	// Check if user already passed -p / --print
	hasPrint := false
	for _, a := range args {
		if a == "-p" || a == "--print" {
			hasPrint = true
			break
		}
	}

	if hasPrint {
		// User already provided -p/--print — pass args as-is
		result = append(result, args...)
	} else {
		// Separate flags from prompt words so we can join prompt into a single -p value
		var flags []string
		var promptParts []string
		for _, a := range args {
			if strings.HasPrefix(a, "-") {
				flags = append(flags, a)
			} else {
				promptParts = append(promptParts, a)
			}
		}
		// Add any user flags first
		result = append(result, flags...)
		// Add -p with the joined prompt as a single argument
		if len(promptParts) > 0 {
			result = append(result, "-p", strings.Join(promptParts, " "))
		} else {
			// No prompt text — still need --print for non-TTY/streaming mode
			result = append(result, "--print")
		}
	}

	return result
}

// buildCodexInteractiveArgs builds Codex CLI args for interactive streaming.
// Uses exec mode with --json for JSONL event output and --full-auto for
// low-friction sandboxed automatic execution.
func buildCodexInteractiveArgs(args []string) []string {
	result := make([]string, 0, len(args)+4)
	result = append(result, "exec")
	result = append(result, "--json")
	result = append(result, "--full-auto")
	result = append(result, args...)
	return result
}

// buildGeminiInteractiveArgs builds Gemini CLI args for interactive streaming.
// Prompt is a positional arg (must come first), then -o stream-json for structured
// streaming output, and --approval-mode auto_edit to auto-approve file edits
// (stdin relay is not possible when Gemini streams).
func buildGeminiInteractiveArgs(args []string) []string {
	result := make([]string, 0, len(args)+4)
	result = append(result, args...)                        // prompt as positional arg first
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

