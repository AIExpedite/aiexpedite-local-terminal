//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

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

func TestCopyInstallTree(t *testing.T) {
	src := t.TempDir()
	sub := filepath.Join(src, "subdir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "app.exe"), []byte("exe-content"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "data.txt"), []byte("data-content"), 0o644); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "dest")
	if err := copyInstallTree(src, dest); err != nil {
		t.Fatalf("copyInstallTree failed: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(dest, "app.exe"))
	if err != nil || string(content) != "exe-content" {
		t.Fatalf("copied exe content = %q, err = %v", string(content), err)
	}

	dataContent, err := os.ReadFile(filepath.Join(dest, "subdir", "data.txt"))
	if err != nil || string(dataContent) != "data-content" {
		t.Fatalf("copied data content = %q, err = %v", string(dataContent), err)
	}
}
