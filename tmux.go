package main

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
)

var tmuxSessionName = "agent"

func ensureTmux() error {
	if _, err := exec.LookPath("tmux"); err == nil {
		return nil // present
	}

	switch runtime.GOOS {
	case "windows":
		return errors.New("tmux not found on Windows (requires WSL or MSYS2)")
	case "darwin":
		if _, err := exec.LookPath("brew"); err == nil {
			_ = exec.Command("brew", "install", "tmux").Run()
			if _, err := exec.LookPath("tmux"); err == nil {
				return nil
			}
		}
		return errors.New("tmux not installed; try `brew install tmux`")
	default: // linux
		if _, err := exec.LookPath("apt-get"); err == nil {
			_ = exec.Command("sudo", "apt-get", "-y", "install", "tmux").Run()
			if _, err := exec.LookPath("tmux"); err == nil {
				return nil
			}
		}
		if _, err := exec.LookPath("yum"); err == nil {
			_ = exec.Command("sudo", "yum", "-y", "install", "tmux").Run()
			if _, err := exec.LookPath("tmux"); err == nil {
				return nil
			}
		}
		return errors.New("tmux not installed; please install via your package manager")
	}
}
