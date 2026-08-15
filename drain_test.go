// File: drain_test.go
package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// resetDrainState returns the package drain gate to a clean state between tests.
func resetDrainState(t *testing.T) {
	t.Helper()
	drain.mu.Lock()
	drain.draining = false
	drain.installing = false
	drain.attemptID = ""
	drain.registering = false
	drain.pendingStarts = 0
	drain.operationalCommands = 0
	drain.continuationCommands = 0
	drain.uploads = 0
	drain.approvals = 0
	drain.terminalPublishes = 0
	drain.mu.Unlock()
	drainWorkSourcesMu.Lock()
	drainWorkSources = nil
	drainWorkSourcesMu.Unlock()
}

func TestRegistrationAndDrainEntryAreMutuallyExclusive(t *testing.T) {
	resetDrainState(t)
	t.Cleanup(func() { resetDrainState(t) })

	if !beginRegistration() {
		t.Fatal("idle agent should begin registration")
	}
	if closeAdmission("attempt-during-registration") {
		t.Fatal("drain must not begin after registration owns the boundary")
	}
	if isDraining() {
		t.Fatal("failed drain claim must leave admission open")
	}

	endRegistration()
	if !closeAdmission("attempt-wins") {
		t.Fatal("drain should begin after registration ends")
	}
	if beginRegistration() {
		t.Fatal("registration must not begin after draining owns the boundary")
	}
}

func TestSealDrainForInstallRefusesNewDemandCommands(t *testing.T) {
	resetDrainState(t)
	closeAdmission("attempt-install")
	t.Cleanup(func() { resetDrainState(t) })

	if !sealDrainForInstall() {
		t.Fatal("idle drain should be sealed for install")
	}
	for _, command := range []string{"__cli_usage_refresh__", "__env_inspect__"} {
		admitted, release := admitWork(commandMsg{Command: command})
		if admitted || release != nil {
			t.Fatalf("demand command %q entered after install seal", command)
		}
	}
	admitted, release := admitWork(commandMsg{Command: "__ping__"})
	if !admitted || release != nil {
		t.Fatal("ping should remain admitted after install seal")
	}
}

func TestContinuationCallbacksHoldDrainOpenThroughInstallSeal(t *testing.T) {
	resetDrainState(t)
	t.Cleanup(func() { resetDrainState(t) })

	// A continuation already executing when draining begins must prevent the
	// updater from observing an idle agent after its manager session disappears.
	admitted, release := admitWork(commandMsg{Type: "session_input"})
	if !admitted || release == nil {
		t.Fatal("continuation should be admitted and tracked before draining")
	}
	closeAdmission("attempt-continuation")
	if got := ActiveWork(); got != 1 {
		t.Fatalf("ActiveWork() = %d while continuation callback is running, want 1", got)
	}
	if sealDrainForInstall() {
		t.Fatal("install seal must wait for the continuation callback")
	}

	release()
	if !sealDrainForInstall() {
		t.Fatal("idle drain should seal after the continuation callback returns")
	}
	admitted, release = admitWork(commandMsg{Type: "codex_appserver_send"})
	if admitted || release != nil {
		t.Fatal("continuation must be refused after the install seal")
	}
}

func TestAdmitWork_TracksDemandCommandsDuringDrain(t *testing.T) {
	resetDrainState(t)
	closeAdmission("attempt-demand")
	t.Cleanup(func() { resetDrainState(t) })

	for _, command := range []string{"__cli_usage_refresh__", "__env_inspect__"} {
		t.Run(command, func(t *testing.T) {
			admitted, release := admitWork(commandMsg{Command: command})
			if !admitted || release == nil {
				t.Fatalf("demand command %q should be admitted and tracked", command)
			}
			if got := ActiveWork(); got != 1 {
				t.Fatalf("ActiveWork() = %d while %q is running, want 1", got, command)
			}
			release()
			if got := ActiveWork(); got != 0 {
				t.Fatalf("ActiveWork() = %d after %q completes, want 0", got, command)
			}
		})
	}

	admitted, release := admitWork(commandMsg{Command: "__ping__"})
	if !admitted || release != nil {
		t.Fatal("ping should remain admitted without holding the drain open")
	}
}

func TestActiveWork_SnapshotsManagerHandoffWithPendingStart(t *testing.T) {
	resetDrainState(t)
	t.Cleanup(func() { resetDrainState(t) })

	var managerCount atomic.Int32
	var sourceCalls atomic.Int32
	sourceSnapshotted := make(chan struct{})
	allowSourceReturn := make(chan struct{})
	registerDrainWorkSource(func() int {
		if sourceCalls.Add(1) != 1 {
			return int(managerCount.Load())
		}
		// Capture the manager count before the admitted start transfers into it,
		// then pause in the aggregate snapshot to make the handoff deterministic.
		snapshot := managerCount.Load()
		close(sourceSnapshotted)
		<-allowSourceReturn
		return int(snapshot)
	})

	admitted, release := admitWork(commandMsg{Type: "session_start"})
	if !admitted || release == nil {
		t.Fatal("session start should be admitted and tracked")
	}

	activeResult := make(chan int, 1)
	go func() { activeResult <- ActiveWork() }()
	<-sourceSnapshotted

	handoffDone := make(chan struct{})
	go func() {
		managerCount.Store(1) // manager owns the session before pending is released
		release()
		close(handoffDone)
	}()

	select {
	case <-handoffDone:
		t.Fatal("pending-start release must wait for the aggregate snapshot")
	case <-time.After(20 * time.Millisecond):
	}

	close(allowSourceReturn)
	if got := <-activeResult; got == 0 {
		t.Fatal("ActiveWork returned zero during pending-start manager handoff")
	}
	<-handoffDone
	if got := ActiveWork(); got != 1 {
		t.Fatalf("ActiveWork() after handoff = %d, want manager-owned count 1", got)
	}
}

func TestAdmitWork_WhileDraining(t *testing.T) {
	resetDrainState(t)
	closeAdmission("attempt-1")
	t.Cleanup(func() { resetDrainState(t) })

	cases := []struct {
		name    string
		cmd     commandMsg
		wantAdm bool
	}{
		// New work starts — refused while draining.
		{"one-shot execute (empty type)", commandMsg{Command: "ls", Type: ""}, false},
		{"one-shot execute (explicit)", commandMsg{Command: "ls", Type: "execute"}, false},
		{"session_start", commandMsg{Type: "session_start"}, false},
		{"codex_appserver_start", commandMsg{Type: "codex_appserver_start"}, false},
		{"claude_native_start", commandMsg{Type: "claude_native_start"}, false},
		{"grok_acp_start", commandMsg{Type: "grok_acp_start"}, false},
		{"antigravity_native_start", commandMsg{Type: "antigravity_native_start"}, false},
		{"unrecognised future type", commandMsg{Type: "quantum_start"}, false},
		{"unrecognised future non-start type", commandMsg{Type: "batch_run"}, false},

		// Continuation for already-accepted work — admitted.
		{"session_input", commandMsg{Type: "session_input"}, true},
		{"session_signal", commandMsg{Type: "session_signal"}, true},
		{"session_end", commandMsg{Type: "session_end"}, true},
		{"codex_appserver_send", commandMsg{Type: "codex_appserver_send"}, true},
		{"codex_appserver_end", commandMsg{Type: "codex_appserver_end"}, true},
		{"claude_native_send", commandMsg{Type: "claude_native_send"}, true},
		{"grok_acp_send", commandMsg{Type: "grok_acp_send"}, true},
		{"antigravity_native_end", commandMsg{Type: "antigravity_native_end"}, true},

		// Operational / demand traffic — always admitted.
		{"ping", commandMsg{Command: "__ping__"}, true},
		{"cli usage refresh", commandMsg{Command: "__cli_usage_refresh__"}, true},
		{"env inspect", commandMsg{Command: "__env_inspect__"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, release := admitWork(tc.cmd)
			if release != nil {
				defer release()
			}
			if got != tc.wantAdm {
				t.Fatalf("admitWork(%+v) = %v, want %v", tc.cmd, got, tc.wantAdm)
			}
		})
	}
}

func TestAdmitWork_NotDrainingAdmitsEverything(t *testing.T) {
	resetDrainState(t)
	t.Cleanup(func() { resetDrainState(t) })

	// A work start when not draining is admitted AND tracked as pending until
	// its release is called.
	admitted, release := admitWork(commandMsg{Type: "session_start"})
	if !admitted {
		t.Fatal("work start should be admitted when not draining")
	}
	if release == nil {
		t.Fatal("admitted work start must return a release func")
	}
	if got := ActiveWork(); got != 1 {
		t.Fatalf("ActiveWork() = %d, want 1 while start in flight", got)
	}
	release()
	if got := ActiveWork(); got != 0 {
		t.Fatalf("ActiveWork() = %d after release, want 0", got)
	}
	// release is idempotent.
	release()
	if got := ActiveWork(); got != 0 {
		t.Fatalf("ActiveWork() = %d after double release, want 0", got)
	}
}

func TestActiveWork_AggregatesEverySource(t *testing.T) {
	resetDrainState(t)
	t.Cleanup(func() { resetDrainState(t) })

	// Each independent source must hold the drain open on its own.
	sessions := 0
	registerDrainWorkSource(func() int { return sessions })

	if got := ActiveWork(); got != 0 {
		t.Fatalf("ActiveWork() = %d, want 0 initially", got)
	}

	sessions = 2
	if got := ActiveWork(); got != 2 {
		t.Fatalf("ActiveWork() = %d, want 2 with a manager source", got)
	}
	sessions = 0

	trackUploadStart()
	if got := ActiveWork(); got != 1 {
		t.Fatalf("ActiveWork() = %d, want 1 with an in-flight upload", got)
	}
	trackUploadEnd()

	trackApprovalStart()
	if got := ActiveWork(); got != 1 {
		t.Fatalf("ActiveWork() = %d, want 1 with an open approval dialog", got)
	}
	trackApprovalEnd()

	publishStarted := make(chan struct{})
	publishDone := make(chan struct{})
	publishTerminalResultAsync(func(resultMsg) {
		close(publishStarted)
		<-publishDone
	}, resultMsg{Type: "session_ended"})
	<-publishStarted
	if got := ActiveWork(); got != 1 {
		t.Fatalf("ActiveWork() = %d, want 1 with a terminal publish in flight", got)
	}
	close(publishDone)
	deadline := time.Now().Add(time.Second)
	for ActiveWork() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	if got := ActiveWork(); got != 0 {
		t.Fatalf("ActiveWork() = %d, want 0 after all work ends", got)
	}
}

// TestAdmitWork_CloseBoundaryRace asserts a work start arriving exactly at the
// close-admission boundary is either fully accepted-before-close (and holds the
// drain open) or refused — never accepted after the tracker reported zero. Run
// under -race.
func TestAdmitWork_CloseBoundaryRace(t *testing.T) {
	resetDrainState(t)
	t.Cleanup(func() { resetDrainState(t) })

	const n = 200
	var wg sync.WaitGroup
	var accepted int
	var mu sync.Mutex
	releases := make([]func(), 0, n)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			admittedOK, release := admitWork(commandMsg{Type: "session_start"})
			if admittedOK {
				mu.Lock()
				accepted++
				if release != nil {
					releases = append(releases, release)
				}
				mu.Unlock()
			}
		}
	}()

	// Close admission concurrently.
	closeAdmission("race-attempt")
	wg.Wait()

	// Any accepted start increased pendingStarts and must be reflected by
	// ActiveWork until released — i.e. the drain could not have completed with
	// accepted-but-uncounted work.
	mu.Lock()
	if got := ActiveWork(); got != accepted {
		t.Fatalf("ActiveWork()=%d but accepted=%d — accepted work not counted", got, accepted)
	}
	for _, r := range releases {
		r()
	}
	mu.Unlock()
	if got := ActiveWork(); got != 0 {
		t.Fatalf("ActiveWork()=%d after releasing all, want 0", got)
	}
}

func TestIsWorkStartCommand(t *testing.T) {
	starts := []commandMsg{
		{Command: "ls"}, {Type: "execute"}, {Type: "session_start"},
		{Type: "codex_appserver_start"}, {Type: "grok_acp_start"},
		{Type: "some_future_start"},
	}
	for _, c := range starts {
		if !isWorkStartCommand(c) {
			t.Errorf("isWorkStartCommand(%+v) = false, want true", c)
		}
	}
	notStarts := []commandMsg{
		{Command: "__ping__"}, {Command: "__cli_usage_refresh__"},
		{Command: "__env_inspect__"}, {Type: "session_input"},
		{Type: "codex_appserver_send"},
	}
	for _, c := range notStarts {
		if isWorkStartCommand(c) {
			t.Errorf("isWorkStartCommand(%+v) = true, want false", c)
		}
	}
}

func TestTrackedTerminalPublisherHoldsDrainUntilQueueFlushes(t *testing.T) {
	resetDrainState(t)
	t.Cleanup(func() { resetDrainState(t) })

	queue := make(chan resultMsg, 1)
	publishStarted := make(chan struct{})
	releasePublish := make(chan struct{})
	done := startTrackedTerminalPublisher(queue, func(resultMsg) {
		close(publishStarted)
		<-releasePublish
	})
	queue <- resultMsg{Type: "codex_appserver_message"}
	close(queue)
	<-publishStarted

	if got := ActiveWork(); got != 1 {
		t.Fatalf("ActiveWork() = %d, want 1 while queued stream publish is blocked", got)
	}
	close(releasePublish)
	<-done
	if got := ActiveWork(); got != 0 {
		t.Fatalf("ActiveWork() = %d, want 0 after queued stream publish flushes", got)
	}
}
