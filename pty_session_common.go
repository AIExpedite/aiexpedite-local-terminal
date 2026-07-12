// File: pty_session_common.go
// Cross-platform declarations for the opt-in PTY session path. The actual spawn
// lives in the platform files: pty_session_unix.go (macOS/Linux, real PTY) and
// pty_session_windows.go (ConPTY deferred — rejected with a clear error).
package main

import (
	"errors"
	"time"
)

// errPTYUnsupportedWindows is surfaced (captured/streamed) when a tty=true
// request lands on Windows. ConPTY is deferred to a later cut; the request is
// rejected rather than silently downgraded to pipes so callers get a truthful
// signal. All non-agent hardening and timeout behaviour still work on Windows.
var errPTYUnsupportedWindows = errors.New(
	"tty/PTY sessions are not supported on Windows in this build (ConPTY deferred); rerun the command without tty")

// DefaultPTYPromptTimeout is how long a tty session may sit on a detected input
// prompt with no further output before it is aborted. Gated on a DETECTED
// prompt (not general quiet) so a TUI legitimately working after output is not
// killed. Order-of-magnitude aligned with the utility short window.
const DefaultPTYPromptTimeout = 15 * time.Minute

// ptySessionPromptTimeout is the quiet-after-prompt abort window for an
// interactive PTY session (readPTYStream). A package var (not a const) so tests
// can shorten it; production uses DefaultPTYPromptTimeout.
var ptySessionPromptTimeout = DefaultPTYPromptTimeout
