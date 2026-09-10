package main

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// tmuxSessionName is the session this channel owns. It is per channel
// (local_ports.go) because the prod and dev agents run side by side on one
// machine: a shared name made them race for one session at startup and kill
// each other's session at shutdown.
var tmuxSessionName = tmuxSessionNameFor(EnvName)

// tmuxTarget returns tmuxSessionName as an EXACT target-session. tmux resolves
// a bare `-t <name>` by trying an exact match and then a UNIQUE PREFIX match,
// and prod's `agent` is a prefix of every other channel's `agent-<env>`: with
// only `agent-dev` alive, prod's `has-session -t agent` would succeed against
// the dev session, then `attach`/`kill-session` would hit — and finally
// destroy — it. The `=` prefix disables prefix matching, so every has/attach/
// kill below addresses this channel's session and nothing else. Session
// CREATION (`new-session -s`) takes a literal name, not a target, so it must
// keep using tmuxSessionName unprefixed.
func tmuxTarget() string { return "=" + tmuxSessionName }

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
	if err := exec.Command("tmux", "has-session", "-t", tmuxTarget()).Run(); err == nil {
		return nil
	}

	// Create a new detached session.
	var stderr bytes.Buffer
	create := exec.Command("tmux", "new-session", "-d", "-s", tmuxSessionName)
	create.Stderr = &stderr
	if err := create.Run(); err != nil {
		// Lost a create race (another agent instance of THIS channel, or a
		// relaunch overlapping its predecessor's teardown): the session we
		// wanted now exists, which is the outcome we were after.
		if tmuxDuplicateSession(stderr.String()) &&
			exec.Command("tmux", "has-session", "-t", tmuxTarget()).Run() == nil {
			return nil
		}
		return fmt.Errorf("failed to start tmux session %q: %w (%s)",
			tmuxSessionName, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

// tmuxDuplicateSession reports whether tmux refused new-session because the
// name is already taken (`duplicate session: <name>`).
func tmuxDuplicateSession(stderr string) bool {
	return strings.Contains(stderr, "duplicate session")
}
