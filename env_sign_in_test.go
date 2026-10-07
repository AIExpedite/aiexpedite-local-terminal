package main

import (
	"strings"
	"testing"
)

// The not-installed error is a server contract: terminal-service matches
// "<program> is not installed (not found on PATH)" to fail a sign-in whose CLI
// is missing instead of asking the user to run it by hand.
func TestResolveSignInProgramNotInstalledError(t *testing.T) {
	const program = "definitely-not-a-cli-xyz"
	path, err := resolveSignInProgram(program)
	if err == nil {
		t.Fatalf("resolveSignInProgram(%q) = %q, want an error", program, path)
	}
	msg := err.Error()
	if !strings.HasSuffix(msg, signInNotInstalledSuffix) {
		t.Errorf("error %q does not end with %q", msg, signInNotInstalledSuffix)
	}
	if !strings.HasPrefix(msg, program+" ") {
		t.Errorf("error %q does not name the program %q", msg, program)
	}
	if signInNotInstalledSuffix != "is not installed (not found on PATH)" {
		t.Errorf("signInNotInstalledSuffix changed to %q; terminal-service matches the old text", signInNotInstalledSuffix)
	}
}
