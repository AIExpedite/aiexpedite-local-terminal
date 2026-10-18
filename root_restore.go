// File: root_restore.go
// -----------------------------------------------------------------------------
// __root_restore__ — return a root checkout to its default branch
// (PARALLEL_RUNS_AND_COMPUTER_MAINTENANCE_PLAN.md §4.8).
//
// Today the repo sync's `switch` is the only thing that ever returns a root to
// its default branch after an author creates `feature/<kebab>` in it. §4.6
// removes that switch, so without a replacement every root parked on a feature
// branch would stay there forever, never fast-forward again, and read-only
// analysis would read a stale branch indefinitely. The feature whose branch the
// root holds could also never get a worktree at all.
//
// THE DEVICE RE-CHECKS EVERYTHING AND ABORTS ON ANY MISMATCH. The server's
// evaluation is a PRE-FILTER that decides whether to send this command at all;
// it is never the authority, because between that read and the switch a person
// can start a session, check out their own branch or edit the tree. So all four
// conditions are re-read HERE:
//
//  1. CLEAN — no uncommitted and no untracked changes — and idle;
//  2. on a branch the platform RECORDED CREATING, looked up in the records the
//     server sends, NEVER matched by name. This is a portability rule, not a
//     detail: `feature/*` is the single most common hand-rolled branch
//     convention there is, so a name test would move other people's branches on
//     most codebases. No record → not ours → never moved;
//  3. every commit on that branch is PUSHED. Deliberate belt-and-braces:
//     switching away loses nothing on its own, because the branch ref keeps
//     every commit reachable. Requiring "pushed" anyway means a restore never
//     even momentarily leans on a local-only ref;
//  4. presence is idle or away, never active.
//
// If any condition fails the root is left exactly as it is and reported. A
// branch the person checked out themselves is never moved, whatever state it is
// in. The restore is NEVER REQUIRED: a stale-but-known root costs a stale read,
// while a wrong switch costs a person's working state.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Refusal reasons. Persisted vocabulary: they reach the Computers tab, which
// names the failing condition so a person can clear it.
const (
	restoreReasonFenceUnavailable  = "fence_unavailable"
	restoreReasonPresenceActive    = "presence_active"
	restoreReasonNotCheckout       = "not_a_checkout"
	restoreReasonInsideAixwt       = "inside_aixwt"
	restoreReasonAlreadyOnDefault  = "already_on_default"
	restoreReasonDetachedHead      = "detached_head"
	restoreReasonBranchNotRecorded = "branch_not_recorded"
	restoreReasonNotClean          = "not_clean"
	restoreReasonIndexLockPresent  = "index_lock_present"
	restoreReasonUnpushedCommits   = "unpushed_commits"
	restoreReasonDefaultMissing    = "default_branch_missing"
	restoreReasonSwitchFailed      = "switch_failed"
	restoreReasonStateUnreadable   = "state_unreadable"
)

type rootRestoreRequest struct {
	// Root, when given, contains the restore: repoPath must sit under it.
	Root     string `json:"root"`
	RepoPath string `json:"repoPath"`
	// DefaultBranch is the branch to return to.
	DefaultBranch string `json:"defaultBranch"`
	Remote        string `json:"remote"`
	// RecordedBranches are the branches the platform RECORDED CREATING in this
	// checkout (from the worktree registry and the feature's own record). The
	// current branch must be one of them, exactly. Never a name pattern.
	RecordedBranches []string `json:"recordedBranches"`
	// Presence is the server's presence reading: "active" | "idle" | "away".
	// Active refuses. An empty value also refuses, because an unknown presence
	// is not evidence that the person is away.
	Presence string `json:"presence"`
}

type rootRestoreResult struct {
	RepoPath   string `json:"repoPath"`
	Restored   bool   `json:"restored"`
	Refused    bool   `json:"refused"`
	Reason     string `json:"reason,omitempty"`
	Detail     string `json:"detail,omitempty"`
	FromBranch string `json:"fromBranch,omitempty"`
	ToBranch   string `json:"toBranch,omitempty"`
	Head       string `json:"head,omitempty"`
	Detached   bool   `json:"detached,omitempty"`
	// ChecksPassed names the re-checks that held ON THE DEVICE, in order.
	ChecksPassed []string `json:"checksPassed,omitempty"`
	// IdleEvidence names what the device could actually verify about idleness.
	// It is deliberately explicit about being incomplete — see the comment on
	// collectIdleEvidence.
	IdleEvidence []string `json:"idleEvidence,omitempty"`
	DirtyPaths   []string `json:"dirtyPaths,omitempty"`
}

func refuseRestore(res rootRestoreResult, reason, detail string) (rootRestoreResult, error) {
	res.Refused = true
	res.Reason = reason
	res.Detail = detail
	return res, nil
}

// restoreRootCheckout re-checks every condition on the device, under a
// root-use fence held across the re-check AND the switch, and aborts on any
// mismatch.
func restoreRootCheckout(ctx context.Context, req rootRestoreRequest) (rootRestoreResult, error) {
	res := rootRestoreResult{}
	if len(req.RecordedBranches) > maxRecordedBranches {
		return res, fmt.Errorf("too many recorded branches (%d, max %d)", len(req.RecordedBranches), maxRecordedBranches)
	}
	repoPath := strings.TrimSpace(req.RepoPath)
	if repoPath == "" || !filepath.IsAbs(repoPath) {
		return res, policyRefusal(ruleMainRepoNotAbsolute, fmt.Sprintf("repoPath %q must be absolute", req.RepoPath))
	}
	repoPath = filepath.Clean(repoPath)
	res.RepoPath = repoPath
	for _, seg := range strings.Split(filepath.ToSlash(repoPath), "/") {
		if strings.EqualFold(seg, worktreeRootSegment) {
			return refuseRestore(res, restoreReasonInsideAixwt,
				fmt.Sprintf("%s is a worktree, not a root checkout", repoPath))
		}
	}
	if strings.TrimSpace(req.Root) != "" {
		root := filepath.Clean(strings.TrimSpace(req.Root))
		if !filepath.IsAbs(root) {
			return res, policyRefusal(rulePathRootNotAbsolute, fmt.Sprintf("root %q is not absolute", req.Root))
		}
		if !isPathWithin(root, repoPath) {
			return res, policyRefusal(rulePathNotUnderRoot,
				fmt.Sprintf("%s is not under the recorded root %s", repoPath, root))
		}
	}
	if !isGitCheckout(repoPath) {
		return refuseRestore(res, restoreReasonNotCheckout, repoPath+" is not a git checkout")
	}
	defaultBranch := strings.TrimSpace(req.DefaultBranch)
	if defaultBranch == "" {
		return res, fmt.Errorf("defaultBranch is required")
	}
	res.ToBranch = defaultBranch

	// Presence first: it costs nothing and it is the condition that most often
	// says "not now". An unknown presence refuses, because absence of a reading
	// is not evidence the person is away.
	switch strings.ToLower(strings.TrimSpace(req.Presence)) {
	case "idle", "away":
		res.ChecksPassed = append(res.ChecksPassed, "presence_idle_or_away")
	default:
		return refuseRestore(res, restoreReasonPresenceActive,
			fmt.Sprintf("presence is %q; a restore runs only while the person is idle or away", req.Presence))
	}

	// ── THE FENCE ───────────────────────────────────────────────────────────
	// Held across the re-check AND the switch, and released immediately after.
	// A restore that cannot acquire it ABORTS rather than proceeding on a stale
	// read.
	//
	// IT IS COOPERATIVE AND CANNOT BIND A PERSON'S OWN `git` OR AN IDE. It is a
	// process-local mutex: a person's `git` in a terminal and an IDE writing to
	// the tree consult no device mutex, so the fence closes PLATFORM-INTERNAL
	// races only and a residual external window remains between the re-check
	// and the switch. There is no honest way to close it without an OS-level
	// lock the platform does not have. What makes that acceptable is that the
	// restore is never required, runs only while the person is idle or away,
	// and is attempted only from a tree the re-check found clean — so the
	// realistic failure is a lost SWITCH, not lost content.
	release, ok := acquireRootUseFence(repoPath)
	if !ok {
		return refuseRestore(res, restoreReasonFenceUnavailable,
			"another platform operation holds the root-use fence for "+repoPath)
	}
	defer release()
	res.ChecksPassed = append(res.ChecksPassed, "root_use_fence_held")

	state, serr := wtReadState(ctx, repoPath)
	if serr != nil {
		return refuseRestore(res, restoreReasonStateUnreadable, serr.Error())
	}
	res.FromBranch, res.Head, res.Detached = state.Branch, state.Head, state.Detached
	res.DirtyPaths = state.DirtyPaths

	if state.Detached {
		return refuseRestore(res, restoreReasonDetachedHead,
			"the root is on a detached HEAD, which the platform did not record creating")
	}
	if state.Branch == defaultBranch {
		res.Reason = restoreReasonAlreadyOnDefault
		res.ChecksPassed = append(res.ChecksPassed, "already_on_default")
		return res, nil
	}

	// Condition 2: looked up in the records, NEVER matched by name.
	if !containsExact(req.RecordedBranches, state.Branch) {
		return refuseRestore(res, restoreReasonBranchNotRecorded,
			fmt.Sprintf("the platform has no record of creating %q in this checkout, so it is not ours to move", state.Branch))
	}
	res.ChecksPassed = append(res.ChecksPassed, "branch_recorded_as_platform_created")

	// Condition 1: clean (uncommitted AND untracked).
	if state.Dirty {
		return refuseRestore(res, restoreReasonNotClean,
			fmt.Sprintf("%d uncommitted or untracked path(s)", len(state.DirtyPaths)))
	}
	res.ChecksPassed = append(res.ChecksPassed, "clean")

	idle, lockPath := collectIdleEvidence(repoPath)
	res.IdleEvidence = idle
	if lockPath != "" {
		return refuseRestore(res, restoreReasonIndexLockPresent,
			"a git operation is in progress ("+lockPath+" exists)")
	}

	// Condition 3: every commit pushed.
	unpushed, sample, uerr := wtUnpushedCommits(ctx, repoPath, remoteOrDefault(req.Remote), defaultBranch)
	if uerr != nil {
		return refuseRestore(res, restoreReasonUnpushedCommits, uerr.Error())
	}
	if unpushed {
		return refuseRestore(res, restoreReasonUnpushedCommits,
			fmt.Sprintf("%d commit(s) on %q are not pushed", len(sample), state.Branch))
	}
	res.ChecksPassed = append(res.ChecksPassed, "fully_pushed")

	target, terr := wtRevParse(ctx, repoPath, "refs/heads/"+defaultBranch)
	if terr != nil {
		return refuseRestore(res, restoreReasonStateUnreadable, terr.Error())
	}
	if target == "" {
		return refuseRestore(res, restoreReasonDefaultMissing,
			fmt.Sprintf("this checkout has no local %q to return to", defaultBranch))
	}

	// If another worktree holds the default branch, git refuses to switch to
	// it — so detach the root at the target instead, which is what the repo
	// sync already does for a held ref (a held ref would make the holder's
	// index look modified).
	heldElsewhere, herr := defaultBranchHeldElsewhere(ctx, repoPath, defaultBranch)
	if herr != nil {
		return refuseRestore(res, restoreReasonStateUnreadable, herr.Error())
	}
	if heldElsewhere {
		if _, cerr := wtGit(ctx, repoPath, "checkout", "--detach", target); cerr != nil {
			return refuseRestore(res, restoreReasonSwitchFailed, cerr.Error())
		}
		res.Restored = true
		res.Detached = true
		res.Head = target
		res.ChecksPassed = append(res.ChecksPassed, "detached_at_default_held_elsewhere")
		return res, nil
	}
	if _, cerr := wtGit(ctx, repoPath, "switch", defaultBranch); cerr != nil {
		// A failed or refused switch is REPORTED, never retried blindly.
		return refuseRestore(res, restoreReasonSwitchFailed, cerr.Error())
	}
	after, _ := wtReadState(ctx, repoPath)
	res.Restored = true
	res.Head, res.Detached = after.Head, after.Detached
	res.ChecksPassed = append(res.ChecksPassed, "switched")
	return res, nil
}

// collectIdleEvidence names what this device can actually verify about a root
// being idle, and returns the path of a git lock if one is present.
//
// IT IS DELIBERATELY INCOMPLETE, and saying so is the point. §4.8 condition 1
// is "clean AND idle, with no live session or execution using it as its working
// folder". This agent has no registry of which folder each live CLI session is
// working in — the session managers do not record one — so the device cannot
// check that half today. What it can check, and reports:
//
//   - the ROOT-USE FENCE: no other platform operation in this process holds
//     the root (acquired by the caller before this runs);
//   - NO GIT LOCK: `.git/index.lock` absent, which is direct evidence that no
//     git process is mid-operation in this checkout.
//
// The server's "no live execution" pre-filter is therefore still load-bearing
// for that one condition. Building the session-cwd registry so the device can
// re-check it too is recorded as remaining work rather than claimed here.
func collectIdleEvidence(repoPath string) ([]string, string) {
	evidence := []string{"root_use_fence", "server_prefilter_no_live_execution"}
	gitDir, _, ok := resolveGitDirs(repoPath)
	if !ok {
		return evidence, ""
	}
	lock := filepath.Join(gitDir, "index.lock")
	if _, err := os.Lstat(lock); err == nil {
		return evidence, lock
	}
	return append(evidence, "no_git_index_lock"), ""
}

// defaultBranchHeldElsewhere reports whether a worktree OTHER than repoPath has
// the default branch checked out.
func defaultBranchHeldElsewhere(ctx context.Context, repoPath, defaultBranch string) (bool, error) {
	entries, err := wtListWorktrees(ctx, repoPath)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Branch != defaultBranch {
			continue
		}
		if pathIdentityKey(e.Path) != pathIdentityKey(repoPath) {
			return true, nil
		}
	}
	return false, nil
}

// containsExact is an exact, case-sensitive membership test. Branch names are
// case-sensitive in git, and a case-insensitive match here would let
// `Feature/x` stand in for a recorded `feature/x`.
func containsExact(list []string, want string) bool {
	if want == "" {
		return false
	}
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
