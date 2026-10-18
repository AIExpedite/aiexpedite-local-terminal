package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func (fx wtFixture) restoreReq(branches ...string) rootRestoreRequest {
	return rootRestoreRequest{
		Root:             fx.Root,
		RepoPath:         fx.MainRepo,
		DefaultBranch:    fx.Default,
		Remote:           "origin",
		RecordedBranches: branches,
		Presence:         "idle",
	}
}

// parkRootOnAPlatformBranch leaves the root checkout where Phase 1 inherits it:
// on a feature branch the author created there, fully pushed and clean.
func parkRootOnAPlatformBranch(t *testing.T, fx wtFixture, branch string) {
	t.Helper()
	wtMustGit(t, fx.MainRepo, "checkout", "-b", branch)
	wtWrite(t, filepath.Join(fx.MainRepo, "feature.txt"), "work\n")
	wtMustGit(t, fx.MainRepo, "add", "-A")
	wtMustGit(t, fx.MainRepo, "commit", "-m", "feature work")
	wtMustGit(t, fx.MainRepo, "push", "-u", "origin", branch)
}

func TestRootRestoreSwitchesWhenEveryConditionHolds(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	parkRootOnAPlatformBranch(t, fx, "feature/left-behind")

	res, err := restoreRootCheckout(ctx, fx.restoreReq("feature/left-behind"))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !res.Restored || res.Refused {
		t.Fatalf("res = %+v, want restored", res)
	}
	if branch := wtMustGit(t, fx.MainRepo, "symbolic-ref", "--short", "HEAD"); branch != fx.Default {
		t.Fatalf("branch = %q, want %q", branch, fx.Default)
	}
	// Switching away loses nothing: the branch ref still holds the commits.
	if sha := wtMustGit(t, fx.MainRepo, "rev-parse", "refs/heads/feature/left-behind"); sha == "" {
		t.Fatal("the feature branch ref was removed")
	}
	if heldRootUseFenceCount() != 0 {
		t.Fatal("the root-use fence was not released")
	}
}

// TestRootRestoreNeverMatchesABranchByName is the portability rule.
//
// `feature/*` is the single most common hand-rolled branch convention there is,
// so a name test would move other people's branches on most codebases. The
// branch must be looked up in what the platform RECORDED creating.
func TestRootRestoreNeverMatchesABranchByName(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	// A person's own branch that happens to follow the same convention.
	parkRootOnAPlatformBranch(t, fx, "feature/their-own-work")

	// The platform recorded creating a DIFFERENT branch.
	res, err := restoreRootCheckout(ctx, fx.restoreReq("feature/something-else"))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if res.Restored {
		t.Fatal("moved a branch the platform has no record of creating")
	}
	if res.Reason != restoreReasonBranchNotRecorded {
		t.Fatalf("reason = %q, want %q", res.Reason, restoreReasonBranchNotRecorded)
	}
	if branch := wtMustGit(t, fx.MainRepo, "symbolic-ref", "--short", "HEAD"); branch != "feature/their-own-work" {
		t.Fatalf("the checkout moved to %q", branch)
	}
	// And an empty record list never admits anything.
	empty, err := restoreRootCheckout(ctx, fx.restoreReq())
	if err != nil {
		t.Fatal(err)
	}
	if empty.Restored || empty.Reason != restoreReasonBranchNotRecorded {
		t.Fatalf("res = %+v, want refused: no record means not ours", empty)
	}
}

func TestRootRestoreRefusesADirtyRoot(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	parkRootOnAPlatformBranch(t, fx, "feature/dirty-root")
	wtWrite(t, filepath.Join(fx.MainRepo, "uncommitted.txt"), "someone is working\n")

	res, err := restoreRootCheckout(ctx, fx.restoreReq("feature/dirty-root"))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if res.Restored || res.Reason != restoreReasonNotClean {
		t.Fatalf("res = %+v, want refused with %q", res, restoreReasonNotClean)
	}
	if _, serr := os.Stat(filepath.Join(fx.MainRepo, "uncommitted.txt")); serr != nil {
		t.Fatalf("the untracked file was lost: %v", serr)
	}
}

func TestRootRestoreRefusesUnpushedCommits(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	parkRootOnAPlatformBranch(t, fx, "feature/unpushed-root")
	wtWrite(t, filepath.Join(fx.MainRepo, "more.txt"), "more\n")
	wtMustGit(t, fx.MainRepo, "add", "-A")
	wtMustGit(t, fx.MainRepo, "commit", "-m", "not pushed")

	res, err := restoreRootCheckout(ctx, fx.restoreReq("feature/unpushed-root"))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if res.Restored || res.Reason != restoreReasonUnpushedCommits {
		t.Fatalf("res = %+v, want refused with %q", res, restoreReasonUnpushedCommits)
	}
}

func TestRootRestoreRefusesWhilePresenceIsActive(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	parkRootOnAPlatformBranch(t, fx, "feature/person-is-here")

	for _, presence := range []string{"active", "", "unknown"} {
		req := fx.restoreReq("feature/person-is-here")
		req.Presence = presence
		res, err := restoreRootCheckout(ctx, req)
		if err != nil {
			t.Fatalf("restore: %v", err)
		}
		if res.Restored || res.Reason != restoreReasonPresenceActive {
			t.Fatalf("presence %q: res = %+v, want refused", presence, res)
		}
	}
}

// TestRootRestoreAbortsWhenTheFenceIsHeld: the decision and the change happen
// inside ONE claim, so a restore that cannot acquire the fence aborts rather
// than proceeding on a stale read.
func TestRootRestoreAbortsWhenTheFenceIsHeld(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	parkRootOnAPlatformBranch(t, fx, "feature/contended")

	release, ok := acquireRootUseFence(fx.MainRepo)
	if !ok {
		t.Fatal("could not take the fence for the test")
	}
	defer release()

	res, err := restoreRootCheckout(ctx, fx.restoreReq("feature/contended"))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if res.Restored || res.Reason != restoreReasonFenceUnavailable {
		t.Fatalf("res = %+v, want aborted with %q", res, restoreReasonFenceUnavailable)
	}
	if branch := wtMustGit(t, fx.MainRepo, "symbolic-ref", "--short", "HEAD"); branch != "feature/contended" {
		t.Fatalf("the checkout moved to %q while the fence was held", branch)
	}
}

func TestRootRestoreRefusesWhileAGitOperationIsInProgress(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	parkRootOnAPlatformBranch(t, fx, "feature/locked")
	gitDir, _, ok := resolveGitDirs(fx.MainRepo)
	if !ok {
		t.Fatal("no git dir")
	}
	if err := os.WriteFile(filepath.Join(gitDir, "index.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := restoreRootCheckout(ctx, fx.restoreReq("feature/locked"))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if res.Restored || res.Reason != restoreReasonIndexLockPresent {
		t.Fatalf("res = %+v, want refused with %q", res, restoreReasonIndexLockPresent)
	}
}

func TestRootRestoreRefusesAWorktreePath(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	created, err := ensureWorktree(ctx, fx.ensureReq("feature/not-a-root"))
	if err != nil {
		t.Fatal(err)
	}
	req := fx.restoreReq("feature/not-a-root")
	req.RepoPath = created.Path
	res, err := restoreRootCheckout(ctx, req)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if res.Restored || res.Reason != restoreReasonInsideAixwt {
		t.Fatalf("res = %+v, want refused with %q", res, restoreReasonInsideAixwt)
	}
}

func TestRootRestoreRefusesAPathOutsideTheRecordedRoot(t *testing.T) {
	fx := newWorktreeFixture(t)
	req := fx.restoreReq("feature/x")
	req.RepoPath = filepath.Join(fx.Base, "seed")
	_, err := restoreRootCheckout(context.Background(), req)
	if err == nil {
		t.Fatal("admitted a checkout outside the recorded root")
	}
	if got := worktreePolicyRuleOf(err); got != rulePathNotUnderRoot {
		t.Fatalf("rule = %q, want %q (%v)", got, rulePathNotUnderRoot, err)
	}
}

func TestRootRestoreIsANoOpOnTheDefaultBranch(t *testing.T) {
	fx := newWorktreeFixture(t)
	res, err := restoreRootCheckout(context.Background(), fx.restoreReq("feature/anything"))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if res.Restored || res.Refused || res.Reason != restoreReasonAlreadyOnDefault {
		t.Fatalf("res = %+v, want a no-op", res)
	}
}

// TestRootRestoreDetachesWhenTheDefaultBranchIsHeldElsewhere mirrors what the
// repo sync already does: a held ref would make the holder's index look
// modified, so the root is detached at the target instead of switched to it.
func TestRootRestoreDetachesWhenTheDefaultBranchIsHeldElsewhere(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	parkRootOnAPlatformBranch(t, fx, "feature/default-held")
	// Another worktree takes the default branch.
	holder := filepath.Join(fx.Base, "holds-main")
	wtMustGit(t, fx.MainRepo, "worktree", "add", holder, fx.Default)

	res, err := restoreRootCheckout(ctx, fx.restoreReq("feature/default-held"))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !res.Restored || !res.Detached {
		t.Fatalf("res = %+v, want restored by detaching", res)
	}
	target := wtMustGit(t, fx.MainRepo, "rev-parse", "refs/heads/"+fx.Default)
	if res.Head != target {
		t.Fatalf("head = %s, want %s", res.Head, target)
	}
}

func TestRootUseFenceIsExclusiveAndReleasable(t *testing.T) {
	dir := tempRootDir(t)
	release, ok := acquireRootUseFence(dir)
	if !ok {
		t.Fatal("first acquire failed")
	}
	if _, second := acquireRootUseFence(dir); second {
		t.Fatal("the fence is not exclusive")
	}
	if !isRootUseFenced(dir) {
		t.Fatal("isRootUseFenced should report a held fence")
	}
	release()
	release() // idempotent
	if isRootUseFenced(dir) {
		t.Fatal("the fence was not released")
	}
	release2, again := acquireRootUseFence(dir)
	if !again {
		t.Fatal("could not re-acquire after release")
	}
	release2()
	if heldRootUseFenceCount() != 0 {
		t.Fatalf("held fences = %d, want 0", heldRootUseFenceCount())
	}
}
