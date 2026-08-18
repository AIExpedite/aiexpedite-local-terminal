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
//     commands through so accepted sessions can finish. Demand commands are
//     counted until their correlated result has been published. It is an ALLOWLIST of
//     pass-through commands, not a denylist of starts, so a work type added
//     later is refused by default without needing its own opt-in. Admitted
//     continuation callbacks are counted through their final correlated
//     publish, and are refused once the idle drain is sealed for replacement.
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
// closeAdmission() flips `draining` under that same lock, no new user-work
// start can be admitted. Operational demand commands may briefly add tracked
// work until sealDrainForInstall atomically observes zero and closes that final
// admission path for the replacement handoff.
package main

import (
	"sync"
)

// drainState is the process-wide admission gate + work counters. There is one
// instance (drain); all fields are guarded by mu.
type drainState struct {
	mu          sync.Mutex
	draining    bool
	installing  bool
	attemptID   string
	registering bool
	// reconnectPending covers the tray's offline->online transition. It keeps
	// an updater from sealing between the user's reconnect click and the point
	// where that reconnect either proceeds normally or joins the active drain.
	reconnectPending bool

	// pendingStarts counts work STARTS that passed admission but whose
	// ownership has not yet been handed to a long-lived tracker — a one-shot
	// execute for its whole in-callback lifetime, or a session_start in the
	// brief window before its manager registers the session. Incremented under
	// mu at admission (so it is race-free against closeAdmission) and released
	// exactly once when the receive callback that accepted the work returns.
	pendingStarts int

	// operationalCommands counts demand callbacks that must publish a
	// correlated result before an update may restart the process. Pings are
	// deliberately excluded because they carry no durable backend marker.
	operationalCommands int
	// continuationCommands counts callbacks for work accepted before draining.
	// Managers normally keep the drain open, but this closes the race where a
	// session disappears while its final input/end callback is still publishing.
	continuationCommands int
	rejectionPublishes   int

	// uploads / approvals count the two work classes with no manager to ask.
	uploads           int
	approvals         int
	terminalPublishes int
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
func closeAdmission(attemptID string) bool {
	drain.mu.Lock()
	defer drain.mu.Unlock()
	if drain.registering {
		return false
	}
	drain.draining = true
	drain.installing = false
	drain.attemptID = attemptID
	return true
}

// beginRegistration and closeAdmission share drain.mu so registration cannot
// begin in the final check-to-drain window. Whichever transition wins makes
// the other wait rather than allowing an update to terminate device-code
// polling midway through registration.
func beginRegistration() bool {
	drain.mu.Lock()
	defer drain.mu.Unlock()
	if drain.registering || drain.draining {
		return false
	}
	drain.registering = true
	return true
}

func endRegistration() {
	drain.mu.Lock()
	drain.registering = false
	drain.mu.Unlock()
}

func registrationInProgress() bool {
	drain.mu.Lock()
	defer drain.mu.Unlock()
	return drain.registering
}

// reopenAdmission exits draining and returns the agent to normal admission.
// Called when the preference is turned off before replacement begins, when a
// drain is deferred (7-day window elapsed), or when an attempt is abandoned.
func reopenAdmission() {
	drain.mu.Lock()
	drain.draining = false
	drain.installing = false
	drain.attemptID = ""
	drain.reconnectPending = false
	drain.mu.Unlock()
}

// beginCloudReconnect reserves the drain boundary before the tray clears
// OfflineMode. A replacement that has already sealed cannot be reopened.
func beginCloudReconnect() bool {
	drain.mu.Lock()
	defer drain.mu.Unlock()
	if drain.installing {
		return false
	}
	drain.reconnectPending = true
	return true
}

// finishCloudReconnectPreparation completes the tray-side transition. If a
// drain began while OfflineMode was being cleared, keep the reservation until
// the updater has reported that drain to the service and skip generic /online.
func finishCloudReconnectPreparation() bool {
	drain.mu.Lock()
	defer drain.mu.Unlock()
	if drain.draining {
		return true
	}
	drain.reconnectPending = false
	return false
}

func completeCloudReconnectDrain() {
	drain.mu.Lock()
	drain.reconnectPending = false
	drain.mu.Unlock()
}

// admitWork is the single admission decision for an inbound command. It returns
// whether the command may proceed and, for accepted work starts, demand /
// continuation commands, and pre-seal rejection publishes, a release function
// the caller MUST defer so the callback is counted for exactly its processing
// lifetime. A refused command has a nil release only after install is sealed.
func admitWork(cmd commandMsg) (admitted bool, release func()) {
	start := isWorkStartCommand(cmd)
	trackedOperational := isInternalDemandCommand(cmd.Command)
	continuation := isWorkContinuationCommand(cmd)

	drain.mu.Lock()
	if drain.draining {
		// While draining, admit only the pass-through allowlist. Everything
		// else — one-shot execute, every *_start, and any unrecognised future
		// type — is refused so it is not silently lost or queued indefinitely.
		if !isDrainPassThrough(cmd) {
			// Count the correlated refusal publish before releasing the same
			// mutex used by sealDrainForInstall. Once sealed, acknowledge late
			// arrivals without starting a publish that replacement could cut off.
			if drain.installing {
				drain.mu.Unlock()
				return false, nil
			}
			drain.rejectionPublishes++
			drain.mu.Unlock()
			return false, drain.releaseRejectionPublish()
		}
		if trackedOperational || continuation {
			// Once the updater atomically seals an idle drain for install,
			// demand and continuation callbacks may no longer enter and
			// recreate active work after the zero-work observation.
			if drain.installing {
				drain.mu.Unlock()
				return false, nil
			}
			if trackedOperational {
				drain.operationalCommands++
			} else {
				drain.continuationCommands++
			}
			drain.mu.Unlock()
			return true, drain.releaseTrackedCommand(trackedOperational)
		}
		drain.mu.Unlock()
		return true, nil
	}

	// Not draining: admit everything. Track work starts so the close-admission
	// boundary is race-free — a start that wins this lock before closeAdmission
	// is counted and holds any subsequent drain open.
	if start {
		drain.pendingStarts++
		drain.mu.Unlock()
		return true, drain.releaseStart()
	}
	if trackedOperational {
		drain.operationalCommands++
		drain.mu.Unlock()
		return true, drain.releaseTrackedCommand(true)
	}
	if continuation {
		drain.continuationCommands++
		drain.mu.Unlock()
		return true, drain.releaseTrackedCommand(false)
	}
	drain.mu.Unlock()
	return true, nil
}

func (d *drainState) releaseRejectionPublish() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			d.mu.Lock()
			if d.rejectionPublishes > 0 {
				d.rejectionPublishes--
			}
			d.mu.Unlock()
		})
	}
}

// beginTrackedRejectionPublish brackets a correlated rejection that occurs
// before the main admission gate (stale, rate-limited, or unauthorized). It
// counts even before draining begins so closeAdmission cannot race a publish
// already in progress. Once replacement has sealed the drain, callers must
// acknowledge without starting a publish that process exit could interrupt.
func beginTrackedRejectionPublish() (release func(), allowed bool) {
	drain.mu.Lock()
	defer drain.mu.Unlock()
	if drain.installing {
		return nil, false
	}
	drain.rejectionPublishes++
	return drain.releaseRejectionPublish(), true
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

// releaseTrackedCommand returns an idempotent function that releases a demand
// or continuation callback after its correlated result publish has completed.
func (d *drainState) releaseTrackedCommand(operational bool) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			d.mu.Lock()
			if operational && d.operationalCommands > 0 {
				d.operationalCommands--
			} else if !operational && d.continuationCommands > 0 {
				d.continuationCommands--
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

// publishTerminalResultAsync publishes a terminal result without holding a
// manager session slot, while still keeping the update drain open until the
// final frame has been handed to Pub/Sub. The counter is incremented before
// the goroutine starts, so removing the corresponding session cannot expose a
// false zero to ActiveWork().
func trackTerminalPublishStart() {
	drain.mu.Lock()
	drain.terminalPublishes++
	drain.mu.Unlock()
}

func trackTerminalPublishEnd() {
	drain.mu.Lock()
	if drain.terminalPublishes > 0 {
		drain.terminalPublishes--
	}
	drain.mu.Unlock()
}

func publishTerminalResult(publishFn PublishFunc, msg resultMsg) {
	trackTerminalPublishStart()
	defer trackTerminalPublishEnd()
	publishFn(msg)
}

func publishTerminalResultAsync(publishFn PublishFunc, msg resultMsg) {
	trackTerminalPublishStart()

	go func() {
		defer trackTerminalPublishEnd()
		publishFn(msg)
	}()
}

// startTrackedTerminalPublisher drains an ordered stream queue while counting
// the publisher as active work. The count starts before the goroutine can race
// a session removal and ends only after every queued frame has returned from
// publishFn, so an automatic update cannot restart between manager removal and
// a slow publisher finishing its backlog.
func startTrackedTerminalPublisher(queue <-chan resultMsg, publishFn PublishFunc) <-chan struct{} {
	done := make(chan struct{})
	trackTerminalPublishStart()
	go func() {
		defer close(done)
		defer trackTerminalPublishEnd()
		for msg := range queue {
			publishFn(msg)
		}
	}()
	return done
}

// ActiveWork returns the total number of accepted units of work that have not
// yet reached a terminal state. A drain may install only when this is zero.
func ActiveWork() int {
	// Hold the admission lock while sampling manager-owned work and the
	// pending-start counter. A session start hands ownership to its manager
	// before its deferred release decrements pendingStarts; serializing that
	// release with this whole snapshot prevents observing the manager before
	// insertion and pendingStarts after release (a false zero).
	drain.mu.Lock()
	defer drain.mu.Unlock()
	return drain.activeWorkLocked()
}

func (d *drainState) activeWorkLocked() int {
	total := 0

	drainWorkSourcesMu.Lock()
	for _, fn := range drainWorkSources {
		total += fn()
	}
	drainWorkSourcesMu.Unlock()

	total += d.pendingStarts + d.operationalCommands + d.continuationCommands + d.rejectionPublishes + d.uploads + d.approvals + d.terminalPublishes
	if d.reconnectPending {
		total++
	}
	return total
}

// sealDrainForInstall atomically verifies that the drain is idle and prevents
// fresh demand commands from entering after that zero-work observation. This
// closes the final admission-to-install race while leaving pings deliverable.
func sealDrainForInstall() bool {
	drain.mu.Lock()
	defer drain.mu.Unlock()
	if !drain.draining || drain.installing || drain.activeWorkLocked() != 0 {
		return false
	}
	drain.installing = true
	return true
}

// isOperationalCommand reports whether the command carries no user work — it is
// health / demand traffic that must keep flowing even while draining.
func isOperationalCommand(command string) bool {
	return command == "__ping__" || isInternalDemandCommand(command)
}

// isWorkStartCommand reports whether the command STARTS a new unit of user work
// (a one-shot execute, interactive session start, or unrecognized command type
// that publishes a correlated error). These are the commands refused while draining.
func isWorkStartCommand(cmd commandMsg) bool {
	return !isDrainPassThrough(cmd)
}

// isDrainPassThrough is the allowlist of commands admitted while draining:
// operational/demand traffic, and continuation messages for work accepted
// before the drain began. Everything not on this list is refused.
func isDrainPassThrough(cmd commandMsg) bool {
	if isOperationalCommand(cmd.Command) {
		return true
	}
	return isWorkContinuationCommand(cmd)
}

// isWorkContinuationCommand reports traffic correlated to work accepted before
// a drain. These callbacks remain deliverable while draining, but are tracked
// until they return and are no longer admitted after the install seal.
func isWorkContinuationCommand(cmd commandMsg) bool {
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
