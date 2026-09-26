//go:build !windows && !darwin

package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
)

// launchSignInWindow starts the first available terminal emulator on the
// sign-in. Needs a graphical session. Not waited for: the window belongs to the
// person signing in.
func launchSignInWindow(program string, args []string, title string) error {
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return errors.New("no graphical session to open a terminal window in (DISPLAY / WAYLAND_DISPLAY unset)")
	}
	var tried []string
	for _, argv := range linuxSignInCandidates(program, args, title) {
		if _, err := exec.LookPath(argv[0]); err != nil {
			tried = append(tried, argv[0])
			continue
		}
		c := exec.Command(argv[0], argv[1:]...)
		c.Env = os.Environ()
		if home, err := os.UserHomeDir(); err == nil {
			c.Dir = home
		}
		if err := c.Start(); err != nil {
			tried = append(tried, argv[0]+" ("+err.Error()+")")
			continue
		}
		go func() { _ = c.Wait() }()
		return nil
	}
	return errors.New("no terminal emulator found (tried " + strings.Join(tried, ", ") + ")")
}
