package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The run-completion debt state machine, without a spawn, a poller or the
// network. Every seam is pinned small here, so no case pays the shipped 60 s
// interval or the 5 s retry delay.

// helperIsolateAntigravityFreshness points the debt file, the quota cache and
// the Code Assist read at test-owned locations and shrinks the timing seams.
func helperIsolateAntigravityFreshness(t *testing.T) (state, cache string) {
	t.Helper()
	antigravityUsageRefreshWaitIdle()
	state = filepath.Join(t.TempDir(), "agy_freshness.json")
	cache = filepath.Join(t.TempDir(), "agyq.json")
	t.Setenv(antigravityFreshnessEnv, state)
	t.Setenv("AIEXPEDITE_AGY_QUOTA_CACHE", cache)
	helperIsolateAntigravityGate(t)

	origRetry, origInterval, origNow := antigravityRefreshAfterRunRetryDelay, antigravityRefreshMinInterval, antigravityUsageFreshnessNow
	t.Cleanup(func() {
		// The worker reads all three, so it has to be out of flight first.
		antigravityUsageRefreshWaitIdle()
		antigravityRefreshAfterRunRetryDelay = origRetry
		antigravityRefreshMinInterval = origInterval
		antigravityUsageFreshnessNow = origNow
	})
	antigravityRefreshAfterRunRetryDelay = time.Millisecond
	// `agy` has to look installed, or every debt retires before it is paid.
	helperFakeAgyOnPath(t)
	return state, cache
}

// helperFakeAgyOnPath puts a trivial `agy` first on PATH so the CLI looks
// installed and a debt is not retired before it can be paid. Deliberately not
// helperMockAgyOnPath: that copies the whole test binary (tens of MB) per call,
// which these cases pay for nothing — none of them spawns `agy`, and the debt
// worker only asks whether it EXISTS. A case that needs the version probe to
// answer must use the real mock binary instead: Windows cannot CreateProcess
// the `.cmd` written here.
func helperFakeAgyOnPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	name, body, mode := "agy", "#!/bin/sh\necho 'agy version 1.2.3'\n", os.FileMode(0o755)
	if runtime.GOOS == "windows" {
		name, body = "agy.cmd", "@echo off\r\necho agy version 1.2.3\r\n"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("write fake agy: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

// helperWriteAntigravityCache seeds a cached reading observed at `at`.
func helperWriteAntigravityCache(t *testing.T, cache string, at time.Time) {
	t.Helper()
	snap := antigravityQuotaSnapshot{
		ObservedAt:         at.UTC().Format(time.RFC3339),
		AccountFingerprint: fingerprintAccount("antigravity", "ada@example.com"),
		Account:            "ada@example.com",
		Buckets: []antigravityQuotaBucket{
			{Group: "Gemini Models", Window: "weekly", RemainingFraction: 0.4, ResetTime: "2126-08-14T00:00:00Z"},
		},
	}
	helperWriteJSON(t, cache, snap)
}

// A reading older than the run's floor leaves the debt standing; one at the
// floor retires it. The boundary is the whole settle decision — a flag would
// have made a pre-run reading look like the run's own.
func TestAntigravityFreshness_ReadingClearsOnlyAtOrAfterTheFloor(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	floor := time.Now().Truncate(time.Second)

	for _, tc := range []struct {
		name      string
		observed  time.Time
		wantOwing bool
	}{
		{"a reading from before the run", floor.Add(-time.Minute), true},
		{"a reading at exactly the floor", floor, false},
		{"a reading from during the run", floor.Add(30 * time.Second), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(antigravityFreshnessPath())
			helperWriteAntigravityCache(t, cache, tc.observed)
			calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })

			antigravityUsageRunSettled(floor, false, true)
			antigravityUsageRefreshWaitIdle()

			if owing := helperFreshnessState(t).RefreshOwedAtMs != 0; owing != tc.wantOwing {
				t.Errorf("owing=%v, want %v", owing, tc.wantOwing)
			}
			if tc.wantOwing && calls.Load() == 0 {
				t.Error("an unpaid run spent no Code Assist read")
			}
			if !tc.wantOwing && calls.Load() != 0 {
				t.Errorf("a covered run spent %d Code Assist reads", calls.Load())
			}
		})
	}
}

// The attempt cap: one immediate read and one retry, then the debt stops
// spending however long it stays unpaid.
func TestAntigravityFreshness_AttemptCapIsHonoured(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	floor := time.Now()
	helperWriteAntigravityCache(t, cache, floor.Add(-time.Hour))
	calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })
	antigravityRefreshMinInterval = time.Nanosecond

	antigravityUsageRunSettled(floor, false, true)
	antigravityUsageRefreshWaitIdle()
	if got := calls.Load(); got != int64(antigravityRefreshAfterRunMaxAttempts) {
		t.Fatalf("reads=%d, want %d", got, antigravityRefreshAfterRunMaxAttempts)
	}
	state := helperFreshnessState(t)
	if state.RefreshOwedAtMs == 0 || state.Attempts != antigravityRefreshAfterRunMaxAttempts {
		t.Fatalf("state=%+v, want the debt kept with its budget spent", state)
	}

	// A second worker on the same debt must spend nothing further.
	antigravityStartRunDebtWorker(antigravityRefreshAfterRunMaxAttempts, true)
	antigravityUsageRefreshWaitIdle()
	if got := calls.Load(); got != int64(antigravityRefreshAfterRunMaxAttempts) {
		t.Errorf("reads=%d after a second worker, want the cap to hold", got)
	}
}

// The minimum interval spaces the outbound call a NEW run's debt triggers. A
// burst of short runs therefore costs at most one read per interval.
func TestAntigravityFreshness_MinimumIntervalBlocksTheNextRunsPayment(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	helperWriteAntigravityCache(t, cache, time.Now().Add(-time.Hour))
	calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistNotSigned })

	antigravityUsageRunSettled(time.Now(), false, true)
	antigravityUsageRefreshWaitIdle()
	first := calls.Load()
	if first == 0 {
		t.Fatal("the first run spent no read")
	}

	// A second run seconds later: the debt's floor advances, its budget resets,
	// and the interval — not the cap — is what holds the outbound call back.
	antigravityUsageRunSettled(time.Now(), false, true)
	antigravityUsageRefreshWaitIdle()
	if got := calls.Load(); got != first {
		t.Errorf("reads=%d, want the interval to block the second run's payment (was %d)", got, first)
	}
	if state := helperFreshnessState(t); state.RefreshOwedAtMs == 0 {
		t.Error("the blocked debt was dropped instead of kept for the next run")
	}
}

// A debt whose worker is already in flight must not start a second one: the
// debt they would both pay is the same unpaid run.
func TestAntigravityFreshness_SingleFlightBlocksAConcurrentPayment(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	helperWriteAntigravityCache(t, cache, time.Now().Add(-time.Hour))
	antigravityRefreshMinInterval = time.Nanosecond
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	calls := helperStubAntigravityCodeAssistOutcome(t, func() string {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return liveProbeOutcomeCodeAssistHTTPError
	})

	antigravityUsageRunSettled(time.Now(), false, true)
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the debt worker never started")
	}
	antigravityStartRunDebtWorker(antigravityRefreshAfterRunMaxAttempts, true)
	antigravityStartRunDebtWorker(antigravityRefreshAfterRunMaxAttempts, true)
	if got := calls.Load(); got != 1 {
		t.Errorf("reads=%d while one worker is in flight, want 1", got)
	}
	close(release)
	antigravityUsageRefreshWaitIdle()
}

// no_login stops attempting at once (nothing on this machine can pay it) but
// keeps the debt, so the card can say why the reading is not moving.
// token_expired keeps both the debt AND its budget: the next real run refreshes
// the keyring token for free, and the worker must never spawn `agy` to do it.
func TestAntigravityFreshness_LocalRefusalsDecideWhetherToKeepSpending(t *testing.T) {
	for _, tc := range []struct {
		outcome      string
		wantAttempts int
		wantReads    int64
	}{
		{liveProbeOutcomeCodeAssistNoLogin, antigravityRefreshAfterRunMaxAttempts, 1},
		{liveProbeOutcomeCodeAssistTokenExpired, 0, 1},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			_, cache := helperIsolateAntigravityFreshness(t)
			helperWriteAntigravityCache(t, cache, time.Now().Add(-time.Hour))
			calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return tc.outcome })

			antigravityUsageRunSettled(time.Now(), false, true)
			antigravityUsageRefreshWaitIdle()

			state := helperFreshnessState(t)
			if state.RefreshOwedAtMs == 0 {
				t.Fatalf("%s dropped the debt", tc.outcome)
			}
			if state.Attempts != tc.wantAttempts {
				t.Errorf("attempts=%d, want %d", state.Attempts, tc.wantAttempts)
			}
			if got := calls.Load(); got != tc.wantReads {
				t.Errorf("reads=%d, want %d", got, tc.wantReads)
			}
			if state.LastPaidAtMs != 0 {
				t.Error("a local refusal sent no request and must not space the next one")
			}
		})
	}
}

// A debt nothing could pay must age out rather than pin a worker or a warning
// forever.
func TestAntigravityFreshness_DebtPastMaxAgeRetires(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	now := time.Now()
	helperWriteAntigravityCache(t, cache, now.Add(-24*time.Hour))
	stale := now.Add(-antigravityRefreshOwedMaxAge - time.Minute)
	helperWriteJSON(t, antigravityFreshnessPath(), antigravityUsageFreshness{
		SchemaVersion:      antigravityFreshnessSchema,
		RefreshOwedFloorMs: stale.UnixMilli(),
		RefreshOwedAtMs:    stale.UnixMilli(),
		Attempts:           antigravityRefreshAfterRunMaxAttempts,
		Outcome:            liveProbeOutcomeCodeAssistHTTPError,
	})

	notice, pending := antigravityFreshnessNotice(now.Add(-24*time.Hour).Format(time.RFC3339), now)
	if pending || notice != "" {
		t.Errorf("notice=%q pending=%v, want an aged-out debt to warn about nothing", notice, pending)
	}

	calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })
	antigravityStartRunDebtWorker(antigravityRefreshAfterRunMaxAttempts, true)
	antigravityUsageRefreshWaitIdle()
	if calls.Load() != 0 {
		t.Errorf("reads=%d, want an aged-out debt to spend nothing", calls.Load())
	}
	if state := helperFreshnessState(t); state.RefreshOwedAtMs != 0 {
		t.Errorf("state=%+v, want the aged-out debt retired", state)
	}
}

// A floor further ahead of `now` than the local skew is a backwards clock step.
// Parked in the future it would make every later run owe a debt no reading can
// cover, so it is discarded.
func TestAntigravityFreshness_FutureFloorIsDiscardedAsAClockRollback(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	now := time.Now()
	helperWriteAntigravityCache(t, cache, now)
	future := now.Add(antigravityRunFloorLocalSkew + time.Hour)
	helperWriteJSON(t, antigravityFreshnessPath(), antigravityUsageFreshness{
		SchemaVersion:      antigravityFreshnessSchema,
		RunFloorMs:         future.UnixMilli(),
		RefreshOwedFloorMs: future.UnixMilli(),
		RefreshOwedAtMs:    future.UnixMilli(),
	})

	if _, pending := antigravityFreshnessNotice(now.Format(time.RFC3339), now); pending {
		t.Error("a rolled-back clock left a debt pending against a floor nothing can cover")
	}
	// Arming rewrites the floor at the current clock rather than keeping the
	// future one.
	floor := armAntigravityUsageRunFloor(now)
	antigravityUsageRefreshWaitIdle()
	if state := helperFreshnessState(t); state.RunFloorMs != floor.UnixMilli() {
		t.Errorf("runFloorMs=%d, want the rearmed %d", state.RunFloorMs, floor.UnixMilli())
	}
}

// A corrupt or truncated state file is "no debt", never a panic: this is a
// freshness optimisation and the next run rewrites it.
func TestAntigravityFreshness_CorruptStateFileReadsAsNoDebt(t *testing.T) {
	statePath, _ := helperIsolateAntigravityFreshness(t)
	if err := os.WriteFile(statePath, []byte(`{"refreshOwedAtMs":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if notice, pending := antigravityFreshnessNotice("", time.Now()); pending || notice != "" {
		t.Errorf("notice=%q pending=%v, want a corrupt file to read as no debt", notice, pending)
	}
	floor := armAntigravityUsageRunFloor(time.Now())
	antigravityUsageRefreshWaitIdle()
	if state := helperFreshnessState(t); state.RunFloorMs != floor.UnixMilli() {
		t.Errorf("runFloorMs=%d, want the corrupt file replaced by a real floor", state.RunFloorMs)
	}
}

// An offline agent makes no outbound request at all; the debt waits for the
// next run rather than retiring, because offline is temporary.
func TestAntigravityFreshness_OfflineKeepsTheDebtAndSpendsNothing(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	helperWriteAntigravityCache(t, cache, time.Now().Add(-time.Hour))
	calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistOK })

	offlineMutex.Lock()
	wasOffline := isOffline
	isOffline = true
	offlineMutex.Unlock()
	t.Cleanup(func() {
		offlineMutex.Lock()
		isOffline = wasOffline
		offlineMutex.Unlock()
	})

	antigravityUsageRunSettled(time.Now(), false, true)
	antigravityUsageRefreshWaitIdle()
	if calls.Load() != 0 {
		t.Errorf("reads=%d while offline, want none", calls.Load())
	}
	state := helperFreshnessState(t)
	if state.RefreshOwedAtMs == 0 || state.Attempts != 0 {
		t.Errorf("state=%+v, want the debt kept with no attempt consumed", state)
	}
}

// An uninstall between the run and the payment retires the debt without an
// attempt: neither a retry nor a notice belongs to a provider the card no
// longer shows.
func TestAntigravityFreshness_UninstalledAgyRetiresTheDebt(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	helperWriteAntigravityCache(t, cache, time.Now().Add(-time.Hour))
	calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistOK })
	// An empty PATH and an installer dir that holds nothing: `agy` is gone.
	t.Setenv("PATH", t.TempDir())

	antigravityUsageRunSettled(time.Now(), false, true)
	antigravityUsageRefreshWaitIdle()
	if calls.Load() != 0 {
		t.Errorf("reads=%d for an uninstalled CLI, want none", calls.Load())
	}
	if state := helperFreshnessState(t); state.RefreshOwedAtMs != 0 {
		t.Errorf("state=%+v, want the debt retired", state)
	}
	if notice, pending := antigravityFreshnessNotice("", time.Now()); pending || notice != "" {
		t.Errorf("notice=%q pending=%v, want nothing said about a CLI that is gone", notice, pending)
	}
}

// The notice accessor is the single source for both questions the parser asks.
// A gated debt reports pending with NO wording — antigravityGateNotice already
// names the build and the reading's age, and two sources for one banner drift.
func TestAntigravityFreshnessNotice_WordsOnlyTheNonGatedCase(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	floor := now.Add(-5 * time.Minute)
	lastObserved := now.Add(-48 * time.Hour).Format(time.RFC3339)

	for _, tc := range []struct {
		name        string
		state       antigravityUsageFreshness
		wantPending bool
		wantNotice  []string
	}{
		{
			name:        "a gated debt leaves the wording to the gate banner",
			state:       antigravityUsageFreshness{Gated: true, Attempts: antigravityRefreshAfterRunMaxAttempts, Outcome: liveProbeOutcomeCodeAssistHTTPError},
			wantPending: true,
		},
		{
			name:        "a debt with attempts left says nothing yet",
			state:       antigravityUsageFreshness{Attempts: 0},
			wantPending: true,
		},
		{
			name:        "a spent no_login debt names the missing login",
			state:       antigravityUsageFreshness{Attempts: antigravityRefreshAfterRunMaxAttempts, Outcome: liveProbeOutcomeCodeAssistNoLogin},
			wantPending: true,
			wantNotice:  []string{"No Antigravity login is stored", "2026-09-18 12:00 UTC", "2026-09-20 11:55 UTC"},
		},
		{
			name:        "a spent failing debt names the stored login",
			state:       antigravityUsageFreshness{Attempts: antigravityRefreshAfterRunMaxAttempts, Outcome: liveProbeOutcomeCodeAssistHTTPError},
			wantPending: true,
			wantNotice:  []string{"Google returned no reading", "2026-09-20 11:55 UTC"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			helperIsolateAntigravityFreshness(t)
			state := tc.state
			state.SchemaVersion = antigravityFreshnessSchema
			state.RefreshOwedFloorMs = floor.UnixMilli()
			state.RefreshOwedAtMs = floor.UnixMilli()
			helperWriteJSON(t, antigravityFreshnessPath(), state)

			notice, pending := antigravityFreshnessNotice(lastObserved, now)
			if pending != tc.wantPending {
				t.Errorf("pending=%v, want %v", pending, tc.wantPending)
			}
			if len(tc.wantNotice) == 0 {
				if notice != "" {
					t.Errorf("notice=%q, want none", notice)
				}
				return
			}
			for _, want := range tc.wantNotice {
				if !strings.Contains(notice, want) {
					t.Errorf("notice=%q, want it to contain %q", notice, want)
				}
			}
			// Timestamps and fixed text only.
			for _, forbidden := range []string{"@", "http", "token", string(os.PathSeparator) + "Users"} {
				if strings.Contains(notice, forbidden) {
					t.Errorf("notice=%q leaked %q", notice, forbidden)
				}
			}
		})
	}
}

// A debt the previous process never settled is adopted and paid ONCE at
// startup, bypassing the interval: nothing in this fresh process has read yet.
func TestPayOwedAntigravityUsageRefresh_AdoptsAnUnsettledFloor(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	now := time.Now()
	helperWriteAntigravityCache(t, cache, now.Add(-time.Hour))
	helperWriteJSON(t, antigravityFreshnessPath(), antigravityUsageFreshness{
		SchemaVersion: antigravityFreshnessSchema,
		RunFloorMs:    now.Add(-time.Minute).UnixMilli(),
		// A read seconds ago would block a NEW debt; a startup adoption
		// bypasses the interval.
		LastPaidAtMs: now.Add(-time.Second).UnixMilli(),
	})
	calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })

	payOwedAntigravityUsageRefresh()
	antigravityUsageRefreshWaitIdle()

	if got := calls.Load(); got != 1 {
		t.Fatalf("reads=%d, want exactly one bounded attempt at startup", got)
	}
	state := helperFreshnessState(t)
	if state.RefreshOwedAtMs == 0 || state.RefreshOwedFloorMs != now.Add(-time.Minute).UnixMilli() {
		t.Errorf("state=%+v, want the interrupted run's floor adopted as a debt", state)
	}
}

// Startup must not resurrect a run older than the age-out, and must spend
// nothing when a reading already covers the adopted floor.
func TestPayOwedAntigravityUsageRefresh_SkipsWhatItCannotOrNeedNotPay(t *testing.T) {
	t.Run("a floor older than the age-out", func(t *testing.T) {
		_, cache := helperIsolateAntigravityFreshness(t)
		now := time.Now()
		helperWriteAntigravityCache(t, cache, now.Add(-24*time.Hour))
		helperWriteJSON(t, antigravityFreshnessPath(), antigravityUsageFreshness{
			SchemaVersion: antigravityFreshnessSchema,
			RunFloorMs:    now.Add(-antigravityRefreshOwedMaxAge - time.Minute).UnixMilli(),
		})
		calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistOK })

		payOwedAntigravityUsageRefresh()
		antigravityUsageRefreshWaitIdle()
		if calls.Load() != 0 {
			t.Errorf("reads=%d, want none for a run too old to matter", calls.Load())
		}
	})

	t.Run("a floor a run of THIS process armed", func(t *testing.T) {
		_, cache := helperIsolateAntigravityFreshness(t)
		now := time.Now()
		helperWriteAntigravityCache(t, cache, now.Add(-time.Hour))
		calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistOK })

		// The replay is spawned, so a session can arm between StartAgent asking
		// and the goroutine reading its state. Pin that ordering rather than
		// racing it: a floor stamped after this call's own instant is a run
		// this process armed. Converting it would book a completion time for a
		// run that is still going, and the reading it triggered — taken at the
		// run's START — would then satisfy the run's own settle, leaving the
		// finished run with no refresh at all.
		helperWriteJSON(t, antigravityFreshnessPath(), antigravityUsageFreshness{
			SchemaVersion: antigravityFreshnessSchema,
			RunFloorMs:    now.Add(500 * time.Millisecond).UnixMilli(),
		})
		payOwedAntigravityUsageRefresh()
		antigravityUsageRefreshWaitIdle()

		if calls.Load() != 0 {
			t.Errorf("reads=%d, want none for a run that is still running", calls.Load())
		}
		state := helperFreshnessState(t)
		if state.RefreshOwedAtMs != 0 {
			t.Errorf("state=%+v, want a live run's floor left to its own settle", state)
		}
		if state.RunFloorMs == 0 {
			t.Error("the live run's floor was dropped")
		}
	})

	t.Run("a floor a reading already covers", func(t *testing.T) {
		_, cache := helperIsolateAntigravityFreshness(t)
		now := time.Now()
		helperWriteAntigravityCache(t, cache, now)
		helperWriteJSON(t, antigravityFreshnessPath(), antigravityUsageFreshness{
			SchemaVersion: antigravityFreshnessSchema,
			RunFloorMs:    now.Add(-time.Minute).UnixMilli(),
		})
		calls := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistOK })

		payOwedAntigravityUsageRefresh()
		antigravityUsageRefreshWaitIdle()
		if calls.Load() != 0 {
			t.Errorf("reads=%d, want none when the run already has its reading", calls.Load())
		}
	})
}

// Every route that lands a reading is a settler: a Code Assist read, a Refresh
// click and a concurrent run's poller all go through
// writeAntigravityQuotaSnapshotLocked, so the debt is retired exactly once by
// whichever of them covers the floor — with no call back into the freshness
// worker.
func TestSettleAntigravityRunFreshness_AnyPersistedReadingRetiresTheDebt(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	floor := time.Now()
	helperWriteAntigravityCache(t, cache, floor.Add(-time.Hour))
	helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })
	antigravityUsageRunSettled(floor, false, true)
	antigravityUsageRefreshWaitIdle()
	if helperFreshnessState(t).RefreshOwedAtMs == 0 {
		t.Fatal("no debt to retire")
	}

	snap := antigravityQuotaSnapshot{
		ObservedAt:         floor.Add(time.Second).UTC().Format(time.RFC3339),
		AccountFingerprint: fingerprintAccount("antigravity", "ada@example.com"),
		Account:            "ada@example.com",
		Buckets: []antigravityQuotaBucket{
			{Group: "Gemini Models", Window: "weekly", RemainingFraction: 0.3, ResetTime: "2126-08-14T00:00:00Z"},
		},
	}
	if !saveAntigravityQuotaSnapshotIfNewer(snap) {
		t.Fatal("the reading was not persisted")
	}
	state := helperFreshnessState(t)
	if state.RefreshOwedAtMs != 0 || state.RunFloorMs != 0 {
		t.Errorf("state=%+v, want the landed reading to retire both the debt and the floor", state)
	}
}

// The redaction contract for the state file, asserted on the SERIALIZED bytes:
// timestamps, counters and a hashed fingerprint, never a credential, an
// account, a port, a path or a command.
func TestAntigravityFreshness_StateFileCarriesNothingIdentifying(t *testing.T) {
	statePath, cache := helperIsolateAntigravityFreshness(t)
	helperWriteAntigravityCache(t, cache, time.Now().Add(-time.Hour))
	helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })

	antigravityUsageRunSettled(time.Now(), false, true)
	antigravityUsageRefreshWaitIdle()

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	body := string(raw)
	// "http" alone would match the closed-set outcome code codeassist_http_error.
	for _, forbidden := range []string{"ada@example.com", "access_token", "Bearer", "http://", "https://", "agy", "127.0.0.1", statePath, cache} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the freshness state leaked %q:\n%s", forbidden, body)
		}
	}
	allowed := map[string]bool{
		"schemaVersion": true, "runFloorMs": true, "refreshOwedFloorMs": true,
		"refreshOwedAtMs": true, "lastPaidAtMs": true, "attempts": true,
		"gated": true, "accountFingerprint": true, "outcome": true,
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("state is not a JSON object: %v", err)
	}
	for key := range decoded {
		if !allowed[key] {
			t.Errorf("unexpected persisted field %q", key)
		}
	}
	// The outcome is one of the closed Code Assist codes, never free text.
	if outcome, _ := decoded["outcome"].(string); outcome != "" && !strings.HasPrefix(outcome, "codeassist_") {
		t.Errorf("outcome=%q, want a closed-set code", outcome)
	}
}

// probeAntigravityQuotaCodeAssistFn is the ONLY outbound call the worker makes,
// and it must be reachable with no detectedCLIAgent: the version the request
// identifies itself as comes from the same cached probe detection uses.
func TestAntigravityCodeAssistBuildVersion_FallsBackToTheInstalledBinary(t *testing.T) {
	helperIsolateAntigravityFreshness(t)
	// The one case that needs a REAL executable: helperFakeAgyOnPath's shell
	// script is enough for "is agy installed", but Windows cannot CreateProcess
	// a .cmd directly (the same reason grokProbeVersion exists), so the probe
	// would answer "" there and this assertion would pass only on Unix. The
	// mock binary is a genuine .exe on every platform.
	helperMockAgyOnPath(t, "antigravity-diagnostic")
	if got := antigravityCodeAssistBuildVersion("1.2.4"); got != "1.2.4" {
		t.Errorf("version=%q, want the detected one kept", got)
	}
	resetVersionProbeCache()
	if got := antigravityCodeAssistBuildVersion(""); !strings.Contains(got, "1.2.3") {
		t.Errorf("version=%q, want it probed off the installed binary", got)
	}
	t.Setenv("PATH", t.TempDir())
	if got := antigravityCodeAssistBuildVersion(""); got != "" {
		t.Errorf("version=%q, want empty when nothing is installed", got)
	}
	// And the User-Agent still names a build, so a licensed request is never
	// sent with Go's default header.
	if ua := antigravityCodeAssistUserAgent(""); !strings.HasPrefix(ua, "antigravity/cli/") {
		t.Errorf("user agent=%q", ua)
	}
}
