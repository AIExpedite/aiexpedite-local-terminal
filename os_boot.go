// File: os_boot.go
// -----------------------------------------------------------------------------
// The OS boot identity and the OS-reboot proof (failover Wave 4 row 2.2,
// v1.0.39 — local/documents/features/ship/
// ORCHESTRATOR_SPEND_GUARD_AND_COMPUTER_LOSS_PLAN.md §5.2).
//
// An OS reboot ends every process on the machine. A session whose processes
// were all started under an EARLIER OS boot is therefore provably gone,
// whatever its containment, its PID state or how complete its ledger record
// is, and no process needs to be looked at to know it. That is the one proof
// Unix has: there, a descendant can setsid out of its process group, so the
// same-boot rules (session_ledger.go) never vouch for a session.
//
// Every ledger entry records the OS boot its agent process ran under
// (ledgerEntry.OSBoot / OSUptimeMs, refreshed on every change of the entry;
// an agent process lives inside one OS boot, so every process it spawned
// did too). At the boot reap, an earlier agent boot's entry whose recorded OS
// boot is PROVEN different from the current one is reaped without probing,
// signalling or inspecting anything (osBootRebootProven).
//
// The identities, as "<kind>:<value>":
//
//	linux-boot       /proc/sys/kernel/random/boot_id (random per boot) — exact
//	darwin-session   sysctl kern.bootsessionuuid (random per boot) — exact
//	darwin-boottime  sysctl kern.boottime, "<sec>.<usec>", read only when the
//	                 uuid is unavailable — a wall-clock boot time
//	win-boottime     now − GetTickCount64, whole seconds — a wall-clock boot
//	                 time; OSUptimeMs is the GetTickCount64 reading itself
//
// A random per-boot id proves a reboot by being different. A wall-clock boot
// time is (wall clock − time since boot), so it also moves when the wall
// clock does: rounding jitter, NTP slewing against the uptime counter, and
// clock steps. Such a value proves a reboot only when:
//
//   - the uptime counter went BACKWARDS (Windows only: GetTickCount64 counts
//     from boot and includes sleep and hibernation, so within one boot it
//     never decreases — this proof ignores the wall clock entirely); or
//   - the boot time moved FORWARD (a later boot always starts later) by more
//     than the slack — osBootTimeTolerance, or 1/1000 of the uptime elapsed
//     between the two readings, whichever is larger, which no oscillator
//     drift or NTP slew reaches — AND by at least the recorded uptime minus
//     the slack. A reboot after the recording moves the boot time by more
//     than the recorded uptime, always; within one boot only a forward clock
//     step at least that large could, never jitter, drift or sleep.
//
// No identity readable (on either side), different kinds, or a malformed
// value: no proof, never a guess. The entry then stays on the same-boot rules.
// -----------------------------------------------------------------------------

package main

import (
	"strconv"
	"strings"
	"time"
)

// osBootStamp is one reading of the OS boot identity.
type osBootStamp struct {
	// ID is "<kind>:<value>", or "" when no identity could be read.
	ID string
	// UptimeMs is the time since that boot, in milliseconds, when ID was
	// read. Used only by the wall-clock kinds (0 for the others).
	UptimeMs int64
}

const (
	osBootKindLinux           = "linux-boot"
	osBootKindDarwinSession   = "darwin-session"
	osBootKindDarwinBootTime  = "darwin-boottime"
	osBootKindWindowsBootTime = "win-boottime"
)

// osBootTimeTolerance is the least a wall-clock boot time must move to count:
// the computed value jitters by rounding and by the gap between reading the
// clock and the uptime counter.
const osBootTimeTolerance = 5 * time.Second

// osBootDriftDivisor bounds the drift accepted between the wall clock and
// the uptime counter within one boot: 1/1000 (1000 ppm) of the uptime that
// elapsed between the two readings, far beyond any real oscillator error or
// NTP slew rate (tens of ppm).
const osBootDriftDivisor = 1000

// splitOSBootID splits "<kind>:<value>"; both parts must be non-empty.
func splitOSBootID(id string) (kind, value string, ok bool) {
	kind, value, found := strings.Cut(id, ":")
	if !found || kind == "" || value == "" {
		return "", "", false
	}
	return kind, value, true
}

// osBootRebootProven reports whether recorded and current name DIFFERENT OS
// boots, proven. Anything short of proof answers false.
func osBootRebootProven(recorded, current osBootStamp) bool {
	rk, rv, ok := splitOSBootID(recorded.ID)
	if !ok {
		return false
	}
	ck, cv, ok := splitOSBootID(current.ID)
	if !ok || rk != ck {
		return false
	}
	switch rk {
	case osBootKindLinux, osBootKindDarwinSession:
		return rv != cv
	case osBootKindWindowsBootTime:
		return wallClockRebootProven(rv, cv, recorded.UptimeMs, current.UptimeMs, true)
	case osBootKindDarwinBootTime:
		// kern.boottime's companion uptime is itself wall-clock derived,
		// so it proves nothing by going backwards.
		return wallClockRebootProven(rv, cv, recorded.UptimeMs, current.UptimeMs, false)
	}
	return false
}

// wallClockRebootProven applies the wall-clock rules (see the file header).
// monotonicUptime says the uptimes come from a counter that never decreases
// within one boot (GetTickCount64).
func wallClockRebootProven(recValue, curValue string, recUptimeMs, curUptimeMs int64, monotonicUptime bool) bool {
	recMs, ok := parseBootTimeMs(recValue)
	if !ok {
		return false
	}
	curMs, ok := parseBootTimeMs(curValue)
	if !ok || recUptimeMs <= 0 || curUptimeMs <= 0 {
		return false
	}
	if monotonicUptime && curUptimeMs < recUptimeMs {
		return true // the since-boot counter restarted: a new boot
	}
	moved := curMs - recMs
	slack := osBootTimeTolerance.Milliseconds()
	elapsed := curUptimeMs - recUptimeMs
	if elapsed < 0 {
		elapsed = -elapsed
	}
	if drift := elapsed / osBootDriftDivisor; drift > slack {
		slack = drift
	}
	return moved > slack && moved >= recUptimeMs-slack
}

// parseBootTimeMs parses "<sec>" or "<sec>.<fraction>" (a positive Unix time)
// into milliseconds.
func parseBootTimeMs(v string) (int64, bool) {
	secPart, frac, hasFrac := strings.Cut(v, ".")
	if secPart == "" || strings.IndexFunc(secPart, notDigit) >= 0 {
		return 0, false
	}
	sec, err := strconv.ParseInt(secPart, 10, 64)
	if err != nil || sec <= 0 || sec > (1<<62)/1000 {
		return 0, false
	}
	ms := sec * 1000
	if hasFrac {
		if frac == "" || len(frac) > 9 || strings.IndexFunc(frac, notDigit) >= 0 {
			return 0, false
		}
		n, _ := strconv.Atoi((frac + "00")[:3])
		ms += int64(n)
	}
	return ms, true
}

func notDigit(r rune) bool { return r < '0' || r > '9' }
