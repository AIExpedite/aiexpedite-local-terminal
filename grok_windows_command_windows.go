//go:build windows

package main

import (
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
