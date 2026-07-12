// File: askpass.go
// Non-interactive askpass helper. GIT_ASKPASS / SSH_ASKPASS are pointed at a
// tiny script that prints nothing and exits non-zero, so git/ssh credential and
// passphrase prompts fail fast (with a captured error) instead of falling back
// to an interactive /dev/tty prompt that would hang.
package main

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

var (
	askpassPathOnce sync.Once
	askpassPath     string
)

// nonInteractiveAskpassPath lazily materialises the askpass helper script in
// BinDir and returns its absolute path. Returns "" if the script cannot be
// written (in which case GIT_TERMINAL_PROMPT=0 + no controlling terminal still
// prevent an interactive hang; askpass is defence in depth).
func nonInteractiveAskpassPath() string {
	askpassPathOnce.Do(func() {
		dir := BinDir()
		if dir == "" {
			return
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return
		}
		if runtime.GOOS == "windows" {
			// SSH_ASKPASS/GIT_ASKPASS are invoked as executables; a .cmd works.
			p := filepath.Join(dir, "aix-askpass-noninteractive.cmd")
			script := "@echo off\r\nexit /b 1\r\n"
			if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
				return
			}
			askpassPath = p
			return
		}
		p := filepath.Join(dir, "aix-askpass-noninteractive.sh")
		script := "#!/bin/sh\n# Non-interactive askpass: refuse all credential/passphrase prompts.\nexit 1\n"
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			return
		}
		askpassPath = p
	})
	return askpassPath
}
