//go:build windows

// File: pubsub_fallback_windows_test.go
// Windows integration tests: a failing native command — or a program that never
// started — routed through the one-shot fallback PowerShell must surface a
// non-zero exit (an *exec.ExitError), not be masked as success by the trailing
// cwd probe; the host runs with a process-scoped execution-policy bypass. This is the path detected test
// runners (npm test / pytest) take on Windows — see runLocalCommand's
// test-runner routing and buildFallbackProbeCommand.
package main

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestRunLocalCommandFallback_PropagatesNativeFailure(t *testing.T) {
	// `cmd /c exit 3` is a native process that exits non-zero, standing in for a
	// failing `npm test`/`pytest`. The trailing Write-Host/(Get-Location).Path
	// probe succeeds, so without the $LASTEXITCODE capture this would report
	// success.
	_, err := runLocalCommandFallback("cmd /c exit 3", "", 30*time.Second)
	if err == nil {
		t.Fatalf("expected a non-zero exit error from a failing native command, got nil")
	}
	if _, ok := err.(*exec.ExitError); !ok {
		t.Fatalf("expected *exec.ExitError, got %T: %v", err, err)
	}
}

func TestRunLocalCommandFallback_SucceedsOnPassingCommand(t *testing.T) {
	// A passing native command must still report success (no false failure from
	// the $null-guard or the probe).
	_, err := runLocalCommandFallback("cmd /c exit 0", "", 30*time.Second)
	if err != nil {
		t.Fatalf("expected success for a passing command, got %v", err)
	}
}

// A program that is not on PATH never starts, so $LASTEXITCODE stays 0. The
// fallback must still fail — this is how a missing `npm` surfaces during
// computer setup instead of a masked success — and keep PowerShell's own
// "not recognized" text as the output.
func TestRunLocalCommandFallback_MissingProgramFails(t *testing.T) {
	out, err := runLocalCommandFallback("aix-definitely-missing-cmd --version", "", 30*time.Second)
	if err == nil {
		t.Fatalf("expected a non-zero exit for a missing program, got nil; output: %s", out)
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() == 0 {
		t.Fatalf("expected *exec.ExitError with a non-zero code, got %T: %v", err, err)
	}
	if !strings.Contains(out, "not recognized") {
		t.Errorf("expected PowerShell's \"not recognized\" error in the output, got: %s", out)
	}
}

// Every fallback host runs with a process-scoped `-ExecutionPolicy Bypass` so
// npm.ps1 and other .ps1 shims load under a restrictive machine policy.
func TestRunLocalCommandFallback_ProcessScopeBypass(t *testing.T) {
	out, err := runLocalCommandFallback("Get-ExecutionPolicy -Scope Process", "", 30*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput: %s", err, out)
	}
	if got := strings.TrimSpace(out); got != "Bypass" {
		t.Fatalf("expected Bypass, got %q", got)
	}
}

// A handled error is never promoted to a failure.
func TestRunLocalCommandFallback_HandledMissingProgramSucceeds(t *testing.T) {
	out, err := runLocalCommandFallback("try { aix-definitely-missing-cmd } catch { 'handled' }", "", 30*time.Second)
	if err != nil {
		t.Fatalf("expected a handled error to exit 0, got %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "handled") {
		t.Errorf("expected the catch block output, got: %s", out)
	}
}
