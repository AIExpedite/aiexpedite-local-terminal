package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

const (
	grokJunctionLinkEnv   = "AIEXPEDITE_GROK_JUNCTION_LINK"
	grokJunctionTargetEnv = "AIEXPEDITE_GROK_JUNCTION_TARGET"
)

// Grok persists every conversation to disk under
// `$GROK_HOME/sessions/<url-encoded-cwd>/<session-uuid>/`, and ACP
// `session/load` resolves a conversation by reading that directory. There is
// no separate knob for the session path: GROK_HOME is the only lever xAI
// exposes, so the conversation store necessarily lives wherever GROK_HOME
// points.
//
// That collides head-on with the per-session isolated GROK_HOME
// (setupIsolatedGrokHome): every ACP session got a fresh temp dir which
// waitForExit then deleted, taking the conversation store with it. The next
// process therefore started with an EMPTY store, so a cross-session
// `session/load` could never succeed — it failed, every time, with
//
//	{"code":-32603,"message":"Path not found.",
//	 "data":{"code":"FS_NOT_FOUND","detail":"The system cannot find the path specified. (os error 3)"}}
//
// which is grok stat-ing a `sessions/<cwd>` directory that this process never
// had. Conversation-scoped resume was structurally impossible, not flaky: the
// orchestrator would park the run and retry the reattach forever against a
// transcript that had already been deleted on disk.
//
// The fix keeps the isolation intact and persists ONLY the conversation store:
// the per-session temp home stays ephemeral, and its `sessions` entry is a
// symlink (junction on Windows) into one per-device store under the agent's
// own config dir. Everything else grok persists into GROK_HOME — plugins,
// skills, hooks, requirements, its own config — is still discarded when the
// session ends, so setupIsolatedGrokHome's "neutralise every persisted-config
// vector by omission" property is unchanged. The one thing that now outlives a
// session is the transcript, which is exactly what resume needs.
//
// Note the deliberate consequence: transcripts of earlier grok sessions on
// this device remain readable by later ones (they share an OS user and a grok
// account, as they already do for interactive `grok --resume`). Resume cannot
// work without that, and the store is per-device, never shared off-machine.

// grokSessionStoreMaxAge bounds the store: a session directory untouched for
// longer than this is pruned. Resumes happen within hours (an execution parks
// and comes back), so two weeks is far past the useful window while keeping
// the on-disk footprint from growing without limit — xAI documents no
// retention policy of its own, so the agent has to impose one.
const grokSessionStoreMaxAge = 14 * 24 * time.Hour

// grokSessionStoreRoot overrides the parent of the persistent store. Empty
// means GetConfigDir() (the agent's per-env config dir). Tests set it.
var grokSessionStoreRoot string

var grokSessionStorePruneOnce sync.Once

// grokSessionStoreDir returns the per-device directory that backs every ACP
// session's `sessions` entry.
func grokSessionStoreDir() string {
	root := grokSessionStoreRoot
	if root == "" {
		root = GetConfigDir()
	}
	return filepath.Join(root, "grok-sessions")
}

// linkGrokSessionStore points `<home>/sessions` at the persistent per-device
// store so conversations survive the process that created them.
//
// Failure is NOT fatal to the caller: a session that runs with an ephemeral
// store still works for everything except reattach, which is strictly better
// than refusing to start the CLI at all. The caller logs and continues.
func linkGrokSessionStore(home string) error {
	store := grokSessionStoreDir()
	if err := os.MkdirAll(store, 0o700); err != nil {
		return fmt.Errorf("create grok session store: %w", err)
	}
	return linkGrokDirectory(filepath.Join(home, "sessions"), store, "session store")
}

// linkGrokDirectory creates a directory link on every supported platform. The
// persistent conversation store uses it to stay available from a
// security-isolated GROK_HOME; billing logs deliberately remain session-local
// until an account-bound normalized snapshot is persisted after process exit.
func linkGrokDirectory(link, target, description string) error {
	// The isolated home is freshly created, so the entry should not exist;
	// clear a stray one rather than failing the link. RemoveAll does not follow
	// a junction/symlink — it unlinks it — so this cannot reach the target.
	if _, err := os.Lstat(link); err == nil {
		if rerr := os.RemoveAll(link); rerr != nil {
			return fmt.Errorf("clear stale %s entry: %w", description, rerr)
		}
	}

	if runtime.GOOS == "windows" {
		// Directory junctions need no special privilege, unlike symlinks,
		// which require SeCreateSymbolicLinkPrivilege (admin or Developer
		// Mode) and would therefore fail on an ordinary user's machine. Go
		// has no junction API, so shell out to mklink. os.Symlink is kept as
		// a fallback for the machines where the privilege IS held.
		out, err := grokWindowsJunctionCommand(link, target).CombinedOutput()
		if err == nil {
			return nil
		}
		if serr := os.Symlink(target, link); serr != nil {
			return fmt.Errorf("junction grok %s (mklink: %v, output: %s): %w",
				description, err, string(out), serr)
		}
		return nil
	}

	if err := os.Symlink(target, link); err != nil {
		return fmt.Errorf("symlink grok %s: %w", description, err)
	}
	return nil
}

// grokWindowsJunctionCommand keeps filesystem paths out of cmd.exe's command
// tokens. GROK_HOME is user-controlled and valid Windows paths may contain
// metacharacters such as `&`; interpolating one into `cmd /c mklink` could run
// unintended commands. Environment expansion inside quotes makes those paths
// data, while /d and /v:off disable AutoRun and delayed `!` expansion.
func grokWindowsJunctionCommand(link, target string) *exec.Cmd {
	cmd := exec.Command(
		"cmd", "/d", "/v:off", "/c",
		`mklink /J "%`+grokJunctionLinkEnv+`%" "%`+grokJunctionTargetEnv+`%"`,
	)
	env := setEnvVar(os.Environ(), grokJunctionLinkEnv, link)
	cmd.Env = setEnvVar(env, grokJunctionTargetEnv, target)
	return cmd
}

// pruneGrokSessionStoreOnce prunes the store at most once per agent process,
// in the background. Called from the session-start path rather than agent
// startup so a device that never runs grok never touches the directory.
func pruneGrokSessionStoreOnce() {
	grokSessionStorePruneOnce.Do(func() {
		// Resolve the store path on THIS goroutine, not inside the one below.
		// grokSessionStoreRoot is a plain global that tests swap and restore
		// via t.Cleanup, so a background read of it races the restore and
		// trips -race in an unrelated test.
		store := grokSessionStoreDir()
		go func() {
			removed, err := pruneGrokSessionStoreAt(store, grokSessionStoreMaxAge, time.Now())
			if err != nil {
				fmt.Printf("%s[grok-acp] session store prune failed: %v%s\n", colorYellow, err, colorReset)
				return
			}
			if removed > 0 {
				fmt.Printf("%s[grok-acp] pruned %d expired grok session(s) from the local store%s\n",
					colorCyan, removed, colorReset)
			}
		}()
	})
}

// pruneGrokSessionStore removes session directories whose most recent write is
// older than maxAge, then drops any per-cwd directory left empty. Returns the
// number of session directories removed.
//
// Recency is the NEWEST mtime among a session directory's entries, not the
// directory's own: grok appends to `updates.jsonl` inside it, and on some
// filesystems that does not bubble up to the parent's timestamp. Reading the
// children keeps a long-running or recently-resumed conversation from being
// deleted out from under a live session.
func pruneGrokSessionStore(maxAge time.Duration, now time.Time) (int, error) {
	return pruneGrokSessionStoreAt(grokSessionStoreDir(), maxAge, now)
}

// pruneGrokSessionStoreAt is pruneGrokSessionStore against an already-resolved
// store directory, so callers running off-goroutine never read the global.
func pruneGrokSessionStoreAt(store string, maxAge time.Duration, now time.Time) (int, error) {
	cwdDirs, err := os.ReadDir(store)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	cutoff := now.Add(-maxAge)
	removed := 0
	for _, cwdDir := range cwdDirs {
		if !cwdDir.IsDir() {
			// e.g. grok's own `session_search.sqlite` index — leave it alone.
			continue
		}
		cwdPath := filepath.Join(store, cwdDir.Name())
		sessionDirs, rerr := os.ReadDir(cwdPath)
		if rerr != nil {
			continue
		}
		liveSessions := 0
		for _, sessionDir := range sessionDirs {
			if !sessionDir.IsDir() {
				continue
			}
			sessionPath := filepath.Join(cwdPath, sessionDir.Name())
			if newestModTime(sessionPath).After(cutoff) {
				liveSessions++
				continue
			}
			if os.RemoveAll(sessionPath) == nil {
				removed++
			} else {
				liveSessions++
			}
		}
		if liveSessions == 0 {
			// Best-effort: only succeeds when nothing else is left behind.
			_ = os.Remove(cwdPath)
		}
	}
	return removed, nil
}

// newestModTime returns the most recent mtime of dir or anything directly
// inside it, falling back to the zero time when dir cannot be read (which
// makes the caller treat it as expired).
func newestModTime(dir string) time.Time {
	info, err := os.Stat(dir)
	if err != nil {
		return time.Time{}
	}
	newest := info.ModTime()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return newest
	}
	for _, entry := range entries {
		entryInfo, ierr := entry.Info()
		if ierr != nil {
			continue
		}
		if entryInfo.ModTime().After(newest) {
			newest = entryInfo.ModTime()
		}
	}
	return newest
}
