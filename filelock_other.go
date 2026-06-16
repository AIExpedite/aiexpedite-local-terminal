//go:build !windows
// +build !windows

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockFileExclusive blocks until an exclusive (writer) advisory lock is held on
// f. flock(2) locks are released automatically when the file descriptor closes,
// so callers can rely on `defer f.Close()` if unlockFile is missed.
func lockFileExclusive(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX)
}

// unlockFile releases the advisory lock acquired by lockFileExclusive.
func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
