// File: git_setup.go
//
// Workstation-setup helper that ensures Git is available, installing it via
// the shared dependency runner (WinGet on Windows, brew/apt elsewhere) with
// guided recovery when the installer fails to launch. Git is a warning-level
// dependency for readiness (see readiness.go), so a decline or unrecoverable
// failure here is non-fatal — the agent keeps running and the missing-git
// warning surfaces through the normal readiness path.
package main

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
)

const (
	gitDownloadURL     = "https://git-scm.com/downloads"
	gitTroubleshootURL = "https://git-scm.com/book/en/v2/Getting-Started-Installing-Git"
)

// gitDependencySpec describes Git for the shared install runner. The prompt
// names the actual package manager for the current OS so the consent dialog
// matches what will run (winget / brew / apt).
func gitDependencySpec() DependencySpec {
	return DependencySpec{
		DisplayName: "Git",
		PromptDescription: "Git is used to clone and work with repositories.\n" +
			"It can be installed automatically via " + packageManagerLabel(runtime.GOOS) + ".",
		WingetID:        "Git.Git",
		UnixPackage:     "git",
		VerifyCommand:   "git",
		ManualURL:       gitDownloadURL,
		TroubleshootURL: gitTroubleshootURL,
	}
}

// ensureGit checks whether Git is on PATH and, if not, walks the user through
// the guided install + recovery flow. Non-fatal: it never returns an error to
// abort startup; it logs the outcome and lets readiness report the warning.
func ensureGit() {
	if _, err := exec.LookPath("git"); err == nil {
		return
	}

	switch err := runDependencyInstall(gitDependencySpec()); {
	case err == nil:
		fmt.Printf("%s✓ Git installed successfully%s\n", colorGreen, colorReset)
	case errors.Is(err, errInstallDeclined):
		fmt.Printf("%s[setup] Git install skipped — some workflows that need Git may not work until it's installed.%s\n", colorYellow, colorReset)
	case errors.Is(err, errInstallManual):
		// The runner surfaces the download link either via the browser or, if
		// that can't be launched, a copyable-link dialog — so avoid claiming
		// the browser specifically opened.
		fmt.Printf("%s[setup] Follow the on-screen instructions to install Git manually, then restart.%s\n", colorYellow, colorReset)
	default:
		fmt.Printf("%s[setup] Git could not be installed automatically: %v%s\n", colorYellow, err, colorReset)
	}
}
