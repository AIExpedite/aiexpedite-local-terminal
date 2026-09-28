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
// (ledgerEntry.OSBoot / OSUptimeMs, refreshed on every change of the entry
// and periodically while the agent runs; an agent process lives inside one
// OS boot, so every process it spawned did too). At the boot reap, an earlier agent boot's entry whose recorded OS
// boot is PROVEN different from the current one is reaped without probing,
// signalling or inspecting anything (osBootRebootProven).
//
// The identities, as "<kind>:<value>":
//
//	linux-boot       /proc/sys/kernel/random/boot_id (random per boot) — exact
//	darwin-session   sysctl kern.bootsessionuuid (random per boot) — exact
//	darwin-boottime  sysctl kern.boottime, "<sec>.<usec>", read only when the
//	                 uuid is unavailable; OSUptimeMs is CLOCK_MONOTONIC_RAW
//	win-boottime     now − GetTickCount64, whole seconds; OSUptimeMs is the
//	                 GetTickCount64 reading itself
//
// A random per-boot id proves a reboot by being different. A wall-clock boot
// time never does on its own: it is (wall clock − time since boot), so a
// clock step within one boot moves it exactly as a reboot would, and no
// tolerance tells the two apart. It proves a reboot only together with the
// since-boot counter it comes with:
//
//   - the counter went BACKWARDS. GetTickCount64 and CLOCK_MONOTONIC_RAW
//     restart at boot, include sleep and hibernation, and never decrease
//     within one boot, whatever the wall clock does — so a smaller reading
//     than the one recorded can only come from a later boot; AND
//   - the boot time moved by more than osBootTimeTolerance, in either
//     direction (a reboot's RTC may even set the clock back): the computed
//     value jitters by rounding and by the gap between the two reads.
//
// A new boot whose counter has already passed the recorded reading proves
// nothing; the entry then stays on the same-boot rules. The recorded reading
// is refreshed while the agent runs (RefreshOSBootStamps), so it is close to
// the old boot's whole uptime and the new boot's agent almost always starts
// below it.
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
	// UptimeMs is the since-boot counter, in milliseconds, when ID was
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
// clock and the since-boot counter.
const osBootTimeTolerance = 5 * time.Second

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
	case osBootKindWindowsBootTime, osBootKindDarwinBootTime:
		return counterRebootProven(rv, cv, recorded.UptimeMs, current.UptimeMs)
	}
	return false
}

// counterRebootProven applies the wall-clock kinds' rule (see the file
// header): the since-boot counter restarted AND the boot time moved beyond
// the tolerance. The wall clock alone never proves a reboot.
func counterRebootProven(recValue, curValue string, recUptimeMs, curUptimeMs int64) bool {
	recMs, ok := parseBootTimeMs(recValue)
	if !ok {
		return false
	}
	curMs, ok := parseBootTimeMs(curValue)
	if !ok || recUptimeMs <= 0 || curUptimeMs <= 0 {
		return false
	}
	if curUptimeMs >= recUptimeMs {
		return false // the counter did not restart: same boot, or unprovable
	}
	moved := curMs - recMs
	if moved < 0 {
		moved = -moved
	}
	return moved > osBootTimeTolerance.Milliseconds()
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
