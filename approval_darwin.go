//go:build darwin
// +build darwin

// File: approval_darwin.go
// macOS-specific command approval dialog using osascript
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

// ShowCommandApprovalDialog displays a macOS dialog via osascript
// Buttons: [No] [Always] [Yes]
// Returns: ApprovalDeny, ApprovalOnce, or ApprovalAlways
func ShowCommandApprovalDialog(cmd string, args []string, timeoutSec int) ApprovalResult {
	fullCmd := cmd
	if len(args) > 0 {
		fullCmd = cmd + " " + strings.Join(args, " ")
	}

	// Truncate for display
	displayCmd := fullCmd
	if len(displayCmd) > 300 {
		displayCmd = displayCmd[:300] + "..."
	}

	// Escape special characters for AppleScript
	displayCmd = strings.ReplaceAll(displayCmd, `\`, `\\`)
	displayCmd = strings.ReplaceAll(displayCmd, `"`, `\"`)

	script := fmt.Sprintf(`
		display dialog "An agent wants to execute a command not in your allow list:\n\n%s\n\nYes = Allow once\nAlways = Always allow\nNo = Deny" buttons {"No", "Always", "Yes"} default button "No" with icon caution with title "AI Expedite - Command Approval" giving up after %d
	`, displayCmd, timeoutSec)

	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
		// Timeout or error = deny
		fmt.Printf("[security] osascript dialog error or timeout: %v\n", err)
		return ApprovalDeny
	}

	result := string(out)
	switch {
	case strings.Contains(result, "Yes"):
		return ApprovalOnce
	case strings.Contains(result, "Always"):
		return ApprovalAlways
	default: // "No" or gave up (timeout)
		return ApprovalDeny
	}
}
