// File: antigravity_turn_monitor.go
// -----------------------------------------------------------------------------
// Watches one one-shot Antigravity turn while its `agy` process runs:
//
//   - STDOUT TEE. Every stdout byte still lands in the bounded buffer the turn
//     parses after exit; each complete line also feeds the monitor (last
//     activity, whether `init` was seen, interaction signals).
//   - OWN LOG. Once a second the monitor follows the run's own PID block in
//     agy's per-run log (antigravityPIDBlockFile): new bytes are activity, a
//     quota failure line stops the turn (it used to be a 15 s poll of the
//     shared cli.log, which a neighbour run could trip and which a 1.5.10 run
//     — dead in ~4 s — never reached), and a pending-permission line is an
//     interaction signal.
//   - STALL. If nothing at all follows `init` for antigravityStallBound (no
//     stdout line and no new log bytes for this run), the turn stops as
//     `stalled`. Once anything follows init the rule is disarmed and the
//     turn's own timeout governs it.
//   - INTERACTION. ask_question fires at once; ask_permission /
//     ask_custom_permission fire only if neither a DONE for that step nor a
//     later step arrives within antigravityPermissionSettle — that window is
//     where the existing auto-approve wins. A progress step for any other tool
//     (an edit, a shell command) is never a signal.
//
// antigravityInteractionStopMode is "observe" until captured agy 1.5.10
// output confirms the ask_* step shapes: a match only writes an agent.log line
// and the turn continues. A wrong guess in "enforce" would kill ordinary
// steps. The stall rule and the exit classifier still bound a hung turn.
//
// The monitor fires its stop exactly once (atomic), and records the cause and
// the moment the signal was first seen, so the turn can publish within
// antigravityStopPublishBound of it whatever the process tree does.
// -----------------------------------------------------------------------------

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	antigravityInteractionObserve = "observe"
	antigravityInteractionEnforce = "enforce"
	// Bytes of one stdout line the monitor parses; a longer line (a long init
	// tool list) still counts as activity and still lands in the buffer whole.
	antigravityMonitorMaxLine = 256 * 1024
	// Bytes of new log read per tick.
	antigravityMonitorLogChunk = 256 * 1024
	// Ticks spent looking for the run's own log block before giving up. agy
	// writes it at startup, so a block not found by then is not coming (a
	// build that logs elsewhere); each search lists the log directory and may
	// read up to antigravityWatchedNewest logs, so it is not repeated forever.
	antigravityMonitorLogSearchMax = 30
)

// Package vars so tests can shorten them.
var (
	antigravityInteractionStopMode = antigravityInteractionObserve
	antigravityStallBound          = 3 * time.Minute
	antigravityPermissionSettle    = 1 * time.Second
	antigravityMonitorTick         = 1 * time.Second
	// From a stop signal's first sighting to the publish.
	antigravityStopPublishBound = 5 * time.Second
	// From the interrupt to the force-kill of the stopped turn's tree.
	antigravityStopKillGrace = 2 * time.Second
	// antigravityRunLogBase is where agy writes its logs (a test seam).
	antigravityRunLogBase = antigravityHomeBase
)

// A run's own log line reporting a pending permission or question. The format
// is unverified on agy 1.5.10 (no AIE2 run reached a tool call), so it is
// deliberately narrow: an interactive tool id next to a waiting word.
var antigravityPendingInteractionLogLine = regexp.MustCompile(`(?i)\b(ask_question|ask_permission|ask_custom_permission)\b.*\b(pending|waiting|awaiting)\b|\b(pending|waiting|awaiting)\b.*\b(ask_question|ask_permission|ask_custom_permission)\b`)

type antigravityTurnMonitor struct {
	sessionID string
	spawnedAt time.Time
	// The bounds and log base this turn runs under, read once at its start
	// (the package vars are test seams; the monitor's goroutines never read
	// them again).
	mode       string
	stallBound time.Duration
	permSettle time.Duration
	tickEvery  time.Duration
	logBase    string
	// onStop interrupts the turn and force-kills its tree; onQuota records a
	// quota reason from the log. Both are called at most once.
	onStop  func()
	onQuota func(reason string)

	mu       sync.Mutex
	buf      bytes.Buffer
	limit    int
	trunc    bool
	partial  []byte
	initAt   time.Time
	afterIni bool // anything followed init
	// pending permission requests by step index → first sighting.
	pendingPerm map[int]time.Time
	observed    map[string]bool

	pid         int
	logPath     string
	logOffset   int64
	logForeign  bool // a later run's block started in the same file
	logSearches int  // ticks spent looking for the run's block
	quotaSeen   bool

	stopped    atomic.Bool
	stopCh     chan struct{}
	stopCause  string
	stopDetail string
	signalAt   time.Time
	doneCh     chan struct{}
}

func newAntigravityTurnMonitor(sessionID string, spawnedAt time.Time, limit int, onStop func(), onQuota func(string)) *antigravityTurnMonitor {
	return &antigravityTurnMonitor{
		sessionID:   sessionID,
		spawnedAt:   spawnedAt,
		mode:        antigravityInteractionStopMode,
		stallBound:  antigravityStallBound,
		permSettle:  antigravityPermissionSettle,
		tickEvery:   antigravityMonitorTick,
		logBase:     antigravityRunLogBase(),
		onStop:      onStop,
		onQuota:     onQuota,
		limit:       limit,
		pendingPerm: map[int]time.Time{},
		observed:    map[string]bool{},
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
	}
}

// consumeStdout drains r into the bounded buffer and feeds each line to the
// monitor. Never returns early: a full buffer still drains r so the child
// cannot block on a full pipe.
func (mon *antigravityTurnMonitor) consumeStdout(r io.Reader) {
	chunk := make([]byte, 32*1024)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			mon.write(chunk[:n])
		}
		if err != nil {
			break
		}
	}
	mon.mu.Lock()
	rest := mon.partial
	mon.partial = nil
	mon.mu.Unlock()
	if len(bytes.TrimSpace(rest)) > 0 {
		mon.onLine(string(rest))
	}
}

func (mon *antigravityTurnMonitor) write(p []byte) {
	mon.mu.Lock()
	if !mon.trunc {
		room := mon.limit - mon.buf.Len()
		if len(p) > room {
			mon.buf.Write(p[:max(room, 0)])
			mon.trunc = true
		} else {
			mon.buf.Write(p)
		}
	}
	var lines []string
	data := append(mon.partial, p...)
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		lines = append(lines, string(data[:i]))
		data = data[i+1:]
	}
	if len(data) > antigravityMonitorMaxLine {
		data = data[:antigravityMonitorMaxLine]
	}
	mon.partial = append([]byte(nil), data...)
	mon.mu.Unlock()
	for _, l := range lines {
		mon.onLine(l)
	}
}

// snapshot returns the stdout captured so far and whether it was truncated.
func (mon *antigravityTurnMonitor) snapshot() (string, bool) {
	mon.mu.Lock()
	defer mon.mu.Unlock()
	return mon.buf.String(), mon.trunc
}

func (mon *antigravityTurnMonitor) onLine(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	now := time.Now()
	var event antigravityStreamEvent
	parsed := len(line) <= antigravityMonitorMaxLine && strings.HasPrefix(line, "{") && json.Unmarshal([]byte(line), &event) == nil
	isInit := parsed && event.Event == "init" || (!parsed && strings.HasPrefix(line, `{"event":"init"`))

	mon.mu.Lock()
	if isInit && mon.initAt.IsZero() {
		mon.initAt = now
	} else if !mon.initAt.IsZero() {
		mon.afterIni = true
	}
	var fire string
	if parsed && event.Event == "step_update" && event.StepUpdate != nil {
		s := event.StepUpdate
		idx := -1
		if s.StepIndex != nil {
			idx = *s.StepIndex
		}
		// A later step, or the request's own DONE, resolves pending requests.
		for k := range mon.pendingPerm {
			if k != idx || strings.EqualFold(s.State, "DONE") {
				delete(mon.pendingPerm, k)
			}
		}
		if antigravityInteractionTools[s.ToolName] && !strings.EqualFold(s.State, "DONE") {
			if isAntigravityPermissionTool(s.ToolName) {
				if _, ok := mon.pendingPerm[idx]; !ok {
					mon.pendingPerm[idx] = now
					label := s.interactionLabel()
					first := now
					time.AfterFunc(mon.permSettle, func() { mon.settlePermission(idx, first, label) })
				}
			} else {
				fire = s.interactionLabel()
			}
		}
	}
	mon.mu.Unlock()
	if fire != "" {
		mon.interactionSignal(fire, now)
	}
}

func (mon *antigravityTurnMonitor) settlePermission(idx int, first time.Time, label string) {
	mon.mu.Lock()
	at, still := mon.pendingPerm[idx]
	stillSame := still && at.Equal(first)
	mon.mu.Unlock()
	if stillSame {
		mon.interactionSignal(label, first)
	}
}

// interactionSignal acts on an uncovered request: in observe mode it only
// logs (once per label); in enforce mode it stops the turn.
func (mon *antigravityTurnMonitor) interactionSignal(label string, firstSeen time.Time) {
	if mon.mode != antigravityInteractionEnforce {
		mon.mu.Lock()
		seen := mon.observed[label]
		mon.observed[label] = true
		mon.mu.Unlock()
		if !seen {
			fmt.Printf("%s[antigravity-native] Interaction request observed on %s: %q (observe mode — turn continues)%s\n",
				colorYellow, mon.sessionID, label, colorReset)
		}
		return
	}
	mon.stop(antigravityFailureInteraction, label, firstSeen)
}

// stop fires the turn's stop once.
func (mon *antigravityTurnMonitor) stop(cause, detail string, signalAt time.Time) {
	// A finished turn is never stopped: a permission settle timer can fire
	// after the run returned, and its process is gone by then.
	select {
	case <-mon.doneCh:
		return
	default:
	}
	if !mon.stopped.CompareAndSwap(false, true) {
		return
	}
	mon.mu.Lock()
	mon.stopCause, mon.stopDetail, mon.signalAt = cause, detail, signalAt
	mon.mu.Unlock()
	fmt.Printf("%s[antigravity-native] Stopping turn on %s: %s%s%s\n", colorYellow, mon.sessionID, cause,
		map[bool]string{true: " (" + detail + ")", false: ""}[detail != ""], colorReset)
	close(mon.stopCh)
	if mon.onStop != nil {
		go mon.onStop()
	}
}

// stopInfo returns the stop's cause, detail and first-sighting time ("" when
// the monitor never stopped the turn).
func (mon *antigravityTurnMonitor) stopInfo() (string, string, time.Time) {
	mon.mu.Lock()
	defer mon.mu.Unlock()
	return mon.stopCause, mon.stopDetail, mon.signalAt
}

// run ticks until finish: the stall rule and the run's own log.
func (mon *antigravityTurnMonitor) run(pid int) {
	mon.mu.Lock()
	mon.pid = pid
	mon.mu.Unlock()
	ticker := time.NewTicker(mon.tickEvery)
	defer ticker.Stop()
	for {
		select {
		case <-mon.doneCh:
			return
		case <-ticker.C:
			mon.tick(time.Now())
		}
	}
}

func (mon *antigravityTurnMonitor) finish() {
	select {
	case <-mon.doneCh:
	default:
		close(mon.doneCh)
	}
}

func (mon *antigravityTurnMonitor) tick(now time.Time) {
	if mon.stopped.Load() {
		return
	}
	mon.followLog(now)
	mon.mu.Lock()
	stalled := !mon.initAt.IsZero() && !mon.afterIni && now.Sub(mon.initAt) >= mon.stallBound
	mon.mu.Unlock()
	if stalled {
		mon.stop(antigravityFailureStalled, "", now)
	}
}

// followLog reads what the run added to its own log block since the last
// tick: activity, a quota failure line, a pending-interaction line.
func (mon *antigravityTurnMonitor) followLog(now time.Time) {
	mon.mu.Lock()
	pid, path, offset, foreign := mon.pid, mon.logPath, mon.logOffset, mon.logForeign
	searching := path == "" && mon.logSearches < antigravityMonitorLogSearchMax
	if searching {
		mon.logSearches++
	}
	mon.mu.Unlock()
	if pid <= 0 || foreign || (path == "" && !searching) {
		return
	}
	var fresh []byte
	if path == "" {
		p, block, ended, ok := antigravityPIDBlockFile(mon.logBase, pid, mon.spawnedAt)
		if !ok {
			return
		}
		info, err := os.Stat(p)
		if err != nil {
			return
		}
		mon.mu.Lock()
		// A later run's block already follows this one: what is appended
		// from here on is that run's, so the follow stops at the boundary.
		mon.logPath, mon.logOffset, mon.logForeign = p, info.Size(), ended
		mon.mu.Unlock()
		fresh = block
	} else {
		f, err := os.Open(path)
		if err != nil {
			return
		}
		info, err := f.Stat()
		if err != nil || info.Size() <= offset {
			f.Close()
			return
		}
		data := make([]byte, min(info.Size()-offset, antigravityMonitorLogChunk))
		n, _ := f.ReadAt(data, offset)
		f.Close()
		data = data[:n]
		// Only whole lines; the rest is read next tick, so the offset
		// advances only past what is consumed here.
		if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
			data = data[:i+1]
		} else if len(data) < antigravityMonitorLogChunk {
			return
		}
		consumed := int64(len(data))
		// A later run's block in the same file ends this run's block.
		want := strconv.Itoa(pid)
		for _, loc := range antigravityLanguageServerPIDPattern.FindAllSubmatchIndex(data, -1) {
			if string(data[loc[2]:loc[3]]) != want {
				data = data[:loc[0]]
				mon.mu.Lock()
				mon.logForeign = true
				mon.mu.Unlock()
				break
			}
		}
		mon.mu.Lock()
		mon.logOffset = offset + consumed
		if !mon.initAt.IsZero() && len(bytes.TrimSpace(data)) > 0 {
			mon.afterIni = true
		}
		mon.mu.Unlock()
		fresh = data
	}
	if len(fresh) == 0 {
		return
	}
	if reason := newestAntigravityQuotaReason(fresh, mon.spawnedAt.Add(-antigravityLogClockSkew), antigravityQuotaClock()); reason != "" {
		mon.mu.Lock()
		first := !mon.quotaSeen
		mon.quotaSeen = true
		mon.mu.Unlock()
		if first && mon.onQuota != nil {
			mon.onQuota(reason)
		}
	}
	for _, raw := range bytes.Split(fresh, []byte("\n")) {
		line := string(raw)
		if !antigravityGlogRecord.MatchString(line) {
			continue
		}
		if m := antigravityPendingInteractionLogLine.FindStringSubmatch(line); m != nil {
			tool := m[1]
			if tool == "" {
				tool = m[4]
			}
			mon.interactionSignal(strings.ToLower(tool), now)
			return
		}
	}
}
