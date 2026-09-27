//go:build linux

// File: process_identity_linux.go
// -----------------------------------------------------------------------------
// Linux half of the spawn-ledger process identity (session_ledger.go).
//
// The start token is the kernel's own record of when the process started:
// field 22 of /proc/<pid>/stat (`starttime`, clock ticks since boot), tied to
// the machine boot by /proc/sys/kernel/random/boot_id. Both are fixed for the
// life of a process and immune to wall-clock changes, so a PID that was reused
// by another process (or survives only as a number across a reboot) can never
// match. Only the one recorded PID is read; nothing is enumerated.
// -----------------------------------------------------------------------------

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
)

const processStartTokenPrefixLinux = "linux:"

// readProcStat is the /proc reader behind a seam for tests.
var readProcStat = func(pid int) ([]byte, error) {
	return os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
}

var linuxBootID = func() string {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// parseProcStat returns the state letter and starttime of a /proc/<pid>/stat
// line. The command name (field 2) is parenthesised and may itself contain
// spaces or parentheses, so fields are counted after the LAST ')'.
func parseProcStat(raw []byte) (state byte, starttime string, err error) {
	s := string(raw)
	end := strings.LastIndexByte(s, ')')
	if end < 0 || end+2 > len(s) {
		return 0, "", errors.New("malformed stat")
	}
	fields := strings.Fields(s[end+1:])
	// fields[0] is field 3 (state); starttime is field 22 → fields[19].
	if len(fields) < 20 || len(fields[0]) != 1 {
		return 0, "", errors.New("malformed stat")
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return 0, "", fmt.Errorf("malformed starttime: %w", err)
	}
	return fields[0][0], fields[19], nil
}

// processStartToken returns the opaque start-time identity of a live PID.
func processStartToken(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid pid %d", pid)
	}
	raw, err := readProcStat(pid)
	if err != nil {
		return "", err
	}
	_, starttime, err := parseProcStat(raw)
	if err != nil {
		return "", err
	}
	return processStartTokenPrefixLinux + linuxBootID() + ":" + starttime, nil
}

// probeRecordedProcess reports whether the recorded process is still ours.
func probeRecordedProcess(rec ledgerProcess) processProbeResult {
	if rec.PID <= 0 || !strings.HasPrefix(rec.StartTime, processStartTokenPrefixLinux) {
		return processUnknown
	}
	raw, err := readProcStat(rec.PID)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return processGone
		}
		return processUnknown
	}
	state, starttime, err := parseProcStat(raw)
	if err != nil {
		return processUnknown
	}
	if processStartTokenPrefixLinux+linuxBootID()+":"+starttime != rec.StartTime {
		return processGone // a reused PID: not ours, never touched
	}
	// A zombie (Z) or dead (X) process has exited; it cannot act.
	if state == 'Z' || state == 'X' {
		return processGone
	}
	return processOurs
}
