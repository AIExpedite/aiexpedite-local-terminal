// File: install_runner_test.go
//
// Tests for the cross-platform install/recovery orchestration in
// install_runner.go. The platform install, the recovery dialog, and the
// browser launch are all swapped out through the package-level seams so no
// real WinGet / GUI runs.
package main

import (
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
	origPrompt, origExec, origRecovery, origOpen := installPrompt, installExec, installRecovery, installOpenURL
	return func() {
		installPrompt, installExec, installRecovery, installOpenURL = origPrompt, origExec, origRecovery, origOpen
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
	installPrompt = func(string, string) InstallChoice { return InstallYes }
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
	installPrompt = func(string, string) InstallChoice { return InstallYes }
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
	installPrompt = func(string, string) InstallChoice { return InstallYes }
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
	installPrompt = func(string, string) InstallChoice { return InstallYes }
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
	installPrompt = func(string, string) InstallChoice { return InstallYes }
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

func TestRunDependencyInstall_DeclineAtPrompt(t *testing.T) {
	defer withInstallSeams(t)()

	installPrompt = func(string, string) InstallChoice { return InstallNo }
	installExec = func(DependencySpec) installOutcome {
		t.Fatal("install must not run when declined at the prompt")
		return installOutcome{}
	}

	if err := runDependencyInstall(testSpec()); err != errInstallDeclined {
		t.Fatalf("expected errInstallDeclined, got %v", err)
	}
}

func TestRunDependencyInstall_LogsDiagnosticsOnFailure(t *testing.T) {
	defer withInstallSeams(t)()
	logPath := redirectSecurityLog(t)

	installPrompt = func(string, string) InstallChoice { return InstallYes }
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

func TestInstallDiagnosticsSummary_Truncates(t *testing.T) {
	long := strings.Repeat("x", 1000)
	out := installDiagnosticsSummary(testSpec(), installOutcome{Kind: InstallOther, ExitCode: 5, Output: long})
	if !strings.Contains(out, "Git.Git") {
		t.Errorf("summary should include package id: %q", out)
	}
	if len(out) > 500 {
		t.Errorf("summary should be bounded, got %d chars", len(out))
	}
}
