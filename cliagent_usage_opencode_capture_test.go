package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_opencode_capture_test.go — step_finish parsing, dedup, the
   local-day split and the ledger's bounds. Every case runs against its own
   ledger file and a pinned zone; nothing reaches a real `opencode`.
   ------------------------------------------------------------------------ */

// openCodeUsageTestScheduler records the ladder's bookings instead of arming
// real timers, so a case decides when an attempt runs.
type openCodeUsageTestScheduler struct {
	mu       sync.Mutex
	bookings []time.Duration
	pending  []func()
}

func (s *openCodeUsageTestScheduler) afterFunc(d time.Duration, fn func()) *time.Timer {
	s.mu.Lock()
	s.bookings = append(s.bookings, d)
	s.pending = append(s.pending, fn)
	s.mu.Unlock()
	// A stopped, never-firing timer: Stop on it is harmless.
	t := time.NewTimer(time.Hour)
	t.Stop()
	return t
}

// fireNext runs the oldest booked attempt; false when none is booked.
func (s *openCodeUsageTestScheduler) fireNext() bool {
	s.mu.Lock()
	if len(s.pending) == 0 {
		s.mu.Unlock()
		return false
	}
	fn := s.pending[0]
	s.pending = s.pending[1:]
	s.mu.Unlock()
	fn()
	return true
}

func (s *openCodeUsageTestScheduler) delays() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.bookings...)
}

// openCodeUsageFixture isolates the ledger, pins the epoch and the zone, and
// replaces the ladder's timers. It returns the scheduler.
func openCodeUsageFixture(t *testing.T, epoch int64) *openCodeUsageTestScheduler {
	t.Helper()
	// Background work an earlier case left running (a session's async settle)
	// reads the seams swapped below: let it finish first.
	resetOpenCodeUsageFreshness()
	withCodexGenerationEpoch(t, epoch)
	t.Setenv("AIEXPEDITE_OPENCODE_USAGE_CACHE", t.TempDir()+"/opencode_usage.json")
	sched := &openCodeUsageTestScheduler{}
	prevLoc, prevRotated := openCodeUsageLocation, openCodeGenerationRotated.Load()
	prevNow := openCodeUsageFreshnessNow
	prevAfter := swapOpenCodeUsageAfterFunc(sched.afterFunc)
	openCodeUsageLocation = time.UTC
	openCodeGenerationRotated.Store(false)
	resetOpenCodeReadinessCache()
	t.Cleanup(func() {
		resetOpenCodeUsageFreshness()
		swapOpenCodeUsageAfterFunc(prevAfter)
		openCodeUsageLocation, openCodeUsageFreshnessNow = prevLoc, prevNow
		openCodeGenerationRotated.Store(prevRotated)
		resetOpenCodeReadinessCache()
	})
	return sched
}

// swapOpenCodeUsageAfterFunc replaces the ladder's timer seam under the lock
// scheduleOpenCodeUsageAttempt reads it with, returning the previous one.
func swapOpenCodeUsageAfterFunc(fn func(time.Duration, func()) *time.Timer) func(time.Duration, func()) *time.Timer {
	openCodeUsageTimersMu.Lock()
	defer openCodeUsageTimersMu.Unlock()
	prev := openCodeUsageAfterFunc
	openCodeUsageAfterFunc = fn
	return prev
}

// openCodeStepFinish is a step_finish frame in the shape `opencode run
// --format json` emits.
func openCodeStepFinish(session, part string, input, output, reasoning int64, cost string, endMs int64) string {
	return fmt.Sprintf(`{"type":"step_finish","timestamp":%d,"sessionID":%q,"part":{"id":%q,"sessionID":%q,"messageID":"msg_1","type":"step-finish","reason":"stop","cost":%s,"tokens":{"input":%d,"output":%d,"reasoning":%d,"cache":{"read":100,"write":7}},"time":{"end":%d}}}`,
		endMs, session, part, session, cost, input, output, reasoning, endMs)
}

// openCodeZeroStepFinish is a well-formed step_finish that reports no spend at
// all — every token and the cost zero.
func openCodeZeroStepFinish(session, part string, endMs int64) string {
	return fmt.Sprintf(`{"type":"step_finish","timestamp":%d,"sessionID":%q,"part":{"id":%q,"sessionID":%q,"messageID":"msg_1","type":"step-finish","reason":"stop","cost":0,"tokens":{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}},"time":{"end":%d}}}`,
		endMs, session, part, session, endMs)
}

// A step that contributes nothing to a bucket is not payment: settlement and the
// export fallback must keep owing a reading for the turn that produced it.
func TestOpenCodeUsageStepsCarryUsage(t *testing.T) {
	zero, _, ok := parseOpenCodeUsageFrame(openCodeZeroStepFinish("ses_z", "prt_z", 1_790_000_000_000), time.Now())
	if !ok {
		t.Fatal("a zero-only step_finish is still a valid frame")
	}
	if openCodeUsageStepsCarryUsage(nil) || openCodeUsageStepsCarryUsage([]openCodeUsageStep{zero, zero}) {
		t.Fatal("zero-only steps were read as payment")
	}
	for name, step := range map[string]openCodeUsageStep{
		"input":      {Input: 1},
		"output":     {Output: 1},
		"reasoning":  {Reasoning: 1},
		"cacheRead":  {CacheRead: 1},
		"cacheWrite": {CacheWrite: 1},
		"cost":       {Cost: 0.0001},
	} {
		if !openCodeUsageStepsCarryUsage([]openCodeUsageStep{zero, step}) {
			t.Fatalf("a step carrying %s was not read as payment", name)
		}
	}
}

func TestParseOpenCodeUsageFrame_EverySpellingAndShape(t *testing.T) {
	at := time.UnixMilli(1_790_000_000_000)
	cases := []struct {
		name string
		line string
		want openCodeUsageStep
	}{
		{"step_finish nested", openCodeStepFinish("ses_a", "prt_1", 10, 20, 3, "0.5", 1_790_000_000_500),
			openCodeUsageStep{Input: 10, Output: 20, Reasoning: 3, CacheRead: 100, CacheWrite: 7, Cost: 0.5, AtMs: 1_790_000_000_500}},
		{"step-finish part only", `{"type":"message.part.updated","part":{"id":"prt_2","sessionID":"ses_a","type":"step-finish","tokens":{"input":1,"output":2}}}`,
			openCodeUsageStep{Input: 1, Output: 2, AtMs: at.UnixMilli()}},
		{"step.finish flat tokens", `{"type":"step.finish","sessionId":"ses_a","part":{"id":"prt_3","tokens":{"inputTokens":4,"output_tokens":5,"cache_read":6,"cacheWrite":2}}}`,
			openCodeUsageStep{Input: 4, Output: 5, CacheRead: 6, CacheWrite: 2, AtMs: at.UnixMilli()}},
		{"cost absent", `{"type":"step_finish","part":{"id":"prt_4","tokens":{"input":1}}}`,
			openCodeUsageStep{Input: 1, AtMs: at.UnixMilli()}},
		{"a tool-call step is still counted", `{"type":"step_finish","part":{"id":"prt_5","reason":"tool-calls","cost":0.01,"tokens":{"input":9,"output":1}}}`,
			openCodeUsageStep{Input: 9, Output: 1, Cost: 0.01, AtMs: at.UnixMilli()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			step, _, ok := parseOpenCodeUsageFrame(tc.line, at)
			if !ok {
				t.Fatalf("frame not read: %s", tc.line)
			}
			step.Key = ""
			if step != tc.want {
				t.Fatalf("step = %+v, want %+v", step, tc.want)
			}
		})
	}
}

func TestParseOpenCodeUsageFrame_RejectsAndIgnores(t *testing.T) {
	at := time.Now()
	for _, line := range []string{
		`{"type":"step_finish","part":{"id":"p","tokens":{"input":-1,"output":5}}}`,
		`{"type":"step_finish","part":{"id":"p","tokens":{"input":1},"cost":-0.2}}`,
		`{"type":"step_finish","part":{"id":"p","tokens":{"input":"12"}}}`,
		`{"type":"step_finish","part":{"id":"p","tokens":{"input":1e19,"output":5}}}`,          // past int64
		`{"type":"step_finish","part":{"id":"p","tokens":{"input":1,"cache":{"read":1e300}}}}`, // past int64, nested
		`{"type":"step_finish","part":{"id":"p","tokens":{"input":1},"cost":"free"}}`,
		`{"type":"step_finish","part":{"id":"p"}}`,               // no tokens
		`{"type":"text","part":{"id":"p","tokens":{"input":1}}}`, // not a step
		`{"type":"step_start","part":{"id":"p","tokens":{"input":1}}}`,
		`not json`,
	} {
		if step, _, ok := parseOpenCodeUsageFrame(line, at); ok {
			t.Errorf("frame %s was read as %+v, want rejected", line, step)
		}
	}
}

func TestCaptureOpenCodeUsage_DedupsByPartIDThenMessageAndEnd(t *testing.T) {
	openCodeUsageFixture(t, 901)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	frame := openCodeStepFinish("ses_a", "prt_1", 10, 20, 0, "0", now.UnixMilli())
	noPart := fmt.Sprintf(`{"type":"step_finish","sessionID":"ses_a","part":{"messageID":"msg_9","type":"step-finish","tokens":{"input":5},"time":{"end":%d}}}`, now.UnixMilli())
	noKey := fmt.Sprintf(`{"type":"step_finish","timestamp":%d,"part":{"type":"step-finish","tokens":{"input":1}}}`, now.UnixMilli())

	run := armOpenCodeUsageRun("opencode", "", "fp-a")
	for _, line := range []string{frame, frame, noPart, noPart, noKey} {
		captureOpenCodeUsageLine(run, line)
	}
	settleOpenCodeUsageRun(run, "")
	bucket, ok, _ := openCodeUsageBucketForDay("fp-a", now)
	// Within one run, every frame is a step; the duplicates collapse at commit.
	if !ok || bucket.InputTokens != 10+5+1 || bucket.OutputTokens != 20 {
		t.Fatalf("bucket = %+v, want the duplicate step and message counted once", bucket)
	}

	// The same steps seen again (a second capture of the same session) add nothing.
	again := armOpenCodeUsageRun("opencode", "", "fp-a")
	captureOpenCodeUsageLine(again, frame)
	captureOpenCodeUsageLine(again, noPart)
	settleOpenCodeUsageRun(again, "")
	if b, _, _ := openCodeUsageBucketForDay("fp-a", now); b.InputTokens != 16 {
		t.Fatalf("input = %d after a recapture, want 16", b.InputTokens)
	}
}

func TestCaptureOpenCodeUsage_ARunSpanningMidnightSplitsByStepTime(t *testing.T) {
	openCodeUsageFixture(t, 902)
	midnight := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	run := armOpenCodeUsageRun("opencode", "", "fp-a")
	captureOpenCodeUsageLine(run, openCodeStepFinish("ses_a", "prt_1", 100, 0, 0, "0", midnight.Add(-time.Minute).UnixMilli()))
	captureOpenCodeUsageLine(run, openCodeStepFinish("ses_a", "prt_2", 7, 0, 0, "0", midnight.Add(time.Minute).UnixMilli()))
	settleOpenCodeUsageRun(run, "")

	yesterday, _, _ := openCodeUsageBucketForDay("fp-a", midnight.Add(-time.Hour))
	today, _, _ := openCodeUsageBucketForDay("fp-a", midnight.Add(time.Hour))
	if yesterday.InputTokens != 100 || today.InputTokens != 7 {
		t.Fatalf("yesterday=%d today=%d, want 100 and 7", yesterday.InputTokens, today.InputTokens)
	}
}

func TestOpenCodeLocalDay_ResetFollowsTheLocalDateAcrossDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	prev := openCodeUsageLocation
	openCodeUsageLocation = ny
	t.Cleanup(func() { openCodeUsageLocation = prev })

	for _, tc := range []struct {
		day  time.Time
		want time.Duration
	}{
		{time.Date(2026, 3, 8, 0, 30, 0, 0, ny), 22*time.Hour + 30*time.Minute},  // spring forward: 23 h day
		{time.Date(2026, 11, 1, 0, 30, 0, 0, ny), 24*time.Hour + 30*time.Minute}, // fall back: 25 h day
		{time.Date(2026, 10, 6, 0, 30, 0, 0, ny), 23*time.Hour + 30*time.Minute},
	} {
		date, reset := openCodeLocalDay(tc.day)
		if date != tc.day.Format("2006-01-02") {
			t.Errorf("date = %s, want %s", date, tc.day.Format("2006-01-02"))
		}
		if got := reset.Sub(tc.day); got != tc.want {
			t.Errorf("%s: reset in %s, want %s", date, got, tc.want)
		}
		if h, m, _ := reset.In(ny).Clock(); h != 0 || m != 0 {
			t.Errorf("%s: reset %s is not local midnight", date, reset)
		}
	}
}

func TestMergeOpenCodeUsageSteps_BucketCapEvictsTheOldest(t *testing.T) {
	ledger := openCodeUsageLedger{SchemaVersion: openCodeUsageSchemaVersion}
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	prev := openCodeUsageLocation
	openCodeUsageLocation = time.UTC
	t.Cleanup(func() { openCodeUsageLocation = prev })
	for i := 0; i < openCodeUsageMaxBuckets+3; i++ {
		at := base.AddDate(0, 0, i).UnixMilli()
		mergeOpenCodeUsageSteps(&ledger, "fp", []openCodeUsageStep{{Key: fmt.Sprint(i), AtMs: at, Input: 1}})
	}
	if len(ledger.Buckets) != openCodeUsageMaxBuckets {
		t.Fatalf("buckets = %d, want %d", len(ledger.Buckets), openCodeUsageMaxBuckets)
	}
	for _, b := range ledger.Buckets {
		if b.LocalDate < "2026-01-04" {
			t.Fatalf("bucket %s survived the cap; the oldest must go first", b.LocalDate)
		}
	}
}

func TestMergeOpenCodeUsageSteps_SumsSaturateInsteadOfWrapping(t *testing.T) {
	ledger := openCodeUsageLedger{SchemaVersion: openCodeUsageSchemaVersion}
	at := time.Now().UnixMilli()
	// 1,024 steps at the per-frame bound sum to exactly 2^63: one past int64.
	steps := make([]openCodeUsageStep, 1024)
	for i := range steps {
		steps[i] = openCodeUsageStep{Key: fmt.Sprint(i), AtMs: at, Input: openCodeUsageMaxInt, Output: openCodeUsageMaxInt, Cost: math.MaxFloat64}
	}
	mergeOpenCodeUsageSteps(&ledger, "fp", steps)
	if len(ledger.Buckets) != 1 {
		t.Fatalf("buckets = %d, want 1", len(ledger.Buckets))
	}
	b := ledger.Buckets[0]
	if b.InputTokens != math.MaxInt64 || b.OutputTokens != math.MaxInt64 {
		t.Fatalf("input/output = %d/%d, want both saturated at MaxInt64", b.InputTokens, b.OutputTokens)
	}
	if b.tokens() != math.MaxInt64 {
		t.Fatalf("tokens() = %d, want saturated at MaxInt64", b.tokens())
	}
	if math.IsInf(b.CostUsd, 0) || b.CostUsd != math.MaxFloat64 {
		t.Fatalf("cost = %v, want saturated at MaxFloat64", b.CostUsd)
	}
	if _, err := json.Marshal(ledger); err != nil {
		t.Fatalf("a saturated ledger must still encode: %v", err)
	}
	if (openCodeUsageStep{Input: math.MaxInt64, Output: 1}).isZero() {
		t.Fatal("a step whose counts wrap to zero must still carry usage")
	}
}

func TestMergeOpenCodeUsageSteps_SeenStepsAreBounded(t *testing.T) {
	ledger := openCodeUsageLedger{SchemaVersion: openCodeUsageSchemaVersion}
	steps := make([]openCodeUsageStep, openCodeUsageMaxSeenSteps+50)
	for i := range steps {
		steps[i] = openCodeUsageStep{Key: openCodeUsageStepKey("s", fmt.Sprint(i)), AtMs: time.Now().UnixMilli(), Input: 1}
	}
	mergeOpenCodeUsageSteps(&ledger, "fp", steps)
	// A day of the direct reader's message keys fits: 8,192, pruned oldest first.
	if openCodeUsageMaxSeenSteps != 8192 || len(ledger.SeenSteps) != openCodeUsageMaxSeenSteps {
		t.Fatalf("seenSteps = %d, want the cap 8192", len(ledger.SeenSteps))
	}
	if ledger.SeenSteps[len(ledger.SeenSteps)-1] != steps[len(steps)-1].Key {
		t.Fatal("the newest key must survive the cap")
	}
	if ledger.SeenSteps[0] != steps[50].Key {
		t.Fatal("the cap must drop the oldest keys first")
	}
	for _, k := range ledger.SeenSteps {
		if _, err := hex.DecodeString(k); len(k) != 16 || err != nil {
			t.Fatalf("seen key %q is not a 16-hex hash", k)
		}
	}
}

func TestMergeOpenCodeUsageSteps_EvictingSeenStepsRaisesDirectRewindFloor(t *testing.T) {
	ledger := openCodeUsageLedger{
		SchemaVersion: openCodeUsageSchemaVersion,
		DirectCursor: &openCodeDirectCursor{
			ThroughMs: 123456,
		},
	}
	steps := make([]openCodeUsageStep, openCodeUsageMaxSeenSteps+10)
	for i := range steps {
		steps[i] = openCodeUsageStep{Key: openCodeUsageStepKey("s", fmt.Sprint(i)), AtMs: time.Now().UnixMilli(), Input: 1}
	}
	mergeOpenCodeUsageSteps(&ledger, "fp", steps)
	if !ledger.seenEvicted {
		t.Fatal("seenEvicted = false, want true when seenSteps rolls over")
	}
	if ledger.DirectCursor.RewindFloorMs != 123456 {
		t.Fatalf("rewindFloorMs = %d, want 123456", ledger.DirectCursor.RewindFloorMs)
	}
}

func TestOpenCodeUsageTransaction_GenerationAdvancesOnlyOnABucketChange(t *testing.T) {
	openCodeUsageFixture(t, 903)
	now := time.Now()
	run := armOpenCodeUsageRun("opencode", "", "fp-a")
	_, _, armed := openCodeUsageBucketForDay("fp-a", now)
	if armed.Counter != 0 {
		t.Fatalf("arming a run moved the generation to %+v", armed)
	}
	captureOpenCodeUsageLine(run, openCodeStepFinish("ses_a", "prt_1", 1, 1, 0, "0", now.UnixMilli()))
	settleOpenCodeUsageRun(run, "")
	_, _, first := openCodeUsageBucketForDay("fp-a", now)
	if first != (cliUsageGeneration{Epoch: 903, Counter: 1}) {
		t.Fatalf("generation = %+v, want {903,1}", first)
	}
	if !openCodeGenerationRotated.Load() {
		t.Fatal("a committed write on this epoch must complete the rotation")
	}
	// A recapture of the same step changes nothing and does not advance.
	again := armOpenCodeUsageRun("opencode", "", "fp-a")
	captureOpenCodeUsageLine(again, openCodeStepFinish("ses_a", "prt_1", 1, 1, 0, "0", now.UnixMilli()))
	settleOpenCodeUsageRun(again, "")
	if _, _, g := openCodeUsageBucketForDay("fp-a", now); g != first {
		t.Fatalf("generation moved to %+v on a no-op", g)
	}
}

// A ledger whose directory does not exist yet still writes under the
// cross-process lock: the directory is made before the sibling lock is opened,
// so a missing one cannot read as "no lock offered" and let two processes
// write unlocked.
func TestOpenCodeUsageTransaction_MissingDirectoryStillTakesTheLock(t *testing.T) {
	openCodeUsageFixture(t, 904)
	path := filepath.Join(t.TempDir(), "fresh", "nested", "opencode_usage.json")
	t.Setenv("AIEXPEDITE_OPENCODE_USAGE_CACHE", path)
	sawLock := false
	committed, _, _ := openCodeUsageTransaction(func(*openCodeUsageLedger) (bool, bool) {
		_, err := os.Stat(path + ".lock")
		sawLock = err == nil
		return true, false
	})
	if !committed {
		t.Fatal("the write into a missing directory did not commit")
	}
	if !sawLock {
		t.Fatal("the transaction ran without the cross-process lock file")
	}
}

// A run that has hit the step cap with every step committed recycles those
// slots instead of dropping each new step, so a long session keeps counting.
// While a commit is in flight its untake still needs the slots, so the step
// goes one past the cap instead.
func TestOpenCodeUsageRun_AFullyCommittedCapRecyclesItsSlots(t *testing.T) {
	now := time.Now().UnixMilli()
	full := func() *openCodeUsageRun {
		run := &openCodeUsageRun{sessionID: "ses_a"}
		run.steps = make([]openCodeUsageStep, openCodeUsageMaxRunSteps)
		run.committed = openCodeUsageMaxRunSteps
		return run
	}

	run := full()
	captureOpenCodeUsageLine(run, openCodeStepFinish("ses_a", "prt_late", 30, 5, 0, "0", now))
	if len(run.steps) != 1 || run.committed != 0 || run.steps[0].Input != 30 || run.steps[0].Output != 5 {
		t.Fatalf("steps = %d (committed %d), want the late step alone in a recycled run", len(run.steps), run.committed)
	}
	if taken, _, _ := run.takeUncommitted(); len(taken) != 1 {
		t.Fatalf("took %+v, want the late step offered to the ledger", taken)
	}

	busy := full()
	busy.commitMu.Lock()
	captureOpenCodeUsageLine(busy, openCodeStepFinish("ses_a", "prt_late", 30, 5, 0, "0", now))
	busy.commitMu.Unlock()
	if len(busy.steps) != openCodeUsageMaxRunSteps+1 || busy.committed != openCodeUsageMaxRunSteps {
		t.Fatalf("steps = %d (committed %d), want the step kept past the cap while a commit is in flight", len(busy.steps), busy.committed)
	}
	captureOpenCodeUsageLine(busy, openCodeStepFinish("ses_a", "prt_later", 1, 1, 0, "0", now))
	if len(busy.steps) != openCodeUsageMaxRunSteps+1 || busy.steps[len(busy.steps)-1].Input != 31 {
		t.Fatalf("a later step did not fold into the uncommitted one: %+v", busy.steps[len(busy.steps)-1])
	}
}

// The direct reader's fields are additive under schemaVersion 1: they
// round-trip, and a ledger written before them still loads its buckets.
func TestOpenCodeUsageLedger_AdditiveFieldsRoundTripUnderSchemaOne(t *testing.T) {
	openCodeUsageFixture(t, 1301)
	owned := []openCodeOwnedRun{{RunID: "run1", SessionKey: openCodeUsageSessionKey("ses_x"), FromMs: 10, ToMs: 20}, {RunID: "run2", SessionKey: "k", FromMs: 30}}
	cursor := &openCodeDirectCursor{
		Layout: openCodeStoreLayoutSQLite, ThroughMs: 40,
		PinnedKeys: []string{"k1", "k2"}, PinnedWrittenMs: []int64{41, 42}, PinnedCompletedMs: []int64{39, 40},
		PinnedDroppedMs: 30, PinnedDroppedDoneMs: 29,
	}
	coverage := &openCodeDirectCoverage{Layout: openCodeStoreLayoutSQLite, ObservedAtMs: 50, LastOkLocalDate: "2026-10-08"}
	if committed, _, _ := openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		ledger.OwnedRuns, ledger.DirectCursor, ledger.DirectCoverage = owned, cursor, coverage
		return true, false
	}); !committed {
		t.Fatal("write refused")
	}
	raw, err := os.ReadFile(openCodeUsageCachePath())
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]json.RawMessage
	if json.Unmarshal(raw, &onDisk) != nil || string(onDisk["schemaVersion"]) != "1" {
		t.Fatalf("schemaVersion = %s, want 1", onDisk["schemaVersion"])
	}
	got := loadOpenCodeUsageLedger()
	if len(got.OwnedRuns) != 2 || got.OwnedRuns[0] != owned[0] || got.OwnedRuns[1] != owned[1] || !reflect.DeepEqual(got.DirectCursor, cursor) || *got.DirectCoverage != *coverage {
		t.Fatalf("round trip = %+v", got)
	}

	// The shape an older build writes: no direct fields at all.
	older := `{"schemaVersion":1,"generation":{"epoch":7,"counter":3},"buckets":[{"accountFingerprint":"fp","localDate":"2026-10-08","inputTokens":5,"outputTokens":1,"reasoningTokens":0,"cacheReadTokens":0,"cacheWriteTokens":0,"costUsd":0.5,"observedAtMs":99}],"seenSteps":["00112233aabbccdd"]}`
	if err := os.WriteFile(openCodeUsageCachePath(), []byte(older), 0o600); err != nil {
		t.Fatal(err)
	}
	got = loadOpenCodeUsageLedger()
	if len(got.Buckets) != 1 || got.Buckets[0].tokens() != 6 || got.OwnedRuns != nil || got.DirectCursor != nil || got.DirectCoverage != nil {
		t.Fatalf("older ledger = %+v", got)
	}
}

// A ledger filled to every cap still loads: one past the read bound would read
// as EMPTY and drop today's buckets. The ledger has its own bound, not the
// small-config one shared with other readers.
func TestOpenCodeUsageLedger_AFullLedgerStillLoads(t *testing.T) {
	openCodeUsageFixture(t, 1302)
	committed, _, _ := openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		for i := 0; i < openCodeUsageMaxSeenSteps; i++ {
			ledger.SeenSteps = append(ledger.SeenSteps, openCodeUsageStepKey("s", fmt.Sprint(i)))
		}
		for i := 0; i < openCodeUsageMaxBuckets; i++ {
			ledger.Buckets = append(ledger.Buckets, openCodeUsageBucket{AccountFingerprint: "0123456789abcdef01234567", LocalDate: fmt.Sprintf("2026-10-%02d", i+1), InputTokens: math.MaxInt64, CostUsd: math.MaxFloat64, ObservedAtMs: int64(i + 1)})
		}
		for i := 0; i < openCodeUsageMaxDebts; i++ {
			ledger.Debts = append(ledger.Debts, openCodeUsageDebt{RunID: fmt.Sprintf("%016d", i), RunFloorMs: 1, SessionID: "ses_" + strings.Repeat("x", 120), Dir: strings.Repeat("d", 1024)})
		}
		for i := 0; i < openCodeUsageMaxOwnedRuns; i++ {
			ledger.OwnedRuns = append(ledger.OwnedRuns, openCodeOwnedRun{RunID: fmt.Sprintf("%016d", i), SessionKey: openCodeUsageSessionKey(fmt.Sprint(i)), FromMs: math.MaxInt32, ToMs: math.MaxInt32})
		}
		return true, false
	})
	if !committed {
		t.Fatal("write refused")
	}
	info, err := os.Stat(openCodeUsageCachePath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > openCodeUsageMaxLedgerBytes/2 {
		t.Fatalf("a full ledger is %d bytes: keep at least 2x headroom under %d", info.Size(), openCodeUsageMaxLedgerBytes)
	}
	if got := loadOpenCodeUsageLedger(); len(got.Buckets) != openCodeUsageMaxBuckets || len(got.SeenSteps) != openCodeUsageMaxSeenSteps {
		t.Fatalf("a full ledger (%d bytes) did not load: %d buckets, %d keys", info.Size(), len(got.Buckets), len(got.SeenSteps))
	}
}
