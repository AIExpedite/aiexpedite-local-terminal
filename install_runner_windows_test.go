//go:build windows
// +build windows

// File: install_runner_windows_test.go
//
// Tests for WinGet install-failure classification.
package main

import "testing"

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
