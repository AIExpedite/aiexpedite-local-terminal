// File: worktree_git.go
// -----------------------------------------------------------------------------
// The bounded git calls the worktree built-ins make, and the two git facts that
// carry safety weight (PARALLEL_RUNS_AND_COMPUTER_MAINTENANCE_PLAN.md §4.4,
// §4.8, §9.2).
//
// TWO GIT SUBCOMMANDS ARE NEVER USED, and the reasons are recorded here because
// each would undo a safety property:
//
//   - `git worktree remove` deletes the working tree recursively ITSELF, so it
//     would traverse junctions before anything link-safe looked at the path.
//     `--force` only waives its dirty / locked refusals; it is not what makes it
//     delete. Removal is linkSafeRemoveAll, then the one admin record.
//   - `git worktree prune` takes NO target operand: it drops the record of
//     every worktree whose tree is missing, which is exactly what a person's
//     own worktree on an unmounted drive or a disconnected share looks like.
//     So the admin directory name is read WHILE the worktree still exists
//     (wtAdminDir) and, after the delete, that ONE directory under the main
//     repository's `.git/worktrees/` is removed and nothing else.
//
// Every call is bounded, runs with the headless environment overlay (so git can
// never open a terminal prompt and block) and hides the console window.
// -----------------------------------------------------------------------------

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// wtGitTimeout bounds an ordinary local git call. A fetch and a push get
// wtGitNetworkTimeout instead.
const (
	wtGitTimeout        = 60 * time.Second
	wtGitNetworkTimeout = 5 * time.Minute
)

// wtGitError carries git's own stderr, which is the only useful part of a
// failure (`fatal: '<branch>' is already used by worktree at …`).
type wtGitError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *wtGitError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), msg)
}

func (e *wtGitError) Unwrap() error { return e.Err }

// wtRunGitIn runs one git command in dir with a timeout, returning trimmed
// stdout. A non-zero exit is an error carrying stderr.
func wtRunGitIn(ctx context.Context, dir string, timeout time.Duration, args ...string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git is not on PATH: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(cctx, "git", full...)
	hideWindow(cmd)
	// The headless overlay is appended last so it always wins: git must never
	// reach for a terminal prompt or an editor on this path.
	cmd.Env = append(append([]string(nil), cmd.Environ()...), headlessEnvOverlay()...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(stdout.String()), &wtGitError{Args: full, Stderr: stderr.String(), Err: err}
	}
	return strings.TrimSpace(stdout.String()), nil
}

// wtGit is wtRunGitIn with the ordinary local timeout.
func wtGit(ctx context.Context, dir string, args ...string) (string, error) {
	return wtRunGitIn(ctx, dir, wtGitTimeout, args...)
}

// wtGitNetwork is wtRunGitIn with the network timeout (fetch, push).
func wtGitNetwork(ctx context.Context, dir string, args ...string) (string, error) {
	return wtRunGitIn(ctx, dir, wtGitNetworkTimeout, args...)
}

// gitWorktreeEntry is one record of `git worktree list --porcelain`.
type gitWorktreeEntry struct {
	Path     string
	Head     string
	Branch   string // "" when detached
	Detached bool
	Bare     bool
	Locked   bool
	Prunable bool
}

// parseWorktreeListPorcelain parses `git worktree list --porcelain`. Records are
// blank-line separated; `branch` carries a full ref name.
func parseWorktreeListPorcelain(out string) []gitWorktreeEntry {
	var entries []gitWorktreeEntry
	var cur *gitWorktreeEntry
	flush := func() {
		if cur != nil && cur.Path != "" {
			entries = append(entries, *cur)
		}
		cur = nil
	}
	for _, raw := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "worktree":
			flush()
			cur = &gitWorktreeEntry{Path: filepath.Clean(strings.TrimSpace(value))}
		case "HEAD":
			if cur != nil {
				cur.Head = strings.TrimSpace(value)
			}
		case "branch":
			if cur != nil {
				cur.Branch = strings.TrimPrefix(strings.TrimSpace(value), "refs/heads/")
			}
		case "detached":
			if cur != nil {
				cur.Detached = true
			}
		case "bare":
			if cur != nil {
				cur.Bare = true
			}
		case "locked":
			if cur != nil {
				cur.Locked = true
			}
		case "prunable":
			if cur != nil {
				cur.Prunable = true
			}
		}
	}
	flush()
	return entries
}

// wtListWorktrees returns every worktree git has registered for mainRepo.
func wtListWorktrees(ctx context.Context, mainRepo string) ([]gitWorktreeEntry, error) {
	out, err := wtGit(ctx, mainRepo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktreeListPorcelain(out), nil
}

// wtFindWorktree returns the registered entry whose path is the same directory
// as want (comparing the way the file system does).
func wtFindWorktree(entries []gitWorktreeEntry, want string) (gitWorktreeEntry, bool) {
	for _, e := range entries {
		if pathIdentityKey(e.Path) == pathIdentityKey(want) {
			return e, true
		}
	}
	return gitWorktreeEntry{}, false
}

// wtAdminDir resolves the ONE administrative directory that belongs to the
// worktree at path: `<mainRepo>/.git/worktrees/<name>`.
//
// It must be read WHILE the worktree still exists, because the pointer that
// names it is the worktree's own `.git` file.
//
// That pointer is NOT trusted. An agent working inside the worktree can
// rewrite `.git`, and the admin directory's back-pointer and `git worktree
// list` both read from the same files, so none of those three is an
// independent authority. What bounds the damage is the one input that did not
// come from the worktree: mainRepo, validated by resolveMainRepoPath from the
// dispatch. Four things are verified:
//
//  1. the pointer resolves under the MAIN repository's own
//     `<commonDir>/worktrees/`, resolved from mainRepo rather than from the
//     worktree — so a forged `.git` can only ever name a directory inside the
//     real main repository's bookkeeping, never an arbitrary path;
//  2. it resolves under the worktree's own commonDir too, and the two agree —
//     a disagreement means the worktree belongs to a different repository
//     than the one the dispatch named;
//  3. the admin directory's own `gitdir` file points back at this worktree;
//  4. git itself still lists the worktree at this path.
//
// (3) and (4) then pick the RIGHT record inside that bookkeeping; (1) is what
// makes a wrong answer survivable.
func wtAdminDir(ctx context.Context, mainRepo, path string) (string, error) {
	gitDir, commonDir, ok := resolveGitDirs(path)
	if !ok {
		return "", fmt.Errorf("%s is not a git worktree (no readable .git)", path)
	}
	// Resolved from the VALIDATED main repository, independently of anything the
	// worktree says about itself.
	_, mainCommonDir, mainOK := resolveGitDirs(mainRepo)
	if !mainOK {
		return "", fmt.Errorf("%s is not a readable git checkout", mainRepo)
	}
	mainWorktreesDir := filepath.Join(mainCommonDir, "worktrees")
	if !isPathWithin(mainWorktreesDir, gitDir) || pathIdentityKey(gitDir) == pathIdentityKey(mainWorktreesDir) {
		return "", fmt.Errorf("%s points its git dir at %s, which is not one worktree's admin directory under %s",
			path, gitDir, mainWorktreesDir)
	}
	// And the worktree's own view must agree, or it belongs to another
	// repository than the one the dispatch named.
	worktreesDir := filepath.Join(commonDir, "worktrees")
	if pathIdentityKey(worktreesDir) != pathIdentityKey(mainWorktreesDir) {
		return "", fmt.Errorf("%s belongs to %s, not to %s", path, worktreesDir, mainWorktreesDir)
	}
	// The back-pointer: the admin directory records the worktree's own `.git`.
	raw, err := os.ReadFile(filepath.Join(gitDir, "gitdir"))
	if err != nil {
		return "", fmt.Errorf("reading the admin back-pointer %s: %w", filepath.Join(gitDir, "gitdir"), err)
	}
	back := filepath.Clean(strings.TrimSpace(string(raw)))
	if pathIdentityKey(back) != pathIdentityKey(filepath.Join(path, ".git")) {
		return "", fmt.Errorf("admin directory %s points back at %s, not at %s", gitDir, back, filepath.Join(path, ".git"))
	}
	entries, err := wtListWorktrees(ctx, mainRepo)
	if err != nil {
		return "", err
	}
	if _, found := wtFindWorktree(entries, path); !found {
		return "", fmt.Errorf("git does not list a worktree at %s for %s", path, mainRepo)
	}
	return gitDir, nil
}

// wtWorktreeState is what a worktree reports about itself.
type wtWorktreeState struct {
	Head     string `json:"head"`
	Branch   string `json:"branch"`
	Detached bool   `json:"detached"`
	Dirty    bool   `json:"dirty"`
	// Ahead / Behind count commits against the branch's upstream. Both are
	// zero and HasUpstream false when there is none.
	Ahead       int  `json:"ahead"`
	Behind      int  `json:"behind"`
	HasUpstream bool `json:"hasUpstream"`
	// DirtyPaths is a bounded sample, so a refusal can say WHAT was dirty.
	DirtyPaths []string `json:"dirtyPaths,omitempty"`
}

// wtMaxDirtySample bounds the dirty-path sample in a result.
const wtMaxDirtySample = 10

// wtReadState reads a worktree's branch, head, dirtiness and ahead/behind.
//
// "Dirty" includes UNTRACKED files: §4.8 condition 1 is "no uncommitted or
// untracked changes", and an untracked file is precisely the content git cannot
// recover, so it is the one that most needs to block a destructive step.
func wtReadState(ctx context.Context, path string) (wtWorktreeState, error) {
	var st wtWorktreeState
	head, err := wtGit(ctx, path, "rev-parse", "--verify", "-q", "HEAD")
	if err == nil {
		st.Head = head
	}
	if branch, berr := wtGit(ctx, path, "symbolic-ref", "--short", "-q", "HEAD"); berr == nil && branch != "" {
		st.Branch = branch
	} else {
		st.Detached = true
	}
	status, err := wtGit(ctx, path, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return st, err
	}
	for _, line := range strings.Split(strings.ReplaceAll(status, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		st.Dirty = true
		if len(st.DirtyPaths) < wtMaxDirtySample {
			st.DirtyPaths = append(st.DirtyPaths, line)
		}
	}
	if st.Branch != "" {
		if counts, cerr := wtGit(ctx, path, "rev-list", "--left-right", "--count", "@{u}...HEAD"); cerr == nil {
			fields := strings.Fields(counts)
			if len(fields) == 2 {
				st.HasUpstream = true
				st.Behind, _ = strconv.Atoi(fields[0])
				st.Ahead, _ = strconv.Atoi(fields[1])
			}
		}
	}
	return st, nil
}

// wtIsAncestor reports whether ancestor is an ancestor of (or equal to)
// descendant. `merge-base --is-ancestor` exits 0 for yes and 1 for no, so an
// error is only ambiguous when it is not that exit.
func wtIsAncestor(ctx context.Context, repo, ancestor, descendant string) (bool, error) {
	_, err := wtGit(ctx, repo, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	var ge *wtGitError
	if errors.As(err, &ge) {
		var ee *exec.ExitError
		if errors.As(ge.Err, &ee) && ee.ExitCode() == 1 {
			return false, nil
		}
	}
	return false, err
}

// wtRevParse resolves a revision, returning "" with no error when it does not
// exist (`--verify -q` exits non-zero and prints nothing).
func wtRevParse(ctx context.Context, repo, rev string) (string, error) {
	out, err := wtGit(ctx, repo, "rev-parse", "--verify", "-q", rev)
	if err != nil {
		var ge *wtGitError
		if errors.As(err, &ge) && strings.TrimSpace(ge.Stderr) == "" {
			return "", nil
		}
		return "", err
	}
	return out, nil
}

// wtLocalBranchExists reports whether refs/heads/<branch> exists.
func wtLocalBranchExists(ctx context.Context, repo, branch string) (bool, error) {
	if branch == "" {
		return false, nil
	}
	sha, err := wtRevParse(ctx, repo, "refs/heads/"+branch)
	return sha != "", err
}

// wtRemoteBranchExists reports whether refs/remotes/<remote>/<branch> exists.
func wtRemoteBranchExists(ctx context.Context, repo, remote, branch string) (bool, error) {
	if branch == "" {
		return false, nil
	}
	sha, err := wtRevParse(ctx, repo, "refs/remotes/"+remote+"/"+branch)
	return sha != "", err
}

// wtUnpushedCommits returns the commits on the current branch that no remote
// has, as a bounded sample, plus whether any exist.
//
// Two ways to be pushed, per §9.2: `@{u}..HEAD` is empty, OR the branch is
// already reachable from `origin/<default>` (a merged PR whose branch was
// deleted on the remote has no upstream any more).
func wtUnpushedCommits(ctx context.Context, path, remote, defaultBranch string) (unpushed bool, sample []string, err error) {
	out, uerr := wtGit(ctx, path, "rev-list", "--max-count=10", "@{u}..HEAD")
	if uerr == nil {
		lines := nonEmptyLines(out)
		if len(lines) == 0 {
			return false, nil, nil
		}
		return true, lines, nil
	}
	// No upstream: fall back to reachability from the default branch.
	if defaultBranch == "" {
		return true, nil, fmt.Errorf("no upstream for %s and no default branch to compare against: %w", path, uerr)
	}
	target := remote + "/" + defaultBranch
	sha, rerr := wtRevParse(ctx, path, "refs/remotes/"+target)
	if rerr != nil || sha == "" {
		return true, nil, fmt.Errorf("no upstream for %s and %s does not exist locally", path, target)
	}
	merged, merr := wtIsAncestor(ctx, path, "HEAD", sha)
	if merr != nil {
		return true, nil, merr
	}
	if merged {
		return false, nil, nil
	}
	out, _ = wtGit(ctx, path, "rev-list", "--max-count=10", sha+"..HEAD")
	return true, nonEmptyLines(out), nil
}

// wtTrackedFilesUnder returns a bounded sample of TRACKED files inside rel.
// A single tracked file disqualifies a directory from eviction, and an
// unmodified tracked file never appears in `git status`, so ls-files is the
// only check that sees it.
func wtTrackedFilesUnder(ctx context.Context, worktree, rel string) ([]string, error) {
	out, err := wtGit(ctx, worktree, "ls-files", "-z", "--", rel)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(out, "\x00") {
		if strings.TrimSpace(f) == "" {
			continue
		}
		files = append(files, f)
		if len(files) >= wtMaxDirtySample {
			break
		}
	}
	return files, nil
}

// wtPathIsIgnored reports whether git ignores rel. `check-ignore -q` exits 0
// when ignored, 1 when not, and >1 on a real error.
func wtPathIsIgnored(ctx context.Context, worktree, rel string) (bool, error) {
	_, err := wtGit(ctx, worktree, "check-ignore", "-q", "--", rel)
	if err == nil {
		return true, nil
	}
	var ge *wtGitError
	if errors.As(err, &ge) {
		var ee *exec.ExitError
		if errors.As(ge.Err, &ee) && ee.ExitCode() == 1 {
			return false, nil
		}
	}
	return false, err
}

// wtModifiedUnder returns a bounded sample of modified TRACKED content inside
// rel (status entries that are not `??` untracked or `!!` ignored).
func wtModifiedUnder(ctx context.Context, worktree, rel string) ([]string, error) {
	out, err := wtGit(ctx, worktree, "status", "--porcelain", "--", rel)
	if err != nil {
		return nil, err
	}
	var modified []string
	for _, line := range nonEmptyLines(out) {
		if strings.HasPrefix(line, "??") || strings.HasPrefix(line, "!!") {
			continue
		}
		modified = append(modified, line)
		if len(modified) >= wtMaxDirtySample {
			break
		}
	}
	return modified, nil
}

func nonEmptyLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	return lines
}
