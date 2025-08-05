package main

import (
	"errors"
	"os/exec"
	"runtime"
)

// ensureTtyd verifies ttyd is on PATH or tries to install it automatically.
func ensureTtyd() error {
	if _, err := exec.LookPath("ttyd"); err == nil {
		return nil
	}

	switch runtime.GOOS {
	case "windows":
		if _, err := exec.LookPath("winget"); err == nil {
			_ = exec.Command("winget", "install", "-q",
				"--accept-package-agreements", "--accept-source-agreements",
				"tsl0922.ttyd").Run()
			if _, err := exec.LookPath("ttyd"); err == nil {
				return nil
			}
		}
		if _, err := exec.LookPath("scoop"); err == nil {
			_ = exec.Command("scoop", "install", "ttyd").Run()
			if _, err := exec.LookPath("ttyd"); err == nil {
				return nil
			}
		}
		return errors.New("ttyd not found; install via winget or scoop")
	case "darwin":
		if _, err := exec.LookPath("brew"); err == nil {
			_ = exec.Command("brew", "install", "ttyd").Run()
			if _, err := exec.LookPath("ttyd"); err == nil {
				return nil
			}
		}
		return errors.New("ttyd not found; install via Homebrew")
	default: // linux
		if _, err := exec.LookPath("apt-get"); err == nil {
			_ = exec.Command("sudo", "apt-get", "-y", "install", "ttyd").Run()
			if _, err := exec.LookPath("ttyd"); err == nil {
				return nil
			}
		}
		if _, err := exec.LookPath("snap"); err == nil {
			_ = exec.Command("sudo", "snap", "install", "ttyd", "--classic").Run()
			if _, err := exec.LookPath("ttyd"); err == nil {
				return nil
			}
		}
		return errors.New("ttyd not found; install via your package manager")
	}
}
