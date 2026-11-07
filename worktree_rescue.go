// File: worktree_rescue.go
// -----------------------------------------------------------------------------
// Preserving a retiring worktree's commits WITHOUT writing to anybody's remote
// (WORKTREE_CONFIG_AND_PORTABILITY.md §7 invariant 4, §8 item 5).
//
// What this replaces: retirement used to PUSH `HEAD:refs/heads/aix/rescue/...`
// to the repository's remote, and `allowRescuePush` was a `*bool` whose nil
// meant push — which the hourly sweep never sends. So the live default was an
// unattended timer creating a vendor-named branch in someone's GitHub, visible
// to their whole organisation, for work they had not asked us to publish.
//
// Nothing of ours reaches their remote without consent. Pushing is now opt-in:
// `allowRescuePush: true`, and nothing sends it today.
//
// WHAT ACTUALLY NEEDS RESCUING IS NARROWER THAN IT LOOKS, and measuring that is
// what makes this safe rather than merely quieter. A worktree checked out on a
// BRANCH loses nothing when its directory and admin record go: `refs/heads/<x>`
// lives in the shared common directory, survives both deletions, and survives
// `reflog expire --expire-unreachable=now --all && gc --prune=now`. The commits
// are already named. The only real loss case is a DETACHED worktree — a
// companion — whose HEAD and reflog live in the admin directory that step 2
// deletes; its object was measurably pruned.
//
// So there are three outcomes, and each is reported:
//
//	branch_retained  the commits are already on a local branch. Nothing is
//	                 created, nothing is pushed, and the worktree is removed.
//	branch_created   detached, or on a branch whose ref has moved away from
//	                 HEAD: a local `aix/rescue/...` ref is made at that SHA in
//	                 the MAIN repository, and the worktree is removed.
//	pushed           `allowRescuePush: true` was sent. Unchanged behaviour,
//	                 now only on request.
//
// `branch_retained` is also a behaviour FIX, not just a rename. With pushing
// off, unpushed commits on a branch used to keep the worktree as
// `unpushed_work` for ever, so nothing was ever cleaned up and the directory
// sat there needing a person. Now the retirement completes and the report names
// the branch holding the work.
//
// Never a ref RESET. `git branch <name> <sha>` creates; it never moves an
// existing ref, and `git branch -f` is refused here for the same reason
// `createWorktree` refuses `worktree add -B`: moving a ref can discard the only
// name a commit had.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"fmt"
	"strings"
)

// The rescue outcomes, reported on the result. Persisted vocabulary: the server
// stores the word, so renaming one is a contract change.
const (
	rescueOutcomeBranchRetained = "branch_retained"
	rescueOutcomeBranchCreated  = "branch_created"
	rescueOutcomePushed         = "pushed"
)

// rescueRefShaSuffixLength is how much of the SHA disambiguates a rescue ref
// whose name is already taken by a DIFFERENT commit. Enough to be unique in
// practice, short enough to read.
const rescueRefShaSuffixLength = 7

// rescueUnpushedCommits makes sure the retiring worktree's commits are named by
// something that will outlive it, and reports which way.
//
// Returns an error when it cannot prove that — never a partial success. The
// caller keeps the worktree, because a retirement that cannot say where the
// work went is one a person has to look at.
func rescueUnpushedCommits(
	ctx context.Context,
	mainRepo string,
	wt resolvedWorktreePath,
	state wtWorktreeState,
	req worktreeRemoveRequest,
	rescueRef string,
	res *worktreeRemoveResult,
) error {
	head := strings.TrimSpace(state.Head)
	if head == "" {
		// wtReadState swallows a failed `rev-parse HEAD`, so this is reachable,
		// and without a SHA there is nothing to point a ref at. Refusing is the
		// only honest answer: the directory stays and says why.
		return fmt.Errorf("the worktree's HEAD could not be read, so its commits cannot be preserved")
	}

	// Opt-in, and nothing in the platform sends it. Kept because a person who
	// asks for their work on the remote should get it.
	if req.AllowRescuePush != nil && *req.AllowRescuePush {
		if _, perr := wtGitNetwork(ctx, wt.Abs, "push", remoteOrDefault(req.Remote),
			"HEAD:refs/heads/"+rescueRef); perr != nil {
			return &rescuePushError{err: perr}
		}
		res.RescuePushed = true
		res.RescueOutcome = rescueOutcomePushed
		res.ChecksPassed = append(res.ChecksPassed, "rescue_pushed")
		return nil
	}

	// Already named by its own branch? Read the REF rather than trusting the
	// checkout: a branch whose ref has moved elsewhere (another worktree
	// advanced it, a reset) no longer holds this HEAD, and then the commits do
	// need a ref of their own.
	if state.Branch != "" {
		if sha, found, rerr := rescueRefSha(ctx, mainRepo, state.Branch); rerr == nil && found && sha == head {
			res.RescueBranch = state.Branch
			res.RescueOutcome = rescueOutcomeBranchRetained
			res.ChecksPassed = append(res.ChecksPassed, "rescue_branch_retained")
			return nil
		}
	}

	// In the MAIN repository: `refs/heads` is in the shared common directory, so
	// the ref outlives both of the deletions that follow, and the main checkout
	// is the one place guaranteed still to be there.
	ref, cerr := createLocalRescueBranch(ctx, mainRepo, rescueRef, head)
	if cerr != nil {
		return cerr
	}
	res.RescueBranch = ref
	res.RescueOutcome = rescueOutcomeBranchCreated
	res.ChecksPassed = append(res.ChecksPassed, "rescue_branch_created")
	return nil
}

// rescuePushError marks a failure of the OPT-IN push, so the caller can report
// the long-standing `rescue_push_failed` reason for it and the new
// `rescue_failed` for everything else. Two words because they tell a person to
// do different things.
type rescuePushError struct{ err error }

func (e *rescuePushError) Error() string { return e.err.Error() }
func (e *rescuePushError) Unwrap() error { return e.err }

// createLocalRescueBranch creates `ref` at `head`, and returns the name it
// actually used.
//
// A name already taken is the ordinary case, not an error: the same (feature,
// repository) can retire more than once, and `git branch` refuses a second
// create outright (`fatal: a branch named '...' already exists`, exit 128) even
// when the existing ref points at the very commit asked for. So:
//
//   - taken AND already at this SHA  → done, nothing to do;
//   - taken at a DIFFERENT SHA       → a `-<7 of the sha>` name, which is
//     unique to the commit being saved and cannot collide with the first;
//   - taken twice over               → done if that one holds this SHA too.
//
// The suffixed name goes back through validateRescueRef, because every ref this
// agent writes passes the same gate.
func createLocalRescueBranch(ctx context.Context, mainRepo, ref, head string) (string, error) {
	existing, found, rerr := rescueRefSha(ctx, mainRepo, ref)
	if rerr != nil {
		return "", fmt.Errorf("reading the rescue branch %q: %w", ref, rerr)
	}
	if found {
		if existing == head {
			return ref, nil
		}
		// Taken by another commit. Name this one after itself.
		suffixed := ref + "-" + head[:min(rescueRefShaSuffixLength, len(head))]
		checked, verr := validateRescueRef(suffixed)
		if verr != nil {
			return "", fmt.Errorf("rescue branch %q is taken and %q is not a usable name: %w", ref, suffixed, verr)
		}
		other, otherFound, oerr := rescueRefSha(ctx, mainRepo, checked)
		if oerr != nil {
			return "", fmt.Errorf("reading the rescue branch %q: %w", checked, oerr)
		}
		if otherFound {
			if other == head {
				return checked, nil
			}
			return "", fmt.Errorf("rescue branches %q and %q are both taken by other commits", ref, checked)
		}
		ref = checked
	}
	if _, berr := wtGit(ctx, mainRepo, "branch", ref, head); berr != nil {
		return "", fmt.Errorf("creating the local rescue branch %q at %s: %w", ref, head, berr)
	}
	return ref, nil
}

// rescueRefSha answers the THREE states a ref read actually has, because
// wtRevParse collapses two of them: it returns ("", nil) for a ref that does
// not exist, which reads identically to a successful empty answer.
//
// Treating that nil as "the ref exists at some other commit" is not a
// hypothetical mistake — it is the one this function was extracted to fix. The
// first version of the rescue took the "already taken" path for every ref,
// including the ones it was about to create, and reported a detached worktree
// as kept/rescue_failed with both candidate names "taken by other commits".
// Caught by TestRemoveDetachedCompanionRescuesItsOwnCommits.
func rescueRefSha(ctx context.Context, mainRepo, ref string) (sha string, found bool, err error) {
	out, rerr := wtRevParse(ctx, mainRepo, "refs/heads/"+ref)
	if rerr != nil {
		return "", false, rerr
	}
	if strings.TrimSpace(out) == "" {
		return "", false, nil
	}
	return strings.TrimSpace(out), true, nil
}
