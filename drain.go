// File: drain.go
// -----------------------------------------------------------------------------
// Update-drain admission gate and in-flight work accounting.
//
// When an automatic update has been downloaded and verified, the agent enters
// a "draining" state: it stays connected and keeps finishing work it already
// accepted, but refuses every NEW unit of work so it can install and restart
// without interrupting a running job. This file owns the two primitives that
// make that safe:
//
//  1. admitWork(cmd) — the SINGLE admission decision, consulted from one place
//     in the pubsub receive loop (after ping/staleness/rate-limit/signature).
//     While draining it refuses new work STARTS (one-shot execute, and every
//     *_start session family) but lets continuation / input / signal / end and
//     the operational __ping__ / __cli_usage_refresh__ / __env_inspect__
//     commands through so accepted sessions can finish. It is an ALLOWLIST of
//     pass-through commands, not a denylist of starts, so a work type added
//     later is refused by default without needing its own opt-in.
//
//  2. ActiveWork() — the single aggregate in-flight count. Before this the
//     count was scattered across the session manager, four CLI-agent managers,
//     and the process registry, with no way to ask "is anything still running?"
//     in one call. It also folds in two work classes that have no manager to
//     ask — an open native approval dialog, and an in-flight file upload — each
//     of which must hold the drain open (a drain that ignored either could
//     restart the process under a dialog the user is still reading, or
//     mid-upload). Work whose terminal condition cannot be observed counts as
//     active; nothing is assumed complete to let a drain finish.
//
// The admission decision and the "decide the drain is complete" read share one
// mutex, so a work start arriving exactly at the close-admission boundary is
// either fully accepted before the drain completes (and therefore holds it
// open) or refused — never accepted after ActiveWork() reported zero. Because
// closeAdmission() flips `draining` under that same lock, once draining begins
// ActiveWork() can only decrease: no new start can be admitted, so the count
// converges to zero and stays there until the restart.
package main

import (
	"strings"
	"sync"
)

// drainState is the process-wide admission gate + work counters. There is one
// instance (drain); all fields are guarded by mu.
type drainState struct {
	mu        sync.Mutex
	draining  bool
	attemptID string

	// pendingStarts counts work STARTS that passed admission but whose
	// ownership has not yet been handed to a long-lived tracker — a one-shot
	// execute for its whole in-callback lifetime, or a session_start in the
	// brief window before its manager registers the session. Incremented under
	// mu at admission (so it is race-free against closeAdmission) and released
	// exactly once when the receive callback that accepted the work returns.
	pendingStarts int

	// uploads / approvals count the two work classes with no manager to ask.
	uploads   int
	approvals int
}

var drain = &drainState{}

// drainWorkSources are functions that report how many units of work each
// subsystem is currently tracking. StartAgent registers one per manager after
// the managers exist; tests may register a fake source. Kept as a registry
// (rather than direct global reads inside ActiveWork) so the aggregate is unit
// testable without constructing every real manager.
var (
	drainWorkSourcesMu sync.Mutex
	drainWorkSources   []func() int
)

// registerDrainWorkSource adds a contributor to ActiveWork(). Called once per
// manager from StartAgent.
func registerDrainWorkSource(fn func() int) {
	if fn == nil {
		return
	}
	drainWorkSourcesMu.Lock()
	drainWorkSources = append(drainWorkSources, fn)
	drainWorkSourcesMu.Unlock()
}

// isDraining reports whether the agent is currently draining for an update.
func isDraining() bool {
	drain.mu.Lock()
	defer drain.mu.Unlock()
	return drain.draining
}

// drainingAttempt returns the attempt id the current drain belongs to, or "".
func drainingAttempt() string {
	drain.mu.Lock()
	defer drain.mu.Unlock()
	return drain.attemptID
}

// closeAdmission enters draining for the given attempt. From this instant the
// agent's own refusal is authoritative — new work starts are refused whether or
// not the cloud has learned of the drain yet. Idempotent for the same attempt.
func closeAdmission(attemptID string) {
	drain.mu.Lock()
	drain.draining = true
	drain.attemptID = attemptID
	drain.mu.Unlock()
}

// reopenAdmission exits draining and returns the agent to normal admission.
// Called when the preference is turned off before replacement begins, when a
// drain is deferred (7-day window elapsed), or when an attempt is abandoned.
func reopenAdmission() {
	drain.mu.Lock()
	drain.draining = false
	drain.attemptID = ""
	drain.mu.Unlock()
}

// admitWork is the single admission decision for an inbound command. It returns
// whether the command may proceed and, for accepted work starts, a release
// function the caller MUST defer so the work is counted for exactly its
// processing lifetime. release is nil when there is nothing to release
// (continuation / operational traffic, or a refused start).
func admitWork(cmd commandMsg) (admitted bool, release func()) {
	start := isWorkStartCommand(cmd)

	drain.mu.Lock()
	if drain.draining {
		drain.mu.Unlock()
		// While draining, admit only the pass-through allowlist. Everything
		// else — one-shot execute, every *_start, and any unrecognised future
		// type — is refused so it is not silently lost or queued indefinitely.
		if isDrainPassThrough(cmd) {
			return true, nil
		}
		return false, nil
	}

	// Not draining: admit everything. Track work starts so the close-admission
	// boundary is race-free — a start that wins this lock before closeAdmission
	// is counted and holds any subsequent drain open.
	if start {
		drain.pendingStarts++
		drain.mu.Unlock()
		return true, drain.releaseStart()
	}
	drain.mu.Unlock()
	return true, nil
}

// releaseStart returns an idempotent function that decrements pendingStarts.
func (d *drainState) releaseStart() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			d.mu.Lock()
			if d.pendingStarts > 0 {
				d.pendingStarts--
			}
			d.mu.Unlock()
		})
	}
}

// trackUploadStart / trackUploadEnd bracket an in-flight file upload so it holds
// the drain open until it completes or fails durably.
func trackUploadStart() {
	drain.mu.Lock()
	drain.uploads++
	drain.mu.Unlock()
}

func trackUploadEnd() {
	drain.mu.Lock()
	if drain.uploads > 0 {
		drain.uploads--
	}
	drain.mu.Unlock()
}

// trackApprovalStart / trackApprovalEnd bracket an open native approval dialog
// so an update never restarts the process while the user is still reading it.
func trackApprovalStart() {
	drain.mu.Lock()
	drain.approvals++
	drain.mu.Unlock()
}

func trackApprovalEnd() {
	drain.mu.Lock()
	if drain.approvals > 0 {
		drain.approvals--
	}
	drain.mu.Unlock()
}

// ActiveWork returns the total number of accepted units of work that have not
// yet reached a terminal state. A drain may install only when this is zero.
func ActiveWork() int {
	total := 0
	drainWorkSourcesMu.Lock()
	for _, fn := range drainWorkSources {
		total += fn()
	}
	drainWorkSourcesMu.Unlock()

	drain.mu.Lock()
	total += drain.pendingStarts + drain.uploads + drain.approvals
	drain.mu.Unlock()
	return total
}

// isOperationalCommand reports whether the command carries no user work — it is
// health / demand traffic that must keep flowing even while draining.
func isOperationalCommand(command string) bool {
	return command == "__ping__" || isInternalDemandCommand(command)
}

// isWorkStartCommand reports whether the command STARTS a new unit of user work
// (a one-shot execute, or any interactive session start). These are the only
// commands refused while draining.
func isWorkStartCommand(cmd commandMsg) bool {
	switch cmd.Type {
	case "", "execute":
		// A one-shot shell execution. Operational demand commands also carry an
		// empty Type, so exclude them explicitly.
		return !isOperationalCommand(cmd.Command)
	case "session_start",
		"codex_appserver_start",
		"claude_native_start",
		"grok_acp_start",
		"antigravity_native_start":
		return true
	default:
		// Any *_start family added later is a new work start too.
		return strings.HasSuffix(cmd.Type, "_start")
	}
}

// isDrainPassThrough is the allowlist of commands admitted while draining:
// operational/demand traffic, and continuation messages for work accepted
// before the drain began. Everything not on this list is refused.
func isDrainPassThrough(cmd commandMsg) bool {
	if isOperationalCommand(cmd.Command) {
		return true
	}
	switch cmd.Type {
	case "session_input", "session_signal", "session_end",
		"codex_appserver_send", "codex_appserver_end",
		"claude_native_send", "claude_native_end",
		"grok_acp_send", "grok_acp_end",
		"antigravity_native_send", "antigravity_native_end":
		return true
	}
	return false
}
