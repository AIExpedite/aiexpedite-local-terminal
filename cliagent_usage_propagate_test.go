package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
	// outcome, when set, answers the whole outcome (status, accepted, reason)
	// and takes precedence over status.
	outcome func(cliUsageObservedHint) cliUsageHintOutcome
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
	prev := []time.Duration{cliUsageHintDebounce, cliUsageHintSpacing, cliUsageRotationFirstRetry, cliUsageRotationRetryPeriod, cliUsageWatchPeriod}
	cliUsageHintDebounce = 20 * time.Millisecond
	cliUsageHintSpacing = 150 * time.Millisecond
	cliUsageRotationFirstRetry = 40 * time.Millisecond
	cliUsageRotationRetryPeriod = 60 * time.Millisecond
	t.Setenv("TERMINAL_SERVICE_URL", "https://terminal.example.test")
	// The startup rotation writes the Codex and Claude caches: never the
	// machine's own.
	if os.Getenv("AIEXPEDITE_CODEX_RL_CACHE") == "" {
		t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", t.TempDir()+"/codex_rate_limits.json")
		t.Setenv("CODEX_HOME", t.TempDir())
	}
	if os.Getenv("AIEXPEDITE_CLAUDE_RL_CACHE") == "" {
		t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", t.TempDir()+"/claude_rate_limits.json")
	}
	if os.Getenv("CLAUDE_CONFIG_DIR") == "" {
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	}
	resetClaudeUsageWatchState()
	rec := &cliUsageHintRecorder{}
	prevSend := sendCLIUsageObservedHint
	sendCLIUsageObservedHint = func(_ context.Context, url string, hint cliUsageObservedHint) cliUsageHintOutcome {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.hints = append(rec.hints, recordedCLIUsageHint{url: url, hint: hint})
		if rec.outcome != nil {
			return rec.outcome(hint)
		}
		status := 202
		if rec.status != nil {
			status = rec.status(hint)
		}
		return cliUsageHintOutcome{status: status, accepted: status/100 == 2}
	}
	t.Cleanup(func() {
		stopCLIUsagePropagator()
		resetCLIUsagePropagator()
		sendCLIUsageObservedHint = prevSend
		cliUsageHintDebounce, cliUsageHintSpacing, cliUsageRotationFirstRetry, cliUsageRotationRetryPeriod, cliUsageWatchPeriod = prev[0], prev[1], prev[2], prev[3], prev[4]
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

/* ------------------------------ per provider ------------------------------ */

// markClaudeRotated stands in for a committed Claude stamp on this epoch.
func markClaudeRotated(t *testing.T) {
	t.Helper()
	claudeGenerationRotated.Store(true)
	noteCLIUsageGenerationRotated(claudeUsageProvider)
}

func hintsFor(hints []recordedCLIUsageHint, provider string) []cliUsageObservedHint {
	var out []cliUsageObservedHint
	for _, h := range hints {
		if h.hint.Provider == provider {
			out = append(out, h.hint)
		}
	}
	return out
}

// isolateClaudeCache points the Claude cache and config dir at temp paths.
func isolateClaudeCache(t *testing.T) string {
	t.Helper()
	cache := filepath.Join(t.TempDir(), "claude_rate_limits.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	return cache
}

// A Claude note does not clobber a pending Codex one: each keeps its own
// two-hint budget, and the device-wide spacing separates every send.
func TestCLIUsageHint_ProvidersKeepTheirOwnBudgets(t *testing.T) {
	withCodexGenerationEpoch(t, 520)
	rec, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)
	markRotated(t)
	markClaudeRotated(t)

	noteCLIUsageObservationAdvanced(codexUsageProvider, gen(520, 1))
	noteCLIUsageObservationAdvanced(claudeUsageProvider, gen(520, 4))
	hints := waitHints(t, rec, 4, 2*cliUsageHintSpacing)
	if len(hints) != 4 {
		t.Fatalf("got %d hints, want two per provider: %+v", len(hints), hints)
	}
	if len(hintsFor(hints, codexUsageProvider)) != 2 || len(hintsFor(hints, claudeUsageProvider)) != 2 {
		t.Fatalf("hints per provider are not 2+2: %+v", hints)
	}
	// The oldest observation (Codex, noted first) goes first.
	if hints[0].hint.Provider != codexUsageProvider {
		t.Fatalf("first hint = %+v, want the older Codex observation", hints[0].hint)
	}
	for i := 1; i < len(hints); i++ {
		gap := time.UnixMilli(hints[i].hint.Timestamp).Sub(time.UnixMilli(hints[i-1].hint.Timestamp))
		if gap < cliUsageHintSpacing-10*time.Millisecond {
			t.Fatalf("hints %d and %d are %s apart, want the device-wide spacing", i-1, i, gap)
		}
	}
}

// A note recorded for another provider while a hint is in flight must not let
// a second send select that same in-flight observation: both sends would skip
// the device-wide spacing, and both would be processed as an initial send,
// leaving a third follow-up for one observation.
func TestCLIUsageHint_ANoteDuringAnInFlightSendDoesNotResendIt(t *testing.T) {
	withCodexGenerationEpoch(t, 540)
	rec, cfg := propagatorFixture(t)
	noted, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	rec.outcome = func(cliUsageObservedHint) cliUsageHintOutcome {
		// The recorder holds its own lock here, so a concurrent send blocks
		// until release and is recorded after it: an extra hint either way.
		once.Do(func() {
			noteCLIUsageObservationAdvanced(claudeUsageProvider, gen(540, 7))
			close(noted)
			<-release
		})
		return cliUsageHintOutcome{status: 202, accepted: true}
	}
	startCLIUsagePropagator(cfg)
	markRotated(t)
	markClaudeRotated(t)

	noteCLIUsageObservationAdvanced(codexUsageProvider, gen(540, 1))
	<-noted
	time.Sleep(2 * cliUsageHintSpacing) // a second sendDue would fire in here
	close(release)

	hints := waitHints(t, rec, 4, 2*cliUsageHintSpacing)
	codex, claude := hintsFor(hints, codexUsageProvider), hintsFor(hints, claudeUsageProvider)
	if len(codex) != 2 || len(claude) != 2 || len(hints) != 4 {
		t.Fatalf("got %d hints (codex=%d claude=%d), want one hint and one follow-up each: %+v",
			len(hints), len(codex), len(claude), hints)
	}
}

// Every retryable refusal keeps the hint pending with its follow-up unspent,
// retried at the next spacing; three of them, of any mix, drop it.
func TestCLIUsageHint_RetryableRefusalsAreRetriedThenCapped(t *testing.T) {
	for _, reason := range []string{"observed_cooldown", "wake_cooldown", "in_flight", "shutting_down", "publish_failed", "", "a_future_code"} {
		t.Run("reason="+reason, func(t *testing.T) {
			withCodexGenerationEpoch(t, 521)
			rec, cfg := propagatorFixture(t)
			rec.outcome = func(cliUsageObservedHint) cliUsageHintOutcome {
				return cliUsageHintOutcome{status: 202, reason: reason}
			}
			startCLIUsagePropagator(cfg)
			markClaudeRotated(t)
			noteCLIUsageObservationAdvanced(claudeUsageProvider, gen(521, 1))
			hints := waitHints(t, rec, cliUsageHintMaxRetryable, 2*cliUsageHintSpacing)
			if len(hints) != cliUsageHintMaxRetryable {
				t.Fatalf("got %d hints, want %d retries then a drop", len(hints), cliUsageHintMaxRetryable)
			}
		})
	}
}

// A mix of retryable codes counts toward one cap.
func TestCLIUsageHint_AMixOfRetryableRefusalsSharesOneCap(t *testing.T) {
	withCodexGenerationEpoch(t, 529)
	rec, cfg := propagatorFixture(t)
	reasons := []string{"observed_cooldown", "in_flight", ""}
	calls := 0
	rec.outcome = func(cliUsageObservedHint) cliUsageHintOutcome {
		r := reasons[calls%len(reasons)]
		calls++
		return cliUsageHintOutcome{status: 202, reason: r}
	}
	startCLIUsagePropagator(cfg)
	markClaudeRotated(t)
	noteCLIUsageObservationAdvanced(claudeUsageProvider, gen(529, 1))
	if hints := waitHints(t, rec, 3, 2*cliUsageHintSpacing); len(hints) != 3 {
		t.Fatalf("got %d hints, want 3", len(hints))
	}
}

// A retryable refusal does not spend the follow-up: one refusal, then a
// delivered hint, then its follow-up — three sends.
func TestCLIUsageHint_ARetryableRefusalDoesNotSpendTheFollowUp(t *testing.T) {
	withCodexGenerationEpoch(t, 522)
	rec, cfg := propagatorFixture(t)
	calls := 0
	rec.outcome = func(cliUsageObservedHint) cliUsageHintOutcome {
		calls++
		if calls == 1 {
			return cliUsageHintOutcome{status: 202, reason: "observed_cooldown"}
		}
		return cliUsageHintOutcome{status: 202, accepted: true}
	}
	startCLIUsagePropagator(cfg)
	markClaudeRotated(t)
	noteCLIUsageObservationAdvanced(claudeUsageProvider, gen(522, 1))
	if hints := waitHints(t, rec, 3, 2*cliUsageHintSpacing); len(hints) != 3 {
		t.Fatalf("got %d hints, want refusal + hint + follow-up", len(hints))
	}
}

// already_applied and a rejection drop the observation at once.
func TestCLIUsageHint_TerminalRefusalsDropAtOnce(t *testing.T) {
	for _, reason := range []string{"already_applied", "provider_not_reported", "owner_changed"} {
		t.Run(reason, func(t *testing.T) {
			withCodexGenerationEpoch(t, 523)
			rec, cfg := propagatorFixture(t)
			rec.outcome = func(cliUsageObservedHint) cliUsageHintOutcome {
				return cliUsageHintOutcome{status: 202, reason: reason}
			}
			startCLIUsagePropagator(cfg)
			markClaudeRotated(t)
			noteCLIUsageObservationAdvanced(claudeUsageProvider, gen(523, 1))
			if hints := waitHints(t, rec, 1, 2*cliUsageHintSpacing); len(hints) != 1 {
				t.Fatalf("got %d hints, want exactly 1", len(hints))
			}
		})
	}
}

// A refusal's reason never reaches the log; only the fixed class label does.
func TestCLIUsageHint_TheServerReasonIsNeverLogged(t *testing.T) {
	withCodexGenerationEpoch(t, 524)
	rec, cfg := propagatorFixture(t)
	rec.outcome = func(cliUsageObservedHint) cliUsageHintOutcome {
		return cliUsageHintOutcome{status: 202, reason: "secret-server-detail"}
	}
	logged := captureStdout(t, func() {
		startCLIUsagePropagator(cfg)
		markClaudeRotated(t)
		noteCLIUsageObservationAdvanced(claudeUsageProvider, gen(524, 1))
		waitHints(t, rec, 1, cliUsageHintSpacing/2)
		stopCLIUsagePropagator()
	})
	if strings.Contains(logged, "secret-server-detail") || !strings.Contains(logged, "refused_retryable provider=claudeCode") {
		t.Fatalf("log = %q", logged)
	}
}

// Startup recovery for Claude: a numeric reading the previous process captured
// is rotated onto this epoch and hinted.
func TestCLIUsageHint_ClaudeStartupRecovery(t *testing.T) {
	withCodexGenerationEpoch(t, 525)
	cache := isolateClaudeCache(t)
	rec, cfg := propagatorFixture(t)
	now := time.Now()
	// Captured by the PREVIOUS process, under its epoch.
	startCLIUsagePropagator(cfg)
	claudeReading(t, cache, "", now, map[string]time.Time{claudeWindowFiveHour: now})
	stopCLIUsagePropagator()
	before := len(rec.all())

	simulateCodexProcessRestart(t, 626)
	startCLIUsagePropagator(cfg)
	hints := waitHints(t, rec, before+1, 0)
	h := hints[len(hints)-1].hint
	if h.Provider != claudeUsageProvider || h.GenerationEpoch != 626 || claudeGenerationOf(t, cache) != gen(626, h.Generation) {
		t.Fatalf("recovery hint = %+v, cache = %+v", h, claudeGenerationOf(t, cache))
	}
}

// A committed clear (a generation, no numeric rows left) is recovered too.
func TestCLIUsageHint_ClaudeStartupRecoversACommittedClear(t *testing.T) {
	withCodexGenerationEpoch(t, 530)
	cache := isolateClaudeCache(t)
	rec, cfg := propagatorFixture(t)
	mutateClaudeRateLimitSnapshot(cache, "", func(snap *claudeRateLimitSnapshot) bool {
		snap.GenerationEpoch, snap.Generation = 529, 4 // a previous process published, then cleared
		return true
	})
	simulateCodexProcessRestart(t, 631)
	startCLIUsagePropagator(cfg)
	h := waitHints(t, rec, 1, 0)[0].hint
	if h.Provider != claudeUsageProvider || h.GenerationEpoch != 631 {
		t.Fatalf("recovery hint = %+v, want the rotated clear", h)
	}
}

func TestCLIUsageHint_ClaudeNeverPublishedCacheIsNotRecovered(t *testing.T) {
	withCodexGenerationEpoch(t, 527)
	cache := isolateClaudeCache(t)
	rec, cfg := propagatorFixture(t)
	// A debt marker only: no reading, no generation.
	mutateClaudeRateLimitSnapshot(cache, "", func(snap *claudeRateLimitSnapshot) bool {
		snap.RefreshOwedAtMs = time.Now().UnixMilli()
		return true
	})
	startCLIUsagePropagator(cfg)
	time.Sleep(2 * cliUsageHintSpacing)
	if n := len(rec.all()); n != 0 {
		t.Fatalf("sent %d hints for a cache that never published", n)
	}
}

// A watcher tick that notes while a cache transaction notes a rotation does
// not deadlock: the tick never holds the propagator lock across cache work.
func TestCLIUsageHint_TheWatcherDoesNotDeadlockAgainstACacheTransaction(t *testing.T) {
	withCodexGenerationEpoch(t, 528)
	cache := isolateClaudeCache(t)
	_, cfg := propagatorFixture(t)
	cliUsageWatchPeriod = time.Millisecond
	startCLIUsagePropagator(cfg)

	done := make(chan struct{})
	go func() {
		defer close(done)
		now := time.Now()
		for i := 0; i < 50; i++ {
			at := now.Add(time.Duration(i) * time.Millisecond)
			// Best-effort, as the stream capture is: on Windows a rename can lose a
			// race with the tick's stat, and the write is simply dropped.
			mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
				claudeWindowFiveHour: {UsedPercentage: 30, ResetsAtMs: at.Add(time.Hour).UnixMilli(), ObservedAtMs: at.UnixMilli(), usageKnown: true},
			}, at, "", claudeRateLimitSourceProbe)
			resetClaudeUsageWatchState()
		}
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("watcher ticks and cache transactions deadlocked")
	}
}
