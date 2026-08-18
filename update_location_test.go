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
	writable := func(string) bool { return true }

	// Mounted DMG — unsupported.
	if ok, _ := darwinLocationSupportedWithWritable("/Volumes/AI Expedite/AI Expedite.app/Contents/MacOS/aix", writable); ok {
		t.Fatal("a mounted DMG must be unsupported")
	}
	// ~/Applications — supported.
	userApp := filepath.Join(home, "Applications", "AI Expedite.app", "Contents", "MacOS", "aix")
	if ok, _ := darwinLocationSupportedWithWritable(userApp, writable); !ok {
		t.Fatal("~/Applications must be supported")
	}
	// /Applications — supported (pre-relocation).
	if ok, _ := darwinLocationSupportedWithWritable("/Applications/AI Expedite.app/Contents/MacOS/aix", writable); !ok {
		t.Fatal("/Applications must be supported before relocation")
	}
	// ~/Downloads — unsupported.
	dl := filepath.Join(home, "Downloads", "AI Expedite.app", "Contents", "MacOS", "aix")
	if ok, _ := darwinLocationSupportedWithWritable(dl, writable); ok {
		t.Fatal("~/Downloads must be unsupported")
	}
	// Raw executables cannot be replaced by the bundle updater, even when the
	// containing directory is writable.
	if ok, reason := darwinLocationSupportedWithWritable("/usr/local/bin/aix", writable); ok || reason == "" {
		t.Fatalf("raw executable = (%v, %q), want unsupported with a reason", ok, reason)
	}
}

func TestDarwinLocationSupportedRequiresWritableApplicationsParent(t *testing.T) {
	exe := "/Applications/AI Expedite.app/Contents/MacOS/aix"
	probed := ""
	ok, reason := darwinLocationSupportedWithWritable(exe, func(dir string) bool {
		probed = filepath.ToSlash(dir)
		return false
	})
	if ok {
		t.Fatal("a read-only /Applications install must not silently update")
	}
	if probed != "/Applications" {
		t.Fatalf("writability probe = %q, want /Applications", probed)
	}
	if reason == "" {
		t.Fatal("blocked install must provide a tray reason")
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
	if got := effectiveUpdateTarget(embedded, "linux", "relative.AppImage"); got != embedded {
		t.Fatalf("relative APPIMAGE target = %q, want executable %q", got, embedded)
	}
	if got := effectiveUpdateTarget(embedded, "windows", outer); got != embedded {
		t.Fatalf("non-Linux target = %q, want executable %q", got, embedded)
	}
}

func TestEffectiveUpdateTargetSelectsTemporaryUpdaterAppImage(t *testing.T) {
	embeddedUpdater := "/tmp/.mount_new/usr/bin/AIExpediteTerminal"
	temporaryAppImage := filepath.Join(t.TempDir(), "agent_update_123.AppImage")
	if got := effectiveUpdateTarget(embeddedUpdater, "linux", temporaryAppImage); got != temporaryAppImage {
		t.Fatalf("self-replace source = %q, want outer updater AppImage %q", got, temporaryAppImage)
	}
}

func TestSilentUpdateCapableFlag(t *testing.T) {
	// Default build is capable; the label reflects that.
	label, _ := autoUpdateTrayLabel()
	if silentUpdateCapable() && label != "Automatically update" {
		t.Fatalf("capable build should label %q, got %q", "Automatically update", label)
	}
}
