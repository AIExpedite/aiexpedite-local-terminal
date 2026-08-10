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
	user32           = syscall.NewLazyDLL("user32.dll")
	procMsgBox       = user32.NewProc("MessageBoxW")
	procFindWindowW  = user32.NewProc("FindWindowW")
	procPostMessageW = user32.NewProc("PostMessageW")
)

const WM_CLOSE = 0x0010

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
//
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
		// Timeout — programmatically close the MessageBox so its goroutine can exit
		// rather than leaking it as a zombie waiting for user input nobody will give.
		// FindWindowW with a NULL class finds the top-level window matching the title.
		titlePtr, _ := syscall.UTF16PtrFromString(title)
		hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(titlePtr)))
		if hwnd != 0 {
			procPostMessageW.Call(hwnd, WM_CLOSE, 0, 0)
		}
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
	InstallCancel
)

// ShowInstallPrompt displays a dialog asking user permission to install a dependency.
// optional selects wording that accurately describes dependencies the app can
// run without (CANCEL is then "skip and continue", not "exit"); it does not
// change the Yes / No / Cancel button semantics.
// Returns:
//   - InstallYes: install automatically
//   - InstallNo: open the download page, then exit the install flow
//   - InstallCancel: cancel only — close the dialog and take no other action
func ShowInstallPrompt(component, description string, optional bool) InstallChoice {
	message := fmt.Sprintf(
		"%s\n\n"+
			"%s\n\n"+
			"Would you like to install it automatically?\n\n"+
			"Click:\n"+
			"  YES = Install automatically (via winget)\n"+
			"  NO = %s\n"+
			"  CANCEL = %s",
		installPromptHeadline(component, optional),
		description,
		installManualLabel(component),
		installCancelLabel(component, optional),
	)

	title := installPromptTitle(optional)

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
	default: // IDCANCEL or dialog closed
		return InstallCancel
	}
}

// ShowInstallRecovery displays the guided recovery dialog shown after an
// install attempt fails. The Win32 MessageBox exposes at most three buttons,
// so Retry/Manual are the buttons and troubleshooting guidance is reachable
// via the third button (which re-shows or opens docs through the caller's
// recovery loop). Mapping:
//   - Yes    = Retry (only offered when allowRetry; otherwise "View troubleshooting")
//   - No     = Install manually (open download page)
//   - Cancel = Exit
//
// The InstallRecoveryChoice enum is shared (defined in install_runner.go).
func ShowInstallRecovery(component, explanation, diagnostics string, allowRetry bool) InstallRecoveryChoice {
	var firstButton string
	if allowRetry {
		firstButton = "  YES = Retry the automatic install"
	} else {
		firstButton = "  YES = View troubleshooting guidance"
	}

	message := fmt.Sprintf(
		"%s could not be installed.\n\n"+
			"%s\n\n"+
			"%s\n\n"+
			"Click:\n"+
			"%s\n"+
			"  NO = Install manually (open download page)\n"+
			"  CANCEL = Skip for now",
		component, explanation, diagnostics, firstButton)

	title := "AI Expedite - Setup Problem"

	titlePtr, _ := syscall.UTF16PtrFromString(title)
	msgPtr, _ := syscall.UTF16PtrFromString(message)

	flags := uint32(MB_YESNOCANCEL | MB_ICONWARNING | MB_DEFBUTTON1 | MB_SETFOREGROUND | MB_TOPMOST)

	ret, _, _ := procMsgBox.Call(
		0,
		uintptr(unsafe.Pointer(msgPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		uintptr(flags),
	)

	switch int(ret) {
	case IDYES:
		if allowRetry {
			return RecoveryRetry
		}
		return RecoveryTroubleshoot
	case IDNO:
		return RecoveryManual
	default: // IDCANCEL or dialog closed
		return RecoveryExit
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

// ShowYesNoDialog shows a Yes/No confirmation dialog
// Returns true if user clicks Yes, false otherwise
func ShowYesNoDialog(title, message string) bool {
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	msgPtr, _ := syscall.UTF16PtrFromString(message)

	flags := uint32(MB_YESNO | MB_ICONQUESTION | MB_DEFBUTTON2 | MB_SETFOREGROUND | MB_TOPMOST)

	ret, _, _ := procMsgBox.Call(
		0,
		uintptr(unsafe.Pointer(msgPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		uintptr(flags),
	)

	return int(ret) == IDYES
}

/* --------------------------------------------------------------------------
   Update Dialog
   -------------------------------------------------------------------------- */

// UpdateChoice represents the user's response to update dialog
type UpdateChoice int

const (
	UpdateNow UpdateChoice = iota
	UpdateLater
	SkipVersion
)

// ShowUpdateDialog shows a dialog asking to update with 3 options.
// Uses Yes/No/Cancel buttons mapped as:
//   - Yes = Update Now
//   - No = Later (remind via tray menu)
//   - Cancel = Skip this version
//
// Returns UpdateNow, UpdateLater, or SkipVersion
func ShowUpdateDialog(currentVersion, newVersion string) UpdateChoice {
	message := fmt.Sprintf(
		"A new version of AI Expedite is available!\n\n"+
			"Current version: %s\n"+
			"New version: %s\n\n"+
			"Would you like to update now?\n\n"+
			"Click:\n"+
			"  YES = Update now (app will restart)\n"+
			"  NO = Remind me later (via tray menu)\n"+
			"  CANCEL = Skip this version",
		currentVersion, newVersion)

	title := "AI Expedite - Update Available"

	titlePtr, _ := syscall.UTF16PtrFromString(title)
	msgPtr, _ := syscall.UTF16PtrFromString(message)

	flags := uint32(MB_YESNOCANCEL | MB_ICONINFO | MB_DEFBUTTON1 | MB_SETFOREGROUND | MB_TOPMOST)

	ret, _, _ := procMsgBox.Call(
		0,
		uintptr(unsafe.Pointer(msgPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		uintptr(flags),
	)

	switch int(ret) {
	case IDYES:
		return UpdateNow
	case IDNO:
		return UpdateLater
	default: // IDCANCEL or dialog closed
		return SkipVersion
	}
}
