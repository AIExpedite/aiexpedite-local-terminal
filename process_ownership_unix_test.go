//go:build !windows

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestOwnProcessGroup_ChildLeadsItsGroup: every CLI session process leads
// its own process group, so the group can be signalled as a unit.
func TestOwnProcessGroup_ChildLeadsItsGroup(t *testing.T) {
	cmd, _ := startLedgerChild(t, "grok-smoke-hang")
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if pgid != cmd.Process.Pid {
		t.Fatalf("pgid = %d, want the child's own pid %d", pgid, cmd.Process.Pid)
	}
	if got := processGroupOf(cmd.Process); got != cmd.Process.Pid {
		t.Fatalf("processGroupOf = %d", got)
	}
	// A process that merely belongs to another group reports none.
	if self := os.Getpid(); mustGetpgid(t, self) != self && processGroupOf(&os.Process{Pid: self}) != 0 {
		t.Fatalf("processGroupOf reported a group the process does not lead")
	}
}

func mustGetpgid(t *testing.T, pid int) int {
	t.Helper()
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatal(err)
	}
	return pgid
}

// TestOwnProcessGroup_LeavesSetsidAlone: Setpgid on a Setsid child would make
// the exec fail; a Setsid child already leads its group.
func TestOwnProcessGroup_LeavesSetsidAlone(t *testing.T) {
	cmd := exec.Command("true")
	detachControllingTTY(cmd)
	ownProcessGroup(cmd)
	if cmd.SysProcAttr.Setpgid {
		t.Fatalf("Setpgid set on a Setsid child")
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("Setsid child failed to run: %v", err)
	}

	plain := exec.Command("true")
	ownProcessGroup(plain)
	if !plain.SysProcAttr.Setpgid {
		t.Fatalf("Setpgid not set")
	}
}

// TestEndRecordedProcess_EndsTheWholeGroup: the boot reap signals the group a
// recorded leader owns, so a descendant the CLI started dies with it.
func TestEndRecordedProcess_EndsTheWholeGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd, stdin := startLedgerChild(t, "ledger-tree-on-stdin", mockSessionKillChildPidEnv, pidFile)
	root := recordedProcess(t, cmd.Process)
	if root.PGID != cmd.Process.Pid {
		t.Fatalf("root does not lead its group: %+v", root)
	}
	if _, err := stdin.Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	childPID := readPIDFile(t, pidFile)
	child := recordedProcess(t, proc(childPID))
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	if mustGetpgid(t, childPID) != root.PGID {
		t.Fatalf("descendant is not in the CLI's group")
	}

	if got := endRecordedProcess(root, 5*time.Second); got != processGone {
		t.Fatalf("end(root) = %v", got)
	}
	waitForProbe(t, child, processGone, 10*time.Second)
}

// TestBootReap_LeaderGoneGroupAliveIsNotReaped: the CLI leader died but a tool
// it started still runs in its group. The session is not provably gone, so it
// is never reported reaped, and the tool (not a recorded PID) is not touched.
func TestBootReap_LeaderGoneGroupAliveIsNotReaped(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd, stdin := startLedgerChild(t, "ledger-tree-on-stdin", mockSessionKillChildPidEnv, pidFile)
	root := recordedProcess(t, cmd.Process)
	if _, err := stdin.Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	childPID := readPIDFile(t, pidFile)
	child := recordedProcess(t, proc(childPID))
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })

	// Only the leader dies.
	if err := syscall.Kill(root.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitForProbe(t, root, processGone, 5*time.Second)

	dir := t.TempDir()
	writeLedgerFixture(t, dir, ledgerFileData{
		BootID:  "boot-old",
		Entries: []*ledgerEntry{{SessionID: "leaky", BootID: "boot-old", PIDs: []ledgerProcess{root}}},
	})
	path := filepath.Join(dir, spawnLedgerFileName)
	l := newRealLedger(t, path, "boot-new")
	l.RunBootReap()
	if r := l.Report(context.Background()); len(r.SessionsReaped) != 0 {
		t.Fatalf("a session with a live group member was reported reaped: %+v", r)
	}
	if got := probeRecordedProcess(child); got != processOurs {
		t.Fatalf("the unrecorded group member was touched (probe = %v)", got)
	}
}

// TestUnixNeverVouchesForDescendants: an empty process group does not prove
// a setsid'd descendant gone, so Unix never vouches for descendants.
func TestUnixNeverVouchesForDescendants(t *testing.T) {
	cmd, _ := startLedgerChild(t, "grok-smoke-hang")
	rec := recordedProcess(t, cmd.Process)
	_ = cmd.Process.Kill()
	waitForProbe(t, rec, processGone, 5*time.Second)
	if got := probeRecordedDescendants(rec); got != processUnknown {
		t.Fatalf("descendants = %v, want unknown", got)
	}
}
