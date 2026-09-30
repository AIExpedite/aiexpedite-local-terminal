package main

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The regression the CLI-maintenance smokes reported, end to end: on a build
// whose language server refuses loopback quota reads (every current `agy`), a
// green run — direct, or through the Windows encoded-PowerShell transport —
// left the CLI Agents card on a day-old observedAt. After a successful run the
// card must show NUMERIC Antigravity utilization observed after that run.
//
// Real pieces: the spawn paths, the capture arm, the settle, the debt worker,
// the retry schedule, the Code Assist read (against loopback stand-ins for
// Google) and the gather. Stubbed, as in the existing suites: the gate marker
// (the real 401/CSRF refusal) and the keyring login (the real Credential
// Manager entry).

// helperStaleRegression is one gated device: a marker matching the installed
// build, a stored login, Google stand-ins, the smoke's stale cached reading and
// a mock `agy` that exits at once — no language server, because a gated build
// must not be probed for one.
type helperStaleRegression struct {
	home, cache string
	stale       time.Time
	executable  string
	quotaCalls  *int32
}

func helperStaleRegressionFixture(t *testing.T, keyring map[string]any, userinfo func(string) (int, string)) helperStaleRegression {
	t.Helper()
	return helperStaleRegressionFixtureQuota(t, keyring,
		func(string) (int, string) { return http.StatusOK, antigravityCodeAssistFixture }, userinfo)
}

// helperStaleRegressionFixtureQuota is helperStaleRegressionFixture with the
// Code Assist reply chosen by the caller.
func helperStaleRegressionFixtureQuota(t *testing.T, keyring map[string]any, quota, userinfo func(string) (int, string)) helperStaleRegression {
	t.Helper()
	helperStubAntigravityKeyring(t, keyring)
	quotaCalls, _ := helperCodeAssistServers(t, quota, userinfo)
	// After helperCodeAssistServers, which points the cache at its own dir.
	home, cache := helperIsolateAntigravityCapture(t, "20ms")
	helperIsolateAntigravityGate(t)
	// The refusal was recorded before these runs, as on a device that has
	// already met it once: every run here arms with the gate known.
	noteAntigravityQuotaGate("", time.Now().Add(-time.Hour))
	helperSeedStaleAntigravityCache(t, cache)
	_, executable := helperMockAgyOnPath(t, "no-prompt-immediate-exit")
	probeAntigravityQuotaCodeAssistFn = probeAntigravityQuotaCodeAssist
	// Short rungs, so a deferral is followed by its retry inside the test.
	helperPinAntigravityRefreshSchedule(t, 50*time.Millisecond, 20*time.Millisecond)
	origInterval := antigravityRefreshMinInterval
	antigravityRefreshMinInterval = time.Nanosecond
	t.Cleanup(func() { helperStopAntigravityRefreshSchedule(); antigravityRefreshMinInterval = origInterval })

	stale, err := time.Parse(time.RFC3339, helperStaleObservedAt)
	if err != nil {
		t.Fatalf("parse seed: %v", err)
	}
	return helperStaleRegression{home: home, cache: cache, stale: stale, executable: executable, quotaCalls: quotaCalls}
}

func helperStoredLogin() map[string]any {
	return map[string]any{
		"access_token": "access-A", "token_type": "Bearer", "refresh_token": "never-read",
		"expiry": time.Now().Add(30 * time.Minute).Format(time.RFC3339Nano),
	}
}

func helperUserinfoAda(string) (int, string) {
	return http.StatusOK, `{"sub":"123","email":"ada@example.com"}`
}

// helperRunDirectAgy is the direct transport: a tty=false execute of `agy`.
func helperRunDirectAgy(t *testing.T, f helperStaleRegression) {
	t.Helper()
	out, err := executeTerminalCommand(nil, commandMsg{
		Command: f.executable, Args: []string{"--print", "hello"},
		Cwd: t.TempDir(), TimeoutMs: 30000, Tty: false,
	})
	if err != nil {
		t.Fatalf("direct agy execute failed: %v (output=%q)", err, out)
	}
}

// helperRunEncodedPowerShellAgy is the Windows terminal transport the smoke
// really arrives as: `powershell -EncodedCommand <b64>` wrapping the agy call,
// through the runEncodedPowerShellViaArgFn seam so it runs on every OS.
func helperRunEncodedPowerShellAgy(t *testing.T, f helperStaleRegression) {
	t.Helper()
	restore := runEncodedPowerShellViaArgFn
	t.Cleanup(func() { runEncodedPowerShellViaArgFn = restore })
	runEncodedPowerShellViaArgFn = func(encodedScript, workDir string, timeout time.Duration, _ func(int)) (string, error) {
		script, err := decodeBase64PowerShellStrict(encodedScript)
		if err != nil {
			return "", err
		}
		if !strings.Contains(script, "agy") {
			return "", fmt.Errorf("stub received an unexpected script")
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		out, runErr := exec.CommandContext(ctx, f.executable, "--print", "hello").CombinedOutput()
		return string(out), runErr
	}
	script := fmt.Sprintf(`Set-Location %q; & %q --print "hello"`, t.TempDir(), f.executable)
	out, err := runLocalCommandWindows("powershell",
		[]string{"-EncodedCommand", encodeForPowerShell(script)}, t.TempDir(), 30*time.Second)
	if err != nil {
		t.Fatalf("encoded-PowerShell execute failed: %v (output=%q)", err, out)
	}
}

// The acceptance: for both transports, a green run on a gated build ends with
// the card showing numeric utilization observed after that run — with no
// loopback poller started for a build known to refuse it.
func TestAntigravityStaleRegression_GatedRunPublishesAFreshNumericReading(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T, helperStaleRegression)
	}{
		{"direct agy", helperRunDirectAgy},
		{"windows encoded powershell", helperRunEncodedPowerShellAgy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := helperStaleRegressionFixture(t, helperStoredLogin(), helperUserinfoAda)

			startedAt := time.Now()
			tc.run(t, f)
			helperDrainAntigravityRefreshSchedule(t)
			snap := helperAwaitPaidRefresh(t, f.cache, f.stale)

			if got := antigravityCaptureArms.Load(); got != 1 {
				t.Errorf("arms=%d, want the run armed exactly once", got)
			}
			if got := antigravityCaptureTailProbes.Load(); got != 0 {
				t.Errorf("tailProbes=%d on a gated build, want no poller at all", got)
			}
			if got := atomic.LoadInt32(f.quotaCalls); got != 1 {
				t.Errorf("Code Assist reads=%d, want exactly one for one run", got)
			}
			// The debt was cleared (helperAwaitPaidRefresh), which the settle
			// hook only does for a reading at or after the run's completion.
			if antigravitySnapshotObservedMs(snap) < startedAt.UnixMilli() {
				t.Errorf("observedAt=%s predates the run", snap.ObservedAt)
			}

			usage, observed := helperParsedObservedAt(t, f.home, time.Now())
			if !observed.After(f.stale) {
				t.Fatalf("observedAt=%s did not advance past the stale %s", observed, f.stale)
			}
			if observed.Before(startedAt.Truncate(time.Second)) {
				t.Errorf("card observedAt=%s is before the run started at %s", observed, startedAt)
			}
			for _, m := range usage.Metrics {
				if m.Unknown || m.Consumed == nil || m.Remaining == nil {
					t.Errorf("metric %+v is not numeric", m)
				}
			}
			if usage.Notice != "" {
				t.Errorf("notice=%q, want none once the run's reading landed", usage.Notice)
			}
		})
	}
}

// Google answers but the reading cannot be attributed to an account: the debt
// must stay alive and keep retrying on the ladder, never be mistaken for paid,
// and stop at the lifetime budget.
func TestAntigravityStaleRegression_UnattributableReadingKeepsTheDebt(t *testing.T) {
	f := helperStaleRegressionFixture(t, helperStoredLogin(),
		func(string) (int, string) { return http.StatusInternalServerError, `{}` })

	helperRunDirectAgy(t, f)
	helperDrainAntigravityRefreshSchedule(t)

	state := helperFreshnessState(t)
	if state.RefreshOwedAtMs == 0 {
		t.Fatal("an unattributable reading retired the debt")
	}
	if state.Outcome != liveProbeOutcomeCodeAssistNotSigned {
		t.Errorf("outcome=%q, want %q", state.Outcome, liveProbeOutcomeCodeAssistNotSigned)
	}
	if got := atomic.LoadInt32(f.quotaCalls); got != antigravityRefreshDebtMaxAttempts {
		t.Errorf("Code Assist reads=%d, want the lifetime budget %d and no more", got, antigravityRefreshDebtMaxAttempts)
	}
	if state.NextAttemptAtMs != 0 || antigravityRunDebtRetryPending() {
		t.Errorf("state=%+v, want nothing booked once the budget is spent", state)
	}
	if _, observed := helperParsedObservedAt(t, f.home, time.Now()); !observed.Equal(f.stale) {
		t.Errorf("observedAt=%s, want the stale reading kept rather than an unattributed one", observed)
	}
}

// No stored login: nothing on the device can pay the debt, so it is terminal —
// no outbound read, no rung — and the card says why instead of staying quiet.
func TestAntigravityStaleRegression_NoLoginIsTerminalAndSaysSo(t *testing.T) {
	f := helperStaleRegressionFixture(t, nil, helperUserinfoAda)

	helperRunDirectAgy(t, f)
	helperDrainAntigravityRefreshSchedule(t)

	state := helperFreshnessState(t)
	if state.RefreshOwedAtMs == 0 || state.Attempts != antigravityRefreshDebtMaxAttempts || state.NextAttemptAtMs != 0 {
		t.Errorf("state=%+v, want a kept, spent, unscheduled no_login debt", state)
	}
	if got := atomic.LoadInt32(f.quotaCalls); got != 0 {
		t.Errorf("Code Assist reads=%d with no login, want none", got)
	}
	usage, _ := helperParsedObservedAt(t, f.home, time.Now())
	if usage.Notice == "" || usage.NoticeSeverity != "warning" {
		t.Errorf("notice=%q severity=%q, want the card to explain the stale figure", usage.Notice, usage.NoticeSeverity)
	}
}

// Two smokes inside one minimum interval: the second run's payment is deferred
// by the interval, and the schedule — not a later run — pays it, so BOTH runs
// end covered.
func TestAntigravityStaleRegression_TwoRunsInsideOneIntervalBothEndCovered(t *testing.T) {
	f := helperStaleRegressionFixture(t, helperStoredLogin(), helperUserinfoAda)
	// Longer than the gap between the two runs below, so the second one's
	// payment is genuinely deferred by it.
	antigravityRefreshMinInterval = 3 * time.Second

	helperRunDirectAgy(t, f)
	antigravityUsageRefreshWaitIdle()
	if got := atomic.LoadInt32(f.quotaCalls); got != 1 {
		t.Fatalf("Code Assist reads=%d after the first run, want 1", got)
	}
	// observedAt has one-second resolution on the card; the second reading
	// has to land in a later second to be provably a new observation.
	time.Sleep(1100 * time.Millisecond)
	secondStarted := time.Now()
	helperRunDirectAgy(t, f)
	antigravityUsageRefreshWaitIdle()
	if got := atomic.LoadInt32(f.quotaCalls); got != 1 {
		t.Fatalf("Code Assist reads=%d, want the interval to defer the second run's read", got)
	}
	if state := helperFreshnessState(t); state.RefreshOwedAtMs == 0 || state.NextAttemptAtMs == 0 {
		t.Fatalf("state=%+v, want the deferred debt kept and scheduled", state)
	}

	// The booked rung waits out the interval, then pays.
	helperDrainAntigravityRefreshSchedule(t)
	snap := helperAwaitPaidRefresh(t, f.cache, f.stale)
	if got := atomic.LoadInt32(f.quotaCalls); got != 2 {
		t.Errorf("Code Assist reads=%d, want exactly one per run", got)
	}
	if antigravitySnapshotObservedMs(snap) < secondStarted.UnixMilli() {
		t.Errorf("observedAt=%s predates the second run", snap.ObservedAt)
	}
}

// settings.json names an account the keyring login does not. The reading the
// debt worker landed must still replay minutes later — after the click-sized
// producer window — instead of the card falling back to Unknown rows.
func TestAntigravityStaleRegression_ReadingOutlivesAStaleSettingsAccount(t *testing.T) {
	f := helperStaleRegressionFixture(t, helperStoredLogin(), helperUserinfoAda)
	helperWriteJSON(t, filepath.Join(f.home, ".gemini", "antigravity-cli", "settings.json"),
		map[string]any{"email": "bob@example.com"})
	resetAntigravityLiveProducer(t)

	helperRunDirectAgy(t, f)
	helperDrainAntigravityRefreshSchedule(t)
	helperAwaitPaidRefresh(t, f.cache, f.stale)

	// Minutes later: the producer is older than antigravityLiveProducerTTL.
	antigravityLiveProducer.mu.Lock()
	antigravityLiveProducer.at = time.Now().Add(-antigravityLiveProducerTTL - 5*time.Minute)
	antigravityLiveProducer.mu.Unlock()

	usage, observed := helperParsedObservedAt(t, f.home, time.Now())
	if !observed.After(f.stale) || usage.Account != "ada@example.com" {
		t.Fatalf("account=%q observedAt=%s, want the stored login's fresh reading", usage.Account, observed)
	}
	for _, m := range usage.Metrics {
		if m.Unknown {
			t.Errorf("metric %+v fell back to Unknown", m)
		}
	}

	// A restart or self-update discards the in-process note entirely. The
	// reading itself records that the stored login produced it, so the card
	// still replays it instead of falling back to Unknown rows.
	resetAntigravityLiveProducer(t)
	usage, observed = helperParsedObservedAt(t, f.home, time.Now())
	if !observed.After(f.stale) || usage.Account != "ada@example.com" {
		t.Fatalf("after a restart account=%q observedAt=%s, want the stored login's fresh reading",
			usage.Account, observed)
	}
	for _, m := range usage.Metrics {
		if m.Unknown {
			t.Errorf("after a restart metric %+v fell back to Unknown", m)
		}
	}

	// The contrast: an aged attestation from a loopback probe (a Refresh click)
	// does not outrank settings.json, exactly as before. Such a reading carries
	// no StoredLoginRead — only the Code Assist route writes it — so the cached
	// one is rewritten here as the click would have left it.
	helperClearStoredLoginAttestation(t, f.cache)
	noteAntigravityLiveProducerForTest(fingerprintAccount("antigravity", "ada@example.com"),
		time.Now().Add(-antigravityLiveProducerTTL-5*time.Minute))
	usage, _ = antigravityUsageParser{}.Parse(f.home, detectedCLIAgent{Detected: true}, time.Now())
	if len(usage.Metrics) == 0 || !usage.Metrics[0].Unknown {
		t.Errorf("an aged loopback attestation replayed %q's reading under settings.json's account", usage.Account)
	}
}

// helperClearStoredLoginAttestation rewrites the cached reading as a loopback
// probe would have left it: same numbers and account, no route attestation.
func helperClearStoredLoginAttestation(t *testing.T, cache string) {
	t.Helper()
	var snap antigravityQuotaSnapshot
	if !readJSONFile(cache, &snap) {
		t.Fatal("no snapshot cached")
	}
	if !snap.StoredLoginRead {
		t.Fatal("the Code Assist reading did not record the stored login as its producer")
	}
	snap.StoredLoginRead = false
	helperWriteJSON(t, cache, snap)
}

// A burst of runs costs at most one outbound read per interval: every later
// run's debt is deferred onto the same single timer, never multiplied.
func TestAntigravityStaleRegression_ABurstOfRunsIsBounded(t *testing.T) {
	f := helperStaleRegressionFixture(t, helperStoredLogin(), helperUserinfoAda)
	antigravityRefreshMinInterval = time.Hour

	for i := 0; i < 6; i++ {
		helperRunDirectAgy(t, f)
	}
	antigravityUsageRefreshWaitIdle()

	if got := atomic.LoadInt32(f.quotaCalls); got != 1 {
		t.Errorf("Code Assist reads=%d for a burst, want one", got)
	}
	state := helperFreshnessState(t)
	if state.RefreshOwedAtMs == 0 || state.Attempts != 0 {
		t.Errorf("state=%+v, want one deferred debt with its budget intact", state)
	}
	if !antigravityRunDebtRetryPending() {
		t.Error("the deferred debt has nothing scheduled")
	}
	if until := time.Until(time.UnixMilli(state.NextAttemptAtMs)); until > antigravityRefreshMinInterval+time.Second {
		t.Errorf("next attempt is %s away, want it bounded by the interval", until)
	}
}

/* ─────────── discovery, managed-run evidence and update survival ─────────── */

// helperPinFreshnessClock pins the freshness clock (the discovery tick, the
// nudge and the debt worker all read it) and returns a setter.
func helperPinFreshnessClock(t *testing.T, at time.Time) func(time.Time) {
	t.Helper()
	orig := antigravityUsageFreshnessNow
	t.Cleanup(func() { antigravityUsageFreshnessNow = orig })
	var mu sync.Mutex
	antigravityUsageFreshnessNow = func() time.Time { mu.Lock(); defer mu.Unlock(); return at }
	return func(next time.Time) { mu.Lock(); at = next; mu.Unlock() }
}

// helperRunPlainAgy is a DIRECT run: the user's own shell, a plain exec with
// no capture, no gather and no cloud loop.
func helperRunPlainAgy(t *testing.T, f helperStaleRegression) {
	t.Helper()
	cmd := exec.Command(f.executable, "--print", "hello")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("plain agy run failed: %v (%s)", err, out)
	}
}

// DirectRunDiscoveredWithoutGather: nothing classifies the run and nothing
// gathers. The discovery tick alone finds it, holds it until it has exited and
// settled, owes it, and the debt worker lands a numeric reading within
// exit + 2.5 min on the pinned clock.
func TestAntigravityStaleRegression_DirectRunDiscoveredWithoutGather(t *testing.T) {
	f := helperStaleRegressionFixture(t, helperStoredLogin(), helperUserinfoAda)
	t.Setenv(mockCLIEnvVar, "antigravity-pid-block")
	resetAntigravityLogIndex()
	t.Cleanup(resetAntigravityLogIndex)

	exit := time.Now()
	helperRunPlainAgy(t, f)
	setClock := helperPinFreshnessClock(t, exit.Add(time.Second))
	antigravityDiscoveryTick()
	if got := atomic.LoadInt32(f.quotaCalls); got != 0 {
		t.Fatalf("Code Assist reads=%d before the run settled", got)
	}
	setClock(exit.Add(61 * time.Second))
	antigravityDiscoveryTick()
	helperDrainAntigravityRefreshSchedule(t)

	snap := helperAwaitPaidRefresh(t, f.cache, f.stale)
	landed := time.UnixMilli(antigravitySnapshotObservedMs(snap))
	if landed.Before(exit.Truncate(time.Second)) || landed.After(exit.Add(150*time.Second)) {
		t.Errorf("reading landed at %s, want within exit + 2.5 min of %s", landed, exit)
	}
	if got := atomic.LoadInt32(f.quotaCalls); got < 1 || got > 2 {
		t.Errorf("Code Assist reads=%d, want at most two for one run", got)
	}
	usage, _ := helperParsedObservedAt(t, f.home, time.Now())
	for _, m := range usage.Metrics {
		if m.Unknown || m.Consumed == nil {
			t.Errorf("metric %+v is not numeric", m)
		}
	}
}

// helperAllExhaustedReply is Google's reply for an account with nothing left:
// every fraction omitted. The 5h bucket resets 1h39m21s from now, the reset
// the mock's own 429 names.
func helperAllExhaustedReply() string {
	reset := time.Now().Add(time.Hour + 39*time.Minute + 21*time.Second).UTC().Format(time.RFC3339)
	weekly := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	return `{"groups":[{"displayName":"Gemini Models","buckets":[` +
		`{"bucketId":"gemini-5h","window":"5h","resetTime":"` + reset + `"},` +
		`{"bucketId":"gemini-weekly","window":"weekly","resetTime":"` + weekly + `"}]}]}`
}

func helperUserinfoBob(string) (int, string) {
	return http.StatusOK, `{"sub":"456","email":"bob@example.com"}`
}

// AllExhaustedReply: a MANAGED turn whose own log block holds account B's 429
// makes B's all-omitted reply chartable (the 5h bucket at 100% consumed). The
// same reply after a restart (evidence is in memory only) stays unknown, the
// recorded limitation.
func TestAntigravityStaleRegression_AllExhaustedReply(t *testing.T) {
	f := helperStaleRegressionFixtureQuota(t, helperStoredLogin(),
		func(string) (int, string) { return http.StatusOK, helperAllExhaustedReply() }, helperUserinfoBob)
	t.Setenv(mockCLIEnvVar, "antigravity-pid-block")
	t.Setenv(mockAgyExhaustedEnv, "bob@example.com")
	resetAntigravityLogIndex()
	resetAntigravityExhaustionEvidence()
	t.Cleanup(func() { resetAntigravityLogIndex(); resetAntigravityExhaustionEvidence() })

	helperRunDirectAgy(t, f) // tty=false execute: a managed run whose PID the capture holds
	helperDrainAntigravityRefreshSchedule(t)
	snap := helperAwaitPaidRefresh(t, f.cache, f.stale)
	if snap.Account != "bob@example.com" || len(snap.Buckets) != 1 || snap.Buckets[0].RemainingFraction != 0 {
		t.Fatalf("snap=%+v, want bob's 5h bucket charted as fully consumed", snap)
	}
	usage, _ := helperParsedObservedAt(t, f.home, time.Now())
	if len(usage.Metrics) != 1 || usage.Metrics[0].Consumed == nil || *usage.Metrics[0].Consumed != 100 {
		t.Errorf("metrics=%+v, want the exhausted bucket numeric at 100%%", usage.Metrics)
	}

	// After a self-update the evidence is gone: the same reply is unplottable.
	resetAntigravityExhaustionEvidence()
	if got := probeAntigravityQuotaCodeAssist(context.Background(), "1.2.3", time.Now); got != liveProbeOutcomeCodeAssistBadResponse {
		t.Errorf("outcome=%q after a restart, want the reply unplottable", got)
	}
}

// The direct-run half of the same case: a user-shell run's 429 is never
// evidence, so its debt is paid with an unplottable reply and the card keeps
// its older reading.
func TestAntigravityStaleRegression_AllExhaustedAfterADirectRunStaysUnknown(t *testing.T) {
	f := helperStaleRegressionFixtureQuota(t, helperStoredLogin(),
		func(string) (int, string) { return http.StatusOK, helperAllExhaustedReply() }, helperUserinfoBob)
	t.Setenv(mockCLIEnvVar, "antigravity-pid-block")
	t.Setenv(mockAgyExhaustedEnv, "bob@example.com")
	resetAntigravityLogIndex()
	resetAntigravityExhaustionEvidence()
	t.Cleanup(func() { resetAntigravityLogIndex(); resetAntigravityExhaustionEvidence() })

	exit := time.Now()
	helperRunPlainAgy(t, f)
	setClock := helperPinFreshnessClock(t, exit.Add(time.Second))
	antigravityDiscoveryTick()
	setClock(exit.Add(61 * time.Second))
	antigravityDiscoveryTick()
	antigravityUsageRefreshWaitIdle()
	helperStopAntigravityRefreshSchedule()

	if got := atomic.LoadInt32(f.quotaCalls); got < 1 {
		t.Fatal("the direct run's debt was never paid")
	}
	if state := helperFreshnessState(t); state.Outcome != liveProbeOutcomeCodeAssistBadResponse {
		t.Errorf("outcome=%q, want the reply recorded as unplottable", state.Outcome)
	}
	if _, observed := helperParsedObservedAt(t, f.home, time.Now()); !observed.Equal(f.stale) {
		t.Errorf("observedAt=%s, want the older reading kept", observed)
	}
}

// ConcurrentMultiAccountEvidence: two managed runs share one second-stamped
// log; only A's block holds the 429. Only A gets evidence, and B's identical
// reset is never charted from A's block.
func TestAntigravityEvidence_OnlyTheRunsOwnBlockCounts(t *testing.T) {
	h := helperIsolateLogIndex(t)
	now := time.Now().Truncate(time.Second)
	h.write(t, helperLogName(now),
		helperPIDBlock(60001)+"authenticated successfully as ada@example.com\n"+
			"RESOURCE_EXHAUSTED (code 429): exhausted. Resets in 1h39m21s.\n"+
			helperPIDBlock(60002)+"authenticated successfully as bob@example.com\n",
		now, false)
	settled := now.Add(5 * time.Second)
	recordAntigravityRunEvidence(now, 60001, settled)
	recordAntigravityRunEvidence(now, 60002, settled)
	if got := antigravityExhaustionEvidence(settled, fingerprintAccount("antigravity", "ada@example.com")); len(got) != 1 {
		t.Errorf("ada evidence=%v, want one event", got)
	}
	if got := antigravityExhaustionEvidence(settled, fingerprintAccount("antigravity", "bob@example.com")); len(got) != 0 {
		t.Errorf("bob evidence=%v, want none from ada's block", got)
	}
	if got := antigravityExhaustionEvidence(settled.Add(antigravityExhaustionTTL+time.Second), fingerprintAccount("antigravity", "ada@example.com")); len(got) != 0 {
		t.Error("evidence outlived its TTL")
	}
}

// MaintenanceSmokeSurvivesUpdate: the Antigravity maintenance smoke arrives
// as a Windows encoded-PowerShell execute. The agent is told to shut down the
// moment the command returns; its payment is stuck, so the bounded drain gives
// up. The next process adopts the persisted debt and the card ends numeric,
// observed after the smoke.
func TestAntigravityStaleRegression_MaintenanceSmokeSurvivesUpdate(t *testing.T) {
	f := helperStaleRegressionFixture(t, helperStoredLogin(), helperUserinfoAda)
	resetAntigravityLogIndex()
	t.Cleanup(resetAntigravityLogIndex)
	_, entered, release := helperBlockingCodeAssistStub(t, liveProbeOutcomeCodeAssistHTTPError)
	origDrain := antigravityShutdownDrain
	antigravityShutdownDrain = 200 * time.Millisecond
	t.Cleanup(func() { antigravityShutdownDrain = origDrain })

	smokeStarted := time.Now()
	helperRunEncodedPowerShellAgy(t, f)
	<-entered
	drained := time.Now()
	drainAntigravityUsageWrites()
	if waited := time.Since(drained); waited > 2*time.Second {
		t.Errorf("the drain waited %s on a stuck write, want it bounded", waited)
	}
	release()
	antigravityUsageRefreshWaitIdle()
	stopAntigravityRunDebtRetry()
	if state := helperFreshnessState(t); state.RefreshOwedAtMs == 0 && state.RunFloorMs == 0 {
		t.Fatalf("state=%+v, want the smoke's debt or floor on disk for the next process", state)
	}

	// The next process (after the update): in-process state is gone, the real
	// Code Assist read answers, and StartAgent's replay adopts the debt.
	helperResetAntigravityLiveRuns()
	probeAntigravityQuotaCodeAssistFn = probeAntigravityQuotaCodeAssist
	adoptAndPayOwedAntigravityRunDebt(time.Now())
	helperDrainAntigravityRefreshSchedule(t)
	snap := helperAwaitPaidRefresh(t, f.cache, f.stale)
	if antigravitySnapshotObservedMs(snap) < smokeStarted.UnixMilli() {
		t.Errorf("observedAt=%s predates the smoke", snap.ObservedAt)
	}
	usage, _ := helperParsedObservedAt(t, f.home, time.Now())
	for _, m := range usage.Metrics {
		if m.Unknown || m.Consumed == nil {
			t.Errorf("metric %+v is not numeric after the update", m)
		}
	}
}
