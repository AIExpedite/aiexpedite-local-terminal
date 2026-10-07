package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	// Claude cache commits in earlier tests record generations on the
	// process-global propagator, which never started there: forget them. Codex
	// state a test set up before calling this (a rotation) is kept.
	p := cliUsagePropagator
	p.mu.Lock()
	delete(p.pending, claudeUsageProvider)
	delete(p.seen, claudeUsageProvider)
	p.mu.Unlock()
	prev := []time.Duration{cliUsageHintDebounce, cliUsageHintSpacing, cliUsageRotationFirstRetry, cliUsageRotationRetryPeriod, cliUsageClaudeFallbackPeriod}
	prevFallback, prevDetected := cliUsageClaudeFallbackEnabled, claudeUsageFallbackDetected
	// The Claude fallback read is opt-in per test (claudeFallbackFixture).
	cliUsageClaudeFallbackEnabled = false
	claudeUsageFallbackDetected = func() bool { return false }
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
	// The startup recovery reads the Claude cache: never the machine's own.
	if os.Getenv("AIEXPEDITE_CLAUDE_RL_CACHE") == "" {
		t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", t.TempDir()+"/claude_rate_limits.json")
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
		cliUsageHintDebounce, cliUsageHintSpacing, cliUsageRotationFirstRetry, cliUsageRotationRetryPeriod, cliUsageClaudeFallbackPeriod = prev[0], prev[1], prev[2], prev[3], prev[4]
		cliUsageClaudeFallbackEnabled, claudeUsageFallbackDetected = prevFallback, prevDetected
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

/* ------------------------------- Claude Code ------------------------------- */

// The Go provider id is the one terminal-service's CLI_AGENT_IDS.CLAUDE_CODE
// names, pinned by the shared vector both repos sign.
func TestCLIUsageHint_ClaudeProviderIDMatchesTheSharedVector(t *testing.T) {
	if claudeUsageProvider != "claudeCode" || (claudeCodeUsageParser{}).Provider() != claudeUsageProvider {
		t.Fatalf("claude provider id = %q", claudeUsageProvider)
	}
	data, err := os.ReadFile("testdata/cli_usage_observed_hint_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Vectors []struct{ Provider string } `json:"vectors"`
	}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, v := range vectors.Vectors {
		found[v.Provider] = true
	}
	if !found[claudeUsageProvider] || !found[codexUsageProvider] {
		t.Fatalf("shared vectors cover %v, want both %q and %q", found, codexUsageProvider, claudeUsageProvider)
	}
}

// requireSpaced fails when two consecutive hints went out closer than the
// device-wide spacing (minus scheduler slack).
func requireSpaced(t *testing.T, hints []recordedCLIUsageHint) {
	t.Helper()
	for i := 1; i < len(hints); i++ {
		gap := time.UnixMilli(hints[i].hint.Timestamp).Sub(time.UnixMilli(hints[i-1].hint.Timestamp))
		if gap < cliUsageHintSpacing-5*time.Millisecond {
			t.Fatalf("hints %d and %d went out %s apart, want >= %s", i-1, i, gap, cliUsageHintSpacing)
		}
	}
}

// Claude and Codex bursts each keep their newest generation, share ONE spacing,
// and each get exactly their hint and one follow-up.
func TestCLIUsageHint_ClaudeAndCodexBurstsShareTheSpacing(t *testing.T) {
	withCodexGenerationEpoch(t, 601)
	rec, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)
	markRotated(t)

	for c := int64(1); c <= 3; c++ {
		noteCLIUsageObservationAdvanced(codexUsageProvider, gen(601, c))
		noteCLIUsageObservationAdvanced(claudeUsageProvider, gen(77, c))
	}
	hints := waitHints(t, rec, 4, 3*cliUsageHintSpacing)
	if len(hints) != 4 {
		t.Fatalf("got %d hints, want two per provider: %+v", len(hints), hints)
	}
	per := map[string]int{}
	for _, h := range hints {
		per[h.hint.Provider]++
		if h.hint.Generation != 3 {
			t.Fatalf("hint = %+v, want each provider's newest generation", h.hint)
		}
	}
	if per[codexUsageProvider] != 2 || per[claudeUsageProvider] != 2 {
		t.Fatalf("per provider = %v", per)
	}
	requireSpaced(t, hints)
}

// A provider whose generations keep advancing resets only its OWN debounce:
// a pending Claude generation still goes at the next boundary.
func TestCLIUsageHint_ContinuousCodexActivityCannotStarveClaude(t *testing.T) {
	withCodexGenerationEpoch(t, 602)
	rec, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)
	markRotated(t)

	noteCLIUsageObservationAdvanced(claudeUsageProvider, gen(88, 1))
	stop := time.Now().Add(2 * cliUsageHintSpacing)
	for c := int64(1); time.Now().Before(stop); c++ {
		noteCLIUsageObservationAdvanced(codexUsageProvider, gen(602, c))
		time.Sleep(cliUsageHintDebounce / 4)
	}
	for _, h := range rec.all() {
		if h.hint.Provider == claudeUsageProvider {
			return
		}
	}
	t.Fatalf("no Claude hint while Codex kept advancing: %+v", rec.all())
}

// The newer generation of one provider resets only that provider's two-hint
// budget: the other's follow-up still goes, and is still its last.
func TestCLIUsageHint_ANewerGenerationResetsOnlyItsOwnBudget(t *testing.T) {
	withCodexGenerationEpoch(t, 603)
	rec, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)
	markRotated(t)

	noteCLIUsageObservationAdvanced(claudeUsageProvider, gen(91, 1))
	waitHints(t, rec, 1, 0)
	noteCLIUsageObservationAdvanced(codexUsageProvider, gen(603, 1))
	noteCLIUsageObservationAdvanced(claudeUsageProvider, gen(91, 2)) // replaces Claude's follow-up
	hints := waitHints(t, rec, 5, 3*cliUsageHintSpacing)
	var claude []int64
	codex := 0
	for _, h := range hints {
		switch h.hint.Provider {
		case claudeUsageProvider:
			claude = append(claude, h.hint.Generation)
		case codexUsageProvider:
			codex++
		}
	}
	if len(hints) != 5 || codex != 2 || len(claude) != 3 || claude[0] != 1 || claude[1] != 2 || claude[2] != 2 {
		t.Fatalf("hints = %+v; want Claude 1, 2, 2 (a fresh budget) and Codex twice", hints)
	}
	requireSpaced(t, hints)
}

// A generation discovered by an ordinary (non-refresh) read is hinted at once;
// reading the same commit again stays quiet.
func TestCLIUsageHint_AClaudeCacheReadNotesANewGenerationOnce(t *testing.T) {
	rec, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)

	g := gen(93, 4)
	for i := 0; i < 5; i++ {
		observeCLIUsageGeneration(context.Background(), claudeUsageProvider, &g)
	}
	hints := waitHints(t, rec, 2, 2*cliUsageHintSpacing)
	for i := 0; i < 5; i++ {
		observeCLIUsageGeneration(context.Background(), claudeUsageProvider, &g)
	}
	time.Sleep(2 * cliUsageHintSpacing)
	if n := len(rec.all()); len(hints) != 2 || n != 2 {
		t.Fatalf("hints = %d then %d, want the hint and its follow-up only", len(hints), n)
	}
	if h := hints[0].hint; h.Provider != claudeUsageProvider || h.GenerationEpoch != 93 || h.Generation != 4 {
		t.Fatalf("hint = %+v", h)
	}
}

// A generation a signed refresh discovered rides in that receipt: no immediate
// or debounced hint, then exactly ONE confirmation no sooner than the spacing
// after the publish — which the backend, having applied the receipt, answers
// without dispatching another refresh.
func TestCLIUsageHint_AReceiptCarriedGenerationLeavesOneConfirmation(t *testing.T) {
	rec, cfg := propagatorFixture(t)
	var appliedMu sync.Mutex
	applied := map[string]cliUsageGeneration{}
	dispatched := 0
	rec.status = func(h cliUsageObservedHint) int {
		appliedMu.Lock()
		defer appliedMu.Unlock()
		if a, ok := applied[h.Provider]; !ok || !a.covers(gen(h.GenerationEpoch, h.Generation)) {
			dispatched++
		}
		return 202
	}
	startCLIUsagePropagator(cfg)

	g := gen(94, 2)
	ctx, reservation := withCLIUsageReceiptReservation(context.Background())
	observeCLIUsageGeneration(ctx, claudeUsageProvider, &g)
	time.Sleep(3 * cliUsageHintDebounce)
	if n := len(rec.all()); n != 0 {
		t.Fatalf("a receipt-reserved generation was hinted %d times before publication", n)
	}
	publishedAt := time.Now()
	appliedMu.Lock()
	applied[claudeUsageProvider] = g // the backend applies the receipt
	appliedMu.Unlock()
	settleCLIUsageReceipt(reservation, true, []cliAgentUsage{{Provider: claudeUsageProvider, UsageGeneration: &g}})

	hints := waitHints(t, rec, 1, 3*cliUsageHintSpacing)
	if len(hints) != 1 {
		t.Fatalf("got %d hints, want exactly one confirmation: %+v", len(hints), hints)
	}
	if sentAt := time.UnixMilli(hints[0].hint.Timestamp); sentAt.Before(publishedAt.Add(cliUsageHintSpacing - 5*time.Millisecond)) {
		t.Fatalf("confirmation went %s after publication, want >= %s", sentAt.Sub(publishedAt), cliUsageHintSpacing)
	}
	appliedMu.Lock()
	defer appliedMu.Unlock()
	if dispatched != 0 {
		t.Fatalf("the confirmation of an applied receipt dispatched %d refreshes", dispatched)
	}
}

// Signing or publishing the receipt failed: the reservation is released into
// the ordinary lifecycle — its hint and one follow-up, never lost.
func TestCLIUsageHint_AFailedReceiptReleasesTheReservation(t *testing.T) {
	rec, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)

	g := gen(95, 1)
	ctx, reservation := withCLIUsageReceiptReservation(context.Background())
	observeCLIUsageGeneration(ctx, claudeUsageProvider, &g)
	settleCLIUsageReceipt(reservation, false, nil)
	hints := waitHints(t, rec, 2, 2*cliUsageHintSpacing)
	if len(hints) != 2 || hints[0].hint.Generation != 1 || hints[0].hint.Provider != claudeUsageProvider {
		t.Fatalf("hints = %+v, want the released generation's hint and follow-up", hints)
	}
}

// A refresh that carried an already-propagated generation leaves nothing
// behind: confirmations are only for generations that were undelivered.
func TestCLIUsageHint_AReceiptOfADeliveredGenerationAddsNoHint(t *testing.T) {
	rec, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)
	g := gen(96, 1)
	observeCLIUsageGeneration(context.Background(), claudeUsageProvider, &g)
	waitHints(t, rec, 2, 2*cliUsageHintSpacing)

	ctx, reservation := withCLIUsageReceiptReservation(context.Background())
	observeCLIUsageGeneration(ctx, claudeUsageProvider, &g)
	settleCLIUsageReceipt(reservation, true, []cliAgentUsage{{Provider: claudeUsageProvider, UsageGeneration: &g}})
	time.Sleep(2 * cliUsageHintSpacing)
	if n := len(rec.all()); n != 2 {
		t.Fatalf("got %d hints, want no confirmation for a delivered generation", n)
	}
}

// claudeFallbackFixture enables the gated fallback read on a small period and
// counts its reads.
func claudeFallbackFixture(t *testing.T, detected bool) (cache string, reads *int64) {
	t.Helper()
	cache = t.TempDir() + "/claude_rate_limits.json"
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	// No installed hook unless a test writes one: the fallback must not follow
	// the real machine's settings to a pinned cache.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cliUsageClaudeFallbackEnabled = true
	cliUsageClaudeFallbackPeriod = 30 * time.Millisecond
	claudeUsageFallbackDetected = func() bool { return detected }
	var n int64
	prevLoad := claudeUsageFallbackLoad
	claudeUsageFallbackLoad = func(path string) (claudeRateLimitSnapshot, bool) {
		atomic.AddInt64(&n, 1)
		return prevLoad(path)
	}
	t.Cleanup(func() { claudeUsageFallbackLoad = prevLoad })
	return cache, &n
}

func writeVersionedClaudeCache(t *testing.T, cache string, g cliUsageGeneration, pct float64) {
	t.Helper()
	observed := time.Now().Add(-time.Minute).UnixMilli()
	body, _ := json.Marshal(claudeRateLimitSnapshot{
		UpdatedAt:       "2026-10-06T09:00:04Z",
		Buckets:         map[string]claudeRateLimitBucket{claudeWindowFiveHour: {UsedPercentage: pct, ResetsAtMs: observed + 3600000, ObservedAtMs: observed}},
		GenerationEpoch: g.Epoch, Generation: g.Counter,
	})
	if err := os.WriteFile(cache, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The status-line hook commits from another process: the fallback read finds
// it, even when the replacement has the same size and timestamp as the file it
// replaced.
func TestCLIUsageHint_FallbackDiscoversAnExternalClaudeCommit(t *testing.T) {
	rec, cfg := propagatorFixture(t)
	cache, _ := claudeFallbackFixture(t, true)
	writeVersionedClaudeCache(t, cache, gen(97, 1), 41)
	info, _ := os.Stat(cache)

	startCLIUsagePropagator(cfg) // the startup recovery hints {97,1}
	first := waitHints(t, rec, 1, 0)[0].hint
	if first.Provider != claudeUsageProvider || first.Generation != 1 {
		t.Fatalf("recovery hint = %+v", first)
	}

	writeVersionedClaudeCache(t, cache, gen(97, 2), 42) // same size
	if err := os.Chtimes(cache, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if now, _ := os.Stat(cache); now.Size() != info.Size() || !now.ModTime().Equal(info.ModTime()) {
		t.Fatal("fixture did not reproduce a same-size, same-stamp replacement")
	}
	deadline := time.Now().Add(5 * cliUsageHintSpacing)
	for time.Now().Before(deadline) {
		for _, h := range rec.all() {
			if h.hint.Generation == 2 {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the fallback never discovered {97,2}: %+v", rec.all())
}

// On a dual-channel machine the hook commits to the OTHER channel's cache: the
// fallback imports that newer reading and hints the own generation it advanced.
func TestCLIUsageHint_FallbackHintsAReadingCommittedToThePinnedCache(t *testing.T) {
	rec, cfg := propagatorFixture(t)
	cache, _ := claudeFallbackFixture(t, true)
	pinned := filepath.Join(t.TempDir(), "pinned", "rl.json")
	helperWriteJSON(t, filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"), map[string]any{
		"statusLine": map[string]any{
			"type": "command",
			"command": "AIEXPEDITE_CLAUDE_RL_CACHE=" + posixSingleQuote(pinned) +
				" '/opt/aiexpedite/aiexpedite-terminal' " + statusLineHookArg,
		},
	})
	writeVersionedClaudeCache(t, cache, gen(96, 1), 41)

	startCLIUsagePropagator(cfg) // the startup recovery hints {96,1}
	if first := waitHints(t, rec, 1, 0)[0].hint; first.Generation != 1 {
		t.Fatalf("recovery hint = %+v", first)
	}

	now := time.Now()
	mergeClaudeRateLimitCacheFromSource(pinned, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {UsedPercentage: 77, ResetsAtMs: now.Add(time.Hour).UnixMilli(), ObservedAtMs: now.UnixMilli(), usageKnown: true},
	}, now, "", claudeRateLimitSourceStatusLine)
	deadline := time.Now().Add(5 * cliUsageHintSpacing)
	for time.Now().Before(deadline) {
		for _, h := range rec.all() {
			if h.hint.GenerationEpoch == 96 && h.hint.Generation == 2 {
				own, _ := loadClaudeRateLimitSnapshot(cache)
				if b := own.Buckets[claudeWindowFiveHour]; b.UsedPercentage != 77 || b.Source != claudeRateLimitSourceStatusLine {
					t.Fatalf("own cache five_hour = %+v, want the imported status-line reading", b)
				}
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the fallback never hinted the imported pinned reading: %+v", rec.all())
}

// A fresh dual-channel install: the hook is pinned to the other channel and
// this agent has no own cache at all. The fallback must still import the
// pinned commit — creating the own cache — and hint the generation it wrote,
// or this device's backend card stays stale until an unrelated refresh.
func TestCLIUsageHint_FallbackHintsAPinnedReadingWithNoOwnCache(t *testing.T) {
	rec, cfg := propagatorFixture(t)
	cache, _ := claudeFallbackFixture(t, true)
	pinned := filepath.Join(t.TempDir(), "pinned", "rl.json")
	helperWriteJSON(t, filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"), map[string]any{
		"statusLine": map[string]any{
			"type": "command",
			"command": "AIEXPEDITE_CLAUDE_RL_CACHE=" + posixSingleQuote(pinned) +
				" '/opt/aiexpedite/aiexpedite-terminal' " + statusLineHookArg,
		},
	})
	if _, err := os.Stat(cache); err == nil {
		t.Fatal("fixture wrote an own cache; this case is about its absence")
	}

	startCLIUsagePropagator(cfg) // nothing to recover: there is no own cache
	// Written as the short-lived hook writes it — another process, so nothing
	// notifies this propagator in-process and only the fallback can find it.
	if err := os.MkdirAll(filepath.Dir(pinned), 0o755); err != nil {
		t.Fatal(err)
	}
	writeVersionedClaudeCache(t, pinned, gen(94, 6), 68)

	hint := waitHints(t, rec, 1, 0)[0].hint
	if hint.Provider != claudeUsageProvider || hint.GenerationEpoch == 0 || hint.Generation == 0 {
		t.Fatalf("hint = %+v, want the generation the bootstrap import committed", hint)
	}
	if hint.GenerationEpoch == 94 {
		t.Fatalf("hint = %+v, want this agent's own generation, not the pinned cache's", hint)
	}
	own, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok || own.Buckets[claudeWindowFiveHour].UsedPercentage != 68 {
		t.Fatalf("own snapshot = %+v (ok=%v), want the imported pinned reading", own, ok)
	}
	if own.GenerationEpoch != hint.GenerationEpoch || own.Generation != hint.Generation {
		t.Fatalf("own generation = {%d,%d}, want the hinted {%d,%d}",
			own.GenerationEpoch, own.Generation, hint.GenerationEpoch, hint.Generation)
	}
}

// The fallback reads nothing when Claude is not detected, when no cache
// exists, or while the agent is offline, draining, shutting down or stopped.
func TestCLIUsageHint_FallbackReadsOnlyWhileEveryGateIsOpen(t *testing.T) {
	gates := []struct {
		name     string
		detected bool
		noCache  bool
		block    func(t *testing.T)
		stop     bool
	}{
		{name: "not detected", detected: false},
		{name: "no cache", detected: true, noCache: true},
		{name: "offline", detected: true, block: func(t *testing.T) { setCodexTestOffline(t, true) }},
		{name: "draining", detected: true, block: func(t *testing.T) {
			drain.mu.Lock()
			drain.draining = true
			drain.mu.Unlock()
			t.Cleanup(func() { drain.mu.Lock(); drain.draining = false; drain.mu.Unlock() })
		}},
		{name: "shutting down", detected: true, block: func(t *testing.T) {
			shutdownInProgress.Store(true)
			t.Cleanup(func() { shutdownInProgress.Store(false) })
		}},
		{name: "stopped", detected: true, stop: true},
	}
	for _, g := range gates {
		t.Run(g.name, func(t *testing.T) {
			rec, cfg := propagatorFixture(t)
			cache, reads := claudeFallbackFixture(t, g.detected)
			if !g.noCache {
				writeVersionedClaudeCache(t, cache, gen(98, 1), 41)
			}
			if g.block != nil {
				g.block(t)
			}
			startCLIUsagePropagator(cfg)
			if g.stop {
				stopCLIUsagePropagator()
			}
			time.Sleep(10 * cliUsageClaudeFallbackPeriod)
			if n := atomic.LoadInt64(reads); n != 0 {
				t.Fatalf("the fallback read the cache %d times", n)
			}
			if g.stop && len(rec.all()) != 0 {
				t.Fatalf("a stopped propagator sent %+v", rec.all())
			}
		})
	}
}

// A receipt that signed an older generation than the one reserved for it (or
// none for that provider) did not deliver the reservation: it is released.
func TestCLIUsageHint_AReceiptThatDidNotCarryTheReservationReleasesIt(t *testing.T) {
	rec, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)

	reserved, older := gen(99, 3), gen(99, 2)
	ctx, reservation := withCLIUsageReceiptReservation(context.Background())
	observeCLIUsageGeneration(ctx, claudeUsageProvider, &reserved)
	settleCLIUsageReceipt(reservation, true, []cliAgentUsage{{Provider: claudeUsageProvider, UsageGeneration: &older}})
	hints := waitHints(t, rec, 2, 2*cliUsageHintSpacing)
	if len(hints) != 2 || hints[0].hint.Generation != 3 {
		t.Fatalf("hints = %+v, want the reserved {99,3} hinted and followed up", hints)
	}
}

// A generation noted for the provider while its reservation was out — the same
// epoch's next counter, or a new epoch after an account switch — supersedes the
// reservation however the receipt settles: it alone is hinted, with its own
// hint and follow-up.
func TestCLIUsageHint_ANewerPendingGenerationSurvivesTheReceiptSettlement(t *testing.T) {
	reserved := gen(100, 3)
	older := gen(100, 2)
	cases := []struct {
		name      string
		newer     cliUsageGeneration
		published bool
		carried   *cliUsageGeneration
	}{
		{"receipt carried the reservation", gen(100, 4), true, &reserved},
		{"receipt carried an older generation", gen(100, 4), true, &older},
		{"receipt not published", gen(100, 4), false, nil},
		{"new epoch, receipt carried the reservation", gen(200, 1), true, &reserved},
		{"new epoch, receipt carried an older generation", gen(200, 1), true, &older},
		{"new epoch, receipt not published", gen(200, 1), false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, cfg := propagatorFixture(t)
			startCLIUsagePropagator(cfg)

			ctx, reservation := withCLIUsageReceiptReservation(context.Background())
			r := reserved
			observeCLIUsageGeneration(ctx, claudeUsageProvider, &r)
			noteCLIUsageObservationAdvanced(claudeUsageProvider, tc.newer)
			var agents []cliAgentUsage
			if tc.carried != nil {
				agents = []cliAgentUsage{{Provider: claudeUsageProvider, UsageGeneration: tc.carried}}
			}
			settleCLIUsageReceipt(reservation, tc.published, agents)

			hints := waitHints(t, rec, 2, 2*cliUsageHintSpacing)
			if len(hints) != 2 {
				t.Fatalf("got %d hints, want the newer generation's hint and follow-up: %+v", len(hints), hints)
			}
			for _, h := range hints {
				if gen(h.hint.GenerationEpoch, h.hint.Generation) != tc.newer {
					t.Fatalf("hint %+v, want only the newer %+v", h.hint, tc.newer)
				}
			}
		})
	}
}

// A note recorded while a hint's send is in flight re-arms the timer; the hint
// being sent must not be picked again (and sent a second time, unspaced)
// before that send settles.
func TestCLIUsageHint_ANoteDuringASendDoesNotResendTheHintInFlight(t *testing.T) {
	withCodexGenerationEpoch(t, 603)
	rec, cfg := propagatorFixture(t)
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	var mu sync.Mutex
	claudeStarts := 0
	started := make(chan struct{}, 8)
	record := sendCLIUsageObservedHint
	sendCLIUsageObservedHint = func(ctx context.Context, url string, hint cliUsageObservedHint) int {
		if hint.Provider == claudeUsageProvider && hint.Generation == 1 {
			mu.Lock()
			claudeStarts++
			mu.Unlock()
			started <- struct{}{}
			<-release
		}
		return record(ctx, url, hint)
	}
	startCLIUsagePropagator(cfg)
	markRotated(t)

	noteCLIUsageObservationAdvanced(claudeUsageProvider, gen(78, 1))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the Claude hint was never sent")
	}
	// Another provider's note re-arms the timer while the Claude send is held.
	noteCLIUsageObservationAdvanced(codexUsageProvider, gen(603, 1))
	time.Sleep(3 * cliUsageHintDebounce)
	mu.Lock()
	starts := claudeStarts
	mu.Unlock()
	if starts != 1 {
		t.Fatalf("the in-flight Claude hint was started %d times, want once", starts)
	}
	close(release)
	if hints := waitHints(t, rec, 1, cliUsageHintDebounce); len(hints) == 0 {
		t.Fatal("no hint was recorded after the send settled")
	}
}
