//go:build windows

// File: process_ownership_windows.go
// -----------------------------------------------------------------------------
// Windows process ownership for CLI sessions (session_ledger.go).
//
// Every CLI session process is assigned, right after Start, to its own Job
// Object created with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE. Only this agent
// holds the job handle (it is not inheritable), so when the agent process
// dies for ANY reason — crash, kill, power-button restart of the tray app —
// Windows closes the handle and ends the CLI and every descendant it started.
// That is what the 2026-09-25 AIE2 incident lacked: claude.exe and codex.exe
// survived the agent as orphans.
//
// Descendants inherit the job from the moment they are created, so a CLI's
// own children are covered too. The CLI is assigned right after
// CreateProcess returns, before it has had time to start children of its own;
// a failed assignment is logged and the session runs unowned (the next boot
// still ends it from the ledger, by recorded PID and start time).
//
// When a session ends normally the job's kill-on-close limit is cleared
// before the handle is closed, so a clean end keeps exactly the behaviour it
// had before Job Objects (session teardown owns what happens to the tree).
// -----------------------------------------------------------------------------

package main

import (
	"errors"
	"os"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ownProcessGroup is Unix-only (process groups); Windows uses Job Objects.
func ownProcessGroup(cmd *exec.Cmd) {}

// processGroupOf is Unix-only.
func processGroupOf(proc *os.Process) int { return 0 }

func setJobKillOnClose(job windows.Handle, on bool) error {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if on {
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	}
	_, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	return err
}

// attachSessionJob creates a kill-on-close Job Object and assigns proc to it.
// The returned handle must be passed to releaseSessionJob when the session
// ends. proc's own handle is pinned for the assignment, so a process that
// already exited and was reaped is never confused with a reused PID.
func attachSessionJob(proc *os.Process) (uintptr, error) {
	if proc == nil {
		return 0, errors.New("no process")
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	if err := setJobKillOnClose(job, true); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	var assignErr error
	if err := proc.WithHandle(func(h uintptr) {
		assignErr = windows.AssignProcessToJobObject(job, windows.Handle(h))
	}); err != nil {
		assignErr = err
	}
	if assignErr != nil {
		windows.CloseHandle(job)
		return 0, assignErr
	}
	return uintptr(job), nil
}

// releaseSessionJob clears the kill-on-close limit and closes the handle.
func releaseSessionJob(job uintptr) {
	if job == 0 {
		return
	}
	h := windows.Handle(job)
	_ = setJobKillOnClose(h, false)
	_ = windows.CloseHandle(h)
}
