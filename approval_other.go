//go:build !windows
// +build !windows

// File: approval_other.go
// Non-Windows dialog implementations using osascript (macOS) or zenity/kdialog (Linux).
// Provides the same dialog functions as approval_windows.go so platform-agnostic
// code in main.go, ttyd.go, and pubsub.go compiles and works on all platforms.

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// InstallChoice represents the user's choice for installation
type InstallChoice int

const (
	InstallYes InstallChoice = iota
	InstallNo
	InstallManual
)

// UpdateChoice represents the user's response to update dialog
type UpdateChoice int

const (
	UpdateNow UpdateChoice = iota
	UpdateLater
	SkipVersion
)

// ---------------------------------------------------------------------------
// Info / Error / Success dialogs
// ---------------------------------------------------------------------------

// ShowInfoDialog displays an informational dialog.
func ShowInfoDialog(title, message string) {
	switch runtime.GOOS {
	case "darwin":
		showOsascriptAlert(title, message, "informational")
	case "linux":
		if showLinuxDialog("--info", title, message) {
			return
		}
	}
	// Fallback: console output
	fmt.Printf("[info] %s: %s\n", title, message)
}

// ShowErrorDialog displays an error dialog.
func ShowErrorDialog(title, message string) {
	switch runtime.GOOS {
	case "darwin":
		showOsascriptAlert(title, message, "critical")
	case "linux":
		if showLinuxDialog("--error", title, message) {
			return
		}
	}
	fmt.Printf("[error] %s: %s\n", title, message)
}

// ShowSuccessDialog displays a success dialog.
func ShowSuccessDialog(title, message string) {
	switch runtime.GOOS {
	case "darwin":
		showOsascriptAlert(title, message, "informational")
	case "linux":
		if showLinuxDialog("--info", title, message) {
			return
		}
	}
	fmt.Printf("[success] %s: %s\n", title, message)
}

// ---------------------------------------------------------------------------
// Yes / No dialog
// ---------------------------------------------------------------------------

// ShowYesNoDialog shows a Yes/No confirmation dialog.
// Returns true if user clicks Yes, false otherwise.
func ShowYesNoDialog(title, message string) bool {
	switch runtime.GOOS {
	case "darwin":
		return showOsascriptYesNo(title, message)
	case "linux":
		return showLinuxYesNo(title, message)
	}
	// Fallback: console prompt
	return consoleYesNo(title, message)
}

// ---------------------------------------------------------------------------
// Update dialog (3 choices: Now / Later / Skip)
// ---------------------------------------------------------------------------

// ShowUpdateDialog shows a dialog asking to update with 3 options.
// Returns UpdateNow, UpdateLater, or SkipVersion.
func ShowUpdateDialog(currentVersion, newVersion string) UpdateChoice {
	message := fmt.Sprintf(
		"A new version of AI Expedite Terminal is available!\n\n"+
			"Current version: %s\nNew version: %s\n\n"+
			"Would you like to update now?",
		currentVersion, newVersion)
	title := "AI Expedite Terminal - Update Available"

	switch runtime.GOOS {
	case "darwin":
		return showOsascriptUpdateDialog(title, message)
	case "linux":
		return showLinuxUpdateDialog(title, message)
	}
	return consoleUpdateDialog(title, message)
}

// ---------------------------------------------------------------------------
// Install prompt (3 choices: Yes / No / Manual)
// ---------------------------------------------------------------------------

// ShowInstallPrompt displays a dialog asking user permission to install a dependency.
// Returns InstallYes, InstallNo, or InstallManual.
func ShowInstallPrompt(component, description string) InstallChoice {
	message := fmt.Sprintf(
		"%s is required but not installed.\n\n%s\n\n"+
			"Would you like to install it automatically?",
		component, description)
	title := "AI Expedite Terminal - Setup Required"

	switch runtime.GOOS {
	case "darwin":
		return showOsascriptInstallPrompt(title, message)
	case "linux":
		return showLinuxInstallPrompt(title, message)
	}
	return consoleInstallPrompt(title, message)
}

// ===========================================================================
// macOS (osascript) implementations
// ===========================================================================

func escapeOsascript(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	return s
}

func showOsascriptAlert(title, message, icon string) {
	script := fmt.Sprintf(
		`display alert "%s" message "%s" as %s`,
		escapeOsascript(title), escapeOsascript(message), icon)
	_ = exec.Command("osascript", "-e", script).Run()
}

func showOsascriptYesNo(title, message string) bool {
	script := fmt.Sprintf(
		`display dialog "%s" buttons {"No", "Yes"} default button "No" with title "%s" with icon caution`,
		escapeOsascript(message), escapeOsascript(title))
	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "Yes")
}

func showOsascriptUpdateDialog(title, message string) UpdateChoice {
	script := fmt.Sprintf(
		`display dialog "%s" buttons {"Skip Version", "Later", "Update Now"} default button "Update Now" with title "%s" with icon note`,
		escapeOsascript(message), escapeOsascript(title))
	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
		return SkipVersion
	}
	result := string(out)
	switch {
	case strings.Contains(result, "Update Now"):
		return UpdateNow
	case strings.Contains(result, "Later"):
		return UpdateLater
	default:
		return SkipVersion
	}
}

func showOsascriptInstallPrompt(title, message string) InstallChoice {
	script := fmt.Sprintf(
		`display dialog "%s" buttons {"Open Download Page", "Exit", "Install"} default button "Install" with title "%s" with icon note`,
		escapeOsascript(message), escapeOsascript(title))
	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
		return InstallNo
	}
	result := string(out)
	switch {
	case strings.Contains(result, "Install"):
		return InstallYes
	case strings.Contains(result, "Open Download Page"):
		return InstallManual
	default:
		return InstallNo
	}
}

// ===========================================================================
// Linux (zenity / kdialog) implementations
// ===========================================================================

// escapeZenityMarkup escapes characters that zenity interprets as Pango markup.
func escapeZenityMarkup(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// showLinuxDialog shows a simple zenity/kdialog dialog. Returns true if shown.
func showLinuxDialog(zenityType, title, message string) bool {
	if _, err := exec.LookPath("zenity"); err == nil {
		_ = exec.Command("zenity", zenityType,
			"--title="+title,
			"--text="+escapeZenityMarkup(message),
			"--width=400").Run()
		return true
	}
	if _, err := exec.LookPath("kdialog"); err == nil {
		kType := "--sorry" // default
		if zenityType == "--error" {
			kType = "--error"
		} else if zenityType == "--info" {
			kType = "--msgbox"
		}
		_ = exec.Command("kdialog", "--title", title, kType, message).Run()
		return true
	}
	return false
}

func showLinuxYesNo(title, message string) bool {
	if _, err := exec.LookPath("zenity"); err == nil {
		err := exec.Command("zenity", "--question",
			"--title="+title,
			"--text="+escapeZenityMarkup(message),
			"--ok-label=Yes",
			"--cancel-label=No",
			"--width=400").Run()
		return err == nil // exit 0 = Yes
	}
	if _, err := exec.LookPath("kdialog"); err == nil {
		err := exec.Command("kdialog", "--title", title,
			"--yesno", message).Run()
		return err == nil // exit 0 = Yes
	}
	return consoleYesNo(title, message)
}

func showLinuxUpdateDialog(title, message string) UpdateChoice {
	if _, err := exec.LookPath("zenity"); err == nil {
		out, err := exec.Command("zenity", "--question",
			"--title="+title,
			"--text="+escapeZenityMarkup(message)+"\n\nYes = Update now\nLater = Remind later\nSkip = Skip this version",
			"--ok-label=Update Now",
			"--cancel-label=Skip Version",
			"--extra-button=Later",
			"--width=500").Output()
		if err == nil {
			return UpdateNow
		}
		if strings.TrimSpace(string(out)) == "Later" {
			return UpdateLater
		}
		return SkipVersion
	}
	if _, err := exec.LookPath("kdialog"); err == nil {
		cmd := exec.Command("kdialog", "--title", title,
			"--yesnocancel", message+"\n\nYes = Update now\nNo = Later\nCancel = Skip version")
		_ = cmd.Run()
		if cmd.ProcessState != nil {
			switch cmd.ProcessState.ExitCode() {
			case 0:
				return UpdateNow
			case 1:
				return UpdateLater
			}
		}
		return SkipVersion
	}
	return consoleUpdateDialog(title, message)
}

func showLinuxInstallPrompt(title, message string) InstallChoice {
	if _, err := exec.LookPath("zenity"); err == nil {
		out, err := exec.Command("zenity", "--question",
			"--title="+title,
			"--text="+escapeZenityMarkup(message)+"\n\nYes = Install automatically\nNo = Exit\nManual = Open download page",
			"--ok-label=Install",
			"--cancel-label=Exit",
			"--extra-button=Open Download Page",
			"--width=500").Output()
		if err == nil {
			return InstallYes
		}
		if strings.TrimSpace(string(out)) == "Open Download Page" {
			return InstallManual
		}
		return InstallNo
	}
	if _, err := exec.LookPath("kdialog"); err == nil {
		cmd := exec.Command("kdialog", "--title", title,
			"--yesnocancel", message+"\n\nYes = Install\nNo = Open download page\nCancel = Exit")
		_ = cmd.Run()
		if cmd.ProcessState != nil {
			switch cmd.ProcessState.ExitCode() {
			case 0:
				return InstallYes
			case 1:
				return InstallManual
			}
		}
		return InstallNo
	}
	return consoleInstallPrompt(title, message)
}

// ===========================================================================
// Console fallbacks (when no GUI tool is available)
// ===========================================================================

func consoleYesNo(title, message string) bool {
	fmt.Printf("\n[%s]\n%s\n\nType 'yes' to confirm: ", title, message)
	reader := bufio.NewReader(os.Stdin)
	input, _ := reader.ReadString('\n')
	return strings.TrimSpace(strings.ToLower(input)) == "yes"
}

func consoleUpdateDialog(title, message string) UpdateChoice {
	fmt.Printf("\n[%s]\n%s\n\n", title, message)
	fmt.Println("  1) Update now")
	fmt.Println("  2) Later")
	fmt.Println("  3) Skip this version")
	fmt.Print("\nChoice [1/2/3]: ")
	reader := bufio.NewReader(os.Stdin)
	input, _ := reader.ReadString('\n')
	switch strings.TrimSpace(input) {
	case "1":
		return UpdateNow
	case "2":
		return UpdateLater
	default:
		return SkipVersion
	}
}

func consoleInstallPrompt(title, message string) InstallChoice {
	fmt.Printf("\n[%s]\n%s\n\n", title, message)
	fmt.Println("  1) Install automatically")
	fmt.Println("  2) Exit (install manually)")
	fmt.Println("  3) Open download page")
	fmt.Print("\nChoice [1/2/3]: ")
	reader := bufio.NewReader(os.Stdin)
	input, _ := reader.ReadString('\n')
	switch strings.TrimSpace(input) {
	case "1":
		return InstallYes
	case "3":
		return InstallManual
	default:
		return InstallNo
	}
}
