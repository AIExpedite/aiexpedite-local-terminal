//go:build linux
// +build linux

package main

// keep_awake_linux.go: an idle-only inhibitor (never `sleep`), ended with the
// agent, and a release that kills and reaps the child.

import (
	"os/exec"
	"reflect"
	"syscall"
	"testing"
)

func TestSystemdInhibitCommandBlocksIdleOnly(t *testing.T) {
	cmd := systemdInhibitCommand()
	want := []string{"systemd-inhibit", "--what=idle", "--mode=block", "--who=AI Expedite",
		"--why=" + keepAwakeReason, "sleep", "infinity"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("argv %q, want %q", cmd.Args, want)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Pdeathsig != syscall.SIGTERM {
		t.Fatal("the inhibitor must end when the agent dies (Pdeathsig)")
	}
}

func TestLinuxKeepAwakeReleaseKillsAndReapsTheChild(t *testing.T) {
	prev := systemdInhibitCommand
	t.Cleanup(func() { systemdInhibitCommand = prev })
	var started *exec.Cmd
	systemdInhibitCommand = func() *exec.Cmd {
		started = exec.Command("sleep", "60")
		return started
	}

	release, err := acquirePlatformKeepAwake(func(err error) { t.Errorf("a released child was reported lost: %v", err) })
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()
	if started.ProcessState == nil {
		t.Fatal("release returned before the child was reaped")
	}
}
