//go:build !windows
// +build !windows

// File: processes_other.go
// -----------------------------------------------------------------------------
// Cross-platform stub for non-Windows builds. The orphan scanner is a no-op
// on macOS/Linux for now — the immediate problem is Windows-specific (claude
// CLI processes detached from their parent terminal session). The scanner
// loop in orphanScanner.go will see an empty process list and never act.
//
// Future: implement using `ps -eo pid,ppid,lstart,comm` parsing.
// -----------------------------------------------------------------------------

package main

import (
	"fmt"
	"time"
)

// ProcessInfo describes a running OS process at scan time.
type ProcessInfo struct {
	PID       int
	ParentPID int
	Name      string
	StartTime time.Time
}

// ScanCLIProcesses returns an empty list on non-Windows platforms.
func ScanCLIProcesses() []ProcessInfo { return nil }

// KillProcessTree is a no-op on non-Windows platforms (returns an error so
// callers can log it; the scanner ignores the result).
func KillProcessTree(pid int) error {
	_ = pid
	return fmt.Errorf("KillProcessTree not implemented on this platform")
}
