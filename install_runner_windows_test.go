//go:build windows
// +build windows

// File: install_runner_windows_test.go
//
// Tests for WinGet install-failure classification.
package main

import (
	"os"
	"strings"
	"testing"
)

// asExit reinterprets a WinGet HRESULT-style uint32 as the signed int Go's
// exec ExitCode() would report (a runtime conversion — a constant one would
// overflow int32).
func asExit(c uint32) int { return int(int32(c)) }

func TestClassifyInstallFailure(t *testing.T) {
	cases := []struct {
		name     string
		exitCode int
		output   string
		want     InstallFailureKind
	}{
		{"success", 0, "", InstallOK},

		// Launch failures — documented literal HRESULTs.
		{"file not found 0x80070002", asExit(0x80070002), "", InstallLaunchFailed},
		{"shellexec install failed 0x8A150006", asExit(0x8A150006), "", InstallLaunchFailed},

		// NON-launch documented failures must route to InstallOther, not be
		// mistaken for an installer-launch problem.
		{"download failed 0x8A150008", asExit(0x8A150008), "", InstallOther},
		{"hash mismatch 0x8A150011", asExit(0x8A150011), "", InstallOther},
		{"update not applicable 0x8A15002B", asExit(0x8A15002B), "", InstallOther},
		{"agreements not accepted 0x8A150041", asExit(0x8A150041), "", InstallOther},
		{"install in progress 0x8A150102", asExit(0x8A150102), "", InstallOther},

		// Named-constant parity with the literals above (also asserts the
		// non-launch constants aren't misclassified).
		{"file not found const", asExit(wingetErrFileNotFound), "", InstallLaunchFailed},
		{"shellexec const", asExit(wingetErrShellExecFailed), "", InstallLaunchFailed},
		{"download failed const", asExit(wingetErrDownloadFailed), "", InstallOther},
		{"hash mismatch const", asExit(wingetErrHashMismatch), "", InstallOther},
		{"update not applicable const", asExit(wingetErrUpdateNotApplic), "", InstallOther},
		{"agreements const", asExit(wingetErrAgreementsNotOK), "", InstallOther},
		{"install in progress const", asExit(wingetErrInstallInProgress), "", InstallOther},

		// English substring fallback (only when the code is unrecognised).
		{"substring file", 1, "The system cannot find the file specified", InstallLaunchFailed},
		{"substring launch", 1, "Installer failed to launch", InstallLaunchFailed},
		{"substring shellexecute", 1, "ShellExecute reported failure", InstallLaunchFailed},

		// Unrelated failures stay generic — including hash text (now Other).
		{"hash text not launch", 1, "Installer hash does not match", InstallOther},
		{"unknown non-zero", 3, "network unreachable", InstallOther},
		{"non-zero empty output", 9009, "", InstallOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyInstallFailure(tc.exitCode, tc.output); got != tc.want {
				t.Errorf("classifyInstallFailure(0x%X, %q) = %v, want %v", uint32(tc.exitCode), tc.output, got, tc.want)
			}
		})
	}
}

func TestClassifyInstallFailure_CaseInsensitiveOutput(t *testing.T) {
	if got := classifyInstallFailure(1, "CANNOT FIND THE FILE on disk"); got != InstallLaunchFailed {
		t.Errorf("expected case-insensitive output match, got %v", got)
	}
}

// Finding 1: a clean manager exit with a FAILED PostInstall check must NOT be
// treated as success — it becomes an explicit InstallOther verification error.
func TestAttemptOutcome_ExitZeroButVerificationFails(t *testing.T) {
	spec := DependencySpec{DisplayName: "ttyd", PostInstall: func() bool { return false }}
	oc, ok := attemptOutcome(spec, "winget", true /*exitOK*/, "winget: Successfully installed", 0, nil)
	if ok {
		t.Fatal("exit-zero with failed PostInstall must not be reported as success")
	}
	if oc.Kind != InstallOther {
		t.Fatalf("want InstallOther verification failure, got %v", oc.Kind)
	}
	if !strings.Contains(oc.Output, "could not be verified") {
		t.Errorf("expected a verification-failure diagnostic, got %q", oc.Output)
	}
}

func TestAttemptOutcome_PostInstallDetectsSuccessDespiteNonZeroExit(t *testing.T) {
	// ttyd pattern: manager exit is ignored; PostInstall (PATH probe) is truth.
	spec := DependencySpec{DisplayName: "ttyd", PostInstall: func() bool { return true }}
	if _, ok := attemptOutcome(spec, "scoop", false /*exitOK*/, "", 1, nil); !ok {
		t.Fatal("PostInstall true should report success regardless of exit code")
	}
}

func TestAttemptOutcome_NoDetectorTrustsExitZero(t *testing.T) {
	spec := DependencySpec{DisplayName: "Git"} // no PostInstall, no VerifyCommand
	if _, ok := attemptOutcome(spec, "winget", true, "", 0, nil); !ok {
		t.Fatal("no detector + clean exit should be success")
	}
	if oc, ok := attemptOutcome(spec, "winget", false, "boom", asExit(0x8A150006), nil); ok || oc.Kind != InstallLaunchFailed {
		t.Fatalf("no detector + shellexec failure should classify as launch failure, got %v ok=%v", oc.Kind, ok)
	}
}

// A clean exit for a spec with a VerifyCommand (e.g. Git) must be re-verified on
// a refreshed PATH — WinGet updates the registry PATH but not this process's —
// so success is only reported when the tool is actually resolvable now.
func TestAttemptOutcome_VerifyCommandGatesCleanExit(t *testing.T) {
	spec := DependencySpec{DisplayName: "Git", VerifyCommand: "git"}

	orig := verifyInstalledOnPath
	t.Cleanup(func() { verifyInstalledOnPath = orig })

	// Verify succeeds (PATH refresh made git visible) → success.
	var gotCmd string
	verifyInstalledOnPath = func(cmd string) bool { gotCmd = cmd; return true }
	if _, ok := attemptOutcome(spec, "winget", true, "", 0, nil); !ok {
		t.Fatal("clean exit with a passing verify should be success")
	}
	if gotCmd != "git" {
		t.Errorf("verify should be called with the spec's VerifyCommand, got %q", gotCmd)
	}

	// Verify still fails even after refresh → NOT success; explicit
	// verification-failure outcome that points the user at a restart.
	verifyInstalledOnPath = func(string) bool { return false }
	oc, ok := attemptOutcome(spec, "winget", true, "winget: Successfully installed", 0, nil)
	if ok {
		t.Fatal("clean exit with a failing verify must not be reported as success")
	}
	if oc.Kind != InstallOther {
		t.Fatalf("want InstallOther verification failure, got %v", oc.Kind)
	}
	if !strings.Contains(oc.Output, "could not be verified") || !strings.Contains(oc.Output, "restart") {
		t.Errorf("expected a verify-failure diagnostic mentioning a restart, got %q", oc.Output)
	}

	// A non-zero exit is still classified normally, without consulting verify.
	verifyInstalledOnPath = func(string) bool { t.Fatal("verify must not run on a failed exit"); return false }
	if oc, ok := attemptOutcome(spec, "winget", false, "boom", asExit(0x8A150006), nil); ok || oc.Kind != InstallLaunchFailed {
		t.Fatalf("failed exit should classify as launch failure, got %v ok=%v", oc.Kind, ok)
	}
}

func TestMergePathList_DedupsAndDropsEmpties(t *testing.T) {
	sep := string(os.PathListSeparator)
	in := strings.Join([]string{`C:\Git\cmd`, "", `C:\Windows`, `c:\git\cmd`, `C:\Windows`}, sep)
	got := mergePathList(in)
	want := strings.Join([]string{`C:\Git\cmd`, `C:\Windows`}, sep)
	if got != want {
		t.Errorf("mergePathList(%q) = %q, want %q", in, got, want)
	}
}

// Finding 4: when both managers fail, diagnostics aggregate BOTH attempts and
// the user-facing classification prefers the launch failure.
func TestAggregateFailures_CombinesBothManagers(t *testing.T) {
	attempts := []installAttempt{
		{"winget", installOutcome{Kind: InstallOther, ExitCode: 3, Output: "winget network error"}},
		{"scoop", installOutcome{Kind: InstallLaunchFailed, ExitCode: asExit(0x8A150006), Output: "scoop shellexec failed"}},
	}
	got := aggregateFailures(attempts)
	if got.Kind != InstallLaunchFailed {
		t.Errorf("expected the launch-failure classification to win, got %v", got.Kind)
	}
	for _, want := range []string{"[winget]", "winget network error", "[scoop]", "scoop shellexec failed"} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("aggregated diagnostics missing %q; got:\n%s", want, got.Output)
		}
	}
}

func TestAggregateFailures_SingleManagerUnchanged(t *testing.T) {
	only := installOutcome{Kind: InstallLaunchFailed, ExitCode: 7, Output: "raw winget tail"}
	got := aggregateFailures([]installAttempt{{"winget", only}})
	if got.Output != "raw winget tail" {
		t.Errorf("single-manager output should be untouched, got %q", got.Output)
	}
}
