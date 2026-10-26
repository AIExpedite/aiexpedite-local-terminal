//go:build windows

// File: powershell_ps51_windows_test.go
// Runs the persistent-host stderr and exit-code cases on Windows PowerShell 5.1
// (powershell.exe). newTestPersistentPS prefers pwsh.exe, but 5.1 is the host
// where a redirected native stderr line becomes an ErrorRecord that a Stop
// preference makes terminating, so these pin that the host never redirects it.
package main

import (
	"os/exec"
	"testing"
)

func newTestPersistentPS51(t *testing.T) *PersistentPowerShell {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping persistent PowerShell 5.1 integration test in -short mode")
	}
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		t.Skip("powershell.exe not on PATH")
	}
	ps, err := newPersistentPowerShellWithExe("powershell.exe")
	if err != nil {
		t.Fatalf("start persistent Windows PowerShell 5.1: %v", err)
	}
	t.Cleanup(ps.Close)
	return ps
}

func TestPersistentPS51_MissingProgramExitsOne(t *testing.T) {
	checkMissingProgramExitsOne(t, newTestPersistentPS51(t))
}

func TestPersistentPS51_NativeStderrInOutputOnFailure(t *testing.T) {
	checkNativeStderrInOutputOnFailure(t, newTestPersistentPS51(t))
}

func TestPersistentPS51_NativeStderrOnSuccessIsNotFailure(t *testing.T) {
	checkNativeStderrOnSuccessIsNotFailure(t, newTestPersistentPS51(t))
}

func TestPersistentPS51_GitCloneIntoNonEmptyDirKeepsExitCode(t *testing.T) {
	checkGitCloneIntoNonEmptyDirKeepsExitCode(t, newTestPersistentPS51(t))
}

func TestPersistentPS51_MissingProgramErrorOnce(t *testing.T) {
	checkMissingProgramErrorOnce(t, newTestPersistentPS51(t))
}

func TestPersistentPS51_OwnStopPreferenceRunsToCompletion(t *testing.T) {
	checkOwnStopPreferenceRunsToCompletion(t, newTestPersistentPS51(t))
}

func TestPersistentPS51_LeakedStopPreferenceDoesNotFailNativeStderr(t *testing.T) {
	checkLeakedStopPreferenceDoesNotFailNativeStderr(t, newTestPersistentPS51(t))
}
