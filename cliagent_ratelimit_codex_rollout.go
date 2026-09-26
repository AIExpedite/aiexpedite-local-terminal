// cliagent_ratelimit_codex_rollout.go — Codex session rollout discovery and
// scan (CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl).
//
// Split out of cliagent_ratelimit_codex.go, which keeps live capture, the
// contributor cache merge and the metric renderers. Everything here is the
// optional, budget-bounded scan that backfills and reconciles that cache from
// Codex's own persisted `token_count` telemetry: the redacted scan cursor,
// candidate discovery / ordering, per-file reads, and the usage-limit
// (quota-refusal) evidence mined from the same pass.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// codexRolloutScanFileCap bounds how many of the most-recent rollout logs the
// fallback opens before giving up, so a sessions directory holding thousands of
// files can't turn a usage refresh into a long scan. Every in-cap log is folded
// so distinct per-(identity, limit) contributors that newer logs omitted are
// still gathered; newest-first (identity, limit) dedup keeps the freshest
// reading per contributor.
const codexRolloutScanFileCap = 16

// Keep part of the optional rollout budget for consuming candidates found by
// discovery. Otherwise a slow sessions-tree walk can expire the shared child
// context after finding today's rollout but before opening it, repeating that
// starvation on every refresh.
const codexRolloutCandidateReadReserve = time.Second

// Candidate ranking gets only the first half of the post-discovery reserve.
// The remaining half is kept for opening and reading at least one selected
// rollout even when a large candidate backlog makes ranking hit its deadline.
const codexRolloutFileReadReserve = 500 * time.Millisecond

// A single slow rollout must leave a small slice for each later selected file.
// Without this reserve, the first retryable file can consume the entire child
// deadline and pin a capped batch forever.
const codexRolloutLaterFileReserve = 25 * time.Millisecond

// How often candidate ranking rechecks the optional scan budget. Frequent enough
// that a huge backlog cannot hold the reserve for long, coarse enough that the
// context check does not dominate the pass itself.
const codexRolloutCandidateRankCheckInterval = 256

// Directory enumeration is deliberately chunked so a large, slow, or
// AV-monitored rollout directory cannot trap the optional scan inside one
// uncancellable os.ReadDir call. The entries are sorted after collection to
// preserve the newest-date-first traversal of Codex's YYYY/MM/DD layout.
const codexRolloutReadDirChunkSize = 128

// codexReconcileFromRollout merges direct-run rollout evidence into the same
// normalized contributor cache populated by terminal-managed stdout. It is
// intentionally not Unknown-only: a still-live cached percentage may be older
// than a successful direct run. Newest evidence wins per metric identity and
// limit id, then the existing most-constrained aggregate determines the row and
// its observation timestamp.
//
// A FORCED reconcile (ctx from withCodexForcedReconcile) is one a run or the
// user is waiting on: it scans from below the run floor rather than trusting a
// cursor that may already have advanced past the run's rollout, and it commits
// through the blocking merge because dropping its evidence on lock contention
// is exactly how a finished run's utilization went missing.
func codexReconcileFromRollout(ctx context.Context, base, currentFingerprint string, now time.Time) ([]cliAgentUsageMetric, codexUsageLimitEvidence, time.Time) {
	codexResetRolloutCursorForVersion(ctx, currentFingerprint, now)
	cursor := codexRolloutScanCursorForAccount(base, currentFingerprint, now)
	floor, forced := codexForcedReconcileFrom(ctx)
	if forced {
		cursor = codexRolloutCursorBelowFloor(cursor, floor)
	}
	latestCachedObservation := codexLatestContributorObservation(codexContributorsForAccount(currentFingerprint))
	contribs, limit, latestObservation, highWater, ok := codexRolloutFallbackBuckets(ctx, base, now, cursor, latestCachedObservation)
	// Authentication can change while filesystem I/O is in progress. Never let
	// an old-account scan clear or overwrite a live capture already scoped to the
	// newly active account; the next refresh will reconcile under that account.
	if codexAccountFingerprintAtBase(base) != currentFingerprint {
		return codexMetricsFromCache(now, currentFingerprint), codexUsageLimitEvidence{}, time.Time{}
	}
	if ok || highWater != nil {
		// The forced path passes ctx so its bounded lock wait is clamped to the
		// gather deadline it is running inside.
		mergeCodexRateLimitCachePerLimitProgressWithLock(
			ctx, codexRateLimitCachePath(), contribs, nil, false, nil, false,
			now, currentFingerprint, highWater, base, forced, nil,
		)
	}
	return codexMetricsFromCache(now, currentFingerprint), limit, latestObservation
}

// codexRolloutCursorBelowFloor restarts a forced scan just below the run
// floor when the persisted cursor already sits above it. The contributor-derived
// watermark can advance past a run's rollout — a heartbeat captured live, a
// sibling file with a later mtime — and the run's own numeric frames would then
// never be read again. Retry identities are kept (they bypass the watermark
// anyway); the rest of the capped-scan state belongs to the higher cursor and is
// rebuilt by this pass. The merge only commits the resulting progress when it is
// not behind the stored cursor, so a forced pass can never rewind it.
func codexRolloutCursorBelowFloor(cursor codexRolloutScanCursor, floor time.Time) codexRolloutScanCursor {
	if floor.IsZero() {
		return cursor
	}
	floorNs := floor.Add(-codexRunFloorGrace).UnixNano()
	if floorNs < 0 {
		floorNs = 0
	}
	if cursor.mtimeNs <= floorNs {
		return cursor
	}
	return codexRolloutScanCursor{mtimeNs: floorNs, retryEntries: cursor.retryEntries}
}

// codexResetRolloutCursorForVersion resets the persisted scan cursor the first
// time a Codex binary other than the one that wrote it is in use, and stamps
// that binary in the SAME write (RolloutCursorVersion). A cursor written
// against the previous binary's layout may sit past files the new one writes
// differently, and trusting it is how a post-upgrade run's telemetry stayed
// unfound.
//
// The stamp lands at the reset, not when a pass completes: a post-upgrade
// rescan re-reads up to codexRolloutScanFileCap files under a bounded budget
// and often will not finish, and stamping on completion would reset the
// cursor again on every later refresh, starving the backlog march it is
// making. Mirrors the RolloutRootFingerprint reset in the contributor merge.
// A snapshot of another account, or an unknown current version, is left
// alone.
func codexResetRolloutCursorForVersion(ctx context.Context, currentFingerprint string, now time.Time) {
	version := currentCodexUsageCaptureVersion()
	if version == "" {
		return
	}
	snap, ok := loadCodexRateLimitSnapshot(codexRateLimitCachePath())
	if !ok || snap.AccountFingerprint != currentFingerprint || snap.RolloutCursorVersion == version {
		return
	}
	codexRateLimitCacheTransaction(ctx, codexRateLimitCachePath(), now, true, func(snap *codexRateLimitSnapshot) bool {
		if snap.AccountFingerprint != currentFingerprint || snap.RolloutCursorVersion == version {
			return false
		}
		codexClearRolloutProgress(snap)
		snap.RolloutCursorVersion = version
		return true
	})
}

// codexClearRolloutProgress drops every field of the persisted scan cursor,
// leaving the telemetry and run bookkeeping untouched.
func codexClearRolloutProgress(snap *codexRateLimitSnapshot) {
	snap.RolloutHighWaterMtimeMs = 0
	snap.RolloutHighWaterMtimeNs = 0
	snap.RolloutHighWaterBoundaryFingerprint = ""
	snap.RolloutHighWaterBoundaryCursor = ""
	snap.RolloutBacklogFingerprint = ""
	snap.RolloutBacklogCursor = ""
	snap.RolloutBacklogMtimeNs = 0
	snap.RolloutBacklogCohortSize = 0
	snap.RolloutRetryEntries = nil
	snap.RolloutRetryCursor = ""
	snap.RolloutRetryFingerprint = ""
	snap.RolloutFutureMtimeAnchorNs = 0
	snap.RolloutFutureMtimeFloorNs = 0
	snap.RolloutFutureMtimeCeilingNs = 0
	snap.RolloutFutureMtimeFingerprint = ""
	snap.RolloutFutureMtimeCursor = ""
	snap.RolloutFutureMtimeCohortSize = 0
	snap.RolloutFutureMtimeComplete = false
}

func codexLatestContributorObservation(contribs map[string]map[string]codexRateLimitBucket) time.Time {
	var latest time.Time
	for _, limits := range contribs {
		for _, bucket := range limits {
			if observed := time.UnixMilli(bucket.ObservedAtMs); bucket.ObservedAtMs > 0 && observed.After(latest) {
				latest = observed
			}
		}
	}
	return latest
}

type codexRolloutScanCursor struct {
	mtimeNs             int64
	boundaryFingerprint string
	boundaryCursor      string
	backlogFingerprint  string
	backlogCursor       string
	backlogMtimeNs      int64
	backlogCohortSize   int
	retryEntries        []string
	retryCursor         string
	retryFingerprint    string
	futureFingerprint   string
	futureCursor        string
	futureComplete      bool
	futureAnchorNs      int64
	futureFloorNs       int64
	futureCeilingNs     int64
	futureCohortSize    int
}

type codexRolloutScanProgress struct {
	mtimeNs             int64
	boundaryFingerprint string
	boundaryCursor      string
	backlogFingerprint  string
	backlogCursor       string
	backlogMtimeNs      int64
	backlogCohortSize   int
	retryEntries        []string
	retryCursor         string
	retryFingerprint    string
	futureFingerprint   string
	futureCursor        string
	futureComplete      bool
	futureAnchorNs      int64
	futureFloorNs       int64
	futureCeilingNs     int64
	futureCohortSize    int
}

// codexRolloutRootFingerprint identifies a CODEX_HOME without persisting its
// path. Absolute and symlink-resolved normalization prevents equivalent roots
// from triggering avoidable rescans; Windows paths are case-insensitive.
func codexRolloutRootFingerprint(base string) string {
	if base == "" {
		return ""
	}
	normalized, err := filepath.Abs(base)
	if err != nil {
		normalized = filepath.Clean(base)
	}
	if resolved, err := filepath.EvalSymlinks(normalized); err == nil {
		normalized = resolved
	}
	normalized = filepath.Clean(normalized)
	if runtime.GOOS == "windows" {
		normalized = strings.ToLower(normalized)
	}
	sum := sha256.Sum256([]byte(normalized))
	return fmt.Sprintf("%x", sum[:])
}

// codexRolloutScanCursorForAccount separates filesystem progress from provider
// event time. Completed progress is reusable only for the same account,
// CODEX_HOME and Codex binary. Legacy/unscoped cursors reset once, then the
// completed scan writes the root fingerprint; a cursor another binary wrote
// reads as empty until codexResetRolloutCursorForVersion has stamped the
// current one. Read-only: it selects a cursor and writes nothing. Otherwise legacy caches fall back to the oldest
// observable aggregate so a newer weekly-bearing file is not hidden by a
// fresher session row; empty/all-Unknown caches scan from zero.
func codexRolloutScanCursorForAccount(base, currentFingerprint string, now time.Time) codexRolloutScanCursor {
	snap, ok := loadCodexRateLimitSnapshot(codexRateLimitCachePath())
	if !ok || snap.AccountFingerprint != currentFingerprint {
		return codexRolloutScanCursor{}
	}
	if version := currentCodexUsageCaptureVersion(); version != "" && snap.RolloutCursorVersion != version {
		return codexRolloutScanCursor{}
	}
	hasStoredProgress := snap.RolloutHighWaterMtimeNs > 0 || snap.RolloutHighWaterMtimeMs > 0 ||
		snap.RolloutBacklogCursor != "" || len(snap.RolloutRetryEntries) > 0
	if hasStoredProgress &&
		snap.RolloutRootFingerprint != codexRolloutRootFingerprint(base) {
		return codexRolloutScanCursor{}
	}
	if snap.RolloutHighWaterMtimeNs > 0 || snap.RolloutBacklogCursor != "" || len(snap.RolloutRetryEntries) > 0 {
		// A completed cursor is filesystem progress, so it cannot legitimately
		// remain ahead of the current clock. This can happen after a clock
		// rollback or when upgrading a cache written before future mtimes were
		// clamped. Resetting (rather than capping to now) keeps rollouts written
		// between the rollback and this gather eligible for discovery.
		if snap.RolloutHighWaterMtimeNs > now.UnixNano() {
			return codexRolloutScanCursor{}
		}
		return codexRolloutScanCursor{
			mtimeNs:             snap.RolloutHighWaterMtimeNs,
			boundaryFingerprint: snap.RolloutHighWaterBoundaryFingerprint,
			boundaryCursor:      snap.RolloutHighWaterBoundaryCursor,
			backlogFingerprint:  snap.RolloutBacklogFingerprint,
			backlogCursor:       snap.RolloutBacklogCursor,
			backlogMtimeNs:      snap.RolloutBacklogMtimeNs,
			backlogCohortSize:   snap.RolloutBacklogCohortSize,
			retryEntries:        codexRolloutRetryList(codexRolloutRetrySet(snap.RolloutRetryEntries)),
			retryCursor:         snap.RolloutRetryCursor,
			retryFingerprint:    snap.RolloutRetryFingerprint,
			futureAnchorNs:      snap.RolloutFutureMtimeAnchorNs,
			futureFloorNs:       snap.RolloutFutureMtimeFloorNs,
			futureCeilingNs:     snap.RolloutFutureMtimeCeilingNs,
			futureFingerprint:   snap.RolloutFutureMtimeFingerprint,
			futureCursor:        snap.RolloutFutureMtimeCursor,
			futureCohortSize:    snap.RolloutFutureMtimeCohortSize,
			futureComplete:      snap.RolloutFutureMtimeComplete,
		}
	}
	if snap.RolloutHighWaterMtimeMs > 0 {
		mtimeNs := time.UnixMilli(snap.RolloutHighWaterMtimeMs).UnixNano()
		if mtimeNs > now.UnixNano() {
			return codexRolloutScanCursor{}
		}
		return codexRolloutScanCursor{
			mtimeNs:           mtimeNs,
			futureAnchorNs:    snap.RolloutFutureMtimeAnchorNs,
			futureFloorNs:     snap.RolloutFutureMtimeFloorNs,
			futureCeilingNs:   snap.RolloutFutureMtimeCeilingNs,
			futureFingerprint: snap.RolloutFutureMtimeFingerprint,
			futureCursor:      snap.RolloutFutureMtimeCursor,
			futureCohortSize:  snap.RolloutFutureMtimeCohortSize,
			futureComplete:    snap.RolloutFutureMtimeComplete,
		}
	}
	parts := codexPartitionByIdentity(codexContributorsForAccount(currentFingerprint))
	oldest := int64(0)
	for _, row := range []struct{ identity, slot string }{
		{codexIdentitySession, codexWindowPrimary},
		{codexIdentityWeekly, codexWindowSecondary},
	} {
		b, present := codexIdentityDisplayBucket(parts, row.identity, row.slot, now)
		if !present || b.ObservedAtMs <= 0 || (b.ResetsAtMs > 0 && now.UnixMilli() >= b.ResetsAtMs) {
			// A legacy snapshot has no completed-scan watermark. If either
			// display row is absent, scanning from the populated row's timestamp
			// could permanently hide an older rollout that supplies the missing
			// identity. Start from zero until both rows are observable.
			return codexRolloutScanCursor{}
		}
		if oldest == 0 || b.ObservedAtMs < oldest {
			oldest = b.ObservedAtMs
		}
	}
	return codexRolloutScanCursor{mtimeNs: time.UnixMilli(oldest).UnixNano()}
}

type codexRolloutCandidate struct {
	path       string
	boundaryID string
	mtime      time.Time
	size       int64
}

func codexRolloutBoundaryFingerprint(candidates []codexRolloutCandidate, mtimeNs int64) string {
	entries := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.mtime.UnixNano() == mtimeNs {
			entries = append(entries, codexRolloutBoundaryEntryDigest(candidate))
		}
	}
	if len(entries) == 0 {
		return ""
	}
	sort.Strings(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return fmt.Sprintf("%x", sum[:])
}

func codexRolloutBoundaryEntryDigest(candidate codexRolloutCandidate) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", candidate.boundaryID, candidate.size)))
	return fmt.Sprintf("%x", sum[:])
}

func codexRolloutFutureEntryDigest(candidate codexRolloutCandidate) string {
	identity := candidate.boundaryID
	if identity == "" {
		identity = candidate.path
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d", identity, candidate.size, candidate.mtime.UnixNano())))
	return fmt.Sprintf("%x", sum[:])
}

func codexRolloutBacklogEntryDigest(candidate codexRolloutCandidate) string {
	identity := candidate.boundaryID
	if identity == "" {
		identity = candidate.path
	}
	sum := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%x", sum[:])
}

func codexRolloutRetryEntryValid(entry string) bool {
	if len(entry) != sha256.Size*2 {
		return false
	}
	for _, c := range entry {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func codexRolloutRetrySet(entries []string) map[string]struct{} {
	retries := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if codexRolloutRetryEntryValid(entry) {
			retries[entry] = struct{}{}
		}
	}
	return retries
}

func codexRolloutRetryList(retries map[string]struct{}) []string {
	entries := make([]string, 0, len(retries))
	for entry := range retries {
		if codexRolloutRetryEntryValid(entry) {
			entries = append(entries, entry)
		}
	}
	sort.Strings(entries)
	return entries
}

func codexRolloutRetryFingerprint(retries map[string]struct{}) string {
	entries := codexRolloutRetryList(retries)
	if len(entries) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return fmt.Sprintf("%x", sum[:])
}

// codexRolloutBacklogFingerprint hashes the redacted identities of one saved
// cohort: the normal-time candidates above the completed watermark and at or
// below the cohort ceiling, in deterministic discovery order. It also reports
// how many identities it covered.
//
// Scoping to the ceiling is what lets the scan keep marching toward older
// rollouts on a machine that keeps starting sessions: a rollout created after
// the cohort was saved sits above the ceiling, is already eligible on its own
// (the resume filter only suppresses entries at or below the ceiling), and must
// not be read as a membership change that discards the saved rank cursor.
// Hashing identities rather than mutable size/mtime state keeps an append to an
// already-consumed rollout from restarting the cohort as well; when that append
// carries the file's mtime above the ceiling the cohort simply shrinks, which
// the size is there to distinguish from a substitution.
//
// Residual: on a filesystem whose mtime resolution is coarse enough that an
// append leaves the timestamp unchanged, that append is invisible to this cohort
// until a later write crosses a tick boundary and carries the file above the
// ceiling, where it is re-offered and re-read whole (reconciliation is
// newest-wins per identity and limit ID, so nothing earlier is lost by the
// re-read). Folding size into the cohort digest is deliberately not the answer:
// it cannot say which member changed, so the only available response is to drop
// the rank cursor, and an actively appended file on such a filesystem would then
// reset the cursor on every pass and starve the march toward older rollouts.
// Appends at the completed watermark's own tick are already covered separately,
// by the size-bearing boundary fingerprint.
func codexRolloutBacklogFingerprint(candidates []codexRolloutCandidate, cursorMtimeNs, ceilingMtimeNs int64, now time.Time) (string, int) {
	hash := sha256.New()
	count := 0
	for _, candidate := range candidates {
		mtimeNs := candidate.mtime.UnixNano()
		if mtimeNs <= cursorMtimeNs || candidate.mtime.After(now) {
			continue
		}
		if ceilingMtimeNs > 0 && mtimeNs > ceilingMtimeNs {
			continue
		}
		_, _ = hash.Write([]byte(codexRolloutBacklogEntryDigest(candidate)))
		_, _ = hash.Write([]byte{'\n'})
		count++
	}
	if count == 0 {
		return "", 0
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), count
}

// codexRolloutBacklogCutoff resolves the saved rank cursor when the stored
// cohort is still trustworthy. An exact fingerprint match means nothing inside
// the cohort changed. A strictly smaller cohort means members only left it —
// an append carries a rollout above the ceiling, where it is re-offered anyway —
// so the rank cursor still describes consumed work. Residual: a pass that both
// loses two members and gains one restored file with a preserved in-cohort mtime
// is accepted, which can leave that one file unread until the cohort completes.
func codexRolloutBacklogCutoff(candidates []codexRolloutCandidate, cursor codexRolloutScanCursor, now time.Time) (codexRolloutCandidate, bool) {
	if cursor.backlogCursor == "" || cursor.backlogFingerprint == "" ||
		cursor.backlogMtimeNs <= cursor.mtimeNs {
		return codexRolloutCandidate{}, false
	}
	fingerprint, size := codexRolloutBacklogFingerprint(candidates, cursor.mtimeNs, cursor.backlogMtimeNs, now)
	if fingerprint != cursor.backlogFingerprint &&
		!(cursor.backlogCohortSize > 0 && size < cursor.backlogCohortSize) {
		return codexRolloutCandidate{}, false
	}
	for _, candidate := range candidates {
		if candidate.mtime.UnixNano() > cursor.mtimeNs && !candidate.mtime.After(now) &&
			codexRolloutBacklogEntryDigest(candidate) == cursor.backlogCursor {
			return candidate, true
		}
	}
	return codexRolloutCandidate{}, false
}

// codexRolloutFutureFingerprint identifies the anomalous-mtime cohort above
// `anchor`. Positive bounds close that cohort around the members which existed
// when the anchor was saved: without them every rollout created afterwards — an
// ordinary current-time one on the next refresh included — is also newer than the
// anchor, so it changes the fingerprint, discards an unfinished capped cursor and
// restarts selection while older cohort members stay unread. Later arrivals are
// scanned on their own, outside the cohort.
func codexRolloutFutureFingerprint(candidates []codexRolloutCandidate, anchor time.Time, floorNs, ceilingNs int64) (string, int) {
	entries := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if !codexRolloutInFutureCohort(candidate, anchor, floorNs, ceilingNs) {
			continue
		}
		entries = append(entries, codexRolloutFutureEntryDigest(candidate))
	}
	if len(entries) == 0 {
		return "", 0
	}
	sort.Strings(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return fmt.Sprintf("%x", sum[:]), len(entries)
}

func codexRolloutFutureAnchor(cursor codexRolloutScanCursor, now time.Time) time.Time {
	if cursor.futureFingerprint != "" && cursor.futureAnchorNs > 0 {
		return time.Unix(0, cursor.futureAnchorNs)
	}
	return now
}

// codexRolloutInFutureCohort reports whether a candidate belongs to the cohort
// bounded by [floorNs, ceilingNs] above the anchor. Unset bounds leave that side
// open, which is how a cursor written before the bounds were persisted is read.
func codexRolloutInFutureCohort(candidate codexRolloutCandidate, anchor time.Time, floorNs, ceilingNs int64) bool {
	if !candidate.mtime.After(anchor) {
		return false
	}
	mtimeNs := candidate.mtime.UnixNano()
	if floorNs > 0 && mtimeNs < floorNs {
		return false
	}
	return ceilingNs <= 0 || mtimeNs <= ceilingNs
}

// codexRolloutFutureCohortBounds are the oldest and newest mtimes above the
// anchor at the moment the cohort is defined — exactly the open cohort at that
// instant, so deriving them for a legacy cursor that predates the bounds
// reproduces the membership that cursor was saved with. Afterwards they keep the
// cohort fixed: a rollout written later lands at wall-clock time, below a cohort
// whose members are ahead of the clock, and an appended member climbs above the
// ceiling — either way it is scanned separately instead of redefining the cohort.
//
// Residual: an arrival whose mtime happens to fall inside the bounds is still
// indistinguishable from a cohort member, so it restarts the cohort as before.
// Residual: an arrival above the ceiling has no cursor of its own — the main
// high-water is clamped below the clock and the backlog cursor skips
// future-dated files — so it is re-offered on every refresh until the clock
// catches up and codexRolloutFutureCohortCaughtUp retires the anomaly. That
// repeats a bounded read of the newest evidence rather than losing an older
// one; codexRolloutFutureCohortReserve is what keeps those repeats from
// starving the cohort underneath them.
//
// Residual: more than codexRolloutScanFileCap arrivals above the same ceiling
// fill the batch with the newest of them, so an older arrival stays unread
// until the clock passes its mtime and the ordinary backlog cursor walks it —
// delayed, never dropped. The cohort reserve does not cover that set at either
// end of the cohort's life: its bounds stop at the ceiling, so an above-ceiling
// arrival is outside the reserve whether or not the cohort is complete.
func codexRolloutFutureCohortBounds(candidates []codexRolloutCandidate, anchor time.Time) (int64, int64) {
	oldest, newest := int64(0), int64(0)
	for _, candidate := range candidates {
		if !candidate.mtime.After(anchor) {
			continue
		}
		mtimeNs := candidate.mtime.UnixNano()
		if oldest == 0 || mtimeNs < oldest {
			oldest = mtimeNs
		}
		if mtimeNs > newest {
			newest = mtimeNs
		}
	}
	return oldest, newest
}

// codexRolloutFutureCohort resolves the saved anomalous-mtime cohort against the
// current listing and reports whether its rank cursor still describes consumed
// work. An exact scoped fingerprint match means nothing inside the cohort
// changed; a strictly smaller cohort means members only left it — an append
// carries a member above the ceiling, where it is re-offered anyway — which is
// the same tolerance the backlog cohort uses. Residual: a pass that both loses a
// member and gains a restored file with a preserved in-cohort mtime is accepted,
// leaving that one file unread until the cohort completes.
func codexRolloutFutureCohort(candidates []codexRolloutCandidate, cursor codexRolloutScanCursor, now time.Time) (time.Time, int64, int64, bool) {
	anchor := codexRolloutFutureAnchor(cursor, now)
	if cursor.futureFingerprint == "" {
		return anchor, 0, 0, false
	}
	floorNs, ceilingNs := cursor.futureFloorNs, cursor.futureCeilingNs
	if ceilingNs <= 0 {
		floorNs, ceilingNs = codexRolloutFutureCohortBounds(candidates, anchor)
	}
	fingerprint, size := codexRolloutFutureFingerprint(candidates, anchor, floorNs, ceilingNs)
	matches := fingerprint == cursor.futureFingerprint ||
		(cursor.futureCohortSize > 0 && size < cursor.futureCohortSize)
	return anchor, floorNs, ceilingNs, matches
}

type codexReadDirResumeState struct {
	f        *os.File
	entries  []os.DirEntry
	complete bool
	// Per-entry metadata work runs after enumeration reaches EOF, so a listing
	// can be complete while the caller's budget expires part way through it.
	// traversal remembers the reverse index still to be processed so the next
	// bounded refresh continues past that prefix instead of re-Stat-ing the same
	// newest names forever and never reaching the directory's older entries.
	traversal    int
	traversalSet bool
	// A caller that runs out of budget part way through an interrupted (still
	// incomplete) listing cannot record a traversal offset, because the entries
	// it was handed are only ever returned once. pending holds the tail it could
	// not process so the next bounded refresh replays those names instead of
	// waiting for the stream to reach EOF to see them again.
	pending []os.DirEntry
	gate    chan struct{}
	refs    int
	usedAt  time.Time
}

// How long the metadata pass over an interrupted leaf listing may run after the
// discovery context is already done. The chunk is bounded, so consuming it is
// what lets a repeatedly cancelled refresh reach its rollout evidence at all;
// this caps the overrun so a slow or stalled filesystem cannot spend the
// parent's sibling reserve on os.Stat calls. Whatever the grace does not cover
// is preserved for the next refresh rather than dropped. Overridden in tests.
var codexRolloutPartialLeafGrace = 25 * time.Millisecond

const codexReadDirResumeLimit = 16

var (
	codexReadDirResumeMu sync.Mutex
	codexReadDirResumes  = map[string]*codexReadDirResumeState{}
)

func codexCloseReadDirResume(dir string) {
	var f *os.File
	codexReadDirResumeMu.Lock()
	state := codexReadDirResumes[dir]
	if state != nil && state.refs == 0 {
		delete(codexReadDirResumes, dir)
		f = state.f
		state.f = nil
	}
	codexReadDirResumeMu.Unlock()
	if f != nil {
		_ = f.Close()
	}
}

// codexReadDirContext returns the directory listing together with the reverse
// index its caller should start from: len(entries)-1 for a fresh or partial
// listing, or the position a previous interrupted metadata pass recorded.
func codexReadDirContext(ctx context.Context, dir string) ([]os.DirEntry, int, bool, error) {
	// Keep an interrupted directory stream open so the next bounded refresh
	// resumes after the last complete chunk instead of repeatedly enumerating
	// the same prefix. Serialize access because os.File's directory offset and
	// the accumulated listing form one cursor.
	codexReadDirResumeMu.Lock()
	state := codexReadDirResumes[dir]
	if state == nil {
		state = &codexReadDirResumeState{gate: make(chan struct{}, 1)}
		state.gate <- struct{}{}
		codexReadDirResumes[dir] = state
	}
	state.refs++
	state.usedAt = time.Now()
	codexReadDirResumeMu.Unlock()

	select {
	case <-ctx.Done():
		codexReadDirResumeRelease(dir, state, false)
		return nil, 0, false, ctx.Err()
	case <-state.gate:
	}
	// An open stream, accumulated entries, or a completed listing means this
	// invocation did not enumerate the directory from a fresh snapshot. The
	// caller may consume its entries, but must require one fresh pass before
	// advancing filesystem progress: a file created in an already-consumed
	// prefix is not guaranteed to appear when the stream later reaches EOF.
	resumed := state.f != nil || len(state.entries) > 0 || state.complete
	remove := false
	defer func() {
		state.gate <- struct{}{}
		codexReadDirResumeRelease(dir, state, remove)
	}()
	// A reader that reached EOF can remain in the map while callers that were
	// already queued still hold references to it. Reuse its complete listing;
	// reopening here would append every directory entry a second time.
	if state.complete {
		// The retained listing is what carries the metadata-traversal position, so
		// it is retired by codexRecordReadDirTraversal once a caller works through
		// it rather than by the first reader that consumes it. It already contains
		// every name an interrupted hand-off left pending, so drop that replay
		// queue rather than offering the same entries twice.
		codexDropPendingReplay(state)
		return append([]os.DirEntry(nil), state.entries...), codexReadDirTraversalStart(state), true, nil
	}
	if state.f == nil {
		f, err := os.Open(dir)
		if err != nil {
			remove = true
			return nil, 0, resumed, err
		}
		state.f = f
	}
	// Entries accumulated before this invocation were already returned to the
	// previous discovery pass. If this invocation is interrupted too, return
	// only its newly completed chunks so cancellation cannot trigger an
	// ever-growing replay of duplicate metadata work.
	firstNew := len(state.entries)

	for {
		if err := ctx.Err(); err != nil {
			partial := codexTakePendingReplay(state, append([]os.DirEntry(nil), state.entries[firstNew:]...))
			return partial, len(partial) - 1, resumed, err
		}
		batch, readErr := state.f.ReadDir(codexRolloutReadDirChunkSize)
		state.entries = append(state.entries, batch...)
		if readErr == io.EOF {
			_ = state.f.Close()
			state.f = nil
			state.complete = true
			break
		}
		if readErr != nil {
			entries := codexTakePendingReplay(state, append([]os.DirEntry(nil), state.entries[firstNew:]...))
			_ = state.f.Close()
			state.f = nil
			remove = true
			return entries, len(entries) - 1, resumed, readErr
		}
	}
	entries := state.entries
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	return entries, codexReadDirTraversalStart(state), resumed, nil
}

func codexReadDirTraversalStart(state *codexReadDirResumeState) int {
	if state.traversalSet && state.traversal >= 0 && state.traversal < len(state.entries) {
		return state.traversal
	}
	return len(state.entries) - 1
}

// codexRecordReadDirTraversal stores how far a caller processed a completed
// listing. next is the reverse index still to be handled; a negative value means
// the listing is exhausted, which retires the retained state so the following
// refresh enumerates the directory from a fresh snapshot and can once again
// authorize completed-scan progress.
// codexTakePendingReplay appends the tail an earlier interrupted caller could
// not process to the entries this invocation newly enumerated. Callers walk the
// returned slice from its end, so replayed names go last and drain before the
// newer ones rather than being deferred again.
func codexTakePendingReplay(state *codexReadDirResumeState, fresh []os.DirEntry) []os.DirEntry {
	codexReadDirResumeMu.Lock()
	pending := state.pending
	state.pending = nil
	codexReadDirResumeMu.Unlock()
	if len(pending) == 0 {
		return fresh
	}
	return append(fresh, pending...)
}

func codexDropPendingReplay(state *codexReadDirResumeState) {
	codexReadDirResumeMu.Lock()
	state.pending = nil
	codexReadDirResumeMu.Unlock()
}

// codexRecordReadDirPending preserves entries handed out by an interrupted
// listing that their caller could not process within its budget. An incomplete
// listing has no traversal offset to record — its entries are returned once and
// then advance past firstNew — so without this they would only reappear once the
// stream finally reached EOF.
func codexRecordReadDirPending(dir string, remaining []os.DirEntry) {
	if len(remaining) == 0 {
		return
	}
	codexReadDirResumeMu.Lock()
	defer codexReadDirResumeMu.Unlock()
	state := codexReadDirResumes[dir]
	if state == nil || state.complete {
		// The listing was retired by a read failure, or has since reached EOF and
		// retained the full enumeration. Either way the next refresh surfaces these
		// names again on its own, so queuing them would only duplicate candidates.
		return
	}
	state.usedAt = time.Now()
	state.pending = append(state.pending, remaining...)
}

func codexRecordReadDirTraversal(dir string, next int) {
	var closeFile *os.File
	codexReadDirResumeMu.Lock()
	if state := codexReadDirResumes[dir]; state != nil && state.complete {
		state.usedAt = time.Now()
		switch {
		case next >= 0 && next < len(state.entries):
			state.traversal, state.traversalSet = next, true
		case state.refs == 0:
			delete(codexReadDirResumes, dir)
			closeFile, state.f = state.f, nil
		default:
			// Another reader still holds this listing. Drop the position rather
			// than the state; that reader restarts from the newest entry, which is
			// the behaviour a fresh enumeration would give it anyway.
			state.traversal, state.traversalSet = 0, false
		}
	}
	codexReadDirResumeMu.Unlock()
	if closeFile != nil {
		_ = closeFile.Close()
	}
}

func codexReadDirResumeRelease(dir string, state *codexReadDirResumeState, remove bool) {
	var closeStates []*codexReadDirResumeState
	codexReadDirResumeMu.Lock()
	state.refs--
	state.usedAt = time.Now()
	if remove && state.refs == 0 && codexReadDirResumes[dir] == state {
		delete(codexReadDirResumes, dir)
	}
	for len(codexReadDirResumes) > codexReadDirResumeLimit {
		var oldestDir string
		var oldest *codexReadDirResumeState
		for candidateDir, candidate := range codexReadDirResumes {
			if candidate.refs == 0 && (oldest == nil || candidate.usedAt.Before(oldest.usedAt)) {
				oldestDir, oldest = candidateDir, candidate
			}
		}
		if oldest == nil {
			break
		}
		delete(codexReadDirResumes, oldestDir)
		closeStates = append(closeStates, oldest)
	}
	codexReadDirResumeMu.Unlock()
	for _, candidate := range closeStates {
		if candidate.f != nil {
			_ = candidate.f.Close()
		}
	}
}

// codexDiscoverRolloutCandidates walks the fixed YYYY/MM/DD rollout layout
// newest-first while checking the optional scan context between filesystem
// entries. Starting with the newest date prevents a large history from starving
// today's direct-run evidence when the Codex child budget expires. Unlike
// filepath.Glob, discovery also stops promptly after cancellation. complete is
// false when cancellation or a metadata error means scan progress must not
// advance.
func codexDiscoverRolloutCandidates(ctx context.Context, base string, cursor codexRolloutScanCursor) ([]codexRolloutCandidate, bool) {
	root := filepath.Join(base, "sessions")
	candidates := []codexRolloutCandidate{}
	boundaryCandidates := []codexRolloutCandidate{}
	retryEntries := codexRolloutRetrySet(cursor.retryEntries)
	complete := true
	var walkDateLayout func(string, int)
	walkDateLayout = func(dir string, depth int) {
		if ctx.Err() != nil {
			complete = false
			return
		}
		entries, start, resumed, err := codexReadDirContext(ctx, dir)
		if resumed {
			// Resumed directory streams are intentionally retained across bounded
			// refreshes, but reaching EOF does not prove the accumulated listing
			// includes files created in a prefix consumed by an earlier refresh.
			// Consume and persist the discovered evidence, then require one fresh
			// full enumeration before advancing the completed-scan cursor.
			complete = false
		}
		if err != nil {
			if depth == 0 && os.IsNotExist(err) {
				return
			}
			complete = false
			if len(entries) == 0 {
				return
			}
		}
		// A canceled leaf-directory read can still return a complete chunk of
		// directory entries. Consume those bounded results so repeated refreshes
		// can reach their rollout evidence, while keeping discovery incomplete so
		// the completed-scan cursor does not advance.
		consumePartialLeaf := err != nil && depth == 3
		// Cancellation is usually what produced that chunk, so re-checking ctx here
		// would consume none of it and starve discovery again. Bound the pass by a
		// short grace instead: it covers the whole chunk on a responsive filesystem
		// and collapses to a couple of entries on a stalled one.
		partialLeafDeadline := time.Time{}
		if consumePartialLeaf {
			partialLeafDeadline = time.Now().Add(codexRolloutPartialLeafGrace)
		}
		// os.ReadDir sorts by filename. Codex's zero-padded YYYY/MM/DD layout
		// therefore becomes chronological when traversed in reverse.
		for i := start; i >= 0; i-- {
			if consumePartialLeaf {
				if time.Now().After(partialLeafDeadline) {
					// This listing is incomplete, so there is no traversal offset to
					// record: these entries were handed out once and the stream has
					// already advanced past them. Preserve the unprocessed tail so the
					// next refresh replays it instead of waiting for EOF to see it again.
					complete = false
					codexRecordReadDirPending(dir, entries[:i+1])
					return
				}
			} else if ctx.Err() != nil {
				// Enumeration may already have reached EOF; only the metadata pass ran
				// out of budget. Record where it stopped so the next refresh resumes
				// below this prefix instead of re-walking it and stalling forever.
				complete = false
				codexRecordReadDirTraversal(dir, i)
				return
			}
			entry := entries[i]
			path := filepath.Join(dir, entry.Name())
			if depth < 3 {
				if entry.IsDir() {
					walkDateLayout(path, depth+1)
				}
				continue
			}
			if entry.IsDir() {
				continue
			}
			matched, err := filepath.Match("rollout-*.jsonl", entry.Name())
			if err != nil || !matched {
				continue
			}
			info, err := os.Stat(path)
			if err != nil {
				// A transient metadata failure leaves this discovery pass incomplete.
				// We may still consume other candidates, but must not advance progress
				// past a file whose mtime could not be evaluated.
				complete = false
				continue
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				complete = false
				continue
			}
			candidate := codexRolloutCandidate{path: path, boundaryID: rel, mtime: info.ModTime(), size: info.Size()}
			_, retry := retryEntries[codexRolloutBacklogEntryDigest(candidate)]
			switch mtimeNs := info.ModTime().UnixNano(); {
			case retry:
				// Retry identities bypass the completed watermark, but still flow
				// through the ordinary capped newest-first selection below.
				candidates = append(candidates, candidate)
			case mtimeNs > cursor.mtimeNs:
				candidates = append(candidates, candidate)
			case cursor.mtimeNs > 0 && mtimeNs == cursor.mtimeNs:
				boundaryCandidates = append(boundaryCandidates, candidate)
			}
		}
		codexRecordReadDirTraversal(dir, -1)
	}
	walkDateLayout(root, 0)
	// Exact-mtime equality normally means the boundary is unchanged. On coarse
	// filesystems, however, an append can increase a rollout's size without
	// changing its reported mtime. Compare a redacted metadata fingerprint and
	// rescan equality only when that boundary changed (or once for a legacy cache
	// that predates fingerprints).
	if len(boundaryCandidates) > 0 &&
		(cursor.boundaryCursor != "" || cursor.boundaryFingerprint == "" ||
			codexRolloutBoundaryFingerprint(boundaryCandidates, cursor.mtimeNs) != cursor.boundaryFingerprint) {
		candidates = append(candidates, boundaryCandidates...)
	}
	return candidates, complete
}

func codexRolloutDiscoveryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return ctx, func() {}
	}
	remaining := time.Until(deadline)
	reserve := codexRolloutCandidateReadReserve
	if remaining <= reserve {
		reserve = remaining / 2
	}
	return context.WithDeadline(ctx, deadline.Add(-reserve))
}

func codexRolloutCandidateOrderingContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return ctx, func() {}
	}
	remaining := time.Until(deadline)
	reserve := codexRolloutFileReadReserve
	if remaining <= reserve {
		reserve = remaining / 2
	}
	return context.WithDeadline(ctx, deadline.Add(-reserve))
}

func codexRolloutFileContext(ctx context.Context, filesAfter int) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok || filesAfter <= 0 {
		return ctx, func() {}
	}
	reserve := time.Duration(filesAfter) * codexRolloutLaterFileReserve
	remaining := time.Until(deadline)
	if reserve >= remaining {
		reserve = remaining / 2
	}
	return context.WithDeadline(ctx, deadline.Add(-reserve))
}

// codexUsageLimitNotice renders the card notice for a quota refusal, or "" when
// none should be shown.
//
// Two guards keep the notice honest:
//
//   - It is only shown while a window is still unobservable. Any window we can
//     actually report supersedes the refusal — a real percentage is strictly
//     more informative than "you were refused a while ago".
//   - The evidence must be newer than every observation behind those metrics.
//     Codex writes a rollout log on every attempt, so a still-exhausted account
//     re-evidences itself the moment the user tries again; ranking by time means
//     an old refusal can never shout over telemetry that arrived after it.
//
// The age cap exists because a refusal carries no window and therefore no reset:
// Codex says only "try again at <time>" in prose we deliberately do not parse.
// Expiring the notice can understate a multi-day weekly exhaustion, which is the
// safe direction — the next attempt re-evidences it — whereas an unbounded
// notice would keep declaring a limit that cleared days ago.
func codexUsageLimitNotice(metrics []cliAgentUsageMetric, limit codexUsageLimitEvidence, latestRolloutObservation, now time.Time) string {
	if limit.At.IsZero() || now.Sub(limit.At) > codexUsageLimitNoticeMaxAge {
		return ""
	}
	if !latestRolloutObservation.IsZero() && !latestRolloutObservation.Before(limit.At) {
		return ""
	}
	anyUnknown := false
	for _, m := range metrics {
		if m.Unknown {
			anyUnknown = true
		}
		observed, err := time.Parse(time.RFC3339, m.ObservedAt)
		if err == nil && !observed.Before(limit.At) {
			return ""
		}
	}
	if !anyUnknown {
		return ""
	}
	notice := "Codex refused a run because this account's usage limit was reached, so its capacity is unreported until Codex sends a fresh window."
	return notice
}

// codexUsageLimitNoticeMaxAge is how long a quota refusal keeps explaining an
// unobservable card. Sized to comfortably outlast Codex's 5-hour window while
// staying far short of the weekly one, so a cleared limit stops being announced
// without waiting days for the truth to catch up.
const codexUsageLimitNoticeMaxAge = 12 * time.Hour

// codexRolloutCandidateBefore ranks rollout candidates newest-first: normal-time
// files ahead of future-dated ones (invalid ordering evidence after a clock
// rollback or a timestamp-preserving restore), then by mtime descending, then by
// filename so files sharing an mtime break the tie deterministically.
func codexRolloutCandidateBefore(a, b codexRolloutCandidate, now time.Time) bool {
	aFuture := a.mtime.After(now)
	bFuture := b.mtime.After(now)
	if aFuture != bFuture {
		return !aFuture
	}
	if !a.mtime.Equal(b.mtime) {
		return a.mtime.After(b.mtime)
	}
	return a.path > b.path
}

// codexInsertNewestRolloutCandidate keeps a fixed-capacity slice in rollout
// rank order without sorting candidates that cannot enter it.
func codexInsertNewestRolloutCandidate(selected []codexRolloutCandidate, candidate codexRolloutCandidate, now time.Time, capacity int) []codexRolloutCandidate {
	if capacity <= 0 || len(selected) == capacity &&
		!codexRolloutCandidateBefore(candidate, selected[len(selected)-1], now) {
		return selected
	}
	pos := sort.Search(len(selected), func(j int) bool {
		return codexRolloutCandidateBefore(candidate, selected[j], now)
	})
	selected = append(selected, candidate)
	copy(selected[pos+1:], selected[pos:])
	selected[pos] = candidate
	if len(selected) > capacity {
		selected = selected[:capacity]
	}
	return selected
}

// codexRolloutReserve reports whether a candidate belongs to a group that a
// capped selection must keep making progress on. Groups are defined by a saved
// scan cursor whose only way forward is to open its own members, so a group is
// starved — not merely delayed — when newer arrivals can take every slot.
type codexRolloutReserve func(codexRolloutCandidate) bool

// codexRolloutBoundaryReserve claims a share of the batch for the unfinished
// equal-mtime boundary a saved boundary cursor is resuming through.
func codexRolloutBoundaryReserve(mtimeNs int64) codexRolloutReserve {
	return func(candidate codexRolloutCandidate) bool {
		return mtimeNs > 0 && candidate.mtime.UnixNano() == mtimeNs
	}
}

// codexRolloutFutureCohortReserve claims a share of the batch for an unfinished
// anchored future cohort. Every future-dated rollout that arrives above the
// cohort's saved ceiling is scanned outside it (see codexRolloutFutureCohortBounds)
// and outranks its members, so a full batch of such arrivals would otherwise
// leave the cohort cursor unchanged on every refresh and its older members —
// carrying quota evidence no other file restates — permanently unread.
func codexRolloutFutureCohortReserve(anchor time.Time, floorNs, ceilingNs int64) codexRolloutReserve {
	return func(candidate codexRolloutCandidate) bool {
		return codexRolloutInFutureCohort(candidate, anchor, floorNs, ceilingNs)
	}
}

// codexRolloutBacklogReserve claims a share of the batch for the unread tail of
// the normal-time cohort a saved rank cursor is resuming through: the eligible
// members ranked after `cutoff` and at or below the cohort ceiling the cursor
// was saved with.
//
// That cohort ceiling rises to the newest normal-time mtime on every completed
// pass, so a sustained stream of cap-sized arrivals above it keeps the ordinary
// newest-first selection full while codexRolloutBacklogProgress holds the saved
// cutoff in place (it deliberately keeps the deeper of this pass's oldest pick
// and the saved one). Without a reserve the tail is then never selected at all,
// and a distinct metered limit only an older rollout restates stays
// unreconciled for as long as the arrival rate holds.
func codexRolloutBacklogReserve(cutoff codexRolloutCandidate, cursorMtimeNs, ceilingMtimeNs int64, now time.Time) codexRolloutReserve {
	return func(candidate codexRolloutCandidate) bool {
		mtimeNs := candidate.mtime.UnixNano()
		return mtimeNs > cursorMtimeNs && mtimeNs <= ceilingMtimeNs &&
			!candidate.mtime.After(now) && codexRolloutCandidateBefore(cutoff, candidate, now)
	}
}

// codexSelectNewestRolloutCandidates returns the `capacity` highest-ranked
// candidates in rank order without ordering the rest. A full sort of a large
// backlog is both O(n log n) and uncancellable, so it can burn the read reserve
// that codexRolloutDiscoveryContext deliberately kept for opening the files it
// just found. Selection instead makes one pass in which every candidate past the
// first `capacity` usually costs a single comparison against the current worst
// kept entry, and it observes ctx so an exhausted budget stops here with the
// evidence gathered so far rather than mid-sort. complete is false when the pass
// was cut short, which keeps the completed-scan cursor from advancing past
// candidates that were never ranked.
//
// Each entry in reserves claims a bounded share of the batch for a group whose
// saved scan cursor can only advance when that group is opened — an unfinished
// equal-mtime boundary, an unfinished anchored future cohort that every later
// future-dated arrival outranks, or the unread tail of an unfinished normal-time
// backlog whose ceiling every later arrival sits above. Without the reserve an
// ongoing stream of newer files fills every slot on every refresh and the group
// is starved permanently. The reserved share is capacity/(len(reserves)+1) each,
// so the ordinary top-N selection always keeps at least an equal share for fresh
// evidence and each group still finishes in bounded deterministic batches.
// When retryEntries fill the ordinary selection, one slot is reserved for the
// newest non-retry candidate so persistent failures cannot pin an older backlog.
// When the retry cohort is larger than the slots left for it, a matching retry
// cursor rotates the retry subset past the previous batch so a recovered older
// failure is reopened instead of reselecting the same newest identities forever.
// The rotation covers mixed batches too — fresh files keep their slots, but they
// no longer suppress rotation through the retries sharing the batch.
func codexSelectNewestRolloutCandidates(ctx context.Context, candidates []codexRolloutCandidate, now time.Time, capacity int, reserves []codexRolloutReserve, retryEntries map[string]struct{}, retryCursor, retryFingerprint string) ([]codexRolloutCandidate, bool) {
	if capacity <= 0 {
		return nil, true
	}
	selected := make([]codexRolloutCandidate, 0, capacity+1)
	var newestNonRetry codexRolloutCandidate
	haveNonRetry := false
	reserveCapacity := capacity / (len(reserves) + 1)
	if reserveCapacity == 0 {
		reserveCapacity = 1
	}
	reserved := make([][]codexRolloutCandidate, len(reserves))
	for i, candidate := range candidates {
		if i%codexRolloutCandidateRankCheckInterval == 0 && ctx.Err() != nil {
			return selected, false
		}
		selected = codexInsertNewestRolloutCandidate(selected, candidate, now, capacity)
		if _, retry := retryEntries[codexRolloutBacklogEntryDigest(candidate)]; !retry &&
			(!haveNonRetry || codexRolloutCandidateBefore(candidate, newestNonRetry, now)) {
			newestNonRetry, haveNonRetry = candidate, true
		}
		for r, member := range reserves {
			if member(candidate) {
				reserved[r] = codexInsertNewestRolloutCandidate(reserved[r], candidate, now, reserveCapacity)
			}
		}
	}
	// Collect every reserved path before merging any of them: one group must not
	// be able to evict an entry another group already counted toward its reserve.
	reservedPaths := map[string]struct{}{}
	for _, group := range reserved {
		for _, entry := range group {
			reservedPaths[entry.path] = struct{}{}
		}
	}
	for _, group := range reserved {
		for _, entry := range group {
			alreadySelected := false
			for _, candidate := range selected {
				if candidate.path == entry.path {
					alreadySelected = true
					break
				}
			}
			if alreadySelected {
				continue
			}
			if len(selected) == capacity {
				// Evict the lowest-ranked non-reserved entry. Truncating the slice
				// would discard a reserved entry already counted toward a group's
				// share whenever the ordinary top-N selection included only part of it.
				evict := len(selected) - 1
				for ; evict >= 0; evict-- {
					if _, isReserved := reservedPaths[selected[evict].path]; !isReserved {
						break
					}
				}
				if evict < 0 {
					continue
				}
				selected = append(selected[:evict], selected[evict+1:]...)
			}
			selected = codexInsertNewestRolloutCandidate(selected, entry, now, capacity)
		}
	}
	if haveNonRetry && len(selected) == capacity {
		hasSelectedNonRetry := false
		for _, candidate := range selected {
			if _, retry := retryEntries[codexRolloutBacklogEntryDigest(candidate)]; !retry {
				hasSelectedNonRetry = true
				break
			}
		}
		if !hasSelectedNonRetry {
			// The ordinary rank is entirely retry work. Replace its lowest-ranked
			// member; that identity remains in retryEntries and is re-offered on the
			// next pass while the saved backlog cursor advances past fresh work.
			selected = selected[:len(selected)-1]
			selected = codexInsertNewestRolloutCandidate(selected, newestNonRetry, now, capacity)
		}
	}
	if len(candidates) > capacity &&
		retryCursor != "" && retryFingerprint != "" &&
		codexRolloutRetryFingerprint(retryEntries) == retryFingerprint {
		if cutoff, ok := codexRolloutCandidateByDigest(candidates, retryCursor); ok {
			selected = codexRotateRetryRolloutCandidates(selected, candidates, cutoff, retryEntries, now, capacity)
		}
	}
	return selected, true
}

func codexRolloutCandidateByDigest(candidates []codexRolloutCandidate, digest string) (codexRolloutCandidate, bool) {
	for _, candidate := range candidates {
		if codexRolloutBacklogEntryDigest(candidate) == digest {
			return candidate, true
		}
	}
	return codexRolloutCandidate{}, false
}

// codexRotateRetryRolloutCandidates refills the retry slots of an ordinary
// selection from the retry cohort ranked after `cutoff`, wrapping back to the
// newest retries once that tail is exhausted.
//
// Non-retry members keep their slots: fresh evidence and the newest-non-retry
// reserve are not part of the retry rotation, and a batch that mixes both must
// still rotate its retry subset. Otherwise a steady trickle of fresh rollouts —
// one new session log per refresh, alongside a retry cohort larger than the
// remaining slots — would reselect the same highest-ranked retries forever and a
// recovered lower-ranked one would never be reopened.
func codexRotateRetryRolloutCandidates(selected, candidates []codexRolloutCandidate, cutoff codexRolloutCandidate, retryEntries map[string]struct{}, now time.Time, capacity int) []codexRolloutCandidate {
	kept := make([]codexRolloutCandidate, 0, capacity+1)
	retrySlots := 0
	for _, candidate := range selected {
		if _, retry := retryEntries[codexRolloutBacklogEntryDigest(candidate)]; retry {
			retrySlots++
			continue
		}
		kept = append(kept, candidate)
	}
	if retrySlots == 0 {
		return selected
	}
	after := make([]codexRolloutCandidate, 0, retrySlots+1)
	wrap := make([]codexRolloutCandidate, 0, retrySlots+1)
	for _, candidate := range candidates {
		if _, retry := retryEntries[codexRolloutBacklogEntryDigest(candidate)]; !retry {
			continue
		}
		if codexRolloutCandidateBefore(cutoff, candidate, now) {
			after = codexInsertNewestRolloutCandidate(after, candidate, now, retrySlots)
			continue
		}
		wrap = codexInsertNewestRolloutCandidate(wrap, candidate, now, retrySlots)
	}
	rotated := after
	for _, candidate := range wrap {
		if len(rotated) == retrySlots {
			break
		}
		rotated = codexInsertNewestRolloutCandidate(rotated, candidate, now, retrySlots)
	}
	for _, candidate := range rotated {
		kept = codexInsertNewestRolloutCandidate(kept, candidate, now, capacity)
	}
	return kept
}

func codexRolloutRetryRotationProgress(eligible, attempted []codexRolloutCandidate, retryEntries map[string]struct{}) (string, string) {
	if len(attempted) == 0 || len(retryEntries) == 0 {
		return "", ""
	}
	attemptedIDs := make(map[string]struct{}, len(attempted))
	// `attempted` is in rollout rank order, so the last still-retryable member is
	// the lowest-ranked retry this pass opened — where the next batch resumes.
	// Non-retry members (fresh evidence, the newest-non-retry reserve) and retries
	// this pass recovered are skipped rather than voiding the rotation: a batch
	// that mixes fresh work with an overflowing retry cohort still has to record
	// how far through that cohort it got.
	cursor := ""
	for _, candidate := range attempted {
		digest := codexRolloutBacklogEntryDigest(candidate)
		attemptedIDs[digest] = struct{}{}
		if _, retry := retryEntries[digest]; retry {
			cursor = digest
		}
	}
	if cursor == "" {
		return "", ""
	}
	omitted := false
	for _, candidate := range eligible {
		digest := codexRolloutBacklogEntryDigest(candidate)
		if _, retry := retryEntries[digest]; !retry {
			continue
		}
		if _, ok := attemptedIDs[digest]; !ok {
			omitted = true
			break
		}
	}
	if !omitted {
		return "", ""
	}
	return codexRolloutRetryFingerprint(retryEntries), cursor
}

func codexUnconsumedRolloutCandidates(candidates []codexRolloutCandidate, cursor codexRolloutScanCursor, now time.Time) []codexRolloutCandidate {
	// Any membership or size change invalidates a positional cursor. Restart the
	// affected set rather than letting a newly appended/created entry rank ahead
	// of the saved cutoff and get mistaken for already-consumed evidence.
	boundaryMatches := cursor.mtimeNs > 0 && cursor.boundaryCursor != "" &&
		codexRolloutBoundaryFingerprint(candidates, cursor.mtimeNs) == cursor.boundaryFingerprint
	var boundaryCutoff codexRolloutCandidate
	boundaryFound := false
	if boundaryMatches {
		for _, candidate := range candidates {
			if candidate.mtime.UnixNano() == cursor.mtimeNs &&
				codexRolloutBoundaryEntryDigest(candidate) == cursor.boundaryCursor {
				boundaryCutoff, boundaryFound = candidate, true
				break
			}
		}
	}

	backlogCutoff, backlogFound := codexRolloutBacklogCutoff(candidates, cursor, now)

	futureAnchor, futureFloorNs, futureCeilingNs, futureMatches := codexRolloutFutureCohort(candidates, cursor, now)
	var futureCutoff codexRolloutCandidate
	futureFound := false
	if futureMatches && !cursor.futureComplete && cursor.futureCursor != "" {
		for _, candidate := range candidates {
			if codexRolloutInFutureCohort(candidate, futureAnchor, futureFloorNs, futureCeilingNs) &&
				codexRolloutFutureEntryDigest(candidate) == cursor.futureCursor {
				futureCutoff, futureFound = candidate, true
				break
			}
		}
	}

	retryEntries := codexRolloutRetrySet(cursor.retryEntries)
	eligible := make([]codexRolloutCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if _, retry := retryEntries[codexRolloutBacklogEntryDigest(candidate)]; retry {
			eligible = append(eligible, candidate)
			continue
		}
		if backlogFound && candidate.mtime.UnixNano() > cursor.mtimeNs &&
			candidate.mtime.UnixNano() <= cursor.backlogMtimeNs && !candidate.mtime.After(now) &&
			!codexRolloutCandidateBefore(backlogCutoff, candidate, now) {
			continue
		}
		if futureMatches && codexRolloutInFutureCohort(candidate, futureAnchor, futureFloorNs, futureCeilingNs) {
			// Rank the cohort against its own anchor, never the moving clock: every
			// member is above the anchor by definition, so the ahead-of-now tier in
			// codexRolloutCandidateBefore collapses and the order stays the one the
			// cursor was saved with. Ranking against `now` would reorder the cohort
			// mid-catch-up as members cross the clock — an unread member that just
			// became normal-time would outrank the still-future cutoff and be
			// mistaken for consumed work.
			if cursor.futureComplete ||
				(futureFound && !codexRolloutCandidateBefore(futureCutoff, candidate, futureAnchor)) {
				continue
			}
		}
		if boundaryFound && candidate.mtime.UnixNano() == cursor.mtimeNs {
			// Equal-mtime ties are deterministic. Everything ranked before or at the
			// saved cutoff was opened by an earlier pass; resume strictly after it.
			if !codexRolloutCandidateBefore(boundaryCutoff, candidate, now) {
				continue
			}
		}
		eligible = append(eligible, candidate)
	}
	return eligible
}

func codexRolloutBacklogProgress(candidates, eligible, selected []codexRolloutCandidate, now time.Time, cursor codexRolloutScanCursor) (string, string, int64, int, bool) {
	cohortMtimeNs := codexRolloutNewestNormalMtimeNs(candidates, cursor.mtimeNs, now)
	fingerprint, cohortSize := codexRolloutBacklogFingerprint(candidates, cursor.mtimeNs, cohortMtimeNs, now)
	selectedNormal := make(map[string]struct{}, len(selected))
	var lastSelected codexRolloutCandidate
	haveLast := false
	newestSelectedMtimeNs := cursor.mtimeNs
	for _, candidate := range selected {
		if candidate.mtime.UnixNano() <= cursor.mtimeNs || candidate.mtime.After(now) {
			continue
		}
		if candidate.mtime.UnixNano() > newestSelectedMtimeNs {
			newestSelectedMtimeNs = candidate.mtime.UnixNano()
		}
		selectedNormal[codexRolloutFutureEntryDigest(candidate)] = struct{}{}
		if !haveLast || codexRolloutCandidateBefore(lastSelected, candidate, now) {
			lastSelected, haveLast = candidate, true
		}
	}
	// `remaining` decides whether the cohort is finished; `rankBound` is the
	// newest thing this pass left unread and therefore bounds how deep a single
	// positional cursor may claim. They differ by one rule: equal-mtime overflow
	// at the newest selected boundary has its own boundary cursor, so it does not
	// keep the cohort open, but a reserve can still evict such an entry and the
	// backlog cursor must not be allowed to step over it.
	remaining := false
	var rankBound codexRolloutCandidate
	haveRankBound := false
	for _, candidate := range eligible {
		if candidate.mtime.UnixNano() <= cursor.mtimeNs || candidate.mtime.After(now) {
			continue
		}
		if _, ok := selectedNormal[codexRolloutFutureEntryDigest(candidate)]; ok {
			continue
		}
		if !haveRankBound || codexRolloutCandidateBefore(candidate, rankBound, now) {
			rankBound, haveRankBound = candidate, true
		}
		if candidate.mtime.UnixNano() < newestSelectedMtimeNs {
			remaining = true
		}
	}
	if !remaining {
		return "", "", 0, 0, true
	}
	if !haveLast {
		return fingerprint, cursor.backlogCursor, cohortMtimeNs, cohortSize, false
	}
	// Selection normally ranks the eligible set newest-first, so its picks are one
	// run contiguous from the newest candidate and its oldest pick is the deepest
	// safe cursor. A reserve breaks that: it can open members ranked below an
	// entry it evicted, leaving two disjoint runs. Claim only the run contiguous
	// with the newest, or the tail loses the evicted entry between them.
	advance, haveAdvance := lastSelected, true
	if haveRankBound && !codexRolloutCandidateBefore(advance, rankBound, now) {
		advance, haveAdvance = codexRolloutDeepestBefore(selected, rankBound, cursor.mtimeNs, now)
	}
	saved, savedFound := codexRolloutBacklogCutoff(candidates, cursor, now)
	if savedFound {
		// When a batch of rollouts created above the ceiling fills the cap, this
		// pass's own oldest pick can still be newer than the saved cutoff; keeping
		// the deeper of the two stops that from replaying the tail. It cannot be
		// taken past an unread entry, which a reserved batch can leave above it.
		if haveAdvance && codexRolloutCandidateBefore(advance, saved, now) &&
			(!haveRankBound || codexRolloutCandidateBefore(saved, rankBound, now)) {
			advance = saved
		}
		// The reserved tail run is contiguous with the region the saved cutoff
		// already covers, so it can be recorded even when unread newer files sit
		// above it — as long as the ceiling stays where it was, leaving those files
		// outside the cohort and still eligible. Prefer it only when it reaches
		// deeper into the cohort than the advancing form would.
		if tail, ok := codexRolloutBacklogTailRun(eligible, selectedNormal, saved, cursor, now); ok &&
			(!haveAdvance || codexRolloutCandidateBefore(advance, tail, now)) {
			pinnedFingerprint, pinnedSize :=
				codexRolloutBacklogFingerprint(candidates, cursor.mtimeNs, cursor.backlogMtimeNs, now)
			return pinnedFingerprint, codexRolloutBacklogEntryDigest(tail), cursor.backlogMtimeNs, pinnedSize, false
		}
	}
	if !haveAdvance {
		return fingerprint, cursor.backlogCursor, cohortMtimeNs, cohortSize, false
	}
	return fingerprint, codexRolloutBacklogEntryDigest(advance), cohortMtimeNs, cohortSize, false
}

// codexRolloutDeepestBefore returns the oldest-ranked normal-time member of
// `selected` that still ranks ahead of `bound`.
func codexRolloutDeepestBefore(selected []codexRolloutCandidate, bound codexRolloutCandidate, cursorMtimeNs int64, now time.Time) (codexRolloutCandidate, bool) {
	var deepest codexRolloutCandidate
	found := false
	for _, candidate := range selected {
		if candidate.mtime.UnixNano() <= cursorMtimeNs || candidate.mtime.After(now) ||
			!codexRolloutCandidateBefore(candidate, bound, now) {
			continue
		}
		if !found || codexRolloutCandidateBefore(deepest, candidate, now) {
			deepest, found = candidate, true
		}
	}
	return deepest, found
}

// codexRolloutBacklogTailRun reports how far into the saved cohort this pass
// walked, starting at the saved cutoff and stopping at the first member it did
// not open. The backlog cursor is one position and everything ranked ahead of it
// is treated as consumed, so only a run that stays contiguous with the region
// the cutoff already covers may be recorded.
func codexRolloutBacklogTailRun(eligible []codexRolloutCandidate, opened map[string]struct{}, cutoff codexRolloutCandidate, cursor codexRolloutScanCursor, now time.Time) (codexRolloutCandidate, bool) {
	inTail := func(candidate codexRolloutCandidate) bool {
		mtimeNs := candidate.mtime.UnixNano()
		return mtimeNs > cursor.mtimeNs && mtimeNs <= cursor.backlogMtimeNs &&
			!candidate.mtime.After(now) && codexRolloutCandidateBefore(cutoff, candidate, now)
	}
	var blocker codexRolloutCandidate
	haveBlocker := false
	for _, candidate := range eligible {
		if _, ok := opened[codexRolloutFutureEntryDigest(candidate)]; ok || !inTail(candidate) {
			continue
		}
		if !haveBlocker || codexRolloutCandidateBefore(candidate, blocker, now) {
			blocker, haveBlocker = candidate, true
		}
	}
	var deepest codexRolloutCandidate
	found := false
	for _, candidate := range eligible {
		if _, ok := opened[codexRolloutFutureEntryDigest(candidate)]; !ok || !inTail(candidate) {
			continue
		}
		if haveBlocker && !codexRolloutCandidateBefore(candidate, blocker, now) {
			continue
		}
		if !found || codexRolloutCandidateBefore(deepest, candidate, now) {
			deepest, found = candidate, true
		}
	}
	return deepest, found
}

func codexRolloutNewestNormalMtimeNs(candidates []codexRolloutCandidate, cursorMtimeNs int64, now time.Time) int64 {
	newest := cursorMtimeNs
	for _, candidate := range candidates {
		mtimeNs := candidate.mtime.UnixNano()
		if mtimeNs > newest && !candidate.mtime.After(now) {
			newest = mtimeNs
		}
	}
	return newest
}

// codexRolloutDeepestContiguous returns the oldest-ranked member of `selected`
// that still ranks ahead of `bound` — the deepest position a single positional
// cursor may claim when the pass's picks are not one contiguous run.
//
// Retry rotation can open a cohort member ranked below one it evicted, leaving
// an unselected healthy entry between two selected ones. Healthy entries do not
// bypass a positional cursor the way retry identities do, so recording the
// lowest selected member would treat that gap as consumed and the distinct
// quota evidence behind it would never be read.
func codexRolloutDeepestContiguous(selected []codexRolloutCandidate, bound codexRolloutCandidate, now time.Time) (codexRolloutCandidate, bool) {
	var deepest codexRolloutCandidate
	found := false
	for _, candidate := range selected {
		if !codexRolloutCandidateBefore(candidate, bound, now) {
			continue
		}
		if !found || codexRolloutCandidateBefore(deepest, candidate, now) {
			deepest, found = candidate, true
		}
	}
	return deepest, found
}

func codexRolloutBoundaryProgress(candidates, eligible, selected []codexRolloutCandidate, mtimeNs int64) codexRolloutScanProgress {
	// Every boundary member shares `mtimeNs`, so any instant at that mtime orders
	// them identically.
	ref := time.Unix(0, mtimeNs)
	selectedAtBoundary := map[string]struct{}{}
	selectedList := make([]codexRolloutCandidate, 0, len(selected))
	var lastSelected codexRolloutCandidate
	haveLast := false
	for _, candidate := range selected {
		if candidate.mtime.UnixNano() == mtimeNs {
			selectedAtBoundary[codexRolloutBoundaryEntryDigest(candidate)] = struct{}{}
			selectedList = append(selectedList, candidate)
			if !haveLast || codexRolloutCandidateBefore(lastSelected, candidate, ref) {
				lastSelected, haveLast = candidate, true
			}
		}
	}
	// `rankBound` is the newest boundary member this pass left unread. A retry
	// rotation can evict an entry ranked above one it opens, so the picks are not
	// necessarily contiguous and the cursor may not be taken past this bound.
	remaining := false
	var rankBound codexRolloutCandidate
	for _, candidate := range eligible {
		if candidate.mtime.UnixNano() != mtimeNs {
			continue
		}
		if _, ok := selectedAtBoundary[codexRolloutBoundaryEntryDigest(candidate)]; ok {
			continue
		}
		if !remaining || codexRolloutCandidateBefore(candidate, rankBound, ref) {
			rankBound, remaining = candidate, true
		}
	}
	boundaryCursor := ""
	if remaining && haveLast {
		advance, haveAdvance := lastSelected, true
		if !codexRolloutCandidateBefore(advance, rankBound, ref) {
			advance, haveAdvance = codexRolloutDeepestContiguous(selectedList, rankBound, ref)
		}
		if haveAdvance {
			boundaryCursor = codexRolloutBoundaryEntryDigest(advance)
		}
	}
	return codexRolloutScanProgress{
		mtimeNs:             mtimeNs,
		boundaryFingerprint: codexRolloutBoundaryFingerprint(candidates, mtimeNs),
		boundaryCursor:      boundaryCursor,
	}
}

func codexRolloutBoundaryHasUnselected(eligible, selected []codexRolloutCandidate, mtimeNs int64) bool {
	selectedAtBoundary := map[string]struct{}{}
	for _, candidate := range selected {
		if candidate.mtime.UnixNano() == mtimeNs {
			selectedAtBoundary[codexRolloutBoundaryEntryDigest(candidate)] = struct{}{}
		}
	}
	for _, candidate := range eligible {
		if candidate.mtime.UnixNano() != mtimeNs {
			continue
		}
		if _, ok := selectedAtBoundary[codexRolloutBoundaryEntryDigest(candidate)]; !ok {
			return true
		}
	}
	return false
}

func codexRolloutHasUnselectedBelowProgress(eligible, selected []codexRolloutCandidate, cursorMtimeNs, progressMtimeNs int64) bool {
	selectedEntries := make(map[string]struct{}, len(selected))
	for _, candidate := range selected {
		selectedEntries[codexRolloutBoundaryEntryDigest(candidate)] = struct{}{}
	}
	for _, candidate := range eligible {
		mtimeNs := candidate.mtime.UnixNano()
		if mtimeNs <= cursorMtimeNs || mtimeNs >= progressMtimeNs {
			continue
		}
		if _, ok := selectedEntries[codexRolloutBoundaryEntryDigest(candidate)]; !ok {
			return true
		}
	}
	return false
}

// codexRolloutFutureProgress carries the anchored cohort forward. A cohort that
// still matches keeps its saved bounds, so rollouts written afterwards are
// scanned as ordinary candidates without redefining — or restarting — a cohort
// whose capped scan is still unfinished. A cohort that no longer matches is
// redefined by the current listing.
func codexRolloutFutureProgress(candidates, eligible, selected []codexRolloutCandidate, now time.Time, cursor codexRolloutScanCursor) (int64, int64, int64, string, string, int, bool) {
	anchor, floorNs, ceilingNs, matches := codexRolloutFutureCohort(candidates, cursor, now)
	if !matches {
		floorNs, ceilingNs = codexRolloutFutureCohortBounds(candidates, anchor)
	}
	fingerprint, cohortSize := codexRolloutFutureFingerprint(candidates, anchor, floorNs, ceilingNs)
	if fingerprint == "" {
		return 0, 0, 0, "", "", 0, false
	}
	selectedFuture := map[string]struct{}{}
	selectedList := make([]codexRolloutCandidate, 0, len(selected))
	var lastSelected codexRolloutCandidate
	haveLast := false
	for _, candidate := range selected {
		if !codexRolloutInFutureCohort(candidate, anchor, floorNs, ceilingNs) {
			continue
		}
		selectedFuture[codexRolloutFutureEntryDigest(candidate)] = struct{}{}
		selectedList = append(selectedList, candidate)
		if !haveLast || codexRolloutCandidateBefore(lastSelected, candidate, anchor) {
			lastSelected, haveLast = candidate, true
		}
	}
	// `rankBound` is the newest cohort member this pass left unread. Retry
	// rotation can evict an entry ranked above one it opens, so the picks are not
	// necessarily contiguous and the cursor may not be taken past this bound.
	// Every rank here is taken against the anchor for the same reason the resume
	// filter is: a cursor written under clock-relative ordering would not mean the
	// same thing once a member crosses `now`.
	remaining := false
	var rankBound codexRolloutCandidate
	for _, candidate := range eligible {
		if !codexRolloutInFutureCohort(candidate, anchor, floorNs, ceilingNs) {
			continue
		}
		if _, ok := selectedFuture[codexRolloutFutureEntryDigest(candidate)]; ok {
			continue
		}
		if !remaining || codexRolloutCandidateBefore(candidate, rankBound, anchor) {
			rankBound, remaining = candidate, true
		}
	}
	if !remaining {
		return anchor.UnixNano(), floorNs, ceilingNs, fingerprint, "", cohortSize, true
	}
	if haveLast {
		advance, haveAdvance := lastSelected, true
		if !codexRolloutCandidateBefore(advance, rankBound, anchor) {
			advance, haveAdvance = codexRolloutDeepestContiguous(selectedList, rankBound, anchor)
		}
		// No pick ranks ahead of the gap: this pass claimed nothing contiguous with
		// the cohort head, so fall through to the saved cursor rather than stepping
		// the cohort over an unread member.
		if haveAdvance {
			return anchor.UnixNano(), floorNs, ceilingNs, fingerprint,
				codexRolloutFutureEntryDigest(advance), cohortSize, false
		}
	}
	if matches {
		return anchor.UnixNano(), floorNs, ceilingNs, fingerprint, cursor.futureCursor, cohortSize, false
	}
	return anchor.UnixNano(), floorNs, ceilingNs, fingerprint, "", cohortSize, false
}

func codexRolloutFutureCohortCaughtUp(candidates []codexRolloutCandidate, now time.Time, fingerprint string, complete bool) bool {
	// Once every member of the anchored future cohort has been consumed and its
	// mtime is no longer ahead of the clock, it is ordinary completed filesystem
	// progress. Retaining the old anchor would make each later normal rollout
	// change the historical cohort fingerprint and reopen the consumed files.
	// The catch-up test is deliberately unscoped: any file still ahead of the
	// clock — cohort member or later arrival — keeps anomalous handling on.
	current, _ := codexRolloutFutureFingerprint(candidates, now, 0, 0)
	return complete && fingerprint != "" && current == ""
}

// codexRolloutFallbackBuckets reads Codex's session rollout logs
// (CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl) for the most recent populated
// `rate_limits` frame and returns its per-(window, limit) contributors. The
// contributors are returned un-collapsed so the caller can partition them by
// metric identity — a slot that carried two distinct-identity limits (e.g. a
// session and a weekly reading during bucket migration) must not be flattened to
// a single bucket here or one row would be lost.
//
// Account scoping: rollout logs carry no account identity of their own, so they
// can't be fingerprint-matched the way the on-disk cache is. Instead we reject
// any log written BEFORE auth.json's current mtime — a fresh `codex login`
// rewrites auth.json, so its mtime advances past every rollout log the
// previous account produced. This prevents the credentials-swap bleed the cache
// path's fingerprint check already guards against (showing a prior account's
// quota under the new account). When auth.json is missing/unreadable the guard
// is disabled, matching the best-effort unscoped behaviour the parser already
// uses when the account is unknown. Best-effort: returns (nil, false) on any
// problem.
func codexRolloutFallbackBuckets(ctx context.Context, base string, now time.Time, cursor codexRolloutScanCursor, priorObservations ...time.Time) (map[string]map[string]codexRateLimitBucket, codexUsageLimitEvidence, time.Time, *codexRolloutScanProgress, bool) {
	if base == "" {
		return nil, codexUsageLimitEvidence{}, time.Time{}, nil, false
	}
	// auth.json mtime is the account-login watermark (zero = missing → guard
	// off). A fresh `codex login` rewrites auth.json, so its mtime marks when
	// the current account took over this CODEX_HOME.
	//
	// We scope rollout logs by their SESSION START time (the first line's
	// timestamp), NOT the file mtime: a previous account's session that is still
	// running when a new account logs in keeps appending to its log, pushing the
	// file mtime past the login even though the session — and its quota — belong
	// to the old account. The start time, fixed when the session began, stays on
	// the correct side of the login. (Residual caveat: a token refresh that
	// rewrites auth.json mid-session can over-reject same-account logs that
	// started earlier; that degrades to Unknown, never to cross-account bleed.)
	var authMod time.Time
	if info, err := os.Stat(expandHome(base, "auth.json")); err == nil {
		authMod = info.ModTime()
	}
	// A future login watermark cannot safely separate the current account from
	// earlier sessions. Treat the optional rollout source as unavailable for
	// this pass instead of rejecting current evidence and then advancing past
	// it; once the clock or file timestamp is corrected, the unchanged rollout
	// remains eligible for a retry.
	if authMod.After(now) {
		return nil, codexUsageLimitEvidence{}, time.Time{}, nil, false
	}
	// Date-nested layout: sessions/YYYY/MM/DD/rollout-<ISO-timestamp>-*.jsonl.
	discoveryCtx, cancelDiscovery := codexRolloutDiscoveryContext(ctx)
	candidates, discoveryComplete := codexDiscoverRolloutCandidates(discoveryCtx, base, cursor)
	cancelDiscovery()
	if len(candidates) == 0 {
		return nil, codexUsageLimitEvidence{}, time.Time{}, nil, false
	}
	eligibleCandidates := codexUnconsumedRolloutCandidates(candidates, cursor, now)
	if len(eligibleCandidates) == 0 {
		// The boundary changed only by removing entries after an earlier partial
		// pass, or every unchanged future-dated entry was already consumed. With
		// no unread entry left, finish the normal boundary without reopening an
		// already-consumed file and preserve the anomalous-file completion state.
		if discoveryComplete {
			progress := codexRolloutBoundaryProgress(candidates, nil, nil, cursor.mtimeNs)
			progress.futureAnchorNs, progress.futureFloorNs, progress.futureCeilingNs,
				progress.futureFingerprint, progress.futureCursor,
				progress.futureCohortSize, progress.futureComplete =
				codexRolloutFutureProgress(candidates, nil, nil, now, cursor)
			if codexRolloutFutureCohortCaughtUp(
				candidates, now, progress.futureFingerprint, progress.futureComplete,
			) {
				progress = codexRolloutBoundaryProgress(
					candidates, nil, nil,
					codexRolloutNewestNormalMtimeNs(candidates, cursor.mtimeNs, now),
				)
			}
			return nil, codexUsageLimitEvidence{}, time.Time{}, &progress, false
		}
		return nil, codexUsageLimitEvidence{}, time.Time{}, nil, false
	}
	// Rank candidates by file mtime descending, NOT by filename (= session
	// start time). When sessions overlap — e.g. an older still-active session
	// runs alongside a newer-started but idle one, or a long-lived session is
	// resumed after newer files exist — filename order treats the stale session
	// as the "newest" reading. mtime tracks the last append, so the file being
	// written most recently (the live source of truth) is considered first.
	// Future-dated mtimes are invalid ordering evidence after a clock rollback
	// or timestamp-preserving restore, so rank them after every normal-time
	// candidate. Otherwise more than one capped batch of anomalous files could
	// hide a newly completed normal-time rollout and then advance past it.
	// Ties fall back to filename order so a deterministic chronological tiebreak
	// applies when two files share an mtime.
	//
	// Ranking keeps only the capped newest candidates in one cancellable pass
	// rather than ordering the whole backlog first: a sessions tree holding
	// thousands of logs would otherwise spend the reserved candidate-read time
	// inside an uninterruptible sort and reach the read loop with nothing left,
	// repeating that same discovery-and-sort on every refresh without ever
	// opening a rollout.
	// Accumulate across files newest-first, keyed by (identity, limit id) — NOT by
	// physical slot. Keying on the slot would let a newer log that only carried a
	// migrated weekly contributor under `primary` block an older log's `primary`
	// session contributor, leaving the session identity Unknown even though a
	// slightly older log still holds it. Deduping by identity+limit keeps the
	// newest reading per contributor while still backfilling identities/limits the
	// newer logs never restated, so downstream identity partitioning sees every
	// distinct reading. Newest-first iteration means the first-seen reading of a
	// given (identity, limit) wins.
	type rolloutContribution struct {
		slot    string
		limitID string
		bucket  codexRateLimitBucket
	}
	winners := map[string]rolloutContribution{}
	runFloor, _ := codexForcedReconcileFrom(ctx)
	var limit codexUsageLimitEvidence
	// Identity of the rollout that supplied `limit`, so a still-live refusal can
	// be held eligible on its own rather than by voiding the whole pass.
	var limitEntry string
	var latestObservation time.Time
	for _, observed := range priorObservations {
		if observed.After(latestObservation) {
			latestObservation = observed
		}
	}
	orderingCtx, cancelOrdering := codexRolloutCandidateOrderingContext(ctx)
	var reserves []codexRolloutReserve
	if cursor.boundaryCursor != "" && cursor.mtimeNs > 0 {
		reserves = append(reserves, codexRolloutBoundaryReserve(cursor.mtimeNs))
	}
	if cursor.futureFingerprint != "" && !cursor.futureComplete {
		// Resolve the cohort against the same listing the eligibility filter used,
		// so the reserve covers exactly the members that filter left unconsumed.
		if anchor, floorNs, ceilingNs, matches := codexRolloutFutureCohort(candidates, cursor, now); matches {
			reserves = append(reserves, codexRolloutFutureCohortReserve(anchor, floorNs, ceilingNs))
		}
	}
	if backlogCutoff, resuming := codexRolloutBacklogCutoff(candidates, cursor, now); resuming {
		// Same reasoning as the cohort above, resolved against the same listing:
		// reserve the members this cursor still has to walk so a cap-sized batch of
		// newer rollouts cannot take every slot on every refresh.
		reserves = append(reserves, codexRolloutBacklogReserve(
			backlogCutoff, cursor.mtimeNs, cursor.backlogMtimeNs, now,
		))
	}
	selected, selectionComplete := codexSelectNewestRolloutCandidates(
		orderingCtx, eligibleCandidates, now, codexRolloutScanFileCap, reserves,
		codexRolloutRetrySet(cursor.retryEntries), cursor.retryCursor, cursor.retryFingerprint,
	)
	cancelOrdering()
	progressComplete := discoveryComplete && selectionComplete
	attempted := make([]codexRolloutCandidate, 0, len(selected))
	retryEntries := codexRolloutRetrySet(cursor.retryEntries)
	if discoveryComplete {
		present := make(map[string]struct{}, len(candidates))
		for _, candidate := range candidates {
			present[codexRolloutBacklogEntryDigest(candidate)] = struct{}{}
		}
		for entry := range retryEntries {
			if _, ok := present[entry]; !ok {
				delete(retryEntries, entry)
			}
		}
	}
	for i, c := range selected {
		if ctx.Err() != nil {
			progressComplete = false
			break
		}
		attempted = append(attempted, c)
		fileCtx, cancelFile := codexRolloutFileContext(ctx, len(selected)-i-1)
		buckets, sessionStart, fileLimit, handled, ok := codexBucketsFromRolloutFile(fileCtx, c.path, now)
		cancelFile()
		retryEntry := codexRolloutBacklogEntryDigest(c)
		retry := !handled
		// A fully read file with neither numeric nor refusal evidence has nothing
		// account-scoped to merge. It is safe to count as handled even when its
		// first record does not provide a usable session timestamp.
		if sessionStart.IsZero() && handled && !ok && fileLimit.At.IsZero() {
			delete(retryEntries, retryEntry)
			continue
		}
		// Reject logs whose session began before the current login (a possible
		// prior account). When a login watermark exists, a log with no verified
		// start time can't be scoped, so withhold its evidence and retry it. This
		// matters when cancellation interrupts a large session_meta header: tail
		// telemetry must not cross an account boundary without a verified start.
		// Applied BEFORE the no-buckets skip so exhaustion evidence is scoped to
		// the current account exactly as usage readings are.
		if !authMod.IsZero() {
			accept, authRetry := codexRolloutSessionMatchesAuth(sessionStart, authMod, handled)
			retry = retry || authRetry
			if retry {
				retryEntries[retryEntry] = struct{}{}
			} else {
				delete(retryEntries, retryEntry)
			}
			if !accept {
				continue
			}
		} else if retry {
			retryEntries[retryEntry] = struct{}{}
		} else {
			delete(retryEntries, retryEntry)
		}
		if fileLimit.At.After(limit.At) {
			limit = fileLimit
			limitEntry = retryEntry
		}
		if !ok {
			continue
		}
		for w, limits := range buckets {
			for limitID, b := range limits {
				if observed := time.UnixMilli(b.ObservedAtMs); b.ObservedAtMs > 0 && observed.After(latestObservation) {
					latestObservation = observed
				}
				key := codexWindowIdentity(b.WindowMinutes, w) + "\x00" + limitID
				if prev, exists := winners[key]; !exists || codexRolloutReadingBeats(b, prev.bucket, runFloor) {
					winners[key] = rolloutContribution{slot: w, limitID: limitID, bucket: b}
				}
			}
		}
		// Do NOT stop once both display identities are present. Winners are keyed by
		// (identity, limit id), and this accumulation exists precisely to backfill
		// distinct metered limits that newer logs never restated. Breaking on
		// identity presence would stop before an older log's separate, stricter
		// weekly limit is seen, dropping it from the weekly identity's
		// most-constrained aggregate and understating usage. The scan is already
		// bounded by codexRolloutScanFileCap, so keep folding every in-cap log and
		// let newest-first (identity, limit) dedup keep the freshest reading per
		// contributor.
	}
	if limitEntry != "" && !limit.At.IsZero() && now.Sub(limit.At) <= codexUsageLimitNoticeMaxAge &&
		(latestObservation.IsZero() || latestObservation.Before(limit.At)) {
		// A refusal has no normalized contributor to persist, so its rollout must
		// stay readable while the notice is still live and newer than every numeric
		// observation in the selected set. Hold ONLY that identity above the
		// completed-scan watermark instead of voiding the pass: forcing the scan
		// incomplete discards the boundary/backlog/retry cursors too, so a backlog
		// larger than the file cap would re-select the same newest batch on every
		// refresh for the notice's full lifetime and never reach an older rollout
		// holding a distinct session or weekly contributor. Retry identities bypass
		// the watermark in both discovery and the eligibility filter, and this one
		// is dropped again by the read loop on the first later pass where newer
		// telemetry supersedes the refusal or it ages out.
		retryEntries[limitEntry] = struct{}{}
	}
	var highWater *codexRolloutScanProgress
	if progressComplete {
		maxSelectedMtimeNs := int64(0)
		for _, c := range attempted {
			if c.mtime.UnixNano() > maxSelectedMtimeNs {
				maxSelectedMtimeNs = c.mtime.UnixNano()
			}
		}
		backlogFingerprint, backlogCursor, backlogMtimeNs, backlogCohortSize, backlogComplete :=
			codexRolloutBacklogProgress(candidates, eligibleCandidates, attempted, now, cursor)
		if !backlogComplete {
			// Keep the completed high-water below every member of this stable
			// cohort. The redacted rank cursor excludes this pass's newest files on
			// the next refresh so older distinct-mtime candidates get their turn.
			maxSelectedMtimeNs = cursor.mtimeNs
		} else if cursor.backlogCursor != "" {
			// The final batch may contain only the cohort's oldest file. Advance to
			// the newest mtime from the full, unchanged cohort now that every member
			// has been handled across passes.
			maxSelectedMtimeNs = codexRolloutNewestNormalMtimeNs(candidates, cursor.mtimeNs, now)
		}
		// Filesystem mtimes are progress hints, not provider observation times.
		// Never let a restored/future-dated file move the cursor beyond the
		// current clock and suppress normally timestamped rollouts written next.
		// Retain the coarsest common mtime-resolution overlap: a rollout created
		// after its directory was enumerated can otherwise round below now while
		// an already-enumerated active rollout advances above now, causing the
		// clamped cursor to skip the undiscovered file forever.
		if nowNs := now.UnixNano(); maxSelectedMtimeNs > nowNs {
			maxSelectedMtimeNs = now.Add(-codexRolloutCoarseMtimeOverlap).UnixNano()
		}
		// Clamping a future-only batch must not move an already completed normal
		// cursor backwards. Keeping its current value also lets the transaction
		// persist the separate future-file state under the monotonic write guard.
		if maxSelectedMtimeNs < cursor.mtimeNs {
			maxSelectedMtimeNs = cursor.mtimeNs
		}
		// A saved equal-mtime boundary can share a capped pass with rollouts newer
		// than that boundary. Do not let those newer files pull the main watermark
		// past boundary entries that still did not fit: pin progress to the saved
		// boundary until its remaining deterministic batches have been consumed.
		hasSavedBoundary := cursor.boundaryCursor != "" && cursor.mtimeNs > 0
		unfinishedSavedBoundary := hasSavedBoundary &&
			codexRolloutBoundaryHasUnselected(eligibleCandidates, attempted, cursor.mtimeNs)
		deferredNewerCandidate := hasSavedBoundary &&
			codexRolloutHasUnselectedBelowProgress(
				eligibleCandidates, attempted, cursor.mtimeNs, maxSelectedMtimeNs,
			)
		if unfinishedSavedBoundary || deferredNewerCandidate {
			// The reserved boundary entries can evict newer candidates from the
			// ordinary top-N selection. Even when this pass finishes the saved
			// boundary, retain its high-water once so the next pass can consume the
			// newer entries without a reservation before progress moves beyond them.
			maxSelectedMtimeNs = cursor.mtimeNs
		}
		// Record a redacted cursor for the equal-mtime entries this pass opened. A
		// later pass resumes after it before applying the cap, so a large coarse-
		// mtime boundary advances through distinct batches instead of selecting the
		// same deterministic newest subset forever. Once the whole boundary is
		// consumed, the cursor is cleared and its full fingerprint restores the
		// ordinary cache-only unchanged-boundary fast path.
		futureAnchorNs, futureFloorNs, futureCeilingNs, futureFingerprint, futureCursor,
			futureCohortSize, futureComplete :=
			codexRolloutFutureProgress(candidates, eligibleCandidates, attempted, now, cursor)
		if cursor.futureFingerprint != "" && futureFingerprint != "" && !futureComplete {
			// A cohort that was future-dated when its capped scan started may be
			// normal-time by the next refresh. Keep the main high-water below that
			// unfinished cohort so its older unread entries cannot be skipped.
			maxSelectedMtimeNs = cursor.mtimeNs
		}
		retireFutureCohort := backlogComplete && !unfinishedSavedBoundary && !deferredNewerCandidate &&
			codexRolloutFutureCohortCaughtUp(candidates, now, futureFingerprint, futureComplete)
		if retireFutureCohort {
			maxSelectedMtimeNs = codexRolloutNewestNormalMtimeNs(candidates, cursor.mtimeNs, now)
			futureAnchorNs, futureFloorNs, futureCeilingNs, futureFingerprint, futureCursor,
				futureCohortSize, futureComplete = 0, 0, 0, "", "", 0, false
		}
		progress := codexRolloutBoundaryProgress(candidates, eligibleCandidates, attempted, maxSelectedMtimeNs)
		if unfinishedSavedBoundary && progress.boundaryCursor == "" &&
			codexRolloutBoundaryFingerprint(candidates, cursor.mtimeNs) == cursor.boundaryFingerprint {
			// Newer files can fill the whole cap before this pass reaches the saved
			// boundary. Keep its prior position rather than restarting or clearing it.
			progress.boundaryFingerprint = cursor.boundaryFingerprint
			progress.boundaryCursor = cursor.boundaryCursor
		}
		progress.futureAnchorNs = futureAnchorNs
		progress.futureFloorNs = futureFloorNs
		progress.futureCeilingNs = futureCeilingNs
		progress.backlogFingerprint = backlogFingerprint
		progress.backlogCursor = backlogCursor
		progress.backlogMtimeNs = backlogMtimeNs
		progress.backlogCohortSize = backlogCohortSize
		progress.retryEntries = codexRolloutRetryList(retryEntries)
		progress.retryFingerprint, progress.retryCursor = codexRolloutRetryRotationProgress(
			eligibleCandidates, attempted, retryEntries,
		)
		progress.futureFingerprint = futureFingerprint
		progress.futureCursor = futureCursor
		progress.futureCohortSize = futureCohortSize
		progress.futureComplete = futureComplete
		highWater = &progress
	}
	if len(winners) == 0 {
		// No usable window anywhere in the scanned logs — but a quota refusal
		// found along the way still explains WHY, so it is reported even though
		// there is nothing to backfill.
		return nil, limit, latestObservation, highWater, false
	}
	// Rebuild the slot-keyed contributor map downstream expects. Normalize the
	// displayed identities onto their canonical cache slots while preserving the
	// provider's real limit id. Codex can transiently emit both a session and
	// weekly reading under `primary`; inventing a synthetic key for one would no
	// longer match a later sparse update for the real limit, leaving stale evidence
	// behind. Non-canonical durations retain their source slot so the display path
	// can apply its slot-scoped fallback.
	acc := map[string]map[string]codexRateLimitBucket{}
	for _, c := range winners {
		slot := c.slot
		switch codexWindowIdentity(c.bucket.WindowMinutes, c.slot) {
		case codexIdentitySession:
			slot = codexWindowPrimary
		case codexIdentityWeekly:
			slot = codexWindowSecondary
		}
		slotMap := acc[slot]
		if slotMap == nil {
			slotMap = map[string]codexRateLimitBucket{}
			acc[slot] = slotMap
		}
		limitKey := c.limitID
		if _, taken := slotMap[limitKey]; taken {
			// Only non-canonical duration identities can still collide after the
			// displayed rows were normalized above. Preserve both rather than
			// silently dropping one; these plan-specific fallback rows have no
			// stable canonical slot available.
			limitKey += "\x00" + codexWindowIdentity(c.bucket.WindowMinutes, c.slot)
		}
		slotMap[limitKey] = c.bucket
	}
	return acc, limit, latestObservation, highWater, true
}

// codexRolloutReadingBeats orders two readings of the same (identity, limit)
// found in one scan: newest wins, except that an inferred reading never beats a
// stated one that already covers the run floor, and loses every tie.
func codexRolloutReadingBeats(b, prev codexRateLimitBucket, runFloor time.Time) bool {
	if b.Inferred != prev.Inferred && !runFloor.IsZero() {
		stated := prev
		if !b.Inferred {
			stated = b
		}
		if stated.ObservedAtMs >= runFloor.UnixMilli() {
			return !b.Inferred
		}
	}
	if b.ObservedAtMs != prev.ObservedAtMs {
		return b.ObservedAtMs > prev.ObservedAtMs
	}
	return prev.Inferred && !b.Inferred
}

func codexRolloutSessionMatchesAuth(sessionStart, authMod time.Time, handled bool) (accept, retry bool) {
	if sessionStart.IsZero() {
		// An authenticated scan must prove which account produced the rollout.
		// Withhold unscoped evidence in either case, but only an interrupted read
		// needs retrying. A fully consumed malformed/legacy file is deterministic;
		// a later append changes its discovery fingerprint and makes it eligible
		// again without holding back completed-scan progress indefinitely.
		return false, !handled
	}
	return !sessionStart.Before(authMod), false
}

// codexBucketsFromRolloutFile returns the per-(window, limit) contributors from
// the LAST populated `rate_limits` frame in a single rollout log, plus the
// session's start time (the first line's `timestamp`). Codex emits
// `rate_limits: null` on most token_count events and the real object only
// periodically, so the last non-empty extraction — not the first — is the live
// reading. Contributors are returned un-aggregated so the caller can partition
// them by identity; reset-passed rollover to 0% is applied downstream per
// contributor (codexAggregateIdentity), matching aggregateCodexBuckets. The
// start time is used by the caller to scope logs to the current account.
// Best-effort: returns ok=false when the file holds no usable frame or can't be
// read; the returned start time is zero when no line carried a parseable
// timestamp.
const (
	codexRolloutTailReadChunkSize = 64 * 1024
	codexRolloutTailProbeMaxBytes = 4 * 1024 * 1024
)

// codexRecentRolloutLines probes backwards from EOF up to a fixed byte ceiling.
// The normal forward scan
// still runs and remains authoritative for sparse carry-forward, session
// scoping, and completed-scan progress. This small second view prevents a
// repeatedly slow/large file from replaying only the same prefix forever while
// ensuring a sparse newest frame does not hide the other identity immediately
// before it.
func codexRecentRolloutLines(ctx context.Context, f *os.File, size int64, now time.Time) []string {
	if size <= 0 {
		return nil
	}
	offset := size
	remaining := int64(codexRolloutTailProbeMaxBytes)
	var suffix []byte
	var groups [][]string
	for offset > 0 && remaining > 0 {
		if ctx.Err() != nil {
			break
		}
		readSize := int64(codexRolloutTailReadChunkSize)
		if readSize > offset {
			readSize = offset
		}
		if readSize > remaining {
			readSize = remaining
		}
		offset -= readSize
		remaining -= readSize
		chunk := make([]byte, readSize)
		n, err := f.ReadAt(chunk, offset)
		if err != nil && err != io.EOF {
			break
		}
		chunk = chunk[:n]
		combined := make([]byte, 0, len(chunk)+len(suffix))
		combined = append(combined, chunk...)
		combined = append(combined, suffix...)
		parts := bytes.Split(combined, []byte{'\n'})
		complete := parts
		if offset > 0 {
			// The first fragment begins before this chunk. Retain it for the
			// next backwards read; only newline-delimited suffixes are complete.
			suffix = append(suffix[:0], parts[0]...)
			complete = parts[1:]
		} else {
			suffix = nil
		}
		group := make([]string, 0, len(complete))
		for _, rawLine := range complete {
			rawLine = bytes.TrimSuffix(rawLine, []byte{'\r'})
			if len(rawLine) == 0 {
				continue
			}
			line := string(rawLine)
			group = append(group, line)
		}
		if len(group) > 0 {
			groups = append(groups, group)
		}
	}

	// Groups were discovered newest-to-oldest; replay them chronologically so
	// sparse frames retain the same semantics as the forward reader.
	var lines []string
	for i := len(groups) - 1; i >= 0; i-- {
		lines = append(lines, groups[i]...)
	}
	return lines
}

// codexRolloutSessionMetaType is the `type` Codex stamps on the session header
// it writes as a rollout's first record. Every telemetry record carries a
// different type (`event_msg`, `response_item`, `turn_context`, `compacted`).
const codexRolloutSessionMetaType = "session_meta"

// codexRolloutSessionStartFromLine reads a rollout's session start from its
// FIRST record, and only when that record is the session header.
//
// The start time is an ACCOUNT SCOPING claim, not merely a timestamp:
// codexRolloutSessionMatchesAuth accepts a rollout whose start is at or after
// the current login watermark. A prior-account process that keeps appending
// after a new login writes telemetry stamped AFTER that watermark, so accepting
// the first record of a truncated, rotated, or legacy log purely because it
// parses would let that account's quota be cached under the new fingerprint.
// Only the header proves whose session produced the file; anything else leaves
// the start unverified, which the auth check already treats as "withhold this
// file's evidence" rather than as permission to merge it.
func codexRolloutSessionStartFromLine(line string) (time.Time, bool) {
	var envelope struct {
		Type *string          `json:"type"`
		ID   *json.RawMessage `json:"id"`
	}
	if json.Unmarshal([]byte(line), &envelope) != nil {
		return time.Time{}, false
	}
	if envelope.Type != nil {
		if *envelope.Type != codexRolloutSessionMetaType {
			return time.Time{}, false
		}
	} else if envelope.ID == nil {
		// Rollouts predating the typed envelope open with a bare session header
		// carrying the session `id` and no `type`. With neither marker the record
		// is telemetry (or unrecognizable), so it cannot scope the file.
		return time.Time{}, false
	}
	return codexRolloutLineTimestamp(line)
}

func codexRolloutSessionStartPrefix(f *os.File) time.Time {
	buf := make([]byte, codexRolloutTailReadChunkSize)
	n, err := f.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return time.Time{}
	}
	line := buf[:n]
	if newline := bytes.IndexByte(line, '\n'); newline >= 0 {
		line = line[:newline]
	}
	if ts, ok := codexRolloutSessionStartFromLine(string(bytes.TrimSuffix(line, []byte{'\r'}))); ok {
		return ts
	}
	return time.Time{}
}

var codexOpenRolloutFile = os.Open

func codexBucketsFromRolloutFile(ctx context.Context, path string, now time.Time) (map[string]map[string]codexRateLimitBucket, time.Time, codexUsageLimitEvidence, bool, bool) {
	f, err := codexOpenRolloutFile(path)
	if err != nil {
		// Only a file that definitively vanished is handled progress. Permission
		// failures, descriptor exhaustion, and other open errors can be transient;
		// advancing the watermark past them would suppress a later successful read.
		return nil, time.Time{}, codexUsageLimitEvidence{}, codexRolloutOpenFailureHandled(err), false
	}
	defer f.Close()

	sessionStart := codexRolloutSessionStartPrefix(f)
	inferredAt := codexRolloutInferredObservation(ctx, f, now)
	var limit codexUsageLimitEvidence
	acc := map[string]map[string]codexRateLimitBucket{}
	// An inferred anchor is built from the file's mtime, which describes the
	// NEWEST append and nothing else. Stamping every timestamp-less frame with
	// it would republish an old percentage from an earlier turn as a post-run
	// observation — wrongly settling the freshness debt — so inference is
	// narrowed to the one frame that can honestly claim the mtime: a
	// timestamp-less frame that is the file's FINAL record, in a file where NO
	// frame stated a time at all.
	//
	// That second condition is what keeps an ordinary multi-turn rollout out of
	// this path. A build that stamps its envelopes produces stated frames, so a
	// lone timestamp-less frame among them is an anomaly from an earlier turn,
	// not the finished run's evidence, and stays dropped exactly as on a routine
	// scan. Only a file that is timestamp-less throughout — the renamed-key
	// build this fallback exists for — has no stated time to prefer, and there
	// the mtime is the sole anchor available. It stays clamped to
	// [runFloor, now] and flagged Inferred, so it can never outrank or back-date
	// a stated observation.
	var pendingInferred map[string]interface{}
	statedFrameSeen := false
	finalRecordSeen := false
	consumeLine := func(line string) {
		// Any later record — telemetry or not — invalidates a pending candidate.
		// The mtime describes the file's newest append and nothing else, so only
		// the file's FINAL record can honestly claim it. Appending a completion
		// event or a reasoning item for the latest run advances the mtime while
		// leaving an older numeric frame behind it; stamping that old percentage
		// with the new mtime would settle the run's debt with evidence that
		// predates it.
		pendingInferred = nil
		// Exhaustion evidence is collected from the SAME pass, ahead of the
		// rate-limit prefilter: a refused turn carries no window at all (Codex
		// sends `primary: null, secondary: null` once the limit is reached), so
		// these lines are exactly the ones the bucket scan discards.
		if ev, ok := codexUsageLimitEvidenceFromLine(line, now); ok && ev.At.After(limit.At) {
			limit = ev
		}
		// Cheap prefilter: only decode lines that could carry a window update.
		// Mirror captureCodexRateLimitLine's gate exactly so camelCase frames
		// (`rateLimits` / `rateLimitsByLimitId`) the extractor supports aren't
		// dropped — `rate_limit` alone wouldn't match the camelCase spelling.
		if !strings.Contains(line, "token_count") &&
			!strings.Contains(line, "rateLimits") &&
			!strings.Contains(line, "rate_limit") {
			return
		}
		var raw map[string]interface{}
		if json.Unmarshal([]byte(line), &raw) != nil {
			return
		}
		if !isRecognizedCodexRateLimitEnvelope(raw) {
			return
		}
		// Anchor relative reset fields (`resets_in_seconds`) to the moment the
		// line was EMITTED, not the usage-refresh time. A historical rollout
		// line saying "resets in 3600s" reset an hour after it was written; with
		// the refresh time as the anchor it would falsely look like it resets an
		// hour from now, masking a window that has long since rolled over.
		// Absolute `resets_at` fields ignore this anchor, so the fallback is
		// when the line carries no parseable timestamp.
		eventTime, observedAt := codexObservationTimes(raw, now, false)
		if eventTime.IsZero() {
			// Numeric rollout telemetry without an enclosing event time cannot
			// advance freshness on a routine scan. Drop this object only and
			// continue later lines — unless a forced post-run reconcile vouched
			// for this file (written at/after the run floor), in which case the
			// file's own mtime may stand in for the LAST such frame, flagged
			// Inferred so it never outranks a stated observation. A Codex build
			// that renames its envelope timestamp key would otherwise leave the
			// card stale after every run.
			if !inferredAt.IsZero() {
				pendingInferred = raw
			}
			return
		}
		statedFrameSeen = true
		// The rollout shape nests telemetry under `payload`
		// ({"type":"event_msg","payload":{"type":"token_count","rate_limits":…}}),
		// which extractCodexRateLimitBuckets already unwraps. fullSnapshot is
		// false for that envelope, so null windows are ignored rather than
		// treated as clears — exactly what we want when mining for live usage.
		if updates, _ := extractCodexRateLimitBuckets(raw, eventTime); len(updates) > 0 {
			codexStampContributorObservations(updates, observedAt)
			updates = codexCanonicalizeContributors(updates)
			// Merge, don't replace: token_count notifications are sparse, so a
			// later frame restating only `primary` must not drop a `secondary`
			// reading an earlier frame in this same file already captured.
			// Liveness for sparse-merge is judged at the frame's own event
			// time so an expired prior reset isn't carried onto fresh usage.
			mergeCodexRolloutFrame(acc, updates, eventTime)
		}
	}
	var size int64
	if info, statErr := f.Stat(); statErr == nil {
		size = info.Size()
	}
	tailLines := codexRecentRolloutLines(ctx, f, size, now)
	consumeTail := func() {
		// The tail lines ARE the file's trailing records, so whatever survives
		// this pass is anchored at the end of the file.
		if len(tailLines) > 0 {
			finalRecordSeen = true
		}
		for _, line := range tailLines {
			consumeLine(line)
		}
		tailLines = nil
	}

	// Scanner cannot recover after an oversized token and cannot be canceled
	// while accumulating it. Read in bounded fragments instead: complete JSONL
	// objects up to the existing 30 MB ceiling are consumed, oversized objects
	// are discarded through their newline, and cancellation preserves earlier
	// complete evidence while leaving the file unhandled for retry.
	reader := bufio.NewReaderSize(f, 64*1024)
	lineBytes := make([]byte, 0, 64*1024)
	oversized := false
	firstRecordSeen := false
	headerUnread := false
	handled := true
	for {
		if ctx.Err() != nil {
			handled = false
			break
		}
		fragment, readErr := reader.ReadSlice('\n')
		if !oversized {
			if len(lineBytes)+len(fragment) > codexAppServerMaxLineSize {
				lineBytes = lineBytes[:0]
				oversized = true
			} else {
				lineBytes = append(lineBytes, fragment...)
			}
		}
		if readErr == bufio.ErrBufferFull {
			continue
		}
		if readErr != nil && readErr != io.EOF {
			handled = false
			break
		}
		if oversized && !firstRecordSeen {
			// The discarded object may have been the session_meta header. Do not
			// promote a later telemetry timestamp to the session start: an older
			// account can keep emitting after a new login, and that later event
			// would make its rollout appear to belong to the new account. Keep the
			// start unverified AND report the file as unhandled: unlike a malformed
			// header, whose bytes were read and judged, this record was never
			// decoded at all, so the scan cannot claim it as deterministic progress
			// and the rollout stays above the cursor for retry.
			firstRecordSeen = true
			headerUnread = true
		}
		if !oversized && len(lineBytes) > 0 {
			for len(lineBytes) > 0 && (lineBytes[len(lineBytes)-1] == '\n' || lineBytes[len(lineBytes)-1] == '\r') {
				lineBytes = lineBytes[:len(lineBytes)-1]
			}
			if len(lineBytes) > 0 {
				line := string(lineBytes)
				if !firstRecordSeen {
					firstRecordSeen = true
					if ts, ok := codexRolloutSessionStartFromLine(line); ok {
						sessionStart = ts
					}
				}
				consumeLine(line)
			}
		}
		lineBytes = lineBytes[:0]
		oversized = false
		if readErr == io.EOF {
			// The forward pass reached the end of the file, so the last record
			// it consumed is the file's final one.
			finalRecordSeen = true
			break
		}
	}
	if ctx.Err() != nil || headerUnread {
		handled = false
	}
	// Tail lines were already read as complete objects. Fold them even when the
	// forward pass was interrupted; their newer timestamps supersede any prefix
	// evidence without treating an incomplete fragment as provider telemetry.
	consumeTail()
	// Fold the anchored frame last: it is the file's final record, in a file that
	// stated no times at all, so the sparse-merge rules in mergeCodexRolloutFrame
	// treat it exactly as they would a stated frame at that time, and
	// codexRolloutReadingBeats still lets any stated observation covering the run
	// floor outrank it.
	if pendingInferred != nil && !statedFrameSeen && finalRecordSeen {
		if updates, _ := extractCodexRateLimitBuckets(pendingInferred, inferredAt); len(updates) > 0 {
			codexStampContributorObservations(updates, inferredAt)
			codexMarkContributorsInferred(updates)
			updates = codexCanonicalizeContributors(updates)
			mergeCodexRolloutFrame(acc, updates, inferredAt)
		}
	}
	if !handled {
		return acc, sessionStart, limit, false, len(acc) > 0
	}
	if len(acc) == 0 {
		// ok=false means "no usable window here", NOT "nothing here": a log whose
		// every turn was refused for quota is precisely the case that produces no
		// buckets AND the evidence the card needs, so the evidence is returned
		// alongside the miss.
		return nil, sessionStart, limit, true, false
	}
	// Return the per-limit contributors un-collapsed. Rollover-to-0% for a window
	// whose reset already passed as of `now` is applied by the display path
	// (codexAggregateIdentity), so a stale relative reset anchored above still
	// clears instead of showing old usage — without flattening two distinct
	// identities that share a storage slot into one bucket here.
	return acc, sessionStart, limit, true, true
}

func codexRolloutOpenFailureHandled(err error) bool {
	return os.IsNotExist(err)
}

// codexUsageLimitEvidence records that Codex refused a turn because the
// account's quota was exhausted, as seen in a rollout log. `At` is the event's
// own timestamp (zero = no evidence); `Message` is Codex's own user-facing text,
// which names the retry time and the top-up link.
type codexUsageLimitEvidence struct {
	At      time.Time
	Message string
}

// codexUsageLimitErrorCode is the `codex_error_info` value Codex attaches to a
// turn it refused because the account is out of quota.
const codexUsageLimitErrorCode = "usage_limit_exceeded"

// codexUsageLimitMessageMaxLen bounds how much of Codex's message the card may
// carry. Long enough for the real text ("You've hit your usage limit. Visit
// <url> to purchase more credits or try again at 11:28 PM."), short enough that
// a future upstream paragraph can't bloat every device document.
const codexUsageLimitMessageMaxLen = 240

// codexUsageLimitEvidenceFromLine extracts quota-exhaustion evidence from one
// rollout JSONL line. Codex reports it on the turn's terminal event:
//
//	{"timestamp":…,"type":"event_msg","payload":{"type":"task_complete",
//	  "error":{"message":"You've hit your usage limit…",
//	           "codex_error_info":"usage_limit_exceeded"}}}
//
// The code — not the prose — is what we match on, so a reworded message keeps
// working and a message merely MENTIONING a limit is never mistaken for one.
func codexUsageLimitEvidenceFromLine(line string, now time.Time) (codexUsageLimitEvidence, bool) {
	// Cheap prefilter: the code is a literal, so a line that doesn't contain it
	// cannot be evidence and is never decoded.
	if !strings.Contains(line, codexUsageLimitErrorCode) {
		return codexUsageLimitEvidence{}, false
	}
	var raw map[string]interface{}
	if json.Unmarshal([]byte(line), &raw) != nil {
		return codexUsageLimitEvidence{}, false
	}
	_, observedAt := codexObservationTimes(raw, now, false)
	if observedAt.IsZero() {
		// Without a valid timestamp the evidence can't be ranked against a usage
		// reading, and stale or excessively future-dated exhaustion must never
		// outrank fresh telemetry. Accepted provider skew is clamped to now by the
		// shared observation-time policy.
		return codexUsageLimitEvidence{}, false
	}
	// The rollout envelope nests the event under `payload`; the app-server
	// streams the same object at the top level.
	scopes := []map[string]interface{}{raw}
	if payload, ok := raw["payload"].(map[string]interface{}); ok {
		scopes = append(scopes, payload)
	}
	for _, scope := range scopes {
		errObj, ok := scope["error"].(map[string]interface{})
		if !ok {
			continue
		}
		code, _ := pickField(errObj, "codex_error_info", "codexErrorInfo")
		if s, _ := code.(string); s != codexUsageLimitErrorCode {
			continue
		}
		message, _ := errObj["message"].(string)
		message = strings.TrimSpace(message)
		if len(message) > codexUsageLimitMessageMaxLen {
			message = strings.TrimSpace(message[:codexUsageLimitMessageMaxLen])
		}
		return codexUsageLimitEvidence{At: observedAt, Message: message}, true
	}
	return codexUsageLimitEvidence{}, false
}

// codexRolloutLineTimestamp extracts the top-level `timestamp` (RFC3339) from
// one rollout JSONL line. Used to read a session's start time without decoding
// the whole line. ok=false when the line has no parseable timestamp.
func codexRolloutLineTimestamp(line string) (time.Time, bool) {
	var probe struct {
		Timestamp string `json:"timestamp"`
	}
	if json.Unmarshal([]byte(line), &probe) != nil || probe.Timestamp == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, probe.Timestamp)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// mergeCodexRolloutFrame folds one frame's per-(window, limit) contributors into
// the accumulated snapshot for a single rollout file, latest-wins, mirroring the
// live cache's sparse-merge semantics:
//
//   - A reset-only update within the SAME LIVE window (prior reset still in the
//     future as of this frame's event time, and within jitter of the new reset)
//     carries the prior usage forward; a usage-only update keeps the prior reset
//     only while it's still live. Window-length hints survive from either side.
//   - A bucket with no known usage is NEVER stored as a standalone observed
//     contributor — the live path ignores reset-only updates that have no prior
//     same-window usage to merge into, and so do we. Storing one would make
//     codexMetricFromBucket report a bogus 0% and block an older file
//     from filling the real usage.
//   - When a reset-only update jumps to a NEW window (reset beyond jitter), the
//     prior reading is stale: drop it so it can't keep rendering an expired
//     percentage, and leave the window unobserved until a real usage frame lands.
//   - A usage-only update arriving after the prior window has already expired
//     stands on its own — copying the expired prev reset would make
//     codexMetricFromBucket zero out the fresh usage as rolled over.
//
// Windows/limits the frame doesn't mention are left untouched (rollout frames
// never clear). `frameTime` is the line's own timestamp so liveness is judged
// at the moment the frame was emitted, not at refresh time.
func mergeCodexRolloutFrame(acc, updates map[string]map[string]codexRateLimitBucket, frameTime time.Time) {
	frameMs := frameTime.UnixMilli()
	for window, contributors := range updates {
		for limit, b := range contributors {
			var prev codexRateLimitBucket
			hadPrev := false
			if acc[window] != nil {
				prev, hadPrev = acc[window][limit]
			}
			priorStillLive := hadPrev && prev.resetKnown && prev.ResetsAtMs > frameMs
			sameWindow := hadPrev && (!b.resetKnown || !prev.resetKnown ||
				resetsWithinJitter(b.ResetsAtMs, prev.ResetsAtMs))
			sameLiveWindow := priorStillLive && (!b.resetKnown || resetsWithinJitter(b.ResetsAtMs, prev.ResetsAtMs))

			if !b.usageKnown && sameLiveWindow && prev.usageKnown {
				b.UsedPercentage = prev.UsedPercentage
				// The sparse frame re-observed the reset, not utilization. Keep the
				// carried percentage paired with its original observation time so
				// repeated rollout heartbeats cannot make stale usage appear fresh.
				b.ObservedAtMs = prev.ObservedAtMs
				b.Inferred = prev.Inferred
				b.usageKnown = true
			}
			if !b.resetKnown && priorStillLive {
				b.ResetsAtMs = prev.ResetsAtMs
				b.resetKnown = true
			}
			if b.WindowMinutes == 0 && hadPrev && prev.WindowMinutes > 0 {
				b.WindowMinutes = prev.WindowMinutes
			}

			if !b.usageKnown {
				// Reset-only update with no usage to anchor it. Drop a stale
				// prior when the window rolled over; otherwise ignore.
				if hadPrev && b.resetKnown && !sameWindow {
					delete(acc[window], limit)
					if len(acc[window]) == 0 {
						delete(acc, window)
					}
				}
				continue
			}
			if acc[window] == nil {
				acc[window] = map[string]codexRateLimitBucket{}
			}
			acc[window][limit] = b
		}
	}
}
