//go:build !darwin

// File: update_selfreplace.go
// Windows/Linux update application via the temp-binary self-replace handoff.
// (macOS replaces the signed .app bundle instead — see update_darwin.go.)

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/getlantern/systray"
)

var (
	resolveSelfReplaceTarget  = runningUpdateTarget
	validateSelfReplaceTarget = validateUpdateTarget
	startSelfReplaceProcess   = func(path string, args []string) error {
		cmd := exec.Command(path, args...)
		setNewConsole(cmd)
		return cmd.Start()
	}
	quitAfterSelfReplaceStart = systray.Quit
)

// applyVerifiedUpdate installs a verified update on Windows and Linux. The
// verified artifact is an executable (a raw .exe on Windows or an AppImage on
// Linux). Start the updater before committing the running process to shutdown;
// if execution is blocked (for example by a noexec mount or endpoint
// protection), return an error so the current agent stays alive and routable.
func applyVerifiedUpdate(path string, _ *UpdateInfo) error {
	if err := os.Chmod(path, 0o755); err != nil {
		return fmt.Errorf("make updater executable: %w", err)
	}

	originalExe, err := resolveSelfReplaceTarget()
	if err != nil {
		return fmt.Errorf("determine update target: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(originalExe); err == nil {
		originalExe = resolved
	}

	args := []string{}
	if originalExe != "" && !isInTempDir(originalExe) {
		if err := validateSelfReplaceTarget(originalExe); err != nil {
			return fmt.Errorf("validate update target: %w", err)
		}
		args = append(args, fmt.Sprintf("--update-from=%s", originalExe))
	} else if originalExe != "" {
		fmt.Printf("[update] Current exe is in temp dir (%s), skipping --update-from\n", originalExe)
	}
	if err := startSelfReplaceProcess(path, args); err != nil {
		return fmt.Errorf("start updater: %w", err)
	}

	// The replacement process is already running and waiting for this process
	// to exit, so onTrayExit must only suppress normal offline/teardown work.
	SetUpdateHandoff()
	quitAfterSelfReplaceStart()
	return nil
}
