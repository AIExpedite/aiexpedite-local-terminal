//go:build !windows
// +build !windows

// File: install_runner_other.go
//
// Non-Windows install execution. macOS/Linux are best-effort via the system
// package manager (Homebrew / apt); when no manager is available we return an
// exec-failure so the shared recovery flow steers the user to manual install.
// This keeps runDependencyInstall cross-platform and compiling everywhere —
// the launch-failure handling itself is primarily a Windows/WinGet concern.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
)

// verifyInstalledOnPath reports whether cmd is actually usable after a
// package-manager run. It executes the working-version probe (`cmd --version`)
// — the same check ensureGit/readiness use — rather than exec.LookPath alone:
// setup can be triggered precisely because a stale or broken git already
// resolves on PATH, and a bare LookPath would accept that same broken entry
// once brew/apt exits cleanly, letting ensureGit announce success while every
// subsequent Git command still fails. Mirrors the Windows runner's helper of
// the same name (which additionally refreshes PATH from the registry, a
// WinGet-specific concern). Package-level seam so tests can exercise the
// post-install verify branch without a real install.
var verifyInstalledOnPath = func(cmd string) bool {
	return probeVersion(cmd, "--version") != ""
}

// platformPackageID returns the package identifier performInstall actually
// hands to brew/apt on this platform, so the recovery dialog and the audit log
// name the package that was really attempted (`git`) rather than the WinGet id
// (`Git.Git`), which is meaningless off Windows.
func platformPackageID(spec DependencySpec) string {
	return unixInstallPackage(spec)
}

// unixInstallPackage is the single source of truth for which package name the
// Unix managers install, shared by performInstall and platformPackageID so the
// attempted package and the reported one can't drift.
func unixInstallPackage(spec DependencySpec) string {
	if spec.UnixPackage != "" {
		return spec.UnixPackage
	}
	return spec.VerifyCommand
}

// performInstall attempts a package-manager install on macOS/Linux.
func performInstall(spec DependencySpec) installOutcome {
	pkg := unixInstallPackage(spec)

	// binary is what must exist on PATH; cmdName/args is what we actually run
	// (apt-get needs sudo, mirroring ensureTtyd's Linux path).
	var binary, cmdName string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		binary, cmdName = "brew", "brew"
		args = []string{"install", pkg}
	default: // linux and other unix
		binary, cmdName = "apt-get", "sudo"
		args = []string{"apt-get", "-y", "install", pkg}
	}

	if _, err := exec.LookPath(binary); err != nil {
		return installOutcome{
			Kind:   InstallExecFailed,
			Err:    fmt.Errorf("%s not found on PATH: %w", binary, err),
			Output: fmt.Sprintf("No supported package manager (%s) is available to install %s automatically.", binary, spec.DisplayName),
		}
	}

	fmt.Printf("\n→ Installing %s via %s...\n\n", spec.DisplayName, binary)

	cmd := exec.Command(cmdName, args...)
	tail := newTailWriter(diagnosticCaptureBytes)
	cmd.Stdout = io.MultiWriter(os.Stdout, tail)
	cmd.Stderr = io.MultiWriter(os.Stderr, tail)

	runErr := cmd.Run()
	output := tail.String()

	if runErr == nil {
		if spec.VerifyCommand == "" {
			return installOutcome{Kind: InstallOK}
		}
		if verifyInstalledOnPath(spec.VerifyCommand) {
			return installOutcome{Kind: InstallOK}
		}
		return installOutcome{
			Kind:   InstallOther,
			Output: fmt.Sprintf("%s installed but %q is not usable on PATH.", spec.DisplayName, spec.VerifyCommand),
		}
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return installOutcome{
			Kind:     InstallOther,
			ExitCode: exitErr.ExitCode(),
			Output:   output,
			Err:      runErr,
		}
	}

	return installOutcome{
		Kind:   InstallExecFailed,
		Output: output,
		Err:    runErr,
	}
}
