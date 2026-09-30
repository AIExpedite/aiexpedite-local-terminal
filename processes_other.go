//go:build !windows
// +build !windows

// File: processes_other.go
// -----------------------------------------------------------------------------
// Cross-platform stub for non-Windows builds. The orphan scanner is a no-op
// on macOS/Linux for now — the immediate problem is Windows-specific (claude
// CLI processes detached from their parent terminal session). The scanner
// loop in orphanScanner.go will see an empty process list and never act.
//
// The CHECKED variants report ok=false: an unimplemented scan is not an empty
// process table, and the Antigravity index must never read it as one.
//
// Future: implement using `ps -eo pid,ppid,lstart,comm` parsing.
// -----------------------------------------------------------------------------

package main

import "fmt"

// ScanCLIProcesses returns an empty list on non-Windows platforms.
func ScanCLIProcesses() []ProcessInfo { return nil }

// ScanCLIProcessesChecked is unavailable here.
func ScanCLIProcessesChecked() ([]ProcessInfo, bool) { return nil, false }

// ScanProcessAncestryChecked is unavailable here.
func ScanProcessAncestryChecked(rootPID int) ([]ProcessInfo, bool) {
	_ = rootPID
	return nil, false
}

// KillProcessTree is a no-op on non-Windows platforms (returns an error so
// callers can log it; the scanner ignores the result).
func KillProcessTree(pid int) error {
	_ = pid
	return fmt.Errorf("KillProcessTree not implemented on this platform")
}
