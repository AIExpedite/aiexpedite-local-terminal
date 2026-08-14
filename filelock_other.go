//go:build !windows
// +build !windows

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryLockFileExclusive(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}

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
