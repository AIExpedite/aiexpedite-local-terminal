//go:build windows

package main

import (
	"os"
	"syscall"
	"testing"
)

// The tree kill names its target by PID, so a concurrent waitForExit reap
// landing mid-kill is what makes it dangerous: Wait drops the os.Process
// handle, Windows may hand the PID to a stranger, and `taskkill /F /T` takes
// that stranger's whole tree. killProcessTreePinned holds the handle for the
// duration of the call, which keeps the process object — and therefore the
// PID — reserved even after the reap.
//
// This reaps the root from inside the tree kill (the exact interleaving) and
// requires the PID to still resolve to a live process object. Without the pin
// OpenProcess would answer ERROR_INVALID_PARAMETER (87): the record is gone
// and the number is free.
func TestKillProcessTreePinned_PidStaysReservedThroughTheTreeKill(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	proc := startKillTestHang(t, exe)

	var (
		reapErr  error
		openErr  error
		reaped   bool
		resolved bool
	)
	calls := stubKillProcessTree(t, func(pid int) error {
		if err := proc.Process.Kill(); err != nil {
			reapErr = err
			return nil
		}
		if _, err := proc.Process.Wait(); err != nil {
			reapErr = err
			return nil
		}
		reaped = true
		h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
		if err != nil {
			openErr = err
			return nil
		}
		defer syscall.CloseHandle(h)
		resolved = true
		return nil
	})

	treeErr, unreaped := killProcessTreePinned(proc.Process)
	if !unreaped {
		t.Fatal("a live process was reported reaped — the tree kill never ran")
	}
	if treeErr != nil {
		t.Fatalf("tree kill: %v", treeErr)
	}
	if *calls != 1 {
		t.Fatalf("tree kill ran %d time(s), want 1", *calls)
	}
	if reapErr != nil {
		t.Fatalf("reap the root mid-kill: %v", reapErr)
	}
	if !reaped {
		t.Fatal("the root was never reaped mid-kill — the race this pins against did not happen")
	}
	if !resolved {
		t.Fatalf("pid %d stopped resolving while the tree kill held it: %v — a reused pid would have been taskkill'd instead", proc.Process.Pid, openErr)
	}
}

// killSessionProcess must survive that same interleaving: the root is gone by
// the time Process.Kill runs, and an already-dead root is a successful kill.
func TestKillSessionProcess_SurvivesAReapDuringTheTreeKill(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	proc := startKillTestHang(t, exe)
	stubKillProcessTree(t, func(int) error {
		_ = proc.Process.Kill()
		_, _ = proc.Process.Wait()
		return nil
	})
	if err := killSessionProcess(&CLISession{ID: "reap-race", Process: proc}); err != nil {
		t.Fatalf("killSessionProcess with a reap mid-kill: %v", err)
	}
	// Nothing left to wait for: the stub already reaped the root.
}
