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

// linuxElevation describes the ambient conditions that decide how a Linux
// install elevates. Kept as plain data so linuxInstallCommand stays a pure
// function the tests can drive without touching the real process environment.
type linuxElevation struct {
	// EUID is the effective user id of this process.
	EUID int
	// HasTTY reports whether stdin is a character device, i.e. whether sudo
	// would have a terminal on which to prompt for a password.
	HasTTY bool
	// HasGraphicalSession reports whether a desktop session is present
	// (DISPLAY / WAYLAND_DISPLAY), which polkit's agent needs to show its
	// authentication dialog.
	HasGraphicalSession bool
	// HasPkexec reports whether the pkexec binary resolves on PATH.
	HasPkexec bool
}

// currentLinuxElevation samples the real process environment.
func currentLinuxElevation() linuxElevation {
	env := linuxElevation{
		EUID:                os.Geteuid(),
		HasGraphicalSession: os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "",
	}
	if fi, err := os.Stdin.Stat(); err == nil {
		env.HasTTY = fi.Mode()&os.ModeCharDevice != 0
	}
	if _, err := exec.LookPath("pkexec"); err == nil {
		env.HasPkexec = true
	}
	return env
}

// linuxInstallCommand builds the apt-get invocation for the given package,
// choosing how (and whether) to elevate.
//
//   - Already root (euid 0): invoke apt-get directly. It has the privileges to
//     install, and minimal root environments (e.g. containers) may ship apt-get
//     without a sudo binary, so spawning sudo there would fail to launch and
//     drop into recovery even though the install could have succeeded.
//   - Non-root with no controlling terminal: prefer pkexec when a desktop
//     session and the binary are both present. The agent normally starts from
//     the XDG autostart entry, which sets Terminal=false (tray_linux.go), so
//     sudo has neither a tty to prompt on nor an askpass helper — it would fail
//     to authenticate and send every automatic install into recovery on any
//     machine without cached credentials or a NOPASSWD rule. pkexec routes the
//     prompt through the polkit agent, matching the GUI-first approach the
//     zenity/kdialog dialogs already take on this platform.
//   - Otherwise: sudo, which prompts on the terminal we do have. This is also
//     the fallback when pkexec or the desktop session is missing, since sudo
//     still succeeds under NOPASSWD or cached credentials and its failure text
//     is captured into the recovery diagnostics either way.
func linuxInstallCommand(pkg string, env linuxElevation) (string, []string) {
	aptArgs := []string{"apt-get", "-y", "install", pkg}
	switch {
	case env.EUID == 0:
		return "apt-get", []string{"-y", "install", pkg}
	case !env.HasTTY && env.HasGraphicalSession && env.HasPkexec:
		return "pkexec", aptArgs
	default:
		return "sudo", aptArgs
	}
}

// performInstall attempts a package-manager install on macOS/Linux.
func performInstall(spec DependencySpec) installOutcome {
	pkg := unixInstallPackage(spec)

	// binary is the package manager that must exist on PATH; cmdName/args is what
	// we actually spawn. On Linux apt-get normally needs elevation, and which
	// launcher can actually authenticate depends on how the agent was started —
	// see linuxInstallCommand for the root / pkexec / sudo selection.
	var binary, cmdName string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		binary, cmdName = "brew", "brew"
		args = []string{"install", pkg}
	default: // linux and other unix
		binary = "apt-get"
		cmdName, args = linuxInstallCommand(pkg, currentLinuxElevation())
	}

	// Verify both the package manager and the launcher we'll actually spawn
	// (sudo, when elevation is required) are present, so a missing wrapper is
	// reported as "no package manager available" up front rather than surfacing
	// as an opaque spawn failure once we try to run it.
	for _, exe := range []string{binary, cmdName} {
		if _, err := exec.LookPath(exe); err != nil {
			return installOutcome{
				Kind:   InstallExecFailed,
				Err:    fmt.Errorf("%s not found on PATH: %w", exe, err),
				Output: fmt.Sprintf("No supported package manager (%s) is available to install %s automatically.", binary, spec.DisplayName),
			}
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
