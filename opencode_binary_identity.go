// opencode_binary_identity.go — a content identity for what a Windows
// `opencode.cmd` npm shim actually launches.
//
// `npm install -g opencode-ai@latest` replaces the package under
// node_modules but leaves the shim it put on PATH byte-for-byte untouched —
// same bytes, same size, and often the same mtime. Every cache that keys on the
// shim's (path, mtime, size) therefore survives the upgrade: the `--version`
// cache keeps reporting the old version, and the smoke cooldown replays the
// pre-upgrade verdict for the post-upgrade smoke, which is exactly the run the
// maintenance harness needs a fresh answer for.
//
// The identity hashes the shim's own bytes plus the package.json of the
// `opencode-ai` package and of each `opencode-windows-*` platform package it may
// resolve, so any npm replacement changes it. Bounded by construction:
// at most openCodeIdentityMaxFiles files, openCodeIdentityMaxFileBytes each,
// only paths inside the shim's directory tree, regular files only. A native
// binary returns "":
// its own (path, mtime, size) already changes when it is replaced.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const (
	// openCodeIdentityMaxFiles bounds how many files one identity reads.
	openCodeIdentityMaxFiles = 8
	// openCodeIdentityMaxFileBytes bounds how much of each file is hashed. A
	// package.json is a few KiB; the shim is a few hundred bytes.
	openCodeIdentityMaxFileBytes = 64 << 10
	// openCodeIdentityPackageName is the npm package the shim launches.
	openCodeIdentityPackageName = "opencode-ai"
	// openCodeIdentityPlatformPrefix names the per-platform binary packages
	// opencode-ai installs (opencode-windows-x64, …-baseline, …-arm64). Only a
	// Windows shim is hashed, so only Windows platform packages can apply.
	openCodeIdentityPlatformPrefix = "opencode-windows-"
)

// openCodeIdentityMissing is hashed in place of a file that is absent or
// unreadable, so "no package" is a stable identity rather than an error.
var openCodeIdentityMissing = []byte("\x00aix-opencode-identity-missing\x00")

// openCodeBinaryIdentity returns the content identity of an OpenCode launch
// path: a SHA-256 over the shim and its package manifests for a Windows
// `.cmd` / `.bat` shim, "" for anything else. Registered as the opencode
// provider's identity hook in cliSmokeProviders.
func openCodeBinaryIdentity(path string) string {
	if path == "" || !isWindowsShimPath(path) {
		return ""
	}
	return openCodeShimPackageIdentity(path)
}

// openCodeShimPackageIdentity is the platform-neutral hash behind
// openCodeBinaryIdentity, split out so its bounds are testable on every OS.
func openCodeShimPackageIdentity(shimPath string) string {
	root := filepath.Dir(shimPath)
	h := sha256.New()
	for _, file := range openCodeShimIdentityFiles(shimPath) {
		// The name relative to the shim's directory is hashed with the content,
		// so a manifest moving between the nested and the sibling location is a
		// change, and two missing files cannot collide with one present one.
		rel, err := filepath.Rel(root, file)
		if err != nil {
			rel = filepath.Base(file)
		}
		_, _ = io.WriteString(h, filepath.ToSlash(rel))
		_, _ = h.Write([]byte{0})
		openCodeHashIdentityFile(h, file)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// openCodeShimIdentityFiles lists, in a stable order, the files the identity
// covers: the shim, the opencode-ai manifest, then each opencode-windows-* platform
// package manifest nested under opencode-ai or installed beside it. Capped at
// openCodeIdentityMaxFiles.
func openCodeShimIdentityFiles(shimPath string) []string {
	modules := filepath.Join(filepath.Dir(shimPath), "node_modules")
	pkg := filepath.Join(modules, openCodeIdentityPackageName)
	files := []string{shimPath, filepath.Join(pkg, "package.json")}
	for _, dir := range []string{filepath.Join(pkg, "node_modules"), modules} {
		// The platform-package prefix, not a bare `opencode-*`: npm's global
		// node_modules can hold unrelated `opencode-*` packages (plugins) that
		// would otherwise crowd the real platform package out of the file cap
		// and churn the identity whenever they update.
		matches, _ := filepath.Glob(filepath.Join(dir, openCodeIdentityPlatformPrefix+"*", "package.json"))
		sort.Strings(matches)
		for _, match := range matches {
			if len(files) >= openCodeIdentityMaxFiles {
				return files
			}
			files = append(files, match)
		}
	}
	return files
}

// openCodeHashIdentityFile writes at most openCodeIdentityMaxFileBytes of
// file into h, or the missing token when it is not a readable regular file.
// Only a regular file is opened: a manifest path that resolves (through an npm
// link) to a FIFO or device could otherwise block the version probe on open.
func openCodeHashIdentityFile(h io.Writer, file string) {
	if info, err := os.Stat(file); err != nil || !info.Mode().IsRegular() {
		_, _ = h.Write(openCodeIdentityMissing)
		return
	}
	f, err := os.Open(file)
	if err != nil {
		_, _ = h.Write(openCodeIdentityMissing)
		return
	}
	defer f.Close()
	if _, err := io.Copy(h, io.LimitReader(f, openCodeIdentityMaxFileBytes)); err != nil {
		_, _ = h.Write(openCodeIdentityMissing)
	}
}

var (
	openCodeVersionIdentityMu sync.Mutex
	// openCodeVersionIdentity is the shim identity each cached `--version`
	// reading was taken under, keyed by path.
	openCodeVersionIdentity = map[string]string{}
)

// openCodeForgetVersionOnIdentityChange drops the cached `--version` reading
// for a shim whose package contents changed since it was read. The shared
// version cache keys on the shim's (path, mtime, size) alone, which an npm
// upgrade leaves unchanged. The first sighting of a path only records it.
func openCodeForgetVersionOnIdentityChange(path string) {
	identity := openCodeBinaryIdentity(path)
	if identity == "" {
		return
	}
	openCodeVersionIdentityMu.Lock()
	previous, seen := openCodeVersionIdentity[path]
	openCodeVersionIdentity[path] = identity
	openCodeVersionIdentityMu.Unlock()
	if seen && previous != identity {
		forgetCachedProbeVersion(path)
	}
}
