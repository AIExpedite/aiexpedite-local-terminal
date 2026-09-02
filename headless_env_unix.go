//go:build !windows
// +build !windows

// File: headless_env_unix.go
// Unix (macOS/Linux) controlling-terminal detachment for non-agent commands.
package main

import (
	"os/exec"
	"syscall"
)

// detachControllingTTY puts the child in a new session (Setsid) so it has no
// controlling terminal. Without this a piped-stdio git/ssh/credential helper
// can still open /dev/tty and prompt, bypassing the captured pipes and hanging
// forever. Setsid also makes the child a process-group leader (pgid == pid),
// which lets killProcessGroup reap the whole descendant tree on timeout.
func detachControllingTTY(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Setsid = true
}

// killProcessGroup terminates the process group led by pid. Used on timeout so
// git descendants (ssh, credential helpers, editors) cannot survive the parent.
// Relies on the child having been started with Setsid (pgid == pid).
func killProcessGroup(pid int) error {
	if pid <= 0 {
		return nil
	}
	return syscall.Kill(-pid, syscall.SIGKILL)
}

// configureCommandCancellation replaces CommandContext's single-process kill
// with whole-process-group cleanup on Unix. Swallowing ESRCH preserves the
// natural context cancellation error returned by Cmd.Wait.
func configureCommandCancellation(c *exec.Cmd) {
	c.Cancel = func() error {
		_ = killProcessGroup(c.Process.Pid)
		return nil
	}
}
