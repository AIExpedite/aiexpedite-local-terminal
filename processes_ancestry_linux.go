//go:build linux

// File: processes_ancestry_linux.go
// -----------------------------------------------------------------------------
// ScanProcessAncestryChecked on Linux: one pass over /proc/<pid>/stat for the
// parent links. Used only by the Antigravity wrapper resolver, to find the agy
// a `bash -c "agy …"` execute or PTY session started. The orphan scanner and
// ScanCLIProcesses stay unavailable here.
// -----------------------------------------------------------------------------

package main

import (
	"bytes"
	"os"
	"strconv"
	"strings"
)

// ScanProcessAncestryChecked returns rootPID (when it is still running) and its
// descendants within unixAncestryMaxDepth levels. ok=false only when /proc
// cannot be listed. StartTime is left zero: every Unix wrapper is a fresh
// child, so the resolver's floor check has nothing to exclude.
func ScanProcessAncestryChecked(rootPID int) ([]ProcessInfo, bool) {
	if rootPID <= 0 {
		return nil, false
	}
	dirs, err := os.ReadDir("/proc")
	if err != nil {
		return nil, false
	}
	var procs []ProcessInfo
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil || pid <= 0 {
			continue
		}
		raw, err := os.ReadFile("/proc/" + d.Name() + "/stat")
		if err != nil {
			continue // exited mid-scan
		}
		name, ppid, ok := parseProcStatParent(raw)
		if !ok {
			continue
		}
		procs = append(procs, ProcessInfo{PID: pid, ParentPID: ppid, Name: strings.ToLower(name)})
	}
	return unixProcessAncestry(procs, rootPID), true
}

// parseProcStatParent reads comm and ppid from /proc/<pid>/stat: "pid (comm)
// state ppid …". comm may itself hold spaces and parentheses, so it ends at the
// LAST ')'.
func parseProcStatParent(raw []byte) (string, int, bool) {
	open, end := bytes.IndexByte(raw, '('), bytes.LastIndexByte(raw, ')')
	if open < 0 || end < open {
		return "", 0, false
	}
	fields := strings.Fields(string(raw[end+1:]))
	if len(fields) < 2 {
		return "", 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return "", 0, false
	}
	return string(raw[open+1 : end]), ppid, true
}
