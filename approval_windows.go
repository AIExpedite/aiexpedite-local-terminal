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
	MB_OK            = 0x00000000
	MB_OKCANCEL      = 0x00000001
	MB_YESNO         = 0x00000004
	MB_YESNOCANCEL   = 0x00000003
	MB_ICONERROR     = 0x00000010
	MB_ICONQUESTION  = 0x00000020
	MB_ICONWARNING   = 0x00000030
	MB_ICONINFO      = 0x00000040
	MB_DEFBUTTON1    = 0x00000000 // Default to first button
	MB_DEFBUTTON2    = 0x00000100 // Default to second button
	MB_DEFBUTTON3    = 0x00000200 // Default to Cancel (No/Deny)
	MB_SETFOREGROUND = 0x00010000
	MB_SYSTEMMODAL   = 0x00001000
	MB_TOPMOST       = 0x00040000

	IDOK     = 1
	IDCANCEL = 2
	IDYES    = 6
	IDNO     = 7
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

/* --------------------------------------------------------------------------
   Installation Dialogs
   -------------------------------------------------------------------------- */

// InstallChoice represents the user's choice for installation
type InstallChoice int

const (
	InstallYes InstallChoice = iota
	InstallNo
	InstallManual
)

// ShowInstallPrompt displays a dialog asking user permission to install a dependency
// Returns: InstallYes (auto-install), InstallNo (exit), InstallManual (open download page)
func ShowInstallPrompt(component, description string) InstallChoice {
	message := fmt.Sprintf(
		"%s is required but not installed.\n\n"+
			"%s\n\n"+
			"Would you like to install it automatically?\n\n"+
			"Click:\n"+
			"  YES = Install automatically (via winget)\n"+
			"  NO = Exit and install manually\n"+
			"  CANCEL = Open download page",
		component,
		description,
	)

	title := "AI Expedite Terminal - Setup Required"

	titlePtr, _ := syscall.UTF16PtrFromString(title)
	msgPtr, _ := syscall.UTF16PtrFromString(message)

	flags := uint32(MB_YESNOCANCEL | MB_ICONQUESTION | MB_DEFBUTTON1 | MB_SETFOREGROUND | MB_TOPMOST)

	ret, _, _ := procMsgBox.Call(
		0,
		uintptr(unsafe.Pointer(msgPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		uintptr(flags),
	)

	switch int(ret) {
	case IDYES:
		return InstallYes
	case IDNO:
		return InstallNo
	default: // IDCANCEL
		return InstallManual
	}
}

// ShowInstallProgress displays an info dialog during installation
// This is non-blocking - just shows a notification
func ShowInfoDialog(title, message string) {
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	msgPtr, _ := syscall.UTF16PtrFromString(message)

	flags := uint32(MB_OK | MB_ICONINFO | MB_SETFOREGROUND | MB_TOPMOST)

	procMsgBox.Call(
		0,
		uintptr(unsafe.Pointer(msgPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		uintptr(flags),
	)
}

// ShowErrorDialog displays an error dialog
func ShowErrorDialog(title, message string) {
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	msgPtr, _ := syscall.UTF16PtrFromString(message)

	flags := uint32(MB_OK | MB_ICONERROR | MB_SETFOREGROUND | MB_TOPMOST)

	procMsgBox.Call(
		0,
		uintptr(unsafe.Pointer(msgPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		uintptr(flags),
	)
}

// ShowSuccessDialog displays a success dialog
func ShowSuccessDialog(title, message string) {
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	msgPtr, _ := syscall.UTF16PtrFromString(message)

	flags := uint32(MB_OK | MB_ICONINFO | MB_SETFOREGROUND | MB_TOPMOST)

	procMsgBox.Call(
		0,
		uintptr(unsafe.Pointer(msgPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		uintptr(flags),
	)
}
