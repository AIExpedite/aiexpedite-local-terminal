package main

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// putUnreadableFile puts a folder where path's file would be: it is there (it
// stats) but every read of it fails (EISDIR; Windows answers
// ERROR_INVALID_FUNCTION), the way a file whose permissions or disk refuse
// the read does. The test is skipped where a folder reads as a file.
func putUnreadableFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(path); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Skipf("reading the folder at %s gave %v: an unreadable file cannot be staged here", path, err)
	}
}

// putUnstatableFile puts a symbolic link to itself where path's file would
// be: every stat of it fails for another reason than that it is not there
// (ELOOP), the way a stat its folder's permissions or the disk refuse does,
// and it does so for root too. The test is skipped where such a link cannot
// be made or stats as no such file.
func putUnstatableFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(path), path); err != nil {
		t.Skipf("no symbolic link can be made here (%v): an unstatable file cannot be staged", err)
	}
	if _, err := os.Stat(path); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Skipf("the stat of a link to itself at %s gave %v: an unstatable file cannot be staged here", path, err)
	}
}

// makeUnlistable takes dir's permissions away until the test ends. The test
// is skipped where this user lists it all the same (root, Windows).
func makeUnlistable(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := os.ReadDir(dir); err == nil {
		t.Skipf("%s lists without its permissions for this user: an unlistable folder cannot be staged here", dir)
	}
}

// cliSessionDirNames reads a folder the way filepath.Glob does (no such
// folder, or a file, holds nothing) but says when a folder that is there
// could not be listed, which Glob drops: a probe must not read a session
// kept there as gone.
func TestCliSessionDirNamesSaysWhenAFolderCouldNotBeListed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rollout.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "2026"), 0o755); err != nil {
		t.Fatal(err)
	}
	names, err := cliSessionDirNames(dir)
	sort.Strings(names)
	if err != nil || strings.Join(names, ",") != "2026,rollout.jsonl" {
		t.Fatalf("names = %v, %v; want both entries", names, err)
	}
	for _, none := range []string{filepath.Join(dir, "absent"), filepath.Join(dir, "rollout.jsonl")} {
		if names, err := cliSessionDirNames(none); err != nil || len(names) != 0 {
			t.Fatalf("cliSessionDirNames(%s) = %v, %v; want nothing, no error", none, names, err)
		}
	}
	makeUnlistable(t, filepath.Join(dir, "2026"))
	if _, err := cliSessionDirNames(filepath.Join(dir, "2026")); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a folder that could not be listed = %v, want its read error", err)
	}
}
