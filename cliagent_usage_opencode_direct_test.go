package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_opencode_direct_test.go — the direct-run reader: OpenCode
   runs the agent did not spawn reach the card as fresh numbers, are never
   counted twice against managed runs or the export fallback, and stay within
   their bounds. Every case reads a temporary store under a pinned clock.
   ------------------------------------------------------------------------ */

// openCodeDirectTestNoon is where each case's clock starts: noon UTC on a fixed
// day keeps every case clear of midnight. The clock is what both the reader and
// the run lifecycle read.
var openCodeDirectTestNoon = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

type openCodeDirectTestClock struct{ now time.Time }

func (c *openCodeDirectTestClock) ms(d time.Duration) int64 { return c.now.Add(d).UnixMilli() }

// openCodeDirectFixture isolates the ledger and the store, pins the clock and
// enables the reader. It returns the ladder's scheduler, the store and the clock.
func openCodeDirectFixture(t *testing.T, epoch int64) (*openCodeUsageTestScheduler, *openCodeTestStore, *openCodeDirectTestClock) {
	t.Helper()
	sched := openCodeUsageFixture(t, epoch)
	root := t.TempDir()
	t.Setenv("OPENCODE_DATA", root)
	clock := &openCodeDirectTestClock{now: openCodeDirectTestNoon}
	prevNow, prevFreshNow := openCodeDirectNow, openCodeUsageFreshnessNow
	prevEnabled := openCodeDirectScanEnabled
	prevCaps := []int{openCodeDirectMaxSessions, openCodeDirectMaxRecords}
	prevBytes, prevBudget := openCodeDirectMaxRecordBytes, openCodeDirectTickBudget
	openCodeDirectNow = func() time.Time { return clock.now }
	openCodeUsageFreshnessNow = func() time.Time { return clock.now }
	openCodeDirectScanEnabled = true
	resetOpenCodeDirectScan()
	t.Cleanup(func() {
		openCodeDirectNow, openCodeUsageFreshnessNow = prevNow, prevFreshNow
		openCodeDirectScanEnabled = prevEnabled
		openCodeDirectMaxSessions, openCodeDirectMaxRecords = prevCaps[0], prevCaps[1]
		openCodeDirectMaxRecordBytes, openCodeDirectTickBudget = prevBytes, prevBudget
		resetOpenCodeDirectScan()
	})
	store := &openCodeTestStore{t: t, root: root}
	if err := os.MkdirAll(filepath.Join(root, "storage", "message"), 0o755); err != nil {
		t.Fatal(err)
	}
	return sched, store, clock
}

// openCodeTestStore writes OpenCode's JSON-file layout.
type openCodeTestStore struct {
	t    *testing.T
	root string
}

// openCodeTestMessage is one stored assistant message and its step parts.
type openCodeTestMessage struct {
	session, id string
	role        string // "" = assistant
	createdMs   int64
	completedMs int64 // 0 = still running
	writtenMs   int64 // 0 = completedMs, else createdMs
	steps       [][3]int64
	cost        float64
	extra       string // raw JSON members appended to the message (redaction)
}

func (s *openCodeTestStore) write(m openCodeTestMessage) {
	s.t.Helper()
	role := firstNonEmpty(m.role, "assistant")
	written := m.writtenMs
	if written == 0 {
		written = max(m.completedMs, m.createdMs)
	}
	var in, out, reasoning int64
	for _, st := range m.steps {
		in, out, reasoning = in+st[0], out+st[1], reasoning+st[2]
	}
	timeJSON := fmt.Sprintf(`{"created":%d}`, m.createdMs)
	if m.completedMs > 0 {
		timeJSON = fmt.Sprintf(`{"created":%d,"completed":%d}`, m.createdMs, m.completedMs)
	}
	last := [3]int64{}
	if len(m.steps) > 0 {
		last = m.steps[len(m.steps)-1]
	}
	// info.tokens is the LAST step's, as OpenCode leaves it; cost is cumulative.
	body := fmt.Sprintf(`{"id":%q,"sessionID":%q,"role":%q,"time":%s,"cost":%g,"tokens":{"input":%d,"output":%d,"reasoning":%d,"cache":{"read":0,"write":0}}%s}`,
		m.id, m.session, role, timeJSON, m.cost, last[0], last[1], last[2], m.extra)
	dir := filepath.Join(s.root, "storage", "message", m.session)
	s.writeFile(filepath.Join(dir, m.id+".json"), body, written)
	for i, st := range m.steps {
		s.writeFile(filepath.Join(s.root, "storage", "part", m.id, fmt.Sprintf("prt_%s_%d.json", m.id, i)),
			fmt.Sprintf(`{"id":"prt_%s_%d","messageID":%q,"sessionID":%q,"type":"step-finish","reason":"stop","cost":%g,"tokens":{"input":%d,"output":%d,"reasoning":%d,"cache":{"read":0,"write":0}}}`,
				m.id, i, m.id, m.session, m.cost/float64(len(m.steps)), st[0], st[1], st[2]), written)
	}
	s.touchDir(dir, written)
}

func (s *openCodeTestStore) writeFile(path, body string, atMs int64) {
	s.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		s.t.Fatal(err)
	}
	at := time.UnixMilli(atMs)
	if err := os.Chtimes(path, at, at); err != nil {
		s.t.Fatal(err)
	}
}

// touchDir moves a session directory's mtime forward to atMs, as creating a
// record in it would.
func (s *openCodeTestStore) touchDir(dir string, atMs int64) {
	s.t.Helper()
	if info, err := os.Stat(dir); err == nil && info.ModTime().UnixMilli() > atMs {
		atMs = info.ModTime().UnixMilli()
	}
	at := time.UnixMilli(atMs)
	if err := os.Chtimes(dir, at, at); err != nil {
		s.t.Fatal(err)
	}
}

// countOpenCodeDirectReads counts the adapter reads of one layout.
func countOpenCodeDirectReads(t *testing.T, layout string) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	prev := openCodeDirectReaders[layout]
	openCodeDirectReaders[layout] = func(ctx context.Context, root string, floorMs int64, limits openCodeDirectLimits) (openCodeDirectRead, error) {
		n.Add(1)
		return prev(ctx, root, floorMs, limits)
	}
	t.Cleanup(func() { openCodeDirectReaders[layout] = prev })
	return &n
}

// scanOpenCodeDirect runs one scan as the tick would, ungated.
func scanOpenCodeDirect(t *testing.T) string {
	t.Helper()
	return openCodeDirectScanShared(openCodeDirectClickBudget)
}

func openCodeDirectBucket(t *testing.T, fingerprint string, at time.Time) openCodeUsageBucket {
	t.Helper()
	b, _, _ := openCodeUsageBucketForDay(fingerprint, at)
	return b
}

/* --------------------------------------------------------------------------
   Acceptance
   -------------------------------------------------------------------------- */

// A direct run's completed messages give fresh numeric "Tokens today" and
// "Cost today" on the next gather, with a generation to publish.
func TestOpenCodeDirect_ADirectRunBecomesTokensTodayOnTheNextGather(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2001)
	stubOpenCodeReadiness(t, "anthropic/claude-sonnet-4-5\n", true)
	store.write(openCodeTestMessage{session: "ses_shell", id: "msg_a", createdMs: clock.ms(-time.Hour), completedMs: clock.ms(-59 * time.Minute),
		steps: [][3]int64{{1000, 20, 5}, {1200, 30, 0}}, cost: 0.04})
	store.write(openCodeTestMessage{session: "ses_ide", id: "msg_b", createdMs: clock.ms(-30 * time.Minute), completedMs: clock.ms(-29 * time.Minute),
		steps: [][3]int64{{300, 10, 0}}, cost: 0.01})
	store.write(openCodeTestMessage{session: "ses_shell", id: "msg_user", role: "user", createdMs: clock.ms(-61 * time.Minute)})

	if label := scanOpenCodeDirect(t); label != "direct_scanned" {
		t.Fatalf("scan = %q", label)
	}
	usage, _ := openCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{Path: "opencode"}, clock.now)
	tokens := openCodeMetricByLabel(t, usage.Metrics, "Tokens today")
	// Multi-step messages are summed from their parts (2,255 + 310), not read
	// from info.tokens (the last step's alone).
	if tokens.Consumed == nil || *tokens.Consumed != 2565 {
		t.Fatalf("tokens row = %+v, want 2565", tokens)
	}
	if cost := openCodeMetricByLabel(t, usage.Metrics, "Cost today"); cost.Consumed == nil || *cost.Consumed != 0.05 {
		t.Fatalf("cost row = %+v, want 0.05", cost)
	}
	if usage.UsageGeneration == nil || usage.UsageGeneration.Epoch != 2001 {
		t.Fatalf("usage generation = %+v, want this process's epoch", usage.UsageGeneration)
	}
	if usage.DataSource != "opencode store" {
		t.Fatalf("data source = %q", usage.DataSource)
	}
	// A second scan of the same store adds nothing.
	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, usage.AccountFingerprint, clock.now); b.tokens() != 2565 {
		t.Fatalf("a re-scan re-counted: %d", b.tokens())
	}
}

// Spend scanned before any probe named the account banks under "" and the
// gather adopts it.
func TestOpenCodeDirect_SpendBeforeAnyFingerprintIsAdoptedByTheGather(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2002)
	store.write(openCodeTestMessage{session: "ses_a", id: "msg_a", createdMs: clock.ms(-time.Hour), completedMs: clock.ms(-time.Hour + time.Second), steps: [][3]int64{{40, 2, 0}}})
	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 42 {
		t.Fatalf("pending bucket = %+v", b)
	}
	stubOpenCodeReadiness(t, "anthropic/claude-sonnet-4-5\n", true)
	usage, _ := openCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{Path: "opencode"}, clock.now)
	if tokens := openCodeMetricByLabel(t, usage.Metrics, "Tokens today"); *tokens.Consumed != 42 {
		t.Fatalf("adopted tokens = %+v", tokens)
	}
}

/* --------------------------------------------------------------------------
   Dedup
   -------------------------------------------------------------------------- */

// A managed stream run and the same session's stored messages count once.
func TestOpenCodeDirect_AManagedStreamRunIsCountedOnce(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2011)
	run := armOpenCodeUsageRun("opencode", "", "")
	clock.now = clock.now.Add(time.Second)
	captureOpenCodeUsageLine(run, openCodeStepFinish("ses_m", "prt_1", 500, 50, 0, "0.02", clock.ms(0)))
	openCodeUsageInFlight.Wait()
	settleOpenCodeUsageRun(run, "")
	store.write(openCodeTestMessage{session: "ses_m", id: "msg_m", createdMs: clock.ms(-500 * time.Millisecond), completedMs: clock.ms(0), steps: [][3]int64{{500, 50, 0}}, cost: 0.02})

	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 550 || b.CostUsd != 0.02 {
		t.Fatalf("bucket = %+v, want the stream's 550 once", b)
	}
}

// A message the export fallback already paid (its key in seenSteps) is not
// counted again, even with no window left to cover it.
func TestOpenCodeDirect_AnExportPaidMessageIsCountedOnce(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2012)
	exported := openCodeUsageStep{Key: openCodeUsageStepKey("ses_e", "message:msg_e"), AtMs: clock.ms(-time.Hour), Input: 70, Output: 30}
	openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		return true, mergeOpenCodeUsageSteps(ledger, "", []openCodeUsageStep{exported})
	})
	store.write(openCodeTestMessage{session: "ses_e", id: "msg_e", createdMs: clock.ms(-time.Hour - time.Second), completedMs: clock.ms(-time.Hour), steps: [][3]int64{{70, 30, 0}}})

	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 100 {
		t.Fatalf("bucket = %+v, want the export's 100 once", b)
	}
}

// A managed session the user resumes directly is counted from the moment the
// run's window closed.
func TestOpenCodeDirect_AManagedSessionResumedDirectlyCountsOnlyLaterMessages(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2013)
	run := armOpenCodeUsageRun("opencode", "", "")
	clock.now = clock.now.Add(time.Second)
	captureOpenCodeUsageLine(run, openCodeStepFinish("ses_r", "prt_1", 100, 0, 0, "0", clock.ms(0)))
	openCodeUsageInFlight.Wait()
	clock.now = clock.now.Add(time.Second)
	settleOpenCodeUsageRun(run, "") // the stream is over: the window closes
	store.write(openCodeTestMessage{session: "ses_r", id: "msg_in", createdMs: clock.ms(-1500 * time.Millisecond), completedMs: clock.ms(-time.Second), steps: [][3]int64{{100, 0, 0}}})

	clock.now = clock.now.Add(time.Minute)
	store.write(openCodeTestMessage{session: "ses_r", id: "msg_later", createdMs: clock.ms(-30 * time.Second), completedMs: clock.ms(-20 * time.Second), steps: [][3]int64{{7, 0, 0}}})
	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 107 {
		t.Fatalf("bucket = %+v, want the run's 100 plus the direct 7", b)
	}
}

/* --------------------------------------------------------------------------
   In-flight managed runs
   -------------------------------------------------------------------------- */

// An armed run with no session id holds the reader at its floor; its window
// then skips its own messages, and once the window closes later messages count.
func TestOpenCodeDirect_AnArmedRunWithoutASessionHoldsTheReader(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2021)
	store.write(openCodeTestMessage{session: "ses_before", id: "msg_before", createdMs: clock.ms(-time.Hour), completedMs: clock.ms(-time.Hour + time.Second), steps: [][3]int64{{10, 0, 0}}})
	run := armOpenCodeUsageRun("opencode", "", "")
	floor := run.floorMs
	clock.now = clock.now.Add(2 * time.Second)
	// Direct use in another session while the managed run has not named its own.
	store.write(openCodeTestMessage{session: "ses_other", id: "msg_other", createdMs: clock.ms(-time.Second), completedMs: clock.ms(0), steps: [][3]int64{{20, 0, 0}}})
	// The managed run's own message, already in the store.
	store.write(openCodeTestMessage{session: "ses_m", id: "msg_m", createdMs: clock.ms(-time.Second), completedMs: clock.ms(0), steps: [][3]int64{{500, 0, 0}}})

	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 10 {
		t.Fatalf("bucket = %+v, want only the message before the hold", b)
	}
	if c := loadOpenCodeUsageLedger().DirectCursor; c == nil || c.ThroughMs > floor {
		t.Fatalf("cursor = %+v passed the hold at %d", c, floor)
	}

	// The run names its session: its window skips its message, the other counts.
	captureOpenCodeUsageLine(run, openCodeStepFinish("ses_m", "prt_m", 500, 0, 0, "0", clock.ms(0)))
	openCodeUsageInFlight.Wait()
	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 30 {
		t.Fatalf("bucket = %+v, want the held direct message counted once the window is on disk", b)
	}

	// The stream ends: the window closes and a later direct turn on the same
	// session counts.
	settleOpenCodeUsageRun(run, "")
	clock.now = clock.now.Add(time.Minute)
	store.write(openCodeTestMessage{session: "ses_m", id: "msg_resumed", createdMs: clock.ms(-10 * time.Second), completedMs: clock.ms(-5 * time.Second), steps: [][3]int64{{4, 0, 0}}})
	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 534 {
		t.Fatalf("bucket = %+v, want 10 + 20 + the run's 500 + the resumed 4", b)
	}
}

// An armed run whose debt write was refused still holds the reader, through
// this process's memory.
func TestOpenCodeDirect_ARefusedArmStillHoldsTheReader(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2022)
	release := holdOpenCodeLedgerLock(t)
	run := armOpenCodeUsageRun("opencode", "", "")
	release()
	if !run.armRefused.Load() || len(loadOpenCodeUsageLedger().Debts) != 0 {
		t.Fatal("the arm was not refused")
	}
	clock.now = clock.now.Add(time.Second)
	store.write(openCodeTestMessage{session: "ses_x", id: "msg_x", createdMs: clock.ms(-500 * time.Millisecond), completedMs: clock.ms(0), steps: [][3]int64{{9, 0, 0}}})
	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 0 {
		t.Fatalf("bucket = %+v: a refused arm released the hold", b)
	}
	disarmOpenCodeUsageRun(run)
	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 9 {
		t.Fatalf("bucket = %+v, want the message once the run is gone", b)
	}
}

// A hold older than the debt age-out is ignored: a stuck debt cannot freeze
// direct capture.
func TestOpenCodeDirect_AStaleArmedDebtNoLongerHolds(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2023)
	openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		ledger.Debts = append(ledger.Debts, openCodeUsageDebt{RunID: "stuck", RunFloorMs: clock.ms(-7 * time.Hour)})
		return true, false
	})
	store.write(openCodeTestMessage{session: "ses_x", id: "msg_x", createdMs: clock.ms(-time.Hour), completedMs: clock.ms(-time.Hour + time.Second), steps: [][3]int64{{3, 0, 0}}})
	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 3 {
		t.Fatalf("bucket = %+v, want the stale hold ignored", b)
	}
}

/* --------------------------------------------------------------------------
   Cursor, downgrade, incomplete messages
   -------------------------------------------------------------------------- */

// A window opened before midnight and still open survives the prune; a closed
// one that ended before midnight goes.
func TestOpenCodeDirect_OpenWindowsSurviveMidnight(t *testing.T) {
	_, _, clock := openCodeDirectFixture(t, 2031)
	midnight := openCodeLocalMidnight(clock.now).UnixMilli()
	ledger := openCodeUsageLedger{OwnedRuns: []openCodeOwnedRun{
		{RunID: "open", SessionKey: "k1", FromMs: midnight - 3600_000},
		{RunID: "closed", SessionKey: "k2", FromMs: midnight - 3600_000, ToMs: midnight - 60_000},
		{RunID: "today", SessionKey: "k3", FromMs: midnight - 60_000, ToMs: midnight + 60_000},
	}}
	pruneOpenCodeOwnedRuns(&ledger, clock.ms(0))
	if len(ledger.OwnedRuns) != 2 || ledger.OwnedRuns[0].RunID != "open" || ledger.OwnedRuns[1].RunID != "today" {
		t.Fatalf("windows = %+v", ledger.OwnedRuns)
	}
	// Past the cap the oldest CLOSED window goes, never an open one.
	ledger.OwnedRuns = nil
	for i := 0; i < openCodeUsageMaxOwnedRuns; i++ {
		ledger.OwnedRuns = append(ledger.OwnedRuns, openCodeOwnedRun{RunID: fmt.Sprint(i), FromMs: clock.ms(-time.Hour), ToMs: clock.ms(time.Duration(i) * time.Second)})
	}
	ledger.OwnedRuns = append(ledger.OwnedRuns, openCodeOwnedRun{RunID: "open", FromMs: clock.ms(-2 * time.Hour)})
	pruneOpenCodeOwnedRuns(&ledger, clock.ms(0))
	if len(ledger.OwnedRuns) != openCodeUsageMaxOwnedRuns || ledger.OwnedRuns[0].RunID != "1" || openCodeOwnedRunByID(&ledger, "open") == nil {
		t.Fatalf("capped windows = %+v", ledger.OwnedRuns)
	}
}

// Evicting a closed window the cursor has not passed would let the reader count
// that managed turn's stored message as direct use. The eviction raises a floor
// instead, and the turn is never counted; a window the cursor passed goes
// without one.
func TestOpenCodeDirect_AnEvictedUnpassedWindowStillGuardsItsMessages(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2034)
	managed := openCodeTestMessage{session: "ses_m", id: "msg_m", createdMs: clock.ms(-30 * time.Minute), completedMs: clock.ms(-29 * time.Minute), steps: [][3]int64{{500, 0, 0}}}
	store.write(managed)
	direct := openCodeTestMessage{session: "ses_d", id: "msg_d", createdMs: clock.ms(-time.Minute), completedMs: clock.ms(-50 * time.Second), steps: [][3]int64{{7, 0, 0}}}
	store.write(direct)
	openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		ledger.OwnedRuns = append(ledger.OwnedRuns, openCodeOwnedRun{RunID: "managed", SessionKey: openCodeUsageSessionKey("ses_m"), FromMs: clock.ms(-31 * time.Minute), ToMs: clock.ms(-28 * time.Minute)})
		for i := 0; i < openCodeUsageMaxOwnedRuns; i++ {
			ledger.OwnedRuns = append(ledger.OwnedRuns, openCodeOwnedRun{RunID: fmt.Sprint(i), SessionKey: openCodeUsageSessionKey(fmt.Sprint("ses_", i)), FromMs: clock.ms(-20 * time.Minute), ToMs: clock.ms(-10 * time.Minute)})
		}
		pruneOpenCodeOwnedRuns(ledger, clock.ms(0))
		return true, false
	})
	ledger := loadOpenCodeUsageLedger()
	if openCodeOwnedRunByID(&ledger, "managed") != nil || ledger.OwnedEvictedMs != clock.ms(-28*time.Minute) {
		t.Fatalf("eviction = %+v, floor %d", ledger.OwnedRuns, ledger.OwnedEvictedMs)
	}
	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 7 {
		t.Fatalf("bucket = %+v, want the direct message only", b)
	}

	// Once the cursor has passed a window, evicting it raises no floor.
	passed := openCodeUsageLedger{DirectCursor: &openCodeDirectCursor{ThroughMs: clock.ms(0)}}
	for i := 0; i <= openCodeUsageMaxOwnedRuns; i++ {
		passed.OwnedRuns = append(passed.OwnedRuns, openCodeOwnedRun{RunID: fmt.Sprint(i), FromMs: clock.ms(-time.Hour), ToMs: clock.ms(-30*time.Minute + time.Duration(i)*time.Second)})
	}
	pruneOpenCodeOwnedRuns(&passed, clock.ms(0))
	if len(passed.OwnedRuns) != openCodeUsageMaxOwnedRuns || passed.OwnedEvictedMs != 0 {
		t.Fatalf("passed eviction = %d windows, floor %d", len(passed.OwnedRuns), passed.OwnedEvictedMs)
	}
}

// A missing cursor with today's bucket present — an older build rewrote the
// ledger — starts at that bucket's observation, never re-counting from midnight.
func TestOpenCodeDirect_ADowngradedLedgerDoesNotReCount(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2032)
	store.write(openCodeTestMessage{session: "ses_a", id: "msg_old", createdMs: clock.ms(-3 * time.Hour), completedMs: clock.ms(-3*time.Hour + time.Second), steps: [][3]int64{{100, 0, 0}}})
	scanOpenCodeDirect(t)
	// A managed run commits later, then an older build rewrites the ledger:
	// buckets kept, the new fields dropped, and seenSteps trimmed — here past
	// the direct message's key.
	openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		return true, mergeOpenCodeUsageSteps(ledger, "", []openCodeUsageStep{{Key: "stream", AtMs: clock.ms(-2 * time.Hour), Input: 1}})
	})
	openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		ledger.DirectCursor, ledger.DirectCoverage, ledger.OwnedRuns, ledger.SeenSteps = nil, nil, nil, nil
		return true, false
	})
	store.write(openCodeTestMessage{session: "ses_a", id: "msg_new", createdMs: clock.ms(-time.Minute), completedMs: clock.ms(-50 * time.Second), steps: [][3]int64{{5, 0, 0}}})
	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 106 {
		t.Fatalf("bucket = %+v, want 100 + 1 + 5 with nothing re-counted", b)
	}
	if got := openCodeDirectScanFloor(openCodeUsageLedger{Buckets: []openCodeUsageBucket{{LocalDate: "2026-10-08", ObservedAtMs: clock.ms(-time.Hour)}}}, clock.now); got != clock.ms(-time.Hour) {
		t.Fatalf("floor = %d, want the bucket's observation", got)
	}
}

// A message still running is skipped and holds the cursor; it is counted once
// it completes.
func TestOpenCodeDirect_AnIncompleteMessageIsCountedOnceComplete(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2033)
	running := openCodeTestMessage{session: "ses_a", id: "msg_run", createdMs: clock.ms(-time.Hour), writtenMs: clock.ms(-time.Hour), steps: [][3]int64{{60, 0, 0}}}
	store.write(running)
	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 0 {
		t.Fatalf("an incomplete message counted: %+v", b)
	}
	if c := loadOpenCodeUsageLedger().DirectCursor; c == nil || c.ThroughMs > running.createdMs {
		t.Fatalf("cursor %+v passed the running message", c)
	}
	clock.now = clock.now.Add(2 * time.Hour)
	running.completedMs = clock.ms(-time.Minute)
	running.writtenMs = 0
	store.write(running)
	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 60 {
		t.Fatalf("bucket = %+v, want the completed message", b)
	}
}

/* --------------------------------------------------------------------------
   Bounds
   -------------------------------------------------------------------------- */

func TestOpenCodeDirect_CapsTruncateIntoAnUnderCountThatResumes(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2041)
	for i := 0; i < 3; i++ {
		store.write(openCodeTestMessage{session: "ses_a", id: fmt.Sprintf("msg_%d", i), createdMs: clock.ms(time.Duration(i-10) * time.Minute), completedMs: clock.ms(time.Duration(i-10)*time.Minute + time.Second), steps: [][3]int64{{1, 0, 0}}})
	}
	openCodeDirectMaxRecords = 2
	if label := scanOpenCodeDirect(t); label != "direct_truncated" {
		t.Fatalf("scan = %q, want truncated", label)
	}
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 2 {
		t.Fatalf("capped bucket = %+v", b)
	}
	openCodeDirectMaxRecords = 8192
	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 3 {
		t.Fatalf("resumed bucket = %+v, want each message once", b)
	}

	// The session cap.
	store.write(openCodeTestMessage{session: "ses_b", id: "msg_b", createdMs: clock.ms(-time.Second), completedMs: clock.ms(0), steps: [][3]int64{{10, 0, 0}}})
	store.write(openCodeTestMessage{session: "ses_c", id: "msg_c", createdMs: clock.ms(-time.Second), completedMs: clock.ms(0), steps: [][3]int64{{10, 0, 0}}})
	openCodeDirectMaxSessions = 1
	clock.now = clock.now.Add(time.Second)
	if label := scanOpenCodeDirect(t); label != "direct_truncated" {
		t.Fatalf("session-capped scan = %q", label)
	}
}

// A capped scan whose backlog sits inside the overlap still drains it: the next
// scan resumes at the cursor rather than re-reading the records it counted.
func TestOpenCodeDirect_ACappedBacklogInsideTheOverlapDrains(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2044)
	for i := 0; i < 3; i++ {
		store.write(openCodeTestMessage{session: "ses_a", id: fmt.Sprintf("msg_%d", i), createdMs: clock.ms(time.Duration(i-5) * time.Minute), completedMs: clock.ms(time.Duration(i-5)*time.Minute + time.Second), steps: [][3]int64{{1, 0, 0}}})
	}
	openCodeDirectMaxRecords = 2
	scanOpenCodeDirect(t)
	if c := loadOpenCodeUsageLedger().DirectCursor; c == nil || !c.Continue {
		t.Fatalf("capped cursor = %+v, want a continuation", c)
	}
	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 3 {
		t.Fatalf("bucket = %+v, want the backlog drained under the same cap", b)
	}
	// A full scan goes back to re-reading the overlap.
	if c := loadOpenCodeUsageLedger().DirectCursor; c == nil || c.Continue {
		t.Fatalf("cursor after a full scan = %+v", c)
	}
}

// More sessions than the cap inside the session slack still drain: a scan the
// session cap stopped resumes at the first unread session, not a minute before
// it, where the same capped sessions would fill the cap again.
func TestOpenCodeDirect_ASessionCappedBacklogInsideTheSlackDrains(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2046)
	for i := 0; i < 5; i++ {
		session, at := fmt.Sprintf("ses_%d", i), clock.ms(time.Duration(i*5-30)*time.Second)
		store.write(openCodeTestMessage{session: session, id: "msg_" + session, createdMs: at - 1000, completedMs: at, steps: [][3]int64{{1, 0, 0}}})
		if err := os.Chtimes(filepath.Join(store.root, "storage", "message", session), time.UnixMilli(at), time.UnixMilli(at)); err != nil {
			t.Fatal(err)
		}
	}
	openCodeDirectMaxSessions = 2
	scanOpenCodeDirect(t)
	if c := loadOpenCodeUsageLedger().DirectCursor; c == nil || !c.Continue || !c.AtSession {
		t.Fatalf("session-capped cursor = %+v", c)
	}
	for i := 0; i < 3; i++ {
		scanOpenCodeDirect(t)
	}
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 5 {
		t.Fatalf("bucket = %+v, want every session drained under the same cap", b)
	}
}

// A session-capped continuation reads the sessions it has not listed yet from
// the capped scan's record floor, not from their own last write: messages
// written before something touched every session directory are still counted.
func TestOpenCodeDirect_ASessionContinuationKeepsTheRecordFloor(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2048)
	touched := clock.ms(-time.Minute)
	for i := 0; i < 3; i++ {
		session, at := fmt.Sprintf("ses_%d", i), clock.ms(time.Duration(i-30)*time.Minute)
		store.write(openCodeTestMessage{session: session, id: "msg_" + session, createdMs: at - 1000, completedMs: at, steps: [][3]int64{{1, 0, 0}}})
		if err := os.Chtimes(filepath.Join(store.root, "storage", "message", session), time.UnixMilli(touched), time.UnixMilli(touched)); err != nil {
			t.Fatal(err)
		}
	}
	openCodeDirectMaxSessions = 1
	scanOpenCodeDirect(t)
	c := loadOpenCodeUsageLedger().DirectCursor
	if c == nil || !c.Continue || !c.AtSession || c.ThroughMs != touched || c.RecordFloorMs >= clock.ms(-30*time.Minute) {
		t.Fatalf("session-capped cursor = %+v, want the record floor kept apart from the session cut", c)
	}
	for i := 0; i < 2; i++ {
		scanOpenCodeDirect(t)
	}
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 3 {
		t.Fatalf("bucket = %+v, want every session's earlier message counted", b)
	}
}

// A record continuation keeps listing sessions from the capped scan's session
// floor: a session listed then, whose directory was last written well before
// the cut, still holds a message rewritten in place after it, and recomputing
// the floor from the cut would drop that session and its unread message.
func TestOpenCodeDirect_ARecordContinuationKeepsTheSessionFloor(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2052)
	store.write(openCodeTestMessage{session: "ses_a", id: "msg_0", createdMs: clock.ms(-16 * time.Minute), completedMs: clock.ms(-15 * time.Minute), steps: [][3]int64{{1, 0, 0}}})
	store.write(openCodeTestMessage{session: "ses_a", id: "msg_1", createdMs: clock.ms(-16 * time.Minute), completedMs: clock.ms(-5 * time.Minute), steps: [][3]int64{{100, 0, 0}}})
	store.write(openCodeTestMessage{session: "ses_b", id: "msg_b", createdMs: clock.ms(-15 * time.Minute), completedMs: clock.ms(-14 * time.Minute), steps: [][3]int64{{10, 0, 0}}})
	dirTimes := map[string]int64{"ses_a": clock.ms(-20 * time.Minute), "ses_b": clock.ms(-14 * time.Minute)}
	for session, at := range dirTimes {
		if err := os.Chtimes(filepath.Join(store.root, "storage", "message", session), time.UnixMilli(at), time.UnixMilli(at)); err != nil {
			t.Fatal(err)
		}
	}
	openCodeDirectMaxRecords = 1
	scanOpenCodeDirect(t)
	c := loadOpenCodeUsageLedger().DirectCursor
	if c == nil || !c.Continue || c.AtSession || c.ThroughMs != clock.ms(-14*time.Minute) || c.SessionSinceMs > dirTimes["ses_a"] {
		t.Fatalf("record-capped cursor = %+v, want the capped scan's session floor kept", c)
	}
	for i := 0; i < 3; i++ {
		scanOpenCodeDirect(t)
	}
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 111 {
		t.Fatalf("bucket = %+v, want the message rewritten in an older session counted", b)
	}
}

// A continuation cut by one adapter is not replayed against another: after the
// store switches layout, its skip would index records the new adapter orders
// differently at the cut, and could skip an unread one. The scan re-reads from
// the cut instead, and the record at it is counted once.
func TestOpenCodeDirect_ALayoutSwitchDropsTheContinuationState(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2053)
	cut := clock.ms(-5 * time.Minute)
	store.write(openCodeTestMessage{session: "ses_a", id: "msg_a", createdMs: cut - 1000, completedMs: cut, steps: [][3]int64{{10, 0, 0}}})
	openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		ledger.DirectCursor = &openCodeDirectCursor{Layout: openCodeStoreLayoutSQLite, ThroughMs: cut, Continue: true, Skip: 1, SessionSinceMs: cut}
		return true, false
	})
	var got []openCodeDirectLimits
	prev := openCodeDirectReaders[openCodeStoreLayoutJSON]
	openCodeDirectReaders[openCodeStoreLayoutJSON] = func(ctx context.Context, root string, floorMs int64, limits openCodeDirectLimits) (openCodeDirectRead, error) {
		got = append(got, limits)
		return prev(ctx, root, floorMs, limits)
	}
	t.Cleanup(func() { openCodeDirectReaders[openCodeStoreLayoutJSON] = prev })

	scanOpenCodeDirect(t)
	if len(got) != 1 || got[0].Skip.N != 0 || got[0].ExactSessions || got[0].SessionSinceMs != 0 {
		t.Fatalf("limits after a layout switch = %+v, want no continuation state", got)
	}
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 10 {
		t.Fatalf("bucket = %+v, want the record at the cut counted", b)
	}
}

// When the session cap cuts first and the record cap cuts later in the same
// read, the cursor resumes the way the earliest cut stopped — by session, with
// the record floor — so the unread sessions' earlier messages are still
// counted. The record left past the later cut, rewritten in place in a session
// already listed, is the accepted under-count.
func TestOpenCodeDirect_AMixedCapResumesByTheEarliestCut(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2050)
	sessionCut := clock.ms(-time.Minute)
	store.write(openCodeTestMessage{session: "ses_0", id: "msg_a", createdMs: clock.ms(-31 * time.Minute), completedMs: clock.ms(-30 * time.Minute), steps: [][3]int64{{1, 0, 0}}})
	store.write(openCodeTestMessage{session: "ses_0", id: "msg_b", createdMs: clock.ms(-21 * time.Minute), completedMs: clock.ms(-20 * time.Minute), writtenMs: clock.ms(-10 * time.Second), steps: [][3]int64{{100, 0, 0}}})
	for i := 1; i < 3; i++ {
		session, at := fmt.Sprintf("ses_%d", i), clock.ms(time.Duration(i-30)*time.Minute)
		store.write(openCodeTestMessage{session: session, id: "msg_" + session, createdMs: at - 1000, completedMs: at, steps: [][3]int64{{1, 0, 0}}})
	}
	dirTimes := map[string]int64{"ses_0": clock.ms(-2 * time.Minute), "ses_1": sessionCut, "ses_2": sessionCut}
	for session, at := range dirTimes {
		if err := os.Chtimes(filepath.Join(store.root, "storage", "message", session), time.UnixMilli(at), time.UnixMilli(at)); err != nil {
			t.Fatal(err)
		}
	}
	openCodeDirectMaxSessions, openCodeDirectMaxRecords = 1, 1
	scanOpenCodeDirect(t)
	c := loadOpenCodeUsageLedger().DirectCursor
	if c == nil || !c.Continue || !c.AtSession || c.ThroughMs != sessionCut || c.Skip != 0 || c.RecordFloorMs >= clock.ms(-30*time.Minute) {
		t.Fatalf("mixed-cap cursor = %+v, want a session continuation at the session cut", c)
	}
	for i := 0; i < 3; i++ {
		scanOpenCodeDirect(t)
	}
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 3 {
		t.Fatalf("bucket = %+v, want msg_a and both unread sessions' messages", b)
	}
}

// truncate keeps the earliest cut and resumes the way it stopped; at one
// millisecond a session cut wins, and a skip only counts entries of its kind.
func TestOpenCodeDirectRead_TruncateKeepsTheEarliestCutsKind(t *testing.T) {
	var r openCodeDirectRead
	r.truncateSessionsAt(100, 2)
	r.truncateAt(200, 5)
	if !r.Truncated || !r.AtSession || r.ThroughMs != 100 || r.ThroughSkip != 2 {
		t.Fatalf("session then later record cut = %+v", r)
	}
	r = openCodeDirectRead{}
	r.truncateAt(100, 5)
	r.truncateSessionsAt(200, 2)
	if r.AtSession || r.ThroughMs != 100 || r.ThroughSkip != 5 {
		t.Fatalf("record then later session cut = %+v", r)
	}
	r.truncateSessionsAt(100, 9)
	if !r.AtSession || r.ThroughMs != 100 || r.ThroughSkip != 9 {
		t.Fatalf("a session cut at the record cut's millisecond = %+v", r)
	}
	r.truncateAt(100, 0)
	if !r.AtSession || r.ThroughSkip != 9 {
		t.Fatalf("a record cut at the session cut's millisecond = %+v", r)
	}
	r.truncateSessionsAt(100, 3)
	if !r.AtSession || r.ThroughSkip != 3 {
		t.Fatalf("an earlier session at the same millisecond = %+v", r)
	}
}

// After a capped backlog drains, the overlap does not rewind into it: seenSteps
// holds no more keys than one scan reads, so the drained records' keys may have
// rolled over and a re-read would count them again.
func TestOpenCodeDirect_ADrainedBacklogIsNotRewoundInto(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2049)
	for i := 0; i < 3; i++ {
		store.write(openCodeTestMessage{session: "ses_a", id: fmt.Sprintf("msg_%d", i), createdMs: clock.ms(time.Duration(i-5) * time.Minute), completedMs: clock.ms(time.Duration(i-5)*time.Minute + time.Second), steps: [][3]int64{{1, 0, 0}}})
	}
	var floors []int64
	prev := openCodeDirectReaders[openCodeStoreLayoutJSON]
	openCodeDirectReaders[openCodeStoreLayoutJSON] = func(ctx context.Context, root string, floorMs int64, limits openCodeDirectLimits) (openCodeDirectRead, error) {
		floors = append(floors, floorMs)
		return prev(ctx, root, floorMs, limits)
	}
	t.Cleanup(func() { openCodeDirectReaders[openCodeStoreLayoutJSON] = prev })

	openCodeDirectMaxRecords = 2
	scanOpenCodeDirect(t)
	scanOpenCodeDirect(t)
	drained := loadOpenCodeUsageLedger().DirectCursor
	if drained == nil || drained.Continue || drained.RewindFloorMs != clock.ms(0) {
		t.Fatalf("cursor after the backlog drained = %+v, want a rewind floor at its end", drained)
	}

	clock.now = clock.now.Add(time.Minute)
	store.write(openCodeTestMessage{session: "ses_a", id: "msg_new", createdMs: clock.ms(-time.Second), completedMs: clock.ms(0), steps: [][3]int64{{10, 0, 0}}})
	scanOpenCodeDirect(t)
	if got := floors[len(floors)-1]; got != drained.ThroughMs {
		t.Fatalf("scan after the drain read from %d, want the drain's end %d", got, drained.ThroughMs)
	}
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 13 {
		t.Fatalf("bucket = %+v, want each message once", b)
	}

	// Once the overlap no longer reaches it, the floor is dropped.
	clock.now = clock.now.Add(2 * openCodeDirectOverlap)
	store.write(openCodeTestMessage{session: "ses_a", id: "msg_later", createdMs: clock.ms(-time.Second), completedMs: clock.ms(0), steps: [][3]int64{{1, 0, 0}}})
	scanOpenCodeDirect(t)
	if c := loadOpenCodeUsageLedger().DirectCursor; c == nil || c.RewindFloorMs != 0 {
		t.Fatalf("cursor = %+v, want the passed rewind floor dropped", c)
	}
}

// When managed captures (or an export) roll seenSteps over after a completed
// direct scan, the direct messages previously counted lose their dedup keys.
// The next direct scan must not rewind into them, and keeps the floor while the
// overlap can reach them.
func TestOpenCodeDirect_ManagedRunsRollingSeenStepsOverDoNotCauseDirectRewindDoubleCount(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2050)
	store.write(openCodeTestMessage{session: "ses_a", id: "msg_direct_0", createdMs: clock.ms(-5 * time.Minute), completedMs: clock.ms(-5*time.Minute + time.Second), steps: [][3]int64{{10, 0, 0}}})

	var floors []int64
	prev := openCodeDirectReaders[openCodeStoreLayoutJSON]
	openCodeDirectReaders[openCodeStoreLayoutJSON] = func(ctx context.Context, root string, floorMs int64, limits openCodeDirectLimits) (openCodeDirectRead, error) {
		floors = append(floors, floorMs)
		return prev(ctx, root, floorMs, limits)
	}
	t.Cleanup(func() { openCodeDirectReaders[openCodeStoreLayoutJSON] = prev })

	// First direct scan: counts msg_direct_0.
	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 10 {
		t.Fatalf("bucket = %+v, want 10 tokens", b)
	}
	cursor0 := loadOpenCodeUsageLedger().DirectCursor
	if cursor0 == nil || cursor0.ThroughMs == 0 {
		t.Fatalf("cursor0 = %+v", cursor0)
	}

	// Managed capture appends enough stream keys to roll seenSteps over and evict msg_direct_0's key.
	managedSteps := make([]openCodeUsageStep, openCodeUsageMaxSeenSteps+10)
	for i := range managedSteps {
		managedSteps[i] = openCodeUsageStep{Key: openCodeUsageStepKey("ses_managed", fmt.Sprint(i)), AtMs: clock.ms(0), Input: 1}
	}
	openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		return true, mergeOpenCodeUsageSteps(ledger, "managed_fp", managedSteps)
	})

	afterManaged := loadOpenCodeUsageLedger()
	if afterManaged.DirectCursor.RewindFloorMs != cursor0.ThroughMs {
		t.Fatalf("rewindFloorMs = %d, want cursor0.ThroughMs %d", afterManaged.DirectCursor.RewindFloorMs, cursor0.ThroughMs)
	}

	// Next store change: 1 minute later, user runs OpenCode directly again.
	clock.now = clock.now.Add(time.Minute)
	store.write(openCodeTestMessage{session: "ses_a", id: "msg_direct_1", createdMs: clock.ms(-time.Second), completedMs: clock.ms(0), steps: [][3]int64{{5, 0, 0}}})
	scanOpenCodeDirect(t)

	if got := floors[len(floors)-1]; got != cursor0.ThroughMs {
		t.Fatalf("scan after managed capture read from %d, want cursor0.ThroughMs %d", got, cursor0.ThroughMs)
	}
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 15 {
		t.Fatalf("bucket = %+v, want 15 tokens (10 + 5, no double count of msg_direct_0)", b)
	}

	cursor1 := loadOpenCodeUsageLedger().DirectCursor
	if cursor1.RewindFloorMs != cursor1.ThroughMs {
		t.Fatalf("cursor1.RewindFloorMs = %d, want cursor1.ThroughMs %d", cursor1.RewindFloorMs, cursor1.ThroughMs)
	}
}

// When a single direct scan rolls seenSteps over, the new cursor sets its
// RewindFloorMs to throughMs so a subsequent scan within the overlap does not
// rewind before records whose keys rolled over.
func TestOpenCodeDirect_DirectScanRollingSeenStepsOverSetsRewindFloor(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2051)
	openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		for i := 0; i < openCodeUsageMaxSeenSteps-2; i++ {
			ledger.SeenSteps = append(ledger.SeenSteps, openCodeUsageStepKey("s", fmt.Sprint(i)))
		}
		return true, false
	})

	for i := 0; i < 5; i++ {
		store.write(openCodeTestMessage{session: "ses_a", id: fmt.Sprintf("msg_%d", i), createdMs: clock.ms(time.Duration(i-5) * time.Minute), completedMs: clock.ms(time.Duration(i-5)*time.Minute + time.Second), steps: [][3]int64{{1, 0, 0}}})
	}
	scanOpenCodeDirect(t)
	c := loadOpenCodeUsageLedger().DirectCursor
	if c == nil || c.RewindFloorMs != clock.ms(0) {
		t.Fatalf("cursor = %+v, want RewindFloorMs = %d", c, clock.ms(0))
	}
}

// More records, or more sessions, than the cap sharing one millisecond still
// drain: the continuation saves how many it read at its cut time, so a resume
// does not select the same capped prefix again.
func TestOpenCodeDirect_ACappedBacklogInOneMillisecondDrains(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2047)
	at := clock.ms(-time.Minute)
	for i := 0; i < 5; i++ {
		store.write(openCodeTestMessage{session: "ses_a", id: fmt.Sprintf("msg_%d", i), createdMs: at - 1000, completedMs: at - 500, writtenMs: at, steps: [][3]int64{{1, 0, 0}}})
	}
	openCodeDirectMaxRecords = 2
	for i := 0; i < 3; i++ {
		scanOpenCodeDirect(t)
	}
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 5 {
		t.Fatalf("records: bucket = %+v, want every record drained under the same cap", b)
	}

	openCodeDirectMaxRecords = 8192
	clock.now = clock.now.Add(time.Hour)
	at = clock.ms(-time.Minute)
	for i := 0; i < 5; i++ {
		session := fmt.Sprintf("ses_t%d", i)
		store.write(openCodeTestMessage{session: session, id: "msg_" + session, createdMs: at - 1000, completedMs: at - 500, writtenMs: at, steps: [][3]int64{{10, 0, 0}}})
		if err := os.Chtimes(filepath.Join(store.root, "storage", "message", session), time.UnixMilli(at), time.UnixMilli(at)); err != nil {
			t.Fatal(err)
		}
	}
	openCodeDirectMaxSessions = 2
	for i := 0; i < 3; i++ {
		scanOpenCodeDirect(t)
	}
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 55 {
		t.Fatalf("sessions: bucket = %+v, want every session drained under the same cap", b)
	}
}

// A JSON turn begun before midnight and finished after it is counted, though
// rewriting its record in place did not move its session directory.
func TestOpenCodeDirect_ATurnAcrossMidnightIsCounted(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2045)
	midnight := openCodeLocalMidnight(clock.now)
	for _, withCursor := range []bool{false, true} {
		session := fmt.Sprintf("ses_%v", withCursor)
		clock.now = midnight.Add(30 * time.Minute)
		created, completed := midnight.Add(-20*time.Minute).UnixMilli(), midnight.Add(5*time.Minute).UnixMilli()
		openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
			ledger.DirectCursor = nil
			if withCursor {
				// Yesterday's last scan, held by the running turn.
				ledger.DirectCursor = &openCodeDirectCursor{Layout: openCodeStoreLayoutJSON, ThroughMs: created}
			}
			return true, false
		})
		store.write(openCodeTestMessage{session: session, id: "msg_" + session, createdMs: created, completedMs: completed, steps: [][3]int64{{5, 0, 0}}})
		dir := filepath.Join(store.root, "storage", "message", session)
		if err := os.Chtimes(dir, time.UnixMilli(created), time.UnixMilli(created)); err != nil {
			t.Fatal(err)
		}
		scanOpenCodeDirect(t)
		want := int64(5)
		if withCursor {
			want = 10
		}
		if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != want {
			t.Fatalf("cursor %v: bucket = %+v, want %d", withCursor, b, want)
		}
	}
}

func TestOpenCodeDirect_ASpentBudgetCountsNothingItDidNotRead(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2042)
	store.write(openCodeTestMessage{session: "ses_a", id: "msg_a", createdMs: clock.ms(-time.Hour), completedMs: clock.ms(-time.Hour + time.Second), steps: [][3]int64{{8, 0, 0}}})
	if label := openCodeDirectScanShared(0); label != "direct_truncated" {
		t.Fatalf("scan with no budget = %q", label)
	}
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 0 {
		t.Fatalf("bucket = %+v", b)
	}
	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 8 {
		t.Fatalf("bucket = %+v after a full scan", b)
	}
}

// Oversized, malformed, negative and non-finite records are rejected without
// failing the scan.
func TestOpenCodeDirect_BadRecordsAreRejectedWithoutFailingTheScan(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2043)
	at := clock.ms(-time.Hour)
	dir := filepath.Join(store.root, "storage", "message", "ses_a")
	store.writeFile(filepath.Join(dir, "msg_garbage.json"), `{not json`, at)
	store.writeFile(filepath.Join(dir, "msg_negative.json"), fmt.Sprintf(`{"id":"msg_negative","sessionID":"ses_a","role":"assistant","time":{"created":%d,"completed":%d},"cost":0,"tokens":{"input":-5,"output":0}}`, at, at+1), at)
	store.writeFile(filepath.Join(dir, "msg_huge.json"), fmt.Sprintf(`{"id":"msg_huge","sessionID":"ses_a","role":"assistant","time":{"created":%d,"completed":%d},"cost":0,"tokens":{"input":1e400,"output":0}}`, at, at+1), at)
	store.writeFile(filepath.Join(dir, "msg_big.json"), `{"id":"msg_big","pad":"`+strings.Repeat("x", 4096)+`"}`, at)
	store.write(openCodeTestMessage{session: "ses_a", id: "msg_ok", createdMs: at, completedMs: at + 1, steps: [][3]int64{{11, 0, 0}}})
	openCodeDirectMaxRecordBytes = 2048
	if label := scanOpenCodeDirect(t); label != "direct_truncated" {
		t.Fatalf("scan = %q, want the skipped records reported", label)
	}
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 11 {
		t.Fatalf("bucket = %+v, want only the valid message", b)
	}
}

/* --------------------------------------------------------------------------
   Calendar, locks, layout
   -------------------------------------------------------------------------- */

// Local midnight and a DST day use openCodeUsageLocation; a message completed
// yesterday lands in yesterday's bucket.
func TestOpenCodeDirect_CalendarBoundariesUseTheUsageZone(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2051)
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no zone database")
	}
	openCodeUsageLocation = ny
	// 2026-03-08 is a 23-hour day in New York.
	clock.now = time.Date(2026, 3, 8, 0, 30, 0, 0, ny)
	if m := openCodeLocalMidnight(clock.now); !m.Equal(time.Date(2026, 3, 8, 0, 0, 0, 0, ny)) {
		t.Fatalf("midnight = %s", m)
	}
	if _, reset := openCodeLocalDay(clock.now); reset.Sub(openCodeLocalMidnight(clock.now)) != 23*time.Hour {
		t.Fatalf("DST day length = %s", reset.Sub(openCodeLocalMidnight(clock.now)))
	}
	// Completed a minute before midnight, its record last written after it.
	store.write(openCodeTestMessage{session: "ses_a", id: "msg_late", createdMs: clock.ms(-32 * time.Minute), completedMs: clock.ms(-31 * time.Minute), writtenMs: clock.ms(-20 * time.Minute), steps: [][3]int64{{6, 0, 0}}})
	store.write(openCodeTestMessage{session: "ses_a", id: "msg_today", createdMs: clock.ms(-10 * time.Minute), completedMs: clock.ms(-9 * time.Minute), steps: [][3]int64{{4, 0, 0}}})
	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now.Add(-time.Hour)); b.LocalDate != "2026-03-07" || b.tokens() != 6 {
		t.Fatalf("yesterday = %+v", b)
	}
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 4 {
		t.Fatalf("today = %+v", b)
	}
}

// With the ledger lock held by another process the scan commits nothing,
// leaves the cursor alone and runs again on the next tick.
func TestOpenCodeDirect_AHeldLedgerLockCommitsNothingAndRetries(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2052)
	prevWait := openCodeUsageLockWait
	openCodeUsageLockWait = 20 * time.Millisecond
	t.Cleanup(func() { openCodeUsageLockWait = prevWait })
	prevDetected := openCodeDirectDetected
	openCodeDirectDetected = func() bool { return true }
	t.Cleanup(func() { openCodeDirectDetected = prevDetected })
	store.write(openCodeTestMessage{session: "ses_a", id: "msg_a", createdMs: clock.ms(-time.Hour), completedMs: clock.ms(-time.Hour + time.Second), steps: [][3]int64{{12, 0, 0}}})

	release := holdOpenCodeLedgerLock(t)
	if label := scanOpenCodeDirect(t); label != "direct_lock_busy" {
		t.Fatalf("scan = %q", label)
	}
	release()
	ledger := loadOpenCodeUsageLedger()
	if ledger.DirectCursor != nil || len(ledger.Buckets) != 0 {
		t.Fatalf("a refused scan moved the ledger: %+v", ledger)
	}
	// The store did not change, but the refused scan left work: the tick scans.
	openCodeDirectScanIfChanged()
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 12 {
		t.Fatalf("bucket = %+v after the retry", b)
	}
}

// An unrecognised store fails closed: nothing merged and no coverage, so the
// card keeps "(agent runs)".
func TestOpenCodeDirect_AnUnknownLayoutFailsClosed(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2053)
	if err := os.RemoveAll(filepath.Join(store.root, "storage")); err != nil {
		t.Fatal(err)
	}
	if label := scanOpenCodeDirect(t); label != "direct_layout_unknown" {
		t.Fatalf("scan = %q", label)
	}
	// Records in a shape no adapter knows.
	store.writeFile(filepath.Join(store.root, "storage", "message", "ses_a", "msg_a.json"), `{"kind":"something else"}`, clock.ms(-time.Minute))
	if label := scanOpenCodeDirect(t); label != "direct_layout_unknown" {
		t.Fatalf("scan = %q", label)
	}
	if ledger := loadOpenCodeUsageLedger(); ledger.DirectCoverage != nil {
		t.Fatalf("coverage = %+v for an unknown store", ledger.DirectCoverage)
	}
	raw, _ := json.Marshal(openCodeUsageMetrics(openCodeUsageBucket{InputTokens: 1, ObservedAtMs: clock.ms(0)}, true, openCodeUsageDayFor("", clock.now).Direct, clock.now))
	if !json.Valid(raw) || !strings.Contains(string(raw), "Tokens today (agent runs)") {
		t.Fatalf("label = %s", raw)
	}
}

// A scan that finds nothing new skips the whole-ledger rewrite while its cursor
// stays inside the overlap, and writes again once it would fall behind.
func TestOpenCodeDirect_AnIdleScanSkipsTheLedgerRewrite(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2061)
	store.write(openCodeTestMessage{session: "ses_a", id: "msg_a", createdMs: clock.ms(-time.Hour), completedMs: clock.ms(-time.Hour + time.Second), steps: [][3]int64{{2, 0, 0}}})
	if label := scanOpenCodeDirect(t); label != "direct_scanned" {
		t.Fatalf("first scan = %q", label)
	}
	first := loadOpenCodeUsageLedger().DirectCursor.ThroughMs
	clock.now = clock.now.Add(time.Minute)
	if label := scanOpenCodeDirect(t); label != "direct_unchanged" {
		t.Fatalf("idle scan = %q", label)
	}
	if c := loadOpenCodeUsageLedger().DirectCursor; c.ThroughMs != first {
		t.Fatalf("an idle scan rewrote the cursor: %+v", c)
	}
	clock.now = clock.now.Add(openCodeDirectOverlap)
	if label := scanOpenCodeDirect(t); label != "direct_scanned" {
		t.Fatalf("scan past the overlap = %q", label)
	}
	if c := loadOpenCodeUsageLedger().DirectCursor; c.ThroughMs != clock.ms(0) {
		t.Fatalf("cursor = %+v, want advanced to the scan", c)
	}
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 2 {
		t.Fatalf("bucket = %+v", b)
	}
}

// A session cut whose selected session contains an incomplete message at the
// cut timestamp is treated as a record continuation, so the next scan does not
// skip the session and revisits the message once complete.
func TestOpenCodeDirect_SessionCutWithIncompleteMessageAtCutBecomesRecordContinuation(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2062)
	cut := clock.ms(-10 * time.Minute)
	store.write(openCodeTestMessage{
		session:     "ses_a",
		id:          "msg_a_incomplete",
		createdMs:   cut,
		completedMs: 0,
		steps:       [][3]int64{{42, 0, 0}},
	})
	store.write(openCodeTestMessage{
		session:     "ses_b",
		id:          "msg_b",
		createdMs:   cut - 1000,
		completedMs: cut,
		steps:       [][3]int64{{10, 0, 0}},
	})
	for _, s := range []string{"ses_a", "ses_b"} {
		if err := os.Chtimes(filepath.Join(store.root, "storage", "message", s), time.UnixMilli(cut), time.UnixMilli(cut)); err != nil {
			t.Fatal(err)
		}
	}
	msgBPath := filepath.Join(store.root, "storage", "message", "ses_b", "msg_b.json")
	if err := os.Chtimes(msgBPath, time.UnixMilli(cut-1000), time.UnixMilli(cut-1000)); err != nil {
		t.Fatal(err)
	}
	openCodeDirectMaxSessions = 1
	scanOpenCodeDirect(t)
	c := loadOpenCodeUsageLedger().DirectCursor
	if c == nil || !c.Continue {
		t.Fatalf("cursor = %+v, want continuation", c)
	}
	if c.AtSession {
		t.Fatalf("cursor.AtSession = true, want record continuation to revisit ses_a")
	}
	if c.RecordFloorMs == 0 || c.RecordFloorMs >= cut {
		t.Fatalf("cursor.RecordFloorMs = %d, want preserved record floor < %d", c.RecordFloorMs, cut)
	}
	store.write(openCodeTestMessage{
		session:     "ses_a",
		id:          "msg_a_incomplete",
		createdMs:   cut,
		completedMs: cut + 500,
		steps:       [][3]int64{{42, 0, 0}},
	})
	if err := os.Chtimes(filepath.Join(store.root, "storage", "message", "ses_a"), time.UnixMilli(cut), time.UnixMilli(cut)); err != nil {
		t.Fatal(err)
	}
	openCodeDirectMaxSessions = 8192
	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 52 {
		t.Fatalf("bucket tokens = %d, want 52 (42 + 10)", b.tokens())
	}
}


// A message counted past a cursor an incomplete message pinned behind it keeps
// its key in the cursor: a managed capture that rolls seenSteps over raises the
// rewind floor only to the pinned cursor, so the next scan reads the message
// again and must not count it twice.
func TestOpenCodeDirect_KeysPastAPinnedCursorSurviveASeenStepsRollover(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2052)
	running := openCodeTestMessage{session: "ses_a", id: "msg_run", createdMs: clock.ms(-30 * time.Minute), writtenMs: clock.ms(-30 * time.Minute), steps: [][3]int64{{60, 0, 0}}}
	store.write(running)
	store.write(openCodeTestMessage{session: "ses_b", id: "msg_after", createdMs: clock.ms(-10 * time.Minute), completedMs: clock.ms(-9 * time.Minute), steps: [][3]int64{{10, 0, 0}}})
	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 10 {
		t.Fatalf("bucket = %+v, want 10 tokens", b)
	}
	cursor := loadOpenCodeUsageLedger().DirectCursor
	if cursor == nil || cursor.ThroughMs > running.createdMs || len(cursor.PinnedKeys) != 1 {
		t.Fatalf("cursor = %+v, want it pinned at the running message with msg_after's key", cursor)
	}

	managedSteps := make([]openCodeUsageStep, openCodeUsageMaxSeenSteps+10)
	for i := range managedSteps {
		managedSteps[i] = openCodeUsageStep{Key: openCodeUsageStepKey("ses_managed", fmt.Sprint(i)), AtMs: clock.ms(0), Input: 1}
	}
	openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		return true, mergeOpenCodeUsageSteps(ledger, "managed_fp", managedSteps)
	})

	clock.now = clock.now.Add(time.Minute)
	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 10 {
		t.Fatalf("bucket = %+v, want 10 tokens (msg_after not counted twice)", b)
	}

	running.completedMs = clock.ms(-time.Second)
	running.writtenMs = 0
	store.write(running)
	clock.now = clock.now.Add(time.Minute)
	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 70 {
		t.Fatalf("bucket = %+v, want 70 tokens once the running message completes", b)
	}
	if c := loadOpenCodeUsageLedger().DirectCursor; c == nil || len(c.PinnedKeys) != 0 {
		t.Fatalf("cursor = %+v, want no pinned keys once the cursor passes them", c)
	}
}
