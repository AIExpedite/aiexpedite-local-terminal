//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"
)

// whatsAppKeystoreCLITimeout bounds every credential-store call, so a locked
// keychain or a keyring prompt can never hang a link start or a cleanup.
const whatsAppKeystoreCLITimeout = 10 * time.Second

// runWhatsAppKeystoreCLI runs one credential-store command. The secret, when
// there is one, travels on STDIN only — never in argv, where any local user
// could read it from the process table.
var runWhatsAppKeystoreCLI = func(stdin string, name string, args ...string) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), whatsAppKeystoreCLITimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	hideWindow(cmd)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			return "", -1, err
		}
	}
	return strings.TrimSpace(out.String()), code, nil
}
