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
	"sort"
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
	// AccountFingerprints carries the ADDITIONAL accounts this debt may belong
	// to — the other cache paths' owners on a multi-channel device. Additive
	// within schema 1: an older agent ignores it and keeps today's behaviour.
	AccountFingerprints []string `json:"accountFingerprints,omitempty"`
	OwedObservedAtMs    int64    `json:"owedObservedAtMs"`
	RecordedAtMs        int64    `json:"recordedAtMs"`
}

// scopedTo reports whether this record may be paid under `fingerprint`.
func (r claudeUsagePendingRun) scopedTo(fingerprint string) bool {
	if r.AccountFingerprint == fingerprint {
		return true
	}
	for _, alt := range r.AccountFingerprints {
		if alt == fingerprint {
			return true
		}
	}
	return false
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
// Called from the post-run goroutine for the stream-driven paths, never from
// their synchronous trigger: the scanner that decides "this turn is over" must
// stay free of filesystem work. The smoke is the one caller that pays it
// synchronously (noteClaudeTurnSpentDurably) because its process may be replaced
// the moment it returns.
//
// Coalescing is by BASELINE, not by elapsed time: a baseline at or before the
// one already on disk writes nothing — which is exactly the repeat the trailing
// goroutine makes of what the smoke already wrote, and the dominant duplicate —
// while any ADVANCE is written immediately. A time window here would be a
// correctness bug rather than a saving: two turns settling inside it leave the
// OLDER baseline durable, and an observation landing between them then settles
// the hydrated debt even though it predates the second turn. The bound that
// matters is per settled turn, which this keeps.
func claudeUsageRecordPendingRun(baseline time.Time) {
	if baseline.IsZero() {
		return
	}
	st := &claudeUsagePendingRunState
	st.mu.Lock()
	due := baseline.After(st.settled) && baseline.After(st.persisted)
	st.mu.Unlock()
	if !due {
		return
	}
	// Resolved OFF the lock — it reads the cache files.
	accounts := claudeUsagePendingRunAccounts()
	if len(accounts) == 0 {
		return
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	// Re-check under the lock: the debt may have been settled (and the file
	// removed) while the account was being resolved.
	if !baseline.After(st.settled) {
		return
	}
	if persistClaudeUsagePendingRun(baseline, accounts) {
		st.persisted = baseline
	}
}

// claudeUsagePendingRunAccounts returns the accounts the record must be scoped
// to, read from the rate-limit caches' own `accountFingerprint`.
//
// Deliberately NOT currentClaudeAccountFingerprint: on a default macOS config
// that shells out to `security` under a 3s timeout, and paying it per settled
// turn would move the per-run cost this feature's coalescing exists to avoid
// rather than remove it (the same rule claudeUsageProbeStoredIdentity's comment
// states, and TestClaudeUsageProbeAfterRun_ReadsCredentialsOncePerActualProbe
// pins). The caches are the RIGHT source anyway: they hold the identity the
// buckets a hydrated debt would refresh are already scoped to.
//
// Read from EVERY path the displayed rows are merged from
// (claudeRateLimitCachePaths), not just this channel's own file. On a box with
// two channels installed the one that lost Claude's statusLine hook has no local
// cache at all and shows rows exclusively from the pinned one — and consulting
// only the local path there returns nothing, so the smoke's debt is never
// persisted and the pre-smoke pinned rows go on suppressing the probe after the
// update: the very failure this record exists to survive, in its dual-channel
// form.
//
// EVERY distinct account found is kept, rather than just the newest cache's.
// Picking one by timestamp guesses which account the next gather will display,
// and after an account switch the guess is wrong in the direction that hurts:
// a newer cache for the previous account A scopes the debt to A while
// loadMergedClaudeRateLimitView renders the still-within-TTL cache for the
// current account B, so hydration under B rejects the record and the fresh-
// looking B rows suppress the probe — the stale card again. The turn was spent
// by whichever of these accounts Claude was authenticated as, the set is bounded
// by the number of cache paths (two), and the worst case of being generous is
// one extra probe under the existing per-account bounds. Ordered newest
// observation first so the primary field stays the best single guess for a
// reader that only understands it.
//
// Empty when no cache exists on any path — a device with no reading at all has
// no stale row to rescue, and its next gather probes on the zero observation
// regardless, so there is nothing for a persisted debt to buy.
func claudeUsagePendingRunAccounts() []string {
	type seen struct {
		fingerprint string
		observed    int64
	}
	found := make([]seen, 0, 2)
	for _, path := range claudeRateLimitCachePaths() {
		snap, ok := loadClaudeRateLimitSnapshot(path)
		if !ok {
			continue
		}
		observed := snap.LastProbeObservedAtMs
		if latest := latestClaudeObservation(snap.Buckets); !latest.IsZero() && latest.UnixMilli() > observed {
			observed = latest.UnixMilli()
		}
		at := -1
		for i, prev := range found {
			if prev.fingerprint == snap.AccountFingerprint {
				at = i
				break
			}
		}
		switch {
		case at < 0:
			found = append(found, seen{fingerprint: snap.AccountFingerprint, observed: observed})
		case observed > found[at].observed:
			found[at].observed = observed
		}
	}
	// Stable by construction: the path order is fixed (local, then pinned), so a
	// tie keeps the local account first exactly as it did before.
	sort.SliceStable(found, func(i, j int) bool { return found[i].observed > found[j].observed })
	accounts := make([]string, 0, len(found))
	for _, f := range found {
		accounts = append(accounts, f.fingerprint)
	}
	return accounts
}

// persistClaudeUsagePendingRun writes the record, reporting whether it landed.
// Callers hold claudeUsagePendingRunState.mu.
func persistClaudeUsagePendingRun(baseline time.Time, accounts []string) bool {
	path := claudeUsagePendingRunPath()
	now := time.Now()
	rec := claudeUsagePendingRun{
		SchemaVersion:    claudeUsagePendingRunSchema,
		OwedObservedAtMs: baseline.UnixMilli(),
		RecordedAtMs:     now.UnixMilli(),
	}
	if len(accounts) > 0 {
		// The newest cache's account stays in the single-valued field, so a
		// reader that predates the set still scopes the record the way it always
		// did; the rest ride along in `accountFingerprints`.
		rec.AccountFingerprint = accounts[0]
	}
	if len(accounts) > 1 {
		rec.AccountFingerprints = accounts[1:]
	}
	out, err := json.Marshal(rec)
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
//
// The removal is CONDITIONAL on what is actually on disk. A settlement reads the
// debt under the gate lock and clears the file afterwards, so a smoke that
// records and persists a NEWER baseline in that gap would otherwise have its
// record deleted by a settlement covering only the older one — and then the
// agent replacement the smoke triggers takes the in-memory debt with it, which
// is precisely the restart window this file exists to close. A record newer than
// `settled` is therefore left standing; its own settlement clears it.
func clearClaudeUsagePendingRun(settled time.Time) {
	st := &claudeUsagePendingRunState
	st.mu.Lock()
	defer st.mu.Unlock()
	if settled.After(st.settled) {
		st.settled = settled
	}
	// Read the record rather than st.persisted: the writer may be another
	// process (or this one, before a restart), and only the file can say which
	// baseline the deletion would actually be throwing away. An unreadable or
	// corrupt record carries no baseline to protect, so it is removed.
	if rec, ok := readClaudeUsagePendingRunRecord(); ok &&
		time.UnixMilli(rec.OwedObservedAtMs).After(settled) {
		return
	}
	st.persisted = time.Time{}
	if err := os.Remove(claudeUsagePendingRunPath()); err != nil && !os.IsNotExist(err) {
		// Best-effort: a record we cannot delete expires on its own at
		// claudeUsagePendingRunMaxAge, and until then costs at most one probe per
		// process start — the same bound every other path here obeys.
		return
	}
}

// readClaudeUsagePendingRunRecord returns the raw on-disk record, without any of
// the age/account judgement claudeUsagePendingRunLoad applies. (false) when the
// file is absent, unreadable, corrupt, of an unknown schema, or carries no
// usable baseline.
func readClaudeUsagePendingRunRecord() (claudeUsagePendingRun, bool) {
	raw, err := os.ReadFile(claudeUsagePendingRunPath())
	if err != nil {
		return claudeUsagePendingRun{}, false
	}
	var rec claudeUsagePendingRun
	if json.Unmarshal(raw, &rec) != nil {
		return claudeUsagePendingRun{}, false
	}
	if rec.SchemaVersion != claudeUsagePendingRunSchema {
		return claudeUsagePendingRun{}, false
	}
	if rec.OwedObservedAtMs <= 0 || rec.RecordedAtMs <= 0 {
		return claudeUsagePendingRun{}, false
	}
	return rec, true
}

// loadClaudeUsagePendingRun returns the persisted debt when it is still payable
// for THIS account, and (zero, false) otherwise.
//
// Discards: absent/unreadable/corrupt file, an unknown schema, a fingerprint
// belonging to another account (the same rule mergeClaudeRateLimitCache applies
// to the buckets themselves), a non-positive baseline, and a record older than
// claudeUsagePendingRunMaxAge.
func loadClaudeUsagePendingRun(fingerprint string, now time.Time) (time.Time, bool) {
	baseline, ok, _ := claudeUsagePendingRunLoad(fingerprint, now)
	return baseline, ok
}

// claudeUsagePendingRunLoad is loadClaudeUsagePendingRun plus the one thing the
// hydrate latch needs: whether the answer is SETTLED for this process.
//
// It is settled for every discard whose verdict cannot change while the process
// runs — no file, unreadable, corrupt, unknown schema, nonsense baseline, past
// claudeUsagePendingRunMaxAge — and for a successful load. It is NOT settled for
// the one discard that depends on something the caller may not have had yet: a
// well-formed, unexpired record whose fingerprint disagrees with the identity we
// were handed. The gather derives that identity from a credential read (bounded,
// and on macOS a `security` spawn), so a timeout there hands us "" for an
// account whose record is scoped — and latching on that would retire the debt
// for the whole process even though the very next gather can read the account
// fine. That is the failure this record exists to prevent, just moved.
//
// The order matters: age and shape are checked BEFORE the fingerprint, so a
// record that has aged out is settled regardless of whose it is and cannot keep
// the latch open forever.
func claudeUsagePendingRunLoad(fingerprint string, now time.Time) (baseline time.Time, ok, settled bool) {
	rec, readable := readClaudeUsagePendingRunRecord()
	if !readable {
		return time.Time{}, false, true
	}
	if now.Sub(time.UnixMilli(rec.RecordedAtMs)) > claudeUsagePendingRunMaxAge {
		return time.Time{}, false, true
	}
	if !rec.scopedTo(fingerprint) {
		return time.Time{}, false, false // may be ours; the identity we were given cannot say
	}
	return time.UnixMilli(rec.OwedObservedAtMs), true, true
}

// claudeUsageHydratePendingRun loads the persisted debt ONCE per process and
// records it back on the gate, so the first gather after an agent update (or any
// other restart) pays the run the previous process never got to.
//
// Scoped by the fingerprint the gather already decoded — a record written under
// another account is not ours to pay.
//
// "Once" means once the record has actually been ANSWERED: the latch is set for
// a load and for every permanent discard, but not for a fingerprint mismatch,
// which the next gather may resolve differently (see claudeUsagePendingRunLoad).
// A mismatch therefore costs one ~200-byte read per gather until the identity
// settles or the record ages out — far cheaper than the missed refresh it buys.
func claudeUsageHydratePendingRun(fingerprint string, now time.Time) {
	st := &claudeUsagePendingRunState
	st.mu.Lock()
	hydrated := st.hydrated
	st.mu.Unlock()
	if hydrated {
		return
	}

	// Read OFF the lock, the way every other reader here does.
	baseline, ok, settled := claudeUsagePendingRunLoad(fingerprint, now)

	st.mu.Lock()
	defer st.mu.Unlock()
	if settled {
		st.hydrated = true
	}
	// Never below the watermark this process has already PAID, and the test and
	// the recordOwed are done WITHOUT releasing the lock in between. Two gathers
	// can start concurrently after a restart and read the same record; if one of
	// them dropped the lock here, the other's probe could settle that baseline in
	// the gap and the late recordOwed would resurrect a discharged debt —
	// recordOwed has no watermark of its own — buying an OAuth request for
	// nothing. clearClaudeUsagePendingRun takes this same mutex to advance
	// st.settled, so holding it is what makes the pair atomic. (Lock order is
	// st.mu → gate.mu; every settlement releases the gate before it takes st.mu,
	// so the reverse edge does not exist.)
	if ok && baseline.After(st.settled) {
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
