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
