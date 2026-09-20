//go:build windows

package main

import (
	"context"
	"os/exec"
	"syscall"
)

// cmd.exe does not use CommandLineToArgvW quoting rules. Supplying the fixed
// script through CmdLine prevents os/exec from backslash-escaping its quotes,
// while link and target paths remain environment data rather than script text.
//
// Merges into any existing SysProcAttr so it cannot drop flags a caller already
// set — notably HideWindow, without which cmd.exe flashes a console window on
// the user's desktop during a background usage refresh.
func configureGrokWindowsCommandLine(cmd *exec.Cmd, script string) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CmdLine = "/d /v:off /c " + script
}

// grokSmokeShimCommand routes a `.cmd` / `.bat` npm shim launch of the Grok
// maintenance smoke through cmd.exe with an explicit command line, so the
// shim's re-parse cannot re-split or drop a token. Both paths ride in the
// child's environment (grokSmokeShimScript); the script itself carries only
// the ladder's fixed flag tokens. Returns ok=false for a native binary or a
// launch whose argv the script renderer refuses, and the caller spawns
// directly instead.
func grokSmokeShimCommand(ctx context.Context, launch grokSmokeLaunch) (*exec.Cmd, bool) {
	if !isGrokWindowsShim(launch.Path) {
		return nil, false
	}
	script, ok := grokSmokeShimScript(launch.Args)
	if !ok {
		return nil, false
	}
	cmd := grokWindowsCommandContext(ctx, script)
	env := setEnvVar(launch.Env, grokSmokeShimPathEnv, launch.Path)
	cmd.Env = setEnvVar(env, grokSmokeShimPromptEnv, launch.PromptFile)
	cmd.Dir = launch.Dir
	return cmd, true
}
