//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// An npm `claude.cmd` shim cannot be started by CreateProcess directly, and a
// read-only start never routes its argv through cmd.exe: it is refused with a
// reason before anything spawns.
func TestCheckReadOnlyExecutable_RefusesWindowsBatchShims(t *testing.T) {
	cwd := t.TempDir()
	dir := t.TempDir()
	for _, name := range []string{"claude.cmd", "claude.CMD", "claude.bat"} {
		shim := filepath.Join(dir, name)
		if err := os.WriteFile(shim, []byte("@echo off\r\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		err := readOnlySessionErrorFrom(checkReadOnlyExecutable("claude", shim, cwd))
		if err == nil {
			t.Fatalf("checkReadOnlyExecutable(%q) accepted a batch launcher", shim)
		}
	}
	native := filepath.Join(dir, "claude.exe")
	if err := os.WriteFile(native, []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkReadOnlyExecutable("claude", native, cwd); err != nil {
		t.Fatalf("the native executable was refused: %v", err)
	}
}
