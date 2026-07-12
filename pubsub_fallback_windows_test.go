//go:build windows

// File: pubsub_fallback_windows_test.go
// Windows integration test: a failing native command routed through the one-shot
// fallback PowerShell must surface a non-zero exit (an *exec.ExitError), not be
// masked as success by the trailing cwd probe. This is the path detected test
// runners (npm test / pytest) take on Windows — see runLocalCommand's
// test-runner routing and buildFallbackProbeCommand.
package main

import (
	"os/exec"
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
