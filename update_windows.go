//go:build windows
// +build windows

// File: update_windows.go
// Windows-specific UAC elevation for auto-update when installed in Program Files.

package main

import (
	"fmt"
	"os"
)

// requestElevatedCopy re-launches the current executable with UAC elevation
// to copy src over dst. Used when performSelfReplace fails due to permissions
// (e.g., when the app is installed in C:\Program Files).
func requestElevatedCopy(src, dst string) error {
	myExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine own exe: %w", err)
	}

	args := `--elevated-copy --copy-from="` + src + `" --copy-to="` + dst + `"`

	fmt.Printf("[update] Requesting UAC elevation for copy: %s -> %s\n", src, dst)
	return runElevatedAndWait(myExe, args)
}
