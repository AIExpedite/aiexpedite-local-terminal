package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withTempGrokSessionStore redirects the persistent conversation store into a
// per-test temp dir so tests never touch the real agent config dir.
func withTempGrokSessionStore(t *testing.T) string {
	t.Helper()
	prev := grokSessionStoreRoot
	root := t.TempDir()
	grokSessionStoreRoot = root
	t.Cleanup(func() { grokSessionStoreRoot = prev })
	return filepath.Join(root, "grok-sessions")
}

// TestSetupIsolatedGrokHome_SessionStoreSurvivesHomeRemoval is the regression
// test for the defect that made grok resume impossible: the isolated GROK_HOME
// is deleted when the child exits, and grok keeps its conversation transcripts
// under GROK_HOME, so the next process found an empty store and every
// `session/load` failed with -32603 / FS_NOT_FOUND. Writing through the linked
// `sessions` entry and then removing the home must leave the transcript intact.
func TestSetupIsolatedGrokHome_SessionStoreSurvivesHomeRemoval(t *testing.T) {
	store := withTempGrokSessionStore(t)
	t.Setenv("GROK_HOME", t.TempDir())

	dir, err := setupIsolatedGrokHome(false, grokACPDefaultModel)
	if err != nil {
		t.Fatalf("setupIsolatedGrokHome: %v", err)
	}

	// Stand in for grok writing `sessions/<url-encoded-cwd>/<uuid>/updates.jsonl`.
	sessionDir := filepath.Join(dir, "sessions", "C%3A%5Crepo", "session-uuid")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("write through linked sessions dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "updates.jsonl"), []byte("turn"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	// waitForExit's teardown.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove isolated home: %v", err)
	}

	persisted := filepath.Join(store, "C%3A%5Crepo", "session-uuid", "updates.jsonl")
	body, err := os.ReadFile(persisted)
	if err != nil {
		t.Fatalf("transcript did not survive the isolated home: %v", err)
	}
	if string(body) != "turn" {
		t.Fatalf("persisted transcript = %q, want %q", body, "turn")
	}
}

// TestSetupIsolatedGrokHome_StillOmitsPersistedConfig pins the other half of
// the contract: persisting the transcript store must not start persisting
// anything ELSE across sessions. A plugin/hook/skill dropped into the isolated
// home by one session must be gone once that home is removed, so the next
// session cannot inherit it.
func TestSetupIsolatedGrokHome_StillOmitsPersistedConfig(t *testing.T) {
	store := withTempGrokSessionStore(t)
	t.Setenv("GROK_HOME", t.TempDir())

	first, err := setupIsolatedGrokHome(false, grokACPDefaultModel)
	if err != nil {
		t.Fatalf("setupIsolatedGrokHome: %v", err)
	}
	hookDir := filepath.Join(first, "installed-plugins")
	if err := os.MkdirAll(hookDir, 0o700); err != nil {
		t.Fatalf("seed plugin dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hookDir, "evil.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed plugin: %v", err)
	}
	if err := os.RemoveAll(first); err != nil {
		t.Fatalf("remove first home: %v", err)
	}

	second, err := setupIsolatedGrokHome(false, grokACPDefaultModel)
	if err != nil {
		t.Fatalf("setupIsolatedGrokHome (second): %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(second) })

	if _, err := os.Stat(filepath.Join(second, "installed-plugins", "evil.json")); !os.IsNotExist(err) {
		t.Fatalf("persisted config leaked into a later session (stat err = %v)", err)
	}
	if _, err := os.Stat(store); err != nil {
		t.Fatalf("session store should still exist: %v", err)
	}
}

func TestPruneGrokSessionStore_RemovesExpiredKeepsFresh(t *testing.T) {
	store := withTempGrokSessionStore(t)
	now := time.Now()

	seed := func(cwd, session string, age time.Duration) string {
		dir := filepath.Join(store, cwd, session)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("seed %s: %v", dir, err)
		}
		file := filepath.Join(dir, "updates.jsonl")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed transcript: %v", err)
		}
		stamp := now.Add(-age)
		if err := os.Chtimes(file, stamp, stamp); err != nil {
			t.Fatalf("chtimes file: %v", err)
		}
		if err := os.Chtimes(dir, stamp, stamp); err != nil {
			t.Fatalf("chtimes dir: %v", err)
		}
		return dir
	}

	expired := seed("C%3A%5Cold", "gone", 30*24*time.Hour)
	fresh := seed("C%3A%5Cnew", "kept", 2*time.Hour)

	removed, err := pruneGrokSessionStore(grokSessionStoreMaxAge, now)
	if err != nil {
		t.Fatalf("pruneGrokSessionStore: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Fatalf("expired session survived (stat err = %v)", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh session was pruned: %v", err)
	}
	// The emptied per-cwd directory goes too; the one still holding a session stays.
	if _, err := os.Stat(filepath.Join(store, "C%3A%5Cold")); !os.IsNotExist(err) {
		t.Fatalf("empty cwd dir survived (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(store, "C%3A%5Cnew")); err != nil {
		t.Fatalf("populated cwd dir was removed: %v", err)
	}
}

// A session directory whose own mtime is stale but which holds a freshly
// written transcript is LIVE — grok appends to updates.jsonl inside it, and on
// some filesystems that does not update the parent directory's timestamp.
// Pruning it would delete a running conversation.
func TestPruneGrokSessionStore_KeepsSessionWithFreshChildWrite(t *testing.T) {
	store := withTempGrokSessionStore(t)
	now := time.Now()

	dir := filepath.Join(store, "C%3A%5Crepo", "live")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("seed: %v", err)
	}
	file := filepath.Join(dir, "updates.jsonl")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed transcript: %v", err)
	}
	stale := now.Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(dir, stale, stale); err != nil {
		t.Fatalf("chtimes dir: %v", err)
	}

	removed, err := pruneGrokSessionStore(grokSessionStoreMaxAge, now)
	if err != nil {
		t.Fatalf("pruneGrokSessionStore: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0 (fresh child write)", removed)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("live session was pruned: %v", err)
	}
}

func TestPruneGrokSessionStore_LeavesIndexFileAndMissingStore(t *testing.T) {
	store := withTempGrokSessionStore(t)
	now := time.Now()

	// Missing store is a no-op, not an error.
	removed, err := pruneGrokSessionStore(grokSessionStoreMaxAge, now)
	if err != nil {
		t.Fatalf("missing store should be a no-op: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d on missing store, want 0", removed)
	}

	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatalf("mkdir store: %v", err)
	}
	// Grok's own cross-session search index lives beside the per-cwd dirs.
	index := filepath.Join(store, "session_search.sqlite")
	if err := os.WriteFile(index, []byte("db"), 0o600); err != nil {
		t.Fatalf("seed index: %v", err)
	}
	stale := now.Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(index, stale, stale); err != nil {
		t.Fatalf("chtimes index: %v", err)
	}

	if _, err := pruneGrokSessionStore(grokSessionStoreMaxAge, now); err != nil {
		t.Fatalf("pruneGrokSessionStore: %v", err)
	}
	if _, err := os.Stat(index); err != nil {
		t.Fatalf("search index must not be pruned: %v", err)
	}
}
