// File: artifact_scan_guard.go
// Which folders the post-command / post-session media scan may walk.
package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// artifactScanHomeDir finds the home folder for artifactScanRootRefusal. It is
// a variable so tests can place the home folder without moving HOME or
// USERPROFILE for the whole process.
var artifactScanHomeDir = os.UserHomeDir

// What artifactScanRootRefusal says a refused root "is".
const (
	artifactScanRefusedFilesystemRoot = "a filesystem root"
	artifactScanRefusedHome           = "the home folder"
	artifactScanRefusedHomeAncestor   = "a folder that contains the home folder"
)

// artifactScanRootRefusal says why the media-artifact scan must not walk root,
// or returns "" when it may. scanOutputFilesSince asks before it walks
// anything, for the root as given and again once symlinks are resolved.
//
// # WHY
//
// The scan walks every folder under its root. A command sent without a cwd
// runs in Config.WorkingDirectory, which is the home folder, and its scan
// walked the whole home folder: on macOS that reached ~/Library, so the system
// asked the user whether the app "would like to access data from other apps",
// and it ran into the two-minute collection bound on every command
// (repository discovery, 2026-11-08). Screenshots and recordings belong to a
// project folder, and a filesystem root, the home folder and the folders above
// it are never one.
//
// Only those folders are refused. Everything INSIDE the home folder is scanned
// as before: a checkout, a worktree, OneDrive\Documents\GitHub\<repo>, and on
// Windows the temp folder under AppData, where the agent's own temporary
// folders and Go's t.TempDir live.
//
// Two paths name the same folder when their text matches after Clean (ignoring
// case on windows and darwin, as pathIdentityKey does, and reading a \\?\
// prefix as the plain path) or when os.SameFile says so. The identity check
// catches the spellings text cannot: an 8.3 short name, a symlink, a junction
// (EvalSymlinks has not followed those since Go 1.23) and the macOS firmlink
// /System/Volumes/Data/Users/<name>. Apart from root itself, only the home
// folder and its ancestors are stat'ed, so nothing else inside the home folder
// is touched before the walk would start.
func artifactScanRootRefusal(root string) string {
	if root == "" {
		return ""
	}
	if !filepath.IsAbs(root) {
		abs, err := filepath.Abs(root)
		if err != nil {
			return ""
		}
		root = abs
	}
	key := artifactScanPathKey(root)
	if filepath.Dir(key) == key {
		return artifactScanRefusedFilesystemRoot
	}

	homeDirs := artifactScanHomeAndAncestors()
	if len(homeDirs) == 0 {
		return ""
	}
	// Text first: it needs no I/O and answers the usual spellings.
	rootKey := pathIdentityKey(key)
	for _, dir := range homeDirs {
		if dir.key == rootKey {
			return dir.refusal
		}
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return ""
	}
	for _, dir := range homeDirs {
		if info, err := os.Stat(dir.path); err == nil && os.SameFile(rootInfo, info) {
			return dir.refusal
		}
	}
	return ""
}

type artifactScanHomeEntry struct {
	path    string // as it is stat'ed
	key     string // pathIdentityKey(path), for the text comparison
	refusal string
}

// artifactScanHomeAndAncestors lists the home folder and every folder above
// it, up to and including its volume root, for the home folder as reported and
// again with symlinks resolved. It is empty when the home folder cannot be
// determined, and then only the filesystem-root check applies.
func artifactScanHomeAndAncestors() []artifactScanHomeEntry {
	home, err := artifactScanHomeDir()
	if err != nil || home == "" || !filepath.IsAbs(home) {
		return nil
	}
	spellings := []string{home}
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		spellings = append(spellings, resolved)
	}
	var entries []artifactScanHomeEntry
	seen := make(map[string]bool)
	for _, spelling := range spellings {
		refusal := artifactScanRefusedHome
		for dir := artifactScanPathKey(spelling); ; {
			if key := pathIdentityKey(dir); !seen[key] {
				seen[key] = true
				entries = append(entries, artifactScanHomeEntry{path: dir, key: key, refusal: refusal})
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir, refusal = parent, artifactScanRefusedHomeAncestor
		}
	}
	return entries
}

// artifactScanPathKey is p cleaned, with a Windows \\?\ (or \??\) prefix on a
// drive or UNC path spelled the ordinary way: \\?\C:\x is C:\x, and
// \\?\UNC\srv\share is \\srv\share. Go treats \\?\UNC as the VOLUME of the
// latter, so without this a verbatim share root would not look like a root.
// Other device paths (\\?\Volume{...}\) are left as they are; Go already reads
// their first component as the volume.
func artifactScanPathKey(p string) string {
	p = filepath.Clean(p)
	if runtime.GOOS != "windows" {
		return p
	}
	for _, prefix := range []string{`\\?\`, `\??\`} {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := p[len(prefix):]
		switch {
		case len(rest) >= 2 && rest[1] == ':':
			return filepath.Clean(rest)
		case len(rest) >= 4 && strings.EqualFold(rest[:4], `UNC\`):
			return filepath.Clean(`\\` + rest[4:])
		}
	}
	return p
}
