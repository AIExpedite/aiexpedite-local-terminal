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
   cliagent_usage_opencode_direct_run_live_windows_test.go — the live
   acceptance gate for "OpenCode fresh direct-run usage does not reach the
   parser ledger".
   --------------------------------------------------------------------------
   OPT-IN, never part of CI. It spends one real `opencode run` turn against the
   device's own OpenCode login, so it runs only when

       AIEXPEDITE_LIVE_OPENCODE=1

   is set:

       go test -run TestOpenCodeDirectRunLive -count=1 -v -timeout 10m .

   AIEXPEDITE_LIVE_OPENCODE_MODEL=<provider/model> passes --model, for a
   device whose default model has no working provider.

   It runs `opencode run` from a plain PowerShell child — the direct smoke's
   shape, owing no debt — and reads the device's REAL opencode.db (read-only)
   into an isolated ledger (AIEXPEDITE_OPENCODE_USAGE_CACHE). Today's bucket
   must move to a step that ended after the run started within 120 s of the
   run's end, one openCodeDirectScanShared per poll.

   It logs timestamps, layout labels and booleans only, never a count, id,
   path or prompt.
   ------------------------------------------------------------------------ */

func TestOpenCodeDirectRunLive(t *testing.T) {
	if os.Getenv("AIEXPEDITE_LIVE_OPENCODE") != "1" {
		t.Skip("set AIEXPEDITE_LIVE_OPENCODE=1 to run the live OpenCode direct-run gate")
	}
	// TestMain sandboxes the profile; OpenCode keeps its login, provider config
	// and store in the real one. The ledger stays isolated below, and the agent
	// config dir stays in the sandbox.
	if realHomeAtStartup != "" {
		t.Setenv("HOME", realHomeAtStartup)
		t.Setenv("USERPROFILE", realHomeAtStartup)
		if realAppDataAtStartup != "" {
			t.Setenv("APPDATA", realAppDataAtStartup)
		}
		if strings.HasPrefix(os.Getenv("XDG_CONFIG_HOME"), testSandboxDir) {
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(realHomeAtStartup, ".config"))
		}
	}
	openCodePath, err := exec.LookPath("opencode")
	if err != nil {
		t.Skip("opencode is not on PATH")
	}
	if layout, _ := openCodeStoreLayout(); layout != openCodeStoreLayoutSQLite {
		t.Skipf("no OpenCode SQLite store on this device (layout %q)", layout)
	}

	t.Setenv("AIEXPEDITE_OPENCODE_USAGE_CACHE", filepath.Join(t.TempDir(), "opencode_usage.json"))
	prevEnabled := openCodeDirectScanEnabled
	openCodeDirectScanEnabled = true
	resetOpenCodeDirectScan()
	t.Cleanup(func() {
		openCodeDirectScanEnabled = prevEnabled
		resetOpenCodeDirectScan()
	})
	newest := func() int64 {
		today, _ := openCodeLocalDay(time.Now())
		var at int64
		for _, b := range loadOpenCodeUsageLedger().Buckets {
			if b.LocalDate == today && b.ObservedAtMs > at {
				at = b.ObservedAtMs
			}
		}
		return at
	}
	baseline := openCodeDirectScanShared(openCodeDirectClickBudget)
	t.Logf("baseline scan: %s", baseline)

	// A plain PowerShell child, as the maintenance controller launches its
	// smoke: the agent arms nothing for it.
	command := "& " + powerShellDoubleQuote(openCodePath) + " run"
	if model := os.Getenv("AIEXPEDITE_LIVE_OPENCODE_MODEL"); model != "" {
		command += " --model " + powerShellDoubleQuote(model)
	}
	ps := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", command+" 'Reply with the single word OK.'")
	ps.Dir = t.TempDir()
	start := time.Now()
	if out, err := ps.CombinedOutput(); err != nil {
		t.Fatalf("opencode run failed after %v (%d bytes of output)", time.Since(start).Round(time.Second), len(out))
	}
	runEnd := time.Now()
	t.Logf("opencode run finished in %v", runEnd.Sub(start).Round(time.Second))

	deadline := runEnd.Add(120 * time.Second)
	for time.Now().Before(deadline) {
		label := openCodeDirectScanShared(openCodeDirectClickBudget)
		if at := newest(); at >= start.UnixMilli() {
			layout := ""
			if c := loadOpenCodeUsageLedger().DirectCoverage; c != nil {
				layout = c.Layout
			}
			t.Logf("scan %s: today's bucket moved %v after the run ended (coverage layout %q)", label,
				time.Since(runEnd).Round(time.Second), layout)
			return
		}
		time.Sleep(2 * time.Second)
	}
	c := loadOpenCodeUsageLedger().DirectCoverage
	t.Fatalf("today's bucket did not move within 120 s of the run (coverage present %v)", c != nil)
}
