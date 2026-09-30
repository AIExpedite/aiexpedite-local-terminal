package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The log index: which `agy` logs are runs that owe a utilization reading.
// Every case drives antigravityDiscoveryPass with a pinned `now` and fake
// process identities, so nothing here waits on a real clock or spawns agy.

// helperLogIndex is one isolated index: a base under a temp home, a fake
// process table and no cached reading.
type helperLogIndex struct {
	home, base string
	mu         sync.Mutex
	live       map[int]bool
	unknown    map[int]bool
}

func helperIsolateLogIndex(t *testing.T) *helperLogIndex {
	t.Helper()
	helperIsolateAntigravityFreshness(t)
	resetAntigravityLogIndex()
	resetAntigravityExhaustionEvidence()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	idx := &helperLogIndex{
		home: home, base: filepath.Join(home, ".gemini", "antigravity-cli"),
		live: map[int]bool{}, unknown: map[int]bool{},
	}
	if err := os.MkdirAll(antigravityLogDir(idx.base), 0o755); err != nil {
		t.Fatal(err)
	}
	origProbe, origToken, origScan := antigravityCandidateProbe, antigravityProcessStartToken, antigravityProcessScan
	antigravityCandidateProbe = func(pid int, token string) processProbeResult {
		idx.mu.Lock()
		defer idx.mu.Unlock()
		switch {
		case token == "":
			return processGone
		case idx.unknown[pid]:
			return processUnknown
		case idx.live[pid] && token == fmt.Sprintf("tok-%d", pid):
			return processOurs
		}
		return processGone
	}
	antigravityProcessStartToken = func(pid int) (string, error) {
		idx.mu.Lock()
		defer idx.mu.Unlock()
		if idx.live[pid] || idx.unknown[pid] {
			return fmt.Sprintf("tok-%d", pid), nil
		}
		return "", fmt.Errorf("no such process")
	}
	antigravityProcessScan = func() ([]ProcessInfo, bool) { return nil, false }
	t.Cleanup(func() {
		antigravityCandidateProbe, antigravityProcessStartToken, antigravityProcessScan = origProbe, origToken, origScan
		antigravityUsageRefreshWaitIdle()
		resetAntigravityLogIndex()
		resetAntigravityExhaustionEvidence()
	})
	return idx
}

func (h *helperLogIndex) setLive(pid int, live bool) {
	h.mu.Lock()
	h.live[pid] = live
	h.mu.Unlock()
}

// logName is the second-stamped name a run started at `at` gets.
func helperLogName(at time.Time) string {
	return "cli-" + at.In(time.Local).Format("20060102_150405") + ".log"
}

// helperPIDBlock is one run's block as agy writes it.
func helperPIDBlock(pid int) string {
	return fmt.Sprintf("I0929 20:43:56.000000 1 main.go:1] Starting language server process with pid %d\n"+
		"I0929 20:43:56.100000 1 server.go:584] Language server listening on random port at 5%04d for HTTP\n", pid, pid%10000)
}

// write writes (or appends to) a log and pins its mtime.
func (h *helperLogIndex) write(t *testing.T, name, body string, mtime time.Time, appendTo bool) string {
	t.Helper()
	path := filepath.Join(antigravityLogDir(h.base), name)
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendTo {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

func (h *helperLogIndex) pass(now time.Time, observedMs int64) antigravityDiscoveryResult {
	res := antigravityDiscoveryPass([]string{h.base}, now, observedMs)
	antigravityApplyDiscovery(res, now)
	antigravityUsageRefreshWaitIdle()
	return res
}

func helperEntryClass(path string) (antigravityLogClass, bool) {
	lockAntigravityLogIndex()
	defer unlockAntigravityLogIndex()
	entry, ok := antigravityLogIndex.entries[path]
	if !ok {
		return antigravityLogSettled, false
	}
	return entry.class, true
}

func helperIndexCounts() (owned, candidates, noPID, grace, processOnly int, sentinel bool) {
	lockAntigravityLogIndex()
	defer unlockAntigravityLogIndex()
	for _, entry := range antigravityLogIndex.entries {
		if entry.grace {
			grace++
			continue
		}
		switch entry.class {
		case antigravityLogOwned:
			owned++
		case antigravityLogCandidate:
			candidates++
		case antigravityLogNoPID:
			noPID++
		}
	}
	return owned, candidates, noPID, grace, len(antigravityLogIndex.processOnly), antigravityLogIndex.sentinel != nil
}

/* ─────────────────────────── attribution ─────────────────────────── */

// An own child's log is owned only when its SOLE block is the child's; a file
// that also holds another run's block is not the child's to claim.
func TestAntigravityOwnChild_OwnsOnlyItsSoleBlockFiles(t *testing.T) {
	h := helperIsolateLogIndex(t)
	now := time.Now()
	child := beginAntigravityOwnChild(h.home)
	if !antigravityOwnChildRunning() {
		t.Fatal("an own child that began is not counted as running")
	}
	child.setPID(4101)
	owned := h.write(t, helperLogName(now), helperPIDBlock(4101), now, false)
	shared := h.write(t, helperLogName(now.Add(time.Second)), helperPIDBlock(4101)+helperPIDBlock(4102), now, false)
	child.done()
	child.done() // idempotent

	if antigravityOwnChildRunning() {
		t.Error("done() left the child counted as running")
	}
	if class, ok := helperEntryClass(owned); !ok || class != antigravityLogOwned {
		t.Errorf("the child's own log class=%v tracked=%v, want owned", class, ok)
	}
	if class, ok := helperEntryClass(shared); ok && class == antigravityLogOwned {
		t.Error("a file holding another run's block was claimed as owned")
	}
}

// done() examines at most antigravityOwnChildMaxFiles new files, however many
// appeared.
func TestAntigravityOwnChild_ExaminesAtMostEightNewFiles(t *testing.T) {
	h := helperIsolateLogIndex(t)
	now := time.Now()
	child := beginAntigravityOwnChild(h.home)
	child.setPID(4201)
	for i := 0; i < 12; i++ {
		h.write(t, helperLogName(now.Add(time.Duration(i)*time.Second)), helperPIDBlock(4201), now, false)
	}
	child.done()
	if owned, _, _, _, _, _ := helperIndexCounts(); owned != antigravityOwnChildMaxFiles {
		t.Errorf("owned=%d, want at most %d examined", owned, antigravityOwnChildMaxFiles)
	}
}

// A child that never started (no PID) claims nothing and still releases.
func TestAntigravityOwnChild_StartFailureReleases(t *testing.T) {
	h := helperIsolateLogIndex(t)
	child := beginAntigravityOwnChild(h.home)
	h.write(t, helperLogName(time.Now()), helperPIDBlock(4301), time.Now(), false)
	child.done()
	if antigravityOwnChildRunning() {
		t.Error("a child that never started is still counted as running")
	}
	if owned, _, _, _, _, _ := helperIndexCounts(); owned != 0 {
		t.Errorf("owned=%d for a child with no PID, want 0", owned)
	}
}

/* ─────────────────────────── candidates ─────────────────────────── */

// A live foreign run is a candidate, armed and never owed while it lives. Once
// its PID is gone it is owed after the settle window, floored at max(mtime,
// exit seen).
func TestAntigravityCandidate_OwedOnlyAfterExit(t *testing.T) {
	h := helperIsolateLogIndex(t)
	start := time.Now().Add(-10 * time.Minute)
	h.setLive(5001, true)
	path := h.write(t, helperLogName(start), helperPIDBlock(5001), start.Add(2*time.Second), false)

	res := h.pass(start.Add(time.Minute), 0)
	if class, _ := helperEntryClass(path); class != antigravityLogCandidate {
		t.Fatalf("class=%v, want a candidate", class)
	}
	if !res.owed.IsZero() {
		t.Fatalf("owed=%s for a live run", res.owed)
	}
	if antigravityOldestProtectedFloorMs() == 0 {
		t.Error("a live candidate's floor is not protected")
	}
	// Quiet log, process still alive: still not owed.
	if res := h.pass(start.Add(9*time.Minute), 0); !res.owed.IsZero() {
		t.Fatalf("owed=%s while the process is alive", res.owed)
	}

	h.setLive(5001, false)
	exitSeen := start.Add(10 * time.Minute)
	// The log has been idle for minutes, so the exit is owed at once, at the
	// instant the exit was seen.
	res = h.pass(exitSeen, 0)
	if !res.owed.Equal(exitSeen) {
		t.Errorf("owed=%s, want the exit-seen instant %s", res.owed, exitSeen)
	}
	if antigravityOldestProtectedFloorMs() != 0 {
		t.Error("an owed candidate kept its floor protected")
	}
}

// A candidate whose log is still being written when the exit is seen waits the
// settle window after that exit.
func TestAntigravityCandidate_SettlesAfterExitSeen(t *testing.T) {
	h := helperIsolateLogIndex(t)
	now := time.Now().Add(-time.Hour)
	h.setLive(5101, true)
	h.write(t, helperLogName(now), helperPIDBlock(5101), now, false)
	h.pass(now.Add(5*time.Second), 0)
	h.setLive(5101, false)
	if res := h.pass(now.Add(10*time.Second), 0); !res.owed.IsZero() {
		t.Fatalf("owed=%s inside the settle window", res.owed)
	}
	res := h.pass(now.Add(10*time.Second+antigravityCandidateSettle), 0)
	if !res.owed.Equal(now.Add(10 * time.Second)) {
		t.Errorf("owed=%s, want the exit-seen floor", res.owed)
	}
}

// A PID the OS handed to another process (a different start token) is not the
// run: the candidate counts as exited.
func TestAntigravityCandidate_RecycledPIDCountsAsExited(t *testing.T) {
	h := helperIsolateLogIndex(t)
	now := time.Now().Add(-time.Hour)
	h.setLive(5201, true)
	h.write(t, helperLogName(now), helperPIDBlock(5201), now, false)
	h.pass(now.Add(time.Second), 0)
	orig := antigravityCandidateProbe
	antigravityCandidateProbe = func(pid int, token string) processProbeResult { return processGone }
	defer func() { antigravityCandidateProbe = orig }()
	if res := h.pass(now.Add(2*time.Minute), 0); res.owed.IsZero() {
		t.Error("a recycled PID kept the candidate live")
	}
}

// Unreadable PIDs, or more PIDs than are tracked, hold a candidate live — but
// only for the hold limit, after which it is owed with one counter line.
func TestAntigravityCandidate_UnknownHeldUntilTheHoldLimit(t *testing.T) {
	h := helperIsolateLogIndex(t)
	now := time.Now().Add(-8 * time.Hour)
	h.mu.Lock()
	h.unknown[5301] = true
	h.mu.Unlock()
	h.write(t, helperLogName(now), helperPIDBlock(5301), now, false)
	if res := h.pass(now.Add(time.Hour), 0); !res.owed.IsZero() {
		t.Fatalf("owed=%s for an unreadable PID inside the hold limit", res.owed)
	}
	var res antigravityDiscoveryResult
	logged := captureStdout(t, func() { res = h.pass(now.Add(time.Hour+antigravityCandidateHoldLimit+time.Minute), 0) })
	if res.owed.IsZero() || res.heldOwed != 1 {
		t.Errorf("owed=%s held=%d, want owed at the hold limit", res.owed, res.heldOwed)
	}
	if !strings.Contains(logged, "count=1") {
		t.Errorf("no counter line for the held candidate: %q", logged)
	}
}

// Five foreign PIDs in one log: the fifth flags pidOverflow, which keeps the
// run live even when every tracked PID has exited.
func TestAntigravityCandidate_PIDOverflowStaysLive(t *testing.T) {
	h := helperIsolateLogIndex(t)
	now := time.Now().Add(-time.Hour)
	body := ""
	for pid := 5401; pid <= 5405; pid++ {
		body += helperPIDBlock(pid)
	}
	h.write(t, helperLogName(now), body, now, false)
	if res := h.pass(now.Add(10*time.Minute), 0); !res.owed.IsZero() {
		t.Errorf("owed=%s with an overflowing PID set, want held live", res.owed)
	}
}

// A log with no PID block is owed after the settle window of silence; one that
// gains a block while its process lives becomes a live candidate instead.
func TestAntigravityNoPID_OwedAfterIdleOrPromoted(t *testing.T) {
	h := helperIsolateLogIndex(t)
	now := time.Now().Add(-time.Hour)
	quiet := h.write(t, helperLogName(now), "I0929 starting\n", now, false)
	late := h.write(t, helperLogName(now.Add(time.Second)), "I0929 starting\n", now.Add(time.Second), false)

	h.pass(now.Add(5*time.Second), 0)
	if class, _ := helperEntryClass(late); class != antigravityLogNoPID {
		t.Fatalf("class=%v, want noPID", class)
	}
	// PIDBlockAppearsAfterFirstTick: the block lands 5 s later while the
	// process lives.
	h.setLive(5501, true)
	h.write(t, filepath.Base(late), helperPIDBlock(5501), now.Add(6*time.Second), true)
	res := h.pass(now.Add(65*time.Second), 0)
	if class, _ := helperEntryClass(late); class != antigravityLogCandidate {
		t.Errorf("class=%v, want the promoted log to be a live candidate", class)
	}
	if !res.owed.Equal(now) {
		t.Errorf("owed=%s, want only the quiet noPID log owed at its mtime %s", res.owed, now)
	}
	if class, tracked := helperEntryClass(quiet); tracked && class != antigravityLogSettled {
		t.Errorf("the owed noPID log is still class %v", class)
	}
	// Not owed early: owed only after its process exits.
	h.setLive(5501, false)
	res = h.pass(now.Add(10*time.Minute), 0)
	if !res.owed.Equal(now.Add(10 * time.Minute)) {
		t.Errorf("owed=%s, want the promoted run owed at its exit", res.owed)
	}
}

// A reading that lands after a candidate's exit was seen releases it without
// owing anything; so does one newer than a noPID log.
func TestAntigravityCandidate_ReleasedByALaterReading(t *testing.T) {
	h := helperIsolateLogIndex(t)
	now := time.Now().Add(-time.Hour)
	h.setLive(5601, true)
	path := h.write(t, helperLogName(now), helperPIDBlock(5601), now, false)
	h.pass(now.Add(time.Second), 0)
	h.setLive(5601, false)
	h.pass(now.Add(2*time.Second), 0)
	res := h.pass(now.Add(3*time.Second), now.Add(2500*time.Millisecond).UnixMilli())
	if !res.owed.IsZero() {
		t.Errorf("owed=%s after a reading covered the exit", res.owed)
	}
	if class, tracked := helperEntryClass(path); tracked && class == antigravityLogCandidate {
		t.Error("the covered candidate is still held")
	}
}

// Managed runs are never candidates: their capture settles them.
func TestAntigravityCandidate_ManagedPIDIsNotACandidate(t *testing.T) {
	h := helperIsolateLogIndex(t)
	now := time.Now().Add(-time.Hour)
	h.setLive(5701, true)
	noteAntigravityManagedPID(5701)
	path := h.write(t, helperLogName(now), helperPIDBlock(5701), now, false)
	if res := h.pass(now.Add(2*time.Minute), 0); !res.owed.IsZero() {
		t.Errorf("owed=%s for a managed run", res.owed)
	}
	if class, _ := helperEntryClass(path); class == antigravityLogCandidate {
		t.Error("a managed run's log became a candidate")
	}
}

// A remembered own or managed PID that the OS reuses for a user's agy is a
// direct run: the start token, not the bare PID, decides.
func TestAntigravityCandidate_ReusedRememberedPIDIsADirectRun(t *testing.T) {
	for _, kind := range []string{"own", "managed"} {
		t.Run(kind, func(t *testing.T) {
			h := helperIsolateLogIndex(t)
			var mu sync.Mutex
			tokens := map[int]string{7101: "start-a"}
			antigravityProcessStartToken = func(pid int) (string, error) {
				mu.Lock()
				defer mu.Unlock()
				if token, ok := tokens[pid]; ok {
					return token, nil
				}
				return "", fmt.Errorf("no such process")
			}
			antigravityCandidateProbe = func(pid int, token string) processProbeResult {
				mu.Lock()
				defer mu.Unlock()
				if token != "" && tokens[pid] == token {
					return processOurs
				}
				return processGone
			}
			setToken := func(token string) {
				mu.Lock()
				defer mu.Unlock()
				if token == "" {
					delete(tokens, 7101)
					return
				}
				tokens[7101] = token
			}

			if kind == "own" {
				child := beginAntigravityOwnChild(h.home)
				child.setPID(7101)
				child.done()
			} else {
				noteAntigravityManagedPID(7101)
			}
			// The remembered process exits and the OS hands its PID to a
			// user-started agy.
			setToken("start-b")
			now := time.Now().Add(-time.Hour)
			path := h.write(t, helperLogName(now), helperPIDBlock(7101), now, false)
			if res := h.pass(now.Add(30*time.Second), 0); !res.owed.IsZero() {
				t.Fatalf("owed=%s while the user's run is live", res.owed)
			}
			if class, _ := helperEntryClass(path); class != antigravityLogCandidate {
				t.Fatalf("class=%v, want the reused PID's log to be a direct-run candidate", class)
			}
			setToken("")
			if res := h.pass(now.Add(5*time.Minute), 0); res.owed.IsZero() {
				t.Error("the user's run on a reused PID was never owed after it exited")
			}
		})
	}
}

// A user's agy that reuses a remembered PID and exits before the next pass
// leaves no readable start token. The remembered run had already ended when
// that log was named, so the PID alone must not claim it: it is a direct run
// and is owed. The remembered run's own log, named while it ran, still
// matches by PID when its token is unreadable.
func TestAntigravityCandidate_ExitedReusedPIDIsADirectRun(t *testing.T) {
	for _, kind := range []string{"own", "managed"} {
		t.Run(kind, func(t *testing.T) {
			h := helperIsolateLogIndex(t)
			antigravityProcessStartToken = func(int) (string, error) { return "", fmt.Errorf("no such process") }
			antigravityCandidateProbe = func(int, string) processProbeResult { return processGone }

			ran := time.Now().Add(-time.Minute)
			ownLog := h.write(t, helperLogName(ran), helperPIDBlock(7201), ran, false)
			if kind == "own" {
				child := beginAntigravityOwnChild(h.home)
				child.setPID(7201)
				child.done()
			} else {
				noteAntigravityManagedPID(7201)
				noteAntigravityManagedPIDExited(7201)
			}

			later := time.Now().Add(time.Minute)
			reused := h.write(t, helperLogName(later), helperPIDBlock(7201), later, false)
			h.pass(later.Add(10*time.Second), 0)
			if class, _ := helperEntryClass(reused); class != antigravityLogCandidate {
				t.Fatalf("class=%v, want the exited reused PID's log to be a direct-run candidate", class)
			}
			if class, _ := helperEntryClass(ownLog); class == antigravityLogCandidate {
				t.Error("the remembered run's own log became a candidate")
			}
			if res := h.pass(later.Add(5*time.Minute), 0); res.owed.IsZero() {
				t.Error("the user's run on an exited reused PID was never owed")
			}
		})
	}
}

/* ─────────────────────────── reclassification ─────────────────────────── */

// DirectRunAppendedToOwnedLog: a direct run started in the same second as an
// own `agy models` appends its block to that owned file. No new name appears;
// the next tick sees the size change and the file turns foreign.
func TestAntigravityDiscovery_DirectRunAppendedToOwnedLog(t *testing.T) {
	h := helperIsolateLogIndex(t)
	now := time.Now().Add(-time.Hour)
	name := helperLogName(now)
	child := beginAntigravityOwnChild(h.home)
	child.setPID(6001)
	path := h.write(t, name, helperPIDBlock(6001), now, false)
	child.done()
	h.pass(now.Add(time.Second), 0)
	if class, _ := helperEntryClass(path); class != antigravityLogOwned {
		t.Fatalf("class=%v, want owned", class)
	}

	h.setLive(6002, true)
	h.write(t, name, helperPIDBlock(6002), now.Add(2*time.Second), true)
	if res := h.pass(now.Add(30*time.Second), 0); !res.owed.IsZero() {
		t.Fatalf("owed=%s while the direct run is live", res.owed)
	}
	if class, _ := helperEntryClass(path); class != antigravityLogCandidate {
		t.Fatalf("class=%v, want the grown owned file reclassified as a candidate", class)
	}
	h.setLive(6002, false)
	if res := h.pass(now.Add(5*time.Minute), 0); res.owed.IsZero() {
		t.Error("the appended direct run was never owed after it exited")
	}
}

/* ─────────────────────────── caps and grace ─────────────────────────── */

// A burst of 40 owned logs keeps exactly 32 owned; the 8 evicted stay in the
// collision grace. A same-second direct block appended to the oldest evicted
// file inside the grace is still found; after the grace the entry is gone.
func TestAntigravityDiscovery_OwnedBurstGraceAndCollision(t *testing.T) {
	h := helperIsolateLogIndex(t)
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	var paths []string
	for i := 0; i < 40; i++ {
		at := start.Add(time.Duration(i) * time.Second)
		child := beginAntigravityOwnChild(h.home)
		child.setPID(7000 + i)
		paths = append(paths, h.write(t, helperLogName(at), helperPIDBlock(7000+i), at, false))
		child.done()
	}
	now := start.Add(41 * time.Second)
	h.pass(now, 0)
	owned, _, _, grace, _, _ := helperIndexCounts()
	if owned != antigravityOwnedCap || grace != 8 {
		t.Fatalf("owned=%d grace=%d, want %d owned and 8 in grace", owned, grace, antigravityOwnedCap)
	}

	oldest := paths[0]
	h.setLive(7999, true)
	h.write(t, filepath.Base(oldest), helperPIDBlock(7999), start.Add(time.Second), true)
	h.pass(now.Add(10*time.Second), 0)
	if class, _ := helperEntryClass(oldest); class != antigravityLogCandidate {
		t.Fatalf("class=%v, want the evicted file reclassified inside its grace", class)
	}
	h.setLive(7999, false)
	res := h.pass(now.Add(20*time.Second), 0)
	res = h.pass(now.Add(20*time.Second+antigravityCandidateSettle), 0)
	if res.owed.IsZero() {
		t.Error("the collided direct run was never owed")
	}

	// Past the grace window the other evicted files are dropped.
	h.pass(start.Add(antigravityCollisionGrace+2*time.Minute), 0)
	if _, _, _, grace, _, _ := helperIndexCounts(); grace != 0 {
		t.Errorf("grace=%d after the collision window, want 0", grace)
	}
}

// 40 noPID logs: 32 kept, 8 in grace; one that gains a PID block inside its
// grace becomes a candidate; the rest are owed on the idle rule.
func TestAntigravityDiscovery_NoPIDCapAndGrace(t *testing.T) {
	h := helperIsolateLogIndex(t)
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	var paths []string
	for i := 0; i < 40; i++ {
		at := start.Add(time.Duration(i) * time.Second)
		paths = append(paths, h.write(t, helperLogName(at), "I0929 starting\n", at.Add(50*time.Second), false))
	}
	now := start.Add(41 * time.Second)
	h.pass(now, 0)
	if _, _, noPID, grace, _, _ := helperIndexCounts(); noPID != antigravityNoPIDCap || grace != 8 {
		t.Fatalf("noPID=%d grace=%d, want %d and 8", noPID, grace, antigravityNoPIDCap)
	}
	h.setLive(8001, true)
	h.write(t, filepath.Base(paths[0]), helperPIDBlock(8001), now.Add(5*time.Second), true)
	h.pass(now.Add(10*time.Second), 0)
	if class, _ := helperEntryClass(paths[0]); class != antigravityLogCandidate {
		t.Errorf("class=%v, want the grace noPID file promoted to a candidate", class)
	}
	res := h.pass(start.Add(antigravityCollisionGrace+5*time.Minute), 0)
	if res.owed.IsZero() {
		t.Error("the noPID logs were dropped without being owed")
	}
}

// The grace list itself is capped, oldest dropped first.
func TestAntigravityDiscovery_GraceListCapped(t *testing.T) {
	h := helperIsolateLogIndex(t)
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	for i := 0; i < antigravityOwnedCap+antigravityGraceCap+10; i++ {
		at := start.Add(time.Duration(i) * time.Second)
		child := beginAntigravityOwnChild(h.home)
		child.setPID(9000 + i)
		h.write(t, helperLogName(at), helperPIDBlock(9000+i), at, false)
		child.done()
	}
	h.pass(start.Add(90*time.Second), 0)
	if _, _, _, grace, _, _ := helperIndexCounts(); grace > antigravityGraceCap {
		t.Errorf("grace=%d, want at most %d", grace, antigravityGraceCap)
	}
}

// ManyLiveDirectCandidates: 10 live runs keep 8 candidates and move 2 to
// process-only tracking, still armed; exiting later, they are owed.
func TestAntigravityDiscovery_ManyLiveCandidates(t *testing.T) {
	h := helperIsolateLogIndex(t)
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	for i := 0; i < 10; i++ {
		at := start.Add(time.Duration(i) * time.Second)
		h.setLive(10000+i, true)
		h.write(t, helperLogName(at), helperPIDBlock(10000+i), at, false)
	}
	h.pass(start.Add(20*time.Second), 0)
	_, candidates, _, _, processOnly, _ := helperIndexCounts()
	if candidates != antigravityCandidateCap || processOnly != 2 {
		t.Fatalf("candidates=%d processOnly=%d, want %d and 2", candidates, processOnly, antigravityCandidateCap)
	}
	if antigravityOldestProtectedFloorMs() == 0 {
		t.Error("evicted live runs lost their protected floor")
	}
	for i := 0; i < 10; i++ {
		h.setLive(10000+i, false)
	}
	h.pass(start.Add(30*time.Second), 0)
	if res := h.pass(start.Add(30*time.Second+antigravityCandidateSettle), 0); res.owed.IsZero() {
		t.Error("the exited runs were never owed")
	}
	if _, _, _, _, processOnly, _ := helperIndexCounts(); processOnly != 0 {
		t.Errorf("processOnly=%d after every run exited", processOnly)
	}
}

// 70 live runs overflow process-only tracking into the sentinel, which is
// owed only on an ok checked scan with no untracked agy — never on a failed
// one.
func TestAntigravityDiscovery_SentinelNeedsAnOKScan(t *testing.T) {
	h := helperIsolateLogIndex(t)
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	for i := 0; i < 70+antigravityCandidateCap; i++ {
		at := start.Add(time.Duration(i) * time.Second)
		h.setLive(20000+i, true)
		h.write(t, helperLogName(at), helperPIDBlock(20000+i), at, false)
	}
	scanOK := false
	antigravityProcessScan = func() ([]ProcessInfo, bool) { return nil, scanOK }
	now := start.Add(2 * time.Minute)
	h.pass(now, 0)
	if _, _, _, _, processOnly, sentinel := helperIndexCounts(); processOnly != antigravityProcessOnlyCap || !sentinel {
		t.Fatalf("processOnly=%d sentinel=%v, want the cap and a sentinel", processOnly, sentinel)
	}
	h.pass(now.Add(time.Second), 0)
	if _, _, _, _, _, sentinel := helperIndexCounts(); !sentinel {
		t.Fatal("a failed scan released the sentinel")
	}
	scanOK = true
	res := h.pass(now.Add(2*time.Second), 0)
	_ = res
	if _, _, _, _, _, sentinel := helperIndexCounts(); sentinel {
		t.Error("an ok scan with no untracked agy kept the sentinel")
	}
	lockAntigravityLogIndex()
	owed := antigravityLogIndex.pendingOwed
	unlockAntigravityLogIndex()
	if owed.IsZero() {
		t.Error("the released sentinel owed nothing")
	}
}

/* ─────────────────────────── startup scan ─────────────────────────── */

// The first pass classifies names at or after the cached reading less the DST
// slack, at most the newest antigravityStartupScanCap.
func TestAntigravityStartupScan_DSTSlackAndCap(t *testing.T) {
	h := helperIsolateLogIndex(t)
	observed := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	inside := observed.Add(-30 * time.Minute)
	outside := observed.Add(-2 * time.Hour)
	in := h.write(t, helperLogName(inside), "I0929 starting\n", observed.Add(time.Minute), false)
	out := h.write(t, helperLogName(outside), "I0929 starting\n", observed.Add(time.Minute), false)
	h.pass(time.Now(), observed.UnixMilli())
	if _, tracked := helperEntryClass(in); !tracked {
		t.Error("a name inside the DST slack was not classified")
	}
	if _, tracked := helperEntryClass(out); tracked {
		t.Error("a name older than the reading less the slack was classified")
	}

	// NeverObservedRunWhileAgentDown with 300 logs: only the newest 256 are
	// examined, so the oldest is the documented miss.
	h2 := helperIsolateLogIndex(t)
	first := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	var oldest string
	for i := 0; i < 300; i++ {
		at := first.Add(time.Duration(i) * time.Second)
		p := h2.write(t, helperLogName(at), "I0929 starting\n", at, false)
		if i == 0 {
			oldest = p
		}
	}
	h2.pass(time.Now(), 0)
	if _, tracked := helperEntryClass(oldest); tracked {
		t.Error("the startup scan examined more than its cap")
	}
}

// NeverObservedRunWhileAgentDown: a direct run that started and ended while no
// agent was running is found by the startup scan and owed.
func TestAntigravityStartupScan_FindsARunThatEndedWhileDown(t *testing.T) {
	h := helperIsolateLogIndex(t)
	observed := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	ran := observed.Add(30 * time.Minute)
	h.write(t, helperLogName(ran), helperPIDBlock(30001), ran.Add(time.Minute), false)
	res := h.pass(time.Now(), observed.UnixMilli())
	if res.owed.IsZero() {
		t.Error("the run that ended while the agent was down was never owed")
	}
}

/* ─────────────────────────── tick ─────────────────────────── */

func TestAntigravityDiscovery_TickStartsAndStops(t *testing.T) {
	helperIsolateLogIndex(t)
	orig := antigravityDiscoveryInterval
	antigravityDiscoveryInterval = 5 * time.Millisecond
	defer func() { antigravityDiscoveryInterval = orig }()
	startAntigravityDiscovery()
	startAntigravityDiscovery() // no second ticker
	if !antigravityDiscoveryRunning() {
		t.Fatal("the tick did not start")
	}
	stopAntigravityDiscovery()
	if antigravityDiscoveryRunning() {
		t.Error("the tick is still running after stop")
	}
	stopAntigravityDiscovery() // idempotent
}

/* ─────────────────────────── lock order ─────────────────────────── */

// Arm, settle (cache → freshness → live-runs), rollback and discovery run
// concurrently under -race; the assertion hook panics if any path takes the
// live-runs or freshness lock while holding the index lock.
func TestAntigravityLogIndex_LockOrder(t *testing.T) {
	h := helperIsolateLogIndex(t)
	antigravityLockOrderCheck.Store(true)
	defer antigravityLockOrderCheck.Store(false)
	now := time.Now().Add(-time.Hour)
	for i := 0; i < 6; i++ {
		h.setLive(40000+i, i%2 == 0)
		h.write(t, helperLogName(now.Add(time.Duration(i)*time.Second)), helperPIDBlock(40000+i), now, false)
	}
	var wg sync.WaitGroup
	var panics atomic.Int64
	run := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if recover() != nil {
					panics.Add(1)
				}
			}()
			fn()
		}()
	}
	for i := 0; i < 20; i++ {
		run(func() { h.pass(now.Add(time.Duration(i)*time.Second), 0) })
		run(func() {
			floor := armAntigravityUsageRunFloor(time.Now())
			antigravityUsageRunSettled(floor, 0, false, true)
		})
		run(func() { saveAntigravityQuotaSnapshotIfNewer(helperAntigravitySnapshotAt(time.Now())) })
		run(func() {
			armAntigravityCandidateFloor(time.Now().UnixMilli())
			releaseAntigravityCandidateFloor(time.Now().UnixMilli())
		})
		run(func() { child := beginAntigravityOwnChild(h.home); child.setPID(41000 + i); child.done() })
	}
	wg.Wait()
	helperStopAntigravityRefreshSchedule()
	if panics.Load() != 0 {
		t.Fatalf("%d paths took the live-runs or freshness lock under the index lock", panics.Load())
	}
}

// The hook itself fires when the order is violated.
func TestAntigravityLogIndex_LockOrderHookFires(t *testing.T) {
	antigravityLockOrderCheck.Store(true)
	defer antigravityLockOrderCheck.Store(false)
	defer func() {
		if recover() == nil {
			t.Error("taking live-runs under the index lock did not panic")
		}
	}()
	lockAntigravityLogIndex()
	defer unlockAntigravityLogIndex()
	antigravityOldestLiveRunFloorMs()
}

func helperAntigravitySnapshotAt(at time.Time) antigravityQuotaSnapshot {
	return antigravityQuotaSnapshot{
		ObservedAt: at.UTC().Format(time.RFC3339), ObservedAtMs: at.UnixMilli(),
		AccountFingerprint: fingerprintAccount("antigravity", "ada@example.com"), Account: "ada@example.com",
		Buckets: []antigravityQuotaBucket{{BucketID: "b", Window: "5h", RemainingFraction: 0.5}},
	}
}

/* ─────────────────────────── PID blocks ─────────────────────────── */

// Two runs that share a second-stamped file are kept apart by their blocks,
// and logs named before the floor (less the slack) are not read.
func TestAntigravityPIDBlock_OnlyTheRequestedRun(t *testing.T) {
	h := helperIsolateLogIndex(t)
	now := time.Now().Truncate(time.Second)
	h.write(t, helperLogName(now),
		helperPIDBlock(50001)+"authenticated successfully as ada@example.com\n"+
			helperPIDBlock(50002)+"authenticated successfully as bob@example.com\nRESOURCE_EXHAUSTED (code 429): quota. Resets in 1h39m21s.\n",
		now, false)
	old := now.Add(-3 * time.Hour)
	h.write(t, helperLogName(old), helperPIDBlock(50003), old, false)

	block, ok := antigravityPIDBlock(h.base, 50001, now)
	if !ok || !strings.Contains(string(block), "ada@example.com") || strings.Contains(string(block), "bob@") {
		t.Errorf("block=%q ok=%v, want only pid 50001's block", block, ok)
	}
	block, ok = antigravityPIDBlock(h.base, 50002, now)
	if !ok || !strings.Contains(string(block), "RESOURCE_EXHAUSTED") || strings.Contains(string(block), "ada@") {
		t.Errorf("block=%q ok=%v, want only pid 50002's block", block, ok)
	}
	if _, ok := antigravityPIDBlock(h.base, 50003, now); ok {
		t.Error("a log named before the floor was read")
	}
}

// Unstamped legacy names sort after `cli-…` lexically. At least
// antigravityWatchedNewest of them must not push the current stamped run out
// of the newest-N cap, for the run-log listing (port discovery) or for a
// managed run's PID block.
func TestAntigravityLogNames_StampedLeadLegacy(t *testing.T) {
	h := helperIsolateLogIndex(t)
	now := time.Now().Truncate(time.Second)
	old := now.Add(-48 * time.Hour)
	for i := 0; i < antigravityWatchedNewest+8; i++ {
		h.write(t, fmt.Sprintf("legacy-%03d.log", i), "", old, false)
	}
	current := h.write(t, helperLogName(now), helperPIDBlock(50011), now, false)

	names, ok := antigravityListLogNames(h.base)
	if !ok || len(names) == 0 || names[0] != filepath.Base(current) {
		t.Fatalf("names[0]=%v, want the stamped run %q first", names, filepath.Base(current))
	}
	found := false
	for _, file := range antigravityRunLogs(h.base) {
		found = found || file.path == current
	}
	if !found {
		t.Error("antigravityRunLogs dropped the stamped run behind legacy names")
	}
	if _, ok := antigravityPIDBlock(h.base, 50011, now); !ok {
		t.Error("antigravityPIDBlock did not reach the stamped run behind legacy names")
	}
}

// The evidence a block yields: the account and each reset, a reset anchored
// to its line's own glog time when present.
func TestAntigravityBlockEvidence(t *testing.T) {
	at := time.Date(2026, 9, 29, 21, 0, 0, 0, time.Local)
	block := []byte("I0929 20:43:50.000000 1 auth.go:1] authenticated successfully as ada@example.com.\n" +
		"E0929 20:43:56.000000 1 api.go:9] RESOURCE_EXHAUSTED (code 429): You have exhausted your capacity. Resets in 1h39m21s.\n" +
		"RESOURCE_EXHAUSTED (code 429): throttled. Resets in 30s.\n")
	email, resets := antigravityBlockEvidence(block, at)
	if email != "ada@example.com" {
		t.Errorf("email=%q", email)
	}
	if len(resets) != 2 {
		t.Fatalf("resets=%v, want two", resets)
	}
	want := time.Date(2026, 9, 29, 20, 43, 56, 0, time.Local).Add(time.Hour + 39*time.Minute + 21*time.Second)
	if !resets[0].Equal(want) {
		t.Errorf("reset=%s, want %s (anchored to the line)", resets[0], want)
	}
	if !resets[1].Equal(at.Add(30 * time.Second)) {
		t.Errorf("reset=%s, want the settle time plus 30 s for an unstamped line", resets[1])
	}
}

// withProcessStartHook carries a hook to a runner; without one the runner's
// call is a no-op.
func TestProcessStartHook(t *testing.T) {
	var got int
	processStartHookFrom(withProcessStartHook(context.Background(), func(pid int) { got = pid }))(42)
	if got != 42 {
		t.Errorf("hook got %d", got)
	}
	processStartHookFrom(context.Background())(7) // no hook: no panic
	processStartHookFrom(nil)(7)
}

// The gather's pass never runs the sentinel's process scan (it would spend the
// shared gather budget on a PowerShell child); the tick's pass does.
func TestAntigravityNewestOwedLog_NeverRunsTheSentinelScan(t *testing.T) {
	h := helperIsolateLogIndex(t)
	var scans atomic.Int64
	antigravityProcessScan = func() ([]ProcessInfo, bool) { scans.Add(1); return nil, false }
	lockAntigravityLogIndex()
	antigravityLogIndex.sentinel = &antigravitySentinel{firstSeen: time.Now()}
	unlockAntigravityLogIndex()

	antigravityNewestOwedLog([]string{h.base}, time.Now())
	if scans.Load() != 0 {
		t.Errorf("the gather's pass ran %d process scans, want none", scans.Load())
	}
	h.pass(time.Now(), 0)
	if scans.Load() != 1 {
		t.Errorf("the tick's pass ran %d process scans, want one", scans.Load())
	}
}

// Unstamped (legacy) names never crowd the stamped ones out of the newest
// watched names: a settled stamped log appended to after 40 unstamped logs
// exist is still re-stat'ed and reclassified.
func TestAntigravityDiscovery_UnstampedNamesDoNotCrowdTheWatchedSet(t *testing.T) {
	h := helperIsolateLogIndex(t)
	now := time.Now().Add(-time.Hour).Truncate(time.Second)
	// A managed run's log: settled, so only the newest-name watch re-stats it.
	noteAntigravityManagedPID(70001)
	stamped := h.write(t, helperLogName(now), helperPIDBlock(70001), now, false)
	for i := 0; i < 40; i++ {
		h.write(t, fmt.Sprintf("legacy-%02d.log", i), "I0929 starting\n", now, false)
	}
	h.pass(now.Add(time.Second), now.Add(time.Hour).UnixMilli())

	h.setLive(70002, true)
	h.write(t, filepath.Base(stamped), helperPIDBlock(70002), now.Add(2*time.Second), true)
	h.pass(now.Add(10*time.Second), now.Add(time.Hour).UnixMilli())
	if class, _ := helperEntryClass(stamped); class != antigravityLogCandidate {
		t.Errorf("class=%v, want the stamped log still watched and reclassified", class)
	}
}

// A remembered PID whose start token cannot be read matches a log only when
// that log's name-second is strictly before the second the remembered process
// exited in. A log named in the SAME second cannot be told apart from a user
// run that reused the PID inside it, so it must be read as foreign.
func TestAntigravityPIDIn_SameSecondReuseIsForeign(t *testing.T) {
	exited := time.Date(2026, 9, 30, 12, 0, 5, 400_000_000, time.UTC)
	ring := []antigravityTrackedPID{{pid: 4242, token: "start-1", exitedAt: exited}}
	sameSecond := exited.Truncate(time.Second)

	for _, tc := range []struct {
		name  string
		token string
		at    time.Time
		want  bool
	}{
		{"same token is always ours", "start-1", sameSecond, true},
		{"another token is never ours", "start-2", sameSecond, false},
		{"unknown token, earlier second", "", sameSecond.Add(-time.Second), true},
		{"unknown token, exit second", "", sameSecond, false},
		{"unknown token, later second", "", sameSecond.Add(time.Second), false},
		{"unknown token, live now inside the exit second", "", exited.Add(300 * time.Millisecond), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := antigravityPIDIn(ring, 4242, tc.token, tc.at); got != tc.want {
				t.Fatalf("antigravityPIDIn=%v, want %v", got, tc.want)
			}
		})
	}

	live := []antigravityTrackedPID{{pid: 4242}}
	if !antigravityPIDIn(live, 4242, "", sameSecond) {
		t.Error("a remembered run that has not exited stopped matching")
	}
}
