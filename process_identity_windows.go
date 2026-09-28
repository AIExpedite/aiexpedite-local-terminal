//go:build windows

// File: process_identity_windows.go
// -----------------------------------------------------------------------------
// Windows half of the spawn-ledger process identity (session_ledger.go).
//
// A PID alone never identifies a process across an agent restart: Windows
// reuses PIDs. Every recorded PID is therefore paired with the process's
// CREATION TIME (GetProcessTimes, a FILETIME fixed at creation), and a PID
// counts as ours only while it is alive AND its creation time is the recorded
// one. Every check opens the PID once and holds that handle across the
// comparison and the kill, and Windows never reuses a PID while any handle to
// its process object is open, so the process compared is the process killed.
//
// Nothing here enumerates processes or reads another process's memory or
// environment: each function only opens the one PID the ledger recorded.
// -----------------------------------------------------------------------------

package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

const processStartTokenPrefixWindows = "win:"

// processStartToken returns the opaque start-time identity of a live PID.
func processStartToken(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid pid %d", pid)
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	return startTokenOfHandle(h)
}

func startTokenOfHandle(h windows.Handle) (string, error) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return "", err
	}
	ticks := uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
	if ticks == 0 {
		return "", errors.New("process has no creation time")
	}
	return processStartTokenPrefixWindows + strconv.FormatUint(ticks, 10), nil
}

// openRecordedProcess opens pid with access and classifies it against the
// recorded start token. The handle is returned (to be closed by the caller)
// only for processOurs.
func openRecordedProcess(rec ledgerProcess, access uint32) (windows.Handle, processProbeResult) {
	if rec.PID <= 0 || !strings.HasPrefix(rec.StartTime, processStartTokenPrefixWindows) {
		return 0, processUnknown
	}
	h, err := windows.OpenProcess(access, false, uint32(rec.PID))
	if err != nil {
		// ERROR_INVALID_PARAMETER: no process object has this PID at all.
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return 0, processGone
		}
		// Access denied (or anything else): the state cannot be read, so it
		// is never counted either way.
		return 0, processUnknown
	}
	// An exited process whose object is still held open elsewhere keeps its
	// PID reserved, so it is the recorded process, and it is gone.
	if ev, err := windows.WaitForSingleObject(h, 0); err == nil && ev == windows.WAIT_OBJECT_0 {
		windows.CloseHandle(h)
		return 0, processGone
	} else if err != nil {
		windows.CloseHandle(h)
		return 0, processUnknown
	}
	token, err := startTokenOfHandle(h)
	if err != nil {
		windows.CloseHandle(h)
		return 0, processUnknown
	}
	if token != rec.StartTime {
		// A live process with another creation time is a stranger that was
		// handed the reused PID: not ours, never touched.
		windows.CloseHandle(h)
		return 0, processGone
	}
	return h, processOurs
}

// probeRecordedProcess reports whether the recorded process is still ours.
func probeRecordedProcess(rec ledgerProcess) processProbeResult {
	h, res := openRecordedProcess(rec, windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE)
	if h != 0 {
		windows.CloseHandle(h)
	}
	return res
}

// runRecordedTreeKill is the tree kill (taskkill /F /T, processes_windows.go)
// behind a seam for tests. It names the PID the caller holds pinned open.
var runRecordedTreeKill = KillProcessTree

// endRecordedProcess ends the recorded process and its tree, ONLY when it is
// still ours (alive, same creation time), and reports whether it is gone
// afterwards. Normally the kill-on-close Job Object already ended it when the
// previous agent process died; this is the fallback for a process that
// escaped that (for example a job assignment that failed).
func endRecordedProcess(rec ledgerProcess, wait time.Duration) processProbeResult {
	access := uint32(windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.SYNCHRONIZE | windows.PROCESS_TERMINATE)
	h, res := openRecordedProcess(rec, access)
	if res != processOurs {
		return res
	}
	defer windows.CloseHandle(h)
	// The handle pins the PID for the tree kill (a PID is never reused while a
	// handle to its process object is open).
	_ = runRecordedTreeKill(rec.PID)
	_ = windows.TerminateProcess(h, 1)
	ms := uint32(wait / time.Millisecond)
	if ev, err := windows.WaitForSingleObject(h, ms); err == nil && ev == windows.WAIT_OBJECT_0 {
		return processGone
	}
	return processOurs
}
