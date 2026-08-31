//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrokWindowsJunctionCommands_HandleMetacharacterPaths(t *testing.T) {
	t.Setenv("AIEXPEDITE_GROK_UNSET_SENTINEL", "")
	root := t.TempDir()
	target := filepath.Join(root, `target%AIEXPEDITE_GROK_UNSET_SENTINEL%&(!)`)
	link := filepath.Join(root, `home&(!)`, `sessions%AIEXPEDITE_GROK_UNSET_SENTINEL%!`)
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("create target: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatalf("create link parent: %v", err)
	}
	persisted := filepath.Join(target, "updates.jsonl")
	if err := os.WriteFile(persisted, []byte("synthetic"), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	createCmd := grokWindowsJunctionCommand(link, target)
	if strings.Contains(createCmd.SysProcAttr.CmdLine, link) || strings.Contains(createCmd.SysProcAttr.CmdLine, target) {
		t.Fatalf("filesystem path leaked into raw cmd.exe command line: %q", createCmd.SysProcAttr.CmdLine)
	}
	if out, err := createCmd.CombinedOutput(); err != nil {
		t.Fatalf("create junction: %v (output: %s)", err, out)
	}
	t.Cleanup(func() {
		if _, err := os.Lstat(link); err == nil {
			_, _ = grokWindowsRemoveJunctionCommand(link).CombinedOutput()
		}
	})

	if _, err := os.Stat(filepath.Join(link, "updates.jsonl")); err != nil {
		t.Fatalf("read target through metacharacter junction: %v", err)
	}
	removeCmd := grokWindowsRemoveJunctionCommand(link)
	if strings.Contains(removeCmd.SysProcAttr.CmdLine, link) {
		t.Fatalf("filesystem path leaked into raw cmd.exe removal line: %q", removeCmd.SysProcAttr.CmdLine)
	}
	if out, err := removeCmd.CombinedOutput(); err != nil {
		t.Fatalf("remove junction: %v (output: %s)", err, out)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("junction still exists (lstat err = %v)", err)
	}
	if body, err := os.ReadFile(persisted); err != nil || string(body) != "synthetic" {
		t.Fatalf("target after junction removal = %q, err=%v", body, err)
	}
}

func TestRemoveIsolatedGrokHome_KeepsCaseVariantStoreWhenUnlinkFails(t *testing.T) {
	store := withTempGrokSessionStore(t)
	t.Setenv("GROK_HOME", t.TempDir())
	home, err := setupIsolatedGrokHome(false, grokACPDefaultModel)
	if err != nil {
		t.Fatalf("setupIsolatedGrokHome: %v", err)
	}

	lowercaseLink := filepath.Join(home, grokSessionsDirName)
	caseVariantLink := filepath.Join(home, "Sessions")
	temporaryLink := filepath.Join(home, "sessions-case-rename")
	if err := os.Rename(lowercaseLink, temporaryLink); err != nil {
		t.Fatalf("temporarily rename sessions junction: %v", err)
	}
	if err := os.Rename(temporaryLink, caseVariantLink); err != nil {
		t.Fatalf("case-rename sessions junction: %v", err)
	}
	persisted := writeTestGrokTranscript(t, store)

	syntheticErr := errors.New("synthetic unlink failure")
	err = removeIsolatedGrokHomeWithUnlink(home, func(string) error { return syntheticErr })
	if !errors.Is(err, syntheticErr) {
		t.Fatalf("removeIsolatedGrokHome error = %v, want synthetic unlink failure", err)
	}
	if _, err := os.Lstat(caseVariantLink); err != nil {
		t.Fatalf("case-variant sessions junction should remain: %v", err)
	}
	if body, err := os.ReadFile(persisted); err != nil || string(body) != testGrokTranscript {
		t.Fatalf("persistent transcript after partial cleanup = %q, err=%v", body, err)
	}
	if err := removeIsolatedGrokHome(home); err != nil {
		t.Fatalf("final isolated home cleanup: %v", err)
	}
}
