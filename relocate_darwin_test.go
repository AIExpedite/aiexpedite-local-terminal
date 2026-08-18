//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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

// hdiutilInfoFixture is `hdiutil info` output with one attached image mounted
// at /Volumes/AI Expedite. /Volumes/USB below is deliberately absent: an
// ordinary mounted volume never appears here.
const hdiutilInfoFixture = "framework       : 594.30.1\n" +
	"driver          : 594.30.1\n" +
	"================================================\n" +
	"image-path      : /Users/test/Downloads/aiexpedite-terminal-darwin-arm64.dmg\n" +
	"image-type      : read-only disk image\n" +
	"writeable       : false\n" +
	"/dev/disk4\tGUID_partition_scheme\t\n" +
	"/dev/disk4s1\t48465300-0000-11AA-AA11-00306543ECAC\t/Volumes/AI Expedite\n"

func stubHdiutilInfo(t *testing.T, out string, err error) {
	t.Helper()
	previous := runHdiutilInfo
	runHdiutilInfo = func() (string, error) { return out, err }
	t.Cleanup(func() { runHdiutilInfo = previous })
}

func TestDarwinDiskImageMountPoints(t *testing.T) {
	got := darwinDiskImageMountPoints(hdiutilInfoFixture)
	if len(got) != 1 || got[0] != "/Volumes/AI Expedite" {
		t.Fatalf("expected the image mount point only, got %q", got)
	}
}

func TestRunningFromDarwinInstallMedia(t *testing.T) {
	stubHdiutilInfo(t, hdiutilInfoFixture, nil)
	cases := []struct {
		bundle string
		want   bool
	}{
		{"/Volumes/AI Expedite/AIExpediteTerminal.app", true},
		{"/private/var/folders/x1/T/AppTranslocation/2B7A/d/AIExpediteTerminal.app", true},
		// A USB stick or network share is NOT install media: an app someone
		// deliberately runs off portable media must be left where it is.
		{"/Volumes/USB/Tools/AIExpediteTerminal.app", false},
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

func TestRunningFromDarwinInstallMediaFailsClosedWithoutHdiutil(t *testing.T) {
	stubHdiutilInfo(t, "", errors.New("hdiutil unavailable"))
	if runningFromDarwinInstallMedia("/Volumes/AI Expedite/AIExpediteTerminal.app") {
		t.Fatal("an unprovable volume must not be treated as install media")
	}
	if !runningFromDarwinInstallMedia("/private/var/folders/x1/T/AppTranslocation/2B7A/d/X.app") {
		t.Fatal("a translocated bundle is install media regardless of hdiutil")
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
	stubAgentInstanceForInstall(t, true)
	return openLog
}

// stubAgentInstanceForInstall replaces the per-account singleton with an
// in-memory one so tests never touch the real lock (or contend with an agent
// actually running on the machine). It reports whether the release ran.
func stubAgentInstanceForInstall(t *testing.T, acquired bool) *bool {
	t.Helper()
	released := false
	previous := acquireAgentInstanceForInstall
	acquireAgentInstanceForInstall = func() (func(), bool) {
		return func() { released = true }, acquired
	}
	t.Cleanup(func() { acquireAgentInstanceForInstall = previous })
	return &released
}

// testBundleID is the identifier the fake bundles below are signed with —
// the channel under test.
const testBundleID = "com.aiexpedite.terminal"

// newFakeDarwinBundle creates <dir>/<name>/Contents/MacOS/<binary> plus an
// Info.plist carrying testBundleID, and returns the bundle and exe paths.
func newFakeDarwinBundle(t *testing.T, dir, marker string) (bundle, exe string) {
	t.Helper()
	return newFakeDarwinBundleAs(t, dir, defaultDarwinBundleName, marker, testBundleID)
}

// newFakeDarwinBundleAs is newFakeDarwinBundle with an explicit bundle name and
// identifier, for cross-channel cases.
func newFakeDarwinBundleAs(t *testing.T, dir, name, marker, bundleID string) (bundle, exe string) {
	t.Helper()
	bundle = filepath.Join(dir, name)
	exe = filepath.Join(bundle, "Contents", "MacOS", "AIExpediteTerminal")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>%s</string>
<key>CFBundleExecutable</key><string>AIExpediteTerminal</string>
</dict></plist>
`, bundleID)
	if err := os.WriteFile(filepath.Join(bundle, "Contents", "Info.plist"), []byte(plist), 0o644); err != nil {
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

func TestInstallDarwinFromMediaReleasesSingletonBeforeHandingOver(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubDarwinInstallTools(t, 0)
	released := stubAgentInstanceForInstall(t, true)

	media := filepath.Join(t.TempDir(), "AppTranslocation", "d")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	bundle, exe := newFakeDarwinBundle(t, media, "new")

	if !installDarwinFromMedia(bundle, exe, filepath.Join(home, "Applications")) {
		t.Fatal("install should hand over")
	}
	// The replacement acquires the same lock as soon as LaunchServices starts
	// it, so it must be free by the time `open` runs.
	if !*released {
		t.Fatal("the singleton must be released before the replacement is launched")
	}
}

func TestInstallDarwinFromMediaLeavesRunningInstallUntouched(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	openLog := stubDarwinInstallTools(t, 0)
	stubAgentInstanceForInstall(t, false) // another agent owns this install

	userApps := filepath.Join(home, "Applications")
	if err := os.MkdirAll(userApps, 0o755); err != nil {
		t.Fatal(err)
	}
	_, installedExe := newFakeDarwinBundle(t, userApps, "running")

	media := filepath.Join(t.TempDir(), "AppTranslocation", "d")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	bundle, exe := newFakeDarwinBundle(t, media, "new")

	if !installDarwinFromMedia(bundle, exe, userApps) {
		t.Fatal("the running install is authoritative, so this process must exit")
	}

	// A running agent may be mid-update on exactly this bundle: replacing it
	// here could overwrite a verified update after its rollback copy is gone.
	got, err := os.ReadFile(installedExe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "running" {
		t.Fatalf("the running install must not be replaced, got %q", got)
	}
	if _, err := os.Stat(openLog); !os.IsNotExist(err) {
		t.Fatal("no second agent may be launched alongside the running one")
	}
}

func TestInstallDarwinFromMediaInstallsWhenNothingIsInstalledYet(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubDarwinInstallTools(t, 0)
	stubAgentInstanceForInstall(t, false) // an agent runs from somewhere else

	media := filepath.Join(t.TempDir(), "AppTranslocation", "d")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	bundle, exe := newFakeDarwinBundle(t, media, "new")
	userApps := filepath.Join(home, "Applications")

	if !installDarwinFromMedia(bundle, exe, userApps) {
		t.Fatal("a fresh install destroys nothing and must still proceed")
	}
	if _, err := os.Stat(filepath.Join(userApps, defaultDarwinBundleName)); err != nil {
		t.Fatalf("expected a fresh install at %s: %v", userApps, err)
	}
}

func TestChannelScopedDarwinBundleName(t *testing.T) {
	// EnvConfigSuffix is "" in tests (prod), so the scoped name is -Prod.
	if got := channelScopedDarwinBundleName("AIExpediteTerminal.app"); got != "AIExpediteTerminal-Prod.app" {
		t.Fatalf("unexpected scoped name %q", got)
	}
	if got := channelScopedDarwinBundleName("AIExpediteTerminal-Prod.app"); got != "AIExpediteTerminal-Prod.app" {
		t.Fatalf("an already-scoped name must not be scoped twice, got %q", got)
	}
}

func TestInstallDarwinFromMediaNeverOverwritesAnotherChannel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubDarwinInstallTools(t, 0)

	userApps := filepath.Join(home, "Applications")
	if err := os.MkdirAll(userApps, 0o755); err != nil {
		t.Fatal(err)
	}
	// A dev install occupies the shared bundle name. Its singleton is a
	// different lock, so nothing else stops this install from eating it.
	_, devExe := newFakeDarwinBundleAs(t, userApps, defaultDarwinBundleName, "dev", "com.aiexpedite.terminal-Dev")

	media := filepath.Join(t.TempDir(), "AppTranslocation", "d")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	bundle, exe := newFakeDarwinBundle(t, media, "prod")

	if !installDarwinFromMedia(bundle, exe, userApps) {
		t.Fatal("install should proceed alongside the other channel")
	}

	got, err := os.ReadFile(devExe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "dev" {
		t.Fatalf("the other channel's install must be untouched, got %q", got)
	}
	scoped := filepath.Join(userApps, channelScopedDarwinBundleName(defaultDarwinBundleName))
	if _, err := os.Stat(scoped); err != nil {
		t.Fatalf("expected this channel to install alongside at %s: %v", scoped, err)
	}
}

func TestInstallDarwinFromMediaStaysAtItsScopedName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubDarwinInstallTools(t, 0)

	userApps := filepath.Join(home, "Applications")
	if err := os.MkdirAll(userApps, 0o755); err != nil {
		t.Fatal(err)
	}
	scopedName := channelScopedDarwinBundleName(defaultDarwinBundleName)
	_, scopedExe := newFakeDarwinBundleAs(t, userApps, scopedName, "old", testBundleID)

	media := filepath.Join(t.TempDir(), "AppTranslocation", "d")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	bundle, exe := newFakeDarwinBundle(t, media, "new")

	if !installDarwinFromMedia(bundle, exe, userApps) {
		t.Fatal("install should update the existing scoped copy")
	}

	got, err := os.ReadFile(scopedExe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("expected the scoped install to be updated in place, got %q", got)
	}
	// The now-free plain name must NOT collect a second copy of this channel.
	if _, err := os.Stat(filepath.Join(userApps, defaultDarwinBundleName)); !os.IsNotExist(err) {
		t.Fatal("install forked into a second copy at the plain name")
	}
}

func TestInstallDarwinFromMediaYieldsToALauncherThatWonTheRace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	openLog := stubDarwinInstallTools(t, 0)
	stubAgentInstanceForInstall(t, false) // no lock: create-only

	userApps := filepath.Join(home, "Applications")
	if err := os.MkdirAll(userApps, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(userApps, defaultDarwinBundleName)

	media := filepath.Join(t.TempDir(), "AppTranslocation", "d")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	bundle, exe := newFakeDarwinBundle(t, media, "loser")

	// The winner lands its install (and starts it) while this one is copying.
	previous := exclusiveRenameDarwin
	exclusiveRenameDarwin = func(src, dst string) error {
		newFakeDarwinBundleAs(t, userApps, defaultDarwinBundleName, "winner", testBundleID)
		return previous(src, dst)
	}
	t.Cleanup(func() { exclusiveRenameDarwin = previous })

	if !installDarwinFromMedia(bundle, exe, userApps) {
		t.Fatal("the winner's install is authoritative, so this process must exit")
	}

	got, err := os.ReadFile(filepath.Join(dest, "Contents", "MacOS", "AIExpediteTerminal"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "winner" {
		t.Fatalf("a lockless install must never replace the winner, got %q", got)
	}
	if _, err := os.Stat(openLog); !os.IsNotExist(err) {
		t.Fatal("the loser must not launch a second agent")
	}
}

func TestExclusiveRenameDarwinRefusesAnExistingDestination(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	for _, p := range []string{src, dst} {
		if err := os.MkdirAll(filepath.Join(p, "Contents"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := exclusiveRenameDarwin(src, dst); !errors.Is(err, unix.EEXIST) {
		t.Fatalf("expected EEXIST for an occupied destination, got %v", err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("a refused rename must leave the source in place: %v", err)
	}
	fresh := filepath.Join(dir, "fresh")
	if err := exclusiveRenameDarwin(src, fresh); err != nil {
		t.Fatalf("rename to a free path should succeed: %v", err)
	}
}

// stubAgentInstanceSequence returns the given acquisition results in order,
// repeating the last one, so a test can model "the replacement took the
// singleton while we were handing over".
func stubAgentInstanceSequence(t *testing.T, results ...bool) {
	t.Helper()
	previous := acquireAgentInstanceForInstall
	call := 0
	acquireAgentInstanceForInstall = func() (func(), bool) {
		acquired := results[len(results)-1]
		if call < len(results) {
			acquired = results[call]
		}
		call++
		return func() {}, acquired
	}
	t.Cleanup(func() { acquireAgentInstanceForInstall = previous })
}

// setDarwinInstallLockTiming shrinks the destination-lock wait so the tests
// below do not spend the production timeout.
func setDarwinInstallLockTiming(t *testing.T, wait, poll time.Duration) {
	t.Helper()
	previousWait, previousPoll := darwinInstallLockWait, darwinInstallLockPoll
	darwinInstallLockWait, darwinInstallLockPoll = wait, poll
	t.Cleanup(func() { darwinInstallLockWait, darwinInstallLockPoll = previousWait, previousPoll })
}

func TestInstallDarwinFromMediaWaitsOutAnotherInstaller(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubDarwinInstallTools(t, 0)
	setDarwinInstallLockTiming(t, darwinInstallLockWait, 10*time.Millisecond)

	userApps := filepath.Join(home, "Applications")
	if err := os.MkdirAll(userApps, 0o755); err != nil {
		t.Fatal(err)
	}
	// Another installer holds the directory. It may well be installing a
	// DIFFERENT channel, which would do nothing for this one, so this install
	// has to wait rather than treat it as the job being done.
	held, acquired, err := tryAcquireAgentInstanceLock(filepath.Join(userApps, ".aixinstall.lock"))
	if err != nil || !acquired {
		t.Fatalf("could not simulate a competing installer: %v", err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = unlockFile(held)
		_ = held.Close()
	}()

	media := filepath.Join(t.TempDir(), "AppTranslocation", "d")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	bundle, exe := newFakeDarwinBundle(t, media, "new")

	if !installDarwinFromMedia(bundle, exe, userApps) {
		t.Fatal("install should proceed once the other installer is done")
	}
	if _, err := os.Stat(filepath.Join(userApps, defaultDarwinBundleName)); err != nil {
		t.Fatalf("expected the install to land after waiting: %v", err)
	}
}

func TestInstallDarwinFromMediaGivesUpOnAWedgedInstaller(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubDarwinInstallTools(t, 0)
	setDarwinInstallLockTiming(t, 30*time.Millisecond, 5*time.Millisecond)

	userApps := filepath.Join(home, "Applications")
	if err := os.MkdirAll(userApps, 0o755); err != nil {
		t.Fatal(err)
	}
	held, acquired, err := tryAcquireAgentInstanceLock(filepath.Join(userApps, ".aixinstall.lock"))
	if err != nil || !acquired {
		t.Fatalf("could not simulate a competing installer: %v", err)
	}
	defer held.Close()

	media := filepath.Join(t.TempDir(), "AppTranslocation", "d")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	bundle, exe := newFakeDarwinBundle(t, media, "new")

	// Nothing was installed, so the app must keep running from the image
	// instead of exiting into nothing.
	if installDarwinFromMedia(bundle, exe, userApps) {
		t.Fatal("a wedged installer must not be reported as a completed install")
	}
	if _, err := os.Stat(filepath.Join(userApps, defaultDarwinBundleName)); !os.IsNotExist(err) {
		t.Fatal("nothing may be placed while another installer holds the lock")
	}
}

func TestInstallDarwinFromMediaRevalidatesOwnershipBeforeSwapping(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubDarwinInstallTools(t, 0) // singleton acquired

	userApps := filepath.Join(home, "Applications")
	if err := os.MkdirAll(userApps, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(userApps, defaultDarwinBundleName)

	media := filepath.Join(t.TempDir(), "AppTranslocation", "d")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	bundle, exe := newFakeDarwinBundle(t, media, "ours")

	// The destination was free when it was chosen; another channel's installer
	// lands there while this one is copying. Holding OUR singleton says
	// nothing about that bundle — it answers to a different lock.
	previous := exclusiveRenameDarwin
	exclusiveRenameDarwin = func(src, dst string) error {
		newFakeDarwinBundleAs(t, userApps, defaultDarwinBundleName, "other-channel", "com.aiexpedite.terminal-Beta")
		return previous(src, dst)
	}
	t.Cleanup(func() { exclusiveRenameDarwin = previous })

	if installDarwinFromMedia(bundle, exe, userApps) {
		t.Fatal("nothing was installed, so the app must keep running from the image")
	}
	got, err := os.ReadFile(filepath.Join(dest, "Contents", "MacOS", "AIExpediteTerminal"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "other-channel" {
		t.Fatalf("a stale ownership decision must not authorize the swap, got %q", got)
	}
}

func TestInstallDarwinFromMediaKeepsNewBundleWhenReplacementOwnsTheSingleton(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubDarwinInstallTools(t, 23) // LaunchServices reports an error...
	// ...but the replacement started anyway and now owns the singleton, so the
	// second acquisition (the rollback decision) fails.
	stubAgentInstanceSequence(t, true, false)

	userApps := filepath.Join(home, "Applications")
	if err := os.MkdirAll(userApps, 0o755); err != nil {
		t.Fatal(err)
	}
	_, installedExe := newFakeDarwinBundle(t, userApps, "old")

	media := filepath.Join(t.TempDir(), "AppTranslocation", "d")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	bundle, exe := newFakeDarwinBundle(t, media, "new")

	if !installDarwinFromMedia(bundle, exe, userApps) {
		t.Fatal("the replacement is authoritative, so this process must exit")
	}
	got, err := os.ReadFile(installedExe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("rolling back under the running replacement, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(userApps, ".aixinstall_old_"+defaultDarwinBundleName)); !os.IsNotExist(err) {
		t.Fatal("the rollback copy should be dropped once the replacement owns the install")
	}
}

func TestDarwinInstallDestinationRefusesWhenEveryCandidateIsForeign(t *testing.T) {
	userApps := t.TempDir()
	newFakeDarwinBundleAs(t, userApps, defaultDarwinBundleName, "dev", "com.aiexpedite.terminal-Dev")
	newFakeDarwinBundleAs(t, userApps, channelScopedDarwinBundleName(defaultDarwinBundleName),
		"beta", "com.aiexpedite.terminal-Beta")

	if dest, ok := darwinInstallDestination(userApps, defaultDarwinBundleName, testBundleID); ok {
		t.Fatalf("no path is free to take, yet it chose %s", dest)
	}
}

func TestWithDarwinInstallDirLockSerializesMutations(t *testing.T) {
	dir := t.TempDir()
	setDarwinInstallLockTiming(t, 30*time.Millisecond, 5*time.Millisecond)

	ran := false
	if err := withDarwinInstallDirLock(dir, func() error { ran = true; return nil }); err != nil {
		t.Fatalf("a free directory should run the mutation: %v", err)
	}
	if !ran {
		t.Fatal("mutation did not run")
	}

	// Someone else — an installer, or the updater's own swap — holds it.
	held, acquired, err := tryAcquireAgentInstanceLock(filepath.Join(dir, ".aixinstall.lock"))
	if err != nil || !acquired {
		t.Fatalf("could not simulate a holder: %v", err)
	}
	defer held.Close()

	ran = false
	if err := withDarwinInstallDirLock(dir, func() error { ran = true; return nil }); err == nil {
		t.Fatal("a held directory must not run the mutation")
	}
	if ran {
		t.Fatal("the mutation ran while another process held the destination")
	}
}
