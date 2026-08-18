//go:build darwin

// File: relocate_darwin.go
// Prompt-free placement of the app in the user-writable ~/Applications, so
// signed .app bundle self-updates never need an administrator prompt. Two
// entry points, both running before the tray starts:
//
//   - launched from the release DMG (or the App Translocation mirror macOS
//     substitutes for a quarantined image): install into ~/Applications and
//     hand over. The DMG intentionally ships NO /Applications shortcut — a
//     machine-wide copy needs an admin prompt and cannot be replaced silently
//     by the auto-updater — so double-clicking the app IS the installation.
//   - one-time relocation of a legacy machine-wide /Applications install.
//
// Copying under ~/Applications is a user-owned action; the leftover
// /Applications bundle is removed only by a user-initiated uninstall (where one
// admin prompt is permitted), never from here.

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const defaultDarwinBundleName = "AIExpediteTerminal.app"

// currentDarwinBundleName returns the basename of the running .app bundle
// (e.g. "AIExpediteTerminal.app"), falling back to defaultDarwinBundleName if
// the process was not started from a .app bundle.
func currentDarwinBundleName() string {
	exe, err := os.Executable()
	if err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		if bundle := darwinBundlePath(exe); bundle != "" {
			return filepath.Base(bundle)
		}
	}
	return defaultDarwinBundleName
}

// recoverInterruptedDarwinInstall checks for orphaned backup/staged bundles for
// the current bundle name in ~/Applications and /Applications and restores them if the
// main bundle was left missing after an interrupted update.
func recoverInterruptedDarwinInstall(bundleName string) {
	if bundleName == "" {
		bundleName = currentDarwinBundleName()
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		recoverDarwinAppBundle(filepath.Join(home, "Applications"), bundleName)
	}
	recoverDarwinAppBundle("/Applications", bundleName)
}

func recoverDarwinAppBundle(dir, bundleName string) {
	if bundleName == "" {
		return
	}
	targetPath := filepath.Join(dir, bundleName)
	backupPath := filepath.Join(dir, ".aixupd_old_"+bundleName)
	stagedPath := filepath.Join(dir, ".aixupd_staged_"+bundleName)

	targetExists := false
	if _, err := os.Stat(targetPath); err == nil {
		targetExists = true
	}

	if !targetExists {
		// Prefer restoring backup, then staged if backup is absent
		if _, err := os.Stat(backupPath); err == nil {
			if err := os.Rename(backupPath, targetPath); err == nil {
				targetExists = true
			}
		} else if _, err := os.Stat(stagedPath); err == nil {
			if err := os.Rename(stagedPath, targetPath); err == nil {
				targetExists = true
			}
		}
	}

	// Clean up stale leftovers belonging strictly to this channel once target is in place
	if targetExists {
		_ = os.RemoveAll(backupPath)
		_ = os.RemoveAll(stagedPath)
	}
}

// maybeRelocateInstall relocates a /Applications install to ~/Applications on
// first run of the migration-capable build, hands over to the copy, and returns
// true so the caller exits (only one agent per account). Returns false when no
// relocation is needed (already per-user, running from another location, or an
// unrecoverable copy error leaves the legacy install launchable to retry next
// start).
// recoverInterruptedInstall restores or cleans leftover Darwin update
// artifacts for this channel. It must run only after the per-account
// singleton is held so a losing same-channel launcher cannot delete the
// staged/backup bundles an in-flight update still owns.
func recoverInterruptedInstall() {
	recoverInterruptedDarwinInstall("")
}

func maybeRelocateInstall(_ *Config) bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	bundle := darwinBundlePath(exe)
	if bundle == "" {
		return false // not a bundled app (e.g. `go run`) — nothing to relocate
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	userApps := filepath.Join(home, "Applications")

	// Already running per-user: done.
	if bundle == userApps || strings.HasPrefix(bundle, userApps+string(filepath.Separator)) {
		return false
	}
	// Launched straight out of the release DMG: this IS the installation.
	// Copy into ~/Applications, hand over to the copy, and let this process
	// exit, so the user never has to drag anything anywhere.
	if runningFromDarwinInstallMedia(bundle) {
		return installDarwinFromMedia(bundle, exe, userApps)
	}
	// Otherwise only relocate FROM the machine-wide /Applications. Any other
	// location (Downloads, an ad-hoc path, ...) is left alone and reported as
	// unsupported by installUpdatable.
	if !strings.HasPrefix(bundle, "/Applications/") {
		return false
	}

	if err := os.MkdirAll(userApps, 0o755); err != nil {
		fmt.Printf("[relocate] Cannot create %s: %v\n", userApps, err)
		return false
	}
	// Same destination, same lock as the DMG install: without it a concurrent
	// image install can land its bundle between the ownership decision below
	// and the move that acts on it.
	unlockDir, dirLocked := waitForDarwinInstallDirLock(userApps)
	if !dirLocked {
		fmt.Printf("[relocate] %s stayed busy for %s; leaving the machine-wide copy in place\n",
			userApps, darwinInstallLockWait)
		return false
	}
	defer unlockDir()

	// Same shared-bundle-name hazard as the DMG install: every channel packages
	// AIExpediteTerminal.app, so a legacy machine-wide install must not land on
	// top of a different channel's per-user bundle.
	sourceID, err := darwinBundleIdentifier(bundle)
	if err != nil {
		fmt.Printf("[relocate] Cannot read the bundle's identifier: %v\n", err)
	}
	dest, ok := darwinInstallDestination(userApps, filepath.Base(bundle), sourceID)
	if !ok {
		return false // another channel owns every candidate path; stay put
	}
	rel, relErr := filepath.Rel(bundle, exe)
	newExe := ""
	if relErr == nil {
		newExe = filepath.Join(dest, rel)
	}

	// A previous run may already have relocated and silently updated this
	// account. Never replace that authoritative per-user bundle with the stale
	// machine-wide copy when an old Dock shortcut launches /Applications again;
	// validate the destination binary and hand over to the existing destination.
	if newExe != "" {
		if exeInfo, err := os.Stat(newExe); err == nil && !exeInfo.IsDir() {
			fmt.Printf("[relocate] Per-user bundle already present at %s; handing over\n", dest)
			// This is the destination bundle while the source bundle with the same ID
			// is still running. Force LaunchServices to start a new destination
			// instance; ordinary `open` merely activates the source and leaves no
			// process after we exit. The per-account singleton acquired during
			// startup rejects any actual duplicate.
			if err := launchRelocatedDarwinBundle(dest, true); err != nil {
				fmt.Printf("[relocate] Failed to launch existing per-user copy: %v\n", err)
				return false
			}
			if err := writeDarwinLaunchAgentFor(newExe); err != nil {
				fmt.Printf("[relocate] Could not re-point LaunchAgent (non-fatal): %v\n", err)
			}
			return true
		}
	}

	// Stage then swap so an interrupted copy never leaves a half-written bundle
	// at the destination — the legacy /Applications bundle stays launchable.
	staged := fmt.Sprintf("%s.aixupd_new-%d", dest, os.Getpid())
	_ = os.RemoveAll(staged)
	defer os.RemoveAll(staged)
	if err := exec.Command("ditto", bundle, staged).Run(); err != nil {
		fmt.Printf("[relocate] ditto copy failed: %v\n", err)
		return false
	}
	// Create-only, and hand over ONLY to this channel's own bundle: a directory
	// at dest is not proof that a relocation of THIS channel won the race, and
	// launching another channel's app would start the wrong agent.
	if err := exclusiveRenameDarwin(staged, dest); err != nil {
		if errors.Is(err, unix.EEXIST) &&
			darwinDestinationState(dest, sourceID) == darwinDestinationOurs {
			fmt.Printf("[relocate] Concurrent relocation won by another launcher; handing over to %s\n", dest)
			if err := launchRelocatedDarwinBundle(dest, true); err != nil {
				fmt.Printf("[relocate] Failed to launch concurrent per-user copy: %v\n", err)
				return false
			}
			return true
		}
		fmt.Printf("[relocate] cannot move relocated bundle into place: %v\n", err)
		return false
	}

	// Re-point the login LaunchAgent at the relocated binary (its path derives
	// from the bundle, so relocation must rewrite it).
	if newExe != "" {
		if err := writeDarwinLaunchAgentFor(newExe); err != nil {
			fmt.Printf("[relocate] Could not re-point LaunchAgent (non-fatal): %v\n", err)
		}
	}

	fmt.Printf("[relocate] Relocated %s -> %s; handing over\n", bundle, dest)
	// This is a newly copied bundle while the source bundle with the same ID is
	// still running. Force LaunchServices to start the destination; ordinary
	// `open` may merely activate the source and leave no process after we exit.
	// The per-account singleton acquired during destination startup prevents a
	// lasting duplicate if simultaneous relocation launchers race here.
	if err := launchRelocatedDarwinBundle(dest, true); err != nil {
		fmt.Printf("[relocate] Failed to launch relocated copy: %v\n", err)
		return false
	}
	return true
}

// acquireAgentInstanceForInstall takes the per-account singleton around a
// bundle replacement; replaced in tests.
var acquireAgentInstanceForInstall = acquireAgentInstance

// runHdiutilInfo returns `hdiutil info` output; replaced in tests.
var runHdiutilInfo = func() (string, error) {
	out, err := exec.Command("hdiutil", "info").Output()
	return string(out), err
}

// runningFromDarwinInstallMedia reports whether bundle is running from install
// media rather than an installed location.
func runningFromDarwinInstallMedia(bundle string) bool {
	lower := strings.ToLower(filepath.ToSlash(bundle))
	// App Translocation. macOS has already decided this bundle came out of a
	// quarantined archive or image and mirrored it onto a randomized read-only
	// path that vanishes when the app quits — there is nothing to keep running
	// from and nothing that could ever self-update, whatever the origin was.
	if strings.Contains(lower, "/apptranslocation/") {
		return true
	}
	// A /Volumes path on its own proves nothing: USB sticks, network shares and
	// external disks mount there too, and quietly installing an app someone
	// deliberately runs off portable media is not this code's call. Only an
	// attached disk image — the release DMG — counts as install media.
	volume := darwinVolumeRoot(bundle)
	return volume != "" && darwinVolumeIsDiskImage(volume)
}

// darwinVolumeIsDiskImage reports whether volume is the mount point of an
// attached disk image. It fails CLOSED: without proof that the volume is an
// image, the app keeps running where it is instead of installing itself.
func darwinVolumeIsDiskImage(volume string) bool {
	out, err := runHdiutilInfo()
	if err != nil {
		fmt.Printf("[install] Cannot tell whether %s is a disk image (%v); leaving the app where it is\n", volume, err)
		return false
	}
	for _, mount := range darwinDiskImageMountPoints(out) {
		if mount == volume {
			return true
		}
	}
	return false
}

// darwinDiskImageMountPoints extracts the mount points of every attached disk
// image from `hdiutil info` output. Each image lists its partitions as
// tab-separated entity lines whose last field is the mount point:
//
//	/dev/disk4          \tGUID_partition_scheme          \t
//	/dev/disk4s1        \t48465300-0000-11AA-AA11-...    \t/Volumes/AI Expedite
func darwinDiskImageMountPoints(hdiutilInfo string) []string {
	var mounts []string
	for _, line := range strings.Split(hdiutilInfo, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "/dev/") {
			continue // a key-value line (image-path, writeable, ...)
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			continue
		}
		mount := strings.TrimSpace(fields[len(fields)-1])
		if strings.HasPrefix(mount, "/") {
			mounts = append(mounts, mount)
		}
	}
	return mounts
}

// darwinBundleIdentifier reads CFBundleIdentifier out of a bundle's Info.plist;
// replaced in tests.
var darwinBundleIdentifier = func(bundle string) (string, error) {
	out, err := exec.Command("plutil", "-extract", "CFBundleIdentifier", "raw", "-o", "-",
		filepath.Join(bundle, "Contents", "Info.plist")).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// darwinOccupancy describes what already sits at a candidate install path.
type darwinOccupancy int

const (
	darwinDestinationFree darwinOccupancy = iota
	darwinDestinationOurs
	darwinDestinationForeign
)

// darwinDestinationState classifies path for an install whose bundle
// identifier is sourceID. An identity that cannot be established counts as
// FOREIGN: a bundle whose ownership is unknown is never destroyed.
func darwinDestinationState(path, sourceID string) darwinOccupancy {
	if _, err := os.Stat(path); err != nil {
		return darwinDestinationFree
	}
	if sourceID == "" {
		return darwinDestinationForeign
	}
	id, err := darwinBundleIdentifier(path)
	if err != nil || id != sourceID {
		return darwinDestinationForeign
	}
	return darwinDestinationOurs
}

// channelScopedDarwinBundleName appends this channel's suffix to a bundle name
// ("AIExpediteTerminal.app" -> "AIExpediteTerminal-Dev.app").
func channelScopedDarwinBundleName(bundleName string) string {
	suffix := EnvConfigSuffix
	if suffix == "" {
		suffix = "-Prod"
	}
	base := strings.TrimSuffix(bundleName, ".app")
	if strings.HasSuffix(base, suffix) {
		return bundleName
	}
	return base + suffix + ".app"
}

// darwinInstallDestination picks where a bundle with identifier sourceID
// installs to. Every channel packages the app as AIExpediteTerminal.app while
// being otherwise fully separate — own bundle id, own config dir, own
// singleton, own LaunchAgent label — so that shared name is the one place a
// dev or beta image could silently overwrite a prod install whose lock this
// process never even contends for. Another channel's bundle is never touched:
// the install falls back to a channel-scoped name and the two coexist.
func darwinInstallDestination(userApps, bundleName, sourceID string) (string, bool) {
	// Prefer wherever this channel already lives, so an install that once fell
	// back to the scoped name does not fork into a second copy later when the
	// plain name frees up.
	scoped := filepath.Join(userApps, channelScopedDarwinBundleName(bundleName))
	scopedState := darwinDestinationState(scoped, sourceID)
	if scopedState == darwinDestinationOurs {
		return scoped, true
	}
	preferred := filepath.Join(userApps, bundleName)
	if darwinDestinationState(preferred, sourceID) != darwinDestinationForeign {
		return preferred, true
	}
	if scopedState == darwinDestinationForeign {
		// Both candidates belong to someone else. There is no path left that
		// can be taken without destroying a bundle this build does not own,
		// so nothing is installed.
		fmt.Printf("[install] %s and %s both belong to another AI Expedite channel; not installing\n",
			preferred, scoped)
		return "", false
	}
	fmt.Printf("[install] %s belongs to another AI Expedite channel; installing alongside it at %s\n",
		preferred, scoped)
	return scoped, true
}

// lockDarwinInstallDir serializes every installer targeting dir, whatever
// channel it belongs to. The per-account singleton cannot do this job — each
// channel holds a different one — so without it two fresh installers can both
// see a destination as free and the loser can land on top of the winner. The
// ownership checks below are only meaningful while this is held.
func lockDarwinInstallDir(dir string) (unlock func(), acquired bool) {
	file, acquired, err := tryAcquireAgentInstanceLock(filepath.Join(dir, ".aixinstall.lock"))
	if err != nil {
		// Cannot create the lock at all (read-only dir, exotic filesystem):
		// fall through rather than block an otherwise fine install.
		fmt.Printf("[install] Could not take the install lock in %s (continuing): %v\n", dir, err)
		return func() {}, true
	}
	if !acquired {
		return func() {}, false
	}
	return func() {
		_ = unlockFile(file)
		_ = file.Close()
	}, true
}

var (
	darwinInstallLockWait = 30 * time.Second
	darwinInstallLockPoll = 250 * time.Millisecond
)

// waitForDarwinInstallDirLock takes the destination lock, waiting a concurrent
// installer out rather than assuming it is doing this channel's work: an
// installer for a DIFFERENT channel places a different bundle and would leave
// this one uninstalled. Bounded, so a stuck holder can never keep the app from
// starting.
func waitForDarwinInstallDirLock(dir string) (unlock func(), acquired bool) {
	deadline := time.Now().Add(darwinInstallLockWait)
	for {
		unlock, ok := lockDarwinInstallDir(dir)
		if ok {
			return unlock, true
		}
		if !time.Now().Before(deadline) {
			return func() {}, false
		}
		time.Sleep(darwinInstallLockPoll)
	}
}

// darwinPlacement is the outcome of moving a staged bundle into place.
type darwinPlacement int

const (
	darwinPlacementCreated  darwinPlacement = iota // nothing was there
	darwinPlacementReplaced                        // this channel's copy was swapped out
	darwinPlacementYielded                         // another launcher of THIS channel won
	darwinPlacementBlocked                         // another channel's bundle is there
	darwinPlacementFailed
)

// placeDarwinBundle moves staged to dest. It always tries create-first, so a
// bundle that appeared since the destination was chosen is never silently
// replaced, and it re-reads ownership immediately before any swap: the choice
// made before the copy is stale by now, and only a bundle re-verified as this
// channel's — with the singleton held — may be replaced.
func placeDarwinBundle(staged, dest, backup, sourceID string, acquired bool) darwinPlacement {
	err := exclusiveRenameDarwin(staged, dest)
	if err == nil {
		return darwinPlacementCreated
	}
	if !errors.Is(err, unix.EEXIST) {
		fmt.Printf("[install] Cannot move installed bundle into place: %v\n", err)
		return darwinPlacementFailed
	}
	if state := darwinDestinationState(dest, sourceID); state != darwinDestinationOurs {
		fmt.Printf("[install] %s now belongs to another AI Expedite channel; leaving it in place\n", dest)
		return darwinPlacementBlocked
	}
	if !acquired {
		// Without the singleton this install may only CREATE: a launcher that
		// won the race may already be running out of that bundle.
		fmt.Printf("[install] Another launcher installed %s first; leaving it in place\n", dest)
		return darwinPlacementYielded
	}
	if err := swapDarwinBundles(dest, staged, backup); err != nil {
		fmt.Printf("[install] Cannot replace existing install at %s: %v\n", dest, err)
		return darwinPlacementFailed
	}
	return darwinPlacementReplaced
}

// exclusiveRenameDarwin renames src to dst only when dst does not exist
// (renameatx_np with RENAME_EXCL), reporting unix.EEXIST otherwise. os.Rename
// cannot express this — it replaces an empty directory at dst — and a lockless
// install must never take over a path another launcher just created.
var exclusiveRenameDarwin = func(src, dst string) error {
	p1, err := unix.BytePtrFromString(src)
	if err != nil {
		return err
	}
	p2, err := unix.BytePtrFromString(dst)
	if err != nil {
		return err
	}
	fd := unix.AT_FDCWD
	atFdCwd := uintptr(fd)
	_, _, errno := unix.Syscall6(
		unix.SYS_RENAMEATX_NP,
		atFdCwd,
		uintptr(unsafe.Pointer(p1)),
		atFdCwd,
		uintptr(unsafe.Pointer(p2)),
		uintptr(unix.RENAME_EXCL),
		0,
	)
	switch errno {
	case 0:
		return nil
	case unix.ENOTSUP, unix.EINVAL:
		// Filesystem without RENAME_EXCL: check-then-rename. The window this
		// reopens is the one the flag exists to close, so keep the check as
		// tight as possible.
		if _, err := os.Stat(dst); err == nil {
			return unix.EEXIST
		}
		return os.Rename(src, dst)
	default:
		return errno
	}
}

// installDarwinFromMedia copies the bundle running off the DMG into
// ~/Applications, re-points auto-start at the installed copy, launches it and
// returns true so the caller exits. Returns false when the install cannot be
// completed — the app then keeps running from the image (without self-update)
// rather than refusing to start.
func installDarwinFromMedia(bundle, exe, userApps string) bool {
	if err := os.MkdirAll(userApps, 0o755); err != nil {
		fmt.Printf("[install] Cannot create %s: %v\n", userApps, err)
		return false
	}

	sourceID, err := darwinBundleIdentifier(bundle)
	if err != nil {
		fmt.Printf("[install] Cannot read the image bundle's identifier: %v\n", err)
	}

	// Serialize with any other AI Expedite installer targeting this directory,
	// whichever channel it belongs to. Every ownership check below reads the
	// destination and acts on it; without this lock another installer can land
	// a bundle in between.
	unlockDir, dirLocked := waitForDarwinInstallDirLock(userApps)
	if !dirLocked {
		// Waited out: the holder may be installing a different channel, which
		// would leave this one uninstalled, so exiting here is not an option.
		fmt.Printf("[install] %s stayed busy for %s; not installing\n", userApps, darwinInstallLockWait)
		notifyDarwinInstallBusy()
		return false
	}
	defer unlockDir()

	dest, ok := darwinInstallDestination(userApps, filepath.Base(bundle), sourceID)
	if !ok {
		notifyDarwinInstallBlocked()
		return false
	}

	// Replacing an existing install must not race the installed agent, whose
	// own updater swaps exactly this bundle from applyVerifiedUpdate. The
	// per-account singleton is that mutual exclusion — and because dest is
	// this channel's own bundle, it is the lock that actually governs it.
	// main.go only takes it AFTER this path runs, so take it here and hold it
	// across the swap. It has to be released again before the replacement is
	// launched (and on every error return, or main's own acquisition below
	// would find the lock held by this very process and exit).
	release, acquired := acquireAgentInstanceForInstall()
	releaseOnce := sync.Once{}
	releaseLock := func() {
		if acquired {
			releaseOnce.Do(release)
		}
	}
	defer releaseLock()

	if _, err := os.Stat(dest); err == nil && !acquired {
		// Another agent owns this install and may be mid-update. Its bundle is
		// not ours to overwrite, and a second agent must not run alongside it.
		fmt.Printf("[install] %s is already installed and running; leaving it untouched\n", dest)
		notifyDarwinInstallSkipped()
		scheduleDarwinVolumeEject(bundle)
		return true
	}

	// Stage beside the destination (same volume, so the move below is a
	// rename) and only then put it in place: an interrupted copy can never
	// leave a half-written bundle installed. The pid suffix keeps two
	// simultaneous installers off each other's staging directory, and the
	// prefix stays distinct from the updater's .aixupd_* artifacts so the
	// startup recovery pass can tell the two apart.
	staged := fmt.Sprintf("%s.aixinstall_new-%d", dest, os.Getpid())
	_ = os.RemoveAll(staged)
	defer os.RemoveAll(staged)
	if err := exec.Command("ditto", bundle, staged).Run(); err != nil {
		fmt.Printf("[install] ditto copy failed: %v\n", err)
		return false
	}
	// The image is quarantined and ditto preserves that attribute. Clearing it
	// on our own copy keeps LaunchServices from re-running the first-launch
	// warning for a bundle the user just opened, and from translocating the
	// installed copy to a read-only path where it could not self-update.
	_ = exec.Command("xattr", "-d", "-r", "com.apple.quarantine", staged).Run()

	backup := filepath.Join(userApps, ".aixinstall_old_"+filepath.Base(dest))
	replaced := false
	switch placeDarwinBundle(staged, dest, backup, sourceID, acquired) {
	case darwinPlacementCreated:
	case darwinPlacementReplaced:
		replaced = true
	case darwinPlacementYielded:
		// This channel is installed and running from dest, just not by us.
		notifyDarwinInstallSkipped()
		scheduleDarwinVolumeEject(bundle)
		return true
	case darwinPlacementBlocked:
		// Nothing was installed: keep running from the image rather than
		// exiting into nothing.
		notifyDarwinInstallBlocked()
		return false
	default:
		return false
	}

	// Point the login LaunchAgent at the installed binary (its path derives
	// from the bundle, so it must be written for the destination, not the
	// image this process is running from).
	if rel, err := filepath.Rel(bundle, exe); err == nil {
		if err := writeDarwinLaunchAgentFor(filepath.Join(dest, rel)); err != nil {
			fmt.Printf("[install] Could not write LaunchAgent (non-fatal): %v\n", err)
		}
	}

	fmt.Printf("[install] Installed %s -> %s; handing over\n", bundle, dest)
	// The replacement acquires the same per-account singleton as soon as
	// LaunchServices starts it, so this process must let go of it first.
	releaseLock()
	// Force a new instance: the copy carries the same bundle identifier as
	// this running process, so a plain `open` would merely activate the image
	// copy and leave nothing behind once we exit.
	if err := launchRelocatedDarwinBundle(dest, true); err != nil {
		return handleFailedDarwinInstallLaunch(bundle, dest, backup, replaced, err)
	}
	if replaced {
		_ = os.RemoveAll(backup)
	}

	notifyDarwinInstalled(dest)
	// The image is still busy with this process, so it can only be ejected
	// after we exit — hand the detach to a child that outlives us.
	scheduleDarwinVolumeEject(bundle)
	return true
}

// handleFailedDarwinInstallLaunch resolves what a LaunchServices error actually
// means: the replacement may have started regardless, or another same-channel
// launcher may have taken the singleton during the intentionally unlocked
// handoff. Only a process that can retake the lock may roll the bundle back —
// otherwise the rollback would pull the bundle out from under the authoritative
// agent. Same ambiguity, and same resolution, as handleFailedDarwinRelaunch in
// the updater.
func handleFailedDarwinInstallLaunch(bundle, dest, backup string, replaced bool, launchErr error) bool {
	release, acquired := acquireAgentInstanceForInstall()
	if !acquired {
		fmt.Printf("[install] open reported %v, but another agent owns %s; leaving the new bundle in place\n",
			launchErr, dest)
		if replaced {
			_ = os.RemoveAll(backup)
		}
		scheduleDarwinVolumeEject(bundle)
		return true
	}
	defer release()

	fmt.Printf("[install] Failed to launch installed copy: %v\n", launchErr)
	if replaced {
		if err := restoreDarwinBundleBackup(dest, backup); err != nil {
			fmt.Printf("[install] Could not restore previous install: %v\n", err)
		}
	}
	return false
}

// notifyDarwinInstalled tells the user where the app went. The agent is an
// LSUIElement with no window, so without this a double-click on the DMG would
// look like nothing happened at all.
func notifyDarwinInstalled(dest string) {
	shown := dest
	if home, err := os.UserHomeDir(); err == nil && home != "" &&
		strings.HasPrefix(dest, home+string(filepath.Separator)) {
		shown = "~" + strings.TrimPrefix(dest, home)
	}
	notifyDarwin(fmt.Sprintf("Installed to %s and running in the menu bar.", shown))
}

// notifyDarwin posts a user notification under the channel's display name.
func notifyDarwin(body string) {
	title := EnvDisplayName
	if title == "" {
		title = "AI Expedite"
	}
	script := fmt.Sprintf("display notification %s with title %s",
		appleScriptString(body), appleScriptString(title))
	_ = exec.Command("osascript", "-e", script).Run()
}

// notifyDarwinInstallSkipped explains why double-clicking the image appeared to
// do nothing: a running agent owns the install and must be quit first.
func notifyDarwinInstallSkipped() {
	notifyDarwin("Already installed and running. Quit it from the menu bar first to install this copy.")
}

// notifyDarwinInstallBusy explains that another installer held the destination
// for too long, so this image installed nothing.
func notifyDarwinInstallBusy() {
	notifyDarwin("Could not install right now — another installer is busy. Open this image again in a moment.")
}

// notifyDarwinInstallBlocked explains that the install location is held by a
// different AI Expedite channel, which this build will not overwrite.
func notifyDarwinInstallBlocked() {
	notifyDarwin("Could not install: another AI Expedite channel occupies the Applications folder entry. Remove it and open this image again.")
}

// appleScriptString quotes s as an AppleScript string literal.
func appleScriptString(s string) string {
	escaped := strings.ReplaceAll(s, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

// darwinVolumeRoot returns the /Volumes/<name> mount a path lives on, or "" if
// it is not on a mounted volume (including the App Translocation case, where
// the originating image cannot be identified without private APIs).
func darwinVolumeRoot(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(strings.ToLower(p), "/volumes/") {
		return ""
	}
	rest := strings.TrimPrefix(p[len("/Volumes/"):], "/")
	name, _, _ := strings.Cut(rest, "/")
	if name == "" {
		return ""
	}
	return "/Volumes/" + name
}

// scheduleDarwinVolumeEject ejects the install image shortly after this process
// exits, so the Finder window the user opened closes itself once the install is
// done. Best effort: a still-busy image simply stays mounted.
func scheduleDarwinVolumeEject(bundle string) {
	volume := darwinVolumeRoot(bundle)
	if volume == "" {
		return
	}
	cmd := exec.Command("/bin/sh", "-c",
		`sleep 5; /usr/bin/hdiutil detach -force "$1" >/dev/null 2>&1`, "sh", volume)
	if err := cmd.Start(); err != nil {
		fmt.Printf("[install] Could not schedule eject of %s (non-fatal): %v\n", volume, err)
		return
	}
	// Detach from the child so it survives this process and is not waited on.
	_ = cmd.Process.Release()
}

func launchRelocatedDarwinBundle(bundle string, forceNew bool) error {
	args := []string{bundle}
	if forceNew {
		args = []string{"-n", bundle}
	}
	out, err := exec.Command("open", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("open: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// handleDarwinUninstall removes user-owned per-user bundles and prints clear removal guidance.
func handleDarwinUninstall(quiet bool) {
	exe, err := os.Executable()
	var runningBundle string
	if err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		runningBundle = darwinBundlePath(exe)
	}

	bundleName := currentDarwinBundleName()

	home, _ := os.UserHomeDir()
	var userBundle string
	if home != "" {
		userBundle = filepath.Join(home, "Applications", bundleName)
	}

	removedUserBundle := false
	if userBundle != "" {
		if info, err := os.Stat(userBundle); err == nil && info.IsDir() {
			if err := os.RemoveAll(userBundle); err == nil {
				removedUserBundle = true
				if !quiet {
					fmt.Printf("→ Removed per-user application bundle at %s\n", userBundle)
				}
			}
		}
	}

	if runningBundle != "" && runningBundle != userBundle && home != "" && strings.HasPrefix(runningBundle, home+string(filepath.Separator)) {
		if err := os.RemoveAll(runningBundle); err == nil {
			removedUserBundle = true
			if !quiet {
				fmt.Printf("→ Removed application bundle at %s\n", runningBundle)
			}
		}
	}

	if !quiet {
		systemBundle := filepath.Join("/Applications", bundleName)
		if _, err := os.Stat(systemBundle); err == nil {
			fmt.Printf("→ To remove the system-wide copy, drag %s from /Applications to the Trash.\n", bundleName)
		} else if !removedUserBundle {
			fmt.Printf("→ To finish removal, drag %s from ~/Applications or /Applications to the Trash.\n", bundleName)
		}
	}
}
