package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_opencode_v2_capture_test.go — every OpenCode 2 capture path
   moves the card: a smoke or terminal-managed run whose stream carries no
   usage is paid from the V2 store (never by an export, which pays nothing on
   V2), a direct run is counted by the reader, a debt pays only its own turn,
   store reads that cannot answer spend no attempt, and a Refresh click pays
   owed debts inside a bounded budget without starving any.
   ------------------------------------------------------------------------ */

// openCodeV2Fixture is the direct fixture over a V2 store, with the export
// seam counting calls and paying nothing — as on OpenCode 2.
func openCodeV2Fixture(t *testing.T, epoch int64) (*openCodeUsageTestScheduler, *openCodeV2TestStore, *openCodeDirectTestClock, *int) {
	t.Helper()
	sched, fixture, clock := openCodeDirectFixture(t, epoch)
	store := newOpenCodeV2TestStore(t, fixture.root)
	exports := stubOpenCodeExport(t, func(context.Context, string) ([]byte, bool) {
		return []byte(`{"info":{"id":"ses"},"messages":[]}`), true
	})
	return sched, store, clock, exports
}

// openCodeTodayTokens is "Tokens today" summed over every account's bucket.
func openCodeTodayTokens(t *testing.T, now time.Time) int64 {
	t.Helper()
	today, _ := openCodeLocalDay(now)
	var n int64
	for _, b := range loadOpenCodeUsageLedger().Buckets {
		if b.LocalDate == today {
			n += b.tokens()
		}
	}
	return n
}

// oweOpenCodeV2Run arms a run, names its session on the stream, lets the
// stream write the turn's rows to the store, and settles it with no usage in
// the stream: the run's debt is owed.
func oweOpenCodeV2Run(t *testing.T, store *openCodeV2TestStore, clock *openCodeDirectTestClock, session, msg string, input int64) *openCodeUsageRun {
	t.Helper()
	run := armOpenCodeUsageRun("opencode", "", "fp-v2")
	captureOpenCodeUsageLine(run, fmt.Sprintf(`{"type":"session.created","sessionID":%q}`, session))
	openCodeUsageInFlight.Wait()
	clock.now = clock.now.Add(time.Second)
	store.assistant(session, msg, clock.ms(-500*time.Millisecond), clock.ms(0), input, 0, 0, 0)
	store.idle(session, msg+"_idle", clock.ms(0))
	settleOpenCodeUsageRun(run, "")
	return run
}

func openCodeDebtOf(t *testing.T, runID string) *openCodeUsageDebt {
	t.Helper()
	ledger := loadOpenCodeUsageLedger()
	return openCodeUsageDebtByID(&ledger, runID)
}

// A passing smoke on OpenCode 2 whose stream reports no step_finish owes a
// debt; the first rung pays it from the V2 store, without the binary's export.
func TestOpenCodeV2_ASmokeWithNoUsageInTheStreamIsPaidFromTheStore(t *testing.T) {
	sched, store, clock, exports := openCodeV2Fixture(t, 4001)
	openCodeSmokeEnv(t)
	path := stubOpenCodeBinary(t)
	stubOpenCodeReadiness(t, "anthropic/claude-sonnet-4-5\n", true)
	stubOpenCodeSmokeExec(t, func(_ context.Context, launch openCodeLaunch) ([]byte, []byte, error) {
		// The child writes its turn to the store as it runs.
		clock.now = clock.now.Add(time.Second)
		store.assistant("ses_probe", "msg_smoke", clock.ms(-500*time.Millisecond), clock.ms(0), 1200, 34, 5, 0.0123)
		store.idle("ses_probe", "msg_smoke_idle", clock.ms(0))
		return openCodeSuccessFrames(openCodeMarkerFromLaunch(t, launch)), nil, nil
	})

	if result := runOpenCodeSmoke(context.Background(), path, "2.0.30"); result.Status != cliSmokeStatusSuccess {
		t.Fatalf("smoke = %+v, want success", result)
	}
	ledger := loadOpenCodeUsageLedger()
	if len(ledger.Debts) != 1 || !ledger.Debts[0].owed() {
		t.Fatalf("debts = %+v, want the smoke's owed debt", ledger.Debts)
	}
	if d := sched.delays(); len(d) == 0 || d[len(d)-1] != openCodeUsageDebtLadder[0] {
		t.Fatalf("booked %v, want the first rung", d)
	}
	sched.fireNext()
	if n := openCodeTodayTokens(t, clock.now); n != 1239 {
		t.Fatalf("tokens today = %d, want the stored 1239", n)
	}
	if left := loadOpenCodeUsageLedger().Debts; len(left) != 0 {
		t.Fatalf("debts = %+v after the store paid", left)
	}
	if *exports != 0 {
		t.Fatalf("the export ran %d times", *exports)
	}
}

// A stream that did carry step_finish usage commits at settle; the store later
// holds the same messages, but the debt is gone and nothing is counted twice —
// neither by a click's payment nor by the direct reader.
func TestOpenCodeV2_AStreamThatCarriedUsageIsNotPaidAgainFromTheStore(t *testing.T) {
	_, store, clock, _ := openCodeV2Fixture(t, 4002)
	run := armOpenCodeUsageRun("opencode", "", "fp-v2")
	clock.now = clock.now.Add(time.Second)
	captureOpenCodeUsageLine(run, openCodeStepFinish("ses_s", "prt_1", 300, 20, 0, "0.01", clock.ms(0)))
	openCodeUsageInFlight.Wait()
	store.assistant("ses_s", "msg_s", clock.ms(-500*time.Millisecond), clock.ms(0), 300, 20, 0, 0.01)
	store.idle("ses_s", "msg_s_idle", clock.ms(0))
	settleOpenCodeUsageRun(run, "")

	if paid := payOwedOpenCodeUsageFromStore(context.Background(), openCodeUsageMaxDebts); paid != 0 {
		t.Fatalf("a click paid %d debts for a settled stream", paid)
	}
	scanOpenCodeDirect(t)
	if n := openCodeTodayTokens(t, clock.now); n != 320 {
		t.Fatalf("tokens today = %d, want the stream's 320 once", n)
	}
}

// A terminal-managed `opencode run` settles on its terminal event while the
// child still runs: the store waits for the stream to close, then pays.
func TestOpenCodeV2_ATerminalManagedRunIsPaidFromTheStoreOnceItsStreamCloses(t *testing.T) {
	sched, store, clock, exports := openCodeV2Fixture(t, 4003)
	run := armOpenCodeUsageRun("opencode", "", "fp-v2")
	captureOpenCodeUsageLine(run, `{"type":"session.created","sessionID":"ses_term"}`)
	openCodeUsageInFlight.Wait()
	clock.now = clock.now.Add(time.Second)
	store.assistant("ses_term", "msg_t", clock.ms(-500*time.Millisecond), clock.ms(0), 80, 8, 0, 0)
	store.idle("ses_term", "msg_t_idle", clock.ms(0))
	settleOpenCodeUsageRunOnStream(run, "")

	sched.fireNext() // the run is live: the store stands down, spending nothing
	if d := openCodeDebtOf(t, run.id); d == nil || d.Attempts != 0 {
		t.Fatalf("debt = %+v, want it owed with no attempt spent", d)
	}
	if n := openCodeTodayTokens(t, clock.now); n != 0 {
		t.Fatalf("tokens today = %d while the run was live", n)
	}
	finishOpenCodeUsageRun(run)
	for sched.fireNext() {
	}
	if n := openCodeTodayTokens(t, clock.now); n != 88 {
		t.Fatalf("tokens today = %d, want the stored 88", n)
	}
	if *exports != 0 {
		t.Fatalf("the export ran %d times", *exports)
	}
}

// The TUI in a PTY or a shell run owes no debt: the tick counts it once the
// store's marker moves, and an unchanged marker skips the scan.
func TestOpenCodeV2_ADirectRunIsCountedByTheTickOnce(t *testing.T) {
	_, store, clock, _ := openCodeV2Fixture(t, 4004)
	prevDetected, prevSettle := openCodeDirectDetected, openCodeDirectSettleWindow
	openCodeDirectDetected = func() bool { return true }
	openCodeDirectSettleWindow = 0
	t.Cleanup(func() { openCodeDirectDetected, openCodeDirectSettleWindow = prevDetected, prevSettle })
	reads := countOpenCodeDirectReads(t, openCodeStoreLayoutSQLite)
	store.assistant("ses_tui", "msg_tui", clock.ms(-time.Hour), clock.ms(-time.Hour+time.Second), 64, 6, 0, 0.02)

	openCodeDirectScanIfChanged()
	if n := openCodeTodayTokens(t, clock.now); n != 70 {
		t.Fatalf("tokens today = %d, want the direct 70", n)
	}
	if c := loadOpenCodeUsageLedger().DirectCoverage; c == nil || c.Layout != openCodeStoreLayoutSQLiteV2 {
		t.Fatalf("coverage = %+v, want today's V2 coverage", c)
	}
	openCodeDirectScanIfChanged()
	if n := reads.Load(); n != 1 {
		t.Fatalf("the store was read %d times, want the unchanged marker to skip", n)
	}
	if n := openCodeTodayTokens(t, clock.now); n != 70 {
		t.Fatalf("tokens today = %d after the second tick", n)
	}
}

// A native-chat debt on a resumed session pays only its own turn — inside its
// window, and, once that window is gone, up to the session's first idle row
// after its floor.
func TestOpenCodeV2_ADebtPaysOnlyItsOwnTurnOfAResumedSession(t *testing.T) {
	for _, pruned := range []bool{false, true} {
		t.Run(fmt.Sprintf("window pruned %v", pruned), func(t *testing.T) {
			sched, store, clock, _ := openCodeV2Fixture(t, 4005)
			run := oweOpenCodeV2Run(t, store, clock, "ses_r", "msg_turn1", 40)
			// The user resumes the session directly, before the rung fires.
			clock.now = clock.now.Add(5 * time.Second)
			store.assistant("ses_r", "msg_turn2", clock.ms(-time.Second), clock.ms(0), 900, 0, 0, 0)
			store.idle("ses_r", "msg_turn2_idle", clock.ms(0))
			if pruned {
				// The window was pruned past the cap and the debt adopted from a
				// build that recorded no settle time.
				openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
					ledger.OwnedRuns = nil
					openCodeUsageDebtByID(ledger, run.id).SettledAtMs = 0
					return true, false
				})
			}
			sched.fireNext()
			ledger := loadOpenCodeUsageLedger()
			if len(ledger.Debts) != 0 {
				t.Fatalf("debts = %+v, want the store to have paid", ledger.Debts)
			}
			b, _, _ := openCodeUsageBucketForDay("fp-v2", clock.now)
			if b.tokens() != 40 {
				t.Fatalf("bucket = %+v, want the debt's own 40 only", b)
			}
		})
	}
}

// Store reads that cannot answer defer for free; only a whole, empty V2 read
// spends an attempt, and three of those retire the debt — with no export.
func TestOpenCodeV2_OnlyAWholeEmptyStoreReadSpendsAnAttempt(t *testing.T) {
	sched, store, clock, exports := openCodeV2Fixture(t, 4006)
	run := armOpenCodeUsageRun("opencode", "", "fp-v2")
	captureOpenCodeUsageLine(run, `{"type":"session.created","sessionID":"ses_q"}`)
	openCodeUsageInFlight.Wait()
	clock.now = clock.now.Add(time.Second)
	store.assistant("ses_q", "msg_running", clock.ms(-500*time.Millisecond), 0, 10, 0, 0, 0)
	settleOpenCodeUsageRun(run, "")

	sched.fireNext() // incomplete: the turn is still being written
	if d := openCodeDebtOf(t, run.id); d == nil || d.Attempts != 0 {
		t.Fatalf("debt = %+v, want no attempt spent on an incomplete read", d)
	}
	openCodeDirectMaxRecords = 0
	sched.fireNext() // truncated by the record cap
	if d := openCodeDebtOf(t, run.id); d == nil || d.Attempts != 0 {
		t.Fatalf("debt = %+v, want no attempt spent on a truncated read", d)
	}
	openCodeDirectMaxRecords = 8192

	// The running row turns out to be no assistant message after all: the
	// turn holds nothing.
	if _, err := store.db.Exec(`UPDATE session_message SET type = 'synthetic' WHERE id = 'msg_running'`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= openCodeUsageDebtMaxAttempts; i++ {
		if !sched.fireNext() {
			t.Fatalf("no attempt booked before empty read %d", i)
		}
		if i < openCodeUsageDebtMaxAttempts {
			if d := openCodeDebtOf(t, run.id); d == nil || d.Attempts != i {
				t.Fatalf("after empty read %d debt = %+v", i, d)
			}
		}
	}
	if d := openCodeDebtOf(t, run.id); d != nil {
		t.Fatalf("debt = %+v, want it retired as exhausted", d)
	}
	if *exports != 0 {
		t.Fatalf("the export ran %d times on a V2 store", *exports)
	}
}

func TestOpenCodeStoreDebtOutcome_MapsEveryRead(t *testing.T) {
	v2 := openCodeStoreLayoutSQLiteV2
	paying := []openCodeUsageStep{{Input: 1}}
	for _, tc := range []struct {
		store openCodeStoreSessionRead
		err   error
		want  string
	}{
		{openCodeStoreSessionRead{}, nil, ""},
		{openCodeStoreSessionRead{Layout: openCodeStoreLayoutSQLite}, nil, ""},
		{openCodeStoreSessionRead{Layout: openCodeStoreLayoutSQLite, Steps: paying}, nil, "store_paid"},
		{openCodeStoreSessionRead{}, errOpenCodeDirectLayoutUnknown, ""},
		{openCodeStoreSessionRead{}, context.DeadlineExceeded, "store_deferred_truncated"},
		{openCodeStoreSessionRead{}, fmt.Errorf("database is locked"), "store_deferred_busy"},
		{openCodeStoreSessionRead{Layout: v2, SessionKnown: true, Steps: paying, Incomplete: true}, nil, "store_paid"},
		{openCodeStoreSessionRead{Layout: v2, SessionKnown: true, Unbounded: true}, nil, "store_deferred_unbounded"},
		{openCodeStoreSessionRead{Layout: v2, SessionKnown: true, Truncated: true}, nil, "store_deferred_truncated"},
		{openCodeStoreSessionRead{Layout: v2, SessionKnown: true, Incomplete: true}, nil, "store_deferred_incomplete"},
		{openCodeStoreSessionRead{Layout: v2, SessionKnown: true}, nil, "store_empty"},
		{openCodeStoreSessionRead{Layout: v2}, nil, ""}, // a session V2 never wrote
	} {
		if got := openCodeStoreDebtOutcome(tc.store, tc.err); got != tc.want {
			t.Errorf("outcome(%+v, %v) = %q, want %q", tc.store, tc.err, got, tc.want)
		}
	}
}

// A debt owed before the upgrade names a session only the V1 tables hold: the
// V2 store cannot speak for it, so it spends no attempt and the export pays.
func TestOpenCodeV2_ADebtFromBeforeTheUpgradeIsLeftToTheExport(t *testing.T) {
	sched, store, clock, _ := openCodeV2Fixture(t, 4010)
	stubOpenCodeExecutable(t)
	var paidAt time.Time
	exports := stubOpenCodeExport(t, func(_ context.Context, sessionID string) ([]byte, bool) {
		return openCodeExportJSON("msg_v1era", paidAt.UnixMilli(), 21, 0, 0), true
	})
	store.assistant("ses_other", "msg_other", clock.ms(-time.Hour), clock.ms(-time.Hour+time.Second), 1, 0, 0, 0)
	run := armOpenCodeUsageRun("opencode", "", "fp-v2")
	captureOpenCodeUsageLine(run, `{"type":"session.created","sessionID":"ses_v1era"}`)
	openCodeUsageInFlight.Wait()
	clock.now = clock.now.Add(time.Second)
	paidAt = clock.now.Add(-500 * time.Millisecond)
	settleOpenCodeUsageRun(run, "")

	sched.fireNext()
	if *exports != 1 {
		t.Fatalf("the export ran %d times, want once", *exports)
	}
	if b, _, _ := openCodeUsageBucketForDay("fp-v2", clock.now); b.tokens() != 21 {
		t.Fatalf("bucket = %+v, want the exported 21", b)
	}
	if d := openCodeDebtOf(t, run.id); d != nil {
		t.Fatalf("debt = %+v after the export paid", d)
	}
}

// A Refresh payment and a ladder attempt racing on one debt merge it once.
func TestOpenCodeV2_AClickAndTheLadderRacingOnADebtMergeItOnce(t *testing.T) {
	sched, store, clock, _ := openCodeV2Fixture(t, 4007)
	run := oweOpenCodeV2Run(t, store, clock, "ses_race", "msg_race", 55)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); payOwedOpenCodeUsageFromStore(context.Background(), openCodeUsageMaxDebts) }()
	go func() { defer wg.Done(); sched.fireNext() }()
	wg.Wait()
	if n := openCodeTodayTokens(t, clock.now); n != 55 {
		t.Fatalf("tokens today = %d, want 55 once", n)
	}
	if d := openCodeDebtOf(t, run.id); d != nil {
		t.Fatalf("debt = %+v after the race", d)
	}
	openCodeUsageTimersMu.Lock()
	_, booked := openCodeUsageTimers[run.id]
	openCodeUsageTimersMu.Unlock()
	if booked {
		t.Fatal("the paid debt kept a booked attempt")
	}
}

// openCodeClickBudgetCtx reports plenty of budget for its first n Deadline
// calls and none after: the click's reserve guard trips after n debts.
type openCodeClickBudgetCtx struct {
	context.Context
	left atomic.Int64
}

func (c *openCodeClickBudgetCtx) Deadline() (time.Time, bool) {
	if c.left.Add(-1) >= 0 {
		return time.Now().Add(time.Hour), true
	}
	return time.Now(), true
}

func newOpenCodeClickBudgetCtx(n int) *openCodeClickBudgetCtx {
	c := &openCodeClickBudgetCtx{Context: context.Background()}
	c.left.Store(int64(n))
	return c
}

// A click pays at most what its budget reaches, leaves the rest on their ladder
// bookings, and the next click starts with the debts this one never tried — so
// the tail is not starved behind debts the store cannot pay yet. A live run is
// skipped and spends nothing.
func TestOpenCodeV2_ABoundedRefreshDoesNotStarveTheTail(t *testing.T) {
	_, store, clock, _ := openCodeV2Fixture(t, 4008)
	var runs []*openCodeUsageRun
	for i := 0; i < openCodeUsageMaxDebts; i++ {
		session := fmt.Sprintf("ses_%d", i)
		if i < 3 {
			// The oldest three: their turns are still being written.
			run := armOpenCodeUsageRun("opencode", "", "fp-v2")
			captureOpenCodeUsageLine(run, fmt.Sprintf(`{"type":"session.created","sessionID":%q}`, session))
			openCodeUsageInFlight.Wait()
			clock.now = clock.now.Add(time.Second)
			store.assistant(session, "msg_"+session, clock.ms(-500*time.Millisecond), 0, 1000, 0, 0, 0)
			settleOpenCodeUsageRun(run, "")
			runs = append(runs, run)
			continue
		}
		runs = append(runs, oweOpenCodeV2Run(t, store, clock, session, "msg_"+session, 1))
	}
	if n := len(loadOpenCodeUsageLedger().Debts); n != openCodeUsageMaxDebts {
		t.Fatalf("%d debts owed, want %d", n, openCodeUsageMaxDebts)
	}

	if paid := payOwedOpenCodeUsageFromStore(newOpenCodeClickBudgetCtx(3), openCodeUsageMaxDebts); paid != 0 {
		t.Fatalf("first click paid %d, want the three oldest tried and unpayable", paid)
	}
	openCodeUsageTimersMu.Lock()
	booked := len(openCodeUsageTimers)
	openCodeUsageTimersMu.Unlock()
	if booked != openCodeUsageMaxDebts {
		t.Fatalf("%d ladder bookings left, want every debt's kept", booked)
	}
	if paid := payOwedOpenCodeUsageFromStore(newOpenCodeClickBudgetCtx(3), openCodeUsageMaxDebts); paid != 3 {
		t.Fatalf("second click paid %d, want three of the untried tail", paid)
	}

	// A live run is skipped: nothing paid, no attempt spent.
	live := runs[7]
	openCodeUsageLiveMu.Lock()
	openCodeUsageLiveRuns[live.id] = live
	openCodeUsageLiveMu.Unlock()
	if paid := payOwedOpenCodeUsageFromStore(context.Background(), openCodeUsageMaxDebts); paid != 1 {
		t.Fatalf("third click paid %d, want only the debt not live", paid)
	}
	if d := openCodeDebtOf(t, live.id); d == nil || d.Attempts != 0 {
		t.Fatalf("live debt = %+v, want it owed with no attempt spent", d)
	}
	for _, r := range runs[:3] {
		if d := openCodeDebtOf(t, r.id); d == nil || d.Attempts != 0 {
			t.Fatalf("unpayable debt = %+v, want no attempt spent by a click", d)
		}
	}
	if n := openCodeTodayTokens(t, clock.now); n != 4 {
		t.Fatalf("tokens today = %d, want the four paid debts", n)
	}
}

// A V2 store with a column set the reader does not know fails closed: no
// coverage is booked and the card keeps "(agent runs)".
func TestOpenCodeV2_AnUnknownColumnSetFailsClosed(t *testing.T) {
	_, fixture, clock := openCodeDirectFixture(t, 4009)
	db := openCodeSQLiteTestStore(t, fixture.root, `
CREATE TABLE session_v2 (id text PRIMARY KEY, time_updated integer NOT NULL);
CREATE TABLE session_message (id text PRIMARY KEY, session_id text NOT NULL, kind text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);`)
	if _, err := db.Exec(`INSERT INTO session_v2 VALUES ('ses_a', ?)`, clock.ms(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if label := scanOpenCodeDirect(t); label != "direct_layout_unknown" {
		t.Fatalf("scan = %q", label)
	}
	if c := loadOpenCodeUsageLedger().DirectCoverage; c != nil {
		t.Fatalf("coverage = %+v for an unknown store", c)
	}
	raw, _ := json.Marshal(openCodeUsageMetrics(openCodeUsageBucket{InputTokens: 1, ObservedAtMs: clock.ms(0)}, true, openCodeUsageDayFor("", clock.now).Direct, clock.now))
	if !strings.Contains(string(raw), "Tokens today (agent runs)") {
		t.Fatalf("label = %s", raw)
	}
}
