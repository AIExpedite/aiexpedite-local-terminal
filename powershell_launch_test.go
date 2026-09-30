// File: powershell_launch_test.go
// Cross-platform unit tests for powershell_launch.go: .cmd/.bat shim selection
// through the lookPathFn seam, routing in runLocalCommandWindows (classify on
// the original name, launch the shim), and the exit-capture script's contract.
// The Windows host behavior is covered in powershell_windows_test.go.
package main

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// stubLookPath points lookPathFn at a fixed name -> path table for one test.
func stubLookPath(t *testing.T, table map[string]string) {
	t.Helper()
	orig := lookPathFn
	t.Cleanup(func() { lookPathFn = orig })
	lookPathFn = func(name string) (string, error) {
		if p, ok := table[name]; ok {
			return p, nil
		}
		return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
}

func TestPreferBatchShim(t *testing.T) {
	stubLookPath(t, map[string]string{
		"npm":  `C:\Program Files\nodejs\npm.cmd`,
		"grok": `C:\Users\u\AppData\Local\Microsoft\WinGet\Links\grok.exe`,
		"tool": `C:\tools\tool.bat`,
		"up":   `C:\tools\UP.CMD`,
		"bare": `C:\tools\bare`,
		"dots": `C:\node.js\dots`,
	})

	cases := []struct {
		name string
		cmd  string
		args []string
		want string
	}{
		{"npm resolves to npm.cmd", "npm", []string{"install", "-g", "@xai-org/grok"}, "npm.cmd"},
		{"exe hit unchanged (winget grok.exe)", "grok", []string{"--version"}, "grok"},
		{"lookup fails (Node missing) unchanged", "node", []string{"--version"}, "node"},
		{"bat hit", "tool", nil, "tool.bat"},
		{"upper-case CMD keeps its case", "up", nil, "up.CMD"},
		{"hit with no extension unchanged", "bare", nil, "bare"},
		{"dot in directory is not an extension", "dots", nil, "dots"},
		{"path-qualified unchanged", `C:\x\npm`, nil, `C:\x\npm`},
		{"forward-slash path unchanged", "./npm", nil, "./npm"},
		{"explicit .cmd unchanged", "npm.cmd", nil, "npm.cmd"},
		{"explicit .ps1 unchanged", "npm.ps1", nil, "npm.ps1"},
		{"empty unchanged", "", nil, ""},
		{"at sign is cmd-safe", "npm", []string{"view", "@xai-org/grok", "version"}, "npm.cmd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := preferBatchShim(tc.cmd, tc.args); got != tc.want {
				t.Fatalf("preferBatchShim(%q, %q) = %q, want %q", tc.cmd, tc.args, got, tc.want)
			}
		})
	}

	// Any cmd.exe metacharacter in any argument keeps the original name.
	for _, meta := range []string{"&", "%", "^", "|", "<", ">", "!", `"`, "\n", "\r"} {
		args := []string{"run", "a" + meta + "b"}
		if got := preferBatchShim("npm", args); got != "npm" {
			t.Errorf("arg with %q: preferBatchShim = %q, want npm unchanged", meta, got)
		}
	}
}

// withSpiedFallback replaces runLocalCommandFallbackFn and records the command
// line it was handed.
func withSpiedFallback(t *testing.T) *[]string {
	t.Helper()
	orig := runLocalCommandFallbackFn
	t.Cleanup(func() { runLocalCommandFallbackFn = orig })
	var lines []string
	runLocalCommandFallbackFn = func(cmdLine, workDir string, timeout time.Duration) (string, error) {
		lines = append(lines, cmdLine)
		return "fallback-stub", nil
	}
	return &lines
}

func TestRunLocalCommandWindows_TestRunnerClassifiesOnOriginalLaunchesShim(t *testing.T) {
	stubLookPath(t, map[string]string{"npm": `C:\Program Files\nodejs\npm.cmd`})
	lines := withSpiedFallback(t)

	out, err := runLocalCommandWindows("npm", []string{"test", "--", "--watch=false"}, "", 5*time.Second)
	if err != nil || out != "fallback-stub" {
		t.Fatalf("expected the test-runner branch (fallback stub), got out=%q err=%v", out, err)
	}
	if len(*lines) != 1 {
		t.Fatalf("expected one fallback launch, got %d: %q", len(*lines), *lines)
	}
	if got := (*lines)[0]; !strings.HasPrefix(got, "npm.cmd test") {
		t.Fatalf("test runner must launch the shim; got %q", got)
	}
}

func TestRunLocalCommandWindows_CLIAgentNeverRewritten(t *testing.T) {
	stubLookPath(t, map[string]string{
		"claude": `C:\Users\u\AppData\Roaming\npm\claude.cmd`,
		"codex":  `C:\Users\u\AppData\Roaming\npm\codex.cmd`,
	})
	lines := withSpiedFallback(t)

	if _, err := runLocalCommandWindows("claude", []string{"--version"}, "", 5*time.Second); err != nil {
		t.Fatalf("claude: unexpected error %v", err)
	}
	if _, err := runLocalCommandWindows("codex", []string{"--version"}, "", 5*time.Second); err != nil {
		t.Fatalf("codex: unexpected error %v", err)
	}
	if len(*lines) != 2 {
		t.Fatalf("expected two dedicated-process launches, got %q", *lines)
	}
	for _, line := range *lines {
		// claude may still be swapped for its resolved full path
		// (cachedResolveClaudePath) — just never for the bare shim name.
		if lower := strings.ToLower(line); strings.HasPrefix(lower, "claude.cmd") || strings.HasPrefix(lower, "codex.cmd") {
			t.Errorf("CLI agent line must not be shim-rewritten; got %q", line)
		}
		if !strings.HasSuffix(line, " --version") {
			t.Errorf("CLI agent line lost its args; got %q", line)
		}
	}
	if got := (*lines)[1]; got != "codex --version" {
		t.Errorf("codex line = %q, want %q", got, "codex --version")
	}
}

func TestPsExitCaptureScript_Contract(t *testing.T) {
	// Exactly these two error ids mean "PowerShell never started the program".
	for _, id := range []string{"'CommandNotFoundException'", "'UnauthorizedAccess'"} {
		if !strings.Contains(psExitCaptureScript, id) {
			t.Errorf("psExitCaptureScript must match %s exactly; got %q", id, psExitCaptureScript)
		}
	}
	// Exact match via -contains, never a prefix/-like match: a cmdlet's own
	// error id carries a suffix and must not promote.
	if !strings.Contains(psExitCaptureScript, "-contains $Error[0].FullyQualifiedErrorId") {
		t.Errorf("promotion must compare the error id exactly; got %q", psExitCaptureScript)
	}
	// The exit code is read first, before anything else can touch it.
	if !strings.HasPrefix(psExitCaptureScript, "$__aix_exit = $LASTEXITCODE;") {
		t.Errorf("psExitCaptureScript must start by reading $LASTEXITCODE; got %q", psExitCaptureScript)
	}
	// Promotion only when the command failed and no native code was recorded.
	if !strings.Contains(psExitCaptureScript, "-not $__aix_ok -and ($null -eq $__aix_exit -or $__aix_exit -eq 0)") {
		t.Errorf("promotion must require a failed status and no native exit code; got %q", psExitCaptureScript)
	}
	if strings.Contains(psExitCaptureScript, "\n") {
		t.Errorf("psExitCaptureScript must be one line (the persistent host reads -Command - from stdin); got %q", psExitCaptureScript)
	}
	if psStatusCaptureStatement != "$global:__aix_ok = $?" {
		t.Errorf("psStatusCaptureStatement changed: %q", psStatusCaptureStatement)
	}
	for _, stmt := range []string{"$LASTEXITCODE = 0", "$Error.Clear()", "$global:__aix_ok = $true"} {
		if !strings.Contains(psResetScript, stmt) {
			t.Errorf("psResetScript must contain %q; got %q", stmt, psResetScript)
		}
	}
}

func TestPsHostPolicyArgs_ProcessScopedBypass(t *testing.T) {
	joined := strings.Join(psHostPolicyArgs, " ")
	if joined != "-ExecutionPolicy Bypass" {
		t.Fatalf("psHostPolicyArgs = %q, want -ExecutionPolicy Bypass", joined)
	}
}

func TestIsCmdSafeArg(t *testing.T) {
	for _, s := range []string{"install", "-g", "@xai-org/grok", "--prefix=C:\\x y", "a(b)", "x=1,2"} {
		if !isCmdSafeArg(s) {
			t.Errorf("isCmdSafeArg(%q) = false, want true", s)
		}
	}
	if isCmdSafeArg("100%") {
		t.Error("isCmdSafeArg(\"100%\") = true, want false")
	}
}
