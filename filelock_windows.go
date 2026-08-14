//go:build windows
// +build windows

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// tryLockFileExclusive attempts to acquire the same exclusive lock as
// lockFileExclusive without waiting when another process already owns it.
func tryLockFileExclusive(f *os.File) (bool, error) {
	ol := &windows.Overlapped{}
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 0xFFFFFFFF, 0xFFFFFFFF, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

// lockFileExclusive blocks until an exclusive byte-range lock is held on f via
// LockFileEx. Locks are released when the file handle closes, so callers can
// rely on `defer f.Close()` if unlockFile is missed.
func lockFileExclusive(f *os.File) error {
	ol := &windows.Overlapped{}
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 0xFFFFFFFF, 0xFFFFFFFF, ol)
}

// unlockFile releases the byte-range lock acquired by lockFileExclusive.
func unlockFile(f *os.File) error {
	ol := &windows.Overlapped{}
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 0xFFFFFFFF, 0xFFFFFFFF, ol)
}
