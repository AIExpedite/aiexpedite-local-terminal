//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLaunchRelocatedDarwinBundleWaitsForOpenResult(t *testing.T) {
	binDir := t.TempDir()
	openPath := filepath.Join(binDir, "open")
	script := []byte("#!/bin/sh\necho relocation-rejected >&2\nexit 23\n")
	if err := os.WriteFile(openPath, script, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	for _, forceNew := range []bool{false, true} {
		err := launchRelocatedDarwinBundle("/Users/test/Applications/AI Expedite.app", forceNew)
		if err == nil {
			t.Fatalf("forceNew=%v: non-zero LaunchServices result must fail relocation", forceNew)
		}
		if !strings.Contains(err.Error(), "relocation-rejected") {
			t.Fatalf("forceNew=%v: error should include open output, got %v", forceNew, err)
		}
	}
}
