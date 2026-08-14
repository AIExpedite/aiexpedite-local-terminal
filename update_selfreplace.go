//go:build !darwin

// File: update_selfreplace.go
// Windows/Linux update application via the temp-binary self-replace handoff.
// (macOS replaces the signed .app bundle instead — see update_darwin.go.)

package main

import (
	"os"

	"github.com/getlantern/systray"
)

// applyVerifiedUpdate installs a verified update on Windows and Linux. The
// verified artifact is an executable (a raw .exe on Windows or an AppImage on
// Linux); we mark it ready and quit, and the tray-exit handoff relaunches it
// with --update-from so it replaces the installed artifact and restarts.
func applyVerifiedUpdate(path string, _ *UpdateInfo) error {
	_ = os.Chmod(path, 0o755) // ensure the downloaded binary is executable
	SetUpdateReady(path)
	systray.Quit() // graceful restart via onTrayExit
	return nil
}
