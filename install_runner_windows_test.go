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
		stderr   string
		want     InstallFailureKind
	}{
		{"success", 0, "", InstallOK},
		{"file not found code", asExit(wingetErrFileNotFound), "", InstallLaunchFailed},
		{"exec install failed code", asExit(wingetErrInstallFailed), "", InstallLaunchFailed},
		{"hash mismatch code", asExit(wingetErrHashMismatch), "", InstallLaunchFailed},
		{"missing from path code", asExit(wingetErrMissingFromPath), "", InstallLaunchFailed},
		{"stderr fallback file", 1, "The system cannot find the file specified", InstallLaunchFailed},
		{"stderr fallback launch", 1, "Installer failed to launch", InstallLaunchFailed},
		{"stderr fallback hash", 1, "Installer hash does not match", InstallLaunchFailed},
		{"unknown non-zero", 3, "network unreachable", InstallOther},
		{"non-zero empty stderr", 9009, "", InstallOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyInstallFailure(tc.exitCode, tc.stderr); got != tc.want {
				t.Errorf("classifyInstallFailure(%d, %q) = %v, want %v", tc.exitCode, tc.stderr, got, tc.want)
			}
		})
	}
}

func TestClassifyInstallFailure_CaseInsensitiveStderr(t *testing.T) {
	if got := classifyInstallFailure(1, "CANNOT FIND THE FILE on disk"); got != InstallLaunchFailed {
		t.Errorf("expected case-insensitive stderr match, got %v", got)
	}
}
