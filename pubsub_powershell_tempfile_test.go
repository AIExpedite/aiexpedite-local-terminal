//go:build windows

package main

import (
	"os"
	"path/filepath"
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

// TestTempFileLifecycle verifies that the temp .ps1 file is created during
// the fallback call, the script can observe itself on disk, and the file is
// removed after the call returns.
func TestTempFileLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping powershell.exe integration test in -short mode")
	}

	tempDir := os.TempDir()
	beforePaths := listAixPSScripts(t, tempDir)

	// The script captures its own path via $MyInvocation.MyCommand.Path —
	// this is what we'll verify is gone after the call returns. Padded to
	// push the encoded form past the threshold.
	script := `$path = $MyInvocation.MyCommand.Path
Write-Output "TEMPPATH:$path"
Write-Output "EXISTS-DURING:$(Test-Path -LiteralPath $path)"
` + strings.Repeat("# pad line\n", 3000)

	encoded := encodeForPowerShell(script)
	if len(encoded) <= encodedCommandFallbackThreshold {
		t.Fatalf("test setup invariant violated: encoded length %d does not exceed threshold", len(encoded))
	}

	out, err := runEncodedPowerShellCommand(encoded, "", 30*time.Second)
	if err != nil {
		t.Fatalf("expected success, got error: %v\noutput: %s", err, out)
	}

	// Extract the temp file path the script saw on disk.
	var capturedPath string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "TEMPPATH:") {
			capturedPath = strings.TrimPrefix(line, "TEMPPATH:")
			break
		}
	}
	if capturedPath == "" {
		t.Fatalf("could not extract TEMPPATH line from output: %s", out)
	}
	if !strings.Contains(out, "EXISTS-DURING:True") {
		t.Errorf("script did not observe its own .ps1 on disk during execution; output: %s", out)
	}

	// Sanity: the captured path should look like one of our temp files.
	base := filepath.Base(capturedPath)
	if !strings.HasPrefix(base, "aiexpedite-ps-") || !strings.HasSuffix(base, ".ps1") {
		t.Errorf("unexpected temp filename shape: %s", base)
	}

	// After the call returns, the deferred os.Remove should have wiped it.
	if _, statErr := os.Stat(capturedPath); !os.IsNotExist(statErr) {
		t.Errorf("temp file %s still exists after runEncodedPowerShellCommand returned (stat err: %v)", capturedPath, statErr)
	}

	// Net file count for our prefix should be unchanged from before the call.
	afterPaths := listAixPSScripts(t, tempDir)
	if len(afterPaths) != len(beforePaths) {
		t.Errorf("aiexpedite-ps-*.ps1 file count drifted: before=%d after=%d", len(beforePaths), len(afterPaths))
	}
}

// TestNonASCIIRoundTripsThroughTempFile guards the UTF-8 BOM: without it,
// Windows PowerShell 5.x reads .ps1 files as Windows-1252 and mangles
// non-ASCII characters. The script outputs a mix of Latin-1 and CJK
// codepoints and we assert they round-trip intact.
func TestNonASCIIRoundTripsThroughTempFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping powershell.exe integration test in -short mode")
	}

	// Force UTF-8 stdout on PowerShell so the chars don't get re-mangled
	// on the way out. The padding pushes encoded length past the threshold.
	script := `[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
Write-Output 'cafe-cafe-café'
Write-Output 'cjk-中文-cjk'
` + strings.Repeat("# pad line\n", 3000)

	encoded := encodeForPowerShell(script)
	if len(encoded) <= encodedCommandFallbackThreshold {
		t.Fatalf("test setup invariant violated: encoded length %d does not exceed threshold", len(encoded))
	}

	out, err := runEncodedPowerShellCommand(encoded, "", 30*time.Second)
	if err != nil {
		t.Fatalf("expected success, got error: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "café") {
		t.Errorf("expected 'café' in output (UTF-8 BOM regression?); output: %s", out)
	}
	if !strings.Contains(out, "中文") {
		t.Errorf("expected '中文' in output (UTF-8 BOM regression?); output: %s", out)
	}
}

// TestNoStdinPipeInTempFileTransport is the load-bearing regression test
// that prevents anyone from "optimizing away" the temp file and switching
// back to `powershell.exe -Command -` (which would pipe the script source
// as the child's stdin and let inner native tools consume PowerShell
// source bytes). The check is structural — it greps pubsub.go — so it
// runs in every environment, including TTY-attached test runs where
// runtime stdin probing is unreliable.
//
// If this test ever fails, do NOT relax it. The temp-file transport
// exists specifically to keep the child's stdin default/empty.
func TestNoStdinPipeInTempFileTransport(t *testing.T) {
	src, err := os.ReadFile("pubsub.go")
	if err != nil {
		t.Fatalf("read pubsub.go: %v", err)
	}

	// Find the body of runPowerShellCommandViaTempFile.
	start := strings.Index(string(src), "func runPowerShellCommandViaTempFile(")
	if start < 0 {
		t.Fatal("could not locate runPowerShellCommandViaTempFile in pubsub.go — refactor without updating this regression test?")
	}
	rest := string(src[start:])
	// Crudely bound the function body by finding the next top-level func.
	end := strings.Index(rest[1:], "\nfunc ")
	if end < 0 {
		end = len(rest)
	}
	body := rest[:end]

	// These patterns would indicate stdin is being routed to the child —
	// either via Go's pipe API or via `-Command -` on the cmdline. Any of
	// them means the fallback has regressed to the unsafe transport.
	forbidden := []string{
		"StdinPipe",       // c.StdinPipe()
		"c.Stdin =",       // any direct Stdin assignment to a non-nil source
		"cmd.Stdin =",     // same, alternate naming
		`"-Command"`,      // -Command argument (would be paired with `-` or pipe)
		`"-Command", "-"`, // exact `-Command -` form
		`"-Command","-"`,  // formatter quirk
	}
	for _, pat := range forbidden {
		if strings.Contains(body, pat) {
			t.Fatalf("runPowerShellCommandViaTempFile contains forbidden pattern %q — the temp-file fallback MUST NOT route script source through stdin (`-Command -` or StdinPipe). If you intentionally changed the transport, also update the rationale comment in the function and read pubsub_powershell_tempfile_test.go before adjusting this assertion.", pat)
		}
	}

	// Positive check: confirm `-File` is still the chosen transport.
	if !strings.Contains(body, `"-File"`) {
		t.Error("runPowerShellCommandViaTempFile no longer uses `-File` — what's the new transport?")
	}
}

// TestStdinIsEmptyOnTempFileTransport probes at runtime that the child
// PowerShell does not see script source on its stdin. It's a belt-and-
// suspenders companion to TestNoStdinPipeInTempFileTransport: that test
// catches the regression at the source level, this one catches it at
// process-spawn level.
//
// Skipped when the test process has a TTY stdin, because in that case
// the child PowerShell inherits the terminal and [Console]::In.ReadLine
// would block indefinitely. The structural test above still guards
// against the same regression in TTY-attached runs.
func TestStdinIsEmptyOnTempFileTransport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping powershell.exe integration test in -short mode")
	}
	stat, err := os.Stdin.Stat()
	if err == nil && (stat.Mode()&os.ModeCharDevice) != 0 {
		t.Skip("test requires non-TTY stdin (CI runs this; interactive `go test` skips). The structural regression test still guards this invariant in TTY contexts.")
	}

	// If anyone re-routes us through `-Command -`, ReadLine will consume
	// the next line of script source. We'd see "got:# pad line",
	// "got:Write-Output", etc. With the -File path, stdin is the NUL
	// device Go opens for nil Stdin, and ReadLine returns null/empty.
	script := `$line = [Console]::In.ReadLine()
if ($null -eq $line -or $line.Length -eq 0) {
    Write-Output 'got:EMPTY'
} else {
    Write-Output ('got:' + $line)
}
` + strings.Repeat("# pad line\n", 3000)

	encoded := encodeForPowerShell(script)
	if len(encoded) <= encodedCommandFallbackThreshold {
		t.Fatalf("test setup invariant violated: encoded length %d does not exceed threshold", len(encoded))
	}

	out, runErr := runEncodedPowerShellCommand(encoded, "", 15*time.Second)
	if runErr != nil {
		t.Fatalf("expected success, got error: %v\noutput: %s", runErr, out)
	}

	if !strings.Contains(out, "got:EMPTY") {
		t.Errorf("expected 'got:EMPTY' (stdin should be the NUL device); output: %s", out)
	}

	// Trip wires: these strings would only appear in `got:` output if the
	// child's stdin had been fed the script source itself.
	leakSentinels := []string{
		"got:# pad",
		"got:Write-Output",
		"got:[Console]",
		"got:$line",
		"got:if (",
	}
	for _, sentinel := range leakSentinels {
		if strings.Contains(out, sentinel) {
			t.Fatalf("stdin appears to have been fed PowerShell source — sentinel %q found in output. The fallback transport must keep stdin default/empty. Output:\n%s", sentinel, out)
		}
	}
}

// listAixPSScripts returns the absolute paths of any aiexpedite-ps-*.ps1
// files currently present in dir. Used by lifecycle assertions.
func listAixPSScripts(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "aiexpedite-ps-*.ps1"))
	if err != nil {
		t.Fatalf("glob temp dir %s: %v", dir, err)
	}
	return matches
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
