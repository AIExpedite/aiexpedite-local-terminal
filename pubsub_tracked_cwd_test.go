package main

import (
	"fmt"
	"testing"
	"time"
)

// The tracked cwd is per sender, not per agent process (pubsub.go, "Tracked
// Working Directory"). Before this was keyed, one package variable held the
// only cwd on the device, so a `cd` from any one-off command moved the cwd for
// every later one whichever run or session had sent it — the case
// TestTrackedCwd_OneSendersCdDoesNotMoveAnother now pins.

func TestTrackedCwd_OneSendersCdDoesNotMoveAnother(t *testing.T) {
	resetTrackedCwds()
	t.Cleanup(resetTrackedCwds)

	runA := commandMsg{ID: "a1", ExecutionID: "exec-a"}
	runB := commandMsg{ID: "b1", ExecutionID: "exec-b"}

	// Run A cd'd somewhere. With a single package variable this was the whole
	// device's cwd, and run B's next command ran in it.
	setTrackedCwd(cwdScopeFor(runA), `C:\work\aixwt\feature-a\repo`)

	if got := getTrackedCwd(cwdScopeFor(runB)); got != "" {
		t.Fatalf("run B inherited run A's cwd: %q", got)
	}
	if got := getTrackedCwd(cwdScopeFor(runA)); got != `C:\work\aixwt\feature-a\repo` {
		t.Fatalf("run A lost its own cwd: %q", got)
	}
}

// Most specific first: an execution's own cwd beats its session's, which beats
// the process-wide default.
func TestTrackedCwd_ResolutionOrder(t *testing.T) {
	resetTrackedCwds()
	t.Cleanup(resetTrackedCwds)

	setTrackedCwd(cwdScope{}, `C:\default`)
	setTrackedCwd(cwdScope{sessionID: "sess-1"}, `C:\session`)
	setTrackedCwd(cwdScope{executionID: "exec-1"}, `C:\execution`)

	cases := []struct {
		name  string
		scope cwdScope
		want  string
	}{
		{"execution wins", cwdScope{executionID: "exec-1", sessionID: "sess-1"}, `C:\execution`},
		{"session next", cwdScope{sessionID: "sess-1"}, `C:\session`},
		{"unknown session falls through", cwdScope{sessionID: "sess-9"}, `C:\default`},
		{"unknown execution falls through to its session", cwdScope{executionID: "exec-9", sessionID: "sess-1"}, `C:\session`},
		{"no identity reads the default", cwdScope{}, `C:\default`},
	}
	for _, tc := range cases {
		if got := getTrackedCwd(tc.scope); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The current server sends neither id on a one-off `execute`, so those commands
// must still share the one process-wide cwd they always have — the fix is not
// allowed to change what today's server observes.
func TestTrackedCwd_CommandWithoutIdsUsesProcessWideDefault(t *testing.T) {
	resetTrackedCwds()
	t.Cleanup(resetTrackedCwds)

	first := commandMsg{ID: "c1"}
	second := commandMsg{ID: "c2"}

	setTrackedCwd(cwdScopeFor(first), `C:\repo`)
	if got := getTrackedCwd(cwdScopeFor(second)); got != `C:\repo` {
		t.Fatalf("a second identity-less command lost the tracked cwd: %q", got)
	}
}

// resolveWorkDir is where the tracked cwd actually reaches a command, so the
// scoping has to hold there too: the server re-sends the configured default
// every time, and step 2 prefers the sender's own tracked cwd over it.
func TestResolveWorkDir_PrefersTheSendersOwnTrackedCwd(t *testing.T) {
	resetTrackedCwds()
	t.Cleanup(resetTrackedCwds)

	cfg := &Config{WorkingDirectory: `C:\work`}
	setTrackedCwd(cwdScope{executionID: "exec-a"}, `C:\work\aixwt\feature-a\repo`)

	if got := resolveWorkDir(cfg, `C:\work`, cwdScope{executionID: "exec-a"}); got != `C:\work\aixwt\feature-a\repo` {
		t.Fatalf("run A did not get its own tracked cwd: %q", got)
	}
	if got := resolveWorkDir(cfg, `C:\work`, cwdScope{executionID: "exec-b"}); got != `C:\work` {
		t.Fatalf("run B should fall back to the configured default, got %q", got)
	}
}

// Nothing signals the end of an execution, so the lifetime is a TTL: an idle
// scope is swept, an active one is not. getTrackedCwd counts as activity.
func TestTrackedCwd_SweepEvictsIdleScopesOnly(t *testing.T) {
	resetTrackedCwds()
	t.Cleanup(resetTrackedCwds)

	setTrackedCwd(cwdScope{executionID: "idle"}, `C:\idle`)
	setTrackedCwd(cwdScope{executionID: "busy"}, `C:\busy`)

	// Age both, then touch one through a read. Two minutes rather than the real
	// TTL so the background cleanup goroutine — which may tick mid-test — can
	// never be the thing that evicts either entry.
	trackedCwdLock.Lock()
	for _, entry := range trackedCwds {
		entry.lastAccess = time.Now().Add(-2 * time.Minute)
	}
	trackedCwdLock.Unlock()
	getTrackedCwd(cwdScope{executionID: "busy"})

	if removed := sweepTrackedCwdsBefore(time.Now().Add(-time.Minute)); removed != 1 {
		t.Fatalf("expected exactly the idle scope to be swept, removed %d", removed)
	}
	if got := getTrackedCwd(cwdScope{executionID: "idle"}); got != "" {
		t.Fatalf("idle scope survived the sweep: %q", got)
	}
	if got := getTrackedCwd(cwdScope{executionID: "busy"}); got != `C:\busy` {
		t.Fatalf("active scope was swept: %q", got)
	}
}

// Between sweeps the map still has a hard bound, so a burst of scopes cannot
// grow it for the life of the agent.
func TestTrackedCwd_InsertsAreBoundedByMaxEntries(t *testing.T) {
	resetTrackedCwds()
	t.Cleanup(resetTrackedCwds)

	for i := 0; i < trackedCwdMaxEntries+50; i++ {
		setTrackedCwd(cwdScope{executionID: fmt.Sprintf("exec-%d", i)}, fmt.Sprintf(`C:\d%d`, i))
	}

	trackedCwdLock.Lock()
	size := len(trackedCwds)
	trackedCwdLock.Unlock()
	if size > trackedCwdMaxEntries {
		t.Fatalf("tracked cwd map grew past its bound: %d > %d", size, trackedCwdMaxEntries)
	}
	// The sender that just wrote must be one of the survivors.
	last := fmt.Sprintf("exec-%d", trackedCwdMaxEntries+49)
	if got := getTrackedCwd(cwdScope{executionID: last}); got == "" {
		t.Fatalf("the most recent scope was evicted instead of an older one")
	}
}

// At the bound it is the least recently used scope that goes, so a run that is
// still sending commands never loses its cwd to a burst of new senders.
func TestTrackedCwd_EvictionDropsTheLeastRecentlyUsed(t *testing.T) {
	resetTrackedCwds()
	t.Cleanup(resetTrackedCwds)

	for i := 0; i < trackedCwdMaxEntries; i++ {
		setTrackedCwd(cwdScope{executionID: fmt.Sprintf("exec-%d", i)}, fmt.Sprintf(`C:\d%d`, i))
	}
	// Make one entry unambiguously the oldest rather than relying on the clock
	// resolution across a tight insert loop. Well inside the TTL, so the
	// background cleanup goroutine is not what removes it.
	trackedCwdLock.Lock()
	trackedCwds["exec:exec-7"].lastAccess = time.Now().Add(-2 * time.Minute)
	trackedCwdLock.Unlock()

	setTrackedCwd(cwdScope{executionID: "newcomer"}, `C:\new`)

	if got := getTrackedCwd(cwdScope{executionID: "exec-7"}); got != "" {
		t.Fatalf("the least recently used scope survived eviction: %q", got)
	}
	if got := getTrackedCwd(cwdScope{executionID: "newcomer"}); got != `C:\new` {
		t.Fatalf("the new scope was not recorded: %q", got)
	}
}
