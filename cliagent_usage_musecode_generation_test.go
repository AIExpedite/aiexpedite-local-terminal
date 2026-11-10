package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_musecode_generation_test.go — Muse Code as a fresh-usage hint
   provider: the live cache's capture generation, the hint a committed reading
   sends (and every reading that sends none), what the parser publishes and
   reserves, the propagator's rotation gate, and startup recovery. Hints are
   recorded through propagatorFixture's seam, so nothing reaches the network.
   ------------------------------------------------------------------------ */

// museHintFixture isolates every Muse file and the home folder, pins this
// "process" to epoch with nothing committed yet, and records hints on a
// propagator the test starts only when it needs one to send.
func museHintFixture(t *testing.T, epoch int64) (home string, rec *cliUsageHintRecorder, cfg *Config) {
	t.Helper()
	withCodexGenerationEpoch(t, epoch)
	home = isolateMuseCodeHome(t)
	rec, cfg = propagatorFixture(t)
	return home, rec, cfg
}

// museRunRecordAt is the real run.usage record observed at asOf, with these
// figures and reset times.
func museRunRecordAt(windowPct, weeklyPct int, asOf, windowReset, weeklyReset time.Time) string {
	r := museRunUsageWith(windowPct, weeklyPct, asOf)
	r = strings.Replace(r, `"resets_at_ms":1794169407000`, `"resets_at_ms":`+strconv.FormatInt(windowReset.UnixMilli(), 10), 1)
	return strings.Replace(r, `"resets_at_ms":1794182400000`, `"resets_at_ms":`+strconv.FormatInt(weeklyReset.UnixMilli(), 10), 1)
}

// museRunRecordNow is a run.usage record observed at asOf whose windows are
// still open. windowPct must not be 99 (museRunUsageWith's replacement order).
func museRunRecordNow(windowPct, weeklyPct int, asOf time.Time) string {
	return museRunRecordAt(windowPct, weeklyPct, asOf, asOf.Add(2*time.Hour), asOf.Add(72*time.Hour))
}

// museCacheReadingJSON is one cached reading as the cache file spells it.
func museCacheReadingJSON(accountKey string, observed, windowReset, weeklyReset time.Time) string {
	return fmt.Sprintf(`{"accountKey":%q,"observedAtMs":%d,"window":{"usedPercent":12,"windowDurationMins":300,"resetsAtMs":%d},"weekly":{"usedPercent":40,"resetsAtMs":%d}}`,
		accountKey, observed.UnixMilli(), windowReset.UnixMilli(), weeklyReset.UnixMilli())
}

// museCacheFileJSON is a schema-2 cache file; generation nil leaves the field
// out, exactly as every build before this one wrote it.
func museCacheFileJSON(generation *cliUsageGeneration, readings ...string) string {
	g := ""
	if generation != nil {
		g = fmt.Sprintf(`,"generation":{"epoch":%d,"counter":%d}`, generation.Epoch, generation.Counter)
	}
	return `{"schemaVersion":2,"readings":[` + strings.Join(readings, ",") + `]` + g + `}`
}

// writeMuseCacheFile replaces the cache file, as another process or an older
// build would have left it.
func writeMuseCacheFile(t *testing.T, raw string) {
	t.Helper()
	if err := os.WriteFile(museCodeUsageLiveCachePath(), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// museCacheGeneration is the generation the cache file on disk carries.
func museCacheGeneration() *cliUsageGeneration {
	museCodeUsageLiveCacheMu.Lock()
	defer museCodeUsageLiveCacheMu.Unlock()
	return readMuseCodeUsageLiveFileLocked().Generation
}

// musePendingHint is the Muse Code generation the propagator holds, if any.
func musePendingHint() (cliUsageGeneration, bool) {
	p := cliUsagePropagator
	p.mu.Lock()
	defer p.mu.Unlock()
	if h := p.pending[museCodeUsageProvider]; h != nil {
		return h.generation, true
	}
	return cliUsageGeneration{}, false
}

// genPtr is a generation literal's address.
func genPtr(epoch, counter int64) *cliUsageGeneration {
	g := gen(epoch, counter)
	return &g
}

/* ------------------------------ Committed hint ------------------------------ */

// A run the Chat manager drives commits its allowance: the cache's generation
// advances on this process's epoch and exactly ONE generation is hinted — its
// first send, with the follow-up a spacing later — and the parser publishes
// that same generation with the reading.
func TestMuseCodeUsageHint_ACommittedRunReadingSendsOneHint(t *testing.T) {
	home, rec, cfg := museHintFixture(t, 9301)
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	startCLIUsagePropagator(cfg)
	cliUsagePropagator.rotating.Wait() // startup recovery: an empty cache, nothing

	now := time.Now()
	observe := museCodeNativeSpec.ObserveRun()
	observe(`{"payload_type":"runtime.command.accepted","payload":{}}`)
	observe(museRunRecordNow(3, 40, now))
	if g := museCacheGeneration(); g == nil || *g != gen(9301, 1) {
		t.Fatalf("cache generation = %+v, want {9301,1}", g)
	}
	hints := waitHints(t, rec, 1, cliUsageHintSpacing/2)
	if len(hints) != 1 {
		t.Fatalf("got %d hints before the follow-up boundary, want exactly 1: %+v", len(hints), hints)
	}
	h := hints[0].hint
	if h.Provider != museCodeUsageProvider || h.GenerationEpoch != 9301 || h.Generation != 1 {
		t.Fatalf("hint = %+v, want museCode {9301,1}", h)
	}
	if h.Signature != generateHMAC(buildCLIUsageObservedSignedMessage("agent-1", h.Timestamp, h.Provider, gen(9301, 1)), "secret") {
		t.Fatal("the hint's signature does not cover its provider and generation")
	}

	usage, _ := museCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !museCodeUsageMetricsNumeric(usage.Metrics) {
		t.Fatalf("metrics = %#v, want the run's reading", usage.Metrics)
	}
	if usage.UsageGeneration == nil || *usage.UsageGeneration != gen(9301, 1) {
		t.Fatalf("published generation = %+v, want the hinted {9301,1}", usage.UsageGeneration)
	}
	// Reading the same commit again is not a discovery: it stays one generation.
	if g, ok := musePendingHint(); ok && g != gen(9301, 1) {
		t.Fatalf("pending = %+v after a re-read, want nothing newer than {9301,1}", g)
	}
}

// A run's reading that is not committed sends nothing: an older reading, one
// read after a login switch mid-run, and one whose write failed.
func TestMuseCodeUsageHint_NothingIsHintedWithoutACommittedReading(t *testing.T) {
	t.Run("an older reading", func(t *testing.T) {
		home, _, _ := museHintFixture(t, 9302)
		writeMuseConfig(t, home, "auth.json", museAuthFixture)
		now := time.Now()
		if !newMuseCodeRunUsageCapture().Line(museRunRecordNow(3, 40, now), now) {
			t.Fatal("the first reading must be kept")
		}
		first, ok := musePendingHint()
		if !ok || first != gen(9302, 1) {
			t.Fatalf("pending = %+v ok=%t, want {9302,1}", first, ok)
		}
		for name, asOf := range map[string]time.Time{"older": now.Add(-time.Minute), "same instant": now} {
			if newMuseCodeRunUsageCapture().Line(museRunRecordNow(5, 50, asOf), now) {
				t.Fatalf("%s: a reading not newer than the cached one was kept", name)
			}
		}
		if g := museCacheGeneration(); g == nil || *g != gen(9302, 1) {
			t.Fatalf("cache generation = %+v, want it unmoved at {9302,1}", g)
		}
		if g, _ := musePendingHint(); g != first {
			t.Fatalf("pending = %+v, want only the first reading's {9302,1}", g)
		}
	})

	t.Run("a login switch mid-run", func(t *testing.T) {
		home, _, _ := museHintFixture(t, 9303)
		writeMuseConfig(t, home, "auth.json", museAuthFixture)
		launched := newMuseCodeRunUsageCapture()
		writeMuseConfig(t, home, "auth.json", museOtherAuthFixture)
		now := time.Now()
		if launched.Line(museRunRecordNow(3, 40, now), now) {
			t.Fatal("a reading read after a login switch was kept")
		}
		if g := museCacheGeneration(); g != nil {
			t.Fatalf("cache generation = %+v, want none", g)
		}
		if g, ok := musePendingHint(); ok {
			t.Fatalf("hinted %+v for a reading that was never saved", g)
		}
		if museCodeGenerationRotated.Load() {
			t.Fatal("the cache counts as rotated with nothing committed")
		}
	})

	t.Run("a write that did not commit", func(t *testing.T) {
		home, _, _ := museHintFixture(t, 9304)
		writeMuseConfig(t, home, "auth.json", museAuthFixture)
		// The cache's folder is a regular file: no write can land.
		blocker := filepath.Join(t.TempDir(), "not-a-folder")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AIEXPEDITE_MUSECODE_USAGE_LIVE_CACHE", filepath.Join(blocker, "musecode_usage_live.json"))
		now := time.Now()
		if newMuseCodeRunUsageCapture().Line(museRunRecordNow(3, 40, now), now) {
			t.Fatal("a reading whose write failed reported kept")
		}
		if g, ok := musePendingHint(); ok {
			t.Fatalf("hinted %+v for a write that never committed", g)
		}
		if museCodeGenerationRotated.Load() {
			t.Fatal("a failed write rotated the cache")
		}
		// The next write that commits starts at counter 1: the failed one
		// reached no one, so its number is not spent.
		t.Setenv("AIEXPEDITE_MUSECODE_USAGE_LIVE_CACHE", filepath.Join(t.TempDir(), "musecode_usage_live.json"))
		if !newMuseCodeRunUsageCapture().Line(museRunRecordNow(3, 40, now), now) {
			t.Fatal("the reading must be kept once the cache is writable")
		}
		if g, ok := musePendingHint(); !ok || g != gen(9304, 1) {
			t.Fatalf("pending = %+v ok=%t, want {9304,1}", g, ok)
		}
	})
}

// A Refresh probe's reading commits like a run's, and is hinted.
func TestMuseCodeUsageHint_AProbeReadingSendsAHint(t *testing.T) {
	home, rec, cfg := museHintFixture(t, 9305)
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	now := time.Now()
	fake := &fakeMuseServe{usage: museUsageJSON(now.Add(time.Hour), now.Add(72*time.Hour), now)}
	startMuseCodeMSPFn = fake.start
	startCLIUsagePropagator(cfg)
	cliUsagePropagator.rotating.Wait()

	if outcome := probeMuseCodeUsageLive(context.Background(), detectedCLIAgent{Detected: true, Path: "/x/muse"}, time.Now); outcome != liveProbeOutcomeOK {
		t.Fatalf("outcome = %q", outcome)
	}
	if g := museCacheGeneration(); g == nil || *g != gen(9305, 1) {
		t.Fatalf("cache generation = %+v, want {9305,1}", g)
	}
	if h := waitHintFor(t, rec, gen(9305, 1)).hint; h.Provider != museCodeUsageProvider {
		t.Fatalf("hint = %+v, want museCode", h)
	}
}

// The hint is noted only after the cache transaction released its lock: with
// the propagator's lock held, a commit still lands and the cache stays
// readable while the hint waits.
func TestMuseCodeUsageHint_TheHintIsNotedOutsideTheCacheLock(t *testing.T) {
	home, _, _ := museHintFixture(t, 9306)
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	p := cliUsagePropagator
	p.mu.Lock()
	held := true
	release := func() {
		if held {
			held = false
			p.mu.Unlock()
		}
	}
	t.Cleanup(release) // runs before the fixture's own cleanup, which needs the lock

	now := time.Now()
	kept := make(chan bool, 1)
	go func() { kept <- newMuseCodeRunUsageCapture().Line(museRunRecordNow(3, 40, now), now) }()
	readable := make(chan struct{})
	go func() {
		for {
			if g := museCacheGeneration(); g != nil && *g == gen(9306, 1) {
				close(readable)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	select {
	case <-readable:
	case <-time.After(5 * time.Second):
		release()
		t.Fatal("the cache stayed locked while its hint waited for the propagator")
	}
	select {
	case <-kept:
		release()
		t.Fatal("the save returned without noting its hint")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	if !<-kept {
		t.Fatal("the reading must be kept")
	}
	if g, ok := musePendingHint(); !ok || g != gen(9306, 1) {
		t.Fatalf("pending = %+v ok=%t, want {9306,1}", g, ok)
	}
}

/* ------------------------------ The generation ------------------------------ */

// A file an earlier process left publishes no generation, and neither does one
// naming this process's epoch before anything here committed; this process's
// first write publishes, counting up from the file's counter on its epoch.
func TestMuseCodeUsageGeneration_NothingIsPublishedBeforeThisProcessCommits(t *testing.T) {
	home, _, _ := museHintFixture(t, 9307)
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	now := time.Now()
	reading := museCacheReadingJSON(museDevAccountKey(), now.Add(-time.Minute), now.Add(time.Hour), now.Add(72*time.Hour))
	parse := func() (*cliAgentUsage, *cliUsageReceiptReservation) {
		ctx, reservation := withCLIUsageReceiptReservation(context.Background())
		usage, ok := museCodeUsageParser{}.ParseContext(ctx, home, detectedCLIAgent{Detected: true}, now)
		if !ok || usage == nil || !museCodeUsageMetricsNumeric(usage.Metrics) {
			t.Fatalf("usage = %#v, want the cached reading rendered", usage)
		}
		return usage, reservation
	}

	// In this order: the file the commit below counts up from is the last one.
	for _, c := range []struct {
		name string
		g    *cliUsageGeneration
	}{
		{"an earlier process's epoch", genPtr(1234, 7)},
		{"this epoch, nothing committed here", genPtr(9307, 7)},
	} {
		writeMuseCacheFile(t, museCacheFileJSON(c.g, reading))
		usage, reservation := parse()
		if usage.UsageGeneration != nil {
			t.Fatalf("%s: published %+v", c.name, usage.UsageGeneration)
		}
		if len(reservation.generations) != 0 {
			t.Fatalf("%s: reserved %+v for the receipt", c.name, reservation.generations)
		}
	}

	if !newMuseCodeRunUsageCapture().Line(museRunRecordNow(4, 41, now), now) {
		t.Fatal("this process's reading must be kept")
	}
	usage, _ := parse()
	if usage.UsageGeneration == nil || *usage.UsageGeneration != gen(9307, 8) {
		t.Fatalf("published %+v, want {9307,8}", usage.UsageGeneration)
	}
}

// What every earlier build wrote — schema 2, no generation — still loads and
// renders; the next write versions it and keeps the other accounts' readings.
func TestMuseCodeUsageGeneration_ASchemaTwoFileWithoutAGenerationStillLoads(t *testing.T) {
	home, _, _ := museHintFixture(t, 9308)
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	now := time.Now()
	writeMuseCacheFile(t, museCacheFileJSON(nil,
		museCacheReadingJSON(museDevAccountKey(), now.Add(-time.Minute), now.Add(time.Hour), now.Add(72*time.Hour)),
		museCacheReadingJSON(museOtherAccountKey(), now.Add(-2*time.Minute), now.Add(time.Hour), now.Add(72*time.Hour))))

	got, ok := loadMuseCodeUsageLive(museDevAccountKey())
	if !ok || got.Window.UsedPercent != 12 || got.Weekly.UsedPercent != 40 {
		t.Fatalf("reading = %#v ok=%t", got, ok)
	}
	usage, _ := museCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !museCodeUsageMetricsNumeric(usage.Metrics) || usage.UsageGeneration != nil {
		t.Fatalf("metrics = %#v generation = %+v, want the reading without a generation", usage.Metrics, usage.UsageGeneration)
	}

	if !newMuseCodeRunUsageCapture().Line(museRunRecordNow(4, 41, now), now) {
		t.Fatal("a newer run reading must be kept")
	}
	if g := museCacheGeneration(); g == nil || *g != gen(9308, 1) {
		t.Fatalf("cache generation = %+v, want {9308,1}", g)
	}
	if _, ok := loadMuseCodeUsageLive(museOtherAccountKey()); !ok {
		t.Fatal("the other account's reading was dropped")
	}
}

// The counter never moves backwards within this process's epoch — not after
// the file read as empty (unreadable), nor after another process rewrote it
// under its own epoch. Reissuing a counter would make terminal-service skip
// its hint as already applied.
func TestMuseCodeUsageGeneration_TheCounterNeverRegresses(t *testing.T) {
	home, _, _ := museHintFixture(t, 9309)
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	base := time.Now()
	commit := func(step int) {
		t.Helper()
		at := base.Add(time.Duration(step) * time.Second)
		if !newMuseCodeRunUsageCapture().Line(museRunRecordNow(step, 40, at), at) {
			t.Fatalf("step %d: the reading must be kept", step)
		}
	}
	expect := func(counter int64) {
		t.Helper()
		if g := museCacheGeneration(); g == nil || *g != gen(9309, counter) {
			t.Fatalf("cache generation = %+v, want {9309,%d}", g, counter)
		}
		if g, ok := musePendingHint(); !ok || g != gen(9309, counter) {
			t.Fatalf("pending = %+v ok=%t, want {9309,%d}", g, ok, counter)
		}
	}
	commit(1)
	commit(2)
	expect(2)

	writeMuseCacheFile(t, `{"schemaVersion":2,"readings":[{"accountKey":`)
	if readings := museRunReadings(); len(readings) != 0 {
		t.Fatalf("fixture: an unreadable cache must read as empty, got %#v", readings)
	}
	commit(3)
	expect(3)

	writeMuseCacheFile(t, museCacheFileJSON(genPtr(4321, 1),
		museCacheReadingJSON(museDevAccountKey(), base.Add(3*time.Second), base.Add(time.Hour), base.Add(72*time.Hour))))
	commit(4)
	expect(4)
}

/* ------------------------------- The gather -------------------------------- */

// Muse Code's parser takes the gather's context (its receipt reservation) but
// reads only local files: it declares zero serial probes, so the gather holds
// nothing back for it, nor for it behind another provider's discovery.
func TestMuseCodeUsageParser_TheGatherReservesNothingForIt(t *testing.T) {
	parser := cliAgentUsageParserIndex()[museCodeUsageProvider]
	if parser == nil {
		t.Fatal("museCode missing from the parser index")
	}
	if _, bounded := parser.(cliAgentUsageContextParser); !bounded {
		t.Fatal("the Muse Code parser must take the gather's context, or no receipt can carry its generation")
	}
	counter, declares := parser.(cliAgentUsageSerialProbeCounter)
	if !declares || counter.SerialProbeCount() != 0 {
		t.Fatal("the Muse Code parser must declare zero serial probes")
	}
	if got := cliAgentUsageGatherReserve(parser); got != 0 {
		t.Fatalf("reserve = %v, want 0", got)
	}
	if got := cliAgentUsageGatherReservesAfter([]cliAgentUsageParser{codexUsageParser{}, parser}); got[0] != 0 {
		t.Fatalf("a probe ahead of Muse Code must leave it nothing, reserves = %v", got)
	}
}

/* ------------------------------ The propagator ------------------------------ */

// A Muse Code hint waits for its own cache's rotation and never holds Codex,
// Claude, OpenCode or Grok back.
func TestMuseCodeUsageHint_IsGatedOnItsOwnRotationAndBlocksNoOne(t *testing.T) {
	withCodexGenerationEpoch(t, 9310)
	rec, cfg := propagatorFixture(t)
	t.Setenv("AIEXPEDITE_OPENCODE_USAGE_CACHE", t.TempDir()+"/opencode_usage.json")
	prevOpenCode := openCodeGenerationRotated.Load()
	openCodeGenerationRotated.Store(true)
	t.Cleanup(func() { openCodeGenerationRotated.Store(prevOpenCode) })
	grokGenerationRotated.Store(true)
	museCodeGenerationRotated.Store(false)
	startCLIUsagePropagator(cfg)
	cliUsagePropagator.rotating.Wait()
	markRotated(t)

	noteCLIUsageObservationAdvanced(museCodeUsageProvider, gen(9310, 1))
	time.Sleep(cliUsageHintDebounce / 2)
	noteCLIUsageObservationAdvanced(codexUsageProvider, gen(9310, 2))
	noteCLIUsageObservationAdvanced(claudeUsageProvider, gen(9310, 3))
	noteCLIUsageObservationAdvanced(openCodeUsageProvider, gen(9310, 4))
	noteCLIUsageObservationAdvanced(grokUsageProvider, gen(9310, 5))
	hints := waitHints(t, rec, 4, cliUsageHintSpacing/2)
	for _, h := range hints {
		if h.hint.Provider == museCodeUsageProvider {
			t.Fatalf("sent a museCode hint before its rotation: %+v", h.hint)
		}
	}
	museCodeGenerationRotated.Store(true)
	if h := waitHintFor(t, rec, gen(9310, 1)).hint; h.Provider != museCodeUsageProvider {
		t.Fatalf("hint = %+v, want museCode once its cache rotated", h)
	}
}

// A signed refresh that carried the Muse Code generation leaves exactly one
// confirmation, after the spacing.
func TestMuseCodeUsageHint_AReceiptCarryingMuseLeavesOneConfirmation(t *testing.T) {
	_, rec, cfg := museHintFixture(t, 9311)
	museCodeGenerationRotated.Store(true)
	startCLIUsagePropagator(cfg)
	cliUsagePropagator.rotating.Wait()

	g := gen(9311, 1)
	ctx, reservation := withCLIUsageReceiptReservation(context.Background())
	observeCLIUsageGeneration(ctx, museCodeUsageProvider, &g)
	time.Sleep(3 * cliUsageHintDebounce)
	if n := len(rec.all()); n != 0 {
		t.Fatalf("a receipt-reserved museCode generation was hinted %d times before publication", n)
	}
	settleCLIUsageReceipt(reservation, true, []cliAgentUsage{{Provider: museCodeUsageProvider, UsageGeneration: &g}})
	hints := waitHints(t, rec, 1, 3*cliUsageHintSpacing)
	if len(hints) != 1 || hints[0].hint.Provider != museCodeUsageProvider || hints[0].hint.Generation != 1 {
		t.Fatalf("hints = %+v, want exactly one museCode confirmation", hints)
	}
}

// End to end: a run commits a reading, a signed refresh's gather reads it back
// before the hint fires and carries it in the receipt, and the published
// receipt leaves that generation ONE confirmation instead of a hint plus a
// follow-up.
func TestMuseCodeUsageHint_AReceiptThatCarriedTheCommitLeavesOneConfirmation(t *testing.T) {
	home, rec, cfg := museHintFixture(t, 9312)
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	cliUsageHintDebounce = time.Hour // the commit's own hint cannot fire before the receipt
	startCLIUsagePropagator(cfg)
	cliUsagePropagator.rotating.Wait()

	now := time.Now()
	if !newMuseCodeRunUsageCapture().Line(museRunRecordNow(3, 40, now), now) {
		t.Fatal("the run's reading must be kept")
	}
	ctx, reservation := withCLIUsageReceiptReservation(context.Background())
	usage, _ := museCodeUsageParser{}.ParseContext(ctx, home, detectedCLIAgent{Detected: true}, now)
	if usage.UsageGeneration == nil || *usage.UsageGeneration != gen(9312, 1) {
		t.Fatalf("the receipt's generation = %+v, want the committed {9312,1}", usage.UsageGeneration)
	}
	settledAt := time.Now()
	settleCLIUsageReceipt(reservation, true, []cliAgentUsage{*usage})
	hints := waitHints(t, rec, 1, 3*cliUsageHintSpacing)
	if len(hints) != 1 || hints[0].hint.Provider != museCodeUsageProvider || hints[0].hint.Generation != 1 {
		t.Fatalf("hints = %+v, want exactly one museCode confirmation", hints)
	}
	if gap := time.UnixMilli(hints[0].hint.Timestamp).Sub(settledAt); gap < cliUsageHintSpacing-10*time.Millisecond {
		t.Fatalf("the confirmation came %v after the receipt, want no sooner than the spacing %v", gap, cliUsageHintSpacing)
	}
}

/* ----------------------------- Startup recovery ----------------------------- */

// The previous process committed a reading just before an update hand-off and
// its hint died inside the debounce: startup moves the cache onto this epoch in
// one write and hints it once, and the parser publishes the rotated generation.
func TestMuseCodeUsageHint_StartupRecoveryHintsAReadingLeftByThePreviousProcess(t *testing.T) {
	home, rec, cfg := museHintFixture(t, 9313)
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	now := time.Now()
	writeMuseCacheFile(t, museCacheFileJSON(genPtr(4242, 6),
		museCacheReadingJSON(museDevAccountKey(), now.Add(-time.Minute), now.Add(time.Hour), now.Add(72*time.Hour))))

	startCLIUsagePropagator(cfg)
	cliUsagePropagator.rotating.Wait()
	if g := museCacheGeneration(); g == nil || *g != gen(9313, 1) {
		t.Fatalf("cache generation = %+v, want it rotated to {9313,1}", g)
	}
	hints := waitHints(t, rec, 1, cliUsageHintSpacing/2)
	if len(hints) != 1 || hints[0].hint.Provider != museCodeUsageProvider || hints[0].hint.GenerationEpoch != 9313 || hints[0].hint.Generation != 1 {
		t.Fatalf("hints = %+v, want one museCode hint for {9313,1}", hints)
	}
	usage, _ := museCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, time.Now())
	if usage.UsageGeneration == nil || *usage.UsageGeneration != gen(9313, 1) {
		t.Fatalf("published %+v, want the rotated {9313,1}", usage.UsageGeneration)
	}
	if got, ok := loadMuseCodeUsageLive(museDevAccountKey()); !ok || got.Weekly.UsedPercent != 40 {
		t.Fatalf("the rotation changed the reading: %#v ok=%t", got, ok)
	}
}

// Recovery writes nothing and hints nothing for a cache that would render no
// number for the account signed in now, and nothing for this process's own
// commit, which it hinted when it made it.
func TestMuseCodeUsageHint_StartupRecoverySkipsWhatRendersNoNumber(t *testing.T) {
	now := time.Now()
	for name, setup := range map[string]func(t *testing.T){
		"both windows already reset": func(t *testing.T) {
			writeMuseCacheFile(t, museCacheFileJSON(genPtr(4242, 6),
				museCacheReadingJSON(museDevAccountKey(), now.Add(-3*time.Hour), now.Add(-time.Hour), now.Add(-time.Minute))))
		},
		"only another account's reading": func(t *testing.T) {
			writeMuseCacheFile(t, museCacheFileJSON(genPtr(4242, 6),
				museCacheReadingJSON(museOtherAccountKey(), now.Add(-time.Minute), now.Add(time.Hour), now.Add(72*time.Hour))))
		},
		"no cache": func(*testing.T) {},
	} {
		t.Run(name, func(t *testing.T) {
			home, _, _ := museHintFixture(t, 9314)
			writeMuseConfig(t, home, "auth.json", museAuthFixture)
			setup(t)
			before, _ := os.ReadFile(museCodeUsageLiveCachePath())
			if g := museCodeRecoveryGeneration(now); g != nil {
				t.Fatalf("recovered %+v", g)
			}
			after, _ := os.ReadFile(museCodeUsageLiveCachePath())
			if string(before) != string(after) {
				t.Fatalf("recovery rewrote the cache:\n%s\n→\n%s", before, after)
			}
			if museCodeGenerationRotated.Load() {
				t.Fatal("recovery rotated a cache it had nothing to recover from")
			}
		})
	}

	t.Run("this process's own commit", func(t *testing.T) {
		home, _, _ := museHintFixture(t, 9315)
		writeMuseConfig(t, home, "auth.json", museAuthFixture)
		if !newMuseCodeRunUsageCapture().Line(museRunRecordNow(3, 40, now), now) {
			t.Fatal("the run's reading must be kept")
		}
		if g := museCodeRecoveryGeneration(now); g != nil {
			t.Fatalf("recovered %+v, a generation this process already hinted", g)
		}
		if g := museCacheGeneration(); g == nil || *g != gen(9315, 1) {
			t.Fatalf("cache generation = %+v, want it unmoved at {9315,1}", g)
		}
	})
}
