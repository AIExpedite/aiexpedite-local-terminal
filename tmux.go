package main

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
)

var tmuxSessionName = "agent"

// ensureTmux returns nil when tmux is available (installed automatically when
// possible) or an error explaining why it cannot be used.
func ensureTmux() error {
	if _, err := exec.LookPath("tmux"); err == nil {
		return nil
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
	default: // linux & friends
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

// startTmuxSession ensures a detached tmux session named tmuxSessionName exists.
func startTmuxSession() error {
	// Is the session already running?
	if err := exec.Command("tmux", "has-session", "-t", tmuxSessionName).Run(); err == nil {
		return nil
	}

	// Create a new detached session.
	if err := exec.Command("tmux", "new-session", "-d", "-s", tmuxSessionName).Run(); err != nil {
		return fmt.Errorf("failed to start tmux session: %w", err)
	}
	return nil
}
