//go:build darwin

// File: process_identity_darwin.go
// -----------------------------------------------------------------------------
// macOS half of the spawn-ledger process identity (session_ledger.go).
//
// The start token is the kernel's p_starttime for the PID (sysctl
// kern.proc.pid.<pid>), a timestamp fixed when the process was created, so a
// reused PID never matches it. Only the one recorded PID is read; nothing is
// enumerated.
// -----------------------------------------------------------------------------

package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const processStartTokenPrefixDarwin = "darwin:"

// darwinZombieState is SZOMB from <sys/proc.h>: exited, awaiting reap.
const darwinZombieState = 5

// kinfoForPID is the sysctl reader behind a seam for tests.
var kinfoForPID = func(pid int) (*unix.KinfoProc, error) {
	return unix.SysctlKinfoProc("kern.proc.pid", pid)
}

func darwinStartToken(kp *unix.KinfoProc) (string, error) {
	st := kp.Proc.P_starttime
	if st.Sec == 0 && st.Usec == 0 {
		return "", errors.New("process has no start time")
	}
	return processStartTokenPrefixDarwin + strconv.FormatInt(int64(st.Sec), 10) + "." +
		strconv.FormatInt(int64(st.Usec), 10), nil
}

// processStartToken returns the opaque start-time identity of a live PID.
func processStartToken(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid pid %d", pid)
	}
	kp, err := kinfoForPID(pid)
	if err != nil {
		return "", err
	}
	if int(kp.Proc.P_pid) != pid {
		return "", errors.New("sysctl answered another pid")
	}
	return darwinStartToken(kp)
}

// processStartTokenErrGone reports whether a processStartToken error proves no
// process holds the PID (the kernel returned no record for it). Any other
// error leaves the process unread, not gone.
func processStartTokenErrGone(err error) bool {
	return errors.Is(err, unix.EIO) || errors.Is(err, unix.ESRCH)
}

// probeRecordedProcess reports whether the recorded process is still ours.
func probeRecordedProcess(rec ledgerProcess) processProbeResult {
	if rec.PID <= 0 || !strings.HasPrefix(rec.StartTime, processStartTokenPrefixDarwin) {
		return processUnknown
	}
	kp, err := kinfoForPID(rec.PID)
	if err != nil {
		// SysctlKinfoProc answers EIO when the kernel returned no record for
		// the PID: no such process.
		if errors.Is(err, unix.EIO) || errors.Is(err, unix.ESRCH) {
			return processGone
		}
		return processUnknown
	}
	if int(kp.Proc.P_pid) != rec.PID {
		return processUnknown
	}
	token, err := darwinStartToken(kp)
	if err != nil {
		return processUnknown
	}
	if token != rec.StartTime {
		return processGone // a reused PID: not ours, never touched
	}
	if kp.Proc.P_stat == darwinZombieState {
		return processGone
	}
	return processOurs
}
