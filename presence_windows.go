//go:build windows
// +build windows

// File: presence_windows.go
// -----------------------------------------------------------------------------
// Windows presence signals (PARALLEL_RUNS_AND_COMPUTER_MAINTENANCE_PLAN.md
// §8.1). Pure syscalls, no shell-out and no WMI, so the 30-second sampler
// costs nothing measurable.
//
//	seconds since last input  GetLastInputInfo (user32)
//	screen locked             OpenInputDesktop (user32) — see below
//	display asleep            NOT READ in Phase 0 (see below)
//	on battery                GetSystemPowerStatus (kernel32)
// -----------------------------------------------------------------------------

package main

import (
	"syscall"
	"time"
	"unsafe"
)

var (
	presenceUser32           = syscall.NewLazyDLL("user32.dll")
	presenceKernel32         = syscall.NewLazyDLL("kernel32.dll")
	procGetLastInputInfo     = presenceUser32.NewProc("GetLastInputInfo")
	procOpenInputDesktop     = presenceUser32.NewProc("OpenInputDesktop")
	procCloseDesktop         = presenceUser32.NewProc("CloseDesktop")
	procGetTickCount64       = presenceKernel32.NewProc("GetTickCount64")
	procGetSystemPowerStatus = presenceKernel32.NewProc("GetSystemPowerStatus")
	desktopReadObjectsAccess = uintptr(0x0001) // DESKTOP_READOBJECTS
	acLineStatusOffline      = byte(0)
	acLineStatusUnknown      = byte(255)
)

type lastInputInfo struct {
	cbSize uint32
	dwTime uint32
}

// systemPowerStatus is SYSTEM_POWER_STATUS. Only ACLineStatus is read; the
// rest of the struct must still be laid out exactly or the call writes past
// what we allocated.
type systemPowerStatus struct {
	ACLineStatus        byte
	BatteryFlag         byte
	BatteryLifePercent  byte
	SystemStatusFlag    byte
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

// platformPresenceSignals reads what Windows exposes to a tray app running in
// the person's own interactive session.
func platformPresenceSignals() presenceSignals {
	signals := presenceSignals{}

	if idle, ok := windowsInputIdle(); ok {
		signals.Idle, signals.IdleOK = idle, true
	}
	if locked, ok := windowsScreenLocked(); ok {
		signals.Locked, signals.LockedOK = locked, true
	}
	// Display power state (GUID_CONSOLE_DISPLAY_STATE) needs a window handle
	// and RegisterPowerSettingNotification, i.e. a message pump wired into the
	// tray window. Deliberately NOT read in Phase 0: the idle timer already
	// reaches `away` after 20 minutes, and a display signal only ever makes
	// `away` arrive sooner. Left unread rather than guessed, so the field says
	// "unknown" instead of "the display is on".
	return signals
}

// windowsInputIdle returns the time since the last input in this session.
//
// GetLastInputInfo's dwTime is a 32-BIT tick count, so the subtraction must be
// done in 32-bit arithmetic: it wraps every ~49.7 days of uptime, and widening
// first would report a 49-day idle period on the wrap — an instant, confident
// `away` on a machine someone is typing on.
func windowsInputIdle() (time.Duration, bool) {
	info := lastInputInfo{cbSize: uint32(unsafe.Sizeof(lastInputInfo{}))}
	ret, _, _ := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&info)))
	if ret == 0 {
		return 0, false
	}
	tick, _, _ := procGetTickCount64.Call()
	elapsed := uint32(tick) - info.dwTime
	return time.Duration(elapsed) * time.Millisecond, true
}

// windowsScreenLocked reports whether the interactive desktop is locked.
//
// OpenInputDesktop fails while the workstation is locked or on the secure
// (Winlogon) desktop, and succeeds otherwise. It needs no window and no
// session notification registration, which is why it is used here instead of
// WTSRegisterSessionNotification.
//
// A FAILURE IS "unknown", never "locked": the call can also fail because the
// agent runs outside the interactive session, and reporting that as locked
// would make such a machine permanently `away` — the one direction §8.1
// forbids guessing.
func windowsScreenLocked() (bool, bool) {
	handle, _, _ := procOpenInputDesktop.Call(0, 0, desktopReadObjectsAccess)
	if handle != 0 {
		procCloseDesktop.Call(handle)
		return false, true
	}
	return false, false
}

// probeOnBattery reads AC line status. Cheap enough for every sample, unlike
// gatherBatteryWindows, which spawns PowerShell for the richer battery shape.
func probeOnBattery() (bool, bool) {
	status := systemPowerStatus{}
	ret, _, _ := procGetSystemPowerStatus.Call(uintptr(unsafe.Pointer(&status)))
	if ret == 0 {
		return false, false
	}
	if status.ACLineStatus == acLineStatusUnknown {
		return false, false
	}
	return status.ACLineStatus == acLineStatusOffline, true
}
