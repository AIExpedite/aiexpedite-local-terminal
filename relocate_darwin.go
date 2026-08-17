//go:build darwin

// File: relocate_darwin.go
// One-time, prompt-free relocation of a machine-wide /Applications install to
// the user-writable ~/Applications, so signed .app bundle self-updates never
// need an administrator prompt. Copying under ~/Applications is a user-owned
// action; the leftover /Applications bundle is removed only by a user-initiated
// uninstall (where one admin prompt is permitted), never from here.

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// maybeRelocateInstall relocates a /Applications install to ~/Applications on
// first run of the migration-capable build, hands over to the copy, and returns
// true so the caller exits (only one agent per account). Returns false when no
// relocation is needed (already per-user, running from another location, or an
// unrecoverable copy error leaves the legacy install launchable to retry next
// start).
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
	// Only relocate FROM the machine-wide /Applications. Any other location
	// (mounted DMG, Downloads, ...) is handled as unsupported by installUpdatable.
	if !strings.HasPrefix(bundle, "/Applications/") {
		return false
	}

	dest := filepath.Join(userApps, filepath.Base(bundle))
	if err := os.MkdirAll(userApps, 0o755); err != nil {
		fmt.Printf("[relocate] Cannot create %s: %v\n", userApps, err)
		return false
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
	if err := os.Rename(staged, dest); err != nil {
		if info, statErr := os.Stat(dest); statErr == nil && info.IsDir() {
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

	bundleName := EnvDisplayName + ".app"
	if runningBundle != "" {
		bundleName = filepath.Base(runningBundle)
	}

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

