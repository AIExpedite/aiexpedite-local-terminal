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
	// offset is the end of the last complete line read.
	offset int64
	// midLine: the read stopped inside one over-long line, so the next read's
	// first segment is its tail and is dropped.
	midLine bool
	// newestMarkerMs is the newest turn completion found (epoch ms), kept until
	// a numeric reading covers it, so a deferred nudge is retried on every poll
	// even when Grok writes nothing more.
	newestMarkerMs int64
	// nudgedMarkerMs is the newest marker a nudge handed to the run debt. From
	// there the debt's own ladder owns it (any read it sends postdates the
	// turn), so the same turn never re-opens a budget the debt spent.
	nudgedMarkerMs int64
}

var (
	grokDirectMu    sync.Mutex
	grokDirectState grokDirectCursor
)

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
	grokDirectMu.Lock()
	grokDirectAdvanceLocked(grokBillingLogPath(grokPersistentHome()), now)
	markerMs, nudgedMs := grokDirectState.newestMarkerMs, grokDirectState.nudgedMarkerMs
	grokDirectMu.Unlock()
	if markerMs == 0 || markerMs <= nudgedMs {
		return
	}
	fingerprint := grokUsageCurrentFingerprint()
	if fingerprint == "" {
		return // kept: the first poll after login nudges it
	}
	if grokLiveReadingCovers(fingerprint, markerMs, now) {
		grokDirectMu.Lock()
		if grokDirectState.newestMarkerMs == markerMs {
			grokDirectState.newestMarkerMs = 0
		}
		grokDirectMu.Unlock()
		return
	}
	// A live armed run or the nudge cooldown refuses: the marker is kept and
	// re-evaluated on the next poll.
	if !nudgeGrokUsageRefreshFor(now, time.UnixMilli(markerMs), fingerprint) {
		return
	}
	grokDirectMu.Lock()
	if markerMs > grokDirectState.nudgedMarkerMs {
		grokDirectState.nudgedMarkerMs = markerMs
	}
	grokDirectMu.Unlock()
	fmt.Printf("%s[cli-usage] grok direct run: owed%s\n", colorCyan, colorReset)
}

// grokDirectAdvanceLocked reads what was appended to path since the cursor
// and records the newest marker in it. Caller holds grokDirectMu.
func grokDirectAdvanceLocked(path string, now time.Time) {
	c := &grokDirectState
	if path != c.path {
		// Another file (GROK_HOME moved): never seek into it at the old offset.
		*c = grokDirectCursor{path: path}
	}
	if path == "" {
		return
	}
	// One handle for the stat and the read, so both see the same file even if
	// Grok replaces it in between.
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return
	}
	size, modTime := info.Size(), info.ModTime()
	if c.seen && size == c.size && modTime.Equal(c.modTime) {
		return
	}
	start, jumped := c.offset, false
	if !c.seen || size < c.offset || size-c.offset > grokDirectScanMaxBytes {
		// First scan of this process, a rotation / truncation, or more appended
		// than one read may take: only the newest window.
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
		return
	}
	fragment := c.midLine
	if jumped {
		fragment = chunk[0] != '\n'
		chunk = chunk[1:]
	}
	c.seen, c.size, c.modTime = true, size, modTime
	lastNewline := bytes.LastIndexByte(chunk, '\n')
	if lastNewline < 0 {
		// No complete line yet. A partial line at a line boundary is read again
		// whole next time; the middle of an over-long line is skipped to its end.
		if fragment {
			c.offset, c.midLine = size, true
		} else {
			c.offset = start
		}
		return
	}
	lines := bytes.Split(chunk[:lastNewline], []byte("\n"))
	if fragment {
		// The first segment is the tail of a line that began before the window.
		lines = lines[1:]
	}
	c.offset, c.midLine = start+int64(lastNewline)+1, false
	if ms := grokNewestDirectMarkerMs(lines, now); ms > c.newestMarkerMs {
		c.newestMarkerMs = ms
	}
}

// grokNewestDirectMarkerMs returns the newest marker's completion (epoch ms,
// rounded up as a completion is), or 0. A ts further than
// grokBillingMaxClockSkew in the future is ignored; a nearer future one is
// clamped to now.
func grokNewestDirectMarkerMs(lines [][]byte, now time.Time) int64 {
	newest := int64(0)
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
		if ms := grokCompletionMs(ts); ms > newest {
			newest = ms
		}
	}
	return newest
}

// resetGrokDirectScan forgets the cursor (tests, propagator reset).
func resetGrokDirectScan() {
	grokDirectMu.Lock()
	grokDirectState = grokDirectCursor{}
	grokDirectMu.Unlock()
}
