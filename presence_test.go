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
	t.Cleanup(func() { setPresenceAllowedByOwner(previous) })
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
	start := presenceStateStart(now, presenceStateIdle, presenceSignals{Idle: 7 * time.Minute, IdleOK: true})
	if want := now.Add(-2 * time.Minute); !start.Equal(want) {
		t.Fatalf("idle start = %v, want %v", start, want)
	}

	// Idle for 25 minutes: away began 5 minutes ago.
	start = presenceStateStart(now, presenceStateAway, presenceSignals{Idle: 25 * time.Minute, IdleOK: true})
	if want := now.Add(-5 * time.Minute); !start.Equal(want) {
		t.Fatalf("away start = %v, want %v", start, want)
	}

	// Active, or a state known only from the lock signal, is dated now: the
	// real start needs a history of the person's activity, which §8.3 forbids.
	for _, c := range []struct {
		state   string
		signals presenceSignals
	}{
		{presenceStateActive, presenceSignals{Idle: time.Second, IdleOK: true}},
		{presenceStateAway, presenceSignals{Locked: true, LockedOK: true}},
		{presenceStateIdle, presenceSignals{}},
	} {
		if start := presenceStateStart(now, c.state, c.signals); !start.Equal(now) {
			t.Fatalf("%s start = %v, want %v", c.state, start, now)
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
