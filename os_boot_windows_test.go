//go:build windows

package main

import (
	"strings"
	"testing"
	"time"
)

func withWindowsBootClock(t *testing.T, now time.Time, uptime time.Duration) {
	t.Helper()
	prevUp, prevNow := windowsTimeSinceBoot, windowsNow
	windowsTimeSinceBoot = func() time.Duration { return uptime }
	windowsNow = func() time.Time { return now }
	t.Cleanup(func() { windowsTimeSinceBoot, windowsNow = prevUp, prevNow })
}

// TestWindowsOSBootStamp: now − GetTickCount64, rounded to whole seconds,
// with the tick reading as the uptime.
func TestWindowsOSBootStamp(t *testing.T) {
	now := time.UnixMilli(1_727_003_600_499)
	withWindowsBootClock(t, now, time.Hour)
	if s := readOSBootStamp(); s.ID != "win-boottime:1727000000" || s.UptimeMs != 3_600_000 {
		t.Fatalf("stamp = %+v", s)
	}
	withWindowsBootClock(t, time.UnixMilli(1_727_003_600_500), time.Hour)
	if s := readOSBootStamp(); s.ID != "win-boottime:1727000001" {
		t.Fatalf("rounding: %+v", s)
	}
	// The same boot read a day later, with the usual jitter: never proof.
	a := readOSBootStamp()
	withWindowsBootClock(t, time.UnixMilli(1_727_003_600_500+86_400_000+14), time.Hour+24*time.Hour)
	b := readOSBootStamp()
	if osBootRebootProven(a, b) {
		t.Fatalf("one boot proved a reboot: %+v vs %+v", a, b)
	}
	// After a reboot the tick count restarts.
	withWindowsBootClock(t, time.UnixMilli(1_727_100_000_000), 3*time.Minute)
	if c := readOSBootStamp(); !osBootRebootProven(b, c) {
		t.Fatalf("a restarted tick count was not a reboot: %+v vs %+v", b, c)
	}
	withWindowsBootClock(t, now, 0)
	if s := readOSBootStamp(); s.ID != "" {
		t.Fatalf("no uptime, but a stamp: %+v", s)
	}
}

func TestWindowsOSBootStampReal(t *testing.T) {
	s := readOSBootStamp()
	if !strings.HasPrefix(s.ID, osBootKindWindowsBootTime+":") || s.UptimeMs <= 0 {
		t.Fatalf("real stamp = %+v", s)
	}
}
