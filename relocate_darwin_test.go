//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLaunchRelocatedDarwinBundleWaitsForOpenResult(t *testing.T) {
	binDir := t.TempDir()
	openPath := filepath.Join(binDir, "open")
	script := []byte("#!/bin/sh\necho relocation-rejected >&2\nexit 23\n")
	if err := os.WriteFile(openPath, script, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	for _, forceNew := range []bool{false, true} {
		err := launchRelocatedDarwinBundle("/Users/test/Applications/AI Expedite.app", forceNew)
		if err == nil {
			t.Fatalf("forceNew=%v: non-zero LaunchServices result must fail relocation", forceNew)
		}
		if !strings.Contains(err.Error(), "relocation-rejected") {
			t.Fatalf("forceNew=%v: error should include open output, got %v", forceNew, err)
		}
	}
}

func TestHandleDarwinUninstallRemovesUserBundle(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	bundleName := EnvDisplayName + ".app"
	userApp := filepath.Join(fakeHome, "Applications", bundleName)
	if err := os.MkdirAll(userApp, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(userApp); err != nil {
		t.Fatalf("expected %s to exist before uninstall", userApp)
	}

	handleDarwinUninstall(true)

	if _, err := os.Stat(userApp); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be removed by handleDarwinUninstall", userApp)
	}
}

func TestRecoverDarwinAppsDir(t *testing.T) {
	dir := t.TempDir()
	targetName := "AI Expedite.app"
	target := filepath.Join(dir, targetName)
	backup := filepath.Join(dir, ".aixupd_old_"+targetName)

	if err := os.MkdirAll(backup, 0o755); err != nil {
		t.Fatal(err)
	}

	recoverDarwinAppsDir(dir)

	if _, err := os.Stat(target); err != nil {
		t.Fatalf("expected backup to be restored to target %s: %v", target, err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("expected backup %s to be gone after restore", backup)
	}
}
