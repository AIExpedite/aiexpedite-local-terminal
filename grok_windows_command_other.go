//go:build !windows

package main

import "os/exec"

func configureGrokWindowsCommandLine(_ *exec.Cmd, _ string) {}
