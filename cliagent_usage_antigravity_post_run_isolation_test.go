package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Order independence of the Antigravity usage suite: the leak detector and its
// redacted diagnostic (cliagent_usage_antigravity_test_isolation_test.go), the
// disabled-quota flow run after each leftover an earlier test could leave in
// this process, a straggler worker crossing into the next test, the disabled
// shape under a burst, and the suite's PATH pruning of a real `agy`. Same
// shape as cliagent_usage_claudecode_post_run_isolation_test.go.

// helperRecordingTB records Fatalf / Errorf instead of stopping the test, so a
// case can assert what requireAntigravityStateQuiet WOULD have failed with.
type helperRecordingTB struct {
	testing.TB
	mu     sync.Mutex
	fatals []string
	errors []string
}

func (r *helperRecordingTB) Helper() {}

func (r *helperRecordingTB) Fatalf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fatals = append(r.fatals, fmt.Sprintf(format, args...))
}

func (r *helperRecordingTB) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *helperRecordingTB) recorded() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(append(append([]string{}, r.fatals...), r.errors...), "\n")
}

// Each condition is induced on its own, named, and undone; a quiet process
// reports nothing.
func TestAntigravityStateLeak_NamesEachCondition(t *testing.T) {
	helperIsolateAntigravityFreshness(t)
	if reason := antigravityStateLeak(); reason != "" {
		t.Fatalf("leak=%q before anything was induced, want a quiet process", reason)
	}
	origTick := antigravityDiscoveryInterval
	antigravityDiscoveryInterval = time.Hour
	t.Cleanup(func() {
		stopAntigravityDiscovery()
		antigravityDiscoveryInterval = origTick
	})
	setWorker := func(running, rearm bool) {
		antigravityRefreshWorkerMu.Lock()
		antigravityRefreshWorkerRunning, antigravityRefreshWorkerRearm = running, rearm
		antigravityRefreshWorkerMu.Unlock()
	}

	for _, tc := range []struct {
		name         string
		induce, undo func()
		want         string
	}{
		{"in-flight write",
			func() { antigravityFreshnessInFlight.Add(1) }, func() { antigravityFreshnessInFlight.Add(-1) },
			"1 background write(s) in flight"},
		{"armed rung",
			func() { antigravityArmRunDebtRetry(antigravityDebtID{floorMs: 1, owedAtMs: 1}, time.Hour) },
			stopAntigravityRunDebtRetry,
			"a run-debt retry rung is still armed"},
		{"discovery tick", startAntigravityDiscovery, stopAntigravityDiscovery,
			"the discovery tick is still running"},
		{"worker claim", func() { setWorker(true, false) }, func() { setWorker(false, false) },
			"the debt worker still holds its claim"},
		{"pending re-arm", func() { setWorker(false, true) }, func() { setWorker(false, false) },
			"a debt worker re-arm is still pending"},
	} {
		tc.induce()
		got := antigravityStateLeak()
		tc.undo()
		if got != tc.want {
			t.Errorf("%s: leak=%q, want %q", tc.name, got, tc.want)
		}
		if after := antigravityStateLeak(); after != "" {
			t.Fatalf("%s: leak=%q after undoing it, want quiet", tc.name, after)
		}
	}
}

// The diagnostic prints instants, counters, codes and booleans only — never the
// account, its fingerprint, a token, a path or a URL.
func TestAntigravityStateForTest_IsRedacted(t *testing.T) {
	state, cache := helperIsolateAntigravityFreshness(t)
	const email, token, url = "ada@example.com", "ya29.helper-redaction-token-7Q2", "http://127.0.0.1:45871/v1internal"
	helperStubAntigravityKeyring(t, map[string]any{
		"access_token": token, "token_type": "Bearer", "refresh_token": "never-read",
		"expiry": time.Now().Add(30 * time.Minute).Format(time.RFC3339Nano),
	})
	t.Setenv("AIEXPEDITE_AGY_CODEASSIST_URL", url)
	t.Setenv("AIEXPEDITE_AGY_USERINFO_URL", url)
	debt := helperOwedDebt(t, antigravityUsageFreshness{Attempts: 1, NextAttemptAtMs: time.Now().Add(time.Minute).UnixMilli()})
	fingerprint := fingerprintAccount("antigravity", email)
	observed := time.Now().Add(-time.Hour).Truncate(time.Second)
	helperWriteJSON(t, cache, antigravityQuotaSnapshot{
		SchemaVersion: antigravityQuotaSchemaVersion, ObservedAt: observed.Format(time.RFC3339),
		ObservedAtMs: observed.UnixMilli(), Account: email, AccountFingerprint: fingerprint,
		Buckets: []antigravityQuotaBucket{{Group: "Gemini Models", Window: "weekly", RemainingFraction: 0.4}},
	})

	got := antigravityStateForTest(cache)
	for name, secret := range map[string]string{
		"email": email, "local part": "ada", "fingerprint": fingerprint, "token": token,
		"cache path": cache, "state path": state, "url": url, "loopback host": "127.0.0.1",
	} {
		if secret != "" && strings.Contains(got, secret) {
			t.Errorf("the diagnostic leaks the %s: %q", name, got)
		}
	}
	for _, want := range []string{
		"refreshOwedAtMs=" + strconv.FormatInt(debt.RefreshOwedAtMs, 10),
		"refreshOwedFloorMs=" + strconv.FormatInt(debt.RefreshOwedFloorMs, 10),
		"nextAttemptAtMs=" + strconv.FormatInt(debt.NextAttemptAtMs, 10),
		"attempts=1",
		"observedAtMs=" + strconv.FormatInt(observed.UnixMilli(), 10),
		"schemaVersion=" + strconv.Itoa(antigravityQuotaSchemaVersion),
		"buckets=1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the diagnostic lacks %q: %q", want, got)
		}
	}
}

// helperOtherAccountDebtID is a debt generation an earlier test's run noted,
// close to now as a real one would be.
func helperOtherAccountDebtID() antigravityDebtID {
	now := time.Now().UnixMilli()
	return antigravityDebtID{floorMs: now, owedAtMs: now}
}

// The disabled-bucket flow, including its post-update restart, gives the same
// result after each leftover the old fixture did not clear. The fixture itself
// is checked first to forget every one of them.
func TestAntigravityStaleRegression_DisabledBucketIsOrderIndependent(t *testing.T) {
	bob := fingerprintAccount("antigravity", "bob@example.com")
	leftovers := []struct {
		name  string
		leave func()
	}{
		{"another account's debt and live-producer notes", func() {
			noteAntigravityDebtAccount(helperOtherAccountDebtID(), bob)
			noteAntigravityLiveProducer(bob, time.Now())
		}},
		{"a debt route already closed", func() {
			id := helperOtherAccountDebtID()
			noteAntigravityDebtRoute(id, antigravityDebtRouteSettle)
			antigravityDebtRoute.mu.Lock()
			antigravityDebtRoute.closed = true
			antigravityDebtRoute.mu.Unlock()
		}},
		{"a spent login-renewal clock", func() {
			antigravityLoginRenewal.mu.Lock()
			antigravityLoginRenewal.lastAt, antigravityLoginRenewal.result = time.Now(), "still_expired"
			antigravityLoginRenewal.mu.Unlock()
		}},
		{"an unplottable attempt mark", func() {
			noteAntigravityCurrentAttempt(helperOtherAccountDebtID(), bob, true)
		}},
	}

	t.Run("the fixture forgets every leftover", func(t *testing.T) {
		for _, l := range leftovers {
			l.leave()
		}
		antigravityDebtAccount.mu.Lock()
		accountID := antigravityDebtAccount.debtID
		antigravityDebtAccount.mu.Unlock()
		antigravityAttempt.mu.Lock()
		attemptID := antigravityAttempt.current.debtID
		antigravityAttempt.mu.Unlock()
		if antigravityDebtAccountFor(accountID) == "" || !antigravityAttemptWasUnplottable(attemptID) {
			t.Fatal("the leftovers were not in place before the fixture")
		}
		ids := []antigravityDebtID{accountID, attemptID}
		helperIsolateAntigravityCapture(t, "20ms")
		for _, id := range ids {
			if got := antigravityDebtAccountFor(id); got != "" {
				t.Errorf("the debt account survived the fixture for %+v", id)
			}
			if antigravityAttemptWasUnplottable(id) {
				t.Errorf("the unplottable attempt mark survived the fixture for %+v", id)
			}
		}
		antigravityDebtRoute.mu.Lock()
		route, closed := antigravityDebtRoute.route, antigravityDebtRoute.closed
		antigravityDebtRoute.mu.Unlock()
		if route != "" || closed {
			t.Errorf("route=%q closed=%v survived the fixture", route, closed)
		}
		antigravityLiveProducer.mu.Lock()
		producer := antigravityLiveProducer.fingerprint
		antigravityLiveProducer.mu.Unlock()
		if producer != "" {
			t.Error("the live producer survived the fixture")
		}
		antigravityLoginRenewal.mu.Lock()
		renewedAt := antigravityLoginRenewal.lastAt
		antigravityLoginRenewal.mu.Unlock()
		if !renewedAt.IsZero() {
			t.Error("the login-renewal clock survived the fixture")
		}
	})

	for _, l := range leftovers {
		t.Run("after "+l.name, func(t *testing.T) {
			l.leave()
			helperAssertDisabledBucketLeavesTheCard(t, helperRunDirectAgy)
		})
	}
}

// A test that leaves a debt worker running is named by the next fixture's
// quiet check, with the redacted state, instead of surfacing later as a
// miscounted Code Assist read. The worker is released before the case ends,
// so nothing outlives it.
func TestAntigravityStaleRegression_StragglerFailsTheTestThatLeaked(t *testing.T) {
	_, cache := helperIsolateAntigravityFreshness(t)
	helperWriteAntigravityCache(t, cache, time.Now().Add(-time.Hour))
	antigravityRefreshMinInterval = time.Nanosecond
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	var releaseOnce sync.Once
	releaseWorker := func() { releaseOnce.Do(func() { close(release) }) }
	reads := helperStubAntigravityCodeAssistOutcome(t, func() string {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		// Terminal on the spot, so the released worker reads exactly once.
		return liveProbeOutcomeCodeAssistNoLogin
	})
	// Registered last, so it runs first: the worker is out before the
	// fixture's own cleanup waits for it.
	t.Cleanup(func() { releaseWorker(); antigravityUsageRefreshWaitIdle() })
	origBound := antigravityStateQuietBound
	antigravityStateQuietBound = 200 * time.Millisecond
	t.Cleanup(func() { antigravityStateQuietBound = origBound })

	t.Run("leaks a blocked debt worker", func(t *testing.T) {
		antigravityUsageRunSettled(time.Now(), 0, false, true)
		select {
		case <-entered:
		case <-time.After(30 * time.Second):
			t.Fatal("the debt worker never started")
		}
		// Ends without releasing it: the straggler.
	})

	t.Run("the next fixture names it", func(t *testing.T) {
		rec := &helperRecordingTB{TB: t}
		requireAntigravityStateQuiet(rec)
		got := rec.recorded()
		if !strings.Contains(got, "not quiet") ||
			!(strings.Contains(got, "the debt worker still holds its claim") || strings.Contains(got, "1 background write(s) in flight")) {
			t.Errorf("recorded=%q, want the held worker named", got)
		}
		if !strings.Contains(got, "workerRunning=true") {
			t.Errorf("recorded=%q, want the redacted state alongside", got)
		}
	})

	releaseWorker()
	if !antigravityUsageRefreshWaitIdleFor(30 * time.Second) {
		t.Fatalf("the released worker never finished [%s]", antigravityStateForTest(cache))
	}
	helperStopAntigravityRefreshSchedule()
	if reason := antigravityStateLeak(); reason != "" {
		t.Errorf("leak=%q once the straggler was released, want quiet", reason)
	}
	if got := reads.Load(); got != 1 {
		t.Errorf("reads=%d, want the straggler's one", got)
	}
}

// The disabled shape under a burst: one Code Assist read for three runs, the
// later runs leave ONE deferred debt with its budget intact and one rung
// within the interval, and the card keeps showing only the enabled 5h row
// while that debt waits (bounded refresh propagation).
func TestAntigravityStaleRegression_DisabledBucketBoundedUnderABurst(t *testing.T) {
	f := helperStaleRegressionFixtureQuota(t, helperStoredLogin(),
		func(string) (int, string) { return http.StatusOK, helperDisabledWeeklyCodeAssistFixture }, helperUserinfoAda)
	antigravityRefreshMinInterval = time.Hour

	for i := 0; i < 3; i++ {
		helperRunDirectAgy(t, f)
	}
	if !antigravityUsageRefreshWaitIdleFor(antigravityStateQuietBound) {
		t.Fatalf("the burst left a write in flight [%s]", antigravityStateForTest(f.cache))
	}

	if got := atomic.LoadInt32(f.quotaCalls); got != 1 {
		t.Errorf("Code Assist reads=%d for a burst, want one [%s]", got, antigravityStateForTest(f.cache))
	}
	state := helperFreshnessState(t)
	if state.RefreshOwedAtMs == 0 || state.Attempts != 0 {
		t.Errorf("state=%+v, want one deferred debt with its budget intact [%s]", state, antigravityStateForTest(f.cache))
	}
	if !antigravityRunDebtRetryPending() {
		t.Error("the deferred debt has nothing scheduled")
	}
	if until := time.Until(time.UnixMilli(state.NextAttemptAtMs)); until > antigravityRefreshMinInterval+time.Second {
		t.Errorf("next attempt is %s away, want it bounded by the interval", until)
	}

	usage := helperGather(t, f.home)
	if len(usage.Metrics) != 1 || usage.Metrics[0].Label != "Gemini — 5-hour session window" ||
		usage.Metrics[0].Remaining == nil || *usage.Metrics[0].Remaining != 96 {
		t.Errorf("metrics=%+v while the debt waits, want only the enabled 5h row at 96%% left", usage.Metrics)
	}
	if !antigravityUsageRefreshWaitIdleFor(antigravityStateQuietBound) {
		t.Fatalf("the gather left a write in flight [%s]", antigravityStateForTest(f.cache))
	}
	if got := atomic.LoadInt32(f.quotaCalls); got != 1 {
		t.Errorf("Code Assist reads=%d after the gather, want still one", got)
	}
}

// Only a PATH entry holding `agy` and none of the suite's tools is dropped;
// the rest keep their order, and an empty entry stays.
func TestPruneRealAntigravityFromPath_DropsOnlyAgyDirs(t *testing.T) {
	exe := func(name string) string {
		if runtime.GOOS == "windows" {
			return name + ".exe"
		}
		return name
	}
	mkdir := func(files ...string) string {
		dir := t.TempDir()
		for _, f := range files {
			if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	plain1, agyOnly, plain2, agyAndGit := mkdir(), mkdir(exe("agy")), mkdir("readme.txt"), mkdir(exe("agy"), exe("git"))
	sep := string(os.PathListSeparator)
	t.Setenv("PATH", strings.Join([]string{plain1, agyOnly, "", plain2, agyAndGit}, sep))
	orig := prunedAntigravityPathEntries
	t.Cleanup(func() { prunedAntigravityPathEntries = orig })

	removed, keptForTools := pruneRealAntigravityFromPath()

	if removed != 1 || keptForTools != 1 {
		t.Errorf("removed=%d keptForTools=%d, want 1 and 1", removed, keptForTools)
	}
	if want := strings.Join([]string{plain1, "", plain2, agyAndGit}, sep); os.Getenv("PATH") != want {
		t.Errorf("PATH=%q, want %q", os.Getenv("PATH"), want)
	}
	if len(prunedAntigravityPathEntries) != 1 || prunedAntigravityPathEntries[0] != agyOnly {
		t.Errorf("pruned=%q, want only the agy-only dir", prunedAntigravityPathEntries)
	}
}
