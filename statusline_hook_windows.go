// File: statusline_hook_windows.go
//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// setStatusLineChildProcessGroup gives the chained status-line child its own
// process group so it can be terminated independently of the wrapper — Windows
// has no POSIX `kill(-pgid)`, but the new group lets us tear down the child
// (and, via taskkill /T, its tree) without affecting the rest of the console.
func setStatusLineChildProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

// killStatusLineChild terminates the child and any descendants via `taskkill
// /T /F`. Mirrors the fallback in session_signal_windows.go — on Windows we
// can't relay a POSIX signal, so killing the tree is the reliable analog and
// matches what users had before we wrapped their status-line command.
func killStatusLineChild(p *os.Process, _ os.Signal) {
	if p == nil {
		return
	}
	kill := exec.Command("taskkill", "/PID", fmt.Sprintf("%d", p.Pid), "/T", "/F")
	hideWindow(kill)
	_ = kill.Run()
}
