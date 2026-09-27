// File: fenced_sessions.go
// -----------------------------------------------------------------------------
// Sessions terminal-service FENCED on this device (failover Wave 4 Phase 2,
// plan §4.7b): a user's "Move to another computer", or the end of a run's
// maximum park, moved the run off this computer. /online answers their ids
// (`fencedSessionIds`). The agent then:
//
//  1. records them here at once, so the Pub/Sub entry gate refuses every
//     command for them except their END (refuseFencedSessionCommand) — nothing
//     else can run for their executions from this moment;
//  2. ends each one it still holds, in whichever session manager has it.
//
// The set is per process (a new boot holds none of the old sessions, and every
// /online re-sends the current list) and bounded.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"cloud.google.com/go/pubsub/v2"
)

// fencedSessionsMax bounds the in-memory set; the oldest ids go first. A
// device holds a handful of sessions at a time.
const fencedSessionsMax = 1024

var fencedSessions = struct {
	sync.Mutex
	ids   map[string]struct{}
	order []string
}{ids: make(map[string]struct{})}

// isSessionFenced reports whether terminal-service fenced sessionID.
func isSessionFenced(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	fencedSessions.Lock()
	defer fencedSessions.Unlock()
	_, ok := fencedSessions.ids[sessionID]
	return ok
}

// markSessionsFenced records ids and returns the ones newly fenced.
func markSessionsFenced(ids []string) []string {
	fencedSessions.Lock()
	defer fencedSessions.Unlock()
	var added []string
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := fencedSessions.ids[id]; ok {
			continue
		}
		fencedSessions.ids[id] = struct{}{}
		fencedSessions.order = append(fencedSessions.order, id)
		added = append(added, id)
	}
	for len(fencedSessions.order) > fencedSessionsMax {
		delete(fencedSessions.ids, fencedSessions.order[0])
		fencedSessions.order = fencedSessions.order[1:]
	}
	return added
}

// endFencedSession ends one fenced session. A var so tests can observe it.
var endFencedSession = endSessionOnDevice

// endSessionOnDevice ends sessionID in whichever manager holds it. A session
// this process never held (a new boot) is a no-op.
func endSessionOnDevice(sessionID string) {
	ended := false
	if m := globalSessionManager; m != nil && m.GetSession(sessionID) != nil {
		ended = true
		if err := m.EndSession(sessionID); err != nil {
			fmt.Printf("%s[fence] Ending session %s: %v%s\n", colorYellow, sessionID, err, colorReset)
		}
	}
	if m := globalCodexAppServerManager; m != nil && m.Get(sessionID) != nil {
		ended = true
		if err := m.End(sessionID); err != nil {
			fmt.Printf("%s[fence] Ending codex session %s: %v%s\n", colorYellow, sessionID, err, colorReset)
		}
	}
	if m := globalClaudeNativeManager; m != nil && m.Get(sessionID) != nil {
		ended = true
		if err := m.End(sessionID); err != nil {
			fmt.Printf("%s[fence] Ending claude session %s: %v%s\n", colorYellow, sessionID, err, colorReset)
		}
	}
	if m := globalGrokACPManager; m != nil && m.Get(sessionID) != nil {
		ended = true
		if err := m.End(sessionID); err != nil {
			fmt.Printf("%s[fence] Ending grok session %s: %v%s\n", colorYellow, sessionID, err, colorReset)
		}
	}
	if m := globalAntigravityNativeManager; m != nil && m.Get(sessionID) != nil {
		ended = true
		if err := m.End(sessionID); err != nil {
			fmt.Printf("%s[fence] Ending antigravity session %s: %v%s\n", colorYellow, sessionID, err, colorReset)
		}
	}
	if m := globalOpenCodeNativeManager; m != nil && m.Get(sessionID) != nil {
		ended = true
		if err := m.End(sessionID); err != nil {
			fmt.Printf("%s[fence] Ending opencode session %s: %v%s\n", colorYellow, sessionID, err, colorReset)
		}
	}
	if ended {
		fmt.Printf("%s[fence] Ended fenced session %s%s\n", colorYellow, sessionID, colorReset)
	}
}

// handleFencedSessions fences ids and ends the ones this process holds. The
// fence is recorded synchronously (the entry gate refuses everything but END
// from here on); the ends run in the background so the /online caller, which
// holds the connectivity mutex, is not held for a session's graceful stop.
func handleFencedSessions(ids []string) {
	added := markSessionsFenced(ids)
	if len(added) == 0 {
		return
	}
	fmt.Printf("%s[fence] terminal-service fenced %d session(s) on this device: %v%s\n",
		colorYellow, len(added), added, colorReset)
	for _, id := range added {
		go endFencedSession(id)
	}
}

// ── The fence report gate ────────────────────────────────────────────────
//
// The Pub/Sub loop starts before the /online that answers the fence list (at
// boot and on a tray Reconnect), so until an /online has been ACCEPTED on
// this connection the fence set may be incomplete. Session commands other
// than END wait for it (bounded) and are otherwise Nacked for redelivery; a
// background loop keeps re-sending /online until one is accepted.

// fenceReportGateWait bounds how long one session command waits for the
// fence report before it is Nacked (Pub/Sub redelivers it).
const fenceReportGateWait = 15 * time.Second

var fenceReport = struct {
	sync.Mutex
	applied bool
	ready   chan struct{}
}{ready: make(chan struct{})}

// markFenceReportApplied records that an accepted /online's fence list is in
// force (called after handleFencedSessions).
func markFenceReportApplied() {
	fenceReport.Lock()
	defer fenceReport.Unlock()
	if !fenceReport.applied {
		fenceReport.applied = true
		close(fenceReport.ready)
	}
}

// resetFenceReport forgets it: the agent went offline, and the next
// connection must learn the fences again before running session commands.
func resetFenceReport() {
	fenceReport.Lock()
	defer fenceReport.Unlock()
	if fenceReport.applied {
		fenceReport.applied = false
		fenceReport.ready = make(chan struct{})
	}
}

// fenceReportConnections counts Pub/Sub connections of this process.
var fenceReportConnections atomic.Int64

// beginFenceReportConnection starts a Pub/Sub connection: from the second
// connection on it forgets the fence report, then makes sure an /online is
// (re)sent until one is accepted.
func beginFenceReportConnection(cfg *Config) {
	if fenceReportConnections.Add(1) > 1 {
		resetFenceReport()
	}
	ensureFenceReport(cfg)
}

func fenceReportApplied() bool {
	fenceReport.Lock()
	defer fenceReport.Unlock()
	return fenceReport.applied
}

// waitFenceReport waits until the fence report is applied, ctx ends, or wait
// elapses, and reports whether it is applied.
func waitFenceReport(ctx context.Context, wait time.Duration) bool {
	fenceReport.Lock()
	if fenceReport.applied {
		fenceReport.Unlock()
		return true
	}
	ready := fenceReport.ready
	fenceReport.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ready:
		return true
	case <-timer.C:
	case <-ctx.Done():
	}
	return fenceReportApplied()
}

// holdForFenceReport is the entry gate half that waits for the fence report.
// END and a signal (interrupt / kill: a cancel) always pass — stopping a
// session is always safe. Everything that starts or drives a session waits,
// and is Nacked (never acked) for redelivery when the report has not landed
// within wait. Returns true when it handled (Nacked) the message.
func holdForFenceReport(ctx context.Context, m *pubsub.Message, cmd commandMsg, wait time.Duration) bool {
	if isSessionEndCommandType(cmd.Type) || cmd.Type == "session_signal" || waitFenceReport(ctx, wait) {
		return false
	}
	fmt.Printf("%s[fence] %s for session %s held: no /online fence report accepted yet — Nacked for redelivery%s\n",
		colorYellow, cmd.Type, cmd.SessionID, colorReset)
	if m != nil {
		m.Nack()
	}
	return true
}

// fenceWorker is the single-flight state of ensureFenceReport. A worker
// decides to stop and clears running in ONE critical section, so a
// resetFenceReport + ensureFenceReport racing a stopping worker either finds
// it still running (it then sees the reset and keeps going) or finds it gone
// and starts a new one — a new connection is never left without a worker.
var fenceWorker struct {
	sync.Mutex
	running bool
}

// fenceWorkerRunning reports whether an ensureFenceReport worker is active.
func fenceWorkerRunning() bool {
	fenceWorker.Lock()
	defer fenceWorker.Unlock()
	return fenceWorker.running
}

// sendOnlineForFenceReport is the /online behind a seam for tests.
var sendOnlineForFenceReport = func(ctx context.Context, cfg *Config) error { return notifyOnline(ctx, cfg) }

// fenceReportRetryDelays is the backoff of ensureFenceReport. The first
// delay leaves the caller's own /online (boot, Reconnect) time to land.
var fenceReportRetryDelays = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}

// ensureFenceReport keeps re-sending /online until one is accepted, so a
// failed boot / reconnect /online cannot hold session commands for the whole
// connection. It stops when the report is applied, on shutdown, when the
// agent is offline or unregistered, and it leaves /online to the update
// reconciliation while an update attempt is pending (that path reports the
// version-aware /online, which applies the fences too).
func ensureFenceReport(cfg *Config) {
	if cfg == nil {
		return
	}
	registered := false
	cfg.WithPersistenceLock(func() { registered = cfg.IsRegistered() && !cfg.OfflineMode })
	if !registered {
		return // nothing to report as; a registration starts a new connection
	}
	fenceWorker.Lock()
	if fenceWorker.running {
		fenceWorker.Unlock()
		return
	}
	fenceWorker.running = true
	fenceWorker.Unlock()

	// Read the seams once, here, so the loop never races a test restoring them.
	delays := append([]time.Duration(nil), fenceReportRetryDelays...)
	send := sendOnlineForFenceReport
	// stopIf ends the worker when stop() holds, deciding and releasing the
	// single-flight slot atomically.
	stopIf := func(stop func() bool) bool {
		fenceWorker.Lock()
		defer fenceWorker.Unlock()
		if stop() {
			fenceWorker.running = false
			return true
		}
		return false
	}
	go func() {
		for attempt := 0; ; attempt++ {
			delay := delays[len(delays)-1]
			if attempt < len(delays) {
				delay = delays[attempt]
			}
			select {
			case <-shutdownChan:
				stopIf(func() bool { return true })
				return
			case <-time.After(delay):
			}
			registered, pendingUpdate := false, false
			cfg.WithPersistenceLock(func() {
				registered = cfg.IsRegistered() && !cfg.OfflineMode
				pendingUpdate = cfg.PendingUpdateAttemptID != ""
			})
			if stopIf(func() bool {
				return fenceReportApplied() || IsShutdownInProgress() || IsOffline() || !registered
			}) {
				return
			}
			if !pendingUpdate {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = send(ctx, cfg)
				cancel()
			}
		}
	}()
}

// isSessionEndCommandType reports the END command of every session family.
func isSessionEndCommandType(t string) bool {
	switch t {
	case "session_end", "codex_appserver_end", "claude_native_end",
		"grok_acp_end", "antigravity_native_end", "opencode_native_end":
		return true
	}
	return false
}

// fencedSessionRejectionCode is the rejection reason for a refused command.
const fencedSessionRejectionCode = "SESSION_FENCED"

// refuseFencedSessionCommand refuses any command for a fenced session except
// its END. Returns true when it handled (refused and settled) the message.
func refuseFencedSessionCommand(ctx context.Context, topic *pubsub.Publisher, m *pubsub.Message, cmd commandMsg, cfg *Config) bool {
	if cmd.SessionID == "" || isSessionEndCommandType(cmd.Type) || !isSessionFenced(cmd.SessionID) {
		return false
	}
	fmt.Printf("%s[fence] Refused %s for fenced session %s%s\n", colorYellow, cmd.Type, cmd.SessionID, colorReset)
	agentID := ""
	if cfg != nil {
		agentID = cfg.AgentID
	}
	res := makeRejectionResult(cmd, agentID, "denied", fencedSessionRejectionCode,
		fmt.Sprintf("session %s was moved off this computer; only its END is accepted", cmd.SessionID))
	err := publishMsg(ctx, topic, res)
	if m != nil {
		if err != nil {
			m.Nack()
		} else {
			m.Ack()
		}
	}
	return true
}
