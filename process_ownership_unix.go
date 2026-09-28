//go:build !windows

// File: process_ownership_unix.go
// -----------------------------------------------------------------------------
// Unix (macOS / Linux) process ownership for CLI sessions (session_ledger.go).
//
// Every CLI session process leads its own process group, so the whole group
// can be signalled as a unit: the pipe-session, Claude, Codex and Grok spawns
// call ownProcessGroup (Setpgid) before Start; Antigravity / OpenCode turns
// already Setsid (detachControllingTTY) and PTY sessions are Setsid by
// creack/pty, which also makes them group leaders.
//
// There is no kernel equivalent of a kill-on-close Job Object here, so a CLI
// can outlive a crashed agent; the next boot ends it from the spawn ledger,
// signalling its group only after proving the leader is still the recorded
// process (same start time).
// -----------------------------------------------------------------------------

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ownProcessGroup makes the child lead a new process group (pgid == pid).
// Must run immediately before Start, after any other SysProcAttr setup. A
// child already configured with Setsid is left alone: Setsid already makes it
// a group leader, and adding Setpgid to it would make setpgid fail after
// setsid and abort the exec.
func ownProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	if cmd.SysProcAttr.Setsid {
		return
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pgid = 0
}

// processGroupOf returns the child's process group id when the child leads
// its own group, else 0 (a group the child merely joined is never signalled).
func processGroupOf(proc *os.Process) int {
	if proc == nil || proc.Pid <= 0 {
		return 0
	}
	pgid, err := syscall.Getpgid(proc.Pid)
	if err != nil || pgid != proc.Pid {
		return 0
	}
	return pgid
}

// prepareOwnedStart makes a session process lead its own process group.
func prepareOwnedStart(cmd *exec.Cmd) { ownProcessGroup(cmd) }

// startedSuspended: Unix never starts a session process suspended.
func startedSuspended(cmd *exec.Cmd) bool { return false }

// attachSessionJob / releaseSessionJob: Job Objects are Windows-only.
func attachSessionJob(proc *os.Process, suspended bool) (uintptr, bool, error) {
	return 0, false, nil
}
func releaseSessionJob(job uintptr) {}

// signalRecordedGroup is the kill behind a seam for tests.
var signalRecordedGroup = func(rec ledgerProcess) {
	if rec.PGID > 0 && rec.PGID == rec.PID {
		_ = syscall.Kill(-rec.PGID, syscall.SIGKILL)
	}
	_ = syscall.Kill(rec.PID, syscall.SIGKILL)
}

// probeRecordedDescendants answers whether everything a gone process started
// is provably gone too. On Unix it never is: an empty process group proves
// nothing about a descendant that called setsid / setpgid and left it, and
// the agent does not enumerate processes to look for one. There is no kernel
// containment equivalent to a kill-on-close Job Object here, so a Unix
// session is never certified reaped through a process it spawned (it stays
// unproven; the run parks rather than failing over). Linux cgroups would be
// the way to add this proof later.
func probeRecordedDescendants(rec ledgerProcess) processProbeResult {
	return processUnknown
}

// processGroupEmpty reports whether the group the recorded process led has
// no member left. It decides only housekeeping (whether a record is still
// worth keeping), never proof.
func processGroupEmpty(rec ledgerProcess) bool {
	if rec.PGID <= 0 || rec.PGID != rec.PID {
		return true
	}
	err := syscall.Kill(-rec.PGID, 0)
	return errors.Is(err, syscall.ESRCH)
}

// processTreeGone: an exited per-turn leader's process group is empty.
func processTreeGone(p ledgerProcess, job uintptr) bool {
	return processGroupEmpty(p)
}

// endRecordedProcess ends the recorded process (its whole process group when
// it leads one), ONLY when it is still ours (alive, same start time), and
// reports whether it and its group are gone afterwards.
func endRecordedProcess(rec ledgerProcess, wait time.Duration) processProbeResult {
	if res := probeRecordedProcess(rec); res != processOurs {
		return res
	}
	signalRecordedGroup(rec)
	deadline := time.Now().Add(wait)
	for {
		res := probeRecordedProcess(rec)
		if res == processUnknown {
			return res
		}
		// Killed members linger as zombies until init reaps them; wait for
		// the group to empty, within the same bound.
		if res == processGone && processGroupEmpty(rec) {
			return processGone
		}
		if time.Now().After(deadline) {
			return processOurs
		}
		time.Sleep(50 * time.Millisecond)
	}
}
