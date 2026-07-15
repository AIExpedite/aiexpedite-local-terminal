// File: file_upload_detect_test.go
// Unit tests for detectOutputFilesSince — the mtime-based file discovery
// that powers the post-session screenshot upload pipeline.
package main

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"testing"
	"time"
)

// freshOffset is how far past sessionStart we age "fresh" test files. Must
// comfortably exceed mtimeSkew (5s in production) so a future change to the
// skew window doesn't accidentally make these tests pass on broken code.
const freshOffset = 10 * time.Second

// writeFileAt creates a file with the given contents and sets its modtime.
func writeFileAt(t *testing.T, path string, content string, modTime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if !modTime.IsZero() {
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatalf("chtimes %s: %v", path, err)
		}
	}
}

// baseNames returns just the file names (no directory) for comparison.
func baseNames(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.Base(p)
	}
	sort.Strings(out)
	return out
}

// TestDetectOutputFilesSince_MtimeFilter verifies that only files modified at
// or after sessionStart are returned. Pre-existing files (older mtime) must
// be excluded so prior test runs aren't re-uploaded.
func TestDetectOutputFilesSince_MtimeFilter(t *testing.T) {
	dir := t.TempDir()
	sessionStart := time.Now()

	// Pre-existing screenshot (modified well before sessionStart).
	writeFileAt(t, filepath.Join(dir, "old-screenshot.png"), "old", sessionStart.Add(-1*time.Hour))
	// Fresh screenshot from this session.
	writeFileAt(t, filepath.Join(dir, "new-screenshot.png"), "new", sessionStart.Add(freshOffset))

	files := detectOutputFilesSince(dir, sessionStart)
	got := baseNames(files)
	want := []string{"new-screenshot.png"}

	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

// TestDetectOutputFilesSince_FindsArbitraryFolders confirms the headline
// behavior: screenshots are found regardless of which subdirectory the test
// framework chose to write them to. This is the test that captures the
// regression we're fixing — the prior implementation only scanned two
// hardcoded directories.
func TestDetectOutputFilesSince_FindsArbitraryFolders(t *testing.T) {
	dir := t.TempDir()
	sessionStart := time.Now()
	freshTime := sessionStart.Add(freshOffset)

	// One screenshot per framework convention, plus a custom user dir.
	cases := map[string]string{
		"cypress/screenshots/login.png":       "playwright would normally not see this",
		"wdio-output/home.png":                "WebdriverIO output",
		"artifacts/detox-failure.png":         "Detox artifacts",
		".maestro/output/checkout-flow.png":   "Maestro hidden-dir convention (allow-listed)",
		"my-custom-screenshots/dashboard.png": "user's custom config",
		"test-results-ui/legacy.png":          "still works alongside everything else",
	}
	for relPath := range cases {
		writeFileAt(t, filepath.Join(dir, relPath), "x", freshTime)
	}

	files := detectOutputFilesSince(dir, sessionStart)
	got := baseNames(files)
	want := []string{
		"checkout-flow.png",
		"dashboard.png",
		"detox-failure.png",
		"home.png",
		"legacy.png",
		"login.png",
	}

	if len(got) != len(want) {
		t.Fatalf("expected %d files, got %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
}

// TestDetectOutputFilesSince_IgnoredDirs verifies that node_modules, .git,
// build caches, and similar directories are pruned even when they contain
// files with fresh mtime. The mtime filter alone would exclude pre-existing
// node_modules content, but pruning is a major perf win on large repos and
// also protects against a freshly-cloned/installed repo where node_modules
// mtimes are all "new."
func TestDetectOutputFilesSince_IgnoredDirs(t *testing.T) {
	dir := t.TempDir()
	sessionStart := time.Now()
	freshTime := sessionStart.Add(freshOffset)

	// A real screenshot we expect to find.
	writeFileAt(t, filepath.Join(dir, "keep-me.png"), "real", freshTime)

	// Files inside ignored dirs that share the fresh mtime — must NOT be
	// returned. node_modules in particular has lots of .png icons.
	ignoredCases := []string{
		"node_modules/some-pkg/icon.png",
		".git/objects/pack-abc.png",
		".next/static/screenshot.png",
		".cache/foo.png",
	}
	for _, p := range ignoredCases {
		writeFileAt(t, filepath.Join(dir, p), "x", freshTime)
	}

	files := detectOutputFilesSince(dir, sessionStart)
	got := baseNames(files)
	want := []string{"keep-me.png"}

	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

// TestDetectOutputFilesSince_ExtensionWhitelist checks that only known media
// extensions are uploaded — random JSON, HTML, source maps, and the like must
// not be swept up just because they were written during the session.
func TestDetectOutputFilesSince_ExtensionWhitelist(t *testing.T) {
	dir := t.TempDir()
	sessionStart := time.Now()
	freshTime := sessionStart.Add(freshOffset)

	keep := []string{"screenshot.png", "thumb.jpg", "anim.gif", "clip.webm", "demo.mp4", "snap.webp", "render.mov"}
	drop := []string{"report.json", "trace.html", "bundle.js.map", "notes.txt", "config.yml"}

	for _, f := range keep {
		writeFileAt(t, filepath.Join(dir, f), "x", freshTime)
	}
	for _, f := range drop {
		writeFileAt(t, filepath.Join(dir, f), "x", freshTime)
	}

	got := baseNames(detectOutputFilesSince(dir, sessionStart))

	wantSet := map[string]bool{}
	for _, f := range keep {
		wantSet[f] = true
	}
	for _, f := range got {
		if !wantSet[f] {
			t.Fatalf("unexpected file in result: %s (got %v)", f, got)
		}
		delete(wantSet, f)
	}
	if len(wantSet) > 0 {
		t.Fatalf("expected files not returned: %v", wantSet)
	}
}

// TestDetectOutputFilesSince_RespectsCap verifies the 50-file cap. Without it
// a runaway test could enqueue thousands of GCS uploads.
func TestDetectOutputFilesSince_RespectsCap(t *testing.T) {
	dir := t.TempDir()
	sessionStart := time.Now()
	freshTime := sessionStart.Add(freshOffset)

	// Produce 75 fresh screenshots — more than maxUploadFiles=50.
	for i := 0; i < 75; i++ {
		writeFileAt(t, filepath.Join(dir, "screenshots", "shot-"+strconv.Itoa(i)+".png"), "x", freshTime)
	}

	files := detectOutputFilesSince(dir, sessionStart)
	if len(files) > maxUploadFiles {
		t.Fatalf("expected at most %d files, got %d", maxUploadFiles, len(files))
	}
	if len(files) < maxUploadFiles {
		t.Fatalf("expected the cap to be reached (%d), got only %d files", maxUploadFiles, len(files))
	}
}

// TestDetectOutputFilesSince_ZeroTimeAcceptsAny confirms that passing a zero
// time disables the mtime filter. Tests rely on this, and it also serves as
// the fallback when start time is unknown.
func TestDetectOutputFilesSince_ZeroTimeAcceptsAny(t *testing.T) {
	dir := t.TempDir()

	// An "old" screenshot — clearly predates any plausible session start.
	old := time.Now().Add(-30 * 24 * time.Hour)
	writeFileAt(t, filepath.Join(dir, "ancient.png"), "old", old)

	files := detectOutputFilesSince(dir, time.Time{})
	got := baseNames(files)
	if len(got) != 1 || got[0] != "ancient.png" {
		t.Fatalf("expected ancient.png to be returned with zero cutoff, got %v", got)
	}
}

// TestDetectOutputFilesSince_HiddenDirPruning verifies that hidden directories
// (dotfiles) are pruned EXCEPT for the .maestro convention used for mobile UI
// testing output. A `.config/foo.png` in someone's home dir is noise; a
// `.maestro/output/flow.png` is exactly what we want.
func TestDetectOutputFilesSince_HiddenDirPruning(t *testing.T) {
	dir := t.TempDir()
	sessionStart := time.Now()
	freshTime := sessionStart.Add(freshOffset)

	writeFileAt(t, filepath.Join(dir, ".config", "noise.png"), "x", freshTime)
	writeFileAt(t, filepath.Join(dir, ".maestro", "output", "real.png"), "x", freshTime)
	writeFileAt(t, filepath.Join(dir, "visible.png"), "x", freshTime)

	got := baseNames(detectOutputFilesSince(dir, sessionStart))
	want := []string{"real.png", "visible.png"}

	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
}

// TestDetectOutputFilesSince_NonexistentWorkDir confirms the function returns
// an empty slice (not nil, not panic, not a walk error log spam) when the
// caller passes a workDir that doesn't exist on disk. This can happen when
// the command's effective working directory was removed between command
// dispatch and session end.
func TestDetectOutputFilesSince_NonexistentWorkDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	files := detectOutputFilesSince(missing, time.Now())
	if len(files) != 0 {
		t.Fatalf("expected empty result for missing workDir, got %v", files)
	}
	// Also confirm empty string short-circuits to empty.
	if got := detectOutputFilesSince("", time.Now()); len(got) != 0 {
		t.Fatalf("expected empty result for empty workDir, got %v", got)
	}
}

// TestDetectOutputFilesSince_MtimeSkewBoundary covers the clock-skew slack
// window. A file written exactly sessionStart - 4s should still be included
// (within the 5s skew); sessionStart - 6s should be excluded.
func TestDetectOutputFilesSince_MtimeSkewBoundary(t *testing.T) {
	dir := t.TempDir()
	sessionStart := time.Now()

	writeFileAt(t, filepath.Join(dir, "within-skew.png"), "x", sessionStart.Add(-4*time.Second))
	writeFileAt(t, filepath.Join(dir, "outside-skew.png"), "x", sessionStart.Add(-6*time.Second))

	got := baseNames(detectOutputFilesSince(dir, sessionStart))
	want := []string{"within-skew.png"}

	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

// TestDetectOutputFilesSince_SensitiveBasenameBlocklist verifies that files
// with credential-looking basenames are refused even when their extension is
// on the media allowlist. This is defense-in-depth against a misconfigured
// tool or careless dev dropping `.env.png` or `id_rsa.jpg` into the workdir.
func TestDetectOutputFilesSince_SensitiveBasenameBlocklist(t *testing.T) {
	dir := t.TempDir()
	sessionStart := time.Now()
	freshTime := sessionStart.Add(freshOffset)

	drop := []string{
		".env.png",
		".env.local.png",
		"id_rsa.jpg",
		"id_ed25519.png",
		"credentials.json.png",
		"service-account.gif",
		"service_account-prod.png",
		"secrets.png",
		"secret-token.mp4",
		".aws-config.png",
		".kube-config.png",
		".npmrc.png",
		".netrc.gif",
	}
	keep := []string{
		"environment-banner.png", // doesn't match ".env" prefix (no leading dot)
		"login-screen.png",
		"my-secrets-page-DESIGN.png", // doesn't match "secret" prefix
		"id_card-icon.png",           // doesn't match "id_rsa" / "id_ed25519" etc.
	}

	for _, f := range append(append([]string{}, drop...), keep...) {
		writeFileAt(t, filepath.Join(dir, f), "x", freshTime)
	}

	got := baseNames(detectOutputFilesSince(dir, sessionStart))
	gotSet := map[string]bool{}
	for _, f := range got {
		gotSet[f] = true
	}

	for _, f := range drop {
		if gotSet[f] {
			t.Fatalf("sensitive file %q leaked into upload set", f)
		}
	}
	for _, f := range keep {
		if !gotSet[f] {
			t.Fatalf("legitimate file %q was incorrectly blocked: got %v", f, got)
		}
	}
}

// TestDetectOutputFilesSince_FileSymlinkSkipped verifies that file symlinks
// pointing inside or outside workDir are silently skipped, never uploaded.
// Symlink creation on Windows requires elevated privileges or developer
// mode, so this test is skipped there.
func TestDetectOutputFilesSince_FileSymlinkSkipped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}
	dir := t.TempDir()
	outside := t.TempDir()
	sessionStart := time.Now()
	freshTime := sessionStart.Add(freshOffset)

	// A real file outside workDir we'd never want exfiltrated.
	writeFileAt(t, filepath.Join(outside, "secret-design.png"), "x", freshTime)
	// A regular file we DO want.
	writeFileAt(t, filepath.Join(dir, "real.png"), "x", freshTime)
	// A symlink inside workDir pointing OUTSIDE — must be ignored.
	if err := os.Symlink(filepath.Join(outside, "secret-design.png"), filepath.Join(dir, "link-out.png")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got := baseNames(detectOutputFilesSince(dir, sessionStart))
	want := []string{"real.png"}

	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

// TestDetectOutputFilesSince_DirSymlinkNotTraversed verifies that a symlink
// pointing to a directory is NOT walked into — even if that directory
// contains media files with fresh mtime.
func TestDetectOutputFilesSince_DirSymlinkNotTraversed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}
	dir := t.TempDir()
	outside := t.TempDir()
	sessionStart := time.Now()
	freshTime := sessionStart.Add(freshOffset)

	// Stage media OUTSIDE workDir — must not be returned.
	writeFileAt(t, filepath.Join(outside, "leaked.png"), "x", freshTime)
	// Real screenshot inside workDir.
	writeFileAt(t, filepath.Join(dir, "kept.png"), "x", freshTime)
	// Symlink whose target is the outside directory.
	if err := os.Symlink(outside, filepath.Join(dir, "shortcut")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got := baseNames(detectOutputFilesSince(dir, sessionStart))
	want := []string{"kept.png"}

	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

// TestDetectOutputFilesSince_MtimeExcludesPreExistingInNonIgnoredDir proves
// the mtime filter — by itself, with no help from the ignore-list — drops
// pre-existing files in an ordinary subdirectory the walk DOES descend into.
// Prior tests either co-located pre-existing files with the workDir root or
// inside an ignored dir, neither of which exercises the mtime-only path.
// This is the regression seal for "we re-uploaded last week's screenshots
// because the new session reset the cutoff but the ignore-list let the walk
// in."
func TestDetectOutputFilesSince_MtimeExcludesPreExistingInNonIgnoredDir(t *testing.T) {
	dir := t.TempDir()
	sessionStart := time.Now()

	// "screenshots/" is NOT in ignoredDirs — the walk descends into it.
	// One pre-existing PNG (well before session start) and one fresh PNG.
	writeFileAt(t, filepath.Join(dir, "screenshots", "prior-run.png"), "x", sessionStart.Add(-2*time.Hour))
	writeFileAt(t, filepath.Join(dir, "screenshots", "this-run.png"), "x", sessionStart.Add(freshOffset))

	got := baseNames(detectOutputFilesSince(dir, sessionStart))
	want := []string{"this-run.png"}

	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("expected %v, got %v — mtime filter failed to exclude pre-existing file in a non-ignored subdir", want, got)
	}
}

// TestUniqueScanRoots_DedupesAndDropsEmpty verifies the candidate-root
// builder skips empties and de-duplicates (case-insensitively on Windows).
func TestUniqueScanRoots_DedupesAndDropsEmpty(t *testing.T) {
	got := uniqueScanRoots("", "/a/repo", "/a/repo", "", "/b/other")
	want := []string{"/a/repo", "/b/other"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order/content mismatch: expected %v, got %v", want, got)
		}
	}

	// All-empty → empty slice (never nil-panics downstream).
	if r := uniqueScanRoots("", ""); len(r) != 0 {
		t.Fatalf("expected empty, got %v", r)
	}

	// Case-insensitive de-dup on Windows only.
	roots := uniqueScanRoots("C:\\Repo", "c:\\repo")
	if runtime.GOOS == "windows" {
		if len(roots) != 1 {
			t.Fatalf("windows: expected case-insensitive dedup to 1 root, got %v", roots)
		}
	} else if len(roots) != 2 {
		t.Fatalf("non-windows: expected 2 distinct roots, got %v", roots)
	}
}

// TestDetectOutputFilesAcross_MergesSiblingRoots is the regression seal for
// the headline bug: the agent is spawned in one dir (home) but writes
// screenshots under a SIBLING repo dir. Scanning both roots finds them;
// scanning only the spawn dir would miss them.
func TestDetectOutputFilesAcross_MergesSiblingRoots(t *testing.T) {
	base := t.TempDir()
	sessionStart := time.Now()

	spawnDir := filepath.Join(base, "home") // where the CLI launched
	repoDir := filepath.Join(base, "repo")  // where it cd'd + wrote
	if err := os.MkdirAll(spawnDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Screenshot written under the sibling repo's output dir.
	writeFileAt(t, filepath.Join(repoDir, "test-results-ui", "step-1.png"), "x", sessionStart.Add(freshOffset))

	// Spawn dir alone → nothing.
	if got := detectOutputFilesAcross([]string{spawnDir}, sessionStart); len(got) != 0 {
		t.Fatalf("spawn-dir-only scan should find nothing, got %v", baseNames(got))
	}
	// Both roots → the screenshot surfaces.
	got := baseNames(detectOutputFilesAcross([]string{spawnDir, repoDir}, sessionStart))
	want := []string{"step-1.png"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("expected %v across roots, got %v", want, got)
	}
}

// TestDetectOutputFilesAcross_DedupesOverlappingRoots ensures a file
// reachable from two overlapping roots is only returned once.
func TestDetectOutputFilesAcross_DedupesOverlappingRoots(t *testing.T) {
	dir := t.TempDir()
	sessionStart := time.Now()
	writeFileAt(t, filepath.Join(dir, "sub", "shot.png"), "x", sessionStart.Add(freshOffset))

	// dir and dir/sub overlap; the file under sub must appear exactly once.
	got := detectOutputFilesAcross([]string{dir, filepath.Join(dir, "sub")}, sessionStart)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 file after dedup, got %d: %v", len(got), baseNames(got))
	}
}

// TestDetectOutputFilesAcross_EmptyRoots returns an empty slice, never nil-panics.
func TestDetectOutputFilesAcross_EmptyRoots(t *testing.T) {
	if got := detectOutputFilesAcross(nil, time.Now()); got == nil || len(got) != 0 {
		t.Fatalf("expected non-nil empty slice, got %v", got)
	}
}
