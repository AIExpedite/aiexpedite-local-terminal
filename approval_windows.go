//go:build windows
// +build windows

// File: approval_windows.go
// Windows-specific command approval dialog using Win32 MessageBox
package main

import (
	"fmt"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	user32      = syscall.NewLazyDLL("user32.dll")
	procMsgBox  = user32.NewProc("MessageBoxW")
)

const (
	MB_YESNOCANCEL   = 0x00000003
	MB_ICONWARNING   = 0x00000030
	MB_DEFBUTTON3    = 0x00000200 // Default to Cancel (No/Deny)
	MB_SETFOREGROUND = 0x00010000
	MB_SYSTEMMODAL   = 0x00001000
	MB_TOPMOST       = 0x00040000

	IDYES    = 6
	IDNO     = 7
	IDCANCEL = 2
)

// ApprovalResult represents the user's decision
type ApprovalResult int

const (
	ApprovalDeny ApprovalResult = iota
	ApprovalOnce
	ApprovalAlways
)

// ShowCommandApprovalDialog displays a Win32 MessageBox asking user to approve a command
// Buttons mapping:
//   - Yes = Allow this once
//   - No = Always allow this command pattern
//   - Cancel = Deny execution
// Returns: ApprovalDeny, ApprovalOnce, or ApprovalAlways
func ShowCommandApprovalDialog(cmd string, args []string, timeoutSec int) ApprovalResult {
	fullCmd := cmd
	if len(args) > 0 {
		fullCmd = cmd + " " + strings.Join(args, " ")
	}

	// Truncate very long commands for display
	displayCmd := fullCmd
	if len(displayCmd) > 300 {
		displayCmd = displayCmd[:300] + "..."
	}

	message := fmt.Sprintf(
		"An agent wants to execute a command not in your allow list:\n\n%s\n\n"+
			"Click:\n"+
			"  YES = Allow this once\n"+
			"  NO = Always allow this command\n"+
			"  CANCEL = Deny execution\n\n"+
			"(Dialog will auto-close in %d seconds)",
		displayCmd,
		timeoutSec,
	)

	title := "AI Expedite - Command Approval"

	// Result channel for timeout handling
	resultCh := make(chan ApprovalResult, 1)

	go func() {
		titlePtr, _ := syscall.UTF16PtrFromString(title)
		msgPtr, _ := syscall.UTF16PtrFromString(message)

		flags := uint32(MB_YESNOCANCEL | MB_ICONWARNING | MB_DEFBUTTON3 | MB_SETFOREGROUND | MB_SYSTEMMODAL | MB_TOPMOST)

		ret, _, _ := procMsgBox.Call(
			0,
			uintptr(unsafe.Pointer(msgPtr)),
			uintptr(unsafe.Pointer(titlePtr)),
			uintptr(flags),
		)

		switch int(ret) {
		case IDYES:
			resultCh <- ApprovalOnce
		case IDNO:
			resultCh <- ApprovalAlways
		default: // IDCANCEL or dialog closed
			resultCh <- ApprovalDeny
		}
	}()

	// Wait for result or timeout
	select {
	case result := <-resultCh:
		return result
	case <-time.After(time.Duration(timeoutSec) * time.Second):
		// Timeout - return deny (caller can override based on config)
		fmt.Println("[security] Approval dialog timed out")
		return ApprovalDeny
	}
}
