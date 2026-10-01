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
		grokLiveRunAccounts = map[int64]string{}
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
		// Stamped when the request is sent, as probeGrokBillingLive does.
		sentAt := now()
		if h.block != nil {
			<-h.block
		}
		outcome := h.outcome.Load().(string)
		if outcome == grokLiveOutcomeOK {
			saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
				ObservedAt: sentAt, PeriodType: "USAGE_PERIOD_TYPE_WEEKLY", UsedPercent: 36, HasUsedPercent: !h.unmetered.Load(),
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
	if got := grokBillingReadOnce(context.Background(), "", "fp-ada", h.clock, time.Time{}); got != grokLiveOutcomeOK {
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

func TestGrokRunDebt_RunIsBoundToTheAccountItWasArmedUnder(t *testing.T) {
	h := newGrokDebtHarness(t)
	// An isolated child spawned with ada's copied login; the persistent login
	// switches to bob before it exits.
	floor := armGrokUsageRunFloor(h.clock())
	h.signIn("fp-bob")
	h.advance(time.Second)
	if grokUsageRunSettled(floor) {
		t.Fatal("a run armed under another account opened a debt against the current one")
	}
	h.idle()
	if h.reads.Load() != 0 {
		t.Fatalf("reads = %d, ada's run must never be read against bob's login", h.reads.Load())
	}
	if state := h.state(); state.owed() || state.RunFloorMs != 0 {
		t.Fatalf("state = %+v, want nothing owed and no floor left behind", state)
	}

	// Same account at both ends still owes the read.
	floor = armGrokUsageRunFloor(h.clock())
	h.advance(time.Second)
	if !grokUsageRunSettled(floor) {
		t.Fatal("a run settled under its own account owed nothing")
	}
	h.idle()
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want one", h.reads.Load())
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
			outcomes[i] = grokBillingReadOnce(context.Background(), "", fp, h.clock, time.Time{})
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
	if got := grokBillingReadOnce(ctx, "", "fp-ada", h.clock, time.Time{}); got != liveProbeOutcomeTimeout {
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

// A rung booked by an earlier failed read must not outlive an auth refusal:
// left in place it falls due, and every gather (or a restart) would send
// another read — and, for `unauthorized`, another `grok models` renewal.
func TestGrokRunDebt_AuthRefusalCancelsAnEarlierRung(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.outcome.Store(grokLiveOutcomeHTTPError)
	h.runAndSettle(time.Second)
	if h.state().NextAttemptAtMs == 0 || !grokRunDebtRetryPending() {
		t.Fatalf("fixture: no rung booked after http_error: %+v", h.state())
	}

	h.outcome.Store(grokLiveOutcomeUnauthorized)
	h.advance(time.Minute)
	h.pass()
	state := h.state()
	if state.LastOutcome != grokLiveOutcomeUnauthorized || state.NextAttemptAtMs != 0 || grokRunDebtRetryPending() {
		t.Fatalf("state = %+v pending=%t, want the earlier rung cancelled", state, grokRunDebtRetryPending())
	}
	if h.reads.Load() != 2 {
		t.Fatalf("fixture reads = %d", h.reads.Load())
	}

	// Past where the old rung would have fallen due: gathers send nothing.
	for i := 0; i < 3; i++ {
		h.advance(10 * time.Minute)
		nudgeGrokUsageRefresh(h.clock(), "fp-ada")
		h.idle()
	}
	// Nor does an update restart.
	stopGrokRunDebtRetry()
	payOwedGrokUsageRefresh()
	h.idle()
	if h.reads.Load() != 2 {
		t.Fatalf("reads = %d, an auth-stopped debt was read by a gather or a restart", h.reads.Load())
	}
	if !h.state().owed() {
		t.Fatal("the debt must stay open for the next run")
	}

	// The next run that settles still reads.
	h.outcome.Store(grokLiveOutcomeOK)
	h.runAndSettle(time.Second)
	if h.reads.Load() != 3 || h.state().owed() {
		t.Fatalf("reads=%d state=%+v, want the next run to read and pay", h.reads.Load(), h.state())
	}
}

// The restart replay keys "stay idle" on the debt's own auth outcome, not on
// the spacing clock — which outlives debts — so a fresh debt a run settled
// just before the hand-off is still paid at start.
func TestPayOwedGrokUsageRefresh_FreshDebtIsPaidDespiteAnOldSpacingClock(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.write(grokUsageFreshness{
		CompletionMs: h.clock().Add(-time.Second).UnixMilli(), OwedAtMs: h.clock().Add(-time.Second).UnixMilli(),
		LastAttemptAtMs: h.clock().Add(-time.Hour).UnixMilli(), AccountFingerprint: "fp-ada",
	})
	payOwedGrokUsageRefresh()
	h.idle()
	if h.reads.Load() != 1 || h.state().owed() {
		t.Fatalf("reads=%d state=%+v, want the fresh debt paid at start", h.reads.Load(), h.state())
	}
}

// grokStartOverlappingAuthRefusal books a rung with an http_error, then runs
// the next read blocked as an `unauthorized`, calls during() while it is
// blocked, and releases it.
func grokStartOverlappingAuthRefusal(t *testing.T, h *grokDebtHarness, during func()) {
	t.Helper()
	h.outcome.Store(grokLiveOutcomeHTTPError)
	h.runAndSettle(time.Second)
	if !grokRunDebtRetryPending() {
		t.Fatal("fixture: no rung booked")
	}
	h.advance(time.Minute)
	h.block = make(chan struct{})
	h.outcome.Store(grokLiveOutcomeUnauthorized)
	grokStartRunDebtWorker(false)
	deadline := time.Now().Add(5 * time.Second)
	for h.reads.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if h.reads.Load() != 2 {
		t.Fatal("the overlapping read never started")
	}
	during()
	grokRefreshWorkerMu.Lock()
	rearmed := grokRefreshWorkerRearm
	grokRefreshWorkerMu.Unlock()
	if !rearmed {
		t.Fatal("fixture: the start during the read did not re-arm the worker")
	}
	close(h.block)
	h.idle()
}

// The old rung firing while a refusal is in flight must not turn into another
// read once that refusal idles the debt.
func TestGrokRunDebt_RungFiringDuringAnAuthRefusalIsDropped(t *testing.T) {
	h := newGrokDebtHarness(t)
	grokStartOverlappingAuthRefusal(t, h, func() {
		grokArmRunDebtRetry(h.state().debtID(), 0) // the rung comes due now
		deadline := time.Now().Add(5 * time.Second)
		for grokRunDebtRetryPending() && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond) // let the callback reach the worker claim
	})
	if grokRunDebtRetryPending() || h.state().NextAttemptAtMs != 0 {
		t.Fatalf("state = %+v pending=%t, want no rung left", h.state(), grokRunDebtRetryPending())
	}
	h.advance(10 * time.Minute)
	nudgeGrokUsageRefresh(h.clock(), "fp-ada")
	h.idle()
	if h.reads.Load() != 2 {
		t.Fatalf("reads = %d, want no read after the refusal", h.reads.Load())
	}
}

// A smoke that settles during the refusal still gets its own pass.
func TestGrokRunDebt_SmokeDuringAnAuthRefusalStillReads(t *testing.T) {
	h := newGrokDebtHarness(t)
	grokStartOverlappingAuthRefusal(t, h, func() { grokStartRunDebtWorker(true) })
	if h.reads.Load() != 3 {
		t.Fatalf("reads = %d, want the smoke's bypass pass to read after the refusal", h.reads.Load())
	}
}

func TestGrokRunDebt_IsolatedRunIsBoundToItsCopiedLogin(t *testing.T) {
	h := newGrokDebtHarness(t)
	orig := grokUsageHomeFingerprint
	t.Cleanup(func() { grokUsageHomeFingerprint = orig })
	// The isolated home was copied from ada's login; the persistent home is
	// switched to bob before the arm runs.
	grokUsageHomeFingerprint = func(home string) string {
		if home == "isolated-home" {
			return "fp-ada"
		}
		return ""
	}
	h.signIn("fp-bob")
	floor := armGrokUsageRunFloorFor(h.clock(), "isolated-home")
	h.idle()
	if state := h.state(); state.RunFloorAccount != "fp-ada" {
		t.Fatalf("RunFloorAccount = %q, want the copied login's account", state.RunFloorAccount)
	}
	h.advance(time.Second)
	if grokUsageRunSettled(floor) {
		t.Fatal("ada's isolated run opened a debt against bob's login")
	}
	h.idle()
	if h.reads.Load() != 0 {
		t.Fatalf("reads = %d, an isolated run must never refresh another account", h.reads.Load())
	}
}

func TestGrokRunDebt_RestartNeverAdoptsAFloorArmedUnderAnotherAccount(t *testing.T) {
	h := newGrokDebtHarness(t)
	// The old process died mid-run under ada; bob signed in before restart.
	h.write(grokUsageFreshness{RunFloorMs: h.clock().Add(-30 * time.Second).UnixMilli(), RunFloorAccount: "fp-ada"})
	h.signIn("fp-bob")
	payOwedGrokUsageRefresh()
	h.idle()
	if h.reads.Load() != 0 {
		t.Fatalf("reads = %d, ada's interrupted run must never be paid against bob", h.reads.Load())
	}
	if state := h.state(); state.owed() || state.RunFloorMs != 0 || state.RunFloorAccount != "" {
		t.Fatalf("state = %+v, want the foreign floor dropped", state)
	}
}

func TestGrokRunDebt_RetiringAnOldDebtKeepsALiveRunsFloor(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.outcome.Store(grokLiveOutcomeHTTPError)
	h.runAndSettle(time.Second) // ada owes a read, booked on a rung
	if !h.state().owed() {
		t.Fatalf("state = %+v, want ada's debt owed", h.state())
	}
	// Bob signs in and starts a run that is still live when ada's rung fires.
	h.signIn("fp-bob")
	h.advance(time.Minute)
	bobFloor := armGrokUsageRunFloor(h.clock())
	h.idle()
	h.pass() // account_changed: ada's debt is retired
	state := h.state()
	if state.owed() {
		t.Fatalf("state = %+v, want ada's debt retired", state)
	}
	if state.RunFloorMs != bobFloor.UnixMilli() || state.RunFloorAccount != "fp-bob" {
		t.Fatalf("state = %+v, want bob's live floor kept as the restart marker", state)
	}

	// The process dies before bob's run settles: the restart owes it a read.
	grokLiveRunsMu.Lock()
	grokLiveRuns = map[int64]int{}
	grokLiveRunAccounts = map[int64]string{}
	grokLiveRunsMu.Unlock()
	h.outcome.Store(grokLiveOutcomeOK)
	reads := h.reads.Load()
	h.advance(time.Second)
	payOwedGrokUsageRefresh()
	h.idle()
	if h.reads.Load() != reads+1 {
		t.Fatalf("reads = %d, want bob's interrupted run paid once after restart", h.reads.Load()-reads)
	}
}

// A debt retirement that read an old generation never deletes the debt a
// newer run opened while the retiring pass was out.
func TestGrokRunDebt_RetireLeavesANewerDebtGeneration(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.write(grokUsageFreshness{OwedAtMs: h.clock().UnixMilli(), CompletionMs: h.clock().UnixMilli(), AccountFingerprint: "fp-ada"})
	inspected := h.state().debtID()

	// The login switched to B and a B run settled before the retirement landed.
	h.signIn("fp-bob")
	h.advance(time.Second)
	h.write(grokUsageFreshness{OwedAtMs: h.clock().UnixMilli(), CompletionMs: h.clock().UnixMilli(), AccountFingerprint: "fp-bob"})
	grokRetireRunDebt(inspected, "account_changed", 0)
	if state := h.state(); !state.owed() || state.AccountFingerprint != "fp-bob" {
		t.Fatalf("state = %+v, want B's newer debt kept", state)
	}

	grokRetireRunDebt(h.state().debtID(), "account_changed", 0)
	if state := h.state(); state.owed() {
		t.Fatalf("state = %+v, want the inspected generation retired", state)
	}
}

// A run-completion read never joins a flight sent before the run completed:
// that response cannot include the run, so it must not pay the debt.
func TestGrokRunDebt_FlightSentBeforeCompletionNeverPaysTheDebt(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.block = make(chan struct{})
	floor := armGrokUsageRunFloor(h.clock())

	// A Refresh click sends its request while the run is still live.
	click := make(chan string, 1)
	go func() { click <- grokBillingReadOnce(context.Background(), "", "fp-ada", h.clock, time.Time{}) }()
	deadline := time.Now().Add(5 * time.Second)
	for h.reads.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	// The run completes; its debt worker finds the click's flight in the air.
	h.advance(time.Second)
	if !grokUsageRunSettled(floor) {
		t.Fatal("the run must owe a read")
	}
	time.Sleep(50 * time.Millisecond)
	close(h.block)
	h.idle()

	if got := <-click; got != grokLiveOutcomeOK {
		t.Fatalf("click = %q, want ok", got)
	}
	if got := h.reads.Load(); got != 2 {
		t.Fatalf("reads = %d, want the debt to send its own post-completion read", got)
	}
	if state := h.state(); state.owed() {
		t.Fatalf("state = %+v, want the post-completion read to pay the debt", state)
	}
	if snap, ok := loadGrokBillingLiveSnapshot("fp-ada"); !ok || snap.ObservedAt.Before(h.clock()) {
		t.Fatalf("live reading = %+v ok=%t, want one sent at or after completion", snap, ok)
	}
}

// Readings and completions are compared at millisecond precision. A flight
// sent earlier inside the millisecond a run completes in cannot include that
// run, so it must not pay the debt: the completion rounds UP.
func TestGrokRunDebt_FlightSentEarlierInTheCompletionMillisecondNeverPays(t *testing.T) {
	h := newGrokDebtHarness(t)
	h.block = make(chan struct{})
	floor := armGrokUsageRunFloor(h.clock())

	// A Refresh click sends its request 0.3 ms into the millisecond...
	h.advance(300 * time.Microsecond)
	click := make(chan string, 1)
	go func() { click <- grokBillingReadOnce(context.Background(), "", "fp-ada", h.clock, time.Time{}) }()
	deadline := time.Now().Add(5 * time.Second)
	for h.reads.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	// ...and the run completes 0.4 ms later, inside the same millisecond.
	h.advance(400 * time.Microsecond)
	if !grokUsageRunSettled(floor) {
		t.Fatal("the run must owe a read")
	}
	if got, want := h.state().CompletionMs, h.clock().Truncate(time.Millisecond).Add(time.Millisecond).UnixMilli(); got != want {
		t.Fatalf("CompletionMs = %d, want the completion rounded up to %d", got, want)
	}
	time.Sleep(50 * time.Millisecond)
	h.advance(time.Second)
	close(h.block)
	h.idle()

	if got := <-click; got != grokLiveOutcomeOK {
		t.Fatalf("click = %q, want ok", got)
	}
	if got := h.reads.Load(); got != 2 {
		t.Fatalf("reads = %d, want the debt to send its own post-completion read", got)
	}
	if state := h.state(); state.owed() {
		t.Fatalf("state = %+v, want the post-completion read to pay the debt", state)
	}
}

func TestGrokCompletionMs_RoundsUp(t *testing.T) {
	at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	if got := grokCompletionMs(at); got != at.UnixMilli() {
		t.Fatalf("whole millisecond = %d, want %d", got, at.UnixMilli())
	}
	if got := grokCompletionMs(at.Add(time.Nanosecond)); got != at.UnixMilli()+1 {
		t.Fatalf("sub-millisecond = %d, want %d", got, at.UnixMilli()+1)
	}
}

func TestGrokRunDebt_WaitsForTheRoundedCompletionBoundary(t *testing.T) {
	h := newGrokDebtHarness(t)
	floor := armGrokUsageRunFloor(h.clock())

	// The run completes 0.7 ms into a millisecond; its boundary rounds up.
	h.advance(700 * time.Microsecond)
	if !grokUsageRunSettled(floor) {
		t.Fatal("the run must owe a read")
	}
	time.Sleep(30 * time.Millisecond)
	if got := h.reads.Load(); got != 0 {
		t.Fatalf("reads = %d, want none before the rounded boundary", got)
	}

	// Once the clock reaches the boundary the read goes out and pays the debt.
	h.advance(300 * time.Microsecond)
	h.idle()
	if got := h.reads.Load(); got != 1 {
		t.Fatalf("reads = %d, want one read at the boundary", got)
	}
	if state := h.state(); state.owed() {
		t.Fatalf("state = %+v, want the boundary read to pay the debt", state)
	}
}

func TestGrokBillingReadOnce_ClockRollbackDoesNotWait(t *testing.T) {
	h := newGrokDebtHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// A completion a minute "in the future" is a clock step, not rounding.
	if got := grokBillingReadOnce(ctx, "", "fp-ada", h.clock, h.clock().Add(time.Minute)); got != grokLiveOutcomeOK {
		t.Fatalf("outcome = %q, want ok without waiting for the future completion", got)
	}
	if got := h.reads.Load(); got != 1 {
		t.Fatalf("reads = %d, want 1", got)
	}
}
