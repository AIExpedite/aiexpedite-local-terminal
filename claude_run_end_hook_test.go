package main

// Skip and write rules of the SessionEnd hook (claude_run_end_hook.go): when a
// Claude session the agent did not spawn ends, the hook owes exactly one
// refresh, under the same account-scope and monotonic rules as every other
// owe, and stays silent otherwise.

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// armClaudeRunEndHookTest isolates one hook test: a private cache and Claude
// config dir (armClaudeUsageProbe), no env credential, no owner marker, and the
// probe gate DISARMED — the hook runs in a short-lived process where the
// resident agent's gate is never armed, and it must owe anyway.
func armClaudeRunEndHookTest(t *testing.T, handler http.HandlerFunc) (string, *int64) {
	t.Helper()
	cache, calls := armClaudeUsageProbe(t, handler)
	for _, name := range []string{
		"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
		"CLAUDE_CODE_USE_ANTHROPIC_AWS", "CLAUDE_CODE_USE_MANTLE", claudeRunOwnerEnv,
	} {
		t.Setenv(name, "")
	}
	resetClaudeUsageProbeGate()
	return cache, calls
}

// seedClaudeStatusLineReading leaves a status-line row observed at `at`.
func seedClaudeStatusLineReading(t *testing.T, cache string, at time.Time) {
	t.Helper()
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {UsedPercentage: 21, ResetsAtMs: at.Add(time.Hour).UnixMilli(), ObservedAtMs: at.UnixMilli(), usageKnown: true},
	}, at, currentClaudeAccountFingerprint(), claudeRateLimitSourceStatusLine)
}

func TestClaudeRunEndHook_OwesADueDebtWithoutTheArmedGate(t *testing.T) {
	cache, calls := armClaudeRunEndHookTest(t, claudeProbeOKHandler)
	end := time.Now()
	if !claudeRunEndHookAt(end) {
		t.Fatal("the hook did not owe a refresh for a run the agent did not spawn")
	}
	snap := claudeCacheSnapshot(t, cache)
	if snap.RefreshOwedAtMs != end.UnixMilli() || snap.RefreshOwedAttempts != 0 || snap.NextAttemptAtMs != 0 {
		t.Fatalf("snap=%+v, want a due debt at the run's end with a fresh budget", snap)
	}
	if *calls != 0 {
		t.Fatalf("the hook made %d requests, want none — it does no network work", *calls)
	}
}

func TestClaudeRunEndHook_SkipRules(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, cache string, end time.Time)
	}{
		{"env credential active", func(t *testing.T, _ string, _ time.Time) {
			t.Setenv("ANTHROPIC_AUTH_TOKEN", "env-bearer")
		}},
		{"cloud provider selected", func(t *testing.T, _ string, _ time.Time) {
			t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
		}},
		{"agent-owned run", func(t *testing.T, _ string, _ time.Time) {
			t.Setenv(claudeRunOwnerEnv, claudeRunOwnerAgent)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache, _ := armClaudeRunEndHookTest(t, claudeProbeOKHandler)
			end := time.Now()
			tc.setup(t, cache, end)
			if claudeRunEndHookAt(end) {
				t.Fatal("the hook owed a refresh it must skip")
			}
			if snap, ok := loadClaudeRateLimitSnapshot(cache); ok && snap.RefreshOwedAtMs != 0 {
				t.Fatalf("snap=%+v, want no debt", snap)
			}
		})
	}
}

// The cache is shared by every Claude process, so no existing reading — not
// even a status-line row rendered at the very instant this run ended — proves
// it covers THIS run's usage: it may be another session's. The hook owes
// regardless; the attempt's coverage pre-check settles it for free once a
// reading observed after the run's end lands.
func TestClaudeRunEndHook_OwesDespiteAnExistingReading(t *testing.T) {
	cases := []struct {
		name string
		seed func(t *testing.T, cache string, end time.Time)
	}{
		{"another session's status-line reading just now", func(t *testing.T, cache string, end time.Time) {
			seedClaudeStatusLineReading(t, cache, end)
		}},
		{"only a probe reading", func(t *testing.T, cache string, end time.Time) {
			mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
				claudeWindowFiveHour: {UsedPercentage: 9, ResetsAtMs: end.Add(time.Hour).UnixMilli(), ObservedAtMs: end.UnixMilli(), usageKnown: true},
			}, end, currentClaudeAccountFingerprint(), claudeRateLimitSourceProbe)
		}},
		{"only a stream reading", func(t *testing.T, cache string, end time.Time) {
			mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
				claudeWindowFiveHour: {UsedPercentage: 9, ResetsAtMs: end.Add(time.Hour).UnixMilli(), ObservedAtMs: end.UnixMilli(), usageKnown: true},
			}, end, currentClaudeAccountFingerprint(), claudeRateLimitSourceStream)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache, _ := armClaudeRunEndHookTest(t, claudeProbeOKHandler)
			end := time.Now()
			tc.seed(t, cache, end)
			if !claudeRunEndHookAt(end) {
				t.Fatal("the hook let an existing reading stand in for this run's refresh")
			}
			if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != end.UnixMilli() {
				t.Fatalf("snap=%+v, want a debt at the run's end", snap)
			}
		})
	}
}

// Monotonic, like every owe: an existing NEWER debt keeps its instant and its
// spent budget; an older one advances and gets a fresh budget.
func TestClaudeRunEndHook_IsMonotonic(t *testing.T) {
	cache, _ := armClaudeRunEndHookTest(t, claudeProbeOKHandler)
	fp := currentClaudeAccountFingerprint()
	end := time.Now()

	seedClaudeRefreshDebt(t, cache, fp, end.Add(time.Second), 2, time.Time{})
	if !claudeRunEndHookAt(end) {
		t.Fatal("a newer standing debt is still a debt on disk")
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != end.Add(time.Second).UnixMilli() || snap.RefreshOwedAttempts != 2 {
		t.Fatalf("snap=%+v, want the newer debt and its attempt counter untouched", snap)
	}

	later := end.Add(time.Minute)
	if !claudeRunEndHookAt(later) {
		t.Fatal("a later run end did not advance the debt")
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != later.UnixMilli() || snap.RefreshOwedAttempts != 0 {
		t.Fatalf("snap=%+v, want the debt advanced with a reset budget", snap)
	}
}

// A credential read that resolves no account over a SCOPED cache is not a
// logout the hook may act on: it must not wipe that account's buckets.
func TestClaudeRunEndHook_EmptyFingerprintNeverWipesAScopedCache(t *testing.T) {
	cache, _ := armClaudeRunEndHookTest(t, claudeProbeOKHandler)
	if fp := currentClaudeAccountFingerprint(); fp != "" {
		t.Skipf("fixture credential resolves an account (%d chars); this case needs none", len(fp))
	}
	now := time.Now()
	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {UsedPercentage: 44, ResetsAtMs: now.Add(time.Hour).UnixMilli(), ObservedAtMs: now.Add(-time.Hour).UnixMilli(), usageKnown: true},
	}, now.Add(-time.Hour), "fp-A")
	before, _ := os.ReadFile(cache)

	if claudeRunEndHookAt(now) {
		t.Fatal("the hook owed under an empty fingerprint over a scoped cache")
	}
	if after, _ := os.ReadFile(cache); string(after) != string(before) {
		t.Fatal("the refused owe rewrote the scoped cache")
	}
}

// Lock contention past the budget drops the write: nothing is written, and the
// hook still returns promptly so Claude's exit is never held.
func TestClaudeRunEndHook_LockContentionWritesNothing(t *testing.T) {
	cache, _ := armClaudeRunEndHookTest(t, claudeProbeOKHandler)
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
		t.Fatal(err)
	}
	held, ok := acquireCrossProcessCacheLock(cache)
	if !ok {
		t.Skip("advisory locking unavailable on this filesystem")
	}
	defer func() {
		_ = unlockFile(held)
		_ = held.Close()
	}()
	prevWait := claudeRateLimitCacheLockWait
	claudeRateLimitCacheLockWait = 50 * time.Millisecond
	t.Cleanup(func() { claudeRateLimitCacheLockWait = prevWait })

	done := make(chan bool, 1)
	go func() { done <- claudeRunEndHookAt(time.Now()) }()
	select {
	case wrote := <-done:
		if wrote {
			t.Fatal("the hook reported a debt it could not write")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hook wedged behind a held cache lock")
	}
	if snap, ok := loadClaudeRateLimitSnapshot(cache); ok && snap.RefreshOwedAtMs != 0 {
		t.Fatalf("snap=%+v, want nothing written under contention", snap)
	}
}
