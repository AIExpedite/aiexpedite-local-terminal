//go:build darwin

package main

import (
	"fmt"
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
	bundleName := currentDarwinBundleName()
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

func TestRecoverDarwinAppBundle(t *testing.T) {
	dir := t.TempDir()
	myBundle := "AI Expedite Dev.app"
	otherBundle := "AI Expedite.app"

	target := filepath.Join(dir, myBundle)
	backup := filepath.Join(dir, ".aixupd_old_"+myBundle)

	otherTarget := filepath.Join(dir, otherBundle)
	otherBackup := filepath.Join(dir, ".aixupd_old_"+otherBundle)
	otherStaged := filepath.Join(dir, ".aixupd_staged_"+otherBundle)

	if err := os.MkdirAll(backup, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(otherTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(otherBackup, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(otherStaged, 0o755); err != nil {
		t.Fatal(err)
	}

	recoverDarwinAppBundle(dir, myBundle)

	// My bundle should be restored and its backup removed
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("expected backup to be restored to target %s: %v", target, err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("expected backup %s to be gone after restore", backup)
	}

	// Other channel's backup and staged artifacts should NOT have been touched
	if _, err := os.Stat(otherBackup); err != nil {
		t.Fatalf("expected other channel backup to remain untouched: %v", err)
	}
	if _, err := os.Stat(otherStaged); err != nil {
		t.Fatalf("expected other channel staged to remain untouched: %v", err)
	}
}

func TestMaybeRelocateInstallDoesNotCleanInFlightUpdateArtifacts(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	apps := filepath.Join(fakeHome, "Applications")
	if err := os.MkdirAll(apps, 0o755); err != nil {
		t.Fatal(err)
	}
	bundleName := defaultDarwinBundleName
	target := filepath.Join(apps, bundleName)
	staged := filepath.Join(apps, ".aixupd_staged_"+bundleName)
	backup := filepath.Join(apps, ".aixupd_old_"+bundleName)
	for _, path := range []string{target, staged, backup} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if maybeRelocateInstall(nil) {
		t.Fatal("unbundled test process should not relocate")
	}

	for _, path := range []string{staged, backup} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("in-flight update artifact %s must survive relocation before singleton: %v", path, err)
		}
	}
}

func TestRecoverInterruptedInstallCleansStaleArtifactsAfterSingleton(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	apps := filepath.Join(fakeHome, "Applications")
	if err := os.MkdirAll(apps, 0o755); err != nil {
		t.Fatal(err)
	}
	bundleName := currentDarwinBundleName()
	target := filepath.Join(apps, bundleName)
	staged := filepath.Join(apps, ".aixupd_staged_"+bundleName)
	backup := filepath.Join(apps, ".aixupd_old_"+bundleName)
	for _, path := range []string{target, staged, backup} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	recoverInterruptedInstall()

	if _, err := os.Stat(target); err != nil {
		t.Fatalf("installed target must remain: %v", err)
	}
	for _, path := range []string{staged, backup} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stale artifact %s should be cleaned after singleton recovery: %v", path, err)
		}
	}
}

func TestCurrentDarwinBundleName_Fallback(t *testing.T) {
	name := currentDarwinBundleName()
	if name == "" {
		t.Fatal("currentDarwinBundleName returned empty string")
	}
	if !strings.HasSuffix(name, ".app") {
		t.Fatalf("currentDarwinBundleName = %q, want *.app suffix", name)
	}
}

func TestRunningFromDarwinInstallMedia(t *testing.T) {
	cases := []struct {
		bundle string
		want   bool
	}{
		{"/Volumes/AI Expedite/AIExpediteTerminal.app", true},
		{"/private/var/folders/x1/T/AppTranslocation/2B7A/d/AIExpediteTerminal.app", true},
		{"/Users/test/Applications/AIExpediteTerminal.app", false},
		{"/Applications/AIExpediteTerminal.app", false},
		{"/Users/test/Downloads/AIExpediteTerminal.app", false},
	}
	for _, c := range cases {
		if got := runningFromDarwinInstallMedia(c.bundle); got != c.want {
			t.Errorf("runningFromDarwinInstallMedia(%q) = %v, want %v", c.bundle, got, c.want)
		}
	}
}

func TestDarwinVolumeRoot(t *testing.T) {
	cases := map[string]string{
		"/Volumes/AI Expedite/AIExpediteTerminal.app":             "/Volumes/AI Expedite",
		"/Volumes/AI Expedite":                                    "/Volumes/AI Expedite",
		"/Volumes/":                                               "",
		"/Users/test/Applications/X.app":                          "",
		"/private/var/folders/x1/T/AppTranslocation/2B7A/d/X.app": "",
	}
	for in, want := range cases {
		if got := darwinVolumeRoot(in); got != want {
			t.Errorf("darwinVolumeRoot(%q) = %q, want %q", in, got, want)
		}
	}
}

// stubDarwinInstallTools puts fake ditto/xattr/open/osascript on PATH and
// returns the file the `open` stub records its arguments in.
func stubDarwinInstallTools(t *testing.T, openExit int) string {
	t.Helper()
	binDir := t.TempDir()
	openLog := filepath.Join(binDir, "open.args")

	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write("ditto", "#!/bin/sh\ncp -R \"$1\" \"$2\"\n")
	write("xattr", "#!/bin/sh\nexit 0\n")
	write("osascript", "#!/bin/sh\nexit 0\n")
	write("open", fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\nexit %d\n", openLog, openExit))

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return openLog
}

// newFakeDarwinBundle creates <dir>/<name>/Contents/MacOS/<binary> and returns
// the bundle and executable paths.
func newFakeDarwinBundle(t *testing.T, dir, marker string) (bundle, exe string) {
	t.Helper()
	bundle = filepath.Join(dir, defaultDarwinBundleName)
	exe = filepath.Join(bundle, "Contents", "MacOS", "AIExpediteTerminal")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	return bundle, exe
}

func TestInstallDarwinFromMediaInstallsAndHandsOver(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	openLog := stubDarwinInstallTools(t, 0)

	media := filepath.Join(t.TempDir(), "AppTranslocation", "d")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	bundle, exe := newFakeDarwinBundle(t, media, "new")
	userApps := filepath.Join(home, "Applications")

	if !installDarwinFromMedia(bundle, exe, userApps) {
		t.Fatal("install from media should hand over and report true")
	}

	dest := filepath.Join(userApps, defaultDarwinBundleName)
	destExe := filepath.Join(dest, "Contents", "MacOS", "AIExpediteTerminal")
	if _, err := os.Stat(destExe); err != nil {
		t.Fatalf("expected installed binary at %s: %v", destExe, err)
	}
	args, err := os.ReadFile(openLog)
	if err != nil {
		t.Fatalf("expected the installed copy to be launched: %v", err)
	}
	if !strings.Contains(string(args), "-n "+dest) {
		t.Fatalf("expected a forced new instance of %s, got %q", dest, args)
	}

	// The login item must point at the INSTALLED binary, never at the image.
	plist, err := os.ReadFile(filepath.Join(home, "Library", "LaunchAgents",
		"com.aiexpedite.terminal"+EnvConfigSuffix+".plist"))
	if err != nil {
		t.Fatalf("expected a LaunchAgent for the installed copy: %v", err)
	}
	if !strings.Contains(string(plist), destExe) {
		t.Fatalf("LaunchAgent should point at %s, got:\n%s", destExe, plist)
	}

	// No staging or rollback leftovers next to the install.
	entries, err := os.ReadDir(userApps)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".aixinstall_") {
			t.Fatalf("leftover install artifact %s", e.Name())
		}
	}
}

func TestInstallDarwinFromMediaReplacesExistingInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubDarwinInstallTools(t, 0)

	userApps := filepath.Join(home, "Applications")
	if err := os.MkdirAll(userApps, 0o755); err != nil {
		t.Fatal(err)
	}
	_, oldExe := newFakeDarwinBundle(t, userApps, "old")

	media := filepath.Join(t.TempDir(), "AppTranslocation", "d")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	bundle, exe := newFakeDarwinBundle(t, media, "new")

	if !installDarwinFromMedia(bundle, exe, userApps) {
		t.Fatal("reinstall over an existing copy should succeed")
	}

	got, err := os.ReadFile(oldExe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("expected the installed bundle to be replaced, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(userApps, ".aixinstall_old_"+defaultDarwinBundleName)); !os.IsNotExist(err) {
		t.Fatal("rollback copy should be removed once the new copy launched")
	}
}

func TestInstallDarwinFromMediaRestoresPreviousInstallOnFailedLaunch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubDarwinInstallTools(t, 23) // LaunchServices rejects the new bundle

	userApps := filepath.Join(home, "Applications")
	if err := os.MkdirAll(userApps, 0o755); err != nil {
		t.Fatal(err)
	}
	_, oldExe := newFakeDarwinBundle(t, userApps, "old")

	media := filepath.Join(t.TempDir(), "AppTranslocation", "d")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	bundle, exe := newFakeDarwinBundle(t, media, "new")

	if installDarwinFromMedia(bundle, exe, userApps) {
		t.Fatal("a rejected launch must not report a completed install")
	}

	got, err := os.ReadFile(oldExe)
	if err != nil {
		t.Fatalf("previous install should have been restored: %v", err)
	}
	if string(got) != "old" {
		t.Fatalf("expected the previous bundle back in place, got %q", got)
	}
}

func TestAppleScriptStringEscapes(t *testing.T) {
	if got := appleScriptString(`say "hi"\now`); got != `"say \"hi\"\\now"` {
		t.Fatalf("unexpected AppleScript literal %s", got)
	}
}
