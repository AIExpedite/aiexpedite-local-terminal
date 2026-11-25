package main

// Pins how resetClaudeUsageProbeGate contains work it could not drain. The
// full Windows suite failed the post-run freshness tests while each passed on
// its own: a reset that timed out used to zero inFlight / settling under a live
// straggler, so the straggler's late writes landed in the NEXT test's gate. A
// reset that cannot drain now cancels the straggler, abandons the background
// work outside the brackets, and leaves the brackets standing.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// pinClaudeDrainTimeout shortens the reset's drain so a test can wedge it
// without paying the production five seconds, twice.
func pinClaudeDrainTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	original := claudeUsageProbeDrainTimeout
	claudeUsageProbeDrainTimeout = d
	t.Cleanup(func() { claudeUsageProbeDrainTimeout = original })
}

// wedgeClaudeSettlement holds one settlement open, as a straggling post-run
// attempt does, and returns its release. The release is also registered as a
// cleanup, so the wedge never outlives the test whatever path it fails on;
// registered after armClaudeUsageProbe, it runs before that cleanup's reset.
func wedgeClaudeSettlement(t *testing.T) (release func()) {
	t.Helper()
	claudeUsageProbe.beginSettling()
	var once atomic.Bool
	release = func() {
		if once.CompareAndSwap(false, true) {
			claudeUsageProbe.endSettling()
		}
	}
	t.Cleanup(release)
	return release
}

func claudeGateSettling() int {
	claudeUsageProbe.mu.Lock()
	defer claudeUsageProbe.mu.Unlock()
	return claudeUsageProbe.settling
}

func TestResetClaudeUsageProbeGate_LateEndSettlingCannotChangeTheNewCounter(t *testing.T) {
	armClaudeUsageProbe(t, unreachableProbeHandler)
	pinClaudeDrainTimeout(t, 50*time.Millisecond)
	release := wedgeClaudeSettlement(t)

	if resetClaudeUsageProbeGate() {
		t.Fatal("the reset reported a drain while a settlement was still running")
	}
	if got := claudeGateSettling(); got != 1 {
		t.Fatalf("settling=%d after an undrained reset, want the straggler's 1 left standing", got)
	}
	release()
	if got := claudeGateSettling(); got != 0 {
		t.Fatalf("settling=%d after the straggler ended, want 0 — never below", got)
	}
	if !resetClaudeUsageProbeGate() {
		t.Fatal("the reset still could not drain once the straggler ended")
	}
}

func TestResetClaudeUsageProbeGate_StaleHolderCannotClearOrRestoreTheNextLatch(t *testing.T) {
	armClaudeUsageProbe(t, unreachableProbeHandler)
	pinClaudeDrainTimeout(t, 50*time.Millisecond)

	// The straggler: admitted, so it holds the latch and charged lastAttempt.
	if code := claudeUsageProbe.admit(time.Now(), false); code != "" {
		t.Fatalf("precondition: admit refused as %q", code)
	}
	finished := false
	t.Cleanup(func() {
		if !finished {
			claudeUsageProbe.finish(claudeProbeResult{code: claudeProbeCancelled}, false, time.Time{}, "")
		}
	})

	if resetClaudeUsageProbeGate() {
		t.Fatal("the reset reported a drain while a probe held the latch")
	}
	// Re-armed as the next test would be: the latch must still refuse, because
	// the straggler still holds it — admitting a second holder beside it is
	// exactly what a force-cleared latch allowed.
	SetClaudeUsageProbeDisabled(false)
	if code := claudeUsageProbe.admit(time.Now(), false); code != claudeProbeInFlight {
		t.Fatalf("a second holder was admitted as %q beside the straggler, want %q", code, claudeProbeInFlight)
	}
	if leak := claudeGateLeak(); !strings.Contains(leak, "single-flight latch") {
		t.Fatalf("claudeGateLeak()=%q, want it to name the held latch", leak)
	}

	// The straggler gives its charge back and releases the latch.
	claudeUsageProbe.refundAdmission()
	claudeUsageProbe.finish(claudeProbeResult{code: claudeProbeCancelled}, false, time.Time{}, "")
	finished = true

	requireClaudeGateQuiet(t)
	if !resetClaudeUsageProbeGate() {
		t.Fatal("the reset could not drain once the straggler finished")
	}
	SetClaudeUsageProbeDisabled(false)
	claudeUsageProbe.mu.Lock()
	lastAttempt, inFlight := claudeUsageProbe.lastAttempt, claudeUsageProbe.inFlight
	claudeUsageProbe.mu.Unlock()
	if !lastAttempt.IsZero() || inFlight {
		t.Fatalf("after the quiet reset lastAttempt=%v inFlight=%v, want a clean latch", lastAttempt, inFlight)
	}
}

func TestResetClaudeUsageProbeGate_StaleSettleOwedCannotClearANewerDebt(t *testing.T) {
	armClaudeUsageProbe(t, unreachableProbeHandler)
	pinClaudeDrainTimeout(t, 50*time.Millisecond)
	release := wedgeClaudeSettlement(t)
	stale := time.Now().Add(-time.Minute)
	claudeUsageProbe.recordOwed(stale)

	if resetClaudeUsageProbeGate() {
		t.Fatal("the reset reported a drain while a settlement was still running")
	}
	// The next test cannot arm beside the straggler.
	if leak := waitClaudeGate(20*time.Millisecond, claudeGateLeak); !strings.Contains(leak, "settlement") {
		t.Fatalf("claudeGateLeak()=%q while the straggler settles, want it to name the settlement", leak)
	}
	// The straggler settles the debt it was paying, then ends.
	claudeUsageProbe.settleOwed(time.Now())
	release()

	requireClaudeGateQuiet(t)
	if !resetClaudeUsageProbeGate() {
		t.Fatal("the reset could not drain once the straggler ended")
	}
	SetClaudeUsageProbeDisabled(false)
	newer := time.Now()
	claudeUsageProbe.recordOwed(newer)
	if owed := claudeUsageProbe.owedObservation(); !owed.Equal(newer) {
		t.Fatalf("owed=%v, want the new generation's debt %v still owed", owed, newer)
	}
}

// The straggler ends DURING the reset's second wait — once the cancel reached
// it — after recording a hold and booking a rung. The reset reports the failed
// drain, but nothing the straggler wrote survives into the re-armed gate.
func TestResetClaudeUsageProbeGate_StaleHoldAndRungCannotLandAfterReArm(t *testing.T) {
	armClaudeUsageProbe(t, unreachableProbeHandler)
	pinClaudeDrainTimeout(t, 100*time.Millisecond)
	release := wedgeClaudeSettlement(t)

	gen := claudeUsageProbe.generationContext()
	var fired atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-gen.Done():
		case <-time.After(10 * time.Second):
		}
		claudeUsageProbe.holdUntil(time.Now().Add(time.Hour))
		claudeArmRunDebtRetryFn(300*time.Millisecond, func() { fired.Add(1) })
		release()
	}()

	if resetClaudeUsageProbeGate() {
		t.Fatal("the reset reported a drain while a settlement was still running")
	}
	<-done
	requireClaudeGateQuiet(t)
	if !resetClaudeUsageProbeGate() {
		t.Fatal("the reset could not drain a quiet gate")
	}
	SetClaudeUsageProbeDisabled(false)

	claudeUsageProbe.mu.Lock()
	held := claudeUsageProbe.heldUntil
	claudeUsageProbe.mu.Unlock()
	if !held.IsZero() {
		t.Errorf("heldUntil=%v after the re-arm, want the straggler's hold cleared", held)
	}
	if claudeRunDebtRetryPending() {
		t.Error("the straggler's rung is still armed after the re-arm")
	}
	time.Sleep(500 * time.Millisecond)
	if n := fired.Load(); n != 0 {
		t.Errorf("the straggler's rung fired %d time(s) into the re-armed gate", n)
	}
}

func TestResetClaudeUsageProbeGate_CancelStopsTheStragglerBeforeTheEnvIsRestored(t *testing.T) {
	t.Run("request in flight", func(t *testing.T) {
		sawCancel := make(chan struct{})
		armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
				close(sawCancel)
			case <-time.After(10 * time.Second):
			}
		})
		pinClaudeDrainTimeout(t, 100*time.Millisecond)

		done := make(chan claudeProbeResult, 1)
		go func() {
			result, _, _ := probeClaudeUsageResult(context.Background(), time.Now(), claudeUsageProbeStoredIdentity, time.Time{}, false)
			done <- result
		}()
		waitForClaudeProbeInFlight(t)
		// Let the request reach the handler before the reset gives up on it.
		time.Sleep(50 * time.Millisecond)

		resetClaudeUsageProbeGate()
		select {
		case <-sawCancel:
		case <-time.After(5 * time.Second):
			t.Fatal("the handler never saw the request context end — the reset did not cancel the straggler")
		}
		select {
		case result := <-done:
			if result.code == claudeProbeOK {
				t.Errorf("the cancelled probe reported %q", result.code)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the cancelled probe never returned")
		}
	})

	t.Run("before the endpoint is resolved", func(t *testing.T) {
		_, calls := armClaudeUsageProbe(t, claudeProbeOKHandler)
		pinClaudeDrainTimeout(t, 50*time.Millisecond)
		var restoredCalls atomic.Int64
		restored := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			restoredCalls.Add(1)
			claudeProbeOKHandler(w, nil)
		}))
		t.Cleanup(restored.Close)

		admitted, unblock := make(chan struct{}), make(chan struct{})
		t.Cleanup(func() {
			select {
			case <-unblock:
			default:
				close(unblock)
			}
		})
		done := make(chan claudeProbeResult, 1)
		go func() {
			result, _, _ := probeClaudeUsageResult(context.Background(), time.Now(), func() claudeUsageProbeIdentity {
				close(admitted)
				<-unblock
				return claudeUsageProbeStoredIdentity()
			}, time.Time{}, false)
			done <- result
		}()
		<-admitted

		if resetClaudeUsageProbeGate() {
			t.Fatal("the reset reported a drain while a probe held the latch")
		}
		// The environment moves on to the "next test", then the straggler resumes.
		t.Setenv(claudeUsageProbeEndpointEnv, restored.URL)
		close(unblock)
		var result claudeProbeResult
		select {
		case result = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the straggler never returned")
		}
		if result.code != claudeProbeCancelled || result.issued {
			t.Errorf("straggler result code=%q issued=%v, want %q and nothing issued", result.code, result.issued, claudeProbeCancelled)
		}
		if n := restoredCalls.Load(); n != 0 {
			t.Errorf("the straggler sent %d request(s) to the restored endpoint", n)
		}
		if n := atomic.LoadInt64(calls); n != 0 {
			t.Errorf("the straggler sent %d request(s) to its own endpoint after the cancel", n)
		}
	})
}

func TestResetClaudeUsageProbeGate_FailedDrainAbandonsTheAuthClearLoop(t *testing.T) {
	armClaudeUsageProbe(t, unreachableProbeHandler)
	pinClaudeDrainTimeout(t, 50*time.Millisecond)

	prevDelays, prevRetry := claudePendingAuthClearRetryDelays, claudePendingAuthClearRetry
	t.Cleanup(func() { claudePendingAuthClearRetryDelays, claudePendingAuthClearRetry = prevDelays, prevRetry })
	claudePendingAuthClearRetryDelays = []time.Duration{300 * time.Millisecond, 300 * time.Millisecond}
	var retries atomic.Int64
	claudePendingAuthClearRetry = func() bool {
		retries.Add(1)
		return claudeRetryPendingAuthClear()
	}

	claudeQueuePendingAuthClear(currentClaudeAccountFingerprint(), claudeCredStamp{modNs: 1, size: 1})
	if leak := claudeGateLeak(); !strings.Contains(leak, "auth-clear") {
		t.Fatalf("claudeGateLeak()=%q, want it to name the running auth-clear loop", leak)
	}
	release := wedgeClaudeSettlement(t)
	if resetClaudeUsageProbeGate() {
		t.Fatal("the reset reported a drain while a settlement was still running")
	}
	restoredCache := filepath.Join(t.TempDir(), "restored-rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", restoredCache)

	time.Sleep(900 * time.Millisecond)
	if n := retries.Load(); n != 0 {
		t.Errorf("the abandoned auth-clear loop retried %d time(s) after the failed reset", n)
	}
	if _, err := os.Stat(restoredCache); !os.IsNotExist(err) {
		t.Errorf("the abandoned loop touched the restored cache path (stat err=%v)", err)
	}
	claudeUsageProbe.mu.Lock()
	retrying := claudeUsageProbe.authClearRetrying
	claudeUsageProbe.mu.Unlock()
	if retrying {
		t.Error("authClearRetrying is still set after the failed reset abandoned the loop")
	}
	release()
}

func TestResetClaudeUsageProbeGate_LateSeedCannotMarkTheNewGeneration(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	pinClaudeDrainTimeout(t, 50*time.Millisecond)
	fp := currentClaudeAccountFingerprint()
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Minute))

	claimed, unblock := make(chan struct{}), make(chan struct{})
	original := claudeUsageProbeAfterSeedRead
	t.Cleanup(func() { claudeUsageProbeAfterSeedRead = original })
	var once atomic.Bool
	claudeUsageProbeAfterSeedRead = func() {
		if once.CompareAndSwap(false, true) {
			close(claimed)
			<-unblock
		}
	}
	t.Cleanup(func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		claudeUsageProbe.seedOwedFromCache(context.Background(), fp, probeTestToken,
			claudeUsageProbe.refreshGeneration(), time.Now(), time.Time{})
	}()
	<-claimed

	release := wedgeClaudeSettlement(t)
	if resetClaudeUsageProbeGate() {
		t.Fatal("the reset reported a drain while a settlement was still running")
	}
	close(unblock)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the held seed never returned")
	}
	claudeUsageProbe.mu.Lock()
	seeded, seededFor := claudeUsageProbe.owedSeeded, claudeUsageProbe.owedSeededFor
	claudeUsageProbe.mu.Unlock()
	if seeded || seededFor != "" {
		t.Errorf("a seed the reset abandoned marked the gate seeded (owedSeeded=%v, for set=%v)", seeded, seededFor != "")
	}
	release()
}

// The quiet check names the condition that still holds, so a leak fails the
// next test with a cause rather than as a timed-out freshness wait.
func TestRequireClaudeGateQuiet_NamesEachLeak(t *testing.T) {
	armClaudeUsageProbe(t, unreachableProbeHandler)
	for _, tc := range []struct {
		name, want string
		leak       func(t *testing.T)
	}{
		{"offline flag", "offline", func(t *testing.T) {
			SetOffline(true)
			t.Cleanup(func() { SetOffline(false) })
		}},
		{"shutdown flag", "shutdown", func(t *testing.T) {
			shutdownInProgress.Store(true)
			t.Cleanup(func() { shutdownInProgress.Store(false) })
		}},
		{"armed rung", "rung", func(t *testing.T) {
			claudeArmRunDebtRetryFn(time.Hour, func() {})
			t.Cleanup(stopClaudeRunDebtRetry)
		}},
		{"live settlement", "settlement", func(t *testing.T) { wedgeClaudeSettlement(t) }},
		{"auth-clear loop", "auth-clear", func(t *testing.T) {
			prev := claudePendingAuthClearRetryDelays
			t.Cleanup(func() { claudePendingAuthClearRetryDelays = prev })
			claudePendingAuthClearRetryDelays = []time.Duration{time.Hour}
			claudeQueuePendingAuthClear("", claudeCredStamp{modNs: 1, size: 1})
			t.Cleanup(func() {
				claudeUsageProbe.mu.Lock()
				claudeUsageProbe.abandonBackgroundLocked()
				claudeUsageProbe.mu.Unlock()
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.leak(t)
			if got := waitClaudeGate(20*time.Millisecond, claudeGateLeak); !strings.Contains(got, tc.want) {
				t.Errorf("claudeGateLeak()=%q, want it to name %q", got, tc.want)
			}
		})
	}
	if got := claudeGateLeak(); got != "" {
		t.Errorf("claudeGateLeak()=%q after every leak was cleaned up, want quiet", got)
	}
}

// The diagnostic printed on a failed wait carries instants and counters only:
// no credential, no account fingerprint and no file path.
func TestClaudeProbeStateForTest_PrintsOnlyInstantsAndCounters(t *testing.T) {
	cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
	const scoped = "scope-0123456789abcdef"
	observed := time.Now().Add(-time.Minute)
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 12, ResetsAtMs: time.Now().Add(time.Hour).UnixMilli(),
			ObservedAtMs: observed.UnixMilli(), usageKnown: true,
		},
	}, observed, scoped, claudeRateLimitSourceProbe)
	owed := time.Now()
	seedClaudeRefreshDebt(t, cache, scoped, owed, 1, time.Time{})

	dump := claudeProbeStateForTest(cache)
	for _, hidden := range []string{probeTestToken, scoped, currentClaudeAccountFingerprint(), cache, filepath.Dir(cache)} {
		if hidden != "" && strings.Contains(dump, hidden) {
			t.Errorf("the diagnostic carries a value it must omit: %s", dump)
		}
	}
	for _, want := range []string{
		"refreshOwedAtMs=" + strconv.FormatInt(owed.UnixMilli(), 10),
		"observedAtMs=" + strconv.FormatInt(observed.UnixMilli(), 10),
	} {
		if !strings.Contains(dump, want) {
			t.Errorf("the diagnostic lacks %q: %s", want, dump)
		}
	}
}
