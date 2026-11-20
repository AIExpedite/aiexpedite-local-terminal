// File: relay_path_containment.go
// -----------------------------------------------------------------------------
// ONE folder-containment check for every voice start path: the signed grant
// fence (relay_cwd_fence.go), the one-shot drivers' per-turn re-check
// (containedCwd: Antigravity, OpenCode, Muse Code), the Grok ACP cwd probe and
// the ACP and MSP fences. The filesystem answers it, never a string compare:
// case variants match exactly where the volume folds case, a shared-prefix
// sibling is never inside, and a symlink or junction resolves to where it
// really points before anything is compared.
// -----------------------------------------------------------------------------

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// dirWithin reports whether candidate is root, or a directory below it, as the
// host filesystem answers it:
//
//  1. both paths are resolved to the path the filesystem itself gives them
//     (realDirPath: every symlink, and on Windows every junction and 8.3
//     short name), which also collapses `.` and `..`;
//  2. both must be absolute paths of existing directories;
//  3. candidate is walked up through filepath.Dir to its volume root, and is
//     within root at the first ancestor (or itself) that os.SameFile says IS
//     root.
//
// Identity, not spelling: on a case-insensitive volume `/Users/a/AIExpedite`
// and `/users/a/aiexpedite` are one directory, on a case-sensitive one they
// are two; an 8.3 short name or a trailing-dot alias on Windows is the same
// folder. A missing path, another drive or a path that escapes through `..`
// is never within.
func dirWithin(root, candidate string) bool {
	if root == "" || candidate == "" || !filepath.IsAbs(root) || !filepath.IsAbs(candidate) {
		return false
	}
	realRoot, err := realDirPath(root)
	if err != nil {
		return false
	}
	realCandidate, err := realDirPath(candidate)
	if err != nil {
		return false
	}
	rootInfo, err := os.Stat(realRoot)
	if err != nil || !rootInfo.IsDir() {
		return false
	}
	info, err := os.Stat(realCandidate)
	if err != nil || !info.IsDir() {
		return false
	}
	for cur := realCandidate; ; {
		if os.SameFile(info, rootInfo) {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
		if info, err = os.Stat(cur); err != nil {
			return false
		}
	}
}

// containedCwd resolves symlinks on cwd and, when workspaceRoot is set,
// verifies the resolved cwd stays inside the root (dirWithin), returning the
// real (symlink-free) cwd to use as the process working directory. Called from
// a one-shot-per-turn manager's Start with an empty root (resolve-only — the
// result BECOMES the session's root) and again per turn (in the Send path,
// just before launch) against that captured root, so a cwd component that is
// swapped for a symlink between <kind>_native_start and <kind>_native_send
// resolves elsewhere and cannot run an auto-approving CLI outside the
// directory the session was started in.
//
// Shared by the Antigravity, OpenCode and one-shot native managers: they
// launch no process until a turn arrives, which is exactly what leaves the
// TOCTOU window this closes.
func containedCwd(cwd, workspaceRoot string) (string, error) {
	resolvedCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", fmt.Errorf("cwd %q symlink resolution failed: %w", cwd, err)
	}
	if workspaceRoot == "" {
		return resolvedCwd, nil
	}
	resolvedRoot, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return "", fmt.Errorf("workspace root %q symlink resolution failed: %w", workspaceRoot, err)
	}
	if !dirWithin(resolvedRoot, resolvedCwd) {
		return "", fmt.Errorf("cwd %q is outside the session's workspace root %q (the directory the session was started in)", resolvedCwd, resolvedRoot)
	}
	return resolvedCwd, nil
}

// resolvedPathWithin is dirWithin for a path that may name a folder that does
// not exist yet (an ACP `session/new` or MSP workspace cwd the CLI creates):
// the nearest EXISTING ancestor of resolved must be root or below it. resolved
// must already come from resolveCwdForContainment — every link on its existing
// prefix followed and no `..` left — so the missing suffix is plain names that
// cannot climb out of the ancestor checked here.
func resolvedPathWithin(root, resolved string) bool {
	if resolved == "" || !filepath.IsAbs(resolved) {
		return false
	}
	for cur := filepath.Clean(resolved); ; {
		if _, err := os.Lstat(cur); err == nil {
			return dirWithin(root, cur)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
}
