package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

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
func installTtydWindows() error {
	// Try winget first
	if _, err := exec.LookPath("winget"); err == nil {
		fmt.Println("Attempting to install ttyd via winget (this may take a moment)...")
		cmd := exec.Command("winget", "install",
			"-e", "--id", "tsl0922.ttyd",
			"-h",
			"--accept-package-agreements",
			"--accept-source-agreements",
			"--disable-interactivity")

		output, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Printf("winget install failed: %v\nOutput: %s\n", err, string(output))
		} else {
			fmt.Printf("winget output: %s\n", string(output))

			// Check if ttyd is now available (try PATH first, then common install locations)
			if checkTtydInstalled() {
				fmt.Println("ttyd installed successfully via winget!")
				return nil
			}
		}
	}

	// Try scoop as fallback
	if _, err := exec.LookPath("scoop"); err == nil {
		fmt.Println("Attempting to install ttyd via scoop...")
		cmd := exec.Command("scoop", "install", "ttyd")
		output, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Printf("scoop install failed: %v\nOutput: %s\n", err, string(output))
		} else {
			fmt.Printf("scoop output: %s\n", string(output))

			if checkTtydInstalled() {
				fmt.Println("ttyd installed successfully via scoop!")
				return nil
			}
		}
	}

	return errors.New("ttyd not found. Please install it manually:\n" +
		"  Option 1: winget install tsl0922.ttyd\n" +
		"  Option 2: scoop install ttyd\n" +
		"Then restart this application.")
}

// installTtydDarwin attempts to install ttyd on macOS via Homebrew
func installTtydDarwin() error {
	if _, err := exec.LookPath("brew"); err == nil {
		fmt.Println("Attempting to install ttyd via Homebrew...")
		cmd := exec.Command("brew", "install", "ttyd")
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
