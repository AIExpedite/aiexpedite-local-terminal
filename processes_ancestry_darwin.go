//go:build darwin

// File: processes_ancestry_darwin.go
// -----------------------------------------------------------------------------
// ScanProcessAncestryChecked on macOS: one sysctl kern.proc.all read for the
// parent links. Used only by the Antigravity wrapper resolver, to find the agy
// a `bash -c "agy …"` execute or PTY session started. The orphan scanner and
// ScanCLIProcesses stay unavailable here.
// -----------------------------------------------------------------------------

package main

import (
	"strings"

	"golang.org/x/sys/unix"
)

// kinfoAllProcs is the process-table reader behind a seam for tests.
var kinfoAllProcs = func() ([]unix.KinfoProc, error) {
	return unix.SysctlKinfoProcSlice("kern.proc.all")
}

// ScanProcessAncestryChecked returns rootPID (when it is still running) and its
// descendants within unixAncestryMaxDepth levels. ok=false only when the table
// cannot be read. StartTime is left zero: every Unix wrapper is a fresh child,
// so the resolver's floor check has nothing to exclude.
func ScanProcessAncestryChecked(rootPID int) ([]ProcessInfo, bool) {
	if rootPID <= 0 {
		return nil, false
	}
	kps, err := kinfoAllProcs()
	if err != nil {
		return nil, false
	}
	procs := make([]ProcessInfo, 0, len(kps))
	for i := range kps {
		kp := &kps[i]
		procs = append(procs, ProcessInfo{
			PID:       int(kp.Proc.P_pid),
			ParentPID: int(kp.Eproc.Ppid),
			Name:      strings.ToLower(unix.ByteSliceToString(kp.Proc.P_comm[:])),
		})
	}
	return unixProcessAncestry(procs, rootPID), true
}
