// File: update_location_test.go
// Tests for the install-location probes that gate the automatic path
// (writability + macOS/Windows location rules).
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDarwinBundlePath(t *testing.T) {
	got := darwinBundlePath("/Users/x/Applications/AI Expedite.app/Contents/MacOS/aix")
	want := "/Users/x/Applications/AI Expedite.app"
	if got != want {
		t.Fatalf("darwinBundlePath = %q, want %q", got, want)
	}
	if got := darwinBundlePath("/usr/local/bin/aix"); got != "" {
		t.Fatalf("non-bundle path should return empty, got %q", got)
	}
}

func TestDarwinLocationSupported(t *testing.T) {
	home, _ := os.UserHomeDir()

	// Mounted DMG — unsupported.
	if ok, _ := darwinLocationSupported("/Volumes/AI Expedite/AI Expedite.app/Contents/MacOS/aix"); ok {
		t.Fatal("a mounted DMG must be unsupported")
	}
	// ~/Applications — supported.
	userApp := filepath.Join(home, "Applications", "AI Expedite.app", "Contents", "MacOS", "aix")
	if ok, _ := darwinLocationSupported(userApp); !ok {
		t.Fatal("~/Applications must be supported")
	}
	// /Applications — supported (pre-relocation).
	if ok, _ := darwinLocationSupported("/Applications/AI Expedite.app/Contents/MacOS/aix"); !ok {
		t.Fatal("/Applications must be supported before relocation")
	}
	// ~/Downloads — unsupported.
	dl := filepath.Join(home, "Downloads", "AI Expedite.app", "Contents", "MacOS", "aix")
	if ok, _ := darwinLocationSupported(dl); ok {
		t.Fatal("~/Downloads must be unsupported")
	}
}

func TestIsMachineWideWindowsInstall(t *testing.T) {
	if !isMachineWideWindowsInstall(`C:\Program Files\AIExpedite\aix.exe`) {
		t.Fatal("Program Files should be machine-wide")
	}
	if !isMachineWideWindowsInstall(`C:\Program Files (x86)\AIExpedite\aix.exe`) {
		t.Fatal("Program Files (x86) should be machine-wide")
	}
	if isMachineWideWindowsInstall(`C:\Users\me\AppData\Local\AIExpedite\aix.exe`) {
		t.Fatal("%LOCALAPPDATA% must NOT be machine-wide")
	}
}

func TestDirWritable(t *testing.T) {
	if !dirWritable(t.TempDir()) {
		t.Fatal("a fresh temp dir should be writable")
	}
	if dirWritable(filepath.Join(t.TempDir(), "does-not-exist")) {
		t.Fatal("a nonexistent dir should not be writable")
	}
}

func TestEffectiveUpdateTargetUsesOuterAppImage(t *testing.T) {
	embedded := filepath.Join(string(filepath.Separator), "tmp", ".mount_aix", "usr", "bin", "AIExpediteTerminal")
	outer := filepath.Join(t.TempDir(), "AIExpediteTerminal.AppImage")

	if got := effectiveUpdateTarget(embedded, "linux", outer); got != outer {
		t.Fatalf("effectiveUpdateTarget = %q, want outer AppImage %q", got, outer)
	}
	if got := effectiveUpdateTarget(embedded, "linux", ""); got != embedded {
		t.Fatalf("raw Linux target = %q, want executable %q", got, embedded)
	}
	if got := effectiveUpdateTarget(embedded, "windows", outer); got != embedded {
		t.Fatalf("non-Linux target = %q, want executable %q", got, embedded)
	}
}

func TestSilentUpdateCapableFlag(t *testing.T) {
	// Default build is capable; the label reflects that.
	label, _ := autoUpdateTrayLabel()
	if silentUpdateCapable() && label != "Automatically update" {
		t.Fatalf("capable build should label %q, got %q", "Automatically update", label)
	}
}
