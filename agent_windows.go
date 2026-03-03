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

// hideWindow sets Windows-specific flags to hide the console window for a subprocess.
// Merges with any existing SysProcAttr to avoid overwriting other flags.
func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
}

// setNewConsole sets Windows-specific flags to create a new console for the subprocess.
// This is used when launching an updated executable to ensure it gets a fresh console
// with valid stdout/stderr handles from the start.
// Merges with any existing SysProcAttr to avoid overwriting other flags.
func setNewConsole(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags = CREATE_NEW_CONSOLE
}
