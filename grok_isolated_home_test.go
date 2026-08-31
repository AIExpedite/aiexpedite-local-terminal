package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const (
	testGrokEncodedCWD = "C%3A%5Crepo"
	testGrokSessionID  = "session-uuid"
	testGrokTranscript = "turn"
)

func assertGrokSessionsLinked(t *testing.T, home string) {
	t.Helper()
	info, err := os.Lstat(filepath.Join(home, grokSessionsDirName))
	if err != nil {
		t.Fatalf("inspect sessions link: %v", err)
	}
	if info.IsDir() && info.Mode().Type()&^os.ModeDir == 0 {
		t.Fatal("session store was never linked: sessions is a plain directory")
	}
}

func writeTestGrokTranscript(t *testing.T, sessionsRoot string) string {
	t.Helper()
	transcript := filepath.Join(sessionsRoot, testGrokEncodedCWD, testGrokSessionID, "updates.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatalf("create synthetic session: %v", err)
	}
	if err := os.WriteFile(transcript, []byte(testGrokTranscript), 0o600); err != nil {
		t.Fatalf("write synthetic transcript: %v", err)
	}
	return transcript
}

// TestSetupIsolatedGrokHome_SessionStoreSurvivesHomeRemoval is the regression
// for Windows teardown descending into the sessions junction. Production
// teardown must remove the home without deleting the transcript behind it.
func TestSetupIsolatedGrokHome_SessionStoreSurvivesHomeRemoval(t *testing.T) {
	store := withTempGrokSessionStore(t)
	t.Setenv("GROK_HOME", t.TempDir())

	home, err := setupIsolatedGrokHome(false, grokACPDefaultModel)
	if err != nil {
		t.Fatalf("setupIsolatedGrokHome: %v", err)
	}
	assertGrokSessionsLinked(t, home)
	writeTestGrokTranscript(t, filepath.Join(home, grokSessionsDirName))

	if err := removeIsolatedGrokHome(home); err != nil {
		t.Fatalf("remove isolated home: %v", err)
	}

	persisted := filepath.Join(store, testGrokEncodedCWD, testGrokSessionID, "updates.jsonl")
	body, err := os.ReadFile(persisted)
	if err != nil {
		t.Fatalf("transcript did not survive the isolated home: %v", err)
	}
	if string(body) != testGrokTranscript {
		t.Fatalf("persisted transcript = %q, want %q", body, testGrokTranscript)
	}
}

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
	if err := os.WriteFile(filepath.Join(hookDir, "synthetic.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed plugin: %v", err)
	}
	if err := removeIsolatedGrokHome(first); err != nil {
		t.Fatalf("remove first home: %v", err)
	}

	second, err := setupIsolatedGrokHome(false, grokACPDefaultModel)
	if err != nil {
		t.Fatalf("setupIsolatedGrokHome (second): %v", err)
	}
	t.Cleanup(func() { _ = removeIsolatedGrokHome(second) })

	if _, err := os.Stat(filepath.Join(second, "installed-plugins", "synthetic.json")); !os.IsNotExist(err) {
		t.Fatalf("persisted config leaked into a later session (stat err = %v)", err)
	}
	if _, err := os.Stat(store); err != nil {
		t.Fatalf("session store should still exist: %v", err)
	}
}

func TestRemoveIsolatedGrokHome_UnlinksWithoutDeletingStore(t *testing.T) {
	store := withTempGrokSessionStore(t)
	t.Setenv("GROK_HOME", t.TempDir())

	home, err := setupIsolatedGrokHome(false, grokACPDefaultModel)
	if err != nil {
		t.Fatalf("setupIsolatedGrokHome: %v", err)
	}
	assertGrokSessionsLinked(t, home)
	persisted := writeTestGrokTranscript(t, store)
	want, err := os.ReadFile(persisted)
	if err != nil {
		t.Fatalf("read seeded transcript: %v", err)
	}

	if err := removeIsolatedGrokHome(home); err != nil {
		t.Fatalf("removeIsolatedGrokHome: %v", err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("isolated home still exists (stat err = %v)", err)
	}
	got, err := os.ReadFile(persisted)
	if err != nil {
		t.Fatalf("persistent transcript was removed: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("persistent transcript changed: got %q, want %q", got, want)
	}
}

func TestRemoveIsolatedGrokHome_ResumeSeesPreCleanupTranscript(t *testing.T) {
	withTempGrokSessionStore(t)
	t.Setenv("GROK_HOME", t.TempDir())

	first, err := setupIsolatedGrokHome(false, grokACPDefaultModel)
	if err != nil {
		t.Fatalf("setup first isolated home: %v", err)
	}
	writeTestGrokTranscript(t, filepath.Join(first, grokSessionsDirName))
	if err := removeIsolatedGrokHome(first); err != nil {
		t.Fatalf("remove first isolated home: %v", err)
	}

	second, err := setupIsolatedGrokHome(false, grokACPDefaultModel)
	if err != nil {
		t.Fatalf("setup resumed isolated home: %v", err)
	}
	t.Cleanup(func() { _ = removeIsolatedGrokHome(second) })
	resumed := filepath.Join(second, grokSessionsDirName, testGrokEncodedCWD, testGrokSessionID, "updates.jsonl")
	body, err := os.ReadFile(resumed)
	if err != nil {
		t.Fatalf("resumed home cannot read pre-cleanup transcript: %v", err)
	}
	if string(body) != testGrokTranscript {
		t.Fatalf("resumed transcript = %q, want %q", body, testGrokTranscript)
	}
}

func TestRemoveIsolatedGrokHome_KeepsStoreWhenUnlinkFails(t *testing.T) {
	store := withTempGrokSessionStore(t)
	t.Setenv("GROK_HOME", t.TempDir())

	home, err := setupIsolatedGrokHome(false, grokACPDefaultModel)
	if err != nil {
		t.Fatalf("setupIsolatedGrokHome: %v", err)
	}
	assertGrokSessionsLinked(t, home)
	persisted := writeTestGrokTranscript(t, store)
	sibling := filepath.Join(home, "config.toml")

	originalUnlink := grokUnlinkDir
	t.Cleanup(func() { grokUnlinkDir = originalUnlink })
	syntheticErr := errors.New("synthetic unlink failure")
	grokUnlinkDir = func(string) error { return syntheticErr }
	err = removeIsolatedGrokHome(home)
	grokUnlinkDir = originalUnlink

	if !errors.Is(err, syntheticErr) {
		t.Fatalf("removeIsolatedGrokHome error = %v, want synthetic unlink failure", err)
	}
	if _, err := os.Lstat(filepath.Join(home, grokSessionsDirName)); err != nil {
		t.Fatalf("sessions link should remain after unlink failure: %v", err)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("isolated home should remain after unlink failure: %v", err)
	}
	if _, err := os.Stat(sibling); !os.IsNotExist(err) {
		t.Fatalf("non-session sibling survived partial cleanup (stat err = %v)", err)
	}
	if body, err := os.ReadFile(persisted); err != nil || string(body) != testGrokTranscript {
		t.Fatalf("persistent transcript after unlink failure = %q, err=%v", body, err)
	}
	if err := removeIsolatedGrokHome(home); err != nil {
		t.Fatalf("final isolated home cleanup: %v", err)
	}
}
