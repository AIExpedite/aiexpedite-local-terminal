// File: orphanScanner.go
// -----------------------------------------------------------------------------
// Periodic scanner that detects and (optionally) kills orphaned CLI processes.
//
// Background: when an interactive CLI session (claude/codex) is started
// via session.go and the parent terminal session is torn down or times out,
// the spawned CLI process can keep running detached — autonomously consuming
// AI credits and disk while the user has no UI control over it. This scanner
// catches those orphans by comparing the OS process table against the central
// processRegistry of PIDs this agent knows about.
//
// Safety guardrails (in addition to the strict allowlist enforced by the
// platform-specific ScanCLIProcesses):
//
//  1. Skip processes that started before this Go agent (we have no record of
//     them and shouldn't assume ownership).
//  2. 30-second grace period for newly-spawned processes (give the registering
//     code time to call processRegistry.Register).
//  3. Two-strikes rule: a process must be flagged orphan in two consecutive
//     scans before we kill it (eliminates race conditions during session
//     startup).
//  4. Skip if PID is in the registry (backed by an active spawn).
//  5. Skip if PID's parent is in the registry (descendant of an active spawn).
//  6. Self-protection: never kill the agent's own PID or its children.
//  7. Bounded candidate map (capped at 1000 entries) so a runaway process
//     table can't blow up memory.
//  8. Mode selector: dryrun (default), enforce, disabled.
//  9. Configurable scan interval via ORPHAN_SCAN_INTERVAL_SECONDS.
//
// All actions log at WARN-equivalent visibility for auditability.
// -----------------------------------------------------------------------------

package main

import (
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// Cleanup mode — set via ORPHAN_CLEANUP_MODE env var.
const (
	cleanupModeDryRun   = "dryrun"   // log only, take no action (default)
	cleanupModeEnforce  = "enforce"  // log + kill orphans
	cleanupModeDisabled = "disabled" // halt the scanner entirely
)

// Defaults / bounds for the scan loop.
const (
	defaultScanIntervalSeconds = 15
	minScanIntervalSeconds     = 5
	maxScanIntervalSeconds     = 300

	// orphanGracePeriod gives a brand-new process time to be registered by
	// the spawning code before we consider it for kill. Longer than typical
	// session-startup latency (which is sub-second).
	orphanGracePeriod = 30 * time.Second

	// orphanStrikesRequired is the number of consecutive scans a PID must
	// appear orphaned before we kill it. With 15s scans + 2 strikes, an
	// orphan dies within ~30–45s of becoming truly unbacked.
	orphanStrikesRequired = 2

	// candidateMapCap is a safety net so runaway process tables can't grow
	// the candidate map without bound.
	candidateMapCap = 1000

	// pidReuseSlack is the maximum delta between a process's OS start time
	// and the time we registered its PID before we consider the registration
	// stale (i.e. the PID has been reused by an unrelated later process).
	// Normal flow registers within microseconds of process creation; 1 minute
	// is comfortably above any plausible Register() latency and well below
	// typical session lifetimes.
	pidReuseSlack = 1 * time.Minute
)

// orphanScannerStarted prevents the goroutine from being launched twice if
// StartAgent is somehow re-entered (defensive — no known caller does this).
var orphanScannerStarted atomic.Bool

// StartOrphanScanner launches the background goroutine. Safe to call once at
// agent startup. Returns immediately — the scan loop runs forever (until the
// process exits).
func StartOrphanScanner(cfg *Config) {
	if !orphanScannerStarted.CompareAndSwap(false, true) {
		return
	}

	mode := readCleanupMode()
	interval := readScanInterval()

	if mode == cleanupModeDisabled {
		fmt.Printf("%s[orphan-scanner] Disabled via ORPHAN_CLEANUP_MODE=disabled%s\n",
			colorYellow, colorReset)
		return
	}

	fmt.Printf("%s[orphan-scanner] Starting in %s mode (interval=%ds, app PID=%d, app start=%s)%s\n",
		colorCyan, mode, int(interval.Seconds()), SelfPID(),
		AppStartTime.Format(time.RFC3339), colorReset)

	go runOrphanScanner(mode, interval)
}

// runOrphanScanner is the scan loop. Exposed-as-unexported for clarity.
//
// Threading model: this function runs in exactly one goroutine for the lifetime
// of the process. The `candidates` map is accessed only from within this
// goroutine (via scanOnce), so it does not require a mutex. If concurrent
// scanning is ever introduced, `candidates` must be wrapped in a sync.Mutex.
func runOrphanScanner(mode string, interval time.Duration) {
	candidates := make(map[int]int) // PID → consecutive strike count

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-shutdownChan:
			return
		case <-ticker.C:
			scanOnce(mode, candidates)
		}
	}
}

// scanOnce performs one detection pass. Exposed for unit testability.
func scanOnce(mode string, candidates map[int]int) {
	procs := ScanCLIProcesses()
	if len(procs) == 0 {
		// Nothing to do — clear stale candidate entries and return.
		clear(candidates)
		return
	}

	// Snapshot the registry with registration times so we can detect PID
	// reuse (a parent PID registered AFTER the child's OS start time means
	// the "parent" is a new process — the child should NOT be protected
	// under the descendant rule).
	registered := globalProcessRegistry.GetSnapshot()

	now := time.Now()
	selfPID := SelfPID()
	seenThisScan := make(map[int]struct{}, len(procs))

	for _, p := range procs {
		seenThisScan[p.PID] = struct{}{}

		// Guardrail 6: never touch ourselves
		if p.PID == selfPID {
			continue
		}

		// Guardrail 0: if we couldn't determine the start time, conservatively
		// skip this process. Both the predate check and grace period rely on
		// StartTime; without it we'd bypass both guards and potentially kill a
		// legitimate brand-new process. Better a missed orphan than a false kill.
		if p.StartTime.IsZero() {
			continue
		}

		// Guardrail 1: skip processes that predate this agent
		if p.StartTime.Before(AppStartTime) {
			continue
		}

		// Guardrail 2: grace period for very new processes
		if now.Sub(p.StartTime) < orphanGracePeriod {
			continue
		}

		// Guardrail 4: backed by an active session/command — but guard against
		// PID reuse. Normal flow registers within microseconds of the OS
		// creating the process, so p.StartTime and regTime should be within
		// pidReuseSlack of each other. If the process is substantially NEWER
		// than the registration, the registered entry is stale (original
		// process died without Deregister) and this PID belongs to an
		// unrelated later process — don't protect it.
		if regTime, ok := registered[p.PID]; ok && p.StartTime.Sub(regTime) < pidReuseSlack {
			continue
		}

		// Guardrail 5: descendant of a registered PID — same PID-reuse guard
		// applied to the parent relationship.
		if regTime, ok := registered[p.ParentPID]; ok && p.StartTime.Sub(regTime) < pidReuseSlack {
			continue
		}

		// Guardrail 6 (extended): descendant of the agent itself
		if p.ParentPID == selfPID {
			continue
		}

		// CANDIDATE — bump the strike counter.
		candidates[p.PID]++

		// Guardrail 7: don't let the candidate map grow unbounded.
		if len(candidates) > candidateMapCap {
			// Drop ALL candidates if we exceed the cap; next scan will
			// re-populate. Safer than a partial eviction that could let
			// stale state linger.
			fmt.Printf("%s[orphan-scanner] Candidate map exceeded cap (%d) — clearing%s\n",
				colorYellow, candidateMapCap, colorReset)
			clear(candidates)
			return
		}

		// Guardrail 3: two-strikes rule
		if candidates[p.PID] >= orphanStrikesRequired {
			// Only remove from tracking on confirmed success. On dry-run we
			// also remove (intent was logged). On enforce-with-failure we
			// keep the entry so the next scan retries.
			if handleOrphanKill(mode, p) {
				delete(candidates, p.PID)
			}
		} else {
			// %q around process name to neutralize any control/ANSI characters
			// an adversarial rename could smuggle into log output.
			fmt.Printf("%s[orphan-scanner] Tentative orphan: name=%q pid=%d ppid=%d age=%s strikes=%d/%d%s\n",
				colorYellow, p.Name, p.PID, p.ParentPID,
				now.Sub(p.StartTime).Round(time.Second), candidates[p.PID], orphanStrikesRequired,
				colorReset)
		}
	}

	// Clear strike counts for PIDs no longer in the process table (they
	// either exited on their own or someone else killed them).
	for pid := range candidates {
		if _, ok := seenThisScan[pid]; !ok {
			delete(candidates, pid)
		}
	}
}

// handleOrphanKill emits the audit log and, in enforce mode, kills the tree.
// Returns true when the candidate should be removed from tracking (dry-run
// always removes since intent was recorded; enforce removes only on successful
// kill so a failed kill can be retried on the next scan cycle).
func handleOrphanKill(mode string, p ProcessInfo) bool {
	age := time.Since(p.StartTime).Round(time.Second)

	// %q around process name to neutralize any control/ANSI characters
	// an adversarial rename could smuggle into log output.
	if mode == cleanupModeDryRun {
		fmt.Printf("%s[orphan-scanner] WOULD KILL orphan: name=%q pid=%d ppid=%d age=%s (dry-run)%s\n",
			colorYellow, p.Name, p.PID, p.ParentPID, age, colorReset)
		return true
	}

	// enforce mode
	fmt.Printf("%s[orphan-scanner] KILLING orphan: name=%q pid=%d ppid=%d age=%s%s\n",
		colorRed, p.Name, p.PID, p.ParentPID, age, colorReset)

	if err := KillProcessTree(p.PID); err != nil {
		fmt.Printf("%s[orphan-scanner] Kill failed for pid=%d: %v (will retry next scan)%s\n",
			colorRed, p.PID, err, colorReset)
		return false
	}
	fmt.Printf("%s[orphan-scanner] Killed orphan tree pid=%d name=%q%s\n",
		colorGreen, p.PID, p.Name, colorReset)
	return true
}

// readCleanupMode parses ORPHAN_CLEANUP_MODE; defaults to dryrun.
func readCleanupMode() string {
	v := os.Getenv("ORPHAN_CLEANUP_MODE")
	switch v {
	case cleanupModeEnforce, cleanupModeDryRun, cleanupModeDisabled:
		return v
	case "":
		return cleanupModeDryRun
	default:
		fmt.Printf("%s[orphan-scanner] Unknown ORPHAN_CLEANUP_MODE=%q — defaulting to dryrun%s\n",
			colorYellow, v, colorReset)
		return cleanupModeDryRun
	}
}

// readScanInterval parses ORPHAN_SCAN_INTERVAL_SECONDS; clamps to [5, 300]s.
func readScanInterval() time.Duration {
	v := os.Getenv("ORPHAN_SCAN_INTERVAL_SECONDS")
	if v == "" {
		return time.Duration(defaultScanIntervalSeconds) * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < minScanIntervalSeconds {
		n = defaultScanIntervalSeconds
	}
	if n > maxScanIntervalSeconds {
		n = maxScanIntervalSeconds
	}
	return time.Duration(n) * time.Second
}
