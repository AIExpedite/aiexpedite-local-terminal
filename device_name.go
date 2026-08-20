// device_name.go — the human-readable name this device reports to the cloud.
//
// The name is sent on EVERY token exchange (auth.go) as well as at registration,
// so the website's device list follows whatever this returns. It is a display
// label only: the device is identified by its agent id, never by its name.
package main

import (
	"os"
	"strings"
	"sync"
)

// deviceNameOnce caches the resolved name for the process lifetime. Resolution
// can shell out (see friendlyComputerName), and the token exchange that reports
// it runs on a timer — re-resolving each time would spawn a subprocess per hour
// for a value that changes about never.
var (
	deviceNameOnce  sync.Once
	deviceNameCache string
)

// friendlyComputerName is the platform hook, indirected so tests can drive every
// branch of resolveDeviceName on any host — the darwin implementation shells out
// to scutil and the others read an environment variable, so neither is
// exercisable on a single CI runner. Same seam style as claudeKeychainReader.
var friendlyComputerName = friendlyComputerNamePlatform

// getDeviceName returns the computer name or a default.
//
// os.Hostname() alone is not good enough. On macOS the *hostname* is set from
// DHCP/Bonjour and is frequently something like "56:36:05:D3:C2:75" or
// "dhcp-10-0-1-42", while the name the user actually recognises — the one in
// System Settings → General → About — is the ComputerName. Reporting the
// hostname made a Mac show up in the device list as a MAC address.
//
// So: ask the platform for its friendly name first, and fall back to the
// hostname when there isn't one.
func getDeviceName() string {
	deviceNameOnce.Do(func() { deviceNameCache = resolveDeviceName() })
	return deviceNameCache
}

func resolveDeviceName() string {
	if name := strings.TrimSpace(friendlyComputerName()); name != "" {
		return name
	}
	hostname, err := os.Hostname()
	if err != nil {
		return "Unknown Device"
	}
	if trimmed := strings.TrimSpace(hostname); trimmed != "" {
		return trimmed
	}
	return "Unknown Device"
}

// resetDeviceNameCache clears the memoised name. Test-only seam.
func resetDeviceNameCache() {
	deviceNameOnce = sync.Once{}
	deviceNameCache = ""
}
