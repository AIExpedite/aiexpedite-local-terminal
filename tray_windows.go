//go:build windows
// +build windows

package main

import (
	"os"

	_ "embed"

	"golang.org/x/sys/windows/registry"
)

//go:embed assets/icon.ico
var iconData []byte

// ensureAutoStart adds/updates HKCU\Software\Microsoft\Windows\CurrentVersion\Run
// so that the agent launches automatically when the user logs in.
func ensureAutoStart() error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	// Quote the path in case it contains spaces.
	exePath = `"` + exePath + `"`

	key, err := registry.OpenKey(
		registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Run`,
		registry.QUERY_VALUE|registry.SET_VALUE,
	)
	if err != nil {
		return err
	}
	defer key.Close()

	return key.SetStringValue("TrayAgent", exePath)
}
