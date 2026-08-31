//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// cmd.exe does not use CommandLineToArgvW quoting rules. Supplying the fixed
// script through CmdLine prevents os/exec from backslash-escaping its quotes,
// while link and target paths remain environment data rather than script text.
func configureGrokWindowsCommandLine(cmd *exec.Cmd, script string) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: "/d /v:off /c " + script,
	}
}
