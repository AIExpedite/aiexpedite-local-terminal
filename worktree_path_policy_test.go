package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// wtTestFeatureFolder is a valid `<slug>-<key>`: 12 lowercase hex characters
// after the kebab slug.
const wtTestFeatureFolder = "demo-feature-aabbccddeeff"

// makeAbsDirLink links `link` to the ABSOLUTE directory `target`: a junction on
// Windows (which is how npm links workspaces there, and what the 2026-09-17
// incident followed), a symlink elsewhere. It differs from makeDirLink in
// env_verify_deps_test.go only in taking an absolute target, which is what a
// cross-checkout junction actually has.
//
// The link is removed NON-RECURSIVELY in cleanup, before t.TempDir's own
// recursive cleanup runs. That is load-bearing rather than tidiness: since Go
// 1.23 a Windows junction is not reported as ModeSymlink, so os.RemoveAll would
// descend it and empty the link's target — the very incident these tests cover.
func makeAbsDirLink(t *testing.T, link, target string) {
	t.Helper()
	t.Cleanup(func() { _ = os.Remove(link) })
	if runtime.GOOS == "windows" {
		out, jerr := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
		if jerr == nil {
			return
		}
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("neither a junction (%v: %s) nor a symlink (%v) could be created", jerr, out, err)
		}
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symlink on this host: %v", err)
	}
}

// tempRootDir returns a resolved temp directory, so containment comparisons
// compare like with like (macOS hands out /var -> /private/var).
func tempRootDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return dir
}

// TestWorktreeRelativePathDiagnosticsMatchPattern is the drift guard between
// the two halves of the policy: the per-rule diagnostics exist only to NAME a
// failure, so their verdict must be identical to the shared pattern's over a
// corpus. If they ever disagree, one of them has been changed alone.
func TestWorktreeRelativePathDiagnosticsMatchPattern(t *testing.T) {
	corpus := []string{
		// Accepted.
		"aixwt/" + wtTestFeatureFolder + "/frontend",
		"aixwt/a-aabbccddeeff/a",
		"aixwt/a-aabbccddeeff/ab",
		"aixwt/a-aabbccddeeff/a-",
		"aixwt/a-aabbccddeeff/a_",
		"aixwt/a-aabbccddeeff/my.repo-name_1",
		"aixwt/a-aabbccddeeff/a..b",
		"aixwt/a-aabbccddeeff/" + strings.Repeat("a", 64),
		"aixwt/" + strings.Repeat("a", 24) + "-aabbccddeeff/repo",
		"aixwt/abc-aabbccddeeff-aabbccddeeff/repo",
		// The traversal family — the whole reason the segment is not a plain
		// character class.
		"aixwt/" + wtTestFeatureFolder + "/..",
		"aixwt/" + wtTestFeatureFolder + "/.",
		"aixwt/" + wtTestFeatureFolder + "/...",
		// Windows strips a trailing dot, so these two name ONE directory.
		"aixwt/" + wtTestFeatureFolder + "/repo.",
		"aixwt/" + wtTestFeatureFolder + "/a.",
		// Must start alphanumeric.
		"aixwt/" + wtTestFeatureFolder + "/.hidden",
		"aixwt/" + wtTestFeatureFolder + "/-lead",
		"aixwt/" + wtTestFeatureFolder + "/_lead",
		// Length and charset.
		"aixwt/" + wtTestFeatureFolder + "/" + strings.Repeat("a", 65),
		"aixwt/" + wtTestFeatureFolder + "/rep o",
		"aixwt/" + wtTestFeatureFolder + "/repö",
		"aixwt/" + wtTestFeatureFolder + "/",
		// Feature folder.
		"aixwt/DEMO-aabbccddeeff/repo",
		"aixwt/demo-AABBCCDDEEFF/repo",
		"aixwt/demo-aabbccddeef/repo",
		"aixwt/demo-aabbccddeeff0/repo",
		"aixwt/demo/repo",
		"aixwt/" + strings.Repeat("a", 25) + "-aabbccddeeff/repo",
		// Shape.
		"",
		"aixwt",
		"aixwt/" + wtTestFeatureFolder,
		"aixwt/" + wtTestFeatureFolder + "/repo/src",
		"aixwtx/" + wtTestFeatureFolder + "/repo",
		"AIXWT/" + wtTestFeatureFolder + "/repo",
		"other/" + wtTestFeatureFolder + "/repo",
		"../aixwt/" + wtTestFeatureFolder + "/repo",
	}
	for _, rel := range corpus {
		diag := diagnoseWorktreeRelativePath(rel)
		matched := worktreeRelativePathPattern.MatchString(rel)
		if (diag == nil) != matched {
			t.Errorf("drift for %q: diagnostics say ok=%v, WORKTREE_RELATIVE_PATH_PATTERN says ok=%v",
				rel, diag == nil, matched)
		}
	}
}

// TestWorktreePatternHasNoLookahead pins the deliberate constraint: the desktop
// app checks the same pattern in Go, whose RE2 engine has no lookahead, so a
// lookahead on either side would mean two different rules.
func TestWorktreePatternHasNoLookahead(t *testing.T) {
	src := worktreeRelativePathPattern.String()
	for _, forbidden := range []string{"(?=", "(?!", "(?<=", "(?<!"} {
		if strings.Contains(src, forbidden) {
			t.Fatalf("pattern uses %q, which RE2 has no equivalent for: %s", forbidden, src)
		}
	}
	// And keep it literally identical to shared-constants
	// WORKTREE_RELATIVE_PATH_PATTERN. Changing this literal is a cross-repo
	// contract change, not a local edit.
	const shared = `^aixwt/[a-z0-9-]{1,24}-[0-9a-f]{12}/[A-Za-z0-9]([A-Za-z0-9._-]{0,62}[A-Za-z0-9_-])?$`
	if src != shared {
		t.Fatalf("pattern drifted from shared-constants:\n got %s\nwant %s", src, shared)
	}
}

func TestResolveWorktreePathAcceptsAValidPath(t *testing.T) {
	root := tempRootDir(t)
	wt, err := resolveWorktreePath(root, filepath.Join("aixwt", wtTestFeatureFolder, "frontend"))
	if err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
	if wt.Rel != "aixwt/"+wtTestFeatureFolder+"/frontend" {
		t.Fatalf("rel = %q", wt.Rel)
	}
	if wt.FeatureFolder != wtTestFeatureFolder || wt.RepoFolder != "frontend" {
		t.Fatalf("segments = %q / %q", wt.FeatureFolder, wt.RepoFolder)
	}
	if wt.Exists {
		t.Fatal("Exists should be false for a path that is not there yet")
	}
}

// TestResolveWorktreePathRefusals is the refusal table. Every row is a path a
// dispatch could carry, and the test asserts WHICH rule refused — because a
// refusal has to name its rule, not just fail.
func TestResolveWorktreePathRefusals(t *testing.T) {
	root := tempRootDir(t)
	other := tempRootDir(t)
	cases := []struct {
		name string
		root string
		path string
		rule string
	}{
		{"no root", "", "aixwt/" + wtTestFeatureFolder + "/repo", rulePathRootMissing},
		{"relative root", "relative", "aixwt/" + wtTestFeatureFolder + "/repo", rulePathRootNotAbsolute},
		{"no path", root, "", rulePathMissing},
		{"absolute path outside the root", root, filepath.Join(other, "aixwt", wtTestFeatureFolder, "repo"), rulePathNotUnderRoot},
		{"climbing out with ..", root, "aixwt/" + wtTestFeatureFolder + "/../../../etc", rulePathNotUnderRoot},
		{"bare .. as the repository segment", root, "aixwt/" + wtTestFeatureFolder + "/..", rulePathSegmentCount},
		{"bare . as the repository segment", root, "aixwt/" + wtTestFeatureFolder + "/.", rulePathSegmentCount},
		{"... as the repository segment", root, "aixwt/" + wtTestFeatureFolder + "/...", rulePathRepoTraversal},
		{"trailing dot", root, "aixwt/" + wtTestFeatureFolder + "/repo.", rulePathRepoTrailingDot},
		{"leading dot", root, "aixwt/" + wtTestFeatureFolder + "/.hidden", rulePathRepoLeadingChar},
		{"leading hyphen", root, "aixwt/" + wtTestFeatureFolder + "/-lead", rulePathRepoLeadingChar},
		{"65-character repository segment", root, "aixwt/" + wtTestFeatureFolder + "/" + strings.Repeat("a", 65), rulePathRepoTooLong},
		{"space in the repository segment", root, "aixwt/" + wtTestFeatureFolder + "/rep o", rulePathRepoCharset},
		{"wrong first segment", root, "elsewhere/" + wtTestFeatureFolder + "/repo", rulePathRootSegment},
		{"feature folder without a hex key", root, "aixwt/demo/repo", rulePathFeatureFolder},
		{"feature folder with an uppercase key", root, "aixwt/demo-AABBCCDDEEFF/repo", rulePathFeatureFolder},
		{"nested below the repository", root, "aixwt/" + wtTestFeatureFolder + "/repo/src", rulePathSegmentCount},
		{"the shared aixwt directory itself", root, "aixwt", rulePathSegmentCount},
		{"the feature folder itself", root, "aixwt/" + wtTestFeatureFolder, rulePathSegmentCount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveWorktreePath(tc.root, tc.path)
			if err == nil {
				t.Fatalf("expected a refusal for %q", tc.path)
			}
			if got := worktreePolicyRuleOf(err); got != tc.rule {
				t.Fatalf("rule = %q, want %q (%v)", got, tc.rule, err)
			}
		})
	}
}

// TestBareDotDotSegmentWouldNameTheSharedAixwtDirectory is the finding this
// rule came from, written as the consequence rather than as a syntax check: a
// plain character class admits `..`, and `aixwt/<folder>/..` then names the
// SHARED aixwt directory — one registry row aimed at every worktree on the
// computer.
func TestBareDotDotSegmentWouldNameTheSharedAixwtDirectory(t *testing.T) {
	root := tempRootDir(t)
	rel := "aixwt/" + wtTestFeatureFolder + "/.."
	// What the path would have resolved to had it been admitted:
	wouldBe := filepath.Clean(filepath.Join(root, rel))
	if wouldBe != filepath.Join(root, "aixwt") {
		t.Fatalf("premise wrong: %q resolves to %q", rel, wouldBe)
	}
	if _, err := resolveWorktreePath(root, rel); err == nil {
		t.Fatalf("admitted %q, which names the shared %s directory", rel, worktreeRootSegment)
	}
	// And the same for `.`, which names the feature folder.
	if _, err := resolveWorktreePath(root, "aixwt/"+wtTestFeatureFolder+"/."); err == nil {
		t.Fatal("admitted a bare `.` repository segment, which names the feature folder")
	}
}

// TestResolveWorktreePathRefusesALinkedComponent covers what the pattern cannot
// see: a lexically perfect path whose feature folder is a junction into a
// sibling checkout.
func TestResolveWorktreePathRefusesALinkedComponent(t *testing.T) {
	root := tempRootDir(t)
	sibling := tempRootDir(t)
	if err := os.MkdirAll(filepath.Join(root, "aixwt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sibling, "frontend"), 0o755); err != nil {
		t.Fatal(err)
	}
	makeAbsDirLink(t, filepath.Join(root, "aixwt", wtTestFeatureFolder), sibling)

	_, err := resolveWorktreePath(root, "aixwt/"+wtTestFeatureFolder+"/frontend")
	if err == nil {
		t.Fatal("admitted a path whose feature folder is a link into a sibling checkout")
	}
	if rule := worktreePolicyRuleOf(err); rule != rulePathComponentIsLink && rule != rulePathEscapesRootByLink {
		t.Fatalf("rule = %q, want component_is_link or escapes_root_by_link (%v)", rule, err)
	}
}

func TestResolveWorktreePathRefusesALinkedLeaf(t *testing.T) {
	root := tempRootDir(t)
	sibling := tempRootDir(t)
	if err := os.MkdirAll(filepath.Join(root, "aixwt", wtTestFeatureFolder), 0o755); err != nil {
		t.Fatal(err)
	}
	makeAbsDirLink(t, filepath.Join(root, "aixwt", wtTestFeatureFolder, "frontend"), sibling)
	_, err := resolveWorktreePath(root, "aixwt/"+wtTestFeatureFolder+"/frontend")
	if err == nil {
		t.Fatal("admitted a worktree path that is itself a link")
	}
	if rule := worktreePolicyRuleOf(err); rule != rulePathComponentIsLink {
		t.Fatalf("rule = %q, want %q (%v)", rule, rulePathComponentIsLink, err)
	}
}

func TestResolveInsideWorktreeRefusals(t *testing.T) {
	root := tempRootDir(t)
	wt, err := resolveWorktreePath(root, "aixwt/"+wtTestFeatureFolder+"/frontend")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wt.Abs, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		rel  string
		rule string
	}{
		{"empty", "", rulePathMissing},
		{"absolute", filepath.Join(root, "node_modules"), rulePathNotUnderRoot},
		{"climbing out", "../../../node_modules", rulePathRepoTraversal},
		{"a bare .. segment", "packages/../..", rulePathRepoTraversal},
		{"a bare . segment", "./node_modules", rulePathRepoTraversal},
		{"the worktree itself", ".", rulePathRepoTraversal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := resolveInsideWorktree(wt, tc.rel); err == nil {
				t.Fatalf("admitted %q", tc.rel)
			} else if got := worktreePolicyRuleOf(err); got != tc.rule {
				t.Fatalf("rule = %q, want %q (%v)", got, tc.rule, err)
			}
		})
	}
	if _, err := resolveInsideWorktree(wt, "packages/web/node_modules"); err != nil {
		t.Fatalf("a nested dependency directory should be accepted: %v", err)
	}
}

func TestResolveInsideWorktreeRefusesALink(t *testing.T) {
	root := tempRootDir(t)
	sibling := tempRootDir(t)
	wt, err := resolveWorktreePath(root, "aixwt/"+wtTestFeatureFolder+"/frontend")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wt.Abs, 0o755); err != nil {
		t.Fatal(err)
	}
	makeAbsDirLink(t, filepath.Join(wt.Abs, "node_modules"), sibling)
	if _, err := resolveInsideWorktree(wt, "node_modules"); err == nil {
		t.Fatal("admitted a dependency directory that is a link into a sibling checkout")
	} else if got := worktreePolicyRuleOf(err); got != rulePathComponentIsLink {
		t.Fatalf("rule = %q, want %q (%v)", got, rulePathComponentIsLink, err)
	}
}

func TestResolveMainRepoPathRefusals(t *testing.T) {
	root := tempRootDir(t)
	plain := filepath.Join(root, "notarepo")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "aixwt", wtTestFeatureFolder, "frontend")
	if err := os.MkdirAll(filepath.Join(inside, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		path string
		rule string
	}{
		{"empty", "", ruleMainRepoMissing},
		{"relative", "repo", ruleMainRepoNotAbsolute},
		{"not a checkout", plain, ruleMainRepoNotCheckout},
		{"a worktree, not a root checkout", inside, ruleMainRepoInsideAixwt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := resolveMainRepoPath(tc.path); err == nil {
				t.Fatalf("admitted %q", tc.path)
			} else if got := worktreePolicyRuleOf(err); got != tc.rule {
				t.Fatalf("rule = %q, want %q (%v)", got, tc.rule, err)
			}
		})
	}
}

func TestValidateRescueRefRefusals(t *testing.T) {
	for _, ref := range []string{
		"",
		"feature/x",
		"aix/rescue/",
		"aix/rescue/../../etc",
		"aix/rescue/x.lock",
		"aix/rescue//x",
		"aix/rescue/x y",
		"--upload-pack=evil",
	} {
		if _, err := validateRescueRef(ref); err == nil {
			t.Errorf("admitted rescue ref %q", ref)
		}
	}
	if got, err := validateRescueRef("aix/rescue/" + wtTestFeatureFolder + "/frontend"); err != nil || got == "" {
		t.Fatalf("rejected a valid rescue ref: %v", err)
	}
}
