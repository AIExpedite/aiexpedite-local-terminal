package main

// A Claude run the agent did not spawn — the maintenance controller's
// `claude -p` smoke, a user's shell, a `claude` inside an agent-managed
// PowerShell session — owes its refresh through the SessionEnd hook, and the
// resident agent adopts and pays it (adoptObservedClaudeRunDebt). These cases
// drive that path end to end against a fake OAuth usage endpoint.

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// claudeBothWindowsHandler answers the usage endpoint with both windows.
func claudeBothWindowsHandler(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprint(w, probeUsageJSON(map[string]string{
		claudeWindowFiveHour: fmt.Sprintf(`{"utilization":37,"resets_at":%d,"status":"allowed"}`, time.Now().Add(3*time.Hour).Unix()),
		claudeWindowSevenDay: fmt.Sprintf(`{"utilization":58,"resets_at":%d,"status":"allowed"}`, time.Now().Add(96*time.Hour).Unix()),
	}))
}

// armClaudeDirectRunTest is armClaudeRunEndHookTest with the resident gate
// ARMED (this process plays both the hook and the agent) and the adoption latch
// and retry timer cleared.
func armClaudeDirectRunTest(t *testing.T, handler http.HandlerFunc) (string, *int64) {
	t.Helper()
	cache, calls := armClaudeRunEndHookTest(t, handler)
	SetClaudeUsageProbeDisabled(false)
	resetClaudeObservedDebtAdoption()
	t.Cleanup(func() {
		stopClaudeRunDebtRetry()
		resetClaudeObservedDebtAdoption()
	})
	return cache, calls
}

func writeClaudeExpiringCredential(t *testing.T, expiresAt time.Time, token string) {
	t.Helper()
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"rt","subscriptionType":"max","expiresAt":%d}}`,
		token, expiresAt.UnixMilli())
	if err := os.WriteFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// requireNumericClaudeWindow asserts a real reading observed at or after `since`.
func requireNumericClaudeWindow(t *testing.T, snap claudeRateLimitSnapshot, window string, since time.Time) {
	t.Helper()
	b, ok := snap.Buckets[window]
	if !ok || !b.hasObservedUsage() || b.UsedPercentage <= 0 || b.ObservedAtMs < since.UnixMilli() {
		t.Fatalf("%s = %+v (present=%v), want a numeric reading observed at or after %d", window, b, ok, since.UnixMilli())
	}
}

// The acceptance criterion at unit level, through the real tick: a hook debt
// at T is adopted within one poll, exactly one request goes out, both windows
// carry numeric readings observed at or after T, and one generation hint is
// queued for terminal-service.
func TestClaudeDirectRun_TickAdoptsTheHookDebtEndToEnd(t *testing.T) {
	cache, calls := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	rec, cfg := propagatorFixture(t)
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour)) // the stale reading the card was stuck on
	cliUsageClaudeFallbackEnabled = true
	cliUsageClaudeFallbackPeriod = time.Hour // only the stat gate can make the tick work
	cliUsageClaudeFallbackPoll = 20 * time.Millisecond
	claudeUsageFallbackDetected = func() bool { return true }
	startCLIUsagePropagator(cfg)
	waitHints(t, rec, 1, 0) // the startup recovery of the stale reading

	end := time.Now()
	if !claudeRunEndHookAt(end) {
		t.Fatal("the hook did not owe the direct run's refresh")
	}
	waitForClaudeCondition(t, 5*time.Second, "the tick never paid the hook's debt", func() bool {
		snap, ok := loadClaudeRateLimitSnapshot(cache)
		return ok && snap.RefreshOwedAtMs == 0 && atomic.LoadInt64(calls) > 0
	})
	claudeFreshnessWaitIdle(t)
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("requests=%d, want exactly one", got)
	}
	snap := claudeCacheSnapshot(t, cache)
	requireNumericClaudeWindow(t, snap, claudeWindowFiveHour, end)
	requireNumericClaudeWindow(t, snap, claudeWindowSevenDay, end)
	committed := gen(snap.GenerationEpoch, snap.Generation)
	waitForClaudeCondition(t, 5*time.Second, "no hint carried the new generation", func() bool {
		for _, h := range rec.all() {
			if h.hint.Provider == claudeUsageProvider && h.hint.GenerationEpoch == committed.Epoch && h.hint.Generation == committed.Counter {
				return true
			}
		}
		return false
	})
}

// The reporting device's loop: a previous attempt is parked on
// `credential_expired`, and the run that just ended rewrote the credential.
// The adopted debt sends at once.
func TestClaudeDirectRun_CredentialRewrittenByTheRunSendsAtOnce(t *testing.T) {
	cache, calls := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	now := time.Now()
	writeClaudeExpiringCredential(t, now.Add(-time.Hour), "sk-ant-oat-expired")
	old := now.Add(-10 * time.Minute)
	claudeOweRunRefresh(old)
	claudeRunDebtAttemptAt(now, claudeDebtTriggerRun)
	claudeFreshnessWaitIdle(t)
	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("precondition: an expired token was sent (%d requests)", got)
	}
	if snap := claudeCacheSnapshot(t, cache); snap.AuthWaitCredStampNs == 0 {
		t.Fatalf("precondition: snap=%+v, want the credential wait persisted", snap)
	}

	// The direct run refreshes the token (Claude Code rewrites the file before
	// SessionEnd fires), then ends.
	time.Sleep(20 * time.Millisecond) // a distinct mtime on coarse filesystems
	writeClaudeExpiringCredential(t, now.Add(time.Hour), "sk-ant-oat-refreshed-longer")
	end := time.Now()
	if !claudeRunEndHookAt(end) {
		t.Fatal("the hook did not owe the run")
	}
	if !adoptObservedClaudeRunDebt(end) {
		t.Fatal("the tick did not adopt the hook's debt")
	}
	claudeFreshnessWaitIdle(t)
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("requests=%d, want the rewritten credential sent once", got)
	}
	requireNumericClaudeWindow(t, claudeCacheSnapshot(t, cache), claudeWindowFiveHour, end)
}

// Without the rewrite the same token is still expired: the adoption books a
// free rung and sends nothing.
func TestClaudeDirectRun_StillExpiredCredentialSendsNothing(t *testing.T) {
	cache, calls := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	pinClaudeRunDebtLadder(t, []time.Duration{time.Hour}, time.Hour, time.Millisecond)
	now := time.Now()
	writeClaudeExpiringCredential(t, now.Add(-time.Hour), "sk-ant-oat-expired")
	end := now
	if !claudeRunEndHookAt(end) {
		t.Fatal("the hook did not owe the run")
	}
	if !adoptObservedClaudeRunDebt(end) {
		t.Fatal("the tick did not adopt the hook's debt")
	}
	claudeFreshnessWaitIdle(t)
	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("requests=%d, want an expired token never sent", got)
	}
	snap := claudeCacheSnapshot(t, cache)
	if snap.RefreshOwedAtMs != end.UnixMilli() || snap.RefreshOwedAttempts != 0 || snap.NextAttemptAtMs <= end.UnixMilli() {
		t.Fatalf("snap=%+v, want the debt standing, uncharged, with a free rung booked", snap)
	}
	// Adopted once: the next tick does not re-attempt the same debt.
	if adoptObservedClaudeRunDebt(time.Now()) {
		t.Fatal("the same debt was adopted twice")
	}
}

// Ten runs ending inside one 60 s floor cost one request between them.
func TestClaudeDirectRun_ABurstInsideTheFloorCostsOneRequest(t *testing.T) {
	_, calls := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	t.Setenv(claudeUsageProbeMinIntervalEnv, "60000")
	pinClaudeRunDebtLadder(t, []time.Duration{time.Hour}, time.Hour, time.Millisecond)
	base := time.Now()
	for i := 0; i < 10; i++ {
		end := base.Add(time.Duration(i) * 10 * time.Millisecond)
		claudeRunEndHookAt(end)
		adoptObservedClaudeRunDebt(end)
		claudeFreshnessWaitIdle(t)
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("requests=%d for a burst of 10 runs inside one floor, want 1", got)
	}
}

// A steady stream of runs costs at most one request per floor (pinned small
// here); a newer instant resets the debt's own counter by design, so the floor
// is the bound for continuous runs.
func TestClaudeDirectRun_ASteadyStreamIsBoundedByTheFloor(t *testing.T) {
	_, calls := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	const floor = 200 * time.Millisecond
	t.Setenv(claudeUsageProbeMinIntervalEnv, fmt.Sprint(floor.Milliseconds()))
	start := time.Now()
	for time.Since(start) < time.Second {
		now := time.Now()
		claudeRunEndHookAt(now)
		adoptObservedClaudeRunDebt(now)
		time.Sleep(15 * time.Millisecond)
	}
	claudeFreshnessWaitIdle(t)
	elapsed := time.Since(start)
	if got, max := atomic.LoadInt64(calls), int64(elapsed/floor)+2; got > max {
		t.Fatalf("requests=%d over %v, want at most %d (one per %v floor)", got, elapsed, max, floor)
	}
}

// One debt instant never costs more than claudeRefreshOwedMaxRequests, however
// often the tick or the ladder comes back to it.
func TestClaudeDirectRun_OneDebtNeverCostsMoreThanTheCap(t *testing.T) {
	_, calls := armClaudeDirectRunTest(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	pinClaudeRunDebtLadder(t, []time.Duration{time.Hour}, time.Hour, time.Millisecond)
	end := time.Now()
	claudeRunEndHookAt(end)
	adoptObservedClaudeRunDebt(end)
	claudeFreshnessWaitIdle(t)
	for i := 1; i <= 10; i++ {
		// Past every rung, inside the age-out.
		claudeRunDebtAttemptAt(end.Add(time.Duration(i)*20*time.Minute), claudeDebtTriggerObserved)
		claudeFreshnessWaitIdle(t)
	}
	if got := atomic.LoadInt64(calls); got > claudeRefreshOwedMaxRequests {
		t.Fatalf("requests=%d for one debt, want at most %d", got, claudeRefreshOwedMaxRequests)
	}
}

// A retired debt — aged out or at the cap — is not adopted and costs nothing.
func TestClaudeDirectRun_ARetiredDebtIsNotAdopted(t *testing.T) {
	for _, tc := range []struct {
		name     string
		age      time.Duration
		attempts int
	}{
		{"aged out", claudeRefreshOwedMaxAge + time.Minute, 0},
		{"at the cap", time.Minute, claudeRefreshOwedMaxRequests},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache, calls := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
			now := time.Now()
			seedClaudeRefreshDebt(t, cache, currentClaudeAccountFingerprint(), now.Add(-tc.age), tc.attempts, time.Time{})
			if adoptObservedClaudeRunDebt(now) {
				t.Fatal("a retired debt was adopted")
			}
			if got := atomic.LoadInt64(calls); got != 0 {
				t.Fatalf("requests=%d, want none", got)
			}
		})
	}
}

// Dual channel: the installed hooks pin the OTHER channel's cache, so the hook
// wrote its debt there. This channel adopts it onto its own cache and pays it.
func TestClaudeDirectRun_AdoptsADebtOnThePinnedCache(t *testing.T) {
	cache, calls := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	fp := currentClaudeAccountFingerprint()
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))
	pinned := filepath.Join(t.TempDir(), "other-channel", "rl.json")
	helperWriteJSON(t, filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"), map[string]any{
		"statusLine": map[string]any{
			"type": "command",
			"command": "AIEXPEDITE_CLAUDE_RL_CACHE=" + posixSingleQuote(pinned) +
				" '/opt/other/aiexpedite-terminal' " + statusLineHookArg,
		},
	})
	end := time.Now()
	if err := os.MkdirAll(filepath.Dir(pinned), 0o755); err != nil {
		t.Fatal(err)
	}
	seedClaudeRefreshDebt(t, pinned, fp, end, 0, time.Time{})

	if !adoptObservedClaudeRunDebt(end) {
		t.Fatal("the pinned cache's debt was not adopted")
	}
	claudeFreshnessWaitIdle(t)
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("requests=%d, want one", got)
	}
	snap := claudeCacheSnapshot(t, cache)
	if snap.RefreshOwedAtMs != 0 {
		t.Fatalf("own snap=%+v, want the adopted debt paid", snap)
	}
	requireNumericClaudeWindow(t, snap, claudeWindowFiveHour, end)
}

// An agent-owned debt the gate already knows about is not adopted a second
// time: the run's own trigger pays it.
func TestClaudeDirectRun_AnAgentOwnedDebtIsNotAdoptedAgain(t *testing.T) {
	_, calls := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	owed := time.Now()
	claudeUsageProbe.recordOwed(owed)
	claudeOweRunRefresh(owed)
	if adoptObservedClaudeRunDebt(owed) {
		t.Fatal("the tick adopted a debt this process already owes")
	}
	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("requests=%d, want none from the tick", got)
	}
}
