package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const ttydDownloadURL = "https://github.com/tsl0922/ttyd/releases"

// Seams for testing the install-choice side effects without opening a real
// browser or terminating the test process.
var (
	openBrowserFn = openBrowser
	exitFn        = os.Exit
)

// handleInstallChoice applies the side effects for the user's install-prompt
// choice and reports whether the automatic installation should proceed.
//   - InstallYes:    returns (true, nil); the caller continues the winget/scoop install.
//   - InstallNo:     opens the dependency's download page, then exits the flow
//     (manual installation path). The dialog does not proceed with auto-install.
//   - InstallCancel: cancel only — no browser, no install; returns an error so the
//     caller aborts (the app cannot run without ttyd).
func handleInstallChoice(choice InstallChoice) (proceed bool, err error) {
	switch choice {
	case InstallNo:
		openBrowserFn(ttydDownloadURL)
		exitFn(0)
		return false, nil
	case InstallCancel:
		return false, errors.New("ttyd installation cancelled by user")
	default: // InstallYes
		return true, nil
	}
}

// ensureTtyd verifies ttyd is on PATH or tries to install it automatically.
func ensureTtyd() error {
	if checkTtydInstalled() {
		return nil
	}

	switch runtime.GOOS {
	case "windows":
		return installTtydWindows()
	case "darwin":
		return installTtydDarwin()
	default: // linux
		return installTtydLinux()
	}
}

// checkTtydInstalled checks if ttyd is available on PATH or in common install locations
func checkTtydInstalled() bool {
	// First check PATH
	if _, err := exec.LookPath("ttyd"); err == nil {
		return true
	}

	// On Windows, check common installation paths
	if runtime.GOOS == "windows" {
		commonPaths := []string{
			filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "WinGet", "Packages", "tsl0922.ttyd_Microsoft.Winget.Source_8wekyb3d8bbwe", "ttyd.exe"),
			filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "WinGet", "Links", "ttyd.exe"),
			filepath.Join(os.Getenv("ProgramFiles"), "ttyd", "ttyd.exe"),
			filepath.Join(os.Getenv("USERPROFILE"), "scoop", "apps", "ttyd", "current", "ttyd.exe"),
		}

		for _, path := range commonPaths {
			if _, err := os.Stat(path); err == nil {
				fmt.Printf("Found ttyd at: %s\n", path)
				// Add the directory to PATH for this process
				dir := filepath.Dir(path)
				os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
				return true
			}
		}
	}

	return false
}

// installTtydWindows attempts to install ttyd on Windows via winget or scoop
// Shows user-friendly dialogs and progress
func installTtydWindows() error {
	// Show install prompt dialog
	choice := ShowInstallPrompt(
		"ttyd (Terminal Server)",
		"ttyd provides the web-based terminal interface.\n"+
			"It will be installed via Windows Package Manager (winget).",
	)

	// No -> open download page and exit; Cancel -> abort with no action.
	// Only InstallYes falls through to the automatic winget/scoop install below.
	if proceed, err := handleInstallChoice(choice); !proceed {
		return err
	}

	// Show console during installation
	showConsoleWindow(true)

	fmt.Println("")
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║           Installing ttyd - Please wait...                 ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println("")

	// Try winget first
	if _, err := exec.LookPath("winget"); err == nil {
		fmt.Println("→ Using Windows Package Manager (winget)...")
		fmt.Println("→ Running: winget install tsl0922.ttyd")
		fmt.Println("")

		cmd := exec.Command("winget", "install",
			"-e", "--id", "tsl0922.ttyd",
			"--accept-package-agreements",
			"--accept-source-agreements",
			"--disable-interactivity")

		// Show output in real-time
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		err := cmd.Run()
		if err != nil {
			fmt.Printf("\n⚠ winget install returned: %v\n", err)
		}

		// Check if ttyd is now available
		if checkTtydInstalled() {
			fmt.Println("")
			fmt.Println("╔════════════════════════════════════════════════════════════╗")
			fmt.Println("║              ✓ ttyd installed successfully!                ║")
			fmt.Println("╚════════════════════════════════════════════════════════════╝")
			fmt.Println("")
			return nil
		}
	}

	// Try scoop as fallback
	if _, err := exec.LookPath("scoop"); err == nil {
		fmt.Println("")
		fmt.Println("→ Trying Scoop as fallback...")
		fmt.Println("→ Running: scoop install ttyd")
		fmt.Println("")

		cmd := exec.Command("scoop", "install", "ttyd")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		err := cmd.Run()
		if err != nil {
			fmt.Printf("\n⚠ scoop install returned: %v\n", err)
		}

		if checkTtydInstalled() {
			fmt.Println("")
			fmt.Println("╔════════════════════════════════════════════════════════════╗")
			fmt.Println("║              ✓ ttyd installed successfully!                ║")
			fmt.Println("╚════════════════════════════════════════════════════════════╝")
			fmt.Println("")
			return nil
		}
	}

	// Installation failed
	fmt.Println("")
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║              ✗ Installation failed                         ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println("")
	fmt.Println("Please install ttyd manually:")
	fmt.Println("  Option 1: winget install tsl0922.ttyd")
	fmt.Println("  Option 2: scoop install ttyd")
	fmt.Println("  Option 3: Download from", ttydDownloadURL)
	fmt.Println("")

	ShowErrorDialog(
		"Installation Failed",
		"ttyd could not be installed automatically.\n\n"+
			"Please install it manually:\n"+
			"  winget install tsl0922.ttyd\n\n"+
			"Then restart this application.",
	)

	return errors.New("ttyd installation failed")
}

// installTtydDarwin attempts to install ttyd on macOS via Homebrew
func installTtydDarwin() error {
	if _, err := exec.LookPath("brew"); err == nil {
		fmt.Println("Attempting to install ttyd via Homebrew...")
		cmd := exec.Command("brew", "install", "ttyd")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err == nil {
			if _, err := exec.LookPath("ttyd"); err == nil {
				fmt.Println("ttyd installed successfully via Homebrew!")
				return nil
			}
		}
	}
	return errors.New("ttyd not found. Please install it manually:\n" +
		"  brew install ttyd\n" +
		"Then restart this application.")
}

// installTtydLinux attempts to install ttyd on Linux via apt or snap
func installTtydLinux() error {
	if _, err := exec.LookPath("apt-get"); err == nil {
		fmt.Println("Attempting to install ttyd via apt-get...")
		cmd := exec.Command("sudo", "apt-get", "-y", "install", "ttyd")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err == nil {
			if _, err := exec.LookPath("ttyd"); err == nil {
				fmt.Println("ttyd installed successfully via apt!")
				return nil
			}
		}
	}
	if _, err := exec.LookPath("snap"); err == nil {
		fmt.Println("Attempting to install ttyd via snap...")
		cmd := exec.Command("sudo", "snap", "install", "ttyd", "--classic")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err == nil {
			if _, err := exec.LookPath("ttyd"); err == nil {
				fmt.Println("ttyd installed successfully via snap!")
				return nil
			}
		}
	}
	return errors.New("ttyd not found. Please install it manually via your package manager.\n" +
		"Then restart this application.")
}
