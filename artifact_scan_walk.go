// File: artifact_scan_walk.go
// -----------------------------------------------------------------------------
// The media walk behind scanOutputFilesSince, reading several folders at once.
//
// Every session's `*_ended` frame waits for this walk: the session managers
// collect the media a session wrote (collectSessionArtifactsBounded) BEFORE
// they publish the frame, so the files ride on it, and terminal-service shows
// the session as running until the frame arrives. A session started in the
// computer's default working directory walks every checkout under it. That is
// what made a Muse Code MSP session take 42 s to end (dev, 2026-11-15): after
// End's 5 s grace and the kill, the walk of C:\aiexpedite (about 109,000
// folders and 760,000 files) ran for more than 30 s before the ended frame
// went out. The same walk delays every other kind's ended frame (a Codex
// session in that folder took 14 s to end; two Claude Code sessions hit the
// walk's 2-minute bound).
//
// Reading one folder at a time leaves the disk idle between calls. Reading
// artifactScanWorkers folders at once walks the same tree in about a fifth of
// the time (7.4 s down to 1.5 s on that computer, warm).
//
// The answer does not change: the same folders are pruned, the same files are
// kept, and the list is the one the one-folder-at-a-time walk
// (filepath.WalkDir) returned: the first maxUploadFiles matches in its order,
// names sorted and depth first. The walk checks the context before every
// entry, and a stopped walk returns the context's error with what it had
// found.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// artifactScanWorkers is how many folders the media walk reads at once. The
// walk is bound by the file system, not the CPU: eight readers took the
// measured walk from 7.4 s to 1.5 s, sixteen only to 1.2 s.
const artifactScanWorkers = 8

// walkArtifactCandidates returns the media files under absBase that the scan
// keeps: modified at or after cutoff (any, when cutoff is zero), at most
// maxUploadFiles of them, in filepath.WalkDir's order. When ctx stops the
// walk, the error is ctx's and the files are what was found by then.
func walkArtifactCandidates(ctx context.Context, absBase string, cutoff time.Time) ([]string, error) {
	return walkArtifactTree(ctx, absBase, cutoff, artifactScanWorkers, os.ReadDir)
}

// walkArtifactTree is walkArtifactCandidates with its readers and its folder
// read given, so a test can watch the reads.
func walkArtifactTree(
	ctx context.Context,
	absBase string,
	cutoff time.Time,
	workers int,
	readDir func(string) ([]os.DirEntry, error),
) ([]string, error) {
	files := []string{}
	if err := ctx.Err(); err != nil {
		return files, err
	}
	root, err := os.Lstat(absBase)
	if err != nil {
		// The workdir may have been removed between dispatch and session end.
		reportArtifactWalkError(absBase, err)
		return files, nil
	}
	if !root.IsDir() {
		// filepath.WalkDir hands a root that is not a folder to the file check.
		if artifactCandidate(absBase, absBase, fs.FileInfoToDirEntry(root), cutoff) {
			files = append(files, absBase)
		}
		return files, nil
	}
	if artifactScanSkipsLink(fs.FileInfoToDirEntry(root)) {
		return files, nil
	}
	if workers < 1 {
		workers = 1
	}

	// One shared list of folders still to read. A reader takes the newest
	// (depth first, so the list stays short), reads it, and adds its
	// subfolders. The walk is over when the list is empty and nobody is
	// reading, since only a reader can add to it, or when one reader stopped
	// on ctx.
	var (
		mu      sync.Mutex
		changed = sync.NewCond(&mu)
		pending = []string{absBase}
		reading int
		stopped error
		wg      sync.WaitGroup
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				for len(pending) == 0 && reading > 0 && stopped == nil {
					changed.Wait()
				}
				if len(pending) == 0 || stopped != nil {
					mu.Unlock()
					changed.Broadcast()
					return
				}
				dir := pending[len(pending)-1]
				pending = pending[:len(pending)-1]
				reading++
				mu.Unlock()

				subdirs, found, err := readArtifactDir(ctx, dir, absBase, cutoff, readDir)

				mu.Lock()
				reading--
				pending = append(pending, subdirs...)
				files = append(files, found...)
				if err != nil && stopped == nil {
					stopped = err
				}
				mu.Unlock()
				changed.Broadcast()
			}
		}()
	}
	wg.Wait()

	sortArtifactWalkOrder(files)
	if len(files) > maxUploadFiles {
		files = files[:maxUploadFiles]
	}
	return files, stopped
}

// readArtifactDir reads one folder of the walk: the subfolders still to walk
// and the files in it the scan keeps. It stops at the first entry after ctx
// ends, returning ctx's error.
func readArtifactDir(
	ctx context.Context,
	dir, absBase string,
	cutoff time.Time,
	readDir func(string) ([]os.DirEntry, error),
) (subdirs, found []string, err error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	entries, err := readDir(dir)
	if err != nil {
		// As filepath.WalkDir: a folder that cannot be read is skipped, the
		// entries it did list with it.
		reportArtifactWalkError(dir, err)
		return nil, nil, nil
	}
	for _, d := range entries {
		if err := ctx.Err(); err != nil {
			return subdirs, found, err
		}
		path := filepath.Join(dir, d.Name())
		if d.IsDir() {
			if !artifactScanSkipsDir(d) {
				subdirs = append(subdirs, path)
			}
			continue
		}
		if artifactCandidate(path, absBase, d, cutoff) {
			found = append(found, path)
		}
	}
	return subdirs, found, nil
}

// reportArtifactWalkError logs a folder the walk could not read. Permission
// and not-exist errors on individual subtrees (or on the root itself, when
// the workdir was removed between command dispatch and session end) are
// expected operational states and are not logged.
func reportArtifactWalkError(path string, err error) {
	if !errors.Is(err, fs.ErrPermission) && !errors.Is(err, fs.ErrNotExist) {
		fmt.Printf("[file-upload] Walk error at %s: %v\n", path, err)
	}
}

// artifactScanSkipsLink refuses any "directory" entry that is not a pure
// directory. This catches symlinked dirs on POSIX, NTFS junctions (created by
// `mklink /J`) and mount points on Windows — both of which have ModeIrregular
// set on Go 1.20+ even though IsDir() returns true — plus anything else exotic
// the OS surfaces (Windows reparse-point tags we don't know about). The walk
// never follows a symlink, and this keeps it out of junctions too.
func artifactScanSkipsLink(d fs.DirEntry) bool {
	return d.Type()&^os.ModeDir != 0
}

// artifactScanSkipsDir reports whether the walk prunes a subfolder (never the
// root): a link (artifactScanSkipsLink), an ignored dependency or cache
// folder, or a hidden one.
func artifactScanSkipsDir(d fs.DirEntry) bool {
	if artifactScanSkipsLink(d) {
		return true
	}
	// Prune ignored directories. Match by base name only — a project named
	// "vendor" at the repo root is rare enough that the false positive is
	// acceptable, and the user can rename if needed.
	name := d.Name()
	if _, skip := ignoredDirs[name]; skip {
		return true
	}
	// Hidden directories are pruned too, EXCEPT the .maestro convention used
	// by Maestro for mobile UI testing output.
	return strings.HasPrefix(name, ".") && name != ".maestro"
}

// artifactCandidate reports whether the scan keeps the file at path.
func artifactCandidate(path, absBase string, d fs.DirEntry, cutoff time.Time) bool {
	// Only upload regular files. d.Type() returns just the type bits
	// (ModeDir, ModeSymlink, ModeNamedPipe, ModeSocket, ModeDevice,
	// ModeCharDevice, ModeIrregular) — a value of zero means a regular
	// file. This blocks symlinks, pipes, sockets, devices, and the
	// ModeIrregular tag Windows uses for reparse points other than
	// symlinks (e.g. NTFS junctions).
	if d.Type() != 0 {
		return false
	}

	ext := strings.ToLower(filepath.Ext(path))
	if _, ok := mediaExtensions[ext]; !ok {
		return false
	}

	// Sensitive-name guard: even though the extension is on the media
	// allowlist, refuse to upload basenames that look like disguised
	// credential / secret files. Catches the case where someone (or
	// some misconfigured tool) drops `.env.png` or `id_rsa.jpg` into
	// the workdir.
	baseLower := strings.ToLower(filepath.Base(path))
	for _, prefix := range sensitiveBasenamePrefixes {
		if strings.HasPrefix(baseLower, prefix) {
			LogSecurityEvent(SecEvtPathTraversal, "blocked sensitive-looking filename from upload",
				"path", path, "site", "detectOutputFilesSince.sensitiveBasename")
			return false
		}
	}

	// Defense in depth: the walk shouldn't leave absBase since we
	// rejected symlinks, but verify before uploading.
	if !isPathSafeUnder(path, absBase) {
		LogSecurityEvent(SecEvtPathTraversal, "blocked file outside base during walk",
			"path", path, "base_dir", absBase, "site", "detectOutputFilesSince")
		return false
	}

	if !cutoff.IsZero() {
		info, infoErr := d.Info()
		if infoErr != nil {
			return false
		}
		if info.ModTime().Before(cutoff) {
			return false
		}
	}
	return true
}

// sortArtifactWalkOrder puts paths in filepath.WalkDir's order: each folder's
// names sorted byte by byte, a subfolder's files where its name falls. That is
// the paths compared one name at a time, which is a plain comparison once the
// separator sorts below every byte a name can hold (no name holds NUL).
func sortArtifactWalkOrder(paths []string) {
	sep := string(filepath.Separator)
	keys := make(map[string]string, len(paths))
	for _, p := range paths {
		keys[p] = strings.ReplaceAll(p, sep, "\x00")
	}
	sort.Slice(paths, func(i, j int) bool { return keys[paths[i]] < keys[paths[j]] })
}
