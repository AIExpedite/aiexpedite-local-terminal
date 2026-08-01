package main

import "testing"

// TestHandleInstallChoice verifies the setup-required dialog's button semantics:
//   - Yes    -> proceed with the automatic install, no side effects.
//   - No      -> open the download page and exit the flow (manual path).
//   - Cancel  -> abort with no browser/exit side effects.
func TestHandleInstallChoice(t *testing.T) {
	tests := []struct {
		name         string
		choice       InstallChoice
		wantProceed  bool
		wantErr      bool
		wantBrowser  bool
		wantExit     bool
		wantExitCode int
	}{
		{
			name:        "Yes proceeds with automatic install",
			choice:      InstallYes,
			wantProceed: true,
		},
		{
			name:         "No opens download page and exits",
			choice:       InstallNo,
			wantProceed:  false,
			wantBrowser:  true,
			wantExit:     true,
			wantExitCode: 0,
		},
		{
			name:        "Cancel aborts with no side effects",
			choice:      InstallCancel,
			wantProceed: false,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var browsedURL string
			var browserCalled bool
			var exitCalled bool
			var exitCode int

			origBrowser, origExit := openBrowserFn, exitFn
			defer func() { openBrowserFn, exitFn = origBrowser, origExit }()

			openBrowserFn = func(url string) {
				browserCalled = true
				browsedURL = url
			}
			exitFn = func(code int) {
				exitCalled = true
				exitCode = code
			}

			proceed, err := handleInstallChoice(tt.choice)

			if proceed != tt.wantProceed {
				t.Errorf("proceed = %v, want %v", proceed, tt.wantProceed)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if browserCalled != tt.wantBrowser {
				t.Errorf("browser called = %v, want %v", browserCalled, tt.wantBrowser)
			}
			if tt.wantBrowser && browsedURL != ttydDownloadURL {
				t.Errorf("browsed URL = %q, want %q", browsedURL, ttydDownloadURL)
			}
			if exitCalled != tt.wantExit {
				t.Errorf("exit called = %v, want %v", exitCalled, tt.wantExit)
			}
			if tt.wantExit && exitCode != tt.wantExitCode {
				t.Errorf("exit code = %d, want %d", exitCode, tt.wantExitCode)
			}
		})
	}
}
