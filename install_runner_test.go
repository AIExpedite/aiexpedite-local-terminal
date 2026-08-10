// File: install_runner_test.go
//
// Tests for the cross-platform install/recovery orchestration in
// install_runner.go. The platform install, the recovery dialog, and the
// browser launch are all swapped out through the package-level seams so no
// real WinGet / GUI runs.
package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// withInstallSeams saves and restores the install seams around a test body so
// overrides never leak between tests.
func withInstallSeams(t *testing.T) func() {
	t.Helper()
	origPrompt, origExec, origRecovery, origOpen, origInfo := installPrompt, installExec, installRecovery, installOpenURL, installShowInfo
	// Default the info-dialog seam to a no-op so a stray call can never block
	// the test on a real modal dialog.
	installShowInfo = func(string, string) {}
	return func() {
		installPrompt, installExec, installRecovery, installOpenURL, installShowInfo = origPrompt, origExec, origRecovery, origOpen, origInfo
	}
}

// redirectSecurityLog points the audit log at a temp dir and resets the
// once-guarded logger so a test can inspect the emitted diagnostics. Returns
// the log path.
func redirectSecurityLog(t *testing.T) string {
	t.Helper()
	origBase := baseDir
	dir := t.TempDir()
	baseDir = dir
	// Reset the sync.Once + handle so initSecurityLogger re-runs against the
	// temp dir instead of any previously-initialised logger.
	secLogOnce = sync.Once{}
	secLogFile = nil
	secLogInitOk = false
	t.Cleanup(func() {
		CloseSecurityLogger()
		baseDir = origBase
		secLogOnce = sync.Once{}
		secLogFile = nil
		secLogInitOk = false
	})
	return filepath.Join(dir, "security.log")
}

func testSpec() DependencySpec {
	return DependencySpec{
		DisplayName:     "Git",
		WingetID:        "Git.Git",
		VerifyCommand:   "git",
		ManualURL:       "https://example.com/download",
		TroubleshootURL: "https://example.com/help",
	}
}

func TestRunDependencyInstall_SuccessFirstAttempt(t *testing.T) {
	defer withInstallSeams(t)()

	execCalls := 0
	installPrompt = func(string, string, bool) InstallChoice { return InstallYes }
	installExec = func(DependencySpec) installOutcome { execCalls++; return installOutcome{Kind: InstallOK} }
	installRecovery = func(string, string, string, bool) InstallRecoveryChoice {
		t.Fatal("recovery dialog should not be shown on success")
		return RecoveryExit
	}

	if err := runDependencyInstall(testSpec()); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if execCalls != 1 {
		t.Fatalf("expected 1 install attempt, got %d", execCalls)
	}
}

func TestRunDependencyInstall_RetryThenSucceeds(t *testing.T) {
	defer withInstallSeams(t)()

	execCalls, recoveryCalls := 0, 0
	installPrompt = func(string, string, bool) InstallChoice { return InstallYes }
	installExec = func(DependencySpec) installOutcome {
		execCalls++
		if execCalls == 1 {
			return installOutcome{Kind: InstallLaunchFailed, ExitCode: -1978793950}
		}
		return installOutcome{Kind: InstallOK}
	}
	installRecovery = func(_, _, _ string, allowRetry bool) InstallRecoveryChoice {
		recoveryCalls++
		if !allowRetry {
			t.Fatalf("retry should be allowed on first failure")
		}
		return RecoveryRetry
	}

	if err := runDependencyInstall(testSpec()); err != nil {
		t.Fatalf("expected nil error after retry, got %v", err)
	}
	if execCalls != 2 {
		t.Fatalf("expected 2 install attempts, got %d", execCalls)
	}
	if recoveryCalls != 1 {
		t.Fatalf("expected 1 recovery dialog, got %d", recoveryCalls)
	}
}

func TestRunDependencyInstall_RetryCapEnforced(t *testing.T) {
	defer withInstallSeams(t)()

	execCalls := 0
	installPrompt = func(string, string, bool) InstallChoice { return InstallYes }
	installExec = func(DependencySpec) installOutcome {
		execCalls++
		return installOutcome{Kind: InstallLaunchFailed}
	}
	// Always ask to retry — the runner must stop once the cap is reached even
	// if the dialog keeps returning Retry.
	installRecovery = func(string, string, string, bool) InstallRecoveryChoice { return RecoveryRetry }

	if err := runDependencyInstall(testSpec()); err == nil {
		t.Fatal("expected a failure error once retries are exhausted")
	}
	if want := 1 + maxInstallRetries; execCalls != want {
		t.Fatalf("expected %d install attempts (initial + %d retries), got %d", want, maxInstallRetries, execCalls)
	}
}

func TestRunDependencyInstall_ManualOpensBrowserOnce(t *testing.T) {
	defer withInstallSeams(t)()

	var openedURLs []string
	installPrompt = func(string, string, bool) InstallChoice { return InstallYes }
	installExec = func(DependencySpec) installOutcome { return installOutcome{Kind: InstallOther} }
	installRecovery = func(string, string, string, bool) InstallRecoveryChoice { return RecoveryManual }
	installOpenURL = func(url string) error { openedURLs = append(openedURLs, url); return nil }

	if err := runDependencyInstall(testSpec()); err != errInstallManual {
		t.Fatalf("expected errInstallManual, got %v", err)
	}
	if len(openedURLs) != 1 || openedURLs[0] != testSpec().ManualURL {
		t.Fatalf("expected manual URL opened once, got %v", openedURLs)
	}
}

func TestRunDependencyInstall_TroubleshootThenExit(t *testing.T) {
	defer withInstallSeams(t)()

	var openedURLs []string
	recoveryCalls := 0
	installPrompt = func(string, string, bool) InstallChoice { return InstallYes }
	installExec = func(DependencySpec) installOutcome { return installOutcome{Kind: InstallLaunchFailed} }
	installOpenURL = func(url string) error { openedURLs = append(openedURLs, url); return nil }
	installRecovery = func(string, string, string, bool) InstallRecoveryChoice {
		recoveryCalls++
		if recoveryCalls == 1 {
			return RecoveryTroubleshoot // opens docs, dialog re-shows
		}
		return RecoveryExit
	}

	if err := runDependencyInstall(testSpec()); err == nil {
		t.Fatal("expected failure error after exiting recovery")
	}
	if len(openedURLs) != 1 || openedURLs[0] != testSpec().TroubleshootURL {
		t.Fatalf("expected troubleshoot URL opened once, got %v", openedURLs)
	}
	if recoveryCalls != 2 {
		t.Fatalf("expected recovery dialog shown twice (troubleshoot then exit), got %d", recoveryCalls)
	}
}

func TestRunDependencyInstall_CancelAtPrompt(t *testing.T) {
	defer withInstallSeams(t)()

	installPrompt = func(string, string, bool) InstallChoice { return InstallCancel }
	installExec = func(DependencySpec) installOutcome {
		t.Fatal("install must not run when cancelled at the prompt")
		return installOutcome{}
	}

	if err := runDependencyInstall(testSpec()); err != errInstallDeclined {
		t.Fatalf("expected errInstallDeclined, got %v", err)
	}
}

func TestRunDependencyInstall_LogsDiagnosticsOnFailure(t *testing.T) {
	defer withInstallSeams(t)()
	logPath := redirectSecurityLog(t)

	installPrompt = func(string, string, bool) InstallChoice { return InstallYes }
	installExec = func(DependencySpec) installOutcome {
		return installOutcome{Kind: InstallLaunchFailed, ExitCode: 42, Output: "installer file not found"}
	}
	installRecovery = func(string, string, string, bool) InstallRecoveryChoice { return RecoveryExit }

	_ = runDependencyInstall(testSpec())

	CloseSecurityLogger()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading security log: %v", err)
	}
	log := string(data)
	for _, want := range []string{"install_failed", "installer_launch_failed", "Git", "installer file not found"} {
		if !strings.Contains(log, want) {
			t.Fatalf("expected diagnostics to contain %q, log was:\n%s", want, log)
		}
	}
}

// Skipping at the recovery dialog is an explicit opt-out, so the error must
// satisfy errors.Is(err, errInstallDeclined) — installTtydWindows keys its
// "install it and restart" exit branch off that sentinel, and without it the
// tray app would keep running with no ttyd.
func TestRunDependencyInstall_RecoveryExitReturnsDeclineSentinel(t *testing.T) {
	defer withInstallSeams(t)()

	installPrompt = func(string, string, bool) InstallChoice { return InstallYes }
	installExec = func(DependencySpec) installOutcome { return installOutcome{Kind: InstallLaunchFailed} }
	installRecovery = func(string, string, string, bool) InstallRecoveryChoice { return RecoveryExit }

	err := runDependencyInstall(testSpec())
	if !errors.Is(err, errInstallDeclined) {
		t.Fatalf("expected an error wrapping errInstallDeclined, got %v", err)
	}
	// The classified reason must survive in the message for logs/support.
	if !strings.Contains(err.Error(), "installer_launch_failed") {
		t.Errorf("expected the failure reason in the message, got %q", err.Error())
	}
}

// Exhausting retries is NOT a user opt-out, so it must not masquerade as one.
func TestRunDependencyInstall_RetryCapIsNotADecline(t *testing.T) {
	defer withInstallSeams(t)()

	installPrompt = func(string, string, bool) InstallChoice { return InstallYes }
	installExec = func(DependencySpec) installOutcome { return installOutcome{Kind: InstallLaunchFailed} }
	installRecovery = func(string, string, string, bool) InstallRecoveryChoice { return RecoveryRetry }

	err := runDependencyInstall(testSpec())
	if err == nil {
		t.Fatal("expected a failure error once retries are exhausted")
	}
	if errors.Is(err, errInstallDeclined) {
		t.Fatalf("exhausted retries must not report a user decline, got %v", err)
	}
}

// The spec's Optional flag must reach the permission prompt so an optional
// dependency isn't described as required.
func TestRunDependencyInstall_PassesOptionalToPrompt(t *testing.T) {
	defer withInstallSeams(t)()

	var gotOptional bool
	installPrompt = func(_, _ string, optional bool) InstallChoice {
		gotOptional = optional
		return InstallCancel
	}

	spec := testSpec()
	spec.Optional = true
	if err := runDependencyInstall(spec); !errors.Is(err, errInstallDeclined) {
		t.Fatalf("expected errInstallDeclined, got %v", err)
	}
	if !gotOptional {
		t.Fatal("expected the prompt to be told the dependency is optional")
	}
}

// An optional dependency must not be told it "is required". Button semantics
// remain Yes / No / Cancel regardless of whether the dependency is optional.
func TestInstallPromptWording_OptionalVsRequired(t *testing.T) {
	required := installPromptHeadline("Git", false)
	if !strings.Contains(required, "required") {
		t.Errorf("required headline should say so, got %q", required)
	}
	optional := installPromptHeadline("Git", true)
	if strings.Contains(optional, "is required") {
		t.Errorf("optional headline must not claim the dependency is required, got %q", optional)
	}
	if !strings.Contains(optional, "optional") {
		t.Errorf("optional headline should say it's optional, got %q", optional)
	}

	if installPromptTitle(true) == installPromptTitle(false) {
		t.Error("optional and required prompts should not share a title")
	}
}

// Git is warning-level for readiness (ensureGit never aborts startup), so its
// spec must be marked optional or the prompt misdescribes what happens next.
func TestGitDependencySpec_IsOptional(t *testing.T) {
	if !gitDependencySpec().Optional {
		t.Error("Git is non-fatal for startup, so its spec must be Optional")
	}
}

func TestInstallDiagnosticsSummary_Truncates(t *testing.T) {
	long := strings.Repeat("x", 1000)
	out := installDiagnosticsSummary(testSpec(), installOutcome{Kind: InstallOther, ExitCode: 5, Output: long})
	// The package line names whatever THIS platform actually installs (the
	// WinGet id on Windows, the brew/apt package elsewhere), so assert against
	// platformPackageID rather than a hard-coded id. The per-platform values
	// are pinned in install_runner_windows_test.go / install_runner_other_test.go.
	if want := "Package: " + platformPackageID(testSpec()); !strings.Contains(out, want) {
		t.Errorf("summary should include %q: %q", want, out)
	}
	if len(out) > 500 {
		t.Errorf("summary should be bounded, got %d chars", len(out))
	}
}

// Finding 4: a failed browser launch on "Install manually" must NOT be
// reported as a successful navigation; recovery choices stay available.
func TestRunDependencyInstall_ManualBrowserFailureKeepsChoices(t *testing.T) {
	defer withInstallSeams(t)()

	openAttempts, recoveryCalls := 0, 0
	installPrompt = func(string, string, bool) InstallChoice { return InstallYes }
	installExec = func(DependencySpec) installOutcome { return installOutcome{Kind: InstallLaunchFailed} }
	installOpenURL = func(string) error { openAttempts++; return errors.New("no browser") }
	installRecovery = func(string, string, string, bool) InstallRecoveryChoice {
		recoveryCalls++
		if recoveryCalls == 1 {
			return RecoveryManual // browser fails → must stay in loop
		}
		return RecoveryExit
	}

	err := runDependencyInstall(testSpec())
	if errors.Is(err, errInstallManual) {
		t.Fatal("must not report manual success when the browser failed to launch")
	}
	if err == nil {
		t.Fatal("expected a failure error after exiting recovery")
	}
	if openAttempts != 1 {
		t.Fatalf("expected exactly 1 browser attempt, got %d", openAttempts)
	}
	if recoveryCalls != 2 {
		t.Fatalf("expected recovery dialog re-shown after failed browser launch, got %d calls", recoveryCalls)
	}
}

// Finding 3: truncateDiagnostic keeps the TAIL (where the actionable error
// lives), not the head.
func TestTruncateDiagnostic_KeepsTail(t *testing.T) {
	s := strings.Repeat("HEADER ", 100) + "ACTUAL_ERROR_AT_END"
	out := truncateDiagnostic(s, 40)
	if !strings.Contains(out, "ACTUAL_ERROR_AT_END") {
		t.Errorf("expected tail (actual error) retained, got %q", out)
	}
	if len(out) > 40+len("…") {
		t.Errorf("expected bounded output, got %d chars", len(out))
	}
}

// Finding 3: tailWriter bounds memory while retaining the tail of the stream.
func TestTailWriter_BoundsAndKeepsTail(t *testing.T) {
	w := newTailWriter(16)
	for i := 0; i < 1000; i++ {
		if _, err := w.Write([]byte("0123456789")); err != nil {
			t.Fatal(err)
		}
	}
	if len(w.buf) > 16 {
		t.Fatalf("buffer must stay bounded to maxBytes, got %d", len(w.buf))
	}
	got := w.String()
	if !strings.HasPrefix(got, "…") {
		t.Errorf("truncated output should be marked with an ellipsis, got %q", got)
	}
	// The last bytes written were "...0123456789"; the tail must reflect them.
	if !strings.HasSuffix(got, "6789") {
		t.Errorf("expected the tail of the stream, got %q", got)
	}
}

// Finding 3: tailWriter is teed into a command's stdout and stderr, which
// os/exec copies on separate goroutines — so writes must be race-free. Run
// under `go test -race` to catch regressions.
func TestTailWriter_ConcurrentWrites(t *testing.T) {
	w := newTailWriter(64)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				_, _ = w.Write([]byte("abcdefghij"))
			}
		}()
	}
	wg.Wait()
	if got := w.String(); len(got) > 64+len("…") {
		t.Fatalf("buffer must stay bounded under concurrent writes, got %d chars", len(got))
	}
}

// Finding 5: prompts must name the package manager that actually runs per OS.
func TestPackageManagerLabel(t *testing.T) {
	cases := map[string]string{
		"windows": "winget",
		"darwin":  "brew",
		"linux":   "apt",
	}
	for goos, want := range cases {
		if got := packageManagerLabel(goos); !strings.Contains(got, want) {
			t.Errorf("packageManagerLabel(%q) = %q, want it to mention %q", goos, got, want)
		}
	}
}
