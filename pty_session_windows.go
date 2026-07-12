//go:build windows
// +build windows

// File: pty_session_windows.go
// Windows PTY stub. ConPTY-backed tty is deferred; a tty=true request is
// rejected immediately with a clear, captured error rather than silently
// downgraded to pipes. Non-agent hardening and timeout behaviour are unaffected.
package main

import "time"

// runPTYCommand always fails on Windows: PTY sessions are unsupported in this
// build. The caller surfaces errPTYUnsupportedWindows through the normal
// captured-result path.
func runPTYCommand(cmd string, args []string, workDir string, env []string,
	timeout, promptTimeout time.Duration, emit func(string)) (string, bool, string, error) {
	return "", false, "", errPTYUnsupportedWindows
}
