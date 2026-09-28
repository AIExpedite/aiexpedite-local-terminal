//go:build windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A `.cmd` launcher (Muse Code's `muse.cmd`) must actually run through
// newOneShotCommand, with a spaced, percent-bearing operand arriving intact.
func TestNewOneShotCommand_LaunchesCmdShim(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "A B")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(dir, "muse.cmd")
	if err := os.WriteFile(shim, []byte("@echo off\r\necho ARGS:%1^|%2^|%~3\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	operand := filepath.Join(dir, "%TEMP% x", "prompt.txt")
	cmd := newOneShotCommand(context.Background(), shim, []string{"exec", "--json", operand}, os.Environ(), dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim launch failed: %v\n%s", err, out)
	}
	want := "ARGS:exec|--json|" + operand
	if got := strings.TrimSpace(string(out)); got != want {
		t.Fatalf("shim saw %q, want %q", got, want)
	}
}
