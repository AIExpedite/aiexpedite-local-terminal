package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_grok_direct_test.go
   --------------------------------------------------------------------------
   The Grok direct-run detector (cliagent_usage_grok_direct.go): a turn
   marker in the persistent home's unified.jsonl owes one live read through
   the bounded run debt. These pin the exact-match rule, the read bounds,
   the clock rule, the gates and the cursor's path identity, against the run
   debt harness's fake read — no test reaches xAI or the developer's log.
   ------------------------------------------------------------------------ */

// grokDirectHarness is the run debt harness with the direct scan enabled
// against the harness's own temporary GROK_HOME.
type grokDirectHarness struct {
	*grokDebtHarness
	home string
}

func newGrokDirectHarness(t *testing.T) *grokDirectHarness {
	t.Helper()
	h := &grokDirectHarness{grokDebtHarness: newGrokDebtHarness(t), home: os.Getenv("GROK_HOME")}
	if err := os.MkdirAll(filepath.Join(h.home, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	prevEnabled, prevDetected := grokDirectScanEnabled, grokDirectDetected
	grokDirectScanEnabled = true
	grokDirectDetected = func() bool { return true }
	resetGrokDirectScan()
	t.Cleanup(func() {
		grokDirectScanEnabled, grokDirectDetected = prevEnabled, prevDetected
		resetGrokDirectScan()
	})
	return h
}

func (h *grokDirectHarness) logPath() string { return grokBillingLogPath(h.home) }

// appendRaw appends bytes to the persistent log as-is.
func (h *grokDirectHarness) appendRaw(s string) {
	h.t.Helper()
	appendGrokDirectLog(h.t, h.logPath(), s)
}

func appendGrokDirectLog(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// grokTurnMarkerLine is the line Grok writes when a direct turn finishes.
func grokTurnMarkerLine(ts time.Time) string {
	return fmt.Sprintf(`{"ts":%q,"level":"info","msg":"shell.turn.inference_done","ctx":{"turn":1}}`+"\n",
		ts.UTC().Format(time.RFC3339Nano))
}

// grokFillerLines returns about n bytes of unrelated telemetry lines.
func grokFillerLines(n int) string {
	line := `{"ts":"2026-09-30T11:00:00Z","msg":"chat: telemetry","ctx":{"pad":"` + strings.Repeat("x", 200) + `"}}` + "\n"
	return strings.Repeat(line, n/len(line)+1)
}

func (h *grokDirectHarness) scan() {
	h.t.Helper()
	grokDirectRunScanIfChanged()
	h.idle()
}

func TestGrokDirectScan_ATurnOwesOneReadAndAnUnchangedLogNone(t *testing.T) {
	h := newGrokDirectHarness(t)
	turn := h.clock().Add(-5 * time.Second)
	h.appendRaw(grokTurnMarkerLine(turn))
	h.scan()
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want one for the turn", h.reads.Load())
	}
	snap, ok := loadGrokBillingLiveSnapshot("fp-ada", h.clock())
	if !ok || !snap.HasUsedPercent || snap.ObservedAt.Before(turn) {
		t.Fatalf("live = %+v ok=%t, want a numeric reading at or after the turn", snap, ok)
	}
	h.advance(2 * grokRefreshNudgeCooldown)
	h.scan()
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want a covered turn to owe nothing more", h.reads.Load())
	}
}

func TestGrokDirectScan_MatchesTheMarkerExactlyAndOnlyInMsg(t *testing.T) {
	h := newGrokDirectHarness(t)
	ts := h.clock().Add(-5 * time.Second).UTC().Format(time.RFC3339Nano)
	h.appendRaw(fmt.Sprintf(`{"ts":%q,"msg":"shell.turn.inference_start"}`+"\n", ts))
	h.appendRaw(fmt.Sprintf(`{"ts":%q,"msg":"before shell.turn.inference_done after"}`+"\n", ts))
	h.appendRaw(fmt.Sprintf(`{"ts":%q,"msg":"tool: result","ctx":{"msg":"shell.turn.inference_done"}}`+"\n", ts))
	h.appendRaw(`{"ts":"not-a-time","msg":"shell.turn.inference_done"}` + "\n")
	h.scan()
	if h.reads.Load() != 0 {
		t.Fatalf("reads = %d, want no match outside an exact top-level msg", h.reads.Load())
	}
}

func TestGrokDirectScan_ATruncatedFirstLineOfTheWindowNeverMatches(t *testing.T) {
	h := newGrokDirectHarness(t)
	marker := grokTurnMarkerLine(h.clock().Add(-5 * time.Second))
	// The newest-window start lands inside the marker line.
	h.appendRaw(grokFillerLines(4096))
	h.appendRaw(marker)
	after := grokDirectScanMaxBytes - len(marker)/2
	h.appendRaw(grokFillerLines(after)[:after-1] + "\n")
	info, err := os.Stat(h.logPath())
	if err != nil {
		t.Fatal(err)
	}
	start := info.Size() - grokDirectScanMaxBytes
	data, _ := os.ReadFile(h.logPath())
	at := int64(strings.Index(string(data), strings.TrimSuffix(marker, "\n")))
	if !(at < start && start < at+int64(len(marker))) {
		t.Fatalf("fixture: window start %d is not inside the marker [%d,%d)", start, at, at+int64(len(marker)))
	}
	h.scan()
	if h.reads.Load() != 0 {
		t.Fatalf("reads = %d, want the window's partial first line ignored", h.reads.Load())
	}
}

func TestGrokDirectScan_AnUnchangedStatReadsNothing(t *testing.T) {
	h := newGrokDirectHarness(t)
	old := h.clock().Add(-time.Hour)
	line := grokTurnMarkerLine(old)
	h.appendRaw(line)
	// Covered by a reading after it: the first scan owes nothing.
	saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
		ObservedAt: h.clock(), PeriodType: "USAGE_PERIOD_TYPE_WEEKLY", UsedPercent: 10, HasUsedPercent: true,
	}, "fp-ada"))
	h.scan()
	info, err := os.Stat(h.logPath())
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite the same bytes with a NEWER turn, same size, same mtime: only a
	// read could find it.
	newer := grokTurnMarkerLine(h.clock().Add(time.Second))
	if len(newer) != len(line) {
		t.Fatalf("fixture: lines differ in length")
	}
	if err := os.WriteFile(h.logPath(), []byte(newer), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(h.logPath(), info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	h.advance(2 * time.Second)
	h.scan()
	if h.reads.Load() != 0 {
		t.Fatalf("reads = %d, want no read while the stat is unchanged", h.reads.Load())
	}
}

func TestGrokDirectScan_ALargeAppendScansOnlyTheNewestWindow(t *testing.T) {
	h := newGrokDirectHarness(t)
	h.appendRaw(grokFillerLines(1024))
	h.scan() // cursor at the end
	h.appendRaw(grokTurnMarkerLine(h.clock().Add(-5 * time.Second)))
	h.appendRaw(grokFillerLines(grokDirectScanMaxBytes + 4096))
	h.scan()
	if h.reads.Load() != 0 {
		t.Fatalf("reads = %d, want a marker outside the newest window unread", h.reads.Load())
	}
	h.appendRaw(grokTurnMarkerLine(h.clock().Add(-time.Second)))
	h.scan()
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want the next turn read", h.reads.Load())
	}
}

func TestGrokDirectScan_RotationOrTruncationResetsTheOffset(t *testing.T) {
	h := newGrokDirectHarness(t)
	h.appendRaw(grokFillerLines(64 * 1024))
	h.scan()
	if err := os.WriteFile(h.logPath(), []byte(grokTurnMarkerLine(h.clock().Add(-time.Second))), 0o600); err != nil {
		t.Fatal(err)
	}
	h.scan()
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want the rotated log read from its start", h.reads.Load())
	}
}

func TestGrokDirectScan_APartialLastLineWaitsUntilItCompletes(t *testing.T) {
	h := newGrokDirectHarness(t)
	line := grokTurnMarkerLine(h.clock().Add(-time.Second))
	h.appendRaw(strings.TrimSuffix(line, "\n"))
	h.scan()
	if h.reads.Load() != 0 {
		t.Fatalf("reads = %d, want a half-written line ignored", h.reads.Load())
	}
	h.appendRaw("\n")
	h.scan()
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want the completed line read", h.reads.Load())
	}
}

func TestGrokDirectScan_FarFutureIsIgnoredAndNearFutureClamped(t *testing.T) {
	h := newGrokDirectHarness(t)
	h.outcome.Store(grokLiveOutcomeHTTPError) // keep the debt on disk to inspect it
	h.appendRaw(grokTurnMarkerLine(h.clock().Add(grokBillingMaxClockSkew + time.Minute)))
	h.scan()
	if h.reads.Load() != 0 || h.state().owed() {
		t.Fatalf("reads = %d state=%+v, want a far-future turn ignored", h.reads.Load(), h.state())
	}
	h.appendRaw(grokTurnMarkerLine(h.clock().Add(time.Minute)))
	h.scan()
	if state := h.state(); !state.owed() || state.CompletionMs != h.clock().UnixMilli() {
		t.Fatalf("state = %+v, want the near-future turn clamped to now (%d)", state, h.clock().UnixMilli())
	}
}

func TestGrokDirectScan_ALiveRunDefersWithoutLosingTheTurn(t *testing.T) {
	h := newGrokDirectHarness(t)
	floor := armGrokUsageRunFloor(h.clock())
	h.appendRaw(grokTurnMarkerLine(h.clock().Add(-time.Second)))
	h.scan()
	if h.reads.Load() != 0 {
		t.Fatalf("reads = %d, want the nudge deferred while a run is live", h.reads.Load())
	}
	disarmGrokUsageRunFloor(floor)
	h.scan() // the log did not change
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want the deferred turn nudged on the next poll", h.reads.Load())
	}
}

func TestGrokDirectScan_TheCooldownDefersWithoutLosingTheTurn(t *testing.T) {
	h := newGrokDirectHarness(t)
	grokRefreshNudge.mu.Lock()
	grokRefreshNudge.lastAt = h.clock()
	grokRefreshNudge.mu.Unlock()
	h.appendRaw(grokTurnMarkerLine(h.clock().Add(-time.Second)))
	h.scan()
	if h.reads.Load() != 0 {
		t.Fatalf("reads = %d, want the cooldown to defer", h.reads.Load())
	}
	h.advance(grokRefreshNudgeCooldown)
	h.scan()
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want the turn nudged once the cooldown passed", h.reads.Load())
	}
}

func TestGrokDirectScan_ATurnScannedSignedOutIsNudgedOnTheFirstPollAfterLogin(t *testing.T) {
	h := newGrokDirectHarness(t)
	h.signIn("")
	h.appendRaw(grokTurnMarkerLine(h.clock().Add(-time.Second)))
	h.scan()
	if h.reads.Load() != 0 || h.state().owed() {
		t.Fatalf("reads = %d state=%+v, want nothing without an account", h.reads.Load(), h.state())
	}
	h.signIn("fp-ada")
	h.scan() // unchanged log
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want the turn nudged after login", h.reads.Load())
	}
}

func TestGrokDirectScan_ATurnHandedToTheDebtNeverReopensItsBudget(t *testing.T) {
	h := newGrokDirectHarness(t)
	h.outcome.Store(grokLiveOutcomeHTTPError)
	h.appendRaw(grokTurnMarkerLine(h.clock().Add(-time.Second)))
	h.scan()
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want one", h.reads.Load())
	}
	// Exhausted and cooled off: the debt could re-open on a nudge, but no new
	// turn happened, so the old one must not ask again.
	h.write(grokUsageFreshness{})
	h.advance(time.Hour)
	h.scan()
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want the same turn never re-nudged", h.reads.Load())
	}
}

func TestGrokDirectScan_ReadsOnlyThePersistentHomeAndFollowsAMove(t *testing.T) {
	h := newGrokDirectHarness(t)
	// An isolated home's log is never the scan's.
	isolated := filepath.Join(t.TempDir(), "grok-acp-home-1")
	appendGrokDirectLog(t, grokBillingLogPath(isolated), grokTurnMarkerLine(h.clock().Add(-time.Second)))
	h.appendRaw(grokFillerLines(8 * 1024))
	h.scan()
	if h.reads.Load() != 0 {
		t.Fatalf("reads = %d, want an isolated home never scanned", h.reads.Load())
	}
	// GROK_HOME moves to a home whose log is LARGER than the old offset and
	// holds its marker near the start: the cursor must reset, not seek past it.
	moved := t.TempDir()
	t.Setenv("GROK_HOME", moved)
	appendGrokDirectLog(t, grokBillingLogPath(moved), grokTurnMarkerLine(h.clock().Add(-time.Second))+grokFillerLines(32*1024))
	h.scan()
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want the moved home's turn found", h.reads.Load())
	}
}

func TestGrokDirectScan_GatesHoldThePoll(t *testing.T) {
	cases := []struct {
		name  string
		block func(t *testing.T)
	}{
		{"refresh disabled", func(*testing.T) { SetGrokUsageRefreshEnabled(false) }},
		{"not detected", func(*testing.T) {}},
		{"offline", func(t *testing.T) { setCodexTestOffline(t, true) }},
		{"draining", func(t *testing.T) {
			drain.mu.Lock()
			drain.draining = true
			drain.mu.Unlock()
			t.Cleanup(func() { drain.mu.Lock(); drain.draining = false; drain.mu.Unlock() })
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newGrokDirectHarness(t)
			if c.name == "not detected" {
				grokDirectDetected = func() bool { return false }
			}
			c.block(t)
			h.appendRaw(grokTurnMarkerLine(h.clock().Add(-time.Second)))
			h.scan()
			grokDirectMu.Lock()
			seen := grokDirectState.seen
			grokDirectMu.Unlock()
			if h.reads.Load() != 0 || seen {
				t.Fatalf("reads = %d seen=%t, want the gate to hold the scan", h.reads.Load(), seen)
			}
		})
	}
}

func TestGrokDirectScanAtStartup_NeedsNoDetection(t *testing.T) {
	h := newGrokDirectHarness(t)
	grokDirectDetected = func() bool { return false }
	h.appendRaw(grokTurnMarkerLine(h.clock().Add(-time.Second)))
	grokDirectScanAtStartup()
	h.idle()
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want the startup scan to find the turn", h.reads.Load())
	}
}

// A newest window that starts exactly on a line boundary keeps that line: the
// byte before the window says it is whole.
func TestGrokDirectScan_AWindowStartingOnALineBoundaryKeepsItsFirstLine(t *testing.T) {
	h := newGrokDirectHarness(t)
	marker := grokTurnMarkerLine(h.clock().Add(-5 * time.Second))
	h.appendRaw(grokFillerLines(4096))
	h.appendRaw(marker)
	after := grokDirectScanMaxBytes - len(marker)
	h.appendRaw(grokFillerLines(after)[:after-1] + "\n")
	data, _ := os.ReadFile(h.logPath())
	if start := len(data) - grokDirectScanMaxBytes; data[start-1] != '\n' || !strings.HasPrefix(string(data[start:]), strings.TrimSuffix(marker, "\n")) {
		t.Fatal("fixture: the window does not start at the marker line")
	}
	h.scan()
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want the whole first line of the window read", h.reads.Load())
	}
}
