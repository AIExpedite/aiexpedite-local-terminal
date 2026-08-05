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
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
)

// performInstall attempts a package-manager install on macOS/Linux.
func performInstall(spec DependencySpec) installOutcome {
	pkg := spec.UnixPackage
	if pkg == "" {
		pkg = spec.VerifyCommand
	}

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
			Stderr: fmt.Sprintf("No supported package manager (%s) is available to install %s automatically.", binary, spec.DisplayName),
		}
	}

	fmt.Printf("\n→ Installing %s via %s...\n\n", spec.DisplayName, binary)

	cmd := exec.Command(cmdName, args...)
	var stderrBuf bytes.Buffer
	cmd.Stdout = os.Stdout
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)

	runErr := cmd.Run()
	stderr := stderrBuf.String()

	if runErr == nil {
		if spec.VerifyCommand == "" {
			return installOutcome{Kind: InstallOK}
		}
		if _, err := exec.LookPath(spec.VerifyCommand); err == nil {
			return installOutcome{Kind: InstallOK}
		}
		return installOutcome{
			Kind:   InstallOther,
			Stderr: fmt.Sprintf("%s installed but %q is not on PATH.", spec.DisplayName, spec.VerifyCommand),
		}
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return installOutcome{
			Kind:     InstallOther,
			ExitCode: exitErr.ExitCode(),
			Stderr:   stderr,
			Err:      runErr,
		}
	}

	return installOutcome{
		Kind:   InstallExecFailed,
		Stderr: stderr,
		Err:    runErr,
	}
}
