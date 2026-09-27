//go:build windows

package main

import (
	"context"
	"io"
	"os"
	"os/exec"
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

// startOwnedChild starts the test binary in a mock mode exactly like a
// session spawn: prepared by beginSessionSpawn's prepareOwnedStart, i.e.
// created SUSPENDED. The caller must hand it to attachSessionJob /
// TrackProcess, which resumes it.
func startOwnedChild(t *testing.T, mode string, extraEnv ...string) (*exec.Cmd, io.WriteCloser) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self)
	env := setEnvVar(os.Environ(), mockCLIEnvVar, mode)
	for i := 0; i+1 < len(extraEnv); i += 2 {
		env = setEnvVar(env, extraEnv[i], extraEnv[i+1])
	}
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	hideWindow(cmd)
	prepareOwnedStart(cmd)
	if !startedSuspended(cmd) {
		t.Fatalf("prepareOwnedStart did not request a suspended start")
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	})
	return cmd, stdin
}

// TestSessionJob_SuspendedStartIsContainedAndResumed: the process cannot run
// before it is in its job (it writes its pid file only once resumed), is
// resumed by the assignment, and is recorded contained.
func TestSessionJob_SuspendedStartIsContainedAndResumed(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "self.pid")
	cmd, _ := startOwnedChild(t, "grok-smoke-hang", mockGrokSmokeHangPidEnv, pidFile)

	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(pidFile); err == nil {
		t.Fatalf("a suspended session process ran before it was put in its job")
	}

	job, contained, err := attachSessionJob(cmd.Process, true)
	if err != nil || job == 0 || !contained {
		t.Fatalf("attachSessionJob = %v, %v, %v", job, contained, err)
	}
	defer releaseSessionJob(job)
	if !processInJob(t, cmd.Process.Pid, windows.Handle(job)) {
		t.Fatalf("session process is not in its job")
	}
	if pid := readPIDFile(t, pidFile); pid != cmd.Process.Pid {
		t.Fatalf("pid file = %d, want %d", pid, cmd.Process.Pid)
	}
}

// TestSessionJob_KillOnCloseEndsTheWholeTree is the direct fix for the
// 2026-09-25 orphans: when the only handle to the job closes (the agent
// died), Windows ends the CLI AND the descendants it started.
func TestSessionJob_KillOnCloseEndsTheWholeTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd, stdin := startOwnedChild(t, "ledger-tree-on-stdin", mockSessionKillChildPidEnv, pidFile)
	job, contained, err := attachSessionJob(cmd.Process, true)
	if err != nil || job == 0 || !contained {
		t.Fatalf("attachSessionJob = %v, %v, %v", job, contained, err)
	}
	root := recordedProcess(t, cmd.Process)

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
	cmd, _ := startOwnedChild(t, "grok-smoke-hang")
	job, _, err := attachSessionJob(cmd.Process, true)
	if err != nil || job == 0 {
		t.Fatalf("attachSessionJob: %v", err)
	}
	rec := recordedProcess(t, cmd.Process)
	releaseSessionJob(job)
	time.Sleep(300 * time.Millisecond)
	if got := probeRecordedProcess(rec); got != processOurs {
		t.Fatalf("release killed the process (probe = %v)", got)
	}
}

// TestSessionJob_NotSuspendedIsNeverContained: a process that ran before its
// assignment may have children outside the job, so it is never recorded
// contained (its descendants are not vouched for at the next boot).
func TestSessionJob_NotSuspendedIsNeverContained(t *testing.T) {
	cmd, _ := startLedgerChild(t, "grok-smoke-hang")
	job, contained, err := attachSessionJob(cmd.Process, false)
	if err != nil || job == 0 {
		t.Fatalf("attachSessionJob: %v", err)
	}
	defer releaseSessionJob(job)
	if contained {
		t.Fatalf("a process assigned after it ran was recorded contained")
	}
	if probeRecordedDescendants(ledgerProcess{PID: cmd.Process.Pid}) != processUnknown {
		t.Fatalf("an uncontained process vouched for its descendants")
	}
}

// TestSessionJob_ResumeFailureTerminates: a suspended process that cannot
// be resumed is terminated rather than left hanging.
func TestSessionJob_ResumeFailureTerminates(t *testing.T) {
	cmd, _ := startOwnedChild(t, "grok-smoke-hang")
	rec := recordedProcess(t, cmd.Process)
	prev := resumeProcess
	resumeProcess = func(windows.Handle) error { return os.ErrPermission }
	t.Cleanup(func() { resumeProcess = prev })
	if _, _, err := attachSessionJob(cmd.Process, true); err == nil {
		t.Fatalf("expected a resume error")
	}
	waitForProbe(t, rec, processGone, 10*time.Second)
}

// TestTrackProcessPutsTheSessionInAJob covers the spawn-site wiring.
func TestTrackProcessPutsTheSessionInAJob(t *testing.T) {
	cmd, _ := startOwnedChild(t, "grok-smoke-hang")
	dir := t.TempDir()
	path := filepath.Join(dir, spawnLedgerFileName)
	l := newRealLedger(t, path, "boot-job")
	l.BeginSpawn("s-job")
	l.TrackProcess("s-job", cmd.Process, startedSuspended(cmd))

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

// TestBootReap_ContainedWindowsSessionIsReaped: a session whose process was
// assigned to its job before it ran (Contained) is reported reaped once that
// process is gone — the job ended everything it started.
func TestBootReap_ContainedWindowsSessionIsReaped(t *testing.T) {
	cmd, _ := startOwnedChild(t, "grok-smoke-hang")
	job, contained, err := attachSessionJob(cmd.Process, true)
	if err != nil || !contained {
		t.Fatalf("attachSessionJob = %v, %v", contained, err)
	}
	rec := recordedProcess(t, cmd.Process)
	rec.Contained = true
	// The agent "dies": its job handle closes and the job ends the tree.
	_ = windows.CloseHandle(windows.Handle(job))
	waitForProbe(t, rec, processGone, 10*time.Second)

	dir := t.TempDir()
	writeLedgerFixture(t, dir, ledgerFileData{
		BootID:  "boot-old",
		Entries: []*ledgerEntry{{SessionID: "contained", BootID: "boot-old", PIDs: []ledgerProcess{rec}}},
	})
	path := filepath.Join(dir, spawnLedgerFileName)
	l := newRealLedger(t, path, "boot-new")
	l.RunBootReap()
	if r := l.Report(context.Background()); len(r.SessionsReaped) != 1 || r.SessionsReaped[0] != "contained" {
		t.Fatalf("report = %+v", r)
	}
}

// TestProcessTreeGone_JobWithAnEscapedToolIsNotEmpty: an exited per-turn
// leader whose job still holds a tool it started is not "gone".
func TestProcessTreeGone_JobWithAnEscapedToolIsNotEmpty(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd, stdin := startOwnedChild(t, "ledger-tree-on-stdin", mockSessionKillChildPidEnv, pidFile)
	job, _, err := attachSessionJob(cmd.Process, true)
	if err != nil || job == 0 {
		t.Fatalf("attachSessionJob: %v", err)
	}
	root := recordedProcess(t, cmd.Process)
	if _, err := stdin.Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	child := recordedProcess(t, proc(readPIDFile(t, pidFile)))
	_ = cmd.Process.Kill() // only the leader
	waitForProbe(t, root, processGone, 10*time.Second)
	if processTreeGone(root, job) {
		t.Fatalf("a job with a live tool read as empty")
	}
	_ = windows.CloseHandle(windows.Handle(job)) // the agent "dies"
	waitForProbe(t, child, processGone, 10*time.Second)
	if processTreeGone(root, 0) {
		t.Fatalf("no job: nothing can be proven")
	}
}
