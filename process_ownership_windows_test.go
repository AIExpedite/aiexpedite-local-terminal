//go:build windows

package main

import (
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procIsProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

func processInJob(t *testing.T, pid int, job windows.Handle) bool {
	t.Helper()
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		t.Fatalf("OpenProcess(%d): %v", pid, err)
	}
	defer windows.CloseHandle(h)
	var result int32
	r, _, callErr := procIsProcessInJob.Call(uintptr(h), uintptr(job), uintptr(unsafe.Pointer(&result)))
	if r == 0 {
		t.Fatalf("IsProcessInJob: %v", callErr)
	}
	return result != 0
}

// TestSessionJob_KillOnCloseEndsTheWholeTree is the direct fix for the
// 2026-09-25 orphans: when the only handle to the job closes (the agent
// died), Windows ends the CLI AND the descendants it started afterwards.
func TestSessionJob_KillOnCloseEndsTheWholeTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd, stdin := startLedgerChild(t, "ledger-tree-on-stdin", mockSessionKillChildPidEnv, pidFile)
	root := recordedProcess(t, cmd.Process)

	job, err := attachSessionJob(cmd.Process)
	if err != nil || job == 0 {
		t.Fatalf("attachSessionJob: %v", err)
	}
	if !processInJob(t, cmd.Process.Pid, windows.Handle(job)) {
		t.Fatalf("CLI process is not in its job")
	}

	// The CLI now starts a child: it inherits the job.
	if _, err := stdin.Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	childPID := readPIDFile(t, pidFile)
	child := recordedProcess(t, proc(childPID))
	if !processInJob(t, childPID, windows.Handle(job)) {
		t.Fatalf("descendant did not inherit the job")
	}

	// Closing the handle WITHOUT clearing the limit is what an agent crash
	// does: both processes end.
	if err := windows.CloseHandle(windows.Handle(job)); err != nil {
		t.Fatal(err)
	}
	waitForProbe(t, root, processGone, 10*time.Second)
	waitForProbe(t, child, processGone, 10*time.Second)
}

// TestSessionJob_ReleaseKeepsCleanEndBehaviour: a normal session end clears
// kill-on-close before closing the handle, so releasing never kills.
func TestSessionJob_ReleaseKeepsCleanEndBehaviour(t *testing.T) {
	cmd, _ := startLedgerChild(t, "grok-smoke-hang")
	rec := recordedProcess(t, cmd.Process)
	job, err := attachSessionJob(cmd.Process)
	if err != nil || job == 0 {
		t.Fatalf("attachSessionJob: %v", err)
	}
	releaseSessionJob(job)
	time.Sleep(300 * time.Millisecond)
	if got := probeRecordedProcess(rec); got != processOurs {
		t.Fatalf("release killed the process (probe = %v)", got)
	}
}

// TestTrackProcessPutsTheSessionInAJob covers the spawn-site wiring.
func TestTrackProcessPutsTheSessionInAJob(t *testing.T) {
	cmd, _ := startLedgerChild(t, "grok-smoke-hang")
	dir := t.TempDir()
	path := filepath.Join(dir, spawnLedgerFileName)
	l := newSpawnLedger(func() string { return path }, "boot-job")
	l.BeginSpawn("s-job")
	l.TrackProcess("s-job", cmd.Process)

	l.mu.Lock()
	job := l.jobs[cmd.Process.Pid]
	l.mu.Unlock()
	if job == 0 {
		t.Fatalf("no job recorded for the session process")
	}
	if !processInJob(t, cmd.Process.Pid, windows.Handle(job)) {
		t.Fatalf("session process not in its job")
	}
	if e := readLedgerFile(t, dir).Entries; len(e) != 1 || len(e[0].PIDs) != 1 || !e[0].PIDs[0].Contained {
		t.Fatalf("a job-owned process must be recorded contained: %+v", e)
	}
	rec := recordedProcess(t, cmd.Process)
	l.ReleaseSession("s-job")
	l.mu.Lock()
	_, still := l.jobs[cmd.Process.Pid]
	l.mu.Unlock()
	if still {
		t.Fatalf("ReleaseSession kept the job handle")
	}
	time.Sleep(200 * time.Millisecond)
	if got := probeRecordedProcess(rec); got != processOurs {
		t.Fatalf("a clean release must not kill (probe = %v)", got)
	}
}
