//go:build !windows

package main

import (
	"bytes"
	"context"
	"os/exec"
	"runtime"
)

// Keyring read for the Antigravity login off Windows: the macOS Keychain item
// (service `gemini`, account `antigravity`) through the same `security` call
// the Claude parser uses, and secret-service on Linux through `secret-tool`.
// Both are bounded by machineInfoProbeTimeout so a locked keychain or an
// access prompt cannot hang the probe; a timeout reads as "no credential".
func readAntigravityKeyringCredential(ctx context.Context) ([]byte, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, machineInfoProbeTimeout)
	defer cancel()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "security", "find-generic-password",
			"-s", antigravityKeyringService, "-a", antigravityKeyringUser, "-w")
	case "linux":
		cmd = exec.CommandContext(ctx, "secret-tool", "lookup",
			"service", antigravityKeyringService, "username", antigravityKeyringUser)
	default:
		return nil, false
	}
	hideWindow(cmd) // no-op off Windows; kept uniform with the other probes
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, false
	}
	return trimmed, true
}
