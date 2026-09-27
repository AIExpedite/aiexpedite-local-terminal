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
