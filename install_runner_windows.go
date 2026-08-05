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
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// WinGet exit codes (HRESULT-style, from the winget APPINSTALLER_CLI_ERROR_*
// set) that indicate the downloaded installer could not be found or launched.
// WinGet returns these as 32-bit values; Go's ExitCode() hands them back as a
// signed int, so we compare on the uint32 reinterpretation.
const (
	// ERROR_FILE_NOT_FOUND — "the system cannot find the file specified":
	// the installer WinGet tried to launch was not present on disk.
	wingetErrFileNotFound uint32 = 0x80070002
	// APPINSTALLER_CLI_ERROR_EXEC_INSTALL_FAILED — the installer process was
	// invoked but failed to run to completion (failed to launch / start).
	wingetErrInstallFailed uint32 = 0x8A150102
	// APPINSTALLER_CLI_ERROR_INSTALLER_HASH_MISMATCH — the downloaded
	// installer was corrupt, so it is effectively unusable to launch.
	wingetErrHashMismatch uint32 = 0x8A15002B
	// APPINSTALLER_CLI_ERROR_MISSING_FROM_PATH — installed binary not visible.
	wingetErrMissingFromPath uint32 = 0x8A150041
)

// launchFailureSubstrings is an English-language fallback for WinGet builds /
// locales whose exit codes we don't recognise but whose stderr still names the
// installer-not-found / launch problem. Best-effort: non-English Windows may
// fall through to the generic recovery path (still recoverable).
var launchFailureSubstrings = []string{
	"cannot find the file",
	"could not find the file",
	"installer file",
	"failed to install",
	"failed to launch",
	"unable to launch",
	"downloaded installer",
	"installer hash does not match",
}

// performInstall runs `winget install` for the dependency, streams output to
// the console (preserving the existing setup UX) while also capturing stderr
// for diagnostics, and classifies the result.
func performInstall(spec DependencySpec) installOutcome {
	// If WinGet isn't even present we can't launch anything — that's an
	// exec-failure, not an installer-launch failure.
	if _, err := exec.LookPath("winget"); err != nil {
		return installOutcome{
			Kind:   InstallExecFailed,
			Err:    fmt.Errorf("winget not found on PATH: %w", err),
			Output: "Windows Package Manager (winget) is not installed or not on PATH.",
		}
	}

	showConsoleWindow(true)
	fmt.Printf("\n→ Installing %s via Windows Package Manager (winget)...\n", spec.DisplayName)
	fmt.Printf("→ Running: winget install --id %s\n\n", spec.WingetID)

	cmd := exec.Command("winget", "install",
		"-e", "--id", spec.WingetID,
		"--accept-package-agreements",
		"--accept-source-agreements",
		"--disable-interactivity")

	// Tee both streams so the user still sees progress in real time AND we
	// retain a copy for classification / the audit log. WinGet writes most of
	// its output — including error text — to stdout, so classification must
	// consider both streams, not stderr alone.
	var outBuf bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &outBuf)
	cmd.Stderr = io.MultiWriter(os.Stderr, &outBuf)

	runErr := cmd.Run()
	output := outBuf.String()

	// Trust WinGet's exit status: a clean exit means the install succeeded.
	// We deliberately do NOT re-probe PATH here — a freshly-installed binary
	// won't appear on this process's (already-captured) PATH until a restart,
	// so a LookPath check would spuriously report a successful install as a
	// failure. Callers re-detect the tool on the next launch.
	if runErr == nil {
		return installOutcome{Kind: InstallOK}
	}

	// The process ran but exited non-zero → classify by exit code + output.
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		code := exitErr.ExitCode()
		return installOutcome{
			Kind:     classifyInstallFailure(code, output),
			ExitCode: code,
			Output:   output,
			Err:      runErr,
		}
	}

	// Couldn't start the process at all (spawn error) → exec failure.
	return installOutcome{
		Kind:   InstallExecFailed,
		Output: output,
		Err:    runErr,
	}
}

// classifyInstallFailure maps a WinGet exit code (and, as a fallback, its
// captured output text) to an InstallFailureKind. Keyed off documented
// installer not-found / launch codes, with an English substring fallback for
// unrecognised builds.
func classifyInstallFailure(exitCode int, output string) InstallFailureKind {
	if exitCode == 0 {
		return InstallOK
	}

	switch uint32(exitCode) {
	case wingetErrFileNotFound,
		wingetErrInstallFailed,
		wingetErrHashMismatch,
		wingetErrMissingFromPath:
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
