package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_grok_direct_run_test.go
   --------------------------------------------------------------------------
   Acceptance: after a successful Grok run — direct or terminal-managed —
   the card publishes a numeric credit reading taken at or after that run,
   and terminal-service is told about it with one Grok usage hint. A
   loopback billing server stands in for xAI, a temporary real home for
   ~/.grok, and a recording sink for terminal-service.
   ------------------------------------------------------------------------ */

type grokAcceptance struct {
	t      *testing.T
	home   string
	rec    *cliUsageHintRecorder
	cfg    *Config
	reads  *int32
	status atomic.Int32
	// unmetered makes a 200 answer carry no percentage.
	unmetered atomic.Bool
}

func newGrokAcceptance(t *testing.T, epoch int64) *grokAcceptance {
	t.Helper()
	withCodexGenerationEpoch(t, epoch)
	a := &grokAcceptance{t: t, home: isolateGrok(t)}
	writeGrokAuth(t, a.home, "access-A", "ada@example.com", time.Now().Add(5*time.Hour))
	grokEnableRunRefresh(t)
	a.status.Store(http.StatusOK)
	a.reads = grokBillingServer(t, func(string) (int, string) {
		status := int(a.status.Load())
		if status != http.StatusOK {
			return status, `{}`
		}
		if a.unmetered.Load() {
			return status, `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"` +
				time.Now().Add(72*time.Hour).UTC().Format(time.RFC3339Nano) + `"}}}`
		}
		return status, grokFixtureBody(time.Now().Add(72 * time.Hour))
	})
	if err := os.MkdirAll(filepath.Join(a.home, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	a.rec, a.cfg = propagatorFixture(t)
	cliUsageClaudeFallbackPoll = 20 * time.Millisecond
	prevEnabled, prevDetected := grokDirectScanEnabled, grokDirectDetected
	grokDirectScanEnabled = true
	grokDirectDetected = func() bool { return true }
	resetGrokDirectScan()
	resetGrokNudgeCooldown()
	t.Cleanup(func() {
		stopCLIUsagePropagator()
		resetCLIUsagePropagator()
		grokDirectScanEnabled, grokDirectDetected = prevEnabled, prevDetected
		resetGrokNudgeCooldown()
	})
	return a
}

func resetGrokNudgeCooldown() {
	grokRefreshNudge.mu.Lock()
	grokRefreshNudge.lastAt = time.Time{}
	grokRefreshNudge.mu.Unlock()
}

func (a *grokAcceptance) start() {
	a.t.Helper()
	startCLIUsagePropagator(a.cfg)
	cliUsagePropagator.rotating.Wait()
}

// grokHints returns the Grok hints recorded so far.
func (a *grokAcceptance) grokHints() []cliUsageObservedHint {
	var out []cliUsageObservedHint
	for _, r := range a.rec.all() {
		if r.hint.Provider == grokUsageProvider {
			out = append(out, r.hint)
		}
	}
	return out
}

func (a *grokAcceptance) waitGrokHint() cliUsageObservedHint {
	a.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if hints := a.grokHints(); len(hints) > 0 {
			return hints[0]
		}
		if time.Now().After(deadline) {
			a.t.Fatalf("no grok hint; reads=%d hints=%+v", atomic.LoadInt32(a.reads), a.rec.all())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// assertFreshNumber parses the card and checks a numeric reading observed at
// or after since, versioned with want.
func (a *grokAcceptance) assertFreshNumber(since time.Time, want cliUsageObservedHint) {
	a.t.Helper()
	usage, ok := grokUsageParser{}.Parse(filepath.Dir(a.home), detectedCLIAgent{Detected: true}, time.Now())
	if !ok || len(usage.Metrics) == 0 {
		a.t.Fatalf("parse = %+v ok=%t", usage, ok)
	}
	metric := usage.Metrics[0]
	if metric.Unknown || metric.Consumed == nil || *metric.Consumed != 9 {
		a.t.Fatalf("row = %+v, want the live numeric reading", metric)
	}
	observed, err := time.Parse(time.RFC3339, metric.ObservedAt)
	if err != nil || observed.Before(since.Truncate(time.Second)) {
		a.t.Fatalf("observedAt %q predates the run (%s)", metric.ObservedAt, since)
	}
	g := usage.UsageGeneration
	if g == nil || g.Epoch != want.GenerationEpoch || g.Counter != want.Generation {
		a.t.Fatalf("usageGeneration = %+v, want the hinted {%d,%d}", g, want.GenerationEpoch, want.Generation)
	}
}

// assertOneGrokGeneration: every Grok hint names one generation (its first
// hint plus at most the one follow-up).
func (a *grokAcceptance) assertOneGrokGeneration() {
	a.t.Helper()
	time.Sleep(3 * cliUsageHintSpacing)
	hints := a.grokHints()
	if len(hints) == 0 || len(hints) > 2 {
		a.t.Fatalf("grok hints = %+v, want one generation's hint and at most its follow-up", hints)
	}
	for _, h := range hints[1:] {
		if h.GenerationEpoch != hints[0].GenerationEpoch || h.Generation != hints[0].Generation {
			a.t.Fatalf("grok hints = %+v, want a single generation", hints)
		}
	}
}

func TestGrokUsage_DirectRunPublishesAFreshNumberAndHintsOnce(t *testing.T) {
	a := newGrokAcceptance(t, 7101)
	a.start()
	turn := time.Now()
	appendGrokDirectLog(t, grokBillingLogPath(a.home), grokTurnMarkerLine(turn))

	hint := a.waitGrokHint()
	if hint.GenerationEpoch != 7101 || hint.Generation != 1 {
		t.Fatalf("hint = %+v, want this process's first generation", hint)
	}
	if got := atomic.LoadInt32(a.reads); got != 1 {
		t.Fatalf("billing reads = %d, want one", got)
	}
	a.assertFreshNumber(turn, hint)
	a.assertOneGrokGeneration()
}

// An interactive `grok` TUI in a PTY on the persistent home is not armed by
// session.go; its turn reaches the card through the same marker.
func TestGrokUsage_InteractivePersistentHomePTYTurnTakesTheMarkerPath(t *testing.T) {
	// session.go arms a PTY grok only for an isolated home or the managed `-p`
	// argv; a TUI's argv carries neither sentinel.
	if isManagedGrokPromptArgv(nil) || isManagedGrokPromptArgv([]string{"--model", "grok-4"}) {
		t.Fatal("a TUI argv reads as managed: session.go would arm it and this case no longer applies")
	}
	a := newGrokAcceptance(t, 7102)
	a.start()
	turn := time.Now()
	appendGrokDirectLog(t, grokBillingLogPath(a.home),
		`{"ts":"`+turn.Add(-time.Second).UTC().Format(time.RFC3339Nano)+`","msg":"shell.turn.inference_start"}`+"\n"+
			grokTurnMarkerLine(turn))
	hint := a.waitGrokHint()
	a.assertFreshNumber(turn, hint)
	a.assertOneGrokGeneration()
}

// A managed run (smoke, ACP, isolated-home or managed `grok -p` PTY) settles
// its own debt, and its commit hints the same way. A direct turn finishing
// while it is live is covered by the managed read: one request, not two.
func TestGrokUsage_ManagedRunHintsAndOverlapsADirectTurnWithOneRead(t *testing.T) {
	a := newGrokAcceptance(t, 7103)
	a.start()
	floor := armGrokUsageRunFloorFor(time.Now(), "")
	turn := time.Now()
	appendGrokDirectLog(t, grokBillingLogPath(a.home), grokTurnMarkerLine(turn))
	time.Sleep(10 * cliUsageClaudeFallbackPoll) // the polls see the turn and defer
	if got := atomic.LoadInt32(a.reads); got != 0 {
		t.Fatalf("billing reads = %d while the managed run is live, want 0", got)
	}
	if !grokUsageRunSettled(floor) {
		t.Fatal("the managed run must owe a read")
	}
	hint := a.waitGrokHint()
	grokUsageRefreshWaitFor(10 * time.Second)
	time.Sleep(10 * cliUsageClaudeFallbackPoll) // the polls see the turn covered
	grokUsageRefreshWaitFor(10 * time.Second)
	if got := atomic.LoadInt32(a.reads); got != 1 {
		t.Fatalf("billing reads = %d, want the managed read to cover the direct turn", got)
	}
	a.assertFreshNumber(turn, hint)
	a.assertOneGrokGeneration()
}

// Failure paths keep the debt's 4-read cap and never hint without a number.
func TestGrokUsage_FailedDirectReadsKeepTheCapAndNeverHint(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		unmetered bool
	}{
		{"http_error", http.StatusInternalServerError, false},
		{"unmetered", http.StatusOK, true},
		{"unauthorized", http.StatusUnauthorized, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newGrokAcceptance(t, 7104)
			a.status.Store(int32(c.status))
			a.unmetered.Store(c.unmetered)
			if c.status == http.StatusUnauthorized {
				runGrokLoginRenewal = func(context.Context, string, string) {}
			}
			a.start()
			appendGrokDirectLog(t, grokBillingLogPath(a.home), grokTurnMarkerLine(time.Now()))
			deadline := time.Now().Add(5 * time.Second)
			for atomic.LoadInt32(a.reads) == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			grokUsageRefreshWaitFor(10 * time.Second)
			first := atomic.LoadInt32(a.reads)
			if first == 0 {
				t.Fatal("the direct turn never owed a read")
			}
			// Exhaust the budget as the ladder would, then keep writing turns:
			// no further read may go out while the debt cools off.
			updateGrokUsageFreshness(func(s *grokUsageFreshness) {
				if s.owed() {
					s.Attempts, s.LastAttemptAtMs, s.NextAttemptAtMs = grokRunDebtMaxAttempts, time.Now().UnixMilli(), 0
				}
			})
			for i := 0; i < 3; i++ {
				resetGrokNudgeCooldown()
				appendGrokDirectLog(t, grokBillingLogPath(a.home), grokTurnMarkerLine(time.Now()))
				time.Sleep(5 * cliUsageClaudeFallbackPoll)
			}
			grokUsageRefreshWaitFor(10 * time.Second)
			if got := atomic.LoadInt32(a.reads); got != first || got > grokRunDebtMaxAttempts {
				t.Fatalf("billing reads = %d, want %d (capped)", got, first)
			}
			time.Sleep(2 * cliUsageHintSpacing)
			if hints := a.grokHints(); len(hints) != 0 {
				t.Fatalf("grok hints = %+v, want none without a numeric commit", hints)
			}
		})
	}
}
