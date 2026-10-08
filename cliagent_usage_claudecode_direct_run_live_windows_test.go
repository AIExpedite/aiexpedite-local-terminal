//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_claudecode_direct_run_live_windows_test.go — the live
   acceptance gate for "Claude Code direct CLI smoke leaves utilization stale".
   --------------------------------------------------------------------------
   OPT-IN, never part of CI. It spends one real `claude -p` turn and one real
   OAuth usage request against the device's own Claude login, so it runs only
   when

       AIEXPEDITE_LIVE_CLAUDE=1

   is set:

       go test -run TestClaudeDirectRunLive -count=1 -v -timeout 10m .

   It builds the agent binary, installs the SessionEnd hook into the device's
   REAL Claude config dir (the login is never copied: a copied refresh token
   that rotates would sign the user's CLI out), runs `claude -p` from a plain
   PowerShell child that carries no run-owner marker — the controller's smoke
   shape — and requires a numeric reading newer than the run's end within
   120 s. settings.json is restored byte for byte afterwards.

   It logs timestamps and types only, never a value, path, account or prompt.
   ------------------------------------------------------------------------ */

func TestClaudeDirectRunLive(t *testing.T) {
	if os.Getenv("AIEXPEDITE_LIVE_CLAUDE") != "1" {
		t.Skip("set AIEXPEDITE_LIVE_CLAUDE=1 to run the live Claude direct-run gate")
	}
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not on PATH")
	}

	// The hook command must name a real agent binary: os.Executable is the test
	// binary here, which would run the whole suite on every session end.
	agent := filepath.Join(t.TempDir(), "aiexpedite-terminal.exe")
	if out, err := exec.Command("go", "build", "-o", agent, ".").CombinedOutput(); err != nil {
		t.Fatalf("building the agent binary failed (%d bytes of output)", len(out))
	}
	prevExe := claudeHookExecutable
	claudeHookExecutable = func() (string, error) { return agent, nil }
	t.Cleanup(func() { claudeHookExecutable = prevExe })

	cache := filepath.Join(t.TempDir(), "claude_rate_limits.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)

	home, _ := os.UserHomeDir()
	settingsPath := claudeSettingsPathIfPresent(home)
	if settingsPath == "" {
		t.Skip("no Claude config dir on this device")
	}
	original, readErr := os.ReadFile(settingsPath)
	t.Cleanup(func() {
		if readErr != nil {
			_ = os.Remove(settingsPath)
			return
		}
		if err := writeSettingsAtomic(settingsPath, original); err != nil {
			t.Errorf("restoring settings.json failed: %T", err)
		}
	})
	if _, err := ensureClaudeRunEndHook(home); err != nil {
		t.Fatalf("installing the run-end hook failed: %T", err)
	}

	resetClaudeUsageProbeGate()
	SetClaudeUsageProbeDisabled(false)
	resetClaudeObservedDebtAdoption()
	t.Cleanup(func() {
		stopClaudeRunDebtRetry()
		resetClaudeUsageProbeGate()
		resetClaudeObservedDebtAdoption()
	})

	// A plain PowerShell child, as the maintenance controller launches its
	// smoke: no run-owner marker, no nested-session markers.
	var env []string
	for _, e := range os.Environ() {
		upper := strings.ToUpper(e)
		if strings.HasPrefix(upper, claudeRunOwnerEnv+"=") || strings.HasPrefix(upper, "CLAUDECODE=") ||
			strings.HasPrefix(upper, "CLAUDE_CODE_ENTRYPOINT=") {
			continue
		}
		env = append(env, e)
	}
	ps := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		"& "+powerShellDoubleQuote(claudePath)+" -p 'Reply with the single word OK.'")
	ps.Env = env
	ps.Dir = t.TempDir()
	start := time.Now()
	if out, err := ps.CombinedOutput(); err != nil {
		t.Fatalf("claude -p failed after %v (%d bytes of output)", time.Since(start).Round(time.Second), len(out))
	}
	runEnd := time.Now()
	t.Logf("claude -p finished in %v", runEnd.Sub(start).Round(time.Second))

	deadline := runEnd.Add(120 * time.Second)
	for time.Now().Before(deadline) {
		adoptObservedClaudeRunDebt(time.Now())
		if snap, ok := loadClaudeRateLimitSnapshot(cache); ok {
			for window, b := range snap.Buckets {
				// Only the probe this test adopts can write here (the cache is
				// private), and it goes out after the run ended.
				if b.hasObservedUsage() && b.ObservedAtMs >= runEnd.UnixMilli() {
					t.Logf("numeric %s reading (%T) observed %v after the run ended", window, b.UsedPercentage,
						time.UnixMilli(b.ObservedAtMs).Sub(runEnd).Round(time.Second))
					return
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("no numeric reading newer than the run within 120 s")
}
