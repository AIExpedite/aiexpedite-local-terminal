//go:build !windows && !darwin
// +build !windows,!darwin

// File: presence_other.go
// -----------------------------------------------------------------------------
// Presence on every other OS — in practice Linux
// (PARALLEL_RUNS_AND_COMPUTER_MAINTENANCE_PLAN.md §8.1, which specifies macOS
// and Windows only).
//
// NOTHING IS READ, on purpose, and this is the honest answer rather than a
// missing feature:
//   - Wayland has no idle-time API a normal process may ask. Idle is reported
//     by the compositor to its own idle-inhibit / idle-notify clients, and
//     reaching it needs a session-bus portal the agent does not speak.
//   - X11 has XScreenSaverQueryInfo, but it needs cgo and a live X
//     connection; this agent builds without cgo, and a headless or
//     SSH-launched agent has no display to query.
//   - /proc exposes no "time since last human input" anywhere.
//
// So platformPresenceSignals returns an empty struct, samplePresence reports
// NOTHING (not a guessed `active`), and the server reads a computer with no
// presence as `active` — the direction that only ever costs parallelism.
// Load metrics are unaffected: they come from gopsutil and work here.
// -----------------------------------------------------------------------------

package main

// platformPresenceSignals reads nothing on this OS.
func platformPresenceSignals() presenceSignals {
	return presenceSignals{}
}
