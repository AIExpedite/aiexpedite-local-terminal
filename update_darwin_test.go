//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRelaunchDarwinBundleWaitsForOpenResult(t *testing.T) {
	binDir := t.TempDir()
	openPath := filepath.Join(binDir, "open")
	script := []byte("#!/bin/sh\necho launch-rejected >&2\nexit 23\n")
	if err := os.WriteFile(openPath, script, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := relaunchDarwinBundle("/Applications/AI Expedite.app")
	if err == nil {
		t.Fatal("non-zero LaunchServices result must fail the handoff")
	}
	if !strings.Contains(err.Error(), "launch-rejected") {
		t.Fatalf("relaunch error should include open output, got %v", err)
	}
}
