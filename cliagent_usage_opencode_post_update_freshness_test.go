package main

import (
	"context"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_opencode_post_update_freshness_test.go — usage owed by one
   agent process is paid by the next: a settled debt, a run cut off mid-turn,
   and a ledger reading whose hint died in the hand-off.
   ------------------------------------------------------------------------ */

// simulateOpenCodeProcessRestart is a fresh agent process over the same
// ledger: a new epoch, no rotation, no timers, no propagator memory.
func simulateOpenCodeProcessRestart(t *testing.T, epoch int64) {
	t.Helper()
	resetOpenCodeUsageFreshness()
	resetCLIUsagePropagator()
	codexProcessGenerationEpoch.Store(epoch)
	codexGenerationRotated.Store(false)
	openCodeGenerationRotated.Store(false)
}

func TestOpenCodeUsage_ADebtSurvivesTheProcessAndIsPaidByTheNext(t *testing.T) {
	sched := openCodeUsageFixture(t, 1101)
	stubOpenCodeExecutable(t)
	run := armOpenCodeUsageRun("opencode", "", "fp-a")
	created := time.Now()
	settleOpenCodeUsageRun(run, "ses_handoff")

	// The update hand-off replaces the process before the first rung fires.
	simulateOpenCodeProcessRestart(t, 1102)
	stubOpenCodeExport(t, func(_ context.Context, sessionID string) ([]byte, bool) {
		if sessionID != "ses_handoff" {
			t.Errorf("exported %q, want the owed session", sessionID)
		}
		return openCodeExportJSON("msg_h", created.UnixMilli(), 70, 30, 0.2), true
	})
	adoptOwedOpenCodeUsage(time.Now())
	if !sched.fireNext() || !sched.fireNext() {
		t.Fatal("the next process did not book the owed attempt")
	}
	b, ok, g := openCodeUsageBucketForDay("fp-a", time.Now())
	if !ok || b.tokens() != 100 || b.CostUsd != 0.2 {
		t.Fatalf("bucket = %+v, want the exported spend", b)
	}
	if g.Epoch != 1102 {
		t.Fatalf("generation %+v was not committed under the new epoch", g)
	}
	if left := readOpenCodeUsageLedger().Debts; len(left) != 0 {
		t.Fatalf("debts = %+v after payment", left)
	}
}

func TestOpenCodeUsage_ARunKilledMidTurnIsPaidOnlyWhenItNamedItsSession(t *testing.T) {
	sched := openCodeUsageFixture(t, 1103)
	stubOpenCodeExecutable(t)

	named := armOpenCodeUsageRun("opencode", "", "fp-a")
	captureOpenCodeUsageLine(named, `{"type":"session.created","sessionID":"ses_killed"}`)
	// The crash drops the in-memory step too: only the debt is left.
	captureOpenCodeUsageLine(named, openCodeStepFinish("ses_killed", "prt_lost", 9, 9, 0, "0", time.Now().UnixMilli()))
	openCodeUsageInFlight.Wait()
	armOpenCodeUsageRun("opencode", "", "fp-a") // never named a session

	simulateOpenCodeProcessRestart(t, 1104)
	exported := ""
	stubOpenCodeExport(t, func(_ context.Context, sessionID string) ([]byte, bool) {
		exported = sessionID
		// Created after the run's floor; an adopted run has no settle bound.
		return openCodeExportJSON("msg_k", time.Now().UnixMilli(), 11, 22, 0), true
	})
	adoptOwedOpenCodeUsage(time.Now().Add(time.Second))
	debts := readOpenCodeUsageLedger().Debts
	if len(debts) != 1 || debts[0].SessionID != "ses_killed" || !debts[0].owed() {
		t.Fatalf("debts = %+v, want only the named run, now owed", debts)
	}
	for sched.fireNext() {
	}
	if exported != "ses_killed" {
		t.Fatalf("exported %q", exported)
	}
	if b, _, _ := openCodeUsageBucketForDay("fp-a", time.Now()); b.tokens() != 33 {
		t.Fatalf("tokens = %d, want 33", b.tokens())
	}
}

func TestOpenCodeUsage_AdoptionLeavesThisProcesssOwnArmedRunAlone(t *testing.T) {
	openCodeUsageFixture(t, 1105)
	startedAt := time.Now().Add(-time.Second)
	run := armOpenCodeUsageRun("opencode", "", "fp-a") // a smoke racing startup
	adoptOwedOpenCodeUsage(startedAt)
	debts := readOpenCodeUsageLedger().Debts
	if len(debts) != 1 || debts[0].owed() || debts[0].RunID != run.id {
		t.Fatalf("debts = %+v, want this process's armed run untouched", debts)
	}
}

func TestOpenCodeUsage_TheLedgerSurvivesARestartAndRecoveryNotesItOnce(t *testing.T) {
	openCodeUsageFixture(t, 1106)
	now := time.Now()
	run := armOpenCodeUsageRun("opencode", "", "fp-a")
	captureOpenCodeUsageLine(run, openCodeStepFinish("ses_a", "prt_1", 40, 2, 0, "0", now.UnixMilli()))
	settleOpenCodeUsageRun(run, "")

	simulateOpenCodeProcessRestart(t, 1107)
	stubOpenCodeReadiness(t, "", false)
	// Before the rotation, the parser must not publish the earlier epoch's
	// generation — the backend may already have applied it.
	usage, _ := openCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{Path: "opencode"}, now)
	if usage.UsageGeneration != nil {
		t.Fatalf("published %+v before the rotation", usage.UsageGeneration)
	}

	g := openCodeRecoveryGeneration(now)
	if g == nil || g.Epoch != 1107 || g.Counter != 1 {
		t.Fatalf("recovery generation = %+v, want the rotated {1107,1}", g)
	}
	if again := openCodeRecoveryGeneration(now); again == nil || *again != *g {
		t.Fatalf("a second recovery moved the generation to %+v", again)
	}
	if b, ok, _ := openCodeUsageBucketForDay("fp-a", now); !ok || b.tokens() != 42 {
		t.Fatalf("bucket lost across the restart: %+v", b)
	}
	// Tomorrow there is nothing to publish, so nothing is recovered.
	simulateOpenCodeProcessRestart(t, 1108)
	if g := openCodeRecoveryGeneration(now.Add(48 * time.Hour)); g != nil {
		t.Fatalf("recovered %+v for a day with no numbers", g)
	}
}

func TestOpenCodeUsage_StartupRecoveryHintsTheRotatedLedger(t *testing.T) {
	openCodeUsageFixture(t, 1109)
	now := time.Now()
	run := armOpenCodeUsageRun("opencode", "", "fp-a")
	captureOpenCodeUsageLine(run, openCodeStepFinish("ses_a", "prt_1", 3, 4, 0, "0", now.UnixMilli()))
	settleOpenCodeUsageRun(run, "")

	rec, cfg := propagatorFixture(t)
	simulateOpenCodeProcessRestart(t, 1110)
	startCLIUsagePropagator(cfg)
	h := waitHints(t, rec, 1, 0)[0].hint
	if h.Provider != openCodeUsageProvider || h.GenerationEpoch != 1110 || h.Generation != 1 {
		t.Fatalf("recovery hint = %+v, want opencode {1110,1}", h)
	}
}

// During an update hand-off the old and new agent processes overlap. A ledger
// write while the other holds the cross-process lock is refused — never renamed
// over the holder's — and the run keeps its steps, so the next flush commits
// them once the lock is free.
func TestOpenCodeUsage_AContendedLedgerLockRefusesTheWriteAndTheFlushRetries(t *testing.T) {
	openCodeUsageFixture(t, 1111)
	prevWait := openCodeUsageLockWait
	openCodeUsageLockWait = 20 * time.Millisecond
	t.Cleanup(func() { openCodeUsageLockWait = prevWait })
	now := time.Now()

	run := armOpenCodeUsageRun("opencode", "", "fp-a")
	captureOpenCodeUsageLine(run, openCodeStepFinish("ses_a", "prt_1", 12, 3, 0, "0", now.UnixMilli()))
	openCodeUsageInFlight.Wait() // its session-id write holds the lock briefly

	// The other process holds the lock across this run's settle.
	held, outcome := acquireCrossProcessCacheLockUntil(openCodeUsageCachePath(), time.Now())
	if outcome != crossProcessLockAcquired {
		t.Fatalf("could not take the lock as the other process: %v", outcome)
	}
	settleOpenCodeUsageRun(run, "")
	if _, ok, _ := openCodeUsageBucketForDay("fp-a", now); ok {
		t.Fatal("a write landed while another process held the ledger lock")
	}
	_ = unlockFile(held)
	_ = held.Close()

	flushOpenCodeUsageRun(run) // the session's exit path
	if b, ok, _ := openCodeUsageBucketForDay("fp-a", now); !ok || b.tokens() != 15 {
		t.Fatalf("bucket = %+v, want the refused steps committed by the flush", b)
	}
	if debts := readOpenCodeUsageLedger().Debts; len(debts) != 0 {
		t.Fatalf("debts = %+v after the retried commit", debts)
	}
}

// holdOpenCodeLedgerLock takes the ledger's cross-process lock as the other
// agent process would, returning its release.
func holdOpenCodeLedgerLock(t *testing.T) func() {
	t.Helper()
	held, outcome := acquireCrossProcessCacheLockUntil(openCodeUsageCachePath(), time.Now())
	if outcome != crossProcessLockAcquired {
		t.Fatalf("could not take the lock as the other process: %v", outcome)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = unlockFile(held)
			_ = held.Close()
		}
	}
	t.Cleanup(release)
	return release
}

// A session arms under the session manager's lock, so a held ledger lock must
// not make it wait; the run still settles and owes normally.
func TestOpenCodeUsage_ArmingNeverWaitsOnAHeldLedgerLock(t *testing.T) {
	sched := openCodeUsageFixture(t, 1112)
	prevWait := openCodeUsageLockWait
	openCodeUsageLockWait = 2 * time.Second
	t.Cleanup(func() { openCodeUsageLockWait = prevWait })

	release := holdOpenCodeLedgerLock(t)
	start := time.Now()
	run := armOpenCodeUsageRun("opencode", "", "fp-a")
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("arming waited %s on another process's lock", took)
	}
	release()
	settleOpenCodeUsageRun(run, "ses_after_refused_arm")
	if debts := readOpenCodeUsageLedger().Debts; len(debts) != 1 || !debts[0].owed() {
		t.Fatalf("debts = %+v, want the run owed despite its refused arm", debts)
	}
	if len(sched.delays()) != 1 {
		t.Fatalf("booked %v, want the first rung", sched.delays())
	}
}

// An export that paid while the other process holds the ledger is not lost:
// the debt keeps a booked attempt (free, no attempt spent) and pays later.
func TestOpenCodeUsage_ARefusedAttemptWriteIsRetriedNotStranded(t *testing.T) {
	sched := openCodeUsageFixture(t, 1113)
	stubOpenCodeExecutable(t)
	prevWait := openCodeUsageLockWait
	openCodeUsageLockWait = 20 * time.Millisecond
	t.Cleanup(func() { openCodeUsageLockWait = prevWait })
	run := armOpenCodeUsageRun("opencode", "", "fp-a")
	created := time.Now()
	settleOpenCodeUsageRun(run, "ses_refused")
	stubOpenCodeExport(t, func(context.Context, string) ([]byte, bool) {
		return openCodeExportJSON("msg_r", created.UnixMilli(), 5, 5, 0), true
	})

	release := holdOpenCodeLedgerLock(t)
	sched.fireNext()
	if d := sched.delays(); len(d) != 2 || d[1] < openCodeUsageFreeFloor {
		t.Fatalf("booked %v, want a free retry after the refused write", d)
	}
	release()
	sched.fireNext()
	if b, ok, _ := openCodeUsageBucketForDay("fp-a", time.Now()); !ok || b.tokens() != 10 {
		t.Fatalf("bucket = %+v, want the export paid on the retry", b)
	}
	if debts := readOpenCodeUsageLedger().Debts; len(debts) != 0 {
		t.Fatalf("debts = %+v after payment", debts)
	}
}

// The process this update replaces may still hold the ledger when the new one
// adopts its debts: adoption is retried, not skipped until the next restart.
func TestOpenCodeUsage_StartupAdoptionRetriesWhileTheOldProcessHoldsTheLedger(t *testing.T) {
	sched := openCodeUsageFixture(t, 1114)
	prevWait := openCodeUsageLockWait
	openCodeUsageLockWait = 20 * time.Millisecond
	t.Cleanup(func() { openCodeUsageLockWait = prevWait })
	run := armOpenCodeUsageRun("opencode", "", "fp-a")
	captureOpenCodeUsageLine(run, `{"type":"session.created","sessionID":"ses_cutoff"}`)
	openCodeUsageInFlight.Wait()

	simulateOpenCodeProcessRestart(t, 1115)
	release := holdOpenCodeLedgerLock(t)
	adoptOwedOpenCodeUsage(time.Now().Add(time.Second))
	if d := readOpenCodeUsageLedger().Debts; len(d) != 1 || d[0].owed() {
		t.Fatalf("debts = %+v, want the armed debt untouched while locked", d)
	}
	release()
	sched.fireNext() // the adoption retry
	if d := readOpenCodeUsageLedger().Debts; len(d) != 1 || !d[0].owed() {
		t.Fatalf("debts = %+v, want the debt adopted on the retry", d)
	}
	if !sched.fireNext() {
		t.Fatal("the adopted debt has no attempt booked")
	}
}

// A refused commit hands its steps back only when nothing was taken since, so a
// later flush never offers the same steps twice.
func TestOpenCodeUsageRun_UntakeNeverReoffersStepsTakenSince(t *testing.T) {
	run := &openCodeUsageRun{steps: []openCodeUsageStep{{Input: 1}, {Input: 2}}}
	first, _, from := run.takeUncommitted()
	run.steps = append(run.steps, openCodeUsageStep{Input: 3})
	second, _, _ := run.takeUncommitted()
	run.untake(from, len(first)) // the first commit failed after the second take
	if again, _, _ := run.takeUncommitted(); len(again) != 0 {
		t.Fatalf("re-offered %+v after a later take (second took %+v)", again, second)
	}

	solo := &openCodeUsageRun{steps: []openCodeUsageStep{{Input: 1}}}
	taken, _, from := solo.takeUncommitted()
	solo.untake(from, len(taken))
	if again, _, _ := solo.takeUncommitted(); len(again) != 1 {
		t.Fatalf("a refused commit's steps were not handed back: %+v", again)
	}
}
