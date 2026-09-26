package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// depsFixture describes one checkout on disk.
type depsFixture struct {
	lock       map[string]any // package-lock.json "packages"; nil = no lockfile
	hidden     map[string]any // node_modules/.package-lock.json "packages"; nil = no hidden lockfile
	folders    []string       // package folders to create (relative, slash-separated)
	links      map[string]string
	staleAfter []string // folders whose mtime is set AFTER the hidden lockfile's
}

var fixturePlatform = npmPlatform{OS: "win32", CPU: "x64"}

func pkg(version, integrity string) map[string]any {
	return map[string]any{"version": version, "resolved": "https://registry.npmjs.org/x/-/x-" + version + ".tgz", "integrity": integrity}
}

// baseLock is a small but representative tree: a top-level package, a scoped
// package and a nested (deduped-away) copy.
func baseLock() map[string]any {
	return map[string]any{
		"":                              map[string]any{"name": "app", "version": "1.0.0"},
		"node_modules/left-pad":         pkg("1.3.0", "sha512-left"),
		"node_modules/@x/y":             pkg("2.0.0", "sha512-xy"),
		"node_modules/a":                pkg("1.0.0", "sha512-a"),
		"node_modules/a/node_modules/b": pkg("3.1.0", "sha512-b"),
	}
}

func hiddenOf(lock map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range lock {
		if k != "" {
			out[k] = v
		}
	}
	return out
}

func baseFolders() []string {
	return []string{"node_modules/left-pad", "node_modules/@x/y", "node_modules/a", "node_modules/a/node_modules/b"}
}

func writeJSONFile(t *testing.T, p string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// build lays the fixture out and fixes every mtime: all directories at base,
// the hidden lockfile at base+10s, staleAfter folders at base+20s.
func (f depsFixture) build(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range f.folders {
		p := filepath.Join(root, filepath.FromSlash(d))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "package.json"), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		// A package's own files (lib/) are never walked.
		if err := os.MkdirAll(filepath.Join(p, "lib"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "node_modules", ".bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for link, target := range f.links {
		lp := filepath.Join(root, filepath.FromSlash(link))
		if err := os.MkdirAll(filepath.Dir(lp), 0o755); err != nil {
			t.Fatal(err)
		}
		makeDirLink(t, filepath.FromSlash(target), lp)
	}
	if f.lock != nil {
		writeJSONFile(t, filepath.Join(root, "package-lock.json"), map[string]any{"lockfileVersion": 3, "packages": f.lock})
	}
	hiddenPath := filepath.Join(root, "node_modules", ".package-lock.json")
	if f.hidden != nil {
		writeJSONFile(t, hiddenPath, map[string]any{"lockfileVersion": 3, "packages": f.hidden})
	}

	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	// filepath.Walk does not follow links, so a link target is reached (and
	// stamped) through its own path.
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return os.Chtimes(p, base, base)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.hidden != nil {
		if err := os.Chtimes(hiddenPath, base.Add(10*time.Second), base.Add(10*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range f.staleAfter {
		p := filepath.Join(root, filepath.FromSlash(d))
		if err := os.Chtimes(p, base.Add(20*time.Second), base.Add(20*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestVerifyDependencies(t *testing.T) {
	withEntry := func(m map[string]any, k string, v any) map[string]any {
		out := map[string]any{}
		for kk, vv := range m {
			out[kk] = vv
		}
		if v == nil {
			delete(out, k)
		} else {
			out[k] = v
		}
		return out
	}
	optionalLinux := map[string]any{
		"version": "0.21.5", "integrity": "sha512-esb-linux", "optional": true,
		"os": []string{"linux"}, "cpu": []string{"x64"},
	}
	optionalWin := map[string]any{
		"version": "0.21.5", "integrity": "sha512-esb-win", "optional": true,
		"os": []string{"win32"}, "cpu": []string{"x64"},
	}

	tests := []struct {
		name       string
		fixture    depsFixture
		wantMatch  bool
		wantReason string
		wantDetail string // substring expected in mismatches[0]
	}{
		{
			name:      "match",
			fixture:   depsFixture{lock: baseLock(), hidden: hiddenOf(baseLock()), folders: baseFolders()},
			wantMatch: true,
		},
		{
			name: "root entry exists only in the lockfile",
			fixture: depsFixture{
				lock:    withEntry(baseLock(), "", map[string]any{"name": "app", "version": "9.9.9", "dependencies": map[string]any{"a": "^1"}}),
				hidden:  hiddenOf(baseLock()),
				folders: baseFolders(),
			},
			wantMatch: true,
		},
		{
			name: "optional entry for another platform is dropped",
			fixture: depsFixture{
				lock:    withEntry(withEntry(baseLock(), "node_modules/@esbuild/linux-x64", optionalLinux), "node_modules/@esbuild/win32-x64", optionalWin),
				hidden:  withEntry(hiddenOf(baseLock()), "node_modules/@esbuild/win32-x64", optionalWin),
				folders: append(baseFolders(), "node_modules/@esbuild/win32-x64"),
			},
			wantMatch: true,
		},
		{
			name: "optional entry for THIS platform must be installed",
			fixture: depsFixture{
				lock:    withEntry(baseLock(), "node_modules/@esbuild/win32-x64", optionalWin),
				hidden:  hiddenOf(baseLock()),
				folders: baseFolders(),
			},
			wantReason: verifyReasonEntriesDiffer,
			wantDetail: "node_modules/@esbuild/win32-x64: not installed",
		},
		{
			name:       "version mismatch",
			fixture:    depsFixture{lock: baseLock(), hidden: withEntry(hiddenOf(baseLock()), "node_modules/left-pad", pkg("1.2.0", "sha512-left")), folders: baseFolders()},
			wantReason: verifyReasonEntriesDiffer,
			wantDetail: "node_modules/left-pad: version 1.2.0 != 1.3.0",
		},
		{
			name:       "integrity mismatch",
			fixture:    depsFixture{lock: baseLock(), hidden: withEntry(hiddenOf(baseLock()), "node_modules/@x/y", pkg("2.0.0", "sha512-OTHER")), folders: baseFolders()},
			wantReason: verifyReasonEntriesDiffer,
			wantDetail: "node_modules/@x/y: integrity differs",
		},
		{
			name:       "extra installed entry",
			fixture:    depsFixture{lock: withEntry(baseLock(), "node_modules/a/node_modules/b", nil), hidden: hiddenOf(baseLock()), folders: baseFolders()},
			wantReason: verifyReasonEntriesDiffer,
			wantDetail: "node_modules/a/node_modules/b: installed but not in package-lock.json",
		},
		{
			name:       "missing folder",
			fixture:    depsFixture{lock: baseLock(), hidden: hiddenOf(baseLock()), folders: []string{"node_modules/left-pad", "node_modules/@x/y", "node_modules/a"}},
			wantReason: verifyReasonFolderMissing,
			wantDetail: "node_modules/a/node_modules/b: listed but not on disk",
		},
		{
			name:       "unlisted top-level folder",
			fixture:    depsFixture{lock: baseLock(), hidden: hiddenOf(baseLock()), folders: append(baseFolders(), "node_modules/copied-by-hand")},
			wantReason: verifyReasonUnlistedFolder,
			wantDetail: "node_modules/copied-by-hand: not in the hidden lockfile",
		},
		{
			name:       "unlisted scoped folder",
			fixture:    depsFixture{lock: baseLock(), hidden: hiddenOf(baseLock()), folders: append(baseFolders(), "node_modules/@x/z")},
			wantReason: verifyReasonUnlistedFolder,
			wantDetail: "node_modules/@x/z: not in the hidden lockfile",
		},
		{
			name:       "unlisted nested folder",
			fixture:    depsFixture{lock: baseLock(), hidden: hiddenOf(baseLock()), folders: append(baseFolders(), "node_modules/a/node_modules/c")},
			wantReason: verifyReasonUnlistedFolder,
			wantDetail: "node_modules/a/node_modules/c: not in the hidden lockfile",
		},
		{
			name:       "hidden lockfile older than a package folder",
			fixture:    depsFixture{lock: baseLock(), hidden: hiddenOf(baseLock()), folders: baseFolders(), staleAfter: []string{"node_modules/@x/y"}},
			wantReason: verifyReasonHiddenStale,
			wantDetail: "node_modules/@x/y: modified after the hidden lockfile",
		},
		{
			name:       "hidden lockfile older than node_modules itself",
			fixture:    depsFixture{lock: baseLock(), hidden: hiddenOf(baseLock()), folders: baseFolders(), staleAfter: []string{"node_modules"}},
			wantReason: verifyReasonHiddenStale,
			wantDetail: "node_modules: modified after the hidden lockfile",
		},
		{
			name:       "no hidden lockfile",
			fixture:    depsFixture{lock: baseLock(), hidden: nil, folders: baseFolders()},
			wantReason: verifyReasonNoHiddenLockfile,
		},
		{
			name:       "no lockfile",
			fixture:    depsFixture{lock: nil, hidden: hiddenOf(baseLock()), folders: baseFolders()},
			wantReason: verifyReasonNoLockfile,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := tt.fixture.build(t)
			res, err := verifyDependencies(context.Background(), envVerifyDepsRequest{Path: root, Manager: "npm"}, fixturePlatform)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Match != tt.wantMatch || res.Reason != tt.wantReason {
				t.Fatalf("match=%v reason=%q, want match=%v reason=%q (mismatches %q)", res.Match, res.Reason, tt.wantMatch, tt.wantReason, res.Mismatches)
			}
			if tt.wantDetail != "" && (len(res.Mismatches) == 0 || res.Mismatches[0] != tt.wantDetail) {
				t.Fatalf("mismatches = %q, want first %q", res.Mismatches, tt.wantDetail)
			}
			if res.Mismatches == nil {
				t.Fatal("mismatches must encode as [], never null")
			}
			if tt.fixture.lock != nil {
				raw, err := os.ReadFile(filepath.Join(root, "package-lock.json"))
				if err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256(raw)
				if !res.LockfilePresent || res.LockfileSha256 != hex.EncodeToString(sum[:]) {
					t.Fatalf("lockfile present=%v sha=%q, want sha256 of package-lock.json bytes", res.LockfilePresent, res.LockfileSha256)
				}
			} else if res.LockfilePresent || res.LockfileSha256 != "" {
				t.Fatalf("no lockfile: present=%v sha=%q", res.LockfilePresent, res.LockfileSha256)
			}
			if res.HiddenLockfilePresent != (tt.fixture.hidden != nil) {
				t.Fatalf("hiddenLockfilePresent=%v", res.HiddenLockfilePresent)
			}
		})
	}
}

func TestVerifyDependencies_LinkEntries(t *testing.T) {
	link := map[string]any{"resolved": "packages/ws-a", "link": true}
	ws := map[string]any{"name": "ws-a", "version": "0.1.0"}
	lock := baseLock()
	lock["node_modules/ws-a"] = link
	lock["packages/ws-a"] = ws
	lock["packages/ws-a/node_modules/dep"] = pkg("4.0.0", "sha512-dep")
	hidden := hiddenOf(lock)

	fx := depsFixture{
		lock:    lock,
		hidden:  hidden,
		folders: append(baseFolders(), "packages/ws-a", "packages/ws-a/node_modules/dep"),
		links:   map[string]string{"node_modules/ws-a": "../packages/ws-a"},
	}
	root := fx.build(t)
	res, err := verifyDependencies(context.Background(), envVerifyDepsRequest{Path: root, Manager: "npm"}, fixturePlatform)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Match {
		t.Fatalf("expected match with a workspace link, got %q %q", res.Reason, res.Mismatches)
	}

	// A link retargeted in the lockfile no longer matches.
	lock2 := hiddenOf(lock)
	lock2[""] = map[string]any{"name": "app"}
	lock2["node_modules/ws-a"] = map[string]any{"resolved": "packages/ws-b", "link": true}
	fx.lock = lock2
	root = fx.build(t)
	res, err = verifyDependencies(context.Background(), envVerifyDepsRequest{Path: root, Manager: "npm"}, fixturePlatform)
	if err != nil {
		t.Fatal(err)
	}
	if res.Match || res.Reason != verifyReasonEntriesDiffer || !strings.Contains(res.Mismatches[0], "link target") {
		t.Fatalf("expected a link-target difference, got match=%v %q %q", res.Match, res.Reason, res.Mismatches)
	}
}

// makeDirLink creates a directory link: a symlink where the OS allows it, else
// (Windows without the symlink privilege) a junction — what npm itself uses for
// workspace links there. The link is removed non-recursively before the temp
// dir's recursive cleanup runs, so that cleanup never has a link to follow.
func makeDirLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS != "windows" {
			t.Skipf("symlinks unavailable here: %v", err)
		}
		abs := filepath.Join(filepath.Dir(link), target)
		if out, jerr := exec.Command("cmd", "/c", "mklink", "/J", link, abs).CombinedOutput(); jerr != nil {
			t.Skipf("neither a symlink (%v) nor a junction (%v: %s) could be created", err, jerr, out)
		}
	}
	t.Cleanup(func() { _ = os.Remove(link) })
}

// A dangling link (a symlink, or a junction on Windows) for a linked
// dependency must count as missing, not present — otherwise a deleted link
// target would verify as a match.
func TestVerifyDependencies_DanglingLinkIsMissing(t *testing.T) {
	lock := baseLock()
	lock["node_modules/linked"] = map[string]any{"resolved": "linked-target", "link": true}
	lock["linked-target"] = map[string]any{"name": "linked", "version": "0.0.1"}
	root := depsFixture{lock: lock, hidden: hiddenOf(lock), folders: append(baseFolders(), "linked-target")}.build(t)

	target := filepath.Join(root, "linked-target")
	link := filepath.Join(root, "node_modules", "linked")
	makeDirLink(t, filepath.Join("..", "linked-target"), link)
	// Adding the link touched node_modules: put its mtime back before the
	// hidden lockfile's, so only the link itself can decide the result.
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(filepath.Join(root, "node_modules"), old, old); err != nil {
		t.Fatal(err)
	}

	res, err := verifyDependencies(context.Background(), envVerifyDepsRequest{Path: root, Manager: "npm"}, fixturePlatform)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Match {
		t.Fatalf("a live link should verify: reason=%q mismatches=%q", res.Reason, res.Mismatches)
	}

	// Remove the target: the link now dangles. Drop the target's own entry so
	// the ONLY evidence left is the link entry itself.
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	delete(lock, "linked-target")
	writeJSONFile(t, filepath.Join(root, "package-lock.json"), map[string]any{"lockfileVersion": 3, "packages": lock})
	hiddenPath := filepath.Join(root, "node_modules", ".package-lock.json")
	writeJSONFile(t, hiddenPath, map[string]any{"lockfileVersion": 3, "packages": hiddenOf(lock)})
	_ = os.Chtimes(filepath.Join(root, "node_modules"), old, old)
	_ = os.Chtimes(hiddenPath, old.Add(10*time.Second), old.Add(10*time.Second))

	res, err = verifyDependencies(context.Background(), envVerifyDepsRequest{Path: root, Manager: "npm"}, fixturePlatform)
	if err != nil {
		t.Fatal(err)
	}
	if res.Match || res.Reason != verifyReasonFolderMissing ||
		len(res.Mismatches) != 1 || res.Mismatches[0] != "node_modules/linked: listed but not on disk" {
		t.Fatalf("dangling link: match=%v reason=%q mismatches=%q", res.Match, res.Reason, res.Mismatches)
	}
}

func TestVerifyDependencies_Inputs(t *testing.T) {
	res, err := verifyDependencies(context.Background(), envVerifyDepsRequest{Path: t.TempDir(), Manager: "pnpm"}, fixturePlatform)
	if err != nil || res.Match || res.Reason != verifyReasonUnsupportedManager {
		t.Fatalf("pnpm: res=%+v err=%v", res, err)
	}
	if _, err := verifyDependencies(context.Background(), envVerifyDepsRequest{Path: "relative/dir", Manager: "npm"}, fixturePlatform); err == nil {
		t.Fatal("expected an error for a relative path")
	}
	if _, err := verifyDependencies(context.Background(), envVerifyDepsRequest{Path: filepath.Join(t.TempDir(), "missing"), Manager: "npm"}, fixturePlatform); err == nil {
		t.Fatal("expected an error for a missing directory")
	}

	// A v1 lockfile has no packages map: unproven, not an error.
	root := t.TempDir()
	writeJSONFile(t, filepath.Join(root, "package-lock.json"), map[string]any{"lockfileVersion": 1, "dependencies": map[string]any{}})
	writeJSONFile(t, filepath.Join(root, "node_modules", ".package-lock.json"), map[string]any{"lockfileVersion": 3, "packages": map[string]any{}})
	res, err = verifyDependencies(context.Background(), envVerifyDepsRequest{Path: root, Manager: "npm"}, fixturePlatform)
	if err != nil || res.Match || res.Reason != verifyReasonEntriesDiffer {
		t.Fatalf("v1 lockfile: res=%+v err=%v", res, err)
	}
}

func TestVerifyDependencies_MismatchesCapped(t *testing.T) {
	lock := map[string]any{"": map[string]any{}}
	for i := 0; i < 25; i++ {
		lock["node_modules/p"+string(rune('a'+i))] = pkg("1.0.0", "sha512-x")
	}
	root := depsFixture{lock: lock, hidden: map[string]any{}, folders: nil}.build(t)
	res, err := verifyDependencies(context.Background(), envVerifyDepsRequest{Path: root, Manager: "npm"}, fixturePlatform)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Mismatches) != maxVerifyMismatches {
		t.Fatalf("expected %d mismatches, got %d", maxVerifyMismatches, len(res.Mismatches))
	}
}

func TestNpmPlatformChecks(t *testing.T) {
	cases := []struct {
		value string
		list  []string
		want  bool
	}{
		{"linux", []string{"linux"}, true},
		{"win32", []string{"linux", "darwin"}, false},
		{"win32", []string{"!linux"}, true},
		{"linux", []string{"!linux"}, false},
		{"x64", []string{"any"}, true},
		{"arm64", []string{"!x64", "arm64"}, true},
		{"ia32", []string{"!x64", "arm64"}, false},
	}
	for _, c := range cases {
		if got := npmCheckList(c.value, c.list); got != c.want {
			t.Errorf("npmCheckList(%q, %q) = %v, want %v", c.value, c.list, got, c.want)
		}
	}

	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	linuxGlibc := npmPlatform{OS: "linux", CPU: "x64", Libc: "glibc"}
	if !npmPlatformOK(lockEntry{OS: raw("linux"), CPU: raw([]string{"x64"}), Libc: raw([]string{"glibc"})}, linuxGlibc) {
		t.Error("glibc package should install on glibc linux")
	}
	if npmPlatformOK(lockEntry{Libc: raw([]string{"musl"})}, linuxGlibc) {
		t.Error("musl package must not install on glibc")
	}
	if npmPlatformOK(lockEntry{Libc: raw([]string{"glibc"})}, fixturePlatform) {
		t.Error("a libc constraint fails off Linux")
	}
	if !npmPlatformOK(lockEntry{}, fixturePlatform) {
		t.Error("no constraints installs everywhere")
	}

	// normalizeLockPackages drops "", workspace folders, and platform-excluded
	// optional entries, keeps everything else.
	got := normalizeLockPackages(map[string]lockEntry{
		"":                          {},
		"packages/ws":               {Version: "1"},
		"node_modules/a":            {Version: "1"},
		"node_modules/opt-linux":    {Optional: true, OS: raw([]string{"linux"})},
		"node_modules/devopt-linux": {DevOptional: true, OS: raw([]string{"linux"})},
		"node_modules/req-linux":    {OS: raw([]string{"linux"})},
	}, fixturePlatform)
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	if len(keys) != 2 || got["node_modules/a"].Version != "1" {
		t.Fatalf("normalized keys %q", keys)
	}
	if _, ok := got["node_modules/req-linux"]; !ok {
		t.Fatal("a NON-optional platform-mismatched entry is kept (npm would fail the install, not skip it)")
	}
}

func TestCompareLockEntries_ResolvedWhenNoIntegrity(t *testing.T) {
	want := map[string]lockEntry{"node_modules/g": {Version: "1.0.0", Resolved: "git+ssh://git@github.com/o/g.git#aaa"}}
	have := map[string]lockEntry{"node_modules/g": {Version: "1.0.0", Resolved: "git+ssh://git@github.com/o/g.git#bbb"}}
	if diffs := compareLockEntries(want, have); !reflect.DeepEqual(diffs, []string{"node_modules/g: resolved differs"}) {
		t.Fatalf("diffs %q", diffs)
	}
	have["node_modules/g"] = want["node_modules/g"]
	if diffs := compareLockEntries(want, have); len(diffs) != 0 {
		t.Fatalf("diffs %q", diffs)
	}
}

func TestCurrentNpmPlatform(t *testing.T) {
	p := currentNpmPlatform()
	switch runtime.GOOS {
	case "windows":
		if p.OS != "win32" {
			t.Fatalf("OS %q", p.OS)
		}
	default:
		if p.OS != runtime.GOOS {
			t.Fatalf("OS %q", p.OS)
		}
	}
	if runtime.GOARCH == "amd64" && p.CPU != "x64" {
		t.Fatalf("CPU %q", p.CPU)
	}
	if runtime.GOOS != "linux" && p.Libc != "" {
		t.Fatalf("libc %q off linux", p.Libc)
	}
}
