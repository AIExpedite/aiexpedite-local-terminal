package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// museRunUsageRecord is the `run.usage` record Muse Code 1.4.14-R7309.1 printed
// on AIE2 on 2026-11-08, for a run Meta refused with 429 "Subscription quota
// exhausted" (ids shortened, figures and times verbatim).
const museRunUsageRecord = `{"schema_version":1,"id":"019e8ff1-0d40-7207-9d4b-99a9f41aa809","stream":{"kind":"exec.usage","id":"b8601254-ef19-4196-a45a-f140a55ec7cb"},"sequence":1,"recorded_at":1780531400000040,"record_type":"status","durability":"ephemeral","causation_id":"01a1bbc1","payload_type":"run.usage","payload_schema_version":1,"payload":{"kind":"run_usage","command_id":"01a1bbc1","run_stream":{"kind":"run","id":"01a1bbc1"},"models":[],"calls":1,"completed_calls":0,"failed_calls":1,"cancelled_calls":0,"children_included":false,"allowance":{"tier":"Muse Code Power Usage","window":{"used_percent":0,"resets_at_ms":1794169407000},"weekly":{"used_percent":99,"resets_at_ms":1794182400000},"as_of_ms":1794151407390,"source":"mint"}}}`

// museRunUsageAsOf is that record's as_of_ms.
var museRunUsageAsOf = time.UnixMilli(1794151407390)

// museRunUsageWith is the record with other figures and observation time.
func museRunUsageWith(windowPct, weeklyPct int, asOf time.Time) string {
	r := strings.Replace(museRunUsageRecord, `"used_percent":0,`, `"used_percent":`+strconv.Itoa(windowPct)+`,`, 1)
	r = strings.Replace(r, `"used_percent":99,`, `"used_percent":`+strconv.Itoa(weeklyPct)+`,`, 1)
	return strings.Replace(r, `"as_of_ms":1794151407390`, `"as_of_ms":`+strconv.FormatInt(asOf.UnixMilli(), 10), 1)
}

// isolateMuseCodeHome is isolateMuseCode with the home folder pointed at the
// same temp dir, so the account a capture keys its reading to (the CURRENT
// login, read from the home folder) is the one the parser reads for.
func isolateMuseCodeHome(t *testing.T) (home string) {
	t.Helper()
	home = isolateMuseCode(t)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	return home
}

func TestParseMuseCodeRunUsageLine_TheRecordMusePrints(t *testing.T) {
	r, ok := parseMuseCodeRunUsageLine(museRunUsageRecord, museRunUsageAsOf.Add(time.Second))
	if !ok {
		t.Fatal("the real run.usage record must parse")
	}
	if r.ObservedAtMs != 1794151407390 || r.Window.UsedPercent != 0 || r.Window.ResetsAtMs != 1794169407000 ||
		r.Weekly.UsedPercent != 99 || r.Weekly.ResetsAtMs != 1794182400000 {
		t.Fatalf("reading=%#v", r)
	}
	if r.Window.WindowDurationMins != 0 {
		t.Fatalf("the record names no window length; got %d", r.Window.WindowDurationMins)
	}
}

func TestParseMuseCodeRunUsageLine_RejectsAnythingIncomplete(t *testing.T) {
	now := museRunUsageAsOf
	for name, line := range map[string]string{
		"not json":           "run ended with Failed: API error 429 run.usage allowance",
		"another record":     strings.Replace(museRunUsageRecord, `"payload_type":"run.usage"`, `"payload_type":"run.terminal.failed"`, 1),
		"no allowance":       `{"payload_type":"run.usage","payload":{"kind":"run_usage","calls":1},"note":"allowance"}`,
		"no weekly figure":   strings.Replace(museRunUsageRecord, `"used_percent":99,`, ``, 1),
		"no window figure":   strings.Replace(museRunUsageRecord, `"used_percent":0,`, ``, 1),
		"negative figure":    museRunUsageWith(-1, 99, now),
		"no weekly reset":    strings.Replace(museRunUsageRecord, `"resets_at_ms":1794182400000`, `"resets_at_ms":0`, 1),
		"no window reset":    strings.Replace(museRunUsageRecord, `"resets_at_ms":1794169407000`, `"resets_at_ms":0`, 1),
		"truncated":          museRunUsageRecord[:len(museRunUsageRecord)-20],
		"stderr text":        "muse: warning: run.usage allowance unavailable",
		"delta naming usage": `{"payload_type":"run.output.delta","payload":{"text":"run.usage allowance"}}`,
	} {
		if r, ok := parseMuseCodeRunUsageLine(line, now); ok {
			t.Errorf("%s: parsed %#v", name, r)
		}
	}
}

func TestParseMuseCodeRunUsageLine_FutureObservationIsReadAsNow(t *testing.T) {
	now := museRunUsageAsOf.Add(-time.Hour) // the host clock is an hour behind the record
	r, ok := parseMuseCodeRunUsageLine(museRunUsageRecord, now)
	if !ok || r.ObservedAtMs != now.UnixMilli() {
		t.Fatalf("a record from the future must not outrank later readings: ok=%v observed=%d", ok, r.ObservedAtMs)
	}
	// Inside the skew it is kept as written.
	r, _ = parseMuseCodeRunUsageLine(museRunUsageRecord, museRunUsageAsOf.Add(-30*time.Second))
	if r.ObservedAtMs != museRunUsageAsOf.UnixMilli() {
		t.Fatalf("observed=%d", r.ObservedAtMs)
	}
}

// The card a refused run leaves behind: the figures Meta sent with the refusal,
// not a reading from days before.
func TestCaptureMuseCodeRunUsage_TheCardShowsTheRunsAllowance(t *testing.T) {
	home := isolateMuseCodeHome(t)
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	now := museRunUsageAsOf.Add(2 * time.Second)
	if !newMuseCodeRunUsageCapture().Line(museRunUsageRecord, now) {
		t.Fatal("capture must keep the run's reading")
	}
	usage, _ := museCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if len(usage.Metrics) != 2 {
		t.Fatalf("metrics=%#v", usage.Metrics)
	}
	session, weekly := usage.Metrics[0], usage.Metrics[1]
	if session.Unknown || *session.Consumed != 0 || session.Label != "5-hour session window" {
		t.Fatalf("session=%#v", session)
	}
	if weekly.Unknown || *weekly.Consumed != 99 || *weekly.Remaining != 1 ||
		weekly.ResetAt != "2026-11-09T00:00:00Z" {
		t.Fatalf("weekly=%#v", weekly)
	}
	if session.ObservedAt == "" || weekly.ObservedAt != observedAtRFC3339(museRunUsageAsOf.UnixMilli()) {
		t.Fatalf("observedAt=%q / %q", session.ObservedAt, weekly.ObservedAt)
	}
	if usage.DataSource != "muse usage/read" {
		t.Fatalf("dataSource=%q", usage.DataSource)
	}
}

// A reading is the signed-in account's: another account's run reading is
// never shown, and a keychain login (no file) keys it to the device.
func TestCaptureMuseCodeRunUsage_IsKeyedToTheAccountMuseRunsAs(t *testing.T) {
	home := isolateMuseCodeHome(t)
	now := museRunUsageAsOf.Add(time.Second)
	if !newMuseCodeRunUsageCapture().Line(museRunUsageRecord, now) {
		t.Fatal("capture")
	}
	if _, ok := loadMuseCodeUsageLive(museCodeUnknownAccountKey); !ok {
		t.Fatal("a keychain login's reading must be kept under the device key")
	}
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	usage, _ := museCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !usage.Metrics[0].Unknown || !usage.Metrics[1].Unknown {
		t.Fatalf("a reading taken before this account signed in was shown: %#v", usage.Metrics)
	}
}

func TestCaptureMuseCodeRunUsage_NeverReplacesANewerReading(t *testing.T) {
	home := isolateMuseCodeHome(t)
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	key := museCodeAccountFingerprint("dev@example.com")
	base := museRunUsageAsOf

	// A Refresh probe reading, 10 minutes after the run, with a 4-hour window.
	probe, ok := parseMuseCodeSubscriptionUsage(museUsageJSON(base.Add(3*time.Hour), base.Add(24*time.Hour), base.Add(10*time.Minute)))
	if !ok {
		t.Fatal("fixture")
	}
	probe.Window.WindowDurationMins = 240
	probe.AccountKey = key
	if !saveMuseCodeUsageLive(probe) {
		t.Fatal("save")
	}

	// The older run's record arrives late: kept out.
	if newMuseCodeRunUsageCapture().Line(museRunUsageRecord, base.Add(11*time.Minute)) {
		t.Fatal("an older run reading replaced a newer probe reading")
	}
	got, _ := loadMuseCodeUsageLive(key)
	if got.ObservedAtMs != probe.ObservedAtMs || got.Weekly.UsedPercent != 140 {
		t.Fatalf("kept=%#v", got)
	}
	// The same instant is not newer either.
	if newMuseCodeRunUsageCapture().Line(museRunUsageWith(5, 50, base.Add(10*time.Minute)), base.Add(11*time.Minute)) {
		t.Fatal("a reading observed at the same instant replaced the stored one")
	}

	// A later run replaces it, and keeps the window length the probe learned.
	if !newMuseCodeRunUsageCapture().Line(museRunUsageWith(7, 100, base.Add(20*time.Minute)), base.Add(21*time.Minute)) {
		t.Fatal("a newer run reading must replace the probe's")
	}
	got, _ = loadMuseCodeUsageLive(key)
	if got.Window.UsedPercent != 7 || got.Weekly.UsedPercent != 100 || got.Window.WindowDurationMins != 240 {
		t.Fatalf("after the newer run: %#v", got)
	}
}

func TestCaptureMuseCodeRunUsage_UnknownWindowLengthDefaultsToFiveHours(t *testing.T) {
	isolateMuseCodeHome(t)
	if !newMuseCodeRunUsageCapture().Line(museRunUsageRecord, museRunUsageAsOf.Add(time.Second)) {
		t.Fatal("capture")
	}
	got, _ := loadMuseCodeUsageLive(museCodeUnknownAccountKey)
	if got.Window.WindowDurationMins != museCodeDefaultWindowMins {
		t.Fatalf("window=%d", got.Window.WindowDurationMins)
	}
	// The cached shape is what the probe writes, so the cache stays readable.
	if _, ok := parseMuseCodeSubscriptionUsage(mustJSON(t, got)); !ok {
		t.Fatalf("the saved reading does not satisfy the probe's own rule: %#v", got)
	}
}

// The chat manager hands every line of every run to ObserveRun: a run's stdout
// holds dozens of records, and the allowance among them lands on the card.
// ObserveTurn, handed only the finished turn's bounded prefix and nothing of
// the account the run was launched with, records none.
func TestMuseCodeNativeObserveRun_KeepsTheRunsAllowance(t *testing.T) {
	isolateMuseCodeHome(t)
	lines := []string{
		`{"payload_type":"runtime.command.accepted","payload":{}}`,
		`{"payload_type":"task.lifecycle.failed","payload":{"event":{"reason":"API error 429: Subscription quota exhausted."}}}`,
		museRunUsageRecord,
		`{"payload_type":"run.terminal.failed","payload":{"terminal":"failed","reason":"API error 429"}}`,
	}
	museCodeNativeSpec.ObserveTurn(strings.Join(lines, "\n"), "run ended with Failed: API error 429")
	if r, ok := loadMuseCodeUsageLive(museCodeUnknownAccountKey); ok {
		t.Fatalf("ObserveTurn filed a reading: %#v", r)
	}
	observe := museCodeNativeSpec.ObserveRun()
	for _, line := range lines {
		observe(line)
	}
	got, ok := loadMuseCodeUsageLive(museCodeUnknownAccountKey)
	if !ok || got.Weekly.UsedPercent != 99 || got.ObservedAtMs != museRunUsageAsOf.UnixMilli() {
		t.Fatalf("ok=%v reading=%#v", ok, got)
	}
}

// A run's reading is filed under the account it was launched with, and only
// while that account is still signed in: after a switch it is saved for
// neither account (the old one is no longer whoever Muse runs as, the new one
// never made the request).
func TestMuseCodeRunUsageCapture_IsPinnedToTheLaunchAccount(t *testing.T) {
	home := isolateMuseCodeHome(t)
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	launched := newMuseCodeRunUsageCapture()
	writeMuseConfig(t, home, "auth.json", museOtherAuthFixture)
	if launched.Line(museRunUsageRecord, museRunUsageAsOf.Add(time.Second)) {
		t.Fatal("a run launched before a login switch saved its reading")
	}
	if readings := museRunReadings(); len(readings) != 0 {
		t.Fatalf("saved %#v", readings)
	}
	// The same capture saves again once its account is back.
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	if !launched.Line(museRunUsageRecord, museRunUsageAsOf.Add(time.Second)) {
		t.Fatal("the launch account's reading must be kept while it is signed in")
	}
	if _, ok := loadMuseCodeUsageLive(museDevAccountKey()); !ok {
		t.Fatal("the reading must be filed under the launch account")
	}
}

// Every session that is not a Muse run carries a nil capture.
func TestMuseCodeRunUsageCapture_NilRecordsNothing(t *testing.T) {
	isolateMuseCodeHome(t)
	var none *museCodeRunUsageCapture
	if none.Line(museRunUsageRecord, museRunUsageAsOf.Add(time.Second)) {
		t.Fatal("a nil capture reported a save")
	}
	if readings := museRunReadings(); len(readings) != 0 {
		t.Fatalf("a nil capture saved %#v", readings)
	}
}

// With a run's reading fresh, a Refresh click keeps it instead of spending a
// turn of the user's quota on the probe.
func TestCaptureMuseCodeRunUsage_ARefreshRightAfterARunSpendsNothing(t *testing.T) {
	isolateMuseCodeHome(t)
	now := time.Now()
	if !newMuseCodeRunUsageCapture().Line(museRunUsageWith(3, 40, now), now) {
		t.Fatal("capture")
	}
	started := false
	startMuseCodeMSPFn = func(ctx context.Context, launcher string, onNotify func(string, json.RawMessage)) (*museCodeMSPClient, error) {
		started = true
		return nil, context.Canceled
	}
	if got := probeMuseCodeUsageLive(context.Background(), detectedCLIAgent{Detected: true, Path: "/x/muse"}, time.Now); got != liveProbeOutcomeCooldown {
		t.Fatalf("outcome=%q", got)
	}
	if started {
		t.Fatal("the probe started a host (and would spend a turn) right after a run reported the quota")
	}
}

// End to end through the stub `muse` and the real chat manager: a turn Meta
// refused (429, exit 1) still leaves its allowance on the card.
func TestMuseCodeChatTurn_ARefusedTurnStillUpdatesTheCard(t *testing.T) {
	home := isolateMuseCodeHome(t)
	m := installMuseCodeStub(t)
	startMuse(t, m, "s", "")
	refused := `{"payload_type":"run.terminal.failed","payload":{"kind":"run_terminal","terminal":"failed","text":"","reason":"API error 429: Subscription quota exhausted. Your usage window resets at 2026-11-09T00:00:00Z. (rate_limit_error)"}}`
	t.Setenv("OPENCODE_STUB_STDOUT", museStubStdout(museRunUsageRecord, refused))
	t.Setenv("OPENCODE_STUB_EXIT", "1")
	_ = m.Send("s", "reply with hello", (&frameSink{}).publish, time.Minute)

	usage, _ := museCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, museRunUsageAsOf.Add(time.Minute))
	weekly := usage.Metrics[1]
	if weekly.Unknown || *weekly.Consumed != 99 || weekly.ResetAt != "2026-11-09T00:00:00Z" {
		t.Fatalf("the refused turn's allowance did not reach the card: %#v", usage.Metrics)
	}
}
