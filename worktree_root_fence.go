// File: worktree_root_fence.go
// -----------------------------------------------------------------------------
// The device-side ROOT-USE FENCE a root restore holds across its re-check AND
// its switch (PARALLEL_RUNS_AND_COMPUTER_MAINTENANCE_PLAN.md §4.8).
//
// A re-check alone only narrows the window: between reading the branch and
// switching it, a person can start a session, check out their own branch or
// edit the tree. So the decision and the change happen inside ONE claim — the
// rule the reservation system already lives by.
//
// ─── THE FENCE IS COOPERATIVE, AND THIS IS NOT A PAPERED-OVER LIMIT ─────────
//
// It is a process-local mutex. It binds the platform's own work on this device,
// and NOTHING ELSE:
//
//   - a person's own `git` in a terminal consults no device mutex;
//   - an IDE writing to the working tree consults no device mutex;
//   - another process on the same computer (including a second copy of this
//     agent, which the single-instance lock prevents but does not make
//     impossible) does not see it.
//
// So the fence closes PLATFORM-INTERNAL races only, and a residual external
// window remains between the re-check and the switch. There is no honest way to
// close it without an OS-level lock the platform does not have. What makes that
// acceptable is everything around it, not the fence:
//
//   - the restore is NEVER REQUIRED. It exists to tidy inherited leftovers, so
//     when anything is uncertain the answer is to report and leave the root
//     alone: a stale-but-known root costs a stale read, while a wrong switch
//     costs a person's working state;
//   - it runs only while presence says the person is idle or away, which is
//     when the external window is narrowest, and never while they are active;
//   - the switch is attempted only from a tree the re-check found clean, so the
//     realistic failure is a lost SWITCH, not lost content — and a failed or
//     refused switch is reported, never retried blindly.
//
// A restore that cannot acquire the fence ABORTS and reports, rather than
// proceeding on a stale read.
// -----------------------------------------------------------------------------

package main

import (
	"path/filepath"
	"sync"
	"time"
)

// rootUseFences holds the repository roots whose use is currently claimed by
// platform work in this process, keyed the way the file system compares paths.
var rootUseFences = struct {
	sync.Mutex
	held map[string]time.Time
}{held: make(map[string]time.Time)}

// acquireRootUseFence claims repoPath for platform work. It NEVER waits: a
// contended root means another piece of platform work is already acting on it,
// and a restore that queued behind it would act on a stale re-check. The second
// return is false when the fence is already held.
//
// The release function is idempotent.
func acquireRootUseFence(repoPath string) (release func(), ok bool) {
	key := pathIdentityKey(filepath.Clean(repoPath))
	rootUseFences.Lock()
	defer rootUseFences.Unlock()
	if _, held := rootUseFences.held[key]; held {
		return func() {}, false
	}
	rootUseFences.held[key] = time.Now()
	var once sync.Once
	return func() {
		once.Do(func() {
			rootUseFences.Lock()
			delete(rootUseFences.held, key)
			rootUseFences.Unlock()
		})
	}, true
}

// isRootUseFenced reports whether platform work currently holds repoPath.
//
// Session admission and every platform git operation on a root are meant to
// honour this (§4.8). Today only __root_restore__ takes the fence; wiring the
// other platform paths to consult it is recorded as remaining work rather than
// claimed here.
func isRootUseFenced(repoPath string) bool {
	key := pathIdentityKey(filepath.Clean(repoPath))
	rootUseFences.Lock()
	defer rootUseFences.Unlock()
	_, held := rootUseFences.held[key]
	return held
}

// heldRootUseFenceCount is for tests and diagnostics.
func heldRootUseFenceCount() int {
	rootUseFences.Lock()
	defer rootUseFences.Unlock()
	return len(rootUseFences.held)
}
