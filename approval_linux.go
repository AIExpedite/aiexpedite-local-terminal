//go:build linux
// +build linux

// File: approval_linux.go
// Linux-specific command approval dialog using zenity or kdialog
package main

import (
	"fmt"
	"os/exec"
	"strings"
)

// ApprovalResult represents the user's decision
type ApprovalResult int

const (
	ApprovalDeny ApprovalResult = iota
	ApprovalOnce
	ApprovalAlways
)

// ShowCommandApprovalDialog displays a Linux dialog via zenity or kdialog
// Returns: ApprovalDeny, ApprovalOnce, or ApprovalAlways
func ShowCommandApprovalDialog(cmd string, args []string, timeoutSec int) ApprovalResult {
	fullCmd := cmd
	if len(args) > 0 {
		fullCmd = cmd + " " + strings.Join(args, " ")
	}

	displayCmd := fullCmd
	if len(displayCmd) > 300 {
		displayCmd = displayCmd[:300] + "..."
	}

	// Try zenity first (GTK - most common)
	if _, err := exec.LookPath("zenity"); err == nil {
		return showZenityDialog(displayCmd, timeoutSec)
	}

	// Fall back to kdialog (KDE)
	if _, err := exec.LookPath("kdialog"); err == nil {
		return showKdialogDialog(displayCmd, timeoutSec)
	}

	// No dialog tool available - deny by default for security
	fmt.Println("[security] No dialog tool (zenity/kdialog) found - denying command")
	return ApprovalDeny
}

func showZenityDialog(displayCmd string, timeoutSec int) ApprovalResult {
	message := fmt.Sprintf(
		"An agent wants to execute a command not in your allow list:\n\n%s\n\n"+
			"Yes = Allow once\n"+
			"Always = Always allow\n"+
			"No = Deny",
		escapeZenityMarkup(displayCmd),
	)

	// zenity --question with extra button for "Always"
	out, err := exec.Command("zenity",
		"--question",
		"--title=AI Expedite - Command Approval",
		"--text="+message,
		"--extra-button=Always",
		"--ok-label=Yes",
		"--cancel-label=No",
		fmt.Sprintf("--timeout=%d", timeoutSec),
		"--width=500",
	).Output()

	outStr := string(out)

	if err != nil {
		// Check if "Always" was clicked (zenity returns extra button text in stdout)
		if strings.TrimSpace(outStr) == "Always" {
			return ApprovalAlways
		}
		// Error or Cancel clicked = Deny
		return ApprovalDeny
	}

	// OK button (Yes) clicked
	return ApprovalOnce
}

func showKdialogDialog(displayCmd string, timeoutSec int) ApprovalResult {
	message := fmt.Sprintf(
		"An agent wants to execute a command not in your allow list:\n\n%s\n\n"+
			"Yes = Allow once\n"+
			"Always = Always allow\n"+
			"No = Deny",
		displayCmd,
	)

	// kdialog with yesnocancel
	cmd := exec.Command("kdialog",
		"--title", "AI Expedite - Command Approval",
		"--yesnocancel", message,
		"--yes-label", "Yes",
		"--no-label", "Always",
		"--cancel-label", "No",
	)

	err := cmd.Run()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	if err != nil && exitCode != 1 && exitCode != 2 {
		// Unexpected error
		fmt.Printf("[security] kdialog error: %v\n", err)
		return ApprovalDeny
	}

	switch exitCode {
	case 0: // Yes button = Allow Once
		return ApprovalOnce
	case 1: // No button = Always Allow
		return ApprovalAlways
	default: // Cancel or other = Deny
		return ApprovalDeny
	}
}
