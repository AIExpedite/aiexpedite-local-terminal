//go:build windows

package main

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"time"
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

// configureGrokWindowsShimCommandLine renders the `.cmd` shim launch, whose
// script begins with a quoted operand and carries a second one.
//
// Without /s, cmd.exe applies its "strip the first and last quote character"
// rule to such a line and the shim path loses its quoting. The line used to
// lead with `call` to dodge that rule, but `call` performs a SECOND percent
// expansion over the already-expanded text, so a path holding a paired percent
// sequence (`C:\Users\dev\%DEV%\grok.cmd`) was re-read as an environment
// reference and mangled. /s strips exactly the outer quote pair and leaves
// everything between untouched — one expansion pass, both paths literal.
func configureGrokWindowsShimCommandLine(cmd *exec.Cmd, script string) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CmdLine = `/d /s /v:off /c "` + script + `"`
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
	configureGrokWindowsShimCommandLine(cmd, script)
	bindGrokShimProcessTree(cmd)
	env := setEnvVar(launch.Env, grokSmokeShimPathEnv, launch.Path)
	cmd.Env = setEnvVar(env, grokSmokeShimPromptEnv, launch.PromptFile)
	cmd.Dir = launch.Dir
	return cmd, true
}

// grokShimWaitDelay bounds how long Run / CombinedOutput may stay blocked on
// the captured stdout/stderr handles once the deadline has fired and the tree
// has been killed. A descendant that outlives the kill (or a grandchild the
// snapshot taskkill enumerates missed) still holds the write end of the pipe,
// and without a delay Wait blocks on that handle forever — the per-attempt
// deadline would bound nothing. Short, because by this point the whole tree
// has already been force-killed; this only covers the reap race.
const grokShimWaitDelay = 2 * time.Second

// bindGrokShimProcessTree makes the per-attempt deadline reach the process the
// smoke actually cares about. exec.CommandContext kills only the intermediate
// cmd.exe, but the npm `grok.cmd` shim starts Node as a separate child, so the
// default cancel would leave the real Grok process running — holding the
// smoke's stdout/stderr handles and leaking a provider process on every timed
// out attempt. Reuses KillProcessTree (processes_windows.go), the same
// `taskkill /F /T /PID` teardown cleanup and the session signal path use, so
// the whole tree goes down with the context.
func bindGrokShimProcessTree(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		// Tree first: killing cmd.exe on its own reparents the shim's Node
		// child out of taskkill /T's reach.
		_ = KillProcessTree(cmd.Process.Pid)
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = grokShimWaitDelay
}
