//go:build darwin
// +build darwin

package main

import _ "embed"

//go:embed assets/icon.png
var iconData []byte

// ensureAutoStart creates a LaunchAgent plist (TODO).
func ensureAutoStart() error {
	// TODO: implement macOS autostart
	return nil
}

// showConsoleWindow is a no-op on macOS (console visibility is handled differently)
func showConsoleWindow(show bool) {
	// No-op on macOS
}
