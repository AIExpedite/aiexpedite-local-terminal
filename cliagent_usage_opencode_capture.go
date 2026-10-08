// cliagent_usage_opencode_capture.go — OpenCode's device-local usage ledger.
//
// OpenCode has no quota of its own and no usage command, but every turn
// reports what it spent. The ledger, opencode_usage.json, has two producers:
//
//   - stream capture (this file): every `opencode run --format json` turn this
//     agent spawns — the `__cli_smoke__` probe, the resident chat
//     (opencode_native.go) and terminal sessions (session.go) — closes each
//     step with a `step_finish` frame whose part carries `tokens` and `cost`.
//   - the direct-run reader (cliagent_usage_opencode_direct.go): the assistant
//     messages OpenCode persists in its own store, for the runs the agent did
//     not spawn — the user's shell or IDE, and the TUI in a terminal-managed
//     PTY. It skips every message inside a managed run's ownership window
//     (OwnedRuns), so no turn is counted by both.
//
// cliagent_usage_opencode.go publishes the result as "Tokens today" (or
// "Tokens today (agent runs)" while the store cannot be read) and "Cost today".
//
// Shape and invariants:
//
//   - one bucket per (account fingerprint, local calendar date), capped at
//     openCodeUsageMaxBuckets, oldest observation dropped first. Values only
//     move forward; a negative or non-finite token or cost rejects the whole
//     frame, never clamped.
//   - "Tokens" is input + output + reasoning. Cache read/write are kept per
//     bucket but not published: on a long session they dwarf real usage.
//   - a step counts once: its key is sha256(sessionID NUL part id), truncated,
//     remembered in seenSteps (capped). A frame with no part id keys on its
//     message id plus end time; one with neither counts without dedup.
//   - the generation advances only when a write changes a bucket, under this
//     process's epoch (codexProcessGenerationEpoch — one random epoch per agent
//     process). A committed advance hints the backend
//     (noteCLIUsageObservationAdvanced).
//
// Parsing a frame touches memory only: the stream goroutines never wait on the
// ledger. A run's steps are committed once, when it settles
// (cliagent_usage_opencode_freshness.go).
//
// SECRETS: the ledger holds counts, hashed step keys and the account
// fingerprint. The only raw identifier is a debt's OpenCode session id — a
// local opaque id the export fallback needs, never a credential — and it stays
// in this 0600 file. Logs are fixed labels only.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	openCodeUsageProvider      = "opencode"
	openCodeUsageSchemaVersion = 1
	openCodeUsageMaxBuckets    = 16
	// openCodeUsageMaxSeenSteps holds a day of hashed keys for a heavy user
	// (about 150 KB on disk): the direct reader keys every assistant message.
	openCodeUsageMaxSeenSteps = 8192
	openCodeUsageMaxDebts     = 8
	// openCodeUsageMaxOwnedRuns caps the ownership windows. Open windows are
	// bounded by the live runs; past the cap the oldest CLOSED window goes.
	openCodeUsageMaxOwnedRuns = 32
	// openCodeUsageMaxLedgerBytes bounds the ledger read. A full ledger (8,192
	// keys, 16 buckets, 8 debts, 32 windows, indented) is about 230 KB; one that
	// does not fit reads as EMPTY, dropping today's buckets, so the bound keeps
	// several times that headroom.
	openCodeUsageMaxLedgerBytes = 1 << 20
	// openCodeUsageMaxRunSteps bounds one run's in-memory accumulator. Steps past
	// it fold into the last uncommitted one without a dedup key, or recycle the
	// slots of steps already committed: still counted, never unbounded.
	openCodeUsageMaxRunSteps = 4096
)

// openCodeUsageLedger is opencode_usage.json.
type openCodeUsageLedger struct {
	SchemaVersion int                   `json:"schemaVersion"`
	Generation    cliUsageGeneration    `json:"generation"`
	Buckets       []openCodeUsageBucket `json:"buckets,omitempty"`
	Debts         []openCodeUsageDebt   `json:"debts,omitempty"`
	SeenSteps     []string              `json:"seenSteps,omitempty"`
	// The fields below are additive under schemaVersion 1: an older build that
	// rewrites the ledger drops them and keeps the buckets
	// (openCodeDirectScanFloor reads that downgrade signature).
	OwnedRuns []openCodeOwnedRun `json:"ownedRuns,omitempty"`
	// OwnedEvictedMs is the newest end of a closed window evicted before the
	// reader's cursor passed it (pruneOpenCodeOwnedRuns): the reader skips every
	// message created by then, since it can no longer tell which were managed.
	OwnedEvictedMs int64                   `json:"ownedEvictedMs,omitempty"`
	DirectCursor   *openCodeDirectCursor   `json:"directCursor,omitempty"`
	DirectCoverage *openCodeDirectCoverage `json:"directCoverage,omitempty"`

	// seenEvicted records that mergeOpenCodeUsageSteps evicted keys from
	// seenSteps: the direct reader must not rewind before records whose dedup keys
	// rolled over.
	seenEvicted bool
}

// openCodeOwnedRun is one managed run's ownership window: the direct reader
// skips every message of the session created in [FromMs, ToMs], because the
// run's stream (or its export debt) pays those. ToMs 0 is open — the run's
// stream may still commit. The session id is stored hashed.
type openCodeOwnedRun struct {
	RunID      string `json:"runId"`
	SessionKey string `json:"sessionKey"`
	FromMs     int64  `json:"fromMs"`
	ToMs       int64  `json:"toMs,omitempty"`
}

func (w openCodeOwnedRun) open() bool { return w.ToMs == 0 }

// covers reports whether a message of the hashed session created at createdMs
// belongs to the run.
func (w openCodeOwnedRun) covers(sessionKey string, createdMs int64) bool {
	return w.SessionKey == sessionKey && createdMs >= w.FromMs && (w.open() || createdMs <= w.ToMs)
}

// openCodeDirectCursor is where the direct reader resumes: records last
// written at or after ThroughMs (less an overlap) are read again. Continue
// marks a cursor a capped scan saved: the next scan resumes AT ThroughMs, since
// re-reading the overlap could spend the whole cap on records already counted.
// AtSession marks one the session cap alone stopped: the next scan lists
// sessions from ThroughMs without the slack, or the same capped sessions would
// fill the cap again. Skip is the continuation's tie-breaker: how many
// sessions (AtSession) or records last written at ThroughMs the capped scan
// read, so entries sharing that millisecond cannot refill the cap forever.
// RecordFloorMs is a session cut's record floor: the sessions it
// has not listed yet are read from the floor the capped scan used, since their
// messages can predate their session's own last write. SessionSinceMs is a
// record continuation's session listing floor: the one the capped scan listed
// sessions from, kept while it drains, since a session listed then can still
// hold unread records rewritten after the cut while its own last write sits
// before the cut's slack. RewindFloorMs is the end
// of the last drained backlog: a later scan's overlap does not reach before
// it, because a backlog larger than a scan's cap can roll seenSteps over, and
// the keys of the records it read are then no longer there to absorb a re-read.
// RevisitFloorMs is where a continuation returns once it drains: the earliest
// point an incomplete message or a hold pinned a capped scan behind its cut,
// while ThroughMs and Skip keep the cap's own position.
// PinnedKeys are the keys of the messages a scan read at or after ThroughMs —
// ones counted past a cursor an incomplete message or a hold pinned behind
// them: the next scan reads them again, and seenSteps may have rolled their
// keys over by then, so the cursor keeps them itself until it passes them.
type openCodeDirectCursor struct {
	Layout         string   `json:"layout"`
	ThroughMs      int64    `json:"throughMs"`
	Continue       bool     `json:"continue,omitempty"`
	AtSession      bool     `json:"atSession,omitempty"`
	Skip           int      `json:"skip,omitempty"`
	RecordFloorMs  int64    `json:"recordFloorMs,omitempty"`
	SessionSinceMs int64    `json:"sessionSinceMs,omitempty"`
	RewindFloorMs  int64    `json:"rewindFloorMs,omitempty"`
	RevisitFloorMs int64    `json:"revisitFloorMs,omitempty"`
	PinnedKeys     []string `json:"pinnedKeys,omitempty"`
}

// openCodeDirectCoverage records the reader's last scan. LastOkLocalDate is the
// local date of the newest scan that read the store: today's bucket then holds
// direct spend, and the card drops "(agent runs)".
type openCodeDirectCoverage struct {
	Layout          string `json:"layout"`
	ObservedAtMs    int64  `json:"observedAtMs"`
	LastOkLocalDate string `json:"lastOkLocalDate,omitempty"`
}

// openCodeUsageSessionKey hashes a session id for an ownership window.
func openCodeUsageSessionKey(sessionID string) string {
	return openCodeUsageStepKey(sessionID, "session")
}

// openCodeUsageBucket is one account's spend on one local calendar day.
type openCodeUsageBucket struct {
	AccountFingerprint string  `json:"accountFingerprint"`
	LocalDate          string  `json:"localDate"`
	InputTokens        int64   `json:"inputTokens"`
	OutputTokens       int64   `json:"outputTokens"`
	ReasoningTokens    int64   `json:"reasoningTokens"`
	CacheReadTokens    int64   `json:"cacheReadTokens"`
	CacheWriteTokens   int64   `json:"cacheWriteTokens"`
	CostUsd            float64 `json:"costUsd"`
	// ObservedAtMs is the newest contributing step's end time.
	ObservedAtMs int64 `json:"observedAtMs"`
}

// add folds other's counts into b, saturating, keeping the newer observation.
func (b *openCodeUsageBucket) add(other openCodeUsageBucket) {
	b.InputTokens = addOpenCodeUsageCount(b.InputTokens, other.InputTokens)
	b.OutputTokens = addOpenCodeUsageCount(b.OutputTokens, other.OutputTokens)
	b.ReasoningTokens = addOpenCodeUsageCount(b.ReasoningTokens, other.ReasoningTokens)
	b.CacheReadTokens = addOpenCodeUsageCount(b.CacheReadTokens, other.CacheReadTokens)
	b.CacheWriteTokens = addOpenCodeUsageCount(b.CacheWriteTokens, other.CacheWriteTokens)
	b.CostUsd = addOpenCodeUsageCost(b.CostUsd, other.CostUsd)
	if other.ObservedAtMs > b.ObservedAtMs {
		b.ObservedAtMs = other.ObservedAtMs
	}
}

// tokens is what "Tokens today" publishes.
func (b openCodeUsageBucket) tokens() int64 {
	return addOpenCodeUsageCount(addOpenCodeUsageCount(b.InputTokens, b.OutputTokens), b.ReasoningTokens)
}

// openCodeUsageDebt is a run whose usage is not in a bucket yet. Armed
// (OwedAtMs 0) from the child's spawn until it settles, so a crash or update
// mid-turn still leaves something to pay from; owed when it settled with no
// usage captured but a session id to export.
type openCodeUsageDebt struct {
	RunID              string `json:"runId"`
	RunFloorMs         int64  `json:"runFloorMs"`
	SessionID          string `json:"sessionId,omitempty"`
	AccountFingerprint string `json:"accountFingerprint,omitempty"`
	// Dir is the run's working directory: OpenCode files a session under the
	// project it resolves from cwd, so the export runs there.
	Dir         string `json:"dir,omitempty"`
	Attempts    int    `json:"attempts,omitempty"`
	OwedAtMs    int64  `json:"owedAtMs,omitempty"`
	SettledAtMs int64  `json:"settledAtMs,omitempty"`
}

func (d openCodeUsageDebt) owed() bool { return d.OwedAtMs > 0 }

// openCodeUsageStep is one step's spend.
type openCodeUsageStep struct {
	// Key is the hashed dedup key, "" when the frame named nothing stable.
	Key        string
	AtMs       int64
	Input      int64
	Output     int64
	Reasoning  int64
	CacheRead  int64
	CacheWrite int64
	Cost       float64
}

// isZero reports whether the step spends nothing. Field by field, so counts
// near the int64 bound cannot sum to a wrapped zero.
func (s openCodeUsageStep) isZero() bool {
	return s.Input == 0 && s.Output == 0 && s.Reasoning == 0 && s.CacheRead == 0 && s.CacheWrite == 0 && s.Cost == 0
}

// openCodeUsageLockWait bounds how long a ledger write waits on another agent
// process holding the cross-process lock (an update hand-off). A var so tests
// can pin it small.
var openCodeUsageLockWait = 2 * time.Second

var (
	// openCodeUsageMu serialises every ledger read-modify-write in this process,
	// and is held while a writer waits on the cross-process lock — so plain
	// reads never take it.
	openCodeUsageMu sync.Mutex
	// openCodeUsageFileMu guards the file itself: readers share it, and a
	// writer holds it only across its read-modify-rename, never while waiting
	// on another process. It is not optional on Windows: Go opens files without
	// FILE_SHARE_DELETE, so a rename over a ledger another goroutine has open
	// for reading fails — an unguarded card read would fail this process's own
	// commit.
	openCodeUsageFileMu sync.RWMutex
	// openCodeGenerationRotated reports a committed write carrying this
	// process's epoch. Until then the ledger may hold a generation an earlier
	// process published, so ParseContext omits UsageGeneration and the
	// propagator holds OpenCode hints.
	openCodeGenerationRotated atomic.Bool
	// openCodeUsageLocation is the zone the local calendar day is cut in; nil
	// means time.Local. A test seam.
	openCodeUsageLocation *time.Location
)

// openCodeUsageCachePath: AIEXPEDITE_OPENCODE_USAGE_CACHE overrides it (tests
// isolate from the real machine ledger).
func openCodeUsageCachePath() string {
	if p := os.Getenv("AIEXPEDITE_OPENCODE_USAGE_CACHE"); p != "" {
		return p
	}
	return filepath.Join(GetConfigDir(), "opencode_usage.json")
}

// openCodeUsageWriteAttempts bounds the rename retries of one commit. A
// rename can still collide with ANOTHER process reading the ledger (Windows,
// see openCodeUsageFileMu); a read lasts microseconds, so a few short retries
// clear it without holding anything for long.
const (
	openCodeUsageWriteAttempts = 3
	openCodeUsageWriteBackoff  = 15 * time.Millisecond
)

// loadOpenCodeUsageLedger is readOpenCodeUsageLedger for callers outside a
// transaction: it waits only for an in-flight rename, never for a writer that
// is waiting on another process's lock.
func loadOpenCodeUsageLedger() openCodeUsageLedger {
	openCodeUsageFileMu.RLock()
	defer openCodeUsageFileMu.RUnlock()
	return readOpenCodeUsageLedger()
}

// readOpenCodeUsageLedger reads the ledger. A missing, unreadable, oversized
// or other-schema file reads as empty. Callers hold openCodeUsageFileMu.
func readOpenCodeUsageLedger() openCodeUsageLedger {
	var ledger openCodeUsageLedger
	if !readJSONFileWithin(openCodeUsageCachePath(), openCodeUsageMaxLedgerBytes, &ledger) || ledger.SchemaVersion != openCodeUsageSchemaVersion {
		return openCodeUsageLedger{SchemaVersion: openCodeUsageSchemaVersion}
	}
	return ledger
}

// openCodeUsageTransaction runs mutate as one read-modify-write of the ledger.
// mutate returns (write, changed): write=false aborts without writing;
// changed=true advances the generation. It reports whether the write committed
// and the generation after it. The caller decides whether a committed change
// is worth a hint (noteOpenCodeUsageAdvanced): a startup rotation is hinted
// only when there is something to publish.
//
// The in-process mutex alone is not enough: a self-update hands off between two
// agent processes that briefly run side by side, and each would rename its own
// read-modify-write over the other's. The cross-process sibling lock the Codex
// and Claude caches use serialises them; a contended lock refuses the write
// rather than overwrite a competitor's, and a filesystem that offers no lock
// at all degrades to the in-process mutex alone.
func openCodeUsageTransaction(mutate func(ledger *openCodeUsageLedger) (write, changed bool)) (committed, changed bool, generation cliUsageGeneration) {
	return openCodeUsageTransactionWithin(openCodeUsageLockWait, mutate)
}

// openCodeUsageTransactionWithin is openCodeUsageTransaction waiting at most
// lockWait for the cross-process lock; 0 tries once. For a write made while a
// caller holds a lock of its own (the session manager's, across a spawn), where
// waiting on another process would stall every other session. A zero wait also
// refuses rather than queue behind an in-process writer, which may itself be
// waiting on the cross-process lock or a stalled write.
func openCodeUsageTransactionWithin(lockWait time.Duration, mutate func(ledger *openCodeUsageLedger) (write, changed bool)) (committed, changed bool, generation cliUsageGeneration) {
	if lockWait <= 0 {
		if !openCodeUsageMu.TryLock() {
			logOpenCodeUsageCapture("lock_busy")
			return false, false, cliUsageGeneration{}
		}
	} else {
		openCodeUsageMu.Lock()
	}
	path := openCodeUsageCachePath()
	// The lock file is the ledger's sibling: with the directory missing (a fresh
	// or custom cache path) it cannot be opened, reads as "no lock offered", and
	// two processes would both write unlocked. Create it first, as the Codex
	// cache does; a directory that cannot be made cannot take the write either.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		openCodeUsageMu.Unlock()
		logOpenCodeUsageCapture("write_failed")
		return false, false, cliUsageGeneration{}
	}
	lock, outcome := acquireCrossProcessCacheLockUntil(path, time.Now().Add(lockWait))
	if outcome == crossProcessLockContended {
		openCodeUsageMu.Unlock()
		logOpenCodeUsageCapture("lock_contended")
		return false, false, cliUsageGeneration{}
	}
	release := func() {
		if lock != nil {
			_ = unlockFile(lock)
			_ = lock.Close()
		}
		openCodeUsageMu.Unlock()
	}
	openCodeUsageFileMu.Lock()
	ledger := readOpenCodeUsageLedger()
	write, changed := mutate(&ledger)
	if !write {
		openCodeUsageFileMu.Unlock()
		release()
		return false, false, ledger.Generation
	}
	if changed {
		openCodeBumpGeneration(&ledger)
	}
	for attempt := 1; attempt <= openCodeUsageWriteAttempts; attempt++ {
		if committed = writeJSONFileAtomic(path, ledger); committed || attempt == openCodeUsageWriteAttempts {
			break
		}
		time.Sleep(openCodeUsageWriteBackoff)
	}
	openCodeUsageFileMu.Unlock()
	generation = ledger.Generation
	release()
	if !committed {
		logOpenCodeUsageCapture("write_failed")
		return false, false, generation
	}
	if generation.Epoch == codexProcessGenerationEpoch.Load() {
		openCodeGenerationRotated.Store(true)
	}
	return true, changed, generation
}

// noteOpenCodeUsageAdvanced hints the backend about a committed bucket change.
func noteOpenCodeUsageAdvanced(committed, changed bool, generation cliUsageGeneration) {
	if committed && changed {
		noteCLIUsageObservationAdvanced(openCodeUsageProvider, generation)
	}
}

// openCodeBumpGeneration advances the ledger's generation; a ledger from any
// other epoch restarts at counter 1 under this process's.
func openCodeBumpGeneration(ledger *openCodeUsageLedger) {
	if epoch := codexProcessGenerationEpoch.Load(); ledger.Generation.Epoch != epoch {
		ledger.Generation = cliUsageGeneration{Epoch: epoch, Counter: 1}
		return
	}
	ledger.Generation.Counter++
}

// openCodeRecoveryGeneration moves a ledger an earlier process wrote onto this
// process's epoch in one write, then returns its generation when a bucket for
// today still has numbers to publish (nil otherwise). Called once at startup
// by the propagator, so a reading captured just before an update handoff —
// whose hint died with that process — is hinted again. A ledger that never
// published anything is left for its first real write to rotate. refused
// reports a rotation the ledger did not commit (a contended lock, a failed
// rename): the generation it would carry exists only in memory, so the caller
// retries rather than hint it (recoverOpenCodeUsageGeneration).
func openCodeRecoveryGeneration(now time.Time) (generation *cliUsageGeneration, refused bool) {
	today, _ := openCodeLocalDay(now)
	ran, hasToday, rotate := false, false, false
	committed, _, g := openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		ran = true
		for _, b := range ledger.Buckets {
			if b.LocalDate == today && (b.tokens() > 0 || b.CostUsd > 0) {
				hasToday = true
			}
		}
		rotate = len(ledger.Buckets) > 0 && ledger.Generation.Epoch != codexProcessGenerationEpoch.Load()
		return rotate, rotate
	})
	// A contended lock never runs mutate, so it cannot say whether a rotation
	// was due: that is a refusal too.
	if !ran || (rotate && !committed) {
		return nil, true
	}
	if !hasToday || g.Counter <= 0 || g.Epoch != codexProcessGenerationEpoch.Load() {
		return nil, false
	}
	openCodeGenerationRotated.Store(true)
	return &g, false
}

// recoverOpenCodeUsageGeneration runs startup recovery, hinting a recovered
// generation and retrying a refused rotation on the commit ladder — startup
// asks only once, so an unretried refusal would leave the pre-update reading
// stale until an unrelated capture or the periodic gather.
func recoverOpenCodeUsageGeneration(attempt int) {
	g, refused := openCodeRecoveryGeneration(time.Now())
	if g != nil {
		noteCLIUsageObservationAdvanced(openCodeUsageProvider, *g)
	}
	if !refused {
		return
	}
	delay, more := refreshRetryDelayForAttempt(attempt, openCodeUsageCommitMaxAttempts, openCodeUsageCommitLadder)
	if !more {
		logOpenCodeUsageCapture("recovery_abandoned")
		return
	}
	logOpenCodeUsageCapture("recovery_retry")
	openCodeUsageAfterFunc(delay, func() { recoverOpenCodeUsageGeneration(attempt + 1) })
}

// openCodeLocalDay is t's local calendar date and the next local midnight — 23
// or 25 hours away on a DST day.
func openCodeLocalDay(t time.Time) (date string, resetAt time.Time) {
	loc := openCodeUsageZone()
	local := t.In(loc)
	y, m, d := local.Date()
	return local.Format("2006-01-02"), time.Date(y, m, d+1, 0, 0, 0, 0, loc)
}

// openCodeLocalMidnight is the start of t's local calendar day.
func openCodeLocalMidnight(t time.Time) time.Time {
	loc := openCodeUsageZone()
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// openCodeUsageZone is the zone local days are cut in.
func openCodeUsageZone() *time.Location {
	if openCodeUsageLocation != nil {
		return openCodeUsageLocation
	}
	return time.Local
}

// openCodeUsageStepsCarryUsage reports whether any of steps would contribute to
// a bucket. A syntactically valid step whose every token and cost is zero spends
// nothing and publishes nothing, so it is not payment: a turn that reported only
// those is treated as a turn that reported none, and still owes the export
// fallback its reading (cliagent_usage_opencode_freshness.go). The condition
// mirrors the per-step skip in mergeOpenCodeUsageSteps.
func openCodeUsageStepsCarryUsage(steps []openCodeUsageStep) bool {
	for _, s := range steps {
		if !s.isZero() {
			return true
		}
	}
	return false
}

// mergeOpenCodeUsageSteps folds steps into the ledger's buckets for
// fingerprint, skipping any whose key was already counted. It reports whether a
// bucket changed.
func mergeOpenCodeUsageSteps(ledger *openCodeUsageLedger, fingerprint string, steps []openCodeUsageStep) bool {
	seen := make(map[string]bool, len(ledger.SeenSteps))
	for _, k := range ledger.SeenSteps {
		seen[k] = true
	}
	if c := ledger.DirectCursor; c != nil {
		for _, k := range c.PinnedKeys {
			seen[k] = true
		}
	}
	changed := false
	for _, step := range steps {
		if step.Key != "" {
			if seen[step.Key] {
				continue
			}
			seen[step.Key] = true
			ledger.SeenSteps = append(ledger.SeenSteps, step.Key)
		}
		if step.isZero() {
			continue
		}
		date, _ := openCodeLocalDay(time.UnixMilli(step.AtMs))
		openCodeUsageBucketFor(ledger, fingerprint, date).add(openCodeUsageBucket{
			InputTokens:      step.Input,
			OutputTokens:     step.Output,
			ReasoningTokens:  step.Reasoning,
			CacheReadTokens:  step.CacheRead,
			CacheWriteTokens: step.CacheWrite,
			CostUsd:          step.Cost,
			ObservedAtMs:     step.AtMs,
		})
		changed = true
	}
	if n := len(ledger.SeenSteps); n > openCodeUsageMaxSeenSteps {
		ledger.SeenSteps = append([]string(nil), ledger.SeenSteps[n-openCodeUsageMaxSeenSteps:]...)
		ledger.seenEvicted = true
		if c := ledger.DirectCursor; c != nil && c.ThroughMs > 0 {
			c.RewindFloorMs = max(c.RewindFloorMs, c.ThroughMs)
		}
	}
	capOpenCodeUsageBuckets(ledger)
	return changed
}

// openCodeUsageBucketFor returns the bucket for (fingerprint, date), creating it.
func openCodeUsageBucketFor(ledger *openCodeUsageLedger, fingerprint, date string) *openCodeUsageBucket {
	for i := range ledger.Buckets {
		if ledger.Buckets[i].AccountFingerprint == fingerprint && ledger.Buckets[i].LocalDate == date {
			return &ledger.Buckets[i]
		}
	}
	ledger.Buckets = append(ledger.Buckets, openCodeUsageBucket{AccountFingerprint: fingerprint, LocalDate: date})
	return &ledger.Buckets[len(ledger.Buckets)-1]
}

// capOpenCodeUsageBuckets drops the oldest observations past the cap. A bucket
// created for a long-past date by a late export lands oldest, so it is the one
// dropped.
func capOpenCodeUsageBuckets(ledger *openCodeUsageLedger) {
	for len(ledger.Buckets) > openCodeUsageMaxBuckets {
		oldest := 0
		for i, b := range ledger.Buckets {
			if b.ObservedAtMs < ledger.Buckets[oldest].ObservedAtMs {
				oldest = i
			}
		}
		ledger.Buckets = append(ledger.Buckets[:oldest], ledger.Buckets[oldest+1:]...)
	}
}

// adoptPendingOpenCodeUsageBuckets claims the spend banked before any probe had
// named the install's account — a bucket under the empty fingerprint — for the
// account the card publishes.
//
// A run arms when its child spawns, which can precede the first readiness probe
// of the gather: the smoke's own pre-check answers for an isolated directory
// (openCodeSmokeLoggedInDir), so its provider list is not the account the card
// keys the install by, and guessing from it would bank the turn under a
// fingerprint ParseContext never queries. Such a run banks under "" instead, and
// the first gather that resolves an account adopts it. "" means "we could not
// ask", never a different account, so this cannot mix two accounts' spend.
//
// Nothing to adopt is the usual case and costs one lock-free read. The adopting
// write does NOT advance the generation and sends no hint: it re-keys spend the
// commit already counted and hinted, and the gather that calls it publishes the
// adopted numbers itself.
//
// It reports false while pending spend stays un-adopted (a refused write), so
// that gather withholds its generation: published without the adopted numbers,
// the backend would record it as applied and the next adopting gather would
// look covered.
func adoptPendingOpenCodeUsageBuckets(fingerprint string) bool {
	if fingerprint == "" {
		return true
	}
	if !openCodeUsageHasPendingBucket(loadOpenCodeUsageLedger()) {
		return true
	}
	committed, _, _ := openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		var pending []openCodeUsageBucket
		kept := ledger.Buckets[:0]
		for _, b := range ledger.Buckets {
			if b.AccountFingerprint == "" {
				pending = append(pending, b)
				continue
			}
			kept = append(kept, b)
		}
		if len(pending) == 0 {
			return false, false
		}
		ledger.Buckets = kept
		for _, p := range pending {
			openCodeUsageBucketFor(ledger, fingerprint, p.LocalDate).add(p)
		}
		capOpenCodeUsageBuckets(ledger)
		logOpenCodeUsageCapture("account_adopted")
		return true, false
	})
	return committed || !openCodeUsageHasPendingBucket(loadOpenCodeUsageLedger())
}

func openCodeUsageHasPendingBucket(ledger openCodeUsageLedger) bool {
	for _, b := range ledger.Buckets {
		if b.AccountFingerprint == "" {
			return true
		}
	}
	return false
}

// openCodeUsageDay is what the card reads from the ledger for one day.
type openCodeUsageDay struct {
	Bucket     openCodeUsageBucket
	OK         bool
	Generation cliUsageGeneration
	// Direct reports that a direct-run scan read the store on this local date,
	// so the bucket counts every OpenCode run on the computer, not only the
	// agent's own.
	Direct bool
}

// openCodeUsageDayFor reads the bucket for (fingerprint, the local date of now),
// the ledger's generation and the direct reader's coverage.
func openCodeUsageDayFor(fingerprint string, now time.Time) openCodeUsageDay {
	today, _ := openCodeLocalDay(now)
	// Not under openCodeUsageMu: the card's gather must not queue behind a
	// writer that is waiting on another agent process.
	ledger := loadOpenCodeUsageLedger()
	day := openCodeUsageDay{
		Generation: ledger.Generation,
		Direct:     ledger.DirectCoverage != nil && ledger.DirectCoverage.LastOkLocalDate == today,
	}
	for _, b := range ledger.Buckets {
		if b.AccountFingerprint == fingerprint && b.LocalDate == today {
			day.Bucket, day.OK = b, true
			break
		}
	}
	return day
}

// openCodeUsageBucketForDay reads the bucket for (fingerprint, the local date of
// now), and the ledger's generation.
func openCodeUsageBucketForDay(fingerprint string, now time.Time) (openCodeUsageBucket, bool, cliUsageGeneration) {
	day := openCodeUsageDayFor(fingerprint, now)
	return day.Bucket, day.OK, day.Generation
}

// Card labels: "Tokens today" once the direct reader covers the day, and the
// floor's own label while only the agent's runs are counted.
const (
	openCodeTokensTodayLabel          = "Tokens today"
	openCodeTokensTodayAgentRunsLabel = "Tokens today (agent runs)"
)

// openCodeUsageMetrics turns today's bucket into the card's rows: tokens
// always, cost only when above zero — a local model or a subscription
// provider reports 0, and "$0.00" would read as free rather than unmetered. No
// bucket, no rows. direct labels the tokens row by coverage.
func openCodeUsageMetrics(bucket openCodeUsageBucket, ok, direct bool, now time.Time) []cliAgentUsageMetric {
	if !ok || (bucket.tokens() <= 0 && bucket.CostUsd <= 0) {
		return nil
	}
	_, resetAt := openCodeLocalDay(now)
	observedAt := time.UnixMilli(bucket.ObservedAtMs).UTC().Format(time.RFC3339)
	reset := resetAt.Format(time.RFC3339)
	tokens := float64(bucket.tokens())
	label := openCodeTokensTodayAgentRunsLabel
	if direct {
		label = openCodeTokensTodayLabel
	}
	metrics := []cliAgentUsageMetric{{
		Kind:       limitKindDaily,
		Label:      label,
		Unit:       "tokens",
		Consumed:   &tokens,
		ResetAt:    reset,
		ObservedAt: observedAt,
	}}
	if bucket.CostUsd > 0 {
		cost := openCodeUsagePublishedCost(bucket.CostUsd)
		metrics = append(metrics, cliAgentUsageMetric{
			Kind:       limitKindDaily,
			Label:      "Cost today",
			Unit:       usageUnitUSD,
			Consumed:   &cost,
			ResetAt:    reset,
			ObservedAt: observedAt,
		})
	}
	return metrics
}

// openCodeUsagePublishedCost rounds a day's cost to four decimals without an
// overflowing multiplication: a saturated sum (math.MaxFloat64) or a corrupt
// frame times 10,000 is +Inf, canonicalFloat rejects a non-finite metric, and
// the whole signed refresh would fail rather than publish the reading. A cost
// at or above openCodeUsageMaxInt is already far past any real daily spend, so
// it is clamped there and published as-is.
func openCodeUsagePublishedCost(cost float64) float64 {
	if math.IsNaN(cost) {
		return 0
	}
	if cost >= openCodeUsageMaxInt {
		return openCodeUsageMaxInt
	}
	return math.Round(cost*10000) / 10000
}

/* --------------------------------------------------------------------------
   Frame parsing
   -------------------------------------------------------------------------- */

// openCodeUsageRun is one turn's in-memory accumulator. Parsing appends to it;
// nothing here does I/O. A nil run ignores everything, so a caller that could
// not arm still streams.
type openCodeUsageRun struct {
	id          string
	floorMs     int64
	fingerprint string
	executable  string
	dir         string

	mu    sync.Mutex
	steps []openCodeUsageStep
	// committed counts the leading steps already handed to the ledger.
	committed int
	// commitMu serialises take→commit→untake: a terminal-event settle and an
	// exit flush overlapping would otherwise let a refused prefix see a later
	// take, stay marked handed, and be lost while the suffix retires the debt.
	commitMu  sync.Mutex
	sessionID string
	settled   atomic.Bool
	// armRefused reports that the arm has written no debt yet, so there is none
	// to name. A refused arm is retried off the caller's lock; one that lands
	// clears it.
	armRefused atomic.Bool
	// persistedSession is set once the session id reached the armed debt.
	persistedSession atomic.Bool
	// streamClosed is set by finishOpenCodeUsageRun: the run's stream is over,
	// so no further step can join it and the export fallback is free to pay
	// whatever the stream left owed.
	streamClosed atomic.Bool
	// exportDeferred records that an export attempt stood down because this run
	// could still commit the same spend, so the attempt is re-booked the moment
	// the run goes quiet.
	exportDeferred atomic.Bool
	// closingSettle is set by a settle made after the stream is over, so its
	// write closes the run's ownership window (cliagent_usage_opencode_freshness.go).
	closingSettle atomic.Bool
	// windowClosed: the run's ownership window is closed on disk.
	windowClosed atomic.Bool
	// streamEndMs is when the stream ended (0 until it has), recorded once so
	// every close or commit retry closes the window there, not at its own time.
	streamEndMs atomic.Int64
}

// session returns the run's session id, "" until one is known.
func (run *openCodeUsageRun) session() string {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.sessionID
}

// adoptSession records sessionID when the stream named none.
func (run *openCodeUsageRun) adoptSession(sessionID string) {
	if sessionID == "" {
		return
	}
	run.mu.Lock()
	if run.sessionID == "" {
		run.sessionID = sessionID
	}
	run.mu.Unlock()
}

// captureOpenCodeUsageLine reads one streamed line: a step-finish frame's
// usage joins the run, and the first session id any frame names is recorded on
// the run's debt — once per run, off the calling goroutine.
func captureOpenCodeUsageLine(run *openCodeUsageRun, line string) {
	if run == nil {
		return
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") {
		return
	}
	// Cheap pre-filter, so the stream does not decode every text delta: once
	// the session is known only a step frame carrying tokens can matter; until
	// then, any frame naming a session.
	run.mu.Lock()
	knownSession := run.sessionID != ""
	run.mu.Unlock()
	isStep := strings.Contains(line, "tokens") && strings.Contains(line, "step")
	if !isStep && (knownSession || !strings.Contains(line, "ession")) {
		return
	}
	step, sessionID, ok := parseOpenCodeUsageFrame(line, time.Now())
	if !isValidOpenCodeSessionID(sessionID) {
		sessionID = ""
	}
	run.mu.Lock()
	newSession := sessionID != "" && run.sessionID == ""
	if newSession {
		run.sessionID = sessionID
	}
	if ok {
		switch {
		case len(run.steps) < openCodeUsageMaxRunSteps:
			run.steps = append(run.steps, step)
		case run.committed >= len(run.steps):
			// Every held step is already handed to the ledger, so there is
			// nothing to fold into. With no commit in flight those steps are
			// settled for good and their slots are recycled (TryLock: the commit
			// path takes commitMu before run.mu). A commit in flight may still
			// untake them, so the step goes one past the cap instead and the
			// next recycle reclaims it.
			if run.commitMu.TryLock() {
				run.steps = append(run.steps[:0], step)
				run.committed = 0
				run.commitMu.Unlock()
			} else {
				run.steps = append(run.steps, step)
			}
			logOpenCodeUsageCapture("run_step_cap")
		default:
			last := &run.steps[len(run.steps)-1]
			last.Key = ""
			last.Input = addOpenCodeUsageCount(last.Input, step.Input)
			last.Output = addOpenCodeUsageCount(last.Output, step.Output)
			last.Reasoning = addOpenCodeUsageCount(last.Reasoning, step.Reasoning)
			last.CacheRead = addOpenCodeUsageCount(last.CacheRead, step.CacheRead)
			last.CacheWrite = addOpenCodeUsageCount(last.CacheWrite, step.CacheWrite)
			last.Cost = addOpenCodeUsageCost(last.Cost, step.Cost)
			last.AtMs = max(last.AtMs, step.AtMs)
		}
	}
	run.mu.Unlock()
	if newSession {
		run.persistSessionIDAsync(sessionID)
	}
}

// captureOpenCodeUsageStream folds a buffered stdout (the smoke's) into run.
func captureOpenCodeUsageStream(stdout []byte, run *openCodeUsageRun) {
	if run == nil {
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	scanner.Buffer(make([]byte, 0, 64*1024), openCodeNativeMaxFrameBytes)
	for scanner.Scan() {
		captureOpenCodeUsageLine(run, scanner.Text())
	}
}

// snapshot returns a copy of the run's steps and its session id.
func (run *openCodeUsageRun) snapshot() ([]openCodeUsageStep, string) {
	run.mu.Lock()
	defer run.mu.Unlock()
	return append([]openCodeUsageStep(nil), run.steps...), run.sessionID
}

// takeUncommitted returns the steps not yet handed to the ledger, marking them
// handed, and the run's session id. from is the handed count before the call,
// for untake.
func (run *openCodeUsageRun) takeUncommitted() (steps []openCodeUsageStep, sessionID string, from int) {
	run.mu.Lock()
	defer run.mu.Unlock()
	from = run.committed
	steps = append([]openCodeUsageStep(nil), run.steps[run.committed:]...)
	run.committed = len(run.steps)
	return steps, run.sessionID, from
}

// untake hands back the n steps taken from `from`, so a write the ledger
// refused (another agent process holding it, a failed rename) is retried by the
// run's next flush instead of lost. Only when nothing was taken since: steps a
// later take already handed on must not be offered twice.
func (run *openCodeUsageRun) untake(from, n int) {
	run.mu.Lock()
	if run.committed == from+n {
		run.committed = from
	}
	run.mu.Unlock()
}

// pendingCommit reports whether the run still holds steps no ledger write has
// accepted — either never taken, or taken and handed back by untake while a
// commit retry is booked. Those steps carry their own streamed keys, so the
// export fallback must not pay the same turn while any of them is outstanding.
func (run *openCodeUsageRun) pendingCommit() bool {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.committed < len(run.steps)
}

// openCodeUsageFrame is the permissive subset of an event the capture reads.
// Numbers are RawMessage so a string-typed or absent field degrades to "no
// reading" rather than a decode failure.
type openCodeUsageFrame struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionID"`
	SessionId string          `json:"sessionId"`
	Timestamp json.RawMessage `json:"timestamp"`
	Tokens    json.RawMessage `json:"tokens"`
	Cost      json.RawMessage `json:"cost"`
	Part      *struct {
		ID        string          `json:"id"`
		Type      string          `json:"type"`
		SessionID string          `json:"sessionID"`
		MessageID string          `json:"messageID"`
		Tokens    json.RawMessage `json:"tokens"`
		Cost      json.RawMessage `json:"cost"`
		Time      struct {
			End json.RawMessage `json:"end"`
		} `json:"time"`
	} `json:"part"`
}

// isOpenCodeStepFinishType matches the step-finish spellings OpenCode has
// used: `step_finish` (the event), `step-finish` (its part) and `step.finish`.
// isOpenCodeTerminalEventType answers a different question (did the TURN end)
// and rejects a tool-call step, which still spent tokens.
func isOpenCodeStepFinishType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "step_finish", "step-finish", "step.finish":
		return true
	}
	return false
}

// parseOpenCodeUsageFrame decodes one line. ok reports a step-finish frame
// with a valid usage reading; sessionID is any session id the frame names,
// whatever its type. A negative or non-finite value rejects the frame.
func parseOpenCodeUsageFrame(line string, capturedAt time.Time) (step openCodeUsageStep, sessionID string, ok bool) {
	var frame openCodeUsageFrame
	if json.Unmarshal([]byte(line), &frame) != nil {
		return openCodeUsageStep{}, "", false
	}
	partType, partID, messageID := "", "", ""
	tokensRaw, costRaw, endRaw := frame.Tokens, frame.Cost, json.RawMessage(nil)
	if frame.Part != nil {
		partType, partID, messageID = frame.Part.Type, frame.Part.ID, frame.Part.MessageID
		sessionID = frame.Part.SessionID
		if len(frame.Part.Tokens) > 0 {
			tokensRaw = frame.Part.Tokens
		}
		if len(frame.Part.Cost) > 0 {
			costRaw = frame.Part.Cost
		}
		endRaw = frame.Part.Time.End
	}
	sessionID = firstNonEmpty(sessionID, frame.SessionID, frame.SessionId)
	if !isOpenCodeStepFinishType(frame.Type) && !isOpenCodeStepFinishType(partType) {
		return openCodeUsageStep{}, sessionID, false
	}
	step, valid := openCodeUsageFromTokens(tokensRaw, costRaw)
	if !valid {
		logOpenCodeUsageCapture("frame_rejected")
		return openCodeUsageStep{}, sessionID, false
	}
	step.AtMs = capturedAt.UnixMilli()
	end, endOK := openCodeUsageMillis(endRaw)
	if !endOK {
		end, endOK = openCodeUsageMillis(frame.Timestamp)
	}
	if endOK {
		step.AtMs = end
	}
	switch {
	case partID != "":
		step.Key = openCodeUsageStepKey(sessionID, partID)
	case messageID != "" && endOK:
		step.Key = openCodeUsageStepKey(sessionID, messageID+"@"+strconv.FormatInt(end, 10))
	}
	return step, sessionID, true
}

// openCodeUsageFromTokens reads a tokens object — nested
// `{input, output, reasoning, cache: {read, write}}` or flat (`cacheRead`,
// `cache_read`, `inputTokens`, …) — and a cost. valid=false for a missing
// tokens object or any negative / non-finite value.
func openCodeUsageFromTokens(tokensRaw, costRaw json.RawMessage) (openCodeUsageStep, bool) {
	var tokens map[string]json.RawMessage
	if len(tokensRaw) == 0 || json.Unmarshal(tokensRaw, &tokens) != nil || tokens == nil {
		return openCodeUsageStep{}, false
	}
	var cache map[string]json.RawMessage
	if raw, ok := tokens["cache"]; ok {
		_ = json.Unmarshal(raw, &cache)
	}
	var step openCodeUsageStep
	valid := true
	read := func(m map[string]json.RawMessage, keys ...string) int64 {
		for _, k := range keys {
			raw, ok := m[k]
			if !ok {
				continue
			}
			n, ok := openCodeUsageNumber(raw)
			if !ok || n < 0 || math.Round(n) > openCodeUsageMaxInt {
				valid = false
				return 0
			}
			return int64(math.Round(n))
		}
		return 0
	}
	step.Input = read(tokens, "input", "inputTokens", "input_tokens")
	step.Output = read(tokens, "output", "outputTokens", "output_tokens")
	step.Reasoning = read(tokens, "reasoning", "reasoningTokens", "reasoning_tokens")
	step.CacheRead = read(tokens, "cacheRead", "cache_read", "cacheReadTokens", "cache_read_tokens")
	step.CacheWrite = read(tokens, "cacheWrite", "cache_write", "cacheWriteTokens", "cache_write_tokens")
	if cache != nil {
		if step.CacheRead == 0 {
			step.CacheRead = read(cache, "read")
		}
		if step.CacheWrite == 0 {
			step.CacheWrite = read(cache, "write")
		}
	}
	if len(costRaw) > 0 && string(costRaw) != "null" {
		cost, ok := openCodeUsageNumber(costRaw)
		if !ok || cost < 0 {
			return openCodeUsageStep{}, false
		}
		step.Cost = cost
	}
	return step, valid
}

// usageUnitUSD is the unit of a money metric. The CLI Agents card formats a
// reading in exactly this unit as currency (two decimals), so it must not be
// lower-cased.
const usageUnitUSD = "USD"

// openCodeUsageMaxInt bounds a token count or timestamp before its int64
// conversion: the largest integer a float64 holds exactly. Past the int64 range
// that conversion is implementation-defined (it can go negative). Sums of
// bounded steps can still pass int64, so they saturate (addOpenCodeUsageCount).
const openCodeUsageMaxInt = 1 << 53

// addOpenCodeUsageCount sums two non-negative counts, saturating at
// math.MaxInt64: each step is bounded by openCodeUsageMaxInt, but a day's
// bucket accumulates across any number of runs, so a corrupt provider repeating
// huge counts must pin the total rather than wrap it negative.
func addOpenCodeUsageCount(a, b int64) int64 {
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}

// addOpenCodeUsageCost sums two non-negative costs, saturating at the largest
// finite float64: +Inf cannot be encoded to JSON, so it would refuse every
// later ledger write.
func addOpenCodeUsageCost(a, b float64) float64 {
	if s := a + b; !math.IsInf(s, 0) {
		return s
	}
	return math.MaxFloat64
}

// openCodeUsageNumber reads a JSON number. A string, NaN or ±Inf is invalid.
func openCodeUsageNumber(raw json.RawMessage) (float64, bool) {
	var n float64
	if json.Unmarshal(raw, &n) != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, false
	}
	return n, true
}

// openCodeUsageMillis reads a positive epoch-millisecond timestamp.
func openCodeUsageMillis(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	n, ok := openCodeUsageNumber(raw)
	if !ok || n <= 0 || n > openCodeUsageMaxInt {
		return 0, false
	}
	return int64(n), true
}

// openCodeSessionIDPattern is what an OpenCode session id may look like before
// this file stores it or puts it on the export's argv (`ses_…` today): no
// leading dash an option parser would read as a flag, and no character cmd.exe
// treats as syntax when the binary is an npm `.cmd` shim. The id comes from the
// child's own stdout, so anything else is dropped rather than trusted.
var openCodeSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func isValidOpenCodeSessionID(id string) bool {
	return openCodeSessionIDPattern.MatchString(id)
}

// openCodeUsageStepKey hashes a step's identity: sessionID NUL id, 16 hex chars.
func openCodeUsageStepKey(sessionID, id string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + id))
	return hex.EncodeToString(sum[:8])
}

func logOpenCodeUsageCapture(label string) {
	fmt.Printf("%s[cli-usage] opencode capture: %s%s\n", colorCyan, label, colorReset)
}
