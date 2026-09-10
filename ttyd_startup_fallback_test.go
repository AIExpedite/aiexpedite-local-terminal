// Tests for the ttyd startup fallback (ttyd.go, agent.go §1-2). The local web
// terminal is a convenience; the cloud connection is the job. Every way ttyd
// can be unavailable must therefore leave StartAgent running with
// `localTerminal` false — never exit the process, and never leave the flag
// true while nothing is listening on the port.
package main

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// An opt-out on Windows used to os.Exit(0) because ttyd was required. It no
// longer is, and exiting here lands in the very window this change closes —
// after the previous instance announced its shutdown — so the device would sit
// Disconnected. Every outcome must come back as an error instead.
func TestInstallTtydWindowsReturnsOptOutsInsteadOfExiting(t *testing.T) {
	tests := []struct {
		name       string
		choice     InstallChoice
		wantErr    error
		wantDialog bool
	}{
		// Cancel at the permission prompt is cancel only: no browser, no
		// install, and no follow-up dialog.
		{name: "Cancel is silent", choice: InstallCancel, wantErr: errInstallCancelled},
		// "No" opened the download page; tell the user how to finish.
		{name: "No explains the recovery", choice: InstallNo, wantErr: errInstallManual, wantDialog: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer withInstallSeams(t)()

			dialogTitle := ""
			dialogBody := ""
			installPrompt = func(string, string, bool) InstallChoice { return tt.choice }
			installOpenURL = func(string) error { return nil }
			installShowInfo = func(title, body string) { dialogTitle, dialogBody = title, body }

			err := installTtydWindows()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			// errInstallCancelled WRAPS errInstallDeclined, so the quiet path
			// only stays quiet if it is excluded explicitly.
			if got := dialogTitle != ""; got != tt.wantDialog {
				t.Fatalf("info dialog shown = %v, want %v (title %q)", got, tt.wantDialog, dialogTitle)
			}
			if tt.wantDialog && strings.Contains(dialogBody, "required to run") {
				t.Errorf("dialog still claims ttyd is required to run the app: %q", dialogBody)
			}
		})
	}
}

// The startup fallback is only honest if nothing in the ttyd install path can
// terminate the process behind ensureTtyd's back.
func TestTtydInstallNeverExitsTheProcess(t *testing.T) {
	src, err := os.ReadFile("ttyd.go")
	if err != nil {
		t.Fatalf("read ttyd.go: %v", err)
	}
	if strings.Contains(string(src), "os.Exit(") {
		t.Error("ttyd.go exits the process; a missing ttyd must disable only the local terminal")
	}
}

// `localTerminal` — not `ttydCmd` — is what showConnectionInstructions reads,
// so a spawn failure that clears only the handle still advertises a dead
// loopback URL. Pin every disable site to clearing the flag.
func TestTtydSpawnFailureClearsTerminalAvailability(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatalf("read agent.go: %v", err)
	}
	body := string(src)
	if !strings.Contains(body, "ttydCmd = nil") {
		t.Fatal("agent.go no longer clears ttydCmd on a spawn failure; update this guard")
	}
	// Each of the three disable paths (missing binary, busy port, failed
	// spawn) must set the flag the instructions read.
	if got := strings.Count(body, "localTerminal = false"); got < 3 {
		t.Errorf("localTerminal = false appears %d times, want one per disable path (>=3)", got)
	}
	idx := strings.Index(body, "ttydCmd = nil")
	if !strings.Contains(body[idx:min(idx+400, len(body))], "localTerminal = false") {
		t.Error("the ttyd spawn-failure branch clears ttydCmd without clearing localTerminal")
	}
}
