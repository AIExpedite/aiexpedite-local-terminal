// Real-process tests for the spawn ledger's identity guard (session_ledger.go,
// process_identity_*.go, process_ownership_*.go). Uses the test binary as the
// CLI (TEST_MOCK_CLI_MODE, session_integration_test.go).
package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// startLedgerChild starts the test binary in a mock mode, owning its process
// group on Unix exactly like a session spawn does.
func startLedgerChild(t *testing.T, mode string, extraEnv ...string) (*exec.Cmd, io.WriteCloser) {
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
	ownProcessGroup(cmd)
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

func recordedProcess(t *testing.T, p *os.Process) ledgerProcess {
	t.Helper()
	var token string
	var err error
	// The start time is readable as soon as the process exists; retry
	// briefly for /proc on a loaded CI runner.
	for i := 0; i < 50; i++ {
		if token, err = processStartToken(p.Pid); err == nil && token != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || token == "" {
		t.Fatalf("processStartToken(%d): %q, %v", p.Pid, token, err)
	}
	return ledgerProcess{PID: p.Pid, StartTime: token, PGID: processGroupOf(p)}
}

func waitForProbe(t *testing.T, rec ledgerProcess, want processProbeResult, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if got := probeRecordedProcess(rec); got == want {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("probe(%d) = %v, want %v", rec.PID, got, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func unsupportedIdentityPlatform() bool {
	return runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin"
}

// TestStartTimeGuard_ReusedPIDIsNeverKilledOrCounted: a live process at the
// recorded PID with ANOTHER start time is a stranger that was handed a reused
// PID. It must read as "not ours" (gone) and must never be signalled.
func TestStartTimeGuard_ReusedPIDIsNeverKilledOrCounted(t *testing.T) {
	if unsupportedIdentityPlatform() {
		t.Skip("no start-time reader on this platform")
	}
	cmd, _ := startLedgerChild(t, "grok-smoke-hang")
	rec := recordedProcess(t, cmd.Process)

	if got := probeRecordedProcess(rec); got != processOurs {
		t.Fatalf("live recorded process: probe = %v, want ours", got)
	}

	reused := rec
	reused.StartTime = rec.StartTime + "1" // same PID, different start time
	if got := probeRecordedProcess(reused); got != processGone {
		t.Fatalf("reused PID must not count as ours: probe = %v", got)
	}
	if got := endRecordedProcess(reused, time.Second); got != processGone {
		t.Fatalf("reused PID: end = %v, want gone (untouched)", got)
	}
	time.Sleep(200 * time.Millisecond)
	if got := probeRecordedProcess(rec); got != processOurs {
		t.Fatalf("the process at a reused PID was killed (probe = %v)", got)
	}

	// With the matching start time it is ours, and it is ended.
	if got := endRecordedProcess(rec, 5*time.Second); got != processGone {
		t.Fatalf("end(ours) = %v, want gone", got)
	}
	waitForProbe(t, rec, processGone, 5*time.Second)
}

func TestStartTimeGuard_ExitedPIDIsGone(t *testing.T) {
	if unsupportedIdentityPlatform() {
		t.Skip("no start-time reader on this platform")
	}
	self, _ := os.Executable()
	cmd := exec.Command(self, "-test.run=^$")
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	rec := recordedProcess(t, cmd.Process)
	_ = cmd.Wait()
	if got := probeRecordedProcess(rec); got != processGone {
		t.Fatalf("exited process: probe = %v, want gone", got)
	}
	if got := probeRecordedProcess(ledgerProcess{PID: rec.PID, StartTime: "bogus"}); got != processUnknown {
		t.Fatalf("a record in another platform's format must read unknown, got %v", got)
	}
}

// TestBootReapEndsAnEarlierBootsProcess drives the whole boot path with a
// real process: the ledger of an earlier boot recorded it, the new boot ends
// it and reports its session reaped.
func TestBootReapEndsAnEarlierBootsProcess(t *testing.T) {
	if unsupportedIdentityPlatform() {
		t.Skip("no start-time reader on this platform")
	}
	cmd, _ := startLedgerChild(t, "grok-smoke-hang")
	rec := recordedProcess(t, cmd.Process)

	dir := t.TempDir()
	writeLedgerFixture(t, dir, ledgerFileData{
		BootID:  "boot-old",
		Entries: []*ledgerEntry{{SessionID: "orphaned", BootID: "boot-old", PIDs: []ledgerProcess{rec}}},
	})
	path := filepath.Join(dir, spawnLedgerFileName)
	l := newRealLedger(t, path, "boot-new")
	l.RunBootReap()

	r := l.Report(context.Background())
	waitForProbe(t, rec, processGone, 5*time.Second)
	if r.PreviousBootID != "boot-old" {
		t.Fatalf("report = %+v", r)
	}
	if runtime.GOOS == "windows" {
		// Not started suspended in a job: its descendants are never vouched
		// for, so ending it proves the session nothing (see the contained
		// case in process_ownership_windows_test.go).
		if len(r.SessionsReaped) != 0 {
			t.Fatalf("an uncontained Windows session was reported reaped: %+v", r)
		}
		return
	}
	if len(r.SessionsReaped) != 1 || r.SessionsReaped[0] != "orphaned" {
		t.Fatalf("report = %+v", r)
	}
}

// readPIDFile waits for the mock descendant to write its PID.
func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("descendant never wrote %s", path)
	return 0
}
