package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ─── fixture ────────────────────────────────────────────────────────────────

type wtFixture struct {
	Base     string
	Root     string // the recorded root (the computer's Default working directory)
	MainRepo string // <Root>/demo — the root checkout
	Remote   string // a bare repository standing in for origin
	Default  string
}

func wtMustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := wtGit(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %s in %s: %v", strings.Join(args, " "), dir, err)
	}
	return out
}

func wtConfigRepo(t *testing.T, repo string) {
	t.Helper()
	wtMustGit(t, repo, "config", "user.email", "test@example.invalid")
	wtMustGit(t, repo, "config", "user.name", "AIX Test")
	wtMustGit(t, repo, "config", "commit.gpgsign", "false")
}

func wtWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newWorktreeFixture builds a bare "origin", a seed clone that pushes the
// default branch, and a root checkout under a root that stands in for the
// computer's Default working directory.
func newWorktreeFixture(t *testing.T) wtFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	base := tempRootDir(t)
	fx := wtFixture{Base: base, Default: "main"}
	fx.Remote = filepath.Join(base, "remote.git")
	wtMustGit(t, base, "init", "--bare", "--initial-branch=main", fx.Remote)

	seed := filepath.Join(base, "seed")
	wtMustGit(t, base, "init", "--initial-branch=main", seed)
	wtConfigRepo(t, seed)
	wtWrite(t, filepath.Join(seed, "README.md"), "demo\n")
	wtWrite(t, filepath.Join(seed, ".gitignore"), "node_modules/\ndist/\n")
	wtMustGit(t, seed, "add", "-A")
	wtMustGit(t, seed, "commit", "-m", "initial")
	wtMustGit(t, seed, "remote", "add", "origin", fx.Remote)
	wtMustGit(t, seed, "push", "-u", "origin", "main")

	fx.Root = filepath.Join(base, "root")
	if err := os.MkdirAll(fx.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	wtMustGit(t, fx.Root, "clone", fx.Remote, "demo")
	fx.MainRepo = filepath.Join(fx.Root, "demo")
	wtConfigRepo(t, fx.MainRepo)
	return fx
}

// seedCommit adds a commit to the default branch on the remote, so a fetch has
// something new to find.
func (fx wtFixture) seedCommit(t *testing.T, name string) string {
	t.Helper()
	seed := filepath.Join(fx.Base, "seed")
	wtWrite(t, filepath.Join(seed, name), name)
	wtMustGit(t, seed, "add", "-A")
	wtMustGit(t, seed, "commit", "-m", "add "+name)
	wtMustGit(t, seed, "push", "origin", "main")
	return wtMustGit(t, seed, "rev-parse", "HEAD")
}

func (fx wtFixture) wtPath(repo string) string {
	// Fixtures name a repository the way a person does. The server appends the
	// discriminator, so the helper does too — a name that already carries one
	// is left alone, so a test can still pin an exact segment.
	if !hasWorktreeRepoDiscriminator(repo) {
		repo += wtTestRepoDiscriminator
	}
	return filepath.Join("aixwt", wtTestFeatureFolder, repo)
}

func (fx wtFixture) ensureReq(branch string) worktreeEnsureRequest {
	no := false
	return worktreeEnsureRequest{
		Root:          fx.Root,
		MainRepo:      fx.MainRepo,
		Path:          fx.wtPath("demo"),
		Branch:        branch,
		DefaultBranch: fx.Default,
		Remote:        "origin",
		Fetch:         &no,
	}
}

// ─── __worktree_ensure__ ────────────────────────────────────────────────────

func TestEnsureWorktreeCreatesFromTheDefaultBranch(t *testing.T) {
	fx := newWorktreeFixture(t)
	res, err := ensureWorktree(context.Background(), fx.ensureReq("feature/new-thing"))
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !res.Created || !res.Registered {
		t.Fatalf("res = %+v, want created and registered", res)
	}
	if res.Branch != "feature/new-thing" {
		t.Fatalf("branch = %q", res.Branch)
	}
	if res.Dirty {
		t.Fatalf("a fresh worktree should be clean, dirty paths: %v", res.DirtyPaths)
	}
	entries, err := wtListWorktrees(context.Background(), fx.MainRepo)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := wtFindWorktree(entries, res.Path); !ok {
		t.Fatalf("git does not list the worktree at %s", res.Path)
	}
}

// TestEnsureWorktreeAttachesAnExistingLocalBranchWithoutResettingIt is rule 5,
// written as the loss it prevents.
//
// `git worktree add -B` is documented as "create or RESET a branch". The Phase
// 1 transition inherits exactly this state: today's author creates the feature
// branch in the ROOT checkout, so an unpushed local branch of that name is the
// expected starting point. Had -B been used, the branch ref would have moved to
// origin/main and those commits would have lost their only named reference.
func TestEnsureWorktreeAttachesAnExistingLocalBranchWithoutResettingIt(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	wtMustGit(t, fx.MainRepo, "checkout", "-b", "feature/inherited")
	wtWrite(t, filepath.Join(fx.MainRepo, "work.txt"), "unpushed work\n")
	wtMustGit(t, fx.MainRepo, "add", "-A")
	wtMustGit(t, fx.MainRepo, "commit", "-m", "unpushed")
	unpushedSha := wtMustGit(t, fx.MainRepo, "rev-parse", "HEAD")
	wtMustGit(t, fx.MainRepo, "checkout", "main")

	res, err := ensureWorktree(ctx, fx.ensureReq("feature/inherited"))
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if res.Head != unpushedSha {
		t.Fatalf("HEAD = %s, want the inherited unpushed commit %s — the ref was reset", res.Head, unpushedSha)
	}
	after := wtMustGit(t, fx.MainRepo, "rev-parse", "refs/heads/feature/inherited")
	if after != unpushedSha {
		t.Fatalf("the branch ref moved to %s; -B (or a reset) was used", after)
	}
	// The ahead/behind report is what tells the run to reconcile.
	if res.Ahead == 0 && res.Behind == 0 && !res.HasUpstream {
		t.Log("no upstream, as expected for a local-only branch")
	}
}

func TestEnsureWorktreeTracksARemoteOnlyBranch(t *testing.T) {
	fx := newWorktreeFixture(t)
	seed := filepath.Join(fx.Base, "seed")
	wtMustGit(t, seed, "checkout", "-b", "feature/remote-only")
	wtWrite(t, filepath.Join(seed, "remote.txt"), "remote\n")
	wtMustGit(t, seed, "add", "-A")
	wtMustGit(t, seed, "commit", "-m", "remote only")
	remoteSha := wtMustGit(t, seed, "rev-parse", "HEAD")
	wtMustGit(t, seed, "push", "origin", "feature/remote-only")
	wtMustGit(t, seed, "checkout", "main")

	req := fx.ensureReq("feature/remote-only")
	yes := true
	req.Fetch = &yes
	res, err := ensureWorktree(context.Background(), req)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if res.Branch != "feature/remote-only" || res.Head != remoteSha {
		t.Fatalf("res = %+v, want the remote branch's head %s", res, remoteSha)
	}
	if !res.HasUpstream {
		t.Fatal("a branch created with --track should have an upstream")
	}
}

func TestEnsureWorktreeRefusesWhenTheRootHoldsTheBranch(t *testing.T) {
	fx := newWorktreeFixture(t)
	wtMustGit(t, fx.MainRepo, "checkout", "-b", "feature/held-by-root")
	_, err := ensureWorktree(context.Background(), fx.ensureReq("feature/held-by-root"))
	if err == nil {
		t.Fatal("expected a refusal naming the root checkout")
	}
	if !strings.Contains(err.Error(), "__root_restore__") {
		t.Fatalf("the refusal should name the remedy, got: %v", err)
	}
}

func TestEnsureWorktreeRefusesAnUnregisteredDirectoryAtThePath(t *testing.T) {
	fx := newWorktreeFixture(t)
	target := filepath.Join(fx.Root, fx.wtPath("demo"))
	wtWrite(t, filepath.Join(target, "someones-file.txt"), "mine\n")
	_, err := ensureWorktree(context.Background(), fx.ensureReq("feature/x"))
	if err == nil {
		t.Fatal("expected a refusal: a directory git does not know about is never clobbered")
	}
	if _, serr := os.Stat(filepath.Join(target, "someones-file.txt")); serr != nil {
		t.Fatalf("the existing directory was touched: %v", serr)
	}
}

func TestEnsureWorktreeRefusesABadPath(t *testing.T) {
	fx := newWorktreeFixture(t)
	for name, path := range map[string]string{
		"outside the root": filepath.Join(fx.Base, "elsewhere"),
		"bare ..":          "aixwt/" + wtTestFeatureFolder + "/..",
		"trailing dot":     "aixwt/" + wtTestFeatureFolder + "/demo.",
	} {
		t.Run(name, func(t *testing.T) {
			req := fx.ensureReq("feature/x")
			req.Path = path
			if _, err := ensureWorktree(context.Background(), req); err == nil {
				t.Fatalf("admitted %q", path)
			} else if worktreePolicyRuleOf(err) == "" {
				t.Fatalf("the refusal should name a policy rule, got: %v", err)
			}
		})
	}
}

// TestEnsureSeedsAllowlistedFilesOnlyAtCreation is rule 7.
//
// Step 4 runs on EVERY refresh and these files are untracked, so re-copying the
// root's version would destroy work git cannot recover. "Copy when the
// destination is absent" is not enough either: an agent that deliberately
// DELETED an allowlisted file would have it silently restored, and the feature
// would run with configuration it had explicitly removed.
func TestEnsureSeedsAllowlistedFilesOnlyAtCreation(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	wtWrite(t, filepath.Join(fx.MainRepo, ".env.local"), "ROOT=1\n")

	req := fx.ensureReq("feature/seeded")
	req.CopyUntracked = []string{".env.local"}
	first, err := ensureWorktree(ctx, req)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(first.SeededNow) != 1 || first.SeededNow[0] != ".env.local" {
		t.Fatalf("seededNow = %v, want the allowlisted file seeded at creation", first.SeededNow)
	}
	seeded := filepath.Join(first.Path, ".env.local")
	if body, rerr := os.ReadFile(seeded); rerr != nil || string(body) != "ROOT=1\n" {
		t.Fatalf("seeded file = %q, %v", body, rerr)
	}

	// The agent edits it, and the root's copy changes too.
	wtWrite(t, seeded, "AGENT=1\n")
	wtWrite(t, filepath.Join(fx.MainRepo, ".env.local"), "ROOT=2\n")

	req.Seeded = []string{".env.local"}
	second, err := ensureWorktree(ctx, req)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if second.Created {
		t.Fatal("the second call should have found the worktree, not created it")
	}
	if len(second.SeededNow) != 0 {
		t.Fatalf("seededNow = %v on a refresh; seeding happens ONCE, at creation", second.SeededNow)
	}
	if body, _ := os.ReadFile(seeded); string(body) != "AGENT=1\n" {
		t.Fatalf("the agent's edit was overwritten: %q", body)
	}
	if len(second.SeedReports) != 1 || second.SeedReports[0].State != "differs" {
		t.Fatalf("seedReports = %+v, want the difference REPORTED", second.SeedReports)
	}

	// And a file the agent deliberately deleted stays deleted, reported as
	// missing rather than silently restored.
	if err := os.Remove(seeded); err != nil {
		t.Fatal(err)
	}
	third, err := ensureWorktree(ctx, req)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, serr := os.Stat(seeded); serr == nil {
		t.Fatal("a deliberately deleted allowlisted file was silently restored")
	}
	if len(third.SeedReports) != 1 || third.SeedReports[0].State != "missing" {
		t.Fatalf("seedReports = %+v, want state missing", third.SeedReports)
	}
}

// TestEnsureCompanionIsDetachedAndAdvancesOnlyWhenSafe is rule 6.
func TestEnsureCompanionIsDetachedAndAdvancesOnlyWhenSafe(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	yes := true

	req := fx.ensureReq("feature/with-companion")
	req.Fetch = &yes
	req.Companions = []worktreeCompanionSpec{{
		MainRepo:      fx.MainRepo,
		Path:          fx.wtPath("lib"),
		DefaultBranch: fx.Default,
		Remote:        "origin",
	}}
	first, err := ensureWorktree(ctx, req)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(first.Companions) != 1 || !first.Companions[0].Created {
		t.Fatalf("companions = %+v, want one created", first.Companions)
	}
	companion := first.Companions[0].Path
	state, err := wtReadState(ctx, companion)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Detached {
		t.Fatal("a companion must be DETACHED: the root holds the default branch, and git refuses one branch in two worktrees")
	}

	// A new default-branch commit: the companion advances, because it is clean
	// and carries nothing of its own.
	newSha := fx.seedCommit(t, "newer.txt")
	second, err := ensureWorktree(ctx, req)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !second.Companions[0].Advanced || second.Companions[0].Head != newSha {
		t.Fatalf("companion = %+v, want advanced to %s", second.Companions[0], newSha)
	}

	// Now it holds a commit of its own while the working tree looks pristine.
	// "Clean" alone is not enough: re-detaching would leave that commit
	// reachable only through the reflog.
	wtWrite(t, filepath.Join(companion, "companion-work.txt"), "local\n")
	wtMustGit(t, companion, "add", "-A")
	wtMustGit(t, companion, "commit", "-m", "work inside the companion")
	ownSha := wtMustGit(t, companion, "rev-parse", "HEAD")
	fx.seedCommit(t, "newest.txt")

	third, err := ensureWorktree(ctx, req)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	got := third.Companions[0]
	if got.Advanced {
		t.Fatal("a diverged companion must be left exactly as it is")
	}
	if !got.Diverged || got.Reason != "left_alone_diverged" {
		t.Fatalf("companion = %+v, want diverged and reported", got)
	}
	if head := wtMustGit(t, companion, "rev-parse", "HEAD"); head != ownSha {
		t.Fatalf("the companion's HEAD moved from %s to %s", ownSha, head)
	}
}

func TestEnsureCompanionWithLocalModificationsIsLeftAlone(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	yes := true
	req := fx.ensureReq("feature/dirty-companion")
	req.Fetch = &yes
	req.Companions = []worktreeCompanionSpec{{
		MainRepo: fx.MainRepo, Path: fx.wtPath("lib"), DefaultBranch: fx.Default, Remote: "origin",
	}}
	first, err := ensureWorktree(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	companion := first.Companions[0].Path
	wtWrite(t, filepath.Join(companion, "README.md"), "edited by hand\n")
	fx.seedCommit(t, "later.txt")

	second, err := ensureWorktree(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	got := second.Companions[0]
	if got.Advanced || got.Reason != "left_alone_dirty" {
		t.Fatalf("companion = %+v, want left alone and reported as dirty", got)
	}
	if body, _ := os.ReadFile(filepath.Join(companion, "README.md")); string(body) != "edited by hand\n" {
		t.Fatalf("the modification was discarded: %q", body)
	}
}

// ─── provenance marker and depsReady ────────────────────────────────────────

func TestEnsureRecordsAndReconcilesDepsProvenance(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	// A lockfile the install would have come from.
	wtWrite(t, filepath.Join(fx.MainRepo, "package-lock.json"), `{"v":1}`)
	wtMustGit(t, fx.MainRepo, "add", "-A")
	wtMustGit(t, fx.MainRepo, "commit", "-m", "lockfile")
	wtMustGit(t, fx.MainRepo, "push", "origin", "main")

	req := fx.ensureReq("feature/deps")
	req.Deps = []worktreeDepsSpec{{Ecosystem: "npm", Dir: "node_modules", Lockfile: "package-lock.json"}}
	first, err := ensureWorktree(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.DepsReady {
		t.Fatal("depsReady must be false before anything is installed")
	}
	if first.Deps[0].State != "absent" {
		t.Fatalf("state = %q, want absent", first.Deps[0].State)
	}

	// The install runs, then the server asks for the marker.
	deps := filepath.Join(first.Path, "node_modules", "pkg")
	wtWrite(t, filepath.Join(deps, "index.js"), "module.exports = 1\n")
	req.MarkDeps = true
	marked, err := ensureWorktree(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !marked.Deps[0].Marked || !marked.DepsReady || marked.Deps[0].State != "ready" {
		t.Fatalf("deps = %+v, depsReady = %v", marked.Deps[0], marked.DepsReady)
	}
	if !marked.Deps[0].BaselineComplete || !marked.Deps[0].TreeDigestComplete {
		t.Fatalf("the marker must carry a complete baseline AND a complete content digest: %+v", marked.Deps[0])
	}

	// A lockfile-only change makes it stale, which is the drift npm's own
	// hidden lockfile would also catch — but a virtualenv or `vendor/bundle`
	// would NOT, which is why the platform records its own provenance.
	wtWrite(t, filepath.Join(first.Path, "package-lock.json"), `{"v":2}`)
	req.MarkDeps = false
	stale, err := ensureWorktree(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if stale.DepsReady || stale.Deps[0].State != "stale" {
		t.Fatalf("deps = %+v, depsReady = %v, want stale", stale.Deps[0], stale.DepsReady)
	}
}

// ─── __worktree_list__ ──────────────────────────────────────────────────────

func TestListWorktreesReportsStateAndCountsOthersWithoutNamingThem(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	if _, err := ensureWorktree(ctx, fx.ensureReq("feature/listed")); err != nil {
		t.Fatal(err)
	}
	// A worktree the PERSON made, outside aixwt. It must never be named back
	// to the server: a path in the result invites a registry row that targets
	// it, and unknown worktrees are never touched at any level.
	mine := filepath.Join(fx.Base, "my-own-worktree")
	wtMustGit(t, fx.MainRepo, "worktree", "add", "--detach", mine)

	res, err := listWorktrees(ctx, worktreeListRequest{Root: fx.Root, MainRepos: []string{fx.MainRepo}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Worktrees) != 1 {
		t.Fatalf("worktrees = %+v, want exactly ours", res.Worktrees)
	}
	if res.OtherWorktreeCount != 1 {
		t.Fatalf("otherWorktreeCount = %d, want 1", res.OtherWorktreeCount)
	}
	row := res.Worktrees[0]
	if row.Branch != "feature/listed" || row.Dirty || row.RepoFolder != "demo"+wtTestRepoDiscriminator {
		t.Fatalf("row = %+v", row)
	}

	// Dirty and ahead are reported.
	wtWrite(t, filepath.Join(row.Path, "scratch.txt"), "x\n")
	wtMustGit(t, row.Path, "add", "-A")
	wtMustGit(t, row.Path, "commit", "-m", "one ahead")
	wtWrite(t, filepath.Join(row.Path, "untracked.txt"), "y\n")

	res, err = listWorktrees(ctx, worktreeListRequest{Root: fx.Root, MainRepos: []string{fx.MainRepo}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Worktrees[0].Dirty {
		t.Fatal("an untracked file must read as dirty")
	}
}

// ─── __worktree_remove__ ────────────────────────────────────────────────────

func (fx wtFixture) removeReq(path string) worktreeRemoveRequest {
	return worktreeRemoveRequest{
		Root:          fx.Root,
		MainRepo:      fx.MainRepo,
		Path:          path,
		Remote:        "origin",
		DefaultBranch: fx.Default,
	}
}

func TestRemoveWorktreeKeepsAnUncommittedTree(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	created, err := ensureWorktree(ctx, fx.ensureReq("feature/dirty"))
	if err != nil {
		t.Fatal(err)
	}
	precious := filepath.Join(created.Path, "not-committed.txt")
	wtWrite(t, precious, "work git cannot recover\n")

	res, err := removeWorktree(ctx, fx.removeReq(fx.wtPath("demo")))
	if err != nil {
		t.Fatalf("removeWorktree: %v", err)
	}
	if res.Removed || !res.Kept || res.KeptReason != keptReasonUncommittedWork {
		t.Fatalf("res = %+v, want kept with %q", res, keptReasonUncommittedWork)
	}
	if _, serr := os.Stat(precious); serr != nil {
		t.Fatalf("uncommitted work was deleted: %v", serr)
	}
}

func TestRemoveWorktreeKeepsUnpushedWorkWhenRescuePushingIsOff(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	created, err := ensureWorktree(ctx, fx.ensureReq("feature/unpushed"))
	if err != nil {
		t.Fatal(err)
	}
	wtWrite(t, filepath.Join(created.Path, "work.txt"), "committed but unpushed\n")
	wtMustGit(t, created.Path, "add", "-A")
	wtMustGit(t, created.Path, "commit", "-m", "unpushed")

	req := fx.removeReq(fx.wtPath("demo"))
	off := false
	req.AllowRescuePush = &off
	res, err := removeWorktree(ctx, req)
	if err != nil {
		t.Fatalf("removeWorktree: %v", err)
	}
	if res.Removed || res.KeptReason != keptReasonUnpushedWork {
		t.Fatalf("res = %+v, want kept with %q", res, keptReasonUnpushedWork)
	}
	if _, serr := os.Stat(created.Path); serr != nil {
		t.Fatalf("the worktree was deleted: %v", serr)
	}
}

func TestRemoveWorktreeKeepsOnABranchMismatch(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	if _, err := ensureWorktree(ctx, fx.ensureReq("feature/actual")); err != nil {
		t.Fatal(err)
	}
	req := fx.removeReq(fx.wtPath("demo"))
	req.Branch = "feature/what-the-server-thinks"
	res, err := removeWorktree(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed || res.KeptReason != keptReasonBranchMismatch {
		t.Fatalf("res = %+v, want kept with %q", res, keptReasonBranchMismatch)
	}
}

func TestRemoveWorktreeRescuePushesThenRemoves(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	created, err := ensureWorktree(ctx, fx.ensureReq("feature/rescued"))
	if err != nil {
		t.Fatal(err)
	}
	wtWrite(t, filepath.Join(created.Path, "work.txt"), "only here\n")
	wtMustGit(t, created.Path, "add", "-A")
	wtMustGit(t, created.Path, "commit", "-m", "only copy")
	sha := wtMustGit(t, created.Path, "rev-parse", "HEAD")

	res, err := removeWorktree(ctx, fx.removeReq(fx.wtPath("demo")))
	if err != nil {
		t.Fatalf("removeWorktree: %v", err)
	}
	if !res.RescuePushed || !res.Removed || !res.AdminDirRemoved {
		t.Fatalf("res = %+v, want rescue pushed then removed", res)
	}
	if _, serr := os.Lstat(created.Path); !os.IsNotExist(serr) {
		t.Fatalf("the worktree directory survived: %v", serr)
	}
	// The commit must be on the remote under aix/rescue/….
	rescued := wtMustGit(t, fx.Remote, "rev-parse", "refs/heads/"+res.RescueBranch)
	if rescued != sha {
		t.Fatalf("rescue ref is at %s, want %s", rescued, sha)
	}
}

// TestRemoveWorktreeNeverPrunesAnotherWorktreesRecord is rule 2.
//
// `git worktree prune` takes NO target operand: it drops the record of every
// worktree whose tree is missing. A person's own worktree on an unmounted drive
// or a disconnected network share looks exactly like that — so the removal
// sequence reads ONE admin directory name by path and removes only that one.
func TestRemoveWorktreeNeverPrunesAnotherWorktreesRecord(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	if _, err := ensureWorktree(ctx, fx.ensureReq("feature/removable")); err != nil {
		t.Fatal(err)
	}
	// A person's worktree whose drive is "unmounted": registered, tree gone.
	mine := filepath.Join(fx.Base, "on-a-removable-drive")
	wtMustGit(t, fx.MainRepo, "worktree", "add", "--detach", mine)
	mineAdmin, err := wtAdminDir(ctx, fx.MainRepo, mine)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := linkSafeRemoveAll(mine); err != nil {
		t.Fatal(err)
	}
	if _, serr := os.Stat(mineAdmin); serr != nil {
		t.Fatalf("premise wrong: the admin record should still be there: %v", serr)
	}

	res, err := removeWorktree(ctx, fx.removeReq(fx.wtPath("demo")))
	if err != nil {
		t.Fatalf("removeWorktree: %v", err)
	}
	if !res.Removed || !res.AdminDirRemoved {
		t.Fatalf("res = %+v", res)
	}
	if _, serr := os.Stat(mineAdmin); serr != nil {
		t.Fatalf("the person's worktree record was dropped — prune semantics leaked in: %v", serr)
	}
}

func TestRemoveWorktreeRefusesABadPathNamingTheRule(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	if _, err := ensureWorktree(ctx, fx.ensureReq("feature/safe")); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		path string
		rule string
	}{
		"outside the root":      {filepath.Join(fx.Base, "seed"), rulePathNotUnderRoot},
		"the shared aixwt dir":  {"aixwt/" + wtTestFeatureFolder + "/..", rulePathSegmentCount},
		"the feature folder":    {"aixwt/" + wtTestFeatureFolder + "/.", rulePathSegmentCount},
		"a trailing dot":        {"aixwt/" + wtTestFeatureFolder + "/demo.", rulePathRepoDiscriminator},
		"an all-dots segment":   {"aixwt/" + wtTestFeatureFolder + "/...", rulePathRepoTraversal},
		"nested below the repo": {"aixwt/" + wtTestFeatureFolder + "/demo/src", rulePathSegmentCount},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := removeWorktree(ctx, fx.removeReq(tc.path))
			if err == nil {
				t.Fatalf("admitted %q (res = %+v)", tc.path, res)
			}
			if got := worktreePolicyRuleOf(err); got != tc.rule {
				t.Fatalf("rule = %q, want %q (%v)", got, tc.rule, err)
			}
		})
	}
	// And nothing was deleted on the way.
	if _, serr := os.Stat(filepath.Join(fx.Root, fx.wtPath("demo"))); serr != nil {
		t.Fatalf("a refused removal deleted something: %v", serr)
	}
}

func TestRemoveWorktreeRefusesALinkedPath(t *testing.T) {
	fx := newWorktreeFixture(t)
	sibling := filepath.Join(fx.Base, "sibling-checkout")
	if err := os.MkdirAll(filepath.Join(sibling, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	wtWrite(t, filepath.Join(sibling, "precious.txt"), "do not delete\n")
	if err := os.MkdirAll(filepath.Join(fx.Root, "aixwt", wtTestFeatureFolder), 0o755); err != nil {
		t.Fatal(err)
	}
	// The link has to stand where the POLICY will walk, which is the
	// discriminated folder name the server derives — not the bare checkout
	// name. Linking the wrong one made this test pass vacuously.
	makeAbsDirLink(t, filepath.Join(fx.Root, "aixwt", wtTestFeatureFolder, "demo"+wtTestRepoDiscriminator), sibling)

	_, err := removeWorktree(context.Background(), fx.removeReq(fx.wtPath("demo")))
	if err == nil {
		t.Fatal("admitted a worktree path that is a link into a sibling checkout")
	}
	if got := worktreePolicyRuleOf(err); got != rulePathComponentIsLink {
		t.Fatalf("rule = %q, want %q (%v)", got, rulePathComponentIsLink, err)
	}
	if _, serr := os.Stat(filepath.Join(sibling, "precious.txt")); serr != nil {
		t.Fatalf("the sibling checkout was touched: %v", serr)
	}
}

// TestRemoveWorktreeDeletesLinkSafely puts a junction into the worktree's own
// dependency directory — the common case, not the rare one — and proves the
// sibling survives.
func TestRemoveWorktreeDeletesLinkSafely(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	created, err := ensureWorktree(ctx, fx.ensureReq("feature/with-junction"))
	if err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(fx.Base, "shared-constants")
	if err := os.MkdirAll(filepath.Join(sibling, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	wtWrite(t, filepath.Join(sibling, "src", "index.js"), "shared\n")
	scope := filepath.Join(created.Path, "node_modules", "@aiexpedite")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	makeAbsDirLink(t, filepath.Join(scope, "shared-constants"), sibling)

	res, err := removeWorktree(ctx, fx.removeReq(fx.wtPath("demo")))
	if err != nil {
		t.Fatalf("removeWorktree: %v", err)
	}
	if !res.Removed {
		t.Fatalf("res = %+v", res)
	}
	if res.Deleted.Links != 1 {
		t.Fatalf("links unlinked = %d, want 1", res.Deleted.Links)
	}
	if _, serr := os.Stat(filepath.Join(sibling, "src", "index.js")); serr != nil {
		t.Fatalf("the sibling repository was emptied through the junction: %v", serr)
	}
}

// ─── __worktree_evict_deps__ ────────────────────────────────────────────────

func (fx wtFixture) evictReq(dirs ...string) worktreeEvictDepsRequest {
	return worktreeEvictDepsRequest{
		Root: fx.Root, MainRepo: fx.MainRepo, Path: fx.wtPath("demo"), Dirs: dirs,
	}
}

// markDirForTest records a provenance marker the way `prepare` would.
func markDirForTest(t *testing.T, wtPath, rel string) {
	t.Helper()
	wt, err := resolveWorktreePath(filepath.Dir(filepath.Dir(filepath.Dir(wtPath))), wtPath)
	if err != nil {
		t.Fatal(err)
	}
	cand, err := resolveInsideWorktree(wt, rel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recordDepsMarker(wt, worktreeDepsSpec{Ecosystem: "npm", Dir: rel}, cand.Abs, "lockdigest"); err != nil {
		t.Fatal(err)
	}
}

func TestEvictDepsKeepsADirectoryWithNoMarker(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	created, err := ensureWorktree(ctx, fx.ensureReq("feature/evict"))
	if err != nil {
		t.Fatal(err)
	}
	// Build output the agent produced inside its own CLI session: ignored by
	// git, large, rebuildable — and with NO baseline, so not evictable.
	wtWrite(t, filepath.Join(created.Path, "dist", "bundle.js"), "built\n")

	res, err := evictWorktreeDeps(ctx, fx.evictReq("dist"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Evicted) != 0 || len(res.Kept) != 1 || res.Kept[0].Reason != evictReasonNoMarker {
		t.Fatalf("res = %+v, want kept with %q", res, evictReasonNoMarker)
	}
	if _, serr := os.Stat(filepath.Join(created.Path, "dist", "bundle.js")); serr != nil {
		t.Fatalf("markerless build output was deleted: %v", serr)
	}
}

func TestEvictDepsKeepsADirectoryGitDoesNotIgnore(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	created, err := ensureWorktree(ctx, fx.ensureReq("feature/evict-unignored"))
	if err != nil {
		t.Fatal(err)
	}
	wtWrite(t, filepath.Join(created.Path, "generated", "a.js"), "x\n")
	markDirForTest(t, created.Path, "generated")

	res, err := evictWorktreeDeps(ctx, fx.evictReq("generated"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Kept) != 1 || res.Kept[0].Reason != evictReasonNotIgnored {
		t.Fatalf("res = %+v, want kept with %q", res, evictReasonNotIgnored)
	}
}

func TestEvictDepsKeepsADirectoryHoldingTrackedFiles(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	// A repository that commits a generated bundle on purpose, inside a path
	// that is also gitignored by pattern. A single tracked file inside
	// disqualifies the whole directory.
	wtWrite(t, filepath.Join(fx.MainRepo, "dist", "committed.js"), "on purpose\n")
	wtMustGit(t, fx.MainRepo, "add", "-f", "dist/committed.js")
	wtMustGit(t, fx.MainRepo, "commit", "-m", "commit a generated bundle on purpose")
	wtMustGit(t, fx.MainRepo, "push", "origin", "main")

	created, err := ensureWorktree(ctx, fx.ensureReq("feature/tracked-dist"))
	if err != nil {
		t.Fatal(err)
	}
	markDirForTest(t, created.Path, "dist")
	res, err := evictWorktreeDeps(ctx, fx.evictReq("dist"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Kept) != 1 || res.Kept[0].Reason != evictReasonTrackedContent {
		t.Fatalf("res = %+v, want kept with %q", res, evictReasonTrackedContent)
	}
	if _, serr := os.Stat(filepath.Join(created.Path, "dist", "committed.js")); serr != nil {
		t.Fatalf("a tracked file was deleted: %v", serr)
	}
}

// TestEvictDepsKeepsATouchedTree is the "creation marker is not enough" rule: an
// agent that patched an ignored dependency to debug it passes every other
// check.
func TestEvictDepsKeepsATouchedTree(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	created, err := ensureWorktree(ctx, fx.ensureReq("feature/touched"))
	if err != nil {
		t.Fatal(err)
	}
	patched := filepath.Join(created.Path, "node_modules", "pkg", "index.js")
	wtWrite(t, patched, "original\n")
	markDirForTest(t, created.Path, "node_modules")

	// The agent patches it to debug something.
	wtWrite(t, patched, "patched by the agent to debug\n")

	res, err := evictWorktreeDeps(ctx, fx.evictReq("node_modules"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Evicted) != 0 || len(res.Kept) != 1 {
		t.Fatalf("res = %+v, want kept", res)
	}
	if res.Kept[0].Reason != evictReasonBaselineMismatch {
		t.Fatalf("reason = %q, want %q", res.Kept[0].Reason, evictReasonBaselineMismatch)
	}
	if res.Kept[0].Diff == nil || res.Kept[0].Diff.Total == 0 {
		t.Fatalf("the diff must be reported: %+v", res.Kept[0])
	}
	if res.Kept[0].Export == "" {
		t.Fatal("what can be saved is exported before the directory is kept")
	}
	if body, _ := os.ReadFile(patched); string(body) != "patched by the agent to debug\n" {
		t.Fatalf("the patched file was deleted or changed: %q", body)
	}
}

// TestEvictDepsSeesADeletionInsideAnIgnoredTree is why a MANIFEST and not bare
// mtimes: an mtime comparison can only see entries that still exist.
func TestEvictDepsSeesADeletionInsideAnIgnoredTree(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	created, err := ensureWorktree(ctx, fx.ensureReq("feature/deleted-dep"))
	if err != nil {
		t.Fatal(err)
	}
	wtWrite(t, filepath.Join(created.Path, "node_modules", "pkg", "a.js"), "a\n")
	wtWrite(t, filepath.Join(created.Path, "node_modules", "pkg", "b.js"), "b\n")
	markDirForTest(t, created.Path, "node_modules")
	if err := os.Remove(filepath.Join(created.Path, "node_modules", "pkg", "b.js")); err != nil {
		t.Fatal(err)
	}

	res, err := evictWorktreeDeps(ctx, fx.evictReq("node_modules"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Kept) != 1 || res.Kept[0].Reason != evictReasonBaselineMismatch {
		t.Fatalf("res = %+v, want the deletion to be seen", res)
	}
	if len(res.Kept[0].Diff.Deleted) != 1 {
		t.Fatalf("diff = %+v, want one deletion", res.Kept[0].Diff)
	}
}

func TestEvictDepsRemovesAnUntouchedMarkedTree(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	created, err := ensureWorktree(ctx, fx.ensureReq("feature/evictable"))
	if err != nil {
		t.Fatal(err)
	}
	deps := filepath.Join(created.Path, "node_modules")
	wtWrite(t, filepath.Join(deps, "pkg", "index.js"), "dependency\n")
	// A junction into a sibling checkout, where one actually lives.
	sibling := filepath.Join(fx.Base, "sibling")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	wtWrite(t, filepath.Join(sibling, "keep.js"), "keep\n")
	if err := os.MkdirAll(filepath.Join(deps, "@aiexpedite"), 0o755); err != nil {
		t.Fatal(err)
	}
	makeAbsDirLink(t, filepath.Join(deps, "@aiexpedite", "shared"), sibling)
	markDirForTest(t, created.Path, "node_modules")

	// A dry run proves the verdict without deleting.
	dry := fx.evictReq("node_modules")
	dry.DryRun = true
	dryRes, err := evictWorktreeDeps(ctx, dry)
	if err != nil {
		t.Fatal(err)
	}
	if len(dryRes.Evictable) != 1 || len(dryRes.Evicted) != 0 || !dryRes.DepsReady {
		t.Fatalf("dry run = %+v, kept = %+v, want evictable and nothing deleted", dryRes, dryRes.Kept)
	}
	if _, serr := os.Stat(filepath.Join(deps, "pkg", "index.js")); serr != nil {
		t.Fatalf("a dry run deleted something: %v", serr)
	}

	res, err := evictWorktreeDeps(ctx, fx.evictReq("node_modules"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Evicted) != 1 || len(res.Kept) != 0 {
		t.Fatalf("res = %+v, want one eviction", res)
	}
	if res.DepsReady {
		t.Fatal("depsReady must go false so the next start rebuilds rather than trusting a half-tree")
	}
	if _, serr := os.Lstat(deps); !os.IsNotExist(serr) {
		t.Fatalf("node_modules survived: %v", serr)
	}
	// The worktree itself is intact and resumable: the path, the branch and
	// the tracked files are all still there.
	if _, serr := os.Stat(filepath.Join(created.Path, "README.md")); serr != nil {
		t.Fatalf("the worktree's tracked files went with the dependencies: %v", serr)
	}
	if branch := wtMustGit(t, created.Path, "symbolic-ref", "--short", "HEAD"); branch != "feature/evictable" {
		t.Fatalf("branch = %q", branch)
	}
	// And the sibling checkout survived the junction.
	if _, serr := os.Stat(filepath.Join(sibling, "keep.js")); serr != nil {
		t.Fatalf("the sibling checkout was emptied through the junction: %v", serr)
	}
	// The marker went with it, so a later pass cannot approve a rebuilt tree
	// against a stale baseline.
	if marker, _ := readDepsMarker(deps); marker != nil {
		t.Fatal("the provenance marker outlived the directory")
	}
}

func TestEvictDepsRefusesAPathOutsideTheWorktree(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	if _, err := ensureWorktree(ctx, fx.ensureReq("feature/evict-escape")); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{
		"climbing out": "../../../demo/node_modules",
		"absolute":     filepath.Join(fx.MainRepo, "node_modules"),
		"the worktree": ".",
		"a .. segment": "node_modules/..",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := evictWorktreeDeps(ctx, fx.evictReq(dir)); err == nil {
				t.Fatalf("admitted %q", dir)
			} else if worktreePolicyRuleOf(err) == "" {
				t.Fatalf("the refusal should name a policy rule, got: %v", err)
			}
		})
	}
}

func TestEvictDepsRefusesALinkedDependencyDirectory(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	created, err := ensureWorktree(ctx, fx.ensureReq("feature/linked-deps"))
	if err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(fx.Base, "shared-node-modules")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	wtWrite(t, filepath.Join(sibling, "keep.js"), "keep\n")
	makeAbsDirLink(t, filepath.Join(created.Path, "node_modules"), sibling)

	if _, err := evictWorktreeDeps(ctx, fx.evictReq("node_modules")); err == nil {
		t.Fatal("admitted a dependency directory that is itself a link")
	} else if got := worktreePolicyRuleOf(err); got != rulePathComponentIsLink {
		t.Fatalf("rule = %q, want %q (%v)", got, rulePathComponentIsLink, err)
	}
	if _, serr := os.Stat(filepath.Join(sibling, "keep.js")); serr != nil {
		t.Fatalf("the link target was touched: %v", serr)
	}
}

// TestRemoveDetachedCompanionRescuesItsOwnCommits covers the shape that has no
// branch and therefore no upstream.
//
// A companion is DETACHED, and a detached HEAD can hold commits an earlier run
// made while the working tree looks pristine — that is exactly the "diverged
// companion" ensure reports and leaves alone. A removal that only checked
// `@{u}..HEAD` would find no upstream, conclude nothing, and delete the only
// copy of those commits.
func TestRemoveDetachedCompanionRescuesItsOwnCommits(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()
	yes := true
	req := fx.ensureReq("feature/companion-removal")
	req.Fetch = &yes
	req.Companions = []worktreeCompanionSpec{{
		MainRepo: fx.MainRepo, Path: fx.wtPath("lib"), DefaultBranch: fx.Default, Remote: "origin",
	}}
	created, err := ensureWorktree(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	companion := created.Companions[0].Path
	if state, serr := wtReadState(ctx, companion); serr != nil || !state.Detached {
		t.Fatalf("premise wrong: the companion should be detached (%+v, %v)", state, serr)
	}
	wtWrite(t, filepath.Join(companion, "left-behind.txt"), "the only copy\n")
	wtMustGit(t, companion, "add", "-A")
	wtMustGit(t, companion, "commit", "-m", "work nobody pushed")
	sha := wtMustGit(t, companion, "rev-parse", "HEAD")

	// With rescue pushing off, the worktree is kept rather than deleted.
	off := false
	keepReq := fx.removeReq(fx.wtPath("lib"))
	keepReq.AllowRescuePush = &off
	kept, err := removeWorktree(ctx, keepReq)
	if err != nil {
		t.Fatalf("removeWorktree: %v", err)
	}
	if kept.Removed || kept.KeptReason != keptReasonUnpushedWork {
		t.Fatalf("res = %+v, want kept with %q", kept, keptReasonUnpushedWork)
	}
	if _, serr := os.Stat(filepath.Join(companion, "left-behind.txt")); serr != nil {
		t.Fatalf("the detached commit's content was deleted: %v", serr)
	}

	// With it on, the commit reaches the remote first.
	res, err := removeWorktree(ctx, fx.removeReq(fx.wtPath("lib")))
	if err != nil {
		t.Fatalf("removeWorktree: %v", err)
	}
	if !res.RescuePushed || !res.Removed {
		t.Fatalf("res = %+v, want rescue pushed then removed", res)
	}
	if rescued := wtMustGit(t, fx.Remote, "rev-parse", "refs/heads/"+res.RescueBranch); rescued != sha {
		t.Fatalf("rescue ref is at %s, want the companion's own commit %s", rescued, sha)
	}
}
