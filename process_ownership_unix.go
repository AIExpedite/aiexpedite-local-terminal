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
// unproven; the run parks rather than failing over) — only an OS reboot since
// the session's OS boot proves it gone (os_boot.go), and that proof looks at
// no process at all. Linux cgroups would be the way to add a same-boot proof
// later.
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

// leftoverWait bounds how long ending a session's leftovers waits for them
// to go (killed members linger as zombies until reaped).
var leftoverWait = 3 * time.Second

// endLeftoverProcessTree SIGKILLs what a session left in the process group its
// recorded leader led, ONLY with proof that every member is the session's own:
//
//   - the recorded leader is gone, and no live process holds its PID: a live
//     holder means the PID (and with it the group id) was reused by an
//     unrelated program, which must never be signalled;
//   - every member of the group carries this session's owner marker
//     (session_owner.go). A group id alone proves nothing once the group has
//     been empty, because the kernel may hand it to an unrelated group (Codex
//     P1 on #304); a member we cannot read, or one without the marker, means
//     nothing is signalled at all.
//
// A descendant that left the group (setsid / setpgid) is out of reach: the
// agent does not enumerate process trees (see probeRecordedDescendants).
// Reports whether the group is empty afterwards.
func endLeftoverProcessTree(rec ledgerProcess, job uintptr, owner string) bool {
	if rec.PGID <= 0 || rec.PGID != rec.PID {
		return processGroupEmpty(rec)
	}
	if probeRecordedProcess(rec) != processGone || pidAlive(rec.PID) {
		return false
	}
	members, err := processGroupMembers(rec.PGID)
	if err != nil {
		return false
	}
	live := 0
	for _, m := range members {
		// An exited member (a zombie awaiting its parent) cannot run and has
		// no environment to prove anything with; it is neither a reason to
		// stop nor something to end. Every LIVE member still has to prove
		// itself (Codex P2 on #304).
		if m.Exited {
			continue
		}
		live++
		if owner == "" {
			return false
		}
		env, err := processEnviron(m.PID)
		if err != nil || !envHasSessionOwner(env, owner) {
			return false
		}
	}
	if live == 0 {
		return true
	}
	_ = syscall.Kill(-rec.PGID, syscall.SIGKILL)
	deadline := time.Now().Add(leftoverWait)
	for !processGroupEmpty(rec) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

// groupMember is one process in a process group.
type groupMember struct {
	PID int
	// Exited: a zombie (or dead) process, awaiting its parent's reap.
	Exited bool
}

// pidAlive reports whether any process (zombies included) holds pid.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
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
