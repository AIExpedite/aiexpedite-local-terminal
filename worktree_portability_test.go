// File: worktree_portability_test.go
// -----------------------------------------------------------------------------
// The two rules that make a worktree usable on someone else's codebase
// (WORKTREE_CONFIG_AND_PORTABILITY.md §8 items 2 and 4):
//
//	2  a worktree parent is never inside a repository
//	4  a new worktree's submodules are checked out
//
// Our own layout meets neither case: the Default working directory sits above
// the root checkouts, and no repository here has a submodule. Both are ordinary
// for other people, which is the whole point of the item.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ─── item 2: the worktree parent is never inside a repository ───────────────

func TestEnclosingRepositoryOfReadsBothGitShapes(t *testing.T) {
	base := tempRootDir(t)

	// A main checkout: `.git` is a DIRECTORY.
	mainRepo := filepath.Join(base, "main")
	if err := os.MkdirAll(filepath.Join(mainRepo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A LINKED worktree: `.git` is a FILE holding a gitdir pointer. A root
	// inside one is inside a repository just as surely.
	linked := filepath.Join(base, "linked")
	if err := os.MkdirAll(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	pointer := "gitdir: " + filepath.Join(mainRepo, ".git", "worktrees", "linked") + "\n"
	if err := os.WriteFile(filepath.Join(linked, ".git"), []byte(pointer), 0o644); err != nil {
		t.Fatal(err)
	}
	// Plain directories, with no repository anywhere above them inside base.
	plain := filepath.Join(base, "plain", "nested")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		dir  string
		want string
	}{
		{"the repository itself", mainRepo, mainRepo},
		{"a directory inside it", filepath.Join(mainRepo, "src", "deep"), mainRepo},
		{"a linked worktree's gitfile", linked, linked},
		{"a plain tree", plain, ""},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := enclosingRepositoryOf(tc.dir); got != tc.want {
				t.Fatalf("enclosingRepositoryOf(%q) = %q, want %q", tc.dir, got, tc.want)
			}
		})
	}
}

// THE SINGLE-REPOSITORY CASE, which is most other people's case: the Default
// working directory IS the repository, so `<root>/aixwt/...` lands inside that
// repository's working tree. `git clean -fdx` there deletes every worktree we
// made, and the owner's `git status` reports our scaffolding as their own
// uncommitted work.
func TestEnsureRefusesAWorktreeParentInsideARepository(t *testing.T) {
	fx := newWorktreeFixture(t)

	req := fx.ensureReq("feature/single-repo")
	req.Root = fx.MainRepo // the developer with one repository

	_, err := ensureWorktree(context.Background(), req)
	if err == nil {
		t.Fatal("ensureWorktree accepted a root inside a git checkout")
	}
	if rule := worktreePolicyRuleOf(err); rule != ruleParentInsideRepository {
		t.Fatalf("rule = %q, want %q (err: %v)", rule, ruleParentInsideRepository, err)
	}
	// The remedy is the owner's setting, so the message has to name it.
	if !strings.Contains(err.Error(), "Default working directory") {
		t.Fatalf("the refusal does not say what to change: %v", err)
	}

	// NOTHING WAS CREATED, and the repository is exactly as it was. This is the
	// half that matters: a refusal that had already made the directory would
	// have left behind the mess it was refusing to make.
	if _, serr := os.Stat(filepath.Join(fx.MainRepo, worktreeRootSegment)); serr == nil {
		t.Fatalf("%s was created under the repository anyway", worktreeRootSegment)
	}
	if status := wtMustGit(t, fx.MainRepo, "status", "--porcelain"); status != "" {
		t.Fatalf("the root checkout is no longer clean:\n%s", status)
	}
}

// An EXISTING worktree under such a root is reported, never retro-refused.
//
// Refusing it would strand the directory: the run would fall back to the shared
// root checkout — the very sharing worktrees exist to stop — and the sweep
// still has to be able to remove it.
func TestEnsureReportsRatherThanRefusesAnExistingWorktreeUnderARepositoryRoot(t *testing.T) {
	fx := newWorktreeFixture(t)

	req := fx.ensureReq("feature/already-there")
	req.Root = fx.MainRepo
	abs := filepath.Join(fx.MainRepo, req.Path)

	// Built the way an earlier build of this agent would have built it, before
	// the rule existed.
	wtMustGit(t, fx.MainRepo, "worktree", "add", "-b", "feature/already-there", abs, "HEAD")

	res, err := ensureWorktree(context.Background(), req)
	if err != nil {
		t.Fatalf("ensureWorktree refused a worktree that already exists: %v", err)
	}
	if res.Created {
		t.Fatalf("res.Created = true, want the existing worktree reused: %+v", res)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), ruleParentInsideRepository) {
		t.Fatalf("notes do not name the rule: %q", res.Notes)
	}
}

// ─── item 4: a new worktree's submodules are checked out ────────────────────

// addSubmodule gives the fixture's repository a submodule at `sub`, through the
// remote so the root checkout picks it up on a fetch. `url` is written into
// `.gitmodules` verbatim, so a test can supply a hostile one.
//
// The gitlink is written with update-index rather than `git submodule add`,
// because `add` would try to CLONE the url — which is the thing under test.
func addSubmodule(t *testing.T, fx wtFixture, url, gitlink string) {
	t.Helper()
	seed := filepath.Join(fx.Base, "seed")
	if gitlink == "" {
		gitlink = wtMustGit(t, seed, "rev-parse", "HEAD")
	}
	body := "[submodule \"sub\"]\n\tpath = sub\n\turl = " + url + "\n"
	wtWrite(t, filepath.Join(seed, ".gitmodules"), body)
	wtMustGit(t, seed, "add", ".gitmodules")
	wtMustGit(t, seed, "update-index", "--add", "--cacheinfo", "160000,"+gitlink+",sub")
	wtMustGit(t, seed, "commit", "-m", "add submodule")
	wtMustGit(t, seed, "push", "origin", "main")
	wtMustGit(t, fx.MainRepo, "fetch", "origin", "main")
	wtMustGit(t, fx.MainRepo, "reset", "--hard", "origin/main")
}

// A submodule fixture is POPULATED in the worktree. Without this, a repository
// that keeps a dependency or its own tooling in a submodule cannot build in a
// worktree at all, and the failure looks like a missing file.
//
// The submodule's url is a local path, which git refuses to clone unless the
// owner has allowed the `file` transport — and MEASURED HERE: git ignores a
// REPOSITORY-level `protocol.file.allow`, honouring only the command line, the
// environment and the global config. That is deliberate upstream, and it is the
// same boundary §6 draws: a repository must not be able to re-enable a
// transport for its own submodules. So the fixture sets it GLOBALLY, where only
// a person can, inside the sandboxed HOME this test binary already runs under.
func TestEnsureInitialisesSubmodulesInANewWorktree(t *testing.T) {
	fx := newWorktreeFixture(t)

	// The submodule's own origin, with one file to find.
	subRemote := filepath.Join(fx.Base, "sub.git")
	wtMustGit(t, fx.Base, "init", "--bare", "--initial-branch=main", subRemote)
	subSeed := filepath.Join(fx.Base, "subseed")
	wtMustGit(t, fx.Base, "init", "--initial-branch=main", subSeed)
	wtConfigRepo(t, subSeed)
	wtWrite(t, filepath.Join(subSeed, "MODULE.txt"), "from the submodule\n")
	wtMustGit(t, subSeed, "add", "-A")
	wtMustGit(t, subSeed, "commit", "-m", "submodule initial")
	wtMustGit(t, subSeed, "remote", "add", "origin", subRemote)
	wtMustGit(t, subSeed, "push", "-u", "origin", "main")
	subHead := wtMustGit(t, subSeed, "rev-parse", "HEAD")

	addSubmodule(t, fx, filepath.ToSlash(subRemote), subHead)
	// The owner's own setting. HOME is redirected for the whole test binary
	// (test_environment_test.go), so this never touches a real global config.
	wtMustGit(t, fx.Base, "config", "--global", "protocol.file.allow", "always")
	t.Cleanup(func() {
		_, _ = wtGit(context.Background(), fx.Base, "config", "--global", "--unset", "protocol.file.allow")
	})

	res, err := ensureWorktree(context.Background(), fx.ensureReq("feature/with-submodule"))
	if err != nil {
		t.Fatalf("ensureWorktree: %v", err)
	}
	if res.Submodules == nil {
		t.Fatal("res.Submodules is nil for a repository that declares one")
	}
	if !res.Submodules.Present || !res.Submodules.Initialised || res.Submodules.Count != 1 {
		t.Fatalf("res.Submodules = %+v, want present, initialised, count 1", res.Submodules)
	}
	// The real assertion: the submodule's content is on disk in the WORKTREE,
	// which is what "it builds" reduces to.
	if _, serr := os.Stat(filepath.Join(res.Path, "sub", "MODULE.txt")); serr != nil {
		t.Fatalf("the submodule is not populated in the worktree: %v", serr)
	}
}

// A `.gitmodules` url never reaches a transport that EXECUTES A COMMAND, even
// when the owner has allowed that transport globally.
//
// `ext::<command>` makes the command the transport. A pull request editing a
// tracked file would therefore get code execution on the owner's computer out
// of an unattended prepare — the same class of hole as the git hooks one, by a
// different route.
//
// WHICH CONFIG DOES THE PROTECTING MATTERS, and the first two versions of this
// test got it wrong and SKIPPED, proving nothing. Measured here:
//
//   - a REPOSITORY-level `protocol.ext.allow = always` is ignored for a
//     submodule clone, so a repository cannot re-enable the transport by
//     itself and our flag would be redundant against that;
//   - the owner's own GLOBAL `always` IS honoured — a reasonable setting for
//     someone who uses an `ext::` helper for their own remotes — and it would
//     then hand a tracked `.gitmodules` an arbitrary command.
//
// That second case is what the `-c` denial is for, and the command line is
// where it has to be, because that is the one place that outranks a global
// config.
//
// The assertion is at the TRANSPORT, not on a sentinel file: a transport git
// refused never started, so nothing could have run. That also keeps the test
// off platform-specific shell quoting, which is what made the sentinel version
// skip on Windows while passing nowhere.
func TestEnsureRefusesAnExecutingSubmoduleTransport(t *testing.T) {
	const refused = "transport 'ext' not allowed"

	fx := newWorktreeFixture(t)
	// The payload is immaterial — the transport is refused before it is read.
	addSubmodule(t, fx, "ext::some-helper --payload", "")

	// The owner allows the transport for their own remotes. HOME is redirected
	// for the whole test binary (test_environment_test.go), so no real global
	// config is touched.
	wtMustGit(t, fx.Base, "config", "--global", "protocol.ext.allow", "always")
	t.Cleanup(func() {
		_, _ = wtGit(context.Background(), fx.Base, "config", "--global", "--unset", "protocol.ext.allow")
	})

	// THE CONTROL, raw so it carries none of our overrides. If this platform's
	// git refuses `ext` anyway, our flag is doing nothing here and the test
	// below would pass against nothing.
	controlPath := filepath.Join(fx.Base, "control")
	wtMustGit(t, fx.MainRepo, "worktree", "add", "--detach", controlPath, "HEAD")
	control := exec.Command("git", "-C", controlPath, "submodule", "update", "--init")
	control.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, _ := control.CombinedOutput()
	if strings.Contains(string(out), refused) {
		t.Skipf("this platform's git refuses ext even with the owner's global allowance: %s", out)
	}

	res, err := ensureWorktree(context.Background(), fx.ensureReq("feature/hostile-submodule"))
	if err != nil {
		t.Fatalf("ensureWorktree: %v", err)
	}
	if res.Submodules == nil {
		t.Fatal("res.Submodules is nil for a repository that declares one")
	}
	if !strings.Contains(res.Submodules.Error, refused) {
		t.Fatalf("the ext transport was not denied; res.Submodules = %+v", res.Submodules)
	}
	// The worktree is still a worktree, and the failure is REPORTED rather than
	// fatal: a run can still work in it, and the brief can say what is missing.
	if !res.Created {
		t.Fatalf("res = %+v, want the worktree created despite the submodule", res)
	}
	if res.Submodules.Initialised || res.Submodules.Reason == "" {
		t.Fatalf("res.Submodules = %+v, want not initialised, with a reason", res.Submodules)
	}
}

func TestEnsureSkipsSubmodulesWhenTheRequestSaysSo(t *testing.T) {
	fx := newWorktreeFixture(t)
	addSubmodule(t, fx, "https://example.invalid/never-fetched.git", "")

	req := fx.ensureReq("feature/no-submodules")
	off := false
	req.Submodules = &off

	res, err := ensureWorktree(context.Background(), req)
	if err != nil {
		t.Fatalf("ensureWorktree: %v", err)
	}
	if res.Submodules == nil || res.Submodules.Reason != "disabled" {
		t.Fatalf("res.Submodules = %+v, want reason=disabled", res.Submodules)
	}
	if entries, _ := os.ReadDir(filepath.Join(res.Path, "sub")); len(entries) != 0 {
		t.Fatalf("the submodule was populated anyway: %d entries", len(entries))
	}
}

// A repository WITHOUT submodules says nothing at all, rather than reporting a
// row of zeroes the server would then have to interpret.
func TestEnsureReportsNoSubmodulesWhenTheRepositoryDeclaresNone(t *testing.T) {
	fx := newWorktreeFixture(t)
	res, err := ensureWorktree(context.Background(), fx.ensureReq("feature/plain"))
	if err != nil {
		t.Fatalf("ensureWorktree: %v", err)
	}
	if res.Submodules != nil {
		t.Fatalf("res.Submodules = %+v, want nil", res.Submodules)
	}
}

// `submodule.<name>.update = !<command>` in a TRACKED `.gitmodules` is the
// other way a submodule declaration reaches for code execution.
//
// git itself refuses the `!` form from `.gitmodules` — but measured, it refuses
// it by failing the WHOLE update with `fatal: invalid value for
// 'submodule.sub.update'`, so a repository could stop every worktree of itself
// from initialising any submodule at all. Pinning the mode with `--checkout`
// means the value is never consulted: the submodule is checked out normally.
func TestEnsureIgnoresASubmoduleUpdateCommand(t *testing.T) {
	fx := newWorktreeFixture(t)

	subRemote := filepath.Join(fx.Base, "sub.git")
	wtMustGit(t, fx.Base, "init", "--bare", "--initial-branch=main", subRemote)
	subSeed := filepath.Join(fx.Base, "subseed")
	wtMustGit(t, fx.Base, "init", "--initial-branch=main", subSeed)
	wtConfigRepo(t, subSeed)
	wtWrite(t, filepath.Join(subSeed, "MODULE.txt"), "from the submodule\n")
	wtMustGit(t, subSeed, "add", "-A")
	wtMustGit(t, subSeed, "commit", "-m", "submodule initial")
	wtMustGit(t, subSeed, "push", subRemote, "main:main")
	subHead := wtMustGit(t, subSeed, "rev-parse", "HEAD")

	// `.gitmodules` with BOTH a url and a hostile update mode.
	seed := filepath.Join(fx.Base, "seed")
	body := "[submodule \"sub\"]\n\tpath = sub\n\turl = " + filepath.ToSlash(subRemote) +
		"\n\tupdate = !touch " + filepath.ToSlash(filepath.Join(fx.Base, "UPDATE_RAN.txt")) + "\n"
	wtWrite(t, filepath.Join(seed, ".gitmodules"), body)
	wtMustGit(t, seed, "add", ".gitmodules")
	wtMustGit(t, seed, "update-index", "--add", "--cacheinfo", "160000,"+subHead+",sub")
	wtMustGit(t, seed, "commit", "-m", "add submodule with an update command")
	wtMustGit(t, seed, "push", "origin", "main")
	wtMustGit(t, fx.MainRepo, "fetch", "origin", "main")
	wtMustGit(t, fx.MainRepo, "reset", "--hard", "origin/main")
	wtMustGit(t, fx.Base, "config", "--global", "protocol.file.allow", "always")
	t.Cleanup(func() {
		_, _ = wtGit(context.Background(), fx.Base, "config", "--global", "--unset", "protocol.file.allow")
	})

	res, err := ensureWorktree(context.Background(), fx.ensureReq("feature/update-command"))
	if err != nil {
		t.Fatalf("ensureWorktree: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(fx.Base, "UPDATE_RAN.txt")); serr == nil {
		t.Fatal("submodule.<name>.update ran a command during prepare")
	}
	if res.Submodules == nil || !res.Submodules.Initialised {
		t.Fatalf("res.Submodules = %+v, want initialised despite the update mode", res.Submodules)
	}
	if _, serr := os.Stat(filepath.Join(res.Path, "sub", "MODULE.txt")); serr != nil {
		t.Fatalf("the submodule is not populated: %v", serr)
	}
}
