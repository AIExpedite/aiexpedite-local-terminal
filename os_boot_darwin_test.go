//go:build darwin

package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func withDarwinBootSources(t *testing.T, uuid string, uuidErr error, tv *unix.Timeval, tvErr error, since time.Duration) {
	t.Helper()
	prevU, prevT, prevS := darwinBootSessionUUID, darwinBootTime, darwinSinceBoot
	darwinBootSessionUUID = func() (string, error) { return uuid, uuidErr }
	darwinBootTime = func() (*unix.Timeval, error) { return tv, tvErr }
	darwinSinceBoot = func() (time.Duration, error) {
		if since <= 0 {
			return 0, errors.New("no clock")
		}
		return since, nil
	}
	t.Cleanup(func() { darwinBootSessionUUID, darwinBootTime, darwinSinceBoot = prevU, prevT, prevS })
}

// TestDarwinOSBootStamp: kern.bootsessionuuid first; kern.boottime only when
// the uuid is unavailable; nothing when neither is readable.
func TestDarwinOSBootStamp(t *testing.T) {
	const up = time.Hour
	tv := &unix.Timeval{Sec: 1_727_000_000, Usec: 42}

	withDarwinBootSources(t, "8C0F0D5E-1111-4000-8000-000000000001\x00", nil, tv, nil, up)
	if s := readOSBootStamp(); s.ID != "darwin-session:8C0F0D5E-1111-4000-8000-000000000001" || s.UptimeMs != 0 {
		t.Fatalf("uuid stamp = %+v", s)
	}
	withDarwinBootSources(t, "", errors.New("no such sysctl"), tv, nil, up)
	if s := readOSBootStamp(); s.ID != "darwin-boottime:1727000000.000042" || s.UptimeMs != 3_600_000 {
		t.Fatalf("fallback stamp = %+v", s)
	}
	withDarwinBootSources(t, "  ", nil, tv, nil, up)
	if s := readOSBootStamp(); !strings.HasPrefix(s.ID, osBootKindDarwinBootTime+":") {
		t.Fatalf("an empty uuid must fall back: %+v", s)
	}
	withDarwinBootSources(t, "", errors.New("no"), nil, errors.New("no"), up)
	if s := readOSBootStamp(); s.ID != "" {
		t.Fatalf("nothing readable, but a stamp: %+v", s)
	}
	withDarwinBootSources(t, "", errors.New("no"), tv, nil, 0)
	if s := readOSBootStamp(); s.ID != "" {
		t.Fatalf("no since-boot counter, but a stamp: %+v", s)
	}
	withDarwinBootSources(t, "", errors.New("no"), &unix.Timeval{Sec: 0}, nil, up)
	if s := readOSBootStamp(); s.ID != "" {
		t.Fatalf("no boot time, but a stamp: %+v", s)
	}
}

func TestDarwinOSBootStampReal(t *testing.T) {
	s := readOSBootStamp()
	if !strings.HasPrefix(s.ID, osBootKindDarwinSession+":") {
		t.Fatalf("kern.bootsessionuuid unread: %+v", s)
	}
	// The fallback's real sources are readable too.
	if tv, err := unix.SysctlTimeval("kern.boottime"); err != nil || tv.Sec <= 0 {
		t.Fatalf("kern.boottime: %v, %v", tv, err)
	}
	if d, err := darwinSinceBoot(); err != nil || d <= 0 {
		t.Fatalf("CLOCK_MONOTONIC_RAW: %v, %v", d, err)
	}
}
