// cliagent_usage_opencode_direct.go — the direct-run reader: OpenCode spend
// from runs this agent did not spawn.
//
// Stream capture (cliagent_usage_opencode_capture.go) sees only the turns the
// agent spawns as a `--format json` stream. A run from the user's own shell or
// IDE, or the OpenCode TUI in a terminal-managed PTY, streams to nobody — but
// OpenCode persists every assistant message, with its tokens and cost, in its
// own store. This file reads that store (read-only) and folds today's spend into
// the same ledger.
//
//   - Source: openCodeStoreLayout() picks an adapter — the SQLite database
//     (cliagent_usage_opencode_direct_sqlite.go) or the older JSON files
//     (cliagent_usage_opencode_direct_json.go). Each checks the exact shape it
//     needs; an unrecognised store FAILS CLOSED: nothing is merged, coverage is
//     not renewed, and the card keeps "Tokens today (agent runs)".
//   - Dedup: a message is keyed sha256(session, "message:"+id) — the key the
//     export fallback writes — so the two cannot count one message twice. A
//     stream step is keyed by its part id instead, so the reader skips every
//     message inside a managed run's ownership window (openCodeOwnedRun). A
//     managed run that has not named its session yet HOLDS the reader: no
//     message created after its floor is counted, and the cursor does not pass
//     that floor, until its window is on disk. The hold comes from the ledger's
//     armed debts and from this process's armed runs (a refused debt write
//     still holds); one older than the debt age-out no longer does.
//   - Bounds: one scan reads records last written since
//     max(local midnight, cursor − 10 min), capped in sessions, records, bytes
//     per record and wall-clock time. A capped scan saves a partial cursor at the
//     first record it did not read, and the next scan resumes there without the
//     overlap, so a backlog drains: a heavy user is under-counted, never
//     over-counted. A message with no completed time is not counted yet, and the
//     cursor waits for it. Sessions are listed from the cursor even across
//     midnight, so a turn begun before midnight is found once it finishes.
//   - Triggers: once at startup (runs made while the agent was down or
//     mid-update), on the propagator's 60 s tick only while the store's change
//     marker moved or the last scan left work, and on a Refresh click. One
//     single flight is shared by all three; ParseContext never scans.
//   - Survive: the cursor, coverage and ownership windows live in the ledger, so
//     they cross the self-update hand-off. A ledger an older build rewrote has
//     today's buckets but no cursor; the scan then starts at the newest of them
//     rather than local midnight — an under-count over a double count.
//
// A committed change advances the ledger generation, and the propagator sends
// at most one bounded hint for it (noteOpenCodeUsageAdvanced).
//
// SECRETS: adapters decode only a message's id, session id, role, timestamps,
// tokens and cost, and a part's type, tokens and cost, into narrow structs —
// never message text, tool output, paths or credentials. The ledger gains
// counts, hashed keys and hashed session ids only. Logs are fixed labels.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Vars so tests can pin them.
var (
	openCodeDirectNow                  = time.Now
	openCodeDirectMaxSessions          = 256
	openCodeDirectMaxRecords           = 8192
	openCodeDirectMaxRecordBytes int64 = 1 << 20
	openCodeDirectTickBudget           = 750 * time.Millisecond
	// openCodeDirectSettleWindow: a store changed this recently may still be
	// changing within its filesystem's timestamp granularity (a coarse kernel
	// tick on Linux), so the marker cannot be trusted to move again — the next
	// tick scans regardless.
	openCodeDirectSettleWindow = 2 * time.Second
	openCodeDirectClickBudget  = 2 * time.Second
	// openCodeDirectOverlap is how far before the cursor a scan re-reads, for
	// records whose last-write time lags their neighbours'; seenSteps absorbs
	// the repeats.
	openCodeDirectOverlap = 10 * time.Minute
	// openCodeDirectScanEnabled gates every trigger. TestMain turns it off, so
	// no test reads the developer's own store.
	openCodeDirectScanEnabled = true
	// openCodeDirectDetected reports whether the last machine-info gather
	// detected OpenCode; it gates the tick only.
	openCodeDirectDetected = func() bool {
		info := GetMachineInfo()
		return info != nil && info.DetectedCliAgents[openCodeUsageProvider].Detected
	}
	openCodeDirectReaders = map[string]openCodeDirectReader{
		openCodeStoreLayoutSQLite: readOpenCodeDirectSQLite,
		openCodeStoreLayoutJSON:   readOpenCodeDirectJSON,
	}
)

// errOpenCodeDirectLayoutUnknown: the store is not a shape an adapter knows.
var errOpenCodeDirectLayoutUnknown = errors.New("opencode store layout unknown")

// openCodeDirectLimits bounds one adapter read; the time budget is the read's
// context deadline.
type openCodeDirectLimits struct {
	MaxSessions    int
	MaxRecords     int
	MaxRecordBytes int64
	// SessionFloorMs, when set below the read's floor, lists sessions last
	// written since then (openCodeDirectSessionFloor); 0 lists from the floor.
	SessionFloorMs int64
	// ExactSessions resumes a scan the session cap stopped: the listing starts
	// at the first unread session (SessionFloorMs), without the slack, or it
	// would re-list the same capped sessions forever; their records are still
	// read from the capped scan's own floor.
	ExactSessions bool
	// Skip is a continuation's tie-breaker: the entries the capped scan read at
	// its cut time — sessions when ExactSessions, records otherwise.
	Skip openCodeDirectSkip
}

// skipFor is the tie-breaker for the session listing (sessions) or the records.
func (l openCodeDirectLimits) skipFor(sessions bool) openCodeDirectSkip {
	if sessions != l.ExactSessions {
		return openCodeDirectSkip{}
	}
	return l.Skip
}

// openCodeDirectSkip drops the first N entries last written at AtMs, in the
// adapter's (time, id) order: a capped scan read them already. Without it, more
// entries sharing one millisecond than the cap would refill the cap on every
// resume and the rest would never be read. An entry rewritten since moves
// later, so a count can then drop one unread entry — an under-count, never a
// double count.
type openCodeDirectSkip struct {
	AtMs  int64
	N     int
	taken int
}

// take reports whether the entry last written at ms is one to drop.
func (s *openCodeDirectSkip) take(ms int64) bool {
	if ms != s.AtMs || s.taken >= s.N {
		return false
	}
	s.taken++
	return true
}

// openCodeDirectTies counts the entries already seen at one last-write time,
// in read order, for the tie-breaker a cut saves.
type openCodeDirectTies struct {
	at int64
	n  int
}

// see records an entry last written at ms and returns how many came before it
// at that time.
func (t *openCodeDirectTies) see(ms int64) int {
	if ms != t.at || t.n == 0 {
		t.at, t.n = ms, 0
	}
	t.n++
	return t.n - 1
}

// through is how many entries were seen at ms.
func (t openCodeDirectTies) through(ms int64) int {
	if ms != t.at {
		return 0
	}
	return t.n
}

// sessionsSinceMs is the oldest session last write a read from floorMs lists,
// less the slack: a session's last write can precede its last message's.
func (l openCodeDirectLimits) sessionsSinceMs(floorMs int64) int64 {
	if l.ExactSessions {
		if l.SessionFloorMs > 0 {
			return l.SessionFloorMs
		}
		return floorMs
	}
	if l.SessionFloorMs > 0 {
		floorMs = min(floorMs, l.SessionFloorMs)
	}
	return floorMs - openCodeDirectSessionSlack.Milliseconds()
}

// openCodeDirectMessage is one stored assistant message.
type openCodeDirectMessage struct {
	ID          string
	SessionID   string
	CreatedMs   int64
	CompletedMs int64 // 0 while the turn is still running
	WrittenMs   int64 // the record's last write: what the cursor compares
	Usage       openCodeUsageStep
	Valid       bool // Usage was readable
}

// openCodeDirectRead is an adapter's answer, in ascending last-write order.
type openCodeDirectRead struct {
	Messages []openCodeDirectMessage
	// Truncated: a cap or the time budget stopped the read; every record last
	// written before ThroughMs was read, and the first ThroughSkip at it.
	Truncated   bool
	ThroughMs   int64
	ThroughSkip int
	// AtSession: the earliest cut was the session listing's, so every session
	// last written before ThroughMs was listed whole and ThroughSkip counts
	// sessions. A later record cut in the same read does not change how the
	// next scan resumes; the records it left in listed sessions are an
	// under-count, never a double count.
	AtSession bool
	// Skipped counts oversized or unparseable records passed over for good.
	Skipped int
}

// openCodeDirectReader reads the assistant messages last written at or after
// floorMs. errOpenCodeDirectLayoutUnknown fails the scan closed; any other error
// is a store that could not be read this time.
type openCodeDirectReader func(ctx context.Context, root string, floorMs int64, limits openCodeDirectLimits) (openCodeDirectRead, error)

// truncateAt marks the read stopped before a record last written at ms, after
// reading read records at that time.
func (r *openCodeDirectRead) truncateAt(ms int64, read int) {
	r.truncate(ms, read, true)
}

// truncateSessionsAt marks the read stopped before a session last written at
// ms, after listing read sessions at that time.
func (r *openCodeDirectRead) truncateSessionsAt(ms int64, read int) {
	r.truncate(ms, read, false)
}

// truncate keeps the earliest cut, and the next scan resumes the way that cut
// stopped: by session (AtSession) or by record, with ThroughSkip counting
// entries of that kind. At one millisecond a session cut wins over a record
// cut, since the sessions it left unlisted are read whole from the scan's floor.
func (r *openCodeDirectRead) truncate(ms int64, read int, records bool) {
	sameKind := r.AtSession == !records
	if !r.Truncated || ms < r.ThroughMs ||
		(ms == r.ThroughMs && (sameKind && read < r.ThroughSkip || !sameKind && !records)) {
		r.ThroughMs, r.ThroughSkip, r.AtSession = ms, read, !records
	}
	r.Truncated = true
}

var (
	openCodeDirectGroup singleflight.Group
	// openCodeDirectGate is the tick's memory: the store marker the last scan
	// started from, and whether that scan left work a later one must finish
	// (a capped read, an incomplete message, a hold, a refused write).
	openCodeDirectGateMu     sync.Mutex
	openCodeDirectLastMarker string
	openCodeDirectAgain      bool
)

/* --------------------------------------------------------------------------
   Triggers
   -------------------------------------------------------------------------- */

// openCodeDirectScanAtStartup runs one scan when the agent starts, after the
// ledger's startup recovery: runs made while it was down or mid-update.
func openCodeDirectScanAtStartup() {
	if !openCodeDirectScanEnabled || IsShutdownInProgress() {
		return
	}
	openCodeDirectScanShared(openCodeDirectTickBudget)
}

// openCodeDirectScanIfChanged is the propagator tick's scan: only while
// OpenCode is detected, the agent is online and not draining or shutting down,
// and the store changed since the last scan (or that scan left work). An idle
// computer pays a few stats or one directory listing.
func openCodeDirectScanIfChanged() {
	if !openCodeDirectScanEnabled || IsShutdownInProgress() || isDraining() || IsOffline() || !openCodeDirectDetected() {
		return
	}
	layout, root := openCodeStoreLayout()
	if layout == openCodeStoreLayoutUnknown {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), openCodeDirectTickBudget)
	marker, _, ok := openCodeStoreMarker(ctx, layout, root)
	cancel()
	if !ok {
		// A session root too large to list inside the tick's budget: the
		// reader's own listing would stop at the same deadline, so the tick
		// leaves it to the Refresh click's larger budget.
		return
	}
	openCodeDirectGateMu.Lock()
	unchanged := marker == openCodeDirectLastMarker && !openCodeDirectAgain
	openCodeDirectGateMu.Unlock()
	if unchanged {
		return
	}
	openCodeDirectScanShared(openCodeDirectTickBudget)
}

// openCodeDirectScanOnRefresh is the Refresh click's scan, inside the live
// probe's budget with the reader's own cap. It reports a live-probe outcome.
func openCodeDirectScanOnRefresh(ctx context.Context) string {
	if !openCodeDirectScanEnabled {
		return liveProbeOutcomeNotMerged
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < openCodeDirectClickBudget {
		return liveProbeOutcomeNotMerged
	}
	switch openCodeDirectScanShared(openCodeDirectClickBudget) {
	case "direct_scanned", "direct_unchanged":
	default:
		return liveProbeOutcomeNotMerged
	}
	return liveProbeOutcomeOK
}

// openCodeDirectScanShared runs one scan, or shares the one in flight, and
// records what the tick gate needs. It returns the scan's label.
func openCodeDirectScanShared(budget time.Duration) string {
	v, _, _ := openCodeDirectGroup.Do("scan", func() (any, error) {
		layout, root := openCodeStoreLayout()
		// One budget covers the marker and the read.
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		// The marker is taken BEFORE the read, so a write that lands during it
		// moves the marker again for the next tick.
		marker, changedAt, ok := openCodeStoreMarker(ctx, layout, root)
		label, again := "direct_truncated", true
		if ok {
			label, again = openCodeDirectScan(ctx, layout, root)
		}
		if time.Since(changedAt) < openCodeDirectSettleWindow {
			again = true
		}
		openCodeDirectGateMu.Lock()
		openCodeDirectLastMarker, openCodeDirectAgain = marker, again
		openCodeDirectGateMu.Unlock()
		logOpenCodeUsageCapture(label)
		return label, nil
	})
	label, _ := v.(string)
	return label
}

// openCodeStoreMarker is a cheap fingerprint of the store's last change, and
// when that change was. SQLite: the size and mtime of the database and its
// write-ahead log. JSON: the newest mtime among the session directories under
// storage/message, from one listing — the top directory's own mtime moves only
// when a NEW session directory is made, not when a message lands in an
// existing one. That listing is batched under ctx's deadline, so a large
// session root cannot stall the tick or a Refresh; ok is false when the
// deadline stopped it and the marker is incomplete.
func openCodeStoreMarker(ctx context.Context, layout, root string) (marker string, changedAt time.Time, ok bool) {
	switch layout {
	case openCodeStoreLayoutSQLite:
		marker = layout
		for _, name := range []string{"opencode.db", "opencode.db-wal"} {
			info, err := os.Stat(filepath.Join(root, name))
			if err != nil {
				marker += "|-"
				continue
			}
			marker += fmt.Sprintf("|%d:%d", info.ModTime().UnixNano(), info.Size())
			if info.ModTime().After(changedAt) {
				changedAt = info.ModTime()
			}
		}
		return marker, changedAt, true
	case openCodeStoreLayoutJSON:
		messageRoot := filepath.Join(root, "storage", "message")
		if _, err := os.Stat(messageRoot); err != nil {
			return layout + "|-", changedAt, true
		}
		dirs := 0
		ok = eachOpenCodeDirectEntry(ctx, messageRoot, 0, func(e os.DirEntry) {
			if !e.IsDir() {
				return
			}
			dirs++
			if info, err := e.Info(); err == nil && info.ModTime().After(changedAt) {
				changedAt = info.ModTime()
			}
		})
		return fmt.Sprintf("%s|%d|%d", layout, dirs, changedAt.UnixNano()), changedAt, ok
	}
	return layout, changedAt, true
}

/* --------------------------------------------------------------------------
   Scan
   -------------------------------------------------------------------------- */

// openCodeDirectScan reads the store once, inside ctx's budget, and commits
// what it found. label is the fixed log label; again reports work a later scan
// must finish.
func openCodeDirectScan(ctx context.Context, layout, root string) (label string, again bool) {
	read := openCodeDirectReaders[layout]
	if layout == openCodeStoreLayoutUnknown || read == nil {
		return "direct_layout_unknown", false
	}
	now := openCodeDirectNow()
	saved := loadOpenCodeUsageLedger()
	floorMs := openCodeDirectScanFloor(saved, now)
	limits := openCodeDirectLimits{
		MaxSessions:    openCodeDirectMaxSessions,
		MaxRecords:     openCodeDirectMaxRecords,
		MaxRecordBytes: openCodeDirectMaxRecordBytes,
		SessionFloorMs: openCodeDirectSessionFloor(saved, now),
	}
	if c := saved.DirectCursor; c != nil && c.Continue {
		limits.ExactSessions = c.AtSession
		limits.Skip = openCodeDirectSkip{AtMs: c.ThroughMs, N: c.Skip}
		if c.AtSession {
			limits.SessionFloorMs = c.ThroughMs
		}
	}
	result, err := read(ctx, root, floorMs, limits)
	switch {
	case errors.Is(err, errOpenCodeDirectLayoutUnknown):
		return "direct_layout_unknown", false
	case err != nil:
		return "direct_read_failed", true
	}

	memHold := openCodeUsageArmedHoldMs(now)
	fingerprint := openCodeKnownAccountFingerprint(resolveOpenCodeExecutable())
	today, _ := openCodeLocalDay(now)
	held, pending, unchanged := false, false, false
	committed, changed, generation := openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		steps, throughMs, h, p := openCodeDirectSteps(ledger, result, minNonZero(memHold, openCodeLedgerHoldMs(ledger, now)), now)
		held, pending = h, p
		changed := mergeOpenCodeUsageSteps(ledger, fingerprint, steps)
		if !changed && !result.Truncated && openCodeDirectWriteCanWait(ledger, layout, today, throughMs) {
			// Nothing new, coverage already today and the cursor within the
			// overlap: the next scan re-reads the same few records, so the
			// whole-ledger rewrite is skipped. A capped scan always saves its
			// cursor, or it would stop at the same record forever.
			unchanged = true
			return false, false
		}
		pruneOpenCodeOwnedRuns(ledger, now.UnixMilli())
		cursor := &openCodeDirectCursor{Layout: layout, ThroughMs: throughMs, Continue: result.Truncated}
		if result.Truncated && throughMs == result.ThroughMs {
			cursor.AtSession, cursor.Skip = result.AtSession, result.ThroughSkip
			if result.AtSession {
				cursor.RecordFloorMs = floorMs
			}
		}
		cursor.RewindFloorMs = openCodeDirectRewindFloor(ledger.DirectCursor, result.Truncated, throughMs)
		ledger.DirectCursor = cursor
		ledger.DirectCoverage = &openCodeDirectCoverage{Layout: layout, ObservedAtMs: now.UnixMilli(), LastOkLocalDate: today}
		return true, changed
	})
	noteOpenCodeUsageAdvanced(committed, changed, generation)
	switch {
	case !committed && !unchanged:
		// Nothing moved: the cursor and coverage stay, and the next tick retries.
		return "direct_lock_busy", true
	case result.Truncated || result.Skipped > 0:
		return "direct_truncated", result.Truncated || held || pending
	case unchanged:
		return "direct_unchanged", held || pending
	}
	return "direct_scanned", held || pending
}

// openCodeDirectWriteCanWait reports that a scan which found nothing new need
// not rewrite the ledger: today's coverage is already recorded and the saved
// cursor trails throughMs by less than the overlap every scan re-reads anyway.
func openCodeDirectWriteCanWait(ledger *openCodeUsageLedger, layout, today string, throughMs int64) bool {
	c, cov := ledger.DirectCursor, ledger.DirectCoverage
	return c != nil && cov != nil && !c.Continue && c.Layout == layout && cov.Layout == layout && cov.LastOkLocalDate == today &&
		throughMs >= c.ThroughMs && throughMs-c.ThroughMs < openCodeDirectOverlap.Milliseconds()
}

// openCodeDirectRewindFloor is the new cursor's RewindFloorMs. A scan that
// finishes a continuation has drained a backlog larger than one scan's cap —
// the number of keys seenSteps holds — so the keys of the records it read may
// have rolled over: the overlap must not re-read them, and the floor is this
// scan's end. Otherwise the previous floor stays while the overlap can reach it.
func openCodeDirectRewindFloor(prev *openCodeDirectCursor, truncated bool, throughMs int64) int64 {
	switch {
	case prev == nil:
		return 0
	case prev.Continue && !truncated:
		return throughMs
	case prev.RewindFloorMs > throughMs-openCodeDirectOverlap.Milliseconds():
		return prev.RewindFloorMs
	}
	return 0
}

// openCodeDirectSteps turns a read into ledger steps against ledger's ownership
// windows, and the cursor the scan may save. holdMs (0 for none) is the floor of
// the oldest managed run with no window yet.
func openCodeDirectSteps(ledger *openCodeUsageLedger, read openCodeDirectRead, holdMs int64, now time.Time) (steps []openCodeUsageStep, throughMs int64, held, pending bool) {
	throughMs = now.UnixMilli()
	if read.Truncated {
		throughMs = read.ThroughMs
	}
	staleMs := now.Add(-openCodeUsageDebtMaxAge).UnixMilli()
	for _, m := range read.Messages {
		if m.CompletedMs == 0 {
			// Still running: counted once complete, and the cursor waits for it
			// — unless it is older than the debt age-out (a crashed TUI), which
			// would otherwise pin the cursor for the rest of the day.
			if m.CreatedMs >= staleMs {
				throughMs = min(throughMs, m.CreatedMs)
				pending = true
			}
			continue
		}
		if !m.Valid {
			continue
		}
		if holdMs > 0 && m.CreatedMs >= holdMs {
			held = true
			continue
		}
		sessionKey := openCodeUsageSessionKey(m.SessionID)
		if m.CreatedMs <= ledger.OwnedEvictedMs || openCodeOwnedByRun(ledger, sessionKey, m.CreatedMs) {
			continue
		}
		step := m.Usage
		step.AtMs = m.CompletedMs
		step.Key = openCodeUsageStepKey(m.SessionID, "message:"+m.ID)
		steps = append(steps, step)
	}
	if holdMs > 0 {
		throughMs = min(throughMs, holdMs)
	}
	return steps, throughMs, held, pending
}

// openCodeOwnedByRun reports whether a managed run's window covers the message.
func openCodeOwnedByRun(ledger *openCodeUsageLedger, sessionKey string, createdMs int64) bool {
	for _, w := range ledger.OwnedRuns {
		if w.covers(sessionKey, createdMs) {
			return true
		}
	}
	return false
}

// resumeMs is where the scan after c reads from, before the midnight clamp: the
// cursor less the overlap, but not before the end of a drained backlog; the
// cursor itself after a capped scan; or, after one the session cap stopped, the
// floor that scan read records from.
func (c openCodeDirectCursor) resumeMs() int64 {
	if c.Continue {
		if c.AtSession && c.RecordFloorMs > 0 {
			return c.RecordFloorMs
		}
		return c.ThroughMs
	}
	return max(c.ThroughMs-openCodeDirectOverlap.Milliseconds(), min(c.RewindFloorMs, c.ThroughMs))
}

// openCodeDirectScanFloor is where a scan starts: the cursor's resume point,
// never before local midnight. With no cursor but a bucket for today — an older
// build rewrote the ledger and dropped the cursor, or this is the first scan
// after the update that added it — it starts at that bucket's newest
// observation instead: an under-count over a double count.
func openCodeDirectScanFloor(ledger openCodeUsageLedger, now time.Time) int64 {
	floor := openCodeLocalMidnight(now).UnixMilli()
	if c := ledger.DirectCursor; c != nil && c.ThroughMs > 0 {
		return max(floor, c.resumeMs())
	}
	today, _ := openCodeLocalDay(now)
	for _, b := range ledger.Buckets {
		if b.LocalDate == today && b.ObservedAtMs > floor {
			floor = b.ObservedAtMs
		}
	}
	return floor
}

// openCodeDirectSessionFloor is how far back a scan lists sessions. A message
// can finish long after its session was last touched — in the JSON store a
// record rewritten in place moves no directory mtime — so the listing is not
// clamped to midnight: it reaches back to the cursor's resume point (where a
// still-running turn holds it), or with no cursor to the debt age-out before
// midnight, the oldest running turn the cursor would wait for.
func openCodeDirectSessionFloor(ledger openCodeUsageLedger, now time.Time) int64 {
	floor := openCodeLocalMidnight(now).Add(-openCodeUsageDebtMaxAge).UnixMilli()
	if c := ledger.DirectCursor; c != nil && c.ThroughMs > 0 {
		floor = max(floor, c.resumeMs())
	}
	return floor
}

// openCodeLedgerHoldMs is the floor of the oldest ARMED debt that has not named
// its session — a managed run, in this process or another, whose messages the
// reader cannot yet tell from direct use — or 0. A debt older than the debt
// age-out no longer holds.
func openCodeLedgerHoldMs(ledger *openCodeUsageLedger, now time.Time) int64 {
	cutoff := now.Add(-openCodeUsageDebtMaxAge).UnixMilli()
	hold := int64(0)
	for _, d := range ledger.Debts {
		if !d.owed() && d.SessionID == "" && d.RunFloorMs >= cutoff {
			hold = minNonZero(hold, d.RunFloorMs)
		}
	}
	return hold
}

// minNonZero is the smaller of a and b, where 0 means "none".
func minNonZero(a, b int64) int64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	}
	return min(a, b)
}

// resetOpenCodeDirectScan forgets the tick gate (tests).
func resetOpenCodeDirectScan() {
	openCodeDirectGateMu.Lock()
	openCodeDirectLastMarker, openCodeDirectAgain = "", false
	openCodeDirectGateMu.Unlock()
}
