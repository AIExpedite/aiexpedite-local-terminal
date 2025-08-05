//go:build windows
// +build windows

package main

import (
	"os"
    _ "embed"
	"golang.org/x/sys/windows/registry"
)

// Embed the company logo icon for the system tray on Windows.
//go:embed assets/icon.ico
var iconData []byte

// ensureAutoStart registers the application to start at login via Windows Registry.
// It creates/updates the "TrayAgent" entry under HKCU\Software\Microsoft\Windows\CurrentVersion\Run
// so that Windows will launch the agent on user login:contentReference[oaicite:7]{index=7}:contentReference[oaicite:8]{index=8}.
func ensureAutoStart() error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	// Quote the path in case it contains spaces
	exePath = `"` + exePath + `"`
	key, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Run`,
		registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	// Set or update the value
	if err := key.SetStringValue("TrayAgent", exePath); err != nil {
		return err
	}
	return nil
}
