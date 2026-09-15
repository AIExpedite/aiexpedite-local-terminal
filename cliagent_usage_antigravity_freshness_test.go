package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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

	// runOneShot released the capture on return; wait for poller shutdown.
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

// Capture belongs to a live child, not merely an argv that looks like agy. A
// failed Start must leave no poller behind and must not perform the immediate
// pre-spawn probe that previously raced short successful executions.
func TestAntigravityFreshness_NonTTYExecuteStartFailureArmsNothing(t *testing.T) {
	helperIsolateAntigravityCapture(t, "20ms")
	missing := filepath.Join(t.TempDir(), "agy-missing")
	if runtime.GOOS == "windows" {
		missing += ".exe"
	}

	if _, err := executeTerminalCommand(nil, commandMsg{
		Command:   missing,
		Args:      []string{"--print", "hello"},
		Cwd:       t.TempDir(),
		TimeoutMs: 30000,
	}); err == nil {
		t.Fatal("execute unexpectedly started the missing agy binary")
	}
	if got := antigravityCaptureArms.Load(); got != 0 {
		t.Errorf("arms=%d, want 0 when the child never started", got)
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
	helperRemoveFile(t, filepath.Join(legacy, "config.json"))
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
// helperFileModeLauncher reproduces terminal-service's PowerShell
// `scriptMode: "file"` launcher (commandNormalize.util.js →
// buildFileModeInvocation): the REAL script is a second base64 layer inside the
// launcher, executed from a temp file with -File. A classifier that stops at the
// outer layer sees only the launcher and never the agy inside it.
func helperFileModeLauncher(script string) string {
	launcher := strings.Join([]string{
		`$ErrorActionPreference='Stop';`,
		`$b=[Convert]::FromBase64String('` + encodeForPowerShell(script) + `');`,
		`$s=[Text.Encoding]::Unicode.GetString($b);`,
		`$f=Join-Path $env:TEMP ("aix-"+[Guid]::NewGuid().ToString()+".ps1");`,
		`Set-Content -LiteralPath $f -Value $s -Encoding UTF8;`,
		`try{& 'powershell' -NoProfile -ExecutionPolicy Bypass -File $f;exit $LASTEXITCODE}`,
		`finally{Remove-Item -LiteralPath $f -Force -ErrorAction SilentlyContinue}`,
	}, "")
	return encodeForPowerShell(launcher)
}

// helperPosixFileModeLauncher is the same launcher's POSIX half — the identical
// defect on macOS/Linux, emitted by the same terminal-service function.
func helperPosixFileModeLauncher(script string) string {
	return `f=$(mktemp -t aix-script.XXXXXX) && ` +
		`printf '%s' '` + base64.StdEncoding.EncodeToString([]byte(script)) + `' | base64 -d > "$f" && ` +
		`trap 'rm -f "$f"' EXIT INT TERM && bash "$f"`
}

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

		// Every wrapper transport terminal-service actually emits on Windows.
		// Before these were unwrapped, a green CLI-maintenance smoke on Windows
		// armed no capture at all and freshness never moved.
		{"ksh -lc", "ksh", []string{"-lc", "agy --print hi"}, true},
		{"powershell -Command", "powershell", []string{"-Command", "agy -p hi"}, true},
		{"pwsh.exe -c", "pwsh.exe", []string{"-c", "agy -p hi"}, true},
		{"cmd /c", "cmd", []string{"/c", "agy --version"}, true},
		{"cmd.exe /k", "cmd.exe", []string{"/k", "agy --version"}, true},
		{"powershell -Command with a call operator and a quoted path", "powershell",
			[]string{"-Command", `Set-Location C:\tmp; & 'C:\Program Files\agy.cmd' -p "do it"`}, true},
		{"powershell -EncodedCommand", "powershell",
			[]string{"-EncodedCommand", encodeForPowerShell(`Set-Location C:\t; & 'C:\t\agy.cmd' -p "hi"`)}, true},
		// terminal-service prepends its own flags ahead of the script flag, so
		// matching only args[0] would miss every real dispatch.
		{"encoded behind leading flags", "powershell.exe",
			[]string{"-NoProfile", "-NonInteractive", "-OutputFormat", "Text", "-EncodedCommand",
				encodeForPowerShell("agy --print hi")}, true},
		{"file-mode launcher", "powershell.exe",
			[]string{"-NoProfile", "-EncodedCommand", helperFileModeLauncher(`agy --print "a long prompt"`)}, true},
		{"posix file-mode launcher", "bash",
			[]string{"-c", helperPosixFileModeLauncher("agy --print hi")}, true},
		{"start-process", "powershell", []string{"-Command", "Start-Process agy -ArgumentList '-p','hi'"}, true},
		{"env prefix", "bash", []string{"-c", "AGY_HOME=/tmp agy --print hi"}, true},

		// Negatives: a mention is not a spawn, and an unreadable payload must
		// answer "no" rather than error or guess.
		{"powershell mentions agy in an argument", "powershell",
			[]string{"-Command", "git log --grep agy"}, false},
		{"file-mode launcher wrapping another program", "powershell.exe",
			[]string{"-EncodedCommand", helperFileModeLauncher("npm run build")}, false},
		{"undecodable base64", "powershell", []string{"-EncodedCommand", "!!!not base64!!!"}, false},
		{"payload over the classify cap", "powershell",
			[]string{"-Command", strings.Repeat("x", antigravityClassifyMaxPayloadBytes+1) + "; agy -p hi"}, false},
		{"empty powershell payload", "powershell", []string{"-Command"}, false},
		{"cmd with a quoted agy mention", "cmd", []string{"/c", `echo "run agy later"`}, false},
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

// The reported defect, end to end: a Windows CLI-maintenance smoke reaches the
// device as `powershell -EncodedCommand <base64 of an agy invocation>`, and
// every one of those runs used to arm nothing, so a PASSED smoke was still
// followed by a day-old observedAt.
//
// Runs on every OS: runLocalCommandWindows carries no build tag and the
// transport itself is stubbed through runEncodedPowerShellViaArgFn, so what is
// under test here is the classify-and-arm decision rather than powershell.exe.
func TestAntigravityFreshness_EncodedPowerShellExecuteAdvancesObservedAt(t *testing.T) {
	home, cache := helperIsolateAntigravityCapture(t, "")
	helperSeedStaleAntigravityCache(t, cache)
	t.Setenv(mockAgyQuotaBaseEnv, filepath.Join(home, ".gemini", "antigravity-cli"))
	_, executable := helperMockAgyOnPath(t, "antigravity-quota-server")

	stale, err := time.Parse(time.RFC3339, helperStaleObservedAt)
	if err != nil {
		t.Fatalf("parse seed: %v", err)
	}

	// The stub stands in for powershell.exe: it decodes the script the real
	// transport would have run and executes the agy invocation inside it, so the
	// child owns its quota server exactly as on Windows.
	restore := runEncodedPowerShellViaArgFn
	t.Cleanup(func() { runEncodedPowerShellViaArgFn = restore })
	var ran atomic.Int64
	runEncodedPowerShellViaArgFn = func(encodedScript, workDir string, timeout time.Duration) (string, error) {
		ran.Add(1)
		script, decodeErr := decodeBase64PowerShellStrict(encodedScript)
		if decodeErr != nil {
			return "", decodeErr
		}
		if !strings.Contains(script, "agy") {
			return "", fmt.Errorf("stub received an unexpected script")
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		out, runErr := exec.CommandContext(ctx, executable, "--print", "hello").CombinedOutput()
		return string(out), runErr
	}

	// The bare `powershell -EncodedCommand <b64>` shape, which is the one
	// runLocalCommandWindows routes straight to the encoded transport. The
	// flag-prefixed spelling terminal-service also emits lands on the persistent
	// PowerShell path instead — both sit UNDER the single arm at the top of the
	// function, which is why one arm site covers the whole chain.
	script := fmt.Sprintf(`Set-Location %q; & %q --print "hello"`, t.TempDir(), executable)
	out, execErr := runLocalCommandWindows("powershell",
		[]string{"-EncodedCommand", encodeForPowerShell(script)},
		t.TempDir(), 30*time.Second)
	if execErr != nil {
		t.Fatalf("windows execute failed: %v (output=%q)", execErr, out)
	}
	if ran.Load() != 1 {
		t.Fatalf("the encoded-PowerShell transport ran %d times, want exactly once", ran.Load())
	}

	if stopped := antigravityCaptureStopped(); stopped != nil {
		select {
		case <-stopped:
		case <-time.After(30 * time.Second):
			t.Fatal("capture poller did not stop after the execute returned")
		}
	} else {
		t.Fatal("the Windows execute never armed a quota capture")
	}
	// Exactly one arm for the whole transport chain, and exactly one release:
	// the fallbacks below the arm are sequential, so a failover must not
	// double-arm and a poller must never outlive the execute.
	if got := antigravityCaptureArms.Load(); got != 1 {
		t.Errorf("arms=%d, want exactly one per execute", got)
	}
	if got := antigravityCaptureFinishes.Load(); got != 1 {
		t.Errorf("finishes=%d, want exactly one per execute", got)
	}

	if _, observed := helperParsedObservedAt(t, home, time.Now()); !observed.After(stale) {
		t.Fatalf("observedAt=%s did not advance past the stale %s — the smoke passed and freshness did not move",
			observed, stale)
	}
}

// A non-agy Windows execute must arm nothing at all. The cost of a false
// positive is only a poller that finds no server, but a capture attached to
// every `npm test` on the device is noise in the logs and work on the user's
// machine for nothing.
func TestAntigravityFreshness_NonAgyWindowsExecuteArmsNothing(t *testing.T) {
	helperIsolateAntigravityCapture(t, "1h")
	before := antigravityCaptureStopped()

	restore := runEncodedPowerShellViaArgFn
	t.Cleanup(func() { runEncodedPowerShellViaArgFn = restore })
	runEncodedPowerShellViaArgFn = func(string, string, time.Duration) (string, error) {
		return "ok", nil
	}

	if _, err := runLocalCommandWindows("powershell",
		[]string{"-EncodedCommand", encodeForPowerShell("git log --grep agy")},
		t.TempDir(), 30*time.Second); err != nil {
		t.Fatalf("windows execute failed: %v", err)
	}
	if got := antigravityCaptureArms.Load(); got != 0 {
		t.Errorf("arms=%d, want 0 for a command that never spawns agy", got)
	}
	if antigravityCaptureStopped() != before {
		t.Error("a poller was started for a non-agy execute")
	}
}

// The acceptance's second half: freshness must survive an `agy` self-update.
// Distinct from …SurvivesAnInstallTreeMigrationMidCapture, which moves the tree
// under a single live poller. This is the maintenance flow's real shape — two
// separate runs with the update between them — where the risk is not the poller
// losing track mid-flight but the SECOND run reading a tree the first one never
// knew about, and the replay then serving the pre-update observation forever.
func TestAntigravityFreshness_SurvivesACLIUpdateBetweenSmokes(t *testing.T) {
	home, cache := helperIsolateAntigravityCapture(t, "20ms")
	helperSeedStaleAntigravityCache(t, cache)
	stale, err := time.Parse(time.RFC3339, helperStaleObservedAt)
	if err != nil {
		t.Fatalf("parse seed: %v", err)
	}

	// Smoke one, on the legacy install tree.
	legacy := filepath.Join(home, ".agy")
	helperWriteJSON(t, filepath.Join(legacy, "config.json"), map[string]any{})
	first := helperStartCaptureServer(t, legacy, helperQuotaJSON, helperStatusJSON)
	firstRun := startAntigravityQuotaCapture("smoke one")
	preUpdate := helperAwaitSnapshot(t, cache, stale, "the pre-update observation")
	helperStopCapture(t, firstRun)
	first.srv.Close()
	preUpdateObserved, err := time.Parse(time.RFC3339, preUpdate.ObservedAt)
	if err != nil {
		t.Fatalf("pre-update observedAt=%q is not RFC3339: %v", preUpdate.ObservedAt, err)
	}

	// The CLI updates between the two smokes: the install moves to the modern
	// tree and the legacy config goes with it.
	helperRemoveFile(t, filepath.Join(legacy, "config.json"))
	modern := filepath.Join(home, ".gemini", "antigravity-cli")
	helperWriteJSON(t, filepath.Join(modern, "settings.json"), map[string]any{})

	// observedAt has one-second resolution; the post-update reading has to land
	// in a strictly later second to be provably a new observation.
	time.Sleep(1100 * time.Millisecond)

	// Smoke two, on the updated install.
	helperStartCaptureServer(t, modern, helperQuotaJSON, helperStatusJSON)
	secondRun := startAntigravityQuotaCapture("smoke two")
	helperAwaitSnapshot(t, cache, preUpdateObserved, "the post-update observation")
	helperStopCapture(t, secondRun)

	// And the card — which reads through the parser, with no server up — must
	// show the post-update reading rather than replaying the pre-update one.
	_, observed := helperParsedObservedAt(t, home, time.Now())
	if !observed.After(preUpdateObserved) {
		t.Fatalf("observedAt=%s did not advance past the pre-update %s — the update lost the capture",
			observed, preUpdateObserved)
	}
}
