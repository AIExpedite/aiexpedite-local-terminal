// Tests for the spawn ledger and the boot classification (session_ledger.go).
// These use fake probes; session_ledger_process_test.go drives real processes.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestLedger returns a ledger on a temp path whose process-facing seams
// never touch a real process (no Job Object, no process group, no /proc).
func newTestLedger(t *testing.T, dir, bootID string) *spawnLedger {
	t.Helper()
	path := filepath.Join(dir, spawnLedgerFileName)
	l := newSpawnLedger(func() string { return path }, bootID)
	l.attachJob = func(*os.Process) (uintptr, error) { return 0, nil }
	l.releaseJob = func(uintptr) {}
	l.groupOf = func(*os.Process) int { return 0 }
	l.descendants = func(ledgerProcess) processProbeResult { return processGone }
	l.startToken = func(pid int) (string, error) { return fmt.Sprintf("tok-%d", pid), nil }
	l.probe = func(ledgerProcess) processProbeResult {
		t.Fatalf("unexpected probe")
		return processUnknown
	}
	l.end = func(ledgerProcess, time.Duration) processProbeResult {
		t.Fatalf("unexpected end")
		return processUnknown
	}
	var tick int64
	l.now = func() time.Time { tick++; return time.UnixMilli(1_700_000_000_000 + tick) }
	return l
}

func readLedgerFile(t *testing.T, dir string) ledgerFileData {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, spawnLedgerFileName))
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	var data ledgerFileData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("ledger is not valid JSON: %v\n%s", err, raw)
	}
	return data
}

func writeLedgerFixture(t *testing.T, dir string, data ledgerFileData) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, spawnLedgerFileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func proc(pid int) *os.Process { return &os.Process{Pid: pid} }

func TestNewBootIDIsAUniqueUUID(t *testing.T) {
	a, b := newBootID(), newBootID()
	if a == b {
		t.Fatalf("two boots share an id: %s", a)
	}
	if len(a) != 36 || strings.Count(a, "-") != 4 || a[14] != '4' {
		t.Fatalf("not a v4 UUID: %q", a)
	}
	if CurrentBootID() != agentBootID || agentBootID == "" {
		t.Fatalf("CurrentBootID mismatch")
	}
}

func TestSpawnLedgerPersistsEveryChangeAtomically(t *testing.T) {
	dir := t.TempDir()
	l := newTestLedger(t, dir, "boot-1")

	l.BeginSpawn("s1")
	data := readLedgerFile(t, dir)
	if data.BootID != "boot-1" || len(data.Entries) != 1 || data.Entries[0].PendingSpawns != 1 {
		t.Fatalf("BeginSpawn not persisted before Start: %+v", data)
	}

	l.TrackProcess("s1", proc(101))
	l.OpenLogicalSession("s2")
	data = readLedgerFile(t, dir)
	if len(data.Entries) != 2 {
		t.Fatalf("want 2 entries, got %+v", data.Entries)
	}
	e := data.Entries[0]
	if e.SessionID != "s1" || e.BootID != "boot-1" || e.PendingSpawns != 0 ||
		!reflect.DeepEqual(e.PIDs, []ledgerProcess{{PID: 101, StartTime: "tok-101"}}) {
		t.Fatalf("tracked entry wrong: %+v", e)
	}
	if !data.Entries[1].Logical || len(data.Entries[1].PIDs) != 0 {
		t.Fatalf("logical entry wrong: %+v", data.Entries[1])
	}

	// Written through a temp file + rename: no temp file is left behind.
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		if f.Name() != spawnLedgerFileName {
			t.Fatalf("stray file left in ledger dir: %s", f.Name())
		}
	}

	// A clean end removes the session.
	l.ReleaseSession("s1")
	data = readLedgerFile(t, dir)
	if len(data.Entries) != 1 || data.Entries[0].SessionID != "s2" {
		t.Fatalf("ReleaseSession did not remove s1: %+v", data.Entries)
	}
}

func TestSpawnLedgerAbortAndLogicalTurns(t *testing.T) {
	dir := t.TempDir()
	l := newTestLedger(t, dir, "boot-1")

	// A failed Start leaves nothing behind.
	l.BeginSpawn("failed")
	l.AbortSpawn("failed")
	if got := readLedgerFile(t, dir).Entries; len(got) != 0 {
		t.Fatalf("aborted spawn left an entry: %+v", got)
	}

	// A logical session keeps its entry between per-turn processes.
	l.OpenLogicalSession("agy")
	l.BeginSpawn("agy")
	l.TrackProcess("agy", proc(7))
	l.UntrackProcess("agy", 7)
	got := readLedgerFile(t, dir).Entries
	if len(got) != 1 || !got[0].Logical || len(got[0].PIDs) != 0 || got[0].PendingSpawns != 0 {
		t.Fatalf("logical session between turns: %+v", got)
	}
	l.ReleaseSession("agy")
	if got := readLedgerFile(t, dir).Entries; len(got) != 0 {
		t.Fatalf("released logical session kept: %+v", got)
	}

	// A turn that outlived its session's release does not leave a bare entry.
	l.BeginSpawn("late")
	l.TrackProcess("late", proc(8))
	l.UntrackProcess("late", 8)
	if got := readLedgerFile(t, dir).Entries; len(got) != 0 {
		t.Fatalf("bare entry kept after its only process: %+v", got)
	}
}

func TestSpawnLedgerMissingStartTimeIsIncomplete(t *testing.T) {
	dir := t.TempDir()
	l := newTestLedger(t, dir, "boot-1")
	l.startToken = func(int) (string, error) { return "", fmt.Errorf("denied") }
	l.BeginSpawn("s")
	l.TrackProcess("s", proc(9))
	got := readLedgerFile(t, dir).Entries
	if len(got) != 1 || !got[0].Incomplete || len(got[0].PIDs) != 0 {
		t.Fatalf("a PID without a start time must mark the entry incomplete: %+v", got)
	}
}

func TestSpawnLedgerIsBounded(t *testing.T) {
	dir := t.TempDir()
	l := newTestLedger(t, dir, "boot-1")
	for i := 0; i < spawnLedgerMaxEntries+5; i++ {
		l.OpenLogicalSession(fmt.Sprintf("s%03d", i))
	}
	got := readLedgerFile(t, dir).Entries
	if len(got) != spawnLedgerMaxEntries {
		t.Fatalf("want %d entries, got %d", spawnLedgerMaxEntries, len(got))
	}
	if got[0].SessionID != "s005" {
		t.Fatalf("the oldest entries must be evicted first, first kept = %s", got[0].SessionID)
	}

	// Too many processes for one session: never certifiable.
	for i := 0; i < spawnLedgerMaxPIDs+1; i++ {
		l.TrackProcess("many", proc(1000+i))
	}
	for _, e := range readLedgerFile(t, dir).Entries {
		if e.SessionID == "many" && (!e.Incomplete || len(e.PIDs) != spawnLedgerMaxPIDs) {
			t.Fatalf("PID cap: %+v", e)
		}
	}
}

// TestBootReapClassification pins reaped / surviving / unknown: only
// recorded PIDs are probed, only ones still ours are ended, and an
// undeterminable session is never listed.
func TestBootReapClassification(t *testing.T) {
	dir := t.TempDir()
	writeLedgerFixture(t, dir, ledgerFileData{
		Version: spawnLedgerVersion,
		BootID:  "boot-1",
		Entries: []*ledgerEntry{
			{SessionID: "gone", BootID: "boot-1", PIDs: []ledgerProcess{{PID: 1, StartTime: "t1"}}},
			{SessionID: "ended", BootID: "boot-1", PIDs: []ledgerProcess{{PID: 2, StartTime: "t2", PGID: 2}}},
			{SessionID: "stuck", BootID: "boot-1", PIDs: []ledgerProcess{{PID: 3, StartTime: "t3"}}},
			{SessionID: "denied", BootID: "boot-1", PIDs: []ledgerProcess{{PID: 4, StartTime: "t4"}}},
			{SessionID: "mixed", BootID: "boot-1", PIDs: []ledgerProcess{{PID: 1, StartTime: "t1"}, {PID: 4, StartTime: "t4"}}},
			{SessionID: "idle-agy", BootID: "boot-1", Logical: true, PIDs: []ledgerProcess{}},
			// Leader gone, but its group / job cannot be proven empty.
			{SessionID: "left-a-tool", BootID: "boot-1", PIDs: []ledgerProcess{{PID: 5, StartTime: "t5", PGID: 5}}},
			{SessionID: "pending", BootID: "boot-1", PendingSpawns: 1, PIDs: []ledgerProcess{}},
			{SessionID: "incomplete", BootID: "boot-1", Incomplete: true, PIDs: []ledgerProcess{{PID: 1, StartTime: "t1"}}},
		},
	})

	l := newTestLedger(t, dir, "boot-2")
	var mu sync.Mutex
	var probed, ended []int
	l.probe = func(p ledgerProcess) processProbeResult {
		mu.Lock()
		probed = append(probed, p.PID)
		mu.Unlock()
		switch p.PID {
		case 1, 5:
			return processGone
		case 2, 3:
			return processOurs
		default:
			return processUnknown
		}
	}
	l.descendants = func(p ledgerProcess) processProbeResult {
		if p.PID == 5 {
			return processUnknown
		}
		return processGone
	}
	l.end = func(p ledgerProcess, _ time.Duration) processProbeResult {
		mu.Lock()
		ended = append(ended, p.PID)
		mu.Unlock()
		if p.PID == 2 {
			return processGone
		}
		return processOurs // pid 3 refuses to die
	}

	l.RunBootReap()
	r := l.Report(context.Background())

	if r.BootID != "boot-2" || r.PreviousBootID != "boot-1" {
		t.Fatalf("boot ids: %+v", r)
	}
	if want := []string{"ended", "gone", "idle-agy"}; !reflect.DeepEqual(r.SessionsReaped, want) {
		t.Fatalf("reaped = %v, want %v", r.SessionsReaped, want)
	}
	if want := []string{"stuck"}; !reflect.DeepEqual(r.SessionsSurviving, want) {
		t.Fatalf("surviving = %v, want %v", r.SessionsSurviving, want)
	}
	if !reflect.DeepEqual(ended, []int{2, 3}) {
		t.Fatalf("only processes still ours may be ended, ended = %v", ended)
	}
	for _, pid := range probed {
		if pid < 1 || pid > 5 {
			t.Fatalf("probed a PID the ledger never recorded: %d", pid)
		}
	}

	// Surviving and unknown are kept for a retry; the permanently
	// unknowable (pending / incomplete) are dropped.
	states := map[string]string{}
	for _, e := range readLedgerFile(t, dir).Entries {
		states[e.SessionID] = e.State
	}
	want := map[string]string{
		"gone": ledgerStateReaped, "ended": ledgerStateReaped, "idle-agy": ledgerStateReaped,
		"stuck": ledgerStateSurviving, "denied": ledgerStateUnknown, "mixed": ledgerStateUnknown,
		"left-a-tool": ledgerStateUnknown,
	}
	if !reflect.DeepEqual(states, want) {
		t.Fatalf("persisted states = %v, want %v", states, want)
	}

	// Only an accepted /online drops a reaped entry.
	l.AckReaped([]string{"gone"})
	if got := l.Report(context.Background()).SessionsReaped; !reflect.DeepEqual(got, []string{"ended", "idle-agy"}) {
		t.Fatalf("after ack: %v", got)
	}

	// The next boot retries the survivors and the unknown, and re-reports
	// the reaped sessions whose report was never accepted.
	l3 := newTestLedger(t, dir, "boot-3")
	l3.probe = func(ledgerProcess) processProbeResult { return processGone }
	l3.RunBootReap()
	r3 := l3.Report(context.Background())
	if r3.PreviousBootID != "boot-2" {
		t.Fatalf("previousBootId = %q", r3.PreviousBootID)
	}
	if want := []string{"denied", "ended", "idle-agy", "left-a-tool", "mixed", "stuck"}; !reflect.DeepEqual(r3.SessionsReaped, want) {
		t.Fatalf("boot-3 reaped = %v, want %v", r3.SessionsReaped, want)
	}
	if len(r3.SessionsSurviving) != 0 {
		t.Fatalf("boot-3 surviving = %v", r3.SessionsSurviving)
	}
}

func TestBootReapLeavesCurrentBootAlone(t *testing.T) {
	dir := t.TempDir()
	l := newTestLedger(t, dir, "boot-now")
	l.BeginSpawn("live")
	l.TrackProcess("live", proc(55))
	// probe / end fail the test if called: a current-boot session is never
	// probed, ended or reported.
	l.RunBootReap()
	r := l.Report(context.Background())
	if len(r.SessionsReaped) != 0 || len(r.SessionsSurviving) != 0 {
		t.Fatalf("current boot reported: %+v", r)
	}
}

func TestReportBeforeReapCarriesBootIDOnly(t *testing.T) {
	l := newTestLedger(t, t.TempDir(), "boot-x")
	r := l.Report(context.Background())
	if r.BootID != "boot-x" || r.PreviousBootID != "" || len(r.SessionsReaped) != 0 {
		t.Fatalf("report without a reap: %+v", r)
	}
}

func TestBootReapDropsStaleUnresolvedEntries(t *testing.T) {
	dir := t.TempDir()
	old := time.UnixMilli(1_700_000_000_000).Add(-spawnLedgerMaxAge - time.Hour).UnixMilli()
	writeLedgerFixture(t, dir, ledgerFileData{
		BootID: "boot-1",
		Entries: []*ledgerEntry{
			{SessionID: "ancient", BootID: "boot-1", UpdatedAt: old, PIDs: []ledgerProcess{{PID: 4, StartTime: "t4"}}},
		},
	})
	l := newTestLedger(t, dir, "boot-2")
	l.probe = func(ledgerProcess) processProbeResult { return processUnknown }
	l.RunBootReap()
	if got := readLedgerFile(t, dir).Entries; len(got) != 0 {
		t.Fatalf("stale unresolved entry kept: %+v", got)
	}
}

func TestLedgerIgnoresUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, spawnLedgerFileName), []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := newTestLedger(t, dir, "boot-2")
	l.RunBootReap()
	if r := l.Report(context.Background()); len(r.SessionsReaped) != 0 || r.PreviousBootID != "" {
		t.Fatalf("a torn ledger must certify nothing: %+v", r)
	}
}
