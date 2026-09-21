//go:build !windows

package main

import (
	"context"
	"os/exec"
)

func configureGrokWindowsCommandLine(_ *exec.Cmd, _ string) {}

// grokSmokeShimCommand never applies off Windows: there is no cmd.exe shim to
// route through, so the smoke spawns the resolved binary directly.
func grokSmokeShimCommand(_ context.Context, _ grokSmokeLaunch) (*exec.Cmd, bool) {
	return nil, false
}
