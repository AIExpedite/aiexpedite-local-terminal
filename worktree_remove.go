// File: worktree_remove.go
// -----------------------------------------------------------------------------
// __worktree_remove__ and __worktree_evict_deps__
// (PARALLEL_RUNS_AND_COMPUTER_MAINTENANCE_PLAN.md §9.2).
//
// THE ORDER IS THE WHOLE SAFETY PROPERTY:
//
//	0. read the worktree's ONE administrative directory name while the worktree
//	   still exists;
//	1. the link-safe walk deletes the directory, unlinking reparse points and
//	   never descending them;
//	2. exactly that ONE admin directory is removed under the main repository's
//	   .git/worktrees/ — never `git worktree prune`, which takes no target
//	   operand and would drop the record of EVERY worktree whose tree is
//	   missing, including a person's own on an unmounted drive or a disconnected
//	   share.
//
// `git worktree remove` is not used and must not be: it deletes the working
// tree recursively itself, so it would traverse the very junctions step 1
// exists to avoid — exactly the sequence that emptied `local/node_modules` and
// both shared repositories on 2026-09-17.
//
// Before any of that, the DEVICE (never the server) checks that the path
// matches the pattern and sits under its recorded root, that HEAD and branch
// are what the server expects, that nothing is uncommitted, and that nothing is
// unpushed. Uncommitted work is never deleted: the worktree is KEPT and
// reported. If work exists only here it is pushed to aix/rescue/… first, and
// only if that push fails is the worktree kept.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Kept reasons. Persisted vocabulary: they reach the registry's `keptReason`
// and the Computers tab.
const (
	keptReasonBranchMismatch   = "branch_mismatch"
	keptReasonHeadMismatch     = "head_mismatch"
	keptReasonUncommittedWork  = "uncommitted_work"
	keptReasonUnpushedWork     = "unpushed_work"
	keptReasonRescuePushFailed = "rescue_push_failed"
	keptReasonNotRegistered    = "not_registered"
	keptReasonStateUnreadable  = "state_unreadable"
)

type worktreeRemoveRequest struct {
	Root     string `json:"root"`
	MainRepo string `json:"mainRepo"`
	Path     string `json:"path"`
	// Branch and Head are what the SERVER expects. A mismatch keeps the
	// worktree: the server's view is stale, and this is the window in which a
	// person could have committed.
	Branch        string `json:"branch"`
	Head          string `json:"head"`
	Remote        string `json:"remote"`
	DefaultBranch string `json:"defaultBranch"`
	// RescueBranch defaults to aix/rescue/<featureFolder>/<repoFolder>.
	RescueBranch string `json:"rescueBranch"`
	// AllowRescuePush defaults to true.
	AllowRescuePush *bool `json:"allowRescuePush"`
}

type worktreeRemoveResult struct {
	Path            string              `json:"path"`
	Rel             string              `json:"rel"`
	Removed         bool                `json:"removed"`
	Kept            bool                `json:"kept"`
	KeptReason      string              `json:"keptReason,omitempty"`
	Detail          string              `json:"detail,omitempty"`
	AdminDir        string              `json:"adminDir,omitempty"`
	AdminDirRemoved bool                `json:"adminDirRemoved"`
	RescuePushed    bool                `json:"rescuePushed,omitempty"`
	RescueBranch    string              `json:"rescueBranch,omitempty"`
	Deleted         linkSafeDeleteStats `json:"deleted"`
	// ChecksPassed names the device-side proofs that held, in order, so a
	// removal is reported rather than merely claimed.
	ChecksPassed []string `json:"checksPassed,omitempty"`
	DirtyPaths   []string `json:"dirtyPaths,omitempty"`
}

func removeWorktree(ctx context.Context, req worktreeRemoveRequest) (worktreeRemoveResult, error) {
	var res worktreeRemoveResult
	wt, err := resolveWorktreePath(req.Root, req.Path)
	if err != nil {
		return res, err
	}
	res.Path, res.Rel = wt.Abs, wt.Rel
	res.ChecksPassed = append(res.ChecksPassed, "path_policy")

	mainRepo, err := resolveMainRepoPath(req.MainRepo)
	if err != nil {
		return res, err
	}

	rescueRef := strings.TrimSpace(req.RescueBranch)
	if rescueRef == "" {
		rescueRef = "aix/rescue/" + wt.FeatureFolder + "/" + wt.RepoFolder
	}
	rescueRef, err = validateRescueRef(rescueRef)
	if err != nil {
		return res, err
	}
	res.RescueBranch = rescueRef

	// The tree is already gone: remove the one stale admin record, by path.
	if !wt.Exists {
		adminDir, aerr := findAdminDirForPath(mainRepo, wt.Abs)
		if aerr != nil {
			return res, aerr
		}
		res.Removed = true
		if adminDir == "" {
			res.Detail = "no directory and no admin record for this path"
			return res, nil
		}
		res.AdminDir = adminDir
		if _, derr := linkSafeRemoveAll(adminDir); derr != nil {
			return res, fmt.Errorf("removing the admin record %s: %w", adminDir, derr)
		}
		res.AdminDirRemoved = true
		return res, nil
	}

	// Step 0: the admin directory name, read WHILE the worktree exists.
	adminDir, err := wtAdminDir(ctx, mainRepo, wt.Abs)
	if err != nil {
		res.Kept, res.KeptReason, res.Detail = true, keptReasonNotRegistered, err.Error()
		return res, nil
	}
	res.AdminDir = adminDir
	res.ChecksPassed = append(res.ChecksPassed, "registered_admin_dir")

	state, err := wtReadState(ctx, wt.Abs)
	if err != nil {
		res.Kept, res.KeptReason, res.Detail = true, keptReasonStateUnreadable, err.Error()
		return res, nil
	}
	if req.Branch != "" && state.Branch != req.Branch {
		res.Kept, res.KeptReason = true, keptReasonBranchMismatch
		res.Detail = fmt.Sprintf("worktree is on %q, the server expected %q", state.Branch, req.Branch)
		return res, nil
	}
	if req.Head != "" && state.Head != req.Head {
		res.Kept, res.KeptReason = true, keptReasonHeadMismatch
		res.Detail = fmt.Sprintf("worktree HEAD is %s, the server expected %s", state.Head, req.Head)
		return res, nil
	}
	res.ChecksPassed = append(res.ChecksPassed, "head_and_branch_match")

	if state.Dirty {
		// Uncommitted work is NEVER deleted — and "dirty" includes untracked
		// files, which are precisely what git cannot recover.
		res.Kept, res.KeptReason = true, keptReasonUncommittedWork
		res.DirtyPaths = state.DirtyPaths
		res.Detail = fmt.Sprintf("%d uncommitted or untracked path(s)", len(state.DirtyPaths))
		return res, nil
	}
	res.ChecksPassed = append(res.ChecksPassed, "nothing_uncommitted")

	// The unpushed check runs for a DETACHED worktree too, not only a branch.
	// A companion is detached, and a detached HEAD can hold commits an earlier
	// run made while the working tree looks pristine (that is exactly the
	// "diverged companion" ensure reports and leaves alone) — so skipping this
	// for a detached HEAD would delete the only copy of those commits.
	// wtUnpushedCommits answers both shapes: `@{u}..HEAD` where there is an
	// upstream, reachability from <remote>/<default> where there is not.
	unpushed, sample, uerr := wtUnpushedCommits(ctx, wt.Abs, remoteOrDefault(req.Remote), req.DefaultBranch)
	if uerr != nil {
		// Could not PROVE it is pushed. Keep it.
		res.Kept, res.KeptReason, res.Detail = true, keptReasonUnpushedWork, uerr.Error()
		return res, nil
	}
	if unpushed {
		if req.AllowRescuePush != nil && !*req.AllowRescuePush {
			res.Kept, res.KeptReason = true, keptReasonUnpushedWork
			res.Detail = fmt.Sprintf("%d unpushed commit(s) and rescue pushing is off", len(sample))
			return res, nil
		}
		if _, perr := wtGitNetwork(ctx, wt.Abs, "push", remoteOrDefault(req.Remote),
			"HEAD:refs/heads/"+rescueRef); perr != nil {
			res.Kept, res.KeptReason, res.Detail = true, keptReasonRescuePushFailed, perr.Error()
			return res, nil
		}
		res.RescuePushed = true
		res.ChecksPassed = append(res.ChecksPassed, "rescue_pushed")
	}
	res.ChecksPassed = append(res.ChecksPassed, "nothing_unpushed")

	// Step 1: the link-safe delete. NOT `git worktree remove`.
	stats, derr := linkSafeRemoveAll(wt.Abs)
	res.Deleted = stats
	if derr != nil {
		return res, fmt.Errorf("link-safe delete of %s: %w", wt.Abs, derr)
	}
	res.Removed = true
	res.ChecksPassed = append(res.ChecksPassed, "link_safe_delete")

	// Step 2: exactly ONE admin record, by path. NOT `git worktree prune`.
	if _, aerr := linkSafeRemoveAll(adminDir); aerr != nil {
		return res, fmt.Errorf("removing the admin record %s: %w", adminDir, aerr)
	}
	res.AdminDirRemoved = true
	res.ChecksPassed = append(res.ChecksPassed, "one_admin_record_removed")
	// Drop the dependency markers whose directories have just gone.
	_ = removeMarkersUnder(wt.Abs)
	return res, nil
}

// findAdminDirForPath locates the one admin directory under
// <commonDir>/worktrees/ whose `gitdir` back-pointer names path — used when the
// worktree's own `.git` is already gone, so wtAdminDir cannot read it.
//
// It scans the admin directories and matches BY PATH. That is the opposite of
// `git worktree prune`, which has no target at all: this touches one record,
// the one that names the path the server asked about, and returns "" when no
// record names it.
func findAdminDirForPath(mainRepo, path string) (string, error) {
	_, commonDir, ok := resolveGitDirs(mainRepo)
	if !ok {
		return "", fmt.Errorf("%s has no readable git directory", mainRepo)
	}
	worktreesDir := filepath.Join(commonDir, "worktrees")
	entries, err := os.ReadDir(worktreesDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	want := pathIdentityKey(filepath.Join(path, ".git"))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		admin := filepath.Join(worktreesDir, e.Name())
		raw, rerr := os.ReadFile(filepath.Join(admin, "gitdir"))
		if rerr != nil {
			continue
		}
		if pathIdentityKey(filepath.Clean(strings.TrimSpace(string(raw)))) == want {
			return admin, nil
		}
	}
	return "", nil
}

// removeMarkersUnder forgets the dependency markers of a worktree that no
// longer exists. Best effort: a stale marker is harmless, because every reader
// checks that the marker's Dir still matches the directory it is evaluating.
func removeMarkersUnder(worktree string) error {
	dir := filepath.Join(GetConfigDir(), depsMarkerDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		var m depsProvenanceMarker
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if m.Worktree != "" && isPathWithin(worktree, m.Dir) {
			_ = os.Remove(path)
		}
	}
	return nil
}

// ─── __worktree_evict_deps__ ────────────────────────────────────────────────

type worktreeEvictDepsRequest struct {
	Root     string   `json:"root"`
	MainRepo string   `json:"mainRepo"`
	Path     string   `json:"path"`
	Dirs     []string `json:"dirs"`
	// DryRun evaluates every proof and deletes nothing.
	DryRun bool `json:"dryRun"`
}

type worktreeEvictedDir struct {
	Dir     string              `json:"dir"`
	Deleted linkSafeDeleteStats `json:"deleted"`
}

type worktreeKeptDir struct {
	Dir    string        `json:"dir"`
	Reason string        `json:"reason"`
	Detail string        `json:"detail,omitempty"`
	Diff   *baselineDiff `json:"diff,omitempty"`
	Export string        `json:"export,omitempty"`
}

type worktreeEvictDepsResult struct {
	Path string `json:"path"`
	Rel  string `json:"rel"`
	// Evictable names the directories that passed every proof. In a dry run
	// nothing is in Evicted, so the two together say what WOULD go.
	Evictable  []string             `json:"evictable,omitempty"`
	Evicted    []worktreeEvictedDir `json:"evicted,omitempty"`
	Kept       []worktreeKeptDir    `json:"kept,omitempty"`
	FreedBytes int64                `json:"freedBytes"`
	// DepsReady goes false as soon as anything was evicted: the worktree is
	// intact and resumable, and the next start rebuilds rather than trusting a
	// half-tree.
	DepsReady bool `json:"depsReady"`
	DryRun    bool `json:"dryRun,omitempty"`
}

// evictWorktreeDeps reclaims a worktree's rebuildable directories while keeping
// everything that makes it resumable: its tracked files, its branch and its
// PATH. Resume is unaffected — the path still exists, so the `agentId + cwd`
// affinity, Claude `--resume` and the Codex conversation store all still
// resolve — and the next start rebuilds through `prepare`.
//
// A folder name is a CANDIDATE, never a licence to delete. `build`, `target`,
// `dist` and `out` are also ordinary source directory names, and plenty of
// repositories commit a generated bundle on purpose. So every candidate proves
// it is disposable first, and anything that fails a check is skipped and
// reported, never deleted.
func evictWorktreeDeps(ctx context.Context, req worktreeEvictDepsRequest) (worktreeEvictDepsResult, error) {
	res := worktreeEvictDepsResult{DepsReady: true, DryRun: req.DryRun}
	if len(req.Dirs) > maxEvictDirs {
		return res, fmt.Errorf("too many directories (%d, max %d)", len(req.Dirs), maxEvictDirs)
	}
	wt, err := resolveWorktreePath(req.Root, req.Path)
	if err != nil {
		return res, err
	}
	res.Path, res.Rel = wt.Abs, wt.Rel
	if _, err := resolveMainRepoPath(req.MainRepo); err != nil {
		return res, err
	}
	if !wt.Exists {
		return res, fmt.Errorf("%s does not exist", wt.Abs)
	}

	for _, dir := range req.Dirs {
		abs, perr := resolveInsideWorktree(wt, dir)
		if perr != nil {
			return res, perr // a bad dispatch, not a device state: refuse loudly
		}
		verdict := evaluateEvictionCandidate(ctx, wt, dir, abs)
		if verdict.Reason != evictReasonEvictable {
			kept := worktreeKeptDir{Dir: dir, Reason: verdict.Reason, Detail: verdict.Detail}
			if verdict.Diff != nil {
				kept.Diff = verdict.Diff
				// A touched tree is KEPT, and what can be saved is exported.
				// This is not a claim that an arbitrary change can be
				// losslessly reconstructed and then safely deleted: keeping the
				// directory is the honest outcome.
				if export, eerr := exportBaselineDiff(abs, *verdict.Diff); eerr == nil {
					kept.Export = export
				}
			}
			res.Kept = append(res.Kept, kept)
			continue
		}
		res.Evictable = append(res.Evictable, dir)
		if req.DryRun {
			continue
		}
		stats, derr := linkSafeRemoveAll(abs)
		if derr != nil {
			res.Kept = append(res.Kept, worktreeKeptDir{Dir: dir, Reason: "delete_failed", Detail: derr.Error()})
			continue
		}
		_ = removeDepsMarker(abs)
		res.Evicted = append(res.Evicted, worktreeEvictedDir{Dir: dir, Deleted: stats})
		res.FreedBytes += stats.Bytes
		res.DepsReady = false
	}
	return res, nil
}

// evictionVerdict is one candidate's answer.
type evictionVerdict struct {
	Reason string
	Detail string
	Diff   *baselineDiff
}

// evaluateEvictionCandidate runs every proof, cheapest first, and returns
// evictReasonEvictable only when ALL of them hold:
//
//   - the platform recorded creating it, WITH a baseline. Build output the
//     agent produced inside its own CLI session has no marker, so it is NOT
//     evictable — a real reduction in what eviction reclaims, stated plainly;
//   - git ignores it, so the repository itself considers it disposable;
//   - it holds no tracked file and no modified tracked content. A single
//     tracked file inside disqualifies the whole directory, and an UNMODIFIED
//     tracked file never appears in `git status`, so `ls-files` is the only
//     check that sees it;
//   - it still matches its baseline exactly. A creation marker proves who MADE
//     the directory, not that everything now inside it is reproducible: an
//     agent that patched an ignored dependency to debug it, or hand-wrote a
//     file into build output, passes every check above;
//   - a content digest over the candidate matches the one recorded at prepare
//     time. The manifest finds the changes; the digest AUTHORISES the delete,
//     because size and mtime can be identical over changed bytes after a
//     timestamp-preserving copy or an archive extraction, and this gate deletes
//     irreversibly.
func evaluateEvictionCandidate(ctx context.Context, wt resolvedWorktreePath, rel, abs string) evictionVerdict {
	info, serr := os.Lstat(abs)
	if serr != nil {
		return evictionVerdict{Reason: evictReasonAbsent, Detail: "nothing at " + rel}
	}
	if isWorktreeLink(info, abs) || !info.IsDir() {
		return evictionVerdict{Reason: evictReasonNoMarker, Detail: rel + " is not a directory the platform created"}
	}
	marker, merr := readDepsMarker(abs)
	if merr != nil {
		return evictionVerdict{Reason: evictReasonNoMarker, Detail: merr.Error()}
	}
	if marker == nil {
		return evictionVerdict{Reason: evictReasonNoMarker,
			Detail: "no provenance marker: the platform did not create " + rel + ", so it is not ours to reclaim"}
	}
	if !marker.BaselineComplete {
		return evictionVerdict{Reason: evictReasonBaselineIncomplete,
			Detail: "the recorded baseline is incomplete, so a diff cannot prove the tree is untouched"}
	}
	if !marker.TreeDigestComplete || marker.TreeSha256 == "" {
		return evictionVerdict{Reason: evictReasonDigestIncomplete,
			Detail: "no complete content digest was recorded at prepare time"}
	}
	// Tracked content is checked BEFORE the ignore rule, and the order is for
	// the REPORT rather than for safety: `git check-ignore` deliberately does
	// not report a tracked path as ignored, so a directory holding a committed
	// bundle would otherwise come back as `not_ignored` when the real reason is
	// that it holds a person's tracked files. Both refuse; only one explains.
	tracked, terr := wtTrackedFilesUnder(ctx, wt.Abs, rel)
	if terr != nil {
		return evictionVerdict{Reason: evictReasonScanFailed, Detail: terr.Error()}
	}
	if len(tracked) > 0 {
		return evictionVerdict{Reason: evictReasonTrackedContent,
			Detail: fmt.Sprintf("%s holds tracked files (e.g. %s)", rel, strings.Join(tracked, ", "))}
	}
	modified, moderr := wtModifiedUnder(ctx, wt.Abs, rel)
	if moderr != nil {
		return evictionVerdict{Reason: evictReasonScanFailed, Detail: moderr.Error()}
	}
	if len(modified) > 0 {
		return evictionVerdict{Reason: evictReasonModifiedTracked,
			Detail: fmt.Sprintf("%s holds modified tracked content (e.g. %s)", rel, strings.Join(modified, ", "))}
	}
	ignored, ierr := wtPathIsIgnored(ctx, wt.Abs, rel)
	if ierr != nil {
		return evictionVerdict{Reason: evictReasonScanFailed, Detail: ierr.Error()}
	}
	if !ignored {
		return evictionVerdict{Reason: evictReasonNotIgnored,
			Detail: "git does not ignore " + rel + ", so the repository does not consider it disposable"}
	}
	current, complete, cerr := scanBaseline(abs)
	if cerr != nil {
		return evictionVerdict{Reason: evictReasonScanFailed, Detail: cerr.Error()}
	}
	if !complete {
		return evictionVerdict{Reason: evictReasonBaselineIncomplete, Detail: "the current scan hit its bound"}
	}
	diff := diffBaseline(marker.Baseline, current)
	if !diff.empty() {
		d := diff
		return evictionVerdict{Reason: evictReasonBaselineMismatch,
			Detail: fmt.Sprintf("%d difference(s) from the recorded baseline", diff.Total), Diff: &d}
	}
	digest, digestComplete, derr := digestTree(abs)
	if derr != nil {
		return evictionVerdict{Reason: evictReasonScanFailed, Detail: derr.Error()}
	}
	if !digestComplete {
		return evictionVerdict{Reason: evictReasonDigestIncomplete, Detail: "the content digest could not be completed"}
	}
	if digest != marker.TreeSha256 {
		return evictionVerdict{Reason: evictReasonDigestMismatch,
			Detail: "the content digest does not match the one recorded at prepare time"}
	}
	return evictionVerdict{Reason: evictReasonEvictable}
}
