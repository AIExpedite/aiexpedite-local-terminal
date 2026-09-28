//go:build darwin

// File: os_boot_darwin.go
// -----------------------------------------------------------------------------
// macOS OS boot identity (os_boot.go).
//
// kern.bootsessionuuid is a random UUID the kernel mints at every boot, so a
// different value proves a reboot and no clock is involved. Only when it is
// unavailable does the identity fall back to kern.boottime, the wall-clock
// time of the boot: XNU moves that value when the calendar clock is set, so
// it is compared under the wall-clock rules (tolerance, uptime bound), never
// as an exact string. Nothing about any process is read.
// -----------------------------------------------------------------------------

package main

import (
	"fmt"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// The sysctl readers and the clock, behind seams for tests.
var (
	darwinBootSessionUUID = func() (string, error) { return unix.Sysctl("kern.bootsessionuuid") }
	darwinBootTime        = func() (*unix.Timeval, error) { return unix.SysctlTimeval("kern.boottime") }
	darwinNow             = time.Now
)

// readOSBootStamp returns the current OS boot identity, or none.
func readOSBootStamp() osBootStamp {
	if u, err := darwinBootSessionUUID(); err == nil {
		u = strings.TrimSpace(strings.TrimRight(u, "\x00"))
		if u != "" && !strings.ContainsAny(u, " \t\r\n") {
			return osBootStamp{ID: osBootKindDarwinSession + ":" + u}
		}
	}
	tv, err := darwinBootTime()
	if err != nil || tv == nil || tv.Sec <= 0 || tv.Usec < 0 || tv.Usec >= 1_000_000 {
		return osBootStamp{}
	}
	bootMs := int64(tv.Sec)*1000 + int64(tv.Usec)/1000
	uptimeMs := darwinNow().UnixMilli() - bootMs
	if uptimeMs <= 0 {
		// A boot time in the future: the clock is not usable as evidence.
		return osBootStamp{}
	}
	return osBootStamp{
		ID:       fmt.Sprintf("%s:%d.%06d", osBootKindDarwinBootTime, int64(tv.Sec), int64(tv.Usec)),
		UptimeMs: uptimeMs,
	}
}
