package main

// Acceptance for a COLD binary after an update. The first spawn of a freshly
// written CLI (an agent or CLI self-update) can take seconds on Windows while
// real-time AV scans it, so its first `--version` hits machineInfoProbeTimeout.
// That cannot be reproduced on Linux or macOS CI, so each case stands in for the
// cold child through the probe seam (versionProbeCombinedOutput) and lets the
// shipped classification and cache run on the timeout.
//
// For Claude Code, Codex and Antigravity, on a direct run and a
// terminal-managed run, the run's utilization must come back numeric and
// observed after the run, AND the version a gather reads must recover on the
// next gather instead of staying "" until the binary changes again.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// helperColdVersionProbeOnce makes the FIRST `--version` of the binary named
// `name` time out, as a cold binary under an AV scan does, and answers every
// later one with `warm` (or the real child when warm is nil). Returns how many
// cold probes were served.
func helperColdVersionProbeOnce(t *testing.T, name, warm string) *atomic.Int64 {
	t.Helper()
	resetVersionProbeCache()
	t.Cleanup(resetVersionProbeCache)
	prev := versionProbeCombinedOutput
	t.Cleanup(func() { versionProbeCombinedOutput = prev })
	var cold atomic.Int64
	versionProbeCombinedOutput = func(c *exec.Cmd) ([]byte, error) {
		if strings.TrimSuffix(filepath.Base(c.Path), ".exe") != name {
			return prev(c)
		}
		if cold.CompareAndSwap(0, 1) {
			return nil, context.DeadlineExceeded
		}
		if warm != "" {
			return []byte(warm + "\n"), nil
		}
		return prev(c)
	}
	return &cold
}

// helperRequireVersionRecovers is the gather's side: the cold probe already
// happened (cold == 1), and the next probe of the same, unchanged binary must
// answer rather than replay a cached "".
func helperRequireVersionRecovers(t *testing.T, cold *atomic.Int64, probe func() string, want string) {
	t.Helper()
	if cold.Load() != 1 {
		t.Fatalf("cold probes=%d, want the first --version after the update to have timed out", cold.Load())
	}
	if got := probe(); got != want {
		t.Fatalf("version after the cold start = %q, want %q (a timed-out probe was cached)", got, want)
	}
}

// helperFakeCLIBinary writes an executable stand-in for a CLI whose version is
// answered by the probe seam, so only its (path, mtime, size) matters.
func helperFakeCLIBinary(t *testing.T, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("updated build"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

/* ------------------------------- Claude Code ------------------------------- */

const claudeColdStartVersion = "2.1.300 (Claude Code)"

func TestPostUpdateColdStart_ClaudeCode(t *testing.T) {
	for _, path := range []string{"direct run", "terminal-managed run"} {
		t.Run(path, func(t *testing.T) {
			skipIfUnsupportedOS(t)
			cache, healthy := armPostUpdateEndpoint(t)
			seedStaleClaudeObservation(t, cache)
			cold := helperColdVersionProbeOnce(t, "claude", claudeColdStartVersion)

			var binDir string
			if path == "direct run" {
				binDir = installMockClaude(t, "claude-heartbeat-result")
				m := NewClaudeNativeManager(nil)
				id := "claude-cold-start-direct"
				if err := m.Start(id, binDir, nil, "hello", "ws", "uid", func(resultMsg) {}, nil); err != nil {
					t.Fatalf("Start: %v", err)
				}
				t.Cleanup(func() { _ = m.End(id) })
			} else {
				sm, id := startManagedClaudeSession(t, "claude-heartbeat-result")
				t.Cleanup(func() { _ = sm.EndSession(id) })
				binDir = filepath.Dir(resolveExecutable("claude"))
			}
			binary := filepath.Join(binDir, "claude")
			if runtime.GOOS == "windows" {
				binary += ".exe"
			}

			payAfterUpdate(t, cache, healthy)
			// The new process's first gather meets the cold binary.
			if got := cachedProbeVersion(binary); got != "" {
				t.Fatalf("cold probe = %q, want the timeout's \"\"", got)
			}
			requireNumericClaudeRows(t)
			helperRequireVersionRecovers(t, cold, func() string { return cachedProbeVersion(binary) }, claudeColdStartVersion)
		})
	}
}

/* ---------------------------------- Codex ---------------------------------- */

const codexColdStartVersion = "codex-cli 0.130.0"

// Both run paths settle through codexUsageRunStarted / codexUsageRunSettled (the
// direct app-server turn and a terminal `codex` session alike), so one table
// covers both, as in cliagent_usage_codex_post_update_freshness_test.go.
func TestPostUpdateColdStart_Codex(t *testing.T) {
	for _, source := range []string{"direct app-server turn", "terminal codex session"} {
		t.Run(source, func(t *testing.T) {
			now := time.Now()
			runStart := now.Add(-2 * time.Minute)
			f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
			f.seedPreRunReading(t, now.Add(-time.Hour), now)
			f.advanceCursorPast(t, now.Add(-10*time.Second), now)
			binary := helperFakeCLIBinary(t, "codex")
			cold := helperColdVersionProbeOnce(t, "codex", codexColdStartVersion)

			codexUsageRunStarted(runStart)
			codexUsageRunSettled(runStart)
			waitCodexUsageRefreshIdle(t)
			if snap := f.snapshot(t); snap.RefreshOwedAtMs == 0 {
				t.Fatalf("the settle must record the debt: %+v", snap)
			}

			// The update replaces the agent; the new process's first gather
			// meets the cold binary, then Codex flushes the run's telemetry.
			simulateCodexAgentRestart(t)
			if got := codexProbeVersion(binary); got != "" {
				t.Fatalf("cold probe = %q, want the timeout's \"\"", got)
			}
			writeCodexRunRollout(t, f.home, "cold-start", runStart, runStart.Add(time.Minute), runStart.Add(70*time.Second), true,
				[]map[string]any{codexRateLimitFrame(47, 52, now)})
			payOwedCodexUsageRefresh()
			drainCodexRunDebtLadder(t)

			if got := codexObservedAt(t, f); got.UnixMilli() < runStart.UnixMilli() {
				t.Fatalf("observedAt %s still predates the run floor %s after the update", got, runStart)
			}
			if snap := f.snapshot(t); snap.RefreshOwedAtMs != 0 {
				t.Fatalf("a paid debt must clear: %+v", snap)
			}
			helperRequireVersionRecovers(t, cold, func() string { return codexProbeVersion(binary) }, codexColdStartVersion)
		})
	}
}

/* ------------------------------- Antigravity ------------------------------- */

// On a CSRF-gated build the run's refresh is paid by the Code Assist read,
// whose User-Agent carries the build antigravityCodeAssistBuildVersion probes
// off the installed `agy`. A cold first probe must cost that one read its
// build string (the default stands in), never pin "" for the next ones.
func TestPostUpdateColdStart_Antigravity(t *testing.T) {
	for _, path := range []string{"direct run", "terminal-managed run"} {
		t.Run(path, func(t *testing.T) {
			home, cache, stale, executable := helperGatedRunFreshnessFixture(t)
			cold := helperColdVersionProbeOnce(t, "agy", "")
			// Cleanups run last-registered-first: wait out the debt worker (it
			// probes the build version) BEFORE the seam above is restored.
			t.Cleanup(antigravityUsageRefreshWaitIdle)

			if path == "direct run" {
				session := &AntigravityNativeSession{ID: "cold-start-native", status: "idle"}
				out, _, exitCode, timedOut, _, runErr := NewAntigravityNativeManager(nil).runOneShot(
					session, t.TempDir(), executable, "hello", "", 30*time.Second, "test:antigravity-cold-start")
				if runErr != nil || exitCode != 0 || timedOut {
					t.Fatalf("stub turn failed: out=%q exit=%d timedOut=%v err=%v", out, exitCode, timedOut, runErr)
				}
			} else {
				if _, _, err := captureSession(t, "antigravity-quota-gated", "agy", []string{"do the thing"}, ""); err != nil {
					t.Fatalf("captureSession: %v", err)
				}
				executable = resolveExecutable("agy")
			}
			helperAwaitCaptureStopped(t, "capture poller did not stop after the run", "the run never armed a quota capture")

			helperAwaitPaidRefresh(t, cache, stale)
			if _, observed := helperParsedObservedAt(t, home, time.Now()); !observed.After(stale) {
				t.Fatalf("observedAt=%s did not advance past the stale %s after a cold start", observed, stale)
			}
			// Whoever asked first (the gate, or the paying read's User-Agent) met
			// the cold binary; the next Code Assist read must name the build.
			if cold.Load() == 0 {
				antigravityCodeAssistBuildVersion("")
			}
			helperRequireVersionRecovers(t, cold, func() string { return antigravityCodeAssistBuildVersion("") }, "agy version 1.2.3")
		})
	}
}

// helperRunLateStartedCapture arms a capture, lets the opening discovery ramp
// run out with no server (the run's process is still inside a cold Start), and
// only THEN starts the run's language server and reports the process. The
// steady tick is pinned at an hour, so only a ramp restarted by the start can
// capture the reading. Returns what the poller logged.
func helperRunLateStartedCapture(t *testing.T) (cache, logged string) {
	t.Helper()
	home, cache := helperIsolateAntigravityCapture(t, "1h")
	base := filepath.Join(home, ".gemini", "antigravity-cli")
	testExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	logged = captureStdout(t, func() {
		discoveryBefore := antigravityCaptureDiscoveryProbes.Load()
		capture := startAntigravityQuotaCapture("late start")
		deadline := time.Now().Add(30 * time.Second)
		for antigravityCaptureDiscoveryProbes.Load()-discoveryBefore < int64(antigravityCaptureInitialPolls) {
			if time.Now().After(deadline) {
				t.Fatal("the opening discovery ramp never ran out")
			}
			time.Sleep(20 * time.Millisecond)
		}

		// The run's child finally starts: its server comes up and advertises its
		// port, and the transport reports the process.
		helperStartCaptureServer(t, base, helperQuotaJSON, helperStatusJSON)
		child := exec.Command(testExe)
		child.Env = setEnvVar(os.Environ(), mockCLIEnvVar, mockCLIWarmupMode)
		if err := child.Start(); err != nil {
			t.Fatalf("start child: %v", err)
		}
		capture.SetPID(child.Process.Pid)
		_ = child.Wait()

		helperAwaitSnapshot(t, cache, time.Time{}, "a reading from the late-started run")
		helperStopCapture(t, capture.Finish)
	})
	return cache, logged
}

func TestPostUpdateColdStart_AntigravityLateStartedRunIsCaptured(t *testing.T) {
	_, logged := helperRunLateStartedCapture(t)
	if !strings.Contains(logged, "Capture discovery ramp restarted") {
		t.Errorf("the ramp restart was not logged:\n%s", logged)
	}
	if got := antigravityCaptureSnapshots.Load(); got == 0 {
		t.Fatal("no snapshot persisted from the late-started run")
	}
}
