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

// TestReleaseSession_EndsWhatTheSessionLeftInItsGroup: the CLI's own END
// signals only the CLI's PID; a tool it started stayed in its process group
// until the computer restarted. Releasing the session now ends the group,
// whose members carry the session's owner marker.
func TestReleaseSession_EndsWhatTheSessionLeftInItsGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	owner := sessionOwnerValue("boot-leftover", "s-leftover")
	cmd, stdin := startLedgerChild(t, "ledger-tree-on-stdin",
		mockSessionKillChildPidEnv, pidFile, sessionOwnerEnvVar, owner)
	dir := t.TempDir()
	l := newRealLedger(t, filepath.Join(dir, spawnLedgerFileName), "boot-leftover")
	l.BeginSpawn("s-leftover")
	l.TrackProcess("s-leftover", cmd.Process, startedSuspended(cmd))
	root := recordedProcess(t, cmd.Process)
	if _, err := stdin.Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	childPID := readPIDFile(t, pidFile)
	child := recordedProcess(t, proc(childPID))
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })

	// What the END paths do: SIGKILL the CLI's PID only, and reap it.
	if err := syscall.Kill(root.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitForProbe(t, root, processGone, 5*time.Second)
	waitForPIDFree(t, root.PID, 5*time.Second)
	if got := probeRecordedProcess(child); got != processOurs {
		t.Fatalf("the tool should still run before the release (probe = %v)", got)
	}

	l.ReleaseSession("s-leftover")
	waitForProbe(t, child, processGone, 10*time.Second)
}

// TestEndLeftoverProcessTree_AZombieDoesNotShieldALiveLeftover is Codex's
// P2 on #304: an exited, unreaped member has no environment to read, and it
// made the whole cleanup give up, leaving the live, marked tool running.
func TestEndLeftoverProcessTree_AZombieDoesNotShieldALiveLeftover(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	owner := sessionOwnerValue("boot", "s-zombie")
	cmd, stdin := startLedgerChild(t, "ledger-tree-on-stdin",
		mockSessionKillChildPidEnv, pidFile, sessionOwnerEnvVar, owner)
	root := recordedProcess(t, cmd.Process)
	if _, err := stdin.Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	childPID := readPIDFile(t, pidFile)
	child := recordedProcess(t, proc(childPID))
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })

	// A member of the same group that exits and is never reaped: a zombie.
	z := exec.Command("true")
	z.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: root.PGID}
	if err := z.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = z.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		members, err := processGroupMembers(root.PGID)
		if err != nil {
			t.Fatal(err)
		}
		zombie := false
		for _, m := range members {
			if m.PID == z.Process.Pid && m.Exited {
				zombie = true
			}
		}
		if zombie {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("setup: PID %d never showed as an exited group member", z.Process.Pid)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := syscall.Kill(root.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitForProbe(t, root, processGone, 5*time.Second)
	waitForPIDFree(t, root.PID, 5*time.Second)

	// The zombie keeps the group non-empty until it is reaped, so the call
	// may report the group not yet empty; what matters is the live tool.
	_ = endLeftoverProcessTree(root, 0, owner)
	waitForProbe(t, child, processGone, 10*time.Second)
}

// TestEndLeftoverProcessTree_NeverTouchesAnUnmarkedGroup: a group whose
// members do not carry the session's marker is not proven ours, so nothing
// is signalled, even with the recorded leader gone.
func TestEndLeftoverProcessTree_NeverTouchesAnUnmarkedGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd, stdin := startLedgerChild(t, "ledger-tree-on-stdin", mockSessionKillChildPidEnv, pidFile)
	root := recordedProcess(t, cmd.Process)
	if _, err := stdin.Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	childPID := readPIDFile(t, pidFile)
	child := recordedProcess(t, proc(childPID))
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	if err := syscall.Kill(root.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitForProbe(t, root, processGone, 5*time.Second)
	waitForPIDFree(t, root.PID, 5*time.Second)

	if endLeftoverProcessTree(root, 0, sessionOwnerValue("boot", "s")) {
		t.Fatalf("an unmarked group read as ended")
	}
	time.Sleep(200 * time.Millisecond)
	if got := probeRecordedProcess(child); got != processOurs {
		t.Fatalf("an unmarked group member was killed (probe = %v)", got)
	}
}

// TestEndLeftoverProcessTree_NeverTouchesAReusedGroup is Codex's P1 on #304:
// a retained record whose group emptied, and whose id the kernel then handed
// to an unrelated group. The record's start time no longer matches (probe says
// gone), but a live process holds the PID: it is not ours, and neither is its
// group, even when its members happen to carry a marker.
func TestEndLeftoverProcessTree_NeverTouchesAReusedGroup(t *testing.T) {
	owner := sessionOwnerValue("boot", "s")
	cmd, _ := startLedgerChild(t, "grok-smoke-hang", sessionOwnerEnvVar, owner)
	live := recordedProcess(t, cmd.Process)
	stale := live
	stale.StartTime = live.StartTime + "-stale" // the recorded, long-gone leader
	if got := probeRecordedProcess(stale); got != processGone {
		t.Fatalf("setup: a stale start token should probe gone, got %v", got)
	}
	if endLeftoverProcessTree(stale, 0, owner) {
		t.Fatalf("a reused group read as ended")
	}
	time.Sleep(200 * time.Millisecond)
	if got := probeRecordedProcess(live); got != processOurs {
		t.Fatalf("the unrelated live group was killed (probe = %v)", got)
	}
}

// TestEndLeftoverProcessTree_NeverTouchesALiveLeader: the CLI itself is
// still running, so nothing in its group is a leftover yet.
func TestEndLeftoverProcessTree_NeverTouchesALiveLeader(t *testing.T) {
	owner := sessionOwnerValue("boot", "s")
	cmd, _ := startLedgerChild(t, "grok-smoke-hang", sessionOwnerEnvVar, owner)
	rec := recordedProcess(t, cmd.Process)
	if endLeftoverProcessTree(rec, 0, owner) {
		t.Fatalf("a live leader's group read as ended")
	}
	time.Sleep(200 * time.Millisecond)
	if got := probeRecordedProcess(rec); got != processOurs {
		t.Fatalf("a live leader was killed (probe = %v)", got)
	}
}

// waitForPIDFree waits until no process (zombie included) holds pid: the
// test helper's own Wait goroutine reaps the killed leader.
func waitForPIDFree(t *testing.T, pid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for pidAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("PID %d still held after %s", pid, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
