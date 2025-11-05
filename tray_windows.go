//go:build windows
// +build windows

package main

import (
	"os"
	"syscall"

	_ "embed"

	"golang.org/x/sys/windows/registry"
)

//go:embed assets/aiexpedite-icon.ico
var iconData []byte

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	user32           = syscall.NewLazyDLL("user32.dll")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	procShowWindow   = user32.NewProc("ShowWindow")
)

const (
	SW_HIDE = 0
	SW_SHOW = 5
)

// getConsoleWindow returns the handle to the console window
func getConsoleWindow() uintptr {
	ret, _, _ := procGetConsoleWindow.Call()
	return ret
}

// showConsoleWindow shows or hides the console window
func showConsoleWindow(show bool) {
	hwnd := getConsoleWindow()
	if hwnd == 0 {
		return // No console window
	}

	if show {
		procShowWindow.Call(hwnd, SW_SHOW)
	} else {
		procShowWindow.Call(hwnd, SW_HIDE)
	}
}

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
