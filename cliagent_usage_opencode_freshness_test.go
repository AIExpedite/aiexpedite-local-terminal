// cliagent_usage_opencode_freshness_test.go — the run-completion debt, the
// continuation chain, the discovery nudge and restart survival.
//
// The ladder and the nudge cooldown are pinned to milliseconds and the CLI is
// stubbed, so nothing here waits on a real clock or a real `opencode`.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// openCodeDebtFixture is openCodeUsageFixture with the schedule pinned small.
func openCodeDebtFixture(t *testing.T, at time.Time) func(time.Time) {
	t.Helper()
	setNow := openCodeUsageFixture(t, at)
	prevLadder := openCodeRunDebtRetryLadder
	prevFree, prevSpacing, prevNudge := openCodeRunDebtFreeRetryDelay,
		openCodeReconcileMinInterval, openCodeReconcileNudgeCooldown
	openCodeRunDebtRetryLadder = []time.Duration{
		5 * time.Millisecond, 10 * time.Millisecond, 15 * time.Millisecond, 20 * time.Millisecond,
	}
	openCodeRunDebtFreeRetryDelay = 2 * time.Millisecond
	openCodeReconcileMinInterval = 0
	openCodeReconcileNudgeCooldown = 50 * time.Millisecond
	t.Cleanup(func() {
		// Drain BEFORE restoring: cleanups run LIFO, so the shared fixture's own
		// drain runs after this one and a worker still on the pinned ladder
		// would read these vars as they were restored (a -race failure).
		openCodeUsageRefreshEnabled.Store(false)
		stopOpenCodeRunDebtRetry()
		openCodeUsageRefreshWaitFor(5 * time.Second)
		openCodeRunDebtRetryLadder = prevLadder
		openCodeRunDebtFreeRetryDelay, openCodeReconcileMinInterval, openCodeReconcileNudgeCooldown =
			prevFree, prevSpacing, prevNudge
	})
	return setNow
}

// forceOpenCodeOffline flips offline mode for one row and returns the restore.
// SetOffline without a config touches no file.
func forceOpenCodeOffline(t *testing.T) func() {
	t.Helper()
	was := IsOffline()
	SetOffline(true)
	restored := false
	restore := func() {
		if !restored {
			restored = true
			SetOffline(was)
		}
	}
	t.Cleanup(restore)
	return restore
}

func readOpenCodeFreshness(t *testing.T) openCodeUsageFreshness {
	t.Helper()
	var state openCodeUsageFreshness
	readJSONFile(openCodeUsageFreshnessPath(), &state)
	return state
}

/* ───────────────────────────── arm and settle ──────────────────────────── */

func TestOpenCodeDebt_ACoveredRunOwesNothingAndAnUncoveredOneOwesOne(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeDebtFixture(t, now)
	// Nothing to reconcile with, so the worker's pass is a launch_error that
	// spends one attempt — which is what the ladder rows below assert on.
	(&openCodeCLIStub{sessions: "[]"}).install(t)

	covered := armOpenCodeUsageRun("covered")
	covered.Observe(`{"type":"step_finish","sessionID":"s","part":{"messageID":"m","type":"step-finish","tokens":{"input":5}}}`)
	if covered.Finish(true) {
		t.Fatal("a clean stream with tokens must owe nothing")
	}
	openCodeUsageRefreshWaitFor(2 * time.Second)
	if state := readOpenCodeFreshness(t); state.owed() {
		t.Fatalf("state = %+v, want no debt", state)
	}

	for _, tc := range []struct {
		name     string
		cleanEnd bool
		tokens   bool
	}{
		{"a killed or timed-out stream", false, true},
		{"a stream that reported no tokens", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handle := armOpenCodeUsageRun("unit")
			if tc.tokens {
				handle.Observe(`{"type":"step_finish","sessionID":"s2","part":{"messageID":"m2","type":"step-finish","tokens":{"input":5}}}`)
			}
			if !handle.Finish(tc.cleanEnd) {
				t.Fatal("expected one owed reconcile")
			}
		})
	}
}

func TestOpenCodeDebt_AnExecuteRunAlwaysOwesAndACleanSmokeDoesNotPayIt(t *testing.T) {
	// Execute and PTY runs are never tapped, so their usage can only come from
	// a reconcile. A clean smoke afterwards is COVERED — and a covered settle
	// must never retire somebody else's debt, or the execute run's tokens would
	// go uncounted.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeDebtFixture(t, now)
	(&openCodeCLIStub{launchError: true}).install(t)

	execute := armOpenCodeUsageForCommand("local execute", "opencode", []string{"run", "-m", "x"})
	if execute == nil {
		t.Fatal("an execute run must arm")
	}
	if !execute.Finish(false) {
		t.Fatal("an execute run always owes a reconcile")
	}
	owedAt := readOpenCodeFreshness(t).OwedAtMs
	if owedAt == 0 {
		t.Fatal("no debt was written")
	}

	smoke := armOpenCodeUsageRun("smoke")
	smoke.Observe(`{"type":"step_finish","sessionID":"s","part":{"messageID":"m","type":"step-finish","tokens":{"input":5}}}`)
	if smoke.Finish(true) {
		t.Fatal("the smoke itself owes nothing")
	}
	openCodeUsageRefreshWaitFor(2 * time.Second)
	if state := readOpenCodeFreshness(t); !state.owed() || state.OwedAtMs != owedAt {
		t.Fatalf("state = %+v, want the execute run's debt still open (owedAt %d)", state, owedAt)
	}
}

func TestOpenCodeDebt_ANewerOwingRunKeepsTheAttemptCount(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeDebtFixture(t, now)

	state := openCodeUsageFreshness{}
	openCodeOweReconcile(&state, now, openCodeCompletionMs(now))
	state.Attempts = 2
	first := state.OwedAtMs

	later := now.Add(time.Minute)
	openCodeOweReconcile(&state, later, openCodeCompletionMs(later))
	if state.OwedAtMs != first {
		t.Fatalf("owedAt moved: %d -> %d; one pending debt at a time", first, state.OwedAtMs)
	}
	if state.Attempts != 2 {
		t.Fatalf("attempts = %d, want the count to survive a newer run", state.Attempts)
	}
	if state.CompletionMs != openCodeCompletionMs(later) {
		t.Fatalf("completion = %d, want the newer run's %d", state.CompletionMs, openCodeCompletionMs(later))
	}
}

func TestOpenCodeDebt_RetiresPastItsMaximumAge(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	setNow := openCodeDebtFixture(t, now)
	updateOpenCodeUsageFreshness(func(s *openCodeUsageFreshness) {
		openCodeOweReconcile(s, now, openCodeCompletionMs(now))
	})
	setNow(now.Add(openCodeRunDebtMaxAge + time.Minute))
	if _, owed := openCodePendingDebt(openCodeUsageNow()); owed {
		t.Fatal("a debt nothing could pay must retire rather than pin the schedule forever")
	}
	// What it never reconciled is uncounted, so the day is a lower bound.
	if day := readOpenCodeUsageLedger().Days[openCodeDayKey(openCodeUsageNow())]; day == nil || !day.Partial {
		t.Fatalf("day = %+v, want the lower-bound notice", day)
	}
}

func TestOpenCodeDebt_SpendsAtMostItsBudgetOfPasses(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeDebtFixture(t, now)
	stub := (&openCodeCLIStub{launchError: true}).install(t)

	handle := armOpenCodeUsageRun("unit")
	handle.Finish(false)

	// The settle's own pass plus every rung, and no more.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if readOpenCodeFreshness(t).Attempts >= openCodeRunDebtMaxAttempts {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	openCodeUsageRefreshWaitFor(2 * time.Second)
	if attempts := readOpenCodeFreshness(t).Attempts; attempts != openCodeRunDebtMaxAttempts {
		t.Fatalf("attempts = %d, want exactly %d", attempts, openCodeRunDebtMaxAttempts)
	}
	time.Sleep(60 * time.Millisecond)
	openCodeUsageRefreshWaitFor(2 * time.Second)
	if calls := len(stub.recorded()); calls > openCodeRunDebtMaxAttempts {
		t.Fatalf("the CLI ran %d times, want at most %d", calls, openCodeRunDebtMaxAttempts)
	}
	if openCodeRunDebtRetryPending() {
		t.Fatal("a spent budget must book nothing more")
	}
}

func TestOpenCodeDebt_FreeRetriesBackOffWithTheDebtsAge(t *testing.T) {
	// A device that stays offline must not re-check at the floor for the
	// debt's whole six-hour window.
	floor := 30 * time.Second
	ladder := []time.Duration{time.Minute, 2 * time.Minute, 8 * time.Minute, 30 * time.Minute}
	if got := refreshFreeRetryDelay(0, floor, ladder); got != floor {
		t.Fatalf("a fresh debt = %v, want the floor %v", got, floor)
	}
	if got := refreshFreeRetryDelay(5*time.Minute, floor, ladder); got != 5*time.Minute {
		t.Fatalf("a five-minute-old debt = %v, want its own age", got)
	}
	if got := refreshFreeRetryDelay(5*time.Hour, floor, ladder); got != 30*time.Minute {
		t.Fatalf("an old debt = %v, want the ladder's longest rung", got)
	}
}

func TestOpenCodeDebt_OfflineTakesTheFreeRetryAndSpendsNoBudget(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeDebtFixture(t, now)
	(&openCodeCLIStub{sessions: "[]"}).install(t)
	restore := forceOpenCodeOffline(t)

	updateOpenCodeUsageFreshness(func(s *openCodeUsageFreshness) {
		openCodeOweReconcile(s, now, openCodeCompletionMs(now))
	})
	openCodePayReconcile(context.Background(), false)
	state := readOpenCodeFreshness(t)
	if state.Attempts != 0 {
		t.Fatalf("attempts = %d, want offline to spend no budget", state.Attempts)
	}
	if !state.owed() || state.NextAttemptAtMs == 0 {
		t.Fatalf("state = %+v, want the debt open with a booked free retry", state)
	}
	// With a debt open the ladder is the ONLY timer: no continuation is booked.
	if readOpenCodeUsageLedger().ContinuationDue {
		t.Fatal("a debt-open outcome must leave no continuation booked")
	}
	restore()
}

/* ──────────────────────── the continuation chain ───────────────────────── */

func TestOpenCodeContinuation_DrainsABacklogWithNoDebtOpen(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeDebtFixture(t, now)
	created := now.UnixMilli()
	exports := map[string]string{}
	rows := make([]openCodeSessionRow, 0, 5)
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("ses_%d", i)
		rows = append(rows, openCodeSessionRow{id, created + int64(i+1)*1000})
		exports[id] = exportWith(id, fmt.Sprintf("msg_%d", i), created, 10, 0, "0")
	}
	(&openCodeCLIStub{sessions: sessionList(rows...), exports: exports}).install(t)

	// ONE pass first, so the intermediate state is observable: five changed
	// sessions, three exports a pass.
	updateOpenCodeUsageLedgerContinuation(true, 0, 0)
	if result := reconcileOpenCodeUsageOnce(context.Background(), now); result.Outcome != openCodeReconcileMore {
		t.Fatalf("first pass = %q, want more", result.Outcome)
	}
	// 0 is never published while today's sessions are queued, and the three
	// that DID commit are.
	if metrics, _, _ := openCodeLedgerMetrics(now); len(metrics) != 1 || *metrics[0].Consumed != 30 {
		t.Fatalf("metrics = %+v, want only the three committed sessions", metrics)
	}

	// A nudge books the continuation without owing a debt, which is how a
	// direct `opencode` run in the user's own shell is reconciled.
	if !nudgeOpenCodeUsageRefresh(now) {
		t.Fatal("the nudge did not start the worker")
	}
	if readOpenCodeFreshness(t).owed() {
		t.Fatal("a nudge must not open a debt")
	}

	// The booked continuation then drains the rest on its own, with no new
	// filesystem write.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if openCodeReconcileSucceeded(readOpenCodeUsageLedger().LastPassOutcome) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	openCodeUsageRefreshWaitFor(2 * time.Second)
	ledger := readOpenCodeUsageLedger()
	if !openCodeReconcileSucceeded(ledger.LastPassOutcome) || ledger.ContinuationDue {
		t.Fatalf("ledger = outcome %q continuationDue %v, want the chain finished",
			ledger.LastPassOutcome, ledger.ContinuationDue)
	}
	if tokens, _, rows := todayTotals(t, now); tokens != 50 || rows != 5 {
		t.Fatalf("tokens=%d rows=%d, want all five sessions", tokens, rows)
	}
}

func TestOpenCodeContinuation_StopsAfterItsFailureBudgetAndMarksTheDayPartial(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeDebtFixture(t, now)
	// A list we can read, an export that always fails to launch: candidates
	// remain and every pass fails.
	(&openCodeCLIStub{
		sessions:    sessionList(openCodeSessionRow{"ses_a", now.UnixMilli()}),
		launchError: true,
	}).install(t)

	updateOpenCodeUsageLedgerContinuation(true, 0, 0)
	for i := 0; i < openCodeContinuationMaxFailures+2; i++ {
		openCodePayReconcile(context.Background(), false)
	}
	ledger := readOpenCodeUsageLedger()
	if ledger.ContinuationDue {
		t.Fatalf("continuationDue is still set after %d failures", openCodeContinuationMaxFailures)
	}
	if ledger.ContinuationFailures < openCodeContinuationMaxFailures {
		// The chain cleared its own counter when it gave up; what matters is
		// that it stopped and said the day is a lower bound.
		t.Logf("continuationFailures = %d", ledger.ContinuationFailures)
	}
	if day := ledger.Days[openCodeDayKey(now)]; day == nil || !day.Partial {
		t.Fatalf("day = %+v, want the lower-bound notice", day)
	}
	// And no zero row is published off a chain that gave up.
	if metrics, _, partial := openCodeLedgerMetrics(now); len(metrics) != 0 || !partial {
		t.Fatalf("metrics = %+v partial = %v, want no row and a notice", metrics, partial)
	}
}

func TestOpenCodeContinuation_UnsupportedBooksNothingAndIsNotReArmed(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeDebtFixture(t, now)
	(&openCodeCLIStub{}).install(t) // the human-table session list

	updateOpenCodeUsageLedgerContinuation(true, 0, 0)
	openCodePayReconcile(context.Background(), false)
	if readOpenCodeUsageLedger().ContinuationDue {
		t.Fatal("unsupported output can never be fixed by a retry")
	}
	if openCodeRunDebtRetryPending() {
		t.Fatal("unsupported must book no pass")
	}
	// A simulated restart books none either.
	payOwedOpenCodeUsageRefresh()
	openCodeUsageRefreshWaitFor(2 * time.Second)
	if openCodeRunDebtRetryPending() {
		t.Fatal("a restart must not re-arm an unsupported install")
	}
}

func TestOpenCodeDebt_UnsupportedRetiresTheDebtSoStreamFiguresStand(t *testing.T) {
	now := openCodeFixtureRunAt
	openCodeDebtFixture(t, now)
	(&openCodeCLIStub{}).install(t)

	handle := armOpenCodeUsageRun("unit")
	observeFixture(t, handle, "run_two_steps.jsonl")
	handle.Finish(false) // a cut-off stream: it owes
	openCodeUsageRefreshWaitFor(2 * time.Second)

	if state := readOpenCodeFreshness(t); state.owed() {
		t.Fatalf("state = %+v, want the debt retired by `unsupported`", state)
	}
	// The figures the stream DID see still stand.
	if tokens, _, _ := todayTotals(t, now); tokens != 63 {
		t.Fatalf("tokens = %d, want the stream's 63", tokens)
	}
}

/* ──────────────────────────────── the nudge ────────────────────────────── */

func TestOpenCodeNudge_HonoursItsCooldownAndLiveRuns(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeDebtFixture(t, now)
	(&openCodeCLIStub{sessions: "[]"}).install(t)

	if !nudgeOpenCodeUsageRefresh(now) {
		t.Fatal("the first nudge must start the worker")
	}
	if nudgeOpenCodeUsageRefresh(now.Add(time.Millisecond)) {
		t.Fatal("a second nudge inside the cooldown must be refused")
	}
	openCodeUsageRefreshWaitFor(2 * time.Second)
	if !nudgeOpenCodeUsageRefresh(now.Add(openCodeReconcileNudgeCooldown + time.Millisecond)) {
		t.Fatal("a nudge past the cooldown must be accepted")
	}
	openCodeUsageRefreshWaitFor(2 * time.Second)

	// A run of this process is live: it settles itself, so the nudge stands
	// down rather than racing it.
	handle := armOpenCodeUsageRun("unit")
	t.Cleanup(func() { handle.Disarm() })
	if nudgeOpenCodeUsageRefresh(now.Add(time.Hour)) {
		t.Fatal("a nudge must not fire while one of this process's runs is live")
	}
}

func TestOpenCodeDiscovery_SeesAWriteUnderTheProjectScopedLayout(t *testing.T) {
	// The TUI and native capture write `<storage>/project/<slug>/storage/
	// session[/info]`, whose SLUG directory mtime does not move when a session
	// file inside it is rewritten — so the walk must rank by the session
	// directories themselves.
	data := t.TempDir()
	t.Setenv("OPENCODE_DATA", data)
	t.Setenv("XDG_DATA_HOME", "")

	before := time.Now().Add(-time.Hour)
	// 70 slugs, so the newest one has to beat the 64-slug bound on merit.
	var newest string
	for i := 0; i < 70; i++ {
		slug := filepath.Join(data, "project", fmt.Sprintf("slug-%02d", i), "storage", "session", "info")
		if err := os.MkdirAll(slug, 0o755); err != nil {
			t.Fatal(err)
		}
		stamp := before.Add(-time.Duration(i) * time.Minute)
		if i == 69 {
			// The oldest slug DIRECTORY, holding the newest session write.
			stamp = time.Now()
			newest = slug
		}
		if err := os.Chtimes(slug, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if newest == "" {
		t.Fatal("fixture did not record the newest slug")
	}
	if !openCodeSessionStoreChangedSince(before) {
		t.Fatal("a write under project/<slug>/storage/session/info must be seen")
	}
	if openCodeSessionStoreChangedSince(time.Now().Add(time.Hour)) {
		t.Fatal("nothing is newer than an hour from now")
	}
}

func TestOpenCodeDiscovery_SeesAGlobalRootWrite(t *testing.T) {
	data := t.TempDir()
	t.Setenv("OPENCODE_DATA", data)
	t.Setenv("XDG_DATA_HOME", "")
	root := filepath.Join(data, "storage", "session")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-time.Hour)
	if err := os.Chtimes(root, before, before); err != nil {
		t.Fatal(err)
	}
	if openCodeSessionStoreChangedSince(time.Now().Add(-time.Minute)) {
		t.Fatal("an hour-old directory is not a change since a minute ago")
	}
	if err := os.WriteFile(filepath.Join(root, "ses_1.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !openCodeSessionStoreChangedSince(before) {
		t.Fatal("a write under the global root must be seen")
	}
}

/* ──────────────────────────── clock and restart ────────────────────────── */

func TestOpenCodeFreshness_RebasesAClockStepBack(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeDebtFixture(t, now)
	state := openCodeUsageFreshness{
		RunFloorMs:      now.Add(48 * time.Hour).UnixMilli(),
		CompletionMs:    now.Add(48 * time.Hour).UnixMilli(),
		OwedAtMs:        now.Add(48 * time.Hour).UnixMilli(),
		LastAttemptAtMs: now.Add(48 * time.Hour).UnixMilli(),
		NextAttemptAtMs: now.Add(72 * time.Hour).UnixMilli(),
	}
	openCodeRebaseFutureFreshness(&state, now)
	for name, got := range map[string]int64{
		"runFloor": state.RunFloorMs, "completion": state.CompletionMs,
		"owedAt": state.OwedAtMs, "lastAttempt": state.LastAttemptAtMs,
		"nextAttempt": state.NextAttemptAtMs,
	} {
		if got != now.UnixMilli() {
			t.Fatalf("%s = %d, want it pulled back to now (%d)", name, got, now.UnixMilli())
		}
	}
}

func TestOpenCodeRestart_AnUnsettledFloorOwesOneReconcile(t *testing.T) {
	// The self-update case: the previous process armed a floor and died before
	// the run settled, so nothing else will ever account for that run.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeDebtFixture(t, now)
	(&openCodeCLIStub{launchError: true}).install(t)

	if !writeJSONFileAtomic(openCodeUsageFreshnessPath(), openCodeUsageFreshness{
		SchemaVersion: openCodeUsageFreshnessSchema,
		RunFloorMs:    now.Add(-time.Minute).UnixMilli(),
	}) {
		t.Fatal("could not seed the previous process's state")
	}
	adoptAndPayOwedOpenCodeRunDebt(now)
	openCodeUsageRefreshWaitFor(2 * time.Second)
	if state := readOpenCodeFreshness(t); !state.owed() {
		t.Fatalf("state = %+v, want the interrupted run's debt adopted", state)
	}
}

func TestOpenCodeRestart_ReArmsABookedRung(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeDebtFixture(t, now)
	(&openCodeCLIStub{sessions: "[]"}).install(t)

	if !writeJSONFileAtomic(openCodeUsageFreshnessPath(), openCodeUsageFreshness{
		SchemaVersion: openCodeUsageFreshnessSchema,
		OwedAtMs:      now.Add(-time.Minute).UnixMilli(),
		CompletionMs:  now.Add(-time.Minute).UnixMilli(),
		Attempts:      1,
		// Inside the clock-skew ceiling, so the rebase leaves it alone and the
		// restart re-arms the same instant rather than paying it now.
		NextAttemptAtMs: now.Add(time.Minute).UnixMilli(),
	}) {
		t.Fatal("could not seed the previous process's state")
	}
	adoptAndPayOwedOpenCodeRunDebt(now)
	if !openCodeRunDebtRetryPending() {
		t.Fatal("a rung the previous process booked must be re-armed")
	}
}

func TestOpenCodeRestart_ResumesABookedContinuationWithNoNewFileWrite(t *testing.T) {
	// The persisted lastPassStartedAtMs would otherwise suppress the nudge, and
	// a backlog interrupted by a restart would sit until the next managed run.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeDebtFixture(t, now)
	(&openCodeCLIStub{sessions: "[]"}).install(t)

	updateOpenCodeUsageLedgerContinuation(true, 0, 0)
	adoptAndPayOwedOpenCodeRunDebt(now)
	if !openCodeRunDebtRetryPending() {
		t.Fatal("a booked continuation must be re-armed after a restart")
	}
}

func TestOpenCodeRestart_AMissingOrCorruptStateFileOwesNothing(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeDebtFixture(t, now)
	(&openCodeCLIStub{sessions: "[]"}).install(t)

	adoptAndPayOwedOpenCodeRunDebt(now)
	openCodeUsageRefreshWaitFor(2 * time.Second)
	if state := readOpenCodeFreshness(t); state.owed() {
		t.Fatalf("state = %+v, want nothing owed with no state file", state)
	}

	if err := os.WriteFile(openCodeUsageFreshnessPath(), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	adoptAndPayOwedOpenCodeRunDebt(now)
	openCodeUsageRefreshWaitFor(2 * time.Second)
	if state := readOpenCodeFreshness(t); state.owed() {
		t.Fatalf("state = %+v, want nothing owed for a corrupt state file", state)
	}
}

func TestOpenCodeLedger_SurvivesAnOpenCodeUpgrade(t *testing.T) {
	// Neither file is keyed by the binary, so a reading taken before an upgrade
	// still reaches the card afterwards — and the post-upgrade run adds to the
	// SAME day row.
	now := openCodeFixtureRunAt
	openCodeDebtFixture(t, now)

	first := armOpenCodeUsageRun("smoke")
	observeFixture(t, first, "run_two_steps.jsonl")
	first.Finish(true)
	openCodeUsageRefreshWaitFor(2 * time.Second)
	if tokens, _, _ := todayTotals(t, now); tokens != 63 {
		t.Fatalf("tokens = %d, want 63 before the upgrade", tokens)
	}
	generationBefore := readOpenCodeUsageLedger().Generation

	// "The binary changed"; the ledger does not care.
	second := armOpenCodeUsageRun("smoke")
	second.Observe(`{"type":"step_finish","timestamp":` +
		fmt.Sprintf("%d", now.UnixMilli()) +
		`,"sessionID":"ses_after","part":{"messageID":"msg_after","type":"step-finish",` +
		`"reason":"stop","cost":0,"tokens":{"input":7,"output":0,"reasoning":0}}}`)
	second.Finish(true)
	openCodeUsageRefreshWaitFor(2 * time.Second)

	tokens, _, rows := todayTotals(t, now)
	if tokens != 70 || rows != 2 {
		t.Fatalf("tokens=%d rows=%d, want the pre-upgrade reading carried across", tokens, rows)
	}
	if after := readOpenCodeUsageLedger().Generation; after.Counter <= generationBefore.Counter {
		t.Fatalf("generation %+v did not advance past %+v", after, generationBefore)
	}
}
