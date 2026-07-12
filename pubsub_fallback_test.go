// File: pubsub_fallback_test.go
// Cross-platform structural tests for buildFallbackProbeCommand: the one-shot
// fallback PowerShell must preserve the user command's native exit code so a
// failing test runner (npm test / pytest) is not masked as success by the
// trailing cwd probe. See runLocalCommandFallback.
package main

import (
	"strings"
	"testing"
)

func TestBuildFallbackProbeCommand_PreservesExitCodeOrdering(t *testing.T) {
	const sentinel = "<<<AIX_CWD_PROBE>>>"
	userCmd := "npm test"
	got := buildFallbackProbeCommand(userCmd, sentinel)

	idxReset := strings.Index(got, "$LASTEXITCODE = 0")
	idxUser := strings.Index(got, userCmd)
	idxCapture := strings.Index(got, "$__aix_exit = $LASTEXITCODE")
	idxSentinel := strings.Index(got, sentinel)
	idxGetLoc := strings.Index(got, "(Get-Location).Path")

	// $LASTEXITCODE must be reset to 0 before the user command so a cmdlet-only
	// command can't inherit a stale non-zero value from a fresh pwsh.exe.
	if idxReset != 0 {
		t.Fatalf("LASTEXITCODE reset must come first; got %q", got)
	}
	// The exit code MUST be captured after the user command but BEFORE any of
	// the trailing probe cmdlets (which succeed and would reset the status).
	if !(idxReset < idxUser && idxUser < idxCapture && idxCapture < idxSentinel && idxCapture < idxGetLoc) {
		t.Fatalf("exit-code capture must sit between the reset, the user command, and the cwd probe; got %q", got)
	}
	// And it must be re-raised as the process exit code.
	if !strings.Contains(got, "exit $__aix_exit") {
		t.Errorf("probe must re-exit with the captured code; got %q", got)
	}
	// $null (no native command ran) is treated as success rather than crashing
	// `exit $null` semantics.
	if !strings.Contains(got, "if ($null -eq $__aix_exit)") {
		t.Errorf("probe must guard the $null (no native command) case; got %q", got)
	}
}
