//go:build !windows && !darwin

// File: relocate_other.go
// Linux keeps its current packaging and install location, so there is nothing
// to relocate. Matches the cleanup_other.go / filelock_other.go build-tag
// pattern.

package main

// maybeRelocateInstall is a no-op on Linux: the automatic update behaviour is
// the same as on other platforms, but the install location does not move.
func maybeRelocateInstall(_ *Config) bool { return false }

func handleDarwinUninstall(_ bool) {}
