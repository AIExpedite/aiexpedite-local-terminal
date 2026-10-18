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
	"encoding/json"
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

// ─── the device advertises the built-ins ────────────────────────────────────

// Contract pin: "worktreeBuiltIns" is the wire key terminal-service reads
// (shared-constants DEVICE_WORKTREE_CAPABILITIES.BUILT_INS). Renaming it makes
// every already-released agent read as legacy, so the literal IS the assertion
// here — the same reason readOnlySessions is pinned this way.
func TestCapabilities_ReportWorktreeBuiltIns(t *testing.T) {
	raw, err := json.Marshal(newMachineCapabilities())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["worktreeBuiltIns"] != true {
		t.Fatalf("capabilities JSON = %s, want worktreeBuiltIns true", raw)
	}
	// Never omitempty: the server reads absent as "cannot", and a build that
	// one day reports false must say so rather than vanish from the map.
	if _, present := got["worktreeBuiltIns"]; !present {
		t.Fatal("worktreeBuiltIns was omitted from the capabilities JSON")
	}
}

// The flag must be true in exactly the build that HAS the commands, so these
// two facts are asserted together rather than in separate files where they
// could drift apart.
func TestCapabilityMatchesTheCommandsThisBuildHandles(t *testing.T) {
	caps := newMachineCapabilities()
	for _, name := range worktreeBuiltIns() {
		if !isWorktreeDeviceCommand(name) {
			t.Fatalf("%s is advertised by the capability but not recognised", name)
		}
	}
	if !caps.WorktreeBuiltIns {
		t.Fatal("this build handles the worktree commands but does not advertise them")
	}
}

// ─── a rescue ref is a REF, and refs are stricter than directories ──────────

// Allowing a dot-leading repository folder (`.github`, legal and ordinary)
// made the rescue ref `aix/rescue/<feature>/.github-7b3c91`, and git refuses a
// ref whose component begins with a dot. The validator missed it because its
// pattern anchors only the FIRST character of the whole ref — always the `a` of
// `aix/` — so every later component went unchecked.
func TestRescueRefRefusesAComponentGitWouldReject(t *testing.T) {
	for _, bad := range []string{
		"aix/rescue/feat-aabbccddeeff/.github-7b3c91",
		"aix/rescue/.feat-aabbccddeeff/repo-7b3c91",
		"aix/rescue/feat-aabbccddeeff/repo-7b3c91.",
	} {
		if _, err := validateRescueRef(bad); err == nil {
			t.Fatalf("%q was accepted as a rescue ref", bad)
		}
	}
	// And the ordinary shape still passes.
	for _, ok := range []string{
		"aix/rescue/feat-aabbccddeeff/repo-7b3c91",
		"aix/rescue/feat-aabbccddeeff/my.repo-7b3c91",
		"aix/rescue/feat-aabbccddeeff/github-7b3c91",
		// A DASH may begin a ref component — git documents it, and the argument
		// reaching git is `HEAD:refs/heads/aix/rescue/...`, so a dash this deep
		// can never read as an option. Refusing it blocked every removal of a
		// worktree whose folder legitimately begins with `-`, which the path
		// policy explicitly permits.
		"aix/rescue/feat-aabbccddeeff/-repo-7b3c91",
	} {
		if _, err := validateRescueRef(ok); err != nil {
			t.Fatalf("%q was refused: %v", ok, err)
		}
	}
}

// The folder policy and the ref validator have to agree, or a folder the policy
// admits cannot be rescued. This asserts the agreement directly rather than
// restating either rule, so the next change to one of them fails here.
func TestEveryAdmissiblePolicyFolderBuildsAValidRescueRef(t *testing.T) {
	for _, folder := range []string{
		"frontend-7b3c91",
		".github-7b3c91",
		"_repo-7b3c91",
		"-repo-7b3c91",
		"my.repo.v2-7b3c91",
		"..-7b3c91",
	} {
		rel := "aixwt/" + wtTestFeatureFolder + "/" + folder
		if !worktreeRelativePathPattern.MatchString(rel) {
			t.Fatalf("%q is not admitted by the path policy; fix the test, not the rule", rel)
		}
		ref := "aix/rescue/" + refComponentFromFolder(wtTestFeatureFolder) +
			"/" + refComponentFromFolder(folder)
		if _, err := validateRescueRef(ref); err != nil {
			t.Fatalf("policy admits folder %q but its rescue ref %q is refused: %v", folder, ref, err)
		}
	}
}

// The folder a removal rescues from may begin with a dot; the ref it pushes to
// may not. Stripping leading dots stays unique BECAUSE of the discriminator:
// `.github` and `github` are different repositories, so their folders carry
// different 6-hex suffixes and the two rescue branches cannot collide.
func TestRefComponentFromFolderIsRefSafeAndStillUnique(t *testing.T) {
	cases := map[string]string{
		"frontend-7b3c91": "frontend-7b3c91",
		".github-7b3c91":  "github-7b3c91",
		"..weird-7b3c91":  "weird-7b3c91",
		"_repo-7b3c91":    "_repo-7b3c91",
	}
	for folder, want := range cases {
		if got := refComponentFromFolder(folder); got != want {
			t.Fatalf("refComponentFromFolder(%q) = %q, want %q", folder, got, want)
		}
		ref := "aix/rescue/feat-aabbccddeeff/" + refComponentFromFolder(folder)
		if _, err := validateRescueRef(ref); err != nil {
			t.Fatalf("%q built an invalid rescue ref %q: %v", folder, ref, err)
		}
	}
	// The collision the discriminator prevents: a repository named `.github`
	// and one named `github` differ in their discriminator, so stripping the
	// dot cannot make one rescue branch shadow the other.
	a := refComponentFromFolder(".github-aaaaaa")
	b := refComponentFromFolder("github-bbbbbb")
	if a == b {
		t.Fatalf("two repositories collapsed onto one rescue component: %q", a)
	}
}

// ─── the companion fetch honours the request's one network switch ───────────

// `fetch` is the only network switch an ensure has, so a caller using it for an
// offline or deterministic run must not be made to reach every companion's
// remote. And "we chose not to look" has to stay distinguishable from "we
// looked and could not see", because the two mean different things about how
// stale the companion's target may be.
func TestEnsureCompanionHonoursTheFetchOptOut(t *testing.T) {
	fx := newWorktreeFixture(t)
	ctx := context.Background()

	companion := func(fetch bool) worktreeCompanionResult {
		t.Helper()
		req := fx.ensureReq("feature/companion-fetch-" + map[bool]string{true: "on", false: "off"}[fetch])
		req.Fetch = &fetch
		req.Companions = []worktreeCompanionSpec{{
			MainRepo:      fx.MainRepo,
			Path:          fx.wtPath("lib-" + map[bool]string{true: "on", false: "off"}[fetch]),
			DefaultBranch: fx.Default,
			Remote:        "origin",
		}}
		res, err := ensureWorktree(ctx, req)
		if err != nil {
			t.Fatalf("ensure: %v", err)
		}
		if len(res.Companions) != 1 {
			t.Fatalf("companions = %+v, want exactly one", res.Companions)
		}
		return res.Companions[0]
	}

	off := companion(false)
	if !off.FetchSkipped {
		t.Fatalf("fetch:false still fetched the companion's remote: %+v", off)
	}
	if off.FetchFailed {
		t.Fatalf("an opted-out fetch must not report a FAILURE: %+v", off)
	}

	on := companion(true)
	if on.FetchSkipped {
		t.Fatalf("fetch:true skipped the companion's remote: %+v", on)
	}
	// The fixture's remote is local, so the fetch itself must succeed — if this
	// ever fails it means the fetch is not actually being attempted.
	if on.FetchFailed {
		t.Fatalf("the companion fetch failed against a local remote: %+v", on)
	}
}
