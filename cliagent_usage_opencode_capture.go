// cliagent_usage_opencode_capture.go — the device-side OpenCode usage ledger.
//
// Why this exists:
//
//	OpenCode has no quota of its own — it brokers whatever provider the user
//	configured — so this parser deliberately published NO metric at all, and
//	the CLI Agents card had nothing numeric to show after a green maintenance
//	smoke, a managed chat turn, or an `opencode` the user ran themselves.
//
//	Every OpenCode run does report exact token counts, and a cost when the
//	provider bills per token: each `step_finish` event of
//	`opencode run --format json` carries `part.tokens` / `part.cost`, and
//	OpenCode's own session store keeps the same figures per message. So the
//	numbers exist; nothing was keeping them.
//
// What this file holds:
//
//   - The persisted ledger (`opencode_usage.json`): per local day, per
//     message, the token and cost integers, plus one capture generation.
//   - The run-scoped TAP (openCodeRunUsage): native chat, pipe sessions and
//     the maintenance smoke feed it the JSON stream they are already parsing.
//     Steps of ONE message are SUMMED within a run; the ledger then keeps the
//     per-field MAX of that sum and any other observation of the same message
//     (an export), so a stream and a reconcile of the same run never
//     double-count.
//   - The published rows (openCodeLedgerMetrics): "Tokens today" always, and
//     "Cost today" when some counted message reported a nonzero cost.
//
// The bounded reconciliation that fills the ledger for runs whose stream we
// never saw lives in cliagent_usage_opencode_store.go (it is the only part
// that spawns a process); the run-completion debt that makes sure a fresh
// reading exists after every run lives in
// cliagent_usage_opencode_freshness.go.
//
// Redaction: the file holds integers, 16-hex hashes and `YYYY-MM-DD` day keys,
// plus exactly three exceptions — the closed `lastPassOutcome` code, the
// `continuationDue` bool and each day's `partial` bool. No session id, message
// id, model, provider, prompt, text, path or account is ever written. Tokens
// and costs are clamped to [0, 1e12]; anything NaN, negative or non-numeric is
// dropped field by field.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// openCodeUsageProvider is the provider id OpenCode usage is published under.
const openCodeUsageProvider = "opencode"

const (
	openCodeUsageLedgerSchema = 1
	// openCodeUsageLedgerEnv relocates the ledger (tests isolate from the real
	// machine; mirrors AIEXPEDITE_GROK_USAGE_FRESHNESS).
	openCodeUsageLedgerEnv = "AIEXPEDITE_OPENCODE_USAGE_LEDGER"

	// Bounds. The ledger is a rolling two-day window: "today" is what the card
	// renders and yesterday is kept only so a run that straddles midnight still
	// finds its own message where the first observation put it.
	openCodeLedgerMaxDays = 2
	// openCodeLedgerMaxMessagesPerDay caps one day's rows at roughly 1 MB of
	// JSON. Past it the day is marked partial rather than silently under-counted.
	openCodeLedgerMaxMessagesPerDay = 5000
	// openCodeLedgerMaxStreamSessions caps the "the stream already counted this
	// session" set a reconcile consults before skipping an over-cap export.
	openCodeLedgerMaxStreamSessions = 512
	// openCodeLedgerMaxSkipped caps the over-cap exports remembered for a retry.
	openCodeLedgerMaxSkipped = 32
	// openCodeLedgerMaxRechecks caps the sessions remembered for ONE later
	// re-read because they may still have been mid-turn when a pass exported
	// them. Small on purpose: a re-read is a whole export, and only the newest
	// sessions can ever qualify.
	openCodeLedgerMaxRechecks = 8

	// openCodeRunMaxSessions bounds the session ids ONE run's tap remembers. A
	// real run names one; the cap is there because the ids come from vendor
	// output and this map lives for the run's whole life.
	openCodeRunMaxSessions = 32

	// openCodeUsageValueCeiling clamps every stored integer. A provider that
	// reports a nonsense figure must not turn into an unreadable card.
	openCodeUsageValueCeiling = int64(1_000_000_000_000)
)

// Reconcile pass outcomes — a closed set. Persisted as lastPassOutcome and
// logged on the device only.
const (
	openCodeReconcileOK          = "ok"
	openCodeReconcileNoChange    = "no_change"
	openCodeReconcileMore        = "more"
	openCodeReconcileUnsupported = "unsupported"
	openCodeReconcileTimeout     = "timeout"
	openCodeReconcileLaunchError = "launch_error"
	openCodeReconcileOffline     = "offline"
	// openCodeReconcileWriteError is a commit the ledger refused to persist (a
	// read-only or full data dir). The session is still a candidate, so the
	// pass fails and spends budget like a timeout — it never reports a success
	// that would pay a debt for figures that never reached disk.
	openCodeReconcileWriteError = "write_error"
)

// openCodeReconcileSucceeded reports the two outcomes that reached the end of
// the changed-session list: they pay a debt and stamp
// lastSuccessfulReconcileAtMs.
func openCodeReconcileSucceeded(outcome string) bool {
	return outcome == openCodeReconcileOK || outcome == openCodeReconcileNoChange
}

/* ─────────────────────────────── ledger ─────────────────────────────── */

// openCodeMessageUsage is one assistant message's figures. Integers only.
type openCodeMessageUsage struct {
	In         int64 `json:"in,omitempty"`
	Out        int64 `json:"out,omitempty"`
	Reasoning  int64 `json:"reasoning,omitempty"`
	CacheRead  int64 `json:"cacheRead,omitempty"`
	CacheWrite int64 `json:"cacheWrite,omitempty"`
	CostMicros int64 `json:"costMicros,omitempty"`
	// ObservedAtMs is the COMMIT time of the merge that last raised this row,
	// so a row's observedAt is never earlier than the run it covers.
	ObservedAtMs int64 `json:"observedAtMs,omitempty"`
}

// openCodeLedgerDay is one local day.
type openCodeLedgerDay struct {
	// Messages is keyed by the 16-hex hash of `sessionID\0messageID`.
	Messages map[string]openCodeMessageUsage `json:"messages,omitempty"`
	// StreamSessions holds the hashes of sessions whose stream settled COVERED,
	// so a reconcile that cannot export one of them (over the stdout cap) knows
	// its figures are already counted. The day it is filed under matters only
	// for pruning — openCodeSessionStreamCaptured scans every retained day, so
	// a run that straddled midnight is still recognised.
	StreamSessions []string `json:"streamSessions,omitempty"`
	// Partial marks a day whose totals are a lower bound.
	Partial bool `json:"partial,omitempty"`
}

// openCodeSkippedSession remembers one session whose export was over the stdout
// cap, so a later pass can retry it when it changes. The session is identified
// by its 16-hex HASH, never its id — the field name says so, because this file
// is the feature's redaction contract made visible.
type openCodeSkippedSession struct {
	SessionHash string `json:"sessionHash"`
	UpdatedMs   int64  `json:"updatedMs"`
}

// openCodeRecheckSession remembers one session a pass exported while it may
// still have been running, so a later pass reads it ONCE more. OpenCode writes
// a turn's later parts without necessarily advancing the session's `updated`,
// so neither the cursor nor a `skipped` retry (both keyed on `updated`) would
// ever look at it again. Identified by its 16-hex HASH, like every other
// session record here.
type openCodeRecheckSession struct {
	SessionHash string `json:"sessionHash"`
	// UpdatedMs is the `updated` the pass exported it at — kept only so the
	// retention prune can drop the entry with its day.
	UpdatedMs int64 `json:"updatedMs"`
	// DueAtMs is the earliest pass clock that may spend an export on the
	// re-read, so a turn gets time to finish writing.
	DueAtMs int64 `json:"dueAtMs"`
}

// openCodeUsageLedger is the persisted state. See the file header for the
// redaction allowlist.
type openCodeUsageLedger struct {
	SchemaVersion int                `json:"schemaVersion,omitempty"`
	Generation    cliUsageGeneration `json:"generation"`
	// ReconcileCursorMs is the `updated` time of the newest session a reconcile
	// has committed.
	ReconcileCursorMs int64 `json:"reconcileCursorMs,omitempty"`
	// ReconcileCursorTies holds the hashes of the sessions committed AT
	// ReconcileCursorMs. Several sessions can share one `updated` stamp, and a
	// pass that stops inside such a group must not step over the rest of it.
	ReconcileCursorTies []string `json:"reconcileCursorTies,omitempty"`
	// LastSuccessfulReconcileAtMs is set only by a pass that reached the end of
	// the changed-session list (ok / no_change) — never by `more`.
	LastSuccessfulReconcileAtMs int64 `json:"lastSuccessfulReconcileAtMs,omitempty"`
	// LastPassOutcome / LastPassStartedAtMs are written by EVERY pass.
	// LastPassStartedAtMs is what the gather's nudge compares a session dir's
	// mtime against, and is deliberately distinct from the success stamp.
	LastPassOutcome     string `json:"lastPassOutcome,omitempty"`
	LastPassStartedAtMs int64  `json:"lastPassStartedAtMs,omitempty"`
	// ContinuationDue books one more pass when a backlog remains and no debt is
	// open, so a nudge- or click-originated backlog keeps draining.
	ContinuationDue bool `json:"continuationDue,omitempty"`
	// ContinuationFailures counts consecutive failed no-debt continuation
	// passes; ContinuationFirstFailureAtMs is when that run of failures began.
	// Both reset on any committed progress.
	ContinuationFailures         int   `json:"continuationFailures,omitempty"`
	ContinuationFirstFailureAtMs int64 `json:"continuationFirstFailureAtMs,omitempty"`
	// ContinuationPasses counts the passes ONE chain has spent, successful ones
	// included. The failure counter above cannot bound a chain that keeps
	// making progress, and every pass spawns `opencode` on the user's machine —
	// so a runaway backlog is capped here rather than draining three sessions
	// at a time for hours. Reset when a chain ends.
	ContinuationPasses int `json:"continuationPasses,omitempty"`

	Skipped  []openCodeSkippedSession      `json:"skipped,omitempty"`
	Rechecks []openCodeRecheckSession      `json:"rechecks,omitempty"`
	Days     map[string]*openCodeLedgerDay `json:"days,omitempty"`
}

// openCodeLedgerEdit is what a mutation reports back.
type openCodeLedgerEdit struct {
	// Changed: something must be persisted.
	Changed bool
	// TotalsChanged: a day's PUBLISHED totals moved. The only thing that
	// advances the generation, and so the only thing that hints the backend.
	// updateOpenCodeUsageLedger also sets it when today first turns partial:
	// the lower-bound notice is part of the published view too.
	TotalsChanged bool
}

var (
	openCodeLedgerMu sync.Mutex
	// openCodeUsageNow is the clock; a var so tests can pin it.
	openCodeUsageNow = time.Now
	// openCodeProcessGenerationEpoch is this process's generation epoch, drawn
	// once, uniform in [1, 2^53-1] so it stays a JavaScript-safe integer on the
	// wire (the route bounds it by Number.MAX_SAFE_INTEGER).
	openCodeProcessGenerationEpoch atomic.Int64
	// openCodeDrawGenerationEpoch is the draw — the same one Codex uses, so the
	// two cannot drift on a bound. A var so a test can force a value.
	openCodeDrawGenerationEpoch = func() int64 { return codexDrawGenerationEpoch() }
	// openCodeGenerationRotated reports whether a write carrying this process's
	// epoch has committed. Until it has, the parser omits UsageGeneration and
	// the propagator sends no hint.
	openCodeGenerationRotated atomic.Bool
)

func init() { openCodeProcessGenerationEpoch.Store(openCodeDrawGenerationEpoch()) }

func openCodeUsageLedgerPath() string {
	if p := strings.TrimSpace(os.Getenv(openCodeUsageLedgerEnv)); p != "" {
		return p
	}
	return filepath.Join(GetConfigDir(), "opencode_usage.json")
}

// readOpenCodeUsageLedger loads the ledger. A missing or corrupt file reads as
// an empty one — there is nothing to recover and nothing to fail.
func readOpenCodeUsageLedger() openCodeUsageLedger {
	var ledger openCodeUsageLedger
	path := openCodeUsageLedgerPath()
	if path == "" || !readJSONFile(path, &ledger) {
		return openCodeUsageLedger{}
	}
	return ledger
}

// updateOpenCodeUsageLedger applies mutate under the ledger lock and persists
// the result when it changed (temp file + rename, 0600), bumping the generation
// when a day's totals moved. It reports the resulting ledger and whether disk
// now reflects it (true when nothing needed writing).
//
// Nothing under this lock may take another feature's lock.
func updateOpenCodeUsageLedger(mutate func(*openCodeUsageLedger) openCodeLedgerEdit) (openCodeUsageLedger, bool) {
	openCodeLedgerMu.Lock()
	ledger := readOpenCodeUsageLedger()
	today := openCodeDayKey(openCodeUsageNow())
	wasPartial := openCodeDayPartial(ledger, today)
	edit := mutate(&ledger)
	if !wasPartial && openCodeDayPartial(ledger, today) {
		// The parser turns a partial day into the card's lower-bound notice, so
		// a pass that marks it after the gather returned must hint the backend
		// exactly as a moved total does, whichever helper set the flag.
		edit.TotalsChanged = true
	}
	if edit.TotalsChanged {
		openCodeBumpGeneration(&ledger)
	}
	if !edit.Changed && !edit.TotalsChanged {
		openCodeLedgerMu.Unlock()
		return ledger, true
	}
	openCodePruneLedger(&ledger)
	ledger.SchemaVersion = openCodeUsageLedgerSchema
	path := openCodeUsageLedgerPath()
	// Best-effort: a read-only data dir costs a reading, never a run.
	persisted := path != "" && writeJSONFileAtomic(path, ledger)
	openCodeLedgerMu.Unlock()
	if persisted {
		openCodeNoteCommittedGeneration(ledger)
		if edit.TotalsChanged {
			// The one place a fresh OpenCode reading asks the backend to fetch
			// it (cliagent_usage_propagate.go). No values travel in the hint.
			noteCLIUsageObservationAdvanced(openCodeUsageProvider, ledger.Generation)
		}
	}
	return ledger, persisted
}

// openCodeBumpGeneration advances the generation for a write that changed what
// the card renders. A ledger from any other epoch restarts at counter 1 under
// this process's epoch, so a restored or copied file can never be suppressed by
// an old, higher applied counter.
func openCodeBumpGeneration(ledger *openCodeUsageLedger) {
	epoch := openCodeProcessGenerationEpoch.Load()
	if ledger.Generation.Epoch != epoch {
		ledger.Generation = cliUsageGeneration{Epoch: epoch, Counter: 1}
		return
	}
	ledger.Generation.Counter++
}

// openCodeNoteCommittedGeneration completes the rotation on the first committed
// write carrying this process's epoch.
func openCodeNoteCommittedGeneration(ledger openCodeUsageLedger) {
	if ledger.Generation.Epoch == 0 || ledger.Generation.Epoch != openCodeProcessGenerationEpoch.Load() {
		return
	}
	if openCodeGenerationRotated.CompareAndSwap(false, true) {
		noteCLIUsageGenerationRotated(openCodeUsageProvider)
	}
}

// openCodeRotateGenerationEpoch moves a loaded ledger onto this process's epoch
// in one bounded write, so a reading captured by the previous process (a smoke
// just before an update hand-off) is republished under an epoch the backend has
// never applied. A ledger with no days AND no generation has never published
// anything and is left for its first real write to rotate. `refused` reports a
// write the filesystem turned down, which the caller retries.
func openCodeRotateGenerationEpoch(now time.Time) (rotated, refused bool) {
	_ = now
	if openCodeGenerationRotated.Load() {
		return true, false
	}
	empty := false
	ledger, persisted := updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		if len(l.Days) == 0 && l.Generation.Counter <= 0 {
			empty = true
			return openCodeLedgerEdit{}
		}
		if l.Generation.Epoch == openCodeProcessGenerationEpoch.Load() {
			return openCodeLedgerEdit{} // already this epoch; nothing to write
		}
		openCodeBumpGeneration(l)
		return openCodeLedgerEdit{Changed: true}
	})
	if !empty && persisted {
		// A read that already found this epoch on disk completes the rotation
		// too (updateOpenCodeUsageLedger only notes a write it performed).
		openCodeNoteCommittedGeneration(ledger)
	}
	if openCodeGenerationRotated.Load() {
		return true, false
	}
	return false, !empty
}

// openCodeUsageRecoveryGeneration is the rotated generation of a ledger whose
// today rows are numeric — a reading the previous process committed but whose
// hint it never sent (a shutdown inside the debounce). Nil otherwise.
func openCodeUsageRecoveryGeneration() *cliUsageGeneration {
	metrics, generation, _ := openCodeLedgerMetrics(openCodeUsageNow())
	if generation == nil {
		return nil
	}
	for _, m := range metrics {
		if !m.Unknown && m.Consumed != nil {
			return generation
		}
	}
	return nil
}

/* ────────────────────────── day keys & pruning ─────────────────────── */

// openCodeDayKey is the local-time day a message belongs to.
func openCodeDayKey(t time.Time) string { return t.Local().Format("2006-01-02") }

// openCodeNextLocalMidnight is the instant today's counters reset.
func openCodeNextLocalMidnight(now time.Time) time.Time {
	local := now.Local()
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location()).
		AddDate(0, 0, 1)
}

// openCodeLocalMidnight is the start of now's local day.
func openCodeLocalMidnight(now time.Time) time.Time {
	local := now.Local()
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location())
}

// openCodeRetentionFloor is the start of the OLDEST local day the ledger keeps.
// Nothing before it can ever be counted, so it is also the floor below which a
// reconcile candidate is not worth exporting.
func openCodeRetentionFloor(now time.Time) time.Time {
	return openCodeLocalMidnight(now).AddDate(0, 0, -(openCodeLedgerMaxDays - 1))
}

// openCodeRetainedDayKeys are the day keys inside the retention window, newest
// first.
func openCodeRetainedDayKeys(now time.Time) []string {
	keys := make([]string, 0, openCodeLedgerMaxDays)
	for i := 0; i < openCodeLedgerMaxDays; i++ {
		keys = append(keys, openCodeDayKey(now.Local().AddDate(0, 0, -i)))
	}
	return keys
}

// openCodePruneLedger drops days outside the retention window and bounds every
// collection. Called on every persist, so a ledger can never grow without
// bound even if a clock step left stale days behind.
func openCodePruneLedger(ledger *openCodeUsageLedger) {
	now := openCodeUsageNow()
	keep := map[string]bool{}
	for _, key := range openCodeRetainedDayKeys(now) {
		keep[key] = true
	}
	for key := range ledger.Days {
		if !keep[key] {
			delete(ledger.Days, key)
			continue
		}
		day := ledger.Days[key]
		if day == nil {
			delete(ledger.Days, key)
			continue
		}
		if len(day.StreamSessions) > openCodeLedgerMaxStreamSessions {
			day.StreamSessions = day.StreamSessions[len(day.StreamSessions)-openCodeLedgerMaxStreamSessions:]
		}
	}
	if len(ledger.Days) == 0 {
		ledger.Days = nil
	}
	openCodePruneSkipped(ledger, now)
	openCodePruneRechecks(ledger, now)
}

// openCodePruneSkipped drops over-cap records whose session's `updated` falls
// outside the retention window, then bounds the rest. A full set evicts its
// OLDEST in-retention entry, which simply means that session is never retried —
// the day it belongs to is already marked partial.
func openCodePruneSkipped(ledger *openCodeUsageLedger, now time.Time) {
	floor := openCodeRetentionFloor(now).UnixMilli()
	kept := make([]openCodeSkippedSession, 0, len(ledger.Skipped))
	for _, entry := range ledger.Skipped {
		if entry.SessionHash == "" || entry.UpdatedMs < floor {
			continue
		}
		kept = append(kept, entry)
	}
	if len(kept) > openCodeLedgerMaxSkipped {
		sort.SliceStable(kept, func(i, j int) bool { return kept[i].UpdatedMs < kept[j].UpdatedMs })
		kept = kept[len(kept)-openCodeLedgerMaxSkipped:]
	}
	if len(kept) == 0 {
		ledger.Skipped = nil
		return
	}
	ledger.Skipped = kept
}

// openCodePruneRechecks drops re-read records whose session's `updated` falls
// outside the retention window — nothing such an export could carry would be
// stored — then bounds the rest to the newest, because an older re-read is the
// one least likely to still be mid-turn.
func openCodePruneRechecks(ledger *openCodeUsageLedger, now time.Time) {
	floor := openCodeRetentionFloor(now).UnixMilli()
	kept := make([]openCodeRecheckSession, 0, len(ledger.Rechecks))
	for _, entry := range ledger.Rechecks {
		if entry.SessionHash == "" || entry.UpdatedMs < floor {
			continue
		}
		kept = append(kept, entry)
	}
	if len(kept) > openCodeLedgerMaxRechecks {
		sort.SliceStable(kept, func(i, j int) bool { return kept[i].UpdatedMs < kept[j].UpdatedMs })
		kept = kept[len(kept)-openCodeLedgerMaxRechecks:]
	}
	if len(kept) == 0 {
		ledger.Rechecks = nil
		return
	}
	ledger.Rechecks = kept
}

/* ──────────────────────────────── merge ─────────────────────────────── */

// openCodeUsageHash is the 16-hex identity of a (session, message) pair, and of
// a session on its own (message empty). Nothing reversible is stored.
func openCodeUsageHash(sessionID, messageID string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + messageID))
	return hex.EncodeToString(sum[:8])
}

// openCodeObservedMessage is one observation of a message, from either source.
type openCodeObservedMessage struct {
	SessionID string
	MessageID string
	// EventAt is the SOURCE event time — the first stream event carrying this
	// messageID, or the export's `time.created`. Never the settle or pass clock.
	EventAt time.Time
	Usage   openCodeMessageUsage
}

// openCodeMergeObservations folds observations into the ledger and reports
// whether any day's totals moved.
//
// Day attribution: a message belongs to the local day of its source event
// time, and a merge first looks the hash up in EVERY retained day so one
// message keeps ONE day key even when the stream and the export straddle
// midnight.
//
// Per field the ledger keeps the MAX of the observations. A source reporting
// LESS than an earlier one therefore leaves the higher figure standing, which
// can over-count that one message — preferred over dropping steps, and bounded
// to the message.
func openCodeMergeObservations(ledger *openCodeUsageLedger, observations []openCodeObservedMessage, commitMs int64) bool {
	if len(observations) == 0 {
		return false
	}
	now := openCodeUsageNow()
	retained := openCodeRetainedDayKeys(now)
	changed := false
	for _, obs := range observations {
		if obs.MessageID == "" {
			continue
		}
		key := openCodeUsageHash(obs.SessionID, obs.MessageID)
		dayKey := openCodeDayKey(obs.EventAt)
		if existing := openCodeFindMessageDay(ledger, retained, key); existing != "" {
			dayKey = existing
		}
		if !openCodeDayRetained(retained, dayKey) {
			continue
		}
		day := openCodeEnsureDay(ledger, dayKey)
		current, held := day.Messages[key]
		if !held && len(day.Messages) >= openCodeLedgerMaxMessagesPerDay {
			// Past the row cap the day is a lower bound rather than a silently
			// low number.
			if !day.Partial {
				day.Partial = true
				changed = true
			}
			continue
		}
		merged, raised := openCodeMergeMessageUsage(current, obs.Usage)
		if !raised {
			// Nothing rose: either a replayed observation of a row we already
			// hold, or a message that reported no figures at all (a turn whose
			// only event was a step_start). Neither changes what the card
			// renders, so neither may add a row or move the generation — a row
			// of zeros would inflate the day's row count against its cap and
			// earn the backend a hint with nothing new to fetch.
			continue
		}
		merged.ObservedAtMs = commitMs
		day.Messages[key] = merged
		changed = true
	}
	return changed
}

// openCodeMergeMessageUsage keeps the per-field maximum and reports whether any
// counted field rose. A field that only EQUALS what is stored is not a rise, so
// a replayed observation never moves the generation.
func openCodeMergeMessageUsage(current, incoming openCodeMessageUsage) (openCodeMessageUsage, bool) {
	out := current
	raised := false
	fields := []struct{ into, from *int64 }{
		{&out.In, &incoming.In},
		{&out.Out, &incoming.Out},
		{&out.Reasoning, &incoming.Reasoning},
		{&out.CacheRead, &incoming.CacheRead},
		{&out.CacheWrite, &incoming.CacheWrite},
		{&out.CostMicros, &incoming.CostMicros},
	}
	for _, f := range fields {
		if v := openCodeClampUsageValue(*f.from); v > *f.into {
			*f.into = v
			raised = true
		}
	}
	return out, raised
}

// openCodeClampUsageValue bounds one stored integer; a negative or absurd
// figure is dropped to zero rather than published.
func openCodeClampUsageValue(v int64) int64 {
	if v <= 0 {
		return 0
	}
	if v > openCodeUsageValueCeiling {
		return openCodeUsageValueCeiling
	}
	return v
}

func openCodeDayRetained(retained []string, key string) bool {
	for _, k := range retained {
		if k == key {
			return true
		}
	}
	return false
}

// openCodeFindMessageDay returns the retained day a message hash already lives
// in, or "".
func openCodeFindMessageDay(ledger *openCodeUsageLedger, retained []string, key string) string {
	for _, dayKey := range retained {
		if day := ledger.Days[dayKey]; day != nil {
			if _, ok := day.Messages[key]; ok {
				return dayKey
			}
		}
	}
	return ""
}

func openCodeEnsureDay(ledger *openCodeUsageLedger, dayKey string) *openCodeLedgerDay {
	if ledger.Days == nil {
		ledger.Days = map[string]*openCodeLedgerDay{}
	}
	day := ledger.Days[dayKey]
	if day == nil {
		day = &openCodeLedgerDay{}
		ledger.Days[dayKey] = day
	}
	if day.Messages == nil {
		day.Messages = map[string]openCodeMessageUsage{}
	}
	return day
}

// openCodeDayPartial reports whether a day's totals are marked a lower bound.
func openCodeDayPartial(ledger openCodeUsageLedger, dayKey string) bool {
	day := ledger.Days[dayKey]
	return day != nil && day.Partial
}

// openCodeMarkTodayPartial marks today's totals as a lower bound.
func openCodeMarkTodayPartial(ledger *openCodeUsageLedger, now time.Time) bool {
	day := openCodeEnsureDay(ledger, openCodeDayKey(now))
	if day.Partial {
		return false
	}
	day.Partial = true
	return true
}

// openCodeRecordStreamSession remembers that a session's stream settled covered,
// so a reconcile that cannot export it knows its figures are already counted.
func openCodeRecordStreamSession(ledger *openCodeUsageLedger, dayKey, sessionHash string) bool {
	if sessionHash == "" || !openCodeDayRetained(openCodeRetainedDayKeys(openCodeUsageNow()), dayKey) {
		return false
	}
	day := openCodeEnsureDay(ledger, dayKey)
	for _, held := range day.StreamSessions {
		if held == sessionHash {
			return false
		}
	}
	day.StreamSessions = append(day.StreamSessions, sessionHash)
	if len(day.StreamSessions) > openCodeLedgerMaxStreamSessions {
		day.StreamSessions = day.StreamSessions[len(day.StreamSessions)-openCodeLedgerMaxStreamSessions:]
	}
	return true
}

// openCodeSessionStreamCaptured reports a session any retained day's stream set
// holds.
func openCodeSessionStreamCaptured(ledger openCodeUsageLedger, sessionHash string) bool {
	for _, dayKey := range openCodeRetainedDayKeys(openCodeUsageNow()) {
		day := ledger.Days[dayKey]
		if day == nil {
			continue
		}
		for _, held := range day.StreamSessions {
			if held == sessionHash {
				return true
			}
		}
	}
	return false
}

/* ───────────────────────────── published rows ───────────────────────── */

// openCodeUsagePartialNotice is the card banner a partial day carries.
const openCodeUsagePartialNotice = "Some OpenCode activity today could not be counted; totals are a lower bound"

// openCodeLedgerMetrics renders today's rows.
//
// Rows are "used today" COUNTERS, not gauges: kind daily, `consumed` only, no
// `total` (OpenCode has no limit, so any total would be invented) and resetAt
// at the next local midnight. Tokens are input + output + reasoning — cache
// reads and writes are left out, because they would make a cached turn look
// many times larger than it was. Cost is emitted only when some counted
// message reported one above zero: subscription providers report 0, and
// "$0.00" would mislead there.
//
// With no messages today, a zero row is published ONLY when the latest pass
// reached the end of the candidate list, no continuation is booked, and that
// pass ran after local midnight — so 0 is never shown while today's sessions
// are still queued. Otherwise no row at all, never an "unknown" placeholder.
func openCodeLedgerMetrics(now time.Time) ([]cliAgentUsageMetric, *cliUsageGeneration, bool) {
	ledger := readOpenCodeUsageLedger()
	var generation *cliUsageGeneration
	if ledger.Generation.Counter > 0 && ledger.Generation.Epoch == openCodeProcessGenerationEpoch.Load() {
		g := ledger.Generation
		generation = &g
	}
	day := ledger.Days[openCodeDayKey(now)]
	partial := day != nil && day.Partial
	resetAt := openCodeNextLocalMidnight(now).UTC().Format(time.RFC3339)

	var tokens, costMicros, observedAtMs int64
	if day == nil || len(day.Messages) == 0 {
		if !openCodeZeroRowPublishable(ledger, now) {
			return nil, generation, partial
		}
		observedAtMs = ledger.LastSuccessfulReconcileAtMs
	} else {
		for _, usage := range day.Messages {
			tokens += usage.In + usage.Out + usage.Reasoning
			costMicros += usage.CostMicros
			if usage.ObservedAtMs > observedAtMs {
				observedAtMs = usage.ObservedAtMs
			}
		}
	}

	observedAt := ""
	if observedAtMs > 0 {
		observedAt = time.UnixMilli(observedAtMs).UTC().Format(time.RFC3339)
	}
	metrics := []cliAgentUsageMetric{{
		Kind:       limitKindDaily,
		Label:      "Tokens today",
		Unit:       usageUnitTokens,
		Consumed:   usageFloatPtr(float64(tokens)),
		ResetAt:    resetAt,
		ObservedAt: observedAt,
	}}
	if costMicros > 0 {
		metrics = append(metrics, cliAgentUsageMetric{
			Kind:       limitKindDaily,
			Label:      "Cost today",
			Unit:       usageUnitUSD,
			Consumed:   usageFloatPtr(float64(costMicros) / 1e6),
			ResetAt:    resetAt,
			ObservedAt: observedAt,
		})
	}
	return metrics, generation, partial
}

// openCodeZeroRowPublishable reports whether "0 tokens today" is a statement we
// can stand behind. A later `more` suppresses it even when an earlier `ok` from
// today is still on record.
func openCodeZeroRowPublishable(ledger openCodeUsageLedger, now time.Time) bool {
	return openCodeReconcileSucceeded(ledger.LastPassOutcome) &&
		!ledger.ContinuationDue &&
		ledger.LastSuccessfulReconcileAtMs >= openCodeLocalMidnight(now).UnixMilli()
}

func usageFloatPtr(v float64) *float64 { return &v }

/* ──────────────────────────────── the tap ───────────────────────────── */

// openCodeRunUsage is one armed OpenCode run. A nil handle is a no-op
// everywhere, as with antigravityRunCapture, so an arm site needs no branch.
type openCodeRunUsage struct {
	label string
	floor time.Time

	mu sync.Mutex
	// messages accumulates, per `sessionID\0messageID`, the SUM of every
	// step_finish of that message within this run.
	messages map[string]*openCodeObservedMessage
	// sessions is every session id the stream named.
	sessions map[string]struct{}
	// sawTokens: at least one step_finish carried a nonzero figure.
	sawTokens bool
	// gaps: a step_finish arrived with no messageID, so the run cannot be
	// counted from its stream alone and owes a reconcile.
	gaps     bool
	finished bool
}

// openCodeUsageRunLabels is the closed set of arm-site names, and the ONLY
// strings that can reach a log line from this feature.
//
// The label is enforced rather than merely documented: it is the one free-form
// argument an arm site passes, a new site could pass `cmd` or an argv by
// mistake, and this feature's redaction contract is that nothing but fixed
// labels and counters is ever logged. Anything unrecognised becomes
// openCodeUsageRunLabelOther, so a mistake costs a vague log line instead of a
// leaked command line.
const openCodeUsageRunLabelOther = "other"

var openCodeUsageRunLabels = map[string]bool{
	"native chat": true, "pipe session": true, "smoke": true,
	"local execute": true, "windows execute": true, "PTY session": true,
	openCodeUsageRunLabelOther: true,
}

func openCodeUsageRunLabel(label string) string {
	if openCodeUsageRunLabels[label] {
		return label
	}
	return openCodeUsageRunLabelOther
}

// armOpenCodeUsageRun arms a run whose stream the caller will tap. Returns nil
// when the refresh is disabled, so every method stays a no-op.
//
// label must be one of openCodeUsageRunLabels — never a command line, path or
// prompt.
func armOpenCodeUsageRun(label string) *openCodeRunUsage {
	if !openCodeUsageRefreshEnabled.Load() {
		return nil
	}
	handle := &openCodeRunUsage{
		label:    openCodeUsageRunLabel(label),
		floor:    openCodeUsageNow(),
		messages: map[string]*openCodeObservedMessage{},
		sessions: map[string]struct{}{},
	}
	armOpenCodeUsageRunFloor(handle.floor)
	return handle
}

// armOpenCodeUsageForCommand arms when spawning cmd+args would run an OpenCode
// turn. Every spawn site goes through this rather than pairing its own
// isOpenCodeCommand check with the spend-free carve-out: a site that
// re-implements the pair is a site that can drift (which is how the Windows
// execute path ended up arming nothing for Antigravity).
//
// `commandRunsOpenCode`, not `isOpenCodeCommand`: terminal-service ships an
// operator-joined command to the execute and PTY paths as `bash -c "opencode
// …"` or `powershell -EncodedCommand <base64>`, where the base program is the
// shell. Those are exactly the paths whose figures can come from NOWHERE but a
// reconcile, because their output is never tapped
// (cliagent_usage_wrapped_command.go).
//
// The spend-free carve-out only applies to a DIRECT argv: a wrapper's args are
// the shell's, so there is nothing to read it from. A wrapped run therefore
// arms, which is the safe side — a spurious debt costs one bounded reconcile
// pass, a missed one costs the reading.
func armOpenCodeUsageForCommand(label, cmd string, args []string) *openCodeRunUsage {
	if isOpenCodeCommand(cmd) {
		if isOpenCodeDiagnosticInvocation(args) {
			return nil
		}
		return armOpenCodeUsageRun(label)
	}
	if !commandRunsOpenCode(cmd, args) {
		return nil
	}
	return armOpenCodeUsageRun(label)
}

// Disarm withdraws a run that never reached inference (a failed spawn, a
// refused pre-check): it spent nothing and owes nothing.
func (h *openCodeRunUsage) Disarm() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.finished {
		h.mu.Unlock()
		return
	}
	h.finished = true
	h.mu.Unlock()
	disarmOpenCodeUsageRunFloor(h.floor)
}

// Observe folds one `opencode run --format json` stdout line into the run's
// accumulator. Only integers, the two ids and the event time are read; the line
// itself is never retained.
func (h *openCodeRunUsage) Observe(line string) {
	if h == nil {
		return
	}
	frame, ok := parseOpenCodeUsageFrame(line)
	if !ok {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.finished {
		return
	}
	if frame.SessionID != "" && len(h.sessions) < openCodeRunMaxSessions {
		// Bounded like the message map: these ids come from vendor output, and
		// a run that named an unbounded number of sessions would otherwise grow
		// this map for its whole life. A real run names one.
		h.sessions[frame.SessionID] = struct{}{}
	}
	if frame.MessageID != "" {
		key := frame.SessionID + "\x00" + frame.MessageID
		entry := h.messages[key]
		if entry == nil {
			if len(h.messages) >= openCodeLedgerMaxMessagesPerDay {
				// A pathological run cannot buffer unbounded rows; the
				// reconcile is the backstop.
				h.gaps = true
				return
			}
			entry = &openCodeObservedMessage{
				SessionID: frame.SessionID,
				MessageID: frame.MessageID,
				EventAt:   frame.EventAt,
			}
			h.messages[key] = entry
		}
		if !frame.StepFinish {
			return
		}
		// Each step_finish carries only ITS step's figures, so they SUM within
		// the message.
		openCodeAddStepUsage(&entry.Usage, frame.Usage)
		if entry.Usage.In+entry.Usage.Out+entry.Usage.Reasoning > 0 {
			h.sawTokens = true
		}
		return
	}
	if frame.StepFinish {
		// A step we cannot attribute to a message: not counted, and the run
		// owes a reconcile.
		h.gaps = true
	}
}

// Finish merges the run's accumulator and settles its freshness debt.
//
// The run is COVERED when the stream ended cleanly (a terminal event, no
// overflow, no timeout, no kill) and some step_finish carried tokens for a
// message we could name. Otherwise it OWES one reconcile, as every execute and
// PTY run does — those are never tapped at all.
//
// Returns true when a debt is now owed.
func (h *openCodeRunUsage) Finish(cleanEnd bool) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	if h.finished {
		h.mu.Unlock()
		return false
	}
	h.finished = true
	observations := make([]openCodeObservedMessage, 0, len(h.messages))
	for _, entry := range h.messages {
		observations = append(observations, *entry)
	}
	sessions := make([]string, 0, len(h.sessions))
	for id := range h.sessions {
		sessions = append(sessions, id)
	}
	covered := cleanEnd && h.sawTokens && !h.gaps
	h.mu.Unlock()

	// A TOTAL order, so which messages get in when the day's row cap is reached
	// is deterministic rather than map-iteration order. (The committed file is
	// byte-stable either way: the rows live in a map, and encoding/json sorts
	// map keys.)
	sort.Slice(observations, func(i, j int) bool {
		if observations[i].SessionID != observations[j].SessionID {
			return observations[i].SessionID < observations[j].SessionID
		}
		return observations[i].MessageID < observations[j].MessageID
	})
	sort.Strings(sessions)

	commitMs := openCodeUsageNow().UnixMilli()
	if len(observations) > 0 || covered {
		_, persisted := updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
			totals := openCodeMergeObservations(l, observations, commitMs)
			changed := totals
			if covered {
				for _, id := range sessions {
					if openCodeRecordStreamSession(l, openCodeDayKey(openCodeUsageNow()), openCodeUsageHash(id, "")) {
						changed = true
					}
				}
			}
			return openCodeLedgerEdit{Changed: changed, TotalsChanged: totals}
		})
		// A ledger write is best-effort everywhere ELSE in this feature — a
		// pass stamp or a continuation flag that does not land costs a reading.
		// Here it is the ONLY copy of this run's figures: the handle is
		// consumed, so a refused write (a read-only or full data dir) loses
		// them for good. A covered run whose merge did not land is therefore
		// not covered: it owes the reconcile that can read them back from
		// OpenCode's own store.
		if !persisted {
			covered = false
			logOpenCodeUsage("ledger write refused by=%s", h.label)
		}
	}
	return settleOpenCodeUsageRun(h.floor, covered, h.label)
}

// observeOpenCodeUsageFromStdout folds a CAPTURED stdout buffer into the run's
// accumulator, line by line — the maintenance smoke's shape, where the child's
// bytes were buffered rather than streamed and are about to be discarded. Only
// integers survive the call.
//
// Deliberately separate from parseOpenCodeSmokeStream, which the smoke runs
// TWICE on the same bytes (once to classify, once for its failure log line):
// folding inside it would count every step of a failed smoke twice.
func observeOpenCodeUsageFromStdout(handle *openCodeRunUsage, stdout []byte) {
	if handle == nil || len(stdout) == 0 {
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	scanner.Buffer(make([]byte, 0, 64*1024), openCodeNativeMaxFrameBytes)
	for scanner.Scan() {
		handle.Observe(scanner.Text())
	}
}

/* ────────────────────────────── frame decode ────────────────────────── */

// openCodeUsageFrame is what one stdout line says about usage. The decode is
// deliberately permissive — OpenCode's event schema is upstream-owned and CI
// has no real binary — so an unrecognised shape contributes nothing rather than
// failing the turn. Same RawMessage style as openCodeSmokeFrame.
type openCodeUsageFrame struct {
	SessionID  string
	MessageID  string
	EventAt    time.Time
	StepFinish bool
	Usage      openCodeMessageUsage
}

type openCodeUsageFrameJSON struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionID"`
	SessionId string          `json:"sessionId"`
	Timestamp json.RawMessage `json:"timestamp"`
	Part      struct {
		Type      string             `json:"type"`
		SessionID string             `json:"sessionID"`
		MessageID string             `json:"messageID"`
		MessageId string             `json:"messageId"`
		Cost      json.RawMessage    `json:"cost"`
		Tokens    openCodeTokensJSON `json:"tokens"`
	} `json:"part"`
}

// openCodeTokensJSON is the token block, spelled as OpenCode's AI-SDK formatter
// emits it. Every field is a RawMessage so a string or null degrades to zero
// instead of failing the whole frame.
type openCodeTokensJSON struct {
	Input     json.RawMessage `json:"input"`
	Output    json.RawMessage `json:"output"`
	Reasoning json.RawMessage `json:"reasoning"`
	Cache     struct {
		Read  json.RawMessage `json:"read"`
		Write json.RawMessage `json:"write"`
	} `json:"cache"`
}

func parseOpenCodeUsageFrame(line string) (openCodeUsageFrame, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") {
		return openCodeUsageFrame{}, false
	}
	var raw openCodeUsageFrameJSON
	if json.Unmarshal([]byte(line), &raw) != nil {
		return openCodeUsageFrame{}, false
	}
	frame := openCodeUsageFrame{
		SessionID:  firstNonEmpty(raw.Part.SessionID, raw.SessionID, raw.SessionId),
		MessageID:  firstNonEmpty(raw.Part.MessageID, raw.Part.MessageId),
		StepFinish: isOpenCodeStepFinishType(raw.Type) || isOpenCodeStepFinishType(raw.Part.Type),
		EventAt:    openCodeFrameEventTime(raw.Timestamp),
	}
	frame.Usage = openCodeUsageFromJSON(raw.Part.Tokens, raw.Part.Cost)
	return frame, true
}

// openCodeUsageFromJSON reads one token block and its cost.
func openCodeUsageFromJSON(tokens openCodeTokensJSON, cost json.RawMessage) openCodeMessageUsage {
	return openCodeMessageUsage{
		In:         openCodeJSONInt(tokens.Input),
		Out:        openCodeJSONInt(tokens.Output),
		Reasoning:  openCodeJSONInt(tokens.Reasoning),
		CacheRead:  openCodeJSONInt(tokens.Cache.Read),
		CacheWrite: openCodeJSONInt(tokens.Cache.Write),
		CostMicros: openCodeJSONMicros(cost),
	}
}

// openCodeAddStepUsage adds one step's figures to a message's running sum.
func openCodeAddStepUsage(into *openCodeMessageUsage, step openCodeMessageUsage) {
	into.In += openCodeClampUsageValue(step.In)
	into.Out += openCodeClampUsageValue(step.Out)
	into.Reasoning += openCodeClampUsageValue(step.Reasoning)
	into.CacheRead += openCodeClampUsageValue(step.CacheRead)
	into.CacheWrite += openCodeClampUsageValue(step.CacheWrite)
	into.CostMicros += openCodeClampUsageValue(step.CostMicros)
}

// isOpenCodeStepFinishType matches the step-completion event by SHAPE rather
// than an exact allowlist: `--format json` has spelled it `step_finish` (event
// type) and `step-finish` (part type) across releases, and an exact list would
// silently stop counting on the next rename.
func isOpenCodeStepFinishType(t string) bool {
	t = strings.ToLower(strings.TrimSpace(t))
	return strings.Contains(t, "step") && strings.Contains(t, "finish")
}

// openCodeFrameEventTime reads an event's own clock — epoch millis, epoch
// seconds, or an RFC3339 string — and falls back to now when it names none, so
// a message still lands on a day.
func openCodeFrameEventTime(raw json.RawMessage) time.Time {
	now := openCodeUsageNow()
	if len(raw) == 0 {
		return now
	}
	var asNumber float64
	if json.Unmarshal(raw, &asNumber) == nil && asNumber > 0 {
		return openCodeEpochTime(asNumber)
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		if asString = strings.TrimSpace(asString); asString != "" {
			if t, err := time.Parse(time.RFC3339, asString); err == nil {
				return t
			}
			if n, err := strconv.ParseFloat(asString, 64); err == nil && n > 0 {
				return openCodeEpochTime(n)
			}
		}
	}
	return now
}

// openCodeEpochTime reads a numeric epoch that may be in seconds or
// milliseconds. 1e11 seconds is the year 5138, so anything above it is millis.
func openCodeEpochTime(v float64) time.Time {
	if v >= 1e11 {
		return time.UnixMilli(int64(v))
	}
	return time.Unix(int64(v), 0)
}

// openCodeJSONInt reads a non-negative integer; NaN, a negative, a string and
// any other shape all read as zero, field by field.
func openCodeJSONInt(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var v float64
	if json.Unmarshal(raw, &v) != nil {
		return 0
	}
	if v != v || v <= 0 { // NaN never equals itself
		return 0
	}
	return openCodeClampUsageValue(int64(v))
}

// openCodeJSONMicros reads a dollar cost into integer micro-dollars, so the
// ledger never stores a float.
func openCodeJSONMicros(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var v float64
	if json.Unmarshal(raw, &v) != nil {
		return 0
	}
	if v != v || v <= 0 {
		return 0
	}
	return openCodeClampUsageValue(int64(v*1e6 + 0.5))
}

/* ───────────────────────────────── logs ─────────────────────────────── */

// logOpenCodeUsage emits one device-local line. Labels are fixed values and
// counters only — never a path, a session id, a model or any vendor text.
func logOpenCodeUsage(label string, args ...any) {
	fmt.Printf("%s[cli-usage] opencode usage: %s%s\n", colorCyan, fmt.Sprintf(label, args...), colorReset)
}

// resetOpenCodeUsageLedgerForTests clears the in-process rotation state. Test
// seam only.
func resetOpenCodeUsageLedgerForTests() {
	openCodeGenerationRotated.Store(false)
	openCodeProcessGenerationEpoch.Store(openCodeDrawGenerationEpoch())
}

// openCodeEarliestRecheckDueMs is the soonest clock at which a remembered
// re-read may spend an export, and whether any is remembered at all. The
// freshness worker books a pass for it: a record is consumed only by a pass
// that happens to run after it comes due, so without a wake-up of its own a
// re-read booked for a session nothing else disturbs again would be stranded.
func openCodeEarliestRecheckDueMs(ledger openCodeUsageLedger) (int64, bool) {
	earliest, found := int64(0), false
	for _, entry := range ledger.Rechecks {
		if entry.SessionHash == "" {
			continue
		}
		if !found || entry.DueAtMs < earliest {
			earliest, found = entry.DueAtMs, true
		}
	}
	return earliest, found
}
