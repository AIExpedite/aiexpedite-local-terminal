// File: path_refresh.go
// -----------------------------------------------------------------------------
// Re-reads PATH before a command runs, so a tool an earlier setup step just
// installed is visible to the next step in the same run.
//
// An installer cannot change the environment of a process that is already
// running. WinGet, the Node MSI and friends write the new directory to the
// persisted machine / user PATH in the registry; Homebrew and npm drop binaries
// into directories a launchd- or GUI-started agent never had on PATH. Without a
// refresh, "install Node, then `npm ci`" fails in the same setup run with
// "npm is not recognized" until the agent restarts
// (COMPUTER_SETUP_CHECKLIST_PLAN.md §1, last row).
//
// refreshCommandPath rewrites THIS process's PATH (os.Setenv), so every child
// spawned afterwards — the one-shot PowerShell / bash transports, sessions,
// probes, exec.LookPath — sees it. The persistent PowerShell fixed its PATH at
// spawn time; PersistentPowerShell.Execute re-syncs it when this process's PATH
// changed (powershell_windows.go).
//
// Merge rules (pure, unit-tested on every OS):
//   - Windows: the agent's own PATH entries first, then the registry machine
//     Path, then the user Path (each already %VAR%-expanded), first occurrence
//     wins, compared case-insensitively and ignoring a trailing separator.
//   - macOS / Linux: prepend /opt/homebrew/bin, /usr/local/bin, ~/.local/bin and
//     the npm global prefix's bin when they exist and are not already present.
//     Entries already on PATH keep their position, so the user's explicit order
//     is never rearranged.
// -----------------------------------------------------------------------------

package main

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// persistedPathSource reads the persisted PATH values an installer writes.
// Windows implements it over the registry (path_refresh_windows.go); tests use a
// fake so the merge is exercised on every OS.
type persistedPathSource interface {
	// MachinePath is HKLM\SYSTEM\CurrentControlSet\Control\Session
	// Manager\Environment\Path, %VAR%-expanded; "" when absent.
	MachinePath() string
	// UserPath is HKCU\Environment\Path, %VAR%-expanded; "" when absent.
	UserPath() string
}

// splitPathList splits a PATH-style list on sep, trimming whitespace and
// dropping empty entries.
func splitPathList(list, sep string) []string {
	if list == "" {
		return nil
	}
	out := make([]string, 0)
	for _, e := range strings.Split(list, sep) {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// pathEntryKey is the dedupe key of one PATH entry: trailing separators removed
// (`C:\Tools\` and `C:\Tools` are one directory) and, when caseInsensitive,
// lower-cased (Windows).
func pathEntryKey(entry string, caseInsensitive bool) string {
	k := strings.TrimRight(entry, `/\`)
	if k == "" {
		// A bare root ("/" or "\") trims to empty; keep it distinct.
		k = entry
	}
	if caseInsensitive {
		k = strings.ToLower(k)
	}
	return k
}

// mergePathEntries concatenates the lists in order, keeping the first
// occurrence of every directory. Empty entries are dropped.
func mergePathEntries(caseInsensitive bool, lists ...[]string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, list := range lists {
		for _, e := range list {
			e = strings.TrimSpace(e)
			if e == "" {
				continue
			}
			k := pathEntryKey(e, caseInsensitive)
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, e)
		}
	}
	return out
}

// windowsRefreshedPath merges the agent's current PATH with the persisted
// machine and user PATH: own entries first, first occurrence wins,
// case-insensitive. A nil source leaves the list unchanged (deduped).
func windowsRefreshedPath(current string, src persistedPathSource) string {
	const sep = ";"
	own := splitPathList(current, sep)
	var machine, user []string
	if src != nil {
		machine = splitPathList(src.MachinePath(), sep)
		user = splitPathList(src.UserPath(), sep)
	}
	return strings.Join(mergePathEntries(true, own, machine, user), sep)
}

// unixRefreshedPath prepends every candidate directory that exists and is not
// already on PATH (in candidate order), then the current PATH unchanged
// (deduped, case-sensitive).
func unixRefreshedPath(current string, candidates []string, dirExists func(string) bool) string {
	const sep = ":"
	own := splitPathList(current, sep)
	present := make(map[string]struct{}, len(own))
	for _, e := range own {
		present[pathEntryKey(e, false)] = struct{}{}
	}
	var prepend []string
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, ok := present[pathEntryKey(c, false)]; ok {
			continue
		}
		if dirExists != nil && !dirExists(c) {
			continue
		}
		prepend = append(prepend, c)
	}
	return strings.Join(mergePathEntries(false, prepend, own), sep)
}

// unixPathCandidates lists the common per-machine and per-user bin directories
// macOS / Linux installers write to, most specific install location first.
// npmPrefix is the npm global prefix ("" when unknown); its bin directory is
// where `npm install -g` puts CLIs such as codex and firebase.
func unixPathCandidates(home, npmPrefix string) []string {
	out := []string{"/opt/homebrew/bin", "/usr/local/bin"}
	if home != "" {
		out = append(out, filepath.Join(home, ".local", "bin"))
	}
	if npmPrefix != "" {
		out = append(out, filepath.Join(npmPrefix, "bin"))
	}
	return out
}

// npmGlobalPrefix returns the npm global prefix without spawning npm (a Node
// start is hundreds of milliseconds, far too slow to pay before every command):
// $NPM_CONFIG_PREFIX / $npm_config_prefix, else a `prefix=` line in ~/.npmrc.
// "" means npm's default prefix — the Node install's own directory, whose bin is
// already on PATH whenever `node` is.
func npmGlobalPrefix(getenv func(string) string, home string, readFile func(string) ([]byte, error)) string {
	for _, k := range []string{"NPM_CONFIG_PREFIX", "npm_config_prefix"} {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return expandHomePrefix(v, home)
		}
	}
	if home == "" || readFile == nil {
		return ""
	}
	raw, err := readFile(filepath.Join(home, ".npmrc"))
	if err != nil {
		return ""
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "prefix" {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if value != "" {
			return expandHomePrefix(value, home)
		}
	}
	return ""
}

// expandHomePrefix resolves the `~/…` and `${HOME}/…` spellings npm accepts in
// a prefix value.
func expandHomePrefix(v, home string) string {
	if home == "" {
		return v
	}
	for _, p := range []string{"~/", "${HOME}/", "$HOME/"} {
		if strings.HasPrefix(v, p) {
			return filepath.Join(home, v[len(p):])
		}
	}
	if v == "~" {
		return home
	}
	return v
}

// pathRefreshMu serializes refreshes so two concurrent commands cannot
// interleave a read-merge-write of PATH.
var pathRefreshMu sync.Mutex

// newPersistedPathSource returns the platform's persisted-PATH reader, nil on
// platforms without one. Overridable in tests.
var newPersistedPathSource = platformPersistedPathSource

// refreshCommandPath recomputes this process's PATH from its current value plus
// the platform's install locations and applies it when it changed. Cheap: two
// registry reads on Windows, a handful of stat calls elsewhere, no child
// processes. Safe to call before every command.
func refreshCommandPath() {
	pathRefreshMu.Lock()
	defer pathRefreshMu.Unlock()

	current := os.Getenv("PATH")
	var next string
	if runtime.GOOS == "windows" {
		next = windowsRefreshedPath(current, newPersistedPathSource())
	} else {
		home, _ := os.UserHomeDir()
		prefix := npmGlobalPrefix(os.Getenv, home, os.ReadFile)
		next = unixRefreshedPath(current, unixPathCandidates(home, prefix), isExistingDir)
	}
	if next != "" && next != current {
		_ = os.Setenv("PATH", next)
	}
}

// persistentPSPathAssignment is the PowerShell statement that sets the
// persistent shell's $env:Path to path, as a single-quoted literal (nothing in
// it is expanded). PowerShell also treats the typographic single quotes as quote
// characters, so every quote-like rune is doubled, not just the ASCII one.
func persistentPSPathAssignment(path string) string {
	var b strings.Builder
	b.WriteString("$env:Path = '")
	for _, r := range path {
		switch r {
		case '\'', '‘', '’', '‚', '‛':
			b.WriteRune(r)
			b.WriteRune(r)
		case '\r', '\n':
			// Never part of a real PATH; dropping them keeps the statement on one line.
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString("'\n")
	return b.String()
}

func isExistingDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}
