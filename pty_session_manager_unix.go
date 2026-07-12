//go:build !windows
// +build !windows

// File: pty_session_manager_unix.go
// macOS/Linux PTY path for interactive session_start (opt-in tty=true, allowlisted
// TUI agents only — see isPTYEligibleCommand). The child runs under a real
// pseudo-terminal; its merged output is normalized (pty_normalizer.go) before any
// stream frame is published, and a detected prompt that goes unanswered aborts
// the session. Reuses the normal session lifecycle (waitForExit) so takeover,
// timeout, file-upload, and session_ended all behave as for pipe sessions.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/creack/pty"
)

// startPTYSession spawns the session's child under a PTY and registers a
// CLISession backed by the PTY master (Stdin for interactive SendInput, Stdout/
// Stderr for the merged read). Called with sm.mu held (from StartSession).
func (sm *SessionManager) startPTYSession(id, command string, cliArgs []string,
	cwd, workspaceID, uid string, timeoutMs int64, publishFn PublishFunc) error {

	executable := resolveExecutable(command)
	fmt.Printf("%s[session] Starting PTY session %s: %s %s%s\n",
		colorCyan, id, executable, strings.Join(cliArgs, " "), colorReset)

	proc := exec.Command(executable, cliArgs...)
	if cwd != "" {
		proc.Dir = cwd
	}
	filtered, strippedVars := prepareClaudeChildEnv(command, os.Environ())
	proc.Env = filtered
	if len(strippedVars) > 0 {
		fmt.Printf("%s[session] Stripped env vars from PTY session %s: %s%s\n",
			colorYellow, id, strings.Join(strippedVars, ", "), colorReset)
	}

	ptmx, err := pty.Start(proc)
	if err != nil {
		return fmt.Errorf("failed to start PTY session %s: %w", command, err)
	}

	session := &CLISession{
		ID:             id,
		Command:        command,
		Process:        proc,
		Stdin:          ptmx, // SendInput writes keystrokes to the TUI
		Stdout:         ptmx, // merged read side; Stderr aliases it (PTY merges streams)
		Stderr:         ptmx,
		StartedAt:      time.Now(),
		Status:         "running",
		WorkspaceID:    workspaceID,
		UID:            uid,
		TimeoutMs:      timeoutMs,
		firstRealFrame: make(chan struct{}),
		done:           make(chan struct{}),
		streamDone:     make(chan struct{}),
		publishFn:      publishFn,
	}

	sm.sessions[id] = session
	if proc.Process != nil {
		globalProcessRegistry.Register(proc.Process.Pid, "session:"+id)
	}

	go sm.readPTYStream(session, publishFn, ptmx)
	go sm.waitForExit(session, publishFn)

	fmt.Printf("%s[session] PTY session %s started (PID: %d)%s\n",
		colorGreen, id, proc.Process.Pid, colorReset)
	return nil
}

// readPTYStream reads raw PTY bytes, normalizes them (token-safe), and publishes
// them as `stream` frames. It rate-limits redraws, and aborts the session if a
// prompt is detected and no further output arrives within ptySessionPromptTimeout.
// It closes session.streamDone on exit so waitForExit's drain completes.
func (sm *SessionManager) readPTYStream(session *CLISession, publishFn PublishFunc, ptmx *os.File) {
	defer close(session.streamDone)

	norm := NewPTYNormalizer(DefaultRedrawInterval)
	publish := func(line string) {
		seq := atomic.AddInt64(&session.Seq, 1)
		publishFn(resultMsg{
			ID:          session.ID,
			WorkspaceID: session.WorkspaceID,
			UID:         session.UID,
			Output:      line,
			Status:      "success",
			Ts:          time.Now().UnixMilli(),
			Version:     Version,
			Type:        "stream",
			SessionID:   session.ID,
			Seq:         int(seq),
		})
	}

	dataCh := make(chan []byte, 64)
	exitCh := make(chan struct{}, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		buf := make([]byte, 4096)
		for {
			nr, rerr := ptmx.Read(buf)
			if nr > 0 {
				chunk := make([]byte, nr)
				copy(chunk, buf[:nr])
				select {
				case dataCh <- chunk:
				case <-done:
					return
				}
			}
			if rerr != nil {
				select {
				case exitCh <- struct{}{}:
				case <-done:
				}
				return
			}
		}
	}()

	var promptSince time.Time
	aborting := false
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case chunk := <-dataCh:
			promptSince = time.Time{} // fresh output clears the quiet-after-prompt state
			for _, line := range norm.Write(chunk, time.Now()) {
				publish(line)
			}

		case <-exitCh:
		drain:
			for {
				select {
				case chunk := <-dataCh:
					for _, line := range norm.Write(chunk, time.Now()) {
						publish(line)
					}
				default:
					break drain
				}
			}
			if s, ok := norm.Flush(); ok {
				publish(s)
			}
			return

		case now := <-ticker.C:
			if s, ok := norm.MaybeFlushRedraw(now); ok {
				publish(s)
			}
			if aborting {
				continue // process already killed; wait for exitCh
			}
			if line, ok := norm.PendingPromptLine(); ok {
				if promptSince.IsZero() {
					promptSince = now
				}
				if now.Sub(promptSince) >= ptySessionPromptTimeout {
					publish("[aiexpedite] tty session aborted: prompt \"" + line +
						"\" received no response within " + ptySessionPromptTimeout.String() +
						" (terminal prompts are disabled in tty mode)")
					if session.Process.Process != nil {
						_ = session.Process.Process.Kill()
					}
					aborting = true
				}
			} else {
				promptSince = time.Time{}
			}
		}
	}
}
