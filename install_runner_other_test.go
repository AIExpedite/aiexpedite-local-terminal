//go:build !windows
// +build !windows

// File: install_runner_other_test.go
//
// Tests for the non-Windows install runner's post-install verification.
package main

import (
	"os/exec"
	"strings"
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

// A process already running as root must invoke apt-get directly rather than
// through sudo: a minimal root environment can have apt-get but no sudo binary,
// and spawning the missing wrapper would fail to launch and drop into recovery
// even though the install would have succeeded. Non-root keeps the sudo path.
func TestLinuxInstallCommand_RootSkipsSudo(t *testing.T) {
	cmd, args := linuxInstallCommand("git", 0)
	if cmd != "apt-get" {
		t.Errorf("root should invoke apt-get directly, got %q", cmd)
	}
	want := []string{"-y", "install", "git"}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Errorf("root args = %v, want %v", args, want)
	}

	cmd, args = linuxInstallCommand("git", 1000)
	if cmd != "sudo" {
		t.Errorf("non-root should elevate via sudo, got %q", cmd)
	}
	want = []string{"apt-get", "-y", "install", "git"}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Errorf("non-root args = %v, want %v", args, want)
	}
}

// Support diagnostics must name the package that was actually attempted. Off
// Windows that is the brew/apt package (`git`), never the WinGet id (`Git.Git`)
// — reporting the latter is misleading on exactly the failure path the recovery
// dialog exists to explain.
func TestPlatformPackageID_UsesUnixPackage(t *testing.T) {
	spec := DependencySpec{DisplayName: "Git", WingetID: "Git.Git", UnixPackage: "git", VerifyCommand: "git"}
	if got := platformPackageID(spec); got != "git" {
		t.Errorf("platformPackageID = %q, want the Unix package %q", got, "git")
	}

	// Mirrors performInstall's fallback so the reported package can't drift
	// from the installed one.
	noPkg := DependencySpec{DisplayName: "Git", WingetID: "Git.Git", VerifyCommand: "git"}
	if got := platformPackageID(noPkg); got != "git" {
		t.Errorf("platformPackageID = %q, want the VerifyCommand fallback %q", got, "git")
	}

	summary := installDiagnosticsSummary(spec, installOutcome{Kind: InstallOther, ExitCode: 1})
	if strings.Contains(summary, "Git.Git") {
		t.Errorf("recovery diagnostics must not report the WinGet id off Windows: %q", summary)
	}
	if !strings.Contains(summary, "Package: git") {
		t.Errorf("recovery diagnostics should report the Unix package: %q", summary)
	}
}
