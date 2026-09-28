//go:build darwin

package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func withDarwinBootSources(t *testing.T, uuid string, uuidErr error, tv *unix.Timeval, tvErr error, now time.Time) {
	t.Helper()
	prevU, prevT, prevN := darwinBootSessionUUID, darwinBootTime, darwinNow
	darwinBootSessionUUID = func() (string, error) { return uuid, uuidErr }
	darwinBootTime = func() (*unix.Timeval, error) { return tv, tvErr }
	darwinNow = func() time.Time { return now }
	t.Cleanup(func() { darwinBootSessionUUID, darwinBootTime, darwinNow = prevU, prevT, prevN })
}

// TestDarwinOSBootStamp: kern.bootsessionuuid first; kern.boottime only when
// the uuid is unavailable; nothing when neither is readable.
func TestDarwinOSBootStamp(t *testing.T) {
	now := time.UnixMilli(1_727_003_600_000)
	tv := &unix.Timeval{Sec: 1_727_000_000, Usec: 42}

	withDarwinBootSources(t, "8C0F0D5E-1111-4000-8000-000000000001\x00", nil, tv, nil, now)
	if s := readOSBootStamp(); s.ID != "darwin-session:8C0F0D5E-1111-4000-8000-000000000001" || s.UptimeMs != 0 {
		t.Fatalf("uuid stamp = %+v", s)
	}
	withDarwinBootSources(t, "", errors.New("no such sysctl"), tv, nil, now)
	if s := readOSBootStamp(); s.ID != "darwin-boottime:1727000000.000042" || s.UptimeMs != 3_600_000 {
		t.Fatalf("fallback stamp = %+v", s)
	}
	withDarwinBootSources(t, "  ", nil, tv, nil, now)
	if s := readOSBootStamp(); !strings.HasPrefix(s.ID, osBootKindDarwinBootTime+":") {
		t.Fatalf("an empty uuid must fall back: %+v", s)
	}
	withDarwinBootSources(t, "", errors.New("no"), nil, errors.New("no"), now)
	if s := readOSBootStamp(); s.ID != "" {
		t.Fatalf("nothing readable, but a stamp: %+v", s)
	}
	withDarwinBootSources(t, "", errors.New("no"), &unix.Timeval{Sec: 1_727_999_999}, nil, now)
	if s := readOSBootStamp(); s.ID != "" {
		t.Fatalf("a boot time in the future gave a stamp: %+v", s)
	}
}

func TestDarwinOSBootStampReal(t *testing.T) {
	s := readOSBootStamp()
	if !strings.HasPrefix(s.ID, osBootKindDarwinSession+":") {
		t.Fatalf("kern.bootsessionuuid unread: %+v", s)
	}
}
