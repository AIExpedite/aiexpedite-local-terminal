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
		"A new version of AI Expedite is available!\n\n"+
			"Current version: %s\nNew version: %s\n\n"+
			"Would you like to update now?",
		currentVersion, newVersion)
	title := "AI Expedite - Update Available"

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
// optional selects the wording for a dependency the app runs without (see
// DependencySpec.Optional): declining then means "skip and continue", not
// "exit", so the decline button is labelled accordingly.
// Returns InstallYes, InstallNo, or InstallManual.
func ShowInstallPrompt(component, description string, optional bool) InstallChoice {
	message := fmt.Sprintf(
		"%s\n\n%s\n\nWould you like to install it automatically?",
		installPromptHeadline(component, optional), description)
	title := installPromptTitle(optional)
	declineBtn := installDeclineButton(optional)
	declineLabel := installDeclineLabel(component, optional)

	switch runtime.GOOS {
	case "darwin":
		return showOsascriptInstallPrompt(title, message, declineBtn)
	case "linux":
		return showLinuxInstallPrompt(title, message, declineBtn, declineLabel)
	}
	return consoleInstallPrompt(title, message, declineLabel)
}

// ---------------------------------------------------------------------------
// Install recovery dialog (Retry / Manual / Troubleshoot / Exit)
// ---------------------------------------------------------------------------

// ShowInstallRecovery displays the guided recovery dialog after an install
// attempt fails. Retry is only offered when allowRetry is true (the runner
// caps automatic retries). The InstallRecoveryChoice enum is shared (defined
// in install_runner.go). Mirrors the ShowInstallPrompt multi-choice pattern.
func ShowInstallRecovery(component, explanation, diagnostics string, allowRetry bool) InstallRecoveryChoice {
	title := "AI Expedite - Setup Problem"
	message := fmt.Sprintf("%s could not be installed.\n\n%s\n\n%s",
		component, explanation, diagnostics)

	switch runtime.GOOS {
	case "darwin":
		return showOsascriptInstallRecovery(title, message, allowRetry)
	case "linux":
		return showLinuxInstallRecovery(title, message, allowRetry)
	}
	return consoleInstallRecovery(title, message, allowRetry)
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

func showOsascriptInstallPrompt(title, message, declineBtn string) InstallChoice {
	script := fmt.Sprintf(
		`display dialog "%s" buttons {"Open Download Page", "%s", "Install"} default button "Install" with title "%s" with icon note`,
		escapeOsascript(message), escapeOsascript(declineBtn), escapeOsascript(title))
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

func showOsascriptInstallRecovery(title, message string, allowRetry bool) InstallRecoveryChoice {
	// macOS `display dialog` allows at most three buttons, and Escape only
	// dismisses (returning an error osascript maps to Exit) when a `cancel
	// button` is designated. Skip is therefore always a real, designated cancel
	// button so users can bail out at any time — including while retries remain.
	// With retries offered the three slots are Skip / Install Manually / Retry;
	// Troubleshooting takes Retry's slot once the retry cap is reached (it stays
	// reachable across the recovery loop, which re-shows this dialog).
	buttons := `{"Skip", "Install Manually", "Retry"}`
	defaultBtn := "Retry"
	if !allowRetry {
		buttons = `{"Skip", "Install Manually", "Troubleshooting"}`
		defaultBtn = "Install Manually"
	}
	script := fmt.Sprintf(
		`display dialog "%s" buttons %s default button "%s" cancel button "Skip" with title "%s" with icon caution`,
		escapeOsascript(message), buttons, defaultBtn, escapeOsascript(title))
	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
		return RecoveryExit // Skip button, Escape, or dismissed / cancelled
	}
	result := string(out)
	switch {
	case allowRetry && strings.Contains(result, "Retry"):
		return RecoveryRetry
	case strings.Contains(result, "Install Manually"):
		return RecoveryManual
	case strings.Contains(result, "Troubleshooting"):
		return RecoveryTroubleshoot
	default:
		return RecoveryExit
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

func showLinuxInstallPrompt(title, message, declineBtn, declineLabel string) InstallChoice {
	if _, err := exec.LookPath("zenity"); err == nil {
		out, err := exec.Command("zenity", "--question",
			"--title="+title,
			"--text="+escapeZenityMarkup(message)+"\n\nYes = Install automatically\nNo = "+escapeZenityMarkup(declineLabel)+"\nManual = Open download page",
			"--ok-label=Install",
			"--cancel-label="+declineBtn,
			"--extra-button=Open Download Page",
			"--width=500").Output()
		// The extra button writes its label to stdout and may still exit 0, so
		// check the label before treating a successful exit as the OK button.
		if strings.TrimSpace(string(out)) == "Open Download Page" {
			return InstallManual
		}
		if err == nil {
			return InstallYes
		}
		return InstallNo
	}
	if _, err := exec.LookPath("kdialog"); err == nil {
		cmd := exec.Command("kdialog", "--title", title,
			"--yesnocancel", message+"\n\nYes = Install\nNo = Open download page\nCancel = "+declineLabel)
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
	return consoleInstallPrompt(title, message, declineLabel)
}

func showLinuxInstallRecovery(title, message string, allowRetry bool) InstallRecoveryChoice {
	if _, err := exec.LookPath("zenity"); err == nil {
		okLabel := "Retry"
		if !allowRetry {
			okLabel = "Install Manually"
		}
		zargs := []string{
			"--question",
			"--title=" + title,
			"--text=" + escapeZenityMarkup(message),
			"--ok-label=" + okLabel,
			"--cancel-label=Skip",
			"--width=520",
		}
		if allowRetry {
			zargs = append(zargs, "--extra-button=Install Manually")
		}
		zargs = append(zargs, "--extra-button=Troubleshooting")

		out, err := exec.Command("zenity", zargs...).Output()
		// An --extra-button selection writes its label to stdout and may still
		// exit 0, so inspect the label before treating a successful exit as the
		// OK button; otherwise extra buttons get misread as Retry/Manual.
		switch strings.TrimSpace(string(out)) {
		case "Install Manually":
			return RecoveryManual
		case "Troubleshooting":
			return RecoveryTroubleshoot
		}
		if err == nil {
			// OK button (no extra-button label on stdout).
			if allowRetry {
				return RecoveryRetry
			}
			return RecoveryManual
		}
		return RecoveryExit // cancel/skip
	}
	if _, err := exec.LookPath("kdialog"); err == nil {
		// kdialog --yesnocancel only exposes three outcomes, which drops the
		// troubleshooting option; use --menu so every recovery choice
		// (including RecoveryTroubleshoot) is reachable. The selected tag is
		// written to stdout; a cancel/dismiss exits non-zero.
		margs := []string{"--title", title, "--menu", message}
		if allowRetry {
			margs = append(margs, "retry", "Retry the automatic install")
		}
		margs = append(margs,
			"manual", "Install manually (open download page)",
			"troubleshoot", "View troubleshooting guidance",
			"skip", "Skip for now")
		out, err := exec.Command("kdialog", margs...).Output()
		if err != nil {
			return RecoveryExit // cancelled / dismissed
		}
		switch strings.TrimSpace(string(out)) {
		case "retry":
			if allowRetry {
				return RecoveryRetry
			}
			return RecoveryExit
		case "manual":
			return RecoveryManual
		case "troubleshoot":
			return RecoveryTroubleshoot
		}
		return RecoveryExit
	}
	return consoleInstallRecovery(title, message, allowRetry)
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

func consoleInstallPrompt(title, message, declineLabel string) InstallChoice {
	fmt.Printf("\n[%s]\n%s\n\n", title, message)
	fmt.Println("  1) Install automatically")
	fmt.Println("  2) " + declineLabel)
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

// consoleInstallRecovery maps a numeric choice to an InstallRecoveryChoice.
// Retry (option 1) is only listed when allowRetry is true; otherwise the menu
// starts at Install Manually.
func consoleInstallRecovery(title, message string, allowRetry bool) InstallRecoveryChoice {
	fmt.Printf("\n[%s]\n%s\n\n", title, message)
	if allowRetry {
		fmt.Println("  1) Retry the automatic install")
	}
	fmt.Println("  2) Install manually (open download page)")
	fmt.Println("  3) View troubleshooting guidance")
	fmt.Println("  4) Skip for now")
	fmt.Print("\nChoice [1/2/3/4]: ")
	reader := bufio.NewReader(os.Stdin)
	input, _ := reader.ReadString('\n')
	switch strings.TrimSpace(input) {
	case "1":
		if allowRetry {
			return RecoveryRetry
		}
		return RecoveryExit
	case "2":
		return RecoveryManual
	case "3":
		return RecoveryTroubleshoot
	default:
		return RecoveryExit
	}
}
