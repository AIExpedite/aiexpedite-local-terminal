//go:build !windows
// +build !windows

// File: install_runner_other_test.go
//
// Tests for the non-Windows install runner's post-install verification.
package main

import (
	"os/exec"
	"testing"
)

// verifyInstalledOnPath must execute the working-version probe, not just resolve
// the command on PATH. Setup can be triggered precisely because a stale or
// broken git already resolves (Apple's /usr/bin/git developer-tools shim, a
// half-removed package), and a bare LookPath would accept that same entry once
// brew/apt exits cleanly — letting ensureGit announce success while every
// subsequent Git command still fails. `go` is guaranteed present when tests run
// yet fails `--version` (it uses `go version`), so it stands in for a
// resolvable-but-broken command.
func TestVerifyInstalledOnPath_RejectsResolvableButBroken(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; cannot exercise the resolvable-but-broken case")
	}
	if verifyInstalledOnPath("go") {
		t.Error("verifyInstalledOnPath should reject a command that resolves on PATH but fails --version")
	}
}

// A genuinely working command must still verify, so the stricter probe doesn't
// turn every successful brew/apt install into a verification failure.
func TestVerifyInstalledOnPath_AcceptsWorkingCommand(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	// `uname --version` is not portable; use a command whose --version works on
	// both macOS and Linux test runners, skipping when neither is available.
	for _, cmd := range []string{"git", "bash", "python3"} {
		if _, err := exec.LookPath(cmd); err != nil {
			continue
		}
		if verifyInstalledOnPath(cmd) {
			return // at least one working command verified
		}
	}
	t.Skip("no command with a working --version available to verify against")
}
