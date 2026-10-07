package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_claudecode_propagation_test.go — Claude Code's capture
   generation: stamped by the agent process when a DISPLAYED row's observation
   advances, noted to the provider-neutral hint sender, and published by the
   parser so the signed receipt names it.
   ------------------------------------------------------------------------ */

// claudeGenerationFixture pins this "process" to epoch, isolates the Claude
// cache and config dir, and starts the propagator with a hint recorder — the
// agent-process state in which merges stamp.
func claudeGenerationFixture(t *testing.T, epoch int64) (*cliUsageHintRecorder, string) {
	t.Helper()
	withCodexGenerationEpoch(t, epoch)
	cache := filepath.Join(t.TempDir(), "claude_rate_limits.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)
	return rec, cache
}

// claudeReading merges one observed reading per window, as the probe would.
func claudeReading(t *testing.T, cache, fingerprint string, at time.Time, observed map[string]time.Time) {
	t.Helper()
	updates := map[string]claudeRateLimitBucket{}
	for window, obs := range observed {
		updates[window] = claudeRateLimitBucket{
			UsedPercentage: 30, ResetsAtMs: at.Add(48 * time.Hour).UnixMilli(),
			ObservedAtMs: obs.UnixMilli(), usageKnown: true,
		}
	}
	if _, err := mergeClaudeRateLimitCacheInto(context.Background(), cache, updates, at, fingerprint,
		claudeRateLimitSourceProbe, false, nil, claudeCredStamp{}); err != nil {
		t.Fatalf("merge: %v", err)
	}
}

func claudeGenerationOf(t *testing.T, cache string) cliUsageGeneration {
	t.Helper()
	snap := claudeCacheSnapshot(t, cache)
	return cliUsageGeneration{Epoch: snap.GenerationEpoch, Counter: snap.Generation}
}

func TestClaudeGeneration_AProbeMergeBumpsOnceAndARepeatDoesNot(t *testing.T) {
	rec, cache := claudeGenerationFixture(t, 9101)
	now := time.Now()
	claudeReading(t, cache, "", now, map[string]time.Time{claudeWindowFiveHour: now, claudeWindowSevenDay: now})
	if g := claudeGenerationOf(t, cache); g != gen(9101, 1) {
		t.Fatalf("generation = %+v, want {9101,1}", g)
	}
	claudeReading(t, cache, "", now, map[string]time.Time{claudeWindowFiveHour: now, claudeWindowSevenDay: now})
	if g := claudeGenerationOf(t, cache); g != gen(9101, 1) {
		t.Fatalf("a repeat merge of the same observation bumped: %+v", g)
	}
	h := waitHints(t, rec, 1, 0)[0].hint
	if h.Provider != claudeUsageProvider || h.GenerationEpoch != 9101 || h.Generation != 1 {
		t.Fatalf("hint = %+v, want claudeCode {9101,1}", h)
	}
}

func TestClaudeGeneration_AHeartbeatOnlyBucketNeverBumps(t *testing.T) {
	_, cache := claudeGenerationFixture(t, 9102)
	now := time.Now()
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {ResetsAtMs: now.Add(time.Hour).UnixMilli(), ObservedAtMs: now.UnixMilli(), Status: "allowed"},
	}, now, "", claudeRateLimitSourceStream)
	if snap := claudeCacheSnapshot(t, cache); snap.Generation != 0 {
		t.Fatalf("a heartbeat-only bucket bumped the generation: %+v", snap)
	}
}

// A row the generation described as numeric that now shows none (an expired
// bucket replaced by a heartbeat) changes the card, so it bumps like an advance.
func TestClaudeRowsAdvanced(t *testing.T) {
	for _, tc := range []struct {
		name         string
		rows, stored []int64
		want         bool
	}{
		{"unchanged", []int64{10, 20}, []int64{10, 20}, false},
		{"a row advanced", []int64{10, 25}, []int64{10, 20}, true},
		{"a row went backwards", []int64{10, 15}, []int64{10, 20}, false},
		{"a numeric row cleared", []int64{10, 0}, []int64{10, 20}, true},
		{"an unknown row stays unknown", []int64{10, 0}, []int64{10, 0}, false},
		{"a new row appeared", []int64{10, 20, 5}, []int64{10, 20}, true},
		{"a described row is no longer shown", []int64{10}, []int64{10, 20}, true},
		{"an unknown row is no longer shown", []int64{10}, []int64{10, 0}, false},
		{"never stamped, nothing shown", []int64{0, 0}, nil, false},
	} {
		if got := claudeRowsAdvanced(tc.rows, tc.stored); got != tc.want {
			t.Errorf("%s: claudeRowsAdvanced(%v, %v) = %v, want %v", tc.name, tc.rows, tc.stored, got, tc.want)
		}
	}
}

// five_hour stays the NEWEST observation while the constraining weekly bucket
// advances: a newest-across-rows rule would never notice; the per-row rule does.
func TestClaudeGeneration_AWeeklyAdvanceBelowTheNewestRowStillBumps(t *testing.T) {
	_, cache := claudeGenerationFixture(t, 9103)
	base := time.Now().Add(-time.Hour)
	claudeReading(t, cache, "", base, map[string]time.Time{
		claudeWindowFiveHour: base.Add(10 * time.Minute),
		claudeWindowSevenDay: base,
	})
	first := claudeGenerationOf(t, cache)
	claudeReading(t, cache, "", base, map[string]time.Time{claudeWindowSevenDay: base.Add(5 * time.Minute)})
	if next := claudeGenerationOf(t, cache); next.Counter != first.Counter+1 {
		t.Fatalf("the weekly advance did not bump: %+v -> %+v", first, next)
	}
}

// A write made outside the agent process (the status-line hook) is stamped by
// the watcher on a later tick — after the startup hint and its follow-up have
// gone idle.
func TestClaudeGeneration_TheWatcherStampsAnOutOfProcessWrite(t *testing.T) {
	rec, cache := claudeGenerationFixture(t, 9104)
	now := time.Now()
	cliUsagePropagatorRunning.Store(false) // the hook's process does not stamp
	claudeReading(t, cache, "", now, map[string]time.Time{claudeWindowFiveHour: now})
	cliUsagePropagatorRunning.Store(true)
	if snap := claudeCacheSnapshot(t, cache); snap.Generation != 0 {
		t.Fatalf("a non-agent merge stamped: %+v", snap)
	}

	cliUsagePropagator.mu.Lock()
	cliUsageWatchPeriod = 30 * time.Millisecond
	cliUsagePropagator.armWatchLocked()
	cliUsagePropagator.mu.Unlock()

	h := waitHints(t, rec, 1, 0)[0].hint
	if h.Provider != claudeUsageProvider || h.Generation != 1 || claudeGenerationOf(t, cache) != gen(9104, 1) {
		t.Fatalf("hint = %+v, cache generation = %+v", h, claudeGenerationOf(t, cache))
	}
}

// A dual-channel install: our cache is still scoped to the previous account,
// while the installed hook's pinned cache already holds a reading for the
// account signed in now. The watcher moves our cache off the scope it sampled
// before the credential read and stamps; a stamp from any other scope (the
// cache moved again in between) is still refused.
func TestClaudeGeneration_TheWatcherStampsAPinnedReadingAfterAnAccountTransition(t *testing.T) {
	rec, cache := claudeGenerationFixture(t, 9108)
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	pinned := filepath.Join(t.TempDir(), "pinned", "rl.json")
	helperWriteJSON(t, filepath.Join(configDir, "settings.json"), map[string]any{
		"statusLine": map[string]any{
			"type": "command",
			"command": "AIEXPEDITE_CLAUDE_RL_CACHE=" + posixSingleQuote(pinned) +
				" AIEXPEDITE_CLAUDE_STATUSLINE_PREV='/tmp/prev.json'" +
				" '/opt/aiexpedite/aiexpedite-terminal' " + statusLineHookArg,
		},
	})
	now := time.Now()
	claudeReading(t, cache, "previous-account", now.Add(-time.Hour), map[string]time.Time{claudeWindowFiveHour: now.Add(-time.Hour)})
	before := claudeGenerationOf(t, cache)

	writeClaudeAccountCredential(t, configDir, "ada@example.com")
	fp := currentClaudeAccountFingerprint()
	if fp == "" {
		t.Fatal("the credential fixture resolved to an unscoped account; this case needs a scoped one")
	}
	cliUsagePropagatorRunning.Store(false) // the hook's process does not stamp
	claudeReading(t, pinned, fp, now, map[string]time.Time{claudeWindowFiveHour: now})
	cliUsagePropagatorRunning.Store(true)

	if _, bumped, _ := claudeStampWatchedGeneration(fp, "some-third-account"); bumped {
		t.Fatal("a stamp moved a cache off a scope it never sampled")
	}
	if snap := claudeCacheSnapshot(t, cache); snap.AccountFingerprint != "previous-account" {
		t.Fatalf("a refused stamp re-scoped the cache to %q", snap.AccountFingerprint)
	}

	resetClaudeUsageWatchState()
	t.Cleanup(resetClaudeUsageWatchState)
	claudeWatchStampCheck()
	snap := claudeCacheSnapshot(t, cache)
	if snap.AccountFingerprint != fp || snap.GenerationEpoch != 9108 || snap.Generation <= before.Counter {
		t.Fatalf("after the watcher: scope=%q generation={%d,%d}, want %q above %+v",
			snap.AccountFingerprint, snap.GenerationEpoch, snap.Generation, fp, before)
	}
	waitHints(t, rec, 1, 0)
	found := false
	for _, h := range rec.all() {
		found = found || (h.hint.Provider == claudeUsageProvider && h.hint.Generation == snap.Generation)
	}
	if !found {
		t.Fatalf("the pinned reading's generation %d was not hinted: %+v", snap.Generation, rec.all())
	}
}

// A stamp refused because the credential read transiently failed (an empty
// fingerprint against a scoped cache) stays pending: a later tick retries it
// although neither cache file moved. The retries are bounded, so a refusal
// that persists stops costing a credential read every tick.
func TestClaudeGeneration_TheWatcherRetriesAStampRefusedByACredentialReadFailure(t *testing.T) {
	_, cache := claudeGenerationFixture(t, 9109)
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	pinned := filepath.Join(t.TempDir(), "pinned", "rl.json")
	helperWriteJSON(t, filepath.Join(configDir, "settings.json"), map[string]any{
		"statusLine": map[string]any{
			"type": "command",
			"command": "AIEXPEDITE_CLAUDE_RL_CACHE=" + posixSingleQuote(pinned) +
				" AIEXPEDITE_CLAUDE_STATUSLINE_PREV='/tmp/prev.json'" +
				" '/opt/aiexpedite/aiexpedite-terminal' " + statusLineHookArg,
		},
	})
	now := time.Now()
	claudeReading(t, cache, "previous-account", now.Add(-time.Hour), map[string]time.Time{claudeWindowFiveHour: now.Add(-time.Hour)})
	before := claudeGenerationOf(t, cache)

	writeClaudeAccountCredential(t, configDir, "ada@example.com")
	fp := currentClaudeAccountFingerprint()
	if fp == "" {
		t.Fatal("the credential fixture resolved to an unscoped account; this case needs a scoped one")
	}
	cliUsagePropagatorRunning.Store(false) // the hook's process does not stamp
	claudeReading(t, pinned, fp, now, map[string]time.Time{claudeWindowFiveHour: now})
	cliUsagePropagatorRunning.Store(true)

	credential := filepath.Join(configDir, ".credentials.json")
	if err := os.Remove(credential); err != nil {
		t.Fatal(err)
	}
	resetClaudeUsageWatchState()
	t.Cleanup(resetClaudeUsageWatchState)
	claudeWatchStampCheck()
	if snap := claudeCacheSnapshot(t, cache); snap.AccountFingerprint != "previous-account" || snap.Generation != before.Counter {
		t.Fatalf("a stamp under an empty fingerprint wrote: scope=%q generation=%d", snap.AccountFingerprint, snap.Generation)
	}

	writeClaudeAccountCredential(t, configDir, "ada@example.com")
	claudeWatchStampCheck() // neither file moved since the refused tick
	snap := claudeCacheSnapshot(t, cache)
	if snap.AccountFingerprint != fp || snap.Generation <= before.Counter {
		t.Fatalf("the refused stamp was not retried: scope=%q generation=%d, want %q above %d",
			snap.AccountFingerprint, snap.Generation, fp, before.Counter)
	}

	// Bounded: once the retries are spent on a refusal that persists, an
	// unchanged file is left alone until it moves again.
	resetClaudeUsageWatchState()
	if err := os.Remove(credential); err != nil {
		t.Fatal(err)
	}
	cliUsagePropagatorRunning.Store(false)
	claudeReading(t, cache, "previous-account", now.Add(-time.Hour), map[string]time.Time{claudeWindowFiveHour: now.Add(-time.Hour)})
	cliUsagePropagatorRunning.Store(true)
	for range claudeWatchStampMaxRetries + 1 {
		claudeWatchStampCheck()
	}
	claudeUsageWatchState.mu.Lock()
	retries := claudeUsageWatchState.stampRetries
	claudeUsageWatchState.mu.Unlock()
	if retries != 0 {
		t.Fatalf("stampRetries = %d after %d refused ticks, want the retries spent", retries, claudeWatchStampMaxRetries+1)
	}
}

// An account flip drops the row set with the rest of the account's state, but
// the counter keeps rising within the epoch, so the new account's first
// reading is never `already_applied` on the server. Only the signed-in
// fingerprint is noted.
func TestClaudeGeneration_AnAccountFlipKeepsTheCounterMonotonic(t *testing.T) {
	rec, cache := claudeGenerationFixture(t, 9105)
	now := time.Now()
	claudeReading(t, cache, "", now, map[string]time.Time{claudeWindowFiveHour: now})
	waitHints(t, rec, 1, 0)
	claudeReading(t, cache, "other-account", now, map[string]time.Time{claudeWindowFiveHour: now})
	snap := claudeCacheSnapshot(t, cache)
	if snap.GenerationEpoch != 9105 || snap.Generation != 2 {
		t.Fatalf("after the flip generation = {%d,%d}, want {9105,2}", snap.GenerationEpoch, snap.Generation)
	}
	time.Sleep(3 * cliUsageHintDebounce)
	for _, h := range rec.all() {
		if h.hint.Generation == 2 {
			t.Fatalf("a reading for an account not signed in was hinted: %+v", h.hint)
		}
	}
}

// A writer from an older binary re-serializes the snapshot without the
// generation fields; the next bump still goes above what this process
// committed.
func TestClaudeGeneration_AnOlderBinaryRewriteNeverRestartsTheCounter(t *testing.T) {
	_, cache := claudeGenerationFixture(t, 9106)
	now := time.Now()
	claudeReading(t, cache, "", now, map[string]time.Time{claudeWindowFiveHour: now})
	claudeReading(t, cache, "", now, map[string]time.Time{claudeWindowFiveHour: now.Add(time.Second)})
	snap := claudeCacheSnapshot(t, cache)
	snap.GenerationEpoch, snap.Generation, snap.GenerationRowObservedMs = 0, 0, nil
	raw, _ := json.Marshal(snap)
	if err := os.WriteFile(cache, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	claudeReading(t, cache, "", now, map[string]time.Time{claudeWindowFiveHour: now.Add(2 * time.Second)})
	if g := claudeGenerationOf(t, cache); g != gen(9106, 3) {
		t.Fatalf("generation = %+v, want {9106,3}", g)
	}
}

// The statusline-hook subcommand runs in its own process, where the
// propagator never starts: its merges must never stamp.
func TestClaudeGeneration_AMergeOutsideTheAgentProcessNeverStamps(t *testing.T) {
	withCodexGenerationEpoch(t, 9107)
	cache := filepath.Join(t.TempDir(), "claude_rate_limits.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now()
	claudeReading(t, cache, "", now, map[string]time.Time{claudeWindowFiveHour: now})
	if snap := claudeCacheSnapshot(t, cache); snap.Generation != 0 || snap.GenerationEpoch != 0 {
		t.Fatalf("a merge outside the agent stamped: %+v", snap)
	}
}

func TestClaudeGeneration_TheParserPublishesOnlyThisProcessEpoch(t *testing.T) {
	_, cache := claudeGenerationFixture(t, 9108)
	now := time.Now()
	claudeReading(t, cache, "", now, map[string]time.Time{claudeWindowFiveHour: now})
	usage, _ := claudeCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{}, time.Now())
	if usage.UsageGeneration == nil || *usage.UsageGeneration != gen(9108, 1) {
		t.Fatalf("UsageGeneration = %+v, want {9108,1}", usage.UsageGeneration)
	}
	cliUsageProcessGenerationEpoch.Store(9109) // a new process before its rotation
	usage, _ = claudeCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{}, time.Now())
	if usage.UsageGeneration != nil {
		t.Fatalf("published another epoch's generation: %+v", usage.UsageGeneration)
	}
}

// The new snapshot fields are integers only.
func TestClaudeGeneration_SnapshotFieldsAreIntegersOnly(t *testing.T) {
	_, cache := claudeGenerationFixture(t, 9110)
	now := time.Now()
	claudeReading(t, cache, "", now, map[string]time.Time{claudeWindowFiveHour: now})
	raw, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	for _, key := range []string{"generationEpoch", "generation"} {
		var n int64
		if json.Unmarshal(fields[key], &n) != nil || n <= 0 {
			t.Fatalf("%s = %s, want a positive integer", key, fields[key])
		}
	}
	var rows []int64
	if err := json.Unmarshal(fields["generationRowObservedMs"], &rows); err != nil || len(rows) == 0 {
		t.Fatalf("generationRowObservedMs = %s, want integers", fields["generationRowObservedMs"])
	}
}
