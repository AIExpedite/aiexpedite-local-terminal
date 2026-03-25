//go:build !windows
// +build !windows

package main

// aggressiveCleanup is a no-op on non-Windows platforms.
// On Windows this uses taskkill to kill the process tree.
func aggressiveCleanup() {}
