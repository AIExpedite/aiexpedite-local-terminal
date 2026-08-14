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
	// simply re-point startup and hand over to the existing destination.
	if info, err := os.Stat(dest); err == nil && info.IsDir() {
		if newExe != "" {
			if err := writeDarwinLaunchAgentFor(newExe); err != nil {
				fmt.Printf("[relocate] Could not re-point LaunchAgent (non-fatal): %v\n", err)
			}
		}
		fmt.Printf("[relocate] Per-user bundle already present at %s; handing over\n", dest)
		// Ordinary `open` activates an existing instance and launches one only
		// when needed. `open -n` would force a duplicate agent.
		if err := exec.Command("open", dest).Start(); err != nil {
			fmt.Printf("[relocate] Failed to launch existing per-user copy: %v\n", err)
			return false
		}
		return true
	}

	// Stage then swap so an interrupted copy never leaves a half-written bundle
	// at the destination — the legacy /Applications bundle stays launchable.
	staged := dest + ".aixupd_new"
	_ = os.RemoveAll(staged)
	if err := exec.Command("ditto", bundle, staged).Run(); err != nil {
		fmt.Printf("[relocate] ditto copy failed: %v\n", err)
		_ = os.RemoveAll(staged)
		return false
	}
	_ = os.RemoveAll(dest)
	if err := os.Rename(staged, dest); err != nil {
		fmt.Printf("[relocate] cannot move relocated bundle into place: %v\n", err)
		_ = os.RemoveAll(staged)
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
	if err := exec.Command("open", dest).Start(); err != nil {
		fmt.Printf("[relocate] Failed to launch relocated copy: %v\n", err)
		return false
	}
	return true
}
