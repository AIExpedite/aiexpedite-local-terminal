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
		// An unknown session does NOT fall through to the unidentified pool.
		// setTrackedCwd writes only the most specific key, so this sender can
		// never have written "" — inheriting it would mean running in whatever
		// directory the last identity-less command happened to leave there.
		// Empty makes the caller use the cwd the server sent instead.
		{"unknown session does not inherit the unidentified cwd", cwdScope{sessionID: "sess-9"}, ""},
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
	// The senders already recorded keep their cwd: an insert into a map full of
	// LIVE scopes is declined, never traded for one of them (see
	// evictExpiredTrackedCwdLocked and
	// TestTrackedCwd_ALiveRunKeepsItsCwdThroughAFloodOfNewScopes).
	if got := getTrackedCwd(cwdScope{executionID: "exec-0"}); got != `C:\d0` {
		t.Fatalf("the first scope lost its cwd to the burst: %q", got)
	}
}

// At the bound it is an EXPIRED scope that goes, and a live one is kept even
// when it is the least recently used — a run that is still working must never
// lose its cwd to a burst of new senders, because its next command would then
// silently run somewhere else.
func TestTrackedCwd_EvictionDropsAnExpiredScopeAndSparesALiveOne(t *testing.T) {
	resetTrackedCwds()
	t.Cleanup(resetTrackedCwds)

	for i := 0; i < trackedCwdMaxEntries; i++ {
		setTrackedCwd(cwdScope{executionID: fmt.Sprintf("exec-%d", i)}, fmt.Sprintf(`C:\d%d`, i))
	}
	// Make one entry unambiguously the least recently used rather than relying
	// on the clock resolution across a tight insert loop. Well inside the TTL,
	// so it is still a live scope.
	trackedCwdLock.Lock()
	trackedCwds["exec:exec-7"].lastAccess = time.Now().Add(-2 * time.Minute)
	trackedCwdLock.Unlock()

	setTrackedCwd(cwdScope{executionID: "newcomer"}, `C:\new`)

	if got := getTrackedCwd(cwdScope{executionID: "exec-7"}); got != `C:\d7` {
		t.Fatalf("a live least-recently-used scope was evicted: %q", got)
	}
	if got := getTrackedCwd(cwdScope{executionID: "newcomer"}); got != "" {
		t.Fatalf("the newcomer should have been declined while every scope is live: %q", got)
	}

	// Age it past the TTL and the next insert reclaims it.
	trackedCwdLock.Lock()
	trackedCwds["exec:exec-7"].lastAccess = time.Now().Add(-2 * trackedCwdTTL)
	trackedCwdLock.Unlock()

	setTrackedCwd(cwdScope{executionID: "newcomer"}, `C:\new`)

	if got := getTrackedCwd(cwdScope{executionID: "exec-7"}); got != "" {
		t.Fatalf("the expired scope survived eviction: %q", got)
	}
	if got := getTrackedCwd(cwdScope{executionID: "newcomer"}); got != `C:\new` {
		t.Fatalf("the new scope was not recorded: %q", got)
	}
}

// A SIGNED cwd must win over any tracked one.
//
// Written against a tamper path that is now closed: while `executionID` arrived
// unsigned, an attacker with topic-write but no HMAC key could append one to a
// REPLAYED signed frame without disturbing its signature, and if the signed cwd
// happened to equal the configured default, the `cd`-persistence substitution
// would redirect the command to whatever that scope had remembered. The id is
// part of the signature canonical now (signaturePayload), so the scope selector
// is authenticated.
//
// The precedence asserted here is kept anyway, because it was never only an
// anti-tamper measure: a legitimate run whose remembered directory differs from
// the signed cwd must not move an approved, disk-gated step either. That is why
// cwdScope.cwdSigned and this case both survive the transition. Env-setup
// (RiskLevel) and read-only frames are the two that sign `cwd` — exactly
// signatureCwd's condition.
func TestResolveWorkDir_TrackedCwdNeverOverridesASignedCwd(t *testing.T) {
	resetTrackedCwds()
	t.Cleanup(resetTrackedCwds)

	cfg := &Config{WorkingDirectory: "C:/work"}

	// The scope has a remembered cwd, as if an earlier `cd` had run under it.
	poisoned := commandMsg{ID: "c1", ExecutionID: "exec-victim"}
	setTrackedCwd(cwdScopeFor(poisoned), "C:/attacker")
	if got := getTrackedCwd(cwdScopeFor(poisoned)); got != "C:/attacker" {
		t.Fatalf("precondition: tracked cwd not recorded, got %q", got)
	}

	// Each frame signs its cwd, and each names the configured default — the one
	// value that triggers the substitution.
	for _, signed := range []commandMsg{
		{ID: "c2", ExecutionID: "exec-victim", Cwd: "C:/work", RiskLevel: "high"},
		{ID: "c3", ExecutionID: "exec-victim", Cwd: "C:/work", ReadOnly: true},
	} {
		scope := cwdScopeFor(signed)
		if !scope.cwdSigned {
			t.Fatalf("frame %s should be treated as signing its cwd", signed.ID)
		}
		if got := resolveWorkDir(cfg, signed.Cwd, scope); got != "C:/work" {
			t.Errorf("frame %s: signed cwd was overridden, got %q want %q", signed.ID, got, "C:/work")
		}
	}

	// The same scope on an ORDINARY frame still gets `cd` persistence: the
	// hardening must not remove the feature it is protecting.
	ordinary := commandMsg{ID: "c4", ExecutionID: "exec-victim", Cwd: "C:/work"}
	if got := resolveWorkDir(cfg, ordinary.Cwd, cwdScopeFor(ordinary)); got != "C:/attacker" {
		t.Errorf("unsigned frame lost cd persistence, got %q want %q", got, "C:/attacker")
	}
}

// A signed frame with no cwd at all falls back to the configured directory,
// never to a tracked one.
func TestResolveWorkDir_SignedFrameWithoutCwdUsesTheConfiguredDirectory(t *testing.T) {
	resetTrackedCwds()
	t.Cleanup(resetTrackedCwds)

	cfg := &Config{WorkingDirectory: "C:/work"}
	signed := commandMsg{ID: "d1", ExecutionID: "exec-d", ReadOnly: true}
	setTrackedCwd(cwdScopeFor(signed), "C:/attacker")

	if got := resolveWorkDir(cfg, "", cwdScopeFor(signed)); got != "C:/work" {
		t.Errorf("got %q want %q", got, "C:/work")
	}
}
