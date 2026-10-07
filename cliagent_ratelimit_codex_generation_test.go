package main

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_ratelimit_codex_generation_test.go — the capture generation
   {GenerationEpoch, Generation} that lets the backend tell which device-side
   state it has applied. It moves on every committed change, restarts at 1 under
   each process's random epoch, and is never published under an epoch this
   process did not write.
   ------------------------------------------------------------------------ */

// withCodexGenerationEpoch pins this "process" to epoch with no rotation
// committed yet, and resets the propagator, restoring both afterwards.
func withCodexGenerationEpoch(t *testing.T, epoch int64) {
	t.Helper()
	prevEpoch, prevRotated := cliUsageProcessGenerationEpoch.Load(), codexGenerationRotated.Load()
	cliUsageProcessGenerationEpoch.Store(epoch)
	codexGenerationRotated.Store(false)
	resetClaudeGenerationProcessState()
	resetCLIUsagePropagator()
	t.Cleanup(func() {
		resetCLIUsagePropagator()
		cliUsageProcessGenerationEpoch.Store(prevEpoch)
		codexGenerationRotated.Store(prevRotated)
		resetClaudeGenerationProcessState()
	})
}

// simulateCodexProcessRestart is a fresh agent process over the same cache
// file: a new epoch, nothing rotated, the propagator's memory gone.
func simulateCodexProcessRestart(t *testing.T, epoch int64) {
	t.Helper()
	cliUsageProcessGenerationEpoch.Store(epoch)
	codexGenerationRotated.Store(false)
	resetClaudeGenerationProcessState()
	resetCLIUsagePropagator()
	simulateCodexAgentRestart(t)
}

func codexLiveReadAt(t *testing.T, f codexFreshnessFixture, primaryPct, secondaryPct int, at time.Time) {
	t.Helper()
	if !captureCodexRateLimitLineForAccount(codexLiveReadEnvelope(primaryPct, secondaryPct, at), at, f.fp) {
		t.Fatal("live read did not land")
	}
}

func TestCodexGeneration_EveryDistinctReadingAdvancesItEvenWithinOneSecond(t *testing.T) {
	withCodexGenerationEpoch(t, 101)
	now := time.Now().Truncate(time.Second)
	f := newCodexFreshnessFixture(t, now.Add(-time.Hour))

	codexLiveReadAt(t, f, 10, 20, now)
	first := f.snapshot(t)
	codexLiveReadAt(t, f, 11, 20, now.Add(200*time.Millisecond))
	second := f.snapshot(t)

	if first.GenerationEpoch != 101 || first.Generation != 1 {
		t.Fatalf("first write = {%d,%d}, want {101,1}", first.GenerationEpoch, first.Generation)
	}
	if second.GenerationEpoch != 101 || second.Generation <= first.Generation {
		t.Fatalf("a second reading in the same second did not advance: %d → %d", first.Generation, second.Generation)
	}
}

func TestCodexGeneration_ANoOpMergeDoesNotAdvanceIt(t *testing.T) {
	withCodexGenerationEpoch(t, 102)
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-time.Hour))
	codexLiveReadAt(t, f, 10, 20, now)
	before := f.snapshot(t).Generation

	// An older restatement of the same limit is refused by the merge, and a pure
	// bookkeeping write touches no contributor.
	captureCodexRateLimitLineForAccount(codexLiveReadEnvelope(90, 90, now.Add(-time.Hour)), now.Add(-time.Hour), f.fp)
	codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) { snap.NextAttemptAtMs = 0 })

	if after := f.snapshot(t).Generation; after != before {
		t.Fatalf("a write that changed nothing moved the generation %d → %d", before, after)
	}
}

func TestCodexGeneration_AnAccountRescopeAlwaysAdvancesIt(t *testing.T) {
	withCodexGenerationEpoch(t, 103)
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-time.Hour))
	codexLiveReadAt(t, f, 10, 20, now)
	before := f.snapshot(t)

	// Account B signs in within the same second and writes nothing numeric yet.
	helperCodexAuthAt(t, f.home, "other@example.com", now)
	fpB := currentCodexAccountFingerprint()
	codexRecordRunFreshness(fpB, now, func(*codexRateLimitSnapshot) {})

	after := f.snapshot(t)
	if after.AccountFingerprint != fpB || after.Generation <= before.Generation {
		t.Fatalf("a rescope to another account kept generation %d (was %d)", after.Generation, before.Generation)
	}
}

func TestCodexGeneration_FirstWriteInANewProcessStampsItsEpochAtOne(t *testing.T) {
	withCodexGenerationEpoch(t, 104)
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-time.Hour))
	codexLiveReadAt(t, f, 10, 20, now)
	codexLiveReadAt(t, f, 12, 20, now.Add(time.Second))

	simulateCodexProcessRestart(t, 205)
	codexLiveReadAt(t, f, 14, 20, now.Add(2*time.Second))
	if snap := f.snapshot(t); snap.GenerationEpoch != 205 || snap.Generation != 1 {
		t.Fatalf("after a restart = {%d,%d}, want {205,1}", snap.GenerationEpoch, snap.Generation)
	}

	// A deleted cache file is the same case.
	if err := os.Remove(f.cache); err != nil {
		t.Fatal(err)
	}
	simulateCodexProcessRestart(t, 306)
	codexLiveReadAt(t, f, 16, 20, now.Add(3*time.Second))
	if snap := f.snapshot(t); snap.GenerationEpoch != 306 || snap.Generation != 1 {
		t.Fatalf("after a deleted cache = {%d,%d}, want {306,1}", snap.GenerationEpoch, snap.Generation)
	}
}

// A pre-generation cache (written before these fields existed) gains an epoch
// on its first write.
func TestCodexGeneration_APreGenerationCacheGainsAnEpochOnFirstWrite(t *testing.T) {
	withCodexGenerationEpoch(t, 107)
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	codexRateLimitCacheTransaction(context.Background(), f.cache, now, true, func(snap *codexRateLimitSnapshot) bool {
		snap.GenerationEpoch, snap.Generation = 0, 0
		return true
	})
	codexLiveReadAt(t, f, 30, 40, now)
	if snap := f.snapshot(t); snap.GenerationEpoch != 107 || snap.Generation != 1 {
		t.Fatalf("= {%d,%d}, want {107,1}", snap.GenerationEpoch, snap.Generation)
	}
}

// Before this process's epoch is on disk, a receipt carries no generation, so
// the backend records none for a snapshot it cannot attribute.
func TestCodexGeneration_ReceiptOmitsItUntilTheRotationCommits(t *testing.T) {
	withCodexGenerationEpoch(t, 108)
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-time.Hour))
	codexLiveReadAt(t, f, 10, 20, now)

	simulateCodexProcessRestart(t, 209)
	metrics, generation := codexMetricsAndGenerationFromCache(now, f.fp)
	if generation != nil {
		t.Fatalf("a restored snapshot was published under generation %+v", generation)
	}
	if len(metrics) == 0 || metrics[0].Consumed == nil {
		t.Fatalf("the metrics themselves are still published: %+v", metrics)
	}

	if rotated, _ := codexRotateGenerationEpoch(now); !rotated {
		t.Fatal("rotation did not commit")
	}
	_, generation = codexMetricsAndGenerationFromCache(now, f.fp)
	if generation == nil || generation.Epoch != 209 || generation.Counter != 1 {
		t.Fatalf("after rotation = %+v, want {209,1}", generation)
	}
}

// An older snapshot restored with the SAME epoch the backend applied — and a
// counter below the applied one — is rotated at start, so its next hint names
// an epoch the backend has never applied and is never `already_applied`.
func TestCodexGeneration_ARestoredSnapshotIsRotatedAtStart(t *testing.T) {
	withCodexGenerationEpoch(t, 110)
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-time.Hour))
	codexLiveReadAt(t, f, 10, 20, now)
	backup, err := os.ReadFile(f.cache)
	if err != nil {
		t.Fatal(err)
	}
	codexLiveReadAt(t, f, 11, 20, now.Add(time.Second))
	codexLiveReadAt(t, f, 12, 20, now.Add(2*time.Second))
	applied := cliUsageGeneration{Epoch: 110, Counter: f.snapshot(t).Generation}

	if err := os.WriteFile(f.cache, backup, 0o600); err != nil {
		t.Fatal(err)
	}
	simulateCodexProcessRestart(t, 211)
	codexRotateGenerationEpoch(now)
	_, g := codexMetricsAndGenerationFromCache(now, f.fp)
	if g == nil || g.Epoch == applied.Epoch {
		t.Fatalf("restored snapshot published as %+v; backend holds %+v", g, applied)
	}
	if backendAlreadyApplied(applied, *g) {
		t.Fatal("the restored snapshot's hint would be skipped as already applied")
	}
}

// backendAlreadyApplied mirrors terminal-service's usageObservedSkipReason.
func backendAlreadyApplied(stored, hint cliUsageGeneration) bool {
	return stored.Epoch == hint.Epoch && stored.Counter >= hint.Counter
}

// Two process starts under an identical frozen wall clock still draw distinct
// epochs: the draw is random, not the clock. Only a forced collision through
// the seam reproduces `already_applied` — the documented 2^-53 residual.
func TestCodexGeneration_EpochsAreRandomNotClockDerived(t *testing.T) {
	seen := map[int64]bool{}
	for i := 0; i < 64; i++ {
		e := cliUsageDrawGenerationEpoch()
		if e < 1 || e > cliUsageMaxSafeInteger {
			t.Fatalf("epoch %d outside [1, 2^53-1]", e)
		}
		if seen[e] {
			t.Fatalf("duplicate epoch %d within 64 draws", e)
		}
		seen[e] = true
	}

	withCodexGenerationEpoch(t, 1)
	frozen := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	prevNow := codexUsageFreshnessNow
	codexUsageFreshnessNow = func() time.Time { return frozen }
	t.Cleanup(func() { codexUsageFreshnessNow = prevNow })
	f := newCodexFreshnessFixture(t, frozen.Add(-time.Hour))

	firstEpoch := cliUsageDrawGenerationEpoch()
	simulateCodexProcessRestart(t, firstEpoch)
	codexLiveReadAt(t, f, 10, 20, frozen)
	codexLiveReadAt(t, f, 11, 20, frozen.Add(time.Second))
	applied := cliUsageGeneration{Epoch: firstEpoch, Counter: 50} // backend ahead

	secondEpoch := cliUsageDrawGenerationEpoch()
	simulateCodexProcessRestart(t, secondEpoch)
	codexRotateGenerationEpoch(frozen)
	_, g := codexMetricsAndGenerationFromCache(frozen, f.fp)
	if g == nil || backendAlreadyApplied(applied, *g) {
		t.Fatalf("second process's generation %+v was suppressed by %+v", g, applied)
	}

	// Forced collision: the only way to get `already_applied` for new state.
	simulateCodexProcessRestart(t, firstEpoch)
	codexRotateGenerationEpoch(frozen)
	codexLiveReadAt(t, f, 12, 20, frozen.Add(2*time.Second))
	_, g = codexMetricsAndGenerationFromCache(frozen, f.fp)
	if g == nil || !backendAlreadyApplied(applied, *g) {
		t.Fatalf("a forced epoch collision should be the one already_applied case: %+v", g)
	}
}

// The generation ParseContext publishes always belongs to the snapshot its
// metrics came from, even with a merge racing the read.
func TestCodexGeneration_ParseContextPairsMetricsWithTheirOwnGeneration(t *testing.T) {
	withCodexGenerationEpoch(t, 112)
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-time.Hour))
	stubCodexLogin(t, true, true)
	codexLiveReadAt(t, f, 10, 20, now)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		pct := 21
		for {
			select {
			case <-stop:
				return
			default:
			}
			captureCodexRateLimitLineForAccount(codexLiveReadEnvelope(pct, pct, time.Now()), time.Now(), f.fp)
			pct++
			if pct > 99 {
				pct = 21
			}
		}
	}()
	defer func() { close(stop); wg.Wait() }()

	published := 0
	for i := 0; i < 20; i++ {
		usage, ok := codexUsageParser{}.ParseContext(context.Background(), f.home, detectedCLIAgent{Path: "codex"}, time.Now())
		if !ok {
			t.Fatalf("parse = %+v", usage)
		}
		if usage.UsageGeneration == nil {
			// A read that lost a race with the atomic rename (a sharing violation
			// on Windows) sees no snapshot at all: no metrics AND no generation,
			// which is still one consistent read.
			for _, m := range usage.Metrics {
				if m.Consumed != nil {
					t.Fatalf("numeric metrics published without their generation: %+v", usage.Metrics)
				}
			}
			continue
		}
		published++
		session := codexSessionMetric(t, usage.Metrics)
		// Every generation above 1 was written by the racing capture with
		// primary == secondary == its own percentage; the published pair must
		// agree with each other, i.e. come from one read.
		weekly := codexWeeklyMetric(t, usage.Metrics)
		if usage.UsageGeneration.Counter > 1 && (session.Consumed == nil || weekly.Consumed == nil || *session.Consumed != *weekly.Consumed) {
			t.Fatalf("metrics %v/%v do not come from one snapshot (generation %+v)", *session.Consumed, *weekly.Consumed, usage.UsageGeneration)
		}
	}
	if published == 0 {
		t.Fatal("no parse published a generation")
	}
}

// resetClaudeGenerationProcessState is the Claude half of a fresh "process":
// nothing rotated, nothing committed under this epoch, the watcher's memory
// gone.
func resetClaudeGenerationProcessState() {
	claudeGenerationRotated.Store(false)
	claudeLastCommittedGeneration.Store(0)
	resetClaudeUsageWatchState()
}
