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

	"golang.org/x/sys/windows/registry"
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

// installAttempt records the result of one package-manager attempt so that,
// when every manager fails, diagnostics can report each one (which ran and
// why it failed) rather than only the first.
type installAttempt struct {
	manager string
	outcome installOutcome
}

// performInstall installs the dependency via WinGet, falling back to Scoop
// when the spec configures it (e.g. ttyd). Output streams to the console
// (preserving the setup UX) while a bounded tail is captured for diagnostics
// and classification.
func performInstall(spec DependencySpec) installOutcome {
	// Surface the console so install progress is visible, but remember whether it
	// was already showing and restore that state when we're done. The tray's
	// "Show Console" checkbox is initialized from registration state, not this
	// call, so leaving a forced-visible console would desync the menu item and
	// leave the user having to toggle it twice to hide the window again.
	consoleWasVisible := consoleWindowVisible()
	showSeq := showConsoleWindowSeq(true)
	defer func() {
		if consoleWasVisible {
			return
		}
		// Another flow may have asked for the console while WinGet was running —
		// the tray runs concurrently with StartAgent, so auto-registration
		// (showConsoleWindow(true) + a checked menu item) or the user toggling
		// "Show Console" can land mid-install. That request is newer than ours
		// and wins; hiding on our stale snapshot would leave a hidden window
		// behind a checked menu item.
		if consoleVisibilityChangedSince(showSeq) {
			return
		}
		showConsoleWindow(false)
	}()

	var attempts []installAttempt

	// tryManager runs one manager and returns true on success. On failure it
	// appends the attempt's outcome to attempts.
	tryManager := func(manager string, args []string) bool {
		exitOK, out, code, runErr := runInstallManager(spec.DisplayName, manager, args)
		oc, ok := attemptOutcome(spec, manager, exitOK, out, code, runErr)
		if ok {
			return true
		}
		attempts = append(attempts, installAttempt{manager, oc})
		return false
	}

	// 1) WinGet (primary).
	if _, err := exec.LookPath("winget"); err == nil {
		if tryManager("winget", []string{
			"install", "-e", "--id", spec.WingetID,
			"--accept-package-agreements", "--accept-source-agreements",
			"--disable-interactivity",
		}) {
			return installOutcome{Kind: InstallOK}
		}
	}

	// 2) Scoop (optional Windows fallback, e.g. ttyd).
	if spec.ScoopID != "" {
		if _, err := exec.LookPath("scoop"); err == nil {
			if tryManager("scoop", []string{"install", spec.ScoopID}) {
				return installOutcome{Kind: InstallOK}
			}
		}
	}

	if len(attempts) == 0 {
		managers := "winget"
		if spec.ScoopID != "" {
			managers = "winget/scoop"
		}
		return installOutcome{
			Kind:   InstallExecFailed,
			Output: fmt.Sprintf("No supported package manager (%s) is available to install %s.", managers, spec.DisplayName),
		}
	}
	return aggregateFailures(attempts)
}

// attemptOutcome interprets a single manager run. It returns (outcome, success).
// success is true when the tool is now considered installed. How that is
// decided depends on the spec:
//
//   - PostInstall set (e.g. ttyd): its in-process detector — not the exit code —
//     is authoritative, and it may augment PATH so the tool is usable now.
//   - No PostInstall but VerifyCommand set (e.g. Git): a clean exit is NOT
//     trusted on its own. WinGet writes the updated PATH to the registry but
//     cannot mutate this already-running process's environment, so a tool it
//     just installed stays invisible to this process (and to the ttyd/shell
//     StartAgent launches next) until a restart. verifyInstalledOnPath refreshes
//     PATH from the registry and re-checks, so success means "usable now".
//   - Neither set: the exit code is trusted directly.
//
// A clean exit that still fails verification is NOT success: the manager
// reported success but the tool isn't usable/visible, so we return an explicit
// InstallOther verification-failure outcome instead of leaking a zero exit
// code through classifyRun (which would look like InstallOK).
func attemptOutcome(spec DependencySpec, manager string, exitOK bool, out string, code int, runErr error) (installOutcome, bool) {
	detected := exitOK
	switch {
	case spec.PostInstall != nil:
		detected = spec.PostInstall()
	case exitOK && spec.VerifyCommand != "":
		detected = verifyInstalledOnPath(spec.VerifyCommand)
	}
	if detected {
		return installOutcome{Kind: InstallOK}, true
	}
	if exitOK {
		return installOutcome{
			Kind: InstallOther,
			Output: fmt.Sprintf("%s reported success but %s could not be verified on PATH — a restart may be required.\n%s",
				manager, spec.DisplayName, out),
		}, false
	}
	return classifyRun(code, out, runErr), false
}

// verifyInstalledOnPath reports whether cmd is actually usable, refreshing this
// process's PATH from the persisted Windows environment first when it isn't.
// It executes the working-version probe (`cmd --version`) — the same check
// ensureGit/readiness use — rather than exec.LookPath alone: setup can be
// triggered precisely because a stale or broken git already resolves on PATH,
// and a bare LookPath would accept that same broken entry after WinGet exits,
// short-circuiting before the PATH refresh that would surface the freshly
// installed (working) git. Probing execution instead means a resolvable-but-
// broken command fails the first probe, the refresh promotes the newly
// installed directory, and only a genuinely working command reports success.
// It is a package-level seam so tests can exercise attemptOutcome's verify
// branch without a real registry or a real install.
var verifyInstalledOnPath = func(cmd string) bool {
	if probeVersion(cmd, "--version") != "" {
		return true
	}
	refreshProcessPathFromRegistry()
	return probeVersion(cmd, "--version") != ""
}

// refreshProcessPathFromRegistry re-reads the persisted machine and user PATH
// from the registry and merges them into this process's PATH. WinGet writes the
// updated PATH there when it installs a tool, but it cannot mutate the
// environment of an already-running process — so without this refresh a tool
// WinGet just installed stays invisible to this process (and to everything it
// spawns) until the app restarts.
func refreshProcessPathFromRegistry() {
	sep := string(os.PathListSeparator)
	persisted := make([]string, 0, 2)
	if p := readPersistedPath(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Control\Session Manager\Environment`); p != "" {
		persisted = append(persisted, p)
	}
	if p := readPersistedPath(registry.CURRENT_USER, `Environment`); p != "" {
		persisted = append(persisted, p)
	}
	if len(persisted) == 0 {
		return
	}
	// Put the freshly-persisted entries ahead of the inherited PATH so a
	// just-installed dir wins, then dedup so PATH doesn't grow across refreshes.
	os.Setenv("PATH", mergePathList(strings.Join(persisted, sep)+sep+os.Getenv("PATH")))
}

// readPersistedPath reads and expands the "Path" value under the given registry
// key, returning "" if it is absent or unreadable.
func readPersistedPath(root registry.Key, subkey string) string {
	key, err := registry.OpenKey(root, subkey, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer key.Close()
	// PATH is usually REG_EXPAND_SZ (contains %SystemRoot% etc.); read the raw
	// value and expand any embedded environment variables.
	val, _, err := key.GetStringValue("Path")
	if err != nil {
		return ""
	}
	if expanded, err := registry.ExpandString(val); err == nil {
		return expanded
	}
	return val
}

// mergePathList drops empty and case-insensitively duplicate entries from a
// PATH-style list while preserving first-seen order, so merged machine/user/
// process PATHs stay bounded across repeated refreshes.
func mergePathList(list string) string {
	sep := string(os.PathListSeparator)
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, e := range strings.Split(list, sep) {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		k := strings.ToLower(e)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, e)
	}
	return strings.Join(out, sep)
}

// aggregateFailures combines every attempted manager's failure into one
// outcome. For the user-facing classification it prefers a launch failure (the
// most actionable), else the first attempt. The Output aggregates each
// manager's reason/exit/tail so the audit trail shows the fallback ran too.
func aggregateFailures(attempts []installAttempt) installOutcome {
	if len(attempts) == 1 {
		return attempts[0].outcome
	}

	chosen := attempts[0].outcome
	for _, a := range attempts {
		if a.outcome.Kind == InstallLaunchFailed {
			chosen = a.outcome
			break
		}
	}

	var b strings.Builder
	for i, a := range attempts {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[%s] %s", a.manager, installFailureReason(a.outcome.Kind))
		if a.outcome.ExitCode != 0 {
			fmt.Fprintf(&b, " (exit 0x%X)", uint32(a.outcome.ExitCode))
		}
		if snippet := truncateDiagnostic(a.outcome.Output, 300); snippet != "" {
			fmt.Fprintf(&b, ": %s", snippet)
		}
	}
	chosen.Output = b.String()
	return chosen
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
