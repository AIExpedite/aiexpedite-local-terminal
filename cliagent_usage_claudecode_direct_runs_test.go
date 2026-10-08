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
	resetClaudeDisplacedPinnedCaches()
	t.Cleanup(func() {
		stopClaudeRunDebtRetry()
		resetClaudeObservedDebtAdoption()
		resetClaudeDisplacedPinnedCaches()
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
	// Exactly the cap: the ladder does retry a failing endpoint, and stops there.
	if got := atomic.LoadInt64(calls); got != claudeRefreshOwedMaxRequests {
		t.Fatalf("requests=%d for one failing debt, want exactly the cap %d", got, claudeRefreshOwedMaxRequests)
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
	if pinnedSnap := claudeCacheSnapshot(t, pinned); pinnedSnap.RefreshOwedAtMs != 0 {
		t.Fatalf("pinned snap=%+v, want the debt transferred off it, not copied", pinnedSnap)
	}
}

// pinOtherChannelCache installs a status line pinned to another channel's
// cache and returns that cache's path.
func pinOtherChannelCache(t *testing.T) string {
	t.Helper()
	pinned := filepath.Join(t.TempDir(), "other-channel", "rl.json")
	helperWriteJSON(t, filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"), map[string]any{
		"statusLine": map[string]any{
			"type": "command",
			"command": "AIEXPEDITE_CLAUDE_RL_CACHE=" + posixSingleQuote(pinned) +
				" '/opt/other/aiexpedite-terminal' " + statusLineHookArg,
		},
	})
	if err := os.MkdirAll(filepath.Dir(pinned), 0o755); err != nil {
		t.Fatal(err)
	}
	return pinned
}

// pinRunEndHookOnly installs ONLY our SessionEnd hook, pinned to another
// channel's cache — a partial settings.json rewrite dropped the status line —
// and returns that cache's path.
func pinRunEndHookOnly(t *testing.T) string {
	t.Helper()
	pinned := filepath.Join(t.TempDir(), "other-channel", "rl.json")
	helperWriteJSON(t, filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"), map[string]any{
		"hooks": map[string]any{
			claudeRunEndHookEvent: []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command",
				"command": "AIEXPEDITE_CLAUDE_RL_CACHE=" + posixSingleQuote(pinned) +
					" '/opt/other/aiexpedite-terminal' " + claudeRunEndHookArg,
			}}}},
		},
	})
	if err := os.MkdirAll(filepath.Dir(pinned), 0o755); err != nil {
		t.Fatal(err)
	}
	return pinned
}

// requirePinnedDebtTransferredAndPaid adopts, then asserts one request paid
// the debt on the own cache and the pinned copy is gone.
func requirePinnedDebtTransferredAndPaid(t *testing.T, cache, pinned string, calls *int64, end time.Time) {
	t.Helper()
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
	if pinnedSnap := claudeCacheSnapshot(t, pinned); pinnedSnap.RefreshOwedAtMs != 0 {
		t.Fatalf("pinned snap=%+v, want the debt transferred off it", pinnedSnap)
	}
}

// Dual channel, status line gone: the run-end hook alone still pins the other
// channel's cache, so the debt it wrote there is found from the hook itself.
func TestClaudeDirectRun_AdoptsADebtTheRunEndHookAlonePins(t *testing.T) {
	cache, calls := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))
	pinned := pinRunEndHookOnly(t)
	end := time.Now()
	seedClaudeRefreshDebt(t, pinned, currentClaudeAccountFingerprint(), end, 0, time.Time{})

	requirePinnedDebtTransferredAndPaid(t, cache, pinned, calls, end)
}

// Dual channel, repaired first: the tick's reconcile re-points both hooks at
// this channel's cache BEFORE adoption runs. The debt the old run-end hook
// wrote to the other channel's cache must still be adopted, though
// settings.json no longer names that cache.
func TestClaudeDirectRun_APinnedDebtSurvivesTheReconcileThatRePointsTheHooks(t *testing.T) {
	cache, calls := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	resetClaudeStatusLineReconcile()
	SetClaudeStatusLineHookDisabled(false)
	t.Cleanup(resetClaudeStatusLineReconcile)
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))
	pinned := pinRunEndHookOnly(t)
	end := time.Now()
	seedClaudeRefreshDebt(t, pinned, currentClaudeAccountFingerprint(), end, 0, time.Time{})

	home, _ := os.UserHomeDir()
	if changed, err := ensureClaudeStatusLineHookIfStale(home); err != nil || !changed {
		t.Fatalf("reconcile changed=%v err=%v, want the hooks re-pointed", changed, err)
	}
	if got := installedClaudeRunEndCachePath(home); got == pinned {
		t.Fatal("the reconcile left the run-end hook on the other channel's cache")
	}
	requirePinnedDebtTransferredAndPaid(t, cache, pinned, calls, end)
}

// Dual channel, re-pointed then restarted: the agent exits after the reconcile
// rewrote settings.json but before its adoption tick, so the next process
// neither sees the other channel's cache in settings.json nor inherits the
// in-memory memo. The re-point itself moved the debt onto the own cache, so
// the next process still pays it.
func TestClaudeDirectRun_APinnedDebtSurvivesARestartAfterTheRePoint(t *testing.T) {
	cache, calls := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	resetClaudeStatusLineReconcile()
	SetClaudeStatusLineHookDisabled(false)
	t.Cleanup(resetClaudeStatusLineReconcile)
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))
	pinned := pinRunEndHookOnly(t)
	end := time.Now()
	seedClaudeRefreshDebt(t, pinned, currentClaudeAccountFingerprint(), end, 0, time.Time{})

	home, _ := os.UserHomeDir()
	if changed, err := ensureClaudeStatusLineHookIfStale(home); err != nil || !changed {
		t.Fatalf("reconcile changed=%v err=%v, want the hooks re-pointed", changed, err)
	}
	if got := claudeCacheSnapshot(t, cache).RefreshOwedAtMs; got != end.UnixMilli() {
		t.Fatalf("own debt=%d, want the re-point to have moved the debt at %d onto the own cache", got, end.UnixMilli())
	}
	// The restart: the memo and the adoption latch are gone.
	resetClaudeDisplacedPinnedCaches()
	resetClaudeObservedDebtAdoption()
	requirePinnedDebtTransferredAndPaid(t, cache, pinned, calls, end)
}

// seedClaudeOwnCacheUnderPreviousAccount signs this device in to a scoped
// account and leaves the own cache scoped to the login it was on before that
// switch, with a reading and a debt of that account's own.
func seedClaudeOwnCacheUnderPreviousAccount(t *testing.T, cache string, owedAt time.Time) {
	t.Helper()
	writeClaudeAccountCredential(t, os.Getenv("CLAUDE_CONFIG_DIR"), "ada@example.com")
	if currentClaudeAccountFingerprint() == "" {
		t.Fatal("the credential fixture resolved to an unscoped account; this case needs a scoped one")
	}
	const previous = "fp-previous-account"
	observedAt := time.Now().Add(-time.Hour)
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 33, ResetsAtMs: observedAt.Add(time.Hour).UnixMilli(),
			ObservedAtMs: observedAt.UnixMilli(), usageKnown: true,
		},
	}, observedAt, previous, claudeRateLimitSourceStatusLine)
	seedClaudeRefreshDebt(t, cache, previous, owedAt, 0, time.Time{})
}

// Dual channel, account switched: the hook re-scoped the pinned cache to the
// login signed in now, while the own cache is still on the previous one — with
// a newer debt of its own. The pinned run belongs to the current account, so it
// is transferred (re-scoping the own cache) and paid.
func TestClaudeDirectRun_APinnedDebtAfterAnAccountSwitchIsTransferred(t *testing.T) {
	cache, calls := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	pinned := pinOtherChannelCache(t)
	end := time.Now()
	seedClaudeOwnCacheUnderPreviousAccount(t, cache, end.Add(time.Second))
	seedClaudeRefreshDebt(t, pinned, currentClaudeAccountFingerprint(), end, 0, time.Time{})

	requirePinnedDebtTransferredAndPaid(t, cache, pinned, calls, end)
	if got := claudeCacheSnapshot(t, cache).AccountFingerprint; got != currentClaudeAccountFingerprint() {
		t.Fatal("the own cache was not moved onto the current account")
	}
}

// The same switch through the re-point, then a restart: the reconcile moves the
// current account's pinned debt onto the own cache before it rewrites
// settings.json, so the next process pays it without the in-memory memo.
func TestClaudeDirectRun_APinnedDebtAfterAnAccountSwitchSurvivesARestartAfterTheRePoint(t *testing.T) {
	cache, calls := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	resetClaudeStatusLineReconcile()
	SetClaudeStatusLineHookDisabled(false)
	t.Cleanup(resetClaudeStatusLineReconcile)
	pinned := pinRunEndHookOnly(t)
	end := time.Now()
	seedClaudeOwnCacheUnderPreviousAccount(t, cache, end.Add(-time.Minute))
	seedClaudeRefreshDebt(t, pinned, currentClaudeAccountFingerprint(), end, 0, time.Time{})

	home, _ := os.UserHomeDir()
	if changed, err := ensureClaudeStatusLineHookIfStale(home); err != nil || !changed {
		t.Fatalf("reconcile changed=%v err=%v, want the hooks re-pointed", changed, err)
	}
	own := claudeCacheSnapshot(t, cache)
	if own.RefreshOwedAtMs != end.UnixMilli() || own.AccountFingerprint != currentClaudeAccountFingerprint() {
		t.Fatal("the re-point did not move the current account's debt onto the own cache")
	}
	resetClaudeDisplacedPinnedCaches()
	resetClaudeObservedDebtAdoption()
	requirePinnedDebtTransferredAndPaid(t, cache, pinned, calls, end)
}

// A pinned debt scoped to neither the own cache's account nor the one signed in
// now belongs to a login this device is not on: it is left where it is.
func TestClaudeDirectRun_APinnedDebtOfAnotherAccountIsNotTransferred(t *testing.T) {
	cache, calls := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	pinned := pinOtherChannelCache(t)
	end := time.Now()
	seedClaudeOwnCacheUnderPreviousAccount(t, cache, end.Add(-time.Minute))
	seedClaudeRefreshDebt(t, pinned, "fp-unrelated-account", end, 0, time.Time{})

	if _, _, owedMs := claudePinnedObservedDebt(claudeCacheSnapshot(t, cache), true, end); owedMs != 0 {
		t.Fatalf("pinned debt %d offered for transfer, want none", owedMs)
	}
	adoptObservedClaudeRunDebt(end)
	claudeFreshnessWaitIdle(t)
	if p := claudeCacheSnapshot(t, pinned); p.RefreshOwedAtMs != end.UnixMilli() {
		t.Fatal("the other account's debt was taken off its cache")
	}
	if own := claudeCacheSnapshot(t, cache); own.RefreshOwedAtMs == end.UnixMilli() {
		t.Fatal("the other account's debt was owed onto the own cache")
	}
	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("requests=%d, want none for another account's run", got)
	}
}

// Dual channel, owner first: the channel that owns the pinned cache has
// already started paying the hook's debt there (a charged request with its
// claim lease, or a booked rung). This channel must not copy it into its own
// separately budgeted cache and send a second request for the same run.
func TestClaudeDirectRun_APinnedDebtTheOwnerIsPayingIsNotCopied(t *testing.T) {
	cases := map[string]func(*claudeRateLimitSnapshot, time.Time){
		"claimed": func(snap *claudeRateLimitSnapshot, now time.Time) {
			snap.RefreshOwedAttempts = 1
			claudePublishRunDebtClaim(snap, now)
		},
		"rung booked": func(snap *claudeRateLimitSnapshot, now time.Time) {
			snap.NextAttemptAtMs = now.Add(time.Minute).UnixMilli()
		},
	}
	for name, ownerTouch := range cases {
		t.Run(name, func(t *testing.T) {
			cache, calls := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
			fp := currentClaudeAccountFingerprint()
			seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))
			pinned := pinOtherChannelCache(t)
			end := time.Now()
			seedClaudeRefreshDebt(t, pinned, fp, end, 0, time.Time{})
			if !mutateClaudeRateLimitSnapshot(pinned, fp, func(snap *claudeRateLimitSnapshot) bool {
				ownerTouch(snap, end)
				return true
			}) {
				t.Fatal("seeding the owner's attempt did not write the pinned cache")
			}

			if adoptObservedClaudeRunDebt(end) {
				t.Fatal("adopted a pinned debt its owner is already paying")
			}
			claudeFreshnessWaitIdle(t)
			if got := atomic.LoadInt64(calls); got != 0 {
				t.Fatalf("requests=%d, want none", got)
			}
			if own := claudeCacheSnapshot(t, cache); own.RefreshOwedAtMs != 0 {
				t.Fatalf("own snap=%+v, want no copy of the owner's debt", own)
			}
			if p := claudeCacheSnapshot(t, pinned); p.RefreshOwedAtMs != end.UnixMilli() {
				t.Fatalf("pinned snap=%+v, want the owner's debt left standing", p)
			}
		})
	}
}

// Dual channel, peer first: once this channel has taken the pinned debt, the
// owning channel's own adoption finds nothing to pay.
func TestClaudeDirectRun_ATransferredPinnedDebtIsNotPaidTwice(t *testing.T) {
	cache, calls := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	fp := currentClaudeAccountFingerprint()
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))
	pinned := pinOtherChannelCache(t)
	end := time.Now()
	seedClaudeRefreshDebt(t, pinned, fp, end, 0, time.Time{})

	if !adoptObservedClaudeRunDebt(end) {
		t.Fatal("the pinned cache's debt was not adopted")
	}
	claudeFreshnessWaitIdle(t)

	// The owning channel now ticks on its own cache — the pinned one.
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", pinned)
	resetClaudeObservedDebtAdoption()
	if adoptObservedClaudeRunDebt(end.Add(time.Second)) {
		t.Fatal("the owning channel adopted a debt already transferred away")
	}
	claudeFreshnessWaitIdle(t)
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("requests=%d, want exactly one across both channels", got)
	}
}

// Dual channel, interrupted after the claim: the peer leased the pinned debt
// and died before owing it on its own cache. The debt must still be on the
// pinned cache, so once the lease lapses the run is paid, not lost.
func TestClaudeDirectRun_APinnedDebtSurvivesATransferInterruptedAfterItsClaim(t *testing.T) {
	cache, calls := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	fp := currentClaudeAccountFingerprint()
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))
	pinned := pinOtherChannelCache(t)
	end := time.Now()
	seedClaudeRefreshDebt(t, pinned, fp, end, 0, time.Time{})

	var claim claudePinnedDebtClaim
	if !mutateClaudeRateLimitSnapshotScoped(pinned, fp, []string{fp}, claimClaudePinnedObservedDebt(end.UnixMilli(), &claim)) {
		t.Fatal("precondition: the claim did not write the pinned cache")
	}
	if p := claudeCacheSnapshot(t, pinned); p.RefreshOwedAtMs != end.UnixMilli() {
		t.Fatalf("pinned snap=%+v, want the claimed debt still on the pinned cache", p)
	}
	if adoptObservedClaudeRunDebt(end) {
		t.Fatal("adopted a pinned debt under another claim's live lease")
	}

	// The claimant is gone; its lease lapses.
	if !mutateClaudeRateLimitSnapshot(pinned, fp, func(snap *claudeRateLimitSnapshot) bool {
		snap.AttemptClaimedUntilMs = time.Now().Add(-time.Second).UnixMilli()
		return true
	}) {
		t.Fatal("expiring the lease did not write the pinned cache")
	}
	if !adoptObservedClaudeRunDebt(time.Now()) {
		t.Fatal("the pinned debt was not adopted once the dead claim lapsed")
	}
	claudeFreshnessWaitIdle(t)
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("requests=%d, want one", got)
	}
	requireNumericClaudeWindow(t, claudeCacheSnapshot(t, cache), claudeWindowFiveHour, end)
	if p := claudeCacheSnapshot(t, pinned); p.RefreshOwedAtMs != 0 || p.AttemptClaimedUntilMs != 0 {
		t.Fatalf("pinned snap=%+v, want the transfer finished", p)
	}
}

// Dual channel, interrupted after the owe: both caches hold the same instant.
// The next adoption finishes the transfer — the own copy is paid once and the
// pinned copy is cleared rather than left for the owner to pay again.
func TestClaudeDirectRun_ATransferInterruptedAfterItsOweIsFinished(t *testing.T) {
	cache, calls := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	fp := currentClaudeAccountFingerprint()
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))
	pinned := pinOtherChannelCache(t)
	end := time.Now()
	seedClaudeRefreshDebt(t, pinned, fp, end, 0, time.Time{})
	seedClaudeRefreshDebt(t, cache, fp, end, 0, time.Time{})

	if !adoptObservedClaudeRunDebt(end) {
		t.Fatal("the half-transferred debt was not adopted")
	}
	claudeFreshnessWaitIdle(t)
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("requests=%d, want one", got)
	}
	if p := claudeCacheSnapshot(t, pinned); p.RefreshOwedAtMs != 0 || p.AttemptClaimedUntilMs != 0 {
		t.Fatalf("pinned snap=%+v, want the interrupted transfer finished", p)
	}
}

// A credential that is transiently unreadable when the tick adopts resolves no
// account, so the attempt cannot scope the debt and books nothing. The debt
// must stay adoptable: once the credential reads again, the next tick pays it.
func TestClaudeDirectRun_ATransientIdentityFailureIsAdoptedAgain(t *testing.T) {
	cache, calls := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	credential := filepath.Join(configDir, ".credentials.json")
	good := []byte(fmt.Sprintf(`{"account":"acct-A","claudeAiOauth":{"accessToken":%q,"refreshToken":"rt","subscriptionType":"max"}}`, probeTestToken))
	if err := os.WriteFile(credential, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if currentClaudeAccountFingerprint() == "" {
		t.Fatal("precondition: the fixture credential resolves no account")
	}
	end := time.Now()
	if !claudeRunEndHookAt(end) {
		t.Fatal("the hook did not owe the run")
	}

	if err := os.WriteFile(credential, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !adoptObservedClaudeRunDebt(end) {
		t.Fatal("the tick did not attempt the hook's debt")
	}
	claudeFreshnessWaitIdle(t)
	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("requests=%d with no readable credential, want none", got)
	}
	if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != end.UnixMilli() || snap.NextAttemptAtMs != 0 {
		t.Fatalf("precondition: snap=%+v, want the debt standing with nothing booked", snap)
	}

	if err := os.WriteFile(credential, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if !adoptObservedClaudeRunDebt(time.Now()) {
		t.Fatal("the debt was latched by an attempt that could not scope it")
	}
	claudeFreshnessWaitIdle(t)
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("requests=%d after the credential recovered, want one", got)
	}
	requireNumericClaudeWindow(t, claudeCacheSnapshot(t, cache), claudeWindowFiveHour, end)
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

// The installed hooks carry the cache path in their shell's spelling (the Git
// Bash form on Windows writes forward slashes). Once they are pinned to the own
// cache, that spelling must not list it as another channel's: the transfer
// would then "move" the own debt onto itself and clear it unpaid.
func TestClaudePinnedCacheCandidates_SkipTheOwnCacheInAnySpelling(t *testing.T) {
	armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	own := claudeRateLimitCachePath()
	rememberClaudeHookPinnedCaches("")
	claudeDisplacedPinnedCaches.mu.Lock()
	claudeDisplacedPinnedCaches.paths = append(claudeDisplacedPinnedCaches.paths, filepath.ToSlash(own), own+string(filepath.Separator)+".")
	claudeDisplacedPinnedCaches.mu.Unlock()
	home, _ := os.UserHomeDir()
	if got := claudePinnedCacheCandidates(home); len(got) != 0 {
		t.Fatalf("candidates=%q, want the own cache (%q) excluded in every spelling", got, own)
	}
}

// An alias the spelling cannot show — a hard link or a symlink to the own
// cache — is still the own cache, so it must not be listed as another
// channel's either.
func TestClaudePinnedCacheCandidates_SkipTheOwnCacheThroughAnAlias(t *testing.T) {
	armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	own := claudeRateLimitCachePath()
	if err := os.MkdirAll(filepath.Dir(own), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(own, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	aliases := []string{}
	hard := filepath.Join(dir, "hard.json")
	if err := os.Link(own, hard); err == nil {
		aliases = append(aliases, hard)
	}
	sym := filepath.Join(dir, "sym.json")
	if err := os.Symlink(own, sym); err == nil {
		aliases = append(aliases, sym)
	}
	if len(aliases) == 0 {
		t.Skip("this filesystem allows neither hard links nor symlinks")
	}
	other := filepath.Join(dir, "other.json")
	if err := os.WriteFile(other, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	rememberClaudeHookPinnedCaches("")
	claudeDisplacedPinnedCaches.mu.Lock()
	claudeDisplacedPinnedCaches.paths = append(append(claudeDisplacedPinnedCaches.paths, aliases...), other)
	claudeDisplacedPinnedCaches.mu.Unlock()
	home, _ := os.UserHomeDir()
	if got := claudePinnedCacheCandidates(home); len(got) != 1 || got[0] != other {
		t.Fatalf("candidates=%q, want only %q (the own cache's aliases excluded)", got, other)
	}
}

// Dual channel, owner gone mid-payment: the channel that owns the pinned cache
// charged an attempt or booked a rung, then exited. Once its rung (or the
// lease of its last attempt) is past the abandonment slack, this channel takes
// the debt over, with the requests it already cost, and pays it.
func TestClaudeDirectRun_APinnedDebtItsOwnerAbandonedIsTakenOver(t *testing.T) {
	cases := map[string]func(*claudeRateLimitSnapshot, time.Time){
		"rung lapsed": func(snap *claudeRateLimitSnapshot, now time.Time) {
			snap.RefreshOwedAttempts = 2
			snap.NextAttemptAtMs = now.Add(-claudePinnedDebtAbandonedAfter - time.Second).UnixMilli()
		},
		"lease lapsed": func(snap *claudeRateLimitSnapshot, now time.Time) {
			snap.RefreshOwedAttempts = 2
			snap.AttemptClaimedUntilMs = now.Add(-claudePinnedDebtAbandonedAfter - time.Second).UnixMilli()
		},
	}
	for name, ownerTouch := range cases {
		t.Run(name, func(t *testing.T) {
			cache, calls := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
			fp := currentClaudeAccountFingerprint()
			seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))
			pinned := pinOtherChannelCache(t)
			now := time.Now()
			end := now.Add(-10 * time.Minute)
			seedClaudeRefreshDebt(t, pinned, fp, end, 0, time.Time{})
			if !mutateClaudeRateLimitSnapshot(pinned, fp, func(snap *claudeRateLimitSnapshot) bool {
				ownerTouch(snap, now)
				return true
			}) {
				t.Fatal("seeding the owner's attempt did not write the pinned cache")
			}

			own, haveOwn := loadClaudeRateLimitSnapshot(cache)
			if got := transferClaudePinnedObservedDebt(own, haveOwn, 0, now); got != end.UnixMilli() {
				t.Fatalf("transferred=%d, want the abandoned debt at %d taken over", got, end.UnixMilli())
			}
			if o := claudeCacheSnapshot(t, cache); o.RefreshOwedAtMs != end.UnixMilli() || o.RefreshOwedAttempts != 2 || o.NextAttemptAtMs != 0 {
				t.Fatalf("own snap=%+v, want the debt due with its 2 spent requests kept", o)
			}
			if p := claudeCacheSnapshot(t, pinned); p.RefreshOwedAtMs != 0 || p.AttemptClaimedUntilMs != 0 {
				t.Fatalf("pinned snap=%+v, want the debt moved off it", p)
			}
			if !adoptObservedClaudeRunDebt(now) {
				t.Fatal("the taken-over debt was not attempted")
			}
			claudeFreshnessWaitIdle(t)
			if got := atomic.LoadInt64(calls); got != 1 {
				t.Fatalf("requests=%d, want one", got)
			}
			requireNumericClaudeWindow(t, claudeCacheSnapshot(t, cache), claudeWindowFiveHour, end)
		})
	}
}

// A takeover never refills the budget: a debt that already cost all but its
// last request has exactly that one left on the own cache.
func TestClaudeDirectRun_ATakenOverDebtKeepsItsSpentBudget(t *testing.T) {
	cache, _ := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	fp := currentClaudeAccountFingerprint()
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))
	pinned := pinOtherChannelCache(t)
	now := time.Now()
	end := now.Add(-10 * time.Minute)
	// Interrupted after the owe: the own copy already holds the instant with a
	// smaller count than the pinned one.
	seedClaudeRefreshDebt(t, cache, fp, end, 0, time.Time{})
	seedClaudeRefreshDebt(t, pinned, fp, end, claudeRefreshOwedMaxRequests-1, time.Time{})
	if !mutateClaudeRateLimitSnapshot(pinned, fp, func(snap *claudeRateLimitSnapshot) bool {
		snap.NextAttemptAtMs = now.Add(-claudePinnedDebtAbandonedAfter - time.Second).UnixMilli()
		return true
	}) {
		t.Fatal("seeding the owner's rung did not write the pinned cache")
	}
	own, haveOwn := loadClaudeRateLimitSnapshot(cache)
	if got := transferClaudePinnedObservedDebt(own, haveOwn, 0, now); got != end.UnixMilli() {
		t.Fatalf("transferred=%d, want %d", got, end.UnixMilli())
	}
	if o := claudeCacheSnapshot(t, cache); o.RefreshOwedAttempts != claudeRefreshOwedMaxRequests-1 {
		t.Fatalf("own attempts=%d, want %d carried over", o.RefreshOwedAttempts, claudeRefreshOwedMaxRequests-1)
	}
}

// A rung only just overdue is a live owner's timer about to fire, not an
// abandoned debt: it stays with its owner.
func TestClaudeDirectRun_APinnedDebtJustPastItsRungIsNotTakenOver(t *testing.T) {
	cache, calls := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	fp := currentClaudeAccountFingerprint()
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))
	pinned := pinOtherChannelCache(t)
	now := time.Now()
	end := now.Add(-10 * time.Minute)
	seedClaudeRefreshDebt(t, pinned, fp, end, 1, time.Time{})
	if !mutateClaudeRateLimitSnapshot(pinned, fp, func(snap *claudeRateLimitSnapshot) bool {
		snap.NextAttemptAtMs = now.Add(-claudePinnedDebtAbandonedAfter / 2).UnixMilli()
		return true
	}) {
		t.Fatal("seeding the owner's rung did not write the pinned cache")
	}
	if adoptObservedClaudeRunDebt(now) {
		t.Fatal("took over a debt whose owner may still be paying it")
	}
	claudeFreshnessWaitIdle(t)
	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("requests=%d, want none", got)
	}
	if p := claudeCacheSnapshot(t, pinned); p.RefreshOwedAtMs != end.UnixMilli() || p.RefreshOwedAttempts != 1 {
		t.Fatalf("pinned snap=%+v, want the owner's debt left standing", p)
	}
}

// The owner charged a request, its rung write was refused (it retries in
// memory only), and its lease release still landed. The rung claim's release
// keeps the instant the lease ended, so once the owner is gone past the slack
// the debt reads as abandoned instead of sitting with no timestamp forever. A
// release on a debt with a rung on disk, or one never charged, clears to zero.
func TestClaudeDirectRun_AReleasedDebtWhoseRungFailedToPersistCanBeAbandoned(t *testing.T) {
	cache, _ := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	fp := currentClaudeAccountFingerprint()
	end := time.Now().Add(-10 * time.Minute)
	release := func(attempts int, rungMs int64) claudeRateLimitSnapshot {
		t.Helper()
		seedClaudeRefreshDebt(t, cache, fp, end, attempts, time.Time{})
		var lease int64
		if !mutateClaudeRateLimitSnapshot(cache, fp, func(snap *claudeRateLimitSnapshot) bool {
			snap.NextAttemptAtMs = rungMs
			lease = claudePublishRunDebtClaim(snap, time.Now())
			return true
		}) {
			t.Fatal("seeding the owner's lease did not write the cache")
		}
		if !mutateClaudeRateLimitSnapshot(cache, fp, releaseClaudeRunDebtRungClaim(lease)) {
			t.Fatal("the owner's release did not land")
		}
		return claudeCacheSnapshot(t, cache)
	}

	before := time.Now()
	snap := release(1, 0)
	if snap.AttemptClaimedUntilMs == 0 || snap.AttemptClaimedUntilMs < before.UnixMilli() || snap.AttemptClaimedUntilMs > time.Now().UnixMilli() {
		t.Fatalf("released lease=%d, want the release instant kept", snap.AttemptClaimedUntilMs)
	}
	if _, live := claudeRunDebtLeaseLive(&snap, time.Now()); live {
		t.Fatal("the kept release instant reads as a live lease")
	}
	if claudePinnedDebtAbandoned(&snap, time.Now()) {
		t.Fatal("judged abandoned straight after its owner released it")
	}
	if !claudePinnedDebtAbandoned(&snap, time.Now().Add(claudePinnedDebtAbandonedAfter+time.Second)) {
		t.Fatal("never judged abandoned once its owner was gone past the slack")
	}

	if snap := release(1, time.Now().Add(time.Minute).UnixMilli()); snap.AttemptClaimedUntilMs != 0 {
		t.Fatalf("released lease=%d with a rung on disk, want 0", snap.AttemptClaimedUntilMs)
	}
	if snap := release(0, 0); snap.AttemptClaimedUntilMs != 0 {
		t.Fatalf("released lease=%d on an uncharged debt, want 0", snap.AttemptClaimedUntilMs)
	}
}

// The hooks were pinned to an alias of the own cache when they were
// re-pointed. A hook still running on the old settings commits through that
// alias afterwards, and the commit's rename turns it into a file of its own:
// the re-point must still have remembered it, so that debt is found and paid.
func TestClaudeDirectRun_AnAliasRePointedAwayIsStillFoundOnceALateWriteSplitsIt(t *testing.T) {
	cache, calls := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	fp := currentClaudeAccountFingerprint()
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))
	dir := t.TempDir()
	alias := filepath.Join(dir, "sym.json")
	if err := os.Symlink(cache, alias); err != nil {
		alias = filepath.Join(dir, "hard.json")
		if err := os.Link(cache, alias); err != nil {
			t.Skip("this filesystem allows neither hard links nor symlinks")
		}
	}
	settings := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json")
	helperWriteJSON(t, settings, map[string]any{
		"statusLine": map[string]any{
			"type": "command",
			"command": "AIEXPEDITE_CLAUDE_RL_CACHE=" + posixSingleQuote(alias) +
				" '/opt/other/aiexpedite-terminal' " + statusLineHookArg,
		},
	})

	// The re-point: remembered, then settings.json names only the own cache.
	rememberClaudeHookPinnedCaches("")
	helperWriteJSON(t, settings, map[string]any{})
	home, _ := os.UserHomeDir()
	if got := claudePinnedCacheCandidates(home); len(got) != 0 {
		t.Fatalf("candidates=%q, want the alias skipped while it is still the own cache", got)
	}

	// The late hook commits through the alias, splitting it off.
	end := time.Now()
	seedClaudeRefreshDebt(t, alias, fp, end, 0, time.Time{})
	if sameClaudeCachePath(alias, cache) {
		t.Fatal("precondition: the late write did not split the alias from the own cache")
	}
	requirePinnedDebtTransferredAndPaid(t, cache, alias, calls, end)
}
