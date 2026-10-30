//go:build darwin
// +build darwin

// File: keep_awake_darwin.go
// -----------------------------------------------------------------------------
// macOS keep-awake (keep_awake.go): a `caffeinate -i -w <agent pid>` child.
//
// `-i` holds a PreventUserIdleSystemSleep assertion (idle sleep only: the
// display may sleep, a closed lid still sleeps). `-w` makes caffeinate exit on
// its own when this agent's process does, so a crashed agent never leaves the
// computer awake. No cgo, the same as presence_darwin.go.
// -----------------------------------------------------------------------------

package main

import (
	"os"
	"os/exec"
	"strconv"
)

// caffeinateCommand builds the child. A var so tests replace the binary.
var caffeinateCommand = func(agentPID int) *exec.Cmd {
	return exec.Command("caffeinate", "-i", "-w", strconv.Itoa(agentPID))
}

func acquirePlatformKeepAwake(onLost func(error)) (func(), error) {
	return holdWhileChildRuns(caffeinateCommand(os.Getpid()), onLost)
}
