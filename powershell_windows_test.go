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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
//
// The refusal's message changes with the UI language and with the policy that
// blocked it (a MachinePolicy / UserPolicy pin outranks the process scope, and
// AllSigned reports "not digitally signed"), so the test checks the error id
// production promotes on, not the wording, via AIX_PS_EVIDENCE lines. When a
// pin allows local scripts there is no refusal to test and it skips.
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
	// redirected. The policy line comes first and reads the effective policy;
	// the rest run after psExitCaptureScript while $Error[0] is still the
	// command's error (Write-Output changes neither $LASTEXITCODE nor $Error).
	script := "Write-Output ('AIX_PS_EVIDENCE policy=' + (Get-ExecutionPolicy))\n" +
		psResetScript +
		"& { & '" + blocked + "'; " + psStatusCaptureStatement + " }\n" +
		psExitCaptureScript + "\n" +
		"$__e = if ($Error.Count -gt 0) { $Error[0] } else { $null }\n" +
		"Write-Output ('AIX_PS_EVIDENCE promoted=' + $__aix_promoted)\n" +
		"Write-Output ('AIX_PS_EVIDENCE id=' + $(if ($__e) { $__e.FullyQualifiedErrorId }))\n" +
		"Write-Output ('AIX_PS_EVIDENCE category=' + $(if ($__e) { $__e.CategoryInfo.Category }))\n" +
		"Write-Output ('AIX_PS_EVIDENCE type=' + $(if ($__e) { $__e.Exception.GetType().FullName }))\n" +
		"exit $__aix_exit"
	c := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Restricted",
		"-OutputFormat", "Text", "-EncodedCommand", encodeForPowerShell(script))
	raw, err := c.CombinedOutput()
	out := string(raw)

	ev, evErr := blockedScriptEvidence(out)
	if evErr != nil {
		t.Fatalf("%s\noutput: %s", redact(evErr.Error()), redact(out))
	}
	policy := ev["policy"]
	run, skip := blockedScriptPolicyAction(policy)
	if skip {
		t.Skip(redact(fmt.Sprintf("effective execution policy %q: a higher-precedence pin allows local scripts, "+
			"so the blocked-script scenario is not covered on this machine", policy)))
	}
	if !run {
		t.Fatalf("unrecognised effective execution policy %q\noutput: %s", redact(policy), redact(out))
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("expected exit 1 for a blocked script, got %s\noutput: %s", redact(fmt.Sprint(err)), redact(out))
	}
	if strings.Contains(out, "PS1 RAN") {
		t.Fatalf("blocked .ps1 must not run under %s; output: %s", redact(policy), redact(out))
	}
	for _, want := range []struct{ key, value string }{
		{"promoted", "True"},
		{"id", "UnauthorizedAccess"},
	} {
		if got := ev[want.key]; got != want.value {
			t.Errorf("evidence %s: want %q, got %q\noutput: %s", want.key, want.value, redact(got), redact(out))
		}
	}
}

// psEvidenceMarker prefixes each structured line
// TestExitCapture_BlockedScriptExitsOne prints from inside powershell.exe.
const psEvidenceMarker = "AIX_PS_EVIDENCE "

// blockedScriptEvidence parses the `AIX_PS_EVIDENCE <key>=<value>` lines. The
// marker may follow other text on a line: CombinedOutput mixes in stderr's
// CLIXML, which need not end with a newline. policy, promoted and id are
// required (blank values are allowed); a repeated key is an error.
func blockedScriptEvidence(out string) (map[string]string, error) {
	ev := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		i := strings.Index(line, psEvidenceMarker)
		if i < 0 {
			continue
		}
		key, value, ok := strings.Cut(line[i+len(psEvidenceMarker):], "=")
		if !ok {
			return nil, fmt.Errorf("malformed evidence line %q", line)
		}
		key = strings.TrimSpace(key)
		if _, dup := ev[key]; dup {
			return nil, fmt.Errorf("evidence key %q repeated", key)
		}
		ev[key] = strings.TrimSpace(value)
	}
	for _, key := range []string{"policy", "promoted", "id"} {
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
// inside home, so it is replaced first. The user name is matched only where no
// Unicode letter, digit or `_` touches it (Go's \b is ASCII-only and would miss
// "José"); the delimiters are consumed, so it repeats until stable to catch two
// names sharing one delimiter. Empty values are skipped.
func redactPSDiagnostics(out, tempDir, home, user string) string {
	for _, r := range []struct{ value, placeholder string }{
		{tempDir, "<tempdir>"},
		{home, "<home>"},
	} {
		if r.value == "" {
			continue
		}
		out = regexp.MustCompile(`(?i)`+regexp.QuoteMeta(r.value)).ReplaceAllLiteralString(out, r.placeholder)
	}
	if user != "" {
		re := regexp.MustCompile(`(?i)(^|[^\p{L}\p{N}_])` + regexp.QuoteMeta(user) + `($|[^\p{L}\p{N}_])`)
		for {
			next := re.ReplaceAllString(out, "${1}<user>${2}")
			if next == out {
				break
			}
			out = next
		}
	}
	return redactAgentSecrets(out)
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
		out := "AIX_PS_EVIDENCE policy=Restricted\r\nnoise\r\nAIX_PS_EVIDENCE promoted=True\r\n" +
			"AIX_PS_EVIDENCE id=UnauthorizedAccess\r\nAIX_PS_EVIDENCE category=SecurityError\r\n" +
			"AIX_PS_EVIDENCE type=System.Management.Automation.PSSecurityException\r\n"
		ev, err := blockedScriptEvidence(out)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			"policy": "Restricted", "promoted": "True", "id": "UnauthorizedAccess",
			"category": "SecurityError", "type": "System.Management.Automation.PSSecurityException",
		}
		for k, v := range want {
			if ev[k] != v {
				t.Errorf("%s: got %q, want %q", k, ev[k], v)
			}
		}
	})
	t.Run("marker after CLIXML on the same line", func(t *testing.T) {
		out := "AIX_PS_EVIDENCE policy=AllSigned\n#< CLIXML\n<Objs Version=\"1.1.0.1\"></Objs>" +
			"AIX_PS_EVIDENCE promoted=True\nAIX_PS_EVIDENCE id=UnauthorizedAccess\n"
		ev, err := blockedScriptEvidence(out)
		if err != nil {
			t.Fatal(err)
		}
		if ev["policy"] != "AllSigned" || ev["promoted"] != "True" || ev["id"] != "UnauthorizedAccess" {
			t.Errorf("got %v", ev)
		}
	})
	t.Run("blank values", func(t *testing.T) {
		ev, err := blockedScriptEvidence("AIX_PS_EVIDENCE policy=Restricted\nAIX_PS_EVIDENCE promoted=False\n" +
			"AIX_PS_EVIDENCE id=\nAIX_PS_EVIDENCE type=\n")
		if err != nil {
			t.Fatal(err)
		}
		if v, ok := ev["id"]; !ok || v != "" {
			t.Errorf("id: got %q (present %v), want blank", v, ok)
		}
	})
	t.Run("missing id", func(t *testing.T) {
		if _, err := blockedScriptEvidence("AIX_PS_EVIDENCE policy=Restricted\nAIX_PS_EVIDENCE promoted=True\n"); err == nil {
			t.Error("expected an error for a missing id")
		}
	})
	t.Run("duplicated key", func(t *testing.T) {
		out := "AIX_PS_EVIDENCE policy=Restricted\nAIX_PS_EVIDENCE policy=Bypass\n" +
			"AIX_PS_EVIDENCE promoted=True\nAIX_PS_EVIDENCE id=UnauthorizedAccess\n"
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

	out, err := runLocalCommandWindows("aixshim", []string{"--version"}, "", 30*time.Second)
	if err != nil {
		t.Fatalf("expected exit 0 from the .cmd shim, got %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "CMD") || strings.Contains(out, "PS1") {
		t.Fatalf("expected the .cmd shim to run (not the .ps1); output: %q", out)
	}
}
