//go:build windows
// +build windows

// File: install_runner_windows.go
//
// Windows install execution via the Windows Package Manager (WinGet), plus
// classification of the failure modes this feature cares about — most
// importantly the "downloaded installer couldn't be found / failed to launch"
// case, which WinGet surfaces through specific exit codes and stderr text.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// WinGet return codes (HRESULT-style, from winget-cli's authoritative
// APPINSTALLER_CLI_ERROR_* set). WinGet returns these as 32-bit values; Go's
// ExitCode() hands them back as a signed int, so we compare on the uint32
// reinterpretation.
//
// Installer-launch failures — the "installer couldn't be found / failed to
// launch" case this feature targets:
const (
	// ERROR_FILE_NOT_FOUND — "the system cannot find the file specified":
	// the installer WinGet tried to launch was not present on disk.
	wingetErrFileNotFound uint32 = 0x80070002
	// APPINSTALLER_CLI_ERROR_SHELLEXEC_INSTALL_FAILED — WinGet located the
	// installer but ShellExecute failed to launch it. This is the primary
	// installer-launch failure this feature targets.
	wingetErrShellExecFailed uint32 = 0x8A150006
)

// Other documented WinGet failures that are NOT installer-launch problems.
// Declared so the distinction is explicit and pinned by tests; they route to
// the generic recovery path (retry / manual / troubleshoot) rather than the
// "installer couldn't launch" explanation.
const (
	wingetErrDownloadFailed    uint32 = 0x8A150008 // installer download failed
	wingetErrHashMismatch      uint32 = 0x8A150011 // corrupt / tampered installer
	wingetErrUpdateNotApplic   uint32 = 0x8A15002B // no applicable update found
	wingetErrAgreementsNotOK   uint32 = 0x8A150041 // package agreements not accepted
	wingetErrInstallInProgress uint32 = 0x8A150102 // another install already running
)

// launchFailureSubstrings is an English-language fallback for WinGet builds /
// locales whose exit codes we don't recognise but whose output still names the
// installer-not-found / launch problem. Kept tight to avoid misclassifying
// unrelated failures; best-effort — non-English Windows may fall through to
// the generic recovery path (still recoverable).
var launchFailureSubstrings = []string{
	"cannot find the file",
	"could not find the file",
	"failed to launch",
	"unable to launch",
	"shellexecute",
}

// performInstall installs the dependency via WinGet, falling back to Scoop
// when the spec configures it (e.g. ttyd). Output streams to the console
// (preserving the setup UX) while a bounded tail is captured for diagnostics
// and classification.
func performInstall(spec DependencySpec) installOutcome {
	showConsoleWindow(true)

	// detected reports whether the tool is now installed. When the spec
	// provides an in-process detector (e.g. ttyd, which the app uses
	// immediately), its result is authoritative and it may augment PATH.
	// Otherwise we trust the manager's clean exit — fine for tools only needed
	// after a restart (e.g. Git): a freshly-installed binary won't appear on
	// this process's already-captured PATH, so re-probing would spuriously
	// report success as failure.
	detected := func(exitOK bool) bool {
		if spec.PostInstall != nil {
			return spec.PostInstall()
		}
		return exitOK
	}

	var failure installOutcome
	haveFailure, triedManager := false, false

	// 1) WinGet (primary).
	if _, err := exec.LookPath("winget"); err == nil {
		triedManager = true
		exitOK, out, code, runErr := runInstallManager(spec.DisplayName, "winget", []string{
			"install", "-e", "--id", spec.WingetID,
			"--accept-package-agreements", "--accept-source-agreements",
			"--disable-interactivity",
		})
		if detected(exitOK) {
			return installOutcome{Kind: InstallOK}
		}
		failure, haveFailure = classifyRun(code, out, runErr), true
	}

	// 2) Scoop (optional Windows fallback, e.g. ttyd).
	if spec.ScoopID != "" {
		if _, err := exec.LookPath("scoop"); err == nil {
			triedManager = true
			exitOK, out, code, runErr := runInstallManager(spec.DisplayName, "scoop", []string{"install", spec.ScoopID})
			if detected(exitOK) {
				return installOutcome{Kind: InstallOK}
			}
			// Prefer WinGet's classification when we have one (its output names
			// the installer-launch problem); otherwise use Scoop's.
			if !haveFailure {
				failure, haveFailure = classifyRun(code, out, runErr), true
			}
		}
	}

	if !triedManager {
		managers := "winget"
		if spec.ScoopID != "" {
			managers = "winget/scoop"
		}
		return installOutcome{
			Kind:   InstallExecFailed,
			Output: fmt.Sprintf("No supported package manager (%s) is available to install %s.", managers, spec.DisplayName),
		}
	}
	return failure
}

// runInstallManager runs a package-manager command, streaming its output live
// to the console while capturing a bounded tail for diagnostics/classification.
// Returns (exitedZero, capturedTail, exitCode, runErr). exitCode is 0 on a
// spawn failure (runErr will be a non-ExitError in that case).
func runInstallManager(display, manager string, args []string) (bool, string, int, error) {
	fmt.Printf("\n→ Installing %s via %s...\n→ Running: %s %s\n\n",
		display, manager, manager, strings.Join(args, " "))

	cmd := exec.Command(manager, args...)
	// Tee both streams into one bounded tail buffer: WinGet writes most output
	// (including the actionable error, which comes last) to stdout, so
	// classification must see both streams and keep the end.
	tail := newTailWriter(diagnosticCaptureBytes)
	cmd.Stdout = io.MultiWriter(os.Stdout, tail)
	cmd.Stderr = io.MultiWriter(os.Stderr, tail)

	runErr := cmd.Run()
	out := tail.String()
	if runErr == nil {
		return true, out, 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return false, out, exitErr.ExitCode(), runErr
	}
	return false, out, 0, runErr // spawn error
}

// classifyRun turns a single manager run into an installOutcome.
func classifyRun(exitCode int, output string, runErr error) installOutcome {
	// A non-ExitError means the process couldn't be started → exec failure.
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return installOutcome{Kind: InstallExecFailed, Output: output, Err: runErr}
	}
	return installOutcome{
		Kind:     classifyInstallFailure(exitCode, output),
		ExitCode: exitCode,
		Output:   output,
		Err:      runErr,
	}
}

// classifyInstallFailure maps a WinGet exit code (and, as a fallback, its
// captured output text) to an InstallFailureKind. Keyed off the documented
// installer-launch codes, with a tight English substring fallback for
// unrecognised builds.
func classifyInstallFailure(exitCode int, output string) InstallFailureKind {
	if exitCode == 0 {
		return InstallOK
	}

	switch uint32(exitCode) {
	case wingetErrFileNotFound, wingetErrShellExecFailed:
		return InstallLaunchFailed
	}

	lower := strings.ToLower(output)
	for _, sub := range launchFailureSubstrings {
		if strings.Contains(lower, sub) {
			return InstallLaunchFailed
		}
	}

	return InstallOther
}
