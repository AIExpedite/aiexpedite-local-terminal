//go:build windows

package main

import "testing"

func TestRelocationDestinationMatchesInstallerDirectory(t *testing.T) {
	oldDisplayName := EnvDisplayName
	t.Cleanup(func() { EnvDisplayName = oldDisplayName })

	EnvDisplayName = "AI Expedite (Dev)"
	got := windowsPerUserInstallDir(`C:\Users\tester\AppData\Local`)
	want := `C:\Users\tester\AppData\Local\AI Expedite (Dev)`
	if got != want {
		t.Fatalf("relocation destination = %q, want installer directory %q", got, want)
	}
}
