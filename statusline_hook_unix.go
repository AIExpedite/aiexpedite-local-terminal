// File: statusline_hook_unix.go
//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// setStatusLineChildProcessGroup puts the chained status-line child in its own
// process group so a single `kill(-pgid, sig)` terminates the whole pipeline
// (the wrapping `sh -c "cmd | thing"` AND any grandchildren) rather than only
// the immediate shell — otherwise a `sleep` / `curl` inside the user's command
// keeps running after we signal the shell.
func setStatusLineChildProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killStatusLineChild forwards the termination signal we received to the
// child's whole process group. Best-effort: if the group is already gone the
// error is ignored.
func killStatusLineChild(p *os.Process, sig os.Signal) {
	if p == nil {
		return
	}
	s, ok := sig.(syscall.Signal)
	if !ok {
		s = syscall.SIGTERM
	}
	_ = syscall.Kill(-p.Pid, s)
}
