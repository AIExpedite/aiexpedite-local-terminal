//go:build windows
// +build windows

// File: headless_env_windows.go
// Windows console detachment for non-agent commands. ConPTY-backed tty is
// deferred (see pty_session_windows.go); the non-interactive env overlay plus
// no-console-window hardening still apply so git/credential prompts fail fast.
package main

import "os/exec"

// detachControllingTTY hides the console window so a child process cannot draw
// an interactive prompt. Combined with the -NonInteractive PowerShell flags and
// the headless env overlay (GIT_TERMINAL_PROMPT=0 etc.) this keeps non-agent
// commands from blocking on a prompt on Windows.
func detachControllingTTY(c *exec.Cmd) {
	hideWindow(c)
}

// killProcessGroup is a no-op on Windows; timeout kills are handled by the
// context cancellation on exec.CommandContext. Whole-tree termination via Job
// Objects is tracked separately (see EXECUTION_LIVENESS_REDESIGN.md).
func killProcessGroup(pid int) error {
	return nil
}
