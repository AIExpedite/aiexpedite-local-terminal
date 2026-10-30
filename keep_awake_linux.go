//go:build linux
// +build linux

// File: keep_awake_linux.go
// -----------------------------------------------------------------------------
// Linux keep-awake (keep_awake.go): a `systemd-inhibit --what=idle` child that
// sleeps until it is killed.
//
// `--what=idle` only, never `sleep`: a sleep block would also refuse a suspend
// the person asks for, beyond the idle-only promise (macOS `-i`, Windows
// PowerRequestSystemRequired). Pdeathsig ends the child if the agent dies.
// No systemd-inhibit (a non-systemd distro, a container): the error is logged
// once by the manager and sessions run without keep-awake.
// -----------------------------------------------------------------------------

package main

import (
	"os/exec"
	"syscall"
)

// systemdInhibitCommand builds the child. A var so tests replace the binary.
var systemdInhibitCommand = func() *exec.Cmd {
	cmd := exec.Command("systemd-inhibit",
		"--what=idle", "--mode=block", "--who=AI Expedite", "--why="+keepAwakeReason,
		"sleep", "infinity")
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	return cmd
}

func acquirePlatformKeepAwake(onLost func(error)) (func(), error) {
	return holdWhileChildRuns(systemdInhibitCommand(), onLost)
}
