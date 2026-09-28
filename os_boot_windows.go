//go:build windows

// File: os_boot_windows.go
// -----------------------------------------------------------------------------
// Windows OS boot identity (os_boot.go): the system boot time, computed as
// now − GetTickCount64 and rounded to whole seconds, with the GetTickCount64
// reading itself as the uptime.
//
// GetTickCount64 counts milliseconds from boot and includes time spent in
// sleep and hibernation, so the computed boot time does not move while the
// machine sleeps, and the counter never decreases within one boot. The value
// still jitters (timer resolution, the gap between the two reads) and follows
// wall-clock changes, which is why it is compared under the wall-clock rules
// rather than for equality. Nothing about any process is read.
// -----------------------------------------------------------------------------

package main

import (
	"strconv"
	"time"

	"golang.org/x/sys/windows"
)

// The uptime counter and the clock, behind seams for tests.
var (
	windowsTimeSinceBoot = windows.DurationSinceBoot // GetTickCount64
	windowsNow           = time.Now
)

// readOSBootStamp returns the current OS boot identity, or none.
func readOSBootStamp() osBootStamp {
	uptimeMs := windowsTimeSinceBoot().Milliseconds()
	now := windowsNow()
	if uptimeMs <= 0 {
		return osBootStamp{}
	}
	bootMs := now.UnixMilli() - uptimeMs
	if bootMs <= 0 {
		return osBootStamp{}
	}
	bootSec := (bootMs + 500) / 1000
	return osBootStamp{
		ID:       osBootKindWindowsBootTime + ":" + strconv.FormatInt(bootSec, 10),
		UptimeMs: uptimeMs,
	}
}
