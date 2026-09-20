//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_smoke_grok_windows_test.go — the Windows .cmd shim spawn shape.
   --------------------------------------------------------------------------
   `npm install -g` puts a `grok.cmd` batch shim on PATH, and cmd.exe re-parses
   the shim's `%*` before the real binary sees argv. The smoke must therefore
   route such a launch through cmd.exe with an explicit command line whose
   only variable parts (the two paths) ride in the environment. These cases
   run only on a Windows runner; the pure-string script assertions live in
   cliagent_smoke_grok_test.go and run everywhere.
   ------------------------------------------------------------------------ */

func TestGrokSmokeShimCommand_RoutesCmdShimThroughCmdExeAndHidesTheWindow(t *testing.T) {
	promptFile := filepath.Join(t.TempDir(), "smoke prompt & (nonce)!.txt")
	launch := grokSmokeLaunch{
		Path:       `C:\Users\someone\AppData\Roaming\npm\grok.cmd`,
		Args:       buildGrokNoToolsSmokeArgs(grokSmokeArgvShapes[0], promptFile),
		Env:        []string{`PATH=C:\Windows`, `GROK_HOME=C:\isolated`},
		Dir:        `C:\isolated\workspace`,
		PromptFile: promptFile,
	}
	cmd, ok := grokSmokeShimCommand(context.Background(), launch)
	if !ok {
		t.Fatal("a .cmd shim launch must take the cmd.exe route")
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow {
		t.Error("shim route lost HideWindow — cmd.exe would flash a console on the user's desktop")
	}
	line := cmd.SysProcAttr.CmdLine
	if !strings.HasPrefix(line, "/d /v:off /c call \"%"+grokSmokeShimPathEnv+"%\"") {
		t.Errorf("CmdLine = %q, want the /d /v:off /c call prefix with an env-indirected shim path", line)
	}
	if strings.Contains(line, launch.Path) || strings.Contains(line, promptFile) {
		t.Errorf("a path leaked into the raw cmd.exe command line: %q", line)
	}
	// Every fixed token survives verbatim (including the equals-form empty
	// --tools=), and the prompt path is handed over by env, not script.
	for _, arg := range launch.Args[:len(launch.Args)-2] {
		if !strings.Contains(" "+line+" ", " "+arg+" ") {
			t.Errorf("CmdLine dropped token %q: %q", arg, line)
		}
	}
	env := map[string]string{}
	for _, kv := range cmd.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if env[grokSmokeShimPathEnv] != launch.Path || env[grokSmokeShimPromptEnv] != promptFile {
		t.Errorf("shim/prompt env = %q / %q", env[grokSmokeShimPathEnv], env[grokSmokeShimPromptEnv])
	}
	if env["GROK_HOME"] != `C:\isolated` || cmd.Dir != launch.Dir {
		t.Errorf("shim route changed the child's home/cwd: home=%q dir=%q", env["GROK_HOME"], cmd.Dir)
	}

	// A native binary is spawned directly.
	launch.Path = `C:\Users\someone\.grok\bin\grok.exe`
	if _, ok := grokSmokeShimCommand(context.Background(), launch); ok {
		t.Error("a native grok.exe must not be routed through cmd.exe")
	}
	if isGrokWindowsShim(`C:\tools\grok.ps1`) {
		t.Error("a .ps1 is not a cmd.exe batch shim (CreateProcess cannot run it); it must not take this route")
	}
}

// End to end through a REAL batch shim: cmd.exe re-parses the line, the shim's
// `%*` re-parses it again, and the child must still receive exactly the
// ladder's tokens — with the prompt path (spaces and metacharacters included)
// intact and no empty element dropped.
func TestGrokSmokeShimCommand_RoundTripsEveryTokenThroughARealShim(t *testing.T) {
	testExe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mockExe := filepath.Join(dir, "grok-mock.exe")
	if err := copyTestBinary(testExe, mockExe); err != nil {
		t.Fatal(err)
	}
	// The shape npm writes: a batch file that forwards %* to the real program.
	shim := filepath.Join(dir, "grok.cmd")
	if err := os.WriteFile(shim, []byte("@echo off\r\n\"%~dp0grok-mock.exe\" %*\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	promptDir := filepath.Join(t.TempDir(), "with space & (meta)")
	if err := os.MkdirAll(promptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	promptFile := filepath.Join(promptDir, "grok-prompt-x.txt")
	if err := os.WriteFile(promptFile, []byte("prompt"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, shape := range grokSmokeArgvShapes {
		args := buildGrokNoToolsSmokeArgs(shape, promptFile)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		stdout, stderr, runErr := runGrokSmokeCommand(ctx, grokSmokeLaunch{
			Path: shim, Args: args, Dir: dir, PromptFile: promptFile,
			Env: setEnvVar(os.Environ(), mockCLIEnvVar, "grok-smoke-argv-echo"),
		})
		cancel()
		if runErr != nil {
			t.Fatalf("rung %s: shim run failed: %v (stderr %q)", shape.ID, runErr, stderr)
		}
		var received []string
		for _, line := range bytes.Split(stdout, []byte("\n")) {
			var frame struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(bytes.TrimSpace(line), &frame) == nil && frame.Type == "text" {
				received = append(received, frame.Text)
			}
		}
		if !reflect.DeepEqual(received, args) {
			t.Fatalf("rung %s: child received %#v, want %#v", shape.ID, received, args)
		}
		stream := parseGrokSmokeStream(stdout)
		if !stream.Ended {
			t.Fatalf("rung %s: terminal end frame missing from %q", shape.ID, stdout)
		}
	}
}
