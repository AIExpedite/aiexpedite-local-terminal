//go:build linux || darwin

package main

import (
	"os/exec"
	"testing"
	"time"
)

// The Unix ancestry scan used by the Antigravity wrapper resolver reads the
// real process table: the wrapper itself comes first, then its children.
func TestScanProcessAncestryChecked_FindsTheWrappersChild(t *testing.T) {
	// `; true` keeps sh from exec'ing sleep in place, so sleep is a child.
	cmd := exec.Command("sh", "-c", "sleep 5; true")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sh: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	root := cmd.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		procs, ok := ScanProcessAncestryChecked(root)
		if !ok {
			t.Fatal("ScanProcessAncestryChecked reported the table unreadable")
		}
		if len(procs) >= 2 {
			if procs[0].PID != root {
				t.Errorf("first entry pid=%d, want the wrapper %d", procs[0].PID, root)
			}
			if procs[1].ParentPID != root || procs[1].Name != "sleep" {
				t.Errorf("child=%+v, want sleep under %d", procs[1], root)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the wrapper's sleep child never appeared in the scan")
}

func TestScanProcessAncestryChecked_RejectsBadRoot(t *testing.T) {
	if procs, ok := ScanProcessAncestryChecked(0); ok || procs != nil {
		t.Errorf("ScanProcessAncestryChecked(0)=(%v,%v), want (nil,false)", procs, ok)
	}
}

// unixProcessAncestry puts the root first (a shell may have exec'd agy in
// place), then its tree within unixAncestryMaxDepth, and nothing unrelated.
func TestUnixProcessAncestry(t *testing.T) {
	procs := []ProcessInfo{
		{PID: 1, ParentPID: 0, Name: "init"},
		{PID: 10, ParentPID: 1, Name: "bash"},
		{PID: 11, ParentPID: 10, Name: "sh"},
		{PID: 12, ParentPID: 11, Name: "agy"},
		{PID: 20, ParentPID: 1, Name: "agy"},
	}
	got := unixProcessAncestry(procs, 10)
	want := []int{10, 11, 12}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want pids %v", got, want)
	}
	for i, pid := range want {
		if got[i].PID != pid {
			t.Errorf("got[%d].PID=%d, want %d", i, got[i].PID, pid)
		}
	}
	if got := unixProcessAncestry(procs, 99); len(got) != 0 {
		t.Errorf("an exited root returned %+v, want nothing", got)
	}
}
