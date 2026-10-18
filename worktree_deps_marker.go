// File: worktree_deps_marker.go
// -----------------------------------------------------------------------------
// The provenance marker `prepare` writes for every dependency directory the
// platform creates, and the proof of disposability eviction needs before it
// deletes one (PARALLEL_RUNS_AND_COMPUTER_MAINTENANCE_PLAN.md §4.3.1, §9.2).
//
// WHY THE PLATFORM RECORDS ITS OWN PROVENANCE. npm leaves
// `node_modules/.package-lock.json`, but a Python virtualenv, Bundler's
// `vendor/bundle` and Mix's `deps` keep no portable record of which lockfile
// produced them — so a check built on the ecosystem's own record would silently
// accept stale dependencies in exactly the ecosystems it cannot read.
//
// WHY THE MARKER LIVES IN APP DATA AND NOT BESIDE THE DIRECTORY. A marker
// written into the worktree would be an untracked file, and "nothing is
// uncommitted or untracked" is a removal gate (§9.2, §4.8) — the platform's own
// bookkeeping would then block its own removal, or worse, be excused by a
// special case that also excuses a person's file. Keyed by the directory's
// absolute path instead.
//
// WHAT THE MARKER HAS TO CARRY, and why each part:
//
//   - the ECOSYSTEM and the LOCKFILE DIGEST it installed from: reconciling the
//     worktree's current digest against this is what decides `depsReady`.
//     Marker absent counts as "different" — never as ready;
//   - a BASELINE MANIFEST (relative path, size, mtime per entry). Paths and
//     sizes, not content digests: cheap enough for a large dependency tree and
//     enough to diff. A manifest rather than bare mtimes because an mtime
//     comparison can only see entries that still EXIST — an agent that deletes
//     or renames a file inside an ignored tree leaves no surviving entry to
//     notice, and a scan built on mtimes would reinstall over the deletion and
//     quietly erase it. A manifest sees additions, modifications AND deletions;
//   - a CONTENT DIGEST over the tree. The manifest FINDS changes; the digest
//     AUTHORISES the delete. Size and mtime can be identical over changed bytes
//     after a timestamp-preserving copy, an archive extraction, or on a
//     coarse-resolution file system — and this gate deletes irreversibly, so it
//     cannot rest on a heuristic. The cheap scan decides whether a directory is
//     a candidate at all; the digest is recomputed once, on the one directory
//     about to go.
//
// A creation marker proves WHO MADE the directory, not that everything now
// inside it is reproducible. Build output the agent produced inside its own CLI
// session has no marker at all, and is therefore NOT evictable — stated plainly
// because it is a real reduction in what eviction reclaims.
// -----------------------------------------------------------------------------

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// depsMarkerVersion is bumped when the marker shape changes. An older
	// version reads as "no usable marker", which refuses eviction — the safe
	// direction.
	depsMarkerVersion = 1

	// maxBaselineEntries bounds a manifest. A large npm tree is a few hundred
	// thousand entries; beyond this the baseline is recorded INCOMPLETE, and an
	// incomplete baseline refuses eviction rather than approving a partial diff.
	maxBaselineEntries = 400000

	// maxTreeDigestBytes bounds the content digest. Exceeding it records the
	// digest as incomplete, which refuses eviction.
	maxTreeDigestBytes = int64(16) << 30

	// maxLockfileDigestBytes bounds a lockfile read for its digest.
	maxLockfileDigestBytes = int64(256) << 20

	depsMarkerDirName = "worktree-deps-markers"
)

// baselineEntry is one recorded entry of a prepared directory. Links are
// recorded as links and never followed, so a junction into a sibling checkout
// is part of the baseline rather than a door into it.
//
// SIZE AND MTIME ARE RECORDED FOR FILES ONLY. For a directory both are platform
// noise: on Windows a directory entry's metadata comes from the parent's cached
// directory enumeration, which lags behind the real value, so two scans of an
// untouched tree disagree about a directory's mtime and every prepared
// directory would read as modified. Dropping them loses nothing, because a
// change inside a directory shows up as its CHILDREN being added, modified or
// deleted — which is the whole reason this is a manifest and not a timestamp
// check.
//
// For a link, Target is recorded instead. That is strictly stronger than an
// mtime: repointing `node_modules/@scope/x` at another checkout leaves the
// link's own timestamp alone on most platforms, and it is exactly the change
// that would make the tree load code from somewhere else.
type baselineEntry struct {
	Path            string `json:"p"`
	Size            int64  `json:"s"`
	ModTimeUnixNano int64  `json:"m"`
	Dir             bool   `json:"d,omitempty"`
	Link            bool   `json:"l,omitempty"`
	Target          string `json:"t,omitempty"`
}

// baselineUnreadableTarget stands in for a link whose target cannot be read, so
// a dangling or unreadable link is still an entry that can differ.
const baselineUnreadableTarget = "<unreadable>"

// depsProvenanceMarker is what `prepare` recorded about one directory.
type depsProvenanceMarker struct {
	Version   int    `json:"version"`
	Ecosystem string `json:"ecosystem"`
	// Dir and Worktree are absolute paths as they were at prepare time. A
	// marker whose Dir no longer matches the directory being evaluated is not
	// this directory's marker.
	Dir      string `json:"dir"`
	Worktree string `json:"worktree"`
	// LockfilePath is relative to Worktree; LockfileSha256 is the digest of
	// the lockfile (or manifest, where there is no lockfile) installed from.
	LockfilePath   string `json:"lockfilePath,omitempty"`
	LockfileSha256 string `json:"lockfileSha256,omitempty"`
	InstallOptions string `json:"installOptions,omitempty"`
	// Method is "install" or "clone"; Relocatable records whether the clone was
	// earned by a scan (§4.3.1). Both default to the safe value.
	Method      string `json:"method,omitempty"`
	Relocatable bool   `json:"relocatable,omitempty"`

	CreatedAt time.Time `json:"createdAt"`

	Baseline         []baselineEntry `json:"baseline"`
	BaselineComplete bool            `json:"baselineComplete"`

	TreeSha256         string `json:"treeSha256,omitempty"`
	TreeDigestComplete bool   `json:"treeDigestComplete"`
}

// depsMarkerPath is where the marker for dir is stored: app data, keyed by the
// directory's absolute path (case-folded where the file system is).
func depsMarkerPath(dir string) string {
	key := pathIdentityKey(filepath.Clean(dir))
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(GetConfigDir(), depsMarkerDirName, hex.EncodeToString(sum[:])+".json")
}

// readDepsMarker loads the marker for dir. A missing, unreadable, malformed or
// older-version marker returns (nil, nil): "no usable marker", which every
// caller treats as "not ours to evict" and "dependencies not ready".
func readDepsMarker(dir string) (*depsProvenanceMarker, error) {
	raw, err := os.ReadFile(depsMarkerPath(dir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var m depsProvenanceMarker
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil
	}
	if m.Version != depsMarkerVersion {
		return nil, nil
	}
	if pathIdentityKey(m.Dir) != pathIdentityKey(filepath.Clean(dir)) {
		return nil, nil
	}
	return &m, nil
}

func writeDepsMarker(m *depsProvenanceMarker) error {
	path := depsMarkerPath(m.Dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func removeDepsMarker(dir string) error {
	err := os.Remove(depsMarkerPath(dir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// scanBaseline walks dir and records every entry, NEVER following a symlink or
// a Windows junction (it records it as a link instead). Returns the manifest
// sorted by path and whether it is complete.
func scanBaseline(dir string) ([]baselineEntry, bool, error) {
	entries := make([]baselineEntry, 0, 256)
	complete := true
	var walk func(cur string, depth int) error
	walk = func(cur string, depth int) error {
		if depth > linkSafeDeleteMaxDepth {
			complete = false
			return nil
		}
		items, err := os.ReadDir(cur)
		if err != nil {
			return err
		}
		for _, item := range items {
			full := filepath.Join(cur, item.Name())
			info, infoErr := item.Info()
			if infoErr != nil {
				if errors.Is(infoErr, fs.ErrNotExist) {
					continue
				}
				return infoErr
			}
			rel, relErr := filepath.Rel(dir, full)
			if relErr != nil {
				return relErr
			}
			if len(entries) >= maxBaselineEntries {
				complete = false
				return nil
			}
			be := baselineEntry{
				Path:            filepath.ToSlash(rel),
				Size:            info.Size(),
				ModTimeUnixNano: info.ModTime().UnixNano(),
			}
			if isWorktreeLink(info, full) {
				be.Link = true
				be.Size, be.ModTimeUnixNano = 0, 0 // platform noise; the target is the signal
				be.Target = baselineUnreadableTarget
				if target, lerr := os.Readlink(full); lerr == nil {
					be.Target = filepath.ToSlash(target)
				}
				entries = append(entries, be)
				continue
			}
			if info.IsDir() {
				be.Dir = true
				be.Size, be.ModTimeUnixNano = 0, 0 // see baselineEntry: both are platform noise
				entries = append(entries, be)
				if err := walk(full, depth+1); err != nil {
					return err
				}
				continue
			}
			entries = append(entries, be)
		}
		return nil
	}
	if err := walk(dir, 0); err != nil {
		return nil, false, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, complete, nil
}

// digestTree hashes dir's contents into one digest: for every entry, in sorted
// path order, its relative path, its kind, and either its bytes (a file) or its
// link target (a link). Directories contribute their path alone.
//
// Link targets are hashed rather than followed, for the same reason the delete
// walk unlinks them: following one leaves the directory's frame of reference.
func digestTree(dir string) (string, bool, error) {
	entries, complete, err := scanBaseline(dir)
	if err != nil {
		return "", false, err
	}
	h := sha256.New()
	var bytesRead int64
	for _, e := range entries {
		full := filepath.Join(dir, filepath.FromSlash(e.Path))
		kind := "f"
		switch {
		case e.Link:
			kind = "l"
		case e.Dir:
			kind = "d"
		}
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00", e.Path, kind, e.Size)
		switch kind {
		case "l":
			if e.Target == baselineUnreadableTarget {
				complete = false
				continue
			}
			h.Write([]byte(e.Target))
		case "f":
			if bytesRead+e.Size > maxTreeDigestBytes {
				complete = false
				continue
			}
			f, oerr := os.Open(full)
			if oerr != nil {
				complete = false
				continue
			}
			n, cerr := io.Copy(h, f)
			_ = f.Close()
			bytesRead += n
			if cerr != nil {
				complete = false
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)), complete, nil
}

// fileSha256 digests one file, bounded.
func fileSha256(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Size() > maxLockfileDigestBytes {
		return "", fmt.Errorf("%s is %d bytes, over the %d-byte bound", path, info.Size(), maxLockfileDigestBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// baselineDiff is what changed under a prepared directory since `prepare`
// recorded it. Empty means untouched.
type baselineDiff struct {
	Added    []string `json:"added,omitempty"`
	Modified []string `json:"modified,omitempty"`
	Deleted  []string `json:"deleted,omitempty"`
	// Total is the full count of differences; the three lists above are
	// bounded samples.
	Total int `json:"total"`
}

func (d baselineDiff) empty() bool { return d.Total == 0 }

// maxBaselineDiffSample bounds each list in a reported diff.
const maxBaselineDiffSample = 20

// diffBaseline compares a directory's current state with a recorded manifest.
// Additions, modifications and deletions all count: a modification is a
// changed size OR a changed mtime, because writing a file moves its mtime.
func diffBaseline(recorded []baselineEntry, current []baselineEntry) baselineDiff {
	var diff baselineDiff
	want := make(map[string]baselineEntry, len(recorded))
	for _, e := range recorded {
		want[e.Path] = e
	}
	have := make(map[string]struct{}, len(current))
	for _, e := range current {
		have[e.Path] = struct{}{}
		w, ok := want[e.Path]
		if !ok {
			diff.Total++
			if len(diff.Added) < maxBaselineDiffSample {
				diff.Added = append(diff.Added, e.Path)
			}
			continue
		}
		if w.Size != e.Size || w.ModTimeUnixNano != e.ModTimeUnixNano ||
			w.Dir != e.Dir || w.Link != e.Link || w.Target != e.Target {
			diff.Total++
			if len(diff.Modified) < maxBaselineDiffSample {
				diff.Modified = append(diff.Modified, e.Path)
			}
		}
	}
	for _, e := range recorded {
		if _, ok := have[e.Path]; !ok {
			diff.Total++
			if len(diff.Deleted) < maxBaselineDiffSample {
				diff.Deleted = append(diff.Deleted, e.Path)
			}
		}
	}
	return diff
}

// exportBaselineDiff writes a touched directory's added and modified entries,
// and a list of its deletions, to the app-data folder — the same treatment §9.2
// gives a platform stash before any drop. The directory itself STAYS; this is
// what makes keeping it a reported outcome rather than a silent one.
//
// It is deliberately NOT a claim that an arbitrary change can be losslessly
// reconstructed and the directory then safely deleted. Keeping the directory is
// the honest outcome; disk is reclaimed from the next candidate instead.
func exportBaselineDiff(dir string, diff baselineDiff) (string, error) {
	out := filepath.Join(GetConfigDir(), depsMarkerDirName, "exports",
		strconv.FormatInt(time.Now().UnixNano(), 10)+"-"+filepath.Base(dir)+".json")
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return "", err
	}
	payload := map[string]any{
		"dir":        dir,
		"recordedAt": time.Now().UTC().Format(time.RFC3339),
		"diff":       diff,
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(out, raw, 0o600); err != nil {
		return "", err
	}
	return out, nil
}

// Eviction verdict reasons. Persisted vocabulary: they travel to the server and
// onto the Computers tab.
const (
	evictReasonEvictable          = "evictable"
	evictReasonAbsent             = "absent"
	evictReasonNoMarker           = "no_marker"
	evictReasonBaselineIncomplete = "baseline_incomplete"
	evictReasonDigestIncomplete   = "digest_incomplete"
	evictReasonNotIgnored         = "not_ignored"
	evictReasonTrackedContent     = "tracked_content"
	evictReasonModifiedTracked    = "modified_tracked"
	evictReasonBaselineMismatch   = "baseline_mismatch"
	evictReasonDigestMismatch     = "digest_mismatch"
	evictReasonScanFailed         = "scan_failed"
)

// relocatableScanResult is the per-directory clone gate of §4.3.1, kept here
// because it is the same link-safe walk.
//
// The frame of reference is THE DIRECTORY BEING CLONED, never the worktree.
// Getting that wrong silently defeats the gate: `frontend/node_modules/
// @aiexpedite/eslint-plugin-architecture -> frontend/eslint-rules` resolves
// INSIDE the worktree, so a worktree-scoped test would pass it — and the clone
// would still load that package from the ROOT rather than the feature.
// Equivalently: evaluate each link as it would resolve from the destination.
type relocatableScanResult struct {
	Relocatable bool     `json:"relocatable"`
	Links       int      `json:"links"`
	Escaping    []string `json:"escaping,omitempty"`
}

// scanRelocatable reports whether dir embeds no link pointing outside itself.
// One escaping target means `install`, never a repair. When in doubt the answer
// is false: an unnecessary install costs minutes, a broken clone costs a test
// result that is wrong and looks right.
func scanRelocatable(dir string) (relocatableScanResult, error) {
	res := relocatableScanResult{Relocatable: true}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return relocatableScanResult{}, err
	}
	var walk func(cur string, depth int) error
	walk = func(cur string, depth int) error {
		if depth > linkSafeDeleteMaxDepth {
			res.Relocatable = false
			return nil
		}
		items, rerr := os.ReadDir(cur)
		if rerr != nil {
			return rerr
		}
		for _, item := range items {
			full := filepath.Join(cur, item.Name())
			info, infoErr := item.Info()
			if infoErr != nil {
				continue
			}
			if isWorktreeLink(info, full) {
				res.Links++
				target, lerr := os.Readlink(full)
				if lerr != nil {
					res.Relocatable = false
					continue
				}
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(full), target)
				}
				resolved := filepath.Clean(target)
				if r, eerr := filepath.EvalSymlinks(resolved); eerr == nil {
					resolved = r
				}
				if !isPathWithin(realDir, resolved) {
					res.Relocatable = false
					if len(res.Escaping) < maxBaselineDiffSample {
						res.Escaping = append(res.Escaping, filepath.ToSlash(full)+" -> "+filepath.ToSlash(resolved))
					}
				}
				continue
			}
			if info.IsDir() {
				if err := walk(full, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(dir, 0); err != nil {
		return relocatableScanResult{}, err
	}
	return res, nil
}

// lockfileDigestFor digests the lockfile (or manifest) a spec names, relative
// to the worktree. An absent lockfile yields "" with no error: the caller then
// records no digest and reconcile treats it as never ready.
func lockfileDigestFor(worktree, lockfileRel string) (string, error) {
	if strings.TrimSpace(lockfileRel) == "" {
		return "", nil
	}
	abs := filepath.Join(worktree, filepath.FromSlash(lockfileRel))
	if _, err := os.Stat(abs); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return fileSha256(abs)
}

// goosIsCaseInsensitive is here only so a test can read the same rule the
// path-identity key uses.
func goosIsCaseInsensitive() bool {
	return runtime.GOOS == "windows" || runtime.GOOS == "darwin"
}
