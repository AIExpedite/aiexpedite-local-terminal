// Tests for the OS-reboot proof's comparison (os_boot.go). Platform-free: the
// rules for every kind are exercised on every platform.
package main

import (
	"strconv"
	"testing"
	"time"
)

const (
	testOSBootA = osBootKindLinux + ":0b6b1c3e-5a57-4c2f-9e2b-7f1f3b9a0001"
	testOSBootB = osBootKindLinux + ":0b6b1c3e-5a57-4c2f-9e2b-7f1f3b9a0002"
)

const (
	hourMs = int64(time.Hour / time.Millisecond)
	dayMs  = 24 * hourMs
)

func winStamp(bootSec int64, uptimeMs int64) osBootStamp {
	return osBootStamp{ID: osBootKindWindowsBootTime + ":" + strconv.FormatInt(bootSec, 10), UptimeMs: uptimeMs}
}

// TestOSBootExactKinds: a random per-boot id proves a reboot by differing;
// the same id, a missing id on either side, a different kind or a malformed
// value proves nothing.
func TestOSBootExactKinds(t *testing.T) {
	darwinA := osBootStamp{ID: osBootKindDarwinSession + ":8C0F0D5E-0000-4000-8000-000000000001"}
	darwinB := osBootStamp{ID: osBootKindDarwinSession + ":8C0F0D5E-0000-4000-8000-000000000002"}
	cases := []struct {
		name     string
		rec, cur osBootStamp
		want     bool
	}{
		{"linux different boot", osBootStamp{ID: testOSBootA}, osBootStamp{ID: testOSBootB}, true},
		{"linux same boot", osBootStamp{ID: testOSBootA}, osBootStamp{ID: testOSBootA}, false},
		{"darwin different boot", darwinA, darwinB, true},
		{"darwin same boot", darwinA, darwinA, false},
		{"no recorded id", osBootStamp{}, osBootStamp{ID: testOSBootB}, false},
		{"no current id", osBootStamp{ID: testOSBootA}, osBootStamp{}, false},
		{"neither", osBootStamp{}, osBootStamp{}, false},
		{"kinds differ", darwinA, osBootStamp{ID: osBootKindDarwinBootTime + ":1727000000.000001", UptimeMs: 1000}, false},
		{"unknown kind", osBootStamp{ID: "plan9:a"}, osBootStamp{ID: "plan9:b"}, false},
		{"no kind", osBootStamp{ID: ":a"}, osBootStamp{ID: ":b"}, false},
		{"no value", osBootStamp{ID: osBootKindLinux + ":"}, osBootStamp{ID: testOSBootB}, false},
		{"no separator", osBootStamp{ID: "linux-boot"}, osBootStamp{ID: testOSBootB}, false},
	}
	for _, c := range cases {
		if got := osBootRebootProven(c.rec, c.cur); got != c.want {
			t.Errorf("%s: proven = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestOSBootWindowsTolerance pins the wall-clock rules on the Windows kind.
func TestOSBootWindowsTolerance(t *testing.T) {
	const boot = int64(1_727_000_000)
	cases := []struct {
		name     string
		rec, cur osBootStamp
		want     bool
	}{
		// The same boot, read an hour apart: the computed value jitters by a
		// second or two and must never count.
		{"same boot, 1 s jitter", winStamp(boot, hourMs), winStamp(boot+1, 2*hourMs), false},
		{"same boot, jitter backwards", winStamp(boot, hourMs), winStamp(boot-1, 2*hourMs), false},
		{"moved exactly the tolerance", winStamp(boot, 1000), winStamp(boot+5, 2000), false},
		{"moved beyond the tolerance, after a short uptime", winStamp(boot, 1000), winStamp(boot+6, 2000), true},
		// A forward clock step within one boot, smaller than the uptime the
		// session was recorded at: not proof.
		{"same boot, 6 s clock step", winStamp(boot, hourMs), winStamp(boot+6, 2*hourMs), false},
		{"same boot, 30 min clock step", winStamp(boot, hourMs), winStamp(boot+1800, 2*hourMs), false},
		// Moving backwards is never a later boot.
		{"clock stepped back a day", winStamp(boot, hourMs), winStamp(boot-86400, 2*hourMs), false},
		// Weeks of oscillator drift against NTP within one boot, recorded
		// right after boot: the drift slack keeps it from counting.
		{"30 days of drift", winStamp(boot, 60_000), winStamp(boot+60, 60_000+30*dayMs), false},
		// A reboot: the new boot's uptime is below the recorded one. Proven
		// by the counter alone, whatever the wall clock says.
		{"rebooted, uptime restarted", winStamp(boot, 5*hourMs), winStamp(boot+5*3600+120, 2*60_000), true},
		{"rebooted, clock even stepped back", winStamp(boot, 5*hourMs), winStamp(boot-3600, 2*60_000), true},
		// A reboot whose new boot has been up longer than the old one was at
		// the recording: the boot time moved by more than that uptime.
		{"rebooted long ago", winStamp(boot, 10*60_000), winStamp(boot+3*86400, 4*dayMs), true},
		{"recorded without an uptime", winStamp(boot, 0), winStamp(boot+86400, hourMs), false},
		{"current without an uptime", winStamp(boot, hourMs), winStamp(boot+86400, 0), false},
		{"malformed recorded value", osBootStamp{ID: osBootKindWindowsBootTime + ":abc", UptimeMs: hourMs}, winStamp(boot+86400, 60_000), false},
		{"negative recorded value", osBootStamp{ID: osBootKindWindowsBootTime + ":-5", UptimeMs: hourMs}, winStamp(boot+86400, 60_000), false},
	}
	for _, c := range cases {
		if got := osBootRebootProven(c.rec, c.cur); got != c.want {
			t.Errorf("%s: proven = %v, want %v (rec %+v cur %+v)", c.name, got, c.want, c.rec, c.cur)
		}
	}
}

// TestOSBootDarwinBootTimeFallback: kern.boottime is a wall-clock value whose
// uptime is wall-clock derived too, so only a forward move beyond the slack
// AND the recorded uptime counts — never an uptime that went down.
func TestOSBootDarwinBootTimeFallback(t *testing.T) {
	st := func(v string, up int64) osBootStamp {
		return osBootStamp{ID: osBootKindDarwinBootTime + ":" + v, UptimeMs: up}
	}
	cases := []struct {
		name     string
		rec, cur osBootStamp
		want     bool
	}{
		{"same boot, NTP moved it 300 ms", st("1727000000.100000", hourMs), st("1727000000.400000", 2*hourMs), false},
		{"clock stepped back, uptime reads lower", st("1727000000.000000", 5*hourMs), st("1726990000.000000", hourMs), false},
		{"rebooted", st("1727000000.000000", 5*hourMs), st("1727030000.000000", 60_000), true},
		{"same boot, clock stepped forward 1 min", st("1727000000.000000", 5*hourMs), st("1727000060.000000", 6*hourMs), false},
	}
	for _, c := range cases {
		if got := osBootRebootProven(c.rec, c.cur); got != c.want {
			t.Errorf("%s: proven = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseBootTimeMs(t *testing.T) {
	good := map[string]int64{
		"1727000000":        1_727_000_000_000,
		"1727000000.5":      1_727_000_000_500,
		"1727000000.05":     1_727_000_000_050,
		"1727000000.123456": 1_727_000_000_123,
		"1727000000.000999": 1_727_000_000_000,
	}
	for in, want := range good {
		if got, ok := parseBootTimeMs(in); !ok || got != want {
			t.Errorf("parseBootTimeMs(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "0", "-1", "+5", "1.", ".5", "1.x", "1e9", "1.1234567890", "99999999999999999999"} {
		if got, ok := parseBootTimeMs(in); ok {
			t.Errorf("parseBootTimeMs(%q) = %d, accepted", in, got)
		}
	}
}

// TestReadOSBootStampIsStable: on a real machine two readings of the current
// identity, in order, never prove a reboot.
func TestReadOSBootStampIsStable(t *testing.T) {
	a := readOSBootStamp()
	time.Sleep(20 * time.Millisecond)
	b := readOSBootStamp()
	if a.ID == "" || b.ID == "" {
		t.Skipf("no OS boot identity on this platform/runner: %+v", a)
	}
	// Always in reading order: the recorded stamp is the older one.
	if osBootRebootProven(a, b) {
		t.Fatalf("one boot proved a reboot against itself: %+v vs %+v", a, b)
	}
}
