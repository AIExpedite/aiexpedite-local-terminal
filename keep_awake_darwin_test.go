//go:build darwin
// +build darwin

package main

// keep_awake_darwin.go: the caffeinate child's argv, and a release that kills
// and reaps it. A real idle-sleep cycle cannot be exercised in CI; the
// overnight resume_limit_exceeded metric is the acceptance check for that.

import (
	"os"
	"os/exec"
	"reflect"
	"syscall"
	"testing"
)

func TestCaffeinateCommandHoldsIdleSleepForTheAgentProcess(t *testing.T) {
	cmd := caffeinateCommand(4242)
	want := []string{"caffeinate", "-i", "-w", "4242"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("argv %q, want %q", cmd.Args, want)
	}
}

func TestDarwinKeepAwakeReleaseKillsAndReapsTheChild(t *testing.T) {
	prev := caffeinateCommand
	t.Cleanup(func() { caffeinateCommand = prev })
	var started *exec.Cmd
	caffeinateCommand = func(agentPID int) *exec.Cmd {
		if agentPID != os.Getpid() {
			t.Errorf("waits on pid %d, want the agent's %d", agentPID, os.Getpid())
		}
		started = exec.Command("sleep", "60")
		return started
	}

	release, err := acquirePlatformKeepAwake(func(err error) { t.Errorf("a released child was reported lost: %v", err) })
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	pid := started.Process.Pid
	release()

	if started.ProcessState == nil {
		t.Fatal("release returned before the child was reaped")
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("child %d still running after release", pid)
	}
}
