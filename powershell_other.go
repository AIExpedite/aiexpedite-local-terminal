//go:build !windows
// +build !windows

// File: powershell_other.go
// Non-Windows stubs for PowerShell functions. PowerShell persistence
// is only used on Windows; Mac/Linux use native shells directly.

package main

// ShutdownPowerShell is a no-op on non-Windows platforms.
func ShutdownPowerShell() {}
