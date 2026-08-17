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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/getlantern/systray"
	"golang.org/x/sys/unix"
)

var (
	acquireAgentInstanceAfterDarwinHandoff = acquireAgentInstance
	quitAfterDarwinHandoff                 = systray.Quit
	extractDarwinTeamID                    = darwinTeamID
)

// atomicSwapDarwin performs an atomic rename swap between two paths on Darwin
// using renameatx_np(AT_FDCWD, path1, AT_FDCWD, path2, RENAME_SWAP).
func atomicSwapDarwin(path1, path2 string) error {
	return unix.Renameatx(unix.AT_FDCWD, path1, unix.AT_FDCWD, path2, unix.RENAME_SWAP)
}

func swapDarwinBundles(currentBundle, staged, backup string) error {
	_ = os.RemoveAll(backup)
	// Try atomic filesystem exchange first (APFS / macOS 10.12+).
	// This leaves zero window where currentBundle is absent.
	if err := atomicSwapDarwin(staged, currentBundle); err == nil {
		if err := os.Rename(staged, backup); err != nil {
			_ = os.RemoveAll(staged)
		}
		return nil
	}

	// Fallback to two-step rename if atomic exchange is unavailable:
	if err := os.Rename(currentBundle, backup); err != nil {
		return fmt.Errorf("cannot move current bundle aside: %w", err)
	}
	if err := os.Rename(staged, currentBundle); err != nil {
		if rbErr := os.Rename(backup, currentBundle); rbErr != nil {
			return fmt.Errorf("swap failed (%v) AND rollback failed (%v)", err, rbErr)
		}
		return fmt.Errorf("cannot move new bundle into place: %w", err)
	}
	return nil
}

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
	if err := verifyDarwinSigningTeam(currentBundle, newBundle); err != nil {
		return err
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

	// Swap: move the old bundle aside, then move the staged one into place. Keep
	// the old bundle until LaunchServices accepts the relaunch so a rejected
	// handoff can restore the version that is still running.
	backup := filepath.Join(parent, ".aixupd_old_"+filepath.Base(currentBundle))
	if err := swapDarwinBundles(currentBundle, staged, backup); err != nil {
		return err
	}
	// Re-point the LaunchAgent at the (unchanged-path but freshly written)
	// bundle so its ProgramArguments stay valid, then relaunch and quit.
	_ = ensureAutoStart()

	fmt.Printf("[update] Replaced macOS bundle at %s; relaunching\n", currentBundle)
	// The replacement must be able to acquire the per-account singleton as
	// soon as LaunchServices starts it. The deferred release in main otherwise
	// runs only after systray exits, causing the new process to lose the lock
	// race and exit before this process quits too.
	releaseAgentInstanceForHandoff()
	if err := relaunchDarwinBundle(currentBundle); err != nil {
		return handleFailedDarwinRelaunch(currentBundle, backup, err)
	}
	_ = os.RemoveAll(backup)
	// The bundle has already been replaced and relaunched, so tray exit must
	// skip the ordinary offline notification and subprocess teardown. Using a
	// handoff marker with no artifact path also prevents onTrayExit from trying
	// to launch a second updater.
	SetUpdateHandoff()
	quitAfterDarwinHandoff()
	return nil
}

// handleFailedDarwinRelaunch restores singleton ownership when the old process
// can safely remain authoritative. If another process acquired the singleton
// during the intentionally unlocked LaunchServices window, this process must
// yield instead of reopening admission and serving concurrently.
func handleFailedDarwinRelaunch(bundle, backup string, relaunchErr error) error {
	if release, acquired := acquireAgentInstanceAfterDarwinHandoff(); acquired {
		trackAgentInstanceRelease(release)
		if err := restoreDarwinBundleBackup(bundle, backup); err != nil {
			return fmt.Errorf("failed to relaunch %s (%v) and could not restore the previous bundle: %w", bundle, relaunchErr, err)
		}
		return fmt.Errorf("failed to relaunch %s: %w", bundle, relaunchErr)
	}

	// Another process owns the singleton, so the replacement did start despite
	// the LaunchServices error. It is authoritative and the old backup is no
	// longer needed.
	_ = os.RemoveAll(backup)
	SetUpdateHandoff()
	quitAfterDarwinHandoff()
	return nil
}

// restoreDarwinBundleBackup replaces the newly installed bundle with the
// previous complete bundle after a failed relaunch. The rejected bundle is
// moved aside first so a failed restore can put it back at the installed path.
func restoreDarwinBundleBackup(bundle, backup string) error {
	failed := filepath.Join(filepath.Dir(bundle), ".aixupd_failed_"+filepath.Base(bundle))
	_ = os.RemoveAll(failed)
	if err := os.Rename(bundle, failed); err != nil {
		return fmt.Errorf("cannot move rejected bundle aside: %w", err)
	}
	if err := os.Rename(backup, bundle); err != nil {
		if rbErr := os.Rename(failed, bundle); rbErr != nil {
			return fmt.Errorf("cannot restore backup (%v) and cannot restore rejected bundle (%v)", err, rbErr)
		}
		return fmt.Errorf("cannot restore backup: %w", err)
	}
	_ = os.RemoveAll(failed)
	return nil
}

func relaunchDarwinBundle(bundle string) error {
	out, err := exec.Command("open", "-n", bundle, "--args", updateAppliedArg).CombinedOutput()
	if err != nil {
		return fmt.Errorf("open: %v: %s", err, strings.TrimSpace(string(out)))
	}
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

// verifyDarwinSigningTeam verifies that both the running bundle and the candidate
// update bundle have valid Developer ID signing team identifiers and that they match.
// If either bundle lacks a team identifier, extraction fails, or the teams do
// not match, an error is returned to prevent unauthorized bundle replacement.
func verifyDarwinSigningTeam(currentBundle, newBundle string) error {
	currentTeam, err := extractDarwinTeamID(currentBundle)
	if err != nil {
		return fmt.Errorf("installed bundle team verification failed: %w", err)
	}
	if currentTeam == "" {
		return errors.New("installed bundle has empty signing team identifier")
	}
	newTeam, err := extractDarwinTeamID(newBundle)
	if err != nil {
		return fmt.Errorf("update bundle team verification failed: %w", err)
	}
	if newTeam == "" {
		return errors.New("update bundle has empty signing team identifier")
	}
	if newTeam != currentTeam {
		return fmt.Errorf("signing team mismatch: installed=%s update=%s", currentTeam, newTeam)
	}
	return nil
}

// parseDarwinTeamID extracts the Developer ID team identifier from `codesign -dv` output.
func parseDarwinTeamID(codesignOutput string) (string, error) {
	for _, line := range strings.Split(codesignOutput, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "TeamIdentifier=") {
			team := strings.TrimSpace(strings.TrimPrefix(line, "TeamIdentifier="))
			if team == "" || team == "not set" {
				return "", errors.New("empty or unset TeamIdentifier in signature")
			}
			return team, nil
		}
	}
	return "", errors.New("TeamIdentifier not found in signature")
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
	team, err := parseDarwinTeamID(string(out))
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return team, nil
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
