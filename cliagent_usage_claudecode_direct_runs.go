// cliagent_usage_claudecode_direct_runs.go — the usage watcher's Claude work:
// stamping readings written outside this process, and owing a refresh for
// `claude` runs the agent did not start.
//
// Why this exists:
//
//	Debts are created for the agent's own sessions (session.go,
//	claude_native.go) and the smoke. A `claude -p` the user runs in their own
//	shell leaves nothing behind that the agent sees: a headless run renders no
//	status line, and an under-quota run emits only heartbeats with no numbers.
//	Its one durable trace is the transcript Claude Code appends under
//	<claudeConfigDir>/projects/<project>/<session>.jsonl.
//
// The watcher (cliagent_usage_propagate.go) calls claudeUsageWatchTick every
// 60 s. Each tick:
//
//   - stats both cache paths (this channel's and the installed hook's pinned
//     one) and, only when either moved, stamps the generation — the status-line
//     hook is a separate process and never advances the counter itself;
//   - every claudeDirectRunScanPeriod, while requests are allowed (not offline
//     or draining) and the probe is armed, stats the transcripts under a time
//     budget and owes a refresh for a run newer than the reading
//     (claudeOweDirectRunRefresh). A scan that runs out of budget is UNKNOWN —
//     it neither owes nor counts as covered, and the next due scan tries again.
//
// Cost: the transcript scan is stat-only, one directory level of projects and
// every *.jsonl in each, and never opens a transcript. Stat cost is linear in the
// file count, so on a device with tens of thousands of transcripts on a slow
// disk the budget bites and those runs stay uncovered until the user prunes
// old transcripts. A run launched with `--no-session-persistence` writes no
// transcript and is never covered.
//
// REDACTION: project directory names encode the user's working directory.
// Nothing here returns, logs or persists a path, a project name or a session
// id — log lines are a fixed label plus counters.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Vars so tests can pin them small.
var (
	claudeDirectRunScanPeriod = 5 * time.Minute
	claudeDirectRunScanBudget = 2 * time.Second
)

// claudeUsageWatchState is the watcher's process-local memory: the cache stamps
// the last tick saw, when the transcript scan last ran, and the newest evidence
// it already acted on. None of it needs persisting — a missed tick or scan is
// simply retried.
var claudeUsageWatchState struct {
	mu                    sync.Mutex
	primed                bool
	ownMod, ownSize       int64
	pinnedMod, pinnedSize int64
	// stampRetries counts ticks that re-attempt a refused stamp (an empty
	// fingerprint from a transient credential-read failure, or busy cache
	// locks) for files whose stamps did not move since.
	stampRetries   int
	lastScanAt     time.Time
	lastEvidenceMs int64
	// lastEvidenceAtMs is the wall clock read when lastEvidenceMs was recorded.
	// Both are wall-clock readings, so a backward clock step is detectable:
	// without it the mark would suppress every later run (claudeScanDirectRuns).
	lastEvidenceAtMs int64
}

// claudeUsageWatchTick is the watcher's per-tick Claude work (see the header).
// It takes no propagator lock: the stamp runs a cache transaction, and the
// propagator is noted only after it returns.
func claudeUsageWatchTick(now time.Time, mayRequest bool) {
	claudeWatchStampCheck()
	if mayRequest && claudeUsageProbe.armedForProbe() && claudeDirectRunScanDue(now) {
		claudeScanDirectRuns(now)
	}
}

// claudeWatchStampCheck stamps the generation when either cache file changed
// since the last tick, catching writes made outside this process. Recorded
// BEFORE the stamp, so a write landing during it is seen next tick; the
// stamp's own write is seen too, and costs one load that finds nothing new.
func claudeWatchStampCheck() {
	home, _ := os.UserHomeDir()
	ownMod, ownSize := claudeCacheFileStamp(claudeRateLimitCachePath())
	var pinnedMod, pinnedSize int64
	if pinned := installedClaudeRateLimitCachePath(home); pinned != "" {
		pinnedMod, pinnedSize = claudeCacheFileStamp(pinned)
	}
	s := &claudeUsageWatchState
	s.mu.Lock()
	moved := !s.primed || ownMod != s.ownMod || ownSize != s.ownSize || pinnedMod != s.pinnedMod || pinnedSize != s.pinnedSize
	if moved {
		s.stampRetries = 0
	}
	changed := moved || s.stampRetries > 0
	s.primed = true
	s.ownMod, s.ownSize, s.pinnedMod, s.pinnedSize = ownMod, ownSize, pinnedMod, pinnedSize
	s.mu.Unlock()
	if !changed || (ownMod == 0 && pinnedMod == 0) {
		return
	}
	// Resolved only when a file moved: on macOS a credential read can spawn
	// `security`, which a once-a-minute tick must not pay for nothing. The scope
	// is sampled BEFORE it, so only the account our cache held then may be
	// moved off (claudeStampWatchedGeneration).
	scopeBefore := claudeRateLimitCacheScope()
	fingerprint := currentClaudeAccountFingerprint()
	generation, bumped, refused := claudeStampWatchedGeneration(fingerprint, scopeBefore)
	// A refused stamp stays pending: the file stamps above already describe
	// this write, so an unchanged file would otherwise never be looked at
	// again. A write racing the attempt moves the stamps, which resets the
	// count and is retried as a change anyway. Bounded, so a refusal that
	// persists (a cache another account owns) stops paying a credential read
	// every tick.
	s.mu.Lock()
	if refused && s.stampRetries < claudeWatchStampMaxRetries {
		s.stampRetries++
	} else {
		s.stampRetries = 0
	}
	s.mu.Unlock()
	if bumped {
		noteCLIUsageObservationAdvanced(claudeUsageProvider, generation)
	}
}

// claudeWatchStampMaxRetries bounds how many later ticks re-attempt a refused
// watcher stamp for cache files that have not moved since.
const claudeWatchStampMaxRetries = 5

// claudeDirectRunScanDue reports whether the transcript scan is due, and marks
// it as run. A skipped scan (offline, draining, unarmed) never reaches here, so
// it does not move the throttle.
func claudeDirectRunScanDue(now time.Time) bool {
	s := &claudeUsageWatchState
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.lastScanAt.IsZero() && now.Sub(s.lastScanAt) < claudeDirectRunScanPeriod {
		return false
	}
	s.lastScanAt = now
	return true
}

// claudeScanDirectRuns runs one bounded scan and owes a refresh for evidence it
// has not acted on yet.
func claudeScanDirectRuns(now time.Time) {
	home, _ := os.UserHomeDir()
	started := time.Now()
	// Transcripts stamped past the skew ceiling are skipped, not selected: one
	// future-dated file (a clock step, synced metadata) must not mask every real
	// run behind it. Each scan restats, so it is reconsidered once the clock
	// catches up to it.
	ceilingMs := now.Add(claudeRefreshOwedLocalSkew).UnixMilli()
	newestMs, files, complete := claudeNewestTranscriptMs(home, claudeDirectRunScanBudget, ceilingMs)
	took := time.Since(started)
	if !complete {
		logClaudeDirectRun("incomplete", files, took)
		return
	}
	if newestMs <= 0 {
		return
	}
	s := &claudeUsageWatchState
	nowMs := now.UnixMilli()
	s.mu.Lock()
	// A backward clock step (an NTP correction, a resumed suspend) leaves the
	// mark ahead of the clock that stamps new transcripts, so every later run
	// would read as already seen for the life of the process. Forget the mark
	// instead: the owe rule still gates what the next scan does (coverage, a
	// standing debt, the quiet window, the age-out), so no budget is refilled.
	if s.lastEvidenceMs > 0 && nowMs < s.lastEvidenceAtMs-claudeRefreshOwedLocalSkew.Milliseconds() {
		s.lastEvidenceMs, s.lastEvidenceAtMs = 0, 0
	}
	seen := newestMs <= s.lastEvidenceMs
	s.mu.Unlock()
	if seen {
		return // nothing newer than the evidence already acted on
	}
	outcome := claudeOweDirectRunRefresh(time.UnixMilli(newestMs), now)
	if outcome == claudeDirectRunBusy {
		return // the cache refused the write: retried at the next scan
	}
	// A standing debt only defers the evidence: its probe may cover just its
	// own baseline, so the newer run is reconsidered once that debt settles.
	// Future-dated evidence (a clock correction, synced metadata) is likewise
	// reconsidered once the clock catches up to it, and evidence the quiet
	// window deferred once that window ends while it is still inside the age
	// limit (the age-out still bounds it, so no budget is refilled).
	if outcome != claudeDirectRunStanding && outcome != claudeDirectRunFuture && outcome != claudeDirectRunQuiet {
		s.mu.Lock()
		if newestMs > s.lastEvidenceMs {
			s.lastEvidenceMs, s.lastEvidenceAtMs = newestMs, nowMs
		}
		s.mu.Unlock()
	}
	logClaudeDirectRun(outcome, files, took)
}

// claudeNewestTranscriptMs walks <claudeConfigDir>/projects one directory
// level deep and stats every *.jsonl in each project, keeping the newest mtime
// (epoch ms) not beyond ceilingMs as it goes, so directory order does not
// matter and a future-dated file never hides the rest. Mtime is read per
// FILE because on Windows an append updates the file's mtime but not its
// directory's. It never opens a transcript and never returns a path.
//
// complete is false when the budget ran out: what was seen so far is not
// evidence, because the newest transcript may be in a directory not reached.
// Both levels are read in batches (claudeWalkTranscriptDir), so the budget
// bounds a single very full project directory too.
// A missing projects directory is a complete scan with nothing in it.
func claudeNewestTranscriptMs(home string, budget time.Duration, ceilingMs int64) (newestMs int64, files int, complete bool) {
	base := claudeConfigDir(home)
	if base == "" {
		return 0, 0, true
	}
	root := filepath.Join(base, "projects")
	deadline := time.Now().Add(budget)
	var projects []string
	rootDone, err := claudeWalkTranscriptDir(root, deadline, func(entry os.DirEntry) {
		if entry.IsDir() {
			projects = append(projects, entry.Name())
		}
	})
	if err != nil {
		return 0, 0, true // no projects directory: a complete scan with nothing in it
	}
	if !rootDone {
		return newestMs, files, false
	}
	for _, project := range projects {
		dirDone, err := claudeWalkTranscriptDir(filepath.Join(root, project), deadline, func(entry os.DirEntry) {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				return
			}
			info, err := entry.Info()
			if err != nil {
				return
			}
			files++
			if ms := info.ModTime().UnixMilli(); ms > newestMs && ms <= ceilingMs {
				newestMs = ms
			}
		})
		if err != nil {
			continue
		}
		if !dirDone {
			return newestMs, files, false
		}
	}
	return newestMs, files, true
}

// claudeTranscriptScanBatch is how many directory entries one read takes. Small
// enough that the deadline is checked often on a slow or very full directory,
// large enough to keep the syscall count down on an ordinary one.
const claudeTranscriptScanBatch = 128

// claudeWalkTranscriptDir calls visit for every entry in dir, reading the
// directory in bounded batches and checking the deadline between reads: a
// single read of a whole directory would allocate every entry before the next
// check, so one project with tens of thousands of transcripts, or a slow
// filesystem, could block the watcher goroutine well past the scan budget.
//
// It reports whether the directory was walked to its end — false means the
// deadline passed, which makes the scan incomplete and therefore not evidence.
// The error is the open failure only (a missing directory), never a path leak
// beyond what the caller already holds.
func claudeWalkTranscriptDir(dir string, deadline time.Time, visit func(os.DirEntry)) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer f.Close()
	for {
		if time.Now().After(deadline) {
			return false, nil
		}
		entries, err := f.ReadDir(claudeTranscriptScanBatch)
		for _, entry := range entries {
			visit(entry)
		}
		// io.EOF ends the walk; any other read error is treated the same way,
		// because a partial directory is all this scan can see of it.
		if err != nil || len(entries) == 0 {
			return true, nil
		}
	}
}

// logClaudeDirectRun is the scan's one log line: a fixed label and counters.
func logClaudeDirectRun(label string, files int, took time.Duration) {
	fmt.Printf("%s[claude-usage] direct-run evidence: %s files=%d took_ms=%d%s\n",
		colorCyan, label, files, took.Milliseconds(), colorReset)
}

// resetClaudeUsageWatchState clears the watcher's memory (tests).
func resetClaudeUsageWatchState() {
	s := &claudeUsageWatchState
	s.mu.Lock()
	defer s.mu.Unlock()
	s.primed = false
	s.stampRetries = 0
	s.ownMod, s.ownSize, s.pinnedMod, s.pinnedSize = 0, 0, 0, 0
	s.lastScanAt = time.Time{}
	s.lastEvidenceMs, s.lastEvidenceAtMs = 0, 0
}
