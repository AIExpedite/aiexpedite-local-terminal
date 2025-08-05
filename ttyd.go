package main

import (
	"errors"
	"os/exec"
	"runtime"
)

// ensureTtyd checks if ttyd is available, and attempts to install it if missing.
func ensureTtyd() error {
	_, err := exec.LookPath("ttyd")
	if err == nil {
		return nil
	}
	// Not found, attempt installation
	switch runtime.GOOS {
	case "windows":
		// Try using winget or scoop to install ttyd:contentReference[oaicite:10]{index=10}
		if _, werr := exec.LookPath("winget"); werr == nil {
			cmd := exec.Command("winget", "install", "-q", "--accept-package-agreements", "--accept-source-agreements", "tsl0922.ttyd")
			_ = cmd.Run()
			if _, err2 := exec.LookPath("ttyd"); err2 == nil {
				return nil
			}
		}
		if _, serr := exec.LookPath("scoop"); serr == nil {
			cmd := exec.Command("scoop", "install", "ttyd")
			_ = cmd.Run()
			if _, err2 := exec.LookPath("ttyd"); err2 == nil {
				return nil
			}
		}
		return errors.New("ttyd not found, please install it (winget or scoop can be used)")
	case "darwin":
		// Try Homebrew
		if _, berr := exec.LookPath("brew"); berr == nil {
			cmd := exec.Command("brew", "install", "ttyd")
			_ = cmd.Run()
			if _, err2 := exec.LookPath("ttyd"); err2 == nil {
				return nil
			}
		}
		return errors.New("ttyd not found, please install it (e.g., via Homebrew)")
	default:
		// Linux/Unix: try apt-get or snap
		if _, aerr := exec.LookPath("apt-get"); aerr == nil {
			cmd := exec.Command("sudo", "apt-get", "-y", "install", "ttyd")
			_ = cmd.Run()
			if _, err2 := exec.LookPath("ttyd"); err2 == nil {
				return nil
			}
		}
		if _, serr := exec.LookPath("snap"); serr == nil {
			cmd := exec.Command("sudo", "snap", "install", "ttyd", "--classic")
			_ = cmd.Run()
			if _, err2 := exec.LookPath("ttyd"); err2 == nil {
				return nil
			}
		}
		return errors.New("ttyd not found, please install it via your package manager")
	}
}
