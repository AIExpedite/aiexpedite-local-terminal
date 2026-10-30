//go:build !darwin && !windows && !linux
// +build !darwin,!windows,!linux

// File: keep_awake_other.go
// -----------------------------------------------------------------------------
// No keep-awake on other platforms (keep_awake.go): the manager logs this once
// and sessions run without it.
// -----------------------------------------------------------------------------

package main

import "errors"

func acquirePlatformKeepAwake(func(error)) (func(), error) {
	return nil, errors.New("keep-awake is not supported on this platform")
}
