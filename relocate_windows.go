//go:build windows

// File: relocate_windows.go
// One-time, prompt-free relocation of a machine-wide (Program Files) install to
// the per-user %LOCALAPPDATA%, so routine self-updates never need UAC. Writing
// under %LOCALAPPDATA% is a user-owned action needing no elevation. The
// relocated copy re-points this account's HKCU\...\Run startup entry and the
// installed-app registration at itself on its own boot (main() calls
// ensureAutoStart/ensureAppRegistration, both of which write os.Executable()),
// so relocation only has to copy and hand over. The legacy Program Files copy
// is removed later by a user-initiated uninstall, never from here.

package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
)

// maybeRelocateInstall copies a Program Files install to %LOCALAPPDATA%, hands
// over to the copy, and returns true so the caller exits (only one agent per
// account). Returns false when no relocation is needed (already per-user, no
// %LOCALAPPDATA%) or a copy failure leaves the legacy install launchable to
// retry next start.
func maybeRelocateInstall(_ *Config) bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	// Already per-user (or running from temp during a chained update): nothing
	// to do.
	if !isMachineWideWindowsInstall(exe) || isInTempDir(exe) {
		return false
	}

	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" {
		return false
	}
	destDir := filepath.Join(localAppData, "AIExpedite"+EnvConfigSuffix)
	destExe := filepath.Join(destDir, filepath.Base(exe))

	// A previous run may have already relocated: if the destination exe exists
	// and is not this process, hand over to it rather than copying again.
	if _, statErr := os.Stat(destExe); statErr == nil {
		if relocatedAgentIsRunning() {
			fmt.Printf("[relocate] Per-user copy already running at %s; exiting legacy copy\n", destExe)
			return true
		}
		fmt.Printf("[relocate] Per-user copy already present at %s; launching it\n", destExe)
		if err := exec.Command(destExe).Start(); err != nil {
			fmt.Printf("[relocate] Failed to launch existing per-user copy: %v\n", err)
			return false
		}
		return true
	}

	if err := copyInstallTree(filepath.Dir(exe), destDir); err != nil {
		fmt.Printf("[relocate] Copy to %s failed: %v\n", destDir, err)
		return false
	}

	fmt.Printf("[relocate] Relocated install to %s; handing over\n", destDir)
	if err := exec.Command(destExe).Start(); err != nil {
		fmt.Printf("[relocate] Failed to launch relocated copy: %v\n", err)
		return false
	}
	return true
}

// copyInstallTree copies the contents of srcDir into destDir, preserving the
// relative layout. Best-effort per file; a single failure aborts so the caller
// does not hand over to a partial install.
func copyInstallTree(srcDir, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destDir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}
