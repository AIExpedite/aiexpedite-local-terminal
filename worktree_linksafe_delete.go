// File: worktree_linksafe_delete.go
// -----------------------------------------------------------------------------
// The one delete used by every worktree built-in
// (PARALLEL_RUNS_AND_COMPUTER_MAINTENANCE_PLAN.md §9.2 "Removal").
//
// THE RULE: the walk never follows a reparse point or a symlink. It UNLINKS one
// and never descends it.
//
// This is not a theoretical hardening. On 2026-09-17 a recursive remove that
// followed a junction emptied `local/node_modules` AND two entire sibling
// repositories, `.git` included, losing unpushed work. A dependency directory is
// exactly where such a junction lives: `node_modules/@aiexpedite/*` are links
// into sibling checkouts, and `frontend/node_modules/@aiexpedite/eslint-plugin-
// architecture` is a symlink to `frontend/eslint-rules`. So this is the common
// case, not the rare one.
//
// Why not os.RemoveAll: it is link-safe for the ENTRY it removes, but it decides
// what to descend from os.ReadDir's type bits alone — and since Go 1.23 a
// Windows junction is NOT reported as ModeSymlink, so a junction reads as a
// plain directory and is descended. isWorktreeLink's os.Readlink probe is what
// closes that, and it has to run on every entry.
//
// Why not `git worktree remove`: it deletes the working tree recursively itself
// — that is its job, and `--force` only waives the dirty / locked refusals. So
// calling it would traverse the very junctions this walk exists to avoid,
// before anything link-safe had looked at the path. It is never used.
// -----------------------------------------------------------------------------

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	// linkSafeDeleteMaxDepth bounds the recursion. A dependency tree nests
	// deeply (npm's historical layout especially), so the bound is generous —
	// it exists to turn a pathological or cyclic layout into a reported
	// refusal rather than a stack overflow.
	linkSafeDeleteMaxDepth = 256
)

// linkSafeDeleteStats is what a delete did, so a removal can be reported
// rather than merely claimed. Links is the count that was UNLINKED without
// being descended — the number that would have been followed by a naive
// recursive delete.
type linkSafeDeleteStats struct {
	Files int   `json:"files"`
	Dirs  int   `json:"dirs"`
	Links int   `json:"links"`
	Bytes int64 `json:"bytes"`
}

func (s *linkSafeDeleteStats) add(o linkSafeDeleteStats) {
	s.Files += o.Files
	s.Dirs += o.Dirs
	s.Links += o.Links
	s.Bytes += o.Bytes
}

// linkSafeRemoveAll removes path and everything beneath it WITHOUT ever
// descending a symlink or a Windows junction. An absent path is success with
// zero stats, so a retry after a partial failure is safe.
func linkSafeRemoveAll(path string) (linkSafeDeleteStats, error) {
	var stats linkSafeDeleteStats
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return stats, nil
		}
		return stats, err
	}
	err = linkSafeRemoveEntry(path, info, 0, &stats)
	return stats, err
}

// linkSafeRemoveEntry removes one lstat'd entry. info MUST come from os.Lstat
// (or os.DirEntry.Info of a ReadDir entry, which is also lstat-based): a
// Stat-derived mode describes the link TARGET and would send the walk through
// it.
func linkSafeRemoveEntry(path string, info os.FileInfo, depth int, stats *linkSafeDeleteStats) error {
	if depth > linkSafeDeleteMaxDepth {
		return fmt.Errorf("refusing to delete below %d levels at %s: the layout is cyclic or pathological", linkSafeDeleteMaxDepth, path)
	}
	// A link is UNLINKED. Checked before IsDir(), because a Windows junction
	// and a directory symlink both report IsDir() and descending either is the
	// 2026-09-17 incident.
	if isWorktreeLink(info, path) {
		if err := removeOne(path); err != nil {
			return err
		}
		stats.Links++
		return nil
	}
	if !info.IsDir() {
		if err := removeOne(path); err != nil {
			return err
		}
		stats.Files++
		if sz := info.Size(); sz > 0 {
			stats.Bytes += sz
		}
		return nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		// An unreadable directory is never force-deleted around: report it.
		return fmt.Errorf("reading %s: %w", path, err)
	}
	for _, e := range entries {
		childInfo, infoErr := e.Info()
		if infoErr != nil {
			if errors.Is(infoErr, fs.ErrNotExist) {
				continue // vanished under us; nothing to delete
			}
			return fmt.Errorf("lstat %s: %w", filepath.Join(path, e.Name()), infoErr)
		}
		if err := linkSafeRemoveEntry(filepath.Join(path, e.Name()), childInfo, depth+1, stats); err != nil {
			return err
		}
	}
	if err := removeOne(path); err != nil {
		return err
	}
	stats.Dirs++
	return nil
}

// removeOne unlinks exactly one entry, retrying once after clearing a
// read-only bit. os.Remove on Windows maps to DeleteFile for a file and
// RemoveDirectory for a directory, a junction or a directory symlink — in every
// case it unlinks the entry itself and never its target.
func removeOne(path string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	// A read-only file (npm and pip both leave some) refuses to unlink on
	// Windows until the attribute goes.
	if chmodErr := os.Chmod(path, 0o666); chmodErr == nil {
		if err2 := os.Remove(path); err2 == nil || errors.Is(err2, fs.ErrNotExist) {
			return nil
		}
	}
	return fmt.Errorf("removing %s: %w", path, err)
}

// countReparsePoints walks path and returns how many symlinks and junctions it
// holds, without following or removing any. The scan a clone has to pass
// (§4.3.1) and a cheap way for a test to assert the walk's frame of reference.
func countReparsePoints(path string) (int, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	if isWorktreeLink(info, path) {
		return 1, nil
	}
	if !info.IsDir() {
		return 0, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, e := range entries {
		childInfo, infoErr := e.Info()
		if infoErr != nil {
			continue
		}
		child := filepath.Join(path, e.Name())
		if isWorktreeLink(childInfo, child) {
			total++
			continue
		}
		if childInfo.IsDir() {
			n, nerr := countReparsePoints(child)
			if nerr != nil {
				return total, nerr
			}
			total += n
		}
	}
	return total, nil
}
