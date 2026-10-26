//go:build windows

// File: powershell_windows_test.go
// Windows integration tests for the persistent PowerShell host and the
// execution-policy handling in powershell_launch.go: the host runs with a
// process-scoped Bypass, a program that never started (missing, or its .ps1
// blocked by policy) exits non-zero, handled errors are never promoted, and a
// bare name with a .cmd shim launches the shim instead of the .ps1.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
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
	out, _, err := ps.Execute(ctx, command, "")
	return out, err
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
// the reset value 0. Its error reaches the output through the host's stderr.
func TestPersistentPS_MissingProgramExitsOne(t *testing.T) {
	checkMissingProgramExitsOne(t, newTestPersistentPS(t))
}

func checkMissingProgramExitsOne(t *testing.T, ps *PersistentPowerShell) {
	t.Helper()
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

// The stderr cases below are helpers so powershell_ps51_windows_test.go runs
// the same bodies on Windows PowerShell 5.1, the host whose error handling
// differs most (stderr turned into ErrorRecords under redirection, verbose
// error formatting). Output checks are Contains: hosts decorate error text.

func TestPersistentPS_NativeStderrInOutputOnFailure(t *testing.T) {
	checkNativeStderrInOutputOnFailure(t, newTestPersistentPS(t))
}

func TestPersistentPS_NativeStderrOnSuccessIsNotFailure(t *testing.T) {
	checkNativeStderrOnSuccessIsNotFailure(t, newTestPersistentPS(t))
}

func TestPersistentPS_GitCloneIntoNonEmptyDirKeepsExitCode(t *testing.T) {
	checkGitCloneIntoNonEmptyDirKeepsExitCode(t, newTestPersistentPS(t))
}

func TestPersistentPS_MissingProgramErrorOnce(t *testing.T) {
	checkMissingProgramErrorOnce(t, newTestPersistentPS(t))
}

func TestPersistentPS_LeakedStopPreferenceDoesNotFailNativeStderr(t *testing.T) {
	checkLeakedStopPreferenceDoesNotFailNativeStderr(t, newTestPersistentPS(t))
}

func TestPersistentPS_OwnStopPreferenceRunsToCompletion(t *testing.T) {
	checkOwnStopPreferenceRunsToCompletion(t, newTestPersistentPS(t))
}

// A command (or a .ps1 it runs) that sets Stop itself and then runs a native
// program writing to stderr must run to completion with its own exit code. A
// `2>&1` inside PowerShell made that stderr line terminating in 5.1: the rest
// of the command was skipped and reported as success (or exit -1).
func checkOwnStopPreferenceRunsToCompletion(t *testing.T, ps *PersistentPowerShell) {
	t.Helper()
	const prefix = `$ErrorActionPreference = 'Stop'; `
	out, err := execPS(t, ps, prefix+`cmd /c "echo progress 1>&2 & exit 0"; Write-Output afterStop`)
	if err != nil {
		t.Fatalf("expected success, got %v (output %q)", err, out)
	}
	for _, want := range []string{"afterStop", "progress"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in the output, got %q", want, out)
		}
	}
	_, err = execPS(t, ps, prefix+`cmd /c "echo boom 1>&2 & exit 3"; Write-Output afterStop`)
	requireExitCode(t, err, 3)
	var exitErr *ExitCodeError
	errors.As(err, &exitErr)
	for _, want := range []string{"afterStop", "boom"} {
		if !strings.Contains(exitErr.Output, want) {
			t.Errorf("expected %q in the output, got %q", want, exitErr.Output)
		}
	}
}

// A native program's failure text goes to stderr; before drainStderr the step
// reported a bare "exit code 3".
func checkNativeStderrInOutputOnFailure(t *testing.T, ps *PersistentPowerShell) {
	t.Helper()
	_, err := execPS(t, ps, `cmd /c "echo boom 1>&2 & exit 3"`)
	requireExitCode(t, err, 3)
	var exitErr *ExitCodeError
	errors.As(err, &exitErr)
	if !strings.Contains(exitErr.Output, "boom") {
		t.Errorf("expected the program's stderr in the output, got %q", exitErr.Output)
	}
}

// Success stays on the native exit code: stderr text (git clone progress) at
// exit 0 is not a failure, even where it turns $? false.
func checkNativeStderrOnSuccessIsNotFailure(t *testing.T, ps *PersistentPowerShell) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	out, err := execPS(t, ps, `git --version; cmd /c "echo progress 1>&2"`)
	if err != nil {
		t.Fatalf("expected success, got %v (output %q)", err, out)
	}
	if !strings.Contains(out, "progress") {
		t.Errorf("expected stderr text in the output, got %q", out)
	}
}

// The env-setup clone skeleton into an existing, non-empty directory: git
// checks the destination before any transport, so no network is needed.
func checkGitCloneIntoNonEmptyDirKeepsExitCode(t *testing.T, ps *PersistentPowerShell) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dest := filepath.Join(dir, "dest")
	if out, err := exec.Command("git", "init", "-q", src).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := fmt.Sprintf("git -c protocol.ext.allow=never -c protocol.file.allow=user clone -- '%s' '%s'", src, dest)
	_, err := execPS(t, ps, cmd)
	requireExitCode(t, err, 128)
	var exitErr *ExitCodeError
	errors.As(err, &exitErr)
	if !strings.Contains(exitErr.Output, "already exists") {
		t.Errorf("expected git's fatal: ... already exists in the output, got %q", exitErr.Output)
	}
}

// The promoted error reaches the output once, through stderr: the old Write-Host
// echo of $Error[0] must not print it a second time.
func checkMissingProgramErrorOnce(t *testing.T, ps *PersistentPowerShell) {
	t.Helper()
	out, err := execPS(t, ps, "aix-definitely-missing-cmd")
	requireExitCode(t, err, 1)
	if n := strings.Count(out, "not recognized"); n != 1 {
		t.Errorf("expected \"not recognized\" exactly once, got %d in %q", n, out)
	}
	// pwsh 7.2+ colours error text even into a pipe unless the host renders
	// PlainText (psPlainTextRenderingScript).
	if strings.Contains(out, "\x1b[") {
		t.Errorf("expected plain text, got ANSI escapes in %q", out)
	}
}

// A Stop preference left in the host by an earlier command must not change
// how the next command runs, and its stderr must still reach the output.
func checkLeakedStopPreferenceDoesNotFailNativeStderr(t *testing.T, ps *PersistentPowerShell) {
	t.Helper()
	if out, err := execPS(t, ps, "$global:ErrorActionPreference = 'Stop'"); err != nil {
		t.Fatalf("set Stop: %v (output %q)", err, out)
	}
	// Under a terminating error the rest of the wrapper line is skipped, the
	// text goes to the unread stderr and the reset status reads as success, so
	// the output and the failing exit code are what tell the two apart.
	out, err := execPS(t, ps, `cmd /c "echo progress 1>&2 & exit 0"`)
	if err != nil {
		t.Fatalf("expected success under a leaked Stop preference, got %v (output %q)", err, out)
	}
	if !strings.Contains(out, "progress") {
		t.Errorf("expected stderr text in the output under a leaked Stop preference, got %q", out)
	}
	_, err = execPS(t, ps, `cmd /c "echo boom 1>&2 & exit 4"`)
	requireExitCode(t, err, 4)
	if !ps.IsHealthy() {
		t.Fatal("host became unhealthy")
	}
	if out, err := execPS(t, ps, "Write-Output ok"); err != nil || strings.TrimSpace(out) != "ok" {
		t.Fatalf("expected ok/exit 0, got %q, %v", out, err)
	}
}

// A script blocked by execution policy never starts. CI's default policy is
// permissive and a Group-Policy pin cannot be simulated, so a test-only
// `powershell.exe -ExecutionPolicy Restricted` host runs the same reset / status
// / exit-capture statements the persistent host wraps around a command.
//
// The refusal's message changes with the UI language and with the policy that
// blocked it (a MachinePolicy / UserPolicy pin outranks the process scope, and
// AllSigned reports "not digitally signed"), so the test checks the error id
// production promotes on, not the wording, via AIX_PS_EVIDENCE lines. When a
// pin allows local scripts there is no refusal to test and it skips.
// Import the Security module from $PSHOME to avoid inherited module paths;
// parse evidence only from stdout so stderr's source echoes cannot supply it.
func TestExitCapture_BlockedScriptExitsOne(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping powershell.exe integration test in -short mode")
	}
	dir := t.TempDir()
	redact := func(s string) string {
		return redactPSDiagnostics(s, dir, os.Getenv("USERPROFILE"), os.Getenv("USERNAME"))
	}
	blocked := filepath.Join(dir, "aixblocked.ps1")
	if err := os.WriteFile(blocked, []byte("Write-Output 'PS1 RAN'\nexit 9\n"), 0o644); err != nil {
		t.Fatal(redact(err.Error()))
	}

	// One field per line: -OutputFormat Text wraps long lines when stdout is
	// redirected. The policy probe comes first and reads the effective policy;
	// the rest run after psExitCaptureScript while $Error[0] is still the
	// command's error (Write-Output changes neither $LASTEXITCODE nor $Error).
	script := psSecurityModuleImport + "\n" +
		"Write-Output ('AIX_PS_EVIDENCE policy=' + (Get-ExecutionPolicy))\n" +
		psResetScript +
		psWrapUserCommand("& '"+blocked+"'") + "\n" +
		psExitCaptureScript + "\n" +
		"$__e = if ($Error.Count -gt 0) { $Error[0] } else { $null }\n" +
		"Write-Output ('AIX_PS_EVIDENCE id=' + $(if ($__e) { $__e.FullyQualifiedErrorId }))\n" +
		"Write-Output ('AIX_PS_EVIDENCE category=' + $(if ($__e) { $__e.CategoryInfo.Category }))\n" +
		"Write-Output ('AIX_PS_EVIDENCE type=' + $(if ($__e) { $__e.Exception.GetType().FullName }))\n" +
		"exit $__aix_exit"
	c := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Restricted",
		"-OutputFormat", "Text", "-EncodedCommand", encodeForPowerShell(script))
	var stdoutBuf, stderrBuf bytes.Buffer
	c.Stdout, c.Stderr = &stdoutBuf, &stderrBuf
	err := c.Run()
	stdout, stderr := stdoutBuf.String(), stderrBuf.String()

	ev, evErr := blockedScriptEvidence(stdout)
	if evErr != nil {
		t.Fatalf("%s\nstdout: %s\nstderr: %s", redact(evErr.Error()), redact(stdout), redact(stderr))
	}
	policy := ev["policy"]
	run, skip := blockedScriptPolicyAction(policy)
	if skip {
		t.Skip(redact(fmt.Sprintf("effective execution policy %q: a higher-precedence pin allows local scripts, "+
			"so the blocked-script scenario is not covered on this machine", policy)))
	}
	if !run {
		t.Fatalf("unrecognised effective execution policy %q\nstdout: %s\nstderr: %s", redact(policy), redact(stdout), redact(stderr))
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("expected exit 1 for a blocked script, got %s\nstdout: %s\nstderr: %s", redact(fmt.Sprint(err)), redact(stdout), redact(stderr))
	}
	if strings.Contains(stdout+stderr, "PS1 RAN") {
		t.Fatalf("blocked .ps1 must not run under %s\nstdout: %s\nstderr: %s", redact(policy), redact(stdout), redact(stderr))
	}
	for _, want := range []struct{ key, value string }{
		{"id", "UnauthorizedAccess"},
	} {
		if got := ev[want.key]; got != want.value {
			t.Errorf("evidence %s: want %q, got %q\nstdout: %s\nstderr: %s", want.key, want.value, redact(got), redact(stdout), redact(stderr))
		}
	}
}

// psEvidenceMarker prefixes each structured line
// TestExitCapture_BlockedScriptExitsOne prints from inside powershell.exe.
const psEvidenceMarker = "AIX_PS_EVIDENCE "

// blockedScriptEvidence parses stdout's `AIX_PS_EVIDENCE <key>=<value>` lines.
// The marker must start a line so an error record's echo of the script source
// cannot supply evidence. policy and id are required (blank values
// are allowed); a repeated key is an error.
func blockedScriptEvidence(out string) (map[string]string, error) {
	ev := map[string]string{}
	out = strings.TrimPrefix(out, "\ufeff")
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, psEvidenceMarker) {
			continue
		}
		key, value, ok := strings.Cut(line[len(psEvidenceMarker):], "=")
		if !ok {
			return nil, fmt.Errorf("malformed evidence line %q", line)
		}
		key = strings.TrimSpace(key)
		if _, dup := ev[key]; dup {
			return nil, fmt.Errorf("evidence key %q repeated", key)
		}
		ev[key] = strings.TrimSpace(value)
	}
	for _, key := range []string{"policy", "id"} {
		if _, ok := ev[key]; !ok {
			return nil, fmt.Errorf("evidence key %q missing", key)
		}
	}
	return ev, nil
}

// psPermissivePolicies let an unsigned local .ps1 run.
var psPermissivePolicies = []string{"RemoteSigned", "Unrestricted", "Bypass"}

// blockedScriptPolicyAction decides from the effective policy whether the
// blocked-script scenario runs or skips. policyBlocksLocalScripts is also false
// for blank and unknown values, so skip is only the explicit permissive set;
// neither run nor skip means the probe is broken and the test fails.
func blockedScriptPolicyAction(policy string) (run, skip bool) {
	if policyBlocksLocalScripts(policy) {
		return true, false
	}
	for _, p := range psPermissivePolicies {
		if strings.EqualFold(policy, p) {
			return false, true
		}
	}
	return false, false
}

// redactPSDiagnostics masks the temp dir, home folder, user name and
// credential-shaped text in output a failing test prints. The temp dir lives
// inside home, so it is replaced first. Empty values are skipped.
//
// powershell.exe -EncodedCommand writes stderr as CLIXML, where each line ends
// in `_x000D__x000A_`. `_` counts as a name rune, so a path or user name at the
// end of a line would escape the boundary check. CLIXML is also XML, so a user
// or folder name with `&` (allowed on Windows) arrives as `&amp;` and would not
// match. Both layers are decoded first, outer XML entities then `_xHHHH_`
// (filterCLIXML would drop the error text the failure message needs).
func redactPSDiagnostics(out, tempDir, home, user string) string {
	out = html.UnescapeString(out)
	out = psCLIXMLEscapePattern.ReplaceAllStringFunc(out, func(m string) string {
		r, _ := strconv.ParseUint(m[2:6], 16, 32)
		return string(rune(r))
	})
	out = redactPSWord(out, tempDir, "<tempdir>")
	out = redactPSWord(out, home, "<home>")
	out = redactPSWord(out, user, "<user>")
	return redactAgentSecrets(out)
}

// psCLIXMLEscapePattern matches CLIXML's `_xHHHH_` character escapes.
var psCLIXMLEscapePattern = regexp.MustCompile(`_x[0-9A-Fa-f]{4}_`)

// psPlaceholders are the words redactPSDiagnostics writes; a value that
// matches one of them in place (a user named "home") is left alone.
var psPlaceholders = []string{"<tempdir>", "<home>", "<user>"}

// redactPSWord replaces each case-insensitive match of value that no Unicode
// letter, digit or `_` touches (Go's \b is ASCII-only and would miss "José"),
// so home C:\Users\dk never turns C:\Users\dkupi into "<home>upi". The sides are
// checked rather than consumed, so two names sharing one delimiter are both
// caught in a single pass.
func redactPSWord(out, value, placeholder string) string {
	if value == "" {
		return out
	}
	var b strings.Builder
	last := 0
	for _, m := range regexp.MustCompile(`(?i)`+regexp.QuoteMeta(value)).FindAllStringIndex(out, -1) {
		before, _ := utf8.DecodeLastRuneInString(out[:m[0]])
		after, _ := utf8.DecodeRuneInString(out[m[1]:])
		if isPSNameRune(before) || isPSNameRune(after) || isPSPlaceholderAt(out, m[0], m[1]) {
			continue
		}
		b.WriteString(out[last:m[0]])
		b.WriteString(placeholder)
		last = m[1]
	}
	b.WriteString(out[last:])
	return b.String()
}

// isPSPlaceholderAt reports whether out[start:end] is the inside of one of
// psPlaceholders, e.g. "home" in "<home>".
func isPSPlaceholderAt(out string, start, end int) bool {
	if start == 0 || end >= len(out) {
		return false
	}
	for _, p := range psPlaceholders {
		if strings.EqualFold(out[start-1:end+1], p) {
			return true
		}
	}
	return false
}

// isPSNameRune reports whether r would join a user name into a longer word.
// utf8.RuneError (start or end of text) is not a name rune.
func isPSNameRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

func TestRedactPSDiagnostics(t *testing.T) {
	const (
		temp = `C:\Users\X\AppData\Local\Temp\TestExit123\001`
		home = `C:\Users\X`
	)
	cases := []struct {
		name, in, home, user string
		want, absent         []string
	}{
		{
			name: "temp dir in mixed case",
			in:   `at C:\USERS\X\AppData\Local\Temp\TestExit123\001\aixblocked.ps1:1`,
			home: home, user: "X",
			want:   []string{`<tempdir>\aixblocked.ps1`},
			absent: []string{`TestExit123`, `AppData`},
		},
		{
			name: "home outside temp dir",
			in:   `profile c:\users\x\Documents\WindowsPowerShell`,
			home: home, user: "X",
			want:   []string{`<home>\Documents`},
			absent: []string{`users\x`},
		},
		{
			name: "user as path segment",
			in:   `D:\Users\X\work`,
			home: home, user: "X",
			want:   []string{`D:\Users\<user>\work`},
			absent: []string{`\X\`},
		},
		{
			name:   "short user name is whole word only",
			in:     `owner dk ran dkupi-tools`,
			user:   "dk",
			want:   []string{`owner <user> ran`, `dkupi-tools`},
			absent: []string{` dk `},
		},
		{
			name: "unicode user name",
			in:   `C:\Users\José\x DOMAIN\José josé@host Josée ends José`,
			user: "José",
			want: []string{`C:\Users\<user>\x`, `DOMAIN\<user> `, ` <user>@host`, ` Josée `,
				`ends <user>`},
			absent: []string{`\José`, `josé@`, `ends José`},
		},
		{
			name: "two names sharing one delimiter",
			in:   `José José`,
			user: "José",
			want: []string{`<user> <user>`},
		},
		{
			name: "empty home and user",
			in:   `plain text C:\Users\X\y`,
			want: []string{`plain text C:\Users\X\y`},
		},
		{
			name: "home is not a prefix of a longer folder name",
			in:   `C:\Users\Xavier\y and C:\Users\X\z`,
			home: home, user: "X",
			want:   []string{`C:\Users\Xavier\y`, `<home>\z`},
			absent: []string{`<home>avier`},
		},
		{
			name: "CLIXML line-end escapes do not shield a path or name",
			in: `<S S="Error">at C:\Users\X\AppData\Local\Temp\TestExit123\001_x000D__x000A_</S>` +
				`<S S="Error">owner dk_x000D__x000A_</S>`,
			home: home, user: "dk",
			want:   []string{"at <tempdir>\r\n</S>", "owner <user>\r\n</S>"},
			absent: []string{`TestExit123`, `_x000D_`},
		},
		{
			name:   "XML-escaped ampersand in a user name and path",
			in:     `<S S="Error">C:\Users\A&amp;B\x and A&amp;B_x000D__x000A_</S>`,
			home:   `C:\Users\A&B`,
			user:   "A&B",
			want:   []string{`<home>\x and <user>`},
			absent: []string{`A&amp;B`, `A&B`},
		},
		{
			name:   "user name inside angle brackets is still redacted",
			in:     `owner <dk> and <user>`,
			user:   "dk",
			want:   []string{`owner <<user>> and <user>`},
			absent: []string{`<dk>`},
		},
		{
			name: "user named like a placeholder terminates and keeps placeholders",
			in:   `C:\Users\X\Documents user`,
			home: home, user: "user",
			want: []string{`<home>\Documents <user>`},
		},
		{
			name: "user named home keeps the home placeholder",
			in:   `C:\Users\X\a home`,
			home: home, user: "home",
			want: []string{`<home>\a <user>`},
		},
		{
			name:   "credentials",
			in:     "token=abcdefghijklmnopqrst\nAuthorization: Bearer abc.def.ghi",
			want:   []string{"token=[REDACTED]", "Authorization: Bearer [REDACTED]"},
			absent: []string{"abcdefghijklmnopqrst", "abc.def.ghi"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactPSDiagnostics(tc.in, temp, tc.home, tc.user)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("want %q in %q", w, got)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(got, a) {
					t.Errorf("%q must be redacted from %q", a, got)
				}
			}
		})
	}
}

func TestBlockedScriptPolicyAction(t *testing.T) {
	cases := []struct {
		policy    string
		run, skip bool
	}{
		{"Restricted", true, false},
		{"restricted", true, false},
		{"AllSigned", true, false},
		{"ALLSIGNED", true, false},
		{"RemoteSigned", false, true},
		{"remotesigned", false, true},
		{"Unrestricted", false, true},
		{"UNRESTRICTED", false, true},
		{"Bypass", false, true},
		{"bypass", false, true},
		{"", false, false},
		{"   ", false, false},
		{"Undefinedx", false, false},
		{"SomeFuturePolicy", false, false},
		// Exact malformed policy observed in CI when CLIXML echoed the probe source.
		{"' + (Get-ExecutionPolicy))\r\n</S><S S=\"Error\">+ ...", false, false},
	}
	for _, tc := range cases {
		run, skip := blockedScriptPolicyAction(tc.policy)
		if run != tc.run || skip != tc.skip {
			t.Errorf("%q: got run=%v skip=%v, want run=%v skip=%v", tc.policy, run, skip, tc.run, tc.skip)
		}
	}
}

func TestBlockedScriptEvidence(t *testing.T) {
	t.Run("own lines with CRLF", func(t *testing.T) {
		out := "AIX_PS_EVIDENCE policy=Restricted\r\nnoise\r\n" +
			"AIX_PS_EVIDENCE id=UnauthorizedAccess\r\nAIX_PS_EVIDENCE category=SecurityError\r\n" +
			"AIX_PS_EVIDENCE type=System.Management.Automation.PSSecurityException\r\n"
		ev, err := blockedScriptEvidence(out)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			"policy": "Restricted", "id": "UnauthorizedAccess",
			"category": "SecurityError", "type": "System.Management.Automation.PSSecurityException",
		}
		for k, v := range want {
			if ev[k] != v {
				t.Errorf("%s: got %q, want %q", k, ev[k], v)
			}
		}
	})
	t.Run("marker inside other text is not evidence", func(t *testing.T) {
		out := "#< CLIXML\n<Objs Version=\"1.1.0.1\"></Objs>AIX_PS_EVIDENCE policy=Bypass\n" +
			"AIX_PS_EVIDENCE policy=AllSigned\n" +
			"AIX_PS_EVIDENCE id=UnauthorizedAccess\n"
		ev, err := blockedScriptEvidence(out)
		if err != nil {
			t.Fatal(err)
		}
		if ev["policy"] != "AllSigned" || ev["id"] != "UnauthorizedAccess" {
			t.Errorf("got %v", ev)
		}
	})
	t.Run("CI regression: autoload failure echo", func(t *testing.T) {
		out := "AIX_PS_EVIDENCE id=UnauthorizedAccess\n" +
			"AIX_PS_EVIDENCE category=SecurityError\n" +
			"AIX_PS_EVIDENCE type=System.Management.Automation.PSSecurityException\n" +
			`<S S="Error">+ Write-Output ('AIX_PS_EVIDENCE policy=' + (Get-ExecutionPolicy))_x000D__x000A_</S>`
		ev, err := blockedScriptEvidence(out)
		if err == nil || !strings.Contains(err.Error(), `evidence key "policy" missing`) {
			t.Fatalf("expected missing policy error, got %v (evidence %v)", err, ev)
		}
		if ev != nil {
			t.Errorf("broken probe must not return evidence, got %v", ev)
		}
	})
	t.Run("BOM before first marker", func(t *testing.T) {
		ev, err := blockedScriptEvidence("\ufeffAIX_PS_EVIDENCE policy=Restricted\n" +
			"AIX_PS_EVIDENCE id=UnauthorizedAccess\n")
		if err != nil {
			t.Fatal(err)
		}
		if ev["policy"] != "Restricted" {
			t.Errorf("policy: got %q, want Restricted", ev["policy"])
		}
	})
	t.Run("blank values", func(t *testing.T) {
		ev, err := blockedScriptEvidence("AIX_PS_EVIDENCE policy=Restricted\n" +
			"AIX_PS_EVIDENCE id=\nAIX_PS_EVIDENCE type=\n")
		if err != nil {
			t.Fatal(err)
		}
		if v, ok := ev["id"]; !ok || v != "" {
			t.Errorf("id: got %q (present %v), want blank", v, ok)
		}
	})
	t.Run("missing id", func(t *testing.T) {
		if _, err := blockedScriptEvidence("AIX_PS_EVIDENCE policy=Restricted\n"); err == nil {
			t.Error("expected an error for a missing id")
		}
	})
	t.Run("duplicated key", func(t *testing.T) {
		out := "AIX_PS_EVIDENCE policy=Restricted\nAIX_PS_EVIDENCE policy=Bypass\n" +
			"AIX_PS_EVIDENCE id=UnauthorizedAccess\n"
		if _, err := blockedScriptEvidence(out); err == nil {
			t.Error("expected an error for a repeated key")
		}
	})
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

	out, err := runLocalCommandWindows("aixshim", []string{"--version"}, "", 30*time.Second, cwdScope{})
	if err != nil {
		t.Fatalf("expected exit 0 from the .cmd shim, got %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "CMD") || strings.Contains(out, "PS1") {
		t.Fatalf("expected the .cmd shim to run (not the .ps1); output: %q", out)
	}
}
