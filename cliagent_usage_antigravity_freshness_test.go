package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The exact staleness the CLI-maintenance smoke reported: the Antigravity quota
// file never moved off this instant no matter how many runs succeeded.
const helperStaleObservedAt = "2026-08-28T05:13:22Z"

// helperSeedStaleAntigravityCache writes the observed-stale snapshot the smoke
// found, so every test below asserts an ADVANCE rather than merely a presence.
func helperSeedStaleAntigravityCache(t *testing.T, cache string) {
	t.Helper()
	body, err := json.Marshal(antigravityQuotaSnapshot{
		ObservedAt:         helperStaleObservedAt,
		AccountFingerprint: fingerprintAccount("antigravity", "ada@example.com"),
		Account:            "ada@example.com",
		Plan:               "Pro",
		Buckets: []antigravityQuotaBucket{
			{Group: "Gemini Models", Window: "weekly", RemainingFraction: 0.4, ResetTime: "2126-08-14T00:00:00Z"},
		},
	})
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(cache, body, 0o600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
}

// helperMockAgyOnPath copies the test binary into a temp dir under the `agy`
// name and puts that dir first on PATH, so both exec.LookPath and a direct
// executable path resolve to the mock CLI (see runMockCLI).
func helperMockAgyOnPath(t *testing.T, mode string) (dir, executable string) {
	t.Helper()
	testExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	dir = t.TempDir()
	name := "agy"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	executable = filepath.Join(dir, name)
	if err := copyTestBinary(testExe, executable); err != nil {
		t.Fatalf("copy test binary: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(mockCLIEnvVar, mode)
	return dir, executable
}

// helperParsedObservedAt renders the provider entry the CLI Agents card reads
// and returns the observation time its metrics carry.
func helperParsedObservedAt(t *testing.T, home string, now time.Time) (*cliAgentUsage, time.Time) {
	t.Helper()
	usage, ok := antigravityUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("Parse failed")
	}
	if len(usage.Metrics) == 0 {
		t.Fatalf("no metrics rendered")
	}
	observed, err := time.Parse(time.RFC3339, usage.Metrics[0].ObservedAt)
	if err != nil {
		t.Fatalf("metric observedAt=%q is not RFC3339: %v", usage.Metrics[0].ObservedAt, err)
	}
	return usage, observed
}

// Direct path: a native `agy` turn must leave behind a reading the NEXT usage
// refresh can publish, even though that refresh runs with no server up. This is
// the regression the whole change exists for.
//
// The quota server is owned by the SPAWNED PROCESS (mock mode
// antigravity-quota-server), not by the test, so it vanishes when the turn ends
// exactly as a real agy language server does. A capture that only probed after
// the child was reaped — or that waited a full steady tick before its first
// probe — finds nothing here and this test fails.
func TestAntigravityFreshness_NativeTurnAdvancesObservedAt(t *testing.T) {
	// No interval override: this asserts the SHIPPED capture cadence samples a
	// turn shorter than antigravityCapturePollInterval.
	home, cache := helperIsolateAntigravityCapture(t, "")
	helperSeedStaleAntigravityCache(t, cache)
	base := filepath.Join(home, ".gemini", "antigravity-cli")
	t.Setenv(mockAgyQuotaBaseEnv, base)
	_, executable := helperMockAgyOnPath(t, "antigravity-quota-server")

	stale, err := time.Parse(time.RFC3339, helperStaleObservedAt)
	if err != nil {
		t.Fatalf("parse seed: %v", err)
	}

	session := &AntigravityNativeSession{ID: "freshness-native", status: "idle"}
	out, _, exitCode, timedOut, _, runErr := NewAntigravityNativeManager(nil).runOneShot(
		session, t.TempDir(), executable, "hello", "", 30*time.Second, "test:antigravity-freshness")
	if runErr != nil || exitCode != 0 || timedOut {
		t.Fatalf("stub turn failed: out=%q exit=%d timedOut=%v err=%v", out, exitCode, timedOut, runErr)
	}

	// runOneShot released the capture on return; wait for the final probe.
	if stopped := antigravityCaptureStopped(); stopped != nil {
		select {
		case <-stopped:
		case <-time.After(30 * time.Second):
			t.Fatal("capture poller did not stop after the turn")
		}
	} else {
		t.Fatal("the native turn never armed a quota capture")
	}
	if got := antigravityCaptureArms.Load(); got != 1 {
		t.Errorf("arms=%d, want exactly one per turn", got)
	}
	if got := antigravityCaptureFinishes.Load(); got != 1 {
		t.Errorf("finishes=%d, want exactly one per turn", got)
	}

	// Nothing is listening any more — the child took its server with it — so
	// Parse can only replay what capture persisted DURING the run.
	usage, observed := helperParsedObservedAt(t, home, time.Now())
	if !observed.After(stale) {
		t.Fatalf("observedAt=%s did not advance past the stale %s", observed, stale)
	}
	if usage.Account != "ada@example.com" {
		t.Errorf("account=%q, want the producing identity", usage.Account)
	}
}

// Terminal-managed path: the same guarantee for an `agy` session started through
// SessionManager (session_start / terminal.execute.command), whose capture is
// armed at spawn and released in waitForExit. Same child-owned quota server, so
// the reading can only have been taken while the session's process was alive.
func TestAntigravityFreshness_TerminalManagedSessionAdvancesObservedAt(t *testing.T) {
	home, cache := helperIsolateAntigravityCapture(t, "")
	helperSeedStaleAntigravityCache(t, cache)
	t.Setenv(mockAgyQuotaBaseEnv, filepath.Join(home, ".gemini", "antigravity-cli"))

	stale, err := time.Parse(time.RFC3339, helperStaleObservedAt)
	if err != nil {
		t.Fatalf("parse seed: %v", err)
	}

	if _, _, err := captureSession(t, "antigravity-quota-server", "agy", []string{"do the thing"}, ""); err != nil {
		t.Fatalf("captureSession: %v", err)
	}
	if stopped := antigravityCaptureStopped(); stopped != nil {
		select {
		case <-stopped:
		case <-time.After(30 * time.Second):
			t.Fatal("capture poller did not stop after the session ended")
		}
	} else {
		t.Fatal("the terminal-managed session never armed a quota capture")
	}
	if got := antigravityCaptureArms.Load(); got != 1 {
		t.Errorf("arms=%d, want exactly one per session", got)
	}

	if _, observed := helperParsedObservedAt(t, home, time.Now()); !observed.After(stale) {
		t.Fatalf("observedAt=%s did not advance past the stale %s", observed, stale)
	}
}

// Non-PTY `execute` path: a tty=false terminal.execute of `agy --print …` runs
// through runLocalCommand (runLocalCommandUnix on macOS/Linux; persistent
// PowerShell, the dedicated CLI-agent process or the one-shot fallback on
// Windows), none of which touch the PTY or session hooks. It is a real
// Antigravity run and must refresh the quota just the same.
func TestAntigravityFreshness_NonTTYExecuteAdvancesObservedAt(t *testing.T) {
	home, cache := helperIsolateAntigravityCapture(t, "")
	helperSeedStaleAntigravityCache(t, cache)
	t.Setenv(mockAgyQuotaBaseEnv, filepath.Join(home, ".gemini", "antigravity-cli"))
	_, executable := helperMockAgyOnPath(t, "antigravity-quota-server")

	stale, err := time.Parse(time.RFC3339, helperStaleObservedAt)
	if err != nil {
		t.Fatalf("parse seed: %v", err)
	}

	out, execErr := executeTerminalCommand(nil, commandMsg{
		Command:   executable,
		Args:      []string{"--print", "hello"},
		Cwd:       t.TempDir(),
		TimeoutMs: 30000,
		Tty:       false,
	})
	if execErr != nil {
		t.Fatalf("execute failed: %v (output=%q)", execErr, out)
	}
	if !strings.Contains(out, "quota-server turn done") {
		t.Fatalf("mock agy did not run to completion: %q", out)
	}

	if stopped := antigravityCaptureStopped(); stopped != nil {
		select {
		case <-stopped:
		case <-time.After(30 * time.Second):
			t.Fatal("capture poller did not stop after the execute returned")
		}
	} else {
		t.Fatal("a tty=false execute of agy never armed a quota capture")
	}
	if got := antigravityCaptureArms.Load(); got != 1 {
		t.Errorf("arms=%d, want exactly one per execute", got)
	}
	if got := antigravityCaptureFinishes.Load(); got != 1 {
		t.Errorf("finishes=%d, want exactly one per execute", got)
	}

	if _, observed := helperParsedObservedAt(t, home, time.Now()); !observed.After(stale) {
		t.Fatalf("observedAt=%s did not advance past the stale %s", observed, stale)
	}
}

// The negative for the execute path: an ordinary tty=false command must not pay
// for loopback probes.
func TestAntigravityFreshness_NonTTYExecuteOfOtherCommandArmsNothing(t *testing.T) {
	helperIsolateAntigravityCapture(t, "")
	_, executable := helperMockAgyOnPath(t, "no-prompt-immediate-exit")
	// Same binary, renamed so the classifier sees a non-Antigravity program.
	other := filepath.Join(filepath.Dir(executable), "notagy")
	if runtime.GOOS == "windows" {
		other += ".exe"
	}
	if err := copyTestBinary(executable, other); err != nil {
		t.Fatalf("copy mock: %v", err)
	}

	if _, err := executeTerminalCommand(nil, commandMsg{
		Command:   other,
		Args:      []string{"--version"},
		Cwd:       t.TempDir(),
		TimeoutMs: 30000,
	}); err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if got := antigravityCaptureArms.Load(); got != 0 {
		t.Errorf("arms=%d, want 0 for a non-agy execute", got)
	}
}

// A non-Antigravity session must not arm the poller: capture is scoped to runs
// that actually start a language server.
func TestAntigravityFreshness_NonAntigravitySessionArmsNothing(t *testing.T) {
	helperIsolateAntigravityCapture(t, "20ms")

	if _, _, err := captureSession(t, "no-prompt-immediate-exit", "git", []string{"--version"}, ""); err != nil {
		t.Fatalf("captureSession: %v", err)
	}
	if got := antigravityCaptureArms.Load(); got != 0 {
		t.Errorf("arms=%d, want 0 for a non-agy session", got)
	}
}

// Post-update freshness: `agy` can self-update from the legacy ~/.agy tree onto
// ~/.gemini/antigravity-cli mid-flight. Bases are re-resolved on every attempt,
// so the next tick must find the relocated server rather than going stale until
// the agent restarts.
func TestAntigravityFreshness_SurvivesAnInstallTreeMigrationMidCapture(t *testing.T) {
	home, cache := helperIsolateAntigravityCapture(t, "20ms")
	helperSeedStaleAntigravityCache(t, cache)
	legacy := filepath.Join(home, ".agy")
	helperWriteJSON(t, filepath.Join(legacy, "config.json"), map[string]any{})
	old := helperStartCaptureServer(t, legacy, helperQuotaJSON, helperStatusJSON)

	finish := startAntigravityQuotaCapture("migration run")
	defer helperStopCapture(t, finish)
	helperAwaitSnapshot(t, cache, time.Time{}, "a capture from the legacy install")

	// The update lands: the old server is gone, its config with it, and a new
	// log under the modern tree names the relocated server (a different account
	// so the source of the next reading is unambiguous).
	old.srv.Close()
	if err := os.Remove(filepath.Join(legacy, "config.json")); err != nil {
		t.Fatalf("remove legacy config: %v", err)
	}
	modern := filepath.Join(home, ".gemini", "antigravity-cli")
	helperWriteJSON(t, filepath.Join(modern, "settings.json"), map[string]any{})
	helperStartCaptureServer(t, modern, helperQuotaJSON,
		`{"userStatus":{"email":"grace@example.com","planStatus":{"planInfo":{"planName":"Ultra"}}}}`)

	deadline := time.Now().Add(30 * time.Second)
	for {
		var snap antigravityQuotaSnapshot
		if readJSONFile(cache, &snap) && snap.Account == "grace@example.com" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("capture never followed the install tree migration")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Signed propagation: an advanced observedAt has to survive receipt
// normalization and the per-metric bounds check, or the backend would keep
// showing the old figure however fresh the local capture is.
func TestAntigravityFreshness_AdvancedObservedAtSurvivesTheSignedRefresh(t *testing.T) {
	home, cache := helperIsolateAntigravityCapture(t, "20ms")
	helperSeedStaleAntigravityCache(t, cache)
	server := helperStartCaptureServer(t, filepath.Join(home, ".gemini", "antigravity-cli"),
		helperQuotaJSON, helperStatusJSON)

	stale, err := time.Parse(time.RFC3339, helperStaleObservedAt)
	if err != nil {
		t.Fatalf("parse seed: %v", err)
	}

	finish := startAntigravityQuotaCapture("signed refresh run")
	helperAwaitSnapshot(t, cache, stale, "a capture newer than the stale seed")
	helperStopCapture(t, finish)
	server.srv.Close()

	usage, observed := helperParsedObservedAt(t, home, time.Now())
	if !observed.After(stale) {
		t.Fatalf("observedAt=%s did not advance past the stale %s", observed, stale)
	}

	receipt, normalized, _, err := prepareCLIUsageRefreshResult(
		"refresh-secret", "refresh-1", time.Now().Unix(), true, []cliAgentUsage{*usage}, nil)
	if err != nil {
		t.Fatalf("signed refresh rejected the captured usage: %v", err)
	}
	if receipt == "" {
		t.Fatalf("no receipt produced")
	}
	if len(normalized) != 1 || len(normalized[0].Metrics) == 0 {
		t.Fatalf("normalized usage lost its metrics: %+v", normalized)
	}
	for _, metric := range normalized[0].Metrics {
		got, parseErr := time.Parse(time.RFC3339, metric.ObservedAt)
		if parseErr != nil {
			t.Fatalf("normalized metric observedAt=%q is not RFC3339: %v", metric.ObservedAt, parseErr)
		}
		if !got.After(stale) {
			t.Errorf("metric %q observedAt=%s did not survive as an advance", metric.Label, got)
		}
	}
}

// commandRunsAntigravity is the gate every terminal-managed spawn path shares.
// It must see through the `bash -c "agy …"` wrapper terminal-service ships, and
// must never fire for a command that merely mentions agy in its arguments.
func TestCommandRunsAntigravity(t *testing.T) {
	cases := []struct {
		name    string
		command string
		args    []string
		want    bool
	}{
		{"direct agy", "agy", []string{"--print", "hi"}, true},
		{"absolute path", "/usr/local/bin/agy", []string{"--version"}, true},
		{"windows shim", `C:\tools\agy.exe`, nil, true},
		{"antigravity alias", "antigravity", []string{"--print", "hi"}, true},
		{"shell wrapped", "bash", []string{"-c", "agy --print hi"}, true},
		{"shell wrapped with path", "sh", []string{"-c", "  /opt/agy --print hi"}, true},
		{"shell wrapped other program", "bash", []string{"-c", "git commit -m 'ask agy'"}, false},
		{"argument mentions agy", "git", []string{"log", "--grep", "agy"}, false},
		{"empty shell payload", "bash", []string{"-c", ""}, false},
		{"unrelated", "claude", []string{"-p", "hi"}, false},
	}
	for _, tc := range cases {
		if got := commandRunsAntigravity(tc.command, tc.args); got != tc.want {
			t.Errorf("%s: commandRunsAntigravity(%q, %v) = %v, want %v",
				tc.name, tc.command, tc.args, got, tc.want)
		}
	}
}

// Concurrent turns share one poller, so several `agy` runs overlapping must
// still leave exactly one poller behind and one fresh reading.
func TestAntigravityFreshness_ConcurrentRunsShareOnePoller(t *testing.T) {
	home, cache := helperIsolateAntigravityCapture(t, "20ms")
	helperSeedStaleAntigravityCache(t, cache)
	helperStartCaptureServer(t, filepath.Join(home, ".gemini", "antigravity-cli"),
		helperQuotaJSON, helperStatusJSON)

	stale, err := time.Parse(time.RFC3339, helperStaleObservedAt)
	if err != nil {
		t.Fatalf("parse seed: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			finish := startAntigravityQuotaCapture(fmt.Sprintf("run %d", n))
			time.Sleep(time.Duration(50+n*30) * time.Millisecond)
			finish()
		}(i)
	}
	wg.Wait()

	if stopped := antigravityCaptureStopped(); stopped != nil {
		select {
		case <-stopped:
		case <-time.After(30 * time.Second):
			t.Fatal("the shared poller did not stop after the last run")
		}
	}
	if got := antigravityCaptureFinishes.Load(); got != 4 {
		t.Errorf("finishes=%d, want one per run", got)
	}

	var snap antigravityQuotaSnapshot
	if !readJSONFile(cache, &snap) {
		t.Fatalf("cache unreadable")
	}
	observed, err := time.Parse(time.RFC3339, snap.ObservedAt)
	if err != nil {
		t.Fatalf("observedAt=%q is not RFC3339: %v", snap.ObservedAt, err)
	}
	if !observed.After(stale) {
		t.Errorf("observedAt=%s did not advance past the stale %s", observed, stale)
	}
	if strings.TrimSpace(snap.Account) != "ada@example.com" {
		t.Errorf("account=%q, want the server-named identity", snap.Account)
	}
}
