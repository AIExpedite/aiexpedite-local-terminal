// File: session_signal_unix.go
//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// sendInterrupt sends SIGINT to the process on Unix/macOS.
func sendInterrupt(process *os.Process) error {
	if process == nil {
		return fmt.Errorf("process is nil")
	}
	return process.Signal(syscall.SIGINT)
}
