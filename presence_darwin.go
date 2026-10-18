//go:build darwin
// +build darwin

// File: presence_darwin.go
// -----------------------------------------------------------------------------
// macOS presence signals (PARALLEL_RUNS_AND_COMPUTER_MAINTENANCE_PLAN.md §8.1).
//
//	seconds since last input  ioreg -c IOHIDSystem  →  "HIDIdleTime" (ns)
//	display asleep            pmset -g powerstate IODisplayWrangler
//	screen locked             NOT READ in Phase 0 (see below)
//
// This agent builds without cgo, so the IOKit and CoreGraphics calls §8.1
// names are not reachable directly; these two command-line probes read the
// same kernel properties. `ioreg` is a cheap read-only probe and runs on every
// sample; `pmset` is cached for one report interval so the sampler never
// spawns it more than once every five minutes.
//
// WHAT IS NOT AVAILABLE, honestly:
//   - screen lock. CGSessionCopyCurrentDictionary's CGSSessionScreenIsLocked
//     needs cgo; there is no documented command-line equivalent. Unread, so
//     the lock signal is "unknown" and a locked Mac reaches `away` through the
//     20-minute idle rule (or the display rule) instead of immediately.
//   - IODisplayWrangler does not exist on Apple Silicon, where the probe finds
//     nothing and the display signal is simply "unknown". Again the idle timer
//     carries it.
// -----------------------------------------------------------------------------

package main

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// presenceProbeOutput is probeOutputArgs, replaced in tests so no test spawns
// a process.
var presenceProbeOutput = probeOutputArgs

// platformPresenceSignals reads what macOS exposes without cgo.
func platformPresenceSignals() presenceSignals {
	signals := presenceSignals{}
	if idle, ok := darwinInputIdle(); ok {
		signals.Idle, signals.IdleOK = idle, true
	}
	if off, ok := darwinDisplayOff(); ok {
		signals.DisplayOff, signals.DisplayOffOK = off, true
	}
	return signals
}

// darwinInputIdle reads IOHIDSystem's HIDIdleTime, in nanoseconds since the
// last HID event.
func darwinInputIdle() (time.Duration, bool) {
	out := presenceProbeOutput("ioreg", "-c", "IOHIDSystem", "-d", "4", "-r")
	if out == "" {
		return 0, false
	}
	return parseHIDIdleTime(out)
}

// parseHIDIdleTime finds the smallest HIDIdleTime in an ioreg dump.
//
// SMALLEST, not first: a Mac with several HID devices reports one idle time
// each, and the person's activity is the most recent event on ANY of them. The
// largest would read a mouse they never touch as "nobody is here".
func parseHIDIdleTime(out string) (time.Duration, bool) {
	var best int64 = -1
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "HIDIdleTime") {
			continue
		}
		_, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		ns, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || ns < 0 {
			continue
		}
		if best < 0 || ns < best {
			best = ns
		}
	}
	if best < 0 {
		return 0, false
	}
	return time.Duration(best) * time.Nanosecond, true
}

// darwinDisplayCache holds one pmset reading for a report interval.
type darwinDisplayCache struct {
	mu      sync.Mutex
	at      time.Time
	off, ok bool
}

var displayCache = &darwinDisplayCache{}

// presenceNow is time.Now, replaced in tests.
var presenceNow = time.Now

// darwinDisplayOff reports whether the display is asleep, at most one probe
// per report interval (the same floor the reporter paces itself with, so no
// second magic number).
func darwinDisplayOff() (bool, bool) {
	now := presenceNow()
	displayCache.mu.Lock()
	defer displayCache.mu.Unlock()
	if !displayCache.at.IsZero() && now.Sub(displayCache.at) < loadReportMaxInterval {
		return displayCache.off, displayCache.ok
	}
	out := presenceProbeOutput("pmset", "-g", "powerstate", "IODisplayWrangler")
	off, ok := parseDisplayWranglerState(out)
	displayCache.at, displayCache.off, displayCache.ok = now, off, ok
	return off, ok
}

// parseDisplayWranglerState reads the current power state out of
// `pmset -g powerstate IODisplayWrangler`:
//
//	Current power states:
//	  Device Name          State  Max  ...
//	  IODisplayWrangler        4    4  ...
//
// State 4 is a lit display; anything lower is dimmed, asleep or off. An
// absent device (Apple Silicon) parses as "unknown".
func parseDisplayWranglerState(out string) (bool, bool) {
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "IODisplayWrangler") {
			continue
		}
		fields := strings.Fields(line)
		for i, field := range fields {
			if field != "IODisplayWrangler" {
				continue
			}
			if i+1 >= len(fields) {
				break
			}
			state, err := strconv.Atoi(fields[i+1])
			if err != nil {
				break
			}
			return state < 4, true
		}
	}
	return false, false
}
