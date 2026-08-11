package main

import (
	"path/filepath"
	"testing"
)

// isolateTestUserHome confines home- and installer-based executable/config
// discovery to a test-owned directory. HOME alone is insufficient on Windows,
// where os.UserHomeDir reads USERPROFILE, and an empty PATH alone is
// insufficient for Grok because production deliberately probes GROK_BIN_DIR.
func isolateTestUserHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GROK_BIN_DIR", filepath.Join(home, ".grok", "bin"))
}
