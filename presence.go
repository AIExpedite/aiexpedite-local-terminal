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
// So: UNKNOWN WITHHOLDS, and only a DENIAL is ever remembered across restarts.
// Remembering the opt-IN too was the obvious next step, and was wrong: see
// setPresenceAllowedByOwner.
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

// loadPresenceOwnerState restores a remembered DENIAL. Anything else — an
// unreadable file, an unrecognised word, or the `allowed` an earlier build
// wrote — stays UNKNOWN, which withholds. A file can only ever take presence
// reporting away, never grant it, and that is what makes a stale or corrupt
// one harmless.
func loadPresenceOwnerState() {
	raw, err := os.ReadFile(presenceOwnerStatePath())
	if err != nil {
		return
	}
	if strings.EqualFold(strings.TrimSpace(string(raw)), "denied") {
		presenceAllowedByOwner.Store(presenceOwnerDenied)
	}
}

// setPresenceAllowedByOwner records the switch from a load-report response.
//
// ONLY A DENIAL IS PERSISTED, and the asymmetry is the whole point. Remembering
// the opt-IN as well looked free — it saves one withheld interval after a
// restart — but it reopens the hole the tri-state was built to close: if the
// owner turns presence off while this agent is stopped, a remembered `allowed`
// authorises the first probe before any response can carry the new `false`. The
// file also names no agent, so that authorisation outlives a re-registration.
// A remembered DENIAL has no equivalent failure: the worst a stale one does is
// withhold. The file's asymmetry therefore matches the costs' — a withheld
// sample is a missing measurement, a sent one is a privacy breach. (Codex, PR
// #231.)
//
// A failed write costs one unwithheld interval after the next restart; it is
// not worth failing the report over, and the in-memory state is right now.
func setPresenceAllowedByOwner(allowed bool) {
	if allowed {
		presenceAllowedByOwner.Store(presenceOwnerAllowed)
		// Drop any remembered denial — keeping it would withhold for an
		// interval after every restart of a computer that has since opted in.
		_ = os.Remove(presenceOwnerStatePath())
		return
	}
	presenceAllowedByOwner.Store(presenceOwnerDenied)
	_ = os.MkdirAll(GetConfigDir(), 0o700)
	_ = os.WriteFile(presenceOwnerStatePath(), []byte("denied"), 0o600)
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

// presenceStateStart estimates when the state began, and says whether the idle
// timer is what dated it.
//
// For idle and away PAST THEIR THRESHOLDS the timer dates the start exactly —
// the state began when the timer crossed. For everything else (active, or an
// `away` we know only from the lock or the sleeping display) the start is
// unknowable without keeping a history of the person's activity, which §8.3
// forbids, so it is "not earlier than when we first saw it": the tracker's own
// first observation.
//
// THE SECOND RETURN IS NOT THE SAME AS "the state is idle or away". A locked
// session or a sleeping display classifies `away` while the readable idle timer
// is still under 20 minutes, and such a start is just `now` — observation, not
// measurement. Treating the state name as the test (as the first version of the
// unobserved-transition fix did) re-dated that state on every sample, so a
// display asleep for an hour reported an away age near zero. (Codex, PR #231.)
func presenceStateStart(now time.Time, state string, s presenceSignals) (start time.Time, timerDerived bool) {
	if !s.IdleOK {
		return now, false
	}
	switch state {
	case presenceStateAway:
		if s.Idle >= presenceAwayAfter {
			return now.Add(-(s.Idle - presenceAwayAfter)), true
		}
	case presenceStateIdle:
		if s.Idle >= presenceActiveWithin {
			return now.Add(-(s.Idle - presenceActiveWithin)), true
		}
	}
	return now, false
}

// presenceTracker holds the current state and when it began, so `since` does
// not need any history of what the person did.
type presenceTracker struct {
	mu    sync.Mutex
	state string
	since time.Time
	// sinceDerived records HOW `since` was dated: by the idle timer, or
	// merely by observing the state. Without it the two kinds of start are
	// interchangeable, and a timer-derived start then overwrites an earlier
	// observed one — see observe. (Codex, PR #232.)
	sinceDerived bool
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
	start, timerDerived := presenceStateStart(now, state, s)
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.state != state || t.since.IsZero():
		t.state, t.since, t.sinceDerived = state, start, timerDerived
	case timerDerived && t.sinceDerived && start.After(t.since):
		// AN UNOBSERVED TRANSITION. Two consecutive samples can both classify
		// `idle` while the person was active in between — easily, once the
		// server configures a sample interval longer than the five-minute idle
		// threshold. The state did not change, so the old branch kept the old
		// `since` and the reported age then SPANNED that activity.
		//
		// The idle timer is evidence the state alone is not: a later derived
		// start means the current idle period began after the one on record,
		// which can only be because it was interrupted. Only ever moved
		// FORWARD, and only when the timer ACTUALLY dated this start — a start
		// that is merely `now` (an `active` sample, or an `away` read off the
		// lock or a sleeping display) is evidence of nothing, and advancing on
		// it would peg the age at zero every sample. (Codex, PR #231.)
		//
		// A START IS ONLY REPLACED BY A LATER ONE OF THE SAME KIND, which is
		// what `t.sinceDerived` is for. A signal-derived start is the earliest
		// evidence of a state that has held continuously since, so it is the
		// better lower bound and a derived start must not overwrite it: a
		// display asleep since T0 with idle under 20 minutes records T0, and
		// when that same uninterrupted idle counter crosses the threshold the
		// derived start is LATER than T0 — so without provenance the age
		// collapsed from 23 minutes to 5 at the crossing, with the state
		// unchanged throughout. (Codex, PR #232.)
		t.since = start
	case !timerDerived:
		// SIGNAL EVIDENCE OUTRANKS A LATER CROSSING, so it also has to
		// DISQUALIFY the recorded start from being replaced by one.
		//
		// The state is unchanged and this sample did not date it, which means
		// the lock or the sleeping display is still asserting it: we have
		// continuous evidence of this state from `t.since` onwards. Leaving
		// `sinceDerived` as it was let a timer-derived start replace it later,
		// and that is not merely a different estimate — it is the wrong one. A
		// lock-driven `away` begins AT THE LOCK, while the crossing falls 20
		// minutes after the keypress that preceded it, so the age collapsed to
		// roughly zero for someone who had been locked out the whole time.
		// `t.since` itself never moves here: it stays the earliest evidence we
		// have, which is the lower bound we want. (Codex, PR #232.)
		t.sinceDerived = false
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
	t.state, t.since, t.sinceDerived = "", time.Time{}, false
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
