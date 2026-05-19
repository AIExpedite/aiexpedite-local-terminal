//go:build windows

package main

import (
	"strings"
	"testing"
	"time"
)

// withSpiedTransports swaps the two PowerShell transport vars used by
// runEncodedPowerShellCommand for stubs, records which one was called, and
// restores the originals when the returned cleanup runs.
func withSpiedTransports(t *testing.T) (called *struct{ arg, tempFile bool }, lastScript *string, cleanup func()) {
	t.Helper()
	flags := &struct{ arg, tempFile bool }{}
	captured := new(string)

	origArg := runEncodedPowerShellViaArgFn
	origTempFile := runPowerShellCommandViaTempFileFn

	runEncodedPowerShellViaArgFn = func(encoded string, workDir string, timeout time.Duration) (string, error) {
		flags.arg = true
		*captured = encoded
		return "arg-stub", nil
	}
	runPowerShellCommandViaTempFileFn = func(script string, workDir string, timeout time.Duration) (string, error) {
		flags.tempFile = true
		*captured = script
		return "tempfile-stub", nil
	}

	return flags, captured, func() {
		runEncodedPowerShellViaArgFn = origArg
		runPowerShellCommandViaTempFileFn = origTempFile
	}
}

// TestSmallEncodedScriptUsesArgPath verifies that scripts whose encoded form
// fits under the threshold go through the -EncodedCommand argument path.
func TestSmallEncodedScriptUsesArgPath(t *testing.T) {
	flags, _, restore := withSpiedTransports(t)
	defer restore()

	encoded := encodeForPowerShell("Write-Host hello")
	if len(encoded) > encodedCommandFallbackThreshold {
		t.Fatalf("test setup invariant violated: short script encoded to %d chars", len(encoded))
	}

	out, err := runEncodedPowerShellCommand(encoded, "", 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "arg-stub" {
		t.Fatalf("expected arg-stub output, got %q", out)
	}
	if !flags.arg {
		t.Error("expected arg path to be invoked")
	}
	if flags.tempFile {
		t.Error("temp-file path should not have been invoked for small script")
	}
}

// TestLargeEncodedScriptUsesTempFilePath verifies that when the encoded form
// exceeds the threshold, runEncodedPowerShellCommand decodes the script and
// routes it through the temp-file helper.
func TestLargeEncodedScriptUsesTempFilePath(t *testing.T) {
	flags, captured, restore := withSpiedTransports(t)
	defer restore()

	// Encoded form is ~2.67x raw (UTF-16LE + base64). 15000 raw bytes lands
	// well above the 30000-char encoded threshold.
	raw := "Write-Output 'sentinel-large'\n" + strings.Repeat("# pad\n", 2500)
	encoded := encodeForPowerShell(raw)
	if len(encoded) <= encodedCommandFallbackThreshold {
		t.Fatalf("test setup invariant violated: large script encoded to only %d chars", len(encoded))
	}

	out, err := runEncodedPowerShellCommand(encoded, "", 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "tempfile-stub" {
		t.Fatalf("expected tempfile-stub output, got %q", out)
	}
	if !flags.tempFile {
		t.Error("expected temp-file path to be invoked")
	}
	if flags.arg {
		t.Error("arg path should not have been invoked for large script")
	}
	// The temp-file helper receives the *decoded* script, not the encoded blob.
	if !strings.Contains(*captured, "sentinel-large") {
		t.Errorf("temp-file helper received script that doesn't contain the original sentinel; first 80 chars: %q", truncate(*captured, 80))
	}
}

// TestTempFileExecutesLargeScript exercises the real temp-file helper
// end-to-end against the local powershell.exe to confirm that scripts past
// the -EncodedCommand cmdline limit actually run and return their output.
func TestTempFileExecutesLargeScript(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping powershell.exe integration test in -short mode")
	}

	sentinel := "TEMPFILE-OK-9F2A"
	// ~18KB raw → ~48KB base64-encoded UTF-16LE, well over the threshold and
	// over the historical CreateProcess failure point.
	padding := strings.Repeat("# noise line\n", 3000)
	script := "Write-Output '" + sentinel + "'\n" + padding
	encoded := encodeForPowerShell(script)
	if len(encoded) <= encodedCommandFallbackThreshold {
		t.Fatalf("test setup invariant violated: encoded length %d does not exceed threshold", len(encoded))
	}

	out, err := runEncodedPowerShellCommand(encoded, "", 30*time.Second)
	if err != nil {
		t.Fatalf("expected temp-file large script to succeed, got error: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, sentinel) {
		t.Errorf("expected sentinel %q in output, got: %s", sentinel, out)
	}
}

// TestInvalidBase64AboveThresholdReturnsError verifies that a malformed
// encoded payload large enough to trip the temp-file fallback surfaces a real
// error instead of being silently forwarded to PowerShell.
func TestInvalidBase64AboveThresholdReturnsError(t *testing.T) {
	// '!' is not a valid base64 character, so any size of this string fails
	// the decoder. Sized above the threshold so the fallback path is taken.
	bad := strings.Repeat("!", encodedCommandFallbackThreshold+10)

	out, err := runEncodedPowerShellCommand(bad, "", 5*time.Second)
	if err == nil {
		t.Fatalf("expected decode error to surface, got nil (output: %q)", out)
	}
	if strings.Contains(out, "[decode error]") || strings.Contains(out, "[invalid encoding]") {
		t.Errorf("expected real error, got legacy sentinel string in output: %q", out)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
