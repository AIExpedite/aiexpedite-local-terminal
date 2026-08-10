package main

import (
	"errors"
	"testing"
)

// TestInstallPromptChoiceSemantics guards the setup-required dialog contract
// after ttyd moved to the shared dependency installer.
func TestInstallPromptChoiceSemantics(t *testing.T) {
	tests := []struct {
		name        string
		choice      InstallChoice
		wantErr     error
		wantBrowser bool
		wantInstall bool
	}{
		{name: "Yes installs automatically", choice: InstallYes, wantInstall: true},
		{name: "No opens download page", choice: InstallNo, wantErr: errInstallManual, wantBrowser: true},
		{name: "Cancel has no side effects", choice: InstallCancel, wantErr: errInstallDeclined},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer withInstallSeams(t)()

			browserCalled := false
			installCalled := false
			installPrompt = func(string, string, bool) InstallChoice { return tt.choice }
			installOpenURL = func(url string) error {
				browserCalled = true
				if url != ttydDownloadURL {
					t.Fatalf("opened URL = %q, want %q", url, ttydDownloadURL)
				}
				return nil
			}
			installExec = func(DependencySpec) installOutcome {
				installCalled = true
				return installOutcome{Kind: InstallOK}
			}

			err := runDependencyInstall(ttydDependencySpec())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if browserCalled != tt.wantBrowser {
				t.Errorf("browser called = %v, want %v", browserCalled, tt.wantBrowser)
			}
			if installCalled != tt.wantInstall {
				t.Errorf("install called = %v, want %v", installCalled, tt.wantInstall)
			}
		})
	}
}
