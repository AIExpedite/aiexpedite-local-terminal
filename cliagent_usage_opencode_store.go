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
//     `export` calls, OLDEST changed session first, each export's stdout capped
//     at openCodeExportMaxStdout and decoded as a stream.
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
	v, _, _ := openCodeReconcileGroup.Do("reconcile", func() (any, error) {
		return runOpenCodeReconcilePass(parent, now), nil
	})
	result, _ := v.(openCodeReconcileResult)
	return result
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

	sessions, listOutcome := listOpenCodeSessionsForUsage(ctx, path)
	if listOutcome != "" {
		return openCodeFinishPass(openCodeReconcileResult{Outcome: listOutcome})
	}

	ledger := readOpenCodeUsageLedger()
	plan := planOpenCodeReconcile(ledger, sessions)
	result := openCodeReconcileResult{Remaining: plan.remaining}
	committed, skippedChanged := false, false

	for _, candidate := range plan.exports {
		if ctx.Err() != nil {
			if result.Exported == 0 && !committed {
				return openCodeFinishPass(openCodeReconcileResult{
					Outcome: openCodeReconcileTimeout, Remaining: plan.remaining,
				})
			}
			// Some sessions were committed before the budget ran out; what is
			// left is a backlog for the next pass, not a failure.
			result.Remaining += len(plan.exports) - result.Exported
			break
		}
		observations, filled, outcome := exportOpenCodeSessionUsage(ctx, path, candidate.id)
		result.Exported++
		switch {
		case outcome == openCodeReconcileTimeout && !committed:
			return openCodeFinishPass(openCodeReconcileResult{
				Outcome: openCodeReconcileTimeout, Exported: result.Exported, Remaining: plan.remaining,
			})
		case outcome != "":
			// A launch or decode failure mid-pass: stop, keeping what landed.
			result.Outcome = outcome
			result.Remaining += len(plan.exports) - result.Exported
			return openCodeFinishPass(result)
		}
		commitMs := openCodeUsageNow().UnixMilli()
		_, _ = updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
			edit := openCodeLedgerEdit{}
			if filled {
				// The export was over the cap, so it merged nothing. A session
				// the stream already counted is simply stepped over; any other
				// is remembered for a retry and makes the day a lower bound.
				if openCodeSessionStreamCaptured(*l, openCodeUsageHash(candidate.id, "")) {
					edit.Changed = true
				} else {
					openCodeRememberSkippedSession(l, candidate.id, candidate.updatedMs)
					edit.Changed = openCodeMarkTodayPartial(l, openCodeUsageNow()) || true
					skippedChanged = true
				}
			} else {
				edit.TotalsChanged = openCodeMergeObservations(l, observations, commitMs)
				edit.Changed = openCodeForgetSkippedSession(l, candidate.id) || edit.TotalsChanged
			}
			// The cursor advances past EVERY attempted session, over-cap
			// included, so no outcome can ever stall on one.
			if candidate.updatedMs > l.ReconcileCursorMs {
				l.ReconcileCursorMs = candidate.updatedMs
				edit.Changed = true
			}
			if edit.Changed || edit.TotalsChanged {
				committed = true
			}
			return edit
		})
	}

	switch {
	case result.Remaining > 0 && (committed || skippedChanged):
		result.Outcome = openCodeReconcileMore
	case result.Remaining > 0:
		// Cannot happen: every attempted export ends in a merge or a skipped
		// record. Treated as `more` only if something moved, so an
		// unreachable no-progress pass reports no_change rather than looping.
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
func openCodeFinishPass(result openCodeReconcileResult) openCodeReconcileResult {
	completedAtMs := openCodeUsageNow().UnixMilli()
	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		l.LastPassOutcome = result.Outcome
		if openCodeReconcileSucceeded(result.Outcome) {
			l.LastSuccessfulReconcileAtMs = completedAtMs
			// Committed progress clears the continuation failure run.
			l.ContinuationFailures, l.ContinuationFirstFailureAtMs = 0, 0
		}
		return openCodeLedgerEdit{Changed: true}
	})
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
}

// planOpenCodeReconcile picks this pass's exports: changed sessions
// OLDEST-FIRST, plus any remembered over-cap session whose listed `updated` is
// now later than the stored one — whatever the cursor says, because a cursor
// already past it would otherwise never look again.
//
// While sessions beyond the cursor remain, retries take at most ONE of the
// export slots, so the cursor always moves.
func planOpenCodeReconcile(ledger openCodeUsageLedger, sessions []openCodeSessionRow) openCodeReconcilePlan {
	stored := map[string]int64{}
	for _, entry := range ledger.Skipped {
		stored[entry.Session] = entry.UpdatedMs
	}
	var fresh, retries []openCodeSessionRow
	for _, row := range sessions {
		if row.id == "" {
			continue
		}
		hash := openCodeUsageHash(row.id, "")
		if held, ok := stored[hash]; ok {
			if row.updatedMs > held {
				retries = append(retries, row)
			}
			continue
		}
		if row.updatedMs > ledger.ReconcileCursorMs {
			fresh = append(fresh, row)
		}
	}
	sort.SliceStable(fresh, func(i, j int) bool { return fresh[i].updatedMs < fresh[j].updatedMs })
	sort.SliceStable(retries, func(i, j int) bool { return retries[i].updatedMs < retries[j].updatedMs })

	retrySlots := len(retries)
	if len(fresh) > 0 && retrySlots > 1 {
		retrySlots = 1
	}
	plan := openCodeReconcilePlan{}
	for i := 0; i < retrySlots && len(plan.exports) < openCodeReconcileMaxExports; i++ {
		plan.exports = append(plan.exports, retries[i])
	}
	for i := 0; i < len(fresh) && len(plan.exports) < openCodeReconcileMaxExports; i++ {
		plan.exports = append(plan.exports, fresh[i])
	}
	plan.remaining = len(fresh) + len(retries) - len(plan.exports)
	return plan
}

// openCodeRememberSkippedSession records an over-cap session for a retry,
// evicting the oldest in-retention entry when the set is full. An evicted
// session is never retried, which is why the day stays partial.
func openCodeRememberSkippedSession(ledger *openCodeUsageLedger, sessionID string, updatedMs int64) {
	hash := openCodeUsageHash(sessionID, "")
	for i := range ledger.Skipped {
		if ledger.Skipped[i].Session == hash {
			ledger.Skipped[i].UpdatedMs = updatedMs
			return
		}
	}
	ledger.Skipped = append(ledger.Skipped, openCodeSkippedSession{Session: hash, UpdatedMs: updatedMs})
	openCodePruneSkipped(ledger, openCodeUsageNow())
}

// openCodeForgetSkippedSession drops a session that has now exported within the
// cap.
func openCodeForgetSkippedSession(ledger *openCodeUsageLedger, sessionID string) bool {
	hash := openCodeUsageHash(sessionID, "")
	for i := range ledger.Skipped {
		if ledger.Skipped[i].Session != hash {
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

/* ─────────────────────────────── commands ───────────────────────────── */

// listOpenCodeSessionsForUsage asks OpenCode what changed. The second return is
// "" on success, or the closed outcome the pass must report.
func listOpenCodeSessionsForUsage(ctx context.Context, path string) ([]openCodeSessionRow, string) {
	stdout, _, err := openCodeRunCommand(ctx, path,
		[]string{"session", "list", "--format", "json"}, openCodeSessionListMaxStdout)
	if outcome := openCodeCommandOutcome(ctx, err); outcome != "" {
		return nil, outcome
	}
	rows, ok := parseOpenCodeSessionList(stdout)
	if !ok {
		// A build that prints a human table rather than JSON: nothing a retry
		// can fix.
		return nil, openCodeReconcileUnsupported
	}
	if len(rows) > openCodeSessionListMaxSessions {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].updatedMs > rows[j].updatedMs })
		rows = rows[:openCodeSessionListMaxSessions]
	}
	return rows, ""
}

// exportOpenCodeSessionUsage exports one session and folds its assistant
// messages. `filled` reports an export over the stdout cap (which merges
// nothing); the third return is "" on success or the pass's closed outcome.
func exportOpenCodeSessionUsage(ctx context.Context, path, sessionID string) ([]openCodeObservedMessage, bool, string) {
	stdout, filled, err := openCodeRunCommand(ctx, path,
		[]string{"export", sessionID}, openCodeExportMaxStdout)
	if outcome := openCodeCommandOutcome(ctx, err); outcome != "" {
		return nil, filled, outcome
	}
	if filled {
		return nil, true, ""
	}
	observations, ok := parseOpenCodeExport(stdout, sessionID)
	if !ok {
		return nil, false, openCodeReconcileUnsupported
	}
	return observations, false, ""
}

// openCodeCommandOutcome classifies a command's failure: the budget (or a hung
// child the deadline killed) is a timeout, anything else a launch error. "" for
// a clean exit.
func openCodeCommandOutcome(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return openCodeReconcileTimeout
	}
	if err == nil {
		return ""
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// A non-zero exit from a command this build does not have.
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
		if strings.TrimSpace(row.ID) == "" {
			continue
		}
		updated := openCodeFirstEpochMs(row.Time.Updated, row.Updated, row.Time.Created, row.Created)
		out = append(out, openCodeSessionRow{id: row.ID, updatedMs: updated})
	}
	return out, true
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
}

// parseOpenCodeExport folds an export's assistant messages into observations.
// ok=false means the output is not a shape we recognise.
func parseOpenCodeExport(stdout []byte, sessionID string) ([]openCodeObservedMessage, bool) {
	body := openCodeJSONBody(stdout)
	if len(body) == 0 {
		return nil, false
	}
	var export openCodeExportJSON
	decoder := json.NewDecoder(bytes.NewReader(body))
	if decoder.Decode(&export) != nil || export.Messages == nil {
		var bare []openCodeExportMessageJSON
		if json.Unmarshal(body, &bare) != nil {
			return nil, false
		}
		export.Messages = bare
	}
	out := make([]openCodeObservedMessage, 0, len(export.Messages))
	for _, message := range export.Messages {
		info := message.Info
		if info.ID == "" {
			continue
		}
		if role := strings.ToLower(strings.TrimSpace(info.Role)); role != "" && role != "assistant" {
			continue
		}
		observation := openCodeObservedMessage{
			SessionID: firstNonEmpty(info.SessionID, sessionID),
			MessageID: info.ID,
			EventAt:   openCodeFrameEventTime(info.Time.Created),
			Usage: openCodeMessageUsage{
				In:         openCodeJSONInt(info.Tokens.Input),
				Out:        openCodeJSONInt(info.Tokens.Output),
				Reasoning:  openCodeJSONInt(info.Tokens.Reasoning),
				CacheRead:  openCodeJSONInt(info.Tokens.Cache.Read),
				CacheWrite: openCodeJSONInt(info.Tokens.Cache.Write),
				CostMicros: openCodeJSONMicros(info.Cost),
			},
		}
		out = append(out, observation)
	}
	// An export with no assistant message is a real, recognised answer.
	return out, true
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
	openCodeDiscoveryMaxStats = 256
)

// openCodeSessionStoreChangedSince reports whether any session directory
// OpenCode might write changed after `since` — the only evidence this feature
// has of a run it never saw (the user's own shell or TUI).
//
// It reads NAMES AND MTIMES ONLY, never file contents. It walks the global
// roots openCodeSessionDirs("") already knows plus their immediate children,
// and every `<storage>/project/<slug>` entry's `storage/session` and
// `storage/session/info` — which is the project-scoped layout both native
// capture and the TUI write today. Slugs are ranked by the NEWER of those two
// directories' mtimes, not the slug directory's own, because a slug directory's
// mtime does not move when a session file inside it is rewritten.
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
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			note(filepath.Join(dir, entry.Name()))
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
	entries, err := os.ReadDir(filepath.Join(base, "project"))
	if err != nil {
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
		root := filepath.Join(base, "project", entry.Name(), "storage", "session")
		dirs := []string{root, filepath.Join(root, "info")}
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
	out := make([]string, 0, len(candidates)*2)
	for _, candidate := range candidates {
		out = append(out, candidate.dirs...)
	}
	return out
}

// openCodeReconcileBudgetForTests shortens a pass's budget and returns the
// restore function. Test seam: a hung child must not spend the shipped 20
// seconds in CI.
func openCodeReconcileBudgetForTests(d time.Duration) func() {
	prev := openCodeReconcileBudgetValue
	openCodeReconcileBudgetValue = d
	return func() { openCodeReconcileBudgetValue = prev }
}
