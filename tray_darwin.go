//go:build darwin
// +build darwin

package main

// Embed the icon (PNG format) for macOS tray.
//go:embed assets/icon.png
var iconData []byte

// ensureAutoStart on macOS: Not implemented (requires creating a LaunchAgent or adding to login items).
func ensureAutoStart() error {
	// TODO: implement macOS autostart (e.g., via LaunchAgent plist in ~/Library/LaunchAgents)
	return nil
}
