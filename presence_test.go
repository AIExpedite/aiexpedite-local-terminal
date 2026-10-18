// Coarse presence (PARALLEL_RUNS_AND_COMPUTER_MAINTENANCE_PLAN.md §8).
//
// The vocabulary and the two thresholds are asserted against
// testdata/device_load_report_vectors.json, the file terminal-service pins
// against @aiexpedite/shared-constants — so a threshold changed in the package
// fails there, and this test fails until the Go copy follows. Nothing here
// transcribes a value independently.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

type presenceContract struct {
	States        []string `json:"states"`
	ActiveWithin  int64    `json:"activeWithinMs"`
	AwayAfter     int64    `json:"awayAfterMs"`
	NotReportedAs string   `json:"notReported"`
}

type loadReportContract struct {
	Version           int              `json:"version"`
	Presence          presenceContract `json:"presence"`
	MemPressureLevels []string         `json:"memPressureLevels"`
	Unreadable        int64            `json:"unreadable"`
	Cadence           struct {
		SampleIntervalMs int64 `json:"sampleIntervalMs"`
		MaxIntervalMs    int64 `json:"maxIntervalMs"`
		ChangePercent    int64 `json:"changePercent"`
	} `json:"cadence"`
	Vectors []struct {
		Name      string           `json:"name"`
		Secret    string           `json:"secret"`
		AgentID   string           `json:"agentId"`
		Timestamp int64            `json:"timestamp"`
		Report    deviceLoadReport `json:"report"`
		Message   string           `json:"message"`
		Signature string           `json:"signature"`
	} `json:"vectors"`
}

func readLoadReportContract(t *testing.T) loadReportContract {
	t.Helper()
	data, err := os.ReadFile("testdata/device_load_report_vectors.json")
	if err != nil {
		t.Fatalf("read contract: %v", err)
	}
	var contract loadReportContract
	if err := json.Unmarshal(data, &contract); err != nil {
		t.Fatalf("decode contract: %v", err)
	}
	return contract
}

// withProbe installs a presence probe for one test and restores the real one.
func withProbe(t *testing.T, signals presenceSignals) *int {
	t.Helper()
	calls := 0
	previous := probePresenceSignals
	probePresenceSignals = func() presenceSignals {
		calls++
		return signals
	}
	t.Cleanup(func() { probePresenceSignals = previous })
	return &calls
}

func withPresenceEnv(t *testing.T, value string, present bool) {
	t.Helper()
	previous := presenceEnvLookup
	presenceEnvLookup = func(key string) (string, bool) {
		if key == presenceEnvVar {
			return value, present
		}
		return previous(key)
	}
	t.Cleanup(func() { presenceEnvLookup = previous })
}

func withOwnerSwitch(t *testing.T, allowed bool) {
	t.Helper()
	previous := presenceAllowedByOwner.Load()
	setPresenceAllowedByOwner(allowed)
	// Restored as a tri-state, so a test that ran with UNKNOWN is put back to
	// UNKNOWN rather than being silently promoted to allowed.
	t.Cleanup(func() { presenceAllowedByOwner.Store(previous) })
}

func TestPresenceVocabularyMatchesTheSharedContract(t *testing.T) {
	contract := readLoadReportContract(t)

	want := []string{presenceStateActive, presenceStateIdle, presenceStateAway}
	if len(contract.Presence.States) != len(want) {
		t.Fatalf("contract has %d presence states, this build knows %d", len(contract.Presence.States), len(want))
	}
	for i, state := range want {
		if contract.Presence.States[i] != state {
			t.Fatalf("presence state %d: contract %q, build %q", i, contract.Presence.States[i], state)
		}
	}
	if got := presenceActiveWithin.Milliseconds(); got != contract.Presence.ActiveWithin {
		t.Fatalf("presenceActiveWithin = %dms, contract %dms", got, contract.Presence.ActiveWithin)
	}
	if got := presenceAwayAfter.Milliseconds(); got != contract.Presence.AwayAfter {
		t.Fatalf("presenceAwayAfter = %dms, contract %dms", got, contract.Presence.AwayAfter)
	}
	if contract.Presence.NotReportedAs != presenceStateNotReported {
		t.Fatalf("not-reported marker: contract %q, build %q", contract.Presence.NotReportedAs, presenceStateNotReported)
	}
}

func TestClassifyPresence(t *testing.T) {
	cases := []struct {
		name    string
		signals presenceSignals
		want    string
	}{
		{"fresh input is active", presenceSignals{Idle: time.Minute, IdleOK: true}, presenceStateActive},
		{"input exactly at the active edge is idle", presenceSignals{Idle: presenceActiveWithin, IdleOK: true}, presenceStateIdle},
		{"between the thresholds is idle", presenceSignals{Idle: 12 * time.Minute, IdleOK: true}, presenceStateIdle},
		{"input exactly at the away edge is away", presenceSignals{Idle: presenceAwayAfter, IdleOK: true}, presenceStateAway},
		{"a long silence is away", presenceSignals{Idle: 3 * time.Hour, IdleOK: true}, presenceStateAway},
		{
			"a locked screen is away whatever the idle timer says",
			presenceSignals{Idle: time.Second, IdleOK: true, Locked: true, LockedOK: true},
			presenceStateAway,
		},
		{
			"a sleeping display is away whatever the idle timer says",
			presenceSignals{Idle: time.Second, IdleOK: true, DisplayOff: true, DisplayOffOK: true},
			presenceStateAway,
		},
		{
			"an unlocked screen does not override a long silence",
			presenceSignals{Idle: time.Hour, IdleOK: true, Locked: false, LockedOK: true},
			presenceStateAway,
		},
		{"nothing readable is active, never away", presenceSignals{}, presenceStateActive},
		{
			"an unreadable idle timer with an unlocked screen is active",
			presenceSignals{Locked: false, LockedOK: true},
			presenceStateActive,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyPresence(c.signals); got != c.want {
				t.Fatalf("classifyPresence = %q, want %q", got, c.want)
			}
		})
	}
}

func TestPresenceStateStartDatesIdleAndAwayFromTheTimer(t *testing.T) {
	now := time.Unix(1790000000, 0)

	// Idle for 7 minutes: the state began 2 minutes ago, when the timer
	// crossed the 5-minute threshold.
	start, derived := presenceStateStart(now, presenceStateIdle, presenceSignals{Idle: 7 * time.Minute, IdleOK: true})
	if want := now.Add(-2 * time.Minute); !start.Equal(want) || !derived {
		t.Fatalf("idle start = %v (derived %v), want %v derived", start, derived, want)
	}

	// Idle for 25 minutes: away began 5 minutes ago.
	start, derived = presenceStateStart(now, presenceStateAway, presenceSignals{Idle: 25 * time.Minute, IdleOK: true})
	if want := now.Add(-5 * time.Minute); !start.Equal(want) || !derived {
		t.Fatalf("away start = %v (derived %v), want %v derived", start, derived, want)
	}

	// Active, or a state known only from the lock signal, is dated now AND
	// reported as not timer-derived: the real start needs a history of the
	// person's activity, which §8.3 forbids. The second return is what keeps
	// the tracker from re-dating these on every sample.
	for _, c := range []struct {
		state   string
		signals presenceSignals
	}{
		{presenceStateActive, presenceSignals{Idle: time.Second, IdleOK: true}},
		{presenceStateAway, presenceSignals{Locked: true, LockedOK: true}},
		{presenceStateAway, presenceSignals{DisplayOff: true, DisplayOffOK: true, Idle: 3 * time.Minute, IdleOK: true}},
		{presenceStateIdle, presenceSignals{}},
	} {
		start, derived := presenceStateStart(now, c.state, c.signals)
		if !start.Equal(now) || derived {
			t.Fatalf("%s start = %v (derived %v), want %v not derived", c.state, start, derived, now)
		}
	}
}

// THE INVARIANT ALL FOUR DEFECTS BROKE, asserted directly rather than as a
// fifth special case.
//
// A state that has not changed must age by exactly the time that passed. Four
// rounds of review found four ways to break that — a signal-derived `away`
// re-dated every sample, the 20-minute crossing replacing a signal-derived
// start, the same crossing again after a lock — and each was fixed in isolation
// while the next one waited. So this walks a grid instead: every combination of
// the signals that can assert a state, started either side of both thresholds,
// sampled across the crossings.
//
// The grid has a second dimension because the first version of it MISSED round
// 4, which is worth keeping in the file. Holding the signals constant and
// letting the idle timer only climb never models the person coming BACK, and
// that is precisely what that defect needed: a timer-derived `away`, then
// input, then a lock holding the state while a fresh idle period climbs to its
// own crossing. So a sequence may reset the idle timer once mid-way. While a
// signal holds the state, input does not end it — only the state changing is
// allowed to reset the age.
//
// It deliberately asserts nothing about WHICH state is reported (§8.2 owns
// that) and nothing about how a start is dated — only that an unchanged state's
// age tracks the clock.
//
// IT COVERS ONE DIRECTION ONLY, and the pair matters. This is the net for
// re-dating a state that never stopped holding; the opposite error — NOT
// re-dating when the person was active between two samples that happen to
// classify alike — cannot be asserted here, because there a reset is the
// correct answer. Mutation says so: blanking the re-dating branch entirely
// leaves this test green and fails
// TestPresenceAgeDoesNotSpanAnUnobservedTransition, which is that direction's
// net. A change to this state machine should be mutation-checked against both.
func TestPresenceAgeTracksTheClockWhileNobodyTouchesTheMachine(t *testing.T) {
	const step = 2 * time.Minute
	t0 := time.Unix(1790000000, 0)

	// Signal shapes that can hold a state on their own, plus the plain
	// idle-timer-only case. Each is held CONSTANT across a sequence: the point
	// is a state nothing is changing.
	shapes := []struct {
		name                  string
		locked, lockedOK      bool
		displayOff, displayOK bool
	}{
		{name: "idle timer only"},
		{name: "locked", locked: true, lockedOK: true},
		{name: "display asleep", displayOff: true, displayOK: true},
		{name: "locked and display asleep", locked: true, lockedOK: true, displayOff: true, displayOK: true},
	}

	// Start either side of the 5- and 20-minute thresholds so the sequences run
	// across both crossings, which is where three of the four defects lived.
	// resetAt 0 means nobody ever touches the machine; otherwise the idle timer
	// restarts at that sample, as one keypress would restart it.
	for _, startIdle := range []time.Duration{
		0, 30 * time.Second, 4 * time.Minute, 6 * time.Minute,
		18 * time.Minute, 19 * time.Minute, 25 * time.Minute,
	} {
		for _, resetAt := range []int{0, 1, 3, 8} {
			for _, shape := range shapes {
				name := fmt.Sprintf("%s/idle=%s/reset=%d", shape.name, startIdle, resetAt)
				t.Run(name, func(t *testing.T) {
					tracker := &presenceTracker{}
					var prevState string
					var prevAge int64
					var prevAt time.Time

					for i := 0; i < 20; i++ {
						at := t0.Add(time.Duration(i) * step)
						idle := startIdle + time.Duration(i)*step
						if resetAt > 0 && i >= resetAt {
							// One keypress at `resetAt`, then nothing again.
							idle = 30*time.Second + time.Duration(i-resetAt)*step
						}
						got := tracker.observe(at, presenceSignals{
							Idle:         idle,
							IdleOK:       true,
							Locked:       shape.locked,
							LockedOK:     shape.lockedOK,
							DisplayOff:   shape.displayOff,
							DisplayOffOK: shape.displayOK,
						})
						if got.AgeMs < 0 {
							t.Fatalf("sample %d: negative age %dms", i, got.AgeMs)
						}
						if i > 0 && got.State == prevState {
							elapsed := at.Sub(prevAt).Milliseconds()
							if want := prevAge + elapsed; got.AgeMs != want {
								t.Fatalf("sample %d: %q held but aged %dms, want %dms (previous %dms + %dms elapsed) — an unchanged state was re-dated",
									i, got.State, got.AgeMs, want, prevAge, elapsed)
							}
						}
						prevState, prevAge, prevAt = got.State, got.AgeMs, at
					}
				})
			}
		}
	}
}

// The mixed case, and the one two rounds of fixes got wrong in both directions:
// an `away` dated by the idle timer, then the person returns (resetting the
// timer) and locks the screen. The lock asserts `away` from the moment it
// happens, so when the LOCK's own idle timer later crosses 20 minutes that
// crossing must not re-date the state — it falls 20 minutes after the keypress
// and would report someone locked out for half an hour as away for seconds.
// (Codex, PR #232.)
func TestPresenceLockAfterATimerDerivedAwayIsNotReDatedByTheCrossing(t *testing.T) {
	tracker := &presenceTracker{}
	t0 := time.Unix(1790000000, 0)

	// Away because the idle timer passed 20 minutes: dated by the timer, so the
	// recorded start is five minutes before this observation.
	sinceStart := t0.Add(-5 * time.Minute)
	first := tracker.observe(t0, presenceSignals{Idle: 25 * time.Minute, IdleOK: true})
	if first.State != presenceStateAway {
		t.Fatalf("first observation = %+v", first)
	}

	// The person comes back and locks the screen, which resets the idle timer.
	// Still `away` — now because of the lock — so this is not a transition, and
	// the recorded start stays where it was.
	locked := presenceSignals{Locked: true, LockedOK: true, Idle: 30 * time.Second, IdleOK: true}
	if got := tracker.observe(t0.Add(time.Minute), locked); got.State != presenceStateAway {
		t.Fatalf("after the lock = %+v", got)
	}

	// The locked session now idles past 20 minutes. The age must keep growing
	// across that crossing rather than snapping back to it.
	before := tracker.observe(t0.Add(20*time.Minute), presenceSignals{
		Locked: true, LockedOK: true, Idle: 19 * time.Minute, IdleOK: true,
	})
	after0 := t0.Add(22 * time.Minute)
	after := tracker.observe(after0, presenceSignals{
		Locked: true, LockedOK: true, Idle: 21 * time.Minute, IdleOK: true,
	})
	if after.AgeMs <= before.AgeMs {
		t.Fatalf("away age went backwards across the idle threshold: %dms then %dms",
			before.AgeMs, after.AgeMs)
	}
	// The retained start is the FIRST one — the timer-derived t0-5m, from before
	// the person came back — so the age reads 27 minutes, not the 22 since the
	// lock. That is the residual of this design and it is the SAFE direction: an
	// age that reads long only makes the platform more willing to use a computer
	// whose screen is locked, which is what `away` is for. The collapse is the
	// unsafe direction and is what this test exists to prevent. Do not "correct"
	// this figure to the age since the lock without re-reading §8.2 — dating a
	// lock-driven `away` from the idle crossing is how it collapsed.
	if want := after0.Sub(sinceStart).Milliseconds(); after.AgeMs != want {
		t.Fatalf("away age after the crossing = %dms, want %dms", after.AgeMs, want)
	}
}

// The same uninterrupted idle counter eventually crosses 20 minutes, at which
// point presenceStateStart starts dating the state from the threshold — a start
// LATER than the moment the display slept. Without provenance on the recorded
// start, that later value replaced the observed one and the reported age
// collapsed (23 minutes to 5) while the state never changed. (Codex, PR #232.)
func TestPresenceAwayAgeSurvivesTheIdleThresholdCrossing(t *testing.T) {
	tracker := &presenceTracker{}
	start := time.Unix(1790000000, 0)

	// The display sleeps with the idle timer at 2 minutes, so this `away` is
	// dated by observation: the state began now.
	first := tracker.observe(start, presenceSignals{DisplayOff: true, DisplayOffOK: true, Idle: 2 * time.Minute, IdleOK: true})
	if first.State != presenceStateAway || first.AgeMs != 0 {
		t.Fatalf("first observation = %+v", first)
	}

	// Nobody has touched the machine since, so the idle timer keeps climbing
	// past its own 20-minute threshold with the display still asleep. The age
	// has to keep growing from when the display slept.
	for _, elapsed := range []time.Duration{10 * time.Minute, 23 * time.Minute, 90 * time.Minute} {
		got := tracker.observe(start.Add(elapsed), presenceSignals{
			DisplayOff: true, DisplayOffOK: true,
			Idle:   2*time.Minute + elapsed,
			IdleOK: true,
		})
		if got.State != presenceStateAway {
			t.Fatalf("state after %v = %q", elapsed, got.State)
		}
		if want := elapsed.Milliseconds(); got.AgeMs != want {
			t.Fatalf("away age after %v = %dms, want %dms — the threshold crossing re-dated an unchanged state",
				elapsed, got.AgeMs, want)
		}
	}
}

// A sleeping display or a locked session classifies `away` while the idle timer
// is still under 20 minutes. The unobserved-transition fix first tested the
// STATE NAME for timer-derivation, so every one of those samples re-dated the
// state to `now` and the reported age never grew past one interval — a display
// asleep for an hour read as away for seconds. (Codex, PR #231.)
func TestPresenceAwayFromASignalKeepsItsAge(t *testing.T) {
	tracker := &presenceTracker{}
	start := time.Unix(1790000000, 0)
	// Idle stays well under the 20-minute threshold throughout, so every
	// `away` here comes from the display signal alone.
	signals := presenceSignals{DisplayOff: true, DisplayOffOK: true, Idle: 2 * time.Minute, IdleOK: true}

	if first := tracker.observe(start, signals); first.State != presenceStateAway || first.AgeMs != 0 {
		t.Fatalf("first observation = %+v", first)
	}
	for _, elapsed := range []time.Duration{3 * time.Minute, 10 * time.Minute, time.Hour} {
		got := tracker.observe(start.Add(elapsed), signals)
		if got.State != presenceStateAway {
			t.Fatalf("state after %v = %q", elapsed, got.State)
		}
		if want := elapsed.Milliseconds(); got.AgeMs != want {
			t.Fatalf("away age after %v = %dms, want %dms", elapsed, got.AgeMs, want)
		}
	}
}

func TestPresenceTrackerAgesTheStateAndResetsOnTransition(t *testing.T) {
	tracker := &presenceTracker{}
	start := time.Unix(1790000000, 0)

	first := tracker.observe(start, presenceSignals{Idle: time.Minute, IdleOK: true})
	if first.State != presenceStateActive || first.AgeMs != 0 {
		t.Fatalf("first observation = %+v", first)
	}

	// Same state 90 s later: the age grows, the start does not move.
	later := tracker.observe(start.Add(90*time.Second), presenceSignals{Idle: 2 * time.Minute, IdleOK: true})
	if later.State != presenceStateActive || later.AgeMs != 90_000 {
		t.Fatalf("second observation = %+v", later)
	}

	// A transition re-dates the state from the idle timer: idle for 6 minutes
	// means idle began 1 minute ago.
	transitioned := tracker.observe(start.Add(10*time.Minute), presenceSignals{Idle: 6 * time.Minute, IdleOK: true})
	if transitioned.State != presenceStateIdle {
		t.Fatalf("expected idle, got %+v", transitioned)
	}
	if transitioned.AgeMs != time.Minute.Milliseconds() {
		t.Fatalf("idle age = %dms, want %dms", transitioned.AgeMs, time.Minute.Milliseconds())
	}

	tracker.reset()
	afterReset := tracker.observe(start.Add(20*time.Minute), presenceSignals{Idle: 6 * time.Minute, IdleOK: true})
	if afterReset.AgeMs != time.Minute.Milliseconds() {
		t.Fatalf("age after reset = %dms", afterReset.AgeMs)
	}
}

func TestPresenceTrackerNeverReportsANegativeAge(t *testing.T) {
	tracker := &presenceTracker{}
	now := time.Unix(1790000000, 0)
	tracker.observe(now, presenceSignals{Idle: time.Minute, IdleOK: true})
	// A clock that jumped backwards must not produce a negative age.
	back := tracker.observe(now.Add(-time.Hour), presenceSignals{Idle: time.Minute, IdleOK: true})
	if back.AgeMs < 0 {
		t.Fatalf("age = %dms, want >= 0", back.AgeMs)
	}
}

func TestSamplePresenceReportsTheState(t *testing.T) {
	withOwnerSwitch(t, true)
	withPresenceEnv(t, "", false)
	calls := withProbe(t, presenceSignals{Idle: 30 * time.Minute, IdleOK: true})
	globalPresenceTracker.reset()
	t.Cleanup(globalPresenceTracker.reset)

	sample := samplePresence(time.Unix(1790000000, 0))

	if sample.State != presenceStateAway {
		t.Fatalf("state = %q, want %q", sample.State, presenceStateAway)
	}
	if sample.AgeMs != (10 * time.Minute).Milliseconds() {
		t.Fatalf("age = %dms", sample.AgeMs)
	}
	if *calls != 1 {
		t.Fatalf("probe called %d times, want 1", *calls)
	}
}

// The off switch must stop the SAMPLING, not merely the reporting: the point
// of D12 is that the device stops reading the person's activity.
func TestSamplePresenceDoesNotProbeWhenTheOwnerSwitchedItOff(t *testing.T) {
	withPresenceEnv(t, "", false)
	withOwnerSwitch(t, false)
	calls := withProbe(t, presenceSignals{Idle: time.Hour, IdleOK: true})

	sample := samplePresence(time.Unix(1790000000, 0))

	if sample.State != presenceStateNotReported || sample.AgeMs != loadMetricUnreadable {
		t.Fatalf("sample = %+v, want not reported", sample)
	}
	if *calls != 0 {
		t.Fatalf("probe called %d times with presence off, want 0", *calls)
	}
}

func TestSamplePresenceDoesNotProbeWhenDisabledOnThisMachine(t *testing.T) {
	withOwnerSwitch(t, true)
	for _, value := range []string{"off", "0", "false", "no", "OFF", " off "} {
		t.Run(value, func(t *testing.T) {
			withPresenceEnv(t, value, true)
			calls := withProbe(t, presenceSignals{Idle: time.Hour, IdleOK: true})

			sample := samplePresence(time.Unix(1790000000, 0))

			if sample.State != presenceStateNotReported {
				t.Fatalf("sample = %+v, want not reported", sample)
			}
			if *calls != 0 {
				t.Fatalf("probe called %d times, want 0", *calls)
			}
		})
	}
}

func TestSamplePresenceKeepsSamplingForAnUnrelatedEnvValue(t *testing.T) {
	withOwnerSwitch(t, true)
	withPresenceEnv(t, "on", true)
	calls := withProbe(t, presenceSignals{Idle: time.Minute, IdleOK: true})
	globalPresenceTracker.reset()
	t.Cleanup(globalPresenceTracker.reset)

	if sample := samplePresence(time.Unix(1790000000, 0)); sample.State != presenceStateActive {
		t.Fatalf("sample = %+v, want active", sample)
	}
	if *calls != 1 {
		t.Fatalf("probe called %d times, want 1", *calls)
	}
}

// An OS that can read nothing reports NOTHING rather than a confident
// `active`: the server cannot otherwise tell a measurement from a guess, and
// its reading of "nothing" is `active` anyway.
func TestSamplePresenceReportsNothingWhenNoSignalIsReadable(t *testing.T) {
	withOwnerSwitch(t, true)
	withPresenceEnv(t, "", false)
	withProbe(t, presenceSignals{})

	sample := samplePresence(time.Unix(1790000000, 0))

	if sample.State != presenceStateNotReported || sample.AgeMs != loadMetricUnreadable {
		t.Fatalf("sample = %+v, want not reported", sample)
	}
}

func TestSamplePresenceReportsAStateKnownOnlyFromTheLockSignal(t *testing.T) {
	withOwnerSwitch(t, true)
	withPresenceEnv(t, "", false)
	withProbe(t, presenceSignals{Locked: true, LockedOK: true})
	globalPresenceTracker.reset()
	t.Cleanup(globalPresenceTracker.reset)

	if sample := samplePresence(time.Unix(1790000000, 0)); sample.State != presenceStateAway {
		t.Fatalf("sample = %+v, want away", sample)
	}
}

func TestPresenceDisabledLocally(t *testing.T) {
	withPresenceEnv(t, "", false)
	if presenceDisabledLocally() {
		t.Fatal("an unset variable must not disable presence")
	}
	withPresenceEnv(t, "", true)
	if presenceDisabledLocally() {
		t.Fatal("an empty value must not disable presence")
	}
}

// ─── an opt-out survives a restart ──────────────────────────────────────────

// The defect this pins: the owner's switch arrives only in a load-report
// RESPONSE, so a process-local flag defaulting to "allowed" meant a computer
// whose owner had turned presence OFF probed and transmitted it on the first
// report after every start — and on every retry if that response was lost. An
// opt-out a restart undoes is not an opt-out.
func TestPresenceWithheldUntilTheOwnerSwitchIsKnown(t *testing.T) {
	isolateMachineInfo(t)
	previous := presenceAllowedByOwner.Load()
	t.Cleanup(func() { presenceAllowedByOwner.Store(previous) })

	// A fresh process, nothing persisted: UNKNOWN, and unknown WITHHOLDS.
	presenceAllowedByOwner.Store(presenceOwnerUnknown)
	_ = os.Remove(presenceOwnerStatePath())
	if presenceSamplingEnabled() {
		t.Fatal("presence was sampled before the owner's switch was known")
	}

	// The server says off. That has to survive the next start.
	setPresenceAllowedByOwner(false)
	if presenceSamplingEnabled() {
		t.Fatal("an explicit off did not stop sampling")
	}
	presenceAllowedByOwner.Store(presenceOwnerUnknown) // the restart
	loadPresenceOwnerState()
	if presenceAllowedByOwner.Load() != presenceOwnerDenied {
		t.Fatal("the owner's off switch did not survive a restart")
	}
	if presenceSamplingEnabled() {
		t.Fatal("a restart undid the owner's opt-out")
	}

	// An explicit ON opens the probe now...
	setPresenceAllowedByOwner(true)
	if !presenceSamplingEnabled() {
		t.Fatal("presence stayed withheld after the owner allowed it")
	}

	// ...and is deliberately NOT remembered. Remembering it would authorise the
	// first probe after a restart, and an owner who turned presence off while
	// this agent was stopped would be sampled anyway — the very hole the
	// tri-state closes. One withheld interval per restart is the price.
	// (Codex, PR #231.)
	//
	// The WRITER is pinned separately from the reader below: the reader ignores
	// the word `allowed` outright, so asserting only the restored state leaves a
	// regression in the writer invisible. Both layers have to fail loudly.
	if raw, err := os.ReadFile(presenceOwnerStatePath()); err == nil {
		t.Fatalf("an opt-in wrote %q to disk; only a denial may be persisted", raw)
	}
	presenceAllowedByOwner.Store(presenceOwnerUnknown) // the restart
	loadPresenceOwnerState()
	if presenceAllowedByOwner.Load() != presenceOwnerUnknown {
		t.Fatal("an opt-in was remembered across a restart")
	}
	if presenceSamplingEnabled() {
		t.Fatal("a remembered opt-in authorised the first probe after a restart")
	}

	// Turning it on also CLEARS a remembered denial, so an opt-out followed by
	// an opt-in does not withhold for an interval after every later restart.
	setPresenceAllowedByOwner(false)
	setPresenceAllowedByOwner(true)
	presenceAllowedByOwner.Store(presenceOwnerUnknown)
	loadPresenceOwnerState()
	if presenceAllowedByOwner.Load() == presenceOwnerDenied {
		t.Fatal("an opt-in left the earlier denial on disk")
	}
}

// A corrupt or truncated state file must not read as permission.
func TestPresenceOwnerStateFailsSafeOnGarbage(t *testing.T) {
	isolateMachineInfo(t)
	previous := presenceAllowedByOwner.Load()
	t.Cleanup(func() { presenceAllowedByOwner.Store(previous) })

	// `allowed` is in this list on purpose: an earlier build of this agent wrote
	// it, and such a file must now read as UNKNOWN rather than as permission.
	for _, garbage := range []string{"", "yes", "true", "1", "allowed", "ALLOWED-ish", "\x00\x01"} {
		presenceAllowedByOwner.Store(presenceOwnerUnknown)
		if err := os.MkdirAll(GetConfigDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(presenceOwnerStatePath(), []byte(garbage), 0o600); err != nil {
			t.Fatal(err)
		}
		loadPresenceOwnerState()
		if presenceAllowedByOwner.Load() != presenceOwnerUnknown {
			t.Fatalf("%q was read as a decision", garbage)
		}
		if presenceSamplingEnabled() {
			t.Fatalf("%q opened the presence probe", garbage)
		}
	}
}

// Two consecutive samples can both classify `idle` while the person was active
// in between — easily, once the server sets a sample interval longer than the
// five-minute idle threshold. The state does not change, so a tracker keyed only
// on state keeps the old `since` and the reported age SPANS that activity.
func TestPresenceAgeDoesNotSpanAnUnobservedTransition(t *testing.T) {
	withOwnerSwitch(t, true)
	tracker := &presenceTracker{}
	base := time.Unix(1_700_000_000, 0)

	// First sample: idle for 15 minutes — past the 5 min idle threshold but
	// short of the 20 min away threshold — so the idle period began 10 minutes
	// ago (15 min idle − the 5 min threshold).
	first := tracker.observe(base, presenceSignals{IdleOK: true, Idle: 15 * time.Minute})
	if first.State != presenceStateIdle {
		t.Fatalf("state = %q, want idle", first.State)
	}
	if got := time.Duration(first.AgeMs) * time.Millisecond; got != 10*time.Minute {
		t.Fatalf("age = %v, want 10m", got)
	}

	// An hour later, idle for only 6 minutes: the person clearly came back and
	// left again. The state is still `idle`, and the honest age is 1 minute —
	// not the 70 minutes a state-only tracker would report.
	second := tracker.observe(base.Add(time.Hour), presenceSignals{IdleOK: true, Idle: 6 * time.Minute})
	if second.State != presenceStateIdle {
		t.Fatalf("state = %q, want idle", second.State)
	}
	if got := time.Duration(second.AgeMs) * time.Millisecond; got != time.Minute {
		t.Fatalf("age = %v, want 1m — the age spanned the intervening activity", got)
	}
}

// `since` only ever moves FORWARD. A longer idle reading within the same
// uninterrupted period must not drag the start backwards.
func TestPresenceAgeNeverMovesBackwards(t *testing.T) {
	withOwnerSwitch(t, true)
	tracker := &presenceTracker{}
	base := time.Unix(1_700_000_000, 0)

	tracker.observe(base, presenceSignals{IdleOK: true, Idle: 6 * time.Minute})
	// Ten minutes later the same period has simply continued: idle is now 16
	// minutes, whose derived start is EARLIER than the one on record.
	later := tracker.observe(base.Add(10*time.Minute), presenceSignals{IdleOK: true, Idle: 16 * time.Minute})
	if got := time.Duration(later.AgeMs) * time.Millisecond; got != 11*time.Minute {
		t.Fatalf("age = %v, want 11m (1m at first sample + 10m elapsed)", got)
	}
}

// An `active` observation must not advance on its derived start, which is just
// `now` — that would peg the age at zero on every sample.
func TestPresenceActiveAgeAccumulates(t *testing.T) {
	withOwnerSwitch(t, true)
	tracker := &presenceTracker{}
	base := time.Unix(1_700_000_000, 0)

	tracker.observe(base, presenceSignals{IdleOK: true, Idle: time.Second})
	later := tracker.observe(base.Add(3*time.Minute), presenceSignals{IdleOK: true, Idle: 2 * time.Second})
	if later.State != presenceStateActive {
		t.Fatalf("state = %q, want active", later.State)
	}
	if got := time.Duration(later.AgeMs) * time.Millisecond; got != 3*time.Minute {
		t.Fatalf("age = %v, want 3m — an active age must accumulate", got)
	}
}
