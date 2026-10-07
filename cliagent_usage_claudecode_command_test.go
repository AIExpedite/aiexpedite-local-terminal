package main

import (
	"strings"
	"testing"
)

// commandRunsClaude sees `claude` through every wrapper transport
// terminal-service emits — POSIX `-c`, `cmd /c`, PowerShell `-Command`,
// `-EncodedCommand` and the file-mode launcher — with the Windows program forms
// isClaudeCommand already accepts for a direct launch.
func TestCommandRunsClaude(t *testing.T) {
	cases := []struct {
		name    string
		command string
		args    []string
		want    bool
	}{
		{"bash -c", "bash", []string{"-c", "claude -p hi"}, true},
		{"bash -lc", "bash", []string{"-lc", "cd /repo && claude -p hi"}, true},
		{"cmd /c", "cmd", []string{"/c", "claude -p hi"}, true},
		{"cmd.exe /c claude.cmd", "cmd.exe", []string{"/c", `claude.cmd -p "hi"`}, true},
		{"powershell -Command", "powershell", []string{"-Command", "claude -p hi"}, true},
		{"powershell -Command claude.exe", "powershell", []string{"-Command", `C:\tools\claude.exe -p hi`}, true},
		{"quoted absolute path", "powershell",
			[]string{"-Command", `& "C:\Program Files\nodejs\claude.cmd" -p "hi"`}, true},
		{"powershell -EncodedCommand npm shim", "powershell.exe",
			[]string{"-NoProfile", "-NonInteractive", "-EncodedCommand",
				encodeForPowerShell(`Set-Location C:\repo; & 'C:\Users\u\AppData\Roaming\npm\claude.cmd' -p "do it"`)}, true},
		{"powershell file-mode launcher", "powershell.exe",
			[]string{"-NoProfile", "-EncodedCommand", helperFileModeLauncher(`claude.exe -p "a long prompt"`)}, true},
		{"posix file-mode launcher", "bash",
			[]string{"-c", helperPosixFileModeLauncher("claude -p hi")}, true},
		{"bash -c exec", "bash", []string{"-c", `exec claude -p "hi"`}, true},
		{"bash -c env assignment", "bash", []string{"-c", `env FOO=bar claude -p "hi"`}, true},
		{"bash -c env flags", "bash", []string{"-c", "env -i -u HOME FOO=bar /usr/local/bin/claude -p hi"}, true},
		{"sh -c nohup", "sh", []string{"-c", "cd /repo && nohup claude -p hi"}, true},
		{"bash -c exec env", "bash", []string{"-c", "exec env FOO=1 claude -p hi"}, true},
		{"bash -c command", "bash", []string{"-c", `command claude -p "hi"`}, true},
		{"bash -c command -p", "bash", []string{"-c", "command -p claude -p hi"}, true},
		{"bash -c exec -c", "bash", []string{"-c", `exec -c claude -p "hi"`}, true},
		{"bash -c exec -cl", "bash", []string{"-c", "exec -cl claude -p hi"}, true},
		{"bash -c exec -a name", "bash", []string{"-c", "exec -a claudish claude -p hi"}, true},
		{"bash -c exec -ca name", "bash", []string{"-c", "exec -ca claudish claude -p hi"}, true},

		// A mention is not a spawn; a direct launch is isClaudeCommand's case.
		{"git log --grep claude", "bash", []string{"-c", "git log --grep claude"}, false},
		{"echo claude", "cmd", []string{"/c", "echo claude"}, false},
		{"powershell echo claude", "powershell", []string{"-Command", "Write-Output claude"}, false},
		{"file-mode launcher wrapping another program", "powershell.exe",
			[]string{"-EncodedCommand", helperFileModeLauncher("npm run build")}, false},
		{"oversize payload", "powershell",
			[]string{"-Command", strings.Repeat("x", wrapperClassifyMaxPayloadBytes+1) + "; claude -p hi"}, false},
		{"undecodable base64", "powershell", []string{"-EncodedCommand", "!!!not base64!!!"}, false},
		{"direct launch is not a wrapper", "claude", []string{"-p", "hi"}, false},
		{"agy wrapper", "bash", []string{"-c", "agy --print hi"}, false},
		{"exec another program", "bash", []string{"-c", "exec env FOO=1 git log --grep claude"}, false},
		{"exec -a renames another program", "bash", []string{"-c", "exec -a claude git log"}, false},
		{"command -v only locates", "bash", []string{"-c", "command -v claude"}, false},
		{"command -V only describes", "bash", []string{"-c", "command -V claude"}, false},
		{"bare env", "bash", []string{"-c", "env"}, false},
	}
	for _, tc := range cases {
		if got := commandRunsClaude(tc.command, tc.args); got != tc.want {
			t.Errorf("%s: commandRunsClaude(%q, %v) = %v, want %v", tc.name, tc.command, tc.args, got, tc.want)
		}
	}
}

// The session-exit owe is gated on runsClaude: a direct launch or a wrapped
// one, never anything else.
func TestSessionRunsClaude_DirectOrWrapped(t *testing.T) {
	for _, tc := range []struct {
		command string
		args    []string
		want    bool
	}{
		{"claude", []string{"-p", "hi"}, true},
		{`C:\Users\u\AppData\Roaming\npm\claude.cmd`, nil, true},
		{"powershell", []string{"-EncodedCommand", encodeForPowerShell("claude -p hi")}, true},
		{"powershell", []string{"-EncodedCommand", encodeForPowerShell("codex exec hi")}, false},
		{"codex", []string{"exec", "hi"}, false},
	} {
		if got := isClaudeCommand(tc.command) || commandRunsClaude(tc.command, tc.args); got != tc.want {
			t.Errorf("runsClaude(%q, %v) = %v, want %v", tc.command, tc.args, got, tc.want)
		}
	}
}
