// File: env_verify_deps.go
// -----------------------------------------------------------------------------
// __env_verify_deps__ — decide, without installing anything, whether a
// checkout's node_modules is exactly what its package-lock.json describes
// (COMPUTER_SETUP_CHECKLIST_PLAN.md §7 step 6).
//
// `npm ls` is not used: it checks the tree against package.json RANGES, so an
// old install passes after a lockfile-only update. Instead this compares the
// lockfile with npm's hidden lockfile, node_modules/.package-lock.json, which
// npm rewrites on every install:
//
//	normalization (both sides)
//	  - drop the root project (packages[""]): the hidden lockfile never has it;
//	  - drop optional entries whose os / cpu / libc exclude this device: npm
//	    does not install them here;
//	  - `link: true` entries compare by `resolved` (the link target);
//	  - every other node_modules/… entry compares `version` + `integrity`
//	    (and `resolved` when neither side has an integrity — a git or file
//	    dependency, whose version alone does not move with its commit).
//
//	representativeness (npm's own rule for trusting a hidden lockfile,
//	arborist shrinkwrap.js assertNoNewer)
//	  - every package folder it lists exists;
//	  - no package folder exists that it does not list (top level, scoped
//	    @x/y, and nested node_modules alike);
//	  - no package folder (nor node_modules / @scope dir) is newer than it.
//
// Match only when both hold. Anything else is "unproven" and the server
// reinstalls; this never reports a match it cannot prove.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	verifyReasonNoLockfile         = "no_lockfile"
	verifyReasonNoHiddenLockfile   = "no_hidden_lockfile"
	verifyReasonEntriesDiffer      = "entries_differ"
	verifyReasonFolderMissing      = "folder_missing"
	verifyReasonUnlistedFolder     = "unlisted_folder"
	verifyReasonHiddenStale        = "hidden_stale"
	verifyReasonUnsupportedManager = "unsupported_manager"

	maxVerifyMismatches = 10
	// maxLockfileBytes bounds what is read into memory (the largest real
	// lockfiles are a few tens of MB).
	maxLockfileBytes = 256 << 20
)

type envVerifyDepsRequest struct {
	Path    string `json:"path"`
	Manager string `json:"manager"`
}

type envVerifyDepsResult struct {
	LockfilePresent       bool     `json:"lockfilePresent"`
	LockfileSha256        string   `json:"lockfileSha256"`
	HiddenLockfilePresent bool     `json:"hiddenLockfilePresent"`
	Match                 bool     `json:"match"`
	Reason                string   `json:"reason,omitempty"`
	Mismatches            []string `json:"mismatches"`
}

// npmPlatform is the device as npm's platform checks see it
// (npm-install-checks checkPlatform).
type npmPlatform struct {
	OS   string // process.platform: win32 | darwin | linux | …
	CPU  string // process.arch: x64 | arm64 | ia32 | arm | …
	Libc string // "glibc" | "musl" on Linux; "" elsewhere or unknown
}

func currentNpmPlatform() npmPlatform {
	p := npmPlatform{OS: runtime.GOOS, CPU: runtime.GOARCH}
	switch runtime.GOOS {
	case "windows":
		p.OS = "win32"
	}
	switch runtime.GOARCH {
	case "amd64":
		p.CPU = "x64"
	case "386":
		p.CPU = "ia32"
	}
	if runtime.GOOS == "linux" {
		p.Libc = detectLinuxLibc()
	}
	return p
}

// detectLinuxLibc: musl systems ship /lib/ld-musl-<arch>.so.1; everything else
// with a dynamic loader is treated as glibc.
func detectLinuxLibc() string {
	if matches, _ := filepath.Glob("/lib/ld-musl-*"); len(matches) > 0 {
		return "musl"
	}
	return "glibc"
}

// lockEntry is one packages[...] entry of a v2/v3 lockfile.
type lockEntry struct {
	Version     string          `json:"version"`
	Resolved    string          `json:"resolved"`
	Integrity   string          `json:"integrity"`
	Link        bool            `json:"link"`
	Optional    bool            `json:"optional"`
	DevOptional bool            `json:"devOptional"`
	OS          json.RawMessage `json:"os"`
	CPU         json.RawMessage `json:"cpu"`
	Libc        json.RawMessage `json:"libc"`
}

type lockfileDoc struct {
	LockfileVersion int                  `json:"lockfileVersion"`
	Packages        map[string]lockEntry `json:"packages"`
}

// stringList decodes an os / cpu / libc field, which npm accepts as a string
// or an array of strings.
func stringList(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var one string
	if json.Unmarshal(raw, &one) == nil && one != "" {
		return []string{one}
	}
	return nil
}

// npmCheckList mirrors npm-install-checks' checkList: "any" matches; a value
// matches when it hits no negated ("!x") entry and at least one plain entry, or
// when every entry is negated.
func npmCheckList(value string, list []string) bool {
	if len(list) == 1 && list[0] == "any" {
		return true
	}
	negated := 0
	match := false
	for _, entry := range list {
		if strings.HasPrefix(entry, "!") {
			negated++
			if value == entry[1:] {
				return false
			}
		} else if value == entry {
			match = true
		}
	}
	return match || negated == len(list)
}

// npmPlatformOK mirrors npm-install-checks' checkPlatform for one entry.
func npmPlatformOK(e lockEntry, p npmPlatform) bool {
	if os := stringList(e.OS); os != nil && !npmCheckList(p.OS, os) {
		return false
	}
	if cpu := stringList(e.CPU); cpu != nil && !npmCheckList(p.CPU, cpu) {
		return false
	}
	if libc := stringList(e.Libc); libc != nil {
		// libc checks only work on Linux; npm fails any libc constraint elsewhere
		// or when the family is unknown.
		if p.OS != "linux" || p.Libc == "" || !npmCheckList(p.Libc, libc) {
			return false
		}
	}
	return true
}

// isNodeModulesKey reports whether a packages key is an installed package
// location (as opposed to a workspace folder like "packages/foo").
func isNodeModulesKey(k string) bool {
	return strings.HasPrefix(k, "node_modules/") || strings.Contains(k, "/node_modules/")
}

// normalizeLockPackages applies the comparison normalization to one side.
func normalizeLockPackages(pkgs map[string]lockEntry, p npmPlatform) map[string]lockEntry {
	out := make(map[string]lockEntry, len(pkgs))
	for k, e := range pkgs {
		if k == "" || !isNodeModulesKey(k) {
			continue
		}
		if (e.Optional || e.DevOptional) && !npmPlatformOK(e, p) {
			continue
		}
		out[k] = e
	}
	return out
}

// compareLockEntries returns human-readable differences between the
// normalized lockfile (want) and hidden lockfile (have), sorted.
func compareLockEntries(want, have map[string]lockEntry) []string {
	var diffs []string
	for k, w := range want {
		h, ok := have[k]
		if !ok {
			diffs = append(diffs, k+": not installed")
			continue
		}
		switch {
		case w.Link || h.Link:
			if w.Link != h.Link {
				diffs = append(diffs, k+": link differs")
			} else if w.Resolved != h.Resolved {
				diffs = append(diffs, fmt.Sprintf("%s: link target %s != %s", k, h.Resolved, w.Resolved))
			}
		case w.Version != h.Version:
			diffs = append(diffs, fmt.Sprintf("%s: version %s != %s", k, h.Version, w.Version))
		case w.Integrity != h.Integrity:
			diffs = append(diffs, k+": integrity differs")
		case w.Integrity == "" && w.Resolved != h.Resolved:
			diffs = append(diffs, k+": resolved differs")
		}
	}
	for k := range have {
		if _, ok := want[k]; !ok {
			diffs = append(diffs, k+": installed but not in package-lock.json")
		}
	}
	sort.Strings(diffs)
	return diffs
}

func capMismatches(list []string) []string {
	if len(list) > maxVerifyMismatches {
		return list[:maxVerifyMismatches]
	}
	if list == nil {
		return []string{}
	}
	return list
}

// verifyDependencies implements __env_verify_deps__ for one checkout.
func verifyDependencies(ctx context.Context, req envVerifyDepsRequest, platform npmPlatform) (envVerifyDepsResult, error) {
	res := envVerifyDepsResult{Mismatches: []string{}}
	if !strings.EqualFold(strings.TrimSpace(req.Manager), "npm") {
		res.Reason = verifyReasonUnsupportedManager
		return res, nil
	}
	root := strings.TrimSpace(req.Path)
	if root == "" || !filepath.IsAbs(root) {
		return res, errors.New("path must be an absolute directory")
	}
	root = filepath.Clean(root)
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return res, fmt.Errorf("path is not a directory: %s", root)
	}

	lockBytes, err := readBoundedFile(filepath.Join(root, "package-lock.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_, hiddenErr := os.Stat(filepath.Join(root, "node_modules", ".package-lock.json"))
			res.HiddenLockfilePresent = hiddenErr == nil
			res.Reason = verifyReasonNoLockfile
			return res, nil
		}
		return res, fmt.Errorf("read package-lock.json: %w", err)
	}
	res.LockfilePresent = true
	sum := sha256.Sum256(lockBytes)
	res.LockfileSha256 = hex.EncodeToString(sum[:])

	hiddenPath := filepath.Join(root, "node_modules", ".package-lock.json")
	hiddenInfo, err := os.Stat(hiddenPath)
	if err != nil {
		res.Reason = verifyReasonNoHiddenLockfile
		return res, nil
	}
	res.HiddenLockfilePresent = true
	hiddenBytes, err := readBoundedFile(hiddenPath)
	if err != nil {
		res.Reason = verifyReasonNoHiddenLockfile
		res.Mismatches = []string{"node_modules/.package-lock.json: unreadable"}
		return res, nil
	}

	var lock, hidden lockfileDoc
	if err := json.Unmarshal(lockBytes, &lock); err != nil {
		res.Reason = verifyReasonEntriesDiffer
		res.Mismatches = []string{"package-lock.json: invalid JSON"}
		return res, nil
	}
	if lock.Packages == nil {
		// lockfileVersion 1 has no packages map; npm 7+ upgrades it on install.
		res.Reason = verifyReasonEntriesDiffer
		res.Mismatches = []string{fmt.Sprintf("package-lock.json: lockfileVersion %d has no packages map", lock.LockfileVersion)}
		return res, nil
	}
	if err := json.Unmarshal(hiddenBytes, &hidden); err != nil || hidden.Packages == nil {
		res.Reason = verifyReasonEntriesDiffer
		res.Mismatches = []string{"node_modules/.package-lock.json: invalid"}
		return res, nil
	}

	if diffs := compareLockEntries(
		normalizeLockPackages(lock.Packages, platform),
		normalizeLockPackages(hidden.Packages, platform),
	); len(diffs) > 0 {
		res.Reason = verifyReasonEntriesDiffer
		res.Mismatches = capMismatches(diffs)
		return res, nil
	}

	rep, err := checkHiddenLockfileRepresentative(ctx, root, hidden.Packages, hiddenInfo.ModTime())
	if err != nil {
		return res, err
	}
	if rep.reason != "" {
		res.Reason = rep.reason
		res.Mismatches = capMismatches(rep.details)
		return res, nil
	}
	res.Match = true
	return res, nil
}

func readBoundedFile(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxLockfileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxLockfileBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", filepath.Base(p), maxLockfileBytes)
	}
	return b, nil
}

type representativeness struct {
	reason  string
	details []string
}

// checkHiddenLockfileRepresentative walks node_modules the way npm does before
// it trusts a hidden lockfile (arborist shrinkwrap.js assertNoNewer):
//
//   - node_modules itself, every @scope directory and every package folder
//     must not be newer than the hidden lockfile (strictly: npm's
//     `dirTime > lockTime`);
//   - every package folder must be listed in it;
//   - a package folder is descended into only through its own nested
//     node_modules (whose entries are read directly, as npm does);
//   - a link is recorded as seen and its target folder checked the same way;
//   - finally every listed location must have been seen on disk.
//
// A directory's mtime moves when an entry is added to or removed from it — a
// package copied in or deleted by hand — which is exactly "changed without npm".
func checkHiddenLockfileRepresentative(ctx context.Context, root string, packages map[string]lockEntry, lockTime time.Time) (representativeness, error) {
	var (
		missing, unlisted, stale []string
		seen                     = map[string]bool{}
	)
	// A link target may come back in the filesystem's canonical spelling (a
	// resolved temp dir, an 8.3 name), so a path outside root is also tried
	// against root's own resolved form.
	realRoot, _ := filepath.EvalSymlinks(root)
	rel := func(p string) string {
		r, err := filepath.Rel(root, p)
		if (err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator))) && realRoot != "" {
			if r2, err2 := filepath.Rel(realRoot, p); err2 == nil && !strings.HasPrefix(r2, "..") {
				r, err = r2, nil
			}
		}
		if err != nil {
			return filepath.ToSlash(p)
		}
		return filepath.ToSlash(r)
	}

	const (
		kindNodeModules = iota // a node_modules directory (the top-level one)
		kindScope              // an @scope directory
		kindPackage            // a package folder
	)
	var walk func(dir string, kind int) error
	walk = func(dir string, kind int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		r := rel(dir)
		seen[r] = true
		info, err := os.Stat(dir)
		if err != nil {
			return nil
		}
		if info.ModTime().After(lockTime) {
			stale = append(stale, r+": modified after the hidden lockfile")
		}
		listDir := dir
		if kind == kindPackage {
			if _, listed := packages[r]; !listed {
				unlisted = append(unlisted, r+": not in the hidden lockfile")
			}
			listDir = filepath.Join(dir, "node_modules")
		}
		entries, err := os.ReadDir(listDir)
		if err != nil {
			return nil // no nested node_modules, or unreadable
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") {
				continue // .bin, .package-lock.json, .cache …
			}
			child := filepath.Join(listDir, name)
			if isLinkEntry(e, child) {
				seen[rel(child)] = true
				target, err := resolveDirLink(child)
				if err != nil {
					continue // dangling: a listed target shows up as missing below
				}
				if tinfo, err := os.Stat(target); err != nil || !tinfo.IsDir() || seen[rel(target)] {
					continue
				}
				if err := walk(target, kindPackage); err != nil {
					return err
				}
				continue
			}
			if !e.IsDir() {
				continue
			}
			childKind := kindPackage
			if kind != kindScope && strings.HasPrefix(name, "@") {
				childKind = kindScope
			}
			if err := walk(child, childKind); err != nil {
				return err
			}
		}
		return nil
	}

	if err := walk(filepath.Join(root, "node_modules"), kindNodeModules); err != nil {
		return representativeness{}, err
	}
	for k := range packages {
		if k != "" && !seen[k] {
			missing = append(missing, k+": listed but not on disk")
		}
	}
	sort.Strings(missing)
	sort.Strings(unlisted)
	sort.Strings(stale)
	switch {
	case len(missing) > 0:
		return representativeness{reason: verifyReasonFolderMissing, details: missing}, nil
	case len(unlisted) > 0:
		return representativeness{reason: verifyReasonUnlistedFolder, details: unlisted}, nil
	case len(stale) > 0:
		return representativeness{reason: verifyReasonHiddenStale, details: stale}, nil
	}
	return representativeness{}, nil
}

// resolveDirLink returns a link's target the way npm does
// (path.resolve(dirname(link), readlink(link))), falling back to full
// resolution when the link cannot be read directly.
func resolveDirLink(link string) (string, error) {
	if t, err := os.Readlink(link); err == nil && t != "" {
		if !filepath.IsAbs(t) {
			t = filepath.Join(filepath.Dir(link), t)
		}
		return filepath.Clean(t), nil
	}
	return filepath.EvalSymlinks(link)
}

// isLinkEntry reports whether a directory entry is a symlink or, on Windows, a
// junction (npm links workspaces with junctions there; since Go 1.23 a junction
// is not reported as ModeSymlink, so readlink decides).
func isLinkEntry(e os.DirEntry, full string) bool {
	if e.Type()&os.ModeSymlink != 0 {
		return true
	}
	if runtime.GOOS == "windows" && (e.IsDir() || e.Type()&os.ModeIrregular != 0) {
		if _, err := os.Readlink(full); err == nil {
			return true
		}
	}
	return false
}
