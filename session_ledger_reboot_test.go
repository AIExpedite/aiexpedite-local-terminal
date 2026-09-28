// Tests for the OS-reboot proof in the spawn ledger (session_ledger.go,
// os_boot.go): an earlier agent boot's session that ran under an earlier OS
// boot is reaped whatever its containment or state, nothing is probed or
// killed on that path, and everything else keeps the same-boot rules.
package main

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

// rebootFixture is one entry of every shape the same-boot rules can never
// certify, all recorded under testOSBootA, plus a legacy (v1.0.38, no OS boot)
// entry and one whose later generation was refused.
func rebootFixture() ledgerFileData {
	return ledgerFileData{
		Version: spawnLedgerVersion,
		BootID:  "boot-1",
		Entries: []*ledgerEntry{
			// Unix: a process group leader, never vouched for by the same-boot
			// rules (a descendant may have setsid'd out of the group).
			{SessionID: "uncontained-unix", BootID: "boot-1", OSBoot: testOSBootA,
				PIDs: []ledgerProcess{{PID: 11, StartTime: "linux:x:1", PGID: 11}}},
			// Windows: a process that never made it into its Job Object.
			{SessionID: "uncontained-win", BootID: "boot-1", OSBoot: testOSBootA,
				PIDs: []ledgerProcess{{PID: 12, StartTime: "win:1"}}},
			{SessionID: "incomplete", BootID: "boot-1", OSBoot: testOSBootA, Incomplete: true,
				PIDs: []ledgerProcess{{PID: 13, StartTime: "t13"}}},
			{SessionID: "pending", BootID: "boot-1", OSBoot: testOSBootA, PendingSpawns: 1, PIDs: []ledgerProcess{}},
			{SessionID: "unknown", BootID: "boot-1", OSBoot: testOSBootA, State: ledgerStateUnknown,
				PIDs: []ledgerProcess{{PID: 14, StartTime: "t14"}}},
			{SessionID: "surviving", BootID: "boot-1", OSBoot: testOSBootA, State: ledgerStateSurviving,
				PIDs: []ledgerProcess{{PID: 15, StartTime: "t15"}}},
			{SessionID: "logical", BootID: "boot-1", OSBoot: testOSBootA, Logical: true, PIDs: []ledgerProcess{}},
			// Written by v1.0.38: no OS boot recorded, same-boot rules only.
			{SessionID: "legacy", BootID: "boot-1",
				PIDs: []ledgerProcess{{PID: 16, StartTime: "t16", PGID: 16}}},
			// A later generation of this id was refused (ledger full).
			{SessionID: "refused-twin", BootID: "boot-1", OSBoot: testOSBootA, Incomplete: true, UnrecordedGeneration: true,
				PIDs: []ledgerProcess{}},
		},
	}
}

// strictLedger is a test ledger under currentOSBoot whose probe, end and
// descendants seams record every PID they are asked about; end fails the
// test unless allowEnd.
type strictLedger struct {
	*spawnLedger
	recMu                   sync.Mutex
	probed, ended, descProb []int
}

func newStrictLedger(t *testing.T, dir, bootID string, current osBootStamp, allowEnd bool) *strictLedger {
	t.Helper()
	s := &strictLedger{spawnLedger: newTestLedger(t, dir, bootID)}
	s.osBoot = func() osBootStamp { return current }
	s.probe = func(p ledgerProcess) processProbeResult {
		s.recMu.Lock()
		s.probed = append(s.probed, p.PID)
		s.recMu.Unlock()
		return processGone
	}
	s.descendants = func(p ledgerProcess) processProbeResult {
		s.recMu.Lock()
		s.descProb = append(s.descProb, p.PID)
		s.recMu.Unlock()
		return processUnknown // Unix: never provable
	}
	s.end = func(p ledgerProcess, _ time.Duration) processProbeResult {
		if !allowEnd {
			t.Errorf("ended PID %d", p.PID)
		}
		s.recMu.Lock()
		s.ended = append(s.ended, p.PID)
		s.recMu.Unlock()
		return processGone
	}
	return s
}

func persistedStates(t *testing.T, dir string) map[string]string {
	t.Helper()
	states := map[string]string{}
	for _, e := range readLedgerFile(t, dir).Entries {
		states[e.SessionID] = e.State
	}
	return states
}

// TestRebootReapsEveryEarlierOSBootSession: a different OS boot id proves
// every session of the earlier OS boot reaped — uncontained Unix and Windows,
// incomplete, pending, unknown, surviving, logical — with no probe, no kill
// and no descendant check. A legacy entry (no OS boot) and a refused twin
// keep the same-boot rules.
func TestRebootReapsEveryEarlierOSBootSession(t *testing.T) {
	dir := t.TempDir()
	writeLedgerFixture(t, dir, rebootFixture())
	l := newStrictLedger(t, dir, "boot-2", osBootStamp{ID: testOSBootB}, false)

	l.RunBootReap()
	r := l.Report(context.Background())

	want := []string{"incomplete", "logical", "pending", "surviving", "uncontained-unix", "uncontained-win", "unknown"}
	if !reflect.DeepEqual(r.SessionsReaped, want) {
		t.Fatalf("reaped = %v, want %v", r.SessionsReaped, want)
	}
	if len(r.SessionsSurviving) != 0 {
		t.Fatalf("surviving = %v", r.SessionsSurviving)
	}
	// Only the legacy entry went through the same-boot rules; nothing of
	// the reboot-proven sessions was looked at.
	if !reflect.DeepEqual(l.probed, []int{16}) || !reflect.DeepEqual(l.descProb, []int{16}) {
		t.Fatalf("probed %v, descendants %v — want only the legacy PID 16", l.probed, l.descProb)
	}
	if len(l.ended) != 0 {
		t.Fatalf("ended %v on the reboot path", l.ended)
	}

	states := persistedStates(t, dir)
	for _, id := range want {
		if states[id] != ledgerStateReaped {
			t.Fatalf("%s persisted as %q, want reaped", id, states[id])
		}
	}
	if states["legacy"] != ledgerStateUnknown || states["refused-twin"] != ledgerStateUnknown {
		t.Fatalf("legacy / refused-twin persisted as %q / %q, want unknown", states["legacy"], states["refused-twin"])
	}
	for _, e := range readLedgerFile(t, dir).Entries {
		if e.State == ledgerStateReaped && len(e.PIDs) != 0 {
			t.Fatalf("a reaped entry kept its PIDs: %+v", e)
		}
		// The reap never restamps an earlier boot's entry.
		if e.SessionID != "legacy" && e.OSBoot != testOSBootA {
			t.Fatalf("earlier entry restamped: %+v", e)
		}
	}
}

// TestSameOSBootKeepsTheSameBootRules: the same OS boot id changes nothing —
// an uncontained / incomplete / pending session stays unproven, a still-live
// recorded process is ended, and only a provably empty tree is reaped.
func TestSameOSBootKeepsTheSameBootRules(t *testing.T) {
	dir := t.TempDir()
	writeLedgerFixture(t, dir, rebootFixture())
	l := newStrictLedger(t, dir, "boot-2", osBootStamp{ID: testOSBootA}, true)
	l.probe = func(p ledgerProcess) processProbeResult {
		l.recMu.Lock()
		l.probed = append(l.probed, p.PID)
		l.recMu.Unlock()
		if p.PID == 12 {
			return processOurs // still running: ended below
		}
		return processGone
	}

	l.RunBootReap()
	r := l.Report(context.Background())
	// Surviving / unknown are retried: both probed gone, but nothing proves
	// their descendants gone, so nothing is certified.
	if len(r.SessionsReaped) != 1 || r.SessionsReaped[0] != "logical" {
		t.Fatalf("reaped = %v, want only the empty logical session", r.SessionsReaped)
	}
	if !reflect.DeepEqual(l.ended, []int{12}) {
		t.Fatalf("ended = %v, want the still-live PID 12", l.ended)
	}
	sort.Ints(l.probed)
	if !reflect.DeepEqual(l.probed, []int{11, 12, 14, 15, 16}) {
		t.Fatalf("probed = %v", l.probed)
	}
	states := persistedStates(t, dir)
	for _, id := range []string{"uncontained-unix", "uncontained-win", "incomplete", "pending", "unknown", "surviving", "legacy", "refused-twin"} {
		if states[id] != ledgerStateUnknown {
			t.Fatalf("%s persisted as %q, want unknown", id, states[id])
		}
	}
}

// TestNoOSBootIdentityNoRebootProof: without a current OS boot id — or
// without a recorded one — nothing is proven by a reboot.
func TestNoOSBootIdentityNoRebootProof(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded string
		current  osBootStamp
	}{
		{"current unreadable", testOSBootA, osBootStamp{}},
		{"never recorded", "", osBootStamp{ID: testOSBootB}},
		{"kinds differ", testOSBootA, osBootStamp{ID: osBootKindDarwinSession + ":X"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeLedgerFixture(t, dir, ledgerFileData{BootID: "boot-1", Entries: []*ledgerEntry{
				{SessionID: "u", BootID: "boot-1", OSBoot: tc.recorded, PIDs: []ledgerProcess{{PID: 21, StartTime: "t", PGID: 21}}},
				{SessionID: "i", BootID: "boot-1", OSBoot: tc.recorded, Incomplete: true, PIDs: []ledgerProcess{}},
			}})
			l := newStrictLedger(t, dir, "boot-2", tc.current, false)
			l.RunBootReap()
			if r := l.Report(context.Background()); len(r.SessionsReaped) != 0 {
				t.Fatalf("reaped without an OS-reboot proof: %v", r.SessionsReaped)
			}
			if !reflect.DeepEqual(l.probed, []int{21}) {
				t.Fatalf("the same-boot rules did not run: probed %v", l.probed)
			}
		})
	}
}

// TestWindowsBootTimeToleranceThroughTheLedger: a computed Windows boot time
// that jittered or followed a clock step is the same boot; one whose tick
// count restarted is a reboot; a new boot that already ran past the recorded
// tick count proves nothing.
func TestWindowsBootTimeToleranceThroughTheLedger(t *testing.T) {
	const boot = int64(1_727_000_000)
	current := winStamp(boot, 3*hourMs)
	rec := func(bootSec, uptimeMs int64) (string, int64) {
		s := winStamp(bootSec, uptimeMs)
		return s.ID, s.UptimeMs
	}
	jitterID, jitterUp := rec(boot-2, hourMs)
	stepID, stepUp := rec(boot-600, 2*hourMs)
	rebootID, rebootUp := rec(boot-10*86400, 5*hourMs)
	longerID, longerUp := rec(boot-10*86400, hourMs)
	dir := t.TempDir()
	writeLedgerFixture(t, dir, ledgerFileData{BootID: "boot-1", Entries: []*ledgerEntry{
		{SessionID: "jitter", BootID: "boot-1", OSBoot: jitterID, OSUptimeMs: jitterUp, Incomplete: true, PIDs: []ledgerProcess{}},
		{SessionID: "clock-step", BootID: "boot-1", OSBoot: stepID, OSUptimeMs: stepUp, Incomplete: true, PIDs: []ledgerProcess{}},
		{SessionID: "rebooted", BootID: "boot-1", OSBoot: rebootID, OSUptimeMs: rebootUp, Incomplete: true, PIDs: []ledgerProcess{}},
		{SessionID: "new-boot-ran-longer", BootID: "boot-1", OSBoot: longerID, OSUptimeMs: longerUp, Incomplete: true, PIDs: []ledgerProcess{}},
	}})
	l := newStrictLedger(t, dir, "boot-2", current, false)
	l.RunBootReap()
	if r := l.Report(context.Background()); !reflect.DeepEqual(r.SessionsReaped, []string{"rebooted"}) {
		t.Fatalf("reaped = %v, want only the rebooted session", r.SessionsReaped)
	}
}

// TestSpawnStampsTheOSBoot: every spawn-path change of a current-boot entry
// records the OS boot (and refreshes the uptime); a failed read keeps the
// last stamp; a never-stamped entry carries none.
func TestSpawnStampsTheOSBoot(t *testing.T) {
	dir := t.TempDir()
	l := newTestLedger(t, dir, "boot-1")
	var uptime int64 = 1000
	readable := true
	l.osBoot = func() osBootStamp {
		if !readable {
			return osBootStamp{}
		}
		uptime += 1000
		return osBootStamp{ID: testOSBootA, UptimeMs: uptime}
	}
	l.BeginSpawn("s")
	e := readLedgerFile(t, dir).Entries[0]
	if e.OSBoot != testOSBootA || e.OSUptimeMs != 2000 {
		t.Fatalf("BeginSpawn stamp = %q / %d", e.OSBoot, e.OSUptimeMs)
	}
	l.TrackProcess("s", proc(31), false)
	if e := readLedgerFile(t, dir).Entries[0]; e.OSUptimeMs != 3000 {
		t.Fatalf("TrackProcess did not refresh the uptime: %d", e.OSUptimeMs)
	}
	readable = false
	l.OpenLogicalSession("s")
	if e := readLedgerFile(t, dir).Entries[0]; e.OSBoot != testOSBootA || e.OSUptimeMs != 3000 {
		t.Fatalf("an unreadable identity dropped the stamp: %+v", e)
	}
	l.OpenLogicalSession("never")
	for _, e := range readLedgerFile(t, dir).Entries {
		if e.SessionID == "never" && (e.OSBoot != "" || e.OSUptimeMs != 0) {
			t.Fatalf("stamped without an identity: %+v", e)
		}
	}

	// The next agent boot, after an OS reboot, reaps "s" (stamped) but not
	// "never" (unstamped; a pending spawn keeps it from being certified by
	// the same-boot rules as an empty logical session).
	l.BeginSpawn("never")
	l2 := newStrictLedger(t, dir, "boot-2", osBootStamp{ID: testOSBootB}, false)
	l2.RunBootReap()
	if r := l2.Report(context.Background()); !reflect.DeepEqual(r.SessionsReaped, []string{"s"}) {
		t.Fatalf("reaped = %v, want [s]", r.SessionsReaped)
	}
}

// TestRefusedGenerationBlocksTheRebootProof: when a full ledger refuses a
// session id, its recorded generations are flagged, and a later OS reboot
// never certifies that id through them — the refused generation may have
// run under the current OS boot.
func TestRefusedGenerationBlocksTheRebootProof(t *testing.T) {
	dir := t.TempDir()
	entries := []*ledgerEntry{}
	for i := 0; i < spawnLedgerMaxEntries; i++ {
		entries = append(entries, &ledgerEntry{
			SessionID: fmt.Sprintf("u%03d", i), BootID: "boot-1", OSBoot: testOSBootA,
			State: ledgerStateUnknown, UpdatedAt: int64(1_699_999_000_000 + i),
			PIDs: []ledgerProcess{{PID: 4, StartTime: "t"}},
		})
	}
	entries[0].SessionID = "twin"
	writeLedgerFixture(t, dir, ledgerFileData{BootID: "boot-1", Entries: entries})

	// boot-2, same OS boot: everything stays unknown; the full ledger
	// refuses a new generation of "twin".
	l2 := newTestLedger(t, dir, "boot-2")
	l2.probe = func(ledgerProcess) processProbeResult { return processUnknown }
	l2.RunBootReap()
	l2.BeginSpawn("twin")
	var twin *ledgerEntry
	for _, e := range readLedgerFile(t, dir).Entries {
		if e.SessionID == "twin" {
			twin = e
		}
	}
	if twin == nil || !twin.UnrecordedGeneration || twin.BootID != "boot-1" {
		t.Fatalf("twin = %+v, want its recorded generation flagged", twin)
	}

	// boot-3, after an OS reboot: every other session is proven reaped by
	// the reboot; "twin" is not.
	l3 := newStrictLedger(t, dir, "boot-3", osBootStamp{ID: testOSBootB}, false)
	l3.RunBootReap()
	r := l3.Report(context.Background())
	if len(r.SessionsReaped) != spawnLedgerMaxEntries-1 {
		t.Fatalf("reaped %d sessions, want %d", len(r.SessionsReaped), spawnLedgerMaxEntries-1)
	}
	for _, id := range r.SessionsReaped {
		if id == "twin" {
			t.Fatalf("a session id with a refused generation was certified")
		}
	}
}

// TestRebootReapedSessionsAreSignedOnOnline drives /online: sessions proven
// reaped by a reboot ride in sessionsReaped, covered by bootReportSignature.
func TestRebootReapedSessionsAreSignedOnOnline(t *testing.T) {
	resetConnectivityState(t)
	resetFencedSessions(t)
	dir := t.TempDir()
	writeLedgerFixture(t, dir, ledgerFileData{BootID: "boot-a", Entries: []*ledgerEntry{
		{SessionID: "s-mac", BootID: "boot-a", OSBoot: testOSBootA, PIDs: []ledgerProcess{{PID: 7, StartTime: "darwin:1.2", PGID: 7}}},
		{SessionID: "s-pending", BootID: "boot-a", OSBoot: testOSBootA, PendingSpawns: 2, PIDs: []ledgerProcess{}},
	}})
	l := newStrictLedger(t, dir, "boot-b", osBootStamp{ID: testOSBootB}, false)
	l.RunBootReap()
	prevReport, prevAccepted := bootReportForOnline, onOnlineAccepted
	bootReportForOnline = func(ctx context.Context) bootReport { return l.Report(ctx) }
	onOnlineAccepted = func(resp *onlineResponse) { applyOnlineAccepted(l.spawnLedger, resp) }
	t.Cleanup(func() { bootReportForOnline, onOnlineAccepted = prevReport, prevAccepted })

	c := &onlineCapture{}
	srv := c.server(t, func(int) int { return http.StatusOK }, `{"success":true}`)
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)
	cfg := &Config{AgentID: "agent-mac", CommandSecret: "secret-mac"}
	if err := notifyOnline(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	b := c.bodies[0]
	reaped := bodyStringList(t, b["sessionsReaped"])
	if !reflect.DeepEqual(reaped, []string{"s-mac", "s-pending"}) {
		t.Fatalf("sessionsReaped = %v", reaped)
	}
	ts := int64(b["timestamp"].(float64))
	if b["bootReportSignature"] != expectedBootReportSignature("secret-mac", "agent-mac", ts, "boot-b", reaped) {
		t.Fatalf("bootReportSignature does not cover the reboot-reaped list: %v", b["bootReportSignature"])
	}
	if b["previousBootId"] != "boot-a" || b["bootId"] != "boot-b" {
		t.Fatalf("boot ids: %v / %v", b["bootId"], b["previousBootId"])
	}
	if got := l.Report(context.Background()).SessionsReaped; len(got) != 0 {
		t.Fatalf("an accepted report must drop the entries: %v", got)
	}
}

// TestRebootPathNeverTouchesALiveProcess: a live process at a recorded PID
// with even the recorded start time (as if a PID and start time came back
// after a reboot) is never probed or ended on the reboot path — with every
// real seam in place.
func TestRebootPathNeverTouchesALiveProcess(t *testing.T) {
	if unsupportedIdentityPlatform() {
		t.Skip("no start-time reader on this platform")
	}
	cmd, _ := startLedgerChild(t, "grok-smoke-hang")
	rec := recordedProcess(t, cmd.Process)

	current := readOSBootStamp()
	if current.ID == "" {
		t.Skip("no OS boot identity on this runner")
	}
	// An OS boot of the same kind that is proven earlier than this one.
	earlier := earlierOSBootStampFor(t, current)

	dir := t.TempDir()
	writeLedgerFixture(t, dir, ledgerFileData{BootID: "boot-old", Entries: []*ledgerEntry{
		{SessionID: "s", BootID: "boot-old", OSBoot: earlier.ID, OSUptimeMs: earlier.UptimeMs, PIDs: []ledgerProcess{rec}},
	}})
	l := newRealLedger(t, filepath.Join(dir, spawnLedgerFileName), "boot-new")
	l.RunBootReap()
	if r := l.Report(context.Background()); !reflect.DeepEqual(r.SessionsReaped, []string{"s"}) {
		t.Fatalf("reaped = %v, want [s]", r.SessionsReaped)
	}
	time.Sleep(200 * time.Millisecond)
	if got := probeRecordedProcess(rec); got != processOurs {
		t.Fatalf("the reboot path touched a live process (probe = %v)", got)
	}
}

// earlierOSBootStampFor builds a stamp of current's kind that the rules
// prove to be an earlier OS boot.
func earlierOSBootStampFor(t *testing.T, current osBootStamp) osBootStamp {
	t.Helper()
	kind, value, _ := splitOSBootID(current.ID)
	var s osBootStamp
	switch kind {
	case osBootKindLinux, osBootKindDarwinSession:
		s = osBootStamp{ID: kind + ":not-" + value}
	case osBootKindWindowsBootTime, osBootKindDarwinBootTime:
		ms, ok := parseBootTimeMs(value)
		if !ok {
			t.Fatalf("unparseable current boot time %q", value)
		}
		// Booted two days earlier, and recorded after a longer uptime than
		// this boot has now: the since-boot counter restarted.
		s = osBootStamp{
			ID:       kind + ":" + strconv.FormatInt(ms/1000-2*86400, 10),
			UptimeMs: current.UptimeMs + dayMs,
		}
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	if !osBootRebootProven(s, current) {
		t.Fatalf("fixture %+v is not proven earlier than %+v", s, current)
	}
	return s
}

// TestRefreshOSBootStampsKeepsTheCounterCurrent: the running agent re-stamps
// only its own boot's entries, writes only on a change, and a larger recorded
// tick count is what lets the next boot prove the reboot.
func TestRefreshOSBootStampsKeepsTheCounterCurrent(t *testing.T) {
	const boot = int64(1_727_000_000)
	dir := t.TempDir()
	writeLedgerFixture(t, dir, ledgerFileData{BootID: "boot-0", Entries: []*ledgerEntry{
		{SessionID: "older", BootID: "boot-0", OSBoot: testOSBootA, State: ledgerStateUnknown, PIDs: []ledgerProcess{{PID: 3, StartTime: "t"}}},
	}})
	l := newTestLedger(t, dir, "boot-1")
	l.probe = func(ledgerProcess) processProbeResult { return processUnknown }
	tick := 2 * 60_000 // the session starts two minutes into the boot
	l.osBoot = func() osBootStamp { return winStamp(boot, int64(tick)) }
	var writes int
	l.writeFile = func(path string, data []byte) error { writes++; return writeLedgerFileAtomic(path, data) }

	l.RefreshOSBootStamps() // before the ledger is loaded: a no-op
	if writes != 0 {
		t.Fatalf("refresh before load wrote the ledger")
	}
	l.RunBootReap()
	l.BeginSpawn("s")
	l.TrackProcess("s", proc(41), false)
	writes = 0
	l.RefreshOSBootStamps() // nothing changed
	if writes != 0 {
		t.Fatalf("an unchanged refresh wrote the ledger %d time(s)", writes)
	}
	tick = int(5 * hourMs) // the session runs for hours
	l.RefreshOSBootStamps()
	if writes != 1 {
		t.Fatalf("refresh wrote %d time(s), want 1", writes)
	}
	for _, e := range readLedgerFile(t, dir).Entries {
		switch e.SessionID {
		case "s":
			if e.OSUptimeMs != 5*hourMs {
				t.Fatalf("s not refreshed: %+v", e)
			}
		case "older":
			if e.OSBoot != testOSBootA || e.OSUptimeMs != 0 {
				t.Fatalf("an earlier boot's entry was re-stamped: %+v", e)
			}
		}
	}

	// The machine reboots; the agent starts ten minutes into the new boot —
	// past the two minutes of the first stamp, well below the refreshed one.
	l2 := newStrictLedger(t, dir, "boot-2", winStamp(boot+6*3600, 10*60_000), false)
	l2.RunBootReap()
	if r := l2.Report(context.Background()); !reflect.DeepEqual(r.SessionsReaped, []string{"s"}) {
		t.Fatalf("reaped = %v, want [s]", r.SessionsReaped)
	}
}

// TestRefresherStopsOnShutdown: the refresher ticks and exits with stop.
func TestRefresherStopsOnShutdown(t *testing.T) {
	dir := t.TempDir()
	l := newTestLedger(t, dir, "boot-1")
	var mu sync.Mutex
	reads := 0
	l.osBoot = func() osBootStamp {
		mu.Lock()
		reads++
		mu.Unlock()
		return osBootStamp{ID: testOSBootA}
	}
	l.OpenLogicalSession("s")
	stop := make(chan struct{})
	startOSBootStampRefresher(l, 5*time.Millisecond, stop)
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := reads
		mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the refresher never ran (reads = %d)", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	after := reads
	mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if reads != after {
		t.Fatalf("the refresher kept running after stop (%d -> %d)", after, reads)
	}
}
