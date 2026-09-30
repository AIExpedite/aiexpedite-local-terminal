//go:build windows

// File: powershell_windows_test.go
// Windows integration tests for the persistent PowerShell host and the
// execution-policy handling in powershell_launch.go: the host runs with a
// process-scoped Bypass, a program that never started (missing, or its .ps1
// blocked by policy) exits non-zero, handled errors are never promoted, and a
// bare name with a .cmd shim launches the shim instead of the .ps1.
package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestPersistentPS starts a dedicated persistent host (not the global one)
// and closes it when the test ends.
func newTestPersistentPS(t *testing.T) *PersistentPowerShell {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping persistent PowerShell integration test in -short mode")
	}
	ps, err := NewPersistentPowerShell()
	if err != nil {
		t.Fatalf("start persistent PowerShell: %v", err)
	}
	t.Cleanup(ps.Close)
	return ps
}

func execPS(t *testing.T, ps *PersistentPowerShell, command string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return ps.Execute(ctx, command, "")
}

func requireExitCode(t *testing.T, err error, want int) {
	t.Helper()
	var exitErr *ExitCodeError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected *ExitCodeError with code %d, got %T: %v", want, err, err)
	}
	if exitErr.Code != want {
		t.Fatalf("exit code = %d, want %d", exitErr.Code, want)
	}
}

func TestPersistentPS_ProcessScopeBypass(t *testing.T) {
	ps := newTestPersistentPS(t)
	out, err := execPS(t, ps, "Get-ExecutionPolicy -Scope Process")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.TrimSpace(out); got != "Bypass" {
		t.Fatalf("expected Bypass, got %q", got)
	}
}

// Before the fix a missing program reported success: $LASTEXITCODE stayed at
// the reset value 0. Its error is echoed to stdout because this host's stderr
// is not part of the output.
func TestPersistentPS_MissingProgramExitsOne(t *testing.T) {
	ps := newTestPersistentPS(t)
	out, err := execPS(t, ps, "aix-definitely-missing-cmd --version")
	requireExitCode(t, err, 1)
	if !strings.Contains(out, "not recognized") {
		t.Errorf("expected PowerShell's \"not recognized\" error in the output, got: %q", out)
	}
}

func TestPersistentPS_NativeExitCodeNotOverridden(t *testing.T) {
	ps := newTestPersistentPS(t)
	_, err := execPS(t, ps, "cmd /c exit 7")
	requireExitCode(t, err, 7)
}

// $Error.Clear() before each command: a leftover CommandNotFoundException from
// the previous command must not fail the next one, even when that next one's
// final statement fails without recording an error of its own.
func TestPersistentPS_LeftoverErrorDoesNotFailNextCommand(t *testing.T) {
	ps := newTestPersistentPS(t)
	_, err := execPS(t, ps, "aix-definitely-missing-cmd")
	requireExitCode(t, err, 1)

	if out, err := execPS(t, ps, "Get-Command aix-definitely-missing-cmd -ErrorAction Ignore"); err != nil {
		t.Fatalf("expected exit 0 with no error of its own, got %v (output %q)", err, out)
	}
	if out, err := execPS(t, ps, "Write-Output ok"); err != nil || strings.TrimSpace(out) != "ok" {
		t.Fatalf("expected ok/exit 0, got %q, %v", out, err)
	}
}

func TestPersistentPS_HandledErrorsNotPromoted(t *testing.T) {
	ps := newTestPersistentPS(t)
	commands := []string{
		"try { aix-definitely-missing-cmd } catch { 'handled' }",
		"$c = Get-Command aix-definitely-missing-cmd -ErrorAction SilentlyContinue; if (-not $c) { 'absent' }",
		// Final statement fails ($? false) but the id carries the cmdlet suffix.
		"Get-Command aix-definitely-missing-cmd -ErrorAction SilentlyContinue",
		// An earlier failure followed by a successful final statement.
		"aix-definitely-missing-cmd; Write-Output after",
		// A command that leaves the block early never reaches the status capture.
		"aix-definitely-missing-cmd; return",
	}
	for _, c := range commands {
		if out, err := execPS(t, ps, c); err != nil {
			t.Errorf("%q: expected exit 0, got %v (output %q)", c, err, out)
		}
	}
}

// A script blocked by execution policy never starts. CI's default policy is
// permissive and a Group-Policy pin cannot be simulated, so a test-only
// `powershell.exe -ExecutionPolicy Restricted` host runs the same reset / status
// / exit-capture statements the persistent host wraps around a command.
func TestExitCapture_BlockedScriptExitsOne(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping powershell.exe integration test in -short mode")
	}
	dir := t.TempDir()
	blocked := filepath.Join(dir, "aixblocked.ps1")
	if err := os.WriteFile(blocked, []byte("Write-Output 'PS1 RAN'\nexit 9\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	script := psResetScript +
		"& { & '" + blocked + "'; " + psStatusCaptureStatement + " }\n" +
		psExitCaptureScript + "\n" +
		"exit $__aix_exit"
	c := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Restricted",
		"-OutputFormat", "Text", "-EncodedCommand", encodeForPowerShell(script))
	out, err := c.CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("expected exit 1 for a blocked script, got %v\noutput: %s", err, out)
	}
	if strings.Contains(string(out), "PS1 RAN") {
		t.Fatalf("blocked .ps1 must not run under Restricted; output: %s", out)
	}
	if !strings.Contains(string(out), "running scripts is disabled") {
		t.Errorf("expected the execution-policy error in the output, got: %s", out)
	}
}

// End to end on the real Windows path: a directory first on PATH (picked up by
// the persistent host's PATH sync) holds aixshim.ps1 and aixshim.cmd. The bare
// name must launch the .cmd shim; PowerShell alone would prefer the .ps1.
func TestRunLocalCommandWindows_LaunchesCmdShimNotPs1(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping PowerShell integration test in -short mode")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "aixshim.ps1"), []byte("Write-Output 'PS1'\nexit 9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aixshim.cmd"), []byte("@echo CMD\r\n@exit /b 0\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// runLocalCommandWindows starts the global persistent host; don't leave it
	// (and its synced test PATH) running for later tests.
	t.Cleanup(ShutdownPowerShell)

	if got := preferBatchShim("aixshim", []string{"--version"}); got != "aixshim.cmd" {
		t.Fatalf("preferBatchShim = %q, want aixshim.cmd", got)
	}

	out, err := runLocalCommandWindows("aixshim", []string{"--version"}, "", 30*time.Second)
	if err != nil {
		t.Fatalf("expected exit 0 from the .cmd shim, got %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "CMD") || strings.Contains(out, "PS1") {
		t.Fatalf("expected the .cmd shim to run (not the .ps1); output: %q", out)
	}
}
