// cliagent_usage_grok_direct.go — finds Grok turns the agent did not spawn,
// so their credit spend is read.
//
// Why this exists:
//
//	A `grok -p` (or the TUI) run from the user's own shell, an IDE, the
//	maintenance controller's shell, an agent-managed PowerShell session, or an
//	interactive PTY TUI in the persistent home (session.go arms only isolated
//	homes and managed `grok -p`) is never armed, so nothing owes the live
//	billing read that would refresh the card. Grok ≥ 1.0.40 headless runs
//	write no `billing: fetched credits config` record either, so the gather's
//	billing-record nudge never fires and grok_billing_live.json keeps its old
//	observedAt — the "billing_live stale after polling and retry" the Windows
//	maintenance controller reported.
//
//	Grok closes every turn with its own `shell.turn.inference_done` line in
//	the persistent home's logs/unified.jsonl. The propagator's fallback timer
//	(cliagent_usage_propagate.go) runs grokDirectRunScanIfChanged on every
//	poll: a turn newer than the account's newest NUMERIC live reading owes one
//	read, through the bounded run debt (nudgeGrokUsageRefreshFor), stamped
//	with the turn's own completion time.
//
// Bounds:
//   - one os.Stat per poll; a read only when the log's size or mtime moved;
//   - at most grokDirectScanMaxBytes of appended bytes per read, from the
//     in-memory offset (Stat-then-ReadFull, as readGrokBillingLogTailWithOffset);
//     a rotation, a truncation, a moved GROK_HOME or the first scan of a process
//     reads only the newest window;
//   - the owed rule is stateless — the newest marker against the live cache —
//     so the cursor needs no persistence: a restart's first scan re-finds an
//     unpaid turn, and the debt's own budget, spacing and cooling
//     (cliagent_usage_grok_freshness.go) bound the reads.
//
// The marker is vendor-internal: a Grok release that renames it makes this
// detector go quiet, leaving the billing-record nudge and the maintenance
// smoke's freshness gate as the drift alarm.
//
// Redaction: a line is decoded into {msg, ts} only — `ctx` is never decoded —
// and nothing from it is logged or persisted. The one log label is fixed.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"
)

// grokDirectScanMaxBytes bounds one read of appended log bytes.
const grokDirectScanMaxBytes = 256 * 1024

var (
	// grokDirectRunMarkers is the CLOSED set of `msg` values that close a
	// direct turn. Matching is exact. A var so a follow-up can widen it
	// without touching the logic.
	grokDirectRunMarkers = []string{"shell.turn.inference_done"}
	// grokDirectScanEnabled gates every trigger. TestMain turns it off, so no
	// test reads the developer's own Grok log.
	grokDirectScanEnabled = true
	// grokDirectDetected reports whether the last machine-info gather detected
	// Grok; it gates the poll only.
	grokDirectDetected = func() bool {
		info := GetMachineInfo()
		return info != nil && info.DetectedCliAgents[grokUsageProvider].Detected
	}
)

// grokDirectCursor is how far the scan has read the persistent home's log.
type grokDirectCursor struct {
	// path is the resolved log the cursor belongs to; any other path resets it.
	path    string
	seen    bool
	size    int64
	modTime time.Time
	// file identifies the log the offset belongs to (os.SameFile), so a log
	// rotated at the same path is read from its start even once it has grown
	// past the old offset.
	file os.FileInfo
	// offset is the end of the last complete line read.
	offset int64
	// midLine: the read stopped inside one over-long line, so the next read's
	// first segment is its tail and is dropped.
	midLine bool
	// pending is, per owning fingerprint (grokDirectAttributeLocked), the
	// newest turn completion found (epoch ms), kept until a numeric reading
	// for that account covers it, so a deferred nudge is retried on every poll
	// even when Grok writes nothing more. A turn is only ever paid under the
	// account that ran it, never against a login that switched in later. Key
	// "" holds turns no login has been seen for yet. At most
	// grokDirectPendingOwners entries.
	pending map[string]int64
	// nudged is, per fingerprint, the newest marker a nudge handed to the run
	// debt. From there the debt's own ladder owns it (any read it sends
	// postdates the turn), so the same turn never re-opens a budget the debt
	// spent — until the persisted forget count moves (another process
	// retired or replaced that debt unpaid).
	nudged map[string]grokDirectNudged
	// accounts is the sign-in timeline the polls observed, oldest first, at
	// most grokDirectAccountSpans long. It is the identity evidence for a
	// marker: the log line's ctx is never decoded.
	accounts []grokDirectAccountSpan
	// startMs is the process's first poll (0 before it). A turn older than it
	// completed while no poll was watching, so any login may have run it.
	startMs int64
	// preStartMs is the newest such turn. With no evidence of its owner it is
	// filed once under every login the polls see (preStartFiled, at most
	// grokDirectPendingOwners), so the account that ran it is paid whenever
	// it is signed in — an update hand-off's first login, or the account that
	// signed out while the agent was stopped once it is back.
	preStartMs    int64
	preStartFiled []string
}

// grokDirectNudged is a turn handed to the run debt, with the persisted
// grokUsageFreshness.DirectForgets read before the hand-off.
type grokDirectNudged struct {
	ms      int64
	forgets uint64
}

// grokDirectAccountSpan is one run of polls that saw the same login
// (fingerprint "" = signed out), from its first to its last observation.
type grokDirectAccountSpan struct {
	fingerprint     string
	firstMs, lastMs int64
}

// grokDirectAccountSpans bounds the sign-in timeline; a marker older than its
// oldest span is never attributed.
const grokDirectAccountSpans = 4

// grokDirectPendingOwners bounds the per-account pending markers: every
// account the timeline can name, plus turns not yet owned.
const grokDirectPendingOwners = grokDirectAccountSpans + 1

// grokDirectNoOwner marks a turn no account can be shown to own; it matches no
// fingerprint, so the turn is never paid.
const grokDirectNoOwner = "no-owner:"

// grokDirectPreStart marks a turn that completed before the process's first
// poll; it is never a pending key (grokDirectCursor.preStartMs holds it).
const grokDirectPreStart = "pre-start:"

var (
	grokDirectMu    sync.Mutex
	grokDirectState grokDirectCursor
	// grokDirectForgets counts grokDirectForgetNudged calls (under
	// grokDirectMu). A scan records a nudged marker only if no forget ran
	// since it read the marker: the worker it starts can retire the debt and
	// forget before the scan gets to record, and recording then would mark an
	// unpaid turn handled.
	grokDirectForgets uint64
)

// grokDirectNudge hands a turn to the run debt (a seam for tests).
var grokDirectNudge = nudgeGrokUsageRefreshFor

// grokDirectScanAtStartup runs one scan as the propagator starts: a turn that
// completed while the agent was down or mid-update is found at once rather
// than after the first poll. Detection is not required — the machine-info
// gather may not have run yet, and a missing log costs one failed Stat.
func grokDirectScanAtStartup() {
	grokDirectScan(false)
}

// grokDirectRunScanIfChanged is the propagator poll's scan.
func grokDirectRunScanIfChanged() {
	grokDirectScan(true)
}

// grokDirectScan advances the cursor whenever the process-level gates are
// open, then applies the owed rule to the newest marker it holds. The nudge's
// own gates (a signed-in account, no live armed run, the cooldown) never stop
// the cursor, so a marker found while signed out is nudged on the first poll
// after login, even when the log has not changed.
func grokDirectScan(requireDetected bool) {
	if !grokDirectScanEnabled || !grokUsageRefreshEnabled.Load() ||
		IsShutdownInProgress() || isDraining() || IsOffline() {
		return
	}
	if requireDetected && !grokDirectDetected() {
		return
	}
	now := grokUsageFreshnessNow()
	fingerprint := grokUsageCurrentFingerprint()
	grokDirectMu.Lock()
	markers := grokDirectAdvanceLocked(grokBillingLogPath(grokPersistentHome()), now)
	grokDirectObserveAccountLocked(fingerprint, now.UnixMilli())
	grokDirectAttributeLocked(markers)
	// Only this login's turns are nudged; the rest stay pending: a turn with
	// no owner known yet (signed out since the process started) is nudged on
	// the first poll after login, another account's once it signs back in.
	markerMs, handed := grokDirectState.pending[fingerprint], grokDirectState.nudged[fingerprint]
	forgets := grokDirectForgets
	grokDirectMu.Unlock()
	if fingerprint == "" || markerMs == 0 {
		return
	}
	// Read before any hand-off below, so a forget landing after it shows as a
	// moved count on the next poll rather than being missed.
	persistedForgets := grokDirectPersistedForgets()
	if markerMs <= handed.ms {
		if persistedForgets == handed.forgets {
			return
		}
		// A debt was retired or replaced unpaid since the hand-off — possibly
		// by another agent process, whose forget cleared only its own map —
		// so the turn may no longer be held by any debt: it is owed again.
		grokDirectMu.Lock()
		if grokDirectState.nudged[fingerprint] == handed {
			delete(grokDirectState.nudged, fingerprint)
		}
		grokDirectMu.Unlock()
	}
	if grokLiveReadingCovers(fingerprint, markerMs, now) {
		grokDirectMu.Lock()
		if grokDirectState.pending[fingerprint] == markerMs {
			delete(grokDirectState.pending, fingerprint)
		}
		grokDirectMu.Unlock()
		return
	}
	// A live armed run or the nudge cooldown refuses: the marker is kept and
	// re-evaluated on the next poll.
	if !grokDirectNudge(now, time.UnixMilli(markerMs), fingerprint) {
		return
	}
	grokDirectMu.Lock()
	// A forget since the read means a debt was retired unpaid, possibly the
	// one this nudge just started: the turn stays owed for the next poll.
	if forgets == grokDirectForgets && markerMs > grokDirectState.nudged[fingerprint].ms {
		if grokDirectState.nudged == nil {
			grokDirectState.nudged = map[string]grokDirectNudged{}
		}
		grokDirectState.nudged[fingerprint] = grokDirectNudged{ms: markerMs, forgets: persistedForgets}
	}
	grokDirectMu.Unlock()
	fmt.Printf("%s[cli-usage] grok direct run: owed%s\n", colorCyan, colorReset)
}

// grokDirectPersistedForgets is the freshness file's cross-process forget
// count (grokUsageFreshness.DirectForgets); a missing file reads 0. Read only
// while a marker is pending, i.e. not yet covered: one small file per poll.
func grokDirectPersistedForgets() uint64 {
	var state grokUsageFreshness
	if !readJSONFile(grokUsageFreshnessPath(), &state) {
		return 0
	}
	return state.DirectForgets
}

// grokDirectAdvanceLocked reads what was appended to path since the cursor
// and returns the turn completions (epoch ms) found in it. Caller holds
// grokDirectMu.
func grokDirectAdvanceLocked(path string, now time.Time) []int64 {
	c := &grokDirectState
	if path != c.path {
		// Another file (GROK_HOME moved): never seek into it at the old offset.
		*c = grokDirectCursor{path: path}
	}
	if path == "" {
		return nil
	}
	// One handle for the stat and the read, so both see the same file even if
	// Grok replaces it in between.
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	size, modTime := info.Size(), info.ModTime()
	replaced := c.seen && c.file != nil && !os.SameFile(c.file, info)
	if c.seen && !replaced && size == c.size && modTime.Equal(c.modTime) {
		return nil
	}
	start, jumped := c.offset, false
	if !c.seen || replaced || size < c.offset || size-c.offset > grokDirectScanMaxBytes {
		// First scan of this process, a rotation (another file at this path)
		// or truncation, or more appended than one read may take: only the
		// newest window.
		start = max(size-grokDirectScanMaxBytes, 0)
		jumped, c.midLine = start > 0, false
	}
	// A jumped window also reads the byte before it, to tell a window that
	// starts on a line boundary from one that starts inside a line.
	readFrom := start
	if jumped {
		readFrom--
	}
	chunk, ok := readFileRangeExact(f, readFrom, size)
	if !ok {
		// Truncated between Stat and read: the next poll retries.
		return nil
	}
	fragment := c.midLine
	if jumped {
		fragment = chunk[0] != '\n'
		chunk = chunk[1:]
	}
	c.seen, c.size, c.modTime, c.file = true, size, modTime, info
	lastNewline := bytes.LastIndexByte(chunk, '\n')
	if lastNewline < 0 {
		// No complete line yet. A partial line at a line boundary is read again
		// whole next time; the middle of an over-long line is skipped to its end.
		if fragment {
			c.offset, c.midLine = size, true
		} else {
			c.offset = start
		}
		return nil
	}
	lines := bytes.Split(chunk[:lastNewline], []byte("\n"))
	if fragment {
		// The first segment is the tail of a line that began before the window.
		lines = lines[1:]
	}
	c.offset, c.midLine = start+int64(lastNewline)+1, false
	return grokDirectMarkerTimesMs(lines, now)
}

// grokDirectObserveAccountLocked records the login a poll saw at nowMs.
// Caller holds grokDirectMu.
func grokDirectObserveAccountLocked(fingerprint string, nowMs int64) {
	c := &grokDirectState
	if n := len(c.accounts); n > 0 && c.accounts[n-1].fingerprint == fingerprint {
		c.accounts[n-1].lastMs = nowMs
		return
	}
	if c.startMs == 0 {
		c.startMs = nowMs
	}
	c.accounts = append(c.accounts, grokDirectAccountSpan{fingerprint: fingerprint, firstMs: nowMs, lastMs: nowMs})
	if len(c.accounts) > grokDirectAccountSpans {
		c.accounts = slices.Delete(c.accounts, 0, len(c.accounts)-grokDirectAccountSpans)
	}
}

// grokDirectMarkerOwnerLocked returns the account signed in when a turn
// completed at markerMs, from the polls' sign-in timeline:
//   - inside a signed-in span: that account;
//   - between two polls that saw different logins: the earlier signed-in one,
//     else the later one (grokDirectAttributeLocked also files the gap's
//     newest turn under the later login, since either may have run it);
//   - inside the process's first span while signed out: the next login owns
//     it ("" until one appears — the first poll after login nudges it);
//   - before the process's first poll: grokDirectPreStart, since no poll saw
//     who was signed in (grokDirectAttributeLocked files it under each login);
//   - inside a later signed-out span, or older than the timeline:
//     grokDirectNoOwner.
//
// gap is the index of the span a between-polls turn follows, else -1.
// Caller holds grokDirectMu.
func grokDirectMarkerOwnerLocked(markerMs int64) (owner string, gap int) {
	spans := grokDirectState.accounts
	k := -1
	for i := len(spans) - 1; i >= 0; i-- {
		if spans[i].firstMs <= markerMs {
			k = i
			break
		}
	}
	if k < 0 {
		if len(spans) > 0 && spans[0].firstMs == grokDirectState.startMs {
			return grokDirectPreStart, -1
		}
		return grokDirectNoOwner, -1
	}
	s := spans[k]
	if markerMs > s.lastMs && k+1 < len(spans) {
		if s.fingerprint != "" {
			return s.fingerprint, k
		}
		return spans[k+1].fingerprint, k
	}
	if s.fingerprint != "" {
		return s.fingerprint, -1
	}
	if s.firstMs != grokDirectState.startMs {
		return grokDirectNoOwner, -1
	}
	for _, next := range spans[k+1:] {
		if next.fingerprint != "" {
			return next.fingerprint, -1
		}
	}
	return "", -1
}

// grokDirectAttributeLocked files the markers one read found under their
// owners (grokDirectMarkerOwnerLocked), keeping each owner's newest, and
// re-resolves turns no login had been seen for. A turn between two polls that
// saw different logins cannot be placed on either side of the switch, so the
// newest such turn is filed under the later login too, never only under the
// earlier one: whichever account ran it is paid, at worst one extra bounded
// read per switch (none when that login's reading already covers it).
// Caller holds grokDirectMu.
func grokDirectAttributeLocked(markers []int64) {
	c := &grokDirectState
	type turn struct {
		ms    int64
		owner string
		gap   int
	}
	turns := make([]turn, 0, len(markers)+1)
	if unowned, ok := c.pending[""]; ok {
		if owner, _ := grokDirectMarkerOwnerLocked(unowned); owner != "" {
			delete(c.pending, "")
			turns = append(turns, turn{ms: unowned, owner: owner, gap: -1})
		}
	}
	gapNewest := map[int]int64{}
	for _, ms := range markers {
		owner, gap := grokDirectMarkerOwnerLocked(ms)
		if owner == grokDirectPreStart {
			c.preStartMs = max(c.preStartMs, ms)
			continue
		}
		turns = append(turns, turn{ms: ms, owner: owner, gap: gap})
		if gap >= 0 {
			gapNewest[gap] = max(gapNewest[gap], ms)
		}
	}
	file := func(owner string, ms int64) {
		if owner == grokDirectNoOwner || owner == grokDirectPreStart || ms <= c.pending[owner] {
			return
		}
		if c.pending == nil {
			c.pending = map[string]int64{}
		}
		c.pending[owner] = ms
	}
	for _, t := range turns {
		file(t.owner, t.ms)
		if t.gap >= 0 && t.ms == gapNewest[t.gap] {
			if later := c.accounts[t.gap+1].fingerprint; later != "" {
				file(later, t.ms)
			}
		}
	}
	if n := len(c.accounts); c.preStartMs > 0 && n > 0 {
		// The pre-start turn, once under each login seen: at most one read per
		// account (none where that account's reading already covers it).
		if login := c.accounts[n-1].fingerprint; login != "" && !slices.Contains(c.preStartFiled, login) {
			file(login, c.preStartMs)
			c.preStartFiled = append(c.preStartFiled, login)
			if len(c.preStartFiled) > grokDirectPendingOwners {
				c.preStartFiled = slices.Delete(c.preStartFiled, 0, len(c.preStartFiled)-grokDirectPendingOwners)
			}
		}
	}
	for len(c.pending) > grokDirectPendingOwners {
		oldest, oldestMs := "", int64(-1)
		for owner, ms := range c.pending {
			if oldestMs < 0 || ms < oldestMs {
				oldest, oldestMs = owner, ms
			}
		}
		delete(c.pending, oldest)
	}
	for owner := range c.nudged {
		if _, ok := c.pending[owner]; !ok {
			delete(c.nudged, owner)
		}
	}
}

// grokDirectMarkerTimesMs returns each marker's completion (epoch ms, rounded
// up as a completion is), in log order. A ts further than
// grokBillingMaxClockSkew in the future is ignored; a nearer future one is
// clamped to now.
func grokDirectMarkerTimesMs(lines [][]byte, now time.Time) []int64 {
	var out []int64
	for _, line := range lines {
		if !grokLineMentionsAnyMessage(line, grokDirectRunMarkers) {
			continue
		}
		var rec struct {
			Msg string `json:"msg"`
			TS  string `json:"ts"`
		}
		if json.Unmarshal(line, &rec) != nil || !slices.Contains(grokDirectRunMarkers, rec.Msg) {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, rec.TS)
		if err != nil || ts.After(now.Add(grokBillingMaxClockSkew)) {
			continue
		}
		if ts.After(now) {
			ts = now
		}
		out = append(out, grokCompletionMs(ts))
	}
	return out
}

// grokDirectForgetNudged is told when a debt is retired because the login
// moved off its account, or replaced by another account's debt (grokOweRead):
// no reading landed for a turn handed to it, so that turn is no longer handled
// and is nudged again once its account is back.
func grokDirectForgetNudged() {
	grokDirectMu.Lock()
	grokDirectState.nudged = nil
	grokDirectForgets++
	grokDirectMu.Unlock()
}

// resetGrokDirectScan forgets the cursor (tests, propagator reset).
func resetGrokDirectScan() {
	grokDirectMu.Lock()
	grokDirectState = grokDirectCursor{}
	grokDirectMu.Unlock()
}
