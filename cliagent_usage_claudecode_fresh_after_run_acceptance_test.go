package main

// Acceptance: after a finished Claude run of ANY kind — a terminal-managed
// session, a shell-wrapped terminal-managed launch (the Windows
// `powershell -EncodedCommand` and file-mode shapes terminal-service ships), a
// `claude` the user ran in their own shell, or a `__cli_smoke__` followed by an
// update — the card gets a fresh numeric reading AND the backend is told: the
// generation moves, one signed hint goes out, and the parser publishes rows
// and that generation. Fake usage endpoint, fake hint sender, temp home.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// freshAfterRunFixture arms a numeric endpoint, seeds a stale reading, pins
// this "process" to epoch and starts the propagator.
func freshAfterRunFixture(t *testing.T, epoch int64) (cache string, calls *int64, rec *cliUsageHintRecorder) {
	t.Helper()
	cache, calls = armClaudeUsageProbe(t, numericClaudeEndpoint)
	seedStaleClaudeObservation(t, cache)
	withCodexGenerationEpoch(t, epoch)
	rec, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)
	return cache, calls, rec
}

// requireFreshClaudeUsagePublished waits for the debt to be paid and a Claude
// hint to go out, then asserts the published usage: numeric rows, and the
// generation the hint named.
func requireFreshClaudeUsagePublished(t *testing.T, cache string, rec *cliUsageHintRecorder, epoch int64) {
	t.Helper()
	// The run's own probe reading, landed BEFORE anything parses: the parser
	// probes a stale cache by itself, which must not stand in for the run's debt.
	waitForClaudeCondition(t, 15*time.Second, "the run's debt was never paid by a probe", func() bool {
		snap, ok := loadClaudeRateLimitSnapshot(cache)
		return ok && snap.LastProbeObservedAtMs > 0 && snap.RefreshOwedAtMs == 0
	})
	claudeFreshnessWaitIdle(t)
	paid := claudeGenerationOf(t, cache)
	var hint cliUsageObservedHint
	waitForClaudeCondition(t, 10*time.Second, "the paid reading was never hinted", func() bool {
		for _, h := range hintsFor(rec.all(), claudeUsageProvider) {
			if h.GenerationEpoch == paid.Epoch && h.Generation == paid.Counter {
				hint = h
				return true
			}
		}
		return false
	})
	if hint.GenerationEpoch != epoch {
		t.Fatalf("hint = %+v, want epoch %d", hint, epoch)
	}
	requireNumericClaudeRows(t)
	usage, _ := claudeCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{}, time.Now())
	if g := claudeGenerationOf(t, cache); usage.UsageGeneration == nil || *usage.UsageGeneration != g {
		t.Fatalf("published generation %+v, cache generation %+v", usage.UsageGeneration, g)
	}
}

func TestClaudeFreshAfterRun_TerminalManagedRun(t *testing.T) {
	skipIfUnsupportedOS(t)
	cache, _, rec := freshAfterRunFixture(t, 8101)
	startManagedClaudeSession(t, "claude-heartbeat-result")
	requireFreshClaudeUsagePublished(t, cache, rec, 8101)
}

// startWrappedClaudeSession runs the mock `claude` through a shell wrapper, so
// the session's base command is the shell — no stream-json `result` detection
// — and the run owes at exit.
func startWrappedClaudeSession(t *testing.T, command string, args []string) {
	t.Helper()
	sm := NewSessionManager(nil)
	id := fmt.Sprintf("claude-wrapped-%d", time.Now().UnixNano())
	if err := sm.StartSession(id, command, args, t.TempDir(), "ws", "uid", 60000, false, func(resultMsg) {}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() {
		_ = sm.EndSession(id)
		waitForManagedSessionDrained(t, sm, id)
	})
}

func TestClaudeFreshAfterRun_WrappedTerminalManagedRun(t *testing.T) {
	skipIfUnsupportedOS(t)
	if runtime.GOOS != "windows" {
		cache, _, rec := freshAfterRunFixture(t, 8102)
		dir := installMockClaude(t, "claude-heartbeat-result")
		startWrappedClaudeSession(t, "bash", []string{"-c", helperPosixFileModeLauncher(filepath.Join(dir, "claude") + " -p hi")})
		requireFreshClaudeUsagePublished(t, cache, rec, 8102)
		return
	}
	t.Run("powershell -EncodedCommand of the npm shim shape", func(t *testing.T) {
		cache, _, rec := freshAfterRunFixture(t, 8103)
		dir := installMockClaude(t, "claude-heartbeat-result")
		script := fmt.Sprintf("& '%s' -p hi", filepath.Join(dir, "claude.exe"))
		startWrappedClaudeSession(t, "powershell", []string{"-NoProfile", "-NonInteractive", "-EncodedCommand", encodeForPowerShell(script)})
		requireFreshClaudeUsagePublished(t, cache, rec, 8103)
	})
	t.Run("file-mode launcher running claude.exe", func(t *testing.T) {
		cache, _, rec := freshAfterRunFixture(t, 8104)
		dir := installMockClaude(t, "claude-heartbeat-result")
		script := fmt.Sprintf("& '%s' -p 'a long prompt'", filepath.Join(dir, "claude.exe"))
		startWrappedClaudeSession(t, "powershell", []string{"-NoProfile", "-EncodedCommand", helperFileModeLauncher(script)})
		requireFreshClaudeUsagePublished(t, cache, rec, 8104)
	})
}

// A `claude -p` in the user's own shell: only its transcript is seen. The
// watcher owes, the probe pays, a hint goes out — one request.
func TestClaudeFreshAfterRun_DirectRun(t *testing.T) {
	cache, calls, rec := freshAfterRunFixture(t, 8105)
	writeClaudeTranscript(t, os.Getenv("CLAUDE_CONFIG_DIR"), "-home-u-repo", "s.jsonl", time.Now())
	claudeUsageWatchTick(time.Now(), true)
	requireFreshClaudeUsagePublished(t, cache, rec, 8105)
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("%d probe requests, want 1", got)
	}
}

// The smoke owes and the process is replaced before its probe can pay; the new
// process pays the debt and hints it, and the card's rows are newer than the
// smoke.
func TestClaudeFreshAfterRun_SmokeThenUpdate(t *testing.T) {
	smokeEnv(t)
	cache, healthy := armPostUpdateEndpoint(t)
	seedStaleClaudeObservation(t, cache)
	withCodexGenerationEpoch(t, 8106)
	rec, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)

	path := stubClaudeBinary(t)
	stubAuthProbe(t, true, true)
	stubSmokeExec(t, func(_ context.Context, _ []string, prompt string) ([]byte, []byte, error) {
		return successEnvelope(markerFromPrompt(prompt)), nil, nil
	})
	smokeAt := time.Now()
	if result := runClaudeCodeSmoke(context.Background(), path, "2.1.251"); result.Status != cliSmokeStatusSuccess {
		t.Fatalf("smoke = %+v", result)
	}
	claudeFreshnessWaitIdle(t)
	stopCLIUsagePropagator()

	healthy.Store(true)
	simulateCodexProcessRestart(t, 8107)
	simulateClaudeAgentRestart(t)
	startCLIUsagePropagator(cfg)
	payOwedClaudeUsageRefreshAt(claudeAfterFirstRung())
	requireFreshClaudeUsagePublished(t, cache, rec, 8107)
	if observed := claudeObservedAt(t, time.Now()); observed.Before(smokeAt) {
		t.Fatalf("card observedAt %s predates the smoke %s", observed, smokeAt)
	}
}

// A burst of 10 runs inside one minute costs at most 2 requests — the first
// run's immediate attempt, then one rung after the 60 s floor that covers the
// last run — and one hint plus its follow-up.
func TestClaudeFreshAfterRun_ABurstIsBounded(t *testing.T) {
	cache, calls, rec := freshAfterRunFixture(t, 8108)
	cliUsageHintDebounce = 400 * time.Millisecond
	t.Setenv(claudeUsageProbeMinIntervalEnv, "60000")

	var last time.Time
	for i := 0; i < 10; i++ {
		last = time.Now()
		claudeUsageProbeAfterRun(last)
	}
	claudeFreshnessWaitIdle(t)
	// The rung after the 60 s floor: the floor is measured on the wall clock, so
	// lift it rather than wait it out.
	t.Setenv(claudeUsageProbeMinIntervalEnv, "0")
	claudeRunDebtAttemptAt(time.Now().Add(61*time.Second), claudeDebtTriggerTimer)
	claudeFreshnessWaitIdle(t)

	if got := atomic.LoadInt64(calls); got > 2 {
		t.Fatalf("%d probe requests for a burst, want at most 2", got)
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("the last run of the burst is still owed: %d (last %d)", snap.RefreshOwedAtMs, last.UnixMilli())
	}
	hints := waitHints(t, rec, 2, 3*cliUsageHintSpacing)
	if n := len(hintsFor(hints, claudeUsageProvider)); n != 2 {
		t.Fatalf("%d Claude hints, want one hint and its follow-up", n)
	}
}

// A 429 hold recorded before an update is honoured by the new process: no
// request until it ends.
func TestClaudeFreshAfterRun_AHoldIsHonouredAcrossTheRestart(t *testing.T) {
	var held atomic.Bool
	held.Store(true)
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		if held.Load() {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		numericClaudeEndpoint(w, r)
	})
	seedStaleClaudeObservation(t, cache)
	claudeUsageProbeAfterRun(time.Now())
	claudeFreshnessWaitIdle(t)
	if snap := claudeCacheSnapshot(t, cache); snap.HeldUntilMs == 0 || atomic.LoadInt64(calls) != 1 {
		t.Fatalf("hold=%d calls=%d, want the 429 recorded after one request", snap.HeldUntilMs, atomic.LoadInt64(calls))
	}
	held.Store(false)
	simulateClaudeAgentRestart(t)
	payOwedClaudeUsageRefreshAt(time.Now())
	claudeFreshnessWaitIdle(t)
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("%d requests: the new process ignored the persisted hold", got)
	}
}

// Every log line this feature adds is a fixed label: the watcher, the stamp,
// the scan and the per-provider hint lines carry no path, project, script,
// fingerprint, token or session id.
func TestClaudeFreshAfterRun_LogsAreRedacted(t *testing.T) {
	logged := captureStdout(t, func() {
		cache, _, rec := freshAfterRunFixture(t, 8109)
		project := "-Users-private-person-secret-repo"
		writeClaudeTranscript(t, os.Getenv("CLAUDE_CONFIG_DIR"), project, "7f3e-session.jsonl", time.Now())
		claudeUsageWatchTick(time.Now(), true)
		requireFreshClaudeUsagePublished(t, cache, rec, 8109)
		_ = commandRunsClaude("powershell", []string{"-EncodedCommand", encodeForPowerShell("claude -p 'sk-ant-secret-prompt'")})
		stopCLIUsagePropagator()
	})
	for _, leak := range []string{"private-person", "secret-repo", "7f3e-session", "sk-ant-secret-prompt", probeTestToken, os.Getenv("CLAUDE_CONFIG_DIR")} {
		if leak != "" && strings.Contains(logged, leak) {
			t.Fatalf("log leaks %q:\n%s", leak, logged)
		}
	}
}
