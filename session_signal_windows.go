// File: session_signal_windows.go
//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
)

// sendInterrupt asks the process to terminate gracefully on Windows.
//
// It deliberately does NOT use GenerateConsoleCtrlEvent. That call takes a
// process GROUP id, not a pid, and none of the CLI children are spawned with
// CREATE_NEW_PROCESS_GROUP — so passing a child's pid either fails or, worse,
// resolves to the group the agent itself belongs to and delivers CTRL_BREAK to
// our own console. The agent then dies with STATUS_CONTROL_C_EXIT (0xC000013A)
// while trying to cancel one turn; under `go test` it took the whole test
// binary down at TestOpenCodeNativeManager_TurnTimeoutIsReported, which is how
// this was found.
//
// taskkill without /F is the graceful path, and /T covers the CLI's own
// children. Callers all escalate to Process.Kill() when this returns an error
// or the process does not exit in time, so a refusal here is never terminal.
func sendInterrupt(process *os.Process) error {
	if process == nil {
		return fmt.Errorf("process is nil")
	}

	killCmd := exec.Command("taskkill", "/PID", fmt.Sprintf("%d", process.Pid), "/T")
	hideWindow(killCmd)
	if err := killCmd.Run(); err != nil {
		return fmt.Errorf("taskkill of pid %d failed: %w", process.Pid, err)
	}

	return nil
}
