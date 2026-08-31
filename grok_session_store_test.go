package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGrokWindowsJunctionCommand_DoesNotInterpolatePaths(t *testing.T) {
	link := `C:\temp\grok&(link)`
	target := `C:\Users\name\grok%target%!`
	cmd := grokWindowsJunctionCommand(link, target)
	commandLine := strings.Join(cmd.Args, " ")
	if strings.Contains(commandLine, link) || strings.Contains(commandLine, target) {
		t.Fatalf("junction paths leaked into cmd.exe tokens: %q", commandLine)
	}
	for _, want := range []string{"/d", "/v:off", `%` + grokJunctionLinkEnv + `%`, `%` + grokJunctionTargetEnv + `%`} {
		if !strings.Contains(commandLine, want) {
			t.Fatalf("junction command %q missing %q", commandLine, want)
		}
	}

	if got := commandEnvValue(cmd, grokJunctionLinkEnv); got != link {
		t.Fatalf("junction link env = %q, want %q", got, link)
	}
	if got := commandEnvValue(cmd, grokJunctionTargetEnv); got != target {
		t.Fatalf("junction target env = %q, want %q", got, target)
	}
}

func TestGrokWindowsRemoveJunctionCommand_DoesNotInterpolatePaths(t *testing.T) {
	link := `C:\temp\grok&(link)`
	cmd := grokWindowsRemoveJunctionCommand(link)
	commandLine := strings.Join(cmd.Args, " ")
	if strings.Contains(commandLine, link) {
		t.Fatalf("junction path leaked into cmd.exe tokens: %q", commandLine)
	}
	for _, want := range []string{"/d", "/v:off", "rmdir", `%` + grokJunctionLinkEnv + `%`} {
		if !strings.Contains(commandLine, want) {
			t.Fatalf("junction removal command %q missing %q", commandLine, want)
		}
	}
	if got := commandEnvValue(cmd, grokJunctionLinkEnv); got != link {
		t.Fatalf("junction link env = %q, want %q", got, link)
	}
}

func TestGrokWindowsCommands_UseComSpec(t *testing.T) {
	comSpec := filepath.Join(t.TempDir(), "sentinel-cmd.exe")
	t.Setenv("ComSpec", comSpec)
	t.Setenv("SystemRoot", filepath.Join(t.TempDir(), "ignored-system-root"))
	if got := grokWindowsJunctionCommand("link", "target").Path; got != comSpec {
		t.Fatalf("junction command path = %q, want ComSpec %q", got, comSpec)
	}
	if got := grokWindowsRemoveJunctionCommand("link").Path; got != comSpec {
		t.Fatalf("remove command path = %q, want ComSpec %q", got, comSpec)
	}

	systemRoot := t.TempDir()
	t.Setenv("ComSpec", "")
	t.Setenv("SystemRoot", systemRoot)
	wantFallback := filepath.Join(systemRoot, "System32", "cmd.exe")
	if got := grokWindowsJunctionCommand("link", "target").Path; got != wantFallback {
		t.Fatalf("junction fallback path = %q, want %q", got, wantFallback)
	}
	if got := grokWindowsRemoveJunctionCommand("link").Path; got != wantFallback {
		t.Fatalf("remove fallback path = %q, want %q", got, wantFallback)
	}
}

func TestUnlinkGrokDirectory_PlainDirReturnsNotLinkedSentinel(t *testing.T) {
	plainDir := t.TempDir()
	if err := unlinkGrokDirectory(plainDir); !errors.Is(err, errGrokStoreNotLinked) {
		t.Fatalf("unlinkGrokDirectory error = %v, want errGrokStoreNotLinked", err)
	}
	if _, err := os.Stat(plainDir); err != nil {
		t.Fatalf("plain directory should remain: %v", err)
	}
}

func TestUnlinkGrokDirectory_MissingIsNoOp(t *testing.T) {
	if err := unlinkGrokDirectory(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatalf("missing directory link should be a no-op: %v", err)
	}
}

func TestUnlinkGrokDirectory_RemovesSymlinkWithoutDeletingTarget(t *testing.T) {
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "transcript.jsonl"), []byte("synthetic"), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(t.TempDir(), "sessions-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	if err := unlinkGrokDirectory(link); err != nil {
		t.Fatalf("unlinkGrokDirectory: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("directory link still exists (lstat err = %v)", err)
	}
	if body, err := os.ReadFile(filepath.Join(target, "transcript.jsonl")); err != nil || string(body) != "synthetic" {
		t.Fatalf("target content after unlink = %q, err=%v", body, err)
	}
}

func commandEnvValue(cmd interface{ Environ() []string }, key string) string {
	prefix := key + "="
	for _, value := range cmd.Environ() {
		if strings.EqualFold(value[:min(len(value), len(prefix))], prefix) {
			return strings.TrimPrefix(value, value[:len(prefix)])
		}
	}
	return ""
}

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
