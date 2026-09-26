package main

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
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
	helperStubAntigravityKeyring(t, keyring)
	quotaCalls, _ := helperCodeAssistServers(t,
		func(string) (int, string) { return http.StatusOK, antigravityCodeAssistFixture },
		userinfo)
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
	runEncodedPowerShellViaArgFn = func(encodedScript, workDir string, timeout time.Duration) (string, error) {
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
