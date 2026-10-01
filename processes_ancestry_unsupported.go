//go:build !windows && !linux && !darwin

// File: processes_ancestry_unsupported.go
// Platforms without a process-table reader: the ancestry scan is unavailable,
// never an empty ok result.

package main

// ScanProcessAncestryChecked is unavailable here.
func ScanProcessAncestryChecked(rootPID int) ([]ProcessInfo, bool) {
	_ = rootPID
	return nil, false
}
