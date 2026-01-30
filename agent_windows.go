//go:build windows
// +build windows

// File: agent_windows.go
// Windows-specific helper functions for agent.go

package main

import (
	"os/exec"
	"syscall"
)

// Windows process creation flags
const (
	CREATE_NEW_CONSOLE = 0x00000010
)

// hideWindow sets Windows-specific flags to hide the console window for a subprocess
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

// setNewConsole sets Windows-specific flags to create a new console for the subprocess.
// This is used when launching an updated executable to ensure it gets a fresh console
// with valid stdout/stderr handles from the start.
func setNewConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: CREATE_NEW_CONSOLE,
	}
}
