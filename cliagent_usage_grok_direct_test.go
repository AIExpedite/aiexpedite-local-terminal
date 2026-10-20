package main

import (
	"context"
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

// A turn handed to an older debt's due rung moves that debt's completion: a
// reading taken before the turn (it covers only the older run) must not
// settle it while the marker is recorded as nudged.
func TestGrokDirectScan_ADueRungForAnOlderDebtPaysTheTurnToo(t *testing.T) {
	h := newGrokDirectHarness(t)
	now := h.clock()
	h.write(grokUsageFreshness{
		AccountFingerprint: "fp-ada",
		OwedAtMs:           now.Add(-10 * time.Minute).UnixMilli(),
		CompletionMs:       now.Add(-10 * time.Minute).UnixMilli(),
		LastAttemptAtMs:    now.Add(-5 * time.Minute).UnixMilli(),
		NextAttemptAtMs:    now.Add(-time.Second).UnixMilli(),
		Attempts:           1,
		LastOutcome:        grokLiveOutcomeHTTPError,
	})
	// A numeric reading after the older run but before the turn.
	saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
		ObservedAt: now.Add(-30 * time.Second), PeriodType: "USAGE_PERIOD_TYPE_WEEKLY", UsedPercent: 12, HasUsedPercent: true,
	}, "fp-ada"))
	turn := now.Add(-5 * time.Second)
	h.appendRaw(grokTurnMarkerLine(turn))
	h.scan()
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want the due rung to read for the turn", h.reads.Load())
	}
	snap, ok := loadGrokBillingLiveSnapshot("fp-ada", h.clock())
	if !ok || !snap.HasUsedPercent || snap.ObservedAt.Before(turn) {
		t.Fatalf("live = %+v ok=%t, want a numeric reading at or after the turn", snap, ok)
	}
	if h.state().owed() {
		t.Fatalf("state = %+v, want the debt paid", h.state())
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

// A log rotated at the same path is read from its start, even when the new
// file already outgrew the old offset.
func TestGrokDirectScan_ALogReplacedAtTheSamePathIsReadFromItsStart(t *testing.T) {
	h := newGrokDirectHarness(t)
	h.appendRaw(grokFillerLines(8 * 1024))
	h.scan()
	replacement := filepath.Join(filepath.Dir(h.logPath()), "unified.jsonl.new")
	content := grokTurnMarkerLine(h.clock().Add(-time.Second)) + grokFillerLines(32*1024)
	if err := os.WriteFile(replacement, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, h.logPath()); err != nil {
		t.Fatal(err)
	}
	h.scan()
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want the rotated log's turn found", h.reads.Load())
	}
}

// A turn whose debt was retired because the login moved is no longer handled:
// it is never paid under the other account, and is owed again once its own
// account signs back in.
func TestGrokDirectScan_ATurnRetiredAcrossAnAccountChangeIsOwedAgainUnderItsAccount(t *testing.T) {
	h := newGrokDirectHarness(t)
	h.outcome.Store(liveProbeOutcomeAccountChanged)
	turn := h.clock().Add(-time.Second)
	h.appendRaw(grokTurnMarkerLine(turn))
	h.scan()
	if h.reads.Load() != 1 || h.state().owed() {
		t.Fatalf("reads = %d state=%+v, want one read retired as account_changed", h.reads.Load(), h.state())
	}
	h.outcome.Store(grokLiveOutcomeOK)
	h.signIn("fp-bob")
	h.advance(2 * grokRefreshNudgeCooldown)
	h.scan()
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want fp-ada's turn never paid under fp-bob", h.reads.Load())
	}
	h.signIn("fp-ada")
	h.advance(2 * grokRefreshNudgeCooldown)
	h.scan()
	if h.reads.Load() != 2 {
		t.Fatalf("reads = %d, want the turn owed again under its own account", h.reads.Load())
	}
	if !grokLiveReadingCovers("fp-ada", turn.UnixMilli(), h.clock()) {
		t.Fatal("want fp-ada's reading to cover the turn")
	}
}

// The worker a nudge starts can retire the debt as account_changed (and
// forget the handled marker) before the scan records the nudge: the scan must
// not then mark the unpaid turn handled.
func TestGrokDirectScan_AForgetBeforeTheNudgeIsRecordedKeepsTheTurnOwed(t *testing.T) {
	h := newGrokDirectHarness(t)
	h.outcome.Store(liveProbeOutcomeAccountChanged)
	prevNudge := grokDirectNudge
	grokDirectNudge = func(now, completion time.Time, fingerprint string) bool {
		started := prevNudge(now, completion, fingerprint)
		h.idle() // the worker retires the debt and forgets first
		return started
	}
	t.Cleanup(func() { grokDirectNudge = prevNudge })
	turn := h.clock().Add(-time.Second)
	h.appendRaw(grokTurnMarkerLine(turn))
	h.scan()
	if h.reads.Load() != 1 || h.state().owed() {
		t.Fatalf("reads = %d state=%+v, want one read retired as account_changed", h.reads.Load(), h.state())
	}
	grokDirectMu.Lock()
	nudged := grokDirectState.nudged["fp-ada"]
	grokDirectMu.Unlock()
	if nudged != 0 {
		t.Fatalf("nudged = %d, want the retired turn not recorded as handled", nudged)
	}
	h.outcome.Store(grokLiveOutcomeOK)
	h.signIn("fp-bob")
	h.advance(2 * grokRefreshNudgeCooldown)
	h.scan()
	h.signIn("fp-ada")
	h.advance(2 * grokRefreshNudgeCooldown)
	h.scan()
	if h.reads.Load() != 2 {
		t.Fatalf("reads = %d, want the turn owed again under its own account", h.reads.Load())
	}
	if !grokLiveReadingCovers("fp-ada", turn.UnixMilli(), h.clock()) {
		t.Fatal("want fp-ada's reading to cover the turn")
	}
}

// A read with no percentage but a numeric on-demand pool refreshes the card's
// number, so it pays the turn instead of spending the rest of the ladder.
func TestGrokDirectScan_ANumericOnDemandReadingPaysTheTurn(t *testing.T) {
	h := newGrokDirectHarness(t)
	grokBillingLiveReadFn = func(_ context.Context, _, _ string, now func() time.Time) string {
		h.reads.Add(1)
		saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
			ObservedAt: now(), PeriodType: "USAGE_PERIOD_TYPE_WEEKLY",
			HasOnDemand: true, OnDemandCap: 50, HasOnDemandUsed: true, OnDemandUsed: 7,
		}, h.fingerprint()))
		return grokLiveOutcomeOK
	}
	turn := h.clock().Add(-time.Second)
	h.appendRaw(grokTurnMarkerLine(turn))
	h.scan()
	if h.reads.Load() != 1 || h.state().owed() {
		t.Fatalf("reads = %d state=%+v, want the on-demand reading to pay the debt", h.reads.Load(), h.state())
	}
	if got := grokLastRefreshOutcome.Load(); got != grokLiveOutcomeOK {
		t.Fatalf("outcome = %v, want ok (not relabelled unmetered)", got)
	}
	for range 4 {
		h.advance(time.Hour)
		h.scan()
	}
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want no further reads for a covered turn", h.reads.Load())
	}
}

// A turn the polls saw under one login, found only after the user switched to
// another, cannot be told from a turn the new login ran before the poll: it is
// paid under the new login too, but never retired there, so the earlier
// account's turn stays owed and is paid once that account is back.
func TestGrokDirectScan_ADelayedMarkerIsNeverAssignedToTheNextLogin(t *testing.T) {
	h := newGrokDirectHarness(t)
	h.scan() // a poll sees fp-ada signed in
	h.advance(5 * time.Second)
	turn := h.clock()
	h.appendRaw(grokTurnMarkerLine(turn))
	h.advance(3 * time.Second)
	h.signIn("fp-bob")
	h.advance(2 * time.Second)
	h.scan()
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want one read under fp-bob for the ambiguous turn", h.reads.Load())
	}
	h.advance(2 * grokRefreshNudgeCooldown)
	h.scan()
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want fp-bob's reading to cover the turn with no further reads", h.reads.Load())
	}
	h.signIn("fp-ada")
	h.scan()
	if h.reads.Load() != 2 || !grokLiveReadingCovers("fp-ada", turn.UnixMilli(), h.clock()) {
		t.Fatalf("reads = %d, want the turn still paid once fp-ada is back", h.reads.Load())
	}
}

// A single turn the new login ran between the poll that saw the old login and
// the next one is paid under the new login, not filed only under the old one.
func TestGrokDirectScan_ASingleTurnAfterASwitchIsPaidUnderTheNewLogin(t *testing.T) {
	h := newGrokDirectHarness(t)
	h.scan() // a poll sees fp-ada signed in
	h.advance(2 * time.Second)
	h.signIn("fp-bob")
	h.advance(3 * time.Second)
	bobTurn := h.clock()
	h.appendRaw(grokTurnMarkerLine(bobTurn))
	h.advance(2 * time.Second)
	h.scan()
	if h.reads.Load() != 1 || !grokLiveReadingCovers("fp-bob", bobTurn.UnixMilli(), h.clock()) {
		t.Fatalf("reads = %d, want fp-bob's turn paid under fp-bob", h.reads.Load())
	}
}

// A turn found while signed out belongs to the account that signed out, not
// to whoever logs in next.
func TestGrokDirectScan_AMarkerFoundSignedOutBelongsToTheAccountThatLeft(t *testing.T) {
	h := newGrokDirectHarness(t)
	h.scan() // fp-ada signed in
	h.advance(5 * time.Second)
	h.appendRaw(grokTurnMarkerLine(h.clock()))
	h.signIn("")
	h.advance(5 * time.Second)
	h.scan()
	h.signIn("fp-bob")
	h.advance(10 * time.Second)
	h.scan()
	if h.reads.Load() != 0 {
		t.Fatalf("reads = %d, want fp-ada's turn never paid under fp-bob", h.reads.Load())
	}
	h.signIn("fp-ada")
	h.advance(10 * time.Second)
	h.scan()
	if h.reads.Load() != 1 {
		t.Fatalf("reads = %d, want the turn paid under fp-ada", h.reads.Load())
	}
}

// A turn written while the polls saw nobody signed in (after an account was
// seen) has no owner and is never paid.
func TestGrokDirectScan_AMarkerInsideASignedOutSpanHasNoOwner(t *testing.T) {
	h := newGrokDirectHarness(t)
	h.scan() // fp-ada
	h.signIn("")
	h.advance(10 * time.Second)
	h.scan()
	h.advance(10 * time.Second)
	h.appendRaw(grokTurnMarkerLine(h.clock()))
	h.advance(5 * time.Second)
	h.scan()
	for _, fp := range []string{"fp-bob", "fp-ada"} {
		h.signIn(fp)
		h.advance(2 * grokRefreshNudgeCooldown)
		h.scan()
	}
	if h.reads.Load() != 0 {
		t.Fatalf("reads = %d, want an unowned turn never paid", h.reads.Load())
	}
}

// Turns by two accounts between the same two polls are not collapsed into one
// marker: the newest is paid under the login that switched in, and the
// earlier account's turn stays owed until that account is back.
func TestGrokDirectScan_TurnsOnBothSidesOfALoginSwitchAreBothPaid(t *testing.T) {
	h := newGrokDirectHarness(t)
	h.scan() // a poll sees fp-ada signed in
	h.advance(5 * time.Second)
	adaTurn := h.clock()
	h.appendRaw(grokTurnMarkerLine(adaTurn))
	h.signIn("fp-bob")
	h.advance(3 * time.Second)
	bobTurn := h.clock()
	h.appendRaw(grokTurnMarkerLine(bobTurn))
	h.advance(2 * time.Second)
	h.scan()
	if h.reads.Load() != 1 || !grokLiveReadingCovers("fp-bob", bobTurn.UnixMilli(), h.clock()) {
		t.Fatalf("reads = %d, want fp-bob's turn paid under fp-bob", h.reads.Load())
	}
	h.signIn("fp-ada")
	h.advance(2 * grokRefreshNudgeCooldown)
	h.scan()
	if h.reads.Load() != 2 || !grokLiveReadingCovers("fp-ada", adaTurn.UnixMilli(), h.clock()) {
		t.Fatalf("reads = %d, want fp-ada's turn still owed and paid once it is back", h.reads.Load())
	}
}
