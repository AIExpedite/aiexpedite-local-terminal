// File: install_runner.go
//
// Cross-platform orchestration for installing a missing workstation
// dependency (e.g. Git) via the platform package manager. The goal is that a
// failed install — in particular the WinGet case where the downloaded
// installer can't be found or won't launch — never leaves the user staring at
// a raw OS error dialog. Instead we:
//
//  1. Ask permission (ShowInstallPrompt).
//  2. Run the platform install command and classify the outcome.
//  3. On failure, explain what happened in plain language and offer a guided
//     recovery dialog (retry / install manually / view troubleshooting).
//  4. Log full diagnostics (command, exit code, captured stderr, classified
//     reason) to the audit log so support can investigate.
//
// The actual install execution is platform-specific and lives in
// install_runner_windows.go (WinGet) and install_runner_other.go (brew/apt).
// This file is deliberately platform-agnostic so the orchestration and its
// tests compile everywhere.
package main

import (
	"errors"
	"fmt"
	"strings"
)

// maxInstallRetries bounds how many times the recovery dialog will offer an
// automatic retry before only Manual / Troubleshooting / Exit remain. This
// stops a persistently-broken package source (bad WinGet cache, offline
// machine) from trapping the user in an infinite retry loop.
const maxInstallRetries = 2

// DependencySpec describes a workstation dependency the setup flow can install.
type DependencySpec struct {
	// DisplayName is the human-facing name shown in dialogs, e.g. "Git".
	DisplayName string
	// PromptDescription is the one-paragraph explanation shown in the initial
	// permission dialog.
	PromptDescription string
	// WingetID is the WinGet package identifier, e.g. "Git.Git".
	WingetID string
	// UnixPackage is the package name for brew/apt on macOS/Linux, e.g. "git".
	UnixPackage string
	// VerifyCommand is the executable that must appear on PATH once the
	// install succeeds, e.g. "git". Used to confirm the install actually
	// landed (WinGet can report success while the binary isn't yet visible).
	VerifyCommand string
	// ManualURL is the vendor download page opened for "install manually".
	ManualURL string
	// TroubleshootURL is the docs page opened for "view troubleshooting".
	TroubleshootURL string
}

// InstallFailureKind classifies why an install attempt did not succeed. The
// classification drives both the plain-language explanation shown to the user
// and the diagnostics recorded for support.
type InstallFailureKind int

const (
	// InstallOK — the install completed and the dependency is now available.
	InstallOK InstallFailureKind = iota
	// InstallLaunchFailed — the package manager downloaded the installer but
	// could not find or launch it (the case this feature primarily targets).
	InstallLaunchFailed
	// InstallExecFailed — the package manager process itself could not be
	// spawned (winget/brew absent from PATH, permission denied).
	InstallExecFailed
	// InstallOther — any other non-zero exit (network, hash mismatch,
	// user-cancelled UAC). Routed to the generic recovery path.
	InstallOther
)

// InstallRecoveryChoice is the user's response to the guided recovery dialog.
// Defined once here (no build tag) so the Windows and non-Windows
// ShowInstallRecovery implementations can't drift apart.
type InstallRecoveryChoice int

const (
	// RecoveryRetry — try the automatic install again.
	RecoveryRetry InstallRecoveryChoice = iota
	// RecoveryManual — open the vendor download page and install by hand.
	RecoveryManual
	// RecoveryTroubleshoot — open troubleshooting guidance, then return to
	// the recovery dialog.
	RecoveryTroubleshoot
	// RecoveryExit — give up on this dependency for now.
	RecoveryExit
)

// installOutcome is the result of a single platform install attempt.
type installOutcome struct {
	Kind     InstallFailureKind
	ExitCode int    // package-manager exit code (0 when not applicable)
	Stderr   string // captured stderr / diagnostic text (may be truncated)
	Err      error  // underlying Go error, if any
}

// Sentinel errors so callers can distinguish user intent from real failures
// and decide whether to treat them as fatal.
var (
	// errInstallDeclined — user chose not to install (Exit at the prompt or
	// recovery dialog).
	errInstallDeclined = errors.New("dependency install declined by user")
	// errInstallManual — user opted to install manually; the download page
	// was opened for them.
	errInstallManual = errors.New("dependency install deferred to manual")
)

// Seams — package-level function variables so tests can substitute the
// dialog, install execution, and browser calls without touching real
// WinGet / GUI. Mirrors the existing repo pattern (see auth.go).
var (
	installPrompt   = ShowInstallPrompt
	installExec     = performInstall
	installRecovery = ShowInstallRecovery
	installOpenURL  = openBrowser
)

// runDependencyInstall walks a dependency through the permission prompt, the
// platform install, and — on failure — the guided recovery loop. It returns
// nil on success, errInstallDeclined / errInstallManual when the user opts
// out, or the classified failure error when an install could not be
// recovered. Callers decide whether that is fatal.
func runDependencyInstall(spec DependencySpec) error {
	switch installPrompt(spec.DisplayName, spec.PromptDescription) {
	case InstallNo:
		LogSecurityEvent(SecEvtInstallDeclined, "user declined install",
			"component", spec.DisplayName, "at", "prompt")
		return errInstallDeclined
	case InstallManual:
		openRecoveryURL(spec, spec.ManualURL, "manual_from_prompt")
		return errInstallManual
	case InstallYes:
		// fall through to the install / recovery loop
	}

	attempt := 0
	for {
		LogSecurityEvent(SecEvtInstallStarted, "starting dependency install",
			"component", spec.DisplayName, "package", spec.WingetID, "attempt", attempt)

		outcome := installExec(spec)
		if outcome.Kind == InstallOK {
			LogSecurityEvent(SecEvtInstallSucceeded, "dependency installed",
				"component", spec.DisplayName, "attempt", attempt)
			return nil
		}

		LogSecurityEvent(SecEvtInstallFailed, "dependency install failed",
			"component", spec.DisplayName,
			"reason", installFailureReason(outcome.Kind),
			"exit_code", outcome.ExitCode,
			"error", errString(outcome.Err),
			"stderr", truncateDiagnostic(outcome.Stderr, 500),
			"attempt", attempt)

		// Recovery sub-loop: stays open across "view troubleshooting" so the
		// user can read the docs and then still choose retry / manual / exit.
		// retry becomes true only when the user asks (and is allowed) to retry,
		// which breaks back out to re-run the install.
		retry := false
		for !retry {
			allowRetry := attempt < maxInstallRetries
			switch installRecovery(
				spec.DisplayName,
				installExplanation(spec, outcome),
				installDiagnosticsSummary(spec, outcome),
				allowRetry,
			) {
			case RecoveryRetry:
				if !allowRetry {
					// The dialog shouldn't offer Retry past the cap; guard
					// against a stuck dialog by treating it as giving up.
					return installGaveUp(spec, outcome)
				}
				attempt++
				retry = true
			case RecoveryManual:
				openRecoveryURL(spec, spec.ManualURL, "manual_from_recovery")
				return errInstallManual
			case RecoveryTroubleshoot:
				openRecoveryURL(spec, spec.TroubleshootURL, "troubleshoot")
				// stay in the recovery sub-loop
			case RecoveryExit:
				return installGaveUp(spec, outcome)
			}
		}
	}
}

// installGaveUp logs the user giving up on recovery and returns the classified
// failure error.
func installGaveUp(spec DependencySpec, o installOutcome) error {
	LogSecurityEvent(SecEvtInstallDeclined, "user exited recovery",
		"component", spec.DisplayName, "at", "recovery",
		"reason", installFailureReason(o.Kind))
	return fmt.Errorf("%s install failed: %s",
		spec.DisplayName, installFailureReason(o.Kind))
}

// openRecoveryURL opens a recovery URL in the browser and records whether the
// launch itself failed — a failed "manual install" / "troubleshoot" click is
// otherwise silently dropped and looks like the button did nothing.
func openRecoveryURL(spec DependencySpec, url, context string) {
	if url == "" {
		return
	}
	if err := installOpenURL(url); err != nil {
		LogSecurityEvent(SecEvtInstallFailed, "could not open recovery URL",
			"component", spec.DisplayName, "context", context,
			"url", url, "error", err.Error())
	}
}

// installFailureReason returns a short, log-friendly reason slug.
func installFailureReason(k InstallFailureKind) string {
	switch k {
	case InstallLaunchFailed:
		return "installer_launch_failed"
	case InstallExecFailed:
		return "package_manager_unavailable"
	case InstallOther:
		return "install_failed"
	default:
		return "ok"
	}
}

// installExplanation returns the plain-language explanation shown to the user
// in the recovery dialog for the given outcome.
func installExplanation(spec DependencySpec, o installOutcome) string {
	switch o.Kind {
	case InstallLaunchFailed:
		return fmt.Sprintf(
			"%s was downloaded, but Windows could not start its installer "+
				"(the setup file couldn't be found or failed to launch).\n\n"+
				"This is usually temporary — a retry often succeeds. You can "+
				"also install it yourself or view troubleshooting steps.",
			spec.DisplayName)
	case InstallExecFailed:
		return fmt.Sprintf(
			"%s could not be installed because the package manager could not "+
				"be started (it may not be installed or on your PATH).\n\n"+
				"You can install %s manually or view troubleshooting steps.",
			spec.DisplayName, spec.DisplayName)
	default:
		return fmt.Sprintf(
			"%s could not be installed automatically.\n\n"+
				"You can retry, install it manually, or view troubleshooting "+
				"steps.",
			spec.DisplayName)
	}
}

// installDiagnosticsSummary builds the short technical summary surfaced in the
// recovery dialog (and useful when a user pastes it to support). Full detail
// lives in the audit log.
func installDiagnosticsSummary(spec DependencySpec, o installOutcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Package: %s\n", spec.WingetID)
	fmt.Fprintf(&b, "Reason: %s\n", installFailureReason(o.Kind))
	if o.ExitCode != 0 {
		fmt.Fprintf(&b, "Exit code: %d (0x%X)\n", o.ExitCode, uint32(o.ExitCode))
	}
	if o.Err != nil {
		fmt.Fprintf(&b, "Error: %s\n", o.Err.Error())
	}
	if snippet := truncateDiagnostic(o.Stderr, 300); snippet != "" {
		fmt.Fprintf(&b, "Details: %s", snippet)
	}
	return strings.TrimRight(b.String(), "\n")
}

// truncateDiagnostic trims captured output to a bounded, single-block snippet
// so a runaway installer log can't bloat the audit record or overflow a dialog.
func truncateDiagnostic(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
