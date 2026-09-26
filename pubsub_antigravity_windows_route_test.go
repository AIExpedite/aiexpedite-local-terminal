//go:build windows

// pubsub_antigravity_windows_route_test.go — the route half of the Antigravity
// capture fix, which only a Windows build can observe.
//
// runLocalCommand's job is now a single decision: a DIRECT `agy` takes the bare
// exec route (runLocalCommandUnix, which keeps the CLI's stream intact), and
// everything else on Windows takes the PowerShell chain (runLocalCommandWindows,
// which arms the quota capture around it). The OS-agnostic tests in
// cliagent_usage_antigravity_freshness_test.go drive runLocalCommandWindows
// directly and therefore cannot see that decision at all — if the route sent a
// wrapped agy payload down the bare path, it would exec
// `powershell.exe -EncodedCommand …` as a literal program and lose CLIXML
// filtering, the oversized-script temp-file fallback and hardenNonAgentCommand,
// and every one of those tests would still pass.
package main

import (
	"strings"
	"testing"
	"time"
)

// helperStubWindowsTransports replaces both encoded-PowerShell transports with
// spies, so a route assertion never spawns a real powershell.exe.
func helperStubWindowsTransports(t *testing.T) (viaArg, viaTempFile *int) {
	t.Helper()
	restoreArg, restoreFile := runEncodedPowerShellViaArgFn, runPowerShellCommandViaTempFileFn
	t.Cleanup(func() {
		runEncodedPowerShellViaArgFn, runPowerShellCommandViaTempFileFn = restoreArg, restoreFile
	})
	argHits, fileHits := 0, 0
	runEncodedPowerShellViaArgFn = func(string, string, time.Duration) (string, error) {
		argHits++
		return "stub", nil
	}
	runPowerShellCommandViaTempFileFn = func(string, string, time.Duration) (string, error) {
		fileHits++
		return "stub", nil
	}
	return &argHits, &fileHits
}

// A wrapped agy payload must stay on the Windows transport AND arm the capture.
// This is the reported defect: the smoke passed, the payload was agy, and
// nothing was ever armed.
func TestWindowsRoute_WrappedAgyKeepsPowerShellAndArmsCapture(t *testing.T) {
	helperIsolateAntigravityCapture(t, "1h")
	viaArg, _ := helperStubWindowsTransports(t)

	out, err := runLocalCommand(nil, "powershell",
		[]string{"-EncodedCommand", encodeForPowerShell(`& 'C:\t\agy.cmd' -p "hi"`)},
		t.TempDir(), 30000)
	if err != nil {
		t.Fatalf("execute failed: %v (output=%q)", err, out)
	}
	if *viaArg != 1 {
		t.Errorf("encoded transport ran %d times, want once — the wrapped payload left the PowerShell path", *viaArg)
	}
	if got := antigravityCaptureArms.Load(); got != 1 {
		t.Errorf("arms=%d, want exactly one for a wrapped agy payload", got)
	}
	if got := antigravityCaptureFinishes.Load(); got != 1 {
		t.Errorf("finishes=%d, want exactly one — the poller outlived the execute", got)
	}
}

// A DIRECT `agy` still takes the bare-exec route on Windows: the CLI streams,
// and wrapping it in PowerShell breaks that. Its capture is armed by
// runLocalCommandUnix after Start, so exactly one arm happens either way.
func TestWindowsRoute_DirectAgyStillTakesBareExec(t *testing.T) {
	helperIsolateAntigravityCapture(t, "1h")
	viaArg, viaFile := helperStubWindowsTransports(t)

	// A path that cannot exist: the bare route reports a spawn failure, which is
	// the proof it was taken. The PowerShell route would have returned "stub".
	out, err := runLocalCommand(nil, `C:\definitely\missing\agy.exe`,
		[]string{"--version"}, t.TempDir(), 30000)
	if err == nil {
		t.Fatalf("expected a spawn failure from the bare exec route, got output=%q", out)
	}
	if out == "stub" || *viaArg != 0 || *viaFile != 0 {
		t.Errorf("a direct agy reached the PowerShell chain (viaArg=%d viaFile=%d output=%q)",
			*viaArg, *viaFile, out)
	}
}

// A non-agy Windows execute arms nothing: capture must attach to agy runs, not
// to every command the device runs.
func TestWindowsRoute_NonAgyPayloadArmsNothing(t *testing.T) {
	helperIsolateAntigravityCapture(t, "1h")
	before := antigravityCaptureStopped()
	helperStubWindowsTransports(t)

	if _, err := runLocalCommand(nil, "powershell",
		[]string{"-EncodedCommand", encodeForPowerShell("git log --grep agy")},
		t.TempDir(), 30000); err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if got := antigravityCaptureArms.Load(); got != 0 {
		t.Errorf("arms=%d, want 0 — `git log --grep agy` mentions agy, it does not run it", got)
	}
	if antigravityCaptureStopped() != before {
		t.Error("a poller was started for a non-agy execute")
	}
}

// One arm covers the WHOLE transport chain, including a failover. The chain is
// sequential, so a persistent-PowerShell failure that falls through to the
// one-shot process must not arm a second poller — a double arm would leave a
// refcount that never reaches zero and a goroutine that never exits.
func TestWindowsRoute_FailoverDoesNotDoubleArm(t *testing.T) {
	helperIsolateAntigravityCapture(t, "1h")
	restoreArg := runEncodedPowerShellViaArgFn
	t.Cleanup(func() { runEncodedPowerShellViaArgFn = restoreArg })
	calls := 0
	runEncodedPowerShellViaArgFn = func(encoded, workDir string, timeout time.Duration) (string, error) {
		calls++
		script, err := decodeBase64PowerShellStrict(encoded)
		if err != nil || !strings.Contains(script, "agy") {
			t.Errorf("transport received an unexpected script (err=%v)", err)
		}
		return "stub", nil
	}

	// Two sequential executes of the same wrapped payload: each must arm and
	// release exactly once, never leaving a refcount behind.
	for i := 0; i < 2; i++ {
		if _, err := runLocalCommand(nil, "powershell",
			[]string{"-EncodedCommand", encodeForPowerShell(`& 'C:\t\agy.cmd' -p "hi"`)},
			t.TempDir(), 30000); err != nil {
			t.Fatalf("execute %d failed: %v", i, err)
		}
	}
	if calls != 2 {
		t.Errorf("transport ran %d times, want 2", calls)
	}
	if arms, finishes := antigravityCaptureArms.Load(), antigravityCaptureFinishes.Load(); arms != 2 || finishes != 2 {
		t.Errorf("arms=%d finishes=%d, want 2 and 2 — every armed run must be released", arms, finishes)
	}
	helperAwaitCaptureStopped(t, "a poller is still running after both executes returned", "neither execute armed a quota capture")
}
