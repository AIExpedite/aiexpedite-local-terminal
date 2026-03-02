// File: session_signal_windows.go
//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// procGenerateConsoleCtrlEvent is resolved once at startup using the package-level
// kernel32 handle (declared in tray_windows.go) to avoid a redundant DLL load and
// repeated proc-lookup allocations on every sendInterrupt call.
var procGenerateConsoleCtrlEvent = kernel32.NewProc("GenerateConsoleCtrlEvent")

// sendInterrupt sends a Ctrl+C event to the process on Windows.
// Uses taskkill as a reliable cross-process interrupt mechanism.
func sendInterrupt(process *os.Process) error {
	if process == nil {
		return fmt.Errorf("process is nil")
	}

	// On Windows, GenerateConsoleCtrlEvent doesn't work well across process groups.
	// Instead, use taskkill with /T (tree) to send a terminate signal.
	// For a more graceful approach, we first try sending Ctrl+C via
	// GenerateConsoleCtrlEvent, then fall back to taskkill.

	// Try GenerateConsoleCtrlEvent first (Ctrl+Break which is more reliable)
	ret, _, err := procGenerateConsoleCtrlEvent.Call(
		uintptr(syscall.CTRL_BREAK_EVENT),
		uintptr(process.Pid),
	)
	if ret != 0 {
		return nil // Success
	}

	// Fallback: use taskkill for a graceful termination
	killCmd := exec.Command("taskkill", "/PID", fmt.Sprintf("%d", process.Pid))
	if killErr := killCmd.Run(); killErr != nil {
		return fmt.Errorf("GenerateConsoleCtrlEvent failed (%v), taskkill also failed: %w", err, killErr)
	}

	return nil
}
