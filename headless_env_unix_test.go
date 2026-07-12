//go:build !windows
// +build !windows

// File: headless_env_unix_test.go
// Unix-specific assertion that hardenNonAgentCommand detaches the child from any
// controlling terminal (Setsid) — the mechanism that stops git/ssh/credential
// helpers from reaching /dev/tty and hanging.
package main

import (
	"os/exec"
	"testing"
)

func TestHardenNonAgentCommand_DetachesControllingTTY(t *testing.T) {
	c := exec.Command("git", "status")
	hardenNonAgentCommand(c, "git status")
	if c.SysProcAttr == nil || !c.SysProcAttr.Setsid {
		t.Fatalf("expected Setsid (no controlling terminal) to be set; SysProcAttr=%+v", c.SysProcAttr)
	}
}
