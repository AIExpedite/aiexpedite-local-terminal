package main

// Tests for keep_awake.go: one OS keep-awake lock, reference counted by
// session id, taken at the spawn-ledger helpers and released when the last
// session ends, after the per-session cap, or at shutdown.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeKeepAwake counts OS lock acquisitions and releases, and drives the
// per-session cap with a manual clock.
type fakeKeepAwake struct {
	mu        sync.Mutex
	acquired  int
	released  int
	failWith  error
	onLost    func(error) // the latest acquisition's
	logs      []string
	timers    map[int]*fakeKeepAwakeTimer
	nextTimer int
}

type fakeKeepAwakeTimer struct {
	d       time.Duration
	fire    func()
	stopped bool
}

func (f *fakeKeepAwake) holding() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acquired > f.released
}

func (f *fakeKeepAwake) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acquired, f.released
}

// fireAll runs every live cap timer (the 12 h elapsing).
func (f *fakeKeepAwake) fireAll() {
	f.mu.Lock()
	var fires []func()
	for _, tm := range f.timers {
		if !tm.stopped {
			tm.stopped = true
			fires = append(fires, tm.fire)
		}
	}
	f.mu.Unlock()
	for _, fire := range fires {
		fire()
	}
}

func newFakeKeepAwakeManager(f *fakeKeepAwake) *keepAwakeManager {
	m := newKeepAwakeManager()
	f.timers = map[int]*fakeKeepAwakeTimer{}
	m.disabled = func() bool { return false }
	m.acquire = func(onLost func(error)) (func(), error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failWith != nil {
			return nil, f.failWith
		}
		f.acquired++
		f.onLost = onLost
		return func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.released++
		}, nil
	}
	m.afterFunc = func(d time.Duration, fire func()) func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.nextTimer++
		tm := &fakeKeepAwakeTimer{d: d, fire: fire}
		f.timers[f.nextTimer] = tm
		return func() bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			was := !tm.stopped
			tm.stopped = true
			return was
		}
	}
	m.logf = func(format string, args ...any) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.logs = append(f.logs, fmt.Sprintf(format, args...))
	}
	return m
}

// helperFakeKeepAwake swaps the global manager for a fake for one test.
func helperFakeKeepAwake(t *testing.T) *fakeKeepAwake {
	t.Helper()
	f := &fakeKeepAwake{}
	prev := globalKeepAwake
	globalKeepAwake = newFakeKeepAwakeManager(f)
	t.Cleanup(func() { globalKeepAwake = prev })
	return f
}

func TestKeepAwakeReferenceCountsSessions(t *testing.T) {
	f := &fakeKeepAwake{}
	m := newFakeKeepAwakeManager(f)

	m.Hold("a")
	m.Hold("b")
	m.Hold("a") // idempotent
	if acquired, _ := f.counts(); acquired != 1 {
		t.Fatalf("acquired %d times, want 1 for the first hold only", acquired)
	}
	m.Release("a")
	if !f.holding() {
		t.Fatal("releasing one of two sessions let the computer sleep")
	}
	m.Release("b")
	if f.holding() {
		t.Fatal("the last release kept the computer awake")
	}
	if acquired, released := f.counts(); acquired != 1 || released != 1 {
		t.Fatalf("acquired %d / released %d, want 1 / 1", acquired, released)
	}

	// A new session after an idle spell takes the lock again.
	m.Hold("c")
	if acquired, _ := f.counts(); acquired != 2 || !f.holding() {
		t.Fatalf("a new session did not re-take the lock (acquired %d)", acquired)
	}
}

func TestKeepAwakeReleaseOfUnknownOrRepeatedIDIsANoOp(t *testing.T) {
	f := &fakeKeepAwake{}
	m := newFakeKeepAwakeManager(f)
	m.Release("never-held")
	m.Release("")
	m.Hold("")
	if acquired, released := f.counts(); acquired != 0 || released != 0 {
		t.Fatalf("acquired %d / released %d for no session", acquired, released)
	}
	m.Hold("a")
	m.Release("a")
	m.Release("a")
	if _, released := f.counts(); released != 1 {
		t.Fatalf("a duplicate release released %d times", released)
	}
}

func TestKeepAwakeCapDropsAForgottenSession(t *testing.T) {
	f := &fakeKeepAwake{}
	m := newFakeKeepAwakeManager(f)
	m.Hold("forgotten")
	for _, tm := range f.timers {
		if tm.d != keepAwakeSessionCap {
			t.Fatalf("cap timer %s, want %s", tm.d, keepAwakeSessionCap)
		}
	}
	f.fireAll()
	if f.holding() || m.heldCount() != 0 {
		t.Fatal("a session past the cap still holds the computer awake")
	}
	// The late Release of the expired session is harmless.
	m.Release("forgotten")
	if _, released := f.counts(); released != 1 {
		t.Fatalf("released %d times, want 1", released)
	}
}

func TestKeepAwakeCapOfAnEarlierHoldNeverEndsAReHold(t *testing.T) {
	f := &fakeKeepAwake{}
	m := newFakeKeepAwakeManager(f)
	m.Hold("a")
	var first func()
	for _, tm := range f.timers {
		first = tm.fire
	}
	m.Release("a")
	m.Hold("a")
	// The first hold's timer fires late (it lost the race with Stop).
	first()
	if !f.holding() || m.heldCount() != 1 {
		t.Fatal("the earlier hold's cap released the new hold")
	}
}

func TestKeepAwakeOffSwitchNeverAcquires(t *testing.T) {
	for _, value := range []string{"off", "0", "FALSE", " no "} {
		t.Setenv(keepAwakeEnvVar, value)
		f := &fakeKeepAwake{}
		m := newFakeKeepAwakeManager(f)
		m.disabled = keepAwakeDisabledByEnv
		m.Hold("a")
		if acquired, _ := f.counts(); acquired != 0 {
			t.Fatalf("%s=%q still took the lock", keepAwakeEnvVar, value)
		}
	}
	t.Setenv(keepAwakeEnvVar, "on")
	if keepAwakeDisabledByEnv() {
		t.Fatal("any other value must leave keep-awake on")
	}
}

func TestKeepAwakeAcquireErrorIsLoggedOnceAndHoldReturns(t *testing.T) {
	f := &fakeKeepAwake{failWith: errors.New("caffeinate: not found")}
	m := newFakeKeepAwakeManager(f)
	m.Hold("a")
	m.Hold("b")
	warnings := 0
	for _, line := range f.logs {
		if strings.Contains(line, "Could not keep the computer awake") && strings.Contains(line, "caffeinate: not found") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatalf("logged the failure %d times, want once: %q", warnings, f.logs)
	}
	m.Release("a")
	m.Release("b") // no lock to release: no panic
	if m.heldCount() != 0 {
		t.Fatal("sessions still held after release")
	}
}

func TestKeepAwakeReleaseAll(t *testing.T) {
	f := &fakeKeepAwake{}
	m := newFakeKeepAwakeManager(f)
	m.Hold("a")
	m.Hold("b")
	m.ReleaseAll()
	if f.holding() || m.heldCount() != 0 {
		t.Fatal("ReleaseAll left the computer awake")
	}
}

func TestKeepAwakeConcurrentHoldsAndReleases(t *testing.T) {
	f := &fakeKeepAwake{}
	m := newFakeKeepAwakeManager(f)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("s%d", i%5)
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Hold(id)
			m.Release(id)
		}()
	}
	wg.Wait()
	if f.holding() || m.heldCount() != 0 {
		t.Fatal("concurrent holds and releases left the lock held")
	}
}

// The spawn-ledger helpers every manager already calls hold and release.
func TestKeepAwakeFollowsTheSpawnLedgerHelpers(t *testing.T) {
	f := helperFakeKeepAwake(t)
	dir := t.TempDir()
	prev := globalSpawnLedger
	globalSpawnLedger = newTestLedger(t, dir, "boot-1")
	t.Cleanup(func() { globalSpawnLedger = prev })

	beginSessionSpawn("s1", nil)
	if !f.holding() {
		t.Fatal("beginSessionSpawn did not hold the computer awake")
	}
	releaseLedgerSession("s1")
	if f.holding() {
		t.Fatal("releaseLedgerSession did not release")
	}

	// A start that failed for a session that never ran releases.
	beginSessionSpawn("s2", nil)
	abortSessionSpawn("s2")
	if f.holding() {
		t.Fatal("an aborted spawn of a never-started session kept the computer awake")
	}

	// A logical session (one process per turn) stays held across a failed turn.
	openLedgerLogicalSession("s3")
	beginSessionSpawn("s3", nil)
	abortSessionSpawn("s3")
	if !f.holding() {
		t.Fatal("a failed turn released a logical session that is still open")
	}
	releaseLedgerSession("s3")
	if f.holding() {
		t.Fatal("the logical session's release kept the computer awake")
	}
}

// An inhibitor child that exits on its own (systemd-inhibit with no system
// bus) is forgotten, so a later Hold takes the lock again.
func TestKeepAwakeLostLockIsRetakenByTheNextHold(t *testing.T) {
	f := &fakeKeepAwake{}
	m := newFakeKeepAwakeManager(f)
	m.Hold("a")
	lostFirst := f.onLost
	lostFirst(errors.New("systemd-inhibit exited: Failed to connect to bus"))
	warned := false
	for _, line := range f.logs {
		warned = warned || strings.Contains(line, "Failed to connect to bus")
	}
	if !warned {
		t.Fatalf("the lost lock was not logged: %q", f.logs)
	}

	m.Hold("b")
	if acquired, _ := f.counts(); acquired != 2 {
		t.Fatalf("acquired %d times, want a retry after the lock was lost", acquired)
	}
	// The first lock's late report never drops the new one.
	lostFirst(errors.New("late"))
	m.Release("a")
	if !f.holding() {
		t.Fatal("a stale lost report dropped the retaken lock")
	}
	m.Release("b")
	if acquired, released := f.counts(); acquired != 2 || released != 1 {
		t.Fatalf("acquired %d / released %d, want 2 / 1 (the dead lock is never released)", acquired, released)
	}
}

func TestHoldWhileChildRunsReportsAChildThatExitsOnItsOwn(t *testing.T) {
	lost := make(chan error, 1)
	release, err := holdWhileChildRuns(exec.Command(os.Args[0], "-test.unknown-flag-for-exit-error"),
		func(err error) { lost <- err })
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case err := <-lost:
		if !strings.Contains(err.Error(), "unknown-flag-for-exit-error") {
			t.Fatalf("lost error %q does not carry the child's stderr", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a child that exited was never reported lost")
	}
	release() // after the exit: returns, no panic
}
