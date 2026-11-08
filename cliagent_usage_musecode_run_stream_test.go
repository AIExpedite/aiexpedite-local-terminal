package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_musecode_run_stream_test.go — a Muse run's allowance record,
   end to end through the stub `muse` on both launch paths (the Chat manager
   and a session_start run): it is read however much the run prints before
   it, and filed only under the account the run was launched with.
   ------------------------------------------------------------------------ */

// museOtherAuthFixture is museAuthFixture signed in as another account.
var museOtherAuthFixture = strings.Replace(museAuthFixture, "Dev@Example.com", "Other@Example.com", 1)

func museDevAccountKey() string   { return museCodeAccountFingerprint("dev@example.com") }
func museOtherAccountKey() string { return museCodeAccountFingerprint("other@example.com") }

// museToolHeavyRunStdout is a run that prints more tool output than the Chat
// manager keeps as its diagnostic prefix (oneShotNativeMaxRawStdout) before
// its allowance record and its completion: the shape of a tool-heavy turn.
func museToolHeavyRunStdout(usageRecord string) string {
	output := strings.Repeat("PASS: TestRunsTheSuite (0.01s) ", 36)
	var lines []string
	for before := 0; before <= oneShotNativeMaxRawStdout+16*1024; {
		line := museRecord("tool.result", fmt.Sprintf(
			`{"kind":"tool_result","call_id":"call_%04d","text":%s,"correlation_facts":{"tool_name":"powershell","outcome":"success"}}`,
			len(lines), museJSONString(output)))
		lines = append(lines, line)
		before += len(line) + 1
	}
	lines = append(lines, usageRecord, museCompletedRecord("All tests pass."))
	return strings.Join(lines, "\n") + "\n"
}

// useMuseStubStdoutFile has the stub print stdout verbatim from a file: more
// than one environment variable can carry on Windows.
func useMuseStubStdoutFile(t *testing.T, stdout string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdout.jsonl")
	if err := os.WriteFile(path, []byte(stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE_STUB_STDOUT_FILE", path)
}

// museStubGate holds the stub's stdout until the test opens it, so a test can
// sign another account in between the spawn and the run's records.
type museStubGate struct{ runLog, release string }

func holdMuseStubOutput(t *testing.T) museStubGate {
	t.Helper()
	dir := t.TempDir()
	g := museStubGate{runLog: filepath.Join(dir, "run.log"), release: filepath.Join(dir, "release")}
	t.Setenv("OPENCODE_STUB_RUN_LOG", g.runLog)
	t.Setenv("OPENCODE_STUB_WAIT_FILE", g.release)
	return g
}

// waitSpawned returns once the stub's child is running (its first act is to
// append to the run log).
func (g museStubGate) waitSpawned(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(g.runLog); err == nil {
			return
		}
	}
	t.Fatal("the stub child never started")
}

// open lets the held child print. Idempotent, so a cleanup can call it too.
func (g museStubGate) open() { _ = os.WriteFile(g.release, []byte("go"), 0o600) }

// museRunReadings is every reading in the usage cache, whatever its account.
func museRunReadings() []museCodeUsageLiveCache {
	museCodeUsageLiveCacheMu.Lock()
	defer museCodeUsageLiveCacheMu.Unlock()
	return readMuseCodeUsageLiveFileLocked().Readings
}

// A tool-heavy Chat turn prints more than the manager's bounded diagnostic
// prefix before its run.usage record. The record is still read, so the turn
// updates the card rather than leaving it stale for a Refresh to spend
// another turn on.
func TestMuseCodeChatTurn_ATurnLongerThanTheDiagnosticPrefixStillUpdatesTheCard(t *testing.T) {
	home := isolateMuseCodeHome(t)
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	m := installMuseCodeStub(t)
	startMuse(t, m, "s", "")
	stdout := museToolHeavyRunStdout(museRunUsageRecord)
	if at := strings.Index(stdout, museRunUsageRecord); at <= oneShotNativeMaxRawStdout {
		t.Fatalf("fixture: the record starts at byte %d, inside the %d-byte prefix", at, oneShotNativeMaxRawStdout)
	}
	useMuseStubStdoutFile(t, stdout)

	sink := &frameSink{}
	if err := m.Send("s", "run the tests", sink.publish, time.Minute); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if done, ok := sink.completion(); !ok || !strings.Contains(done.Output, "All tests pass.") {
		t.Fatalf("the turn did not complete: errors=%#v", sink.ofType("musecode_native_error"))
	}
	got, ok := loadMuseCodeUsageLive(museDevAccountKey())
	if !ok || got.Window.UsedPercent != 0 || got.Weekly.UsedPercent != 99 {
		t.Fatalf("the allowance after %d bytes of output did not reach the card: ok=%v reading=%#v",
			strings.Index(stdout, museRunUsageRecord), ok, got)
	}
	usage, _ := museCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, museRunUsageAsOf.Add(time.Minute))
	if weekly := usage.Metrics[1]; weekly.Unknown || *weekly.Consumed != 99 {
		t.Fatalf("card=%#v", usage.Metrics)
	}
}

// A `muse login` as another account while a Chat turn runs leaves the turn's
// reading with no account it can be filed under: the old account is no longer
// whoever Muse runs as, and the new one did not make the request. Nothing is
// saved for either, so the new account's card shows no borrowed quota and its
// Refresh is not held off. The next turn, launched as the new account, files
// its own reading.
func TestMuseCodeChatTurn_ALoginSwitchMidTurnSavesTheReadingForNeitherAccount(t *testing.T) {
	home := isolateMuseCodeHome(t)
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	m := installMuseCodeStub(t)
	startMuse(t, m, "s", "")
	gate := holdMuseStubOutput(t)
	t.Setenv("OPENCODE_STUB_STDOUT", museStubStdout(museRunUsageRecord, museCompletedRecord("hello")))

	var sendErr error
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		sendErr = m.Send("s", "reply with hello", (&frameSink{}).publish, time.Minute)
	}()
	// Never leave the child waiting, nor the turn running, past a failed test.
	t.Cleanup(func() { gate.open(); <-finished })

	gate.waitSpawned(t) // launched as dev@example.com
	writeMuseConfig(t, home, "auth.json", museOtherAuthFixture)
	gate.open()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("the turn never ended")
	}
	if sendErr != nil {
		t.Fatalf("Send: %v", sendErr)
	}

	if readings := museRunReadings(); len(readings) != 0 {
		t.Fatalf("a reading from a turn launched as one account and read after a switch was saved: %#v", readings)
	}
	usage, _ := museCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, museRunUsageAsOf.Add(time.Minute))
	if !usage.Metrics[0].Unknown || !usage.Metrics[1].Unknown {
		t.Fatalf("the new account's card shows a quota it never read: %#v", usage.Metrics)
	}
	started := false
	startMuseCodeMSPFn = func(ctx context.Context, launcher string, onNotify func(string, json.RawMessage)) (*museCodeMSPClient, error) {
		started = true
		return nil, context.Canceled
	}
	if outcome := probeMuseCodeUsageLive(context.Background(), detectedCLIAgent{Detected: true, Path: "/x/muse"}, time.Now); !started {
		t.Fatalf("the new account's Refresh was held off (%q) by a reading that is not its own", outcome)
	}

	// The control: a turn launched as the new account files its reading.
	t.Setenv("OPENCODE_STUB_STDOUT", museStubStdout(museRunUsageWith(10, 50, museRunUsageAsOf.Add(time.Minute)), museCompletedRecord("hello")))
	if err := m.Send("s", "reply with hello", (&frameSink{}).publish, time.Minute); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got, ok := loadMuseCodeUsageLive(museOtherAccountKey())
	if !ok || got.Window.UsedPercent != 10 || got.Weekly.UsedPercent != 50 {
		t.Fatalf("a turn launched as the new account must file its reading: ok=%v reading=%#v", ok, got)
	}
	if _, ok := loadMuseCodeUsageLive(museDevAccountKey()); ok {
		t.Fatal("the new account's reading was filed under the old account")
	}
}

// startMuseStubSession starts a session_start `muse` run against the stub and
// returns the channel closed when its session_ended frame is published (after
// its whole stdout has been read).
func startMuseStubSession(t *testing.T) <-chan struct{} {
	t.Helper()
	ended, publishFn := openCodeSessionEndedSignal()
	id := fmt.Sprintf("muse-usage-%d", time.Now().UnixNano())
	if err := NewSessionManager(nil).StartSession(id, "muse", []string{"run the tests"},
		t.TempDir(), "ws", "uid", 30000, false, publishFn); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	return ended
}

func waitMuseSessionEnded(t *testing.T, ended <-chan struct{}) {
	t.Helper()
	select {
	case <-ended:
	case <-time.After(30 * time.Second):
		t.Fatal("the session never ended")
	}
}

// The session_start path reads a long run's record the same way, under the
// account the child was spawned with.
func TestMuseCodeSession_ALongRunsAllowanceIsFiledUnderTheAccountItWasSpawnedWith(t *testing.T) {
	home := isolateMuseCodeHome(t)
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	installMuseCodeStub(t)
	useMuseStubStdoutFile(t, museToolHeavyRunStdout(museRunUsageRecord))

	waitMuseSessionEnded(t, startMuseStubSession(t))
	got, ok := loadMuseCodeUsageLive(museDevAccountKey())
	if !ok || got.Window.UsedPercent != 0 || got.Weekly.UsedPercent != 99 {
		t.Fatalf("the session run's allowance did not reach the card: ok=%v reading=%#v", ok, got)
	}
}

// A login switch while a session_start run is going saves its reading for
// neither account, as on the Chat path; a run spawned after the switch files
// its own.
func TestMuseCodeSession_ALoginSwitchMidRunSavesTheReadingForNeitherAccount(t *testing.T) {
	home := isolateMuseCodeHome(t)
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	installMuseCodeStub(t)
	gate := holdMuseStubOutput(t)
	t.Setenv("OPENCODE_STUB_STDOUT", museStubStdout(museRunUsageRecord, museCompletedRecord("done")))

	ended := startMuseStubSession(t) // returns once the child is spawned, as dev@example.com
	t.Cleanup(func() {
		gate.open()
		select {
		case <-ended:
		case <-time.After(30 * time.Second):
		}
	})
	writeMuseConfig(t, home, "auth.json", museOtherAuthFixture)
	gate.open()
	waitMuseSessionEnded(t, ended)
	if readings := museRunReadings(); len(readings) != 0 {
		t.Fatalf("a reading from a run spawned as one account and read after a switch was saved: %#v", readings)
	}

	// The control: a run spawned as the new account files its reading.
	t.Setenv("OPENCODE_STUB_STDOUT", museStubStdout(museRunUsageWith(10, 50, museRunUsageAsOf.Add(time.Minute)), museCompletedRecord("done")))
	waitMuseSessionEnded(t, startMuseStubSession(t))
	got, ok := loadMuseCodeUsageLive(museOtherAccountKey())
	if !ok || got.Window.UsedPercent != 10 || got.Weekly.UsedPercent != 50 {
		t.Fatalf("a run spawned as the new account must file its reading: ok=%v reading=%#v", ok, got)
	}
	if _, ok := loadMuseCodeUsageLive(museDevAccountKey()); ok {
		t.Fatal("the new account's reading was filed under the old account")
	}
}
