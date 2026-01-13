//go:build windows
// +build windows

// File: agent_windows.go
// Windows-specific helper functions for agent.go

package main

import (
	"os/exec"
	"syscall"
)

// hideWindow sets Windows-specific flags to hide the console window for a subprocess
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
