// File: worktree_hardening_test.go
// -----------------------------------------------------------------------------
// Tests for the boundaries the worktree built-ins rely on, each one pinned
// against the specific way it was wrong before:
//
//   - a git PROOF and the DELETE must denote the same path. They used to be
//     taken from two different strings;
//   - a presence reading has an age. It used to have none, so a redelivered
//     dispatch could act on "idle" indefinitely;
//   - the admin directory a remove deletes is pinned under the validated main
//     repository, not under a path read out of the worktree's own `.git`;
//   - a seeded file is not read through a linked directory in the root
//     checkout. Only the file itself was checked.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ─── the git proof and the delete name the same path ────────────────────────

// A pathspec is not a path. `:(glob)x`, a wildcard or a glob escape would make
// `git ls-files -- <arg>` answer about a different set of paths than the one
// being deleted, and an empty answer reads as "holds no tracked file" — a
// vacuous proof authorising a real delete.
func TestResolveInsideWorktreeRefusesGitPatterns(t *testing.T) {
	wt := resolvedWorktreePath{Abs: filepath.Join(t.TempDir(), "repo")}
	for _, rel := range []string{
		":(glob)node_modules",
		":!node_modules",
		":(icase)node_modules",
		"node_modules/*",
		"node_mod?les",
		"node_modules/[abc]",
	} {
		if _, err := resolveInsideWorktree(wt, rel); err == nil {
			t.Fatalf("%q was accepted as a directory name", rel)
		}
	}

	// A backslash is platform-dependent and both answers are correct. On Windows
	// it IS a separator, so the path splits into ordinary segments and the git
	// half comes back slash-joined. Everywhere else it is a legal character in a
	// file name AND git's glob escape, so it is refused.
	cand, err := resolveInsideWorktree(wt, `node_modules\sub`)
	if runtime.GOOS == "windows" {
		if err != nil {
			t.Fatalf("a Windows-separated path was refused: %v", err)
		}
		if cand.GitPath != "node_modules/sub" {
			t.Fatalf("git path %q, want node_modules/sub", cand.GitPath)
		}
	} else if err == nil {
		t.Fatalf("a backslash in a segment was accepted as a directory name (git path %q)", cand.GitPath)
	}
}

// Both halves come from the one list of validated segments, so they cannot
// denote different directories. GitPath is always slash-joined because a
// pathspec always is, whatever the platform separator.
func TestResolveInsideWorktreeReturnsAgreeingHalves(t *testing.T) {
	root := t.TempDir()
	wt := resolvedWorktreePath{Abs: filepath.Join(root, "repo")}
	for _, tc := range []struct{ rel, wantGit string }{
		{"node_modules", "node_modules"},
		{"packages/web/node_modules", "packages/web/node_modules"},
		{"node_modules/", "node_modules"},
		{"packages//web//node_modules", "packages/web/node_modules"},
	} {
		cand, err := resolveInsideWorktree(wt, tc.rel)
		if err != nil {
			t.Fatalf("%q: %v", tc.rel, err)
		}
		if cand.GitPath != tc.wantGit {
			t.Fatalf("%q: git path %q, want %q", tc.rel, cand.GitPath, tc.wantGit)
		}
		if strings.Contains(cand.GitPath, `\`) {
			t.Fatalf("%q: git path %q carries a backslash", tc.rel, cand.GitPath)
		}
		// The agreement itself: joining the git path onto the worktree must
		// land exactly where the delete would go.
		want := filepath.Join(wt.Abs, filepath.FromSlash(cand.GitPath))
		if pathIdentityKey(cand.Abs) != pathIdentityKey(want) {
			t.Fatalf("%q: abs %q and git path %q name different directories", tc.rel, cand.Abs, cand.GitPath)
		}
	}
}

// ─── a presence reading has a shelf life ────────────────────────────────────

func TestPresenceReadingAge(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	stamp := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }

	for _, tc := range []struct {
		name     string
		at       string
		wantFail bool
		because  string
	}{
		{"fresh", stamp(-10 * time.Second), false, ""},
		{"at the edge", stamp(-presenceReadingMaxAge + time.Second), false, ""},
		{"stale", stamp(-10 * time.Minute), true, "old"},
		{"missing", "", true, "no presenceAt"},
		{"blank", "   ", true, "no presenceAt"},
		{"unparseable", "yesterday", true, "RFC3339"},
		{"epoch millis, not RFC3339", "1760788800000", true, "RFC3339"},
		// A stamp ahead of the device's clock would never expire, so skew
		// would silently disable the bound.
		{"far future", stamp(10 * time.Minute), true, "ahead of this device's clock"},
		{"ordinary skew is tolerated", stamp(30 * time.Second), false, ""},
	} {
		err := checkPresenceReadingAge(tc.at, now)
		if tc.wantFail {
			if err == nil {
				t.Fatalf("%s: accepted %q", tc.name, tc.at)
			}
			if !strings.Contains(err.Error(), tc.because) {
				t.Fatalf("%s: refused for the wrong reason: %v", tc.name, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: refused %q: %v", tc.name, tc.at, err)
		}
	}
}

// The restore itself must refuse a stale reading, not just the helper — and it
// must refuse it as presence_stale, so the Computers tab can say which
// condition failed rather than conflating it with "the person is at the
// keyboard".
func TestRestoreRefusesAStalePresenceReading(t *testing.T) {
	fx := newWorktreeFixture(t)
	res, err := restoreRootCheckout(context.Background(), rootRestoreRequest{
		Root:          fx.Root,
		RepoPath:      fx.MainRepo,
		DefaultBranch: "main",
		Presence:      "idle",
		PresenceAt:    time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Restored {
		t.Fatal("a restore ran on a half-hour-old presence reading")
	}
	if res.Reason != restoreReasonPresenceStale {
		t.Fatalf("refused as %q, want %q", res.Reason, restoreReasonPresenceStale)
	}
}

func TestRestoreRefusesAMissingPresenceTimestamp(t *testing.T) {
	fx := newWorktreeFixture(t)
	res, err := restoreRootCheckout(context.Background(), rootRestoreRequest{
		Root:          fx.Root,
		RepoPath:      fx.MainRepo,
		DefaultBranch: "main",
		Presence:      "away",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Restored || res.Reason != restoreReasonPresenceStale {
		t.Fatalf("restored=%v reason=%q, want a presence_stale refusal", res.Restored, res.Reason)
	}
}

// ─── the root-use fence binds the platform's own writers ────────────────────

// The fence was a mechanism with no reader: nothing in production consulted it,
// so a restore switching the root's branch could overlap a built-in writing the
// same repository's `.git/worktrees`.
func TestWorktreeWritersRefuseWhileTheRootUseFenceIsHeld(t *testing.T) {
	fx := newWorktreeFixture(t)
	release, ok := acquireRootUseFence(fx.MainRepo)
	if !ok {
		t.Fatal("the fence was already held")
	}
	defer release()

	cfg := &Config{CommandSecret: "s3cret"}
	for _, name := range []string{worktreeEnsureCommand, worktreeRemoveCommand} {
		if !worktreeCommandWritesRootBookkeeping(name) {
			t.Fatalf("%s is not classed as a root-bookkeeping writer", name)
		}
		_, err := runInterceptedDeviceCommand(context.Background(), cfg, commandMsg{
			Command: name,
			Args:    []string{`{"mainRepo":` + quoteJSON(fx.MainRepo) + `}`},
		})
		if err == nil || !strings.Contains(err.Error(), "root-use fence") {
			t.Fatalf("%s did not refuse while the fence was held: %v", name, err)
		}
	}

	// A read and a worktree-local delete are not fenced: refusing the list
	// would leave the sweep with no inventory for that tick and buy nothing.
	for _, name := range []string{worktreeListCommand, worktreeEvictDepsCommand} {
		if worktreeCommandWritesRootBookkeeping(name) {
			t.Fatalf("%s must not be fenced", name)
		}
	}

	// And a DIFFERENT repository is unaffected — the fence is per root.
	other := filepath.Join(t.TempDir(), "elsewhere")
	if isRootUseFenced(other) {
		t.Fatal("the fence leaked to an unrelated path")
	}
}

func TestRootUseFenceIsReleasedAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	before := heldRootUseFenceCount()
	release, ok := acquireRootUseFence(dir)
	if !ok {
		t.Fatal("could not take a free fence")
	}
	if _, second := acquireRootUseFence(dir); second {
		t.Fatal("the fence was handed out twice")
	}
	release()
	release() // idempotent
	if isRootUseFenced(dir) {
		t.Fatal("the fence stayed held after release")
	}
	if got := heldRootUseFenceCount(); got != before {
		t.Fatalf("fence count %d, want %d", got, before)
	}
}

// ─── a seeded file is not read through a link ───────────────────────────────

func TestRefuseLinkedComponents(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secrets"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "real", "ok"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := refuseLinkedComponents(base, "real/ok"); err != nil {
		t.Fatalf("an ordinary path was refused: %v", err)
	}
	// Absent is not unsafe: the caller's own lstat reports it, with a message
	// about the missing file rather than about links.
	if err := refuseLinkedComponents(base, "real/nope"); err != nil {
		t.Fatalf("an absent path was refused as a link: %v", err)
	}

	if err := os.Symlink(outside, filepath.Join(base, "config")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("creating a symlink needs privilege on this host")
		}
		t.Fatal(err)
	}
	// The link is ABOVE the file, which is why checking the file alone missed
	// it: `config/secrets` lstats as an ordinary file.
	if err := refuseLinkedComponents(base, "config/secrets"); err == nil {
		t.Fatal("a file read through a linked directory was accepted")
	}
	if err := refuseLinkedComponents(base, "config"); err == nil {
		t.Fatal("a linked final component was accepted")
	}
}

// quoteJSON is a minimal string quoter for the fixture paths above, which carry
// Windows separators that a raw JSON literal would read as escapes.
func quoteJSON(s string) string {
	return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"`
}
