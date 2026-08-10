//go:build !windows
// +build !windows

// File: install_runner_other_test.go
//
// Tests for the non-Windows install runner's post-install verification.
package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The TTY probe must reject non-terminal character devices. /dev/null is a
// character device (its os.ModeCharDevice bit is set), so the previous
// ModeCharDevice heuristic reported HasTTY=true for a session-manager launch
// whose stdin is /dev/null — which flipped linuxInstallCommand from pkexec to
// sudo and reproduced the no-password-prompt failure this runner avoids. A pipe
// stands in for the redirected-stdin case. A real tty can't be opened in CI, so
// only the false cases are asserted; those are exactly the regression.
func TestIsTerminalFd_RejectsNonTTY(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer devNull.Close()
	if isTerminalFd(int(devNull.Fd())) {
		t.Errorf("%s is a character device but not a terminal; isTerminalFd must return false", os.DevNull)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()
	if isTerminalFd(int(r.Fd())) {
		t.Error("a pipe read end is not a terminal; isTerminalFd must return false")
	}
}

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

// Elevation selection has to cover three distinct environments: a root process
// (no wrapper at all — a minimal root container can have apt-get but no sudo
// binary, and spawning the missing wrapper would drop into recovery even though
// the install would have succeeded); a non-root process with a terminal (sudo
// can prompt); and a non-root process started from the XDG autostart entry,
// which sets Terminal=false, so sudo has no tty and no askpass helper and
// cannot authenticate — that one must route through pkexec's polkit agent.
func TestLinuxInstallCommand_ElevationSelection(t *testing.T) {
	tests := []struct {
		name     string
		env      linuxElevation
		wantCmd  string
		wantArgs []string
	}{
		{
			name:     "root invokes apt-get directly",
			env:      linuxElevation{EUID: 0, HasTTY: true},
			wantCmd:  "apt-get",
			wantArgs: []string{"-y", "install", "git"},
		},
		{
			name:     "root without tty still skips any wrapper",
			env:      linuxElevation{EUID: 0, HasGraphicalSession: true, HasPkexec: true},
			wantCmd:  "apt-get",
			wantArgs: []string{"-y", "install", "git"},
		},
		{
			name:     "non-root with a terminal prompts via sudo",
			env:      linuxElevation{EUID: 1000, HasTTY: true, HasGraphicalSession: true, HasPkexec: true},
			wantCmd:  "sudo",
			wantArgs: []string{"apt-get", "-y", "install", "git"},
		},
		{
			name:     "autostart session without a tty elevates via pkexec",
			env:      linuxElevation{EUID: 1000, HasGraphicalSession: true, HasPkexec: true},
			wantCmd:  "pkexec",
			wantArgs: []string{"apt-get", "-y", "install", "git"},
		},
		{
			name:     "no tty and no pkexec falls back to sudo",
			env:      linuxElevation{EUID: 1000, HasGraphicalSession: true},
			wantCmd:  "sudo",
			wantArgs: []string{"apt-get", "-y", "install", "git"},
		},
		{
			name:     "no tty and no desktop session falls back to sudo",
			env:      linuxElevation{EUID: 1000, HasPkexec: true},
			wantCmd:  "sudo",
			wantArgs: []string{"apt-get", "-y", "install", "git"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd, args := linuxInstallCommand("git", tc.env)
			if cmd != tc.wantCmd {
				t.Errorf("launcher = %q, want %q", cmd, tc.wantCmd)
			}
			if strings.Join(args, " ") != strings.Join(tc.wantArgs, " ") {
				t.Errorf("args = %v, want %v", args, tc.wantArgs)
			}
		})
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
