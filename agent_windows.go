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
	// CREATE_NO_WINDOW: the child is a console application that runs without a
	// console window at all. See hideWindow for why HideWindow alone is not
	// enough.
	CREATE_NO_WINDOW = 0x08000000
)

// hideWindow keeps a background child off the user's desktop.
//
// Two flags, because Windows has two console hosts. HideWindow
// (STARTF_USESHOWWINDOW + SW_HIDE) is honoured by the classic conhost, but on a
// machine whose default terminal application is Windows Terminal the new
// console a windowless parent creates is handed to Windows Terminal, which
// ignores the show-window request and opens a visible tab titled with the
// child's path — the "momentary terminal window" every usage refresh flashed
// on a Windows 11 desktop (`agy models` was the visible one; confirmed with a
// window-event hook, 2026-09-15). CREATE_NO_WINDOW creates the console without
// any window, so nothing is delegated to the terminal app in the first place;
// the child's stdio pipes are unaffected and its descendants inherit the
// windowless console.
//
// Merges into any existing SysProcAttr rather than replacing it (a caller may
// already carry CmdLine or CREATE_NEW_PROCESS_GROUP). Never combine with
// setNewConsole: CREATE_NEW_CONSOLE makes Windows ignore CREATE_NO_WINDOW.
func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= CREATE_NO_WINDOW
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
