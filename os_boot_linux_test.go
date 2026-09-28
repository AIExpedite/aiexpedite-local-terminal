//go:build linux

package main

import (
	"strings"
	"testing"
)

// TestLinuxOSBootStamp: the kernel boot id, the same reader the start token
// uses; none without it.
func TestLinuxOSBootStamp(t *testing.T) {
	if s := readOSBootStamp(); !strings.HasPrefix(s.ID, osBootKindLinux+":") || s.ID == osBootKindLinux+":" {
		t.Fatalf("real stamp = %+v", s)
	}
	prev := linuxBootID
	t.Cleanup(func() { linuxBootID = prev })
	linuxBootID = func() string { return "5a0e5c1c-0000-4000-8000-000000000009" }
	if s := readOSBootStamp(); s.ID != "linux-boot:5a0e5c1c-0000-4000-8000-000000000009" || s.UptimeMs != 0 {
		t.Fatalf("stamp = %+v", s)
	}
	linuxBootID = func() string { return "" }
	if s := readOSBootStamp(); s.ID != "" {
		t.Fatalf("no boot id, but a stamp: %+v", s)
	}
}
