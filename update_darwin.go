//go:build darwin

// File: update_darwin.go
// macOS silent self-update: replace the installed, signed .app bundle in place
// from the downloaded DMG, then relaunch. Because the supported install
// location (~/Applications, or /Applications before relocation) is user-owned,
// the swap needs no administrator prompt, and a Developer ID + notarized +
// stapled bundle re-launches with no Gatekeeper warning.
//
// The DMG is NEVER copied over the running Mach-O — we attach it read-only,
// validate the extracted .app's signature/team/notarization, stage a copy next
// to the install bundle, and swap. At every instant at least one complete,
// launchable bundle exists on disk (the old one until the new one is fully
// staged and moved into place).

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/getlantern/systray"
)

// applyVerifiedUpdate replaces the running .app bundle with the one inside the
// verified DMG at dmgPath and relaunches. dmgPath is the provenance-verified
// artifact from downloadAndVerifyUpdate.
func applyVerifiedUpdate(dmgPath string, _ *UpdateInfo) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine own path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	currentBundle := darwinBundlePath(exe)
	if currentBundle == "" {
		return fmt.Errorf("not running from a .app bundle (exe=%s); cannot replace bundle", exe)
	}

	// Attach the DMG read-only with no Finder window.
	mountPoint, err := os.MkdirTemp("", "aixupd_mount_*")
	if err != nil {
		return fmt.Errorf("cannot create mount point: %w", err)
	}
	defer os.RemoveAll(mountPoint)
	if err := runDarwinCmd("hdiutil", "attach", "-nobrowse", "-readonly",
		"-mountpoint", mountPoint, dmgPath); err != nil {
		return fmt.Errorf("hdiutil attach failed: %w", err)
	}
	defer func() {
		// Best-effort detach; -force so a lingering handle doesn't strand it.
		_ = runDarwinCmd("hdiutil", "detach", "-force", mountPoint)
	}()

	// Locate the .app inside the mounted image.
	newBundle, err := findAppBundle(mountPoint)
	if err != nil {
		return err
	}

	// Validate the new bundle BEFORE it can replace anything: a valid Developer
	// ID signature (deep + strict), Gatekeeper acceptance (notarization +
	// stapled ticket), and the SAME signing team as the currently-installed
	// bundle. Requiring team equality means an update must be signed by the
	// same team as the running app without hardcoding a team id here.
	if err := verifyDarwinBundleSignature(newBundle); err != nil {
		return fmt.Errorf("new bundle failed signature validation: %w", err)
	}
	if err := verifyDarwinGatekeeper(newBundle); err != nil {
		return fmt.Errorf("new bundle failed Gatekeeper assessment: %w", err)
	}
	currentTeam, curErr := darwinTeamID(currentBundle)
	newTeam, newErr := darwinTeamID(newBundle)
	if curErr == nil && newErr == nil && currentTeam != "" && newTeam != currentTeam {
		return fmt.Errorf("signing team mismatch: installed=%s update=%s", currentTeam, newTeam)
	}

	// Stage a copy of the new bundle next to the install location, preserving
	// signature/metadata with ditto. Staging on the same volume makes the final
	// swap a fast rename.
	parent := filepath.Dir(currentBundle)
	staged := filepath.Join(parent, ".aixupd_staged_"+filepath.Base(currentBundle))
	_ = os.RemoveAll(staged)
	if err := runDarwinCmd("ditto", newBundle, staged); err != nil {
		return fmt.Errorf("staging copy failed: %w", err)
	}
	defer os.RemoveAll(staged) // no-op if the rename below consumed it

	// Swap: move the old bundle aside, move the staged one into place, then
	// remove the old. If the process dies between the two renames, the ".old"
	// bundle is still a complete launchable copy.
	backup := filepath.Join(parent, ".aixupd_old_"+filepath.Base(currentBundle))
	_ = os.RemoveAll(backup)
	if err := os.Rename(currentBundle, backup); err != nil {
		return fmt.Errorf("cannot move current bundle aside: %w", err)
	}
	if err := os.Rename(staged, currentBundle); err != nil {
		// Roll back so we are never left without a bundle at the install path.
		if rbErr := os.Rename(backup, currentBundle); rbErr != nil {
			return fmt.Errorf("swap failed (%v) AND rollback failed (%v)", err, rbErr)
		}
		return fmt.Errorf("cannot move new bundle into place: %w", err)
	}
	_ = os.RemoveAll(backup)

	// Re-point the LaunchAgent at the (unchanged-path but freshly written)
	// bundle so its ProgramArguments stay valid, then relaunch and quit.
	_ = ensureAutoStart()

	fmt.Printf("[update] Replaced macOS bundle at %s; relaunching\n", currentBundle)
	// The replacement must be able to acquire the per-account singleton as
	// soon as LaunchServices starts it. The deferred release in main otherwise
	// runs only after systray exits, causing the new process to lose the lock
	// race and exit before this process quits too.
	releaseAgentInstanceForHandoff()
	if err := exec.Command("open", "-n", currentBundle).Start(); err != nil {
		return fmt.Errorf("failed to relaunch %s: %w", currentBundle, err)
	}
	// The bundle has already been replaced and relaunched, so tray exit must
	// skip the ordinary offline notification and subprocess teardown. Using a
	// handoff marker with no artifact path also prevents onTrayExit from trying
	// to launch a second updater.
	SetUpdateHandoff()
	systray.Quit()
	return nil
}

// findAppBundle returns the single top-level *.app inside dir.
func findAppBundle(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("cannot read mounted image: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".app") {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("no .app bundle found in %s", dir)
}

// verifyDarwinBundleSignature runs codesign --verify --deep --strict.
func verifyDarwinBundleSignature(bundle string) error {
	return runDarwinCmd("codesign", "--verify", "--deep", "--strict", "--verbose=2", bundle)
}

// verifyDarwinGatekeeper asserts the bundle passes Gatekeeper (notarized +
// stapled), which is what prevents a first-launch security warning after the
// update.
func verifyDarwinGatekeeper(bundle string) error {
	return runDarwinCmd("spctl", "--assess", "--type", "execute", "--verbose=2", bundle)
}

// darwinTeamID extracts the Developer ID team identifier from a bundle's
// signature via `codesign -dv`.
func darwinTeamID(bundle string) (string, error) {
	cmd := exec.Command("codesign", "-dv", "--verbose=4", bundle)
	// codesign writes its detail to stderr.
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("codesign -dv failed: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "TeamIdentifier=") {
			return strings.TrimSpace(strings.TrimPrefix(line, "TeamIdentifier=")), nil
		}
	}
	return "", fmt.Errorf("TeamIdentifier not found in signature")
}

// runDarwinCmd runs a command and returns a wrapped error including any output
// on failure, so update logs explain WHY a validation step rejected a bundle.
func runDarwinCmd(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
