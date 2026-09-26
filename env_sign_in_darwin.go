//go:build darwin

package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// launchSignInWindow opens Terminal.app on the sign-in via osascript. osascript
// returns once Terminal has accepted the script, well before the sign-in ends.
func launchSignInWindow(program string, args []string, title string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "osascript", "-e", macSignInScript(program, args, title)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("could not open Terminal: %v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
