package main

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
)

var tmuxSessionName = "agent" // name of the tmux session we manage

// ensureTmux checks if tmux is available, and attempts to install it if missing.
// Returns nil if tmux is available (possibly after installation), otherwise an error.
func ensureTmux() error {
	// Check if tmux command exists
	_, err := exec.LookPath("tmux")
	if err == nil {
		return nil // tmux is present
	}
	// tmux not found, attempt to install if possible
	switch runtime.GOOS {
	case "windows":
		// On Windows, tmux isn't natively available outside WSL/MSYS.
		// We cannot easily auto-install tmux on Windows, so inform the user.
		return errors.New("tmux not found on Windows (requires WSL or MSYS2 installation):contentReference[oaicite:9]{index=9}")
	case "darwin":
		// Try Homebrew to install tmux
		if _, berr := exec.LookPath("brew"); berr == nil {
			cmd := exec.Command("brew", "install", "tmux")
			if err := cmd.Run(); err == nil {
				return nil // installed successfully
			}
		}
		return errors.New("tmux not found, please install it (e.g., via Homebrew)")
	default:
		// On Linux/Unix, try apt-get or yum, etc.
		if _, aerr := exec.LookPath("apt-get"); aerr == nil {
			// Use apt-get if available
			cmd := exec.Command("sudo", "apt-get", "-y", "install", "tmux")
			// If not running as root, this may fail without password prompt.
			_ = cmd.Run()
			// Check again
			if _, err2 := exec.LookPath("tmux"); err2 == nil {
				return nil
			}
		}
		if _, yerr := exec.LookPath("yum"); yerr == nil {
			cmd := exec.Command("sudo", "yum", "-y", "install", "tmux")
			_ = cmd.Run()
			if _, err2 := exec.LookPath("tmux"); err2 == nil {
				return nil
			}
		}
		// Could add other package managers if needed (snap, pacman, etc.)
		return errors.New("tmux not found, please install it via your package manager")
	}
}

// startTmuxSession starts a persistent tmux session in the background if it's not already running.
func startTmuxSession() error {
	// Check if our session already exists
	cmdCheck := exec.Command("tmux", "has-session", "-t", tmuxSessionName)
	if err := cmdCheck.Run(); err != nil {
		// Exit code non-zero means session doesn't exist, so create it
		cmdNew := exec.Command("tmux", "new-session", "-d", "-s", tmuxSessionName)
		if err2 := cmdNew.Run(); err2 != nil {
			return fmt.Errorf("failed to start tmux session: %w", err2)
		}
	}
	return nil
}
