//go:build !windows
// +build !windows

// File: agent_other.go
// Non-Windows stub for agent helper functions

package main

import "os/exec"

// hideWindow is a no-op on non-Windows platforms
func hideWindow(cmd *exec.Cmd) {}
