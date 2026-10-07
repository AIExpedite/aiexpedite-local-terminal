package main

import (
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_claudecode_direct_runs_test.go — `claude` runs the agent did
   not start, seen from their transcripts' mtimes, owe a refresh through the
   ordinary debt ladder — boundedly.
   ------------------------------------------------------------------------ */

// writeClaudeTranscript creates <configDir>/projects/<project>/<name> stamped
// at mtime.
func writeClaudeTranscript(t *testing.T, configDir, project, name string, mtime time.Time) {
	t.Helper()
	dir := filepath.Join(configDir, "projects", project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeNewestTranscript_TheMaximumWinsRegardlessOfOrder(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	writeClaudeTranscript(t, configDir, "-a-project", "s1.jsonl", base)
	writeClaudeTranscript(t, configDir, "-b-project", "s2.jsonl", base.Add(time.Minute))
	writeClaudeTranscript(t, configDir, "-z-project", "s3.jsonl", base.Add(10*time.Minute)) // walked last
	writeClaudeTranscript(t, configDir, "-z-project", "notes.txt", base.Add(time.Hour))     // not a transcript

	newest, files, complete := claudeNewestTranscriptMs("", time.Minute, math.MaxInt64)
	if !complete || files != 3 || newest != base.Add(10*time.Minute).UnixMilli() {
		t.Fatalf("newest=%d files=%d complete=%v, want the last directory's transcript", newest, files, complete)
	}
}

func TestClaudeNewestTranscript_AMissingProjectsDirIsACompleteEmptyScan(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if newest, files, complete := claudeNewestTranscriptMs("", time.Minute, math.MaxInt64); newest != 0 || files != 0 || !complete {
		t.Fatalf("newest=%d files=%d complete=%v", newest, files, complete)
	}
}

// A scan that exhausts its budget is unknown: it reports incomplete, and the
// watcher owes nothing even though what it saw is newer than the reading.
func TestClaudeNewestTranscript_AnExhaustedBudgetIsIncompleteAndOwesNothing(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	seedClaudeProbeReading(t, cache, time.Now().Add(-2*time.Hour))
	writeClaudeTranscript(t, configDir, "-p", "s.jsonl", time.Now().Add(-time.Minute))

	if _, _, complete := claudeNewestTranscriptMs("", -time.Second, math.MaxInt64); complete {
		t.Fatal("a scan past its budget reported complete")
	}
	resetClaudeUsageWatchState()
	prev := claudeDirectRunScanBudget
	claudeDirectRunScanBudget = -time.Second
	t.Cleanup(func() { claudeDirectRunScanBudget = prev })
	claudeScanDirectRuns(time.Now())
	claudeFreshnessWaitIdle(t)
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 || atomic.LoadInt64(calls) != 0 {
		t.Fatalf("an incomplete scan owed: debt=%d calls=%d", snap.RefreshOwedAtMs, atomic.LoadInt64(calls))
	}
}

func TestClaudeOweDirectRun_TheOweRule(t *testing.T) {
	failing := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }

	t.Run("owes when the reading does not cover the run", func(t *testing.T) {
		cache, calls := armClaudeUsageProbe(t, failing)
		now := time.Now()
		seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
		evidence := now.Add(-5 * time.Minute)
		if got := claudeOweDirectRunRefresh(evidence, now); got != claudeDirectRunOwed {
			t.Fatalf("outcome = %s, want owed", got)
		}
		claudeFreshnessWaitIdle(t)
		snap := claudeCacheSnapshot(t, cache)
		if snap.RefreshOwedAtMs != evidence.UnixMilli() || atomic.LoadInt64(calls) != 1 {
			t.Fatalf("debt=%d calls=%d, want the evidence owed and one attempt", snap.RefreshOwedAtMs, atomic.LoadInt64(calls))
		}
	})

	t.Run("covered inside the 30 s margin", func(t *testing.T) {
		cache, calls := armClaudeUsageProbe(t, failing)
		now := time.Now()
		seedClaudeProbeReading(t, cache, now.Add(-time.Minute))
		if got := claudeOweDirectRunRefresh(now.Add(-time.Minute+20*time.Second), now); got != claudeDirectRunCovered {
			t.Fatalf("outcome = %s, want covered", got)
		}
		if atomic.LoadInt64(calls) != 0 || claudeCacheSnapshot(t, cache).RefreshOwedAtMs != 0 {
			t.Fatal("a covered run owed")
		}
	})

	t.Run("a standing debt is never moved forward", func(t *testing.T) {
		cache, _ := armClaudeUsageProbe(t, failing)
		now := time.Now()
		seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
		owed := now.Add(-20 * time.Minute).UnixMilli()
		mutateClaudeRateLimitSnapshot(cache, currentClaudeAccountFingerprint(), func(snap *claudeRateLimitSnapshot) bool {
			snap.RefreshOwedAtMs, snap.RefreshOwedAttempts = owed, 2
			return true
		})
		if got := claudeOweDirectRunRefresh(now.Add(-time.Minute), now); got != claudeDirectRunStanding {
			t.Fatalf("outcome = %s, want standing", got)
		}
		if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != owed || snap.RefreshOwedAttempts != 2 {
			t.Fatalf("the standing debt moved: %+v", snap)
		}
	})

	t.Run("quiet window, age-out, future stamp and opt-out owe nothing", func(t *testing.T) {
		cache, calls := armClaudeUsageProbe(t, failing)
		now := time.Now()
		seedClaudeProbeReading(t, cache, now.Add(-5*time.Hour))
		mutateClaudeRateLimitSnapshot(cache, currentClaudeAccountFingerprint(), func(snap *claudeRateLimitSnapshot) bool {
			snap.DirectRunQuietUntilMs = now.Add(time.Hour).UnixMilli()
			return true
		})
		if got := claudeOweDirectRunRefresh(now.Add(-time.Minute), now); got != claudeDirectRunQuiet {
			t.Fatalf("inside the quiet window: %s", got)
		}
		if got := claudeOweDirectRunRefresh(now.Add(-claudeRefreshOwedMaxAge-time.Minute), now); got != claudeDirectRunStale {
			t.Fatalf("past the age-out: %s", got)
		}
		if got := claudeOweDirectRunRefresh(now.Add(claudeRefreshOwedLocalSkew+time.Minute), now); got != claudeDirectRunFuture {
			t.Fatalf("stamped in the future: %s", got)
		}
		SetClaudeUsageProbeDisabled(true)
		if got := claudeOweDirectRunRefresh(now.Add(-time.Minute), now); got != claudeDirectRunDisarmed {
			t.Fatalf("opted out: %s", got)
		}
		if atomic.LoadInt64(calls) != 0 || claudeCacheSnapshot(t, cache).RefreshOwedAtMs != 0 {
			t.Fatal("a refused owe still wrote a debt or sent a request")
		}
	})
}

// Five advancing transcript mtimes across restarts against a failing endpoint
// stop at claudeRefreshOwedMaxRequests requests, and the retired debt leaves
// the quiet window behind.
func TestClaudeOweDirectRun_AFailingEndpointIsBoundedAcrossRestarts(t *testing.T) {
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	pinClaudeRunDebtLadder(t, []time.Duration{10 * time.Millisecond}, 10*time.Millisecond, 5*time.Millisecond)
	base := time.Now().Add(-30 * time.Minute)
	seedClaudeProbeReading(t, cache, base.Add(-time.Hour))

	for i := 0; i < 5; i++ {
		claudeOweDirectRunRefresh(base.Add(time.Duration(i)*time.Minute), time.Now())
		claudeFreshnessWaitIdle(t)
		// The update: a new process replays the debt at its booked rung.
		simulateClaudeAgentRestart(t)
		at := time.Now()
		if rung := claudeCacheSnapshot(t, cache).NextAttemptAtMs; rung > 0 {
			at = time.UnixMilli(rung).Add(time.Second)
		}
		payOwedClaudeUsageRefreshAt(at)
		claudeFreshnessWaitIdle(t)
	}
	if got := atomic.LoadInt64(calls); got > claudeRefreshOwedMaxRequests {
		t.Fatalf("%d requests, want at most %d", got, claudeRefreshOwedMaxRequests)
	}
	snap := claudeCacheSnapshot(t, cache)
	if snap.RefreshOwedAtMs != 0 || snap.DirectRunQuietUntilMs <= time.Now().UnixMilli() {
		t.Fatalf("debt=%d quietUntil=%d, want the debt retired and the quiet window set", snap.RefreshOwedAtMs, snap.DirectRunQuietUntilMs)
	}
}

// A debt retired unpaid by the ladder itself (the cap) starts the quiet window;
// one retired because it was covered does not.
func TestClaudeOweDirectRun_OnlyAnUnpaidRetirementStartsTheQuietWindow(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	owed := now.Add(-10 * time.Minute)
	mutateClaudeRateLimitSnapshot(cache, currentClaudeAccountFingerprint(), func(snap *claudeRateLimitSnapshot) bool {
		snap.RefreshOwedAtMs, snap.RefreshOwedAttempts = owed.UnixMilli(), claudeRefreshOwedMaxRequests
		return true
	})
	payOwedClaudeUsageRefreshAt(now)
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 || snap.DirectRunQuietUntilMs == 0 {
		t.Fatalf("a capped retirement left quiet=%d debt=%d", snap.DirectRunQuietUntilMs, snap.RefreshOwedAtMs)
	}

	cache2, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {})
	seedClaudeProbeReading(t, cache2, now)
	mutateClaudeRateLimitSnapshot(cache2, currentClaudeAccountFingerprint(), func(snap *claudeRateLimitSnapshot) bool {
		snap.RefreshOwedAtMs = now.Add(-time.Minute).UnixMilli()
		return true
	})
	payOwedClaudeUsageRefreshAt(now)
	if snap := claudeCacheSnapshot(t, cache2); snap.RefreshOwedAtMs != 0 || snap.DirectRunQuietUntilMs != 0 {
		t.Fatalf("a covered retirement set quiet=%d debt=%d", snap.DirectRunQuietUntilMs, snap.RefreshOwedAtMs)
	}
}

// Evidence met by a standing debt is not marked as handled: once the older
// debt settles with a reading that covers only its own baseline, the next scan
// owes the newer run.
func TestClaudeDirectRunScan_StandingEvidenceIsReconsideredAfterTheDebtSettles(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-2*time.Hour))
	owed := now.Add(-20 * time.Minute).UnixMilli()
	mutateClaudeRateLimitSnapshot(cache, currentClaudeAccountFingerprint(), func(snap *claudeRateLimitSnapshot) bool {
		snap.RefreshOwedAtMs, snap.RefreshOwedAttempts = owed, 1
		return true
	})
	evidence := now.Add(-time.Minute).Truncate(time.Second)
	writeClaudeTranscript(t, configDir, "-p", "s.jsonl", evidence)
	resetClaudeUsageWatchState()
	t.Cleanup(resetClaudeUsageWatchState)

	claudeScanDirectRuns(now)
	claudeFreshnessWaitIdle(t)
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != owed {
		t.Fatalf("the standing debt moved: %+v", snap)
	}

	// The older debt is paid by a reading that covers its baseline only.
	seedClaudeProbeReading(t, cache, now.Add(-10*time.Minute))
	mutateClaudeRateLimitSnapshot(cache, currentClaudeAccountFingerprint(), func(snap *claudeRateLimitSnapshot) bool {
		snap.RefreshOwedAtMs, snap.RefreshOwedAttempts, snap.NextAttemptAtMs = 0, 0, 0
		return true
	})
	claudeScanDirectRuns(now)
	claudeFreshnessWaitIdle(t)
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != evidence.UnixMilli() {
		t.Fatalf("debt=%d, want the newer run owed after the standing debt settled", snap.RefreshOwedAtMs)
	}
}

// A transcript stamped beyond the skew ceiling owes nothing now, but is not
// marked as handled: once the clock catches up it is owed like any other run.
func TestClaudeDirectRunScan_FutureEvidenceIsReconsideredOnceTheClockCatchesUp(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-2*time.Hour))
	evidence := now.Add(claudeRefreshOwedLocalSkew + 5*time.Minute).Truncate(time.Second)
	writeClaudeTranscript(t, configDir, "-p", "s.jsonl", evidence)
	resetClaudeUsageWatchState()
	t.Cleanup(resetClaudeUsageWatchState)

	claudeScanDirectRuns(now)
	claudeFreshnessWaitIdle(t)
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("future evidence was owed: %+v", snap)
	}

	claudeScanDirectRuns(evidence.Add(time.Minute))
	claudeFreshnessWaitIdle(t)
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != evidence.UnixMilli() {
		t.Fatalf("debt=%d, want the run owed once the clock caught up", snap.RefreshOwedAtMs)
	}
}

// Evidence deferred by the quiet window is not marked as handled: once the
// window ends, while the run is still inside the age limit, it is owed.
func TestClaudeDirectRunScan_QuietEvidenceIsReconsideredAfterTheWindow(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-2*time.Hour))
	quietUntil := now.Add(time.Hour)
	mutateClaudeRateLimitSnapshot(cache, currentClaudeAccountFingerprint(), func(snap *claudeRateLimitSnapshot) bool {
		snap.DirectRunQuietUntilMs = quietUntil.UnixMilli()
		return true
	})
	evidence := now.Add(-time.Minute).Truncate(time.Second)
	writeClaudeTranscript(t, configDir, "-p", "s.jsonl", evidence)
	resetClaudeUsageWatchState()
	t.Cleanup(resetClaudeUsageWatchState)

	claudeScanDirectRuns(now)
	claudeFreshnessWaitIdle(t)
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 {
		t.Fatalf("evidence was owed inside the quiet window: %+v", snap)
	}

	claudeScanDirectRuns(quietUntil.Add(time.Minute))
	claudeFreshnessWaitIdle(t)
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != evidence.UnixMilli() {
		t.Fatalf("debt=%d, want the run owed once the quiet window ended", snap.RefreshOwedAtMs)
	}
}

// A future-dated transcript does not mask a real run behind it: the scan owes
// the newest transcript inside the skew ceiling.
func TestClaudeDirectRunScan_AFutureTranscriptDoesNotMaskARealRun(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-2*time.Hour))
	writeClaudeTranscript(t, configDir, "-synced", "f.jsonl", now.Add(24*time.Hour))
	evidence := now.Add(-time.Minute).Truncate(time.Second)
	writeClaudeTranscript(t, configDir, "-p", "s.jsonl", evidence)
	resetClaudeUsageWatchState()
	t.Cleanup(resetClaudeUsageWatchState)

	claudeScanDirectRuns(now)
	claudeFreshnessWaitIdle(t)
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != evidence.UnixMilli() {
		t.Fatalf("debt=%d, want the real run owed despite the future transcript", snap.RefreshOwedAtMs)
	}
}

func TestClaudeDirectRunScan_IsThrottled(t *testing.T) {
	resetClaudeUsageWatchState()
	t.Cleanup(resetClaudeUsageWatchState)
	now := time.Now()
	if !claudeDirectRunScanDue(now) {
		t.Fatal("the first scan is not due")
	}
	if claudeDirectRunScanDue(now.Add(claudeDirectRunScanPeriod - time.Second)) {
		t.Fatal("a scan inside the period is due")
	}
	if !claudeDirectRunScanDue(now.Add(claudeDirectRunScanPeriod)) {
		t.Fatal("a scan after the period is not due")
	}
}

// The scan logs a fixed label and counters: never a path or project name.
func TestClaudeDirectRunScan_LogsNoPath(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	seedClaudeProbeReading(t, cache, time.Now().Add(-2*time.Hour))
	project := "-Users-secret-person-private-repo"
	writeClaudeTranscript(t, configDir, project, "0d9c7e2a-session-id.jsonl", time.Now().Add(-time.Minute))
	resetClaudeUsageWatchState()
	t.Cleanup(resetClaudeUsageWatchState)

	logged := captureStdout(t, func() {
		claudeScanDirectRuns(time.Now())
		claudeFreshnessWaitIdle(t)
	})
	if !strings.Contains(logged, "direct-run evidence: owed files=1") {
		t.Fatalf("log = %q", logged)
	}
	for _, leak := range []string{project, "secret-person", "0d9c7e2a", configDir, filepath.ToSlash(configDir)} {
		if strings.Contains(logged, leak) {
			t.Fatalf("scan log leaks %q: %q", leak, logged)
		}
	}
}
