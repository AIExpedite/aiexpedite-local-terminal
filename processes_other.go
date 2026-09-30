//go:build !windows
// +build !windows

// File: processes_other.go
// -----------------------------------------------------------------------------
// Cross-platform stub for non-Windows builds. The orphan scanner is a no-op
// on macOS/Linux for now — the immediate problem is Windows-specific (claude
// CLI processes detached from their parent terminal session). The scanner
// loop in orphanScanner.go will see an empty process list and never act.
//
// ScanCLIProcessesChecked reports ok=false: an unimplemented scan is not an
// empty process table, and the Antigravity index must never read it as one.
// ScanProcessAncestryChecked is implemented per platform
// (processes_ancestry_*.go) for the Antigravity wrapper resolver only.
//
// Future: implement using `ps -eo pid,ppid,lstart,comm` parsing.
// -----------------------------------------------------------------------------

package main

import "fmt"

// ScanCLIProcesses returns an empty list on non-Windows platforms.
func ScanCLIProcesses() []ProcessInfo { return nil }

// ScanCLIProcessesChecked is unavailable here.
func ScanCLIProcessesChecked() ([]ProcessInfo, bool) { return nil, false }

// unixAncestryMaxDepth bounds the Unix wrapper walk: the shell, the file-mode
// launcher's inner shell, then agy.
const unixAncestryMaxDepth = 3

// unixProcessAncestry is rootPID itself (a POSIX shell given one simple
// command execs it in place, so the wrapper PID may already be agy) followed by
// its descendants within unixAncestryMaxDepth levels, nearest first.
func unixProcessAncestry(procs []ProcessInfo, rootPID int) []ProcessInfo {
	out := []ProcessInfo{}
	for _, p := range procs {
		if p.PID == rootPID {
			out = append(out, p)
			break
		}
	}
	return append(out, filterProcessAncestry(procs, rootPID, unixAncestryMaxDepth)...)
}

// KillProcessTree is a no-op on non-Windows platforms (returns an error so
// callers can log it; the scanner ignores the result).
func KillProcessTree(pid int) error {
	_ = pid
	return fmt.Errorf("KillProcessTree not implemented on this platform")
}
