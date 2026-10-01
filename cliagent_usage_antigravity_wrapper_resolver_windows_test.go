//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The wrapped Windows execute chain, for real: a PowerShell wrapper runs a
// mock agy.exe (the copied test binary) that logs its own PID block with a
// quota refusal. The resolver must find that agy through the real
// ScanProcessAncestryChecked while it runs, and the settle must then record
// the run's exhaustion evidence.

func helperWrappedAgyFixture(t *testing.T, sleep string) string {
	t.Helper()
	helperIsolateAntigravityCapture(t, "1h")
	_, executable := helperMockAgyOnPath(t, "antigravity-pid-block")
	t.Setenv(mockAgyExhaustedEnv, "bob@example.com")
	t.Setenv(mockAgySleepEnv, sleep)
	resetAntigravityExhaustionEvidence()
	t.Cleanup(resetAntigravityExhaustionEvidence)
	return executable
}

func helperBobEvidence() []antigravityExhaustionEvent {
	return antigravityExhaustionEvidence(time.Now(), fingerprintAccount("antigravity", "bob@example.com"))
}

func helperShortResolverTick(t *testing.T) {
	t.Helper()
	orig := antigravityWrapperScanEvery
	antigravityWrapperScanEvery = time.Second
	t.Cleanup(func() { antigravityWrapperScanEvery = orig })
}

// Resolves: `powershell -EncodedCommand` launches agy, which lives long enough
// for a resolver tick; the settle records evidence from agy's own block.
func TestAntigravityWrapperResolver_EncodedPowerShellResolves(t *testing.T) {
	executable := helperWrappedAgyFixture(t, "4s")
	helperShortResolverTick(t)
	script := fmt.Sprintf(`& '%s' --print hello`, executable)
	if out, err := runLocalCommandWindows("powershell",
		[]string{"-EncodedCommand", encodeForPowerShell(script)}, t.TempDir(), time.Minute); err != nil {
		t.Fatalf("wrapped run failed: %v (%s)", err, out)
	}
	antigravityUsageRefreshWaitIdle()
	if len(helperBobEvidence()) != 1 {
		t.Fatal("the wrapped agy's quota refusal was not recorded as evidence")
	}
}

// Fast failure: a run far shorter than the shipped 5 s steady interval is
// still resolved, because the resolver scans at once and through its opening
// ramp, and its quota refusal is recorded.
func TestAntigravityWrapperResolver_ShortRunResolvedByRamp(t *testing.T) {
	executable := helperWrappedAgyFixture(t, "2s")
	script := fmt.Sprintf(`& '%s' --print hello`, executable)
	if out, err := runLocalCommandWindows("powershell",
		[]string{"-EncodedCommand", encodeForPowerShell(script)}, t.TempDir(), time.Minute); err != nil {
		t.Fatalf("wrapped run failed: %v (%s)", err, out)
	}
	antigravityUsageRefreshWaitIdle()
	if len(helperBobEvidence()) != 1 {
		t.Error("a wrapped run shorter than one steady resolver interval recorded no evidence")
	}
}

// File-mode: `powershell -File` with a nested PowerShell intermediate resolves
// through it to the agy grandchild.
func TestAntigravityWrapperResolver_FileModeThroughAnIntermediate(t *testing.T) {
	executable := helperWrappedAgyFixture(t, "4s")
	helperShortResolverTick(t)
	ps1 := filepath.Join(t.TempDir(), "launch.ps1")
	inner := fmt.Sprintf(`powershell -NoProfile -NonInteractive -Command "& '%s' --print hello"`, executable)
	if err := os.WriteFile(ps1, []byte(inner), 0o600); err != nil {
		t.Fatal(err)
	}
	capture := startAntigravityQuotaCapture("file-mode test")
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", ps1)
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	capture.SetWrapper(cmd.Process.Pid)
	_ = cmd.Wait()
	capture.Finish()
	antigravityUsageRefreshWaitIdle()
	if len(helperBobEvidence()) != 1 {
		t.Error("the file-mode launcher's agy grandchild was not resolved")
	}
}

// Persistent PowerShell: the wrapper is the long-lived host, and only an agy
// started after the capture floor is taken. The host is shared process-wide
// and keeps the environment it was started with, so the mock's environment is
// set inside the command itself.
func TestAntigravityWrapperResolver_PersistentPowerShell(t *testing.T) {
	executable := helperWrappedAgyFixture(t, "4s")
	helperShortResolverTick(t)
	ps, err := GetPowerShell()
	if err != nil {
		t.Skipf("persistent PowerShell unavailable: %v", err)
	}
	home, _ := os.UserHomeDir()
	vars := map[string]string{
		mockCLIEnvVar: "antigravity-pid-block", mockAgyExhaustedEnv: "bob@example.com",
		mockAgySleepEnv: "4s", "USERPROFILE": home, "HOME": home,
	}
	script := ""
	for name, value := range vars {
		script += fmt.Sprintf("$env:%s = '%s'; ", name, value)
	}
	script += fmt.Sprintf("& '%s' --print hello; ", executable)
	for _, name := range []string{mockCLIEnvVar, mockAgyExhaustedEnv, mockAgySleepEnv} {
		script += fmt.Sprintf("Remove-Item Env:%s -ErrorAction SilentlyContinue; ", name)
	}

	capture := startAntigravityQuotaCapture("persistent test")
	capture.SetWrapper(ps.HostPID())
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if out, err := ps.Execute(ctx, script, t.TempDir()); err != nil {
		t.Fatalf("persistent-PowerShell run failed: %v (%s)", err, out)
	}
	capture.Finish()
	antigravityUsageRefreshWaitIdle()
	if len(helperBobEvidence()) != 1 {
		t.Error("the agy the persistent host ran was not resolved")
	}
}
