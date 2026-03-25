//go:build windows
// +build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// aggressiveCleanup forcefully kills child processes that might be stuck.
// This is necessary for updates when long-running commands (like claude -p) are hanging.
// IMPORTANT: Only kills processes spawned by this application, not other user processes.
func aggressiveCleanup() {
	// Get our own PID - we'll kill our process tree
	myPID := os.Getpid()
	fmt.Printf("[cleanup] Killing child processes of PID %d...\n", myPID)

	// Use taskkill /T to kill our entire process tree (all children)
	// This includes any PowerShell, claude, ttyd processes we spawned
	cmd := exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprintf("%d", myPID))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	// Note: This will fail to kill ourselves (which is fine - we're exiting anyway)
	// but it WILL kill all our child processes
	_ = cmd.Run()

	// Also kill ttyd specifically if we have its PID (belt and suspenders)
	if ttydCmd != nil && ttydCmd.Process != nil {
		pid := ttydCmd.Process.Pid
		fmt.Printf("[cleanup] Killing ttyd process tree (PID %d)...\n", pid)
		cmd := exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprintf("%d", pid))
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		_ = cmd.Run()
	}

	// Brief pause to let processes terminate
	time.Sleep(500 * time.Millisecond)
}
