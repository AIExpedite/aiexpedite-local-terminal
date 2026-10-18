// File: presence.go
// -----------------------------------------------------------------------------
// Coarse presence: whether the person is at this computer, so background work
// can adapt instead of disturbing them.
// PARALLEL_RUNS_AND_COMPUTER_MAINTENANCE_PLAN.md §8 (Phase 0 measures it;
// Phase 3 is the first phase that lets it change anything).
//
// WHAT CROSSES THE WIRE, and nothing else (§8.3, decision D12):
//   - one of exactly three states — active / idle / away — and how long it has
//     held.
// Never an application, a window title, a keystroke, an idle-seconds history,
// or a "presenting" state. `presenting` is deliberately absent from the
// vocabulary: it is a statement about what the person is DOING, which is what
// §8.3 promises not to report, so a presenting or do-not-disturb session is
// folded into `active` HERE, on the device, and the no-interrupt rule falls out
// of the state already disclosed.
//
// EVERY UNKNOWN IS `active`, never `away`. Presence only ever REDUCES
// background work, so an OS we cannot read costs parallelism and never
// disturbs anyone. `away` is the dangerous guess: it ramps work UP on a
// computer someone may be sitting at.
//
// THE OFF SWITCH (D12) has two independent layers, and either one stops the
// sampling itself rather than merely dropping the result:
//  1. the owner's per-computer switch, as terminal-service answers every load
//     report (`presenceEnabled`);
//  2. AIEXPEDITE_PRESENCE_REPORTING=off|0|false|no on this machine, for the
//     person at the keyboard, independent of the cloud.
// With presence off the computer reads as `active` for routing — that is the
// server's reading of an absent state, and it is the safe direction above.
//
// The three state names and the two thresholds are owned by
// @aiexpedite/shared-constants (constants/parallelRuns/presence.js). Go cannot
// import it, so they are mirrored here and pinned by
// testdata/device_load_report_vectors.json, which terminal-service asserts
// against the real constants: a change there fails that test, and this copy's
// own test fails until it follows.
// -----------------------------------------------------------------------------

package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The presence vocabulary (shared-constants PRESENCE_STATE). Exactly three
// states, plus the empty string for "this device reports no presence", which
// is NOT a state and never stored as one.
const (
	presenceStateActive      = "active"
	presenceStateIdle        = "idle"
	presenceStateAway        = "away"
	presenceStateNotReported = ""
)

// shared-constants PRESENCE_ACTIVE_WITHIN_MS / PRESENCE_AWAY_AFTER_MS (§8.2).
const (
	presenceActiveWithin = 5 * time.Minute
	presenceAwayAfter    = 20 * time.Minute
)

// presenceEnvVar lets the person at the keyboard stop presence sampling
// without the cloud. Any of off / 0 / false / no (case-insensitive) disables it.
const presenceEnvVar = "AIEXPEDITE_PRESENCE_REPORTING"

// presenceSignals is what this OS could tell us. Each `…OK` flag is the
// honest part: a signal we cannot read is absent, never a default that would
// look like a measurement.
type presenceSignals struct {
	// Idle is the time since the last keyboard / mouse / pen input in THIS
	// session.
	Idle   time.Duration
	IdleOK bool
	// Locked is the screen-lock state of the session the person uses.
	Locked   bool
	LockedOK bool
	// DisplayOff is a display that is asleep or off.
	DisplayOff   bool
	DisplayOffOK bool
}

// probePresenceSignals is the per-OS probe (presence_windows.go,
// presence_darwin.go, presence_other.go). A var so tests replace it and so no
// test ever touches the real desktop.
var probePresenceSignals = platformPresenceSignals

// presenceAllowedByOwner mirrors the owner's switch as the server last
// answered it, as a TRI-STATE: unknown, allowed, or denied.
//
// It used to start `true`, reasoning that D12 ships presence ON by default. That
// was the wrong read of D12: the default governs the SERVER's stored setting,
// not what an agent may assume before it has been told. The switch only arrives
// in a load-report RESPONSE, so a process-local flag defaulting to allowed
// meant a computer whose owner had turned presence OFF probed and transmitted
// it on the first report after every start or restart — and on every retry if
// that response was lost. An opt-out that a restart silently undoes is not an
// opt-out. (Codex, PR #231.)
//
// So: UNKNOWN WITHHOLDS. The answer is also persisted, so the withholding costs
// one interval on a brand-new install rather than one after every restart.
const (
	presenceOwnerUnknown int32 = 0
	presenceOwnerAllowed int32 = 1
	presenceOwnerDenied  int32 = 2
)

var presenceAllowedByOwner = &atomic.Int32{} // zero value is UNKNOWN, deliberately

// presenceOwnerStatePath is where the last known answer is remembered. A small
// dedicated file rather than config.json: that file holds the registration, and
// a stray write to it has unregistered a device before (see paths.go).
func presenceOwnerStatePath() string {
	return filepath.Join(GetConfigDir(), "presence-reporting.state")
}

// loadPresenceOwnerState restores the last known answer. Anything unreadable or
// unrecognised stays UNKNOWN, which withholds — a corrupt file must not be a
// licence to report.
func loadPresenceOwnerState() {
	raw, err := os.ReadFile(presenceOwnerStatePath())
	if err != nil {
		return
	}
	switch strings.ToLower(strings.TrimSpace(string(raw))) {
	case "allowed":
		presenceAllowedByOwner.Store(presenceOwnerAllowed)
	case "denied":
		presenceAllowedByOwner.Store(presenceOwnerDenied)
	}
}

// setPresenceAllowedByOwner records the switch from a load-report response and
// remembers it across restarts. A failed write only costs one withheld interval
// after the next restart, so it is not worth failing the report over.
func setPresenceAllowedByOwner(allowed bool) {
	state, text := presenceOwnerDenied, "denied"
	if allowed {
		state, text = presenceOwnerAllowed, "allowed"
	}
	presenceAllowedByOwner.Store(state)
	_ = os.MkdirAll(GetConfigDir(), 0o700)
	_ = os.WriteFile(presenceOwnerStatePath(), []byte(text), 0o600)
}

// presenceEnvLookup is os.LookupEnv, replaced in tests.
var presenceEnvLookup = os.LookupEnv

// presenceDisabledLocally reports the machine-local override.
func presenceDisabledLocally() bool {
	raw, ok := presenceEnvLookup(presenceEnvVar)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "off", "0", "false", "no":
		return true
	default:
		return false
	}
}

// presenceSamplingEnabled answers whether this computer may READ presence at
// all. Both layers gate the probe itself, not just the report, so "off" means
// the idle timer is never consulted.
func presenceSamplingEnabled() bool {
	// UNKNOWN withholds: only an explicit `allowed` from the server opens the
	// probe. See presenceAllowedByOwner for why the default is not `allowed`.
	return !presenceDisabledLocally() && presenceAllowedByOwner.Load() == presenceOwnerAllowed
}

// classifyPresence turns one probe into one state (§8.2):
//
//	away   no input for ≥ 20 min, OR the screen locked, OR the display asleep
//	idle   no input for 5–20 min, unlocked
//	active anything else — including every signal this OS cannot read
//
// A lock or a sleeping display outranks the idle timer because both are
// positive evidence the person is not looking at the screen, while the idle
// timer on a locked session only tells us about the session we can see.
func classifyPresence(s presenceSignals) string {
	if s.LockedOK && s.Locked {
		return presenceStateAway
	}
	if s.DisplayOffOK && s.DisplayOff {
		return presenceStateAway
	}
	if !s.IdleOK {
		// Nothing readable: `active`, so the computer costs parallelism
		// instead of risking an interruption.
		return presenceStateActive
	}
	switch {
	case s.Idle >= presenceAwayAfter:
		return presenceStateAway
	case s.Idle >= presenceActiveWithin:
		return presenceStateIdle
	default:
		return presenceStateActive
	}
}

// presenceStateStart estimates when the state began.
//
// For idle and away the idle timer dates it exactly — the state began when the
// timer crossed the threshold. For everything else (active, or a state we only
// know from the lock / display signal) the start is unknowable without keeping
// a history of the person's activity, which §8.3 forbids, so it is "not
// earlier than when we first saw it": the tracker's own first observation.
func presenceStateStart(now time.Time, state string, s presenceSignals) time.Time {
	if !s.IdleOK {
		return now
	}
	switch state {
	case presenceStateAway:
		if s.Idle >= presenceAwayAfter {
			return now.Add(-(s.Idle - presenceAwayAfter))
		}
	case presenceStateIdle:
		if s.Idle >= presenceActiveWithin {
			return now.Add(-(s.Idle - presenceActiveWithin))
		}
	}
	return now
}

// isTimerDerivedPresence reports whether this state's start is computed from
// the idle timer rather than from the moment of observation. Only those carry
// evidence of WHEN the state began, which is what lets an unobserved
// transition be detected without keeping any history of what the person did.
func isTimerDerivedPresence(state string) bool {
	return state == presenceStateIdle || state == presenceStateAway
}

// presenceTracker holds the current state and when it began, so `since` does
// not need any history of what the person did.
type presenceTracker struct {
	mu    sync.Mutex
	state string
	since time.Time
}

var globalPresenceTracker = &presenceTracker{}

// presenceSample is what the load report carries: a state and its age, or
// nothing at all.
type presenceSample struct {
	State string
	// AgeMs is how long State has held, or loadMetricUnreadable when the
	// device cannot date it. -1 with an empty State means "not reported".
	AgeMs int64
}

// notReportedPresence is the sample for a computer that reports no presence:
// the switch is off, or this OS gives nothing. The server reads it as `active`
// (§8.3).
func notReportedPresence() presenceSample {
	return presenceSample{State: presenceStateNotReported, AgeMs: loadMetricUnreadable}
}

// observe folds one probe into the tracker and returns the state with its age.
func (t *presenceTracker) observe(now time.Time, s presenceSignals) presenceSample {
	state := classifyPresence(s)
	start := presenceStateStart(now, state, s)
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.state != state || t.since.IsZero():
		t.state, t.since = state, start
	case start.After(t.since) && isTimerDerivedPresence(state) && s.IdleOK:
		// AN UNOBSERVED TRANSITION. Two consecutive samples can both classify
		// `idle` while the person was active in between — easily, once the
		// server configures a sample interval longer than the five-minute idle
		// threshold. The state did not change, so the old branch kept the old
		// `since` and the reported age then SPANNED that activity.
		//
		// The idle timer is evidence the state alone is not: a later derived
		// start means the current idle period began after the one on record,
		// which can only be because it was interrupted. Only ever moved
		// FORWARD, and only for the states whose start the timer actually
		// derives — for `active` the derived start is simply `now`, so
		// advancing on it would peg the age at zero every sample.
		// (Codex, PR #231.)
		t.since = start
	}
	age := now.Sub(t.since)
	if age < 0 {
		age = 0
	}
	return presenceSample{State: state, AgeMs: age.Milliseconds()}
}

// reset clears the tracker, so a switch turned off and back on cannot report
// an age that spans the gap.
func (t *presenceTracker) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state, t.since = "", time.Time{}
}

// samplePresence is the only entry point the reporter uses.
//
// When presence is off it returns "not reported" WITHOUT probing: the switch
// stops the device reading the person's activity, it does not merely discard
// what was read.
func samplePresence(now time.Time) presenceSample {
	if !presenceSamplingEnabled() {
		globalPresenceTracker.reset()
		return notReportedPresence()
	}
	signals := probePresenceSignals()
	if !signals.IdleOK && !signals.LockedOK && !signals.DisplayOffOK {
		// This OS tells us nothing. Report nothing rather than a confident
		// `active` the server cannot distinguish from a real reading; the
		// server's reading of "nothing" is `active` anyway.
		globalPresenceTracker.reset()
		return notReportedPresence()
	}
	return globalPresenceTracker.observe(now, signals)
}
