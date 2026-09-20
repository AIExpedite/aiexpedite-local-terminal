// cliagent_usage_claudecode_pending_run.go — the DURABLE half of the post-run
// utilization debt.
//
// claudeUsageProbeGate holds "a Claude turn finished at T, and no observation
// at/after T exists yet" in process memory (owedBaseline). That is enough while
// the agent keeps running, and wrong for the one window where the freshness
// question matters most: the CLI-maintenance flow runs a smoke, updates the CLI,
// and the agent may SELF-REPLACE in between. A restart drops the debt, the
// follow-up refreshes then find a cache the staleness TTL still calls fresh, and
// the CLI Agents card keeps its pre-smoke observedAt — the exact field report
// this file exists to close (three Claude rows stale after a passing smoke, a
// 90s refresh and a 20s retry).
//
// So the coalesced baseline is written next to the rate-limit cache, in the same
// idiom that cache uses: one small JSON scalar per device, atomic tmp+rename,
// 0600. On the next process's first gather it is hydrated ONCE, recorded back on
// the gate, and paid by the ordinary settlement machinery — no new probe path,
// no new bounds.
//
// Retention: NUMERIC fields plus the account FINGERPRINT (already a hash — see
// fingerprintAccount), mirroring claudeRateLimitSnapshot. No token, no path, no
// email, no config fragment. Everything here is best-effort and silent: a device
// with a read-only data dir keeps exactly the behaviour it had before.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// claudeUsagePendingRunSchema is the on-disk shape version. An unknown value
	// is DISCARDED rather than coerced: the record is a pure optimisation, and a
	// misread baseline would either spend a needless OAuth request or suppress a
	// real one.
	claudeUsagePendingRunSchema = 1

	// claudeUsagePendingRunEnv overrides the record's location — the seam
	// AIEXPEDITE_CLAUDE_RL_CACHE already provides for the rate-limit cache, for
	// the same two reasons: tests must never touch the real device record, and a
	// read-only data dir can be relocated.
	claudeUsagePendingRunEnv = "AIEXPEDITE_CLAUDE_PENDING_RUN"

	// claudeUsagePendingRunCoalesce is how far a new baseline must advance past
	// the one already on disk before it is worth another write. A chatty session
	// settles a turn every few seconds, and the record exists only to survive a
	// restart — re-writing it for a baseline that moved 200ms buys nothing and
	// costs a file write per turn. The cost of the skip is bounded by this same
	// value: after a restart the hydrated debt can be up to one interval older
	// than the newest turn, which only ever makes the debt EASIER to settle.
	claudeUsagePendingRunCoalesce = time.Second
)

// claudeUsagePendingRunMaxAge is how long a persisted debt stays payable.
//
// Beyond it the run belongs to a session nobody is still looking at, and paying
// it would spend an OAuth request to refresh a card the user will refresh
// themselves the moment they open it. Twelve hours covers "closed the laptop
// after work, opened it the next morning" without carrying a debt across days.
const claudeUsagePendingRunMaxAge = 12 * time.Hour

// claudeUsagePendingRun is the record. Every field is a number except the
// fingerprint, which is already a hash.
type claudeUsagePendingRun struct {
	SchemaVersion      int    `json:"schemaVersion"`
	AccountFingerprint string `json:"accountFingerprint,omitempty"`
	OwedObservedAtMs   int64  `json:"owedObservedAtMs"`
	RecordedAtMs       int64  `json:"recordedAtMs"`
}

// claudeUsagePendingRunState serialises the record's writers against its
// clearers and remembers what this process already wrote.
//
//   - settled is the newest baseline this process has PAID. A write for a
//     baseline at or before it is refused, so a persist racing a settlement
//     cannot resurrect a debt that is already discharged — the one ordering that
//     would leave a permanent debt on disk.
//   - persisted is the newest baseline actually on disk, driving the coalesce.
//   - hydrated latches the once-per-process read.
var claudeUsagePendingRunState struct {
	mu        sync.Mutex
	settled   time.Time
	persisted time.Time
	hydrated  bool
}

// claudeUsagePendingRunPath is the record's location inside the agent's data
// dir, beside claude_rate_limits.json.
func claudeUsagePendingRunPath() string {
	if p := os.Getenv(claudeUsagePendingRunEnv); p != "" {
		return p
	}
	return filepath.Join(GetConfigDir(), "claude_usage_pending_run.json")
}

// claudeUsageRecordPendingRun persists the coalesced post-run debt.
//
// Called from the post-run goroutine, never from the synchronous trigger: the
// stream scanner that decides "this turn is over" must stay free of filesystem
// work. Coalesced (claudeUsagePendingRunCoalesce) so a chatty multi-turn session
// costs roughly one write per settled turn rather than one per stream line.
func claudeUsageRecordPendingRun(baseline time.Time) {
	if baseline.IsZero() {
		return
	}
	st := &claudeUsagePendingRunState
	st.mu.Lock()
	due := baseline.After(st.settled) &&
		(st.persisted.IsZero() || baseline.Sub(st.persisted) >= claudeUsagePendingRunCoalesce)
	st.mu.Unlock()
	if !due {
		return
	}
	// Resolved OFF the lock — it reads the cache file.
	fingerprint, scoped := claudeUsagePendingRunFingerprint()
	if !scoped {
		return
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	// Re-check under the lock: the debt may have been settled (and the file
	// removed) while the account was being resolved.
	if !baseline.After(st.settled) {
		return
	}
	if persistClaudeUsagePendingRun(baseline, fingerprint) {
		st.persisted = baseline
	}
}

// claudeUsagePendingRunFingerprint returns the account the record must be
// scoped to, read from the rate-limit cache's own `accountFingerprint`.
//
// Deliberately NOT currentClaudeAccountFingerprint: on a default macOS config
// that shells out to `security` under a 3s timeout, and paying it per settled
// turn would move the per-run cost this feature's coalescing exists to avoid
// rather than remove it (the same rule claudeUsageProbeStoredIdentity's comment
// states, and TestClaudeUsageProbeAfterRun_ReadsCredentialsOncePerActualProbe
// pins). The cache is the RIGHT source anyway: it is the identity the buckets a
// hydrated debt would refresh are already scoped to, so the two cannot disagree.
//
// (false) when no cache exists yet — a device with no reading at all has no
// stale row to rescue, and its next gather probes on the zero observation
// regardless, so there is nothing for a persisted debt to buy.
func claudeUsagePendingRunFingerprint() (string, bool) {
	snap, ok := loadClaudeRateLimitSnapshot(claudeRateLimitCachePath())
	if !ok {
		return "", false
	}
	return snap.AccountFingerprint, true
}

// persistClaudeUsagePendingRun writes the record, reporting whether it landed.
// Callers hold claudeUsagePendingRunState.mu.
func persistClaudeUsagePendingRun(baseline time.Time, fingerprint string) bool {
	path := claudeUsagePendingRunPath()
	now := time.Now()
	out, err := json.Marshal(claudeUsagePendingRun{
		SchemaVersion:      claudeUsagePendingRunSchema,
		AccountFingerprint: fingerprint,
		OwedObservedAtMs:   baseline.UnixMilli(),
		RecordedAtMs:       now.UnixMilli(),
	})
	if err != nil {
		return false
	}
	// Write-then-rename, the same idiom (and the same reason) as
	// mergeClaudeRateLimitCacheSerialized: a reader must never observe a
	// half-written file, and the PID + nanosecond suffix keeps two writers — or a
	// tmp left by a crashed run — from colliding on the intermediate name.
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), now.UnixNano())
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return false
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false
	}
	return true
}

// clearClaudeUsagePendingRun removes the record once `settled` — the baseline a
// settlement just discharged — has been observed.
//
// The watermark is kept in memory as well as on disk: a persist that was already
// resolving its account fingerprint when the settlement landed must not re-create
// the file behind it.
func clearClaudeUsagePendingRun(settled time.Time) {
	st := &claudeUsagePendingRunState
	st.mu.Lock()
	defer st.mu.Unlock()
	if settled.After(st.settled) {
		st.settled = settled
	}
	st.persisted = time.Time{}
	if err := os.Remove(claudeUsagePendingRunPath()); err != nil && !os.IsNotExist(err) {
		// Best-effort: a record we cannot delete expires on its own at
		// claudeUsagePendingRunMaxAge, and until then costs at most one probe per
		// process start — the same bound every other path here obeys.
		return
	}
}

// loadClaudeUsagePendingRun returns the persisted debt when it is still payable
// for THIS account, and (zero, false) otherwise.
//
// Discards: absent/unreadable/corrupt file, an unknown schema, a fingerprint
// belonging to another account (the same rule mergeClaudeRateLimitCache applies
// to the buckets themselves), a non-positive baseline, and a record older than
// claudeUsagePendingRunMaxAge.
func loadClaudeUsagePendingRun(fingerprint string, now time.Time) (time.Time, bool) {
	raw, err := os.ReadFile(claudeUsagePendingRunPath())
	if err != nil {
		return time.Time{}, false
	}
	var rec claudeUsagePendingRun
	if json.Unmarshal(raw, &rec) != nil {
		return time.Time{}, false
	}
	if rec.SchemaVersion != claudeUsagePendingRunSchema {
		return time.Time{}, false
	}
	if rec.AccountFingerprint != fingerprint {
		return time.Time{}, false
	}
	if rec.OwedObservedAtMs <= 0 || rec.RecordedAtMs <= 0 {
		return time.Time{}, false
	}
	recordedAt := time.UnixMilli(rec.RecordedAtMs)
	if now.Sub(recordedAt) > claudeUsagePendingRunMaxAge {
		return time.Time{}, false
	}
	return time.UnixMilli(rec.OwedObservedAtMs), true
}

// claudeUsageHydratePendingRun loads the persisted debt ONCE per process and
// records it back on the gate, so the first gather after an agent update (or any
// other restart) pays the run the previous process never got to.
//
// Scoped by the fingerprint the gather already decoded — a record written under
// another account is not ours to pay.
func claudeUsageHydratePendingRun(fingerprint string, now time.Time) {
	st := &claudeUsagePendingRunState
	st.mu.Lock()
	if st.hydrated {
		st.mu.Unlock()
		return
	}
	st.hydrated = true
	st.mu.Unlock()

	if baseline, ok := loadClaudeUsagePendingRun(fingerprint, now); ok {
		// recordOwed keeps the newest baseline, so a turn this process already
		// finished outranks the inherited one rather than being rolled back.
		claudeUsageProbe.recordOwed(baseline)
	}
}

// resetClaudeUsagePendingRun drops the in-memory latches and the on-disk record.
// Test-only seam, called from resetClaudeUsageProbeGate so no case can inherit
// another's debt — in memory or on disk.
func resetClaudeUsagePendingRun() {
	st := &claudeUsagePendingRunState
	st.mu.Lock()
	defer st.mu.Unlock()
	st.settled = time.Time{}
	st.persisted = time.Time{}
	st.hydrated = false
	// The file is removed ONLY when the record has been redirected by the env
	// seam. `go test` on a developer's own machine runs this reset dozens of
	// times, and the real agent's device record is not the suite's to delete —
	// the in-memory latches above are what actually leaks between cases.
	if os.Getenv(claudeUsagePendingRunEnv) != "" {
		_ = os.Remove(claudeUsagePendingRunPath())
	}
}
