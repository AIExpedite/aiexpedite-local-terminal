//go:build linux
// +build linux

package main

import _ "embed"

// Embed the icon (PNG format) for Linux tray icon (if supported).
//go:embed assets/icon.png
var iconData []byte

// ensureAutoStart on Linux: Not implemented (could use XDG autostart or systemd service).
func ensureAutoStart() error {
	// TODO: implement Linux autostart (e.g., create ~/.config/autostart/TrayAgent.desktop)
	return nil
}

// showConsoleWindow is a no-op on Linux (console visibility is handled differently)
func showConsoleWindow(show bool) {
	// No-op on Linux
}
