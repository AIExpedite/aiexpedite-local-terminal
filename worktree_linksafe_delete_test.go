package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLinkSafeRemoveAllNeverFollowsALink is the 2026-09-17 incident as a test.
//
// A dependency directory holds a junction into a sibling checkout
// (`node_modules/@aiexpedite/*`). A recursive delete that followed it emptied
// `local/node_modules` AND two entire sibling repositories, `.git` included,
// losing unpushed work. The property that prevents it: the link is UNLINKED and
// its target is untouched.
func TestLinkSafeRemoveAllNeverFollowsALink(t *testing.T) {
	base := tempRootDir(t)
	sibling := filepath.Join(base, "sibling-repo")
	deps := filepath.Join(base, "worktree", "node_modules", "@aiexpedite")
	for _, dir := range []string{sibling, deps} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Content inside the sibling that must survive, including its .git.
	if err := os.MkdirAll(filepath.Join(sibling, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	precious := filepath.Join(sibling, "src.js")
	if err := os.WriteFile(precious, []byte("unpushed work"), 0o644); err != nil {
		t.Fatal(err)
	}
	makeAbsDirLink(t, filepath.Join(deps, "shared-constants"), sibling)

	// Something ordinary to delete alongside it.
	if err := os.WriteFile(filepath.Join(deps, "index.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(base, "worktree", "node_modules")
	stats, err := linkSafeRemoveAll(target)
	if err != nil {
		t.Fatalf("link-safe delete failed: %v", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("%s should be gone, lstat err = %v", target, err)
	}
	if stats.Links != 1 {
		t.Fatalf("links unlinked = %d, want 1 (the junction into the sibling checkout)", stats.Links)
	}
	// The whole point.
	if _, err := os.Stat(precious); err != nil {
		t.Fatalf("the sibling checkout's content was deleted through the link: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sibling, ".git")); err != nil {
		t.Fatalf("the sibling checkout's .git was deleted through the link: %v", err)
	}
}

// TestLinkSafeRemoveAllUnlinksALinkedTargetItself covers the entry point being
// a link: it is unlinked, never descended.
func TestLinkSafeRemoveAllUnlinksALinkedTargetItself(t *testing.T) {
	base := tempRootDir(t)
	sibling := filepath.Join(base, "sibling")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(sibling, "keep.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	makeAbsDirLink(t, link, sibling)

	stats, err := linkSafeRemoveAll(link)
	if err != nil {
		t.Fatalf("link-safe delete failed: %v", err)
	}
	if stats.Links != 1 || stats.Dirs != 0 {
		t.Fatalf("stats = %+v, want exactly one unlinked link and no directory walk", stats)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("the link target's content was deleted: %v", err)
	}
}

func TestLinkSafeRemoveAllDeletesAnOrdinaryTree(t *testing.T) {
	base := tempRootDir(t)
	tree := filepath.Join(base, "node_modules", "pkg", "dist")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "a.js"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A read-only file, which npm and pip both leave behind and which refuses
	// to unlink on Windows until the attribute goes.
	readOnly := filepath.Join(tree, "ro.js")
	if err := os.WriteFile(readOnly, []byte("ro"), 0o444); err != nil {
		t.Fatal(err)
	}
	stats, err := linkSafeRemoveAll(filepath.Join(base, "node_modules"))
	if err != nil {
		t.Fatalf("link-safe delete failed: %v", err)
	}
	if stats.Files != 2 {
		t.Fatalf("files = %d, want 2", stats.Files)
	}
	if stats.Dirs != 3 {
		t.Fatalf("dirs = %d, want 3", stats.Dirs)
	}
	if stats.Bytes != int64(len("hello")+len("ro")) {
		t.Fatalf("bytes = %d, want %d", stats.Bytes, len("hello")+len("ro"))
	}
}

func TestLinkSafeRemoveAllOnAnAbsentPathIsSuccess(t *testing.T) {
	stats, err := linkSafeRemoveAll(filepath.Join(tempRootDir(t), "never-existed"))
	if err != nil {
		t.Fatalf("an absent path should be success so a retry is safe: %v", err)
	}
	if stats.Files+stats.Dirs+stats.Links != 0 {
		t.Fatalf("stats = %+v, want zero", stats)
	}
}

func TestCountReparsePointsSeesALinkWithoutFollowingIt(t *testing.T) {
	base := tempRootDir(t)
	sibling := filepath.Join(base, "sibling")
	inner := filepath.Join(base, "deps", "nested")
	for _, dir := range []string{sibling, inner} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Three levels of content inside the target, to prove the count is not
	// coming from a walk through the link.
	if err := os.MkdirAll(filepath.Join(sibling, "a", "b", "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	makeAbsDirLink(t, filepath.Join(inner, "link"), sibling)
	n, err := countReparsePoints(filepath.Join(base, "deps"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reparse points = %d, want 1", n)
	}
}

// TestScanRelocatableRefusesAnEscapingLink pins §4.3.1's frame of reference:
// the scan is scoped to THE DIRECTORY BEING CLONED, not the worktree. The
// example in the plan — node_modules/@scope/plugin -> <repo>/eslint-rules —
// resolves INSIDE the repository, so a worktree-scoped test would pass it and
// the clone would still load that package from the root.
func TestScanRelocatableRefusesAnEscapingLink(t *testing.T) {
	base := tempRootDir(t)
	repo := filepath.Join(base, "frontend")
	deps := filepath.Join(repo, "node_modules", "@aiexpedite")
	rules := filepath.Join(repo, "eslint-rules")
	for _, dir := range []string{deps, rules} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	makeAbsDirLink(t, filepath.Join(deps, "eslint-plugin-architecture"), rules)

	scan, err := scanRelocatable(filepath.Join(repo, "node_modules"))
	if err != nil {
		t.Fatal(err)
	}
	if scan.Relocatable {
		t.Fatal("a link out of node_modules into the repository must make the tree non-relocatable")
	}
	if scan.Links != 1 || len(scan.Escaping) != 1 {
		t.Fatalf("scan = %+v, want one link, reported as escaping", scan)
	}
}

func TestScanRelocatableAcceptsASelfContainedTree(t *testing.T) {
	base := tempRootDir(t)
	deps := filepath.Join(base, "node_modules", "pkg")
	if err := os.MkdirAll(deps, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deps, "index.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A link that stays inside the directory being cloned is fine.
	makeAbsDirLink(t, filepath.Join(base, "node_modules", "alias"), deps)
	scan, err := scanRelocatable(filepath.Join(base, "node_modules"))
	if err != nil {
		t.Fatal(err)
	}
	if !scan.Relocatable {
		t.Fatalf("scan = %+v, want relocatable", scan)
	}
}
