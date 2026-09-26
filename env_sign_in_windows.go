//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// launchSignInWindow opens a new, visible console window running the sign-in
// through cmd.exe (so npm .cmd shims work and the window can pause on the
// result). The command line is passed verbatim (SysProcAttr.CmdLine) — Go's
// argv escaping follows CommandLineToArgvW rules, which cmd.exe does not use.
// Not waited for and not registered with the process registry: the window
// belongs to the person signing in and must outlive this command.
func launchSignInWindow(program string, args []string, title string) error {
	comspec := os.Getenv("ComSpec")
	if comspec == "" || !filepath.IsAbs(comspec) {
		comspec = filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	}
	c := exec.Command(comspec)
	c.SysProcAttr = &syscall.SysProcAttr{
		CmdLine:       windowsSignInCommandLine(comspec, program, args, title),
		CreationFlags: CREATE_NEW_CONSOLE,
	}
	c.Env = os.Environ()
	if home, err := os.UserHomeDir(); err == nil {
		c.Dir = home
	}
	if err := c.Start(); err != nil {
		return err
	}
	go func() { _ = c.Wait() }()
	return nil
}
