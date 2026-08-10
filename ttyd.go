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

// ttydDependencySpec describes ttyd for the shared install runner. Unlike Git,
// ttyd is used in-process immediately after install, so PostInstall
// (checkTtydInstalled) is the authoritative success signal and augments PATH;
// Scoop is kept as the Windows fallback.
func ttydDependencySpec() DependencySpec {
	return DependencySpec{
		DisplayName: "ttyd (Terminal Server)",
		PromptDescription: "ttyd provides the web-based terminal interface.\n" +
			"It can be installed automatically via the Windows Package Manager " +
			"(winget), with Scoop as a fallback.",
		WingetID:        "tsl0922.ttyd",
		ScoopID:         "ttyd",
		VerifyCommand:   "ttyd",
		PostInstall:     checkTtydInstalled,
		ManualURL:       ttydDownloadURL,
		TroubleshootURL: "https://github.com/tsl0922/ttyd#installation",
	}
}

// installTtydWindows installs ttyd on Windows through the shared dependency
// runner, so an installer-launch failure gets the same guided recovery
// (retry / manual / troubleshoot) and diagnostics as every other package
// install. ttyd is required, so any opt-out or unrecovered failure stops
// startup.
func installTtydWindows() error {
	err := runDependencyInstall(ttydDependencySpec())
	if err == nil {
		return nil
	}
	// The runner already opened the browser / showed the guided recovery. ttyd
	// can't run without being installed, so on an explicit opt-out we tell the
	// user how to finish and exit cleanly (matching prior behavior); any other
	// error is surfaced to the caller as fatal.
	if errors.Is(err, errInstallDeclined) || errors.Is(err, errInstallManual) {
		ShowInfoDialog(
			"Setup Incomplete",
			"ttyd is required to run AI Expedite.\n\n"+
				"Install it and restart the app:\n"+
				"  winget install tsl0922.ttyd",
		)
		os.Exit(0)
	}
	return err
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
