//go:build !windows
// +build !windows

// File: pty_session_unix.go
// macOS/Linux opt-in PTY execution for interactive/TUI CLIs (e.g. agy). The
// child runs under a real pseudo-terminal so it behaves as if attached to a
// terminal; its merged output is normalized (pty_normalizer.go) before any byte
// is streamed upstream, and a detected prompt that goes unanswered aborts the
// session instead of hanging.
package main

import (
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/creack/pty"
)

// ensurePTYTerm guarantees a TERM entry in a PTY child's environment. TUI/curses
// agents (agy/antigravity) read TERM to select their rendering; when the tray/
// desktop app is launched outside a terminal the inherited environment often has
// no TERM, so such an agent sees it unset and aborts even though a real PTY was
// allocated. A caller-supplied TERM (including an intentional TERM=dumb) is left
// untouched; only a missing one is defaulted to xterm-256color.
func ensurePTYTerm(env []string) []string {
	for _, e := range env {
		if strings.HasPrefix(e, "TERM=") {
			return env
		}
	}
	return append(env, "TERM=xterm-256color")
}

// runPTYCommand spawns cmd+args under a PTY, streams normalized lines via emit
// (also accumulated into the returned string), and enforces two deadlines: an
// overall timeout and a prompt-timeout that fires only after a prompt is
// detected and no further output arrives. It returns the accumulated normalized
// output, whether it was aborted, an abort message (when aborted), and the
// child's exit error (nil on abort — the abort message carries the reason).
func runPTYCommand(cmd string, args []string, workDir string, env []string,
	timeout, promptTimeout time.Duration, emit func(string)) (string, bool, string, error) {

	c := exec.Command(cmd, args...)
	if workDir != "" {
		c.Dir = workDir
	}
	// A nil env means "inherit the parent environment" — materialize it via
	// os.Environ() so ensurePTYTerm can guarantee TERM without dropping the rest
	// of the environment (setting c.Env replaces, not merges).
	childEnv := env
	if childEnv == nil {
		childEnv = os.Environ()
	}
	c.Env = ensurePTYTerm(childEnv)

	ptmx, err := pty.Start(c)
	if err != nil {
		return "", false, "", err
	}
	defer func() { _ = ptmx.Close() }()

	if c.Process != nil {
		globalProcessRegistry.Register(c.Process.Pid, "pubsub:pty")
		defer globalProcessRegistry.Deregister(c.Process.Pid)
	}

	// agy's quota is only readable while an agy process is alive, so arm the
	// run-scoped capture poller for the life of this child. Covers both the PTY
	// session path (startPTYSession) and the `execute` path in pubsub.go, and
	// both direct `agy …` argv and shell-wrapped `bash -c "agy …"` payloads. The
	// defer releases it on EVERY exit route — normal exit, overall timeout and
	// prompt-abort. See cliagent_usage_antigravity_capture.go.
	if commandRunsAntigravity(cmd, args) {
		finishQuotaCapture := startAntigravityQuotaCapture("PTY session")
		defer finishQuotaCapture()
	}

	norm := NewPTYNormalizer(DefaultRedrawInterval)
	var sb strings.Builder
	emitLine := func(s string) {
		sb.WriteString(s)
		sb.WriteByte('\n')
		if emit != nil {
			emit(s)
		}
	}

	dataCh := make(chan []byte, 64)
	exitCh := make(chan struct{}, 1)
	// Closed when runPTYCommand returns so the reader goroutine can never leak:
	// on the abort path we return without draining dataCh, and a reader blocked
	// on `dataCh <- chunk` (buffer full) would otherwise hang forever —
	// ptmx.Close() only unblocks a reader parked in Read, not one parked on the
	// channel send.
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

	start := time.Now()
	var promptSince time.Time
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	abort := func(msg string) (string, bool, string, error) {
		_ = c.Process.Kill()
		_ = c.Wait()
		if s, ok := norm.Flush(); ok {
			emitLine(s)
		}
		return sb.String(), true, msg, nil
	}

	for {
		select {
		case chunk := <-dataCh:
			now := time.Now()
			promptSince = time.Time{} // fresh output clears the quiet-after-prompt state
			for _, line := range norm.Write(chunk, now) {
				emitLine(line)
			}

		case <-exitCh:
			// Drain any buffered chunks the reader produced before EOF.
		drain:
			for {
				select {
				case chunk := <-dataCh:
					for _, line := range norm.Write(chunk, time.Now()) {
						emitLine(line)
					}
				default:
					break drain
				}
			}
			if s, ok := norm.Flush(); ok {
				emitLine(s)
			}
			werr := c.Wait()
			return sb.String(), false, "", werr

		case now := <-ticker.C:
			if s, ok := norm.MaybeFlushRedraw(now); ok {
				emitLine(s)
			}
			if line, ok := norm.PendingPromptLine(); ok {
				if promptSince.IsZero() {
					promptSince = now
				}
				if now.Sub(promptSince) >= promptTimeout {
					return abort("terminal session aborted: prompt \"" + line +
						"\" received no response within " + promptTimeout.String() +
						" (terminal prompts are disabled in tty mode)")
				}
			} else {
				promptSince = time.Time{}
			}
			if timeout > 0 && now.Sub(start) >= timeout {
				return abort("terminal session aborted: exceeded " + timeout.String() + " timeout")
			}
		}
	}
}
