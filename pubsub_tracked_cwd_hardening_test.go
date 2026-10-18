package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Hardening around the per-sender tracked cwd (pubsub.go, "Tracked Working
// Directory"). All three cases below have one root cause: `executionID` rides
// on the command frame UNSIGNED and is excluded from the HMAC canonical, so a
// topic writer with no key can append or alter one on a REPLAYED signed frame
// and choose which scope the device's bookkeeping hangs off.

/* -------------------------------------------------------------------------
   (1) The upload scan root must come from the AUTHORITATIVE resolved cwd.
   resolveWorkDir already refuses to let a tracked cwd override a signed one;
   the post-command upload scan used to look the tracked map up a second time
   and so could still be pointed at an attacker-chosen scope — missing the
   command's real artifacts, or uploading fresh media out of an unrelated
   directory, on the Unix, TTY and encoded-PowerShell routes that never
   refresh the tracked entry.
   ------------------------------------------------------------------------- */

func TestCommandUploadScanRoot_SignedCwdIsNeverChosenByTheTrackedMap(t *testing.T) {
	resetTrackedCwds()
	t.Cleanup(resetTrackedCwds)

	cfg := &Config{WorkingDirectory: "C:/work"}

	// An env-setup step and a read-only start are the two frames whose `cwd`
	// the HMAC covers. `exec-appended` is the id an attacker added to the
	// replay; the directory it remembers is theirs to pick.
	for _, signed := range []commandMsg{
		{ID: "s1", ExecutionID: "exec-appended", Cwd: "C:/work/setup", RiskLevel: "high"},
		{ID: "s2", ExecutionID: "exec-appended", Cwd: "C:/work/setup", ReadOnly: true},
	} {
		scope := cwdScopeFor(signed)
		if !scope.cwdSigned {
			t.Fatalf("frame %s should be treated as signing its cwd", signed.ID)
		}
		setTrackedCwd(scope, "C:/attacker/media")

		ranIn := resolveWorkDir(cfg, signed.Cwd, scope)
		if ranIn != "C:/work/setup" {
			t.Fatalf("frame %s: precondition, command ran in %q", signed.ID, ranIn)
		}
		if got := commandUploadScanRoot(cfg, scope, ranIn); got != "C:/work/setup" {
			t.Errorf("frame %s: scan root %q, want the directory the command ran in (%q)",
				signed.ID, got, "C:/work/setup")
		}
		// The result's echoed cwd is derived from the same authority.
		if got := reportedCommandCwd(scope, ranIn); got != "C:/work/setup" {
			t.Errorf("frame %s: reported cwd %q, want %q", signed.ID, got, "C:/work/setup")
		}
	}
}

// The scan root must still follow a `cd` the command itself performed — that
// is what the tracked cwd is for, and the Windows persistent-PowerShell route
// records the host's new location against this sender's own key.
func TestCommandUploadScanRoot_OrdinaryFrameStillFollowsAnInCommandCd(t *testing.T) {
	resetTrackedCwds()
	t.Cleanup(resetTrackedCwds)

	cfg := &Config{WorkingDirectory: "C:/work"}
	cmd := commandMsg{ID: "o1", ExecutionID: "exec-a", Cwd: "C:/work"}
	scope := cwdScopeFor(cmd)

	ranIn := resolveWorkDir(cfg, cmd.Cwd, scope)
	// The command cd'd into a subdirectory and wrote its screenshot there.
	setTrackedCwd(scope, "C:/work/repo/artifacts")

	if got := commandUploadScanRoot(cfg, scope, ranIn); got != "C:/work/repo/artifacts" {
		t.Errorf("scan root %q, want the post-cd directory %q", got, "C:/work/repo/artifacts")
	}
}

// A tracked cwd belonging to ANOTHER sender must not answer here either. The
// command ran where resolveWorkDir sent it, and an explicit cwd that differs
// from the configured default is exactly a case the old tracked-first lookup
// got wrong.
func TestCommandUploadScanRoot_IgnoresAnotherSendersTrackedCwd(t *testing.T) {
	resetTrackedCwds()
	t.Cleanup(resetTrackedCwds)

	cfg := &Config{WorkingDirectory: "C:/work"}
	// The process-wide entry an id-less command left behind.
	setTrackedCwd(cwdScope{}, "C:/work/aixwt/other-feature/repo")

	cmd := commandMsg{ID: "o2", ExecutionID: "exec-b", Cwd: "C:/work/aixwt/mine/repo"}
	scope := cwdScopeFor(cmd)
	ranIn := resolveWorkDir(cfg, cmd.Cwd, scope)
	if ranIn != "C:/work/aixwt/mine/repo" {
		t.Fatalf("precondition: command ran in %q", ranIn)
	}

	if got := commandUploadScanRoot(cfg, scope, ranIn); got != "C:/work/aixwt/mine/repo" {
		t.Errorf("scan root %q, want %q", got, "C:/work/aixwt/mine/repo")
	}
}

// With nothing resolved and nothing tracked the scan still has a root: the
// configured working directory, as it always did.
func TestCommandUploadScanRoot_FallsBackToTheConfiguredDirectory(t *testing.T) {
	resetTrackedCwds()
	t.Cleanup(resetTrackedCwds)

	cfg := &Config{WorkingDirectory: "C:/work"}
	if got := commandUploadScanRoot(cfg, cwdScope{executionID: "exec-c"}, ""); got != "C:/work" {
		t.Errorf("scan root %q, want %q", got, "C:/work")
	}
	if got := commandUploadScanRoot(nil, cwdScope{}, ""); got != "" {
		t.Errorf("with no config and nothing resolved the root should be empty, got %q", got)
	}
}

/* -------------------------------------------------------------------------
   (2) 512 entries is a count, not a memory bound. Neither identifier a scope
   is built from is length-checked on the device, and neither is signed, so a
   near-megabyte `executionId` on each replayed command turned 512 keys into
   ~512 MB.
   ------------------------------------------------------------------------- */

func TestTrackedCwd_OverlongScopeIDIsBoundedBeforeItIsRetained(t *testing.T) {
	resetTrackedCwds()
	t.Cleanup(resetTrackedCwds)

	huge := strings.Repeat("A", 900*1024)
	setTrackedCwd(cwdScopeFor(commandMsg{ID: "m1", ExecutionID: huge}), "C:/work/a")
	setTrackedCwd(cwdScopeFor(commandMsg{ID: "m2", SessionID: huge}), "C:/work/b")

	trackedCwdLock.Lock()
	keyBytes := 0
	longest := 0
	for key := range trackedCwds {
		keyBytes += len(key)
		if len(key) > longest {
			longest = len(key)
		}
	}
	trackedCwdLock.Unlock()

	// "exec:"/"session:" plus a bounded identifier. 32 is slack for the
	// prefixes, not a second bound.
	if longest > trackedCwdMaxScopeIDLen+32 {
		t.Fatalf("a retained key is %d bytes; the bound is %d", longest, trackedCwdMaxScopeIDLen)
	}
	// What the finding is actually about: a full map's keys must be kilobytes,
	// not hundreds of megabytes.
	if worst := (trackedCwdMaxScopeIDLen + 32) * trackedCwdMaxEntries; keyBytes > worst {
		t.Fatalf("retained key bytes %d exceed the whole map's bound %d", keyBytes, worst)
	}
}

// Bounding must not merge two senders onto one cwd — truncation would, a
// digest does not. The same long id must also still find its own entry.
func TestTrackedCwd_BoundedScopeIDsStayDistinctAndReadable(t *testing.T) {
	resetTrackedCwds()
	t.Cleanup(resetTrackedCwds)

	base := strings.Repeat("A", 300)
	first := base + "-one"
	second := base + "-two"
	setTrackedCwd(cwdScope{executionID: first}, "C:/work/one")
	setTrackedCwd(cwdScope{executionID: second}, "C:/work/two")

	if got := getTrackedCwd(cwdScope{executionID: first}); got != "C:/work/one" {
		t.Errorf("first sender read %q, want %q", got, "C:/work/one")
	}
	if got := getTrackedCwd(cwdScope{executionID: second}); got != "C:/work/two" {
		t.Errorf("second sender read %q, want %q — two bounded ids collided", got, "C:/work/two")
	}
}

/* -------------------------------------------------------------------------
   (3) LRU eviction could silently move a live run's cwd: 512 distinct inserts
   after its latest command made it the victim well inside the TTL, and its
   next command then ran somewhere else entirely with a clean exit code. The
   entry of a run that is still working is now retained, and an insert that
   finds no expired entry is declined instead.
   ------------------------------------------------------------------------- */

func TestTrackedCwd_ALiveRunKeepsItsCwdThroughAFloodOfNewScopes(t *testing.T) {
	resetTrackedCwds()
	t.Cleanup(resetTrackedCwds)

	cfg := &Config{WorkingDirectory: "C:/work"}
	live := cwdScope{executionID: "exec-live"}
	setTrackedCwd(live, "C:/work/aixwt/feature-1/repo")
	// A directory the evicted run would have fallen through to — the failure
	// mode is a command running in the WRONG place, not merely a cache miss.
	setTrackedCwd(cwdScope{}, "C:/work/aixwt/feature-2/repo")

	for i := 0; i < trackedCwdMaxEntries+100; i++ {
		setTrackedCwd(cwdScope{executionID: fmt.Sprintf("flood-%d", i)}, fmt.Sprintf("C:/flood/%d", i))
	}

	if got := getTrackedCwd(live); got != "C:/work/aixwt/feature-1/repo" {
		t.Errorf("the live run lost its tracked cwd to the flood: %q", got)
	}
	// And its next command still resolves to its own worktree, which is the
	// consequence that matters: the server re-sends the configured default
	// every time, so an evicted entry sent the command elsewhere.
	if got := resolveWorkDir(cfg, "C:/work", live); got != "C:/work/aixwt/feature-1/repo" {
		t.Errorf("the live run's next command would run in %q", got)
	}

	trackedCwdLock.Lock()
	size := len(trackedCwds)
	trackedCwdLock.Unlock()
	if size > trackedCwdMaxEntries {
		t.Fatalf("the map grew past its bound: %d > %d", size, trackedCwdMaxEntries)
	}
}

// Declining is not a leak: once an entry outlives the TTL it is reclaimed on
// the next insert, without waiting for the one-minute sweeper.
func TestTrackedCwd_AnExpiredEntryStillMakesRoomAtTheCap(t *testing.T) {
	resetTrackedCwds()
	t.Cleanup(resetTrackedCwds)

	for i := 0; i < trackedCwdMaxEntries; i++ {
		setTrackedCwd(cwdScope{executionID: fmt.Sprintf("exec-%d", i)}, fmt.Sprintf("C:/d%d", i))
	}
	trackedCwdLock.Lock()
	trackedCwds["exec:exec-3"].lastAccess = time.Now().Add(-2 * trackedCwdTTL)
	trackedCwdLock.Unlock()

	setTrackedCwd(cwdScope{executionID: "newcomer"}, "C:/new")

	if got := getTrackedCwd(cwdScope{executionID: "newcomer"}); got != "C:/new" {
		t.Errorf("an expired entry did not make room for the newcomer: %q", got)
	}
	if got := getTrackedCwd(cwdScope{executionID: "exec-3"}); got != "" {
		t.Errorf("the expired entry survived: %q", got)
	}
}

// A declined sender is never handed another scope's directory instead: it
// keeps resolving to the cwd the server sent it, which is what every one-off
// command did before this cache existed.
func TestTrackedCwd_ADeclinedSenderRunsWhereItWasSent(t *testing.T) {
	resetTrackedCwds()
	t.Cleanup(resetTrackedCwds)

	cfg := &Config{WorkingDirectory: "C:/work"}

	// The "" entry is the one that matters, and the earlier version of this
	// test never created it — so resolveWorkDir returned at step 1 and the
	// assertion passed against plain LRU and against no cache at all. Every
	// one-off execute from a server that sends no identity writes this key, so
	// in production it is normally populated with SOME other sender's cwd.
	setTrackedCwd(cwdScope{}, "C:/someone/elses/worktree")

	for i := 0; i < trackedCwdMaxEntries; i++ {
		setTrackedCwd(cwdScope{executionID: fmt.Sprintf("exec-%d", i)}, fmt.Sprintf("C:/d%d", i))
	}

	declined := cwdScope{executionID: "exec-declined"}
	setTrackedCwd(declined, "C:/work/aixwt/mine/repo")
	if got := getTrackedCwd(declined); got != "" {
		t.Fatalf("precondition: the insert should have been declined, read %q", got)
	}

	// And the frame must carry the DEFAULT cwd, which is what the server
	// re-sends every time and the only value that reaches the map lookup.
	if got := resolveWorkDir(cfg, cfg.WorkingDirectory, declined); got != cfg.WorkingDirectory {
		t.Errorf("a declined sender ran in %q, want the cwd it was sent (%q)", got, cfg.WorkingDirectory)
	}
}

// An identified sender must never read the unidentified pool's cwd.
//
// setTrackedCwd writes keys()[0] alone, so an identified scope can never have
// WRITTEN the "" key — a fallback to it could only ever return another sender's
// directory. This is the route by which a run executes in another feature's
// worktree with a clean exit code, and it is reachable whenever the sender's own
// entry is absent: never set, expired, evicted, or declined.
func TestTrackedCwd_AnIdentifiedSenderNeverInheritsTheUnidentifiedCwd(t *testing.T) {
	resetTrackedCwds()
	t.Cleanup(resetTrackedCwds)

	cfg := &Config{WorkingDirectory: "C:/work"}
	setTrackedCwd(cwdScope{}, "C:/someone/elses/worktree")

	for name, scope := range map[string]cwdScope{
		"execution only":        {executionID: "exec-a"},
		"session only":          {sessionID: "sess-a"},
		"execution and session": {executionID: "exec-b", sessionID: "sess-b"},
	} {
		if got := getTrackedCwd(scope); got != "" {
			t.Errorf("%s: read the unidentified cwd %q", name, got)
		}
		if got := resolveWorkDir(cfg, cfg.WorkingDirectory, scope); got != cfg.WorkingDirectory {
			t.Errorf("%s: resolved to %q, want the sent cwd %q", name, got, cfg.WorkingDirectory)
		}
	}

	// A frame with NO identity still gets its own entry: that key is theirs,
	// and removing the fallback must not remove the feature.
	if got := getTrackedCwd(cwdScope{}); got != "C:/someone/elses/worktree" {
		t.Errorf("an unidentified sender lost its own cwd, got %q", got)
	}
}

// Execute must hand back ITS OWN resulting cwd, not leave the caller to read
// the shared host afterwards.
//
// ps.lastCwd is written inside ps.mutex, but GetTrackedCwd reads it under
// globalPSLock — a different lock — after Execute has released its own. The
// persistent PowerShell host is shared by every Pub/Sub callback, so in that
// window a second command can run, or restart the host, and overwrite lastCwd.
// The first command would then record the SECOND command's directory against
// its scope and later run in the wrong worktree, which is precisely the failure
// this slice exists to remove.
//
// A unit test cannot drive the real Windows host here, so what it pins is the
// contract that makes the race impossible: the cwd travels out of Execute with
// the output, so no second read is needed.
func TestPersistentPowerShell_ExecuteReturnsItsOwnCwd(t *testing.T) {
	var ps *PersistentPowerShell
	fn := ps.Execute

	// Three results: output, the host's cwd as of this command, and error.
	// If this stops compiling because Execute lost its cwd return, the caller
	// in runLocalCommandIn has gone back to re-reading the shared host.
	var _ func(context.Context, string, string) (string, string, error) = fn
}
