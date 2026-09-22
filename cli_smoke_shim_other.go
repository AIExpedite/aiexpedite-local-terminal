//go:build !windows

// cli_smoke_shim_other.go — the non-Windows half of cli_smoke_shim_windows.go.
// There is no cmd.exe shim to route through off Windows, so every smoke spawns
// the resolved binary directly.

package main

import (
	"context"
	"os/exec"
)

func configureGrokWindowsCommandLine(_ *exec.Cmd, _ string) {}

// isWindowsShimPath is always false off Windows: a `.cmd` file there is not a
// batch shim anything would launch.
func isWindowsShimPath(_ string) bool { return false }

// grokSmokeShimCommand never applies off Windows.
func grokSmokeShimCommand(_ context.Context, _ grokSmokeLaunch) (*exec.Cmd, bool) {
	return nil, false
}

// cliSmokeShimCommand is only reached for a path isWindowsShimPath accepted,
// which never happens off Windows.
func cliSmokeShimCommand(_ context.Context, _ string, _ []string, _ string) *exec.Cmd {
	return nil
}
