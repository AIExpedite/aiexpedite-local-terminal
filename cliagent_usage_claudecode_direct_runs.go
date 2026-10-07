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
// every *.jsonl in each, and never opens a file. Stat cost is linear in the
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
	lastScanAt            time.Time
	lastEvidenceMs        int64
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
	changed := !s.primed || ownMod != s.ownMod || ownSize != s.ownSize || pinnedMod != s.pinnedMod || pinnedSize != s.pinnedSize
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
	if generation, bumped := claudeStampWatchedGeneration(fingerprint, scopeBefore); bumped {
		noteCLIUsageObservationAdvanced(claudeUsageProvider, generation)
	}
}

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
	newestMs, files, complete := claudeNewestTranscriptMs(home, claudeDirectRunScanBudget)
	took := time.Since(started)
	if !complete {
		logClaudeDirectRun("incomplete", files, took)
		return
	}
	if newestMs <= 0 {
		return
	}
	s := &claudeUsageWatchState
	s.mu.Lock()
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
	// reconsidered once the clock catches up to it.
	if outcome != claudeDirectRunStanding && outcome != claudeDirectRunFuture {
		s.mu.Lock()
		if newestMs > s.lastEvidenceMs {
			s.lastEvidenceMs = newestMs
		}
		s.mu.Unlock()
	}
	logClaudeDirectRun(outcome, files, took)
}

// claudeNewestTranscriptMs walks <claudeConfigDir>/projects one directory
// level deep and stats every *.jsonl in each project, keeping the newest mtime
// (epoch ms) as it goes so directory order does not matter. Mtime is read per
// FILE because on Windows an append updates the file's mtime but not its
// directory's. It never opens a file and never returns a path.
//
// complete is false when the budget ran out: what was seen so far is not
// evidence, because the newest transcript may be in a directory not reached.
// A missing projects directory is a complete scan with nothing in it.
func claudeNewestTranscriptMs(home string, budget time.Duration) (newestMs int64, files int, complete bool) {
	base := claudeConfigDir(home)
	if base == "" {
		return 0, 0, true
	}
	root := filepath.Join(base, "projects")
	projects, err := os.ReadDir(root)
	if err != nil {
		return 0, 0, true
	}
	deadline := time.Now().Add(budget)
	for _, project := range projects {
		if time.Now().After(deadline) {
			return newestMs, files, false
		}
		if !project.IsDir() {
			continue
		}
		dir := filepath.Join(root, project.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if time.Now().After(deadline) {
				return newestMs, files, false
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			files++
			if ms := info.ModTime().UnixMilli(); ms > newestMs {
				newestMs = ms
			}
		}
	}
	return newestMs, files, true
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
	s.ownMod, s.ownSize, s.pinnedMod, s.pinnedSize = 0, 0, 0, 0
	s.lastScanAt = time.Time{}
	s.lastEvidenceMs = 0
}
