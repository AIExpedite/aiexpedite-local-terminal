//go:build !windows
// +build !windows

// File: presence_battery_other.go
// -----------------------------------------------------------------------------
// "On battery" off Windows. Windows has a one-syscall answer
// (GetSystemPowerStatus, presence_windows.go); macOS and Linux do not, so this
// reuses the existing richer battery probe (systemInfoExtended.go gatherBattery:
// `pmset -g batt` on macOS, /sys/class/power_supply on Linux).
//
// The caller caches the result for one report interval (load_report.go
// sampleOnBattery), so the macOS shell-out happens at most once every five
// minutes rather than on every 30-second sample.
// -----------------------------------------------------------------------------

package main

// probeOnBattery reports whether the host is running on battery, and whether
// that could be determined at all. A host with no battery is "not on battery",
// known — that is a fact about a desktop, not a failed probe.
func probeOnBattery() (bool, bool) {
	battery := gatherBattery()
	if battery == nil {
		return false, false
	}
	if !battery.Present {
		return false, true
	}
	return !battery.Plugged, true
}
