// cliagent_usage_grok_billing_race_test.go
// -----------------------------------------------------------------------------
// The supersession guard reads a file an EXTERNAL process also appends to. A
// same-account DIRECT Grok child never takes grokBillingAttributionSerialize, so
// holding that mutex serializes our own helpers and nothing else: a record the
// child writes between the guard's scan and our O_APPEND is invisible to the
// scan, and readGrokBillingSnapshot publishes the last billing line by FILE
// ORDER — so our older receipt becomes the current reading.
//
// These tests drive that window deterministically through
// grokBillingPreWriteBarrier rather than by racing a goroutine.
// -----------------------------------------------------------------------------

package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// helperGrokRaceBarrier makes fn run exactly once inside the merge's scan/write
// window, then restores the production no-op.
func helperGrokRaceBarrier(t *testing.T, fn func()) {
	t.Helper()
	previous := grokBillingPreWriteBarrier
	fired := false
	grokBillingPreWriteBarrier = func() {
		if fired {
			return
		}
		fired = true
		fn()
	}
	t.Cleanup(func() { grokBillingPreWriteBarrier = previous })
}

// The regression: a direct child appends a NEWER record after the supersession
// scan has taken its snapshot. Our older receipt lands last, so without the
// post-write repair the card would publish the stale percentage.
func TestPersistGrokManagedBillingSnapshot_RestoresARecordDisplacedByADirectRace(t *testing.T) {
	resetGrokBillingAttribution(t)
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	isolated := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", persistent)

	if err := appendGrokBillingIdentity(persistent); err != nil {
		t.Fatalf("seed persistent identity: %v", err)
	}

	// The managed child fetched two hours ago and only exits now.
	if err := seedGrokManagedBillingIdentity(isolated); err != nil {
		t.Fatalf("seed isolated identity: %v", err)
	}
	helperAppendGrokLogLine(t, isolated,
		grokBillingLine(time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339), 12,
			"USAGE_PERIOD_TYPE_WEEKLY", "2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	// The direct child fetched a moment ago and writes while we are mid-merge.
	raced := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	helperGrokRaceBarrier(t, func() {
		helperAppendGrokLogLine(t, persistent,
			grokBillingLine(raced, 77, "USAGE_PERIOD_TYPE_WEEKLY",
				"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))
	})

	outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if outcome != grokManagedBillingRaced {
		t.Fatalf("outcome = %s, want %s — a displaced newer record must be reported, "+
			"not read as a clean merge", outcome, grokManagedBillingRaced)
	}

	published, ok := readGrokBillingSnapshot(persistent, grokIdentityCandidates(persistent))
	if !ok {
		t.Fatalf("expected a published snapshot after the repair")
	}
	if published.ObservedAt.UTC().Format(time.RFC3339) != raced {
		t.Fatalf("published observation = %s, want %s — the merge left its older "+
			"receipt as the last billing line",
			published.ObservedAt.UTC().Format(time.RFC3339), raced)
	}
	if published.UsedPercent != 77 {
		t.Fatalf("published percent = %v, want 77 — the stale managed receipt is "+
			"being shown as the current reading", published.UsedPercent)
	}
}

// A racer that lands AFTER our write is already the last billing line, so the
// repair must not fire: an unnecessary re-append would write a duplicate record
// into a provider-owned log on every merge that overlapped a direct run.
func TestPersistGrokManagedBillingSnapshot_DoesNotRewriteAnUndisplacedRace(t *testing.T) {
	resetGrokBillingAttribution(t)
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	isolated := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", persistent)

	if err := appendGrokBillingIdentity(persistent); err != nil {
		t.Fatalf("seed persistent identity: %v", err)
	}
	if err := seedGrokManagedBillingIdentity(isolated); err != nil {
		t.Fatalf("seed isolated identity: %v", err)
	}
	merged := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	helperAppendGrokLogLine(t, isolated,
		grokBillingLine(merged, 12, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if outcome != grokManagedBillingPersisted {
		t.Fatalf("outcome = %s, want %s", outcome, grokManagedBillingPersisted)
	}

	// The direct child's record arrives now, on top of ours.
	newer := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	helperAppendGrokLogLine(t, persistent,
		grokBillingLine(newer, 77, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))
	before, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read persistent log: %v", err)
	}

	repaired, err := restoreGrokBillingRecordDisplacedByRace(
		persistent, "acct-1", grokIdentityCandidates(persistent))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if repaired {
		t.Fatalf("repaired a race that displaced nothing — the newest record was " +
			"already the last billing line")
	}
	after, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read persistent log: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("the repair wrote into a provider-owned log with nothing to fix:\n"+
			"before=%s\nafter=%s", before, after)
	}
}

// The repair is a same-account correction, never a cross-account one. When the
// newest line belongs to somebody else the publish reader declines by design;
// re-appending OUR record on top of theirs would republish our usage over
// theirs, which is worse than the staleness this repairs.
func TestRestoreGrokBillingRecordDisplacedByRace_LeavesAForeignNewestRecordAlone(t *testing.T) {
	resetGrokBillingAttribution(t)
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", persistent)

	if err := appendGrokBillingIdentity(persistent); err != nil {
		t.Fatalf("seed persistent identity: %v", err)
	}
	helperAppendGrokLogLine(t, persistent,
		grokBillingLine(time.Now().Add(-1*time.Hour).UTC().Format(time.RFC3339), 52,
			"USAGE_PERIOD_TYPE_WEEKLY", "2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))
	// Another account logs in and fetches its own credits.
	helperAppendGrokLogLine(t, persistent,
		`{"ts":"2026-08-19T12:00:00Z","msg":"session start","ctx":{"user_id":"acct-2"}}`)
	helperAppendGrokLogLine(t, persistent,
		grokBillingLine(time.Now().Add(-1*time.Minute).UTC().Format(time.RFC3339), 5,
			"USAGE_PERIOD_TYPE_WEEKLY", "2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))
	before, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read persistent log: %v", err)
	}

	repaired, err := restoreGrokBillingRecordDisplacedByRace(
		persistent, "acct-1", []string{"acct-1"})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if repaired {
		t.Fatalf("repaired across an account boundary — another account's newest " +
			"record must not be overwritten by ours")
	}
	after, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read persistent log: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("the repair wrote over a foreign record:\nbefore=%s\nafter=%s", before, after)
	}
}

// The restored record carries only the normalized allowlisted fields, exactly
// like the merge it repairs: the repair must not become a second, looser path
// for a provider log's raw contents to reach the persistent home.
func TestRestoreGrokBillingRecordDisplacedByRace_WritesOnlyNormalizedFields(t *testing.T) {
	resetGrokBillingAttribution(t)
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	isolated := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", persistent)

	if err := appendGrokBillingIdentity(persistent); err != nil {
		t.Fatalf("seed persistent identity: %v", err)
	}
	if err := seedGrokManagedBillingIdentity(isolated); err != nil {
		t.Fatalf("seed isolated identity: %v", err)
	}
	helperAppendGrokLogLine(t, isolated,
		grokBillingLine(time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339), 12,
			"USAGE_PERIOD_TYPE_WEEKLY", "2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	// The racing direct record carries a secret in a field we never allowlist.
	helperGrokRaceBarrier(t, func() {
		helperAppendGrokLogLine(t, persistent,
			`{"ts":"`+time.Now().Add(-1*time.Minute).UTC().Format(time.RFC3339)+`",`+
				`"msg":"`+grokBillingLogMessage+`","ctx":{"config":{"creditUsagePercent":77,`+
				`"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-08-17T22:28:32Z",`+
				`"end":"2126-08-24T22:28:32Z"},"apiKey":"xai-super-secret",`+
				`"prompt":"do not copy me"},"subscriptionTier":"SuperGrok"}}`)
	})

	if _, err := persistGrokManagedBillingSnapshot(isolated, persistent); err != nil {
		t.Fatalf("persist: %v", err)
	}

	raw, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read persistent log: %v", err)
	}
	// The racing line itself is the CLI's own and stays; only the record WE
	// re-appended is under test, so count occurrences rather than presence.
	if strings.Count(string(raw), "xai-super-secret") != 1 {
		t.Fatalf("the repair copied a non-allowlisted credential field:\n%s", raw)
	}
	if strings.Count(string(raw), "do not copy me") != 1 {
		t.Fatalf("the repair copied non-metric log content:\n%s", raw)
	}
}

// A managed account-A session can exit while a live DIRECT account-B run has
// already written a NEWER billing record and writes no further one. Appending
// only B's identity after A's identity/record pair leaves A's older record as
// the log's last billing line, and a trailing marker cannot authenticate a
// record that PRECEDES it — so B's gather stops at A's record, refuses it as
// foreign, and reports nothing at all. The merge must carry B's own record
// forward with its marker.
func TestPersistGrokManagedBillingSnapshot_PreservesAnArmedDirectAccountsNewerRecord(t *testing.T) {
	resetGrokBillingAttribution(t)
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	isolated := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", persistent)

	// A live direct run on account B, which already fetched credits.
	finish := startGrokBillingAttributionKeeper("acct-2")
	defer finish()
	helperAppendGrokLogLine(t, persistent,
		`{"ts":"2026-08-19T12:00:00Z","msg":"session start","ctx":{"user_id":"acct-2"}}`)
	direct := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	helperAppendGrokLogLine(t, persistent,
		grokBillingLine(direct, 77, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	// The managed account-A session exits carrying an OLDER receipt.
	if err := seedGrokManagedBillingIdentity(isolated); err != nil {
		t.Fatalf("seed isolated identity: %v", err)
	}
	helperAppendGrokLogLine(t, isolated,
		grokBillingLine(time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339), 12,
			"USAGE_PERIOD_TYPE_WEEKLY", "2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	if _, err := persistGrokManagedBillingSnapshot(isolated, persistent); err != nil {
		t.Fatalf("persist: %v", err)
	}

	published, ok := readGrokBillingSnapshot(persistent, []string{"acct-2"})
	if !ok {
		t.Fatal("the armed direct account can no longer read its own fresher record: " +
			"the merge displaced it behind the managed account's older one")
	}
	if published.UsedPercent != 77 {
		t.Fatalf("published percent = %v, want 77 — the direct account is reading "+
			"something other than its own newest record", published.UsedPercent)
	}
	if published.ObservedAt.UTC().Format(time.RFC3339) != direct {
		t.Fatalf("published observation = %s, want %s",
			published.ObservedAt.UTC().Format(time.RFC3339), direct)
	}
}

// The direct account's record is only carried forward when it is NEWER than the
// one being merged. An OLDER one must stay where it is: the reader publishes by
// FILE ORDER, so moving it last would republish a stale reading — and it would
// also write a duplicate record into a provider-owned log on every merge that
// overlapped a direct run.
func TestPersistGrokManagedBillingSnapshot_DoesNotPromoteAnOlderDirectRecord(t *testing.T) {
	resetGrokBillingAttribution(t)
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	isolated := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", persistent)

	finish := startGrokBillingAttributionKeeper("acct-2")
	defer finish()
	helperAppendGrokLogLine(t, persistent,
		`{"ts":"2026-08-19T12:00:00Z","msg":"session start","ctx":{"user_id":"acct-2"}}`)
	helperAppendGrokLogLine(t, persistent,
		grokBillingLine(time.Now().Add(-3*time.Hour).UTC().Format(time.RFC3339), 77,
			"USAGE_PERIOD_TYPE_WEEKLY", "2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	if err := seedGrokManagedBillingIdentity(isolated); err != nil {
		t.Fatalf("seed isolated identity: %v", err)
	}
	merged := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	helperAppendGrokLogLine(t, isolated,
		grokBillingLine(merged, 12, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	if _, err := persistGrokManagedBillingSnapshot(isolated, persistent); err != nil {
		t.Fatalf("persist: %v", err)
	}

	published, ok := readGrokBillingSnapshot(persistent, grokIdentityCandidates(persistent))
	if !ok {
		t.Fatal("the managed account lost its own freshly merged record")
	}
	if published.ObservedAt.UTC().Format(time.RFC3339) != merged {
		t.Fatalf("published observation = %s, want the freshly merged %s — an older "+
			"direct record was promoted over it",
			published.ObservedAt.UTC().Format(time.RFC3339), merged)
	}
	raw, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read persistent log: %v", err)
	}
	if got := strings.Count(string(raw), `"creditUsagePercent":77`); got != 1 {
		t.Fatalf("the direct account's older record appears %d times — the merge "+
			"duplicated it into a provider-owned log", got)
	}
}

// The regression codex found after the preservation fix landed: the scan that
// decides what to carry forward for an armed DIRECT account is taken BEFORE the
// write, against a child that never takes our mutex. When account B lands its
// newest record inside that window, the merge carries only B's trailing marker —
// which cannot authenticate a record ABOVE it — and account A's older record
// ends up last. B's gather then stops at A's record, refuses it as foreign, and
// publishes nothing, while restoreGrokBillingRecordDisplacedByRace cannot see
// any of it because it scans only A's identities.
func TestPersistGrokManagedBillingSnapshot_RestoresAnArmedDirectRecordRacedAfterThePreserveScan(t *testing.T) {
	resetGrokBillingAttribution(t)
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	isolated := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", persistent)

	// A live direct run on account B that has NOT fetched credits yet, so the
	// pre-write preservation scan finds nothing to carry forward.
	finish := startGrokBillingAttributionKeeper("acct-2")
	defer finish()
	helperAppendGrokLogLine(t, persistent,
		`{"ts":"2026-08-19T12:00:00Z","msg":"session start","ctx":{"user_id":"acct-2"}}`)

	// The managed account-A session exits carrying an older receipt.
	if err := seedGrokManagedBillingIdentity(isolated); err != nil {
		t.Fatalf("seed isolated identity: %v", err)
	}
	helperAppendGrokLogLine(t, isolated,
		grokBillingLine(time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339), 12,
			"USAGE_PERIOD_TYPE_WEEKLY", "2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	// B fetches inside the scan/write window.
	raced := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	helperGrokRaceBarrier(t, func() {
		helperAppendGrokLogLine(t, persistent,
			grokBillingLine(raced, 77, "USAGE_PERIOD_TYPE_WEEKLY",
				"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))
	})

	outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if outcome != grokManagedBillingRaced {
		t.Fatalf("outcome = %s, want %s — a displaced direct record must be reported, "+
			"not read as a clean merge", outcome, grokManagedBillingRaced)
	}

	published, ok := readGrokBillingSnapshot(persistent, []string{"acct-2"})
	if !ok {
		t.Fatal("the armed direct account cannot read the record it wrote mid-merge: " +
			"the managed pair landed on top of it and only a trailing marker followed")
	}
	if published.UsedPercent != 77 {
		t.Fatalf("published percent = %v, want 77", published.UsedPercent)
	}
	if published.ObservedAt.UTC().Format(time.RFC3339) != raced {
		t.Fatalf("published observation = %s, want the raced %s",
			published.ObservedAt.UTC().Format(time.RFC3339), raced)
	}
}

// The post-write direct repair must stay a READ in the ordinary case. A merge
// that overlaps a direct run whose record is already readable — the preserved
// pair the pre-write scan carried forward — must not append that record a second
// time into a provider-owned log on every managed exit.
func TestPersistGrokManagedBillingSnapshot_DirectRepairWritesNothingWhenNothingWasDisplaced(t *testing.T) {
	resetGrokBillingAttribution(t)
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	isolated := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", persistent)

	finish := startGrokBillingAttributionKeeper("acct-2")
	defer finish()
	helperAppendGrokLogLine(t, persistent,
		`{"ts":"2026-08-19T12:00:00Z","msg":"session start","ctx":{"user_id":"acct-2"}}`)
	helperAppendGrokLogLine(t, persistent,
		grokBillingLine(time.Now().Add(-1*time.Minute).UTC().Format(time.RFC3339), 77,
			"USAGE_PERIOD_TYPE_WEEKLY", "2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	if err := seedGrokManagedBillingIdentity(isolated); err != nil {
		t.Fatalf("seed isolated identity: %v", err)
	}
	helperAppendGrokLogLine(t, isolated,
		grokBillingLine(time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339), 12,
			"USAGE_PERIOD_TYPE_WEEKLY", "2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	if _, err := persistGrokManagedBillingSnapshot(isolated, persistent); err != nil {
		t.Fatalf("persist: %v", err)
	}

	raw, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read persistent log: %v", err)
	}
	// Once as the child's own line, once as the pair the merge carried forward.
	if got := strings.Count(string(raw), `"creditUsagePercent":77`); got != 2 {
		t.Fatalf("the direct account's record appears %d times, want 2 — the "+
			"post-write repair re-appended a record that was never displaced", got)
	}
}

// The timestamp tiebreak is only sound while both accounts still have a reader
// here. When the persistent home's credentials have moved to the armed DIRECT
// account, leaving the managed account's newer record last serves nobody: every
// gather on this device reads under the direct account, stops at that foreign
// record and publishes nothing, while the keeper sees a direct marker as newest
// and never repairs it — the live account goes dark with a usable record of its
// own already in the file.
func TestPersistGrokManagedBillingSnapshot_KeepsTheLiveDirectAccountReadable(t *testing.T) {
	resetGrokBillingAttribution(t)
	// The persistent login is account B — the account the direct run is using.
	persistent := helperGrokHomeWithAccount(t, "acct-2")
	isolated := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", persistent)

	finish := startGrokBillingAttributionKeeper("acct-2")
	defer finish()
	helperAppendGrokLogLine(t, persistent,
		`{"ts":"2026-08-19T12:00:00Z","msg":"session start","ctx":{"user_id":"acct-2"}}`)
	direct := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	helperAppendGrokLogLine(t, persistent,
		grokBillingLine(direct, 77, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	// Managed account A exits with a NEWER receipt than B's.
	if err := seedGrokManagedBillingIdentity(isolated); err != nil {
		t.Fatalf("seed isolated identity: %v", err)
	}
	helperAppendGrokLogLine(t, isolated,
		grokBillingLine(time.Now().Add(-1*time.Minute).UTC().Format(time.RFC3339), 12,
			"USAGE_PERIOD_TYPE_WEEKLY", "2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	if _, err := persistGrokManagedBillingSnapshot(isolated, persistent); err != nil {
		t.Fatalf("persist: %v", err)
	}

	published, ok := readGrokBillingSnapshot(persistent, grokIdentityCandidates(persistent))
	if !ok {
		t.Fatal("the live direct account can no longer read anything: the merge left " +
			"a foreign record last and nothing on this device will ever publish it")
	}
	if published.UsedPercent != 77 {
		t.Fatalf("published percent = %v, want 77 — the live account's own record",
			published.UsedPercent)
	}

	// Account A keeps its merged record in the file, so a device that logs back
	// into it later still reads it — it is preceded, not removed.
	if _, ok := newestTrustedGrokBillingRecordFor(
		persistent, []string{"acct-1"}, time.Now().Add(grokBillingMaxClockSkew)); !ok {
		t.Fatal("the merged managed record was lost, not merely preceded")
	}
}

// helperGrokRaceBarrierSequence makes fns[i] run inside the i-th scan/write
// window of a single merge, then no-ops. It exists because one merge now has
// more than one such window — the payload write and each pass of the direct
// repair — and the interesting regression is a racer that lands in the SECOND
// one, after the repair chose what to preserve.
func helperGrokRaceBarrierSequence(t *testing.T, fns ...func()) {
	t.Helper()
	previous := grokBillingPreWriteBarrier
	call := 0
	grokBillingPreWriteBarrier = func() {
		if call < len(fns) {
			fn := fns[call]
			call++
			fn()
			return
		}
		call++
	}
	t.Cleanup(func() { grokBillingPreWriteBarrier = previous })
}

// The regression codex found after the direct repair landed: that repair picks
// what to re-append from a scan taken BEFORE its own write, against the same
// child that never takes our mutex. When account B lands an even newer record
// inside THAT window, the repair appends the older scanned pair last, the
// reader publishes it by file order, and nothing corrects it — the keeper sees a
// marker already naming B, and the managed repair scans only A's identities.
func TestPersistGrokManagedBillingSnapshot_DirectRepairRetestsAfterItsOwnWrite(t *testing.T) {
	resetGrokBillingAttribution(t)
	// The persistent login is account B, so B's own record is what any gather
	// on this device would publish.
	persistent := helperGrokHomeWithAccount(t, "acct-2")
	isolated := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", persistent)

	finish := startGrokBillingAttributionKeeper("acct-2")
	defer finish()
	helperAppendGrokLogLine(t, persistent,
		`{"ts":"2026-08-19T12:00:00Z","msg":"session start","ctx":{"user_id":"acct-2"}}`)
	helperAppendGrokLogLine(t, persistent,
		grokBillingLine(time.Now().Add(-3*time.Hour).UTC().Format(time.RFC3339), 55,
			"USAGE_PERIOD_TYPE_WEEKLY", "2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	// Managed account A exits carrying its own receipt.
	if err := seedGrokManagedBillingIdentity(isolated); err != nil {
		t.Fatalf("seed isolated identity: %v", err)
	}
	helperAppendGrokLogLine(t, isolated,
		grokBillingLine(time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339), 12,
			"USAGE_PERIOD_TYPE_WEEKLY", "2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	// B fetches twice: once inside the payload write's window (the already
	// covered race) and once inside the direct repair's own window.
	displaced := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	newest := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	helperGrokRaceBarrierSequence(t,
		func() {
			helperAppendGrokLogLine(t, persistent,
				grokBillingLine(displaced, 66, "USAGE_PERIOD_TYPE_WEEKLY",
					"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))
		},
		func() {
			helperAppendGrokLogLine(t, persistent,
				grokBillingLine(newest, 88, "USAGE_PERIOD_TYPE_WEEKLY",
					"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))
		},
	)

	outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if outcome != grokManagedBillingRaced {
		t.Fatalf("outcome = %s, want %s", outcome, grokManagedBillingRaced)
	}

	published, ok := readGrokBillingSnapshot(persistent, grokIdentityCandidates(persistent))
	if !ok {
		t.Fatal("the live direct account can no longer read any record of its own")
	}
	if published.ObservedAt.UTC().Format(time.RFC3339) != newest {
		t.Fatalf("published observation = %s, want the newest %s — the direct repair "+
			"left the pair it scanned before its own write as the last line",
			published.ObservedAt.UTC().Format(time.RFC3339), newest)
	}
	if published.UsedPercent != 88 {
		t.Fatalf("published percent = %v, want 88", published.UsedPercent)
	}
}

// A managed-only merge leaves our identity as the log's newest marker, and a
// trailing marker vouches for whatever is written next. Without a seal, a Grok
// invocation OUTSIDE the agent — an API-key override under our still-cached
// login — has its identity-less record accepted as this account's usage, so one
// subscription's utilization publishes as another's.
func TestPersistGrokManagedBillingSnapshot_SealsTheMarkerAfterAManagedOnlyMerge(t *testing.T) {
	resetGrokBillingAttribution(t)
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	isolated := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", persistent)

	if err := appendGrokBillingIdentity(persistent); err != nil {
		t.Fatalf("seed persistent identity: %v", err)
	}
	if err := seedGrokManagedBillingIdentity(isolated); err != nil {
		t.Fatalf("seed isolated identity: %v", err)
	}
	merged := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	helperAppendGrokLogLine(t, isolated,
		grokBillingLine(merged, 12, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if outcome != grokManagedBillingPersisted {
		t.Fatalf("outcome = %s, want %s", outcome, grokManagedBillingPersisted)
	}

	// The merged record keeps its own attribution: it binds to the identity
	// ABOVE it, so the seal cannot unbind it.
	published, ok := readGrokBillingSnapshot(persistent, grokIdentityCandidates(persistent))
	if !ok {
		t.Fatal("the seal cost the merge its own record — a marker appended AFTER a " +
			"record must not unbind it")
	}
	if published.ObservedAt.UTC().Format(time.RFC3339) != merged {
		t.Fatalf("published observation = %s, want the merged %s",
			published.ObservedAt.UTC().Format(time.RFC3339), merged)
	}
	if !grokBillingIdentityIsNewest(persistent, grokContestedBillingIdentity) {
		raw, _ := os.ReadFile(grokBillingLogPath(persistent))
		t.Fatalf("the managed marker is still the newest one, so it keeps vouching "+
			"for records this agent never produced:\n%s", raw)
	}

	// A later out-of-agent CLI on another account writes a record with no
	// identity of its own. It must NOT be read as ours.
	helperAppendGrokLogLine(t, persistent,
		grokBillingLine(time.Now().Add(-1*time.Minute).UTC().Format(time.RFC3339), 99,
			"USAGE_PERIOD_TYPE_WEEKLY", "2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))
	if snap, ok := readGrokBillingSnapshot(persistent, grokIdentityCandidates(persistent)); ok {
		t.Fatalf("a foreign identity-less record was accepted as this account's usage "+
			"(percent %v) — the managed marker vouched for it", snap.UsedPercent)
	}
}
