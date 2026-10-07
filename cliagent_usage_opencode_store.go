// cliagent_usage_opencode_store.go — the bounded reconciliation that fills the
// OpenCode usage ledger from OpenCode's own session store.
//
// Why through the CLI and not the files:
//
//	OpenCode has moved its storage layout between releases — openCodeSessionDirs
//	probes five roots for exactly that reason — so reading `storage/**` or a
//	database would break on an upgrade. This file therefore asks OpenCode:
//	`opencode session list --format json` for what changed, then
//	`opencode export <id>` for the figures. The ONLY file access anywhere in
//	this feature is the stat-only discovery walk below, over roots
//	openCodeSessionDirs already knows.
//
//	If an upgrade moves sessions out of them, a direct run is recovered only by
//	a Refresh click (the live reconcile) or by the next managed run's debt.
//	Output that neither command spells the way we read is the closed outcome
//	`unsupported`: the debt retires and the stream figures stand.
//
// Cost discipline — this runs on the user's machine while they are working:
//
//   - One pass is 1 `session list` + at most openCodeReconcileMaxExports
//     `export` calls for LISTED sessions, OLDEST changed session first (at most
//     one of which is a revisit: an over-cap retry or a due re-read of a
//     session that may have been mid-turn when a pass read it), each
//     export's stdout capped at openCodeExportMaxStdout and decoded as a
//     stream, plus at most openCodeExportMaxChildren further exports per listed
//     session for the subagent sessions it delegated to — a child session holds
//     one turn's messages, and `session list` cannot name it at all.
//   - A pass runs only on a debt rung, a gather nudge (on its cooldown) or a
//     Refresh click — never on an idle gather.
//   - Passes are single-flight: the click's live probe joins the worker's
//     flight rather than starting a second one.
//
// Redaction: nothing the CLI prints is logged or persisted. stderr is only
// CLASSIFIED (a launch failure vs. unrecognised output), never retained, and
// the result is one closed outcome code plus counters.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	// openCodeReconcileMaxExports bounds the exports one pass may run. A
	// backlog of more than this takes several passes.
	openCodeReconcileMaxExports = 3
	// openCodeExportMaxStdout caps one export's stdout. Past it the export
	// merges nothing: the session is remembered for a retry (or, when the
	// stream already counted it, simply stepped over).
	openCodeExportMaxStdout = 8 << 20
	// openCodeSessionListMaxStdout caps the session list. Generous relative to
	// a list of ids and timestamps, and small enough that a runaway cannot be
	// buffered.
	openCodeSessionListMaxStdout = 4 << 20
	// openCodeSessionListMaxSessions bounds how many listed sessions one pass
	// considers, newest-activity first.
	openCodeSessionListMaxSessions = 512
	// openCodeExportMaxChildren bounds the delegated child sessions ONE listed
	// session may pull in. A subagent session is short (one turn's worth of
	// messages), so the cost of following them is small next to the parent's
	// own export — but it must still be bounded, and past the bound the day is
	// a lower bound rather than a silently low number.
	openCodeExportMaxChildren = 8
	// openCodeExportMaxChildDepth bounds how deep that descent goes, so a
	// subagent that itself delegates cannot walk a tree.
	openCodeExportMaxChildDepth = 3
)

// A session exported while its turn was still running is read ONCE more later.
// OpenCode sets a session's `updated` when the prompt arrives and persists the
// turn's later parts and messages without necessarily advancing it, so a pass
// that overlaps a direct/TUI turn can commit the session at the cursor with
// most of the turn still unwritten — and neither the cursor nor a `skipped`
// retry, both keyed on `updated`, would ever look at it again.
const (
	// openCodeRecheckDelay is how long a re-read waits, so the turn has time to
	// finish writing instead of being chased part by part.
	openCodeRecheckDelay = 2 * time.Minute
	// openCodeSessionMaybeActiveWindow bounds which sessions can qualify at
	// all: one whose `updated` is further behind the pass than this was already
	// finished when we read it, and re-reading it would be an export for
	// nothing.
	openCodeSessionMaybeActiveWindow = 15 * time.Minute
)

// openCodeReconcileResult is one pass's closed answer.
type openCodeReconcileResult struct {
	Outcome string
	// Exported is how many exports this pass ran; Remaining is how many
	// candidates it left behind.
	Exported  int
	Remaining int
}

// openCodeReconcileBudgetValue bounds a whole pass, `session list` included. A
// var so a test can pin it small.
var openCodeReconcileBudgetValue = 20 * time.Second

// openCodeReconcileGroup is the single flight every pass shares — the debt
// worker and the Refresh click alike.
var openCodeReconcileGroup singleflight.Group

// openCodeUsageBinary resolves the CLI a pass runs. A seam so a test never
// reaches a real install.
var openCodeUsageBinary = resolveOpenCodeExecutable

// openCodeRunCommand runs one bounded OpenCode command and returns its stdout
// and whether the capture buffer FILLED (i.e. there was more to say than we
// kept). A var so tests never spawn a real CLI.
var openCodeRunCommand = func(ctx context.Context, path string, args []string, limit int) (stdout []byte, filled bool, err error) {
	cmd, launchErr := newOpenCodeCmd(ctx, openCodeUsageLaunch(path, args))
	if launchErr != nil {
		return nil, false, launchErr
	}
	out := &boundedBuffer{limit: limit}
	cmd.Stdout = out
	// stderr is dropped unread: this path classifies by exit status and by
	// whether stdout decoded, so there is nothing a message could add that is
	// worth the risk of retaining it.
	cmd.Stderr = &boundedBuffer{limit: 0}
	runErr := cmd.Run()
	captured := out.Bytes()
	return captured, len(captured) >= limit, runErr
}

// openCodeUsageLaunch is the spawn spec every reconcile command runs under.
// Maintenance pins self-update off (a usage read must never BE an upgrade) and
// the terminal title with it (an OSC write on stdout would sit in front of the
// JSON). A pure function so a test can assert the spec without a child.
func openCodeUsageLaunch(path string, args []string) openCodeLaunch {
	return openCodeLaunch{
		Path:        path,
		Args:        args,
		Env:         sanitizeOpenCodeEnv(os.Environ()),
		Maintenance: true,
	}
}

// reconcileOpenCodeUsageOnce runs ONE bounded pass, sharing a flight with any
// pass already running. Every pass stamps lastPassStartedAtMs and records its
// outcome, so the gather's nudge and the zero-row rule read the same facts.
func reconcileOpenCodeUsageOnce(parent context.Context, now time.Time) openCodeReconcileResult {
	result, _ := reconcileOpenCodeUsageLeading(parent, now)
	return result
}

// reconcileOpenCodeUsageLeading is reconcileOpenCodeUsageOnce that also reports
// whether THIS caller ran the pass. A caller that joined someone else's flight
// gets the same result but must not book it: the leader already does, and two
// callers charging one shared failure would spend two of the debt's attempts.
// (singleflight's own `shared` flag cannot say this — it is true for the leader
// too once anyone has joined.)
func reconcileOpenCodeUsageLeading(parent context.Context, now time.Time) (openCodeReconcileResult, bool) {
	led := false
	v, _, _ := openCodeReconcileGroup.Do("reconcile", func() (any, error) {
		led = true
		return runOpenCodeReconcilePass(parent, now), nil
	})
	result, _ := v.(openCodeReconcileResult)
	return result, led
}

func runOpenCodeReconcilePass(parent context.Context, now time.Time) openCodeReconcileResult {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, openCodeReconcileBudgetValue)
	defer cancel()

	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		l.LastPassStartedAtMs = now.UnixMilli()
		return openCodeLedgerEdit{Changed: true}
	})

	path := openCodeUsageBinary()
	if path == "" {
		return openCodeFinishPass(openCodeReconcileResult{Outcome: openCodeReconcileLaunchError})
	}

	sessions, truncated, listOutcome := listOpenCodeSessionsForUsage(ctx, path)
	if listOutcome != "" {
		return openCodeFinishPass(openCodeReconcileResult{Outcome: listOutcome})
	}
	if truncated {
		// More sessions than one pass will ever consider: whatever the cut
		// dropped is uncounted, so say so rather than letting the totals read
		// as complete. The marker must reach disk: a pass that went on to end
		// no_change would pay the debt for sessions it never counted, with no
		// notice on the card.
		_, persisted := updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
			return openCodeLedgerEdit{Changed: openCodeMarkTodayPartial(l, openCodeUsageNow())}
		})
		if !persisted {
			return openCodeFinishPass(openCodeReconcileResult{Outcome: openCodeReconcileWriteError})
		}
	}

	ledger := readOpenCodeUsageLedger()
	plan := planOpenCodeReconcile(ledger, sessions, now)
	result := openCodeReconcileResult{}
	committed, skippedChanged := false, false
	// settled counts exports that reached a commit decision — merged, or
	// recorded as over-cap. Everything else in the plan is still a candidate
	// next pass, which is what `remaining` has to say: counting a FAILED export
	// as done under-reported the backlog in the pass log.
	settled := 0
	remaining := func() int { return plan.remaining + len(plan.exports) - settled }

	if !truncated && len(plan.staleRechecks) > 0 {
		// Best effort: a record that fails to clear only costs a later pass,
		// never a figure.
		updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
			edit := openCodeLedgerEdit{}
			for _, hash := range plan.staleRechecks {
				if openCodeForgetRecheckHash(l, hash) {
					edit.Changed = true
				}
			}
			return edit
		})
	}

	if plan.cursorFloorMs > ledger.ReconcileCursorMs || plan.undatable > 0 {
		_, persisted := updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
			edit := openCodeLedgerEdit{}
			// Step the cursor past everything older than the retention window
			// in ONE write, so months of history cost no exports at all.
			if plan.cursorFloorMs > l.ReconcileCursorMs {
				l.ReconcileCursorMs, l.ReconcileCursorTies, edit.Changed = plan.cursorFloorMs, nil, true
			}
			if plan.undatable > 0 && openCodeMarkTodayPartial(l, openCodeUsageNow()) {
				edit.Changed = true
			}
			return edit
		})
		// An undatable session exports nothing, so the partial marker is the
		// ONLY record that the day is a lower bound. A refused write must fail
		// the pass for the same reason the truncated list does: ending
		// no_change would pay the debt and present the day as complete.
		if !persisted && plan.undatable > 0 {
			result.Outcome, result.Remaining = openCodeReconcileWriteError, remaining()
			return openCodeFinishPass(result)
		}
	}

	for _, candidate := range plan.exports {
		// The budget, and a shutdown: a pass that started just before teardown
		// must not keep spawning children through it. The schedule is on disk,
		// so the next process picks the backlog up.
		if ctx.Err() != nil || IsShutdownInProgress() {
			if settled == 0 {
				result.Outcome, result.Remaining = openCodeReconcileTimeout, remaining()
				return openCodeFinishPass(result)
			}
			// Some sessions were committed before it ran out; what is left is a
			// backlog for the next pass, not a failure.
			break
		}
		observations, filled, childrenPartial, outcome := exportOpenCodeSessionUsage(ctx, path, candidate.id)
		result.Exported++
		landed := false
		if outcome != "" {
			// A timeout, a launch failure or unrecognised output mid-pass: stop,
			// keeping whatever landed before it.
			result.Outcome, result.Remaining = outcome, remaining()
			return openCodeFinishPass(result)
		}
		commitMs := openCodeUsageNow().UnixMilli()
		_, persisted := updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
			edit := openCodeLedgerEdit{}
			if filled {
				// The export was over the cap, so it merged nothing. A session
				// the stream already counted is simply stepped over; any other
				// is remembered for a retry and makes the day a lower bound.
				if openCodeSessionStreamCaptured(*l, openCodeUsageHash(candidate.id, "")) {
					edit.Changed = true
				} else {
					openCodeRememberSkippedSession(l, candidate.id, candidate.updatedMs)
					openCodeMarkTodayPartial(l, openCodeUsageNow())
					edit.Changed, skippedChanged = true, true
				}
			} else {
				edit.TotalsChanged = openCodeMergeObservations(l, observations, commitMs)
				edit.Changed = openCodeForgetSkippedSession(l, candidate.id) || edit.TotalsChanged
				// A session we may have read mid-turn is booked for ONE later
				// re-read, so the rest of that turn is still counted.
				if openCodeScheduleRecheck(l, candidate, plan.recheck[candidate.id], edit.TotalsChanged, commitMs) {
					edit.Changed = true
				}
				// A delegated session this pass could not read is activity the
				// totals miss, so the card carries the lower-bound notice.
				if childrenPartial && openCodeMarkTodayPartial(l, openCodeUsageNow()) {
					edit.Changed = true
				}
			}
			// The cursor advances past EVERY attempted session, over-cap
			// included, so no outcome can ever stall on one.
			if openCodeAdvanceReconcileCursor(l, candidate) {
				edit.Changed = true
			}
			if edit.Changed || edit.TotalsChanged {
				landed = true
			}
			return edit
		})
		if !persisted {
			// A refused write leaves the cursor where it was, so the session is
			// still a candidate. Settling it anyway could end the pass as
			// no_change — a success that pays the debt for figures that never
			// reached disk — so the pass stops as a failure instead.
			result.Outcome, result.Remaining = openCodeReconcileWriteError, remaining()
			return openCodeFinishPass(result)
		}
		// `committed` means it reached DISK.
		if landed {
			committed = true
		}
		settled++
	}

	result.Remaining = remaining()
	switch {
	case result.Remaining > 0 && (committed || skippedChanged):
		result.Outcome = openCodeReconcileMore
	case result.Remaining > 0:
		// Unreachable: every export that reaches the commit block either merges
		// or records a skip, and one that does not returns above — so a pass
		// cannot leave candidates behind without having moved something. Kept
		// as no_change rather than `more` so an impossible state cannot book an
		// endless continuation.
		result.Outcome = openCodeReconcileNoChange
	case committed:
		result.Outcome = openCodeReconcileOK
	default:
		result.Outcome = openCodeReconcileNoChange
	}
	return openCodeFinishPass(result)
}

// openCodeFinishPass records the outcome and, for a pass that reached the end
// of the candidate list, the success stamp. A timeout, launch failure or
// unsupported output never sets it, even when nothing moved.
//
// A success whose stamp was REFUSED is not a success: the stamp is what pays
// the debt's purpose — for an empty store it is the only state that makes the
// zero row publishable — so a refused write becomes write_error, which spends
// budget and leaves the debt open for its retry instead of clearing it against
// state that never reached disk.
func openCodeFinishPass(result openCodeReconcileResult) openCodeReconcileResult {
	completedAtMs := openCodeUsageNow().UnixMilli()
	record := func(outcome string) bool {
		_, persisted := updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
			l.LastPassOutcome = outcome
			if openCodeReconcileSucceeded(outcome) {
				l.LastSuccessfulReconcileAtMs = completedAtMs
				// Committed progress clears the continuation failure run.
				l.ContinuationFailures, l.ContinuationFirstFailureAtMs = 0, 0
			}
			return openCodeLedgerEdit{Changed: true}
		})
		return persisted
	}
	if !record(result.Outcome) && openCodeReconcileSucceeded(result.Outcome) {
		result.Outcome = openCodeReconcileWriteError
		// Best effort: the same disk just refused a write, so this one may be
		// refused too. The outcome the CALLER sees is what schedules the retry.
		record(result.Outcome)
	}
	return result
}

/* ──────────────────────────── candidate plan ────────────────────────── */

type openCodeSessionRow struct {
	id        string
	updatedMs int64
}

type openCodeReconcilePlan struct {
	exports   []openCodeSessionRow
	remaining int
	// recheck names, by session id, the exports that are a DUE re-read of a
	// session a previous pass may have caught mid-turn. The commit path clears
	// their record, and reschedules only one that raised a figure.
	recheck map[string]bool
	// cursorFloorMs is the newest `updated` among sessions this plan will NEVER
	// export because they fall outside the ledger's retention window. The pass
	// advances the cursor to it so they are never considered again.
	cursorFloorMs int64
	// undatable counts listed sessions whose `updated` we could not read: we
	// cannot tell whether they changed, so they are skipped and the day is a
	// lower bound.
	undatable int
	// staleRechecks holds the hashes of DUE re-read records whose session the
	// list no longer names at all — one OpenCode deleted, or moved out of the
	// layout we can read. Nothing will ever export them, so nothing would ever
	// clear them: a pass would end ok, book a wake-up for the record, and the
	// two would chase each other until the retention prune. They are dropped
	// instead, and only from a list that was NOT truncated.
	staleRechecks []string
}

// planOpenCodeReconcile picks this pass's exports: changed sessions
// OLDEST-FIRST, plus any remembered over-cap session whose listed `updated` is
// now later than the stored one — whatever the cursor says, because a cursor
// already past it would otherwise never look again.
//
// While sessions beyond the cursor remain, retries take at most ONE of the
// export slots, so the cursor always moves. The two queues are then merged
// oldest-first, so a retry can never advance the cursor over a fresh row this
// pass did not reach.
//
// Two whole classes of session are never exported at all:
//
//   - One whose `updated` falls outside the ledger's RETENTION window. A
//     session's `updated` is at least as new as every message in it, so every
//     figure such an export could carry would be dropped by the day check on
//     the way in — and the export is the expensive part (seconds of CPU, up to
//     openCodeExportMaxStdout of stdout). Without this, a first install on a
//     machine with months of OpenCode history would export the whole archive to
//     learn nothing. The cursor is advanced past them instead.
//   - One whose `updated` we could not read. There is no way to tell whether it
//     changed, so exporting it on every pass would be an unbounded repeat; it
//     is skipped and counted, and the caller marks the day a lower bound.
func planOpenCodeReconcile(ledger openCodeUsageLedger, sessions []openCodeSessionRow, now time.Time) openCodeReconcilePlan {
	stored := map[string]int64{}
	for _, entry := range ledger.Skipped {
		stored[entry.SessionHash] = entry.UpdatedMs
	}
	rechecks := map[string]int64{}
	for _, entry := range ledger.Rechecks {
		rechecks[entry.SessionHash] = entry.DueAtMs
	}
	plan := openCodeReconcilePlan{recheck: map[string]bool{}}
	retentionFloorMs := openCodeRetentionFloor(now).UnixMilli()
	listed := map[string]bool{}
	var fresh, retries []openCodeSessionRow
	for _, row := range sessions {
		if row.id == "" {
			continue
		}
		listed[openCodeUsageHash(row.id, "")] = true
		if row.updatedMs <= 0 {
			plan.undatable++
			continue
		}
		if row.updatedMs < retentionFloorMs {
			if row.updatedMs > plan.cursorFloorMs {
				plan.cursorFloorMs = row.updatedMs
			}
			continue
		}
		hash := openCodeUsageHash(row.id, "")
		if held, ok := stored[hash]; ok {
			if row.updatedMs > held {
				retries = append(retries, row)
			}
			continue
		}
		if due, ok := rechecks[hash]; ok && due <= now.UnixMilli() {
			// A re-read is a revisit, so it rides the retry queue's single slot
			// and its oldest-first merge: it can never cost the pass a fresh
			// row, nor carry the cursor past one.
			plan.recheck[row.id] = true
			retries = append(retries, row)
			continue
		}
		if openCodeBeyondReconcileCursor(ledger, row.updatedMs, hash) {
			fresh = append(fresh, row)
		}
	}
	for _, entry := range ledger.Rechecks {
		if entry.DueAtMs > now.UnixMilli() {
			continue
		}
		// Not listed at all, or now remembered as over-cap: either way no
		// export will ever reach the re-read's commit path to clear it, and the
		// `skipped` retry owns the second case.
		if _, held := stored[entry.SessionHash]; !listed[entry.SessionHash] || held {
			plan.staleRechecks = append(plan.staleRechecks, entry.SessionHash)
		}
	}
	sort.SliceStable(fresh, func(i, j int) bool { return fresh[i].updatedMs < fresh[j].updatedMs })
	sort.SliceStable(retries, func(i, j int) bool { return retries[i].updatedMs < retries[j].updatedMs })

	retrySlots := len(retries)
	if len(fresh) > 0 && retrySlots > 1 {
		retrySlots = 1
	}
	// Merge the two queues OLDEST-FIRST, within the retry budget. Taking the
	// retries first would let a retry newer than the fresh rows carry the cursor
	// past the fresh rows this pass has no slots left for: they would fall
	// behind the cursor and never be planned again, even though `remaining`
	// reported them as a backlog. A retry therefore only takes a slot while it
	// is the oldest candidate left, so every row the cursor steps over has
	// already been exported.
	ri, fi := 0, 0
	for len(plan.exports) < openCodeReconcileMaxExports {
		switch {
		case ri < retrySlots && (fi >= len(fresh) || retries[ri].updatedMs <= fresh[fi].updatedMs):
			plan.exports = append(plan.exports, retries[ri])
			ri++
		case fi < len(fresh):
			plan.exports = append(plan.exports, fresh[fi])
			fi++
		default:
			plan.remaining = len(fresh) + len(retries) - len(plan.exports)
			return plan
		}
	}
	plan.remaining = len(fresh) + len(retries) - len(plan.exports)
	return plan
}

// openCodeBeyondReconcileCursor reports whether a listed session is past the
// cursor. A session whose `updated` EQUALS the cursor is past it unless it is
// one of the ties already committed there: a pass that ran out of export slots
// inside a group of sessions sharing one stamp must not step over the rest.
func openCodeBeyondReconcileCursor(ledger openCodeUsageLedger, updatedMs int64, hash string) bool {
	if updatedMs != ledger.ReconcileCursorMs {
		return updatedMs > ledger.ReconcileCursorMs
	}
	for _, tie := range ledger.ReconcileCursorTies {
		if tie == hash {
			return false
		}
	}
	return true
}

// openCodeAdvanceReconcileCursor moves the cursor to a committed session's
// `updated` and records it among the ties at that stamp. A retried session
// older than the cursor leaves it alone. The ties are bounded by the session
// list's own cap, so a pathological list cannot grow the ledger.
func openCodeAdvanceReconcileCursor(ledger *openCodeUsageLedger, candidate openCodeSessionRow) bool {
	hash := openCodeUsageHash(candidate.id, "")
	switch {
	case candidate.updatedMs > ledger.ReconcileCursorMs:
		ledger.ReconcileCursorMs = candidate.updatedMs
		ledger.ReconcileCursorTies = []string{hash}
		return true
	case candidate.updatedMs == ledger.ReconcileCursorMs &&
		openCodeBeyondReconcileCursor(*ledger, candidate.updatedMs, hash) &&
		len(ledger.ReconcileCursorTies) < openCodeSessionListMaxSessions:
		ledger.ReconcileCursorTies = append(ledger.ReconcileCursorTies, hash)
		return true
	}
	return false
}

// openCodeRememberSkippedSession records an over-cap session for a retry,
// evicting the oldest in-retention entry when the set is full. An evicted
// session is never retried, which is why the day stays partial.
func openCodeRememberSkippedSession(ledger *openCodeUsageLedger, sessionID string, updatedMs int64) {
	hash := openCodeUsageHash(sessionID, "")
	for i := range ledger.Skipped {
		if ledger.Skipped[i].SessionHash == hash {
			ledger.Skipped[i].UpdatedMs = updatedMs
			return
		}
	}
	ledger.Skipped = append(ledger.Skipped, openCodeSkippedSession{SessionHash: hash, UpdatedMs: updatedMs})
	openCodePruneSkipped(ledger, openCodeUsageNow())
}

// openCodeForgetSkippedSession drops a session that has now exported within the
// cap.
func openCodeForgetSkippedSession(ledger *openCodeUsageLedger, sessionID string) bool {
	hash := openCodeUsageHash(sessionID, "")
	for i := range ledger.Skipped {
		if ledger.Skipped[i].SessionHash != hash {
			continue
		}
		ledger.Skipped = append(ledger.Skipped[:i], ledger.Skipped[i+1:]...)
		if len(ledger.Skipped) == 0 {
			ledger.Skipped = nil
		}
		return true
	}
	return false
}

// openCodeScheduleRecheck keeps the re-read record for a committed session:
// the entry it was planned from is cleared, and a new one booked only while the
// session may still be producing figures. Reports whether anything changed.
//
// A first export qualifies on the session's own age: one updated within
// openCodeSessionMaybeActiveWindow of this pass may have been mid-turn. A
// re-read qualifies on EVIDENCE instead — it is booked again only when it
// raised a figure, which is what proves the turn was still running, and is what
// stops a quiet session being exported forever.
func openCodeScheduleRecheck(ledger *openCodeUsageLedger, candidate openCodeSessionRow, wasRecheck, raised bool, commitMs int64) bool {
	changed := openCodeForgetRecheckSession(ledger, candidate.id)
	switch {
	case wasRecheck && !raised:
		// The re-read found nothing new: the turn is over.
		return changed
	case wasRecheck:
	case candidate.updatedMs <= 0 ||
		commitMs-candidate.updatedMs >= openCodeSessionMaybeActiveWindow.Milliseconds():
		// Last written long before this pass, so nothing was in flight.
		return changed
	}
	openCodeRememberRecheckSession(ledger, candidate.id, candidate.updatedMs, commitMs+openCodeRecheckDelay.Milliseconds())
	return true
}

// openCodeRememberRecheckSession books one later re-read of a session, bounded
// by openCodeLedgerMaxRechecks (the prune keeps the newest).
func openCodeRememberRecheckSession(ledger *openCodeUsageLedger, sessionID string, updatedMs, dueAtMs int64) {
	hash := openCodeUsageHash(sessionID, "")
	for i := range ledger.Rechecks {
		if ledger.Rechecks[i].SessionHash == hash {
			ledger.Rechecks[i].UpdatedMs, ledger.Rechecks[i].DueAtMs = updatedMs, dueAtMs
			return
		}
	}
	ledger.Rechecks = append(ledger.Rechecks, openCodeRecheckSession{
		SessionHash: hash, UpdatedMs: updatedMs, DueAtMs: dueAtMs,
	})
	openCodePruneRechecks(ledger, openCodeUsageNow())
}

// openCodeForgetRecheckSession drops a session's re-read record.
func openCodeForgetRecheckSession(ledger *openCodeUsageLedger, sessionID string) bool {
	return openCodeForgetRecheckHash(ledger, openCodeUsageHash(sessionID, ""))
}

// openCodeForgetRecheckHash drops a re-read record by hash — what the plan
// carries for a record whose session the list no longer names.
func openCodeForgetRecheckHash(ledger *openCodeUsageLedger, hash string) bool {
	for i := range ledger.Rechecks {
		if ledger.Rechecks[i].SessionHash != hash {
			continue
		}
		ledger.Rechecks = append(ledger.Rechecks[:i], ledger.Rechecks[i+1:]...)
		if len(ledger.Rechecks) == 0 {
			ledger.Rechecks = nil
		}
		return true
	}
	return false
}

/* ─────────────────────────────── commands ───────────────────────────── */

// listOpenCodeSessionsForUsage asks OpenCode what changed. `truncated` reports a
// list longer than openCodeSessionListMaxSessions, of which only the most
// recently active are considered — the day is then a lower bound, because a
// changed session the cut dropped is never exported. The last return is "" on
// success, or the closed outcome the pass must report.
func listOpenCodeSessionsForUsage(ctx context.Context, path string) (rows []openCodeSessionRow, truncated bool, outcome string) {
	// OpenCode's list service defaults an unspecified limit to 100 rows, so the
	// unflagged command silently hides everything older than the newest 100 —
	// and those rows fall behind the cursor once the ones it DID return are
	// committed, with nothing marking the loss. Ask for one more than a pass
	// will consider, so a longer list still shows up as truncated below.
	rows, truncated, outcome = listOpenCodeSessionsWith(ctx, path, openCodeSessionListArgs(openCodeSessionListMaxSessions+1))
	if outcome == openCodeReconcileUnsupported && ctx.Err() == nil {
		// A build that predates `--max-count` rejects the whole command, and
		// `unsupported` RETIRES the debt — so the older spelling is worth one
		// retry before standing the feature down.
		rows, truncated, outcome = listOpenCodeSessionsWith(ctx, path, openCodeSessionListArgs(0))
	}
	return rows, truncated, outcome
}

// openCodeSessionListArgs is the list command. A non-positive maxCount is the
// older spelling, without the flag.
func openCodeSessionListArgs(maxCount int) []string {
	args := []string{"session", "list", "--format", "json"}
	if maxCount > 0 {
		args = append(args, "--max-count", strconv.Itoa(maxCount))
	}
	return args
}

func listOpenCodeSessionsWith(ctx context.Context, path string, args []string) (rows []openCodeSessionRow, truncated bool, outcome string) {
	stdout, filled, err := openCodeRunCommand(ctx, path, args, openCodeSessionListMaxStdout)
	if ctx.Err() != nil {
		return nil, false, openCodeReconcileTimeout
	}
	var parsed []openCodeSessionRow
	if filled {
		// A list over the cap is cut mid-document, so it never parses whole —
		// and reading that as `unsupported` would RETIRE the debt on exactly
		// the installs with the most history, with no lower-bound notice. It
		// is a bounded backlog instead: keep every row that arrived intact and
		// report the list truncated, which marks the day partial.
		parsed, truncated = salvageOpenCodeSessionList(stdout), true
	} else {
		// The ANSWER decides, not the exit status: a build that prints a
		// usable list and then exits non-zero (an update notice on stderr, a
		// warning) must not be read as one that cannot answer at all —
		// `unsupported` RETIRES the debt, so a transient non-zero exit would
		// permanently drop the reading.
		//
		// A clean exit that printed nothing is an EMPTY store: OpenCode's
		// handler returns before printing when there are no sessions. Reading
		// it as `unsupported` would retire the debt and never record the
		// successful pass the zero row waits for. A non-zero silent exit is
		// still classified below.
		var ok bool
		if err == nil && len(bytes.TrimSpace(stdout)) == 0 {
			parsed = nil
		} else if parsed, ok = parseOpenCodeSessionList(stdout); !ok {
			return nil, false, openCodeCommandOutcome(err)
		}
	}
	if len(parsed) > openCodeSessionListMaxSessions {
		sort.SliceStable(parsed, func(i, j int) bool { return parsed[i].updatedMs > parsed[j].updatedMs })
		parsed, truncated = parsed[:openCodeSessionListMaxSessions], true
	}
	return parsed, truncated, ""
}

// exportOpenCodeSessionUsage exports one session and folds its assistant
// messages, following the CHILD sessions its tool calls delegated to (a
// subagent's tokens live there, and nothing else in this feature can see them).
// `filled` reports the session's own export over the stdout cap (which merges
// nothing); `partial` reports a child left uncounted, which makes the day a
// lower bound; the last return is "" on success or the pass's closed outcome.
func exportOpenCodeSessionUsage(ctx context.Context, path, sessionID string) (
	observed []openCodeObservedMessage, filled, partial bool, outcome string) {
	observed, children, filled, outcome := exportOpenCodeOneSession(ctx, path, sessionID)
	if filled || outcome != "" {
		return observed, filled, false, outcome
	}
	childObserved, partial := exportOpenCodeChildSessions(ctx, path, sessionID, children)
	return append(observed, childObserved...), false, partial, ""
}

// exportOpenCodeOneSession is one `export` call, with no descent.
func exportOpenCodeOneSession(ctx context.Context, path, sessionID string) (
	observed []openCodeObservedMessage, children []string, filled bool, outcome string) {
	stdout, filled, err := openCodeRunCommand(ctx, path,
		[]string{"export", sessionID}, openCodeExportMaxStdout)
	if ctx.Err() != nil {
		return nil, nil, filled, openCodeReconcileTimeout
	}
	if filled {
		return nil, nil, true, ""
	}
	observations, children, ok := parseOpenCodeExport(stdout, sessionID)
	if !ok {
		return nil, nil, false, openCodeCommandOutcome(err)
	}
	return observations, children, false, ""
}

// exportOpenCodeChildSessions exports the subagent sessions a parent export
// named, breadth-first and bounded by openCodeExportMaxChildren exports and
// openCodeExportMaxChildDepth levels. Figures merge with the parent's, in the
// parent's single commit, so a child is never half-counted.
//
// A child that cannot be read does NOT fail the pass: its parent exported
// fine, and classifying it `unsupported` would retire the debt over a session
// the feature only learned about from a tool part. It makes the day a lower
// bound instead, which is what `partial` reports.
func exportOpenCodeChildSessions(ctx context.Context, path, parentID string, seeds []string) (
	observed []openCodeObservedMessage, partial bool) {
	queue := make([]string, 0, len(seeds))
	depth := map[string]int{}
	seen := map[string]bool{strings.TrimSpace(parentID): true}
	for _, seed := range seeds {
		if seen[seed] {
			continue
		}
		seen[seed], depth[seed] = true, 1
		queue = append(queue, seed)
	}
	exported := 0
	for len(queue) > 0 {
		id, level := queue[0], depth[queue[0]]
		queue = queue[1:]
		if exported >= openCodeExportMaxChildren {
			return observed, true
		}
		if ctx.Err() != nil || IsShutdownInProgress() {
			// The parent's own figures still commit; the rest of the tree is
			// uncounted, and the day says so.
			return observed, true
		}
		childObserved, grandchildren, filled, outcome := exportOpenCodeOneSession(ctx, path, id)
		exported++
		if filled || outcome != "" {
			partial = true
			continue
		}
		observed = append(observed, childObserved...)
		if level >= openCodeExportMaxChildDepth {
			if len(grandchildren) > 0 {
				partial = true
			}
			continue
		}
		for _, next := range grandchildren {
			if seen[next] {
				continue
			}
			seen[next], depth[next] = true, level+1
			queue = append(queue, next)
		}
	}
	return observed, partial
}

// openCodeCommandOutcome classifies a command whose OUTPUT we could not read: a
// non-zero exit is a build without the subcommand (`unsupported`, which retires
// the debt because no retry can fix it), anything else — a missing binary, an
// unrenderable Windows shim, a pipe failure — is `launch_error`, which the
// ladder retries. A clean exit that printed nothing we recognise is
// `unsupported` for the same reason as a non-zero one.
func openCodeCommandOutcome(err error) string {
	if err == nil {
		return openCodeReconcileUnsupported
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return openCodeReconcileUnsupported
	}
	return openCodeReconcileLaunchError
}

/* ──────────────────────────────── decode ────────────────────────────── */

// openCodeSessionListJSON is one row of `session list --format json`. Every
// shape is read permissively: the list has been spelled as a bare array and as
// `{"sessions":[…]}`, and the timestamps as `time.updated` or a flat `updated`.
type openCodeSessionListJSON struct {
	ID   string `json:"id"`
	Time struct {
		Created json.RawMessage `json:"created"`
		Updated json.RawMessage `json:"updated"`
	} `json:"time"`
	Updated json.RawMessage `json:"updated"`
	Created json.RawMessage `json:"created"`
}

// parseOpenCodeSessionList decodes the session list. ok=false means the output
// is not a shape we recognise, which is the `unsupported` outcome.
func parseOpenCodeSessionList(stdout []byte) ([]openCodeSessionRow, bool) {
	body := openCodeJSONBody(stdout)
	if len(body) == 0 {
		return nil, false
	}
	var rows []openCodeSessionListJSON
	if json.Unmarshal(body, &rows) != nil {
		var wrapped struct {
			Sessions []openCodeSessionListJSON `json:"sessions"`
		}
		if json.Unmarshal(body, &wrapped) != nil || wrapped.Sessions == nil {
			return nil, false
		}
		rows = wrapped.Sessions
	}
	out := make([]openCodeSessionRow, 0, len(rows))
	for _, row := range rows {
		out = appendOpenCodeSessionRow(out, row)
	}
	return out, true
}

func appendOpenCodeSessionRow(out []openCodeSessionRow, row openCodeSessionListJSON) []openCodeSessionRow {
	if strings.TrimSpace(row.ID) == "" {
		return out
	}
	updated := openCodeFirstEpochMs(row.Time.Updated, row.Updated, row.Time.Created, row.Created)
	return append(out, openCodeSessionRow{id: row.ID, updatedMs: updated})
}

// salvageOpenCodeSessionList decodes the COMPLETE rows at the front of a
// session list the capture buffer cut short, in either spelling (a bare array
// or `{"sessions":[…]}`), stopping at the first row the cut broke. Nil when
// nothing intact arrived; the caller still reports the list truncated.
func salvageOpenCodeSessionList(stdout []byte) []openCodeSessionRow {
	body := openCodeJSONBody(stdout)
	if len(body) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	open, err := dec.Token()
	if err != nil {
		return nil
	}
	if open == json.Delim('{') {
		// Walk to the `sessions` key, stepping over any other value.
		for {
			key, keyErr := dec.Token()
			if keyErr != nil || key == json.Delim('}') {
				return nil
			}
			if key == "sessions" {
				break
			}
			var skip json.RawMessage
			if dec.Decode(&skip) != nil {
				return nil
			}
		}
		if open, err = dec.Token(); err != nil {
			return nil
		}
	}
	if open != json.Delim('[') {
		return nil
	}
	var out []openCodeSessionRow
	for dec.More() {
		var row openCodeSessionListJSON
		if dec.Decode(&row) != nil {
			break
		}
		out = appendOpenCodeSessionRow(out, row)
	}
	return out
}

type openCodeExportJSON struct {
	Messages []openCodeExportMessageJSON `json:"messages"`
}

type openCodeExportMessageJSON struct {
	Info struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionID"`
		Role      string `json:"role"`
		Time      struct {
			Created json.RawMessage `json:"created"`
		} `json:"time"`
		Tokens openCodeTokensJSON `json:"tokens"`
		Cost   json.RawMessage    `json:"cost"`
	} `json:"info"`
	// Parts holds each step's own figures. OpenCode OVERWRITES info.tokens on
	// every step (it ends as the last step's), while each step-finish part
	// keeps that step's usage — so a multi-step turn is only whole when its
	// parts are summed, exactly as the stream tap sums step_finish events.
	Parts []struct {
		Type   string             `json:"type"`
		Tokens openCodeTokensJSON `json:"tokens"`
		Cost   json.RawMessage    `json:"cost"`
		// State.Metadata is where the `task` tool records the CHILD session it
		// delegated to (`sessionId`, beside `parentSessionId`). A subagent's
		// tokens live in that child session, which `session list` never returns
		// (it asks for roots only) and the run stream drops (it keeps only
		// parts whose session is the root) — so this is the only place a
		// reconcile can learn the id. Both nestings are read, because older
		// builds put the metadata on the part itself.
		State struct {
			Metadata openCodePartMetadataJSON `json:"metadata"`
		} `json:"state"`
		Metadata openCodePartMetadataJSON `json:"metadata"`
	} `json:"parts"`
}

// openCodePartMetadataJSON is the part metadata a delegating tool call carries.
// Both spellings of the key have shipped.
type openCodePartMetadataJSON struct {
	SessionID    string `json:"sessionId"`
	SessionIDAlt string `json:"sessionID"`
}

func (m openCodePartMetadataJSON) sessionID() string {
	return firstNonEmpty(strings.TrimSpace(m.SessionID), strings.TrimSpace(m.SessionIDAlt))
}

// parseOpenCodeExport folds an export's assistant messages into observations,
// and names the CHILD sessions its tool calls delegated to, whose own usage
// this export does not carry. ok=false means the output is not a shape we
// recognise.
func parseOpenCodeExport(stdout []byte, sessionID string) ([]openCodeObservedMessage, []string, bool) {
	body := openCodeJSONBody(stdout)
	if len(body) == 0 {
		return nil, nil, false
	}
	var export openCodeExportJSON
	if json.NewDecoder(bytes.NewReader(body)).Decode(&export) != nil {
		// Some releases print the messages as a bare array.
		var bare []openCodeExportMessageJSON
		if json.Unmarshal(body, &bare) != nil {
			return nil, nil, false
		}
		export.Messages = bare
	}
	// A recognised object with no `messages` key is an EMPTY session — a real,
	// readable answer. Treating it as unsupported would retire the debt (and
	// stand down the whole feature) on a session that simply holds no
	// assistant turn.
	out := make([]openCodeObservedMessage, 0, len(export.Messages))
	var children []string
	seen := map[string]bool{strings.TrimSpace(sessionID): true}
	for _, message := range export.Messages {
		info := message.Info
		// A delegated child is named on a tool part, whatever the message's
		// role, so it is collected before the assistant-only filter below.
		for _, part := range message.Parts {
			child := firstNonEmpty(part.State.Metadata.sessionID(), part.Metadata.sessionID())
			// One MORE than the descent will export, so a fan-out past the
			// bound is still detectable as a lower bound rather than silently
			// trimmed here — and a pathological export cannot grow this list
			// without limit either.
			if child == "" || seen[child] || len(children) > openCodeExportMaxChildren {
				continue
			}
			seen[child] = true
			children = append(children, child)
		}
		if info.ID == "" {
			continue
		}
		if role := strings.ToLower(strings.TrimSpace(info.Role)); role != "" && role != "assistant" {
			continue
		}
		var steps openCodeMessageUsage
		for _, part := range message.Parts {
			if isOpenCodeStepFinishType(part.Type) {
				openCodeAddStepUsage(&steps, openCodeUsageFromJSON(part.Tokens, part.Cost))
			}
		}
		// The per-field max of the summed steps and info: info alone is the
		// last step's tokens, but it is all an export without parts carries
		// (and its cost is already cumulative).
		usage, _ := openCodeMergeMessageUsage(steps, openCodeUsageFromJSON(info.Tokens, info.Cost))
		out = append(out, openCodeObservedMessage{
			SessionID: firstNonEmpty(info.SessionID, sessionID),
			MessageID: info.ID,
			EventAt:   openCodeFrameEventTime(info.Time.Created),
			Usage:     usage,
		})
	}
	// An export with no assistant message is a real, recognised answer.
	return out, children, true
}

// openCodeJSONBody trims a banner or an updater notice from the front of a
// command's stdout and returns the JSON value, or nil when there is none.
func openCodeJSONBody(stdout []byte) []byte {
	trimmed := bytes.TrimSpace(stdout)
	for len(trimmed) > 0 {
		if trimmed[0] == '{' || trimmed[0] == '[' {
			return trimmed
		}
		newline := bytes.IndexByte(trimmed, '\n')
		if newline < 0 {
			return nil
		}
		trimmed = bytes.TrimSpace(trimmed[newline+1:])
	}
	return nil
}

// openCodeFirstEpochMs is the first readable epoch-millisecond stamp.
func openCodeFirstEpochMs(values ...json.RawMessage) int64 {
	for _, value := range values {
		if len(value) == 0 {
			continue
		}
		var n float64
		if json.Unmarshal(value, &n) == nil && n > 0 {
			return openCodeEpochTime(n).UnixMilli()
		}
		var s string
		if json.Unmarshal(value, &s) == nil {
			if t, err := time.Parse(time.RFC3339, strings.TrimSpace(s)); err == nil {
				return t.UnixMilli()
			}
		}
	}
	return 0
}

/* ───────────────────────── discovery (direct runs) ──────────────────── */

const (
	// openCodeDiscoveryMaxSlugs bounds the project-scoped layouts one walk
	// ranks, newest session activity first.
	openCodeDiscoveryMaxSlugs = 64
	// openCodeDiscoveryMaxStats bounds the whole walk's stat calls, so a data
	// directory with thousands of projects cannot turn a gather into a scan.
	// Room for every ranked slug's directories twice over, roots included.
	openCodeDiscoveryMaxStats = 2 * openCodeDiscoveryDirsPerSlug * openCodeDiscoveryMaxSlugs
	// openCodeDiscoveryDirsPerSlug is how many directories one project-scoped
	// store contributes: session, session/info, message and part.
	openCodeDiscoveryDirsPerSlug = 4
	// openCodeDiscoveryMaxDirEntries bounds how many entries ONE directory
	// contributes. os.ReadDir materialises and sorts every entry, so a project
	// root with tens of thousands of slugs — or a flat session directory with a
	// file per session — would be a large unbounded read on the gather path
	// even though the stat budget above caps what we then look at.
	openCodeDiscoveryMaxDirEntries = 4 * openCodeDiscoveryMaxSlugs
)

// openCodeReadDirBounded lists at most `limit` entries of a directory, in the
// order the filesystem returns them (no sort, unlike os.ReadDir). Nil on any
// error — discovery is best-effort and a missing root is the normal case.
func openCodeReadDirBounded(path string, limit int) []os.DirEntry {
	dir, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = dir.Close() }()
	entries, err := dir.ReadDir(limit)
	if err != nil && len(entries) == 0 {
		return nil
	}
	return entries
}

// openCodeSessionStoreChangedSince reports whether any session directory
// OpenCode might write changed after `since` — the only evidence this feature
// has of a run it never saw (the user's own shell or TUI).
//
// It reads NAMES AND MTIMES ONLY, never file contents. It walks the global
// roots openCodeSessionDirs("") already knows plus their immediate children,
// and every `<storage>/project/<slug>` entry's `storage/session` and
// `storage/session/info` — which is the project-scoped layout both native
// capture and the TUI write today. Slugs are ranked by the NEWEST of their
// store directories' mtimes, not the slug directory's own, because a slug
// directory's mtime does not move when a session file inside it is rewritten.
//
// A new turn in an EXISTING session creates no session entry: OpenCode rewrites
// that session's metadata file in place, which moves no directory mtime. What a
// turn always does create is a new message file and a new per-message part
// directory, so each store's `message` and `part` directories are stat-ed too
// (openCodeTurnStoreDirs) — one stat each, never a per-file walk.
func openCodeSessionStoreChangedSince(since time.Time) bool {
	stats := 0
	newest := time.Time{}
	stat := func(path string) (time.Time, bool) {
		if path == "" || stats >= openCodeDiscoveryMaxStats {
			return time.Time{}, false
		}
		stats++
		info, err := os.Stat(path)
		if err != nil {
			return time.Time{}, false
		}
		return info.ModTime(), true
	}
	note := func(path string) {
		if at, ok := stat(path); ok && at.After(newest) {
			newest = at
		}
	}
	for _, dir := range openCodeSessionDirs("") {
		note(dir)
		for _, entry := range openCodeReadDirBounded(dir, openCodeDiscoveryMaxDirEntries) {
			if !entry.IsDir() {
				continue
			}
			note(filepath.Join(dir, entry.Name()))
		}
	}
	if base := openCodeStorageDir(); base != "" {
		for _, dir := range openCodeTurnStoreDirs(filepath.Join(base, "storage")) {
			note(dir)
		}
	}
	for _, slug := range openCodeProjectSessionDirs(&stats) {
		note(slug)
	}
	return !newest.IsZero() && newest.After(since)
}

// openCodeProjectSessionDirs returns the session directories of the most
// recently active project slugs, bounded by openCodeDiscoveryMaxSlugs. The
// ranking stats count toward the caller's budget.
func openCodeProjectSessionDirs(stats *int) []string {
	base := openCodeStorageDir()
	if base == "" {
		return nil
	}
	entries := openCodeReadDirBounded(filepath.Join(base, "project"), openCodeDiscoveryMaxDirEntries)
	if len(entries) == 0 {
		return nil
	}
	type ranked struct {
		dirs []string
		at   time.Time
	}
	candidates := make([]ranked, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || *stats >= openCodeDiscoveryMaxStats {
			continue
		}
		storage := filepath.Join(base, "project", entry.Name(), "storage")
		session := filepath.Join(storage, "session")
		dirs := append([]string{session, filepath.Join(session, "info")}, openCodeTurnStoreDirs(storage)...)
		newest := time.Time{}
		for _, dir := range dirs {
			if *stats >= openCodeDiscoveryMaxStats {
				break
			}
			*stats++
			if info, statErr := os.Stat(dir); statErr == nil && info.ModTime().After(newest) {
				newest = info.ModTime()
			}
		}
		if newest.IsZero() {
			continue
		}
		candidates = append(candidates, ranked{dirs: dirs, at: newest})
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].at.After(candidates[j].at) })
	if len(candidates) > openCodeDiscoveryMaxSlugs {
		candidates = candidates[:openCodeDiscoveryMaxSlugs]
	}
	out := make([]string, 0, len(candidates)*openCodeDiscoveryDirsPerSlug)
	for _, candidate := range candidates {
		out = append(out, candidate.dirs...)
	}
	return out
}

// openCodeTurnStoreDirs are the directories under one `storage` root whose own
// mtime a turn moves: `part` gains a directory for every new message — which is
// what catches a turn in an EXISTING session — and `message` one per session.
func openCodeTurnStoreDirs(storage string) []string {
	return []string{filepath.Join(storage, "message"), filepath.Join(storage, "part")}
}

// openCodeReconcileBudgetForTests shortens a pass's budget and returns the
// restore function. Test seam: a hung child must not spend the shipped 20
// seconds in CI.
func openCodeReconcileBudgetForTests(d time.Duration) func() {
	prev := openCodeReconcileBudgetValue
	openCodeReconcileBudgetValue = d
	return func() { openCodeReconcileBudgetValue = prev }
}
