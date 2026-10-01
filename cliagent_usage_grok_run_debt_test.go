package main

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_grok_run_debt_test.go
   --------------------------------------------------------------------------
   The Grok run-completion refresh debt (cliagent_usage_grok_freshness.go):
   every finished run owes one live billing read, paid on a bounded ladder by a
   single-flight worker. These tests pin the bounds — the read cap, the
   spacing, free rungs, retirement, account scoping, clock rebases, age-out —
   against a fake read, so no test reaches xAI.
   ------------------------------------------------------------------------ */

// grokDebtHarness isolates the freshness state, pins the clock and the
// signed-in account, and replaces the live read with a counting fake.
type grokDebtHarness struct {
	t     *testing.T
	mu    sync.Mutex
	now   time.Time
	fp    string
	reads atomic.Int64
	// outcome is what the fake read answers; an `ok` saves a numeric reading
	// under the account signed in when it ran.
	outcome atomic.Value
	// block, when set, parks every read until it is closed.
	block chan struct{}
	// unmetered makes an `ok` read save a reading with no percentage.
	unmetered atomic.Bool
}

func newGrokDebtHarness(t *testing.T) *grokDebtHarness {
	t.Helper()
	h := &grokDebtHarness{t: t, now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC), fp: "fp-ada"}
	h.outcome.Store(grokLiveOutcomeOK)
	dir := t.TempDir()
	t.Setenv(grokUsageFreshnessEnv, filepath.Join(dir, "grok_usage_freshness.json"))
	t.Setenv("AIEXPEDITE_GROK_BILLING_LIVE_CACHE", filepath.Join(dir, "grok_billing_live.json"))
	// An empty home: no log record can cover a run, only the live cache.
	t.Setenv("GROK_HOME", filepath.Join(dir, "grok-home"))

	origNow, origFP, origRead, origPath := grokUsageFreshnessNow, grokUsageCurrentFingerprint, grokBillingLiveReadFn, grokUsageRefreshGrokPath
	origBudget, origReadTimeout, origSlack := grokSmokeUsageSettleBudget, grokRunDebtReadTimeout, grokRunDebtReadWaitSlack
	t.Cleanup(func() {
		stopGrokRunDebtRetry()
		if h.block != nil {
			select {
			case <-h.block:
			default:
				close(h.block)
			}
		}
		grokUsageRefreshWaitFor(5 * time.Second)
		SetGrokUsageRefreshEnabled(false)
		grokUsageFreshnessNow, grokUsageCurrentFingerprint, grokBillingLiveReadFn, grokUsageRefreshGrokPath = origNow, origFP, origRead, origPath
		grokSmokeUsageSettleBudget, grokRunDebtReadTimeout, grokRunDebtReadWaitSlack = origBudget, origReadTimeout, origSlack
		grokLiveRunsMu.Lock()
		grokLiveRuns = map[int64]int{}
		grokLiveRunsMu.Unlock()
		grokRefreshNudge.mu.Lock()
		grokRefreshNudge.lastAt = time.Time{}
		grokRefreshNudge.mu.Unlock()
		grokLastRefreshOutcome.Store("")
	})
	grokUsageFreshnessNow = h.clock
	grokUsageCurrentFingerprint = h.fingerprint
	grokUsageRefreshGrokPath = func() string { return "" }
	grokBillingLiveReadFn = func(_ context.Context, _ string, now func() time.Time) string {
		h.reads.Add(1)
		if h.block != nil {
			<-h.block
		}
		outcome := h.outcome.Load().(string)
		if outcome == grokLiveOutcomeOK {
			saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
				ObservedAt: now(), PeriodType: "USAGE_PERIOD_TYPE_WEEKLY", UsedPercent: 36, HasUsedPercent: !h.unmetered.Load(),
			}, h.fingerprint()))
		}
		return outcome
	}
	SetGrokUsageRefreshEnabled(true)
	return h
}

func (h *grokDebtHarness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *grokDebtHarness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

func (h *grokDebtHarness) fingerprint() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fp
}

func (h *grokDebtHarness) signIn(fp string) {
	h.mu.Lock()
	h.fp = fp
	h.mu.Unlock()
}

func (h *grokDebtHarness) idle() {
	h.t.Helper()
	if !grokUsageRefreshWaitFor(5 * time.Second) {
		h.t.Fatal("refresh worker never went idle")
	}
}

func (h *grokDebtHarness) state() grokUsageFreshness {
	var state grokUsageFreshness
	readJSONFile(grokUsageFreshnessPath(), &state)
	return state
}

func (h *grokDebtHarness) write(state grokUsageFreshness) {
	updateGrokUsageFreshness(func(s *grokUsageFreshness) { *s = state })
}

// runAndSettle arms a run now, advances d, and settles it.
func (h *grokDebtHarness) runAndSettle(d time.Duration) {
	floor := armGrokUsageRunFloor(h.clock())
	h.advance(d)
	grokUsageRunSettled(floor)
	h.idle()
}

// pass runs one worker pass the way a due rung does.
func (h *grokDebtHarness) pass() {
	grokStartRunDebtWorker(false)
	h.idle()
}

func TestGrokRunDebt_SettledRunPaysOneReadAndClearsTheDebt(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.runAndSettle(time.Second)

	if got := h.reads.Load(); got != 1 {
		t.Fatalf("reads = %d, want 1", got)
	}
	if state := h.state(); state.owed() || state.RunFloorMs != 0 {
		t.Fatalf("state after an ok read = %+v, want nothing owed", state)
	}
	snap, ok := loadGrokBillingLiveSnapshot("fp-ada")
	if !ok || !snap.HasUsedPercent || snap.ObservedAt.Before(h.clock()) {
		t.Fatalf("live reading = %+v ok=%t, want a numeric reading at or after completion", snap, ok)
	}
}

func TestGrokRunDebt_CoveredRunOwesNothing(t *testing.T) {
	h := newGrokDebtHarness(t)
	floor := armGrokUsageRunFloor(h.clock())
	h.advance(time.Second)
	// A Refresh click landed a reading after the run completed.
	saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
		ObservedAt: h.clock().Add(time.Millisecond), PeriodType: "USAGE_PERIOD_TYPE_WEEKLY", UsedPercent: 1, HasUsedPercent: true,
	}, "fp-ada"))
	if grokUsageRunSettled(floor) {
		t.Fatal("a run covered by a later reading must owe nothing")
	}
	h.idle()
	if h.reads.Load() != 0 || h.state().owed() {
		t.Fatalf("reads=%d state=%+v, want no read and no debt", h.reads.Load(), h.state())
	}
}

func TestGrokRunDebt_LadderAndReadCap(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.outcome.Store(grokLiveOutcomeHTTPError)
	h.runAndSettle(time.Second)

	wantRungs := []time.Duration{time.Minute, 2 * time.Minute, 8 * time.Minute}
	for i, rung := range wantRungs {
		state := h.state()
		if state.Attempts != i+1 {
			t.Fatalf("after read %d attempts = %d", i+1, state.Attempts)
		}
		if got := time.UnixMilli(state.NextAttemptAtMs).Sub(h.clock()); got != rung {
			t.Fatalf("rung after read %d = %s, want %s", i+1, got, rung)
		}
		if !grokRunDebtRetryPending() {
			t.Fatalf("no timer armed after read %d", i+1)
		}
		h.advance(rung)
		h.pass()
	}
	state := h.state()
	if h.reads.Load() != grokRunDebtMaxAttempts || state.Attempts != grokRunDebtMaxAttempts {
		t.Fatalf("reads=%d attempts=%d, want the cap of %d", h.reads.Load(), state.Attempts, grokRunDebtMaxAttempts)
	}
	if state.NextAttemptAtMs != 0 {
		t.Fatalf("a spent budget booked another rung: %+v", state)
	}
	// Further passes and further runs inside the cooling window send nothing.
	h.advance(time.Hour / 4)
	h.pass()
	h.runAndSettle(time.Second)
	if h.reads.Load() != grokRunDebtMaxAttempts {
		t.Fatalf("reads = %d after the cap, want %d", h.reads.Load(), grokRunDebtMaxAttempts)
	}
	// Once the longest rung has passed since the last read, a new run gets a
	// fresh budget.
	h.advance(grokRunDebtRetryLadder[len(grokRunDebtRetryLadder)-1])
	h.runAndSettle(time.Second)
	if h.reads.Load() != grokRunDebtMaxAttempts+1 {
		t.Fatalf("reads = %d, want one more read after the cooling window", h.reads.Load())
	}
}

func TestGrokRunDebt_SpacingAndSmokeBypass(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.outcome.Store(grokLiveOutcomeHTTPError)
	h.runAndSettle(time.Second)
	first := h.state().LastAttemptAtMs

	// A second run 30 s later is spaced: no read, rung at the 60 s mark.
	h.runAndSettle(30 * time.Second)
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want the second run deferred by the spacing", h.reads.Load())
	}
	if state := h.state(); state.NextAttemptAtMs != first+grokRefreshMinInterval.Milliseconds() {
		t.Fatalf("next attempt = %d, want the spacing boundary %d", state.NextAttemptAtMs, first+grokRefreshMinInterval.Milliseconds())
	}

	// A smoke inside the spacing still reads once — it bypasses the spacing,
	// not the cap.
	h.advance(5 * time.Second)
	floor := armGrokUsageRunFloor(h.clock())
	h.advance(time.Second)
	if got := settleOrDisarmGrokSmokeRun(context.Background(), floor, true); got != grokLiveOutcomeHTTPError {
		t.Fatalf("smoke refresh = %q, want the read's own outcome", got)
	}
	if h.reads.Load() != 2 || h.state().Attempts != 2 {
		t.Fatalf("reads=%d state=%+v, want the bypassing read counted", h.reads.Load(), h.state())
	}

	state := h.state()
	state.Attempts = grokRunDebtMaxAttempts
	h.write(state)
	// A spent budget stops an ordinary run's settle...
	h.advance(2 * time.Minute)
	h.runAndSettle(time.Second)
	if h.reads.Load() != 2 {
		t.Fatalf("reads = %d, an ordinary run must not read past the cap", h.reads.Load())
	}
	// ...but a smoke that finished after the last read gets exactly one, so
	// earlier refusals cannot leave the card stale after a green smoke.
	floor = armGrokUsageRunFloor(h.clock())
	h.advance(time.Second)
	if got := settleOrDisarmGrokSmokeRun(context.Background(), floor, true); got != grokLiveOutcomeHTTPError {
		t.Fatalf("smoke refresh = %q, want its one read past the cap", got)
	}
	if h.reads.Load() != 3 || h.state().Attempts != grokRunDebtMaxAttempts {
		t.Fatalf("reads=%d state=%+v, want one smoke read with the attempt count held at the cap", h.reads.Load(), h.state())
	}
	// A second smoke in the same instant as that read gets nothing more.
	floor = armGrokUsageRunFloor(h.clock())
	if got := settleOrDisarmGrokSmokeRun(context.Background(), floor, true); got != "exhausted" {
		t.Fatalf("smoke refresh = %q, want exhausted", got)
	}
	if h.reads.Load() != 3 {
		t.Fatalf("reads = %d, want no read for a smoke no newer than the last one", h.reads.Load())
	}
}

func TestGrokRunDebt_FreeRungsSpendNoBudget(t *testing.T) {
	for _, outcome := range []string{grokLiveOutcomeLoginBusy, grokLiveOutcomeWriteFailed} {
		t.Run(outcome, func(t *testing.T) {
			h := newGrokDebtHarness(t)
			h.outcome.Store(outcome)
			h.runAndSettle(time.Second)
			state := h.state()
			if !state.owed() || state.Attempts != 0 || state.LastOutcome != outcome {
				t.Fatalf("state = %+v, want the debt kept with no budget spent", state)
			}
			if state.NextAttemptAtMs <= h.clock().UnixMilli() || !grokRunDebtRetryPending() {
				t.Fatalf("no free rung booked: %+v", state)
			}
			h.advance(time.Minute)
			h.pass()
			if h.reads.Load() != 2 || h.state().Attempts != 0 {
				t.Fatalf("reads=%d state=%+v", h.reads.Load(), h.state())
			}
		})
	}
}

func TestGrokRunDebt_AuthOutcomesSpendOneAttemptAndNeverReopenOnAGather(t *testing.T) {
	for _, outcome := range []string{grokLiveOutcomeNoLogin, grokLiveOutcomeUnauthorized, grokLiveOutcomeNoAccount} {
		t.Run(outcome, func(t *testing.T) {
			h := newGrokDebtHarness(t)
			h.outcome.Store(outcome)
			h.runAndSettle(time.Second)
			state := h.state()
			if !state.owed() || state.Attempts != 1 || state.NextAttemptAtMs != 0 || grokRunDebtRetryPending() {
				t.Fatalf("state = %+v, want one attempt spent, the debt kept, and no rung", state)
			}

			// Gathers keep finding the unread log record, well past any
			// cooling window. None of them may read (an `unauthorized` read
			// also spawns a login renewal).
			for i := 0; i < 5; i++ {
				h.advance(10 * time.Minute)
				nudgeGrokUsageRefresh(h.clock(), "fp-ada")
				h.idle()
			}
			if h.reads.Load() != 1 {
				t.Fatalf("reads = %d, a gather re-opened an auth-stopped debt", h.reads.Load())
			}

			// The next run that settles still gets its read — one transient
			// refusal does not stop refreshes — and each refusal spends one
			// attempt until the cap.
			for want := int64(2); want <= grokRunDebtMaxAttempts; want++ {
				h.runAndSettle(time.Second)
				if h.reads.Load() != want {
					t.Fatalf("reads = %d, want %d", h.reads.Load(), want)
				}
				h.advance(2 * time.Minute)
			}
			h.runAndSettle(time.Second)
			if h.reads.Load() != grokRunDebtMaxAttempts {
				t.Fatalf("reads = %d, refusals must stop at the cap", h.reads.Load())
			}
		})
	}
}

func TestGrokRunDebt_SmokeAfterATransientRefusalReadsAndPublishes(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.outcome.Store(grokLiveOutcomeUnauthorized)
	h.runAndSettle(time.Second)

	// The login recovers; the maintenance smoke runs seconds later.
	h.outcome.Store(grokLiveOutcomeOK)
	floor := armGrokUsageRunFloor(h.clock())
	h.advance(5 * time.Second)
	if got := settleOrDisarmGrokSmokeRun(context.Background(), floor, true); got != grokLiveOutcomeOK {
		t.Fatalf("smoke refresh = %q, want ok", got)
	}
	if h.reads.Load() != 2 || h.state().owed() {
		t.Fatalf("reads=%d state=%+v, want the smoke's read to pay the debt", h.reads.Load(), h.state())
	}
}

func TestGrokRunDebt_SuccessfulClickClearsAStoppedDebt(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.outcome.Store(grokLiveOutcomeUnauthorized)
	h.runAndSettle(time.Second)
	h.outcome.Store(grokLiveOutcomeOK)
	h.advance(time.Second)
	if got := grokBillingReadOnce(context.Background(), "", "fp-ada", h.clock); got != grokLiveOutcomeOK {
		t.Fatalf("click = %q", got)
	}
	if state := h.state(); state.owed() {
		t.Fatalf("state = %+v, a successful click must clear the stopped debt", state)
	}
}

func TestGrokRunDebt_AccountChangeRetiresWithoutARequest(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.outcome.Store(grokLiveOutcomeHTTPError)
	h.runAndSettle(time.Second)
	h.signIn("fp-bob")
	h.advance(time.Minute)
	h.pass()
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, a debt must never be paid against another login", h.reads.Load())
	}
	if h.state().owed() {
		t.Fatalf("debt kept after the account changed: %+v", h.state())
	}
}

func TestGrokRunDebt_ReadingUnderTheOldAccountNeverCovers(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.signIn("fp-bob")
	floor := armGrokUsageRunFloor(h.clock())
	h.advance(time.Second)
	saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
		ObservedAt: h.clock().Add(time.Second), PeriodType: "USAGE_PERIOD_TYPE_WEEKLY", UsedPercent: 3, HasUsedPercent: true,
	}, "fp-ada"))
	if !grokUsageRunSettled(floor) {
		t.Fatal("a reading saved for another account covered this run")
	}
	h.idle()
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want one read for the new account", h.reads.Load())
	}
}

func TestGrokRunDebt_ClockRollbackRebasesFutureTimestamps(t *testing.T) {
	h := newGrokDebtHarness(t)
	now := h.clock()
	future := now.Add(48 * time.Hour).UnixMilli()
	h.write(grokUsageFreshness{
		RunFloorMs: future, CompletionMs: future, OwedAtMs: future, NextAttemptAtMs: future,
		LastAttemptAtMs: future, AccountFingerprint: "fp-ada",
	})
	state, owed := grokPendingDebt(now)
	if !owed {
		t.Fatal("a rebased debt must still be owed")
	}
	for name, ts := range map[string]int64{
		"runFloor": state.RunFloorMs, "completion": state.CompletionMs, "owedAt": state.OwedAtMs,
		"nextAttempt": state.NextAttemptAtMs, "lastAttempt": state.LastAttemptAtMs,
	} {
		if ts != now.UnixMilli() {
			t.Errorf("%s = %d, want rebased to now %d", name, ts, now.UnixMilli())
		}
	}
}

func TestGrokRunDebt_AgesOutAfterSixHours(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.outcome.Store(grokLiveOutcomeHTTPError)
	h.runAndSettle(time.Second)
	h.advance(grokRunDebtMaxAge + time.Minute)
	h.pass()
	if h.reads.Load() != 1 || h.state().owed() {
		t.Fatalf("reads=%d state=%+v, want an aged-out debt retired unpaid", h.reads.Load(), h.state())
	}
}

func TestGrokRunDebt_SmokeRefusedBeforeInferenceOwesNothing(t *testing.T) {
	h := newGrokDebtHarness(t)
	floor := armGrokUsageRunFloor(h.clock())
	h.idle()
	if h.state().RunFloorMs != floor.UnixMilli() {
		t.Fatalf("arm did not persist its floor: %+v", h.state())
	}
	h.advance(time.Second)
	if got := settleOrDisarmGrokSmokeRun(context.Background(), floor, false); got != "not_owed" {
		t.Fatalf("refresh = %q, want not_owed", got)
	}
	if h.reads.Load() != 0 || h.state() != (grokUsageFreshness{}) {
		t.Fatalf("reads=%d state=%+v, want nothing owed and no floor left", h.reads.Load(), h.state())
	}
}

func TestGrokBillingReadOnce_DifferentAccountsNeverShareARead(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.block = make(chan struct{})
	var wg sync.WaitGroup
	outcomes := make([]string, 3)
	for i, fp := range []string{"fp-ada", "fp-bob", "fp-ada"} {
		wg.Add(1)
		go func(i int, fp string) {
			defer wg.Done()
			outcomes[i] = grokBillingReadOnce(context.Background(), "", fp, h.clock)
		}(i, fp)
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.reads.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(h.block)
	wg.Wait()
	if got := h.reads.Load(); got != 2 {
		t.Fatalf("reads = %d, want one per account (the second ada caller joins the first)", got)
	}
	for i, o := range outcomes {
		if o != grokLiveOutcomeOK {
			t.Errorf("caller %d outcome = %q", i, o)
		}
	}
}

func TestGrokRunDebt_ConcurrentSettlesCollapseAndKeepAttempts(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.outcome.Store(grokLiveOutcomeHTTPError)
	smokeFloor := armGrokUsageRunFloor(h.clock())
	h.advance(time.Second)
	acpFloor := armGrokUsageRunFloor(h.clock())
	h.advance(time.Second)
	grokUsageRunSettled(smokeFloor)
	h.idle()
	first := h.state()
	if first.Attempts != 1 {
		t.Fatalf("first settle attempts = %d", first.Attempts)
	}

	h.advance(10 * time.Second)
	grokUsageRunSettled(acpFloor)
	h.idle()
	second := h.state()
	if second.OwedAtMs != first.OwedAtMs || second.Attempts != 1 {
		t.Fatalf("second settle = %+v, want the same debt with attempts kept", second)
	}
	if second.CompletionMs <= first.CompletionMs || second.RunFloorMs != acpFloor.UnixMilli() {
		t.Fatalf("second settle = %+v, want completion and floor moved forward", second)
	}
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want the spaced second settle to send nothing", h.reads.Load())
	}
}

func TestGrokRunDebt_DirectRunNudgeOwesOncePerCooldown(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.outcome.Store(grokLiveOutcomeHTTPError)
	if !nudgeGrokUsageRefresh(h.clock(), "fp-ada") {
		t.Fatal("the first nudge must owe a read")
	}
	h.idle()
	h.advance(10 * time.Second)
	if nudgeGrokUsageRefresh(h.clock(), "fp-ada") {
		t.Fatal("a nudge inside the cooldown must do nothing")
	}
	h.idle()
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want 1", h.reads.Load())
	}
	// Past the cooldown, a booked rung that is due is started; one not yet
	// due is left to its timer.
	h.advance(grokRefreshNudgeCooldown)
	if !nudgeGrokUsageRefresh(h.clock(), "fp-ada") {
		t.Fatal("a due rung must be started by the nudge")
	}
	h.idle()
	if h.reads.Load() != 2 {
		t.Fatalf("reads = %d, want the due rung paid", h.reads.Load())
	}
}

func TestGrokRunDebt_DisabledRefreshArmsNothing(t *testing.T) {
	h := newGrokDebtHarness(t)
	SetGrokUsageRefreshEnabled(false)
	if floor := armGrokUsageRunFloor(h.clock()); !floor.IsZero() {
		t.Fatalf("disabled arm returned %s", floor)
	}
	if nudgeGrokUsageRefresh(h.clock(), "fp-ada") || h.reads.Load() != 0 {
		t.Fatal("a disabled refresh must never read")
	}
}

// The arm persists off the spawn path (the ACP manager arms under its own
// mutex). A persist that lands after its run already settled must not leave a
// floor behind for the next start to adopt as an interrupted run.
func TestGrokRunDebt_LateArmPersistNeverLeavesAStaleFloor(t *testing.T) {
	h := newGrokDebtHarness(t)
	grokFreshnessMu.Lock() // park the arm's persist
	floor := armGrokUsageRunFloor(h.clock())
	if !grokRunIsLive(floor.UnixMilli()) {
		grokFreshnessMu.Unlock()
		t.Fatal("the floor must be live from the instant it is armed")
	}
	disarmed := make(chan struct{})
	go func() {
		disarmGrokUsageRunFloor(floor) // releases the run, then waits on the lock
		close(disarmed)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for grokRunIsLive(floor.UnixMilli()) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	grokFreshnessMu.Unlock()
	<-disarmed
	h.idle()
	if state := h.state(); state.RunFloorMs != 0 {
		t.Fatalf("state = %+v, a persist landing after the settle left a floor", state)
	}
}

// xAI can answer 200 with a period and no creditUsagePercent. That is no
// number for the card, so it must not pay the debt: the read spends an
// attempt and the ladder tries again.
func TestGrokRunDebt_OkWithoutAPercentagePaysNothing(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.unmetered.Store(true)
	h.runAndSettle(time.Second)
	state := h.state()
	if !state.owed() || state.Attempts != 1 || state.LastOutcome != grokRefreshOutcomeUnmetered {
		t.Fatalf("state = %+v, want the debt kept with one attempt spent as unmetered", state)
	}
	if state.NextAttemptAtMs == 0 || !grokRunDebtRetryPending() {
		t.Fatalf("no rung booked after an unmetered read: %+v", state)
	}
	if grokObservationCovers("fp-ada", state.CompletionMs) {
		t.Fatal("a percent-less live reading counted as covering the run")
	}

	// The next rung gets a number and pays.
	h.unmetered.Store(false)
	h.advance(time.Minute)
	h.pass()
	if h.reads.Load() != 2 || h.state().owed() {
		t.Fatalf("reads=%d state=%+v, want the numeric read to pay", h.reads.Load(), h.state())
	}
}

// A caller that stops waiting on the shared read is not a failed read: it
// spends no budget, and the flight's own ok still retires the debt.
func TestGrokRunDebt_CallerTimeoutSpendsNothingAndTheFlightStillPays(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.block = make(chan struct{})
	grokRunDebtReadWaitSlack = -grokRunDebtReadTimeout + 20*time.Millisecond // the pass waits 20 ms

	floor := armGrokUsageRunFloor(h.clock())
	h.advance(time.Second)
	grokUsageRunSettled(floor)
	deadline := time.Now().Add(5 * time.Second)
	for h.state().LastOutcome != liveProbeOutcomeTimeout && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if state := h.state(); state.LastOutcome != liveProbeOutcomeTimeout || state.Attempts != 0 || !state.owed() {
		t.Fatalf("state = %+v, want a timeout that spends nothing", state)
	}

	close(h.block)
	h.idle()
	if h.reads.Load() != 1 || h.state().owed() {
		t.Fatalf("reads=%d state=%+v, want the late ok from the same flight to retire the debt", h.reads.Load(), h.state())
	}
}

func TestGrokBillingReadOnce_CallerTimeoutReportsTimeout(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.block = make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if got := grokBillingReadOnce(ctx, "", "fp-ada", h.clock); got != liveProbeOutcomeTimeout {
		t.Fatalf("outcome = %q, want %q", got, liveProbeOutcomeTimeout)
	}
	close(h.block)
	h.idle()
}

// gracefulShutdown drains this feature's background writes, so an update
// hand-off cannot exit before an arm's floor (or a settle's debt) is on disk.
func TestDrainGrokUsageWrites_WaitsForAnInFlightPersist(t *testing.T) {
	h := newGrokDebtHarness(t)
	grokFreshnessMu.Lock() // park the arm's persist
	floor := armGrokUsageRunFloor(h.clock())
	drained := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		drainGrokUsageWrites(ctx)
		close(drained)
	}()
	select {
	case <-drained:
		grokFreshnessMu.Unlock()
		t.Fatal("the drain returned while a persist was still in flight")
	case <-time.After(100 * time.Millisecond):
	}
	grokFreshnessMu.Unlock()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain never returned")
	}
	if h.state().RunFloorMs != floor.UnixMilli() {
		t.Fatalf("state = %+v, want the armed floor on disk after the drain", h.state())
	}
}
