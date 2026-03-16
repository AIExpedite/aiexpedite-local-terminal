//go:build !windows
// +build !windows

// File: update_other.go
// Non-Windows stub for UAC elevation (not available on other platforms).

package main

import (
	"fmt"
	"runtime"
)

// requestElevatedCopy is a no-op on non-Windows platforms.
func requestElevatedCopy(src, dst string) error {
	return fmt.Errorf("UAC elevation not available on %s", runtime.GOOS)
}
