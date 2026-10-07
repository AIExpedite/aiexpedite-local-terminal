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
	// The startup rotation writes each provider's capture state: never the
	// machine's own.
	t.Setenv(openCodeUsageLedgerEnv, t.TempDir()+"/opencode_usage.json")
	t.Setenv(openCodeUsageFreshnessEnv, t.TempDir()+"/opencode_usage_freshness.json")
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
		// resetCLIUsagePropagator waits every timer callback out, which is what
		// makes the two restores below safe: a callback releases p.mu around
		// its send and then re-reads the schedule, so it could otherwise be
		// reading the send seam or a pinned duration as this line replaces it.
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
	noteCLIUsageGenerationRotated(codexUsageProvider)
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
	retryAt := time.Time{}
	if state := cliUsagePropagator.providers[codexUsageProvider]; state != nil {
		retryAt = state.rotationRetryAt
	}
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

// A clear committed by the previous process whose hint died in the debounce (an
// update handoff) is recovered like a numeric reading: the replacement process
// rotates the cleared snapshot onto its epoch and hints it, so the backend stops
// publishing the old utilization.
func TestCLIUsageHint_StartupRecoveryHintsACommittedClear(t *testing.T) {
	withCodexGenerationEpoch(t, 515)
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-time.Hour))
	codexLiveReadAt(t, f, 31, 41, now)
	if !captureCodexRateLimitLineForAccount(`{"jsonrpc":"2.0","id":2,"result":{"rateLimits":{}}}`, now.Add(time.Second), f.fp) {
		t.Fatal("the authoritative clear did not land")
	}
	if len(codexContributorsFromSnapshot(f.snapshot(t))) != 0 {
		t.Fatal("the clear left contributors behind")
	}

	// The previous process exits inside the debounce: its hint is never sent.
	rec, cfg := propagatorFixture(t)
	simulateCodexProcessRestart(t, 616)
	startCLIUsagePropagator(cfg)
	h := waitHints(t, rec, 1, 0)[0].hint
	snap := f.snapshot(t)
	if h.GenerationEpoch != 616 || snap.GenerationEpoch != 616 || h.Generation != snap.Generation {
		t.Fatalf("recovery hint = %+v, want the rotated cleared generation {%d,%d}", h, snap.GenerationEpoch, snap.Generation)
	}
}

/* --------------------------------------------------------------------------
   Per-provider pending slots (OpenCode joins Codex)
   -------------------------------------------------------------------------- */

// markOpenCodeRotated stands in for a committed OpenCode ledger write carrying
// this process's epoch.
func markOpenCodeRotated(t *testing.T) {
	t.Helper()
	openCodeGenerationRotated.Store(true)
	noteCLIUsageGenerationRotated(openCodeUsageProvider)
}

// An OpenCode observation must not evict a Codex one that has not been sent
// yet: with one shared pending slot, whichever provider committed last won and
// the other's reading waited for the six-hourly gather.
func TestCLIUsageHint_EachProviderKeepsItsOwnPendingObservation(t *testing.T) {
	withCodexGenerationEpoch(t, 601)
	rec, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)
	markRotated(t)
	markOpenCodeRotated(t)

	noteCLIUsageObservationAdvanced(codexUsageProvider, gen(601, 3))
	noteCLIUsageObservationAdvanced(openCodeUsageProvider, gen(777, 2))

	// Four hints in total: one plus one follow-up PER PROVIDER, spaced
	// globally.
	hints := waitHints(t, rec, 4, 2*cliUsageHintSpacing)
	byProvider := map[string][]int64{}
	for _, h := range hints {
		byProvider[h.hint.Provider] = append(byProvider[h.hint.Provider], h.hint.Generation)
	}
	if len(byProvider["codex"]) != 2 || len(byProvider["opencode"]) != 2 {
		t.Fatalf("hints per provider = %v, want two each (a hint and its follow-up)", byProvider)
	}
	for _, h := range hints {
		switch h.hint.Provider {
		case "codex":
			if h.hint.GenerationEpoch != 601 || h.hint.Generation != 3 {
				t.Fatalf("codex hint = %+v, want {601,3}", h.hint)
			}
		case "opencode":
			if h.hint.GenerationEpoch != 777 || h.hint.Generation != 2 {
				t.Fatalf("opencode hint = %+v, want {777,2}", h.hint)
			}
		default:
			t.Fatalf("unexpected provider %q", h.hint.Provider)
		}
		// The signed message covers the provider, so the two cannot be
		// confused in transit.
		msg := buildCLIUsageObservedSignedMessage("agent-1", h.hint.Timestamp, h.hint.Provider,
			gen(h.hint.GenerationEpoch, h.hint.Generation))
		if h.hint.Signature != generateHMAC(msg, "secret") {
			t.Fatalf("hint signature does not cover the provider: %+v", h.hint)
		}
	}
	// The 5-minute spacing stays GLOBAL: at most one hint leaves the device in
	// that window, whoever it is for.
	for i := 1; i < len(hints); i++ {
		gap := time.UnixMilli(hints[i].hint.Timestamp).Sub(time.UnixMilli(hints[i-1].hint.Timestamp))
		if gap < cliUsageHintSpacing-10*time.Millisecond {
			t.Fatalf("hints %d and %d are %s apart, want >= %s", i-1, i, gap, cliUsageHintSpacing)
		}
	}
}

// OpenCode waits for its OWN rotation: Codex's does not license an OpenCode
// hint, because the backend compares each provider's generation separately.
func TestCLIUsageHint_OpenCodeWaitsForItsOwnRotation(t *testing.T) {
	withCodexGenerationEpoch(t, 602)
	rec, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)
	markRotated(t) // Codex only

	noteCLIUsageObservationAdvanced(openCodeUsageProvider, gen(778, 1))
	time.Sleep(3 * cliUsageHintDebounce)
	if hints := rec.all(); len(hints) != 0 {
		t.Fatalf("hints = %+v, want none before OpenCode's own rotation", hints)
	}

	markOpenCodeRotated(t)
	hints := waitHints(t, rec, 1, 0)
	if hints[0].hint.Provider != "opencode" || hints[0].hint.GenerationEpoch != 778 {
		t.Fatalf("hint = %+v, want the OpenCode observation", hints[0].hint)
	}
}

// A provider the backend does not track gets no slot at all: recording one
// would spend the global spacing on a hint the route answers with
// `accepted: false`.
func TestCLIUsageHint_IgnoresAProviderWithNoGenerationSource(t *testing.T) {
	withCodexGenerationEpoch(t, 603)
	rec, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)
	markRotated(t)

	noteCLIUsageObservationAdvanced("grok", gen(999, 1))
	time.Sleep(3 * cliUsageHintDebounce)
	if hints := rec.all(); len(hints) != 0 {
		t.Fatalf("hints = %+v, want none for an untracked provider", hints)
	}
}

// The startup recovery hints an OpenCode ledger the PREVIOUS process committed
// but never announced (a shutdown inside the debounce).
func TestCLIUsageHint_StartupRecoveryHintsANumericOpenCodeLedger(t *testing.T) {
	withCodexGenerationEpoch(t, 604)
	rec, cfg := propagatorFixture(t)

	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	prevNow := openCodeUsageNow
	openCodeUsageNow = func() time.Time { return now }
	t.Cleanup(func() { openCodeUsageNow = prevNow })
	openCodeGenerationRotated.Store(false)
	t.Cleanup(func() { openCodeGenerationRotated.Store(false) })

	// A reading on disk under ANOTHER process's epoch.
	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		openCodeEnsureDay(l, openCodeDayKey(now)).Messages["0123456789abcdef"] =
			openCodeMessageUsage{In: 11, Out: 4, ObservedAtMs: now.UnixMilli()}
		l.Generation = cliUsageGeneration{Epoch: 4242, Counter: 9}
		return openCodeLedgerEdit{Changed: true}
	})

	startCLIUsagePropagator(cfg)
	hints := waitHints(t, rec, 1, 0)
	found := false
	for _, h := range hints {
		if h.hint.Provider != "opencode" {
			continue
		}
		found = true
		if h.hint.GenerationEpoch == 4242 {
			t.Fatalf("the recovery hint still names the previous process's epoch: %+v", h.hint)
		}
		if h.hint.GenerationEpoch != openCodeProcessGenerationEpoch.Load() {
			t.Fatalf("hint epoch = %d, want this process's %d",
				h.hint.GenerationEpoch, openCodeProcessGenerationEpoch.Load())
		}
	}
	if !found {
		t.Fatalf("hints = %+v, want an OpenCode recovery hint", hints)
	}
}

// The epoch draw is shared with Codex, so neither can drift off the route's
// Number.MAX_SAFE_INTEGER bound.
func TestCLIUsageHint_OpenCodeEpochStaysJavaScriptSafe(t *testing.T) {
	const maxSafe = int64(1)<<53 - 1
	for i := 0; i < 500; i++ {
		if epoch := openCodeDrawGenerationEpoch(); epoch < 1 || epoch > maxSafe {
			t.Fatalf("epoch %d outside [1, 2^53-1]", epoch)
		}
	}
}

// A provider held back by its OWN gate must not starve the other. The two share
// one timer and one spacing window, and sendDue picks a single provider per
// pass — so a Codex observation waiting on its epoch rotation could have parked
// the whole schedule while OpenCode's reading sat ready.
func TestCLIUsageHint_ABlockedProviderDoesNotStarveTheOther(t *testing.T) {
	withCodexGenerationEpoch(t, 701)
	rec, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)
	// OpenCode rotated; Codex deliberately NOT — its hint is gated on
	// `awaiting_rotation`.
	markOpenCodeRotated(t)

	noteCLIUsageObservationAdvanced(codexUsageProvider, gen(701, 4))
	noteCLIUsageObservationAdvanced(openCodeUsageProvider, gen(888, 1))

	hints := waitHints(t, rec, 1, 2*cliUsageHintDebounce)
	for _, h := range hints {
		if h.hint.Provider != "opencode" {
			t.Fatalf("hint = %+v, want only the rotated provider's", h.hint)
		}
	}
	if len(hints) == 0 {
		t.Fatal("the rotated provider's reading was starved by the blocked one")
	}

	// And once Codex rotates, its own held observation goes out — it was
	// deferred, not dropped.
	markRotated(t)
	all := waitHints(t, rec, len(hints)+1, 2*cliUsageHintSpacing)
	sawCodex := false
	for _, h := range all {
		if h.hint.Provider == "codex" {
			sawCodex = true
			if h.hint.GenerationEpoch != 701 || h.hint.Generation != 4 {
				t.Fatalf("codex hint = %+v, want the observation it held {701,4}", h.hint)
			}
		}
	}
	if !sawCodex {
		t.Fatal("the held Codex observation was dropped rather than deferred")
	}
}

// A provider that keeps advancing must not starve the other either. The
// invariant that makes starvation impossible is that sendDue picks the EARLIEST
// due, and a slot's due is anchored to when its observation was noted — so the
// quieter provider's older note always outranks the busy one's fresh one, no
// matter how often the busy one re-notes.
//
// Asserted on nextDueLocked rather than by racing the live scheduler: the
// debounce, the global spacing and the one timer interact, so a wall-clock race
// against them is flaky — and a flaky test here would train people to ignore a
// real starvation regression.
func TestCLIUsageHint_TheEarliestNoteIsAlwaysDueFirst(t *testing.T) {
	_, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)

	p := cliUsagePropagator
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	quiet := p.providerLocked(codexUsageProvider)
	busy := p.providerLocked(openCodeUsageProvider)
	if quiet == nil || busy == nil {
		t.Fatal("both providers must have a slot")
	}

	// The quiet provider noted once, a while ago.
	quiet.pending = &cliUsagePendingHint{generation: gen(1, 1), noteAt: now.Add(-time.Minute)}
	// The busy one has re-noted six times since, each reset moving its noteAt
	// forward — which is what could have starved the other.
	for c := int64(1); c <= 6; c++ {
		busy.pending = &cliUsagePendingHint{generation: gen(2, c), noteAt: now.Add(time.Duration(c) * time.Millisecond)}
	}

	quietDue, okQuiet := p.nextDueLocked(quiet, now)
	busyDue, okBusy := p.nextDueLocked(busy, now)
	if !okQuiet || !okBusy {
		t.Fatalf("both slots must report a due instant (%v/%v)", okQuiet, okBusy)
	}
	if !quietDue.Before(busyDue) {
		t.Fatalf("quiet due %s is not before busy due %s — a busy provider would starve the other",
			quietDue, busyDue)
	}
	// And the busy slot still carries only its NEWEST generation: the ones it
	// superseded are never queued behind it.
	if busy.pending.generation != gen(2, 6) {
		t.Fatalf("busy pending = %+v, want only the newest generation", busy.pending.generation)
	}

	// A follow-up is due immediately, which is how the two-hint budget is spent
	// at the next spacing boundary rather than after another debounce.
	busy.pending.followUp = true
	if due, _ := p.nextDueLocked(busy, now); due.After(now) {
		t.Fatalf("a follow-up due %s is later than now — it waits only on the spacing", due)
	}
}

// After a send, every pending due clamps to the same `lastSentAt + spacing`
// boundary. The tie must go to the OLDER observation: a table-order tie-break
// let Codex, re-noted before each boundary, win every one while an older
// OpenCode reading never left. Asserted on pickDueLocked for the same reason as
// TestCLIUsageHint_TheEarliestNoteIsAlwaysDueFirst.
func TestCLIUsageHint_ASpacingTieGoesToTheOlderObservation(t *testing.T) {
	_, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)

	p := cliUsagePropagator
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	p.lastSentAt = now.Add(-cliUsageHintSpacing) // the boundary is now
	codex := p.providerLocked(codexUsageProvider)
	openCode := p.providerLocked(openCodeUsageProvider)
	// Both past their debounce, so both clamp to the same boundary; OpenCode's
	// observation is the older one.
	openCode.pending = &cliUsagePendingHint{generation: gen(2, 1), noteAt: now.Add(-cliUsageHintDebounce - time.Minute)}
	codex.pending = &cliUsagePendingHint{generation: gen(1, 7), noteAt: now.Add(-cliUsageHintDebounce - time.Second)}

	codexDue, _ := p.nextDueLocked(codex, now)
	openCodeDue, _ := p.nextDueLocked(openCode, now)
	if !codexDue.Equal(openCodeDue) {
		t.Fatalf("dues %s / %s are not tied — the fixture no longer exercises the tie", codexDue, openCodeDue)
	}
	if provider, _ := p.pickDueLocked(now); provider != openCodeUsageProvider {
		t.Fatalf("tie went to %q, want the older observation (opencode)", provider)
	}

	// And the other way round: the order of the table is not what decides.
	codex.pending.noteAt = now.Add(-cliUsageHintDebounce - 2*time.Minute)
	if provider, _ := p.pickDueLocked(now); provider != codexUsageProvider {
		t.Fatalf("tie went to %q, want the older observation (codex)", provider)
	}
}
