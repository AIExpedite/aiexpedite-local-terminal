package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_propagate_test.go — the bounded usage-hint sender. Every row
   records hints through the sendCLIUsageObservedHint seam, so nothing reaches
   the network; the durations are pinned to milliseconds. Run with -race.
   ------------------------------------------------------------------------ */

type recordedCLIUsageHint struct {
	url  string
	hint cliUsageObservedHint
}

// cliUsageHintRecorder stands in for terminal-service: it records each hint
// and answers with status(hint), 202 by default.
type cliUsageHintRecorder struct {
	mu     sync.Mutex
	hints  []recordedCLIUsageHint
	status func(cliUsageObservedHint) int
}

func (r *cliUsageHintRecorder) all() []recordedCLIUsageHint {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedCLIUsageHint(nil), r.hints...)
}

// propagatorFixture pins the schedule small, records hints, sets a
// registration URL and a registered config, and stops the propagator before
// the Codex fixture's environment is undone.
func propagatorFixture(t *testing.T) (*cliUsageHintRecorder, *Config) {
	t.Helper()
	prev := []time.Duration{cliUsageHintDebounce, cliUsageHintSpacing, cliUsageRotationFirstRetry, cliUsageRotationRetryPeriod}
	cliUsageHintDebounce = 20 * time.Millisecond
	cliUsageHintSpacing = 150 * time.Millisecond
	cliUsageRotationFirstRetry = 40 * time.Millisecond
	cliUsageRotationRetryPeriod = 60 * time.Millisecond
	t.Setenv("TERMINAL_SERVICE_URL", "https://terminal.example.test")
	// The startup rotation writes the Codex cache: never the machine's own.
	if os.Getenv("AIEXPEDITE_CODEX_RL_CACHE") == "" {
		t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", t.TempDir()+"/codex_rate_limits.json")
		t.Setenv("CODEX_HOME", t.TempDir())
	}
	rec := &cliUsageHintRecorder{}
	prevSend := sendCLIUsageObservedHint
	sendCLIUsageObservedHint = func(_ context.Context, url string, hint cliUsageObservedHint) int {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.hints = append(rec.hints, recordedCLIUsageHint{url: url, hint: hint})
		if rec.status != nil {
			return rec.status(hint)
		}
		return 202
	}
	t.Cleanup(func() {
		stopCLIUsagePropagator()
		resetCLIUsagePropagator()
		sendCLIUsageObservedHint = prevSend
		cliUsageHintDebounce, cliUsageHintSpacing, cliUsageRotationFirstRetry, cliUsageRotationRetryPeriod = prev[0], prev[1], prev[2], prev[3]
	})
	return rec, &Config{AgentID: "agent-1", CommandSecret: "secret"}
}

// waitHints waits until at least n hints were recorded, then a quiet spell of
// quiet, and returns them all.
func waitHints(t *testing.T, rec *cliUsageHintRecorder, n int, quiet time.Duration) []recordedCLIUsageHint {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(rec.all()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("got %d hints, want at least %d: %+v", len(rec.all()), n, rec.all())
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(quiet)
	return rec.all()
}

func gen(epoch, counter int64) cliUsageGeneration {
	return cliUsageGeneration{Epoch: epoch, Counter: counter}
}

// markRotated stands in for a committed write carrying this process's epoch.
func markRotated(t *testing.T) {
	t.Helper()
	codexGenerationRotated.Store(true)
	noteCLIUsageGenerationRotated()
}

func TestCLIUsageHint_GoldenSignatureVector(t *testing.T) {
	data, err := os.ReadFile("testdata/cli_usage_observed_hint_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Vectors []struct {
			AgentID, Provider, Secret, Message, Signature string
			Timestamp, GenerationEpoch, Generation        int64
		} `json:"vectors"`
	}
	if err := json.Unmarshal(data, &vectors); err != nil || len(vectors.Vectors) == 0 {
		t.Fatalf("vectors: %v", err)
	}
	for _, v := range vectors.Vectors {
		msg := buildCLIUsageObservedSignedMessage(v.AgentID, v.Timestamp, v.Provider, gen(v.GenerationEpoch, v.Generation))
		if msg != v.Message {
			t.Fatalf("message = %q, want %q", msg, v.Message)
		}
		if sig := generateHMAC(msg, v.Secret); sig != v.Signature {
			t.Fatalf("signature = %s, want %s", sig, v.Signature)
		}
	}
}

// A burst sends ONE hint carrying the newest generation, then exactly one
// follow-up at the next spacing boundary, then nothing.
func TestCLIUsageHint_BurstSendsOneHintAndOneFollowUp(t *testing.T) {
	withCodexGenerationEpoch(t, 501)
	rec, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)
	markRotated(t)

	for c := int64(1); c <= 5; c++ {
		noteCLIUsageObservationAdvanced(codexUsageProvider, gen(501, c))
	}
	hints := waitHints(t, rec, 2, 3*cliUsageHintSpacing)
	if len(hints) != 2 {
		t.Fatalf("got %d hints, want the hint and its one follow-up: %+v", len(hints), hints)
	}
	for _, h := range hints {
		if h.hint.GenerationEpoch != 501 || h.hint.Generation != 5 || h.hint.Provider != "codex" {
			t.Fatalf("hint = %+v, want the newest generation {501,5}", h.hint)
		}
		if h.url != "https://terminal.example.test/device/agent-1/cli-usage/observed" {
			t.Fatalf("url = %s", h.url)
		}
		msg := buildCLIUsageObservedSignedMessage("agent-1", h.hint.Timestamp, h.hint.Provider, gen(h.hint.GenerationEpoch, h.hint.Generation))
		if h.hint.Signature != generateHMAC(msg, "secret") {
			t.Fatal("hint signature does not cover every field")
		}
	}
	if gap := time.UnixMilli(hints[1].hint.Timestamp).Sub(time.UnixMilli(hints[0].hint.Timestamp)); gap < cliUsageHintSpacing-10*time.Millisecond {
		t.Fatalf("follow-up came %s after the hint, want >= %s", gap, cliUsageHintSpacing)
	}
}

// A later burst inside the spacing is deferred — not dropped — and sent once
// with its newest generation, followed by its own single follow-up.
func TestCLIUsageHint_ALaterBurstInsideTheSpacingIsDeferredNotDropped(t *testing.T) {
	withCodexGenerationEpoch(t, 502)
	rec, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)
	markRotated(t)

	noteCLIUsageObservationAdvanced(codexUsageProvider, gen(502, 1))
	waitHints(t, rec, 1, 0)
	noteCLIUsageObservationAdvanced(codexUsageProvider, gen(502, 2))
	noteCLIUsageObservationAdvanced(codexUsageProvider, gen(502, 3))

	hints := waitHints(t, rec, 3, 3*cliUsageHintSpacing)
	var got []int64
	for _, h := range hints {
		got = append(got, h.hint.Generation)
	}
	if len(got) != 3 || got[0] != 1 || got[1] != 3 || got[2] != 3 {
		t.Fatalf("generations sent = %v, want [1 3 3]", got)
	}
}

// A failed send still earns the follow-up.
func TestCLIUsageHint_AFailedSendStillProducesTheFollowUp(t *testing.T) {
	withCodexGenerationEpoch(t, 503)
	rec, cfg := propagatorFixture(t)
	rec.status = func(cliUsageObservedHint) int { return 503 }
	startCLIUsagePropagator(cfg)
	markRotated(t)

	noteCLIUsageObservationAdvanced(codexUsageProvider, gen(503, 1))
	if hints := waitHints(t, rec, 2, 2*cliUsageHintSpacing); len(hints) != 2 {
		t.Fatalf("got %d hints, want 2", len(hints))
	}
}

// Offline, draining and unregistered states hold the observation; it goes out
// at the next boundary once the gate opens, with a secret rotated meanwhile.
func TestCLIUsageHint_GatesKeepTheObservationPending(t *testing.T) {
	withCodexGenerationEpoch(t, 504)
	rec, cfg := propagatorFixture(t)
	cfg.AgentID, cfg.CommandSecret = "", ""
	setCodexTestOffline(t, true)
	startCLIUsagePropagator(cfg)
	markRotated(t)
	noteCLIUsageObservationAdvanced(codexUsageProvider, gen(504, 1))

	time.Sleep(2 * cliUsageHintSpacing)
	if n := len(rec.all()); n != 0 {
		t.Fatalf("sent %d hints while offline", n)
	}
	setCodexTestOffline(t, false)
	drain.mu.Lock()
	drain.draining = true
	drain.mu.Unlock()
	t.Cleanup(func() { drain.mu.Lock(); drain.draining = false; drain.mu.Unlock() })
	time.Sleep(2 * cliUsageHintSpacing)
	if n := len(rec.all()); n != 0 {
		t.Fatalf("sent %d hints while draining", n)
	}
	drain.mu.Lock()
	drain.draining = false
	drain.mu.Unlock()
	time.Sleep(2 * cliUsageHintSpacing)
	if n := len(rec.all()); n != 0 {
		t.Fatalf("sent %d hints while unregistered", n)
	}

	// Registration lands with a fresh secret.
	if err := cfg.MutateAndSave(t.TempDir()+"/config.json", func() {
		cfg.AgentID, cfg.CommandSecret = "agent-2", "rotated"
	}); err != nil {
		t.Fatal(err)
	}
	h := waitHints(t, rec, 1, 0)[0]
	if !strings.Contains(h.url, "/device/agent-2/") {
		t.Fatalf("url = %s, want the new registration", h.url)
	}
	msg := buildCLIUsageObservedSignedMessage("agent-2", h.hint.Timestamp, "codex", gen(504, 1))
	if h.hint.Signature != generateHMAC(msg, "rotated") {
		t.Fatal("the rotated secret was not used")
	}
}

func TestCLIUsageHint_NothingIsSentAfterStop(t *testing.T) {
	withCodexGenerationEpoch(t, 505)
	rec, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)
	markRotated(t)
	noteCLIUsageObservationAdvanced(codexUsageProvider, gen(505, 1))
	stopCLIUsagePropagator()
	noteCLIUsageObservationAdvanced(codexUsageProvider, gen(505, 2))
	time.Sleep(2 * cliUsageHintSpacing)
	if n := len(rec.all()); n != 0 {
		t.Fatalf("sent %d hints after stop", n)
	}
}

// An observation noted before the propagator starts (a frame captured during
// boot) is not lost.
func TestCLIUsageHint_AnObservationNotedDuringBootIsSentAfterStart(t *testing.T) {
	withCodexGenerationEpoch(t, 506)
	rec, cfg := propagatorFixture(t)
	markRotated(t)
	noteCLIUsageObservationAdvanced(codexUsageProvider, gen(506, 1))
	startCLIUsagePropagator(cfg)
	if h := waitHints(t, rec, 1, 0)[0]; h.hint.Generation != 1 || h.hint.GenerationEpoch != 506 {
		t.Fatalf("hint = %+v", h.hint)
	}
}

// Startup recovery against a recorded server that applies the first refresh:
// two hints, exactly one dispatch; the follow-up answers already_applied.
func TestCLIUsageHint_StartupRecoveryHintsTheRotatedGeneration(t *testing.T) {
	withCodexGenerationEpoch(t, 507)
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-time.Hour))
	codexLiveReadAt(t, f, 31, 41, now) // captured by the PREVIOUS process
	rec, cfg := propagatorFixture(t)

	var applied *cliUsageGeneration
	dispatched := 0
	rec.status = func(h cliUsageObservedHint) int {
		g := gen(h.GenerationEpoch, h.Generation)
		if applied == nil || !backendAlreadyApplied(*applied, g) {
			dispatched++
			applied = &g // the refresh it dispatched lands
		}
		return 202
	}

	simulateCodexProcessRestart(t, 608)
	startCLIUsagePropagator(cfg)
	hints := waitHints(t, rec, 2, 2*cliUsageHintSpacing)
	if len(hints) != 2 || dispatched != 1 {
		t.Fatalf("hints=%d dispatched=%d, want 2 hints and 1 dispatch", len(hints), dispatched)
	}
	if h := hints[0].hint; h.GenerationEpoch != 608 || h.Generation != 1 {
		t.Fatalf("recovery hint = %+v, want the rotated {608,1}", h)
	}
}

func TestCLIUsageHint_StartupWithoutANumericReadingSchedulesNothing(t *testing.T) {
	withCodexGenerationEpoch(t, 509)
	now := time.Now()
	newCodexFreshnessFixture(t, now.Add(-time.Hour))
	rec, cfg := propagatorFixture(t)
	simulateCodexProcessRestart(t, 610)
	startCLIUsagePropagator(cfg)
	time.Sleep(2 * cliUsageHintSpacing)
	if n := len(rec.all()); n != 0 {
		t.Fatalf("sent %d hints for an empty cache", n)
	}
}

// The first rotation write fails: no hint, nothing published. The retry
// commits, and the recovered generation is hinted and carried by the receipt
// with numeric metrics.
func TestCLIUsageHint_ARefusedRotationIsRetriedThenRecovered(t *testing.T) {
	withCodexGenerationEpoch(t, 511)
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-time.Hour))
	stubCodexLogin(t, true, true)
	codexLiveReadAt(t, f, 32, 42, now)
	rec, cfg := propagatorFixture(t)

	var failMu sync.Mutex
	fail := true
	prevCommit := codexCommitRateLimitSnapshot
	codexCommitRateLimitSnapshot = func(path string, out []byte, now time.Time) bool {
		failMu.Lock()
		defer failMu.Unlock()
		if fail {
			fail = false
			return false
		}
		return prevCommit(path, out, now)
	}
	t.Cleanup(func() { codexCommitRateLimitSnapshot = prevCommit })

	simulateCodexProcessRestart(t, 612)
	startCLIUsagePropagator(cfg)
	time.Sleep(cliUsageRotationFirstRetry / 2)
	if n := len(rec.all()); n != 0 {
		t.Fatalf("hinted %d times before the rotation committed", n)
	}
	if _, g := codexMetricsAndGenerationFromCache(time.Now(), f.fp); g != nil {
		t.Fatalf("published %+v before the rotation committed", g)
	}

	h := waitHints(t, rec, 1, 0)[0].hint
	if h.GenerationEpoch != 612 {
		t.Fatalf("hint = %+v, want the recovered epoch 612", h)
	}
	usage, _ := codexUsageParser{}.ParseContext(context.Background(), f.home, detectedCLIAgent{Path: "codex"}, time.Now())
	if usage.UsageGeneration == nil || usage.UsageGeneration.Epoch != 612 || usage.UsageGeneration.Counter != h.Generation {
		t.Fatalf("receipt generation %+v does not match the hint %+v", usage.UsageGeneration, h)
	}
	if s := codexSessionMetric(t, usage.Metrics); s.Consumed == nil {
		t.Fatalf("receipt metrics are not numeric: %+v", usage.Metrics)
	}
}

// A rotation completed by an unrelated merge before the retry fires cancels
// the retry and still schedules recovery.
func TestCLIUsageHint_ARotationCompletedByAMergeCancelsTheRetry(t *testing.T) {
	withCodexGenerationEpoch(t, 513)
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-time.Hour))
	codexLiveReadAt(t, f, 33, 43, now)
	rec, cfg := propagatorFixture(t)
	cliUsageRotationFirstRetry = time.Hour // only the merge can complete it

	prevCommit := codexCommitRateLimitSnapshot
	var once sync.Once
	codexCommitRateLimitSnapshot = func(path string, out []byte, now time.Time) bool {
		refuse := false
		once.Do(func() { refuse = true })
		if refuse {
			return false
		}
		return prevCommit(path, out, now)
	}
	t.Cleanup(func() { codexCommitRateLimitSnapshot = prevCommit })

	simulateCodexProcessRestart(t, 614)
	startCLIUsagePropagator(cfg)
	time.Sleep(20 * time.Millisecond)
	codexLiveReadAt(t, f, 34, 44, now.Add(time.Second)) // unrelated merge rotates

	h := waitHints(t, rec, 1, 0)[0].hint
	if h.GenerationEpoch != 614 {
		t.Fatalf("hint = %+v", h)
	}
	cliUsagePropagator.mu.Lock()
	retryAt := cliUsagePropagator.rotationRetryAt
	cliUsagePropagator.mu.Unlock()
	if !retryAt.IsZero() {
		t.Fatal("the rotation retry is still booked after the merge completed it")
	}
}

// Hint bodies are integers and fixed strings only.
func TestCLIUsageHint_BodyCarriesNoMetricValues(t *testing.T) {
	body, err := json.Marshal(cliUsageObservedHint{Timestamp: 1, Signature: "ab", Provider: "codex", GenerationEpoch: 2, Generation: 3})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	_ = json.Unmarshal(body, &fields)
	want := map[string]bool{"timestamp": true, "signature": true, "provider": true, "generationEpoch": true, "generation": true}
	for k := range fields {
		if !want[k] {
			t.Fatalf("unexpected hint field %q", k)
		}
	}
}

// An authoritative clear — the out-of-quota shape, null or empty windows —
// retires the cached contributors instead of moving one forward, so the merge
// reports no advance. It still changes what the card must show, so it owes the
// backend a hint; otherwise the old numeric utilization stays published until
// an unrelated refresh happens to run.
func TestCLIUsageHint_AnAuthoritativeClearIsHinted(t *testing.T) {
	withCodexGenerationEpoch(t, 513)
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-time.Hour))
	rec, cfg := propagatorFixture(t)
	codexLiveReadAt(t, f, 31, 41, now) // rotates the epoch and hints the reading
	startCLIUsagePropagator(cfg)
	before := len(waitHints(t, rec, 1, 0))

	if !captureCodexRateLimitLineForAccount(`{"jsonrpc":"2.0","id":2,"result":{"rateLimits":{}}}`, now.Add(time.Second), f.fp) {
		t.Fatal("the authoritative clear did not land")
	}
	hints := waitHints(t, rec, before+1, 0)
	cleared, snap := hints[len(hints)-1].hint, f.snapshot(t)
	if len(codexContributorsFromSnapshot(snap)) != 0 {
		t.Fatalf("the clear left contributors behind: %+v", snap.Contributors)
	}
	if cleared.GenerationEpoch != snap.GenerationEpoch || cleared.Generation != snap.Generation {
		t.Fatalf("hint = %+v, want the cleared generation {%d,%d}", cleared, snap.GenerationEpoch, snap.Generation)
	}
}
