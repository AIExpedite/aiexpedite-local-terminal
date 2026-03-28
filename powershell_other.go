//go:build !windows
// +build !windows

// File: powershell_other.go
// Non-Windows stubs for PowerShell functions. PowerShell persistence
// is only used on Windows; Mac/Linux use native shells directly.

package main

import (
	"context"
	"fmt"
)

// PersistentPowerShell is a stub for non-Windows platforms.
type PersistentPowerShell struct{}

// Execute is a stub (never called on non-Windows).
func (ps *PersistentPowerShell) Execute(ctx context.Context, command string, cwd string) (string, error) {
	return "", fmt.Errorf("PowerShell not supported on this platform")
}

// ExitCodeError is a stub for non-Windows platforms.
type ExitCodeError struct {
	Code   int
	Output string
}

func (e *ExitCodeError) Error() string {
	return fmt.Sprintf("exit code %d", e.Code)
}

// GetPowerShell is a no-op on non-Windows platforms.
func GetPowerShell() (*PersistentPowerShell, error) {
	return nil, fmt.Errorf("PowerShell not supported on this platform")
}

// RestartPowerShell is a no-op on non-Windows platforms.
func RestartPowerShell() error {
	return fmt.Errorf("PowerShell not supported on this platform")
}

// IsPersistentPSPwsh always returns false on non-Windows platforms.
func IsPersistentPSPwsh() bool {
	return false
}

// GetTrackedCwd always returns empty on non-Windows platforms.
func GetTrackedCwd() string {
	return ""
}

// ShutdownPowerShell is a no-op on non-Windows platforms.
func ShutdownPowerShell() {}
