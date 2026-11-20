// cliagent_usage_codex_direct.go — finds Codex runs the agent did not arm, so
// their utilization reaches the cache without waiting for a gather.
//
// Why this exists:
//
//	The agent pays utilization debt for every Codex run it ARMS: app-server
//	turns (codex_appserver.go), stdin-fed terminal `codex` sessions
//	(session.go) and `__cli_smoke__` (cliagent_smoke_codex.go). A `codex`
//	started anywhere else — the maintenance controller's own shell, the user's
//	shell or IDE, an agent-hosted PTY or PowerShell session where it is typed by
//	hand — is never armed. Until now the agent learnt of such a run only at a
//	gather (codexUsageParser.ParseContext), and on an Idle device the next one
//	can be hours away, so the card kept a pre-run observedAt while newer
//	telemetry sat on disk: the Windows nightly's cache at 08:15:41Z behind a
//	rollout written at 08:23:52Z.
//
//	The propagator's fallback timer (cliagent_usage_propagate.go) runs
//	codexDirectRunScanIfChanged on every poll. When the rollout tree's stat
//	signature moved, it runs the gather's own reconcile-and-nudge
//	(codexReconcileAndNudge): telemetry in the rollout merges into
//	codex_rate_limits.json, and a rollout newer than the reading with none is
//	booked as run debt and paid by the existing ladder and its live read.
//
// Bounds:
//   - each poll stats at most the three watched date directories
//     (codexRolloutWatchDirs) and the codexDirectTrackedFiles newest rollouts
//     tracked in memory; a directory is listed (codexNewestRolloutFiles) only
//     when its own stat moved. The tree is never walked: a `codex resume`
//     appending to an untracked rollout in an older date directory is found at
//     the next gather, as before;
//   - a reconcile runs only when that signature moved, or the nudge still holds
//     rollout evidence (PendingRolloutMtimeMs, checked at most once per
//     cliUsageClaudeFallbackPeriod), at most once per
//     codexForcedReconcileMinInterval, never while a run of this process is
//     open (that run settles itself), and within codexDirectReconcileBudget;
//   - the signature is stored only after a reconcile that finished inside its
//     budget, reported its discovery and selection complete, committed its
//     merge and left no capped rollouts unread, so a cut-short, uncommitted,
//     capped, spaced-out or guarded pass is retried by the next eligible
//     poll. State is in memory only: the owed rule lives in the
//     reconcile's cursor and the nudge's held evidence, both on disk.
//
// It runs offline: the reconcile is local file I/O, and the live-read fallback
// and the usage hint are offline-gated by their own code.
//
// Redaction: the detector reads directory entries, file names, mtimes and
// sizes only. Rollout content is parsed only inside the existing reconcile,
// which keeps numeric windows. The one log label is fixed.
package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"
)

// codexDirectTrackedFiles bounds the rollouts whose own stat each poll checks:
// an append to a file already listed moves no directory mtime.
const codexDirectTrackedFiles = 4

var (
	// codexDirectScanEnabled gates every trigger. TestMain turns it off, so no
	// test reads the developer's own ~/.codex/sessions.
	codexDirectScanEnabled = true
	// codexDirectDetected reports whether the last machine-info gather detected
	// Codex; it gates the poll only.
	codexDirectDetected = func() bool {
		info := GetMachineInfo()
		return info != nil && info.DetectedCliAgents[codexUsageProvider].Detected
	}
	// codexDirectReconcile is the reconcile a scan runs (a seam for tests). It
	// reports whether the pass settled (codexRolloutNudgeEvidence.settled): it
	// saw the whole tree — discovery and selection run under their own
	// sub-budgets, which can expire while ctx is still live — its merge
	// committed, and no capped rollouts remain for a later batch.
	codexDirectReconcile = func(ctx context.Context, base, fp string, now time.Time) bool {
		return codexReconcileAndNudge(ctx, base, fp, now, false).rollouts.settled()
	}
	// codexDirectHeldEvidence reports whether the nudge still holds rollout
	// evidence for fp — one cache read (a seam for tests).
	codexDirectHeldEvidence = func(fp string) bool {
		return codexCacheViewForAccount(fp).pendingRolloutMs > 0
	}
	// codexDirectReconcileBudget bounds one scan's reconcile.
	codexDirectReconcileBudget = codexRolloutScanBudget
	// codexDirectListBudget bounds one date directory's listing.
	codexDirectListBudget = time.Second
)

// codexDirectWatch is what the polls know of the rollout tree under one
// CODEX_HOME.
type codexDirectWatch struct {
	base string
	// listed is each watched directory's stat when it was last listed
	// completely; a directory whose stat differs is listed again.
	listed map[string]codexRolloutFileStat
	// tracked is the newest rollouts seen, newest first, at most
	// codexDirectTrackedFiles. Kept across a date rollover until newer ones
	// displace them.
	tracked []codexRolloutFileStat
	// stored is the signature at the last reconcile that finished in budget;
	// storedSet is false before the first one.
	stored    []codexRolloutFileStat
	storedSet bool
	// lastReconcileAt spaces reconciles (codexForcedReconcileMinInterval).
	lastReconcileAt time.Time
	// heldCheckedAt is when an unmoved poll last read the cache for held
	// evidence (or a reconcile last ran).
	heldCheckedAt time.Time
}

var (
	codexDirectMu    sync.Mutex
	codexDirectState codexDirectWatch
)

// codexDirectScanAtStartup runs one reconcile as the propagator starts, with no
// signature change needed: a run that finished while the agent was down or
// mid-update is found at once. Detection is not required (the machine-info
// gather may not have run yet). It still honours the open-run guard and sets
// the baseline signature the first poll compares against.
func codexDirectScanAtStartup() {
	codexDirectScan(true)
}

// codexDirectRunScanIfChanged is the propagator poll's scan.
func codexDirectRunScanIfChanged() {
	codexDirectScan(false)
}

func codexDirectScan(atStartup bool) {
	if !codexDirectScanEnabled || !codexUsageRefresh.isEnabled() || IsShutdownInProgress() || isDraining() {
		return
	}
	if !atStartup && !codexDirectDetected() {
		return
	}
	now := codexUsageFreshnessNow()
	base := codexHomeBase()
	codexDirectMu.Lock()
	w := &codexDirectState
	if w.base != base {
		// A moved CODEX_HOME: nothing seen under the old home says anything
		// about the new one.
		*w = codexDirectWatch{base: base}
	}
	if base == "" {
		codexDirectMu.Unlock()
		return
	}
	sig := codexDirectSignatureLocked(now)
	due := atStartup || !w.storedSet || !slices.Equal(sig, w.stored)
	fp, fpRead := "", false
	if !due && (w.heldCheckedAt.IsZero() || now.Before(w.heldCheckedAt) ||
		now.Sub(w.heldCheckedAt) >= cliUsageClaudeFallbackPeriod) {
		// Nothing moved, but a nudge declined by its settle guard or cooldown —
		// or a non-forced merge dropped on lock contention — leaves evidence
		// only another reconcile-and-nudge pays.
		w.heldCheckedAt = now
		fp, fpRead = codexAccountFingerprintAtBase(base), true
		due = codexDirectHeldEvidence(fp)
	}
	if !due {
		codexDirectMu.Unlock()
		return
	}
	if !w.lastReconcileAt.IsZero() && !now.Before(w.lastReconcileAt) &&
		now.Sub(w.lastReconcileAt) < codexForcedReconcileMinInterval {
		codexDirectMu.Unlock()
		return
	}
	// A run of this process that is still writing its rollout settles itself
	// when it ends; reconciling under it would only rescan the same file on
	// every poll.
	if codexNewestOpenRunFloor() != 0 {
		codexDirectMu.Unlock()
		return
	}
	w.lastReconcileAt, w.heldCheckedAt = now, now
	codexDirectMu.Unlock()

	if !fpRead {
		fp = codexAccountFingerprintAtBase(base)
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexDirectReconcileBudget)
	// Finished means the reconcile itself says it saw everything AND its budget
	// held: a live parent context alone is no proof, because discovery's
	// reserved sub-budget can run out first and the pass then withholds its
	// evidence. Storing the signature then would leave a completed run that
	// writes nothing more unfound until the next gather.
	complete := codexDirectReconcile(ctx, base, fp, now)
	finished := complete && ctx.Err() == nil
	cancel()
	if !finished {
		return
	}
	codexDirectMu.Lock()
	if w.base == base {
		w.stored, w.storedSet = sig, true
	}
	codexDirectMu.Unlock()
	fmt.Printf("%s[cli-usage] codex direct run: reconciled%s\n", colorCyan, colorReset)
}

// codexDirectSignatureLocked stats the watched date directories and the
// tracked rollouts, re-lists a directory whose stat moved, and returns the
// signature: each watched directory's stat, then each tracked rollout's.
// Caller holds codexDirectMu.
func codexDirectSignatureLocked(now time.Time) []codexRolloutFileStat {
	w := &codexDirectState
	dirs := codexRolloutWatchDirs(w.base, now)
	tracked := w.tracked[:0:0]
	for _, file := range w.tracked {
		if stat, ok := codexDirectStat(file.path); ok {
			tracked = append(tracked, stat)
		}
	}
	slices.SortFunc(tracked, codexRolloutFileNewestFirst)
	sig := make([]codexRolloutFileStat, 0, len(dirs)+codexDirectTrackedFiles)
	listed := make(map[string]codexRolloutFileStat, len(dirs))
	for _, dir := range dirs {
		stat, ok := codexDirectStat(dir)
		if !ok {
			// Not created yet: recorded as absent so its creation moves the
			// signature.
			stat = codexRolloutFileStat{path: dir, size: -1}
		}
		sig = append(sig, stat)
		if prev, seen := w.listed[dir]; seen && prev == stat {
			listed[dir] = prev
			continue
		}
		if !ok {
			listed[dir] = stat
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), codexDirectListBudget)
		files, complete := codexNewestRolloutFiles(ctx, dir, codexDirectTrackedFiles)
		cancel()
		for _, file := range files {
			tracked = codexInsertNewestRolloutFile(tracked, file, codexDirectTrackedFiles)
		}
		if complete {
			listed[dir] = stat
		}
	}
	// Only the directories watched now are remembered, so the map stays bounded
	// across date rollovers.
	w.listed, w.tracked = listed, tracked
	return append(sig, tracked...)
}

// codexDirectStat stats path into a signature entry.
func codexDirectStat(path string) (codexRolloutFileStat, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return codexRolloutFileStat{}, false
	}
	return codexRolloutFileStat{path: path, mtimeNs: info.ModTime().UnixNano(), size: info.Size()}, true
}

// resetCodexDirectScan forgets the watch (tests, propagator reset).
func resetCodexDirectScan() {
	codexDirectMu.Lock()
	codexDirectState = codexDirectWatch{}
	codexDirectMu.Unlock()
}
