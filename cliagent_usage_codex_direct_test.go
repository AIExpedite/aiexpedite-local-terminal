package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_codex_direct_test.go — the Codex direct-run detector's bounds
   and gates (cliagent_usage_codex_direct.go). The reconcile and the held-
   evidence read are counted through their seams, so no row here runs one; the
   end-to-end rows are in cliagent_usage_codex_post_update_freshness_test.go.
   ------------------------------------------------------------------------ */

// codexDirectHarness is an isolated CODEX_HOME with the detector enabled, a
// pinned clock, and counting seams.
type codexDirectHarness struct {
	f codexFreshnessFixture

	mu    sync.Mutex
	now   time.Time
	bases []string
	held  bool
	// heldReads counts held-evidence cache reads.
	heldReads int
	// reconcile, when set, runs in place of the counted no-op and reports
	// whether the pass was complete.
	reconcile func(ctx context.Context, base, fp string, now time.Time) bool
	// realHeld is the production held-evidence read (a cache view).
	realHeld func(fp string) bool
}

// codexDirectFixture enables the detector against a temporary home, with the
// clock pinned at start (whose location is the "local" zone).
func codexDirectFixture(t *testing.T, start time.Time) *codexDirectHarness {
	t.Helper()
	return codexDirectFixtureOver(t, newCodexFreshnessFixture(t, start.Add(-3*time.Hour)), start)
}

// codexDirectFixtureOver is codexDirectFixture over an existing freshness
// fixture (a smoke's, say).
func codexDirectFixtureOver(t *testing.T, f codexFreshnessFixture, start time.Time) *codexDirectHarness {
	t.Helper()
	h := &codexDirectHarness{f: f, now: start}
	prevEnabled, prevDetected := codexDirectScanEnabled, codexDirectDetected
	prevReconcile, prevHeld := codexDirectReconcile, codexDirectHeldEvidence
	h.realHeld = prevHeld
	prevNow, prevInterval, prevBudget := codexUsageFreshnessNow, codexForcedReconcileMinInterval, codexDirectReconcileBudget
	prevPeriod := cliUsageClaudeFallbackPeriod
	codexDirectScanEnabled = true
	codexDirectDetected = func() bool { return true }
	codexUsageFreshnessNow = h.clock
	codexForcedReconcileMinInterval = 20 * time.Second
	cliUsageClaudeFallbackPeriod = time.Minute
	codexDirectReconcile = func(ctx context.Context, base, fp string, now time.Time) bool {
		h.mu.Lock()
		h.bases = append(h.bases, base)
		run := h.reconcile
		h.mu.Unlock()
		if run != nil {
			return run(ctx, base, fp, now)
		}
		return true
	}
	codexDirectHeldEvidence = func(string) bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.heldReads++
		return h.held
	}
	resetCodexDirectScan()
	t.Cleanup(func() {
		resetCodexDirectScan()
		codexDirectScanEnabled, codexDirectDetected = prevEnabled, prevDetected
		codexDirectReconcile, codexDirectHeldEvidence = prevReconcile, prevHeld
		codexUsageFreshnessNow, codexForcedReconcileMinInterval, codexDirectReconcileBudget = prevNow, prevInterval, prevBudget
		cliUsageClaudeFallbackPeriod = prevPeriod
	})
	return h
}

// endToEnd runs the real reconcile-and-nudge and the real held-evidence read
// on wall time, with the freshness fixture's test-sized interval, for rows that
// drive the debt ladder.
func (h *codexDirectHarness) endToEnd() {
	codexUsageFreshnessNow = time.Now
	codexForcedReconcileMinInterval = 20 * time.Millisecond
	codexDirectHeldEvidence = h.realHeld
	h.mu.Lock()
	h.reconcile = func(ctx context.Context, base, fp string, now time.Time) bool {
		return !codexReconcileAndNudge(ctx, base, fp, now, false).rollouts.incomplete
	}
	h.mu.Unlock()
}

// pollWall waits out the interval on wall time and runs one poll.
func (h *codexDirectHarness) pollWall() {
	time.Sleep(2 * codexForcedReconcileMinInterval)
	codexDirectRunScanIfChanged()
}

func (h *codexDirectHarness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *codexDirectHarness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

func (h *codexDirectHarness) reconciles() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.bases)
}

func (h *codexDirectHarness) heldReadCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.heldReads
}

func (h *codexDirectHarness) setHeld(held bool) {
	h.mu.Lock()
	h.held = held
	h.mu.Unlock()
}

// dayDir is the sessions/YYYY/MM/DD directory for day under the harness home.
func (h *codexDirectHarness) dayDir(day time.Time) string {
	return filepath.Join(h.f.home, "sessions", day.Format("2006"), day.Format("01"), day.Format("02"))
}

// writeRollout writes (or appends to) a rollout in dir and stamps its mtime.
func (h *codexDirectHarness) writeRollout(t *testing.T, dir, name, body string, mtime time.Time) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "rollout-"+name+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open rollout: %v", err)
	}
	if _, err := f.WriteString(body); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	_ = f.Close()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes rollout: %v", err)
	}
	return path
}

// poll advances the clock past the reconcile interval and runs one poll.
func (h *codexDirectHarness) poll() {
	h.advance(codexForcedReconcileMinInterval + time.Second)
	codexDirectRunScanIfChanged()
}

func (h *codexDirectHarness) wantReconciles(t *testing.T, want int, why string) {
	t.Helper()
	if got := h.reconciles(); got != want {
		t.Fatalf("%s: reconciles = %d, want %d", why, got, want)
	}
}

// A fixed winter evening in a UTC-8 zone: the local date is the 20th, the UTC
// date the 21st, so local-today, local-yesterday and UTC-today all differ.
var codexDirectTestZone = time.FixedZone("UTC-8", -8*60*60)
var codexDirectTestStart = time.Date(2026, 11, 20, 20, 0, 0, 0, codexDirectTestZone)

// An unchanged tree costs stats only: after the baseline, no poll reconciles.
func TestCodexDirectScan_UnchangedSignatureNeverReconciles(t *testing.T) {
	h := codexDirectFixture(t, codexDirectTestStart)
	h.writeRollout(t, h.dayDir(h.clock()), "a", "{}\n", h.clock().Add(-time.Hour))

	codexDirectRunScanIfChanged()
	h.wantReconciles(t, 1, "the first poll takes the baseline")
	for i := 0; i < 20; i++ {
		h.advance(time.Second)
		codexDirectRunScanIfChanged()
	}
	h.wantReconciles(t, 1, "an unchanged signature")
}

// A new rollout in local-today, or an append to a tracked one, reconciles
// exactly once.
func TestCodexDirectScan_NewFileOrAppendReconcilesOnce(t *testing.T) {
	h := codexDirectFixture(t, codexDirectTestStart)
	today := h.dayDir(h.clock())
	h.writeRollout(t, today, "a", "{}\n", h.clock().Add(-time.Hour))
	codexDirectRunScanIfChanged()
	h.wantReconciles(t, 1, "baseline")

	h.writeRollout(t, today, "b", "{}\n", h.clock())
	h.poll()
	h.wantReconciles(t, 2, "a new rollout in local-today")
	h.poll()
	h.wantReconciles(t, 2, "the same tree again")

	// Same mtime as before, only the size moves: the coarse-mtime case.
	h.writeRollout(t, today, "b", "{}\n", h.clock().Add(-codexForcedReconcileMinInterval-time.Second))
	h.poll()
	h.wantReconciles(t, 3, "an append to a tracked rollout")
	h.poll()
	h.wantReconciles(t, 3, "the same tree again")
}

// UTC-today and local-yesterday are watched as well as local-today: a host
// whose local and UTC dates differ — the Windows nightly — files its rollout in
// whichever the Codex build picks.
func TestCodexDirectScan_WatchesUTCTodayAndLocalYesterday(t *testing.T) {
	h := codexDirectFixture(t, codexDirectTestStart)
	now := h.clock()
	dirs := codexRolloutWatchDirs(h.f.home, now)
	want := []string{h.dayDir(now), h.dayDir(now.AddDate(0, 0, -1)), h.dayDir(now.UTC())}
	if !slices.Equal(dirs, want) {
		t.Fatalf("watch dirs = %v, want %v", dirs, want)
	}
	if got := codexRolloutWatchDirs(h.f.home, now.UTC()); len(got) != 2 {
		t.Fatalf("on a UTC host local-today and UTC-today coincide: %v", got)
	}

	codexDirectRunScanIfChanged()
	h.wantReconciles(t, 1, "baseline over an empty tree")
	h.writeRollout(t, h.dayDir(now.UTC()), "utc", "{}\n", now)
	h.poll()
	h.wantReconciles(t, 2, "a rollout in UTC-today")
	h.writeRollout(t, h.dayDir(now.AddDate(0, 0, -1)), "yesterday", "{}\n", now)
	h.poll()
	h.wantReconciles(t, 3, "a rollout in local-yesterday")
	h.writeRollout(t, h.dayDir(now.AddDate(0, 0, -3)), "old", "{}\n", now)
	h.poll()
	h.wantReconciles(t, 3, "an older date directory is never watched")
}

// A run this process armed and has not settled holds the reconcile back; the
// settle releases it on the next poll, with nothing else changed.
func TestCodexDirectScan_OpenRunSuppressesUntilSettled(t *testing.T) {
	h := codexDirectFixture(t, codexDirectTestStart)
	codexDirectRunScanIfChanged()
	h.wantReconciles(t, 1, "baseline")

	// The session manager's view of a terminal `codex` run that
	// codexUsageRunStarted armed (session.go armCodexUsageRun).
	sm := NewSessionManager(nil)
	session := &CLISession{ID: "open-codex-run", Command: "codex"}
	session.codexUsageFloorMs.Store(h.clock().UnixMilli())
	sm.sessions[session.ID] = session
	previous := globalSessionManager
	globalSessionManager = sm
	t.Cleanup(func() { globalSessionManager = previous })

	h.writeRollout(t, h.dayDir(h.clock()), "armed", "{}\n", h.clock())
	for i := 0; i < 3; i++ {
		h.poll()
	}
	h.wantReconciles(t, 1, "an open armed run")

	session.codexUsageSettled.Store(true)
	h.poll()
	h.wantReconciles(t, 2, "the settled run releases the skipped signature")
}

// codexForcedReconcileMinInterval spaces back-to-back changes, and the skipped
// change is reconciled once the interval passes, without another write.
func TestCodexDirectScan_IntervalSpacesChangesAndKeepsTheSkippedOne(t *testing.T) {
	h := codexDirectFixture(t, codexDirectTestStart)
	today := h.dayDir(h.clock())
	codexDirectRunScanIfChanged()
	h.wantReconciles(t, 1, "baseline")

	h.writeRollout(t, today, "a", "{}\n", h.clock())
	h.advance(time.Second)
	codexDirectRunScanIfChanged()
	h.wantReconciles(t, 1, "a change inside the interval")
	h.advance(time.Second)
	codexDirectRunScanIfChanged()
	h.wantReconciles(t, 1, "still inside the interval")

	h.advance(codexForcedReconcileMinInterval)
	codexDirectRunScanIfChanged()
	h.wantReconciles(t, 2, "the skipped change once the interval passed")
	h.poll()
	h.wantReconciles(t, 2, "and only once")
}

// A reconcile cut short by its budget stores no signature: the next eligible
// poll reconciles again with nothing changed.
func TestCodexDirectScan_BudgetExpiryLeavesTheSignatureUnstored(t *testing.T) {
	h := codexDirectFixture(t, codexDirectTestStart)
	codexDirectReconcileBudget = 10 * time.Millisecond
	h.reconcile = func(ctx context.Context, _, _ string, _ time.Time) bool { <-ctx.Done(); return true }
	h.writeRollout(t, h.dayDir(h.clock()), "a", "{}\n", h.clock())

	codexDirectRunScanIfChanged()
	h.poll()
	h.wantReconciles(t, 2, "a reconcile that hit its budget is retried")

	h.mu.Lock()
	h.reconcile = nil
	h.mu.Unlock()
	h.poll()
	h.wantReconciles(t, 3, "the retry that finishes")
	h.poll()
	h.wantReconciles(t, 3, "stores the signature")
}

// A rollout that can never be read to the end (an ACL or AV hold, or one too
// large for its share of the file budget) keeps every pass unsettled. Without a
// bound the scan would spend a full reconcile budget on the same unchanged tree
// on every eligible poll for the life of the process, silently - the log line
// sits after the unsettled return. The gather still covers such a tree.
func TestCodexDirectScan_StopsReconcilingAnUnsettledUnchangedTree(t *testing.T) {
	h := codexDirectFixture(t, codexDirectTestStart)
	h.mu.Lock()
	h.reconcile = func(context.Context, string, string, time.Time) bool { return false }
	h.mu.Unlock()
	h.writeRollout(t, h.dayDir(h.clock()), "a", "{}\n", h.clock())

	for range codexDirectUnsettledAttempts + 7 {
		h.poll()
	}
	h.wantReconciles(t, codexDirectUnsettledAttempts,
		"an always-unsettled tree stops at the attempt bound")

	// The bound is not a permanent stand-down: a change to the tree re-arms it.
	h.writeRollout(t, h.dayDir(h.clock()), "b", "{}\n", h.clock())
	h.poll()
	h.wantReconciles(t, codexDirectUnsettledAttempts+1, "a changed tree re-arms the scan")
}

// A reconcile that returns inside its budget but reports discovery or
// selection incomplete stores no signature either: the parent context is still
// live, yet the pass withheld its evidence, and a finished run that writes
// nothing more would otherwise stay unfound until the next gather.
func TestCodexDirectScan_IncompleteReconcileLeavesTheSignatureUnstored(t *testing.T) {
	h := codexDirectFixture(t, codexDirectTestStart)
	h.mu.Lock()
	h.reconcile = func(context.Context, string, string, time.Time) bool { return false }
	h.mu.Unlock()
	h.writeRollout(t, h.dayDir(h.clock()), "a", "{}\n", h.clock())

	codexDirectRunScanIfChanged()
	h.poll()
	h.wantReconciles(t, 2, "a reconcile that reported itself incomplete is retried")

	h.mu.Lock()
	h.reconcile = nil
	h.mu.Unlock()
	h.poll()
	h.wantReconciles(t, 3, "the retry that completes")
	h.poll()
	h.wantReconciles(t, 3, "stores the signature")
}

// The real reconcile reports a pass whose discovery sub-budget ran out as
// incomplete even though the parent context it was handed is still live — the
// case ctx.Err() alone cannot see.
func TestCodexReconcileFromRollout_DiscoveryCutShortReportsIncomplete(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	writeCodexRunRollout(t, f.home, "slow-discovery", now.Add(-5*time.Minute), now.Add(-time.Minute), now.Add(-time.Minute), true,
		[]map[string]any{codexRateLimitFrame(41, 42, now.Add(-time.Minute))})

	// Parent budget 2 s: discovery gets the first second (codexRolloutDiscoveryContext
	// reserves codexRolloutCandidateReadReserve), so stalling the first listing
	// past it cuts discovery short with the parent still live.
	ctx, cancel := context.WithTimeout(context.Background(), 2*codexRolloutCandidateReadReserve)
	defer cancel()
	discoveryDeadline, _ := ctx.Deadline()
	discoveryDeadline = discoveryDeadline.Add(-codexRolloutCandidateReadReserve)
	originalHook := codexRolloutReadDirChunkHook
	defer func() { codexRolloutReadDirChunkHook = originalHook }()
	var once sync.Once
	codexRolloutReadDirChunkHook = func(string, int) {
		once.Do(func() { time.Sleep(time.Until(discoveryDeadline) + 50*time.Millisecond) })
	}

	_, _, _, evidence := codexReconcileFromRollout(ctx, f.home, f.fp, now)
	if ctx.Err() != nil {
		t.Fatalf("the parent context must still be live for this case: %v", ctx.Err())
	}
	if !evidence.incomplete {
		t.Fatal("a pass whose discovery ran out of budget must report itself incomplete")
	}

	// Unhurried passes resume the interrupted listing (its preserved stream is
	// drained over the following refreshes) and then report complete.
	codexRolloutReadDirChunkHook = originalHook
	for pass := 1; ; pass++ {
		full, cancelFull := context.WithTimeout(context.Background(), codexRolloutScanBudget)
		_, _, _, evidence := codexReconcileFromRollout(full, f.home, f.fp, now)
		cancelFull()
		if !evidence.incomplete {
			t.Logf("complete after %d unhurried pass(es)", pass)
			break
		}
		if pass == 4 {
			t.Fatal("unhurried passes over the same tree never reported themselves complete")
		}
	}
}

// Held PendingRolloutMtimeMs re-runs the reconcile once per period with no file
// change, and stops once the nudge released it. A poll inside the period reads
// no cache file at all.
func TestCodexDirectScan_HeldEvidenceRerunsEveryPeriodUntilCovered(t *testing.T) {
	h := codexDirectFixture(t, codexDirectTestStart)
	codexDirectRunScanIfChanged()
	h.wantReconciles(t, 1, "baseline")
	h.setHeld(true)

	for i := 0; i < 50; i++ {
		h.advance(time.Second)
		codexDirectRunScanIfChanged()
	}
	if n := h.heldReadCount(); n != 0 {
		t.Fatalf("idle polls inside the period read the cache %d times", n)
	}
	h.wantReconciles(t, 1, "inside the period")

	h.advance(cliUsageClaudeFallbackPeriod)
	codexDirectRunScanIfChanged()
	h.wantReconciles(t, 2, "held evidence after the period")
	if n := h.heldReadCount(); n != 1 {
		t.Fatalf("held reads = %d, want 1", n)
	}

	h.setHeld(false)
	h.advance(cliUsageClaudeFallbackPeriod)
	codexDirectRunScanIfChanged()
	h.wantReconciles(t, 2, "released evidence")
	if n := h.heldReadCount(); n != 2 {
		t.Fatalf("held reads = %d, want 2", n)
	}
	h.advance(time.Second)
	codexDirectRunScanIfChanged()
	if n := h.heldReadCount(); n != 2 {
		t.Fatalf("the check is once per period, got %d reads", n)
	}
}

// Every gate refuses on its own; startup ignores detection.
func TestCodexDirectScan_EachGateRefuses(t *testing.T) {
	gates := []struct {
		name  string
		close func(t *testing.T)
	}{
		{"disabled", func(*testing.T) { codexDirectScanEnabled = false }},
		{"refresh disarmed", func(*testing.T) { SetCodexUsageRefreshEnabled(false) }},
		{"shutdown", func(t *testing.T) {
			shutdownInProgress.Store(true)
			t.Cleanup(func() { shutdownInProgress.Store(false) })
		}},
		{"draining", func(t *testing.T) {
			drain.mu.Lock()
			drain.draining = true
			drain.mu.Unlock()
			t.Cleanup(func() { drain.mu.Lock(); drain.draining = false; drain.mu.Unlock() })
		}},
		{"not detected", func(*testing.T) { codexDirectDetected = func() bool { return false } }},
	}
	for _, g := range gates {
		t.Run(g.name, func(t *testing.T) {
			h := codexDirectFixture(t, codexDirectTestStart)
			h.writeRollout(t, h.dayDir(h.clock()), "a", "{}\n", h.clock())
			g.close(t)
			codexDirectRunScanIfChanged()
			h.poll()
			want := 0
			if g.name == "not detected" {
				codexDirectScanAtStartup()
				want = 1
			}
			h.wantReconciles(t, want, g.name)
		})
	}
}

// The startup scan reconciles without a signature change and sets the baseline
// the first poll compares against.
func TestCodexDirectScan_StartupReconcilesAndSetsTheBaseline(t *testing.T) {
	h := codexDirectFixture(t, codexDirectTestStart)
	h.writeRollout(t, h.dayDir(h.clock()), "handoff", "{}\n", h.clock().Add(-time.Minute))
	codexDirectScanAtStartup()
	h.wantReconciles(t, 1, "startup")
	h.poll()
	h.wantReconciles(t, 1, "the first poll over the startup baseline")
}

// With more than four rollouts in a date directory, only the newest four are
// tracked; an append to one of them is seen by its own stat.
func TestCodexDirectScan_TracksOnlyTheNewestFour(t *testing.T) {
	h := codexDirectFixture(t, codexDirectTestStart)
	today := h.dayDir(h.clock())
	var paths []string
	for i, name := range []string{"f0", "f1", "f2", "f3", "f4", "f5"} {
		paths = append(paths, h.writeRollout(t, today, name, "{}\n", h.clock().Add(time.Duration(i-10)*time.Minute)))
	}
	codexDirectRunScanIfChanged()
	codexDirectMu.Lock()
	tracked := slices.Clone(codexDirectState.tracked)
	codexDirectMu.Unlock()
	var got []string
	for _, f := range tracked {
		got = append(got, f.path)
	}
	if want := []string{paths[5], paths[4], paths[3], paths[2]}; !slices.Equal(got, want) {
		t.Fatalf("tracked = %v, want the newest four %v", got, want)
	}

	h.writeRollout(t, today, "f2", "{}\n", h.clock())
	h.poll()
	h.wantReconciles(t, 2, "an append to a tracked rollout")
}

// A poll across local midnight starts watching the new day at once, and a
// rollout tracked from a day that is no longer watched stays tracked until
// newer ones displace it.
func TestCodexDirectScan_MidnightRolloverWatchesTheNewDay(t *testing.T) {
	start := time.Date(2026, 11, 20, 23, 59, 30, 0, codexDirectTestZone)
	h := codexDirectFixture(t, start)
	old := h.writeRollout(t, h.dayDir(start), "late", "{}\n", start.Add(-time.Minute))
	codexDirectRunScanIfChanged()
	h.wantReconciles(t, 1, "baseline")

	h.poll() // 23:59:51 — still the 20th
	h.wantReconciles(t, 1, "before midnight")
	h.advance(time.Minute) // past midnight: the 21st
	codexDirectRunScanIfChanged()
	before := h.reconciles() // the watched set itself moved
	h.writeRollout(t, h.dayDir(h.clock()), "early", "{}\n", h.clock())
	h.poll()
	h.wantReconciles(t, before+1, "a rollout in the new day's directory")

	// Two days on, the 20th is neither local-today, local-yesterday nor
	// UTC-today; only its tracked rollout's own stat can see a `codex resume`.
	h.advance(48 * time.Hour)
	codexDirectRunScanIfChanged()
	before = h.reconciles()
	for _, dir := range codexRolloutWatchDirs(h.f.home, h.clock()) {
		if dir == filepath.Dir(old) {
			t.Fatalf("the 20th must no longer be watched: %v", dir)
		}
	}
	h.writeRollout(t, filepath.Dir(old), "late", "{}\n", h.clock())
	h.poll()
	h.wantReconciles(t, before+1, "an append to a tracked rollout outside the watched days")
}

// A moved CODEX_HOME resets the watch: the reconcile runs against the new home
// only.
func TestCodexDirectScan_MovedHomeResetsWithoutReconcilingTheOldOne(t *testing.T) {
	h := codexDirectFixture(t, codexDirectTestStart)
	oldHome := h.f.home
	codexDirectRunScanIfChanged()
	h.wantReconciles(t, 1, "baseline")

	newHome := t.TempDir()
	t.Setenv("CODEX_HOME", newHome)
	h.writeRollout(t, h.dayDir(h.clock()), "old-home", "{}\n", h.clock())
	h.poll()
	h.wantReconciles(t, 2, "the new home's first poll")
	h.writeRollout(t, h.dayDir(h.clock()), "old-home-again", "{}\n", h.clock())
	h.poll()
	h.wantReconciles(t, 2, "writes under the old home")
	h.mu.Lock()
	bases := slices.Clone(h.bases)
	h.mu.Unlock()
	if bases[1] != newHome || bases[0] != oldHome {
		t.Fatalf("reconciled bases = %v, want [%s %s]", bases, oldHome, newHome)
	}
}

// An account switch between the stat and the reconcile merges nothing under the
// new login: the reconcile's own codexAccountFingerprintAtBase check refuses
// the stale fingerprint.
func TestCodexDirectScan_AccountSwitchMidScanMergesNothingForTheNewLogin(t *testing.T) {
	now := time.Now()
	h := codexDirectFixture(t, now)
	h.f.seedPreRunReading(t, now.Add(-time.Hour), now)
	rollout := now.Add(-time.Minute)
	writeCodexRunRollout(t, h.f.home, "switch", now.Add(-5*time.Minute), rollout, rollout, true,
		[]map[string]any{codexRateLimitFrame(81, 82, now)})
	h.mu.Lock()
	h.reconcile = func(ctx context.Context, base, fp string, at time.Time) bool {
		helperCodexAuthAt(t, base, "other@example.com", now.Add(-time.Second))
		return !codexReconcileAndNudge(ctx, base, fp, at, false).rollouts.incomplete
	}
	h.mu.Unlock()
	codexDirectScanAtStartup()
	waitCodexUsageRefreshIdle(t)

	newFP := currentCodexAccountFingerprint()
	if newFP == h.f.fp {
		t.Fatal("the switch must change the fingerprint")
	}
	for _, m := range codexMetricsFromCache(now, newFP) {
		if m.Consumed != nil && (*m.Consumed == 81 || *m.Consumed == 82) {
			t.Fatalf("the old login's rollout merged under the new one: %+v", m)
		}
	}
}

func TestCodexNewestRolloutFiles_MissingDirIsCompleteAndEmpty(t *testing.T) {
	files, complete := codexNewestRolloutFiles(context.Background(), filepath.Join(t.TempDir(), "none"), 4)
	if len(files) != 0 || !complete {
		t.Fatalf("files = %v complete = %v, want an empty complete listing", files, complete)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, complete := codexNewestRolloutFiles(ctx, t.TempDir(), 4); complete {
		t.Fatal("a cancelled listing must not read as complete")
	}
}

// A non-forced merge that loses the process-local cache lock commits nothing,
// so the pass must not read as settled: otherwise the detector stores the
// signature and the reading it mined never reaches the cache until a gather.
func TestCodexReconcileFromRollout_UncommittedMergeIsNotSettled(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	writeCodexRunRollout(t, f.home, "contended", now.Add(-5*time.Minute), now.Add(-time.Minute), now.Add(-time.Minute), true,
		[]map[string]any{codexRateLimitFrame(41, 42, now.Add(-time.Minute))})

	codexRateLimitMu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), codexRolloutScanBudget)
	_, _, _, evidence := codexReconcileFromRollout(ctx, f.home, f.fp, now)
	cancel()
	codexRateLimitMu.Unlock()
	if !evidence.uncommitted || evidence.settled() {
		t.Fatalf("a merge dropped on lock contention must leave the pass unsettled: %+v", evidence)
	}

	ctx, cancel = context.WithTimeout(context.Background(), codexRolloutScanBudget)
	_, _, _, evidence = codexReconcileFromRollout(ctx, f.home, f.fp, now)
	cancel()
	if !evidence.settled() {
		t.Fatalf("an uncontended retry must settle: %+v", evidence)
	}
}

// More eligible rollouts than codexRolloutScanFileCap: the first pass is
// complete but leaves a backlog, so it must not read as settled until later
// passes have walked every capped batch.
func TestCodexReconcileFromRollout_CappedBacklogIsNotSettledUntilWalked(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	for i := 0; i <= codexRolloutScanFileCap; i++ {
		at := now.Add(-time.Duration(i+1) * time.Minute)
		writeCodexRunRollout(t, f.home, fmt.Sprintf("capped-%02d", i), at.Add(-time.Minute), at, at, true,
			[]map[string]any{codexRateLimitFrame(10, 20, at)})
	}

	ctx, cancel := context.WithTimeout(context.Background(), codexRolloutScanBudget)
	_, _, _, evidence := codexReconcileFromRollout(ctx, f.home, f.fp, now)
	cancel()
	if evidence.incomplete || !evidence.workRemaining || evidence.settled() {
		t.Fatalf("a complete pass that left capped rollouts unread must be unsettled: %+v", evidence)
	}
	for pass := 2; ; pass++ {
		ctx, cancel := context.WithTimeout(context.Background(), codexRolloutScanBudget)
		_, _, _, evidence = codexReconcileFromRollout(ctx, f.home, f.fp, now)
		cancel()
		if evidence.settled() {
			t.Logf("settled after %d passes", pass)
			break
		}
		if pass == 4 {
			t.Fatalf("the capped backlog never drained: %+v", evidence)
		}
	}
}

// A rollout whose open fails transiently stays in the retry set, but its
// recovery moves neither mtime nor size, so the pass must not read as settled
// until a later pass has actually read it.
func TestCodexReconcileFromRollout_TransientReadFailureIsNotSettled(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	writeCodexRunRollout(t, f.home, "steady", now.Add(-6*time.Minute), now.Add(-2*time.Minute), now.Add(-2*time.Minute), true,
		[]map[string]any{codexRateLimitFrame(11, 12, now.Add(-2*time.Minute))})
	writeCodexRunRollout(t, f.home, "locked", now.Add(-5*time.Minute), now.Add(-time.Minute), now.Add(-time.Minute), true,
		[]map[string]any{codexRateLimitFrame(41, 42, now.Add(-time.Minute))})

	originalOpen := codexOpenRolloutFile
	t.Cleanup(func() { codexOpenRolloutFile = originalOpen })
	failedOpens := 0
	codexOpenRolloutFile = func(path string) (*os.File, error) {
		if strings.Contains(filepath.Base(path), "locked") {
			failedOpens++
			return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrPermission}
		}
		return os.Open(path)
	}

	ctx, cancel := context.WithTimeout(context.Background(), codexRolloutScanBudget)
	_, _, _, evidence := codexReconcileFromRollout(ctx, f.home, f.fp, now)
	cancel()
	if failedOpens == 0 {
		t.Fatal("the failing rollout was never attempted")
	}
	if evidence.incomplete || evidence.uncommitted || evidence.workRemaining {
		t.Fatalf("only the read failure should hold the pass open: %+v", evidence)
	}
	if !evidence.readRetry || evidence.settled() {
		t.Fatalf("a transient open failure must leave the pass unsettled: %+v", evidence)
	}

	codexOpenRolloutFile = originalOpen
	ctx, cancel = context.WithTimeout(context.Background(), codexRolloutScanBudget)
	_, _, _, evidence = codexReconcileFromRollout(ctx, f.home, f.fp, now)
	cancel()
	if !evidence.settled() {
		t.Fatalf("once the read recovers the unchanged tree must settle: %+v", evidence)
	}
}

// A future auth.json mtime makes the rollout source unavailable for the pass.
// That must not settle the direct scan: correcting only the auth timestamp
// leaves the watched rollout signature unchanged, so the retry has to come
// from the scan staying unsettled.
func TestCodexReconcileFromRollout_FutureAuthWatermarkIsNotSettled(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	writeCodexRunRollout(t, f.home, "steady", now.Add(-6*time.Minute), now.Add(-2*time.Minute), now.Add(-2*time.Minute), true,
		[]map[string]any{codexRateLimitFrame(11, 12, now.Add(-2*time.Minute))})
	authPath := filepath.Join(f.home, "auth.json")
	info, err := os.Stat(authPath)
	if err != nil {
		t.Fatalf("fixture has no auth.json: %v", err)
	}
	future := now.Add(time.Hour)
	if err := os.Chtimes(authPath, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), codexRolloutScanBudget)
	_, _, _, evidence := codexReconcileFromRollout(ctx, f.home, f.fp, now)
	cancel()
	if !evidence.incomplete || evidence.settled() {
		t.Fatalf("a pass skipped by a future auth watermark must stay unsettled: %+v", evidence)
	}

	if err := os.Chtimes(authPath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), codexRolloutScanBudget)
	_, _, _, evidence = codexReconcileFromRollout(ctx, f.home, f.fp, now)
	cancel()
	if !evidence.settled() {
		t.Fatalf("once the auth timestamp is corrected the unchanged tree must settle: %+v", evidence)
	}
}
