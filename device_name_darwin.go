package main

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// friendlyComputerNamePlatform returns macOS's ComputerName — the name shown in System
// Settings → General → About and advertised over Bonjour — which is what a user
// recognises their Mac by. Empty when it cannot be read; the caller falls back
// to the hostname.
//
// `scutil` is the supported interface for this and is present on every macOS
// install. It is given a short timeout because this runs during registration and
// the token exchange, neither of which may hang on a wedged configd.
func friendlyComputerNamePlatform() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--get", "ComputerName").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
