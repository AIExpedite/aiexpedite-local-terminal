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
// own children are covered too. The CLI is created SUSPENDED
// (prepareOwnedStart), assigned while it cannot run, and only then resumed,
// so no child of it can ever be created outside the job. A failed
// assignment is logged and the session runs unowned: it is then not recorded
// "contained", so its descendants are never vouched for at the next boot.
//
// When a session ends normally the job's kill-on-close limit is cleared
// before the handle is closed, so a clean end keeps exactly the behaviour it
// had before Job Objects (session teardown owns what happens to the tree).
// -----------------------------------------------------------------------------

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// probeRecordedDescendants answers, once the recorded process is gone,
// whether what it started is provably gone too: only when it ran in a
// kill-on-close job, which ended every descendant when the agent died.
// Nothing else is looked up.
func probeRecordedDescendants(rec ledgerProcess) processProbeResult {
	if rec.Contained {
		return processGone
	}
	return processUnknown
}

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

// prepareOwnedStart creates the session process SUSPENDED: its main thread
// does not run until trackSessionProcess has put it in its job, so no child
// of it can ever be created outside the job.
func prepareOwnedStart(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
}

// startedSuspended reports whether cmd was created suspended.
func startedSuspended(cmd *exec.Cmd) bool {
	return cmd != nil && cmd.SysProcAttr != nil &&
		cmd.SysProcAttr.CreationFlags&windows.CREATE_SUSPENDED != 0
}

var procNtResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

// resumeProcess resumes every thread of a process created suspended.
//
// os/exec returns no thread handle, so the main thread must be found another
// way. NtResumeProcess (ntdll, present on every supported Windows) resumes
// the threads of the ONE process whose handle we already hold pinned, in one
// call. The alternative, CreateToolhelp32Snapshot(TH32CS_SNAPTHREAD), snapshots
// every thread on the machine and filters by PID: it touches other processes'
// state (which this agent deliberately never enumerates) and names threads by
// id, which can be reused. A failed resume terminates the process (see
// attachSessionJob), so a session never hangs suspended.
var resumeProcess = func(h windows.Handle) error {
	if err := procNtResumeProcess.Find(); err != nil {
		return err
	}
	status, _, _ := procNtResumeProcess.Call(uintptr(h))
	if status != 0 {
		return fmt.Errorf("NtResumeProcess: NTSTATUS 0x%08x", uint32(status))
	}
	return nil
}

// attachSessionJob creates a kill-on-close Job Object and assigns proc to it,
// then, when proc was created suspended, resumes it. A suspended process is
// ALWAYS resumed — or, when it cannot be, terminated, so it never hangs —
// whatever happened to the job. contained is true only when the assignment
// happened before the process ran (suspended): then nothing it started can be
// outside the job. The returned handle must be passed to releaseSessionJob
// when the session ends. proc's own handle is pinned throughout, so a reused
// PID can never be confused with it.
func attachSessionJob(proc *os.Process, suspended bool) (uintptr, bool, error) {
	if proc == nil {
		return 0, false, errors.New("no process")
	}
	var (
		job       windows.Handle
		assignErr error
		resumeErr error
	)
	handleErr := proc.WithHandle(func(raw uintptr) {
		h := windows.Handle(raw)
		job, assignErr = windows.CreateJobObject(nil, nil)
		if assignErr == nil {
			if assignErr = setJobKillOnClose(job, true); assignErr == nil {
				assignErr = windows.AssignProcessToJobObject(job, h)
			}
			if assignErr != nil {
				windows.CloseHandle(job)
				job = 0
			}
		}
		if suspended {
			if resumeErr = resumeProcess(h); resumeErr != nil {
				_ = windows.TerminateProcess(h, 1)
			}
		}
	})
	if handleErr != nil {
		return 0, false, handleErr
	}
	if resumeErr != nil {
		if job != 0 {
			windows.CloseHandle(job)
		}
		return 0, false, fmt.Errorf("resume suspended session process: %w", resumeErr)
	}
	if assignErr != nil {
		return 0, false, assignErr
	}
	return uintptr(job), suspended, nil
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
